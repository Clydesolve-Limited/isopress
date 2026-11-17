package plan

// T6.3 fix-round divergence hunt: adversarial op shapes where the NEW
// fileID->position index (opsIndex) and the PRE-FIX linear scan could
// in principle produce DIFFERENT write ops. A divergence here is
// data-loss-grade wrongness (an assignment written against the wrong
// file, or silently dropped), so every shape is pinned to the golden
// oracle recordAssignmentLinear (balancer_fix_round_test.go) and the
// whole-Balance flow is checked against an independent oracle.
//
// Shapes covered (the QA brief's (a)-(f)):
//
//	(a) index STALENESS - ops mutated after indexOps. Proven
//	    impossible by code reading AND pinned here twice: the
//	    documented divergence signature (TestStaleIndexSignature...)
//	    plus a Balance-level completeness/attribution oracle
//	    (TestBalanceOpsMatchResultFills...) that fails if any op is
//	    ever missed or cross-wired;
//	(b) chunks of MANY parents interleaved and out of file_id order;
//	(c) re-assignment of the SAME file across different images,
//	    interleaved with other parents;
//	(d) symlink ops (the whole/symlink stamp branch) incl. dropStale;
//	(e) extreme fileIDs: 2^40, MaxInt64, NEGATIVE (corrupt-data)
//	    ids - the map accepts any int64 and the scan compares by ==,
//	    so both paths behave IDENTICALLY (find when the op exists,
//	    silent no-op when it does not);
//	(f) oracle fidelity: recordAssignmentLinear was mechanically
//	    diffed (code lines only) against `git show HEAD:
//	    internal/plan/balancer.go` (== fa8cafd, the last commit to
//	    touch the pre-fix recordAssignment) - IDENTICAL apart from
//	    the function name. The old recordAssignment was
//	    self-contained; no other historical behavior exists to
//	    transcribe.
//
// The ONE semantic delta between scan and index is DUPLICATE fileIDs
// within one slice (scan keeps the first match, the map keeps the
// last). It is unreachable through Balance: files.file_id is the
// table's INTEGER PRIMARY KEY (schema.go) and item construction takes
// exactly one branch per active file, so ops hold at most one op per
// file_id per kind. assertOpsIndexAgrees additionally FAILS on a
// duplicate, so a catalog that somehow violated the PK invariant
// could not slip through silently.

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"testing"
)

// --- (b)/(c)/(d): golden parity over adversarial sequences -----------

// placement is one record step of the golden replay.
type placement struct {
	it      placeItem
	imageID int64
}

// replayGolden feeds every placement through BOTH the indexed path and
// the pre-fix linear scan, asserting byte-identical ops after EACH
// step (so a failure names the exact step where the paths diverged)
// and index<->slice agreement at the end.
func replayGolden(t *testing.T, name string, build func() planOps, steps []placement) {
	t.Helper()
	indexed, scanned := build(), build()
	idx := indexOps(&indexed)
	for i, s := range steps {
		recordAssignment(&idx, s.it, s.imageID)
		recordAssignmentLinear(&scanned, s.it, s.imageID)
		if !reflect.DeepEqual(indexed, scanned) {
			t.Fatalf("%s: step %d (%+v -> image %d) diverged from the pre-fix linear scan\n"+
				"indexed: %+v\nscan:    %+v", name, i, s.it, s.imageID, indexed, scanned)
		}
	}
	assertOpsIndexAgrees(t, &indexed, &idx)
}

