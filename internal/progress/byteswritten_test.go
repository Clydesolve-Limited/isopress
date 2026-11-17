package progress

// The four-phase build progress amendment (DESIGN_SPEC 6.2/9.1/9.3,
// stakeholder 2026-10-20 "looks hung"): the BytesWritten event kind and
// its renderer contract. plain_test.go pins the pre-amendment frozen
// formats; THIS file pins the AMENDMENT's additions:
//
//  1. The two NEW pinned byte-line formats of 9.3, byte-exact against
//     the fake clock (12.1): "build(master): 12.3 GiB/21.0 GiB (58%)"
//     and "build(extract): 8.1 GiB/21.0 GiB (38%)" - percent from
//     bytes, truncated per the established convention.
//  2. SATURATE-AND-HOLD (6.2's ruling): a byte position meeting or
//     exceeding the phase total (a .partial overshooting its
//     conservative RequiredImageBytes estimate) holds the displayed
//     percent at 100 - no rollback, no >100% - in BOTH renderers.
//     The clamp applies to the item-driven determinate line as well
//     (one renderer-wide rule, one honest ceiling).
//  3. The interactive bar is DRIVEN by BytesWritten (bytes-only
//     description per 9.1's "the completed-count context is bytes,
//     not file count") and the Failed/Interrupted CLEAR-the-bar
//     contract covers a BytesWritten-driven bar exactly like an
//     ItemDone-driven one (the clear is keyed on the bar object).

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// --- plain renderer: the pinned byte-line formats (9.3, 2026-10-20) --

// TestPlainExactFormatByteDrivenPhases pins BOTH new 9.3 examples
// byte-exact: "build(master): 12.3 GiB/21.0 GiB (58%)" and
// "build(extract): 8.1 GiB/21.0 GiB (38%)" - bytes-only (no file
// count), percent derived from bytes and truncated (12.3/21.0 = 58.57
// -> 58; 8.1/21.0 = 38.57 -> 38), one line per 5s window through the
// shared cadence.
func TestPlainExactFormatByteDrivenPhases(t *testing.T) {
	w := &lockedWriter{}
	r, clk := newPlainForTest(w)

	r.handle(Phase{Name: "build(master)", TotalKnown: true, TotalBytes: gibToBytes(21.0)})
	r.handle(BytesWritten{Bytes: gibToBytes(12.3)}) // immediate first line
	clk.Advance(PlainInterval)
	r.handle(Phase{Name: "build(extract)", TotalKnown: true, TotalBytes: gibToBytes(21.0)})
	r.handle(BytesWritten{Bytes: gibToBytes(8.1)}) // the next window's line
	r.writeSummary(Done{})

	got := lines(w.String())
	want := []string{
		"build(master): 12.3 GiB/21.0 GiB (58%)", // the locked 9.3 example
		"build(extract): 8.1 GiB/21.0 GiB (38%)", // the locked 9.3 example
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%q", len(got), len(want), w.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want exact %q", i, got[i], want[i])
		}
	}
}

// TestPlainByteDrivenSaturatesAtHundred pins the 6.2
// saturate-and-hold on the byte-driven line: a position ABOVE the
// phase total (the pathological .partial overshoot) prints (100%),
// never more - and the monotonic hold means a later, SMALLER position
// never rolls the display back.
func TestPlainByteDrivenSaturatesAtHundred(t *testing.T) {
	w := &lockedWriter{}
	r, clk := newPlainForTest(w)

	r.handle(Phase{Name: "build(master)", TotalKnown: true, TotalBytes: 1000})
	r.handle(BytesWritten{Bytes: 1200}) // overshoot: saturate at 100
	clk.Advance(PlainInterval)
	r.handle(BytesWritten{Bytes: 900}) // pathological shrink: held, no rollback
	r.writeSummary(Done{})

	got := lines(w.String())
	want := []string{
		"build(master): 1.2 KB/1.0 KB (100%)",
		"build(master): 1.2 KB/1.0 KB (100%)", // position held at 1200, not rolled back to 900
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%q", len(got), len(want), w.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want exact %q", i, got[i], want[i])
		}
	}
}

