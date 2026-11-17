package copyengine

// T3.3: the writer-core test matrix (DESIGN_SPEC 5.1 ordering,
// never-mark-before-verify, source-changed classification, write-fault
// retry, 4.3 first-write chunk hashing, 5.2 mirror (via the fake
// store), crash recovery, cancellation, the 12.7 hook seam, and the
// T3.4 specials' minimal contracts).
//
// Every test drives the REAL engine against a real temp filesystem
// (t.TempDir) with the seams injected: a wrapped SourceFS, a fake
// Store that records marks, an instrumented partial writer, an
// instrumented read-back opener, a recording sleep, and the hook. The
// only fake is the CATALOG (the Store) and the fault seams; the file
// discipline - partial, fsync, rename, dir fsync, read-back - is the
// production code every time.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"isopress/internal/fsmeta"
)

// --- fixtures ----------------------------------------------------------------

// fakeStore is the recording Store: marks land in maps, and any
// injected error fires first (store-failure paths).
type fakeStore struct {
	mu sync.Mutex

	fileCopied  map[int64]string         // fileID -> mediaPath
	chunkCopied map[int64]map[int]string // fileID -> index -> mediaPath
	chunkHash   map[int64]map[int][]byte // fileID -> index -> stored hash
	dirContent  map[int64]string         // dirID -> mediaPath

	fileErr error
	txLog   []string // ordered mark log, for same-call assertions
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		fileCopied:  map[int64]string{},
		chunkCopied: map[int64]map[int]string{},
		chunkHash:   map[int64]map[int][]byte{},
		dirContent:  map[int64]string{},
	}
}

func (s *fakeStore) SetFileCopied(_ context.Context, fileID int64, mediaPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fileErr != nil {
		return s.fileErr
	}
	s.fileCopied[fileID] = mediaPath
	s.txLog = append(s.txLog, fmt.Sprintf("file:%d", fileID))
	return nil
}

func (s *fakeStore) SetChunkCopied(_ context.Context, fileID int64, index int, hash []byte, mediaPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.chunkCopied[fileID] == nil {
		s.chunkCopied[fileID] = map[int]string{}
		s.chunkHash[fileID] = map[int][]byte{}
	}
	s.chunkCopied[fileID][index] = mediaPath
	if len(hash) > 0 {
		s.chunkHash[fileID][index] = append([]byte(nil), hash...)
	}
	s.txLog = append(s.txLog, fmt.Sprintf("chunk:%d#%d", fileID, index))
	return nil
}

func (s *fakeStore) SetChunkHash(_ context.Context, fileID int64, index int, hash []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.chunkHash[fileID] == nil {
		s.chunkHash[fileID] = map[int][]byte{}
	}
	s.chunkHash[fileID][index] = append([]byte(nil), hash...)
	return nil
}

func (s *fakeStore) SetDirContent(_ context.Context, dirID int64, mediaPath string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dirContent[dirID] = mediaPath
	return nil
}

// testFile is one source file prepared for a copy test.
type testFile struct {
	path    string
	content []byte
	meta    fsmeta.Meta
}

func makeSource(t *testing.T, name string, content []byte) testFile {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatalf("writing source %s: %v", path, err)
	}
	meta, err := fsmeta.PathMeta(path)
	if err != nil {
		t.Fatalf("lstat source %s: %v", path, err)
	}
	return testFile{path: path, content: content, meta: meta}
}

// sha256Of is the digest helper (Sum256 returns an array value that
// cannot be sliced inline).
func sha256Of(b []byte) []byte {
	h := sha256.Sum256(b)
	return h[:]
}

// wholeItem builds the Item for a whole-file copy of src.
func wholeItem(src testFile, fileID int64, dest string) Item {
	return Item{
		Kind: ItemWholeFile, FileID: fileID, RelPath: filepath.Base(src.path),
		MediaPath: "files/" + filepath.Base(src.path), SourcePath: src.path, DestPath: dest,
		SourceSize: src.meta.Size, MtimeNS: src.meta.MtimeNS,
		Dev: int64(src.meta.Dev), Inode: int64(src.meta.Inode),
		Size: src.meta.Size, Hash: sha256Of(src.content),
		Mode: 0o644,
	}
}

// chunkItem builds the Item for ONE chunk of src.
func chunkItem(src testFile, fileID int64, index int, offset, length int64, storedHash []byte, dest string) Item {
	return Item{
		Kind: ItemChunk, FileID: fileID, RelPath: filepath.Base(src.path),
		MediaPath:  fmt.Sprintf("chunks/%s/%05d", filepath.Base(src.path), index+1),
		SourcePath: src.path, DestPath: dest,
		SourceSize: src.meta.Size, MtimeNS: src.meta.MtimeNS,
		Dev: int64(src.meta.Dev), Inode: int64(src.meta.Inode),
		Size: length, Offset: offset, Hash: storedHash, ChunkIndex: index,
	}
}

