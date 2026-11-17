package catalog

// T2.4 sprint-closing gap tests (verification pass). They close the
// audit gaps the original clearstore_test.go left open, at the
// catalog-SQL tier (DESIGN_SPEC 12.1):
//
//   - the EXPLICIT non-active-row release: a MISSING files row's
//     historical assignment (2.4 modification versioning) is NULLed
//     while the record itself - status, stored file hash, provenance -
//     survives verbatim (FR-010: clearing deletes no file records and
//     no hashes). The original centerpiece covered this only through
//     an aggregate COUNT(*) = 0; the per-row contract is pinned here.
//
//   - SET SCOPING: ClearPlan of one set must leave every assignment,
//     destination row, stored hash, and run row of ANOTHER set
//     bit-identical. The three NULLing updates and the images DELETE
//     are all `WHERE set_id = ?` - that guard is load-bearing and was
//     previously unpinned (a dropped WHERE would clear the world and
//     no single-set test would notice).
//
//   - the image_contents S4 CLOSURE (FR-020 / DESIGN_SPEC 6.6): the
//     brief's orphan audit, CLOSED by T4.3. The counterfactual that
//     pinned the old S2 boundary (rows surviving the clear) is
//     replaced by the closure test: content rows are deleted in the
//     same transaction as the images rows, and a mid-transaction
//     trigger failure rolls everything back bit-identical (the FR-010
//     atomicity extended to the new statement).

