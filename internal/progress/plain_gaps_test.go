package progress

// Gap-closing tests for the percent-after-walk WINDOWS of the plain
// renderer (DESIGN_SPEC 3.6/9.3, stakeholder decision 2026-10-19),
// added by the verification pass of 2026-10-19. plain_test.go already
// pins both frozen 9.3 windows byte-exact (counts-only pre-walk,
// totals-plus-percent post-walk), the upgrade emitting NO line, and
// counter carry-over. THIS file closes four audit gaps:
//
//  1. A totals event with TotalKnown FALSE for an ALREADY-DETERMINATE
//     phase falls through to the documented reset (9.1 gates the
//     in-place upgrade on TotalKnown): no line at the event, counters
//     restart, the counts-only form returns.
//
//  2. The percent BOUND on honest totals: completed bytes are the sum
//     of ItemDone bytes and the totals are the walk's lstat estimates;
//     when files neither fail nor grow, completed == total lands
//     exactly at (100%) and never beyond.
//
//  3. The UNREADABLE-FILE accounting edge, pinned as documented
//     behavior: WalkResult.TotalBytes (and the totals event built from
//     it) is an lstat-based ESTIMATE; the hash phase's ItemDone bytes
//     are read-based REALITY (hashpool.go: open-failure files emit
//     ItemDone with Bytes 0, mid-read failures with the partial
//     count). A file that cannot be opened therefore contributes its
//     full size to the total but zero to the completed bytes: the
//     final determinate line UNDERSHOOTS honestly (70% in the test
//     below) instead of lying, and this direction can never overshoot
//     100%. The opposite divergence - a file that GROWS between walk
//     lstat and hash read (DESIGN_SPEC 3.5 changed-during-scan) - can
//     push completed bytes past the total: DELIBERATE UPDATE
//     (2026-10-20, the 6.2 saturate-and-hold amendment) - the plain
//     renderer now CAPS the printed percent at (100%) through
//     cappedPercent (pre-amendment it printed the raw arithmetic ratio
//     above 100%; one renderer-wide honest ceiling now covers both the
//     item-driven overshoot and the byte-driven master estimate -
//     pinned by TestPlainItemDrivenPercentAlsoSaturates in
//     byteswritten_test.go). The interactive renderer's progressbar
//     likewise never renders past its max (the Set64 clamp in
//     refresh) and holds the bar at 100% until Finish. Both
//     directions are design-accepted consequences of the
//     detect-and-defer model: totals are walk-time estimates.
//
//  4. Known totals of ZERO bytes: no percent is computed (no ratio
//     denominator); the line stays totals-form without the bracket.

import (
	"strings"
	"testing"
)

// TestPlainTotalsUnknownFallsThroughToReset: TotalKnown=false for the
// running determinate phase resets (see file doc, gap 1). The event
// itself emits no line; the NEXT scheduled line is counts-only with
// reset counters under the same prefix.
func TestPlainTotalsUnknownFallsThroughToReset(t *testing.T) {
	w := &lockedWriter{}
	r, clk := newPlainForTest(w)

	r.handle(Phase{Name: "copy", TotalKnown: true, TotalFiles: 10, TotalBytes: 1000})
	r.handle(ItemDone{Bytes: 100}) // immediate determinate line
	// TotalKnown false for the RUNNING determinate phase: the
	// in-place upgrade is gated on TotalKnown (9.1), so this is an
	// ordinary transition - counters reset, throttle untouched, NO
	// line emitted at the event itself.
	r.handle(Phase{Name: "copy", TotalKnown: false})
	r.handle(ItemDone{Bytes: 50}) // throttled: 0s since the last line
	clk.Advance(PlainInterval)
	r.handle(Tick{}) // prints: counts-only, RESET counters
	r.writeSummary(Done{Summary: "copy: complete"})

	got := lines(w.String())
	want := []string{
		"copy: files 1/10, 100 B/1.0 KB (10%)",
		"copy: files 1, 50 B",
		"copy: complete",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d (the reset event itself must emit NO line):\n%q",
			len(got), len(want), w.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want exact %q", i, got[i], want[i])
		}
	}
}

