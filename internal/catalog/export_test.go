package catalog

// T3.2: export tests (DESIGN_SPEC 12.1 catalog-SQL tier - a real temp
// catalog, the real scan/plan store functions, SQLite in a temp file).
// They pin the ExportSnapshot contract of export.go:
//
//   - the export of a planned set opens read-only (the 8.5
//     restore--from-media path) and passes integrity_check;
//   - the FULL PLACEMENT of the set survives the round trip
//     (files.image_id, chunks.image_id, directories.image_id, the
//     images rows still in state 'planned', image_contents rows), so
//     FR-034's "sufficient to restore the entire set" holds for
//     images not yet built;
//   - set scoping - the (b) decision of export.go: a two-set catalog
//     exports ONLY the requested set's rows; the other set never leaks
//     into the export, and the export is still FK-closed;
//   - the caller contract: existing destination is refused untouched;
//     context cancellation leaves NO artifacts (no destPath, no
//     scratch);
//   - unique per-call scratch (the QA-hardening follow-up): concurrent
//     ExportSnapshot calls to the same destPath can never share
//     scratch paths, so contention can never unlink a live staging
//     file or publish an EMPTY export under err=nil, and no scratch
//     or WAL/SHM sidecar survives any interleaving;
//   - snapshot consistency: an export taken while another connection
//     holds an uncommitted write sees exactly the committed state -
//     never the in-flight transaction, never a torn view of it.

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- fixture -----------------------------------------------------------------

// exportFixture is a catalog with TWO planned sets: S-001 (the set
// under export) carrying files, a chunked file, a symlink, an empty
// dir, two images, seeded image_contents rows, and finished runs with
// events; and S-002 (the OTHER set) carrying its own file, image,
// content row, and run event. The placement of S-001 mirrors the
// planner flow of DESIGN_SPEC 4.5: EnsureChunkRows + StoreChunkSize in
// one PlanTx, then AssignFile / AssignChunk / AssignDir.
type exportFixture struct {
	db        *DB
	catPath   string
	run1      int64 // ingest run of S-001
	run2      int64 // plan run of S-001
	aFile     int64 // a.txt, assigned to image 1
	bigFile   int64 // big.bin, chunked across images 1, 1, 2
	dirID     int64 // emptydir, assigned to image 1
	otherFile int64 // secret.txt of S-002 (the set that must not leak)
}

// smallSpecs is big.bin's 300-byte decomposition: chunks 100/100/100.
func smallSpecs() []ChunkSpec {
	return []ChunkSpec{
		{Index: 0, Offset: 0, Length: 100},
		{Index: 1, Offset: 100, Length: 100},
		{Index: 2, Offset: 200, Length: 100},
	}
}

func seedExportFixture(t *testing.T) *exportFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.sqlite")
	mustCreate(t, path)
	db := mustOpen(t, path, OpenReadWrite)
	ctx := context.Background()
	f := &exportFixture{db: db, catPath: path}

	// --- S-001: ingest ------------------------------------------------------
	if err := db.EnsureSet(ctx, "S-001", "/data", time.Now().UnixNano(), ToolVersion); err != nil {
		t.Fatalf("EnsureSet(S-001): %v", err)
	}
	run1, err := db.StartScanRun(ctx, ScanRun{
		SetID: "S-001", Kind: "ingest", Root: "/data",
		StartedAt: time.Now().UnixNano(), ExcludeJSON: `["*.tmp"]`,
	})
	if err != nil {
		t.Fatalf("StartScanRun(S-001): %v", err)
	}
	f.run1 = run1
	if err := db.InsertRunEvent(ctx, RunEvent{
		RunID: run1, At: 1, Level: "info", Detail: "seed event",
	}); err != nil {
		t.Fatalf("InsertRunEvent: %v", err)
	}

	stx, err := db.BeginScanTx(ctx, run1, "S-001")
	if err != nil {
		t.Fatalf("BeginScanTx(S-001): %v", err)
	}
	if f.aFile, err = stx.InsertScanFile(ctx, regularRow("a.txt", 1000, 0xAA)); err != nil {
		t.Fatalf("InsertScanFile(a.txt): %v", err)
	}
	if f.bigFile, err = stx.InsertScanFile(ctx, regularRow("big.bin", 300, 0xBB)); err != nil {
		t.Fatalf("InsertScanFile(big.bin): %v", err)
	}
	symlink := regularRow("link", 10, 0x00)
	symlink.EntryKind = "symlink"
	symlink.Hash = nil
	symlink.LinkTarget = "/data/a.txt"
	symlink.Size = int64(len(symlink.LinkTarget))
	symlink.Mode = 0o777
	if _, err := stx.InsertScanFile(ctx, symlink); err != nil {
		t.Fatalf("InsertScanFile(link): %v", err)
	}
	if err := stx.UpsertEmptyDir(ctx, DirRow{
		RelPath: "emptydir", Mode: 0o755, UID: 1000, GID: 1000, MtimeNS: 99,
	}); err != nil {
		t.Fatalf("UpsertEmptyDir: %v", err)
	}
	if err := stx.Commit(); err != nil {
		t.Fatalf("commit S-001 scan tx: %v", err)
	}
	if err := db.FinishRun(ctx, run1, "ok", 2, `{"files":3}`); err != nil {
		t.Fatalf("FinishRun(run1): %v", err)
	}
	if err := db.sql.QueryRow(`SELECT dir_id FROM directories WHERE set_id = 'S-001'`).
		Scan(&f.dirID); err != nil {
		t.Fatalf("reading dir_id: %v", err)
	}

	// --- S-001: plan (two images; big.bin chunked; placement everywhere) ---
	run2, err := db.StartScanRun(ctx, ScanRun{
		SetID: "S-001", Kind: "plan", Root: "/data",
		StartedAt: time.Now().UnixNano(),
	})
	if err != nil {
		t.Fatalf("StartScanRun(plan): %v", err)
	}
	f.run2 = run2
	ptx, err := db.BeginPlanTx(ctx, run2, "S-001")
	if err != nil {
		t.Fatalf("BeginPlanTx(S-001): %v", err)
	}
	for _, img := range []ImageSpec{
		{ImageID: 1, NominalSize: 25_000_000_000, UsableSize: 23_000_000_000, MediaProfile: "bd-r-sl"},
		{ImageID: 2, NominalSize: 50_000_000_000, UsableSize: 46_000_000_000, MediaProfile: "bd-r-dl"},
	} {
		if err := ptx.InsertImage(ctx, img); err != nil {
			t.Fatalf("InsertImage(%d): %v", img.ImageID, err)
		}
	}
	if err := ptx.EnsureChunkRows(ctx, f.bigFile, smallSpecs()); err != nil {
		t.Fatalf("EnsureChunkRows: %v", err)
	}
	if err := ptx.StoreChunkSize(ctx, f.bigFile, 100, 3); err != nil {
		t.Fatalf("StoreChunkSize: %v", err)
	}
	if err := ptx.AssignFile(ctx, f.aFile, 1); err != nil {
		t.Fatalf("AssignFile: %v", err)
	}
	for idx, img := range []int64{1, 1, 2} {
		if err := ptx.AssignChunk(ctx, f.bigFile, idx, img); err != nil {
			t.Fatalf("AssignChunk(%d): %v", idx, err)
		}
	}
	if err := ptx.AssignDir(ctx, f.dirID, 1); err != nil {
		t.Fatalf("AssignDir: %v", err)
	}
	if err := ptx.Commit(); err != nil {
		t.Fatalf("commit S-001 plan tx: %v", err)
	}
	if err := db.FinishRun(ctx, run2, "ok", 3, "{}"); err != nil {
		t.Fatalf("FinishRun(run2): %v", err)
	}

	// --- S-001: seeded image_contents rows (the builder writes these at
	// finalize time in a later sprint; direct SQL here, per the T3.2
	// brief) ------------------------------------------------------------
	for _, row := range []struct {
		mediaPath string
		kind      string
		fileID    sql.NullInt64
		chunkIdx  sql.NullInt64
		size      int64
	}{
		{"files/a.txt", "file", sql.NullInt64{Int64: f.aFile, Valid: true}, sql.NullInt64{}, 1000},
		{"chunks/big.bin.000", "chunk", sql.NullInt64{Int64: f.bigFile, Valid: true}, sql.NullInt64{Int64: 0, Valid: true}, 100},
		{"MANIFEST.json", "manifest", sql.NullInt64{}, sql.NullInt64{}, 10},
		{"catalog-export.sqlite", "export", sql.NullInt64{}, sql.NullInt64{}, 10},
	} {
		if _, err := db.sql.Exec(
			`INSERT INTO image_contents (set_id, image_id, media_path, kind, file_id, chunk_index, size, hash)
			 VALUES ('S-001', 1, ?, ?, ?, ?, ?, NULL)`,
			row.mediaPath, row.kind, row.fileID, row.chunkIdx, row.size); err != nil {
			t.Fatalf("seeding image_contents %s: %v", row.mediaPath, err)
		}
	}

	// --- S-002: the OTHER set, fully planned - must never leak -------------
	if err := db.EnsureSet(ctx, "S-002", "/other", time.Now().UnixNano(), ToolVersion); err != nil {
		t.Fatalf("EnsureSet(S-002): %v", err)
	}
	runB, err := db.StartScanRun(ctx, ScanRun{
		SetID: "S-002", Kind: "ingest", Root: "/other",
		StartedAt: time.Now().UnixNano(),
	})
	if err != nil {
		t.Fatalf("StartScanRun(S-002): %v", err)
	}
	otherTx, err := db.BeginScanTx(ctx, runB, "S-002")
	if err != nil {
		t.Fatalf("BeginScanTx(S-002): %v", err)
	}
	var otherFile int64
	if otherFile, err = otherTx.InsertScanFile(ctx, regularRow("secret.txt", 42, 0x77)); err != nil {
		t.Fatalf("InsertScanFile(secret.txt): %v", err)
	}
	f.otherFile = otherFile
	if err := otherTx.Commit(); err != nil {
		t.Fatalf("commit S-002 scan tx: %v", err)
	}
	if err := db.InsertRunEvent(ctx, RunEvent{
		RunID: runB, At: 1, Level: "warn", Detail: "other-set event",
	}); err != nil {
		t.Fatalf("InsertRunEvent(S-002): %v", err)
	}
	otherPlan, err := db.BeginPlanTx(ctx, runB, "S-002")
	if err != nil {
		t.Fatalf("BeginPlanTx(S-002): %v", err)
	}
	if err := otherPlan.InsertImage(ctx, ImageSpec{
		ImageID: 3, NominalSize: 25_000_000_000, UsableSize: 23_000_000_000,
		MediaProfile: "bd-r-sl",
	}); err != nil {
		t.Fatalf("InsertImage(S-002): %v", err)
	}
	if err := otherPlan.AssignFile(ctx, otherFile, 3); err != nil {
		t.Fatalf("AssignFile(S-002): %v", err)
	}
	if err := otherPlan.Commit(); err != nil {
		t.Fatalf("commit S-002 plan tx: %v", err)
	}
	if _, err := db.sql.Exec(
		`INSERT INTO image_contents (set_id, image_id, media_path, kind, file_id, chunk_index, size, hash)
		 VALUES ('S-002', 3, 'files/secret.txt', 'file', ?, NULL, 42, NULL)`,
		otherFile); err != nil {
		t.Fatalf("seeding S-002 image_contents: %v", err)
	}

	return f
}

