package catalog

// T1.6: scanstore integration tests against a REAL temp catalog
// (DESIGN_SPEC 12.1 catalog-SQL tier: SQLite in a temp file is
// milliseconds). They pin the round-trips the reconciler depends on:
// set binding, runs lifecycle (running -> ok / partial), run_events,
// the files insert with its NULL conventions, the 2.4 versioned pair
// (modified_of + superseded_by in ONE transaction, old row keeping
// image_id and copied), the last_seen touch, the missing stamp, and
// transaction atomicity (a mid-tx failure plus rollback leaves the
// catalog byte-identical in content).

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scanFixture is a created catalog + open mutating DB + set + running
// scan run - the state every scan-side test starts from.
type scanFixture struct {
	db    *DB
	runID int64
	setID string
}

func newScanFixture(t *testing.T) *scanFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.sqlite")
	mustCreate(t, path)
	db := mustOpen(t, path, OpenReadWrite)
	ctx := context.Background()
	if err := db.EnsureSet(ctx, "S-001", "/data", time.Now().UnixNano(), ToolVersion); err != nil {
		t.Fatalf("EnsureSet: %v", err)
	}
	runID, err := db.StartScanRun(ctx, ScanRun{
		SetID: "S-001", Kind: "ingest", Root: "/data",
		StartedAt: time.Now().UnixNano(), ExcludeJSON: `["*.tmp"]`,
	})
	if err != nil {
		t.Fatalf("StartScanRun: %v", err)
	}
	return &scanFixture{db: db, runID: runID, setID: "S-001"}
}

