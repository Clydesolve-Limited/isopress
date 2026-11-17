package imagebuild

// Adversarial audit tests for the build(master) byte-watcher lifecycle
// and the four-phase cross-phase emission contract (DESIGN_SPEC 6.2
// four-phase progress; the 2026-10-20 "looks hung" amendment). These
// close the audit gaps left by watch_test.go:
//
//   - the ENOENT window: the .partial does not exist yet when the
//     watcher starts (xorriso creates it a moment later). The contract
//     is HOLD SILENTLY - zero frames, in particular NO spurious
//     0-byte BytesWritten frames that would render a misleading
//     "build(master): 0 B/..." line while nothing has been written.
//   - concurrent double-stop (sync.Once + WaitGroup under parallel
//     callers, the interrupt + return-race shape).
//   - the F4 #2 pre-exec ctx check: Build refuses to exec on a
//     cancelled context BEFORE the watcher starts - pinned here with a
//     goroutine-baseline leak check (the "does the watcher even start
//     there?" audit question: it must not).
//   - THE CROSS-PHASE EMISSION CONTRACT (the key audit question): a
//     late master BytesWritten arriving AFTER the build(extract) /
//     build(verify) Phase event would drive the WRONG bar in both
//     renderers (a BytesWritten flips the CURRENT phase's bar to its
//     byte-driven form - see internal/progress's BytesWritten case -
//     so a stray master emission during build(verify) would corrupt
//     the verify bar's display). The renderers are deliberately
//     phase-agnostic; the ordering is the PRODUCER's contract:
//     startMasterWatcher's stop() WAITS (WaitGroup) for the goroutine
//     before Build emits or calls the next phase, and the Bus is a
//     FIFO channel, so every watcher frame is enqueued strictly before
//     the Phase transition. This test pins that contract through a
//     full Build with a SLOW master (the watcher samples repeatedly)
//     and a phase-emitting Extractor: NO build(master) line may appear
//     after the first build(extract) line, and none of either after
//     the first build(verify) line.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"isopress/internal/progress"
	"isopress/internal/verify"
)

// TestMasterWatcherMissingPartialEmitsNoSpuriousFrames: the .partial
// never exists during the whole watch window (the xorriso startup
// lag, or a master that fails before creating it). The production stat
// probe misses on EVERY poll; the watcher must hold silently - ZERO
// events, never a 0-byte frame per poll that would flood the bus and
// render a phantom "0 B" position.
func TestMasterWatcherMissingPartialEmitsNoSpuriousFrames(t *testing.T) {
	var mu sync.Mutex
	var got []progress.Event
	emit := func(ev progress.Event) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	}

	// A path that is never created: the REAL stat probe misses every
	// poll (1ms - 20+ polls within the sleep below).
	missing := t.TempDir() + "/never-created.iso.partial"
	stop := startMasterWatcher(context.Background(), missing, emit,
		MasterWatch{Interval: time.Millisecond})
	time.Sleep(25 * time.Millisecond)
	stop() // waits for the goroutine; the frame count is final after this

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 0 {
		t.Fatalf("missing .partial produced %d frames (first: %+v); "+
			"the ENOENT window must hold SILENTLY - no 0-byte frames, no flooding", len(got), got[0])
	}
}

// TestMasterWatcherConcurrentDoubleStop: stop() called from several
// goroutines at once (the interrupt handler's stop racing Build's
// deferred/return-path stop) must neither panic, deadlock, nor leak -
// sync.Once serializes, the WaitGroup wait completes once.
func TestMasterWatcherConcurrentDoubleStop(t *testing.T) {
	baseline := settleBuildGoroutines(t)
	stop := startMasterWatcher(context.Background(), "p", func(progress.Event) {},
		MasterWatch{Interval: time.Millisecond})

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			stop()
		}()
	}
	wg.Wait()
	waitForBuildGoroutineBaseline(t, baseline, 2*time.Second)
}