// --- helpers -----------------------------------------------------------------

// openExport exports setID to a fresh temp destination and opens the
// result read-only, the same way restore --from-media will (8.5).
func openExport(t *testing.T, db *DB, setID string) (*DB, string, int64) {
	t.Helper()
	return openExportAt(t, db, setID, filepath.Join(t.TempDir(), "catalog-export.sqlite"))
}

// openExportAt is openExport with a caller-chosen destination path (for
// tests that seed scratch artifacts next to the destination).
func openExportAt(t *testing.T, db *DB, setID, dest string) (*DB, string, int64) {
	t.Helper()
	size, err := db.ExportSnapshot(context.Background(), setID, dest)
	if err != nil {
		t.Fatalf("ExportSnapshot(%q): %v", setID, err)
	}
	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat export: %v", err)
	}
	if fi.Size() != size {
		t.Fatalf("ExportSnapshot returned size %d, stat says %d", size, fi.Size())
	}
	if size == 0 {
		t.Fatal("ExportSnapshot returned size 0")
	}
	return mustOpen(t, dest, OpenReadOnly), dest, size
}

// noScratchInDir fails the test if any export scratch artifact
// (.staging / .partial, including WAL sidecars) remains in dir.
func noScratchInDir(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.ReadDir(dir); err != nil {
		t.Fatalf("read dir %s: %v", dir, err)
	}
	if leftovers := scratchArtifactsIn(dir); len(leftovers) > 0 {
		for _, name := range leftovers {
			t.Errorf("scratch artifact left behind: %s", name)
		}
	}
}

// scratchArtifactsIn lists export scratch artifacts remaining in dir:
// any name containing the ".staging" / ".partial" PREFIXES, which
// match the per-call scratch names (destPath+".staging-<pid>-<rand>" /
// ".partial-<pid>-<rand>") as well as their -wal/-shm sidecars.
func scratchArtifactsIn(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if strings.Contains(e.Name(), exportStagingSuffix) || strings.Contains(e.Name(), exportPartialSuffix) {
			out = append(out, e.Name())
		}
	}
	return out
}

// rowsOf runs a single-column query and returns the values.
func rowsOf(t *testing.T, db *DB, query string, args ...any) []string {
	t.Helper()
	rows, err := db.sql.Query(query, args...)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan %q: %v", query, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate %q: %v", query, err)
	}
	return out
}

// assertNoForeignKeyViolations drains PRAGMA foreign_key_check.
func assertNoForeignKeyViolations(t *testing.T, db *DB) {
	t.Helper()
	rows, err := db.sql.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var table, parent sql.NullString
		var rowid, fkid sql.NullInt64
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			t.Fatalf("scan foreign_key_check: %v", err)
		}
		t.Logf("violation: table %v rowid %v -> %v", table, rowid, parent)
		n++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate foreign_key_check: %v", err)
	}
	if n != 0 {
		t.Fatalf("foreign_key_check reported %d violations, want 0", n)
	}
}

// --- tests -------------------------------------------------------------------

