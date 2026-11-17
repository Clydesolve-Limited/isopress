package ingest

// T1.4: the batched catalog-writer seam of DESIGN_SPEC 3.3 ("one
// catalog writer goroutine ... batched transactions, commit every 2,000
// records or 2 seconds, whichever first"). THIS COMPONENT GUARANTEES
// BATCH BOUNDARIES AND BACKPRESSURE ONLY: the actual catalog SQL
// arrives in T1.6, which supplies the flush callback (a BatchFunc that
// runs one INSERT transaction per batch). Nothing here knows about
// SQLite, the files table, or reconciliation.
//
// Trigger semantics (the acceptance surface): a batch closes when
// EITHER Size records have accumulated (default BatchSize = 2000) OR
// Interval has elapsed since the FIRST record of the current batch
// (default BatchInterval = 2 s), whichever comes first. The interval
// window restarts at every flush. A final partial batch is flushed when
// Close is called.
//
// Error semantics: an error returned by the flush callback STOPS THE
// PIPELINE and propagates. The pump goroutine records the first error,
// stops flushing, and drains-and-discards any further intake (so a
// producer blocked on backpressure is always released); Err and Close
// both report the first error, and Add fails fast with it. The caller
// decides how the failed run maps to exit codes (report.Classify);
// this component only guarantees the error cannot get lost.
//
// Concurrency model: exactly one pump goroutine owns the batch state,
// so there is no lock on the hot path. Add is the producer-side
// backpressure valve (it blocks when the intake queue is full, coupling
// the catalog writer's speed to hashing, which is the DESIGN_SPEC 3.3
// "SQL never blocks hashing" flow control: the pool's bounded result
// queue keeps hashing fed while a transaction is in flight). Single
// producer contract: Add must not be called concurrently with, or
// after, Close; sequential misuse degrades to ErrBatcherClosed rather
// than a panic.
//
// Fake-clock seam (T1.1 pattern, extended): BatcherOptions.Now
// injects the time source, and the unexported newTimer field injects
// the one-shot wake timer the 2-second trigger arms. The production
// default wraps time.NewTimer; tests supply a clock whose Advance both
// moves Now and fires the armed timer, so both triggers are exercised
// deterministically with zero wall-clock waiting and zero sleeps.

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// BatchSize is the record-count trigger of DESIGN_SPEC 3.3: a batch
// commits at 2,000 records. Exposed because T1.6's catalog writer and
// its tests reference the same number.
const BatchSize = 2000

// BatchInterval is the time trigger of DESIGN_SPEC 3.3: a batch
// commits 2 seconds after its first record, whichever comes first with
// BatchSize.
const BatchInterval = 2 * time.Second

// ErrBatcherClosed is returned by Add after (or racing) Close.
var ErrBatcherClosed = errors.New("ingest: batcher is closed")

// BatchFunc flushes one batch of records (one catalog transaction in
// T1.6's wiring). The slice is only valid for the duration of the
// call: a callback that retains records past the call MUST copy them
// (the batcher reuses the backing array). A non-nil error stops the
// pipeline (see the file doc).
type BatchFunc[T any] func(batch []T) error

// BatcherOptions configures a Batcher. The zero value (Flush only)
// selects the DESIGN_SPEC 3.3 defaults.
type BatcherOptions[T any] struct {
	// Flush receives each completed batch; required, never nil.
	Flush BatchFunc[T]
	// Now is the clock seam (default time.Now), the T1.1 fake-clock
	// pattern.
	Now func() time.Time
	// Size is the record-count trigger (default BatchSize = 2000).
	Size int
	// Interval is the time trigger measured from the batch's first
	// record (default BatchInterval = 2 s).
	Interval time.Duration
	// Queue is the intake channel capacity (default 2 * Size). A
	// NEGATIVE value selects an unbuffered channel, a test seam that
	// lets a test sequence records and clock advances
	// deterministically (an Add returns only once the pump has taken
	// the record).
	Queue int
	// newTimer arms a one-shot wake timer firing after d; the
	// returned channel delivers at most once and stop is
	// idempotent. nil selects a time.NewTimer-based implementation.
	// Unexported and test-only: the fake clock provides its own so
	// the 2-second trigger is exercisable without wall-clock waits.
	newTimer func(d time.Duration) (<-chan time.Time, func())
}

// Batcher turns a stream of records into batches with the two
// DESIGN_SPEC 3.3 triggers. See the file doc for the full contract.
type Batcher[T any] struct {
	in       chan T
	dead     chan struct{} // closed when the pump has fully exited
	flush    BatchFunc[T]
	now      func() time.Time
	size     int
	interval time.Duration
	newTimer func(d time.Duration) (<-chan time.Time, func())

	closeOnce sync.Once
	mu        sync.Mutex // guards firstErr (written by pump, read by all)
	firstErr  error
}

