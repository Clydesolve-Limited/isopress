package catalog

// T2.2 upgrade-hazard regression tests: the `directories` table was
// folded into the v1 DDL with NO migration step (PM-ratified,
// pre-release), but a catalog created by a PRE-T2.2 binary is already
// stamped user_version = 1 and predates the table - the forward
// migration path cannot heal it because the version already matches.
// The reported failure was exactly that: scan against such a catalog
// died with "no such table: directories" at the first empty-dir upsert.
//
// ensureFoldedV1Objects (migrate.go) closes the gap on every MUTATING
// Open. These tests pin:
//   - the repair: a v1-stamped catalog missing the folded objects
//     gains table + both indexes on Open(ReadWrite), integrity_check
//     stays ok, and user_version is untouched (still 1 - the repair is
//     NOT a migration);
//   - PARTIAL damage: the table present with ONE index missing heals
//     through the same idempotent script (all three objects are
//     checked, not just the table);
//   - the DRIFT GUARD: the repaired schema is column-for-column and
//     index-for-index identical to a fresh v1 catalog's (the repair
//     DDL duplicates the verbatim block, so the two must not diverge),
//     and the comparison is proven non-vacuous against a deliberately
//     drifted table (column-type and index-set drift both detected);
//   - read-only opens never repair (mode=ro cannot write; the missing
//     table fails loudly and the file is untouched);
//   - a current catalog pays only the existence check: Open is a
//     schema no-op.

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// dropDirectoriesTable simulates a PRE-T2.2 catalog: v1-stamped, but
// without the directories table (SQLite drops its dependent indexes
// along with it).
func dropDirectoriesTable(t *testing.T, path string) {
	t.Helper()
	raw := rawOpen(t, path)
	if _, err := raw.Exec(`DROP TABLE directories`); err != nil {
		t.Fatalf("simulating pre-T2.2 catalog (drop directories): %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close simulation connection: %v", err)
	}
}

// dirTableInfo reads PRAGMA table_info(directories) into an ordered
// slice of canonical "cid|name|type|notnull|dflt|pk" strings.
func dirTableInfo(t *testing.T, sqldb *sql.DB) []string {
	t.Helper()
	rows, err := sqldb.Query(`PRAGMA table_info(directories)`)
	if err != nil {
		t.Fatalf("table_info(directories): %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, typ        string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		out = append(out, strconv.Itoa(cid)+"|"+name+"|"+typ+"|"+
			strconv.Itoa(notnull)+"|"+dflt.String+"|"+strconv.Itoa(pk))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table_info: %v", err)
	}
	return out
}

// dirIndexShape reads the directories index set as canonical
// "name|unique|indexed-columns" strings.
func dirIndexShape(t *testing.T, sqldb *sql.DB) []string {
	t.Helper()
	type idxInfo struct {
		name string
		uniq bool
	}
	var idxs []idxInfo
	rows, err := sqldb.Query(`SELECT name, "unique" FROM pragma_index_list('directories') ORDER BY name`)
	if err != nil {
		t.Fatalf("pragma_index_list(directories): %v", err)
	}
	for rows.Next() {
		var i idxInfo
		if err := rows.Scan(&i.name, &i.uniq); err != nil {
			_ = rows.Close()
			t.Fatalf("scan index_list row: %v", err)
		}
		idxs = append(idxs, i)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		t.Fatalf("iterate index_list: %v", err)
	}
	_ = rows.Close()

	var out []string
	for _, i := range idxs {
		cols, err := indexColumnList(t, sqldb, i.name)
		if err != nil {
			t.Fatalf("pragma_index_info(%s): %v", i.name, err)
		}
		uniqText := "0"
		if i.uniq {
			uniqText = "1"
		}
		out = append(out, i.name+"|"+uniqText+"|"+cols)
	}
	return out
}

// indexColumnList renders one index's indexed columns as a
// comma-joined list.
func indexColumnList(t *testing.T, sqldb *sql.DB, indexName string) (string, error) {
	t.Helper()
	rows, err := sqldb.Query(`SELECT COALESCE(name, '#') FROM pragma_index_info(?) ORDER BY seqno`, indexName)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return "", err
		}
		cols = append(cols, c)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	return strings.Join(cols, ","), nil
}

