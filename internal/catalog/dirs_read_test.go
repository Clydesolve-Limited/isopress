package catalog

// T5.3: the ActiveDirs read (DESIGN_SPEC 1.2 layering - SQL stays in
// this package; 2.3 directories; 8.2 selection over active records;
// IMPLEMENTATION_PLAN 4.6 T5.3). The read is the dir half of the
// restore selection universe, so the assertions pin exactly what that
// selection depends on:
//
//   - ACTIVE rows come back (assigned AND unassigned - the planner's
//     assignment state must not hide a dir from the selection, or an
//     unplanned empty dir would silently vanish from the run);
//   - MISSING rows stay out (FR-032 mark-do-not-delete: a dir no longer
//     empty needs no restore representation);
//   - it runs on *DB outside any transaction and under no advisory lock
//     (the read-only restore open of DESIGN_SPEC 2.1 Option B).
import (
	"context"
	"testing"
)

func TestActiveDirsReturnsActiveAssignedAndUnassignedRows(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()

	unassigned := insertEmptyDir(t, f, "empty-unassigned")
	missing := insertEmptyDir(t, f, "empty-gone")
	markDirMissing(t, f, missing)

	// Assign one row through the planner: an ASSIGNED dir must stay in
	// the selection universe (it is exactly the row whose image holds
	// its bytes), while the planner's own read (unassigned-only) drops
	// it.
	assigned := insertEmptyDir(t, f, "empty-assigned")
	planTxCommit(t, f, func(tx *PlanTx) {
		if err := tx.InsertImage(ctx, ImageSpec{ImageID: 1, NominalSize: 1000,
			UsableSize: 900, MediaProfile: "disk"}); err != nil {
			t.Fatalf("InsertImage: %v", err)
		}
		if err := tx.AssignDir(ctx, assigned, 1); err != nil {
			t.Fatalf("AssignDir: %v", err)
		}
	})

	dirs, err := f.db.ActiveDirs(ctx, f.setID)
	if err != nil {
		t.Fatalf("ActiveDirs: %v", err)
	}
	byPath := make(map[string]int64, len(dirs))
	for _, d := range dirs {
		byPath[d.RelPath] = d.DirID
	}
	if len(dirs) != 2 {
		t.Fatalf("ActiveDirs = %+v, want exactly the two active rows (assigned + unassigned, never the missing one)", dirs)
	}
	if got := byPath["empty-unassigned"]; got != unassigned {
		t.Errorf("unassigned active dir id = %d, want %d", got, unassigned)
	}
	if got := byPath["empty-assigned"]; got != assigned {
		t.Errorf("assigned active dir id = %d, want %d (assignment must not hide a row from the selection)", got, assigned)
	}
	if _, ok := byPath["empty-gone"]; ok {
		t.Error("a status='missing' dir must never appear in ActiveDirs")
	}
}

// An unknown set yields no rows rather than an error: the restore
// selection treats "no active records" as its own empty-selection
// class, so the read must not conflate an unknown set with a catalog
// fault.
func TestActiveDirsUnknownSetIsEmpty(t *testing.T) {
	f := newScanFixture(t)
	dirs, err := f.db.ActiveDirs(context.Background(), "no-such-set")
	if err != nil {
		t.Fatalf("ActiveDirs (unknown set): %v", err)
	}
	if len(dirs) != 0 {
		t.Fatalf("ActiveDirs (unknown set) = %+v, want none", dirs)
	}
}
