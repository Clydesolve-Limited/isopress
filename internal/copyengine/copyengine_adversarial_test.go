package copyengine

// T3.3 adversarial depth pass (the delegated verification brief): the
// data-writing core behaviors that are documented but were not pinned
// by copyengine_test.go. Every test drives the REAL engine against a
// real temp filesystem; fakes are confined to the catalog seam (Store)
// and the fault seams (Source, ReadBack, OpenPartial, Hook).
//
// Gaps closed here, one per audit item of the brief:
//
//   - DIR-FSYNC precision: the EINVAL tolerance is exercised through
//     REAL code (fsync(2) on a char device returns EINVAL), and a
//     NON-tolerance errno at the same call sites is proven to be a
//     hard item failure - the tolerance never masks a real fs error.
//   - RENAME-ONTO-EXISTING: a stale final at the destination (the
//     crash-after-rename state, or debris) is REPLACED atomically,
//     never refused; a DIRECTORY occupying the final path fails
//     honestly with no clobber.
//   - LYING NAS read-back: SHORT data on a stable source exhausts the
//     retry budget and leaves NO corrupt final, no partial, no mark.
//   - LYING SOURCE stream (short bytes, stable lstat): honest
//     write-fault classification, zero residue.
//   - First-write chunk capture under RETRY: the stored hash is the
//     deterministic source-stream capture (attempt N == attempt 1 for
//     a stable source), and the engine never routes through
//     SetChunkHash's never-overwrites-differing refusal (it stores
//     via SetChunkCopied with the verified anchor only).
//   - THE 10.1 DETACHED-MARK WINDOW (SIGINT between verify-pass and
//     mark): the mark completes on a fresh Background-derived context
//     with the 5s bound; a mark that misses the bound leaves the
//     verified final UNMARKED and the NEXT run re-verifies and
//     re-marks - both outcomes converge to the same consistent
//     catalog across repeated runs.
//   - THE SPECIALS' DETACHED-MARK WINDOW (INFO-5, fix round): the
//     same detached treatment covers WriteSymlink and LinkSecondary -
//     a SIGINT in their verify->mark window completes the mark (fresh
//     5s-bound Background descendant) instead of recording a Failed
//     item under an interrupted run; flipCtx is the deterministic
//     stand-in for the signal landing between the specials' entry
//     check and the mark.
//   - 5.4 METADATA WARNING at the engine level: an EPERM chown
//     downgrade is a per-file Warning on a COPIED item - it never
//     unmarks and never fails the content.
//   - SYMLINK SPECIAL collisions: a pre-existing DIRECTORY and a
//     pre-existing symlink with a DIFFERENT target both refuse
//     (never clobber) with the foreign entry untouched.
//   - EmbedFile DISK FULL mid-write: nothing published, the partial
//     is cleaned, and a previously published member survives the
//     failed re-embed (verified data is never deleted by a failed
//     refresh).
//   - In-process CONCURRENCY: CopyItem shares no mutable state across
//     calls (race-detector pinned by `go test -race` over the
//     package; this test also asserts logical correctness).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"isopress/internal/fsmeta"
)

// --- dir-fsync precision (5.1 step 4) ----------------------------------------

// The syncDir tolerance is EXACTLY EINVAL/ENOTSUP and nothing else.
// Both halves run through the production syncDir:
//
//   - tolerance: fsync(2) on a character device has no fsync method
//     and returns EINVAL - syncDir must swallow precisely this;
//   - propagation: opening a unix socket with open(2) fails ENXIO - a
//     NON-tolerance errno at the same entry point - and must surface,
//     not be swallowed. (EIO at the Sync stage is the same shape: the
//     predicate below the tolerance is "any error that is not EINVAL
//     and not ENOTSUP", so EIO is a hard error by construction; the
//     ENXIO case pins the plumbing through the real function because
//     EIO cannot be provoked without broken hardware.)
func TestSyncDirToleranceIsErrnoPrecise(t *testing.T) {
	if _, err := os.Lstat("/dev/null"); err != nil {
		t.Skipf("/dev/null unavailable: %v", err)
	}
	if err := syncDir("/dev/null"); err != nil {
		t.Fatalf("syncDir must tolerate the char-device EINVAL, got: %v", err)
	}

	sock := filepath.Join(t.TempDir(), "sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix socket listener unavailable: %v", err)
	}
	defer l.Close()
	if err := syncDir(sock); err == nil {
		t.Fatal("syncDir swallowed a non-tolerance error (ENXIO opening a socket)")
	}

	dir := t.TempDir()
	if err := syncDir(dir); err != nil {
		t.Fatalf("syncDir of a real directory: %v", err)
	}
	if err := syncDir(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("syncDir of a missing directory must fail")
	}
}

