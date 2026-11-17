package cli

// T2.3 verification-pass gap closures (test-only; no production change).
// These tests close the audit gaps the T2.3 acceptance pass identified
// on top of plan_test.go, reportcmd_test.go, and planreport_test.go:
//
//   - --size parsing driven through the REAL plan command: bare "25"
//     resolves as decimal GB (25e9), "25Gi" as binary (25*2^30), bad
//     sizes exit 2, and the fixed-profile mismatch case the brief names
//     (--size 26 --media-profile bd-r-sl) is asserted through the
//     command, not only at the units layer.
//   - The FR-009 projection-vs-reality round trip end to end: report
//     says N media BEFORE any plan exists, the subsequent real plan
//     creates EXACTLY N images rows, and every projected per-destination
//     number equals the written one.
//   - Plan resumability: run 1 assigns 10 files; a scan adds 5 more;
//     run 2 assigns ONLY the 5 new ones - every pre-existing assignment
//     line (files, chunks, dirs, images rows) is byte-identical.
//   - Two real `plan` runs racing for the flock: exactly one exits 5
//     (the contention refusal through the real mutating command, not
//     only through the lock-holder helper).
//   - Stream separation in BOTH output modes of BOTH commands: --json
//     stdout is exactly one document (nothing else on stdout), and the
//     progress summary lands on stderr only - never the report body on
//     stderr, never the summary on stdout.
//   - The production refuseWrites tripwire is genuinely unreachable in
//     report mode: the REAL command wiring (modeReport + refuseWrites)
//     completes with a projection and never trips the wire; a direct
//     call is proven to fire in TestRefuseWritesIsATripwire.
//   - The folded-v1 repair (missing directories table, pre-T2.2
//     catalog) heals on PLAN's mutating open - the second healing
//     command beyond scan - while the read-only report fails loudly and
//     repairs nothing.
//   - An oversized file big enough for THREE chunks lands on >= 2 (here
//     exactly 3) distinct destinations, and both report forms show the
//     chunk counts.

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"isopress/internal/report"
)

// --- helpers -----------------------------------------------------------------

// makeTinyPlanFixture is the fast-scan fixture for flag-parsing tests:
// two small files, a symlink, and an empty directory - no oversized
// file, so scans stay cheap and plans fit one destination.
func makeTinyPlanFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range []struct{ rel, content string }{
		{"a.txt", "hello a\n"},
		{"b.bin", "binary-b"},
	} {
		if err := os.WriteFile(filepath.Join(root, f.rel), []byte(f.content), 0o644); err != nil {
			t.Fatalf("write %s: %v", f.rel, err)
		}
	}
	if err := os.Symlink("a.txt", filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatalf("mkdir empty: %v", err)
	}
	return root
}

