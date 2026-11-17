package catalog

// T2.2 phase 2: tests for the balancer's catalog surface
// (planstore_assign.go) against a REAL temp catalog (DESIGN_SPEC 12.1
// catalog-SQL tier). They pin: the images-row insert and its guards,
// the FR-011 NULL-guard discipline of AssignFile / AssignChunk /
// AssignDir, the folded NB2 DropChunkState transition (chunk rows
// deleted AND files.chunked/chunk_count/chunk_size un-stamped in one
// transaction; assigned rows refused), the read projections
// ListActiveForPlan / ChunkStates / Destinations (including
// the fill folding over files+chunks+dirs), and the NB1 offset-0
// invariant on every chunk-row entry point. The T2.2 adversarial
// follow-up adds: the dir lifecycle across missing -> reappear with an
// assignment intact (FR-011 for dirs), and the NB2 drop over WRITTEN
// chunk rows (full-replacement semantics, files.hash anchor intact).

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"testing"
)

// --- helpers -----------------------------------------------------------------

// planTxCommit begins, runs fn, commits; failures fail the test.
func planTxCommit(t *testing.T, f *scanFixture, fn func(tx *PlanTx)) {
	t.Helper()
	tx := mustPlanTx(t, f)
	fn(tx)
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}
}

// insertPlanSymlink stores one active symlink row and returns its id.
func insertPlanSymlink(t *testing.T, f *scanFixture, rel, target string) int64 {
	t.Helper()
	var id int64
	f.mustTx(t, func(tx *ScanTx) {
		var err error
		id, err = tx.InsertScanFile(context.Background(), FileRow{
			RelPath: rel, EntryKind: "symlink", Size: int64(len(target)),
			Mode: 0o777, UID: 1000, GID: 1000, MtimeNS: 123,
			Dev: 42, Inode: 2001, Nlink: 1, LinkTarget: target, Status: "active",
		})
		if err != nil {
			t.Fatalf("InsertScanFile(symlink %q): %v", rel, err)
		}
	})
	return id
}

// insertPlanHardlink stores one active regular row of a hard-link
// family (shared dev+inode, nlink=2) and returns its id.
func insertPlanHardlink(t *testing.T, f *scanFixture, rel string, size, inode int64) int64 {
	t.Helper()
	var id int64
	f.mustTx(t, func(tx *ScanTx) {
		row := regularRow(rel, size, 0xAB)
		row.Dev = 7
		row.Inode = inode
		row.Nlink = 2
		var err error
		id, err = tx.InsertScanFile(context.Background(), row)
		if err != nil {
			t.Fatalf("InsertScanFile(hardlink %q): %v", rel, err)
		}
	})
	return id
}

// insertEmptyDir upserts one active empty directory row and returns its id.
func insertEmptyDir(t *testing.T, f *scanFixture, rel string) int64 {
	t.Helper()
	var dirID int64
	f.mustTx(t, func(tx *ScanTx) {
		if err := tx.UpsertEmptyDir(context.Background(), DirRow{
			RelPath: rel, Mode: 0o755, UID: 1000, GID: 1000, MtimeNS: 999,
		}); err != nil {
			t.Fatalf("UpsertEmptyDir(%q): %v", rel, err)
		}
	})
	if err := f.db.sql.QueryRow(
		`SELECT dir_id FROM directories WHERE set_id = ? AND relpath = ?`,
		f.setID, rel).Scan(&dirID); err != nil {
		t.Fatalf("reading dir_id of %q: %v", rel, err)
	}
	return dirID
}

// markDirMissing stamps a directories row missing (test-side state
// builder for the "missing dirs are never assigned" invariant).
func markDirMissing(t *testing.T, f *scanFixture, dirID int64) {
	t.Helper()
	sdb := rawOpen(t, f.db.Path())
	if _, err := sdb.Exec(`UPDATE directories SET status = 'missing' WHERE dir_id = ?`, dirID); err != nil {
		t.Fatalf("marking dir %d missing: %v", dirID, err)
	}
}

// --- images rows --------------------------------------------------------------

