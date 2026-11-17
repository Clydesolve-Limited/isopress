package imagebuild

// T4.3: the build phase unit tests (the fake tier - no xorriso, no
// catalog; DESIGN_SPEC 12.1). The seams carry their weight here:
// Master, Extractor, and BuildStore are fakes that record every call,
// so the ORDER of operations, the exact 6.6 content list, the F2 and
// F4 gates, the --clean-failed policy, and the verification-comparison
// logic are all pinned deterministically. The gated REAL-xorriso
// integration lives in build_real_test.go; the catalog-side
// transaction proofs (trigger injection) live in
// internal/catalog/buildstore_test.go.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"isopress/internal/fsmeta"
	"isopress/internal/media"
	"isopress/internal/progress"
	"isopress/internal/report"
	"isopress/internal/verify"
)

// --- fakes --------------------------------------------------------------------

// fakeChunkKey identifies one chunk in the fake store.
type fakeChunkKey struct {
	fileID int64
	index  int
}

// fakeStore records every BuildStore call (the transaction ORDER and
// the exact content list are the contracts under test). It doubles as
// the ChunkHashStore of staging: SetChunkHash feeds ChunkHashes, the
// real catalog's shape (staging stores the 4.3 capture; the builder
// reads it back after Stage).
type fakeStore struct {
	mu     sync.Mutex
	stored map[fakeChunkKey][]byte
	begins [][]ContentRow
	fails  []failRec
	ends   []finishRec
	events *[]string // the shared order log (nil-safe)

	beginHook  func()
	beginErr   error
	markErr    error
	finishErr  error
	markCtxErr error // ctx.Err() observed at the last MarkFailed call

	// chunkReadMutate, when set, transforms the hashes ChunkHashes
	// returns (the snapshot-vs-stored cross-check injection: staging
	// overwrites the stored capture, so the mismatch must be injected
	// AT READ TIME to reach fetchChunkHashes' comparison).
	chunkReadMutate func([][]byte) [][]byte
}

type failRec struct{ detail string }
type finishRec struct {
	finalPath    string
	builtAt      int64
	bytesWritten int64
}

func (s *fakeStore) log(ev string) {
	if s.events != nil {
		s.mu.Lock()
		*s.events = append(*s.events, ev)
		s.mu.Unlock()
	}
}

func (s *fakeStore) SetChunkHash(_ context.Context, fileID int64, index int, hash []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stored == nil {
		s.stored = map[fakeChunkKey][]byte{}
	}
	s.stored[fakeChunkKey{fileID, index}] = append([]byte(nil), hash...)
	return nil
}

func (s *fakeStore) ChunkHashes(_ context.Context, fileIDs []int64, indexes []int) ([][]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, len(fileIDs))
	for i := range fileIDs {
		out[i] = append([]byte(nil), s.stored[fakeChunkKey{fileIDs[i], indexes[i]}]...)
	}
	if s.chunkReadMutate != nil {
		out = s.chunkReadMutate(out)
	}
	return out, nil
}

func (s *fakeStore) BeginBuild(_ context.Context, rows []ContentRow) error {
	s.log("begin")
	s.mu.Lock()
	s.begins = append(s.begins, rows)
	hook := s.beginHook
	err := s.beginErr
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return err
}

func (s *fakeStore) MarkFailed(ctx context.Context, detail string) error {
	s.log("failed")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markCtxErr = ctx.Err()
	s.fails = append(s.fails, failRec{detail: detail})
	return s.markErr
}

func (s *fakeStore) FinishBuild(_ context.Context, finalPath string, builtAt, bytesWritten int64) error {
	s.log("finish")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ends = append(s.ends, finishRec{finalPath: finalPath, builtAt: builtAt, bytesWritten: bytesWritten})
	return s.finishErr
}

// fakeMaster records every invocation's argv and the pathlist content
// ON DISK at master time (the step-4 contract: the FINAL pathlist
// carries the two image-root graft entries), then writes the -o path
// like a (very) small xorriso.
type fakeMaster struct {
	mu        sync.Mutex
	argvs     [][]string
	pathlists []string
	events    *[]string
	run       func(ctx context.Context, args []string) ([]byte, error)
}

func (m *fakeMaster) Run(ctx context.Context, args []string) ([]byte, error) {
	if m.events != nil {
		m.mu.Lock()
		*m.events = append(*m.events, "master")
		m.mu.Unlock()
	}
	plist := ""
	if i := indexOf(args, "-path-list"); i >= 0 && i+1 < len(args) {
		if b, err := os.ReadFile(args[i+1]); err == nil {
			plist = string(b)
		}
	}
	m.mu.Lock()
	m.argvs = append(m.argvs, append([]string(nil), args...))
	m.pathlists = append(m.pathlists, plist)
	m.mu.Unlock()
	if m.run != nil {
		return m.run(ctx, args)
	}
	return defaultMasterRun(args)
}

// defaultMasterRun is the happy-path fake: write the -o file and exit 0.
func defaultMasterRun(args []string) ([]byte, error) {
	i := indexOf(args, "-o")
	if i < 0 || i+1 >= len(args) {
		return nil, errors.New("fake xorriso: no -o argument")
	}
	if err := os.WriteFile(args[i+1], []byte("FAKE-ISO-IMAGE"), 0o644); err != nil {
		return nil, err
	}
	return []byte("xorriso: mastering done\n"), nil
}

// fakeExtractor returns a canned ExtractResult (or error). It also
// records the Bus and Totals the builder passed, so the four-phase
// progress wiring (the exact content-list totals reaching the
// primitive, 6.2) is pinned from the seam side.
type fakeExtractor struct {
	mu         sync.Mutex
	calls      int
	result     verify.ExtractResult
	err        error
	events     *[]string
	lastBus    *progress.Bus
	lastTotals verify.Totals
}

func (x *fakeExtractor) ExtractAndHash(_ context.Context, _, _ string, bus *progress.Bus, totals verify.Totals) (verify.ExtractResult, error) {
	x.mu.Lock()
	x.calls++
	x.lastBus, x.lastTotals = bus, totals
	x.mu.Unlock()
	if x.events != nil {
		*x.events = append(*x.events, "extract")
	}
	return x.result, x.err
}

