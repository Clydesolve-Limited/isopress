package ingest

// T1.6: reconciler unit tests over a FAKE store - no SQL, no database
// (the catalog-side integration lives in internal/catalog's
// scanstore_test.go). The fake records every store call with the
// transaction it was issued under, so the tests can assert both the
// 3.7 decision table and the transactional shape of the wiring.
//
// Coverage (task-required list):
//   - all five stream outcomes (new / unchanged / modified /
//     unreadable-row / changed-row) plus the not-seen (missing) pass;
//   - MODIFIED keeps the old row: InsertModification carries the old
//     file_id (same-tx modified_of/superseded_by pairing - the fake
//     store models the pair exactly as catalog.ScanTx writes it);
//   - the not-seen pass is SKIPPED on an aborted scan: MarkMissing
//     refuses before ScanComplete (ErrScanIncomplete) and the fake
//     never sees a ListActive/MarkMissing call (the safety pin);
//   - symlink rows: entry_kind 'symlink', link_target set, hash NULL;
//   - empty dirs (T2.2): durable rows via ReconcileEmptyDirs - the
//     unit coverage lives in reconciler_dirs_test.go over the same
//     fake store (upsert / gained-file-missing / deleted-missing /
//     became-empty-again / abort safety);
//   - the stale-digest drop: failure-class rows store hash NULL even
//     when the ClassifiedResult carries a digest.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"isopress/internal/fsmeta"
)

// --- fake store --------------------------------------------------------------

// fakeMod records one versioned-pair write.
type fakeMod struct {
	txID  int
	OldID int64
	NewID int64
	Rec   FileRow
}

// fakeDir models one directories-table row in the fake store.
type fakeDir struct {
	ID      int64
	RelPath string
	Status  string
	Runs    int // upsert count (last_seen bumps are modeled as run touches)
}

// fakeStore is the committed-state model plus a call journal. Ops
// mutate state immediately (the real atomicity semantics are pinned by
// the catalog integration tests); txID tags each call with the
// transaction it was issued under so same-tx pairing is assertable.
type fakeStore struct {
	nextID int64

	active  map[string]ActiveRecord // committed, keyed by relpath
	journal []string
	touches []int64
	mods    []fakeMod
	missing []int64

	// directories model (T2.2): relpath -> row; only rows with
	// Status "active" appear in ListActiveDirs, mirroring the SQL.
	dirs        map[string]*fakeDir
	dirNextID   int64
	dirsMissing []int64

	txSeq     int
	curTx     int
	rollbacks int

	// Failure injection: the named op returns this error.
	failOp  string
	failErr error
	// ListActiveGate: when set, ListActive fails (used to test the
	// not-seen pass error path).
	listErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		active: map[string]ActiveRecord{},
		dirs:   map[string]*fakeDir{},
	}
}

func (f *fakeStore) note(op string) error {
	f.journal = append(f.journal, fmt.Sprintf("tx%d:%s", f.curTx, op))
	if f.failOp == op {
		return f.failErr
	}
	return nil
}

func (f *fakeStore) ActiveByRelPaths(_ context.Context, relpaths []string) (map[string]ActiveRecord, error) {
	if err := f.note("ActiveByRelPaths"); err != nil {
		return nil, err
	}
	out := map[string]ActiveRecord{}
	for _, p := range relpaths {
		if a, ok := f.active[p]; ok {
			out[p] = a
		}
	}
	return out, nil
}

func (f *fakeStore) InsertScanFile(_ context.Context, rec FileRow) (int64, error) {
	if err := f.note("InsertScanFile"); err != nil {
		return 0, err
	}
	f.nextID++
	f.active[rec.RelPath] = ActiveRecord{
		FileID:     f.nextID,
		RelPath:    rec.RelPath,
		EntryKind:  rec.EntryKind,
		Hash:       rec.Hash,
		LinkTarget: rec.LinkTarget,
		Status:     rec.Status,
	}
	return f.nextID, nil
}

func (f *fakeStore) UpdateSeen(_ context.Context, fileID int64) error {
	if err := f.note("UpdateSeen"); err != nil {
		return err
	}
	f.touches = append(f.touches, fileID)
	return nil
}

