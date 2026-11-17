package imagebuild

// The build(master) byte-watcher tests (DESIGN_SPEC 6.2 four-phase
// progress; the 2026-10-20 "looks hung" amendment). Three layers:
//
//  1. The watcher itself, driven directly through its injectable
//     seams (fake stat source, millisecond interval, recording emit):
//     absolute positions, monotonic honesty, lifecycle (stop waits,
//     ctx cancellation, idempotent stop, nil-bus no-op), and the
//     settle-poll goroutine leak checks (the T1.1 pattern, stdlib-only
//     runtime.NumGoroutine with a settle-then-poll loop - the
//     internal/progress and internal/ingest precedent, renamed local
//     copies).
//  2. The FAKE-TIER Build stream: the build(stage) and build(master)
//     phases render on a Bus in order, and the master line's total is
//     the RequiredImageBytes ESTIMATE over the content list.
//  3. The gated REAL-xorriso four-phase e2e lives in
//     build_real_test.go (TestBuildRealXorrisoFourPhaseProgress).

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"isopress/internal/progress"
	"isopress/internal/units"
)

// --- test harness helpers -----------------------------------------------------

// syncBuffer is the mutex-guarded writer the renderer goroutine writes
// while the test goroutine reads (the progress package's lockedWriter
// shape; test-local because progress's copy is unexported).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// stepClock is the fake-clock harness for the stream assertions: every
// Now() read advances by PlainInterval, so EVERY progress event lands
// in its own cadence window and the captured stream shows every phase
// (the renderer's cadence itself is pinned in internal/progress; here
// it is stepped so the phase order becomes observable). Mutex-guarded:
// the renderer goroutine reads while the watcher goroutine emits.
type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(progress.PlainInterval)
	return c.t
}

// settleBuildGoroutines waits for the goroutine count to stabilize
// (three consecutive equal samples) and returns it - the T1.1
// settle-then-poll leak-check pattern (internal/progress hardening_test.go,
// renamed local copy).
func settleBuildGoroutines(t *testing.T) int {
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

// waitForBuildGoroutineBaseline polls until the goroutine count drops
// to baseline or fails loudly with the current count.
func waitForBuildGoroutineBaseline(t *testing.T, baseline int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		n := runtime.NumGoroutine()
		if n <= baseline {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: %d goroutines after %v, baseline %d", n, within, baseline)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// waitEvents polls until pred holds or the deadline passes.
func waitEvents(pred func() bool, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return pred()
}

// --- the watcher's emission contract ------------------------------------------

// TestMasterWatcherEmitsAbsolutePositions pins the watcher's core
// (6.2): every sample emits BytesWritten carrying the ABSOLUTE stat
// position - never an increment - in monotonically non-decreasing
// order, and a stat ERROR (the .partial not yet created) holds the
// last position instead of emitting zero or failing.
func TestMasterWatcherEmitsAbsolutePositions(t *testing.T) {
	var mu sync.Mutex
	var got []int64
	emit := func(ev progress.Event) {
		if bw, ok := ev.(progress.BytesWritten); ok {
			mu.Lock()
			got = append(got, bw.Bytes)
			mu.Unlock()
		}
	}
	// The fake stat source: a growing .partial, then a miss (ENOENT),
	// then the size again - the real xorriso shape (create -> write ->
	// rename away).
	var step atomicSteps
	stat := func(string) (int64, error) {
		s := step.next()
		if s < 0 {
			return 0, errors.New("injected: no such file")
		}
		return s, nil
	}

	stop := startMasterWatcher(context.Background(), "/fake/partial.iso", emit,
		MasterWatch{Interval: time.Millisecond, Stat: stat})
	if !waitEvents(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) >= 4
	}, 10*time.Second) {
		stop()
		mu.Lock()
		t.Fatalf("watcher emitted %d positions, want the stat sequence: %v", len(got), got)
	}
	stop()

	mu.Lock()
	defer mu.Unlock()
	// Absolute, in order, with the miss (error) holding: 1000, 5000,
	// (miss), 9000, 9000, ...
	if len(got) < 3 || got[0] != 1000 || got[1] != 5000 {
		t.Errorf("positions = %v, want the absolute stat values starting 1000, 5000", got)
	}
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Errorf("position %d = %d after %d: stat sizes may wobble but the test source never shrinks", i, got[i], got[i-1])
		}
	}
}

