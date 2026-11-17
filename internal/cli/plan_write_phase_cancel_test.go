package cli

// T6.3 fix round: the WRITE phase of an interrupted plan (cancel
// landing after Balance succeeded, inside the ONE assignment
// transaction) and the precedence of a cancellation against errors
// the plan would raise ANYWAY. The Balance-phase and command-start
// interrupts are pinned in plan_ctx_interrupt_test.go and
// interrupt_shape_matrix_test.go; this file closes the two remaining
// windows honestly - whatever the code does is what gets pinned:
//
//  1. cancel MID-transaction (after the first real statement): the
//     real sqlite statement layer refuses the cancelled context, Plan.Write
//     rolls back - ZERO debris - and the run classifies as
//     Interrupted (130) through the real classification chain;
//  2. cancel at TRANSACTION ENTRY (between Balance and BEGIN): the
//     connection acquisition fails and BeginPlanTx classifies that
//     failure as a KindOpen CatalogError - report.Classify checks the
//     catalog class BEFORE the interrupt class, so this narrow window
//     exits 4, NOT 130 (with the interrupted progress flags, since
//     CatalogError unwraps to the context cause). Pinned as the
//     honest current behavior - zero debris either way;
//  3. cancel vs a failing input: usage errors are classified BEFORE
//     the interrupt class and are detected before any context-aware
//     call, so they win (exit 2); a never-scanned set id is detected
//     by a CONTEXT-AWARE read, so a cancelled run surfaces the
//     cancellation first (exit 130) instead of the usage gate; a
//     missing catalog file (no context involved) always wins (4).

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"isopress/internal/catalog"
	"isopress/internal/plan"
	"isopress/internal/report"
	"isopress/internal/units"
)

// cancelAfterFirstStmt wraps a real PlanTx's store and fires the run
// cancel once the FIRST statement has executed for real - the
// deterministic projection of a signal landing mid-transaction (no
// goroutines, no sleeps: the cancel happens inside a statement call
// Plan.Write itself invokes). Later statements then run with a
// cancelled context, which database/sql's conn layer refuses (the
// driver-model probe this test makes: if it ever stopped refusing,
// the debris assertions below would catch the commit).
type cancelAfterFirstStmt struct {
	inner  plan.AssignStore
	cancel context.CancelFunc
	fired  bool
}

func (c *cancelAfterFirstStmt) arm() {
	if !c.fired {
		c.fired = true
		c.cancel()
	}
}

func (c *cancelAfterFirstStmt) InsertImage(ctx context.Context, img plan.ImageSpec) error {
	err := c.inner.InsertImage(ctx, img)
	c.arm()
	return err
}

func (c *cancelAfterFirstStmt) EnsureChunkRows(ctx context.Context, fileID int64, specs []plan.ChunkSpec) error {
	err := c.inner.EnsureChunkRows(ctx, fileID, specs)
	c.arm()
	return err
}

func (c *cancelAfterFirstStmt) ReplaceAllChunks(ctx context.Context, fileID int64, specs []plan.ChunkSpec) (plan.RechunkOutcome, error) {
	out, err := c.inner.ReplaceAllChunks(ctx, fileID, specs)
	c.arm()
	return out, err
}

func (c *cancelAfterFirstStmt) DropChunkState(ctx context.Context, fileID int64) (int, error) {
	n, err := c.inner.DropChunkState(ctx, fileID)
	c.arm()
	return n, err
}

func (c *cancelAfterFirstStmt) StoreChunkSize(ctx context.Context, fileID, chunkSize int64, chunkCount int) error {
	err := c.inner.StoreChunkSize(ctx, fileID, chunkSize, chunkCount)
	c.arm()
	return err
}

func (c *cancelAfterFirstStmt) AssignFile(ctx context.Context, fileID, imageID int64) error {
	err := c.inner.AssignFile(ctx, fileID, imageID)
	c.arm()
	return err
}

func (c *cancelAfterFirstStmt) AssignChunk(ctx context.Context, fileID int64, chunkIndex int, imageID int64) error {
	err := c.inner.AssignChunk(ctx, fileID, chunkIndex, imageID)
	c.arm()
	return err
}

