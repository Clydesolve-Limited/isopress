package plan

// T6.3 fix round (S6): unit pins for the two deferred balancer debts
// the 12.8 perf smoke surfaced, plus the comment-drift NBs it folded
// (comment-only, no tests needed for those).
//
//  1. recordAssignment is O(1) now (fileID -> position index). The
//     PRE-FIX linear scan is kept here verbatim as a behavioral
//     oracle: the indexed path must produce byte-identical ops for
//     every placement sequence, including the miss cases. The index
//     must also never diverge from the slices it points into (a
//     stale/phantom position would silently mis-assign).
//
//  2. The balancer loops honor ctx: a cancelled run aborts with a
//     ctx-anchored error (errors.Is context.Canceled) BEFORE any work
//     can be persisted - the transaction belongs to Plan.Write, which
//     a Balance error never reaches.
//
// Determinism note: the fix introduced maps for LOOKUP ONLY. Nothing
// in the placement path ranges them (that would leak Go's random map
// iteration order into a plan), and test 1 pins that the indexed write
// path emits exactly what the slice-scanning original emitted, in the
// same order.

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// --- ctx: a cancelled run aborts Balance ----------------------------

// cancelingStore cancels the run context when the LAST catalog read
// (Destinations) completes - the deterministic projection of a ^C
// landing after the load phase and before the first placement
// decision. The cancel is synchronous, inside a read the balancer
// itself invokes: no goroutines, no sleeps.
type cancelingStore struct {
	*fakeStore
	cancel context.CancelFunc
}

// Destinations shadows the embedded fakeStore read to fire the cancel.
func (c *cancelingStore) Destinations(ctx context.Context) ([]DestinationState, error) {
	c.cancel()
	return c.fakeStore.Destinations(ctx)
}

// TestBalancePreCanceledContextAbortsBeforeWork: a context that is
// already cancelled when Balance starts must fail with a ctx-anchored
// error, not with a plan - and, since Balance owns no writes, with no
// transaction ever opened.
func TestBalancePreCanceledContextAbortsBeforeWork(t *testing.T) {
	s := newFakeStore(testProfile(612))
	addFile(s, 1, "whole.bin", 60)
	addFile(s, 2, "big.img", 250) // oversized under this budget -> chunks
	s.dirs[9] = &fakeDir{relPath: "empty/d", active: true}
	before := s.digest()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	p, err := Balance(ctx, s, s.profile)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Balance(pre-cancelled) = %v, want a context.Canceled-anchored error", err)
	}
	if errors.Is(err, ErrItemTooLarge) || errors.Is(err, ErrInvalidProfile) {
		t.Errorf("cancellation misclassified as a plan sentinel: %v", err)
	}
	if !p.ops.empty() {
		t.Errorf("aborted Balance returned write ops: %+v", p.ops)
	}
	if s.began != 0 {
		t.Errorf("aborted Balance opened %d transactions (Balance must not write; Plan.Write owns the tx)", s.began)
	}
	if after := s.digest(); after != before {
		t.Errorf("cancelled Balance mutated the store:\n--- before ---\n%s--- after ---\n%s", before, after)
	}
}

// TestBalanceContextCanceledDuringLoadAbortsBeforePlacement: the
// signal landing AFTER the catalog reads but before the compute phase
// must still abort - and abort before anything is placed. The control
// leg proves the same fixture plans fully when the context stays live,
// so the aborted leg is a genuine early exit, not an empty fixture.
func TestBalanceContextCanceledDuringLoadAbortsBeforePlacement(t *testing.T) {
	build := func() *fakeStore {
		s := newFakeStore(testProfile(612))
		addFile(s, 1, "a.bin", 60)
		addFile(s, 2, "b.bin", 90)
		addFile(s, 3, "big.img", 250)
		s.dirs[9] = &fakeDir{relPath: "empty/d", active: true}
		return s
	}

	aborted := build()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p, err := Balance(ctx, &cancelingStore{fakeStore: aborted, cancel: cancel}, aborted.profile)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Balance(cancelled during Destinations) = %v, want a context.Canceled-anchored error", err)
	}
	if !p.ops.empty() || aborted.began != 0 {
		t.Errorf("abort after the load persisted work: ops empty=%v transactions=%d",
			p.ops.empty(), aborted.began)
	}

	// Control: identical fixture, live context -> a full plan.
	control := build()
	ok, err := Balance(context.Background(), control, control.profile)
	if err != nil {
		t.Fatalf("control Balance: %v", err)
	}
	if ok.Result.FilesAssigned != 2 || ok.Result.ChunksAssigned != 3 || ok.Result.DirsAssigned != 1 {
		t.Errorf("control plan = (files %d, chunks %d, dirs %d), want (2, 3, 1) - "+
			"the aborted leg must not be aborting an EMPTY workload",
			ok.Result.FilesAssigned, ok.Result.ChunksAssigned, ok.Result.DirsAssigned)
	}
}

