package catalog

// T3.4 QA FIX ROUND (manifest honesty, the T3.1 "manifest must never
// lie" principle): unit coverage of the CROSS-IMAGE hard-link family
// filter in the copy-side catalog surface - excludeSecondaryWholeFileClaims
// (the UNIFIED secondary-claim rule, generalized from the earlier
// excludeCrossImageSecondaries) in the inventory and its matching
// filter in CopyPlacement.
//
// THE SCENARIO THESE PIN: plan-1 assigned two files to DIFFERENT
// images as distinct whole files; between plan and copy one was
// replaced by a hard link to the other. The catalog rows now derive as
// one family (shared dev+inode, nlink>1, one row per NAME - the
// primary is the LOWEST file_id, the hardLinkRows derivation), but the
// assignment columns still say "different images". The inventory and
// the placement summary must then claim the member ONLY on the
// primary's image - where its bytes are physically written, as the
// link(2) of D-04 copy mode - never on its own assignment image,
// whose tree never holds its bytes.
//
// Everything here seeds the state DIRECTLY through the exported scan
// and plan APIs (the QA brief's documented seeding path): the family
// exists in the scan rows exactly as a between-plan-and-copy link
// replacement leaves it.

import (
	"context"
	"crypto/sha256"
	"path/filepath"
	"testing"
	"time"
)

var honestyHash = sha256.Sum256([]byte("honesty family content"))

// honestyFixture seeds set S-HONEST with the row shape of the finding:
//
//	fam-a      regular, dev 9 / inode 900 / nlink 2 - the PRIMARY
//	           (inserted first = lowest file_id);
//	fam-b      regular, dev 9 / inode 900 / nlink 2 - the member row;
//	keep-1     regular, nlink 1 - plain row;
//	keep-2     regular, nlink 1 - plain row;
//	sentinel   regular, nlink 2 but INODE 0 (the uncaptured
//	           sentinel - never a family member, derivation pin).
//
// images: 1, 2. Assignments are NOT seeded here - each test picks the
// shape it pins, through the exported PlanTx API (the production
// assignment transaction).
type honestyFixture struct {
	db    *DB
	setID string

	famA, famB, keep1, keep2, sentinel int64
}

func newHonestyFixture(t *testing.T) *honestyFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.sqlite")
	mustCreate(t, path)
	db := mustOpen(t, path, OpenReadWrite)
	ctx := context.Background()
	f := &honestyFixture{db: db, setID: "S-HONEST"}
	if err := db.EnsureSet(ctx, f.setID, "/data", time.Now().UnixNano(), ToolVersion); err != nil {
		t.Fatalf("EnsureSet: %v", err)
	}
	runID, err := db.StartScanRun(ctx, ScanRun{
		SetID: f.setID, Kind: "ingest", Root: "/data", StartedAt: time.Now().UnixNano(),
	})
	if err != nil {
		t.Fatalf("StartScanRun: %v", err)
	}
	tx, err := db.BeginScanTx(ctx, runID, f.setID)
	if err != nil {
		t.Fatalf("BeginScanTx: %v", err)
	}
	insert := func(row FileRow) int64 {
		t.Helper()
		id, err := tx.InsertScanFile(ctx, row)
		if err != nil {
			t.Fatalf("InsertScanFile %q: %v", row.RelPath, err)
		}
		return id
	}
	f.famA = insert(FileRow{RelPath: "fam-a", EntryKind: "regular", Size: 24,
		Mode: 0o644, MtimeNS: 4000, Dev: 9, Inode: 900, Nlink: 2, Hash: honestyHash[:], Status: "active"})
	f.famB = insert(FileRow{RelPath: "fam-b", EntryKind: "regular", Size: 24,
		Mode: 0o644, MtimeNS: 4000, Dev: 9, Inode: 900, Nlink: 2, Hash: honestyHash[:], Status: "active"})
	f.keep1 = insert(FileRow{RelPath: "keep-1", EntryKind: "regular", Size: 8,
		Mode: 0o644, MtimeNS: 5000, Dev: 9, Inode: 901, Nlink: 1, Status: "active"})
	f.keep2 = insert(FileRow{RelPath: "keep-2", EntryKind: "regular", Size: 8,
		Mode: 0o644, MtimeNS: 6000, Dev: 9, Inode: 902, Nlink: 1, Status: "active"})
	f.sentinel = insert(FileRow{RelPath: "sentinel", EntryKind: "regular", Size: 8,
		Mode: 0o644, MtimeNS: 7000, Dev: 9, Inode: 0, Nlink: 2, Status: "active"})
	if err := tx.Commit(); err != nil {
		t.Fatalf("ScanTx commit: %v", err)
	}
	return f
}

