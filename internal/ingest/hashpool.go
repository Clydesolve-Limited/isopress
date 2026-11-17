package ingest

// T1.4: the hash worker pool of DESIGN_SPEC 3.3 (hashing concurrency
// model), consuming the synchronous Job stream of T1.3's walker and
// producing HashResults for the batched catalog writer (FR-002, D-09).
//
// Shape (DESIGN_SPEC 3.3, end to end):
//
//	walker (T1.3, synchronous) --Submit--> buffered jobs channel
//	  --> N hash workers (N = min(GOMAXPROCS, 8) by default,
//	      overridable via PoolOptions.Workers / --hash-workers)
//	  --> buffered results channel --> consumer (the batcher below,
//	      then T1.6's catalog writer).
//
// Every worker opens the file, reads through ONE fixed 1 MiB buffer
// (HashReadBufferSize), and streams into sha256.New(). A file's bytes
// are never held in memory as a whole, so hashing a multi-GiB file costs
// exactly one buffer per worker; the worker count cap of 8 therefore
// bounds the pool's read memory at 8 MiB (the 3.3 memory-cap rationale).
//
// Failure semantics (FR-003): an open or mid-stream read failure
// produces an error HashResult; the pool NEVER dies on a read error and
// keeps processing queued jobs. The unreadable file's catalog status
// ('unreadable' plus status_detail) is T1.5's wiring; this pool only
// guarantees the error reaches the consumer inside the result stream.
//
// Ordering (documented): results may complete OUT OF ORDER (workers
// finish at file-speed, not submission order). The consumer (T1.6's
// catalog writer) processes them as they come; nothing in the pipeline
// depends on walk order.
//
// Progress (DESIGN_SPEC 9.1, 3.6): the pool emits exactly one
// Phase{Name: "hash", TotalKnown: false} at construction (totals are not
// knowable until the walk completes, per 3.6) and one ItemDone per
// processed file. An ABORTED in-flight file (see the context policy
// below) emits NO ItemDone: it did not complete, so it must not inflate
// the completed-files count the renderer shows. A nil Bus is valid and
// silences all emission. The terminal Done event is T1.6's to emit.
//
// Context policy (documented, test-pinned; PM policy change 2026-10-19,
// superseding the original T1.4 "in-flight completes" rule): cancelling
// ctx aborts INTAKE - Submit returns ctx.Err(), which T1.3's contract
// turns into a walk-abort - and workers stop before taking new jobs.
// Cancellation is checked WITH PRIORITY at every decision point (Submit,
// the worker loop, and every read inside hashOne), so "intake aborts,
// queued jobs are abandoned" is deterministic rather than a coin-flip
// against a ready queue in a two-case select. Files already in flight
// ABORT PROMPTLY at the next read boundary: hashOne re-checks ctx.Err()
// around every read, so cancelling no longer waits for every worker to
// grind through its in-flight multi-GiB file (the minutes-long Ctrl+C
// response this policy change fixes). An aborted file returns a
// ctx-canceled HashResult - zero digest, the partial Bytes actually
// read, Err wrapping context.Canceled (observable via HashResult.Aborted).
// The result is delivered when the results channel has room and dropped
// when it is not, exactly like any other result at cancellation time;
// the scan wiring (T1.6) drops aborted results entirely: an ABORTED file
// gets NO catalog record and NO failure warning (FR-014: never mark
// unverified data - the rescan workflow captures the file later). Wait
// returns the ctx error on the cancelled path, so the caller learns
// about cancellation through the one join call. The caller MUST drain
// Results() until it closes (a plain range loop); Wait completes only
// after every result has been consumed.
//
// T1.6 wiring story (the seam this pool exposes):
//
//	pool := ingest.NewHashPool(ctx, ingest.PoolOptions{
//	    Workers: hashWorkersFlag, // 0 selects min(GOMAXPROCS, 8)
//	    Bus:     bus,
//	})
//	batcher := ingest.NewBatcher(ingest.BatcherOptions[ingest.HashResult]{
//	    Flush: catalogInsertBatch, // T1.6 supplies the real INSERT
//	})
//	go func() {
//	    for r := range pool.Results() {
//	        batcher.Add(r)
//	    }
//	    batcher.Close()
//	}()
//	wres, err := ingest.Walk(root, matcher, pool.Submit, bus) // emit = Submit
//	pool.Close()   // intake over (idempotent)
//	werr := pool.Wait()
//	berr := batcher.Close() // also safe to join here; returns the first flush error
//
// The walk stays synchronous (T1.3 contract); pool.Submit is the emit
// function, which naturally throttles the walk to worker capacity
// (backpressure), and the batcher throttles the pool to catalog speed.
//
// Concurrency observability (for tests and diagnostics): Workers reports
// the effective worker count and HighWater the maximum number of
// workers concurrently inside a hash operation (an atomic high-water
// mark, asserted never to exceed the worker count). PoolOptions.OpenFile
// is the injection seam tests use to synchronize on worker activity
// without sleep-racing; it is also the coordination point T1.5's
// change-during-scan test will use to mutate a file mid-hash.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"

	"isopress/internal/progress"
)

