package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	// Pure-Go SQLite driver (DESIGN_SPEC 11, exact pin v1.60.1). It
	// self-registers under the driver name "sqlite"; CGO is not required.
	_ "modernc.org/sqlite"
)

// driverName is the database/sql driver name registered by
// modernc.org/sqlite (DESIGN_SPEC 11). Only this package opens SQL
// connections (DESIGN_SPEC 1.2 layering).
const driverName = "sqlite"

// OpenMode selects how Open opens the catalog (DESIGN_SPEC 2.1).
//
// OpenReadOnly is the zero value so an accidental zero-value call cannot
// take the mutating path.
type OpenMode int

const (
	// OpenReadOnly opens the catalog read-only via the URI flag
	// "file:...?mode=ro". Read-only commands (report, verify-media,
	// validate, restore-read-only) use it; writes fail at the SQLite
	// level. An older catalog is NOT migrated in this mode.
	OpenReadOnly OpenMode = iota
	// OpenReadWrite opens the catalog for mutation and runs the
	// forward-migration path (snapshot + migrate) when the catalog is
	// older than SchemaVersion. Mutating commands (scan, plan, copy,
	// build, restore-with-marking, clear-assignments) use it.
	OpenReadWrite
)

// LockMode distinguishes the advisory-lock acquisition styles of
// DESIGN_SPEC 2.1 (flock LOCK_EX for mutating runs; LOCK_SH retained for
// the Locker seam but unused by Open since the 2026-10-18 amendment -
// read-only runs take no flock at all).
type LockMode int

const (
	// LockShared is LOCK_SH: compatible with other shared holders,
	// incompatible with an exclusive holder. NOT requested by Open since
	// the amended 2.1 - read-only opens take no advisory lock (mode=ro
	// plus the WAL snapshot is their consistency mechanism); it remains
	// part of the Locker seam for direct use and tests.
	LockShared LockMode = iota
	// LockExclusive admits exactly one mutating run at a time
	// (single-writer model, D-11); contention is reported immediately,
	// never waited out. This is the mode Open acquires for read-write
	// opens.
	LockExclusive
)

// Locker is the advisory-lock seam used by Open (DESIGN_SPEC 2.1 as
// amended 2026-10-18, 2.4, D-11).
//
// lock.go supplies the default flock implementation over the sidecar file
// "<catalog>.lock" (FlockLocker). The flock enforces SINGLE-WRITER only:
// Open acquires it (LockExclusive, LOCK_EX|LOCK_NB) for read-write
// opens, and a contended acquisition fails immediately with the
// CatalogBusy-class error (exit 5), never a blocking wait. Read-only
// opens take NO lock at all - mode=ro plus the WAL snapshot is their
// consistency mechanism (amended 2.1) - so Acquire is never called for
// them. The returned release function is invoked by (*DB).Close and is
// safe to call more than once.
//
// WithLocker exists so tests can inject a fake locker on the mutating
// path. See FlockLocker for the acquisition semantics and the sidecar
// lifecycle.
type Locker interface {
	// Acquire takes the advisory lock and returns a release function.
	Acquire(mode LockMode) (release func() error, err error)
}

// options carries the injectable Open dependencies.
type options struct {
	locker Locker
}

// Option customizes Open.
type Option func(*options)

// WithLocker injects the advisory-lock implementation used by MUTATING
// opens, replacing the default flock Locker of lock.go. Read-only opens
// never consult the locker (they take no lock, amended DESIGN_SPEC 2.1).
// Passing a nil Locker is equivalent to passing no option (the default
// flock Locker is used).
func WithLocker(l Locker) Option {
	return func(o *options) {
		if l != nil {
			o.locker = l
		}
	}
}

// ErrorKind classifies catalog failures within the CatalogError class of
// the DESIGN_SPEC 10.1 taxonomy. T0.5 maps every *CatalogError to exit 4;
// the CatalogBusy class (exit 5) is separate and typed by the lock
// implementation (lock.go: *CatalogBusy wrapping ErrCatalogBusy), because
// 10.1 gives it its own exit code.
type ErrorKind string

