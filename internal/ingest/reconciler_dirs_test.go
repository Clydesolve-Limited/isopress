package ingest

// T2.2 (phase 1): reconciler unit tests for the empty-dir durability
// slice (2026-10-19 architect ruling; DESIGN_SPEC 2.3 directories
// table, 3.7 reconciliation extended to dirs). Over the SAME fake store
// as reconciler_test.go; the SQL half lives in internal/catalog's
// dirs tests and the end-to-end flow in internal/cli.
//
// Covered outcomes (the task-required list):
//   - new empty dir -> active (upserted);
//   - still-empty dir -> active again with a STABLE id (upsert, not
//     versioning - dirs are never lineage rows);
//   - dir that gained a file -> missing;
//   - dir deleted from the source -> missing;
//   - dir that became empty again -> active (re-activated by upsert);
//   - NO content/lineage events for dirs (no InsertScanFile /
//     InsertModification / UpdateSeen - contrast the file outcomes);
//   - abort safety: ReconcileEmptyDirs refuses before ScanComplete;
//   - an EMPTY entries slice still marks the whole active set missing
//     (a rescan where every empty dir gained files);
//   - rollback discipline: a failing upsert aborts the transaction and
//     leaves the counts untouched.

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"isopress/internal/fsmeta"
)

// dirEntry builds a walk empty-dir entry with the given relpath and a
// distinguishable mtime so upsert refreshes are assertable.
func dirEntry(rel string, mtimeNS int64) EmptyDirEntry {
	return EmptyDirEntry{
		RelPath: rel,
		Meta:    fsmeta.Meta{Mode: 0o755, UID: 1000, GID: 100, MtimeNS: mtimeNS},
	}
}

// activeDirRels extracts the active dir relpaths from the fake, sorted
// by id (insertion order).
func activeDirRels(f *fakeStore) []string {
	var out []string
	// The fake keys by relpath; walk ids in ascending order via the
	// dirsMissing-agnostic scan below.
	for _, d := range f.dirs {
		if d.Status == "active" {
			out = append(out, d.RelPath)
		}
	}
	return out
}

// TestReconcilerEmptyDirLifecycle drives three rescans over the same
// catalog and asserts every dir outcome of the 2026-10-19 ruling.
func TestReconcilerEmptyDirLifecycle(t *testing.T) {
	f := newFakeStore()

	// Scan 1: two empty dirs. Both active, ids assigned, counts set.
	r1 := NewReconciler(f.factory(), 1)
	r1.ScanComplete()
	if missing, err := r1.ReconcileEmptyDirs(context.Background(),
		[]EmptyDirEntry{dirEntry("keep-empty", 11), dirEntry("will-fill", 12)}); err != nil {
		t.Fatalf("scan 1 ReconcileEmptyDirs: %v", err)
	} else if missing != 0 {
		t.Errorf("scan 1 missing = %d, want 0", missing)
	}
	if got := r1.Counts().Dirs; got != 2 {
		t.Errorf("scan 1 counts.Dirs = %d, want 2", got)
	}
	keepID := f.dirs["keep-empty"].ID
	fillID := f.dirs["will-fill"].ID

	// Scan 2 (rescan): "will-fill" gained a file (not in the empty
	// set), "deleted" was removed from the source, "became-empty" is
	// newly empty. Outcomes: gained-file AND deleted -> missing;
	// became-empty -> active (new row); keep-empty stays active with a
	// STABLE id.
	f.dirs["deleted"] = &fakeDir{ID: 41, RelPath: "deleted", Status: "active"}
	r2 := NewReconciler(f.factory(), 2)
	r2.ScanComplete()
	if missing, err := r2.ReconcileEmptyDirs(context.Background(),
		[]EmptyDirEntry{dirEntry("keep-empty", 21), dirEntry("became-empty", 22)}); err != nil {
		t.Fatalf("scan 2 ReconcileEmptyDirs: %v", err)
	} else if missing != 2 {
		t.Errorf("scan 2 missing = %d, want 2 (gained-file + deleted)", missing)
	}
	if got := r2.Counts().Dirs; got != 2 {
		t.Errorf("scan 2 counts.Dirs = %d, want 2 (the active set after the scan)", got)
	}
	if f.dirs["will-fill"].Status != "missing" || f.dirs["deleted"].Status != "missing" {
		t.Errorf("gained-file/deleted statuses = %s/%s, want missing/missing",
			f.dirs["will-fill"].Status, f.dirs["deleted"].Status)
	}
	if f.dirs["keep-empty"].Status != "active" || f.dirs["keep-empty"].ID != keepID {
		t.Errorf("keep-empty = %+v, want still active with stable id %d", f.dirs["keep-empty"], keepID)
	}
	if f.dirs["will-fill"].ID != fillID {
		t.Errorf("will-fill id = %d, want stable %d (missing rows keep their identity)", f.dirs["will-fill"].ID, fillID)
	}
	// A missing row KEEPS its image_id in the real SQL (status-only
	// write); the fake models the same status-only contract.
	if got, want := sortedOrNil(activeDirRels(f)), []string{"became-empty", "keep-empty"}; !reflect.DeepEqual(got, want) {
		t.Errorf("active dirs after scan 2 = %v, want %v", got, want)
	}

	// Scan 3: "became-empty" went EMPTY AGAIN after being new in scan
	// 2? No - the re-activation case: "will-fill" becomes empty again
	// (its file was deleted) -> upsert re-activates it with its stable
	// id. "deleted" stays missing (never seen).
	r3 := NewReconciler(f.factory(), 3)
	r3.ScanComplete()
	if _, err := r3.ReconcileEmptyDirs(context.Background(),
		[]EmptyDirEntry{dirEntry("keep-empty", 31), dirEntry("will-fill", 32)}); err != nil {
		t.Fatalf("scan 3 ReconcileEmptyDirs: %v", err)
	}
	if f.dirs["will-fill"].Status != "active" || f.dirs["will-fill"].ID != fillID {
		t.Errorf("will-fill after becoming empty again = %+v, want active with stable id %d",
			f.dirs["will-fill"], fillID)
	}
	if f.dirs["deleted"].Status != "missing" {
		t.Errorf("deleted status = %s, want still missing", f.dirs["deleted"].Status)
	}
}

