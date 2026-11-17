#!/usr/bin/env bash
#
# publish.sh - push a SANITIZED, single-commit snapshot of this repository
# to a public GitHub remote.
#
# The snapshot is one squashed commit per run ("snapshot: YYYY-MM-DD (from
# <internal-sha8>)"). Internal history, internal docs and machine-local
# paths never cross the boundary; only the publish set does.
#
# Publish set = every tracked file EXCEPT these 11 internal docs:
#   DESIGN_SPEC.md, DEV_LOG.md, DEV_NOTES.md, IMPLEMENTATION_PLAN.md,
#   NOTES.md, PLAN_SPEC.md, RELEASING.md, TODO.md, USAGE.md, WORK_LOG.md,
#   initial-spec.md
# i.e. README.md + Go sources/tests + go.mod/go.sum + Makefile +
# .golangci.yml + .gitignore (+ this script once committed).
# README-public.md (the public document) is tracked but never shipped under
# its own name: step 3 installs it as the export's README.md, byte for byte.
#
# Usage:
#   scripts/publish.sh [--url GITHUB_URL] [--tag TAG] [-m|--message TEXT]
#                      [--dry-run]
#   scripts/publish.sh --help
#
# Options:
#   --url URL        Public GitHub remote (also settable via env GITHUB_URL),
#                    e.g. https://github.com/OWNER/isopress.git.
#                    Required for a real push; may be omitted with --dry-run
#                    (placeholders are substituted instead - the output says so).
#   --tag TAG        Annotated tag of this name on the snapshot commit, pushed
#                    with it. Internal tags stay on the internal remote;
#                    GitHub tags are mirrored onto snapshot commits here.
#   -m, --message T  Extra commit-message text appended to the snapshot message.
#   --dry-run        Do everything except push and tag.
#   -h, --help       This text.
#
# Environment: GITHUB_URL (same as --url), PUBLISH_DIR (override the publish
# clone location, default <repo>/../isopress-publish).
#
# Behaviour:
#   1. Refuses a dirty working tree (a snapshot publishes committed HEAD);
#      --dry-run warns and continues against HEAD.
#   2. git archive HEAD (tracked files only) into a fresh temp dir.
#   3. Deletes the 11 internal docs from the export.
#   4. Installs README-public.md as the export's README.md (byte copy, no
#      rewriting - the public document is authored for GitHub directly) and
#      removes README-public.md so exactly one README ships.
#   5. Runs the clean-room gate (leaks, links, README byte-identity); any hit
#      aborts before anything is pushed.
#   6. Mirrors the export into ../isopress-publish (outside this repo),
#      commits one snapshot, pushes to the remote named "github".
#   7. Optionally tags the snapshot commit and pushes the tag.
#
set -euo pipefail

# ---------------------------------------------------------------------------
# Excluded internal docs (the 11) - never published.
# ---------------------------------------------------------------------------
EXCLUDED_DOCS=(
  DESIGN_SPEC.md
  DEV_LOG.md
  DEV_NOTES.md
  IMPLEMENTATION_PLAN.md
  NOTES.md
  PLAN_SPEC.md
  RELEASING.md
  TODO.md
  USAGE.md
  WORK_LOG.md
  initial-spec.md
)

# Placeholder used for --dry-run without --url (reported as such).
PLACEHOLDER_URL="https://github.com/Clydesolve-Limited/isopress"

usage() {
  sed -n '2,52p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

# ---------------------------------------------------------------------------
# Arguments
# ---------------------------------------------------------------------------
GITHUB_URL="${GITHUB_URL:-}"
TAG="${TAG:-}"
MESSAGE=""
DRY_RUN=0
URL_PROVIDED=0
if [ -n "$GITHUB_URL" ]; then URL_PROVIDED=1; fi

while [ $# -gt 0 ]; do
  case "$1" in
    --url)        [ $# -ge 2 ] || { echo "error: --url needs a value" >&2; exit 2; }
                  GITHUB_URL="$2"; URL_PROVIDED=1; shift 2 ;;
    --url=*)      GITHUB_URL="${1#--url=}"; URL_PROVIDED=1; shift ;;
    --tag)        [ $# -ge 2 ] || { echo "error: --tag needs a value" >&2; exit 2; }
                  TAG="$2"; shift 2 ;;
    --tag=*)      TAG="${1#--tag=}"; shift ;;
    -m|--message) [ $# -ge 2 ] || { echo "error: $1 needs a value" >&2; exit 2; }
                  MESSAGE="$2"; shift 2 ;;
    --message=*)  MESSAGE="${1#--message=}"; shift ;;
    --dry-run)    DRY_RUN=1; shift ;;
    -h|--help)    usage; exit 0 ;;
    *)            echo "error: unknown argument: $1 (see --help)" >&2; exit 2 ;;
  esac
