package catalog

// T2.1: the plan-side catalog surface (DESIGN_SPEC 1.2 layering - the
// catalog package is the ONLY component issuing SQL; 2.3 chunks table
// used verbatim; 4.2 boundary-stability reuse; 4.3 "Chunk rows
// (offset, length, set_id) are created or reused here"; 4.5
// single-transaction assignment).
//
// This file supplies everything the T2.1/T2.2 planner needs over the
// chunks table and the files chunk columns, and nothing else:
//
//   - chunk row creation and reuse (EnsureChunkRows,
//     ReuseExistingChunkRows) for the boundary-stable decomposition;
//   - the re-chunk path (ReplaceAllChunks) for a changed
//     effective chunk size, with the written-vs-unwritten row policy
//     documented there;
//   - the files.chunk_size / chunked / chunk_count stamp and read
//     (StoreChunkSize, ReadChunkSize) - the stored size is what lets
//     the next plan decide REUSE vs RECHUNK (DESIGN_SPEC 4.2);
//   - PlanTx: ONE BEGIN IMMEDIATE transaction over a dedicated
//     connection, the same idiom as ScanTx. T2.2 wraps the whole
//     planning write side (images rows, assignments, and these chunk
//     functions) in ONE PlanTx per DESIGN_SPEC 4.5: any failure rolls
//     the plan back completely, and a failed plan leaves the catalog
//     exactly as before.
//
// Transactional expectations for the planner (T2.2): BeginPlanTx
// takes the write reservation up front (BEGIN IMMEDIATE rationale in
// the ScanTx doc), every Ensure/Reuse/Replace/Store call MUST run on
// that PlanTx's connection so it joins the single transaction, and
// exactly one of Commit or Rollback ends it. The READ side
// (ReadChunkSize, StoredChunks on *DB) is deliberately available
// outside any transaction so the `report` command can run the
// identical planning pipeline strictly read-only, in no transaction
// (DESIGN_SPEC 4.3), over a WAL snapshot.
//
// As with scanstore.go, the plan service (internal/plan) does NOT
// import this package: it declares plan.ChunkSpec / plan.Decision and
// the cli wiring layer maps between the structurally identical shapes
// (DESIGN_SPEC 1.2, "services depend on interfaces for catalog
// access"). Error classification mirrors scanstore.go: connection
// acquisition failure is a KindOpen CatalogError (exit 4); individual
// statement failures inside an open PlanTx are plain wrapped errors.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrChunkMismatch classifies a stored-chunk-rows vs requested-
// decomposition disagreement (count, index, offset, or length). The
// planner treats it as the re-chunk path: with a changed effective
// size the boundaries legitimately shift, and ReplaceAllChunks
// replaces the stored decomposition. It also fires on genuine catalog
// inconsistency (files.chunk_size matches the effective size but the
// rows do not) - the caller decides, this layer only reports.
var ErrChunkMismatch = errors.New("catalog: stored chunk rows do not match the decomposition")

// ErrChunkRowAssigned guards the FR-011 lock invariant: ReplaceAllChunks
// refuses to rewrite the chunk rows of a file whose chunks still carry
// an assignment (image_id NOT NULL). Assignments are locked once set;
// clear-assignments (which sets chunks.image_id = NULL and deletes
// nothing) is the only path that unlocks them, and the planner only
// ever re-chunks unassigned files (DESIGN_SPEC 2.4, 4.5).
var ErrChunkRowAssigned = errors.New("catalog: chunk rows belong to an assigned (locked) file")

// ChunkSpec is ONE chunk of a decomposition: 0-based index, byte
// offset in the parent file, byte length. Shape-compatible with
// plan.ChunkSpec; the cli wiring layer maps between the two
// (DESIGN_SPEC 1.2). Length must be > 0; offsets must be contiguous.
type ChunkSpec struct {
	Index  int
	Offset int64
	Length int64
}

// ChunkRow is one stored chunks row (DESIGN_SPEC 2.3, used verbatim):
// Hash is the raw sha256 BLOB, NULL until the chunk's first write
// stores it (lazy first-write hashing, 4.3); ImageID is the assignment
// (NULL = unassigned, locked like file assignments); Copied and
// VerifiedAt mirror the write-verify discipline of 5.1.
type ChunkRow struct {
	ChunkID    int64
	FileID     int64
	ChunkIndex int
	Offset     int64
	Length     int64
	Hash       []byte        // NULL scans as nil
	ImageID    sql.NullInt64 // assignment; NULL = unassigned
	Copied     bool
	VerifiedAt sql.NullInt64 // NULL until verify-media stamps it
}

