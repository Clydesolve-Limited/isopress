package ingest

// T1.4 batcher tests: the two DESIGN_SPEC 3.3 commit triggers are the
// acceptance surface, exercised through the fake-clock seam with ZERO
// wall-clock waiting. The unbuffered intake seam (Queue: -1) makes the
// sequencing deterministic: an Add returns only once the pump has taken
// the record, so "N records then Advance(x)" means the pump's batch
// really holds N records before the clock moves. Flush invocations are
// observed through a recording callback that signals a channel per call
// (channel synchronization, no sleeps, no polling).
//
// fakeClock extends the T1.1 injectable-clock pattern with timer
// support: Advance moves Now AND fires every armed timer whose deadline
// has passed, which is exactly the wake-up the pump's 2-second trigger
// needs. Stopped timers never fire again.

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

// --- fake clock with timer support ---------------------------------------

type fakeTimer struct {
	deadline time.Time
	ch       chan time.Time
	stopped  bool
}

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the fake time forward and fires every armed timer whose
// deadline the new time has reached (delivery is non-blocking: the
// timer channel is buffered and the pump reads at most one tick per
// arm).
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	for _, t := range c.timers {
		if !t.stopped && !t.deadline.After(c.now) {
			select {
			case t.ch <- c.now:
			default:
			}
		}
	}
}

// newTimer is the BatcherOptions.newTimer seam for tests.
func (c *fakeClock) newTimer(d time.Duration) (<-chan time.Time, func()) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ft := &fakeTimer{deadline: c.now.Add(d), ch: make(chan time.Time, 1)}
	c.timers = append(c.timers, ft)
	stop := func() {
		c.mu.Lock()
		ft.stopped = true
		c.mu.Unlock()
	}
	return ft.ch, stop
}

// --- recording flush callback ---------------------------------------------

// recordingFlush captures each batch (copied: the pump reuses the
// slice), signals flushed once per invocation, and fails with failErr
// on the Nth call when failOn != 0. failErr is one shared instance so
// tests can compare propagated errors with errors.Is through the
// batcher's wrapping.
type recordingFlush struct {
	mu      sync.Mutex
	batches [][]int
	failOn  int
	failErr error
	calls   int
	flushed chan struct{}
}

func newRecordingFlush(failOn int) *recordingFlush {
	r := &recordingFlush{failOn: failOn, flushed: make(chan struct{}, 64)}
	if failOn != 0 {
		r.failErr = fmt.Errorf("flush #%d fails on purpose", failOn)
	}
	return r
}

func (r *recordingFlush) flush(batch []int) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.batches = append(r.batches, append([]int(nil), batch...))
	select {
	case r.flushed <- struct{}{}:
	default:
	}
	if r.failOn != 0 && r.calls == r.failOn {
		return r.failErr
	}
	return nil
}

func (r *recordingFlush) snapshot() [][]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]int(nil), r.batches...)
}

func (r *recordingFlush) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// waitFlushed blocks until the pump reports n flush invocations.
func (r *recordingFlush) waitFlushed(t *testing.T, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-r.flushed:
		case <-time.After(30 * time.Second):
			t.Fatalf("timed out waiting for flush invocation %d/%d", i+1, n)
		}
	}
}

// assertNoFlush asserts the pump has NOT flushed (non-blocking check;
// only valid where the design makes a flush impossible).
func (r *recordingFlush) assertNoFlush(t *testing.T, why string) {
	t.Helper()
	select {
	case <-r.flushed:
		t.Fatalf("unexpected flush (%s)", why)
	default:
	}
}

// --- trigger tests ---------------------------------------------------------

// TestBatcherConstantsMatchDesignSpec pins the two DESIGN_SPEC 3.3
// numbers; the acceptance criteria name them explicitly.
func TestBatcherConstantsMatchDesignSpec(t *testing.T) {
	if BatchSize != 2000 {
		t.Errorf("BatchSize = %d, want 2000 (DESIGN_SPEC 3.3)", BatchSize)
	}
	if BatchInterval != 2*time.Second {
		t.Errorf("BatchInterval = %v, want 2s (DESIGN_SPEC 3.3)", BatchInterval)
	}
}

