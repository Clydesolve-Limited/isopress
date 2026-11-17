package imagebuild

// T4.2 QA hardening pass: edge-case matrix for the staging subsystem,
// organized by the audit areas of the verification brief:
//
//   - METADATA EDGE: future/epoch/pre-epoch mtimes through utimensat,
//     the non-root chown EPERM partial-application honesty (mode AND
//     mtime still land), and the staging ORDER (files before empty
//     dirs) with its setgid-inheritance consequence.
//   - SOURCE-CHANGED EDGE: symlink retargeting (and the documented
//     same-mtime-same-size blind spot), a chunk PARENT mutated between
//     two chunks (per-chunk compare), and a source-changed failure on
//     the LAST item (error-path scratch cleanup).
//   - PRE-CHECK ARITHMETIC: the fixed-term (manifest+export) rationale
//     pinned via the canonical units estimator, the zero-payload
//     (dirs-only) boundary, the MaxInt64 payload saturation (total
//     estimator, no wrap), and a FreeSpaceFunc returning MaxInt64.
//   - GRAFT GATE COMPLETENESS: '\r' and '=' across every member kind
//     (incl. chunk relpaths), the set-id/'=' question (set ids never
//     reach graft lines), and the overlong-component boundary.
//   - SCRATCH LEAKS: double-Cleanup safety, cancel during the last
//     item, and the post-last-member cancel boundary.
//   - HASHSTORE SEAM: a stored chunk hash that DISAGREES with the
//     staged bytes (classification: write fault, not source-changed),
//     and a HashStore persistence error.
//   - PATHLIST DETERMINISM: two Stage runs over the same inventory
//     produce byte-identical pathlists modulo the unique scratch root.
//
// Everything runs against the real stager and a real temp filesystem;
// only the documented seams (FreeSpace, HashStore, Source) are faked.

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"isopress/internal/copyengine"
	"isopress/internal/fsmeta"
	"isopress/internal/media"
	"isopress/internal/units"
)

// --- small self-contained fixture helpers ------------------------------------

// simpleStageInput builds a minimal valid StageInput over root with one
// whole file (the catalog hash of content) so each edge test controls
// its own inventory exactly.
func simpleStageInput(t *testing.T, root, workDir string, rel string, content []byte, mtimeNS int64) StageInput {
	t.Helper()
	writeSrcFile(t, root, rel, content, 0o644, mtimeNS)
	row := stageFileOf(t, root, rel, 1)
	return StageInput{
		SetID: "edges", ImageID: 1, SourceRoot: root,
		WorkDir: workDir, UsableSize: 1_000_000_000,
		Inventory: StageInventory{Files: []StageFile{row}},
	}
}

// --- METADATA EDGE ------------------------------------------------------------

// TestStageMtimeEdgesApplied pins that utimensat (AT_SYMLINK_NOFOLLOW,
// no time_t range restriction) accepts and EXACTLY reproduces every
// catalog mtime the ingest can record: the Unix epoch (0), a
// pre-epoch negative timestamp, and a FUTURE timestamp (year ~2096).
// These are the values a real archive carries (old backups, clock-skew
// captures, planned-dated files); a stager that silently clamped them
// would master a Rock Ridge timestamp different from the catalog record
// (FR-036 violation), so the equality is asserted at nanosecond
// precision on the staged bytes' metadata.
func TestStageMtimeEdgesApplied(t *testing.T) {
	cases := []struct {
		name    string
		mtimeNS int64
	}{
		{"unix epoch zero", 0},
		{"pre-epoch (1969)", -1_000_000_000_000_000_000},
		{"future (~2096)", 4_000_000_000_000_000_000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := newSourceRoot(t)
			abs := writeSrcFile(t, root, "edge.txt", []byte("edge\n"), 0o644, 0)
			if _, err := fsmeta.Apply(abs, fsmeta.Meta{Mode: fsmeta.GoMode(0o644), MtimeNS: tc.mtimeNS}); err != nil {
				t.Fatalf("pinning source mtime: %v", err)
			}
			// The row is read AFTER the pin so the catalog snapshot
			// (the D-10 compare target) carries the same mtime.
			in := StageInput{
				SetID: "edges", ImageID: 1, SourceRoot: root,
				WorkDir: t.TempDir(), UsableSize: 1_000_000_000,
				Inventory: StageInventory{Files: []StageFile{stageFileOf(t, root, "edge.txt", 1)}},
			}

			st, err := Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace}, in)
			if err != nil {
				t.Fatalf("Stage: %v (utimensat must accept every catalog mtime)", err)
			}
			defer st.Cleanup() //nolint:errcheck

			meta, err := fsmeta.PathMeta(filepath.Join(st.Root, "files", "edge.txt"))
			if err != nil {
				t.Fatalf("lstat staged edge.txt: %v", err)
			}
			if meta.MtimeNS != tc.mtimeNS {
				t.Errorf("staged mtime = %d, want the catalog %d (Rock Ridge masters the staged mtime verbatim)",
					meta.MtimeNS, tc.mtimeNS)
			}
		})
	}
}

// TestStageChownEPERMStillAppliesModeAndMtime pins the 5.4
// partial-application honesty end to end: as non-root, the staged file
// keeps the CATALOG owner's record it cannot apply (warning), but BOTH
// the mode AND the nanosecond mtime still land on the staged bytes.
// This is what xorriso's Rock Ridge will read, so a downgrade that also
// dropped mtime would silently master wrong metadata (FR-036).
func TestStageChownEPERMStillAppliesModeAndMtime(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("EPERM chown cannot be provoked as root")
	}
	root := newSourceRoot(t)
	const pinned = 1_700_000_077_777_777_777
	abs := writeSrcFile(t, root, "foreign.txt", []byte("x\n"), 0o640, pinned)
	if _, err := fsmeta.Apply(abs, fsmeta.Meta{Mode: fsmeta.GoMode(0o640), MtimeNS: pinned}); err != nil {
		t.Fatalf("pin mtime: %v", err)
	}
	row := stageFileOf(t, root, "foreign.txt", 1)
	row.UID = 65534 // nobody: ownership this process may not take
	row.GID = 65534

	in := StageInput{
		SetID: "ep2", ImageID: 1, SourceRoot: root,
		WorkDir: t.TempDir(), UsableSize: 10_000_000,
		Inventory: StageInventory{Files: []StageFile{row}},
	}
	st, err := Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace}, in)
	if err != nil {
		t.Fatalf("Stage: %v (the EPERM downgrade must not fail the item)", err)
	}
	defer st.Cleanup() //nolint:errcheck

	if len(st.Warnings) != 1 || !strings.Contains(st.Warnings[0], "EPERM") {
		t.Fatalf("Warnings = %v, want exactly the chown EPERM downgrade", st.Warnings)
	}
	meta, err := fsmeta.PathMeta(filepath.Join(st.Root, "files", "foreign.txt"))
	if err != nil {
		t.Fatalf("lstat staged foreign.txt: %v", err)
	}
	if meta.Mode.Perm() != 0o640 {
		t.Errorf("staged mode = %04o, want 0640 (chmod runs after the downgraded chown)", meta.Mode.Perm())
	}
	if meta.MtimeNS != pinned {
		t.Errorf("staged mtime = %d, want the catalog %d (the EPERM downgrade must not drop mtime)", meta.MtimeNS, pinned)
	}
	if meta.UID == 65534 {
		t.Errorf("staged uid = 65534; the non-root lchown should have been denied, not silently succeed")
	}
}

