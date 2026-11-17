package catalog

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// --- helpers ----------------------------------------------------------------

// mustCreate creates a fresh v1 catalog and fails the test on error.
func mustCreate(t *testing.T, path string) {
	t.Helper()
	if err := Create(path); err != nil {
		t.Fatalf("Create(%q): %v", path, err)
	}
}

// mustOpen opens the catalog and fails the test on error, registering Close.
func mustOpen(t *testing.T, path string, mode OpenMode, opts ...Option) *DB {
	t.Helper()
	db, err := Open(path, mode, opts...)
	if err != nil {
		t.Fatalf("Open(%q, %d): %v", path, mode, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// integrityCheck runs PRAGMA integrity_check and returns the first result row.
func integrityCheck(t *testing.T, sqldb *sql.DB) string {
	t.Helper()
	var result string
	if err := sqldb.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		t.Fatalf("PRAGMA integrity_check: %v", err)
	}
	return result
}

// rawOpen opens a plain database/sql handle without the catalog policy, for
// test-side stamping and inspection. Callers close it.
func rawOpen(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open(driverName, path)
	if err != nil {
		t.Fatalf("sql.Open(%q): %v", path, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// catalogObjectNames returns the name->type map of every table, view, and
// index in the database.
func catalogObjectNames(t *testing.T, sqldb *sql.DB) map[string]string {
	t.Helper()
	rows, err := sqldb.Query("SELECT name, type FROM sqlite_master")
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var name, typ string
		if err := rows.Scan(&name, &typ); err != nil {
			t.Fatalf("scan sqlite_master: %v", err)
		}
		out[name] = typ
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate sqlite_master: %v", err)
	}
	return out
}

// wantObjects asserts every name->type pair exists in sqlite_master.
func wantObjects(t *testing.T, sqldb *sql.DB, want map[string]string) {
	t.Helper()
	got := catalogObjectNames(t, sqldb)
	for name, typ := range want {
		if got[name] != typ {
			t.Errorf("sqlite_master: want %s %q, got type %q", typ, name, got[name])
		}
	}
}

// stampUserVersion force-sets PRAGMA user_version (test-side simulation of
// an older or future catalog).
func stampUserVersion(t *testing.T, path string, version int) {
	t.Helper()
	db := rawOpen(t, path)
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Fatalf("stamp user_version = %d: %v", version, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close stamp connection: %v", err)
	}
}

// v1Objects enumerates every table, index, and view of DESIGN_SPEC 2.3.
// T2.2 (2026-10-19 architect ruling): the `directories` table and its
// two indexes joined the v1 DDL (PM-ratified: folded into v1 directly,
// no migration - the whole v1 executes as one script; pre-release).
func v1Objects() map[string]string {
	return map[string]string{
		// tables (8)
		"sets":           "table",
		"runs":           "table",
		"files":          "table",
		"directories":    "table",
		"chunks":         "table",
		"images":         "table",
		"image_contents": "table",
		"run_events":     "table",
		// indexes (8)
		"idx_files_unassigned":       "index",
		"idx_files_path":             "index",
		"idx_files_linkgroup":        "index",
		"idx_files_image":            "index",
		"idx_directories_image":      "index",
		"idx_directories_unassigned": "index",
		"idx_chunks_image":           "index",
		"idx_run_events_run":         "index",
		// convenience view
		"v_active_files": "view",
	}
}

// --- tests ------------------------------------------------------------------

// T0.3 acceptance: a fresh v1 catalog is created, user_version equals 1,
// and a VACUUM INTO export (via the internal helper that later becomes
// export.go in T3.2) opens read-only and passes PRAGMA integrity_check.
func TestCreateFreshCatalogAndValidExport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")
	mustCreate(t, path)

	db := mustOpen(t, path, OpenReadWrite)
	version, err := db.SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if SchemaVersion != 1 {
		t.Fatalf("SchemaVersion const = %d, want 1 at T0.3", SchemaVersion)
	}
	if version != SchemaVersion {
		t.Fatalf("user_version = %d, want %d", version, SchemaVersion)
	}

	// Prove the VACUUM INTO mechanism (pre-migration snapshot / FR-034
	// export) produces a valid, openable database.
	exportPath := filepath.Join(dir, "export.sqlite")
	if err := vacuumInto(context.Background(), db.sql, exportPath); err != nil {
		t.Fatalf("vacuumInto: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	export := mustOpen(t, exportPath, OpenReadOnly)
	if got := integrityCheck(t, export.sql); got != "ok" {
		t.Fatalf("export integrity_check = %q, want ok", got)
	}
	exportVersion, err := export.SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("export SchemaVersion: %v", err)
	}
	if exportVersion != 1 {
		t.Fatalf("export user_version = %d, want 1", exportVersion)
	}
	wantObjects(t, export.sql, v1Objects())
}

// T0.3 acceptance: the DESIGN_SPEC 2.3 DDL executes verbatim; after
// execution PRAGMA integrity_check is ok and all 7 tables, 6 indexes, and
// the v_active_files view exist.
func TestSchemaDDLExecutesVerbatim(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "raw.sqlite")
	db := rawOpen(t, path)

	if _, err := db.Exec(schemaV1DDL); err != nil {
		t.Fatalf("executing schemaV1DDL verbatim: %v", err)
	}
	if got := integrityCheck(t, db); got != "ok" {
		t.Fatalf("integrity_check = %q, want ok", got)
	}
	wantObjects(t, db, v1Objects())
}

// T0.3 acceptance: a catalog stamped with a future user_version is rejected
// on open in BOTH modes with the typed schema-too-new error, and the
// catalog file is left byte-identical (no downgrade path, no writes).
func TestOpenRejectsFutureSchemaVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")
	mustCreate(t, path)
	stampUserVersion(t, path, 999)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}

	for _, mode := range []OpenMode{OpenReadWrite, OpenReadOnly} {
		db, err := Open(path, mode)
		if err == nil {
			db.Close()
			t.Fatalf("Open(mode %d) on future schema succeeded, want error", mode)
		}
		if !errors.Is(err, ErrSchemaTooNew) {
			t.Errorf("Open(mode %d): errors.Is(err, ErrSchemaTooNew) = false: %v", mode, err)
		}
		var cerr *CatalogError
		if !errors.As(err, &cerr) {
			t.Errorf("Open(mode %d): not a *CatalogError: %v", mode, err)
			continue
		}
		if cerr.Kind != KindSchemaTooNew {
			t.Errorf("Open(mode %d): Kind = %q, want %q", mode, cerr.Kind, KindSchemaTooNew)
		}
		if !strings.Contains(err.Error(), "newer") {
			t.Errorf("Open(mode %d): message %q does not explain the future schema", mode, err.Error())
		}
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read catalog: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("catalog file was modified by the rejected open")
	}
}

// T0.3 acceptance: every opened connection reports the four settings of
// DESIGN_SPEC 2.1 (journal_mode=WAL, busy_timeout=5000, foreign_keys=ON,
// synchronous=NORMAL); journal_mode is persistent in the file, so a
// read-only connection reports it too.
func TestConnectionPolicyPragmas(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")
	mustCreate(t, path)

	checkPragmas := func(t *testing.T, db *DB) {
		t.Helper()
		var journal string
		if err := db.sql.QueryRow("PRAGMA journal_mode").Scan(&journal); err != nil {
			t.Fatalf("PRAGMA journal_mode: %v", err)
		}
		if !strings.EqualFold(journal, "wal") {
			t.Errorf("journal_mode = %q, want wal", journal)
		}
		var busy int
		if err := db.sql.QueryRow("PRAGMA busy_timeout").Scan(&busy); err != nil {
			t.Fatalf("PRAGMA busy_timeout: %v", err)
		}
		if busy != 5000 {
			t.Errorf("busy_timeout = %d, want 5000", busy)
		}
		var fk int
		if err := db.sql.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatalf("PRAGMA foreign_keys: %v", err)
		}
		if fk != 1 {
			t.Errorf("foreign_keys = %d, want 1", fk)
		}
		var sync int
		if err := db.sql.QueryRow("PRAGMA synchronous").Scan(&sync); err != nil {
			t.Fatalf("PRAGMA synchronous: %v", err)
		}
		if sync != 1 { // 1 == NORMAL (0=OFF, 2=FULL, 3=EXTRA)
			t.Errorf("synchronous = %d, want 1 (NORMAL)", sync)
		}
	}

	t.Run("read-write", func(t *testing.T) {
		checkPragmas(t, mustOpen(t, path, OpenReadWrite))
	})
	t.Run("read-only", func(t *testing.T) {
		checkPragmas(t, mustOpen(t, path, OpenReadOnly))
	})
}