// A dir-fsync FAILURE through the production attempt path (step 4):
// the hook removes the containing directory between the rename and the
// dir fsync, so syncDir's open fails with ENOENT - a NON-tolerance
// errno. The item must classify as a write fault (retried), fail
// after the budget, and leave NO final, NO partial, NO mark. This
// pins "the dir-fsync error path never masks a real error" at the
// engine level: a real errno at that step is a hard item failure.
func TestCopyItemDirSyncFailureIsHardItemFailure(t *testing.T) {
	src := makeSource(t, "dirsync.txt", []byte("payload the dir fsync ate"))
	store := newFakeStore()
	var hookFires int
	deps := Deps{
		Store: store,
		Sleep: func(context.Context, time.Duration) error { return nil },
		Hook: func(_ context.Context, ev HookEvent) error {
			if ev.Kind == HookAfterRename {
				hookFires++
				if err := os.RemoveAll(filepath.Dir(ev.Item.DestPath)); err != nil {
					t.Fatalf("sabotaging the containing directory: %v", err)
				}
			}
			return nil
		},
	}
	dest := filepath.Join(t.TempDir(), "tree", "files", "dirsync.txt")
	res := CopyItem(context.Background(), deps, wholeItem(src, 91, dest))
	if res.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %v, want Failed (err %v)", res.Outcome, res.Err)
	}
	if !strings.Contains(res.Err.Error(), "fsync directory") {
		t.Fatalf("error %q must name the dir-fsync step (a real errno there is NOT tolerated)", res.Err)
	}
	if hookFires != MaxAttempts {
		t.Fatalf("hook fires = %d, want %d (the dir-fsync failure is a retried write fault, not an abort)", hookFires, MaxAttempts)
	}
	if len(store.txLog) != 0 {
		t.Fatalf("marks recorded despite dir-fsync failure: %v", store.txLog)
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatalf("final survived a dir-fsync failure (err=%v)", err)
	}
	if _, err := os.Lstat(PartialPath(dest)); !os.IsNotExist(err) {
		t.Fatalf("partial survived a dir-fsync failure (err=%v)", err)
	}
}

// --- rename-onto-existing (5.1 step 4) ---------------------------------------

