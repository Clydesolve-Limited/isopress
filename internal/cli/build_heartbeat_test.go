package cli

// Heartbeat tests - the 2026-10-20 QA heartbeat fold-in, the emitter
// half. The finding: the build pipeline's await points - build(verify)'s
// hash walk of one huge member (a 20 GiB image entry; ItemDone is
// per-FILE, so the walk emits NOTHING between the member's start and
// its completion) and build(stage)'s intra-member copy - froze the
// progress display for >5s. DESIGN_SPEC 9.1 anticipated this with the
// Tick "heartbeat" event kind; runBuild now owns ONE run-level
// heartbeat goroutine (startHeartbeat, HeartbeatInterval) that fills
// those silent stretches for every phase.
//
// THIS file pins the emitter contract:
//
//   - cadence: exactly one Tick per elapsed interval, NONE before the
//     first interval (time.Ticker fires its first value only after one
//     full interval - no gratuitous pre-phase tick) - driven through
//     the injected tick channel (the fake-clock seam: the test owns
//     tick delivery, so no assertion depends on wall-clock time);
//   - no heartbeat after Done/stop: stop() ends emission AND waits for
//     the goroutine exit, so nothing is consumed after it returns;
//   - prompt stop on ctx cancellation (the interruption path);
//   - nil-bus safety: no goroutine, a callable no-op stop;
//   - no leaks across the run lifecycle (the settle-poll pattern);
//   - the finding-1 closure END-TO-END: the heartbeat goroutine
//     composing with the real renderers - plain output carries
//     heartbeat lines MID-FILE through the fake-clock throttle, the
//     interactive stream carries repaint frames between ItemDones.
//
// The renderer halves live in internal/progress/heartbeat_test.go.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"isopress/internal/progress"
)

// tickRecorder is the heartbeat's event sink for tests: it counts
// progress.Tick events (the heartbeat emits nothing else; any other
// event would be a contract break and is counted separately so the
// cadence assertions fail loudly rather than silently absorbing it).
type tickRecorder struct {
	mu     sync.Mutex
	ticks  int
	others int
}

func (r *tickRecorder) emit(ev progress.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := ev.(progress.Tick); ok {
		r.ticks++
		return
	}
	r.others++
}

func (r *tickRecorder) tickCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ticks
}

// waitForTickCount polls (bounded) until the recorder has observed n
// ticks - a bridge for goroutine scheduling between the test's tick
// delivery and the heartbeat goroutine's emit, not a sleep-assert.
func waitForTickCount(t *testing.T, rec *tickRecorder, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if rec.tickCount() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("heartbeat delivered %d ticks, want >= %d within 5s", rec.tickCount(), n)
}

// TestBuildHeartbeatCadenceOneTickPerInterval pins the cadence through
// the fake-clock seam: no Tick before the first tick value is
// DELIVERED (production: the first HeartbeatInterval has not elapsed;
// the injected channel pins the same shape deterministically), then
// exactly one Tick per delivered interval, and never an event kind
// other than progress.Tick.
func TestBuildHeartbeatCadenceOneTickPerInterval(t *testing.T) {
	rec := &tickRecorder{}
	ticks := make(chan time.Time)
	stop := startHeartbeatOn(context.Background(), ticks, rec.emit)
	defer stop()

	// NO tick before the first interval: with nothing delivered, the
	// heartbeat must be silent.
	if n := rec.tickCount(); n != 0 {
		t.Fatalf("heartbeat emitted %d ticks before the first interval; want 0", n)
	}

	const intervals = 3
	for i := 1; i <= intervals; i++ {
		ticks <- time.Time{} // one interval elapses (the test IS the fake clock)
		waitForTickCount(t, rec, i)
	}
	if n := rec.tickCount(); n != intervals {
		t.Errorf("emitted %d ticks after %d intervals, want exactly %d (one per interval)", n, intervals, intervals)
	}
	if rec.others != 0 {
		t.Errorf("heartbeat emitted %d non-Tick events; it emits only progress.Tick", rec.others)
	}
}

