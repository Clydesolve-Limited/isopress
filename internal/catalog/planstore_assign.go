package catalog

// T2.2 (phase 2): the balancer's catalog surface (DESIGN_SPEC 1.2
// layering - the catalog package is the ONLY component issuing SQL;
// 4.3 item-construction reads; 4.4 wasted-residual visibility; 4.5
// single-transaction assignment; 2.4/FR-011 lock guards).
//
// This file supplies everything the T2.2 balancer (internal/plan)
// needs beyond planstore.go and dirs.go, and nothing else:
//
//   - the read side, available on *DB in NO transaction so the
//     read-only `report` command runs the identical planning pipeline
//     over a WAL snapshot (4.3): ListActiveForPlan (every active
//     files row with the item-construction columns, including the
//     hard-link family fields dev/inode/nlink and the stored
//     files.chunk_size), ChunkStates (stored chunk rows of active
//     previously-chunked files, joined with the parent's chunk_size -
//     the 4.2 boundary-stability input and the LOCKED detector for
//     chunked files, which never carry files.image_id), Destinations
//     (every images row with its current fill - the LOCKED set and
//     the baseline of the D-05 residual figure);
//   - the write side on PlanTx (all joining the ONE planning
//     transaction of 4.5): InsertImage (images row, state 'planned'),
//     AssignFile (files.image_id + files.copied = 0 mirror),
//     AssignChunk (chunks.image_id), DropChunkState (the folded T2.1
//     review note NB2: a previously-chunked file now assigned WHOLE
//     sheds its stale chunk rows and un-stamps files.chunked /
//     chunk_count / chunk_size INSIDE the same transaction, because
//     restore reads chunk rows and the catalog must never lie).
//
// Every assignment write upholds the FR-011 lock mechanically, the
// established pattern of this layer: the UPDATE matches
// image_id IS NULL only, and a zero-row update is an error naming the
// cause (already assigned / missing row / wrong set) - never a silent
// no-op, never a re-target of a locked assignment.
//
// As with scanstore.go, planstore.go, and dirs.go, the plan service
// never imports this package: plan.ActiveFile / plan.StoredChunk /
// plan.DestinationState / plan.ImageSpec mirror the shapes below field
// for field and the cli wiring layer maps between the two
// (DESIGN_SPEC 1.2). Error classification mirrors the established
// pattern: statement failures inside an open PlanTx are plain wrapped
// errors; the read side wraps with query context.

import (
	"context"
	"database/sql"
	"fmt"
)

// ImageSpec is one images row the planner inserts (DESIGN_SPEC 2.3,
// used verbatim): 1-based sequential image_id (6.4), the profile's
// nominal and usable figures (D-06 - resolved exactly once, upstream,
// and carried through here untouched), the profile name, state
// 'planned'. mode / final_path / built_at / verified_at /
// bytes_written stay at their schema defaults until the write path
// fills them.
type ImageSpec struct {
	ImageID      int64
	NominalSize  int64
	UsableSize   int64
	MediaProfile string
}

// InsertImage stores one images row in state 'planned'. A duplicate
// (set_id, image_id) is refused by the PRIMARY KEY - the planner
// allocates an id NOT IN USE by any existing row (lowest-unused
// minting, DESIGN_SPEC 4.6), so a duplicate (set_id, image_id) can
// only mean a concurrent planner violated the single-writer model
// (D-11) or a hand-edited catalog - fail loudly, never silently.
func (tx *PlanTx) InsertImage(ctx context.Context, img ImageSpec) error {
	if img.ImageID <= 0 {
		return fmt.Errorf("catalog: inserting image %d: image id must be positive", img.ImageID)
	}
	if img.UsableSize <= 0 || img.NominalSize <= 0 {
		return fmt.Errorf(
			"catalog: inserting image %d: nominal %d / usable %d must be positive",
			img.ImageID, img.NominalSize, img.UsableSize)
	}
	if img.MediaProfile == "" {
		return fmt.Errorf("catalog: inserting image %d: empty media profile", img.ImageID)
	}
	_, err := tx.conn.ExecContext(ctx,
		`INSERT INTO images (set_id, image_id, nominal_size, usable_size, media_profile, state)
		 VALUES (?, ?, ?, ?, ?, 'planned')`,
		tx.setID, img.ImageID, img.NominalSize, img.UsableSize, img.MediaProfile)
	if err != nil {
		return fmt.Errorf("catalog: inserting image %d for set %q: %w",
			img.ImageID, tx.setID, err)
	}
	return nil
}

