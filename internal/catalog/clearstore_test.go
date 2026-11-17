package catalog

// T2.4: ClearPlan integration tests against a REAL temp catalog
// (DESIGN_SPEC 12.1 catalog-SQL tier). They pin the clearstore.go
// contracts at the SQL layer: the one-transaction clearing of files /
// chunks / directories assignment state, the images-row decision
// (deleted - fresh-plan equivalence against the balancer's locking
// predicate), the FR-010 retention (rows, hashes, and the
// files.chunk_size stamp survive verbatim), the runs row (kind 'clear'
// - the 2.3 CHECK allows it, asserted here), the non-active-row
// coverage (2.4 versioning leaves old rows carrying assignments), the
// nothing-to-clear rollback (no run row, zero changes), unknown-set
// refusal, and the trigger-forced mid-transaction failure leaving the
// catalog bit-identical (the T1.6 QA trigger technique reused).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// --- fixture helpers ---------------------------------------------------------

// newClearFixture builds the full assignment state ClearPlan consumes:
// two active regular files - one assigned WHOLE with its copied mark
// (simulating a completed write), one CHUNKED (two chunk rows with
// stored hashes, copied, and verified_at - simulating a written and
// verified chunk set) - an assigned empty directory, a MISSING row
// carrying a historical assignment (2.4 versioning), and two images
// rows. It returns the fixture, the chunked file id, and the seeded
// per-index chunk hashes.
func newClearFixture(t *testing.T) (f *scanFixture, big int64, hashes map[int][]byte) {
	t.Helper()
	f = newScanFixture(t)
	ctx := context.Background()

	whole := insertPlanFile(t, f, "a.txt", 100)
	big = insertPlanFile(t, f, "big.dat", 4*gib)

	// A MISSING row that will carry a historical assignment (2.4: old
	// rows keep their image_id until clear releases it).
	var missingID int64
	f.mustTx(t, func(tx *ScanTx) {
		var err error
		missingID, err = tx.InsertScanFile(ctx, regularRow("gone.txt", 50, 0x99))
		if err != nil {
			t.Fatalf("InsertScanFile(gone.txt): %v", err)
		}
	})
	sdb := rawOpen(t, f.db.Path())
	if _, err := sdb.Exec(`UPDATE files SET status = 'missing' WHERE file_id = ?`, missingID); err != nil {
		t.Fatalf("stamping gone.txt missing: %v", err)
	}

	specs := []ChunkSpec{{Index: 0, Offset: 0, Length: 2 * gib}, {Index: 1, Offset: 2 * gib, Length: 2 * gib}}
	hashes = map[int][]byte{0: bytes.Repeat([]byte{0x11}, 32), 1: bytes.Repeat([]byte{0x22}, 32)}
	planTxCommit(t, f, func(tx *PlanTx) {
		for _, id := range []int64{1, 2} {
			if err := tx.InsertImage(ctx, ImageSpec{
				ImageID: id, NominalSize: 25_000_000_000, UsableSize: 23_000_000_000,
				MediaProfile: "bd-r-sl",
			}); err != nil {
				t.Fatalf("InsertImage(%d): %v", id, err)
			}
		}
		if err := tx.AssignFile(ctx, whole, 2); err != nil {
			t.Fatalf("AssignFile(a.txt): %v", err)
		}
		if err := tx.AssignFile(ctx, missingID, 2); err != nil {
			t.Fatalf("AssignFile(gone.txt): %v", err)
		}
		if err := tx.EnsureChunkRows(ctx, big, specs); err != nil {
			t.Fatalf("EnsureChunkRows: %v", err)
		}
		if err := tx.StoreChunkSize(ctx, big, 2*gib, len(specs)); err != nil {
			t.Fatalf("StoreChunkSize: %v", err)
		}
		if err := tx.AssignChunk(ctx, big, 0, 1); err != nil {
			t.Fatalf("AssignChunk(0): %v", err)
		}
		if err := tx.AssignChunk(ctx, big, 1, 2); err != nil {
			t.Fatalf("AssignChunk(1): %v", err)
		}
	})
	dirID := insertEmptyDir(t, f, "emptydir")
	planTxCommit(t, f, func(tx *PlanTx) {
		if err := tx.AssignDir(ctx, dirID, 1); err != nil {
			t.Fatalf("AssignDir: %v", err)
		}
	})

	// Seed the written+verified chunk state (simulating T3 first-write
	// hashing and verify-media stamping - those sprints do not exist
	// yet, and clear must clear exactly this state).
	rows, err := sdb.Query(`SELECT chunk_id, chunk_index FROM chunks WHERE set_id = ? ORDER BY chunk_index`, f.setID)
	if err != nil {
		t.Fatalf("query chunk ids: %v", err)
	}
	type pair struct{ id, idx int64 }
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.id, &p.idx); err != nil {
			t.Fatalf("scan chunk id: %v", err)
		}
		pairs = append(pairs, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate chunk ids: %v", err)
	}
	rows.Close()
	for _, p := range pairs {
		if _, err := sdb.Exec(`UPDATE chunks SET hash = ?, copied = 1, verified_at = 111 WHERE chunk_id = ?`,
			hashes[int(p.idx)], p.id); err != nil {
			t.Fatalf("seeding chunk %d written state: %v", p.id, err)
		}
	}
	// a.txt was written too (whole-file copied mark).
	if _, err := sdb.Exec(`UPDATE files SET copied = 1 WHERE file_id = ?`, whole); err != nil {
		t.Fatalf("seeding a.txt copied: %v", err)
	}
	return f, big, hashes
}