// atomicSteps is a tiny monotonically advancing stat-step sequencer.
type atomicSteps struct {
	mu sync.Mutex
	i  int
}

// sequence per next() call: 1000, 5000, -1 (miss), 9000, 9000, ...
func (s *atomicSteps) next() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.i++
	switch s.i {
	case 1:
		return 1000
	case 2:
		return 5000
	case 3:
		return -1 // the injected miss
	default:
		return 9000
	}
}

// TestMasterWatcherDrivenByGrowingFile pins the ".partial watcher"
// half of the architect's shape at the filesystem boundary: a fake
// xorriso grows the -o file WHILE the watcher runs, and the emitted
// positions track the real stat size of the growing file.
func TestMasterWatcherDrivenByGrowingFile(t *testing.T) {
	dir := t.TempDir()
	partial := filepath.Join(dir, "S-TEST-001.iso.partial")

	var mu sync.Mutex
	var got []int64
	emit := func(ev progress.Event) {
		if bw, ok := ev.(progress.BytesWritten); ok {
			mu.Lock()
			got = append(got, bw.Bytes)
			mu.Unlock()
		}
	}
	stop := startMasterWatcher(context.Background(), partial, emit,
		MasterWatch{Interval: time.Millisecond})

	// Grow the file in APPENDING steps (no truncate window: the
	// watcher stats SIZE, which the page cache reports immediately;
	// append keeps the size monotonic so the no-wobble assertion is
	// about the watcher, not about WriteFile's truncate-then-write
	// race).
	final := int64(64 << 10)
	f, err := os.OpenFile(partial, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("creating the .partial: %v", err)
	}
	chunk := bytes.Repeat([]byte{'x'}, 8<<10)
	for written := int64(0); written < final; written += int64(len(chunk)) {
		if _, err := f.Write(chunk); err != nil {
			t.Fatalf("growing the .partial: %v", err)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("closing the .partial: %v", err)
	}
	if !waitEvents(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) > 0 && got[len(got)-1] == final
	}, 10*time.Second) {
		mu.Lock()
		t.Fatalf("the watcher never observed the final size %d; positions: %v", final, got)
	}
	stop()
	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Errorf("positions wobble downward: %v", got)
		}
	}
}

// --- lifecycle (the T1.1 settle-poll leak pattern) ----------------------------

// TestMasterWatcherLifecycleNoLeak pins the goroutine contract: stop
// WAITS for the watcher goroutine (settle-poll back to baseline), ctx
// cancellation ends it without an explicit stop, stop is idempotent,
// and a nil emit starts nothing at all.
func TestMasterWatcherLifecycleNoLeak(t *testing.T) {
	emit := func(progress.Event) {}

	t.Run("stop retracts the goroutine", func(t *testing.T) {
		baseline := settleBuildGoroutines(t)
		stop := startMasterWatcher(context.Background(), "p", emit, MasterWatch{Interval: time.Millisecond})
		stop() // must block until the goroutine exited
		waitForBuildGoroutineBaseline(t, baseline, 2*time.Second)
	})
	t.Run("ctx cancellation ends the goroutine", func(t *testing.T) {
		baseline := settleBuildGoroutines(t)
		ctx, cancel := context.WithCancel(context.Background())
		stop := startMasterWatcher(ctx, "p", emit, MasterWatch{Interval: time.Millisecond})
		cancel()
		stop() // the WaitGroup wait returns promptly once the ctx select fires
		waitForBuildGoroutineBaseline(t, baseline, 2*time.Second)
	})
	t.Run("stop is idempotent", func(t *testing.T) {
		stop := startMasterWatcher(context.Background(), "p", emit, MasterWatch{Interval: time.Millisecond})
		stop()
		stop() // second call must not panic or block
	})
	t.Run("nil emit starts nothing", func(t *testing.T) {
		baseline := settleBuildGoroutines(t)
		stop := startMasterWatcher(context.Background(), "p", nil, MasterWatch{Interval: time.Millisecond})
		stop()
		waitForBuildGoroutineBaseline(t, baseline, time.Second)
	})
	t.Run("already-cancelled ctx emits nothing", func(t *testing.T) {
		var mu sync.Mutex
		var count int
		counting := func(progress.Event) { mu.Lock(); count++; mu.Unlock() }
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		stop := startMasterWatcher(ctx, "p", counting, MasterWatch{Interval: time.Millisecond})
		stop()
		mu.Lock()
		defer mu.Unlock()
		// The loop's first iteration may sample before it observes the
		// cancel; a bounded count is tolerated, an unbounded one is not.
		if count > 2 {
			t.Errorf("cancelled watcher emitted %d events, want at most the first sample", count)
		}
	})
}