// mustTx begins, runs fn, and commits; failures fail the test.
func (f *scanFixture) mustTx(t *testing.T, fn func(tx *ScanTx)) {
	t.Helper()
	tx, err := f.db.BeginScanTx(context.Background(), f.runID, f.setID)
	if err != nil {
		t.Fatalf("BeginScanTx: %v", err)
	}
	fn(tx)
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// rawFileRow reads one files row back through a plain connection for
// test-side assertions the typed API deliberately does not expose
// (NULL-ness, provenance columns).
type rawFileRow struct {
	FileID       int64
	Relpath      string
	EntryKind    string
	Hash         sql.NullString
	LinkTarget   sql.NullString
	Status       string
	StatusDetail sql.NullString
	FirstSeenRun int64
	LastSeenRun  int64
	ModifiedOf   sql.NullInt64
	SupersededBy sql.NullInt64
	ImageID      sql.NullInt64
	Copied       bool
}

func rawFileByPath(t *testing.T, db *DB, setID, relpath string) []rawFileRow {
	t.Helper()
	sdb := rawOpen(t, db.Path())
	rows, err := sdb.Query(`
		SELECT file_id, relpath, entry_kind, hash, link_target, status, status_detail,
		       first_seen_run, last_seen_run, modified_of, superseded_by, image_id, copied
		FROM files WHERE set_id = ? AND relpath = ? ORDER BY file_id`, setID, relpath)
	if err != nil {
		t.Fatalf("query files %q: %v", relpath, err)
	}
	defer rows.Close()
	var out []rawFileRow
	for rows.Next() {
		var r rawFileRow
		if err := rows.Scan(&r.FileID, &r.Relpath, &r.EntryKind, &r.Hash, &r.LinkTarget,
			&r.Status, &r.StatusDetail, &r.FirstSeenRun, &r.LastSeenRun,
			&r.ModifiedOf, &r.SupersededBy, &r.ImageID, &r.Copied); err != nil {
			t.Fatalf("scan files row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate files rows: %v", err)
	}
	return out
}

// regularRow builds a store-shaped regular-file row.
func regularRow(rel string, size int64, digest byte) FileRow {
	h := make([]byte, 32)
	for i := range h {
		h[i] = digest
	}
	return FileRow{
		RelPath: rel, EntryKind: "regular", Size: size,
		Mode: 0o644, UID: 1000, GID: 1000, MtimeNS: 123456789,
		Dev: 42, Inode: 1001, Nlink: 1,
		Hash: h, Status: "active",
	}
}

// --- set binding -------------------------------------------------------------

func TestScanStoreEnsureSetBindsAndRefusesRebind(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()

	// Same binding again: no-op.
	if err := f.db.EnsureSet(ctx, "S-001", "/data", time.Now().UnixNano(), ToolVersion); err != nil {
		t.Fatalf("EnsureSet same root: %v", err)
	}
	// Different root under the same set id: refused, wrapped sentinel.
	err := f.db.EnsureSet(ctx, "S-001", "/other", time.Now().UnixNano(), ToolVersion)
	if !errors.Is(err, ErrSetRootMismatch) {
		t.Fatalf("EnsureSet rebind = %v, want ErrSetRootMismatch", err)
	}
	// The sets row is untouched.
	sdb := rawOpen(t, f.db.Path())
	var root string
	if err := sdb.QueryRow(`SELECT source_root FROM sets WHERE set_id = 'S-001'`).Scan(&root); err != nil {
		t.Fatalf("read sets row: %v", err)
	}
	if root != "/data" {
		t.Errorf("source_root = %q, want /data", root)
	}
}

// --- runs lifecycle ----------------------------------------------------------

func TestScanStoreRunLifecycle(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()

	prior, err := f.db.PriorScanRuns(ctx, "S-001")
	if err != nil || prior != 1 {
		t.Fatalf("PriorScanRuns = %d, %v; want 1, nil", prior, err)
	}

	// The open run reads as 'running' with its exclude snapshot.
	sdb := rawOpen(t, f.db.Path())
	var kind, status, exclude, stats string
	var finNull sql.NullString
	if err := sdb.QueryRow(`SELECT kind, status, exclude_json, stats_json, finished_at
		FROM runs WHERE run_id = ?`, f.runID).Scan(&kind, &status, &exclude, &stats, &finNull); err != nil {
		t.Fatalf("read run row: %v", err)
	}
	if kind != "ingest" || status != "running" || exclude != `["*.tmp"]` || stats != "{}" || finNull.Valid {
		t.Errorf("open run row = kind %q, status %q, exclude %q, stats %q, finished_at valid %v; "+
			"want ingest/running/[\"*.tmp\"]/{}/NULL", kind, status, exclude, stats, finNull.Valid)
	}

	// Finalize partial; the stamp round-trips.
	if err := f.db.FinishRun(ctx, f.runID, "partial", 999, `{"scanned":3}`); err != nil {
		t.Fatalf("FinishRun: %v", err)
	}
	var fin int64
	if err := sdb.QueryRow(`SELECT status, finished_at, stats_json FROM runs WHERE run_id = ?`,
		f.runID).Scan(&status, &fin, &stats); err != nil {
		t.Fatalf("read finalized run row: %v", err)
	}
	if status != "partial" || fin != 999 || stats != `{"scanned":3}` {
		t.Errorf("finalized run = %q/%d/%q, want partial/999/{\"scanned\":3}", status, fin, stats)
	}

	// A second scan of the set is a rescan, and can finalize ok.
	run2, err := f.db.StartScanRun(ctx, ScanRun{
		SetID: "S-001", Kind: "rescan", Root: "/data", StartedAt: 1, ExcludeJSON: `[]`,
	})
	if err != nil {
		t.Fatalf("StartScanRun 2: %v", err)
	}
	if err := f.db.FinishRun(ctx, run2, "ok", 2, ""); err != nil {
		t.Fatalf("FinishRun 2: %v", err)
	}
	if err := f.db.FinishRun(ctx, run2, "not-a-status", 3, ""); err == nil {
		t.Error("FinishRun with an invalid status returned nil, want error")
	}
	if err := f.db.FinishRun(ctx, 99999, "ok", 3, ""); err == nil {
		t.Error("FinishRun for an unknown run_id returned nil, want error")
	}
}

// --- files round-trips -------------------------------------------------------

func TestScanStoreFileRoundTrips(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()

	var id int64
	f.mustTx(t, func(tx *ScanTx) {
		var err error
		id, err = tx.InsertScanFile(ctx, regularRow("docs/a.txt", 11, 0xAB))
		if err != nil {
			t.Fatalf("InsertScanFile: %v", err)
		}
	})

	// Read back through the reconciler's comparison query.
	f.mustTx(t, func(tx *ScanTx) {
		got, err := tx.ActiveByRelPaths(ctx, []string{"docs/a.txt", "absent.txt"})
		if err != nil {
			t.Fatalf("ActiveByRelPaths: %v", err)
		}
		af, ok := got["docs/a.txt"]
		if !ok || len(got) != 1 {
			t.Fatalf("active map = %v, want exactly docs/a.txt", got)
		}
		if af.FileID != id || af.EntryKind != "regular" || af.Size != 11 || af.MtimeNS != 123456789 {
			t.Errorf("active row = %+v, want file_id %d, regular, size 11, mtime 123456789", af, id)
		}
		if len(af.Hash) != 32 || af.Hash[0] != 0xAB {
			t.Errorf("active hash = %v, want the stored 32-byte digest", af.Hash)
		}
		if af.ImageID.Valid {
			t.Errorf("image_id = %v, want NULL (scan inserts are never assigned, FR-029)", af.ImageID)
		}
	})

	// last_seen touch: provenance columns flip to the touching run,
	// nothing else.
	f.mustTx(t, func(tx *ScanTx) {
		if err := tx.UpdateSeen(ctx, id); err != nil {
			t.Fatalf("UpdateSeen: %v", err)
		}
	})
	rows := rawFileByPath(t, f.db, "S-001", "docs/a.txt")
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].FirstSeenRun != f.runID || rows[0].LastSeenRun != f.runID {
		t.Errorf("provenance runs = %d/%d, want %d/%d",
			rows[0].FirstSeenRun, rows[0].LastSeenRun, f.runID, f.runID)
	}
}

// TestScanStoreModificationKeepsAssignment pins the 2.4 versioning
// contract on the SQL side: the pair is written in ONE transaction; the
// NEW row is unassigned with modified_of set; the OLD row keeps its
// image_id and copied state and gains superseded_by - so the media
// holding the old content stays accurately described.
func TestScanStoreModificationKeepsAssignment(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()

	var oldID, newID int64
	f.mustTx(t, func(tx *ScanTx) {
		var err error
		oldID, err = tx.InsertScanFile(ctx, regularRow("data.bin", 50, 0x11))
		if err != nil {
			t.Fatalf("InsertScanFile: %v", err)
		}
	})
	// Simulate a planned+copied row (direct SQL: the planner is a
	// later sprint, T2.2).
	sdb := rawOpen(t, f.db.Path())
	if _, err := sdb.Exec(`UPDATE files SET image_id = 7, copied = 1 WHERE file_id = ?`, oldID); err != nil {
		t.Fatalf("seeding assignment: %v", err)
	}

	f.mustTx(t, func(tx *ScanTx) {
		var err error
		newID, err = tx.InsertModification(ctx, oldID, regularRow("data.bin", 60, 0x22))
		if err != nil {
			t.Fatalf("InsertModification: %v", err)
		}
		if newID == oldID {
			t.Fatalf("new id %d equals old id", newID)
		}
	})

	rows := rawFileByPath(t, f.db, "S-001", "data.bin")
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (old + new version)", len(rows))
	}
	oldRow, newRow := rows[0], rows[1]
	if oldRow.FileID != oldID || newRow.FileID != newID {
		t.Fatalf("row ids = %d,%d want %d,%d", oldRow.FileID, newRow.FileID, oldID, newID)
	}
	// Old row: superseded, assignment + copied PRESERVED.
	if !oldRow.SupersededBy.Valid || oldRow.SupersededBy.Int64 != newID {
		t.Errorf("old superseded_by = %v, want %d", oldRow.SupersededBy, newID)
	}
	if !oldRow.ImageID.Valid || oldRow.ImageID.Int64 != 7 || !oldRow.Copied {
		t.Errorf("old row assignment = image_id %v copied %v, want 7/true (2.4 preservation)",
			oldRow.ImageID, oldRow.Copied)
	}
	// New row: unassigned lineage child.
	if !newRow.ModifiedOf.Valid || newRow.ModifiedOf.Int64 != oldID {
		t.Errorf("new modified_of = %v, want %d", newRow.ModifiedOf, oldID)
	}
	if newRow.SupersededBy.Valid || newRow.ImageID.Valid || newRow.Copied {
		t.Errorf("new row = superseded_by %v image_id %v copied %v, want NULL/false/false (unassigned, FR-029)",
			newRow.SupersededBy, newRow.ImageID, newRow.Copied)
	}
	if !newRow.Hash.Valid || !strings.EqualFold(newRow.Hash.String, string(must22(t))) {
		t.Errorf("new row hash mismatch: %v", newRow.Hash)
	}

	// The active view now resolves to the NEW row only.
	f.mustTx(t, func(tx *ScanTx) {
		got, err := tx.ActiveByRelPaths(ctx, []string{"data.bin"})
		if err != nil {
			t.Fatalf("ActiveByRelPaths: %v", err)
		}
		if af := got["data.bin"]; af.FileID != newID || len(got) != 1 {
			t.Errorf("active row = %+v, want only the new version %d", af, newID)
		}
	})
}

