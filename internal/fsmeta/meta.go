package fsmeta

// T1.2: metadata capture and change detection (DESIGN_SPEC 5.4 metadata
// handling with D-03 symlink policy; 3.5 and 5.1 step 1 change detection;
// 2.3 files-table columns; PLAN_SPEC FR-036 to FR-039, NFR-001).
//
// Linux-only assumption (NFR-001): Capture type-asserts FileInfo.Sys() to
// *syscall.Stat_t, the Linux stat structure. The tool supports no other
// platform in V1, so no portability shim exists by design. The assertion
// is checked, not assumed: on failure Capture returns an error rather
// than panicking, so an exotic FileInfo (fake, or from a future Go stdlib
// change) degrades to a reported failure.

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// Meta is the captured metadata of one filesystem entry. Its fields mirror
// the `files` table metadata columns of DESIGN_SPEC 2.3 (mode, uid, gid,
// mtime_ns, dev, inode, nlink, size) and are the unit that flows from
// ingest capture (FR-036) through change detection (3.5, D-10) to restore
// metadata application (5.4, FR-036 to FR-39).
//
// Mode holds ONLY permission-class bits in Go's fs.FileMode
// representation: fs.ModePerm (rwxrwxrwx) plus fs.ModeSetuid,
// fs.ModeSetgid, and fs.ModeSticky relocated to their Go flag positions.
// Use UnixMode to convert back to raw POSIX bits for chmod(2) or for the
// catalog's INTEGER `mode` column.
//
// Dev, Inode, and Nlink use unsigned kernel-faithful types; the catalog
// layer converts to int64 when writing the INTEGER columns (values are
// always well inside int64 range on Linux).
type Meta struct {
	// Mode is the permission mode (DESIGN_SPEC 2.3 "mode: permission
	// bits"), in fs.FileMode representation; see UnixMode for the
	// POSIX-bits conversion.
	Mode fs.FileMode
	// UID is the numeric owner user id (FR-036 "numeric owner").
	UID uint32
	// GID is the numeric group id (FR-036 "group IDs").
	GID uint32
	// MtimeNS is the modification time in Unix nanoseconds, UTC
	// (DESIGN_SPEC 2.3 "unix nanoseconds, UTC").
	MtimeNS int64
	// Dev is the device id containing the file: hard-link-family and
	// change detection key (DESIGN_SPEC 2.4, 3.5, D-10).
	Dev uint64
	// Inode is the filesystem inode number: change detection key
	// (file replaced in place yields a new inode; 3.5, D-10).
	Inode uint64
	// Nlink is the hard-link count: nlink > 1 plus (dev, inode) marks a
	// hard-link family at ingest (DESIGN_SPEC 2.4, FR-039).
	Nlink uint64
	// Size is the entry size in bytes; for symlinks it is len(target)
	// per DESIGN_SPEC 4.3 symlink items.
	Size int64
}

// UnixMode converts a Meta/fs FileMode permission value back to raw POSIX
// mode bits (rwxrwxrwx plus setuid/setgid/sticky), suitable for the
// catalog `files.mode` INTEGER column of DESIGN_SPEC 2.3 and for direct
// comparison with syscall mode bits. Type bits are never included.
func UnixMode(m fs.FileMode) uint32 {
	u := uint32(m.Perm())
	if m&fs.ModeSetuid != 0 {
		u |= syscall.S_ISUID
	}
	if m&fs.ModeSetgid != 0 {
		u |= syscall.S_ISGID
	}
	if m&fs.ModeSticky != 0 {
		u |= syscall.S_ISVTX
	}
	return u
}

// GoMode is the INVERSE of UnixMode: it converts raw POSIX permission
// bits (rwxrwxrwx plus setuid/setgid/sticky - the shape the catalog's
// INTEGER `mode` columns carry per DESIGN_SPEC 2.3) into Go's
// fs.FileMode representation that fsmeta.Apply and os.Chmod consume.
// Go relocates the special bits to fs.ModeSetuid/ModeSetgid/ModeSticky,
// so handing os.Chmod a raw POSIX 0o4755 silently DROPS the setuid bit
// (Perm() yields only 0o755) - the T1.2 QA finding this function
// exists to prevent. Type bits in the input are ignored.
//
// THE MODE CONVERSION INVARIANT (one direction, one site, per side):
//   - Ingest direction: Capture reads kernel stat bits and the
//     reconciler stores them via UnixMode - the CATALOG STORES RAW
//     POSIX (DESIGN_SPEC 2.3).
//   - Write direction: the copy writer feeds catalog-shaped rows back
//     through GoMode (copyengine.metaFromRaw, the single applyMetadata
//     seam) - FSMETA.APPLY CONSUMES GO SEMANTICS.
//
// GoMode and UnixMode are exact inverses over the permission-class
// subspace: UnixMode(GoMode(x)) == x for every x in [0, 0o7777]
// (pinned exhaustively by TestGoModeInverseOfUnixMode).
func GoMode(posix uint32) fs.FileMode {
	mode := fs.FileMode(posix & 0o777)
	if posix&syscall.S_ISUID != 0 {
		mode |= fs.ModeSetuid
	}
	if posix&syscall.S_ISGID != 0 {
		mode |= fs.ModeSetgid
	}
	if posix&syscall.S_ISVTX != 0 {
		mode |= fs.ModeSticky
	}
	return mode
}

