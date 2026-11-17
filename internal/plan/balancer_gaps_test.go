package plan

// T2.2 adversarial follow-up: focused tests closing the audit gaps the
// hand-computed unit suite and the 12.5 property suite left open
// (DESIGN_SPEC 4.3 WFD reference, 4.3 empty-dir lowest-numbered edge on
// a FULL locked destination, 4.4 wasted-residual exact figure, the
// 4.3/4.6 partially-assigned chunk set - reuse placement + compute-time
// refusal + the write-path guard, 2.4 multi-name hard-link families,
// NB2 written-row full-replacement semantics, and dir determinism under
// id reordering). Every fixture here is small enough
// to hand-compute; the expected values are worked out in the comments.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// --- WFD vs best-fit/first-fit ------------------------------------------------------
//
// The original hand-computed fixture (60/40/30/30 over usable 100) does
// NOT distinguish worst-fit from best-fit or first-fit: every variant
// yields (1,1,2,2) there. This fixture does: after placing 70 and 40,
// two destinations are open with remainders 30 (image 1) and 60
// (image 2), and the next item (30) fits BOTH. WFD sends it to the
// destination with the MOST remaining capacity (image 2); best-fit and
// first-fit would send it to image 1 (the tighter - or first - fit).
// The chosen placement is therefore a direct behavioral proof that the
// balancer implements 4.3's worst-fit-decreasing, not a look-alike.

func TestBalancerWorstFitDecreasingReferencePlacement(t *testing.T) {
	// Hand-computed WFD reference, payload budget 1130 (each item
	// charges size + 512):
	//   items sorted desc: a=70, b=40, c=30
	//   a -> image 1 (rem 1130-582 = 548)
	//   b -> image 1 rem 548 < 552 -> open image 2 (rem 1130-552 = 578)
	//   c -> heap top is image 2 (rem 578 >= 542, worst fit) -> image 2 (rem 36)
	// Media: 2. Fills: image 1 = 70, image 2 = 70 (balanced, FR-007).
	// Best-fit/first-fit would put c on image 1 (fills 70/40).
	s := newFakeStore(testProfile(1130))
	addFile(s, 1, "data/a", 70)
	addFile(s, 2, "data/b", 40)
	addFile(s, 3, "data/c", 30)

	res, err := runPlan(t, s)
	if err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	if res.NewDestinations != 2 || len(s.images) != 2 {
		t.Fatalf("media count = %d (new %d), want 2", len(s.images), res.NewDestinations)
	}
	if imageOf(s, 1) != 1 || imageOf(s, 2) != 2 || imageOf(s, 3) != 2 {
		t.Errorf("placement = (%d %d %d), want the WFD reference (1 2 2); "+
			"(1 2 1) would indicate best-fit/first-fit, not worst-fit",
			imageOf(s, 1), imageOf(s, 2), imageOf(s, 3))
	}
	if st := s.stateOf(1); st.UsedBytes != 70 || st.ItemCount != 1 {
		t.Errorf("image 1 = %+v, want {70 bytes, 1 item}", st)
	}
	if st := s.stateOf(2); st.UsedBytes != 70 || st.ItemCount != 2 {
		t.Errorf("image 2 = %+v, want {70 bytes, 2 items} (the balanced split)", st)
	}
}

// --- empty dirs when the LOWEST destination is AT capacity ------------------
//
// 4.3 documents the judgment call: in an incremental run the dirs land
// on the LOWEST-numbered destination even when it is LOCKED. The
// 2026-10-20 capacity amendment adds the entry-charge concession: the
// builder counts empty dirs as ISO entries (6.3 step 5), so a fully
// packed lowest destination can no longer receive dir entries it
// cannot reserve - the pass falls forward to the LOWEST-NUMBERED
// destination that CAN absorb the charge (here: the empty locked
// image 2), keeping the plan buildable by construction.

