package catalog

// T2.2 (phase 1): the directories-table catalog surface (DESIGN_SPEC
// 1.2 layering - the catalog package is the ONLY component issuing SQL;
// 2.3 directories table used verbatim - no column here invents anything
// the DDL lacks; 3.7 rescan reconciliation extended to dirs per the
// 2026-10-19 architect ruling; 4.3 planner assignment of empty dirs to
// the lowest-numbered destination, persisted to directories.image_id).
//
// This file supplies everything the T2.2 empty-dir slice needs over the
// directories table, and nothing else:
//
//   - scan side (ScanTx methods, joining the reconciliation
//     transaction): UpsertEmptyDir (INSERT ... ON CONFLICT(set_id,
//     relpath) DO UPDATE - dirs are not lineage-versioned, a rescan
//     upserts, per the 2.3 UNIQUE note), ListActiveDirs (the read side
//     of the dir not-seen pass), MarkDirsMissing (status write only,
//     the FR-032 "mark, do not delete" philosophy);
//   - plan side: ListActiveUnassignedDirs on *DB (the planner's
//     metadata-only item source - the 4.3 clarification: empty-dir
//     state is read from the TABLE, never from an in-memory walk
//     result), AssignDir on PlanTx (joins the single planning
//     transaction of DESIGN_SPEC 4.5, guarded like every other
//     assignment by the FR-011 lock: only a NULL image_id is ever
//     written).
//
// As with scanstore.go and planstore.go, the ingest and plan services
// never import this package: they declare their own shapes (ingest.DirRow,
// plan.UnassignedDir) and the cli wiring layer maps between the
// structurally identical shapes (DESIGN_SPEC 1.2). Error classification
// mirrors the established pattern: statement failures inside an open
// transaction are plain wrapped errors.

import (
	"context"
	"fmt"
)

// DirRow is one currently-empty directory the scan pipeline wants
// stored in the directories table (DESIGN_SPEC 2.3, columns filled by
// the ingest path; FR-037).
type DirRow struct {
	RelPath string // slash-separated, relative to the scan root
	Mode    uint32 // raw POSIX permission bits (directories.mode)
	UID     uint32
	GID     uint32
	MtimeNS int64 // unix nanoseconds, UTC
}

// ActiveDir is one catalog-active directories row (status='active')
// read back by the dir not-seen pass (3.7 extended to dirs per the
// 2026-10-19 ruling).
type ActiveDir struct {
	DirID   int64
	RelPath string
}

// UnassignedDir is one ACTIVE, UNASSIGNED directories row the planner
// turns into a size-0 placement item (DESIGN_SPEC 4.3). ImageID is
// deliberately absent: the read only ever returns rows with
// image_id IS NULL, and the write side (AssignDir) upholds the
// FR-011 NULL-guard.
type UnassignedDir struct {
	DirID   int64
	RelPath string
}

// dirColumns is the shared directories projection for active reads.
const dirColumns = `dir_id, relpath`

// UpsertEmptyDir stores one currently-empty directory for the
// transaction's set and run. The SQL implements the 2.3 UNIQUE-note
// semantics exactly:
//
//   - a dir path not yet in the table: inserted with first_seen_run and
//     last_seen_run = this run, status 'active', image_id NULL (scan
//     NEVER assigns - FR-029 holds for dirs too);
//   - a dir path already in the table (rescan): the metadata columns
//     and last_seen_run are refreshed and status is re-activated
//     ('active' - covers both the still-empty case and the dir that
//     was marked missing and became empty again), while
//     first_seen_run and, critically, image_id are PRESERVED: an
//     assignment is locked once set (FR-011), and re-activating a
//     previously assigned dir must not un-assign or re-target it.
func (tx *ScanTx) UpsertEmptyDir(ctx context.Context, d DirRow) error {
	_, err := tx.conn.ExecContext(ctx,
		`INSERT INTO directories (set_id, relpath, mode, uid, gid, mtime_ns,
		                          first_seen_run, last_seen_run, status, image_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'active', NULL)
		 ON CONFLICT(set_id, relpath) DO UPDATE SET
		   mode         = excluded.mode,
		   uid          = excluded.uid,
		   gid          = excluded.gid,
		   mtime_ns     = excluded.mtime_ns,
		   last_seen_run = excluded.last_seen_run,
		   status       = 'active'`,
		tx.setID, d.RelPath, d.Mode, d.UID, d.GID, d.MtimeNS, tx.runID, tx.runID)
	if err != nil {
		return fmt.Errorf("catalog: upserting empty dir %q: %w", d.RelPath, err)
	}
	return nil
}

// ListActiveDirs reads EVERY catalog-active directories row of the set
// - the read side of the dir not-seen pass ("existing directories rows
// not in the current empty set become missing", 2026-10-19 ruling: the
// marking covers dirs deleted from the source AND dirs that gained
// files - either way they need no explicit representation).
func (tx *ScanTx) ListActiveDirs(ctx context.Context) ([]ActiveDir, error) {
	return listActiveDirs(ctx, tx.conn, tx.setID)
}

