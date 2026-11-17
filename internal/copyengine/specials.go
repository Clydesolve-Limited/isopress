package copyengine

// T3.3: the special-member writes and the manifest/export embed
// primitive (DESIGN_SPEC 5.3 layout members, 5.4 metadata, D-03
// symlink policy, D-04 hard links, FR-037 empty dirs; IMPLEMENTATION_PLAN
// T3.3, with the T3.4 seam documented in copyengine.go).
//
// These three specials shipped MINIMAL AND CORRECT in T3.3 so the
// copy command produced a faithful tree end to end. T3.4 (this
// round) owned their creation semantics and test matrix and refined
// them in place: the semantics below are now PINNED by
// specials_test.go (dangling-symlink faithful carry, symlink-to-dir
// and chain/cycle lstat semantics, 3-name family nlink identity,
// empty-dir mode/uid/gid/mtime round-trips, the POSIX->Go conversion
// proofs, the EPERM warning class, and the ordering pin) plus the
// cli-level copy_specials e2e. Like the content path of
// copyengine.go they take NO retry budget: a symlink or link(2)
// fault is recorded once and the run continues; the next run
// re-verifies and re-writes the unmarked member (5.1 crash-recovery
// text applies unchanged).
//
// Ordering discipline honored here, mirroring 5.1 where it applies:
// create, fsync the containing directory, READ-BACK verify (target
// equality for symlinks, dev+inode identity for hard links), THEN
// mark. There is no hash to compare - symlinks are NULL-hash by
// convention (DESIGN_SPEC 2.3, D-03) and a hard link is verified by
// inode identity against the already-verified primary.
//
// Metadata is applied AFTER the mark (5.4, the copyengine.go T3.4
// seam note): a metadata failure is a per-file WARNING carried in
// Result.Warning, never an unmark.
//
// T3.4 MODE-CONVERSION INVARIANT: every Mode field on the item types
// below carries RAW POSIX permission bits - the catalog `mode`
// column's shape (DESIGN_SPEC 2.3, the copystore inventory's
// convention). They are converted to Go fs.FileMode semantics by
// fsmeta.GoMode inside metaFromRaw, at the single applyMetadata seam
// (copyengine.go). Never pass an item's Mode to os.Chmod or
// fsmeta.Apply unconverted: Go's Chmod reads fs.ModeSetuid/Setgid/
// Sticky flag positions and silently drops setuid from a raw 0o4755
// (the T1.2 QA finding).
//
// T3.4 WriteEmptyDir ORDERING (deliberately NOT the file path's
// shape): mkdir -> Apply(metadata) -> fsync(parent) -> SetDirContent.
// The file path defers metadata until after content+fsync+verify+mark
// so a metadata failure can never strand verified content unmarked
// (5.4's never-unmark rule). That rule has no force here: dirs carry
// no copied flag (mkdir is the idempotent publication, 2.3), there is
// no verify->mark window to protect, and applyMetadata downgrades
// every failure (EPERM chown included) to a WARNING that cannot fail
// the item or block the content row. The current order is KEPT
// because it is strictly BETTER for durability: the parent fsync runs
// AFTER Apply, so on journaling filesystems (ext4/xfs) the journal
// commit it triggers carries the just-applied chmod/lchown/utimes -
// the durable state of a completed write IS the recorded state.
// Reordering to mkdir -> fsync -> Apply would instead flush the
// mkdir-default 0755 and leave the recorded mode unforced until the
// next background flush. A crash between Apply and the fsync loses
// only unrecorded metadata on an unrecorded dir - the next run
// re-asserts both (dirs are re-processed every run).
//
// INFO-5 (fix round): WriteSymlink and LinkSecondary marks run through
// the SAME detached-mark treatment CopyItem's content mark gets
// (markContext in copyengine.go): once the read-back verify has
// passed, a SIGINT-cancelled run context no longer strands the
// present-but-unmarked link - the mark completes on a fresh
// Background-derived context with the MarkDetachTimeout bound, and
// the item completes as Copied instead of recording a Failed item
// under an interrupted run. WriteEmptyDir is deliberately OUT of this
// treatment: its SetDirContent row has no verify->mark window to
// protect (mkdir is the content publication, idempotent by 2.3, and
// the dir rows are re-asserted EVERY run, so a lost row is repaired
// trivially), and 10.1's detached clause is scoped to read-back-then-
// mark writers.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// SymlinkItem is one symlink row to recreate with its recorded target
// (D-03: preserve, never follow). Mode/UID/GID/MtimeNS feed the 5.4
// post-mark Apply; on Linux Apply skips chmod for symlinks (no
// lchmod(2); following chmod would mutate the target).
type SymlinkItem struct {
	FileID     int64
	RelPath    string // source relpath
	MediaPath  string // image_contents media path
	DestPath   string // absolute destination path of the LINK
	LinkTarget string // recorded target, recreated verbatim
	Mode       uint32 // raw POSIX bits (informational on Linux symlinks; converted via metaFromRaw, see the file doc)
	UID        uint32
	GID        uint32
	MtimeNS    int64
}

