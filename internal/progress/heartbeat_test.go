package progress

// Heartbeat (Tick) tests - the 2026-10-20 QA heartbeat fold-in. The
// finding: the build pipeline's await points froze the progress
// display for >5s because ItemDone is per-FILE, so build(verify)'s
// hash walk of one huge member (a 20 GiB image entry) and build(stage)'s
// intra-member copy emit NOTHING between the member's start and its
// completion. DESIGN_SPEC 9.1 anticipated exactly this with the Tick
// "heartbeat" event kind, but no emitter existed; internal/cli's build
// command now runs one run-level heartbeat goroutine
// (cli.HeartbeatInterval, 5s) that fills those silent stretches.
//
// THIS file pins the renderer halves of that contract:
//
//   - the plain renderer's Tick line: the current phase's line with
//     UNCHANGED counts (the honest "still alive" repetition), through
//     the SAME PlainInterval throttle as an ItemDone line, and nothing
//     before any phase is declared;
//   - the interactive renderer's Tick repaint: the current bar
//     re-rendered IN PLACE via bar.Describe (verified against the
//     pinned progressbar v3.19.1 source: Describe invokes render()
//     unconditionally, while Add64/Set64 at an unchanged percent are
//     render-suppressed by Add64's percent-change guard) - exactly one
//     frame per Tick, counts identical, no reset;
//   - the finding-1 closure at the renderer boundary: a simulated
//     long single-file verify (a hash walk that spans >1 tick
//     interval with zero ItemDone between) produces plain heartbeat
//     lines MID-FILE and interactive repaint frames BETWEEN ItemDones.
//
// The emitter half (heartbeat goroutine cadence, stop, leak, nil-bus)
// is pinned in internal/cli/build_heartbeat_test.go; the two compose
// end-to-end there in TestBuildLongSingleFileVerifyHeartbeatClosesTheGap.

import (
	"strings"
	"testing"
)

// buildVerifyMidFileLines are the exact plain lines of the simulated
// long single-file verify below: a 2-member build(verify) phase
// (999 B small member + a 20 GiB huge member) whose hash walk stalls
// on the huge member for three heartbeat intervals. 999 B renders as
// "999 B" (below the 1.0 KB threshold) and the totals as "20.0 GiB";
// the percent is 0 until the huge member lands and 100 after (the
// byte-derived, truncated convention of 9.3).
const (
	verifyMidFileLine = "build(verify): files 1/2, 999 B/20.0 GiB (0%)"
	verifyDoneLine    = "build(verify): files 2/2, 20.0 GiB/20.0 GiB (100%)"
)

// TestPlainTickHeartbeatLineRespectsThrottleAndRepeatsState pins the
// plain Tick contract: within a cadence window a Tick prints nothing
// (heartbeats are ordinary 9.3 cadence citizens - no double lines with
// ItemDone); once PlainInterval has elapsed a Tick prints the current
// phase's line with UNCHANGED counts; the next in-window Tick is
// throttled again; and one heartbeat line per interval is the steady
// state.
func TestPlainTickHeartbeatLineRespectsThrottleAndRepeatsState(t *testing.T) {
	w := &lockedWriter{}
	r, clk := newPlainForTest(w)

	r.handle(Phase{Name: "copy", TotalKnown: true, TotalFiles: 10, TotalBytes: 1000})
	r.handle(ItemDone{Bytes: 100}) // immediate first line
	r.handle(Tick{})               // throttled: 0s since the line - NO heartbeat line
	clk.Advance(PlainInterval)
	r.handle(Tick{}) // prints: the repetition of the last state
	r.handle(Tick{}) // throttled again: same window
	clk.Advance(PlainInterval)
	r.handle(Tick{}) // one heartbeat line per interval in the steady state
	r.writeSummary(Done{Summary: "copy: complete"})

	got := lines(w.String())
	want := []string{
		"copy: files 1/10, 100 B/1.0 KB (10%)",
		"copy: files 1/10, 100 B/1.0 KB (10%)", // heartbeat: unchanged counts
		"copy: files 1/10, 100 B/1.0 KB (10%)", // and again, one per interval
		"copy: complete",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d (a Tick in-window prints nothing; one heartbeat line per interval):\n%q",
			len(got), len(want), w.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want exact %q (heartbeat lines repeat the last state verbatim)", i, got[i], want[i])
		}
	}
}

