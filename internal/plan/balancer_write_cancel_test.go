package plan

// T6.3 fix round: Plan.Write behavior when the run context is
// cancelled INSIDE the single assignment transaction (the write phase
// - after Balance returned successfully, tx open, statements in
// flight). The Balance-phase aborts are pinned in
// balancer_fix_round_test.go / balancer_ctx_sweep_test.go; this file
// owns the write-side half of the interrupt contract:
//
//   - a cancel surfaced by the store layer between/during statements
//     aborts the write with a ctx-anchored (bare, never reclassed as
//     usage/catalog) error, calls Rollback, NEVER commits;
//   - a cancel at tx ENTRY (before the first statement) does the
//     same - zero statements applied;
//   - HONEST EDGE, pinned deliberately: a cancel that lands after
//     the LAST statement but before Commit is NOT observed by
//     Plan.Write - Commit takes no context, the transaction commits
//     atomically, and Write returns nil. The process-level verdict is
//     then execute's job (a signal-cancelled run context still exits
//     130 over committed state; see root.go's execute override and
//     the interrupt_shape_matrix's "signal after completion" cell).
//     The catalog-layer classification of the in-flight statement
//     error (plain wrapped, => Classify ExitFatal => 130 via
//     execute's ctx override) is pinned on the REAL sqlite in
//     internal/cli/plan_write_phase_cancel_test.go.
//
// fakeStore is deliberately NOT transactional (its writes apply
// immediately; rollback only counts), so the zero-debris claim under
// a mid-statement cancel is only assertable there for the
// cancel-at-entry leg (no statement ran). The byte-identical /
// zero-row rollback proof against real sqlite lives in the cli test
// named above.

import (
	"context"
	"errors"
	"testing"
)

// ctxCheckingAssignStore models a context-honoring driver (what
// database/sql + modernc sqlite do: the context is checked before
// each statement): every method refuses to run while ctx is done, and
// optionally fires cancel() right after the FIRST statement lands -
// the deterministic in-process projection of a signal arriving mid-
// transaction. No goroutines, no sleeps: the cancel happens inside a
// statement call the Write loop itself invokes.
type ctxCheckingAssignStore struct {
	inner           AssignStore
	cancel          context.CancelFunc
	cancelAfterStmt bool
	stmts           int
}

func (c *ctxCheckingAssignStore) pre(ctx context.Context) error {
	return ctx.Err() // driver refuses statements on a done context
}

func (c *ctxCheckingAssignStore) post() {
	c.stmts++
	if c.cancelAfterStmt && c.stmts == 1 {
		c.cancel()
	}
}

func (c *ctxCheckingAssignStore) InsertImage(ctx context.Context, img ImageSpec) error {
	if err := c.pre(ctx); err != nil {
		return err
	}
	err := c.inner.InsertImage(ctx, img)
	if err != nil {
		return err
	}
	c.post()
	return nil
}

func (c *ctxCheckingAssignStore) EnsureChunkRows(ctx context.Context, fileID int64, specs []ChunkSpec) error {
	if err := c.pre(ctx); err != nil {
		return err
	}
	if err := c.inner.EnsureChunkRows(ctx, fileID, specs); err != nil {
		return err
	}
	c.post()
	return nil
}

func (c *ctxCheckingAssignStore) ReplaceAllChunks(ctx context.Context, fileID int64, specs []ChunkSpec) (RechunkOutcome, error) {
	if err := c.pre(ctx); err != nil {
		return RechunkOutcome{}, err
	}
	out, err := c.inner.ReplaceAllChunks(ctx, fileID, specs)
	if err != nil {
		return RechunkOutcome{}, err
	}
	c.post()
	return out, nil
}

func (c *ctxCheckingAssignStore) DropChunkState(ctx context.Context, fileID int64) (int, error) {
	if err := c.pre(ctx); err != nil {
		return 0, err
	}
	n, err := c.inner.DropChunkState(ctx, fileID)
	if err != nil {
		return 0, err
	}
	c.post()
	return n, nil
}

func (c *ctxCheckingAssignStore) StoreChunkSize(ctx context.Context, fileID, chunkSize int64, chunkCount int) error {
	if err := c.pre(ctx); err != nil {
		return err
	}
	if err := c.inner.StoreChunkSize(ctx, fileID, chunkSize, chunkCount); err != nil {
		return err
	}
	c.post()
	return nil
}

func (c *ctxCheckingAssignStore) AssignFile(ctx context.Context, fileID, imageID int64) error {
	if err := c.pre(ctx); err != nil {
		return err
	}
	if err := c.inner.AssignFile(ctx, fileID, imageID); err != nil {
		return err
	}
	c.post()
	return nil
}

func (c *ctxCheckingAssignStore) AssignChunk(ctx context.Context, fileID int64, chunkIndex int, imageID int64) error {
	if err := c.pre(ctx); err != nil {
		return err
	}
	if err := c.inner.AssignChunk(ctx, fileID, chunkIndex, imageID); err != nil {
		return err
	}
	c.post()
	return nil
}

func (c *ctxCheckingAssignStore) AssignDir(ctx context.Context, dirID, imageID int64) error {
	if err := c.pre(ctx); err != nil {
		return err
	}
	if err := c.inner.AssignDir(ctx, dirID, imageID); err != nil {
		return err
	}
	c.post()
	return nil
}

// writeCancelFixture: a plan with every op kind (whole, chunked with
// rows to create, dir) so the write has statements of each shape.
func writeCancelFixture(t *testing.T) (*fakeStore, Plan) {
	t.Helper()
	s := newFakeStore(testProfile(612))
	addFile(s, 1, "whole.bin", 60)
	addLink(s, 2, "link", 10)
	addFile(s, 3, "big.img", 250) // chunks: 100 + 100 + 50
	s.dirs[9] = &fakeDir{relPath: "empty/d", active: true}

	p, err := Balance(context.Background(), s, s.profile)
	if err != nil {
		t.Fatalf("fixture Balance: %v", err)
	}
	if p.ops.empty() {
		t.Fatal("fixture produced an empty plan - the write tests need real statements")
	}
	return s, p
}

