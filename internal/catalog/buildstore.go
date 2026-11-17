package catalog

// T4.3: the build-side catalog surface (DESIGN_SPEC 1.2 layering - the
// catalog package is the ONLY component issuing SQL; 6.6 per-image
// content list; 2.3 images.state; IMPLEMENTATION_PLAN T4.3).
//
// This file supplies the four catalog acts the image builder
// (internal/imagebuild build.go) needs, and nothing else. The builder
// itself is catalog-free: it declares an imagebuild.BuildStore
// INTERFACE over primitive-shaped methods, and the cli wiring layer
// adapts *BuildStore onto it (the copyengine.Store / copy command
// precedent - neither service nor catalog imports the other).
//
//   - ReadChunkHashes: the STORED hashes of one image's chunk rows,
//     read AFTER staging (6.3 step 1: staging is the first write, so
//     by manifest time every chunk of the image carries its 4.3
//     capture). A NULL hash or a missing row is a loud error naming
//     the chunk - a content list with a NULL chunk hash would lie to
//     restore (8.4 verifies every chunk against it).
//
//   - BeginBuildTx: THE 6.6 ATOMICITY (FR-020/FR-033/FR-034): in ONE
//     transaction, REPLACE (delete-then-insert) the image's
//     image_contents rows with the FINAL content list - every payload
//     file, chunk, symlink, dir, plus the manifest and export
//     self-rows - AND flip images.state to 'building'. The content
//     list and the state flip commit together or not at all: a crash
//     mid-build can never leave a 'building' image with a stale or
//     partial content list, nor a fresh content list describing an
//     image that never started mastering. The REPLACE is deliberate
//     over an upsert: a rebuild of an image whose previous plan placed
//     DIFFERENT paths must not keep the stale rows of the old plan
//     (the copystore.go recorded caveat - the upsert cannot remove
//     paths this build no longer places; the delete-then-insert closes
//     it for image mode).
//
//   - MarkBuildFailed: the small follow-up transaction after any
//     post-'building' failure (xorriso non-zero, self-verify mismatch,
//     interruption): state 'failed' plus a run_events error row
//     carrying the transcript tail (6.2 "capture stdout+stderr to the
//     run log"). The caller may pass a cancelled run context; the cli
//     layer routes the post-cancel case through the detached-timeout
//     pattern (FinishRunDetached's precedent) - this method itself
//     takes whatever context it is given.
//
//   - FinishBuildTx: the 6.3 step-3 finalize transaction: state
//     'built', built_at, final_path, bytes_written, and mode 'iso' (the
//     2.3 CHECK column "set at first write") in ONE transaction, run
//     only after the fsync+rename of the .partial succeeded and the
//     extraction self-verify passed.
//
// Error classification mirrors the established pattern: statement and
// commit failures inside an open transaction are plain wrapped errors
// (the honest fatal class, DESIGN_SPEC 10.1); a missing images row is
// a loud wiring error (run plan first), never a silent no-op.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// buildEventDetailCap bounds the run_events detail text MarkBuildFailed
// records (the xorriso transcript is already tail-capped at 10 MiB by
// T4.1; this second, smaller cap keeps one event row sane in the
// catalog and in the report).
const buildEventDetailCap = 64 << 10 // 64 KiB

// BuildStore is the set/image/run-bound handle of the build-side
// catalog acts. Construct with (*DB).BuildStore; the handle is cheap
// (four fields) and follows the run's connection pool.
type BuildStore struct {
	db      *DB
	setID   string
	imageID int64
	runID   int64
}

// BuildStore returns the build handle for one image of one set under
// one run (the run id anchors the run_events rows).
func (db *DB) BuildStore(setID string, imageID, runID int64) *BuildStore {
	return &BuildStore{db: db, setID: setID, imageID: imageID, runID: runID}
}

// BuildContentRow is one FINAL image_contents row the builder computed
// (DESIGN_SPEC 2.3 column set verbatim). Kind must be one of the 2.3
// CHECK values; FileID 0 means NULL; ChunkIndex -1 means NULL (only
// kind 'chunk' carries an index, the CATALOG 0-based one).
type BuildContentRow struct {
	MediaPath  string
	Kind       string
	FileID     int64
	ChunkIndex int
	Size       int64
	Hash       []byte // 32-byte sha256; nil for dir/symlink (2.3 convention)
}

