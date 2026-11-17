package progress

// T1.1 tests for the plain renderer (DESIGN_SPEC 9.3, FR-044): the
// exact literal line formats locked by the spec examples, the 5-second
// cadence throttled against the fake clock (DESIGN_SPEC 12.1), the
// always-print final summary, and the no-throttle contract for
// warnings, errors, and the terminal summary.
//
// The format and cadence tests drive the renderer's synchronous
// handle/writeSummary core directly on the test goroutine: the fake
// clock must advance in lockstep with event processing, which the
// asynchronous Run loop (correctly) cannot guarantee for a test that
// races ahead of the renderer goroutine. The full bus/Run lifecycle is
// covered end-to-end in progress_test.go.

import (
	"io"
	"strings"
	"testing"
	"time"
)

const gib = int64(1) << 30 // units.BytesPerGiB, inlined to keep the format tests readable

// gibToBytes converts a (possibly fractional) GiB figure to whole bytes
// at runtime - e.g. gibToBytes(4.2) = 4_509_715_660, which
// units.FormatBytes renders as "4.2 GiB". The function boundary keeps
// the fractional constant out of a compile-time int64 conversion.
func gibToBytes(g float64) int64 { return int64(g * float64(gib)) }

// newPlainForTest wires a plain renderer onto a locked writer and a
// fake clock for synchronous driving.
func newPlainForTest(w io.Writer) (*plainRenderer, *fakeClock) {
	clk := newFakeClock()
	return &plainRenderer{w: w, now: clk.Now}, clk
}

// TestPlainThrottleWithFakeClock: events at 1-second intervals produce
// EXACTLY ONE plain line per 5-second window (the first progress event
// prints immediately so short operations are observable; the next
// requires PlainInterval elapsed), and the final Done produces exactly
// one summary line on top. Line contents pin the windows: counts 1, 6,
// 11 are the only ones rendered from 15 one-second items.
func TestPlainThrottleWithFakeClock(t *testing.T) {
	w := &lockedWriter{}
	r, clk := newPlainForTest(w)

	r.handle(Phase{Name: "copy", TotalKnown: true, TotalFiles: 5678, TotalBytes: 23 * gib})
	for i := 0; i < 15; i++ {
		clk.Advance(time.Second)
		r.handle(ItemDone{Bytes: gib / 10})
	}
	r.writeSummary(Done{Summary: "copy: complete (15 files)"})

	got := lines(w.String())
	if len(got) != 4 {
		t.Fatalf("got %d lines, want exactly 4 (3 throttled progress + 1 summary):\n%q",
			len(got), w.String())
	}
	// One line per 5-second window: t=1 (immediate), t=6, t=11; the
	// t=12..15 events fall inside the last window and are throttled.
	for i, want := range []string{"files 1/5678", "files 6/5678", "files 11/5678"} {
		if !strings.Contains(got[i], want) {
			t.Errorf("progress line %d = %q, want it to contain %q", i, got[i], want)
		}
	}
	if got[3] != "copy: complete (15 files)" {
		t.Errorf("summary line = %q, want the exact final summary", got[3])
	}
}

