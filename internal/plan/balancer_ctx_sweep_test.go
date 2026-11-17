package plan

// T6.3 fix round: context-probe cadence and abort-latency pins for
// the balancer's batched ctx checks (ctxCheckInterval = 4096).
//
// Three properties are asserted, none with sleeps or goroutines:
//
//  1. CADENCE: over a 4500-file plan (~9000 per-item iterations) the
//     balancer probes ctx a HANDFUL of times (7, one per documented
//     site), not per iteration - a per-iteration probe inside the
//     placement loop would show thousands of calls here. This is the
//     hot-path-cost proof for the heap/gap inner loops: none of them
//     carries a probe.
//  2. EVERY PROBE HONORS CANCEL: a scripted context that starts
//     returning context.Canceled at call K fails Balance EXACTLY at
//     call K, for K sweeping every probe site (post-load, item
//     construction entry + 4096, pre-sort, placement entry + 4096,
//     empty-dir pass entry). Combined with (1), a cancel landing at
//     any point in the plan is observed within at most
//     ctxCheckInterval (4096) further iterations of the loop it
//     landed in - the abort-latency bound, measured in iterations
//     rather than wall time (no timing flakiness possible).
//  3. MID-LOOP (not just entry): the empty-dir pass is swept at its
//     SECOND probe (i == 4096), i.e. 4096 dirs INTO the pass, and
//     the placement loop likewise at its second probe - aborts still
//     land exactly on schedule with zero persisted state.
//
// The counting context works because the whole compute phase is
// synchronous in one goroutine: fakeStore ignores ctx entirely (its
// reads take `_ context.Context`), so Err() is invoked by
// balanceCtxCheck and nothing else - the count IS the probe count.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// countingCtx is a scripted context: live for the first cancelAt-1
// Err() probes, context.Canceled from probe cancelAt on (cancelAt 0
// = always live). It counts probes and closes Done when it cancels so
// the context contract (Err non-nil => Done closed) holds even though
// nothing in the compute phase selects on Done.
type countingCtx struct {
	parent   context.Context
	done     chan struct{}
	cancelAt int
	calls    int
	fired    bool
}

func newCountingCtx(cancelAt int) *countingCtx {
	return &countingCtx{parent: context.Background(), done: make(chan struct{}), cancelAt: cancelAt}
}

func (c *countingCtx) Deadline() (time.Time, bool) { return time.Time{}, false }

func (c *countingCtx) Done() <-chan struct{} { return c.done }

func (c *countingCtx) Err() error {
	c.calls++
	if c.cancelAt > 0 && c.calls >= c.cancelAt {
		if !c.fired {
			c.fired = true
			close(c.done)
		}
		return context.Canceled
	}
	return nil
}

func (c *countingCtx) Value(key any) any { return c.parent.Value(key) }

// ctxSweepFixture: `files` whole files (60 B each, well under the
// 100000-byte payload budget, so one item per file) plus `dirs`
// active empty dirs.
func ctxSweepFixture(files, dirs int) *fakeStore {
	s := newFakeStore(testProfile(100000))
	for i := 1; i <= files; i++ {
		addFile(s, int64(i), fmt.Sprintf("d/f%05d.bin", i), 60)
	}
	for d := 1; d <= dirs; d++ {
		s.dirs[int64(1000000+d)] = &fakeDir{relPath: fmt.Sprintf("empty/d%05d", d), active: true}
	}
	return s
}

// ctxProbeSites lists the balancer's probe sites IN CALL ORDER for
// the ctxSweepFixture(4500, 3) shape, with the exact Err() call
// number each site owns. If balancer.go gains, loses or reorders a
// probe, the control assertion below fails first (update this map
// deliberately - do not loosen it: the sweep's site NAMES are only as
// truthful as this table).
var ctxProbeSites = []struct {
	name string
	call int
}{
	{"post-load", 1},               // after the 4 catalog reads
	{"item-construction-entry", 2}, // files loop i == 0
	{"item-construction-i4096", 3}, // files loop i == 4096
	{"pre-sort", 4},                // before the (uninterruptible) sortItems
	{"placement-entry", 5},         // WFD placement loop i == 0
	{"placement-i4096", 6},         // WFD placement loop i == 4096
	{"empty-dir-pass-entry", 7},    // dirs loop i == 0
}