func TestPlanAssignInsertImageRoundTripAndGuards(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()

	planTxCommit(t, f, func(tx *PlanTx) {
		if err := tx.InsertImage(ctx, ImageSpec{
			ImageID: 1, NominalSize: 25_000_000_000, UsableSize: 23_000_000_000,
			MediaProfile: "bd-r-sl",
		}); err != nil {
			t.Fatalf("InsertImage: %v", err)
		}
	})

	// Round trip through the Destinations read: state 'planned', the
	// D-06 figures verbatim, zero fill.
	dests, err := f.db.Destinations(ctx, f.setID)
	if err != nil {
		t.Fatalf("Destinations: %v", err)
	}
	if len(dests) != 1 {
		t.Fatalf("Destinations = %d rows, want 1", len(dests))
	}
	d := dests[0]
	if d.ImageID != 1 || d.NominalSize != 25_000_000_000 || d.UsableSize != 23_000_000_000 ||
		d.MediaProfile != "bd-r-sl" || d.UsedBytes != 0 || d.ItemCount != 0 || d.DirCount != 0 {
		t.Errorf("destination = %+v, want fresh image 1 with the profile figures and zero fill", d)
	}
	var state string
	if err := f.db.sql.QueryRow(
		`SELECT state FROM images WHERE set_id = ? AND image_id = 1`, f.setID).Scan(&state); err != nil {
		t.Fatalf("reading image state: %v", err)
	}
	if state != "planned" {
		t.Errorf("image state = %q, want planned", state)
	}

	// Guards: bad ids, bad capacities, empty profile, duplicate id.
	bad := []ImageSpec{
		{ImageID: 0, NominalSize: 10, UsableSize: 9, MediaProfile: "disk"},
		{ImageID: 2, NominalSize: -1, UsableSize: 9, MediaProfile: "disk"},
		{ImageID: 2, NominalSize: 10, UsableSize: 0, MediaProfile: "disk"},
		{ImageID: 2, NominalSize: 10, UsableSize: 9, MediaProfile: ""},
		{ImageID: 1, NominalSize: 10, UsableSize: 9, MediaProfile: "disk"}, // duplicate
	}
	for i, spec := range bad {
		tx := mustPlanTx(t, f)
		if err := tx.InsertImage(ctx, spec); err == nil {
			t.Errorf("case %d (%+v): InsertImage returned nil, want error", i, spec)
		}
		_ = tx.Rollback()
	}
	// Nothing leaked: still exactly one images row.
	var n int
	if err := f.db.sql.QueryRow(`SELECT COUNT(*) FROM images`).Scan(&n); err != nil {
		t.Fatalf("counting images: %v", err)
	}
	if n != 1 {
		t.Errorf("images rows after refused inserts = %d, want 1", n)
	}
}

// --- assignments and the FR-011 lock -------------------------------------------

func TestPlanAssignFileSetsAssignmentAndCopiedMirror(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()
	fileID := insertPlanFile(t, f, "docs/a.txt", 100)

	planTxCommit(t, f, func(tx *PlanTx) {
		if err := tx.InsertImage(ctx, ImageSpec{ImageID: 1, NominalSize: 1000, UsableSize: 900,
			MediaProfile: "disk"}); err != nil {
			t.Fatalf("InsertImage: %v", err)
		}
		if err := tx.AssignFile(ctx, fileID, 1); err != nil {
			t.Fatalf("AssignFile: %v", err)
		}
	})

	var (
		imageID sql.NullInt64
		copied  int64
	)
	if err := f.db.sql.QueryRow(
		`SELECT image_id, copied FROM files WHERE file_id = ?`, fileID).
		Scan(&imageID, &copied); err != nil {
		t.Fatalf("reading assignment: %v", err)
	}
	if !imageID.Valid || imageID.Int64 != 1 || copied != 0 {
		t.Errorf("files row = (image %v, copied %d), want (1, 0)", imageID, copied)
	}

	// FR-011: a second assignment is refused (zero-row guard), and so
	// are unknown files and non-positive image ids.
	reject := []struct {
		name    string
		fileID  int64
		imageID int64
	}{
		{"re-assign locked file", fileID, 2},
		{"unknown file", 99999, 1},
		{"zero image id", fileID, 0},
	}
	for _, r := range reject {
		tx := mustPlanTx(t, f)
		if err := tx.AssignFile(ctx, r.fileID, r.imageID); err == nil {
			t.Errorf("%s: AssignFile returned nil, want error", r.name)
		}
		_ = tx.Rollback()
	}
	// The locked assignment is untouched.
	if err := f.db.sql.QueryRow(
		`SELECT image_id FROM files WHERE file_id = ?`, fileID).Scan(&imageID); err != nil {
		t.Fatalf("re-reading assignment: %v", err)
	}
	if !imageID.Valid || imageID.Int64 != 1 {
		t.Errorf("locked assignment mutated to %v, want image 1", imageID)
	}
}

func TestPlanAssignChunkSetsAssignmentWithLockGuard(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()
	fileID := insertPlanFile(t, f, "big/vm.img", 3*gib)

	planTxCommit(t, f, func(tx *PlanTx) {
		if err := tx.InsertImage(ctx, ImageSpec{ImageID: 1, NominalSize: 5 * gib,
			UsableSize: 4 * gib, MediaProfile: "disk"}); err != nil {
			t.Fatalf("InsertImage: %v", err)
		}
		if err := tx.EnsureChunkRows(ctx, fileID, []ChunkSpec{
			{Index: 0, Offset: 0, Length: 2 * gib},
			{Index: 1, Offset: 2 * gib, Length: gib},
		}); err != nil {
			t.Fatalf("EnsureChunkRows: %v", err)
		}
		for i := 0; i < 2; i++ {
			if err := tx.AssignChunk(ctx, fileID, i, 1); err != nil {
				t.Fatalf("AssignChunk(%d): %v", i, err)
			}
		}
	})

	// Fill folded from the chunks table.
	dests, err := f.db.Destinations(ctx, f.setID)
	if err != nil {
		t.Fatalf("Destinations: %v", err)
	}
	if len(dests) != 1 || dests[0].UsedBytes != 3*gib || dests[0].ItemCount != 2 {
		t.Fatalf("destinations = %+v, want one with 3 GiB across 2 chunk items", dests)
	}

	// Lock guards: re-assign, unknown chunk index, unknown file.
	reject := []struct {
		name    string
		fileID  int64
		chunk   int
		imageID int64
	}{
		{"re-assign locked chunk", fileID, 0, 2},
		{"unknown chunk index", fileID, 9, 1},
		{"unknown file", 99999, 0, 1},
		{"zero image id", fileID, 0, 0},
	}
	for _, r := range reject {
		tx := mustPlanTx(t, f)
		if err := tx.AssignChunk(ctx, r.fileID, r.chunk, r.imageID); err == nil {
			t.Errorf("%s: AssignChunk returned nil, want error", r.name)
		}
		_ = tx.Rollback()
	}
}

