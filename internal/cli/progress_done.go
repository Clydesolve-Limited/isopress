package cli

// The progress Done-flag wiring of the 2026-10-20 cosmetic-honesty fix
// (plus the interrupted-half QA fold-in of the same day). Every command
// that owns a progress bus computes its terminal Done event in a
// deferred closure; these two predicates classify the command's
// returned error into the TWO mutually exclusive abort signals of
// progress.Done (internal/progress/event.go):
//
//	runFailed     - the command is aborting due to an ERROR;
//	runInterrupted - the run was aborted by SIGINT/SIGTERM.
//
// Both feed the interactive renderer's clear-the-bar path: a bar must
// never complete to a misleading 100% on a run that did not finish
// (the stakeholder saw "files 0/1315, 0 B/21.4 GiB 100%" on a failed
// build; an interrupted run rendered the identical lie). The plain
// renderer is unaffected: its counts-and-percent lines were always
// honest about how far the run actually got (DESIGN_SPEC 9.x progress
// event model; internal/progress/event.go).

import (
	"context"
	"errors"

	"isopress/internal/report"
)

// runInterrupted reports whether a command's returned error is the
// INTERRUPTION path for the progress Done.Interrupted signal. Two
// error shapes reach a command's deferred Done on SIGINT/SIGTERM:
//
//   - report.ErrInterrupted (wrapped or not): copy, build and (since
//     T6.1) scan classify the interruption explicitly in finalize
//     (ctx.Err() != nil) and return an ErrInterrupted-wrapped result
//   - the exit-130 class of DESIGN_SPEC 10.1;
//   - context.Canceled (wrapped or not): clear, plan and report have
//     no interrupted-status decision of their own (see the
//     per-command comments at their bus.Finish sites), so a signal
//     mid-operation surfaces as a cancellation-derived error from the
//     in-flight walk / pool / catalog call. The run context is only
//     ever cancelled by the signal scaffolding (root.go's
//     armSecondSignalEscape) or by a test scripting that same
//     scaffolding, so a cancellation-derived result IS the
//     interruption here. root.go's execute maps both shapes to exit
//     130; this predicate mirrors that classification for rendering.
//
// context.DeadlineExceeded is deliberately NOT matched: a timeout is
// an ordinary failure (Failed), never an interruption.
func runInterrupted(err error) bool {
	return err != nil &&
		(errors.Is(err, report.ErrInterrupted) || errors.Is(err, context.Canceled))
}

// runFailed reports whether a command's returned error is an
// abort-due-to-error for the progress Done.Failed signal: any error
// runInterrupted does NOT classify. Defined as the exact complement of
// runInterrupted, so the two flags are mutually exclusive BY
// CONSTRUCTION - no command wiring can ever produce a Done with both
// Failed and Interrupted true.
func runFailed(err error) bool {
	return err != nil && !runInterrupted(err)
}
