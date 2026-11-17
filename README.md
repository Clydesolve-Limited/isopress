# isopress

`isopress` is a command-line backup tool for Linux, written in Go. It scans a
source tree into a SQLite catalog (per-file SHA-256 hashes plus filesystem
metadata), plans that cataloged data onto sized destinations, and writes the plan
as copy trees (plain directories) and/or ISO 9660 images produced with `xorriso`.
Every write is read back and hash verified before it is marked complete, every
image carries a `MANIFEST.json` and a `catalog-export.sqlite` snapshot so the
media can be restored without the original catalog, and both the media and the
source tree can be verified against the catalog at any time.

The tool is written for manual use and for cron or systemd timers: results go to
stdout, progress and diagnostics go to stderr, and exit codes follow a fixed
scheme that automation can act on.

Correct-usage procedures, common mistakes and recovery walkthroughs are in
the full usage guide, which ships with the complete repository and assumes
this README has been read.

## Contents

- [Project status](#project-status)
- [Requirements](#requirements)
- [Installation](#installation)
- [The xorriso dependency](#the-xorriso-dependency)
- [Commands](#commands)
- [Global flags](#global-flags)
- [Quickstart](#quickstart)
- [Output contract](#output-contract)
- [Exit codes](#exit-codes)
- [Running from cron](#running-from-cron)
- [Naming scheme and on-media layout](#naming-scheme-and-on-media-layout)
- [Known V1 limitations](#known-v1-limitations)
- [Development and testing](#development-and-testing)
- [Documentation](#documentation)
- [Licence](#licence)

## Project status

Implementation ran in seven sprints from S0 to S6, all closed on
2026-10-24. All nine commands are implemented and covered by the three
test tiers (unit, end-to-end, and the `xorriso`-gated integration tiers).
The tool is pre-release; the release process it follows is described next.

Releases are published on GitHub with the deb and AppImage attached (see the
[Releases page](https://github.com/Clydesolve-Limited/isopress/releases));
releases are cut as annotated git tags, and `isopress --version` prints the
version of the running binary. The current version is 0.1.0. The project is
licensed under Apache-2.0; see [Licence](#licence).

Whatever the version, run it against test data before trusting it with
anything you cannot recreate: prove a full backup-and-restore cycle on data
you can afford to lose before relying on the tool for data you cannot.

## Requirements

- **Linux.** The only supported platform.
- **Go 1.26.11 or newer** to build. That is the `go` directive in `go.mod`. The
  default `GOTOOLCHAIN=auto` setting makes the Go command download the exact
  toolchain if the installed one is older, so `go build` works out of the box.
  Check with `go version`.
- **`xorriso` >= 1.5.4** on `PATH`, required only for ISO image work. See
  [The xorriso dependency](#the-xorriso-dependency).
- Nothing else. There is no database server (SQLite is embedded through a pure
  Go driver), no C toolchain requirement, no daemon, and no configuration file.

## Installation

### Install a release package

Every GitHub release carries two artifacts: `isopress_<version>_amd64.deb`
and `isopress-<version>-amd64.AppImage`. Download one from the
[Releases page](https://github.com/Clydesolve-Limited/isopress/releases).

Debian package (Debian, Ubuntu and derivatives):

```sh
sudo dpkg -i isopress_<version>_amd64.deb
```

AppImage (any distribution, no installation step; it runs in place):

```sh
chmod +x isopress-<version>-amd64.AppImage
./isopress-<version>-amd64.AppImage --version
```

The deb installs `isopress` on `PATH`; the packaged binary is static, so
the package declares no hard dependencies (`xorriso` is recommended, not
required). Verify a deb install with:

```sh
isopress --version
```

Both artifacts are built from the same source and report the same
version string, for example `isopress version 0.1.0`.

### Build from source

```sh
git clone https://github.com/Clydesolve-Limited/isopress.git
cd isopress
go build -o bin/isopress ./cmd/isopress
```

The binary is written to `bin/isopress`. `make build` runs the same command.
Confirm the build with `./bin/isopress --help`.

For a fully static binary (no dynamic linker at all, convenient for copying the
binary to another Linux host):

```sh
CGO_ENABLED=0 go build -o bin/isopress ./cmd/isopress
```

`ldd` reports `not a dynamic executable` for that build.

### Install into the Go bin directory

```sh
go install ./cmd/isopress
```

This installs `isopress` into `$(go env GOPATH)/bin` (for example
`$HOME/go/bin/isopress`). Add that directory to `PATH` to run
`isopress` from anywhere.

### Dependency posture

The Go module dependencies are exact pins from the project's dependency
gate (completed 2026-10-18): `spf13/cobra` v1.10.2, `spf13/pflag` v1.0.10,
`schollz/progressbar/v3` v3.19.1, `golang.org/x/term` v0.47.0 and
`modernc.org/sqlite` v1.60.1 (pure Go, so no C toolchain is needed).
`go.sum` records their hashes and `go mod verify` checks them. There are no
floating version ranges and no third-party runtime services: the only external
program the tool ever runs is `xorriso`.

## The xorriso dependency

`xorriso` is an external binary, not a Go library. The tool discovers it on
`PATH` at run time and refuses to start an image operation when it is missing or
older than **1.5.4** (Debian bookworm ships 1.5.4, which is the tested floor,
and 1.5.6 is the version verified in this project environment). Check yours with
`xorriso -version`.

Install one package per distribution:

| Distribution | Command |
| --- | --- |
| Debian, Ubuntu | `apt install xorriso` |
| Fedora, RHEL | `dnf install xorriso` |
| Arch Linux | `pacman -S xorriso` |
| openSUSE | `zypper install xorriso` |

The tool itself prints all four hints on failure, in the same order (its
message labels the rows `Debian/Ubuntu`, `Fedora`, `Arch` and `openSUSE`):

```text
isopress: error: missing external dependency "xorriso": not found on PATH: exec: "xorriso": executable file not found in $PATH (install: Debian/Ubuntu: apt install xorriso; Fedora: dnf install xorriso; Arch: pacman -S xorriso; openSUSE: zypper install xorriso)
```

### Which commands need xorriso

Verified by running each command with `xorriso` removed from `PATH`:

| Command | Needs xorriso | Behaviour without it |
| --- | --- | --- |
| `scan`, `plan`, `report`, `clear-assignments`, `copy`, `validate` | Never | No probe happens at all; every one of these ran to exit 0 with an empty `PATH`. |
| `verify-media` | Only when an ISO image is in scope | Copy-tree verification runs without it and exits 0. When a selected image resolves to an `.iso`, the run fails fast with exit 3 before any extraction. |
| `restore` | Only when a supplied image is an `.iso` | Copy-tree restore and `--from-media` restore from a copy set run without it and exit 0. An ISO supply fails with exit 3 before anything is written to `--target`. |
| `build` | Always | Fails with exit 3 before staging anything and before a run record is created, so a failed probe leaves no run debris in the catalog. |

The distinction is deliberate: copy mode is independent of `xorriso`,
so a copy-only workflow works on hosts without the package. Tests that need a
real `xorriso` are gated on `exec.LookPath` and skip cleanly when it is absent.

## Commands

Nine commands, listed by `./bin/isopress --help`:

| Command | Purpose |
| --- | --- |
| `scan` | Walk a source tree, apply the exclusion filters, SHA-256 every included file, and reconcile the results into the catalog. Creates the catalog file when it does not exist. |
| `plan` | Assign every active, unassigned item of the set to destinations in one transaction, chunking only files that exceed one destination. |
| `report` | Run the identical planning pipeline read-only and print the projection: media count, per-destination fill, and the residual space on locked destinations. |
| `clear-assignments` | Clear plan assignments of the set in one transaction, keeping records, hashes and chunk rows so a re-plan reuses them. With no selector the whole set is cleared (unchanged, the pre-feature behaviour). With `--image N` (repeatable) or `--last` only the selected images' assignments, marks and destination rows are released; every other image keeps its assignments and media untouched, and the next plan re-mints the released ids at their lowest-unused slots, so a rewritten image keeps its number. |
| `copy` | Write the planned images as copy trees under `--dest`, with verify-then-mark ordering and resume after a crash. Before writing an image whose catalog content list is empty (a first write, or an image released by `clear-assignments`), `copy` first removes the existing `<dest>/<SET-ID>-NNN` directory after a `MANIFEST.json` ownership check and records the removal loudly (report line, `wipes` in `--json`, a run-log row, a stderr warning); a tree whose manifest names another set or image, or is unparseable, is refused with exit 10 and nothing is written to that image. |
| `build` | Master the planned images into `<SET-ID>-NNN.iso` files under `--dest`, then verify each image by extraction before it is renamed into place. |
| `verify-media` | Re-read built media (ISO images or copy trees) and compare every file and chunk against the catalog hashes. Read-only. |
| `validate` | Compare the live source tree against the catalog and classify every record: `ok`, `changed`, `corrupt`, `broken`, `missing`, `new`. Read-only. |
| `restore` | Restore a selection into `--target`, reassembling and re-verifying chunked files, from the catalog or from the media alone. |

Synopses as printed by `--help`:

```text
isopress scan ROOT [flags]
isopress plan [flags]
isopress report [flags]
isopress clear-assignments [flags]
isopress copy [flags]
isopress build [flags]
isopress verify-media [flags]
isopress validate [flags]
isopress restore [flags]
```

Every command's `--help` also lists the exit codes that command can emit and
how `--json` behaves for it; the root `--help` prints the whole exit-code
scheme.

Command-local flags:

| Command | Flag | Description |
| --- | --- | --- |
| `plan`, `report` | `--size SIZE` | Destination nominal size (required). `25`, `25G`, `25GB`, `25Gi`, `25GiB` are accepted; a bare number means decimal GB. `MB` and friends are rejected. |
| `plan`, `report` | `--media-profile NAME` | `bd-r-sl` (25 GB, 23 GB usable), `bd-r-dl` (50 GB, 46 GB usable) or `disk` (98% of nominal, the default). A fixed optical profile forces `--size` to its exact nominal value. |
| `copy` | `--dest DIR` | Destination root for the copy trees (required). |
| `copy` | `--image N` | Write only that planned image number (repeatable). |
| `clear-assignments` | `--image N` | Unlock only that planned image number (repeatable). Ids are 1-based and must be planned images of the set (exit 2 otherwise, naming the planned ids). |
| `clear-assignments` | `--last` | Unlock the highest planned image id (unioned with any `--image`). |
| `build` | `--dest DIR` | Directory the `<SET-ID>-NNN.iso` images land in (required). |
| `build` | `--image N` | Build only that planned image number (repeatable). |
| `build` | `--verify-after-build` | Extraction self-verification after mastering (default on; `--verify-after-build=false` skips it and trades away the content guarantee). |
| `build` | `--clean-failed` | Remove the `.partial` image of a failed build instead of keeping it for inspection. |
| `verify-media` | `--image N` | Verify only that image number (repeatable). |
| `restore` | `--target DIR` | Directory to restore into (required). |
| `restore` | `--path RELPATH` | Restore this exact catalog relpath (repeatable). |
| `restore` | `--subtree DIR` | Restore everything under this relpath prefix (component-aware, so `docs` matches `docs/a` but never `docs-old`). |
| `restore` | `--image-dir DIR` | Directory holding `<SET-ID>-NNN.iso` images to read from. |
| `restore` | `--copy-tree-root DIR` | Directory holding `<SET-ID>-NNN` copy trees to read from. |
| `restore` | `--image N=PATH` | Supply image N from PATH (repeatable); a file is an ISO, a directory is a copy tree. |
| `restore` | `--from-media PATH` | Media-alone restore: PATH is a copy set or an extracted ISO tree carrying `MANIFEST.json` and `catalog-export.sqlite`. |

For the step-by-step selective-unlock top-up procedure (add files, unlock
the last image, rewrite one image in place, with executed transcripts),
see the full usage guide, which ships with the complete repository; this
README stays the reference, the usage guide carries the procedure.

## Global flags

These flags are accepted by every command (they appear under "Global Flags" in
each `--help`):

| Flag | Description |
| --- | --- |
| `--catalog PATH` | Path to the SQLite catalog file. `scan` creates it when absent; every other command treats a missing file as a catalog error (exit 4). |
| `--set-id ID` | Backup set identifier. Without it, `scan` derives `<root-basename>-<ingest-date-UTC>`, for example `src-20261024`. |
| `--exclude PATTERN` | Exclusion pattern, repeatable, applied at scan time only. |
| `--exclude-from FILE` | File with one exclusion pattern per line (`#` comments and blank lines ignored). |
| `--change-detection MODE` | Change-detection comparison set for `scan`, `copy`, `build` and `validate`: `auto` (default; `statfs` the source root, CIFS/SMB3 sources degrade to size+mtime), `full` (force size+mtime+dev+inode), `metadata` (force size+mtime). Other commands accept and validate the flag but ignore its value. |
| `--json` | Emit the final report as one JSON document on stdout. |
| `--strict` | Promote any item failure to a fatal stop-at-end error (exit 1). Inert for `plan`, `report` and `clear-assignments`, which have no per-item failure path. |
| `--quiet`, `-q` | Suppress visual feedback; periodic plain progress lines remain. |
| `--verbose`, `-v` | Print verbose transcripts on stderr (a fatal error additionally prints a goroutine stack trace). |
| `--work-dir PATH` | Workspace for staging and temporary files (default: OS temp directory). |
| `--image-dir DIR` | Directory containing `<SET-ID>-NNN.iso` images for `restore`. |

`--quiet` and `--verbose` are not exclusive and may be combined.

<!-- quickstart:start -->
## Quickstart

The session below runs a complete backup and restore against a small fixture.
Every command, and the exact output each one prints, is reproduced from an
execution of this section against a fresh directory. Exit codes are stated under
each step.

Work from a clone with a built binary (`go build -o bin/isopress
./cmd/isopress`).

### Step 1: create a fixture tree

```sh
BT=./bin/isopress
W=/tmp/isopress-quickstart
rm -rf "$W"
mkdir -p "$W/src/docs" "$W/src/images"
printf 'quarterly report, revision 3\n' > "$W/src/docs/report.txt"
printf 'notes for the third revision\n' > "$W/src/docs/notes.md"
head -c 1048576 /dev/urandom > "$W/src/images/photo.jpg"
ln -s docs/report.txt "$W/src/report-link"
mkdir -p "$W/src/empty-dir"
```

That gives three regular files, one symlink and one empty directory. All later
commands use `$W` for the catalog (`$W/catalog.sqlite`), the destination
(`$W/backup`) and the restore target (`$W/restored`).

### Step 2: scan

`scan` creates the catalog file when it does not exist yet.

```sh
$BT --catalog "$W/catalog.sqlite" --set-id demo scan "$W/src"
```

Exit code: 0.

stdout:

```text
scanned 4 entries (3 regular, 1 symlinks), 1.0 MB hashed
set: demo (run 1)
added 4, modified 0, unchanged 0, missing 0, unreadable 0, changed 0
empty dirs 1
```

stderr (plain progress lines, because stderr is not a terminal here):

```text
scan(hash): files 1/3, 29 B/1.0 MB (0%)
scan(hash): files 4, 1.0 MB; added 4, modified 0, unchanged 0, missing 0, unreadable 0, changed 0, empty dirs 1
```

The first stderr line is the periodic progress line; its counters are live, so
they depend on timing (which file finishes hashing first). The final summary
line is always identical. Both fixture text files are 29 bytes, so the first
line above also reproduces on a rerun of this session; it would change only if
the 1 MiB image file finished first.

### Step 3: plan

`plan` needs a destination size. `--size 10G` is 10 decimal GB; the default
`disk` profile keeps 98% of it usable.

```sh
$BT --catalog "$W/catalog.sqlite" --set-id demo plan --size 10G
```

Exit code: 0.

stdout:

```text
plan: set demo (profile disk: nominal 9.3 GiB, usable 9.1 GiB, margin 2%)
media: 1 destinations total (1 new, 0 pre-existing)
image 1: 4 items, 1 dirs, 1.0 MB / 9.1 GiB (0%)
  reserved overhead: 2.1 MB (fixed 2.1 MB + 5 entries), free after build: 9.1 GiB
residual space on locked destinations: 0 B across 0 images
assigned this run: 3 files, 0 chunks, 1 symlinks, 1 dirs (hard-link secondaries excluded: 0)
```

stderr:

```text
plan: media 1 (1 new); files 3, chunks 0, symlinks 1, dirs 1
```

### Step 4: report

`report` runs the same pipeline read-only, so it is safe to run during another
run and during cron overlap. After step 3 the destination is locked, so the
projection shows the accept-waste residual.

```sh
$BT --catalog "$W/catalog.sqlite" --set-id demo report --size 10G
```

Exit code: 0.

stdout:

```text
report (projection): set demo (profile disk: nominal 9.3 GiB, usable 9.1 GiB, margin 2%)
media: 1 destinations total (0 new, 1 pre-existing)
image 1: 4 items, 1 dirs, 1.0 MB / 9.1 GiB (0%) [locked]
  reserved overhead: 2.1 MB (fixed 2.1 MB + 5 entries), free after build: 9.1 GiB
residual space on locked destinations: 9.1 GiB across 1 images
nothing to plan: no unassigned items
```

stderr:

```text
report: nothing to plan (media 1)
```

### Step 5: copy

`copy` writes one copy-set directory per image. No `xorriso` is involved at any
point.

```sh
$BT --catalog "$W/catalog.sqlite" --set-id demo copy --dest "$W/backup"
```

Exit code: 0.

stdout:

```text
copy: set demo (run 2)
image 1: copied 5, already-copied 0, skipped 0, failed 0, dirs 1, 1.0 MB (complete: true, manifest: true)
totals: copied 5, already-copied 0, skipped 0, failed 0, dirs 1, 1.0 MB (status: ok)
```

stderr:

```text
copy: files 1/5, 29 B/1.0 MB (0%)
copy: files 5 copied (0 already present), 1.0 MB; skipped 0, failed 0, dirs 1, metadata warnings 0
```

### Step 6: verify the copy tree

`verify-media` resolves each image from the catalog. After step 5 the recorded
destination is the copy tree, so this run needs no `xorriso` and reads the tree
directly.

```sh
$BT --catalog "$W/catalog.sqlite" --set-id demo verify-media
```

Exit code: 0.

stdout:

```text
verify-media: set demo
image 1: verified /tmp/isopress-quickstart/backup/demo-001 (copy) matched 7, mismatched 0, missing 0, unexpected 0, unreadable 0, stamped true
totals: images 1, verified 1, matched 7, mismatched 0, missing 0, unexpected 0, unreadable 0 (status: ok)
```

stderr:

```text
verify(hash): files 1/5, 1.6 KB/1.2 MB (0%)
verify-media: 1 image(s) verified, 0 anomalous (matched 7 entries, mismatched 0, missing 0, unexpected 0, unreadable 0)
```

### Step 7: build an ISO image (needs xorriso)

Skip this step on a host without `xorriso`; everything else in this section
still works. With xorriso present the same plan is mastered a second time as an
ISO 9660 image, self-verified by extraction, and only then renamed into place.

```sh
$BT --catalog "$W/catalog.sqlite" --set-id demo build --dest "$W/backup"
```

Exit code: 0.

stdout:

```text
build: set demo (run 3)
image 1: built /tmp/isopress-quickstart/backup/demo-001.iso (1.5 MB, content entries 7, verified 7, verify skipped: false, warnings 0)
totals: built 1, failed 0, 1.5 MB (status: ok, verified entries 7)
```

stderr:

```text
build(stage): files 1/5, 29 B/1.0 MB (0%)
build: 1 image(s) built (1.5 MB), 0 failed, warnings 0
```

### Step 8: verify the ISO

`verify-media` now resolves the image to the ISO, so this run re-reads it through
one `xorriso` extraction (no mount) and hashes everything again.

```sh
$BT --catalog "$W/catalog.sqlite" --set-id demo verify-media
```

Exit code: 0.

stdout:

```text
verify-media: set demo
image 1: verified /tmp/isopress-quickstart/backup/demo-001.iso (iso) matched 7, mismatched 0, missing 0, unexpected 0, unreadable 0, stamped true
totals: images 1, verified 1, matched 7, mismatched 0, missing 0, unexpected 0, unreadable 0 (status: ok)
```

stderr:

```text
verify(extract): 0 B/1.2 MB (0%)
verify-media: 1 image(s) verified, 0 anomalous (matched 7 entries, mismatched 0, missing 0, unexpected 0, unreadable 0)
```

### Step 9: restore into a fresh target

`--target` is required and restore never falls back to the original source path.
`--copy-tree-root` reads the copy set written in step 5.

```sh
$BT --catalog "$W/catalog.sqlite" --set-id demo restore --target "$W/restored" --copy-tree-root "$W/backup"
```

Exit code: 0.

stdout:

```text
restore: set demo
target: /tmp/isopress-quickstart/restored (selection: whole-set)
image 1: restored /tmp/isopress-quickstart/backup/demo-001 (copy) - restored 5, skipped 0, failed 0 (1.0 MB)
totals: images 1, selected 4 file(s) (1.0 MB), restored 5, skipped 0, failed 0, unclaimed 0 (status: ok)
```

stderr:

```text
restore: files 1/5, 29 B/1.0 MB (0%)
restore: set demo: 1 image(s), selected 4 file(s) (1.0 MB), restored 5, skipped 0, failed 0, unclaimed 0 (status ok)
```

### Step 10: spot-check the restore

```sh
diff -r "$W/src" "$W/restored"
```

Exit code: 0, no output. `diff -r` compares symlink targets and empty
directories too.

```sh
sha256sum "$W/src/docs/report.txt" "$W/restored/docs/report.txt"
```

Exit code: 0.

stdout:

```text
f2b1929d574791b1b12be2cd72adb9f1ae8da75233574e8e19f2fe770b4aae29  /tmp/isopress-quickstart/src/docs/report.txt
f2b1929d574791b1b12be2cd72adb9f1ae8da75233574e8e19f2fe770b4aae29  /tmp/isopress-quickstart/restored/docs/report.txt
```

### Step 11 (optional): validate the source against the catalog

```sh
$BT --catalog "$W/catalog.sqlite" --set-id demo validate
```

Exit code: 0.

stdout:

```text
validate: set demo
root: /tmp/isopress-quickstart/src (comparison set: full)
ok 4, changed 0, corrupt 0, broken 0, missing 0, new 0, anomalies 0
totals: compared 4 records, status ok (ok 4, changed 0, corrupt 0, broken 0, missing 0, new 0)
```

stderr:

```text
validate: files 1/4, 29 B/1.0 MB (0%)
validate: set demo: compared 4 records, ok 4, changed 0, corrupt 0, broken 0, missing 0, new 0 (anomalies 0, status ok)
```

`validate` exits 10 when it finds any anomaly, so a cron job can treat a non-zero
status as "the source drifted".

### Step 12 (optional): restore from the media alone

The copy set carries its own catalog snapshot, so the original catalog file is
not needed. This invocation passes neither `--catalog` nor `--set-id`; the set
identity comes from the media's manifest and the export is the catalog.

```sh
$BT --from-media "$W/backup/demo-001" restore --target "$W/restored-media"
```

Exit code: 0.

stdout:

```text
restore: set demo
target: /tmp/isopress-quickstart/restored-media (selection: whole-set)
image 1: restored /tmp/isopress-quickstart/backup/demo-001 (copy) - restored 5, skipped 0, failed 0 (1.0 MB)
totals: images 1, selected 4 file(s) (1.0 MB), restored 5, skipped 0, failed 0, unclaimed 0 (status: ok)
```

stderr:

```text
restore: files 1/5, 29 B/1.0 MB (0%)
restore: set demo: 1 image(s), selected 4 file(s) (1.0 MB), restored 5, skipped 0, failed 0, unclaimed 0 (status ok)
```

`--catalog` and `--from-media` are mutually exclusive; passing both is a usage
error (exit 2).

### Step 13 (optional): multi-image spanning

Chunking only triggers for a file that exceeds the effective chunk size, which is
`min(20 GiB, usable_size of the destination)`. The smallest destination the CLI
accepts is 1 GB nominal (980 MB usable on the `disk` profile), so the smallest
runnable spanning demo needs a source file of about 1 GB. The source file below
is sparse (`truncate`), so it costs no disk space until it is copied; the copy
tree and the restore target together need about 2.1 GB free.

```sh
S=/tmp/isopress-span
rm -rf "$S"
mkdir -p "$S/src"
truncate -s 1073741824 "$S/src/huge.img"
printf 'tiny\n' > "$S/src/small.txt"
$BT --catalog "$S/catalog.sqlite" --set-id span scan "$S/src"
```

Exit code: 0.

stdout:

```text
scanned 2 entries (2 regular, 0 symlinks), 1.0 GiB hashed
set: span (run 1)
added 2, modified 0, unchanged 0, missing 0, unreadable 0, changed 0
empty dirs 0
```

A 1 GB destination forces the split:

```sh
$BT --catalog "$S/catalog.sqlite" --set-id span plan --size 1G
```

Exit code: 0.

stdout:

```text
plan: set span (profile disk: nominal 1.0 GB, usable 980.0 MB, margin 2%)
media: 2 destinations total (2 new, 0 pre-existing)
image 1: 1 items, 0 dirs, 977.9 MB / 980.0 MB (99%)
  reserved overhead: 2.1 MB (fixed 2.1 MB + 1 entries), free after build: 0 B
image 2: 2 items, 0 dirs, 95.8 MB / 980.0 MB (9%)
  reserved overhead: 2.1 MB (fixed 2.1 MB + 2 entries), free after build: 882.1 MB
residual space on locked destinations: 0 B across 0 images
assigned this run: 1 files, 2 chunks, 0 symlinks, 0 dirs (hard-link secondaries excluded: 0)
```

```sh
$BT --catalog "$S/catalog.sqlite" --set-id span copy --dest "$S/backup"
```

Exit code: 0.

stdout:

```text
copy: set span (run 2)
image 1: copied 1, already-copied 0, skipped 0, failed 0, dirs 0, 977.9 MB (complete: true, manifest: true)
image 2: copied 2, already-copied 0, skipped 0, failed 0, dirs 0, 95.8 MB (complete: true, manifest: true)
totals: copied 3, already-copied 0, skipped 0, failed 0, dirs 0, 1.0 GiB (status: ok)
```

The file was split into two 1-based, 5-digit chunk names, one per image:

```sh
find "$S/backup" -path '*chunks*' -type f -printf '%s %p\n' | sort -n
```

Exit code: 0.

stdout:

```text
95840512 /tmp/isopress-span/backup/span-002/chunks/huge.img/00002
977901312 /tmp/isopress-span/backup/span-001/chunks/huge.img/00001
```

Verify both images:

```sh
$BT --catalog "$S/catalog.sqlite" --set-id span verify-media
```

Exit code: 0.

stdout:

```text
verify-media: set span
image 1: verified /tmp/isopress-span/backup/span-001 (copy) matched 3, mismatched 0, missing 0, unexpected 0, unreadable 0, stamped true
image 2: verified /tmp/isopress-span/backup/span-002 (copy) matched 4, mismatched 0, missing 0, unexpected 0, unreadable 0, stamped true
totals: images 2, verified 2, matched 7, mismatched 0, missing 0, unexpected 0, unreadable 0 (status: ok)
```

Then restore the reassembled file and compare it with the source:

```sh
$BT --catalog "$S/catalog.sqlite" --set-id span restore --target "$S/restored" --copy-tree-root "$S/backup"
```

Exit code: 0.

stdout:

```text
restore: set span
target: /tmp/isopress-span/restored (selection: whole-set)
image 1: restored /tmp/isopress-span/backup/span-001 (copy) - restored 0, skipped 0, failed 0 (977.9 MB)
image 2: restored /tmp/isopress-span/backup/span-002 (copy) - restored 2, skipped 0, failed 0 (95.8 MB)
totals: images 2, selected 2 file(s) (1.0 GiB), restored 2, skipped 0, failed 0, unclaimed 0 (status: ok)
```

```sh
cmp "$S/src/huge.img" "$S/restored/huge.img"
```

Exit code: 0, no output: the reassembled file is byte-identical to the
source, which means both chunk hashes and the whole-file hash matched.

A single file larger than the default 20 GiB chunk size is the same mechanism at
full scale; producing one needs a source file over 20 GiB and is left to the
`large` test tier (see [Development and testing](#development-and-testing)).

<!-- quickstart:end -->

## Output contract

The stream guarantees, which hold for every command:

- **stdout** carries results and reports only: the end-of-run report tables, or
  exactly one JSON document when `--json` is given.
- **stderr** carries the progress renderer, warnings, errors, and verbose
  transcripts. Progress chooses its form from stderr's terminal state: an
  interactive terminal gets bars and spinners, anything else (pipe, cron,
  systemd, redirect) gets plain lines automatically.
- `--json` never changes a run's exit code. A run that aborts before doing any
  work (usage, dependency or catalog refusals) prints nothing at all on stdout,
  so a parser never sees a partial document.
- Plain progress lines are emitted at most once every 5 seconds, plus one final
  summary line.

Using the `$BT` and `$W` variables the quickstart set up:

```sh
$BT --catalog "$W/catalog.sqlite" --set-id demo --json validate
```

Exit code: 0. stdout is one JSON document; the human summary still lands on
stderr:

```json
{
  "set_id": "demo",
  "root": "/tmp/isopress-quickstart/src",
  "status": "ok",
  "comparison_set": "full",
  "records": 4,
  "ok": 4,
  "changed": 0,
  "corrupt": 0,
  "broken": 0,
  "missing": 0,
  "new": 0,
  "anomalies": 0,
  "files": []
}
```

Frozen `--json` key sets for the two commands whose output shapes the
selective-unlock feature extends. The counts below were taken from the
shipped binary (`jq 'keys_unsorted | length'` over a real `--json` run of
each command):

- `clear-assignments --json`: 10 top-level keys in both modes: `mode`,
  `set_id`, `run_id`, `files_cleared`, `chunks_cleared`, `dirs_cleared`,
  `destinations_removed`, `chunk_hashes_retained`, `unlocked_images`,
  `nothing_to_clear`. `unlocked_images` is the selective-unlock addition
  (9 -> 10): the selected ids for a scoped run, every removed id for a
  full clear, `[]` on a completed nothing-to-clear no-op.
- `copy --json`: 13 top-level keys: `set_id`, `run_id`, `dest`, `status`,
  `copied`, `already_copied`, `skipped`, `failed`, `dirs`, `bytes`,
  `metadata_warnings`, `images`, `wipes`. `wipes` is the copy-time
  wipe-gate addition (12 -> 13): one record per removed tree with `image`,
  `path`, `entries_removed` and `bytes_removed`, `[]` when nothing was
  wiped. The key is always present. A run with failures carries one
  additional conditional key, `failures` (14 keys total).

## Exit codes

The exit-code scheme, implemented in `internal/report/exit.go` and
swept per command by `internal/cli/taxonomy_table_test.go`:

| Code | Meaning | Emitted by |
| --- | --- | --- |
| 0 | Success, no item failures. | Every command. |
| 1 | Fatal unexpected error (the bug class). A goroutine stack trace is printed on stderr under `--verbose`. | Every command; also the `--strict` promotion of exit 10. |
| 2 | Usage error: bad flags, missing arguments, unsatisfiable combinations (including a size with an unsupported unit, an invalid `--media-profile`, a missing `--exclude-from` file, and a scan of the filesystem root with no exclusions). `plan` and `report` additionally refuse a re-plan over a partially-assigned chunk set whose stored chunks cannot be reused (`ErrPartialChunkReplan`), with four reasons: the effective chunk size changed (a changed chunk profile), the file now fits one destination (a whole-file fit), the stored chunk rows do not verify against the current decomposition, or the hard-link family rule excludes a secondary row, printed with both remedies (keep the current media profile, or clear the whole set), except on the family-rule leg, which carries only the clear-set remedy (no profile choice can place a secondary row). | Every command. |
| 3 | Missing external dependency: `xorriso` absent or older than 1.5.4 when an image operation was requested. Also used by `restore` for required media that cannot be supplied on a non-interactive run. | `build`, `verify-media` (ISO in scope), `restore`. |
| 4 | Catalog error: cannot open, corrupt, schema too new (including a future-version `catalog-export.sqlite` on `--from-media`), migration failure, or a missing catalog file for any command other than `scan`. | Every command. |
| 5 | Catalog busy: another mutating run holds the advisory lock. Read-only commands never take the lock and cannot return 5. | The mutating commands: `scan`, `plan`, `copy`, `build`, `clear-assignments`. |
| 10 | Completed with per-item failures. The report enumerates every failure and automation decides policy. `--strict` turns this into exit 1. | `scan` (unreadable or changed-during-scan files), `copy`, `build`, `verify-media`, `validate`, `restore`. |
| 130 | Interrupted by SIGINT or SIGTERM (the shell convention 128 + SIGINT for both signals). The in-flight item is finalized and the run row is stamped `interrupted`. | Every command. |

Notes on the two classes that need care:

- **Exit 10 versus 1.** Exit 10 means the operation ran to completion and the
  report lists every failure, which is what cron-style automation usually wants.
  `--strict` promotes it to a fatal exit 1 for fail-loud setups. `plan`, `report`
  and `clear-assignments` have no per-item failure path at all: their work either
  commits atomically or fails, so `--strict` is accepted but inert for them.
- **Exit 3 has two causes.** The original meaning of this class is a missing or too
  old `xorriso`. `restore` reuses the same class when required media cannot be
  supplied: a non-interactive run prints one machine-readable `required-image:`
  line per missing image on stderr and exits 3, and an interactive prompt that is
  aborted with `q` does the same. The cause is always named on stderr, so
  automation can tell the two apart.

Examples of the scheme in action. Each command and each error line below is an
observed transcript; only the diagnostic lines of stderr are shown, so the
periodic progress lines and the scan warning line that precede them are omitted.
The session assumes `isopress` on `PATH`, a catalog `/tmp/demo-catalog.sqlite`
whose set `demo` has already been scanned and planned, a source tree `src` that
contains one unreadable file, and no file at `/tmp/no-such-catalog.sqlite`.

```text
$ isopress --catalog /tmp/demo-catalog.sqlite --set-id demo plan --size 100MB
isopress: error: units: size "100MB" has unsupported unit "mb" (allowed: G, GB, Gi, GiB; a bare number means GB): invalid size
run 'isopress --help' or 'isopress <command> --help' for usage
$ echo $?
2

$ isopress --catalog /tmp/no-such-catalog.sqlite --set-id demo plan --size 10G
isopress: error: catalog: open error at /tmp/no-such-catalog.sqlite: catalog file not found or unreadable (create one first): stat /tmp/no-such-catalog.sqlite: no such file or directory
$ echo $?
4

$ PATH=/empty $(go env GOPATH)/bin/isopress --catalog /tmp/demo-catalog.sqlite --set-id demo build --dest /tmp/out
isopress: error: missing external dependency "xorriso": not found on PATH: exec: "xorriso": executable file not found in $PATH (install: Debian/Ubuntu: apt install xorriso; Fedora: dnf install xorriso; Arch: pacman -S xorriso; openSUSE: zypper install xorriso)
$ echo $?
3

$ isopress --catalog /tmp/demo-catalog.sqlite --set-id demo plan --size 10G   # while another run holds the lock
isopress: error: catalog: busy: /tmp/demo-catalog.sqlite is locked by another isopress process (lock file /tmp/demo-catalog.sqlite.lock): the catalog is single-writer (one mutating or planning run at a time); retry after the other run finishes
$ echo $?
5

$ isopress --catalog /tmp/demo-catalog.sqlite --set-id demo scan src          # with one unreadable file
isopress: error: completed with 1 item failure(s) (see the run report)
$ echo $?
10

$ isopress --strict --catalog /tmp/demo-catalog.sqlite --set-id demo scan src  # same run
isopress: error: scan completed with 1 item failure(s); --strict promotes this to a fatal error
$ echo $?
1
```

Exit 1 (the bug class) has no honest manual reproduction; it is pinned by
`TestFatalUnexpectedExit1VerboseStackDump` in
`internal/cli/taxonomy_table_test.go`, which drives the real command tree through
the copy failure seam and asserts both the code and the `--verbose` stack trace.

## Running from cron

Every line below follows the [output contract](#output-contract): the report goes
to the report file (stdout), diagnostics go to the log (stderr), and the exit
code tells the caller what happened.

```cron
# Nightly scan of a tree, with the broad-root exclusions kept in a file.
17 2 * * * isopress --catalog /var/lib/backup/catalog.sqlite --quiet --exclude-from /etc/isopress/excludes scan /srv/data >> /var/log/isopress/scan.report 2>> /var/log/isopress/scan.log

# Plan and write onto a 25 GB BD-R profile, 02:40.
40 2 * * * isopress --catalog /var/lib/backup/catalog.sqlite --quiet plan --size 25G --media-profile bd-r-sl >> /var/log/isopress/plan.report 2>> /var/log/isopress/plan.log
45 2 * * * isopress --catalog /var/lib/backup/catalog.sqlite --quiet copy --dest /mnt/backup >> /var/log/isopress/copy.report 2>> /var/log/isopress/copy.log

# Machine-readable projection once a week.
5 3 * * 0 isopress --catalog /var/lib/backup/catalog.sqlite --json report --size 25G --media-profile bd-r-sl > /var/lib/backup/plan.json 2>> /var/log/isopress/plan.log
```

Practical points:

- **stdout versus stderr.** Redirect them separately, as above. stdout stays
  machine-parseable (one report, or one JSON document with `--json`); stderr
  carries the plain progress lines and every diagnostic, which is what a log file
  wants.
- **`--quiet`.** When stderr is not a terminal the plain renderer is selected
  automatically, so `--quiet` changes nothing in cron; it is there to force the
  same plain output when you run the same command line by hand on a terminal.
  Bars and spinners are never emitted into a log.
- **Exit codes as alarms.** `10` means "finished, but the report lists
  failures"; add `--strict` if you would rather get `1`. `5` means another
  mutating run still holds the catalog lock: stagger the schedules or accept the
  refusal, and never wrap the tool in a retry loop that hides it.
- **Read-only work never collides.** `report`, `verify-media` and `validate` take
  no lock, so they can run during a long `copy` or `build` and see a consistent
  WAL snapshot.
- **Exclusions are scan-time only** and are snapshotted into the catalog for
  reproducibility.

### Exclusion syntax

Patterns come from `--exclude` (repeatable) or `--exclude-from` (one pattern per
line, `#` comments and blank lines ignored), are case-sensitive, and are matched
against the path relative to the scan root with the shell-glob subset below
(pinned by `internal/ingest/matcher_test.go`):

| Pattern form | Meaning | Example |
| --- | --- | --- |
| No `/` in the pattern | Matches a single path component at any depth (basename match, gitignore-like). | `--exclude '*.iso'` drops every `.iso` at any depth. |
| Contains `/` | Anchored to the scan root. `*` does not cross `/`, `**` does. | `--exclude 'var/log/**'`. |
| Trailing `/` | Directories only; the walker prunes the whole subtree. | `--exclude 'proc/'` never reads `proc`. |
| Leading `/` | Accepted as anchor notation and stripped: `/var/log` is the same as `var/log`. | |
| `!` | Not special. V1 has no negation rules; `!foo` matches entries literally named `!foo`. | |
| `[...]` and `\` | Not special. Character classes and backslash escapes are literal characters. | |
| Empty pattern | Rejected as a usage error (exit 2). Empty lines in `--exclude-from` are ignored. | |

Excluded paths never enter the catalog, so the catalog and the plan cannot
diverge.

### Recommended exclusions for broad roots

There is no implicit default exclusion set, because silently dropping data from a
backup tool is worse than asking for explicit rules. Scanning the filesystem root
`/` without at least one `--exclude` or `--exclude-from` argument is refused with
exit 2 (pinned by `TestScanRootRefusalExit2` in `internal/cli/scan_test.go`).

The recommended set for scanning broad roots, which is also what the refusal
message suggests:

```text
# /etc/isopress/excludes: pseudo-filesystems and transient trees
proc/
sys/
dev/
run/
tmp/
mnt/
media/
```

```sh
isopress --catalog /var/lib/backup/catalog.sqlite --exclude-from /etc/isopress/excludes scan /
```

Only run that when you mean it: a root scan hashes everything the exclusion file
does not prune.

Each entry has a trailing `/`, so the walker prunes that subtree at the directory
itself instead of hashing it. This list is documented deliberately rather than
shipped as a hidden default.

## Naming scheme and on-media layout

Deterministic naming and the copy-set layout, both implemented in
`internal/media/layout.go`:

- **Set identifier.** `--set-id` when given, otherwise
  `<sanitized root basename>-<ingest date in UTC>` (observed default:
  `src-20261024`). It must be a single path component: no `/`, no NUL, valid
  UTF-8, and no newline or carriage return (ISO builds reject those two with a
  usage error because they break the volume id and the file name).
- **Image numbers.** 1-based, zero-padded to three digits. Above 999 the names
  gain digits (`demo-1000`), so sort them numerically, not as strings.
- **Copy set directory:** `<dest>/<SET-ID>-NNN/`.
- **ISO image file:** `<dest>/<SET-ID>-NNN.iso`, written first as
  `<SET-ID>-NNN.iso.partial` and renamed only after verification passes (with
  `--verify-after-build=false` the extraction verification is skipped and the
  rename follows xorriso's exit 0 alone). The ISO volume id (`-V`) is
  `<SET-ID>-<NNN>`, the name the file carries without `.iso`, so every disc
  of a multi-disc set mounts under its own label (amended 2026-11-04; before
  that it was the bare set id, so two discs of one set mounted identically).
  The 32-byte ISO 9660 limit applies to the whole id: a long set id is cut
  back deterministically and the `-<NNN>` suffix is appended last, so the
  number always survives (image 1000 reads `-1000`, exactly as in the file
  name). Observed on a real build: reading `manga-20261020-001.iso` back with
  `xorriso -indev <iso> -pvd_info` reports the volume id
  `manga-20261020-001`.
- **Inside every copy set and every ISO** (identical tree in both modes):

```text
<SET-ID>-NNN/
  files/<relpath>            whole files, plus the empty directories of the set
  chunks/<relpath>/00001     chunk files of a chunked parent, 1-based, 5 digits
  MANIFEST.json              per-image manifest: entries, chunk tables, link groups, whole-set placement
  catalog-export.sqlite      VACUUM INTO snapshot of the catalog, taken after planning
```

- **Partial files.** Payload is written as `<name>.partial` and fsynced, then
  renamed to its final name (with a directory fsync), and only then verified by
  read-back; a crash mid-write therefore never leaves a plausible final name,
  and the file is marked copied only after that verification. Chunk files are
  named only under `chunks/`, so `00001.partial` cannot be mistaken for media.
- **stdout guarantee.** The report you see on stdout (or as one JSON document
  under `--json`) is the same information on every path; progress, warnings and
  errors go to stderr. See [Output contract](#output-contract).

The manifest and the catalog export are what make each image self-sufficient:
`restore --from-media` works with no catalog file at all, because the export is
opened read-only as the catalog and the manifest supplies set identity and
placement.

## Known V1 limitations

Accepted consequences of recorded scope decisions, not bugs. They are
listed here so they are not mistaken for defects.

1. **Unbounded accumulation.** There is no retention, pruning or reclamation:
   catalog records and media consumption only grow across rescans, and superseded
   content stays on old media indefinitely. *Impact:* plan capacity as if every
   backup only ever adds, and retire old media yourself; the tool will not tell
   you when a disc is obsolete.
2. **Locked assignments leave gaps.** Files are locked to their destinations and
   later files never backfill older ones, so partially filled destinations waste
   their residual space (the fill strategy that mitigates this was
   resolved as accept-waste). *Impact:* many small runs produce unevenly filled
   destinations. The lever is `clear-assignments`, which drops every assignment
   in one transaction (or only the selected images' assignments with `--image`
   or `--last`; records and stored hashes are kept either way), so a re-plan
   repacks what it released: the whole set after a full clear, only those
   images after a scoped unlock. `report` quantifies the waste as "residual
   space on locked destinations".
3. **Sparse files are expanded on ISO images.** ISO 9660 with Rock Ridge cannot
   represent sparse extents, so a sparse source occupies its full logical size on
   image media; copy destinations may preserve sparseness. *Impact:* a sparse VM
   image that costs little on disk can fill a whole disc once mastered. Planning
   counts logical size in both modes, so copy projections are conservative and
   ISO projections are exact.
4. **No compression, and no encryption or deduplication.** Media stores raw file
   content, so consumed space equals logical data size plus ISO overhead; there
   is no compression, and encryption and deduplication are recorded non-goals,
   as are incremental transfer, retention, burning to optical drives (image
   files only), cloud or network backends, and extended attributes or ACLs
   (atime and ctime are not preserved either).
   *Impact:* plan capacity on raw sizes, and treat the media as unencrypted
   unless you encrypt the destination filesystem or the container yourself.
5. **ISO 9660 with Rock Ridge and Joliet, not UDF.** `xorriso` does not write
   UDF at all; that was verified against the upstream and distribution man pages
   and recorded as a requirement change request, ratified 2026-10-18. The
   images are archival data discs intended to be mounted on Linux and Windows.
   *Impact:* playback in consumer standalone Blu-ray players is not supported and
   is a recorded non-goal; if you need playable discs, this tool is the wrong
   choice.
6. **Planning state is held in memory (documented V1 risk).** The planner sorts
   and places the whole item set in memory; a spill-to-disk sort is a recorded
   non-goal for V1. *Impact:* memory grows
   with the number of catalog items. The 1,000,000-item performance smoke test
   (`-tags perf`) is the regression tripwire; plan very large sets on a machine
   with headroom.

## Development and testing

Toolchain: the `go` directive in `go.mod` (1.26.11). The default
`GOTOOLCHAIN=auto` downloads it if the installed Go is older. `go vet ./...`
should stay clean.

Every gate below has a `make` target; `make help` prints the full list
(`build`, `test`, `vet`, `test-e2e`, `test-perf`, `test-race`, `verify`,
`vulncheck`, `lint`, `publish`, `deb`, `appimage`, `help`). In the table, the
first column pairs each target with the recipe it runs, copied verbatim from
the Makefile, so `make <target>`
and the raw command run the identical command. The one exception is the `large`
tier, which has no target: run its raw command directly.

Outside the gate set, `make publish` runs `scripts/publish.sh`, which pushes a
sanitized single-commit snapshot of the tree to the public GitHub repository;
run `scripts/publish.sh --help` for its flags.

Packaging targets build the distributable release artifacts from the same tree: `make deb` runs `scripts/package-deb.sh` (static release binary, version read from the binary's own `--version`) and writes dist/isopress_<version>_amd64.deb; `make appimage` runs `scripts/package-appimage.sh` (AppDir with the desktop entry and icon from packaging/, packed with appimagetool) and writes dist/isopress-<version>-amd64.AppImage. Both write only under dist/, which is git-ignored.

Test tiers, by build tag:

| Target (recipe) | What it runs | Typical duration |
| --- | --- | --- |
| `make test` (`go test ./... -timeout 30m`) | Fast inner loop: the default suite, with the 37 slowest tests excluded by build tag. | Tens of seconds (about 46 s recorded baseline; 30 s warm on the reference machine). |
| `make test-e2e` (`go test -tags e2e ./... -timeout 30m`) | Full suite including the end-to-end tiers (real catalogs, real xorriso round trips, crash and resume, interrupts). | About 94 s. |
| `make test-race` (`go test -tags e2e -race -timeout 30m -count=1 ./...`) | Full race gate: the whole suite under the race detector. `-count=1` defeats the test cache so the detector really runs every time, and the target is kept out of `verify` because it takes minutes. | About 11 to 12 minutes. |
| `make test-perf` (`go test -tags perf ./... -timeout 10m -run '^TestPerf'`) | Performance smokes: walk and hash of a generated 100k-file tree, and a 1,000,000-item plan, each under a fixed budget with an asserted memory ceiling. | Bounded, minutes. |
| raw command only, no target: `go test -tags large ./internal/imagebuild/ -run Large -v` | The real multi-extent test: a 5+ GiB sparse source round-trips byte-exact through one ISO. Writes about 15 GiB transiently, needs roughly 20 GiB free in the user cache directory (override with `ISOPRESS_LARGE_DIR`), and takes minutes. | Minutes. |
| `make vet` (`go vet ./...`) | Static checks. | Seconds. |

The perf tier's `-timeout 10m` is a hard backstop, deliberately tighter than
the other tiers' bounds: the perf watchdog trips at budget plus grace well
inside it, so a wedged run fails the target instead of hanging. The timing
tripwires (`perf`) are excluded from the default and `e2e` gates by build tag,
not by a runtime skip, so a slow or loaded machine cannot destabilise the inner
loop.

`make verify` is the fast static gate: it runs the `build` target, the three
`go vet` passes (default, `e2e` and `perf` build tags), a `gofmt -l .`
formatting check, and `go mod verify`. It deliberately excludes `test-race`,
which takes minutes.

Development tools are installed into the Go bin directory and are deliberately
not on `PATH` by default. Resolve that directory once, prepend it to `PATH`, then
run the tools:

```sh
GOPATH_BIN="$(go env GOPATH)/bin"
export PATH="$GOPATH_BIN:$PATH"

govulncheck ./...
staticcheck ./...
golangci-lint run
```

Both setup lines are plain POSIX shell (a `$( ... )` command substitution and a
normal variable assignment); the snippet runs unchanged under `bash` and `dash`.
To run a single tool without touching your shell environment, prefix that one
command: `"$GOPATH_BIN/govulncheck" ./...`.

The Makefile wraps the same three commands: `make vulncheck` runs
`govulncheck ./...`, and `make lint` runs `staticcheck ./...` followed by
`golangci-lint run`, resolving each binary through `$(shell go env GOPATH)/bin`
(the Makefile spelling of the same path as above) and failing with a
`go install ...` hint when one is missing.
`golangci-lint` picks up the repository's `.golangci.yml`, whose only exclusion
is `errcheck` in `_test.go` files; every other default linter stays enabled.

Useful entry points when reading the tests:

- `internal/cli/taxonomy_table_test.go`: one table over every reachable
  (command, error class) cell, asserting the exit code and the stderr cause.
- `internal/cli/json_contract_test.go`: the `--json` contract for all nine
  commands: exactly one document on stdout, frozen key sets, and exit-code parity
  with the human mode.
- `internal/cli/roundtrip_matrix_e2e_test.go`: plan, write, verify and restore in
  both modes.

## Documentation

The complete project documentation ships with the full repository; this
README is the entry point for the published tree.

## Licence

isopress is licensed under the Apache License, Version 2.0. The full text of
the licence is in [LICENSE](LICENSE).

Copyright 2026 Clyde Solve