// MaxHashWorkers is the hard cap on hash workers (DESIGN_SPEC 3.3):
// hashing is sequential-read bound, so more than a handful of parallel
// readers saturate disks, and 8 x 1 MiB buffers caps read memory.
const MaxHashWorkers = 8

// HashReadBufferSize is the per-worker streaming read buffer
// (DESIGN_SPEC 3.3 "reads through a 1 MiB buffer"). This constant IS
// the pool's memory bound: each worker holds exactly one buffer for its
// whole lifetime plus a fixed-size sha256 state, so a file of any size
// costs no additional memory (asserted by the sparse-file test and by
// this constant's pin test).
const HashReadBufferSize = 1 << 20

// DefaultHashWorkers returns the pool size the design mandates when
// --hash-workers is not supplied: min(GOMAXPROCS, 8) (DESIGN_SPEC 3.3).
func DefaultHashWorkers() int {
	n := runtime.GOMAXPROCS(0)
	if n > MaxHashWorkers {
		n = MaxHashWorkers
	}
	if n < 1 {
		n = 1
	}
	return n
}

// ErrPoolClosed is returned by Submit after Close. (Submitting
// CONCURRENTLY with Close is a contract violation - the supported
// wiring has exactly one intake goroutine, the walker; sequential
// misuse degrades to this error rather than a panic.)
var ErrPoolClosed = errors.New("ingest: hash pool is closed")

// HashResult is the completed record of one hash job: the Job it was
// built from (RelPath, AbsPath, and the fsmeta.Meta captured at WALK
// time, the DESIGN_SPEC 3.5 point-1 baseline), the SHA-256 digest, the
// number of bytes actually read, and any open/read error.
//
// Digest is the RAW 32-byte digest ([32]byte), not hex: the catalog
// `files.hash` column is a BLOB holding the raw sha256 bytes
// (DESIGN_SPEC 2.3, D-09), so keeping raw bytes avoids a per-record
// encode/decode round trip; hex conversion happens only at display
// boundaries. Digest is all-zero when Err is non-nil (an unreadable
// file has NO hash, FR-003).
//
// Bytes is the number of bytes actually streamed: equal to the file
// size on success, possibly a partial count on a mid-stream read error
// (progress accounting uses it; the catalog writer must not store a
// record whose Err is non-nil as an active hashed row - that wiring is
// T1.5/T1.6's).
//
// The post-hash re-lstat of DESIGN_SPEC 3.5 point 1 (mid-scan change
// detection, T1.5's territory) compares a fresh fsmeta.PathMeta(AbsPath)
// against Meta with fsmeta.DiffFields: the pool deliberately captures
// the WALK-time baseline only and leaves that comparison to the
// consumer, so the hook needs no pool change.
type HashResult struct {
	// Job is the walk-time job: promoted fields RelPath, AbsPath, Meta.
	Job
	// Digest is the raw 32-byte SHA-256 digest (catalog BLOB form);
	// zero when Err != nil.
	Digest [32]byte
	// Bytes is the number of bytes actually read from the file.
	Bytes int64
	// Err is the open/read failure (nil on success). A failed file does
	// not stop the pool (FR-003). On the cancellation path it wraps
	// context.Canceled (see Aborted).
	Err error
}

