package progress

// T1.1 tests for the interactive renderer's mid-phase determinate
// upgrade (DESIGN_SPEC 3.6/9.1/9.2 percent-after-walk, stakeholder
// decision 2026-10-19): when a totals event arrives for the
// already-running indeterminate phase, the SAME bar upgrades in place
// to determinate percent + ETA - the completed counts carry over, the
// counts-only spinner description disappears, and no extra bar or line
// artifact is produced. Exact bar formatting belongs to
// progressbar/v3, so assertions are exact on the description (this
// tool's format, phaseDesc) and shaped on the library's percent and
// time-bracket rendering.
//
// The test drives the REAL async Run loop over a Bus (the interactive
// renderer has no synchronous handle core and no clock seam: its
// cadence is event-driven, so the event sequence itself is the
// determinism - every assertion is on the final accumulated output,
// valid under any spinner-ticker interleaving).

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

// etaBracketRe matches progressbar/v3's determinate time bracket
// "[<elapsed>:<eta>]" (renderDeterminateProgressBar). The indeterminate
// spinner renders at most a single-value elapsed bracket "[0s]" with no
// colon (DESIGN_SPEC 9.4 bars-and-spinners mode), so a colon inside a
// bracket is unique to determinate-with-prediction output.
var etaBracketRe = regexp.MustCompile(`\[[^]\n]*:[^]\n]*\]`)

// TestInteractiveMidPhaseTotalsUpgrade: three files hash under the
// indeterminate scan(hash) spinner, then the walk-completion totals
// event arrives for the SAME phase, then two more files hash. The bar
// must (a) become determinate - description flips to files N/T form
// and the rendered output carries a percent and an elapsed:eta time
// bracket; (b) keep the completed counts - the final description
// shows 5/10 (3 pre-upgrade + 2 post-upgrade), not 2/10 (reset) and
// not 10/10 (recreated from totals); (c) produce no duplicate
// bar/line artifacts - the counts-only description never reappears
// after the upgrade, and the output holds exactly two newlines (the
// end-of-bar newline plus the summary line).
func TestInteractiveMidPhaseTotalsUpgrade(t *testing.T) {
	w := &lockedWriter{}
	bus := NewBus()
	wait := Start(New(Options{Mode: ModeInteractive, Stderr: w}), bus)

	// Indeterminate hash phase (what the pool emits at construction).
	bus.Emit(Phase{Name: "scan(hash)"})
	bus.Emit(ItemDone{Bytes: 100, Path: "a"})
	bus.Emit(ItemDone{Bytes: 100, Path: "b"})
	bus.Emit(ItemDone{Bytes: 100, Path: "c"})
	// Walk completion: totals for the STILL-RUNNING phase, the
	// scan.go emitScanWalkTotals event shape.
	bus.Emit(Phase{Name: "scan(hash)", TotalKnown: true, TotalFiles: 10, TotalBytes: 1000})
	bus.Emit(ItemDone{Bytes: 100, Path: "d"})
	bus.Emit(ItemDone{Bytes: 100, Path: "e"})
	bus.Finish(Done{Summary: "scan(hash): complete (5 files)"})
	final := wait()

	out := w.String()
	if out == "" {
		t.Fatal("interactive renderer produced no output at all")
	}

	// (a) Determinate: the final description is the files N/T form
	// (units.FormatBytes renders the 1000-byte total as "1.0 KB"),
	// and the output carries a percent plus the elapsed:eta bracket
	// that only determinate-with-prediction frames render.
	if !strings.Contains(out, "scan(hash): files 5/10, 500 B/1.0 KB") {
		t.Errorf("output lacks the determinate post-upgrade description:\n%q", out)
	}
	if !strings.Contains(out, "50%") && !strings.Contains(out, "100%") {
		t.Errorf("output carries no rendered percent after the upgrade:\n%q", out)
	}
	if !etaBracketRe.MatchString(out) {
		t.Errorf("output carries no [elapsed:eta] time bracket after the upgrade:\n%q", out)
	}

	// (b) Completed counts preserved: 5/10 proves the 3 pre-upgrade
	// completions survived the upgrade (a reset would show 2/10; a
	// bar recreated from the totals description alone would not have
	// accumulated the pre-upgrade bytes either - 500 B pins the
	// byte counter too, already asserted above).
	if !strings.Contains(out, "files 5/10") {
		t.Errorf("completed counts were lost across the upgrade:\n%q", out)
	}

	// (c) No duplicate bar/line artifacts: after the upgrade the
	// counts-only description ("files N, ..." without totals) must
	// never be rendered again - it would reappear if the upgrade had
	// torn down the bar and started a fresh spinner instead of
	// upgrading in place. And exactly two newlines: the end-of-bar
	// newline plus the summary line (bar frames are \r-redraws).
	if strings.Contains(out, "scan(hash): files 5, ") {
		t.Errorf("counts-only description reappeared after the upgrade (duplicate bar?):\n%q", out)
	}
	if n := strings.Count(out, "\n"); n != 2 {
		t.Errorf("output holds %d newlines, want exactly 2 (end-of-bar + summary); duplicate line artifacts:\n%q",
			n, out)
	}
	if !strings.Contains(out, "scan(hash): complete (5 files)") {
		t.Errorf("final summary line missing:\n%q", out)
	}
	if final.Summary != "scan(hash): complete (5 files)" {
		t.Errorf("Run returned %+v, want the completion Done", final)
	}
}