// TestStageSetgidDirAppliedAfterFiles pins the staging ORDER and its
// setgid-inheritance consequence. stageAll stages files, chunks, and
// symlinks BEFORE it creates/applies the empty-dir records (the
// copyengine specials convention), so:
//
//  1. a staged dir's catalog mtime SURVIVES files being created inside
//     it during staging (the dir metadata is applied last, after the
//     tree churn), and
//  2. a setgid bit on a staged dir can never leak into a staged FILE's
//     mode, because no file is created inside any catalog dir record
//     after that dir's setgid mode is applied - every staged member
//     carries exactly its own catalog metadata, which is what Rock
//     Ridge masters (FR-036 per-member fidelity).
//
// The inventory below is deliberately CONTRADICTORY (a dir claimed
// empty while a file is staged inside it) - a honest catalog never
// produces it, but it is precisely the shape that would expose an
// ordering bug: if dirs were applied first, the file's creation would
// (a) clobber the dir's mtime and (b) potentially inherit setgid.
func TestStageSetgidDirAppliedAfterFiles(t *testing.T) {
	root := newSourceRoot(t)
	const dirMtime = 1_700_000_300_000_000_000
	writeSrcFile(t, root, "sgdir/data.txt", []byte("payload\n"), 0o644, 0)
	if err := os.MkdirAll(filepath.Join(root, "sgdir"), 0o755); err != nil {
		t.Fatalf("mkdir sgdir: %v", err)
	}

	in := StageInput{
		SetID: "sgid", ImageID: 1, SourceRoot: root,
		WorkDir: t.TempDir(), UsableSize: 10_000_000,
		Inventory: StageInventory{
			Files: []StageFile{stageFileOf(t, root, "sgdir/data.txt", 1)},
			Dirs: []StageDir{{
				DirID: 1, RelPath: "sgdir", Mode: 0o2755,
				UID: uint32(os.Getuid()), GID: uint32(os.Getgid()), MtimeNS: dirMtime,
			}},
		},
	}
	st, err := Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace}, in)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	defer st.Cleanup() //nolint:errcheck

	// 1. The dir's setgid mode lands (GoMode chain, like setuid).
	dirMeta, err := fsmeta.PathMeta(filepath.Join(st.Root, "files", "sgdir"))
	if err != nil {
		t.Fatalf("lstat staged sgdir: %v", err)
	}
	if got := fsmeta.UnixMode(dirMeta.Mode); got != 0o2755 {
		t.Errorf("staged sgdir raw mode = %04o, want 2755 (setgid must survive the chain)", got)
	}
	// 2. The dir's catalog mtime survives the file created inside it
	// during staging - observable proof that dirs are applied LAST.
	if dirMeta.MtimeNS != dirMtime {
		t.Errorf("staged sgdir mtime = %d, want the catalog %d (dir metadata applied after all files)", dirMeta.MtimeNS, dirMtime)
	}
	// 3. The staged file carries exactly its own catalog mode: no
	// setgid inheritance, whatever the parent dir record says.
	fileMeta, err := fsmeta.PathMeta(filepath.Join(st.Root, "files", "sgdir", "data.txt"))
	if err != nil {
		t.Fatalf("lstat staged data.txt: %v", err)
	}
	if got := fsmeta.UnixMode(fileMeta.Mode); got != 0o644 {
		t.Errorf("staged sgdir/data.txt raw mode = %04o, want 0644 (files are staged before dir records are applied - no setgid inheritance)", got)
	}
}

// --- SOURCE-CHANGED EDGE ------------------------------------------------------

// TestStageSymlinkRetargetedCaught pins that a symlink RETARGETED after
// the scan is caught by the staging compare: a retarget rewrites the
// link, which moves its mtime, and the size+mtime compare (the only
// fields the symlink inventory carries - see stageSymlink) fires the
// 3.5 point 3 build failure.
func TestStageSymlinkRetargetedCaught(t *testing.T) {
	root := newSourceRoot(t)
	const pinned = 1_700_000_400_000_000_000
	link := filepath.Join(root, "rel")
	if err := os.Symlink("original.txt", link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := fsmeta.Apply(link, fsmeta.Meta{MtimeNS: pinned}); err != nil {
		t.Fatalf("pin symlink mtime: %v", err)
	}
	meta, err := fsmeta.PathMeta(link)
	if err != nil {
		t.Fatalf("lstat symlink: %v", err)
	}
	in := StageInput{
		SetID: "retarget", ImageID: 1, SourceRoot: root,
		WorkDir: t.TempDir(), UsableSize: 10_000_000,
		Inventory: StageInventory{Symlinks: []StageSymlink{{
			FileID: 7, RelPath: "rel", LinkTarget: "original.txt", Size: meta.Size,
			Mode: fsmeta.UnixMode(meta.Mode), UID: meta.UID, GID: meta.GID, MtimeNS: meta.MtimeNS,
		}}},
	}

	// Same-LENGTH retarget after the "scan": the size field alone
	// would not notice; the mtime move must. ("original.txt" and
	// "modified.txt" are both 12 bytes, so only mtime can fire.)
	if err := os.Remove(link); err != nil {
		t.Fatalf("remove symlink: %v", err)
	}
	if err := os.Symlink("modified.txt", link); err != nil {
		t.Fatalf("retarget symlink: %v", err)
	}

	_, err = Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace}, in)
	var sce *SourceChangedError
	if !errors.As(err, &sce) {
		t.Fatalf("err = %v, want *SourceChangedError (retarget moves the link mtime)", err)
	}
	if sce.Diff != fsmeta.DiffMtimeNS {
		t.Errorf("Diff = %v, want DiffMtimeNS only (same-length retarget)", sce.Diff)
	}
	entries, rerr := os.ReadDir(in.WorkDir)
	if rerr != nil {
		t.Fatalf("read work dir: %v", rerr)
	}
	if len(entries) != 0 {
		t.Errorf("failed build leaked scratch: %v", entries)
	}
}

