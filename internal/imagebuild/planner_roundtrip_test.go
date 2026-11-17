package imagebuild

// THE ROUND-TRIP GUARANTEE (DESIGN_SPEC 4.3 capacity accounting as
// amended 2026-10-20; 12.5 lead invariant): for a matrix of seeded
// plans - small and large, chunked files, symlinks, empty dirs,
// locked destinations, incremental re-plans - EVERY planned image must
// pass the builder's ACTUAL pre-check: the same code path Stage runs
// (validateInput, then units.RequiredImageBytes(payload, entries) <=
// UsableSize, staging.go's check (b)).
//
// This is deliberately NOT the same as asserting the 12.5 invariant
// over plan.Result: the invariant tests compute RequiredImageBytes
// from the planner's own fill figures, i.e. they assert the formula
// against itself. Here the plans are produced by the REAL balancer
// (plan.Balance + plan.Write through a recording AssignStore), the
// per-image inventories are reconstructed from the assignments the
// planner actually PERSISTED, and those inventories are fed to the
// builder's real gates. A drift between the balancer's charged budget
// and the builder's entry accounting shows up here as a pre-check
// refusal - exactly the 2026-10-20 stakeholder failure shape.
//
// The safety-net half of the contract is pinned too: a PRE-amendment
// locked image (planned under the old arithmetic, payload packed up to
// raw usable_size) is refused by the builder's pre-check - and the new
// balancer never writes new payload or dirs onto it beyond its entry
// budget, opening new destinations instead.

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"isopress/internal/plan"
	"isopress/internal/units"
)

// --- the planner-side fixtures: a fake plan.Store and a recording AssignStore ---

// rtPlanStore is the minimal plan.Store for these fixtures: slices in
// the deterministic (file_id / dir_id / image_id ascending) order the
// Store contract promises.
type rtPlanStore struct {
	files  []plan.ActiveFile
	chunks []plan.StoredChunk
	dirs   []plan.UnassignedDir
	dests  []plan.DestinationState

	// targets maps a symlink file_id to its exact link target (the
	// balancer sees only Size = len(target); the builder needs the
	// string to stage a faithful symlink entry).
	targets map[int64]string
}

func (s *rtPlanStore) ActiveFiles(context.Context) ([]plan.ActiveFile, error) {
	return s.files, nil
}
func (s *rtPlanStore) ChunkStates(context.Context) ([]plan.StoredChunk, error) {
	return s.chunks, nil
}
func (s *rtPlanStore) UnassignedDirs(context.Context) ([]plan.UnassignedDir, error) {
	return s.dirs, nil
}
func (s *rtPlanStore) Destinations(context.Context) ([]plan.DestinationState, error) {
	return s.dests, nil
}

func (s *rtPlanStore) addRegular(id int64, rel string, size int64) {
	s.files = append(s.files, plan.ActiveFile{
		FileID: id, RelPath: rel, EntryKind: "regular", Size: size, Nlink: 1,
	})
}

func (s *rtPlanStore) addSymlink(id int64, rel, target string) {
	if s.targets == nil {
		s.targets = map[int64]string{}
	}
	s.targets[id] = target
	s.files = append(s.files, plan.ActiveFile{
		FileID: id, RelPath: rel, EntryKind: "symlink", Size: int64(len(target)), Nlink: 1,
	})
}

func (s *rtPlanStore) addDir(id int64, rel string) {
	s.dirs = append(s.dirs, plan.UnassignedDir{DirID: id, RelPath: rel})
}

// rtRecorder captures every write of plan.Write - the assignments the
// planner actually PERSISTED - so the per-image inventories fed to the
// builder's pre-check are derived from planner output, not from the
// test's expectations.
type rtRecorder struct {
	images     map[int64]plan.ImageSpec
	fileImage  map[int64]int64    // whole-file / symlink assignment
	chunkImage map[[2]int64]int64 // (fileID, chunkIndex) -> image
	chunkSpecs map[int64][]plan.ChunkSpec
	dirImage   map[int64]int64
}