// plan seeds the two planned images and the given assignments
// (file_id -> image_id) in ONE PlanTx - the production assignment
// transaction, never test-side SQL.
func (f *honestyFixture) plan(t *testing.T, assigns map[int64]int64) {
	t.Helper()
	ctx := context.Background()
	ptx, err := f.db.BeginPlanTx(ctx, 0, f.setID)
	if err != nil {
		t.Fatalf("BeginPlanTx: %v", err)
	}
	for _, id := range []int64{1, 2} {
		if err := ptx.InsertImage(ctx, ImageSpec{
			ImageID: id, NominalSize: 10000, UsableSize: 9800, MediaProfile: "disk",
		}); err != nil {
			t.Fatalf("InsertImage %d: %v", id, err)
		}
	}
	for fileID, imageID := range assigns {
		if err := ptx.AssignFile(ctx, fileID, imageID); err != nil {
			t.Fatalf("AssignFile %d -> %d: %v", fileID, imageID, err)
		}
	}
	if err := ptx.Commit(); err != nil {
		t.Fatalf("PlanTx commit: %v", err)
	}
}

// fileRelpaths renders a Files slice as relpaths for readable pins.
func fileRelpaths(files []CopyFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.RelPath)
	}
	return out
}

func assertRelpaths(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", what, got, want)
		}
	}
}

// --- the inventory filter ----------------------------------------------------

// Primary on image 1, member row assigned to image 2: the member is
// EXCLUDED from image 2's inventory (its bytes are written as link(2)
// on image 1) and claimed as a SECONDARY on image 1. Plain rows and
// the inode=0 sentinel pass through untouched.
func TestCopyInventoryCrossImageSecondaryExcluded(t *testing.T) {
	f := newHonestyFixture(t)
	f.plan(t, map[int64]int64{
		f.famA: 1, f.famB: 2, f.keep1: 1, f.keep2: 2, f.sentinel: 2,
	})
	ctx := context.Background()

	inv1, err := f.db.CopyInventory(ctx, f.setID, 1)
	if err != nil {
		t.Fatalf("CopyInventory image 1: %v", err)
	}
	// Image 1: the primary as a whole file + the member as its
	// secondary (the normal D-04 shape) - and NO second claim of the
	// member anywhere else.
	assertRelpaths(t, "image 1 files", fileRelpaths(inv1.Files), "fam-a", "keep-1")
	if len(inv1.Secondaries) != 1 || inv1.Secondaries[0].FileID != f.famB ||
		inv1.Secondaries[0].PrimaryFileID != f.famA ||
		inv1.Secondaries[0].PrimaryRelPath != "fam-a" {
		t.Fatalf("image 1 secondaries = %+v, want fam-b with primary fam-a", inv1.Secondaries)
	}

	inv2, err := f.db.CopyInventory(ctx, f.setID, 2)
	if err != nil {
		t.Fatalf("CopyInventory image 2: %v", err)
	}
	// Image 2: fam-b is GONE (the honesty filter - its bytes would
	// live only as a link on image 1), while plain rows and the
	// inode=0 sentinel (never a family member) stay.
	assertRelpaths(t, "image 2 files", fileRelpaths(inv2.Files), "keep-2", "sentinel")
	if len(inv2.Secondaries) != 0 {
		t.Fatalf("image 2 secondaries = %+v, want none (the primary sits on image 1)", inv2.Secondaries)
	}

	// The filter only shapes the PROJECTION: fam-b's row is untouched
	// in the catalog (still active, still assigned image 2 - the next
	// copy run of image 2 re-derives the same exclusion, and nothing
	// ever re-writes it).
	var assigned int64
	var active string
	if err := f.db.sql.QueryRowContext(ctx,
		`SELECT image_id, status FROM files WHERE set_id = ? AND file_id = ?`, f.setID, f.famB).
		Scan(&assigned, &active); err != nil {
		t.Fatal(err)
	}
	if assigned != 2 || active != "active" {
		t.Fatalf("fam-b row after the filter = (image %d, %s), want (2, active) untouched", assigned, active)
	}
}

