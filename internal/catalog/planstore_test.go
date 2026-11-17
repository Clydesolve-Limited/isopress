package catalog

// T2.1: planstore integration tests against a REAL temp catalog
// (DESIGN_SPEC 12.1 catalog-SQL tier). They pin the chunk-row surface
// the planner depends on: creation with the denormalized set_id and
// NULL lazy-hash columns, idempotent re-ensure, mismatch detection,
// boundary-stable reuse (hashes preserved untouched), the re-chunk
// policy for written vs unwritten rows (4.2), the assignment guard
// (FR-011), the files.chunk_size round-trip, and PlanTx rollback
// atomicity (4.5: a failed plan leaves the catalog exactly as before).

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"
)

const gib int64 = 1 << 30

// --- helpers -----------------------------------------------------------------

// insertPlanFile stores one active regular file row and returns its id.
func insertPlanFile(t *testing.T, f *scanFixture, rel string, size int64) int64 {
	t.Helper()
	var id int64
	f.mustTx(t, func(tx *ScanTx) {
		var err error
		id, err = tx.InsertScanFile(context.Background(), regularRow(rel, size, 0xCD))
		if err != nil {
			t.Fatalf("InsertScanFile(%q): %v", rel, err)
		}
	})
	return id
}

// mustPlanTx begins a planning transaction on the fixture.
func mustPlanTx(t *testing.T, f *scanFixture) *PlanTx {
	t.Helper()
	tx, err := f.db.BeginPlanTx(context.Background(), f.runID, f.setID)
	if err != nil {
		t.Fatalf("BeginPlanTx: %v", err)
	}
	return tx
}

// rawChunkRow is one chunks row read back for test-side assertions the
// typed API deliberately does not expose (NULL-ness of every column).
type rawChunkRow struct {
	ChunkID    int64
	SetID      string
	FileID     int64
	ChunkIndex int
	Offset     int64
	Length     int64
	Hash       []byte // NULL scans as nil
	ImageID    sql.NullInt64
	Copied     bool
	VerifiedAt sql.NullInt64
}

