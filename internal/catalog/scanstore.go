package catalog

// T1.6: the scan-side catalog surface (DESIGN_SPEC 1.2 layering - the
// catalog package is the ONLY component issuing SQL; 3.7 rescan and
// reconciliation; 2.4 lineage and runs bookkeeping; 2.3 schema v1,
// used verbatim - no column here invents anything the DDL lacks).
//
// This file supplies everything the T1.6 reconciler (internal/ingest)
// needs from the database, and nothing else:
//
//   - set binding (EnsureSet): the sets row a files/runs row hangs off
//     (foreign_keys=ON makes it mandatory before any insert);
//   - runs bookkeeping (PriorScanRuns, StartScanRun, FinishRun): the
//     'ingest'/'rescan' run row opened BEFORE the scan and finalized
//     'ok'/'partial'/'failed' afterwards (2.4 justification: last_seen_run
//     provenance is what lets the reconciler classify deletions);
//   - run_events rows (InsertRunEvent): walker warnings and T1.5
//     failure entries, per 3.4 and 10.1;
//   - ScanTx: ONE BEGIN IMMEDIATE transaction over a dedicated
//     connection, carrying the reconciler's read side (active-record
//     comparison) and write side (insert / last_seen touch / versioned
//     modification / missing marking) so each reconciliation batch
//     commits atomically (3.7 "reconciles in one transaction"; with the
//     3.3 writer batching, "one transaction" is one batch - see
//     BeginScanTx).
//
// The ingest package never imports this one: it declares its own
// Store interface plus record types (ingest.Store, ingest.FileRow,
// ingest.ActiveRecord) and the cli wiring layer maps between the two
// structurally identical shapes (DESIGN_SPEC 1.2: "services depend on
// interfaces for catalog access"). That keeps SQL - and the driver -
// contained in this package while the reconciler stays unit-testable
// against a fake store.
//
// Error classification: failures to even get a connection
// (BeginScanTx) surface as *CatalogError (exit 4, the "cannot open"
// class). Failures of individual statements inside an open ScanTx are
// returned as plain wrapped errors: DESIGN_SPEC 10.1 scopes
// CatalogError to open/corrupt/schema-too-new/migration, so a scan-time
// write failure honestly lands in the fatal-unexpected class (exit 1)
// and the caller rolls the transaction back.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ToolVersion is the version string stamped into sets.tool_version and
// runs.tool_version (DESIGN_SPEC 2.3). It records which binary produced
// each row for provenance; it is ALSO the single version source for the
// CLI's --version flag (cobra root Version, internal/cli/root.go), so
// the media stamp and the flag can never disagree. "0.1.0" is the
// first release number (stakeholder decision 2026-11-09); the next
// development cycle bumps back to a -dev suffix per RELEASING.md.
const ToolVersion = "0.1.0"

// ErrSetRootMismatch is wrapped by EnsureSet when a set id already
// exists in the catalog but is bound to a DIFFERENT source root. The
// cli maps it to a usage-class error (exit 2): the user's --set-id
// (or derived id) disagrees with the catalog, which is an input
// mistake, not a corrupt catalog.
var ErrSetRootMismatch = errors.New("catalog: set is bound to a different source root")

// FileRow is one content record the scan pipeline wants stored in the
// files table (DESIGN_SPEC 2.3, columns filled by the ingest path).
//
// Zero-value conventions: LinkTarget "" and StatusDetail "" are stored
// as NULL; Hash nil is stored as NULL (symlinks never carry a hash and,
// per the T1.5 stale-digest drop, unreadable/changed_during_scan rows
// store NO hash - the reconciler enforces that before this layer sees
// the row, and the insert below stores exactly what it is given).
type FileRow struct {
	RelPath      string
	EntryKind    string // 'regular' | 'symlink' (files.entry_kind CHECK)
	Size         int64  // bytes; for symlinks len(target)
	Mode         uint32 // raw POSIX permission bits (files.mode)
	UID          uint32
	GID          uint32
	MtimeNS      int64 // unix nanoseconds, UTC
	Dev          int64
	Inode        int64
	Nlink        int64
	LinkTarget   string // symlink target (D-03); "" -> NULL
	Hash         []byte // raw sha256, 32 bytes; nil -> NULL (D-09)
	Status       string // 'active' | 'unreadable' | 'changed_during_scan'
	StatusDetail string // errno / change detail; "" -> NULL
}