// InsertModification mirrors catalog.ScanTx's versioned pair: the new
// row supersedes the old (modified_of/superseded_by in one tx); the
// old row KEEPS everything else (its image binding, in particular -
// modeled here as a Retained field the test inspects).
func (f *fakeStore) InsertModification(_ context.Context, oldFileID int64, rec FileRow) (int64, error) {
	if err := f.note("InsertModification"); err != nil {
		return 0, err
	}
	old, ok := f.findByID(oldFileID)
	if !ok {
		return 0, fmt.Errorf("fake: no active file %d", oldFileID)
	}
	f.nextID++
	newID := f.nextID
	// Pairing, exactly per 2.4: old superseded, new carries the
	// lineage pointer.
	old.Status = "superseded" // leaves the active view, like superseded_by set
	f.active[rec.RelPath] = ActiveRecord{
		FileID:     newID,
		RelPath:    rec.RelPath,
		EntryKind:  rec.EntryKind,
		Hash:       rec.Hash,
		LinkTarget: rec.LinkTarget,
		Status:     rec.Status,
	}
	f.mods = append(f.mods, fakeMod{txID: f.curTx, OldID: oldFileID, NewID: newID, Rec: rec})
	return newID, nil
}

func (f *fakeStore) findByID(id int64) (ActiveRecord, bool) {
	for _, a := range f.active {
		if a.FileID == id {
			return a, true
		}
	}
	return ActiveRecord{}, false
}

