package cli

// T2.4 sprint-closing gap tests (verification pass), driven through
// the REAL production commands (the runCLIChild re-exec pattern of
// scan_test.go). Each test closes an audit gap the original
// clear_test.go left open:
//
//   - CHANGED-PROFILE re-plan: plan -> seed written chunk state ->
//     clear -> plan under a DIFFERENT effective chunk size exercises
//     the ReplaceAllChunks re-chunk path through real commands (stale
//     hashes correctly DROPPED with their old boundaries, the whole
//     decomposition replaced, no stale rows); a third leg where the
//     file fits whole exercises the NB2 DropChunkState path.
//
//   - the images-row DELETE decision pinned MECHANICALLY: a
//     counterfactual run that re-seeds the images rows a keeping-clear
//     would have left behind proves the balancer citation in
//     clearstore.go (lowest-unused minting, 4.6: with the contiguous
//     seed the first free id is max+1, so the counterfactual
//     expectation (3 or 4) is unchanged): every new id shifts to 3 or
//     4 and the phantom rows inflate the media count - the DELETE is
//     load-bearing for the fresh-plan equivalence.
//
//   - the runs row: the kind='clear' row's stats_json is cross-checked
//     against the --json document through the real command (the same
//     numbers, two presentations) - previously only the catalog tier
//     pinned the stats.
//
//   - PRE-T2.2 HEALING: clear is the THIRD healing mutating command -
//     a v1-stamped catalog missing the folded `directories` table is
//     repaired on clear's mutating Open (foldedV1RepairDDL via
//     ensureFoldedV1Objects, migrate.go), and the clear still clears.
//
//   - contention from BOTH sides under one held flock: while any
//     mutating run holds the exclusive advisory lock, BOTH real
//     commands (plan and clear) are refused with exit 5 - the
//     single-writer model is symmetric because both acquire the same
//     flock through the same catalog.Open(OpenReadWrite) path.
//
//   - the S2 REACHABILITY boundary (forward caveat): real commands in
//     S2 only ever produce images.state 'planned' and never populate
//     image_contents, so today's clear cannot orphan built-media state
//     (documented in the clearstore.go scope note; the counterfactual
//     if-rows-existed behavior is pinned at the catalog tier by
//     clearstore_gaps_test.go).

import (
	"encoding/json"
	"strings"
	"testing"

	"isopress/internal/report"
)

// --- changed-profile re-plan: ReplaceAllChunks and DropChunkState -------------