// The core acceptance test of T3.2: a planned two-set catalog exports
// ONLY the requested set; the export opens read-only, is integrity-
// valid and FK-closed, and carries the FULL placement of the set
// (files/chunks/directories image_id values, planned images rows,
// image_contents, run provenance) - i.e. it is sufficient to restore
// the entire set without the original database (FR-026, FR-034).
func TestExportSnapshotSetScopedRoundTrip(t *testing.T) {
	f := seedExportFixture(t)
	export, dest, _ := openExport(t, f.db, "S-001")
	t.Cleanup(func() { export.Close() })

	// The publish contract left no scratch behind.
	noScratchInDir(t, filepath.Dir(dest))

	// Opens read-only (8.5) and passes the consistency gate.
	if got := integrityCheck(t, export.sql); got != "ok" {
		t.Fatalf("integrity_check = %q, want ok", got)
	}
	assertNoForeignKeyViolations(t, export)
	version, err := export.SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != SchemaVersion {
		t.Fatalf("export user_version = %d, want %d", version, SchemaVersion)
	}
	wantObjects(t, export.sql, v1Objects())

	// --- set scoping: the OTHER set never leaks ------------------------------
	if got := rowsOf(t, export, `SELECT set_id FROM sets`); !equalStrings(got, []string{"S-001"}) {
		t.Fatalf("sets = %v, want exactly [S-001]", got)
	}
	for _, table := range []string{"runs", "files", "directories", "chunks", "images", "image_contents"} {
		var n int
		if err := export.sql.QueryRow(
			fmt.Sprintf(`SELECT count(*) FROM %s WHERE set_id <> 'S-001'`, table)).Scan(&n); err != nil {
			t.Fatalf("count foreign rows in %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s contains %d rows of other sets, want 0", table, n)
		}
	}
	if got := rowsOf(t, export, `SELECT relpath FROM files`); equalStrings(got, []string{"secret.txt"}) ||
		containsString(got, "secret.txt") {
		t.Fatalf("files leaked the other set's rows: %v", got)
	}
	if got := rowsOf(t, export,
		`SELECT detail FROM run_events WHERE run_id NOT IN (SELECT run_id FROM runs WHERE set_id = 'S-001')`); len(got) != 0 {
		t.Fatalf("run_events leaked other-set events: %v", got)
	}
	if got := rowsOf(t, export,
		`SELECT media_path FROM image_contents WHERE set_id <> 'S-001'`); len(got) != 0 {
		t.Fatalf("image_contents leaked other-set rows: %v", got)
	}

	// --- the set's rows are ALL present -------------------------------------
	wantFiles := []string{"a.txt", "big.bin", "link"}
	gotFiles := rowsOf(t, export, `SELECT relpath FROM files WHERE set_id = 'S-001' ORDER BY relpath`)
	if !equalStrings(gotFiles, wantFiles) {
		t.Fatalf("files = %v, want %v", gotFiles, wantFiles)
	}
	if got := rowsOf(t, export, `SELECT relpath FROM v_active_files ORDER BY relpath`); !equalStrings(got, wantFiles) {
		t.Fatalf("v_active_files = %v, want %v", got, wantFiles)
	}
	if n := len(rowsOf(t, export, `SELECT run_id FROM runs WHERE set_id = 'S-001'`)); n != 2 {
		t.Fatalf("runs = %d rows, want 2 (ingest + plan)", n)
	}

	// --- FULL PLACEMENT round trip (the FR-034 core) -------------------------
	// files.image_id / chunks.image_id / directories.image_id equal the
	// source values, row for row.
	placement := func(db *DB) string {
		t.Helper()
		var b strings.Builder
		fmt.Fprintf(&b, "files:")
		for _, r := range rowsOf(t, db, `SELECT relpath || '=' || COALESCE(CAST(image_id AS TEXT), 'NULL')
			FROM files WHERE set_id = 'S-001' ORDER BY relpath`) {
			fmt.Fprintf(&b, " %s", r)
		}
		fmt.Fprintf(&b, "|chunks:")
		for _, r := range rowsOf(t, db, `SELECT CAST(chunk_index AS TEXT) || '=' || COALESCE(CAST(image_id AS TEXT), 'NULL')
			FROM chunks WHERE set_id = 'S-001' ORDER BY chunk_index`) {
			fmt.Fprintf(&b, " %s", r)
		}
		fmt.Fprintf(&b, "|dirs:")
		for _, r := range rowsOf(t, db, `SELECT relpath || '=' || COALESCE(CAST(image_id AS TEXT), 'NULL')
			FROM directories WHERE set_id = 'S-001' ORDER BY relpath`) {
			fmt.Fprintf(&b, " %s", r)
		}
		return b.String()
	}
	if src, exp := placement(f.db), placement(export); src != exp {
		t.Fatalf("placement mismatch:\n source: %s\n export: %s", src, exp)
	}
	// Explicit expected placement, so a both-sides-wrong bug cannot pass.
	wantPlacement := "files: a.txt=1 big.bin=NULL link=NULL|chunks: 0=1 1=1 2=2|dirs: emptydir=1"
	if got := placement(export); got != wantPlacement {
		t.Fatalf("placement = %q, want %q", got, wantPlacement)
	}
	// big.bin's chunk decomposition survives (offsets, lengths, order).
	for i, spec := range smallSpecs() {
		var offset, length int64
		if err := export.sql.QueryRow(
			`SELECT offset, length FROM chunks WHERE set_id = 'S-001' AND chunk_index = ?`, i).
			Scan(&offset, &length); err != nil {
			t.Fatalf("chunk %d decomposition: %v", i, err)
		}
		if offset != spec.Offset || length != spec.Length {
			t.Errorf("chunk %d = offset %d length %d, want offset %d length %d",
				i, offset, length, spec.Offset, spec.Length)
		}
	}
	// The images rows are present and STILL 'planned' - the export was
	// taken after planning, before any build (6.6: "even for images not
	// yet built").
	for _, img := range []struct {
		id    int64
		state string
	}{{1, "planned"}, {2, "planned"}} {
		var state string
		if err := export.sql.QueryRow(
			`SELECT state FROM images WHERE set_id = 'S-001' AND image_id = ?`, img.id).
			Scan(&state); err != nil {
			t.Fatalf("images row %d: %v", img.id, err)
		}
		if state != img.state {
			t.Errorf("image %d state = %q, want %q", img.id, state, img.state)
		}
	}
	// Every assignment references an images row OF THE EXPORT: the
	// placement is self-contained, no dangling image_id.
	for _, table := range []string{"files", "chunks", "directories"} {
		var dangling int
		q := fmt.Sprintf(`SELECT count(*) FROM %s t
			WHERE t.set_id = 'S-001' AND t.image_id IS NOT NULL
			  AND NOT EXISTS (SELECT 1 FROM images i
			                  WHERE i.set_id = t.set_id AND i.image_id = t.image_id)`, table)
		if err := export.sql.QueryRow(q).Scan(&dangling); err != nil {
			t.Fatalf("dangling image_id check on %s: %v", table, err)
		}
		if dangling != 0 {
			t.Errorf("%s has %d assignments referencing no images row of the export", table, dangling)
		}
	}
	// The seeded finalized content lists survive (FR-020 rows; the
	// 'export' kind row is the one the builder will add for this very
	// file at build time).
	gotContents := rowsOf(t, export,
		`SELECT media_path FROM image_contents WHERE set_id = 'S-001' ORDER BY media_path`)
	wantContents := []string{"MANIFEST.json", "catalog-export.sqlite", "chunks/big.bin.000", "files/a.txt"}
	if !equalStrings(gotContents, wantContents) {
		t.Fatalf("image_contents = %v, want %v", gotContents, wantContents)
	}
	// Run provenance carries over: the ingest run is finished 'ok' with
	// its stats, and its event is present.
	var status, stats string
	if err := export.sql.QueryRow(
		`SELECT status, stats_json FROM runs WHERE set_id = 'S-001' AND kind = 'ingest'`).
		Scan(&status, &stats); err != nil {
		t.Fatalf("ingest run row: %v", err)
	}
	if status != "ok" || stats != `{"files":3}` {
		t.Errorf("ingest run = (%q, %q), want (ok, {\"files\":3})", status, stats)
	}
	if got := rowsOf(t, export, `SELECT detail FROM run_events ORDER BY event_id`); !equalStrings(got, []string{"seed event"}) {
		t.Errorf("run_events = %v, want [seed event]", got)
	}
}