// interleavedOpsFixture: P chunked parents (1..4 specs each) PLUS one
// whole file per parent, fileIDs deliberately non-monotonic so the op
// slices' order has nothing to do with the placement order below.
func interleavedOpsFixture(parents int) planOps {
	var ops planOps
	for p := 0; p < parents; p++ {
		fid := int64(1000 + p*7)
		n := p%4 + 1
		specs := make([]ChunkSpec, 0, n)
		off := int64(0)
		for i := 0; i < n; i++ {
			l := int64(50 + i*10 + p) // parent-specific chunk lengths
			specs = append(specs, ChunkSpec{Index: i, Offset: off, Length: l})
			off += l
		}
		ops.chunkFiles = append(ops.chunkFiles, chunkFileOp{
			fileID: fid, relPath: fmt.Sprintf("p%02d.img", p),
			action: ActionCreateFresh, effective: off, specs: specs,
		})
		ops.wholeFiles = append(ops.wholeFiles, wholeFileOp{
			fileID: int64(500 + p), relPath: fmt.Sprintf("f%02d.bin", p),
		})
	}
	return ops
}

// TestRecordAssignmentGoldenInterleavedManyParents (shape b): chunks
// of MANY parents interleaved in every bad order - reverse parent
// order, reverse chunk-index order inside each parent, whole files
// stamped between two chunks of the SAME parent, a second
// out-of-order round that re-assigns already-assigned chunks across
// images, and cross-kind misses. Indexed must equal scan at every
// single step.
func TestRecordAssignmentGoldenInterleavedManyParents(t *testing.T) {
	const parents = 48
	steps := []placement{}

	// Round 1: parents in REVERSE file_id order, chunks in reverse
	// index order, whole file of this parent stamped BETWEEN two of
	// its chunks (so one parent's assignments see foreign imageIDs
	// interleaved), images rotating across 3 destinations.
	for p := parents - 1; p >= 0; p-- {
		fid := int64(1000 + p*7)
		n := p%4 + 1
		img := int64(1 + p%3)
		for i := n - 1; i >= 0; i-- {
			steps = append(steps, placement{
				it: placeItem{kind: itemChunk, relPath: fmt.Sprintf("p%02d.img", p),
					fileID: fid, chunkIndex: i, size: int64(50 + i*10 + p)},
				imageID: img,
			})
			if i == n-1 {
				// whole file stamped mid-parent-chunks
				steps = append(steps, placement{
					it: placeItem{kind: itemWhole, relPath: fmt.Sprintf("f%02d.bin", p),
						fileID: int64(500 + p), chunkIndex: -1, size: 60},
					imageID: int64(4),
				})
			}
		}
	}

	// Round 2: forward parent order, chunks in forward index order,
	// different image set - every chunk index gets a SECOND
	// assignment (the original appends without dedup) and every
	// whole file is re-assigned to another image. A cross-kind miss
	// (chunk item naming a WHOLE-file op) sprinkled in.
	for p := 0; p < parents; p++ {
		fid := int64(1000 + p*7)
		n := p%4 + 1
		img := int64(5 + p%2)
		for i := 0; i < n; i++ {
			steps = append(steps, placement{
				it: placeItem{kind: itemChunk, relPath: fmt.Sprintf("p%02d.img", p),
					fileID: fid, chunkIndex: i, size: int64(50 + i*10 + p)},
				imageID: img,
			})
		}
		steps = append(steps, placement{
			it: placeItem{kind: itemWhole, relPath: fmt.Sprintf("f%02d.bin", p),
				fileID: int64(500 + p), chunkIndex: -1, size: 60},
			imageID: int64(6),
		})
		if p%5 == 0 {
			steps = append(steps, placement{
				it:      placeItem{kind: itemChunk, relPath: "f00.bin", fileID: 500, chunkIndex: 0, size: 60},
				imageID: 7,
			})
		}
	}

	replayGolden(t, "interleaved-parents", func() planOps {
		return interleavedOpsFixture(parents)
	}, steps)
}

