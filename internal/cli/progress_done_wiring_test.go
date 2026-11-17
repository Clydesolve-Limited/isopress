package cli

// The progress Done wiring of the 2026-10-20 cosmetic-honesty fix
// (progress_done.go, plus the interrupted-half QA fold-in of the same
// day): EVERY command's abort path must set Done.Failed via runFailed
// AND Done.Interrupted via runInterrupted, and the two predicates must
// never classify the same error both ways (Done.Failed and
// Done.Interrupted are DISTINCT, MUTUALLY EXCLUSIVE signals -
// event.go; an interrupted OR failed run clears the interactive bar
// rather than rendering a completed 100%, and only a successful run
// completes it).
//
// The per-command pin is AST-level (the repo's established structural-
// assertion pattern, cf. the deps tests): each command file in
// doneWiringFiles must contain exactly one bus.Finish(progress.Done{...})
// call whose Failed field is a runFailed(...) call and whose
// Interrupted field is a runInterrupted(...) call. A command that
// regresses to a bare progress.Done{Summary: ...} - or drops one of the
// two flags - fails here even if its happy path still looks green.

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"isopress/internal/report"
)

// TestRunFailedAndRunInterruptedSemantics pins the classification
// itself: success is never an abort, an ordinary error is a failure
// and never an interruption, and BOTH abort shapes (report.ErrInterrupted
// and the cancellation-derived context.Canceled, each bare and wrapped)
// are interruptions and never failures - so a Done can carry the flags
// truthfully and never both.
func TestRunFailedAndRunInterruptedSemantics(t *testing.T) {
	if runFailed(nil) {
		t.Error("runFailed(nil) = true; a successful run must not signal Failed")
	}
	if runInterrupted(nil) {
		t.Error("runInterrupted(nil) = true; a successful run must not signal Interrupted")
	}
	ordinary := errors.New("database is locked")
	if !runFailed(ordinary) {
		t.Error("runFailed(ordinary error) = false; an abort-due-to-error must signal Failed")
	}
	if runInterrupted(ordinary) {
		t.Error("runInterrupted(ordinary error) = true; an ordinary failure is not an interruption")
	}
	// Every wrapping shape the commands produce.
	wrapped := fmt.Errorf("build: run 7 aborted: %w", ordinary)
	if !runFailed(wrapped) {
		t.Error("runFailed(wrapped ordinary error) = false")
	}
	// The interruption paths, in every shape the commands / root.go
	// produce them: report.ErrInterrupted (copy/build finalize) and
	// context.Canceled (scan/clear/plan/report cancellation-derived
	// results), bare and message-carrying.
	interruptedShapes := []error{
		report.ErrInterrupted,
		fmt.Errorf("%w: signal received, aborting", report.ErrInterrupted),
		fmt.Errorf("copy: run 3 interrupted: %w", report.ErrInterrupted),
		context.Canceled,
		fmt.Errorf("scan(hash): aborted: %w", context.Canceled),
		fmt.Errorf("ingest: hash of huge.bin aborted: %w", context.Canceled),
	}
	for _, err := range interruptedShapes {
		if runFailed(err) {
			t.Errorf("runFailed(%v) = true; the interruption path must NOT set Failed "+
				"(it renders as the cleared bar plus the interrupted summary, event.go)", err)
		}
		if !runInterrupted(err) {
			t.Errorf("runInterrupted(%v) = false; the interruption path must set Done.Interrupted", err)
		}
	}
	// A timeout is an ordinary failure, never an interruption: only
	// Cancellation (the signal scaffolding) matches, not Deadline.
	if runInterrupted(fmt.Errorf("catalog: %w", context.DeadlineExceeded)) {
		t.Error("runInterrupted(DeadlineExceeded) = true; a timeout is an abort-due-to-error, not an interruption")
	}
	if !runFailed(fmt.Errorf("catalog: %w", context.DeadlineExceeded)) {
		t.Error("runFailed(DeadlineExceeded) = false; a timeout must signal Failed")
	}
	// THE MUTUAL EXCLUSION: for EVERY error shape, the two predicates
	// can never both fire - runFailed is defined as runInterrupted's
	// complement - so no command wiring can produce a Done with both
	// flags true.
	for _, err := range append([]error{nil, ordinary, wrapped}, interruptedShapes...) {
		if runFailed(err) && runInterrupted(err) {
			t.Errorf("mutual exclusion violated: runFailed(%v) && runInterrupted(%v) both true", err, err)
		}
	}
}

// doneWiringFiles are the commands that own a progress bus and a
// deferred Done (scan, copy, build, clear, plan/report - the original
// five; T5.1's verify-media and T5.2's validate joined the pin when
// they graduated to real commands, since both wired the same
// Finish/wait pair with both flags).
var doneWiringFiles = []string{
	"scan.go", "copy.go", "build.go", "clear.go", "planreport.go",
	"verifymedia.go", "validate.go",
}

// TestEveryCommandDoneCarriesBothFlags is the structural pin: none of
// the five commands may miss the Failed OR the Interrupted wiring on
// its Done event (a dropped flag would let that abort class render a
// completed 100% bar again).
func TestEveryCommandDoneCarriesBothFlags(t *testing.T) {
	fset := token.NewFileSet()
	for _, name := range doneWiringFiles {
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		doneLits := 0
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			// bus.Finish(progress.Done{...})
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Finish" {
				return true
			}
			recv, ok := sel.X.(*ast.Ident)
			if !ok || recv.Name != "bus" || len(call.Args) != 1 {
				return true
			}
			lit, ok := call.Args[0].(*ast.CompositeLit)
			if !ok {
				return true
			}
			lt, ok := lit.Type.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := lt.X.(*ast.Ident)
			if !ok || pkg.Name != "progress" || lt.Sel.Name != "Done" {
				return true
			}
			doneLits++
			// Both flags must be present and must be calls to the
			// designated helper (progress_done.go) - never a literal,
			// never a different callee: the mutual exclusion lives in
			// the helpers, so the wiring must go through them.
			want := map[string]string{"Failed": "runFailed", "Interrupted": "runInterrupted"}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				callee, hasCallee := want[key.Name]
				if !hasCallee {
					continue
				}
				delete(want, key.Name)
				fn, isCall := kv.Value.(*ast.CallExpr)
				if !isCall {
					t.Errorf("%s: Done.%s is not a call: %v", name, key.Name, kv.Value)
					continue
				}
				fnIdent, ok := fn.Fun.(*ast.Ident)
				if !ok || fnIdent.Name != callee {
					t.Errorf("%s: Done.%s = %v, want %s(runErr) (progress_done.go)", name, key.Name, fn.Fun, callee)
				}
			}
			for missing := range want {
				why := map[string]string{"Failed": "failed", "Interrupted": "interrupted"}[missing]
				t.Errorf("%s: bus.Finish(progress.Done{...}) carries no %s field; "+
					"a %s run would render a misleading 100%% bar (2026-10-20 fix + fold-in)",
					name, missing, why)
			}
			return true
		})
		if doneLits != 1 {
			t.Errorf("%s: found %d bus.Finish(progress.Done{...}) calls, want exactly 1 "+
				"(the deferred Finish/wait pair)", name, doneLits)
		}
	}
}