// DirItem is one empty directory to recreate with its recorded mode
// (FR-037). Directories carry no copied flag (mkdir is idempotent,
// 2.3), so the image_contents 'dir' row IS the durable record that the
// member was written.
type DirItem struct {
	DirID     int64
	RelPath   string // source relpath of the empty directory
	MediaPath string // image_contents media path (files/<relpath>)
	DestPath  string // absolute destination directory path
	Mode      uint32 // raw POSIX bits (catalog directories.mode; converted via metaFromRaw, see the file doc)
	UID       uint32
	GID       uint32
	MtimeNS   int64
}

// LinkItem is one hard-link SECONDARY to create with link(2) against
// the already-written primary (D-04 copy mode: "the primary is written
// once and the secondaries are created as links to it, saving space
// per FR-039"). The secondary shares the primary's inode, hence its
// content hash (the catalog stores the shared family hash on every
// member row, 2.4).
type LinkItem struct {
	FileID          int64
	RelPath         string // secondary source relpath
	MediaPath       string // image_contents media path
	DestPath        string // absolute destination path of the SECONDARY
	PrimaryDestPath string // absolute destination path of the PRIMARY (must exist)
	Size            int64  // family size (bytes), for progress
	Mode            uint32 // raw POSIX bits (catalog files.mode; converted via metaFromRaw, see the file doc)
	UID             uint32
	GID             uint32
	MtimeNS         int64
}

