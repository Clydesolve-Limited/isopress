#!/usr/bin/env bash
#
# package-deb.sh - build the release .deb for isopress.
#
# Builds the release binary the RELEASING way (static, stripped), stages
# the package tree, and runs dpkg-deb. The result lands in
# dist/isopress_<version>_amd64.deb (dist/ is git-ignored).
#
# Usage:
#   scripts/package-deb.sh [--output-dir DIR]
#
# Options:
#   --output-dir DIR  Where the .deb is written (default: dist/,
#                     relative to the repository root).
#   -h, --help        This text.
#
# Design notes:
#   * Version is a SINGLE SOURCE: the built binary's own --version
#     output (catalog.ToolVersion via cobra). Nothing else is consulted,
#     so the package version can never drift from the binary it wraps.
#   * CGO_ENABLED=0 static build: no dynamic linker, therefore NO
#     Depends: field in the control file (no libc dependency).
#   * The packaged README is the repo README as checked out. CI builds
#     run on the published tree, where scripts/publish.sh has already
#     replaced the internal clone URL; a local build from the internal
#     tree packages the internal README (artifacts stay in dist/,
#     which is git-ignored).
#
set -euo pipefail

usage() {
  sed -n '2,28p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

OUTPUT_DIR=""
while [ $# -gt 0 ]; do
  case "$1" in
    --output-dir) [ $# -ge 2 ] || { echo "error: --output-dir needs a value" >&2; exit 2; }
                  OUTPUT_DIR="$2"; shift 2 ;;
    --output-dir=*) OUTPUT_DIR="${1#--output-dir=}"; shift ;;
    -h|--help)    usage; exit 0 ;;
    *)            echo "error: unknown argument: $1 (see --help)" >&2; exit 2 ;;
  esac
done

command -v dpkg-deb >/dev/null 2>&1 || {
  echo "error: dpkg-deb not found on PATH (install dpkg / dpkg-dev)" >&2; exit 1; }
command -v go >/dev/null 2>&1 || {
  echo "error: go not found on PATH" >&2; exit 1; }

REPO_ROOT="$(git rev-parse --show-toplevel 2>/dev/null)" || {
  echo "error: not inside a git repository" >&2; exit 1; }
cd "$REPO_ROOT"

if [ -z "$OUTPUT_DIR" ]; then
  OUTPUT_DIR="$REPO_ROOT/dist"
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/isopress-deb-XXXXXX")"
on_exit() {
  local rc=$?
  if [ "$rc" -eq 0 ]; then
    rm -rf "$WORK"
  else
    echo "note: work dir kept for inspection: $WORK" >&2
  fi
}
trap on_exit EXIT

# ---------------------------------------------------------------------------
# 1. Release binary (the RELEASING build: static + stripped)
# ---------------------------------------------------------------------------
echo "[1/5] build: CGO_ENABLED=0 go build -ldflags '-s -w' ./cmd/isopress"
CGO_ENABLED=0 go build -ldflags "-s -w" -o "$WORK/isopress" ./cmd/isopress

# ---------------------------------------------------------------------------
# 2. Version: SINGLE SOURCE = the built binary's own --version output
#    ("isopress version X", catalog.ToolVersion).
# ---------------------------------------------------------------------------
echo "[2/5] version from binary --version"
RAW_VERSION="$("$WORK/isopress" --version | sed -n 's/^isopress version //p')"
[ -n "$RAW_VERSION" ] || {
  echo "error: could not parse 'isopress version X' from: $("$WORK/isopress" --version)" >&2; exit 1; }

# Debian upstream-version sanitisation: Debian versions may not contain
# '-' (dpkg reads it as the end of the upstream version and the start of
# a Debian revision), so map '-' to '~'. '~' also sorts BEFORE the plain
# release, which is the correct Debian ordering for a pre-release: a
# future 0.2.0-dev becomes 0.2.0~dev and sorts before 0.2.0.
PKG_VERSION="${RAW_VERSION//-/~}"
echo "  upstream: $RAW_VERSION -> package: $PKG_VERSION"

# ---------------------------------------------------------------------------
# 3. Stage the package tree
# ---------------------------------------------------------------------------
echo "[3/5] stage package tree"
STAGE="$WORK/stage"
mkdir -p "$STAGE/DEBIAN" \
         "$STAGE/usr/bin" \
         "$STAGE/usr/share/doc/isopress"