func TestBalancerDirsAttachToLockedDestinationAtCapacity(t *testing.T) {
	// Payload budget 1200. Image 1: file of 688 charges exactly 1200
	// (688 + 512): AT capacity, zero entry slack. Image 2: locked and
	// EMPTY (full 1200 of slack). The dirs (2 * 512 = 1024) cannot fit
	// image 1, so they land on image 2 - the lowest that can absorb
	// them - and the new payload opens image 3 (locked destinations
	// are never in the open set, FR-029).
	s := newFakeStore(testProfile(1200))
	addLockedImage(s, 1, budgetUsable(1200))
	addLockedImage(s, 2, budgetUsable(1200))
	addFile(s, 1, "locked/full", 688) // image 1: charge 1200 == budget: AT capacity
	s.files[1].ImageID = 1
	s.dirs[7] = &fakeDir{relPath: "empty/a", active: true}
	s.dirs[8] = &fakeDir{relPath: "empty/b", active: true}
	addFile(s, 3, "new/payload", 50)

	res, err := runPlan(t, s)
	if err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	// The dirs skip the FULL locked destination 1 and attach to the
	// lowest destination whose reserved budget absorbs their entry
	// charges - the EMPTY locked destination 2.
	if s.dirs[7].imageID != 2 || s.dirs[8].imageID != 2 {
		t.Errorf("dirs landed on (%d, %d), want the lowest destination with entry slack: locked image 2",
			s.dirs[7].imageID, s.dirs[8].imageID)
	}
	if imageOf(s, 3) != 3 || res.NewDestinations != 1 {
		t.Errorf("new payload file landed on image %d (new destinations %d), want new image 3",
			imageOf(s, 3), res.NewDestinations)
	}
	// Capacity-safe: the full destination kept its fill and gained
	// ZERO dir entries (the builder's 6.3 entry accounting stays
	// satisfiable there).
	st := s.stateOf(1)
	if st.UsedBytes != 688 || st.DirCount != 0 {
		t.Errorf("locked image 1 fill = %+v, want {688 bytes, 0 dirs} (full destinations gain no dir entries)", st)
	}
	// The result reports the same state: image 2 locked line with 2 dirs.
	var line *DestFill
	for i := range res.Destinations {
		if res.Destinations[i].ImageID == 2 {
			line = &res.Destinations[i]
		}
	}
	if line == nil || !line.Locked || line.DirCount != 2 || line.UsedBytes != 0 {
		t.Errorf("result line for image 2 = %+v, want a locked line with 0 bytes and 2 dirs", line)
	}
}

// --- dirs + files interplay in a FRESH plan ------------------------------------------
//
// The dirs pass is a separate deterministic sweep AFTER file placement:
// files pack by WFD first, then every active empty dir lands on the
// lowest-numbered destination of the final plan WHOSE RESERVED BUDGET
// absorbs the dirs' entry charges (the 2026-10-20 capacity twist) -
// which here is a NEW destination that also holds payload. Dirs must
// not perturb the WFD placement or the fills' byte accounting.

func TestBalancerFreshPlanDirsLandOnLowestPayloadDestination(t *testing.T) {
	// Payload budget 1124; WFD: 60 -> 1 (rem 548); 40 -> 1 (rem 0);
	// 30 -> 2 (rem 582). ONE dir (512) then sweeps onto image 2:
	// image 1 is exactly packed (zero entry slack), so the pass falls
	// forward to the lowest destination that can absorb the charge -
	// the payload-carrying image 2 (582 >= 512), NOT a fresh image.
	s := newFakeStore(testProfile(1124))
	addFile(s, 1, "data/a", 60)
	addFile(s, 2, "data/b", 40)
	addFile(s, 3, "data/c", 30)
	s.dirs[5] = &fakeDir{relPath: "empty/x", active: true}

	res, err := runPlan(t, s)
	if err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	if imageOf(s, 1) != 1 || imageOf(s, 2) != 1 || imageOf(s, 3) != 2 {
		t.Errorf("file placement = (%d %d %d), want the WFD (1 1 2) - dirs must not perturb it",
			imageOf(s, 1), imageOf(s, 2), imageOf(s, 3))
	}
	if s.dirs[5].imageID != 2 {
		t.Errorf("dir landed on image %d, want the lowest destination with entry slack (2)",
			s.dirs[5].imageID)
	}
	if st := s.stateOf(1); st.UsedBytes != 100 || st.ItemCount != 2 || st.DirCount != 0 {
		t.Errorf("image 1 = %+v, want {100 bytes, 2 items, 0 dirs}", st)
	}
	if st := s.stateOf(2); st.UsedBytes != 30 || st.ItemCount != 1 || st.DirCount != 1 {
		t.Errorf("image 2 = %+v, want {30 bytes, 1 item, 1 dir}", st)
	}
	if len(res.Destinations) != 2 || res.DirsAssigned != 1 {
		t.Errorf("result = (%d destinations, %d dirs), want (2, 1)", len(res.Destinations), res.DirsAssigned)
	}
}