// THE REVERSE SHAPE: the primary is assigned to image 2 and the
// member row to image 1. The claim still follows the BYTES - the
// member is excluded from image 1's inventory and claimed as a
// secondary on image 2 (where link(2) physically lands).
func TestCopyInventoryReverseCrossImageSecondaryExcluded(t *testing.T) {
	f := newHonestyFixture(t)
	f.plan(t, map[int64]int64{
		f.famA: 2, f.famB: 1, f.keep1: 1, f.keep2: 2,
	})
	ctx := context.Background()

	inv1, err := f.db.CopyInventory(ctx, f.setID, 1)
	if err != nil {
		t.Fatalf("CopyInventory image 1: %v", err)
	}
	assertRelpaths(t, "image 1 files", fileRelpaths(inv1.Files), "keep-1")
	if len(inv1.Secondaries) != 0 {
		t.Fatalf("image 1 secondaries = %+v, want none (the primary sits on image 2)", inv1.Secondaries)
	}

	inv2, err := f.db.CopyInventory(ctx, f.setID, 2)
	if err != nil {
		t.Fatalf("CopyInventory image 2: %v", err)
	}
	assertRelpaths(t, "image 2 files", fileRelpaths(inv2.Files), "fam-a", "keep-2")
	if len(inv2.Secondaries) != 1 || inv2.Secondaries[0].FileID != f.famB ||
		inv2.Secondaries[0].PrimaryFileID != f.famA {
		t.Fatalf("image 2 secondaries = %+v, want fam-b with primary fam-a", inv2.Secondaries)
	}
}

// THE SAME-IMAGE BOTH-ASSIGNED FAMILY (both rows assigned to the SAME
// image - seedable through direct-catalog assignment): DELIBERATELY
// UPDATED PIN (the pre-generalization test pinned the BUGGY
// double-claim as "unchanged"). The unified secondary-claim rule
// treats it exactly like every other family with an assigned primary:
// the member's whole-file claim is DROPPED from Files (it was the
// mutually exclusive twin of the link claim - written whole, then
// LinkSecondary hit EEXIST against that very file, and the manifest
// generation failed PERMANENTLY on the duplicate media path), and the
// member is claimed ONCE - as the secondary next to the primary on
// that very image (the normal D-04 topology, all claims on one image).
func TestCopyInventorySameImageBothAssignedSingleClaim(t *testing.T) {
	f := newHonestyFixture(t)
	f.plan(t, map[int64]int64{
		f.famA: 1, f.famB: 1, f.keep2: 2,
	})
	ctx := context.Background()

	inv1, err := f.db.CopyInventory(ctx, f.setID, 1)
	if err != nil {
		t.Fatalf("CopyInventory image 1: %v", err)
	}
	// fam-b is ABSENT from Files (single-claim rule) and present as
	// the secondary of primary fam-a - one claim, the link.
	assertRelpaths(t, "image 1 files", fileRelpaths(inv1.Files), "fam-a")
	if len(inv1.Secondaries) != 1 || inv1.Secondaries[0].FileID != f.famB ||
		inv1.Secondaries[0].PrimaryFileID != f.famA ||
		inv1.Secondaries[0].PrimaryRelPath != "fam-a" {
		t.Fatalf("image 1 secondaries = %+v, want fam-b with primary fam-a", inv1.Secondaries)
	}

	inv2, err := f.db.CopyInventory(ctx, f.setID, 2)
	if err != nil {
		t.Fatalf("CopyInventory image 2: %v", err)
	}
	assertRelpaths(t, "image 2 files", fileRelpaths(inv2.Files), "keep-2")

	// The filter only shapes the PROJECTION: fam-b's row is untouched
	// in the catalog (still active, still assigned image 1).
	var assigned int64
	var active string
	if err := f.db.sql.QueryRowContext(ctx,
		`SELECT image_id, status FROM files WHERE set_id = ? AND file_id = ?`, f.setID, f.famB).
		Scan(&assigned, &active); err != nil {
		t.Fatal(err)
	}
	if assigned != 1 || active != "active" {
		t.Fatalf("fam-b row after the filter = (image %d, %s), want (1, active) untouched", assigned, active)
	}
}

// THE NO-DATA-LOSS PIN: a member row with its own assignment whose
// PRIMARY is UNASSIGNED (image_id NULL) is KEPT on its own image - its
// whole-file write is the only place its bytes would land anywhere,
// and excluding it would lose data, not lies. (The derivation shows a
// family, but no OTHER image claims the bytes, so the assignment
// column is the best available truth for this shape.)
func TestCopyInventoryUnassignedPrimaryKeepsAssignedMember(t *testing.T) {
	f := newHonestyFixture(t)
	// fam-a deliberately NOT assigned; fam-b assigned to image 2.
	f.plan(t, map[int64]int64{
		f.famB: 2, f.keep2: 2,
	})
	ctx := context.Background()

	inv2, err := f.db.CopyInventory(ctx, f.setID, 2)
	if err != nil {
		t.Fatalf("CopyInventory image 2: %v", err)
	}
	assertRelpaths(t, "image 2 files", fileRelpaths(inv2.Files), "fam-b", "keep-2")
	if len(inv2.Secondaries) != 0 {
		t.Fatalf("image 2 secondaries = %+v, want none (the primary is unassigned)", inv2.Secondaries)
	}
}