// Exporting the OTHER set of the same catalog yields only that set -
// the scoping is symmetric, not an artifact of S-001 being first.
func TestExportSnapshotOtherSetIsAlsoScoped(t *testing.T) {
	f := seedExportFixture(t)
	export, _, _ := openExport(t, f.db, "S-002")
	t.Cleanup(func() { export.Close() })

	if got := rowsOf(t, export, `SELECT set_id FROM sets`); !equalStrings(got, []string{"S-002"}) {
		t.Fatalf("sets = %v, want exactly [S-002]", got)
	}
	if got := rowsOf(t, export, `SELECT relpath FROM files`); !equalStrings(got, []string{"secret.txt"}) {
		t.Fatalf("files = %v, want [secret.txt]", got)
	}
	if got := rowsOf(t, export, `SELECT relpath FROM v_active_files`); !equalStrings(got, []string{"secret.txt"}) {
		t.Fatalf("v_active_files = %v, want [secret.txt]", got)
	}
	var imageID int64
	if err := export.sql.QueryRow(`SELECT image_id FROM files WHERE set_id = 'S-002'`).Scan(&imageID); err != nil {
		t.Fatalf("placement read on S-002 export: %v", err)
	}
	if imageID != 3 {
		t.Fatalf("S-002 files.image_id = %d, want 3", imageID)
	}
}

// An unknown set id fails loudly before any staging work and leaves
// nothing behind - no destination, no scratch.
func TestExportSnapshotUnknownSetFails(t *testing.T) {
	f := seedExportFixture(t)
	dir := t.TempDir()
	dest := filepath.Join(dir, "catalog-export.sqlite")

	_, err := f.db.ExportSnapshot(context.Background(), "S-NOPE", dest)
	if err == nil {
		t.Fatal("ExportSnapshot(unknown set) succeeded, want error")
	}
	if !strings.Contains(err.Error(), `set "S-NOPE" not found`) {
		t.Errorf("error = %v, want it to name the missing set", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("dest exists after failed export (stat: %v)", statErr)
	}
	noScratchInDir(t, dir)
}

// The caller contract: an existing destination is refused untouched -
// the same never-overwrite semantics SQLite's own VACUUM INTO has.
func TestExportSnapshotRefusesOverwrite(t *testing.T) {
	f := seedExportFixture(t)
	dir := t.TempDir()
	dest := filepath.Join(dir, "catalog-export.sqlite")
	const marker = "DO NOT TOUCH"
	if err := os.WriteFile(dest, []byte(marker), 0o644); err != nil {
		t.Fatalf("seed destination: %v", err)
	}

	_, err := f.db.ExportSnapshot(context.Background(), "S-001", dest)
	if err == nil {
		t.Fatal("ExportSnapshot over an existing file succeeded, want error")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Errorf("error = %v, want the overwrite refusal", err)
	}
	got, readErr := os.ReadFile(dest)
	if readErr != nil {
		t.Fatalf("re-read destination: %v", readErr)
	}
	if string(got) != marker {
		t.Fatalf("destination was modified: %q, want %q", got, marker)
	}
	noScratchInDir(t, dir)
}

// Context cancellation aborts the export with the cancellation error
// wrapped, and NOTHING is left behind: no destination file, no
// scratch. The publish is the final rename - there is no window in
// which a half export exists at destPath.
func TestExportSnapshotCancelledContextLeavesNoArtifacts(t *testing.T) {
	f := seedExportFixture(t)
	dir := t.TempDir()
	dest := filepath.Join(dir, "catalog-export.sqlite")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.db.ExportSnapshot(ctx, "S-001", dest)
	if err == nil {
		t.Fatal("ExportSnapshot with a cancelled context succeeded, want error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want it to wrap context.Canceled", err)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("dest exists after cancelled export (stat: %v)", statErr)
	}
	noScratchInDir(t, dir)
}

// Snapshot consistency (FR-034's "guaranteed to be internally
// consistent"): an export taken while another connection holds an
// UNCOMMITTED write sees exactly the committed state - the in-flight
// transaction is invisible, and committed rows are present whole.
// After the writer commits, a fresh export sees the new row too.
func TestExportSnapshotConsistentUnderConcurrentWrite(t *testing.T) {
	f := seedExportFixture(t)
	ctx := context.Background()

	// A committed event both exports must see.
	if _, err := f.db.sql.Exec(
		`INSERT INTO run_events (run_id, at, level, file_id, relpath, detail)
		 VALUES (?, 2, 'info', NULL, NULL, 'committed')`, f.run1); err != nil {
		t.Fatalf("committed event: %v", err)
	}

	// A second connection begins an uncommitted write transaction.
	writer := rawOpen(t, f.catPath)
	wtx, err := writer.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("writer BeginTx: %v", err)
	}
	if _, err := wtx.Exec(
		`INSERT INTO run_events (run_id, at, level, file_id, relpath, detail)
		 VALUES (?, 3, 'info', NULL, NULL, 'in-flight')`, f.run1); err != nil {
		t.Fatalf("in-flight event: %v", err)
	}

	// Export DURING the open writer transaction: the WAL read snapshot
	// pins the committed state.
	export1, _, _ := openExport(t, f.db, "S-001")
	got := rowsOf(t, export1, `SELECT detail FROM run_events ORDER BY event_id`)
	if !equalStrings(got, []string{"seed event", "committed"}) {
		t.Fatalf("export during open writer tx = %v, want [seed event committed]", got)
	}
	if err := export1.Close(); err != nil {
		t.Fatalf("close export1: %v", err)
	}

	// Commit, export again: the new committed row appears, whole.
	if err := wtx.Commit(); err != nil {
		t.Fatalf("writer commit: %v", err)
	}
	export2, _, _ := openExport(t, f.db, "S-001")
	got = rowsOf(t, export2, `SELECT detail FROM run_events ORDER BY event_id`)
	if !equalStrings(got, []string{"seed event", "committed", "in-flight"}) {
		t.Fatalf("export after commit = %v, want all three events", got)
	}
	// The committed-during-export export is itself integrity-valid.
	if v := integrityCheck(t, export2.sql); v != "ok" {
		t.Fatalf("integrity_check = %q, want ok", v)
	}
	assertNoForeignKeyViolations(t, export2)
}

// --- adversarial and scale-adjacent gaps (T3.2 audit) ------------------------

// The defer_foreign_keys CLOSURE PROOF (export.go guarantees): if a
// copied row ever references a row the set-scoped selection missed, the
// deferred check fires at COMMIT, the commit fails, and NOTHING
// publishes. Honest API code cannot produce such a row (EnsureChunkRows
// derives chunks.set_id from the parent file), but a CORRUPT catalog
// could: chunks.set_id and chunks.file_id are not constrained to agree,
// so a chunk row of set A pointing at set B's file is storable in the
// source (every individual FK is satisfied there). This test injects
// exactly that missed reference and pins that the export refuses it.
func TestExportSnapshotClosureProofRejectsMissedReference(t *testing.T) {
	f := seedExportFixture(t)
	ctx := context.Background()

	// The corruption: a chunk row scoped to S-001 whose file_id points
	// at S-002's file. Every FK holds in the SOURCE (the file row
	// exists), so this models a corrupt catalog, not an API misuse.
	if _, err := f.db.sql.Exec(
		`INSERT INTO chunks (set_id, file_id, chunk_index, offset, length)
		 VALUES ('S-001', ?, 99, 0, 10)`, f.otherFile); err != nil {
		t.Fatalf("injecting cross-set chunk row: %v", err)
	}

	dir := t.TempDir()
	dest := filepath.Join(dir, "catalog-export.sqlite")
	_, err := f.db.ExportSnapshot(ctx, "S-001", dest)
	if err == nil {
		t.Fatal("ExportSnapshot with a missed reference succeeded, want the commit-time closure check to fail")
	}
	if !strings.Contains(err.Error(), "committing staged rows") {
		t.Errorf("error = %v, want the commit-time failure", err)
	}
	if !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Errorf("error = %v, want it to name the foreign-key violation", err)
	}
	// The closure proof's value: nothing published, nothing left over.
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("dest exists after a rejected export (stat: %v)", statErr)
	}
	noScratchInDir(t, dir)

	// Sanity: the OTHER set (whose file the corrupt row points at) still
	// exports - scoping is unaffected by the corruption of set S-001.
	export2, _, _ := openExport(t, f.db, "S-002")
	t.Cleanup(func() { export2.Close() })
	if got := rowsOf(t, export2, `SELECT relpath FROM files`); !equalStrings(got, []string{"secret.txt"}) {
		t.Fatalf("S-002 export files = %v, want [secret.txt]", got)
	}
}