// TestPlanAssignChunkResetsMarksBehindNullGuard is the M8 half of
// AC-24: AssignChunk mirrors AssignFile's mark reset - a freshly
// assigned chunk row gets copied = 0 and verified_at = NULL in the
// SAME statement, with its stored hash untouched - behind the
// UNCHANGED image_id IS NULL guard. An ALREADY-assigned row can never
// match that guard: the refusal fires and the locked row's image_id,
// copied, verified_at and hash all survive byte-identical (the
// defense-in-depth beneath the balancer's compute-time refusal, 4.3
// PARTIALLY-ASSIGNED CHUNK SETS).
func TestPlanAssignChunkResetsMarksBehindNullGuard(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()
	fileID := insertPlanFile(t, f, "big/vm.img", 3*gib)

	planTxCommit(t, f, func(tx *PlanTx) {
		if err := tx.InsertImage(ctx, ImageSpec{ImageID: 1, NominalSize: 5 * gib,
			UsableSize: 4 * gib, MediaProfile: "disk"}); err != nil {
			t.Fatalf("InsertImage: %v", err)
		}
		if err := tx.EnsureChunkRows(ctx, fileID, []ChunkSpec{
			{Index: 0, Offset: 0, Length: 2 * gib},
			{Index: 1, Offset: 2 * gib, Length: gib},
		}); err != nil {
			t.Fatalf("EnsureChunkRows: %v", err)
		}
	})

	// Out-of-band stale marks on BOTH unassigned rows (the shape a
	// pre-reset writer or a hand-edited catalog could leave): chunk 0
	// gets a stored hash too, so the hash-retention half is pinned.
	staleHash := bytes.Repeat([]byte{0xA5}, 32)
	if _, err := f.db.sql.Exec(
		`UPDATE chunks SET copied = 1, verified_at = 4242, hash = ? WHERE file_id = ?`,
		staleHash, fileID); err != nil {
		t.Fatalf("seeding stale marks: %v", err)
	}

	planTxCommit(t, f, func(tx *PlanTx) {
		if err := tx.AssignChunk(ctx, fileID, 0, 1); err != nil {
			t.Fatalf("AssignChunk: %v", err)
		}
	})

	type chunkRow struct {
		imageID    sql.NullInt64
		copied     int64
		verifiedAt sql.NullInt64
		hash       []byte
	}
	read := func(t *testing.T, idx int) chunkRow {
		t.Helper()
		var r chunkRow
		if err := f.db.sql.QueryRow(
			`SELECT image_id, copied, verified_at, hash FROM chunks
			 WHERE file_id = ? AND chunk_index = ?`, fileID, idx).
			Scan(&r.imageID, &r.copied, &r.verifiedAt, &r.hash); err != nil {
			t.Fatalf("reading chunk %d: %v", idx, err)
		}
		return r
	}

	// The freshly assigned row: marks reset, hash retained, id set.
	got0 := read(t, 0)
	if !got0.imageID.Valid || got0.imageID.Int64 != 1 {
		t.Errorf("chunk 0 image_id = %v, want 1", got0.imageID)
	}
	if got0.copied != 0 || got0.verifiedAt.Valid {
		t.Errorf("chunk 0 marks = (copied %d, verified_at %v), want (0, NULL) - the M8 reset",
			got0.copied, got0.verifiedAt)
	}
	if !bytes.Equal(got0.hash, staleHash) {
		t.Errorf("chunk 0 hash changed across the assignment (FR-010 retention)")
	}
	// The untouched sibling: its stale marks survive (nothing outside
	// the assignment touched it).
	got1 := read(t, 1)
	if got1.imageID.Valid || got1.copied != 1 || !got1.verifiedAt.Valid || got1.verifiedAt.Int64 != 4242 {
		t.Errorf("chunk 1 = %+v, want the untouched stale marks (NULL, 1, 4242)", got1)
	}

	// The guard: an ALREADY-assigned row can never match image_id IS
	// NULL - the refusal fires and the locked row is byte-unchanged.
	tx := mustPlanTx(t, f)
	if err := tx.AssignChunk(ctx, fileID, 0, 2); err == nil {
		t.Error("re-assigning an assigned chunk returned nil, want the FR-011 refusal")
	}
	_ = tx.Rollback()
	got0 = read(t, 0)
	if !got0.imageID.Valid || got0.imageID.Int64 != 1 || got0.copied != 0 ||
		!bytes.Equal(got0.hash, staleHash) {
		t.Errorf("locked chunk 0 mutated by the refused assignment: %+v", got0)
	}
}

// --- NB2: DropChunkState (whole-file transition) --------------------------------

