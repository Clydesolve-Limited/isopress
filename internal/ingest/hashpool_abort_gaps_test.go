package ingest

// Abort-path gap tests (verification round for defects A/C of
// 2026-10-19, PM policy change "in-flight files abort promptly").
// hashpool_test.go pins the pool-level cancellation contract; this file
// closes the hashOne-level and predicate-level gaps the audit
// identified:
//
//   - hashOne with a PRE-CANCELLED ctx must abort BEFORE issuing the
//     open (the defensive first check; currently only reachable through
//     the worker's own priority checks, so it had no direct pin);
//   - a cancellation landing during the FINAL read (the read returns
//     data AND io.EOF after the cancel) must still be honored as an
//     abort: the loop exits on EOF without re-checking ctx, so the
//     POST-LOOP check is the only guard - zero digest, partial bytes
//     counted, Err wrapping context.Canceled. This branch was
//     documented in hashOne but never pinned.
//   - HashResult.Aborted specificity: the production wrap shapes
//     (fmt.Errorf + %w) traverse via errors.Is, while genuine per-file
//     failures (EIO on open or read, plain errno) are NEVER reported as
//     aborts - the classification boundary the T1.6 drain-drop relies
//     on. The EIO subtest of failurepaths_hardening_test.go checks the
//     error identity but never asserted !Aborted() explicitly.
//   - the documented drop policy for a COMPLETED result at
//     cancellation time (channel full, no receiver): the worker already
//     emitted ItemDone for the file, then abandons the result at the
//     delivery select. Pinned deterministically here because it is the
//     mechanism behind the abort-summary count divergence reported to
//     the PM (see the test doc).
//
// Stdlib only; no sleeps in hot paths - channel sync, the seam
// counters, and the settle-then-poll leak pattern carry all ordering.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"isopress/internal/progress"
)

// --- hashOne: pre-cancelled ctx aborts before the open -------------------------

// TestHashOnePreCancelledCtxAbortsBeforeOpen pins hashOne's FIRST
// defensive check: a ctx cancelled before the hash even starts must
// produce an aborted result WITHOUT ever calling the open seam - zero
// digest, zero bytes, Err wrapping context.Canceled (observable via
// Aborted).
func TestHashOnePreCancelledCtxAbortsBeforeOpen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before hashOne is even called

	var opens int
	open := func(string) (io.ReadCloser, error) {
		opens++
		return nil, errors.New("open must never be called for a pre-cancelled hash")
	}

	res := hashOne(ctx, Job{RelPath: "never.bin", AbsPath: "/never.bin"}, make([]byte, 64), open)
	if opens != 0 {
		t.Fatalf("open seam called %d times for a pre-cancelled hash, want 0", opens)
	}
	if !res.Aborted() {
		t.Fatalf("pre-cancelled hashOne result = %+v, want aborted (Err wrapping context.Canceled)", res)
	}
	if !errors.Is(res.Err, context.Canceled) {
		t.Errorf("Err = %v, want errors.Is(context.Canceled)", res.Err)
	}
	if res.Digest != ([32]byte{}) {
		t.Errorf("digest = %x, want the all-zero digest (never mark unverified data, FR-014)", res.Digest)
	}
	if res.Bytes != 0 {
		t.Errorf("Bytes = %d, want 0 (nothing was read)", res.Bytes)
	}
}

// cancelOnReadStream cancels its own ctx on the FIRST Read and returns
// data AND io.EOF in the same call - the "cancellation landed during
// the FINAL read" shape: the read itself completed, and the loop's
// EOF break is followed only by the POST-LOOP ctx check.
type cancelOnReadStream struct {
	cancel context.CancelFunc
	chunk  []byte

	mu    sync.Mutex
	calls int
}

func (r *cancelOnReadStream) Read(p []byte) (int, error) {
	r.mu.Lock()
	r.calls++
	first := r.calls == 1
	r.mu.Unlock()
	if !first {
		return 0, io.EOF // the loop must never read again after the EOF break
	}
	r.cancel() // the cancellation lands while this (final) read is in flight
	return copy(p, r.chunk), io.EOF
}

func (r *cancelOnReadStream) Close() error { return nil }

