package restore

// T5.3 acceptance: selection resolution (DESIGN_SPEC 8.1 D-07 latest-state,
// 8.2 selection model; IMPLEMENTATION_PLAN 4.6 T5.3 acceptance criteria
// "selection by whole set / --path / --subtree", "a partial hard-link family
// selection restores the family content once and links the requested names",
// "chunked records resolve to all their images").
//
// Every test drives ResolveSelection against the fake Store - the same seam
// the cli adapts *catalog.DB onto - so what is asserted here is exactly what
// the production run computes from real catalog reads.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// multiImageStore is the two-image fixture the resolution tests share:
//
//	image 1: whole files small.txt + fam-a (family primary), the empty dir,
//	         the hard-link SECONDARY claim fam-b (bytes live next to fam-a),
//	         and chunk 0 of big.bin;
//	image 2: chunk 1 of big.bin and the symlink link.
//
// placement claims fam-b on image 1 (the unified T3.4 rule) and big.bin on
// BOTH images (a chunked parent is claimed by every image holding a chunk).
func multiImageStore() *fakeStore {
	s := &fakeStore{
		records: []Record{
			{FileID: 1, RelPath: "big.bin", EntryKind: "regular", Size: 1000, Hash: []byte{1}},
			{FileID: 2, RelPath: "small.txt", EntryKind: "regular", Size: 100, Hash: []byte{2}},
			{FileID: 3, RelPath: "fam-a.txt", EntryKind: "regular", Size: 50, Hash: []byte{3}},
			{FileID: 4, RelPath: "fam-b.txt", EntryKind: "regular", Size: 50, Hash: []byte{3}},
			{FileID: 5, RelPath: "link", EntryKind: "symlink", Size: 9, LinkTarget: "small.txt"},
		},
		dirs: []DirRecord{{DirID: 1, RelPath: "empty-dir"}},
		placement: []ImagePaths{
			{ImageID: 1, Paths: []string{"big.bin", "empty-dir", "fam-a.txt", "fam-b.txt", "small.txt"}},
			{ImageID: 2, Paths: []string{"big.bin", "link"}},
		},
		inventory: map[int64]Inventory{
			1: {
				Files: []FileItem{
					{FileID: 2, RelPath: "small.txt", Size: 100, Hash: []byte{2}},
					{FileID: 3, RelPath: "fam-a.txt", Size: 50, Hash: []byte{3}, Nlink: 2},
				},
				Dirs: []DirItem{{DirID: 1, RelPath: "empty-dir", Mode: 0o750}},
				Secondaries: []LinkItem{{
					FileID: 4, RelPath: "fam-b.txt", Size: 50, Hash: []byte{3},
					PrimaryFileID: 3, PrimaryRelPath: "fam-a.txt",
				}},
				Chunks: []ChunkItem{{
					ChunkID: 1, FileID: 1, ChunkIndex: 0, Offset: 0, Length: 600,
					Hash: []byte{9}, ParentRelPath: "big.bin", ParentSize: 1000,
					ParentHash: []byte{1}, ParentNumChunks: 2,
				}},
			},
			2: {
				Chunks: []ChunkItem{{
					ChunkID: 2, FileID: 1, ChunkIndex: 1, Offset: 600, Length: 400,
					Hash: []byte{8}, ParentRelPath: "big.bin", ParentSize: 1000,
					ParentHash: []byte{1}, ParentNumChunks: 2,
				}},
				Symlinks: []SymlinkItem{{FileID: 5, RelPath: "link", LinkTarget: "small.txt", Size: 9}},
			},
		},
	}
	return s
}

func resolve(t *testing.T, store *fakeStore, sel Selector) ResolvedSelection {
	t.Helper()
	out, err := ResolveSelection(context.Background(), store, sel)
	if err != nil {
		t.Fatalf("ResolveSelection(%+v): %v", sel, err)
	}
	return out
}

// --- whole set, --path, --subtree -------------------------------------------