// Aborted reports whether this result is a cancellation-aborted in-flight
// hash: Err wraps context.Canceled because ctx was cancelled while the
// worker was reading the file, so the digest is meaningless (zero, by the
// Err != nil invariant above) and Bytes is only the partial count read
// before the abort. Consumers MUST NOT treat an aborted result as a
// per-file failure: it records nothing about the file's state (the scan
// wiring drops it before classification so it never becomes a catalog row
// or a warning - FR-014, never mark unverified data). The rescan
// workflow captures the file later. Matching is STRICTLY
// context.Canceled (errors.Is): a future consumer that passes a
// DEADLINE'd context would have its DeadlineExceeded aborts classify as
// per-file failures here - the pool's contract assumes a cancel-only
// context.
func (r HashResult) Aborted() bool {
	return r.Err != nil && errors.Is(r.Err, context.Canceled)
}

// PoolOptions configures a HashPool. The zero value (Workers 0, nil
// Bus, nil OpenFile) selects the production defaults.
type PoolOptions struct {
	// Workers is the hash worker count: the --hash-workers override
	// value. 0 or negative selects DefaultHashWorkers() =
	// min(GOMAXPROCS, 8) (DESIGN_SPEC 3.3).
	Workers int
	// Bus receives the Phase (at construction) and one ItemDone per
	// processed file; nil silences all progress output.
	Bus *progress.Bus
	// PhaseName overrides the emitted phase name; empty selects
	// "hash" (DESIGN_SPEC 3.6). T1.6's scan wiring sets "scan(hash)"
	// to match the renderer-prefix examples of DESIGN_SPEC 9.3.
	PhaseName string
	// OpenFile overrides opening of each job's file. The production
	// default is os.Open. It exists as an instrumentation seam:
	// tests synchronize on worker activity through it (no sleep
	// races) and inject open/read failures; T1.5's
	// change-during-scan test coordinates a mid-hash mutation
	// through it.
	OpenFile func(path string) (io.ReadCloser, error)
}

// HashPool is the worker pool of DESIGN_SPEC 3.3. Construct with
// NewHashPool, feed jobs with Submit (as the walker's emit callback),
// drain Results, then Close and Wait. See the package file doc for the
// full wiring story and the context/cancellation policy.
type HashPool struct {
	jobs     chan Job
	results  chan HashResult
	bus      *progress.Bus
	openFile func(string) (io.ReadCloser, error)
	workers  int

	ctx       context.Context
	wg        sync.WaitGroup
	closeOnce sync.Once
	closed    atomic.Bool
	done      chan struct{}

	active    atomic.Int64 // workers currently inside a hash operation
	highWater atomic.Int64 // max observed value of active
}

// NewHashPool builds the pool, emits the hash Phase (when a Bus is
// supplied), and starts the workers plus the results-closing goroutine.
// The Phase is emitted here so it precedes every ItemDone (the renderer
// resets its counters on Phase arrival, DESIGN_SPEC 9.1).
func NewHashPool(ctx context.Context, opts PoolOptions) *HashPool {
	n := opts.Workers
	if n <= 0 {
		n = DefaultHashWorkers()
	}
	open := opts.OpenFile
	if open == nil {
		open = osOpen
	}
	phase := opts.PhaseName
	if phase == "" {
		phase = "hash"
	}

	p := &HashPool{
		// Both queues are bounded at 2x the worker count: enough to
		// keep every worker fed while a batch drains, small enough
		// to bound memory and keep backpressure honest end to end
		// (walk -> pool -> batcher -> SQL, DESIGN_SPEC 3.3).
		jobs:     make(chan Job, 2*n),
		results:  make(chan HashResult, 2*n),
		bus:      opts.Bus,
		openFile: open,
		workers:  n,
		ctx:      ctx,
		done:     make(chan struct{}),
	}
	if p.bus != nil {
		// Totals unknown at walk time for full-tree scans
		// (DESIGN_SPEC 3.6): counts-and-bytes mode only.
		p.bus.Emit(progress.Phase{Name: phase, TotalKnown: false})
	}
	p.wg.Add(n)
	for i := 0; i < n; i++ {
		go p.worker()
	}
	go func() {
		p.wg.Wait()
		close(p.results)
		close(p.done)
	}()
	return p
}