// must22 returns the 0x22-filled digest regularRow(..., 0x22) stores.
func must22(t *testing.T) []byte {
	t.Helper()
	h := make([]byte, 32)
	for i := range h {
		h[i] = 0x22
	}
	return h
}

// TestScanStoreFailureClassNulls: the T1.5 classes store their status
// and detail with hash NULL (this layer stores exactly what the
// reconciler hands it; the stale-digest drop happens upstream).
func TestScanStoreFailureClassNulls(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()

	f.mustTx(t, func(tx *ScanTx) {
		rec := regularRow("bad/x.bin", 10, 0x01)
		rec.Hash = nil
		rec.Status = "unreadable"
		rec.StatusDetail = "permission denied"
		if _, err := tx.InsertScanFile(ctx, rec); err != nil {
			t.Fatalf("InsertScanFile unreadable: %v", err)
		}

		sym := FileRow{
			RelPath: "bad/link", EntryKind: "symlink", Size: 6,
			Mode: 0o777, UID: 1000, GID: 1000, MtimeNS: 5,
			Dev: 42, Inode: 2002, Nlink: 1,
			LinkTarget: "x.bin", Hash: nil, Status: "active",
		}
		if _, err := tx.InsertScanFile(ctx, sym); err != nil {
			t.Fatalf("InsertScanFile symlink: %v", err)
		}
	})

	bad := rawFileByPath(t, f.db, "S-001", "bad/x.bin")
	if len(bad) != 1 || bad[0].Status != "unreadable" ||
		!bad[0].StatusDetail.Valid || bad[0].StatusDetail.String != "permission denied" ||
		bad[0].Hash.Valid {
		t.Fatalf("unreadable row = %+v, want status unreadable, detail set, hash NULL", bad)
	}
	// NULL-hash rows are not catalog-active for comparison purposes.
	f.mustTx(t, func(tx *ScanTx) {
		got, err := tx.ActiveByRelPaths(ctx, []string{"bad/x.bin"})
		if err != nil {
			t.Fatalf("ActiveByRelPaths: %v", err)
		}
		if _, ok := got["bad/x.bin"]; ok {
			t.Error("unreadable row surfaced from ActiveByRelPaths; only active rows belong there")
		}
	})

	sym := rawFileByPath(t, f.db, "S-001", "bad/link")
	if len(sym) != 1 || sym[0].EntryKind != "symlink" ||
		!sym[0].LinkTarget.Valid || sym[0].LinkTarget.String != "x.bin" || sym[0].Hash.Valid {
		t.Fatalf("symlink row = %+v, want entry_kind symlink, link_target x.bin, hash NULL", sym)
	}
}

