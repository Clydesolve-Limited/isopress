package ingest

// T1.4 hardening tests: race, boundary, and oracle coverage that
// complements hashpool_test.go (DESIGN_SPEC 3.3 acceptance) and
// batcher_test.go (the two commit triggers). Everything here pins
// behaviour the base suite leaves uncovered:
//
//   - Digest oracle: one non-trivial (multi-MiB, buffer-boundary
//     crossing) file hashed by the pool through the PRODUCTION open
//     path (os.Open, no injected seam) is cross-checked against
//     /usr/bin/sha256sum via os/exec - an implementation-independent
//     oracle that guards against a systematically-wrong streaming loop
//     that in-test references could plausibly share. (os/exec is
//     standard library, so deps_test's pure-stdlib gate still holds;
//     the os/exec prohibition of DESIGN_SPEC 11 applies to production
//     service code, not to tests.)
//   - Digest value semantics: Digest is a raw [32]byte copied out of
//     the hash state, never a slice of the per-worker 1 MiB buffer
//     (which is REUSED across jobs). A single-worker pool hashing a
//     buffer-crossing file followed by two identical-content files
//     proves no aliasing, and mutating one received result's Digest
//     provably cannot affect another result (value type, not a shared
//     backing array).
//   - Pool lifecycle: Close is idempotent (sequential AND concurrent
//     callers), Wait is the repeatable join call (callable twice, same
//     result), absorbing concurrent Close callers via closeOnce.
//   - Progress accounting on the FAILURE path: a mid-stream read error
//     emits an ItemDone carrying the PARTIAL byte count, so the final
//     rendered line reflects bytes actually streamed (DESIGN_SPEC 9.1:
//     one ItemDone per processed file, success or failure).
//   - Batcher timer boundary, deterministically via the fake clock: a
//     batch does NOT flush one nanosecond before the 2-second deadline
//     and DOES flush exactly AT it; the window restarts at the next
//     record; stopped timers never re-fire (no double flush).
//   - Batcher tick-vs-Close interleaving: with a fired tick pending in
//     the timer channel and intake closing in the same instant, BOTH
//     legal select interleavings must yield exactly one flush holding
//     every record - no double flush, no loss, no missed final flush.
//   - Batcher multi-producer Add: the documented wiring is
//     single-producer, but Add is a plain channel send, so concurrent
//     producers racing with Close REMAIN a documented contract
//     violation (panic window), while concurrent producers that all
//     join BEFORE Close are race-safe and lossless. That supported
//     shape is asserted here under the race detector, with concurrent
//     Err() polling.
//   - Batcher concurrent Close: multiple goroutines may call Close;
//     every caller observes the same result (closeOnce + dead join).
//   - Batcher flush-error stop path: the pipeline stops, producers are
//     released fast, and Close exits the pump - a settle-then-poll
//     goroutine-leak check proves the error path leaks nothing.
//   - Batcher record-races-timer stress: a producer Adding 10,000
//     records concurrently with a clock crossing the interval boundary
//     2,000 times must account for every record exactly once, in
//     order, in non-empty batches of at most Size records - the
//     "record arrives EXACTLY as the timer fires" hazard, hammered
//     under -race.
//
// Synchronization policy (matching the base suite): no sleeps in the
// hot paths; channel hand-offs, the fake clock, and the settle-then-poll
// leak pattern carry all ordering. Stdlib only; every fixture lives in
// t.TempDir().

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"isopress/internal/progress"
	"isopress/internal/units"
)

// --- digest oracle -----------------------------------------------------------