// dumpClearState reads the FULL assignment state of the catalog -
// files, chunks, directories, images - into one deterministic string
// for bit-identical comparisons (the atomicity assertions).
func dumpClearState(t *testing.T, db *DB) string {
	t.Helper()
	sdb := rawOpen(t, db.Path())
	var buf bytes.Buffer
	dump := func(query, what string) {
		t.Helper()
		rows, err := sdb.Query(query)
		if err != nil {
			t.Fatalf("dump %s: %v", what, err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns of %s: %v", what, err)
		}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("scan %s row: %v", what, err)
			}
			for i, v := range vals {
				switch tv := v.(type) {
				case []byte:
					fmt.Fprintf(&buf, "%x", tv)
				case nil:
					buf.WriteString("NULL")
				default:
					buf.WriteString(strings.TrimSpace(toString(tv)))
				}
				if i < len(vals)-1 {
					buf.WriteString("|")
				}
			}
			buf.WriteString("\n")
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate %s: %v", what, err)
		}
	}
	dump(`SELECT file_id, relpath, status, image_id, copied, chunked, chunk_count, chunk_size
	      FROM files ORDER BY file_id`, "files")
	dump(`SELECT chunk_id, file_id, chunk_index, offset, length, hash, image_id, copied, verified_at
	      FROM chunks ORDER BY chunk_id`, "chunks")
	dump(`SELECT dir_id, relpath, status, image_id FROM directories ORDER BY dir_id`, "directories")
	dump(`SELECT image_id, nominal_size, usable_size, media_profile, state
	      FROM images ORDER BY image_id`, "images")
	return buf.String()
}

// toString renders a driver value for the dump (test-only).
func toString(v any) string { return fmt.Sprintf("%v", v) }

// --- the clearing contract ---------------------------------------------------