const (
	// KindOpen: the catalog cannot be opened (missing, unreadable).
	KindOpen ErrorKind = "open"
	// KindCorrupt: the file exists but is not a usable SQLite database.
	KindCorrupt ErrorKind = "corrupt"
	// KindSchemaTooNew: user_version is newer than this tool understands;
	// there is no downgrade path (DESIGN_SPEC 2.1, 2.2).
	KindSchemaTooNew ErrorKind = "schema-too-new"
	// KindMigration: the pre-migration snapshot or a migration step
	// failed (DESIGN_SPEC 2.2).
	KindMigration ErrorKind = "migration"
)

// ErrSchemaTooNew is the sentinel for the hard "future schema" rejection
// (DESIGN_SPEC 2.1: opening a catalog whose user_version is newer than the
// tool understands is a hard error, exit 4). It is always wrapped in a
// *CatalogError with Kind KindSchemaTooNew; callers may classify with
// errors.Is or errors.As.
var ErrSchemaTooNew = errors.New("catalog schema is newer than this tool")

// CatalogError is the typed error of the CatalogError class
// (DESIGN_SPEC 10.1): cannot open, corrupt, schema too new, migration
// failure. T0.5 maps it to exit 4 and prints its message to stderr.
type CatalogError struct {
	Kind ErrorKind
	Path string // catalog path the error refers to ("" when unknown)
	Err  error  // underlying cause, always non-nil
}

func (e *CatalogError) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("catalog: %s error at %s: %v", e.Kind, e.Path, e.Err)
	}
	return fmt.Sprintf("catalog: %s error: %v", e.Kind, e.Err)
}

func (e *CatalogError) Unwrap() error { return e.Err }

// DB is an open catalog handle. It wraps the *sql.DB pool plus the catalog
// path; the pool stays unexported so no code outside this package can
// issue SQL (DESIGN_SPEC 1.2 layering). Later tasks add query methods here.
type DB struct {
	sql     *sql.DB
	path    string
	release func() error // advisory-lock release (lock.go; idempotent)
}

// Path returns the absolute path of the catalog file.
func (db *DB) Path() string { return db.path }

// SchemaVersion reads PRAGMA user_version: the schema version stamped into
// the catalog file (DESIGN_SPEC 2.2). The on-media manifest records it
// (DESIGN_SPEC 6.6).
func (db *DB) SchemaVersion(ctx context.Context) (int, error) {
	return userVersion(ctx, db.sql)
}

// Close releases the connection pool and the advisory lock (if any). It is
// safe to call more than once; the lock is released at most once.
func (db *DB) Close() error {
	var errs []error
	if db.sql != nil {
		if err := db.sql.Close(); err != nil {
			errs = append(errs, fmt.Errorf("catalog: closing connection pool: %w", err))
		}
		db.sql = nil
	}
	if db.release != nil {
		if err := db.release(); err != nil {
			errs = append(errs, fmt.Errorf("catalog: releasing advisory lock: %w", err))
		}
		db.release = nil
	}
	return errors.Join(errs...)
}

// Create initializes a fresh catalog at path with the current schema
// (DESIGN_SPEC 2.2, version 1). It refuses to overwrite a non-empty file;
// an existing empty file is initialized in place. The advisory lock is not
// taken here: Create is a bootstrap primitive, and the mutating commands
// that call Open hold the exclusive lock for their whole run (Open takes
// it before any database work, per T0.4 lock.go).
func Create(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return &CatalogError{Kind: KindOpen, Path: path, Err: err}
	}
	if fi, statErr := os.Stat(abs); statErr == nil {
		if fi.Size() > 0 {
			return &CatalogError{
				Kind: KindOpen,
				Path: abs,
				Err:  errors.New("refusing to overwrite existing non-empty catalog file"),
			}
		}
	} else if !os.IsNotExist(statErr) {
		return &CatalogError{Kind: KindOpen, Path: abs, Err: statErr}
	}

	sqldb, err := sql.Open(driverName, catalogDSN(abs, OpenReadWrite))
	if err != nil {
		return enrichMissingParentDir(abs, classifyOpenErr(abs, err))
	}
	defer sqldb.Close() //nolint:errcheck // best-effort pool close; every path below already returns a typed result or a successful *DB
	// Ping forces the connection open so the DSN pragmas of 2.1 are applied
	// and any corruption surfaces here, not at first query.
	if err := sqldb.Ping(); err != nil {
		return enrichMissingParentDir(abs, classifyOpenErr(abs, err))
	}
	if err := applyMigrations(context.Background(), sqldb, 0); err != nil {
		return err
	}
	return nil
}

