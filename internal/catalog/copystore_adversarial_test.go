package catalog

// T3.3 adversarial depth pass (the delegated verification brief): the
// two copy-mark contracts the base matrix left partially open.
//
//   - THE 5.2 MIRROR PROOF'S NEGATIVE HALF: the base matrix
//     (TestSetChunkCopiedMirrorInSameTransaction) proves the FIRST
//     chunk's transaction rolls everything back together; the trigger
//     here fires on the LAST chunk's mark - the only transaction that
//     can flip files.copied - proving the mirror cannot stand without
//     the last chunk's flag, hash, and content row committing with it
//     (a crash between two chunks' transactions leaves the mirror
//     honestly false until the last one commits, FR-041).
//   - SetChunkHash vs SetChunkCopied hash semantics, pinned side by
//     side: the standalone store NEVER overwrites (the 4.2 reuse
//     budget), while the mark's COALESCE re-asserts whatever verified
//     anchor the writer passes. The engine's retry path cannot hit the
//     refusal because both attempts capture identical bytes of a
//     stable source (copyengine_adversarial_test.go) and it never
//     routes through SetChunkHash.

import (
	"context"
	"testing"
)

// The LAST chunk's mark is the mirror transaction. Inject its content
// row failure AFTER the chunk UPDATE already ran: everything - the
// flag, the stored capture, the parent mirror, and the content row -
// must roll back together, and the parent mirror must be provably
// FALSE without the last chunk's flag. The clean re-mark then flips
// the mirror inside its own transaction.
func TestSetChunkCopiedLastChunkFailureLeavesMirrorFalse(t *testing.T) {
	f := newCopyFixture(t)
	ctx := context.Background()
	cs1 := f.db.CopyStore(f.setID, 1)
	cs2 := f.db.CopyStore(f.setID, 2)

	// Chunk 0 marks cleanly: one of two copied, mirror stays false.
	if err := cs1.SetChunkCopied(ctx, f.big, 0, chunk0[:], "chunks/big.bin/00001"); err != nil {
		t.Fatalf("SetChunkCopied #0: %v", err)
	}
	f.assertFileCopied(t, f.big, false)

	// THE CRASH BETWEEN THE LAST CHUNK'S MARK AND ITS COMMIT: the
	// trigger aborts the LAST chunk's transaction at the content-row
	// insert - after UPDATE chunks and after the mirror COUNT PROOF
	// already ran inside that transaction.
	f.installContentTrigger(t, "BOOM-LAST")
	if err := cs2.SetChunkCopied(ctx, f.big, 1, chunk1[:], "BOOM-LAST"); err == nil {
		t.Fatal("the last chunk's mark must fail when its content row fails")
	}
	f.dropContentTrigger(t)

	// The mirror cannot be set without the last chunk's flag: every
	// piece of the failed transaction rolled back together.
	f.assertChunkState(t, f.big, 1, false, nil)
	f.assertFileCopied(t, f.big, false)
	if f.contentRowExists(t, 2, "BOOM-LAST") {
		t.Fatal("a content row survived the failed last-chunk transaction")
	}
	// The completed chunk 0 is untouched by the sibling's rollback.
	f.assertChunkState(t, f.big, 0, true, chunk0[:])
	kind, _, _ := f.imageContent(t, 1, "chunks/big.bin/00001")
	if kind != "chunk" {
		t.Fatalf("chunk 0 content row kind = %q, want chunk", kind)
	}

	// The clean re-mark flips the mirror in its own transaction.
	if err := cs2.SetChunkCopied(ctx, f.big, 1, chunk1[:], "chunks/big.bin/00002"); err != nil {
		t.Fatalf("clean SetChunkCopied #1: %v", err)
	}
	f.assertChunkState(t, f.big, 1, true, chunk1[:])
	f.assertFileCopied(t, f.big, true)
}

// contentRowExists reports whether the image_contents row of
// (imageID, mediaPath) exists - the negative probe the trigger-rollback
// test needs (imageContent fails the test when the row is absent).
func (f *copyFixture) contentRowExists(t *testing.T, imageID int64, mediaPath string) bool {
	t.Helper()
	var one int
	err := f.db.sql.QueryRowContext(context.Background(),
		`SELECT 1 FROM image_contents WHERE set_id = ? AND image_id = ? AND media_path = ?`,
		f.setID, imageID, mediaPath).Scan(&one)
	return err == nil
}

// The two hash surfaces, pinned side by side:
//
//   - SetChunkHash (standalone lazy store): fills NULL only, is
//     idempotent for the SAME value, and REFUSES a differing one (the
//     4.2 re-plan reuse budget).
//   - SetChunkCopied (the writer's mark path): nil leaves the stored
//     hash untouched; a non-nil hash COALESCEs it unconditionally.
//     Characterization of the documented contract: the engine always
//     passes the anchor the read-back JUST verified against, which for
//     a re-copy IS the stored hash read from the same catalog - so the
//     overwrite can only ever re-assert an identical value in the
//     production path (a differing value would require a corrupt or
//     concurrently-modified catalog, which D-11's single-writer flock
//     excludes).
func TestSetChunkCopiedHashSemanticsPinned(t *testing.T) {
	f := newCopyFixture(t)
	ctx := context.Background()
	cs := f.db.CopyStore(f.setID, 1)

	// First write stores the capture with the mark.
	if err := cs.SetChunkCopied(ctx, f.big, 0, chunk0[:], "chunks/big.bin/00001"); err != nil {
		t.Fatalf("SetChunkCopied first write: %v", err)
	}
	f.assertChunkState(t, f.big, 0, true, chunk0[:])

	// nil: re-assert nothing, stored hash untouched.
	if err := cs.SetChunkCopied(ctx, f.big, 0, nil, "chunks/big.bin/00001"); err != nil {
		t.Fatalf("SetChunkCopied nil hash: %v", err)
	}
	f.assertChunkState(t, f.big, 0, true, chunk0[:])

	// The same non-nil value is an idempotent re-assert.
	if err := cs.SetChunkCopied(ctx, f.big, 0, chunk0[:], "chunks/big.bin/00001"); err != nil {
		t.Fatalf("SetChunkCopied same hash: %v", err)
	}
	f.assertChunkState(t, f.big, 0, true, chunk0[:])

	// CHARACTERIZATION: a different non-nil hash COALESCEs (overwrites)
	// - unlike SetChunkHash, which refuses. Pinned so a future change
	// to either surface is a conscious contract edit. The engine path
	// can never carry a differing value (see the doc comment).
	if err := cs.SetChunkCopied(ctx, f.big, 0, chunk1[:], "chunks/big.bin/00001"); err != nil {
		t.Fatalf("SetChunkCopied differing hash: %v", err)
	}
	f.assertChunkState(t, f.big, 0, true, chunk1[:])

	// The refusal half lives ONLY on the standalone surface.
	if err := cs.SetChunkCopied(ctx, f.big, 0, chunk0[:], "chunks/big.bin/00001"); err != nil {
		t.Fatalf("SetChunkCopied re-assert: %v", err)
	}
	if err := cs.SetChunkHash(ctx, f.big, 0, chunk0[:]); err != nil {
		t.Fatalf("SetChunkHash idempotent: %v", err)
	}
	if err := cs.SetChunkHash(ctx, f.big, 0, chunk1[:]); err == nil {
		t.Fatal("SetChunkHash must refuse overwriting a stored hash (the 4.2 reuse budget)")
	}
}
