package plan

// T2.2: focused balancer unit tests complementing the 12.5 property
// suite in balancer_test.go: the hand-computed WFD example, the
// incremental/locked semantics (FR-011/FR-029), the zero-unassigned
// no-op, the empty-dir lowest-numbered rule (including the dirs-only
// and incremental-locked variants), hard-link family reduction (2.4),
// symlink items, input validation, the three chunk actions with the
// 4.2 reuse/re-hash contracts, the reuse integrity-edge fallback, the
// focused NB2 whole-transition, write-failure rollback, and the
// metadata-only guarantees (AST import allowlist + mode-0000 flow).

import (
	"context"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"isopress/internal/units"
)

// --- fixture helpers -------------------------------------------------------------

// testProfile builds a disk-profile plan input whose one-image PAYLOAD
// budget equals budget: usable = units.FixedImageOverhead + budget.
// Under the 2026-10-20 capacity model (DESIGN_SPEC 4.3 capacity
// accounting) an image opens with usable - FixedImageOverhead =
// budget bytes of remaining capacity and every placement charges
// size + units.ImageEntryOverhead (512), so the hand-computed fixtures
// in this package reason in "budget" terms with the entry charges
// absorbed like any other cost. Nominal is kept above usable to
// satisfy the D-06 validation.
func testProfile(budget int64) units.Profile {
	usable := budgetUsable(budget)
	return units.Profile{Name: "disk", NominalBytes: 2 * usable, UsableBytes: usable}
}

// budgetUsable renders the usable_size whose payload budget is
// budget (the fixed overhead reserved on top).
func budgetUsable(budget int64) int64 {
	return units.FixedImageOverhead + budget
}

// addFile inserts one unassigned regular file and returns its id.
func addFile(s *fakeStore, id int64, rel string, size int64) {
	s.files[id] = &fakeFile{ActiveFile: ActiveFile{
		FileID: id, RelPath: rel, EntryKind: "regular", Size: size,
		Dev: 42, Inode: 1000 + id, Nlink: 1,
	}}
}

// addLink inserts one unassigned symlink of len(target) bytes.
func addLink(s *fakeStore, id int64, rel string, size int64) {
	s.files[id] = &fakeFile{ActiveFile: ActiveFile{
		FileID: id, RelPath: rel, EntryKind: "symlink", Size: size, Nlink: 1,
	}}
}

// imageOf returns a whole file's destination (0 = unassigned).
func imageOf(s *fakeStore, id int64) int64 { return s.files[id].ImageID }

// addLockedImage inserts an existing (locked) destination.
func addLockedImage(s *fakeStore, id, usable int64) {
	s.images[id] = fakeImage{spec: ImageSpec{
		ImageID: id, NominalSize: 2 * usable, UsableSize: usable, MediaProfile: "disk",
	}, state: "planned"}
}

// stampDecomposition stores a committed chunk decomposition (the
// previously-chunked state) on the store.
func stampDecomposition(t *testing.T, s *fakeStore, fileID, size, chunkSize int64, written int) {
	t.Helper()
	plan, err := PlanChunks(fileID, size, chunkSize)
	if err != nil {
		t.Fatalf("PlanChunks(%d, %d): %v", size, chunkSize, err)
	}
	rows := insertFakeRows(plan.Chunks)
	for i := 0; i < written && i < len(rows); i++ {
		rows[i].hasHash = true
	}
	s.chunkRows[fileID] = rows
	f := s.files[fileID]
	f.chunked = true
	f.chunkCount = len(rows)
	f.ChunkSize = chunkSize
}

// --- WFD placement and determinism ------------------------------------------------

