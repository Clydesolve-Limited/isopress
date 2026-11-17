package catalog

// T2.4: the clear-assignments catalog surface (DESIGN_SPEC 1.2 layering
// - the catalog package is the ONLY component issuing SQL; 4.5 clear
// semantics - "FR-010's clear is likewise one transaction: image_id =
// NULL, copied = 0, chunk image_id = NULL, chunk rows and hashes
// retained"; 2.4 locking enforcement - "the only other writer is
// clear-assignments, which sets image_id = NULL, clears copied flags
// and chunk placements, and deletes nothing (FR-010)"; PLAN_SPEC
// FR-010, FR-011).
//
// ClearPlan is the ONLY unlock path in the catalog layer: there is no
// code path anywhere that re-targets a set assignment (FR-011), and
// this one clears it - WHOLESALE when imageIDs is nil (the original
// T2.4 full clear, byte-for-byte unchanged), or SCOPED to a selection
// of image ids when imageIDs is non-nil (DESIGN_SPEC 4.6, FR-047: the
// selective unlock behind `clear-assignments --image N/--last`) - in
// ONE transaction, deleting NO file records, NO hashes, and NO chunk
// rows (FR-010: clearing shall not delete file records or hashes).
//
// # The images-row decision (documented per the T2.4 brief)
//
// The brief asked whether the cleared set's images rows should survive
// "unlocked" or be deleted. This implementation DELETES them, inside
// the same transaction, and the reason is mechanical, in
// internal/plan/balancer.go's Destinations/locking predicate:
//
//   - Balance treats EVERY existing images row as LOCKED: locked
//     destinations "are never in the open set, receive no new payload"
//     and - decisively - "new items only ever open NEW destinations"
//     whose ids the plan mints with newImageIDAllocator(locked) ->
//     idAlloc.mint() (lowest-unused, 4.6). A surviving images
//     row with zero assignments would keep id N IN USE: the re-plan
//     mints only ids not in use (4.6 lowest-unused), so the ids would
//     differ from the fresh sequence - and the row would still render
//     as a phantom `[locked]` destination in the report.
//   - The T2.4 acceptance criterion is exact-fresh-plan equivalence
//     ("clear-then-re-plan on unchanged inputs yields the same plan
//     outcome - same media count, same fill numbers"). That holds only
//     when the balancer sees ZERO locked destinations, i.e. zero
//     images rows, after the clear. A stale images row with no
//     assignments is also a lie the `report` command would render as a
//     phantom empty "[locked]" destination.
//   - The deletion cannot orphan anything: the three NULLing updates
//     above are set-scoped, so at the DELETE point no files, chunks,
//     or directories row of the set (or any set) references this set's
//     image ids. FR-010's "deletes nothing" clause protects FILE
//     records, hashes, and chunk rows - the payload state - and none of
//     those is an images row (the images row is the planner's own
//     bookkeeping, recreated identically by the next plan).
//
// Images-state scope (the S2-era note this replaces said "the only
// reachable images state is 'planned'" and flagged a revisit once the
// write paths landed; they have, and the revisit lands on the SAME
// decision): every state the 2.3 CHECK allows is reachable at the
// point this clear runs. The writers today: plan inserts 'planned'
// (planstore_assign.go InsertImage); the build path flips to
// 'building' (BeginBuildTx, also on rebuild from 'built'/'failed'/
// 'verified'), to 'built' with built_at/final_path/bytes_written/
// mode 'iso' at finalize (FinishBuildTx), or to 'failed'
// (MarkBuildFailed); T6.5-A's copy completion stamps 'built' with
// final_path and mode 'copy' (RecordCopyFinalPath - no built_at);
// verify-media stamps 'verified' (StampImageVerified). ClearPlan
// therefore deletes images rows in ANY of those states, and the
// state-guard-free delete stays correct for each:
//
//   - The balancer locks EVERY images row regardless of state, so the
//     exact-fresh-plan equivalence above still requires ZERO rows - a
//     surviving 'built' or 'verified' row would reserve its id and
//     render as a phantom "[locked]" destination exactly like a
//     'planned' one.
//   - The delete removes CATALOG rows only, never files: built media
//     on disk (the copy tree or the .iso) survives the clear - media
//     hygiene is the operator's call. What goes with the row is the
//     row-level provenance a 'built'/'verified' row carries
//     (final_path, built_at, verified_at), and that trade is
//     deliberate: those columns describe the placement the same
//     transaction released; the build/copy/verify runs rows and their
//     run_events survive the clear untouched (2.4 audit), the clear's
//     own runs row counts the removed images, and FR-010's protection
//     covers FILE records, hashes, and chunk rows - none of which is
//     an images row. The xorriso-gated e2e
//     TestClearAfterBuiltImageKeepsIsoOnDisk (internal/cli/
//     clear_gaps_e2e_test.go) pins this end to end: clearing a
//     really-built set removes the 'built' images row and its
//     image_contents rows, and the .iso stays on disk.
//
// # The image_contents closure (S4 / T4.3, the recorded S2 boundary)
//
// When S4 landed, ClearPlan additionally DELETES the set's
// image_contents rows, inside the same transaction (one statement
// below). The pairing is mechanical, exactly like the images-row
// decision above: image_contents is the FINALIZED PER-IMAGE CONTENT
// LIST of FR-020, written by the build/copy writers against specific
// (set, image, path) keys of specific images rows. Clearing the
// images rows (and the assignments that named their ids) while
// leaving content rows behind would strand rows describing media
// placements that no longer exist - and a re-plan that produces a
// DIFFERENT image layout would find stale rows of the previous plan
// under old keys (the copystore.go replaceImageContent caveat, which
// the S4 builder's delete-then-insert transaction closes for the
// build path and THIS statement closes for the clear path). The rows
// are recreated by the next write run (copy mode per-item, build mode
// in its BeginBuild transaction), so deleting them loses nothing the
// catalog must retain: FR-010's "deletes nothing" clause protects
// FILE records, hashes, and chunk rows - the payload state - and
// image_contents is writer bookkeeping of a placement that is being
// released, exactly like the images rows themselves.
//
// # Which files rows are cleared
//
// Every files row of the SET that carries assignment state - image_id
// NOT NULL or copied != 0 - regardless of status. The planner only
// reads catalog-ACTIVE rows, but 2.4 modification versioning leaves
// old (superseded/missing) rows carrying their historical image_id,
// and "clear all plan assignments" (the command's name and DESIGN_SPEC
// 2.4's own description of this writer) means the whole set, not just
// the active half. The blanket predicate also clears an assignment a
// missing row may still carry - otherwise it could linger referencing
// a deleted destination.
//
// # The scoped unlock transaction (DESIGN_SPEC 4.6, FR-047)
//
// A non-nil imageIDs argument selects the SCOPED form: only the named
// destinations' assignments, marks, and rows are released; every other
// image of the set is provably untouched (every scoped predicate is
// `image_id IN (selected)`; the full-clear predicates stay set-wide).
// The statement ORDER inside the ONE BEGIN IMMEDIATE is load-bearing -
// every statement that READS files.image_id / chunks.image_id runs
// before the statement that NULLs it:
//
//  1. mirror fix: parents of chunks on the selected images lose
//     files.copied (reads chunks.image_id, which only step 4
//     destroys);
//  2. family-claim unmark: copied = 0 on active NON-primary members
//     of families that CLAIM a selected image. The MEMBER side mirrors
//     the hardLinkRows / hardLinkPrimaries derivation line-for-line
//     (active, superseded_by IS NULL, regular, nlink>1, inode!=0,
//     set-scoped); the CLAIM side deliberately DIVERGES from that
//     derivation - ANY row of the family, ANY status (active,
//     superseded, missing), carrying image_id IN (selected) triggers
//     the unmark, with NO MIN(file_id) anchor role (the
//     historical-anchor rule of the 2026-11-03 ruling: bytes live
//     where the ANCHOR's image is, so a superseded anchor's released
//     image still claims the members it wrote). It runs BEFORE step 3
//     so the predicate can still see the rows step 3 would NULL;
//  3. files: image_id = NULL, copied = 0 for rows carrying a selected
//     image_id (ANY status - a superseded or missing row may still
//     carry one; the blanket any-status rule of "Which files rows are
//     cleared" applies to the selection);
//  4. chunks: image_id = NULL, copied = 0, verified_at = NULL - ROWS,
//     HASHES, and the files.chunked/chunk_count/chunk_size stamp
//     retained (FR-010, 4.2 reuse);
//  5. directories: image_id = NULL;
//  6. DELETE the selected images rows (a zero-row or short delete is
//     a LOUD error + rollback - the CLI validates the selection
//     against the planned ids before opening the transaction, so this
//     is defense-in-depth against a race or a direct catalog call);
//  7. DELETE the selected image_contents rows (SCOPED, unlike the
//     full clear's set-wide delete);
//  8. (full clear only) all counts zero -> ROLLBACK, the T2.2 no-op
//     shape; a scoped run never reaches this - step 6 already
//     guarantees at least one removed destination row;
//  9. runs row (kind 'clear') with stats_json, then COMMIT.
//
// PREDICATE DISJOINTNESS (why FilesCleared = sum of RowsAffected is
// an EXACT distinct-row count): the three files-side statements are
// pairwise disjoint by construction. S1 (mirror) requires
// `image_id IS NULL`; S3 (files) requires `image_id IN (selected)`
// (never NULL) -> S1 n S3 = empty. S2 (family) requires
// `(image_id IS NULL OR image_id NOT IN (selected))` -> S2 n S3 =
// empty. S2 additionally excludes every chunk-parent of a chunk on
// the selected images (`file_id NOT IN (chunks on selected)`), which
// is exactly S1's match set (S1 = those parents that also carry
// copied != 0) -> S1 n S2 = empty. Every excluded shape is OWNED by
// one statement: rows carrying a selected image_id -> S3; cleared
// chunk-parents -> S1 (identical copied=0 effect); a non-active row is
// never the row S2 UNMARKS (the active-only member guard, M9), though
// a non-active family row may TRIGGER the unmark (the
// historical-anchor rule); a chunk-parent that is also a family member
// is unreachable in S2's set (a chunked file never carries
// files.image_id, so S1 always owns it) - the round-2 design review
// walked the same three pairs.
//
// The M9 limitation (documented, not a bug): the MEMBER side of the
// family claim is ACTIVE-only (the active-only member guard), so a
// SUPERSEDED member row that carries copied=1 describing the released
// image keeps that mark - the guard names which row S2 unmarks, NOT
// which rows may trigger it, so a superseded ANCHOR carrying
// image_id IN (selected) still triggers the unmark of the active
// members (the historical-anchor rule above). Inert: nothing outside
// the copy pipeline reads copied, and the copy pipeline reads active
// rows only.
//
// UNLOCKED_IMAGES (M6): the runs row's stats_json and ClearResult
// carry `unlocked_images`, a sorted array of image numbers - the
// selected ids when scoped, ALL removed ids for a full clear - in
// BOTH modes (an empty array on a completed nothing-to-clear no-op),
// mirroring the frozen `unlocked_images` key of the CLI's --json
// document.
//
// # Chunk state retained (FR-010 / 4.2 boundary stability)
//
// Chunk ROWS, their stored hashes, and the parent's files.chunked /
// chunk_count / chunk_size stamp are all RETAINED: they are exactly
// what lets a same-effective-size re-plan reuse the decomposition and
// its hashes with ZERO source reads (DESIGN_SPEC 4.2). Only the
// placement state is cleared: chunks.image_id, chunks.copied, and
// chunks.verified_at (a verification stamp describes a placement on
// destination media; with the placement released, a stale
// verified_at would misdescribe the next write - judgment call,
// recorded here).
//
// # Runs bookkeeping and the bit-identical atomicity contract
//
// The runs row (kind 'clear' - the 2.3 CHECK allows it) is inserted
// INSIDE the clear transaction, with its terminal status already
// stamped, and the stats snapshot carrying the cleared counts. This
// deliberately differs from scan's start-'running'-then-FinishRun
// pattern: a clear run has no observable intermediate state (one
// transaction, commit-or-rollback), so inserting the row in-transaction
// buys the exact T2.4 atomicity contract - a FAILED clear rolls back
// to a catalog that is bit-identical, INCLUDING runs (no audit row
// survives for a run that provably changed nothing), while a committed
// clear always carries its audit row. A clear with NOTHING to clear
// rolls its (empty) transaction back too: no run row, zero changes,
// exit 0 - mirroring the T2.2 plan no-op contract.
//
// Error classification mirrors planstore.go/scanstore.go:
// connection-acquisition failure is a KindOpen CatalogError (exit 4);
// individual statement failures inside the open transaction are plain
// wrapped errors.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// ClearResult reports what one ClearPlan run cleared, in row counts -
// the numbers the cli clear-assignments report renders (human and
// --json). RunID is the runs row (kind 'clear') written INSIDE the
// transaction; it is 0 on a nothing-to-clear no-op (no run row is
// written, the transaction was rolled back, the catalog is unchanged).
// UnlockedImages is the sorted image-id array of DESIGN 4.6 (M6): the
// selected ids when scoped, ALL removed ids for a full clear; it is
// non-nil on every return (empty on a completed no-op) so the CLI's
// frozen `unlocked_images` key always encodes as a JSON array.
type ClearResult struct {
	FilesCleared        int     // files rows that carried image_id or copied state
	ChunksCleared       int     // chunks rows that carried image_id/copied/verified state
	DirsCleared         int     // directories rows that carried an image_id
	ImagesRemoved       int     // images rows deleted (see the file doc's decision)
	ContentsCleared     int     // image_contents rows deleted (the S4 closure, see the file doc)
	ChunkHashesRetained int     // chunk rows with a stored hash kept verbatim (FR-010)
	UnlockedImages      []int64 // sorted: selected ids (scoped) or ALL removed ids (full); never nil on success
	RunID               int64   // runs row id (kind 'clear'); 0 = nothing to clear, no run row
	NothingToClear      bool    // no assignment state existed; transaction rolled back, catalog unchanged
}