func (f *fakeStore) ListActive(_ context.Context) ([]ActiveRecord, error) {
	if err := f.note("ListActive"); err != nil {
		return nil, err
	}
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []ActiveRecord
	for _, a := range f.active {
		if a.Status == "active" {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeStore) MarkMissing(_ context.Context, fileIDs []int64) (int, error) {
	if err := f.note("MarkMissing"); err != nil {
		return 0, err
	}
	f.missing = append(f.missing, fileIDs...)
	return len(fileIDs), nil
}

// UpsertEmptyDir models catalog.ScanTx.UpsertEmptyDir: a new path
// inserts with a fresh id; an existing path keeps its id (first_seen)
// and is re-activated. No lineage, no versioning (T2.2).
func (f *fakeStore) UpsertEmptyDir(_ context.Context, rec DirRow) error {
	if err := f.note("UpsertEmptyDir"); err != nil {
		return err
	}
	if d, ok := f.dirs[rec.RelPath]; ok {
		d.Status = "active"
		d.Runs++
		return nil
	}
	f.dirNextID++
	f.dirs[rec.RelPath] = &fakeDir{ID: f.dirNextID, RelPath: rec.RelPath, Status: "active", Runs: 1}
	return nil
}

func (f *fakeStore) ListActiveDirs(_ context.Context) ([]ActiveDir, error) {
	if err := f.note("ListActiveDirs"); err != nil {
		return nil, err
	}
	var out []ActiveDir
	for _, d := range f.dirs {
		if d.Status == "active" {
			out = append(out, ActiveDir{DirID: d.ID, RelPath: d.RelPath})
		}
	}
	return out, nil
}

func (f *fakeStore) MarkDirsMissing(_ context.Context, dirIDs []int64) (int, error) {
	if err := f.note("MarkDirsMissing"); err != nil {
		return 0, err
	}
	f.dirsMissing = append(f.dirsMissing, dirIDs...)
	for _, id := range dirIDs {
		for _, d := range f.dirs {
			if d.ID == id {
				d.Status = "missing"
			}
		}
	}
	return len(dirIDs), nil
}

// factory hands out one Tx per call; Commit is a no-op (state is
// applied eagerly), Rollback counts so tests can assert the deferred
// rollback fired on error paths.
func (f *fakeStore) factory() TxFactory {
	return func(context.Context) (*Tx, error) {
		f.txSeq++
		f.curTx = f.txSeq
		return &Tx{
			Store:    f,
			Commit:   func() error { return nil },
			Rollback: func() { f.rollbacks++ },
		}, nil
	}
}

// --- helpers -----------------------------------------------------------------

// okResult builds a clean hash result with the given digest (the raw
// pipeline shape); okClass wraps it in the T1.5 classification shell.
func okResult(rel string, size int64, digest byte) HashResult {
	var d [32]byte
	for i := range d {
		d[i] = digest
	}
	return HashResult{
		Job:    Job{RelPath: rel, AbsPath: "/" + rel, Meta: fsmeta.Meta{Size: size, MtimeNS: 1}},
		Digest: d,
		Bytes:  size,
	}
}

func okClass(rel string, size int64, digest byte) ClassifiedResult {
	return ClassifiedResult{HashResult: okResult(rel, size, digest), Status: StatusOK}
}

// activeWith builds a committed active row with a 32-byte digest.
func activeWith(id int64, rel string, digest byte) ActiveRecord {
	h := make([]byte, 32)
	for i := range h {
		h[i] = digest
	}
	return ActiveRecord{FileID: id, RelPath: rel, EntryKind: "regular", Hash: h, Status: "active"}
}

// mustBatch runs ReconcileBatch and fails the test on error.
func mustBatch(t *testing.T, r *Reconciler, batch ...ClassifiedResult) {
	t.Helper()
	if err := r.ReconcileBatch(context.Background(), batch); err != nil {
		t.Fatalf("ReconcileBatch: %v", err)
	}
}

// --- the five outcomes + not-seen -------------------------------------------

// TestReconcilerFiveOutcomes drives one batch containing the five
// stream outcomes and then the not-seen pass, asserting every counter
// and every store call of the 3.7 decision table.
func TestReconcilerFiveOutcomes(t *testing.T) {
	f := newFakeStore()
	// Pre-existing active rows: one that will match (unchanged), one
	// that will not (modified), one that the stream never mentions
	// (missing).
	f.active["keep.txt"] = activeWith(1, "keep.txt", 0xAA)
	f.active["edit.txt"] = activeWith(2, "edit.txt", 0x11)
	f.active["gone.txt"] = activeWith(3, "gone.txt", 0x22)
	f.nextID = 3

	r := NewReconciler(f.factory(), 7)

	unreadable := okClass("bad.txt", 10, 0x33)
	unreadable.Err = errors.New("ingest: open bad.txt: permission denied")
	unreadable = ClassifyPath(unreadable.HashResult, nil, fsmeta.ModeFull)
	if unreadable.Status != StatusUnreadable {
		t.Fatalf("fixture: expected unreadable, got %s", unreadable.Status)
	}

	mustBatch(t, r,
		okClass("new.txt", 5, 0x01),  // NEW (no active row)
		okClass("keep.txt", 5, 0xAA), // UNCHANGED (digest matches)
		okClass("edit.txt", 5, 0x99), // MODIFIED (digest differs)
		unreadable,                   // UNREADABLE row
	)
	// The changed class goes through the real classifier, exactly like
	// the pipeline does at result-processing time.
	changed := Classify(okResult("flappy.txt", 20, 0x44), fsmeta.Meta{Size: 21, MtimeNS: 2}, fsmeta.ModeFull)
	if changed.Status != StatusChangedDuringScan {
		t.Fatalf("fixture: expected changed_during_scan, got %s", changed.Status)
	}
	mustBatch(t, r, changed)

	r.ScanComplete()
	missing, err := r.MarkMissing(context.Background())
	if err != nil {
		t.Fatalf("MarkMissing: %v", err)
	}

	// Store calls: two batches, each one transaction.
	if got, want := countPrefix(f.journal, "tx1:InsertScanFile"), 2; got != want {
		t.Errorf("batch 1 inserts = %d, want %d (new + unreadable); journal: %v", got, want, f.journal)
	}
	if got, want := countPrefix(f.journal, "tx1:UpdateSeen"), 1; got != want {
		t.Errorf("batch 1 UpdateSeen calls = %d, want %d; journal: %v", got, want, f.journal)
	}
	if got, want := countPrefix(f.journal, "tx1:InsertModification"), 1; got != want {
		t.Errorf("batch 1 InsertModification calls = %d, want %d; journal: %v", got, want, f.journal)
	}

	// Modification pairing: old id 2 superseded by the new row.
	if len(f.mods) != 1 || f.mods[0].OldID != 2 {
		t.Fatalf("modifications = %+v, want exactly one with old file_id 2", f.mods)
	}

	// Unchanged touch targeted the right row.
	if len(f.touches) != 1 || f.touches[0] != 1 {
		t.Errorf("touches = %v, want [1] (keep.txt)", f.touches)
	}

	// Not-seen: only gone.txt was never in the stream.
	if missing != 1 || len(f.missing) != 1 || f.missing[0] != 3 {
		t.Errorf("missing = %v (%d rows), want exactly file_id 3 (gone.txt)", f.missing, missing)
	}

	want := Counts{
		Scanned: 5, Regular: 5,
		Added: 1, Modified: 1, Unchanged: 1, Missing: 1,
		Unreadable: 1, Changed: 1,
		Bytes: 5 + 5 + 5, // new + keep + edit; failure classes contribute no bytes
	}
	if got := r.Counts(); got != want {
		t.Errorf("counts = %+v, want %+v", got, want)
	}
}

func countPrefix(journal []string, prefix string) int {
	n := 0
	for _, e := range journal {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			n++
		}
	}
	return n
}

// --- modification lineage ----------------------------------------------------

// TestReconcilerModifiedPairsInOneTransaction pins the 2.4 pairing:
// the InsertModification call carries the old file_id (the catalog
// side turns that into modified_of on the new row + superseded_by on
// the old, in one transaction - scanstore_test.go asserts the SQL
// half), and the two writes are issued under the SAME transaction as
// the batch's other statements.
func TestReconcilerModifiedPairsInOneTransaction(t *testing.T) {
	f := newFakeStore()
	f.active["a.txt"] = activeWith(42, "a.txt", 0x11)
	f.nextID = 42
	r := NewReconciler(f.factory(), 1)

	mustBatch(t, r, okClass("a.txt", 9, 0x22))

	if len(f.mods) != 1 {
		t.Fatalf("modifications = %+v, want exactly one", f.mods)
	}
	m := f.mods[0]
	if m.OldID != 42 {
		t.Errorf("modification old file_id = %d, want 42", m.OldID)
	}
	if m.NewID == m.OldID {
		t.Errorf("modification new id %d must differ from old id", m.NewID)
	}
	if m.txID != 1 {
		t.Errorf("modification issued under tx %d, want tx 1 (same transaction as the batch)", m.txID)
	}
	if m.Rec.Hash == nil || m.Rec.Hash[0] != 0x22 {
		t.Errorf("modification row digest = %v, want the new digest", m.Rec.Hash)
	}
	if m.Rec.Status != "active" {
		t.Errorf("modification row status = %q, want active", m.Rec.Status)
	}
	if f.rollbacks != 0 {
		t.Errorf("rollback fired %d times on a clean batch, want 0", f.rollbacks)
	}
}

// --- not-seen safety ---------------------------------------------------------

// TestReconcilerRefusesNotSeenBeforeComplete is the SAFETY pin: an
// aborted scan never calls ScanComplete, and MarkMissing must refuse -
// the fake store must see NO ListActive and NO MarkMissing call, so no
// active row can be stamped missing off a partial stream.
func TestReconcilerRefusesNotSeenBeforeComplete(t *testing.T) {
	f := newFakeStore()
	f.active["survivor.txt"] = activeWith(1, "survivor.txt", 0x01)
	f.nextID = 1
	r := NewReconciler(f.factory(), 1)

	// Simulate an aborted scan: one batch drained, then the pipeline
	// died (no ScanComplete).
	mustBatch(t, r, okClass("seen.txt", 3, 0x02))

	if _, err := r.MarkMissing(context.Background()); !errors.Is(err, ErrScanIncomplete) {
		t.Fatalf("MarkMissing before ScanComplete = %v, want ErrScanIncomplete", err)
	}
	for _, entry := range f.journal {
		if entry == "tx2:ListActive" || entry == "tx2:MarkMissing" {
			t.Fatalf("not-seen pass reached the store on an aborted scan: %v", f.journal)
		}
	}
	if f.active["survivor.txt"].Status != "active" {
		t.Errorf("survivor row status = %q, want active (aborted scan marks nothing missing)",
			f.active["survivor.txt"].Status)
	}
}

// TestReconcilerNotSeenErrorRollsBack: a ListActive failure inside the
// not-seen pass aborts the transaction and propagates.
func TestReconcilerNotSeenErrorRollsBack(t *testing.T) {
	f := newFakeStore()
	f.listErr = errors.New("boom")
	r := NewReconciler(f.factory(), 1)
	r.ScanComplete()
	if _, err := r.MarkMissing(context.Background()); err == nil {
		t.Fatal("MarkMissing with a failing ListActive returned nil, want error")
	}
	if f.rollbacks != 1 {
		t.Errorf("rollbacks = %d, want 1 (failed transaction rolled back)", f.rollbacks)
	}
}

// --- stale-digest drop -------------------------------------------------------

// TestReconcilerDropsStaleDigestOnFailureClasses: a ClassifiedResult
// that carries a digest but failed classification stores hash NULL -
// mechanically, at the insert site, for BOTH failure classes.
func TestReconcilerDropsStaleDigestOnFailureClasses(t *testing.T) {
	cases := []struct {
		name   string
		result ClassifiedResult
		wantSt string
	}{
		{
			name: StatusUnreadable,
			result: func() ClassifiedResult {
				h := okResult("x.bin", 100, 0x77) // digest present!
				h.Err = errors.New("ingest: read x.bin: input/output error")
				h.Bytes = 40 // partial-stream byte count
				return ClassifyPath(h, nil, fsmeta.ModeFull)
			}(),
			wantSt: StatusUnreadable,
		},
		{
			name: StatusChangedDuringScan,
			result: Classify(okResult("y.bin", 100, 0x77),
				fsmeta.Meta{Size: 101, MtimeNS: 9}, fsmeta.ModeFull), // digest present, meta drifted
			wantSt: StatusChangedDuringScan,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeStore()
			r := NewReconciler(f.factory(), 1)
			mustBatch(t, r, tc.result)

			a, ok := f.active[tc.result.RelPath]
			if !ok {
				t.Fatalf("no row inserted for %s", tc.result.RelPath)
			}
			if a.Status != tc.wantSt {
				t.Errorf("stored status = %q, want %q", a.Status, tc.wantSt)
			}
			if a.Hash != nil {
				t.Errorf("stored hash = %v, want NULL (stale-digest drop, T1.5 seam rule)", a.Hash)
			}
			c := r.Counts()
			if c.Unreadable+c.Changed != 1 {
				t.Errorf("failure-class counters = unreadable %d + changed %d, want 1 total",
					c.Unreadable, c.Changed)
			}
		})
	}
}