// TestBuildHeartbeatStopEndsEmissionAndIsIdempotent: stop() ends
// emission, WAITS for the goroutine exit (no receiver remains on the
// tick channel after it returns - the no-leak contract), and is
// idempotent (the once guard; runBuild defers it on every path).
func TestBuildHeartbeatStopEndsEmissionAndIsIdempotent(t *testing.T) {
	rec := &tickRecorder{}
	ticks := make(chan time.Time)
	stop := startHeartbeatOn(context.Background(), ticks, rec.emit)

	ticks <- time.Time{}
	waitForTickCount(t, rec, 1)

	stop()
	stop() // idempotent: a second call must not block or panic

	// The goroutine is gone: no consumer remains on the tick channel.
	select {
	case ticks <- time.Time{}:
		t.Fatal("heartbeat goroutine still consumes ticks after stop returned (stop must wait for the exit)")
	default:
	}
	if n := rec.tickCount(); n != 1 {
		t.Errorf("emitted %d ticks, want exactly 1 (none after stop)", n)
	}
}

// TestBuildHeartbeatStopsPromptlyOnContextCancellation: the
// interruption path - cancelling the run's ctx stops the heartbeat
// without waiting for another interval, and stop() (deferred in
// runBuild) returns promptly once the goroutine has retracted.
func TestBuildHeartbeatStopsPromptlyOnContextCancellation(t *testing.T) {
	rec := &tickRecorder{}
	ticks := make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	stop := startHeartbeatOn(ctx, ticks, rec.emit)

	cancel() // the run is interrupted

	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("heartbeat did not stop promptly on context cancellation")
	}
	select {
	case ticks <- time.Time{}:
		t.Fatal("heartbeat goroutine still consumes ticks after cancellation")
	default:
	}
}

// TestBuildHeartbeatNilBusAndNilEmitAreSafe: nil-bus safety - no
// goroutine is started and the returned stop is callable; the nil emit
// seam (the Deps nil-safety convention) behaves identically.
func TestBuildHeartbeatNilBusAndNilEmitAreSafe(t *testing.T) {
	baseline := settleEscapeGoroutines(t)

	stopBus := startHeartbeat(context.Background(), nil)
	if stopBus == nil {
		t.Fatal("startHeartbeat(nil bus) returned a nil stop; callers defer stop()")
	}
	stopBus() // a callable no-op, not a panic

	ticks := make(chan time.Time)
	stopEmit := startHeartbeatOn(context.Background(), ticks, nil)
	stopEmit()

	waitEscapeGoroutinesBaseline(t, baseline) // nothing spawned by either
}

// TestBuildHeartbeatNoLeakOnStop: the PRODUCTION wiring (startHeartbeat
// - real ticker at HeartbeatInterval, real bus) leaves no goroutine
// behind after stop (the settle-poll pattern; the goroutine sits in
// its select between intervals, so an early stop must still retract
// it). Measured in a TOP-LEVEL test, not a subtest: a t.Run subtest
// runs on its own tRunner goroutine, which would permanently inflate
// the count against a parent-measured baseline.
func TestBuildHeartbeatNoLeakOnStop(t *testing.T) {
	baseline := settleEscapeGoroutines(t)

	bus := progress.NewBus()
	stop := startHeartbeat(context.Background(), bus)
	stop()
	waitEscapeGoroutinesBaseline(t, baseline)
}

// TestBuildHeartbeatNoLeakOnContextCancel: the interruption path -
// cancelling the run's ctx retracts the production heartbeat goroutine
// (stop, as deferred in runBuild, then observes a clean baseline).
func TestBuildHeartbeatNoLeakOnContextCancel(t *testing.T) {
	baseline := settleEscapeGoroutines(t)

	ctx, cancel := context.WithCancel(context.Background())
	bus := progress.NewBus()
	stop := startHeartbeat(ctx, bus)
	cancel()
	stop() // the deferred call in runBuild, post-cancellation
	waitEscapeGoroutinesBaseline(t, baseline)
}

// --- the finding-1 closure, end to end -----------------------------------

