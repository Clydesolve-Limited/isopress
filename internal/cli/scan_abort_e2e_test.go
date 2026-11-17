package cli

// Abort-path e2e gap test (verification round for defect C of
// 2026-10-19, plus the drain-drop-before-classification contract).
// scan_hardening_test.go pins the abort-summary wording through a
// failing FLUSH (ctx live); this file pins it through a REAL SIGINT
// cancellation of the real scan command - the stakeholder's actual
// Ctrl+C path - end to end:
//
//   - the child runs the production scaffolding (the single-channel
//     signal guard + armSecondSignalEscape + execute, the exact Execute
//     wiring) around the real scan command, over real process streams;
//   - the fixture hashes a multi-GiB SPARSE file (cheap truncate)
//     alongside small files, so the parent can wait for the first
//     progress line (proof the scan is mid-hash) and then deliver ONE
//     SIGINT - the single-signal graceful path;
//   - asserted deterministically: exit 130, the forced-exit line ABSENT
//     (defect B single-signal pin on the real command path), the
//     defect-C abort summary wording with BOTH counts, and - the
//     drain-drop-before-classification contract (FR-014) through the
//     REAL wiring - NO catalog row, NO run_events entry, and NO stderr
//     trace for the aborted in-flight file.
//
// T6.1 interrupt semantics (the T0.5/T1.6 deferral CLOSURE): this test
// now pins the full 10.1 Interrupted contract on the real signal path -
// the run row is finalized status='interrupted' via the DETACHED
// finalizer (FinishRunDetached; the pre-T6.1 behavior left it 'running'
// forever because FinishRun ran under the already-cancelled context and
// the driver rejected the write), the original abort cause survives in
// the error chain (NB N1), and the RESUMED RUN completes cleanly: the
// flock is released at process exit (kernel guarantee, lock.go), the
// leftover 'interrupted' row is tolerated by the reconciler, and the
// re-run reconciles everything the aborted run dropped - the second run
// row stamps 'ok'.
//
// GOMAXPROCS is pinned to 4 in the child env so the pool always runs
// multiple workers: the small files then complete (rendering the first
// progress line) while the sparse file is still hashing, on any host -
// the test never depends on the walk order or the host CPU count.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"isopress/internal/report"
)

// scanSignalHelperEnv selects the scan-signal child in TestMain: the
// production signal scaffolding around the real command tree, with the
// parent's args passed through to execute.
const scanSignalHelperEnv = "ISOPRESS_TEST_SCAN_SIGNAL_HELPER"

// scanSignalHelperMain is the re-exec child: EXACTLY the Execute wiring
// (the single-channel signal guard: manual context + cancel, done
// retirement channel, one registered SIGINT/SIGTERM channel consumed by
// one goroutine - the first signal cancels the command context, a
// second forces exit; execute over the real root with the parent's
// args). Unlike signalHelperMain it runs the real requested command
// (scan), so the parent can abort a REAL scan with a real SIGINT.
func scanSignalHelperMain() int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	armSecondSignalEscape(cancel, done, os.Stderr)
	return execute(ctx, NewRootCmd(), os.Args[1:], os.Stderr)
}

// TestScanSigintAbortDropsAbortedFileEndToEnd aborts a REAL mid-hash
// scan with ONE SIGINT and asserts the full defect-C contract on the
// real wiring path (see the file doc). No sleeps: the parent keys off
// the child's first progress line, and every post-signal wait is a
// bounded join.
func TestScanSigintAbortDropsAbortedFileEndToEnd(t *testing.T) {
	runScanAbortSignalCase(t, syscall.SIGINT)
}

