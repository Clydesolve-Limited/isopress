package cli

// T2.3 golden and schema tests for the SHARED plan/report renderer
// (planreport.go). The human formats are pinned EXACTLY here - the
// fill-percentage format, the D-05 residual summary line, the D-06
// profile margin echo, the "[locked]" marker, the 4.2 re-hash
// statements, and the "nothing to plan" no-op report (IMPLEMENTATION_PLAN
// T2.3 acceptance: "Golden-output tests for the human-readable report
// on a fixed fixture plan, including the locked-gap summary line and
// the profile margin echo"). The --json document's key set is FROZEN
// for both commands - plan and report share the one planReport struct,
// so the key sets cannot drift (the scan report froze its keys the
// same way in scan_hardening_test.go; changing a key here is a
// deliberate schema change, pinned by these tests).

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"isopress/internal/plan"
	"isopress/internal/units"
)

// --- golden: the human-readable report ---------------------------------------

// TestPlanReportGoldenHuman pins the exact human output of the shared
// renderer on two fixed models: a plan-mode run with work, a locked
// destination, the D-05 residual figure, and both 4.2 statements; and
// a report-mode projection whose only locked destination holds the
// residual, ending in the "nothing to plan" no-op report.
func TestPlanReportGoldenHuman(t *testing.T) {
	t.Run("plan with work, locked destination, re-hash statements", func(t *testing.T) {
		rep := buildPlanReport(modePlan, "S-GOLD",
			units.Profile{Name: "disk", NominalBytes: 10_000_000_000, UsableBytes: 9_800_000_000},
			plan.Result{
				Destinations: []plan.DestFill{
					{ImageID: 1, NominalSize: 10_000_000_000, UsableSize: 9_800_000_000,
						MediaProfile: "disk", UsedBytes: 9_800_000_000,
						ItemCount: 4, DirCount: 1, Locked: true},
					{ImageID: 2, NominalSize: 10_000_000_000, UsableSize: 9_800_000_000,
						MediaProfile: "disk", UsedBytes: 1_000_000_000, ItemCount: 1},
				},
				NewDestinations:      1,
				FilesAssigned:        3,
				ChunksAssigned:       1,
				DirsAssigned:         1,
				WastedResidualBytes:  1_234_567_890,
				WastedResidualImages: 1,
				RechunkedFiles:       2,
				RehashedAtNextWrite:  4,
			})
		// Byte rendering follows units.FormatBytes (9.3): the largest
		// unit at which the count is >= 1, binary GiB at and above
		// 1 GiB - hence "9.3 GiB" for 10 GB nominal. The reserved-
		// overhead line under each image is the 2026-10-20 capacity
		// amendment (4.3): units.FixedImageOverhead + 512 per entry
		// (payload items + dirs), next to the true free space that
		// remains on the built image (clamped at 0 for the synthetic
		// over-full locked line, which predates the reservation).
		want := "plan: set S-GOLD (profile disk: nominal 9.3 GiB, usable 9.1 GiB, margin 2%)\n" +
			"media: 2 destinations total (1 new, 1 pre-existing)\n" +
			"image 1: 4 items, 1 dirs, 9.1 GiB / 9.1 GiB (100%) [locked]\n" +
			"  reserved overhead: 2.1 MB (fixed 2.1 MB + 5 entries), free after build: 0 B\n" +
			"image 2: 1 items, 0 dirs, 1.0 GB / 9.1 GiB (10%)\n" +
			"  reserved overhead: 2.1 MB (fixed 2.1 MB + 1 entries), free after build: 8.2 GiB\n" +
			"residual space on locked destinations: 1.1 GiB across 1 images\n" +
			"assigned this run: 3 files, 1 chunks, 0 symlinks, 1 dirs (hard-link secondaries excluded: 0)\n" +
			"re-chunked files: 2\n" +
			"already-written chunks re-hashed at next write: 4\n"
		var buf bytes.Buffer
		if err := writePlanReport(&buf, rep, false); err != nil {
			t.Fatalf("writePlanReport: %v", err)
		}
		if got := buf.String(); got != want {
			t.Errorf("golden human report mismatch:\ngot:\n%s\nwant:\n%s", got, want)
		}
	})

	t.Run("report projection, nothing to plan", func(t *testing.T) {
		rep := buildPlanReport(modeReport, "S-PROJ",
			units.Profile{Name: "bd-r-sl", NominalBytes: 25_000_000_000, UsableBytes: 23_000_000_000},
			plan.Result{
				Destinations: []plan.DestFill{
					{ImageID: 1, NominalSize: 25_000_000_000, UsableSize: 23_000_000_000,
						MediaProfile: "bd-r-sl", UsedBytes: 500_000_000,
						ItemCount: 2, Locked: true},
				},
				WastedResidualBytes:  22_500_000_000,
				WastedResidualImages: 1,
			})
		// The reserved-overhead line (the 2026-10-20 amendment) shows
		// the fixed term plus 2 entries here: the 2 payload items and
		// zero dirs.
		want := "report (projection): set S-PROJ (profile bd-r-sl: nominal 23.3 GiB, usable 21.4 GiB, margin 8%)\n" +
			"media: 1 destinations total (0 new, 1 pre-existing)\n" +
			"image 1: 2 items, 0 dirs, 500.0 MB / 21.4 GiB (2%) [locked]\n" +
			"  reserved overhead: 2.1 MB (fixed 2.1 MB + 2 entries), free after build: 21.0 GiB\n" +
			"residual space on locked destinations: 21.0 GiB across 1 images\n" +
			"nothing to plan: no unassigned items\n"
		var buf bytes.Buffer
		if err := writePlanReport(&buf, rep, false); err != nil {
			t.Fatalf("writePlanReport: %v", err)
		}
		if got := buf.String(); got != want {
			t.Errorf("golden projection report mismatch:\ngot:\n%s\nwant:\n%s", got, want)
		}
	})
}