// A set with NOTHING in it (freshly EnsureSet'd, zero runs, zero files)
// still exports: the artifact is integrity-valid, FK-closed, carries the
// full SCHEMA (all tables, indexes, the view, the stamped user_version),
// and is writable for the future builder/restore writer - an empty set
// export must be a usable database, not a hollow file.
func TestExportSnapshotEmptySetHasFullSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.sqlite")
	mustCreate(t, path)
	db := mustOpen(t, path, OpenReadWrite)
	ctx := context.Background()
	if err := db.EnsureSet(ctx, "S-EMPTY", "/empty", time.Now().UnixNano(), ToolVersion); err != nil {
		t.Fatalf("EnsureSet: %v", err)
	}

	export, dest, _ := openExport(t, db, "S-EMPTY")
	t.Cleanup(func() { export.Close() })
	noScratchInDir(t, filepath.Dir(dest))

	if got := integrityCheck(t, export.sql); got != "ok" {
		t.Fatalf("integrity_check = %q, want ok", got)
	}
	assertNoForeignKeyViolations(t, export)
	version, err := export.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != SchemaVersion {
		t.Fatalf("export user_version = %d, want %d", version, SchemaVersion)
	}
	// The full schema is present even with zero data rows.
	wantObjects(t, export.sql, v1Objects())
	// Exactly the set row, and every other table empty but PRESENT.
	if got := rowsOf(t, export, `SELECT set_id FROM sets`); !equalStrings(got, []string{"S-EMPTY"}) {
		t.Fatalf("sets = %v, want [S-EMPTY]", got)
	}
	for _, table := range []string{"runs", "files", "directories", "chunks", "images", "image_contents", "run_events"} {
		var n int
		if err := export.sql.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("%s = %d rows, want 0", table, n)
		}
	}

	// Future-writer usability: a writable copy of the export accepts a
	// valid row and still enforces the schema constraints - the export
	// is a working database, not just a readable one.
	copyPath := filepath.Join(t.TempDir(), "writable.sqlite")
	blob, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	if err := os.WriteFile(copyPath, blob, 0o644); err != nil {
		t.Fatalf("copy export: %v", err)
	}
	wdb := mustOpen(t, copyPath, OpenReadWrite)
	if _, err := wdb.sql.Exec(
		`INSERT INTO runs (set_id, kind, root, started_at, status, tool_version)
		 VALUES ('S-EMPTY', 'plan', '/empty', 1, 'running', 'test')`); err != nil {
		t.Fatalf("writable export rejected a valid run insert: %v", err)
	}
	if _, err := wdb.sql.Exec(
		`INSERT INTO runs (set_id, kind, root, started_at, status, tool_version)
		 VALUES ('S-EMPTY', 'not-a-kind', '/empty', 1, 'running', 'test')`); err == nil {
		t.Error("writable export accepted a runs.kind outside the CHECK constraint")
	}
}

// A set whose runs span EVERY kind the schema's runs.kind CHECK allows
// (DESIGN_SPEC 2.3: ingest, rescan, plan, copy, build, verify_media,
// validate, restore, clear) exports all of them verbatim - the
// positional INSERT..SELECT preserves the CHECK-constrained kind strings
// as data - and every run's events travel with it via run_id. The CHECK
// constraint itself survives the copy: the export still rejects invalid
// kinds and accepts valid ones.
func TestExportSnapshotPreservesEveryRunKind(t *testing.T) {
	allKinds := []string{
		"ingest", "rescan", "plan", "copy", "build",
		"verify_media", "validate", "restore", "clear",
	}
	path := filepath.Join(t.TempDir(), "catalog.sqlite")
	mustCreate(t, path)
	db := mustOpen(t, path, OpenReadWrite)
	ctx := context.Background()
	if err := db.EnsureSet(ctx, "S-KINDS", "/data", time.Now().UnixNano(), ToolVersion); err != nil {
		t.Fatalf("EnsureSet: %v", err)
	}
	for _, kind := range allKinds {
		run, err := db.StartScanRun(ctx, ScanRun{
			SetID: "S-KINDS", Kind: kind, Root: "/data",
			StartedAt: time.Now().UnixNano(),
		})
		if err != nil {
			t.Fatalf("StartScanRun(%s): %v", kind, err)
		}
		if err := db.InsertRunEvent(ctx, RunEvent{
			RunID: run, At: 1, Level: "info", Detail: "event of " + kind,
		}); err != nil {
			t.Fatalf("InsertRunEvent(%s): %v", kind, err)
		}
		if err := db.FinishRun(ctx, run, "ok", 2, "{}"); err != nil {
			t.Fatalf("FinishRun(%s): %v", kind, err)
		}
	}

	export, dest, _ := openExport(t, db, "S-KINDS")
	t.Cleanup(func() { export.Close() })
	if got := integrityCheck(t, export.sql); got != "ok" {
		t.Fatalf("integrity_check = %q, want ok", got)
	}

	// All nine run rows survive with their kind strings intact.
	if got := rowsOf(t, export, `SELECT kind FROM runs`); !equalStrings(got, allKinds) {
		t.Fatalf("runs kinds = %v, want %v", got, allKinds)
	}
	// Every run's event travels with it, correlated through run_id.
	for _, kind := range allKinds {
		got := rowsOf(t, export,
			`SELECT e.detail FROM run_events e JOIN runs r ON r.run_id = e.run_id WHERE r.kind = ?`, kind)
		if !equalStrings(got, []string{"event of " + kind}) {
			t.Errorf("events of %s run = %v, want [event of %s]", kind, got, kind)
		}
	}
	var nEvents int
	if err := export.sql.QueryRow(`SELECT count(*) FROM run_events`).Scan(&nEvents); err != nil {
		t.Fatalf("count run_events: %v", err)
	}
	if nEvents != len(allKinds) {
		t.Errorf("run_events = %d rows, want %d", nEvents, len(allKinds))
	}

	// The runs CHECK constraint still validates IN THE EXPORT: a
	// writable copy rejects an invalid kind and accepts a valid one.
	copyPath := filepath.Join(t.TempDir(), "writable.sqlite")
	blob, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	if err := os.WriteFile(copyPath, blob, 0o644); err != nil {
		t.Fatalf("copy export: %v", err)
	}
	wdb := mustOpen(t, copyPath, OpenReadWrite)
	if _, err := wdb.sql.Exec(
		`INSERT INTO runs (set_id, kind, root, started_at, status, tool_version)
		 VALUES ('S-KINDS', 'not-a-kind', '/data', 1, 'running', 'test')`); err == nil {
		t.Error("export accepted a runs.kind outside the CHECK constraint - positional copy weakened the schema")
	}
	if _, err := wdb.sql.Exec(
		`INSERT INTO runs (set_id, kind, root, started_at, status, tool_version)
		 VALUES ('S-KINDS', 'copy', '/data', 1, 'running', 'test')`); err != nil {
		t.Errorf("export rejected a valid runs.kind: %v", err)
	}
}