// TestReconcilerEmptyDirsEmitNoLineageEvents: dir reconciliation issues
// ONLY dir operations - no file inserts, no modifications, no
// last_seen touches, no file missing marks (no content/lineage logic
// for dirs, contrast files).
func TestReconcilerEmptyDirsEmitNoLineageEvents(t *testing.T) {
	f := newFakeStore()
	f.active["x.txt"] = activeWith(1, "x.txt", 0xAA)
	f.nextID = 1
	r := NewReconciler(f.factory(), 1)
	r.ScanComplete()
	if _, err := r.ReconcileEmptyDirs(context.Background(), []EmptyDirEntry{dirEntry("d1", 1)}); err != nil {
		t.Fatalf("ReconcileEmptyDirs: %v", err)
	}
	for _, entry := range f.journal {
		switch entry {
		case "tx1:InsertScanFile", "tx1:InsertModification", "tx1:UpdateSeen", "tx1:MarkMissing",
			"tx1:ActiveByRelPaths", "tx1:ListActive":
			t.Errorf("dir reconciliation touched the FILES surface: %v (journal: %v)", entry, f.journal)
		}
	}
	if len(f.mods) != 0 || len(f.touches) != 0 || len(f.missing) != 0 {
		t.Errorf("dir reconciliation produced file-side writes: mods=%v touches=%v missing=%v",
			f.mods, f.touches, f.missing)
	}
}

// TestReconcilerEmptyDirsRefusedBeforeComplete is the abort-safety
// pin: an aborted scan must never mark dirs missing off a partial
// walk - same gate as the file pass.
func TestReconcilerEmptyDirsRefusedBeforeComplete(t *testing.T) {
	f := newFakeStore()
	f.dirs["preexisting"] = &fakeDir{ID: 1, RelPath: "preexisting", Status: "active"}
	r := NewReconciler(f.factory(), 1)

	if _, err := r.ReconcileEmptyDirs(context.Background(),
		[]EmptyDirEntry{dirEntry("d1", 1)}); !errors.Is(err, ErrScanIncomplete) {
		t.Fatalf("ReconcileEmptyDirs before ScanComplete = %v, want ErrScanIncomplete", err)
	}
	if len(f.journal) != 0 {
		t.Errorf("aborted scan reached the store: %v", f.journal)
	}
	if f.dirs["preexisting"].Status != "active" {
		t.Errorf("preexisting dir = %s, want active (aborted scan marks nothing)", f.dirs["preexisting"].Status)
	}
}

// TestReconcilerEmptyEntriesMarksAllDirsMissing: a rescan where every
// previously-empty dir gained files (or was deleted) yields an EMPTY
// entries slice, which is NOT a no-op - the whole active set goes
// missing (counts.Dirs = 0 afterwards).
func TestReconcilerEmptyEntriesMarksAllDirsMissing(t *testing.T) {
	f := newFakeStore()
	f.dirs["d1"] = &fakeDir{ID: 1, RelPath: "d1", Status: "active"}
	f.dirs["d2"] = &fakeDir{ID: 2, RelPath: "d2", Status: "active"}
	r := NewReconciler(f.factory(), 1)
	r.ScanComplete()

	missing, err := r.ReconcileEmptyDirs(context.Background(), nil)
	if err != nil {
		t.Fatalf("ReconcileEmptyDirs(nil): %v", err)
	}
	if missing != 2 {
		t.Errorf("missing = %d, want 2 (every active dir lost its emptiness)", missing)
	}
	if got := r.Counts().Dirs; got != 0 {
		t.Errorf("counts.Dirs = %d, want 0", got)
	}
	if f.dirs["d1"].Status != "missing" || f.dirs["d2"].Status != "missing" {
		t.Errorf("dir statuses = %s/%s, want missing/missing", f.dirs["d1"].Status, f.dirs["d2"].Status)
	}
}

// TestReconcilerEmptyDirFailureRollsBack: a failing upsert aborts the
// transaction (rollback fires), the error propagates, and the counts
// carry no trace of the aborted run.
func TestReconcilerEmptyDirFailureRollsBack(t *testing.T) {
	f := newFakeStore()
	f.failOp = "UpsertEmptyDir"
	f.failErr = errors.New("disk full")
	r := NewReconciler(f.factory(), 1)
	r.ScanComplete()

	if _, err := r.ReconcileEmptyDirs(context.Background(),
		[]EmptyDirEntry{dirEntry("d1", 1)}); !errors.Is(err, f.failErr) {
		t.Fatalf("ReconcileEmptyDirs error = %v, want the injected failure", err)
	}
	if f.rollbacks != 1 {
		t.Errorf("rollbacks = %d, want 1 (failed transaction rolled back)", f.rollbacks)
	}
	if got := r.Counts().Dirs; got != 0 {
		t.Errorf("counts.Dirs = %d after a rolled-back run, want 0", got)
	}
}

// sortedOrNil sorts a relpath slice for stable comparison (the fake's
// map iteration order is nondeterministic).
func sortedOrNil(rels []string) []string {
	if len(rels) == 0 {
		return nil
	}
	out := append([]string(nil), rels...)
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}
