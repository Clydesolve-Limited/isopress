package fsmeta

// T1.2: lstat-based entry classification (DESIGN_SPEC 3.1 directory walk;
// PLAN_SPEC FR-038 symlink policy D-03, FR-039 hard-link policy D-04).
//
// The walk layer (T1.3, internal/ingest) drives filepath.WalkDir, which
// yields fs.DirEntry values. Classification therefore has to work from a
// DirEntry's Type() (cheap, no extra syscall) with an Info() fallback only
// when the type is unknown, AND from a full fs.FileInfo wherever an lstat
// result is already on hand (capture window, change detection). Both paths
// are provided here and documented per function.
//
// The four kinds mirror DESIGN_SPEC 3.1: regular files become hash jobs;
// directories are recorded (empty-dir preservation, FR-037); symlinks are
// recorded with their raw target read via os.Readlink and are NEVER
// followed (D-03, confirmed 2026-10-18); special files (device nodes,
// FIFOs, sockets) are skipped with a warning event because PLAN_SPEC
// section 2.1 scopes the tool to files and directories only.

import (
	"fmt"
	"io/fs"
)

// EntryKind classifies a filesystem entry into one of the four classes the
// backup tool recognises (DESIGN_SPEC 3.1). The String values "regular"
// and "symlink" match the catalog `files.entry_kind` vocabulary of 2.3.
type EntryKind int

const (
	// KindRegular is an ordinary file: a hash job at ingest.
	KindRegular EntryKind = iota
	// KindSymlink is a symbolic link: recorded with its raw target,
	// never followed (D-03).
	KindSymlink
	// KindDirectory is a directory: recorded for empty-dir preservation
	// (FR-037) and assigned per DESIGN_SPEC 5.4.
	KindDirectory
	// KindSpecial is a device node, FIFO, or socket: skipped with a
	// warning event (DESIGN_SPEC 3.1).
	KindSpecial
)

// String returns the lowercase class name. The values "regular" and
// "symlink" are exactly the catalog `files.entry_kind` CHECK values of
// DESIGN_SPEC 2.3, so the catalog layer can store the string directly.
func (k EntryKind) String() string {
	switch k {
	case KindRegular:
		return "regular"
	case KindSymlink:
		return "symlink"
	case KindDirectory:
		return "directory"
	case KindSpecial:
		return "special"
	default:
		return fmt.Sprintf("EntryKind(%d)", int(k))
	}
}

// ClassifyType maps raw file-TYPE bits (the FileMode subset returned by
// fs.DirEntry.Type(), i.e. fs.ModeType) to an EntryKind. It performs no
// syscalls and never touches the filesystem.
//
// Mapping (DESIGN_SPEC 3.1):
//
//	0 (no type bits)          -> KindRegular
//	ModeDir                   -> KindDirectory
//	ModeSymlink               -> KindSymlink
//	ModeDevice|ModeCharDevice|
//	ModeNamedPipe|ModeSocket  -> KindSpecial
//	ModeIrregular             -> KindSpecial
//
// ModeIrregular means "does not fit the known types"; for a DirEntry that
// is how Go surfaces an unknown d_type on the wire, so callers holding a
// DirEntry must use ClassifyDirEntry (which falls back to Info()) rather
// than ClassifyType when ModeIrregular is set. For a full fs.FileInfo
// (an lstat result, already known to be non-regular) KindSpecial is the
// correct bucket: the file is skipped with a warning either way.
func ClassifyType(t fs.FileMode) EntryKind {
	switch t & fs.ModeType {
	case 0:
		return KindRegular
	case fs.ModeDir:
		return KindDirectory
	case fs.ModeSymlink:
		return KindSymlink
	default:
		// ModeDevice, ModeCharDevice, ModeNamedPipe, ModeSocket, and
		// ModeIrregular all land here.
		return KindSpecial
	}
}

// Classify classifies an entry from a full fs.FileInfo, i.e. the result of
// an lstat (DESIGN_SPEC 3.1 "lstat classification"). Use this path
// wherever an lstat result is already on hand: the capture window around
// hashing (T1.4), the pre-copy and pre-master comparisons (5.1 step 1,
// 3.5), and restore metadata application (5.4). It never follows
// symlinks because FileInfo values passed here come from os.Lstat.
func Classify(fi fs.FileInfo) EntryKind {
	return ClassifyType(fi.Mode())
}

// ClassifyDirEntry classifies an entry yielded by filepath.WalkDir. This
// is the walker-facing path (DESIGN_SPEC 3.1): it uses the cheap
// DirEntry.Type() bits and falls back to the lstat-equivalent
// DirEntry.Info() ONLY when the type is unknown (Type() reports
// fs.ModeIrregular, which is how Go surfaces an unknown d_type from
// getdents). The fallback can fail (the entry may vanish or become
// unreadable between the directory read and the stat); that failure is
// returned as an error, never swallowed and never panicked on.
func ClassifyDirEntry(de fs.DirEntry) (EntryKind, error) {
	if t := de.Type(); t&fs.ModeIrregular == 0 {
		return ClassifyType(t), nil
	}
	// Type unknown: fall back to the full stat.
	fi, err := de.Info()
	if err != nil {
		return KindSpecial, fmt.Errorf("fsmeta: classify %q: type unknown and Info fallback failed: %w", de.Name(), err)
	}
	return Classify(fi), nil
}