// Submit enqueues one job for hashing. It blocks when the queue is
// full (backpressure on the walker) and returns ctx.Err() once the
// context is cancelled, which aborts the walk via T1.3's emit-error
// contract. After Close it returns ErrPoolClosed. Single-producer
// contract: the walker goroutine is the only Submit caller; calling
// Submit concurrently with Close is unsupported.
func (p *HashPool) Submit(job Job) error {
	if p.closed.Load() {
		return ErrPoolClosed
	}
	// Cancellation has PRIORITY over queue traffic: after cancellation
	// Submit always fails fast, rather than letting the runtime's
	// random select choice accept the job into a queue no worker will
	// ever drain.
	if err := p.ctx.Err(); err != nil {
		return err
	}
	select {
	case p.jobs <- job:
		return nil
	case <-p.ctx.Done():
		return p.ctx.Err()
	}
}

// Results returns the completed-record channel. It is closed after all
// workers have exited; callers drain it with a range loop. Results may
// arrive out of submission order (documented in the file doc).
func (p *HashPool) Results() <-chan HashResult { return p.results }

// Close signals end of intake by closing the job queue. It is
// idempotent; in the absence of cancellation, in-flight files finish
// hashing and their results stay deliverable (see the file doc's context
// policy). Submit must not be called after Close (single-producer
// contract; sequential misuse returns ErrPoolClosed).
func (p *HashPool) Close() {
	p.closeOnce.Do(func() {
		p.closed.Store(true)
		close(p.jobs)
	})
}

// Wait blocks until every worker has exited and Results is closed, then
// reports the context error if the run was cancelled (nil on a clean
// completion). It is the one join call: on the cancellation path the
// caller learns about it here even if the walk already surfaced it via
// Submit. Wait does not return before the consumer has drained Results,
// so a caller that has not started a consumer must not call Wait.
func (p *HashPool) Wait() error {
	<-p.done
	if err := p.ctx.Err(); err != nil {
		return err
	}
	return nil
}

// Workers reports the effective worker count (DefaultHashWorkers when
// no override was supplied).
func (p *HashPool) Workers() int { return p.workers }

// HighWater reports the maximum number of workers ever observed
// concurrently inside a hash operation. The bound HighWater <= Workers
// is the "at most N concurrent readers" memory-cap assertion of
// DESIGN_SPEC 3.3; tests observe worker activity through this counter
// and through the OpenFile seam.
func (p *HashPool) HighWater() int64 { return p.highWater.Load() }

// worker is the hash loop: take a job, hash it through the fixed
// buffer, emit progress, deliver the result. See the file doc for the
// cancellation policy at each step.
func (p *HashPool) worker() {
	defer p.wg.Done()
	buf := make([]byte, HashReadBufferSize)
	for {
		// Cancellation has PRIORITY over queue traffic (checked here
		// and in the select below): once a worker observes
		// cancellation it takes no NEW job, so queued jobs are
		// abandoned deterministically instead of racing the runtime's
		// random select choice. An in-flight job (below) aborts at
		// its next read boundary (PM policy change 2026-10-19).
		if p.ctx.Err() != nil {
			return
		}
		select {
		case <-p.ctx.Done():
			return
		case job, ok := <-p.jobs:
			if !ok {
				return // intake closed, queue drained
			}
			p.enter()
			res := hashOne(p.ctx, job, buf, p.openFile)
			p.leave()
			// One ItemDone per COMPLETED file, success or failure,
			// carrying the bytes actually read (DESIGN_SPEC 9.1;
			// failures are surfaced by T1.5/T1.6 via progress.Error
			// and run_events). An ABORTED result (ctx cancelled
			// mid-file) emits NO ItemDone: the file did not
			// complete, so it must not inflate the renderer's
			// completed-files count (file doc, context policy).
			if p.bus != nil && !res.Aborted() {
				p.bus.Emit(progress.ItemDone{Bytes: res.Bytes, Path: res.RelPath})
			}
			// Deliver, released by ctx only when the buffer is full
			// at cancellation time: a result with room in the
			// channel is always delivered, even after cancel.
			select {
			case p.results <- res:
				continue
			default:
			}
			select {
			case p.results <- res:
			case <-p.ctx.Done():
				return
			}
		}
	}
}