// --- the 5.1 ordering --------------------------------------------------------

// A wrapped source + instrumented partial + hook + read-back + store
// record the EXACT step sequence of a clean copy: lstat-compare,
// partial-write, fsync, close, rename, dir-fsync, read-back, mark -
// the DESIGN_SPEC 5.1 order, pinned seam by seam.
func TestCopyItemStrictOrderingSequence(t *testing.T) {
	src := makeSource(t, "alpha.txt", []byte("hello 5.1 ordering"))
	store := newFakeStore()
	var steps []string
	deps := Deps{
		Store: store,
		Source: &hookedSource{
			inner:   OSSource{},
			onLstat: func(string) { steps = append(steps, "lstat") },
		},
		OpenPartial: func(path string) (WriteSyncCloser, error) {
			steps = append(steps, "open-partial")
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return nil, err
			}
			return &hookedWriter{inner: f, onWrite: func() { steps = append(steps, "write") },
				onSync:  func() { steps = append(steps, "fsync") },
				onClose: func() { steps = append(steps, "close") }}, nil
		},
		ReadBack: func(path string) (io.ReadCloser, error) {
			steps = append(steps, "read-back")
			return os.Open(path)
		},
		Hook: func(_ context.Context, ev HookEvent) error {
			switch ev.Kind {
			case HookAfterPartialWrite:
				steps = append(steps, "hook:partial-written")
			case HookAfterRename:
				steps = append(steps, "hook:renamed")
			case HookAfterDirSync:
				steps = append(steps, "hook:dir-synced")
			}
			return nil
		},
	}
	dest := filepath.Join(t.TempDir(), "tree", "files", "alpha.txt")
	res := CopyItem(context.Background(), deps, wholeItem(src, 1, dest))
	if res.Outcome != OutcomeCopied {
		t.Fatalf("outcome = %v, want Copied (err: %v)", res.Outcome, res.Err)
	}
	want := []string{
		"lstat", "open-partial", "write", "hook:partial-written", "fsync", "close",
		"hook:renamed", "hook:dir-synced", "read-back",
		// The mark is LAST: the store records it after every seam above.
	}
	if strings.Join(steps, ",") != strings.Join(want, ",") {
		t.Fatalf("step sequence mismatch:\n got: %v\nwant: %v", steps, want)
	}
	if len(store.txLog) != 1 || store.txLog[0] != "file:1" {
		t.Fatalf("mark log = %v, want exactly [file:1] AFTER the verify seams", store.txLog)
	}
	got, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, src.content) {
		t.Fatalf("destination content = %q (%v), want %q", got, err, src.content)
	}
}

type hookedSource struct {
	inner   OSSource
	onLstat func(string)
}

func (h *hookedSource) Lstat(path string) (fsmeta.Meta, error) {
	if h.onLstat != nil {
		h.onLstat(path)
	}
	return h.inner.Lstat(path)
}
func (h *hookedSource) Open(path string, offset, length int64) (io.ReadCloser, error) {
	return h.inner.Open(path, offset, length)
}

type hookedWriter struct {
	inner   WriteSyncCloser
	onWrite func()
	onSync  func()
	onClose func()
}

func (w *hookedWriter) Write(p []byte) (int, error) {
	if w.onWrite != nil {
		w.onWrite()
	}
	return w.inner.Write(p)
}
func (w *hookedWriter) Sync() error {
	if w.onSync != nil {
		w.onSync()
	}
	return w.inner.Sync()
}
func (w *hookedWriter) Close() error {
	if w.onClose != nil {
		w.onClose()
	}
	return w.inner.Close()
}

// --- never-mark-before-verify ------------------------------------------------