// TestBuildCancelDuringMasterLeavesNoWatcherLeak is the BUILD-LEVEL
// lifecycle pin: the interrupt path (SIGINT shape - ctx killed while
// xorriso "runs") must leave no watcher goroutine behind once Build
// returns. The fake master parks on ctx.Done exactly like T4.1's
// process kill; the watcher runs at 1ms so several samples fire before
// the cancel.
func TestBuildCancelDuringMasterLeavesNoWatcherLeak(t *testing.T) {
	baseline := settleBuildGoroutines(t)

	f := newBuildFixture(t)
	started := make(chan struct{})
	f.master.run = func(ctx context.Context, args []string) ([]byte, error) {
		if _, err := defaultMasterRun(args); err != nil {
			return nil, err
		}
		close(started)
		<-ctx.Done()
		return []byte("killed"), ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()
	deps := f.deps()
	deps.MasterWatch = MasterWatch{Interval: time.Millisecond}

	if _, err := Build(ctx, deps, f.in); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	waitForBuildGoroutineBaseline(t, baseline, 5*time.Second)
}

// --- the fake-tier Build stream (stage + master phases) -----------------------

// TestBuildEmitsStageAndMasterPhases pins the SERVICE-OWNED phase
// sequence on the fake tier (the cli used to emit one "build" phase;
// the four named phases moved into the services, 6.2): a Bus consumed
// by the plain renderer shows the build(stage) counts+bytes line first
// and the build(master) bytes-only line after it, and the master
// line's TOTAL is the RequiredImageBytes ESTIMATE over the step-3
// content list (the conservative upper bound). build(extract) and
// build(verify) are emitted by internal/verify's primitive - which the
// fake Extractor replaces here - and are pinned in internal/verify's
// tests plus the REAL e2e (build_real_test.go).
func TestBuildEmitsStageAndMasterPhases(t *testing.T) {
	f := newBuildFixture(t)
	f.extract.result = f.happyEntries()
	// The master "runs" slowly enough for the 1ms watcher to sample
	// the .partial it writes (real xorriso takes minutes; the fake
	// takes 30ms - the emission contract itself is pinned above).
	f.master.run = func(ctx context.Context, args []string) ([]byte, error) {
		out, err := defaultMasterRun(args)
		if err != nil {
			return nil, err
		}
		time.Sleep(30 * time.Millisecond)
		return out, nil
	}

	w := &syncBuffer{}
	clk := &stepClock{}
	bus := progress.NewBus()
	wait := progress.Start(progress.New(progress.Options{Mode: progress.ModePlain, Stderr: w, Now: clk.Now}), bus)
	deps := f.deps()
	deps.Bus = bus
	deps.MasterWatch = MasterWatch{Interval: time.Millisecond}

	if _, err := Build(context.Background(), deps, f.in); err != nil {
		t.Fatalf("Build: %v", err)
	}
	bus.Finish(progress.Done{Summary: "build:1 image(s) built"})
	wait()

	out := w.String()
	stageIdx := strings.Index(out, "build(stage): files ")
	masterIdx := strings.Index(out, "build(master): ")
	if stageIdx < 0 || masterIdx < 0 || stageIdx > masterIdx {
		t.Fatalf("phase lines out of order (stage at %d, master at %d):\n%s", stageIdx, masterIdx, out)
	}

	// The stage line carries the inventory totals (11-byte file +
	// 5+6-byte chunks + 5-byte symlink target + one zero-byte dir).
	if !strings.Contains(out[stageIdx:masterIdx], "files 1/5, 11 B/") {
		t.Errorf("build(stage) line lacks the first ItemDone against the inventory totals:\n%s", out)
	}

	// The master line is the pinned BYTES-ONLY form, and its total is
	// the canonical 4.3 estimator over the step-3 content list:
	// payload = file + chunks + manifest + export BYTES (symlink rows
	// add no file bytes), entries = 7 rows - the 2 image-root rows.
	payloadBytes := int64(11 + 5 + 6 + len(f.manifest) + len("fake catalog export bytes"))
	wantTotal := units.FormatBytes(units.RequiredImageBytes(payloadBytes, 5))
	masterLine := out[masterIdx:]
	if i := strings.IndexByte(masterLine, '\n'); i >= 0 {
		masterLine = masterLine[:i]
	}
	if !strings.Contains(masterLine, "/"+wantTotal+" (") {
		t.Errorf("master line %q lacks the RequiredImageBytes estimate total %q", masterLine, wantTotal)
	}
	if strings.Contains(masterLine, "files ") {
		t.Errorf("master line carries a file count; byte-driven phases are bytes-only (9.1): %q", masterLine)
	}
	if !strings.Contains(out, "build:1 image(s) built") {
		t.Errorf("summary line missing:\n%s", out)
	}
}

// TestSelfVerifyTotalsDerivation pins the exact-totals derivation the
// builder hands the verify primitive (verify.Totals are first-class
// parameters, T5.1) against the RECORDED step-3 rows, and the wiring
// itself: the SAME totals reach the Extractor seam. Entries counts
// every content row; Files/Bytes count only the regular-file rows
// (symlink rows carry the 2.3 target-length Size, which the extraction
// materializes as a symlink - NOT as bytes - so counting it would push
// the extract bar past what any tree-sum can reach).
func TestSelfVerifyTotalsDerivation(t *testing.T) {
	f := newBuildFixture(t)
	f.extract.result = f.happyEntries()
	if _, err := Build(context.Background(), f.deps(), f.in); err != nil {
		t.Fatalf("Build: %v", err)
	}

	tot := verifyTotalsOfRows(f.store.begins[0])
	// The fixture's shape: 7 rows, 5 regular files (a.txt, two
	// chunks, manifest, export), 11+5+6+manifest+export bytes - the
	// symlink's 5-byte target size must NOT be in Bytes.
	if tot.Entries != 7 || tot.Files != 5 {
		t.Errorf("totals = %+v, want 7 entries and 5 regular files", tot)
	}
	if want := int64(11 + 5 + 6 + len(f.manifest) + len("fake catalog export bytes")); tot.Bytes != want {
		t.Errorf("total bytes = %d, want %d (symlink target bytes excluded)", tot.Bytes, want)
	}

	// THE WIRING: the builder passed exactly these totals (and its
	// Bus - nil in this fixture's Deps) to the extractor seam.
	f.extract.mu.Lock()
	gotBus, gotTot := f.extract.lastBus, f.extract.lastTotals
	f.extract.mu.Unlock()
	if gotTot != tot {
		t.Errorf("extractor received totals %+v, want the derived %+v", gotTot, tot)
	}
	if gotBus != nil {
		t.Errorf("extractor received bus %v, want nil (fixture Deps carry no Bus)", gotBus)
	}
}
