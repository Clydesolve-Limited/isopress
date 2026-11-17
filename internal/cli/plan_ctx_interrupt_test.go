package cli

// T6.3 fix round: the plan command's interrupt shape now includes the
// balancer's OWN context abort - before this round the WFD placement
// loop ignored ctx entirely (TODO.md, closed here), so a cancelled run
// could only stop at the next blocking call. Two command-level pins,
// both deterministic (pre-cancelled run context; no sleeps, no
// signals):
//
//   - an interrupted plan leaves NO debris on a set that was fully
//     PLANNABLE. The existing matrix cell
//     (interrupt_shape_matrix_test.go) interrupts a plan over an
//     ALREADY-planned set - where the run would be a no-op anyway -
//     so this test deliberately plans a freshly scanned set, where a
//     successful run WOULD mint images rows and assignments;
//   - the error plan.Balance actually returns on cancellation
//     classifies as Interrupted (exit 130) through the REAL execute
//     wrapper, never as usage (exit 2) or catalog (exit 4), and the
//     shared runInterrupted/runFailed predicate pair reads it as an
//     interruption.
//
// Staging note on the debris claim: with a context cancelled at
// command start the pipeline stops at the first ctx-aware catalog
// read (PriorScanRuns), and the assertion below is the STRONGEST form
// of the contract - nothing at all was written. The Balance-stage half
// follows from runPlanReport's control flow (the write hook is called
// only after Balance returns nil, and Plan.Write opens the ONE
// transaction afterwards) plus the plan-package pins in
// balancer_fix_round_test.go: an abort inside Balance produces no ops
// and opens no transaction, so there is no debris to roll back.

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"isopress/internal/plan"
	"isopress/internal/report"
	"isopress/internal/units"
)

// TestPlanInterruptedBeforeBalanceLeavesPlannableSetPristine drives
// the REAL plan command through execute() with a pre-cancelled run
// context (the deterministic in-process projection of a ^C at command
// start) over a set that has real work waiting, and asserts the whole
// no-debris contract: exit 130, no stdout report, no images rows, no
// file/chunk/dir assignments, no runs rows.
func TestPlanInterruptedBeforeBalanceLeavesPlannableSetPristine(t *testing.T) {
	fixtureRoot := makeFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, fixtureRoot, cat, "S-CTXINT")
	runsBefore := countRunsRows(t, cat)

	// Work evidence: this set IS plannable - a successful run would
	// assign the active unassigned payload below.
	db := catOpen(t, cat)
	var unassignedFiles, unassignedDirs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM files
		WHERE set_id = ? AND status = 'active' AND superseded_by IS NULL AND image_id IS NULL`,
		"S-CTXINT").Scan(&unassignedFiles); err != nil {
		t.Fatalf("counting unassigned files: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM directories
		WHERE set_id = ? AND status = 'active' AND image_id IS NULL`,
		"S-CTXINT").Scan(&unassignedDirs); err != nil {
		t.Fatalf("counting unassigned dirs: %v", err)
	}
	if unassignedFiles == 0 || unassignedDirs == 0 {
		t.Fatalf("fixture is not plannable (%d unassigned files, %d dirs): the interrupted-plan cell needs real work",
			unassignedFiles, unassignedDirs)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the "signal", projected onto command start
	root := NewRootCmd()
	outBuf, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	code := execute(ctx, root, []string{
		"plan", "--catalog", cat, "--set-id", "S-CTXINT", "--size", planFixtureSize}, errBuf)

	if code != report.ExitInterrupted {
		t.Errorf("exit = %d, want 130 (an interrupted plan must be the Interrupted class)\nstderr: %s",
			code, errBuf.String())
	}
	stderr := errBuf.String()
	if !strings.Contains(stderr, "interrupted by signal") {
		t.Errorf("stderr lacks the interrupt class wording:\n%s", stderr)
	}
	// Not the usage class (exit 2): the run never fell into the
	// missing-set gate or any other input mistake.
	if strings.Contains(stderr, "no scanned set") || code == report.ExitUsage {
		t.Errorf("cancellation misclassified as usage (exit 2):\n%s", stderr)
	}
	if out := outBuf.String(); out != "" {
		t.Errorf("stdout must stay empty on an interrupted plan (FR-046), got: %q", out)
	}

	// --- NO DEBRIS ---
	var images, assignedFiles, assignedChunks, assignedDirs, planRuns int
	count := func(dst *int, query string, args ...any) {
		t.Helper()
		if err := db.QueryRow(query, args...).Scan(dst); err != nil {
			t.Fatalf("count %s: %v", query, err)
		}
	}
	count(&images, `SELECT COUNT(*) FROM images WHERE set_id = ?`, "S-CTXINT")
	count(&assignedFiles, `SELECT COUNT(*) FROM files WHERE set_id = ? AND image_id IS NOT NULL`, "S-CTXINT")
	count(&assignedChunks, `SELECT COUNT(*) FROM chunks WHERE set_id = ? AND image_id IS NOT NULL`, "S-CTXINT")
	count(&assignedDirs, `SELECT COUNT(*) FROM directories WHERE set_id = ? AND image_id IS NOT NULL`, "S-CTXINT")
	count(&planRuns, `SELECT COUNT(*) FROM runs WHERE set_id = ? AND kind = 'plan'`, "S-CTXINT")
	if images != 0 || assignedFiles != 0 || assignedChunks != 0 || assignedDirs != 0 {
		t.Errorf("interrupted plan wrote state: images=%d files=%d chunks=%d dirs=%d "+
			"(the assignment transaction opens only after a successful Balance)",
			images, assignedFiles, assignedChunks, assignedDirs)
	}
	if planRuns != 0 {
		t.Errorf("interrupted plan left %d 'plan' runs rows, want 0", planRuns)
	}
	if n := countRunsRows(t, cat); n != runsBefore {
		t.Errorf("runs rows = %d after the interrupted plan, want the %d fixture row(s)", n, runsBefore)
	}
}