// TestPlanWriteCancelAtTransactionEntryAppliesNothing: the cancel
// lands after BEGIN (factory returns a usable tx) but before the
// first statement - the driver rejects statement 1, Write rolls back
// and never commits, and (no statement ran) the store is untouched.
func TestPlanWriteCancelAtTransactionEntryAppliesNothing(t *testing.T) {
	s, p := writeCancelFixture(t)
	before := s.digest()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wrapper := &ctxCheckingAssignStore{inner: s, cancel: cancel, cancelAfterStmt: false}
	err := p.Write(ctx, func(wctx context.Context) (*AssignTx, error) {
		tx, err := s.factory()(wctx)
		if err != nil {
			return nil, err
		}
		cancel() // the signal lands between BEGIN and statement 1
		tx.AssignStore = wrapper
		return tx, nil
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Write(cancelled before statement 1) = %v, want a context.Canceled-anchored error", err)
	}
	if s.committed != 0 {
		t.Errorf("cancelled write COMMITTED %d times", s.committed)
	}
	if s.rolledBack != 1 {
		t.Errorf("rollback count = %d, want 1", s.rolledBack)
	}
	if wrapper.stmts != 0 {
		t.Errorf("%d statements ran after the cancel - none may", wrapper.stmts)
	}
	if after := s.digest(); after != before {
		t.Errorf("store changed across a pre-statement cancel:\n--- before ---\n%s--- after ---\n%s", before, after)
	}
}

// TestPlanWriteCancelAfterFirstStatementRollsBack: the cancel fires
// when the FIRST statement has landed (image row inserted inside the
// tx), so the next statement is rejected - Write returns the
// ctx-anchored error, rolls back, never commits. (The fake's rollback
// does not undo in-memory state by design - the REAL rollback proof
// is internal/cli/plan_write_phase_cancel_test.go on sqlite; here we
// pin Write's control flow: error out, roll back, no commit.)
func TestPlanWriteCancelAfterFirstStatementRollsBack(t *testing.T) {
	s, p := writeCancelFixture(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wrapper := &ctxCheckingAssignStore{inner: s, cancel: cancel, cancelAfterStmt: true}
	err := p.Write(ctx, func(wctx context.Context) (*AssignTx, error) {
		tx, err := s.factory()(wctx)
		if err != nil {
			return nil, err
		}
		tx.AssignStore = wrapper
		return tx, nil
	})

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Write(cancelled after statement 1) = %v, want a context.Canceled-anchored error", err)
	}
	if wrapper.stmts != 1 {
		t.Errorf("statements after cancel: %d, want exactly 1 (the one that fired the cancel)", wrapper.stmts)
	}
	if s.committed != 0 {
		t.Errorf("cancelled write COMMITTED %d times - the tx must roll back", s.committed)
	}
	if s.rolledBack != 1 {
		t.Errorf("rollback count = %d, want 1", s.rolledBack)
	}
	// The error must stay in the interruption class all the way out:
	// no usage/catalog sentinel may wrap a cancellation.
	if errors.Is(err, ErrItemTooLarge) || errors.Is(err, ErrInvalidProfile) {
		t.Errorf("write-phase cancellation misclassified as a plan sentinel: %v", err)
	}
}

// TestPlanWriteCancelAfterLastStatementStillCommits (HONEST EDGE):
// Plan.Write performs NO context probe of its own and Commit carries
// no context, so a cancel that lands after the final statement - the
// window between the last assignment and tx.Commit - is invisible to
// the write: it commits atomically and returns nil. Pinned as-is:
// the state is consistent (all-or-nothing), and the interrupt verdict
// for the RUN stays with execute's cancelled-run-context override
// (exit 130), which does not depend on Write's return.
func TestPlanWriteCancelAfterLastStatementStillCommits(t *testing.T) {
	s, p := writeCancelFixture(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	wrapper := &ctxCheckingAssignStore{inner: s, cancel: cancel, cancelAfterStmt: false}
	err := p.Write(ctx, func(wctx context.Context) (*AssignTx, error) {
		tx, err := s.factory()(wctx)
		if err != nil {
			return nil, err
		}
		tx.AssignStore = wrapper
		// Cancel AFTER Write's whole statement sequence: the commit
		// hook fires the signal instead, i.e. exactly that window.
		tx.Commit = func() error {
			cancel()
			s.committed++
			return nil
		}
		return tx, nil
	})

	if err != nil {
		t.Fatalf("Write(cancel between last statement and Commit) = %v, want success (commit not ctx-gated)", err)
	}
	if s.committed != 1 || s.rolledBack != 0 {
		t.Errorf("commit=%d rollback=%d, want 1/0 - the post-statement cancel must not abort a commit "+
			"whose statements all landed (documented window, see file doc)", s.committed, s.rolledBack)
	}

	// Oracle: the state must be EXACTLY what a clean, never-cancelled
	// write of the same fixture produces - atomic all-or-nothing even
	// in this window.
	controlStore, controlPlan := writeCancelFixture(t)
	if err := controlPlan.Write(context.Background(), controlStore.factory()); err != nil {
		t.Fatalf("control Write: %v", err)
	}
	if after, want := s.digest(), controlStore.digest(); after != want {
		t.Errorf("post-statement-cancel state differs from a clean write:\n--- got  ---\n%s--- want ---\n%s", after, want)
	}
}