// ActiveFile is one catalog-active file row (status='active',
// superseded_by IS NULL) read back for reconciliation comparison.
// ImageID/Copied are carried so tests - and later sprints - can assert
// that modification versioning PRESERVES assignment state on the old
// row (2.4: "Old rows keep their image_id and copied state").
type ActiveFile struct {
	FileID     int64
	RelPath    string
	EntryKind  string
	Hash       []byte // NULL scans as nil
	LinkTarget string // NULL scans as ""
	Status     string
	Size       int64
	MtimeNS    int64
	ImageID    sql.NullInt64 // assignment; NULL = unassigned
	Copied     bool
}

// ScanRun describes the runs row opened before a scan begins
// (DESIGN_SPEC 2.3 runs table, 2.4 justification).
type ScanRun struct {
	SetID       string
	Kind        string // 'ingest' (first scan of a set) | 'rescan'
	Root        string // absolute source root
	StartedAt   int64  // unix nanoseconds, UTC
	ExcludeJSON string // JSON array snapshot of the pattern set (D-08)
}

// RunEvent is one run_events row (DESIGN_SPEC 2.3): the operational
// log feeding the end-of-run report and cron observability.
type RunEvent struct {
	RunID   int64
	At      int64 // unix nanoseconds, UTC
	Level   string
	RelPath string // "" -> NULL
	Detail  string
}

// EnsureSet binds setID to sourceRoot in the sets table, creating the
// row if absent (INSERT OR IGNORE, then verify). Re-scanning the same
// set through the same root is a no-op; a set id that already exists
// with a DIFFERENT source root is refused with ErrSetRootMismatch
// (the cli turns that into a usage error): silently re-binding a set
// would corrupt the meaning of every files row already stored under it
// (relpaths are interpreted relative to the bound root).
func (db *DB) EnsureSet(ctx context.Context, setID, sourceRoot string, createdAt int64, toolVersion string) error {
	var root string
	err := db.sql.QueryRowContext(ctx,
		`SELECT source_root FROM sets WHERE set_id = ?`, setID).Scan(&root)
	switch {
	case err == nil:
		if root != sourceRoot {
			return fmt.Errorf("catalog: set %q is bound to source root %q, not %q: %w",
				setID, root, sourceRoot, ErrSetRootMismatch)
		}
		return nil
	case errors.Is(err, sql.ErrNoRows):
		_, ierr := db.sql.ExecContext(ctx,
			`INSERT INTO sets (set_id, source_root, created_at, tool_version) VALUES (?, ?, ?, ?)`,
			setID, sourceRoot, createdAt, toolVersion)
		if ierr != nil {
			return fmt.Errorf("catalog: creating set %q: %w", setID, ierr)
		}
		return nil
	default:
		return fmt.Errorf("catalog: reading set %q: %w", setID, err)
	}
}