// Open opens the catalog at path (DESIGN_SPEC 2.1).
//
// Every connection carries the 2.1 policy (journal_mode=WAL,
// busy_timeout=5000, foreign_keys=ON, synchronous=NORMAL) via DSN pragmas,
// so the policy holds for every pooled connection, not just the first.
//
// Locking (DESIGN_SPEC 2.1 as amended 2026-10-18, 2.4): read-write opens
// acquire the exclusive advisory lock BEFORE any database work - before
// the pragma Ping, the user_version check, and the migration path - and
// hold it until (*DB).Close; with no WithLocker injection the default is
// the flock implementation of lock.go (FlockLocker over
// "<catalog>.lock", created if absent, never deleted). The flock
// enforces single-WRITER only: a contended acquisition fails
// immediately with *CatalogBusy (exit 5, DESIGN_SPEC 10.1), and a
// lock-file infrastructure failure (for example a missing parent
// directory, which also prevents creating the sidecar) is a KindOpen
// error. Read-only opens take NO advisory lock: they open mode=ro via
// the URI flag and rely on the WAL snapshot for consistency, so they run
// alongside a writer and can never fail with the busy class from lock
// contention (exit 4 catalog errors still apply). WithLocker replaces
// the default locker for tests.
//
// ReadOnly opens via the URI flag "file:...?mode=ro" and never writes.
//
// ReadWrite runs the migration path of 2.2: when the catalog's
// user_version is older than SchemaVersion, the exclusive advisory lock is
// already held, a safety snapshot is taken with
// VACUUM INTO '<catalog>.pre-migrate-<version>.sqlite', then each pending
// migration runs in its own transaction and user_version is advanced. The
// snapshot is never overwritten; SQLite refuses VACUUM INTO onto an
// existing file, so a leftover snapshot from an earlier attempt is a hard
// migration error rather than silent data loss.
//
// There is no downgrade path: a catalog stamped with a newer user_version
// is rejected with ErrSchemaTooNew (wrapped in *CatalogError,
// KindSchemaTooNew) in BOTH modes, before any write can occur.
func Open(path string, mode OpenMode, opts ...Option) (*DB, error) {
	var cfg options
	for _, opt := range opts {
		opt(&cfg)
	}
	if mode != OpenReadOnly && mode != OpenReadWrite {
		return nil, &CatalogError{
			Kind: KindOpen,
			Path: path,
			Err:  fmt.Errorf("invalid OpenMode %d", mode),
		}
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, &CatalogError{Kind: KindOpen, Path: path, Err: err}
	}

	// Advisory lock first, held for the whole mutating run (DESIGN_SPEC
	// 2.1 as amended 2026-10-18, single-writer per 2.4). Read-only opens
	// take NO advisory lock: mode=ro plus the WAL snapshot is their
	// consistency mechanism, so they run alongside a writer. The default
	// locker is the flock implementation of lock.go; WithLocker replaces
	// it (tests).
	var release func() error
	if mode == OpenReadWrite {
		locker := cfg.locker
		if locker == nil {
			locker = NewFlockLocker(abs)
		}
		release, err = locker.Acquire(LockExclusive)
		if err != nil {
			// The locker classifies its own errors (*CatalogBusy, exit 5;
			// KindOpen for lock-file infrastructure failures); pass them
			// through untouched - except that a KindOpen failure whose
			// cause is a MISSING PARENT DIRECTORY is enriched to name
			// that directory (message enrichment only, D2; see
			// enrichMissingParentDir). The sidecar shares the catalog's
			// directory, so a mutating open whose parent directory is
			// gone fails HERE, before any database work.
			return nil, enrichMissingParentDir(abs, err)
		}
	}
	abort := func(err error) (*DB, error) {
		if release != nil {
			if rerr := release(); rerr != nil {
				err = errors.Join(err, rerr)
			}
		}
		return nil, err
	}

	if _, err := os.Stat(abs); err != nil {
		return abort(enrichMissingParentDir(abs, &CatalogError{
			Kind: KindOpen,
			Path: abs,
			Err:  fmt.Errorf("catalog file not found or unreadable (create one first): %w", err),
		}))
	}

	sqldb, err := sql.Open(driverName, catalogDSN(abs, mode))
	if err != nil {
		return abort(classifyOpenErr(abs, err))
	}
	if err := sqldb.Ping(); err != nil {
		_ = sqldb.Close()
		return abort(classifyOpenErr(abs, err))
	}

	version, err := userVersion(context.Background(), sqldb)
	if err != nil {
		_ = sqldb.Close()
		return abort(&CatalogError{
			Kind: KindCorrupt,
			Path: abs,
			Err:  fmt.Errorf("reading PRAGMA user_version: %w", err),
		})
	}

	// Hard rejection of future schemas, both modes, before any write
	// (DESIGN_SPEC 2.1, 2.2: no downgrade path ever).
	if version > SchemaVersion {
		_ = sqldb.Close()
		return abort(&CatalogError{
			Kind: KindSchemaTooNew,
			Path: abs,
			Err: fmt.Errorf(
				"%w: catalog is at version %d, this tool understands up to %d; "+
					"refusing to operate on a future schema (no downgrade path)",
				ErrSchemaTooNew, version, SchemaVersion),
		})
	}

	// Forward migration, mutating opens only (DESIGN_SPEC 2.2).
	if mode == OpenReadWrite && version < SchemaVersion {
		if err := migrateUp(context.Background(), sqldb, abs, version); err != nil {
			_ = sqldb.Close()
			return abort(err)
		}
	}

	// Folded-object repair (T2.2 hazard, see foldedV1RepairDDL in
	// schema.go): a catalog stamped CURRENT-version by a PRE-T2.2
	// binary lacks the `directories` table that later folded into the
	// v1 DDL with NO migration step (PM-ratified, pre-release), so the
	// version check above cannot heal it - the stamp already matches.
	// Mutating opens repair it here, once, idempotently. The ordering
	// matters: the repair runs AFTER the migration path, so a genuine
	// version upgrade (whose full v1 script already creates the table)
	// is never raced by the repair, and the exclusive advisory lock
	// taken at the top of Open guarantees no other writer interleaves.
	// Read-only opens deliberately never repair (mode=ro cannot write;
	// against a pre-T2.2 catalog they fail loudly on the missing
	// table, and the first mutating command heals the catalog).
	if mode == OpenReadWrite {
		if err := ensureFoldedV1Objects(context.Background(), sqldb, abs); err != nil {
			_ = sqldb.Close()
			return abort(err)
		}
	}

	return &DB{sql: sqldb, path: abs, release: release}, nil
}

