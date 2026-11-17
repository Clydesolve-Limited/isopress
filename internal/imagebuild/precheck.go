package imagebuild

// T4.2: the two capacity pre-checks that run BEFORE any payload is
// staged and before xorriso is ever invoked (DESIGN_SPEC 6.2 free-space
// pre-check, 6.3 step 5 payload-vs-usable_size; IMPLEMENTATION_PLAN
// T4.2 acceptance).
//
// (a) FREE SPACE (6.2): the staging tree must fit the filesystem
// holding the staging root. Required ~= payload size - the staged tree
// IS the payload (plus the tiny pathlist) - so the requirement reuses
// the ONE arithmetic below (payload + the same conservative metadata
// overhead as check (b); one estimator, both gates, never a second
// magic formula). The probe is stdlib syscall.Statfs on the staging
// root, reported "up front" per 6.2. The DESIGN_SPEC 11 dependency
// gate forbids direct golang.org/x/sys imports; syscall.Statfs is the
// sanctioned stdlib mechanism exactly like syscall.Flock in the catalog
// lock (the same precedent, AST-gated by the deps test). Bavail (not
// Bfree) is the honest figure: it is what an unprivileged process can
// actually write - the tool must never plan around root's reserve.
//
// (b) PAYLOAD vs USABLE_SIZE (6.3 step 5): "sum of payload bytes plus
// estimated metadata overhead (2 MiB + 512 bytes per entry,
// conservative) must not exceed the image's usable_size; otherwise fail
// with a clear plan error rather than letting xorriso fail at the end
// of a long build". The conservative overhead covers ISO 9660 + Rock
// Ridge + Joliet structure (three directory trees, path tables, system
// use entries) and the staging scratch's own filesystem metadata. The
// arithmetic is the CANONICAL estimator of units.RequiredImageBytes
// (DESIGN_SPEC 4.3 capacity accounting, as amended 2026-10-20): the
// fixed 2 MiB plus the two embedded image-root entries live in
// units.FixedImageOverhead, the payload entries charge
// units.ImageEntryOverhead each. The planner reserves exactly this
// overhead at fill time, so a planner-produced image always passes
// this check; it remains as a cheap safety net against estimator drift
// (defense in depth) - see staging.go's pre-check (b).
//
// CLASSIFICATION (documented judgment call, per the T4.2 brief): both
// pre-check failures are *report.UsageError - the UsageError class of
// DESIGN_SPEC 10.1 ("unsatisfiable combinations"), mapped to exit 2 by
// report.Classify. Rationale: (b) is deterministic given the plan and
// the media profile - plan never exceeds usable_size by construction
// (4.3 reserves the overhead by construction), so an over-capacity BUILD means the
// assignment lock (FR-011) holds a plan that no longer fits the chosen
// profile, and the remedy is user action (clear-assignments + replan
// with a larger profile, or fewer files) - the USER's plan does not fit
// the USER's media, exactly the unsatisfiable-combination shape. An
// ItemFailures/exit-10 reading would be wrong: nothing ran to
// completion. A fatal/exit-1 reading would be wrong: it is not a bug
// class, and the design demands a CLEAR message, which is the
// UsageError contract (stderr + usage hint + exit 2). (a) is the same
// shape for the chosen --work-dir: the remedy is a work-dir on a bigger
// filesystem. The messages name required vs available and the overflow
// in bytes and human units, so the user can act without re-running.
//
// Neither check has any side effect; both run before the first payload
// byte is staged (check (b) even before the scratch is created).

import (
	"fmt"
	"syscall"

	"isopress/internal/report"
	"isopress/internal/units"
)

// statfsFreeSpace is the production FreeSpaceFunc: unprivileged
// available bytes (Bavail * Bsize) on the filesystem holding path,
// via stdlib syscall.Statfs (Linux-only, NFR-001; DESIGN_SPEC 11 gate:
// no direct x/sys import).
func statfsFreeSpace(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %q: %w", path, err)
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// workDirGate is the upfront --work-dir format gate, run as the FIRST
// check of validateInput - i.e. BEFORE os.MkdirTemp, so a refusal has
// zero filesystem side effects (no scratch is ever created). The
// WorkDir is the DISK side of EVERY graft line
// ("<workdir>/isopress-imagebuild-*/files/x"), so a
// pathlist-breaking byte in it would corrupt every pathlist line
// wholesale: a newline or CR would split the line into two bogus
// graft entries, a NUL would truncate it at xorriso's reader. Such
// bytes are legal-in-principle path components on Linux but never a
// sane --work-dir, so the flag is refused with the Usage-class error
// (exit 2, remedy: choose a different --work-dir).
//
// '=' is DELIBERATELY NOT refused here (unlike a member media path,
// where graftGate refuses it): xorriso splits each line at the FIRST
// '=', and the disk side is a tool-absolute path that parses whole
// under that split, so a '=' WorkDir is legal-but-weird (N2). The
// member-side '=' refusal stays the conservative gate it is; this
// upfront gate does not extend it.
func workDirGate(workDir string) error {
	for i := 0; i < len(workDir); i++ {
		switch workDir[i] {
		case '\n', '\r', 0:
			return workDirError(workDir, workDir[i])
		}
	}
	return nil
}

// workDirError builds the Usage-class refusal of a --work-dir carrying
// a pathlist-breaking byte, naming the flag and the offending byte.
func workDirError(workDir string, b byte) error {
	return &report.UsageError{Msg: fmt.Sprintf(
		"--work-dir %q contains the byte %q (0x%02x), which would corrupt the line-oriented graft-points pathlist for every member; "+
			"choose a --work-dir whose path carries no newline, carriage return, or NUL (nothing was staged)",
		workDir, string(b), b)}
}

// capacityError builds the 6.3 step 5 plan-class failure (pre-check b):
// required vs usable_size, the overflow, and the remedy.
func capacityError(in StageInput, payloadBytes int64, entries int, required int64) error {
	return &report.UsageError{Msg: fmt.Sprintf(
		"image %d of set %q does not fit its media profile: payload %s + conservative metadata overhead (%s for %d entries) = %s required, usable_size is %s (overflow %s); "+
			"refusing to build rather than letting xorriso fail at the end of a long build (DESIGN_SPEC 6.3 step 5) - clear assignments and replan with a larger --media-profile or fewer files",
		in.ImageID, in.SetID,
		units.FormatBytes(payloadBytes),
		units.FormatBytes(required-payloadBytes), entries,
		units.FormatBytes(required),
		units.FormatBytes(in.UsableSize),
		units.FormatBytes(required-in.UsableSize))}
}

// freeSpaceError builds the 6.2 free-space failure (pre-check a):
// required vs available on the staging filesystem, naming the remedy.
func freeSpaceError(root string, in StageInput, payloadBytes int64, entries int, required int64, avail uint64) error {
	return &report.UsageError{Msg: fmt.Sprintf(
		"staging workspace %q has insufficient free space: %s required (payload %s + %s overhead for %d entries), %s available (DESIGN_SPEC 6.2); "+
			"nothing was staged - choose a --work-dir on a filesystem with more space",
		root,
		units.FormatBytes(required),
		units.FormatBytes(payloadBytes),
		units.FormatBytes(required-payloadBytes), entries,
		units.FormatBytes(int64(avail)))}
}