// TestRecordAssignmentGoldenReassignAcrossImages (shape c): the SAME
// file re-assigned to different images over time, interleaved with
// other parents, plus a whole-file op hit by a cross-kind chunk item
// (the original silently no-ops; the index must too). Pins both
// byte-parity AND the final semantic state (last stamp wins, append
// order preserved).
func TestRecordAssignmentGoldenReassignAcrossImages(t *testing.T) {
	build := func() planOps {
		return planOps{
			wholeFiles: []wholeFileOp{
				{fileID: 11, relPath: "a.bin"},
				{fileID: 22, relPath: "b.bin"},
			},
			chunkFiles: []chunkFileOp{
				{fileID: 33, relPath: "big.img", action: ActionCreateFresh, effective: 150,
					specs: []ChunkSpec{
						{Index: 0, Offset: 0, Length: 100},
						{Index: 1, Offset: 100, Length: 50},
					}},
			},
		}
	}
	steps := []placement{
		// file 11 -> image 1
		{placeItem{kind: itemWhole, relPath: "a.bin", fileID: 11, chunkIndex: -1, size: 60}, 1},
		// chunk of another parent lands between the two stamps of 11
		{placeItem{kind: itemChunk, relPath: "big.img", fileID: 33, chunkIndex: 1, size: 50}, 2},
		// file 11 RE-assigned to a different image
		{placeItem{kind: itemWhole, relPath: "a.bin", fileID: 11, chunkIndex: -1, size: 60}, 9},
		// file 22 -> image 3
		{placeItem{kind: itemWhole, relPath: "b.bin", fileID: 22, chunkIndex: -1, size: 60}, 3},
		// chunk 0 of 33 -> image 2, then chunk 0 AGAIN -> image 7
		// (duplicate assignment of the same index, other image)
		{placeItem{kind: itemChunk, relPath: "big.img", fileID: 33, chunkIndex: 0, size: 100}, 2},
		{placeItem{kind: itemWhole, relPath: "a.bin", fileID: 11, chunkIndex: -1, size: 60}, 4},
		{placeItem{kind: itemChunk, relPath: "big.img", fileID: 33, chunkIndex: 0, size: 100}, 7},
		// cross-kind: chunk item naming file 11 (a WHOLE-file op)
		// and whole item naming 33 (a CHUNKED op) - both silent
		// no-ops in the original scan; the index must match.
		{placeItem{kind: itemChunk, relPath: "a.bin", fileID: 11, chunkIndex: 0, size: 60}, 5},
		{placeItem{kind: itemWhole, relPath: "big.img", fileID: 33, chunkIndex: -1, size: 150}, 5},
	}

	indexed, scanned := build(), build()
	idx := indexOps(&indexed)
	for i, s := range steps {
		recordAssignment(&idx, s.it, s.imageID)
		recordAssignmentLinear(&scanned, s.it, s.imageID)
		if !reflect.DeepEqual(indexed, scanned) {
			t.Fatalf("reassign step %d diverged\nindexed: %+v\nscan: %+v", i, indexed, scanned)
		}
	}
	assertOpsIndexAgrees(t, &indexed, &idx)

	// Final semantics (not just parity): LAST stamp wins for the
	// whole branch; the chunk branch appended in PLACEMENT order
	// with no dedup; cross-kind misses wrote nothing.
	pos11 := idx.whole[11]
	if got := indexed.wholeFiles[pos11].imageID; got != 4 {
		t.Errorf("file 11 final image = %d, want the last stamp 4", got)
	}
	pos22 := idx.whole[22]
	if got := indexed.wholeFiles[pos22].imageID; got != 3 {
		t.Errorf("file 22 final image = %d, want 3", got)
	}
	pos33 := idx.chunk[33]
	wantAssigns := []chunkAssign{{1, 2}, {0, 2}, {0, 7}}
	if got := indexed.chunkFiles[pos33].assignments; !reflect.DeepEqual(got, wantAssigns) {
		t.Errorf("file 33 assignments = %+v, want placement order %+v", got, wantAssigns)
	}
}