// migrateUp brings an older catalog to SchemaVersion: safety snapshot
// first, then one transaction per pending migration step
// (DESIGN_SPEC 2.2). The caller holds the exclusive advisory lock.
func migrateUp(ctx context.Context, sqldb *sql.DB, abs string, from int) error {
	snapshot := preMigrateSnapshotPath(abs, from)
	if err := vacuumInto(ctx, sqldb, snapshot); err != nil {
		return &CatalogError{
			Kind: KindMigration,
			Path: abs,
			Err: fmt.Errorf(
				"pre-migration snapshot to %q failed; refusing to migrate without a snapshot: %w",
				snapshot, err),
		}
	}
	return applyMigrations(ctx, sqldb, from)
}

// foldedV1Objects are the objects foldedV1RepairDDL creates (see the
// const doc for why they need an existence check outside the migration
// list).
var foldedV1Objects = []string{
	"directories",
	"idx_directories_image",
	"idx_directories_unassigned",
}

// ensureFoldedV1Objects applies foldedV1RepairDDL when any folded
// object is missing from an already-current catalog. The check is a
// single sqlite_master query - a catalog produced by the CURRENT
// binary pays one indexed lookup per mutating open and nothing else.
// The repair, when needed, is one transaction (the same shape as a
// migration step) and runs under the exclusive advisory lock Open
// already holds, so no other writer can interleave. A repair failure
// is a KindMigration CatalogError (exit 4): the catalog's shape is
// wrong and cannot be brought up to date.
//
// All THREE objects are checked, not just the table: a catalog
// missing only one index (partial damage, or a future fold mistake)
// is healed by the same idempotent IF NOT EXISTS script.
func ensureFoldedV1Objects(ctx context.Context, sqldb *sql.DB, path string) error {
	rows, err := sqldb.QueryContext(ctx,
		`SELECT name FROM sqlite_master
		 WHERE name IN ('directories', 'idx_directories_image', 'idx_directories_unassigned')`)
	if err != nil {
		return &CatalogError{
			Kind: KindMigration,
			Path: path,
			Err:  fmt.Errorf("checking folded v1 objects (directories table): %w", err),
		}
	}
	present := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return &CatalogError{
				Kind: KindMigration,
				Path: path,
				Err:  fmt.Errorf("reading sqlite_master for folded v1 objects: %w", err),
			}
		}
		present[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return &CatalogError{
			Kind: KindMigration,
			Path: path,
			Err:  fmt.Errorf("iterating sqlite_master for folded v1 objects: %w", err),
		}
	}
	_ = rows.Close()
	for _, name := range foldedV1Objects {
		if !present[name] {
			return applyFoldedV1Repair(ctx, sqldb, path)
		}
	}
	return nil
}