// --- frozen --json schema -----------------------------------------------------

// planReportJSONKeys is the frozen TOP-LEVEL key set of the plan/report
// --json document (19 keys), shared by BOTH commands because both
// render the same planReport struct - only the "mode" VALUE differs
// ("plan" vs "report"). Deliberate T2.3 schema, mirroring the scan
// report's frozen set in scan_hardening_test.go: any addition,
// removal, or rename must update this slice consciously.
var planReportJSONKeys = []string{
	"assigned_chunks", "assigned_dirs", "assigned_files", "assigned_symlinks",
	"destinations", "media_count", "media_profile", "margin_percent", "mode",
	"new_destinations", "nominal_size", "nothing_to_plan", "rechunked_files",
	"rehashed_at_next_write", "residual_bytes", "residual_images",
	"secondaries_excluded", "set_id", "usable_size",
}

// destFillJSONKeys is the frozen per-destination key set (9 keys).
var destFillJSONKeys = []string{
	"dirs", "fill_percent", "image", "items", "locked", "media_profile",
	"nominal_size", "used_bytes", "usable_size",
}

// TestPlanReportJSONSchemaFrozen pins the --json schema for both
// commands: marshal a plan-mode and a report-mode model built from the
// SAME Result, assert their top-level key sets are identical to each
// other and to the frozen list, assert the destination entries carry
// the frozen 9 keys, and spot-check the encoded values (media count,
// residual, margin echo, mode).
func TestPlanReportJSONSchemaFrozen(t *testing.T) {
	profile := units.Profile{Name: "disk", NominalBytes: 10_000_000_000, UsableBytes: 9_800_000_000}
	res := plan.Result{
		Destinations: []plan.DestFill{
			{ImageID: 1, NominalSize: 10_000_000_000, UsableSize: 9_800_000_000,
				MediaProfile: "disk", UsedBytes: 4_900_000_000, ItemCount: 2, Locked: true},
		},
		NewDestinations:      0,
		FilesAssigned:        1,
		WastedResidualBytes:  4_900_000_000,
		WastedResidualImages: 1,
	}

	marshal := func(t *testing.T, mode planReportMode) map[string]any {
		t.Helper()
		var buf bytes.Buffer
		if err := writePlanReport(&buf, buildPlanReport(mode, "S-J", profile, res), true); err != nil {
			t.Fatalf("writePlanReport --json: %v", err)
		}
		return decodeSingleJSONDoc(t, buf.String())
	}

	planDoc := marshal(t, modePlan)
	reportDoc := marshal(t, modeReport)

	wantKeys := append([]string(nil), planReportJSONKeys...)
	sort.Strings(wantKeys)
	if got := sortedKeys(planDoc); !sameStrings(got, wantKeys) {
		t.Errorf("plan --json key set = %v\nwant exactly %v (schema freeze)", got, wantKeys)
	}
	if got := sortedKeys(reportDoc); !sameStrings(got, wantKeys) {
		t.Errorf("report --json key set = %v\nwant exactly %v (schema freeze)", got, wantKeys)
	}

	// Per-destination frozen keys.
	dests, ok := planDoc["destinations"].([]any)
	if !ok || len(dests) != 1 {
		t.Fatalf("plan --json destinations = %v, want one entry", planDoc["destinations"])
	}
	dest, ok := dests[0].(map[string]any)
	if !ok {
		t.Fatalf("destination entry has type %T, want object", dests[0])
	}
	wantDestKeys := append([]string(nil), destFillJSONKeys...)
	sort.Strings(wantDestKeys)
	if got := sortedKeys(dest); !sameStrings(got, wantDestKeys) {
		t.Errorf("destination key set = %v\nwant exactly %v (schema freeze)", got, wantDestKeys)
	}

	// Values: same numbers in both modes; only "mode" differs.
	if planDoc["mode"] != "plan" || reportDoc["mode"] != "report" {
		t.Errorf("modes = %v / %v, want plan / report", planDoc["mode"], reportDoc["mode"])
	}
	for _, doc := range []map[string]any{planDoc, reportDoc} {
		if doc["set_id"] != "S-J" || doc["media_profile"] != "disk" {
			t.Errorf("identity = %v/%v, want S-J/disk", doc["set_id"], doc["media_profile"])
		}
		if doc["media_count"].(float64) != 1 || doc["new_destinations"].(float64) != 0 {
			t.Errorf("media counts = %v/%v, want 1/0", doc["media_count"], doc["new_destinations"])
		}
		if doc["nominal_size"].(float64) != 10_000_000_000 || doc["usable_size"].(float64) != 9_800_000_000 {
			t.Errorf("profile echo = %v/%v, want 10000000000/9800000000",
				doc["nominal_size"], doc["usable_size"])
		}
		if doc["margin_percent"].(float64) != 2 {
			t.Errorf("margin_percent = %v, want 2 (D-06 echo)", doc["margin_percent"])
		}
		if doc["residual_bytes"].(float64) != 4_900_000_000 || doc["residual_images"].(float64) != 1 {
			t.Errorf("residual = %v across %v, want 4900000000 across 1",
				doc["residual_bytes"], doc["residual_images"])
		}
		if doc["nothing_to_plan"] != false {
			t.Errorf("nothing_to_plan = %v, want false (files were assigned)", doc["nothing_to_plan"])
		}
	}
	// The single destination entry's numbers.
	if dest["image"].(float64) != 1 || dest["used_bytes"].(float64) != 4_900_000_000 ||
		dest["fill_percent"].(float64) != 50 || dest["locked"] != true {
		t.Errorf("destination = %v, want image 1, 4.9e9 used, 50%%, locked", dest)
	}
}