// Length boundaries: the export is a positional INSERT..SELECT, so it
// must impose NO length or encoding assumptions on catalog content. A
// 4000+ character set id, a deeply nested relpath, and a unicode
// relpath all round-trip byte-for-byte.
func TestExportSnapshotLongAndUnicodeIdentifiers(t *testing.T) {
	longSet := "S-" + strings.Repeat("x", 4000)
	longRel := strings.Repeat("deep/", 800) + "leaf.bin" // 4008 chars
	unicodeRel := "unicode/データ 🎉/naïve-file.bin"
	path := filepath.Join(t.TempDir(), "catalog.sqlite")
	mustCreate(t, path)
	db := mustOpen(t, path, OpenReadWrite)
	ctx := context.Background()

	if err := db.EnsureSet(ctx, longSet, "/data", time.Now().UnixNano(), ToolVersion); err != nil {
		t.Fatalf("EnsureSet(long): %v", err)
	}
	run, err := db.StartScanRun(ctx, ScanRun{
		SetID: longSet, Kind: "ingest", Root: "/data", StartedAt: time.Now().UnixNano(),
	})
	if err != nil {
		t.Fatalf("StartScanRun: %v", err)
	}
	stx, err := db.BeginScanTx(ctx, run, longSet)
	if err != nil {
		t.Fatalf("BeginScanTx: %v", err)
	}
	for i, rel := range []string{longRel, unicodeRel} {
		if _, err := stx.InsertScanFile(ctx, regularRow(rel, int64(100+i), byte(i))); err != nil {
			t.Fatalf("InsertScanFile(%d): %v", i, err)
		}
	}
	if err := stx.Commit(); err != nil {
		t.Fatalf("commit scan tx: %v", err)
	}

	export, _, _ := openExport(t, db, longSet)
	t.Cleanup(func() { export.Close() })
	if got := integrityCheck(t, export.sql); got != "ok" {
		t.Fatalf("integrity_check = %q, want ok", got)
	}
	// The set id round-trips exactly (PRIMARY KEY lookup works on it).
	if got := rowsOf(t, export, `SELECT set_id FROM sets`); !equalStrings(got, []string{longSet}) {
		t.Fatalf("sets round-trip broken: got len %d, want len %d",
			len(got[0]), len(longSet))
	}
	// Both relpaths round-trip byte-for-byte.
	want := []string{longRel, unicodeRel}
	if got := rowsOf(t, export, `SELECT relpath FROM files`); !equalStrings(got, want) {
		t.Fatalf("files round-trip broken: got %q (lens %d/%d), want %q (lens %d/%d)",
			got[0], len(got[0]), len(got[1]), want[0], len(want[0]), len(want[1]))
	}
}

// destPath inside a MISSING directory: a clean, classified error - the
// documented decision (export.go CALLER CONTRACT) is a PLAIN wrapped
// error, not a CatalogError kind (10.1 kinds cover open/corrupt/schema/
// migration of the CATALOG, and the CLI classifies export failures at
// the command level) - with no panic and no artifacts: the missing
// directory is not created and no scratch appears anywhere.
func TestExportSnapshotMissingDestinationDirectory(t *testing.T) {
	f := seedExportFixture(t)
	missing := filepath.Join(t.TempDir(), "no", "such", "dir")
	dest := filepath.Join(missing, "catalog-export.sqlite")

	_, err := f.db.ExportSnapshot(context.Background(), "S-001", dest)
	if err == nil {
		t.Fatal("ExportSnapshot into a missing directory succeeded, want error")
	}
	if !strings.Contains(err.Error(), "catalog: export:") {
		t.Errorf("error = %v, want the export-prefixed message", err)
	}
	var cerr *CatalogError
	if errors.As(err, &cerr) {
		t.Errorf("error is a *CatalogError (kind %v), want a plain error per the documented decision", cerr.Kind)
	}
	// No artifacts: the directory itself was not created (the scratch
	// files would have lived in it), and destPath does not exist.
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Errorf("missing directory was created (stat: %v)", statErr)
	}
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Errorf("dest exists after failed export (stat: %v)", statErr)
	}
}