// applyFoldedV1Repair executes foldedV1RepairDDL in one transaction:
// either the whole folded-object set is present afterwards, or nothing
// changed (atomic, like every schema step of 2.2).
func applyFoldedV1Repair(ctx context.Context, sqldb *sql.DB, path string) error {
	tx, err := sqldb.BeginTx(ctx, nil)
	if err != nil {
		return &CatalogError{
			Kind: KindMigration,
			Path: path,
			Err:  fmt.Errorf("beginning folded v1 object repair: %w", err),
		}
	}
	defer tx.Rollback() //nolint:errcheck // no-op after successful commit
	if _, err := tx.ExecContext(ctx, foldedV1RepairDDL); err != nil {
		return &CatalogError{
			Kind: KindMigration,
			Path: path,
			Err:  fmt.Errorf("repairing folded v1 objects (directories table): %w", err),
		}
	}
	if err := tx.Commit(); err != nil {
		return &CatalogError{
			Kind: KindMigration,
			Path: path,
			Err:  fmt.Errorf("committing folded v1 object repair: %w", err),
		}
	}
	return nil
}

// preMigrateSnapshotPath names the safety snapshot of DESIGN_SPEC 2.2:
// '<catalog>.pre-migrate-<version>.sqlite', where <version> is the
// pre-migration version being upgraded FROM.
func preMigrateSnapshotPath(catalogPath string, fromVersion int) string {
	return fmt.Sprintf("%s.pre-migrate-%d.sqlite", catalogPath, fromVersion)
}

// vacuumInto exports a consistent snapshot of the database to dest using
// VACUUM INTO (DESIGN_SPEC 2.2 and FR-034 mechanism). It works from a WAL
// catalog, is safe alongside concurrent readers, and SQLite refuses to
// overwrite an existing dest file. The full export.go surface lands in
// T3.2; this internal helper serves the pre-migration snapshot and tests.
func vacuumInto(ctx context.Context, sqldb *sql.DB, dest string) error {
	// VACUUM INTO takes the destination as a SQL string literal; escape
	// embedded single quotes by doubling them.
	stmt := "VACUUM INTO '" + strings.ReplaceAll(dest, "'", "''") + "'"
	if _, err := sqldb.ExecContext(ctx, stmt); err != nil {
		return err
	}
	return nil
}

// migration is one forward-only schema step (DESIGN_SPEC 2.2): the list is
// embedded in the binary, strictly ascending by version, and never edited
// once shipped; a new schema version appends the next step.
type migration struct {
	version int
	ddl     string
}

// migrations is the authoritative ordered migration list. Version 1 is the
// DESIGN_SPEC 2.3 DDL. A future v2 would append {version: 2, ddl: ...}
// here and nothing else changes.
var migrations = []migration{
	{version: 1, ddl: schemaV1DDL},
}