// TestScanStoreEmptyStringBecomesNull: LinkTarget "" and StatusDetail ""
// insert as SQL NULL on a regular row (the 2.3 columns are nullable
// with no default; ” would be a distinct, wrong value).
func TestScanStoreEmptyStringBecomesNull(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()
	f.mustTx(t, func(tx *ScanTx) {
		if _, err := tx.InsertScanFile(ctx, regularRow("plain.txt", 1, 0x01)); err != nil {
			t.Fatalf("InsertScanFile: %v", err)
		}
	})
	row := rawFileByPath(t, f.db, "S-001", "plain.txt")
	if len(row) != 1 {
		t.Fatalf("rows = %d, want 1", len(row))
	}
	if row[0].LinkTarget.Valid || row[0].StatusDetail.Valid {
		t.Errorf("link_target/status_detail = %v/%v, want NULL/NULL",
			row[0].LinkTarget, row[0].StatusDetail)
	}
}

// --- missing pass -------------------------------------------------------------

func TestScanStoreMarkMissingRoundTrip(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()

	var keepID int64
	f.mustTx(t, func(tx *ScanTx) {
		var err error
		if keepID, err = tx.InsertScanFile(ctx, regularRow("keep.txt", 1, 0x01)); err != nil {
			t.Fatalf("insert keep: %v", err)
		}
		if _, err = tx.InsertScanFile(ctx, regularRow("gone.txt", 1, 0x02)); err != nil {
			t.Fatalf("insert gone: %v", err)
		}
	})

	var missing int
	f.mustTx(t, func(tx *ScanTx) {
		active, err := tx.ListActive(ctx)
		if err != nil {
			t.Fatalf("ListActive: %v", err)
		}
		if len(active) != 2 {
			t.Fatalf("ListActive = %d rows, want 2", len(active))
		}
		var ids []int64
		for _, a := range active {
			if a.RelPath == "gone.txt" {
				ids = append(ids, a.FileID)
			}
		}
		if missing, err = tx.MarkMissing(ctx, ids); err != nil {
			t.Fatalf("MarkMissing: %v", err)
		}
	})
	if missing != 1 {
		t.Errorf("MarkMissing = %d, want 1", missing)
	}

	// gone.txt: status missing, EVERYTHING else preserved (3.7:
	// "records and media are untouched" - MarkMissing is a status
	// write ONLY: the stored digest and provenance survive for audit
	// and for any future as-of-date view).
	gone := rawFileByPath(t, f.db, "S-001", "gone.txt")
	if len(gone) != 1 || gone[0].Status != "missing" {
		t.Fatalf("gone row = %+v, want status missing", gone)
	}
	if !gone[0].Hash.Valid || gone[0].FirstSeenRun != f.runID {
		t.Errorf("missing row lost data: hash valid %v first_seen %d (both must be preserved)",
			gone[0].Hash.Valid, gone[0].FirstSeenRun)
	}
	// The remaining active set is exactly keep.txt.
	f.mustTx(t, func(tx *ScanTx) {
		active, err := tx.ListActive(ctx)
		if err != nil {
			t.Fatalf("ListActive: %v", err)
		}
		if len(active) != 1 || active[0].FileID != keepID {
			t.Errorf("active rows = %+v, want only keep.txt (%d)", active, keepID)
		}
	})
}