// manualClock is the cli-side fake clock (the DESIGN_SPEC 12.1 seam,
// the internal/progress fakeClock shape): mutex-guarded because the
// renderer goroutine reads Now() while the test goroutine advances it
// (-race would flag an unsynchronized pair).
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func newManualClock() *manualClock {
	return &manualClock{now: time.Unix(1_700_000_000, 0)}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// lineProbe is a mutex-guarded writer with observation helpers: the
// renderer goroutine writes while the test goroutine polls (and the
// interactive spinner ticker can write from its own goroutine), so the
// lock is required under -race.
type lineProbe struct {
	mu  sync.Mutex
	buf strings.Builder
}

func newLineProbe() *lineProbe { return &lineProbe{} }

func (p *lineProbe) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buf.Write(b)
}

func (p *lineProbe) String() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.buf.String()
}

// LineCount counts newline-terminated lines rendered so far (bar
// frames are \r-redraws; only lines and the summary add newlines).
func (p *lineProbe) LineCount() int {
	return strings.Count(p.String(), "\n")
}

// DescCount counts occurrences of a bar description; every rendered
// progressbar frame carries it verbatim, so this counts repaint frames
// (the \r-clear frames between them are spaces).
func (p *lineProbe) DescCount(desc string) int {
	return strings.Count(p.String(), desc)
}