// RechunkResult reports what a re-chunk replaced, in row counts, so
// the plan report can quantify the cost of a profile change
// (DESIGN_SPEC 4.2: "The plan report states explicitly when a profile
// change forces this re-hashing"): RemovedWritten chunks are re-read
// and re-hashed at their next write; RemovedUnwritten chunks were
// replaced for free.
type RechunkResult struct {
	RemovedUnwritten int // rows with hash NULL deleted
	RemovedWritten   int // rows with hash NOT NULL deleted (re-hashed at next write)
}

// chunkColumns is the shared chunks projection.
const chunkColumns = `chunk_id, file_id, chunk_index, offset, length, hash, image_id, copied, verified_at`

// queryer is the minimal multi-row read seam shared by *sql.DB (the
// read-only report path) and *sql.Conn (inside PlanTx), so the read
// helpers work on both without duplicating SQL.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// queryRower is the single-row read seam (same sharing rationale).
type queryRower interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// execer is the write seam shared by *sql.DB and *sql.Conn.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// --- read-only surface (*DB; usable outside any transaction) -----------------

// StoredChunks reads EVERY stored chunk row of the file, ordered by
// chunk_index (the 2.3 "0-based, ascending file order"). It is
// available on *DB - not only on PlanTx - so the `report` command can
// run the identical planning pipeline strictly read-only, in no
// transaction (DESIGN_SPEC 4.3), and so tests can assert row state
// between transactions. file_id is globally unique; the set_id filter
// is belt-and-braces set scoping, consistent with the rest of the
// layer.
func (db *DB) StoredChunks(ctx context.Context, setID string, fileID int64) ([]ChunkRow, error) {
	return storedChunks(ctx, db.sql, setID, fileID)
}

// ReadChunkSize reads files.chunk_size for one file: the effective
// chunk size the file was decomposed with previously. chunked is false
// (and chunkSize 0) when the column is NULL - the file was never
// chunked. This is the stored state the boundary-stability decision
// (plan.DecideChunking) compares against the current effective size
// (DESIGN_SPEC 4.2).
func (db *DB) ReadChunkSize(ctx context.Context, setID string, fileID int64) (chunkSize int64, chunked bool, err error) {
	return readChunkSize(ctx, db.sql, setID, fileID)
}

// --- write surface (PlanTx; joins the single planning transaction) -----------

// PlanTx is ONE planning transaction over a dedicated pooled
// connection, started with BEGIN IMMEDIATE - the same idiom and
// rationale as ScanTx (see its doc): the write reservation is taken up
// front, so under the 2.1 connection policy a plan either commits
// whole or fails whole (DESIGN_SPEC 4.5, FR-010/FR-011 atomicity).
//
// T2.2 wraps the whole planning write side in exactly one PlanTx:
// insert images rows, ensure/reuse/replace chunk rows, stamp
// files.chunk_size, set files.image_id and chunks.image_id, mirror
// files.copied, commit. Lifecycle is identical to ScanTx: exactly one
// of Commit or Rollback, after which the connection is returned to the
// pool and further use is a driver error.
type PlanTx struct {
	conn  *sql.Conn
	runID int64
	setID string
	done  bool
}

// BeginPlanTx opens the planning transaction on a dedicated connection
// (see PlanTx for the BEGIN IMMEDIATE rationale). runID is the runs
// row the plan run records provenance under (runs.kind 'plan'); setID
// scopes every read and write. Connection-acquisition failure is a
// KindOpen CatalogError (exit 4); the BEGIN IMMEDIATE failure itself
// is a plain wrapped error (same classification as BeginScanTx).
func (db *DB) BeginPlanTx(ctx context.Context, runID int64, setID string) (*PlanTx, error) {
	conn, err := db.sql.Conn(ctx)
	if err != nil {
		return nil, &CatalogError{
			Kind: KindOpen,
			Path: db.path,
			Err:  fmt.Errorf("acquiring connection for plan transaction: %w", err),
		}
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("catalog: BEGIN IMMEDIATE for plan run %d: %w", runID, err)
	}
	return &PlanTx{conn: conn, runID: runID, setID: setID}, nil
}

// RunID returns the run this transaction writes provenance under.
func (tx *PlanTx) RunID() int64 { return tx.runID }

// SetID returns the set this transaction is scoped to.
func (tx *PlanTx) SetID() string { return tx.setID }