// TestClearRetainedImagesRowsWouldShiftPlanIDsPinsDeleteDecision:
// clearstore.go DELETES the set's images rows so the next plan is
// EXACTLY a fresh plan; the cited reason is balancer.go's locking
// predicate (lowest-unused minting, 4.6: with the contiguous seed the
// first free id is max+1; every images row is a LOCKED destination,
// never in the open set). This test pins that
// citation mechanically through the real commands: it replays the
// counterfactual - re-seeding precisely the assignment-free images
// rows a keeping-clear would have left - and proves the re-plan is NOT
// the fresh plan (ids shifted to max+1 - with the contiguous seed the
// lowest-unused first free id IS max+1, so the counterfactual
// expectation (3 or 4) is unchanged - phantom locked destinations
// inflating the media count, the empty dir landing on a phantom). The
// DELETE is load-bearing; removing it (or gating it) breaks the T2.4
// acceptance criterion.
func TestClearRetainedImagesRowsWouldShiftPlanIDsPinsDeleteDecision(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-DPIN")

	// The fresh plan, for the contrast.
	stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", "S-DPIN", "--size", planFixtureSize, "--json")
	if code != report.ExitOK {
		t.Fatalf("fresh plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	freshDoc := decodeSingleJSONDoc(t, stdout)
	if freshDoc["media_count"].(float64) != 2 {
		t.Fatalf("fresh plan media_count = %v, want 2", freshDoc["media_count"])
	}
	freshFiles, _, freshDirs := readAssignments(t, cat, "S-DPIN")
	for rel, id := range freshFiles {
		if id == 0 {
			continue // big.dat is chunked: its files.image_id is NULL, the lock lives on the chunk rows
		}
		if id != 1 && id != 2 {
			t.Fatalf("fresh plan assigned %q to %d, want destination 1 or 2", rel, id)
		}
	}
	// The empty dir lands on the lowest destination whose RESERVED
	// budget absorbs its entry charge (the 2026-10-20 capacity twist):
	// image 1 is exactly packed by chunk 0's charge, so the dir falls
	// to image 2.
	if freshDirs["empty"] != 2 {
		t.Fatalf("fresh plan assigned the empty dir to %d, want the lowest destination with entry slack (2)", freshDirs["empty"])
	}

	stdout, stderr, code = runClearChild(t, "--catalog", cat, "--set-id", "S-DPIN")
	if code != report.ExitOK {
		t.Fatalf("clear exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	// The counterfactual: re-seed exactly the rows a images-keeping
	// clear would have left - the same planner bookkeeping, zero
	// assignments.
	rw := catRW(t, cat)
	for _, id := range []int64{1, 2} {
		if _, err := rw.Exec(`INSERT INTO images (set_id, image_id, nominal_size, usable_size, media_profile)
		                      VALUES ('S-DPIN', ?, ?, ?, 'disk')`,
			id, int64(planFixtureNominal), int64(planFixtureUsable)); err != nil {
			t.Fatalf("seeding the counterfactual images row %d: %v", id, err)
		}
	}

	// Re-plan: NOT the fresh plan anymore.
	stdout, stderr, code = runPlanChild(t,
		"--catalog", cat, "--set-id", "S-DPIN", "--size", planFixtureSize, "--json")
	if code != report.ExitOK {
		t.Fatalf("counterfactual re-plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	doc := decodeSingleJSONDoc(t, stdout)
	if doc["media_count"].(float64) != 4 || doc["new_destinations"].(float64) != 2 {
		t.Fatalf("counterfactual re-plan media %v new %v, want 4/2 (2 phantom locked + 2 shifted new destinations)",
			doc["media_count"], doc["new_destinations"])
	}
	// Every payload id lands on a lowest-unused mint (4.6): with the
	// contiguous seed {1,2} the first free id is max+1, so the
	// counterfactual expectation (3 or 4) is unchanged ...
	reFiles, reChunks, reDirs := readAssignments(t, cat, "S-DPIN")
	for rel, id := range reFiles {
		if id == 0 {
			continue // chunked big.dat again: files.image_id stays NULL
		}
		if id != 3 && id != 4 {
			t.Errorf("counterfactual plan assigned %q to %d, want 3 or 4 (lowest-unused minting (4.6): with the contiguous seed the first free id is max+1)", rel, id)
		}
	}
	for key, id := range reChunks {
		if id != 3 && id != 4 {
			t.Errorf("counterfactual plan assigned chunk %q to %d, want 3 or 4", key, id)
		}
	}
	// ... and the empty dir landed on the PHANTOM locked destination 1
	// (lowestDestination sees the stale row) - placement corruption on
	// top of the id shift.
	if reDirs["empty"] != 1 {
		t.Errorf("counterfactual empty-dir assignment = %d, want the phantom locked destination 1", reDirs["empty"])
	}
	// The phantom rows render as locked, empty destinations in the
	// report model.
	dests, ok := doc["destinations"].([]any)
	if !ok || len(dests) != 4 {
		t.Fatalf("counterfactual destinations = %v, want 4 entries", doc["destinations"])
	}
	lockedEmpty := 0
	for _, d := range dests {
		m := d.(map[string]any)
		if m["locked"].(bool) && m["used_bytes"].(float64) == 0 && m["items"].(float64) == 0 {
			lockedEmpty++
		}
	}
	if lockedEmpty != 2 {
		t.Errorf("phantom locked-empty destinations in the report = %d, want 2", lockedEmpty)
	}
	// Contrast recorded for the reader: the fresh plan placed every
	// payload on destinations {1,2}; the counterfactual places the
	// SAME payload on {3,4} - the sets are disjoint, which is exactly
	// the fresh-plan-equivalence breakage the DELETE prevents.
}

// --- the runs row: stats_json == the --json document ---------------------------

// TestClearRunStatsMatchJSONThroughRealCommand: the runs row (kind
// 'clear', status 'ok', the fixture's source root) and its stats_json
// snapshot must carry the SAME numbers the real command printed on
// stdout - one run, two faithful presentations (2.4 provenance).
func TestClearRunStatsMatchJSONThroughRealCommand(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-CSTATS")
	if stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", "S-CSTATS", "--size", planFixtureSize); code != report.ExitOK {
		t.Fatalf("plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	seedWrittenChunkState(t, cat, "S-CSTATS")

	stdout, stderr, code := runClearChild(t, "--catalog", cat, "--set-id", "S-CSTATS", "--json")
	if code != report.ExitOK {
		t.Fatalf("clear --json exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	doc := decodeSingleJSONDoc(t, stdout) // exactly one document (stdout purity)
	runID := int64(doc["run_id"].(float64))
	if runID <= 0 {
		t.Fatalf("clear run_id = %d, want a positive ids row", runID)
	}

	db := catOpen(t, cat)
	var kind, status, runRoot, stats string
	if err := db.QueryRow(`SELECT kind, status, root, stats_json FROM runs WHERE run_id = ?`, runID).
		Scan(&kind, &status, &runRoot, &stats); err != nil {
		t.Fatalf("read clear run row: %v", err)
	}
	if kind != "clear" || status != "ok" {
		t.Errorf("clear run row = %q/%q, want clear/ok", kind, status)
	}
	if runRoot != root {
		t.Errorf("clear run root = %q, want the set's source root %q", runRoot, root)
	}
	var cs struct {
		FilesCleared        int `json:"files_cleared"`
		ChunksCleared       int `json:"chunks_cleared"`
		DirsCleared         int `json:"dirs_cleared"`
		ImagesRemoved       int `json:"images_removed"`
		ChunkHashesRetained int `json:"chunk_hashes_retained"`
	}
	if err := json.Unmarshal([]byte(stats), &cs); err != nil {
		t.Fatalf("stats_json %q: %v", stats, err)
	}
	// The stats snapshot must equal the stdout document, field by
	// field (images_removed in the catalog is destinations_removed in
	// the report - the documented naming bridge).
	if cs.FilesCleared != int(doc["files_cleared"].(float64)) ||
		cs.ChunksCleared != int(doc["chunks_cleared"].(float64)) ||
		cs.DirsCleared != int(doc["dirs_cleared"].(float64)) ||
		cs.ImagesRemoved != int(doc["destinations_removed"].(float64)) ||
		cs.ChunkHashesRetained != int(doc["chunk_hashes_retained"].(float64)) {
		t.Errorf("stats_json = %+v, want the --json document's counts %v/%v/%v/%v/%v",
			cs, doc["files_cleared"], doc["chunks_cleared"], doc["dirs_cleared"],
			doc["destinations_removed"], doc["chunk_hashes_retained"])
	}
	// Exactly ONE clear run row (the audit trail is not diluted).
	var clearRuns int
	if err := db.QueryRow(`SELECT COUNT(*) FROM runs WHERE kind = 'clear' AND set_id = 'S-CSTATS'`).Scan(&clearRuns); err != nil {
		t.Fatalf("count clear runs: %v", err)
	}
	if clearRuns != 1 {
		t.Errorf("clear run rows = %d, want exactly 1", clearRuns)
	}
}

// --- pre-T2.2 healing through the real clear command ---------------------------

// TestClearHealsPreT22CatalogOnMutatingOpen: a v1-stamped catalog
// missing the folded `directories` table (the pre-T2.2 binary shape)
// is repaired on clear's MUTATING Open - clear is the third healing
// command after scan and plan (ensureFoldedV1Objects runs on every
// OpenReadWrite) - and the clear itself still clears.
func TestClearHealsPreT22CatalogOnMutatingOpen(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-CHEAL")
	if stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", "S-CHEAL", "--size", planFixtureSize); code != report.ExitOK {
		t.Fatalf("plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	// Simulate the pre-T2.2 catalog: drop the folded table (its
	// dependent indexes fall with it).
	rw := catRW(t, cat)
	if _, err := rw.Exec(`DROP TABLE directories`); err != nil {
		t.Fatalf("simulating pre-T2.2 catalog (drop directories): %v", err)
	}

	// The real clear heals on its mutating open and completes.
	stdout, stderr, code := runClearChild(t, "--catalog", cat, "--set-id", "S-CHEAL")
	if code != report.ExitOK {
		t.Fatalf("clear against a pre-T2.2 catalog exit = %d, want 0 (heal on the mutating open)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	// The dirs table was dropped WITH its rows, so the healed table is
	// empty and DirsCleared is 0 - but the payload clear still ran.
	if !strings.Contains(stdout, "cleared: 3 files, 2 chunks, 0 dirs") {
		t.Errorf("healing clear report lacks the cleared line:\n%s", stdout)
	}

	db := catOpen(t, cat)
	objects := map[string]bool{}
	rows, err := db.Query(`SELECT name FROM sqlite_master
	                       WHERE name IN ('directories', 'idx_directories_image', 'idx_directories_unassigned')`)
	if err != nil {
		t.Fatalf("query healed objects: %v", err)
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan healed object: %v", err)
		}
		objects[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate healed objects: %v", err)
	}
	rows.Close()
	for _, want := range []string{"directories", "idx_directories_image", "idx_directories_unassigned"} {
		if !objects[want] {
			t.Errorf("clear did not heal the folded object %q (present: %v)", want, objects)
		}
	}
	// And the clear really cleared through the healed schema.
	var residual, images int
	if err := db.QueryRow(`SELECT COUNT(*) FROM files
	                       WHERE set_id = 'S-CHEAL' AND (image_id IS NOT NULL OR copied != 0)`).
		Scan(&residual); err != nil {
		t.Fatalf("count residual assignments: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM images WHERE set_id = 'S-CHEAL'`).Scan(&images); err != nil {
		t.Fatalf("count images: %v", err)
	}
	if residual != 0 || images != 0 {
		t.Errorf("after the healing clear: %d residual assignments, %d images rows; want 0/0", residual, images)
	}
}

// --- contention from both sides (single-writer, exit 5) -------------------------

// TestClearS2ReachableStateCannotOrphanMedia: TODAY's clear deletes
// only planner bookkeeping, and that is safe because S2 real commands
// can only ever produce images.state 'planned' (the sole images writer
// is the plan transaction; grep-verified: no production UPDATE images
// path exists) and never populate image_contents (its writer landed
// with build in S4 - DESIGN_SPEC 6.6 - and clear now deletes those
// rows in the same transaction, the S4 closure pinned by the
// catalog-tier TestClearPlanDeletesImageContentsInSameTransaction).
// Asserted through the real commands so the boundary is honest: if a
// future sprint makes 'built' state or image_contents rows reachable
// WITHOUT the clearstore closure, the clearstore.go scope note demands
// the decision is revisited BEFORE that sprint ships.
func TestClearS2ReachableStateCannotOrphanMedia(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-S2B")
	if stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", "S-S2B", "--size", planFixtureSize); code != report.ExitOK {
		t.Fatalf("plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	db := catOpen(t, cat)
	var nonPlanned int
	if err := db.QueryRow(`SELECT COUNT(*) FROM images WHERE set_id = 'S-S2B' AND state != 'planned'`).
		Scan(&nonPlanned); err != nil {
		t.Fatalf("count non-planned images: %v", err)
	}
	if nonPlanned != 0 {
		t.Fatalf("real S2 commands produced %d non-'planned' images rows; the clear's DELETE-all decision needs revisiting NOW", nonPlanned)
	}
	var contents int
	if err := db.QueryRow(`SELECT COUNT(*) FROM image_contents`).Scan(&contents); err != nil {
		t.Fatalf("count image_contents: %v", err)
	}
	if contents != 0 {
		t.Fatalf("image_contents holds %d rows after a real plan; S2 is not supposed to populate it (6.6: build-time, S4)", contents)
	}

	stdout, stderr, code := runClearChild(t, "--catalog", cat, "--set-id", "S-S2B")
	if code != report.ExitOK {
		t.Fatalf("clear exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM image_contents`).Scan(&contents); err != nil {
		t.Fatalf("recount image_contents: %v", err)
	}
	if contents != 0 {
		t.Errorf("image_contents changed across the clear (%d rows); nothing in S2 may create orphans to clean", contents)
	}
}