// applyMigrations applies every migration step above fromVersion, one
// transaction per step, stamping PRAGMA user_version inside each step's
// transaction so a crash rolls back both the DDL and the version stamp.
func applyMigrations(ctx context.Context, sqldb *sql.DB, fromVersion int) error {
	for _, m := range migrations {
		if m.version <= fromVersion {
			continue
		}
		if err := applyMigration(ctx, sqldb, m); err != nil {
			return &CatalogError{
				Kind: KindMigration,
				Err:  fmt.Errorf("migration to schema version %d: %w", m.version, err),
			}
		}
	}
	return nil
}

func applyMigration(ctx context.Context, sqldb *sql.DB, m migration) error {
	tx, err := sqldb.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after successful commit

	// The schema driver executes the whole DDL script in one call
	// (multi-statement Exec is supported); SQLite DDL is transactional.
	if _, err := tx.ExecContext(ctx, m.ddl); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return err
	}
	return tx.Commit()
}

// userVersion reads PRAGMA user_version (DESIGN_SPEC 2.2).
func userVersion(ctx context.Context, sqldb *sql.DB) (int, error) {
	var v int
	if err := sqldb.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}

// catalogDSN builds the connection string for the catalog policy of
// DESIGN_SPEC 2.1. Pragmas travel in the DSN so modernc.org/sqlite applies
// them on EVERY pooled connection, not only the first one opened:
//
//	journal_mode=WAL, busy_timeout=5000, foreign_keys=ON,
//	synchronous=NORMAL
//
// Read-only opens use the URI flag "file:...?mode=ro" and skip
// journal_mode (it is a persistent property of the file, already WAL from
// creation; forcing it from a read-only connection would error if the file
// were ever in another mode). The path is URL-encoded so spaces and other
// special characters survive the file: URI.
func catalogDSN(absPath string, mode OpenMode) string {
	q := url.Values{}
	if mode == OpenReadOnly {
		q.Set("mode", "ro")
	} else {
		q.Add("_pragma", "journal_mode(WAL)")
	}
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(NORMAL)")
	return (&url.URL{Scheme: "file", Path: absPath, RawQuery: q.Encode()}).String()
}

// classifyOpenErr maps a connection-level failure to KindOpen or
// KindCorrupt (DESIGN_SPEC 10.1 lists both under CatalogError).
func classifyOpenErr(path string, err error) error {
	kind := KindOpen
	msg := err.Error()
	if strings.Contains(msg, "not a database") ||
		strings.Contains(msg, "malformed") ||
		strings.Contains(msg, "disk image") {
		kind = KindCorrupt
	}
	return &CatalogError{Kind: kind, Path: path, Err: err}
}

// enrichMissingParentDir names the missing PARENT DIRECTORY of a
// catalog path in a KindOpen CatalogError (stakeholder UX defect D2 of
// 2026-10-19): the raw failures - SQLite's "unable to open database
// file (14)" or the flock sidecar's "opening advisory lock file ...:
// no such file or directory" - do not say WHICH directory is absent,
// and the tool deliberately never auto-creates parent directories (PM
// decision: the operator creates them). This is MESSAGE ENRICHMENT
// ONLY, applied at the failure sites of Create and Open:
//
//   - only *CatalogError values of Kind KINDOPEN are touched;
//     corrupt/busy/schema-too-new/migration failures and the
//     *CatalogBusy type pass through byte-identical (busy in
//     particular never reaches this helper - it is not a
//     *CatalogError - but the errors.As gate documents the intent);
//   - enrichment happens only when a stat of the parent directory
//     reports it NOT EXISTING; an existing parent (or a stat that
//     failed for any other reason, e.g. permissions) leaves the
//     original error untouched, so every other error kind keeps its
//     exact message;
//   - the original cause stays wrapped (%w), so errors.Is/As chains
//     over the underlying errno are unaffected, and the error STAYS a
//     KindOpen CatalogError - same class, same exit 4 (10.2). This is
//     not a class change, only a clearer message.
//
// abs must be the ABSOLUTE catalog path (both Create and Open compute
// it before any failure site).
func enrichMissingParentDir(abs string, err error) error {
	var cerr *CatalogError
	if !errors.As(err, &cerr) || cerr.Kind != KindOpen {
		return err
	}
	dir := filepath.Dir(abs)
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		return err
	}
	return &CatalogError{
		Kind: KindOpen,
		Path: cerr.Path,
		Err:  fmt.Errorf("catalog directory does not exist: %s (create it first): %w", dir, cerr.Err),
	}
}