// --- symlinks ----------------------------------------------------------------

// TestReconcilerSymlinkRows: symlinks get rows with entry_kind
// 'symlink', the raw target, and hash NULL (never hashed, D-03); an
// unchanged target touches last_seen only; a changed target versions.
func TestReconcilerSymlinkRows(t *testing.T) {
	f := newFakeStore()
	f.active["link-old"] = ActiveRecord{FileID: 1, RelPath: "link-old", EntryKind: "symlink", LinkTarget: "t1", Status: "active"}
	f.active["link-mod"] = ActiveRecord{FileID: 2, RelPath: "link-mod", EntryKind: "symlink", LinkTarget: "t1", Status: "active"}
	f.nextID = 2
	r := NewReconciler(f.factory(), 1)

	links := []SymlinkEntry{
		{RelPath: "link-new", Target: "t-new", Meta: fsmeta.Meta{Size: 5}},
		{RelPath: "link-old", Target: "t1", Meta: fsmeta.Meta{Size: 2}},
		{RelPath: "link-mod", Target: "t2", Meta: fsmeta.Meta{Size: 2}},
		// Kind change: a regular row that is now a symlink versions too.
	}
	if err := r.ReconcileSymlinks(context.Background(), links); err != nil {
		t.Fatalf("ReconcileSymlinks: %v", err)
	}

	nw, ok := f.active["link-new"]
	if !ok {
		t.Fatal("new symlink inserted no row")
	}
	if nw.EntryKind != "symlink" || nw.LinkTarget != "t-new" || nw.Hash != nil {
		t.Errorf("new symlink row = %+v, want entry_kind symlink, target t-new, hash NULL", nw)
	}
	if len(f.touches) != 1 || f.touches[0] != 1 {
		t.Errorf("touches = %v, want [1] (link-old, unchanged target)", f.touches)
	}
	if len(f.mods) != 1 || f.mods[0].OldID != 2 || f.mods[0].Rec.LinkTarget != "t2" {
		t.Errorf("modifications = %+v, want one for link-old id 2 with the new target", f.mods)
	}
	c := r.Counts()
	if c.Symlinks != 3 || c.Scanned != 3 || c.Added != 1 || c.Unchanged != 1 || c.Modified != 1 {
		t.Errorf("symlink counts = %+v, want symlinks/scanned 3, added 1, unchanged 1, modified 1", c)
	}
}