// TestStageSymlinkSameMtimeRetargetDocumented pins the DOCUMENTED blind
// spot of the symlink compare (stageSymlink's doc): a retarget that
// preserves BOTH size and mtime is not detectable at staging - the
// stager masters the RECORDED target. This is the accepted boundary:
// the symlink inventory carries no dev/inode (catalog.CopySymlink
// shape), and copy mode does no symlink compare at all, so staging is
// strictly stronger, never weaker.
func TestStageSymlinkSameMtimeRetargetDocumented(t *testing.T) {
	root := newSourceRoot(t)
	const pinned = 1_700_000_500_000_000_000
	link := filepath.Join(root, "rel")
	if err := os.Symlink("original.txt", link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := fsmeta.Apply(link, fsmeta.Meta{MtimeNS: pinned}); err != nil {
		t.Fatalf("pin symlink mtime: %v", err)
	}
	meta, err := fsmeta.PathMeta(link)
	if err != nil {
		t.Fatalf("lstat symlink: %v", err)
	}
	in := StageInput{
		SetID: "blindspot", ImageID: 1, SourceRoot: root,
		WorkDir: t.TempDir(), UsableSize: 10_000_000,
		Inventory: StageInventory{Symlinks: []StageSymlink{{
			FileID: 8, RelPath: "rel", LinkTarget: "original.txt", Size: meta.Size,
			Mode: fsmeta.UnixMode(meta.Mode), UID: meta.UID, GID: meta.GID, MtimeNS: meta.MtimeNS,
		}}},
	}

	// Retarget to a same-length name and RESTORE the catalog mtime:
	// size+mtime both match, so staging proceeds (documented limit).
	if err := os.Remove(link); err != nil {
		t.Fatalf("remove symlink: %v", err)
	}
	if err := os.Symlink("modified.txt", link); err != nil { // 12 bytes, like original.txt
		t.Fatalf("retarget symlink: %v", err)
	}
	if _, err := fsmeta.Apply(link, fsmeta.Meta{MtimeNS: pinned}); err != nil {
		t.Fatalf("restore symlink mtime: %v", err)
	}

	st, err := Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace}, in)
	if err != nil {
		t.Fatalf("Stage: %v (same size+mtime is the documented undetectable retarget)", err)
	}
	defer st.Cleanup() //nolint:errcheck

	got, err := os.Readlink(filepath.Join(st.Root, "files", "rel"))
	if err != nil {
		t.Fatalf("read staged link: %v", err)
	}
	if got != "original.txt" {
		t.Errorf("staged link target = %q, want the RECORDED target %q (the documented blind spot masters the record, not the current source state)",
			got, "original.txt")
	}
}

// mutatingHookStore wraps a ChunkHashStore and runs a hook after a
// given chunk's capture is persisted - the seam used to mutate a chunk
// PARENT between two chunks of the same file.
type mutatingHookStore struct {
	inner *recordingHashStore
	after func(fileID int64, index int)
}

func (s *mutatingHookStore) SetChunkHash(ctx context.Context, fileID int64, index int, hash []byte) error {
	if err := s.inner.SetChunkHash(ctx, fileID, index, hash); err != nil {
		return err
	}
	if s.after != nil {
		s.after(fileID, index)
	}
	return nil
}

// TestStageChunkParentChangedBetweenChunks pins the PER-CHUNK parent
// compare: the D-10 lstat check runs before EVERY chunk's read, so a
// parent mutated after chunk 1 was staged is caught at chunk 2's gate -
// a mid-file change can never leak stale bytes into the later chunks of
// the same image.
func TestStageChunkParentChangedBetweenChunks(t *testing.T) {
	root := newSourceRoot(t)
	// Non-uniform 150-byte parent (3 x 50-byte chunks): every chunk
	// range has distinct content, so no chunk can accidentally verify
	// against a WRONG-range hash - the offset-threading regression
	// (staging a chunk's prefix instead of its range) cannot hide
	// behind repeated bytes here.
	content := distinctBytes(150)
	writeSrcFile(t, root, "multi.dat", content, 0o644, 0)
	parentMeta, err := fsmeta.PathMeta(filepath.Join(root, "multi.dat"))
	if err != nil {
		t.Fatalf("lstat parent: %v", err)
	}
	chunk := func(idx int, offset int64, hash []byte) StageChunk {
		return StageChunk{
			FileID: 42, ChunkIndex: idx, Offset: offset, Length: 50, Hash: hash,
			ParentRelPath: "multi.dat", ParentSize: parentMeta.Size, ParentMtimeNS: parentMeta.MtimeNS,
			ParentDev: int64(parentMeta.Dev), ParentInode: int64(parentMeta.Inode),
		}
	}
	// Chunk 0 is a first write (so SetChunkHash fires the mutation
	// hook after it is fully staged); chunks 1 and 2 carry stored
	// hashes.
	in := StageInput{
		SetID: "midfile", ImageID: 1, SourceRoot: root,
		WorkDir: t.TempDir(), UsableSize: 10_000_000,
		Inventory: StageInventory{Chunks: []StageChunk{
			chunk(0, 0, nil),
			chunk(1, 50, make([]byte, 32)),
			chunk(2, 100, make([]byte, 32)),
		}},
	}
	store := &mutatingHookStore{
		inner: newRecordingHashStore(),
		after: func(fileID int64, index int) {
			if index == 0 && fileID == 42 {
				// The parent grows between chunk 1 and chunk 2.
				if err := os.Truncate(filepath.Join(root, "multi.dat"), 151); err != nil {
					t.Fatalf("mutating parent: %v", err)
				}
			}
		},
	}

	_, err = Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace, HashStore: store}, in)
	var sce *SourceChangedError
	if !errors.As(err, &sce) {
		t.Fatalf("err = %v, want *SourceChangedError from chunk 2's per-chunk parent compare", err)
	}
	if sce.RelPath != "multi.dat" {
		t.Errorf("RelPath = %q, want multi.dat", sce.RelPath)
	}
	if sce.Diff&fsmeta.DiffSize == 0 {
		t.Errorf("Diff = %v, want it to name the size field (the truncation also moves mtime)", sce.Diff)
	}
	if !strings.Contains(err.Error(), "chunk 1") {
		t.Errorf("message %q does not name the failing chunk index (want 'chunk 1', the 0-based index of the second chunk)", err.Error())
	}
}