// makeManySmallFilesFixture builds n small files of sizes 100+i%100
// bytes - distinct for n <= 100, which is what the resumability test's
// deterministic worst-fit-decreasing placement and byte-exact fill
// assertions rely on. The 300-file concurrency fixture needs no size
// determinism (it only widens the flock window).
func makeManySmallFilesFixture(t *testing.T, n int, prefix string) string {
	t.Helper()
	root := t.TempDir()
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("%s-%03d.dat", prefix, i)
		if err := os.WriteFile(filepath.Join(root, name),
			[]byte(strings.Repeat("x", 100+i%100)), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return root
}

// --- 1. --size parsing through the REAL plan command --------------------------

// TestPlanSizeParsingThroughRealCommand pins the DESIGN_SPEC 4.1 size
// semantics on the actual command path (flag -> resolvePlanOptions ->
// units.ParseSize -> units.ResolveProfile), not only at the units layer:
// a bare number means decimal GB, a Gi suffix is binary, and a fixed
// profile's nominal must match EXACTLY - the brief's --size 26 against
// bd-r-sl (fixed at 25 GB) is a usage error, exit 2.
func TestPlanSizeParsingThroughRealCommand(t *testing.T) {
	for _, tc := range []struct {
		name           string
		size           string
		profile        string
		wantNominal    int64
		wantUsable     int64
		wantMarginPct  int64
		wantMediaCount int64
	}{
		{
			name:           "bare number means decimal GB (25 -> 25e9)",
			size:           "25",
			profile:        "bd-r-sl",
			wantNominal:    25_000_000_000,
			wantUsable:     23_000_000_000,
			wantMarginPct:  8,
			wantMediaCount: 1, // tiny fixture fits one 23 GB destination
		},
		{
			name:           "Gi suffix is binary (25Gi -> 25*2^30)",
			size:           "25Gi",
			profile:        "disk",
			wantNominal:    26_843_545_600,
			wantUsable:     26_306_674_688, // 98% of 25 GiB
			wantMarginPct:  2,
			wantMediaCount: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := makeTinyPlanFixture(t)
			cat := createCatalog(t)
			scanPlanFixture(t, root, cat, "S-SIZE")

			stdout, stderr, code := runPlanChild(t,
				"--catalog", cat, "--set-id", "S-SIZE",
				"--size", tc.size, "--media-profile", tc.profile, "--json")
			if code != report.ExitOK {
				t.Fatalf("plan --size %s exit = %d, want 0\nstdout: %s\nstderr: %s",
					tc.size, code, stdout, stderr)
			}
			doc := decodeSingleJSONDoc(t, stdout)
			if got := int64(doc["nominal_size"].(float64)); got != tc.wantNominal {
				t.Errorf("nominal_size = %d, want %d (--size %s interpretation)",
					got, tc.wantNominal, tc.size)
			}
			if got := int64(doc["usable_size"].(float64)); got != tc.wantUsable {
				t.Errorf("usable_size = %d, want %d (margin applied once, D-06)", got, tc.wantUsable)
			}
			if got := int64(doc["margin_percent"].(float64)); got != tc.wantMarginPct {
				t.Errorf("margin_percent = %d, want %d", got, tc.wantMarginPct)
			}
			if got := int64(doc["media_count"].(float64)); got != tc.wantMediaCount {
				t.Errorf("media_count = %d, want %d", got, tc.wantMediaCount)
			}
			if doc["media_profile"] != tc.profile {
				t.Errorf("media_profile = %v, want %s", doc["media_profile"], tc.profile)
			}
		})
	}

	t.Run("fixed profile rejects a mismatched size (26 vs bd-r-sl 25)", func(t *testing.T) {
		root := makeTinyPlanFixture(t)
		cat := createCatalog(t)
		scanPlanFixture(t, root, cat, "S-SIZEX")
		stdout, stderr, code := runPlanChild(t,
			"--catalog", cat, "--set-id", "S-SIZEX", "--size", "26", "--media-profile", "bd-r-sl")
		if code != report.ExitUsage {
			t.Fatalf("plan --size 26 --media-profile bd-r-sl exit = %d, want 2\nstdout: %s\nstderr: %s",
				code, stdout, stderr)
		}
		if !strings.Contains(stderr, "fixed nominal size") {
			t.Errorf("stderr lacks the fixed-profile mismatch message:\n%s", stderr)
		}
		if stdout != "" {
			t.Errorf("stdout must stay empty on a profile mismatch, got: %q", stdout)
		}
	})

	t.Run("bad sizes are usage errors through the command", func(t *testing.T) {
		root := makeTinyPlanFixture(t)
		cat := createCatalog(t)
		scanPlanFixture(t, root, cat, "S-SIZEBAD")
		for _, bad := range []string{"0", "-3", "25.5", "25TB", "1e3", "Gi"} {
			t.Run(bad, func(t *testing.T) {
				stdout, stderr, code := runPlanChild(t,
					"--catalog", cat, "--set-id", "S-SIZEBAD", "--size", bad)
				if code != report.ExitUsage {
					t.Fatalf("plan --size %q exit = %d, want 2\nstdout: %s\nstderr: %s",
						bad, code, stdout, stderr)
				}
				if !strings.Contains(stderr, "invalid size") {
					t.Errorf("plan --size %q stderr lacks the invalid-size message:\n%s", bad, stderr)
				}
				if stdout != "" {
					t.Errorf("plan --size %q: stdout must stay empty on usage errors, got %q", bad, stdout)
				}
			})
		}
	})
}

// --- 2. FR-009 projection-vs-reality round trip --------------------------------

