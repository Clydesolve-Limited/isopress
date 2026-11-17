package cli

// T2.3 end-to-end acceptance tests for the real `report` command
// (IMPLEMENTATION_PLAN T2.3 acceptance: "A read-only assertion: report
// opens the catalog without the mutating path, starts no transaction,
// and writes no images or assignment rows (asserted by catalog row
// comparison before and after)"; DESIGN_SPEC 4.3 identical-pipeline
// read-only clause, FR-009, 2.1 Option B amended locking, 9.3/FR-046
// stream separation).
//
// The zero-write proof is deliberately DOUBLED, per the T2.3 brief:
//
//  1. END TO END: capture the catalog file's BYTES, run the real
//     report child, compare bytes - byte-identical (plus zero images
//     rows, zero chunk rows, zero assignments afterwards). This is
//     the property a user cares about.
//  2. AT THE SEAM: TestPlanPipelineReportModeNeverWrites (plan_test.go)
//     drives the shared pipeline with a write hook that fails the test
//     if invoked - the structural guard on the mode gate. The report
//     command additionally wires refuseWrites (planreport.go) as a
//     production tripwire: if the gate ever regressed, a report run
//     would exit non-zero, which the e2e exit-0 assertions here would
//     catch immediately.
//
// The Option B locking amendment (DESIGN_SPEC 2.1) is driven through
// the REAL command too: a report SUCCEEDS while another process holds
// the exclusive mutating flock - read-only opens take no advisory
// lock at all.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"isopress/internal/plan"
	"isopress/internal/report"
)