// TestStageSourceChangedOnLastItemCleansScratch pins the error-path
// cleanup when the source-changed failure lands on the LAST inventory
// item: the partial staging tree that already exists under the scratch
// is removed by Stage itself (Staged.Cleanup's contract covers only a
// SUCCESSFUL Stage; a failed Stage owns and removes its scratch).
func TestStageSourceChangedOnLastItemCleansScratch(t *testing.T) {
	root := newSourceRoot(t)
	workDir := t.TempDir()
	writeSrcFile(t, root, "a-first.txt", []byte("first\n"), 0o644, 0)
	writeSrcFile(t, root, "z-last.txt", []byte("last\n"), 0o644, 0)
	// The last inventory item is a SYMLINK (staged last, after files).
	symlink := filepath.Join(root, "zz-symlink")
	if err := os.Symlink("a-first.txt", symlink); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	syMeta, err := fsmeta.PathMeta(symlink)
	if err != nil {
		t.Fatalf("lstat symlink: %v", err)
	}
	in := StageInput{
		SetID: "last", ImageID: 1, SourceRoot: root,
		WorkDir: workDir, UsableSize: 10_000_000,
		Inventory: StageInventory{
			Files: []StageFile{
				stageFileOf(t, root, "a-first.txt", 1),
				stageFileOf(t, root, "z-last.txt", 2),
			},
			Symlinks: []StageSymlink{{
				FileID: 3, RelPath: "zz-symlink", LinkTarget: "a-first.txt", Size: syMeta.Size,
				Mode: fsmeta.UnixMode(syMeta.Mode), UID: syMeta.UID, GID: syMeta.GID, MtimeNS: syMeta.MtimeNS,
			}},
		},
	}
	// The last inventory item changes after the scan.
	if err := os.Remove(symlink); err != nil {
		t.Fatalf("remove symlink: %v", err)
	}
	if err := os.Symlink("nowhere.txt", symlink); err != nil {
		t.Fatalf("retarget symlink: %v", err)
	}
	// EXPLICIT mtime move (>= 2s, absolute): the retarget's target is
	// the SAME LENGTH as the recorded one ("a-first.txt" -> "nowhere.txt",
	// both 11 bytes), so size cannot separate the two - and the stager's
	// symlink compare (stageSymlink) checks ONLY size + mtime_ns (the
	// 3.5 field set; catalog.CopySymlink carries no dev/inode). A
	// same-instant retarget may leave mtime inside the same tick on a
	// coarse-granularity CI filesystem (which rounds mtimes to 1s), so
	// the change is made unambiguous with an absolute +10s mtime set.
	// fsmeta.Apply is the project's no-follow convention: its mtime leg
	// is utimensat(AT_SYMLINK_NOFOLLOW) - os.Chtimes would FOLLOW the
	// link and mutate the TARGET's stamp instead of the link's (and
	// a-first.txt is itself an inventory member staged earlier).
	if _, err := fsmeta.Apply(symlink, fsmeta.Meta{
		UID: uint32(os.Getuid()), GID: uint32(os.Getgid()),
		MtimeNS: time.Now().Add(10 * time.Second).UnixNano(),
	}); err != nil {
		t.Fatalf("applying the retargeted symlink's mtime: %v", err)
	}

	_, err = Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace}, in)
	var sce *SourceChangedError
	if !errors.As(err, &sce) {
		t.Fatalf("err = %v, want *SourceChangedError on the last item", err)
	}
	if sce.RelPath != "zz-symlink" {
		t.Errorf("RelPath = %q, want zz-symlink", sce.RelPath)
	}
	// Two files were fully staged before the failure - the partial
	// tree existed - and the scratch is nonetheless gone.
	entries, rerr := os.ReadDir(workDir)
	if rerr != nil {
		t.Fatalf("read work dir: %v", rerr)
	}
	if len(entries) != 0 {
		t.Errorf("mid-tree source-changed leaked scratch (partial staging tree): %v", entries)
	}
}

// --- PRE-CHECK ARITHMETIC -----------------------------------------------------

// TestRequiredImageBytesFixedTermRationale pins the canonical
// estimator's exact boundary arithmetic from the builder's side
// (units.RequiredImageBytes, since the 2026-10-20 amendment): the two
// image-root members T4.3 embeds (MANIFEST.json,
// catalog-export.sqlite) are ISO entries whose cost now lives in the
// FIXED term (units.FixedImageOverhead = 2 MiB + 512*2), so
// validateInput counts payload entries only and the estimator adds
// the embedded pair exactly once. Their BYTES are unknowable at
// staging time; the 2 MiB base is their cushion.
func TestRequiredImageBytesFixedTermRationale(t *testing.T) {
	if got, want := units.RequiredImageBytes(0, 0), units.FixedImageOverhead; got != want {
		t.Errorf("RequiredImageBytes(0,0) = %d, want the fixed %d base", got, want)
	}
	if got, want := units.RequiredImageBytes(0, 2), units.FixedImageOverhead+2*units.ImageEntryOverhead; got != want {
		t.Errorf("RequiredImageBytes(0,2) = %d, want fixed+2*512", got)
	}
	if got, want := units.RequiredImageBytes(1000, 5), int64(1000+units.FixedImageOverhead+5*units.ImageEntryOverhead); got != want {
		t.Errorf("RequiredImageBytes(1000,5) = %d, want payload+fixed+5*512", got)
	}
}