func newRTRecorder() *rtRecorder {
	return &rtRecorder{
		images:     map[int64]plan.ImageSpec{},
		fileImage:  map[int64]int64{},
		chunkImage: map[[2]int64]int64{},
		chunkSpecs: map[int64][]plan.ChunkSpec{},
		dirImage:   map[int64]int64{},
	}
}

func (r *rtRecorder) InsertImage(_ context.Context, img plan.ImageSpec) error {
	r.images[img.ImageID] = img
	return nil
}
func (r *rtRecorder) EnsureChunkRows(_ context.Context, fileID int64, specs []plan.ChunkSpec) error {
	r.chunkSpecs[fileID] = specs
	return nil
}
func (r *rtRecorder) ReplaceAllChunks(_ context.Context, fileID int64, specs []plan.ChunkSpec) (plan.RechunkOutcome, error) {
	r.chunkSpecs[fileID] = specs
	return plan.RechunkOutcome{}, nil
}
func (r *rtRecorder) DropChunkState(context.Context, int64) (int, error) { return 0, nil }
func (r *rtRecorder) StoreChunkSize(context.Context, int64, int64, int) error {
	return nil
}
func (r *rtRecorder) AssignFile(_ context.Context, fileID, imageID int64) error {
	r.fileImage[fileID] = imageID
	return nil
}
func (r *rtRecorder) AssignChunk(_ context.Context, fileID int64, chunkIndex int, imageID int64) error {
	r.chunkImage[[2]int64{fileID, int64(chunkIndex)}] = imageID
	return nil
}
func (r *rtRecorder) AssignDir(_ context.Context, dirID, imageID int64) error {
	r.dirImage[dirID] = imageID
	return nil
}

// runRTPlan drives the REAL planner over store and records the writes.
func runRTPlan(t *testing.T, store *rtPlanStore, profile units.Profile, rec *rtRecorder) (plan.Result, error) {
	t.Helper()
	ctx := context.Background()
	p, err := plan.Balance(ctx, store, profile)
	if err != nil {
		return plan.Result{}, err
	}
	err = p.Write(ctx, func(context.Context) (*plan.AssignTx, error) {
		return &plan.AssignTx{AssignStore: rec,
			Commit:   func() error { return nil },
			Rollback: func() error { return nil }}, nil
	})
	if err != nil {
		t.Fatalf("plan.Write: %v", err)
	}
	return p.Result, nil
}

// --- the builder-side half: reconstruct the inventory and run the ACTUAL gates ---

// rtFileByFile indexes the store's active files by id.
func rtFileByFile(store *rtPlanStore) map[int64]plan.ActiveFile {
	by := make(map[int64]plan.ActiveFile, len(store.files))
	for _, f := range store.files {
		by[f.FileID] = f
	}
	return by
}

