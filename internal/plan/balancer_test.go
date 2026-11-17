package plan

// T2.2: balancer tests (DESIGN_SPEC 4.3 WFD + determinism, 4.4
// wasted-residual visibility, 4.5 single-transaction assignment, 2.4
// hard-link families and FR-011 locking; 12.5 planning-engine property
// suite). The catalog SQL underneath is pinned separately in
// internal/catalog/planstore_assign_test.go (DESIGN_SPEC 12.1 split):
// this file tests the pure algorithm against a faithful in-memory
// store double, exactly the T1.6 reconciler pattern.
//
// TestBalancerPropertySeededTrees: for
// every seed it generates a randomized catalog state (whole files,
// oversized files, top-band files at the honest-fit boundary,
// symlinks, hard-link families, previously-chunked files covering
// create-fresh/reuse/rechunk/NB2, active and missing empty dirs,
// locked destinations with locked whole and chunked files), runs
// Balance + Write, and asserts EVERY 12.5 invariant: the buildability
// invariant RequiredImageBytes(payload, entries) <= usable_size for
// every planned image (the 2026-10-20 lead invariant); no destination
// over usable_size; all active unassigned items assigned; chunk sets
// complete and contiguous; locked assignments bit-identical; media
// count matches the report command; determinism (same seed, same
// plan); zero source reads; active empty dirs land on the
// lowest-numbered destination whose reserved budget absorbs their
// entry charge with directories.image_id persisted; missing dirs
// never assigned; the zero-unassigned no-op; and the amended
// wasted-residual figure recomputed from the store. The fixture is
// printed with the seed on failure.

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"isopress/internal/units"
)

// --- the in-memory store double -----------------------------------------------

// fakeFile mirrors one files row plus the chunk stamp columns.
type fakeFile struct {
	ActiveFile
	chunked    bool
	chunkCount int
}

// fakeChunk mirrors one chunks row.
type fakeChunk struct {
	index   int
	offset  int64
	length  int64
	imageID int64 // 0 = unassigned
	hasHash bool
}

// fakeDir mirrors one directories row.
type fakeDir struct {
	relPath string
	active  bool
	imageID int64
}

// fakeImage mirrors one images row.
type fakeImage struct {
	spec  ImageSpec
	state string
}

// fakeStore is the catalog double: Store (read side) + the
// AssignStore write methods, implementing the same semantics the SQL
// layer pins (FR-011 NULL-guards, idempotent EnsureChunkRows,
// full-replacement re-chunk, NB2 drop-and-unstamp, image rows before
// assignments). failOn injects a mid-transaction failure by method
// name; began/committed/rolledBack record the transaction lifecycle.
type fakeStore struct {
	profile units.Profile

	files     map[int64]*fakeFile
	dirs      map[int64]*fakeDir
	images    map[int64]fakeImage
	chunkRows map[int64][]fakeChunk

	failOn     string
	began      int
	committed  int
	rolledBack int
}

func newFakeStore(profile units.Profile) *fakeStore {
	return &fakeStore{
		profile:   profile,
		files:     make(map[int64]*fakeFile),
		dirs:      make(map[int64]*fakeDir),
		images:    make(map[int64]fakeImage),
		chunkRows: make(map[int64][]fakeChunk),
	}
}

// guard is the failure-injection seam.
func (s *fakeStore) guard(method string) error {
	if s.failOn == method {
		return fmt.Errorf("fakeStore: injected failure in %s", method)
	}
	return nil
}

// --- Store (read side) ---------------------------------------------------------

func (s *fakeStore) ActiveFiles(_ context.Context) ([]ActiveFile, error) {
	ids := sortedIDs64(s.files)
	out := make([]ActiveFile, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.files[id].ActiveFile)
	}
	return out, nil
}

func (s *fakeStore) ChunkStates(_ context.Context) ([]StoredChunk, error) {
	fileIDs := sortedKeys64(s.chunkRows)
	var out []StoredChunk
	for _, fid := range fileIDs {
		f := s.files[fid]
		for _, c := range s.chunkRows[fid] {
			out = append(out, StoredChunk{
				FileID: fid, ChunkSize: f.ChunkSize, Index: c.index,
				Offset: c.offset, Length: c.length,
				ImageID: c.imageID, HashPresent: c.hasHash,
			})
		}
	}
	return out, nil
}

func (s *fakeStore) UnassignedDirs(_ context.Context) ([]UnassignedDir, error) {
	ids := sortedKeys64(s.dirs)
	var out []UnassignedDir
	for _, id := range ids {
		d := s.dirs[id]
		if d.active && d.imageID == 0 {
			out = append(out, UnassignedDir{DirID: id, RelPath: d.relPath})
		}
	}
	return out, nil
}

func (s *fakeStore) Destinations(_ context.Context) ([]DestinationState, error) {
	ids := sortedKeys64(s.images)
	out := make([]DestinationState, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.stateOf(id))
	}
	return out, nil
}