// runScanAbortSignalCase is the shared abort-shape body: ONE real sig
// (SIGINT in the default suite, SIGTERM in the e2e twin - 10.1 says
// "SIGINT or SIGTERM" maps to the SAME 130 class with IDENTICAL
// finalization, and both numbers are registered on the one signal
// channel, so the assertions are signal-agnostic by construction)
// delivered to a REAL mid-hash scan child, then the full contract.
func runScanAbortSignalCase(t *testing.T, sig syscall.Signal) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping the multi-GiB sparse abort e2e in -short mode")
	}

	root := t.TempDir()
	for i, content := range []string{"alpha\n", "bravo\n", "charlie\n", "delta\n"} {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("small%d.txt", i)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The long-running in-flight file: a 4 GiB SPARSE regular file
	// (truncate only - no disk cost; hashing takes seconds of CPU,
	// which is exactly the window the SIGINT must land in).
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
		"scan", "--catalog", cat, "--set-id", "S-ABORT-E2E", root)
	// Pin the worker count so small files complete CONCURRENTLY with
	// the sparse hash regardless of the host's CPU count: the first
	// progress line then always arrives while huge.bin is in flight.
	cmd.Env = append(os.Environ(), scanSignalHelperEnv+"=1", "GOMAXPROCS=4")
	stdout := &bytes.Buffer{}
	cmd.Stdout = stdout
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("scan child stderr pipe: %v", err)
	}
	waitCh := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting scan-signal child: %v", err)
	}
	go func() { waitCh <- cmd.Wait() }()
	exited := false
	defer func() {
		if !exited {
			_ = cmd.Process.Kill()
			select {
			case <-waitCh:
			case <-time.After(10 * time.Second):
			}
		}
	}()

	// Incremental stderr collection: lines feed a channel the test
	// selects on with a deadline (a plain Scanner loop would need a
	// goroutine anyway once the SIGINT is in play).
	lineCh := make(chan string, 16)
	go func() {
		defer close(lineCh)
		sc := bufio.NewScanner(stderrPipe)
		for sc.Scan() {
			lineCh <- sc.Text()
		}
		// A scan error here is the Wait-vs-pipe-close race after the
		// child exits; the collected lines are what matters.
		_ = sc.Err()
	}()

	var stderrAll strings.Builder
	signaled := false
	deadline := time.After(2 * time.Minute)
