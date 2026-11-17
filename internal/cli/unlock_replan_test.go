package cli

// U.2 (DESIGN_SPEC 4.6 "Image identity" + 4.3 PARTIALLY-ASSIGNED
// CHUNK SETS): the balancer-tier CLI tests behind the real commands -
// AC-04 (the released slot is re-minted; plan and report agree),
// AC-05 (no-new-disk within the freed capacity), AC-06 (spillover
// opens exactly one id above the pre-unlock max), AC-11 (same-profile
// re-plan retains every chunk hash with zero re-hash cost), AC-22 (the
// {2,1,2} interleaved parent - PLANNING half only; the copy/rewrite
// leg belongs to U.3's wipe-gate tier), AC-23 (partial set + changed
// --size -> exit 2 with the remedies and a bit-identical catalog),
// AC-24 (the locked half is byte-unchanged across a reuse re-plan)
// and AC-36 (gapped-catalog determinism: same inputs -> same ids,
// the reopened slot fills first, report and plan agree - AC-30 in the
// round-2 numbering the PM delegation glossed; the authoritative
// round-3 list numbers it AC-36, and AC-30 is the copy-wipe uniform
// gate payoff owned by U.3).
//
// Every test drives the REAL production entry path through the
// re-exec child (scan_test.go pattern); the catalog side is asserted
// with the SQLite driver directly (tests-only SQL - production SQL
// stays inside internal/catalog).

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"isopress/internal/catalog"
	"isopress/internal/report"
)

// --- helpers -----------------------------------------------------------------