// WriteSymlink recreates one symlink with its recorded target and
// marks it copied (see the file doc for the ordering and the T3.4
// seam).
func WriteSymlink(ctx context.Context, deps Deps, item SymlinkItem) Result {
	d := deps.withDefaults()
	if d.Store == nil {
		return d.failSymlink(item, errors.New("copyengine: nil Store"))
	}
	if err := ctx.Err(); err != nil {
		return Result{Outcome: OutcomeAborted, Err: err}
	}
	if item.DestPath == "" || item.LinkTarget == "" {
		// An empty recorded target is not a creatable symlink (the
		// scan never stores one: readlink on a symlink returns a
		// non-empty target); treat it as a catalog defect.
		return d.failSymlink(item, fmt.Errorf("symlink %q has no destination or target", item.RelPath))
	}
	if err := os.MkdirAll(filepath.Dir(item.DestPath), 0o755); err != nil {
		return d.failSymlink(item, fmt.Errorf("creating destination directory: %w", err))
	}

	// Present-from-a-previous-run handling: EEXIST is fine ONLY when
	// the existing entry is a symlink with the same target (the
	// present-but-unmarked crash state of 5.1); anything else at the
	// destination path fails loudly - the writer never clobbers data
	// it did not write.
	if err := os.Symlink(item.LinkTarget, item.DestPath); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return d.failSymlink(item, fmt.Errorf("creating symlink: %w", err))
		}
		existing, lerr := os.Readlink(item.DestPath)
		if lerr != nil || existing != item.LinkTarget {
			return d.failSymlink(item, fmt.Errorf(
				"destination %s exists and is not a symlink with the recorded target %q (refusing to clobber)",
				item.DestPath, item.LinkTarget))
		}
	}
	if err := syncDir(filepath.Dir(item.DestPath)); err != nil {
		return d.failSymlink(item, fmt.Errorf("fsync directory of %s: %w", item.DestPath, err))
	}

	// Read-back verify: the recreated link must read back the recorded
	// target verbatim (D-03 fidelity; covers a torn create on a flaky
	// NAS).
	readback, err := os.Readlink(item.DestPath)
	if err != nil {
		return d.failSymlink(item, fmt.Errorf("read-back verify: %w", err))
	}
	if readback != item.LinkTarget {
		_ = os.Remove(item.DestPath)
		return d.failSymlink(item, fmt.Errorf("read-back target %q != recorded %q", readback, item.LinkTarget))
	}

	// INFO-5: the detached-mark window (10.1, markContext): the
	// read-back passed, so the mark completes even when a SIGINT
	// cancelled the run context between verify and here.
	markCtx, cancelMark := markContext(ctx)
	defer cancelMark()
	if err := d.Store.SetFileCopied(markCtx, item.FileID, item.MediaPath); err != nil {
		return d.failSymlink(item, fmt.Errorf("marking copied: %w", err))
	}

	// 5.4 metadata after content (T3.4 seam, copyengine.go): Apply
	// skips chmod for symlinks and never follows the link (lchown +
	// utimensat AT_SYMLINK_NOFOLLOW).
	warning := applyMetadata(item.DestPath, metaFromRaw(item.Mode, item.UID, item.GID, item.MtimeNS))
	d.emitDone(int64(len(item.LinkTarget)), item.RelPath)
	return Result{Outcome: OutcomeCopied, Bytes: int64(len(item.LinkTarget)), Attempts: 1, Warning: warning}
}

// failSymlink classifies a symlink failure (no retry budget yet, see
// the file doc) and emits the nil-safe Error event.
func (d Deps) failSymlink(item SymlinkItem, err error) Result {
	d.emitError(fmt.Sprintf("%s: %v", item.RelPath, err))
	return Result{Outcome: OutcomeFailed, Attempts: 1, Err: err}
}

// WriteEmptyDir recreates one empty directory with its recorded
// metadata and records its image_contents 'dir' row (FR-037; see the
// file doc for the T3.4 seam and the kept mkdir -> Apply -> fsync ->
// SetDirContent ordering rationale).
func WriteEmptyDir(ctx context.Context, deps Deps, item DirItem) Result {
	d := deps.withDefaults()
	if d.Store == nil {
		return d.failDir(item, errors.New("copyengine: nil Store"))
	}
	if err := ctx.Err(); err != nil {
		return Result{Outcome: OutcomeAborted, Err: err}
	}
	if item.DestPath == "" {
		return d.failDir(item, fmt.Errorf("dir %q has no destination path", item.RelPath))
	}
	// MkdirAll is idempotent for an existing directory (2.3: "mkdir is
	// idempotent (EEXIST means already present), so dir resumability
	// is a non-issue") and fails honestly if a FILE occupies the path.
	if err := os.MkdirAll(item.DestPath, 0o755); err != nil {
		return d.failDir(item, fmt.Errorf("creating directory: %w", err))
	}
	// Exact recorded mode/ownership/mtime (FR-037: dirs are created
	// "with their recorded mode", owners and mtimes the same way as
	// files per 5.4). Metadata AFTER content - here the "content" is
	// the directory entry itself, which mkdir already published - and
	// BEFORE the parent fsync, so the journal commit the fsync
	// triggers carries the metadata too (see the file doc's T3.4
	// ordering note: applyMetadata only ever warns, it cannot fail
	// the item or block the content row below).
	warning := applyMetadata(item.DestPath, metaFromRaw(item.Mode, item.UID, item.GID, item.MtimeNS))
	if err := syncDir(filepath.Dir(item.DestPath)); err != nil {
		return d.failDir(item, fmt.Errorf("fsync parent of %s: %w", item.DestPath, err))
	}
	if err := d.Store.SetDirContent(ctx, item.DirID, item.MediaPath); err != nil {
		return d.failDir(item, fmt.Errorf("recording dir content row: %w", err))
	}
	// Dirs emit no ItemDone: they carry zero payload bytes, and the
	// copy phase's totals count payload items (the cli report counts
	// dirs separately). Documented in cli/copy.go.
	return Result{Outcome: OutcomeCopied, Attempts: 1, Warning: warning}
}