// --- partially-assigned chunk set: REUSE is the product path, refusal is the
//     rewrite guard -------------------------------------------------------------
//
// RE-SCOPE WITH RATIONALE (not a weakening - design round-3 C.7):
// this block's PREMISE used to read "a partial set is reachable only
// by out-of-band corruption (raw SQL)", and its pinned contract was
// "any partial set aborts loudly". DESIGN_SPEC 4.6 falsifies that
// premise: the product's OWN `clear-assignments --image/--last`
// creates a partially-assigned chunk set whenever only part of a
// chunked file's chunks sat on the unlocked image - the {2,1,2}
// interleaved parent is the canonical shape. Pinning "any partial
// set aborts" would therefore pin the BUG (the headline unlock ->
// re-plan flow aborting on every set with an oversized file).
//
// The property under test is now the 4.3/4.6 pair:
//
//   - REUSE (sub-case A): an unchanged effective chunk size whose
//     stored decomposition verifies places ONLY the unassigned
//     chunks; the assigned half's rows, hashes, and marks are
//     byte-unchanged and it earns no item, no assignment, no re-stamp
//     (the write path's ActionReuse writes assignments only);
//   - REFUSAL (sub-case B + the whole-file / family branches): any
//     path that would REWRITE the locked half - a changed effective
//     chunk size (rechunk), a failed decomposition verify, a
//     whole-file fit (DropChunkState), or the hard-link family rule
//     abandoning the row - refuses at COMPUTE time with
//     ErrPartialChunkReplan naming the file, the locked image ids,
//     and the remedies, BEFORE any transaction opens (zero tx,
//     bit-identical store);
//   - DEFENSE-IN-DEPTH (the hand-built op): Balance can no longer
//     PRODUCE an op against an assigned row, so the mechanical
//     write-path guard is pinned directly - a hand-built chunkFileOp
//     still fails in Write (ReplaceAllChunks' ErrChunkRowAssigned
//     guard, catalog-tier sentinel) with a rollback, so that guard
//     cannot silently weaken underneath the compute-time check.
//
// The pre-re-scope test TestBalancerPartialChunkSetFailsLoudlyAndRollsBack
// (whose premise was "reachable only by out-of-band corruption") is
// SUPERSEDED by the three tests below:
// ...PlacesOnlyUnassignedChunksOnReuse (its sub-case A, inverted),
// ...RechunkStillRefusedWithRemedy (its sub-case B, moved to the
// compute layer) and ...HandBuiltOpOnAssignedChunkRowStillFailsWrite
// (its mechanical write assertion, RETAINED IN SHAPE - same refusal
// at Write with a rollback, but the row-count literal moved 5 -> 3
// with the re-scoped decomposition and the op is now built BY HAND
// instead of produced by Balance).
//
// (The same guards on the real SQLite surface are pinned in
// internal/catalog/planstore_assign_test.go and planstore_test.go;
// real-catalog rollback atomicity is pinned by
// TestPlanStorePlanTxRollbackLeavesCatalogUntouched.)