// T0.3 acceptance: a read-only connection cannot INSERT, confirming the
// file:...?mode=ro URI behaviour of DESIGN_SPEC 2.1.
func TestReadOnlyOpenRejectsWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")
	mustCreate(t, path)

	db := mustOpen(t, path, OpenReadOnly)
	_, err := db.sql.Exec(`INSERT INTO sets (set_id, source_root, created_at, tool_version)
		VALUES ('S-1', '/src', 0, 'test')`)
	if err == nil {
		t.Fatal("INSERT on a read-only connection succeeded, want error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "readonly") {
		t.Errorf("INSERT error = %q, want a readonly-database error", err)
	}
}

// T0.3 acceptance: the pre-migration path. A v0-shaped catalog is opened
// for mutation: the .pre-migrate-0.sqlite snapshot exists and is itself a
// valid openable database, and user_version advanced to 1 with the full v1
// schema in place.
func TestPreMigrationSnapshotAndUpgrade(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")

	// Construct a minimal v0-shaped file: a valid SQLite database with no
	// tables and user_version 0 (a brand-new tool-less catalog).
	raw := rawOpen(t, path)
	if _, err := raw.Exec("VACUUM"); err != nil {
		t.Fatalf("materializing v0 database: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close v0 database: %v", err)
	}

	db := mustOpen(t, path, OpenReadWrite)
	version, err := db.SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != 1 {
		t.Fatalf("user_version after migration = %d, want 1", version)
	}
	wantObjects(t, db.sql, v1Objects())
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// The safety snapshot exists at the 2.2 path and is a valid database
	// holding the PRE-migration state.
	snapshot := preMigrateSnapshotPath(path, 0)
	if _, err := os.Stat(snapshot); err != nil {
		t.Fatalf("pre-migration snapshot missing: %v", err)
	}
	snap := mustOpen(t, snapshot, OpenReadOnly)
	if got := integrityCheck(t, snap.sql); got != "ok" {
		t.Fatalf("snapshot integrity_check = %q, want ok", got)
	}
	snapVersion, err := snap.SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("snapshot SchemaVersion: %v", err)
	}
	if snapVersion != 0 {
		t.Fatalf("snapshot user_version = %d, want the pre-migration value 0", snapVersion)
	}
	if objs := catalogObjectNames(t, snap.sql); len(objs) != 0 {
		t.Fatalf("snapshot contains %d objects, want the pre-migration empty database", len(objs))
	}
}

