package cli

// T3.4 QA FIX ROUND (manifest honesty, the T3.1 "manifest must never
// lie" principle): the CROSS-IMAGE hard-link family and the SAME-IMAGE
// BOTH-ASSIGNED family, end-to-end through the REAL copy command.
//
// THE FINDING THIS PINS: plan-1 assigned fam-a -> image 1 and fam-b ->
// image 2 as DISTINCT whole files; between plan and copy fam-b was
// REPLACED by a hard link to fam-a (one inode, nlink=2 - the catalog
// rows now derive as one hard-link family, one row per NAME). Without
// the fix, image 1 wrote fam-b as link(2) next to fam-a and marked
// fam-b's OWN file row; image 2 then skipped its whole-file write of
// fam-b as already-copied BUT still built a MANIFEST.json entry (and
// a placement claim) for it from the unfiltered inventory - an entry
// whose BYTES LIVE ONLY AS A LINK ON IMAGE 1. A restore from image 2
// alone would fail for that entry: the manifest lying.
//
// THE SECOND FAMILY-CLAIM DEFECT (the QA reject, pinned by the
// "both-assigned" fixture mode below): primary AND member carrying the
// SAME image_id. The cross-image-only filter missed it, so the member
// was claimed TWICE in mutually exclusive forms - written WHOLE via
// Files, then LinkSecondary hit EEXIST against that very file and
// refused to clobber (run 1: exit 10, image incomplete, manifest
// deferred); run 2+ failed manifest generation PERMANENTLY (duplicate
// media path from Files + Secondaries). The image could never
// complete. THE UNIFIED SECONDARY-CLAIM RULE fixes both shapes at
// once: a secondary with an ASSIGNED primary is never an ordinary
// Files claim on ANY image.
//
// THE FIX (option (a), catalog.CopyInventory / excludeSecondaryWholeFileClaims
// - generalized from the earlier excludeCrossImageSecondaries + the
// matching CopyPlacement filter): the hard-link family derivation is
// the ONE truth source; an image claims only members whose bytes are
// written on it. The writer never sees the excluded member, so the
// whole chain - tree, manifest entries, image_contents rows,
// placement, report counts - agrees by construction.
//
// The source-side family here is REAL (fam-b is os.Link'd to fam-a),
// so the copy's step-1 lstat comparison matches the scan rows
// exactly; the ASSIGNMENTS are seeded through the exported catalog
// API (the copyAdvFixture pattern) to reproduce the finding's end
// state. The catalog unit tests (catalog/copystore_gaps_test.go) pin
// the inventory/placement filter at the row level; this file pins the
// whole command chain plus the resume stability the finding asks to
// verify.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"isopress/internal/catalog"
	"isopress/internal/fsmeta"
	"isopress/internal/media"
)

// crossFamPayload is the family's shared content (pattern is
// deterministic, so the test can re-derive it).
var crossFamPayload = pattern(31, 96)