// TestBalancerPartialChunkSetPlacesOnlyUnassignedChunksOnReuse is
// sub-case A, INVERTED per C.7: Balance succeeds, Write succeeds
// (committed, not rolled back), ONLY the unassigned chunks receive
// assignments, and the assigned chunk's row - image_id, geometry,
// hash - is byte-unchanged (copied/verified_at live in the real
// catalog and are pinned at the catalog/CLI tiers: M8 + AC-24).
func TestBalancerPartialChunkSetPlacesOnlyUnassignedChunksOnReuse(t *testing.T) {
	// Payload budget 612 (stored chunk_size 100 == the effective size,
	// so the REUSE branch applies): 250 bytes decompose to 100+100+50,
	// all three WRITTEN (stored hashes), chunk 0 assigned to image 1
	// and chunks 1-2 unassigned - exactly the shape a scoped unlock of
	// the image that held chunks 1 and 2 leaves behind.
	s := newFakeStore(testProfile(612))
	addLockedImage(s, 1, budgetUsable(612))
	addFile(s, 1, "big/partial.img", 250)
	stampDecomposition(t, s, 1, 250, 100, 3)
	s.chunkRows[1][0].imageID = 1 // only chunk 0 assigned: PARTIAL set
	beforeRows := append([]fakeChunk(nil), s.chunkRows[1]...)
	// Sanity: the fixture really is a partial set (0 < assigned < total).
	assigned := 0
	for _, c := range s.chunkRows[1] {
		if c.imageID != 0 {
			assigned++
		}
	}
	if assigned == 0 || assigned == len(s.chunkRows[1]) {
		t.Fatalf("fixture assigned = %d of %d, want a partial chunk set",
			assigned, len(s.chunkRows[1]))
	}

	plan, err := Balance(context.Background(), s, s.profile)
	if err != nil {
		t.Fatalf("Balance over a partial set on the reuse path: %v (the 4.6 product path must succeed)", err)
	}
	// Result.ChunksAssigned counts the UNASSIGNED half only: the
	// assigned chunk earns no assignment (AC-24's count proof).
	if plan.Result.ChunksAssigned != 2 {
		t.Errorf("ChunksAssigned = %d, want 2 (only the unassigned half)", plan.Result.ChunksAssigned)
	}
	if plan.Result.RechunkedFiles != 0 || plan.Result.RehashedAtNextWrite != 0 {
		t.Errorf("reuse plan reports (rechunk %d, rehash %d), want (0, 0) - a reuse is neither",
			plan.Result.RechunkedFiles, plan.Result.RehashedAtNextWrite)
	}

	// The reuse path writes NOTHING beyond assignments: with
	// StoreChunkSize poisoned, the write must still succeed (it never
	// re-stamps a verified decomposition) - the writeChunkFile
	// ActionReuse verdict, pinned mechanically.
	s.failOn = "StoreChunkSize"
	if err := plan.Write(context.Background(), s.factory()); err != nil {
		t.Fatalf("Write over the reuse path (StoreChunkSize poisoned): %v", err)
	}
	if s.committed != 1 || s.rolledBack != 0 {
		t.Errorf("transaction discipline = (committed %d, rolled back %d), want (1, 0)",
			s.committed, s.rolledBack)
	}
	// ONLY the unassigned chunks moved: chunk 1 and 2 onto the new
	// destination, chunk 0 untouched.
	after := s.chunkRows[1]
	if len(after) != 3 {
		t.Fatalf("row count = %d, want the original 3 (a reuse replaces no row)", len(after))
	}
	if after[0] != beforeRows[0] {
		t.Errorf("locked half mutated:\nbefore %+v\nafter  %+v", beforeRows[0], after[0])
	}
	for _, idx := range []int{1, 2} {
		if after[idx].imageID == 0 {
			t.Errorf("unassigned chunk %d still unassigned after the write", idx)
		}
		if !after[idx].hasHash {
			t.Errorf("chunk %d lost its stored hash on the reuse path (4.2 contract)", idx)
		}
	}
	// The parent's stamp is byte-unchanged (no re-stamp on reuse).
	f := s.files[1]
	if !f.chunked || f.chunkCount != 3 || f.ChunkSize != 100 || f.ImageID != 0 {
		t.Errorf("parent after reuse = (chunked %v, count %d, size %d, image %d), want (true, 3, 100, 0)",
			f.chunked, f.chunkCount, f.ChunkSize, f.ImageID)
	}
}

// TestBalancerPartialChunkSetRechunkStillRefusedWithRemedy is
// sub-case B, KEPT but moved one layer earlier per C.7: with a
// changed chunk profile (stored 50 != effective 100) the refusal now
// happens in Balance - ErrPartialChunkReplan naming the file, the
// locked image ids and BOTH remedies, ZERO transactions, store
// digest unchanged (the old Write-level refusal is reproduced by the
// hand-built-op pin below - same Write-level refusal, with the pin's
// op now hand-built rather than Balance-produced).
func TestBalancerPartialChunkSetRechunkStillRefusedWithRemedy(t *testing.T) {
	s := newFakeStore(testProfile(612))
	addLockedImage(s, 1, budgetUsable(612))
	addFile(s, 1, "big/partial.img", 250)
	stampDecomposition(t, s, 1, 250, 50, 0) // 5 x 50, stamp 50 != effective 100
	s.chunkRows[1][0].imageID = 1
	before := s.digest()

	plan, err := Balance(context.Background(), s, s.profile)
	if !errors.Is(err, ErrPartialChunkReplan) {
		t.Fatalf("Balance = %v, want ErrPartialChunkReplan (changed chunk profile)", err)
	}
	if plan.Result.NewDestinations != 0 {
		t.Errorf("refused plan reports %d new destinations, want 0", plan.Result.NewDestinations)
	}
	// The message contract (4.3/4.6, AC-23): the file, the locked
	// image ids, and BOTH remedies.
	for _, want := range []string{
		"big/partial.img",
		"locked on image(s) 1",
		"the effective chunk size changed",
		remedyKeepProfile,
		remedyClearWholeSet,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message %q lacks %q", err.Error(), want)
		}
	}
	// Refusal BEFORE any transaction: zero tx, bit-identical store.
	if s.began != 0 {
		t.Errorf("refused plan opened %d transactions, want 0 (pre-tx refusal)", s.began)
	}
	if s.digest() != before {
		t.Errorf("refused plan mutated the store:\n--- before ---\n%s\n--- after ---\n%s",
			before, s.digest())
	}
	// The refused re-chunk deleted NOTHING: the surviving rows are
	// the original 5-row decomposition.
	if rows := s.chunkRows[1]; len(rows) != 5 {
		t.Errorf("refused re-chunk left %d rows, want the original 5", len(rows))
	}
}