// TestHashPoolDigestMatchesSha256SumOracle hashes one non-trivial file
// (3 MiB + 123 bytes: several 1 MiB buffer cycles plus a ragged tail)
// and the fixed "abc" vector through the pool with the DEFAULT OpenFile
// (production os.Open path - the injection seam is NOT used here) and
// compares the raw digest against `sha256sum` output. sha256sum is a
// fully independent implementation: if the pool's streaming loop were
// systematically wrong (off-by-one read boundary, stale buffer tail,
// double-consumed chunk), every in-test reference could in principle
// share the flaw, but the external binary cannot.
func TestHashPoolDigestMatchesSha256SumOracle(t *testing.T) {
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skipf("sha256sum not on PATH (%v); the in-test reference remains the only oracle here", err)
	}
	root := t.TempDir()

	// Non-trivial content: 3 MiB + 123, patterned across every byte so
	// a shifted or duplicated buffer chunk changes the digest.
	data := make([]byte, 3*(1<<20)+123)
	for i := range data {
		data[i] = byte(i*131 ^ (i >> 8) ^ (i >> 16))
	}
	oraclePath := filepath.Join(root, "oracle.bin")
	if err := os.WriteFile(oraclePath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	abcPath := filepath.Join(root, "abc.bin")
	if err := os.WriteFile(abcPath, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}

	pool := NewHashPool(context.Background(), PoolOptions{Workers: 2}) // no OpenFile: production os.Open
	for _, name := range []string{"oracle.bin", "abc.bin"} {
		if err := pool.Submit(Job{RelPath: name, AbsPath: filepath.Join(root, name)}); err != nil {
			t.Fatalf("Submit(%s): %v", name, err)
		}
	}
	pool.Close()
	got := resultsByRel(drainResults(pool.Results()))
	if err := pool.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	for _, name := range []string{"oracle.bin", "abc.bin"} {
		r, ok := got[name]
		if !ok {
			t.Fatalf("no result for %s", name)
		}
		if r.Err != nil {
			t.Fatalf("%s: %v", name, r.Err)
		}
		out, err := exec.Command("sha256sum", filepath.Join(root, name)).Output()
		if err != nil {
			t.Fatalf("sha256sum %s: %v", name, err)
		}
		fields := strings.Fields(string(out))
		if len(fields) == 0 {
			t.Fatalf("sha256sum %s produced no digest field: %q", name, out)
		}
		if want := fields[0]; hex.EncodeToString(r.Digest[:]) != want {
			t.Errorf("%s: pool digest %x != sha256sum oracle %s (streaming loop is systematically wrong)", name, r.Digest, want)
		}
		// Belt and braces: the independent in-test reference agrees too.
		if ref := seqHashFile(t, filepath.Join(root, name)); ref != r.Digest {
			t.Errorf("%s: pool digest %x != in-test reference %x", name, r.Digest, ref)
		}
	}
}

// --- digest value semantics (no aliasing into the reused worker buffer) ------