done

# ---------------------------------------------------------------------------
# Repo + preconditions
# ---------------------------------------------------------------------------
command -v python3 >/dev/null 2>&1 || {
  echo "error: python3 is required for the clean-room gate's README link check but was not found on PATH" >&2; exit 1; }

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || {
  echo "error: not inside a git repository" >&2; exit 1; }
cd "$REPO_ROOT"

HEAD_SHA="$(git rev-parse HEAD)"
HEAD_SHA8="$(git rev-parse --short=8 HEAD)"

STATUS="$(git status --porcelain)"
if [ -n "$STATUS" ]; then
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "warning: working tree is dirty; --dry-run continues against committed HEAD ${HEAD_SHA8}" >&2
    printf '%s\n' "$STATUS" >&2
  else
    {
      echo "error: refusing to publish from a dirty working tree."
      echo "A snapshot publishes committed HEAD; commit (or stash) first, then re-run."
      echo "Dirty entries:"
      printf '%s\n' "$STATUS"
    } >&2
    exit 1
  fi
fi

# Placeholder URL substituted when none was given (reported as such).
if [ "$URL_PROVIDED" -eq 1 ]; then
  PLACEHOLDER=0
else
  GITHUB_URL="$PLACEHOLDER_URL"
  PLACEHOLDER=1
fi

EXPORT_DIR=""
on_exit() {
  local rc=$?
  if [ -n "$EXPORT_DIR" ]; then
    if [ "$rc" -ne 0 ]; then
      echo "note: export kept for inspection: $EXPORT_DIR" >&2
    else
      rm -rf "$EXPORT_DIR"
    fi
  fi
}
trap on_exit EXIT

PUBLISH_DIR="${PUBLISH_DIR:-$REPO_ROOT/../isopress-publish}"

echo "== isopress publish (sanitized snapshot) =="
echo "source repo : $REPO_ROOT"
echo "source HEAD : $HEAD_SHA8"
echo "mode        : $([ "$DRY_RUN" -eq 1 ] && echo 'dry-run (no push, no tag)' || echo 'push')"
echo "remote      : $GITHUB_URL $([ "$PLACEHOLDER" -eq 1 ] && echo '(PLACEHOLDER)')"
echo "publish dir : $PUBLISH_DIR"
echo

# ---------------------------------------------------------------------------
# [1/7] Export tracked files only
# ---------------------------------------------------------------------------
echo "[1/7] export: git archive HEAD"
EXPORT_DIR="$(mktemp -d "${TMPDIR:-/tmp}/isopress-publish-XXXXXX")"
git archive HEAD | tar -x -C "$EXPORT_DIR"
echo "  -> $EXPORT_DIR"

# ---------------------------------------------------------------------------
# [2/7] Exclude the 11 internal docs
# ---------------------------------------------------------------------------
echo "[2/7] exclude internal docs"
REMOVED=()
ABSENT=()
for f in "${EXCLUDED_DOCS[@]}"; do
  if [ -e "$EXPORT_DIR/$f" ]; then
    rm -f -- "$EXPORT_DIR/$f"
    REMOVED+=("$f")
  else
    ABSENT+=("$f")
  fi
done
echo "  removed : ${REMOVED[*]:-<none>}"
if [ "${#ABSENT[@]}" -gt 0 ]; then echo "  absent  : ${ABSENT[*]}"; fi
echo "  excluded list (all ${#EXCLUDED_DOCS[@]}): ${EXCLUDED_DOCS[*]}"