// --- model fold (pure buildPlanReport behavior) -------------------------------

// TestBuildPlanReportFold pins the model fold: fill percentages
// truncate to integers (the 9.3 percent rule), the nothing-to-plan
// predicate covers files/chunks/symlinks/dirs, destinations keep the
// caller's order (the balancer emits ascending image_id, and the
// renderer must not re-shuffle it), and an empty result folds into an
// empty (non-nil) destinations array so the JSON key is always present.
func TestBuildPlanReportFold(t *testing.T) {
	profile := units.Profile{Name: "disk", NominalBytes: 1_000, UsableBytes: 980}

	rep := buildPlanReport(modePlan, "S-FOLD", profile, plan.Result{
		Destinations: []plan.DestFill{
			{ImageID: 2, NominalSize: 1_000, UsableSize: 980, MediaProfile: "disk", UsedBytes: 99},
			{ImageID: 1, NominalSize: 1_000, UsableSize: 980, MediaProfile: "disk", UsedBytes: 98},
		},
		SymlinksAssigned: 1, // a symlink-only plan is still WORK (not nothing-to-plan)
	})
	if rep.NothingToPlan {
		t.Error("nothing_to_plan = true for a symlink assignment; want false")
	}
	if rep.MediaCount != 2 {
		t.Errorf("media_count = %d, want 2", rep.MediaCount)
	}
	// 99*100/980 = 10 (truncated), 98*100/980 = 10 exactly.
	if rep.Destinations[0].Image != 2 || rep.Destinations[1].Image != 1 {
		t.Errorf("destination order = %d,%d, want the caller order preserved (2,1)",
			rep.Destinations[0].Image, rep.Destinations[1].Image)
	}
	if rep.Destinations[0].FillPercent != 10 || rep.Destinations[1].FillPercent != 10 {
		t.Errorf("fill percents = %d,%d, want 10,10 (integer truncation)",
			rep.Destinations[0].FillPercent, rep.Destinations[1].FillPercent)
	}
	if rep.MarginPercent != 2 {
		t.Errorf("margin_percent = %d, want 2 ((1000-980)*100/1000)", rep.MarginPercent)
	}

	// Zero-everything result: nothing to plan, empty array (never null).
	empty := buildPlanReport(modeReport, "S-EMPTY", profile, plan.Result{})
	if !empty.NothingToPlan {
		t.Error("nothing_to_plan = false for a zero result; want true")
	}
	var buf bytes.Buffer
	if err := writePlanReport(&buf, empty, true); err != nil {
		t.Fatalf("writePlanReport --json: %v", err)
	}
	if !strings.Contains(buf.String(), `"destinations": []`) {
		t.Errorf("empty report must encode an empty destinations ARRAY (frozen schema), got:\n%s", buf.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("empty report is not valid JSON: %v", err)
	}
}