// An instrumented read-back that ALWAYS mismatches (the injected
// verify fault): zero marks, zero final-name files surviving (write
// faults delete the final), exactly the retry budget consumed.
func TestCopyItemNeverMarksBeforeVerify(t *testing.T) {
	src := makeSource(t, "rotten.txt", []byte("payload the NAS ate"))
	store := newFakeStore()
	var attempts int
	deps := Deps{
		Store: store,
		ReadBack: func(path string) (io.ReadCloser, error) {
			attempts++
			return io.NopCloser(strings.NewReader("corrupted beyond recognition")), nil
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	dest := filepath.Join(t.TempDir(), "files", "rotten.txt")
	res := CopyItem(context.Background(), deps, wholeItem(src, 7, dest))
	if res.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %v, want Failed", res.Outcome)
	}
	if res.Attempts != MaxAttempts {
		t.Fatalf("attempts = %d, want the full budget %d", res.Attempts, MaxAttempts)
	}
	if len(store.txLog) != 0 {
		t.Fatalf("marks recorded despite verify mismatch: %v (the INVARIANT is never-mark-before-verify)", store.txLog)
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatalf("final file %s survived a verify mismatch (err=%v); write faults delete it", dest, err)
	}
	if _, err := os.Lstat(PartialPath(dest)); !os.IsNotExist(err) {
		t.Fatalf("partial %s survived a verify mismatch (err=%v)", PartialPath(dest), err)
	}
}

// A verify mismatch on the FIRST attempt followed by a clean read-back
// marks exactly once - the retry path re-verifies and re-marks, never
// double-marks.
func TestCopyItemVerifyMismatchThenRecoveryMarksOnce(t *testing.T) {
	src := makeSource(t, "flaky.txt", []byte("flaky NAS bytes"))
	store := newFakeStore()
	var reads int
	deps := Deps{
		Store: store,
		ReadBack: func(path string) (io.ReadCloser, error) {
			reads++
			if reads == 1 {
				return io.NopCloser(strings.NewReader("torn read")), nil
			}
			return os.Open(path)
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	dest := filepath.Join(t.TempDir(), "files", "flaky.txt")
	res := CopyItem(context.Background(), deps, wholeItem(src, 3, dest))
	if res.Outcome != OutcomeCopied || res.Attempts != 2 {
		t.Fatalf("outcome=%v attempts=%d, want Copied/2", res.Outcome, res.Attempts)
	}
	if len(store.txLog) != 1 {
		t.Fatalf("marks = %v, want exactly one", store.txLog)
	}
}

// --- source-changed classification ------------------------------------------

// A source mutated between the catalog read and the copy: step 1
// refuses it with a skip - no partial, no write, no mark, no retry.
func TestCopyItemSourceChangedAtStep1Skips(t *testing.T) {
	src := makeSource(t, "live.txt", []byte("original"))
	store := newFakeStore()
	var opened int
	deps := Deps{
		Store:  store,
		Source: &hookedSource{inner: OSSource{}, onLstat: func(string) { opened++ }},
		OpenPartial: func(path string) (WriteSyncCloser, error) {
			t.Fatal("step 1 must refuse a changed source BEFORE any write")
			return nil, nil
		},
	}
	// Mutate: rewrite content AND mtime so the fixed field set fires.
	if err := os.WriteFile(src.path, []byte("mutated"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "files", "live.txt")
	res := CopyItem(context.Background(), deps, wholeItem(src, 5, dest))
	if res.Outcome != OutcomeSkipped {
		t.Fatalf("outcome = %v, want Skipped (err %v)", res.Outcome, res.Err)
	}
	if !res.Diff.Any() || res.Diff&fsmeta.DiffSize == 0 {
		t.Fatalf("diff = %s, want at least a size difference", res.Diff)
	}
	if opened != 1 {
		t.Fatalf("lstat count = %d, want exactly the step-1 check", opened)
	}
	if len(store.txLog) != 0 {
		t.Fatalf("marks recorded for a skipped item: %v", store.txLog)
	}
}

// The source changes BETWEEN step 1 and the read-back: step 7's
// re-lstat classifies source-changed (skip), NOT a write fault - one
// attempt only, final deleted, nothing marked.
func TestCopyItemSourceChangedAtVerifySkips(t *testing.T) {
	src := makeSource(t, "moving.txt", []byte("the world moved mid-copy"))
	store := newFakeStore()
	var lstats int
	deps := Deps{
		Store:  store,
		Source: &hookedSource{inner: OSSource{}, onLstat: func(string) { lstats++ }},
		ReadBack: func(string) (io.ReadCloser, error) {
			// The destination verify FAILS (torn NAS read) AND the
			// source changes underneath: step 7 must classify
			// source-changed, not a write fault.
			if err := os.WriteFile(src.path, []byte("changed after step 1"), 0o644); err != nil {
				t.Fatal(err)
			}
			return io.NopCloser(strings.NewReader("torn read")), nil
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	dest := filepath.Join(t.TempDir(), "files", "moving.txt")
	res := CopyItem(context.Background(), deps, wholeItem(src, 6, dest))
	if res.Outcome != OutcomeSkipped {
		t.Fatalf("outcome = %v, want Skipped (err %v)", res.Outcome, res.Err)
	}
	if res.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 (source-changed is a skip, not a retried fault)", res.Attempts)
	}
	if lstats != 2 {
		t.Fatalf("lstat count = %d, want 2 (step 1 + step 7 classification)", lstats)
	}
	if len(store.txLog) != 0 {
		t.Fatalf("marks recorded for a source-changed skip: %v", store.txLog)
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatalf("final file survived a source-changed classification (err=%v)", err)
	}
}

// A vanished source (deleted between scan and copy) is an
// unreadable-class FAILURE at step 1: recorded, run-continuable, no
// retries.
func TestCopyItemMissingSourceFails(t *testing.T) {
	src := makeSource(t, "gone.txt", []byte("soon to be deleted"))
	store := newFakeStore()
	if err := os.Remove(src.path); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "files", "gone.txt")
	res := CopyItem(context.Background(), Deps{Store: store}, wholeItem(src, 9, dest))
	if res.Outcome != OutcomeFailed || res.Attempts != 1 {
		t.Fatalf("outcome=%v attempts=%d, want Failed/1", res.Outcome, res.Attempts)
	}
	if !strings.Contains(res.Err.Error(), "lstat source") {
		t.Errorf("error %q must name the lstat step", res.Err)
	}
}

// --- write-fault retry (5.1 step 7) -----------------------------------------

// A failing partial writer on attempts 1 and 2, success on 3: the item
// completes with ONE mark, the backoff is slept between attempts, and
// the outcome records the full attempt count.
func TestCopyItemWriteFaultRetriesThenSucceeds(t *testing.T) {
	src := makeSource(t, "retry.txt", []byte("transient NAS fault"))
	store := newFakeStore()
	var fails, sleeps int
	deps := Deps{
		Store: store,
		OpenPartial: func(path string) (WriteSyncCloser, error) {
			if fails < 2 {
				fails++
				return nil, errors.New("injected EIO on the partial open")
			}
			return openPartialOS(path)
		},
		Sleep: func(_ context.Context, d time.Duration) error {
			sleeps++
			if d <= 0 {
				t.Errorf("backoff %d must be positive", d)
			}
			return nil
		},
	}
	dest := filepath.Join(t.TempDir(), "files", "retry.txt")
	res := CopyItem(context.Background(), deps, wholeItem(src, 11, dest))
	if res.Outcome != OutcomeCopied || res.Attempts != 3 {
		t.Fatalf("outcome=%v attempts=%d, want Copied/3", res.Outcome, res.Attempts)
	}
	if len(store.txLog) != 1 {
		t.Fatalf("marks = %v, want exactly one (the successful attempt)", store.txLog)
	}
	if sleeps != 2 {
		t.Fatalf("backoff sleeps = %d, want 2 (between the three attempts)", sleeps)
	}
}

// A persistent fault across the whole budget: failure recorded after
// MaxAttempts, nothing marked, nothing left behind - the run continues
// over this item's result.
func TestCopyItemWriteFaultExhausted(t *testing.T) {
	src := makeSource(t, "dead.txt", []byte("permanently broken destination"))
	store := newFakeStore()
	deps := Deps{
		Store: store,
		OpenPartial: func(string) (WriteSyncCloser, error) {
			return nil, errors.New("injected ENOSPC forever")
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	dest := filepath.Join(t.TempDir(), "files", "dead.txt")
	res := CopyItem(context.Background(), deps, wholeItem(src, 12, dest))
	if res.Outcome != OutcomeFailed || res.Attempts != MaxAttempts {
		t.Fatalf("outcome=%v attempts=%d, want Failed/%d", res.Outcome, res.Attempts, MaxAttempts)
	}
	if len(store.txLog) != 0 {
		t.Fatalf("marks recorded for an exhausted write fault: %v", store.txLog)
	}
	if !strings.Contains(res.Err.Error(), "injected ENOSPC") {
		t.Errorf("failure must carry the last cause: %v", res.Err)
	}
}

// A store failure (mark rejected) is an honest item failure with the
// VERIFIED file left in place: deleting verified data because a SQL
// statement failed would destroy what the flag protects.
func TestCopyItemStoreFailureKeepsVerifiedFile(t *testing.T) {
	src := makeSource(t, "sql.txt", []byte("verified but unmarkable"))
	store := newFakeStore()
	store.fileErr = errors.New("injected catalog failure")
	dest := filepath.Join(t.TempDir(), "files", "sql.txt")
	res := CopyItem(context.Background(), Deps{Store: store}, wholeItem(src, 13, dest))
	if res.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %v, want Failed", res.Outcome)
	}
	got, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, src.content) {
		t.Fatalf("verified content was deleted on a store failure (err=%v)", err)
	}
}

// --- chunks: the 4.3 lazy first-write capture --------------------------------

// A first-write chunk (hash nil): the STORED hash equals the sha256 of
// the source byte range, and the read-back was compared against that
// captured value - proven by corrupting the read-back, which must
// produce a write fault with NO hash stored and NO mark.
func TestChunkFirstWriteCapturesStreamHash(t *testing.T) {
	content := []byte("0123456789abcdefCHUNK-CAPTURE")
	src := makeSource(t, "big.bin", content)
	store := newFakeStore()
	offset, length := int64(10), int64(12) // "abcdefCHUNK"
	wantHash := sha256.Sum256(content[offset : offset+length])

	dest := filepath.Join(t.TempDir(), "chunks", "big.bin", "00001")
	res := CopyItem(context.Background(), Deps{Store: store},
		chunkItem(src, 21, 0, offset, length, nil, dest))
	if res.Outcome != OutcomeCopied {
		t.Fatalf("outcome = %v, want Copied (err %v)", res.Outcome, res.Err)
	}
	stored := store.chunkHash[21][0]
	if !bytes.Equal(stored, wantHash[:]) {
		t.Fatalf("stored chunk hash = %x, want the source-stream capture %x", stored, wantHash)
	}
	if !bytes.Equal(res.Anchor, wantHash[:]) {
		t.Fatalf("Result.Anchor = %x, want the captured hash %x", res.Anchor, wantHash)
	}
	if store.chunkCopied[21][0] == "" {
		t.Fatal("chunk was never marked copied")
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, content[offset:offset+length]) {
		t.Fatalf("chunk content = %q, want the [%d:%d) window", got, offset, offset+length)
	}

	// The corruption half: a read-back mismatch on a first write must
	// NOT store the capture (the anchor is only persisted with a
	// verified mark).
	store2 := newFakeStore()
	dest2 := filepath.Join(t.TempDir(), "chunks", "big.bin", "00002")
	deps2 := Deps{
		Store:    store2,
		ReadBack: func(string) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("nope")), nil },
		Sleep:    func(context.Context, time.Duration) error { return nil },
	}
	res2 := CopyItem(context.Background(), deps2, chunkItem(src, 22, 0, offset, length, nil, dest2))
	if res2.Outcome != OutcomeFailed {
		t.Fatalf("corrupted first write outcome = %v, want Failed", res2.Outcome)
	}
	if len(store2.chunkHash[22]) != 0 || len(store2.chunkCopied[22]) != 0 {
		t.Fatalf("a failed first write stored a hash or mark: %v / %v", store2.chunkHash, store2.chunkCopied)
	}
}

// A re-copied chunk verifies against its STORED hash: the item carries
// the anchor, the read-back is compared against it, and the mark
// re-asserts it.
func TestChunkRecopyVerifiesStoredHash(t *testing.T) {
	content := []byte("stored-anchor bytes for re-copy")
	src := makeSource(t, "recopy.bin", content)
	store := newFakeStore()
	stored := sha256.Sum256(content)
	dest := filepath.Join(t.TempDir(), "chunks", "recopy.bin", "00001")
	res := CopyItem(context.Background(), Deps{Store: store},
		chunkItem(src, 31, 0, 0, int64(len(content)), stored[:], dest))
	if res.Outcome != OutcomeCopied {
		t.Fatalf("outcome = %v, want Copied (err %v)", res.Outcome, res.Err)
	}
	if !bytes.Equal(store.chunkHash[31][0], stored[:]) {
		t.Fatalf("stored hash = %x, want the re-asserted stored anchor %x", store.chunkHash[31][0], stored)
	}

	// Wrong stored anchor vs actual bytes: write fault, nothing marked.
	store2 := newFakeStore()
	dest2 := filepath.Join(t.TempDir(), "chunks", "recopy.bin", "00002")
	wrong := sha256.Sum256([]byte("something else entirely"))
	res2 := CopyItem(context.Background(), Deps{
		Store: store2, Sleep: func(context.Context, time.Duration) error { return nil },
	}, chunkItem(src, 32, 0, 0, int64(len(content)), wrong[:], dest2))
	if res2.Outcome != OutcomeFailed {
		t.Fatalf("mismatched stored anchor outcome = %v, want Failed", res2.Outcome)
	}
	if len(store2.chunkCopied[32]) != 0 {
		t.Fatal("mismatched chunk was marked anyway")
	}
}

// --- crash recovery and resume ---------------------------------------------

// A stale .partial left by an earlier crash is deleted at the next
// write of that item, and the item copies cleanly (5.1 "detected at
// the next copy run, deleted, and the file re-copied").
func TestCopyItemRemovesStalePartial(t *testing.T) {
	src := makeSource(t, "resume.txt", []byte("fresh content after crash"))
	store := newFakeStore()
	dest := filepath.Join(t.TempDir(), "files", "resume.txt")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PartialPath(dest), []byte("stale half-written garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := CopyItem(context.Background(), Deps{Store: store}, wholeItem(src, 41, dest))
	if res.Outcome != OutcomeCopied {
		t.Fatalf("outcome = %v, want Copied (err %v)", res.Outcome, res.Err)
	}
	got, _ := os.ReadFile(dest)
	if !bytes.Equal(got, src.content) {
		t.Fatalf("destination = %q, want the fresh content (stale partial must not leak)", got)
	}
	if _, err := os.Lstat(PartialPath(dest)); !os.IsNotExist(err) {
		t.Fatalf("stale partial survived the completed copy (err=%v)", err)
	}
}

// Cancellation mid-write: the in-flight item aborts (never marks), the
// partial is cleaned up best-effort, and the cancellation is
// distinguishable from a write fault (no retries, no backoff).
func TestCopyItemCancelledMidWriteCleansPartial(t *testing.T) {
	src := makeSource(t, "huge.bin", bytes.Repeat([]byte("x"), 256*1024))
	store := newFakeStore()
	ctx, cancel := context.WithCancel(context.Background())
	deps := Deps{
		Store: store,
		OpenPartial: func(path string) (WriteSyncCloser, error) {
			f, err := openPartialOS(path)
			if err != nil {
				return nil, err
			}
			// Cancel the run the moment the first bytes land: the
			// ctx-aware reader aborts the stream at the next Read.
			return &cancelAfterFirstWrite{WriteSyncCloser: f, cancel: cancel}, nil
		},
	}
	dest := filepath.Join(t.TempDir(), "files", "huge.bin")
	res := CopyItem(ctx, deps, wholeItem(src, 42, dest))
	if res.Outcome != OutcomeAborted {
		t.Fatalf("outcome = %v, want Aborted (err %v)", res.Outcome, res.Err)
	}
	if len(store.txLog) != 0 {
		t.Fatalf("marks recorded for an aborted item: %v", store.txLog)
	}
	if _, err := os.Lstat(PartialPath(dest)); !os.IsNotExist(err) {
		t.Fatalf("partial survived the abort (err=%v); it must be cleaned best-effort", err)
	}
}

type cancelAfterFirstWrite struct {
	WriteSyncCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelAfterFirstWrite) Write(p []byte) (int, error) {
	n, err := c.WriteSyncCloser.Write(p)
	c.once.Do(c.cancel)
	return n, err
}

// The 12.7 hook seam: an ErrHookAbort from the hook aborts the item
// WITHOUT the write-fault retry loop, and the sentinel stays
// discoverable for the pipeline's terminal-abort decision.
func TestCopyItemHookAbortIsTerminal(t *testing.T) {
	src := makeSource(t, "hooked.txt", []byte("hook target"))
	store := newFakeStore()
	var hookCalls int
	deps := Deps{
		Store: store,
		Hook: func(context.Context, HookEvent) error {
			hookCalls++
			return fmt.Errorf("hook fired: %w", ErrHookAbort)
		},
	}
	dest := filepath.Join(t.TempDir(), "files", "hooked.txt")
	res := CopyItem(context.Background(), deps, wholeItem(src, 43, dest))
	if res.Outcome != OutcomeAborted {
		t.Fatalf("outcome = %v, want Aborted", res.Outcome)
	}
	if !errors.Is(res.Err, ErrHookAbort) {
		t.Fatalf("error %v must wrap ErrHookAbort", res.Err)
	}
	if hookCalls != 1 {
		t.Fatalf("hook calls = %d, want 1 (hook errors are never retried)", hookCalls)
	}
	if len(store.txLog) != 0 {
		t.Fatalf("marks recorded after a hook abort: %v", store.txLog)
	}
}

// A nil Store fails the item loudly before any filesystem effect: no
// verified-unmarked writes can ever be produced by a misconfigured
// pipeline.
func TestCopyItemNilStoreFailsLoudly(t *testing.T) {
	src := makeSource(t, "nostore.txt", []byte("x"))
	dest := filepath.Join(t.TempDir(), "files", "nostore.txt")
	res := CopyItem(context.Background(), Deps{}, wholeItem(src, 44, dest))
	if res.Outcome != OutcomeFailed || !strings.Contains(res.Err.Error(), "nil Store") {
		t.Fatalf("outcome=%v err=%v, want Failed naming the nil Store", res.Outcome, res.Err)
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatal("a nil-Store item must write nothing")
	}
}

// A malformed item (no catalog hash - the unreadable-class row shape)
// is refused before any write.
func TestCopyItemRefusesAnchorlessWholeFile(t *testing.T) {
	src := makeSource(t, "anchorless.txt", []byte("no hash recorded"))
	item := wholeItem(src, 45, filepath.Join(t.TempDir(), "files", "anchorless.txt"))
	item.Hash = nil
	res := CopyItem(context.Background(), Deps{Store: newFakeStore()}, item)
	if res.Outcome != OutcomeFailed || !strings.Contains(res.Err.Error(), "no catalog hash") {
		t.Fatalf("outcome=%v err=%v, want Failed naming the missing anchor", res.Outcome, res.Err)
	}
}

// Nil-bus safety: the engine's progress emission is optional (the
// Deps.Bus seam is nil-safe), so a service wiring without a bus - and
// every unit test - runs the identical code path. Event SEMANTICS
// (ItemDone per completed item, Warn per skip, Error per failure) are
// pinned by the renderers' tests plus the outcome tests above, which
// drive the same emission sites.
func TestCopyItemNilBusIsSafe(t *testing.T) {
	src := makeSource(t, "progress.txt", []byte("count me"))
	res := CopyItem(context.Background(), Deps{Store: newFakeStore()}, wholeItem(src, 46,
		filepath.Join(t.TempDir(), "files", "progress.txt")))
	if res.Outcome != OutcomeCopied {
		t.Fatalf("nil-bus copy failed: %v", res.Err)
	}
}

// --- the T3.4 specials (minimal contracts; T3.4 owns the semantics) ---------

// WriteSymlink recreates valid AND dangling targets verbatim, marks
// the row, and never follows the link.
func TestWriteSymlinkValidAndDangling(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
	}{
		{"valid", "alpha.txt"},
		{"dangling", "does/not/exist.txt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			dest := filepath.Join(t.TempDir(), "files", "link")
			res := WriteSymlink(context.Background(), Deps{Store: store}, SymlinkItem{
				FileID: 51, RelPath: "link", MediaPath: "files/link", DestPath: dest,
				LinkTarget: tc.target, Mode: 0o777, MtimeNS: time.Now().UnixNano(),
			})
			if res.Outcome != OutcomeCopied {
				t.Fatalf("outcome = %v, want Copied (err %v)", res.Outcome, res.Err)
			}
			got, err := os.Readlink(dest)
			if err != nil || got != tc.target {
				t.Fatalf("readlink = %q (%v), want %q", got, err, tc.target)
			}
			if store.fileCopied[51] != "files/link" {
				t.Fatalf("marks = %v, want file 51 marked", store.fileCopied)
			}
			// Idempotent present-unmarked resume: same target is fine.
			res2 := WriteSymlink(context.Background(), Deps{Store: store}, SymlinkItem{
				FileID: 51, RelPath: "link", MediaPath: "files/link", DestPath: dest,
				LinkTarget: tc.target,
			})
			if res2.Outcome != OutcomeCopied {
				t.Fatalf("re-run outcome = %v, want Copied", res2.Outcome)
			}
		})
	}
}

// A foreign file at a symlink's destination is never clobbered.
func TestWriteSymlinkRefusesClobber(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "files", "occupied")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := WriteSymlink(context.Background(), Deps{Store: newFakeStore()}, SymlinkItem{
		FileID: 52, RelPath: "occupied", DestPath: dest, LinkTarget: "x",
	})
	if res.Outcome != OutcomeFailed || !strings.Contains(res.Err.Error(), "refusing to clobber") {
		t.Fatalf("outcome=%v err=%v, want a clobber refusal", res.Outcome, res.Err)
	}
}