// recordingLocker is a fake Locker that records acquisition modes and
// releases, standing in for the T0.4 flock implementation.
type recordingLocker struct {
	mu       sync.Mutex
	modes    []LockMode
	releases int
	err      error
}

func (l *recordingLocker) Acquire(mode LockMode) (func() error, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.err != nil {
		return nil, l.err
	}
	l.modes = append(l.modes, mode)
	return func() error {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.releases++
		return nil
	}, nil
}

// The locker seam (DESIGN_SPEC 2.1 as amended 2026-10-18, implemented by
// T0.4 lock.go): MUTATING opens acquire the exclusive lock and Close
// releases it exactly once; read-only opens take NO advisory lock at all
// (mode=ro plus WAL is their consistency mechanism), so the injected
// locker is never consulted for them; locker errors abort the open
// untouched.
func TestOpenLockerSeam(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")
	mustCreate(t, path)

	locker := &recordingLocker{}
	db := mustOpen(t, path, OpenReadWrite, WithLocker(locker))
	if len(locker.modes) != 1 || locker.modes[0] != LockExclusive {
		t.Fatalf("read-write open acquired modes %v, want [LockExclusive]", locker.modes)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if locker.releases != 1 {
		t.Fatalf("releases = %d after Close, want 1", locker.releases)
	}

	// A read-only open acquires NOTHING: the locker records no further
	// modes and Close releases nothing further.
	db = mustOpen(t, path, OpenReadOnly, WithLocker(locker))
	if len(locker.modes) != 1 || locker.modes[0] != LockExclusive {
		t.Fatalf("after read-only open, acquired modes %v, want [LockExclusive] "+
			"(read-only opens take no advisory lock)", locker.modes)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if locker.releases != 1 {
		t.Fatalf("releases = %d after read-only Close, want 1 (readers release nothing)", locker.releases)
	}

	// A failing locker aborts a MUTATING open before any database work.
	wantErr := errors.New("lock busy")
	if _, err := Open(path, OpenReadWrite, WithLocker(&recordingLocker{err: wantErr})); !errors.Is(err, wantErr) {
		t.Fatalf("Open with failing locker: err = %v, want the locker error", err)
	}
}

// Opening a nonexistent catalog is a typed KindOpen error in both modes.
func TestOpenMissingCatalogFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.sqlite")
	for _, mode := range []OpenMode{OpenReadWrite, OpenReadOnly} {
		_, err := Open(path, mode)
		var cerr *CatalogError
		if !errors.As(err, &cerr) || cerr.Kind != KindOpen {
			t.Errorf("Open(missing, mode %d): err = %v, want KindOpen CatalogError", mode, err)
		}
	}
}

// Create refuses to overwrite a non-empty catalog but initializes an
// existing empty file in place.
func TestCreateRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")

	if err := os.WriteFile(path, []byte("not a database"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	var cerr *CatalogError
	if err := Create(path); !errors.As(err, &cerr) || cerr.Kind != KindOpen {
		t.Fatalf("Create over non-empty file: err = %v, want KindOpen CatalogError", err)
	}

	// An existing EMPTY file is initialized in place.
	empty := filepath.Join(dir, "empty.sqlite")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatalf("seed empty file: %v", err)
	}
	mustCreate(t, empty)
	db := mustOpen(t, empty, OpenReadWrite)
	version, err := db.SchemaVersion(context.Background())
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != 1 {
		t.Fatalf("user_version = %d, want 1", version)
	}
}

// Dependency condition asserted (DESIGN_SPEC 11, NOTES.md gate): go.mod
// requires modernc.org/sqlite at EXACTLY v1.60.1 (no floating range, no
// other version), and `go mod verify` passes.
func TestGoModPinsSQLiteExactly(t *testing.T) {
	moduleRoot := filepath.Join("..", "..")
	gomod, err := os.ReadFile(filepath.Join(moduleRoot, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}

	exact := regexp.MustCompile(`(?m)^[ \t]*(require[ \t]+)?modernc\.org/sqlite[ \t]+v1\.60\.1[ \t]*(// .*)?$`)
	if !exact.Match(gomod) {
		t.Fatalf("go.mod does not pin modernc.org/sqlite v1.60.1 exactly:\n%s", gomod)
	}
	// Any other modernc.org/sqlite require line (a range or a different
	// version) is a gate violation.
	anyVersion := regexp.MustCompile(`(?m)^[ \t]*(require[ \t]+)?modernc\.org/sqlite[ \t]+v[0-9]`)
	if n := len(anyVersion.FindAll(gomod, -1)); n != 1 {
		t.Fatalf("go.mod has %d modernc.org/sqlite require lines, want exactly 1", n)
	}

	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; skipping go mod verify")
	}
	cmd := exec.Command("go", "mod", "verify")
	cmd.Dir = moduleRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go mod verify failed: %v\n%s", err, out)
	}
}
