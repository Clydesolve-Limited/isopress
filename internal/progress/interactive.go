package progress

// T1.1: the interactive renderer (DESIGN_SPEC 9.2, 9.4, FR-042).
// progressbar/v3 is confined STRICTLY behind this seam (DESIGN_SPEC 11
// binding condition): no package outside internal/progress may import
// it, a condition asserted by an AST-walk test in deps_test.go. The
// plain renderer stays in-tree regardless of this library (9.4), so the
// dependency can be swapped or dropped at a future dependency review
// without touching services.
//
// Behavior: determinate bars where totals are known (FR-042), an
// indeterminate spinner otherwise (progressbar's max == -1 mode, which
// 9.4's "bars and spinners" recommendation refers to). Mid-flight
// totals (DESIGN_SPEC 3.6/9.1/9.2 percent-after-walk, 2026-10-19):
// when a totals event arrives for the already-running indeterminate
// phase, the bar is upgraded IN PLACE to determinate percent + ETA via
// the library's ChangeMax64 - no bar recreation, no counter reset, the
// completed-counts context carries over untouched.
//
// Byte-driven phases (four-phase build progress, DESIGN_SPEC 6.2/9.1,
// 2026-10-20): the BytesWritten event carries the ABSOLUTE cumulative
// position of build(master) (the growing .partial) or build(extract)
// (the growing extraction tree); it drives the bar's byte progress
// against the current Phase's total_bytes. On a byte-driven phase the
// description is bytes-only (no file count - 9.1). SATURATE-AND-HOLD
// (6.2's ruling): the displayed position is capped at the phase total,
// so a position meeting or exceeding the total (a .partial overshooting
// its conservative RequiredImageBytes estimate) holds at 100% - no
// rollback (positions are applied monotonically), no >100% frame, no
// switch back to indeterminate; a bar holding at 100% through xorriso's
// final flush is the honest display.
//
// All output goes to the injected stderr writer (FR-046): the library
// defaults its writer to the process's standard-output handle, so every
// bar constructed here passes OptionSetWriter explicitly and the
// package can never emit a byte to stdout.
//
// Failed runs (Done.Failed, the 2026-10-20 cosmetic-honesty fix) and
// INTERRUPTED runs (Done.Interrupted, the QA fold-in of the same day):
// the bar is CLEARED rather than completed - progressbar's Finish()
// renders a full 100% bar, which on an aborting or signal-interrupted
// command misrepresents how far the run actually got. The clearing is
// driven by the bar OBJECT, so it covers every bar kind equally -
// ItemDone-driven, BytesWritten-driven, spinners, and saturated bars
// alike. The one final summary line still prints on both paths.

import (
	"fmt"
	"io"

	"github.com/schollz/progressbar/v3"

	"isopress/internal/units"
)

// interactiveRenderer renders live bars/spinners to its writer. It is
// selected only when stderr is a TTY and --quiet is absent
// (DESIGN_SPEC 9.2).
type interactiveRenderer struct {
	w io.Writer
}

