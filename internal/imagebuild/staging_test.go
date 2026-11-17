package imagebuild

// T4.2: the staging subsystem test matrix (DESIGN_SPEC 6.2 staging and
// pathlist, 6.3 step 1/5, 5.3 tree, 5.4 metadata, 3.5 point 3, D-04
// ISO family rule; IMPLEMENTATION_PLAN T4.2 acceptance).
//
// Every test drives the REAL stager against a real temp filesystem
// (t.TempDir) with only the seams injected: a fake FreeSpaceFunc, a
// fake ChunkHashStore, a wrapped SourceFS (cancellation), and the fake
// xorriso sentinel script (PATH-installed, asserted never executed -
// nothing in this package spawns processes at all). The file
// discipline, the metadata chain, the pathlist, and the workspace
// lifecycle are production code every time.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"isopress/internal/copyengine"
	"isopress/internal/fsmeta"
	"isopress/internal/media"
	"isopress/internal/report"
)

// --- fixtures ----------------------------------------------------------------

// newSourceRoot creates the fixture source root for a test.
func newSourceRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "docs"), 0o755); err != nil {
		t.Fatalf("mkdir docs: %v", err)
	}
	return root
}

// writeSrcFile writes a source file, applies the requested raw POSIX
// mode, pins its mtime, and returns the absolute path.
func writeSrcFile(t *testing.T, root, rel string, content []byte, rawMode uint32, mtimeNS int64) string {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(abs, content, 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
	if rawMode != 0o644 {
		if err := os.Chmod(abs, fsmeta.GoMode(rawMode)); err != nil {
			t.Fatalf("chmod %s: %v", rel, err)
		}
	}
	if mtimeNS != 0 {
		if _, err := fsmeta.Apply(abs, fsmeta.Meta{Mode: fsmeta.GoMode(rawMode), MtimeNS: mtimeNS}); err != nil {
			t.Fatalf("pin mtime of %s: %v", rel, err)
		}
	}
	return abs
}

// stageFileOf builds a StageFile row from the on-disk source state (the
// cli mapping's stand-in: catalog-shaped fields read via the same
// fsmeta capture the ingest uses).
func stageFileOf(t *testing.T, root, rel string, fileID int64) StageFile {
	t.Helper()
	meta, err := fsmeta.PathMeta(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("lstat %s: %v", rel, err)
	}
	content, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	h := sha256.Sum256(content)
	return StageFile{
		FileID: fileID, RelPath: rel, Size: meta.Size,
		Mode: fsmeta.UnixMode(meta.Mode), UID: meta.UID, GID: meta.GID,
		MtimeNS: meta.MtimeNS, Dev: int64(meta.Dev), Inode: int64(meta.Inode),
		Hash: h[:],
	}
}

// mixedFixture is the T4.2 acceptance fixture: whole files (one with a
// pinned setuid mode), a chunked parent with one STORED and one
// first-write chunk, valid and dangling symlinks, and an empty dir.
// The chunked parent's content is NON-UNIFORM (distinctBytes): a
// uniform "ZZZZ..." parent would mask a chunk-offset regression, because
// staging the prefix of a uniform parent produces the same bytes as
// staging the true range (see TestStageNonUniformChunkRangesPinOffsets).
type mixedFixture struct {
	root       string
	in         StageInput
	workDir    string
	hashStore  *recordingHashStore
	bigContent []byte // the exact bytes of big.dat (the chunked parent)
}

func newMixedFixture(t *testing.T) *mixedFixture {
	t.Helper()
	root := newSourceRoot(t)
	workDir := t.TempDir()

	writeSrcFile(t, root, "docs/readme.txt", []byte("hello staging\n"), 0o644, 1_700_000_000_123_456_789)
	writeSrcFile(t, root, "bin/tool", []byte("#!/bin/sh\necho tool\n"), 0o4755, 1_700_000_100_000_000_000)
	bigContent := distinctBytes(100)
	big := writeSrcFile(t, root, "big.dat", bigContent, 0o644, 0)
	if err := os.MkdirAll(filepath.Join(root, "empty-dir"), 0o755); err != nil {
		t.Fatalf("mkdir empty-dir: %v", err)
	}
	if err := os.Symlink("docs/readme.txt", filepath.Join(root, "link-ok")); err != nil {
		t.Fatalf("symlink link-ok: %v", err)
	}
	if err := os.Symlink("nowhere.txt", filepath.Join(root, "link-dangling")); err != nil {
		t.Fatalf("symlink link-dangling: %v", err)
	}

	emptyMeta, err := fsmeta.PathMeta(filepath.Join(root, "empty-dir"))
	if err != nil {
		t.Fatalf("lstat empty-dir: %v", err)
	}
	okMeta, err := fsmeta.PathMeta(filepath.Join(root, "link-ok"))
	if err != nil {
		t.Fatalf("lstat link-ok: %v", err)
	}
	danglingMeta, err := fsmeta.PathMeta(filepath.Join(root, "link-dangling"))
	if err != nil {
		t.Fatalf("lstat link-dangling: %v", err)
	}
	bigMeta, err := fsmeta.PathMeta(big)
	if err != nil {
		t.Fatalf("lstat big.dat: %v", err)
	}

	// Chunk 0 carries a STORED hash of its TRUE range (bytes [0:60));
	// chunk 1 is a first write over the non-zero range [60:100) (4.3).
	chunk0 := bigContent[:60]
	chunk0Hash := sha256.Sum256(chunk0)

	in := StageInput{
		SetID:      "fixture-20261020",
		ImageID:    1,
		SourceRoot: root,
		WorkDir:    workDir,
		UsableSize: 23_000_000_000,
		Inventory: StageInventory{
			Files: []StageFile{
				stageFileOf(t, root, "docs/readme.txt", 1),
				stageFileOf(t, root, "bin/tool", 2),
			},
			Chunks: []StageChunk{
				{
					FileID: 3, ChunkIndex: 0, Offset: 0, Length: 60,
					Hash:          chunk0Hash[:],
					ParentRelPath: "big.dat", ParentSize: bigMeta.Size, ParentMtimeNS: bigMeta.MtimeNS,
					ParentDev: int64(bigMeta.Dev), ParentInode: int64(bigMeta.Inode),
				},
				{
					FileID: 3, ChunkIndex: 1, Offset: 60, Length: 40,
					Hash:          nil, // first write: the stager captures and stores (6.3 step 1)
					ParentRelPath: "big.dat", ParentSize: bigMeta.Size, ParentMtimeNS: bigMeta.MtimeNS,
					ParentDev: int64(bigMeta.Dev), ParentInode: int64(bigMeta.Inode),
				},
			},
			Symlinks: []StageSymlink{
				{
					FileID: 4, RelPath: "link-ok", LinkTarget: "docs/readme.txt", Size: okMeta.Size,
					Mode: fsmeta.UnixMode(okMeta.Mode), UID: okMeta.UID, GID: okMeta.GID, MtimeNS: okMeta.MtimeNS,
				},
				{
					FileID: 5, RelPath: "link-dangling", LinkTarget: "nowhere.txt", Size: danglingMeta.Size,
					Mode: fsmeta.UnixMode(danglingMeta.Mode), UID: danglingMeta.UID, GID: danglingMeta.GID,
					MtimeNS: danglingMeta.MtimeNS,
				},
			},
			Dirs: []StageDir{
				{
					DirID: 1, RelPath: "empty-dir", Mode: 0o750,
					UID: emptyMeta.UID, GID: emptyMeta.GID, MtimeNS: 1_700_000_200_000_000_000,
				},
			},
		},
	}
	return &mixedFixture{
		root: root, in: in, workDir: workDir,
		hashStore:  newRecordingHashStore(),
		bigContent: bigContent,
	}
}

// recordingHashStore is the fake ChunkHashStore seam.
type recordingHashStore struct {
	stored map[chunkKey][]byte
	err    error
}

type chunkKey struct {
	fileID int64
	index  int
}

func newRecordingHashStore() *recordingHashStore {
	return &recordingHashStore{stored: map[chunkKey][]byte{}}
}

func (s *recordingHashStore) SetChunkHash(_ context.Context, fileID int64, index int, hash []byte) error {
	if s.err != nil {
		return s.err
	}
	s.stored[chunkKey{fileID, index}] = append([]byte(nil), hash...)
	return nil
}

// hugeFreeSpace is the hermetic default FreeSpaceFunc of these tests.
func hugeFreeSpace(string) (uint64, error) { return 1 << 40, nil }

// stageFixture runs Stage with the fixture's inventory and the huge
// free-space seam.
func stageFixture(t *testing.T, fx *mixedFixture) (*Staged, error) {
	t.Helper()
	return Stage(context.Background(), Deps{
		FreeSpace:   hugeFreeSpace,
		HashStore:   fx.hashStore,
		OpenPartial: nil, // production writer
	}, fx.in)
}

// --- the acceptance matrix ---------------------------------------------------

// TestStageMixedFixture verifies the full happy path: the staged TREE
// SHAPE mirrors 5.3, every staged regular file's read-back hash matches
// the source bytes, no .partial debris survives, and the pathlist
// names only real staged paths.
func TestStageMixedFixture(t *testing.T) {
	fx := newMixedFixture(t)

	st, err := stageFixture(t, fx)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	defer st.Cleanup() //nolint:errcheck

	// Expected staged shape (the 5.3 tree rooted at the scratch). The
	// empty dir is asserted separately below (it is a directory, so the
	// file-walk map cannot carry it).
	wantFiles := []string{
		"files/docs/readme.txt",
		"files/bin/tool",
		"files/link-ok",
		"files/link-dangling",
		"chunks/big.dat/00001",
		"chunks/big.dat/00002",
	}
	got := map[string]bool{}
	partials := 0
	err = filepath.WalkDir(st.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(st.Root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." || rel == PathListFileName {
			return nil
		}
		if !d.IsDir() {
			got[rel] = true
			if strings.HasSuffix(rel, ".partial") {
				partials++
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk staged root: %v", err)
	}
	for _, want := range wantFiles {
		if !got[want] {
			t.Errorf("staged tree missing %q (got %v)", want, got)
		}
	}
	if len(got) != len(wantFiles) {
		t.Errorf("staged tree has %d files, want exactly %d (%v)", len(got), len(wantFiles), got)
	}
	if partials != 0 {
		t.Errorf("%d .partial remnants survived a successful stage", partials)
	}

	// Read-back hashes of staged regular files match the SOURCE bytes.
	for mediaPath, srcRel := range map[string]string{
		"files/docs/readme.txt": "docs/readme.txt",
		"files/bin/tool":        "bin/tool",
		"chunks/big.dat/00001":  "big.dat",
		"chunks/big.dat/00002":  "big.dat",
	} {
		stagedPath := filepath.Join(st.Root, filepath.FromSlash(mediaPath))
		gotHash := hashFileOrDie(t, stagedPath)
		var want []byte
		switch mediaPath {
		// The chunked parent is NON-UNIFORM: each chunk's anchor is the
		// sha256 of its TRUE byte range, so a chunk staged with a wrong
		// offset (e.g. the parent's prefix) fails this comparison.
		case "chunks/big.dat/00001":
			want = sha256Of(fx.bigContent[0:60])
		case "chunks/big.dat/00002":
			want = sha256Of(fx.bigContent[60:100])
		default:
			want = hashFileOrDie(t, filepath.Join(fx.root, filepath.FromSlash(srcRel)))
		}
		if string(gotHash) != string(want) {
			t.Errorf("staged %s hash %x, want source hash %x", mediaPath, gotHash, want)
		}
	}

	// Symlinks are recreated as symlinks with recorded targets (D-03),
	// including the dangling one.
	for rel, target := range map[string]string{
		"link-ok":       "docs/readme.txt",
		"link-dangling": "nowhere.txt",
	} {
		gotTarget, err := os.Readlink(filepath.Join(st.Root, "files", rel))
		if err != nil {
			t.Fatalf("staged %s is not a symlink: %v", rel, err)
		}
		if gotTarget != target {
			t.Errorf("staged %s target %q, want %q", rel, gotTarget, target)
		}
	}

	// The empty dir is a directory (FR-037).
	fi, err := os.Lstat(filepath.Join(st.Root, "files", "empty-dir"))
	if err != nil || !fi.IsDir() {
		t.Fatalf("staged empty-dir: %v (dir=%v)", err, fi != nil && fi.IsDir())
	}

	// Accounting: payload bytes and entry counts feed the 6.3 step 5
	// pre-check and the T4.3 reports.
	if st.PayloadBytes != 14+20+60+40 { // readme + tool + big.dat's two chunks
		t.Errorf("PayloadBytes = %d, want %d", st.PayloadBytes, 14+20+60+40)
	}
	// 2 files + 2 chunks + 2 symlinks + 1 dir (+0: the +2 embedded
	// members live in the pre-check's arithmetic, not in EntryCount -
	// EntryCount is the STAGED payload count here; see below).
	if st.EntryCount != 7 {
		t.Errorf("EntryCount = %d, want 7 staged payload members", st.EntryCount)
	}

	// Every pathlist disk path names a REAL staged path ("staged
	// paths real"). The pathlist carries ALL 7 members including the
	// empty dir (FR-037 members are grafted too, or xorriso would not
	// master them).
	if len(st.PathList) != 7 {
		t.Errorf("pathlist has %d entries, want 7", len(st.PathList))
	}
	for _, e := range st.PathList {
		if _, err := os.Lstat(e.DiskPath); err != nil {
			t.Errorf("pathlist disk path %q does not exist: %v", e.DiskPath, err)
		}
	}
}

// TestStageMetadataFidelity pins the 5.4/FR-036 chain END TO END: the
// catalog's raw POSIX bits, mtime, uid, and gid land on the STAGED
// file (what xorriso's Rock Ridge will read), including the setuid bit
// of a raw 0o4755 through fsmeta.GoMode (the T3.4 conversion invariant
// at the imagebuild seam).
func TestStageMetadataFidelity(t *testing.T) {
	fx := newMixedFixture(t)

	st, err := stageFixture(t, fx)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	defer st.Cleanup() //nolint:errcheck

	// setuid proof: catalog 0o4755 -> GoMode -> chmod -> lstat sees
	// setuid + 0755. (The T1.2 QA finding this pins: a raw 0o4755
	// handed to os.Chmod unconverted silently drops the setuid bit.)
	toolMeta, err := fsmeta.PathMeta(filepath.Join(st.Root, "files", "bin", "tool"))
	if err != nil {
		t.Fatalf("lstat staged tool: %v", err)
	}
	if got := fsmeta.UnixMode(toolMeta.Mode); got != 0o4755 {
		t.Errorf("staged tool raw mode = %04o, want 04755 (setuid survived the POSIX->Go->chmod chain)", got)
	}

	// Ordinary file: mode + exact nanosecond mtime.
	readmeMeta, err := fsmeta.PathMeta(filepath.Join(st.Root, "files", "docs", "readme.txt"))
	if err != nil {
		t.Fatalf("lstat staged readme: %v", err)
	}
	if got := readmeMeta.Mode.Perm(); got != 0o644 {
		t.Errorf("staged readme mode = %04o, want 0644", got)
	}
	if readmeMeta.MtimeNS != 1_700_000_000_123_456_789 {
		t.Errorf("staged readme mtime = %d, want the catalog 1700000000123456789 (Rock Ridge reads the staged mtime)",
			readmeMeta.MtimeNS)
	}

	// Empty dir: the catalog mode lands (FR-037), plus its mtime.
	dirMeta, err := fsmeta.PathMeta(filepath.Join(st.Root, "files", "empty-dir"))
	if err != nil {
		t.Fatalf("lstat staged empty-dir: %v", err)
	}
	if got := dirMeta.Mode.Perm(); got != 0o750 {
		t.Errorf("staged empty-dir mode = %04o, want 0750", got)
	}
	if dirMeta.MtimeNS != 1_700_000_200_000_000_000 {
		t.Errorf("staged empty-dir mtime = %d, want 1700000200000000000", dirMeta.MtimeNS)
	}
}

// TestStageStoresFirstWriteChunkHash pins 6.3 step 1: staging IS the
// first write for an image-mode chunk - the source-stream capture is
// stored on the chunk row seam before the chunk counts as staged.
func TestStageStoresFirstWriteChunkHash(t *testing.T) {
	fx := newMixedFixture(t)

	st, err := stageFixture(t, fx)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	defer st.Cleanup() //nolint:errcheck

	// The TRUE range of the non-uniform parent ([60:100)): the capture
	// stored on the chunk row must hash the chunk's own bytes - never
	// the parent's prefix (the offset-threading regression this fixture
	// now pins).
	want := sha256Of(fx.bigContent[60:100])
	if got := fx.hashStore.stored[chunkKey{3, 1}]; string(got) != string(want) {
		t.Errorf("first-write chunk capture = %x, want %x (stored via ChunkHashStore before staging completes)", got, want)
	}
	// The stored-hash chunk is NOT re-stored (its anchor already
	// existed).
	if got, ok := fx.hashStore.stored[chunkKey{3, 0}]; ok {
		t.Errorf("stored-hash chunk was re-captured: %x", got)
	}
}

// TestStageNonUniformChunkRangesPinOffsets is the regression pin for the
// chunk-offset threading bug (the go-reviewer's B1 finding): stageChunk
// once passed a HARDCODED 0 to Source.Open instead of StageChunk.Offset,
// so every chunk with a non-zero offset staged the parent's PREFIX bytes
// instead of its true range - and for first-write chunks it PERSISTED
// sha256(prefix) to the catalog, a SELF-CONSISTENT corruption (staged
// bytes, chunk row, and the staged read-back verify all agree on the
// wrong bytes) that surfaces only at RESTORE when the reassembled file
// fails its whole-file hash. The fixture is deliberately NON-UNIFORM -
// every byte of the 24-byte parent differs, so any two 8-byte ranges
// are distinguishable - and mixes BOTH anchor classes at non-zero
// offsets (copyengine.CopyItem passes item.Offset to the same SourceFS
// seam; the two implementations must agree):
//
//	(a) every chunk's STAGED BYTES == parent[Offset:Offset+Length]
//	    (exact byte-compare against the source slice, not just a hash);
//	(b) the first-write chunk's STORED capture == sha256 of its TRUE
//	    range [8:16), never sha256 of the prefix;
//	(c) the stored-hash chunks verify against TRUE-RANGE anchors
//	    (pre-fix, chunk 2 staged the prefix and the build failed with
//	    a misleading "read-back hash mismatch" after 3 retries).
func TestStageNonUniformChunkRangesPinOffsets(t *testing.T) {
	root := newSourceRoot(t)
	parent := distinctBytes(24) // 3 x 8-byte ranges, all bytes distinct
	writeSrcFile(t, root, "pat.dat", parent, 0o644, 0)
	meta, err := fsmeta.PathMeta(filepath.Join(root, "pat.dat"))
	if err != nil {
		t.Fatalf("lstat parent: %v", err)
	}
	chunk := func(idx int, offset int64, hash []byte) StageChunk {
		return StageChunk{
			FileID: 77, ChunkIndex: idx, Offset: offset, Length: 8, Hash: hash,
			ParentRelPath: "pat.dat", ParentSize: meta.Size, ParentMtimeNS: meta.MtimeNS,
			ParentDev: int64(meta.Dev), ParentInode: int64(meta.Inode),
		}
	}
	// Chunks 0 and 2 carry STORED anchors that are the sha256 of their
	// TRUE ranges; chunk 1 (the reviewer's exact repro shape:
	// Offset=8, Length=8, Hash=nil) is a first write.
	in := StageInput{
		SetID: "offsets", ImageID: 1, SourceRoot: root,
		WorkDir: t.TempDir(), UsableSize: 10_000_000,
		Inventory: StageInventory{Chunks: []StageChunk{
			chunk(0, 0, sha256Of(parent[0:8])),
			chunk(1, 8, nil),
			chunk(2, 16, sha256Of(parent[16:24])),
		}},
	}
	store := newRecordingHashStore()
	st, err := Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace, HashStore: store}, in)
	// (c): BOTH stored-hash chunks verified against their TRUE-RANGE
	// anchors (the anchors above are true-range hashes, so a passing
	// verify pins the offset threading; pre-fix, chunk 2 failed here).
	if err != nil {
		t.Fatalf("Stage: %v (stored-hash chunks must verify against true-range anchors)", err)
	}
	defer st.Cleanup() //nolint:errcheck

	// (a): byte-compare every staged chunk against its TRUE source
	// range - exact bytes, not just a hash.
	for idx := int64(0); idx < 3; idx++ {
		mediaPath, merr := media.ChunkMediaPath("pat.dat", idx)
		if merr != nil {
			t.Fatalf("chunk media path %d: %v", idx, merr)
		}
		got, rerr := os.ReadFile(filepath.Join(st.Root, filepath.FromSlash(mediaPath)))
		if rerr != nil {
			t.Fatalf("read staged %s: %v", mediaPath, rerr)
		}
		want := parent[idx*8 : idx*8+8]
		if !bytes.Equal(got, want) {
			t.Errorf("staged %s = %q, want the TRUE range %q (parent[%d:%d])",
				mediaPath, got, want, idx*8, idx*8+8)
		}
	}

	// (b): the first-write chunk's PERSISTED capture is the sha256 of
	// the TRUE range [8:16). Pre-fix, the catalog received sha256 of
	// the prefix - self-consistent with the wrongly staged bytes and
	// detectable only at RESTORE.
	wantCapture := sha256Of(parent[8:16])
	if bytes.Equal(wantCapture, sha256Of(parent[0:8])) {
		t.Fatal("fixture is degenerate: the prefix and the true range hash identically")
	}
	if got := store.stored[chunkKey{77, 1}]; !bytes.Equal(got, wantCapture) {
		t.Errorf("first-write capture = %x, want sha256 of the TRUE range [8:16) = %x (never the parent prefix)", got, wantCapture)
	}
	// Only the first-write chunk was captured; the stored-hash chunks
	// verify against their anchors and are never re-stored.
	if _, ok := store.stored[chunkKey{77, 0}]; ok {
		t.Error("stored-hash chunk 0 was re-captured instead of verified")
	}
	if _, ok := store.stored[chunkKey{77, 2}]; ok {
		t.Error("stored-hash chunk 2 was re-captured instead of verified")
	}
}

// TestStageWorkDirLineBreakBytesRefused pins the upfront --work-dir byte
// gate (the N1 finding): the WorkDir is the DISK side of EVERY graft
// line, so a pathlist-breaking byte in it (\n, \r, NUL) would corrupt
// the pathlist wholesale. validateInput refuses it as a Usage-class
// error BEFORE MkdirTemp - the work dir stays empty, no scratch side
// effect. A '=' WorkDir is NOT refused by the upfront gate (N2: xorriso
// splits each line at the FIRST '=', and the disk side parses whole
// under that split); the conservative member/disk-side '=' refusal of
// WritePathList still applies downstream - deliberately left alone.
func TestStageWorkDirLineBreakBytesRefused(t *testing.T) {
	cases := []struct {
		name string
		b    string // the offending byte appended to the work dir name
		want string // its %q form as it appears in the message
	}{
		{"newline", "\n", `"\n"`},
		{"carriage return", "\r", `"\r"`},
		{"NUL", "\x00", `"\x00"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := newSourceRoot(t)
			parent := t.TempDir()
			workDir := filepath.Join(parent, "wd"+tc.b)
			// Linux allows '\n' and '\r' in path components but the
			// kernel rejects NUL outright (EINVAL) - for that case the
			// work dir cannot exist at all, and the Usage-class refusal
			// must still fire BEFORE MkdirTemp would have been
			// attempted (an OS error from MkdirTemp would be the bug).
			if tc.b != "\x00" {
				if err := os.MkdirAll(workDir, 0o755); err != nil {
					t.Fatalf("creating work dir: %v", err)
				}
			}
			in := simpleStageInput(t, root, workDir, "ok.txt", []byte("ok\n"), 0)

			_, err := Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace}, in)
			usageErrOf(t, err) // Usage-class flag refusal (exit 2)
			if !strings.Contains(err.Error(), "--work-dir") {
				t.Errorf("error %q does not name the flag", err.Error())
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name the offending byte %s", err.Error(), tc.want)
			}
			// No MkdirTemp side effect: for '\n'/'\r' the pre-existing
			// work dir stays empty; for NUL (which cannot exist) the
			// parent stays empty.
			if tc.b == "\x00" {
				entries, rerr := os.ReadDir(parent)
				if rerr != nil {
					t.Fatalf("read parent: %v", rerr)
				}
				if len(entries) != 0 {
					t.Errorf("byte-gate refusal leaked something under the parent: %v", entries)
				}
				return
			}
			inner, rerr := os.ReadDir(workDir)
			if rerr != nil {
				t.Fatalf("read work dir: %v", rerr)
			}
			if len(inner) != 0 {
				t.Errorf("byte-gate refusal leaked scratch (MkdirTemp side effect): %v", inner)
			}
		})
	}

	t.Run("equals stays allowed by the upfront gate", func(t *testing.T) {
		root := newSourceRoot(t)
		workDir := filepath.Join(t.TempDir(), "wd=with=equals")
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			t.Fatalf("creating work dir: %v", err)
		}
		in := simpleStageInput(t, root, workDir, "ok.txt", []byte("ok\n"), 0)

		// N2: the upfront gate must NOT refuse '=' - Stage proceeds
		// past validation (the scratch IS created). The run then
		// fails at the CONSERVATIVE disk-side '=' refusal of the
		// pathlist writer (validatePathListEntry - deliberately left
		// as-is, not extended into an upfront gate). Assert exactly
		// that split: no Usage-class flag error, and the eventual
		// refusal names the pathlist disk path.
		_, err := Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace}, in)
		if err == nil {
			t.Fatal("Stage succeeded; the conservative pathlist disk-side '=' gate is expected to refuse it today")
		}
		var ue *report.UsageError
		if errors.As(err, &ue) && strings.Contains(ue.Error(), "--work-dir") {
			t.Fatalf("upfront --work-dir gate fired on '=' (N2: '=' must stay allowed there): %v", err)
		}
		if !strings.Contains(err.Error(), "pathlist disk path") || !strings.Contains(err.Error(), "cannot express") {
			t.Errorf("error %v does not carry the conservative pathlist disk-side '=' refusal", err)
		}
		// The failed Stage removed its scratch (the no-leaked-scratch
		// contract covers post-validation failures too).
		entries, rerr := os.ReadDir(workDir)
		if rerr != nil {
			t.Fatalf("read work dir: %v", rerr)
		}
		if len(entries) != 0 {
			t.Errorf("failed '=' stage leaked scratch: %v", entries)
		}
	})
}

// TestStageExcludesHardLinkSecondaries pins the D-04 ISO rule at the
// staging boundary: the ISO tree contains the family PRIMARY only.
// catalog.CopyInventory derives the primary in .Files and the members
// in .Secondaries (the T3.4 unified family rule); the ISO-mode mapping
// (T4.4) drops .Secondaries, and this test pins what that mapping
// produces here: primary staged, secondary ABSENT.
func TestStageExcludesHardLinkSecondaries(t *testing.T) {
	root := newSourceRoot(t)
	primary := writeSrcFile(t, root, "family/a.txt", []byte("family content\n"), 0o644, 0)
	secondary := filepath.Join(root, "family", "b.txt")
	if err := os.Link(primary, secondary); err != nil {
		t.Fatalf("creating hard link: %v", err)
	}

	// The ISO-mode inventory: the primary's row, no secondaries field
	// exists to carry the member (see StageInventory's doc).
	in := StageInput{
		SetID: "family-set", ImageID: 1, SourceRoot: root,
		UsableSize: 10_000_000,
		Inventory:  StageInventory{Files: []StageFile{stageFileOf(t, root, "family/a.txt", 10)}},
	}

	st, err := Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace}, in)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	defer st.Cleanup() //nolint:errcheck

	if _, err := os.Lstat(filepath.Join(st.Root, "files", "family", "a.txt")); err != nil {
		t.Errorf("family primary not staged: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(st.Root, "files", "family", "b.txt")); !os.IsNotExist(err) {
		t.Errorf("hard-link secondary staged (err=%v); D-04: the ISO contains only the primary name's file", err)
	}
	for _, e := range st.PathList {
		if strings.Contains(e.ISOPath, "b.txt") {
			t.Errorf("secondary leaked into the pathlist: %s", e.ISOPath)
		}
	}
}

// TestStageHostileRelPathRefused pins the carried UTF-8 gate (media
// ValidateRelPath via the media path builders), the symlink-target
// gate, and the graft gate - all BEFORE any filesystem effect: the
// work dir stays empty (no scratch was ever created).
func TestStageHostileRelPathRefused(t *testing.T) {
	cases := []struct {
		name    string
		inv     StageInventory
		wantIs  error // sentinel the error must wrap (nil = any refusal)
		wantMsg string
	}{
		{
			name:    "dotdot relpath",
			inv:     StageInventory{Files: []StageFile{{FileID: 1, RelPath: "../evil", Size: 1, Hash: make([]byte, 32)}}},
			wantIs:  media.ErrUnsafeRelPath,
			wantMsg: `"../evil"`,
		},
		{
			name:    "absolute relpath",
			inv:     StageInventory{Files: []StageFile{{FileID: 1, RelPath: "/etc/passwd", Size: 1, Hash: make([]byte, 32)}}},
			wantIs:  media.ErrUnsafeRelPath,
			wantMsg: "absolute",
		},
		{
			name:    "non-utf8 relpath (the JSON-manifest gate)",
			inv:     StageInventory{Files: []StageFile{{FileID: 1, RelPath: "caf\xe9", Size: 1, Hash: make([]byte, 32)}}},
			wantIs:  media.ErrUnsafeRelPath,
			wantMsg: "not valid UTF-8",
		},
		{
			name:    "newline relpath (graft gate)",
			inv:     StageInventory{Files: []StageFile{{FileID: 1, RelPath: "a\nb", Size: 1, Hash: make([]byte, 32)}}},
			wantIs:  ErrUngraftablePath,
			wantMsg: "graft-points",
		},
		{
			name: "equals relpath (graft gate)",
			inv: StageInventory{Files: []StageFile{
				{FileID: 1, RelPath: "a=b", Size: 1, Hash: make([]byte, 32)},
			}},
			wantIs:  ErrUngraftablePath,
			wantMsg: "graft-points",
		},
		{
			name: "non-utf8 symlink target",
			inv: StageInventory{Symlinks: []StageSymlink{
				{FileID: 1, RelPath: "link", LinkTarget: "t\xe9", Size: 3},
			}},
			wantIs:  ErrUnsafeSymlinkTarget,
			wantMsg: "link",
		},
		{
			name: "missing whole-file hash (corrupt catalog)",
			inv: StageInventory{Files: []StageFile{
				{FileID: 1, RelPath: "x", Size: 1, Hash: nil},
			}},
			wantMsg: "hash",
		},
		{
			name: "duplicate media path",
			inv: StageInventory{Files: []StageFile{
				{FileID: 1, RelPath: "x", Size: 1, Hash: make([]byte, 32)},
				{FileID: 2, RelPath: "x", Size: 1, Hash: make([]byte, 32)},
			}},
			wantMsg: "claimed by both",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := newSourceRoot(t)
			workDir := t.TempDir()
			in := StageInput{
				SetID: "hostile", ImageID: 1, SourceRoot: root,
				WorkDir: workDir, UsableSize: 1_000_000_000, Inventory: tc.inv,
			}
			_, err := Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace}, in)
			if err == nil {
				t.Fatal("Stage succeeded, want refusal")
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Errorf("error %v does not wrap %v", err, tc.wantIs)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantMsg)
			}
			// No scratch: validation runs before MkdirTemp.
			entries, rerr := os.ReadDir(workDir)
			if rerr != nil {
				t.Fatalf("read work dir: %v", rerr)
			}
			if len(entries) != 0 {
				t.Errorf("hostile refusal leaked scratch: %v", entries)
			}
		})
	}
}

// TestStageSourceChangedFailsBuild pins DESIGN_SPEC 3.5 point 3 (and
// the IMPLEMENTATION_PLAN T4.2 acceptance): a source that changed since
// the scan FAILS that image build with a clear message - no bytes are
// mastered under a stale record - and the scratch is cleaned.
func TestStageSourceChangedFailsBuild(t *testing.T) {
	fx := newMixedFixture(t)

	// Mutate a whole file after the "scan" (the inventory predates it).
	target := filepath.Join(fx.root, "docs", "readme.txt")
	if err := os.WriteFile(target, []byte("MUTATED after scan\n"), 0o644); err != nil {
		t.Fatalf("mutating source: %v", err)
	}

	_, err := stageFixture(t, fx)
	if err == nil {
		t.Fatal("Stage succeeded, want the 3.5 point 3 build failure")
	}
	var sce *SourceChangedError
	if !errors.As(err, &sce) {
		t.Fatalf("error %v is not a *SourceChangedError", err)
	}
	if sce.RelPath != "docs/readme.txt" {
		t.Errorf("SourceChangedError.RelPath = %q, want docs/readme.txt", sce.RelPath)
	}
	if !strings.Contains(err.Error(), "stale bytes") {
		t.Errorf("message %q does not carry the 3.5 point 3 rationale", err.Error())
	}
	entries, rerr := os.ReadDir(fx.workDir)
	if rerr != nil {
		t.Fatalf("read work dir: %v", rerr)
	}
	if len(entries) != 0 {
		t.Errorf("failed build leaked scratch: %v", entries)
	}
}

// TestStageSourceChangedChunkParent pins the same rule for a chunk's
// PARENT file.
func TestStageSourceChangedChunkParent(t *testing.T) {
	fx := newMixedFixture(t)

	big := filepath.Join(fx.root, "big.dat")
	if err := os.Truncate(big, 101); err != nil { // size change: the D-10 size field
		t.Fatalf("mutating parent: %v", err)
	}

	_, err := stageFixture(t, fx)
	var sce *SourceChangedError
	if !errors.As(err, &sce) {
		t.Fatalf("error %v is not a *SourceChangedError", err)
	}
	if sce.RelPath != "big.dat" {
		t.Errorf("SourceChangedError.RelPath = %q, want big.dat", sce.RelPath)
	}
}

// TestStageCtxCancellationCleansScratch cancels the context INSIDE the
// first file's source open (a wrapped SourceFS) and pins the lifecycle
// contract: the stage fails with the context error and NO scratch
// survives.
func TestStageCtxCancellationCleansScratch(t *testing.T) {
	fx := newMixedFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	deps := Deps{
		FreeSpace: hugeFreeSpace,
		HashStore: fx.hashStore,
		Source:    cancelOnOpenSource{inner: copyengine.OSSource{}, cancel: cancel},
	}

	_, err := Stage(ctx, deps, fx.in)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	entries, rerr := os.ReadDir(fx.workDir)
	if rerr != nil {
		t.Fatalf("read work dir: %v", rerr)
	}
	if len(entries) != 0 {
		t.Errorf("cancelled stage leaked scratch: %v", entries)
	}
}

// cancelOnOpenSource cancels the run context the first time a source
// file is opened (mid-item, like a SIGINT arriving mid-stage).
type cancelOnOpenSource struct {
	inner  copyengine.SourceFS
	cancel context.CancelFunc
}

func (s cancelOnOpenSource) Lstat(path string) (fsmeta.Meta, error) {
	return s.inner.Lstat(path)
}

func (s cancelOnOpenSource) Open(path string, offset, length int64) (io.ReadCloser, error) {
	s.cancel()
	return s.inner.Open(path, offset, length)
}

// TestStageChownEPERMIsWarning pins the 5.4 downgrade at the staging
// seam: as non-root, chown to a foreign uid is a WARNING (collected,
// run continues), not a failure - metadata application otherwise fails
// the item hard (metadata is payload for Rock Ridge).
func TestStageChownEPERMIsWarning(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("EPERM chown cannot be provoked as root")
	}
	root := newSourceRoot(t)
	writeSrcFile(t, root, "owned.txt", []byte("x\n"), 0o644, 0)
	row := stageFileOf(t, root, "owned.txt", 1)
	row.UID = 65534 // nobody: a uid this process may not chown to
	row.GID = 65534

	in := StageInput{
		SetID: "ep", ImageID: 1, SourceRoot: root,
		WorkDir: t.TempDir(), UsableSize: 10_000_000,
		Inventory: StageInventory{Files: []StageFile{row}},
	}
	st, err := Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace}, in)
	if err != nil {
		t.Fatalf("Stage: %v (the EPERM downgrade must not fail the item)", err)
	}
	defer st.Cleanup() //nolint:errcheck

	if len(st.Warnings) != 1 || !strings.Contains(st.Warnings[0], "EPERM") {
		t.Errorf("Warnings = %v, want exactly the chown EPERM downgrade", st.Warnings)
	}
	// The OTHER metadata still landed: the mode was applied.
	meta, err := fsmeta.PathMeta(filepath.Join(st.Root, "files", "owned.txt"))
	if err != nil {
		t.Fatalf("lstat staged owned.txt: %v", err)
	}
	if meta.Mode.Perm() != 0o644 {
		t.Errorf("staged mode = %04o, want 0644 (chmod must run after the downgraded chown)", meta.Mode.Perm())
	}
}

// TestStageWorkDirIsParent pins the 6.2 workspace placement: the
// scratch lives under --work-dir (StageInput.WorkDir), and the staged
// tree is rooted at the scratch itself so the 6.2 graft lines read
// "/files/x=<workdir>/isopress-imagebuild-*/files/x".
func TestStageWorkDirIsParent(t *testing.T) {
	fx := newMixedFixture(t)

	st, err := stageFixture(t, fx)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	defer st.Cleanup() //nolint:errcheck

	if !strings.HasPrefix(st.Root, fx.workDir) {
		t.Errorf("staging root %q is not under --work-dir %q", st.Root, fx.workDir)
	}
	if filepath.Base(st.Root) == "" || !strings.HasPrefix(filepath.Base(st.Root), stagingDirPrefix) {
		t.Errorf("staging root %q does not carry the %q prefix", st.Root, stagingDirPrefix)
	}
	// Unique-per-call by construction: a second Stage gets its own
	// scratch, and Cleanup of one does not touch the other.
	st2, err := stageFixture(t, fx)
	if err != nil {
		t.Fatalf("second Stage: %v", err)
	}
	if st2.Root == st.Root {
		t.Errorf("two concurrent Stages share scratch %q; MkdirTemp must be unique-per-call", st.Root)
	}
	if err := st.Cleanup(); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if _, err := os.Lstat(st2.Root); err != nil {
		t.Errorf("cleaning the first scratch removed the second: %v", err)
	}
	st2.Cleanup() //nolint:errcheck
}

// --- helpers -----------------------------------------------------------------

// distinctBytes returns an n-byte pattern in which EVERY byte differs
// from every other (byte(i)+1, all-distinct for n <= 255): any two
// equal-length ranges of a multi-chunk parent then have different
// content, so a chunk staged at the WRONG offset (e.g. the parent's
// prefix instead of its true range) is caught by an exact byte/hash
// comparison. A uniform "ZZZZ..." parent masks that regression.
func distinctBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i) + 1
	}
	return b
}

func sha256Of(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

func hashFileOrDie(t *testing.T, path string) []byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close() //nolint:errcheck
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return h.Sum(nil)
}

// usageErrOf extracts the UsageError class from a Stage failure (the
// pre-check classification both gates share).
func usageErrOf(t *testing.T, err error) *report.UsageError {
	t.Helper()
	var ue *report.UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("error %v is not a *report.UsageError (exit-2 plan class)", err)
	}
	return ue
}