// TestBatcherCountTriggerFlushesAt2000: 2000 records inside the 2-second
// window close the batch at exactly 2000 (whichever-first loses the race
// to the clock because the clock never moves).
func TestBatcherCountTriggerFlushesAt2000(t *testing.T) {
	clk := newFakeClock()
	rec := newRecordingFlush(0)
	b := NewBatcher(BatcherOptions[int]{
		Flush:    rec.flush,
		Now:      clk.Now,
		newTimer: clk.newTimer,
		Queue:    -1, // unbuffered: deterministic record/clock sequencing
	})
	for i := 0; i < 2000; i++ {
		if err := b.Add(i); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}
	rec.waitFlushed(t, 1)
	// No second flush can follow: the window restarted empty.
	clk.Advance(5 * time.Second)
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got := rec.snapshot()
	if len(got) != 1 || len(got[0]) != 2000 {
		t.Fatalf("batches = %v, want exactly one batch of 2000", batchSizes(got))
	}
	if got[0][0] != 0 || got[0][1999] != 1999 {
		t.Errorf("batch content [0]=%d [1999]=%d, want 0 and 1999 (order preserved per batch)", got[0][0], got[0][1999])
	}
}

// TestBatcherTimeTriggerFlushesAtTwoSeconds: 1999 records (below the
// count trigger) then 2 simulated seconds close the batch at 1999 -
// the time trigger wins.
func TestBatcherTimeTriggerFlushesAtTwoSeconds(t *testing.T) {
	clk := newFakeClock()
	rec := newRecordingFlush(0)
	b := NewBatcher(BatcherOptions[int]{
		Flush:    rec.flush,
		Now:      clk.Now,
		newTimer: clk.newTimer,
		Queue:    -1,
	})
	for i := 0; i < 1999; i++ {
		if err := b.Add(i); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}
	rec.assertNoFlush(t, "1999 records inside the 2s window must not flush")
	clk.Advance(2 * time.Second)
	rec.waitFlushed(t, 1)
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got := rec.snapshot()
	if len(got) != 1 || len(got[0]) != 1999 {
		t.Fatalf("batches = %v, want exactly one batch of 1999", batchSizes(got))
	}
}

// TestBatcherInterleavedCountTriggerWins: 1500 records, 1 second
// (window still open, no flush), then 500 more - the batch closes at
// the 2000th record, and the stale clock never produces a second
// flush.
func TestBatcherInterleavedCountTriggerWins(t *testing.T) {
	clk := newFakeClock()
	rec := newRecordingFlush(0)
	b := NewBatcher(BatcherOptions[int]{
		Flush:    rec.flush,
		Now:      clk.Now,
		newTimer: clk.newTimer,
		Queue:    -1,
	})
	for i := 0; i < 1500; i++ {
		if err := b.Add(i); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}
	clk.Advance(1 * time.Second) // inside the window: deadline not reached
	rec.assertNoFlush(t, "1 second into a 2-second window must not flush")
	for i := 1500; i < 2000; i++ {
		if err := b.Add(i); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}
	rec.waitFlushed(t, 1)
	clk.Advance(5 * time.Second) // stale tick after the size flush: disarmed
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got := rec.snapshot()
	if len(got) != 1 || len(got[0]) != 2000 {
		t.Fatalf("batches = %v, want exactly one batch of 2000", batchSizes(got))
	}
}