// stateOf computes one destination's fill from the assignment columns
// (the same folding the catalog's Destinations read performs).
func (s *fakeStore) stateOf(id int64) DestinationState {
	img := s.images[id]
	d := DestinationState{
		ImageID: id, NominalSize: img.spec.NominalSize,
		UsableSize: img.spec.UsableSize, MediaProfile: img.spec.MediaProfile,
	}
	for _, f := range s.files {
		if f.ImageID == id {
			d.UsedBytes += f.Size
			d.ItemCount++
		}
	}
	for fid, rows := range s.chunkRows {
		if s.files[fid].ImageID != 0 {
			continue // whole-file row already counted
		}
		for _, c := range rows {
			if c.imageID == id {
				d.UsedBytes += c.length
				d.ItemCount++
			}
		}
	}
	for _, dir := range s.dirs {
		if dir.active && dir.imageID == id {
			d.DirCount++
		}
	}
	return d
}

// --- AssignStore (write side, FR-011 guards) -----------------------------------

func (s *fakeStore) InsertImage(_ context.Context, img ImageSpec) error {
	if err := s.guard("InsertImage"); err != nil {
		return err
	}
	if img.ImageID <= 0 || img.NominalSize <= 0 || img.UsableSize <= 0 || img.MediaProfile == "" {
		return fmt.Errorf("fakeStore: invalid image spec %+v", img)
	}
	if _, ok := s.images[img.ImageID]; ok {
		return fmt.Errorf("fakeStore: image %d already exists", img.ImageID)
	}
	s.images[img.ImageID] = fakeImage{spec: img, state: "planned"}
	return nil
}

func (s *fakeStore) EnsureChunkRows(_ context.Context, fileID int64, specs []ChunkSpec) error {
	if err := s.guard("EnsureChunkRows"); err != nil {
		return err
	}
	if err := validateFakeSpecs(specs); err != nil {
		return err
	}
	if rows := s.chunkRows[fileID]; len(rows) > 0 {
		if !fakeMatches(rows, specs) {
			return fmt.Errorf("fakeStore: stored rows of file %d do not match decomposition", fileID)
		}
		return nil
	}
	s.chunkRows[fileID] = insertFakeRows(specs)
	return nil
}

func (s *fakeStore) ReplaceAllChunks(_ context.Context, fileID int64, specs []ChunkSpec) (RechunkOutcome, error) {
	if err := s.guard("ReplaceAllChunks"); err != nil {
		return RechunkOutcome{}, err
	}
	if err := validateFakeSpecs(specs); err != nil {
		return RechunkOutcome{}, err
	}
	var out RechunkOutcome
	for _, c := range s.chunkRows[fileID] {
		if c.imageID != 0 {
			return RechunkOutcome{}, fmt.Errorf("fakeStore: file %d chunk %d is assigned", fileID, c.index)
		}
		if c.hasHash {
			out.RemovedWritten++
		} else {
			out.RemovedUnwritten++
		}
	}
	s.chunkRows[fileID] = insertFakeRows(specs)
	return out, nil
}

func (s *fakeStore) DropChunkState(_ context.Context, fileID int64) (int, error) {
	if err := s.guard("DropChunkState"); err != nil {
		return 0, err
	}
	f, ok := s.files[fileID]
	if !ok {
		return 0, fmt.Errorf("fakeStore: unknown file %d", fileID)
	}
	rows := s.chunkRows[fileID]
	for _, c := range rows {
		if c.imageID != 0 {
			return 0, fmt.Errorf("fakeStore: file %d chunk %d is assigned", fileID, c.index)
		}
	}
	delete(s.chunkRows, fileID)
	f.chunked = false
	f.chunkCount = 0
	f.ChunkSize = 0
	return len(rows), nil
}

func (s *fakeStore) StoreChunkSize(_ context.Context, fileID int64, chunkSize int64, chunkCount int) error {
	if err := s.guard("StoreChunkSize"); err != nil {
		return err
	}
	f, ok := s.files[fileID]
	if !ok {
		return fmt.Errorf("fakeStore: unknown file %d", fileID)
	}
	if chunkSize <= 0 || chunkCount < 0 {
		return fmt.Errorf("fakeStore: invalid stamp (%d, %d) for file %d", chunkSize, chunkCount, fileID)
	}
	f.chunked = true
	f.chunkCount = chunkCount
	f.ChunkSize = chunkSize
	return nil
}

func (s *fakeStore) AssignFile(_ context.Context, fileID, imageID int64) error {
	if err := s.guard("AssignFile"); err != nil {
		return err
	}
	f, ok := s.files[fileID]
	if !ok {
		return fmt.Errorf("fakeStore: unknown file %d", fileID)
	}
	if imageID <= 0 || f.ImageID != 0 {
		return fmt.Errorf("fakeStore: AssignFile(%d, %d): file image is %d", fileID, imageID, f.ImageID)
	}
	if _, ok := s.images[imageID]; !ok {
		return fmt.Errorf("fakeStore: AssignFile(%d, %d): no such image", fileID, imageID)
	}
	f.ImageID = imageID
	return nil
}

func (s *fakeStore) AssignChunk(_ context.Context, fileID int64, chunkIndex int, imageID int64) error {
	if err := s.guard("AssignChunk"); err != nil {
		return err
	}
	if _, ok := s.images[imageID]; !ok {
		return fmt.Errorf("fakeStore: AssignChunk(%d, %d, %d): no such image", fileID, chunkIndex, imageID)
	}
	for i := range s.chunkRows[fileID] {
		c := &s.chunkRows[fileID][i]
		if c.index == chunkIndex {
			if c.imageID != 0 {
				return fmt.Errorf("fakeStore: chunk %d of file %d already assigned", chunkIndex, fileID)
			}
			c.imageID = imageID
			return nil
		}
	}
	return fmt.Errorf("fakeStore: no chunk %d of file %d", chunkIndex, fileID)
}