// enter/leave bracket one hash operation for the HighWater counter
// (lock-free max update; contention is one CAS per file).
func (p *HashPool) enter() {
	n := p.active.Add(1)
	for {
		hw := p.highWater.Load()
		if n <= hw || p.highWater.CompareAndSwap(hw, n) {
			return
		}
	}
}

func (p *HashPool) leave() { p.active.Add(-1) }

// osOpen adapts os.Open to the OpenFile seam's signature.
func osOpen(path string) (io.ReadCloser, error) { return os.Open(path) }

// hashOne streams one file through buf into a fresh SHA-256 state. The
// only per-file memory is the hash state itself: buf is the caller's
// fixed buffer and is reused for the worker's whole lifetime, so a file
// of any size (the 4 GiB sparse case) costs nothing extra. An open or
// mid-stream read failure yields an error result (Bytes = partial
// count, Digest zero); the pool continues (FR-003).
//
// Cancellation (PM policy change 2026-10-19): ctx is checked before the
// open and at the TOP of every read iteration, so a cancelled hash
// aborts at its next read boundary - the next f.Read is simply never
// issued. The returned result carries Err wrapping context.Canceled
// (HashResult.Aborted reports true), a zero digest, and the partial
// Bytes streamed so far. Note the boundary semantics honestly, for BOTH
// blocking stages: a Read already BLOCKED in the kernel (slow external
// media) cannot be interrupted from here - the abort lands as soon as
// that read returns - and the OPEN itself is a boundary too: os.Open on
// hung external media or a stalled NFS mount can block uninterruptibly,
// so the abort only lands after the open returns (the same hardware
// class the stakeholder reported for blocked reads). The caller (the
// worker) closes the file via the defer below, so the handle never
// leaks on the abort path.
func hashOne(ctx context.Context, job Job, buf []byte, open func(string) (io.ReadCloser, error)) HashResult {
	res := HashResult{Job: job}
	if err := ctx.Err(); err != nil {
		res.Err = fmt.Errorf("ingest: hash of %s aborted: %w", job.RelPath, err)
		return res
	}
	f, err := open(job.AbsPath)
	if err != nil {
		res.Err = fmt.Errorf("ingest: open %s: %w", job.RelPath, err)
		return res
	}
	defer f.Close() //nolint:errcheck // read-only handle: a close error carries no information here

	h := sha256.New()
	for {
		// Abort at the next read boundary (cheap nil-compare; see the
		// function doc for why a blocked Read cannot be pre-empted).
		if err := ctx.Err(); err != nil {
			res.Err = fmt.Errorf("ingest: hash of %s aborted: %w", job.RelPath, err)
			return res
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n]) // hash.Hash.Write never returns an error
			res.Bytes += int64(n)
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			res.Err = fmt.Errorf("ingest: read %s: %w", job.RelPath, rerr)
			return res
		}
	}
	// A cancellation that landed while the FINAL read was in flight is
	// still honored: the loop exited on EOF without re-checking ctx, so
	// the digest exists but the run is being abandoned - reporting the
	// file as aborted (rather than delivering a digest nothing will
	// record) keeps the consumer's drop/no-record handling uniform.
	// Digest needs no reset here: it is only ever assigned on the
	// success path below this check, so the zero-digest abort invariant
	// (Err != nil => Digest zero, per the HashResult doc) holds by
	// construction.
	if err := ctx.Err(); err != nil {
		res.Err = fmt.Errorf("ingest: hash of %s aborted: %w", job.RelPath, err)
		return res
	}
	copy(res.Digest[:], h.Sum(nil))
	return res
}