// TestRecordAssignmentGoldenSymlinkStampBranch (shape d): symlink
// items go through the DEFAULT (whole) branch - stamp, not append -
// and must preserve the op's other fields (dropStale, relPath) byte
// for byte; a symlink item naming a CHUNKED op is a cross-kind miss.
func TestRecordAssignmentGoldenSymlinkStampBranch(t *testing.T) {
	build := func() planOps {
		return planOps{
			wholeFiles: []wholeFileOp{
				{fileID: 1, relPath: "link1", dropStale: false},
				{fileID: 2, relPath: "link2", dropStale: true}, // NB2: was chunked before
			},
			chunkFiles: []chunkFileOp{
				{fileID: 3, relPath: "big.img", action: ActionRechunk, effective: 100,
					specs: []ChunkSpec{{Index: 0, Offset: 0, Length: 100}}},
			},
		}
	}
	steps := []placement{
		{placeItem{kind: itemSymlink, relPath: "link1", fileID: 1, chunkIndex: -1, size: 8}, 5},
		{placeItem{kind: itemSymlink, relPath: "link2", fileID: 2, chunkIndex: -1, size: 3}, 6},
		// cross-kind miss: symlink item for a chunked file's id
		{placeItem{kind: itemSymlink, relPath: "big.img", fileID: 3, chunkIndex: -1, size: 8}, 7},
		// re-stamp of a symlink across images
		{placeItem{kind: itemSymlink, relPath: "link1", fileID: 1, chunkIndex: -1, size: 8}, 2},
	}
	indexed, scanned := build(), build()
	idx := indexOps(&indexed)
	for i, s := range steps {
		recordAssignment(&idx, s.it, s.imageID)
		recordAssignmentLinear(&scanned, s.it, s.imageID)
		if !reflect.DeepEqual(indexed, scanned) {
			t.Fatalf("symlink step %d diverged\nindexed: %+v\nscan: %+v", i, indexed, scanned)
		}
	}
	assertOpsIndexAgrees(t, &indexed, &idx)

	// Semantic pins: stamped (one imageID each, no assignments list
	// exists on whole ops), relPath/dropStale untouched, cross-kind
	// miss did not stamp file 3's chunk op.
	if got := indexed.wholeFiles[idx.whole[1]].imageID; got != 2 {
		t.Errorf("link1 image = %d, want the re-stamp 2", got)
	}
	if got := indexed.wholeFiles[idx.whole[2]]; got.imageID != 6 || !got.dropStale || got.relPath != "link2" {
		t.Errorf("link2 op = %+v, want image 6 with dropStale and relPath preserved", got)
	}
	if indexed.chunkFiles[idx.chunk[3]].assignments != nil {
		t.Errorf("symlink cross-kind miss wrote chunk assignments: %+v",
			indexed.chunkFiles[idx.chunk[3]].assignments)
	}
}