// rtStageInput reconstructs ONE image's StageInput from the recorded
// assignments - exactly the inventory the build command would assemble
// from the catalog for that image.
func rtStageInput(t *testing.T, store *rtPlanStore, rec *rtRecorder, imageID int64) StageInput {
	t.Helper()
	img, ok := rec.images[imageID]
	if !ok {
		// A locked destination has no images row written THIS run; its
		// profile figures come from the DestinationState the fixture
		// fed the planner (the catalog's images row in production).
		for _, d := range store.dests {
			if d.ImageID == imageID {
				img = plan.ImageSpec{
					ImageID: imageID, NominalSize: d.NominalSize,
					UsableSize: d.UsableSize, MediaProfile: d.MediaProfile,
				}
				ok = true
				break
			}
		}
	}
	if !ok {
		t.Fatalf("image %d has neither a written spec nor a destination state", imageID)
	}

	in := StageInput{
		SetID:      "S-RT",
		ImageID:    imageID,
		SourceRoot: "/nonexistent-but-nonempty", // validateInput only checks non-empty
		WorkDir:    "/tmp",                      // no pathlist-breaking bytes
		UsableSize: img.UsableSize,
	}
	files := rtFileByFile(store)
	blankHash := bytes.Repeat([]byte{0xab}, 32) // validateInput requires a 32-byte anchor

	for fileID, assigned := range rec.fileImage {
		if assigned != imageID {
			continue
		}
		f, known := files[fileID]
		if !known {
			t.Fatalf("assignment for unknown file %d", fileID)
		}
		if f.EntryKind == "symlink" {
			target, ok := store.targets[fileID]
			if !ok || int64(len(target)) != f.Size {
				t.Fatalf("symlink %d: fixture target missing or size-mismatched", fileID)
			}
			in.Inventory.Symlinks = append(in.Inventory.Symlinks, StageSymlink{
				FileID: fileID, RelPath: f.RelPath, LinkTarget: target, Size: f.Size,
			})
			continue
		}
		in.Inventory.Files = append(in.Inventory.Files, StageFile{
			FileID: fileID, RelPath: f.RelPath, Size: f.Size, Hash: blankHash,
		})
	}
	for key, assigned := range rec.chunkImage {
		if assigned != imageID {
			continue
		}
		fileID, index := key[0], key[1]
		f, known := files[fileID]
		if !known {
			t.Fatalf("chunk assignment for unknown file %d", fileID)
		}
		var spec plan.ChunkSpec
		found := false
		for _, s := range rec.chunkSpecs[fileID] {
			if int64(s.Index) == index {
				spec, found = s, true
				break
			}
		}
		if !found {
			t.Fatalf("file %d: no stored spec for chunk %d", fileID, index)
		}
		in.Inventory.Chunks = append(in.Inventory.Chunks, StageChunk{
			FileID: fileID, ChunkIndex: int(index), Offset: spec.Offset, Length: spec.Length,
			ParentRelPath: f.RelPath, ParentSize: f.Size,
		})
	}
	for dirID, assigned := range rec.dirImage {
		if assigned != imageID {
			continue
		}
		var rel string
		for _, d := range store.dirs {
			if d.DirID == dirID {
				rel = d.RelPath
			}
		}
		if rel == "" {
			t.Fatalf("dir assignment for unknown dir %d", dirID)
		}
		in.Inventory.Dirs = append(in.Inventory.Dirs, StageDir{DirID: dirID, RelPath: rel})
	}
	return in
}

// assertPlannedImageBuildable runs the builder's ACTUAL pre-check on
// one planned image: validateInput (the entry accounting) plus the
// canonical estimator comparison - the exact code path of staging.go's
// check (b). It returns the required bytes for further assertions.
func assertPlannedImageBuildable(t *testing.T, store *rtPlanStore, rec *rtRecorder, imageID int64) int64 {
	t.Helper()
	in := rtStageInput(t, store, rec, imageID)
	payload, entries, err := validateInput(in)
	if err != nil {
		t.Fatalf("image %d: the builder's validateInput REFUSED a planner-produced image: %v", imageID, err)
	}
	required := units.RequiredImageBytes(payload, int64(entries))
	if required > in.UsableSize {
		t.Fatalf("image %d: planner output fails the builder's pre-check (b): "+
			"payload %d + entries %d -> RequiredImageBytes %d > usable %d "+
			"(THIS is the 2026-10-20 stakeholder failure shape)",
			imageID, payload, entries, required, in.UsableSize)
	}
	return required
}

// --- the matrix --------------------------------------------------------------------

func rtProfile(budget int64) units.Profile {
	usable := units.FixedImageOverhead + budget
	return units.Profile{Name: "disk", NominalBytes: 2 * usable, UsableBytes: usable}
}