// TestPlainTickBeforeAnyPhasePrintsNothing: a Tick arriving before any
// Phase - the build run's pre-phase window (catalog export, inventory
// reads) while the run-level heartbeat is already running - has no
// state to repeat and must print nothing, not even an empty-prefixed
// line. The first phase's immediate line covers that window instead.
func TestPlainTickBeforeAnyPhasePrintsNothing(t *testing.T) {
	w := &lockedWriter{}
	r, clk := newPlainForTest(w)

	r.handle(Tick{})
	clk.Advance(PlainInterval)
	r.handle(Tick{}) // even past the throttle: there is no phase line to draw
	r.writeSummary(Done{})

	if out := w.String(); out != "" {
		t.Errorf("a pre-phase Tick printed %q, want zero output (no state to repeat)", out)
	}
}

// TestPlainLongSingleFileVerifyHeartbeatMidFile is the finding-1
// closure at the plain renderer boundary (the emitter half lives in
// internal/cli): a simulated long single-file verify - a 20 GiB member
// whose hash walk spans three heartbeat intervals with ZERO ItemDone
// between - must produce heartbeat lines MID-FILE, each repeating the
// unchanged pre-completion state, and the completion line exactly once
// at the end. Before the fold-in this stretch was silent for the whole
// walk: the cron user saw one line, then nothing for tens of seconds.
func TestPlainLongSingleFileVerifyHeartbeatMidFile(t *testing.T) {
	w := &lockedWriter{}
	r, clk := newPlainForTest(w)

	r.handle(Phase{Name: "build(verify)", TotalKnown: true, TotalFiles: 2, TotalBytes: 20 * gib})
	r.handle(ItemDone{Bytes: 999, Path: "small.txt"}) // immediate line
	// The huge member's hash walk: ItemDone is per-FILE, so the event
	// stream is SILENT here except for the heartbeat (one Tick per
	// elapsed interval, as cli.HeartbeatInterval emits them).
	for i := 0; i < 3; i++ {
		clk.Advance(PlainInterval)
		r.handle(Tick{})
	}
	clk.Advance(PlainInterval)
	r.handle(ItemDone{Bytes: 20*gib - 999, Path: "huge.iso"}) // the walk finishes
	r.writeSummary(Done{Summary: "build: 1 image(s) built (20.0 GiB), 0 failed, warnings 0"})

	got := lines(w.String())
	want := []string{
		verifyMidFileLine, // the small member
		verifyMidFileLine, // heartbeat mid-file, interval 1
		verifyMidFileLine, // heartbeat mid-file, interval 2
		verifyMidFileLine, // heartbeat mid-file, interval 3
		verifyDoneLine,    // the huge member lands
		"build: 1 image(s) built (20.0 GiB), 0 failed, warnings 0",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d (heartbeat lines must appear MID-FILE - the gap is closed):\n%q",
			len(got), len(want), w.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want exact %q", i, got[i], want[i])
		}
	}
}