// TestBalancerPartialChunkSetWholeFileTransitionRefused pins the
// second refusal branch: a partial set whose file now FITS whole (a
// profile change) would take the NB2 path - DropChunkState deletes
// every stored row, locked ones included - so the plan refuses at
// compute time with the remedies, zero transactions, store
// bit-identical.
func TestBalancerPartialChunkSetWholeFileTransitionRefused(t *testing.T) {
	// Budget 1000: the 250-byte file's 762-byte charge fits one
	// destination WHOLE (250 + Fixed + 512 <= Fixed + 1000), so the
	// whole-file branch - and with it NB2's row drop - is reached.
	s := newFakeStore(testProfile(1000))
	addLockedImage(s, 1, budgetUsable(1000))
	addFile(s, 1, "big/nowwhole.img", 250)
	stampDecomposition(t, s, 1, 250, 100, 1)
	s.chunkRows[1][0].imageID = 1
	before := s.digest()

	_, err := Balance(context.Background(), s, s.profile)
	if !errors.Is(err, ErrPartialChunkReplan) {
		t.Fatalf("Balance = %v, want ErrPartialChunkReplan (whole-file transition)", err)
	}
	for _, want := range []string{"big/nowwhole.img", "locked on image(s) 1",
		"now fits one destination", remedyKeepProfile, remedyClearWholeSet} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message %q lacks %q", err.Error(), want)
		}
	}
	if s.began != 0 {
		t.Errorf("refused plan opened %d transactions, want 0", s.began)
	}
	if s.digest() != before {
		t.Errorf("refused plan mutated the store:\n--- before ---\n%s\n--- after ---\n%s",
			before, s.digest())
	}
}

// TestBalancerPartialChunkSetSecondaryExcludedRefused pins the branch
// ORDER: the PARTIAL check runs BEFORE the secondary exclusion, so a
// partial set on a hard-link family row can never be silently left
// behind by the family rule - it refuses instead. Per design C.2 this
// branch carries the SECOND remedy only (clear the whole set): no
// profile choice can make the family rule place a secondary row.
func TestBalancerPartialChunkSetSecondaryExcludedRefused(t *testing.T) {
	s := newFakeStore(testProfile(612))
	addLockedImage(s, 1, budgetUsable(612))
	addFile(s, 1, "hl/primary", 40)
	addFile(s, 2, "hl/secondary", 250)
	for _, id := range []int64{1, 2} { // one family: primary = lowest file_id
		s.files[id].Dev, s.files[id].Inode, s.files[id].Nlink = 7, 9001, 2
	}
	stampDecomposition(t, s, 2, 250, 100, 0)
	s.chunkRows[2][0].imageID = 1 // the secondary carries a PARTIAL set
	before := s.digest()

	_, err := Balance(context.Background(), s, s.profile)
	if !errors.Is(err, ErrPartialChunkReplan) {
		t.Fatalf("Balance = %v, want ErrPartialChunkReplan (partial set hit the family rule)", err)
	}
	if !strings.Contains(err.Error(), "hl/secondary") ||
		!strings.Contains(err.Error(), "locked on image(s) 1") ||
		!strings.Contains(err.Error(), remedyClearWholeSet) {
		t.Errorf("refusal message %q lacks the file, the locked image id, or the remedy", err.Error())
	}
	// The branch remedy is clear-whole-set ONLY (design C.2).
	if strings.Contains(err.Error(), remedyKeepProfile) {
		t.Errorf("secondary-branch refusal carries the profile remedy too: %s "+
			"(design C.2: this branch offers clear-assignments (whole set) only)", err.Error())
	}
	if s.began != 0 {
		t.Errorf("refused plan opened %d transactions, want 0", s.began)
	}
	if s.digest() != before {
		t.Errorf("refused plan mutated the store:\n--- before ---\n%s\n--- after ---\n%s",
			before, s.digest())
	}
	// The primary was never placed either: the refusal aborts the
	// whole plan before placement.
	if imageOf(s, 1) != 0 {
		t.Errorf("primary placed on image %d despite the refusal", imageOf(s, 1))
	}
}