// snapshotAssignments serializes EVERY planning assignment of the set -
// files, chunks, directories, and the images rows themselves - into
// canonical lines, so two snapshots can be compared for byte identity.
// Each shape's %v count must match the query's column count.
func snapshotAssignments(t *testing.T, db *sql.DB, setID string) []string {
	t.Helper()
	var out []string
	collect := func(query, shape string, args ...any) {
		t.Helper()
		rows, err := db.Query(query, args...)
		if err != nil {
			t.Fatalf("snapshot query %q: %v", shape, err)
		}
		defer rows.Close()
		for rows.Next() {
			vals := make([]any, strings.Count(shape, "%"))
			ptrs := make([]any, len(vals))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("snapshot scan %q: %v", shape, err)
			}
			parts := make([]any, len(vals))
			for i, v := range vals {
				parts[i] = fmt.Sprintf("%v", v)
			}
			out = append(out, fmt.Sprintf(shape, parts...))
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("snapshot iterate %q: %v", shape, err)
		}
	}
	collect(`SELECT file_id, image_id FROM files
	         WHERE set_id = ? AND image_id IS NOT NULL ORDER BY file_id`,
		"file:%v->%v", setID)
	collect(`SELECT file_id, chunk_index, image_id FROM chunks
	         WHERE set_id = ? AND image_id IS NOT NULL ORDER BY file_id, chunk_index`,
		"chunk:%v#%v->%v", setID)
	collect(`SELECT dir_id, image_id FROM directories
	         WHERE set_id = ? AND image_id IS NOT NULL ORDER BY dir_id`,
		"dir:%v->%v", setID)
	collect(`SELECT image_id, nominal_size, usable_size, media_profile, state FROM images
	         WHERE set_id = ? ORDER BY image_id`,
		"img:%v:%v:%v:%v:%v", setID)
	return out
}