// The user-reported failure, pinned at the catalog level: a v1-stamped
// pre-T2.2 catalog (directories table absent) is healed by the
// mutating Open, and the healed catalog is fully valid.
func TestOpenRepairsPreT22CatalogMissingDirectories(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")
	mustCreate(t, path)
	dropDirectoriesTable(t, path)

	// Pre-condition: the table really is gone, while the version stamp
	// still says 1 (the hazard: the migration path sees nothing to do).
	pre := rawOpen(t, path)
	if got := catalogObjectNames(t, pre); got["directories"] != "" {
		t.Fatalf("pre-condition: directories still present (%q)", got["directories"])
	}
	if err := pre.Close(); err != nil {
		t.Fatalf("close pre-check: %v", err)
	}
	if v := userVersionOf(t, path); v != SchemaVersion {
		t.Fatalf("pre-condition: user_version = %d, want %d", v, SchemaVersion)
	}

	// The mutating Open heals the catalog.
	db := mustOpen(t, path, OpenReadWrite)

	// Folded objects present, integrity intact, version untouched (the
	// repair is deliberately NOT a migration step).
	wantObjects(t, db.sql, map[string]string{
		"directories":                "table",
		"idx_directories_image":      "index",
		"idx_directories_unassigned": "index",
	})
	if got := integrityCheck(t, db.sql); got != "ok" {
		t.Errorf("integrity_check after repair = %q, want ok", got)
	}
	if v := userVersionOf(t, path); v != SchemaVersion {
		t.Errorf("user_version after repair = %d, want %d (repair must not re-stamp)", v, SchemaVersion)
	}
}

// DRIFT GUARD: the repaired directories schema must be identical to
// the one a fresh v1 creation produces - column-for-column
// (PRAGMA table_info) and index-for-index (names, uniqueness,
// indexed columns). The repair DDL necessarily duplicates the verbatim
// block; this test makes the two impossible to drift apart silently.
func TestFoldedRepairMatchesV1DDL(t *testing.T) {
	dir := t.TempDir()

	control := filepath.Join(dir, "control.sqlite")
	mustCreate(t, control)
	controlDB := rawOpen(t, control)
	defer controlDB.Close()

	repaired := filepath.Join(dir, "repaired.sqlite")
	mustCreate(t, repaired)
	dropDirectoriesTable(t, repaired)
	rdb := mustOpen(t, repaired, OpenReadWrite)

	gotInfo, wantInfo := dirTableInfo(t, rdb.sql), dirTableInfo(t, controlDB)
	if len(wantInfo) == 0 {
		t.Fatal("control table_info is empty: the fresh v1 catalog has no directories table?")
	}
	if len(gotInfo) != len(wantInfo) {
		t.Fatalf("repaired table_info column count = %d, want %d (%v vs %v)",
			len(gotInfo), len(wantInfo), gotInfo, wantInfo)
	}
	for i := range wantInfo {
		if gotInfo[i] != wantInfo[i] {
			t.Fatalf("repaired table_info drift vs fresh v1:\n got: %v\nwant: %v", gotInfo, wantInfo)
		}
	}

	gotIdx, wantIdx := dirIndexShape(t, rdb.sql), dirIndexShape(t, controlDB)
	// The control set is the two named indexes PLUS the implicit
	// sqlite_autoindex from the UNIQUE(set_id, relpath) constraint -
	// comparing the autoindex too pins that the repaired table carries
	// the same uniqueness.
	if len(wantIdx) != 3 {
		t.Fatalf("control index set = %v, want the two named indexes plus the UNIQUE autoindex", wantIdx)
	}
	if len(gotIdx) != len(wantIdx) {
		t.Fatalf("repaired index count = %d, want %d (%v vs %v)", len(gotIdx), len(wantIdx), gotIdx, wantIdx)
	}
	for i := range wantIdx {
		if gotIdx[i] != wantIdx[i] {
			t.Fatalf("repaired index drift vs fresh v1:\n got: %v\nwant: %v", gotIdx, wantIdx)
		}
	}
}