// TestPreCheckZeroPayloadDirsOnly pins the pre-check boundary for an
// image with ZERO payload bytes (a dirs-only image: the 5.3 empty-dir
// skeleton and nothing else). The 2 MiB floor plus the per-entry
// overhead still applies, and the exact boundary is pass-at-equal,
// fail-below.
func TestPreCheckZeroPayloadDirsOnly(t *testing.T) {
	root := newSourceRoot(t)
	if err := os.MkdirAll(filepath.Join(root, "empty-one"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	buildInput := func(workDir string, usable int64) StageInput {
		return StageInput{
			SetID: "dirs-only", ImageID: 1, SourceRoot: root,
			WorkDir: workDir, UsableSize: usable,
			Inventory: StageInventory{Dirs: []StageDir{{
				DirID: 1, RelPath: "empty-one", Mode: 0o755,
				UID: uint32(os.Getuid()), GID: uint32(os.Getgid()), MtimeNS: 1_700_000_600_000_000_000,
			}}},
		}
	}
	// n = 1 payload dir (the 2 embedded members live in the fixed
	// term); payload = 0.
	required := units.RequiredImageBytes(0, 1)

	// Boundary: usable == required passes and stages.
	st, err := Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace}, buildInput(t.TempDir(), required))
	if err != nil {
		t.Fatalf("Stage at the exact boundary: %v (pass-at-equal)", err)
	}
	defer st.Cleanup() //nolint:errcheck
	if st.PayloadBytes != 0 {
		t.Errorf("PayloadBytes = %d, want 0 for a dirs-only image", st.PayloadBytes)
	}

	// One byte below: the 6.3 step 5 plan error, before any scratch.
	workDir := t.TempDir()
	_, err = Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace}, buildInput(workDir, required-1))
	usageErrOf(t, err) // UsageError, exit-2 plan class
	if !strings.Contains(err.Error(), "does not fit its media profile") {
		t.Errorf("message %q does not carry the 6.3 step 5 plan-error shape", err.Error())
	}
	entries, rerr := os.ReadDir(workDir)
	if rerr != nil {
		t.Fatalf("read work dir: %v", rerr)
	}
	if len(entries) != 0 {
		t.Errorf("capacity refusal leaked scratch: %v", entries)
	}
}

// TestPreCheckMaxInt64PayloadSaturates pins the int64-boundary behavior
// of the 6.3 estimator: the estimator is TOTAL - a corrupt-catalog
// payload near math.MaxInt64 SATURATES at math.MaxInt64 instead of
// wrapping negative (see the RequiredImageBytes saturation contract).
// A saturated required is "unfailably over capacity": pre-check (b)
// REFUSES it with the plan-class 6.3 step 5 error (deliberate refusal,
// no longer a vacuous pass that only the free-space gate happened to
// catch), and the message carries a positive "required" figure. The
// stage fails with zero scratch and no writer involvement.
func TestPreCheckMaxInt64PayloadSaturates(t *testing.T) {
	// Saturation pin: MaxInt64-adjacent payload -> MaxInt64, not negative.
	if got, want := units.RequiredImageBytes(math.MaxInt64, 3), int64(math.MaxInt64); got != want {
		t.Errorf("RequiredImageBytes(MaxInt64, 3) = %d, want saturated %d (never a wrapped negative)", got, want)
	}
	// The exact-boundary payload (MaxInt64 - overhead) still computes
	// the true total without saturating.
	if got, want := units.RequiredImageBytes(math.MaxInt64-units.FixedImageOverhead-3*units.ImageEntryOverhead, 3), int64(math.MaxInt64); got != want {
		t.Errorf("RequiredImageBytes at the exact int64 boundary = %d, want the true total %d", got, want)
	}

	root := newSourceRoot(t)
	writeSrcFile(t, root, "bogus.dat", []byte("1 byte on disk"), 0o644, 0)
	bogus := stageFileOf(t, root, "bogus.dat", 1)
	bogus.Size = math.MaxInt64 // corrupt catalog row

	workDir := t.TempDir()
	in := StageInput{
		SetID: "overflow", ImageID: 1, SourceRoot: root,
		WorkDir: workDir, UsableSize: 23_000_000_000,
		Inventory: StageInventory{Files: []StageFile{bogus}},
	}
	_, err := Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace}, in)
	if err == nil {
		t.Fatal("Stage succeeded with a MaxInt64 payload; the saturated required must be refused by pre-check (b)")
	}
	ue := usageErrOf(t, err) // the plan-class refusal, deliberately
	if !strings.Contains(ue.Error(), "does not fit its media profile") {
		t.Errorf("message %q does not carry the 6.3 step 5 plan-error shape", ue.Error())
	}
	entries, rerr := os.ReadDir(workDir)
	if rerr != nil {
		t.Fatalf("read work dir: %v", rerr)
	}
	if len(entries) != 0 {
		t.Errorf("overflow refusal leaked scratch: %v", entries)
	}
}

// TestPreCheckMaxInt64FreeSpaceNoWrap pins the free-space comparison
// against an absurdly large (but representable) probe result: an
// avail of math.MaxInt64 must PASS the uint64 comparison for a payload
// that fits, i.e. the comparison never wraps a small required figure
// into failure (or vice versa).
func TestPreCheckMaxInt64FreeSpaceNoWrap(t *testing.T) {
	root := newSourceRoot(t)
	workDir := t.TempDir()
	in := simpleStageInput(t, root, workDir, "fits.txt", []byte("fits\n"), 0)

	deps := Deps{FreeSpace: func(string) (uint64, error) { return math.MaxInt64, nil }}
	st, err := Stage(context.Background(), deps, in)
	if err != nil {
		t.Fatalf("Stage with MaxInt64 avail: %v (the comparison must not wrap)", err)
	}
	defer st.Cleanup() //nolint:errcheck
}

// --- GRAFT GATE COMPLETENESS --------------------------------------------------