// WriteEmptyDir creates the dir with recorded metadata and records the
// image_contents 'dir' row; it is idempotent (2.3 mkdir semantics).
func TestWriteEmptyDirCreatesAndRecords(t *testing.T) {
	store := newFakeStore()
	dest := filepath.Join(t.TempDir(), "files", "empty-dir")
	mode := uint32(0o750)
	res := WriteEmptyDir(context.Background(), Deps{Store: store}, DirItem{
		DirID: 61, RelPath: "empty-dir", MediaPath: "files/empty-dir", DestPath: dest,
		Mode: mode, MtimeNS: time.Now().UnixNano(),
	})
	if res.Outcome != OutcomeCopied {
		t.Fatalf("outcome = %v, want Copied (err %v)", res.Outcome, res.Err)
	}
	fi, err := os.Lstat(dest)
	if err != nil || !fi.IsDir() {
		t.Fatalf("directory not created (err=%v)", err)
	}
	if fi.Mode().Perm() != os.FileMode(mode).Perm() {
		t.Fatalf("dir mode = %04o, want %04o (recorded mode, FR-037)", fi.Mode().Perm(), mode)
	}
	if store.dirContent[61] != "files/empty-dir" {
		t.Fatalf("dir content rows = %v, want dir 61 recorded", store.dirContent)
	}
	res2 := WriteEmptyDir(context.Background(), Deps{Store: store}, DirItem{
		DirID: 61, RelPath: "empty-dir", MediaPath: "files/empty-dir", DestPath: dest, Mode: mode,
	})
	if res2.Outcome != OutcomeCopied {
		t.Fatalf("re-run outcome = %v, want Copied (mkdir idempotent)", res2.Outcome)
	}
}