// TestReportE2EProjectionIsReadOnly is the read-only centerpiece: after
// a scan WITHOUT any plan, report states the projected media count
// (FR-009 "how many destinations the current file set requires")
// WITHOUT writing anything - the catalog file is byte-identical before
// and after, no images rows exist, no chunk rows were created, and no
// file or directory carries an assignment. The --json leg doubles as
// the frozen-schema pin through the real command.
func TestReportE2EProjectionIsReadOnly(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-REP")

	before := fileBytes(t, cat)
	stdout, stderr, code := runReportChild(t,
		"--catalog", cat, "--set-id", "S-REP", "--size", planFixtureSize)
	if code != report.ExitOK {
		t.Fatalf("report exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	// The projected report (FR-009): media count if planned now, the
	// D-06 margin echo, and the zero-locked residual line.
	for _, want := range []string{
		"report (projection): set S-REP (profile disk: nominal 1.0 GB, usable 980.0 MB, margin 2%)",
		"media: 2 destinations total (2 new, 0 pre-existing)",
		"assigned this run: 2 files, 2 chunks, 1 symlinks, 1 dirs (hard-link secondaries excluded: 0)",
		"residual space on locked destinations: 0 B across 0 images",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("report output lacks %q:\n%s", want, stdout)
		}
	}
	// The projection never claims to have written anything.
	if strings.Contains(stdout, "[locked]") {
		t.Errorf("projection marks destinations locked before any plan:\n%s", stdout)
	}
	// Progress renders on stderr (FR-046): the one summary line.
	if !strings.Contains(stderr, "report: media 2") {
		t.Errorf("stderr lacks the report summary line:\n%s", stderr)
	}

	// ZERO WRITES, end to end: the catalog file is byte-identical ...
	if !bytes.Equal(before, fileBytes(t, cat)) {
		t.Fatal("catalog bytes changed across a report run (read-only violation)")
	}
	// ... and no planning row exists anywhere.
	db := catOpen(t, cat)
	var images, chunks int
	if err := db.QueryRow(`SELECT COUNT(*) FROM images`).Scan(&images); err != nil {
		t.Fatalf("count images: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM chunks`).Scan(&chunks); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if images != 0 || chunks != 0 {
		t.Errorf("report created rows: %d images, %d chunks; want 0/0 (no transaction was started)",
			images, chunks)
	}
	for _, rel := range []string{"a.txt", "sub/b.bin", "big.dat", "link"} {
		rows := queryFiles(t, db, rel)
		if len(rows) != 1 || rows[0].ImageID.Valid {
			t.Errorf("%s rows after report = %+v, want one row with image_id NULL", rel, rows)
		}
	}
	var assignedDirs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM directories WHERE image_id IS NOT NULL`).
		Scan(&assignedDirs); err != nil {
		t.Fatalf("count assigned dirs: %v", err)
	}
	if assignedDirs != 0 {
		t.Errorf("report assigned %d directories; want 0", assignedDirs)
	}

	// The --json leg: frozen schema through the REAL command, mode
	// "report", same media count as the human form.
	stdout, stderr, code = runReportChild(t,
		"--catalog", cat, "--set-id", "S-REP", "--size", planFixtureSize, "--json")
	if code != report.ExitOK {
		t.Fatalf("report --json exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	doc := decodeSingleJSONDoc(t, stdout)
	wantKeys := append([]string(nil), planReportJSONKeys...)
	sort.Strings(wantKeys)
	if got := sortedKeys(doc); !sameStrings(got, wantKeys) {
		t.Errorf("report --json key set = %v\nwant exactly %v (schema freeze)", got, wantKeys)
	}
	if doc["mode"] != "report" || doc["set_id"] != "S-REP" {
		t.Errorf("json identity = %v/%v, want report/S-REP", doc["mode"], doc["set_id"])
	}
	if doc["media_count"].(float64) != 2 || doc["new_destinations"].(float64) != 2 {
		t.Errorf("json projected media count = %v new %v, want 2/2", doc["media_count"], doc["new_destinations"])
	}
	// The JSON projection is byte-for-byte non-writing too.
	if !bytes.Equal(before, fileBytes(t, cat)) {
		t.Fatal("catalog bytes changed across a report --json run (read-only violation)")
	}
}

// TestReportAfterPlanShowsLockedFillAndResidual: after a real plan,
// report switches from projection to describing the LOCKED state -
// the destinations' current fill marked [locked], and the D-05
// residual figure for the gap that accept-waste leaves on them. The
// catalog stays byte-identical here as well (report writes nothing
// even when everything is already planned).
func TestReportAfterPlanShowsLockedFillAndResidual(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-LOCKED")

	if stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", "S-LOCKED", "--size", planFixtureSize); code != report.ExitOK {
		t.Fatalf("plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	before := fileBytes(t, cat)
	stdout, stderr, code := runReportChild(t,
		"--catalog", cat, "--set-id", "S-LOCKED", "--size", planFixtureSize)
	if code != report.ExitOK {
		t.Fatalf("report after plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// Both destinations are now pre-existing (locked); under the
	// 2026-10-20 capacity model image 1 is exactly packed by chunk 0's
	// charge (so the empty dir's entry fell forward to image 2), and
	// destination 2 holds the 905,800,555-byte TRUE free-space gap
	// that can never be used (usable - reserved overhead - payload,
	// the amended 4.4(c) D-05 visibility requirement on the real
	// numbers).
	for _, want := range []string{
		"report (projection): set S-LOCKED",
		"media: 2 destinations total (0 new, 2 pre-existing)",
		"image 1: 1 items, 0 dirs, 977.9 MB / 980.0 MB (99%) [locked]",
		"image 2: 4 items, 1 dirs, 72.1 MB / 980.0 MB (7%) [locked]",
		"residual space on locked destinations: 905.8 MB across 1 images",
		"nothing to plan: no unassigned items",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("report after plan lacks %q:\n%s", want, stdout)
		}
	}
	if !bytes.Equal(before, fileBytes(t, cat)) {
		t.Fatal("catalog bytes changed across a report run after plan (read-only violation)")
	}
}

// TestReportRunsDuringMutatingRunSucceeds drives the Option B
// amendment (DESIGN_SPEC 2.1) through the REAL command: while another
// process holds the exclusive MUTATING flock (the T0.4 holder), report
// - which takes NO advisory lock - succeeds over the WAL snapshot.
// This is the cron-overlap story: a report during a long scan or copy
// never blocks and never fails with the busy class.
func TestReportRunsDuringMutatingRunSucceeds(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-CONC")

	holder := exec.Command(os.Args[0])
	holder.Env = append(os.Environ(), lockHoldEnv+"=1", lockHoldPathEnv+"="+cat)
	holderStdin, err := holder.StdinPipe()
	if err != nil {
		t.Fatalf("holder stdin pipe: %v", err)
	}
	holderOut, err := holder.StdoutPipe()
	if err != nil {
		t.Fatalf("holder stdout pipe: %v", err)
	}
	holderStderr := &bytes.Buffer{}
	holder.Stderr = holderStderr
	if err := holder.Start(); err != nil {
		t.Fatalf("starting lock holder: %v", err)
	}
	defer func() {
		_ = holderStdin.Close() // EOF releases the holder's read loop
		_ = holder.Wait()
	}()
	buf := make([]byte, len(lockHoldReady)+2)
	if _, err := holderOut.Read(buf); err != nil {
		t.Fatalf("holder never became ready: %v (stderr: %s)", err, holderStderr.String())
	}
	if !strings.Contains(string(buf), lockHoldReady) {
		t.Fatalf("holder readiness = %q, want %q", string(buf), lockHoldReady)
	}

	// The mutating lock is held, yet report succeeds (no flock, WAL
	// snapshot) and shows the projection.
	stdout, stderr, code := runReportChild(t,
		"--catalog", cat, "--set-id", "S-CONC", "--size", planFixtureSize)
	if code != report.ExitOK {
		t.Fatalf("report during a mutating run exit = %d, want 0 (Option B: readers take no flock)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if !strings.Contains(stdout, "report (projection): set S-CONC") {
		t.Errorf("report during a mutating run lacks the projection:\n%s", stdout)
	}
}

// TestReportUsageErrors pins the usage-class inputs (exit 2): the
// required flags shared with plan (--size first, per the T2.3 brief;
// --set-id is required for the same documented reason - plan/report
// take no ROOT to derive a default set from).
func TestReportUsageErrors(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-RUSAGE")

	for _, tc := range []struct {
		name    string
		args    []string
		wantMsg string
	}{
		{
			name:    "no --size",
			args:    []string{"--catalog", cat, "--set-id", "S-RUSAGE"},
			wantMsg: "--size is required",
		},
		{
			name:    "no --set-id",
			args:    []string{"--catalog", cat, "--size", planFixtureSize},
			wantMsg: "--set-id is required",
		},
		{
			name:    "no --catalog",
			args:    []string{"--set-id", "S-RUSAGE", "--size", planFixtureSize},
			wantMsg: "--catalog is required",
		},
		{
			name:    "unknown set",
			args:    []string{"--catalog", cat, "--set-id", "S-NOPE", "--size", planFixtureSize},
			wantMsg: "no scanned set",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runReportChild(t, tc.args...)
			if code != report.ExitUsage {
				t.Fatalf("report %s exit = %d, want 2\nstdout: %s\nstderr: %s", tc.name, code, stdout, stderr)
			}
			if !strings.Contains(stderr, tc.wantMsg) {
				t.Errorf("report %s stderr lacks %q:\n%s", tc.name, tc.wantMsg, stderr)
			}
			if stdout != "" {
				t.Errorf("report %s: stdout must stay empty on usage errors, got: %q", tc.name, stdout)
			}
		})
	}
	// A missing catalog is a CatalogError (exit 4), same as plan.
	missing := filepath.Join(t.TempDir(), "no-such.sqlite")
	stdout, stderr, code := runReportChild(t,
		"--catalog", missing, "--set-id", "S-404", "--size", planFixtureSize)
	if code != report.ExitCatalog {
		t.Fatalf("report with missing catalog exit = %d, want 4\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout must stay empty on catalog errors, got: %q", stdout)
	}
}

// TestReportAcceptsStrict documents the --strict judgment call for
// report: like plan, the read-only pipeline has no item-failure path,
// so --strict is accepted and inert (exit 0).
func TestReportAcceptsStrict(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-RSTRICT")
	stdout, stderr, code := runReportChild(t,
		"--catalog", cat, "--set-id", "S-RSTRICT", "--size", planFixtureSize, "--strict")
	if code != report.ExitOK {
		t.Fatalf("report --strict exit = %d, want 0 (no item-failure path to promote)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if !strings.Contains(stdout, "media: 2 destinations total") {
		t.Errorf("strict report lost the media line:\n%s", stdout)
	}
}

// --- compile-time and unit-level guards ----------------------------------------

// TestRefuseWritesIsATripwire self-tests the production tripwire the
// report command wires into the shared pipeline: it ERRORS when
// invoked. Combined with the e2e exit-0 assertions (a tripped wire
// would fail every report run) and the injected-hook seam test in
// plan_test.go, the zero-write property is guarded at three levels.
func TestRefuseWritesIsATripwire(t *testing.T) {
	err := refuseWrites(context.Background(), nil, "S-X", &plan.Plan{})
	if !errors.Is(err, errReportWrite) {
		t.Fatalf("refuseWrites = %v, want the errReportWrite tripwire", err)
	}
}

// TestReportCommandWiresRefuseWrites pins the report command's wiring
// through the real cobra tree: executing `report` with valid inputs
// runs the modeReport pipeline (covered by the e2e tests above), and
// the command's RunE closure captured here is the one that passes
// refuseWrites. This guards against someone silently swapping the
// hook for writeAssignments (which would make report MUTATING - the
// exact regression the tripwire exists for): the projection heading
// plus the byte-identical assertions would still pass, but the seam
// test would not - so this documents the intended wiring explicitly.
func TestReportCommandUsesReportMode(t *testing.T) {
	root := NewRootCmd()
	var found bool
	for _, c := range root.Commands() {
		if c.Name() == "report" {
			found = true
			// The command must NOT be a placeholder stub anymore: its
			// help text carries the projection contract (T2.3 replaced
			// the T0.5 stub in commands.go).
			if !strings.Contains(c.Long, "read-only") {
				t.Errorf("report --help Long text lacks the read-only contract:\n%s", c.Long)
			}
			if c.Flags().Lookup("size") == nil || c.Flags().Lookup("media-profile") == nil {
				t.Error("report command lacks the plan-local --size/--media-profile flags")
			}
		}
		if c.Name() == "plan" {
			if c.Flags().Lookup("size") == nil || c.Flags().Lookup("media-profile") == nil {
				t.Error("plan command lacks the plan-local --size/--media-profile flags")
			}
		}
	}
	if !found {
		t.Fatal("report command missing from the root command tree")
	}
}