// --- recordAssignment: indexed vs the pre-fix linear scan -----------

// recordAssignmentLinear is the PRE-FIX implementation (T6.3's O(n^2)
// linear scan), kept verbatim as the oracle for the indexed path. It
// is test-only; production calls recordAssignment.
func recordAssignmentLinear(ops *planOps, it placeItem, imageID int64) {
	switch it.kind {
	case itemChunk:
		for i := range ops.chunkFiles {
			if ops.chunkFiles[i].fileID == it.fileID {
				ops.chunkFiles[i].assignments = append(
					ops.chunkFiles[i].assignments,
					chunkAssign{index: it.chunkIndex, imageID: imageID})
				return
			}
		}
	default:
		for i := range ops.wholeFiles {
			if ops.wholeFiles[i].fileID == it.fileID {
				ops.wholeFiles[i].imageID = imageID
				return
			}
		}
	}
}

// TestRecordAssignmentIndexedMatchesLinearScan pins the O(1) fix to
// the exact pre-fix semantics: for every placement sequence - chunks,
// wholes, symlinks, re-assignment, cross-kind misses, and fileIDs with
// no op at all - the indexed path leaves ops byte-identical to the
// linear scan's.
func TestRecordAssignmentIndexedMatchesLinearScan(t *testing.T) {
	build := func() planOps {
		return planOps{
			wholeFiles: []wholeFileOp{
				{fileID: 1, relPath: "a.bin"},
				{fileID: 5, relPath: "link", dropStale: true},
				{fileID: 9, relPath: "z.bin"},
			},
			chunkFiles: []chunkFileOp{
				{fileID: 3, relPath: "big.img", action: ActionReuse, effective: 100,
					specs: []ChunkSpec{
						{Index: 0, Offset: 0, Length: 100},
						{Index: 1, Offset: 100, Length: 50},
					}},
				{fileID: 7, relPath: "other.img", action: ActionCreateFresh, effective: 100,
					specs: []ChunkSpec{{Index: 0, Offset: 0, Length: 100}}},
			},
			// Present but untouched by recordAssignment: the ops' other
			// slices must survive byte-identical too.
			dirs:   []dirOp{{dirID: 4, relPath: "empty/d", imageID: 1}},
			images: []ImageSpec{{ImageID: 1, NominalSize: 2000, UsableSize: 1000, MediaProfile: "disk"}},
		}
	}

	placements := []struct {
		it      placeItem
		imageID int64
	}{
		// Whole and symlink items (the default branch).
		{placeItem{kind: itemWhole, relPath: "a.bin", fileID: 1, chunkIndex: -1, size: 60}, 4},
		{placeItem{kind: itemSymlink, relPath: "link", fileID: 5, chunkIndex: -1, size: 10}, 1},
		{placeItem{kind: itemWhole, relPath: "z.bin", fileID: 9, chunkIndex: -1, size: 30}, 3},
		// Chunk items, deliberately OUT of chunk-index order and across
		// two parents (append order inside one parent matters).
		{placeItem{kind: itemChunk, relPath: "big.img", fileID: 3, chunkIndex: 1, size: 50}, 1},
		{placeItem{kind: itemChunk, relPath: "big.img", fileID: 3, chunkIndex: 0, size: 100}, 2},
		{placeItem{kind: itemChunk, relPath: "other.img", fileID: 7, chunkIndex: 0, size: 100}, 1},
		// Cross-kind misses: a chunk item naming a WHOLE-file op and
		// vice versa - both are silent no-ops in the original scan.
		{placeItem{kind: itemChunk, relPath: "a.bin", fileID: 1, chunkIndex: 0, size: 60}, 9},
		{placeItem{kind: itemWhole, relPath: "big.img", fileID: 3, chunkIndex: -1, size: 250}, 9},
		// FileIDs with no op at all (unreachable through Balance, but
		// the parity must hold anyway).
		{placeItem{kind: itemWhole, relPath: "ghost", fileID: 999, chunkIndex: -1, size: 1}, 9},
		{placeItem{kind: itemChunk, relPath: "ghost", fileID: 999, chunkIndex: 0, size: 1}, 9},
		// Re-assignment of an already-assigned file, and a second
		// assignment of an already-assigned chunk index: the original
		// overwrites / appends without dedup, so the index must too.
		{placeItem{kind: itemWhole, relPath: "a.bin", fileID: 1, chunkIndex: -1, size: 60}, 6},
		{placeItem{kind: itemChunk, relPath: "big.img", fileID: 3, chunkIndex: 0, size: 100}, 7},
	}

	indexed, scanned := build(), build()
	idx := indexOps(&indexed)
	for _, p := range placements {
		recordAssignment(&idx, p.it, p.imageID)
		recordAssignmentLinear(&scanned, p.it, p.imageID)
	}
	if !reflect.DeepEqual(indexed, scanned) {
		t.Fatalf("indexed recordAssignment diverged from the pre-fix linear scan\n"+
			"indexed: %+v\nscan:    %+v", indexed, scanned)
	}
	assertOpsIndexAgrees(t, &indexed, &idx)
}

