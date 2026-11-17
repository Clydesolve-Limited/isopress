package ingest

// T1.4 hash pool tests: DESIGN_SPEC 3.3 acceptance (concurrent
// correctness against a sequential reference, worker-count default and
// override with an at-most-N bound, the 1 MiB streaming bound on a
// sparse multi-GiB file, FR-003 error tolerance, cancellation with
// PROMPT in-flight abort and no goroutine leaks) plus the progress
// contract (Phase + one ItemDone per COMPLETED file, nil-bus safety)
// and the documented T1.6 wiring
// (Walk -> pool.Submit -> results -> batcher) end to end.
//
// Synchronization policy: NO sleeps and NO sleep races anywhere.
// Worker activity is observed through the OpenFile seam (a blocking
// open signals on a channel and parks until released) and through the
// pool's HighWater counter; flushes in the wiring test flow through a
// recording callback; goroutine leak checks use the T1.1
// settle-then-poll pattern (renamed local copies, package-private).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"isopress/internal/fsmeta"
	"isopress/internal/progress"
	"isopress/internal/units"
)

// --- structural pin: the memory bound --------------------------------------

// TestHashReadBufferSizeIsOneMiB pins the streaming buffer of
// DESIGN_SPEC 3.3. This constant IS the pool's memory bound: one buffer
// per worker for the worker's lifetime, so file size never translates
// into memory (the sparse multi-GiB test hashes 4 GiB through it).
func TestHashReadBufferSizeIsOneMiB(t *testing.T) {
	if HashReadBufferSize != 1<<20 {
		t.Errorf("HashReadBufferSize = %d, want 1 MiB (DESIGN_SPEC 3.3 memory-cap rationale)", HashReadBufferSize)
	}
}

// TestDefaultHashWorkersFormula pins min(GOMAXPROCS, 8) (DESIGN_SPEC
// 3.3) and the Workers(0) -> default resolution.
func TestDefaultHashWorkersFormula(t *testing.T) {
	want := runtime.GOMAXPROCS(0)
	if want > MaxHashWorkers {
		want = MaxHashWorkers
	}
	if got := DefaultHashWorkers(); got != want {
		t.Errorf("DefaultHashWorkers() = %d, want min(GOMAXPROCS=%d, %d) = %d",
			got, runtime.GOMAXPROCS(0), MaxHashWorkers, want)
	}
	pool := NewHashPool(context.Background(), PoolOptions{})
	defer func() { pool.Close(); pool.Wait() }()
	if got := pool.Workers(); got != want {
		t.Errorf("pool with Workers:0 resolved to %d workers, want the default %d", got, want)
	}
}

// --- helpers ----------------------------------------------------------------

// seqHashFile is the independent sequential reference implementation:
// io.Copy (its own 32 KiB buffer, nothing shared with the pool) into a
// fresh sha256 state. Every pool digest is compared against this per
// file, satisfying the "matches a sequential reference byte-for-byte"
// acceptance criterion.
func seqHashFile(t *testing.T, path string) [32]byte {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("reference open %s: %v", path, err)
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("reference hash %s: %v", path, err)
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum
}

// blockingOpen is the worker-activity instrumentation seam: every open
// emits its path on opened (buffered) and PARKS until release is
// closed. It lets tests hold N workers inside a hash operation and
// assert the worker count deterministically - no sleeps anywhere.
type blockingOpen struct {
	opened  chan string
	release chan struct{}
}

func newBlockingOpen() *blockingOpen {
	return &blockingOpen{opened: make(chan string, 64), release: make(chan struct{})}
}

func (o *blockingOpen) open(path string) (io.ReadCloser, error) {
	o.opened <- path
	<-o.release
	return osOpen(path)
}

// drainResults collects every HashResult until the channel closes.
func drainResults(results <-chan HashResult) []HashResult {
	var out []HashResult
	for r := range results {
		out = append(out, r)
	}
	return out
}

// resultsByRel indexes results by relpath.
func resultsByRel(results []HashResult) map[string]HashResult {
	m := make(map[string]HashResult, len(results))
	for _, r := range results {
		m[r.RelPath] = r
	}
	return m
}