// buildContentKinds is the 2.3 image_contents CHECK list, spelled out
// for the insert-time validation (a wrong kind here would fail the
// statement anyway, but the message must NAME the row).
var buildContentKinds = map[string]bool{
	"file": true, "chunk": true, "symlink": true,
	"dir": true, "manifest": true, "export": true,
}

// ReadChunkHashes returns the STORED hash of each requested chunk, in
// request order. fileIDs and chunkIndexes are parallel slices (the
// primitive-shaped seam the imagebuild interface needs - the
// copyengine.Store precedent: primitive parameters so neither package
// imports the other). Every requested chunk must exist in the set and
// carry a 32-byte stored hash; anything else is a loud error naming
// the chunk (see the file doc).
func (bs *BuildStore) ReadChunkHashes(ctx context.Context, fileIDs []int64, chunkIndexes []int) ([][]byte, error) {
	if len(fileIDs) != len(chunkIndexes) {
		return nil, fmt.Errorf("catalog: ReadChunkHashes (set %q, image %d): %d file ids but %d chunk indexes (parallel-slice contract)",
			bs.setID, bs.imageID, len(fileIDs), len(chunkIndexes))
	}
	out := make([][]byte, len(fileIDs))
	for i := range fileIDs {
		var hash []byte
		err := bs.db.sql.QueryRowContext(ctx,
			`SELECT hash FROM chunks
			 WHERE set_id = ? AND file_id = ? AND chunk_index = ?`,
			bs.setID, fileIDs[i], chunkIndexes[i]).Scan(&hash)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil, fmt.Errorf("catalog: ReadChunkHashes (set %q, image %d): no chunk %d of file %d in set",
				bs.setID, bs.imageID, chunkIndexes[i], fileIDs[i])
		case err != nil:
			return nil, fmt.Errorf("catalog: reading stored hash of chunk %d of file %d (set %q): %w",
				chunkIndexes[i], fileIDs[i], bs.setID, err)
		case len(hash) != 32:
			return nil, fmt.Errorf(
				"catalog: chunk %d of file %d (set %q) stores a %d-byte hash, want 32 (D-09); "+
					"staging is the first write (6.3 step 1), so a NULL or malformed hash at build time is corrupt state",
				chunkIndexes[i], fileIDs[i], bs.setID, len(hash))
		}
		out[i] = hash
	}
	return out, nil
}

// BeginBuildTx is THE 6.6 ATOMICITY transaction (see the file doc):
// BEGIN (plain driver BEGIN - the single-writer flock of D-11 excludes
// competing writers, the copystore tx rationale), DELETE the image's
// existing image_contents rows, INSERT the FINAL list, UPDATE the
// images row to state 'building' (zero-row update = no such image, a
// loud wiring error), COMMIT. Any failure rolls the whole thing back:
// the catalog either carries the complete new content list with state
// 'building', or its previous state bit-for-bit.
func (bs *BuildStore) BeginBuildTx(ctx context.Context, rows []BuildContentRow) error {
	tx, err := bs.db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("catalog: beginning build transaction (set %q, image %d): %w", bs.setID, bs.imageID, err)
	}
	rollback := func(cause error) error {
		_ = tx.Rollback()
		return cause
	}

	// The REPLACE half (delete-then-insert, the S3 stale-row closure):
	// a rebuild whose new plan places different paths must not keep the
	// previous plan's rows under their old (set, image, path) keys.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM image_contents WHERE set_id = ? AND image_id = ?`,
		bs.setID, bs.imageID); err != nil {
		return rollback(fmt.Errorf("catalog: replacing image_contents of set %q image %d: deleting previous rows: %w",
			bs.setID, bs.imageID, err))
	}
	for i, r := range rows {
		if err := validateBuildContentRow(bs, r); err != nil {
			return rollback(fmt.Errorf("catalog: BeginBuild (set %q, image %d) row %d: %w",
				bs.setID, bs.imageID, i, err))
		}
		var fileID, chunkIndex any
		if r.FileID != 0 {
			fileID = r.FileID
		}
		if r.ChunkIndex >= 0 {
			chunkIndex = r.ChunkIndex
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO image_contents
			   (set_id, image_id, media_path, kind, file_id, chunk_index, size, hash)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			bs.setID, bs.imageID, r.MediaPath, r.Kind, fileID, chunkIndex, r.Size, r.Hash); err != nil {
			return rollback(fmt.Errorf("catalog: inserting image_contents row %q (set %q, image %d): %w",
				r.MediaPath, bs.setID, bs.imageID, err))
		}
	}
	// The state flip, SAME transaction: 'planned' (first build) or
	// 'built'/'failed'/'verified' (rebuild) -> 'building'.
	res, err := tx.ExecContext(ctx,
		`UPDATE images SET state = 'building' WHERE set_id = ? AND image_id = ?`,
		bs.setID, bs.imageID)
	if err != nil {
		return rollback(fmt.Errorf("catalog: setting image %d of set %q to building: %w", bs.imageID, bs.setID, err))
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return rollback(fmt.Errorf("catalog: BeginBuild: no images row for set %q image %d (run plan first)",
			bs.setID, bs.imageID))
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("catalog: committing build transaction (set %q, image %d): %w", bs.setID, bs.imageID, err)
	}
	return nil
}