func TestBalancerHandComputedWorstFitDecreasing(t *testing.T) {
	// Payload budget 1200; items sorted 60, 40, 30, 30 (descending
	// size), each charging size + 512. WFD: 60 -> image 1 (rem 628);
	// 40 -> image 1 (rem 76); 30 -> image 2 (rem 658); 30 -> image 2
	// (rem 116). Same placement shape as the pre-amendment fixture
	// (60+40 and 30+30), with the entry charges inside the budget.
	s := newFakeStore(testProfile(1200))
	addFile(s, 1, "a", 60)
	addFile(s, 2, "b", 40)
	addFile(s, 3, "c", 30)
	addFile(s, 4, "d", 30)

	res, err := runPlan(t, s)
	if err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	if res.NewDestinations != 2 || len(s.images) != 2 {
		t.Fatalf("media count = %d (new %d), want 2", len(s.images), res.NewDestinations)
	}
	if imageOf(s, 1) != 1 || imageOf(s, 2) != 1 || imageOf(s, 3) != 2 || imageOf(s, 4) != 2 {
		t.Errorf("placement = (%d %d %d %d), want (1 1 2 2)",
			imageOf(s, 1), imageOf(s, 2), imageOf(s, 3), imageOf(s, 4))
	}
	if st := s.stateOf(1); st.UsedBytes != 100 {
		t.Errorf("image 1 holds %d bytes, want the full 100", st.UsedBytes)
	}
	if st := s.stateOf(2); st.UsedBytes != 60 {
		t.Errorf("image 2 holds %d bytes, want 60", st.UsedBytes)
	}
}

func TestBalancerRelpathTieBreakIsDeterministic(t *testing.T) {
	// Equal sizes tie-break by ascending relpath (4.3). With a payload
	// budget of exactly one item-charge (512 + 10 = 522) and three
	// size-10 items, each item opens its own destination, so the tie
	// order IS the placement: alpha -> 1, mid -> 2, zeta -> 3 -
	// regardless of how file ids happen to be assigned.
	build := func(relByID map[int64]string) *fakeStore {
		s := newFakeStore(testProfile(522))
		for id, rel := range relByID {
			addFile(s, id, rel, 10)
		}
		return s
	}
	placements := func(t *testing.T, s *fakeStore) map[string]int64 {
		t.Helper()
		plan, err := Balance(context.Background(), s, s.profile)
		if err != nil {
			t.Fatalf("Balance: %v", err)
		}
		// Determinism: recomputing over the same state is
		// byte-identical (computed BEFORE any write - a written plan
		// is a locked, incremental plan instead).
		again, err := Balance(context.Background(), s, s.profile)
		if err != nil {
			t.Fatalf("Balance recompute: %v", err)
		}
		if resultDigest(plan.Result) != resultDigest(again.Result) {
			t.Fatalf("recompute differs:\n%s\nvs\n%s",
				resultDigest(plan.Result), resultDigest(again.Result))
		}
		if err := plan.Write(context.Background(), s.factory()); err != nil {
			t.Fatalf("Write: %v", err)
		}
		out := make(map[string]int64, len(s.files))
		for id, f := range s.files {
			out[f.RelPath] = s.files[id].ImageID
		}
		return out
	}
	a := placements(t, build(map[int64]string{1: "zeta", 2: "alpha", 3: "mid"}))
	b := placements(t, build(map[int64]string{1: "mid", 2: "zeta", 3: "alpha"}))
	want := map[string]int64{"alpha": 1, "mid": 2, "zeta": 3}
	for rel, img := range want {
		if a[rel] != img || b[rel] != img {
			t.Errorf("tie-broken placement of %q: (%d, %d), want (%d, %d) in both id orders",
				rel, a[rel], b[rel], img, img)
		}
	}
}

// --- incremental / locked semantics (FR-011, FR-029) ------------------------------

func TestBalancerIncrementalLockedDestinationsUntouched(t *testing.T) {
	s := newFakeStore(testProfile(1200))
	addLockedImage(s, 1, budgetUsable(1200))
	addFile(s, 1, "old", 90)
	s.files[1].ImageID = 1 // locked
	addFile(s, 2, "new-a", 10)
	addFile(s, 3, "new-b", 50)

	res, err := runPlan(t, s)
	if err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	if res.NewDestinations != 1 || imageOf(s, 2) != 2 || imageOf(s, 3) != 2 {
		t.Errorf("new items did not open exactly image 2: new=%d, a->%d, b->%d",
			res.NewDestinations, imageOf(s, 2), imageOf(s, 3))
	}
	if imageOf(s, 1) != 1 {
		t.Errorf("locked file re-targeted to %d", imageOf(s, 1))
	}
	if st := s.stateOf(1); st.UsedBytes != 90 {
		t.Errorf("locked image 1 fill changed to %d, want the original 90", st.UsedBytes)
	}
	if len(res.Destinations) == 0 || !res.Destinations[0].Locked || res.Destinations[0].ImageID != 1 {
		t.Errorf("result must open with the locked image 1 line: %+v", res.Destinations)
	}
}