func (c *cancelAfterFirstStmt) AssignDir(ctx context.Context, dirID, imageID int64) error {
	err := c.inner.AssignDir(ctx, dirID, imageID)
	c.arm()
	return err
}

// planDebris is the zero-debris assertion set of the interrupted-plan
// contract: every assignment-bearing column plus the images and runs
// tables, scoped to one set.
type planDebris struct {
	images, assignedFiles, assignedChunks, assignedDirs, planRuns int
}

func planDebrisOf(t *testing.T, cat, setID string) planDebris {
	t.Helper()
	db := catOpen(t, cat)
	var d planDebris
	count := func(dst *int, query string) {
		t.Helper()
		if err := db.QueryRow(query, setID).Scan(dst); err != nil {
			t.Fatalf("count %s: %v", query, err)
		}
	}
	count(&d.images, `SELECT COUNT(*) FROM images WHERE set_id = ?`)
	count(&d.assignedFiles, `SELECT COUNT(*) FROM files WHERE set_id = ? AND image_id IS NOT NULL`)
	count(&d.assignedChunks, `SELECT COUNT(*) FROM chunks WHERE set_id = ? AND image_id IS NOT NULL`)
	count(&d.assignedDirs, `SELECT COUNT(*) FROM directories WHERE set_id = ? AND image_id IS NOT NULL`)
	count(&d.planRuns, `SELECT COUNT(*) FROM runs WHERE set_id = ? AND kind = 'plan'`)
	return d
}

// setupWritePhaseFixture scans the standard fixture into a fresh
// catalog under setID and returns the catalog path. The set is
// plannable (unassigned files + an empty dir), so a successful plan
// WOULD write assignments - which is what makes the debris
// assertions meaningful.
func setupWritePhaseFixture(t *testing.T, setID string) string {
	t.Helper()
	root := makeFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, setID)
	pre := planDebrisOf(t, cat, setID)
	if pre.images != 0 || pre.assignedFiles != 0 || pre.assignedDirs != 0 || pre.planRuns != 0 {
		t.Fatalf("fixture not clean before the write-phase test: %+v", pre)
	}
	return cat
}