// TestClearPlanClearsAssignmentsAndRetainsHashes is the catalog-tier
// centerpiece: after ClearPlan every assignment is NULL, copied marks
// are 0, chunk ROWS with their stored hashes survive verbatim (FR-010)
// along with the files.chunk_size stamp (4.2 reuse), the images rows
// are GONE (the documented fresh-plan-equivalence decision), the
// non-active MISSING row's historical assignment is released too, and
// exactly one runs row (kind 'clear', allowed by the 2.3 CHECK) records
// the counts.
func TestClearPlanClearsAssignmentsAndRetainsHashes(t *testing.T) {
	f, big, hashes := newClearFixture(t)
	ctx := context.Background()

	res, err := f.db.ClearPlan(ctx, f.setID, nil, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClearPlan: %v", err)
	}
	// a.txt (copied+assigned), big.dat is NOT counted on the files side
	// (chunked files carry files.image_id NULL), gone.txt (assigned).
	if res.FilesCleared != 2 || res.ChunksCleared != 2 || res.DirsCleared != 1 ||
		res.ImagesRemoved != 2 || res.ChunkHashesRetained != 2 || res.NothingToClear || res.RunID == 0 {
		t.Errorf("ClearPlan result = %+v, want files 2, chunks 2, dirs 1, images 2, retained 2, run id set",
			res)
	}

	sdb := rawOpen(t, f.db.Path())
	// Files: every row unassigned and un-copied, active AND missing.
	var fileState int
	if err := sdb.QueryRow(`SELECT COUNT(*) FROM files
	                       WHERE set_id = ? AND (image_id IS NOT NULL OR copied != 0)`, f.setID).
		Scan(&fileState); err != nil {
		t.Fatalf("count residual file assignment state: %v", err)
	}
	if fileState != 0 {
		t.Errorf("%d files rows still carry assignment/copied state after clear", fileState)
	}
	// Chunks: rows RETAINED with hashes byte-identical (FR-010), only
	// the placement state cleared.
	rows := rawChunkRows(t, f.db, big)
	if len(rows) != 2 {
		t.Fatalf("chunk rows after clear = %d, want 2 RETAINED (FR-010)", len(rows))
	}
	for i, r := range rows {
		if !bytes.Equal(r.Hash, hashes[i]) {
			t.Errorf("chunk %d hash after clear = %x, want the stored %x (FR-010 retention)", i, r.Hash, hashes[i])
		}
		if r.ImageID.Valid || r.Copied || r.VerifiedAt.Valid {
			t.Errorf("chunk %d placement state after clear = %+v, want image/copied/verified cleared", i, r)
		}
		if r.Length != 2*gib {
			t.Errorf("chunk %d length = %d, want %d (rows retained verbatim)", i, r.Length, 2*gib)
		}
	}
	// The 4.2 reuse stamp survives.
	var chunked, chunkCount int
	var chunkSize int64
	if err := sdb.QueryRow(`SELECT chunked, chunk_count, chunk_size FROM files WHERE file_id = ?`, big).
		Scan(&chunked, &chunkCount, &chunkSize); err != nil {
		t.Fatalf("read chunk stamp after clear: %v", err)
	}
	if chunked != 1 || chunkCount != 2 || chunkSize != 2*gib {
		t.Errorf("chunk stamp after clear = %d/%d/%d, want 1/2/%d (retained for reuse)",
			chunked, chunkCount, chunkSize, 2*gib)
	}
	// Directories: unassigned.
	var assignedDirs int
	if err := sdb.QueryRow(`SELECT COUNT(*) FROM directories WHERE set_id = ? AND image_id IS NOT NULL`, f.setID).
		Scan(&assignedDirs); err != nil {
		t.Fatalf("count assigned dirs after clear: %v", err)
	}
	if assignedDirs != 0 {
		t.Errorf("%d directories still assigned after clear", assignedDirs)
	}
	// Images: deleted (the documented decision).
	var images int
	if err := sdb.QueryRow(`SELECT COUNT(*) FROM images WHERE set_id = ?`, f.setID).Scan(&images); err != nil {
		t.Fatalf("count images after clear: %v", err)
	}
	if images != 0 {
		t.Errorf("images rows after clear = %d, want 0 (fresh-plan equivalence)", images)
	}
	// Runs: exactly one 'clear' row, 'ok', with the stats snapshot -
	// and the 2.3 CHECK is what admitted kind 'clear' (this insert
	// would have failed had the CHECK disallowed it).
	var kind, status, root, stats string
	if err := sdb.QueryRow(`SELECT kind, status, root, stats_json FROM runs WHERE run_id = ?`, res.RunID).
		Scan(&kind, &status, &root, &stats); err != nil {
		t.Fatalf("read clear run row: %v", err)
	}
	if kind != "clear" || status != "ok" || root != "/data" {
		t.Errorf("clear run row = kind %q status %q root %q, want clear/ok//data", kind, status, root)
	}
	var cs clearStats
	if err := json.Unmarshal([]byte(stats), &cs); err != nil {
		t.Fatalf("stats_json %q: %v", stats, err)
	}
	if cs.FilesCleared != 2 || cs.ChunksCleared != 2 || cs.DirsCleared != 1 ||
		cs.ImagesRemoved != 2 || cs.ChunkHashesRetained != 2 {
		t.Errorf("stats_json = %+v, want the cleared counts", cs)
	}
}

// TestClearPlanNothingToClearIsNoop: a set with no assignment state -
// the fresh-scan case - clears nothing, writes NO run row, and leaves
// the catalog untouched (the T2.2 plan no-op shape).
func TestClearPlanNothingToClearIsNoop(t *testing.T) {
	f := newScanFixture(t)
	insertPlanFile(t, f, "a.txt", 10)

	before := dumpClearState(t, f.db)
	res, err := f.db.ClearPlan(context.Background(), f.setID, nil, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClearPlan (nothing to clear): %v", err)
	}
	if !res.NothingToClear || res.RunID != 0 ||
		res.FilesCleared != 0 || res.ChunksCleared != 0 || res.DirsCleared != 0 || res.ImagesRemoved != 0 {
		t.Errorf("ClearPlan result = %+v, want the nothing-to-clear no-op (RunID 0)", res)
	}
	if got := dumpClearState(t, f.db); got != before {
		t.Errorf("catalog changed across a nothing-to-clear run:\nbefore:\n%s\nafter:\n%s", before, got)
	}
	// Only the fixture's ingest run exists - the clear recorded none.
	sdb := rawOpen(t, f.db.Path())
	var clearRuns int
	if err := sdb.QueryRow(`SELECT COUNT(*) FROM runs WHERE kind = 'clear'`).Scan(&clearRuns); err != nil {
		t.Fatalf("count clear runs: %v", err)
	}
	if clearRuns != 0 {
		t.Errorf("clear run rows = %d after a no-op, want 0", clearRuns)
	}
}