// assertOpsIndexAgrees checks both directions of the index<->slice
// relationship, plus the uniqueness invariant the map relies on: every
// op sits at the position its fileID maps to, and every map entry
// points at a slot that really holds that fileID. A divergence here
// would silently assign the WRONG op (or none).
func assertOpsIndexAgrees(t *testing.T, ops *planOps, idx *opsIndex) {
	t.Helper()
	if idx == nil || idx.ops != ops {
		t.Fatal("index is not bound to the planOps it indexes")
	}

	// --- slice -> index ---
	seenWhole := make(map[int64]int, len(ops.wholeFiles))
	for i, op := range ops.wholeFiles {
		pos, ok := idx.whole[op.fileID]
		if !ok {
			t.Errorf("whole-file op %d (%s) missing from the index", op.fileID, op.relPath)
			continue
		}
		if pos != i {
			t.Errorf("whole-file op %d indexed at %d, want its slice position %d", op.fileID, pos, i)
		}
		if prev, dup := seenWhole[op.fileID]; dup {
			t.Errorf("duplicate whole-file op for file %d (positions %d and %d): "+
				"a map index silently keeps only one of them", op.fileID, prev, i)
		}
		seenWhole[op.fileID] = i
	}
	seenChunk := make(map[int64]int, len(ops.chunkFiles))
	for i, op := range ops.chunkFiles {
		pos, ok := idx.chunk[op.fileID]
		if !ok {
			t.Errorf("chunked-file op %d (%s) missing from the index", op.fileID, op.relPath)
			continue
		}
		if pos != i {
			t.Errorf("chunked-file op %d indexed at %d, want its slice position %d", op.fileID, pos, i)
		}
		if prev, dup := seenChunk[op.fileID]; dup {
			t.Errorf("duplicate chunked-file op for file %d (positions %d and %d): "+
				"a map index silently keeps only one of them", op.fileID, prev, i)
		}
		seenChunk[op.fileID] = i
	}

	// --- index -> slice (no phantom, stale, or mispointed entries) ---
	if len(idx.whole) != len(ops.wholeFiles) {
		t.Errorf("index holds %d whole entries, slice holds %d", len(idx.whole), len(ops.wholeFiles))
	}
	for fid, pos := range idx.whole {
		if pos < 0 || pos >= len(ops.wholeFiles) {
			t.Errorf("whole index[%d] = %d is out of range [0,%d)", fid, pos, len(ops.wholeFiles))
			continue
		}
		if got := ops.wholeFiles[pos].fileID; got != fid {
			t.Errorf("whole index[%d] = %d, but that slot holds file %d", fid, pos, got)
		}
	}
	if len(idx.chunk) != len(ops.chunkFiles) {
		t.Errorf("index holds %d chunk entries, slice holds %d", len(idx.chunk), len(ops.chunkFiles))
	}
	for fid, pos := range idx.chunk {
		if pos < 0 || pos >= len(ops.chunkFiles) {
			t.Errorf("chunk index[%d] = %d is out of range [0,%d)", fid, pos, len(ops.chunkFiles))
			continue
		}
		if got := ops.chunkFiles[pos].fileID; got != fid {
			t.Errorf("chunk index[%d] = %d, but that slot holds file %d", fid, pos, got)
		}
	}
}