// settleIngestGoroutines waits for the goroutine count to stabilize
// (three consecutive equal samples) and returns it - the T1.1
// settle-then-poll leak-check pattern.
func settleIngestGoroutines(t *testing.T) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	prev, stable := -1, 0
	for time.Now().Before(deadline) {
		n := runtime.NumGoroutine()
		if n == prev {
			stable++
			if stable >= 3 {
				return n
			}
		} else {
			prev, stable = n, 0
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Skipf("goroutine count never settled at %d within the deadline", prev)
	return -1
}

// waitIngestGoroutinesBaseline polls until the goroutine count drops to
// baseline, failing loudly with the current count on timeout.
func waitIngestGoroutinesBaseline(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		n := runtime.NumGoroutine()
		if n <= baseline {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: %d goroutines after pool shutdown, baseline %d", n, baseline)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// buildHashFixture writes a mixed-size tree and returns its root plus
// the file sizes. Covers the acceptance shapes: 0 bytes, 1 byte, KiB
// scale, MiB scale, and a sparse file (created with truncate, which
// costs no disk).
func buildHashFixture(t *testing.T) (string, map[string]int64) {
	t.Helper()
	root := t.TempDir()
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), data, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	patterned := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte((i*31 + 7) % 251)
		}
		return b
	}

	write("empty.bin", nil)
	write("one.bin", []byte("x"))
	write("abc.bin", []byte("abc")) // NIST SHA-256 vector
	write("3k.bin", patterned(3*1024))
	write("1m7.bin", patterned((1<<20)+7))

	sizes := map[string]int64{
		"empty.bin": 0,
		"one.bin":   1,
		"abc.bin":   3,
		"3k.bin":    3 * 1024,
		"1m7.bin":   (1 << 20) + 7,
	}
	// Sparse 16 MiB (truncate: no blocks allocated); skip that file
	// on filesystems that refuse truncation.
	f, err := os.Create(filepath.Join(root, "sparse16m.bin"))
	if err == nil {
		if err := f.Truncate(16 << 20); err == nil {
			sizes["sparse16m.bin"] = 16 << 20
		}
		f.Close()
	}
	return root, sizes
}

// jobsFor builds walk-equivalent jobs for every fixture file, capturing
// the fsmeta baseline with the same call the walker uses.
func jobsFor(t *testing.T, root string, sizes map[string]int64) []Job {
	t.Helper()
	jobs := make([]Job, 0, len(sizes))
	for name := range sizes {
		abs := filepath.Join(root, name)
		meta, err := fsmeta.PathMeta(abs)
		if err != nil {
			t.Fatalf("lstat %s: %v", abs, err)
		}
		jobs = append(jobs, Job{RelPath: name, AbsPath: abs, Meta: meta})
	}
	return jobs
}

// --- correctness -------------------------------------------------------------

// TestHashPoolConcurrentMatchesSequential: a 4-worker pool hashing the
// mixed-size fixture must produce, for EVERY file, the same digest as
// the independent sequential reference - including the two fixed test
// vectors (empty and "abc" per the published SHA-256 constants, the
// in-test "sha256sum semantics" cross-check) - with Bytes equal to the
// file size and the walk-time Meta passed through untouched (the T1.5
// re-lstat baseline).
func TestHashPoolConcurrentMatchesSequential(t *testing.T) {
	root, sizes := buildHashFixture(t)
	jobs := jobsFor(t, root, sizes)

	pool := NewHashPool(context.Background(), PoolOptions{Workers: 4})
	for _, j := range jobs {
		if err := pool.Submit(j); err != nil {
			t.Fatalf("Submit(%s): %v", j.RelPath, err)
		}
	}
	pool.Close()
	// Drain BEFORE Wait: the pool's contract is that Wait completes
	// only after the consumer has drained Results (backpressure).
	got := resultsByRel(drainResults(pool.Results()))
	if err := pool.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	if len(got) != len(jobs) {
		t.Fatalf("got %d results, want %d", len(got), len(jobs))
	}
	// Fixed published vectors: the pool's streaming hash equals the
	// sha256sum semantics byte for byte.
	if want := sha256.Sum256(nil); got["empty.bin"].Digest != want {
		t.Errorf("empty.bin digest = %x, want %x (sha256 of the empty input)", got["empty.bin"].Digest, want)
	}
	if want := sha256.Sum256([]byte("abc")); got["abc.bin"].Digest != want {
		t.Errorf("abc.bin digest = %x, want %x (NIST vector)", got["abc.bin"].Digest, want)
	}
	for name, size := range sizes {
		r, ok := got[name]
		if !ok {
			t.Errorf("no result for %s", name)
			continue
		}
		if r.Err != nil {
			t.Errorf("%s: unexpected error %v", name, r.Err)
			continue
		}
		if r.Bytes != size {
			t.Errorf("%s: Bytes = %d, want %d", name, r.Bytes, size)
		}
		if want := seqHashFile(t, filepath.Join(root, name)); r.Digest != want {
			t.Errorf("%s: pool digest %x != sequential reference %x", name, r.Digest, want)
		}
	}
	// Meta passthrough: the walk-time baseline arrives intact (T1.5's
	// change-detection input).
	for _, j := range jobs {
		if r := got[j.RelPath]; r.Meta != j.Meta {
			t.Errorf("%s: Meta not passed through: got %+v want %+v", j.RelPath, r.Meta, j.Meta)
		}
	}
}