// The present-at-rename cases: a stale final (the crash-after-rename
// state of 5.1, or debris) is REPLACED atomically by the verified
// bytes - rename over a regular file never refuses - and the stale
// .partial alongside it is cleaned. The item marks exactly once.
func TestCopyItemRenameOverExistingFinalReplaces(t *testing.T) {
	src := makeSource(t, "replace.txt", []byte("fresh verified bytes"))
	store := newFakeStore()
	dest := filepath.Join(t.TempDir(), "files", "replace.txt")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("stale final from a prior crashed run"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PartialPath(dest), []byte("stale partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := CopyItem(context.Background(), Deps{Store: store}, wholeItem(src, 92, dest))
	if res.Outcome != OutcomeCopied {
		t.Fatalf("outcome = %v, want Copied (a stale final must be replaced, not refused; err %v)", res.Outcome, res.Err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, src.content) {
		t.Fatalf("destination = %q (%v), want the fresh verified bytes", got, err)
	}
	if _, err := os.Lstat(PartialPath(dest)); !os.IsNotExist(err) {
		t.Fatalf("stale partial survived the completed copy (err=%v)", err)
	}
	if len(store.txLog) != 1 {
		t.Fatalf("marks = %v, want exactly one", store.txLog)
	}
}

// The stale-final chain the brief asks about: an earlier verified copy
// left its FINAL at the destination (crash-before-mark: present but
// unmarked), and the SOURCE then changed. The source-changed skip of
// step 1 has NO filesystem effect - the stale final stays present and
// UNMARKED (never trusted by the resume model), and the run that
// finally copies the RE-SCANNED record REPLACES it atomically via
// rename. A stale final is therefore either ignored-untrusted media
// or overwritten - never accepted as the new record's bytes, never
// refused.
func TestCopyItemSourceChangedSkipLeavesStaleFinalForNextRun(t *testing.T) {
	src := makeSource(t, "stale-final.txt", []byte("original verified bytes"))
	store := newFakeStore()
	dest := filepath.Join(t.TempDir(), "files", "stale-final.txt")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	// The crash-before-mark state: the verified final of an EARLIER
	// run sits at the destination, its flag never set.
	if err := os.WriteFile(dest, src.content, 0o644); err != nil {
		t.Fatal(err)
	}
	// The source changes AFTER that stale copy: the item still carries
	// the OLD catalog snapshot (src.meta), so step 1 must skip.
	if err := os.WriteFile(src.path, []byte("the source moved on"), 0o644); err != nil {
		t.Fatal(err)
	}
	res1 := CopyItem(context.Background(), Deps{Store: store}, wholeItem(src, 143, dest))
	if res1.Outcome != OutcomeSkipped {
		t.Fatalf("run 1 outcome = %v, want Skipped (err %v)", res1.Outcome, res1.Err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, src.content) {
		t.Fatalf("the skip touched the stale final: %q (%v)", got, err)
	}
	if len(store.txLog) != 0 {
		t.Fatalf("marks recorded for a source-changed skip: %v", store.txLog)
	}

	// The re-scan: the catalog now records the NEW source. The next run
	// overwrites the stale final with the verified new bytes.
	mutated := testFile{
		path:    src.path,
		content: []byte("the source moved on"),
	}
	meta, err := fsmeta.PathMeta(mutated.path)
	if err != nil {
		t.Fatal(err)
	}
	mutated.meta = meta
	res2 := CopyItem(context.Background(), Deps{Store: store}, wholeItem(mutated, 143, dest))
	if res2.Outcome != OutcomeCopied {
		t.Fatalf("run 2 outcome = %v, want Copied (rename replaces the stale final; err %v)", res2.Outcome, res2.Err)
	}
	got, err = os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, mutated.content) {
		t.Fatalf("run 2 content = %q (%v), want the new verified bytes", got, err)
	}
	if store.fileCopied[143] == "" {
		t.Fatal("run 2 never marked")
	}
}

// The hostile present-at-rename case: a DIRECTORY occupies the final
// path. rename(2) of a file onto a directory fails (EISDIR/ENOTDIR) -
// the item must fail honestly after the write-fault budget, never
// clobber the directory tree, and never mark.
func TestCopyItemFinalPathDirectoryFailsHonestly(t *testing.T) {
	src := makeSource(t, "dirvictim.txt", []byte("cannot land here"))
	store := newFakeStore()
	dest := filepath.Join(t.TempDir(), "files", "dirvictim.txt")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dest, "precious.txt")
	if err := os.WriteFile(marker, []byte("do not delete me"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := CopyItem(context.Background(), Deps{
		Store: store, Sleep: func(context.Context, time.Duration) error { return nil },
	}, wholeItem(src, 93, dest))
	if res.Outcome != OutcomeFailed || res.Attempts != MaxAttempts {
		t.Fatalf("outcome=%v attempts=%d, want Failed/%d", res.Outcome, res.Attempts, MaxAttempts)
	}
	if !strings.Contains(res.Err.Error(), "renaming") {
		t.Fatalf("error %q must name the rename step", res.Err)
	}
	if len(store.txLog) != 0 {
		t.Fatalf("marks recorded when the final path was a directory: %v", store.txLog)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "do not delete me" {
		t.Fatalf("the directory at the final path was clobbered (err=%v)", err)
	}
	if _, err := os.Lstat(PartialPath(dest)); !os.IsNotExist(err) {
		t.Fatalf("partial survived the rename failures (err=%v)", err)
	}
}

// --- lying NAS and lying source (5.1 step 5/7) -------------------------------

// The read-back RETURNS SHORT DATA (a torn NAS read of the final file)
// while the source is stable: the mismatch is a WRITE FAULT, retried
// to the full budget, and the destination ends with NO corrupt final,
// NO partial, NO mark - exactly the state the never-mark-before-verify
// invariant demands after an exhausted retry.
func TestCopyItemShortReadBackExhaustsRetriesLeavesNoFinal(t *testing.T) {
	content := []byte("the whole truth lives in this payload")
	src := makeSource(t, "shortread.bin", content)
	store := newFakeStore()
	deps := Deps{
		Store: store,
		ReadBack: func(path string) (io.ReadCloser, error) {
			f, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			// SHORT data: the first half only - a torn read, not a
			// different file. Its sha256 cannot match the anchor.
			return io.NopCloser(io.LimitReader(f, int64(len(content)/2))), nil
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	dest := filepath.Join(t.TempDir(), "files", "shortread.bin")
	res := CopyItem(context.Background(), deps, wholeItem(src, 94, dest))
	if res.Outcome != OutcomeFailed || res.Attempts != MaxAttempts {
		t.Fatalf("outcome=%v attempts=%d, want Failed/%d", res.Outcome, res.Attempts, MaxAttempts)
	}
	if !strings.Contains(res.Err.Error(), "read-back hash mismatch") {
		t.Fatalf("error %q must name the read-back mismatch", res.Err)
	}
	if len(store.txLog) != 0 {
		t.Fatalf("marks recorded despite short read-back: %v", store.txLog)
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatalf("a CORRUPT final was left behind after the retry budget (err=%v); write faults delete it", err)
	}
	if _, err := os.Lstat(PartialPath(dest)); !os.IsNotExist(err) {
		t.Fatalf("partial survived (err=%v)", err)
	}
}

// lyingSource reports a STABLE lstat (the catalog comparison always
// matches) but its Open stream lies about the payload length - the
// "source shrank between lstat and read" NAS anomaly with the re-lstat
// unable to see it. The honest classification is the write-fault
// class: the budget is consumed, nothing is marked, nothing remains.
type lyingSource struct {
	meta    fsmeta.Meta
	payload []byte
}

func (s lyingSource) Lstat(string) (fsmeta.Meta, error) { return s.meta, nil }
func (s lyingSource) Open(string, int64, int64) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.payload)), nil
}

func TestCopyItemLyingShortSourceStreamFailsClean(t *testing.T) {
	content := []byte("0123456789abcdefghijklmnopqrstuvwxyz")
	src := makeSource(t, "lyingsrc.bin", content)
	store := newFakeStore()
	deps := Deps{
		Store:  store,
		Source: lyingSource{meta: src.meta, payload: content[:10]}, // 10 of 36 bytes
		Sleep:  func(context.Context, time.Duration) error { return nil },
	}
	dest := filepath.Join(t.TempDir(), "files", "lyingsrc.bin")
	res := CopyItem(context.Background(), deps, wholeItem(src, 95, dest))
	if res.Outcome != OutcomeFailed || res.Attempts != MaxAttempts {
		t.Fatalf("outcome=%v attempts=%d, want Failed/%d", res.Outcome, res.Attempts, MaxAttempts)
	}
	if !strings.Contains(res.Err.Error(), "payload length mismatch") {
		t.Fatalf("error %q must name the length mismatch", res.Err)
	}
	if len(store.txLog) != 0 {
		t.Fatalf("marks recorded for a lying source stream: %v", store.txLog)
	}
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatalf("final survived a lying source stream (err=%v)", err)
	}
	if _, err := os.Lstat(PartialPath(dest)); !os.IsNotExist(err) {
		t.Fatalf("partial survived a lying source stream (err=%v)", err)
	}
}

// --- first-write capture under retry (4.3 + 5.1 step 7) ----------------------

// countingStore wraps the fake store and records the mark-context
// observations the detached-mark tests need, plus a SetChunkHash call
// counter and a fail-once file-mark error.
type countingStore struct {
	inner *fakeStore

	mu                sync.Mutex
	setChunkHashCalls int
	markCtxErrs       []error // ctx.Err() observed at each mark call
	markCtxDeadlines  []bool  // whether the mark ctx carried a deadline

	failFileMarkOnce error
}

func newCountingStore() *countingStore {
	return &countingStore{inner: newFakeStore()}
}

func (s *countingStore) record(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.markCtxErrs = append(s.markCtxErrs, ctx.Err())
	_, hasDeadline := ctx.Deadline()
	s.markCtxDeadlines = append(s.markCtxDeadlines, hasDeadline)
	if s.failFileMarkOnce != nil {
		err := s.failFileMarkOnce
		s.failFileMarkOnce = nil
		return err
	}
	return nil
}

func (s *countingStore) SetFileCopied(ctx context.Context, fileID int64, mediaPath string) error {
	if err := s.record(ctx); err != nil {
		return err
	}
	return s.inner.SetFileCopied(ctx, fileID, mediaPath)
}

func (s *countingStore) SetChunkCopied(ctx context.Context, fileID int64, index int, hash []byte, mediaPath string) error {
	if err := s.record(ctx); err != nil {
		return err
	}
	return s.inner.SetChunkCopied(ctx, fileID, index, hash, mediaPath)
}

func (s *countingStore) SetChunkHash(_ context.Context, _ int64, _ int, _ []byte) error {
	// Count-only: the writer's mark path stores first-write captures
	// through SetChunkCopied; a call here means the engine reached the
	// standalone (never-overwrites-differing) surface.
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setChunkHashCalls++
	return nil
}

func (s *countingStore) SetDirContent(ctx context.Context, dirID int64, mediaPath string) error {
	if err := s.record(ctx); err != nil {
		return err
	}
	return s.inner.SetDirContent(ctx, dirID, mediaPath)
}

// A first-write chunk whose FIRST attempt hits a read-back fault and
// whose second attempt succeeds: the STORED hash is the deterministic
// source-stream capture (attempt 2 hashes the same stable bytes as
// attempt 1, so the value equals the sha256 of the source range), the
// item marks exactly once, and the engine NEVER routes through
// SetChunkHash - the store it uses (SetChunkCopied) takes the verified
// anchor in the same transaction as the flag, so the
// never-overwrites-differing refusal is structurally unreachable for a
// retry (both attempts capture identical bytes, and a differing anchor
// would have failed the read-back comparison first).
func TestChunkRetryStoresDeterministicCapture(t *testing.T) {
	content := []byte("0123456789abcdefRETRY-CAPTURE-CHUNK")
	src := makeSource(t, "retry.bin", content)
	store := newCountingStore()
	offset, length := int64(4), int64(16)
	want := sha256.Sum256(content[offset : offset+length])

	var reads int
	deps := Deps{
		Store: store,
		ReadBack: func(path string) (io.ReadCloser, error) {
			reads++
			if reads == 1 {
				return io.NopCloser(strings.NewReader("torn NAS read")), nil
			}
			return os.Open(path)
		},
		Sleep: func(context.Context, time.Duration) error { return nil },
	}
	dest := filepath.Join(t.TempDir(), "chunks", "retry.bin", "00001")
	res := CopyItem(context.Background(), deps, chunkItem(src, 96, 0, offset, length, nil, dest))
	if res.Outcome != OutcomeCopied || res.Attempts != 2 {
		t.Fatalf("outcome=%v attempts=%d, want Copied/2 (err %v)", res.Outcome, res.Attempts, res.Err)
	}
	stored := store.inner.chunkHash[96][0]
	if !bytes.Equal(stored, want[:]) {
		t.Fatalf("stored hash after retry = %x, want the deterministic source-stream capture %x", stored, want)
	}
	if !bytes.Equal(res.Anchor, want[:]) {
		t.Fatalf("Result.Anchor = %x, want %x", res.Anchor, want)
	}
	if len(store.inner.txLog) != 1 {
		t.Fatalf("marks = %v, want exactly one (the successful attempt)", store.inner.txLog)
	}
	store.mu.Lock()
	hashCalls := store.setChunkHashCalls
	store.mu.Unlock()
	if hashCalls != 0 {
		t.Fatalf("SetChunkHash was called %d times; the writer stores first-write captures through SetChunkCopied, never the standalone refusal-prone surface", hashCalls)
	}
}

// --- the 10.1 detached-mark window (SIGINT between verify and mark) ----------

// SIGINT lands AFTER the read-back passed (inside the verify open) and
// BEFORE the mark: the 10.1 "finishes the in-flight item's marking
// transaction" clause means the mark STILL RUNS - on a FRESH
// Background-derived context with the 5s bound (the cancelled run ctx
// would be rejected by the driver). The store observes a LIVE context
// carrying a deadline, and the item completes as Copied.
func TestCopyItemCancelAfterVerifyMarksDetached(t *testing.T) {
	src := makeSource(t, "detach.txt", []byte("verified before the signal"))
	store := newCountingStore()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps := Deps{
		Store: store,
		ReadBack: func(path string) (io.ReadCloser, error) {
			// SIGINT/SIGTERM lands here: bytes are on disk, the run
			// ctx dies, but the verify read and the mark must finish.
			cancel()
			return os.Open(path)
		},
	}
	dest := filepath.Join(t.TempDir(), "files", "detach.txt")
	res := CopyItem(ctx, deps, wholeItem(src, 97, dest))
	if res.Outcome != OutcomeCopied {
		t.Fatalf("outcome = %v, want Copied (the in-flight mark must finish detached; err %v)", res.Outcome, res.Err)
	}
	store.mu.Lock()
	errs, deadlines := append([]error(nil), store.markCtxErrs...), append([]bool(nil), store.markCtxDeadlines...)
	store.mu.Unlock()
	if len(errs) != 1 || errs[0] != nil {
		t.Fatalf("mark ctx observations = %v, want exactly one LIVE context", errs)
	}
	if len(deadlines) != 1 || !deadlines[0] {
		t.Fatalf("mark ctx deadlines = %v, want the fresh 5s-bound Background descendant (not the cancelled run ctx)", deadlines)
	}
	if store.inner.fileCopied[97] == "" {
		t.Fatal("item never marked")
	}
	got, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, src.content) {
		t.Fatalf("final content = %q (%v), want the verified bytes", got, err)
	}
}

// The other side of the same window: the detached mark MISSES its 5s
// bound (a wedged slow-NAS SQL). The honest outcome: the item does
// NOT mark, the verified final is NOT deleted (deleting verified data
// because a SQL statement failed would destroy exactly what the flag
// protects), the run classifies the item as aborted (the run ctx is
// dead - the interruption dominates), and the NEXT run re-verifies and
// re-marks. Repeated runs converge to the same consistent end state:
// final-exists-unmarked OR marked-and-verified, never corrupt, never
// marked-unverified.
func TestCopyItemDetachedMarkTimeoutLeavesConsistentState(t *testing.T) {
	src := makeSource(t, "slowmark.txt", []byte("verified but the catalog was wedged"))
	store := newCountingStore()
	// Simulate the >5s timeout: the detached mark's statement fails
	// exactly once with the driver's deadline error.
	store.failFileMarkOnce = errors.New("context deadline exceeded (detached mark missed MarkDetachTimeout)")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	deps := Deps{
		Store: store,
		ReadBack: func(path string) (io.ReadCloser, error) {
			cancel() // the signal lands between verify-pass and mark
			return os.Open(path)
		},
	}
	dest := filepath.Join(t.TempDir(), "files", "slowmark.txt")

	// Run 1: the mark times out - aborted, final EXACTLY present and
	// verified, nothing marked (final-exists-unmarked, the other legal
	// crash state of 5.1).
	res1 := CopyItem(ctx, deps, wholeItem(src, 98, dest))
	if res1.Outcome != OutcomeAborted {
		t.Fatalf("run 1 outcome = %v, want Aborted (interrupted run + failed detached mark; err %v)", res1.Outcome, res1.Err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, src.content) {
		t.Fatalf("run 1 destroyed the verified final (err=%v); verified data is never deleted by a failed mark", err)
	}
	if store.inner.fileCopied[98] != "" {
		t.Fatal("run 1 marked despite the failed mark statement")
	}

	// Run 2 (the next copy run, fresh context): re-verify + re-mark
	// exactly as the trust model documents.
	res2 := CopyItem(context.Background(), Deps{Store: store}, wholeItem(src, 98, dest))
	if res2.Outcome != OutcomeCopied {
		t.Fatalf("run 2 outcome = %v, want Copied (err %v)", res2.Outcome, res2.Err)
	}
	if store.inner.fileCopied[98] == "" {
		t.Fatal("run 2 never marked")
	}
	got, err = os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, src.content) {
		t.Fatalf("run 2 content = %q (%v), want the verified bytes", got, err)
	}

	// Run 3 (the caller now trusts the flag and skips - pinned here at
	// the engine level as an idempotent no-corruption re-run): the end
	// state stays marked-and-verified.
	res3 := CopyItem(context.Background(), Deps{Store: store}, wholeItem(src, 98, dest))
	if res3.Outcome != OutcomeCopied {
		t.Fatalf("run 3 outcome = %v, want Copied (err %v)", res3.Outcome, res3.Err)
	}
	got, err = os.ReadFile(dest)
	if err != nil || !bytes.Equal(got, src.content) {
		t.Fatalf("run 3 corrupted the final (err=%v)", err)
	}
}

// --- the specials' detached-mark window (INFO-5: symlink + hard link) --------

// flipCtx simulates the SIGINT window the specials' detached-mark
// tests need (INFO-5): the run context is LIVE for the first Err()
// query - the specials' entry check, so the item HAD started - and
// CANCELLED for every query after it, as if the signal landed between
// that check and the mark (the link creation + read-back window; the
// specials expose no fault seam there, so the stateful context is the
// honest deterministic stand-in). It delegates everything else to a
// Background parent, so any ctx-aware plumbing still behaves.
type flipCtx struct {
	ctx     context.Context
	errSeen int32
}

func newFlipCtx() *flipCtx { return &flipCtx{ctx: context.Background()} }

func (c *flipCtx) Deadline() (time.Time, bool) { return c.ctx.Deadline() }
func (c *flipCtx) Done() <-chan struct{}       { return c.ctx.Done() }
func (c *flipCtx) Value(key any) any           { return c.ctx.Value(key) }

func (c *flipCtx) Err() error {
	if atomic.AddInt32(&c.errSeen, 1) == 1 {
		return nil // entry check: the item started while the run was live
	}
	return context.Canceled // the signal landed between entry and mark
}

// INFO-5, symlink half: a SIGINT in WriteSymlink's verify->mark
// window no longer strands a present-but-unmarked link under an
// interrupted run - the mark completes on a fresh Background-derived
// context with the MarkDetachTimeout bound (parity with CopyItem's
// content mark, markContext), and the item completes as Copied. The
// store observes a LIVE context carrying a deadline.
func TestWriteSymlinkCancelBetweenVerifyAndMarkDetached(t *testing.T) {
	store := newCountingStore()
	dest := filepath.Join(t.TempDir(), "files", "detach-link")
	res := WriteSymlink(newFlipCtx(), Deps{Store: store}, SymlinkItem{
		FileID: 151, RelPath: "detach-link", MediaPath: "files/detach-link",
		DestPath: dest, LinkTarget: "target.txt", Mode: 0o777,
	})
	if res.Outcome != OutcomeCopied {
		t.Fatalf("outcome = %v, want Copied (INFO-5: the post-verify mark must finish detached; err %v)", res.Outcome, res.Err)
	}
	store.mu.Lock()
	errs, deadlines := append([]error(nil), store.markCtxErrs...), append([]bool(nil), store.markCtxDeadlines...)
	store.mu.Unlock()
	if len(errs) != 1 || errs[0] != nil {
		t.Fatalf("mark ctx observations = %v, want exactly one LIVE context", errs)
	}
	if len(deadlines) != 1 || !deadlines[0] {
		t.Fatalf("mark ctx deadlines = %v, want the fresh 5s-bound Background descendant (INFO-5 parity with CopyItem)", deadlines)
	}
	if store.inner.fileCopied[151] != "files/detach-link" {
		t.Fatalf("marks = %v, want the symlink row marked", store.inner.fileCopied)
	}
	got, err := os.Readlink(dest)
	if err != nil || got != "target.txt" {
		t.Fatalf("readlink = %q (%v), want the recreated link", got, err)
	}
}

// INFO-5, hard-link half: same window in LinkSecondary - after the
// read-back verified inode identity, the mark completes detached and
// the secondary lands as Copied, sharing the primary's inode.
func TestLinkSecondaryCancelBetweenVerifyAndMarkDetached(t *testing.T) {
	primary := makeSource(t, "detach-family.txt", []byte("shared family bytes"))
	primaryDest := filepath.Join(t.TempDir(), "files", "detach-family.txt")
	if err := os.MkdirAll(filepath.Dir(primaryDest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFileForTest(primary.path, primaryDest); err != nil {
		t.Fatal(err)
	}
	store := newCountingStore()
	dest := filepath.Join(t.TempDir(), "files", "detach-family-alias.txt")
	res := LinkSecondary(newFlipCtx(), Deps{Store: store}, LinkItem{
		FileID: 152, RelPath: "detach-family-alias.txt", MediaPath: "files/detach-family-alias.txt",
		DestPath: dest, PrimaryDestPath: primaryDest, Size: primary.meta.Size,
	})
	if res.Outcome != OutcomeCopied {
		t.Fatalf("outcome = %v, want Copied (INFO-5: the post-verify mark must finish detached; err %v)", res.Outcome, res.Err)
	}
	store.mu.Lock()
	errs, deadlines := append([]error(nil), store.markCtxErrs...), append([]bool(nil), store.markCtxDeadlines...)
	store.mu.Unlock()
	if len(errs) != 1 || errs[0] != nil {
		t.Fatalf("mark ctx observations = %v, want exactly one LIVE context", errs)
	}
	if len(deadlines) != 1 || !deadlines[0] {
		t.Fatalf("mark ctx deadlines = %v, want the fresh 5s-bound Background descendant (INFO-5 parity with CopyItem)", deadlines)
	}
	if store.inner.fileCopied[152] != "files/detach-family-alias.txt" {
		t.Fatalf("marks = %v, want the secondary row marked", store.inner.fileCopied)
	}
	p, err1 := os.Lstat(primaryDest)
	s, err2 := os.Lstat(dest)
	if err1 != nil || err2 != nil || !os.SameFile(p, s) {
		t.Fatalf("secondary does not share the primary's inode (err %v/%v)", err1, err2)
	}
}

// --- 5.4 metadata-after-mark (engine half) -----------------------------------

// An EPERM chown downgrade (a non-root copy cannot chown to a foreign
// uid) is a per-file WARNING on a COPIED item: the content stays
// verified and marked, and the warning text surfaces in Result.Warning
// for the caller to count (5.4's "per-file warning" class).
func TestApplyMetadataEPERMIsWarningNeverUnmark(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("EPERM cannot be provoked as root")
	}
	src := makeSource(t, "ownme.txt", []byte("content wins over metadata"))
	store := newFakeStore()
	item := wholeItem(src, 99, filepath.Join(t.TempDir(), "files", "ownme.txt"))
	item.UID = 1 // a uid this non-root process cannot chown to
	res := CopyItem(context.Background(), Deps{Store: store}, item)
	if res.Outcome != OutcomeCopied {
		t.Fatalf("outcome = %v, want Copied (metadata failures are warnings; err %v)", res.Outcome, res.Err)
	}
	if !strings.Contains(res.Warning, "chown") {
		t.Fatalf("warning = %q, want the chown downgrade note", res.Warning)
	}
	if store.fileCopied[99] == "" {
		t.Fatal("a metadata warning unmarked the item; 5.4 forbids it")
	}
	got, err := os.ReadFile(item.DestPath)
	if err != nil || !bytes.Equal(got, src.content) {
		t.Fatalf("content = %q (%v), want the verified bytes", got, err)
	}
}

// --- symlink special collisions (D-03; T3.4 owns the semantics) -------------

// WriteSymlink refuses BOTH foreign occupants of the destination: a
// DIRECTORY (os.Symlink hits EEXIST and Readlink proves it is not the
// recorded link) and a pre-existing symlink with a DIFFERENT target.
// Neither case clobbers anything and neither marks.
func TestWriteSymlinkRefusesCollisions(t *testing.T) {
	t.Run("directory at path", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "files", "dirlink")
		if err := os.MkdirAll(dest, 0o755); err != nil {
			t.Fatal(err)
		}
		marker := filepath.Join(dest, "inside.txt")
		if err := os.WriteFile(marker, []byte("untouchable"), 0o644); err != nil {
			t.Fatal(err)
		}
		res := WriteSymlink(context.Background(), Deps{Store: newFakeStore()}, SymlinkItem{
			FileID: 141, RelPath: "dirlink", MediaPath: "files/dirlink",
			DestPath: dest, LinkTarget: "alpha.txt",
		})
		if res.Outcome != OutcomeFailed || !strings.Contains(res.Err.Error(), "refusing to clobber") {
			t.Fatalf("outcome=%v err=%v, want a clobber refusal", res.Outcome, res.Err)
		}
		if got, err := os.ReadFile(marker); err != nil || string(got) != "untouchable" {
			t.Fatalf("the directory occupant was modified (err=%v)", err)
		}
	})
	t.Run("differing target symlink", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "files", "relink")
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("old-target.txt", dest); err != nil {
			t.Fatal(err)
		}
		res := WriteSymlink(context.Background(), Deps{Store: newFakeStore()}, SymlinkItem{
			FileID: 142, RelPath: "relink", MediaPath: "files/relink",
			DestPath: dest, LinkTarget: "new-target.txt",
		})
		if res.Outcome != OutcomeFailed || !strings.Contains(res.Err.Error(), "refusing to clobber") {
			t.Fatalf("outcome=%v err=%v, want a clobber refusal", res.Outcome, res.Err)
		}
		got, err := os.Readlink(dest)
		if err != nil || got != "old-target.txt" {
			t.Fatalf("the foreign symlink was rewritten: readlink = %q (%v), want old-target.txt untouched", got, err)
		}
	})
}

