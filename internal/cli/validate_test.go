package cli

// T5.2: the validate command's acceptance matrix (DESIGN_SPEC 7.3,
// FR-022; IMPLEMENTATION_PLAN 4.6 T5.2 acceptance). Everything drives
// the REAL production entry path - a re-exec child running execute()
// over the real command tree with real process streams - against real
// scanned (and, where chunking matters, planned) sets with the REAL
// catalog underneath.
//
// The pins, per the T5.2 acceptance list:
//   - the six 7.3 categories end-to-end with the bit-rot case
//     classifying corrupt (not changed), every finding enumerated on
//     stdout, exit 10;
//   - clean exit 0 (including a scanned-but-UNPLANNED set: validate
//     compares unassigned records - source vs catalog, never vs
//     media - and needs no plan at all);
//   - `new` informational: a new-only run exits 0;
//   - the --json frozen key set with stdout purity (progress on
//     stderr, one JSON document on stdout);
//   - --strict promotion to exit 1;
//   - the Mode threading: full set sees inode drift as changed, the
//     metadata set (the CIFS-class degradation) ignores it and calls
//     the same replacement corrupt;
//   - the concurrent-reader contract: validate DURING a real blocked
//     copy run succeeds with no busy/locked error and leaves the
//     catalog file BYTE-IDENTICAL (no flock, no write);
//   - chunked files riding the whole-file anchor against a real
//     planned catalog whose chunk rows exist.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"isopress/internal/report"
)

// runValidateChild runs the real validate command through the
// production execute path (the runBuildChild pattern).
func runValidateChild(t *testing.T, extraEnv []string, args ...string) (stdout, stderr string, exitCode int) {
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
			t.Fatalf("running validate child %v: %v (stderr: %s)", args, err, errBuf.String())
		}
		code = exitErr.ExitCode()
	}
	return outBuf.String(), errBuf.String(), code
}

// validateFixture writes a mixed fixture tree and scans it into a
// fresh catalog - NO plan: validate must compare an unplanned set
// (every record unassigned) exactly like a planned one.
func validateFixture(t *testing.T, setID string) (root, cat string) {
	t.Helper()
	root = t.TempDir()
	writeFileP(t, filepath.Join(root, "ok.txt"), []byte("ok content\n"), 0o644)
	writeFileP(t, filepath.Join(root, "drift.txt"), []byte("drift content stays\n"), 0o644)
	writeFileP(t, filepath.Join(root, "changed.txt"), []byte("original bytes\n"), 0o644)
	writeFileP(t, filepath.Join(root, "rot.bin"), []byte("0123456789abcdef"), 0o644)
	writeFileP(t, filepath.Join(root, "gone.txt"), []byte("gone content\n"), 0o644)
	writeFileP(t, filepath.Join(root, "locked.txt"), []byte("locked content\n"), 0o644)
	if err := os.Symlink("ok.txt", filepath.Join(root, "link-ok")); err != nil {
		t.Fatalf("symlink link-ok: %v", err)
	}
	if err := os.Symlink("ok.txt", filepath.Join(root, "link-retarget")); err != nil {
		t.Fatalf("symlink link-retarget: %v", err)
	}
	cat = createCatalog(t)
	scanBuildFixture(t, root, cat, setID)
	return root, cat
}