// TestRecordAssignmentGoldenExtremeFileIDs (shape e): fileIDs at the
// int64 edges - 2^40, MaxInt64, ZERO and NEGATIVE (impossible via the
// schema's INTEGER PRIMARY KEY, but the map takes any int64 and the
// scan compares by ==). Both paths must behave IDENTICALLY: find an
// op that exists (stamping works even at MinInt64/MaxInt64), silent
// no-op for an id with no op (the corrupt-data miss case - note the
// miss comes from the ABSENT op, not from the id being negative; a
// negative id that IS in the slices is found by both paths).
func TestRecordAssignmentGoldenExtremeFileIDs(t *testing.T) {
	edgeIDs := []int64{math.MinInt64, -7, 0, 1, 1 << 40, math.MaxInt64}
	build := func() planOps {
		var ops planOps
		for _, id := range edgeIDs {
			ops.wholeFiles = append(ops.wholeFiles, wholeFileOp{fileID: id, relPath: fmt.Sprintf("w/%d", id)})
		}
		for _, id := range edgeIDs {
			ops.chunkFiles = append(ops.chunkFiles, chunkFileOp{
				fileID: id, relPath: fmt.Sprintf("c/%d", id),
				action: ActionCreateFresh, effective: 10,
				specs: []ChunkSpec{{Index: 0, Offset: 0, Length: 10}},
			})
		}
		return ops
	}
	var steps []placement
	img := int64(1)
	for _, id := range edgeIDs {
		steps = append(steps, placement{
			it: placeItem{kind: itemWhole, relPath: fmt.Sprintf("w/%d", id), fileID: id, chunkIndex: -1, size: 5}, imageID: img})
		steps = append(steps, placement{
			it: placeItem{kind: itemChunk, relPath: fmt.Sprintf("c/%d", id), fileID: id, chunkIndex: 0, size: 10}, imageID: img})
		img++
	}
	// ids with NO op at all - both paths must silently no-op:
	// MinInt64-adjacent unrepresentable ids and plain unknowns.
	for _, id := range []int64{math.MinInt64 + 1, -424242, 999, math.MaxInt64 - 1} {
		steps = append(steps, placement{
			it: placeItem{kind: itemWhole, relPath: "ghost", fileID: id, chunkIndex: -1, size: 1}, imageID: 1})
		steps = append(steps, placement{
			it: placeItem{kind: itemChunk, relPath: "ghost", fileID: id, chunkIndex: 0, size: 1}, imageID: 1})
	}

	replayGolden(t, "extreme-fileIDs", build, steps)

	// And the edge ids WITH ops were genuinely stamped (a silently
	// dropped lookup would still pass a pure parity check if both
	// paths dropped it - so assert the positive case explicitly).
	ops := build()
	idx := indexOps(&ops)
	for i, id := range edgeIDs {
		recordAssignment(&idx, placeItem{kind: itemWhole, relPath: "", fileID: id, chunkIndex: -1}, int64(i+100))
		recordAssignment(&idx, placeItem{kind: itemChunk, relPath: "", fileID: id, chunkIndex: 0}, int64(i+100))
		if got := ops.wholeFiles[idx.whole[id]].imageID; got != int64(i+100) {
			t.Errorf("edge id %d whole op not stamped (image %d)", id, got)
		}
		if got := ops.chunkFiles[idx.chunk[id]].assignments; len(got) != 1 || got[0].imageID != int64(i+100) {
			t.Errorf("edge id %d chunk op assignments = %+v, want one entry", id, got)
		}
	}
}

// TestStaleIndexSignaturePostIndexAppendDiverges (shape a, the
// signature): IF ops were ever appended to after indexOps (the
// staleness failure mode), the stale index silently DROPS the
// assignment while the pre-fix scan finds it - no panic, no error,
// just a missing image_id on the written plan. This test pins that
// exact divergence so the invariant's teeth are documented, and shows
// the detector: after such an append the index no longer agrees with
// its slices (len mismatch - the condition assertOpsIndexAgrees keys
// on, which TestOpsIndexAgreesWithOpSlices runs over the real flow).
//
// Production prevents the shape (proved by code reading + the
// Balance-level oracle in TestBalanceOpsMatchResultFillsAndEveryOpRecorded):
//   - the ONLY appends to ops.wholeFiles/ops.chunkFiles are
//     balancer.go item construction (the whole/symlink and chunked
//     branches), both INSIDE the construction loop;
//   - indexOps is called AFTER that loop ends and after the two
//     file_id sorts - at the top of the placement loop;
//   - recordAssignment is the only function touching those slices
//     afterwards, and it mutates elements IN PLACE (no append, no
//     re-slice, no re-sort) - grep-verified single call site.
func TestStaleIndexSignaturePostIndexAppendDiverges(t *testing.T) {
	ops := planOps{
		wholeFiles: []wholeFileOp{
			{fileID: 1, relPath: "early.bin"},
			{fileID: 2, relPath: "also.bin"},
		},
		chunkFiles: []chunkFileOp{
			{fileID: 3, relPath: "big.img", action: ActionCreateFresh, effective: 100,
				specs: []ChunkSpec{{Index: 0, Offset: 0, Length: 100}}},
		},
	}
	idx := indexOps(&ops) // production order: index built once, here

	// The hypothetical post-index mutation (does NOT happen in
	// production - this is the adversarial shape itself):
	ops.wholeFiles = append(ops.wholeFiles, wholeFileOp{fileID: 9, relPath: "late.bin"})
	late := placeItem{kind: itemWhole, relPath: "late.bin", fileID: 9, chunkIndex: -1, size: 7}

	// Stale index: silent no-op, image stays 0.
	recordAssignment(&idx, late, 42)
	if got := ops.wholeFiles[2].imageID; got != 0 {
		t.Fatalf("stale index unexpectedly found the post-index op (image %d) - the divergence shape changed", got)
	}
	// The pre-fix scan (over the CURRENT slice) finds it.
	recordAssignmentLinear(&ops, late, 42)
	if got := ops.wholeFiles[2].imageID; got != 42 {
		t.Fatalf("linear scan missed the op - the oracle itself changed")
	}

	// Divergence observed (the data-loss shape), and the agreement
	// detector flags it: index count no longer matches slice count.
	if len(idx.whole) == len(ops.wholeFiles) {
		t.Fatal("index<->slice length mismatch not detectable - assertOpsIndexAgrees lost its staleness teeth")
	}
	if !reflect.DeepEqual(idx.whole, map[int64]int{1: 0, 2: 1}) {
		t.Errorf("index map drifted from the pre-append snapshot: %+v", idx.whole)
	}
}