// TestPlainExactFormatWithKnownTotals asserts the first DESIGN_SPEC 9.3
// example verbatim: "copy: files 1234/5678, 4.2 GiB/23.0 GiB (18%)".
// The events are constructed so the throttled line at t=5s carries
// exactly 1234 completed items, 4.2 GiB of 23.0 GiB bytes; the percent
// is derived from bytes (4.2/23.0 = 18.26 -> 18, truncated - the files
// ratio would be 21.7, which the spec example rules out).
func TestPlainExactFormatWithKnownTotals(t *testing.T) {
	w := &lockedWriter{}
	r, clk := newPlainForTest(w)

	r.handle(Phase{Name: "copy", TotalKnown: true, TotalFiles: 5678, TotalBytes: 23 * gib})
	for i := 0; i < 1233; i++ {
		r.handle(ItemDone{Bytes: 0}) // first prints at t=0, the rest are throttled
	}
	clk.Advance(PlainInterval)
	r.handle(ItemDone{Bytes: gibToBytes(4.2)}) // the 1234th item
	r.writeSummary(Done{})

	got := lines(w.String())
	want := []string{
		"copy: files 1/5678, 0 B/23.0 GiB (0%)",         // immediate first line
		"copy: files 1234/5678, 4.2 GiB/23.0 GiB (18%)", // the locked 9.3 example
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

// TestPlainExactFormatUnknownTotals asserts the second DESIGN_SPEC 9.3
// example verbatim: "scan(hash): files 45210, 88.3 GiB" - the
// counts-and-bytes shape used when the phase total is not known in
// advance (DESIGN_SPEC 3.6, full-tree scans). No totals, no percent.
func TestPlainExactFormatUnknownTotals(t *testing.T) {
	w := &lockedWriter{}
	r, clk := newPlainForTest(w)

	r.handle(Phase{Name: "scan(hash)"}) // TotalKnown false
	r.handle(ItemDone{Bytes: gibToBytes(88.3)})
	for i := 1; i < 45210; i++ {
		r.handle(ItemDone{Bytes: 0})
	}
	clk.Advance(PlainInterval)
	r.handle(Tick{}) // heartbeat renders the settled counts
	r.writeSummary(Done{})

	got := lines(w.String())
	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2 (immediate + throttled heartbeat):\n%q", len(got), w.String())
	}
	if got[1] != "scan(hash): files 45210, 88.3 GiB" {
		t.Errorf("line 1 = %q, want the exact 9.3 example %q",
			got[1], "scan(hash): files 45210, 88.3 GiB")
	}
}

// TestPlainExactFormatAfterWalkTotals pins BOTH windows of the
// percent-after-walk amendment (DESIGN_SPEC 3.6/9.3, stakeholder
// 2026-10-19) in one continuous stream. The pre-walk window line is
// the counts-only form (already pinned byte-exact by
// TestPlainExactFormatUnknownTotals, which still stands); this test
// pins the transition and the post-walk line byte-exact:
// "scan(hash): files 12340/56283, 30.1 GiB/210.3 GiB (14%)" - percent
// from bytes (30.1/210.3 = 14.3 -> 14, truncated), counters carried
// over from the pre-walk window (no reset), and NO line emitted at the
// upgrade itself (cadence and immediate-line rules unchanged).
func TestPlainExactFormatAfterWalkTotals(t *testing.T) {
	w := &lockedWriter{}
	r, clk := newPlainForTest(w)

	// Pre-walk window: indeterminate phase, first item prints
	// immediately in the counts-only form.
	r.handle(Phase{Name: "scan(hash)"})
	r.handle(ItemDone{Bytes: gibToBytes(30.1)})

	// The walk completes while hashing continues: ONE totals event,
	// adopted in place - counters kept, throttle untouched, NO line.
	r.handle(Phase{Name: "scan(hash)", TotalKnown: true, TotalFiles: 56283, TotalBytes: gibToBytes(210.3)})
	for i := 0; i < 12338; i++ {
		r.handle(ItemDone{Bytes: 0}) // all throttled: still inside the first cadence window
	}
	clk.Advance(PlainInterval)
	r.handle(ItemDone{Bytes: 0}) // the 12340th item: cadence elapsed, determinate form
	r.writeSummary(Done{})

	got := lines(w.String())
	want := []string{
		"scan(hash): files 1, 30.1 GiB",                           // pre-walk window (counts-only)
		"scan(hash): files 12340/56283, 30.1 GiB/210.3 GiB (14%)", // post-walk window (determinate)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d (the totals event itself must emit NO line):\n%q",
			len(got), len(want), w.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want exact %q", i, got[i], want[i])
		}
	}
}

