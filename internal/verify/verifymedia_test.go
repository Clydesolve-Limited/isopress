package verify

// T5.1: the verify-media SERVICE tier (DESIGN_SPEC 7.2;
// IMPLEMENTATION_PLAN 4.6 T5.1 acceptance + adversarial list). The
// fake tier drives VerifyImage over REAL copy trees and a FAKE xorriso
// (/bin/sh script, the extract_test pattern); the corrupted-ISO
// acceptance test lives in verifymedia_real_test.go on the gated real
// binary (12.6).
//
// Every catalog interaction goes through the memStore seam below -
// the *catalog.DB adapter is pinned at the cli tier
// (internal/cli/verifymedia_test.go).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"isopress/internal/media"
	"isopress/internal/progress"
	"isopress/internal/units"
)

// --- the catalog seam fake -----------------------------------------------------

type stampRecord struct {
	setID   string
	imageID int64
	at      int64
}

// memStore is the in-memory VerifyStore: one content list per image,
// every stamp recorded. contentsErr/stampErr inject the failure paths.
type memStore struct {
	mu          sync.Mutex
	lists       map[int64]ContentList
	stamps      []stampRecord
	contentsErr error
	stampErr    error
}

func newMemStore() *memStore { return &memStore{lists: map[int64]ContentList{}} }

func (m *memStore) ImageContents(_ context.Context, _ string, imageID int64) (ContentList, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.contentsErr != nil {
		return ContentList{}, m.contentsErr
	}
	list, ok := m.lists[imageID]
	if !ok {
		return ContentList{}, errors.New("memStore: no content list for image")
	}
	return list, nil
}

func (m *memStore) StampVerified(_ context.Context, setID string, imageID int64, verifiedAt int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stampErr != nil {
		return m.stampErr
	}
	m.stamps = append(m.stamps, stampRecord{setID: setID, imageID: imageID, at: verifiedAt})
	return nil
}

func (m *memStore) stampCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.stamps)
}

// --- the copy-tree fixture -----------------------------------------------------

// copyTreeFixture is a real 5.3 copy tree plus its matching content
// list: whole files, a chunked parent's two chunks, the four symlink
// variants of the pinned osirrox contract (relative, dangling, to-dir,
// absolute), an empty dir, and the manifest/export self-rows.
type copyTreeFixture struct {
	root    string
	list    ContentList
	targets map[string]string
	payload []byte
}

func makeCopyTreeFixture(t *testing.T) *copyTreeFixture {
	t.Helper()
	root := t.TempDir()
	f := &copyTreeFixture{root: root, targets: map[string]string{
		"files/link":          "a.txt",
		"files/link-dangling": "missing-target.txt",
		"files/link-todir":    "sub",
		"files/link-abs":      "/etc/passwd",
	}}

	write := func(rel string, content []byte) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
		f.list.Rows = append(f.list.Rows, ContentRow{
			MediaPath: rel, Kind: media.KindFile, ChunkIndex: -1,
			Size: int64(len(content)), Hash: hashOfBytes(content),
		})
	}
	write("files/a.txt", []byte("hello verify\n"))
	write("files/sub/deep.txt", []byte("deep down\n"))
	write("chunks/big.bin/00001", []byte("chunk-one"))
	write("chunks/big.bin/00002", []byte("chunk-two!"))
	write(media.ManifestFileName, []byte(`{"set_id":"S-V"}`))
	write(media.ExportFileName, []byte("fake export bytes"))
	f.payload = []byte("hello verify\n")

	for _, l := range []struct{ name, target string }{
		{"link", "a.txt"},
		{"link-dangling", "missing-target.txt"},
		{"link-todir", "sub"},
		{"link-abs", "/etc/passwd"},
	} {
		p := filepath.Join(root, "files", l.name)
		if err := os.Symlink(l.target, p); err != nil {
			t.Fatal(err)
		}
		rel := "files/" + l.name
		f.list.Rows = append(f.list.Rows, ContentRow{
			MediaPath: rel, Kind: media.KindSymlink, ChunkIndex: -1, Size: int64(len(l.target)),
		})
	}
	if err := os.MkdirAll(filepath.Join(root, "files", "emptydir"), 0o750); err != nil {
		t.Fatal(err)
	}
	f.list.Rows = append(f.list.Rows, ContentRow{MediaPath: "files/emptydir", Kind: media.KindDir, ChunkIndex: -1})

	f.list.SymlinkTargets = f.targets
	return f
}

