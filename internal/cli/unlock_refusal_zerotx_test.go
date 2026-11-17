package cli

// U.2 adversarial follow-up (DESIGN_SPEC 4.3 PARTIALLY-ASSIGNED CHUNK
// SETS, 4.6 partial-chunk-set remedies): the zero-transaction refusal
// matrix and fixture-legality pins that unlock_replan_test.go's AC
// block leaves open.
//
//   - ROWS-DON'T-VERIFY: the third refusal reason, driven through the
//     REAL commands - plan AND report both exit 2, stdout empty, the
//     message names the file / locked image ids / BOTH remedies, and
//     the catalog dump (files, chunks, directories, images,
//     image_contents, runs) is byte-identical across the refusal, so
//     neither an assignment row nor a runs row appeared.
//   - SECONDARY-PARTIAL: the fourth refusal reason at the CLI tier,
//     including the C.2 remedy ABSENCE assertion - this branch offers
//     clear-assignments (whole set) ONLY, and the message must NOT
//     carry "keep the current media profile".
//   - FIXTURE LEGALITY: the AC-22 synthetic bd-r-sl / 20 GiB catalog
//     must be a LEGAL plan (every image's total charge within its
//     usable budget) - claimed in the fixture's doc comment, asserted
//     here mechanically, before AND after the re-plan.

import (
	"context"
	"strings"
	"testing"

	"isopress/internal/catalog"
	"isopress/internal/report"
	"isopress/internal/units"
)

// imageCharge renders one images row's total charge under the 12.5
// buildability invariant: payload (whole-file items + chunk items the
// image holds) plus the canonical estimator's fixed overhead and
// per-entry charge. Tests-only SQL, same policy as production 6.3.
type imageCharge struct {
	imageID int64
	usable  int64
	payload int64
	entries int64
}

// readImageCharges loads every destination's charge of the set.
func readImageCharges(t *testing.T, cat, setID string) []imageCharge {
	t.Helper()
	db := catOpen(t, cat)
	rows, err := db.Query(`
		SELECT i.image_id, i.usable_size,
		       COALESCE((SELECT SUM(size) FROM files f
		                 WHERE f.set_id = i.set_id AND f.image_id = i.image_id), 0)
		     + COALESCE((SELECT SUM(length) FROM chunks c
		                 WHERE c.set_id = i.set_id AND c.image_id = i.image_id), 0),
		       COALESCE((SELECT COUNT(*) FROM files f
		                 WHERE f.set_id = i.set_id AND f.image_id = i.image_id), 0)
		     + COALESCE((SELECT COUNT(*) FROM chunks c
		                 WHERE c.set_id = i.set_id AND c.image_id = i.image_id), 0)
		     + COALESCE((SELECT COUNT(*) FROM directories d
		                 WHERE d.set_id = i.set_id AND d.image_id = i.image_id), 0)
		FROM images i WHERE i.set_id = ? ORDER BY i.image_id`, setID)
	if err != nil {
		t.Fatalf("query image charges: %v", err)
	}
	defer rows.Close()
	var out []imageCharge
	for rows.Next() {
		var ic imageCharge
		if err := rows.Scan(&ic.imageID, &ic.usable, &ic.payload, &ic.entries); err != nil {
			t.Fatalf("scan image charge: %v", err)
		}
		out = append(out, ic)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate image charges: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("set %q has no images rows", setID)
	}
	return out
}

// assertImagesWithinBudget pins the 12.5 buildability invariant
// REQUIRED_IMAGE_BYTES(payload, entries) <= usable_size for EVERY
// destination of the set - the legality of a hand-built catalog state.
func assertImagesWithinBudget(t *testing.T, cat, setID string, wantImages int) {
	t.Helper()
	charges := readImageCharges(t, cat, setID)
	if len(charges) != wantImages {
		t.Fatalf("destinations = %d, want %d (%+v)", len(charges), wantImages, charges)
	}
	for _, ic := range charges {
		required := units.RequiredImageBytes(ic.payload, ic.entries)
		if required > ic.usable {
			t.Errorf("image %d is OVERFILLED: RequiredImageBytes(payload %d, entries %d) = %d > usable %d "+
				"- the fixture is not a legal plan state",
				ic.imageID, ic.payload, ic.entries, required, ic.usable)
		}
	}
}

// --- refusal reason 3: rows do not verify ---------------------------------