// TestPlanCancelMidWriteRealCatalogRollsBackAndExits130: full
// pipeline (Balance over the real catalog, then the real BeginPlanTx
// transaction) with the signal landing AFTER the first real INSERT.
// Pins: the statement error keeps the context cause, the transaction
// ROLLS BACK (zero rows anywhere), report.Classify sees the plain
// fatal class (never usage/catalog), the shared Done predicates read
// an interruption, and the real execute wrapper maps it to 130.
func TestPlanCancelMidWriteRealCatalogRollsBackAndExits130(t *testing.T) {
	cat := setupWritePhaseFixture(t, "S-MIDWRITE")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	opts := &rootOptions{catalog: cat, setID: "S-MIDWRITE"}
	po := planOptions{size: planFixtureSize, mediaProfile: units.DefaultProfile}
	cmd := &cobra.Command{Use: "plan"}
	outBuf, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(outBuf)
	cmd.SetErr(errBuf)

	hook := func(hctx context.Context, db *catalog.DB, setID string, p *plan.Plan) error {
		return p.Write(hctx, func(wctx context.Context) (*plan.AssignTx, error) {
			// Real BEGIN on a still-live context, then arm the
			// cancel-after-first-statement wrapper: statement 1
			// (INSERT INTO images) lands for real, the signal fires,
			// every later statement meets a cancelled context.
			tx, err := planTxFactory(db, setID)(wctx)
			if err != nil {
				return nil, err
			}
			tx.AssignStore = &cancelAfterFirstStmt{inner: tx.AssignStore, cancel: cancel}
			return tx, nil
		})
	}

	runErr := runPlanReport(ctx, cmd, opts, po, modePlan, hook)

	// --- the write failed on the cancelled context ---
	if runErr == nil {
		t.Fatal("cancelled write-phase pipeline returned nil - the transaction should not have completed " +
			"(if this fails, the statement layer stopped honoring cancelled contexts)")
	}
	if !errors.Is(runErr, context.Canceled) {
		t.Errorf("write-phase error = %v, want the context cause preserved (errors.Is context.Canceled)", runErr)
	}
	if outBuf.Len() != 0 {
		t.Errorf("no report may render on a failed write, got: %q", outBuf.String())
	}

	// --- ZERO DEBRIS: rollback really happened on sqlite ---
	post := planDebrisOf(t, cat, "S-MIDWRITE")
	if post != (planDebris{}) {
		t.Errorf("cancelled write left debris: %+v (the single tx must roll back completely)", post)
	}

	// --- classification chain (the fix round's contract) ---
	if code := report.Classify(runErr); code != report.ExitFatal {
		t.Errorf("Classify(write-phase ctx error) = %d, want %d (fatal): cancellation must not be "+
			"reclassified as usage(2)/catalog(4)", code, report.ExitFatal)
	}
	if !runInterrupted(runErr) {
		t.Errorf("runInterrupted(write-phase ctx error) = false, want true")
	}
	if runFailed(runErr) {
		t.Errorf("runFailed(write-phase ctx error) = true: the Done flags must stay mutually exclusive")
	}

	// --- the REAL execute wrapper with the cancelled run context ---
	errBuf2 := &bytes.Buffer{}
	root := &cobra.Command{
		Use:          "planwrite",
		SilenceUsage: true,
		RunE:         func(*cobra.Command, []string) error { return runErr },
	}
	if code := execute(ctx, root, nil, errBuf2); code != report.ExitInterrupted {
		t.Errorf("execute(write-phase ctx error) = %d, want 130\nstderr: %s", code, errBuf2.String())
	}
	for _, want := range []string{"interrupted by signal", "context canceled"} {
		if !strings.Contains(errBuf2.String(), want) {
			t.Errorf("stderr lacks %q:\n%s", want, errBuf2.String())
		}
	}
}

// TestPlanCancelAtWriteEntryClassifiesCatalogNotInterrupted: the
// narrow window between Balance's success and the first statement -
// the cancel is already done when BEGIN tries to acquire its
// connection. BeginPlanTx wraps that failure in a KindOpen
// CatalogError (catalog planstore doc: connection-acquisition failure
// = KindOpen), and report.Classify checks the catalog class BEFORE
// ErrInterrupted, so the run exits 4 - not 130 - even through
// execute's interrupt wrap (errors.As still finds the CatalogError).
// The progress Done flags, keyed on errors.Is(context.Canceled)
// through CatalogError's Unwrap, DO read Interrupted - the honest
// current shape, pinned here rather than smoothed over. Zero debris
// either way (no tx was ever opened).
func TestPlanCancelAtWriteEntryClassifiesCatalogNotInterrupted(t *testing.T) {
	cat := setupWritePhaseFixture(t, "S-WENTRY")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	opts := &rootOptions{catalog: cat, setID: "S-WENTRY"}
	po := planOptions{size: planFixtureSize, mediaProfile: units.DefaultProfile}
	cmd := &cobra.Command{Use: "plan"}
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})

	hook := func(hctx context.Context, db *catalog.DB, setID string, p *plan.Plan) error {
		cancel() // signal between Balance and BEGIN
		return writeAssignments(hctx, db, setID, p)
	}

	runErr := runPlanReport(ctx, cmd, opts, po, modePlan, hook)
	if runErr == nil {
		t.Fatal("pipeline with a cancel before BEGIN returned nil")
	}
	if !errors.Is(runErr, context.Canceled) {
		t.Fatalf("entry-window error = %v, want the context cause preserved through the CatalogError wrap", runErr)
	}
	post := planDebrisOf(t, cat, "S-WENTRY")
	if post != (planDebris{}) {
		t.Errorf("entry-window cancel left debris: %+v (no tx may have opened)", post)
	}

	// The honest classification: catalog class wins over interrupt.
	if code := report.Classify(runErr); code != report.ExitCatalog {
		t.Errorf("Classify(entry-window error) = %d, want %d (catalog) - BeginPlanTx's KindOpen wrap decides; "+
			"if this changed to fatal/130 the window now classifies as interrupted, update this pin",
			code, report.ExitCatalog)
	}
	if !runInterrupted(runErr) {
		t.Errorf("runInterrupted = false: the CatalogError unwraps to the context cause and must read Interrupted")
	}
	errBuf := &bytes.Buffer{}
	root := &cobra.Command{Use: "planwriteentry", SilenceUsage: true,
		RunE: func(*cobra.Command, []string) error { return runErr }}
	if code := execute(ctx, root, nil, errBuf); code != report.ExitCatalog {
		t.Errorf("execute(entry-window error) = %d, want 4 (catalog class precedes the interrupt wrap)\nstderr: %s",
			code, errBuf.String())
	}
}