func (r *cancelOnReadStream) readCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// TestHashOneCancelDuringFinalReadIsHonoredAsAbort pins the post-loop
// guard of hashOne: the final read returns (n>0, io.EOF) and the
// cancellation landed while that read was in flight. The loop exits on
// EOF without re-checking ctx, so only the POST-LOOP check can honor
// the abort - the digest that was already computed must be discarded
// (zeroed) and the result reported aborted. The partial bytes ARE
// counted (the read completed; documented boundary semantics), and no
// further read is issued.
func TestHashOneCancelDuringFinalReadIsHonoredAsAbort(t *testing.T) {
	const chunk = "final-read chunk!"
	rdr := &cancelOnReadStream{
		cancel: nil, // bound below, before the call
		chunk:  []byte(chunk),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rdr.cancel = cancel

	var opens int
	res := hashOne(ctx, Job{RelPath: "final.bin", AbsPath: "/final.bin"}, make([]byte, 64),
		func(string) (io.ReadCloser, error) {
			opens++
			return rdr, nil
		})
	if opens != 1 {
		t.Fatalf("open seam called %d times, want 1", opens)
	}
	if !res.Aborted() {
		t.Fatalf("final-read-cancelled result = %+v, want aborted: the post-loop ctx check must honor a cancellation that landed during the last read", res)
	}
	if !errors.Is(res.Err, context.Canceled) {
		t.Errorf("Err = %v, want errors.Is(context.Canceled)", res.Err)
	}
	if !strings.Contains(res.Err.Error(), "aborted") {
		t.Errorf("Err = %q, want the documented 'aborted' phrasing", res.Err)
	}
	if res.Digest != ([32]byte{}) {
		t.Errorf("digest = %x, want the all-zero digest: the computed digest of an aborted run must be discarded (never mark unverified data)", res.Digest)
	}
	if res.Bytes != int64(len(chunk)) {
		t.Errorf("Bytes = %d, want %d (the final read completed, so its bytes are counted)", res.Bytes, len(chunk))
	}
	if got := rdr.readCalls(); got != 1 {
		t.Errorf("Read was called %d times, want exactly 1 (EOF ends the loop)", got)
	}
}

// --- Aborted() predicate specificity -------------------------------------------

// TestHashResultAbortedPredicateSpecificity pins the exact contract the
// T1.6 drain-drop depends on: Aborted is true ONLY for errors wrapping
// context.Canceled (the cancellation-abort wraps hashOne produces),
// and NEVER for genuine per-file failures - a read/open failure (EIO,
// plain errno) must classify as a failure, not as a drop-before-
// classification abort, or real unreadable files would silently vanish
// from the catalog.
func TestHashResultAbortedPredicateSpecificity(t *testing.T) {
	pathErr := func() error {
		return &os.PathError{Op: "open", Path: "x.bin", Err: syscall.EIO}
	}
	for _, tc := range []struct {
		name string
		res  HashResult
		want bool
	}{
		{"nil Err", HashResult{}, false},
		{"open EIO (production wrap)", HashResult{Err: fmt.Errorf("ingest: open x.bin: %w", pathErr())}, false},
		{"read EIO (production wrap)", HashResult{Err: fmt.Errorf("ingest: read x.bin: %w", &os.PathError{Op: "read", Path: "x.bin", Err: syscall.EIO})}, false},
		{"plain errno, no wrap", HashResult{Err: syscall.EIO}, false},
		{"generic failure, no errno", HashResult{Err: errors.New("ingest: read x.bin: unexpected EOF")}, false},
		{"pre-open abort wrap", HashResult{Err: fmt.Errorf("ingest: hash of x.bin aborted: %w", context.Canceled)}, true},
		{"mid-loop abort wrap", HashResult{Err: fmt.Errorf("ingest: hash of x.bin aborted: %w", context.Canceled)}, true},
		{"bare context.Canceled", HashResult{Err: context.Canceled}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.res.Aborted(); got != tc.want {
				t.Errorf("Aborted() = %v, want %v (Err = %v)", got, tc.want, tc.res.Err)
			}
		})
	}
}

// --- the completed-result drop at cancellation (channel full) -------------------

// lineWatcher is an io.Writer for a progress renderer that snapshots
// every write under a mutex (the renderer goroutine is the only writer;
// the test goroutine never races it) and signals `seen` the first time
// the accumulated output contains the wanted substring.
type lineWatcher struct {
	want string
	seen chan struct{}
	once sync.Once
	mu   sync.Mutex
	buf  bytes.Buffer
}

func newLineWatcher(want string) *lineWatcher {
	return &lineWatcher{want: want, seen: make(chan struct{})}
}

func (w *lineWatcher) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf.Write(p)
	hit := strings.Contains(w.buf.String(), w.want)
	w.mu.Unlock()
	if hit {
		w.once.Do(func() { close(w.seen) })
	}
	return len(p), nil
}