// TestStageGraftGateEveryMemberKind pins the line-oriented graft gate
// ('\n', '\r', '=') across EVERY member kind, including CHUNK relpaths
// (whose media path derives from the parent relpath plus the tool
// fixed chunk name - the gate runs on the DERIVED media path, so a CR
// in the parent is caught) and DIR relpaths.
func TestStageGraftGateEveryMemberKind(t *testing.T) {
	sha32 := make([]byte, 32) // any 32 bytes: the gate fires before any hash check
	cases := []struct {
		name    string
		inv     StageInventory
		wantIs  error
		wantMsg string
	}{
		{
			name: "file with CR",
			inv: StageInventory{Files: []StageFile{
				{FileID: 1, RelPath: "a\rb", Size: 1, Hash: sha32},
			}},
			wantIs:  ErrUngraftablePath,
			wantMsg: "graft-points",
		},
		{
			name: "chunk parent with newline",
			inv: StageInventory{Chunks: []StageChunk{
				{FileID: 1, ChunkIndex: 0, Length: 1, ParentRelPath: "a\nb"},
			}},
			wantIs:  ErrUngraftablePath,
			wantMsg: "graft-points",
		},
		{
			name: "chunk parent with CR",
			inv: StageInventory{Chunks: []StageChunk{
				{FileID: 1, ChunkIndex: 0, Length: 1, ParentRelPath: "a\rb"},
			}},
			wantIs:  ErrUngraftablePath,
			wantMsg: "graft-points",
		},
		{
			name: "chunk parent with equals",
			inv: StageInventory{Chunks: []StageChunk{
				{FileID: 1, ChunkIndex: 0, Length: 1, ParentRelPath: "a=b"},
			}},
			wantIs:  ErrUngraftablePath,
			wantMsg: "graft-points",
		},
		{
			name: "dir with equals",
			inv: StageInventory{Dirs: []StageDir{
				{DirID: 1, RelPath: "a=b"},
			}},
			wantIs:  ErrUngraftablePath,
			wantMsg: "graft-points",
		},
		{
			name: "symlink with CR",
			inv: StageInventory{Symlinks: []StageSymlink{
				{FileID: 1, RelPath: "a\rb", LinkTarget: "t", Size: 1},
			}},
			wantIs:  ErrUngraftablePath,
			wantMsg: "graft-points",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := newSourceRoot(t)
			workDir := t.TempDir()
			in := StageInput{
				SetID: "gate", ImageID: 1, SourceRoot: root,
				WorkDir: workDir, UsableSize: 1_000_000_000, Inventory: tc.inv,
			}
			_, err := Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace}, in)
			if err == nil {
				t.Fatal("Stage succeeded, want graft-gate refusal")
			}
			if !errors.Is(err, tc.wantIs) {
				t.Errorf("error %v does not wrap %v", err, tc.wantIs)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantMsg)
			}
			entries, rerr := os.ReadDir(workDir)
			if rerr != nil {
				t.Fatalf("read work dir: %v", rerr)
			}
			if len(entries) != 0 {
				t.Errorf("refusal leaked scratch: %v", entries)
			}
		})
	}
}

// TestStageSetIDNeverReachesGraftLines pins the answer to the
// set-id-'=' question: a set id is USER input (DESIGN_SPEC 6.4 derives
// or accepts it) and media.validateSetID refuses '/', NUL, non-UTF-8,
// and "."/".." - but NOT '=' or newlines. That is SAFE for the
// pathlist, because the set id never appears in a graft line: the ISO
// paths are tool-generated ("/files/...", "/chunks/...", the two
// image-root constant names), so no set-id byte can ever mangle the
// iso=disk split. The set id DOES flow into the output FILE NAME and
// the -V volume id (T4.3's xorriso argv, not this package's files).
func TestStageSetIDNeverReachesGraftLines(t *testing.T) {
	root := newSourceRoot(t)
	writeSrcFile(t, root, "x.txt", []byte("x\n"), 0o644, 0)
	in := StageInput{
		SetID: "set=id=with=equals", ImageID: 1, SourceRoot: root,
		WorkDir: t.TempDir(), UsableSize: 10_000_000,
		Inventory: StageInventory{Files: []StageFile{stageFileOf(t, root, "x.txt", 1)}},
	}
	st, err := Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace}, in)
	if err != nil {
		t.Fatalf("Stage with '=' set id: %v (set ids never reach graft lines)", err)
	}
	defer st.Cleanup() //nolint:errcheck

	if len(st.PathList) != 1 {
		t.Fatalf("pathlist has %d entries, want 1", len(st.PathList))
	}
	e := st.PathList[0]
	if strings.Contains(e.ISOPath, "=") || strings.Contains(e.ISOPath, in.SetID) {
		t.Errorf("ISO path %q carries set-id bytes; the ISO side must be tool-generated only", e.ISOPath)
	}
	if e.ISOPath != "/files/x.txt" {
		t.Errorf("ISO path = %q, want the tool-generated /files/x.txt", e.ISOPath)
	}
	// Where the set id DOES go: the copy-set dir / ISO file name
	// (media.ImageDirName). It accepts '=' today - the '=' is harmless
	// THERE because the filename is argv, never a pathlist line. This
	// assertion documents the boundary rather than endorsing xorriso's
	// tolerance of exotic -o arguments (T4.3's concern).
	if _, err := media.ImageDirName(in.SetID, 1); err != nil {
		t.Errorf("ImageDirName(%q) = %v; set-id validation is media's gate, not the graft gate", in.SetID, err)
	}
}

// TestStageOverlongPathComponentFails pins the CURRENT boundary for
// very long paths: media.ValidateRelPath imposes no length limit, so
// an overlong COMPONENT passes the gate and fails at the filesystem
// (ENAMETOOLONG on the .partial create), classified as a write fault
// (retried per 5.1 step 7) and finally failing the item with the
// scratch cleaned. The filesystem is the boundary today; if a future
// xorriso-side path limit needs a clearer message, it belongs at this
// gate.
func TestStageOverlongPathComponentFails(t *testing.T) {
	root := newSourceRoot(t)
	writeSrcFile(t, root, "ok.txt", []byte("ok\n"), 0o644, 0)
	long := strings.Repeat("L", 300) // > NAME_MAX (255) on ext4/tmpfs
	bogus := stageFileOf(t, root, "ok.txt", 1)
	bogus.RelPath = long
	bogus.FileID = 2

	workDir := t.TempDir()
	in := StageInput{
		SetID: "longpath", ImageID: 1, SourceRoot: root,
		WorkDir: workDir, UsableSize: 1_000_000_000,
		Inventory: StageInventory{Files: []StageFile{bogus}},
	}
	_, err := Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace}, in)
	if err == nil {
		t.Fatal("Stage succeeded with a 300-byte path component")
	}
	if !errors.Is(err, syscall.ENAMETOOLONG) {
		t.Errorf("err = %v, want the ENAMETOOLONG filesystem boundary", err)
	}
	entries, rerr := os.ReadDir(workDir)
	if rerr != nil {
		t.Fatalf("read work dir: %v", rerr)
	}
	if len(entries) != 0 {
		t.Errorf("failed long-path stage leaked scratch: %v", entries)
	}
}

