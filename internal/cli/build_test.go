package cli

// T4.3 (wiring) / T4.4 (command acceptance): the build command's test
// matrix. Two tiers:
//
//   - in-process / fake-xorriso tests: the usage gates, the F2 gate at
//     CLI entry, the NFR-004 dependency fail-fast (exit 3), and the
//     failure-path contract through the REAL command tree with a fake
//     xorriso on PATH (step-7 failure -> exit 10, state 'failed',
//     .partial retained / removed by --clean-failed, the transcript in
//     run_events);
//   - the GATED REAL-xorriso e2e (DESIGN_SPEC 12.6): scan child ->
//     REAL plan pipeline (the substituted small profile, the copy_test.go
//     precedent) -> build child -> both ISOs built, self-verified, with
//     the manifest + export + content rows in the catalog, and the
//     N3 partial-failure semantics (image 2 fails on a changed source,
//     image 1 stays built, exit 10).

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"isopress/internal/catalog"
	"isopress/internal/report"
)

// runBuildChildEnv runs the real build command with extra environment
// (the fake-xorriso PATH injection) through the production execute
// path (the runCopyChild pattern).
func runBuildChildEnv(t *testing.T, extraEnv []string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(append(os.Environ(), cliHelperEnv+"=1"), extraEnv...)
	outBuf, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = outBuf, errBuf
	err := cmd.Run()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("running build child %v: %v (stderr: %s)", args, err, errBuf.String())
		}
		code = exitErr.ExitCode()
	}
	return outBuf.String(), errBuf.String(), code
}

// runBuildChild is the plain build child (no extra environment).
func runBuildChild(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	return runBuildChildEnv(t, nil, args...)
}

// --- usage gates and the F2 gate at CLI entry ---------------------------------

// TestBuildUsageGates: every required input is a usage error naming
// the flag (exit 2), and the F2 set-id gate fires at CLI entry before
// ANY database or filesystem work; '=' set ids pass the gate.
func TestBuildUsageGates(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing --catalog", []string{"build", "--set-id", "S", "--dest", "/tmp/x"}, "--catalog"},
		{"missing --set-id", []string{"build", "--catalog", "c.sqlite", "--dest", "/tmp/x"}, "--set-id"},
		{"missing --dest", []string{"build", "--catalog", "c.sqlite", "--set-id", "S"}, "--dest"},
		{"F2 newline set id", []string{"build", "--catalog", "c.sqlite", "--set-id", "S\nEVIL", "--dest", "/tmp/x"}, "newline or carriage return"},
		{"F2 CR set id", []string{"build", "--catalog", "c.sqlite", "--set-id", "S\rEVIL", "--dest", "/tmp/x"}, "newline or carriage return"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code := runInProcess(t, tc.args...)
			if code != report.ExitUsage {
				t.Fatalf("exit = %d, want 2 (usage)\nstderr: %s", code, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr does not contain %q:\n%s", tc.want, stderr)
			}
		})
	}
}