func (r *interactiveRenderer) Run(bus *Bus) Done {
	acquireBus(bus)
	defer releaseBus(bus)

	var (
		bar        *progressbar.ProgressBar
		phase      string
		totalKnown bool
		byteDriven bool
		files      int64
		totalFiles int64
		bytes      int64
		totalBytes int64
	)

	// newBar builds the bar for a phase: determinate over bytes when
	// the totals are known, indeterminate spinner otherwise (FR-042).
	// On a byte-driven phase (see the BytesWritten case) the
	// description is bytes-only - the phase has no file count (9.1).
	newBar := func(p Phase) *progressbar.ProgressBar {
		desc := phaseDesc(p.Name, 0, p.TotalFiles, 0, p.TotalBytes, p.TotalKnown, byteDriven)
		if p.TotalKnown {
			max := p.TotalBytes
			if max <= 0 {
				// Known totals of zero bytes: keep the bar away
				// from the library's division-by-zero edge; the
				// counts live in the description anyway.
				max = 1
			}
			return progressbar.NewOptions64(max,
				progressbar.OptionSetWriter(r.w),
				progressbar.OptionSetWidth(40),
				progressbar.OptionSetDescription(desc),
				progressbar.OptionSetPredictTime(false),
			)
		}
		return progressbar.NewOptions64(-1, // indeterminate spinner (9.4)
			progressbar.OptionSetWriter(r.w),
			progressbar.OptionSetWidth(40),
			progressbar.OptionSetDescription(desc),
			// Predict-time is ON for spinner bars even though the
			// spinner display never renders the right-hand ETA
			// bracket (progressbar/v3 draws it for determinate
			// bars only): it is display-neutral during the
			// indeterminate window, and it is what lets a mid-phase
			// totals upgrade (see the Phase case below,
			// percent-after-walk, DESIGN_SPEC 3.6/9.2) render the
			// determinate percent + ETA the amendment calls for.
			// It must be fixed at CONSTRUCTION: the flag has no
			// exported runtime setter, and setting config fields
			// directly on a live bar would race the library's
			// spinner ticker goroutine.
			progressbar.OptionSetPredictTime(true),
		)
	}

	// refresh re-renders the current bar with updated counters. The
	// Describe call itself triggers the library's render path, which
	// is what repaints the line for both bar kinds.
	refresh := func() {
		if bar == nil {
			return
		}
		bar.Describe(phaseDesc(phase, files, totalFiles, bytes, totalBytes, totalKnown, byteDriven))
		if totalKnown {
			// SATURATE-AND-HOLD (6.2): clamp the position at the
			// phase total before Set64, so progressbar can never
			// render a >100% frame or complete early on an
			// overshooting byte position. The description keeps the
			// real position (honest numbers; only the bar and its
			// percent saturate).
			pos := bytes
			if pos > totalBytes {
				pos = totalBytes
			}
			_ = bar.Set64(pos)
			return
		}
		// Indeterminate: the spinner animates on its own ticker; the
		// zero-delta add keeps the bar's rate bookkeeping warm without
		// moving any count.
		_ = bar.Add64(0)
	}

	// endBar finishes the current bar with a trailing newline so the
	// next output (a new bar, a warning, or the summary) starts clean.
	// The newline is written explicitly rather than via
	// OptionOnCompletion so it cannot double up.
	endBar := func() {
		if bar == nil {
			return
		}
		_ = bar.Finish()
		fmt.Fprintln(r.w) //nolint:errcheck // best-effort render write: FR-046 makes the stdout report the artifact, a failed stderr write must not perturb the render loop
		bar = nil
	}

	for {
		switch ev := (<-bus.ch).(type) {
		case Phase:
			// Mid-phase totals upgrade (DESIGN_SPEC 3.6/9.1/9.2,
			// percent-after-walk): a totals event naming the
			// ALREADY-RUNNING phase (same name, still indeterminate)
			// upgrades the bar IN PLACE - ChangeMax64 flips the
			// spinner to determinate on the SAME bar object, keeping
			// both the library's current count (0 in spinner mode:
			// the spinner only ever receives zero-delta adds) and,
			// crucially, this renderer's files/bytes counters, so
			// the completed-counts context never resets. ETA note:
			// the spinner window's Add64(0) calls (refresh below)
			// append ZERO-RATE samples to the library's 10-slot
			// rolling byte-rate window every >0.5s, so the first
			// determinate ETA is diluted - roughly a 10x
			// overestimate that converges in ~5s as real samples
			// roll in. This is self-healing and the amendment
			// accepts it; the percent is exact immediately. That
			// zero-delta discipline is LOAD-BEARING and the only
			// guard for it: a nonzero add would hit the spinner's
			// ignoreLength modulo-40 currentNum path and accumulate
			// currentBytes, corrupting the post-upgrade Set64
			// rebase (Set64 computes toAdd = bytes - currentBytes).
			// Structurally upheld here and pinned by
			// interactive_gaps_test.go. A totals event for any other
			// phase name, or before any phase, is an ordinary phase
			// transition.
			if ev.TotalKnown && !totalKnown && bar != nil && ev.Name == phase {
				totalKnown = true
				totalFiles, totalBytes = ev.TotalFiles, ev.TotalBytes
				max := totalBytes
				if max <= 0 {
					// Known totals of zero bytes: the same
					// degenerate guard as newBar.
					max = 1
				}
				bar.ChangeMax64(max)
				refresh()
				continue
			}
			endBar()
			phase, totalKnown = ev.Name, ev.TotalKnown
			byteDriven = false
			files, totalFiles = 0, ev.TotalFiles
			bytes, totalBytes = 0, ev.TotalBytes
			bar = newBar(ev)
		case ItemDone:
			files++
			bytes += ev.Bytes
			refresh()
		case BytesWritten:
			// Byte-driven phases (build master/extract, 6.2): the
			// event carries the ABSOLUTE cumulative position (9.1),
			// so it REPLACES the byte counter - with a monotonic
			// hold, the no-rollback half of 6.2's clamp (a
			// pathological shrink may never move the bar backwards).
			// The first BytesWritten flips the description to the
			// bytes-only form (no file count on a byte-driven phase,
			// 9.1); refresh clamps the position at the total (the
			// saturate-and-hold half).
			byteDriven = true
			if ev.Bytes > bytes {
				bytes = ev.Bytes
			}
			refresh()
		case Tick:
			// THE HEARTBEAT REPAINT (DESIGN_SPEC 9.1 Tick; the
			// 2026-10-20 QA heartbeat fold-in - the build command's
			// run-level heartbeat emits a Tick every
			// cli.HeartbeatInterval): re-render the CURRENT bar IN
			// PLACE - no reset, no count changes. The repaint is
			// driven by refresh's bar.Describe call, verified against
			// the pinned progressbar v3.19.1 source (progressbar.go):
			// Describe sets the description and invokes the bar's
			// render() path UNCONDITIONALLY (its only gate is the
			// by-default zero throttleDuration), while the
			// Set64/Add64 that follow are render-suppressed at an
			// UNCHANGED percent by Add64's percent-change guard
			// (updateBar := currentPercent != lastPercent &&
			// currentPercent > 0 - the same 0%-guard that made
			// ChangeMax64's internal Add(0) silent in the upgrade
			// path, interactive_gaps_test.go gap 2). So a Tick
			// repaints EXACTLY ONE frame with identical counts -
			// visible liveness (the bar line redraws; the spinner
			// advances) without inventing progress. Determinate bars
			// here are built with predict-time OFF, so there is no
			// elapsed-time display to advance: the repaint frame with
			// unchanged counts is the honest observable, pinned by
			// TestInteractiveTickRepaintsDeterminateBarInPlace.
			refresh()
		case Warn:
			// Clear the bar so the warning line is readable, then
			// keep the bar alive for subsequent renders.
			if bar != nil {
				_ = bar.Clear()
			}
			fmt.Fprintf(r.w, "warning: %s\n", ev.Msg) //nolint:errcheck // best-effort render write: FR-046 makes the stdout report the artifact, a failed stderr write must not perturb the render loop
		case Error:
			if bar != nil {
				_ = bar.Clear()
			}
			fmt.Fprintf(r.w, "error: %s\n", ev.Msg) //nolint:errcheck // best-effort render write: FR-046 makes the stdout report the artifact, a failed stderr write must not perturb the render loop
		case Done:
			if ev.Failed || ev.Interrupted {
				// Failed OR Interrupted run (cosmetic-honesty fix,
				// 2026-10-20; the interrupted half is the QA fold-in
				// of the same day): progressbar's Finish() would
				// COMPLETE the bar and render a misleading 100% on a
				// command that is dying with an error OR being aborted
				// by SIGINT/SIGTERM. Clear the bar (and its
				// description line) instead; the summary below is the
				// honest end-of-run record. The two flags are
				// mutually exclusive at the cli layer (progress_done.go),
				// but the renderer treats either as "abort: do not
				// claim completion". The plain renderer needs no such
				// branch - its counts never claimed completion.
				if bar != nil {
					_ = bar.Clear()
					bar = nil
				}
			} else {
				endBar()
			}
			if ev.Summary != "" {
				fmt.Fprintln(r.w, ev.Summary) //nolint:errcheck // best-effort render write: FR-046 makes the stdout report the artifact, a failed stderr write must not perturb the render loop
			}
			return ev
		}
	}
}

// phaseDesc builds the bar description: the phase name plus the same
// counts and bytes the plain renderer shows (the interactive bar adds
// the live percent bar on top, so no percent is embedded here). A
// byte-driven phase (a BytesWritten-driven phase such as build(master)
// or build(extract), DESIGN_SPEC 6.2) has NO file count, so its
// description is bytes-only - matching the plain renderer's pinned
// bytes-only line forms (9.1: "on a byte-driven phase the
// completed-count context is bytes, not file count").
func phaseDesc(phase string, files, totalFiles, bytes, totalBytes int64, totalKnown, byteDriven bool) string {
	if byteDriven {
		if totalKnown {
			return fmt.Sprintf("%s: %s/%s",
				phase, units.FormatBytes(bytes), units.FormatBytes(totalBytes))
		}
		return fmt.Sprintf("%s: %s", phase, units.FormatBytes(bytes))
	}
	if totalKnown {
		return fmt.Sprintf("%s: files %d/%d, %s/%s",
			phase, files, totalFiles,
			units.FormatBytes(bytes), units.FormatBytes(totalBytes))
	}
	return fmt.Sprintf("%s: files %d, %s", phase, files, units.FormatBytes(bytes))
}