// TestInteractiveTotalsForDifferentPhaseStartsNewBar: a totals event
// naming a DIFFERENT phase is an ordinary phase transition - the
// running spinner is ended cleanly and a fresh determinate bar starts
// (the in-place upgrade is name-gated, DESIGN_SPEC 9.1).
func TestInteractiveTotalsForDifferentPhaseStartsNewBar(t *testing.T) {
	w := &lockedWriter{}
	bus := NewBus()
	wait := Start(New(Options{Mode: ModeInteractive, Stderr: w}), bus)

	bus.Emit(Phase{Name: "scan(hash)"})
	bus.Emit(ItemDone{Bytes: 100, Path: "a"})
	// Different name: NOT an upgrade of scan(hash) - a new phase.
	bus.Emit(Phase{Name: "copy", TotalKnown: true, TotalFiles: 2, TotalBytes: 200})
	bus.Emit(ItemDone{Bytes: 100, Path: "S-001/file"})
	bus.Finish(Done{Summary: "copy: complete"})
	wait()

	out := w.String()
	if !strings.Contains(out, "copy: files 1/2, 100 B/200 B") {
		t.Errorf("output lacks the new determinate phase description:\n%q", out)
	}
	if !strings.Contains(out, "scan(hash): files 1, 100 B") {
		t.Errorf("output lacks the first phase's counts-only description:\n%q", out)
	}
	// The transition ended the first bar: two end-of-bar newlines plus
	// the summary.
	if n := strings.Count(out, "\n"); n != 3 {
		t.Errorf("output holds %d newlines, want exactly 3 (two end-of-bar + summary):\n%q", n, out)
	}
}

// --- the failed-run cosmetic-honesty fix (2026-10-20) -------------------------

// TestInteractiveFailedDoneClearsBarWithoutCompletion pins the
// Done.Failed contract on the interactive renderer: a determinate bar
// that would render "100%" when progressbar's Finish() runs is instead
// CLEARED on a Failed Done - the output must contain no "100%" frame,
// no end-of-bar newline (the bar line was erased, not completed), and
// exactly the one honest summary line. The stakeholder failure this
// fixes: a refused build rendered "files 0/1315, 0 B/21.4 GiB 100%".
func TestInteractiveFailedDoneClearsBarWithoutCompletion(t *testing.T) {
	w := &lockedWriter{}
	bus := NewBus()
	wait := Start(New(Options{Mode: ModeInteractive, Stderr: w}), bus)

	bus.Emit(Phase{Name: "build", TotalKnown: true, TotalFiles: 1315, TotalBytes: 23_000_000_000})
	bus.Emit(ItemDone{Bytes: 1_000_000, Path: "a"})
	bus.Finish(Done{Summary: "build: 0 image(s) built (0 B), 1 failed, warnings 0", Failed: true})
	final := wait()

	out := w.String()
	if strings.Contains(out, "100%") {
		t.Errorf("a FAILED Done rendered a 100%% frame (Finish must not complete the bar):\n%q", out)
	}
	if !strings.Contains(out, "build: 0 image(s) built (0 B), 1 failed, warnings 0") {
		t.Errorf("the honest summary line is missing:\n%q", out)
	}
	// The bar line was CLEARED, not completed: exactly one newline in
	// the whole output - the summary line's own. A successful endBar
	// would add its end-of-bar newline (two total).
	if n := strings.Count(out, "\n"); n != 1 {
		t.Errorf("output holds %d newlines, want exactly 1 (the summary; the bar was cleared):\n%q", n, out)
	}
	if final.Failed != true || final.Summary == "" {
		t.Errorf("Run returned %+v, want the Failed Done passed through faithfully", final)
	}
}