// --- the placement filter ----------------------------------------------------

// placements renders CopyPlacement as image -> sorted paths for pins.
func placements(t *testing.T, db *DB, setID string) map[int64][]string {
	t.Helper()
	ps, err := db.CopyPlacement(context.Background(), setID)
	if err != nil {
		t.Fatalf("CopyPlacement: %v", err)
	}
	out := make(map[int64][]string, len(ps))
	for _, p := range ps {
		out[p.ImageID] = p.Paths
	}
	return out
}

// The placement summary lists the cross-image member under the
// PRIMARY's image ONLY (the claim follows the bytes), both
// orientations; the same-image and unassigned-primary shapes keep
// their current listings.
func TestCopyPlacementCrossImageSecondaryClaimFollowsBytes(t *testing.T) {
	t.Run("primary on image 1", func(t *testing.T) {
		f := newHonestyFixture(t)
		f.plan(t, map[int64]int64{
			f.famA: 1, f.famB: 2, f.keep1: 1, f.keep2: 2, f.sentinel: 2,
		})
		got := placements(t, f.db, f.setID)
		assertRelpaths(t, "image 1 placement", got[1], "fam-a", "fam-b", "keep-1")
		// fam-b must NOT appear under image 2 (its assignment) - its
		// bytes are the link(2) on image 1. The sentinel stays (never
		// a family member).
		assertRelpaths(t, "image 2 placement", got[2], "keep-2", "sentinel")
	})
	t.Run("primary on image 2", func(t *testing.T) {
		f := newHonestyFixture(t)
		f.plan(t, map[int64]int64{
			f.famA: 2, f.famB: 1, f.keep1: 1, f.keep2: 2,
		})
		got := placements(t, f.db, f.setID)
		assertRelpaths(t, "image 1 placement", got[1], "keep-1")
		assertRelpaths(t, "image 2 placement", got[2], "fam-a", "fam-b", "keep-2")
	})
	t.Run("same image both-assigned family", func(t *testing.T) {
		f := newHonestyFixture(t)
		f.plan(t, map[int64]int64{f.famA: 1, f.famB: 1, f.keep2: 2})
		got := placements(t, f.db, f.setID)
		// The LISTING is unchanged for this shape (both derivations
		// claim the path on image 1, the map dedupes), but the
		// inventory pin above now pins the SINGLE claim - the member's
		// Files row is gone, its path re-lands here via the
		// secondaries loop.
		assertRelpaths(t, "image 1 placement", got[1], "fam-a", "fam-b")
		assertRelpaths(t, "image 2 placement", got[2], "keep-2")
	})
	t.Run("unassigned primary keeps the member's own claim", func(t *testing.T) {
		f := newHonestyFixture(t)
		f.plan(t, map[int64]int64{f.famB: 2, f.keep2: 2})
		got := placements(t, f.db, f.setID)
		assertRelpaths(t, "image 2 placement", got[2], "fam-b", "keep-2")
	})
}

// The whole-set hard-link family read (CopyHardLinkFamilies) is
// DELIBERATELY untouched by the filter: it is set-scoped
// reconstruction metadata (D-04: recorded regardless of write mode so
// a media-alone restore recreates the family from one stored copy),
// not a per-image byte claim - the judgment call pinned here and in
// the cli e2e.
func TestCopyHardLinkFamiliesUnaffectedByHonestyFilter(t *testing.T) {
	f := newHonestyFixture(t)
	f.plan(t, map[int64]int64{f.famA: 1, f.famB: 2, f.keep1: 1, f.keep2: 2})
	fams, err := f.db.CopyHardLinkFamilies(context.Background(), f.setID)
	if err != nil {
		t.Fatalf("CopyHardLinkFamilies: %v", err)
	}
	if len(fams) != 1 || len(fams[0].Names) != 2 ||
		fams[0].Names[0] != "fam-a" || fams[0].Names[1] != "fam-b" ||
		string(fams[0].Hash) != string(honestyHash[:]) {
		t.Fatalf("hard-link families = %+v, want the whole fam family with its shared hash", fams)
	}
}