// TestBuildCancelBeforeExecStartsNoWatcher: the F4 #2 ctx check (the
// 6.6 transaction committed, THEN the context dies) refuses the exec
// BEFORE the master Phase and watcher exist - pinned with the leak
// baseline so a future refactor that starts the watcher earlier fails
// here.
func TestBuildCancelBeforeExecStartsNoWatcher(t *testing.T) {
	baseline := settleBuildGoroutines(t)

	f := newBuildFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.store.beginHook = cancel // the SIGINT window: dies between step 3 and the exec
	deps := f.deps()
	deps.MasterWatch = MasterWatch{Interval: time.Millisecond}

	if _, err := Build(ctx, deps, f.in); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled (the F4 #2 refusal)", err)
	}
	waitForBuildGoroutineBaseline(t, baseline, 5*time.Second)
}

// phaseEmitterExtractor emulates the verify primitive's emission shape
// at the imagebuild seam: the two self-verify Phase events plus their
// driver events, exactly in the order internal/verify emits them
// (extract Phase + a BytesWritten frame, then the verify Phase +
// per-entry ItemDone).
type phaseEmitterExtractor struct {
	result verify.ExtractResult
}

func (x *phaseEmitterExtractor) ExtractAndHash(_ context.Context, _, _ string, bus *progress.Bus, totals verify.Totals) (verify.ExtractResult, error) {
	if bus != nil {
		bus.Emit(progress.Phase{Name: verify.PhaseExtract, TotalKnown: true, TotalFiles: totals.Entries, TotalBytes: totals.Bytes})
		bus.Emit(progress.BytesWritten{Bytes: totals.Bytes})
		bus.Emit(progress.Phase{Name: verify.PhaseVerify, TotalKnown: true, TotalFiles: totals.Files, TotalBytes: totals.Bytes})
		bus.Emit(progress.ItemDone{Bytes: totals.Bytes})
	}
	return x.result, nil
}

// TestBuildNoMasterFrameAfterLaterPhases pins the CROSS-PHASE
// emission contract end to end (see the file doc): with a master slow
// enough for the 1ms watcher to sample dozens of times, EVERY
// build(master) line must precede EVERY build(extract) line, and every
// build(extract) line must precede every build(verify) line - the
// anti-interleaving half beyond the existing first-occurrence order
// pins. A single late BytesWritten (rendered as a build(master)
// bytes-only line after the extract/verify lines) would mean stop()
// failed to fence the watcher, and the renderers' phase-agnostic
// BytesWritten handling would let it corrupt the wrong phase's bar.
func TestBuildNoMasterFrameAfterLaterPhases(t *testing.T) {
	f := newBuildFixture(t)
	f.extract = nil // replaced below by the phase-emitting extractor
	f.master.run = func(ctx context.Context, args []string) ([]byte, error) {
		out, err := defaultMasterRun(args)
		if err != nil {
			return nil, err
		}
		// Long enough for ~30 watcher samples at 1ms.
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
	deps.Extractor = &phaseEmitterExtractor{result: f.happyEntries()}

	if _, err := Build(context.Background(), deps, f.in); err != nil {
		t.Fatalf("Build: %v", err)
	}
	bus.Finish(progress.Done{Summary: "build: cross-phase fence"})
	wait()

	out := w.String()
	masterLast := strings.LastIndex(out, "build(master): ")
	extractFirst := strings.Index(out, "build(extract): ")
	extractLast := strings.LastIndex(out, "build(extract): ")
	verifyFirst := strings.Index(out, "build(verify): ")
	if masterLast < 0 || extractFirst < 0 || verifyFirst < 0 {
		t.Fatalf("missing phase lines in the captured stream:\n%s", out)
	}
	if masterLast > extractFirst {
		t.Fatalf("CROSS-PHASE LEAK: a build(master) frame at byte %d landed after the first build(extract) line at %d "+
			"(the master watcher outlived stop(); its BytesWritten would drive the extract/verify bar):\n%s",
			masterLast, extractFirst, out)
	}
	if extractLast > verifyFirst {
		t.Fatalf("CROSS-PHASE LEAK: a build(extract) frame at byte %d landed after the first build(verify) line at %d "+
			"(the extract watcher outlived stop() in this emulation):\n%s",
			extractLast, verifyFirst, out)
	}
}