// TestPlainTotalsForDifferentPhaseResets: a totals event naming a
// DIFFERENT phase is an ordinary phase transition (new prefix, reset
// counters, determinate form) - the in-place upgrade only applies to
// the currently-running phase (name equality, DESIGN_SPEC 9.1).
func TestPlainTotalsForDifferentPhaseResets(t *testing.T) {
	w := &lockedWriter{}
	r, clk := newPlainForTest(w)

	r.handle(Phase{Name: "scan(hash)"})
	r.handle(ItemDone{Bytes: 100}) // immediate counts-only line
	clk.Advance(PlainInterval)
	r.handle(Phase{Name: "copy", TotalKnown: true, TotalFiles: 10, TotalBytes: 1000})
	r.handle(ItemDone{Bytes: 180}) // prints under the NEW phase with RESET counters
	r.writeSummary(Done{})

	got := lines(w.String())
	want := []string{
		"scan(hash): files 1, 100 B",
		"copy: files 1/10, 180 B/1.0 KB (18%)", // 1000 bytes formats as "1.0 KB"
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

// TestPlainPhaseTransitionResetsCountersNotThrottle: a new Phase resets
// the counts (new prefix, zeroed totals) but does not reset the
// cadence clock - the 9.3 cadence is "per operation", so a multi-phase
// operation cannot print more than one line per PlainInterval even at
// phase boundaries.
func TestPlainPhaseTransitionResetsCountersNotThrottle(t *testing.T) {
	w := &lockedWriter{}
	r, clk := newPlainForTest(w)

	r.handle(Phase{Name: "scan(walk)", TotalKnown: true, TotalFiles: 10, TotalBytes: 100})
	r.handle(ItemDone{Bytes: 100}) // immediate first line
	clk.Advance(time.Second)
	r.handle(Phase{Name: "scan(hash)"})
	r.handle(ItemDone{Bytes: 50}) // throttled: only 1s since the last line
	clk.Advance(PlainInterval)
	r.handle(ItemDone{Bytes: 50}) // prints, under the NEW phase prefix
	r.writeSummary(Done{})

	got := lines(w.String())
	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2 (one per phase, cadence is per-operation):\n%q", len(got), w.String())
	}
	if !strings.HasPrefix(got[0], "scan(walk): ") {
		t.Errorf("first line = %q, want the scan(walk) prefix", got[0])
	}
	if got[1] != "scan(hash): files 2, 100 B" {
		t.Errorf("second line = %q, want the scan(hash) counts-only line after the reset", got[1])
	}
}

// TestPlainWarningsAndErrorsBypassThrottle: warnings and errors are
// rare and important (they mirror run_events), so they print
// immediately regardless of the progress cadence.
func TestPlainWarningsAndErrorsBypassThrottle(t *testing.T) {
	w := &lockedWriter{}
	r, clk := newPlainForTest(w)

	r.handle(Phase{Name: "copy"})
	clk.Advance(time.Second)
	r.handle(ItemDone{Bytes: 100}) // progress line (immediate first)
	clk.Advance(time.Second)
	r.handle(Warn{Msg: "skipping special file: /dev/null"})
	r.handle(Error{Msg: "unreadable: /root/x (permission denied)"})
	clk.Advance(time.Second)
	r.handle(Tick{}) // throttled: 3s since the progress line
	r.writeSummary(Done{})

	got := lines(w.String())
	want := []string{
		"copy: files 1, 100 B",
		"warning: skipping special file: /dev/null",
		"error: unreadable: /root/x (permission denied)",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%q", len(got), len(want), w.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestPlainSummaryBypassesThrottle: a Done arriving well inside the
// cadence window still prints its summary line - the throttle never
// suppresses the end-of-run record.
func TestPlainSummaryBypassesThrottle(t *testing.T) {
	w := &lockedWriter{}
	r, clk := newPlainForTest(w)

	r.handle(Phase{Name: "copy"})
	r.handle(ItemDone{Bytes: 1}) // prints at t0
	clk.Advance(time.Second)     // far inside the 5s window
	r.writeSummary(Done{Summary: "copy: complete (1 files)"})

	got := lines(w.String())
	if len(got) != 2 || got[1] != "copy: complete (1 files)" {
		t.Fatalf("summary not printed exactly once despite throttle window:\n%q", w.String())
	}
}

// TestPlainEmptySummaryPrintsNothing: a service that supplies no
// summary gets no summary line (documented: services own the end-of-run
// text; Done{} still terminates Run). The immediate first progress line
// remains the only output.
func TestPlainEmptySummaryPrintsNothing(t *testing.T) {
	w := &lockedWriter{}
	r, _ := newPlainForTest(w)

	r.handle(Phase{Name: "copy"})
	r.handle(ItemDone{Bytes: 1})
	r.writeSummary(Done{})

	if got := len(lines(w.String())); got != 1 {
		t.Errorf("got %d lines, want 1 (progress only, empty summary prints nothing):\n%q", got, w.String())
	}
}