func TestResolveSelectionWholeSet(t *testing.T) {
	store := multiImageStore()
	got := resolve(t, store, Selector{SetID: "S"})

	wantPaths := []string{"big.bin", "empty-dir", "fam-a.txt", "fam-b.txt", "link", "small.txt"}
	if strings.Join(got.Paths, ",") != strings.Join(wantPaths, ",") {
		t.Errorf("whole-set paths = %v, want %v", got.Paths, wantPaths)
	}
	if got.Mode != "whole-set" {
		t.Errorf("mode = %q, want whole-set", got.Mode)
	}
	// Both images are required, ascending: the loop order is the report and
	// the supply order.
	if len(got.Work) != 2 || got.Work[0].Image != 1 || got.Work[1].Image != 2 {
		t.Fatalf("required images = %+v, want [1 2] ascending", got.Work)
	}
	// Totals are the SELECTION's records (8.4 determinate progress): five
	// file records (dirs are zero-byte members, excluded from Files).
	if got.Files != 5 || got.Bytes != 1209 {
		t.Errorf("totals = %d files / %d bytes, want 5 / 1209", got.Files, got.Bytes)
	}
	if len(got.Unclaimed) != 0 {
		t.Errorf("unclaimed = %v, want none", got.Unclaimed)
	}
	// The prompt path (<path> of 8.3) must name a path the image holds.
	if got.Work[0].NeededFor == "" || got.Work[1].NeededFor == "" {
		t.Errorf("every required image needs a NeededFor path: %+v", got.Work)
	}
	if got.Work[1].NeededFor != "big.bin" && got.Work[1].NeededFor != "link" {
		t.Errorf("image 2 NeededFor = %q, want one of its claimed paths", got.Work[1].NeededFor)
	}
}

func TestResolveSelectionExactPathsRepeatable(t *testing.T) {
	store := multiImageStore()
	got := resolve(t, store, Selector{SetID: "S", Paths: []string{"small.txt", "link", "small.txt"}})

	if strings.Join(got.Paths, ",") != "link,small.txt" {
		t.Errorf("selected paths = %v, want link+small.txt (deduped, ascending)", got.Paths)
	}
	if got.Mode != "path" {
		t.Errorf("mode = %q, want path", got.Mode)
	}
	// small.txt lives on image 1, link on image 2: both required.
	if len(got.Work) != 2 {
		t.Fatalf("required images = %+v, want both", got.Work)
	}
	if len(got.Work[0].Inv.Files) != 1 || got.Work[0].Inv.Files[0].RelPath != "small.txt" {
		t.Errorf("image 1 work = %+v, want ONLY small.txt (unselected members are filtered out)", got.Work[0].Inv.Files)
	}
	if len(got.Work[1].Inv.Symlinks) != 1 || got.Work[1].Inv.Symlinks[0].RelPath != "link" {
		t.Errorf("image 2 work = %+v, want only the symlink", got.Work[1].Inv.Symlinks)
	}
}

func TestResolveSelectionPathMatchingNothingIsInputError(t *testing.T) {
	store := multiImageStore()
	_, err := ResolveSelection(context.Background(), store, Selector{SetID: "S", Paths: []string{"nope.txt"}})
	var input *InputError
	if !errors.As(err, &input) {
		t.Fatalf("err = %v, want *InputError (the cli maps it to exit 2)", err)
	}
	if !strings.Contains(err.Error(), "nope.txt") || !strings.Contains(err.Error(), "--subtree") {
		t.Errorf("message must name the offending path and point at --subtree: %v", err)
	}
}