// crossLinkFixture seeds one orientation of the finding through the
// exported catalog API. mode picks the assignment shape:
//
//	"primary-on-1"  - fam-a (the LOWEST file_id = the family primary)
//	                  assigned image 1, fam-b assigned image 2;
//	"primary-on-2"  - the reverse: the primary on image 2, the member
//	                  row on image 1;
//	"same-image"    - the NORMAL D-04 shape: the primary on image 1,
//	                  the member carries NO image_id (what the balancer
//	                  produces for a family present at scan time);
//	"both-assigned" - THE QA REJECT SHAPE: primary AND member BOTH
//	                  assigned to the SAME image 1 (direct-catalog
//	                  seeding - the balancer never produces it). The
//	                  unified secondary-claim rule applies: the member
//	                  is never an ordinary Files claim, its bytes land
//	                  as link(2) next to the primary on image 1, and
//	                  the image completes ONCE.
//
// keep-1.bin / keep-2.bin are plain files giving each image its own
// honest payload.
func crossLinkFixture(t *testing.T, setID, mode string) (catPath, sourceRoot string) {
	t.Helper()
	sourceRoot = t.TempDir()
	catPath = filepath.Join(t.TempDir(), "catalog.sqlite")
	if err := catalog.Create(catPath); err != nil {
		t.Fatal(err)
	}
	db, err := catalog.Open(catPath, catalog.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.EnsureSet(ctx, setID, sourceRoot, time.Now().UnixNano(), catalog.ToolVersion); err != nil {
		t.Fatal(err)
	}
	runID, err := db.StartScanRun(ctx, catalog.ScanRun{
		SetID: setID, Kind: "ingest", Root: sourceRoot, StartedAt: time.Now().UnixNano(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// THE REAL SOURCE FAMILY: same dev+inode, nlink=2 on both names -
	// the copy's step-1 comparison (fsmeta.DiffFields: size, mtime_ns,
	// dev, inode) matches the scan rows exactly.
	famPath := filepath.Join(sourceRoot, "fam-a.bin")
	writeFileP(t, famPath, crossFamPayload, 0o644)
	if err := os.Link(famPath, filepath.Join(sourceRoot, "fam-b.bin")); err != nil {
		t.Fatalf("building the source hard-link family: %v", err)
	}
	writeFileP(t, filepath.Join(sourceRoot, "keep-1.bin"), pattern(32, 64), 0o644)
	writeFileP(t, filepath.Join(sourceRoot, "keep-2.bin"), pattern(33, 64), 0o644)

	famMeta, err := fsmeta.PathMeta(famPath)
	if err != nil {
		t.Fatal(err)
	}
	famSum := sha256.Sum256(crossFamPayload)

	tx, err := db.BeginScanTx(ctx, runID, setID)
	if err != nil {
		t.Fatal(err)
	}
	fileIDs := map[string]int64{}
	insert := func(rel string, meta fsmeta.Meta, nlink int64, hash []byte) {
		t.Helper()
		id, err := tx.InsertScanFile(ctx, catalog.FileRow{
			RelPath: rel, EntryKind: "regular", Size: meta.Size, Mode: 0o644,
			MtimeNS: meta.MtimeNS, Dev: int64(meta.Dev), Inode: int64(meta.Inode),
			Nlink: nlink, Hash: hash, Status: "active",
		})
		if err != nil {
			t.Fatalf("InsertScanFile %q: %v", rel, err)
		}
		fileIDs[rel] = id
	}
	// fam-a FIRST: the lowest file_id wins the primary role (the
	// hardLinkRows derivation both catalog filters share).
	insert("fam-a.bin", famMeta, 2, famSum[:])
	insert("fam-b.bin", famMeta, 2, famSum[:])
	for _, keep := range []string{"keep-1.bin", "keep-2.bin"} {
		meta, err := fsmeta.PathMeta(filepath.Join(sourceRoot, keep))
		if err != nil {
			t.Fatal(err)
		}
		// Whole-file rows always carry their content hash (the 2.3
		// convention; the writer and the manifest refuse hash-less
		// regular rows).
		sum := sha256.Sum256(mustRead(t, filepath.Join(sourceRoot, keep)))
		insert(keep, meta, 1, sum[:])
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	ptx, err := db.BeginPlanTx(ctx, 0, setID)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2} {
		if err := ptx.InsertImage(ctx, catalog.ImageSpec{
			ImageID: id, NominalSize: 100000, UsableSize: 98000, MediaProfile: "disk",
		}); err != nil {
			t.Fatal(err)
		}
	}
	assign := func(rel string, imageID int64) {
		t.Helper()
		if err := ptx.AssignFile(ctx, fileIDs[rel], imageID); err != nil {
			t.Fatalf("AssignFile %s -> image %d: %v", rel, imageID, err)
		}
	}
	switch mode {
	case "primary-on-1":
		assign("fam-a.bin", 1)
		assign("fam-b.bin", 2) // THE FINDING: assigned away from its primary
		assign("keep-1.bin", 1)
		assign("keep-2.bin", 2)
	case "primary-on-2":
		assign("fam-a.bin", 2) // THE REVERSE: the primary itself sits on image 2
		assign("fam-b.bin", 1)
		assign("keep-1.bin", 1)
		assign("keep-2.bin", 2)
	case "same-image":
		assign("fam-a.bin", 1) // fam-b deliberately unassigned (balancer shape)
		assign("keep-1.bin", 1)
		assign("keep-2.bin", 2)
	case "both-assigned":
		assign("fam-a.bin", 1)
		assign("fam-b.bin", 1) // THE REJECT SHAPE: the member's own claim on the primary's image
		assign("keep-1.bin", 1)
		assign("keep-2.bin", 2)
	default:
		t.Fatalf("crossLinkFixture: unknown mode %q", mode)
	}
	if err := ptx.Commit(); err != nil {
		t.Fatal(err)
	}
	return catPath, sourceRoot
}

// xlinkEntryPaths renders a manifest's entry media paths, SORTED (the
// manifest slices are sorted already; sorting again makes the pins
// independent of the comparator).
func xlinkEntryPaths(m media.Manifest) []string {
	out := make([]string, 0, len(m.Entries))
	for _, e := range m.Entries {
		out = append(out, e.MediaPath)
	}
	sort.Strings(out)
	return out
}

// xlinkPlacement renders a manifest's whole-set placement summary as
// image -> paths.
func xlinkPlacement(m media.Manifest) map[int64][]string {
	out := map[int64][]string{}
	for _, p := range m.Placement {
		out[p.ImageNumber] = p.Paths
	}
	return out
}

func assertEqualPaths(t *testing.T, what string, got []string, want ...string) {
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

// runHonestyScenario drives the REAL copy command over a seeded
// orientation and asserts the full honesty chain: tree, manifests
// (entries + hard_link_groups + placement), content rows, copied
// flags, the embedded exports, and a byte-stable resume re-run.
func runHonestyScenario(t *testing.T, setID, mode string, primaryImage, otherImage int64) {
	t.Helper()
	cat, _ := crossLinkFixture(t, setID, mode)
	dest := filepath.Join(t.TempDir(), "dest")

	stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json")
	if code != 0 {
		t.Fatalf("copy exit = %d, want 0 (stderr: %s)\nstdout: %s", code, stderr, stdout)
	}
	var rep copyReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("copy --json output does not parse: %v\n%s", err, stdout)
	}
	// 4 payload items: fam-a whole + fam-b link(2) (both on the
	// primary's image) + the two keeps. The excluded member is NOT an
	// already-copied skip of the other image - it is not that image's
	// business at all (AlreadyCopied stays 0).
	if rep.Status != "ok" || rep.Copied != 4 || rep.AlreadyCopied != 0 ||
		rep.Skipped != 0 || rep.Failed != 0 {
		t.Fatalf("report = %+v, want a clean ok run with 4 copied and 0 already-copied", rep)
	}
	// THE IMAGE-COMPLETENESS PIN (what the copy command records for a
	// complete image): every planned image completed its payload and
	// wrote its MANIFEST.json IN THIS RUN. The both-assigned defect
	// left the family's image incomplete with a deferred manifest on
	// run 1 and manifest generation failing PERMANENTLY on run 2+ (the
	// permanent brick) - this pin must never show that shape again.
	if len(rep.Images) != 2 {
		t.Fatalf("report images = %+v, want one report entry per planned image", rep.Images)
	}
	for _, img := range rep.Images {
		if !img.Complete || !img.Manifest {
			t.Fatalf("image %d report = %+v, want complete=true and manifest_written=true", img.Image, img)
		}
	}

	primFiles := filepath.Join(destImageDir(dest, setID, primaryImage), "files")
	otherFiles := filepath.Join(destImageDir(dest, setID, otherImage), "files")

	// --- THE TREE: the primary's image holds both names as ONE inode -
	famAPath, famBPath := filepath.Join(primFiles, "fam-a.bin"), filepath.Join(primFiles, "fam-b.bin")
	assertBytes(t, famAPath, crossFamPayload)
	infoA, err := os.Lstat(famAPath)
	if err != nil {
		t.Fatal(err)
	}
	infoB, err := os.Lstat(famBPath)
	if err != nil {
		t.Fatalf("the member's path is missing on the primary's image %d: %v", primaryImage, err)
	}
	if !os.SameFile(infoA, infoB) {
		t.Fatal("fam-a and fam-b do not share an inode on the primary's image (D-04 link(2))")
	}
	if got := specialsNlink(t, famAPath); got != 2 {
		t.Fatalf("family nlink on the primary's image = %d, want 2", got)
	}

	// --- THE OTHER IMAGE: never holds either family path -
	for _, name := range []string{"fam-a.bin", "fam-b.bin"} {
		if _, err := os.Lstat(filepath.Join(otherFiles, name)); !os.IsNotExist(err) {
			t.Fatalf("image %d tree holds %s (err=%v): the family's bytes live only on image %d",
				otherImage, name, err, primaryImage)
		}
	}

	// --- MANIFESTS: entries claim only this image's bytes; the
	// hard_link_groups stay WHOLE-SET on every image (D-04: recorded
	// regardless of write mode so a media-alone restore recreates the
	// family from one stored copy - reconstruction metadata, not a
	// per-image byte claim: the documented judgment call).
	keepPrim := "keep-1.bin"
	keepOther := "keep-2.bin"
	if primaryImage == 2 {
		keepPrim, keepOther = keepOther, keepPrim
	}
	mPrim := readManifest(t, filepath.Join(destImageDir(dest, setID, primaryImage), media.ManifestFileName),
		setID, primaryImage)
	mOther := readManifest(t, filepath.Join(destImageDir(dest, setID, otherImage), media.ManifestFileName),
		setID, otherImage)
	assertEqualPaths(t, "primary image manifest entries", xlinkEntryPaths(mPrim),
		"files/fam-a.bin", "files/fam-b.bin", "files/"+keepPrim)
	assertEqualPaths(t, "other image manifest entries", xlinkEntryPaths(mOther),
		"files/"+keepOther)
	for _, e := range mOther.Entries {
		if strings.Contains(e.MediaPath, "fam") {
			t.Fatalf("image %d manifest claims %q whose bytes live only as a link on image %d (the manifest lie)",
				otherImage, e.MediaPath, primaryImage)
		}
	}
	for _, m := range []media.Manifest{mPrim, mOther} {
		if len(m.HardLinkGroups) != 1 ||
			strings.Join(m.HardLinkGroups[0].Names, ",") != "fam-a.bin,fam-b.bin" {
			t.Fatalf("manifest %d hard_link_groups = %+v, want the whole-set fam group on every image",
				m.ImageNumber, m.HardLinkGroups)
		}
	}

	// --- PLACEMENT (from BOTH manifests, which carry the whole-set
	// summary): the member is claimed under the PRIMARY's image ONLY -
	// the claim follows the bytes.
	for _, m := range []media.Manifest{mPrim, mOther} {
		place := xlinkPlacement(m)
		assertEqualPaths(t, "placement of the primary's image", place[primaryImage],
			"fam-a.bin", "fam-b.bin", keepPrim)
		assertEqualPaths(t, "placement of the other image", place[otherImage], keepOther)
	}

	// --- CATALOG: content rows and flags - the member's row is marked
	// and its content row lives on the PRIMARY's image (LinkSecondary
	// marks the secondary's OWN file row through the image-bound
	// CopyStore); the other image records nothing for it.
	db := catOpen(t, cat)
	assertContentKinds(t, db, setID, map[int64]map[string]int{
		primaryImage: {"file": 3, "manifest": 1, "export": 1},
		otherImage:   {"file": 1, "manifest": 1, "export": 1},
	})
	assertAllCopied(t, db, setID)

	// --- THE EXPORTS: one run's bytes, byte-identical across images,
	// open read-only with integrity ok (T3.2/FR-035).
	expPrim := mustRead(t, filepath.Join(destImageDir(dest, setID, primaryImage), media.ExportFileName))
	expOther := mustRead(t, filepath.Join(destImageDir(dest, setID, otherImage), media.ExportFileName))
	if !bytes.Equal(expPrim, expOther) {
		t.Fatal("the embedded exports differ across images; FR-035 embeds ONE snapshot everywhere")
	}
	expPath := filepath.Join(t.TempDir(), "exported.sqlite")
	if err := os.WriteFile(expPath, expPrim, 0o644); err != nil {
		t.Fatal(err)
	}
	expDB := catOpen(t, expPath)
	var verdict string
	if err := expDB.QueryRow(`PRAGMA integrity_check`).Scan(&verdict); err != nil || verdict != "ok" {
		t.Fatalf("export integrity_check = %q (%v), want ok", verdict, err)
	}
	if err := expDB.Close(); err != nil {
		t.Fatal(err)
	}

	// --- THE RESUME RE-RUN: stable on BOTH images - nothing re-written,
	// nothing added, the exclusion re-derived identically.
	beforeMtime := fileMtime(t, famAPath)
	stdout, stderr, code = runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json")
	if code != 0 {
		t.Fatalf("re-run exit = %d, want 0 (stderr: %s)\nstdout: %s", code, stderr, stdout)
	}
	var rep2 copyReport
	if err := json.Unmarshal([]byte(stdout), &rep2); err != nil {
		t.Fatal(err)
	}
	if rep2.Status != "ok" || rep2.Copied != 0 || rep2.AlreadyCopied != 4 ||
		rep2.Skipped != 0 || rep2.Failed != 0 {
		t.Fatalf("re-run report = %+v, want 0 copied + 4 already-copied, nothing skipped or failed", rep2)
	}
	// The re-run's images are complete too - the manifest REGENERATED
	// (the both-assigned defect's permanent "duplicate media path"
	// failure would surface here as a failed manifest write).
	for _, img := range rep2.Images {
		if !img.Complete || !img.Manifest {
			t.Fatalf("re-run image %d report = %+v, want complete=true and manifest_written=true", img.Image, img)
		}
	}
	if fileMtime(t, famAPath) != beforeMtime {
		t.Fatal("the re-run touched an already-copied family member (resume must skip it entirely)")
	}
	if got := specialsNlink(t, famAPath); got != 2 {
		t.Fatalf("family nlink after the re-run = %d, want 2 (nothing re-linked)", got)
	}
	entries, err := os.ReadDir(otherFiles)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("image %d files/ after the re-run = %v, want exactly the one keep file", otherImage, names)
	}
	assertContentKinds(t, catOpen(t, cat), setID, map[int64]map[string]int{
		primaryImage: {"file": 3, "manifest": 1, "export": 1},
		otherImage:   {"file": 1, "manifest": 1, "export": 1},
	})
}

// THE QA SCENARIO, both orientations: the family's claim follows the
// BYTES - whichever image holds the primary writes it whole and
// link(2)s the member next to it; the member's OWN assignment image
// claims (and holds) nothing.
func TestCopyCrossImageFamilyClaimFollowsBytes(t *testing.T) {
	t.Run("primary on image 1", func(t *testing.T) {
		runHonestyScenario(t, "S-XI1", "primary-on-1", 1, 2)
	})
	t.Run("primary on image 2", func(t *testing.T) {
		runHonestyScenario(t, "S-XI2", "primary-on-2", 2, 1)
	})
}

// THE NORMAL D-04 PATH (the same-IMAGE normal family: primary +
// secondary on one image, the member carries NO image_id): EXACTLY as
// before the fix - both names on the primary's image, both manifested
// there, the member placed under that image, nothing claimed anywhere
// else. The unified secondary-claim rule never touches this shape: the
// member was never an ordinary Files claim to begin with (the balancer
// excludes secondaries from items, 2.4), so there is nothing to drop.
func TestCopySameImageFamilyUnchanged(t *testing.T) {
	runHonestyScenario(t, "S-XSIM", "same-image", 1, 2)
}

// THE BOTH-ASSIGNED E2E (the decisive regression pin of the T3.4
// reject / second family-claim defect): primary AND member both carry
// image_id = image 1 (direct-catalog seeding). THE BUG this pins away:
// the member was claimed twice in mutually exclusive forms - written
// WHOLE via Files, then LinkSecondary hit EEXIST against that very
// file and refused to clobber (run 1: exit 10, image incomplete,
// manifest deferred, topology lost); run 2+ manifest generation failed
// PERMANENTLY ("duplicate media path" - the member added from both
// Files and Secondaries), so the image could NEVER complete.
//
// THE FIXED SHAPE (the unified secondary-claim rule): run 1 writes the
// member as link(2) NEXT TO the primary on image 1 (both paths, one
// inode, SameFile, nlink=2), the manifest lists each path EXACTLY ONCE
// (the exact-list compare below pins the absence of the duplicate
// media path), hard_link_groups carries the family, the image
// COMPLETES (Complete + manifest_written per image), exit 0, and no
// EEXIST failure appears anywhere. Run 2 is stable: AlreadyCopied, the
// family mtime untouched, nothing re-linked, the manifest regenerates.
func TestCopyBothAssignedSameImageFamilyCompletes(t *testing.T) {
	runHonestyScenario(t, "S-BAF", "both-assigned", 1, 2)
}