// validateBuildContentRow enforces the row shape the 2.3 schema and the
// 6.6 contract need, with messages that name the offending row.
func validateBuildContentRow(bs *BuildStore, r BuildContentRow) error {
	switch {
	case r.MediaPath == "":
		return errors.New("empty media path")
	case !buildContentKinds[r.Kind]:
		return fmt.Errorf("kind %q is not one of the 2.3 image_contents CHECK values", r.Kind)
	case r.Size < 0:
		return fmt.Errorf("%s: negative size %d", r.MediaPath, r.Size)
	case r.Kind == "chunk" && r.ChunkIndex < 0:
		return fmt.Errorf("%s: chunk row without a catalog chunk index", r.MediaPath)
	case r.ChunkIndex >= 0 && r.Kind != "chunk":
		return fmt.Errorf("%s: kind %q carries a chunk index (only 'chunk' rows may)", r.MediaPath, r.Kind)
	case (r.Kind == "file" || r.Kind == "chunk" || r.Kind == "manifest" || r.Kind == "export") && len(r.Hash) != 32:
		// The 6.6 honesty rule: content rows of hashed kinds always
		// carry their 32-byte digest; a NULL here would lie to restore.
		return fmt.Errorf("%s: kind %q carries a %d-byte hash, want 32 (D-09)", r.MediaPath, r.Kind, len(r.Hash))
	case (r.Kind == "dir" || r.Kind == "symlink") && r.Hash != nil:
		return fmt.Errorf("%s: kind %q must carry a NULL hash (2.3 convention)", r.MediaPath, r.Kind)
	}
	return nil
}

// MarkBuildFailed stamps state 'failed' and records the run_events
// error row carrying the failure detail (xorriso transcript tail,
// verification mismatch, ...) - ONE small transaction (the 6.2 run-log
// clause; the state half pairs with BeginBuildTx's 'building' flip).
// detail is truncated to buildEventDetailCap bytes (tail - xorriso
// prints its diagnostics last, the T4.1 tail-cap rationale).
func (bs *BuildStore) MarkBuildFailed(ctx context.Context, detail string) error {
	if len(detail) > buildEventDetailCap {
		detail = fmt.Sprintf("[... %d earlier detail bytes omitted ...]\n",
			len(detail)-buildEventDetailCap) + detail[len(detail)-buildEventDetailCap:]
	}
	tx, err := bs.db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("catalog: beginning build-failure transaction (set %q, image %d): %w", bs.setID, bs.imageID, err)
	}
	rollback := func(cause error) error {
		_ = tx.Rollback()
		return cause
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE images SET state = 'failed' WHERE set_id = ? AND image_id = ?`,
		bs.setID, bs.imageID)
	if err != nil {
		return rollback(fmt.Errorf("catalog: setting image %d of set %q to failed: %w", bs.imageID, bs.setID, err))
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return rollback(fmt.Errorf("catalog: MarkBuildFailed: no images row for set %q image %d", bs.setID, bs.imageID))
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO run_events (run_id, at, level, relpath, detail)
		 VALUES (?, ?, 'error', NULL, ?)`,
		bs.runID, time.Now().UnixNano(), detail); err != nil {
		return rollback(fmt.Errorf("catalog: recording build failure event for run %d: %w", bs.runID, err))
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("catalog: committing build-failure transaction (set %q, image %d): %w", bs.setID, bs.imageID, err)
	}
	return nil
}