// waitForLineCount polls the writer until it holds at least n
// newline-terminated lines (a scheduling bridge between the heartbeat
// goroutine's emit and the renderer goroutine's write - the events are
// committed in order, so reaching n is a happens-before pin, not a
// sleep-assert).
func waitForLineCount(w *lineProbe, n int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if w.LineCount() >= n {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return w.LineCount() >= n
}

// waitForDescCount polls the writer until it holds at least n
// occurrences of desc (the interactive frame-count scheduling bridge).
func waitForDescCount(w *lineProbe, desc string, n int) bool {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if w.DescCount(desc) >= n {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return w.DescCount(desc) >= n
}

// nonEmptyLines splits rendered output into non-empty lines (the
// internal/progress lines helper, duplicated here for the cli package
// boundary).
func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// TestBuildLongSingleFileVerifyHeartbeatClosesTheGap is the finding-1
// closure: the REAL heartbeat goroutine (startHeartbeatOn, one per
// run) composing with the REAL renderers over a real Bus, simulating
// build(verify)'s hash walk of one huge member - a 2-member phase
// where the second member's walk spans three heartbeat intervals with
// ZERO ItemDone between (ItemDone is per-FILE; verify.hashTree emits
// nothing mid-member).
//
//   - Plain surface: the output must carry the phase line MID-FILE -
//     one heartbeat line per elapsed interval, each repeating the
//     unchanged pre-completion state through the PlainInterval
//     throttle - then the completion line and the summary. Before the
//     fold-in this stream was silent for the whole walk.
//   - Interactive surface: the bar must carry repaint frames between
//     the ItemDones (one frame per Tick, counts unchanged), then the
//     completion frame.
//
// Determinism: the test advances the plain renderer's fake clock and
// DELIVERS each heartbeat tick itself (the injected channel is the
// clock seam), waiting for each expected line/frame before the next
// step - so the assertions are exact counts, not timing windows.
func TestBuildLongSingleFileVerifyHeartbeatClosesTheGap(t *testing.T) {
	const gib = int64(1) << 30
	hugeBytes := 20*gib - 999
	preLine := "build(verify): files 1/2, 999 B/20.0 GiB (0%)"
	doneLine := "build(verify): files 2/2, 20.0 GiB/20.0 GiB (100%)"
	summary := "build: 1 image(s) built (20.0 GiB), 0 failed, warnings 0"

	t.Run("plain heartbeat lines mid-file", func(t *testing.T) {
		w := newLineProbe()
		clk := newManualClock()
		bus := progress.NewBus()
		wait := progress.Start(progress.New(progress.Options{
			Mode: progress.ModePlain, Stderr: w, Now: clk.Now,
		}), bus)
		ticks := make(chan time.Time)
		stopHeartbeat := startHeartbeatOn(context.Background(), ticks, bus.Emit)

		bus.Emit(progress.Phase{Name: "build(verify)", TotalKnown: true, TotalFiles: 2, TotalBytes: 20 * gib})
		bus.Emit(progress.ItemDone{Bytes: 999, Path: "small.txt"})
		if !waitForLineCount(w, 1) {
			t.Fatalf("no first progress line; nothing to close the gap against:\n%s", w.String())
		}
		if n := w.LineCount(); n != 1 {
			t.Fatalf("plain output holds %d lines before any tick was delivered, want 1 (no line before the first tick):\n%s", n, w.String())
		}

		// The huge member's hash walk: three heartbeat intervals with
		// zero ItemDone. Each delivered tick must surface as a
		// MID-FILE heartbeat line with unchanged counts.
		for i := 1; i <= 3; i++ {
			clk.Advance(progress.PlainInterval)
			ticks <- time.Time{}
			if !waitForLineCount(w, 1+i) {
				t.Fatalf("no heartbeat line for interval %d; the mid-file gap is NOT closed:\n%s", i, w.String())
			}
		}

		// The huge member finishes; the completion line prints (the
		// clock has advanced a full interval past the last line).
		clk.Advance(progress.PlainInterval)
		bus.Emit(progress.ItemDone{Bytes: hugeBytes, Path: "huge.iso"})
		if !waitForLineCount(w, 5) {
			t.Fatalf("no completion line after the huge member's ItemDone:\n%s", w.String())
		}

		stopHeartbeat()
		bus.Finish(progress.Done{Summary: summary})
		wait()

		want := []string{preLine, preLine, preLine, preLine, doneLine, summary}
		got := nonEmptyLines(w.String())
		if len(got) != len(want) {
			t.Fatalf("got %d lines, want %d (3 mid-file heartbeat lines):\n%s", len(got), len(want), w.String())
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("line %d = %q, want exact %q", i, got[i], want[i])
			}
		}
	})

	t.Run("interactive repaint frames between ItemDones", func(t *testing.T) {
		w := newLineProbe()
		bus := progress.NewBus()
		wait := progress.Start(progress.New(progress.Options{
			Mode: progress.ModeInteractive, Stderr: w,
		}), bus)
		ticks := make(chan time.Time)
		stopHeartbeat := startHeartbeatOn(context.Background(), ticks, bus.Emit)

		pre := "build(verify): files 1/2, 999 B/20.0 GiB"
		bus.Emit(progress.Phase{Name: "build(verify)", TotalKnown: true, TotalFiles: 2, TotalBytes: 20 * gib})
		bus.Emit(progress.ItemDone{Bytes: 999, Path: "small.txt"})
		waitForDescCount(w, pre, 1) // the small member's frame

		// Three heartbeat intervals mid-huge-member: one repaint frame
		// each, counts unchanged.
		for i := 1; i <= 3; i++ {
			ticks <- time.Time{}
			if !waitForDescCount(w, pre, 1+i) {
				t.Fatalf("no repaint frame for interval %d; the interactive mid-file gap is NOT closed:\n%s", i, w.String())
			}
		}

		bus.Emit(progress.ItemDone{Bytes: hugeBytes, Path: "huge.iso"})
		if !waitForDescCount(w, "build(verify): files 2/2, 20.0 GiB/20.0 GiB", 1) {
			t.Fatalf("no completion frame after the huge member's ItemDone:\n%s", w.String())
		}

		stopHeartbeat()
		bus.Finish(progress.Done{Summary: summary})
		wait()

		out := w.String()
		if n := strings.Count(out, pre); n != 4 {
			t.Errorf("found %d frames with the pre-completion description, want exactly 4 (1 ItemDone + 3 Tick repaints):\n%s", n, out)
		}
		if strings.Contains(out, "files 2/2, 999") {
			t.Errorf("a frame mixes post-completion counts with pre-completion bytes:\n%s", out)
		}
	})
}