// TestBalancerHandBuiltOpOnAssignedChunkRowStillFailsWrite is C.7's
// direct WRITE-PATH pin: Balance can no longer produce an op that
// targets an assigned chunk row (the compute-time refusal owns that),
// so the test constructs planOps BY HAND - the test is in-package -
// and proves the mechanical guard behind it still refuses: a rechunk
// op whose assignments target an assigned row fails in Write with the
// assigned-row error, rolls back, commits nothing, and deletes no
// row. This is the old sub-case B mechanical assertion, preserved so
// ReplaceAllChunks' guard cannot silently weaken (its catalog-side
// sentinel is ErrChunkRowAssigned, pinned at the catalog tier; the
// fake mirrors the message).
func TestBalancerHandBuiltOpOnAssignedChunkRowStillFailsWrite(t *testing.T) {
	s := newFakeStore(testProfile(612))
	addLockedImage(s, 1, budgetUsable(612))
	addFile(s, 1, "big/partial.img", 250)
	stampDecomposition(t, s, 1, 250, 100, 3)
	s.chunkRows[1][0].imageID = 1
	specs, err := planSpecs(1, 250, 100)
	if err != nil {
		t.Fatalf("planSpecs: %v", err)
	}
	hand := Plan{ops: planOps{chunkFiles: []chunkFileOp{{
		fileID: 1, relPath: "big/partial.img",
		action: ActionRechunk, effective: 100, specs: specs,
		assignments: []chunkAssign{{index: 0, imageID: 1}}, // the ASSIGNED row
	}}}}

	writeErr := hand.Write(context.Background(), s.factory())
	if writeErr == nil {
		t.Fatal("hand-built rechunk op over an assigned row returned nil, want a loud refusal")
	}
	if !strings.Contains(writeErr.Error(), "assigned") {
		t.Errorf("Write error = %v, want it to name the assigned row", writeErr)
	}
	if s.rolledBack == 0 || s.committed != 0 {
		t.Errorf("transaction discipline = (rolled back %d, committed %d), want (1, 0)",
			s.rolledBack, s.committed)
	}
	// The refused re-chunk deleted NOTHING.
	if rows := s.chunkRows[1]; len(rows) != 3 || rows[0].imageID != 1 {
		t.Errorf("refused write left rows %+v, want the original 3 with chunk 0 still on image 1", rows)
	}
}

// --- oversized symlink: pre-tx abort with ZERO writes --------------------------------
//
// A symlink's item size is len(target) (2.3); a target longer than the
// plan's usable capacity is unplaceable. 4.3's ErrItemTooLarge guard
// must abort BEFORE any transaction opens - asserted here both by the
// factory counter and by a bit-identical store digest.

func TestBalancerOversizedSymlinkAbortsWithZeroWrites(t *testing.T) {
	// Payload budget 1200; the symlink's len(target) 1300 exceeds the
	// whole one-image payload budget (1300 + 512 > 1200).
	s := newFakeStore(testProfile(1200))
	addFile(s, 1, "fine", 50)
	addLink(s, 2, "huge-link", 1300)
	before := s.digest()

	_, err := Balance(context.Background(), s, s.profile)
	if !errors.Is(err, ErrItemTooLarge) {
		t.Fatalf("Balance = %v, want ErrItemTooLarge", err)
	}
	if !strings.Contains(err.Error(), "huge-link") || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("ErrItemTooLarge message = %v, want it to name the symlink and its kind", err)
	}
	if s.began != 0 {
		t.Errorf("aborted plan opened %d transactions, want 0 (pre-tx abort)", s.began)
	}
	if s.digest() != before {
		t.Errorf("aborted plan mutated the catalog state:\n--- before ---\n%s\n--- after ---\n%s",
			before, s.digest())
	}
}

// --- hard-link families with THREE names (2.4 derivation) ----------------------------
//
// The family key is (dev, inode) over active rows with nlink > 1; the
// primary is the LOWEST file_id. A three-name family therefore yields
// TWO excluded secondaries and exactly ONE placement - the family's
// bytes are counted once (FR-039), and Result.SecondariesExcluded
// counts both names.