// PriorScanRuns counts the ingest/rescan runs already recorded for the
// set. Zero means this run is the set's FIRST scan (kind 'ingest');
// anything above zero makes it a 'rescan' (DESIGN_SPEC 2.3 runs.kind,
// 3.7). The count includes prior run rows of ANY status - 'ok',
// 'partial', 'failed', and a crashed 'running' row that never got its
// FinishRun - because any prior scan INTENT for the set classifies the
// next scan as 'rescan': a second 'ingest' would misrepresent the audit
// trail (2.4 provenance), and the crashed-'running'-row case is pinned
// by cli.TestScanAfterCrashedPriorRunRowIsRescan. Plan/copy/build/...
// runs are deliberately not counted at all: they are not scans of the
// set.
func (db *DB) PriorScanRuns(ctx context.Context, setID string) (int, error) {
	var n int
	if err := db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM runs WHERE set_id = ? AND kind IN ('ingest','rescan')`, setID).
		Scan(&n); err != nil {
		return 0, fmt.Errorf("catalog: counting prior scan runs for set %q: %w", setID, err)
	}
	return n, nil
}

// StartScanRun inserts the runs row in status 'running' and returns its
// run_id. The run id is the first_seen_run/last_seen_run provenance key
// every files row inserted under this run carries (2.4: the runs table
// is what lets the reconciler classify deletions per FR-032).
func (db *DB) StartScanRun(ctx context.Context, run ScanRun) (int64, error) {
	res, err := db.sql.ExecContext(ctx,
		`INSERT INTO runs (set_id, kind, root, started_at, finished_at, status,
		                  exclude_json, stats_json, tool_version)
		 VALUES (?, ?, ?, ?, NULL, 'running', ?, '{}', ?)`,
		run.SetID, run.Kind, run.Root, run.StartedAt, run.ExcludeJSON, ToolVersion)
	if err != nil {
		return 0, fmt.Errorf("catalog: starting scan run for set %q: %w", run.SetID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("catalog: run id after insert: %w", err)
	}
	return id, nil
}

// FinishRun stamps the terminal status ('ok', 'partial', 'failed',
// 'interrupted' - runs.status CHECK), the finish timestamp, and the
// stats snapshot on the run row. An unknown status is rejected before
// hitting the CHECK constraint so the error names the mistake.
func (db *DB) FinishRun(ctx context.Context, runID int64, status string, finishedAt int64, statsJSON string) error {
	switch status {
	case "ok", "partial", "failed", "interrupted":
	default:
		return fmt.Errorf("catalog: FinishRun: invalid run status %q", status)
	}
	if statsJSON == "" {
		statsJSON = "{}"
	}
	res, err := db.sql.ExecContext(ctx,
		`UPDATE runs SET status = ?, finished_at = ?, stats_json = ? WHERE run_id = ?`,
		status, finishedAt, statsJSON, runID)
	if err != nil {
		return fmt.Errorf("catalog: finishing run %d: %w", runID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("catalog: finishing run %d: no such run_id", runID)
	}
	return nil
}

// InsertRunEvent appends one run_events row (warn rows for walker
// warnings and T1.5 failure entries; see DESIGN_SPEC 3.4 and the T1.5
// ownership split). file_id stays NULL: at scan time the event is
// correlated by relpath, and a failure row may not exist yet.
func (db *DB) InsertRunEvent(ctx context.Context, ev RunEvent) error {
	switch ev.Level {
	case "info", "warn", "error":
	default:
		return fmt.Errorf("catalog: InsertRunEvent: invalid level %q", ev.Level)
	}
	rel := sql.NullString{String: ev.RelPath, Valid: ev.RelPath != ""}
	if _, err := db.sql.ExecContext(ctx,
		`INSERT INTO run_events (run_id, at, level, file_id, relpath, detail)
		 VALUES (?, ?, ?, NULL, ?, ?)`,
		ev.RunID, ev.At, ev.Level, rel, ev.Detail); err != nil {
		return fmt.Errorf("catalog: run event for run %d: %w", ev.RunID, err)
	}
	return nil
}

// ScanTx is ONE reconciliation transaction over a dedicated pooled
// connection, started with BEGIN IMMEDIATE (DESIGN_SPEC 3.7 "reconciles
// in one transaction"; 4.5 uses the same BEGIN IMMEDIATE idiom for
// plan assignment).
//
// Why BEGIN IMMEDIATE: a deferred SQLite transaction upgrades its
// read lock to a write lock at first write, which can fail with
// SQLITE_BUSY after the read snapshot is taken. BEGIN IMMEDIATE takes
// the write reservation up front, so under the 2.1 connection policy
// (busy_timeout=5000 as second line of defense; the advisory flock as
// the primary single-writer mechanism) a batch either commits whole or
// fails whole - never a mid-batch upgrade failure.
//
// Why a dedicated *sql.Conn rather than *sql.Tx: database/sql's Tx
// begins with a plain driver BEGIN, and there is no portable way to
// request BEGIN IMMEDIATE through TxOptions with every driver. Holding
// one *sql.Conn and driving BEGIN IMMEDIATE / COMMIT / ROLLBACK as
// plain statements on it is the driver-independent form; the
// connection is returned to the pool by Commit or Rollback, and every
// statement of the transaction MUST go through this connection so it
// stays inside the transaction.
//
// Lifecycle: exactly one of Commit or Rollback must be called; after
// either, the connection is closed and further use is a driver error
// (callers guard with the committed flag pattern - see the reconciler).
type ScanTx struct {
	conn  *sql.Conn
	runID int64
	setID string
	now   func() time.Time // clock seam for first_seen_at (T1.1 fake-clock pattern)
	done  bool
}

// BeginScanTx opens the reconciliation transaction on a dedicated
// connection (see ScanTx for the BEGIN IMMEDIATE rationale). runID is
// the runs row started for this scan; setID scopes every read and
// write. Connection-acquisition failure is a KindOpen CatalogError
// (exit 4); the BEGIN IMMEDIATE failure itself is a plain wrapped error
// (see the file doc's classification note).
func (db *DB) BeginScanTx(ctx context.Context, runID int64, setID string) (*ScanTx, error) {
	conn, err := db.sql.Conn(ctx)
	if err != nil {
		return nil, &CatalogError{
			Kind: KindOpen,
			Path: db.path,
			Err:  fmt.Errorf("acquiring connection for scan transaction: %w", err),
		}
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("catalog: BEGIN IMMEDIATE for scan run %d: %w", runID, err)
	}
	return &ScanTx{conn: conn, runID: runID, setID: setID, now: time.Now}, nil
}

// RunID returns the run this transaction writes provenance under.
func (tx *ScanTx) RunID() int64 { return tx.runID }

// SetID returns the set this transaction is scoped to.
func (tx *ScanTx) SetID() string { return tx.setID }

// activeColumns is the shared projection for the read side.
const activeColumns = `file_id, relpath, entry_kind, hash, link_target, status, size, mtime_ns, image_id, copied`

// activeWhere narrows to the catalog-active view of DESIGN_SPEC 2.3
// (status='active' AND superseded_by IS NULL - exactly the v_active_files
// view's predicate, spelled out so the IN-clause variants can extend it).
const activeWhere = `status = 'active' AND superseded_by IS NULL`

// ActiveByRelPaths reads the catalog-active rows for the given relpaths
// of this transaction's set (the 3.7 comparison read: "existing path,
// content unchanged (hash matches latest active row)"). The result is
// keyed by relpath; a relpath with no active row is simply absent. The
// IN-clause is chunked so arbitrarily large batches stay under any
// variable-count limit.
func (tx *ScanTx) ActiveByRelPaths(ctx context.Context, relpaths []string) (map[string]ActiveFile, error) {
	out := make(map[string]ActiveFile, len(relpaths))
	for _, chunk := range chunkStrings(relpaths, activeInChunk) {
		q := fmt.Sprintf(
			`SELECT %s FROM files WHERE set_id = ? AND %s AND relpath IN (%s)`,
			activeColumns, activeWhere, placeholders(len(chunk)))
		rows, err := tx.conn.QueryContext(ctx, q, append([]any{tx.setID}, argsOf(chunk)...)...)
		if err != nil {
			return nil, fmt.Errorf("catalog: active rows for scan batch: %w", err)
		}
		afs, err := scanActiveRows(rows)
		if err != nil {
			return nil, err
		}
		for _, af := range afs {
			out[af.RelPath] = af
		}
	}
	return out, nil
}

// ListActive reads EVERY catalog-active row of the set - the read side
// of the 3.7 not-seen pass ("catalog-active path not seen in this
// scan: status='missing'").
func (tx *ScanTx) ListActive(ctx context.Context) ([]ActiveFile, error) {
	q := fmt.Sprintf(`SELECT %s FROM files WHERE set_id = ? AND %s ORDER BY file_id`,
		activeColumns, activeWhere)
	rows, err := tx.conn.QueryContext(ctx, q, tx.setID)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing active rows for set %q: %w", tx.setID, err)
	}
	return scanActiveRows(rows)
}

// scanActiveRows drains and closes a query over the activeColumns
// projection.
func scanActiveRows(rows *sql.Rows) ([]ActiveFile, error) {
	defer rows.Close() //nolint:errcheck // read-only close; rows.Err dominates
	var out []ActiveFile
	for rows.Next() {
		var (
			af         ActiveFile
			hash       []byte
			linkTarget sql.NullString
			imageID    sql.NullInt64
			copied     int64
			entryKind  string
			status     string
			size       int64
			mtimeNS    int64
		)
		if err := rows.Scan(&af.FileID, &af.RelPath, &entryKind, &hash, &linkTarget,
			&status, &size, &mtimeNS, &imageID, &copied); err != nil {
			return nil, fmt.Errorf("catalog: scanning active row: %w", err)
		}
		af.EntryKind = entryKind
		af.Hash = hash
		af.LinkTarget = linkTarget.String
		af.Status = status
		af.Size = size
		af.MtimeNS = mtimeNS
		af.ImageID = imageID
		af.Copied = copied != 0
		out = append(out, af)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: iterating active rows: %w", err)
	}
	return out, nil
}

// insertColumns is the shared files INSERT projection: every
// DESIGN_SPEC 2.3 column the ingest path fills, plus the columns the
// insert holds at their defaults (link_group NULL until hard-link
// family grouping lands with the copy/plan work; superseded_by NULL -
// a fresh row is never superseded; chunked/chunk_count/chunk_size and
// image_id/copied stay at their schema defaults because scan NEVER
// assigns: FR-029 guarantees new rows land unassigned).
const insertColumns = `set_id, relpath, entry_kind, size, mode, uid, gid, mtime_ns, dev, inode, nlink,
                       link_group, link_target, hash, first_seen_run, last_seen_run, first_seen_at,
                       status, status_detail, modified_of, superseded_by, chunked, chunk_count,
                       chunk_size, image_id, copied`

// insertValues is the matching placeholder tuple (26 columns).
const insertValues = `?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
                      NULL, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, 0, 0, NULL, NULL, 0`

// InsertScanFile stores one new files row for the transaction's set and
// run: first_seen_run and last_seen_run are this run, first_seen_at is
// the transaction clock, the row starts status-supplied and
// unassigned (image_id NULL, copied 0). It returns the new file_id.
//
// The row is stored EXACTLY as the reconciler shaped it: a NULL hash
// stays NULL (symlinks and, per the T1.5 stale-digest drop, the
// unreadable/changed_during_scan classes - this layer never invents a
// digest).
func (tx *ScanTx) InsertScanFile(ctx context.Context, f FileRow) (int64, error) {
	linkTarget := sql.NullString{String: f.LinkTarget, Valid: f.LinkTarget != ""}
	hash := f.Hash
	if hash != nil && len(hash) == 0 {
		hash = nil
	}
	detail := sql.NullString{String: f.StatusDetail, Valid: f.StatusDetail != ""}
	res, err := tx.conn.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO files (%s) VALUES (%s)`, insertColumns, insertValues),
		tx.setID, f.RelPath, f.EntryKind, f.Size, f.Mode, f.UID, f.GID, f.MtimeNS,
		f.Dev, f.Inode, f.Nlink,
		linkTarget, hash, tx.runID, tx.runID, tx.now().UnixNano(),
		f.Status, detail)
	if err != nil {
		return 0, fmt.Errorf("catalog: inserting scan file %q: %w", f.RelPath, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("catalog: file id after inserting %q: %w", f.RelPath, err)
	}
	return id, nil
}