// destImageIDs extracts a plan/report --json document's destination
// ids, ascending.
func destImageIDs(t *testing.T, doc map[string]any) []int64 {
	t.Helper()
	raw, ok := doc["destinations"].([]any)
	if !ok {
		t.Fatalf("destinations = %v, want an array", doc["destinations"])
	}
	ids := make([]int64, 0, len(raw))
	for _, d := range raw {
		m, ok := d.(map[string]any)
		if !ok {
			t.Fatalf("destination entry = %v, want an object", d)
		}
		ids = append(ids, int64(m["image"].(float64)))
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// destLines renders each destination's identity + fill as one string,
// ascending by id: the plan/report agreement comparison (a projection
// and the write it predicts must agree on ids AND fills).
func destLines(t *testing.T, doc map[string]any) []string {
	t.Helper()
	raw, ok := doc["destinations"].([]any)
	if !ok {
		t.Fatalf("destinations = %v, want an array", doc["destinations"])
	}
	out := make([]string, 0, len(raw))
	for _, d := range raw {
		m := d.(map[string]any)
		out = append(out, fmt.Sprintf("image %d: used=%d items=%d dirs=%d locked=%v",
			int64(m["image"].(float64)), int64(m["used_bytes"].(float64)),
			int64(m["items"].(float64)), int64(m["dirs"].(float64)), m["locked"].(bool)))
	}
	sort.Strings(out)
	return out
}

// num64 reads a numeric field of a --json document as int64 (the
// json_contract jsonNum helper returns int; the ids and byte counts
// asserted here are int64).
func num64(t *testing.T, doc map[string]any, key string) int64 {
	t.Helper()
	v, ok := doc[key].(float64)
	if !ok {
		t.Fatalf("%s = %v, want a number", key, doc[key])
	}
	return int64(v)
}

// chunkSnap is one chunks row's placement state - the columns the
// locked half must never lose (AC-24) plus the hash AC-11 retains.
type chunkSnap struct {
	imageID    int64 // 0 = NULL
	copied     int64
	verifiedAt int64 // 0 = NULL
	hash       []byte
}

// chunkSnapshot reads every chunk row of the set keyed by chunk index
// (tests-only SQL). The set has exactly one chunked parent in these
// fixtures, so chunk_index is the unique key.
func chunkSnapshot(t *testing.T, cat, setID string) map[int]chunkSnap {
	t.Helper()
	db := catOpen(t, cat)
	rows, err := db.Query(`SELECT chunk_index, COALESCE(image_id, 0), copied,
		COALESCE(verified_at, 0), COALESCE(hash, X'')
		FROM chunks WHERE set_id = ? ORDER BY chunk_index`, setID)
	if err != nil {
		t.Fatalf("query chunk snapshot: %v", err)
	}
	defer rows.Close()
	out := map[int]chunkSnap{}
	for rows.Next() {
		var (
			idx  int
			cs   chunkSnap
			hash []byte
		)
		if err := rows.Scan(&idx, &cs.imageID, &cs.copied, &cs.verifiedAt, &hash); err != nil {
			t.Fatalf("scan chunk snapshot: %v", err)
		}
		cs.hash = hash
		out[idx] = cs
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate chunk snapshot: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("chunk snapshot of set %q is empty", setID)
	}
	return out
}

// chunkSnapEqual compares two snapshots field by field (the hash is a
// []byte, so the struct itself is not comparable with ==): image_id,
// copied, verified_at and the hash bytes must ALL match.
func chunkSnapEqual(a, b chunkSnap) bool {
	return a.imageID == b.imageID && a.copied == b.copied &&
		a.verifiedAt == b.verifiedAt && bytes.Equal(a.hash, b.hash)
}

// assertPlanReportAgree pins AC-04/AC-36's "plan and report agree":
// the read-only projection computed BEFORE the write must carry the
// same media count, the same new-destination count, and the same
// per-destination ids and fills the write then produces.
func assertPlanReportAgree(t *testing.T, projected, written map[string]any) {
	t.Helper()
	if num64(t, projected, "media_count") != num64(t, written, "media_count") {
		t.Errorf("media_count: report %d vs plan %d - the projection must match the write",
			num64(t, projected, "media_count"), num64(t, written, "media_count"))
	}
	if num64(t, projected, "new_destinations") != num64(t, written, "new_destinations") {
		t.Errorf("new_destinations: report %d vs plan %d",
			num64(t, projected, "new_destinations"), num64(t, written, "new_destinations"))
	}
	want := destLines(t, projected)
	got := destLines(t, written)
	if !sameStrings(want, got) {
		t.Errorf("destination fills differ between report and plan:\nreport: %v\nplan:   %v", want, got)
	}
}

// runPlanJSON / runReportJSON run the real commands with --json and
// return the decoded document (failures fail the test).
func runPlanJSON(t *testing.T, cat, setID string, extra ...string) map[string]any {
	t.Helper()
	args := append([]string{"--catalog", cat, "--set-id", setID, "--size", planFixtureSize, "--json"}, extra...)
	stdout, stderr, code := runPlanChild(t, args...)
	if code != report.ExitOK {
		t.Fatalf("plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	return decodeSingleJSONDoc(t, stdout)
}

func runReportJSON(t *testing.T, cat, setID string, extra ...string) map[string]any {
	t.Helper()
	args := append([]string{"--catalog", cat, "--set-id", setID, "--size", planFixtureSize, "--json"}, extra...)
	stdout, stderr, code := runReportChild(t, args...)
	if code != report.ExitOK {
		t.Fatalf("report exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	return decodeSingleJSONDoc(t, stdout)
}

// --- AC-04: identity (the released slot is re-minted) + plan/report agreement --

// TestReplanReMintsReleasedSlotAndReportAgrees is AC-04: a mid-set
// unlock of image 2 followed by a re-plan mints EXACTLY id 2 - no id
// above the maximum while the slot fits - and the read-only report
// projection agrees with the write (ids, fills, counts). It also
// pins the partial-set reuse half: only the RELEASED chunk is
// re-assigned (assigned_chunks == 1), the sibling chunk keeps image 1.
func TestReplanReMintsReleasedSlotAndReportAgrees(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	const setID = "S-IDENT"
	scanPlanFixture(t, root, cat, setID)

	// Fresh plan: images {1,2} - big.dat's chunk0 -> 1 (exactly
	// packed), chunk1 + the small items + the empty dir -> 2.
	if stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", setID, "--size", planFixtureSize); code != report.ExitOK {
		t.Fatalf("fresh plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	// The scoped unlock of image 2 (U.1): image 2's row and its
	// assignments are released; image 1 (chunk0) stays locked.
	if stdout, stderr, code := runClearChild(t,
		"--catalog", cat, "--set-id", setID, "--image", "2"); code != report.ExitOK {
		t.Fatalf("clear --image 2 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	// The projection BEFORE the re-plan (read-only, same inputs).
	projected := runReportJSON(t, cat, setID)

	// AC-04: the re-plan mints EXACTLY the released id.
	written := runPlanJSON(t, cat, setID)
	ids := destImageIDs(t, written)
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatalf("re-plan destination ids = %v, want exactly {1,2} (the released slot re-minted, no id above the pre-unlock max 2)", ids)
	}
	if num64(t, written, "media_count") != 2 || num64(t, written, "new_destinations") != 1 {
		t.Errorf("media_count %d / new %d, want 2/1 (no new disk: the freed slot absorbs the payload)",
			num64(t, written, "media_count"), num64(t, written, "new_destinations"))
	}

	// Only the RELEASED half is re-placed: big.dat#1 (on image 2)
	// comes back, big.dat#0 keeps image 1 (the locked sibling).
	_, chunks, _ := readAssignments(t, cat, setID)
	if chunks["big.dat#0"] != 1 || chunks["big.dat#1"] != 2 {
		t.Errorf("chunk placement = (%d, %d), want (1, 2): only the released chunk is re-assigned",
			chunks["big.dat#0"], chunks["big.dat#1"])
	}
	if got := num64(t, written, "assigned_chunks"); got != 1 {
		t.Errorf("assigned_chunks = %d, want 1 (the released half only)", got)
	}

	// Plan and report agree (the read-only pipeline is the same
	// pipeline, 4.3).
	assertPlanReportAgree(t, projected, written)
}

// --- AC-05: no-new-disk within the freed capacity ------------------------------

// TestReplanAddedPayloadWithinFreedCapacityAddsNoNewDisk is AC-05:
// added payload that FITS the freed capacity re-uses the released
// slot - media count unchanged, no image id above the pre-unlock
// maximum minted - asserted through plan --json AND the report
// projection agreeing.
func TestReplanAddedPayloadWithinFreedCapacityAddsNoNewDisk(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	const setID = "S-NODISK"
	scanPlanFixture(t, root, cat, setID)
	if stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", setID, "--size", planFixtureSize); code != report.ExitOK {
		t.Fatalf("fresh plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	// Added payload: one small file, well inside image 2's freed
	// capacity (~905 MB free after the unlock).
	if err := os.WriteFile(filepath.Join(root, "extra.txt"), []byte("extra payload\n"), 0o644); err != nil {
		t.Fatalf("add extra.txt: %v", err)
	}
	if stdout, stderr, code := runScanChild(t,
		"--catalog", cat, "--set-id", setID, root); code != report.ExitOK {
		t.Fatalf("rescan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if stdout, stderr, code := runClearChild(t,
		"--catalog", cat, "--set-id", setID, "--image", "2"); code != report.ExitOK {
		t.Fatalf("clear --image 2 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	projected := runReportJSON(t, cat, setID)
	written := runPlanJSON(t, cat, setID)

	ids := destImageIDs(t, written)
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatalf("destination ids = %v, want {1,2}: media count unchanged and no id above the pre-unlock max 2", ids)
	}
	if num64(t, written, "media_count") != 2 {
		t.Errorf("media_count = %d, want 2 (the added payload fits the freed capacity)", num64(t, written, "media_count"))
	}
	fassign, _, _ := readAssignments(t, cat, setID)
	// U.4 (senior S3): the assertion is EXACT, not a range. Image 1 is
	// LOCKED (never released here), so no payload can ever land on it;
	// the added file must take the one released slot, image 2.
	if fassign["extra.txt"] != 2 {
		t.Errorf("extra.txt landed on image %d, want 2 (image 1 is locked; the released slot is image 2)",
			fassign["extra.txt"])
	}
	assertPlanReportAgree(t, projected, written)
}

// --- AC-06: spillover opens exactly one id above the max -----------------------

// TestReplanSpilloverOpensExactlyOneIdAboveMax is AC-06: added
// payload LARGER than the freed capacity refills the released slot
// first (lowest-unused) and then spills to exactly ONE destination
// above the pre-unlock maximum - max+1 on a contiguous catalog.
func TestReplanSpilloverOpensExactlyOneIdAboveMax(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	const setID = "S-SPILL"
	scanPlanFixture(t, root, cat, setID)
	if stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", setID, "--size", planFixtureSize); code != report.ExitOK {
		t.Fatalf("fresh plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	// Added payload: 950,000,000 bytes - a whole-file item (below the
	// 977,901,312 honest-fit line) whose charge (950,000,512) exceeds
	// the ~905,800,555 that will be free on the released image 2, so
	// it must spill. Sparse: instant to create, cheap to scan.
	spill := filepath.Join(root, "spill.dat")
	f, err := os.Create(spill)
	if err != nil {
		t.Fatalf("create spill.dat: %v", err)
	}
	if err := f.Truncate(950_000_000); err != nil {
		t.Fatalf("truncate spill.dat: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close spill.dat: %v", err)
	}
	if stdout, stderr, code := runScanChild(t,
		"--catalog", cat, "--set-id", setID, root); code != report.ExitOK {
		t.Fatalf("rescan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if stdout, stderr, code := runClearChild(t,
		"--catalog", cat, "--set-id", setID, "--image", "2"); code != report.ExitOK {
		t.Fatalf("clear --image 2 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	written := runPlanJSON(t, cat, setID)
	ids := destImageIDs(t, written)
	// The pre-unlock maximum is 2 (the catalog after the unlock holds
	// image 1 only): the reopened slot 2 refills first, and exactly
	// ONE id above the maximum - 3 = max+1 - is minted for the spill.
	if len(ids) != 3 || ids[0] != 1 || ids[1] != 2 || ids[2] != 3 {
		t.Fatalf("destination ids = %v, want {1,2,3}: slot 2 refilled, spillover exactly one id above the pre-unlock max", ids)
	}
	aboveMax := 0
	for _, id := range ids {
		if id > 2 {
			aboveMax++
		}
	}
	if aboveMax != 1 {
		t.Errorf("ids above the pre-unlock max = %d, want exactly 1", aboveMax)
	}
	if num64(t, written, "media_count") != 3 || num64(t, written, "new_destinations") != 2 {
		t.Errorf("media_count %d / new %d, want 3/2 (slot refill + one spillover)",
			num64(t, written, "media_count"), num64(t, written, "new_destinations"))
	}
}

// --- AC-11 + AC-24: hash retention and the byte-unchanged locked half ----------

// TestReplanReuseRetainsHashesAndLeavesLockedHalfUnchanged covers
// AC-11 (same-profile re-plan after an unlock: RehashedAtNextWrite ==
// 0, chunk hash bytes unchanged, the retained-hash count unchanged)
// and AC-24's catalog half (after the reuse write, the LOCKED chunk's
// image_id / copied / verified_at / hash are byte-unchanged, while the
// released chunk comes back re-minted with its own hash intact and no
// stale mark).
func TestReplanReuseRetainsHashesAndLeavesLockedHalfUnchanged(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	const setID = "S-HASH"
	scanPlanFixture(t, root, cat, setID)
	if stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", setID, "--size", planFixtureSize); code != report.ExitOK {
		t.Fatalf("fresh plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// The write cycle's catalog effect: every chunk gets a stored
	// hash, copied=1, verified_at (U.1's seeding helper).
	seedWrittenChunkState(t, cat, setID)

	// Unlock image 1 - the image holding big.dat's chunk0: the set
	// becomes PARTIAL (chunk0 released, chunk1 still locked on 2).
	before := chunkSnapshot(t, cat, setID)
	if stdout, stderr, code := runClearChild(t,
		"--catalog", cat, "--set-id", setID, "--image", "1"); code != report.ExitOK {
		t.Fatalf("clear --image 1 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	unlocked := chunkSnapshot(t, cat, setID)
	if unlocked[0].imageID != 0 || unlocked[0].copied != 0 || unlocked[0].verifiedAt != 0 {
		t.Errorf("released chunk0 after unlock = %+v, want the assignment and marks cleared", unlocked[0])
	}
	if !bytes.Equal(unlocked[0].hash, before[0].hash) || !bytes.Equal(unlocked[1].hash, before[1].hash) {
		t.Error("the unlock changed a chunk hash (FR-010 retention)")
	}
	if !chunkSnapEqual(unlocked[1], before[1]) {
		t.Errorf("the unlock touched the LOCKED chunk1: %+v vs %+v", unlocked[1], before[1])
	}

	// Same-profile re-plan: the reuse path, zero re-hash cost.
	written := runPlanJSON(t, cat, setID)
	if num64(t, written, "rechunked_files") != 0 || num64(t, written, "rehashed_at_next_write") != 0 {
		t.Errorf("re-plan reports rechunked %d / rehashed %d, want 0/0 (AC-11: same profile, zero re-hash cost)",
			num64(t, written, "rechunked_files"), num64(t, written, "rehashed_at_next_write"))
	}

	after := chunkSnapshot(t, cat, setID)
	// AC-11: every hash byte retained, count unchanged.
	retainedBefore, retainedAfter := 0, 0
	for i := range before {
		if len(before[i].hash) != 0 {
			retainedBefore++
		}
		if len(after[i].hash) != 0 {
			retainedAfter++
		}
		if !bytes.Equal(after[i].hash, before[i].hash) {
			t.Errorf("chunk %d hash changed across the unlock + re-plan:\nbefore %x\nafter  %x",
				i, before[i].hash, after[i].hash)
		}
	}
	if retainedBefore != retainedAfter || retainedAfter != len(after) {
		t.Errorf("retained hashes = %d after %d before (of %d rows), want all retained",
			retainedAfter, retainedBefore, len(after))
	}
	// AC-24: the LOCKED half is byte-unchanged across the reuse write.
	if !chunkSnapEqual(after[1], before[1]) {
		t.Errorf("locked half mutated by the reuse re-plan:\nbefore %+v\nafter  %+v", before[1], after[1])
	}
	// The released chunk came back on the re-minted slot, hash intact,
	// with no stale mark carried over (AssignChunk's M8 reset).
	if after[0].imageID != 1 {
		t.Errorf("released chunk0 re-placed on image %d, want the re-minted slot 1", after[0].imageID)
	}
	if after[0].copied != 0 || after[0].verifiedAt != 0 {
		t.Errorf("re-assigned chunk0 marks = (copied %d, verified_at %d), want (0, 0) - the M8 reset",
			after[0].copied, after[0].verifiedAt)
	}
}

// --- AC-23: changed profile over a partial set -> usage, exit 2 ----------------

// addHugeSparseFile appends a 1,960,000,000-byte SPARSE file to the
// fixture tree. Under --size 1 it chunks into 977,901,312 +
// 977,901,312 + 4,197,376; under --size 2 it STILL chunks (its size
// clears the 1,957,901,312 whole-fit line by exactly the reserved
// overhead) but at a DIFFERENT effective size - the shape that
// exercises AC-23's changed-chunk-size refusal branch (the plain
// fixture's 1.05 GB big.dat instead FITS whole at --size 2, which is
// the whole-file-fit branch, pinned separately below).
func addHugeSparseFile(t *testing.T, root string) {
	t.Helper()
	f, err := os.Create(filepath.Join(root, "huge.dat"))
	if err != nil {
		t.Fatalf("create huge.dat: %v", err)
	}
	if err := f.Truncate(1_960_000_000); err != nil {
		t.Fatalf("truncate huge.dat: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close huge.dat: %v", err)
	}
}

// TestReplanPartialSetChangedProfileExits2WithRemedies is AC-23's
// RECHUNK leg: a partial set re-planned under a DIFFERENT --size (the
// effective chunk size changed while the file still does not fit
// whole) refuses with exit 2 - the ratified usage class (10.1
// "unsatisfiable combinations") - naming the file, the locked image
// ids, and BOTH remedies, with a bit-identical catalog (zero
// transactions). `report` refuses identically (its projection is
// undefined too), and the SAME size re-uses the decomposition and
// exits 0.
func TestReplanPartialSetChangedProfileExits2WithRemedies(t *testing.T) {
	root := makePlanFixture(t)
	addHugeSparseFile(t, root)
	cat := createCatalog(t)
	const setID = "S-PARTIAL"
	scanPlanFixture(t, root, cat, setID)
	// Under --size 1 the layout is: image 1 = big.dat#0 (exact fill),
	// image 2 = huge.dat#0, image 3 = huge.dat#1 (exact fill), image
	// 4 = big.dat#1 + huge.dat#2 + the small items + the dir.
	if stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", setID, "--size", planFixtureSize); code != report.ExitOK {
		t.Fatalf("fresh plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// FIXTURE LEGALITY: the 1.96 GB file really CHUNKS at --size 1 -
	// three stored rows (977,901,312 + 977,901,312 + 4,197,376), not a
	// whole-file placement that would make the changed-size branch
	// below unreachable.
	var hugeChunks int
	if err := catOpen(t, cat).QueryRow(
		`SELECT COUNT(*) FROM chunks WHERE set_id = ? AND file_id =
		   (SELECT file_id FROM files WHERE set_id = ? AND relpath = 'huge.dat')`,
		setID, setID).Scan(&hugeChunks); err != nil {
		t.Fatalf("counting huge.dat's chunk rows: %v", err)
	}
	if hugeChunks != 3 {
		t.Fatalf("huge.dat chunk rows at --size 1 = %d, want 3 (the fixture must chunk, not fit whole)", hugeChunks)
	}
	seedWrittenChunkState(t, cat, setID)
	// Unlock image 3 - the image holding ONLY huge.dat's chunk1: the
	// set becomes PARTIAL (chunks 0 and 2 stay locked on images 2,4)
	// while big.dat stays fully locked (the fixture's whole-file
	// branch must not fire first).
	if stdout, stderr, code := runClearChild(t,
		"--catalog", cat, "--set-id", setID, "--image", "3"); code != report.ExitOK {
		t.Fatalf("clear --image 3 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	before := dumpUnlockStateCLI(t, catOpen(t, cat))

	// Changed profile: --size 2 doubles the effective chunk size
	// (977,901,312 -> 1,957,901,312) while huge.dat still does not fit
	// whole -> the stored rows cannot be reused and the locked half
	// would be rewritten -> REFUSE.
	stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", setID, "--size", "2")
	if code != report.ExitUsage {
		t.Fatalf("plan --size 2 over a partial set exit = %d, want 2 (usage, 10.1)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if stdout != "" {
		t.Errorf("refused plan printed to stdout (usage errors print nothing): %q", stdout)
	}
	// The message contract: file, locked image ids, BOTH remedies -
	// plus BOTH chunk sizes, which is what proves the fixture chunks at
	// --size 2 too (the refusal is the changed-size branch, NOT the
	// whole-file-fit one: the stored 977,901,312 decompose decides
	// against this run's effective 1,957,901,312).
	for _, want := range []string{
		"huge.dat",
		"locked on image(s) 2, 4",
		"the effective chunk size changed",
		"stored chunk size 977901312",
		"effective 1957901312",
		"keep the current media profile",
		"clear-assignments (whole set)",
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("plan stderr lacks %q:\n%s", want, stderr)
		}
	}
	// Zero transactions: the catalog is bit-identical.
	if after := dumpUnlockStateCLI(t, catOpen(t, cat)); after != before {
		t.Errorf("the refused plan changed the catalog:\n--- before ---\n%s\n--- after ---\n%s",
			before, after)
	}

	// `report` refuses identically (the projection is undefined under
	// a changed profile too) - same exit class, same remedies.
	stdout, stderr, code = runReportChild(t,
		"--catalog", cat, "--set-id", setID, "--size", "2")
	if code != report.ExitUsage {
		t.Fatalf("report --size 2 over a partial set exit = %d, want 2\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	for _, want := range []string{"huge.dat", "keep the current media profile", "clear-assignments (whole set)"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("report stderr lacks %q:\n%s", want, stderr)
		}
	}

	// Same size: the reuse path completes the set, exit 0, and the
	// released slot 3 is re-minted (no id above the pre-unlock max 4).
	written := runPlanJSON(t, cat, setID)
	ids := destImageIDs(t, written)
	if len(ids) != 4 || ids[0] != 1 || ids[1] != 2 || ids[2] != 3 || ids[3] != 4 {
		t.Errorf("same-size re-plan destination ids = %v, want {1,2,3,4} (slot 3 refilled, no id above 4)", ids)
	}
	if num64(t, written, "assigned_chunks") != 1 {
		t.Errorf("assigned_chunks = %d, want 1 (only the released chunk)", num64(t, written, "assigned_chunks"))
	}
}

// TestReplanPartialSetWholeFileFitExits2WithRemedies pins the OTHER
// refusal clause of the plan help's exit-2 text ("or a whole-file
// fit"): a partial set whose file now FITS whole under the changed
// profile would take the NB2 DropChunkState path - which deletes every
// stored row, locked ones included - so the plan refuses with exit 2,
// the file, the locked image ids, and BOTH remedies, catalog
// bit-identical; the same size then re-uses the decomposition (exit 0).
func TestReplanPartialSetWholeFileFitExits2WithRemedies(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	const setID = "S-WHOLEFIT"
	scanPlanFixture(t, root, cat, setID)
	if stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", setID, "--size", planFixtureSize); code != report.ExitOK {
		t.Fatalf("fresh plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	seedWrittenChunkState(t, cat, setID)
	// Unlock image 1: big.dat's chunk0 is released, chunk1 stays
	// locked on image 2 -> a partial set (the ONLY one).
	if stdout, stderr, code := runClearChild(t,
		"--catalog", cat, "--set-id", setID, "--image", "1"); code != report.ExitOK {
		t.Fatalf("clear --image 1 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	before := dumpUnlockStateCLI(t, catOpen(t, cat))

	// --size 2 makes the 1.05 GB big.dat FIT WHOLE (1,052,099,200 <=
	// 1,960,000,000): the whole-file transition would drop the locked
	// rows -> REFUSE.
	stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", setID, "--size", "2")
	if code != report.ExitUsage {
		t.Fatalf("plan --size 2 over a whole-fit partial set exit = %d, want 2\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if stdout != "" {
		t.Errorf("refused plan printed to stdout (usage errors print nothing): %q", stdout)
	}
	for _, want := range []string{
		"big.dat",
		"locked on image(s) 2",
		"now fits one destination",
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

	// `report` refuses identically at the SAME seam (its projection of
	// the whole-file transition would delete the locked rows too):
	// exit 2, empty stdout, both remedies, bit-identical catalog.
	stdout, stderr, code = runReportChild(t,
		"--catalog", cat, "--set-id", setID, "--size", "2")
	if code != report.ExitUsage {
		t.Fatalf("report --size 2 over a whole-fit partial set exit = %d, want 2\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if stdout != "" {
		t.Errorf("refused report printed to stdout: %q", stdout)
	}
	for _, want := range []string{"big.dat", "now fits one destination",
		"keep the current media profile", "clear-assignments (whole set)"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("report stderr lacks %q:\n%s", want, stderr)
		}
	}
	if after := dumpUnlockStateCLI(t, catOpen(t, cat)); after != before {
		t.Errorf("the refused report changed the catalog:\n--- before ---\n%s\n--- after ---\n%s",
			before, after)
	}

	// Same size: the reuse path completes the set, exit 0.
	written := runPlanJSON(t, cat, setID)
	if num64(t, written, "assigned_chunks") != 1 {
		t.Errorf("assigned_chunks = %d, want 1 (only the released chunk)", num64(t, written, "assigned_chunks"))
	}
	ids := destImageIDs(t, written)
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Errorf("same-size re-plan destination ids = %v, want {1,2}", ids)
	}
}

// --- AC-22: the {2,1,2} interleaved parent (PLANNING half) --------------------

// interleavedParentFixture seeds a scanned + planned set whose ONLY
// payload is one chunked parent ("big.bin") assigned INTERLEAVED
// {2,1,2} across two images - the B1/AC-22 shape the balancer's own
// fixtures never produce on small profiles (a full chunk exactly
// fills one image there). It is built through the EXPORTED catalog
// API (the chunkOrderFixture pattern) on bd-r-sl figures, whose
// effective chunk size is the fixed 20 GiB:
//
//	chunk0 (20 GiB) -> image 2, chunk1 (20 GiB) -> image 1,
//	chunk2 (1 GiB)  -> image 2, total 41 GiB.
//
// Both images stay within their 23 GB usable budget (image 2 holds
// 21 GiB of payload + two entries), so the fixture is a LEGAL catalog
// state, not an overfilled one. No source bytes are needed: plan and
// clear are metadata-only.
func interleavedParentFixture(t *testing.T, setID string) string {
	t.Helper()
	const (
		bdNominal = int64(25_000_000_000) // bd-r-sl (units.ResolveProfile)
		bdUsable  = int64(23_000_000_000)
		chunkSize = int64(20 << 30) // 20 GiB = the bd-r-sl effective size
		fileSize  = int64(41 << 30) // 20 + 20 + 1 GiB -> three chunks
	)
	catPath := filepath.Join(t.TempDir(), "catalog.sqlite")
	if err := catalog.Create(catPath); err != nil {
		t.Fatal(err)
	}
	db, err := catalog.Open(catPath, catalog.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	sourceRoot := t.TempDir()
	if err := db.EnsureSet(ctx, setID, sourceRoot, time.Now().UnixNano(), catalog.ToolVersion); err != nil {
		t.Fatal(err)
	}
	runID, err := db.StartScanRun(ctx, catalog.ScanRun{
		SetID: setID, Kind: "ingest", Root: sourceRoot, StartedAt: time.Now().UnixNano(),
	})
	if err != nil {
		t.Fatal(err)
	}
	stx, err := db.BeginScanTx(ctx, runID, setID)
	if err != nil {
		t.Fatal(err)
	}
	fileID, err := stx.InsertScanFile(ctx, catalog.FileRow{
		RelPath: "big.bin", EntryKind: "regular", Size: fileSize, Mode: 0o644,
		MtimeNS: 1_600_000_000_000_000_000, Dev: 1, Inode: 4242, Nlink: 1, Status: "active",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := stx.Commit(); err != nil {
		t.Fatal(err)
	}

	ptx, err := db.BeginPlanTx(ctx, 0, setID)
	if err != nil {
		t.Fatal(err)
	}
	for _, img := range []int64{1, 2} {
		if err := ptx.InsertImage(ctx, catalog.ImageSpec{
			ImageID: img, NominalSize: bdNominal, UsableSize: bdUsable, MediaProfile: "bd-r-sl",
		}); err != nil {
			t.Fatal(err)
		}
	}
	specs := []catalog.ChunkSpec{
		{Index: 0, Offset: 0, Length: chunkSize},
		{Index: 1, Offset: chunkSize, Length: chunkSize},
		{Index: 2, Offset: 2 * chunkSize, Length: fileSize - 2*chunkSize},
	}
	if err := ptx.EnsureChunkRows(ctx, fileID, specs); err != nil {
		t.Fatal(err)
	}
	if err := ptx.StoreChunkSize(ctx, fileID, chunkSize, len(specs)); err != nil {
		t.Fatal(err)
	}
	for i, img := range []int64{2, 1, 2} { // THE interleaved {2,1,2} placement
		if err := ptx.AssignChunk(ctx, fileID, i, img); err != nil {
			t.Fatalf("AssignChunk chunk %d -> image %d: %v", i, img, err)
		}
	}
	if err := ptx.Commit(); err != nil {
		t.Fatal(err)
	}
	return catPath
}

// TestInterleavedParentUnlockPlansOnlyUnassignedChunks is AC-22's
// PLANNING half (the copy/rewrite leg - the mirror flipping back via
// the 5.2 count-proof - belongs to U.3's wipe-gate tier): unlock
// image 1 of the {2,1,2} parent -> plan exits 0 -> ONLY the released
// chunk is assigned (back onto the re-minted slot 1), the locked
// chunks 0 and 2 keep image 2 with their marks and hashes
// byte-unchanged, no id above the pre-unlock max appears, and the
// parent's mirror is NOT flipped by planning (the rewrite does that).
func TestInterleavedParentUnlockPlansOnlyUnassignedChunks(t *testing.T) {
	cat := interleavedParentFixture(t, "S-INTER")
	const setID = "S-INTER"
	// FIXTURE LEGALITY (the helper's claim, asserted mechanically): the
	// hand-built {2,1,2} catalog is a LEGAL plan state - every image's
	// total charge (payload + the canonical estimator's overhead +
	// per-entry charge) is within the bd-r-sl usable budget, and that
	// budget is the 23 GB figure the geometry is worked out against.
	assertImagesWithinBudget(t, cat, setID, 2)
	for _, ic := range readImageCharges(t, cat, setID) {
		if ic.usable != 23_000_000_000 {
			t.Errorf("image %d usable = %d, want the bd-r-sl figure 23000000000 (the 20 GiB chunks' budget)",
				ic.imageID, ic.usable)
		}
		if ic.payload == 0 {
			t.Errorf("image %d holds no payload: the fixture's interleave is not being read back", ic.imageID)
		}
	}
	seedWrittenChunkState(t, cat, setID)
	// A completed copy had marked the chunk parent's mirror; the
	// unlock's mirror fix clears it (U.1 step 1) and planning must
	// leave it cleared.
	if _, err := catRW(t, cat).Exec(`UPDATE files SET copied = 1 WHERE set_id = ?`, setID); err != nil {
		t.Fatalf("seeding the parent mirror: %v", err)
	}
	before := chunkSnapshot(t, cat, setID)
	if before[0].imageID != 2 || before[1].imageID != 1 || before[2].imageID != 2 {
		t.Fatalf("fixture placement = (%d,%d,%d), want the {2,1,2} interleave",
			before[0].imageID, before[1].imageID, before[2].imageID)
	}

	// THE REAL COMMANDS: unlock image 1 (chunk1's image), then plan
	// under the SAME bd-r-sl profile - the reuse path must complete
	// the set with exit 0.
	if stdout, stderr, code := runClearChild(t,
		"--catalog", cat, "--set-id", setID, "--image", "1"); code != report.ExitOK {
		t.Fatalf("clear --image 1 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", setID, "--size", "25", "--media-profile", "bd-r-sl", "--json")
	if code != report.ExitOK {
		t.Fatalf("plan over the interleaved partial set exit = %d, want 0 (AC-22 planning half)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	written := decodeSingleJSONDoc(t, stdout)
	if num64(t, written, "assigned_chunks") != 1 {
		t.Errorf("assigned_chunks = %d, want 1 (only the released chunk)", num64(t, written, "assigned_chunks"))
	}
	if num64(t, written, "rechunked_files") != 0 || num64(t, written, "rehashed_at_next_write") != 0 {
		t.Errorf("re-plan reports rechunked %d / rehashed %d, want 0/0 (unchanged effective chunk size)",
			num64(t, written, "rechunked_files"), num64(t, written, "rehashed_at_next_write"))
	}
	ids := destImageIDs(t, written)
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Errorf("destination ids = %v, want {1,2}: the released slot 1 refills, no id above the pre-unlock max 2", ids)
	}

	after := chunkSnapshot(t, cat, setID)
	// Only chunk1 (the released one) moved: it comes back on the
	// re-minted slot 1.
	if after[1].imageID != 1 {
		t.Errorf("released chunk1 re-placed on image %d, want the re-minted slot 1", after[1].imageID)
	}
	// The locked half is byte-unchanged (image_id, copied,
	// verified_at, hash).
	for _, idx := range []int{0, 2} {
		if !chunkSnapEqual(after[idx], before[idx]) {
			t.Errorf("locked chunk %d mutated by the re-plan:\nbefore %+v\nafter  %+v",
				idx, before[idx], after[idx])
		}
	}
	// The parent mirror is NOT flipped by planning: the count-proof
	// flips it when the rewrite marks the released chunk (U.3's leg).
	db := catOpen(t, cat)
	var parentImage sql.NullInt64
	var parentCopied int
	if err := db.QueryRow(`SELECT image_id, copied FROM files WHERE set_id = ? AND relpath = ?`,
		setID, "big.bin").Scan(&parentImage, &parentCopied); err != nil {
		t.Fatalf("reading the chunk parent: %v", err)
	}
	if parentImage.Valid || parentCopied != 0 {
		t.Errorf("chunk parent after planning = (image %v, copied %d), want (NULL, 0): "+
			"planning assigns chunks only; the mirror flip belongs to the rewrite leg",
			parentImage, parentCopied)
	}
	// ... and the re-planned state is STILL a legal plan: the released
	// chunk went back to the re-minted slot 1 without overfilling it.
	assertImagesWithinBudget(t, cat, setID, 2)
}

// --- AC-36: gapped-catalog determinism (load-bearing, do not defer) -------------

// TestReplanGappedCatalogDeterminismPlanAndReportAgree is AC-36: over
// a catalog with a HOLE (image 1 released), identical inputs produce
// identical ids across runs (two read-only reports byte-equal on ids
// and fills), the reopened slot fills FIRST (no id above the pre-
// unlock max), and the write agrees with the projection it was
// predicted from.
func TestReplanGappedCatalogDeterminismPlanAndReportAgree(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	const setID = "S-GAPPED"
	scanPlanFixture(t, root, cat, setID)
	if stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", setID, "--size", planFixtureSize); code != report.ExitOK {
		t.Fatalf("fresh plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// The hole: unlock image 1 (big.dat's chunk0's image).
	if stdout, stderr, code := runClearChild(t,
		"--catalog", cat, "--set-id", setID, "--image", "1"); code != report.ExitOK {
		t.Fatalf("clear --image 1 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	// Same inputs -> same ids: two independent read-only projections
	// over the gapped catalog must be identical (ids AND fills).
	first := runReportJSON(t, cat, setID)
	second := runReportJSON(t, cat, setID)
	if !sameStrings(destLines(t, first), destLines(t, second)) {
		t.Errorf("two report runs over the same gapped inputs disagree:\n#1 %v\n#2 %v",
			destLines(t, first), destLines(t, second))
	}
	if num64(t, first, "media_count") != num64(t, second, "media_count") ||
		num64(t, first, "new_destinations") != num64(t, second, "new_destinations") {
		t.Errorf("report runs disagree on counts: (%d,%d) vs (%d,%d)",
			num64(t, first, "media_count"), num64(t, first, "new_destinations"),
			num64(t, second, "media_count"), num64(t, second, "new_destinations"))
	}

	// The reopened slot fills FIRST: the write mints id 1, never an
	// id above the pre-unlock max (2), and agrees with the
	// projection.
	written := runPlanJSON(t, cat, setID)
	ids := destImageIDs(t, written)
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Fatalf("re-plan destination ids = %v, want {1,2}: the reopened slot fills first, no id above the max", ids)
	}
	assertPlanReportAgree(t, first, written)

	// The payload really landed in the reopened slot.
	_, chunks, _ := readAssignments(t, cat, setID)
	if chunks["big.dat#0"] != 1 || chunks["big.dat#1"] != 2 {
		t.Errorf("chunk placement = (%d, %d), want (1, 2): chunk0 refills the reopened slot",
			chunks["big.dat#0"], chunks["big.dat#1"])
	}
}