func TestBalancerHardLinkThreeNameFamily(t *testing.T) {
	// Case A: primary unassigned -> the primary is the single item;
	// BOTH secondaries are excluded.
	s := newFakeStore(testProfile(2000))
	for _, id := range []int64{1, 2, 3} {
		addFile(s, id, fmt.Sprintf("hl/name%d", id), 40)
		s.files[id].Dev, s.files[id].Inode, s.files[id].Nlink = 7, 9001, 3
	}
	res, err := runPlan(t, s)
	if err != nil {
		t.Fatalf("runPlan (case A): %v", err)
	}
	if imageOf(s, 1) == 0 {
		t.Error("family primary (lowest file_id) was not placed")
	}
	if imageOf(s, 2) != 0 || imageOf(s, 3) != 0 {
		t.Errorf("secondaries placed on (%d, %d), want both excluded (2.4)",
			imageOf(s, 2), imageOf(s, 3))
	}
	if res.SecondariesExcluded != 2 || res.FilesAssigned != 1 {
		t.Errorf("counts = (excluded %d, files %d), want (2, 1) - family counted once",
			res.SecondariesExcluded, res.FilesAssigned)
	}
	if st := s.stateOf(imageOf(s, 1)); st.UsedBytes != 40 {
		t.Errorf("destination holds %d bytes, want the family size counted once (40)", st.UsedBytes)
	}

	// Case B: primary LOCKED -> the family's bytes are already on
	// media; the two unassigned secondaries create NO items and NO
	// transaction.
	s2 := newFakeStore(testProfile(2000))
	addLockedImage(s2, 1, budgetUsable(2000))
	for _, id := range []int64{1, 2, 3} {
		addFile(s2, id, fmt.Sprintf("hl/name%d", id), 40)
		s2.files[id].Dev, s2.files[id].Inode, s2.files[id].Nlink = 7, 9001, 3
	}
	s2.files[1].ImageID = 1 // primary already on media
	res2, err := runPlan(t, s2)
	if err != nil {
		t.Fatalf("runPlan (case B): %v", err)
	}
	if imageOf(s2, 2) != 0 || imageOf(s2, 3) != 0 {
		t.Errorf("case B: secondaries placed on (%d, %d), want both excluded",
			imageOf(s2, 2), imageOf(s2, 3))
	}
	if res2.NewDestinations != 0 || s2.began != 0 {
		t.Errorf("case B: new destinations %d, transactions %d, want 0/0",
			res2.NewDestinations, s2.began)
	}
	if res2.SecondariesExcluded != 2 {
		t.Errorf("case B: SecondariesExcluded = %d, want 2", res2.SecondariesExcluded)
	}
}

// --- NB2 whole-file transition over WRITTEN chunk rows --------------------------------
//
// The NB2 transition (chunked under a small profile, whole under the
// current one) deletes the stale chunk rows INCLUDING rows that carry
// a stored hash and copied mark. That is correct full-replacement
// semantics per 4.2/4.5: clear-assignments retains rows+hashes
// because the PLACEMENT survives it, but NB2 deletes the placement
// itself - the stored hash described the OLD byte range of a row that
// no longer exists, and nothing on media references that row anymore.
// The whole-file placement carries its own files.hash, against which
// the copy engine re-verifies the file end-to-end (FR-014 step 5), so
// no verification data is lost. The re-chunk counters stay at zero:
// NB2 is a whole-file transition, NOT a re-chunk, and the plan report
// must not claim a re-hash cost for it.

func TestBalancerNB2WholeTransitionDropsWrittenChunkRows(t *testing.T) {
	// Payload budget 612 (effective chunk size 100), then a profile
	// switch to budget 1000 where the file's 762-byte charge fits
	// WHOLE.
	s := newFakeStore(testProfile(612))
	addFile(s, 1, "movie.iso", 250)
	stampDecomposition(t, s, 1, 250, 100, 1) // chunk 0 already written (hash stored)

	s.profile = testProfile(1000)
	res, err := runPlan(t, s)
	if err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	if imageOf(s, 1) == 0 {
		t.Fatal("file unassigned under the large profile")
	}
	if len(s.chunkRows[1]) != 0 {
		t.Errorf("file keeps %d stale chunk rows, want 0 - the WRITTEN row dies with its "+
			"old-boundary placement (4.2 full replacement; files.hash carries the "+
			"whole-file verification)", len(s.chunkRows[1]))
	}
	if s.files[1].chunked || s.files[1].ChunkSize != 0 || s.files[1].chunkCount != 0 {
		t.Errorf("file keeps the stale stamp (chunked=%v, count=%d, size=%d), want clean",
			s.files[1].chunked, s.files[1].chunkCount, s.files[1].ChunkSize)
	}
	if res.FilesAssigned != 1 || res.ChunksAssigned != 0 {
		t.Errorf("counts = (files %d, chunks %d), want (1, 0)", res.FilesAssigned, res.ChunksAssigned)
	}
	if res.RechunkedFiles != 0 || res.RehashedAtNextWrite != 0 {
		t.Errorf("NB2 plan reports (rechunk %d, rehash %d), want (0, 0) - "+
			"an NB2 whole transition is not a re-chunk and claims no re-hash cost",
			res.RechunkedFiles, res.RehashedAtNextWrite)
	}
}

// --- wasted-residual figure, hand-computed (4.4 / D-05, amended 2026-10-20) --------
//
// The 12.5 property suite RECOMPUTES the residual from the store;
// this fixture pins the EXACT bytes against hand arithmetic and pins
// the semantics: only LOCKED destinations contribute, only when the
// residual is positive, and the figure is the TRUE free space under
// the reserved-overhead model - usable - FixedImageOverhead - 512*
// entries - payload (4.4 consequence c: "the true free space that
// will remain on the built image").