// AssignFile sets files.image_id for one whole-file item and mirrors
// files.copied = 0 in the same statement (DESIGN_SPEC 4.5). The
// FR-011 lock guard is mechanical: the UPDATE only matches
// image_id IS NULL, so a locked assignment can never be re-targeted;
// a zero-row update is an error naming the cause (already assigned,
// missing row, or wrong set).
func (tx *PlanTx) AssignFile(ctx context.Context, fileID, imageID int64) error {
	if imageID <= 0 {
		return fmt.Errorf("catalog: assigning file %d to image %d: image id must be positive",
			fileID, imageID)
	}
	res, err := tx.conn.ExecContext(ctx,
		`UPDATE files SET image_id = ?, copied = 0
		 WHERE file_id = ? AND set_id = ? AND image_id IS NULL`,
		imageID, fileID, tx.setID)
	if err != nil {
		return fmt.Errorf("catalog: assigning file %d to destination %d: %w",
			fileID, imageID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf(
			"catalog: assigning file %d to destination %d: no unassigned row in set %q "+
				"(already assigned rows are locked, FR-011)",
			fileID, imageID, tx.setID)
	}
	return nil
}

// AssignChunk sets chunks.image_id for one chunk item (the chunk
// groups of a file are assigned in the same transaction as their
// parent, FR-041 as amended - a PLAN never leaves a file newly
// partially planned, and a pre-existing partial set 4.6 completes by
// assigning only its unassigned half). Same FR-011 NULL-guard and
// zero-row-is-an-error discipline as AssignFile, plus AssignFile's
// mark reset: the statement also writes copied = 0 and
// verified_at = NULL (M8), so a freshly assigned row can never carry
// a stale mark into the write path - and an ALREADY-assigned row can
// never match the image_id IS NULL guard, so locked halves are
// untouched (defense-in-depth under the compute-time refusal).
func (tx *PlanTx) AssignChunk(ctx context.Context, fileID int64, chunkIndex int, imageID int64) error {
	if imageID <= 0 {
		return fmt.Errorf("catalog: assigning chunk %d of file %d to image %d: image id must be positive",
			chunkIndex, fileID, imageID)
	}
	res, err := tx.conn.ExecContext(ctx,
		`UPDATE chunks SET image_id = ?, copied = 0, verified_at = NULL
		 WHERE set_id = ? AND file_id = ? AND chunk_index = ? AND image_id IS NULL`,
		imageID, tx.setID, fileID, chunkIndex)
	if err != nil {
		return fmt.Errorf("catalog: assigning chunk %d of file %d to destination %d: %w",
			chunkIndex, fileID, imageID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf(
			"catalog: assigning chunk %d of file %d to destination %d: no unassigned chunk row "+
				"in set %q (already assigned rows are locked, FR-011)",
			chunkIndex, fileID, imageID, tx.setID)
	}
	return nil
}

// DropChunkState is the folded T2.1 review note NB2: it deletes the
// file's chunk rows AND un-stamps files.chunked / chunk_count /
// chunk_size (chunk_size back to NULL), in the SAME transaction. The
// scenario: a file chunked under a small destination profile, then
// clear-assignments, then re-planned under a larger profile where it
// fits WHOLE. Without this, the catalog would keep the stale chunk
// rows and the chunked stamp while files.image_id describes a
// whole-file assignment - and restore (DESIGN_SPEC 8.4) reads chunk
// rows, so the catalog would lie about the file's placement shape.
//
// Returns the number of chunk rows deleted. A row with image_id NOT
// NULL aborts with ErrChunkRowAssigned (FR-011: the planner only ever
// rewrites chunk state of fully unassigned files; clear-assignments
// is the only unlock path).
func (tx *PlanTx) DropChunkState(ctx context.Context, fileID int64) (int, error) {
	rows, err := storedChunks(ctx, tx.conn, tx.setID, fileID)
	if err != nil {
		return 0, err
	}
	for _, r := range rows {
		if r.ImageID.Valid {
			return 0, fmt.Errorf("%w: file %d chunk %d has image_id %d",
				ErrChunkRowAssigned, fileID, r.ChunkIndex, r.ImageID.Int64)
		}
	}
	if len(rows) > 0 {
		if _, err := tx.conn.ExecContext(ctx,
			`DELETE FROM chunks WHERE set_id = ? AND file_id = ?`, tx.setID, fileID); err != nil {
			return 0, fmt.Errorf("catalog: dropping chunk rows of file %d: %w", fileID, err)
		}
	}
	res, err := tx.conn.ExecContext(ctx,
		`UPDATE files SET chunked = 0, chunk_count = 0, chunk_size = NULL
		 WHERE file_id = ? AND set_id = ?`,
		fileID, tx.setID)
	if err != nil {
		return 0, fmt.Errorf("catalog: un-stamping chunk state of file %d: %w", fileID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, fmt.Errorf("catalog: dropping chunk state of file %d: no such file_id in set %q",
			fileID, tx.setID)
	}
	return len(rows), nil
}

// --- read side (*DB; usable in NO transaction - the `report` path) ---------

// PlanActiveFile is one catalog-ACTIVE files row as the balancer sees
// it (status='active' AND superseded_by IS NULL - the v_active_files
// predicate, spelled out). It carries the 4.3 item-construction
// columns: the kind and size (symlinks: len(target)), the stored
// files.chunk_size (boundary stability), the hard-link family fields
// (dev/inode/nlink; link_group is NOT populated at scan time - see the
// balancer's family-grouping note), and the current assignment
// (ImageID 0 = unassigned; non-zero = LOCKED).
type PlanActiveFile struct {
	FileID    int64
	RelPath   string
	EntryKind string
	Size      int64
	ChunkSize int64 // sql.NullInt64 -> 0 (never chunked)
	Dev       int64
	Inode     int64
	Nlink     int64
	ImageID   int64 // sql.NullInt64 -> 0 (unassigned)
}

// ListActiveForPlan reads every catalog-ACTIVE files row of the set,
// ordered by file_id - the balancer's ONLY file source (planning runs
// as a separate process days after the scan and reads only the
// catalog, per the empty-dir durability ruling that applies to files
// a fortiori). The full active set (not just the unassigned half) is
// deliberate: hard-link family reduction (DESIGN_SPEC 2.4) needs the
// assigned members too, to know which rows are family primaries.
func (db *DB) ListActiveForPlan(ctx context.Context, setID string) ([]PlanActiveFile, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT file_id, relpath, entry_kind, size, COALESCE(chunk_size, 0),
		        dev, inode, nlink, COALESCE(image_id, 0)
		 FROM files
		 WHERE set_id = ? AND status = 'active' AND superseded_by IS NULL
		 ORDER BY file_id`, setID)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing active files for planning, set %q: %w", setID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only close; rows.Err dominates
	var out []PlanActiveFile
	for rows.Next() {
		var f PlanActiveFile
		if err := rows.Scan(&f.FileID, &f.RelPath, &f.EntryKind, &f.Size, &f.ChunkSize,
			&f.Dev, &f.Inode, &f.Nlink, &f.ImageID); err != nil {
			return nil, fmt.Errorf("catalog: scanning active plan row: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: iterating active plan rows: %w", err)
	}
	return out, nil
}

// ChunkState is one stored chunks row of a previously-chunked file,
// joined with the parent's stored files.chunk_size (the 4.2
// boundary-stability input the balancer compares against the current
// effective size). ImageID is the chunk's own assignment: a chunked
// file never carries files.image_id (DESIGN_SPEC 4.5 assigns chunked
// files via chunks.image_id only), so a set whose rows ALL carry an
// image_id is a LOCKED, already-placed file the planner must skip.
type ChunkState struct {
	FileID      int64
	ChunkSize   int64 // parent files.chunk_size
	Index       int
	Offset      int64
	Length      int64
	ImageID     int64 // sql.NullInt64 -> 0 (unassigned)
	HashPresent bool  // chunks.hash NOT NULL: written at least once
}

// ChunkStates reads the stored chunk rows of every ACTIVE,
// previously-chunked file of the set (files.chunk_size IS NOT NULL),
// ordered by (file_id, chunk_index). The join carries the parent's
// chunk_size per row so the balancer can decide LOCKED / REUSE /
// RECHUNK from ONE read. A file stamped chunked but with zero rows
// (a catalog-integrity edge) simply yields no rows here; the balancer
// falls back to the create-fresh path (or the NB2 whole-file drop).
func (db *DB) ChunkStates(ctx context.Context, setID string) ([]ChunkState, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT c.file_id, f.chunk_size, c.chunk_index, c.offset, c.length, c.image_id, c.hash
		 FROM chunks c
		 JOIN files f ON f.file_id = c.file_id
		 WHERE c.set_id = ? AND f.set_id = ?
		   AND f.status = 'active' AND f.superseded_by IS NULL
		   AND f.chunk_size IS NOT NULL
		 ORDER BY c.file_id, c.chunk_index`, setID, setID)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading chunk states for set %q: %w", setID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only close; rows.Err dominates
	var out []ChunkState
	for rows.Next() {
		var (
			cs    ChunkState
			image sql.NullInt64
			hash  []byte
		)
		if err := rows.Scan(&cs.FileID, &cs.ChunkSize, &cs.Index, &cs.Offset,
			&cs.Length, &image, &hash); err != nil {
			return nil, fmt.Errorf("catalog: scanning chunk state: %w", err)
		}
		cs.ImageID = image.Int64
		cs.HashPresent = len(hash) > 0
		out = append(out, cs)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: iterating chunk states: %w", err)
	}
	return out, nil
}

// DestinationState is one existing images row with its current fill:
// the LOCKED destinations of an incremental plan (DESIGN_SPEC 4.3),
// the media count baseline (FR-008/FR-009), and the input of the
// D-05 wasted-residual figure (4.4). UsedBytes sums assigned
// whole-file and symlink sizes (files.image_id) plus assigned chunk
// lengths (chunks.image_id); ItemCount counts those payload
// assignments; DirCount counts assigned directories (zero capacity by
// design - mkdir is idempotent, 2.3).
type DestinationState struct {
	ImageID      int64
	NominalSize  int64
	UsableSize   int64
	MediaProfile string
	UsedBytes    int64
	ItemCount    int
	DirCount     int
}

// Destinations reads every images row of the set with its current
// fill, ordered by image_id. The fill is computed from the assignment
// columns only (metadata-only: no source read), in three grouped
// passes folded in Go - simple, index-driven, and exact.
func (db *DB) Destinations(ctx context.Context, setID string) ([]DestinationState, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT image_id, nominal_size, usable_size, media_profile
		 FROM images WHERE set_id = ? ORDER BY image_id`, setID)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing destinations for set %q: %w", setID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only close; rows.Err dominates
	var out []DestinationState
	byID := make(map[int64]int)
	for rows.Next() {
		var d DestinationState
		if err := rows.Scan(&d.ImageID, &d.NominalSize, &d.UsableSize, &d.MediaProfile); err != nil {
			return nil, fmt.Errorf("catalog: scanning destination row: %w", err)
		}
		byID[d.ImageID] = len(out)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: iterating destination rows: %w", err)
	}
	if err := db.foldPayloadFill(ctx, setID, byID, out); err != nil {
		return nil, err
	}
	if err := db.foldDirFill(ctx, setID, byID, out); err != nil {
		return nil, err
	}
	return out, nil
}

// foldPayloadFill adds the files+chunks assignment sums per image.
func (db *DB) foldPayloadFill(ctx context.Context, setID string,
	byID map[int64]int, out []DestinationState) error {
	fill := func(query, what string) error {
		rows, err := db.sql.QueryContext(ctx, query, setID)
		if err != nil {
			return fmt.Errorf("catalog: reading %s fill for set %q: %w", what, setID, err)
		}
		defer rows.Close() //nolint:errcheck // read-only close; rows.Err dominates
		for rows.Next() {
			var (
				imageID int64
				nbytes  sql.NullInt64
				count   int
			)
			if err := rows.Scan(&imageID, &nbytes, &count); err != nil {
				return fmt.Errorf("catalog: scanning %s fill row: %w", what, err)
			}
			if idx, ok := byID[imageID]; ok {
				out[idx].UsedBytes += nbytes.Int64
				out[idx].ItemCount += count
			}
		}
		return rows.Err()
	}
	if err := fill(
		`SELECT image_id, SUM(size), COUNT(*) FROM files
		 WHERE set_id = ? AND image_id IS NOT NULL GROUP BY image_id`, "file"); err != nil {
		return err
	}
	return fill(
		`SELECT image_id, SUM(length), COUNT(*) FROM chunks
		 WHERE set_id = ? AND image_id IS NOT NULL GROUP BY image_id`, "chunk")
}

// foldDirFill adds the directories assignment counts per image.
func (db *DB) foldDirFill(ctx context.Context, setID string,
	byID map[int64]int, out []DestinationState) error {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT image_id, COUNT(*) FROM directories
		 WHERE set_id = ? AND image_id IS NOT NULL GROUP BY image_id`, setID)
	if err != nil {
		return fmt.Errorf("catalog: reading dir fill for set %q: %w", setID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only close; rows.Err dominates
	for rows.Next() {
		var (
			imageID int64
			count   int
		)
		if err := rows.Scan(&imageID, &count); err != nil {
			return fmt.Errorf("catalog: scanning dir fill row: %w", err)
		}
		if idx, ok := byID[imageID]; ok {
			out[idx].DirCount += count
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("catalog: iterating dir fill rows: %w", err)
	}
	return nil
}