// UpdateSeen touches last_seen_run only - the 3.7 UNCHANGED outcome
// ("update last_seen_run only"): no content column is written, so a
// matched row's hash, status, and assignment state are bit-identical
// before and after.
func (tx *ScanTx) UpdateSeen(ctx context.Context, fileID int64) error {
	if _, err := tx.conn.ExecContext(ctx,
		`UPDATE files SET last_seen_run = ? WHERE file_id = ? AND set_id = ?`,
		tx.runID, fileID, tx.setID); err != nil {
		return fmt.Errorf("catalog: touching last_seen_run for file %d: %w", fileID, err)
	}
	return nil
}

// InsertModification executes the 2.4 versioning pair in THIS
// transaction: the new row carries modified_of = oldFileID, and the
// old row gets superseded_by = <new id>. The old row is NOT otherwise
// touched - it keeps its image_id and copied state, because the media
// that holds the old content must keep being described accurately
// (2.4 modification versioning, FR-030). Returns the new file_id.
func (tx *ScanTx) InsertModification(ctx context.Context, oldFileID int64, f FileRow) (int64, error) {
	linkTarget := sql.NullString{String: f.LinkTarget, Valid: f.LinkTarget != ""}
	hash := f.Hash
	if hash != nil && len(hash) == 0 {
		hash = nil
	}
	detail := sql.NullString{String: f.StatusDetail, Valid: f.StatusDetail != ""}
	// The INSERT projection differs from insertColumns only in
	// modified_of: same 26 columns, tuple position 20 filled with ?.
	const modValues = `?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
	                   NULL, ?, ?, ?, ?, ?, ?, ?, ?, NULL, 0, 0, NULL, NULL, 0`
	res, err := tx.conn.ExecContext(ctx,
		fmt.Sprintf(`INSERT INTO files (%s) VALUES (%s)`, insertColumns, modValues),
		tx.setID, f.RelPath, f.EntryKind, f.Size, f.Mode, f.UID, f.GID, f.MtimeNS,
		f.Dev, f.Inode, f.Nlink,
		linkTarget, hash, tx.runID, tx.runID, tx.now().UnixNano(),
		f.Status, detail, oldFileID)
	if err != nil {
		return 0, fmt.Errorf("catalog: inserting modification of file %d (%q): %w", oldFileID, f.RelPath, err)
	}
	newID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("catalog: file id after inserting modification of %q: %w", f.RelPath, err)
	}
	up, err := tx.conn.ExecContext(ctx,
		`UPDATE files SET superseded_by = ? WHERE file_id = ? AND set_id = ?`,
		newID, oldFileID, tx.setID)
	if err != nil {
		return 0, fmt.Errorf("catalog: superseding file %d with %d: %w", oldFileID, newID, err)
	}
	if n, _ := up.RowsAffected(); n == 0 {
		return 0, fmt.Errorf("catalog: superseding file %d: no such active file_id in set %q", oldFileID, tx.setID)
	}
	return newID, nil
}