func (w *lineWatcher) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// TestHashPoolCancelDropsCompletedResultWhenChannelFull pins the
// pool's documented delivery policy at its sharpest corner (file doc,
// context policy: "a result with room in the channel is always
// delivered... released by ctx only when the buffer is full at
// cancellation time"): a file that COMPLETED hashing - its ItemDone was
// already emitted, so the progress line counted it - whose result is
// still blocked on a FULL results channel when the cancellation lands
// is DROPPED, not delivered.
//
// Deterministic construction: one worker, no consumer, results buffer
// 2. Files A and B fill the buffer; file C completes hashing (the
// renderer watcher observes the third cumulative line, which is the
// proof that C was NOT aborted - aborted files emit no ItemDone), and
// the worker blocks at C's delivery. The cancel then deterministically
// takes the delivery's ctx-done arm: the send can never succeed (no
// receiver, full buffer).
//
// WHY THIS MATTERS (reported to the PM, test pins the mechanism): the
// scan abort summary's "M completed files" counter counts DELIVERED
// non-aborted results, while the progress line counts ItemDone events.
// For aborted files the two populations agree (no ItemDone, dropped
// result). But for THIS corner they diverge: the progress line says
// "files 3", the delivered population is 2 - so the summary can report
// "N of 2 completed files reconciled" under a progress line that showed
// 3 completed files. The scan.go drain comment's "minus nothing" claim
// is inaccurate for this window.
func TestHashPoolCancelDropsCompletedResultWhenChannelFull(t *testing.T) {
	baseline := settleIngestGoroutines(t)

	root := t.TempDir()
	for _, n := range []string{"a.bin", "b.bin", "c.bin"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("content-"+n), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// A running plain renderer with an advancing clock (every ItemDone
	// passes the cadence throttle) writing into the watcher. The third
	// cumulative line "hash: files 3" is the deterministic signal that
	// all three files COMPLETED before the test cancels.
	watcher := newLineWatcher("hash: files 3, ")
	bus := progress.NewBus()
	var tick time.Duration
	rendDone := make(chan progress.Done, 1)
	go func() {
		rendDone <- progress.New(progress.Options{
			Mode:   progress.ModePlain,
			Stderr: watcher,
			Now: func() time.Time {
				tick += progress.PlainInterval
				return time.Unix(0, 0).Add(tick)
			},
		}).Run(bus)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := NewHashPool(ctx, PoolOptions{Workers: 1, Bus: bus})
	for _, n := range []string{"a.bin", "b.bin", "c.bin"} {
		if err := pool.Submit(Job{RelPath: n, AbsPath: filepath.Join(root, n)}); err != nil {
			t.Fatalf("Submit(%s): %v", n, err)
		}
	}

	// All three files completed: the third cumulative progress line is
	// on the wire. The results channel (capacity 2) now holds A and B,
	// and C's result is blocked at the delivery select - the test is
	// still not draining, so the send can never succeed.
	select {
	case <-watcher.seen:
	case <-time.After(30 * time.Second):
		t.Fatalf("the third cumulative progress line never rendered (output so far: %q)", watcher.String())
	}

	cancel() // the delivery select now deterministically takes the ctx-done arm

	// Drain only AFTER the cancellation: C's completed result must be
	// gone (dropped), A and B (which had room) must be delivered whole.
	got := drainResults(pool.Results())
	if err := pool.Wait(); !errors.Is(err, context.Canceled) {
		t.Errorf("Wait = %v, want context.Canceled", err)
	}
	if len(got) != 2 {
		t.Fatalf("delivered %d results, want 2 (A and B had room; C's COMPLETED result is dropped at cancellation)", len(got))
	}
	byRel := resultsByRel(got)
	for _, n := range []string{"a.bin", "b.bin"} {
		r, ok := byRel[n]
		if !ok {
			t.Fatalf("missing delivered result %s (got %+v)", n, got)
		}
		if r.Err != nil || r.Bytes != int64(len("content-"+n)) {
			t.Errorf("%s = %+v, want a clean completed result (results with room are always delivered, even at cancellation)", n, r)
		}
	}

	// The progress line counted THREE completed files (C's ItemDone was
	// emitted before its result was abandoned) - pinning both halves of
	// the divergence this test exists to document.
	if out := watcher.String(); !strings.Contains(out, "hash: files 3, ") {
		t.Errorf("rendered output %q lacks the third cumulative line: all three files must have emitted ItemDone", out)
	}

	bus.Finish(progress.Done{Summary: "aborted"}) // guaranteed-delivery join
	<-rendDone
	waitIngestGoroutinesBaseline(t, baseline)
}