// verifyFixtureDeps wires a Deps over a memStore holding the fixture's
// list under image 1 (the returned store lets tests assert stamps).
func verifyFixtureDeps(f *copyTreeFixture) (Deps, *memStore) {
	st := newMemStore()
	st.lists[1] = f.list
	return Deps{Store: st}, st
}

// --- copy-tree mode ---------------------------------------------------------------

func TestVerifyCopyTreeCleanFullMatchStamps(t *testing.T) {
	f := makeCopyTreeFixture(t)
	deps, st := verifyFixtureDeps(f)

	res, err := VerifyImage(context.Background(), deps, VerifyInput{
		SetID: "S-V", ImageID: 1, Path: f.root, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("VerifyImage: %v", err)
	}
	if res.Source != SourceCopyTree || !res.Comparison.Clean() || !res.Stamped {
		t.Fatalf("result = (source %s, clean %v, stamped %v), want (copy, true, true): %v",
			res.Source, res.Comparison.Clean(), res.Stamped, res.Comparison.Problems())
	}
	if len(res.Comparison.Matched) != len(f.list.Rows) {
		t.Errorf("matched %d entries, want all %d", len(res.Comparison.Matched), len(f.list.Rows))
	}
	if st.stampCount() != 1 || st.stamps[0].setID != "S-V" || st.stamps[0].imageID != 1 || st.stamps[0].at <= 0 {
		t.Errorf("stamps = %+v, want exactly one verified stamp with a positive timestamp", st.stamps)
	}
	// Copy-tree mode touches NO temp space (no scratch to clean).
	if _, err := os.Stat(f.root); err != nil {
		t.Errorf("the copy tree must be untouched: %v", err)
	}
}

// TestVerifyCopyTreeCorruptedFileNamesExactlyThatFile is the
// implementation-tier half of the plan's acceptance: one payload byte
// flipped -> exactly that file mismatched with a content-hash reason,
// everything else matched, NO stamp.
func TestVerifyCopyTreeCorruptedFileNamesExactlyThatFile(t *testing.T) {
	f := makeCopyTreeFixture(t)
	deps, st := verifyFixtureDeps(f)

	p := filepath.Join(f.root, "files", "a.txt")
	orig := append([]byte(nil), f.payload...)
	corrupt := append([]byte(nil), orig...)
	corrupt[3] ^= 0xff
	if err := os.WriteFile(p, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := VerifyImage(context.Background(), deps, VerifyInput{SetID: "S-V", ImageID: 1, Path: f.root})
	if err != nil {
		t.Fatalf("VerifyImage: %v", err)
	}
	if res.Comparison.Clean() || res.Stamped {
		t.Fatalf("corrupted tree = (clean %v, stamped %v), want (false, false)", res.Comparison.Clean(), res.Stamped)
	}
	if len(res.Comparison.Mismatched) != 1 || res.Comparison.Mismatched[0].MediaPath != "files/a.txt" ||
		!strings.Contains(res.Comparison.Mismatched[0].Reasons[0], "content hash") {
		t.Errorf("mismatched = %+v, want exactly files/a.txt by content hash", res.Comparison.Mismatched)
	}
	if len(res.Comparison.Missing) != 0 || len(res.Comparison.Unexpected) != 0 || len(res.Comparison.Unreadable) != 0 {
		t.Errorf("other categories = (%v, %v, %v), want empty", res.Comparison.Missing, res.Comparison.Unexpected, res.Comparison.Unreadable)
	}
	if st.stampCount() != 0 {
		t.Errorf("a partial match must never stamp: %+v", st.stamps)
	}
	// The rest of the tree IS verified: the matched set is every other
	// listed row.
	if len(res.Comparison.Matched) != len(f.list.Rows)-1 {
		t.Errorf("matched %d entries, want %d (everything but the corrupted file)",
			len(res.Comparison.Matched), len(f.list.Rows)-1)
	}
}

func TestVerifyCopyTreeMissingAndUnexpectedAndUnreadable(t *testing.T) {
	f := makeCopyTreeFixture(t)
	deps, st := verifyFixtureDeps(f)

	// Delete a listed payload, plant a rogue file.
	if err := os.Remove(filepath.Join(f.root, "files", "sub", "deep.txt")); err != nil {
		t.Fatal(err)
	}
	rogue := filepath.Join(f.root, "files", "rogue.bin")
	if err := os.WriteFile(rogue, []byte("rogue"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := VerifyImage(context.Background(), deps, VerifyInput{SetID: "S-V", ImageID: 1, Path: f.root})
	if err != nil {
		t.Fatalf("VerifyImage: %v", err)
	}
	if len(res.Comparison.Missing) != 1 || res.Comparison.Missing[0].MediaPath != "files/sub/deep.txt" {
		t.Errorf("missing = %+v, want the deleted payload", res.Comparison.Missing)
	}
	if len(res.Comparison.Unexpected) != 1 || res.Comparison.Unexpected[0].MediaPath != "files/rogue.bin" {
		t.Errorf("unexpected = %+v, want the planted rogue file", res.Comparison.Unexpected)
	}
	if st.stampCount() != 0 {
		t.Errorf("an anomalous match must never stamp")
	}

	// The unreadable class: a listed payload nobody can read (skipped
	// as root, where mode 0000 is still readable).
	if os.Geteuid() == 0 {
		t.Skip("running as root: mode 0000 files remain readable, so the unreadable class cannot be provoked")
	}
	f2 := makeCopyTreeFixture(t)
	deps2, st2 := verifyFixtureDeps(f2)
	if err := os.Chmod(filepath.Join(f2.root, "files", "a.txt"), 0o000); err != nil {
		t.Fatal(err)
	}
	res2, err := VerifyImage(context.Background(), deps2, VerifyInput{SetID: "S-V", ImageID: 1, Path: f2.root})
	if err != nil {
		t.Fatalf("VerifyImage (unreadable): %v", err)
	}
	if len(res2.Comparison.Unreadable) != 1 || res2.Comparison.Unreadable[0].MediaPath != "files/a.txt" {
		t.Errorf("unreadable = %+v, want files/a.txt", res2.Comparison.Unreadable)
	}
	if len(res2.Comparison.Mismatched) != 0 {
		t.Errorf("an unreadable member must NOT be reported as a hash mismatch: %+v", res2.Comparison.Mismatched)
	}
	if st2.stampCount() != 0 {
		t.Errorf("an unreadable member is an anomaly: no stamp")
	}
}

// TestVerifyCopyTreeChunkedMultiImage pins the per-image isolation of
// the chunked-set acceptance: the two chunks live on different images
// (the 5.2 placement), and a corrupted chunk on image 2 is reported
// for IMAGE 2 ONLY - image 1 still full-matches and stamps.
func TestVerifyCopyTreeChunkedMultiImage(t *testing.T) {
	root := t.TempDir()
	st := newMemStore()

	// Image 1 holds chunk 0 + the manifest; image 2 holds chunk 1 and
	// the payload file.
	chunk0, chunk1 := []byte("chunk-zero-000"), []byte("chunk-one-111")
	payload := []byte("parent payload\n")
	writeAt := func(rel string, content []byte) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rowFor := func(rel string, content []byte) ContentRow {
		return ContentRow{MediaPath: rel, Kind: media.KindFile, ChunkIndex: -1,
			Size: int64(len(content)), Hash: hashOfBytes(content)}
	}
	chunkRowFor := func(rel string, idx int, content []byte) ContentRow {
		return ContentRow{MediaPath: rel, Kind: media.KindChunk, FileID: 9, ChunkIndex: idx,
			Size: int64(len(content)), Hash: hashOfBytes(content)}
	}

	dir1, dir2 := filepath.Join(root, "S-V-001"), filepath.Join(root, "S-V-002")
	writeAt("S-V-001/chunks/parent.bin/00001", chunk0)
	writeAt("S-V-001/"+media.ManifestFileName, []byte(`{"set_id":"S-V"}`))
	writeAt("S-V-002/chunks/parent.bin/00002", chunk1)
	writeAt("S-V-002/files/parent.bin", payload)
	st.lists[1] = ContentList{Rows: []ContentRow{
		chunkRowFor("chunks/parent.bin/00001", 0, chunk0),
		rowFor(media.ManifestFileName, []byte(`{"set_id":"S-V"}`)),
	}}
	st.lists[2] = ContentList{Rows: []ContentRow{
		chunkRowFor("chunks/parent.bin/00002", 1, chunk1),
		rowFor("files/parent.bin", payload),
	}}

	// Corrupt IMAGE 2's chunk only.
	bad := append([]byte(nil), chunk1...)
	bad[0] ^= 0xff
	writeAt("S-V-002/chunks/parent.bin/00002", bad)

	res1, err := VerifyImage(context.Background(), Deps{Store: st}, VerifyInput{SetID: "S-V", ImageID: 1, Path: dir1})
	if err != nil {
		t.Fatalf("VerifyImage(1): %v", err)
	}
	res2, err := VerifyImage(context.Background(), Deps{Store: st}, VerifyInput{SetID: "S-V", ImageID: 2, Path: dir2})
	if err != nil {
		t.Fatalf("VerifyImage(2): %v", err)
	}
	if !res1.Comparison.Clean() || !res1.Stamped {
		t.Errorf("image 1 = (clean %v, stamped %v), want a full match: %v", res1.Comparison.Clean(), res1.Stamped, res1.Comparison.Problems())
	}
	if len(res2.Comparison.Mismatched) != 1 || res2.Comparison.Mismatched[0].MediaPath != "chunks/parent.bin/00002" {
		t.Errorf("image 2 mismatched = %+v, want exactly its corrupted chunk", res2.Comparison.Mismatched)
	}
	if res2.Stamped {
		t.Error("image 2 must not stamp on a partial match")
	}
	if st.stampCount() != 1 {
		t.Errorf("stamps = %d, want exactly one (image 1)", st.stampCount())
	}
}

// --- ISO mode (fake xorriso) --------------------------------------------------------

// installVerifyFakeTool writes a fake xorriso whose EXTRACTION body
// runs after the argv dump (the extract_test installFakeTool shape).
func installVerifyFakeTool(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	return installFakeTool(t, dir, body)
}

func TestVerifyISOCleanViaFakeXorriso(t *testing.T) {
	prepared := filepath.Join(t.TempDir(), "prepared")
	if err := os.MkdirAll(filepath.Join(prepared, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	payload := []byte("iso payload\n")
	if err := os.WriteFile(filepath.Join(prepared, "files", "a.txt"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prepared, media.ManifestFileName), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(prepared, "files", "link")); err != nil {
		t.Fatal(err)
	}

	toolPath := installVerifyFakeTool(t, `cp -r "`+prepared+`/." "$7"`)
	iso := filepath.Join(t.TempDir(), "S-V-001.iso")
	if err := os.WriteFile(iso, []byte("fake iso"), 0o644); err != nil {
		t.Fatal(err)
	}

	st := newMemStore()
	st.lists[1] = ContentList{
		Rows: []ContentRow{
			{MediaPath: "files/a.txt", Kind: media.KindFile, ChunkIndex: -1, Size: int64(len(payload)), Hash: hashOfBytes(payload)},
			{MediaPath: "files/link", Kind: media.KindSymlink, ChunkIndex: -1, Size: 5},
			{MediaPath: media.ManifestFileName, Kind: KindManifest, ChunkIndex: -1, Size: 2, Hash: hashOfBytes([]byte("{}"))},
		},
		SymlinkTargets: map[string]string{"files/link": "a.txt"},
	}
	work := t.TempDir()
	res, err := VerifyImage(context.Background(), Deps{Store: st, Tool: toolWith(toolPath)}, VerifyInput{
		SetID: "S-V", ImageID: 1, Path: iso, WorkDir: work,
	})
	if err != nil {
		t.Fatalf("VerifyImage (ISO): %v", err)
	}
	if res.Source != SourceISO || !res.Comparison.Clean() || !res.Stamped {
		t.Fatalf("result = (source %s, clean %v, stamped %v), want (iso, true, true): %v",
			res.Source, res.Comparison.Clean(), res.Stamped, res.Comparison.Problems())
	}
	// THE SCRATCH IS GONE on the success path (cleanup on every path).
	assertDirEmpty(t, work)
}

// TestVerifyISORogueExtractionMemberIsUnexpected plants a rogue file
// IN THE EXTRACTION (the fake tool copies the prepared tree and adds
// an extra member) - it must be reported as unexpected, never silently
// ignored.
func TestVerifyISORogueExtractionMemberIsUnexpected(t *testing.T) {
	prepared := filepath.Join(t.TempDir(), "prepared")
	if err := os.MkdirAll(filepath.Join(prepared, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prepared, "files", "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	toolPath := installVerifyFakeTool(t,
		`cp -r "`+prepared+`/." "$7" && echo rogue > "$7/files/rogue.bin"`)
	iso := filepath.Join(t.TempDir(), "S-V-001.iso")
	if err := os.WriteFile(iso, []byte("fake iso"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := newMemStore()
	st.lists[1] = ContentList{Rows: []ContentRow{
		{MediaPath: "files/a.txt", Kind: media.KindFile, ChunkIndex: -1, Size: 1, Hash: hashOfBytes([]byte("x"))},
	}}

	res, err := VerifyImage(context.Background(), Deps{Store: st, Tool: toolWith(toolPath)}, VerifyInput{
		SetID: "S-V", ImageID: 1, Path: iso, WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("VerifyImage: %v", err)
	}
	if len(res.Comparison.Unexpected) != 1 || res.Comparison.Unexpected[0].MediaPath != "files/rogue.bin" {
		t.Errorf("unexpected = %+v, want the planted rogue extraction member", res.Comparison.Unexpected)
	}
	if res.Stamped {
		t.Error("a rogue extraction member is an anomaly: no stamp")
	}
}

// TestVerifyISOEmptyExtractionIsReadFailureNotMissing is the
// SERVICE-tier half of the zero-entry gate: a fake xorriso that EXITS 0
// with its warning on stderr and extracts NOTHING - the xorriso 1.5.6
// unparseable-ISO shape ("No ISO 9660 image at LBA 0 / Creating blank
// image", empty tree) - must fail the image as UNREADABLE (zero entries
// against N expected, xorriso's own output tail surfaced), never as N
// missing payloads, and must stamp nothing and leak no scratch.
func TestVerifyISOEmptyExtractionIsReadFailureNotMissing(t *testing.T) {
	toolPath := installVerifyFakeTool(t,
		"echo 'libisoburn: WARNING : No ISO 9660 image at LBA 0.' >&2\n"+
			"echo 'libisoburn: WARNING : Creating blank image.' >&2\n"+
			"exit 0")
	iso := filepath.Join(t.TempDir(), "S-V-001.iso")
	if err := os.WriteFile(iso, []byte("fake iso"), 0o644); err != nil {
		t.Fatal(err)
	}
	rows := []ContentRow{
		{MediaPath: "files/a.txt", Kind: media.KindFile, ChunkIndex: -1, Size: 1, Hash: hashOfBytes([]byte("x"))},
		{MediaPath: media.ManifestFileName, Kind: KindManifest, ChunkIndex: -1, Size: 2, Hash: hashOfBytes([]byte("{}"))},
	}
	st := newMemStore()
	st.lists[1] = ContentList{Rows: rows}
	work := t.TempDir()

	_, err := VerifyImage(context.Background(), Deps{Store: st, Tool: toolWith(toolPath)}, VerifyInput{
		SetID: "S-V", ImageID: 1, Path: iso, WorkDir: work,
	})
	if err == nil {
		t.Fatal("an extraction that yielded no entries against a non-empty content list must fail as a READ failure")
	}
	for _, want := range []string{
		"no extractable entries",
		fmt.Sprintf("against %d expected", len(rows)),
		"unreadable",
		"not an ISO 9660 image",
		"No ISO 9660 image at LBA 0", // the surfaced xorriso tail
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "missing") {
		t.Errorf("err = %v, must never diagnose the unreadable image as missing payloads", err)
	}
	if st.stampCount() != 0 {
		t.Error("a read failure must never stamp")
	}
	assertDirEmpty(t, work)
}

// TestVerifyISOFreeSpaceRefusalPrecedesExtraction pins the 7.2
// pre-check: an insufficient-space refusal happens BEFORE xorriso runs
// (the fake-xorriso sentinel stays untouched) and cleans its scratch.
func TestVerifyISOFreeSpaceRefusalPrecedesExtraction(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "extract-invoked")
	toolPath := installFakeTool(t, dir, `: > "`+marker+`"`)
	iso := filepath.Join(dir, "S-V-001.iso")
	if err := os.WriteFile(iso, []byte("fake iso"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := newMemStore()
	st.lists[1] = ContentList{Rows: []ContentRow{
		{MediaPath: "files/a.txt", Kind: media.KindFile, ChunkIndex: -1, Size: 1 << 20, Hash: hashOfBytes([]byte("x"))},
	}}
	work := t.TempDir()

	_, err := VerifyImage(context.Background(), Deps{
		Store: st, Tool: toolWith(toolPath),
		FreeSpace: func(string) (uint64, error) { return 1024, nil }, // far below payload + slack
	}, VerifyInput{SetID: "S-V", ImageID: 1, Path: iso, WorkDir: work})
	if err == nil || !strings.Contains(err.Error(), "needs about") || !strings.Contains(err.Error(), "available") {
		t.Fatalf("err = %v, want the up-front free-space refusal stating the requirement", err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the free-space refusal must precede ANY xorriso invocation (sentinel touched)")
	}
	if st.stampCount() != 0 {
		t.Error("a refused verification must never stamp")
	}
	assertDirEmpty(t, work)
}

// TestVerifyISOCancellationCleansScratch: cancellation MID-EXTRACTION
// aborts with the context error, removes the scratch (no leak), and
// stamps nothing.
func TestVerifyISOCancellationCleansScratch(t *testing.T) {
	dir := t.TempDir()
	prepared := filepath.Join(dir, "prepared")
	if err := os.MkdirAll(filepath.Join(prepared, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prepared, "files", "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(dir, "extracting")
	// The fake extraction completes the copy, announces readiness, and
	// parks INSIDE the tool run - the deterministic mid-extraction
	// window (exec so the process the T4.1 wrapper kills IS the sleeper).
	toolPath := installFakeTool(t, dir,
		`cp -r "`+prepared+`/." "$7" && : > "`+ready+`" && exec sleep 60`)
	iso := filepath.Join(dir, "S-V-001.iso")
	if err := os.WriteFile(iso, []byte("fake iso"), 0o644); err != nil {
		t.Fatal(err)
	}
	st := newMemStore()
	st.lists[1] = ContentList{Rows: []ContentRow{
		{MediaPath: "files/a.txt", Kind: media.KindFile, ChunkIndex: -1, Size: 1, Hash: hashOfBytes([]byte("x"))},
	}}
	work := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := VerifyImage(ctx, Deps{Store: st, Tool: toolWith(toolPath)}, VerifyInput{
			SetID: "S-V", ImageID: 1, Path: iso, WorkDir: work,
		})
		done <- err
	}()
	// Wait for the extraction to be IN FLIGHT, then cancel.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the fake extraction never became ready")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("VerifyImage did not return after cancellation")
	}
	if st.stampCount() != 0 {
		t.Error("a cancelled verification must never stamp")
	}
	assertDirEmpty(t, work)
}

// --- refusal paths -----------------------------------------------------------------

func TestVerifyImageRefusals(t *testing.T) {
	t.Run("empty content list", func(t *testing.T) {
		dir := t.TempDir()
		st := newMemStore()
		st.lists[1] = ContentList{} // a never-written image: zero rows
		_, err := VerifyImage(context.Background(), Deps{Store: st}, VerifyInput{SetID: "S", ImageID: 1, Path: dir})
		if err == nil || !strings.Contains(err.Error(), "no finalized content list") {
			t.Fatalf("err = %v, want the empty-list refusal", err)
		}
	})
	t.Run("missing location", func(t *testing.T) {
		_, err := VerifyImage(context.Background(), Deps{Store: newMemStore()}, VerifyInput{SetID: "S", ImageID: 1, Path: ""})
		if err == nil || !strings.Contains(err.Error(), "no location to verify") {
			t.Fatalf("err = %v, want the empty-path refusal", err)
		}
	})
	t.Run("gone location", func(t *testing.T) {
		st := newMemStore()
		st.lists[1] = ContentList{Rows: []ContentRow{{MediaPath: "files/a.txt", Kind: media.KindFile, ChunkIndex: -1, Size: 1, Hash: hashOfBytes([]byte("x"))}}}
		_, err := VerifyImage(context.Background(), Deps{Store: st}, VerifyInput{
			SetID: "S", ImageID: 1, Path: filepath.Join(t.TempDir(), "deleted.iso"),
		})
		if err == nil || !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("err = %v, want the missing-location error", err)
		}
	})
	t.Run("ISO without a tool", func(t *testing.T) {
		iso := filepath.Join(t.TempDir(), "x.iso")
		if err := os.WriteFile(iso, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		st := newMemStore()
		st.lists[1] = ContentList{Rows: []ContentRow{{MediaPath: "files/a.txt", Kind: media.KindFile, ChunkIndex: -1, Size: 1, Hash: hashOfBytes([]byte("x"))}}}
		_, err := VerifyImage(context.Background(), Deps{Store: st}, VerifyInput{SetID: "S", ImageID: 1, Path: iso})
		if err == nil || !strings.Contains(err.Error(), "needs xorriso") {
			t.Fatalf("err = %v, want the nil-tool wiring refusal", err)
		}
	})
	t.Run("pre-cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		dir := t.TempDir()
		st := newMemStore()
		st.lists[1] = ContentList{Rows: []ContentRow{{MediaPath: "files/a.txt", Kind: media.KindFile, ChunkIndex: -1, Size: 1, Hash: hashOfBytes([]byte("x"))}}}
		_, err := VerifyImage(ctx, Deps{Store: st}, VerifyInput{SetID: "S", ImageID: 1, Path: dir})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
		if st.stampCount() != 0 {
			t.Error("a cancelled verification must never stamp")
		}
	})
}

// TestVerifyImageStampFailureIsNonFatalCarveOut is the FLIPPED pin (it
// used to be the "stamp failure is loud" subtest above, which pinned a
// deviation from the architect's ruling): DESIGN_SPEC 2.1, Clarified
// 2026-10-21 - "a stamp that ever exceeded busy_timeout is non-fatal:
// the content result still reports verified, and a later verify
// re-stamps" - applies to ANY stamp write failure. So: NO error, the
// already-computed content verdict SHIPS (Comparison intact), Stamped
// stays false, and the failure surfaces only as a WARNING
// (progress.Warn on the bus + StampWarning on the result) - never an
// anomaly classification, never a failed image. Zero stamps are
// recorded, so the row stays stampable by a LATER verify (the recovery
// clause, asserted at the end).
func TestVerifyImageStampFailureIsNonFatalCarveOut(t *testing.T) {
	f := makeCopyTreeFixture(t)
	st := newMemStore()
	st.lists[1] = f.list
	st.stampErr = errors.New("stamp write refused")

	w := &syncBuffer{}
	bus := progress.NewBus()
	wait := progress.Start(progress.New(progress.Options{Mode: progress.ModePlain, Stderr: w}), bus)
	res, err := VerifyImage(context.Background(), Deps{Store: st, Bus: bus},
		VerifyInput{SetID: "S-V", ImageID: 1, Path: f.root})
	bus.Finish(progress.Done{Summary: "done"})
	wait()

	if err != nil {
		t.Fatalf("VerifyImage: a stamp failure must NOT be an error (2.1 carve-out 2026-10-21): %v", err)
	}
	// THE CONTENT VERDICT SHIPS: full match, every listed row matched.
	if !res.Comparison.Clean() {
		t.Errorf("comparison problems = %v, want the intact full-match verdict", res.Comparison.Problems())
	}
	if len(res.Comparison.Matched) != len(f.list.Rows) {
		t.Errorf("matched %d entries, want all %d (the content verdict ships)", len(res.Comparison.Matched), len(f.list.Rows))
	}
	if len(res.Comparison.Missing)+len(res.Comparison.Unexpected)+len(res.Comparison.Unreadable) != 0 {
		t.Errorf("anomalies = (missing %d, unexpected %d, unreadable %d), want none - a stamp failure classifies nothing",
			len(res.Comparison.Missing), len(res.Comparison.Unexpected), len(res.Comparison.Unreadable))
	}
	// NO stamp claimed.
	if res.Stamped || res.StampAt != 0 {
		t.Errorf("stamped = (%v, %d), want (false, 0): a failed stamp is never claimed", res.Stamped, res.StampAt)
	}
	// THE WARNING, on the result and on the progress stream.
	for _, want := range []string{"matched its content list fully", "stamp write refused", "later verify re-stamps"} {
		if !strings.Contains(res.StampWarning, want) {
			t.Errorf("StampWarning = %q, want it to contain %q", res.StampWarning, want)
		}
	}
	if out := w.String(); !strings.Contains(out, "warning: "+res.StampWarning) {
		t.Errorf("progress stream lacks the progress.Warn line:\n%s", out)
	}
	// Nothing was written: zero stamps recorded.
	if st.stampCount() != 0 {
		t.Errorf("stamps = %+v, want none (the failed write recorded nothing)", st.stamps)
	}

	// THE RE-Stamp CLAIM: with the store healed, the SAME image stamps
	// on the next verify (the failed write never blocked the row).
	st.stampErr = nil
	res2, err := VerifyImage(context.Background(), Deps{Store: st},
		VerifyInput{SetID: "S-V", ImageID: 1, Path: f.root})
	if err != nil {
		t.Fatalf("VerifyImage (later verify): %v", err)
	}
	if !res2.Stamped || res2.StampWarning != "" {
		t.Errorf("later verify = (stamped %v, warning %q), want a clean stamp with no warning", res2.Stamped, res2.StampWarning)
	}
	if st.stampCount() != 1 {
		t.Errorf("stamps = %d, want exactly 1 (the later verify re-stamps)", st.stampCount())
	}
}

// TestVerifyMediaProgressPhases pins the determinate progress of both
// modes through the plain renderer (the step-clock pattern):
//   - copy tree: ONE verify(hash) phase, exact totals, then ItemDone
//     per hashed file;
//   - ISO: verify(extract) then verify(hash) - never "build(...)"
//     labels.
func TestVerifyMediaProgressPhases(t *testing.T) {
	f := makeCopyTreeFixture(t)

	// Copy-tree mode.
	st := newMemStore()
	st.lists[1] = f.list
	w := &syncBuffer{}
	clk := &stepClock{}
	bus := progress.NewBus()
	wait := progress.Start(progress.New(progress.Options{Mode: progress.ModePlain, Stderr: w, Now: clk.Now}), bus)
	_, err := VerifyImage(context.Background(), Deps{Store: st, Bus: bus}, VerifyInput{
		SetID: "S-V", ImageID: 1, Path: f.root,
	})
	if err != nil {
		t.Fatalf("VerifyImage: %v", err)
	}
	bus.Finish(progress.Done{Summary: "done"})
	wait()
	out := w.String()
	if !strings.Contains(out, "verify(hash): ") {
		t.Errorf("copy-tree stream lacks the verify(hash) phase:\n%s", out)
	}
	if strings.Contains(out, "build(") {
		t.Errorf("verify-media must never render build(...) phase labels:\n%s", out)
	}
	tot := TotalsOf(f.list.Rows)
	if !strings.Contains(out, "/"+strconv.FormatInt(tot.Files, 10)+", ") ||
		!strings.Contains(out, "/"+units.FormatBytes(tot.Bytes)+" (") {
		t.Errorf("verify(hash) line lacks the exact content-list totals (%d files, %d bytes):\n%s",
			tot.Files, tot.Bytes, out)
	}

	// ISO mode (fake tool): both phases, in order.
	prepared := filepath.Join(t.TempDir(), "prepared")
	if err := os.MkdirAll(filepath.Join(prepared, "files"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prepared, "files", "a.txt"), f.payload, 0o644); err != nil {
		t.Fatal(err)
	}
	toolPath := installVerifyFakeTool(t, `cp -r "`+prepared+`/." "$7"`)
	iso := filepath.Join(t.TempDir(), "S-V-001.iso")
	if err := os.WriteFile(iso, []byte("fake iso"), 0o644); err != nil {
		t.Fatal(err)
	}
	rows := []ContentRow{{MediaPath: "files/a.txt", Kind: media.KindFile, ChunkIndex: -1,
		Size: int64(len(f.payload)), Hash: hashOfBytes(f.payload)}}
	st2 := newMemStore()
	st2.lists[1] = ContentList{Rows: rows}
	w2 := &syncBuffer{}
	clk2 := &stepClock{}
	bus2 := progress.NewBus()
	wait2 := progress.Start(progress.New(progress.Options{Mode: progress.ModePlain, Stderr: w2, Now: clk2.Now}), bus2)
	if _, err := VerifyImage(context.Background(), Deps{Store: st2, Bus: bus2, Tool: toolWith(toolPath)}, VerifyInput{
		SetID: "S-V", ImageID: 1, Path: iso, WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("VerifyImage (ISO): %v", err)
	}
	bus2.Finish(progress.Done{Summary: "done"})
	wait2()
	out2 := w2.String()
	iExtract := strings.Index(out2, PhaseMediaExtract+": ")
	iHash := strings.Index(out2, PhaseMediaHash+": ")
	if iExtract < 0 || iHash < 0 || iExtract > iHash {
		t.Errorf("ISO stream lacks verify(extract) before verify(hash):\n%s", out2)
	}
}

// --- helpers ----------------------------------------------------------------------------

// assertDirEmpty fails when dir still holds any entry (the no-leaked-
// scratch contract).
func assertDirEmpty(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("%s still holds %v after the run (leaked scratch)", dir, names)
	}
}