// MarkMissing stamps status='missing' on the given rows (the 3.7
// not-seen outcome, FR-032). Records and media are untouched: this is a
// status write, nothing else - no assignment is created, none is
// cleared, and the row keeps its hash and lineage for audit (deletions
// never free planner capacity; 3.7). Returns the number of rows
// changed. The caller (reconciler) selects the file_ids as
// catalog-active rows of the set whose relpath was not in the scan's
// seen set; this method never selects on its own so the safety gate
// lives in ONE place.
func (tx *ScanTx) MarkMissing(ctx context.Context, fileIDs []int64) (int, error) {
	total := 0
	for _, chunk := range chunkInt64s(fileIDs, activeInChunk) {
		q := fmt.Sprintf(`UPDATE files SET status = 'missing' WHERE set_id = ? AND file_id IN (%s)`,
			placeholders(len(chunk)))
		res, err := tx.conn.ExecContext(ctx, q, append([]any{tx.setID}, argsOf64(chunk)...)...)
		if err != nil {
			return total, fmt.Errorf("catalog: marking %d rows missing: %w", len(chunk), err)
		}
		n, _ := res.RowsAffected()
		total += int(n)
	}
	return total, nil
}

// Commit ends the transaction successfully (COMMIT) and returns the
// dedicated connection to the pool. A second Commit/Rollback is a
// no-op returning nil (the done flag mirrors the idempotent release
// pattern of lock.go).
func (tx *ScanTx) Commit() error {
	if tx.done {
		return nil
	}
	tx.done = true
	if _, err := tx.conn.ExecContext(context.Background(), "COMMIT"); err != nil {
		// Best-effort rollback so the connection never returns to the
		// pool with an open transaction.
		_, _ = tx.conn.ExecContext(context.Background(), "ROLLBACK")
		_ = tx.conn.Close()
		return fmt.Errorf("catalog: committing scan transaction (run %d): %w", tx.runID, err)
	}
	if err := tx.conn.Close(); err != nil {
		return fmt.Errorf("catalog: closing scan transaction connection: %w", err)
	}
	return nil
}