func TestResolveSelectionSubtreeIsComponentAware(t *testing.T) {
	store := &fakeStore{
		records: []Record{
			{FileID: 1, RelPath: "docs/a.txt", EntryKind: "regular", Size: 1},
			{FileID: 2, RelPath: "docs/b.txt", EntryKind: "regular", Size: 2},
			{FileID: 3, RelPath: "docs/sub/c.txt", EntryKind: "regular", Size: 3},
			{FileID: 4, RelPath: "docs-old/d.txt", EntryKind: "regular", Size: 4},
		},
		dirs: []DirRecord{
			{DirID: 1, RelPath: "docs/empty"},
			{DirID: 2, RelPath: "docs-old/empty"},
		},
		placement: []ImagePaths{
			{ImageID: 1, Paths: []string{"docs/a.txt", "docs/b.txt", "docs/empty", "docs/sub/c.txt"}},
			{ImageID: 2, Paths: []string{"docs-old/d.txt", "docs-old/empty"}},
		},
		inventory: map[int64]Inventory{
			1: {Files: []FileItem{
				{FileID: 1, RelPath: "docs/a.txt", Size: 1},
				{FileID: 2, RelPath: "docs/b.txt", Size: 2},
				{FileID: 3, RelPath: "docs/sub/c.txt", Size: 3},
			}, Dirs: []DirItem{{DirID: 1, RelPath: "docs/empty"}}},
			2: {Files: []FileItem{{FileID: 4, RelPath: "docs-old/d.txt", Size: 4}},
				Dirs: []DirItem{{DirID: 2, RelPath: "docs-old/empty"}}},
		},
	}

	got := resolve(t, store, Selector{SetID: "S", Subtree: "docs/"})
	want := []string{"docs/a.txt", "docs/b.txt", "docs/empty", "docs/sub/c.txt"}
	if strings.Join(got.Paths, ",") != strings.Join(want, ",") {
		t.Errorf("--subtree docs/ selected %v, want %v (component prefix; docs-old must stay out)", got.Paths, want)
	}
	if len(got.Work) != 1 || got.Work[0].Image != 1 {
		t.Fatalf("required images = %+v, want only image 1", got.Work)
	}
	if got.Mode != "subtree" {
		t.Errorf("mode = %q, want subtree", got.Mode)
	}

	// An empty subtree is an input error, never a silent empty run.
	_, err := ResolveSelection(context.Background(), store, Selector{SetID: "S", Subtree: "nothing-here"})
	var input *InputError
	if !errors.As(err, &input) {
		t.Fatalf("empty --subtree err = %v, want *InputError", err)
	}
}

func TestResolveSelectionRejectsUnsafeRelPaths(t *testing.T) {
	store := multiImageStore()
	for _, bad := range []string{"/etc/passwd", "../escape", "a/../../b"} {
		_, err := ResolveSelection(context.Background(), store, Selector{SetID: "S", Paths: []string{bad}})
		var input *InputError
		if !errors.As(err, &input) {
			t.Errorf("--path %q: err = %v, want *InputError", bad, err)
		}
	}
	_, err := ResolveSelection(context.Background(), store, Selector{SetID: "S", Subtree: ".."})
	var input *InputError
	if !errors.As(err, &input) {
		t.Errorf("--subtree ..: err = %v, want *InputError", err)
	}
}

// --- latest state (D-07) -----------------------------------------------------

// A superseded version of a path is simply NOT in the active view the Store
// exposes, so the selection can only ever see one row per path: no second
// entry, no stale hash, no read besides the three active views.
func TestResolveSelectionIsLatestStateOnly(t *testing.T) {
	store := &fakeStore{
		// The active row of data.txt (FileID 2, post-modification). Its
		// superseded predecessor (FileID 1) exists in the catalog but is
		// absent from ActiveRecords - which is precisely the v_active_files
		// predicate the cli's adapter runs.
		records: []Record{
			{FileID: 2, RelPath: "data.txt", EntryKind: "regular", Size: 20, Hash: []byte{0x22}},
		},
		placement: []ImagePaths{{ImageID: 1, Paths: []string{"data.txt"}}},
		inventory: map[int64]Inventory{
			1: {Files: []FileItem{{FileID: 2, RelPath: "data.txt", Size: 20, Hash: []byte{0x22}}}},
		},
	}
	got := resolve(t, store, Selector{SetID: "S"})
	if strings.Join(got.Paths, ",") != "data.txt" {
		t.Errorf("paths = %v, want exactly the one active record", got.Paths)
	}
	if got.Files != 1 || got.Bytes != 20 {
		t.Errorf("totals = %d/%d, want 1 file / 20 bytes (the superseded row must not double-count)", got.Files, got.Bytes)
	}
	// The run only ever reads the three active views - there is no
	// "all rows" path through this package.
	wantReads := []string{"records", "dirs", "placement", "inventory"}
	if strings.Join(store.reads, ",") != strings.Join(wantReads, ",") {
		t.Errorf("store reads = %v, want %v", store.reads, wantReads)
	}
}

// --- hard-link families (8.2: content once, links for the requested names) ---

