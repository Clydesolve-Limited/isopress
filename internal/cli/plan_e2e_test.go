//go:build e2e

package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"isopress/internal/catalog"
	"isopress/internal/plan"
	"isopress/internal/report"
	"isopress/internal/units"
)

// TestPlanE2EIncrementalLockedAndResidual pins the incremental half of
// the design through the real commands: after a first plan, a newly
// scanned file only ever opens a NEW destination (FR-029; the locked
// ones receive nothing, FR-011), the report marks them "[locked]", and
// the D-05 residual line quantifies the permanent gap on a LOCKED
// destination ("residual space on locked destinations: X across N
// images", 4.4) - the visibility cost of accept-waste.
func TestPlanE2EIncrementalLockedAndResidual(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-INC")

	firstOut, firstErr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", "S-INC", "--size", planFixtureSize)
	if code != report.ExitOK {
		t.Fatalf("first plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, firstOut, firstErr)
	}
	// The HUMAN report through the real command pins the D-06 margin
	// echo, the media count, the fill-percentage format (payload /
	// usable_size - honest disc usage under the reservation), the
	// separate reserved-overhead line, and the zero-locked residual
	// line (the renderer is golden-pinned separately on models; these
	// assert the real pipeline's numbers).
	for _, want := range []string{
		"plan: set S-INC (profile disk: nominal 1.0 GB, usable 980.0 MB, margin 2%)",
		"media: 2 destinations total (2 new, 0 pre-existing)",
		"image 1: 1 items, 0 dirs, 977.9 MB / 980.0 MB (99%)",
		"reserved overhead: " + units.FormatBytes(units.FixedImageOverhead+units.ImageEntryOverhead) +
			" (fixed " + units.FormatBytes(units.FixedImageOverhead) + " + 1 entries), free after build: 0 B",
		"residual space on locked destinations: 0 B across 0 images",
		"assigned this run: 2 files, 2 chunks, 1 symlinks, 1 dirs (hard-link secondaries excluded: 0)",
	} {
		if !strings.Contains(firstOut, want) {
			t.Errorf("first plan report lacks %q:\n%s", want, firstOut)
		}
	}

	// New data: one more small file, rescanned into the same set.
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("brand new\n"), 0o644); err != nil {
		t.Fatalf("add new.txt: %v", err)
	}
	if stdout, stderr, code := runScanChild(t, "--catalog", cat, "--set-id", "S-INC", root); code != report.ExitOK {
		t.Fatalf("rescan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", "S-INC", "--size", planFixtureSize)
	if code != report.ExitOK {
		t.Fatalf("incremental plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	// Media count grew by exactly ONE destination; the other two are
	// pre-existing (locked).
	if !strings.Contains(stdout, "media: 3 destinations total (1 new, 2 pre-existing)") {
		t.Errorf("incremental report lacks the 3-destination media line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "[locked]") {
		t.Errorf("incremental report lacks the [locked] marker:\n%s", stdout)
	}
	// The D-05 residual under the amended formula (4.4c, the TRUE free
	// space of the built image): destination 1 is exactly full
	// (payload + fixed overhead + its one entry = usable -> 0
	// residual); destination 2 holds 72,098,709 payload bytes with 5
	// entries, leaving 980,000,000 - 2,098,176 - 5*512 - 72,098,709 =
	// 905,800,555 bytes ("905.8 MB") that can NEVER be used - across
	// exactly 1 locked image.
	if !strings.Contains(stdout,
		"residual space on locked destinations: 905.8 MB across 1 images") {
		t.Errorf("incremental report lacks the exact D-05 residual line:\n%s", stdout)
	}

	db := catOpen(t, cat)
	// new.txt landed on the NEW destination (3), not on a locked one.
	nw := queryFiles(t, db, "new.txt")
	if len(nw) != 1 || !nw[0].ImageID.Valid || nw[0].ImageID.Int64 != 3 {
		t.Errorf("new.txt rows = %+v, want one row assigned to new destination 3", nw)
	}
	// The locked destinations' historical fill is untouched.
	fills := queryDestFill(t, db, "S-INC")
	if f := fills[1]; f.usedBytes != planFixtureChunk0 {
		t.Errorf("locked destination 1 fill = %d, want %d (untouched)", f.usedBytes, planFixtureChunk0)
	}
	// Locked assignments are bit-identical: the original file rows
	// still point at their first destinations.
	for _, tc := range []struct {
		rel     string
		imageID int64
	}{
		{"a.txt", 2}, {"sub/b.bin", 2}, {"link", 2},
	} {
		rows := queryFiles(t, db, tc.rel)
		if len(rows) != 1 || !rows[0].ImageID.Valid || rows[0].ImageID.Int64 != tc.imageID {
			t.Errorf("%s rows = %+v, want the locked assignment to destination %d", tc.rel, rows, tc.imageID)
		}
	}
}

// --- usage and error paths (DESIGN_SPEC 10.1/10.2) ------------------------------

// TestPlanUsageErrors pins the usage-class inputs (exit 2): the three
// required flags (the T2.3 brief's missing --size case first), a stray
// positional argument through the usageArgs wrap, and the bad-value
// paths (garbled size, unknown profile, fixed-profile size mismatch,
// unknown set id).
func TestPlanUsageErrors(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-USAGE")

	for _, tc := range []struct {
		name    string
		args    []string
		wantMsg string
	}{
		{
			name:    "no --size",
			args:    []string{"--catalog", cat, "--set-id", "S-USAGE"},
			wantMsg: "--size is required",
		},
		{
			name:    "no --set-id",
			args:    []string{"--catalog", cat, "--size", planFixtureSize},
			wantMsg: "--set-id is required",
		},
		{
			name:    "no --catalog",
			args:    []string{"--set-id", "S-USAGE", "--size", planFixtureSize},
			wantMsg: "--catalog is required",
		},
		{
			name:    "stray positional argument",
			args:    []string{"--catalog", cat, "--set-id", "S-USAGE", "--size", planFixtureSize, "oops"},
			wantMsg: "unknown command",
		},
		{
			name:    "garbled size",
			args:    []string{"--catalog", cat, "--set-id", "S-USAGE", "--size", "banana"},
			wantMsg: "invalid size",
		},
		{
			name:    "unknown profile",
			args:    []string{"--catalog", cat, "--set-id", "S-USAGE", "--size", "25", "--media-profile", "tape"},
			wantMsg: "unknown media profile",
		},
		{
			name: "fixed-profile size mismatch",
			args: []string{"--catalog", cat, "--set-id", "S-USAGE",
				"--size", "128", "--media-profile", "bd-r-sl"},
			wantMsg: "fixed nominal size",
		},
		{
			name:    "unknown set",
			args:    []string{"--catalog", cat, "--set-id", "S-NOPE", "--size", planFixtureSize},
			wantMsg: "no scanned set",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runPlanChild(t, tc.args...)
			if code != report.ExitUsage {
				t.Fatalf("plan %s exit = %d, want 2\nstdout: %s\nstderr: %s", tc.name, code, stdout, stderr)
			}
			if !strings.Contains(stderr, tc.wantMsg) {
				t.Errorf("plan %s stderr lacks %q:\n%s", tc.name, tc.wantMsg, stderr)
			}
			if stdout != "" {
				t.Errorf("plan %s: stdout must stay empty on usage errors, got: %q", tc.name, stdout)
			}
		})
	}
}

// TestPlanStrictIsAccepted documents the --strict judgment call for
// this command: the plan pipeline has NO per-item failure path (Balance
// errors are fatal-class by construction EXCEPT ErrPartialChunkReplan,
// which is usage-class exit 2 - see plan.go's file doc - and the write
// is one atomic transaction), so --strict has nothing to promote and
// stays inert - a successful plan exits 0 with the flag set.
func TestPlanStrictIsAccepted(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-STRICT")
	stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", "S-STRICT", "--size", planFixtureSize, "--strict")
	if code != report.ExitOK {
		t.Fatalf("plan --strict exit = %d, want 0 (no item-failure path to promote)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if !strings.Contains(stdout, "media: 2 destinations total") {
		t.Errorf("strict plan report lost the media line:\n%s", stdout)
	}
}

// --- in-process seam: the shared pipeline's mode gate ---------------------------

// TestPlanPipelineReportModeNeverWrites drives the SHARED pipeline
// directly (runPlanReport) in report mode with a write hook that fails
// the test if invoked - the T2.3 brief's zero-write factory proof at
// the seam level. If the mode gate in planreport.go were removed or
// inverted, this test fails at the hook; the byte-identical catalog
// comparison in reportcmd_test.go proves the same property end to end.
func TestPlanPipelineReportModeNeverWrites(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-SEAM")

	opts := &rootOptions{catalog: cat, setID: "S-SEAM"}
	po := planOptions{size: planFixtureSize, mediaProfile: units.DefaultProfile}
	cmd := &cobra.Command{Use: "report"}
	outBuf, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(outBuf)
	cmd.SetErr(errBuf)

	before := fileBytes(t, cat)
	err := runPlanReport(context.Background(), cmd, opts, po, modeReport,
		func(context.Context, *catalog.DB, string, *plan.Plan) error {
			t.Error("report mode invoked the assignment write hook (zero-write violation)")
			return errors.New("write hook invoked")
		})
	if err != nil {
		t.Fatalf("runPlanReport (report mode) = %v", err)
	}
	if !strings.Contains(outBuf.String(), "report (projection): set S-SEAM") {
		t.Errorf("report output lacks the projection heading:\n%s", outBuf.String())
	}
	if !bytes.Equal(before, fileBytes(t, cat)) {
		t.Error("catalog bytes changed across a report-mode pipeline run (zero-write violation)")
	}
}