// Rollback aborts the transaction and returns the connection. It is the
// deferred safety net of every reconciliation batch: any statement
// error must leave the catalog exactly as before (atomic batch, 3.7).
func (tx *ScanTx) Rollback() error {
	if tx.done {
		return nil
	}
	tx.done = true
	if _, err := tx.conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		_ = tx.conn.Close()
		return fmt.Errorf("catalog: rolling back scan transaction (run %d): %w", tx.runID, err)
	}
	return tx.conn.Close()
}

// activeInChunk bounds IN-clause chunking well under every variable
// limit (SQLite's default ceiling is 32766 binds; 500 leaves room for
// the set_id bind and future growth).
const activeInChunk = 500

// placeholders renders n SQL placeholders.
func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// chunkStrings splits xs into slices of at most n elements.
func chunkStrings(xs []string, n int) [][]string {
	var out [][]string
	for i := 0; i < len(xs); i += n {
		end := i + n
		if end > len(xs) {
			end = len(xs)
		}
		out = append(out, xs[i:end])
	}
	return out
}

// chunkInt64s splits xs into slices of at most n elements.
func chunkInt64s(xs []int64, n int) [][]int64 {
	var out [][]int64
	for i := 0; i < len(xs); i += n {
		end := i + n
		if end > len(xs) {
			end = len(xs)
		}
		out = append(out, xs[i:end])
	}
	return out
}

// argsOf converts a []string chunk to []any for QueryContext variadic
// expansion.
func argsOf(chunk []string) []any {
	out := make([]any, len(chunk))
	for i, s := range chunk {
		out[i] = s
	}
	return out
}

// argsOf64 converts a []int64 chunk to []any for ExecContext variadic
// expansion. (Go cannot overload argsOf by slice element type.)
func argsOf64(chunk []int64) []any {
	out := make([]any, len(chunk))
	for i, v := range chunk {
		out[i] = v
	}
	return out
}