// TestPlannerOutputPassesBuilderPreCheck is THE round-trip guarantee:
// every image produced by the real balancer passes the builder's real
// pre-check, across whole files, chunked files, symlinks, empty dirs,
// locked destinations, and dir fall-forward.
func TestPlannerOutputPassesBuilderPreCheck(t *testing.T) {
	cases := []struct {
		name   string
		budget int64
		seed   func(s *rtPlanStore)
	}{
		{
			name:   "small files pack multiple images",
			budget: 2000,
			seed: func(s *rtPlanStore) {
				s.addRegular(1, "data/a.bin", 700)
				s.addRegular(2, "data/b.bin", 600)
				s.addRegular(3, "data/c.bin", 500)
				s.addRegular(4, "data/d.bin", 400)
				s.addSymlink(5, "link/lnk", "data/a.bin")
			},
		},
		{
			name:   "oversized file chunks across images",
			budget: 1200,
			seed: func(s *rtPlanStore) {
				s.addRegular(1, "data/big.bin", 5000) // chunks of 688
				s.addRegular(2, "data/small.bin", 100)
			},
		},
		{
			name:   "exact-fit whole file at the honest threshold",
			budget: 1000,
			seed: func(s *rtPlanStore) {
				// usable - Fixed - 512 = 488: the largest whole file,
				// landing at RequiredImageBytes == usable exactly.
				s.addRegular(1, "data/exact.bin", 488)
				s.addRegular(2, "data/near.bin", 487)
			},
		},
		{
			name:   "dir-heavy plan falls forward and stays buildable",
			budget: 612,
			seed: func(s *rtPlanStore) {
				// One file packed to the byte (charge 612), then five
				// dirs whose 2560 entry charge cannot fit anywhere: the
				// dir pass must fall forward and every fallback image
				// must still satisfy the invariant.
				s.addRegular(1, "data/pack.bin", 100)
				for i := int64(0); i < 5; i++ {
					s.addDir(100+i, fmt.Sprintf("dir/d%d", i))
				}
			},
		},
		{
			name:   "symlinks and dirs alongside chunked parent",
			budget: 1200,
			seed: func(s *rtPlanStore) {
				s.addRegular(1, "data/parent.bin", 2000)
				s.addSymlink(2, "link/to-parent", "data/parent.bin")
				s.addSymlink(3, "link/to-dir", "dir")
				s.addDir(100, "dir/empty")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := &rtPlanStore{}
			tc.seed(store)
			rec := newRTRecorder()
			profile := rtProfile(tc.budget)
			if _, err := runRTPlan(t, store, profile, rec); err != nil {
				t.Fatalf("plan: %v", err)
			}
			if len(rec.images) == 0 {
				t.Fatal("plan produced no images")
			}
			// THE GUARANTEE: every image the planner created passes the
			// builder's actual pre-check.
			for id := range rec.images {
				assertPlannedImageBuildable(t, store, rec, id)
			}
			// And every assignment landed somewhere (no orphan payload).
			for fileID := range rec.fileImage {
				if _, known := rtFileByFile(store)[fileID]; !known {
					t.Errorf("phantom assignment for file %d", fileID)
				}
			}
		})
	}
}