// balanceCtxProbeStore is the minimal plan.Store whose reads IGNORE
// the context, so with a pre-cancelled context the ONLY place Balance
// can fail is its own context check: balErr below is exactly the error
// this fix introduced, not a SQL driver's cancellation message.
type balanceCtxProbeStore struct{}

func (balanceCtxProbeStore) ActiveFiles(context.Context) ([]plan.ActiveFile, error) { return nil, nil }

func (balanceCtxProbeStore) ChunkStates(context.Context) ([]plan.StoredChunk, error) { return nil, nil }

func (balanceCtxProbeStore) UnassignedDirs(context.Context) ([]plan.UnassignedDir, error) {
	return nil, nil
}

func (balanceCtxProbeStore) Destinations(context.Context) ([]plan.DestinationState, error) {
	return nil, nil
}

// TestPlanBalanceContextErrorClassifiesInterrupted pins the
// classification path end to end for the error plan.Balance really
// returns when the run context is cancelled: fatal-class raw (never
// usage/catalog), an interruption for the shared Done predicate, and
// 130 through the real execute wrapper with a cancelled run context -
// with the original cause preserved on stderr (T6.1 NB N1 shape).
func TestPlanBalanceContextErrorClassifiesInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, balErr := plan.Balance(ctx, balanceCtxProbeStore{}, units.Profile{
		Name: units.ProfileDisk, NominalBytes: 1_000_000_000, UsableBytes: 980_000_000,
	})
	if balErr == nil {
		t.Fatal("Balance ignored its cancelled context - the WFD-loop ctx debt is back (TODO.md)")
	}
	if !errors.Is(balErr, context.Canceled) {
		t.Fatalf("Balance error = %v, want it anchored on context.Canceled", balErr)
	}

	// (1) Raw classification: plain fatal class. report.Classify's own
	// table decides 2 (usage) / 4 (catalog) BEFORE the interrupt class,
	// so a ctx error that had been wrapped into either would come back
	// as 2/4 here - it must not.
	if code := report.Classify(balErr); code != report.ExitFatal {
		t.Errorf("report.Classify(Balance ctx error) = %d, want %d (fatal): the error must not be "+
			"classified as usage(2)/catalog(4)", code, report.ExitFatal)
	}

	// (2) The shared Done predicates: an interruption, and NEVER both
	// flags (runFailed is defined as !runInterrupted for a live error).
	if !runInterrupted(balErr) {
		t.Errorf("runInterrupted(Balance ctx error) = false, want true (cleared bar, Interrupted flag)")
	}
	if runFailed(balErr) {
		t.Errorf("runFailed(Balance ctx error) = true: the two Done flags must stay mutually exclusive")
	}

	// (3) The REAL wrapper with the same cancelled run context (what
	// ^C does): execute's interrupt override wraps the command's error
	// in report.ErrInterrupted -> exit 130, preserving the cause.
	errBuf := &bytes.Buffer{}
	root := &cobra.Command{
		Use:          "balctx",
		SilenceUsage: true,
		RunE:         func(*cobra.Command, []string) error { return balErr },
	}
	if code := execute(ctx, root, nil, errBuf); code != report.ExitInterrupted {
		t.Errorf("execute(Balance ctx error) = %d, want 130\nstderr: %s", code, errBuf.String())
	}
	stderr := errBuf.String()
	for _, want := range []string{"interrupted by signal", "balance aborted", "context canceled"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q (interrupt class + preserved cause):\n%s", want, stderr)
		}
	}
}
