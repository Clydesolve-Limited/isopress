package catalog

// Gap-closing tests for T0.3 (catalog schema and migrations). These
// strengthen the acceptance coverage of catalog_test.go with the edge
// cases identified during the T0.3 test audit:
//
//   - double-Create behavior (second Create refused, first catalog intact)
//   - Open on a zero-byte file (SQLite treats it as a fresh database:
//     read-only stays at version 0 with no schema, read-write migrates
//     to v1 and takes the pre-migration snapshot)
//   - migration hard error when the snapshot file already exists
//     (DESIGN_SPEC 2.2: never overwrite a leftover snapshot)
//   - integrity_check and SchemaVersion on the MAIN catalog after a
//     migration (not just on the snapshot/export)
//   - DSN path whose parent directory does not exist
//   - corrupt (non-SQLite) file classification (DESIGN_SPEC 10.1)
//   - schema constraint semantics of the verbatim 2.3 DDL (FK, CHECK,
//     v_active_files view filtering)
//   - documented idempotent Close and the invalid-OpenMode guard

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// materializeV0 builds a minimal v0-shaped catalog: a valid SQLite
// database with no tables and user_version 0 (a brand-new, not-yet
// initialized catalog file).
func materializeV0(t *testing.T, path string) {
	t.Helper()
	raw := rawOpen(t, path)
	if _, err := raw.Exec("VACUUM"); err != nil {
		t.Fatalf("materializing v0 database: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close v0 database: %v", err)
	}
}

// userVersionOf reads PRAGMA user_version through a raw (unpolicy'd)
// connection, for asserting catalog state after failed operations.
func userVersionOf(t *testing.T, path string) int {
	t.Helper()
	raw := rawOpen(t, path)
	defer raw.Close()
	var v int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("read user_version of %q: %v", path, err)
	}
	return v
}

// Creating the same catalog twice is a hard KindOpen error, and the second
// (refused) Create leaves the first catalog fully intact and valid.
func TestCreateTwiceFailsAndPreservesCatalog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")
	mustCreate(t, path)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}

	var cerr *CatalogError
	if err := Create(path); !errors.As(err, &cerr) || cerr.Kind != KindOpen {
		t.Fatalf("second Create: err = %v, want KindOpen CatalogError", err)
	}

	// The refused Create must not have damaged the existing catalog.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read catalog: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("refused second Create modified the existing catalog file")
	}
	db := mustOpen(t, path, OpenReadWrite)
	if v, err := db.SchemaVersion(context.Background()); err != nil || v != 1 {
		t.Fatalf("catalog after refused Create: version = %d, err = %v, want 1, nil", v, err)
	}
	if got := integrityCheck(t, db.sql); got != "ok" {
		t.Fatalf("catalog integrity_check after refused Create = %q, want ok", got)
	}
}

// Open on a zero-byte file: SQLite treats a zero-length file as a fresh
// empty database. Documented behavior:
//   - Read-only open succeeds at user_version 0 with NO schema (read-only
//     never migrates, DESIGN_SPEC 2.2); queries against the catalog fail
//     with "no such table", which surfaces as a query error to callers.
//   - Read-write open runs the migration path: the catalog becomes v1
//     with the full schema, and the .pre-migrate-0.sqlite snapshot of the
//     empty state is taken first.
func TestOpenZeroByteFile(t *testing.T) {
	t.Run("read-only stays at version 0 with no schema", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "zero.sqlite")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatalf("seed zero-byte file: %v", err)
		}

		db := mustOpen(t, path, OpenReadOnly)
		v, err := db.SchemaVersion(context.Background())
		if err != nil {
			t.Fatalf("SchemaVersion: %v", err)
		}
		if v != 0 {
			t.Fatalf("user_version = %d, want 0 (read-only never migrates)", v)
		}
		if objs := catalogObjectNames(t, db.sql); len(objs) != 0 {
			t.Fatalf("read-only open of zero-byte file created %d schema objects, want 0", len(objs))
		}
		// The consequence callers see: no tables exist on this handle.
		if err := db.sql.QueryRow("SELECT count(*) FROM sets").Scan(new(int)); err == nil ||
			!strings.Contains(err.Error(), "no such table") {
			t.Fatalf("query on unmigrated zero-byte catalog: err = %v, want a no-such-table error", err)
		}
		// Read-only open leaves the file empty on disk.
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat catalog: %v", err)
		}
		if fi.Size() != 0 {
			t.Fatalf("read-only open grew the zero-byte file to %d bytes", fi.Size())
		}
	})

	t.Run("read-write migrates to v1 with a pre-migration snapshot", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "zero.sqlite")
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatalf("seed zero-byte file: %v", err)
		}

		db := mustOpen(t, path, OpenReadWrite)
		v, err := db.SchemaVersion(context.Background())
		if err != nil {
			t.Fatalf("SchemaVersion: %v", err)
		}
		if v != 1 {
			t.Fatalf("user_version = %d, want 1 after migration", v)
		}
		wantObjects(t, db.sql, v1Objects())
		if got := integrityCheck(t, db.sql); got != "ok" {
			t.Fatalf("migrated catalog integrity_check = %q, want ok", got)
		}
		if err := db.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}

		snapshot := preMigrateSnapshotPath(path, 0)
		if _, err := os.Stat(snapshot); err != nil {
			t.Fatalf("pre-migration snapshot missing: %v", err)
		}
		if sv := userVersionOf(t, snapshot); sv != 0 {
			t.Fatalf("snapshot user_version = %d, want the pre-migration value 0", sv)
		}
	})
}