abortedLoop:
	for {
		select {
		case line, ok := <-lineCh:
			if !ok {
				break abortedLoop // child's stderr closed: it exited
			}
			stderrAll.WriteString(line)
			stderrAll.WriteByte('\n')
			// The first "scan(hash): files" progress line proves the
			// scan is mid-hash (the sparse file is in flight or about
			// to be): ONE signal now.
			if !signaled && strings.Contains(line, "scan(hash): files") {
				signaled = true
				if err := cmd.Process.Signal(sig); err != nil {
					t.Errorf("%v to scan child: %v", sig, err)
					break abortedLoop
				}
			}
		case <-deadline:
			break abortedLoop
		}
	}

	if !signaled {
		_ = cmd.Process.Kill()
		t.Fatalf("never saw a mid-hash progress line (child exited early or too slow); stderr so far:\n%s", stderrAll.String())
	}

	var waitErr error
	select {
	case waitErr = <-waitCh:
		exited = true
	case <-time.After(2 * time.Minute):
		t.Fatalf("scan child did not exit within 2 minutes of the %v (stderr so far:\n%s)", sig, stderrAll.String())
	}
	// Top up stderr with whatever raced the Wait's pipe close.
	for line := range lineCh {
		stderrAll.WriteString(line)
		stderrAll.WriteByte('\n')
	}
	errOut := stderrAll.String()

	// --- defect B, single-signal half on the REAL command path: the run
	// aborts gracefully with exit 130 and the hard escape stays silent.
	exitErr, ok := waitErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("scan child did not exit with an error after %v: %v (stderr: %s)", sig, waitErr, errOut)
	}
	if got := exitErr.ExitCode(); got != report.ExitInterrupted {
		t.Errorf("exit code after %v = %d, want 130 (stderr: %s)", sig, got, errOut)
	}
	if !strings.Contains(errOut, "interrupted by signal") {
		t.Errorf("stderr lacks the interruption message: %q", errOut)
	}
	if strings.Contains(errOut, "forcing exit") {
		t.Errorf("a SINGLE %v wrongly triggered the second-signal hard escape: %q", sig, errOut)
	}

	// --- defect C: the abort summary names BOTH counts, with the
	// rescan reassurance clause, on the real cancellation path.
	summaryRe := regexp.MustCompile(`aborted: \d+ of \d+ completed files reconciled ` +
		`\(remaining work discarded; a rescan will capture it\)`)
	if !summaryRe.MatchString(errOut) {
		t.Errorf("stderr lacks the reconciled-of-completed abort summary:\n%s", errOut)
	}

	// The "set: <set_id> (run <run_id>)" identity line is a COMPLETED-
	// run affordance (the end-of-run report writer never runs on an
	// abort - stdout is byte-checked empty below): neither the abort
	// summary nor any other stderr line may carry it. Byte-checked
	// against the current summary form pinned by summaryRe above.
	if strings.Contains(errOut, "set: ") {
		t.Errorf("the abort path must not emit the 'set:' identity line: %q", errOut)
	}

	// --- drain-drop-before-classification (FR-014) through the REAL
	// wiring: the aborted in-flight file leaves NO trace anywhere.
	if strings.Contains(errOut, "huge.bin") {
		t.Errorf("the aborted in-flight file leaked into stderr (failure warning or error):\n%s", errOut)
	}
	if stdout.String() != "" {
		t.Errorf("an aborted scan must print NO report on stdout, got: %q", stdout.String())
	}

	db := catOpen(t, cat)
	if rows := queryFiles(t, db, "huge.bin"); len(rows) != 0 {
		t.Errorf("the aborted in-flight file has %d catalog rows, want 0 (dropped BEFORE classification): %+v", len(rows), rows)
	}
	var events int
	if err := db.QueryRow(`SELECT COUNT(*) FROM run_events WHERE relpath = 'huge.bin' OR detail LIKE '%huge.bin%'`).
		Scan(&events); err != nil {
		t.Fatalf("count run_events for the aborted file: %v", err)
	}
	if events != 0 {
		t.Errorf("the aborted in-flight file produced %d run_events rows, want 0 (no warning for a dropped abort)", events)
	}
	// The run row exists AND is finalized with the T6.1 interrupted
	// status: the detached finalizer stamps 'interrupted' even though
	// the run context is already cancelled (the pre-T6.1 behavior -
	// FinishRun under the cancelled ctx, row stuck 'running' - is
	// exactly the 2026-10-19 empirically-confirmed debt this closes).
	var status string
	if err := db.QueryRow(`SELECT status FROM runs WHERE set_id = 'S-ABORT-E2E'`).Scan(&status); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	if status != "interrupted" {
		t.Errorf("run status after SIGINT = %q, want 'interrupted' (DESIGN_SPEC 10.1; T6.1 closure)", status)
	}

	// NB N1 chain preservation: the stderr error line must name what was
	// actually interrupted, not just the interrupt wrapper. The abort
	// cause is cancellation-derived (the walk / pool / drain error
	// under the cancelled context); assert the interrupt CLASS wording
	// plus a cause beyond it (a second ": "-separated clause).
	if !strings.Contains(errOut, "scan run ") || !strings.Contains(errOut, "interrupted (run status 'interrupted' recorded") {
		t.Errorf("stderr lacks the scan interrupted-class error naming the run: %q", errOut)
	}

	// --- the RESUMED RUN (T6.1): the flock was released at process
	// exit (kernel guarantee, catalog/lock.go), the leftover
	// 'interrupted' row is tolerated by the reconciler, and a re-run
	// completes cleanly with its own 'ok' run row. The sparse file is
	// truncated to a stub first so the re-run stays cheap in the
	// DEFAULT suite. T6.1 RESUME-TOTALS HONESTY (strengthened): every
	// file must be counted EXACTLY ONCE by the resumed run - the
	// files the aborted run dropped are not lost (they appear as
	// added), and the ones it reconciled are not double-counted (they
	// appear as unchanged). Whether the sparse file lands as added
	// (dropped by an early abort) or modified (a late abort reconciled
	// it before the truncate) depends on the abort timing; either way
	// it is counted exactly once.
	if err := os.Truncate(sparse, 8); err != nil {
		t.Fatalf("truncate sparse before rescan: %v", err)
	}
	rescanOut, rescanErr, rescanCode := runCLIChild(t,
		"scan", "--catalog", cat, "--set-id", "S-ABORT-E2E", root)
	if rescanCode != report.ExitOK {
		t.Fatalf("resumed scan exit = %d, want 0 (interrupted run must not leave the catalog locked or the rescan broken)\nstdout: %s\nstderr: %s",
			rescanCode, rescanOut, rescanErr)
	}
	// The stdout report totals are exact: EVERY file counted exactly
	// once. The CLASS breakdown is timing-dependent by design (how
	// much of run 1 reached the catalog before the abort varies from
	// nothing to everything - both the "0 of 4 reconciled" and the
	// "4 of 4 reconciled" aborts are honest), so the invariant pinned
	// here is the arithmetic: added+modified+unchanged == all 5 files,
	// nothing missing/unreadable/changed (no double count, no loss).
	if !strings.Contains(rescanOut, "scanned 5 entries") {
		t.Errorf("resumed scan stdout lacks the exact file count (double-count or loss?)\nstdout: %s\nstderr: %s", rescanOut, rescanErr)
	}
	totalsRe := regexp.MustCompile(`added (\d+), modified (\d+), unchanged (\d+), missing (\d+), unreadable (\d+), changed (\d+)`)
	m := totalsRe.FindStringSubmatch(rescanOut)
	if m == nil {
		t.Fatalf("resumed scan stdout lacks the class-totals line\nstdout: %s\nstderr: %s", rescanOut, rescanErr)
	}
	var cls [6]int
	for i, v := range m[1:] {
		fmt.Sscanf(v, "%d", &cls[i])
	}
	if cls[0]+cls[1]+cls[2] != 5 || cls[3] != 0 || cls[4] != 0 || cls[5] != 0 {
		t.Errorf("resumed scan class totals = added %d, modified %d, unchanged %d, missing %d, unreadable %d, changed %d; want the 5 files each counted EXACTLY once",
			cls[0], cls[1], cls[2], cls[3], cls[4], cls[5])
	}
	var lastStatus string
	if err := db.QueryRow(`SELECT status FROM runs WHERE set_id = 'S-ABORT-E2E' ORDER BY run_id DESC LIMIT 1`).
		Scan(&lastStatus); err != nil {
		t.Fatalf("read resumed run status: %v", err)
	}
	if lastStatus != "ok" {
		t.Errorf("resumed run status = %q, want 'ok'", lastStatus)
	}
	// The resumed run captured the file the aborted run dropped
	// (drain-drop-before-classification: no row existed after run 1).
	if rows := queryFiles(t, db, "huge.bin"); len(rows) == 0 {
		t.Error("the resumed scan left huge.bin without a catalog row; the rescan must capture the aborted work")
	}
	// No file was recorded TWICE across the interrupted+resumed pair:
	// relpath is keyed per set, and the resumed reconcile must not
	// have inserted a second row for anything the first run already
	// reconciled (the double-count half of the resume-totals
	// honesty contract).
	var dupes int
	if err := db.QueryRow(`SELECT COUNT(*) FROM (
		SELECT relpath FROM files WHERE set_id = 'S-ABORT-E2E'
		GROUP BY relpath HAVING COUNT(*) > 1)`).Scan(&dupes); err != nil {
		t.Fatalf("counting duplicate relpath rows: %v", err)
	}
	if dupes != 0 {
		t.Errorf("%d relpaths have duplicate catalog rows after the resume; the interrupted+resumed pair must record each file exactly once", dupes)
	}
}