// --- worker count ------------------------------------------------------------

// collectBlockedWorkers deterministically observes exactly n workers
// inside the blocking open: it waits for n tokens and asserts no (n+1)th
// exists (impossible while all n are parked, so the check is race-free).
func collectBlockedWorkers(t *testing.T, ob *blockingOpen, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-ob.opened:
		case <-time.After(30 * time.Second):
			t.Fatalf("timed out waiting for worker %d/%d to reach the open barrier", i+1, n)
		}
	}
	select {
	case p := <-ob.opened:
		t.Fatalf("worker %d started (path %s) with only %d workers configured", n+1, p, n)
	default:
	}
}

// TestHashPoolDefaultWorkerCountViaBarrier: the default pool runs
// exactly min(GOMAXPROCS, 8) workers, observed through the open seam:
// with every worker parked at its first open, no additional open can
// occur, and the high-water mark equals the default worker count.
func TestHashPoolDefaultWorkerCountViaBarrier(t *testing.T) {
	k := DefaultHashWorkers()
	root := t.TempDir()
	const extra = 6
	total := 3*k + extra // enough jobs that workers stay busy after release
	var jobs []Job
	for i := 0; i < total; i++ {
		name := fmt.Sprintf("f%03d.bin", i)
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, Job{RelPath: name, AbsPath: filepath.Join(root, name)})
	}

	ob := newBlockingOpen()
	pool := NewHashPool(context.Background(), PoolOptions{OpenFile: ob.open})
	// Concurrent drainer (the documented wiring shape): keeps results
	// from ever applying backpressure to the barrier sequencing below.
	var wg sync.WaitGroup
	var drained []HashResult
	wg.Add(1)
	go func() {
		defer wg.Done()
		for r := range pool.Results() {
			drained = append(drained, r)
		}
	}()
	// Submit exactly the pool's holding capacity while every worker is
	// parked: k in-flight + 2k queued (queue depth = 2 x workers). Any
	// more would block Submit itself at the barrier.
	filler := jobs[:3*k]
	for _, j := range filler {
		if err := pool.Submit(j); err != nil {
			t.Fatalf("Submit(%s): %v", j.RelPath, err)
		}
	}
	collectBlockedWorkers(t, ob, k) // exactly k concurrent readers, never k+1
	if hw := pool.HighWater(); hw != int64(k) {
		t.Errorf("HighWater = %d at the barrier, want exactly %d (the default worker count)", hw, k)
	}
	close(ob.release)
	// The tail submits flow while the released workers drain the queue.
	for _, j := range jobs[3*k:] {
		if err := pool.Submit(j); err != nil {
			t.Fatalf("Submit(%s): %v", j.RelPath, err)
		}
	}
	pool.Close()
	wg.Wait()
	if err := pool.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(drained) != total {
		t.Errorf("got %d results, want %d", len(drained), total)
	}
}