// TestHashPoolDigestValueIndependence pins that Digest is a VALUE: the
// worker's 1 MiB buffer is reused across every job it ever processes,
// and h.Sum(nil) allocates a fresh byte slice that is COPIED into the
// result. A single worker hashes a buffer-crossing file and then two
// identical-content files through the same buffer; all digests must be
// correct (a buffer-aliased digest would corrupt the earlier results),
// and mutating one received result's Digest must provably leave the
// others untouched (value semantics, not a shared [32]byte backing).
func TestHashPoolDigestValueIndependence(t *testing.T) {
	root := t.TempDir()
	big := make([]byte, 2*(1<<20)+17) // > 1 MiB: forces multiple buffer refills on the one worker
	for i := range big {
		big[i] = byte(i % 253)
	}
	if err := os.WriteFile(filepath.Join(root, "big.bin"), big, 0o644); err != nil {
		t.Fatal(err)
	}
	twins := []byte("identical-twin-content")
	for _, name := range []string{"twin1.bin", "twin2.bin"} {
		if err := os.WriteFile(filepath.Join(root, name), twins, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	pool := NewHashPool(context.Background(), PoolOptions{Workers: 1}) // ONE worker: all three jobs share one buffer
	for _, name := range []string{"big.bin", "twin1.bin", "twin2.bin"} {
		if err := pool.Submit(Job{RelPath: name, AbsPath: filepath.Join(root, name)}); err != nil {
			t.Fatalf("Submit(%s): %v", name, err)
		}
	}
	pool.Close()
	got := drainResults(pool.Results()) // single worker: arrival order == submission order
	if err := pool.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d results, want 3", len(got))
	}

	wantBig := sha256.Sum256(big)
	wantTwin := sha256.Sum256(twins)
	if got[0].Digest != wantBig || got[0].Bytes != int64(len(big)) {
		t.Errorf("big.bin: digest %x bytes %d, want %x / %d (buffer reuse must not corrupt earlier results)", got[0].Digest, got[0].Bytes, wantBig, len(big))
	}
	if got[1].Digest != wantTwin || got[2].Digest != wantTwin {
		t.Fatalf("twin digests %x / %x, want both %x", got[1].Digest, got[2].Digest, wantTwin)
	}

	// Value semantics: mutate one result's digest IN PLACE; its siblings
	// must be byte-identifiable and unchanged. (If Digest were ever
	// refactored into a slice sharing a backing array - e.g. into the
	// reused worker buffer - this catch fires.)
	got[1].Digest[0] ^= 0xff
	if got[2].Digest != wantTwin {
		t.Errorf("mutating one result's Digest changed a sibling result: %x != %x (Digest must be a value, not shared storage)", got[2].Digest, wantTwin)
	}
	if got[0].Digest != wantBig {
		t.Errorf("mutating one result's Digest changed an earlier result: %x != %x", got[0].Digest, wantBig)
	}
	// And the mutation landed on the mutated result itself - proving the
	// assertion above is not vacuous.
	if got[1].Digest == wantTwin {
		t.Error("digest mutation did not stick on the mutated result; the test is vacuous")
	}
}

// --- pool lifecycle ----------------------------------------------------------

// TestHashPoolCloseIdempotentWaitRepeatable pins the documented
// lifecycle: Close is idempotent for sequential AND concurrent callers
// (closeOnce absorbs the race), and Wait is the repeatable join call -
// calling it twice returns the same nil result instead of blocking or
// panicking. Results are still fully delivered around the double-join.
func TestHashPoolCloseIdempotentWaitRepeatable(t *testing.T) {
	root := t.TempDir()
	var jobs []Job
	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("f%d.bin", i)
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, Job{RelPath: name, AbsPath: filepath.Join(root, name)})
	}
	pool := NewHashPool(context.Background(), PoolOptions{Workers: 2})
	for _, j := range jobs {
		if err := pool.Submit(j); err != nil {
			t.Fatalf("Submit(%s): %v", j.RelPath, err)
		}
	}

	pool.Close()
	pool.Close() // sequential idempotence
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ { // concurrent Close callers: closeOnce must absorb them
		wg.Add(1)
		go func() {
			defer wg.Done()
			pool.Close()
		}()
	}
	wg.Wait()

	got := drainResults(pool.Results()) // channel closes exactly once, after all results
	if err := pool.Wait(); err != nil {
		t.Fatalf("first Wait: %v", err)
	}
	if err := pool.Wait(); err != nil {
		t.Fatalf("second Wait = %v, want nil (the join call is repeatable)", err)
	}
	if len(got) != 3 {
		t.Errorf("got %d results, want 3 (all delivered exactly once around the concurrent Closes)", len(got))
	}
}

// --- progress accounting on the failure path ---------------------------------

// TestHashPoolItemDoneCarriesPartialBytesOnFailure: a mid-stream read
// error at byte 100 must still emit an ItemDone (DESIGN_SPEC 9.1: one
// per processed file), and it must carry the PARTIAL byte count the
// stream actually produced - so the rendered cumulative line shows
// 100 + 4 = 104 bytes, not 4. A regression that zeroes Bytes on the
// error path (or skips the ItemDone entirely) breaks scan byte
// accounting for unreadable files.
func TestHashPoolItemDoneCarriesPartialBytesOnFailure(t *testing.T) {
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

	bus := progress.NewBus()
	var buf bytes.Buffer
	// Advancing clock: every ItemDone passes the plain cadence throttle
	// (the T1.1 harness pattern; the renderer goroutine is the only
	// caller of Now after Run starts).
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

	pool := NewHashPool(context.Background(), PoolOptions{Workers: 2, Bus: bus, OpenFile: open})
	for _, name := range []string{"bad.bin", "good.bin"} {
		if err := pool.Submit(Job{RelPath: name, AbsPath: filepath.Join(root, name)}); err != nil {
			t.Fatalf("Submit(%s): %v", name, err)
		}
	}
	pool.Close()
	got := resultsByRel(drainResults(pool.Results()))
	if err := pool.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	bus.Finish(progress.Done{Summary: "hash complete"})
	<-done

	if bad := got["bad.bin"]; bad.Err == nil || bad.Bytes != 100 {
		t.Fatalf("bad.bin result = %+v, want the injected error with 100 partial bytes", bad)
	}
	// The final cumulative line must carry 100 (partial, failure) + 4
	// (good) = 104 bytes over 2 files.
	want := fmt.Sprintf("hash: files 2, %s", units.FormatBytes(104))
	if out := buf.String(); !strings.Contains(out, want) {
		t.Errorf("rendered output %q lacks %q: the failed file's PARTIAL byte count must reach ItemDone (DESIGN_SPEC 9.1)", out, want)
	}
}