func (s *fakeStore) AssignDir(_ context.Context, dirID, imageID int64) error {
	if err := s.guard("AssignDir"); err != nil {
		return err
	}
	d, ok := s.dirs[dirID]
	if !ok || !d.active || d.imageID != 0 {
		return fmt.Errorf("fakeStore: AssignDir(%d, %d): dir missing, inactive, or assigned", dirID, imageID)
	}
	if _, ok := s.images[imageID]; !ok {
		return fmt.Errorf("fakeStore: AssignDir(%d, %d): no such image", dirID, imageID)
	}
	d.imageID = imageID
	return nil
}

// factory returns the AssignTxFactory over this store.
func (s *fakeStore) factory() AssignTxFactory {
	return func(_ context.Context) (*AssignTx, error) {
		s.began++
		return &AssignTx{
			AssignStore: s,
			Commit: func() error {
				s.committed++
				return nil
			},
			Rollback: func() error {
				s.rolledBack++
				return nil
			},
		}, nil
	}
}

// --- fake helpers ----------------------------------------------------------------

func validateFakeSpecs(specs []ChunkSpec) error {
	if len(specs) == 0 || specs[0].Offset != 0 {
		return errors.New("fakeStore: empty decomposition or nonzero first offset")
	}
	for i, sp := range specs {
		if sp.Index != i || sp.Length <= 0 {
			return fmt.Errorf("fakeStore: bad spec %d: %+v", i, sp)
		}
		if i > 0 && sp.Offset != specs[i-1].Offset+specs[i-1].Length {
			return fmt.Errorf("fakeStore: non-contiguous spec %d", i)
		}
	}
	return nil
}

func insertFakeRows(specs []ChunkSpec) []fakeChunk {
	rows := make([]fakeChunk, len(specs))
	for i, sp := range specs {
		rows[i] = fakeChunk{index: sp.Index, offset: sp.Offset, length: sp.Length}
	}
	return rows
}

func fakeMatches(rows []fakeChunk, specs []ChunkSpec) bool {
	if len(rows) != len(specs) {
		return false
	}
	for i, r := range rows {
		if r.index != specs[i].Index || r.offset != specs[i].Offset || r.length != specs[i].Length {
			return false
		}
	}
	return true
}

