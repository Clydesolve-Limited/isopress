package cli

// T6.1 adversarial interrupt-shape matrix (DESIGN_SPEC 10.1
// "Interrupted: SIGINT or SIGTERM ... exits 130"), the cells the
// per-command suites do NOT already cover end to end:
//
//   - every NON-scan/copy/build command interrupted with a
//     signal-cancelled run context exits 130 AND leaves NO run-row
//     debris AND changes no state (plan/clear commit transactionally
//     and never mint a 'running' row - verified here structurally AND
//     behaviorally; the read-only commands never write at all);
//   - a signal AFTER the operation completed (the window between the
//     command's successful return and the wrapper's exit) still exits
//     130, but the catalog keeps the COMPLETED verdict (row 'ok' - the
//     run did finish; the wrapper's 130 is a process-level verdict)
//     and the full report still lands on stdout.
//
// The signal shapes that need a real child process (SIGINT/SIGTERM
// mid-hash on scan, mid-write on copy, mid-mastering on build, the
// detached-finalize bound) live in their own suites:
// scan_abort_e2e_test.go, copy_test.go's interrupted-run closure, and
// interrupt_shapes_e2e_test.go (build + the 5s bound).

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"isopress/internal/report"
)

// countRuns returns the number of rows in runs (any kind, any status).
func countRunsRows(t *testing.T, catPath string) int {
	t.Helper()
	db := catOpen(t, catPath)
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&n); err != nil {
		t.Fatalf("counting runs rows: %v", err)
	}
	return n
}

