package catalog

// T3.3: the copy-side catalog surface tests against a REAL temp
// catalog (the 12.1 SQLite tier). They pin the contracts the writer
// depends on: the complete per-image inventory (files, chunks with
// parents, symlinks, dirs, hard-link secondaries), the mark
// transactions (flag + image_contents row atomic together; the 5.2
// parent mirror inside the LAST chunk's transaction; the 4.3 lazy hash
// stored with the mark), the embedded-member rows, the whole-set
// placement + hard-link family reads, and the interrupted-run
// finalization closure (FinishRunDetached).

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// copyFixture seeds set S-COPY with the full row shape the writer
// consumes:
//
//	files:   a.txt (whole, image 1), big.bin (chunked 50+50 across
//	         images 1-2), link (symlink, image 2), fam-a + fam-b
//	         (hard-link family, inode 500, primary fam-a on image 1,
//	         fam-b excluded from items per 2.4)
//	dirs:    empty (image 1)
//	chunks:  big.bin#0 -> image 1, big.bin#1 -> image 2
//
// images:  1, 2
type copyFixture struct {
	db    *DB
	setID string
	runID int64

	fileA, big, link, famA, famB int64 // file_ids
}

var (
	hashA   = sha256.Sum256([]byte("alpha content"))
	hashBig = sha256.Sum256([]byte("big content"))
	hashFam = sha256.Sum256([]byte("family content"))
	chunk0  = sha256.Sum256([]byte("chunk zero bytes"))
	chunk1  = sha256.Sum256([]byte("chunk one bytes!"))
)