// Migration with a leftover snapshot file is a hard error
// (DESIGN_SPEC 2.2; Open doc contract): SQLite refuses VACUUM INTO onto
// an existing file, so the migration aborts with KindMigration before any
// schema change, the catalog stays at its old version, and the leftover
// snapshot is never overwritten.
func TestMigrationHardErrorWhenSnapshotExists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")
	materializeV0(t, path)

	snapshot := preMigrateSnapshotPath(path, 0)
	leftover := []byte("leftover snapshot from an earlier attempt")
	if err := os.WriteFile(snapshot, leftover, 0o644); err != nil {
		t.Fatalf("seed leftover snapshot: %v", err)
	}

	db, err := Open(path, OpenReadWrite)
	if err == nil {
		db.Close()
		t.Fatal("Open with a pre-existing snapshot succeeded, want a migration error")
	}
	var cerr *CatalogError
	if !errors.As(err, &cerr) {
		t.Fatalf("Open: err = %v, want a *CatalogError", err)
	}
	if cerr.Kind != KindMigration {
		t.Fatalf("Open: Kind = %q, want %q", cerr.Kind, KindMigration)
	}
	if !strings.Contains(err.Error(), snapshot) {
		t.Errorf("Open: message %q does not name the conflicting snapshot %q", err.Error(), snapshot)
	}

	// No downgrade, no partial migration: the catalog is untouched at v0.
	if v := userVersionOf(t, path); v != 0 {
		t.Fatalf("user_version after failed migration = %d, want 0 (unchanged)", v)
	}
	raw := rawOpen(t, path)
	if objs := catalogObjectNames(t, raw); len(objs) != 0 {
		t.Fatalf("failed migration left %d schema objects, want none", len(objs))
	}

	// The leftover snapshot file is byte-identical: never overwritten.
	got, err := os.ReadFile(snapshot)
	if err != nil {
		t.Fatalf("re-read snapshot: %v", err)
	}
	if !bytes.Equal(got, leftover) {
		t.Fatal("failed migration modified the leftover snapshot file")
	}
}

// DESIGN_SPEC 12.3 extends integrity checks to the catalog itself, not
// only exports: after a v0 -> v1 migration the MAIN catalog passes
// PRAGMA integrity_check and reports its version through SchemaVersion on
// a live read-only handle.
func TestIntegrityCheckOnMigratedMainCatalog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")
	materializeV0(t, path)

	db := mustOpen(t, path, OpenReadWrite)
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	ro := mustOpen(t, path, OpenReadOnly)
	if got := integrityCheck(t, ro.sql); got != "ok" {
		t.Fatalf("migrated main catalog integrity_check = %q, want ok", got)
	}
	v, err := ro.SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("SchemaVersion on live read-only handle: %v", err)
	}
	if v != 1 {
		t.Fatalf("SchemaVersion on migrated catalog = %d, want 1", v)
	}
	wantObjects(t, ro.sql, v1Objects())
}

