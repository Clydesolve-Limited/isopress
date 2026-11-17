package catalog

// T3.2: the FR-034/FR-035 catalog-export surface (DESIGN_SPEC 6.6 and
// 8.5; PLAN_SPEC FR-026, FR-034, FR-035). Per DESIGN_SPEC 1.2 the SQL
// of the export lives here; the builder (a later sprint) calls
// ExportSnapshot after planning completes and embeds the produced file
// in every image and copy set. The caller wires the file name - the
// builder joins its staging directory with media.ExportFileName
// ("catalog-export.sqlite"); this package deliberately does NOT import
// internal/media (1.2 layering), so the destination path is a plain
// parameter.
//
// THE (a)-VS-(b) DECISION (recorded here per the T3.2 brief)
//
// FR-034 requires an export "sufficient to restore the entire set"
// produced "through a consistent database snapshot mechanism (for
// example, SQLite VACUUM INTO ...), never a raw copy". DESIGN_SPEC 6.6
// names VACUUM INTO as that mechanism. A raw VACUUM INTO of the
// catalog file snapshots the WHOLE database: VACUUM INTO is a
// whole-file mechanism and cannot be filtered by SQL. Multi-set
// catalogs are real (the CLI binds one set per run via --set-id, and a
// long-lived catalog accumulates sets), so the honest options were:
//
//	(a) whole-catalog snapshot: one VACUUM INTO, trivially consistent,
//    but every disc of set A would carry set B's - and every other
//    set's - complete file list: every path, size, and content hash of
//    an unrelated backup, onto every Blu-ray of this set. It also
//    grows every disc's metadata by the whole catalog rather than the
//    set's slice. FR-034's "sufficient to restore the entire set" is a
//    lower bound, not an invitation to ship the other sets too.
//	(b) set-scoped export: construct the export as ONLY the requested
//    set's rows (see exportCopySteps for the row-selection rules and
//    the FK-closure argument), then finalize it with VACUUM INTO.
//
// DECISION: (b). A Blu-ray lives offline for years (FR-035's
// motivation) and should carry the minimum needed to restore ITS set;
// leaking another backup's file inventory onto it is a privacy defect
// and a size defect. Feasibility was verified against the 2.3 schema
// before committing to it: every foreign-key edge out of a copied row
// lands on a row of the SAME set (the only table without a set_id
// column, run_events, is closed through its run_id FK; the files
// lineage self-references modified_of/superseded_by are closed
// because both endpoints of a lineage edge belong to one set). The
// price versus (a) is one INSERT..SELECT pass per table instead of a
// single whole-file copy - O(catalog size), run once per set at build
// time, and the published bytes ARE VACUUM INTO output (6.6 letter
// satisfied), so the export is compact and integrity-valid by
// construction.
//
// CONSISTENCY AND PUBLISH GUARANTEES (FR-034)
//
// The export never writes to the source catalog and never leaves a
// half-written file at destPath:
//
//  1. Stage: a scratch database with a UNIQUE PER-CALL name
//     (destPath + ".staging-<pid>-<random>", see exportScratchSuffix)
//     is created with the exact current schema (applyMigrations from
//     the same schemaV1DDL, user_version stamped), the source is
//     ATTACHED read-only, and every table is copied in ONE transaction
//     with PRAGMA defer_foreign_keys. In WAL mode the transaction's
//     first read pins one snapshot of the source, so the copy is a
//     point-in-time consistent view even against a concurrent writer
//     (the single-writer model means the exporting run itself holds
//     the flock; the WAL snapshot is the consistency mechanism
//     FR-034 asks for). Deferred FK enforcement doubles as the
//     closure proof: if a copied row ever referenced a row the
//     selection missed, COMMIT would fail and nothing would publish.
//  2. Finalize: VACUUM INTO the per-call scratch
//     (destPath + ".partial-<pid>-<random>", the SAME suffix as the
//     stage path) FROM the staged database - FR-034's named mechanism
//     produces the actual bytes that ship.
//  3. Verify: the scratch is opened read-only (the same way
//     restore --from-media will open it, 8.5) and must pass PRAGMA
//     integrity_check ("ok") and PRAGMA foreign_key_check (zero rows).
//  4. Publish: only then an atomic rename moves it to destPath. Any
//     failure - including context cancellation - removes the call's
//     OWN scratch files (never any other path) and returns an error
//     with destPath never touched.
//
// CONCURRENT CALLS TO THE SAME destPath: every call derives scratch
// names unique to itself (exportScratchSuffix: the pid plus 64
// crypto/rand bits), so two concurrent calls can never share - and
// therefore never unlink, truncate, or re-create - each other's
// scratch files. This hardening follows a QA-measured corruption
// class in the earlier fixed-name design, where the loser of a
// same-destination race unlinked the winner's LIVE staging file from
// its error-path defer, and the winner's vacuum then re-created and
// published a fresh EMPTY database with err=nil (an empty database is
// integrity-valid and FK-closed, so the verify step passed). The worst
// residual interleaving is benign: the no-overwrite re-check and the
// final rename race (documented at the publish site below), and even
// then every published file is a complete, verified export of the
// set - never an empty one.
//
// CALLER CONTRACT
//
//   - destPath must NOT exist: like SQLite's own VACUUM INTO, the
//     export refuses to overwrite, so a stale file at the destination
//     is a loud error instead of silent replacement. The builder
//     passes a fresh path inside the staging directory it just
//     created.
//   - Call after planning completes (6.6): the export then contains
//     the full placement for every image of the set, including images
//     not yet built. Every image and copy set embeds the same file
//     (FR-035), so one export serves the whole set.
//   - The returned int64 is the published file's size in bytes; the
//     builder records it in the image_contents 'export' row and the
//     FR-033 manifest entry (both list sizes, 6.6).
//   - Scratch files live NEXT TO destPath (same directory, so the
//     final rename is atomic within one filesystem). Their names are
//     unique per call (see above), so leftover scratch from a crashed
//     earlier export can neither collide with nor be adopted by a new
//     export - and the export never removes it: it removes only its
//     OWN scratch. The entry-time discard of shared fixed-name
//     scratch is deliberately GONE - unlinking a fixed path is exactly
//     what could delete a concurrent call's live scratch. Scratch is
//     never a deliverable: the deliverable only ever appears via the
//     final rename.

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ExportSnapshot writes the FR-034 catalog export of set setID to
// destPath and returns the export's size in bytes. See the package
// section at the top of this file for the set-scoping decision, the
// consistency guarantees, and the caller contract.
//
// Errors are plain wrapped errors (statement and filesystem
// failures), matching the scanstore/planstore classification: the
// CatalogError kinds of 10.1 cover open/corrupt/schema/migration
// only, and the CLI layer classifies this failure at the command
// level. An unknown setID fails before any staging work.
func (db *DB) ExportSnapshot(ctx context.Context, setID, destPath string) (int64, error) {
	if setID == "" {
		return 0, errors.New("catalog: export: empty set id")
	}
	if destPath == "" {
		return 0, errors.New("catalog: export: empty destination path")
	}
	absDest, err := filepath.Abs(destPath)
	if err != nil {
		return 0, fmt.Errorf("catalog: export: resolving destination %s: %w", destPath, err)
	}

	// Caller contract: a fresh destination path, never an overwrite
	// (the same refusal SQLite's VACUUM INTO enforces natively).
	if _, err := os.Stat(absDest); err == nil {
		return 0, fmt.Errorf(
			"catalog: export: destination %s already exists; refusing to overwrite "+
				"(the caller passes a fresh path, like VACUUM INTO)", absDest)
	} else if !os.IsNotExist(err) {
		return 0, fmt.Errorf("catalog: export: checking destination %s: %w", absDest, err)
	}

	// UNIQUE PER-CALL SCRATCH (the hardening of the QA-measured
	// corruption class, see the guarantees above): the scratch paths
	// carry a per-call suffix - "<pid>-<16 hex digits of crypto/rand>"
	// - so NO two concurrent ExportSnapshot calls can ever share a
	// scratch path. With the earlier fixed names the loser of a
	// same-destination race unlinked the winner's LIVE staging file
	// from its error-path defer, and the winner's vacuum re-created
	// and published a fresh EMPTY database with err=nil.
	suffix, err := exportScratchSuffix()
	if err != nil {
		return 0, err
	}
	stage := absDest + exportStagingSuffix + "-" + suffix
	vac := absDest + exportPartialSuffix + "-" + suffix
	// NO entry-time stale-scratch discard: the scratch paths are
	// unique to this call and cannot pre-exist (the pid plus 64
	// crypto/rand bits), so there is nothing stale to discard -
	// and removing any OTHER path (a fixed name, a crashed earlier
	// export's leftover, a concurrent call's live scratch) is
	// precisely the unlink-what-you-don't-own hazard being fixed.
	// Foreign leftovers are simply ignored: scratch was never a
	// deliverable (publication is only the final rename).
	published := false
	defer func() {
		// Only THIS call's own scratch files are ever removed: the
		// names above are unique to this call, so the defer can never
		// unlink a concurrent export's files. The staging database is
		// scratch the moment the vacuum has consumed it - remove it
		// on every path, with its WAL/SHM sidecars should a pool close
		// have left any. The vacuum scratch is removed everywhere
		// EXCEPT success, where the rename moves it.
		_ = os.Remove(stage)
		_ = os.Remove(stage + "-wal")
		_ = os.Remove(stage + "-shm")
		if !published {
			_ = os.Remove(vac)
			_ = os.Remove(vac + "-wal")
			_ = os.Remove(vac + "-shm")
		}
	}()

	if err := db.exportStage(ctx, setID, stage); err != nil {
		return 0, err
	}
	if err := exportVacuum(ctx, stage, vac); err != nil {
		return 0, err
	}
	if err := verifyExportFile(ctx, vac); err != nil {
		return 0, err
	}
	// A cancelled context must not publish anything, even if every
	// statement above happened to succeed before cancellation landed.
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("catalog: export: aborted before publishing %s: %w", absDest, err)
	}

	// Re-check the no-overwrite contract immediately before the
	// publish. The single-writer model (D-11: the exporting run holds
	// the exclusive flock) excludes a competing creator of this path
	// in practice; the re-check closes the trivial window for anyone
	// else. A residual overlap remains between this Stat and the
	// rename below: with the per-call scratch names two concurrent
	// calls to the same destPath can both reach this point and both
	// rename (last writer wins) - benign, because both files are
	// complete, verified exports of the same set (the guarantees
	// above); it can NEVER produce the empty-publish class.
	if _, err := os.Stat(absDest); err == nil {
		return 0, fmt.Errorf(
			"catalog: export: destination %s appeared during export; refusing to overwrite", absDest)
	} else if !os.IsNotExist(err) {
		return 0, fmt.Errorf("catalog: export: checking destination %s: %w", absDest, err)
	}
	if err := os.Rename(vac, absDest); err != nil {
		return 0, fmt.Errorf("catalog: export: publishing %s: %w", absDest, err)
	}
	published = true

	fi, err := os.Stat(absDest)
	if err != nil {
		return 0, fmt.Errorf("catalog: export: stat published export %s: %w", absDest, err)
	}
	return fi.Size(), nil
}