func TestResolveSelectionPartialFamilySelectsSecondaryOnly(t *testing.T) {
	store := multiImageStore()
	got := resolve(t, store, Selector{SetID: "S", Paths: []string{"fam-b.txt"}})

	// The secondary's bytes live on the PRIMARY's image (the unified T3.4
	// claim rule): placement put fam-b on image 1, so image 1 is required.
	if len(got.Work) != 1 || got.Work[0].Image != 1 {
		t.Fatalf("required images = %+v, want image 1 (the primary's image)", got.Work)
	}
	work := got.Work[0].Inv
	if len(work.Files) != 0 {
		t.Errorf("work files = %v, want NONE: the unselected primary must not be restored as a whole file", rels(work.Files))
	}
	if len(work.Secondaries) != 1 || work.Secondaries[0].RelPath != "fam-b.txt" {
		t.Fatalf("work secondaries = %+v, want exactly the requested name", work.Secondaries)
	}
	if work.Secondaries[0].PrimaryRelPath != "fam-a.txt" || work.Secondaries[0].PrimaryFileID != 3 {
		t.Errorf("secondary primary = %q/%d, want fam-a.txt/3 (T5.4 reads the content there and creates ONLY fam-b)",
			work.Secondaries[0].PrimaryRelPath, work.Secondaries[0].PrimaryFileID)
	}
	if len(work.Secondaries[0].Hash) == 0 {
		t.Error("the shared family hash must travel with the secondary (8.4 verifies content against it)")
	}
}

func TestResolveSelectionPartialFamilySelectsPrimaryOnly(t *testing.T) {
	store := multiImageStore()
	got := resolve(t, store, Selector{SetID: "S", Paths: []string{"fam-a.txt"}})

	work := got.Work[0].Inv
	if len(work.Files) != 1 || work.Files[0].RelPath != "fam-a.txt" {
		t.Fatalf("work files = %v, want the primary only", rels(work.Files))
	}
	if len(work.Secondaries) != 0 {
		t.Errorf("work secondaries = %v, want NONE: an unselected sibling must never be created", rels2(work.Secondaries))
	}
}

// --- chunked records (resolve to EVERY image holding their chunks) -----------

func TestResolveSelectionChunkedSpansEveryHoldingImage(t *testing.T) {
	store := multiImageStore()
	got := resolve(t, store, Selector{SetID: "S", Paths: []string{"big.bin"}})

	if len(got.Work) != 2 || got.Work[0].Image != 1 || got.Work[1].Image != 2 {
		t.Fatalf("required images = %+v, want [1 2]: a chunked record needs every chunk image", got.Work)
	}
	if len(got.Work[0].Inv.Chunks) != 1 || got.Work[0].Inv.Chunks[0].ChunkIndex != 0 {
		t.Errorf("image 1 chunks = %+v, want chunk 0", got.Work[0].Inv.Chunks)
	}
	if len(got.Work[1].Inv.Chunks) != 1 || got.Work[1].Inv.Chunks[0].ChunkIndex != 1 {
		t.Errorf("image 2 chunks = %+v, want chunk 1", got.Work[1].Inv.Chunks)
	}
	for _, w := range got.Work {
		for _, c := range w.Inv.Chunks {
			if c.ParentHash == nil || c.Hash == nil {
				t.Errorf("image %d chunk %d lacks an anchor hash (8.4 double verification needs both)", w.Image, c.ChunkIndex)
			}
			if c.ParentRelPath != "big.bin" {
				t.Errorf("chunk parent = %q, want big.bin", c.ParentRelPath)
			}
		}
	}
	// The whole-file byte total is the record's size, not the sum of chunk
	// lengths counted twice.
	if got.Bytes != 1000 {
		t.Errorf("bytes = %d, want 1000 (the record's size)", got.Bytes)
	}
}

// --- unclaimed paths (records no image holds) --------------------------------

func TestResolveSelectionCollectsUnclaimedPaths(t *testing.T) {
	store := multiImageStore()
	// A file scanned AFTER the plan: active, but no image claims it.
	store.records = append(store.records, Record{
		FileID: 9, RelPath: "new-after-plan.txt", EntryKind: "regular", Size: 7, Hash: []byte{7},
	})
	got := resolve(t, store, Selector{SetID: "S"})

	if len(got.Unclaimed) != 1 || got.Unclaimed[0] != "new-after-plan.txt" {
		t.Errorf("unclaimed = %v, want [new-after-plan.txt] (honest per-item failure, never a silent drop)", got.Unclaimed)
	}
	// The claimed paths still resolve normally: the run does not fail
	// wholesale, it reports the gap and restores the rest.
	if len(got.Work) != 2 {
		t.Errorf("required images = %v, want both (the unclaimed path adds no image)", got.Work)
	}
	// The unclaimed path is still part of the selection (it is reported in
	// the report and counted against the run), but no work list carries it.
	found := false
	for _, p := range got.Paths {
		if p == "new-after-plan.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("paths = %v, want the unclaimed path present (it is a reportable failure)", got.Paths)
	}
}