// LinkSecondary creates a link(2) partner sharing the primary's inode,
// verifies that identity on read-back, and marks the secondary row.
func TestLinkSecondaryInodeIdentity(t *testing.T) {
	primary := makeSource(t, "family.txt", []byte("shared family content"))
	primaryDest := filepath.Join(t.TempDir(), "files", "family.txt")
	if err := os.MkdirAll(filepath.Dir(primaryDest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFileForTest(primary.path, primaryDest); err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()
	dest := filepath.Join(t.TempDir(), "files", "family-alias.txt")
	res := LinkSecondary(context.Background(), Deps{Store: store}, LinkItem{
		FileID: 71, RelPath: "family-alias.txt", MediaPath: "files/family-alias.txt",
		DestPath: dest, PrimaryDestPath: primaryDest, Size: primary.meta.Size,
	})
	if res.Outcome != OutcomeCopied {
		t.Fatalf("outcome = %v, want Copied (err %v)", res.Outcome, res.Err)
	}
	p, err1 := os.Lstat(primaryDest)
	s, err2 := os.Lstat(dest)
	if err1 != nil || err2 != nil || !os.SameFile(p, s) {
		t.Fatalf("secondary does not share the primary's inode (err %v/%v)", err1, err2)
	}
	if store.fileCopied[71] == "" {
		t.Fatal("secondary row never marked")
	}
}

func copyFileForTest(src, dest string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dest, b, 0o644)
}

// EmbedFile: the published member equals the source byte-for-byte,
// carries the correct hash/size, and a corrupted read-back fails
// loudly with nothing published.
func TestEmbedFileVerifyDiscipline(t *testing.T) {
	deps := Deps{}
	srcBytes := []byte("MANIFEST-ish bytes\n")
	srcPath := filepath.Join(t.TempDir(), "MANIFEST.json")
	if err := os.WriteFile(srcPath, srcBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "image", "MANIFEST.json")
	hash, size, err := EmbedFile(context.Background(), deps, srcPath, dest)
	if err != nil {
		t.Fatalf("EmbedFile: %v", err)
	}
	if want := sha256.Sum256(srcBytes); !bytes.Equal(hash, want[:]) {
		t.Fatalf("hash = %x, want %x", hash, want)
	}
	if size != int64(len(srcBytes)) {
		t.Fatalf("size = %d, want %d", size, len(srcBytes))
	}

	badDeps := Deps{
		ReadBack: func(string) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("torn")), nil
		},
	}
	dest2 := filepath.Join(t.TempDir(), "image2", "catalog-export.sqlite")
	if _, _, err := EmbedFile(context.Background(), badDeps, srcPath, dest2); err == nil {
		t.Fatal("EmbedFile with a corrupted read-back must fail")
	}
	if _, err := os.Lstat(dest2); !os.IsNotExist(err) {
		t.Fatalf("corrupted embed published a file (err=%v)", err)
	}
}