// TestInteractiveInterruptedDoneClearsBarWithoutCompletion pins the
// Done.Interrupted contract on the interactive renderer (the QA
// fold-in of 2026-10-20): before it, an INTERRUPTED run hit endBar()
// and rendered the same misleading "100%" frame the Failed flag had
// just fixed. The contract is now identical to the Failed path: the
// bar is CLEARED, the interrupted summary still prints, and Run
// returns the Interrupted flag faithfully.
func TestInteractiveInterruptedDoneClearsBarWithoutCompletion(t *testing.T) {
	w := &lockedWriter{}
	bus := NewBus()
	wait := Start(New(Options{Mode: ModeInteractive, Stderr: w}), bus)

	bus.Emit(Phase{Name: "copy", TotalKnown: true, TotalFiles: 3, TotalBytes: 300})
	bus.Emit(ItemDone{Bytes: 100, Path: "a"})
	bus.Emit(ItemDone{Bytes: 100, Path: "b"})
	bus.Finish(Done{Summary: "copy: files 2 copied (0 already present), 200 B; skipped 0, failed 0, dirs 1, metadata warnings 0", Interrupted: true})
	final := wait()

	out := w.String()
	if strings.Contains(out, "100%") {
		t.Errorf("an INTERRUPTED Done rendered a 100%% frame (Finish must not complete the bar):\n%q", out)
	}
	if !strings.Contains(out, "copy: files 2 copied") {
		t.Errorf("the interrupted summary line is missing:\n%q", out)
	}
	// The bar line was CLEARED, not completed: exactly one newline in
	// the whole output - the summary line's own (same shape as the
	// Failed test above).
	if n := strings.Count(out, "\n"); n != 1 {
		t.Errorf("output holds %d newlines, want exactly 1 (the summary; the bar was cleared):\n%q", n, out)
	}
	if !final.Interrupted || final.Failed {
		t.Errorf("Run returned %+v, want the Interrupted Done passed through faithfully (and Failed false)", final)
	}
}

// TestInteractiveSuccessDoneStillCompletesBar is the success-path
// control: Done without Failed keeps the exact pre-amendment render -
// the bar COMPLETES (a 100% frame), the end-of-bar newline prints,
// then the summary (two newlines total).
func TestInteractiveSuccessDoneStillCompletesBar(t *testing.T) {
	w := &lockedWriter{}
	bus := NewBus()
	wait := Start(New(Options{Mode: ModeInteractive, Stderr: w}), bus)

	bus.Emit(Phase{Name: "build", TotalKnown: true, TotalFiles: 2, TotalBytes: 200})
	bus.Emit(ItemDone{Bytes: 100, Path: "a"})
	bus.Emit(ItemDone{Bytes: 100, Path: "b"})
	bus.Finish(Done{Summary: "build: 1 image(s) built (200 B), 0 failed, warnings 0"})
	final := wait()

	out := w.String()
	if !strings.Contains(out, "100%") {
		t.Errorf("a successful Done must still complete the bar to 100%%:\n%q", out)
	}
	if !strings.Contains(out, "build: 1 image(s) built (200 B), 0 failed, warnings 0") {
		t.Errorf("the summary line is missing:\n%q", out)
	}
	if n := strings.Count(out, "\n"); n != 2 {
		t.Errorf("output holds %d newlines, want exactly 2 (end-of-bar + summary):\n%q", n, out)
	}
	if final.Failed {
		t.Errorf("Run returned Failed=true on a successful Done: %+v", final)
	}
}

// TestPlainRendererFailedDoneUnaffected pins the "plain renderer
// unaffected" half of the fix: its counts-and-percent lines never
// claimed completion, so a Failed Done renders exactly as before -
// the summary line, once, with no rendering change.
func TestPlainRendererFailedDoneUnaffected(t *testing.T) {
	w := &lockedWriter{}
	r := &plainRenderer{w: w, now: func() time.Time { return time.Unix(0, 0) }}
	r.handle(Phase{Name: "build", TotalKnown: true, TotalFiles: 1315, TotalBytes: 23_000_000_000})
	r.handle(ItemDone{Bytes: 1_000_000})
	done := r.Run(startPlainBus(t, Done{Summary: "build: 0 image(s) built", Failed: true}))

	out := w.String()
	if !strings.Contains(out, "build: files 1/1315, 1.0 MB/21.4 GiB") {
		t.Errorf("plain progress line changed on a failed run (it was already honest):\n%q", out)
	}
	if strings.Contains(out, "(100%)") {
		t.Errorf("plain renderer shows 100%% on a failed run:\n%q", out)
	}
	if !strings.Contains(out, "build: 0 image(s) built") {
		t.Errorf("summary line missing:\n%q", out)
	}
	if !done.Failed {
		t.Error("Run did not pass the Failed flag through")
	}
}

// startPlainBus feeds ev to a fresh bus already primed with the given
// terminal Done (the synchronous plain-renderer harness pattern).
func startPlainBus(t *testing.T, d Done) *Bus {
	t.Helper()
	bus := NewBus()
	go func() { bus.Finish(d) }()
	return bus
}