// rawChunkRows reads every chunks row of the file, ordered by index.
func rawChunkRows(t *testing.T, db *DB, fileID int64) []rawChunkRow {
	t.Helper()
	sdb := rawOpen(t, db.Path())
	rows, err := sdb.Query(`
		SELECT chunk_id, set_id, file_id, chunk_index, offset, length,
		       hash, image_id, copied, verified_at
		FROM chunks WHERE file_id = ? ORDER BY chunk_index`, fileID)
	if err != nil {
		t.Fatalf("query chunks of file %d: %v", fileID, err)
	}
	defer rows.Close()
	var out []rawChunkRow
	for rows.Next() {
		var r rawChunkRow
		if err := rows.Scan(&r.ChunkID, &r.SetID, &r.FileID, &r.ChunkIndex, &r.Offset,
			&r.Length, &r.Hash, &r.ImageID, &r.Copied, &r.VerifiedAt); err != nil {
			t.Fatalf("scan chunk row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate chunk rows: %v", err)
	}
	return out
}

// rawChunksEqual compares two raw rows field-by-field (rawChunkRow
// holds a []byte, so it cannot be compared with ==).
func rawChunksEqual(a, b rawChunkRow) bool {
	return a.ChunkID == b.ChunkID && a.SetID == b.SetID && a.FileID == b.FileID &&
		a.ChunkIndex == b.ChunkIndex && a.Offset == b.Offset && a.Length == b.Length &&
		bytes.Equal(a.Hash, b.Hash) && a.ImageID == b.ImageID && a.Copied == b.Copied &&
		a.VerifiedAt == b.VerifiedAt
}

// stampChunkWrite simulates the copy engine's first write on the given
// chunk indices: stores a 32-byte hash, sets copied = 1, and stamps
// verified_at (the 5.1 write-verify-mark outcome).
func stampChunkWrite(t *testing.T, db *DB, fileID int64, indices ...int) {
	t.Helper()
	sdb := rawOpen(t, db.Path())
	for _, idx := range indices {
		h := bytes.Repeat([]byte{byte('A' + idx)}, 32)
		res, err := sdb.Exec(
			`UPDATE chunks SET hash = ?, copied = 1, verified_at = 123
			 WHERE file_id = ? AND chunk_index = ?`, h, fileID, idx)
		if err != nil {
			t.Fatalf("stamping write of chunk %d: %v", idx, err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Fatalf("stamping write of chunk %d: %d rows affected", idx, n)
		}
	}
}

// spec3x20Gib is the 50 GiB / 20 GiB worked-example decomposition
// (DESIGN_SPEC 4.3): 3 chunks of 20 + 20 + 10 GiB.
func spec3x20Gib() []ChunkSpec {
	return []ChunkSpec{
		{Index: 0, Offset: 0, Length: 20 * gib},
		{Index: 1, Offset: 20 * gib, Length: 20 * gib},
		{Index: 2, Offset: 40 * gib, Length: 10 * gib},
	}
}

// --- creation and idempotence -------------------------------------------------

func TestPlanStoreEnsureChunkRowsCreatesWithDenormalizedSetID(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()
	fileID := insertPlanFile(t, f, "big/video.iso", 50*gib)

	tx := mustPlanTx(t, f)
	if err := tx.EnsureChunkRows(ctx, fileID, spec3x20Gib()); err != nil {
		t.Fatalf("EnsureChunkRows: %v", err)
	}
	if err := tx.StoreChunkSize(ctx, fileID, 20*gib, 3); err != nil {
		t.Fatalf("StoreChunkSize: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	rows := rawChunkRows(t, f.db, fileID)
	if len(rows) != 3 {
		t.Fatalf("stored %d chunk rows, want 3", len(rows))
	}
	for i, r := range rows {
		if r.SetID != f.setID {
			t.Errorf("chunk %d set_id = %q, want denormalized %q", i, r.SetID, f.setID)
		}
		if r.FileID != fileID {
			t.Errorf("chunk %d file_id = %d, want %d", i, r.FileID, fileID)
		}
		if r.ChunkIndex != i {
			t.Errorf("chunk %d index = %d, want 0-based %d", i, r.ChunkIndex, i)
		}
		if r.Hash != nil {
			t.Errorf("chunk %d hash = %v, want NULL (lazy first-write, 4.3)", i, r.Hash)
		}
		if r.ImageID.Valid {
			t.Errorf("chunk %d image_id = %d, want NULL (unassigned)", i, r.ImageID.Int64)
		}
		if r.Copied {
			t.Errorf("chunk %d copied = true, want false", i)
		}
		if r.VerifiedAt.Valid {
			t.Errorf("chunk %d verified_at = %d, want NULL", i, r.VerifiedAt.Int64)
		}
	}
	want := spec3x20Gib()
	for i, r := range rows {
		if r.Offset != want[i].Offset || r.Length != want[i].Length {
			t.Errorf("chunk %d = (offset %d, length %d), want (offset %d, length %d)",
				i, r.Offset, r.Length, want[i].Offset, want[i].Length)
		}
	}

	// files.chunk_size round-trip in the same transaction.
	size, chunked, err := f.db.ReadChunkSize(ctx, f.setID, fileID)
	if err != nil || !chunked || size != 20*gib {
		t.Errorf("ReadChunkSize = (%d, %v, %v), want (21474836480, true, nil)", size, chunked, err)
	}
}

func TestPlanStoreEnsureChunkRowsIsIdempotent(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()
	fileID := insertPlanFile(t, f, "big/disk.img", 50*gib)

	tx := mustPlanTx(t, f)
	if err := tx.EnsureChunkRows(ctx, fileID, spec3x20Gib()); err != nil {
		t.Fatalf("EnsureChunkRows #1: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	before := rawChunkRows(t, f.db, fileID)

	// Re-ensure the same decomposition: no-op, rows untouched.
	tx2 := mustPlanTx(t, f)
	if err := tx2.EnsureChunkRows(ctx, fileID, spec3x20Gib()); err != nil {
		t.Fatalf("EnsureChunkRows #2 (idempotent): %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("Commit #2: %v", err)
	}
	after := rawChunkRows(t, f.db, fileID)
	if len(after) != len(before) {
		t.Fatalf("re-ensure changed row count %d -> %d", len(before), len(after))
	}
	for i := range before {
		if !rawChunksEqual(after[i], before[i]) {
			t.Errorf("re-ensure mutated row %d: %+v -> %+v", i, before[i], after[i])
		}
	}
}

func TestPlanStoreEnsureChunkRowsMismatchRefused(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()
	fileID := insertPlanFile(t, f, "big/disk.img", 50*gib)

	tx := mustPlanTx(t, f)
	if err := tx.EnsureChunkRows(ctx, fileID, spec3x20Gib()); err != nil {
		t.Fatalf("EnsureChunkRows: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	before := rawChunkRows(t, f.db, fileID)

	// A DIFFERENT decomposition is a mismatch, not a silent overwrite:
	// the re-chunk path (ReplaceAllChunks) owns that job.
	other := []ChunkSpec{{Index: 0, Offset: 0, Length: 20 * gib}}
	tx2 := mustPlanTx(t, f)
	err := tx2.EnsureChunkRows(ctx, fileID, other)
	if !errors.Is(err, ErrChunkMismatch) {
		t.Fatalf("EnsureChunkRows with different specs = %v, want ErrChunkMismatch", err)
	}
	_ = tx2.Rollback()
	after := rawChunkRows(t, f.db, fileID)
	if len(after) != len(before) {
		t.Fatalf("mismatched ensure changed the catalog: %d rows -> %d", len(before), len(after))
	}
	for i := range before {
		if !rawChunksEqual(after[i], before[i]) {
			t.Errorf("mismatched ensure mutated row %d: %+v -> %+v", i, before[i], after[i])
		}
	}
}

// --- reuse: the boundary-stability read path (4.2) -----------------------------

func TestPlanStoreReuseExistingChunkRowsPreservesHashes(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()
	fileID := insertPlanFile(t, f, "big/video.iso", 50*gib)

	// First plan creates rows; chunks 0 and 1 are written (hash
	// stored); chunk 2 stays unwritten.
	tx := mustPlanTx(t, f)
	if err := tx.EnsureChunkRows(ctx, fileID, spec3x20Gib()); err != nil {
		t.Fatalf("EnsureChunkRows: %v", err)
	}
	if err := tx.StoreChunkSize(ctx, fileID, 20*gib, 3); err != nil {
		t.Fatalf("StoreChunkSize: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	stampChunkWrite(t, f.db, fileID, 0, 1)
	before := rawChunkRows(t, f.db, fileID)

	// Re-plan with the SAME effective size: reuse returns the stored
	// rows; the row-level check verified count/offsets/lengths.
	tx2 := mustPlanTx(t, f)
	reused, err := tx2.ReuseExistingChunkRows(ctx, fileID, spec3x20Gib())
	if err != nil {
		t.Fatalf("ReuseExistingChunkRows: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if len(reused) != 3 {
		t.Fatalf("reuse returned %d rows, want 3", len(reused))
	}
	if len(reused[0].Hash) != 32 || len(reused[1].Hash) != 32 || reused[2].Hash != nil {
		t.Errorf("reused hashes = (%d bytes, %d bytes, %d bytes), want (32, 32, NULL)",
			len(reused[0].Hash), len(reused[1].Hash), len(reused[2].Hash))
	}

	// The reuse path wrote NOTHING: rows bit-identical afterwards.
	after := rawChunkRows(t, f.db, fileID)
	if len(after) != len(before) {
		t.Fatalf("reuse changed row count %d -> %d", len(before), len(after))
	}
	for i := range before {
		if !rawChunksEqual(after[i], before[i]) {
			t.Errorf("reuse mutated row %d: %+v -> %+v", i, before[i], after[i])
		}
	}

	// A DIFFERENT decomposition (changed profile) is a mismatch.
	tx3 := mustPlanTx(t, f)
	_, err = tx3.ReuseExistingChunkRows(ctx, fileID,
		[]ChunkSpec{{Index: 0, Offset: 0, Length: 20 * gib}})
	if !errors.Is(err, ErrChunkMismatch) {
		t.Fatalf("reuse with changed boundaries = %v, want ErrChunkMismatch", err)
	}
	_ = tx3.Rollback()

	// A file with NO stored rows is a mismatch too (create-fresh path).
	fresh := insertPlanFile(t, f, "big/fresh.img", 30*gib)
	tx4 := mustPlanTx(t, f)
	if _, err := tx4.ReuseExistingChunkRows(ctx, fresh, spec3x20Gib()); !errors.Is(err, ErrChunkMismatch) {
		t.Fatalf("reuse with zero stored rows = %v, want ErrChunkMismatch", err)
	}
	_ = tx4.Rollback()
}

// --- re-chunk: written vs unwritten row policy (4.2) ----------------------------

func TestPlanStoreReplaceAllChunksPolicy(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()

	// 3 GiB + 5 bytes with a 1 GiB effective size: 4 chunks
	// (1 GiB x3 + 5). Rows 1 and 2 are already written.
	fileID := insertPlanFile(t, f, "big/vm.img", 3*gib+5)
	old := []ChunkSpec{
		{Index: 0, Offset: 0, Length: gib},
		{Index: 1, Offset: gib, Length: gib},
		{Index: 2, Offset: 2 * gib, Length: gib},
		{Index: 3, Offset: 3 * gib, Length: 5},
	}
	tx := mustPlanTx(t, f)
	if err := tx.EnsureChunkRows(ctx, fileID, old); err != nil {
		t.Fatalf("EnsureChunkRows: %v", err)
	}
	if err := tx.StoreChunkSize(ctx, fileID, gib, 4); err != nil {
		t.Fatalf("StoreChunkSize: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	stampChunkWrite(t, f.db, fileID, 1, 2)

	// Profile change: effective size 2 GiB. New decomposition of the
	// same file: 2 GiB + (1 GiB + 5).
	newSpecs := []ChunkSpec{
		{Index: 0, Offset: 0, Length: 2 * gib},
		{Index: 1, Offset: 2 * gib, Length: gib + 5},
	}
	tx2 := mustPlanTx(t, f)
	res, err := tx2.ReplaceAllChunks(ctx, fileID, newSpecs)
	if err != nil {
		t.Fatalf("ReplaceAllChunks: %v", err)
	}
	if err := tx2.StoreChunkSize(ctx, fileID, 2*gib, 2); err != nil {
		t.Fatalf("StoreChunkSize after re-chunk: %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// The result reports the written rows the profile change
	// invalidated: those ranges are re-read and re-hashed at their
	// next write (the plan report states exactly this).
	if res.RemovedWritten != 2 || res.RemovedUnwritten != 2 {
		t.Errorf("RechunkResult = {unwritten %d, written %d}, want {unwritten 2, written 2}",
			res.RemovedUnwritten, res.RemovedWritten)
	}

	// Full replacement: 2 fresh rows matching the new decomposition,
	// ALL with NULL hash, NULL image_id, copied 0 - the previously
	// written rows are gone with their old-boundary hashes.
	rows := rawChunkRows(t, f.db, fileID)
	if len(rows) != 2 {
		t.Fatalf("after re-chunk: %d rows, want 2", len(rows))
	}
	for i, r := range rows {
		if r.ChunkIndex != i || r.Offset != newSpecs[i].Offset || r.Length != newSpecs[i].Length {
			t.Errorf("row %d = (index %d, offset %d, length %d), want (index %d, offset %d, length %d)",
				i, r.ChunkIndex, r.Offset, r.Length, i, newSpecs[i].Offset, newSpecs[i].Length)
		}
		if r.Hash != nil {
			t.Errorf("row %d carries a stale hash after re-chunk: %v", i, r.Hash)
		}
		if r.ImageID.Valid || r.Copied || r.VerifiedAt.Valid {
			t.Errorf("row %d = {image %v, copied %v, verified %v}, want all cleared",
				i, r.ImageID, r.Copied, r.VerifiedAt)
		}
	}

	// The files row reflects the new decomposition.
	size, chunked, err := f.db.ReadChunkSize(ctx, f.setID, fileID)
	if err != nil || !chunked || size != 2*gib {
		t.Errorf("ReadChunkSize after re-chunk = (%d, %v, %v), want (%d, true, nil)",
			size, chunked, err, 2*gib)
	}
}

func TestPlanStoreReplaceAllChunksRefusesAssignedRows(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()
	fileID := insertPlanFile(t, f, "big/video.iso", 50*gib)

	tx := mustPlanTx(t, f)
	if err := tx.EnsureChunkRows(ctx, fileID, spec3x20Gib()); err != nil {
		t.Fatalf("EnsureChunkRows: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	stampChunkWrite(t, f.db, fileID, 0)
	// An assignment lands on chunk 1 (locked, FR-011): clear-assignments
	// is the only path that may remove it.
	sdb := rawOpen(t, f.db.Path())
	if _, err := sdb.Exec(`UPDATE chunks SET image_id = 7 WHERE file_id = ? AND chunk_index = 1`,
		fileID); err != nil {
		t.Fatalf("assigning chunk 1: %v", err)
	}
	before := rawChunkRows(t, f.db, fileID)

	tx2 := mustPlanTx(t, f)
	_, err := tx2.ReplaceAllChunks(ctx, fileID,
		[]ChunkSpec{{Index: 0, Offset: 0, Length: 30 * gib}})
	if !errors.Is(err, ErrChunkRowAssigned) {
		t.Fatalf("ReplaceAllChunks on assigned rows = %v, want ErrChunkRowAssigned", err)
	}
	_ = tx2.Rollback()

	after := rawChunkRows(t, f.db, fileID)
	if len(after) != len(before) {
		t.Fatalf("refused re-chunk changed row count %d -> %d", len(before), len(after))
	}
	for i := range before {
		if !rawChunksEqual(after[i], before[i]) {
			t.Errorf("refused re-chunk mutated row %d: %+v -> %+v", i, before[i], after[i])
		}
	}
}

// --- chunk-size stamping --------------------------------------------------------

func TestPlanStoreChunkSizeRoundTrip(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()
	fileID := insertPlanFile(t, f, "big/a.img", 50*gib)

	// Never chunked: NULL chunk_size reads as (0, false).
	size, chunked, err := f.db.ReadChunkSize(ctx, f.setID, fileID)
	if err != nil || chunked || size != 0 {
		t.Fatalf("ReadChunkSize (fresh) = (%d, %v, %v), want (0, false, nil)", size, chunked, err)
	}

	// Stamp, read back, re-stamp with a changed profile's size.
	tx := mustPlanTx(t, f)
	if err := tx.StoreChunkSize(ctx, fileID, 20*gib, 3); err != nil {
		t.Fatalf("StoreChunkSize: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if size, chunked, err = f.db.ReadChunkSize(ctx, f.setID, fileID); err != nil || !chunked || size != 20*gib {
		t.Fatalf("ReadChunkSize = (%d, %v, %v), want (%d, true, nil)", size, chunked, err, 20*gib)
	}

	tx2 := mustPlanTx(t, f)
	if err := tx2.StoreChunkSize(ctx, fileID, gib, 50); err != nil {
		t.Fatalf("StoreChunkSize (changed): %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	if size, chunked, err = f.db.ReadChunkSize(ctx, f.setID, fileID); err != nil || !chunked || size != gib {
		t.Fatalf("ReadChunkSize (changed) = (%d, %v, %v), want (%d, true, nil)", size, chunked, err, gib)
	}
	// chunked/chunk_count/chunk_size columns all stamped.
	sdb := rawOpen(t, f.db.Path())
	var stamped, count int64
	var stored sql.NullInt64
	if err := sdb.QueryRow(`SELECT chunked, chunk_count, chunk_size FROM files WHERE file_id = ?`,
		fileID).Scan(&stamped, &count, &stored); err != nil {
		t.Fatalf("read chunk columns: %v", err)
	}
	if stamped != 1 || count != 50 || !stored.Valid || stored.Int64 != gib {
		t.Errorf("chunk columns = (chunked %d, count %d, size %v), want (1, 50, %d)",
			stamped, count, stored, gib)
	}

	// Invalid stamps are refused before hitting the row.
	tx3 := mustPlanTx(t, f)
	if err := tx3.StoreChunkSize(ctx, fileID, 0, 3); err == nil {
		t.Error("StoreChunkSize(0) returned nil, want error")
	}
	if err := tx3.StoreChunkSize(ctx, fileID, gib, -1); err == nil {
		t.Error("StoreChunkSize with negative count returned nil, want error")
	}
	if err := tx3.StoreChunkSize(ctx, 99999, gib, 1); err == nil {
		t.Error("StoreChunkSize for unknown file returned nil, want error")
	}
	_ = tx3.Rollback()

	// Unknown file reads as an error, not silence.
	if _, _, err := f.db.ReadChunkSize(ctx, f.setID, 99999); err == nil {
		t.Error("ReadChunkSize for unknown file returned nil error, want error")
	}
}

// --- spec validation and atomicity ----------------------------------------------

func TestPlanStoreRejectsInvalidDecompositionSpecs(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()
	fileID := insertPlanFile(t, f, "big/a.img", 50*gib)

	cases := []struct {
		name  string
		specs []ChunkSpec
	}{
		{"empty", nil},
		{"non-1-based-gap index", []ChunkSpec{{Index: 1, Offset: 0, Length: 10}}},
		{"non-positive length", []ChunkSpec{{Index: 0, Offset: 0, Length: 0}}},
		{
			"non-contiguous offsets",
			[]ChunkSpec{{Index: 0, Offset: 0, Length: 10}, {Index: 1, Offset: 11, Length: 5}},
		},
	}
	for _, c := range cases {
		tx := mustPlanTx(t, f)
		if err := tx.EnsureChunkRows(ctx, fileID, c.specs); err == nil {
			t.Errorf("EnsureChunkRows accepted %s", c.name)
		}
		_ = tx.Rollback()
		tx2 := mustPlanTx(t, f)
		if _, err := tx2.ReuseExistingChunkRows(ctx, fileID, c.specs); err == nil {
			t.Errorf("ReuseExistingChunkRows accepted %s", c.name)
		}
		_ = tx2.Rollback()
		tx3 := mustPlanTx(t, f)
		if _, err := tx3.ReplaceAllChunks(ctx, fileID, c.specs); err == nil {
			t.Errorf("ReplaceAllChunks accepted %s", c.name)
		}
		_ = tx3.Rollback()
	}
	// Nothing leaked into the catalog.
	if rows := rawChunkRows(t, f.db, fileID); len(rows) != 0 {
		t.Errorf("invalid specs leaked %d chunk rows", len(rows))
	}
}

func TestPlanStorePlanTxRollbackLeavesCatalogUntouched(t *testing.T) {
	// DESIGN_SPEC 4.5 atomicity: a rolled-back planning transaction
	// leaves the catalog exactly as before (FR-010).
	f := newScanFixture(t)
	ctx := context.Background()
	fileID := insertPlanFile(t, f, "big/video.iso", 50*gib)

	tx := mustPlanTx(t, f)
	if err := tx.EnsureChunkRows(ctx, fileID, spec3x20Gib()); err != nil {
		t.Fatalf("EnsureChunkRows: %v", err)
	}
	if err := tx.StoreChunkSize(ctx, fileID, 20*gib, 3); err != nil {
		t.Fatalf("StoreChunkSize: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if rows := rawChunkRows(t, f.db, fileID); len(rows) != 0 {
		t.Fatalf("rollback left %d chunk rows, want 0", len(rows))
	}
	if _, chunked, err := f.db.ReadChunkSize(ctx, f.setID, fileID); err != nil || chunked {
		t.Fatalf("rollback left chunk state (%v, %v), want pristine", chunked, err)
	}
	// Rollback is idempotent (the done-flag pattern of ScanTx).
	if err := tx.Rollback(); err != nil {
		t.Fatalf("second Rollback: %v", err)
	}
}

func TestPlanStoreEnsureChunkRowsUnknownFileFails(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()

	tx := mustPlanTx(t, f)
	// foreign_keys=ON: a chunks row can never reference a missing
	// files row, so the insert fails and the planner rolls back.
	if err := tx.EnsureChunkRows(ctx, 99999, spec3x20Gib()); err == nil {
		t.Fatal("EnsureChunkRows for unknown file_id returned nil, want FK error")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
}

// --- mid-transaction failure atomicity on the destructive re-chunk path -------

func TestPlanStoreMidTxFailureRollsBackRechunkAtomically(t *testing.T) {
	// DESIGN_SPEC 4.5 atomicity for the DESTRUCTIVE path: a re-chunk
	// that has already deleted and re-inserted chunk rows, followed by
	// a mid-transaction failure elsewhere in the same PlanTx, must roll
	// back as ONE unit - the OLD rows (hashes, copied marks,
	// verified_at, chunk_ids included) and the OLD files.chunk_size
	// stamp survive bit-for-bit. This is the scenario a planner crash
	// mid-plan must leave behind: a failed plan changes nothing
	// (FR-010), even when its last action was a re-chunk.
	f := newScanFixture(t)
	ctx := context.Background()
	fileID := insertPlanFile(t, f, "big/vm.img", 50*gib)

	// Committed baseline: 3x20 GiB decomposition, chunks 0 and 1
	// already written (hash stored, copied, verified).
	tx := mustPlanTx(t, f)
	if err := tx.EnsureChunkRows(ctx, fileID, spec3x20Gib()); err != nil {
		t.Fatalf("EnsureChunkRows: %v", err)
	}
	if err := tx.StoreChunkSize(ctx, fileID, 20*gib, 3); err != nil {
		t.Fatalf("StoreChunkSize: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	stampChunkWrite(t, f.db, fileID, 0, 1)
	before := rawChunkRows(t, f.db, fileID)
	if len(before) != 3 {
		t.Fatalf("baseline: %d rows, want 3", len(before))
	}

	// One PlanTx: a re-chunk that succeeds (deletes the 3 old rows,
	// inserts 2 new-boundary rows), then a FAILING statement in the
	// same transaction (StoreChunkSize against a file outside the
	// set - the caller-bug path that must abort the plan).
	tx2 := mustPlanTx(t, f)
	newSpecs := []ChunkSpec{
		{Index: 0, Offset: 0, Length: 30 * gib},
		{Index: 1, Offset: 30 * gib, Length: 20 * gib},
	}
	res, err := tx2.ReplaceAllChunks(ctx, fileID, newSpecs)
	if err != nil {
		t.Fatalf("ReplaceAllChunks: %v", err)
	}
	if res.RemovedWritten != 2 || res.RemovedUnwritten != 1 {
		t.Fatalf("RechunkResult = {unwritten %d, written %d}, want {unwritten 1, written 2}",
			res.RemovedUnwritten, res.RemovedWritten)
	}
	// The mid-transaction failure.
	if err := tx2.StoreChunkSize(ctx, 99999, 30*gib, 2); err == nil {
		t.Fatal("StoreChunkSize for unknown file returned nil, want error")
	}
	// The caller rolls back on failure (the documented PlanTx contract).
	if err := tx2.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// The catalog is exactly as before: the destructive re-chunk left
	// NO trace - old rows, old hashes, old copied/verified marks all
	// intact (rawChunksEqual compares chunk_id too, proving the rows
	// were never actually deleted in the committed state).
	after := rawChunkRows(t, f.db, fileID)
	if len(after) != len(before) {
		t.Fatalf("rolled-back re-chunk changed row count %d -> %d", len(before), len(after))
	}
	for i := range before {
		if !rawChunksEqual(after[i], before[i]) {
			t.Errorf("rolled-back re-chunk mutated row %d: %+v -> %+v", i, before[i], after[i])
		}
	}
	// The files.chunk_size stamp still describes the OLD decomposition.
	size, chunked, err := f.db.ReadChunkSize(ctx, f.setID, fileID)
	if err != nil || !chunked || size != 20*gib {
		t.Errorf("ReadChunkSize after rolled-back re-chunk = (%d, %v, %v), want (%d, true, nil)",
			size, chunked, err, 20*gib)
	}
}

// --- ensure idempotence preserves WRITTEN hashes ------------------------------

func TestPlanStoreEnsureChunkRowsPreservesWrittenHashesOnReensure(t *testing.T) {
	// The idempotent re-ensure must be a true no-op even when the
	// stored rows carry real state (hashes from first writes, copied
	// marks, verified_at): the boundary-stability contract is that a
	// re-plan with the same effective size preserves stored hashes
	// bit-for-bit (DESIGN_SPEC 4.2) - here asserted on the
	// EnsureChunkRows path with non-NULL hashes present.
	f := newScanFixture(t)
	ctx := context.Background()
	fileID := insertPlanFile(t, f, "big/disk.img", 50*gib)

	tx := mustPlanTx(t, f)
	if err := tx.EnsureChunkRows(ctx, fileID, spec3x20Gib()); err != nil {
		t.Fatalf("EnsureChunkRows #1: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
	// First writes land: chunks 0 and 1 get hashes, copied, verified.
	stampChunkWrite(t, f.db, fileID, 0, 1)
	before := rawChunkRows(t, f.db, fileID)
	if before[0].Hash == nil || before[1].Hash == nil || before[2].Hash != nil {
		t.Fatalf("stampChunkWrite did not produce the expected hash state")
	}

	tx2 := mustPlanTx(t, f)
	if err := tx2.EnsureChunkRows(ctx, fileID, spec3x20Gib()); err != nil {
		t.Fatalf("EnsureChunkRows #2 (re-ensure over written rows): %v", err)
	}
	if err := tx2.Commit(); err != nil {
		t.Fatalf("Commit #2: %v", err)
	}
	after := rawChunkRows(t, f.db, fileID)
	if len(after) != len(before) {
		t.Fatalf("re-ensure changed row count %d -> %d", len(before), len(after))
	}
	for i := range before {
		if !rawChunksEqual(after[i], before[i]) {
			t.Errorf("re-ensure over written rows mutated row %d: %+v -> %+v", i, before[i], after[i])
		}
	}
	if len(after[0].Hash) != 32 || len(after[1].Hash) != 32 || !after[0].Copied || !after[1].Copied {
		t.Errorf("re-ensure lost written state: chunk 0 hash %d bytes/copied %v, "+
			"chunk 1 hash %d bytes/copied %v",
			len(after[0].Hash), after[0].Copied, len(after[1].Hash), after[1].Copied)
	}
}