// TestPlanIncrementalLeavesLockedAssignmentsByteIdentical pins plan
// resumability through the real commands: run 1 assigns 10 files; a
// scan adds 5 more; run 2 must assign ONLY the 5 new ones - every
// pre-existing assignment line (all 10 files plus the images row) is
// byte-identical after run 2, and the 5 new lines all name the one NEW
// destination. This is the FR-011/FR-029 lock invariant asserted over
// complete snapshots rather than spot checks.
func TestPlanIncrementalLeavesLockedAssignmentsByteIdentical(t *testing.T) {
	root := makeManySmallFilesFixture(t, 10, "first")
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-RESUME")

	if stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", "S-RESUME", "--size", planFixtureSize); code != report.ExitOK {
		t.Fatalf("plan run 1 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	db := catOpen(t, cat)
	before := snapshotAssignments(t, db, "S-RESUME")
	// 10 file assignments plus exactly one images row (the whole first
	// batch fits one destination: 10 files of 100-109 bytes vs 980 MB).
	if len(before) != 11 {
		t.Fatalf("run 1 snapshot = %d lines (%v), want 10 files + 1 image", len(before), before)
	}

	// New data: 5 more files (sizes 110-114 bytes), rescanned.
	for i := 110; i < 115; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("second-%03d.dat", i)),
			[]byte(strings.Repeat("x", i)), 0o644); err != nil {
			t.Fatalf("add second file %d: %v", i, err)
		}
	}
	if stdout, stderr, code := runScanChild(t, "--catalog", cat, "--set-id", "S-RESUME", root); code != report.ExitOK {
		t.Fatalf("rescan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", "S-RESUME", "--size", planFixtureSize)
	if code != report.ExitOK {
		t.Fatalf("plan run 2 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// The run-2 report states exactly what happened: 1 new destination,
	// the old one locked, and ONLY the 5 new files assigned.
	for _, want := range []string{
		"media: 2 destinations total (1 new, 1 pre-existing)",
		"image 1: 10 items, 0 dirs,", // pre-existing destination, marked locked below
		"[locked]",
		"assigned this run: 5 files, 0 chunks, 0 symlinks, 0 dirs (hard-link secondaries excluded: 0)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("run-2 report lacks %q:\n%s", want, stdout)
		}
	}

	after := snapshotAssignments(t, db, "S-RESUME")
	beforeSet := make(map[string]bool, len(before))
	for _, line := range before {
		beforeSet[line] = true
	}
	afterSet := make(map[string]bool, len(after))
	for _, line := range after {
		afterSet[line] = true
	}

	// BYTE-IDENTICAL: every pre-existing assignment line survives run 2
	// unchanged (a line differing by even one byte would not match).
	for _, line := range before {
		if !afterSet[line] {
			t.Errorf("pre-existing assignment %q changed or vanished across plan run 2 (lock violation)", line)
		}
	}

	// The ONLY new lines: 5 file assignments to destination 2, plus the
	// one new images row.
	var newLines []string
	for _, line := range after {
		if !beforeSet[line] {
			newLines = append(newLines, line)
		}
	}
	sort.Strings(newLines)
	if len(newLines) != 6 {
		t.Fatalf("run 2 added %d assignment lines (%v), want exactly 6 (5 files + 1 image)", len(newLines), newLines)
	}
	newFiles := 0
	for _, line := range newLines {
		switch {
		case strings.HasPrefix(line, "file:"):
			newFiles++
			if !strings.HasSuffix(line, "->2") {
				t.Errorf("new file assignment %q targets a destination other than the new one", line)
			}
		case strings.HasPrefix(line, "img:"):
			if !strings.HasPrefix(line, "img:2:1000000000:980000000:disk:planned") {
				t.Errorf("new images row = %q, want img:2 with the D-06 disk figures, state planned", line)
			}
		default:
			t.Errorf("unexpected new assignment line %q", line)
		}
	}
	if newFiles != 5 {
		t.Errorf("new file assignment lines = %d, want 5", newFiles)
	}

	// The catalog sums agree: destination 1 still holds exactly the
	// first batch (100+...+109 = 1045 bytes), destination 2 the second
	// (110+...+114 = 560 bytes).
	fills := queryDestFill(t, db, "S-RESUME")
	if f := fills[1]; f.usedBytes != 1045 || f.items != 10 {
		t.Errorf("locked destination 1 fill = %+v, want 1045 bytes / 10 items (untouched)", f)
	}
	if f := fills[2]; f.usedBytes != 560 || f.items != 5 {
		t.Errorf("new destination 2 fill = %+v, want 560 bytes / 5 items", f)
	}
}

// --- 4. two real plan runs racing for the flock --------------------------------

// TestPlanConcurrentRunsOneExits5 drives the flock refusal through TWO
// REAL plan commands racing for the same catalog (complementing
// TestPlanLockContentionExit5, which uses the holder helper): exactly
// one run wins and exits 0, the other is refused fast with exit 5. Both
// children run the production entry path, so the mutating open - not a
// test-side lock - is what refuses. Because a race needs overlapping
// lifetimes, the attempt retries with a fresh catalog until a
// collision occurs; a miss (both exit 0) carries no assertion value and
// is retried.
func TestPlanConcurrentRunsOneExits5(t *testing.T) {
	root := makeManySmallFilesFixture(t, 300, "f") // wide flock window
	const setID = "S-RACE"

	waitExit := func(cmd *exec.Cmd) int {
		t.Helper()
		err := cmd.Wait()
		if err == nil {
			return report.ExitOK
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		t.Fatalf("waiting for %v: %v", cmd.Args, err)
		return -1
	}

	const maxAttempts = 10
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		cat := createCatalog(t)
		scanPlanFixture(t, root, cat, setID)

		args := []string{"plan", "--catalog", cat, "--set-id", setID, "--size", planFixtureSize}
		cmdA := exec.Command(os.Args[0], args...)
		cmdB := exec.Command(os.Args[0], args...)
		for _, c := range []*exec.Cmd{cmdA, cmdB} {
			c.Env = append(os.Environ(), cliHelperEnv+"=1")
		}
		outA, errA := &bytes.Buffer{}, &bytes.Buffer{}
		outB, errB := &bytes.Buffer{}, &bytes.Buffer{}
		cmdA.Stdout, cmdA.Stderr = outA, errA
		cmdB.Stdout, cmdB.Stderr = outB, errB
		if err := cmdA.Start(); err != nil {
			t.Fatalf("starting plan child A: %v", err)
		}
		if err := cmdB.Start(); err != nil {
			t.Fatalf("starting plan child B: %v", err)
		}
		codeA, codeB := waitExit(cmdA), waitExit(cmdB)

		codes := []int{codeA, codeB}
		stderrs := []string{errA.String(), errB.String()}
		var busy []int
		var ok []int
		for i, c := range codes {
			switch c {
			case report.ExitCatalogBusy:
				busy = append(busy, i)
			case report.ExitOK:
				ok = append(ok, i)
			default:
				t.Fatalf("attempt %d: child %d exit = %d, want 0 or 5\nstdout: %s\nstderr: %s",
					attempt, i, c, []string{outA.String(), outB.String()}[i], stderrs[i])
			}
		}
		if len(busy) == 1 && len(ok) == 1 {
			// The contention refusal through the real plan command: the
			// loser fails fast with the busy class, the winner completes.
			if !strings.Contains(stderrs[busy[0]], "busy") {
				t.Errorf("refused child stderr lacks the busy message:\n%s", stderrs[busy[0]])
			}
			if !strings.Contains([]string{outA.String(), outB.String()}[ok[0]], "media: 1 destinations total") {
				t.Errorf("winning child report lost the media line:\n%s",
					[]string{outA.String(), outB.String()}[ok[0]])
			}
			// The loser wrote nothing: the catalog still holds exactly
			// the winner's single-destination plan.
			db := catOpen(t, cat)
			var images int
			if err := db.QueryRow(`SELECT COUNT(*) FROM images WHERE set_id = ?`, setID).Scan(&images); err != nil {
				t.Fatalf("count images: %v", err)
			}
			if images != 1 {
				t.Errorf("images rows after the race = %d, want 1 (loser wrote nothing)", images)
			}
			return
		}
		// Both exited 0: the lifetimes did not overlap (a miss). Retry
		// with a fresh catalog so there is real work to contend on.
	}
	t.Fatalf("%d concurrent plan attempts never contended for the flock; "+
		"the test window may be too small on this machine - investigate", maxAttempts)
}

// --- 5. stream separation in both modes of both commands -----------------------

// TestPlanHealsPreT22CatalogMissingDirectories pins the folded-v1
// repair (catalog.ensureFoldedV1Objects, migrate.go) on the PLAN
// command's mutating open - the second healing command beyond scan
// (TestScanHealsPreT22Catalog covers scan). A v1-stamped catalog missing
// the `directories` table predates the T2.2 fold; the version stamp
// already matches, so only the mutating-open repair can heal it. The
// read-only report leg doubles the property from the other side: it
// fails loudly on the missing table and repairs NOTHING (mode=ro cannot
// write), exactly as the repair's design comment requires.
func TestPlanHealsPreT22CatalogMissingDirectories(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-HEAL")

	// Simulate the pre-T2.2 catalog: v1-stamped, directories table
	// absent (its rows go with it - the old binary never recorded them).
	raw := catRW(t, cat)
	if _, err := raw.Exec(`DROP TABLE directories`); err != nil {
		t.Fatalf("simulating pre-T2.2 catalog: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close simulation connection: %v", err)
	}

	objectPresent := func(t *testing.T, db *sql.DB, name string) bool {
		t.Helper()
		var got string
		err := db.QueryRow(`SELECT name FROM sqlite_master WHERE name = ?`, name).Scan(&got)
		if errors.Is(err, sql.ErrNoRows) {
			return false
		}
		if err != nil {
			t.Fatalf("query sqlite_master for %s: %v", name, err)
		}
		return true
	}

	// Leg 1: the read-only report FAILS LOUDLY and repairs nothing.
	before := fileBytes(t, cat)
	stdout, stderr, code := runReportChild(t,
		"--catalog", cat, "--set-id", "S-HEAL", "--size", planFixtureSize)
	if code == report.ExitOK {
		t.Fatalf("report against a pre-T2.2 catalog unexpectedly succeeded:\n%s", stdout)
	}
	if !strings.Contains(stderr, "no such table") || !strings.Contains(stderr, "directories") {
		t.Errorf("report stderr lacks the missing-table failure:\n%s", stderr)
	}
	if !bytes.Equal(before, fileBytes(t, cat)) {
		t.Fatal("the read-only report wrote to a pre-T2.2 catalog (read-only violation)")
	}
	{
		ro := catOpen(t, cat)
		if objectPresent(t, ro, "directories") {
			t.Fatal("the read-only report repaired the directories table (read-only opens must never repair)")
		}
	}

	// Leg 2: the plan command's MUTATING open performs the repair, and
	// the plan itself completes against the healed schema.
	stdout, stderr, code = runPlanChild(t,
		"--catalog", cat, "--set-id", "S-HEAL", "--size", planFixtureSize)
	if code != report.ExitOK {
		t.Fatalf("plan against a pre-T2.2 catalog exit = %d, want 0 (mutating open must heal)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	db := catOpen(t, cat)
	for _, obj := range []string{"directories", "idx_directories_image", "idx_directories_unassigned"} {
		if !objectPresent(t, db, obj) {
			t.Errorf("folded object %q missing after the plan run (repair incomplete)", obj)
		}
	}
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Errorf("integrity_check = %q, want ok after the repair", integrity)
	}
	// The plan completed over the healed catalog: images rows exist and
	// whole items are assigned. The dropped dir rows did not survive the
	// table drop (the pre-T2.2 binary never wrote them), so the plan
	// legitimately assigns zero dirs - stated by the report.
	var images int
	if err := db.QueryRow(`SELECT COUNT(*) FROM images WHERE set_id = 'S-HEAL'`).Scan(&images); err != nil {
		t.Fatalf("count images: %v", err)
	}
	if images != 2 {
		t.Errorf("images rows = %d, want 2 (the plan ran to completion)", images)
	}
	rows := queryFiles(t, db, "a.txt")
	if len(rows) != 1 || !rows[0].ImageID.Valid {
		t.Errorf("a.txt rows = %+v, want one assigned row (plan worked on the healed schema)", rows)
	}
	if !strings.Contains(stdout, "assigned this run: 2 files, 2 chunks, 1 symlinks, 0 dirs") {
		t.Errorf("plan report lacks the zero-dirs assignment line:\n%s", stdout)
	}
}

// --- 8. an oversized file chunks across >= 2 destinations, e2e ------------------