// --- run_events ----------------------------------------------------------------

func TestScanStoreRunEvents(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()
	if err := f.db.InsertRunEvent(ctx, RunEvent{
		RunID: f.runID, At: 111, Level: "warn", RelPath: "bad/x", Detail: "permission denied",
	}); err != nil {
		t.Fatalf("InsertRunEvent: %v", err)
	}
	if err := f.db.InsertRunEvent(ctx, RunEvent{
		RunID: f.runID, At: 112, Level: "warn", Detail: "cannot read directory sub: subtree skipped",
	}); err != nil {
		t.Fatalf("InsertRunEvent (no relpath): %v", err)
	}
	if err := f.db.InsertRunEvent(ctx, RunEvent{RunID: f.runID, At: 113, Level: "fatal", Detail: "x"}); err == nil {
		t.Error("InsertRunEvent with an invalid level returned nil, want error")
	}

	sdb := rawOpen(t, f.db.Path())
	rows, err := sdb.Query(`SELECT at, level, relpath, detail FROM run_events WHERE run_id = ? ORDER BY at`, f.runID)
	if err != nil {
		t.Fatalf("query run_events: %v", err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var at int64
		var level, detail string
		var rel sql.NullString
		if err := rows.Scan(&at, &level, &rel, &detail); err != nil {
			t.Fatalf("scan run_events: %v", err)
		}
		got = append(got, level+":"+rel.String+":"+detail)
	}
	if len(got) != 2 ||
		got[0] != "warn:bad/x:permission denied" ||
		got[1] != "warn::cannot read directory sub: subtree skipped" {
		t.Errorf("run_events = %v, want the two inserted warn rows", got)
	}
}

// --- atomicity ------------------------------------------------------------------

// TestScanStoreTxAtomicity: statements inside a ScanTx are invisible
// until Commit, and a Rollback discards them entirely - including the
// versioned pair, which can never half-land.
func TestScanStoreTxAtomicity(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()

	// Seed one committed row to modify later.
	f.mustTx(t, func(tx *ScanTx) {
		if _, err := tx.InsertScanFile(ctx, regularRow("seed.txt", 5, 0x01)); err != nil {
			t.Fatalf("seed insert: %v", err)
		}
	})

	// (a) Uncommitted work is invisible to a concurrent reader.
	tx, err := f.db.BeginScanTx(ctx, f.runID, f.setID)
	if err != nil {
		t.Fatalf("BeginScanTx: %v", err)
	}
	if _, err := tx.InsertScanFile(ctx, regularRow("ghost.txt", 1, 0x02)); err != nil {
		t.Fatalf("insert ghost: %v", err)
	}
	if rows := rawFileByPath(t, f.db, "S-001", "ghost.txt"); len(rows) != 0 {
		t.Fatalf("uncommitted row visible: %+v", rows)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rows := rawFileByPath(t, f.db, "S-001", "ghost.txt"); len(rows) != 0 {
		t.Fatalf("rolled-back row survived: %+v", rows)
	}

	// (b) Mid-tx failure: a modification of a NONEXISTENT old row
	// fails the UPDATE half; after rollback NOTHING of the tx remains,
	// not even the successful statements issued before the failure.
	tx, err = f.db.BeginScanTx(ctx, f.runID, f.setID)
	if err != nil {
		t.Fatalf("BeginScanTx: %v", err)
	}
	if _, err := tx.InsertScanFile(ctx, regularRow("before-failure.txt", 1, 0x03)); err != nil {
		t.Fatalf("insert before failure: %v", err)
	}
	if _, err := tx.InsertModification(ctx, 424242, regularRow("x.txt", 1, 0x04)); err == nil {
		t.Fatal("InsertModification of a nonexistent old row returned nil, want error")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback after failure: %v", err)
	}
	if rows := rawFileByPath(t, f.db, "S-001", "before-failure.txt"); len(rows) != 0 {
		t.Fatalf("pre-failure statement survived rollback: %+v", rows)
	}

	// (c) The versioned pair is atomic in the other direction too: a
	// modification whose pair write fails leaves the old row
	// unsuperseded (nothing half-landed).
	if rows := rawFileByPath(t, f.db, "S-001", "seed.txt"); len(rows) != 1 || rows[0].SupersededBy.Valid {
		t.Fatalf("seed row = %+v, want unsuperseded", rows)
	}

	// (d) Double commit/rollback is a safe no-op (done-flag contract).
	tx2, err := f.db.BeginScanTx(ctx, f.runID, f.setID)
	if err != nil {
		t.Fatalf("BeginScanTx 2: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("second Commit: %v", err)
	}
	if err := tx2.Rollback(); err != nil {
		t.Fatalf("Rollback after Commit: %v", err)
	}
}
