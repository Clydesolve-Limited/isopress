//go:build e2e

package cli

// T6.1 adversarial interrupt-shape e2e (DESIGN_SPEC 10.1 Interrupted,
// the detached-finalizer bound): the shapes that need a real child
// process, a real signal, or a multi-second stall, kept OUT of the
// default suite's fast loop:
//
//   - the SIGTERM twin of the scan abort e2e: 10.1 says "SIGINT or
//     SIGTERM" maps to the SAME 130 class with IDENTICAL finalization
//     (the one registered channel treats them alike) - pinned on the
//     real command path, not just at the signal-guard harness level;
//   - interrupt during BUILD: the run row 'interrupted', the in-flight
//     image stamped 'failed' detached, its .partial kept for
//     inspection, the report on stdout, and a resumed build that
//     completes the image;
//   - the 5s DETACHED-FINALIZE BOUND (FinishRunDetached's honest
//     degradation): when the catalog's WAL write lock is wedged by a
//     ROGUE writer at interrupt time (a stale NFS lock holder, a
//     a crashed peer's leftover transaction - the lock is advisory at the
//     flock layer, so only the SQLITE layer can wedge), the detached
//     stamp cannot land. The pinned honest behavior: the process STILL
//     exits 130 (the interrupt is the dominant verdict), the stamp is
//     LOST (the row stays 'running' - never a lie that it was
//     'interrupted'), and scan SURFACES the finalize failure on
//     stderr (errors.Join carries it alongside the interrupt line) -
//     the bounded failure is loud, not silent, and never a hang.

import (
	"bufio"
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"isopress/internal/catalog"
	"isopress/internal/report"
)

// TestScanSigtermAbortFinalizesIdentically is the SIGTERM twin of
// scan_abort_e2e_test.go's SIGINT e2e: same fixture, same single-
// signal delivery, same full contract (exit 130, run row
// 'interrupted', abort summary with both counts, no stdout report, no
// hard escape, resumed run 'ok' capturing the dropped work). 10.1
// registers BOTH signals on the one channel; this pins that the
// REAL command path treats them identically, not merely the harness.
func TestScanSigtermAbortFinalizesIdentically(t *testing.T) {
	runScanAbortSignalCase(t, syscall.SIGTERM)
}

// installParkingXorriso installs a fake xorriso that masters NOTHING:
// it creates the -o target (the .partial), writes the PARKED marker,
// and sleeps until killed - the deterministic mid-mastering window
// the SIGINT must land in. It returns the PATH dir and the marker
// path.
func installParkingXorriso(t *testing.T) (dir, marker string) {
	t.Helper()
	dir = t.TempDir()
	marker = filepath.Join(dir, "xorriso-parked.marker")
	// The park MUST survive the test's PATH replacement: the child is
	// started with PATH=dir only (os/exec's dedupEnv keeps the LAST
	// occurrence of a duplicated key), so a bare `sleep 300` fails
	// with "sleep: not found", the script falls through to exit 0, and
	// the fake never parks at all - the SIGINT then races the child's
	// ~ms verify+finalize tail instead of landing in a 300s park, and
	// the run flips between 130/'interrupted' and 10/'partial'
	// depending on where the signal lands (the flake T6.3 QA
	// reproduced; also pre-fix). Bake the absolute path from THIS
	// process, whose PATH is intact, and `exec` it so the parked sleep
	// IS the xorriso process the context kill reaps (no orphaned
	// sleep holding the stderr pipe).
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("finding sleep for the parking xorriso: %v", err)
	}
	script := `#!/bin/sh
if [ "$1" = "-version" ]; then
  echo "xorriso 1.5.6 : RockRidge filesystem manipulator, libburnia project." >&2
  exit 0
fi
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
if [ -n "$out" ]; then : > "$out"; fi
: > "` + marker + `"
exec "` + sleepBin + `" 300
`
	if err := os.WriteFile(filepath.Join(dir, "xorriso"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing the parking xorriso: %v", err)
	}
	return dir, marker
}