// EnsureChunkRows creates the chunk rows for fileID from the given
// decomposition if they are absent, and is idempotent:
//
//   - no rows stored yet -> insert every spec with hash NULL, image_id
//     NULL, copied 0, verified_at NULL (the 2.3 DDL verbatim; the
//     set_id is denormalized from tx.setID per the schema note);
//   - rows stored and they match the decomposition exactly (count,
//     indices, offsets, lengths) -> no-op, existing rows are untouched
//     (stored hashes and copied marks preserved bit-for-bit);
//   - rows stored but they do NOT match -> ErrChunkMismatch. This is
//     not the re-chunk path: call ReplaceAllChunks for a changed
//     effective size; EnsureChunkRows only ever CREATES a fresh
//     decomposition or confirms an existing one.
func (tx *PlanTx) EnsureChunkRows(ctx context.Context, fileID int64, specs []ChunkSpec) error {
	if err := validateChunkSpecs(specs); err != nil {
		return fmt.Errorf("catalog: ensuring chunk rows for file %d: %w", fileID, err)
	}
	rows, err := storedChunks(ctx, tx.conn, tx.setID, fileID)
	if err != nil {
		return err
	}
	if len(rows) > 0 {
		if err := verifyDecomposition(rows, specs); err != nil {
			return fmt.Errorf("catalog: ensuring chunk rows for file %d: %w", fileID, err)
		}
		return nil
	}
	return insertChunkRows(ctx, tx.conn, tx.setID, fileID, specs)
}

// ReuseExistingChunkRows fetches the STORED chunk rows for fileID and
// verifies them against the requested decomposition - the read side of
// the DESIGN_SPEC 4.2 reuse rule. On an exact match (count, indices,
// offsets, lengths) it returns the stored rows UNTOUCHED, hashes
// included: with the same effective chunk size the boundaries are
// identical by construction, so the planner reuses the rows and their
// hashes with zero source reads (the stakeholder-visible contract:
// re-plan with the same profile = zero re-reads).
//
// Any disagreement - including zero stored rows - is
// ErrChunkMismatch, which the caller takes as the create/re-chunk
// path. The caller verifies files.chunk_size == effective size
// (plan.DecideChunking) BEFORE calling this; the row-level check here
// is the second gate that catches catalog inconsistency.
func (tx *PlanTx) ReuseExistingChunkRows(ctx context.Context, fileID int64, specs []ChunkSpec) ([]ChunkRow, error) {
	if err := validateChunkSpecs(specs); err != nil {
		return nil, fmt.Errorf("catalog: reusing chunk rows for file %d: %w", fileID, err)
	}
	rows, err := storedChunks(ctx, tx.conn, tx.setID, fileID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("%w: file %d has no stored chunk rows", ErrChunkMismatch, fileID)
	}
	if err := verifyDecomposition(rows, specs); err != nil {
		return nil, fmt.Errorf("catalog: reusing chunk rows for file %d: %w", fileID, err)
	}
	return rows, nil
}

// ReplaceAllChunks re-chunks fileID: it replaces the ENTIRE
// stored decomposition with the given one, and reports the row counts
// it removed (DESIGN_SPEC 4.2 profile-change path).
//
// The row-level policy, enforced here and documented in
// plan.DecideChunking:
//
//   - The full stored decomposition is deleted, not merged. Stored rows
//     describe the OLD boundaries; mixing them with a new decomposition
//     would break the contiguity invariant (offset[i+1] ==
//     offset[i]+length[i]) that restore reassembly depends on.
//   - UNWRITTEN rows (hash NULL) are replaced for free: no hash exists
//     to lose, and their first write will store the new-range hash.
//   - WRITTEN rows (hash NOT NULL) are deleted with their stored hash
//     and copied mark; those byte ranges are re-read from the source
//     and re-hashed at their next write (4.2: files are "re-chunked
//     and re-hashed"; the plan report states "re-hashed at next
//     write", carried by Decision.RehashedAtNextWrite /
//     RechunkResult.RemovedWritten).
//   - A row with image_id NOT NULL aborts the whole call with
//     ErrChunkRowAssigned (FR-011 lock invariant: the planner never
//     rewrites locked assignments; clear-assignments first).
//
// The new rows are inserted with hash NULL, image_id NULL, copied 0,
// verified_at NULL. Stamp files.chunk_size with StoreChunkSize in the
// same transaction so the NEXT plan can decide reuse.
func (tx *PlanTx) ReplaceAllChunks(ctx context.Context, fileID int64, specs []ChunkSpec) (RechunkResult, error) {
	var zero RechunkResult
	if err := validateChunkSpecs(specs); err != nil {
		return zero, fmt.Errorf("catalog: re-chunking file %d: %w", fileID, err)
	}
	rows, err := storedChunks(ctx, tx.conn, tx.setID, fileID)
	if err != nil {
		return zero, err
	}
	var result RechunkResult
	for _, r := range rows {
		if r.ImageID.Valid {
			return zero, fmt.Errorf("%w: file %d chunk %d has image_id %d",
				ErrChunkRowAssigned, fileID, r.ChunkIndex, r.ImageID.Int64)
		}
		if len(r.Hash) > 0 {
			result.RemovedWritten++
		} else {
			result.RemovedUnwritten++
		}
	}
	if _, err := tx.conn.ExecContext(ctx,
		`DELETE FROM chunks WHERE set_id = ? AND file_id = ?`, tx.setID, fileID); err != nil {
		return zero, fmt.Errorf("catalog: deleting stored chunk rows of file %d: %w", fileID, err)
	}
	if err := insertChunkRows(ctx, tx.conn, tx.setID, fileID, specs); err != nil {
		return zero, err
	}
	return result, nil
}

