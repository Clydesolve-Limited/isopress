package cli

// T6.2 ADDENDUM F1 (the reviewer-specified one-liner, SEPARATE from
// the T6.2 golden-schema scope): copy.go's and build.go's finalize
// used to SILENTLY DISCARD a FinishRunDetached failure on the
// interrupt path - the failure was joined into r.terminal, and the
// interrupted branch returned a fresh fmt.Errorf(report.ErrInterrupted,
// ...) that dropped r.terminal entirely, so a lost run stamp was
// invisible. scan.go is the reference shape (scan.go:457-459 computes
// finishErr and joins it into terminal; scan.go:500-505 returns
// errors.Join(the interrupted error, terminal)), which keeps the
// Interrupted class (exit 130) AND surfaces the original run error
// plus the lost stamp on stderr. copy.go and build.go now use that
// same join shape; these tests pin it per command.
//
// INJECTION DECISION (the brief's "test-only seam" question): NO
// production seam was added. finalize calls FinishRunDetached
// unconditionally, and the REAL catalog fails that call - fast and
// deterministically - for a run row that does not exist
// (catalog.FinishRun's "no such run_id" RowsAffected==0 path, the same
// failure shape interrupt_shapes_e2e_test.go:420 pins at the catalog
// tier). Driving finalize with a deliberately unknown run id therefore
// exercises the UNMODIFIED production failure path end to end, where a
// wrapped-catalog seam (or the 5-second wedged-lock e2e of
// TestInterruptedScanFinalizeUnderWedgedCatalog) would have been the
// alternatives. The runners are constructed white-box (same package)
// with a pre-cancelled context - the deterministic in-process
// projection of a signal landing at finalize time, no sleeps.
//
// Each test runs the finalize result through the REAL execute wrapper,
// so what is pinned is what a user sees: stderr carries BOTH the
// interrupted verdict and the lost-stamp cause, the report still lands
// on stdout (FR-046), and the exit code stays 130.

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"isopress/internal/catalog"
	"isopress/internal/report"
)

// finishFailureCause is the FinishRunDetached failure text the tests
// expect on stderr: FinishRunDetached wraps catalog.FinishRun, which
// reports the unknown row as "no such run_id".
const finishFailureCause = "no such run_id"

// runFinalizeProbe builds a one-command probe root whose RunE returns
// r's finalize result (the caller supplies the already-constructed
// runner with cmd pointing at the probe), executes it through the REAL
// wrapper, and returns the exit code plus both captured streams.
func runFinalizeProbe(t *testing.T, finalize func(cmd *cobra.Command) error) (code int, stdout, stderr string) {
	t.Helper()
	probe := &cobra.Command{
		Use:          "f1-probe",
		SilenceUsage: true,
		RunE:         func(cmd *cobra.Command, args []string) error { return finalize(cmd) },
	}
	outBuf := &bytes.Buffer{}
	probe.SetOut(outBuf)
	errBuf := &bytes.Buffer{}
	code = execute(context.Background(), probe, nil, errBuf)
	return code, outBuf.String(), errBuf.String()
}

// TestCopyInterruptedFinalizeSurfacesFinishRunFailure is the F1 pin
// for copy.go: on the interrupted path a FinishRunDetached failure is
// SURFACED next to the interrupted verdict (scan's join shape) instead
// of being dropped, while the exit code stays 130.
func TestCopyInterruptedFinalizeSurfacesFinishRunFailure(t *testing.T) {
	cat := createCatalog(t)
	db, err := catalog.Open(cat, catalog.OpenReadWrite)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	r := &copyRunner{
		db:      db,
		setID:   "S-F1COPY",
		destAbs: t.TempDir(),
		// The deliberate injection: no runs row with this id exists,
		// so FinishRunDetached fails through its REAL error path (the
		// production failure a wedged catalog produces after 5s).
		runID: 999_999,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the deterministic "signal landed at finalize time" projection

	code, stdout, stderr := runFinalizeProbe(t, func(cmd *cobra.Command) error {
		r.cmd = cmd
		var summary string
		return r.finalize(ctx, &summary, nil)
	})

	if code != report.ExitInterrupted {
		t.Errorf("exit = %d, want 130 (the join must keep the Interrupted class)\nstderr: %s", code, stderr)
	}
	// BOTH causes on stderr (the F1 contract): the interrupted verdict
	// and the lost stamp.
	if !strings.Contains(stderr, "copy run 999999 interrupted") {
		t.Errorf("stderr lacks the interrupted verdict:\n%s", stderr)
	}
	if !strings.Contains(stderr, finishFailureCause) {
		t.Errorf("stderr silently dropped the FinishRunDetached failure (the F1 defect):\n%s", stderr)
	}
	// The report still lands on stdout (FR-046: the result is the
	// stdout artifact even on the abort path).
	if !strings.Contains(stdout, "copy: set S-F1COPY") {
		t.Errorf("stdout lacks the copy report:\n%s", stdout)
	}
}

// TestBuildInterruptedFinalizeSurfacesFinishRunFailure is the F1 pin
// for build.go, identical contract to the copy test above.
func TestBuildInterruptedFinalizeSurfacesFinishRunFailure(t *testing.T) {
	cat := createCatalog(t)
	db, err := catalog.Open(cat, catalog.OpenReadWrite)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	r := &buildRunner{
		db:      db,
		setID:   "S-F1BUILD",
		destAbs: t.TempDir(),
		runID:   999_999, // see the copy test: no such run row, honest failure
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	code, stdout, stderr := runFinalizeProbe(t, func(cmd *cobra.Command) error {
		r.cmd = cmd
		var summary string
		return r.finalize(ctx, &summary, nil)
	})

	if code != report.ExitInterrupted {
		t.Errorf("exit = %d, want 130 (the join must keep the Interrupted class)\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "build run 999999 interrupted") {
		t.Errorf("stderr lacks the interrupted verdict:\n%s", stderr)
	}
	if !strings.Contains(stderr, finishFailureCause) {
		t.Errorf("stderr silently dropped the FinishRunDetached failure (the F1 defect):\n%s", stderr)
	}
	if !strings.Contains(stdout, "build: set S-F1BUILD") {
		t.Errorf("stdout lacks the build report:\n%s", stdout)
	}
}