func sortedIDs64[M ~map[int64]V, V any](m M) []int64 {
	ids := make([]int64, 0, len(m))
	for id := range m {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func sortedKeys64[M ~map[int64]V, V any](m M) []int64 { return sortedIDs64(m) }

// digest renders the full assignment state canonically: the same
// state always digests to the same string, byte-for-byte (the 12.5
// determinism comparison).
func (s *fakeStore) digest() string {
	var b strings.Builder
	for _, id := range sortedKeys64(s.images) {
		img := s.images[id]
		fmt.Fprintf(&b, "image %d %s nominal=%d usable=%d state=%s\n",
			id, img.spec.MediaProfile, img.spec.NominalSize, img.spec.UsableSize, img.state)
	}
	for _, id := range sortedKeys64(s.files) {
		f := s.files[id]
		fmt.Fprintf(&b, "file %d %s kind=%s size=%d image=%d chunked=%v count=%d csize=%d\n",
			id, f.RelPath, f.EntryKind, f.Size, f.ImageID, f.chunked, f.chunkCount, f.ChunkSize)
	}
	for _, fid := range sortedKeys64(s.chunkRows) {
		for _, c := range s.chunkRows[fid] {
			fmt.Fprintf(&b, "chunk %d:%d offset=%d len=%d image=%d hash=%v\n",
				fid, c.index, c.offset, c.length, c.imageID, c.hasHash)
		}
	}
	for _, id := range sortedKeys64(s.dirs) {
		d := s.dirs[id]
		fmt.Fprintf(&b, "dir %d %s active=%v image=%d\n", id, d.relPath, d.active, d.imageID)
	}
	return b.String()
}

// resultDigest renders a Result canonically for determinism compares.
func resultDigest(r Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "new=%d files=%d chunks=%d symlinks=%d dirs=%d secondaries=%d "+
		"residual=%d@%d rechunk=%d rehash=%d\n",
		r.NewDestinations, r.FilesAssigned, r.ChunksAssigned, r.SymlinksAssigned,
		r.DirsAssigned, r.SecondariesExcluded, r.WastedResidualBytes,
		r.WastedResidualImages, r.RechunkedFiles, r.RehashedAtNextWrite)
	for _, d := range r.Destinations {
		fmt.Fprintf(&b, "dest %d %s nominal=%d usable=%d used=%d items=%d dirs=%d locked=%v\n",
			d.ImageID, d.MediaProfile, d.NominalSize, d.UsableSize,
			d.UsedBytes, d.ItemCount, d.DirCount, d.Locked)
	}
	return b.String()
}

// --- fixture generation (seeded, printed on failure) ----------------------------

// usablePool: destination capacities whose one-image PAYLOAD budget
// (usable - units.FixedImageOverhead - units.ImageEntryOverhead, the
// 2026-10-20 capacity model) is large enough for multiple items and
// meaningful chunking but small enough that random sizes freely
// produce whole, exact-fit, top-band, single-remainder, and
// multi-chunk files. Every value is far below the 20 GiB default, so
// the effective chunk size is the budget (usable - reserve) and the
// assertions stay exact. The pre-amendment pool {1000, 4096, ...} is
// gone: usable sizes below the 2,098,688-byte reserve cannot hold an
// image at all and plan.Balance now refuses them (validateProfile).
var usablePool = []int64{3_000_000, 5_000_000, 1 << 24, 65_536_000}

// genFixture builds one randomized catalog state from a seed. It
// covers: 0-3 locked destinations; locked whole files; locked chunked
// files; unassigned regular files at boundary sizes (0, the honest
// fit line, the top band, usable+1, multi-chunk with remainder);
// symlinks; hard-link families (including the primary-locked case);
// previously-chunked unassigned files for the reuse, rechunk, and NB2
// whole-transition paths; and active + missing empty directories.
func genFixture(t *testing.T, seed int64) *fakeStore {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	usable := usablePool[rng.Intn(len(usablePool))]
	s := newFakeStore(units.Profile{
		Name: "disk", NominalBytes: 2 * usable, UsableBytes: usable,
	})
	// The one-image payload budget and the whole-file threshold under
	// the capacity model (each placement charges size + 512).
	reserve := units.FixedImageOverhead + units.ImageEntryOverhead
	budget := usable - units.FixedImageOverhead
	maxWhole := usable - reserve // the exact-fit line: whole iff size <= maxWhole

	nextFile := int64(1)
	addRegular := func(rel string, size int64) int64 {
		id := nextFile
		nextFile++
		s.files[id] = &fakeFile{ActiveFile: ActiveFile{
			FileID: id, RelPath: rel, EntryKind: "regular", Size: size, Dev: 42, Inode: 1000 + id, Nlink: 1,
		}}
		return id
	}
	addSymlink := func(rel string, size int64) int64 {
		id := nextFile
		nextFile++
		s.files[id] = &fakeFile{ActiveFile: ActiveFile{
			FileID: id, RelPath: rel, EntryKind: "symlink", Size: size, Nlink: 1,
		}}
		return id
	}
	chunkFile := func(id int64, chunkSize int64, written int) {
		f := s.files[id]
		plan, err := PlanChunks(id, f.Size, chunkSize)
		if err != nil {
			t.Fatalf("seed %d: PlanChunks: %v", seed, err)
		}
		rows := insertFakeRows(plan.Chunks)
		for i := 0; i < written && i < len(rows); i++ {
			rows[i].hasHash = true
		}
		s.chunkRows[id] = rows
		f.chunked = true
		f.chunkCount = len(rows)
		f.ChunkSize = chunkSize
	}
	effective := EffectiveChunkSize(usable)

	// --- locked destinations and their locked payload (fill tracked
	// in CHARGED budget terms: every item consumes size + 512) ---
	locked := rng.Intn(4)
	remaining := make(map[int64]int64, locked)
	for i := 1; i <= locked; i++ {
		s.images[int64(i)] = fakeImage{spec: ImageSpec{
			ImageID: int64(i), NominalSize: 2 * usable, UsableSize: usable, MediaProfile: "disk",
		}, state: "planned"}
		remaining[int64(i)] = budget
	}
	for i := 0; i < rng.Intn(4) && locked > 0; i++ {
		size := rng.Int63n(usable/2 + 1)
		img := int64(rng.Intn(locked) + 1)
		if remaining[img] < size+units.ImageEntryOverhead {
			continue
		}
		id := addRegular(fmt.Sprintf("locked/f%d", i), size)
		s.files[id].ImageID = img
		remaining[img] -= size + units.ImageEntryOverhead
	}
	for i := 0; i < rng.Intn(3) && locked > 0; i++ {
		img := int64(rng.Intn(locked) + 1)
		size := usable + rng.Int63n(usable*2+1)
		id := addRegular(fmt.Sprintf("locked/big%d.img", i), size)
		chunkFile(id, effective, rng.Intn(2))
		var charge int64
		for _, c := range s.chunkRows[id] {
			charge += c.length + units.ImageEntryOverhead
		}
		if remaining[img] < charge {
			// The locked destination cannot absorb the whole chunk
			// set: the file stays unassigned (and lands through the
			// REUSE path below - stored geometry == current).
			continue
		}
		for j := range s.chunkRows[id] {
			s.chunkRows[id][j].imageID = img
		}
		remaining[img] -= charge
	}

	// --- unassigned regular files at boundary sizes ---
	for i := 0; i < 1+rng.Intn(25); i++ {
		var size int64
		switch r := rng.Intn(10); r {
		case 0:
			size = 0
		case 1:
			size = maxWhole // the honest fit line: whole
		case 2:
			size = maxWhole + 1 // one byte into the top band: chunked
		case 3:
			size = usable // the whole usable_size: deep in the top band
		case 4:
			size = usable + 1
		case 5:
			size = usable*2 + rng.Int63n(100)
		default:
			size = rng.Int63n(usable + usable/2 + 1)
		}
		addRegular(fmt.Sprintf("data/f%03d", i), size)
	}

	// --- symlinks (size = len(target)) ---
	for i := 0; i < rng.Intn(4); i++ {
		addSymlink(fmt.Sprintf("data/link%02d", i), int64(rng.Intn(200)))
	}

	// --- hard-link families among unassigned regulars ---
	unassigned := make([]int64, 0)
	for id, f := range s.files {
		if f.ImageID == 0 && f.EntryKind == "regular" {
			unassigned = append(unassigned, id)
		}
	}
	sort.Slice(unassigned, func(i, j int) bool { return unassigned[i] < unassigned[j] })
	for i := 0; i+1 < len(unassigned) && i < 12; i += 2 {
		if rng.Intn(3) != 0 {
			continue
		}
		a, b := unassigned[i], unassigned[i+1]
		for _, id := range []int64{a, b} {
			s.files[id].Dev = 77
			s.files[id].Inode = 5000 + a
			s.files[id].Nlink = 2
		}
		// Occasionally the family primary is already LOCKED: move the
		// LOWER id onto a locked destination (bytes counted once,
		// charge checked against the reserved budget).
		if locked > 0 && rng.Intn(4) == 0 {
			size := s.files[a].Size
			img := int64(rng.Intn(locked) + 1)
			if remaining[img] >= size+units.ImageEntryOverhead {
				s.files[a].ImageID = img
				remaining[img] -= size + units.ImageEntryOverhead
			}
		}
	}

	// --- previously-chunked unassigned files (reuse / rechunk / NB2) ---
	if rng.Intn(2) == 0 {
		size := usable + rng.Int63n(usable+1)
		id := addRegular("data/reuse.img", size)
		chunkFile(id, effective, rng.Intn(3)) // stamp == effective -> REUSE
	}
	if old := effective / 2; old > 0 && rng.Intn(2) == 0 {
		size := usable + rng.Int63n(usable*2+1)
		id := addRegular("data/rechunk.img", size)
		chunkFile(id, old, 1+rng.Intn(2)) // stamp != effective -> RECHUNK
	}
	if old := effective / 2; old > 0 && rng.Intn(2) == 0 {
		size := rng.Int63n(maxWhole + 1) // fits the CURRENT profile whole...
		id := addRegular("data/nowwhole.img", size)
		chunkFile(id, old, 1) // ...but was chunked under the small profile: NB2
	}

	// --- empty dirs: active and missing ---
	for i := 0; i < rng.Intn(6); i++ {
		id := int64(1000 + i)
		s.dirs[id] = &fakeDir{
			relPath: fmt.Sprintf("empty/d%02d", i),
			active:  rng.Intn(6) > 0, // mostly active; some missing
		}
	}
	return s
}

// runPlan computes and writes a plan against the store.
func runPlan(t *testing.T, s *fakeStore) (Result, error) {
	t.Helper()
	plan, err := Balance(context.Background(), s, s.profile)
	if err != nil {
		return Result{}, err
	}
	if err := plan.Write(context.Background(), s.factory()); err != nil {
		return Result{}, err
	}
	return plan.Result, nil
}

// --- the 12.5 property suite -----------------------------------------------------

func TestBalancerPropertySeededTrees(t *testing.T) {
	// 50 seeds (the 300-seed stress run during development was also
	// green, race included; the committed bound keeps CI instant).
	for seed := int64(1); seed <= 50; seed++ {
		seed := seed
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			store := genFixture(t, seed)

			// Determinism leg 1: two computes on the SAME store state
			// produce byte-identical results.
			pa, err := Balance(context.Background(), store, store.profile)
			if err != nil {
				t.Fatalf("Balance #1: %v", err)
			}
			pb, err := Balance(context.Background(), store, store.profile)
			if err != nil {
				t.Fatalf("Balance #2: %v", err)
			}
			if resultDigest(pa.Result) != resultDigest(pb.Result) {
				t.Fatalf("nondeterministic compute on identical state:\n%s\nvs\n%s",
					resultDigest(pa.Result), resultDigest(pb.Result))
			}
			before := snapshotStore(store)
			if err := pa.Write(context.Background(), store.factory()); err != nil {
				t.Fatalf("Write: %v\nfixture:\n%s", err, dumpStore(seed, store))
			}

			res := pa.Result
			checkInvariants(t, before, store, res)

			// Determinism leg 2: a fresh fixture from the same seed,
			// planned end to end, lands in a byte-identical catalog
			// state and reports a byte-identical result.
			twin := genFixture(t, seed)
			twinRes, err := runPlan(t, twin)
			if err != nil {
				t.Fatalf("twin run: %v", err)
			}
			if store.digest() != twin.digest() {
				t.Errorf("same seed produced different catalogs:\n--- first ---\n%s\n--- twin ---\n%s",
					store.digest(), twin.digest())
			}
			if resultDigest(res) != resultDigest(twinRes) {
				t.Errorf("same seed produced different results:\n%s\nvs\n%s",
					resultDigest(res), resultDigest(twinRes))
			}
		})
	}
}