func indexOf(args []string, want string) int {
	for i, a := range args {
		if a == want {
			return i
		}
	}
	return -1
}

// --- fixture ------------------------------------------------------------------

type buildFixture struct {
	in       BuildInput
	store    *fakeStore
	master   *fakeMaster
	extract  *fakeExtractor
	events   []string
	chunkH0  []byte
	chunkH1  []byte
	manifest []byte // the expected MANIFEST.json bytes (derived via manifestInputFor)
}

// newBuildFixture builds a source tree (a regular file, a symlink, an
// empty dir, a chunked parent staged as two first-write chunks) plus
// the fakes. The chunk hashes flow the REAL way: Stage captures them
// (first write, 4.3) into the store, and fetchChunkHashes reads them
// back.
func newBuildFixture(t *testing.T) *buildFixture {
	t.Helper()
	srcRoot := t.TempDir()
	work := t.TempDir()
	dest := filepath.Join(t.TempDir(), "images")

	content := []byte("hello, iso!") // 11 bytes: chunk0 = 5, chunk1 = 6
	writeFile(t, filepath.Join(srcRoot, "a.txt"), content, 0o644)
	if err := os.Symlink("a.txt", filepath.Join(srcRoot, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(srcRoot, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(srcRoot, "big.bin"), content, 0o644)

	fileHash := sha256.Sum256(content)
	chunk0 := sha256.Sum256(content[:5])
	chunk1 := sha256.Sum256(content[5:])

	exportBytes := []byte("fake catalog export bytes")
	exportPath := filepath.Join(t.TempDir(), media.ExportFileName)
	writeFile(t, exportPath, exportBytes, 0o644)
	exportHash := sha256.Sum256(exportBytes)

	f := &buildFixture{}
	f.store = &fakeStore{}
	f.store.events = &f.events
	f.master = &fakeMaster{events: &f.events}
	f.extract = &fakeExtractor{events: &f.events}
	f.chunkH0, f.chunkH1 = chunk0[:], chunk1[:]

	// The 3.5 point 3 comparison runs against the REAL source stat, so
	// the inventory carries the true dev/inode/mtime the fixture files
	// got on this filesystem.
	fileMeta, err := fsmeta.PathMeta(filepath.Join(srcRoot, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	bigMeta, err := fsmeta.PathMeta(filepath.Join(srcRoot, "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	linkMeta, err := fsmeta.PathMeta(filepath.Join(srcRoot, "link"))
	if err != nil {
		t.Fatal(err)
	}

	c0 := StageChunk{FileID: 2, ChunkIndex: 0, Offset: 0, Length: 5}
	c1 := StageChunk{FileID: 2, ChunkIndex: 1, Offset: 5, Length: 6}
	for _, c := range []*StageChunk{&c0, &c1} {
		c.ParentRelPath = "big.bin"
		c.ParentSize = bigMeta.Size
		c.ParentMode = 0o644
		c.ParentMtimeNS = bigMeta.MtimeNS
		c.ParentDev = int64(bigMeta.Dev)
		c.ParentInode = int64(bigMeta.Inode)
	}

	f.in = BuildInput{
		SetID:            "S-BUILD",
		ImageID:          1,
		SourceRoot:       srcRoot,
		WorkDir:          work,
		DestDir:          dest,
		UsableSize:       10 << 20,
		VerifyAfterBuild: true,
		Inventory: StageInventory{
			Files: []StageFile{{
				FileID: 1, RelPath: "a.txt", Size: fileMeta.Size,
				Mode: 0o644, MtimeNS: fileMeta.MtimeNS,
				Dev: int64(fileMeta.Dev), Inode: int64(fileMeta.Inode), Hash: fileHash[:],
			}},
			Chunks:   []StageChunk{c0, c1},
			Symlinks: []StageSymlink{{FileID: 3, RelPath: "link", LinkTarget: "a.txt", Size: linkMeta.Size, Mode: 0o777, MtimeNS: linkMeta.MtimeNS}},
			Dirs:     []StageDir{{DirID: 1, RelPath: "empty", Mode: 0o755, MtimeNS: 4000}},
		},
		Export: EmbeddedExport{Path: exportPath, Size: int64(len(exportBytes)), Hash: exportHash[:]},
		Manifest: ManifestSeed{
			ToolVersion:   "test",
			SchemaVersion: 1,
			CreatedAt:     time.Unix(1700000000, 0).UTC(),
			ChunkedFiles: []media.ChunkedFileInput{{
				RelPath: "big.bin", WholeSize: bigMeta.Size, WholeSHA256: fileHash[:],
			}},
			Placement: []media.PlacementInput{{
				ImageNumber: 1, Paths: []string{"a.txt", "big.bin", "empty", "link"},
			}},
		},
		Store: f.store,
	}

	// The expected manifest bytes, derived through the SAME pure
	// assembly the builder uses (manifestInputFor) - pinning the
	// determinism contract end to end.
	mi := manifestInputFor(f.in, [][]byte{chunk0[:], chunk1[:]})
	m, err := media.GenerateManifest(mi)
	if err != nil {
		t.Fatalf("generating the expected manifest: %v", err)
	}
	f.manifest, err = media.MarshalManifest(m)
	if err != nil {
		t.Fatalf("marshaling the expected manifest: %v", err)
	}
	return f
}

// deps assembles the Deps over the fixture's fakes.
func (f *buildFixture) deps() Deps {
	return Deps{
		Master:    f.master,
		Extractor: f.extract,
		HashStore: f.store,
	}
}

// refreshManifest recomputes the expected manifest bytes after a
// fixture mutation that changes the manifest content (a new SetID, for
// example), so the canned extraction keeps matching.
func (f *buildFixture) refreshManifest(t *testing.T) {
	t.Helper()
	mi := manifestInputFor(f.in, [][]byte{f.chunkH0, f.chunkH1})
	m, err := media.GenerateManifest(mi)
	if err != nil {
		t.Fatalf("regenerating the expected manifest: %v", err)
	}
	f.manifest, err = media.MarshalManifest(m)
	if err != nil {
		t.Fatalf("re-marshaling the expected manifest: %v", err)
	}
}

// happyEntries is the extraction result matching a successful master of
// the fixture: every content-list member plus the implied parent dirs
// of the 5.3 tree (which the comparison must tolerate).
func (f *buildFixture) happyEntries() verify.ExtractResult {
	manifestSum := sha256.Sum256(f.manifest)
	exportSum := sha256.Sum256([]byte("fake catalog export bytes"))
	return verify.ExtractResult{Entries: []verify.Entry{
		{MediaPath: "files", Kind: verify.KindDir},
		{MediaPath: "files/a.txt", Kind: verify.KindFile, Size: 11, Hash: []byte(sha256Of([]byte("hello, iso!")))},
		{MediaPath: "files/link", Kind: verify.KindSymlink, Size: 5, LinkTarget: "a.txt"},
		{MediaPath: "files/empty", Kind: verify.KindDir},
		{MediaPath: "chunks", Kind: verify.KindDir},
		{MediaPath: "chunks/big.bin", Kind: verify.KindDir},
		{MediaPath: "chunks/big.bin/00001", Kind: verify.KindFile, Size: 5, Hash: f.chunkH0},
		{MediaPath: "chunks/big.bin/00002", Kind: verify.KindFile, Size: 6, Hash: f.chunkH1},
		{MediaPath: media.ManifestFileName, Kind: verify.KindFile, Size: int64(len(f.manifest)), Hash: manifestSum[:]},
		{MediaPath: media.ExportFileName, Kind: verify.KindFile, Size: int64(len("fake catalog export bytes")), Hash: exportSum[:]},
	}}
}

// writeFile is the fixture file writer (MkdirAll + WriteFile).
func writeFile(t *testing.T, path string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatal(err)
	}
}

// --- the happy path -----------------------------------------------------------

// TestBuildFullPipelineSuccess pins the whole 6.3 flow against the
// fakes: the ORDER (content transaction -> master -> extract -> finish),
// the exact argv of the T4.1 template, the FINAL pathlist (payload +
// the two image-root grafts), the exact 6.6 content list, and the
// atomic finalize (final name, size, no .partial, scratch removed).
func TestBuildFullPipelineSuccess(t *testing.T) {
	f := newBuildFixture(t)
	f.extract.result = f.happyEntries()
	ctx := context.Background()

	res, err := Build(ctx, f.deps(), f.in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// THE ORDER: the 6.6 transaction precedes the exec, the
	// self-verify sits between master and finish.
	wantEvents := []string{"begin", "master", "extract", "finish"}
	if strings.Join(f.events, ",") != strings.Join(wantEvents, ",") {
		t.Errorf("operation order = %v, want %v", f.events, wantEvents)
	}

	// The argv is the exact 6.2 template through the T4.1 Invocation
	// (Joliet on, .partial output, the staging pathlist).
	finalISO := filepath.Join(f.in.DestDir, "S-BUILD-001.iso")
	f.master.mu.Lock()
	argv := f.master.argvs[0]
	plist := f.master.pathlists[0]
	f.master.mu.Unlock()
	checks := []struct{ flag, want string }{
		{"-as", "mkisofs"},
		{"-iso-level", "3"},
		{"-V", "S-BUILD-001"}, // 2026-11-04 amendment: <SET-ID>-<NNN>
		{"-o", finalISO + ".partial"},
		{"-path-list", ""}, // the pathlist path is asserted by prefix below
	}
	for _, c := range checks {
		i := indexOf(argv, c.flag)
		if i < 0 || i+1 >= len(argv) {
			t.Fatalf("argv %v lacks flag %q", argv, c.flag)
		}
		if c.want != "" && argv[i+1] != c.want {
			t.Errorf("argv %q = %q, want %q (argv: %v)", c.flag, argv[i+1], c.want, argv)
		}
	}
	if !strings.HasSuffix(argv[indexOf(argv, "-path-list")+1], PathListFileName) {
		t.Errorf("path-list arg %q does not name the staging pathlist", argv[indexOf(argv, "-path-list")+1])
	}
	if indexOf(argv, "-J") < 0 {
		t.Errorf("argv %v lacks -J (Joliet, 6.2)", argv)
	}
	// The FINAL pathlist on disk at master time carries the payload
	// grafts plus the two image-root members (the step-4 contract).
	for _, want := range []string{
		"/files/a.txt=", "/files/link=", "/files/empty=",
		"/chunks/big.bin/00001=", "/chunks/big.bin/00002=",
		"/MANIFEST.json=", "/catalog-export.sqlite=",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("final pathlist lacks %q:\n%s", want, plist)
		}
	}

	// THE 6.6 CONTENT LIST: payload rows + the manifest/export
	// self-rows, in one BeginBuild batch.
	if len(f.store.begins) != 1 {
		t.Fatalf("BeginBuild calls = %d, want 1", len(f.store.begins))
	}
	manifestSum := sha256.Sum256(f.manifest)
	exportSum := sha256.Sum256([]byte("fake catalog export bytes"))
	got := describeRows(f.store.begins[0])
	want := strings.Join([]string{
		"MANIFEST.json manifest -:- " + fmt.Sprintf("%d %x", len(f.manifest), manifestSum),
		"catalog-export.sqlite export -:- " + fmt.Sprintf("%d %x", len("fake catalog export bytes"), exportSum),
		"chunks/big.bin/00001 chunk 2:0 5 " + fmt.Sprintf("%x", f.chunkH0),
		"chunks/big.bin/00002 chunk 2:1 6 " + fmt.Sprintf("%x", f.chunkH1),
		"files/a.txt file 1:- 11 " + fmt.Sprintf("%x", sha256Of([]byte("hello, iso!"))),
		"files/empty dir -:- 0 -",
		"files/link symlink 3:- 5 -",
	}, "\n")
	if got != want {
		t.Errorf("content list = \n%s\nwant \n%s", got, want)
	}

	// The atomic finalize: the final .iso exists at the 6.4 name, no
	// .partial remains, and FinishBuild stamped size and path.
	if res.ISOPath != finalISO {
		t.Errorf("BuildResult.ISOPath = %q, want %q", res.ISOPath, finalISO)
	}
	if _, err := os.Stat(finalISO); err != nil {
		t.Fatalf("final .iso missing: %v", err)
	}
	if _, err := os.Stat(finalISO + ".partial"); !os.IsNotExist(err) {
		t.Errorf(".partial still present after the rename: %v", err)
	}
	if len(f.store.ends) != 1 || f.store.ends[0].finalPath != finalISO {
		t.Fatalf("FinishBuild records = %+v, want one for %s", f.store.ends, finalISO)
	}
	if res.BytesWritten != int64(len("FAKE-ISO-IMAGE")) || f.store.ends[0].bytesWritten != res.BytesWritten {
		t.Errorf("BytesWritten = %d (finish %d), want %d", res.BytesWritten, f.store.ends[0].bytesWritten, len("FAKE-ISO-IMAGE"))
	}
	if res.ContentEntries != 7 || res.VerifiedEntries != 7 || res.VerifySkipped {
		t.Errorf("result = (content %d, verified %d, skipped %v), want (7, 7, false)",
			res.ContentEntries, res.VerifiedEntries, res.VerifySkipped)
	}
	if len(f.store.fails) != 0 {
		t.Errorf("MarkFailed was called on success: %+v", f.store.fails)
	}

	// The staging scratch is removed (the T4.2 caller contract).
	entries, err := os.ReadDir(f.in.WorkDir)
	if err != nil {
		t.Fatal(err)
	}
	// WorkDir holds only the dest dir's parent? No - dest is OUTSIDE
	// work; the manifest scratch and staging scratch are both gone.
	for _, e := range entries {
		t.Errorf("work dir still holds %q after Build", e.Name())
	}
}

// describeRows renders content rows deterministically for comparisons.
func describeRows(rows []ContentRow) string {
	sorted := append([]ContentRow(nil), rows...)
	// insertion sort by media path (small n, no extra imports)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j].MediaPath < sorted[j-1].MediaPath; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	parts := make([]string, len(sorted))
	for i, r := range sorted {
		file, index := "-", "-"
		if r.FileID != 0 {
			file = fmt.Sprint(r.FileID)
		}
		if r.Kind == media.KindChunk {
			index = fmt.Sprint(r.ChunkIndex)
		}
		hash := "-"
		if r.Hash != nil {
			hash = fmt.Sprintf("%x", r.Hash)
		}
		parts[i] = fmt.Sprintf("%s %s %s:%s %d %s", r.MediaPath, r.Kind, file, index, r.Size, hash)
	}
	return strings.Join(parts, "\n")
}

// --- the F2 gate and volume id (6.4) ------------------------------------------

// TestBuildF2SetIDGates: '\n'/'\r' in the set id is a Usage-class
// refusal BEFORE any store or exec call (the F2 carry-in); '=' is
// ALLOWED and flows into -V; a >32-byte id shortens deterministically
// at the ISO 9660 volume-id limit while keeping the "-001" suffix
// (the 2026-11-04 <SET-ID>-<NNN> amendment, number-preserving).
func TestBuildF2SetIDGates(t *testing.T) {
	t.Run("newline refused Usage-class", func(t *testing.T) {
		f := newBuildFixture(t)
		f.in.SetID = "S\nEVIL"
		_, err := Build(context.Background(), f.deps(), f.in)
		var usage *report.UsageError
		if !errors.As(err, &usage) {
			t.Fatalf("err = %v (%T), want a *report.UsageError", err, err)
		}
		if len(f.store.begins) != 0 || len(f.master.argvs) != 0 {
			t.Fatal("the F2 refusal must run before any store or exec call")
		}
	})
	t.Run("carriage return refused", func(t *testing.T) {
		f := newBuildFixture(t)
		f.in.SetID = "S\rEVIL"
		_, err := Build(context.Background(), f.deps(), f.in)
		var usage *report.UsageError
		if !errors.As(err, &usage) {
			t.Fatalf("err = %v (%T), want a *report.UsageError", err, err)
		}
	})
	t.Run("equals allowed and flows into -V", func(t *testing.T) {
		f := newBuildFixture(t)
		f.in.SetID = "S=E=X"
		f.refreshManifest(t)
		f.extract.result = f.happyEntries()
		if _, err := Build(context.Background(), f.deps(), f.in); err != nil {
			t.Fatalf("Build with '=' set id: %v", err)
		}
		argv := f.master.argvs[0]
		i := indexOf(argv, "-V")
		// 2026-11-04 amendment: the '=' set id verbatim, THEN the
		// image number suffix (the fixture builds image 1).
		if i < 0 || argv[i+1] != "S=E=X-001" {
			t.Errorf("-V arg = %q, want the '=' set id verbatim plus -001 (argv %v)", argv[i+1], argv)
		}
	})
	t.Run("truncation at 32 bytes keeps the suffix", func(t *testing.T) {
		f := newBuildFixture(t)
		f.in.SetID = strings.Repeat("L", 40)
		f.refreshManifest(t)
		f.extract.result = f.happyEntries()
		if _, err := Build(context.Background(), f.deps(), f.in); err != nil {
			t.Fatalf("Build with a long set id: %v", err)
		}
		argv := f.master.argvs[0]
		i := indexOf(argv, "-V")
		// 40 set-id bytes + "-001" overflow the 32-byte limit: the
		// SET-ID portion is cut to 28 bytes and the suffix is
		// appended last, so the number survives (2026-11-04).
		want := strings.Repeat("L", volumeIDMax-len("-001")) + "-001"
		if got := argv[i+1]; got != want {
			t.Errorf("-V arg = %q (%d bytes), want the 32-byte cap with the -001 suffix %q", got, len(got), want)
		}
	})
}

// --- the F4 context gates -----------------------------------------------------

// TestBuildCancelBeforeStart: a cancelled context at entry means NO
// staging, NO store call, NO exec - the state never leaves 'planned'.
func TestBuildCancelBeforeStart(t *testing.T) {
	f := newBuildFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Build(ctx, f.deps(), f.in); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(f.store.begins) != 0 || len(f.store.fails) != 0 || len(f.master.argvs) != 0 {
		t.Fatal("a pre-start cancellation must touch nothing")
	}
}

// TestBuildCancelAfterStageRefusesExec is THE F4 CONTRACT TEST: the
// context dies after Stage returned a COMPLETE tree (the T4.2
// cancel-after-last-member window - injected here through the content
// transaction) and Build MUST refuse to master: the exec never runs,
// the already-committed 'building' state takes the DETACHED failure
// path, and the staging scratch is cleaned.
func TestBuildCancelAfterStageRefusesExec(t *testing.T) {
	f := newBuildFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The window injector: the 6.6 transaction commits, THEN the
	// context dies - exactly where a SIGINT can land between the
	// content list and the exec.
	f.store.beginHook = cancel

	_, err := Build(ctx, f.deps(), f.in)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled (the F4 refusal)", err)
	}
	if len(f.master.argvs) != 0 {
		t.Fatal("Build invoked xorriso on a cancelled context (the F4 contract is violated)")
	}
	// The committed 'building' state took the failure path, on the
	// DETACHED context (the driver would reject a cancelled one).
	if len(f.store.fails) != 1 {
		t.Fatalf("MarkFailed calls = %d, want 1 (the committed building state must be failed)", len(f.store.fails))
	}
	if f.store.markCtxErr != nil {
		t.Errorf("MarkFailed observed ctx.Err() = %v, want nil (detached context)", f.store.markCtxErr)
	}
	// No .partial was ever produced, no final .iso exists.
	if _, err := os.Stat(filepath.Join(f.in.DestDir, "S-BUILD-001.iso")); !os.IsNotExist(err) {
		t.Errorf("a final .iso exists after a refused exec: %v", err)
	}
	// The staging scratch is gone (entries, not the dir itself).
	entries, err2 := os.ReadDir(f.in.WorkDir)
	if err2 != nil {
		t.Fatal(err2)
	}
	for _, e := range entries {
		t.Errorf("work dir still holds %q after the refused exec", e.Name())
	}
}

// TestBuildCancelDuringMaster: the ctx kill path - the master returns
// the raw context error, the image takes the failure path (detached
// MarkFailed), the .partial the master already wrote is KEPT (an
// interrupted build is inspection-worthy), and no final .iso exists.
func TestBuildCancelDuringMaster(t *testing.T) {
	f := newBuildFixture(t)
	started := make(chan struct{})
	f.master.run = func(ctx context.Context, args []string) ([]byte, error) {
		// The real xorriso shape: the .partial exists, then the
		// process is killed by the ctx.
		if _, err := defaultMasterRun(args); err != nil {
			return nil, err
		}
		close(started)
		<-ctx.Done()
		return []byte("killed"), ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()

	_, err := Build(ctx, f.deps(), f.in)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(f.store.fails) != 1 || f.store.markCtxErr != nil {
		t.Errorf("interruption must fail the image on a DETACHED context (fails %+v, markCtxErr %v)", f.store.fails, f.store.markCtxErr)
	}
	partial := filepath.Join(f.in.DestDir, "S-BUILD-001.iso.partial")
	if _, err := os.Stat(partial); err != nil {
		t.Errorf(".partial must be kept on interruption: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.in.DestDir, "S-BUILD-001.iso")); !os.IsNotExist(err) {
		t.Errorf("a final .iso exists after an interrupted master: %v", err)
	}
}

// --- the failure path: xorriso non-zero, --clean-failed -----------------------

// TestBuildXorrisoFailureKeepsPartial: a master that wrote a .partial
// and exited non-zero fails the image - state 'failed' via the store,
// the transcript tail travels in the detail (6.2 run-log clause), the
// .partial is KEPT by default (6.3 step 4), and no final .iso exists.
func TestBuildXorrisoFailureKeepsPartial(t *testing.T) {
	f := newBuildFixture(t)
	f.master.run = func(ctx context.Context, args []string) ([]byte, error) {
		if _, err := defaultMasterRun(args); err != nil {
			return nil, err
		}
		return []byte("xorriso : FAILURE : cannot master\n"), errors.New("xorriso: exited with status 5")
	}

	_, err := Build(context.Background(), f.deps(), f.in)
	if err == nil || !strings.Contains(err.Error(), "status 5") {
		t.Fatalf("err = %v, want the xorriso failure", err)
	}
	if len(f.store.fails) != 1 || !strings.Contains(f.store.fails[0].detail, "cannot master") {
		t.Errorf("MarkFailed detail = %+v, want the transcript tail (6.2 run log)", f.store.fails)
	}
	partial := filepath.Join(f.in.DestDir, "S-BUILD-001.iso.partial")
	if _, err := os.Stat(partial); err != nil {
		t.Errorf(".partial must be kept by default (6.3 step 4): %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.in.DestDir, "S-BUILD-001.iso")); !os.IsNotExist(err) {
		t.Errorf("a final .iso exists after a failed master: %v", err)
	}
	if f.extract.calls != 0 {
		t.Error("the self-verify must not run after a failed master")
	}
}

// TestBuildCleanFailedRemovesPartial: --clean-failed removes the
// .partial on the same failure (6.3 step 4's flag).
func TestBuildCleanFailedRemovesPartial(t *testing.T) {
	f := newBuildFixture(t)
	f.in.CleanFailed = true
	f.master.run = func(ctx context.Context, args []string) ([]byte, error) {
		if _, err := defaultMasterRun(args); err != nil {
			return nil, err
		}
		return nil, errors.New("xorriso: exited with status 5")
	}

	if _, err := Build(context.Background(), f.deps(), f.in); err == nil {
		t.Fatal("Build must fail")
	}
	partial := filepath.Join(f.in.DestDir, "S-BUILD-001.iso.partial")
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Errorf(".partial must be removed by --clean-failed: %v", err)
	}
	if len(f.store.fails) != 1 {
		t.Errorf("MarkFailed calls = %d, want 1", len(f.store.fails))
	}
}

// --- the self-verify (step 7) --------------------------------------------------

// TestBuildVerifyMismatchFailsKeepsPartial: an extraction that does
// not match the 6.6 content list fails the image with the per-entry
// problems named (FR-021 failure reporting), keeps the .partial, and
// never renames.
func TestBuildVerifyMismatchFailsKeepsPartial(t *testing.T) {
	f := newBuildFixture(t)
	bad := f.happyEntries()
	for i := range bad.Entries {
		if bad.Entries[i].MediaPath == "files/a.txt" {
			bad.Entries[i].Hash = sha256Of([]byte("corrupted!"))
		}
	}
	f.extract.result = bad

	_, err := Build(context.Background(), f.deps(), f.in)
	var mismatch *VerifyMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("err = %v (%T), want a *VerifyMismatchError", err, err)
	}
	if !strings.Contains(err.Error(), "files/a.txt") || !strings.Contains(err.Error(), "content hash") {
		t.Errorf("mismatch error must name the file and the hash problem:\n%v", err)
	}
	if len(f.store.fails) != 1 {
		t.Errorf("MarkFailed calls = %d, want 1", len(f.store.fails))
	}
	partial := filepath.Join(f.in.DestDir, "S-BUILD-001.iso.partial")
	if _, err := os.Stat(partial); err != nil {
		t.Errorf(".partial must be kept after a verification failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.in.DestDir, "S-BUILD-001.iso")); !os.IsNotExist(err) {
		t.Error("a verification failure must never produce a final .iso name (6.3)")
	}
	if len(f.store.ends) != 0 {
		t.Error("FinishBuild ran despite the verification failure")
	}
}

// TestBuildVerifySkippedFlag: --verify-after-build=false skips the
// extraction pass entirely (the documented V1 speed trade) and the
// result says so.
func TestBuildVerifySkippedFlag(t *testing.T) {
	f := newBuildFixture(t)
	f.in.VerifyAfterBuild = false
	f.extract = nil // no extractor at all: the skip must not need one

	res, err := Build(context.Background(), f.deps(), f.in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !res.VerifySkipped || res.VerifiedEntries != 0 {
		t.Errorf("result = (skipped %v, verified %d), want (true, 0)", res.VerifySkipped, res.VerifiedEntries)
	}
	if _, err := os.Stat(filepath.Join(f.in.DestDir, "S-BUILD-001.iso")); err != nil {
		t.Errorf("the image must still finalize when the verify is skipped: %v", err)
	}
}

// TestBuildNilRequiredSeams: nil Master, and nil Extractor WITH the
// verify enabled, are loud wiring refusals before any staging.
func TestBuildNilRequiredSeams(t *testing.T) {
	f := newBuildFixture(t)
	d := f.deps()
	d.Master = nil
	if _, err := Build(context.Background(), d, f.in); err == nil || !strings.Contains(err.Error(), "nil Master") {
		t.Fatalf("nil Master: err = %v, want the wiring refusal", err)
	}
	d = f.deps()
	d.Extractor = nil
	if _, err := Build(context.Background(), d, f.in); err == nil || !strings.Contains(err.Error(), "nil Extractor") {
		t.Fatalf("nil Extractor with verify on: err = %v, want the wiring refusal", err)
	}
	for _, e := range f.events {
		t.Errorf("seam refusals must run before any pipeline step, saw %q", e)
	}
}

// --- the comparison logic (compareContents) ------------------------------------

// TestCompareContents pins the step-7 comparison: every mismatch class
// is reported per entry, and the implied parent dirs of the 5.3 tree
// are tolerated.
func TestCompareContents(t *testing.T) {
	hashX := sha256Of([]byte("xxx"))
	hashY := sha256Of([]byte("yyy"))
	rows := []ContentRow{
		{MediaPath: "files/a.txt", Kind: media.KindFile, ChunkIndex: -1, Size: 3, Hash: hashX},
		{MediaPath: "files/link", Kind: media.KindSymlink, FileID: 7, ChunkIndex: -1, Size: 5},
		{MediaPath: "files/empty", Kind: media.KindDir, ChunkIndex: -1},
		{MediaPath: "chunks/big/00001", Kind: media.KindChunk, FileID: 2, ChunkIndex: 0, Size: 3, Hash: hashY},
	}
	targets := map[string]string{"files/link": "a.txt"}
	base := func() []verify.Entry {
		return []verify.Entry{
			{MediaPath: "files", Kind: verify.KindDir},
			{MediaPath: "files/a.txt", Kind: verify.KindFile, Size: 3, Hash: hashX},
			{MediaPath: "files/link", Kind: verify.KindSymlink, Size: 5, LinkTarget: "a.txt"},
			{MediaPath: "files/empty", Kind: verify.KindDir},
			{MediaPath: "chunks/big/00001", Kind: verify.KindFile, Size: 3, Hash: hashY},
		}
	}
	if p := compareContents(rows, base(), targets); len(p) != 0 {
		t.Fatalf("clean comparison produced problems: %v", p)
	}

	// Missing: drop the chunk entry.
	e := base()
	e = append(e[:4], e[5:]...)
	if p := compareContents(rows, e, targets); len(p) != 1 || !strings.Contains(p[0], "missing from extraction: chunks/big/00001") {
		t.Errorf("missing-entry problems = %v, want the one named entry", p)
	}
	// Hash mismatch.
	e = base()
	e[1].Hash = hashY
	if p := compareContents(rows, e, targets); len(p) != 1 || !strings.Contains(p[0], "content hash") {
		t.Errorf("hash-mismatch problems = %v", p)
	}
	// Size mismatch.
	e = base()
	e[1].Size = 99
	if p := compareContents(rows, e, targets); len(p) != 1 || !strings.Contains(p[0], "size 99, want 3") {
		t.Errorf("size-mismatch problems = %v", p)
	}
	// Kind mismatch (file extracted as dir).
	e = base()
	e[1].Kind = verify.KindDir
	if p := compareContents(rows, e, targets); len(p) != 1 || !strings.Contains(p[0], "extracted as dir, want file") {
		t.Errorf("kind-mismatch problems = %v", p)
	}
	// Symlink target mismatch.
	e = base()
	e[2].LinkTarget = "other.txt"
	if p := compareContents(rows, e, targets); len(p) != 1 || !strings.Contains(p[0], "symlink target") {
		t.Errorf("target-mismatch problems = %v", p)
	}
	// Unexpected file and stray dir; the implied parent "files" stays
	// tolerated.
	e = append(base(),
		verify.Entry{MediaPath: "files/rogue.bin", Kind: verify.KindFile, Size: 3, Hash: hashX},
		verify.Entry{MediaPath: "chunks", Kind: verify.KindDir},
		verify.Entry{MediaPath: "stray", Kind: verify.KindDir},
	)
	p := compareContents(rows, e, targets)
	if len(p) != 2 || !strings.Contains(p[0], "unexpected file in extraction: files/rogue.bin") ||
		!strings.Contains(p[1], "unexpected dir in extraction: stray") {
		t.Errorf("unexpected-entry problems = %v", p)
	}
}

// --- the state-machine fault paths (the store seams' error injection) ----------

// TestBuildBeginBuildTransactionFailure: when the 6.6 transaction
// fails, it ROLLED BACK - the state is untouched ('planned'), so Build
// must return the plain error with NO MarkFailed (there is nothing to
// fail), no exec, no extraction, and no .partial (catalog tier pins
// the SQL rollback; this pins the builder's side of the contract).
func TestBuildBeginBuildTransactionFailure(t *testing.T) {
	f := newBuildFixture(t)
	f.store.beginErr = errors.New("injected: content transaction rolled back")

	_, err := Build(context.Background(), f.deps(), f.in)
	if err == nil || !strings.Contains(err.Error(), "injected: content transaction") {
		t.Fatalf("err = %v, want the BeginBuild error verbatim", err)
	}
	if len(f.store.fails) != 0 {
		t.Errorf("MarkFailed must NOT run when the 6.6 transaction rolled back (state untouched): %+v", f.store.fails)
	}
	if len(f.master.argvs) != 0 || f.extract.calls != 0 {
		t.Error("a rolled-back 6.6 transaction must stop the pipeline before exec and extraction")
	}
	if _, err := os.Stat(filepath.Join(f.in.DestDir, "S-BUILD-001.iso.partial")); !os.IsNotExist(err) {
		t.Errorf("a rolled-back 6.6 transaction must leave no .partial: %v", err)
	}
	if len(f.store.ends) != 0 {
		t.Error("FinishBuild ran despite the failed content transaction")
	}
}

// TestBuildFinishBuildFailureReportsHonesty: the rename succeeded (the
// .iso EXISTS at the final path) but the 'built' transaction failed.
// The documented behavior (file doc, build.go): report honestly - the
// build SUCCEEDED on media, the state write failed; the error names
// both facts, and NO MarkFailed runs (the media is not a failure; the
// next rebuild self-heals the stale 'building' row).
func TestBuildFinishBuildFailureReportsHonesty(t *testing.T) {
	f := newBuildFixture(t)
	f.store.finishErr = errors.New("injected: built-state write failed")
	f.extract.result = f.happyEntries()

	res, err := Build(context.Background(), f.deps(), f.in)
	if err == nil || !strings.Contains(err.Error(), "finalized at") ||
		!strings.Contains(err.Error(), "built-state transaction failed") {
		t.Fatalf("err = %v, want the honest finalized-but-unstamped report", err)
	}
	if len(f.store.ends) != 1 {
		t.Fatalf("FinishBuild calls = %d, want 1 (the write was attempted)", len(f.store.ends))
	}
	if len(f.store.fails) != 0 {
		t.Errorf("MarkFailed must NOT run: the media succeeded, only the state write failed: %+v", f.store.fails)
	}
	finalISO := filepath.Join(f.in.DestDir, "S-BUILD-001.iso")
	if _, err := os.Stat(finalISO); err != nil {
		t.Errorf("the finalized .iso must EXIST on disk after a FinishBuild failure (documented honesty): %v", err)
	}
	if res.ISOPath != finalISO {
		t.Errorf("BuildResult.ISOPath = %q, want the finalized path %q", res.ISOPath, finalISO)
	}
}

// TestBuildMarkFailedFailureJoinsCause: when the failure path's own
// state write also fails, the returned error carries BOTH the primary
// cause and the state-write failure (errors.Join - neither fault may
// mask the other).
func TestBuildMarkFailedFailureJoinsCause(t *testing.T) {
	f := newBuildFixture(t)
	f.store.markErr = errors.New("injected: mark-failed write refused")
	f.master.run = func(ctx context.Context, args []string) ([]byte, error) {
		if _, err := defaultMasterRun(args); err != nil {
			return nil, err
		}
		return nil, errors.New("xorriso: exited with status 5")
	}

	_, err := Build(context.Background(), f.deps(), f.in)
	if err == nil || !strings.Contains(err.Error(), "status 5") {
		t.Fatalf("err = %v, must keep the primary xorriso cause", err)
	}
	if !strings.Contains(err.Error(), "recording the failed state") ||
		!strings.Contains(err.Error(), "injected: mark-failed") {
		t.Errorf("err = %v, must ALSO carry the MarkFailed write failure (joined, not masked)", err)
	}
}

// --- the finalize fault path (rename) -----------------------------------------

// TestBuildRenameFailureKeepsPartialAndFailsState: fsync of the
// .partial succeeds, but the atomic rename fails (EACCES: the
// destination directory was made read-only after mastering, via the
// AfterMaster seam). The image takes the ONE failure path: state
// 'failed' via MarkFailed, the .partial KEPT for inspection (6.3 step
// 4), no final .iso name, no FinishBuild.
func TestBuildRenameFailureKeepsPartialAndFailsState(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root (euid 0): CAP_DAC_OVERRIDE renames into the chmod-0555 dest, so the rename EACCES cannot be provoked")
	}
	f := newBuildFixture(t)
	f.extract.result = f.happyEntries()
	deps := f.deps()
	deps.AfterMaster = func(ctx context.Context, partialPath string) error {
		// Read-only destination dir: opening/fsyncing files and the
		// dir still work (r-x), but the rename into it hits EACCES.
		if err := os.Chmod(f.in.DestDir, 0o555); err != nil {
			t.Fatalf("chmod dest read-only: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(f.in.DestDir, 0o755) }) // let TempDir cleanup pass
		return nil
	}

	_, err := Build(context.Background(), deps, f.in)
	if err == nil || !strings.Contains(err.Error(), "renaming") {
		t.Fatalf("err = %v, want the rename failure", err)
	}
	if len(f.store.fails) != 1 {
		t.Errorf("MarkFailed calls = %d, want 1 (the committed 'building' state takes the failure path)", len(f.store.fails))
	}
	if len(f.store.ends) != 0 {
		t.Error("FinishBuild ran despite the rename failure")
	}
	finalISO := filepath.Join(f.in.DestDir, "S-BUILD-001.iso")
	if _, err := os.Stat(finalISO); !os.IsNotExist(err) {
		t.Errorf("a final .iso name must not exist after a failed rename: %v", err)
	}
	if _, err := os.Stat(finalISO + ".partial"); err != nil {
		t.Errorf(".partial must be KEPT after the rename failure (6.3 step 4): %v", err)
	}
}

// --- the export/manifest embedding integrity (marshal -> hash -> embed) -------

// TestBuildExportChangedAfterHashFailsBeforeBegin: the run's export
// file changed between export production (Export.Hash) and the embed
// read-back. embedExport's published-hash re-check must refuse the
// build BEFORE the 6.6 transaction - a content list whose export
// self-row carries the PRODUCED hash while the image embeds different
// bytes would lie to restore (FR-034/FR-035 honesty).
func TestBuildExportChangedAfterHashFailsBeforeBegin(t *testing.T) {
	t.Run("same size, different bytes", func(t *testing.T) {
		f := newBuildFixture(t)
		// Overwrite the export with different bytes of the SAME
		// length: only the hash re-check can catch this.
		if err := os.WriteFile(f.in.Export.Path, []byte("fake catalog export byteS"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := Build(context.Background(), f.deps(), f.in)
		if err == nil || !strings.Contains(err.Error(), "export file changed during the run") {
			t.Fatalf("err = %v, want the export-changed refusal", err)
		}
		if len(f.store.begins) != 0 || len(f.store.fails) != 0 || len(f.master.argvs) != 0 {
			t.Error("an export-changed refusal must land BEFORE the 6.6 transaction (state untouched, no exec)")
		}
	})
	t.Run("size changed", func(t *testing.T) {
		f := newBuildFixture(t)
		if err := os.WriteFile(f.in.Export.Path, []byte("appended garbage"), 0o644); err != nil {
			t.Fatal(err)
		}
		// Any size change also changes the hash, so the published-hash
		// re-check fires first (the dedicated pubSize guard below it is
		// defense-in-depth against an inconsistent EmbedFile result,
		// unreachable through file mutation). The build must refuse
		// BEFORE the 6.6 transaction either way.
		_, err := Build(context.Background(), f.deps(), f.in)
		if err == nil || !strings.Contains(err.Error(), "the export file changed during the run") {
			t.Fatalf("err = %v, want the export-changed refusal", err)
		}
		if len(f.store.begins) != 0 {
			t.Error("an export-size mutation must land BEFORE the 6.6 transaction")
		}
	})
}

// TestBuildChunkHashSnapshotMismatchRefuses: fetchChunkHashes' D-09
// cross-check - a chunk whose inventory SNAPSHOT hash disagrees with
// the STORED one (injected at read time; staging overwrites stored
// captures, so the mismatch can only mean the catalog changed under
// the single-writer flock) is a loud refusal before any embed or
// transaction.
func TestBuildChunkHashSnapshotMismatchRefuses(t *testing.T) {
	f := newBuildFixture(t)
	// The snapshot carries the correct hash; the store read returns a
	// different one.
	f.in.Inventory.Chunks[0].Hash = f.chunkH0
	f.store.chunkReadMutate = func(h [][]byte) [][]byte {
		out := append([][]byte(nil), h...)
		out[0] = sha256Of([]byte("wrong capture"))
		return out
	}

	_, err := Build(context.Background(), f.deps(), f.in)
	if err == nil || !strings.Contains(err.Error(), "catalog changed under the single-writer lock") {
		t.Fatalf("err = %v, want the snapshot-vs-stored refusal", err)
	}
	if len(f.store.begins) != 0 || len(f.store.fails) != 0 || len(f.master.argvs) != 0 {
		t.Error("a chunk-hash snapshot mismatch must refuse BEFORE the 6.6 transaction (state untouched, no exec)")
	}
}

// TestFailureDetailPutsCauseFirstAndCapsTail: the run_events detail
// carries the cause at the HEAD and only a capped transcript tail.
func TestFailureDetailPutsCauseFirstAndCapsTail(t *testing.T) {
	cause := errors.New("mastering blew up")
	detail := failureDetail(cause, []byte("short transcript"))
	if !strings.HasPrefix(detail, "image build failed: mastering blew up") ||
		!strings.Contains(detail, "short transcript") {
		t.Errorf("detail = %q, want cause first then transcript", detail)
	}

	long := make([]byte, transcriptDetailCap*2)
	for i := range long {
		long[i] = 'x'
	}
	long = append([]byte("HEAD-OF-TRANSCRIPT"), long...)
	long = append(long, []byte("TAIL-OF-TRANSCRIPT")...)
	detail = failureDetail(cause, long)
	if !strings.HasPrefix(detail, "image build failed:") {
		t.Errorf("capped detail must keep the cause first: %q", detail[:60])
	}
	if strings.Contains(detail, "HEAD-OF-TRANSCRIPT") {
		t.Error("the head of a long transcript must be dropped (tail retention, the T4.1 rationale)")
	}
	if !strings.Contains(detail, "TAIL-OF-TRANSCRIPT") {
		t.Error("the tail of the transcript must survive the cap")
	}
	if len(detail) > len("image build failed: mastering blew up")+len("\nxorriso output [... ]:\n")+transcriptDetailCap+64 {
		t.Errorf("capped detail is %d bytes, want roughly cause + %d-byte tail", len(detail), transcriptDetailCap)
	}
}