// TestOpsIndexAgreesWithOpSlices drives the index through the PUBLIC
// flow: Balance builds the ops (the exact construction point the fix
// indexes), the index is rebuilt from them with the same constructor,
// then mutated through recordAssignment - the only write path that
// touches the indexed slices - and both sides must stay in agreement
// at every step.
func TestOpsIndexAgreesWithOpSlices(t *testing.T) {
	s := newFakeStore(testProfile(612))
	addFile(s, 1, "whole.bin", 60) // whole: 60 + 512 <= 612
	addLink(s, 2, "link", 10)      // symlink: never chunks
	addFile(s, 3, "big.img", 250)  // chunked: 100 + 100 + 50
	addFile(s, 4, "second.bin", 90)
	s.dirs[9] = &fakeDir{relPath: "empty/d", active: true}

	p, err := Balance(context.Background(), s, s.profile)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if len(p.ops.wholeFiles) != 3 || len(p.ops.chunkFiles) != 1 {
		t.Fatalf("fixture shaped differently than the test expects: %d whole, %d chunked ops",
			len(p.ops.wholeFiles), len(p.ops.chunkFiles))
	}

	// The public flow's ops are indexed by the same constructor the
	// placement loop uses.
	idx := indexOps(&p.ops)
	assertOpsIndexAgrees(t, &p.ops, &idx)

	// Mutate through the real record path: re-assign a whole file,
	// add a second assignment for an already-assigned chunk index,
	// and fire a miss (no op for that fileID).
	recordAssignment(&idx, placeItem{kind: itemWhole, relPath: "second.bin", fileID: 4, chunkIndex: -1, size: 90}, 77)
	recordAssignment(&idx, placeItem{kind: itemChunk, relPath: "big.img", fileID: 3, chunkIndex: 0, size: 100}, 88)
	recordAssignment(&idx, placeItem{kind: itemWhole, relPath: "ghost", fileID: 999, chunkIndex: -1}, 5)
	assertOpsIndexAgrees(t, &p.ops, &idx)

	// The mutations landed exactly where the index pointed.
	if pos, ok := idx.whole[4]; !ok || p.ops.wholeFiles[pos].imageID != 77 {
		t.Errorf("re-assignment did not land on file 4's op (index ok=%v)", ok)
	}
	chunkPos, ok := idx.chunk[3]
	if !ok {
		t.Fatal("chunked file 3 lost from the index")
	}
	got := p.ops.chunkFiles[chunkPos].assignments
	if len(got) != 4 || got[3] != (chunkAssign{index: 0, imageID: 88}) {
		t.Errorf("chunk assignments = %+v, want the 3 from Balance plus {0 88}", got)
	}
	// The miss wrote nothing anywhere.
	for _, op := range p.ops.wholeFiles {
		if op.fileID == 999 {
			t.Errorf("phantom op created for an unknown fileID: %+v", op)
		}
	}
}