// --- batcher: exact timer boundary (deterministic, fake clock) ---------------

// TestBatcherTimerExactBoundary pins the 2-second trigger AT the
// deadline, nanosecond by nanosecond: no flush one nanosecond early,
// a flush exactly when the deadline is reached, a full fresh window for
// the next record, and no stale re-fire ever (stopped timers are dead).
// Together these assert "no double flush and no missed flush at the
// boundary" for the deterministic half of the hazard.
func TestBatcherTimerExactBoundary(t *testing.T) {
	clk := newFakeClock()
	rec := newRecordingFlush(0)
	b := NewBatcher(BatcherOptions[int]{
		Flush:    rec.flush,
		Now:      clk.Now,
		newTimer: clk.newTimer,
		Queue:    -1,
	})
	for i := 0; i < 3; i++ {
		if err := b.Add(i); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}
	clk.Advance(2*time.Second - time.Nanosecond) // one ns BEFORE the deadline
	rec.assertNoFlush(t, "a batch must not flush one nanosecond before its 2s deadline")
	clk.Advance(time.Nanosecond) // exactly AT the deadline
	rec.waitFlushed(t, 1)

	// The window restarts at the next record: a full fresh 2 seconds.
	// TWO records: with the unbuffered intake, Add returns at the pump's
	// receive, so a second record is the proof that the pump fully
	// processed the first (armed the new window) before we move the
	// clock - otherwise the advance below could race the arming.
	if err := b.Add(3); err != nil {
		t.Fatalf("Add(3): %v", err)
	}
	if err := b.Add(4); err != nil {
		t.Fatalf("Add(4): %v", err)
	}
	clk.Advance(2*time.Second - time.Nanosecond)
	rec.assertNoFlush(t, "the restarted window must run its full 2s from the new first record")
	clk.Advance(time.Nanosecond)
	rec.waitFlushed(t, 1) // one MORE flush invocation (the first token was consumed above)

	clk.Advance(10 * time.Second) // elapsed time past dead timers must never re-fire them
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got := rec.snapshot()
	if len(got) != 2 || !equalInts(got[0], []int{0, 1, 2}) || !equalInts(got[1], []int{3, 4}) {
		t.Fatalf("batches = %v, want [[0 1 2] [3 4]]: one flush per window, exactly at the deadline", got)
	}
}

// TestBatcherTickAndCloseInterleaveExactlyOnce: a fired tick is pending
// in the timer channel when intake closes in the same instant. The
// pump's select may legally take either ready case first; EVERY legal
// interleaving must produce exactly one flush carrying all five
// records - no double flush (tick then close-flush), no loss (close
// first, tick then flushed with an empty batch), no missed final flush.
// The assertion is deterministic even though the interleaving is not.
func TestBatcherTickAndCloseInterleaveExactlyOnce(t *testing.T) {
	clk := newFakeClock()
	rec := newRecordingFlush(0)
	b := NewBatcher(BatcherOptions[int]{
		Flush:    rec.flush,
		Now:      clk.Now,
		newTimer: clk.newTimer,
		Queue:    -1,
	})
	for i := 0; i < 5; i++ {
		if err := b.Add(i); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}
	clk.Advance(2 * time.Second) // the tick is now delivered into the buffered timer channel
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got := rec.snapshot()
	if len(got) != 1 || !equalInts(got[0], []int{0, 1, 2, 3, 4}) {
		t.Fatalf("batches = %v, want exactly [[0 1 2 3 4]] for every tick/close interleaving", got)
	}
}