// Capture extracts the Meta fields from an fs.FileInfo. The FileInfo is
// expected to come from os.Lstat (or an equivalent that does not follow
// symlinks) so that symlink entries describe the link itself, per the
// D-03 preserve-never-follow policy.
//
// The uid/gid/dev/inode/nlink/mtime_ns fields are read from the
// *syscall.Stat_t behind FileInfo.Sys(); the Linux-only assumption of
// NFR-001 is documented and ENFORCED here: if the assertion fails (wrong
// platform, nil Sys, or a fake FileInfo), Capture returns an error
// naming the entry and the offending type instead of panicking.
func Capture(fi fs.FileInfo) (Meta, error) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return Meta{}, fmt.Errorf(
			"fsmeta: FileInfo.Sys() for %q is not *syscall.Stat_t (got %T); the tool is Linux-only (NFR-001)",
			fi.Name(), fi.Sys())
	}
	// st.Mode carries raw POSIX bits; relocate the permission-class bits
	// into Go's fs.FileMode representation via the canonical inverse of
	// UnixMode (Perm is 0o777; the special bits get their own Go flag
	// positions; any file-type bits in st.Mode are ignored).
	mode := GoMode(st.Mode)
	return Meta{
		Mode:    mode,
		UID:     st.Uid,
		GID:     st.Gid,
		MtimeNS: int64(st.Mtim.Sec)*1_000_000_000 + int64(st.Mtim.Nsec),
		Dev:     uint64(st.Dev),
		Inode:   uint64(st.Ino),
		Nlink:   uint64(st.Nlink),
		Size:    fi.Size(),
	}, nil
}

// PathMeta lstats path (never following a final symlink, D-03) and
// captures its Meta. This is the convenience entry point for the ingest
// capture window (T1.4), the pre-copy and pre-master comparisons (5.1
// step 1, 3.5 point 3), and restore capture.
func PathMeta(path string) (Meta, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return Meta{}, fmt.Errorf("fsmeta: lstat %q: %w", path, err)
	}
	m, err := Capture(fi)
	if err != nil {
		return Meta{}, fmt.Errorf("fsmeta: capture %q: %w", path, err)
	}
	return m, nil
}

// Diff is a bitmask naming which of the FIXED change-detection fields
// differ between two Meta values.
type Diff uint8

const (
	// DiffSize names the file size field.
	DiffSize Diff = 1 << iota
	// DiffMtimeNS names the modification time (nanosecond) field.
	DiffMtimeNS
	// DiffDev names the containing-device field.
	DiffDev
	// DiffInode names the inode field.
	DiffInode
)

// Any reports whether the Diff names at least one differing field.
func (d Diff) Any() bool { return d != 0 }

// String renders the differing field names space-separated (for example
// "size mtime_ns"), or "none" for the empty set. Used in D-10 change
// warnings and in test diagnostics.
func (d Diff) String() string {
	if d == 0 {
		return "none"
	}
	names := [...]string{"size", "mtime_ns", "dev", "inode"}
	out := ""
	for i, name := range names {
		if d&(1<<uint(i)) != 0 {
			if out != "" {
				out += " "
			}
			out += name
		}
	}
	return out
}

// DiffFields compares the RESOLVED change-detection field set of
// DESIGN_SPEC 3.5 (RESOLVED 2026-10-20) and 5.1 step 1. The set is
// mode-aware through the resolved Mode parameter:
//
//   - Mode.Set == ComparisonFull (the zero-value default): the full
//     D-10 set - size, mtime_ns, dev, and inode;
//   - Mode.Set == ComparisonMetadata: size and mtime_ns ONLY - dev and
//     inode differences are IGNORED (both are synthesized on the CIFS
//     family and neither is contractually stable across SMB sessions;
//     see change.go for the resolver and the safety analysis).
//
// Mode, uid, gid, and nlink are NEVER part of the comparison in either
// mode (a chmod, chown, or link-count change does not invalidate a
// captured hash).
//
// THIS IS THE SINGLE COMPARISON used at ALL THREE detection points:
// ingest post-hash re-lstat (3.5 point 1), copy-time pre-copy check
// (3.5 point 2, 5.1 step 1), and image-build-time staging check (3.5
// point 3). The comparison IMPLEMENTATION exists exactly once - here -
// and the resolved mode only masks its output; callers must not
// hand-roll variants. A design change to the field sets happens here,
// once.
func DiffFields(a, b Meta, mode Mode) Diff {
	var d Diff
	if a.Size != b.Size {
		d |= DiffSize
	}
	if a.MtimeNS != b.MtimeNS {
		d |= DiffMtimeNS
	}
	if mode.Set != ComparisonMetadata {
		if a.Dev != b.Dev {
			d |= DiffDev
		}
		if a.Inode != b.Inode {
			d |= DiffInode
		}
	}
	return d
}

// Changed reports whether any field of the RESOLVED detection set
// (full: size, mtime_ns, dev, inode; metadata: size, mtime_ns) differs
// between a and b. It is shorthand for DiffFields(a, b, mode).Any() and
// shares its contract: this is the single comparison used at all three
// detection points of DESIGN_SPEC 3.5.
func Changed(a, b Meta, mode Mode) bool { return DiffFields(a, b, mode).Any() }

// ReadTarget returns the raw symlink target at path via os.Readlink
// (DESIGN_SPEC 3.1, D-03 confirmed 2026-10-18: symlinks are preserved,
// never followed). Readlink never resolves the target, so a DANGLING
// symlink (target does not exist) yields its recorded target string and
// a nil error; the target string is exactly what gets stored in
// `files.link_target` and recreated verbatim on restore.
func ReadTarget(path string) (string, error) {
	target, err := os.Readlink(path)
	if err != nil {
		return "", fmt.Errorf("fsmeta: readlink %q: %w", path, err)
	}
	return target, nil
}