func TestBalancerZeroUnassignedItemsIsNoOp(t *testing.T) {
	// A run with zero unassigned items is a NO-OP: no transaction,
	// no empty destinations (asserted by a factory that fails the
	// test if it is ever called).
	s := newFakeStore(testProfile(1200))
	addLockedImage(s, 1, budgetUsable(1200))
	addFile(s, 1, "done", 10)
	s.files[1].ImageID = 1

	plan, err := Balance(context.Background(), s, s.profile)
	if err != nil {
		t.Fatalf("Balance: %v", err)
	}
	if plan.Result.NewDestinations != 0 || len(plan.Result.Destinations) != 1 {
		t.Errorf("no-op plan reports %d new / %d destinations, want 0 / 1 (the locked one)",
			plan.Result.NewDestinations, len(plan.Result.Destinations))
	}
	err = plan.Write(context.Background(), func(context.Context) (*AssignTx, error) {
		t.Fatal("no-op plan opened a transaction")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("no-op Write: %v", err)
	}
	if s.began != 0 {
		t.Errorf("factory invoked %d times for a no-op", s.began)
	}
}

// --- empty-dir lowest-numbered rule (4.3) -------------------------------------------

func TestBalancerDirsOnlyCreatesDestinationOne(t *testing.T) {
	// Payload budget 2000: the three dirs' entry charges (3 * 512 =
	// 1536) fit the fresh destination's budget, so the dirs-only plan
	// opens destination 1 alone (4.3 empty-dir rule).
	s := newFakeStore(testProfile(2000))
	for i, id := range []int64{5, 3, 4} { // deliberately unordered ids
		s.dirs[id] = &fakeDir{relPath: fmt.Sprintf("empty/d%d", i), active: true}
	}
	res, err := runPlan(t, s)
	if err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	if res.NewDestinations != 1 || len(s.images) != 1 {
		t.Fatalf("dirs-only plan created %d destinations, want 1", len(s.images))
	}
	if st := s.stateOf(1); st.UsedBytes != 0 || st.DirCount != 3 {
		t.Errorf("image 1 = %+v, want zero bytes and 3 dirs", st)
	}
	for _, id := range []int64{3, 4, 5} {
		if s.dirs[id].imageID != 1 {
			t.Errorf("dir %d on image %d, want 1", id, s.dirs[id].imageID)
		}
	}
}

func TestBalancerDirsAttachToLowestLockedDestination(t *testing.T) {
	// 4.3: the empty-dir pass targets the LOWEST-numbered destination
	// of the plan - in an incremental run that is the locked image 1,
	// whose reserved budget (2000 of entry slack here) absorbs the
	// dirs' 512-byte charges, and the dirs add no payload bytes there
	// (capacity lock untouched).
	s := newFakeStore(testProfile(2000))
	addLockedImage(s, 1, budgetUsable(2000))
	addLockedImage(s, 2, budgetUsable(2000))
	s.dirs[7] = &fakeDir{relPath: "empty/a", active: true}
	s.dirs[8] = &fakeDir{relPath: "empty/b", active: true}
	addFile(s, 1, "payload", 10)

	res, err := runPlan(t, s)
	if err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	if s.dirs[7].imageID != 1 || s.dirs[8].imageID != 1 {
		t.Errorf("dirs landed on (%d, %d), want the lowest-numbered destination 1",
			s.dirs[7].imageID, s.dirs[8].imageID)
	}
	if st := s.stateOf(1); st.UsedBytes != 0 || st.DirCount != 2 {
		t.Errorf("locked image 1 = %+v, want 0 payload bytes and 2 dirs", st)
	}
	if imageOf(s, 1) != 3 {
		t.Errorf("payload file landed on image %d, want NEW image 3", imageOf(s, 1))
	}
	if res.Destinations[0].ImageID != 1 || res.Destinations[0].DirCount != 2 {
		t.Errorf("result line for image 1 = %+v, want 2 dirs", res.Destinations[0])
	}
}

func TestBalancerMissingDirsNeverAssigned(t *testing.T) {
	s := newFakeStore(testProfile(2000))
	s.dirs[1] = &fakeDir{relPath: "gone", active: false}
	s.dirs[2] = &fakeDir{relPath: "here", active: true}
	if _, err := runPlan(t, s); err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	if s.dirs[1].imageID != 0 {
		t.Errorf("missing dir assigned to image %d", s.dirs[1].imageID)
	}
	if s.dirs[2].imageID != 1 {
		t.Errorf("active dir assigned to image %d, want 1", s.dirs[2].imageID)
	}
}

// --- hard-link families (2.4) --------------------------------------------------------

func TestBalancerHardLinkSecondariesExcluded(t *testing.T) {
	// Case A: primary unassigned -> the PRIMARY is the item, the
	// secondary is excluded entirely; family bytes counted once.
	s := newFakeStore(testProfile(2000))
	for _, id := range []int64{1, 2} {
		addFile(s, id, fmt.Sprintf("hl/link%d", id), 40)
		s.files[id].Dev, s.files[id].Inode, s.files[id].Nlink = 7, 9001, 2
	}
	res, err := runPlan(t, s)
	if err != nil {
		t.Fatalf("runPlan (case A): %v", err)
	}
	if imageOf(s, 1) == 0 {
		t.Error("primary of an unassigned family was not placed")
	}
	if imageOf(s, 2) != 0 {
		t.Errorf("secondary was placed on image %d, want excluded (2.4)", imageOf(s, 2))
	}
	if res.SecondariesExcluded != 1 || res.FilesAssigned != 1 {
		t.Errorf("counts = (excluded %d, files %d), want (1, 1)",
			res.SecondariesExcluded, res.FilesAssigned)
	}
	if st := s.stateOf(imageOf(s, 1)); st.UsedBytes != 40 {
		t.Errorf("destination holds %d bytes, want the family size counted once (40)", st.UsedBytes)
	}

	// Case B: primary LOCKED -> the family is already on media, so
	// its unassigned secondaries create NO items at all (no empty
	// destinations either).
	s2 := newFakeStore(testProfile(2000))
	addLockedImage(s2, 1, budgetUsable(2000))
	for _, id := range []int64{1, 2} {
		addFile(s2, id, fmt.Sprintf("hl/link%d", id), 40)
		s2.files[id].Dev, s2.files[id].Inode, s2.files[id].Nlink = 7, 9001, 2
	}
	s2.files[1].ImageID = 1 // primary already on media
	res2, err := runPlan(t, s2)
	if err != nil {
		t.Fatalf("runPlan (case B): %v", err)
	}
	if imageOf(s2, 2) != 0 || res2.NewDestinations != 0 || s2.began != 0 {
		t.Errorf("case B: secondary image %d, new destinations %d, transactions %d - want 0/0/0",
			imageOf(s2, 2), res2.NewDestinations, s2.began)
	}
	if res2.SecondariesExcluded != 1 {
		t.Errorf("case B: SecondariesExcluded = %d, want 1", res2.SecondariesExcluded)
	}
}

// --- symlinks and validation -----------------------------------------------------------

func TestBalancerSymlinksAreLenTargetItems(t *testing.T) {
	// Budget 1200: charges 60+512 and 30+512 fit one destination.
	s := newFakeStore(testProfile(1200))
	addFile(s, 1, "big", 60)
	addLink(s, 2, "link", 30)
	res, err := runPlan(t, s)
	if err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	if res.SymlinksAssigned != 1 || res.FilesAssigned != 1 {
		t.Errorf("counts = (symlinks %d, files %d), want (1, 1)",
			res.SymlinksAssigned, res.FilesAssigned)
	}
	if len(s.chunkRows) != 0 {
		t.Errorf("symlink produced %d chunk rows", len(s.chunkRows))
	}
	// 60 + 30 fit one destination; the symlink counts its len(target)
	// bytes toward that destination's fill.
	if imageOf(s, 1) != 1 || imageOf(s, 2) != 1 {
		t.Errorf("placement = (file %d, symlink %d), want both on 1",
			imageOf(s, 1), imageOf(s, 2))
	}
	if st := s.stateOf(1); st.UsedBytes != 90 || st.ItemCount != 2 {
		t.Errorf("destination 1 = %+v, want 90 bytes across 2 items", st)
	}
}

func TestBalancerItemTooLargeAndInvalidProfilesRejected(t *testing.T) {
	// A symlink bigger than one image's whole payload budget (budget
	// 513: len(target) 600 + entry 512 > 513) cannot be placed; the
	// plan aborts BEFORE any transaction opens.
	s := newFakeStore(testProfile(513))
	addLink(s, 1, "huge-link", 600)
	_, err := Balance(context.Background(), s, s.profile)
	if !errors.Is(err, ErrItemTooLarge) {
		t.Fatalf("Balance = %v, want ErrItemTooLarge", err)
	}
	if s.began != 0 {
		t.Errorf("rejected plan opened %d transactions", s.began)
	}

	degenerate := units.FixedImageOverhead + 100 // usable <= fixed + entry: no payload capacity
	bad := []units.Profile{
		{Name: "", NominalBytes: 200, UsableBytes: 100},
		{Name: "disk", NominalBytes: 0, UsableBytes: 100},
		{Name: "disk", NominalBytes: 200, UsableBytes: 0},
		{Name: "disk", NominalBytes: 200, UsableBytes: -1},
		{Name: "disk", NominalBytes: 100, UsableBytes: 200}, // usable > nominal
		// The 2026-10-20 capacity floor: a usable_size that cannot
		// hold the fixed overhead plus one entry can never satisfy
		// the buildability invariant.
		{Name: "disk", NominalBytes: 2 * degenerate, UsableBytes: degenerate},
	}
	empty := newFakeStore(testProfile(1200))
	for i, p := range bad {
		if _, err := Balance(context.Background(), empty, p); !errors.Is(err, ErrInvalidProfile) {
			t.Errorf("case %d (%+v): Balance = %v, want ErrInvalidProfile", i, p, err)
		}
	}
}

// --- chunk actions (4.2 boundary stability) --------------------------------------------

func TestBalancerChunkActionsFreshReuseRechunk(t *testing.T) {
	// Payload budget 612 = 512 entry + 100: the effective chunk size
	// is budget - 512 = 100, so the old fixture's 100-based
	// decompositions carry over exactly (the entry charges now live in
	// the budget instead of the raw usable).
	// CREATE-FRESH: oversized, never chunked.
	fresh := newFakeStore(testProfile(612))
	addFile(fresh, 1, "new.img", 250)
	res, err := runPlan(t, fresh)
	if err != nil {
		t.Fatalf("fresh: %v", err)
	}
	if res.RechunkedFiles != 0 {
		t.Errorf("fresh plan reports %d re-chunked files, want 0", res.RechunkedFiles)
	}
	rows := fresh.chunkRows[1]
	if len(rows) != 3 || rows[0].length != 100 || rows[1].length != 100 || rows[2].length != 50 {
		t.Fatalf("fresh decomposition = %+v, want 100+100+50", rows)
	}
	for i, c := range rows {
		if c.imageID == 0 {
			t.Errorf("fresh chunk %d unassigned", i)
		}
	}

	// REUSE: stamp == effective, matching rows with stored hashes -
	// the hashes must survive bit-for-bit (4.2 zero-re-read contract).
	reuse := newFakeStore(testProfile(612))
	addFile(reuse, 1, "stored.img", 250)
	stampDecomposition(t, reuse, 1, 250, 100, 2)
	res2, err := runPlan(t, reuse)
	if err != nil {
		t.Fatalf("reuse: %v", err)
	}
	if res2.RechunkedFiles != 0 || res2.RehashedAtNextWrite != 0 {
		t.Errorf("reuse plan reports (rechunk %d, rehash %d), want (0, 0)",
			res2.RechunkedFiles, res2.RehashedAtNextWrite)
	}
	if !reuse.chunkRows[1][0].hasHash || !reuse.chunkRows[1][1].hasHash ||
		reuse.chunkRows[1][2].hasHash {
		t.Error("reuse changed the stored hash pattern (4.2 contract)")
	}
	for i, c := range reuse.chunkRows[1] {
		if c.imageID == 0 {
			t.Errorf("reuse chunk %d unassigned", i)
		}
	}

	// RECHUNK: stamp != effective; the two WRITTEN rows are reported
	// re-hashed at next write (4.2), and the fresh rows carry no
	// stale hashes.
	rechunk := newFakeStore(testProfile(612))
	addFile(rechunk, 1, "shifted.img", 250)
	stampDecomposition(t, rechunk, 1, 250, 60, 2)
	res3, err := runPlan(t, rechunk)
	if err != nil {
		t.Fatalf("rechunk: %v", err)
	}
	if res3.RechunkedFiles != 1 || res3.RehashedAtNextWrite != 2 {
		t.Errorf("rechunk plan reports (rechunk %d, rehash %d), want (1, 2)",
			res3.RechunkedFiles, res3.RehashedAtNextWrite)
	}
	newRows := rechunk.chunkRows[1]
	if len(newRows) != 3 || newRows[0].length != 100 || newRows[2].length != 50 {
		t.Fatalf("rechunked decomposition = %+v, want the 100-based boundaries", newRows)
	}
	for i, c := range newRows {
		if c.hasHash {
			t.Errorf("rechunked row %d kept a stale hash", i)
		}
		if c.imageID == 0 {
			t.Errorf("rechunked row %d unassigned", i)
		}
	}
	if rechunk.files[1].ChunkSize != 100 || rechunk.files[1].chunkCount != 3 {
		t.Errorf("rechunked stamp = (size %d, count %d), want (100, 3)",
			rechunk.files[1].ChunkSize, rechunk.files[1].chunkCount)
	}
}

func TestBalancerReuseIntegrityEdgeFallsBackToRechunk(t *testing.T) {
	// files.chunk_size MATCHES the effective size but the stored rows
	// do not verify (catalog corruption edge documented in
	// DecideChunking): the balancer falls back to the re-chunk path
	// instead of trusting rows that lie.
	s := newFakeStore(testProfile(612))
	addFile(s, 1, "liar.img", 250)
	s.chunkRows[1] = []fakeChunk{{index: 0, offset: 0, length: 250}} // wrong geometry
	s.files[1].chunked = true
	s.files[1].chunkCount = 1
	s.files[1].ChunkSize = 100
	res, err := runPlan(t, s)
	if err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	if res.RechunkedFiles != 1 {
		t.Errorf("integrity edge: RechunkedFiles = %d, want 1", res.RechunkedFiles)
	}
	rows := s.chunkRows[1]
	if len(rows) != 3 || rows[0].length != 100 || rows[2].length != 50 {
		t.Errorf("recovered decomposition = %+v, want 100+100+50", rows)
	}
}

func TestBalancerLockedChunkedFileIsNeverReplaced(t *testing.T) {
	// A chunked file's lock lives on its CHUNK rows (4.5: chunked
	// files never carry files.image_id). A fully-assigned chunk set
	// must never be re-placed, re-chunked, or dropped - even when a
	// changed profile would now let the file fit whole.
	s := newFakeStore(testProfile(1200))
	addLockedImage(s, 1, budgetUsable(1200))
	addFile(s, 1, "on-media.img", 250)
	stampDecomposition(t, s, 1, 250, 100, 2)
	for i := range s.chunkRows[1] {
		s.chunkRows[1][i].imageID = 1
	}
	before := s.digest()

	// The plan now has usable 1000 > 250: without the chunk-row lock
	// detector this file would be NB2-dropped and re-placed whole.
	res, err := runPlan(t, s)
	if err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	if s.digest() != before {
		t.Errorf("locked chunked file mutated:\n--- before ---\n%s\n--- after ---\n%s",
			before, s.digest())
	}
	if res.NewDestinations != 0 || s.began != 0 {
		t.Errorf("plan over a locked chunked file: new=%d transactions=%d, want 0/0",
			res.NewDestinations, s.began)
	}
}

// --- NB2 whole-file transition (folded T2.1 review note) ------------------------------

func TestBalancerNBDroppedStaleOnWholeTransition(t *testing.T) {
	// Folded NB2 at the balancer level: chunked under a SMALL
	// profile (budget 612 -> effective chunk size 100), cleared
	// (assignments back to NULL), re-planned under a LARGE profile
	// (budget 1000, where the file's 762-byte charge fits whole) -
	// the file lands as a whole item with ZERO chunk rows and a clean
	// stamp.
	s := newFakeStore(testProfile(612))
	addFile(s, 1, "movie.iso", 250)
	stampDecomposition(t, s, 1, 250, 100, 1)

	s.profile = testProfile(1000)
	res, err := runPlan(t, s)
	if err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	if imageOf(s, 1) == 0 {
		t.Fatal("file unassigned under the large profile")
	}
	if len(s.chunkRows[1]) != 0 {
		t.Errorf("file keeps %d stale chunk rows, want 0 (NB2)", len(s.chunkRows[1]))
	}
	if s.files[1].chunked || s.files[1].ChunkSize != 0 || s.files[1].chunkCount != 0 {
		t.Errorf("file keeps the stale stamp (chunked=%v, count=%d, size=%d)",
			s.files[1].chunked, s.files[1].chunkCount, s.files[1].ChunkSize)
	}
	if res.FilesAssigned != 1 || res.ChunksAssigned != 0 {
		t.Errorf("counts = (files %d, chunks %d), want (1, 0)", res.FilesAssigned, res.ChunksAssigned)
	}
}

// --- transaction discipline (4.5) -------------------------------------------------------

func TestBalancerWriteFailureRollsBackTransaction(t *testing.T) {
	// 4.5: any statement failure aborts the plan; the deferred
	// rollback runs and Commit never does (the catalog-side rollback
	// atomicity itself is pinned in planstore_test.go against the
	// real SQLite transaction).
	s := newFakeStore(testProfile(1200))
	addFile(s, 1, "a", 60)
	addFile(s, 2, "b", 60)
	s.failOn = "AssignFile"
	err := func() error {
		plan, err := Balance(context.Background(), s, s.profile)
		if err != nil {
			t.Fatalf("Balance: %v", err)
		}
		return plan.Write(context.Background(), s.factory())
	}()
	if err == nil {
		t.Fatal("Write with an injected failure returned nil, want an error")
	}
	if s.rolledBack == 0 {
		t.Error("failed Write did not roll back the transaction")
	}
	if s.committed != 0 {
		t.Errorf("failed Write committed %d times", s.committed)
	}
}

// --- the metadata-only guarantee (4.3, 12.5) ----------------------------------------------

// TestBalancerNeverTouchesFilesystem is the hard metadata-only proof:
// balancer.go may import only packages that cannot reach the
// filesystem or the network (the AST import-assertion pattern of the
// T2.1 chunker tests, in ALLOWLIST form from the start).
func TestBalancerNeverTouchesFilesystem(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "balancer.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing balancer.go: %v", err)
	}
	allowed := map[string]bool{
		"container/heap":          true,
		"context":                 true,
		"errors":                  true,
		"fmt":                     true,
		"sort":                    true,
		"isopress/internal/units": true,
	}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if !allowed[path] {
			t.Errorf("balancer.go imports %q: the balancer must stay metadata-only "+
				"(DESIGN_SPEC 4.3, 12.5; allowlist %v)", path, allowed)
		}
	}
}