// The default ranged source reader honors offset/length exactly (the
// chunk window contract) and refuses a short range with a short stream
// (the engine turns that into the honest length-mismatch classification).
func TestOSSourceRangedOpen(t *testing.T) {
	src := makeSource(t, "window.bin", []byte("0123456789"))
	rc, err := OSSource{}.Open(src.path, 3, 4)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(got) != "3456" {
		t.Fatalf("ranged read = %q (%v), want \"3456\"", got, err)
	}
}

// The engine's chunk write of an out-of-bounds window fails as a
// length mismatch (the source shrank between catalog read and copy) -
// the honest classification the caller's re-lstat then skips.
func TestChunkShortWindowFails(t *testing.T) {
	src := makeSource(t, "short.bin", []byte("tiny"))
	store := newFakeStore()
	dest := filepath.Join(t.TempDir(), "chunks", "short.bin", "00001")
	res := CopyItem(context.Background(), Deps{
		Store: store, Sleep: func(context.Context, time.Duration) error { return nil },
	}, chunkItem(src, 81, 0, 0, 1024, nil, dest)) // window larger than the file
	if res.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %v, want Failed (length mismatch)", res.Outcome)
	}
	if !strings.Contains(res.Err.Error(), "payload length mismatch") {
		t.Errorf("error %q must name the length mismatch", res.Err)
	}
	if len(store.txLog) != 0 {
		t.Fatalf("marks recorded for a short window: %v", store.txLog)
	}
}

// The backoff defaults are positive and increasing (5.1 step 7).
func TestDefaultBackoffGrows(t *testing.T) {
	if defaultBackoff(1) <= 0 || defaultBackoff(2) <= defaultBackoff(1) {
		t.Fatalf("backoff must be positive and increasing: %v, %v", defaultBackoff(1), defaultBackoff(2))
	}
}