// --- batcher: concurrency contracts -----------------------------------------

// TestBatcherConcurrentProducersBeforeClose: the documented wiring is
// single-producer (concurrent Add racing Close remains a contract
// violation - the panic window is documented in batcher.go and is NOT
// exercised here), but concurrent producers that all join BEFORE Close
// are a supported, race-free shape: Add is a plain channel send and Err
// is mutex-guarded. Eight producers add 4,000 records while a poller
// hammers Err(); every record must arrive exactly once, and each
// producer's own subsequence must stay in order (channel FIFO).
func TestBatcherConcurrentProducersBeforeClose(t *testing.T) {
	const producers, per = 8, 500
	rec := newRecordingFlush(0)
	b := NewBatcher(BatcherOptions[int]{
		Flush:    rec.flush,
		Size:     100,       // 40 flushes worth of count triggers
		Queue:    -1,        // unbuffered: Adds race the pump's receives for real
		Interval: time.Hour, // real clock; the time trigger never fires here
	})

	var prodWg sync.WaitGroup
	start := make(chan struct{})
	for p := 0; p < producers; p++ {
		prodWg.Add(1)
		go func(p int) {
			defer prodWg.Done()
			<-start
			for i := 0; i < per; i++ {
				if err := b.Add(p*per + i); err != nil {
					t.Errorf("producer %d Add(%d): %v (healthy window must never fail)", p, p*per+i, err)
					return
				}
			}
		}(p)
	}
	// Concurrent Err() polling: the mutex-guarded read must stay
	// race-clean against the pump's writes (asserted by -race).
	stopPoll := make(chan struct{})
	var pollWg sync.WaitGroup
	pollWg.Add(1)
	go func() {
		defer pollWg.Done()
		for {
			select {
			case <-stopPoll:
				return
			default:
				_ = b.Err()
			}
		}
	}()

	close(start)
	prodWg.Wait()
	close(stopPoll)
	pollWg.Wait()

	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	flat := flattenIntBatches(rec.snapshot())
	if len(flat) != producers*per {
		t.Fatalf("flushed %d records, want %d (loss or duplication under concurrent producers)", len(flat), producers*per)
	}
	lastSeen := make([]int64, producers)
	for i := range lastSeen {
		lastSeen[i] = -1 // record numbering starts at 0
	}
	for i, v := range flat {
		if v < 0 || v >= producers*per {
			t.Fatalf("position %d holds out-of-range record %d", i, v)
		}
		p := v / per
		if int64(v) <= lastSeen[p] {
			t.Fatalf("producer %d's records out of order: %d appeared after %d (channel FIFO broken)", p, v, lastSeen[p])
		}
		lastSeen[p] = int64(v)
	}
}

// TestBatcherConcurrentCloseCallersSameResult: Close is documented
// safe from multiple goroutines; closeOnce absorbs the race and every
// caller joins the same pump exit with the same (nil) result.
func TestBatcherConcurrentCloseCallersSameResult(t *testing.T) {
	rec := newRecordingFlush(0)
	b := NewBatcher(BatcherOptions[int]{Flush: rec.flush, Queue: -1})
	for i := 0; i < 10; i++ {
		if err := b.Add(i); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}
	const callers = 4
	errs := make([]error, callers) // distinct slots: no data race between the closers
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = b.Close()
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("concurrent Close #%d = %v, want nil", i, err)
		}
	}
	got := rec.snapshot()
	if len(got) != 1 || !equalInts(got[0], []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}) {
		t.Fatalf("batches = %v, want exactly [[0..9]] (the final partial flushed once)", got)
	}
}

