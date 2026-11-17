package progress

// Cross-phase renderer audit (DESIGN_SPEC 6.2 four-phase build
// progress; the 2026-10-20 "looks hung" amendment): the byte-driven
// line/bar form is PER PHASE. A Phase transition resets byteDriven
// (both renderers), so an ItemDone-driven phase following a
// byte-driven one renders the ordinary counts+bytes form - the plain
// half of this is pinned by
// TestPlainByteDrivenCountsFormResetsAcrossPhases; the INTERACTIVE
// half is pinned here.
//
// Why it matters (the cross-phase audit question): the renderers are
// phase-AGNOSTIC about BytesWritten - the event flips the CURRENT
// phase's bar to its byte-driven form regardless of which phase is
// running. The producer-side ordering (watchers stop()-ed, FIFO bus)
// keeps stray frames out of later phases (pinned in
// internal/imagebuild and internal/verify); the transition reset is
// the renderer-side complement that keeps the DISPLAY contract honest
// across the stage -> master -> extract -> verify sequence.

import (
	"strings"
	"testing"
	"time"

	"isopress/internal/units"
)

// TestInteractivePhaseTransitionResetsByteDrivenForm: a
// build(master)-shaped byte-driven phase followed by a
// build(verify)-shaped ItemDone phase - the verify frames must carry
// the counts+bytes description ("files 1/2, 200 B/1.0 KB"), NOT the
// byte-driven bytes-only form the master frames used. If the reset
// were missing, the verify bar would render "build(verify): 200 B/1.0
// KB" (no file count) and every subsequent ItemDone would move only
// the byte counter - the cross-phase display corruption the audit
// hunts for.
func TestInteractivePhaseTransitionResetsByteDrivenForm(t *testing.T) {
	w := &lockedWriter{}
	bus := NewBus()
	wait := Start(New(Options{Mode: ModeInteractive, Stderr: w}), bus)

	// build(master): byte-driven.
	bus.Emit(Phase{Name: "build(master)", TotalKnown: true, TotalBytes: 1000})
	bus.Emit(BytesWritten{Bytes: 500})
	if !waitForOutput(w, func(out string) bool {
		return strings.Contains(out, "build(master): 500 B/1.0 KB")
	}, 10*time.Second) {
		t.Fatalf("the byte-driven master description never rendered:\n%q", w.String())
	}

	// build(verify): ItemDone-driven - the transition must reset the
	// byte-driven form.
	bus.Emit(Phase{Name: "build(verify)", TotalKnown: true, TotalFiles: 2, TotalBytes: 1000})
	bus.Emit(ItemDone{Bytes: 200})
	if !waitForOutput(w, func(out string) bool {
		return strings.Contains(out, "build(verify): files 1/2, 200 B/1.0 KB")
	}, 10*time.Second) {
		t.Fatalf("the verify phase never rendered its counts+bytes form "+
			"(a leaked byte-driven form would read \"build(verify): 200 B/1.0 KB\"):\n%q", w.String())
	}

	bus.Finish(Done{Summary: "build: cross-phase reset"})
	wait()
	out := w.String()
	if strings.Contains(out, "build(verify): "+units.FormatBytes(200)+"/") {
		t.Errorf("the verify frames kept the byte-driven bytes-only description after the phase transition:\n%q", out)
	}
}