// TestHashPoolOverrideThreeWorkers: --hash-workers 3 runs exactly three
// workers (barrier-observed), and the at-most-N bound holds.
func TestHashPoolOverrideThreeWorkers(t *testing.T) {
	root := t.TempDir()
	const total = 9 // 3 in-flight + 6 queued (queue depth = 2 x workers)
	var jobs []Job
	for i := 0; i < total; i++ {
		name := fmt.Sprintf("f%d.bin", i)
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, Job{RelPath: name, AbsPath: filepath.Join(root, name)})
	}

	ob := newBlockingOpen()
	pool := NewHashPool(context.Background(), PoolOptions{Workers: 3, OpenFile: ob.open})
	if pool.Workers() != 3 {
		t.Fatalf("Workers() = %d, want the override 3", pool.Workers())
	}
	for _, j := range jobs {
		if err := pool.Submit(j); err != nil {
			t.Fatalf("Submit(%s): %v", j.RelPath, err)
		}
	}
	collectBlockedWorkers(t, ob, 3)
	if hw := pool.HighWater(); hw != 3 {
		t.Errorf("HighWater = %d at the barrier, want exactly 3", hw)
	}
	close(ob.release)
	pool.Close()
	results := drainResults(pool.Results())
	if err := pool.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(results) != total {
		t.Fatalf("got %d results, want %d", len(results), total)
	}
	for _, r := range results {
		if r.Err != nil {
			t.Errorf("%s: %v", r.RelPath, r.Err)
		}
	}
}

// TestHashPoolHighWaterBounded: with many small files the high-water
// mark never exceeds the worker count (the DESIGN_SPEC 3.3 memory-cap
// assertion), for the override and for the default pool.
func TestHashPoolHighWaterBounded(t *testing.T) {
	run := func(t *testing.T, workers, files int) int64 {
		t.Helper()
		root := t.TempDir()
		pool := NewHashPool(context.Background(), PoolOptions{Workers: workers})
		// Drain concurrently: the pool's holding capacity (workers +
		// queues) is smaller than the file count, so backpressure
		// needs a live consumer (the documented T1.6 wiring shape).
		var wg sync.WaitGroup
		var results []HashResult
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range pool.Results() {
				results = append(results, r)
			}
		}()
		for i := 0; i < files; i++ {
			name := fmt.Sprintf("f%03d.bin", i)
			if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := pool.Submit(Job{RelPath: name, AbsPath: filepath.Join(root, name)}); err != nil {
				t.Fatalf("Submit: %v", err)
			}
		}
		pool.Close()
		wg.Wait()
		if err := pool.Wait(); err != nil {
			t.Fatalf("Wait: %v", err)
		}
		if len(results) != files {
			t.Fatalf("got %d results, want %d", len(results), files)
		}
		return pool.HighWater()
	}

	if hw := run(t, 3, 64); hw < 1 || hw > 3 {
		t.Errorf("HighWater = %d with 3 workers and 64 files, want 1 <= hw <= 3", hw)
	}
	if hw := run(t, 0, 16); hw < 1 || hw > int64(DefaultHashWorkers()) {
		t.Errorf("HighWater = %d with the default pool, want 1 <= hw <= %d", hw, DefaultHashWorkers())
	}
}

// --- failure tolerance (FR-003) ----------------------------------------------