// TestBatcherFlushErrorNoGoroutineLeak: the flush-error stop path must
// leave nothing behind. The failing flush (batch 1) stops the pipeline;
// the producer is released fast with the error; Close joins the pump's
// exit. The settle-then-poll goroutine check proves the error
// propagation path leaks no goroutine (the pump really returns).
func TestBatcherFlushErrorNoGoroutineLeak(t *testing.T) {
	baseline := settleIngestGoroutines(t)
	rec := newRecordingFlush(1) // the FIRST flush fails
	b := NewBatcher(BatcherOptions[int]{Flush: rec.flush, Queue: -1})
	for i := 0; i < 2000; i++ {
		// As in TestBatcherFlushErrorStopsPipeline: record 2000 may
		// legitimately return nil or the error (its send completes at
		// the pump's receive, which precedes the failure).
		if err := b.Add(i); err != nil && i != 1999 {
			t.Fatalf("Add(%d) inside a healthy window: %v", i, err)
		}
	}
	if err := b.Add(2000); err == nil {
		t.Fatal("Add after the failed flush returned nil, want the fast-fail error")
	} else if !errors.Is(err, rec.failErr) {
		t.Errorf("Add error = %v, want the flush callback's error", err)
	}
	if err := b.Close(); err == nil {
		t.Fatal("Close after a failed flush returned nil, want the error")
	}
	waitIngestGoroutinesBaseline(t, baseline) // the pump exited: no leak on the error stop path
}

// TestBatcherRecordRacesTimerExactlyOnce hammers the "record arrives
// EXACTLY as the timer fires" hazard: one producer Adds 10,000 records
// (unbuffered intake) while a concurrent clock crosses the 10 ms
// interval boundary 2,000 times, so the pump's select constantly finds
// BOTH a pending record and a fired tick ready. Whatever interleaving
// the scheduler picks, the accounting invariants must hold: every
// record flushed exactly once, in submission order (channel FIFO), in
// non-empty batches of at most Size records. An empty batch would betray
// a double-fired timer; an oversized one a missed size trigger; any
// gap, duplicate, or reorder a lost or duplicated flush.
func TestBatcherRecordRacesTimerExactlyOnce(t *testing.T) {
	const (
		total   = 10000
		sizeMax = 500
	)
	clk := newFakeClock()
	rec := newRecordingFlush(0)
	b := NewBatcher(BatcherOptions[int]{
		Flush:    rec.flush,
		Now:      clk.Now,
		newTimer: clk.newTimer,
		Queue:    -1,
		Size:     sizeMax,
		Interval: 10 * time.Millisecond,
	})

	var clockWg sync.WaitGroup
	clockWg.Add(1)
	go func() {
		defer clockWg.Done()
		for i := 0; i < 2000; i++ {
			clk.Advance(10 * time.Millisecond)
		}
	}()
	var prodWg sync.WaitGroup
	prodWg.Add(1)
	go func() {
		defer prodWg.Done()
		for i := 0; i < total; i++ {
			if err := b.Add(i); err != nil {
				t.Errorf("Add(%d): %v (healthy pipeline must never fail)", i, err)
				return
			}
		}
	}()
	prodWg.Wait()
	clockWg.Wait()
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	batches := rec.snapshot()
	flat := flattenIntBatches(batches)
	if len(flat) != total {
		t.Fatalf("flushed %d records, want %d (records lost or duplicated while racing the timer)", len(flat), total)
	}
	for i, v := range flat {
		if v != i {
			t.Fatalf("record %d at position %d: exactly-once, in-order accounting broken by the timer race", v, i)
		}
	}
	for i, batch := range batches {
		if len(batch) == 0 {
			t.Fatalf("flush %d is EMPTY: a timer fired twice for one window (double flush)", i)
		}
		if len(batch) > sizeMax {
			t.Fatalf("flush %d holds %d records, want at most %d (count trigger missed)", i, len(batch), sizeMax)
		}
	}
}

// --- helpers -----------------------------------------------------------------

// equalInts compares int slices for the batch assertions.
func equalInts(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// flattenIntBatches concatenates recorded batches in flush order.
func flattenIntBatches(batches [][]int) []int {
	var flat []int
	for _, b := range batches {
		flat = append(flat, b...)
	}
	return flat
}