install -m 0755 "$WORK/isopress" "$STAGE/usr/bin/isopress"
install -m 0644 "$REPO_ROOT/README.md" "$STAGE/usr/share/doc/isopress/README.md"
# NOTICE must travel with any redistribution of the work (Apache-2.0
# section 4(d)); the .deb is a redistribution, so install it verbatim.
install -m 0644 "$REPO_ROOT/NOTICE" "$STAGE/usr/share/doc/isopress/NOTICE"

# Apache-2.0 copyright for the installed package. The full licence text
# (~11 KB) is NOT inlined: the deb references the canonical LICENSE in
# the source repository and the Apache URL. NOTICE ships alongside as
# /usr/share/doc/isopress/NOTICE (Apache-2.0 s4(d): NOTICE content must
# accompany redistributions, and a .deb IS a redistribution).
# Maintainer carries no email address on purpose (house rule: the site
# scans ban address patterns, and dpkg-deb does not require a valid one).
cat > "$STAGE/usr/share/doc/isopress/copyright" <<'EOF'
Package: isopress
Upstream name: isopress

Copyright: 2026 Clyde Solve

Licence: Apache-2.0
 Licensed under the Apache License, Version 2.0. The full licence text
 ships in the source repository (LICENSE file) and is not reproduced
 here; see https://www.apache.org/licenses/LICENSE-2.0.txt.
EOF

# Installed-Size = KiB of the files the package installs (usr/ only;
# DEBIAN/ is control data and is not counted).
INSTALLED_SIZE="$(du -sk "$STAGE/usr" | awk '{print $1}')"

# Depends is deliberately ABSENT: the binary is CGO_ENABLED=0 static, so
# there is no libc or any other shared-library dependency to declare.
cat > "$STAGE/DEBIAN/control" <<EOF
Package: isopress
Version: $PKG_VERSION
Section: utils
Priority: optional
Architecture: amd64
Maintainer: Clyde Solve
Installed-Size: $INSTALLED_SIZE
Recommends: xorriso
Description: lightweight command-line tool to back up large file trees to discs
 A lightweight command-line tool to make it easier to back up
 directories with a large number of files to multiple Blu-ray discs.
 Source data is planned across as many images as the media set requires,
 every written image is read back and checksum verified against the
 SQLite catalog, and the images can be restored or re-verified from the
 catalog snapshot each one carries.
EOF

# Recommends: xorriso justification - copy mode (plain directories)
# never runs xorriso, so the tool stays usable without it; the tool's
# own floor check probes PATH at image time and prints the clear
# install hint for all four distros (README "The xorriso dependency"),
# which is a better error than a hard Depends that drags xorriso into
# copy-only installs.

# Explicit modes for everything the package ships, independent of umask.
# CI containers commonly run with `umask 000`, under which `mkdir -p`
# creates 0777 directories and the heredoc-created control/copyright
# files land as 0666: dpkg-deb rejects a control directory outside
# 0755..0775 ("control directory has bad permissions 777"), and even
# below that check the archive would ship world-writable contents.
# Setting every mode here makes the package identical no matter what
# umask the caller (or runner) has.
find "$STAGE" -type d -exec chmod 0755 {} +
chmod 0755 "$STAGE/usr/bin/isopress"
chmod 0644 "$STAGE/usr/share/doc/isopress/README.md" \
           "$STAGE/usr/share/doc/isopress/copyright" \
           "$STAGE/usr/share/doc/isopress/NOTICE"
chmod 0644 "$STAGE/DEBIAN/control"

# ---------------------------------------------------------------------------
# 4. Build the .deb
# ---------------------------------------------------------------------------
echo "[4/5] dpkg-deb --build"
mkdir -p "$OUTPUT_DIR"
DEB_PATH="$OUTPUT_DIR/isopress_${PKG_VERSION}_amd64.deb"
dpkg-deb --root-owner-group --build "$STAGE" "$DEB_PATH" >/dev/null
chmod 0644 "$DEB_PATH"

# ---------------------------------------------------------------------------
# 5. Report
# ---------------------------------------------------------------------------
echo "[5/5] done"
echo "  package : isopress $PKG_VERSION (amd64, static, Installed-Size: ${INSTALLED_SIZE}k)"
echo "  output  : $DEB_PATH"
ls -lh "$DEB_PATH" | awk '{print "  size    : " $5}'