// TestPlanCancelVersusFailingInputPrecedence: both a cancel AND an
// input problem on the same run - which wins? Pinned per window:
// input mistakes resolved BEFORE any context-aware call keep their
// class (usage 2 / catalog 4); input problems detected BY a
// context-aware read lose to the cancellation (130) because the read
// fails on the cancelled context before the input verdict is
// produced. Each case runs under a LIVE context first (the plain
// behavior) and then under a pre-cancelled one (the signal shape).
func TestPlanCancelVersusFailingInputPrecedence(t *testing.T) {
	cat := createCatalog(t) // exists but holds no scanned sets

	cases := []struct {
		name      string
		args      []string
		live      int // exit with a live context
		cancelled int // exit with a pre-cancelled run context
	}{
		{
			// resolvePlanOptions runs before ANY context-aware call:
			// the usage error survives the cancellation (Classify
			// checks UsageError before ErrInterrupted, and execute's
			// wrap cannot erase it).
			name:      "missing-size-usage-wins",
			args:      []string{"plan", "--catalog", cat, "--set-id", "S-NOPE"},
			live:      report.ExitUsage,
			cancelled: report.ExitUsage,
		},
		{
			name:      "missing-set-id-usage-wins",
			args:      []string{"plan", "--catalog", cat, "--size", planFixtureSize},
			live:      report.ExitUsage,
			cancelled: report.ExitUsage,
		},
		{
			// catalog.Open does not take a context: the missing-file
			// CatalogError is produced before cancellation matters.
			name:      "missing-catalog-file-catalog-wins",
			args:      []string{"plan", "--catalog", filepath.Join(t.TempDir(), "no-such.sqlite"), "--set-id", "S-X", "--size", planFixtureSize},
			live:      report.ExitCatalog,
			cancelled: report.ExitCatalog,
		},
		{
			// PriorScanRuns is CONTEXT-AWARE: with a live context it
			// returns 0 -> the "no scanned set" usage gate (2); with
			// a cancelled context the driver refuses the read first,
			// so the run dies as an interruption (130) and the usage
			// verdict is never produced.
			name:      "unknown-set-usage-live-cancelled-interrupt",
			args:      []string{"plan", "--catalog", cat, "--set-id", "S-TYPO", "--size", planFixtureSize},
			live:      report.ExitUsage,
			cancelled: report.ExitInterrupted,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("live", func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				errBuf := &bytes.Buffer{}
				if code := execute(ctx, NewRootCmd(), tc.args, errBuf); code != tc.live {
					t.Errorf("live exit = %d, want %d\nstderr: %s", code, tc.live, errBuf.String())
				}
			})
			t.Run("cancelled", func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel() // the signal, projected onto command start
				errBuf := &bytes.Buffer{}
				code := execute(ctx, NewRootCmd(), tc.args, errBuf)
				if code != tc.cancelled {
					t.Errorf("cancelled exit = %d, want %d (precedence pin)\nstderr: %s", code, tc.cancelled, errBuf.String())
				}
				if tc.cancelled == report.ExitInterrupted &&
					!strings.Contains(errBuf.String(), "interrupted by signal") {
					t.Errorf("interrupt-class wording missing:\n%s", errBuf.String())
				}
			})
		})
	}
}