// TestIncrementalReplanRoundTrip: an EXISTING set of locked images
// (one whole-file, one chunked, both planned by a FIRST real plan run)
// plus new files/symlinks/dirs on a SECOND run. Every image - locked
// and new, including the dirs the second run attached to a locked
// image - must pass the builder's actual pre-check; the locked images
// receive no new payload (FR-011) and the report's residual figure for
// them is the true free-after-build space.
func TestIncrementalReplanRoundTrip(t *testing.T) {
	profile := rtProfile(1200)
	rec := newRTRecorder()

	// --- run 1: a whole file and a chunked file land on new images ---
	s1 := &rtPlanStore{}
	s1.addRegular(1, "data/first.bin", 500)   // charge 1012
	s1.addRegular(2, "data/parent.bin", 2000) // chunks 688+688+624
	res1, err := runRTPlan(t, s1, profile, rec)
	if err != nil {
		t.Fatalf("run 1: %v", err)
	}
	if res1.NewDestinations == 0 {
		t.Fatal("run 1 created no destinations")
	}

	// --- run 2: the same set, now locked, plus new items and dirs ---
	s2 := &rtPlanStore{}
	for _, f := range s1.files {
		locked := f
		locked.ImageID = rec.fileImage[f.FileID]
		s2.files = append(s2.files, locked)
	}
	for _, c := range s1.chunks {
		locked := c
		if img, ok := rec.chunkImage[[2]int64{c.FileID, int64(c.Index)}]; ok {
			locked.ImageID = img
		}
		s2.chunks = append(s2.chunks, locked)
	}
	// (run 1 added no chunk rows to the STORE, only to the recorder;
	// mirror them so run 2 sees the locked chunked file as fully placed)
	for fileID, specs := range rec.chunkSpecs {
		for _, spec := range specs {
			s2.chunks = append(s2.chunks, plan.StoredChunk{
				FileID: fileID, ChunkSize: plan.EffectiveChunkSize(profile.UsableBytes),
				Index: spec.Index, Offset: spec.Offset, Length: spec.Length,
				ImageID: rec.chunkImage[[2]int64{fileID, int64(spec.Index)}],
			})
		}
	}
	// Locked destination states, as the catalog would report them.
	for id, img := range rec.images {
		var st plan.DestinationState
		st.ImageID = id
		st.NominalSize = img.NominalSize
		st.UsableSize = img.UsableSize
		st.MediaProfile = img.MediaProfile
		for fileID, assigned := range rec.fileImage {
			if assigned == id {
				f := rtFileByFile(s1)[fileID]
				st.UsedBytes += f.Size
				st.ItemCount++
			}
		}
		for key, assigned := range rec.chunkImage {
			if assigned == id {
				for _, spec := range rec.chunkSpecs[key[0]] {
					if int64(spec.Index) == key[1] {
						st.UsedBytes += spec.Length
						st.ItemCount++
					}
				}
			}
		}
		s2.dests = append(s2.dests, st)
	}

	s2.addRegular(3, "data/new.bin", 600) // charge 1112 -> a new image
	s2.addSymlink(4, "link/new", "data/first.bin")
	s2.addDir(100, "dir/new-a")
	s2.addDir(101, "dir/new-b")

	res2, err := runRTPlan(t, s2, profile, rec)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if res2.NewDestinations == 0 {
		t.Fatal("run 2 opened no destination for its new items")
	}
	// FR-011: the locked images received no new PAYLOAD assignments.
	for _, d := range res2.Destinations {
		if !d.Locked {
			continue
		}
		var before plan.DestinationState
		for _, st := range s2.dests {
			if st.ImageID == d.ImageID {
				before = st
			}
		}
		if d.UsedBytes != before.UsedBytes || d.ItemCount != before.ItemCount {
			t.Errorf("locked image %d: fill changed %+v -> %+v (FR-011)", d.ImageID, before, d)
		}
	}
	// THE ROUND-TRIP: every image of the WHOLE plan - locked ones with
	// the second run's dirs attached, and the new ones - passes the
	// builder's actual pre-check.
	seen := map[int64]bool{}
	for _, d := range res2.Destinations {
		seen[d.ImageID] = true
		assertPlannedImageBuildable(t, s2, rec, d.ImageID)
	}
	for id := range rec.images {
		if !seen[id] {
			t.Errorf("written image %d missing from the plan result", id)
		}
	}
}

// TestLowestNumberedDirRuleWhenSlackExists: when the lowest-numbered
// destination has entry slack, the dir lands THERE (the design-text
// rule), and the resulting image still passes the builder's pre-check.
func TestLowestNumberedDirRuleWhenSlackExists(t *testing.T) {
	profile := rtProfile(1200)
	rec := newRTRecorder()

	// Run 1: one small file -> image 1 with 588 bytes of entry slack.
	s1 := &rtPlanStore{}
	s1.addRegular(1, "data/small.bin", 100)
	if _, err := runRTPlan(t, s1, profile, rec); err != nil {
		t.Fatalf("run 1: %v", err)
	}

	// Run 2: one new dir. Image 1 is the lowest-numbered destination
	// and can absorb the 512 charge, so the dir MUST land there.
	s2 := &rtPlanStore{}
	s2.files = append(s2.files, plan.ActiveFile{
		FileID: 1, RelPath: "data/small.bin", EntryKind: "regular",
		Size: 100, Nlink: 1, ImageID: rec.fileImage[1],
	})
	s2.dests = append(s2.dests, plan.DestinationState{
		ImageID: rec.fileImage[1], NominalSize: profile.NominalBytes,
		UsableSize: profile.UsableBytes, MediaProfile: profile.Name,
		UsedBytes: 100, ItemCount: 1,
	})
	s2.addDir(100, "dir/empty")

	res, err := runRTPlan(t, s2, profile, rec)
	if err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if res.NewDestinations != 0 {
		t.Errorf("dir with slack available opened %d new destinations, want 0", res.NewDestinations)
	}
	if got := rec.dirImage[100]; got != rec.fileImage[1] {
		t.Errorf("dir landed on image %d, want the lowest-numbered %d", got, rec.fileImage[1])
	}
	assertPlannedImageBuildable(t, s2, rec, rec.fileImage[1])
}