// FinishBuildTx is the 6.3 step-3 finalize transaction: state 'built',
// built_at, final_path, bytes_written, and mode 'iso' (2.3: "set at
// first write") - ONE transaction, run only after the fsync+rename and
// the self-verify passed. A zero-row update is a loud wiring error.
func (bs *BuildStore) FinishBuildTx(ctx context.Context, finalPath string, builtAt int64, bytesWritten int64) error {
	if finalPath == "" {
		return fmt.Errorf("catalog: FinishBuild (set %q, image %d): empty final path", bs.setID, bs.imageID)
	}
	if bytesWritten < 0 {
		return fmt.Errorf("catalog: FinishBuild (set %q, image %d): negative byte count %d", bs.setID, bs.imageID, bytesWritten)
	}
	tx, err := bs.db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("catalog: beginning build-finalize transaction (set %q, image %d): %w", bs.setID, bs.imageID, err)
	}
	rollback := func(cause error) error {
		_ = tx.Rollback()
		return cause
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE images SET state = 'built', built_at = ?, final_path = ?, bytes_written = ?, mode = 'iso'
		 WHERE set_id = ? AND image_id = ?`,
		builtAt, finalPath, bytesWritten, bs.setID, bs.imageID)
	if err != nil {
		return rollback(fmt.Errorf("catalog: finalizing image %d of set %q as built: %w", bs.imageID, bs.setID, err))
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return rollback(fmt.Errorf("catalog: FinishBuild: no images row for set %q image %d", bs.setID, bs.imageID))
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("catalog: committing build-finalize transaction (set %q, image %d): %w", bs.setID, bs.imageID, err)
	}
	return nil
}

// --- runs bookkeeping for the build command (2.4) -----------------------------

// BuildRun describes the runs row a build command opens before
// mastering (kind 'build', allowed by the 2.3 CHECK). Root is the
// DESTINATION directory the .iso files land in (runs.root: "source
// root or destination root").
type BuildRun struct {
	SetID     string
	Root      string
	StartedAt int64 // unix nanoseconds, UTC (the caller's clock)
}

// StartBuildRun inserts the runs row in status 'running' and returns
// its run_id (2.4 provenance: build failures and their transcripts
// land in run_events under it, and the final status is stamped on
// every exit path by the cli via FinishRunDetached). An unknown set
// fails before the insert with ErrNoSuchSet.
func (db *DB) StartBuildRun(ctx context.Context, run BuildRun) (int64, error) {
	if run.SetID == "" || run.Root == "" {
		return 0, fmt.Errorf("catalog: StartBuildRun requires a set id and a destination root")
	}
	var one int
	if err := db.sql.QueryRowContext(ctx,
		`SELECT 1 FROM sets WHERE set_id = ?`, run.SetID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("catalog: no set %q in catalog: %w", run.SetID, ErrNoSuchSet)
		}
		return 0, fmt.Errorf("catalog: checking set %q for the build run: %w", run.SetID, err)
	}
	res, err := db.sql.ExecContext(ctx,
		`INSERT INTO runs (set_id, kind, root, started_at, finished_at, status,
		                  exclude_json, stats_json, tool_version)
		 VALUES (?, 'build', ?, ?, NULL, 'running', '[]', '{}', ?)`,
		run.SetID, run.Root, run.StartedAt, ToolVersion)
	if err != nil {
		return 0, fmt.Errorf("catalog: starting build run for set %q: %w", run.SetID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("catalog: build run id after insert: %w", err)
	}
	return id, nil
}