# ---------------------------------------------------------------------------
# [3/7] Install the public README
#   README-public.md IS the public document (authored for GitHub directly),
#   so there is nothing to rewrite: it becomes the export's README.md byte
#   for byte, and is then dropped from the export so exactly ONE README.md
#   ships. Sourced from the checkout rather than the export because a real
#   run has already refused a dirty tree here - the two copies are then
#   identical - and it lets --dry-run preview before README-public.md's
#   first commit lands.
# ---------------------------------------------------------------------------
echo "[3/7] install public README (README-public.md -> README.md)"
[ -f "$REPO_ROOT/README-public.md" ] || {
  echo "error: README-public.md not found at $REPO_ROOT/README-public.md" >&2; exit 1; }
cp "$REPO_ROOT/README-public.md" "$EXPORT_DIR/README.md"
rm -f "$EXPORT_DIR/README-public.md"
echo "  README.md    <- README-public.md (byte copy, no transform)"
echo "  removed      : README-public.md (not shipped; exactly one README.md remains)"

# ---------------------------------------------------------------------------
# [4/7] Clean-room gate (must pass or abort)
# ---------------------------------------------------------------------------
echo "[4/7] clean-room gate"
GATE_FAIL=0

# The two needles are assembled at run time on purpose: this script is
# itself part of the publish set, so a literal needle here would make the
# gate flag its own source. Assembled, the script still scans EVERY
# published file (including this one) for the real string.
NEEDLE_HOME="$(printf '%s%s' '/home/' 'martin')"
NEEDLE_ENV="$(printf '%s%s' 'BACKUP' '_TOOL_')"

# (a) none of the 11 exists
for f in "${EXCLUDED_DOCS[@]}"; do
  if [ -e "$EXPORT_DIR/$f" ]; then
    echo "  (a) FAIL: excluded file still present: $f"; GATE_FAIL=1
  fi
done
if [ "$GATE_FAIL" -eq 0 ]; then echo "  (a) PASS: none of the ${#EXCLUDED_DOCS[@]} excluded docs is present"; fi

# (b) no internal home path (skip binaries)
HITS="$(grep -rI -n -F "$NEEDLE_HOME" "$EXPORT_DIR" 2>/dev/null || true)"
if [ -n "$HITS" ]; then
  echo "  (b) FAIL: $NEEDLE_HOME leaks:"; printf '%s\n' "$HITS" | sed "s|$EXPORT_DIR/|  |"
  GATE_FAIL=1
else
  echo "  (b) PASS: 0 hits of $NEEDLE_HOME (binaries skipped)"
fi

# (c) no internal env-var prefix
HITS="$(grep -rI -n -F "$NEEDLE_ENV" "$EXPORT_DIR" 2>/dev/null || true)"
if [ -n "$HITS" ]; then
  echo "  (c) FAIL: $NEEDLE_ENV leaks:"; printf '%s\n' "$HITS" | sed "s|$EXPORT_DIR/|  |"
  GATE_FAIL=1
else
  echo "  (c) PASS: 0 hits of $NEEDLE_ENV"
fi

# (d) every relative README link target resolves inside the export
LINK_OUT="$(python3 - "$EXPORT_DIR" <<'PYEOF'
import os
import re
import sys

export = sys.argv[1]
readme = os.path.join(export, "README.md")
EXCLUDED = {
    "DESIGN_SPEC.md", "DEV_LOG.md", "DEV_NOTES.md", "IMPLEMENTATION_PLAN.md",
    "NOTES.md", "PLAN_SPEC.md", "RELEASING.md", "TODO.md", "USAGE.md",
    "WORK_LOG.md", "initial-spec.md",
}
with open(readme, encoding="utf-8") as fh:
    body = fh.read()
targets = [m.group(1) for m in re.finditer(r"\]\(([^()\s]+)\)", body)]
bad, excluded_left, relative = [], [], 0
for t in targets:
    if t.startswith("#") or re.match(r"^[a-zA-Z][a-zA-Z0-9+.-]*://", t):
        continue
    relative += 1
    base = t.split("#", 1)[0]
    if base in EXCLUDED:
        excluded_left.append(t)
    elif not os.path.exists(os.path.join(export, base)):
        bad.append(t)