// TestBuildXorrisoMissingIsDependencyExit3 pins the NFR-004 fail-fast
// at the CLI: with xorriso absent from PATH, build exits 3 with the
// DependencyError (and mints no run row - the dependency check runs
// before StartBuildRun).
func TestBuildXorrisoMissingIsDependencyExit3(t *testing.T) {
	root := makeBuildSingleImageTree(t)
	cat := createCatalog(t)
	scanBuildFixture(t, root, cat, "S-DEP")
	planSynthetic(t, cat, "S-DEP", buildFixtureProfile)

	empty := t.TempDir() // a PATH with no xorriso
	stdout, stderr, code := runBuildChildEnv(t,
		[]string{"PATH=" + empty},
		"build", "--catalog", cat, "--set-id", "S-DEP", "--dest", filepath.Join(t.TempDir(), "dest"))
	if code != report.ExitDependency {
		t.Fatalf("exit = %d, want 3 (dependency)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "xorriso") {
		t.Errorf("stderr does not name the missing tool:\n%s", stderr)
	}
	// No run row was minted for the dependency failure.
	db := catOpen(t, cat)
	var runs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM runs WHERE kind = 'build'`).Scan(&runs); err != nil {
		t.Fatal(err)
	}
	if runs != 0 {
		t.Errorf("build runs rows after a dependency failure = %d, want 0 (no run debris)", runs)
	}
}

// TestBuildUnknownImageIsUsage: --image N that was never planned is a
// usage input (exit 2), not a silent no-op.
func TestBuildUnknownImageIsUsage(t *testing.T) {
	root := makeBuildFixtureTree(t)
	cat := createCatalog(t)
	scanBuildFixture(t, root, cat, "S-IMG")
	planSynthetic(t, cat, "S-IMG", buildFixtureProfile)

	_, stderr, code := runInProcess(t,
		"build", "--catalog", cat, "--set-id", "S-IMG", "--dest", t.TempDir(), "--image", "99")
	if code != report.ExitUsage {
		t.Fatalf("exit = %d, want 2 (usage)\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "--image 99") {
		t.Errorf("stderr does not name the offending --image:\n%s", stderr)
	}
}

// --- the fake-xorriso failure-path tier ----------------------------------------

// installFakeXorriso writes a fake `xorriso` into a fresh PATH dir:
// -version answers the probe banner; mastering (-as) creates the -o
// file with fake bytes; extraction (-indev) exits 5 with a marker on
// stderr (the shape of a broken image). It returns the PATH dir and
// the path of a marker file the extraction touch-happens (so tests can
// assert whether an extraction was ever attempted).
func installFakeXorriso(t *testing.T) (pathDir, extractMarker string) {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "extract-invoked")
	script := `#!/bin/sh
if [ "$1" = "-version" ]; then
  echo "xorriso 1.5.6 : RockRidge filesystem manipulator, libburnia project." >&2
  exit 0
fi
if [ "$1" = "-indev" ]; then
  : > "` + marker + `"
  echo "FAKE-XORRISO-EXTRACT-FAILURE" >&2
  exit 5
fi
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
if [ -n "$out" ]; then echo "FAKE ISO BYTES" > "$out"; fi
echo "fake mastering done"
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "xorriso"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing the fake xorriso: %v", err)
	}
	return dir, marker
}