func (d Deps) failDir(item DirItem, err error) Result {
	d.emitError(fmt.Sprintf("%s: %v", item.RelPath, err))
	return Result{Outcome: OutcomeFailed, Attempts: 1, Err: err}
}

// LinkSecondary creates one hard-link secondary with link(2) against
// the primary's destination path and marks it copied (D-04, FR-039;
// see the file doc for the T3.4 seam).
func LinkSecondary(ctx context.Context, deps Deps, item LinkItem) Result {
	d := deps.withDefaults()
	if d.Store == nil {
		return d.failLink(item, errors.New("copyengine: nil Store"))
	}
	if err := ctx.Err(); err != nil {
		return Result{Outcome: OutcomeAborted, Err: err}
	}
	if item.DestPath == "" || item.PrimaryDestPath == "" {
		return d.failLink(item, fmt.Errorf("hard-link %q lacks a destination or primary path", item.RelPath))
	}
	if err := os.MkdirAll(filepath.Dir(item.DestPath), 0o755); err != nil {
		return d.failLink(item, fmt.Errorf("creating destination directory: %w", err))
	}

	primaryInfo, err := os.Lstat(item.PrimaryDestPath)
	if err != nil {
		// The primary must have been written and marked by the
		// caller's ordering (secondaries follow primaries); a missing
		// primary is an ordering or destination defect.
		return d.failLink(item, fmt.Errorf("stat primary %s: %w", item.PrimaryDestPath, err))
	}
	if err := os.Link(item.PrimaryDestPath, item.DestPath); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return d.failLink(item, fmt.Errorf("link(2) %s -> %s: %w", item.PrimaryDestPath, item.DestPath, err))
		}
		// EEXIST: acceptable only when the existing entry already IS
		// this link (present-but-unmarked crash state). Anything else
		// at the path fails loudly - never clobber unowned data.
		existing, lerr := os.Lstat(item.DestPath)
		if lerr != nil || !os.SameFile(primaryInfo, existing) {
			return d.failLink(item, fmt.Errorf(
				"destination %s exists and does not share the primary's inode (refusing to clobber)",
				item.DestPath))
		}
	}
	if err := syncDir(filepath.Dir(item.DestPath)); err != nil {
		return d.failLink(item, fmt.Errorf("fsync directory of %s: %w", item.DestPath, err))
	}

	// Read-back verify: inode identity with the primary is what makes
	// the member a hard link rather than a copy (FR-039's "the family
	// shares an inode").
	written, lerr := os.Lstat(item.DestPath)
	if lerr != nil {
		return d.failLink(item, fmt.Errorf("read-back verify: %w", lerr))
	}
	if !os.SameFile(primaryInfo, written) {
		_ = os.Remove(item.DestPath)
		return d.failLink(item, fmt.Errorf("read-back inode identity with %s failed", item.PrimaryDestPath))
	}

	// INFO-5: the detached-mark window (10.1, markContext): the
	// read-back verified inode identity, so the mark completes even
	// when a SIGINT cancelled the run context between verify and here.
	markCtx, cancelMark := markContext(ctx)
	defer cancelMark()
	if err := d.Store.SetFileCopied(markCtx, item.FileID, item.MediaPath); err != nil {
		return d.failLink(item, fmt.Errorf("marking copied: %w", err))
	}
	warning := applyMetadata(item.DestPath, metaFromRaw(item.Mode, item.UID, item.GID, item.MtimeNS))
	d.emitDone(item.Size, item.RelPath)
	return Result{Outcome: OutcomeCopied, Bytes: item.Size, Attempts: 1, Warning: warning}
}