func TestPlanAssignDropChunkStateDeletesRowsAndUnstamps(t *testing.T) {
	// The folded T2.1 review note NB2 end to end at the SQL level: a
	// file chunked under a small profile, cleared (simulated), then
	// re-planned under a LARGE profile lands WHOLE - and after
	// DropChunkState + AssignFile the catalog shows ZERO chunk rows
	// and chunked = 0 / chunk_size NULL, so restore (8.4), which reads
	// chunk rows, never sees a catalog that lies.
	f := newScanFixture(t)
	ctx := context.Background()

	// Small profile: the 30-byte file does not fit a 20-byte usable
	// destination, so it is chunked 20 + 10.
	fileID := insertPlanFile(t, f, "small-target.iso", 30)
	planTxCommit(t, f, func(tx *PlanTx) {
		if err := tx.InsertImage(ctx, ImageSpec{ImageID: 1, NominalSize: 25,
			UsableSize: 20, MediaProfile: "disk"}); err != nil {
			t.Fatalf("InsertImage: %v", err)
		}
		if err := tx.EnsureChunkRows(ctx, fileID, []ChunkSpec{
			{Index: 0, Offset: 0, Length: 20},
			{Index: 1, Offset: 20, Length: 10},
		}); err != nil {
			t.Fatalf("EnsureChunkRows: %v", err)
		}
		if err := tx.StoreChunkSize(ctx, fileID, 20, 2); err != nil {
			t.Fatalf("StoreChunkSize: %v", err)
		}
		if err := tx.AssignChunk(ctx, fileID, 0, 1); err != nil {
			t.Fatalf("AssignChunk(0): %v", err)
		}
		if err := tx.AssignChunk(ctx, fileID, 1, 1); err != nil {
			t.Fatalf("AssignChunk(1): %v", err)
		}
	})

	// clear-assignments (T2.4, simulated here with the documented
	// semantics: image_id = NULL, nothing deleted).
	sdb := rawOpen(t, f.db.Path())
	if _, err := sdb.Exec(`UPDATE chunks SET image_id = NULL`); err != nil {
		t.Fatalf("simulating clear-assignments: %v", err)
	}
	if _, err := sdb.Exec(`UPDATE files SET image_id = NULL`); err != nil {
		t.Fatalf("simulating clear-assignments (files): %v", err)
	}

	// Large profile: usable 100 >= 30, so the file is now a WHOLE
	// item. The balancer's NB2 op: DropChunkState, then AssignFile.
	planTxCommit(t, f, func(tx *PlanTx) {
		if err := tx.InsertImage(ctx, ImageSpec{ImageID: 2, NominalSize: 100,
			UsableSize: 100, MediaProfile: "disk"}); err != nil {
			t.Fatalf("InsertImage(2): %v", err)
		}
		dropped, err := tx.DropChunkState(ctx, fileID)
		if err != nil {
			t.Fatalf("DropChunkState: %v", err)
		}
		if dropped != 2 {
			t.Errorf("DropChunkState dropped %d rows, want 2", dropped)
		}
		if err := tx.AssignFile(ctx, fileID, 2); err != nil {
			t.Fatalf("AssignFile: %v", err)
		}
	})

	// The catalog tells the truth: ZERO chunk rows, chunked = 0,
	// chunk_count = 0, chunk_size NULL, assignment on image 2.
	if rows := rawChunkRows(t, f.db, fileID); len(rows) != 0 {
		t.Fatalf("after whole-file transition: %d chunk rows, want 0", len(rows))
	}
	var (
		chunked    int64
		chunkCount int64
		chunkSize  sql.NullInt64
		imageID    sql.NullInt64
	)
	if err := f.db.sql.QueryRow(
		`SELECT chunked, chunk_count, chunk_size, image_id FROM files WHERE file_id = ?`,
		fileID).Scan(&chunked, &chunkCount, &chunkSize, &imageID); err != nil {
		t.Fatalf("reading file state: %v", err)
	}
	if chunked != 0 || chunkCount != 0 || chunkSize.Valid {
		t.Errorf("chunk stamp after transition = (chunked %d, count %d, size %v), want (0, 0, NULL)",
			chunked, chunkCount, chunkSize)
	}
	if !imageID.Valid || imageID.Int64 != 2 {
		t.Errorf("assignment after transition = %v, want image 2", imageID)
	}
}

func TestPlanAssignDropChunkStateRefusesAssignedRowsAndUnknownFile(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()
	fileID := insertPlanFile(t, f, "big/locked.img", 40)

	planTxCommit(t, f, func(tx *PlanTx) {
		if err := tx.EnsureChunkRows(ctx, fileID, []ChunkSpec{
			{Index: 0, Offset: 0, Length: 20},
			{Index: 1, Offset: 20, Length: 20},
		}); err != nil {
			t.Fatalf("EnsureChunkRows: %v", err)
		}
		if err := tx.AssignChunk(ctx, fileID, 1, 7); err != nil {
			t.Fatalf("AssignChunk: %v", err)
		}
	})

	// FR-011: a chunk with an assignment blocks the drop outright.
	tx := mustPlanTx(t, f)
	if _, err := tx.DropChunkState(ctx, fileID); !errors.Is(err, ErrChunkRowAssigned) {
		t.Fatalf("DropChunkState with assigned chunk = %v, want ErrChunkRowAssigned", err)
	}
	_ = tx.Rollback()
	if rows := rawChunkRows(t, f.db, fileID); len(rows) != 2 {
		t.Fatalf("refused drop changed the catalog: %d rows, want 2", len(rows))
	}

	// Unknown file is an error, not silence.
	tx2 := mustPlanTx(t, f)
	if _, err := tx2.DropChunkState(ctx, 99999); err == nil {
		t.Error("DropChunkState(99999) returned nil, want error")
	}
	_ = tx2.Rollback()
}