// TestPlainItemDrivenPercentAlsoSaturates: the percent cap is ONE
// renderer-wide rule (cappedPercent), so the item-driven determinate
// line saturates identically - completed bytes can exceed walk-time
// estimate totals (the 3.5 changed-during-scan growth divergence
// documented in plain_gaps_test.go); since the 6.2 amendment the plain
// renderer caps that at (100%) instead of printing the raw arithmetic
// ratio (the gap-3 comment is updated accordingly).
func TestPlainItemDrivenPercentAlsoSaturates(t *testing.T) {
	w := &lockedWriter{}
	r, _ := newPlainForTest(w)

	r.handle(Phase{Name: "copy", TotalKnown: true, TotalFiles: 2, TotalBytes: 200})
	r.handle(ItemDone{Bytes: 300}) // a file that grew past its walk lstat size
	r.writeSummary(Done{})

	got := lines(w.String())
	if len(got) != 1 || got[0] != "copy: files 1/2, 300 B/200 B (100%)" {
		t.Fatalf("line = %q, want the saturated determinate form:\n%q", got, w.String())
	}
}

// TestPlainByteDrivenCountsFormResetsAcrossPhases: the byte-driven
// line form is PER PHASE - a BytesWritten-driven phase followed by an
// ItemDone-driven phase (the build sequence's own verify half) renders
// the ordinary counts+bytes form again, and the byte-driven fallback
// form (bytes-only, no totals - the honest indeterminate-spinner
// companion, 6.2) is the counts-only shape without a file count.
func TestPlainByteDrivenCountsFormResetsAcrossPhases(t *testing.T) {
	w := &lockedWriter{}
	r, clk := newPlainForTest(w)

	r.handle(Phase{Name: "build(master)", TotalKnown: true, TotalBytes: 1000})
	r.handle(BytesWritten{Bytes: 400})
	clk.Advance(PlainInterval)
	// build(verify): per-entry ItemDone - the counts+bytes form.
	r.handle(Phase{Name: "build(verify)", TotalKnown: true, TotalFiles: 2, TotalBytes: 1000})
	r.handle(ItemDone{Bytes: 200})
	clk.Advance(PlainInterval)
	// Defensive byte-driven form WITHOUT totals (a phase whose totals
	// never arrived; not a production path today - the byte-driven
	// phases open determinate - but the honest shape if one ever
	// doesn't).
	r.handle(Phase{Name: "build(extract)", TotalKnown: false})
	r.handle(BytesWritten{Bytes: gibToBytes(8.1)})
	r.writeSummary(Done{})

	got := lines(w.String())
	want := []string{
		"build(master): 400 B/1.0 KB (40%)",
		"build(verify): files 1/2, 200 B/1.0 KB (20%)",
		"build(extract): 8.1 GiB",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%q", len(got), len(want), w.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want exact %q", i, got[i], want[i])
		}
	}
}

// --- interactive renderer: BytesWritten-driven bars --------------------

// overPercentRe matches any rendered percent above 100 (the saturation
// contract's negative shape: 101%-199% can never appear).
var overPercentRe = regexp.MustCompile(`1(?:0[1-9]|[1-9][0-9])%`)

