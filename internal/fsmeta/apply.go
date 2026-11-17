package fsmeta

// T1.2: metadata application for restore and copy-tree writes
// (DESIGN_SPEC 5.4 metadata handling; PLAN_SPEC FR-036 to FR-039).
//
// Policy, fixed here and per 5.4:
//   - chown is attempted UNCONDITIONALLY (uid and gid both from the
//     recorded Meta). As non-root an EPERM from chown is a WARNING, not a
//     failure: Apply reports it distinguishably (ChownSkipped + Warning)
//     and the file still completes successfully with its other metadata
//     applied. The run-level consequence is only that the report notes
//     which files could not be fully owned (5.4). Any OTHER chown error
//     (ENOENT, EIO, EROFS, ...) IS a hard error: EPERM is the only
//     downgraded errno.
//   - chmod and mtime failures ARE hard errors (5.4: "a metadata failure
//     never leaves verified content marked unverified; it is reported as
//     a per-file warning" at the CALLER layer - Apply itself surfaces the
//     failure and the caller decides whether the item is marked).
//   - Ordering: lchown FIRST, then chmod, then mtime. Rationale: on
//     Linux, a chown that changes ownership clears the setuid/setgid
//     bits for non-capable callers, so applying the mode AFTER the chown
//     guarantees the exact recorded mode lands regardless of who runs the
//     restore.
//   - Never follow (D-03): lchown and the mtime syscall below do not
//     resolve a final symlink, so applying metadata to a restored
//     symlink touches the LINK, never its target. chmod on Linux always
//     follows (there is no lchmod in the kernel), so Apply lstats first
//     and SKIPS chmod for symlinks: symlink modes are not preservable on
//     Linux and following the link would violate the policy outright.
//   - mtime is applied with nanosecond precision via utimensat(2)
//     AT_SYMLINK_NOFOLLOW. The stdlib syscall package exposes no
//     utimensat wrapper and syscall.UtimesNano FOLLOWS symlinks, so the
//     raw syscall (stdlib syscall.Syscall6 only - no x/sys/unix, per the
//     DESIGN_SPEC 11 dependency gate) is the sanctioned mechanism, in the
//     same spirit as syscall.Flock and syscall.Statfs elsewhere.
//   - atime is left untouched (UTIME_OMIT): FR-036 records mode, owner,
//     group, and mtime only; Apply never invents an atime it was not
//     given.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"syscall"
	"unsafe"
)

// ApplyResult reports the non-fatal outcome of Apply. It is returned
// alongside a nil error precisely when the EPERM downgrade of DESIGN_SPEC
// 5.4 fired; a caller collecting per-file warnings should surface
// Warning (for example as a progress Warn event and a run_events row)
// whenever ChownSkipped is true.
type ApplyResult struct {
	// ChownSkipped is true when the unconditional chown was denied with
	// EPERM (the non-root case of 5.4) and ownership was therefore NOT
	// applied. All other metadata steps still ran.
	ChownSkipped bool
	// Warning is the human-readable description of the downgrade; it is
	// populated exactly when ChownSkipped is true and is empty otherwise.
	Warning string
}