// --- SCRATCH LEAKS ------------------------------------------------------------

// TestStagedCleanupIdempotent pins the Cleanup contract: idempotent on
// repeated calls and nil-safe on a nil receiver (the T4.3 builder
// defers Cleanup and may also call it on an error path).
func TestStagedCleanupIdempotent(t *testing.T) {
	root := newSourceRoot(t)
	workDir := t.TempDir()
	in := simpleStageInput(t, root, workDir, "c.txt", []byte("c\n"), 0)

	st, err := Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace}, in)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	if err := st.Cleanup(); err != nil {
		t.Fatalf("first Cleanup: %v", err)
	}
	if err := st.Cleanup(); err != nil {
		t.Fatalf("second Cleanup must be a no-op, got %v", err)
	}
	if _, err := os.Lstat(st.Root); !os.IsNotExist(err) {
		t.Errorf("scratch still exists after Cleanup: %v", err)
	}
	var nilStaged *Staged
	if err := nilStaged.Cleanup(); err != nil {
		t.Errorf("nil-receiver Cleanup = %v, want nil", err)
	}
}

// TestStageCancelDuringLastItemCleansScratch pins the cancellation
// contract at the LAST payload item: a SIGINT arriving mid-copy of the
// final member fails the stage with context.Canceled and removes the
// scratch (the pathlist write is never reached). The strictly-later
// cancellation boundary (after the last member, during/before the
// pathlist write - the write itself is not ctx-aware) is pinned by
// TestStageCancelAfterLastMemberYieldsCompleteTree.
func TestStageCancelDuringLastItemCleansScratch(t *testing.T) {
	root := newSourceRoot(t)
	workDir := t.TempDir()
	in := simpleStageInput(t, root, workDir, "only.txt", []byte("only\n"), 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps := Deps{
		FreeSpace: hugeFreeSpace,
		Source:    cancelOnOpenSource{inner: copyengine.OSSource{}, cancel: cancel},
	}
	_, err := Stage(ctx, deps, in)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	entries, rerr := os.ReadDir(workDir)
	if rerr != nil {
		t.Fatalf("read work dir: %v", rerr)
	}
	if len(entries) != 0 {
		t.Errorf("cancel during the last item leaked scratch: %v", entries)
	}
}

// TestStageCancelAfterLastMemberYieldsCompleteTree pins the boundary of
// the cancellation contract: a cancel landing AFTER the last member is
// fully staged (here: inside the final chunk's hash persist) does NOT
// fail the stage - the staging result is already complete and
// consistent, the pathlist is written, and the caller (T4.3) checks
// ctx before invoking xorriso. Nothing partial exists for a later
// reader to find.
func TestStageCancelAfterLastMemberYieldsCompleteTree(t *testing.T) {
	root := newSourceRoot(t)
	content := []byte(strings.Repeat("B", 30))
	writeSrcFile(t, root, "one.dat", content, 0o644, 0)
	meta, err := fsmeta.PathMeta(filepath.Join(root, "one.dat"))
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	in := StageInput{
		SetID: "late-cancel", ImageID: 1, SourceRoot: root,
		WorkDir: t.TempDir(), UsableSize: 10_000_000,
		Inventory: StageInventory{Chunks: []StageChunk{{
			FileID: 9, ChunkIndex: 0, Offset: 0, Length: 30, Hash: nil, // first write
			ParentRelPath: "one.dat", ParentSize: meta.Size, ParentMtimeNS: meta.MtimeNS,
			ParentDev: int64(meta.Dev), ParentInode: int64(meta.Inode),
		}}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &mutatingHookStore{
		inner: newRecordingHashStore(),
		after: func(int64, int) { cancel() }, // after the ONLY member persisted
	}
	st, err := Stage(ctx, Deps{FreeSpace: hugeFreeSpace, HashStore: store}, in)
	if err != nil {
		t.Fatalf("Stage: %v (a post-last-member cancel leaves a complete, consistent tree)", err)
	}
	defer st.Cleanup() //nolint:errcheck

	if len(st.PathList) != 1 {
		t.Errorf("pathlist has %d entries, want the complete 1", len(st.PathList))
	}
	got := hashFileOrDie(t, filepath.Join(st.Root, "chunks", "one.dat", "00001"))
	if string(got) != string(sha256Of(content)) {
		t.Errorf("staged chunk hash %x, want %x (tree must be complete despite the late cancel)", got, sha256Of(content))
	}
}

// --- HASHSTORE SEAM -----------------------------------------------------------

// TestStageChunkStoredHashDisagreesWithStagedBytes pins the failure
// classification of the capture-vs-stored mismatch: a chunk whose
// STORED hash does not match the bytes staged from the source (the
// source drifted without moving size/mtime, or the catalog row is
// corrupt) fails as a WRITE-FAULT retry exhaustion with the read-back
// mismatch message - explicitly NOT as SourceChangedError (the parent
// lstat still matches; only the bytes disagree).
func TestStageChunkStoredHashDisagreesWithStagedBytes(t *testing.T) {
	root := newSourceRoot(t)
	content := []byte(strings.Repeat("S", 50))
	writeSrcFile(t, root, "s.dat", content, 0o644, 0)
	meta, err := fsmeta.PathMeta(filepath.Join(root, "s.dat"))
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	wrongHash := sha256Of([]byte("completely different bytes"))
	in := StageInput{
		SetID: "mismatch", ImageID: 1, SourceRoot: root,
		WorkDir: t.TempDir(), UsableSize: 10_000_000,
		Inventory: StageInventory{Chunks: []StageChunk{{
			FileID: 5, ChunkIndex: 0, Offset: 0, Length: 50, Hash: wrongHash,
			ParentRelPath: "s.dat", ParentSize: meta.Size, ParentMtimeNS: meta.MtimeNS,
			ParentDev: int64(meta.Dev), ParentInode: int64(meta.Inode),
		}}},
	}
	_, err = Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace, HashStore: newRecordingHashStore()}, in)
	if err == nil {
		t.Fatal("Stage succeeded with a stored hash that disagrees with the staged bytes")
	}
	var sce *SourceChangedError
	if errors.As(err, &sce) {
		t.Fatalf("error classified as source-changed (%v); the parent lstat matches - this is a read-back mismatch", err)
	}
	if !strings.Contains(err.Error(), "read-back hash mismatch") {
		t.Errorf("error %q does not carry the read-back mismatch classification", err.Error())
	}
}

// TestStageFirstWriteChunkHashStoreErrorFails pins the 6.3 step 1
// persistence contract: if the HashStore cannot persist a first-write
// capture, the item FAILS (the content list must never see a NULL
// chunk hash) and the scratch is cleaned.
func TestStageFirstWriteChunkHashStoreErrorFails(t *testing.T) {
	root := newSourceRoot(t)
	content := []byte(strings.Repeat("F", 20))
	writeSrcFile(t, root, "f.dat", content, 0o644, 0)
	meta, err := fsmeta.PathMeta(filepath.Join(root, "f.dat"))
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	in := StageInput{
		SetID: "store-err", ImageID: 1, SourceRoot: root,
		WorkDir: t.TempDir(), UsableSize: 10_000_000,
		Inventory: StageInventory{Chunks: []StageChunk{{
			FileID: 6, ChunkIndex: 0, Offset: 0, Length: 20, Hash: nil, // first write
			ParentRelPath: "f.dat", ParentSize: meta.Size, ParentMtimeNS: meta.MtimeNS,
			ParentDev: int64(meta.Dev), ParentInode: int64(meta.Inode),
		}}},
	}
	failing := &recordingHashStore{stored: map[chunkKey][]byte{}, err: errors.New("catalog is locked")}
	_, err = Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace, HashStore: failing}, in)
	if err == nil {
		t.Fatal("Stage succeeded although the first-write capture could not be persisted")
	}
	if !strings.Contains(err.Error(), "storing first-write chunk hash") {
		t.Errorf("error %q does not name the HashStore persistence failure", err.Error())
	}
	entries, rerr := os.ReadDir(in.WorkDir)
	if rerr != nil {
		t.Fatalf("read work dir: %v", rerr)
	}
	if len(entries) != 0 {
		t.Errorf("hash-store failure leaked scratch: %v", entries)
	}
}

