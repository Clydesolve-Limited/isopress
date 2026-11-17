//go:build e2e

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"isopress/internal/report"
	"isopress/internal/units"
)

// TestReportProjectionRoundTripsIntoRealPlan is the core FR-009 promise
// driven end to end: the PROJECTION report (before any plan exists)
// states a media count, and the subsequent REAL plan with the same
// inputs creates EXACTLY that many images rows - not one more, not one
// less. Beyond the count, every projected per-destination number (fill,
// items, dirs) must equal the written plan's, destination for
// destination, and the written rows must match the catalog sums.
func TestReportProjectionRoundTripsIntoRealPlan(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-RT")

	// Leg 1: the projection, before anything is planned.
	repOut, repErr, code := runReportChild(t,
		"--catalog", cat, "--set-id", "S-RT", "--size", planFixtureSize, "--json")
	if code != report.ExitOK {
		t.Fatalf("report (projection) exit = %d, want 0\nstdout: %s\nstderr: %s",
			code, repOut, repErr)
	}
	repDoc := decodeSingleJSONDoc(t, repOut)

	// Leg 2: the real plan, identical inputs.
	planOut, planErr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", "S-RT", "--size", planFixtureSize, "--json")
	if code != report.ExitOK {
		t.Fatalf("plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, planOut, planErr)
	}
	planDoc := decodeSingleJSONDoc(t, planOut)

	// Frozen key sets through both real commands (the pins live in
	// planreport_test.go; this re-asserts the real pipeline emits them).
	wantKeys := append([]string(nil), planReportJSONKeys...)
	sort.Strings(wantKeys)
	for _, doc := range []map[string]any{repDoc, planDoc} {
		if got := sortedKeys(doc); !sameStrings(got, wantKeys) {
			t.Fatalf("json key set = %v\nwant exactly %v (schema freeze)", got, wantKeys)
		}
	}

	// The round trip itself: report says N, plan creates exactly N.
	n := int(repDoc["media_count"].(float64))
	if n < 2 {
		t.Fatalf("fixture projects %d media, want >= 2 for a meaningful round trip", n)
	}
	if int(repDoc["new_destinations"].(float64)) != n {
		t.Errorf("projection new_destinations = %v, want %d (nothing existed yet)",
			repDoc["new_destinations"], n)
	}
	if got := int(planDoc["media_count"].(float64)); got != n {
		t.Errorf("plan media_count = %d, want the projected %d (FR-009 round trip)", got, n)
	}
	if got := int(planDoc["new_destinations"].(float64)); got != n {
		t.Errorf("plan new_destinations = %d, want %d (all destinations were new)", got, n)
	}

	db := catOpen(t, cat)
	var images int
	if err := db.QueryRow(`SELECT COUNT(*) FROM images WHERE set_id = 'S-RT'`).Scan(&images); err != nil {
		t.Fatalf("count images: %v", err)
	}
	if images != n {
		t.Errorf("images rows = %d, want the projected %d (projection-vs-reality)", images, n)
	}

	// Destination-for-destination equality of the projection and the
	// written plan (all frozen per-destination keys, including the
	// truncated fill_percent).
	repDests, ok := repDoc["destinations"].([]any)
	if !ok || len(repDests) != n {
		t.Fatalf("projection destinations = %v, want %d entries", repDoc["destinations"], n)
	}
	planDests, ok := planDoc["destinations"].([]any)
	if !ok || len(planDests) != n {
		t.Fatalf("plan destinations = %v, want %d entries", planDoc["destinations"], n)
	}
	for i := range repDests {
		r, p := repDests[i].(map[string]any), planDests[i].(map[string]any)
		for _, k := range destFillJSONKeys {
			if r[k] != p[k] {
				t.Errorf("destination %d key %q: projection %v != plan %v (projection must equal reality)",
					i, k, r[k], p[k])
			}
		}
	}

	// And the written plan's numbers match the CATALOG rows (the sums
	// the fill assertions in plan_test.go compute independently).
	fills := queryDestFill(t, db, "S-RT")
	for _, d := range planDests {
		dest := d.(map[string]any)
		id := int64(dest["image"].(float64))
		f := fills[id]
		if f.usedBytes != int64(dest["used_bytes"].(float64)) ||
			f.items != int(dest["items"].(float64)) || f.dirs != int(dest["dirs"].(float64)) {
			t.Errorf("destination %d: catalog fill %+v != reported %+v",
				id, f, dest)
		}
	}
}

// --- 3. plan resumability: locked assignments stay byte-identical --------------

// TestPlanReportStreamSeparationPurity pins FR-046/10.3 on the real
// commands for BOTH output modes: --json stdout is EXACTLY one JSON
// document (nothing else), the human stdout carries ONLY the report
// body, and the progress phase summary appears on stderr only - never
// the report body on stderr, never the summary on stdout.
func TestPlanReportStreamSeparationPurity(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-STREAM")

	// The progress summary formats of runPlanReport (stderr side).
	const (
		planSummary    = "plan: media 2 (2 new); files 2, chunks 2, symlinks 1, dirs 1"
		reportSummary  = "report: media 2 (2 new); files 2, chunks 2, symlinks 1, dirs 1"
		reportBodyHead = "destinations total" // the human report's media line
		progressTell   = "); files"           // the summary's tail - never on stdout
	)

	assertStreams := func(t *testing.T, name, stdout, stderr string,
		wantStdout, wantStderr []string) {
		t.Helper()
		for _, want := range wantStdout {
			if !strings.Contains(stdout, want) {
				t.Errorf("%s stdout lacks %q:\n%s", name, want, stdout)
			}
		}
		for _, want := range wantStderr {
			if !strings.Contains(stderr, want) {
				t.Errorf("%s stderr lacks %q:\n%s", name, want, stderr)
			}
		}
		// Progress stays OFF stdout; the report body stays OFF stderr.
		if strings.Contains(stdout, progressTell) {
			t.Errorf("%s stdout carries the progress summary (stream violation):\n%s", name, stdout)
		}
		if strings.Contains(stderr, reportBodyHead) {
			t.Errorf("%s stderr carries the report body (stream violation):\n%s", name, stderr)
		}
	}

	// Report legs FIRST (read-only; the projection stays valid), then
	// the plan legs, which write the plan.
	t.Run("report --json", func(t *testing.T) {
		stdout, stderr, code := runReportChild(t,
			"--catalog", cat, "--set-id", "S-STREAM", "--size", planFixtureSize, "--json")
		if code != report.ExitOK {
			t.Fatalf("report --json exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		// Exactly one JSON document: nothing else on stdout.
		decodeSingleJSONDoc(t, stdout)
		assertStreams(t, "report --json", stdout, stderr, nil, []string{reportSummary})
	})

	t.Run("report human", func(t *testing.T) {
		stdout, stderr, code := runReportChild(t,
			"--catalog", cat, "--set-id", "S-STREAM", "--size", planFixtureSize)
		if code != report.ExitOK {
			t.Fatalf("report exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		assertStreams(t, "report human", stdout, stderr,
			[]string{"report (projection): set S-STREAM", reportBodyHead}, []string{reportSummary})
	})

	t.Run("plan --json", func(t *testing.T) {
		stdout, stderr, code := runPlanChild(t,
			"--catalog", cat, "--set-id", "S-STREAM", "--size", planFixtureSize, "--json")
		if code != report.ExitOK {
			t.Fatalf("plan --json exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		decodeSingleJSONDoc(t, stdout)
		assertStreams(t, "plan --json", stdout, stderr, nil, []string{planSummary})
	})

	t.Run("plan human", func(t *testing.T) {
		// A second plan run is a no-op, but its streams follow the same
		// separation; the summary changes to the nothing-to-plan form.
		stdout, stderr, code := runPlanChild(t,
			"--catalog", cat, "--set-id", "S-STREAM", "--size", planFixtureSize)
		if code != report.ExitOK {
			t.Fatalf("plan re-run exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		assertStreams(t, "plan human", stdout, stderr,
			[]string{"plan: set S-STREAM", reportBodyHead},
			[]string{"plan: nothing to plan (media 2)"})
	})
}

// --- 6. the production refuseWrites wiring is unreachable in report mode -------

// TestReportProductionWiringNeverTripsRefuseWrites drives the SHARED
// pipeline with the report command's PRODUCTION hook (refuseWrites) and
// mode: the projection renders, the run succeeds, and the tripwire -
// proven to fire by a direct call in TestRefuseWritesIsATripwire - is
// never reached. If the mode gate in runPlanReport were removed or
// inverted, this test would fail with the errReportWrite tripwire.
func TestReportProductionWiringNeverTripsRefuseWrites(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-WIRE")
	before := fileBytes(t, cat)

	opts := &rootOptions{catalog: cat, setID: "S-WIRE"}
	po := planOptions{size: planFixtureSize, mediaProfile: units.DefaultProfile}
	cmd := &cobra.Command{Use: "report"}
	outBuf, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(outBuf)
	cmd.SetErr(errBuf)

	if err := runPlanReport(context.Background(), cmd, opts, po, modeReport, refuseWrites); err != nil {
		t.Fatalf("production report wiring (modeReport + refuseWrites) failed: %v "+
			"(the mode gate must keep the tripwire unreachable)", err)
	}
	if !strings.Contains(outBuf.String(), "report (projection): set S-WIRE") {
		t.Errorf("production wiring output lacks the projection heading:\n%s", outBuf.String())
	}
	if !bytes.Equal(before, fileBytes(t, cat)) {
		t.Error("catalog bytes changed across the production report wiring (zero-write violation)")
	}
}

// --- 7. plan heals a pre-T2.2 catalog on its MUTATING open ---------------------

// TestPlanOversizedFileChunksSpanDestinations drives a 2.45 GB sparse
// file through the real scan -> report -> plan -> report flow at
// --size 1 (usable 980 MB): under the 2026-10-20 capacity model it
// MUST chunk into 977,901,312 + 977,901,312 + 494,197,376 bytes (the
// effective chunk size is usable - reserved overhead) and the chunks
// must land on THREE distinct destinations (worst-fit fills the first
// two budgets exactly). The projection report states the chunk count
// before anything is written (FR-009), and the report after the plan
// confirms it against the persisted rows.
func TestPlanOversizedFileChunksSpanDestinations(t *testing.T) {
	root := t.TempDir()
	big := filepath.Join(root, "huge.dat")
	f, err := os.Create(big)
	if err != nil {
		t.Fatalf("create huge.dat: %v", err)
	}
	// Sparse: logical size only; the planner is metadata-only (12.5) and
	// hashing the holes reads fast zeros.
	if err := f.Truncate(2_450_000_000); err != nil {
		t.Fatalf("truncate huge.dat: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close huge.dat: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "small.txt"), []byte("small\n"), 0o644); err != nil {
		t.Fatalf("write small.txt: %v", err)
	}
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-CHUNK3")

	const (
		// The effective chunk size under the fixture profile: usable
		// minus the reserved overhead (4.2 as tied to the 4.3 capacity
		// model by the 2026-10-20 amendment).
		chunkSize = planFixtureChunk0
		bigSize   = 2_450_000_000
	)

	// Leg 1: the PROJECTION shows the chunk count and the three
	// destinations before anything is written.
	stdout, stderr, code := runReportChild(t,
		"--catalog", cat, "--set-id", "S-CHUNK3", "--size", planFixtureSize)
	if code != report.ExitOK {
		t.Fatalf("report (projection) exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	for _, want := range []string{
		"media: 3 destinations total (3 new, 0 pre-existing)",
		// The chunk count IS the projection headline: 3 chunks of the
		// oversized file, plus the one whole small file (1 files).
		"assigned this run: 1 files, 3 chunks, 0 symlinks, 0 dirs (hard-link secondaries excluded: 0)",
		"image 1: 1 items, 0 dirs, 977.9 MB / 980.0 MB (99%)",
		"image 2: 1 items, 0 dirs, 977.9 MB / 980.0 MB (99%)",
		"image 3: 2 items, 0 dirs, 494.2 MB / 980.0 MB (50%)", // chunk 3 + small.txt
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("projection report lacks %q:\n%s", want, stdout)
		}
	}

	// Leg 2: the real plan persists exactly that placement.
	stdout, stderr, code = runPlanChild(t,
		"--catalog", cat, "--set-id", "S-CHUNK3", "--size", planFixtureSize, "--json")
	if code != report.ExitOK {
		t.Fatalf("plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	doc := decodeSingleJSONDoc(t, stdout)
	if doc["media_count"].(float64) != 3 || doc["assigned_chunks"].(float64) != 3 {
		t.Errorf("plan json = media %v chunks %v, want 3/3", doc["media_count"], doc["assigned_chunks"])
	}

	db := catOpen(t, cat)
	chunks := catChunkRows(t, db, "S-CHUNK3", "huge.dat")
	if len(chunks) != 3 {
		t.Fatalf("huge.dat chunks = %d (%+v), want 3", len(chunks), chunks)
	}
	wantLengths := []int64{chunkSize, chunkSize, bigSize - 2*chunkSize}
	var offset int64
	for i, c := range chunks {
		if c.index != i || c.offset != offset || c.length != wantLengths[i] || !c.assigned {
			t.Errorf("chunk %d = %+v, want index %d offset %d length %d assigned",
				i, c, i, offset, wantLengths[i])
		}
		offset += c.length
	}

	// The chunk-to-destination mapping, read directly: three DISTINCT
	// destinations (the brief's >= 2 requirement, at full strength).
	crows, err := db.Query(`SELECT c.chunk_index, c.image_id FROM chunks c
	                        JOIN files f ON f.file_id = c.file_id
	                        WHERE c.set_id = 'S-CHUNK3' AND f.relpath = 'huge.dat'
	                        ORDER BY c.chunk_index`)
	if err != nil {
		t.Fatalf("query chunk placements: %v", err)
	}
	defer crows.Close()
	seen := map[int64]int{}
	var order []int64
	for crows.Next() {
		var idx int
		var imageID int64
		if err := crows.Scan(&idx, &imageID); err != nil {
			t.Fatalf("scan chunk placement: %v", err)
		}
		seen[imageID]++
		order = append(order, imageID)
	}
	if err := crows.Err(); err != nil {
		t.Fatalf("iterate chunk placements: %v", err)
	}
	if len(seen) < 2 {
		t.Errorf("chunks span %d destinations (%v), want >= 2", len(seen), order)
	}
	// Worst-fit-decreasing with these sizes is deterministic: chunk 0
	// fills image 1, chunk 1 fills image 2, chunk 2 opens image 3.
	if order[0] != 1 || order[1] != 2 || order[2] != 3 {
		t.Errorf("chunk destinations = %v, want [1 2 3] (WFD, ascending)", order)
	}

	// small.txt rides along on the last (emptiest) destination.
	srows := queryFiles(t, db, "small.txt")
	if len(srows) != 1 || !srows[0].ImageID.Valid || srows[0].ImageID.Int64 != 3 {
		t.Errorf("small.txt rows = %+v, want one row on destination 3", srows)
	}

	// Per-destination fill within usable (the 4.3 capacity invariant).
	fills := queryDestFill(t, db, "S-CHUNK3")
	for id, f := range fills {
		if f.usedBytes > planFixtureUsable {
			t.Errorf("destination %d fill %d exceeds usable %d (capacity violation)",
				id, f.usedBytes, planFixtureUsable)
		}
	}

	// Leg 3: the report after the plan shows the same chunk counts and
	// zero new destinations (nothing left unassigned).
	stdout, stderr, code = runReportChild(t,
		"--catalog", cat, "--set-id", "S-CHUNK3", "--size", planFixtureSize, "--json")
	if code != report.ExitOK {
		t.Fatalf("report after plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	repDoc := decodeSingleJSONDoc(t, stdout)
	if repDoc["media_count"].(float64) != 3 || repDoc["new_destinations"].(float64) != 0 {
		t.Errorf("report json = media %v new %v, want 3/0", repDoc["media_count"], repDoc["new_destinations"])
	}
	// With everything planned, a report run assigns NOTHING, so the
	// assigned_* counters are 0 by design (they count what a run would
	// assign now); the chunk COUNTS are visible in the projection leg
	// above and in the persisted chunk rows asserted via catChunkRows.
	if repDoc["nothing_to_plan"] != true {
		t.Errorf("report json nothing_to_plan = %v, want true after the full plan", repDoc["nothing_to_plan"])
	}
}