// --- EmbedFile disk-full (5.3 members under the same discipline) -------------

// enospcWriter accepts the first write then fails every later one with
// a wrapped ENOSPC - the disk-full-mid-image injection.
type enospcWriter struct {
	WriteSyncCloser
	writes int
}

func (w *enospcWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == 1 {
		return w.WriteSyncCloser.Write(p)
	}
	return 0, fmt.Errorf("injected disk full: %w", syscall.ENOSPC)
}

// A disk-full during the MANIFEST/export embed mid-image: nothing is
// published (no final, the partial is cleaned), the error is honest -
// and a PREVIOUSLY published member at the same path survives the
// failed re-embed (verified media is never deleted by a failed
// refresh).
func TestEmbedFileDiskFullPublishesNothing(t *testing.T) {
	// Bigger than io.Copy's 32 KiB buffer: the disk-full lands on the
	// SECOND write, i.e. genuinely mid-stream.
	srcPath := filepath.Join(t.TempDir(), "MANIFEST.json")
	big := bytes.Repeat([]byte(`{"version":1,"entries":[]}`+"\n"), 4096)
	if err := os.WriteFile(srcPath, big, 0o644); err != nil {
		t.Fatal(err)
	}
	diskFull := Deps{
		OpenPartial: func(path string) (WriteSyncCloser, error) {
			f, err := openPartialOS(path)
			if err != nil {
				return nil, err
			}
			return &enospcWriter{WriteSyncCloser: f}, nil
		},
	}

	// Fresh destination: the embed fails, nothing lands.
	destFresh := filepath.Join(t.TempDir(), "img1", "MANIFEST.json")
	if _, _, err := EmbedFile(context.Background(), diskFull, srcPath, destFresh); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("EmbedFile disk-full error = %v, want the ENOSPC cause", err)
	}
	if _, err := os.Lstat(destFresh); !os.IsNotExist(err) {
		t.Fatalf("a failed embed published a final (err=%v)", err)
	}
	if _, err := os.Lstat(PartialPath(destFresh)); !os.IsNotExist(err) {
		t.Fatalf("a failed embed left a partial behind (err=%v)", err)
	}

	// Previously published member: a failed REFRESH must not delete
	// the verified bytes already on media.
	destExisting := filepath.Join(t.TempDir(), "img2", "MANIFEST.json")
	if err := os.MkdirAll(filepath.Dir(destExisting), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destExisting, []byte("PREVIOUSLY VERIFIED MANIFEST"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := EmbedFile(context.Background(), diskFull, srcPath, destExisting); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("re-embed disk-full error = %v, want the ENOSPC cause", err)
	}
	if got, err := os.ReadFile(destExisting); err != nil || string(got) != "PREVIOUSLY VERIFIED MANIFEST" {
		t.Fatalf("a failed re-embed destroyed the published member: %q (%v)", got, err)
	}
}