// Apply writes the recorded metadata m onto the filesystem object at
// path: ownership via lchown, permission mode via chmod (regular files,
// directories - skipped for symlinks, see above), and modification time
// via utimensat with AT_SYMLINK_NOFOLLOW. Callers apply metadata AFTER
// content verification (DESIGN_SPEC 5.4, 5.1 step ordering).
//
// Warning-vs-error contract (DESIGN_SPEC 5.4):
//   - lchown denied with EPERM: NOT an error. Returns (ApplyResult with
//     ChownSkipped=true and Warning populated, nil). The file completes
//     successfully with its other metadata applied. Root can always
//     chown, so this path only fires for non-root restores (the test
//     suite skips that subtest when running as euid 0, since EPERM
//     cannot be provoked as root).
//   - Any other lchown error (for example ENOENT on a missing path):
//     hard error.
//   - chmod failure (including on a non-existent path) and mtime
//     (utimensat) failure: hard error.
//   - When a hard error occurs after an EPERM downgrade, the returned
//     ApplyResult still carries the warning; callers check err first.
func Apply(path string, m Meta) (ApplyResult, error) {
	var res ApplyResult

	// Determine the entry type once: chmod must be skipped for symlinks
	// because Linux chmod(2) follows links and D-03 forbids following.
	fi, err := os.Lstat(path)
	if err != nil {
		return res, fmt.Errorf("fsmeta: apply %q: lstat: %w", path, err)
	}
	isSymlink := fi.Mode()&fs.ModeSymlink != 0

	// Step 1: chown, attempted UNCONDITIONALLY (5.4). lchown does not
	// follow symlinks, which is exactly the D-03 preserve policy.
	if err := os.Lchown(path, int(m.UID), int(m.GID)); err != nil {
		if errors.Is(err, syscall.EPERM) {
			// The 5.4 downgrade: warning, not failure.
			res.ChownSkipped = true
			res.Warning = fmt.Sprintf(
				"metadata: chown %q to uid %d gid %d skipped: operation not permitted (non-root EPERM downgrade, DESIGN_SPEC 5.4)",
				path, m.UID, m.GID)
			// Not an error: continue with mode and mtime.
		} else {
			return res, fmt.Errorf("fsmeta: apply %q: lchown %d:%d: %w", path, m.UID, m.GID, err)
		}
	}

	// Step 2: chmod after chown so a chown that cleared setuid/setgid
	// bits cannot clobber the recorded mode (see file header). For
	// symlinks the chmod is skipped deliberately: no lchmod exists in
	// the Linux kernel, chmod would follow the link and mutate the
	// TARGET (D-03 violation), and symlink modes are not preservable on
	// Linux anyway.
	if !isSymlink {
		if err := os.Chmod(path, m.Mode); err != nil {
			return res, fmt.Errorf("fsmeta: apply %q: chmod %04o: %w", path, UnixMode(m.Mode), err)
		}
	}

	// Step 3: mtime with nanosecond precision, no symlink follow.
	if err := utimesNoFollow(path, m.MtimeNS); err != nil {
		return res, fmt.Errorf("fsmeta: apply %q: utimensat mtime: %w", path, err)
	}

	return res, nil
}

// Linux UAPI constants used by utimesNoFollow. The stdlib syscall package
// does not export these (they live in golang.org/x/sys/unix, whose DIRECT
// import the DESIGN_SPEC 11 dependency gate forbids), so they are declared
// here with their fixed kernel-ABI values from include/uapi-linux/fcntl.h
// and include/uapi-linux/stat.h. They are part of the Linux ABI and do not
// vary across architectures or kernel versions.
const (
	atFDCWD           = int(-100) // "current working directory" dirfd marker
	atSymlinkNoFollow = 0x100     // do not resolve the final path symlink
	// utimeOmit is UTIME_OMIT = (1<<30)-2 = 0x3FFFFFFE, the fixed Linux
	// UAPI value from include/uapi-linux/stat.h; it matches
	// golang.org/x/sys v0.49.0 zerrors_linux.go
	// (UTIME_OMIT = 0x3ffffffe) and glibc bits/stat.h
	// (# define UTIME_OMIT ((1l << 30) - 2l)). The previous value
	// 0x380 was WRONG: 896 is a legal tv_nsec (0..999999999), so the
	// kernel did NOT see UTIME_OMIT — it interpreted the atime slot as
	// a REAL timestamp {0 sec, +896ns} and every Apply silently
	// reset atime to Unix-epoch+896ns instead of preserving it.
	utimeOmit = 0x3FFFFFFE // utimensat timespec: ignore this slot
)

// utimesNoFollow sets only the mtime of path (nanosecond precision) via
// utimensat(2) with AT_SYMLINK_NOFOLLOW; the atime slot carries
// UTIME_OMIT so the current atime is preserved (Apply is never given an
// atime: FR-036 does not record one).
//
// The stdlib syscall package has no utimensat wrapper and
// syscall.UtimesNano follows symlinks, so the raw syscall is used. This
// is stdlib-only (syscall + unsafe), consistent with the DESIGN_SPEC 11
// gate that forbids direct golang.org/x/sys/unix imports.
func utimesNoFollow(path string, mtimeNS int64) error {
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return err
	}
	times := [2]syscall.Timespec{
		{Nsec: utimeOmit}, // atime: preserve
		syscall.NsecToTimespec(mtimeNS),
	}
	// The -100 dirfd marker crosses the syscall boundary as a register
	// word; uintptr(int) produces the two's-complement pattern the
	// kernel decodes back as int dirfd.
	fdwd := atFDCWD
	_, _, errno := syscall.Syscall6(
		syscall.SYS_UTIMENSAT,
		uintptr(fdwd),
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&times[0])),
		uintptr(atSymlinkNoFollow),
		0,
		0,
	)
	if errno != 0 {
		return errno
	}
	return nil
}