// TestBalancerZeroSourceReadsOverRealUnreadableFiles runs a full
// Balance + Write whose relpaths are ABSOLUTE paths of real files
// made unreadable (mode 0000): any open attempt by the planner would
// fail with EACCES and surface as a test failure. On a root run the
// chmod guard is vacuous (documented the same way in the T2.1
// chunker test) - the AST allowlist above remains the standing proof.
func TestBalancerZeroSourceReadsOverRealUnreadableFiles(t *testing.T) {
	dir := t.TempDir()
	var paths []string
	for _, name := range []string{"unreadable-a.bin", "unreadable-b.bin"} {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("secret payload"), 0o644); err != nil {
			t.Fatalf("writing %s: %v", p, err)
		}
		if err := os.Chmod(p, 0o000); err != nil {
			t.Fatalf("chmod 0000 %s: %v", p, err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
		paths = append(paths, p)
	}
	if os.Geteuid() == 0 {
		t.Log("running as root: mode-0000 open guard is vacuous; " +
			"the AST import allowlist remains the metadata-only proof")
	}

	s := newFakeStore(testProfile(2048))
	for i, p := range paths {
		addFile(s, int64(i+1), p, 500)
	}
	s.dirs[1] = &fakeDir{relPath: filepath.Join(dir, "empty-dir"), active: true}

	res, err := runPlan(t, s)
	if err != nil {
		t.Fatalf("plan over unreadable sources failed (would require a source open): %v", err)
	}
	if res.FilesAssigned != 2 || res.DirsAssigned != 1 {
		t.Errorf("counts = (files %d, dirs %d), want (2, 1)", res.FilesAssigned, res.DirsAssigned)
	}
}