// --- in-process concurrency (statelessness) ----------------------------------

// CopyItem keeps no package-level mutable state: N concurrent items
// share ONE Deps VALUE (with a shared mutex-guarded store) and never
// interfere. The -race run of this package is the memory-model half;
// the logical half (every item verified, marked, and correct) stands
// on its own.
func TestCopyItemConcurrentCallsShareNoState(t *testing.T) {
	const n = 8
	store := newFakeStore() // mutex-guarded recording store
	deps := Deps{Store: store}
	base := t.TempDir()

	srcs := make([]testFile, n)
	dests := make([]string, n)
	for i := 0; i < n; i++ {
		body := bytes.Repeat([]byte{byte('a' + i)}, 1024+i)
		srcs[i] = makeSource(t, fmt.Sprintf("par-%02d.bin", i), body)
		dests[i] = filepath.Join(base, "files", fmt.Sprintf("par-%02d.bin", i))
	}

	results := make([]Result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = CopyItem(context.Background(), deps, wholeItem(srcs[i], int64(200+i), dests[i]))
		}(i)
	}
	wg.Wait()

	for i := 0; i < n; i++ {
		if results[i].Outcome != OutcomeCopied {
			t.Fatalf("item %d outcome = %v (err %v)", i, results[i].Outcome, results[i].Err)
		}
		got, err := os.ReadFile(dests[i])
		if err != nil || !bytes.Equal(got, srcs[i].content) {
			t.Fatalf("item %d content = %d bytes (%v), want %d bytes", i, len(got), err, len(srcs[i].content))
		}
	}
	store.mu.Lock()
	marks := len(store.txLog)
	store.mu.Unlock()
	if marks != n {
		t.Fatalf("marks = %d, want %d (every concurrent item marks exactly once)", marks, n)
	}
}