import (
	"bytes"
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// TestClearPlanReleasesMissingRowAndRetainsRecord: the fixture's
// gone.txt is status='missing' carrying a historical image_id=2 (2.4:
// old rows keep their assignment until clear releases it). After
// ClearPlan the assignment is gone BUT the record itself is retained
// verbatim - status still 'missing' (2.4 retention philosophy: mark,
// never delete), the 32-byte content hash still present (FR-010:
// hashes are payload state, not placement state), copied 0.
func TestClearPlanReleasesMissingRowAndRetainsRecord(t *testing.T) {
	f, _, _ := newClearFixture(t)
	ctx := context.Background()
	sdb := rawOpen(t, f.db.Path())

	// Fixture sanity: the missing row really carries an assignment.
	var preImage sql.NullInt64
	if err := sdb.QueryRow(`SELECT image_id FROM files
	                        WHERE set_id = ? AND relpath = 'gone.txt'`, f.setID).
		Scan(&preImage); err != nil {
		t.Fatalf("read pre-clear missing row: %v", err)
	}
	if !preImage.Valid || preImage.Int64 != 2 {
		t.Fatalf("fixture gone.txt image_id = %v, want a historical assignment on image 2", preImage)
	}

	res, err := f.db.ClearPlan(ctx, f.setID, nil, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClearPlan: %v", err)
	}
	// a.txt (active, assigned) + gone.txt (missing, assigned). big.dat
	// is chunked: its files.image_id is NULL by construction, its state
	// lives on the chunk rows.
	if res.FilesCleared != 2 {
		t.Fatalf("FilesCleared = %d, want 2 (a.txt AND the missing gone.txt)", res.FilesCleared)
	}

	var (
		status  string
		imageID sql.NullInt64
		copied  int
		hash    []byte
	)
	if err := sdb.QueryRow(`SELECT status, image_id, copied, hash FROM files
	                        WHERE set_id = ? AND relpath = 'gone.txt'`, f.setID).
		Scan(&status, &imageID, &copied, &hash); err != nil {
		t.Fatalf("read post-clear missing row: %v", err)
	}
	if imageID.Valid || copied != 0 {
		t.Errorf("missing row after clear = image_id %v copied %d, want NULL/0 (historical assignment released)",
			imageID, copied)
	}
	if status != "missing" {
		t.Errorf("missing row status after clear = %q, want %q (clear changes placement, never lineage status)",
			status, "missing")
	}
	if len(hash) != 32 {
		t.Errorf("missing row hash after clear = %d bytes, want the 32-byte record hash RETAINED (FR-010)", len(hash))
	}
}

// TestClearPlanIsSetScoped: two sets in ONE catalog, both fully
// assigned. Clearing S-001 must leave S-002's files assignment, its
// images row, its stored chunk state, and its run rows bit-identical -
// and the reported counts must describe ONLY the cleared set.
func TestClearPlanIsSetScoped(t *testing.T) {
	f, _, _ := newClearFixture(t) // S-001: fully assigned (see the fixture doc)
	ctx := context.Background()

	// A second, independently assigned set in the same catalog.
	if err := f.db.EnsureSet(ctx, "S-OTHER", "/other", time.Now().UnixNano(), ToolVersion); err != nil {
		t.Fatalf("EnsureSet(S-OTHER): %v", err)
	}
	runB, err := f.db.StartScanRun(ctx, ScanRun{
		SetID: "S-OTHER", Kind: "ingest", Root: "/other",
		StartedAt: time.Now().UnixNano(), ExcludeJSON: "[]",
	})
	if err != nil {
		t.Fatalf("StartScanRun(S-OTHER): %v", err)
	}
	var bFile int64
	stx, err := f.db.BeginScanTx(ctx, runB, "S-OTHER")
	if err != nil {
		t.Fatalf("BeginScanTx(S-OTHER): %v", err)
	}
	if bFile, err = stx.InsertScanFile(ctx, regularRow("b.txt", 42, 0x77)); err != nil {
		t.Fatalf("InsertScanFile(b.txt): %v", err)
	}
	if err := stx.Commit(); err != nil {
		t.Fatalf("commit S-OTHER scan tx: %v", err)
	}
	ptx, err := f.db.BeginPlanTx(ctx, runB, "S-OTHER")
	if err != nil {
		t.Fatalf("BeginPlanTx(S-OTHER): %v", err)
	}
	if err := ptx.InsertImage(ctx, ImageSpec{
		ImageID: 7, NominalSize: 25_000_000_000, UsableSize: 23_000_000_000,
		MediaProfile: "bd-r-sl",
	}); err != nil {
		t.Fatalf("InsertImage(S-OTHER, 7): %v", err)
	}
	if err := ptx.AssignFile(ctx, bFile, 7); err != nil {
		t.Fatalf("AssignFile(b.txt, 7): %v", err)
	}
	if err := ptx.Commit(); err != nil {
		t.Fatalf("commit S-OTHER plan tx: %v", err)
	}

	before := dumpOtherSetState(t, f.db, "S-OTHER")
	res, err := f.db.ClearPlan(ctx, f.setID, nil, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClearPlan(S-001): %v", err)
	}
	// The counts describe S-001 ONLY: S-OTHER's file + image row are
	// invisible to them.
	if res.FilesCleared != 2 || res.ChunksCleared != 2 || res.DirsCleared != 1 ||
		res.ImagesRemoved != 2 || res.ChunkHashesRetained != 2 {
		t.Errorf("ClearPlan(S-001) result = %+v, want the S-001-only counts 2/2/1/2/2 (S-OTHER must not leak in)", res)
	}
	if after := dumpOtherSetState(t, f.db, "S-OTHER"); after != before {
		t.Errorf("clearing S-001 changed S-002 state (set-scoping violation):\nbefore:\n%s\nafter:\n%s", before, after)
	}
	// And S-001 itself was actually cleared (the scoping did not mute
	// the clear).
	sdb := rawOpen(t, f.db.Path())
	var s001Images int
	if err := sdb.QueryRow(`SELECT COUNT(*) FROM images WHERE set_id = 'S-001'`).Scan(&s001Images); err != nil {
		t.Fatalf("count S-001 images: %v", err)
	}
	if s001Images != 0 {
		t.Errorf("S-001 images rows after clear = %d, want 0", s001Images)
	}
}

// dumpOtherSetState reads the full S-OTHER-scoped state (files,
// chunks, images, runs) into one deterministic string for the
// bit-identical set-scoping comparison.
func dumpOtherSetState(t *testing.T, db *DB, setID string) string {
	t.Helper()
	sdb := rawOpen(t, db.Path())
	var buf bytes.Buffer
	dump := func(query, what string) {
		t.Helper()
		rows, err := sdb.Query(query, setID)
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
					buf.WriteString(toString(tv))
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
			t.Fatalf("iterate %s rows: %v", what, err)
		}
	}
	dump(`SELECT file_id, relpath, status, image_id, copied, hash
	      FROM files WHERE set_id = ? ORDER BY file_id`, "files")
	dump(`SELECT chunk_id, file_id, chunk_index, image_id, copied, hash
	      FROM chunks WHERE set_id = ? ORDER BY chunk_id`, "chunks")
	dump(`SELECT image_id, nominal_size, usable_size, media_profile, state
	      FROM images WHERE set_id = ? ORDER BY image_id`, "images")
	dump(`SELECT run_id, kind, status, root FROM runs WHERE set_id = ? ORDER BY run_id`, "runs")
	return buf.String()
}