// Scratch-name prefix constants of the export pipeline. Each call
// appends "-<pid>-<16 hex digits>" (exportScratchSuffix), so the full
// scratch paths are unique per call and can never collide with another
// call's scratch or with a crashed earlier export's leftover. They
// travel with the destination path (same directory -> same filesystem
// -> atomic final rename) and are documented in the ExportSnapshot
// contract above.
const (
	exportStagingSuffix = ".staging" // staged database: destPath+".staging-<suffix>"
	exportPartialSuffix = ".partial" // VACUUM INTO output: destPath+".partial-<suffix>"
)

// exportScratchSuffix renders the per-call scratch suffix
// "<pid>-<16 hex digits>": the pid scopes the name to this process and
// the 64 crypto/rand bits to this exact call, so two concurrent (or
// even cross-process) ExportSnapshot calls to the same destPath can
// never share a scratch path - the uniqueness invariant the publish
// guarantees rest on. crypto/rand rather than math/rand precisely
// because the invariant must not depend on process-global seeding
// state. A failure here is a plain wrapped error, like every other
// export failure.
func exportScratchSuffix() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("catalog: export: generating scratch suffix: %w", err)
	}
	return fmt.Sprintf("%d-%x", os.Getpid(), b), nil
}

// exportCopySteps is the table-by-table copy of the set-scoped export,
// parent-first by FK dependency. Each step copies EVERY row of the
// named table whose set_id is the exported set, except run_events,
// which carries no set_id column and is selected through its run_id
// foreign key. The statements run inside ONE transaction with
// PRAGMA defer_foreign_keys, so ordering matters only for readability;
// commit-time FK checking is the actual proof that the selection is
// closed (see the ExportSnapshot guarantees).
//
// Column matching is positional (INSERT ... SELECT *): both sides are
// the same schemaV1DDL executed by the same binary - the staging
// schema is built from that very constant via applyMigrations - so the
// column sets cannot diverge within one build.
//
// Row-selection rules, with the FK-closure argument per table:
//
//   - sets: the one exported row. Parent of runs/files/directories/
//     chunks/images (2.3).
//   - runs: every run of the set. Scans, plans, copies, builds,
//     restores, and clears of a set all run under its set_id, so every
//     first_seen_run/last_seen_run reference of the set's files and
//     directories rows lands inside this selection.
//   - files: EVERY row of the set - active, missing, and superseded
//     lineage rows alike. Copying the lineage rows whole is what keeps
//     the files.modified_of / files.superseded_by self-references
//     closed (both endpoints of a lineage edge belong to the same set:
//     the reconciler inserts both versions in one set-scoped ScanTx,
//     3.7). The alternative - dropping inactive rows - would break
//     FR-030 lineage edges and change the semantics of the
//     v_active_files view the restore engine reads. The export thus
//     carries the set's records verbatim, whatever their status, and
//     consumers filter exactly like they do on the live catalog.
//   - directories: every row of the set (active and missing), same
//     runs-closure argument as files (2.3).
//   - chunks: every row of the set - chunks.set_id is denormalized
//     from the parent file (2.3 note), and chunk.file_id points at the
//     set's files rows.
//   - images: every images row of the set (PRIMARY KEY set_id,
//     image_id): the placement targets, including images not yet
//     built (state 'planned' - the 6.6 "even for images not yet
//     built" clause).
//   - image_contents: every row of the set; file_id points at the
//     set's files rows. At export time these are the finalized
//     content lists written at build time (FR-020); a post-planning
//     export may legitimately have none, which is fine - the manifest
//     and the placement columns carry the set-level record (6.6).
//   - run_events: every event of the set's runs, selected through
//     run_id. run_events has no set_id column; run_id is its only FK
//     (file_id is a plain INTEGER correlated by relpath at scan time,
//     2.3), so the selection is closed the moment runs is.
var exportCopySteps = []struct{ table, where string }{
	{"sets", "set_id = ?"},
	{"runs", "set_id = ?"},
	{"files", "set_id = ?"},
	{"directories", "set_id = ?"},
	{"chunks", "set_id = ?"},
	{"images", "set_id = ?"},
	{"image_contents", "set_id = ?"},
	{"run_events", "run_id IN (SELECT run_id FROM src.runs WHERE set_id = ?)"},
}