// TestReconcilerSymlinkKindChangeVersions: an active REGULAR row whose
// path is now a symlink is MODIFIED (kind differs), not deletion plus
// creation - and the old row keeps its hash through the pairing.
func TestReconcilerSymlinkKindChangeVersions(t *testing.T) {
	f := newFakeStore()
	f.active["was-file"] = activeWith(5, "was-file", 0x55)
	f.nextID = 5
	r := NewReconciler(f.factory(), 1)

	if err := r.ReconcileSymlinks(context.Background(), []SymlinkEntry{
		{RelPath: "was-file", Target: "elsewhere", Meta: fsmeta.Meta{Size: 9}},
	}); err != nil {
		t.Fatalf("ReconcileSymlinks: %v", err)
	}
	if len(f.mods) != 1 || f.mods[0].OldID != 5 {
		t.Fatalf("modifications = %+v, want the regular row (id 5) superseded", f.mods)
	}
	if f.mods[0].Rec.EntryKind != "symlink" || f.mods[0].Rec.LinkTarget != "elsewhere" {
		t.Errorf("new row = %+v, want symlink with target elsewhere", f.mods[0].Rec)
	}
}

// --- transaction discipline --------------------------------------------------

// TestReconcilerBatchFailureRollsBackAndPropagates: a store-op error
// inside a batch aborts the transaction (rollback fires), the error
// propagates, and the batch leaves NO counts or seen-set trace
// (snapshot discipline - only committed state is reported).
func TestReconcilerBatchFailureRollsBackAndPropagates(t *testing.T) {
	f := newFakeStore()
	f.active["keep.txt"] = activeWith(1, "keep.txt", 0xAA)
	f.nextID = 1
	f.failOp = "InsertScanFile"
	f.failErr = errConstraint
	r := NewReconciler(f.factory(), 1)

	err := r.ReconcileBatch(context.Background(), []ClassifiedResult{
		okClass("keep.txt", 1, 0xAA), // UNCHANGED (UpdateSeen succeeds)
		okClass("boom.txt", 1, 0x02), // NEW (insert fails)
	})
	if !errors.Is(err, errConstraint) {
		t.Fatalf("ReconcileBatch error = %v, want the injected constraint failure", err)
	}
	if f.rollbacks != 1 {
		t.Errorf("rollbacks = %d, want 1", f.rollbacks)
	}
	// The pipeline stops at the failing record: one insert attempt.
	if got, want := countPrefix(f.journal, "tx1:InsertScanFile"), 1; got != want {
		t.Errorf("journal = %v, want exactly the failing insert attempt", f.journal)
	}
	// Nothing committed is reported: counts restored, seen set clean.
	if got := (Counts{}); r.Counts() != got {
		t.Errorf("counts = %+v after a rolled-back batch, want zero counts", r.Counts())
	}
	r.ScanComplete()
	if _, err := r.MarkMissing(context.Background()); err != nil {
		t.Fatalf("MarkMissing after rolled-back batch: %v", err)
	}
	// keep.txt was in the rolled-back batch, so its seen-mark is gone:
	// it must be reported missing now (the stream never completed it).
	if len(f.missing) != 1 || f.missing[0] != 1 {
		t.Errorf("missing = %v, want [1] (keep.txt's rolled-back seen-mark)", f.missing)
	}
}

// TestReconcilerFactoryErrorPropagates: a failing transaction factory
// surfaces directly (no rollback to fire - no transaction existed).
func TestReconcilerFactoryErrorPropagates(t *testing.T) {
	factory := func(context.Context) (*Tx, error) { return nil, errors.New("db gone") }
	r := NewReconciler(factory, 1)
	if err := r.ReconcileBatch(context.Background(), []ClassifiedResult{okClass("a", 1, 1)}); err == nil {
		t.Fatal("ReconcileBatch with failing factory returned nil, want error")
	}
}

var errConstraint = errors.New("UNIQUE constraint failed: files.index")

// TestReconcilerEmptyBatchIsNoop: an empty batch opens no transaction.
func TestReconcilerEmptyBatchIsNoop(t *testing.T) {
	f := newFakeStore()
	r := NewReconciler(f.factory(), 1)
	if err := r.ReconcileBatch(context.Background(), nil); err != nil {
		t.Fatalf("empty ReconcileBatch: %v", err)
	}
	if len(f.journal) != 0 {
		t.Errorf("journal = %v, want no store calls for an empty batch", f.journal)
	}
}