// The verify step of the publish pipeline CATCHES a damaged .partial:
// the pipeline calls verifyExportFile on the VACUUM INTO output before
// the rename, and this test proves the helper would reject every damage
// class that matters - a truncated file, a corrupted page, and (the
// subtle one) an FK-violating file that is still integrity-VALID, which
// only PRAGMA foreign_key_check can see.
//
// BOUNDARY NOTE (documented per the T3.2 audit): corruption cannot be
// injected between VACUUM INTO and verify THROUGH the API - there is no
// hook between exportVacuum and verifyExportFile - so this exercises the
// helper directly, which is exactly what the pipeline invokes.
func TestExportSnapshotVerifyCatchesCorruption(t *testing.T) {
	f := seedExportFixture(t)
	_, dest, _ := openExport(t, f.db, "S-001")
	ctx := context.Background()
	dir := filepath.Dir(dest)

	// Control: the untouched export passes.
	if err := verifyExportFile(ctx, dest); err != nil {
		t.Fatalf("verifyExportFile(pristine): %v", err)
	}

	// A truncated .partial (torn write, short copy): caught.
	blob, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read export: %v", err)
	}
	trunc := filepath.Join(dir, "truncated.sqlite")
	if err := os.WriteFile(trunc, blob[:len(blob)/2], 0o644); err != nil {
		t.Fatalf("write truncated: %v", err)
	}
	if err := verifyExportFile(ctx, trunc); err == nil {
		t.Error("verifyExportFile(truncated) succeeded, want integrity failure")
	}

	// A corrupted page (bit rot: one flipped byte in page 1's b-tree
	// header, immediately after the 100-byte database header): caught.
	flip := filepath.Join(dir, "flipped.sqlite")
	flipped := append([]byte(nil), blob...)
	flipped[100] ^= 0xFF
	if err := os.WriteFile(flip, flipped, 0o644); err != nil {
		t.Fatalf("write flipped: %v", err)
	}
	if err := verifyExportFile(ctx, flip); err == nil {
		t.Error("verifyExportFile(flipped byte) succeeded, want integrity failure")
	}

	// An FK-violating .partial that is still integrity-VALID: only
	// PRAGMA foreign_key_check can see this. Built with a raw handle
	// (SQLite enforces FKs OFF by default, so the dangling reference
	// commits) - the corruption class the closure proof guards against
	// in the staged copy, replayed against the verifier itself.
	fkv := filepath.Join(dir, "fkviolation.sqlite")
	fdb := rawOpen(t, fkv)
	if _, err := fdb.Exec(`CREATE TABLE parent (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create parent: %v", err)
	}
	if _, err := fdb.Exec(
		`CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER NOT NULL REFERENCES parent(id))`); err != nil {
		t.Fatalf("create child: %v", err)
	}
	if _, err := fdb.Exec(`INSERT INTO child (id, parent_id) VALUES (1, 99)`); err != nil {
		t.Fatalf("insert dangling: %v", err)
	}
	if got := integrityCheck(t, fdb); got != "ok" {
		t.Fatalf("fk-violating fixture should be integrity-ok, got %q", got)
	}
	if err := verifyExportFile(ctx, fkv); err == nil {
		t.Error("verifyExportFile(FK-violating) succeeded, want foreign_key_check failure")
	} else if !strings.Contains(err.Error(), "foreign_key_check") {
		t.Errorf("error = %v, want it to name foreign_key_check", err)
	}
}

// FOREIGN SCRATCH IS IGNORED, NEVER TOUCHED (QA-hardening follow-up to
// the old stale-scratch-discard test): the earlier fixed-name design
// used destPath+".staging"/".partial" for EVERY call, so leftover
// scratch from a crashed earlier export - or the LIVE scratch of a
// concurrent call - sat at exactly those paths, and the entry-time
// discard deleted whatever it found there. That unlink is the
// QA-measured corruption class: the loser of a same-destination race
// unlinked the winner's live staging file, the winner's vacuum
// re-created a fresh EMPTY database, and the EMPTY file published with
// err=nil. With per-call scratch names (export.go) the discard is
// GONE. This test seeds garbage at the exact OLD fixed paths (the
// names a pre-fix crashed export leaves behind, including the -wal
// sidecar QA observed) and proves the new scheme (a) is completely
// unaffected by it and (b) never removes a path it does not own - the
// foreign scratch survives byte-identical.
func TestExportSnapshotIgnoresForeignScratch(t *testing.T) {
	f := seedExportFixture(t)
	dir := t.TempDir()
	dest := filepath.Join(dir, "catalog-export.sqlite")
	// The exact paths the OLD scheme used and shared between calls: a
	// crashed export's leftover - or, under the old collision, a
	// concurrent call's LIVE scratch - provably sits at these names,
	// which the new per-call scheme can never generate.
	foreign := map[string]string{
		dest + exportStagingSuffix:          "garbage left by a crashed export",
		dest + exportStagingSuffix + "-wal": "wal sidecar observed by QA",
		dest + exportPartialSuffix:          "more garbage",
	}
	for p, content := range foreign {
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("seed foreign scratch %s: %v", p, err)
		}
	}

	export, _, _ := openExportAt(t, f.db, "S-001", dest)
	t.Cleanup(func() { export.Close() })
	if got := rowsOf(t, export, `SELECT relpath FROM files`); !equalStrings(got, []string{"a.txt", "big.bin", "link"}) {
		t.Fatalf("files = %v, want the full set", got)
	}
	if got := integrityCheck(t, export.sql); got != "ok" {
		t.Fatalf("integrity_check = %q, want ok", got)
	}

	// (b) The foreign scratch was never REMOVED or MODIFIED: only this
	// call's own uniquely-named scratch is ever deleted. Byte-identical
	// survival is the proof that no unlink-what-you-don't-own behavior
	// remains in the pipeline.
	for p, content := range foreign {
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("foreign scratch %s did not survive (read: %v) - "+
				"the export removed a path it does not own", p, err)
		}
		if string(got) != content {
			t.Fatalf("foreign scratch %s was modified: %q, want %q", p, got, content)
		}
	}
	// (a) The foreign scratch was IGNORED, not adopted: the only
	// scratch-shaped names in the directory are the foreign ones
	// seeded above - this call's own per-call scratch is fully cleaned
	// up by the publish.
	seeded := make(map[string]bool, len(foreign))
	for p := range foreign {
		seeded[filepath.Base(p)] = true
	}
	for _, name := range scratchArtifactsIn(dir) {
		if !seeded[name] {
			t.Errorf("export scratch left behind: %s", name)
		}
	}
}

// Point-in-time semantics: after the set grows (a rescan adds a file, a
// later plan assigns it and adds a third image), a re-export to a
// DIFFERENT destPath contains the grown placement, while the earlier
// export is untouched - both are valid snapshots of their moment.
func TestExportSnapshotPointInTimeAfterGrowth(t *testing.T) {
	f := seedExportFixture(t)
	ctx := context.Background()

	first, firstDest, _ := openExport(t, f.db, "S-001")
	t.Cleanup(func() { first.Close() })
	firstStat, err := os.Stat(firstDest)
	if err != nil {
		t.Fatalf("stat first export: %v", err)
	}

	// Grow the set: rescan adds new.txt, plan places it on image 3.
	rescan, err := f.db.StartScanRun(ctx, ScanRun{
		SetID: "S-001", Kind: "rescan", Root: "/data", StartedAt: time.Now().UnixNano(),
	})
	if err != nil {
		t.Fatalf("StartScanRun(rescan): %v", err)
	}
	stx, err := f.db.BeginScanTx(ctx, rescan, "S-001")
	if err != nil {
		t.Fatalf("BeginScanTx: %v", err)
	}
	newFile, err := stx.InsertScanFile(ctx, regularRow("new.txt", 55, 0xCC))
	if err != nil {
		t.Fatalf("InsertScanFile(new.txt): %v", err)
	}
	if err := stx.Commit(); err != nil {
		t.Fatalf("commit rescan tx: %v", err)
	}
	if err := f.db.FinishRun(ctx, rescan, "ok", 2, "{}"); err != nil {
		t.Fatalf("FinishRun(rescan): %v", err)
	}
	planRun, err := f.db.StartScanRun(ctx, ScanRun{
		SetID: "S-001", Kind: "plan", Root: "/data", StartedAt: time.Now().UnixNano(),
	})
	if err != nil {
		t.Fatalf("StartScanRun(plan): %v", err)
	}
	ptx, err := f.db.BeginPlanTx(ctx, planRun, "S-001")
	if err != nil {
		t.Fatalf("BeginPlanTx: %v", err)
	}
	if err := ptx.InsertImage(ctx, ImageSpec{
		ImageID: 3, NominalSize: 25_000_000_000, UsableSize: 23_000_000_000,
		MediaProfile: "bd-r-sl",
	}); err != nil {
		t.Fatalf("InsertImage(3): %v", err)
	}
	if err := ptx.AssignFile(ctx, newFile, 3); err != nil {
		t.Fatalf("AssignFile(new.txt): %v", err)
	}
	if err := ptx.Commit(); err != nil {
		t.Fatalf("commit plan tx: %v", err)
	}
	if err := f.db.FinishRun(ctx, planRun, "ok", 1, "{}"); err != nil {
		t.Fatalf("FinishRun(plan): %v", err)
	}

	// The SECOND export carries the grown placement.
	second, secondDest, _ := openExport(t, f.db, "S-001")
	t.Cleanup(func() { second.Close() })
	if secondDest == firstDest {
		t.Fatal("second export reused the first destination")
	}
	got := rowsOf(t, second,
		`SELECT relpath FROM files WHERE image_id = 3`)
	if !equalStrings(got, []string{"new.txt"}) {
		t.Fatalf("grown export image-3 files = %v, want [new.txt]", got)
	}
	var nImages int
	if err := second.sql.QueryRow(`SELECT count(*) FROM images WHERE set_id = 'S-001'`).Scan(&nImages); err != nil {
		t.Fatalf("count images: %v", err)
	}
	if nImages != 3 {
		t.Errorf("grown export has %d images, want 3", nImages)
	}

	// The FIRST export is untouched: no new.txt, still two images, same
	// byte size on disk - a point-in-time snapshot, not a live view.
	if got := rowsOf(t, first, `SELECT relpath FROM files WHERE relpath = 'new.txt'`); len(got) != 0 {
		t.Errorf("first export grew retroactively: %v", got)
	}
	if err := first.sql.QueryRow(`SELECT count(*) FROM images WHERE set_id = 'S-001'`).Scan(&nImages); err != nil {
		t.Fatalf("count images (first): %v", err)
	}
	if nImages != 2 {
		t.Errorf("first export has %d images, want 2 (point-in-time)", nImages)
	}
	afterStat, err := os.Stat(firstDest)
	if err != nil {
		t.Fatalf("re-stat first export: %v", err)
	}
	if afterStat.Size() != firstStat.Size() {
		t.Errorf("first export size changed: %d -> %d", firstStat.Size(), afterStat.Size())
	}
	if got := integrityCheck(t, first.sql); got != "ok" {
		t.Errorf("first export integrity_check = %q, want ok", got)
	}
}

// SCRATCH-COLLISION HARDENING (the QA-hardening follow-up to the T3.2
// audit item): the earlier FIXED-NAME design shared
// destPath+".staging"/".partial" between concurrent calls to the same
// destPath, and QA measured the resulting corruption class over 200
// rounds: the loser's error-path defer unlinked the WINNER's live
// staging file, the winner's vacuum re-created a fresh EMPTY database,
// and the EMPTY file published with err=nil - an empty database is
// integrity-valid and FK-closed, so the verify step passed. Leftover
// .staging-wal/.staging-shm sidecars were observed too.
//
// The fix (export.go): UNIQUE PER-CALL scratch names
// (destPath+".staging-<pid-<rand>/" ".partial-<pid>-<rand>"), so no
// call can ever unlink or re-create another call's scratch. This test
// pins the NEW guarantee over repeated contention. EVERY round ends
// with EITHER:
//
//   - at least one SUCCESS whose published export is VALID AND
//     NON-EMPTY - integrity_check ok, FK-closed, and the seeded rows
//     of the set present (sets exactly [S-001], files exactly
//     [a.txt big.bin link]) - with any other call failing cleanly, OR
//   - both calls failing cleanly (a plain "catalog: export:" error,
//     no destination, no scratch);
//
// and NEVER with an empty (or schemaless) publish under a nil error.
// No scratch or WAL/SHM sidecar survives ANY round. With unique
// scratch there is no shared-file fight and no busy_timeout stall, so
// rounds are cheap and can be numerous; the only residual interleaving
// is the benign no-overwrite re-check/rename overlap documented in
// export.go, where both calls succeed and the last rename wins - both
// files are complete verified exports, so it cannot resurrect the
// empty-publish class either.
func TestExportSnapshotConcurrentSameDestination(t *testing.T) {
	f := seedExportFixture(t)
	ctx := context.Background()

	// Each round runs two FULL exports of the same set to the same
	// destPath on their own unique scratch - real work, no timeout
	// fights - so dozens of rounds stay well under a second of budget
	// even under -race.
	const rounds = 40
	var oneOK, twoOK, noneOK int
	for round := 0; round < rounds; round++ {
		dir := t.TempDir()
		dest := filepath.Join(dir, "catalog-export.sqlite")

		var wg sync.WaitGroup
		start := make(chan struct{})
		errs := make([]error, 2)
		sizes := make([]int64, 2)
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				sizes[i], errs[i] = f.db.ExportSnapshot(ctx, "S-001", dest)
			}(i)
		}
		close(start)
		wg.Wait()

		successes := 0
		for i, err := range errs {
			if err == nil {
				successes++
				// The returned-size contract holds per call.
				if sizes[i] <= 0 {
					t.Fatalf("round %d: call %d succeeded with size %d, want > 0", round, i, sizes[i])
				}
				continue
			}
			// Every failure is CLEAN: a plain export-prefixed error,
			// no panic (the test process would not survive one).
			if !strings.Contains(err.Error(), "catalog: export:") {
				t.Fatalf("round %d: call %d failed uncleanly: %v", round, i, err)
			}
		}

		fi, statErr := os.Stat(dest)
		if os.IsNotExist(statErr) {
			// Case (b): both failed cleanly, nothing may exist.
			if successes != 0 {
				t.Fatalf("round %d: %d calls succeeded but no destination published", round, successes)
			}
			noneOK++
		} else if statErr != nil {
			t.Fatalf("round %d: stat dest: %v", round, statErr)
		} else {
			// Case (a): a published destination requires at least one
			// success. A file appearing while BOTH calls failed would
			// be the corruption class resurfacing (a publish that no
			// successful caller owns).
			if successes == 0 {
				t.Fatalf("round %d: destination published although both calls failed (errs: %v | %v)",
					round, errs[0], errs[1])
			}
			// THE corruption invariant: the published export is VALID
			// AND NON-EMPTY. An empty (or even schemaless) file under
			// err=nil is exactly the QA-measured class - fail loudly
			// if it ever regresses.
			if fi.Size() == 0 {
				t.Fatalf("round %d: EMPTY file published with err=nil (the QA corruption class)", round)
			}
			pub := mustOpen(t, dest, OpenReadOnly)
			if got := integrityCheck(t, pub.sql); got != "ok" {
				t.Fatalf("round %d: published export integrity_check = %q, want ok", round, got)
			}
			assertNoForeignKeyViolations(t, pub)
			if got := rowsOf(t, pub, `SELECT set_id FROM sets`); !equalStrings(got, []string{"S-001"}) {
				t.Fatalf("round %d: published sets = %v, want exactly [S-001] (empty/wrong publish)", round, got)
			}
			// The seeded rows are present - NON-EMPTY is pinned as row
			// content, not just byte size: the empty-publish class
			// produced a zero-row (or schemaless) database that passed
			// integrity_check and foreign_key_check.
			gotFiles := rowsOf(t, pub, `SELECT relpath FROM files ORDER BY relpath`)
			if !equalStrings(gotFiles, []string{"a.txt", "big.bin", "link"}) {
				t.Fatalf("round %d: published files = %v, want the full seeded set "+
					"(an empty publish under err=nil is the QA corruption class)", round, gotFiles)
			}
			if err := pub.Close(); err != nil {
				t.Fatalf("round %d: close published: %v", round, err)
			}
			if successes == 2 {
				twoOK++
			} else {
				oneOK++
			}
		}

		// Unique per-call scratch plus an error-path defer that removes
		// only the call's OWN files: no scratch or sidecar may survive
		// ANY round, whatever the interleaving.
		if leftovers := scratchArtifactsIn(dir); len(leftovers) > 0 {
			t.Fatalf("round %d: scratch left behind: %v (errs: %v | %v)",
				round, leftovers, errs[0], errs[1])
		}
	}
	t.Logf("outcomes over %d rounds: %d one-success, %d both-success (benign re-check/rename overlap), %d both-failed",
		rounds, oneOK, twoOK, noneOK)

	// The source catalog is never affected by the contention.
	fresh, _, _ := openExport(t, f.db, "S-001")
	t.Cleanup(func() { fresh.Close() })
	if got := rowsOf(t, fresh, `SELECT relpath FROM files`); !equalStrings(got, []string{"a.txt", "big.bin", "link"}) {
		t.Fatalf("source catalog damaged by concurrent exports: %v", got)
	}
}

// Per-call scratch uniqueness - the invariant every concurrent-publish
// guarantee in export.go rests on: exportScratchSuffix renders
// "<pid>-<16 hex crypto/rand digits>", two calls never share a
// suffix, and the format is stable (a plain suffix, no path
// separators).
func TestExportScratchSuffixUniquePerCall(t *testing.T) {
	pid := strconv.Itoa(os.Getpid())
	seen := make(map[string]bool)
	for i := 0; i < 64; i++ {
		s, err := exportScratchSuffix()
		if err != nil {
			t.Fatalf("exportScratchSuffix: %v", err)
		}
		if seen[s] {
			t.Fatalf("duplicate scratch suffix %q after %d calls - concurrent calls could collide", s, i)
		}
		seen[s] = true
		parts := strings.SplitN(s, "-", 2)
		if len(parts) != 2 || parts[0] != pid || len(parts[1]) != 16 {
			t.Fatalf("scratch suffix %q, want %q + \"-\" + 16 hex digits", s, pid)
		}
		if _, err := hex.DecodeString(parts[1]); err != nil {
			t.Fatalf("scratch suffix %q has a non-hex random part: %v", s, err)
		}
	}
}

// --- string-set helpers ------------------------------------------------------

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