print(f"total={len(targets)} relative={relative}")
for t in excluded_left:
    print(f"EXCLUDED_LINK {t}")
for t in bad:
    print(f"MISSING {t}")
PYEOF
)"
echo "  (d) README links: $(echo "$LINK_OUT" | head -1)"
if echo "$LINK_OUT" | grep -q '^EXCLUDED_LINK'; then
  echo "  (d) FAIL: link(s) to an excluded doc remain:"; echo "$LINK_OUT" | grep '^EXCLUDED_LINK' | sed 's/^/    /'
  GATE_FAIL=1
fi
if echo "$LINK_OUT" | grep -q '^MISSING'; then
  echo "  (d) FAIL: README target(s) missing from the export:"; echo "$LINK_OUT" | grep '^MISSING' | sed 's/^/    /'
  GATE_FAIL=1
fi
if ! echo "$LINK_OUT" | grep -qE '^(EXCLUDED_LINK|MISSING)'; then
  echo "  (d) PASS: every relative README target resolves in the export; http(s)/#anchor links untouched"
fi

# (e) export size + top-level layout
FILE_COUNT="$(find "$EXPORT_DIR" -type f | wc -l | tr -d ' ')"
echo "  (e) export: $FILE_COUNT files; top-level layout:"
ls -1A "$EXPORT_DIR" | sed 's/^/      /'

# (f) the exported README is byte-identical to the public document - a
#     transform creeping back into step 3 would show up here
if cmp -s "$EXPORT_DIR/README.md" "$REPO_ROOT/README-public.md"; then
  echo "  (f) PASS: exported README.md is byte-identical to README-public.md"
else
  echo "  (f) FAIL: exported README.md differs from README-public.md:"
  cmp "$EXPORT_DIR/README.md" "$REPO_ROOT/README-public.md" 2>&1 | sed 's/^/    /' || true
  GATE_FAIL=1
fi

# (g) the public document does not ship under its own name (one README only)
if [ -e "$EXPORT_DIR/README-public.md" ]; then
  echo "  (g) FAIL: README-public.md still present in the export (duplicate README)"
  GATE_FAIL=1
else
  echo "  (g) PASS: README-public.md absent from the export (exactly one README.md)"
fi

if [ "$GATE_FAIL" -ne 0 ]; then
  echo >&2
  echo "error: clean-room gate FAILED - nothing was committed or pushed." >&2
  exit 1
fi
echo "  gate: PASS"
echo

# ---------------------------------------------------------------------------
# [5/7] Publish clone (OUTSIDE the internal repo)
# ---------------------------------------------------------------------------
echo "[5/7] publish clone at $PUBLISH_DIR"
if [ ! -d "$PUBLISH_DIR/.git" ]; then
  git init -q "$PUBLISH_DIR"
  git -C "$PUBLISH_DIR" checkout -q -b main
  echo "  initialised fresh clone on branch main"
else
  echo "  reusing existing clone"
fi
# Wipe working files, keep .git.
find "$PUBLISH_DIR" -mindepth 1 -maxdepth 1 ! -name .git -exec rm -rf {} +
cp -a "$EXPORT_DIR"/. "$PUBLISH_DIR/"
echo "  working tree replaced with the export ($FILE_COUNT files)"

# ---------------------------------------------------------------------------
# [6/7] Snapshot commit
# ---------------------------------------------------------------------------
echo "[6/7] commit"
git -C "$PUBLISH_DIR" add -A
SNAP_MSG="snapshot: $(date +%F) (from $HEAD_SHA8)"
NEW_COMMIT=0
if ! git -C "$PUBLISH_DIR" rev-parse --verify -q HEAD >/dev/null; then
  NEW_COMMIT=1
elif ! git -C "$PUBLISH_DIR" diff --cached --quiet; then
  NEW_COMMIT=1