// TestInterruptedNonRunCommandsLeaveNoDebris drives the REAL command
// tree with a PRE-CANCELLED run context (the deterministic in-process
// projection of a signal landing at command start) for every command
// that does not own a run row of its own, on fixtures where the
// command WOULD otherwise do real work. The contract per command:
// exit 130 (10.1), stderr names the interrupt class, NO runs row
// ('running' debris is exactly the T0.5/T1.6 gap scan/copy/build had
// to close - plan/clear must never need that closure because their
// bookkeeping is transactional), and no committed state change.
func TestInterruptedNonRunCommandsLeaveNoDebris(t *testing.T) {
	preCancelled := func() context.Context {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		return ctx
	}
	run := func(t *testing.T, ctx context.Context, args ...string) (string, int) {
		t.Helper()
		root := NewRootCmd()
		outBuf := &bytes.Buffer{}
		root.SetOut(outBuf)
		errBuf := &bytes.Buffer{}
		code := execute(ctx, root, args, errBuf)
		if code != report.ExitInterrupted {
			t.Errorf("%v: exit = %d, want 130\nstderr: %s", args, code, errBuf.String())
		}
		if !strings.Contains(errBuf.String(), "interrupted by signal") {
			t.Errorf("%v: stderr lacks the interrupt class wording (every cell must NAME its cause):\n%s", args, errBuf.String())
		}
		return outBuf.String(), code
	}

	// A scanned+planned set every mutating command would act on. The
	// fixture's own scan mints ONE 'ok' run row - snapshot it, so the
	// sweep below proves the eight interrupted commands minted NONE.
	root := makeFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-IDEBRIS")
	if _, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", "S-IDEBRIS", "--size", planFixtureSize); code != report.ExitOK {
		t.Fatalf("plan fixture exit = %d, want 0\nstderr: %s", code, stderr)
	}
	fixtureRuns := countRunsRows(t, cat)

	// --- plan: no runs row EVER (structural: plan has no Start*Run),
	// and the interrupted plan commits NO assignment (the transaction
	// never began - images rows stay exactly as they were).
	var imagesBefore int
	if err := catOpen(t, cat).QueryRow(
		`SELECT COUNT(*) FROM images WHERE set_id = 'S-IDEBRIS'`).Scan(&imagesBefore); err != nil {
		t.Fatal(err)
	}
	if imagesBefore == 0 {
		t.Fatal("plan fixture left no images rows; the interrupted-plan cell needs a plannable set")
	}
	run(t, preCancelled(),
		"plan", "--catalog", cat, "--set-id", "S-IDEBRIS", "--size", planFixtureSize)
	var imagesAfterInt int
	if err := catOpen(t, cat).QueryRow(
		`SELECT COUNT(*) FROM images WHERE set_id = 'S-IDEBRIS'`).Scan(&imagesAfterInt); err != nil {
		t.Fatal(err)
	}
	if imagesAfterInt != imagesBefore {
		t.Errorf("interrupted plan changed the images rows (%d -> %d); the assignment transaction must not commit partially",
			imagesBefore, imagesAfterInt)
	}

	// --- clear: the runs row is minted INSIDE the clear transaction
	// with terminal status (clearstore.go) - an interrupted clear
	// therefore leaves NO run row AND the assignments intact.
	imagesPreClear := imagesBefore
	run(t, preCancelled(),
		"clear-assignments", "--catalog", cat, "--set-id", "S-IDEBRIS")
	var imagesPostClear, clearRuns int
	db := catOpen(t, cat)
	if err := db.QueryRow(`SELECT COUNT(*) FROM images WHERE set_id = 'S-IDEBRIS'`).Scan(&imagesPostClear); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM runs WHERE kind = 'clear'`).Scan(&clearRuns); err != nil {
		t.Fatal(err)
	}
	if imagesPostClear != imagesPreClear {
		t.Errorf("interrupted clear changed the images rows (%d -> %d); the transaction must not commit partially",
			imagesPreClear, imagesPostClear)
	}
	if clearRuns != 0 {
		t.Errorf("interrupted clear left %d 'clear' run rows, want 0 (the runs row commits atomically with the clear)", clearRuns)
	}

	// --- copy / build: they DO own run rows - the interrupted shape
	// must leave none ('running' debris is the gap scan/copy/build
	// closed; these cells pin the same closure for the pre-finalize
	// window). A planned set is present, so without the interrupt the
	// run WOULD be minted.
	run(t, preCancelled(),
		"copy", "--catalog", cat, "--set-id", "S-IDEBRIS", "--dest", filepath.Join(t.TempDir(), "dest"))
	run(t, preCancelled(),
		"build", "--catalog", cat, "--set-id", "S-IDEBRIS", "--dest", filepath.Join(t.TempDir(), "dest"))
	// --- report / validate / verify-media / restore: read-only opens
	// (DESIGN_SPEC 2.1 Option B) - 130 and nothing else. restore's
	// media-path hash fill is the one write-shaped read command; with
	// the context cancelled it must not have run either (no stored
	// chunk hashes materialized).
	var hashesBefore int
	if err := db.QueryRow(`SELECT COUNT(*) FROM chunks WHERE hash IS NOT NULL`).Scan(&hashesBefore); err != nil {
		t.Fatal(err)
	}
	run(t, preCancelled(),
		"report", "--catalog", cat, "--set-id", "S-IDEBRIS", "--size", planFixtureSize)
	run(t, preCancelled(),
		"validate", "--catalog", cat, "--set-id", "S-IDEBRIS")
	run(t, preCancelled(),
		"verify-media", "--catalog", cat, "--set-id", "S-IDEBRIS")
	run(t, preCancelled(),
		"restore", "--catalog", cat, "--set-id", "S-IDEBRIS",
		"--target", filepath.Join(t.TempDir(), "out"))

	// THE SWEEP ASSERTION: not ONE runs row was ADDED by any of the
	// eight interrupted commands above, in any status - and nothing
	// was stranded 'running' anywhere.
	if n := countRunsRows(t, cat); n != fixtureRuns {
		t.Errorf("runs rows = %d after the interrupted sweep, want the %d fixture row(s) only; an interrupted command must mint no run row",
			n, fixtureRuns)
	}
	var running int
	if err := catOpen(t, cat).QueryRow(`SELECT COUNT(*) FROM runs WHERE status = 'running'`).Scan(&running); err != nil {
		t.Fatal(err)
	}
	if running != 0 {
		t.Errorf("%d runs rows stranded 'running' after the interrupted sweep", running)
	}
	var hashesAfter int
	if err := catOpen(t, cat).QueryRow(`SELECT COUNT(*) FROM chunks WHERE hash IS NOT NULL`).Scan(&hashesAfter); err != nil {
		t.Fatal(err)
	}
	if hashesAfter != hashesBefore {
		t.Errorf("interrupted restore materialized chunk hashes (%d -> %d); a cancelled run must write nothing",
			hashesBefore, hashesAfter)
	}
}

// TestSignalAfterCompletionKeepsCompletedRunAndFullReport pins the
// shipped semantics of the LAST interrupt window: the signal arriving
// after the command's work and its run-row finalization COMPLETED but
// before the process exits (in-process projection: the real command
// tree runs to success, then the context is cancelled from a cobra
// PostRun hook - exactly the window between RunE's return and the
// wrapper's ctx.Err() check). The wrapper still exits 130 (it cannot
// retroactively distinguish the late signal), the catalog keeps the
// COMPLETED verdict - a finished run is never re-labeled
// 'interrupted' - and the full report already printed on stdout.
func TestSignalAfterCompletionKeepsCompletedRunAndFullReport(t *testing.T) {
	t.Run("scan", func(t *testing.T) {
		fixtureRoot := makeFixture(t)
		cat := createCatalog(t)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		root := NewRootCmd()
		root.PersistentPostRun = func(*cobra.Command, []string) { cancel() }
		outBuf := &bytes.Buffer{}
		root.SetOut(outBuf)
		errBuf := &bytes.Buffer{}
		code := execute(ctx, root, []string{"scan", "--catalog", cat, "--set-id", "S-LATE", fixtureRoot}, errBuf)

		if code != report.ExitInterrupted {
			t.Errorf("exit = %d, want 130 (late signal is still an interrupt at the wrapper)\nstderr: %s", code, errBuf.String())
		}
		if !strings.Contains(errBuf.String(), "interrupted by signal") {
			t.Errorf("stderr lacks the interrupt class wording:\n%s", errBuf.String())
		}
		// The FULL completed report printed on stdout (FR-046: the
		// result is the stdout artifact even on the interrupt path -
		// here the run finished, so the completed-run report stands).
		if !strings.Contains(outBuf.String(), "set: S-LATE") {
			t.Errorf("stdout lacks the completed-run report after the late signal:\n%s", outBuf.String())
		}
		// The catalog keeps the completed verdict: a run that finished
		// is 'ok', never re-labeled 'interrupted'.
		db := catOpen(t, cat)
		var status string
		if err := db.QueryRow(`SELECT status FROM runs WHERE set_id = 'S-LATE'`).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "ok" {
			t.Errorf("run status = %q, want 'ok' (a completed run keeps its verdict under a late signal)", status)
		}
	})

	t.Run("copy", func(t *testing.T) {
		f := newCopyCrashFixture(t)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		root := NewRootCmd()
		root.PersistentPostRun = func(*cobra.Command, []string) { cancel() }
		outBuf := &bytes.Buffer{}
		root.SetOut(outBuf)
		errBuf := &bytes.Buffer{}
		code := execute(ctx, root, []string{
			"copy", "--catalog", f.cat, "--set-id", f.setID, "--dest", f.dest}, errBuf)

		if code != report.ExitInterrupted {
			t.Errorf("exit = %d, want 130\nstderr: %s", code, errBuf.String())
		}
		// The full report printed on stdout with the completed status.
		if !strings.Contains(outBuf.String(), "copy: set "+f.setID) {
			t.Errorf("stdout lacks the completed copy report:\n%s", outBuf.String())
		}
		if !strings.Contains(outBuf.String(), "status: ok") {
			t.Errorf("stdout report lacks the completed 'ok' status:\n%s", outBuf.String())
		}
		db := catOpen(t, f.cat)
		var status string
		if err := db.QueryRow(`SELECT status FROM runs WHERE kind = 'copy'`).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "ok" {
			t.Errorf("copy run status = %q, want 'ok' (completed before the late signal)", status)
		}
		// Everything is marked: the late signal did not unmark or
		// un-complete anything.
		for rel, copied := range copiedFlags(t, f.cat, f.setID) {
			if !copied {
				t.Errorf("file %s unmarked after the late signal; the completed run's marks must stand", rel)
			}
		}
	})
}