// --- the seam completeness pins (files/symlinks/chunks/dirs/secondaries) -----

// allKindsStore is a ONE-image fixture whose inventory carries EVERY member
// kind: two whole files, a hard-link primary, its secondary, a symlink, an
// empty dir, and a chunked parent. It exists so the two seam-completeness
// tests below can speak about all five kinds at once.
func allKindsStore() *fakeStore {
	return &fakeStore{
		records: []Record{
			{FileID: 1, RelPath: "keep.txt", EntryKind: "regular", Size: 10, Hash: []byte{1}},
			{FileID: 2, RelPath: "other.txt", EntryKind: "regular", Size: 20, Hash: []byte{2}},
			{FileID: 3, RelPath: "fam-p.txt", EntryKind: "regular", Size: 30, Hash: []byte{3}},
			{FileID: 4, RelPath: "fam-s.txt", EntryKind: "regular", Size: 30, Hash: []byte{3}},
			{FileID: 5, RelPath: "link", EntryKind: "symlink", Size: 4, LinkTarget: "keep.txt"},
			{FileID: 6, RelPath: "big.bin", EntryKind: "regular", Size: 100, Hash: []byte{6}},
		},
		dirs: []DirRecord{{DirID: 1, RelPath: "empty-dir"}},
		placement: []ImagePaths{{ImageID: 1, Paths: []string{
			"big.bin", "empty-dir", "fam-p.txt", "fam-s.txt", "keep.txt", "link", "other.txt",
		}}},
		inventory: map[int64]Inventory{
			1: {
				Files: []FileItem{
					{FileID: 1, RelPath: "keep.txt", Size: 10, Hash: []byte{1}},
					{FileID: 2, RelPath: "other.txt", Size: 20, Hash: []byte{2}},
					{FileID: 3, RelPath: "fam-p.txt", Size: 30, Hash: []byte{3}, Nlink: 2},
				},
				Symlinks: []SymlinkItem{{FileID: 5, RelPath: "link", LinkTarget: "keep.txt", Size: 4}},
				Dirs:     []DirItem{{DirID: 1, RelPath: "empty-dir"}},
				Secondaries: []LinkItem{{
					FileID: 4, RelPath: "fam-s.txt", Size: 30, Hash: []byte{3},
					PrimaryFileID: 3, PrimaryRelPath: "fam-p.txt",
				}},
				Chunks: []ChunkItem{{
					ChunkID: 1, FileID: 6, ChunkIndex: 0, Offset: 0, Length: 100,
					Hash: []byte{9}, ParentRelPath: "big.bin", ParentSize: 100,
					ParentHash: []byte{6}, ParentNumChunks: 1,
				}},
			},
		},
	}
}

// THE SEAM FILTER PIN: selecting ONE file must drop every UNSELECTED member
// from EVERY inventory kind - the unselected whole files, the sibling symlink,
// the unselected chunked parent (by its parent relpath), the empty dir, and
// the hard-link SECONDARY. ResolvedSelection.Paths is the ONLY set of names
// T5.4 may create, so a leak in ANY kind's filter would restore a name the
// user never selected.
func TestResolveSelectionFilterDropsUnselectedEverywhere(t *testing.T) {
	store := allKindsStore()
	got := resolve(t, store, Selector{SetID: "S", Paths: []string{"keep.txt"}})

	work := got.Work[0].Inv
	if len(work.Files) != 1 || work.Files[0].RelPath != "keep.txt" {
		t.Fatalf("work files = %v, want ONLY keep.txt", rels(work.Files))
	}
	if len(work.Symlinks) != 0 {
		t.Errorf("work symlinks = %v, want NONE (the unselected sibling symlink must not be created)", work.Symlinks)
	}
	if len(work.Chunks) != 0 {
		t.Errorf("work chunks = %v, want NONE (the unselected chunked parent must not be restored)", work.Chunks)
	}
	if len(work.Dirs) != 0 {
		t.Errorf("work dirs = %v, want NONE (the unselected empty dir must not be created)", work.Dirs)
	}
	if len(work.Secondaries) != 0 {
		t.Errorf("work secondaries = %v, want NONE (the unselected family member must not be linked)", rels2(work.Secondaries))
	}
}