// Read-only opens never repair: mode=ro cannot write, so the missing
// table fails loudly and the file keeps its (broken) shape until a
// mutating command heals it.
func TestOpenReadOnlyDoesNotRepair(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")
	mustCreate(t, path)
	dropDirectoriesTable(t, path)

	db := mustOpen(t, path, OpenReadOnly)
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM directories`).Scan(&n); err == nil {
		t.Fatal("read-only query on missing directories table returned nil, want an error")
	}

	// The file is untouched: still no directories table.
	_ = db.Close()
	check := rawOpen(t, path)
	defer check.Close()
	if got := catalogObjectNames(t, check); got["directories"] != "" {
		t.Errorf("read-only open repaired the catalog (directories = %q); it must never write", got["directories"])
	}
}

// A current catalog pays only the existence check: Open(ReadWrite) is
// a schema no-op (no object added or removed).
func TestOpenCurrentCatalogRepairIsNoop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")
	mustCreate(t, path)

	before := rawOpen(t, path)
	beforeObjs := catalogObjectNames(t, before)
	_ = before.Close()

	db := mustOpen(t, path, OpenReadWrite)

	afterObjs := catalogObjectNames(t, db.sql)
	if len(beforeObjs) != len(afterObjs) {
		t.Fatalf("object count changed across open: %d -> %d", len(beforeObjs), len(afterObjs))
	}
	for name, typ := range beforeObjs {
		if afterObjs[name] != typ {
			t.Errorf("object %q (%s) changed or vanished on a current catalog", name, typ)
		}
	}
}

// PARTIAL damage: the directories TABLE is present but ONE of its two
// indexes is missing (partial damage, or a future fold mistake).
// ensureFoldedV1Objects checks all THREE objects, so the same
// idempotent repair heals this shape too - and only the missing index
// is created (CREATE ... IF NOT EXISTS). Both indexes are exercised.
func TestOpenRepairsPartiallyDamagedFoldedObjects(t *testing.T) {
	for _, idx := range []string{"idx_directories_image", "idx_directories_unassigned"} {
		t.Run(idx, func(t *testing.T) {
			dir := t.TempDir()

			control := filepath.Join(dir, "control.sqlite")
			mustCreate(t, control)
			cdb := rawOpen(t, control)
			defer cdb.Close()

			path := filepath.Join(dir, "damaged.sqlite")
			mustCreate(t, path)
			raw := rawOpen(t, path)
			if _, err := raw.Exec(`DROP INDEX ` + idx); err != nil {
				t.Fatalf("simulating partial damage (drop %s): %v", idx, err)
			}
			if err := raw.Close(); err != nil {
				t.Fatalf("close damage connection: %v", err)
			}

			// Pre-condition: table present, exactly that index missing,
			// version stamp already current (the migration path has
			// nothing to do - only the repair can heal this).
			pre := rawOpen(t, path)
			objs := catalogObjectNames(t, pre)
			if objs["directories"] != "table" {
				t.Fatalf("pre-condition: directories = %q, want the table present", objs["directories"])
			}
			if _, ok := objs[idx]; ok {
				t.Fatalf("pre-condition: %s still present after the damage simulation", idx)
			}
			if err := pre.Close(); err != nil {
				t.Fatalf("close pre-check: %v", err)
			}
			if v := userVersionOf(t, path); v != SchemaVersion {
				t.Fatalf("pre-condition: user_version = %d, want %d", v, SchemaVersion)
			}

			// The mutating Open heals the partial damage.
			db := mustOpen(t, path, OpenReadWrite)
			wantObjects(t, db.sql, map[string]string{
				"directories":                "table",
				"idx_directories_image":      "index",
				"idx_directories_unassigned": "index",
			})
			if got := integrityCheck(t, db.sql); got != "ok" {
				t.Errorf("integrity_check after repair = %q, want ok", got)
			}
			if v := userVersionOf(t, path); v != SchemaVersion {
				t.Errorf("user_version after repair = %d, want %d (repair must not re-stamp)", v, SchemaVersion)
			}
			// The healed index set is identical to a fresh v1
			// catalog's, index-for-index (same guard family as
			// TestFoldedRepairMatchesV1DDL).
			gotIdx, wantIdx := dirIndexShape(t, db.sql), dirIndexShape(t, cdb)
			if len(gotIdx) != len(wantIdx) {
				t.Fatalf("healed index count = %d, want %d (%v vs %v)",
					len(gotIdx), len(wantIdx), gotIdx, wantIdx)
			}
			for i := range wantIdx {
				if gotIdx[i] != wantIdx[i] {
					t.Errorf("healed index shape drift vs fresh v1:\n got: %v\nwant: %v",
						gotIdx, wantIdx)
				}
			}
		})
	}
}

// DRIFT-GUARD TEETH: TestFoldedRepairMatchesV1DDL guards against the
// repair DDL drifting away from the verbatim v1 block by comparing
// PRAGMA table_info and the index shape of a repaired catalog against
// a fresh control. This test proves that comparison is not vacuous: a
// directories table that DRIFTS (column type change, missing named
// indexes) is detected on BOTH axes. Production code is untouched - the
// drifted shape exists only inside this test's temp catalog.
//
// (Runtime scope note, pinned here deliberately: the Open-time guard is
// an EXISTENCE check only - a present-but-drifted table passes it. The
// shape guard IS this test pair; that is the designed division of
// labor, and the DDL consts are the only drift source it can catch.)
func TestFoldedRepairDriftComparisonDetectsDivergentDDL(t *testing.T) {
	dir := t.TempDir()

	control := filepath.Join(dir, "control.sqlite")
	mustCreate(t, control)
	cdb := rawOpen(t, control)
	defer cdb.Close()

	drifted := filepath.Join(dir, "drifted.sqlite")
	mustCreate(t, drifted)
	d := rawOpen(t, drifted)
	defer d.Close()
	// Simulated drift: uid became TEXT and the two named indexes (and
	// their creation) are absent - the table NAME survives, so the
	// runtime existence check alone would pass it.
	if _, err := d.Exec(`DROP TABLE directories`); err != nil {
		t.Fatalf("dropping directories for the drift simulation: %v", err)
	}
	if _, err := d.Exec(`CREATE TABLE directories (
  dir_id         INTEGER PRIMARY KEY,
  set_id         TEXT NOT NULL REFERENCES sets(set_id),
  relpath        TEXT NOT NULL,
  mode           INTEGER NOT NULL,
  uid            TEXT,
  gid            INTEGER NOT NULL,
  mtime_ns       INTEGER NOT NULL,
  first_seen_run INTEGER NOT NULL REFERENCES runs(run_id),
  last_seen_run  INTEGER NOT NULL REFERENCES runs(run_id),
  status         TEXT NOT NULL DEFAULT 'active',
  image_id       INTEGER,
  UNIQUE (set_id, relpath)
)`); err != nil {
		t.Fatalf("creating the drifted directories table: %v", err)
	}

	// Axis 1 - column drift (uid INTEGER -> TEXT) is detected by the
	// table_info comparison.
	wantInfo, gotInfo := dirTableInfo(t, cdb), dirTableInfo(t, d)
	if len(wantInfo) == 0 {
		t.Fatal("control table_info is empty: the fresh v1 catalog has no directories table?")
	}
	if len(gotInfo) != len(wantInfo) {
		t.Fatalf("comparison missed a column-count drift: got %d columns, want %d (%v vs %v)",
			len(gotInfo), len(wantInfo), gotInfo, wantInfo)
	}
	columnDrift := false
	for i := range wantInfo {
		if gotInfo[i] != wantInfo[i] {
			columnDrift = true
			break
		}
	}
	if !columnDrift {
		t.Fatalf("table_info comparison failed to detect the uid INTEGER -> TEXT drift:\n got: %v\nwant: %v",
			gotInfo, wantInfo)
	}

	// Axis 2 - index-set drift (named indexes absent) is detected by
	// the index-shape comparison.
	wantIdx, gotIdx := dirIndexShape(t, cdb), dirIndexShape(t, d)
	if len(wantIdx) != 3 {
		t.Fatalf("control index set = %v, want the two named indexes plus the UNIQUE autoindex", wantIdx)
	}
	if len(gotIdx) >= len(wantIdx) {
		t.Fatalf("comparison missed the index-set drift: got %v, want fewer than the control %v",
			gotIdx, wantIdx)
	}
	indexDrift := false
	for i := range gotIdx {
		if gotIdx[i] != wantIdx[i] {
			indexDrift = true
			break
		}
	}
	if !indexDrift && len(gotIdx) == len(wantIdx) {
		t.Fatalf("index-shape comparison failed to detect the drift:\n got: %v\nwant: %v",
			gotIdx, wantIdx)
	}
}