// TestInteractiveTickRepaintsDeterminateBarInPlace is the interactive
// half of the heartbeat contract, driving the REAL async Run loop: a
// determinate build(verify) bar (the huge-member scenario of the
// finding), one small ItemDone, then three Ticks mid-file, then the
// huge member's completion. Every rendered frame carries the bar
// description verbatim (progressbar v3.19.1 renderProgressBar embeds
// c.description; the \r-clear frames are spaces), so counting the
// description occurrences counts repaint frames:
//
//   - 1 frame from the small ItemDone + exactly 1 frame per Tick = 4
//     frames of "files 1/2, 999 B/20.0 GiB" - the Ticks repainted the
//     bar IN PLACE with unchanged counts (a lost heartbeat renders
//     fewer; a heartbeat that reset or double-rendered renders more);
//   - every one of those frames carries the determinate " 0% |" bar -
//     a full repaint, not an empty clear;
//   - "files 2/2" appears only AFTER the huge member's ItemDone, and
//     never combined with the pre-completion byte counter.
//
// Note on the elapsed-time display (documented, per the QA ask): the
// renderer builds determinate bars with OptionSetPredictTime(false)
// (interactive.go newBar), so determinate frames carry NO elapsed/ETA
// bracket to advance - the honest observable of a heartbeat repaint is
// the repaint frame itself with unchanged counts, which is what this
// test asserts. (Indeterminate spinner bars animate on their own
// 100ms library ticker independently of Tick.)
func TestInteractiveTickRepaintsDeterminateBarInPlace(t *testing.T) {
	w := &lockedWriter{}
	bus := NewBus()
	wait := Start(New(Options{Mode: ModeInteractive, Stderr: w}), bus)

	bus.Emit(Phase{Name: "build(verify)", TotalKnown: true, TotalFiles: 2, TotalBytes: 20 * gib})
	bus.Emit(ItemDone{Bytes: 999, Path: "small.txt"})
	for i := 0; i < 3; i++ {
		bus.Emit(Tick{}) // the heartbeat, mid-huge-member
	}
	bus.Emit(ItemDone{Bytes: 20*gib - 999, Path: "huge.iso"})
	bus.Finish(Done{Summary: "build: 1 image(s) built (20.0 GiB), 0 failed, warnings 0"})
	wait()

	out := w.String()
	const pre = "build(verify): files 1/2, 999 B/20.0 GiB"
	if n := strings.Count(out, pre); n != 4 {
		t.Errorf("found %d frames with the pre-completion description, want exactly 4 "+
			"(1 ItemDone + 1 per Tick: the heartbeat must repaint the determinate bar in place):\n%q", n, out)
	}
	if n := strings.Count(out, " 0% |"); n < 4 {
		t.Errorf("found %d determinate bar renders, want at least 4 (every Tick frame is a FULL repaint, not an empty clear):\n%q", n, out)
	}
	if !strings.Contains(out, "build(verify): files 2/2, 20.0 GiB/20.0 GiB") {
		t.Errorf("the huge member's completion frame is missing:\n%q", out)
	}
	if strings.Contains(out, "files 2/2, 999 B") {
		t.Errorf("a frame mixes the post-completion count with the pre-completion bytes (counts changed by a Tick?):\n%q", out)
	}
	if strings.Contains(out, "files 1/1,") || strings.Contains(out, "files 0/2") {
		t.Errorf("a reset counts frame appeared (Tick must not reset the bar):\n%q", out)
	}
	// The bar is \r-redrawn in place: exactly the end-of-bar newline
	// plus the summary line.
	if n := strings.Count(out, "\n"); n != 2 {
		t.Errorf("output holds %d newlines, want exactly 2 (end-of-bar + summary); duplicate line artifacts:\n%q", n, out)
	}
}

// TestInteractiveTickBeforePhaseRendersNothing: a Tick before any
// Phase has no bar to repaint (bar == nil) and must produce zero
// output - the interactive half of the pre-phase-window contract.
func TestInteractiveTickBeforePhaseRendersNothing(t *testing.T) {
	w := &lockedWriter{}
	bus := NewBus()
	wait := Start(New(Options{Mode: ModeInteractive, Stderr: w}), bus)

	bus.Emit(Tick{})
	bus.Emit(Tick{})
	bus.Finish(Done{Summary: "done"})
	wait()

	out := w.String()
	if strings.Contains(out, "%") || strings.Contains(out, "|") {
		t.Errorf("a pre-phase Tick rendered bar content:\n%q", out)
	}
	if !strings.Contains(out, "done") {
		t.Errorf("summary missing (the renderer must still process the stream):\n%q", out)
	}
}