// TestClearPlanDeletesImageContentsInSameTransaction is THE S4 CLOSURE
// of the S2 boundary audit (the T2.4 forward caveat, closed by T4.3):
// image_contents rows - the FR-020 finalized per-image content list,
// written by the S4 builder in its state-'building' transaction (6.6)
// - are DELETED by clear in the SAME transaction that deletes the
// images rows, not left behind as orphans of a released placement.
//
// The two halves pin the contract at both levels:
//
//   - the DELETE half: seeded content rows (the S3 copy shape: file,
//     chunk, manifest, export kinds across two images) are gone after
//     a successful clear, the images rows are gone, and the
//     ClearResult counts the deletion;
//   - the ATOMICITY half: a BEFORE-DELETE trigger trips the clear
//     mid-transaction, and the whole thing rolls back bit-identical -
//     content rows, images rows, assignments, everything (FR-010's
//     atomicity contract extended to the new statement).
func TestClearPlanDeletesImageContentsInSameTransaction(t *testing.T) {
	f, big, hashes := newClearFixture(t)
	ctx := context.Background()
	sdb := rawOpen(t, f.db.Path())

	// Seed the S3/S4 write shape: content rows across BOTH images -
	// payload rows referencing existing files rows (FK-clean) plus the
	// two self-rows the writers record (6.6).
	var whole int64
	if err := sdb.QueryRow(`SELECT file_id FROM files WHERE set_id = ? AND relpath = 'a.txt'`, f.setID).
		Scan(&whole); err != nil {
		t.Fatalf("reading a.txt file_id: %v", err)
	}
	seed := []struct {
		image    int64
		path     string
		kind     string
		fileID   any // nil = NULL
		chunkIdx any
		size     int64
		hash     []byte
	}{
		{1, "chunks/big.dat/00001", "chunk", big, 0, 2 * gib, hashes[0]},
		{2, "files/a.txt", "file", whole, nil, 100, bytes.Repeat([]byte{0x33}, 32)},
		{1, "MANIFEST.json", "manifest", nil, nil, 4096, bytes.Repeat([]byte{0x44}, 32)},
		{1, "catalog-export.sqlite", "export", nil, nil, 8192, bytes.Repeat([]byte{0x55}, 32)},
	}
	for _, r := range seed {
		if _, err := sdb.Exec(`INSERT INTO image_contents
		        (set_id, image_id, media_path, kind, file_id, chunk_index, size, hash)
		        VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			f.setID, r.image, r.path, r.kind, r.fileID, r.chunkIdx, r.size, r.hash); err != nil {
			t.Fatalf("seeding image_contents row %q: %v", r.path, err)
		}
	}

	// THE ATOMICITY HALF FIRST: a BEFORE-DELETE trigger trips the new
	// statement mid-transaction; the whole clear rolls back
	// bit-identical (dumpClearState covers files/chunks/dirs/images;
	// the content rows are asserted separately because they are not in
	// that dump).
	if _, err := sdb.Exec(`CREATE TRIGGER test_block_contents BEFORE DELETE ON image_contents
	        BEGIN SELECT RAISE(ABORT, 'injected contents-delete failure'); END`); err != nil {
		t.Fatalf("installing contents-delete trigger: %v", err)
	}
	before := dumpClearState(t, f.db)
	if _, err := f.db.ClearPlan(ctx, f.setID, nil, time.Now().UnixNano()); err == nil {
		t.Fatal("ClearPlan must fail when the image_contents delete fails")
	}
	if _, err := sdb.Exec(`DROP TRIGGER test_block_contents`); err != nil {
		t.Fatalf("dropping contents-delete trigger: %v", err)
	}
	if after := dumpClearState(t, f.db); after != before {
		t.Errorf("failed clear changed state (atomicity violated):\nbefore:\n%s\nafter:\n%s", before, after)
	}
	var contents int
	if err := sdb.QueryRow(`SELECT COUNT(*) FROM image_contents WHERE set_id = ?`, f.setID).Scan(&contents); err != nil {
		t.Fatalf("counting image_contents after failed clear: %v", err)
	}
	if contents != len(seed) {
		t.Errorf("image_contents rows after the failed clear = %d, want all %d seeded rows (rollback)", contents, len(seed))
	}

	// The DELETE HALF: the clean clear removes every content row of the
	// set together with the images rows.
	res, err := f.db.ClearPlan(ctx, f.setID, nil, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClearPlan: %v", err)
	}
	if res.ContentsCleared != len(seed) {
		t.Errorf("ClearResult.ContentsCleared = %d, want %d", res.ContentsCleared, len(seed))
	}
	if err := sdb.QueryRow(`SELECT COUNT(*) FROM image_contents WHERE set_id = ?`, f.setID).Scan(&contents); err != nil {
		t.Fatalf("counting image_contents after clear: %v", err)
	}
	if contents != 0 {
		t.Errorf("image_contents rows after clear = %d, want 0 (the S4 closure)", contents)
	}
	var images int
	if err := sdb.QueryRow(`SELECT COUNT(*) FROM images WHERE set_id = ?`, f.setID).Scan(&images); err != nil {
		t.Fatalf("counting images after clear: %v", err)
	}
	if images != 0 {
		t.Errorf("images rows after clear = %d, want 0", images)
	}
}