fi
if [ "$NEW_COMMIT" -eq 1 ]; then
  if [ -n "$MESSAGE" ]; then
    git -C "$PUBLISH_DIR" commit -q -m "$SNAP_MSG" -m "$MESSAGE"
  else
    git -C "$PUBLISH_DIR" commit -q -m "$SNAP_MSG"
  fi
  SNAP_SHA="$(git -C "$PUBLISH_DIR" rev-parse HEAD)"
  SNAP_SHA8="$(git -C "$PUBLISH_DIR" rev-parse --short=8 HEAD)"
  echo "  created: $SNAP_SHA8 ($SNAP_MSG)"
else
  SNAP_SHA="$(git -C "$PUBLISH_DIR" rev-parse HEAD)"
  SNAP_SHA8="$(git -C "$PUBLISH_DIR" rev-parse --short=8 HEAD)"
  echo "  no content change since the last snapshot; HEAD stays $SNAP_SHA8"
fi

# ---------------------------------------------------------------------------
# [7/7] Push (+ optional tag)
# ---------------------------------------------------------------------------
echo "[7/7] push"
PUSHED=0
if [ "$DRY_RUN" -eq 1 ]; then
  echo "  dry-run: would push 'main' to remote 'github' ($GITHUB_URL)"
  if [ -n "$TAG" ]; then echo "  dry-run: would tag $SNAP_SHA8 with annotated tag '$TAG' and push it"; fi
elif [ "$URL_PROVIDED" -eq 0 ]; then
  cat <<EOF
  no GITHUB_URL given - nothing pushed. Set it up with:

    export GITHUB_URL=https://github.com/OWNER/isopress.git
    scripts/publish.sh

  (or pass --url https://github.com/OWNER/isopress.git), then re-run; the
  snapshot commit $SNAP_SHA8 is already waiting in $PUBLISH_DIR.
EOF
else
  if git -C "$PUBLISH_DIR" remote get-url github >/dev/null 2>&1; then
    EXISTING="$(git -C "$PUBLISH_DIR" remote get-url github)"
    if [ "$EXISTING" != "$GITHUB_URL" ]; then
      echo "error: remote 'github' points at $EXISTING, not $GITHUB_URL." >&2
      echo "Fix it deliberately: git -C $PUBLISH_DIR remote set-url github $GITHUB_URL" >&2
      exit 1
    fi
    git -C "$PUBLISH_DIR" push -q github main
  else
    git -C "$PUBLISH_DIR" remote add github "$GITHUB_URL"
    git -C "$PUBLISH_DIR" push -q -u github main
  fi
  PUSHED=1
  echo "  pushed: main ($SNAP_SHA8) -> $GITHUB_URL (remote 'github')"

  if [ -n "$TAG" ]; then
    if git -C "$PUBLISH_DIR" rev-parse -q --verify "refs/tags/$TAG" >/dev/null; then
      echo "error: tag '$TAG' already exists in $PUBLISH_DIR; pick another name or delete it deliberately." >&2
      exit 1
    fi
    git -C "$PUBLISH_DIR" tag -a "$TAG" -m "isopress $TAG - snapshot of internal $HEAD_SHA8"
    git -C "$PUBLISH_DIR" push -q github "refs/tags/$TAG"
    echo "  tagged : $TAG (annotated, on $SNAP_SHA8; source $HEAD_SHA8) + pushed"
  fi
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo
echo "== summary =="
echo "excluded   : ${EXCLUDED_DOCS[*]}"
echo "publish set: $FILE_COUNT files = README.md (public document) + Go sources/tests + go.mod/go.sum + Makefile + .golangci.yml + .gitignore (+ scripts/publish.sh once committed)"
echo "gate       : PASS (no excluded docs, no $NEEDLE_HOME, no $NEEDLE_ENV, all README links resolve, README byte-identical to README-public.md, one README.md)"
echo "snapshot   : $SNAP_SHA8 (source HEAD $HEAD_SHA8)"
if [ "$PUSHED" -eq 1 ]; then
  echo "pushed     : main -> $GITHUB_URL"
  if [ -n "$TAG" ]; then echo "             tag $TAG"; fi
elif [ "$DRY_RUN" -eq 1 ]; then
  echo "pushed     : nothing (dry-run)"
else
  echo "pushed     : nothing (no GITHUB_URL)"
fi