// NewBatcher starts the pump goroutine. Panics if Flush is nil: a
// batcher without a sink silently swallows records, which is exactly
// the class of bug the panic makes impossible.
func NewBatcher[T any](opts BatcherOptions[T]) *Batcher[T] {
	if opts.Flush == nil {
		panic("ingest: NewBatcher requires a non-nil Flush callback")
	}
	size := opts.Size
	if size <= 0 {
		size = BatchSize
	}
	interval := opts.Interval
	if interval <= 0 {
		interval = BatchInterval
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	nt := opts.newTimer
	if nt == nil {
		nt = func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTimer(d)
			return t.C, func() { t.Stop() }
		}
	}
	queue := opts.Queue
	if queue == 0 {
		queue = 2 * size
	} else if queue < 0 {
		queue = 0 // unbuffered test seam
	}
	b := &Batcher[T]{
		in:       make(chan T, queue),
		dead:     make(chan struct{}),
		flush:    opts.Flush,
		now:      now,
		size:     size,
		interval: interval,
		newTimer: nt,
	}
	go b.pump()
	return b
}

// Add enqueues one record. It blocks when the intake queue is full
// (backpressure) and fails fast with the first flush error once the
// pipeline has stopped; after Close it returns ErrBatcherClosed. The
// authoritative completion signal is Close (or Err): Add returning nil
// means the record was accepted by the pump, not that it is committed.
func (b *Batcher[T]) Add(rec T) error {
	if err := b.Err(); err != nil {
		return err
	}
	// Best-effort fast path so sequential Add-after-Close misuse gets a
	// clean error instead of a send-on-closed-channel panic (concurrent
	// Add with Close remains a documented contract violation).
	select {
	case <-b.dead:
		return ErrBatcherClosed
	default:
	}
	select {
	case b.in <- rec:
		// Re-check: a flush failure may have landed while this record
		// waited on the queue; surface it so the producer stops early.
		if err := b.Err(); err != nil {
			return err
		}
		return nil
	case <-b.dead:
		if err := b.Err(); err != nil {
			return err
		}
		return ErrBatcherClosed
	}
}

// Close ends intake, waits for the pump to flush the final partial
// batch, and returns the first flush error (nil on a clean run). It is
// safe to call more than once and from multiple goroutines; every
// caller observes the same result. Close must not run concurrently
// with Add (single producer contract).
func (b *Batcher[T]) Close() error {
	b.closeOnce.Do(func() {
		close(b.in)
	})
	<-b.dead
	return b.Err()
}

// Err reports the first flush error recorded so far (nil while the
// pipeline is healthy). It is safe to call from any goroutine at any
// time, including while the pump is running.
func (b *Batcher[T]) Err() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.firstErr
}

func (b *Batcher[T]) recordErr(err error) {
	if err == nil {
		return
	}
	b.mu.Lock()
	if b.firstErr == nil {
		b.firstErr = fmt.Errorf("ingest: batch flush: %w", err)
	}
	b.mu.Unlock()
}

// pump is the single batch-owner goroutine. All batch state (slice,
// window start, timer) lives on its stack: no locks on the hot path.
func (b *Batcher[T]) pump() {
	defer close(b.dead)

	var batch []T
	var firstAt time.Time
	var tickC <-chan time.Time
	var stopTimer func()
	disarm := func() {
		if stopTimer != nil {
			stopTimer()
			stopTimer = nil
			tickC = nil
		}
	}
	flushNow := func() {
		disarm() // the window restarts with the next batch
		b.recordErr(b.flush(batch))
		batch = batch[:0] // Flush may not retain the slice (BatchFunc doc)
	}

	for {
		select {
		case rec, ok := <-b.in:
			if !ok {
				// Intake closed (Close): final partial batch, then
				// exit. After a flush failure the residue is
				// discarded; the error is already recorded.
				if len(batch) > 0 && b.Err() == nil {
					b.recordErr(b.flush(batch))
				}
				return
			}
			if b.Err() != nil {
				continue // pipeline stopped: drain-and-discard, never block producers
			}
			if len(batch) == 0 {
				// First record of a new batch starts the window
				// (DESIGN_SPEC 3.3: 2 s since the FIRST record).
				firstAt = b.now()
				tickC, stopTimer = b.newTimer(firstAt.Add(b.interval).Sub(b.now()))
			}
			batch = append(batch, rec)
			if len(batch) >= b.size {
				flushNow() // count trigger
			}
		case <-tickC:
			// Time trigger: flush only if the batch is open; a
			// stale tick cannot exist because every flush disarms.
			if len(batch) == 0 {
				continue
			}
			flushNow()
		}
	}
}
