package cli

// T5.1: the verify-media command's acceptance matrix (DESIGN_SPEC 7.2,
// FR-021; IMPLEMENTATION_PLAN 4.6 T5.1 acceptance + adversarial list).
// Everything drives the REAL production entry path - a re-exec child
// running execute() over the real command tree with real process
// streams (the cli_test.go pattern) - against real scanned + planned +
// copied sets, with the REAL catalog underneath.
//
// The corrupted-ISO acceptance test is GATED on the real xorriso
// (DESIGN_SPEC 12.6); the copy-tree paths need no xorriso at all -
// which the PATH=empty tests pin both ways (copy works, ISO exits 3).

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"isopress/internal/report"
)

// runVerifyMediaChild runs the real verify-media command through the
// production execute path (the runBuildChild pattern).
func runVerifyMediaChild(t *testing.T, extraEnv []string, args ...string) (stdout, stderr string, exitCode int) {
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
			t.Fatalf("running verify-media child %v: %v (stderr: %s)", args, err, errBuf.String())
		}
		code = exitErr.ExitCode()
	}
	return outBuf.String(), errBuf.String(), code
}

// verifyCopyFixture scans, plans, and COPIES the small single-image
// fixture (small1.txt, small2.txt, a symlink, an empty dir) through
// the REAL commands, returning the catalog and destination paths.
func verifyCopyFixture(t *testing.T, setID string) (cat, dest string) {
	t.Helper()
	root := makeBuildSingleImageTree(t)
	cat = createCatalog(t)
	scanBuildFixture(t, root, cat, setID)
	planSynthetic(t, cat, setID, copySyntheticUsable)
	dest = filepath.Join(t.TempDir(), "dest")
	if stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest); code != 0 {
		t.Fatalf("copy fixture exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	return cat, dest
}

// --- usage gates -------------------------------------------------------------------

func TestVerifyMediaUsageGates(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing --catalog", []string{"verify-media", "--set-id", "S"}, "--catalog"},
		{"missing --set-id", []string{"verify-media", "--catalog", "c.sqlite"}, "--set-id"},
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

	// An --image N that was never PLANNED is a usage error naming the
	// value (selectCopyImages' contract; the real command reaches it
	// only with a live catalog).
	cat, _ := verifyCopyFixture(t, "S-VIMG")
	_, stderr, code := runInProcess(t, "verify-media", "--catalog", cat, "--set-id", "S-VIMG", "--image", "99")
	if code != report.ExitUsage {
		t.Fatalf("--image 99 exit = %d, want 2\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "--image 99") {
		t.Errorf("stderr does not name the offending --image:\n%s", stderr)
	}

	// A scanned set with NO plan is a usage error, not a silent no-op.
	root := makeBuildSingleImageTree(t)
	cat2 := createCatalog(t)
	scanBuildFixture(t, root, cat2, "S-NOPLAN")
	_, stderr, code = runInProcess(t, "verify-media", "--catalog", cat2, "--set-id", "S-NOPLAN")
	if code != report.ExitUsage {
		t.Fatalf("unplanned set exit = %d, want 2\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "no planned images") {
		t.Errorf("stderr lacks the no-images refusal:\n%s", stderr)
	}
}

// --- copy-tree acceptance -----------------------------------------------------------

// TestVerifyMediaCopyTreeCleanStamps is the plan's clean-image
// acceptance over a REAL copy tree: exit 0, the report says verified
// with all counts, and the catalog carries the 7.2 stamp (verified_at
// + state 'verified') plus the recorded final_path (the T5.1
// copy-completion addition this command depends on).
func TestVerifyMediaCopyTreeCleanStamps(t *testing.T) {
	const setID = "S-VCLEAN"
	cat, dest := verifyCopyFixture(t, setID)

	// THE final_path GAP CLOSURE: the copy completion path recorded
	// where image 1 landed.
	db := catOpen(t, cat)
	var finalPath string
	if err := db.QueryRow(`SELECT final_path FROM images WHERE set_id = ? AND image_id = 1`, setID).Scan(&finalPath); err != nil {
		t.Fatalf("reading final_path: %v", err)
	}
	if want := destImageDir(dest, setID, 1); finalPath != want {
		t.Fatalf("final_path = %q, want the copy dir %q (the T5.1 copy completion addition)", finalPath, want)
	}

	stdout, stderr, code := runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID)
	if code != report.ExitOK {
		t.Fatalf("verify-media exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "verify-media: set "+setID) ||
		!strings.Contains(stdout, "image 1: verified "+finalPath+" (copy)") ||
		!strings.Contains(stdout, "totals: images 1, verified 1, matched ") {
		t.Errorf("stdout lacks the human report lines:\n%s", stdout)
	}
	if !strings.Contains(stdout, "stamped true") {
		t.Errorf("stdout does not report the 7.2 stamp:\n%s", stdout)
	}

	// THE STAMP: state 'verified' + verified_at in the catalog.
	var state string
	var verifiedAt sql.NullInt64
	if err := db.QueryRow(`SELECT state, verified_at FROM images WHERE set_id = ? AND image_id = 1`, setID).
		Scan(&state, &verifiedAt); err != nil {
		t.Fatalf("reading the stamped row: %v", err)
	}
	if state != "verified" || !verifiedAt.Valid || verifiedAt.Int64 <= 0 {
		t.Errorf("images row = (state %q, verified_at %v), want ('verified', stamped)", state, verifiedAt)
	}

	// The --json form: parseable, frozen key set, stamped=true.
	stdout, _, code = runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID, "--json")
	if code != report.ExitOK {
		t.Fatalf("verify-media --json exit = %d, want 0\nstdout: %s", code, stdout)
	}
	var rep verifyMediaReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("verify-media --json does not parse: %v\n%s", err, stdout)
	}
	assertFrozenJSONKeys(t, stdout, []string{
		"images", "matched", "mismatched", "missing", "set_id", "status",
		"unexpected", "unreadable", "verified",
	})
	if rep.Status != "ok" || rep.Verified != 1 || rep.Matched == 0 {
		t.Fatalf("report = %+v, want an ok run with one verified image", rep)
	}
	if len(rep.Images) != 1 || rep.Images[0].Source != "copy" || !rep.Images[0].Stamped ||
		rep.Images[0].Status != "verified" || rep.Images[0].Path != finalPath {
		t.Fatalf("image reports = %+v, want one verified copy image at final_path", rep.Images)
	}
}

// TestVerifyMediaCopyTreeDetectsCorruption is the plan's corrupted-
// media acceptance over a real copy tree, with the adversarial mix:
// one flipped payload, one deleted payload, one planted rogue file -
// each reported in ITS class, the run exits 10, and the image state is
// left UNCHANGED (the documented partial-match policy).
func TestVerifyMediaCopyTreeDetectsCorruption(t *testing.T) {
	const setID = "S-VBAD"
	cat, dest := verifyCopyFixture(t, setID)
	imgDir := destImageDir(dest, setID, 1)

	// Flip one byte inside small1.txt (unique content in this tree).
	small1Path := filepath.Join(imgDir, "files", "small1.txt")
	orig, err := os.ReadFile(small1Path)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), orig...)
	corrupt[0] ^= 0xff
	if err := os.WriteFile(small1Path, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	// Delete another payload; plant a rogue file.
	if err := os.Remove(filepath.Join(imgDir, "files", "small2.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(imgDir, "files", "rogue.bin"), []byte("rogue"), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID, "--json")
	if code != report.ExitItemFailures {
		t.Fatalf("verify-media exit = %d, want 10 (anomalies)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	var rep verifyMediaReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("verify-media --json does not parse: %v\n%s", err, stdout)
	}
	if rep.Status != "partial" || rep.Verified != 0 ||
		rep.Mismatched != 1 || rep.Missing != 1 || rep.Unexpected != 1 || rep.Unreadable != 0 {
		t.Fatalf("report = %+v, want one mismatch + one missing + one unexpected, nothing stamped", rep)
	}
	if len(rep.Images) != 1 {
		t.Fatalf("image reports = %+v", rep.Images)
	}
	img := rep.Images[0]
	if img.Status != "mismatch" || img.Stamped {
		t.Errorf("image report = %+v, want a mismatch with no stamp", img)
	}
	// THE PER-FILE LINES name each finding in its class.
	wantLines := []string{
		"files/small1.txt: content hash",
		"missing from extraction: files/small2.txt (kind file)",
		"unexpected file in extraction: files/rogue.bin",
	}
	for _, want := range wantLines {
		if !containsLineWith(img.Problems, want) {
			t.Errorf("image problems lack %q:\n%v", want, img.Problems)
		}
	}
	if len(img.Problems) != len(wantLines) {
		t.Errorf("image problems = %v, want exactly the three findings", img.Problems)
	}

	// THE DOCUMENTED STATE POLICY: a partial match leaves the state
	// UNCHANGED ('built' for a copy image since T6.5, 'planned' for
	// pre-T6.5 rows) and stamps nothing.
	db := catOpen(t, cat)
	var state string
	var verifiedAt sql.NullInt64
	if err := db.QueryRow(`SELECT state, verified_at FROM images WHERE set_id = ? AND image_id = 1`, setID).
		Scan(&state, &verifiedAt); err != nil {
		t.Fatal(err)
	}
	if state != "built" || verifiedAt.Valid {
		t.Errorf("images row = (state %q, verified_at %v), want ('built', NULL) - a partial verify changes nothing",
			state, verifiedAt)
	}
}

// TestVerifyMediaPartialLeavesStateBuilt pins the documented choice on
// the BUILD-side shape: a corrupted image whose state is 'built' stays
// 'built' (a partial verify does NOT downgrade it to 'failed').
func TestVerifyMediaPartialLeavesStateBuilt(t *testing.T) {
	const setID = "S-VPARTIAL"
	cat, dest := verifyCopyFixture(t, setID)
	imgDir := destImageDir(dest, setID, 1)

	// The build-side state shape, seeded explicitly (the copy writer
	// stamps state 'built' itself since T6.5, but never built_at - the
	// copy completion writes three columns only, see
	// RecordCopyFinalPath's godoc - so this test seeds the timestamp).
	rw := catRW(t, cat)
	if _, err := rw.Exec(`UPDATE images SET state = 'built', built_at = 1234 WHERE set_id = ? AND image_id = 1`, setID); err != nil {
		t.Fatal(err)
	}
	small1Path := filepath.Join(imgDir, "files", "small1.txt")
	orig, err := os.ReadFile(small1Path)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), orig...)
	corrupt[1] ^= 0xff
	if err := os.WriteFile(small1Path, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID)
	if code != report.ExitItemFailures {
		t.Fatalf("exit = %d, want 10\nstderr: %s", code, stderr)
	}
	db := catOpen(t, cat)
	var state string
	var verifiedAt sql.NullInt64
	if err := db.QueryRow(`SELECT state, verified_at FROM images WHERE set_id = ? AND image_id = 1`, setID).
		Scan(&state, &verifiedAt); err != nil {
		t.Fatal(err)
	}
	if state != "built" || verifiedAt.Valid {
		t.Errorf("images row = (state %q, verified_at %v), want ('built', NULL): a partial verify leaves state UNCHANGED",
			state, verifiedAt)
	}
}

// --- the xorriso dependency split (copy needs none, ISO exits 3) ----------------------

// TestVerifyMediaCopyTreeWorksWithoutXorriso: with xorriso ABSENT from
// PATH, copy-tree verification still runs end to end (exit 0) - the
// command never pays the dependency check for copy trees.
func TestVerifyMediaCopyTreeWorksWithoutXorriso(t *testing.T) {
	const setID = "S-VNOX"
	cat, _ := verifyCopyFixture(t, setID)
	empty := t.TempDir() // a PATH with no xorriso
	stdout, stderr, code := runVerifyMediaChild(t, []string{"PATH=" + empty},
		"verify-media", "--catalog", cat, "--set-id", setID)
	if code != report.ExitOK {
		t.Fatalf("copy-tree verify with xorriso absent exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "(copy)") {
		t.Errorf("stdout lacks the copy-source report:\n%s", stdout)
	}
}

// TestVerifyMediaISOWithoutXorrisoExits3: an ISO image in scope with
// xorriso absent from PATH fails fast through the T4.1 DependencyError
// seam (exit 3), naming the tool - before any per-image work.
func TestVerifyMediaISOWithoutXorrisoExits3(t *testing.T) {
	const setID = "S-VISO3"
	cat, dest := verifyCopyFixture(t, setID)
	// Point image 1 at an .iso FILE (the resolution pre-pass classes
	// it as ISO, which is what triggers the dependency check).
	iso := filepath.Join(t.TempDir(), setID+"-001.iso")
	if err := os.WriteFile(iso, []byte("not a real iso"), 0o644); err != nil {
		t.Fatal(err)
	}
	rw := catRW(t, cat)
	if _, err := rw.Exec(`UPDATE images SET final_path = ? WHERE set_id = ? AND image_id = 1`, iso, setID); err != nil {
		t.Fatal(err)
	}
	_ = dest

	empty := t.TempDir()
	stdout, stderr, code := runVerifyMediaChild(t, []string{"PATH=" + empty},
		"verify-media", "--catalog", cat, "--set-id", setID)
	if code != report.ExitDependency {
		t.Fatalf("ISO verify with xorriso absent exit = %d, want 3 (dependency)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "xorriso") {
		t.Errorf("stderr does not name the missing tool:\n%s", stderr)
	}
}

// --- the read-only contract (2.1 Option B) -------------------------------------------

// TestVerifyMediaRunsDuringMutatingRunSucceedsAndStamps pins the
// no-flock contract at its strongest: while another process HOLDS the
// mutating catalog lock (the T0.4 holder - what a running copy or
// build holds for its whole run), verify-media READS over the WAL
// snapshot and even WRITES its one full-match stamp (the documented
// lock-free stamping connection) - never the busy class, never a
// block.
func TestVerifyMediaRunsDuringMutatingRunSucceedsAndStamps(t *testing.T) {
	const setID = "S-VCONC"
	cat, _ := verifyCopyFixture(t, setID)

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
		_ = holderStdin.Close() // EOF releases the holder
		_ = holder.Wait()
	}()
	buf := make([]byte, len(lockHoldReady)+2)
	if _, err := holderOut.Read(buf); err != nil {
		t.Fatalf("holder never became ready: %v (stderr: %s)", err, holderStderr.String())
	}
	if !strings.Contains(string(buf), lockHoldReady) {
		t.Fatalf("holder readiness = %q, want %q (stderr: %s)", string(buf), lockHoldReady, holderStderr.String())
	}

	stdout, stderr, code := runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID)
	if code != report.ExitOK {
		t.Fatalf("verify-media during a mutating run exit = %d, want 0 (readers never block, amended 2.1)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if strings.Contains(stderr, "busy") || strings.Contains(stderr, "locked") {
		t.Errorf("a reader during a writer must never see the busy class:\n%s", stderr)
	}
	// THE STAMP landed even though the flock is held elsewhere.
	db := catOpen(t, cat)
	var state string
	if err := db.QueryRow(`SELECT state FROM images WHERE set_id = ? AND image_id = 1`, setID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "verified" {
		t.Errorf("image state = %q, want 'verified' - the lock-free stamp must land during a writer", state)
	}
}

// TestVerifyMediaDuringRealCopyRun drives a REAL copy run BLOCKED
// mid-item (the 12.7 hook seam, "block" mode) and verifies media WHILE
// the copy is in flight: the read-only open must neither block nor
// fail with the busy class, and the report is the honest mid-run view
// (the in-flight image has no final_path yet).
func TestVerifyMediaDuringRealCopyRun(t *testing.T) {
	const setID = "S-VMIDCOPY"
	root := makeBuildFixtureTree(t)
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

	// The copy is IN FLIGHT and holds the mutating flock; verify-media
	// reads anyway (exit 10: the in-flight image has no final_path
	// recorded yet - an honest per-image failure, never a busy error).
	stdout, stderr, code := runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID)
	if code != report.ExitItemFailures {
		t.Fatalf("verify-media during a real copy exit = %d, want 10 (the in-flight image has no final_path)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if strings.Contains(stderr, "busy") || strings.Contains(stderr, "locked") {
		t.Errorf("a reader during a copy run must never see the busy class:\n%s", stderr)
	}
	if !strings.Contains(stdout, "images.final_path is empty") {
		t.Errorf("stdout lacks the honest unresolved-location line:\n%s", stdout)
	}

	// Release the copy: one SIGINT (the graceful path, exit 130).
	if err := child.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("signaling the copy child: %v", err)
	}
	if err := child.Wait(); err == nil {
		t.Error("the interrupted copy child exited 0, want 130")
	}
}

// --- the GATED REAL-xorriso acceptance pair (12.6) ------------------------------------

// containsLineWith reports whether any line contains want (the
// per-file problem lines carry the media path prefix).
func containsLineWith(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}