// exportStage builds the set-scoped staging database at stage: the
// current schema (applyMigrations from schemaV1DDL, user_version
// stamped), the source catalog attached read-only, and one transaction
// copying the set's rows per exportCopySteps. The transaction's first
// read pins a single WAL snapshot of the source, so the whole copy is
// point-in-time consistent; deferred FK enforcement at COMMIT proves
// the selection closed. Any failure leaves only the scratch file,
// which ExportSnapshot's defer removes.
func (db *DB) exportStage(ctx context.Context, setID, stage string) error {
	sqldb, err := sql.Open(driverName, catalogDSN(stage, OpenReadWrite))
	if err != nil {
		return fmt.Errorf("catalog: export: opening staging database %s: %w", stage, err)
	}
	defer sqldb.Close() //nolint:errcheck // best-effort pool close; the export's own result dominates and the caller removes the scratch file either way
	if err := sqldb.Ping(); err != nil {
		return fmt.Errorf("catalog: export: staging database %s: %w", stage, err)
	}
	if err := applyMigrations(ctx, sqldb, 0); err != nil {
		return fmt.Errorf("catalog: export: creating staging schema in %s: %w", stage, err)
	}

	// ATTACH and the copy transaction must share ONE connection: ATTACH
	// is per-connection state, and a *sql.Conn pins it.
	conn, err := sqldb.Conn(ctx)
	if err != nil {
		return fmt.Errorf("catalog: export: acquiring staging connection: %w", err)
	}
	defer conn.Close() //nolint:errcheck // best-effort release of the ATTACH-pinned connection; the export result dominates

	if _, err := conn.ExecContext(ctx,
		fmt.Sprintf("ATTACH DATABASE %s AS src", sqlStringLiteral(attachReadOnlyURI(db.path)))); err != nil {
		return fmt.Errorf("catalog: export: attaching source catalog %s: %w", db.path, err)
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("catalog: export: beginning copy transaction: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after successful commit

	// Defer FK enforcement to COMMIT: the files lineage columns
	// (modified_of, superseded_by) are self-references whose insert
	// order within the copied set is not guaranteed by file_id, and a
	// commit-time check is what makes the closure argument in
	// exportCopySteps an enforced invariant rather than a comment.
	// The pragma resets itself at COMMIT/ROLLBACK (SQLite semantics).
	if _, err := tx.ExecContext(ctx, "PRAGMA defer_foreign_keys = ON"); err != nil {
		return fmt.Errorf("catalog: export: deferring foreign keys for the copy: %w", err)
	}

	// Set existence is checked INSIDE the snapshot transaction, so the
	// export either has this set's rows or fails - never an empty or
	// torn export of a mistyped id.
	var one int
	if err := tx.QueryRowContext(ctx,
		`SELECT 1 FROM src.sets WHERE set_id = ?`, setID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("catalog: export: set %q not found in catalog %s", setID, db.path)
		}
		return fmt.Errorf("catalog: export: reading set %q from %s: %w", setID, db.path, err)
	}

	for _, step := range exportCopySteps {
		q := fmt.Sprintf("INSERT INTO main.%s SELECT * FROM src.%s WHERE %s",
			step.table, step.table, step.where)
		if _, err := tx.ExecContext(ctx, q, setID); err != nil {
			return fmt.Errorf("catalog: export: copying %s rows of set %q: %w",
				step.table, setID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("catalog: export: committing staged rows of set %q: %w", setID, err)
	}
	return nil
}

// exportVacuum finalizes the staged database into the pre-publish
// scratch file with VACUUM INTO - FR-034's named mechanism, and the
// reason the published bytes are a compact, internally consistent
// snapshot by construction (6.6). The staged database is reopened on a
// fresh pool; closing that pool checkpoints and removes its WAL, so
// the staged scratch is a plain file again afterwards.
func exportVacuum(ctx context.Context, stage, vac string) error {
	sqldb, err := sql.Open(driverName, catalogDSN(stage, OpenReadWrite))
	if err != nil {
		return fmt.Errorf("catalog: export: reopening staging database %s: %w", stage, err)
	}
	defer sqldb.Close() //nolint:errcheck // best-effort pool close; VACUUM INTO's result dominates
	if err := sqldb.Ping(); err != nil {
		return fmt.Errorf("catalog: export: staging database %s: %w", stage, err)
	}
	if err := vacuumInto(ctx, sqldb, vac); err != nil {
		return fmt.Errorf("catalog: export: VACUUM INTO %s: %w", vac, err)
	}
	return nil
}

// verifyExportFile opens the finished pre-publish scratch read-only -
// exactly how restore --from-media will open it (DESIGN_SPEC 8.5) -
// and proves the two guarantees the media relies on: internal
// integrity (PRAGMA integrity_check must answer "ok") and foreign-key
// closure of the set-scoped copy (PRAGMA foreign_key_check must
// return no rows). A failure here aborts the export before the
// publish; nothing broken ever reaches destPath.
func verifyExportFile(ctx context.Context, path string) error {
	sqldb, err := sql.Open(driverName, catalogDSN(path, OpenReadOnly))
	if err != nil {
		return fmt.Errorf("catalog: export: opening export %s for verification: %w", path, err)
	}
	defer sqldb.Close() //nolint:errcheck // best-effort pool close; the verification verdict above dominates

	var verdict string
	if err := sqldb.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&verdict); err != nil {
		return fmt.Errorf("catalog: export: integrity_check of %s: %w", path, err)
	}
	if verdict != "ok" {
		return fmt.Errorf("catalog: export: integrity_check of %s reported %q, want ok", path, verdict)
	}

	rows, err := sqldb.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("catalog: export: foreign_key_check of %s: %w", path, err)
	}
	defer rows.Close() //nolint:errcheck // read-only close; rows.Err dominates
	violations := 0
	var first string
	for rows.Next() {
		var table, parent sql.NullString
		var rowid, fkid sql.NullInt64
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return fmt.Errorf("catalog: export: reading foreign_key_check of %s: %w", path, err)
		}
		if violations == 0 {
			first = fmt.Sprintf("table %s rowid %v references %s", table.String, rowid.Int64, parent.String)
		}
		violations++
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("catalog: export: iterating foreign_key_check of %s: %w", path, err)
	}
	if violations > 0 {
		return fmt.Errorf(
			"catalog: export: %s failed foreign_key_check (%d violations, first: %s)",
			path, violations, first)
	}
	return nil
}

// attachReadOnlyURI renders a catalog path as a read-only file: URI
// for the export's ATTACH statement. It is deliberately minimal
// (mode=ro only): the DSN pragma extensions of catalogDSN are a
// database/sql driver convention for MAIN database connections and are
// not valid SQLite ATTACH URI parameters. The read-only mode is what
// makes the "the export never writes the source" guarantee structural.
func attachReadOnlyURI(absPath string) string {
	return (&url.URL{Scheme: "file", Path: absPath, RawQuery: "mode=ro"}).String()
}

// sqlStringLiteral quotes s as a SQL string literal for statement
// building (vacuumInto applies the same doubling inline).
func sqlStringLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