// A catalog path whose parent directory does not exist fails as a typed
// KindOpen error in every entry point: Create, Open read-write, and
// Open read-only. Nothing is created on disk. D2 enrichment (2026-10-19):
// every failure additionally NAMES the missing directory ("catalog
// directory does not exist: <dir> (create it first)") while keeping the
// KindOpen class (exit 4) - message enrichment, not a class change.
func TestMissingParentDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "does", "not", "exist", "catalog.sqlite")
	missing := filepath.Dir(path)

	err := Create(path)
	if err == nil {
		t.Fatal("Create succeeded in a missing directory, want a KindOpen CatalogError")
	}
	var cerr *CatalogError
	if !errors.As(err, &cerr) || cerr.Kind != KindOpen {
		t.Fatalf("Create in missing directory: err = %v, want KindOpen CatalogError", err)
	}
	if !strings.Contains(err.Error(), "catalog directory does not exist: "+missing) {
		t.Errorf("Create message does not name the missing directory: %v", err)
	}
	if !strings.Contains(err.Error(), "create it first") {
		t.Errorf("Create message lacks the create-it-first guidance: %v", err)
	}
	if _, statErr := os.Stat(filepath.Dir(path)); !os.IsNotExist(statErr) {
		t.Fatalf("Create created the missing parent directory (stat err = %v)", statErr)
	}

	for _, mode := range []OpenMode{OpenReadWrite, OpenReadOnly} {
		_, err := Open(path, mode)
		if !errors.As(err, &cerr) || cerr.Kind != KindOpen {
			t.Errorf("Open(missing dir, mode %d): err = %v, want KindOpen CatalogError", mode, err)
			continue
		}
		if !strings.Contains(err.Error(), "catalog directory does not exist: "+missing) {
			t.Errorf("Open(missing dir, mode %d) message does not name the missing directory: %v", mode, err)
		}
	}
	if _, statErr := os.Stat(filepath.Dir(path)); !os.IsNotExist(statErr) {
		t.Fatalf("Open created the missing parent directory (stat err = %v)", statErr)
	}
}

// The D2 missing-directory enrichment is MESSAGE-ONLY surgery on KindOpen
// failures: it fires ONLY when the parent directory truly does not
// exist. Every other failure in the same entry points keeps its exact
// classification and message - a missing catalog file in an EXISTING
// directory, an existing non-empty file refused by Create, and a
// corrupt database (KindCorrupt in both open modes).
func TestParentDirEnrichmentDoesNotFireOtherwise(t *testing.T) {
	dir := t.TempDir()

	// Missing FILE, existing directory: the pre-D2 message stands.
	missingFile := filepath.Join(dir, "absent.sqlite")
	_, err := Open(missingFile, OpenReadOnly)
	var cerr *CatalogError
	if !errors.As(err, &cerr) || cerr.Kind != KindOpen {
		t.Fatalf("Open(absent file, read-only): err = %v, want KindOpen CatalogError", err)
	}
	if strings.Contains(err.Error(), "catalog directory does not exist") {
		t.Errorf("existing-parent open was wrongly enriched: %v", err)
	}
	if !strings.Contains(err.Error(), "not found or unreadable") {
		t.Errorf("message lost the not-found guidance: %v", err)
	}

	// Create refusing to overwrite a non-empty existing file: unchanged.
	occupied := filepath.Join(dir, "occupied.sqlite")
	if werr := os.WriteFile(occupied, []byte("keep me"), 0o644); werr != nil {
		t.Fatalf("seed occupied file: %v", werr)
	}
	if err := Create(occupied); err == nil {
		t.Fatal("Create overwrote a non-empty file, want an error")
	} else {
		if !errors.As(err, &cerr) || cerr.Kind != KindOpen {
			t.Errorf("Create(occupied): err = %v, want KindOpen CatalogError", err)
		}
		if strings.Contains(err.Error(), "catalog directory does not exist") {
			t.Errorf("existing-parent Create was wrongly enriched: %v", err)
		}
	}

	// Corrupt database: KindCorrupt, message unchanged.
	corrupt := filepath.Join(dir, "corrupt.sqlite")
	if werr := os.WriteFile(corrupt, []byte("this is definitely not a sqlite database"), 0o644); werr != nil {
		t.Fatalf("seed corrupt file: %v", werr)
	}
	for _, mode := range []OpenMode{OpenReadWrite, OpenReadOnly} {
		_, err := Open(corrupt, mode)
		if !errors.As(err, &cerr) || cerr.Kind != KindCorrupt {
			t.Errorf("Open(corrupt, mode %d): err = %v, want KindCorrupt CatalogError", mode, err)
			continue
		}
		if strings.Contains(err.Error(), "catalog directory does not exist") {
			t.Errorf("corrupt open (mode %d) was wrongly enriched: %v", mode, err)
		}
	}
}

// A file that exists but is not a usable SQLite database is classified
// KindCorrupt per the ErrorKind contract ("the file exists but is not a
// usable SQLite database") and DESIGN_SPEC 10.1, in both open modes.
func TestOpenCorruptFileClassified(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.sqlite")
	if err := os.WriteFile(path, []byte("this is definitely not a sqlite database"), 0o644); err != nil {
		t.Fatalf("seed corrupt file: %v", err)
	}

	for _, mode := range []OpenMode{OpenReadWrite, OpenReadOnly} {
		_, err := Open(path, mode)
		var cerr *CatalogError
		if !errors.As(err, &cerr) {
			t.Errorf("Open(corrupt, mode %d): err = %v, want a *CatalogError", mode, err)
			continue
		}
		if cerr.Kind != KindCorrupt {
			t.Errorf("Open(corrupt, mode %d): Kind = %q, want %q (err: %v)",
				mode, cerr.Kind, KindCorrupt, err)
		}
	}
}