// snapshot is the pre-write state the invariant checker compares
// against (bit-identical locked assignments, unchanged secondaries).
type snapshot struct {
	fileImage map[int64]int64
	fileStamp map[int64][3]int64 // chunked, chunkCount, chunkSize
	chunks    map[int64][]fakeChunk
	dirImage  map[int64]int64
	dirActive map[int64]bool
	images    map[int64]fakeImage
}

func snapshotStore(s *fakeStore) snapshot {
	snap := snapshot{
		fileImage: make(map[int64]int64, len(s.files)),
		fileStamp: make(map[int64][3]int64, len(s.files)),
		chunks:    make(map[int64][]fakeChunk, len(s.chunkRows)),
		dirImage:  make(map[int64]int64, len(s.dirs)),
		dirActive: make(map[int64]bool, len(s.dirs)),
		images:    make(map[int64]fakeImage, len(s.images)),
	}
	for id, f := range s.files {
		snap.fileImage[id] = f.ImageID
		snap.fileStamp[id] = [3]int64{boolInt(f.chunked), int64(f.chunkCount), f.ChunkSize}
	}
	for id, rows := range s.chunkRows {
		snap.chunks[id] = append([]fakeChunk(nil), rows...)
	}
	for id, d := range s.dirs {
		snap.dirImage[id] = d.imageID
		snap.dirActive[id] = d.active
	}
	for id, img := range s.images {
		snap.images[id] = img
	}
	return snap
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// dumpStore renders the fixture for failure messages.
func dumpStore(seed int64, s *fakeStore) string {
	return fmt.Sprintf("seed %d (usable %d)\n%s", seed, s.profile.UsableBytes, s.digest())
}

// secondarySet recomputes the hard-link secondaries INDEPENDENTLY of
// the balancer (2.4): regular files with nlink > 1 and a known inode
// whose (dev,inode) family has a lower-file_id active member. It both
// drives the checker and cross-validates Result.SecondariesExcluded.
func secondarySet(files map[int64]*fakeFile) map[int64]bool {
	type fam struct {
		dev, inode int64
	}
	primary := make(map[fam]int64)
	for id, f := range files {
		if f.EntryKind != "regular" || f.Nlink <= 1 || f.Inode == 0 {
			continue
		}
		k := fam{f.Dev, f.Inode}
		if p, ok := primary[k]; !ok || id < p {
			primary[k] = id
		}
	}
	out := make(map[int64]bool)
	for id, f := range files {
		if f.EntryKind != "regular" || f.Nlink <= 1 || f.Inode == 0 {
			continue
		}
		if p := primary[fam{f.Dev, f.Inode}]; p != id {
			out[id] = true
		}
	}
	return out
}

// checkInvariants asserts the full 12.5 set against the written store.
func checkInvariants(t *testing.T, before snapshot, s *fakeStore, res Result) {
	t.Helper()
	usable := s.profile.UsableBytes
	effective := EffectiveChunkSize(usable)
	secondaries := secondarySet(s.files)

	// 1. Media count and destination id continuity.
	if want := len(before.images) + res.NewDestinations; len(s.images) != want {
		t.Errorf("images rows = %d, want %d (locked %d + new %d)",
			len(s.images), want, len(before.images), res.NewDestinations)
	}
	if len(res.Destinations) != len(s.images) {
		t.Errorf("result destinations = %d, catalog images = %d (media count must match)",
			len(res.Destinations), len(s.images))
	}
	maxLocked := int64(0)
	for id := range before.images {
		if id > maxLocked {
			maxLocked = id
		}
	}
	for id := int64(1); id <= maxLocked; id++ {
		if _, ok := s.images[id]; !ok {
			t.Errorf("locked image %d vanished", id)
		}
	}
	for id := maxLocked + 1; id <= maxLocked+int64(res.NewDestinations); id++ {
		if _, ok := s.images[id]; !ok {
			t.Errorf("new image %d missing", id)
		}
	}

	// 2. THE BUILDABILITY INVARIANT (the 12.5 lead invariant, 2026-10-20):
	// every planned image - locked or new, dirs included - satisfies
	// RequiredImageBytes(payload, entries) <= usable_size, i.e. the
	// planner reserved the builder's 6.3 overhead by construction.
	// Plus: no destination exceeds usable_size; result fills match.
	for _, id := range sortedKeys64(s.images) {
		st := s.stateOf(id)
		if st.UsedBytes > st.UsableSize {
			t.Errorf("image %d holds %d bytes > usable %d", id, st.UsedBytes, st.UsableSize)
		}
		entries := int64(st.ItemCount + st.DirCount)
		if required := units.RequiredImageBytes(st.UsedBytes, entries); required > st.UsableSize {
			t.Errorf("image %d violates the buildability invariant: "+
				"RequiredImageBytes(%d payload, %d entries) = %d > usable %d",
				id, st.UsedBytes, entries, required, st.UsableSize)
		}
		var df *DestFill
		for i := range res.Destinations {
			if res.Destinations[i].ImageID == id {
				df = &res.Destinations[i]
			}
		}
		if df == nil {
			t.Errorf("image %d absent from the result", id)
			continue
		}
		if df.UsedBytes != st.UsedBytes || df.ItemCount != st.ItemCount || df.DirCount != st.DirCount {
			t.Errorf("image %d result fill %+v does not match store %+v", id, *df, st)
		}
		// Locked marks exactly the pre-existing destinations.
		if _, existed := before.images[id]; df.Locked != existed {
			t.Errorf("image %d Locked = %v, existed before = %v", id, df.Locked, existed)
		}
	}

	// 3. Locked assignments bit-identical; secondaries untouched;
	// everything else assigned.
	previouslyUnassigned, secondariesExcluded := 0, 0
	for id, f := range s.files {
		wasImage := before.fileImage[id]
		if wasImage != 0 {
			// LOCKED whole file: image and stamp untouched.
			if f.ImageID != wasImage {
				t.Errorf("locked file %d re-targeted: %d -> %d", id, wasImage, f.ImageID)
			}
			if [3]int64{boolInt(f.chunked), int64(f.chunkCount), f.ChunkSize} != before.fileStamp[id] {
				t.Errorf("locked file %d stamp mutated", id)
			}
			continue
		}
		previouslyUnassigned++
		if secondaries[id] {
			secondariesExcluded++
			// 2.4: secondaries are excluded entirely - nothing about
			// them may change.
			if f.ImageID != 0 {
				t.Errorf("secondary %d (%s) was assigned to %d", id, f.RelPath, f.ImageID)
			}
			if !fakeChunksEqual(s.chunkRows[id], before.chunks[id]) {
				t.Errorf("secondary %d (%s) chunk rows mutated", id, f.RelPath)
			}
			continue
		}
		if before.fileStamp[id][0] == 1 && !ShouldChunk(f.Size, usable) {
			// NB2 whole-file transition: the file now passes the
			// honest fit test, so it is assigned as a whole item,
			// ZERO chunk rows, clean stamp.
			if f.ImageID == 0 {
				t.Errorf("NB2 file %d (%s) unassigned", id, f.RelPath)
			}
			if len(s.chunkRows[id]) != 0 {
				t.Errorf("NB2 file %d keeps %d chunk rows, want 0", id, len(s.chunkRows[id]))
			}
			if f.chunked || f.ChunkSize != 0 {
				t.Errorf("NB2 file %d keeps stamp (chunked=%v size=%d), want clean",
					id, f.chunked, f.ChunkSize)
			}
			continue
		}
		if ShouldChunk(f.Size, usable) || len(before.chunks[id]) > 0 {
			// Chunked placement: every row assigned, complete,
			// contiguous from 0, summing to the file size.
			rows := s.chunkRows[id]
			if len(rows) == 0 {
				t.Fatalf("oversized file %d (%s) has no chunk rows", id, f.RelPath)
			}
			var offset int64
			for i, c := range rows {
				if c.index != i || c.offset != offset {
					t.Errorf("file %d chunk set not contiguous: index %d offset %d, want %d/%d",
						id, c.index, c.offset, i, offset)
				}
				if c.length <= 0 || c.length > effective {
					t.Errorf("file %d chunk %d length %d out of (0, %d]", id, i, c.length, effective)
				}
				if c.imageID == 0 {
					t.Errorf("file %d chunk %d unassigned", id, i)
				}
				offset += c.length
			}
			if offset != f.Size {
				t.Errorf("file %d chunks sum to %d, want size %d", id, offset, f.Size)
			}
			if !f.chunked || f.ChunkSize != effective || f.chunkCount != len(rows) {
				t.Errorf("file %d stamp = (chunked %v, count %d, size %d), want (true, %d, %d)",
					id, f.chunked, f.chunkCount, f.ChunkSize, len(rows), effective)
			}
			continue
		}
		// Whole-file / symlink placement.
		if f.ImageID == 0 {
			t.Errorf("unassigned item %d (%s, size %d)", id, f.RelPath, f.Size)
		}
	}
	if previouslyUnassigned != 0 && res.SecondariesExcluded != secondariesExcluded {
		t.Errorf("SecondariesExcluded = %d, independently recomputed %d",
			res.SecondariesExcluded, secondariesExcluded)
	}

	// 4. Reuse must preserve stored hashes bit-for-bit (4.2). The
	// hash contract applies exactly when the stored geometry
	// SURVIVED the plan unchanged (the reuse path); a re-chunk
	// legitimately replaces rows and their hashes. Different stamps
	// always produce different geometry for chunked files (size >
	// old effective implies count >= 2 under the old size), so
	// geometry-identity after the write is precisely the reuse case.
	for id, rows := range before.chunks {
		if len(rows) == 0 || before.fileImage[id] != 0 {
			continue
		}
		after := s.chunkRows[id]
		if !fakeChunksEqualGeometry(rows, after) {
			continue // re-chunked: old-boundary hashes die with the old rows
		}
		for i, old := range rows {
			if old.hasHash && !after[i].hasHash {
				t.Errorf("file %d chunk %d lost its stored hash (reuse contract, 4.2)", id, i)
			}
		}
	}

	// 5. Dirs: every ACTIVE dir on the lowest-numbered destination
	// whose RESERVED budget absorbs the dirs' entry charge - the 4.3
	// lowest-numbered rule with the 2026-10-20 capacity twist, whose
	// fallback opens a new destination when no existing one has entry
	// slack. The expectation is recomputed here independently of the
	// balancer: per-image remaining = usable - FixedImageOverhead -
	// payload - 512*(payload items + PRE-RUN dirs).
	preDirCount := make(map[int64]int)
	for id := range s.dirs {
		if before.dirImage[id] != 0 {
			preDirCount[before.dirImage[id]]++
		}
	}
	activeDirs := 0
	for id, d := range s.dirs {
		if !before.dirActive[id] {
			if d.imageID != before.dirImage[id] {
				t.Errorf("missing dir %d (%s) was assigned: %d", id, d.relPath, d.imageID)
			}
			continue
		}
		activeDirs++
	}
	totalCharge := int64(activeDirs) * units.ImageEntryOverhead
	wantTarget := int64(0)
	var maxID int64
	for _, id := range sortedKeys64(s.images) {
		if id > maxID {
			maxID = id
		}
		st := s.stateOf(id)
		remaining := st.UsableSize - units.FixedImageOverhead - st.UsedBytes -
			units.ImageEntryOverhead*int64(st.ItemCount+preDirCount[id])
		if wantTarget == 0 && remaining >= totalCharge {
			wantTarget = id
		}
	}
	if wantTarget == 0 {
		// No existing destination fits: the dirs opened a fallback
		// destination (the fixture's budgets always fit all dirs on
		// ONE fallback image).
		wantTarget = maxID + 1
	}
	for id, d := range s.dirs {
		if !before.dirActive[id] {
			continue
		}
		if d.imageID != wantTarget {
			t.Errorf("active dir %d (%s) on image %d, want %d (lowest destination with dir-entry slack)",
				id, d.relPath, d.imageID, wantTarget)
		}
	}
	if res.DirsAssigned != activeDirs {
		t.Errorf("DirsAssigned = %d, want %d (all active dirs)", res.DirsAssigned, activeDirs)
	}

	// 6. Wasted residual: recomputed over the LOCKED destinations with
	// the amended 4.4(c) formula - the TRUE free space under the
	// reserved-overhead model: usable - FixedImageOverhead - 512*
	// entries - payload, counted only when positive.
	var (
		residual      int64
		residualCount int
	)
	for id := range before.images {
		st := s.stateOf(id)
		free := st.UsableSize - units.FixedImageOverhead -
			units.ImageEntryOverhead*int64(st.ItemCount+st.DirCount) - st.UsedBytes
		if free > 0 {
			residual += free
			residualCount++
		}
	}
	if residual != res.WastedResidualBytes || residualCount != res.WastedResidualImages {
		t.Errorf("wasted residual = %d bytes over %d images, want %d over %d",
			res.WastedResidualBytes, res.WastedResidualImages, residual, residualCount)
	}
}

func fakeChunksEqual(a, b []fakeChunk) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fakeChunksEqualGeometry compares the boundary geometry (index,
// offset, length), ignoring assignment and hash state.
func fakeChunksEqualGeometry(a, b []fakeChunk) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].index != b[i].index || a[i].offset != b[i].offset || a[i].length != b[i].length {
			return false
		}
	}
	return true
}