// --- NB1: the offset-0 invariant on every chunk-row entry point ----------------

func TestPlanAssignNonZeroFirstOffsetRefusedEverywhere(t *testing.T) {
	// NB1: a decomposition must start at the file head. A nonzero
	// first offset would silently drop the file's leading bytes at
	// first write - the truncation shape FR-041 forbids.
	f := newScanFixture(t)
	ctx := context.Background()
	fileID := insertPlanFile(t, f, "big/a.img", 30)

	shifted := []ChunkSpec{{Index: 0, Offset: 5, Length: 25}}
	shiftedMid := []ChunkSpec{
		{Index: 0, Offset: 5, Length: 20},
		{Index: 1, Offset: 25, Length: 5},
	}
	for _, specs := range [][]ChunkSpec{shifted, shiftedMid} {
		tx := mustPlanTx(t, f)
		if err := tx.EnsureChunkRows(ctx, fileID, specs); err == nil {
			t.Errorf("EnsureChunkRows accepted first offset %d", specs[0].Offset)
		}
		_ = tx.Rollback()
		tx2 := mustPlanTx(t, f)
		if _, err := tx2.ReuseExistingChunkRows(ctx, fileID, specs); err == nil {
			t.Errorf("ReuseExistingChunkRows accepted first offset %d", specs[0].Offset)
		}
		_ = tx2.Rollback()
		tx3 := mustPlanTx(t, f)
		if _, err := tx3.ReplaceAllChunks(ctx, fileID, specs); err == nil {
			t.Errorf("ReplaceAllChunks accepted first offset %d", specs[0].Offset)
		}
		_ = tx3.Rollback()
	}
	if rows := rawChunkRows(t, f.db, fileID); len(rows) != 0 {
		t.Errorf("refused decompositions leaked %d chunk rows", len(rows))
	}
}

// --- read side: ListActiveForPlan ----------------------------------------------

func TestPlanAssignListActiveForPlanProjection(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()

	whole := insertPlanFile(t, f, "docs/a.txt", 100)
	assigned := insertPlanFile(t, f, "docs/locked.txt", 50)
	big := insertPlanFile(t, f, "big/vm.img", 5*gib)
	link := insertPlanSymlink(t, f, "docs/link", "../target")
	hl1 := insertPlanHardlink(t, f, "hl/a", 10, 9001)
	hl2 := insertPlanHardlink(t, f, "hl/b", 10, 9001)

	planTxCommit(t, f, func(tx *PlanTx) {
		if err := tx.InsertImage(ctx, ImageSpec{ImageID: 1, NominalSize: 1000,
			UsableSize: 900, MediaProfile: "disk"}); err != nil {
			t.Fatalf("InsertImage: %v", err)
		}
		if err := tx.AssignFile(ctx, assigned, 1); err != nil {
			t.Fatalf("AssignFile: %v", err)
		}
		if err := tx.StoreChunkSize(ctx, big, 2*gib, 3); err != nil {
			t.Fatalf("StoreChunkSize: %v", err)
		}
	})
	// A missing row and a superseded row must NOT appear. The
	// versioned pair contributes only the NEW row (the old one is
	// superseded; the new one is active like any fresh content).
	var oldID, newID int64
	f.mustTx(t, func(tx *ScanTx) {
		old := FileRow{
			RelPath: "docs/gone.txt", EntryKind: "regular", Size: 5, Mode: 0o644,
			UID: 1, GID: 1, MtimeNS: 1, Dev: 1, Inode: 5, Nlink: 1,
			Hash: make([]byte, 32), Status: "active",
		}
		var err error
		oldID, err = tx.InsertScanFile(ctx, old)
		if err != nil {
			t.Fatalf("inserting old row: %v", err)
		}
		mod := old
		mod.Size = 6
		newID, err = tx.InsertModification(ctx, oldID, mod)
		if err != nil {
			t.Fatalf("InsertModification: %v", err)
		}
	})
	f.mustTx(t, func(tx *ScanTx) {
		if _, err := tx.MarkMissing(ctx, []int64{hl2}); err != nil {
			t.Fatalf("MarkMissing: %v", err)
		}
	})

	files, err := f.db.ListActiveForPlan(ctx, f.setID)
	if err != nil {
		t.Fatalf("ListActiveForPlan: %v", err)
	}
	byID := make(map[int64]PlanActiveFile, len(files))
	for _, pf := range files {
		byID[pf.FileID] = pf
	}
	// Active set: whole, assigned, big, link, hl1 (hl2 is missing; the
	// superseded old row is gone; the modification's NEW row counts).
	if len(files) != 6 {
		t.Fatalf("ListActiveForPlan returned %d rows, want 6: %+v", len(files), files)
	}
	if _, ok := byID[oldID]; ok {
		t.Errorf("superseded row %d appeared in the plan read", oldID)
	}
	if pf, ok := byID[newID]; !ok || pf.Size != 6 {
		t.Errorf("modification row %d = %+v present=%v, want active with size 6", newID, pf, ok)
	}
	if pf := byID[whole]; pf.ImageID != 0 || pf.Size != 100 || pf.EntryKind != "regular" {
		t.Errorf("whole file row = %+v, want unassigned regular of 100 bytes", pf)
	}
	if pf := byID[assigned]; pf.ImageID != 1 {
		t.Errorf("assigned row = %+v, want image_id 1 (LOCKED)", pf)
	}
	if pf := byID[big]; pf.ChunkSize != 2*gib {
		t.Errorf("big row = %+v, want stored chunk_size %d", pf, 2*gib)
	}
	if pf := byID[link]; pf.EntryKind != "symlink" || pf.Size != int64(len("../target")) {
		t.Errorf("symlink row = %+v, want symlink with size len(target)", pf)
	}
	if pf := byID[hl1]; pf.Nlink != 2 || pf.Dev != 7 || pf.Inode != 9001 {
		t.Errorf("hard-link row = %+v, want (dev 7, inode 9001, nlink 2)", pf)
	}
}