// TestBatcherInterleavedTimeTriggerPartial: 100 records then 2 seconds
// close the batch at 100; the next window restarts and a later Close
// flushes the final partial batch of 50 (both triggers and the window
// restart in one flow).
func TestBatcherInterleavedTimeTriggerPartial(t *testing.T) {
	clk := newFakeClock()
	rec := newRecordingFlush(0)
	b := NewBatcher(BatcherOptions[int]{
		Flush:    rec.flush,
		Now:      clk.Now,
		newTimer: clk.newTimer,
		Queue:    -1,
	})
	for i := 0; i < 100; i++ {
		if err := b.Add(i); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}
	clk.Advance(2 * time.Second)
	rec.waitFlushed(t, 1)
	for i := 100; i < 150; i++ {
		if err := b.Add(i); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got := rec.snapshot()
	if len(got) != 2 || len(got[0]) != 100 || len(got[1]) != 50 {
		t.Fatalf("batches = %v, want [100, 50]", batchSizes(got))
	}
}

// TestBatcherCloseFlushesFinalPartial: Close always flushes whatever the
// last batch holds, and a second Close changes nothing (idempotent).
func TestBatcherCloseFlushesFinalPartial(t *testing.T) {
	rec := newRecordingFlush(0)
	b := NewBatcher(BatcherOptions[int]{Flush: rec.flush})
	for i := 0; i < 3; i++ {
		if err := b.Add(i); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := b.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	got := rec.snapshot()
	if len(got) != 1 || !reflect.DeepEqual(got[0], []int{0, 1, 2}) {
		t.Fatalf("batches = %v, want exactly [[0 1 2]]", got)
	}
}

// TestBatcherDefaultsSelectDesignSpecValues: NewBatcher with only Flush
// uses Size = 2000 (2001 records -> batches [2000, 1]) and the real
// clock (which never fires inside a fast test).
func TestBatcherDefaultsSelectDesignSpecValues(t *testing.T) {
	rec := newRecordingFlush(0)
	b := NewBatcher(BatcherOptions[int]{Flush: rec.flush})
	for i := 0; i < 2001; i++ {
		if err := b.Add(i); err != nil {
			t.Fatalf("Add(%d): %v", i, err)
		}
	}
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got := rec.snapshot()
	if len(got) != 2 || len(got[0]) != 2000 || len(got[1]) != 1 {
		t.Fatalf("batches = %v, want [2000, 1] (default Size = BatchSize)", batchSizes(got))
	}
}

// TestBatcherFlushErrorStopsPipeline: a failing flush stops the
// pipeline and propagates - Add fails fast with the error, the pump
// performs exactly ONE flush (no retry, no further commits), and Close
// reports the error.
func TestBatcherFlushErrorStopsPipeline(t *testing.T) {
	rec := newRecordingFlush(1) // the FIRST flush call fails
	b := NewBatcher(BatcherOptions[int]{
		Flush: rec.flush,
		Queue: -1,
	})
	for i := 0; i < 2000; i++ {
		// Records 1..1999 always succeed (no flush is possible before
		// the 2000th). The 2000th Add may legitimately return either
		// nil or the flush error: its send completes at the pump's
		// receive, and the pump records the failure immediately
		// after - both interleavings satisfy the Add contract (Add is
		// not a durability guarantee; Close/Err are authoritative).
		if err := b.Add(i); err != nil && i != 1999 {
			t.Fatalf("Add(%d) inside a healthy window: %v", i, err)
		}
	}
	// Record 2001: the pump recorded the error BEFORE it can receive
	// again (same goroutine), so this fast-fail is deterministic.
	if err := b.Add(2000); err == nil {
		t.Fatal("Add after a failed flush returned nil, want the flush error (fast-fail)")
	} else if !errors.Is(err, rec.failErr) {
		t.Errorf("Add error = %v, want the flush callback's error", err)
	}
	if err := b.Close(); err == nil {
		t.Fatal("Close after a failed flush returned nil, want the flush error")
	} else if !errors.Is(err, rec.failErr) {
		t.Errorf("Close error = %v, want the flush callback's error", err)
	}
	if got := rec.callCount(); got != 1 {
		t.Errorf("flush called %d times, want 1 (the pipeline STOPS on the first error)", got)
	}
	if got := rec.snapshot(); len(got) != 1 || len(got[0]) != 2000 {
		t.Errorf("batches = %v, want exactly the one failed batch of 2000", batchSizes(got))
	}
}

// TestBatcherAddAfterCloseIsCleanError: sequential misuse degrades to
// ErrBatcherClosed, not a panic.
func TestBatcherAddAfterCloseIsCleanError(t *testing.T) {
	rec := newRecordingFlush(0)
	b := NewBatcher(BatcherOptions[int]{Flush: rec.flush})
	if err := b.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := b.Add(1); !errors.Is(err, ErrBatcherClosed) {
		t.Fatalf("Add after Close = %v, want ErrBatcherClosed", err)
	}
}

// TestBatcherNilFlushPanics: a batcher without a sink is a programming
// error and must fail loudly at construction.
func TestBatcherNilFlushPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("NewBatcher with nil Flush did not panic")
		}
	}()
	NewBatcher(BatcherOptions[int]{})
}

// batchSizes renders batch lengths for failure messages.
func batchSizes(batches [][]int) []int {
	out := make([]int, len(batches))
	for i, b := range batches {
		out[i] = len(b)
	}
	return out
}