// MarkDirsMissing stamps status='missing' on the given dir ids (the dir
// not-seen outcome, FR-032 "mark, do not delete"). ONLY the status
// column is written: the row keeps its metadata and, where set, its
// image_id - an assignment locked before the dir went missing stays
// described accurately (2.4 retention philosophy). Returns the number
// of rows changed. The caller (reconciler) selects the dir_ids as
// catalog-active rows whose relpath was not in the scan's empty set;
// this method never selects on its own, so the safety gate lives in
// ONE place (mirrors ScanTx.MarkMissing).
func (tx *ScanTx) MarkDirsMissing(ctx context.Context, dirIDs []int64) (int, error) {
	total := 0
	for _, chunk := range chunkInt64s(dirIDs, activeInChunk) {
		q := fmt.Sprintf(
			`UPDATE directories SET status = 'missing' WHERE set_id = ? AND dir_id IN (%s)`,
			placeholders(len(chunk)))
		res, err := tx.conn.ExecContext(ctx, q, append([]any{tx.setID}, argsOf64(chunk)...)...)
		if err != nil {
			return total, fmt.Errorf("catalog: marking %d directories missing: %w", len(chunk), err)
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}
	return total, nil
}

// ActiveDirs reads EVERY catalog-ACTIVE directories row of the set
// (status='active'), ordered by dir_id, on *DB - usable in NO
// transaction and under NO advisory lock (the ListActiveForPlan /
// ActiveRecords read-side shape).
//
// T5.3 (restore selection): DESIGN_SPEC 8.2 selects "the latest state
// of every active record", and an empty directory IS a durable record
// (2.3 directories, FR-037) that restore must recreate - so the
// selection needs the ACTIVE dir universe exactly as much as the files
// one, INCLUDING rows no plan has assigned yet: an unassigned active
// dir must surface as a selected-but-unclaimed path in the restore
// report instead of silently disappearing from the run (the
// no-silent-data-miss discipline ActiveRecords gives the files side).
//
// WHY THIS READ EXISTS rather than a placement-derived dir set (checked
// per T5.3's "minimal read" rule): catalog.CopyPlacement lists only
// ASSIGNED dirs, so deriving the selection universe from it would hide
// an unassigned active dir (a dir scanned after the plan) from both the
// selection and the unclaimed report. The existing readers do not fit
// either - ListActiveUnassignedDirs is deliberately UNASSIGNED-only
// (the planner's item source) and ScanTx.ListActiveDirs is
// transaction-scoped (the mutating scan path). SQL stays here
// (DESIGN_SPEC 1.2); internal/restore reads through its own Store seam
// and the cli adapts the row shape, as every service does.
func (db *DB) ActiveDirs(ctx context.Context, setID string) ([]ActiveDir, error) {
	return listActiveDirs(ctx, db.sql, setID)
}

// ListActiveUnassignedDirs reads every ACTIVE, UNASSIGNED directories
// row of the set (status='active' AND image_id IS NULL), ordered by
// dir_id. This is the planner's ONLY source of empty-dir items
// (DESIGN_SPEC 4.3 clarification 2026-10-19): planning runs as a
// separate process, days after the scan, with no WalkResult in memory -
// the durable table is what makes the empty-dir skeleton survive. It
// is available on *DB - not only on PlanTx - so the read-only `report`
// pipeline (DESIGN_SPEC 4.3) can compute the identical plan without a
// transaction.
func (db *DB) ListActiveUnassignedDirs(ctx context.Context, setID string) ([]UnassignedDir, error) {
	q := fmt.Sprintf(`SELECT %s FROM directories WHERE set_id = ? AND status = 'active' AND image_id IS NULL ORDER BY dir_id`,
		dirColumns)
	rows, err := db.sql.QueryContext(ctx, q, setID)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing active unassigned directories for set %q: %w", setID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only close; rows.Err dominates
	var out []UnassignedDir
	for rows.Next() {
		var d UnassignedDir
		if err := rows.Scan(&d.DirID, &d.RelPath); err != nil {
			return nil, fmt.Errorf("catalog: scanning unassigned directory row: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: iterating unassigned directory rows: %w", err)
	}
	return out, nil
}

// AssignDir persists one empty-dir assignment inside the planning
// transaction (DESIGN_SPEC 4.3: "the assignment is persisted to
// directories.image_id, so a later plan/copy/build run ... can still
// enumerate and write them"). The FR-011 lock guard is mechanical: the
// UPDATE only matches image_id IS NULL, so an already-assigned dir can
// never be re-targeted; a zero-row update is an error naming the cause
// (already assigned, missing row, or wrong set).
func (tx *PlanTx) AssignDir(ctx context.Context, dirID, imageID int64) error {
	if imageID <= 0 {
		return fmt.Errorf("catalog: assigning dir %d to image %d: image id must be positive", dirID, imageID)
	}
	res, err := tx.conn.ExecContext(ctx,
		`UPDATE directories SET image_id = ? WHERE dir_id = ? AND set_id = ? AND image_id IS NULL`,
		imageID, dirID, tx.setID)
	if err != nil {
		return fmt.Errorf("catalog: assigning directory %d to destination %d: %w", dirID, imageID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf(
			"catalog: assigning directory %d to destination %d: no active unassigned row in set %q "+
				"(already assigned rows are locked, FR-011)",
			dirID, imageID, tx.setID)
	}
	return nil
}

// listActiveDirs is the shared active-dir read used by ScanTx.
func listActiveDirs(ctx context.Context, q queryer, setID string) ([]ActiveDir, error) {
	rows, err := q.QueryContext(ctx,
		fmt.Sprintf(`SELECT %s FROM directories WHERE set_id = ? AND status = 'active' ORDER BY dir_id`,
			dirColumns),
		setID)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing active directories for set %q: %w", setID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only close; rows.Err dominates
	var out []ActiveDir
	for rows.Next() {
		var d ActiveDir
		if err := rows.Scan(&d.DirID, &d.RelPath); err != nil {
			return nil, fmt.Errorf("catalog: scanning active directory row: %w", err)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: iterating active directory rows: %w", err)
	}
	return out, nil
}