// Behavioral assertion that the verbatim 2.3 DDL carries its designed
// semantics on a live catalog: foreign keys enforced (files ->
// runs/sets REFERENCES), CHECK constraints enforced (runs.kind), and the
// v_active_files view filters out missing and superseded rows.
func TestSchemaConstraintsAndActiveView(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")
	mustCreate(t, path)
	db := mustOpen(t, path, OpenReadWrite)
	ctx := context.Background()

	exec := func(query string, args ...any) error {
		t.Helper()
		_, err := db.sql.ExecContext(ctx, query, args...)
		return err
	}
	if err := exec(`INSERT INTO sets (set_id, source_root, created_at, tool_version)
		VALUES ('S-1', '/src', 1, 'test')`); err != nil {
		t.Fatalf("insert set: %v", err)
	}
	if err := exec(`INSERT INTO runs (run_id, set_id, kind, root, started_at, status, tool_version)
		VALUES (1, 'S-1', 'ingest', '/src', 1, 'ok', 'test')`); err != nil {
		t.Fatalf("insert run: %v", err)
	}

	// Foreign keys are ON (2.1): a file row referencing a nonexistent run
	// or set is rejected.
	err := exec(`INSERT INTO files (set_id, relpath, entry_kind, size, mode, uid, gid,
			mtime_ns, first_seen_run, last_seen_run, first_seen_at)
		VALUES ('S-1', 'fk', 'regular', 0, 0, 0, 0, 0, 999, 999, 1)`)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Fatalf("file with bogus run reference: err = %v, want a foreign-key violation", err)
	}
	err = exec(`INSERT INTO runs (run_id, set_id, kind, root, started_at, status, tool_version)
		VALUES (2, 'no-such-set', 'ingest', '/src', 1, 'ok', 'test')`)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "foreign key") {
		t.Fatalf("run with bogus set reference: err = %v, want a foreign-key violation", err)
	}

	// CHECK constraints are enforced: runs.kind outside the 2.3 list is
	// rejected.
	err = exec(`INSERT INTO runs (run_id, set_id, kind, root, started_at, status, tool_version)
		VALUES (3, 'S-1', 'bogus-kind', '/src', 1, 'ok', 'test')`)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "check constraint") {
		t.Fatalf("run with invalid kind: err = %v, want a check-constraint violation", err)
	}

	// v_active_files semantics (2.3): active and not superseded.
	insertFile := func(relpath, status string, supersededBy any) {
		t.Helper()
		if err := exec(`INSERT INTO files (set_id, relpath, entry_kind, size, mode, uid, gid,
				mtime_ns, first_seen_run, last_seen_run, first_seen_at, status, superseded_by)
			VALUES ('S-1', ?, 'regular', 0, 0, 0, 0, 0, 1, 1, 1, ?, ?)`,
			relpath, status, supersededBy); err != nil {
			t.Fatalf("insert file %q: %v", relpath, err)
		}
	}
	insertFile("a.txt", "active", nil) // in the view
	insertFile("b.txt", "missing", nil)
	insertFile("c.txt", "active", 1) // superseded by a.txt
	insertFile("d.txt", "unreadable", nil)
	insertFile("e.txt", "changed_during_scan", nil)

	rows, err := db.sql.QueryContext(ctx, `SELECT relpath FROM v_active_files`)
	if err != nil {
		t.Fatalf("query v_active_files: %v", err)
	}
	defer rows.Close()
	var active []string
	for rows.Next() {
		var relpath string
		if err := rows.Scan(&relpath); err != nil {
			t.Fatalf("scan v_active_files: %v", err)
		}
		active = append(active, relpath)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate v_active_files: %v", err)
	}
	if len(active) != 1 || active[0] != "a.txt" {
		t.Fatalf("v_active_files = %v, want exactly [a.txt]", active)
	}
}

// Close is documented safe to call more than once (migrate.go doc); a
// double Close must not error or panic.
func TestCloseIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")
	mustCreate(t, path)

	db, err := Open(path, OpenReadWrite)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// An OpenMode outside the two defined values is rejected as a typed
// KindOpen error before any database work happens.
func TestOpenInvalidModeRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")
	mustCreate(t, path)

	_, err := Open(path, OpenMode(42))
	var cerr *CatalogError
	if !errors.As(err, &cerr) || cerr.Kind != KindOpen {
		t.Fatalf("Open(invalid mode): err = %v, want KindOpen CatalogError", err)
	}
	if !strings.Contains(err.Error(), "invalid OpenMode") {
		t.Errorf("Open(invalid mode): message %q does not explain the bad mode", err.Error())
	}
}