// StoreChunkSize stamps the decomposition state onto the files row:
// chunked = 1, chunk_count, and chunk_size (the DESIGN_SPEC 2.3
// columns). files.chunk_size is the stored state the next plan's
// boundary-stability decision compares against the current effective
// size (4.2), so this MUST run in the same transaction as the chunk
// row writes it describes.
func (tx *PlanTx) StoreChunkSize(ctx context.Context, fileID int64, chunkSize int64, chunkCount int) error {
	if chunkSize <= 0 {
		return fmt.Errorf("catalog: storing chunk size %d for file %d: chunk size must be positive",
			chunkSize, fileID)
	}
	if chunkCount < 0 {
		return fmt.Errorf("catalog: storing chunk count %d for file %d: negative chunk count",
			chunkCount, fileID)
	}
	res, err := tx.conn.ExecContext(ctx,
		`UPDATE files SET chunked = 1, chunk_count = ?, chunk_size = ? WHERE file_id = ? AND set_id = ?`,
		chunkCount, chunkSize, fileID, tx.setID)
	if err != nil {
		return fmt.Errorf("catalog: storing chunk size for file %d: %w", fileID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("catalog: storing chunk size for file %d: no such file_id in set %q", fileID, tx.setID)
	}
	return nil
}

// Commit ends the transaction successfully (COMMIT) and returns the
// dedicated connection to the pool. A second Commit/Rollback is a
// no-op returning nil (the done-flag pattern of ScanTx).
func (tx *PlanTx) Commit() error {
	if tx.done {
		return nil
	}
	tx.done = true
	if _, err := tx.conn.ExecContext(context.Background(), "COMMIT"); err != nil {
		// Best-effort rollback so the connection never returns to the
		// pool with an open transaction.
		_, _ = tx.conn.ExecContext(context.Background(), "ROLLBACK")
		_ = tx.conn.Close()
		return fmt.Errorf("catalog: committing plan transaction (run %d): %w", tx.runID, err)
	}
	if err := tx.conn.Close(); err != nil {
		return fmt.Errorf("catalog: closing plan transaction connection: %w", err)
	}
	return nil
}

// Rollback aborts the transaction and returns the connection. It is
// the atomicity guarantee of 4.5: any failure anywhere in the planning
// transaction - chunk rows included - leaves the catalog exactly as
// before (FR-010: a failed plan changes nothing).
func (tx *PlanTx) Rollback() error {
	if tx.done {
		return nil
	}
	tx.done = true
	if _, err := tx.conn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		_ = tx.conn.Close()
		return fmt.Errorf("catalog: rolling back plan transaction (run %d): %w", tx.runID, err)
	}
	return tx.conn.Close()
}

// --- shared helpers ----------------------------------------------------------

// storedChunks reads every chunk row of the file through q, ordered by
// chunk_index.
func storedChunks(ctx context.Context, q queryer, setID string, fileID int64) ([]ChunkRow, error) {
	rows, err := q.QueryContext(ctx,
		fmt.Sprintf(`SELECT %s FROM chunks WHERE set_id = ? AND file_id = ? ORDER BY chunk_index`,
			chunkColumns),
		setID, fileID)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading chunk rows of file %d: %w", fileID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only close; rows.Err dominates
	var out []ChunkRow
	for rows.Next() {
		var (
			r      ChunkRow
			hash   []byte
			image  sql.NullInt64
			copied int64
			ver    sql.NullInt64
		)
		if err := rows.Scan(&r.ChunkID, &r.FileID, &r.ChunkIndex, &r.Offset, &r.Length,
			&hash, &image, &copied, &ver); err != nil {
			return nil, fmt.Errorf("catalog: scanning chunk row of file %d: %w", fileID, err)
		}
		r.Hash = hash
		r.ImageID = image
		r.Copied = copied != 0
		r.VerifiedAt = ver
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: iterating chunk rows of file %d: %w", fileID, err)
	}
	return out, nil
}