// TestBuildInterruptFinalizesInterruptedRun parks a REAL build child
// (production signal scaffolding, real command tree) mid-mastering via
// the parking fake xorriso, delivers ONE SIGINT, and pins the full
// 10.1 contract for build: exit 130, run row 'interrupted', the
// in-flight image stamped 'failed' with its .partial retained, the
// report on stdout - and the RESUMED build (working fake) completes
// the image to 'built'.
func TestBuildInterruptFinalizesInterruptedRun(t *testing.T) {
	buildRoot := makeBuildSingleImageTree(t)
	cat := createCatalog(t)
	scanBuildFixture(t, buildRoot, cat, "S-BUILDINT")
	planSynthetic(t, cat, "S-BUILDINT", buildFixtureProfile)
	fakePath, parked := installParkingXorriso(t)
	dest := filepath.Join(t.TempDir(), "dest")

	cmd := exec.Command(os.Args[0],
		"build", "--catalog", cat, "--set-id", "S-BUILDINT", "--dest", dest)
	cmd.Env = append(os.Environ(), scanSignalHelperEnv+"=1", "PATH="+fakePath)
	stdout := &bytes.Buffer{}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := false
	defer func() {
		if !exited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	// Wait for the parking marker (the fake is mid-mastering), ONE
	// SIGINT, then bounded join.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		if _, err := os.Stat(parked); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the parking xorriso never started (build never reached mastering)")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("SIGINT to build child: %v", err)
	}
	var errOut strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		sc := bufio.NewScanner(stderrPipe)
		for sc.Scan() {
			errOut.WriteString(sc.Text())
			errOut.WriteByte('\n')
		}
	}()
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waitCh:
		exited = true
	case <-time.After(2 * time.Minute):
		t.Fatalf("build child did not exit within 2 minutes of the SIGINT (stderr: %s)", errOut.String())
	}
	<-done
	errText := errOut.String()

	exitErr, ok := waitErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("build child did not exit with an error after SIGINT: %v (stderr: %s)", waitErr, errText)
	}
	if got := exitErr.ExitCode(); got != report.ExitInterrupted {
		t.Errorf("exit after SIGINT = %d, want 130 (stderr: %s)", got, errText)
	}
	if !strings.Contains(errText, "interrupted by signal") {
		t.Errorf("stderr lacks the interruption message: %s", errText)
	}
	if strings.Contains(errText, "forcing exit") {
		t.Errorf("a SINGLE SIGINT wrongly triggered the hard escape: %s", errText)
	}
	// The interrupted build still prints its report on stdout (FR-046:
	// the result is the stdout artifact even on the abort path).
	if !strings.Contains(stdout.String(), "build: set S-BUILDINT") {
		t.Errorf("stdout lacks the interrupted build's report header: %q", stdout.String())
	}

	// The catalog contract: run 'interrupted', the in-flight image
	// 'failed' (stamped detached by the builder), .partial retained.
	db := catOpen(t, cat)
	var status string
	if err := db.QueryRow(`SELECT status FROM runs WHERE kind = 'build'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "interrupted" {
		t.Errorf("build run status = %q, want 'interrupted'", status)
	}
	var imgState string
	if err := db.QueryRow(`SELECT state FROM images WHERE set_id = 'S-BUILDINT' AND image_id = 1`).Scan(&imgState); err != nil {
		t.Fatal(err)
	}
	if imgState != "failed" {
		t.Errorf("in-flight image state = %q, want 'failed' (stamped detached on interrupt)", imgState)
	}
	if _, err := os.Stat(filepath.Join(dest, "S-BUILDINT-001.iso.partial")); err != nil {
		t.Errorf(".partial must be retained for inspection after the interrupt: %v", err)
	}

	// The RESUMED build: the flock was released at process exit, the
	// 'failed' image is rebuildable ('failed' -> 'building', 6.6), and
	// a REAL xorriso completes it (a fake cannot produce a
	// self-verifiable ISO; the gated-build e2e convention applies -
	// where xorriso is absent the interrupt half above still ran and
	// this resume is skipped).
	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skipf("xorriso not on PATH; the interrupt half is pinned, the resume needs the real masterer: %v", err)
	}
	stdout2, stderr2, code := runCLIChild(t,
		"build", "--catalog", cat, "--set-id", "S-BUILDINT", "--dest", dest, "--json")
	if code != report.ExitOK {
		t.Fatalf("resumed build exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout2, stderr2)
	}
	if err := db.QueryRow(`SELECT state FROM images WHERE set_id = 'S-BUILDINT' AND image_id = 1`).Scan(&imgState); err != nil {
		t.Fatal(err)
	}
	if imgState != "built" {
		t.Errorf("resumed image state = %q, want 'built'", imgState)
	}
	var lastStatus string
	if err := db.QueryRow(`SELECT status FROM runs WHERE kind = 'build' ORDER BY run_id DESC LIMIT 1`).Scan(&lastStatus); err != nil {
		t.Fatal(err)
	}
	if lastStatus != "ok" {
		t.Errorf("resumed build run status = %q, want 'ok'", lastStatus)
	}
}

// holdWalWriter acquires the catalog's WAL write lock through a RAW
// sqlite connection (no flock - the sidecar lock is advisory and only
// honored by catalog.Open, which is exactly why a rogue writer is the
// honest wedge scenario). The returned release func frees it.
func holdWalWriter(t *testing.T, catPath string) (release func()) {
	t.Helper()
	db, err := sql.Open("sqlite", catPath)
	if err != nil {
		t.Fatalf("raw open of the catalog: %v", err)
	}
	// Retry briefly: the child's reconcile batches hold the WAL write
	// lock for microseconds at a time - the wedge must wait for a gap,
	// not race it.
	deadline := time.Now().Add(15 * time.Second)
	for {
		if _, err := db.Exec("BEGIN IMMEDIATE"); err == nil {
			break
		} else if time.Now().After(deadline) {
			db.Close()
			t.Fatalf("BEGIN IMMEDIATE (wedging the WAL write lock): %v", err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	return func() {
		_, _ = db.Exec("COMMIT")
		_ = db.Close()
	}
}

// TestInterruptedScanFinalizeUnderWedgedCatalog loses the stamp
// LOUDLY (the 5s FinishRunDetached bound, probe + pin): a rogue WAL
// writer wedges the catalog's write lock while a REAL scan child is
// interrupted, so the detached finalize cannot land. The pinned
// honest behavior: the process still exits 130 (the interrupt is the
// dominant verdict over the finalize failure), the stderr names the
// finalize failure (NB N1-style cause preservation - the warning is
// surfaced, not swallowed), and the row stays 'running' - a LOST
// stamp, never a fake one. The stall is bounded by the 5s detached
// deadline (the test would time out loudly otherwise).
func TestInterruptedScanFinalizeUnderWedgedCatalog(t *testing.T) {
	root := t.TempDir()
	for i, content := range []string{"alpha\n", "bravo\n", "charlie\n", "delta\n"} {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("small%d.txt", i)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sparse := filepath.Join(root, "huge.bin")
	f, err := os.Create(sparse)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(4 << 30); err != nil {
		f.Close()
		t.Skipf("filesystem refused a 4 GiB sparse truncate: %v", err)
	}
	f.Close()

	cat := filepath.Join(t.TempDir(), "catalog.sqlite")
	cmd := exec.Command(os.Args[0],
		"scan", "--catalog", cat, "--set-id", "S-WEDGE", root)
	cmd.Env = append(os.Environ(), scanSignalHelperEnv+"=1", "GOMAXPROCS=4")
	stdout := &bytes.Buffer{}
	cmd.Stdout = stdout
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := false
	defer func() {
		if !exited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	lineCh := make(chan string, 16)
	go func() {
		defer close(lineCh)
		sc := bufio.NewScanner(stderrPipe)
		for sc.Scan() {
			lineCh <- sc.Text()
		}
	}()
	var stderrAll strings.Builder
	signaled := false
	release := func() {}
	defer func() { release() }() // runs the real release if the wedge was taken
	deadline := time.After(2 * time.Minute)
wedgeLoop:
	for {
		select {
		case line, ok := <-lineCh:
			if !ok {
				break wedgeLoop
			}
			stderrAll.WriteString(line)
			stderrAll.WriteByte('\n')
			// First mid-hash progress line: wedge the write lock
			// FIRST (the scan is hashing, not writing), THEN signal -
			// so the detached finalize is guaranteed to hit the
			// wedged lock.
			if !signaled && strings.Contains(line, "scan(hash): files") {
				signaled = true
				release = holdWalWriter(t, cat)
				if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
					t.Errorf("SIGINT to scan child: %v", err)
					break wedgeLoop
				}
			}
		case <-deadline:
			break wedgeLoop
		}
	}
	if !signaled {
		t.Fatalf("never saw a mid-hash progress line; stderr so far:\n%s", stderrAll.String())
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waitCh:
		exited = true
	case <-time.After(3 * time.Minute):
		t.Fatalf("wedged-catalog scan child did not exit (the 5s bound must prevent a hang; stderr so far:\n%s)", stderrAll.String())
	}
	for line := range lineCh {
		stderrAll.WriteString(line)
		stderrAll.WriteByte('\n')
	}
	release() // free the wedge before reading the catalog
	errOut := stderrAll.String()

	exitErr, ok := waitErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("scan child did not exit with an error after SIGINT: %v (stderr: %s)", waitErr, errOut)
	}
	if got := exitErr.ExitCode(); got != report.ExitInterrupted {
		t.Errorf("exit = %d, want 130 (the interrupt stays the dominant verdict even when the stamp is lost)\nstderr: %s", got, errOut)
	}
	// The lost stamp is LOUD: the finalize failure rides the joined
	// error to stderr (NB N1 - the cause is never swallowed).
	if !strings.Contains(errOut, "finalizing run") {
		t.Errorf("stderr lacks the detached-finalize failure warning (a lost stamp must be loud, not silent):\n%s", errOut)
	}
	// And the stamp is genuinely LOST, not faked: the row never
	// reached 'interrupted'.
	var status string
	if err := catOpen(t, cat).QueryRow(`SELECT status FROM runs WHERE set_id = 'S-WEDGE'`).Scan(&status); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	if status != "running" {
		t.Errorf("run status = %q, want 'running' (the stamp could not land; a fake 'interrupted' would be a lie)", status)
	}
}

// TestFinishRunDetachedIsBoundedUnderWedgedCatalog is the catalog-tier
// half of the wedge probe: FinishRunDetached against a wedged WAL
// write lock RETURNS AN ERROR within the detached deadline (never
// hangs, never reports success), and succeeds normally once the lock
// is free - the primitive the scan/copy/build abort paths rely on.
func TestFinishRunDetachedIsBoundedUnderWedgedCatalog(t *testing.T) {
	cat := createCatalog(t)
	// Take the catalog's own handle so the flock is held exactly like
	// a real run (FinishRunDetached runs under the run's own open).
	db, err := catalog.Open(cat, catalog.OpenReadWrite)
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close()

	release := holdWalWriter(t, cat)
	start := time.Now()
	finishErr := db.FinishRunDetached(999, "interrupted", time.Now().UnixNano(), "{}")
	elapsed := time.Since(start)
	if finishErr == nil {
		release()
		t.Fatal("FinishRunDetached SUCCEEDED against a wedged write lock; the bounded finalizer must fail honestly")
	}
	if elapsed > 15*time.Second {
		release()
		t.Fatalf("FinishRunDetached took %v under a wedged lock; the 5s detached deadline must bound it", elapsed)
	}
	// Release the wedge: the same call now reaches the UPDATE (and
	// fails on the nonexistent run id - a DIFFERENT, prompt error -
	// proving the first failure was the lock, not the API).
	release()
	if err := db.FinishRunDetached(999, "interrupted", time.Now().UnixNano(), "{}"); err == nil {
		t.Fatal("FinishRunDetached(999) succeeded for a nonexistent run id")
	} else if !strings.Contains(err.Error(), "no such run_id") {
		t.Errorf("post-wedge error = %v, want the no-such-run error (the wedge was the only blocker)", err)
	}
}