func newCopyFixture(t *testing.T) *copyFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.sqlite")
	mustCreate(t, path)
	db := mustOpen(t, path, OpenReadWrite)
	ctx := context.Background()
	f := &copyFixture{db: db, setID: "S-COPY"}
	if err := db.EnsureSet(ctx, f.setID, "/data", time.Now().UnixNano(), ToolVersion); err != nil {
		t.Fatalf("EnsureSet: %v", err)
	}
	runID, err := db.StartScanRun(ctx, ScanRun{
		SetID: f.setID, Kind: "ingest", Root: "/data", StartedAt: time.Now().UnixNano(),
	})
	if err != nil {
		t.Fatalf("StartScanRun: %v", err)
	}
	f.runID = runID

	insert := func(row FileRow) int64 {
		t.Helper()
		return f.scanTx(t, func(tx *ScanTx) int64 {
			id, err := tx.InsertScanFile(ctx, row)
			if err != nil {
				t.Fatalf("InsertScanFile %q: %v", row.RelPath, err)
			}
			return id
		})
	}
	f.fileA = insert(FileRow{RelPath: "docs/a.txt", EntryKind: "regular", Size: 10,
		Mode: 0o644, MtimeNS: 1000, Dev: 1, Inode: 101, Nlink: 1, Hash: hashA[:], Status: "active"})
	f.big = insert(FileRow{RelPath: "big.bin", EntryKind: "regular", Size: 100,
		Mode: 0o644, MtimeNS: 2000, Dev: 1, Inode: 102, Nlink: 1, Hash: hashBig[:], Status: "active"})
	f.link = insert(FileRow{RelPath: "link", EntryKind: "symlink", Size: int64(len("docs/a.txt")),
		Mode: 0o777, MtimeNS: 3000, Dev: 1, Inode: 103, Nlink: 1, LinkTarget: "docs/a.txt", Status: "active"})
	f.famA = insert(FileRow{RelPath: "fam-a", EntryKind: "regular", Size: 8,
		Mode: 0o644, MtimeNS: 4000, Dev: 1, Inode: 500, Nlink: 2, Hash: hashFam[:], Status: "active"})
	f.famB = insert(FileRow{RelPath: "fam-b", EntryKind: "regular", Size: 8,
		Mode: 0o644, MtimeNS: 4000, Dev: 1, Inode: 500, Nlink: 2, Hash: hashFam[:], Status: "active"})
	f.scanTx(t, func(tx *ScanTx) int64 {
		if err := tx.UpsertEmptyDir(ctx, DirRow{RelPath: "empty", Mode: 0o755, MtimeNS: 5000}); err != nil {
			t.Fatalf("UpsertEmptyDir: %v", err)
		}
		return 0
	})

	// The plan: image 1 holds a.txt + big.bin#0 + fam-a (primary) +
	// empty; image 2 holds the symlink + big.bin#1.
	if err := db.BeginPlanTxRun(t, ctx, func(tx *PlanTx) {
		if err := tx.InsertImage(ctx, ImageSpec{ImageID: 1, NominalSize: 1000, UsableSize: 980, MediaProfile: "disk"}); err != nil {
			t.Fatalf("InsertImage 1: %v", err)
		}
		if err := tx.InsertImage(ctx, ImageSpec{ImageID: 2, NominalSize: 1000, UsableSize: 980, MediaProfile: "disk"}); err != nil {
			t.Fatalf("InsertImage 2: %v", err)
		}
		if err := tx.AssignFile(ctx, f.fileA, 1); err != nil {
			t.Fatalf("AssignFile a.txt: %v", err)
		}
		if err := tx.EnsureChunkRows(ctx, f.big, []ChunkSpec{
			{Index: 0, Offset: 0, Length: 50}, {Index: 1, Offset: 50, Length: 50},
		}); err != nil {
			t.Fatalf("EnsureChunkRows: %v", err)
		}
		if err := tx.AssignChunk(ctx, f.big, 0, 1); err != nil {
			t.Fatalf("AssignChunk 0: %v", err)
		}
		if err := tx.AssignChunk(ctx, f.big, 1, 2); err != nil {
			t.Fatalf("AssignChunk 1: %v", err)
		}
		if err := tx.AssignFile(ctx, f.famA, 1); err != nil {
			t.Fatalf("AssignFile fam-a: %v", err)
		}
		if err := tx.AssignFile(ctx, f.link, 2); err != nil {
			t.Fatalf("AssignFile link: %v", err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	var dirID int64
	if err := db.sql.QueryRowContext(ctx,
		`SELECT dir_id FROM directories WHERE set_id = ? AND relpath = 'empty'`, f.setID).Scan(&dirID); err != nil {
		t.Fatalf("reading dir id: %v", err)
	}
	if err := db.BeginPlanTxRun(t, ctx, func(tx *PlanTx) {
		if err := tx.AssignDir(ctx, dirID, 1); err != nil {
			t.Fatalf("AssignDir: %v", err)
		}
	}); err != nil {
		t.Fatal(err)
	}
	return f
}

// scanTx runs one ScanTx closure (the fixture's insert seam) and
// returns whatever the closure produces (the inserted file_id).
func (f *copyFixture) scanTx(t *testing.T, fn func(tx *ScanTx) int64) int64 {
	t.Helper()
	tx, err := f.db.BeginScanTx(context.Background(), f.runID, f.setID)
	if err != nil {
		t.Fatalf("BeginScanTx: %v", err)
	}
	id := fn(tx)
	if err := tx.Commit(); err != nil {
		t.Fatalf("ScanTx commit: %v", err)
	}
	return id
}

// BeginPlanTxRun runs one PlanTx closure (the fixture's plan seam).
func (db *DB) BeginPlanTxRun(t *testing.T, ctx context.Context, fn func(tx *PlanTx)) error {
	t.Helper()
	tx, err := db.BeginPlanTx(ctx, 0, "S-COPY")
	if err != nil {
		return err
	}
	fn(tx)
	return tx.Commit()
}

// --- the inventory -----------------------------------------------------------

func TestCopyInventoryCompletePerImage(t *testing.T) {
	f := newCopyFixture(t)
	ctx := context.Background()

	inv1, err := f.db.CopyInventory(ctx, f.setID, 1)
	if err != nil {
		t.Fatalf("CopyInventory image 1: %v", err)
	}
	// Whole files on image 1: a.txt AND the hard-link PRIMARY fam-a
	// (the primary is a normal whole-file item; only the SECONDARY is
	// excluded from placement per 2.4 and recreated via link(2)).
	if len(inv1.Files) != 2 || inv1.Files[0].FileID != f.fileA || inv1.Files[1].FileID != f.famA {
		t.Fatalf("image 1 files = %+v, want [a.txt fam-a]", inv1.Files)
	}
	if len(inv1.Chunks) != 1 || inv1.Chunks[0].ChunkIndex != 0 || inv1.Chunks[0].Length != 50 {
		t.Fatalf("image 1 chunks = %+v, want big.bin#0 (length 50)", inv1.Chunks)
	}
	if inv1.Chunks[0].ParentRelPath != "big.bin" || inv1.Chunks[0].ParentSize != 100 ||
		inv1.Chunks[0].Offset != 0 {
		t.Fatalf("image 1 chunk parent join broken: %+v", inv1.Chunks[0])
	}
	if string(inv1.Chunks[0].ParentHash) != string(hashBig[:]) {
		t.Fatalf("chunk parent hash = %x, want the whole-file anchor", inv1.Chunks[0].ParentHash)
	}
	if len(inv1.Dirs) != 1 || inv1.Dirs[0].RelPath != "empty" {
		t.Fatalf("image 1 dirs = %+v, want the empty dir", inv1.Dirs)
	}
	if len(inv1.Symlinks) != 0 {
		t.Fatalf("image 1 symlinks = %+v, want none", inv1.Symlinks)
	}
	if len(inv1.Secondaries) != 1 || inv1.Secondaries[0].FileID != f.famB ||
		inv1.Secondaries[0].PrimaryFileID != f.famA ||
		inv1.Secondaries[0].PrimaryRelPath != "fam-a" {
		t.Fatalf("image 1 secondaries = %+v, want fam-b with primary fam-a", inv1.Secondaries)
	}
	if string(inv1.Secondaries[0].Hash) != string(hashFam[:]) {
		t.Fatalf("secondary hash = %x, want the shared family hash", inv1.Secondaries[0].Hash)
	}

	inv2, err := f.db.CopyInventory(ctx, f.setID, 2)
	if err != nil {
		t.Fatalf("CopyInventory image 2: %v", err)
	}
	if len(inv2.Files) != 0 {
		t.Fatalf("image 2 files = %+v, want none", inv2.Files)
	}
	if len(inv2.Symlinks) != 1 || inv2.Symlinks[0].FileID != f.link ||
		inv2.Symlinks[0].LinkTarget != "docs/a.txt" {
		t.Fatalf("image 2 symlinks = %+v, want the link row with its target", inv2.Symlinks)
	}
	if len(inv2.Chunks) != 1 || inv2.Chunks[0].ChunkIndex != 1 || inv2.Chunks[0].Offset != 50 {
		t.Fatalf("image 2 chunks = %+v, want big.bin#1 at offset 50", inv2.Chunks)
	}
	if len(inv2.Secondaries) != 0 {
		t.Fatalf("image 2 secondaries = %+v, want none (fam-a is the primary)", inv2.Secondaries)
	}

	if _, err := f.db.CopyInventory(ctx, f.setID, 0); err == nil {
		t.Fatal("CopyInventory with image id 0 must fail (images are 1-based, 6.4)")
	}
}

// --- the marks ---------------------------------------------------------------

func TestSetFileCopiedFlagAndContentRow(t *testing.T) {
	f := newCopyFixture(t)
	ctx := context.Background()
	cs := f.db.CopyStore(f.setID, 1)
	if err := cs.SetFileCopied(ctx, f.fileA, "files/docs/a.txt"); err != nil {
		t.Fatalf("SetFileCopied: %v", err)
	}
	// The flag and the content row exist together; kind derives from
	// entry_kind; size/hash come from the row itself.
	f.assertFileCopied(t, f.fileA, true)
	kind, size, hash := f.imageContent(t, 1, "files/docs/a.txt")
	if kind != "file" || size != 10 || string(hash) != string(hashA[:]) {
		t.Fatalf("content row = (%s, %d, %x), want (file, 10, a.txt hash)", kind, size, hash)
	}
	// Idempotent re-mark (a present-but-unmarked crash re-run): the
	// REPLACE keeps exactly one row.
	if err := cs.SetFileCopied(ctx, f.fileA, "files/docs/a.txt"); err != nil {
		t.Fatalf("re-SetFileCopied: %v", err)
	}
	var rows int
	if err := f.db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM image_contents WHERE set_id = ? AND image_id = 1 AND media_path = 'files/docs/a.txt'`,
		f.setID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("content rows = %d, want 1 after a re-mark", rows)
	}

	// A symlink row marks through the SAME function with kind 'symlink'
	// and a NULL hash (the 2.3 convention).
	cs2 := f.db.CopyStore(f.setID, 2)
	if err := cs2.SetFileCopied(ctx, f.link, "files/link"); err != nil {
		t.Fatalf("SetFileCopied (symlink): %v", err)
	}
	kind, size, hash = f.imageContent(t, 2, "files/link")
	if kind != "symlink" || size != int64(len("docs/a.txt")) || hash != nil {
		t.Fatalf("symlink content row = (%s, %d, %x), want (symlink, len(target), NULL)", kind, size, hash)
	}

	// Unknown file id fails loudly and writes nothing.
	if err := cs.SetFileCopied(ctx, 99999, "files/ghost"); err == nil {
		t.Fatal("SetFileCopied for an unknown file must fail")
	}
}

func TestSetChunkCopiedMirrorInSameTransaction(t *testing.T) {
	f := newCopyFixture(t)
	ctx := context.Background()
	cs1 := f.db.CopyStore(f.setID, 1)
	cs2 := f.db.CopyStore(f.setID, 2)

	// THE NEVER-COMPROMISE ORDERING: a magic media path trips a
	// test-created BEFORE-INSERT trigger, so the transaction fails
	// AFTER the chunk UPDATE already ran - proving the flag, the
	// stored hash, the mirror, and the content row roll back TOGETHER.
	f.installContentTrigger(t, "BOOM")
	err := cs1.SetChunkCopied(ctx, f.big, 0, chunk0[:], "BOOM")
	if err == nil {
		t.Fatal("SetChunkCopied must fail when the content insert fails")
	}
	f.assertChunkState(t, f.big, 0, false, nil)
	f.assertFileCopied(t, f.big, false)
	f.dropContentTrigger(t)

	// The clean first-chunk mark: flag + 4.3 hash stored + content
	// row; the parent mirror stays FALSE (one of two chunks).
	if err := cs1.SetChunkCopied(ctx, f.big, 0, chunk0[:], "chunks/big.bin/00001"); err != nil {
		t.Fatalf("SetChunkCopied #0: %v", err)
	}
	f.assertChunkState(t, f.big, 0, true, chunk0[:])
	f.assertFileCopied(t, f.big, false)
	kind, size, hash := f.imageContent(t, 1, "chunks/big.bin/00001")
	if kind != "chunk" || size != 50 || string(hash) != string(chunk0[:]) {
		t.Fatalf("chunk content row = (%s, %d, %x), want (chunk, 50, captured hash)", kind, size, hash)
	}

	// The LAST chunk flips the parent mirror INSIDE its transaction
	// (5.2) and stores its own hash.
	if err := cs2.SetChunkCopied(ctx, f.big, 1, chunk1[:], "chunks/big.bin/00002"); err != nil {
		t.Fatalf("SetChunkCopied #1: %v", err)
	}
	f.assertChunkState(t, f.big, 1, true, chunk1[:])
	f.assertFileCopied(t, f.big, true)

	// A re-copy with a nil hash re-asserts the stored anchor without
	// touching it (5.2's "verifies against that stored hash").
	if err := cs2.SetChunkCopied(ctx, f.big, 1, nil, "chunks/big.bin/00002"); err != nil {
		t.Fatalf("re-SetChunkCopied #1: %v", err)
	}
	f.assertChunkState(t, f.big, 1, true, chunk1[:])

	// Malformed hashes are refused before any write.
	if err := cs2.SetChunkCopied(ctx, f.big, 1, []byte("short"), "x"); err == nil {
		t.Fatal("a non-32-byte chunk hash must be refused")
	}
	if err := cs2.SetChunkCopied(ctx, 99999, 0, chunk0[:], "files/ghost"); err == nil {
		t.Fatal("SetChunkCopied for an unknown chunk must fail")
	}
}

func TestSetChunkHashStoreOnly(t *testing.T) {
	f := newCopyFixture(t)
	ctx := context.Background()
	cs := f.db.CopyStore(f.setID, 1)

	// Store-only: the hash lands WITHOUT any copied flag (the lazy
	// store surface; the writer's mark path stores through
	// SetChunkCopied instead).
	if err := cs.SetChunkHash(ctx, f.big, 0, chunk0[:]); err != nil {
		t.Fatalf("SetChunkHash: %v", err)
	}
	f.assertChunkState(t, f.big, 0, false, chunk0[:])
	// Idempotent re-store of the SAME anchor; a DIFFERENT one is a
	// loud error (the 4.2 reuse-budget protection).
	if err := cs.SetChunkHash(ctx, f.big, 0, chunk0[:]); err != nil {
		t.Fatalf("idempotent SetChunkHash: %v", err)
	}
	if err := cs.SetChunkHash(ctx, f.big, 0, chunk1[:]); err == nil {
		t.Fatal("overwriting a stored chunk hash must be refused")
	}
	if err := cs.SetChunkHash(ctx, 99999, 0, chunk0[:]); err == nil {
		t.Fatal("SetChunkHash for an unknown chunk must fail")
	}
	if err := cs.SetChunkHash(ctx, f.big, 0, []byte("no")); err == nil {
		t.Fatal("a malformed hash must be refused")
	}
}

func TestSetDirContentAndRecordEmbedded(t *testing.T) {
	f := newCopyFixture(t)
	ctx := context.Background()
	cs := f.db.CopyStore(f.setID, 1)

	var dirID int64
	if err := f.db.sql.QueryRowContext(ctx,
		`SELECT dir_id FROM directories WHERE set_id = ? AND relpath = 'empty'`, f.setID).Scan(&dirID); err != nil {
		t.Fatal(err)
	}
	if err := cs.SetDirContent(ctx, dirID, "files/empty"); err != nil {
		t.Fatalf("SetDirContent: %v", err)
	}
	kind, size, hash := f.imageContent(t, 1, "files/empty")
	if kind != "dir" || size != 0 || hash != nil {
		t.Fatalf("dir content row = (%s, %d, %x), want (dir, 0, NULL)", kind, size, hash)
	}
	if err := cs.SetDirContent(ctx, 99999, "files/ghost-dir"); err == nil {
		t.Fatal("SetDirContent for an unknown dir must fail")
	}

	// The 6.6 self-rows: manifest and export only; payload kinds are
	// refused here (they flow through the marks).
	mf := sha256.Sum256([]byte("{}"))
	if err := cs.RecordEmbedded(ctx, "manifest", EmbeddedContent{
		MediaPath: "MANIFEST.json", Size: 2, Hash: mf[:],
	}); err != nil {
		t.Fatalf("RecordEmbedded manifest: %v", err)
	}
	if err := cs.RecordEmbedded(ctx, "export", EmbeddedContent{
		MediaPath: "catalog-export.sqlite", Size: 4096, Hash: mf[:],
	}); err != nil {
		t.Fatalf("RecordEmbedded export: %v", err)
	}
	if k, _, _ := f.imageContent(t, 1, "MANIFEST.json"); k != "manifest" {
		t.Fatalf("manifest self-row kind = %q, want manifest", k)
	}
	if k, _, _ := f.imageContent(t, 1, "catalog-export.sqlite"); k != "export" {
		t.Fatalf("export self-row kind = %q, want export", k)
	}
	if err := cs.RecordEmbedded(ctx, "file", EmbeddedContent{MediaPath: "x", Size: 1, Hash: mf[:]}); err == nil {
		t.Fatal("RecordEmbedded must refuse payload kinds")
	}
	if err := cs.RecordEmbedded(ctx, "manifest", EmbeddedContent{MediaPath: "MANIFEST.json", Size: 1, Hash: nil}); err == nil {
		t.Fatal("RecordEmbedded must refuse a missing hash")
	}
}

// --- the whole-set reads -----------------------------------------------------

func TestCopyPlacementWholeSet(t *testing.T) {
	f := newCopyFixture(t)
	placement, err := f.db.CopyPlacement(context.Background(), f.setID)
	if err != nil {
		t.Fatalf("CopyPlacement: %v", err)
	}
	if len(placement) != 2 {
		t.Fatalf("placement images = %d, want 2", len(placement))
	}
	got1 := joinPaths(placement[0].Paths)
	// image 1: whole file, chunk (folded to the parent), dir, and BOTH
	// hard-link names (the secondary materializes next to its primary).
	if want := "big.bin,docs/a.txt,empty,fam-a,fam-b"; got1 != want {
		t.Fatalf("image 1 placement = %q, want %q", got1, want)
	}
	got2 := joinPaths(placement[1].Paths)
	// image 2: the symlink and its chunk of the parent.
	if want := "big.bin,link"; got2 != want {
		t.Fatalf("image 2 placement = %q, want %q", got2, want)
	}
}

func TestCopyHardLinkFamilies(t *testing.T) {
	f := newCopyFixture(t)
	fams, err := f.db.CopyHardLinkFamilies(context.Background(), f.setID)
	if err != nil {
		t.Fatalf("CopyHardLinkFamilies: %v", err)
	}
	if len(fams) != 1 {
		t.Fatalf("families = %d, want exactly the fam-a/fam-b family", len(fams))
	}
	if string(fams[0].Hash) != string(hashFam[:]) {
		t.Fatalf("family hash = %x, want the shared hash", fams[0].Hash)
	}
	if joinPaths(fams[0].Names) != "fam-a,fam-b" {
		t.Fatalf("family names = %v, want both names ascending", fams[0].Names)
	}
}

func TestCopyImageIDsAndSetSourceRoot(t *testing.T) {
	f := newCopyFixture(t)
	ctx := context.Background()
	ids, err := f.db.CopyImageIDs(ctx, f.setID)
	if err != nil || len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatalf("CopyImageIDs = %v (%v), want [1 2]", ids, err)
	}
	root, err := f.db.SetSourceRoot(ctx, f.setID)
	if err != nil || root != "/data" {
		t.Fatalf("SetSourceRoot = %q (%v), want /data", root, err)
	}
	if _, err := f.db.SetSourceRoot(ctx, "S-NOPE"); !errors.Is(err, ErrNoSuchSet) {
		t.Fatalf("unknown set error = %v, want ErrNoSuchSet", err)
	}
}

// --- the interrupted-run finalization closure --------------------------------

func TestStartCopyRunAndFinishRunDetached(t *testing.T) {
	f := newCopyFixture(t)
	ctx := context.Background()
	runID, err := f.db.StartCopyRun(ctx, CopyRun{
		SetID: f.setID, Root: "/mnt/backup", StartedAt: time.Now().UnixNano(),
	})
	if err != nil {
		t.Fatalf("StartCopyRun: %v", err)
	}
	if status := f.runStatus(t, runID); status != "running" {
		t.Fatalf("run status after start = %q, want running", status)
	}
	// THE DEFERRAL CLOSURE: the interrupted finalization succeeds with
	// NO caller context at all - it is detached by construction (fresh
	// Background-derived short-timeout ctx inside), which is exactly
	// what a cancelled run context could never achieve.
	if err := f.db.FinishRunDetached(runID, "interrupted", time.Now().UnixNano(),
		`{"copied":3}`); err != nil {
		t.Fatalf("FinishRunDetached: %v", err)
	}
	if status := f.runStatus(t, runID); status != "interrupted" {
		t.Fatalf("run status after detached finalize = %q, want interrupted", status)
	}
	// The kind is 'copy' and the root is the destination root (2.4).
	var kind, root string
	if err := f.db.sql.QueryRowContext(ctx,
		`SELECT kind, root FROM runs WHERE run_id = ?`, runID).Scan(&kind, &root); err != nil {
		t.Fatal(err)
	}
	if kind != "copy" || root != "/mnt/backup" {
		t.Fatalf("run row = (%s, %s), want (copy, /mnt/backup)", kind, root)
	}
	// Invalid statuses are rejected before the CHECK constraint.
	if err := f.db.FinishRunDetached(runID, "sparkled", time.Now().UnixNano(), ""); err == nil {
		t.Fatal("an invalid run status must be refused")
	}
	// An unknown set fails with the sentinel, not a raw FK error.
	if _, err := f.db.StartCopyRun(ctx, CopyRun{SetID: "S-NOPE", Root: "/x", StartedAt: 1}); !errors.Is(err, ErrNoSuchSet) {
		t.Fatalf("StartCopyRun unknown set error = %v, want ErrNoSuchSet", err)
	}
}

// --- helpers -----------------------------------------------------------------

func (f *copyFixture) assertFileCopied(t *testing.T, fileID int64, want bool) {
	t.Helper()
	var copied int
	if err := f.db.sql.QueryRowContext(context.Background(),
		`SELECT copied FROM files WHERE file_id = ? AND set_id = ?`, fileID, f.setID).
		Scan(&copied); err != nil {
		t.Fatalf("reading copied flag of file %d: %v", fileID, err)
	}
	if (copied != 0) != want {
		t.Fatalf("files.copied of file %d = %v, want %v", fileID, copied != 0, want)
	}
}

func (f *copyFixture) assertChunkState(t *testing.T, fileID int64, index int, wantCopied bool, wantHash []byte) {
	t.Helper()
	var copied int
	var hash []byte
	if err := f.db.sql.QueryRowContext(context.Background(),
		`SELECT copied, hash FROM chunks WHERE set_id = ? AND file_id = ? AND chunk_index = ?`,
		f.setID, fileID, index).Scan(&copied, &hash); err != nil {
		t.Fatalf("reading chunk %d of file %d: %v", index, fileID, err)
	}
	if (copied != 0) != wantCopied {
		t.Fatalf("chunks.copied of %d#%d = %v, want %v", fileID, index, copied != 0, wantCopied)
	}
	if string(hash) != string(wantHash) {
		t.Fatalf("chunks.hash of %d#%d = %x, want %x", fileID, index, hash, wantHash)
	}
}

func (f *copyFixture) imageContent(t *testing.T, imageID int64, mediaPath string) (kind string, size int64, hash []byte) {
	t.Helper()
	if err := f.db.sql.QueryRowContext(context.Background(),
		`SELECT kind, size, hash FROM image_contents
		 WHERE set_id = ? AND image_id = ? AND media_path = ?`,
		f.setID, imageID, mediaPath).Scan(&kind, &size, &hash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("no image_contents row for image %d path %q", imageID, mediaPath)
		}
		t.Fatalf("reading image_contents row %q: %v", mediaPath, err)
	}
	return kind, size, hash
}

func (f *copyFixture) runStatus(t *testing.T, runID int64) string {
	t.Helper()
	var status string
	if err := f.db.sql.QueryRowContext(context.Background(),
		`SELECT status FROM runs WHERE run_id = ?`, runID).Scan(&status); err != nil {
		t.Fatalf("reading run %d status: %v", runID, err)
	}
	return status
}

// installContentTrigger creates a BEFORE-INSERT trigger on
// image_contents that aborts any row whose media_path equals magic -
// the honest same-transaction proof: the chunk UPDATE runs first, then
// the content insert fails, and EVERYTHING must roll back together.
func (f *copyFixture) installContentTrigger(t *testing.T, magic string) {
	t.Helper()
	ddl := `CREATE TRIGGER test_magic_path BEFORE INSERT ON image_contents
	        BEGIN SELECT CASE WHEN NEW.media_path = '` + magic + `'
	        THEN RAISE(ABORT, 'injected content-row failure') END; END`
	if _, err := f.db.sql.ExecContext(context.Background(), ddl); err != nil {
		t.Fatalf("installing magic-path trigger: %v", err)
	}
}

func (f *copyFixture) dropContentTrigger(t *testing.T) {
	t.Helper()
	if _, err := f.db.sql.ExecContext(context.Background(),
		`DROP TRIGGER test_magic_path`); err != nil {
		t.Fatalf("dropping magic-path trigger: %v", err)
	}
}

func joinPaths(paths []string) string {
	out := ""
	for i, p := range paths {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}