// --- the honest-fit threshold property (2026-10-20 capacity amendment) ----------

// TestBalancerWholeFileThresholdHonestFit pins the whole-file/chunk
// boundary END TO END through Balance: a file at
// usable - FIXED_OVERHEAD - 512 (the exact fit - the largest file
// whose RequiredImageBytes equals usable_size) plans WHOLE; one byte
// LARGER - usable - FIXED_OVERHEAD - 511, the bottom of the old top
// band - is CHUNKED because it genuinely does not fit a buildable
// image (the DESIGN_SPEC 4.3 fit test `size + FIXED_OVERHEAD + 512 <=
// usable_size` is the contract; the boundary is exactly -512/-511).
func TestBalancerWholeFileThresholdHonestFit(t *testing.T) {
	budget := int64(2_000_000)
	usable := budgetUsable(budget)
	reserve := units.FixedImageOverhead + units.ImageEntryOverhead // 2,098,688
	s := newFakeStore(testProfile(budget))
	addFile(s, 1, "data/whole", usable-reserve)     // the exact fit: whole
	addFile(s, 2, "data/chunked", usable-reserve+1) // one into the top band: chunked

	res, err := runPlan(t, s)
	if err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	// The exact-fit file is a whole-file item with no chunk rows.
	if imageOf(s, 1) == 0 {
		t.Error("exact-fit file was not placed as a whole item")
	}
	if len(s.chunkRows[1]) != 0 {
		t.Errorf("exact-fit file produced %d chunk rows, want 0", len(s.chunkRows[1]))
	}
	// The top-band file is chunked: budget - 512 = 1,999,488 effective
	// yields 2 chunks (1,999,488 + 1), both assigned.
	rows := s.chunkRows[2]
	if len(rows) != 2 || rows[0].length != budget-units.ImageEntryOverhead || rows[1].length != 1 {
		t.Fatalf("top-band decomposition = %+v, want [%d, 1]", rows, budget-units.ImageEntryOverhead)
	}
	for i, c := range rows {
		if c.imageID == 0 {
			t.Errorf("top-band chunk %d unassigned", i)
		}
	}
	if res.FilesAssigned != 1 || res.ChunksAssigned != 2 {
		t.Errorf("counts = (files %d, chunks %d), want (1, 2)", res.FilesAssigned, res.ChunksAssigned)
	}
	// Both files' images satisfy the buildability invariant (the lead
	// 12.5 property on the tightest possible fixtures).
	for _, id := range sortedKeys64(s.images) {
		st := s.stateOf(id)
		if required := units.RequiredImageBytes(st.UsedBytes, int64(st.ItemCount+st.DirCount)); required > st.UsableSize {
			t.Errorf("image %d: RequiredImageBytes = %d > usable %d (threshold fixture violates the invariant)",
				id, required, st.UsableSize)
		}
	}
}