// --- read side: ChunkStates ------------------------------------------------------

func TestPlanAssignChunkStatesIncludeLockedFilesWithImageIDs(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()

	chunked := insertPlanFile(t, f, "big/vm.img", 3*gib)
	assigned := insertPlanFile(t, f, "big/locked.img", 3*gib)
	never := insertPlanFile(t, f, "small.txt", 10)

	planTxCommit(t, f, func(tx *PlanTx) {
		if err := tx.InsertImage(ctx, ImageSpec{ImageID: 1, NominalSize: 5 * gib,
			UsableSize: 4 * gib, MediaProfile: "disk"}); err != nil {
			t.Fatalf("InsertImage: %v", err)
		}
		for _, id := range []int64{chunked, assigned} {
			if err := tx.EnsureChunkRows(ctx, id, []ChunkSpec{
				{Index: 0, Offset: 0, Length: 2 * gib},
				{Index: 1, Offset: 2 * gib, Length: gib},
			}); err != nil {
				t.Fatalf("EnsureChunkRows: %v", err)
			}
			if err := tx.StoreChunkSize(ctx, id, 2*gib, 2); err != nil {
				t.Fatalf("StoreChunkSize: %v", err)
			}
		}
		if err := tx.AssignChunk(ctx, assigned, 0, 1); err != nil {
			t.Fatalf("AssignChunk: %v", err)
		}
		if err := tx.AssignChunk(ctx, assigned, 1, 1); err != nil {
			t.Fatalf("AssignChunk: %v", err)
		}
	})
	// One chunk of the unassigned file is already written (hash
	// stored) - the HashPresent signal of the re-hash report.
	stampChunkWrite(t, f.db, chunked, 1)

	states, err := f.db.ChunkStates(ctx, f.setID)
	if err != nil {
		t.Fatalf("ChunkStates: %v", err)
	}
	// BOTH chunked files appear - including the LOCKED one: a
	// chunked file never carries files.image_id (4.5), so its lock
	// is visible only through the chunk rows' own image_id, which
	// this read must return. The never-chunked file has no rows.
	if len(states) != 4 {
		t.Fatalf("ChunkStates returned %d rows, want 4: %+v", len(states), states)
	}
	for i, cs := range states {
		wantFile, wantIndex := chunked, i
		if i >= 2 {
			wantFile, wantIndex = assigned, i-2
		}
		if cs.FileID != wantFile || cs.ChunkSize != 2*gib || cs.Index != wantIndex {
			t.Errorf("row %d = %+v, want file %d chunk_size %d index %d",
				i, cs, wantFile, 2*gib, wantIndex)
		}
		wantAssigned := i >= 2 // the second file's chunk set is the locked one
		if assigned := cs.ImageID != 0; assigned != wantAssigned {
			t.Errorf("row %d image_id = %d: assigned = %v, want %v (locked set on the second file only)",
				i, cs.ImageID, assigned, wantAssigned)
		}
	}
	if states[0].HashPresent || !states[1].HashPresent {
		t.Errorf("HashPresent = (%v, %v), want (false, true) - index 1 of the first file carries the stamped hash",
			states[0].HashPresent, states[1].HashPresent)
	}
	_ = never
}

// --- read side: Destinations fill folding --------------------------------------