// readChunkSize reads files.chunk_size through q; NULL -> (0, false).
func readChunkSize(ctx context.Context, q queryRower, setID string, fileID int64) (int64, bool, error) {
	var (
		size    sql.NullInt64
		chunked int64
	)
	err := q.QueryRowContext(ctx,
		`SELECT chunk_size, chunked FROM files WHERE file_id = ? AND set_id = ?`,
		fileID, setID).Scan(&size, &chunked)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, false, fmt.Errorf("catalog: file %d not found in set %q", fileID, setID)
	case err != nil:
		return 0, false, fmt.Errorf("catalog: reading chunk size of file %d: %w", fileID, err)
	}
	return size.Int64, chunked != 0 && size.Valid, nil
}

// insertChunkRows inserts one row per spec with hash NULL, image_id
// NULL, copied 0, verified_at NULL (the DESIGN_SPEC 2.3 lazy-hash
// contract: "NULL until first write stores it"). set_id is
// denormalized from setID (the schema note: "populated
// transactionally with the parent file row" - here from the tx scope).
func insertChunkRows(ctx context.Context, e execer, setID string, fileID int64, specs []ChunkSpec) error {
	for _, s := range specs {
		_, err := e.ExecContext(ctx,
			`INSERT INTO chunks (set_id, file_id, chunk_index, offset, length,
			                     hash, image_id, copied, verified_at)
			 VALUES (?, ?, ?, ?, ?, NULL, NULL, 0, NULL)`,
			setID, fileID, s.Index, s.Offset, s.Length)
		if err != nil {
			return fmt.Errorf("catalog: inserting chunk %d of file %d: %w", s.Index, fileID, err)
		}
	}
	return nil
}

// validateChunkSpecs enforces the decomposition invariants the schema
// and restore reassembly depend on (DESIGN_SPEC 4.3, 8.4): non-empty,
// first offset exactly 0 (the decomposition MUST start at the file
// head - a nonzero-start decomposition would silently drop the file's
// leading bytes at first write, the truncation shape FR-041 forbids;
// folded T2.1 review note NB1), 0-based contiguous indices, positive
// lengths, contiguous offsets.
func validateChunkSpecs(specs []ChunkSpec) error {
	if len(specs) == 0 {
		return errors.New("empty decomposition")
	}
	if specs[0].Offset != 0 {
		return fmt.Errorf("first chunk offset %d must be 0 (decompositions start at the file head)",
			specs[0].Offset)
	}
	for i, s := range specs {
		if s.Index != i {
			return fmt.Errorf("chunk %d carries index %d: indices must be 0-based and ascending", i, s.Index)
		}
		if s.Length <= 0 {
			return fmt.Errorf("chunk %d has non-positive length %d", i, s.Length)
		}
		if i > 0 && s.Offset != specs[i-1].Offset+specs[i-1].Length {
			return fmt.Errorf("chunk %d offset %d is not contiguous (previous chunk ends at %d)",
				i, s.Offset, specs[i-1].Offset+specs[i-1].Length)
		}
	}
	return nil
}

// verifyDecomposition checks stored rows against the requested
// decomposition; any disagreement is ErrChunkMismatch (the caller's
// signal for the create/re-chunk path).
func verifyDecomposition(rows []ChunkRow, specs []ChunkSpec) error {
	if len(rows) != len(specs) {
		return fmt.Errorf("%w: stored %d rows, decomposition has %d chunks",
			ErrChunkMismatch, len(rows), len(specs))
	}
	for i, r := range rows {
		s := specs[i]
		if r.ChunkIndex != s.Index || r.Offset != s.Offset || r.Length != s.Length {
			return fmt.Errorf("%w: chunk %d stored as (index %d, offset %d, length %d), "+
				"decomposition requires (index %d, offset %d, length %d)",
				ErrChunkMismatch, i, r.ChunkIndex, r.Offset, r.Length, s.Index, s.Offset, s.Length)
		}
	}
	return nil
}