// TestClearPlanTriggerFailureIsAtomic: a BEFORE DELETE trigger on
// images aborts the transaction AFTER the NULLing updates ran (the
// latest failure point available to SQL) - the catalog must be
// bit-identical (files, chunks, directories, images AND runs) and a
// clean re-run after dropping the trigger must fully succeed (no
// latent damage).
func TestClearPlanTriggerFailureIsAtomic(t *testing.T) {
	f, _, _ := newClearFixture(t)
	ctx := context.Background()

	before := dumpClearState(t, f.db)
	sdb := rawOpen(t, f.db.Path())
	if _, err := sdb.Exec(`CREATE TRIGGER inject_clear_failure BEFORE DELETE ON images
		BEGIN SELECT RAISE(ABORT, 'injected clear failure'); END`); err != nil {
		t.Fatalf("install clear-failure trigger: %v", err)
	}

	_, err := f.db.ClearPlan(ctx, f.setID, nil, time.Now().UnixNano())
	if err == nil || !strings.Contains(err.Error(), "injected clear failure") {
		t.Fatalf("ClearPlan under trigger = %v, want the injected failure", err)
	}
	if got := dumpClearState(t, f.db); got != before {
		t.Errorf("failed clear changed the catalog (atomicity violation):\nbefore:\n%s\nafter:\n%s", before, got)
	}
	var clearRuns int
	if err := sdb.QueryRow(`SELECT COUNT(*) FROM runs WHERE kind = 'clear'`).Scan(&clearRuns); err != nil {
		t.Fatalf("count clear runs: %v", err)
	}
	if clearRuns != 0 {
		t.Errorf("failed clear left %d run rows; the in-transaction insert must roll back with everything else", clearRuns)
	}

	// Recovery: drop the trigger, clear fully succeeds.
	if _, err := sdb.Exec(`DROP TRIGGER inject_clear_failure`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	res, err := f.db.ClearPlan(ctx, f.setID, nil, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClearPlan after dropping the trigger: %v", err)
	}
	if res.FilesCleared != 2 || res.ChunksCleared != 2 || res.DirsCleared != 1 || res.ImagesRemoved != 2 {
		t.Errorf("recovery ClearPlan result = %+v, want the full clear", res)
	}
}

// TestClearPlanUnknownSet: clearing a set that does not exist is an
// error naming the set (the CLI gates it to exit 2 via PriorScanRuns
// before this layer sees it; here we pin the SQL-layer refusal).
func TestClearPlanUnknownSet(t *testing.T) {
	f := newScanFixture(t)
	_, err := f.db.ClearPlan(context.Background(), "S-NOPE", nil, time.Now().UnixNano())
	if err == nil || !strings.Contains(err.Error(), "S-NOPE") {
		t.Fatalf("ClearPlan(unknown set) = %v, want an error naming the set", err)
	}
	var runs int
	sdb := rawOpen(t, f.db.Path())
	if err := sdb.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 1 {
		t.Errorf("runs rows = %d after a refused clear, want 1 (the fixture's ingest run)", runs)
	}
}

// TestClearPlanErrorIsPlainWrapped: the statement-level failure
// surfaces as a plain wrapped error (exit 1 class), NOT a CatalogError
// - the established classification of this layer (10.1 scopes
// CatalogError to open/corrupt/schema/migration).
func TestClearPlanErrorIsPlainWrapped(t *testing.T) {
	f, _, _ := newClearFixture(t)
	sdb := rawOpen(t, f.db.Path())
	if _, err := sdb.Exec(`CREATE TRIGGER inject_clear_failure BEFORE DELETE ON images
		BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatalf("install trigger: %v", err)
	}
	_, err := f.db.ClearPlan(context.Background(), f.setID, nil, time.Now().UnixNano())
	var cerr *CatalogError
	if errors.As(err, &cerr) {
		t.Fatalf("ClearPlan statement failure classified as CatalogError (%v); want a plain wrapped error (exit 1)", err)
	}
	// The catalog file path stays usable: raw introspection still works.
	var n int
	if err := sdb.QueryRow(`SELECT COUNT(*) FROM sqlite_master`).Scan(&n); err != nil {
		t.Fatalf("catalog unusable after failed clear: %v", err)
	}
}