func TestPlanAssignDestinationsFoldsFileChunkAndDirFill(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()

	whole := insertPlanFile(t, f, "docs/a.txt", 100)
	symlink := insertPlanSymlink(t, f, "docs/link", "abcde") // size = 5
	chunked := insertPlanFile(t, f, "big/vm.img", 3*gib)
	dirA := insertEmptyDir(t, f, "empty-a")
	dirB := insertEmptyDir(t, f, "empty-b")

	planTxCommit(t, f, func(tx *PlanTx) {
		if err := tx.InsertImage(ctx, ImageSpec{ImageID: 1, NominalSize: 5 * gib,
			UsableSize: 4 * gib, MediaProfile: "disk"}); err != nil {
			t.Fatalf("InsertImage: %v", err)
		}
		if err := tx.InsertImage(ctx, ImageSpec{ImageID: 2, NominalSize: 5 * gib,
			UsableSize: 4 * gib, MediaProfile: "disk"}); err != nil {
			t.Fatalf("InsertImage: %v", err)
		}
		if err := tx.EnsureChunkRows(ctx, chunked, []ChunkSpec{
			{Index: 0, Offset: 0, Length: 2 * gib},
			{Index: 1, Offset: 2 * gib, Length: gib},
		}); err != nil {
			t.Fatalf("EnsureChunkRows: %v", err)
		}
		for _, a := range []struct {
			fileID  int64
			imageID int64
		}{{whole, 1}, {symlink, 1}} {
			if err := tx.AssignFile(ctx, a.fileID, a.imageID); err != nil {
				t.Fatalf("AssignFile(%d): %v", a.fileID, err)
			}
		}
		if err := tx.AssignChunk(ctx, chunked, 0, 2); err != nil {
			t.Fatalf("AssignChunk(0): %v", err)
		}
		if err := tx.AssignDir(ctx, dirA, 1); err != nil {
			t.Fatalf("AssignDir: %v", err)
		}
		if err := tx.AssignDir(ctx, dirB, 1); err != nil {
			t.Fatalf("AssignDir: %v", err)
		}
	})

	dests, err := f.db.Destinations(ctx, f.setID)
	if err != nil {
		t.Fatalf("Destinations: %v", err)
	}
	if len(dests) != 2 {
		t.Fatalf("Destinations = %d rows, want 2", len(dests))
	}
	// Image 1: whole file (100) + symlink (5) = 105 bytes, 2 items,
	// 2 dirs. Image 2: one chunk of 2 GiB.
	if d := dests[0]; d.ImageID != 1 || d.UsedBytes != 105 || d.ItemCount != 2 || d.DirCount != 2 {
		t.Errorf("image 1 fill = %+v, want {105 bytes, 2 items, 2 dirs}", d)
	}
	if d := dests[1]; d.ImageID != 2 || d.UsedBytes != 2*gib || d.ItemCount != 1 || d.DirCount != 0 {
		t.Errorf("image 2 fill = %+v, want {%d bytes, 1 item, 0 dirs}", d, 2*gib)
	}
}

// --- dirs surface (list + assign) ----------------------------------------------

func TestPlanAssignDirSurfaceListsOnlyActiveUnassigned(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()

	a := insertEmptyDir(t, f, "empty-a")
	b := insertEmptyDir(t, f, "empty-b")
	gone := insertEmptyDir(t, f, "gone")
	markDirMissing(t, f, gone)

	dirs, err := f.db.ListActiveUnassignedDirs(ctx, f.setID)
	if err != nil {
		t.Fatalf("ListActiveUnassignedDirs: %v", err)
	}
	if len(dirs) != 2 || dirs[0].DirID != a || dirs[1].DirID != b {
		t.Fatalf("unassigned dirs = %+v, want active [%d %d] and never the missing one", dirs, a, b)
	}

	planTxCommit(t, f, func(tx *PlanTx) {
		if err := tx.InsertImage(ctx, ImageSpec{ImageID: 1, NominalSize: 1000,
			UsableSize: 900, MediaProfile: "disk"}); err != nil {
			t.Fatalf("InsertImage: %v", err)
		}
		if err := tx.AssignDir(ctx, a, 1); err != nil {
			t.Fatalf("AssignDir: %v", err)
		}
	})
	// Assigned dirs leave the unassigned list; the lock guard refuses
	// a re-target; the missing dir is never assignable through the
	// planner's list.
	dirs, err = f.db.ListActiveUnassignedDirs(ctx, f.setID)
	if err != nil {
		t.Fatalf("ListActiveUnassignedDirs (after assign): %v", err)
	}
	if len(dirs) != 1 || dirs[0].DirID != b {
		t.Fatalf("unassigned dirs after assign = %+v, want only [%d]", dirs, b)
	}
	tx := mustPlanTx(t, f)
	if err := tx.AssignDir(ctx, a, 2); err == nil {
		t.Error("AssignDir re-target of a locked dir returned nil, want error")
	}
	_ = tx.Rollback()
}

// --- dir lifecycle across missing -> reappear, with an assignment (FR-011) -----------
//
// The reconciler pins the status flips (missing on delete/gained-file,
// re-active on reappear); this test pins what an ASSIGNMENT does across
// the same lifecycle, at the SQL level:
//   - a dir that goes missing KEEPS its image_id (MarkDirsMissing
//     writes status only, dirs.go);
//   - reappearing re-activates the row but must NOT un-assign or
//     re-target it (UpsertEmptyDir preserves image_id, dirs.go);
//   - the planner's item source (ListActiveUnassignedDirs) never sees
//     it again - the lock holds, so a re-plan performs no reassignment;
//   - a never-assigned dir that goes missing disappears from the item
//     source while missing, and becomes assignable AGAIN when it
//     reappears (image_id was still NULL).