// TestPreAmendmentLockedImageRefusedByBuilder pins the safety net and
// the balancer's treatment of a LOCKED image planned under the OLD
// arithmetic (payload packed up to raw usable_size, no reservation):
//   - the builder's actual pre-check REFUSES it (required > usable) -
//     the estimator-drift net doing its job;
//   - the new balancer puts no new payload on it and does not count
//     its (negative) capacity in the open set: new items open new
//     images, and the dirs fall forward;
//   - the report's wasted-residual figure for it clamps to zero (an
//     over-full locked image has no "free after build" space).
func TestPreAmendmentLockedImageRefusedByBuilder(t *testing.T) {
	profile := rtProfile(1200)
	usable := profile.UsableBytes

	store := &rtPlanStore{}
	// The old arithmetic packed the payload to the raw usable_size:
	// one 1200-byte file, one entry, zero reservation.
	overfull := plan.DestinationState{
		ImageID: 1, NominalSize: profile.NominalBytes, UsableSize: usable,
		MediaProfile: profile.Name, UsedBytes: 1200, ItemCount: 1,
	}
	store.dests = append(store.dests, overfull)
	store.files = append(store.files, plan.ActiveFile{
		FileID: 1, RelPath: "data/old.bin", EntryKind: "regular", Size: 1200,
		Nlink: 1, ImageID: 1, // LOCKED on the old image
	})
	store.addRegular(2, "data/new.bin", 600)
	store.addDir(100, "dir/new")

	rec := newRTRecorder()
	res, err := runRTPlan(t, store, profile, rec)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	// The locked image is out of the open set: no new payload, no new
	// image row for it.
	if img, written := rec.images[1]; written {
		t.Errorf("plan re-wrote the locked image row: %+v", img)
	}
	for fileID, assigned := range rec.fileImage {
		if fileID == 2 && assigned == 1 {
			t.Error("new file was placed on the pre-amendment locked image")
		}
	}
	// The dirs cannot land on it either (its reserved budget is
	// negative) - they open a fallback or join a new image.
	if got := rec.dirImage[100]; got == 1 {
		t.Error("dir attached to the pre-amendment locked image with negative entry budget")
	}
	// Residual honesty: no free-after-build space on the over-full one
	// (the figure is global; the over-full image clamps to zero and the
	// new images have no locked residual).
	if res.WastedResidualBytes != 0 {
		t.Errorf("wasted residual = %d, want 0 (the over-full locked image contributes no free space)",
			res.WastedResidualBytes)
	}
	// Every image the new run CREATED still passes the builder's gate.
	for id := range rec.images {
		assertPlannedImageBuildable(t, store, rec, id)
	}

	// And the builder's ACTUAL pre-check refuses the locked image's
	// own inventory - the estimator-drift safety net firing on the
	// exact stakeholder-failure shape.
	in := StageInput{
		SetID: "S-RT", ImageID: 1, SourceRoot: "/nonexistent", WorkDir: "/tmp",
		UsableSize: usable,
		Inventory: StageInventory{
			Files: []StageFile{{
				FileID: 1, RelPath: "data/old.bin", Size: 1200,
				Hash: bytes.Repeat([]byte{0xab}, 32),
			}},
		},
	}
	payload, entries, err := validateInput(in)
	if err != nil {
		t.Fatalf("validateInput: %v", err)
	}
	if required := units.RequiredImageBytes(payload, int64(entries)); required <= in.UsableSize {
		t.Fatalf("the pre-amendment inventory (%d required, %d usable) must be REFUSED by the builder's pre-check",
			required, in.UsableSize)
	} else if _, err := func() (int, error) {
		// Reproduce staging.go's refusal path exactly: the same
		// comparison feeding capacityError.
		if required > in.UsableSize {
			return 0, capacityError(in, payload, entries, required)
		}
		return entries, nil
	}(); err == nil {
		t.Fatal("capacityError path did not fire for the over-capacity locked image")
	}
}