// clearStats is the runs.stats_json snapshot of one committed clear -
// the audit trail of what the run removed (2.4 provenance). The
// UnlockedImages field is the M6 audit half of the frozen
// `unlocked_images` key: same sorted array, both modes.
type clearStats struct {
	FilesCleared        int     `json:"files_cleared"`
	ChunksCleared       int     `json:"chunks_cleared"`
	DirsCleared         int     `json:"dirs_cleared"`
	ImagesRemoved       int     `json:"images_removed"`
	ContentsCleared     int     `json:"contents_cleared"`
	ChunkHashesRetained int     `json:"chunk_hashes_retained"`
	UnlockedImages      []int64 `json:"unlocked_images"`
}

// normalizeImageSelection returns the selected ids sorted ascending
// and deduplicated. The CLI validates and dedupes before the
// transaction (DESIGN 4.6); this is the catalog-tier guarantee that
// every scoped predicate and the audit array see ONE canonical list
// regardless of caller order or repeats.
func normalizeImageSelection(ids []int64) []int64 {
	sorted := append([]int64(nil), ids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	out := make([]int64, 0, len(sorted))
	for i, id := range sorted {
		if i == 0 || id != sorted[i-1] {
			out = append(out, id)
		}
	}
	return out
}

// readImageIDs reads the set's image ids in ascending order for the
// `unlocked_images` audit array (M6): the FULL clear's removed ids,
// read before the DELETE that removes them. The scoped form derives
// the array from the selection itself (step 6's exact-count check
// proves every selected id existed). The result is never nil (empty
// on a set with no destinations) so the frozen JSON key always
// encodes as an array.
func readImageIDs(ctx context.Context, conn *sql.Conn, setID string) ([]int64, error) {
	rows, err := conn.QueryContext(ctx,
		`SELECT image_id FROM images WHERE set_id = ? ORDER BY image_id`, setID)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing image ids of set %q for the unlock audit: %w", setID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only close; rows.Err dominates
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("catalog: scanning image id of set %q: %w", setID, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: iterating image ids of set %q: %w", setID, err)
	}
	return out, nil
}

// ClearPlan clears plan assignments of setID in ONE transaction
// (DESIGN_SPEC 4.5, FR-010; 4.6 for the scoped form) - the
// implementation of the file doc's contracts. imageIDs selects the
// scope: NIL is the FULL clear - every statement set-wide, exactly the
// pre-4.6 behavior (identical counts, report bytes, and exit class,
// AC-08); a NON-NIL slice is the SCOPED unlock of those image ids -
// the nine-statement sequence of the file doc's "The scoped unlock
// transaction", every predicate `image_id IN (selected)`. The slice
// may carry any order and repeats; it is normalized (sorted,
// deduplicated) here. now is the unix-nanosecond timestamp stamped
// onto the runs row (the caller's clock, the T1.1 injectable-clock
// convention).
//
// The FULL-clear statement sequence inside the BEGIN IMMEDIATE
// transaction:
//
//  1. resolve the set's source root (runs.root provenance; a missing
//     set is an error BEFORE any write);
//  2. read the set's image ids for the `unlocked_images` audit array
//     (before the DELETE that removes them);
//  3. files: image_id = NULL, copied = 0 (every row of the set
//     carrying assignment state - active AND non-active, file doc
//     "Which files rows are cleared");
//  4. chunks: image_id = NULL, copied = 0, verified_at = NULL - ROWS
//     AND HASHES RETAINED (FR-010, 4.2 reuse);
//  5. directories: image_id = NULL;
//  6. images: DELETE every row of the set (the file doc's
//     images-row decision - fresh-plan equivalence against the
//     balancer's locking predicate);
//  7. image_contents: DELETE every row of the set (the file doc's
//     S4 closure - content rows describe placements the clear
//     releases);
//  8. if all five counts are zero: ROLLBACK (nothing to clear - no run
//     row, zero changes, the T2.2 no-op shape);
//  9. otherwise insert the runs row (kind 'clear', status 'ok',
//     stats_json carrying the counts AND `unlocked_images`) and
//     COMMIT.
//
// The SCOPED sequence runs steps 1-7 of the file doc's nine-step
// table with `image_id IN (selected)` predicates (step 8 is
// full-clear-only - a scoped run cannot reach it because step 6
// already guarantees a removed destination row; step 9 is shared),
// documented with the load-bearing order and the predicate-
// disjointness proof in the file doc above.
//
// Any failure at any point rolls the whole thing back: a failed clear
// leaves the catalog bit-identical (FR-010 atomicity, pinned by the
// BEFORE-DELETE trigger tests at both tiers, including the nine-point
// injection matrix over the scoped sequence).
func (db *DB) ClearPlan(ctx context.Context, setID string, imageIDs []int64, now int64) (ClearResult, error) {
	var res ClearResult
	res.UnlockedImages = []int64{} // never nil: the frozen JSON key must encode as []
	scoped := imageIDs != nil
	var selected []int64
	if scoped {
		selected = normalizeImageSelection(imageIDs)
		if len(selected) == 0 {
			// nil means the full clear; a non-nil EMPTY selection is a
			// caller error. Silently promoting it to a full clear would
			// be catastrophic, and a silent no-op would hide the bug.
			return res, fmt.Errorf("catalog: clear of set %q: empty image selection (pass nil imageIDs for the full clear)", setID)
		}
		res.UnlockedImages = append([]int64(nil), selected...)
	}
	conn, err := db.sql.Conn(ctx)
	if err != nil {
		return res, &CatalogError{
			Kind: KindOpen,
			Path: db.path,
			Err:  fmt.Errorf("acquiring connection for clear transaction: %w", err),
		}
	}
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		_ = conn.Close()
		return res, fmt.Errorf("catalog: BEGIN IMMEDIATE for clear of set %q: %w", setID, err)
	}
	committed := false
	defer func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
			_ = conn.Close()
		}
	}()

	// 1. Set resolution and runs.root provenance, before any write.
	var root string
	if err := conn.QueryRowContext(ctx,
		`SELECT source_root FROM sets WHERE set_id = ?`, setID).Scan(&root); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return res, fmt.Errorf("catalog: no set %q in catalog: %w", setID, err)
		}
		return res, fmt.Errorf("catalog: reading source root of set %q: %w", setID, err)
	}

	// count is the set-wide statement helper of the FULL clear (its
	// predicates stay exactly as they were before 4.6).
	count := func(query string, what string) (int, error) {
		r, err := conn.ExecContext(ctx, query, setID)
		if err != nil {
			return 0, fmt.Errorf("catalog: clearing %s of set %q: %w", what, setID, err)
		}
		n, _ := r.RowsAffected()
		return int(n), nil
	}
	// unlock runs one scoped statement with the given argument list
	// (every scoped statement binds `set_id` and/or the selected ids -
	// the IN-list positions differ per statement, so the caller builds
	// the list explicitly in textual parameter order).
	unlock := func(query string, args []any, what string) (int, error) {
		r, err := conn.ExecContext(ctx, query, args...)
		if err != nil {
			return 0, fmt.Errorf("catalog: unlocking %s of set %q: %w", what, setID, err)
		}
		n, _ := r.RowsAffected()
		return int(n), nil
	}
	// sel returns the selected ids as driver args (for the IN-lists).
	sel := func() []any {
		args := make([]any, len(selected))
		for i, id := range selected {
			args[i] = id
		}
		return args
	}

	if scoped {
		// THE NINE-STATEMENT SEQUENCE of DESIGN 4.6 (file doc) - the
		// order is load-bearing: every statement that READS
		// files.image_id / chunks.image_id runs before the statement
		// that NULLs it, and the three files-side predicates are
		// pairwise disjoint so FilesCleared = sum of RowsAffected is an
		// exact distinct-row count (proof in the file doc).
		ph := placeholders(len(selected))

		// 1. mirror fix: parents of chunks on the selected images lose
		// files.copied - reads chunks.image_id, which only step 4
		// destroys.
		if res.FilesCleared, err = unlock(
			`UPDATE files SET copied = 0
			 WHERE set_id = ? AND image_id IS NULL AND copied != 0
			   AND file_id IN (SELECT file_id FROM chunks
			                    WHERE set_id = ? AND image_id IN (`+ph+`))`,
			append([]any{setID, setID}, sel()...), "mirror-fix parents"); err != nil {
			return res, err
		}
		// 2. family-claim unmark: active NON-primary members of
		// families that CLAIM a selected image. The MEMBER side (the
		// `m` row) mirrors the hardLinkRows / hardLinkPrimaries
		// derivation line-for-line: active, superseded_by IS NULL,
		// regular, nlink > 1, inode != 0, set-scoped. The CLAIM side
		// (the `a` row) deliberately DIVERGES from that derivation:
		// ANY family row, ANY status (active, superseded, missing),
		// carrying image_id IN (selected) triggers the unmark - no
		// MIN(file_id) anchor role (the historical-anchor rule of the
		// 2026-11-03 ruling: a superseded anchor's released image still
		// claims the members whose link(2) it wrote). Runs BEFORE step
		// 3 so the predicate still sees the rows step 3 would NULL.
		// The three guards (active/regular member side, image_id NULL
		// or NOT IN selected, NOT a cleared chunk-parent) make this
		// predicate disjoint from steps 1 and 3 (file doc proof).
		// Parameter order follows the SQL text: outer set_id, the NOT
		// IN list, the chunk-parent subquery (set_id + IN list), the
		// family subquery (m.set_id, a.set_id, the anchor IN list) -
		// the SAME count and sequence the GROUP-BY form bound, so
		// familyArgs is byte-for-byte unchanged.
		familyArgs := []any{setID}
		familyArgs = append(familyArgs, sel()...)
		familyArgs = append(familyArgs, setID)
		familyArgs = append(familyArgs, sel()...)
		familyArgs = append(familyArgs, setID, setID)
		familyArgs = append(familyArgs, sel()...)
		familyCleared, err := unlock(
			`UPDATE files SET copied = 0
			 WHERE set_id = ?
			   AND status = 'active' AND superseded_by IS NULL AND entry_kind = 'regular'
			   AND copied != 0
			   AND (image_id IS NULL OR image_id NOT IN (`+ph+`))
			   AND file_id NOT IN (SELECT file_id FROM chunks
			                        WHERE set_id = ? AND image_id IN (`+ph+`))
			   AND file_id IN (
			     SELECT m.file_id
			       FROM files m
			      WHERE m.set_id = ?
			        AND m.status = 'active' AND m.superseded_by IS NULL
			        AND m.entry_kind = 'regular' AND m.nlink > 1 AND m.inode != 0
			        AND EXISTS (
			          SELECT 1 FROM files a
			           WHERE a.set_id = ? AND a.dev = m.dev AND a.inode = m.inode
			             AND a.entry_kind = 'regular' AND a.nlink > 1 AND a.inode != 0
			             AND a.image_id IN (`+ph+`)))`,
			familyArgs, "family-claim members")
		if err != nil {
			return res, err
		}
		res.FilesCleared += familyCleared
		// 3. files: image_id = NULL, copied = 0 for rows carrying a
		// selected image_id - ANY status (a superseded or missing row
		// may still carry one; AC-16).
		filesCleared, err := unlock(
			`UPDATE files SET image_id = NULL, copied = 0
			 WHERE set_id = ? AND image_id IN (`+ph+`)`,
			append([]any{setID}, sel()...), "files assignments")
		if err != nil {
			return res, err
		}
		res.FilesCleared += filesCleared
		// 4. chunks: image_id = NULL, copied = 0, verified_at = NULL -
		// ROWS, HASHES, and the files chunk stamp RETAINED (FR-010,
		// 4.2 reuse).
		if res.ChunksCleared, err = unlock(
			`UPDATE chunks SET image_id = NULL, copied = 0, verified_at = NULL
			 WHERE set_id = ? AND image_id IN (`+ph+`)`,
			append([]any{setID}, sel()...), "chunk placements"); err != nil {
			return res, err
		}
		// 5. directories: image_id = NULL.
		if res.DirsCleared, err = unlock(
			`UPDATE directories SET image_id = NULL
			 WHERE set_id = ? AND image_id IN (`+ph+`)`,
			append([]any{setID}, sel()...), "directory assignments"); err != nil {
			return res, err
		}
		// 6. DELETE the selected images rows. Every selected id must
		// land: the CLI validated the selection against the planned
		// ids before the transaction, so a zero-row (or short) delete
		// means a race or a direct catalog call - LOUD error + the
		// deferred rollback (DESIGN 4.6 step 6).
		if res.ImagesRemoved, err = unlock(
			`DELETE FROM images WHERE set_id = ? AND image_id IN (`+ph+`)`,
			append([]any{setID}, sel()...), "destinations"); err != nil {
			return res, err
		}
		if res.ImagesRemoved != len(selected) {
			return res, fmt.Errorf(
				"catalog: clear of set %q: unlocking image(s) %v deleted %d destination rows, want %d "+
					"(every selected image must be a planned destination; zero-row delete is a loud error, DESIGN_SPEC 4.6 step 6)",
				setID, selected, res.ImagesRemoved, len(selected))
		}
		// 7. DELETE the selected image_contents rows - SCOPED, unlike
		// the full clear's set-wide delete (the S4 closure scoped to
		// the released placements).
		if res.ContentsCleared, err = unlock(
			`DELETE FROM image_contents WHERE set_id = ? AND image_id IN (`+ph+`)`,
			append([]any{setID}, sel()...), "image contents"); err != nil {
			return res, err
		}
	} else {
		// FULL clear: the pre-4.6 set-wide statements, byte-for-byte.
		// 2. The unlocked-images audit array, read before the DELETE
		// that removes the rows (step 6 below).
		if res.UnlockedImages, err = readImageIDs(ctx, conn, setID); err != nil {
			return res, err
		}

		// 3-5. The NULLing updates (set-scoped - see the file doc).
		if res.FilesCleared, err = count(
			`UPDATE files SET image_id = NULL, copied = 0
			 WHERE set_id = ? AND (image_id IS NOT NULL OR copied != 0)`, "files assignments"); err != nil {
			return res, err
		}
		if res.ChunksCleared, err = count(
			`UPDATE chunks SET image_id = NULL, copied = 0, verified_at = NULL
			 WHERE set_id = ? AND (image_id IS NOT NULL OR copied != 0 OR verified_at IS NOT NULL)`,
			"chunk placements"); err != nil {
			return res, err
		}
		if res.DirsCleared, err = count(
			`UPDATE directories SET image_id = NULL WHERE set_id = ? AND image_id IS NOT NULL`,
			"directory assignments"); err != nil {
			return res, err
		}

		// 6. The images-row decision (file doc): delete them all - no
		// assignment references them anymore (steps 3-5 are set-scoped),
		// and a surviving row would keep its id IN USE for the
		// balancer's lowest-unused minting (4.6), so the re-plan's ids
		// would differ from the fresh sequence (locking predicate,
		// internal/plan).
		if res.ImagesRemoved, err = count(
			`DELETE FROM images WHERE set_id = ?`, "destinations"); err != nil {
			return res, err
		}

		// 7. The S4 closure (file doc): delete the set's image_contents
		// rows in the SAME transaction - they describe placements this
		// clear releases, and stale rows under old (set, image, path) keys
		// would survive into a re-plan with a different image layout.
		if res.ContentsCleared, err = count(
			`DELETE FROM image_contents WHERE set_id = ?`, "image contents"); err != nil {
			return res, err
		}
	}

	// FR-010 evidence for the report: how many stored chunk hashes
	// survived verbatim (the 4.2 reuse budget). Set-wide in BOTH modes:
	// neither scope deletes a chunk row or a hash.
	if err := conn.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chunks WHERE set_id = ? AND hash IS NOT NULL`, setID).
		Scan(&res.ChunkHashesRetained); err != nil {
		return res, fmt.Errorf("catalog: counting retained chunk hashes for set %q: %w", setID, err)
	}

	// 8. (FULL CLEAR ONLY, DESIGN 4.6 step 8) Nothing to clear: roll
	// the empty transaction back - no run row, zero changes (the T2.2
	// no-op shape). A scoped run never reaches this check: step 6
	// already guarantees ImagesRemoved == len(selected) >= 1, so its
	// counts are never all-zero.
	if !scoped && res.FilesCleared == 0 && res.ChunksCleared == 0 &&
		res.DirsCleared == 0 && res.ImagesRemoved == 0 && res.ContentsCleared == 0 {
		res.NothingToClear = true
		return res, nil
	}

	// 9. Runs bookkeeping inside the transaction (file doc): kind
	// 'clear' (allowed by the 2.3 CHECK), status terminal by commit.
	// stats_json carries the counts AND the M6 `unlocked_images` array
	// (sorted; the selected ids when scoped, ALL removed ids when full).
	stats, merr := json.Marshal(clearStats{
		FilesCleared:        res.FilesCleared,
		ChunksCleared:       res.ChunksCleared,
		DirsCleared:         res.DirsCleared,
		ImagesRemoved:       res.ImagesRemoved,
		ContentsCleared:     res.ContentsCleared,
		ChunkHashesRetained: res.ChunkHashesRetained,
		UnlockedImages:      res.UnlockedImages,
	})
	if merr != nil {
		stats = []byte("{}")
	}
	r, err := conn.ExecContext(ctx,
		`INSERT INTO runs (set_id, kind, root, started_at, finished_at, status,
		                  exclude_json, stats_json, tool_version)
		 VALUES (?, 'clear', ?, ?, ?, 'ok', '[]', ?, ?)`,
		setID, root, now, now, string(stats), ToolVersion)
	if err != nil {
		return res, fmt.Errorf("catalog: recording clear run for set %q: %w", setID, err)
	}
	if res.RunID, err = r.LastInsertId(); err != nil {
		return res, fmt.Errorf("catalog: clear run id after insert: %w", err)
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return res, fmt.Errorf("catalog: committing clear transaction for set %q: %w", setID, err)
	}
	committed = true
	if err := conn.Close(); err != nil {
		return res, fmt.Errorf("catalog: closing clear transaction connection: %w", err)
	}
	return res, nil
}