// TestBalanceCtxProbeCadenceIsBatched pins property 1: the probes
// are batched loop-boundary checks, never per-iteration. 4500 files
// -> 4500 construction iterations + 4500 placement iterations + 3
// dir iterations, and exactly the 7 documented probes fire.
func TestBalanceCtxProbeCadenceIsBatched(t *testing.T) {
	const files, dirs = 4500, 3 // > ctxCheckInterval: each per-item loop must probe at i==0 and i==4096 ONLY
	s := ctxSweepFixture(files, dirs)
	c := newCountingCtx(0)

	p, err := Balance(c, s, s.profile)
	if err != nil {
		t.Fatalf("Balance (live ctx): %v", err)
	}
	if p.Result.FilesAssigned != files {
		t.Errorf("FilesAssigned = %d, want %d", p.Result.FilesAssigned, files)
	}
	if p.Result.DirsAssigned != dirs {
		t.Errorf("DirsAssigned = %d, want %d", p.Result.DirsAssigned, dirs)
	}
	if c.calls != len(ctxProbeSites) {
		t.Errorf("Balance probed ctx %d times over %d file + %d dir iterations, want exactly %d (the documented "+
			"batched sites). A per-iteration probe would report thousands; a dropped site would report fewer - "+
			"update ctxProbeSites only if balancer.go's probe layout genuinely changed",
			c.calls, files, dirs, len(ctxProbeSites))
	}
	// The batching bound itself, independent of the exact count: far
	// fewer probes than iterations (a 100x margin here).
	if c.calls*100 > files+dirs {
		t.Errorf("probe count %d is not batched over %d iterations", c.calls, files+dirs)
	}
}

// TestBalanceAbortsAtEveryCtxProbeSite sweeps properties 2 + 3: for
// EVERY documented probe site, a cancel that first surfaces at that
// exact call aborts Balance at that exact call (c.calls ==
// site.call - one probe of latency, never a probe later), with a
// ctx-anchored error, empty write ops and NO transaction.
func TestBalanceAbortsAtEveryCtxProbeSite(t *testing.T) {
	// Control leg first: the probe map must match reality before its
	// names can be trusted in the sweep below.
	t.Run("control-probe-map", func(t *testing.T) {
		s := ctxSweepFixture(4500, 3)
		c := newCountingCtx(0)
		if _, err := Balance(c, s, s.profile); err != nil {
			t.Fatalf("control Balance: %v", err)
		}
		if c.calls != len(ctxProbeSites) {
			t.Fatalf("control run probed %d times, ctxProbeSites documents %d - probe layout changed; "+
				"re-derive the site table from balancer.go before trusting the sweep", c.calls, len(ctxProbeSites))
		}
	})

	for _, site := range ctxProbeSites {
		t.Run(site.name, func(t *testing.T) {
			s := ctxSweepFixture(4500, 3)
			c := newCountingCtx(site.call)

			p, err := Balance(c, s, s.profile)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Balance(cancel at probe %d, %s) = %v, want a context.Canceled-anchored error",
					site.call, site.name, err)
			}
			if errors.Is(err, ErrItemTooLarge) || errors.Is(err, ErrInvalidProfile) {
				t.Errorf("cancellation misclassified as a plan sentinel: %v", err)
			}
			if c.calls != site.call {
				t.Errorf("aborted at Err() call %d, want exactly %d (the %s probe) - "+
					"a later call means this site ignored the cancel for a whole batch",
					c.calls, site.call, site.name)
			}
			if !p.ops.empty() {
				t.Errorf("aborted Balance returned write ops (%d whole, %d chunked)",
					len(p.ops.wholeFiles), len(p.ops.chunkFiles))
			}
			if s.began != 0 {
				t.Errorf("aborted Balance opened %d transactions (Plan.Write owns the tx)", s.began)
			}
		})
	}
}

// TestBalanceAbortsMidEmptyDirPass: the abort is observed from the
// MIDDLE of a long loop, not only at its entry - the dirs pass with
// 4100 entries probes at i==0 and i==4096, and a cancel surfacing at
// the second probe aborts 4096 dirs INTO the pass (property 3; the
// identical shape at the placement loop's second probe is the
// placement-i4096 subtest above, which is the loop that matters for
// data placement).
func TestBalanceAbortsMidEmptyDirPass(t *testing.T) {
	const files, dirs = 5, 4100
	// Probe layout for this shape: post-load 1, construction 1 (only
	// i==0 - 5 files), pre-sort 1, placement 1, dirs 2 (i==0 and
	// i==4096) = 6.
	const wantProbes = 6

	controlStore := ctxSweepFixture(files, dirs)
	control := newCountingCtx(0)
	if _, err := Balance(control, controlStore, controlStore.profile); err != nil {
		t.Fatalf("control Balance: %v", err)
	}
	if control.calls != wantProbes {
		t.Fatalf("control probed %d times, want %d for this shape - probe layout changed", control.calls, wantProbes)
	}

	s := ctxSweepFixture(files, dirs)
	c := newCountingCtx(wantProbes) // last probe = dirs loop i == 4096
	p, err := Balance(c, s, s.profile)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Balance(cancel at dirs i==4096) = %v, want context.Canceled-anchored", err)
	}
	if c.calls != wantProbes {
		t.Errorf("aborted at call %d, want %d (the mid-dirs probe)", c.calls, wantProbes)
	}
	if !p.ops.empty() || s.began != 0 {
		t.Errorf("mid-dirs abort persisted work: ops empty=%v transactions=%d", p.ops.empty(), s.began)
	}
}