// TestPlainPostWalkPercentReachesHundredExactly: the normal-case
// percent bound - completed bytes equal the walk totals and the line
// reads exactly (100%), derived from bytes, truncated (3.6/9.3).
func TestPlainPostWalkPercentReachesHundredExactly(t *testing.T) {
	w := &lockedWriter{}
	r, clk := newPlainForTest(w)

	r.handle(Phase{Name: "scan(hash)"})
	r.handle(ItemDone{Bytes: 100}) // pre-walk window: counts-only
	r.handle(Phase{Name: "scan(hash)", TotalKnown: true, TotalFiles: 2, TotalBytes: 400})
	r.handle(ItemDone{Bytes: 300}) // throttled
	clk.Advance(PlainInterval)
	r.handle(Tick{}) // settled state: 400/400
	r.writeSummary(Done{})

	got := lines(w.String())
	want := []string{
		"scan(hash): files 1, 100 B",
		"scan(hash): files 2/2, 400 B/400 B (100%)",
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

// TestPlainUnreadableFileUndershootsPercentHonestly pins the
// documented totals-vs-completed divergence (see file doc, gap 3) at
// the renderer boundary: two files, 700 B readable and 300 B that
// fails at open. The walk's totals (lstat estimates) count both in
// full: TotalFiles 2, TotalBytes 1000. The hash phase emits
// ItemDone{700} for the readable file and ItemDone{Bytes:0} for the
// unreadable one (hashpool.go: one ItemDone per COMPLETED file,
// failure included, carrying the bytes actually read - zero at an
// open failure). The determinate line therefore reads exactly
// "files 2/2, 700 B/1.0 KB (70%)": the files count is complete, the
// byte percent undershoots, and the failure itself is reported
// separately (progress.Error/run_events - T1.5's surface, out of this
// renderer's scope). The pin guards both halves of the contract: the
// failed file's TOTAL stays in (the walk cannot know it will fail),
// and the percent never overshoots in this direction.
func TestPlainUnreadableFileUndershootsPercentHonestly(t *testing.T) {
	w := &lockedWriter{}
	r, clk := newPlainForTest(w)

	r.handle(Phase{Name: "scan(hash)"})
	r.handle(ItemDone{Bytes: 700}) // the readable file, pre-walk window
	// Walk completion: the unreadable file's lstat size (300) is in
	// the totals - the walk stats it successfully; only the hash
	// READ fails later.
	r.handle(Phase{Name: "scan(hash)", TotalKnown: true, TotalFiles: 2, TotalBytes: 1000})
	r.handle(ItemDone{Bytes: 0}) // the open-failure file: completed, 0 bytes read
	clk.Advance(PlainInterval)
	r.handle(Tick{}) // settled state
	r.writeSummary(Done{})

	got := lines(w.String())
	want := []string{
		"scan(hash): files 1, 700 B",
		"scan(hash): files 2/2, 700 B/1.0 KB (70%)",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%q", len(got), len(want), w.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want exact %q", i, got[i], want[i])
		}
	}
	// The bound this edge must hold, stated independently of the exact
	// match above: an unreadable file can only UNDERSHOOT the percent
	// (read bytes <= lstat size), never push it to 100 or beyond.
	if strings.Contains(got[1], "(100%)") {
		t.Errorf("unreadable-file line claims 100%% despite 300 of 1000 total bytes never being read: %q", got[1])
	}
}

// TestPlainZeroByteTotalsOmitPercent: determinate totals of zero bytes
// have no ratio denominator, so the percent bracket is omitted while
// the totals form itself holds (gap 4; mirrors the interactive
// renderer's max<=0 degenerate guard).
func TestPlainZeroByteTotalsOmitPercent(t *testing.T) {
	w := &lockedWriter{}
	r, clk := newPlainForTest(w)

	r.handle(Phase{Name: "copy", TotalKnown: true, TotalFiles: 2, TotalBytes: 0})
	r.handle(ItemDone{Bytes: 0})
	r.handle(ItemDone{Bytes: 0}) // throttled: still inside the first cadence window
	clk.Advance(PlainInterval)
	r.handle(Tick{})
	r.writeSummary(Done{})

	got := lines(w.String())
	want := []string{
		"copy: files 1/2, 0 B/0 B",
		"copy: files 2/2, 0 B/0 B",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%q", len(got), len(want), w.String())
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want exact %q (no percent without a denominator)", i, got[i], want[i])
		}
	}
}
