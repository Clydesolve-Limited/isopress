package progress

// Gap-closing tests for the percent-after-walk upgrade of the
// INTERACTIVE renderer (DESIGN_SPEC 3.6/9.1/9.2, stakeholder decision
// 2026-10-19), added by the verification pass of 2026-10-19.
// interactive_test.go already pins: the upgrade lands, counts carry
// across, no duplicate bar artifacts (newline count), and name-gating.
// THIS file closes three audit gaps:
//
//  1. Display-neutrality of the pre-walk spinner window: no frame
//     rendered before the upgrade carries a percent or the
//     [elapsed:eta] bracket - OptionSetPredictTime(true) at
//     construction must stay invisible while the bar is indeterminate
//     (progressbar v3.19.1 draws the ETA bracket for determinate bars
//     only; the spinner renders at most a single-value elapsed
//     bracket with no colon, which is what etaBracketRe discriminates
//     on).
//
//  2. The currentNum invariant behind the in-place upgrade: files that
//     complete DURING the spinner window must be reflected in the
//     FIRST determinate state - the correct percent for them, not 0
//     followed by a jump on the next completion. This pins, at the
//     renderer's observable boundary, two library-source facts the
//     implementation rationale (interactive.go, Phase case) rests on
//     (verified against progressbar v3.19.1 source, progressbar.go):
//
//       - state.currentNum is 0-by-construction in the spinner window
//         NOT because the library forbids increments there - the
//         spinner is ignoreLength with config.max = bar width (40),
//         and Add64 would move currentNum modulo the width and also
//         accumulate state.currentBytes for a nonzero delta - but
//         because the renderer's refresh() adds ZERO deltas
//         (Add64(0)) while indeterminate. That zero-delta discipline
//         is LOAD-BEARING: the upgrade rebases via Set64(bytes), and
//         Set64 computes toAdd = bytes - currentBytes, so any
//         pre-upgrade nonzero add would corrupt the first determinate
//         percent. A regression that feeds real deltas to the spinner
//         bar fails this test (the upgrade frame would show a wrong
//         percent until the next completion).
//       - ChangeMax64 flips the SAME bar to determinate; with
//         currentNum at 0 its internal Add(0) re-render is suppressed
//         by the percent-change guard (currentPercent 0 == lastPercent
//         0), so the first determinate frame is the upgrade's own
//         refresh(): the files N/T description plus Set64's correct
//         percent, emitted synchronously within the same event
//         handler - the bar never idles at 0% waiting for the next
//         file.
//
//  3. A totals event with TotalKnown FALSE for an ALREADY-DETERMINATE
//     phase falls through to the documented reset (the in-place
//     upgrade is gated on TotalKnown, DESIGN_SPEC 9.1): no panic, no
//     artifact beyond the ordinary end-of-bar newline, counters
//     restart in counts-only mode.
//
// Test 2 uses a bounded poll across the renderer goroutine (the
// interactive renderer has no synchronous handle core); the poll only
// bridges goroutine scheduling - the renderer processes the totals
// event in microseconds - and every assertion before the poll's
// deadline is on state the implementation has already committed.

import (
	"strings"
	"testing"
	"time"
)