// TestInteractiveBytesWrittenDrivesBar: the build(master) shape -
// Phase with the estimate total, then absolute byte positions. The bar
// must (a) show the bytes-only description (9.1: no file count on a
// byte-driven phase), (b) render the byte percent, (c) SATURATE at
// 100% when the position meets/exceeds the total and HOLD there
// through a later, smaller position (no rollback, no >100% frame).
func TestInteractiveBytesWrittenDrivesBar(t *testing.T) {
	w := &lockedWriter{}
	bus := NewBus()
	wait := Start(New(Options{Mode: ModeInteractive, Stderr: w}), bus)

	total := gibToBytes(21.0)
	bus.Emit(Phase{Name: "build(master)", TotalKnown: true, TotalBytes: total})
	bus.Emit(BytesWritten{Bytes: gibToBytes(12.3)}) // the 9.3 example position
	if !waitForOutput(w, func(out string) bool {
		return strings.Contains(out, "build(master): 12.3 GiB/21.0 GiB")
	}, 10*time.Second) {
		t.Fatalf("no bytes-only description for the BytesWritten-driven bar:\n%q", w.String())
	}
	// The pathological overshoot: above the conservative estimate.
	bus.Emit(BytesWritten{Bytes: gibToBytes(25.0)})
	// A later, smaller stat (a .partial recreated): the monotonic hold
	// keeps the display at the highest position seen.
	bus.Emit(BytesWritten{Bytes: gibToBytes(20.0)})
	bus.Finish(Done{Summary: "build: master done"})
	wait()

	out := w.String()
	if !strings.Contains(out, "build(master): 25.0 GiB/21.0 GiB") {
		t.Errorf("the overshooting position did not render in the honest bytes-only description (or the hold rolled back to 20.0):\n%q", out)
	}
	if !strings.Contains(out, "100%") {
		t.Errorf("the saturated bar never rendered 100%%:\n%q", out)
	}
	if overPercentRe.MatchString(out) {
		t.Errorf("the saturated bar rendered a >100%% frame (the Set64 clamp must cap at the total):\n%q", out)
	}
}

// TestInteractiveFailedDoneClearsBytesWrittenBar pins the
// Failed/Interrupted clear-the-bar contract on a BytesWritten-DRIVEN
// bar (the 6.2 amendment applies 6.3's abort contract to all four
// build phases alike): the bar is CLEARED, not completed - no 100%
// claim on a dying run - and the summary still prints.
func TestInteractiveFailedDoneClearsBytesWrittenBar(t *testing.T) {
	for _, tc := range []struct {
		name string
		done Done
	}{
		{"failed", Done{Summary: "build: aborted", Failed: true}},
		{"interrupted", Done{Summary: "build: interrupted", Interrupted: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := &lockedWriter{}
			bus := NewBus()
			wait := Start(New(Options{Mode: ModeInteractive, Stderr: w}), bus)

			bus.Emit(Phase{Name: "build(master)", TotalKnown: true, TotalBytes: 1000})
			bus.Emit(BytesWritten{Bytes: 400})
			bus.Finish(tc.done)
			final := wait()

			out := w.String()
			if strings.Contains(out, "100%") {
				t.Errorf("a %s run rendered a 100%% frame on a BytesWritten-driven bar:\n%q", tc.name, out)
			}
			if !strings.Contains(out, tc.done.Summary) {
				t.Errorf("summary line missing:\n%q", out)
			}
			// The bar was CLEARED, not completed: exactly one newline
			// (the summary's own) - the ItemDone-driven clear tests'
			// shape.
			if n := strings.Count(out, "\n"); n != 1 {
				t.Errorf("output holds %d newlines, want exactly 1 (the bar was cleared):\n%q", n, out)
			}
			if final != tc.done {
				t.Errorf("Run returned %+v, want the terminal Done verbatim", final)
			}
		})
	}
}

// TestInteractiveSuccessDoneCompletesSaturatedBytesBar: the success
// path on a byte-driven bar completes normally (the end-of-bar newline
// plus the summary) - the saturated-at-100% bar is already finished
// internally, so Finish is a no-op frame and no artifact appears.
func TestInteractiveSuccessDoneCompletesSaturatedBytesBar(t *testing.T) {
	w := &lockedWriter{}
	bus := NewBus()
	wait := Start(New(Options{Mode: ModeInteractive, Stderr: w}), bus)

	bus.Emit(Phase{Name: "build(extract)", TotalKnown: true, TotalBytes: 1000})
	bus.Emit(BytesWritten{Bytes: 1000}) // exact completion
	bus.Finish(Done{Summary: "build(extract): complete"})
	wait()

	out := w.String()
	if !strings.Contains(out, "build(extract): 1.0 KB/1.0 KB") {
		t.Errorf("the exact-completion position missing from the bytes-only description:\n%q", out)
	}
	if !strings.Contains(out, "build(extract): complete") {
		t.Errorf("summary line missing:\n%q", out)
	}
	if n := strings.Count(out, "\n"); n != 2 {
		t.Errorf("output holds %d newlines, want exactly 2 (end-of-bar + summary):\n%q", n, out)
	}
}