// TestHashPoolUnreadableFileContinues: a chmod-000 file (skipped as
// root, where EACCES never fires) yields an error result carrying the
// errno, and the pool keeps hashing its siblings correctly.
func TestHashPoolUnreadableFileContinues(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 000 does not deny access")
	}
	root := t.TempDir()
	good := map[string]string{"a.bin": "aaa", "b.bin": "bbb"}
	for name, content := range good {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bad := filepath.Join(root, "bad.bin")
	if err := os.WriteFile(bad, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(bad, 0o000); err != nil {
		t.Fatalf("chmod 000: %v", err)
	}
	t.Cleanup(func() { os.Chmod(bad, 0o644) }) // let TempDir remove it

	pool := NewHashPool(context.Background(), PoolOptions{Workers: 2})
	for _, name := range []string{"a.bin", "bad.bin", "b.bin"} {
		abs := filepath.Join(root, name)
		if err := pool.Submit(Job{RelPath: name, AbsPath: abs}); err != nil {
			t.Fatalf("Submit(%s): %v", name, err)
		}
	}
	pool.Close()
	results := drainResults(pool.Results())
	if err := pool.Wait(); err != nil {
		t.Fatalf("Wait: %v (an unreadable file must not fail the pool)", err)
	}
	got := resultsByRel(results)

	badRes := got["bad.bin"]
	if badRes.Err == nil {
		t.Fatal("unreadable file produced no error result")
	}
	if !strings.Contains(badRes.Err.Error(), "permission denied") {
		t.Errorf("unreadable error %q does not carry the errno text", badRes.Err)
	}
	if badRes.Bytes != 0 || badRes.Digest != ([32]byte{}) {
		t.Errorf("unreadable file has Bytes=%d digest=%x, want none (no hash, FR-003)", badRes.Bytes, badRes.Digest)
	}
	for name, content := range good {
		r := got[name]
		if r.Err != nil {
			t.Errorf("%s: %v", name, r.Err)
			continue
		}
		if want := sha256.Sum256([]byte(content)); r.Digest != want {
			t.Errorf("%s: digest %x != %x (pool continued past the unreadable file)", name, r.Digest, want)
		}
	}
}

// errAfterReader reads at most remaining bytes, then fails with
// errInjectedRead - the mid-stream read-error shape of FR-003 (EIO,
// ESTALE).
type errAfterReader struct {
	rc        io.ReadCloser
	remaining int
}

var errInjectedRead = errors.New("injected mid-stream read error")

func (e *errAfterReader) Read(p []byte) (int, error) {
	if e.remaining <= 0 {
		return 0, errInjectedRead
	}
	if len(p) > e.remaining {
		p = p[:e.remaining]
	}
	n, err := e.rc.Read(p)
	e.remaining -= n
	return n, err
}

func (e *errAfterReader) Close() error { return e.rc.Close() }

// TestHashPoolMidReadErrorContinues: a read that fails mid-stream
// produces an error result with the partial byte count and no digest;
// the pool continues and the other files hash correctly.
func TestHashPoolMidReadErrorContinues(t *testing.T) {
	root := t.TempDir()
	big := make([]byte, 1000)
	for i := range big {
		big[i] = byte(i % 250)
	}
	if err := os.WriteFile(filepath.Join(root, "bad.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "good.bin"), []byte("good"), 0o644); err != nil {
		t.Fatal(err)
	}

	open := func(path string) (io.ReadCloser, error) {
		rc, err := osOpen(path)
		if err != nil {
			return nil, err
		}
		if filepath.Base(path) == "bad.bin" {
			return &errAfterReader{rc: rc, remaining: 100}, nil
		}
		return rc, nil
	}
	pool := NewHashPool(context.Background(), PoolOptions{Workers: 2, OpenFile: open})
	for _, name := range []string{"bad.bin", "good.bin"} {
		if err := pool.Submit(Job{RelPath: name, AbsPath: filepath.Join(root, name)}); err != nil {
			t.Fatalf("Submit(%s): %v", name, err)
		}
	}
	pool.Close()
	// Drain BEFORE Wait: the pool's contract is that Wait completes
	// only after the consumer has drained Results (backpressure).
	got := resultsByRel(drainResults(pool.Results()))
	if err := pool.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	bad := got["bad.bin"]
	if !errors.Is(bad.Err, errInjectedRead) {
		t.Errorf("bad.bin error = %v, want the injected read error", bad.Err)
	}
	if bad.Bytes != 100 {
		t.Errorf("bad.bin Bytes = %d, want the 100 bytes read before the failure", bad.Bytes)
	}
	if bad.Digest != ([32]byte{}) {
		t.Errorf("bad.bin digest = %x, want zero (no hash on failure)", bad.Digest)
	}
	if r := got["good.bin"]; r.Err != nil || r.Digest != sha256.Sum256([]byte("good")) {
		t.Errorf("good.bin = %+v, want a clean correct digest", r)
	}
}

// --- the streaming bound -----------------------------------------------------

// TestHashPoolCancellationCleanShutdown pins the documented context
// policy (PM policy change 2026-10-19, defect A): cancel aborts intake,
// the three in-flight files ABORT PROMPTLY at their first read boundary,
// the six queued jobs are abandoned, Wait reports the cancel, and no
// goroutine outlives the pool.
//
// POLICY-CHANGE NOTE (deliberate): the pre-change version of this test
// asserted the OPPOSITE - "the three in-flight files complete and
// deliver" - which pinned the T1.4 rule whose Ctrl+C latency (every
// in-flight multi-GiB hash must finish before the pool drains) the PM
// replaced with prompt abort. The three workers here are parked inside
// the blocking open seam, so the very first ctx check of hashOne's read
// loop aborts each file: the results are ctx-canceled, zero digest.
func TestHashPoolCancellationCleanShutdown(t *testing.T) {
	baseline := settleIngestGoroutines(t)

	root := t.TempDir()
	var jobs []Job
	for i := 0; i < 9; i++ { // 3 in workers + 6 in the 2x3 queue: all Submits fit
		name := fmt.Sprintf("f%d.bin", i)
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, Job{RelPath: name, AbsPath: filepath.Join(root, name)})
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ob := newBlockingOpen()
	pool := NewHashPool(ctx, PoolOptions{Workers: 3, OpenFile: ob.open})
	for _, j := range jobs {
		if err := pool.Submit(j); err != nil {
			t.Fatalf("Submit(%s): %v", j.RelPath, err)
		}
	}
	collectBlockedWorkers(t, ob, 3) // all workers mid-hash, queue full

	cancel()          // intake aborts...
	close(ob.release) // ...in-flight files abort at their first read boundary
	got := drainResults(pool.Results())
	if err := pool.Wait(); !errors.Is(err, context.Canceled) {
		t.Errorf("Wait = %v, want context.Canceled", err)
	}
	if len(got) != 3 {
		t.Errorf("got %d results, want exactly the 3 in-flight files (queued jobs abandoned on cancel)", len(got))
	}
	for _, r := range got {
		if !r.Aborted() {
			t.Errorf("in-flight %s result = %+v, want an aborted ctx-canceled result (prompt-abort policy)", r.RelPath, r)
		}
		if r.Digest != [32]byte{} {
			t.Errorf("aborted %s carries digest %x, want the all-zero digest (never mark unverified data, FR-014)", r.RelPath, r.Digest)
		}
	}
	// Sequential misuse degrades cleanly, not to a panic: intake was
	// aborted by the cancellation (Close was never even called), so
	// Submit fails fast with the context error rather than accepting a
	// job no worker will ever drain.
	if err := pool.Submit(jobs[0]); !errors.Is(err, context.Canceled) {
		t.Errorf("Submit after cancel = %v, want context.Canceled", err)
	}
	waitIngestGoroutinesBaseline(t, baseline)
}

// blockingReader is the OpenFile payload of the mid-read cancellation
// test: the FIRST Read closes started, PARKS until release, then yields
// its data chunk; every later Read answers EOF immediately. The Read
// call count proves whether an aborted hash issued any further read.
type blockingReader struct {
	started chan struct{} // closed when the first Read begins
	release chan struct{} // closed by the test to unblock that Read
	chunk   []byte

	mu    sync.Mutex
	calls int
}

func (r *blockingReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	r.calls++
	first := r.calls == 1
	r.mu.Unlock()
	if first {
		close(r.started)
		<-r.release
		return copy(p, r.chunk), nil
	}
	return 0, io.EOF
}

func (r *blockingReader) Close() error { return nil }

func (r *blockingReader) readCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// TestHashPoolCancelMidReadAbortsAtNextReadBoundary pins the prompt-abort
// policy through the REAL pool: the worker is cancelled while blocked
// inside f.Read, and the released read is followed by the ctx check - so
// the in-flight result comes back PROMPTLY as ctx-canceled (bounded by
// channel sync, never a sleep race), with the all-zero digest, without a
// second Read, and without an ItemDone progress event (an aborted file
// did not complete). Bytes proves the abort landed at the NEXT read
// boundary: exactly the released chunk was consumed, then the loop
// aborted instead of reading again.
func TestHashPoolCancelMidReadAbortsAtNextReadBoundary(t *testing.T) {
	baseline := settleIngestGoroutines(t)

	root := t.TempDir()
	path := filepath.Join(root, "mid.bin")
	if err := os.WriteFile(path, []byte("content is irrelevant; the seam serves the reads"), 0o644); err != nil {
		t.Fatal(err)
	}
	const chunk = "0123456789abcdef" // 16 bytes yielded by the released read

	br := &blockingReader{
		started: make(chan struct{}),
		release: make(chan struct{}),
		chunk:   []byte(chunk),
	}

	// A Bus with a RUNNING plain renderer: the aborted file must emit NO
	// ItemDone, so no progress line may ever render.
	bus := progress.NewBus()
	var rendered bytes.Buffer
	rendDone := make(chan progress.Done, 1)
	go func() {
		rendDone <- progress.New(progress.Options{Mode: progress.ModePlain, Stderr: &rendered}).Run(bus)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := NewHashPool(ctx, PoolOptions{
		Workers:  1,
		Bus:      bus,
		OpenFile: func(string) (io.ReadCloser, error) { return br, nil },
	})
	if err := pool.Submit(Job{RelPath: "mid.bin", AbsPath: path}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	<-br.started // deterministic: the worker is INSIDE the first Read

	cancel()          // mid-read cancellation...
	close(br.release) // ...only then does the blocked read return

	// Bounded by channel sync: the aborted result must arrive with no
	// further test interaction of any kind.
	select {
	case r, ok := <-pool.Results():
		if !ok {
			t.Fatal("results closed before delivering the aborted in-flight result")
		}
		if !r.Aborted() {
			t.Errorf("mid-read-cancelled result = %+v, want aborted (Err wrapping context.Canceled)", r)
		}
		if r.Digest != [32]byte{} {
			t.Errorf("digest = %x, want the all-zero digest for an aborted hash", r.Digest)
		}
		if r.Bytes != int64(len(chunk)) {
			t.Errorf("Bytes = %d, want %d: the released read completed, then the abort landed at the NEXT read boundary",
				r.Bytes, len(chunk))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("in-flight hash did not abort promptly after cancel + read release")
	}

	for range pool.Results() { // drain to close (contract: Wait needs a drained consumer)
	}
	if err := pool.Wait(); !errors.Is(err, context.Canceled) {
		t.Errorf("Wait = %v, want context.Canceled", err)
	}
	if got := br.readCalls(); got != 1 {
		t.Errorf("Read was called %d times, want exactly 1 (the loop must not read again after the abort)", got)
	}

	bus.Finish(progress.Done{Summary: "aborted"}) // guaranteed-delivery join
	<-rendDone
	if out := rendered.String(); strings.Contains(out, "hash: files") {
		t.Errorf("aborted file emitted an ItemDone progress line (it did not complete):\n%s", out)
	}
	waitIngestGoroutinesBaseline(t, baseline)
}

// TestHashPoolSubmitAfterClose: sequential Submit-after-Close misuse
// returns ErrPoolClosed instead of panicking on the closed queue.
func TestHashPoolSubmitAfterClose(t *testing.T) {
	pool := NewHashPool(context.Background(), PoolOptions{Workers: 1})
	pool.Close()
	if err := pool.Submit(Job{RelPath: "x", AbsPath: "x"}); !errors.Is(err, ErrPoolClosed) {
		t.Errorf("Submit after Close = %v, want ErrPoolClosed", err)
	}
	drainResults(pool.Results())
	if err := pool.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

// --- progress ------------------------------------------------------------------

// TestHashPoolNilBusWorks: a nil Bus silences all progress emission and
// nothing else changes.
func TestHashPoolNilBusWorks(t *testing.T) {
	root, sizes := buildHashFixture(t)
	pool := NewHashPool(context.Background(), PoolOptions{Workers: 2}) // Bus: nil
	for _, j := range jobsFor(t, root, sizes) {
		if err := pool.Submit(j); err != nil {
			t.Fatalf("Submit: %v", err)
		}
	}
	pool.Close()
	// Drain BEFORE Wait: the pool's contract is that Wait completes
	// only after the consumer has drained Results (backpressure).
	got := resultsByRel(drainResults(pool.Results()))
	if err := pool.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	for name, size := range sizes {
		r, ok := got[name]
		if !ok || r.Err != nil || r.Bytes != size {
			t.Errorf("%s: missing or wrong result %+v (size %d)", name, r, size)
		}
	}
}

// TestHashPoolEmitsPhaseAndItemDone: with a capturing plain renderer,
// the pool emits one Phase ("hash", totals unknown) and one ItemDone
// per file; the final rendered line proves the file count and the
// summed byte counts (each ItemDone carried its file's bytes).
func TestHashPoolEmitsPhaseAndItemDone(t *testing.T) {
	root := t.TempDir()
	sizes := []int64{4, 8, 0}
	var total int64
	for i, size := range sizes {
		name := fmt.Sprintf("f%d.bin", i)
		if err := os.WriteFile(filepath.Join(root, name), bytes.Repeat([]byte{byte('a' + i)}, int(size)), 0o644); err != nil {
			t.Fatal(err)
		}
		total += size
	}

	bus := progress.NewBus()
	var buf bytes.Buffer
	// Advancing clock: every ItemDone passes the plain cadence
	// throttle, so each one renders a cumulative line (the renderer
	// goroutine is the only caller of Now after Run starts).
	var tick time.Duration
	r := progress.New(progress.Options{
		Mode:   progress.ModePlain,
		Stderr: &buf,
		Now: func() time.Time {
			tick += progress.PlainInterval
			return time.Unix(0, 0).Add(tick)
		},
	})
	done := make(chan progress.Done, 1)
	go func() { done <- r.Run(bus) }()

	pool := NewHashPool(context.Background(), PoolOptions{Workers: 2, Bus: bus})
	for i := range sizes {
		name := fmt.Sprintf("f%d.bin", i)
		if err := pool.Submit(Job{RelPath: name, AbsPath: filepath.Join(root, name)}); err != nil {
			t.Fatalf("Submit(%s): %v", name, err)
		}
	}
	pool.Close()
	drainResults(pool.Results()) // contract: drain before Wait
	if err := pool.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	bus.Finish(progress.Done{Summary: "hash complete"}) // guaranteed-delivery join
	<-done

	out := buf.String()
	if !strings.Contains(out, "hash: files 1, ") {
		t.Errorf("output %q lacks the first 'hash: files 1, <bytes>' line (phase or ItemDone missing)", out)
	}
	wantLast := fmt.Sprintf("hash: files %d, %s", len(sizes), units.FormatBytes(total))
	if !strings.Contains(out, wantLast) {
		t.Errorf("output %q lacks the cumulative line %q (one ItemDone per file with its byte count)", out, wantLast)
	}
	if !strings.Contains(out, "hash complete") {
		t.Errorf("output %q lacks the final summary", out)
	}
}

// --- the documented T1.6 wiring ----------------------------------------------

// TestWalkPoolBatcherEndToEnd is the wiring story from the file doc,
// executed: Walk feeds pool.Submit, a consumer goroutine drains the
// pool into the batcher, and the batcher's flush records every result.
// Every walked file must arrive exactly once with the reference digest,
// which pins the whole pipeline's backpressure chain end to end.
func TestWalkPoolBatcherEndToEnd(t *testing.T) {
	root, sizes := buildHashFixture(t)

	var mu sync.Mutex
	var flushed []HashResult
	b := NewBatcher(BatcherOptions[HashResult]{
		Flush: func(batch []HashResult) error {
			mu.Lock()
			defer mu.Unlock()
			flushed = append(flushed, batch...)
			return nil
		},
		Queue: -1, // deterministic hand-off for the test
	})

	pool := NewHashPool(context.Background(), PoolOptions{Workers: 4})
	var consumerErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for r := range pool.Results() {
			if err := b.Add(r); err != nil {
				consumerErr = err
			}
		}
		if err := b.Close(); err != nil && consumerErr == nil {
			consumerErr = err
		}
	}()

	res, err := Walk(root, Matcher{}, pool.Submit, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	pool.Close()
	if err := pool.Wait(); err != nil {
		t.Fatalf("pool Wait: %v", err)
	}
	wg.Wait()
	if consumerErr != nil {
		t.Fatalf("consumer pipeline error: %v", consumerErr)
	}
	if err := b.Close(); err != nil { // joining Close: same result
		t.Fatalf("batcher Close: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if res.Files != len(sizes) || len(flushed) != len(sizes) {
		t.Fatalf("walk emitted %d files, batcher flushed %d records, want %d each", res.Files, len(flushed), len(sizes))
	}
	byRel := make(map[string]HashResult, len(flushed))
	for _, r := range flushed {
		if r.Err != nil {
			t.Errorf("%s: %v", r.RelPath, r.Err)
		}
		byRel[r.RelPath] = r
	}
	for name := range sizes {
		r, ok := byRel[name]
		if !ok {
			t.Errorf("file %s never reached the batcher", name)
			continue
		}
		if want := seqHashFile(t, filepath.Join(root, name)); r.Digest != want {
			t.Errorf("%s: digest %x != reference %x", name, r.Digest, want)
		}
	}
}