// mutateValidateSource pushes the pristine fixture into every 7.3
// class: drift (metadata only, content intact), changed (bytes and
// mtime), corrupt (same-length flip with the original mtime restored),
// missing (deletion), broken (chmod 000, skipped as root), retargeted
// symlink (changed), plus a fresh new.txt. Returns the EXPECTED counts
// for the run (root folds locked.txt into ok).
func mutateValidateSource(t *testing.T, root string) (want validateCounts) {
	t.Helper()
	// drift: same bytes, mtime moved out an hour -> ok (7.3).
	if err := os.Chtimes(filepath.Join(root, "drift.txt"),
		time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	// changed: different AND longer bytes (size alone proves the
	// metadata half even on coarse-granularity filesystems), PLUS an
	// explicit mtime move: the finding detail this fixture pins is
	// "metadata: size mtime_ns; hash differs", and a rewrite alone
	// only moves mtime when the filesystem's granularity lets it - CI
	// filesystems may round mtimes to 1s, so the mtime_ns half is
	// guaranteed by an absolute Chtimes (+10s for margin) instead of
	// by the write itself.
	changedPath := filepath.Join(root, "changed.txt")
	writeFileP(t, changedPath, []byte("completely different and longer bytes\n"), 0o644)
	moved := time.Now().Add(10 * time.Second)
	if err := os.Chtimes(changedPath, moved, moved); err != nil {
		t.Fatal(err)
	}
	// CORRUPT: same length, different bytes, ORIGINAL mtime restored.
	rotPath := filepath.Join(root, "rot.bin")
	rotInfo, err := os.Stat(rotPath)
	if err != nil {
		t.Fatal(err)
	}
	writeFileP(t, rotPath, []byte("fedcba9876543210"), 0o644)
	if err := os.Chtimes(rotPath, rotInfo.ModTime(), rotInfo.ModTime()); err != nil {
		t.Fatal(err)
	}
	// missing: deletion.
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	// broken (skip as root: permissions do not bind root).
	runningAsRoot := os.Geteuid() == 0
	if !runningAsRoot {
		if err := os.Chmod(filepath.Join(root, "locked.txt"), 0o000); err != nil {
			t.Fatal(err)
		}
	}
	// symlink retarget -> changed.
	if err := os.Remove(filepath.Join(root, "link-retarget")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("changed.txt", filepath.Join(root, "link-retarget")); err != nil {
		t.Fatal(err)
	}
	// new.
	writeFileP(t, filepath.Join(root, "new.txt"), []byte("brand new\n"), 0o644)

	want = validateCounts{OK: 3, Changed: 2, Corrupt: 1, Broken: 1, Missing: 1, New: 1, Compared: 8}
	if runningAsRoot {
		want.OK, want.Broken = 4, 0
	}
	return want
}

// validateCounts are the expected report figures of the mutated
// fixture (8 records: 6 regular + 2 symlinks).
type validateCounts struct {
	OK, Changed, Corrupt, Broken, Missing, New, Anomalies, Compared int
}

// wantOf renders the expected counts line and totals line fragments.
func (c validateCounts) countsLine() string {
	return fmt.Sprintf("ok %d, changed %d, corrupt %d, broken %d, missing %d, new %d, anomalies %d",
		c.OK, c.Changed, c.Corrupt, c.Broken, c.Missing, c.New, c.Anomalies)
}

func (c validateCounts) withAnomalies() validateCounts {
	c.Anomalies = c.Changed + c.Corrupt + c.Broken + c.Missing
	return c
}

// asRoot reports whether the suite runs with root privileges (root
// bypasses the permission model, so the broken/chmod fixtures fold
// into ok there).
func asRoot() bool { return os.Geteuid() == 0 }

// --- usage gates -----------------------------------------------------------------------

func TestValidateUsageGates(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing --catalog", []string{"validate", "--set-id", "S"}, "--catalog"},
		{"missing --set-id", []string{"validate", "--catalog", "c.sqlite"}, "--set-id"},
		{"positional argument rejected", []string{"validate", "--catalog", "c.sqlite", "--set-id", "S", "extra"}, "unknown command"},
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

	// A typo'd --set-id against a REAL catalog is usage-class too
	// (ErrNoSuchSet -> exit 2; the catalog itself is fine).
	cat := createCatalog(t)
	_, stderr, code := runInProcess(t, "validate", "--catalog", cat, "--set-id", "S-NOPE")
	if code != report.ExitUsage {
		t.Fatalf("unknown set exit = %d, want 2\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "no set") {
		t.Errorf("stderr does not name the unknown-set refusal:\n%s", stderr)
	}
}

// --- the clean path, stream separation, and the frozen JSON schema ---------------------

// TestValidateCleanFixtureExitsZero runs a pristine, scanned-but-
// UNPLANNED set: exit 0, the identity/counts/totals lines on stdout,
// progress and the Done summary on stderr (never the reverse), and the
// frozen --json key set with an empty findings array.
func TestValidateCleanFixtureExitsZero(t *testing.T) {
	const setID = "S-VCLEAN"
	root, cat := validateFixture(t, setID)
	want := validateCounts{OK: 8, Compared: 8}.withAnomalies()

	stdout, stderr, code := runValidateChild(t, nil, "validate", "--catalog", cat, "--set-id", setID)
	if code != report.ExitOK {
		t.Fatalf("clean validate exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	for _, line := range []string{
		"validate: set " + setID,
		"root: " + root + " (comparison set: full)",
		want.countsLine(),
		"totals: compared 8 records, status ok (ok 8, changed 0, corrupt 0, broken 0, missing 0, new 0)",
	} {
		if !strings.Contains(stdout, line) {
			t.Errorf("stdout lacks %q:\n%s", line, stdout)
		}
	}
	// No findings on a clean run: the counts line is followed by the
	// totals line directly.
	if strings.Contains(stdout, "\nnew:") || strings.Contains(stdout, "\nchanged:") {
		t.Errorf("clean run printed finding lines:\n%s", stdout)
	}
	// STREAM SEPARATION (FR-046): the phase line lives on stderr only,
	// and stderr carries the one Done summary.
	if !strings.Contains(stderr, "validate: files 1/8, ") {
		t.Errorf("stderr lacks the determinate validate phase line:\n%s", stderr)
	}
	if strings.Contains(stdout, "validate: files") {
		t.Errorf("progress leaked onto stdout:\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if !strings.Contains(stderr, "status ok)") {
		t.Errorf("stderr lacks the Done summary:\n%s", stderr)
	}

	// The --json form: ONE document, frozen keys, empty findings.
	stdout, stderr, code = runValidateChild(t, nil, "validate", "--catalog", cat, "--set-id", setID, "--json")
	if code != report.ExitOK {
		t.Fatalf("--json clean validate exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	var rep validateReport
	// json.Unmarshal rejects trailing garbage - stdout purity proven
	// by the parse itself.
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("--json does not parse as a single document: %v\n%s", err, stdout)
	}
	assertFrozenJSONKeys(t, stdout, []string{
		"anomalies", "broken", "changed", "comparison_set", "corrupt",
		"files", "missing", "new", "ok", "records", "root", "set_id", "status",
	})
	if rep.Status != "ok" || rep.OK != 8 || rep.Records != 8 || rep.Anomalies != 0 ||
		rep.ComparisonSet != "full" || rep.SetID != setID || rep.Root != root {
		t.Fatalf("report = %+v, want a clean full-mode run over 8 records", rep)
	}
	if rep.Files == nil || len(rep.Files) != 0 {
		t.Errorf("report.files = %#v, want a non-nil empty array (frozen key presence)", rep.Files)
	}
	// Progress stayed on stderr under --json too.
	if !strings.Contains(stderr, "validate: files") {
		t.Errorf("stderr lost the progress lines under --json:\n%s", stderr)
	}
}

// --- the category matrix end-to-end (the acceptance centerpiece) ----------------------

// TestValidateCategoryMatrixEndToEnd mutates the fixture into every
// 7.3 class and asserts the whole report: exit 10, the exact counts
// line, one category-prefixed line per finding (including the bit-rot
// corrupt line and the informational new line), the totals line, and
// the --json presentation of the same figures.
func TestValidateCategoryMatrixEndToEnd(t *testing.T) {
	const setID = "S-VCAT"
	root, cat := validateFixture(t, setID)
	want := mutateValidateSource(t, root).withAnomalies()
	if want.Anomalies == 0 {
		t.Fatal("fixture produced no anomalies")
	}

	stdout, stderr, code := runValidateChild(t, nil, "validate", "--catalog", cat, "--set-id", setID)
	if code != report.ExitItemFailures {
		t.Fatalf("anomalous validate exit = %d, want 10\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// The counts line (exact - the pinned presentation).
	if !strings.Contains(stdout, want.countsLine()+"\n") {
		t.Errorf("stdout lacks the exact counts line %q:\n%s", want.countsLine(), stdout)
	}
	// Every finding enumerated, category-prefixed, detail pinned.
	for _, line := range []string{
		"changed: changed.txt (metadata: size mtime_ns; hash differs)",
		"corrupt: rot.bin (bit rot: metadata matches, hash differs)",
		"missing: gone.txt",
		"new: new.txt (informational; a rescan would capture it)",
		"changed: link-retarget (symlink target differs: recorded \"ok.txt\", now \"changed.txt\")",
	} {
		if !strings.Contains(stdout, line+"\n") {
			t.Errorf("stdout lacks the finding line %q:\n%s", line, stdout)
		}
	}
	if !asRoot() {
		if line := "broken: locked.txt (unreadable: permission denied)"; !strings.Contains(stdout, line+"\n") {
			t.Errorf("stdout lacks %q:\n%s", line, stdout)
		}
	}
	// ok entries are counted, never listed: no human line may carry the
	// per-file "ok: <path>" form (the counts line's "ok N," prefix is
	// a different shape entirely).
	for _, line := range strings.Split(stdout, "\n") {
		if strings.HasPrefix(line, "ok: ") {
			t.Errorf("per-file lines must never list ok records, found %q:\n%s", line, stdout)
		}
	}
	// The totals line carries record count + status.
	if wantLine := fmt.Sprintf("totals: compared %d records, status partial (ok %d, changed %d, corrupt %d, broken %d, missing %d, new %d)",
		want.Compared, want.OK, want.Changed, want.Corrupt, want.Broken, want.Missing, want.New); !strings.Contains(stdout, wantLine) {
		t.Errorf("stdout lacks the totals line %q:\n%s", wantLine, stdout)
	}
	// Progress/Done stayed on stderr.
	if !strings.Contains(stderr, "validate: files") || !strings.Contains(stderr, "status partial)") {
		t.Errorf("stderr lacks progress or the Done summary:\n%s", stderr)
	}
	if strings.Contains(stdout, "validate: files") {
		t.Errorf("progress leaked onto stdout:\n%s", stdout)
	}

	// The --json presentation: same figures, frozen keys, findings
	// enumerated with their details.
	stdout, stderr, code = runValidateChild(t, nil, "validate", "--catalog", cat, "--set-id", setID, "--json")
	if code != report.ExitItemFailures {
		t.Fatalf("--json anomalous validate exit = %d, want 10\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	var rep validateReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("--json does not parse: %v\n%s", err, stdout)
	}
	assertFrozenJSONKeys(t, stdout, []string{
		"anomalies", "broken", "changed", "comparison_set", "corrupt",
		"files", "missing", "new", "ok", "records", "root", "set_id", "status",
	})
	if rep.Status != "partial" || rep.OK != want.OK || rep.Changed != want.Changed ||
		rep.Corrupt != want.Corrupt || rep.Broken != want.Broken || rep.Missing != want.Missing ||
		rep.New != want.New || rep.Anomalies != want.Anomalies || rep.Records != want.Compared {
		t.Fatalf("json report = %+v, want counts %+v", rep, want)
	}
	// Findings = anomalies + informational new; ok never appears.
	if len(rep.Files) != want.Anomalies+want.New {
		t.Fatalf("json findings = %d, want %d (anomalies + new)", len(rep.Files), want.Anomalies+want.New)
	}
	sawCorrupt := false
	for _, f := range rep.Files {
		if f.Category == "ok" {
			t.Errorf("ok entry leaked into the findings array: %+v", f)
		}
		if f.Category == "corrupt" && f.Path == "rot.bin" {
			sawCorrupt = true
			if f.Detail != "bit rot: metadata matches, hash differs" {
				t.Errorf("corrupt detail = %q, want the bit-rot wording", f.Detail)
			}
		}
	}
	if !sawCorrupt {
		t.Errorf("json findings lack the corrupt/rot.bin entry: %+v", rep.Files)
	}
}

// --- `new` is informational -----------------------------------------------------------

// TestValidateNewOnlyIsInformational pins 7.3's parenthetical: a run
// whose ONLY non-ok class is `new` exits 0 (new is not an anomaly),
// reports it on stdout with the informational detail, and --strict has
// nothing to promote.
func TestValidateNewOnlyIsInformational(t *testing.T) {
	const setID = "S-VNEW"
	root, cat := validateFixture(t, setID)
	writeFileP(t, filepath.Join(root, "new-only.txt"), []byte("not in the catalog\n"), 0o644)

	stdout, stderr, code := runValidateChild(t, nil, "validate", "--catalog", cat, "--set-id", setID)
	if code != report.ExitOK {
		t.Fatalf("new-only validate exit = %d, want 0 (informational, 7.3)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "new: new-only.txt (informational; a rescan would capture it)") {
		t.Errorf("stdout lacks the informational new line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "anomalies 0") {
		t.Errorf("stdout lacks the zero-anomaly count:\n%s", stdout)
	}
	if !strings.Contains(stdout, "status ok") {
		t.Errorf("totals must report status ok for a new-only run:\n%s", stdout)
	}

	// --strict has no anomaly to promote: still exit 0.
	stdout, stderr, code = runValidateChild(t, nil,
		"validate", "--catalog", cat, "--set-id", setID, "--strict")
	if code != report.ExitOK {
		t.Fatalf("--strict new-only exit = %d, want 0 (new never promotes)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	_ = stderr
}

// --- --strict promotion -------------------------------------------------------------------

// TestValidateStrictPromotesToExitOne: anomalies exit 10 by default
// and exit 1 under --strict, with the promotion message on stderr and
// the report still on stdout.
func TestValidateStrictPromotesToExitOne(t *testing.T) {
	const setID = "S-VSTRICT"
	root, cat := validateFixture(t, setID)
	mutateValidateSource(t, root)

	stdout, _, code := runValidateChild(t, nil, "validate", "--catalog", cat, "--set-id", setID)
	if code != report.ExitItemFailures {
		t.Fatalf("baseline exit = %d, want 10\nstdout: %s", code, stdout)
	}

	stdout, stderr, code := runValidateChild(t, nil,
		"validate", "--catalog", cat, "--set-id", setID, "--strict")
	if code != report.ExitFatal {
		t.Fatalf("--strict exit = %d, want 1 (--strict promotes the anomaly tail)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "--strict") {
		t.Errorf("stderr lacks the --strict promotion message:\n%s", stderr)
	}
	if !strings.Contains(stdout, "status partial") {
		t.Errorf("--strict run must still print the report on stdout:\n%s", stdout)
	}
}

// --- the Mode threading (the CIFS fix through 7.3's gate) -------------------------

// TestValidateChangeDetectionModeThreaded replaces a file under a NEW
// inode with the SAME size and the ORIGINAL mtime restored, then runs
// validate twice:
//
//   - default (auto -> full on this local fs): inode drift + hash
//     differ -> changed, the drifted inode named in the detail;
//   - --change-detection=metadata (the CIFS-class degradation): the
//     synthesized/swapped inode is ignored, size+mtime match, the hash
//     differs -> CORRUPT.
//
// Both classifications flow from the one resolved fsmeta.Mode, and the
// report names the comparison set that decided.
func TestValidateChangeDetectionModeThreaded(t *testing.T) {
	const setID = "S-VCD"
	root := t.TempDir()
	swapPath := filepath.Join(root, "swap.bin")
	writeFileP(t, swapPath, []byte("AAAAAAAA"), 0o644)
	cat := createCatalog(t)
	scanBuildFixture(t, root, cat, setID)

	// The replacement: new inode, same size, original mtime restored.
	orig, err := os.Stat(swapPath)
	if err != nil {
		t.Fatal(err)
	}
	tmp := swapPath + ".tmp-replace"
	writeFileP(t, tmp, []byte("BBBBBBBB"), 0o644)
	if err := os.Rename(tmp, swapPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(swapPath, orig.ModTime(), orig.ModTime()); err != nil {
		t.Fatal(err)
	}

	// Full set (auto resolves to full on the local test filesystem):
	// inode drift is visible -> changed.
	stdout, _, code := runValidateChild(t, nil, "validate", "--catalog", cat, "--set-id", setID)
	if code != report.ExitItemFailures {
		t.Fatalf("full-mode exit = %d, want 10\nstdout: %s", code, stdout)
	}
	if !strings.Contains(stdout, "(comparison set: full)") {
		t.Errorf("stdout lacks the full comparison-set line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "changed: swap.bin (metadata: inode; hash differs)") {
		t.Errorf("full mode must classify the inode-drift replacement `changed`:\n%s", stdout)
	}

	// Metadata set: the inode is ignored -> size+mtime match, hash
	// differs -> corrupt (the same replacement, the other verdict).
	stdout, _, code = runValidateChild(t, nil,
		"validate", "--catalog", cat, "--set-id", setID, "--change-detection=metadata")
	if code != report.ExitItemFailures {
		t.Fatalf("metadata-mode exit = %d, want 10\nstdout: %s", code, stdout)
	}
	if !strings.Contains(stdout, "(comparison set: metadata)") {
		t.Errorf("stdout lacks the metadata comparison-set line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "corrupt: swap.bin (bit rot: metadata matches, hash differs)") {
		t.Errorf("metadata mode must classify the inode-drift replacement `corrupt`:\n%s", stdout)
	}
}

// --- the concurrent reader (no flock; read-only) -----------------------------------

// TestValidateDuringRealCopyRun drives validate WHILE a real copy run
// is blocked mid-item (the 12.7 hook seam): the read-only open must
// neither block nor fail with the busy class, the verdict is the
// honest clean one, and the catalog file is BYTE-IDENTICAL before and
// after the read - the no-write proof behind the 2.1 Option B policy
// (validate takes no flock and performs zero catalog writes).
func TestValidateDuringRealCopyRun(t *testing.T) {
	const setID = "S-VIDCOPY"
	root := t.TempDir()
	writeFileP(t, filepath.Join(root, "data.txt"), pattern(7, 2000), 0o644)
	if err := os.Symlink("data.txt", filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cat := createCatalog(t)
	scanBuildFixture(t, root, cat, setID)
	planSynthetic(t, cat, setID, copySyntheticUsable)
	dest := filepath.Join(t.TempDir(), "dest")

	// The copy child blocks on its FIRST partial write (the COPY-HOOK
	// seam's deterministic sentinel).
	child := exec.Command(os.Args[0])
	child.Env = append(os.Environ(), cliHelperEnv+"=1",
		"ISOPRESS_TEST_COPY_FAIL_AFTER=1",
		"ISOPRESS_TEST_COPY_FAIL_KIND=partial",
		"ISOPRESS_TEST_COPY_FAIL_MODE=block")
	child.Args = append(child.Args, "copy", "--catalog", cat, "--set-id", setID, "--dest", dest)
	stderrPipe, err := child.StderrPipe()
	if err != nil {
		t.Fatalf("copy child stderr pipe: %v", err)
	}
	child.Stdout = &bytes.Buffer{}
	if err := child.Start(); err != nil {
		t.Fatalf("starting the blocked copy child: %v", err)
	}
	defer func() {
		if child.ProcessState == nil {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()
	sentinel := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		tmp := make([]byte, 1)
		for {
			n, err := stderrPipe.Read(tmp)
			if n > 0 {
				buf.Write(tmp[:n])
				if strings.Contains(buf.String(), "COPY-HOOK-REACHED") {
					sentinel <- buf.String()
					return
				}
			}
			if err != nil {
				sentinel <- buf.String()
				return
			}
		}
	}()
	select {
	case out := <-sentinel:
		if !strings.Contains(out, "COPY-HOOK-REACHED") {
			t.Fatalf("copy child never reached the hook sentinel (stderr so far: %s)", out)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the copy child never reached the block hook")
	}

	// Snapshot the catalog with the copy quiesced at the hook, run
	// validate against the live writer, snapshot again: byte-identical.
	before, err := os.ReadFile(cat)
	if err != nil {
		t.Fatalf("snapshot catalog: %v", err)
	}
	stdout, stderr, code := runValidateChild(t, nil, "validate", "--catalog", cat, "--set-id", setID)
	if code != report.ExitOK {
		t.Fatalf("validate during a copy run exit = %d, want 0 (readers never block, 2.1)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if strings.Contains(stderr, "busy") || strings.Contains(stderr, "locked") {
		t.Errorf("a reader during a writer must never see the busy class:\n%s", stderr)
	}
	after, err := os.ReadFile(cat)
	if err != nil {
		t.Fatalf("re-read catalog: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("validate modified the catalog file; the 2.1 read-only contract demands a byte-identical catalog")
	}

	// Release the copy: one SIGINT (the graceful path, exit 130).
	if err := child.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("signaling the copy child: %v", err)
	}
	if err := child.Wait(); err == nil {
		t.Error("the interrupted copy child exited 0, want 130")
	}
}

// --- the chunked whole-file anchor --------------------------------------------------

// TestValidateChunkedWholeFileAnchor plans a set whose big file is
// REALLY chunked (chunk rows exist in the catalog), then validates:
// clean content classifies ok through files.hash alone - validate
// never walks the chunk rows - and a same-length flip with the
// original mtime restored classifies corrupt against that same
// whole-file anchor.
func TestValidateChunkedWholeFileAnchor(t *testing.T) {
	const setID = "S-VCHUNK"
	root := t.TempDir()
	big := pattern(1, copyBigChunkedLen) // 1,352,000 B: chunks under the synthetic profile
	writeFileP(t, filepath.Join(root, "big.bin"), big, 0o644)
	cat := createCatalog(t)
	scanBuildFixture(t, root, cat, setID)
	planSynthetic(t, cat, setID, copySyntheticUsable)

	// THE chunked premise: chunk rows exist and the file row is
	// stamped chunked (otherwise this test would prove nothing).
	db := catOpen(t, cat)
	var chunks int
	if err := db.QueryRow(`SELECT COUNT(*) FROM chunks WHERE set_id = ?`, setID).Scan(&chunks); err != nil {
		t.Fatalf("counting chunks: %v", err)
	}
	if chunks < 2 {
		t.Fatalf("chunk rows = %d, want >= 2 (the fixture must really chunk)", chunks)
	}
	var chunked int
	if err := db.QueryRow(`SELECT chunked FROM files WHERE set_id = ? AND relpath = 'big.bin'`, setID).Scan(&chunked); err != nil {
		t.Fatalf("reading chunked flag: %v", err)
	}
	if chunked != 1 {
		t.Fatalf("big.bin chunked = %d, want 1", chunked)
	}

	// Clean chunked content -> ok (the whole-file hash is the anchor).
	stdout, _, code := runValidateChild(t, nil, "validate", "--catalog", cat, "--set-id", setID)
	if code != report.ExitOK {
		t.Fatalf("clean chunked validate exit = %d, want 0\nstdout: %s", code, stdout)
	}
	if !strings.Contains(stdout, "ok 1, changed 0, corrupt 0, broken 0, missing 0, new 0, anomalies 0") {
		t.Errorf("stdout lacks the clean counts line:\n%s", stdout)
	}

	// Bit rot on the chunked file: same length, original mtime.
	bigPath := filepath.Join(root, "big.bin")
	info, err := os.Stat(bigPath)
	if err != nil {
		t.Fatal(err)
	}
	rotten := append([]byte(nil), big...)
	rotten[len(rotten)/2] ^= 0xff // one flipped byte, same size
	writeFileP(t, bigPath, rotten, 0o644)
	if err := os.Chtimes(bigPath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	stdout, _, code = runValidateChild(t, nil, "validate", "--catalog", cat, "--set-id", setID)
	if code != report.ExitItemFailures {
		t.Fatalf("rotten chunked validate exit = %d, want 10\nstdout: %s", code, stdout)
	}
	if !strings.Contains(stdout, "corrupt: big.bin (bit rot: metadata matches, hash differs)") {
		t.Errorf("stdout lacks the corrupt line against the whole-file anchor:\n%s", stdout)
	}
	if !strings.Contains(stdout, "totals: compared 1 records, status partial") {
		t.Errorf("stdout lacks the totals line proving the record-level comparison:\n%s", stdout)
	}
}