// TestBuildFakeXorrisoStep7Failure: the fake masters the .partial and
// then fails the EXTRACTION, so the self-verify (step 7) fails the
// image through the REAL command tree and the REAL catalog: exit 10
// (ItemFailures), image state 'failed', .partial RETAINED by default,
// and the transcript tail in the run_events error row (6.2).
func TestBuildFakeXorrisoStep7Failure(t *testing.T) {
	root := makeBuildSingleImageTree(t)
	cat := createCatalog(t)
	scanBuildFixture(t, root, cat, "S-FAKE")
	planSynthetic(t, cat, "S-FAKE", buildFixtureProfile)
	fakePath, marker := installFakeXorriso(t)
	dest := filepath.Join(t.TempDir(), "dest")

	stdout, stderr, code := runBuildChildEnv(t, []string{"PATH=" + fakePath},
		"build", "--catalog", cat, "--set-id", "S-FAKE", "--dest", dest, "--json")
	if code != report.ExitItemFailures {
		t.Fatalf("exit = %d, want 10 (item failures)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the fake extraction was never invoked: %v", err)
	}

	var rep buildReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("build --json does not parse: %v\n%s", err, stdout)
	}
	// The FROZEN key set (the scan/plan/copy convention; "failures" is
	// omitempty and joins the set exactly when the run has failures).
	wantKeys := []string{
		"built", "bytes", "dest", "failed", "images", "run_id", "set_id",
		"status", "verified_entries", "warnings",
	}
	if len(rep.Failures) > 0 {
		wantKeys = append(wantKeys, "failures")
	}
	assertFrozenJSONKeys(t, stdout, wantKeys)
	if rep.Status != "partial" || rep.Built != 0 || rep.Failed != 1 {
		t.Fatalf("report = %+v, want a partial run with one failed image", rep)
	}
	if len(rep.Images) != 1 || rep.Images[0].Status != "failed" || rep.Images[0].Failure == "" {
		t.Fatalf("image reports = %+v, want one failed image carrying the failure line", rep.Images)
	}

	// The catalog contract: state 'failed', the .partial RETAINED, no
	// final .iso, and the transcript in run_events.
	db := catOpen(t, cat)
	var state string
	if err := db.QueryRow(`SELECT state FROM images WHERE set_id = 'S-FAKE' AND image_id = 1`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "failed" {
		t.Errorf("image state = %q, want 'failed'", state)
	}
	if _, err := os.Stat(filepath.Join(dest, "S-FAKE-001.iso.partial")); err != nil {
		t.Errorf(".partial must be retained by default (6.3 step 4): %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "S-FAKE-001.iso")); !os.IsNotExist(err) {
		t.Errorf("a final .iso must never exist after a failed verify: %v", err)
	}
	var detail string
	if err := db.QueryRow(`SELECT re.detail FROM run_events re
		JOIN runs r ON r.run_id = re.run_id
		WHERE r.kind = 'build' AND re.level = 'error'`).Scan(&detail); err != nil {
		t.Fatalf("reading the failure run_events row: %v", err)
	}
	if !strings.Contains(detail, "FAKE-XORRISO-EXTRACT-FAILURE") {
		t.Errorf("run_events detail lacks the xorriso transcript tail:\n%s", detail)
	}
	// The run row is stamped 'partial' on the interrupted-free exit-10
	// path.
	var status string
	if err := db.QueryRow(`SELECT status FROM runs WHERE kind = 'build'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "partial" {
		t.Errorf("build run status = %q, want 'partial'", status)
	}
}

// TestBuildStrictPromotesItemFailures closes build's leg of the --strict
// wiring matrix (mirroring TestCopyStrictPromotion): the fake xorriso
// fails the extraction self-verify, so the run completes with one failed
// image - exit 10 WITHOUT --strict and exit 1 WITH it, promoting at the
// END of the run through the finalize seam.
func TestBuildStrictPromotesItemFailures(t *testing.T) {
	root := makeBuildSingleImageTree(t)
	cat := createCatalog(t)
	scanBuildFixture(t, root, cat, "S-BSTRICT")
	planSynthetic(t, cat, "S-BSTRICT", buildFixtureProfile)
	fakePath, _ := installFakeXorriso(t)

	buildArgs := func(extra ...string) []string {
		return append(extra, "build", "--catalog", cat,
			"--set-id", "S-BSTRICT", "--dest", filepath.Join(t.TempDir(), "dest"))
	}

	_, stderr, code := runBuildChildEnv(t, []string{"PATH=" + fakePath}, buildArgs()...)
	if code != report.ExitItemFailures {
		t.Fatalf("build exit = %d, want 10 (one failed image without --strict)\nstderr: %s", code, stderr)
	}
	_, stderr, code = runBuildChildEnv(t, []string{"PATH=" + fakePath}, buildArgs("--strict")...)
	if code != report.ExitFatal {
		t.Fatalf("build --strict exit = %d, want 1 (--strict promotes the tail at the end)\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "--strict") {
		t.Errorf("stderr lacks the --strict promotion message:\n%s", stderr)
	}
}

// TestBuildCleanFailedRemovesPartial: the same failure with
// --clean-failed removes the .partial (6.3 step 4's flag).
func TestBuildCleanFailedRemovesPartial(t *testing.T) {
	root := makeBuildSingleImageTree(t)
	cat := createCatalog(t)
	scanBuildFixture(t, root, cat, "S-CLEAN")
	planSynthetic(t, cat, "S-CLEAN", buildFixtureProfile)
	fakePath, _ := installFakeXorriso(t)
	dest := filepath.Join(t.TempDir(), "dest")

	_, stderr, code := runBuildChildEnv(t, []string{"PATH=" + fakePath},
		"build", "--catalog", cat, "--set-id", "S-CLEAN", "--dest", dest, "--clean-failed")
	if code != report.ExitItemFailures {
		t.Fatalf("exit = %d, want 10\nstderr: %s", code, stderr)
	}
	if _, err := os.Stat(filepath.Join(dest, "S-CLEAN-001.iso.partial")); !os.IsNotExist(err) {
		t.Errorf(".partial must be removed by --clean-failed: %v", err)
	}
	// The human report lands on stdout; progress/errors stay on stderr
	// (FR-046 stream separation, exercised through the real child).
	stdout, stderr, code := runBuildChildEnv(t, []string{"PATH=" + fakePath},
		"build", "--catalog", cat, "--set-id", "S-CLEAN", "--dest", dest, "--clean-failed")
	if code != report.ExitItemFailures {
		t.Fatalf("second run exit = %d, want 10\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "build: set S-CLEAN") || !strings.Contains(stdout, "totals:") {
		t.Errorf("stdout lacks the human report lines:\n%s", stdout)
	}
	if !strings.Contains(stderr, "error:") {
		t.Errorf("stderr lacks the error channel lines:\n%s", stderr)
	}
}

// TestBuildVerifyAfterBuildFalseSkipsExtraction: the documented V1
// reading - the flag gates the step-7 self-verify; when off, the fake
// extraction is NEVER invoked and the image finalizes on xorriso's
// exit 0 alone, honestly marked verify_skipped.
func TestBuildVerifyAfterBuildFalseSkipsExtraction(t *testing.T) {
	root := makeBuildSingleImageTree(t)
	cat := createCatalog(t)
	scanBuildFixture(t, root, cat, "S-NOV")
	planSynthetic(t, cat, "S-NOV", buildFixtureProfile)
	fakePath, marker := installFakeXorriso(t)
	dest := filepath.Join(t.TempDir(), "dest")

	stdout, stderr, code := runBuildChildEnv(t, []string{"PATH=" + fakePath},
		"build", "--catalog", cat, "--set-id", "S-NOV", "--dest", dest,
		"--verify-after-build=false", "--json")
	if code != report.ExitOK {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("the extraction ran although --verify-after-build=false was passed")
	}
	var rep buildReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("build --json does not parse: %v\n%s", err, stdout)
	}
	if rep.Built != 1 || rep.Status != "ok" || rep.VerifiedEntries != 0 {
		t.Fatalf("report = %+v, want one built image with zero verified entries", rep)
	}
	if len(rep.Images) != 1 || !rep.Images[0].VerifySkipped {
		t.Fatalf("image reports = %+v, want verify_skipped=true", rep.Images)
	}
	if _, err := os.Stat(filepath.Join(dest, "S-NOV-001.iso")); err != nil {
		t.Errorf("the image must finalize when the verify is skipped: %v", err)
	}
	db := catOpen(t, cat)
	var state string
	if err := db.QueryRow(`SELECT state FROM images WHERE set_id = 'S-NOV' AND image_id = 1`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "built" {
		t.Errorf("image state = %q, want 'built'", state)
	}
}

// --- the failed-run progress rendering (cosmetic-honesty regression) ----------

// TestBuildCapacityRefusalRendersNoCompletedBar pins the 2026-10-20
// cosmetic fix at the command level: a build that ABORTS with the
// 6.3 step-5 capacity refusal - the stakeholder failure shape, made
// deterministic by wedging the planned image's usable_size below its
// payload + reserved overhead - must never render a completed (100%)
// progress frame. The child runs the plain renderer (stderr is a
// pipe), whose counts were already honest; the interactive renderer's
// bar-CLEARING behavior on Done.Failed is pinned directly in
// internal/progress. The honest abort summary line must still print.
func TestBuildCapacityRefusalRendersNoCompletedBar(t *testing.T) {
	root := makeBuildSingleImageTree(t)
	cat := createCatalog(t)
	scanBuildFixture(t, root, cat, "S-CAP")
	planSynthetic(t, cat, "S-CAP", buildFixtureProfile)
	fakePath, _ := installFakeXorriso(t)

	// Wedge the planned image below its buildability floor: usable_size
	// 2500 can no longer hold payload + units.FixedImageOverhead +
	// 512*entries, so the builder's 6.3 step-5 pre-check refuses it -
	// the exact refusal class the stakeholder hit (an unbuildable
	// assignment lock), triggered here without hand-crafting rows.
	rw := catRW(t, cat)
	if _, err := rw.Exec(`UPDATE images SET usable_size = 2500 WHERE set_id = 'S-CAP'`); err != nil {
		t.Fatalf("wedging the images row: %v", err)
	}

	stdout, stderr, code := runBuildChildEnv(t, []string{"PATH=" + fakePath},
		"build", "--catalog", cat, "--set-id", "S-CAP", "--dest", filepath.Join(t.TempDir(), "dest"))
	if code != report.ExitUsage {
		t.Fatalf("exit = %d, want 2 (the 6.3 step 5 capacity refusal)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if !strings.Contains(stderr, "does not fit its media profile") {
		t.Errorf("stderr lacks the capacity refusal message:\n%s", stderr)
	}
	if strings.Contains(stderr, "100%") {
		t.Errorf("a FAILED run rendered a 100%% progress frame:\n%s", stderr)
	}
	if !strings.Contains(stderr, "build: 0 image(s) built") {
		t.Errorf("stderr lacks the honest abort summary line:\n%s", stderr)
	}
}

// --- the GATED REAL-xorriso e2e (12.6) -----------------------------------------

// makeBuildFixtureTree writes the two-image e2e fixture: two 20 MiB
// files that overflow the 25 MiB synthetic profile onto TWO images
// (neither fits beside the other), plus small files, a hard-link
// family, a symlink, and an empty dir (the copy_test.go fixture shape,
// so both writers are proven against the same plan). The margins are
// deliberate: every image's payload leaves well over the reserved
// overhead (units.FixedImageOverhead + 512/entry) the 6.3-step-5
// pre-check demands - under the 2026-10-20 capacity amendment the
// planner reserves exactly that overhead at fill time, so these plans
// are buildable BY CONSTRUCTION.
//
// WHY NO CHUNKED FILE HERE (documented fixture constraint, re-stated
// for the amended model): a synthetic small profile still yields an
// effective chunk size of usable - reserve, and chunk 0 fills its
// image's payload budget to the byte - which the pre-check accepts by
// construction now, but leaves the fixture no packing slack to assert
// over. Real profiles (20 GiB chunks in 23 GB) carry ample headroom;
// the chunked path is covered by the gated imagebuild real e2e
// (internal/imagebuild/build_real_test.go, chunks round-trip and
// manifest chunk tables) and the CLI chunk WIRING by
// TestMapBuildInventoryChunkMapping below.
func makeBuildFixtureTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFileP(t, filepath.Join(root, "a.bin"), pattern(1, 20<<20), 0o644)
	writeFileP(t, filepath.Join(root, "b.bin"), pattern(2, 20<<20), 0o644)
	writeFileP(t, filepath.Join(root, "small1.txt"), pattern(4, 1000), 0o644)
	writeFileP(t, filepath.Join(root, "fam-a.txt"), pattern(5, 800), 0o644)
	if err := os.Link(filepath.Join(root, "fam-a.txt"), filepath.Join(root, "fam-b.txt")); err != nil {
		t.Fatalf("creating the hard-link family: %v", err)
	}
	if err := os.Symlink("small1.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "empty-dir"), 0o750); err != nil {
		t.Fatal(err)
	}
	return root
}

// buildFixtureProfile is the synthetic usable size of the e2e fixture
// (the overhead-aware figure the 6.3-step-5 pre-check accepts for
// every resulting image; see makeBuildFixtureTree).
const buildFixtureProfile = 25 << 20

// makeBuildSingleImageTree writes a tiny fixture that plans onto ONE
// image under the e2e profile: the fake-xorriso tests exercise the
// per-image failure contract, not the packing, and their assertions
// speak of exactly one image.
func makeBuildSingleImageTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFileP(t, filepath.Join(root, "small1.txt"), pattern(4, 1000), 0o644)
	writeFileP(t, filepath.Join(root, "small2.txt"), pattern(5, 1000), 0o644)
	if err := os.Symlink("small1.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "empty-dir"), 0o750); err != nil {
		t.Fatal(err)
	}
	return root
}

// scanBuildFixture runs the real scan child over the fixture tree.
func scanBuildFixture(t *testing.T, root, cat, setID string) {
	t.Helper()
	if stdout, stderr, code := runScanChildForCopy(t,
		"--catalog", cat, "--set-id", setID, root); code != 0 {
		t.Fatalf("scan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
}

// assertFrozenJSONKeys pins the frozen report schema: the top-level
// key set must equal want exactly (sorted comparison).
func assertFrozenJSONKeys(t *testing.T, stdout string, want []string) {
	t.Helper()
	var keyMap map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &keyMap); err != nil {
		t.Fatalf("report does not parse: %v\n%s", err, stdout)
	}
	got := make([]string, 0, len(keyMap))
	for k := range keyMap {
		got = append(got, k)
	}
	sortStrings(got)
	sortStrings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("report key set = %v, want the frozen set %v", got, want)
	}
}

// sortStrings is the tiny insertion sort the key-set pin uses (small
// n, avoids importing sort twice in this file's shape).
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// TestMapBuildInventoryChunkMapping pins the CLI chunk WIRING (the
// D-04 ISO projection + the per-parent reassembly anchors) without a
// real xorriso: a catalog inventory with two chunks of one parent and
// a hard-link secondary maps onto the StageInventory staging consumes
// (Secondaries DROPPED) plus the ChunkedFileInput records.
func TestMapBuildInventoryChunkMapping(t *testing.T) {
	parentHash := bytes.Repeat([]byte{0xab}, 32)
	chunkHash := bytes.Repeat([]byte{0xcd}, 32)
	inv := catalog.CopyInventory{
		Files: []catalog.CopyFile{{
			FileID: 1, RelPath: "docs/a.txt", Size: 10, Mode: 0o644, Hash: bytes.Repeat([]byte{1}, 32),
		}},
		Chunks: []catalog.CopyChunk{
			{FileID: 2, ChunkIndex: 0, Offset: 0, Length: 5, Hash: chunkHash,
				ParentRelPath: "big.bin", ParentSize: 9, ParentMode: 0o644, ParentUID: 1, ParentGID: 2,
				ParentMtimeNS: 7, ParentDev: 3, ParentInode: 4, ParentHash: parentHash},
			{FileID: 2, ChunkIndex: 1, Offset: 5, Length: 4, Hash: chunkHash,
				ParentRelPath: "big.bin", ParentSize: 9, ParentMode: 0o644,
				ParentMtimeNS: 7, ParentDev: 3, ParentInode: 4, ParentHash: parentHash},
		},
		Symlinks: []catalog.CopySymlink{{FileID: 3, RelPath: "link", LinkTarget: "a.txt", Size: 5, Mode: 0o777}},
		Dirs:     []catalog.CopyDir{{DirID: 4, RelPath: "empty", Mode: 0o750}},
		Secondaries: []catalog.CopySecondary{{
			FileID: 5, RelPath: "docs/a-copy.txt", Size: 10, Hash: bytes.Repeat([]byte{1}, 32),
			PrimaryFileID: 1, PrimaryRelPath: "docs/a.txt",
		}},
	}

	stageInv, chunked, err := mapBuildInventory(inv)
	if err != nil {
		t.Fatalf("mapBuildInventory: %v", err)
	}
	if len(stageInv.Files) != 1 || stageInv.Files[0].RelPath != "docs/a.txt" {
		t.Errorf("mapped files = %+v, want only the primary whole file (D-04: no secondaries)", stageInv.Files)
	}
	if len(stageInv.Chunks) != 2 ||
		stageInv.Chunks[0].ChunkIndex != 0 || stageInv.Chunks[1].ChunkIndex != 1 ||
		stageInv.Chunks[0].Length != 5 || stageInv.Chunks[1].Offset != 5 ||
		!bytes.Equal(stageInv.Chunks[0].Hash, chunkHash) ||
		stageInv.Chunks[0].ParentMode != 0o644 || stageInv.Chunks[0].ParentUID != 1 ||
		stageInv.Chunks[0].ParentGID != 2 || stageInv.Chunks[0].ParentMtimeNS != 7 ||
		stageInv.Chunks[0].ParentDev != 3 || stageInv.Chunks[0].ParentInode != 4 {
		t.Errorf("mapped chunks = %+v, want the two catalog chunk rows with parent metadata", stageInv.Chunks)
	}
	if len(stageInv.Symlinks) != 1 || len(stageInv.Dirs) != 1 {
		t.Errorf("mapped symlinks/dirs = %d/%d, want 1/1", len(stageInv.Symlinks), len(stageInv.Dirs))
	}
	if len(chunked) != 1 || chunked[0].RelPath != "big.bin" ||
		chunked[0].WholeSize != 9 || !bytes.Equal(chunked[0].WholeSHA256, parentHash) {
		t.Errorf("chunked anchors = %+v, want one record for big.bin with the whole-file hash", chunked)
	}

	// A chunked parent without a whole-file hash is a loud refusal.
	bad := inv
	bad.Chunks = append([]catalog.CopyChunk(nil), inv.Chunks...)
	bad.Chunks[0].ParentHash = nil
	if _, _, err := mapBuildInventory(bad); err == nil || !strings.Contains(err.Error(), "whole-file hash") {
		t.Errorf("missing parent hash: err = %v, want the loud refusal", err)
	}
}