// waitForOutput polls the writer until pred holds or the deadline
// passes, returning whether it held. It exists for the mid-stream
// assertions the async interactive Run loop requires (see the file
// doc): events are committed in order, so observing the upgraded
// frame before emitting the NEXT event is a happens-before pin, not a
// sleep-assert race.
func waitForOutput(w *lockedWriter, pred func(out string) bool, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if pred(w.String()) {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return pred(w.String())
}

// TestInteractiveSpinnerWindowCarriesNoPercentOrETA: while the hash
// phase is indeterminate, every rendered frame is counts-only - no
// percent, no totals-form description, no [elapsed:eta] bracket -
// even though the bar was constructed with predict-time enabled. The
// upgrade frames that follow are the contrast: totals description
// plus percent plus the colon bracket.
//
// Known tiny race window (accepted, documented): progressbar's 100ms
// spinner ticker could, with probability ~1e-4 per run, render a
// determinate frame in the microsecond gap between the upgrade's
// ChangeMax64 and Describe calls. Such a frame is state-correct
// (cumulative counts, 0%); the assertions below tolerate none of them
// because a real regression (predict-time leaking into the spinner
// display, or an early upgrade) renders such frames for the WHOLE
// spinner window, not in a 1e-4 race.
func TestInteractiveSpinnerWindowCarriesNoPercentOrETA(t *testing.T) {
	w := &lockedWriter{}
	bus := NewBus()
	wait := Start(New(Options{Mode: ModeInteractive, Stderr: w}), bus)

	bus.Emit(Phase{Name: "scan(hash)"})
	bus.Emit(ItemDone{Bytes: 100, Path: "a"})
	bus.Emit(Tick{})
	bus.Emit(ItemDone{Bytes: 100, Path: "b"})
	// Walk completion: totals for the STILL-RUNNING phase.
	bus.Emit(Phase{Name: "scan(hash)", TotalKnown: true, TotalFiles: 4, TotalBytes: 400})
	bus.Emit(ItemDone{Bytes: 100, Path: "c"})
	bus.Finish(Done{Summary: "scan(hash): complete"})
	wait()

	out := w.String()
	frames := strings.Split(out, "\r")
	firstDeterminate := -1
	for i, f := range frames {
		if etaBracketRe.MatchString(f) {
			firstDeterminate = i
			break
		}
	}
	if firstDeterminate < 0 {
		t.Fatalf("no [elapsed:eta] frame after the upgrade; nothing to contrast the spinner window against:\n%q", out)
	}

	sawSpinnerDesc := false
	for i, f := range frames[:firstDeterminate] {
		if !strings.Contains(f, "scan(hash):") {
			continue // clear frames and partial redraws carry no description
		}
		sawSpinnerDesc = true
		if strings.Contains(f, "%") {
			t.Errorf("spinner frame %d carries a percent before the upgrade (predict-time must stay display-neutral while indeterminate): %q", i, f)
		}
		if strings.Contains(f, "/4") {
			t.Errorf("spinner frame %d carries the totals-form description before the upgrade: %q", i, f)
		}
	}
	if !sawSpinnerDesc {
		t.Errorf("no counts-only spinner frame observed before the upgrade:\n%q", out)
	}

	// The first colon-bracket frame must be a genuine upgrade frame:
	// totals description and a percent, with the carried counts.
	if !strings.Contains(frames[firstDeterminate], "files 2/4") {
		t.Errorf("first determinate frame lacks the carried counts (files 2/4): %q", frames[firstDeterminate])
	}
	if !strings.Contains(frames[firstDeterminate], "%") {
		t.Errorf("first determinate frame lacks a percent: %q", frames[firstDeterminate])
	}
}

// TestInteractiveUpgradeShowsImmediateCorrectPercent: three files
// complete DURING the spinner window (300 of 1000 bytes), then the
// walk-completion totals arrive. BEFORE any post-upgrade completion is
// emitted, the determinate state must already show the correct
// percent for those three: 300/1000 = 30%. A regression that relies
// on the library's carried currentNum without rebasing (or that skips
// the upgrade's refresh) leaves the bar at 0% until the NEXT
// completion - exactly the "0 then jump" defect this pins - and a
// regression that recreates the bar renders the zeroed determinate
// description ("files 0/10"), asserted absent below.
func TestInteractiveUpgradeShowsImmediateCorrectPercent(t *testing.T) {
	w := &lockedWriter{}
	bus := NewBus()
	wait := Start(New(Options{Mode: ModeInteractive, Stderr: w}), bus)

	bus.Emit(Phase{Name: "scan(hash)"})
	bus.Emit(ItemDone{Bytes: 100, Path: "a"})
	bus.Emit(ItemDone{Bytes: 100, Path: "b"})
	bus.Emit(ItemDone{Bytes: 100, Path: "c"})
	// The scan.go emitScanWalkTotals event shape.
	bus.Emit(Phase{Name: "scan(hash)", TotalKnown: true, TotalFiles: 10, TotalBytes: 1000})

	// Mid-stream pin: the upgraded frame, with the CORRECT percent,
	// must appear before the next completion exists.
	if !waitForOutput(w, func(out string) bool {
		return strings.Contains(out, "files 3/10, 300 B/1.0 KB") && strings.Contains(out, " 30% |")
	}, 10*time.Second) {
		t.Fatalf("no determinate frame showing the correct percent (30%%) for the 3 spinner-window completions at upgrade time; "+
			"the upgrade must rebase on the renderer's carried counters (Set64), not start from zero:\n%q", w.String())
	}

	// Post-upgrade completions carry from the correct base.
	bus.Emit(ItemDone{Bytes: 100, Path: "d"})
	bus.Emit(ItemDone{Bytes: 100, Path: "e"})
	bus.Finish(Done{Summary: "scan(hash): complete (5 files)"})
	wait()

	out := w.String()
	if !strings.Contains(out, "files 5/10, 500 B/1.0 KB") {
		t.Errorf("final description lacks the carried cumulative state (5/10, 500 B):\n%q", out)
	}
	if strings.Contains(out, "files 0/10") {
		t.Errorf("a zeroed determinate description rendered: the upgrade recreated the bar instead of upgrading in place:\n%q", out)
	}
	// In-place: exactly the end-of-bar newline plus the summary.
	if n := strings.Count(out, "\n"); n != 2 {
		t.Errorf("output holds %d newlines, want exactly 2 (end-of-bar + summary):\n%q", n, out)
	}
}

// TestInteractiveTotalsUnknownFallsThroughToReset: a totals event
// with TotalKnown FALSE naming the ALREADY-DETERMINATE running phase
// is not an upgrade (9.1 gates the in-place path on TotalKnown) - it
// falls through to the ordinary phase-transition path: the determinate
// bar ends with its newline, a fresh indeterminate bar starts for the
// same phase name, and the counters reset. No panic, no artifact
// beyond the ordinary end-of-bar newline.
func TestInteractiveTotalsUnknownFallsThroughToReset(t *testing.T) {
	w := &lockedWriter{}
	bus := NewBus()
	wait := Start(New(Options{Mode: ModeInteractive, Stderr: w}), bus)

	bus.Emit(Phase{Name: "copy", TotalKnown: true, TotalFiles: 10, TotalBytes: 1000})
	bus.Emit(ItemDone{Bytes: 100, Path: "a"})
	// TotalKnown false for the running determinate phase: reset.
	bus.Emit(Phase{Name: "copy", TotalKnown: false})
	bus.Emit(ItemDone{Bytes: 100, Path: "b"})
	bus.Finish(Done{Summary: "copy: complete"})
	wait()

	out := w.String()
	if !strings.Contains(out, "copy: files 1/10, 100 B/1.0 KB") {
		t.Errorf("output lacks the initial determinate description:\n%q", out)
	}
	// The post-reset spinner shows the counts-only form with RESET
	// counters (files 1, not files 2).
	if !strings.Contains(out, "copy: files 1, 100 B") {
		t.Errorf("totals-known=false event did not reset the phase to a counts-only spinner:\n%q", out)
	}
	if n := strings.Count(out, "\n"); n != 3 {
		t.Errorf("output holds %d newlines, want exactly 3 (determinate end-of-bar + spinner end-of-bar + summary):\n%q", n, out)
	}
	if !strings.Contains(out, "copy: complete") {
		t.Errorf("final summary line missing:\n%q", out)
	}
}