// THE WORK-COMPLETENESS PIN (the inverse direction): the per-image Work must
// cover EVERY member kind the content list would hold - a kind missing from
// Work is a member kind that is never restored. And every name the Work
// carries is a SELECTED name: Paths is the ONLY set T5.4 may create from, so
// the inventory can never widen it.
func TestResolveSelectionWorkCoversEveryMemberKind(t *testing.T) {
	store := allKindsStore()
	got := resolve(t, store, Selector{SetID: "S"})

	if len(got.Work) != 1 {
		t.Fatalf("required images = %+v, want the one image", got.Work)
	}
	work := got.Work[0].Inv
	if len(work.Files) == 0 || len(work.Symlinks) == 0 || len(work.Chunks) == 0 ||
		len(work.Dirs) == 0 || len(work.Secondaries) == 0 {
		t.Fatalf("work inventory = %+v, want EVERY member kind present (files/symlinks/chunks/dirs/secondaries)", work)
	}
	// Every inventory name is a selected name - in both directions: member
	// relpaths and chunk PARENT relpaths must all be in Paths.
	selected := make(map[string]struct{}, len(got.Paths))
	for _, p := range got.Paths {
		selected[p] = struct{}{}
	}
	for _, f := range work.Files {
		if _, ok := selected[f.RelPath]; !ok {
			t.Errorf("file %q is in the work list but not in Paths (Paths must be the ONLY creatable names)", f.RelPath)
		}
	}
	for _, s := range work.Symlinks {
		if _, ok := selected[s.RelPath]; !ok {
			t.Errorf("symlink %q is in the work list but not in Paths", s.RelPath)
		}
	}
	for _, d := range work.Dirs {
		if _, ok := selected[d.RelPath]; !ok {
			t.Errorf("dir %q is in the work list but not in Paths", d.RelPath)
		}
	}
	for _, l := range work.Secondaries {
		if _, ok := selected[l.RelPath]; !ok {
			t.Errorf("secondary %q is in the work list but not in Paths", l.RelPath)
		}
	}
	for _, c := range work.Chunks {
		if _, ok := selected[c.ParentRelPath]; !ok {
			t.Errorf("chunk parent %q is in the work list but not in Paths", c.ParentRelPath)
		}
	}
	// NeededFor (the 8.3 prompt's <path>) is the alphabetically first
	// claimed selected path - deterministic across runs.
	if got.Work[0].NeededFor != "big.bin" {
		t.Errorf("NeededFor = %q, want the alphabetically first claimed path big.bin", got.Work[0].NeededFor)
	}
}

// --- the component-aware rule's degenerate case on a FILE --------------------

// --subtree naming a FILE selects exactly that file (the component-aware
// prefix degrades to an exact match), and a bare byte-prefix of the file's
// name matches nothing - it is an input error, never a silent prefix pull.
func TestResolveSelectionSubtreeOnFileIsExactMatch(t *testing.T) {
	store := multiImageStore()
	got := resolve(t, store, Selector{SetID: "S", Subtree: "small.txt"})
	if strings.Join(got.Paths, ",") != "small.txt" {
		t.Errorf("--subtree small.txt selected %v, want exactly the file", got.Paths)
	}

	_, err := ResolveSelection(context.Background(), store, Selector{SetID: "S", Subtree: "small"})
	var input *InputError
	if !errors.As(err, &input) {
		t.Fatalf("--subtree small (a byte-prefix of a FILE): err = %v, want *InputError", err)
	}
}

// --- helpers -----------------------------------------------------------------

func rels(files []FileItem) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.RelPath)
	}
	return out
}

func rels2(links []LinkItem) []string {
	out := make([]string, 0, len(links))
	for _, l := range links {
		out = append(out, l.RelPath)
	}
	return out
}