func TestPlanAssignDirSurvivesMissingAndReappearanceLocked(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()

	assigned := insertEmptyDir(t, f, "empty/assigned")
	pending := insertEmptyDir(t, f, "empty/pending")
	planTxCommit(t, f, func(tx *PlanTx) {
		if err := tx.InsertImage(ctx, ImageSpec{ImageID: 1, NominalSize: 1000,
			UsableSize: 900, MediaProfile: "disk"}); err != nil {
			t.Fatalf("InsertImage: %v", err)
		}
		if err := tx.AssignDir(ctx, assigned, 1); err != nil {
			t.Fatalf("AssignDir: %v", err)
		}
	})

	// Both dirs vanish from the source (deleted, or gained files).
	f.mustTx(t, func(tx *ScanTx) {
		n, err := tx.MarkDirsMissing(ctx, []int64{assigned, pending})
		if err != nil {
			t.Fatalf("MarkDirsMissing: %v", err)
		}
		if n != 2 {
			t.Fatalf("MarkDirsMissing marked %d rows, want 2", n)
		}
	})
	dirs, err := f.db.ListActiveUnassignedDirs(ctx, f.setID)
	if err != nil {
		t.Fatalf("ListActiveUnassignedDirs (missing): %v", err)
	}
	if len(dirs) != 0 {
		t.Fatalf("unassigned list while missing = %+v, want empty (missing dirs are no planner items)", dirs)
	}

	// Both reappear (a rescan finds them empty again).
	f.mustTx(t, func(tx *ScanTx) {
		for _, rel := range []string{"empty/assigned", "empty/pending"} {
			if err := tx.UpsertEmptyDir(ctx, DirRow{
				RelPath: rel, Mode: 0o755, UID: 1000, GID: 1000, MtimeNS: 42,
			}); err != nil {
				t.Fatalf("UpsertEmptyDir(%q): %v", rel, err)
			}
		}
	})

	// The reappeared ASSIGNMENT is intact and LOCKED: only the
	// never-assigned dir is a planner item again.
	dirs, err = f.db.ListActiveUnassignedDirs(ctx, f.setID)
	if err != nil {
		t.Fatalf("ListActiveUnassignedDirs (reappeared): %v", err)
	}
	if len(dirs) != 1 || dirs[0].DirID != pending {
		t.Fatalf("unassigned dirs after reappear = %+v, want only the never-assigned [%d]", dirs, pending)
	}
	var imageID sql.NullInt64
	if err := f.db.sql.QueryRow(
		`SELECT image_id FROM directories WHERE dir_id = ?`, assigned).Scan(&imageID); err != nil {
		t.Fatalf("reading reappeared dir assignment: %v", err)
	}
	if !imageID.Valid || imageID.Int64 != 1 {
		t.Errorf("reappeared dir image_id = %v, want the original 1 (no unassign, no re-target)", imageID)
	}
	// Mechanical confirmation of the lock: a re-target is refused.
	tx := mustPlanTx(t, f)
	if err := tx.AssignDir(ctx, assigned, 2); err == nil {
		t.Error("re-target of a reappeared assigned dir returned nil, want an FR-011 refusal")
	}
	_ = tx.Rollback()
}

// --- NB2 drop over WRITTEN chunk rows: full-replacement semantics --------------------
//
// DropChunkState deletes the file's chunk rows INCLUDING rows whose
// first write already stored a hash (and copied/verified marks). That
// is correct per 4.2/4.5 full-replacement: clear-assignments retains
// rows+hashes because the PLACEMENT survives it, but the NB2
// whole-file transition deletes the placement itself - the boundary
// hash described a row that no longer exists. The whole-file reference
// (files.hash) is the surviving verification anchor (FR-014 step 5
// re-hashes the whole file against it at copy time), and this test
// pins that it is untouched by the drop.

func TestPlanAssignDropChunkStateDropsWrittenRowsKeepsWholeFileHash(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()

	fileID := insertPlanFile(t, f, "big/movie.iso", 30)
	planTxCommit(t, f, func(tx *PlanTx) {
		if err := tx.EnsureChunkRows(ctx, fileID, []ChunkSpec{
			{Index: 0, Offset: 0, Length: 20},
			{Index: 1, Offset: 20, Length: 10},
		}); err != nil {
			t.Fatalf("EnsureChunkRows: %v", err)
		}
		if err := tx.StoreChunkSize(ctx, fileID, 20, 2); err != nil {
			t.Fatalf("StoreChunkSize: %v", err)
		}
	})
	// Chunk 0 was written: hash stored, copied = 1, verified_at set.
	stampChunkWrite(t, f.db, fileID, 0)
	if rows := rawChunkRows(t, f.db, fileID); len(rows) != 2 || len(rows[0].Hash) != 32 {
		t.Fatalf("pre-condition: chunk rows = %+v, want 2 rows with chunk 0 written", rows)
	}

	planTxCommit(t, f, func(tx *PlanTx) {
		dropped, err := tx.DropChunkState(ctx, fileID)
		if err != nil {
			t.Fatalf("DropChunkState: %v", err)
		}
		if dropped != 2 {
			t.Errorf("DropChunkState dropped %d rows, want 2 (the WRITTEN row included)", dropped)
		}
	})

	if rows := rawChunkRows(t, f.db, fileID); len(rows) != 0 {
		t.Fatalf("after NB2 drop: %d chunk rows, want 0", len(rows))
	}
	var (
		chunked    int64
		chunkCount int64
		chunkSize  sql.NullInt64
		hash       []byte
	)
	if err := f.db.sql.QueryRow(
		`SELECT chunked, chunk_count, chunk_size, hash FROM files WHERE file_id = ?`,
		fileID).Scan(&chunked, &chunkCount, &chunkSize, &hash); err != nil {
		t.Fatalf("reading file state: %v", err)
	}
	if chunked != 0 || chunkCount != 0 || chunkSize.Valid {
		t.Errorf("stamp after drop = (chunked %d, count %d, size %v), want (0, 0, NULL)",
			chunked, chunkCount, chunkSize)
	}
	if want := bytes.Repeat([]byte{0xCD}, 32); !bytes.Equal(hash, want) {
		t.Errorf("files.hash after drop = %x, want the whole-file reference %x untouched "+
			"(it anchors the whole-file re-verification, FR-014)", hash, want)
	}
}