// TestReplanPartialSetRowsDoNotVerifyExits2ZeroTx drives the third
// refusal reason through the real commands: after a scoped unlock
// leaves a PARTIAL set, a hand-edited row offset breaks the
// decomposition verify (while files.chunk_size still equals the run's
// effective size, so only this gate can refuse). plan AND report must
// exit 2 with the file, the locked image ids and BOTH remedies on
// stderr, empty stdout, and a byte-identical catalog - no assignment
// row, no runs row, no image row.
func TestReplanPartialSetRowsDoNotVerifyExits2ZeroTx(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	const setID = "S-NOMATCH"
	scanPlanFixture(t, root, cat, setID)
	if stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", setID, "--size", planFixtureSize); code != report.ExitOK {
		t.Fatalf("fresh plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// The scoped unlock: big.dat's chunk0 (image 1) is released,
	// chunk1 stays locked on image 2 -> 0 < assigned < total.
	if stdout, stderr, code := runClearChild(t,
		"--catalog", cat, "--set-id", setID, "--image", "1"); code != report.ExitOK {
		t.Fatalf("clear --image 1 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// Corrupt ONLY the locked row's geometry (tests-only SQL): the
	// stamp still matches the effective size, so the rows verify gate
	// is the one and only branch that can refuse.
	db := catRW(t, cat)
	var fileID int64
	if err := db.QueryRow(`SELECT file_id FROM files WHERE set_id = ? AND relpath = ?`,
		setID, "big.dat").Scan(&fileID); err != nil {
		t.Fatalf("locating big.dat: %v", err)
	}
	if _, err := db.Exec(`UPDATE chunks SET offset = offset + 1
		WHERE set_id = ? AND file_id = ? AND chunk_index = 1`, setID, fileID); err != nil {
		t.Fatalf("corrupting the locked row: %v", err)
	}
	before := dumpUnlockStateCLI(t, catOpen(t, cat))

	// plan: usage exit 2, empty stdout, the full message contract.
	stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", setID, "--size", planFixtureSize)
	if code != report.ExitUsage {
		t.Fatalf("plan over a non-verifying partial set exit = %d, want 2 (usage)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if stdout != "" {
		t.Errorf("refused plan printed to stdout: %q", stdout)
	}
	for _, want := range []string{
		"big.dat",
		"locked on image(s) 2",
		"do not verify against the current decomposition",
		"keep the current media profile",
		"clear-assignments (whole set)",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("plan stderr lacks %q:\n%s", want, stderr)
		}
	}
	if after := dumpUnlockStateCLI(t, catOpen(t, cat)); after != before {
		t.Errorf("the refused plan changed the catalog:\n--- before ---\n%s\n--- after ---\n%s",
			before, after)
	}

	// report: the SAME shared seam, the same exit class.
	stdout, stderr, code = runReportChild(t,
		"--catalog", cat, "--set-id", setID, "--size", planFixtureSize)
	if code != report.ExitUsage {
		t.Fatalf("report over a non-verifying partial set exit = %d, want 2\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if stdout != "" {
		t.Errorf("refused report printed to stdout: %q", stdout)
	}
	for _, want := range []string{"big.dat", "do not verify against the current decomposition",
		"keep the current media profile", "clear-assignments (whole set)"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("report stderr lacks %q:\n%s", want, stderr)
		}
	}
	if after := dumpUnlockStateCLI(t, catOpen(t, cat)); after != before {
		t.Errorf("the refused report changed the catalog:\n--- before ---\n%s\n--- after ---\n%s",
			before, after)
	}
}

// --- refusal reason 4: secondary excluded (C.2: clear-only remedy) --------------

// TestReplanSecondaryPartialSetExits2ClearOnlyRemedy builds the
// hard-link shape the balancer's own fixtures never produce (a
// PARTIAL chunk set on a family SECONDARY) through the exported
// catalog API and drives the real commands: plan AND report must exit
// 2 with the file, the locked image id and the SECOND remedy - and the
// message must NOT offer the profile remedy (design C.2: no profile
// choice can make the family rule place a secondary row). Catalog
// byte-identical across both refusals, runs row included.
func TestReplanSecondaryPartialSetExits2ClearOnlyRemedy(t *testing.T) {
	root := makeUnlockTree(t)
	cat := createCatalog(t)
	const setID = "S-SECFAM"
	scanPlanFixture(t, root, cat, setID)

	// Hand-build the SECONDARY's partial chunk set. The scan assigns
	// file_ids in nondeterministic (parallel-worker) order, so which of
	// the two family names is the secondary varies per run: the
	// secondary is always the member with the HIGHER file_id (the
	// balancer derives the primary as the lowest, 2.4), so query for it
	// rather than assuming a name. Two chunk rows over the 15-byte
	// payload, chunk 0 locked on image 1, chunk 1 unassigned.
	ctx := context.Background()
	var secFileID int64
	var secRelPath string
	if err := catRW(t, cat).QueryRow(
		`SELECT file_id, relpath FROM files WHERE set_id = ? AND relpath IN ('fam-a.txt', 'fam-b.txt')
		 ORDER BY file_id DESC LIMIT 1`, setID).Scan(&secFileID, &secRelPath); err != nil {
		t.Fatalf("locating the secondary: %v", err)
	}
	// Sanity: the pair really IS a captured hard-link family - both rows
	// nlink = 2 with one (dev, inode) - and secFileID carries the HIGHER
	// id. Otherwise the balancer would not treat it as a secondary and
	// this test would silently exercise a different refusal branch.
	type famRow struct {
		id, dev, inode, nlink int64
	}
	var fams []famRow
	{
		rows, err := catRW(t, cat).Query(
			`SELECT file_id, dev, inode, nlink FROM files WHERE set_id = ?
			 AND relpath IN ('fam-a.txt', 'fam-b.txt') ORDER BY file_id`, setID)
		if err != nil {
			t.Fatalf("reading the family: %v", err)
		}
		for rows.Next() {
			var r famRow
			if err := rows.Scan(&r.id, &r.dev, &r.inode, &r.nlink); err != nil {
				rows.Close()
				t.Fatalf("scan family row: %v", err)
			}
			fams = append(fams, r)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatalf("iterate family rows: %v", err)
		}
		rows.Close()
	}
	if len(fams) != 2 || fams[0].nlink != 2 || fams[1].nlink != 2 ||
		fams[0].dev != fams[1].dev || fams[0].inode != fams[1].inode || fams[0].inode == 0 {
		t.Fatalf("family rows = %+v, want exactly two nlink=2 rows sharing one (dev, inode) - "+
			"the scan did not capture the family, so the secondary branch is unreachable", fams)
	}
	if secFileID != fams[1].id {
		t.Fatalf("secondary file_id = %d, want the higher family id %d", secFileID, fams[1].id)
	}
	db, err := catalog.Open(cat, catalog.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ptx, err := db.BeginPlanTx(ctx, 0, setID)
	if err != nil {
		t.Fatal(err)
	}
	if err := ptx.InsertImage(ctx, catalog.ImageSpec{
		ImageID: 1, NominalSize: 1_000_000_000, UsableSize: 980_000_000, MediaProfile: "disk",
	}); err != nil {
		t.Fatal(err)
	}
	// 15-byte family payload split in two rows (8 + 7).
	if err := ptx.EnsureChunkRows(ctx, secFileID, []catalog.ChunkSpec{
		{Index: 0, Offset: 0, Length: 8},
		{Index: 1, Offset: 8, Length: 7},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ptx.StoreChunkSize(ctx, secFileID, 8, 2); err != nil {
		t.Fatal(err)
	}
	if err := ptx.AssignChunk(ctx, secFileID, 0, 1); err != nil {
		t.Fatal(err)
	}
	if err := ptx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Close the seeding handle BEFORE the real commands run: an open
	// write connection would starve them of the catalog lock (exit 5).
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before := dumpUnlockStateCLI(t, catOpen(t, cat))

	// plan: exit 2, empty stdout, clear-only remedy, absence of the
	// profile remedy.
	stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", setID, "--size", planFixtureSize)
	if code != report.ExitUsage {
		t.Fatalf("plan over a secondary partial set exit = %d, want 2 (usage)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if stdout != "" {
		t.Errorf("refused plan printed to stdout: %q", stdout)
	}
	for _, want := range []string{
		secRelPath,
		"locked on image(s) 1",
		"hard-link family rule",
		"clear-assignments (whole set)",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("plan stderr lacks %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "keep the current media profile") {
		t.Errorf("secondary-branch refusal carries the profile remedy too (design C.2: clear-only):\n%s", stderr)
	}
	if after := dumpUnlockStateCLI(t, catOpen(t, cat)); after != before {
		t.Errorf("the refused plan changed the catalog:\n--- before ---\n%s\n--- after ---\n%s",
			before, after)
	}

	// report: same seam, same contract, same remedies.
	stdout, stderr, code = runReportChild(t,
		"--catalog", cat, "--set-id", setID, "--size", planFixtureSize)
	if code != report.ExitUsage {
		t.Fatalf("report over a secondary partial set exit = %d, want 2\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if stdout != "" {
		t.Errorf("refused report printed to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, secRelPath) ||
		!strings.Contains(stderr, "clear-assignments (whole set)") {
		t.Errorf("report stderr lacks the file or the clear-only remedy:\n%s", stderr)
	}
	if strings.Contains(stderr, "keep the current media profile") {
		t.Errorf("secondary-branch report carries the profile remedy too (design C.2):\n%s", stderr)
	}
	if after := dumpUnlockStateCLI(t, catOpen(t, cat)); after != before {
		t.Errorf("the refused report changed the catalog:\n--- before ---\n%s\n--- after ---\n%s",
			before, after)
	}
}