func (d Deps) failLink(item LinkItem, err error) Result {
	d.emitError(fmt.Sprintf("%s: %v", item.RelPath, err))
	return Result{Outcome: OutcomeFailed, Attempts: 1, Err: err}
}

// EmbedFile writes the bytes at srcPath into destPath through the SAME
// write discipline as payload items (remove stale partial, write,
// fsync, atomic rename, directory fsync, read-back verify against the
// source-stream hash captured during the write) but with NO catalog
// mark: MANIFEST.json and catalog-export.sqlite have no files rows
// (DESIGN_SPEC 5.3, 6.6). The caller records their image_contents rows
// itself (catalog.CopyStore.RecordEmbedded). It returns the published
// file's sha256 and size so those rows carry the exact figures.
//
// The read-back verify is what makes these members "ordinary members
// of the tree ... covered by the same write-verify discipline" (5.3):
// a torn export embed on a flaky NAS fails loudly instead of shipping
// a corrupt catalog snapshot.
func EmbedFile(ctx context.Context, deps Deps, srcPath, destPath string) ([]byte, int64, error) {
	d := deps.withDefaults()
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	partial := PartialPath(destPath)
	if err := os.Remove(partial); err != nil && !os.IsNotExist(err) {
		return nil, 0, fmt.Errorf("removing stale partial %s: %w", partial, err)
	}
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return nil, 0, fmt.Errorf("creating destination directory for %s: %w", destPath, err)
	}

	src, err := os.Open(srcPath)
	if err != nil {
		return nil, 0, fmt.Errorf("opening embed source %s: %w", srcPath, err)
	}
	partialFile, err := d.OpenPartial(partial)
	if err != nil {
		_ = src.Close()
		return nil, 0, fmt.Errorf("creating partial %s: %w", partial, err)
	}
	h := sha256.New()
	n, cerr := io.Copy(io.MultiWriter(partialFile, h), &ctxReader{ctx: ctx, r: src})
	if cerr != nil {
		_ = partialFile.Close()
		_ = os.Remove(partial)
		_ = src.Close()
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		return nil, 0, fmt.Errorf("writing embed payload: %w", cerr)
	}
	if err := src.Close(); err != nil {
		_ = partialFile.Close()
		_ = os.Remove(partial)
		return nil, 0, fmt.Errorf("closing embed source: %w", err)
	}
	if err := partialFile.Sync(); err != nil {
		_ = partialFile.Close()
		_ = os.Remove(partial)
		return nil, 0, fmt.Errorf("fsync partial %s: %w", partial, err)
	}
	if err := partialFile.Close(); err != nil {
		_ = os.Remove(partial)
		return nil, 0, fmt.Errorf("closing partial %s: %w", partial, err)
	}
	if err := os.Rename(partial, destPath); err != nil {
		_ = os.Remove(partial)
		return nil, 0, fmt.Errorf("renaming %s to %s: %w", partial, destPath, err)
	}
	if err := syncDir(filepath.Dir(destPath)); err != nil {
		_ = os.Remove(destPath)
		return nil, 0, fmt.Errorf("fsync directory of %s: %w", destPath, err)
	}
	readHash, err := d.hashFile(destPath)
	if err != nil {
		_ = os.Remove(destPath)
		return nil, 0, fmt.Errorf("read-back verify of %s: %w", destPath, err)
	}
	if expected := h.Sum(nil); !bytes.Equal(readHash, expected) {
		_ = os.Remove(destPath)
		return nil, 0, fmt.Errorf("read-back hash mismatch for %s: written %x, expected %x", destPath, readHash, expected)
	}
	return readHash, n, nil
}