// TestBalanceOpsMatchResultFillsAndEveryOpRecorded (shape a,
// end-to-end): the Balance-level oracle for index staleness and
// cross-wiring. It compares two INDEPENDENT accounts of the same
// plan:
//
//   - Result.Destinations fills: accumulated item-by-item INSIDE the
//     placement loop directly from placeItems (bypasses recordAssignment
//     and the index entirely);
//   - the write ops: attributed to images only THROUGH the index
//     (whole/chunk stamps) and the store's file sizes / op specs.
//
// If indexOps ever ran over stale, incomplete, pre-sort, or shifted
// slices, one of three things happens - all caught here: an op never
// gets stamped (completeness fail), an op gets ANOTHER file's image
// (per-image totals diverge from the fills), or the index misses an
// op entirely (completeness). Also asserts every chunk parent ends
// with EXACTLY its spec indices, each once, all assigned - a
// cross-wired lookup would double-stamp one parent and starve
// another.
//
// Fixture notes: several chunked parents with DISTINCT decompositions
// (distinct last-chunk lengths) interleaved with wholes/symlinks by
// file_id, plus edge file_ids (2^40, MaxInt64, NEGATIVE - see shape
// e) flowing through the REAL Balance path. No locked destinations in
// this fixture (locked fills are historical, not ops-attributed).
func TestBalanceOpsMatchResultFillsAndEveryOpRecorded(t *testing.T) {
	s := newFakeStore(testProfile(100000)) // payload budget 100000; chunks of 99488
	// Edge ids FIRST in construction order (map iteration in the fake
	// store sorts by id, so -7 arrives first, MaxInt64 last).
	addFile(s, -7, "neg-id.bin", 70)            // whole (corrupt-data id)
	addFile(s, 1, "a.bin", 60)                  // whole
	addFile(s, 2, "big1.img", 120000)           // chunks: 99488 + 20512
	addLink(s, 3, "link1", 10)                  // symlink -> whole op
	addFile(s, 4, "big2.img", 210000)           // chunks: 99488 + 99488 + 11024
	addFile(s, 5, "b.bin", 90)                  // whole
	addFile(s, 6, "big3.img", 150000)           // chunks: 99488 + 50512
	addFile(s, 7, "c.bin", 3)                   // whole
	addFile(s, 1<<40, "huge-id.bin", 55)        // whole, 2^40 id
	addLink(s, 9, "link2", 20)                  // symlink
	addFile(s, math.MaxInt64, "max-id.bin", 50) // whole, MaxInt64 id
	s.dirs[101] = &fakeDir{relPath: "empty/d1", active: true}
	s.dirs[102] = &fakeDir{relPath: "empty/d2", active: true}

	p, err := Balance(context.Background(), s, s.profile)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if len(p.ops.chunkFiles) != 3 {
		t.Fatalf("fixture shaped differently: %d chunked ops, want 3", len(p.ops.chunkFiles))
	}
	if len(p.ops.wholeFiles) != 8 { // 6 regular wholes + 2 symlinks
		t.Fatalf("fixture shaped differently: %d whole ops, want 8", len(p.ops.wholeFiles))
	}

	// --- completeness: every op was recorded (no stale-index miss) ---
	for _, op := range p.ops.wholeFiles {
		if op.imageID == 0 {
			t.Errorf("whole/symlink op %d (%s) never stamped: the index missed it (staleness!)", op.fileID, op.relPath)
		}
	}
	for _, op := range p.ops.chunkFiles {
		if len(op.assignments) != len(op.specs) {
			t.Errorf("chunked op %d (%s): %d assignments for %d specs - a lookup miss or cross-wire",
				op.fileID, op.relPath, len(op.assignments), len(op.specs))
			continue
		}
		seen := make(map[int]bool, len(op.assignments))
		for _, a := range op.assignments {
			if a.imageID == 0 {
				t.Errorf("chunked op %d: chunk %d unassigned", op.fileID, a.index)
			}
			if a.index < 0 || a.index >= len(op.specs) {
				t.Errorf("chunked op %d: assignment index %d out of range [0,%d)", op.fileID, a.index, len(op.specs))
				continue
			}
			if seen[a.index] {
				t.Errorf("chunked op %d: chunk index %d assigned twice - cross-wired lookup", op.fileID, a.index)
			}
			seen[a.index] = true
		}
	}

	// --- attribution: ops-derived fills == placement-loop fills ---
	type fill struct {
		used  int64
		items int
		dirs  int
	}
	fromOps := map[int64]*fill{}
	fillOf := func(id int64) *fill {
		if f := fromOps[id]; f != nil {
			return f
		}
		f := &fill{}
		fromOps[id] = f
		return f
	}
	for _, op := range p.ops.wholeFiles {
		sf, ok := s.files[op.fileID]
		if !ok {
			t.Fatalf("whole op for file %d not in the store", op.fileID)
		}
		f := fillOf(op.imageID)
		f.used += sf.Size
		f.items++
	}
	for _, op := range p.ops.chunkFiles {
		for _, a := range op.assignments {
			f := fillOf(a.imageID)
			f.used += op.specs[a.index].Length
			f.items++
		}
	}
	for _, d := range p.ops.dirs {
		fillOf(d.imageID).dirs++
	}

	matched := 0
	for _, dest := range p.Result.Destinations {
		if dest.Locked {
			t.Fatalf("unexpected locked destination in this fixture: %+v", dest)
		}
		f := fromOps[dest.ImageID]
		if f == nil {
			t.Errorf("image %d has fills (used %d, items %d) but no op references it", dest.ImageID, dest.UsedBytes, dest.ItemCount)
			continue
		}
		if f.used != dest.UsedBytes || f.items != dest.ItemCount || f.dirs != dest.DirCount {
			t.Errorf("image %d attribution diverged: ops say (used %d, items %d, dirs %d), "+
				"placement loop says (used %d, items %d, dirs %d) - the index pointed at the wrong ops",
				dest.ImageID, f.used, f.items, f.dirs, dest.UsedBytes, dest.ItemCount, dest.DirCount)
			continue
		}
		matched++
	}
	if len(fromOps) != len(p.Result.Destinations) || matched != len(p.Result.Destinations) {
		t.Errorf("ops reference %d images, Result has %d, matched %d - a stale index stamps phantom or zero images",
			len(fromOps), len(p.Result.Destinations), matched)
	}

	// The index rebuilt over the FINAL ops still agrees with them
	// (both directions, duplicates flagged).
	idx := indexOps(&p.ops)
	assertOpsIndexAgrees(t, &p.ops, &idx)
}