func TestBalancerWastedResidualExactBytes(t *testing.T) {
	// Payload budget 1200. Locked image 1: file 70, one entry ->
	// residual 1200 - 512 - 70 = 618. Locked image 2: file 688, one
	// entry -> charge 1200 = budget, EXACTLY full -> residual 0, not
	// counted. Unassigned 40 + 10 (charges 552 + 522) both land on
	// NEW image 3 (locked destinations are never open), so the
	// residual is permanent (accept-waste, D-05) until clear +
	// re-plan.
	s := newFakeStore(testProfile(1200))
	addLockedImage(s, 1, budgetUsable(1200))
	addLockedImage(s, 2, budgetUsable(1200))
	addFile(s, 1, "locked/gap", 70)
	s.files[1].ImageID = 1
	addFile(s, 2, "locked/full", 688)
	s.files[2].ImageID = 2
	addFile(s, 3, "new/big", 40)
	addFile(s, 4, "new/small", 10)

	res, err := runPlan(t, s)
	if err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	if imageOf(s, 3) != 3 || imageOf(s, 4) != 3 || res.NewDestinations != 1 {
		t.Errorf("new files placed on (%d, %d) with %d new destinations, want (3, 3) and 1",
			imageOf(s, 3), imageOf(s, 4), res.NewDestinations)
	}
	if res.WastedResidualBytes != 618 {
		t.Errorf("WastedResidualBytes = %d, want the hand-computed 618 "+
			"(image 1: 1200 budget - 512 entry - 70 payload; image 2 contributes "+
			"nothing: its charge fills the budget exactly)",
			res.WastedResidualBytes)
	}
	if res.WastedResidualImages != 1 {
		t.Errorf("WastedResidualImages = %d, want 1 (only the gapped locked image)",
			res.WastedResidualImages)
	}
}

// --- determinism under dir-id reordering -----------------------------------------------
//
// Dir ids are catalog-local (autoincrement): the same LOGICAL set of
// empty dirs can arrive with entirely different ids depending on
// insertion history. The 4.3 sort key (dir_id ascending) fixes the
// ops order, but what must be deterministic is the PERSISTED OUTCOME:
// two stores holding the same relpaths under permuted ids must produce
// identical results and identical relpath -> destination mappings.

func TestBalancerDirIdReorderingIsDeterministic(t *testing.T) {
	build := func(dirIDs map[string]int64) *fakeStore {
		// Payload budget 1600: files 60 + 40 pack onto image 1 (rem
		// 1600 - 572 - 552 = 476), which cannot absorb the three dirs'
		// 1536 entry charge - they all fall forward to ONE fallback
		// destination (1600 >= 1536) deterministically.
		s := newFakeStore(testProfile(1600))
		addFile(s, 1, "data/a", 60)
		addFile(s, 2, "data/b", 40)
		for rel, id := range dirIDs {
			s.dirs[id] = &fakeDir{relPath: rel, active: true}
		}
		return s
	}
	rels := []string{"empty/x", "empty/y", "empty/z"}
	// Same logical dir set, three different id assignments.
	layouts := []map[string]int64{
		{"empty/x": 10, "empty/y": 11, "empty/z": 12},
		{"empty/x": 12, "empty/y": 10, "empty/z": 11},
		{"empty/x": 11, "empty/y": 12, "empty/z": 10},
	}
	var (
		refResult string
		refMap    map[string]int64
	)
	for i, layout := range layouts {
		s := build(layout)
		res, err := runPlan(t, s)
		if err != nil {
			t.Fatalf("layout %d: runPlan: %v", i, err)
		}
		if got := resultDigest(res); refResult == "" {
			refResult = got
		} else if got != refResult {
			t.Errorf("layout %d produced a different result:\n--- reference ---\n%s\n--- layout %d ---\n%s",
				i, refResult, i, got)
		}
		m := make(map[string]int64, len(rels))
		for _, d := range s.dirs {
			m[d.relPath] = d.imageID
		}
		for _, rel := range rels {
			if m[rel] != 2 {
				t.Errorf("layout %d: dir %q landed on image %d, want the fallback destination 2 "+
					"(image 1 is packed too tight for the dir entries)", i, rel, m[rel])
			}
		}
		if refMap == nil {
			refMap = m
		} else {
			for _, rel := range rels {
				if m[rel] != refMap[rel] {
					t.Errorf("layout %d: dir %q landed on image %d, reference layout on %d",
						i, rel, m[rel], refMap[rel])
				}
			}
		}
	}
}