// TestStageFirstWriteChunkNilHashStoreFails pins the missing-seam
// refusal: a first-write chunk without a HashStore is a wiring bug,
// refused loudly rather than dropping the capture silently.
func TestStageFirstWriteChunkNilHashStoreFails(t *testing.T) {
	root := newSourceRoot(t)
	writeSrcFile(t, root, "n.dat", []byte(strings.Repeat("N", 10)), 0o644, 0)
	meta, err := fsmeta.PathMeta(filepath.Join(root, "n.dat"))
	if err != nil {
		t.Fatalf("lstat: %v", err)
	}
	in := StageInput{
		SetID: "nil-store", ImageID: 1, SourceRoot: root,
		WorkDir: t.TempDir(), UsableSize: 10_000_000,
		Inventory: StageInventory{Chunks: []StageChunk{{
			FileID: 7, ChunkIndex: 0, Offset: 0, Length: 10, Hash: nil,
			ParentRelPath: "n.dat", ParentSize: meta.Size, ParentMtimeNS: meta.MtimeNS,
			ParentDev: int64(meta.Dev), ParentInode: int64(meta.Inode),
		}}},
	}
	_, err = Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace}, in) // no HashStore
	if err == nil {
		t.Fatal("Stage succeeded with a first-write chunk and a nil HashStore")
	}
	if !strings.Contains(err.Error(), "HashStore is nil") {
		t.Errorf("error %q does not name the missing HashStore seam", err.Error())
	}
}

// --- PATHLIST DETERMINISM -----------------------------------------------------

// TestPathListDeterministicAcrossRuns pins the xorriso-input
// determinism of the pathlist: two Stage runs over the SAME inventory
// and source produce pathlist FILES whose bytes are IDENTICAL after
// normalizing the one intentionally varying component - the unique
// MkdirTemp scratch root on the disk side. The ISO side (paths and
// sort order) is fully deterministic, which is what makes golden
// image-layout comparisons meaningful.
func TestPathListDeterministicAcrossRuns(t *testing.T) {
	fx := newMixedFixture(t)

	st1, err := stageFixture(t, fx)
	if err != nil {
		t.Fatalf("first Stage: %v", err)
	}
	defer st1.Cleanup() //nolint:errcheck
	st2, err := stageFixture(t, fx)
	if err != nil {
		t.Fatalf("second Stage: %v", err)
	}
	defer st2.Cleanup() //nolint:errcheck

	if st1.Root == st2.Root {
		t.Fatal("the two runs shared a scratch root; determinism would be vacuous")
	}
	raw1, err := os.ReadFile(st1.PathListPath)
	if err != nil {
		t.Fatalf("read pathlist 1: %v", err)
	}
	raw2, err := os.ReadFile(st2.PathListPath)
	if err != nil {
		t.Fatalf("read pathlist 2: %v", err)
	}
	norm1 := strings.ReplaceAll(string(raw1), st1.Root, "@SCRATCH@")
	norm2 := strings.ReplaceAll(string(raw2), st2.Root, "@SCRATCH@")
	if norm1 != norm2 {
		t.Errorf("pathlists differ across runs modulo the scratch root:\nrun 1:\n%s\nrun 2:\n%s", norm1, norm2)
	}

	// The in-memory entries match the file bytes exactly and are
	// sorted by ISO path (the determinism key).
	for i := 1; i < len(st1.PathList); i++ {
		if st1.PathList[i-1].ISOPath >= st1.PathList[i].ISOPath {
			t.Fatalf("pathlist not sorted by ISO path at %d: %q >= %q",
				i, st1.PathList[i-1].ISOPath, st1.PathList[i].ISOPath)
		}
	}
	if !strings.HasSuffix(norm1, "\n") {
		t.Error("pathlist does not end with a newline (xorriso line-reader friendliness)")
	}
}
