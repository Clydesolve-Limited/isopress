package cli

// Interrupted-run rendering e2e (the 2026-10-20 cosmetic-honesty QA
// fold-in): the copy SIGINT e2e pattern (TestCopySigintFinalizes-
// InterruptedRun - block-mode hook parks the run deterministically at
// the third item's write, ONE real SIGINT, exit 130) re-run with the
// child's stderr attached to a REAL PTY instead of a pipe.
//
// Why the PTY: the DESIGN_SPEC 9.2 renderer matrix only selects the
// INTERACTIVE (progressbar) renderer when stderr is a TTY, and the
// cosmetic lie this fold-in fixes lives exactly there - an
// interrupted run used to hit progressbar's Finish() and render a
// complete "100%" bar for a run that copied nothing past its last
// item. Asserted here on the REAL wiring path: the interactive
// renderer output of the SIGINT-interrupted copy contains NO "100%"
// frame, the interrupted summary still prints (exactly once), and the
// interruption message plus exit 130 are untouched. The success
// control (a completed run still renders its 100% frame) is pinned in
// internal/progress/interactive_test.go; this test is the abort-path
// half.
//
// The PTY pair is opened through /dev/ptmx with two ioctls
// (TIOCSPTLCK unlock, TIOCGPTN number) using only the stdlib syscall
// package - no new dependency (DESIGN_SPEC 11). The child is the
// COMMAND-AGNOSTIC scan-signal helper (production signal scaffolding
// around the real command tree), reused exactly as the plain-stderr
// copy SIGINT e2e reuses it.

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"isopress/internal/report"
)

// openPtyPair returns a connected (master, slave) pseudo-terminal
// pair. The caller passes the slave to a re-exec child as its stderr
// and reads the child's rendered output from the master. Skips the
// test when the host has no devpts (no /dev/ptmx).
func openPtyPair(t *testing.T) (master, slave *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx on this host (PTY e2e needs devpts): %v", err)
	}
	closeMaster := func() { _ = master.Close() }
	// Unlock the slave side (arg 0 = unlock).
	var unlock int32
	if err := ptyIoctl(master, syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		closeMaster()
		t.Fatalf("TIOCSPTLCK (unlock pty slave): %v", err)
	}
	// Fetch the slave's devpts number.
	var index uint32
	if err := ptyIoctl(master, syscall.TIOCGPTN, unsafe.Pointer(&index)); err != nil {
		closeMaster()
		t.Fatalf("TIOCGPTN (pty slave number): %v", err)
	}
	slave, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", index), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		closeMaster()
		t.Fatalf("open pty slave /dev/pts/%d: %v", index, err)
	}
	return master, slave
}

// ptyIoctl is the one ioctl wrapper the PTY pair construction needs.
func ptyIoctl(f *os.File, req uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), req, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

// TestCopySigintInteractiveBarNeverCompletesTo100 is the fold-in's
// end-to-end acceptance test: a REAL SIGINT to a REAL copy child whose
// stderr is a PTY (so the interactive renderer is selected) must
// clear the bar rather than complete it to 100%, while the summary,
// the interruption message, and exit 130 stay exactly as the
// plain-stderr e2e pins them.
func TestCopySigintInteractiveBarNeverCompletesTo100(t *testing.T) {
	f := newCopyCrashFixture(t)
	hookEnv := []string{
		"ISOPRESS_TEST_COPY_FAIL_AFTER=3",
		"ISOPRESS_TEST_COPY_FAIL_KIND=partial",
		"ISOPRESS_TEST_COPY_FAIL_MODE=block",
	}
	master, slave := openPtyPair(t)
	defer master.Close()

	// The scan signal helper is command-agnostic (production signal
	// scaffolding around the real command tree, args through).
	cmd := exec.Command(os.Args[0],
		"copy", "--catalog", f.cat, "--set-id", f.setID, "--dest", f.dest)
	cmd.Env = append(append(os.Environ(), scanSignalHelperEnv+"=1"), hookEnv...)
	cmd.Stderr = slave // a REAL terminal -> the 9.2 matrix picks interactive
	stdout := &bytes.Buffer{}
	cmd.Stdout = stdout
	if err := cmd.Start(); err != nil {
		_ = slave.Close()
		t.Fatal(err)
	}
	// The child owns the slave fd now; the parent keeps only the
	// master, so the final master read terminates (EIO) when the
	// child exits.
	_ = slave.Close()
	exited := false
	defer func() {
		if !exited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	// Continuous master drain (a full pty buffer would block the
	// child mid-render), with a one-shot sentinel on the deterministic
	// mid-copy marker the block-mode hook prints.
	var mu sync.Mutex
	var out strings.Builder
	sentinel := make(chan struct{})
	var sentinelOnce sync.Once
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				mu.Lock()
				out.Write(buf[:n])
				hit := strings.Contains(out.String(), "COPY-HOOK-REACHED")
				mu.Unlock()
				if hit {
					sentinelOnce.Do(func() { close(sentinel) })
				}
			}
			if err != nil {
				return // EIO/EOF: the child closed its slave end
			}
		}
	}()
	snapshot := func() string {
		mu.Lock()
		defer mu.Unlock()
		return out.String()
	}

	// Wait for the deterministic mid-copy park, then ONE SIGINT.
	select {
	case <-sentinel:
	case <-time.After(2 * time.Minute):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		exited = true
		<-readerDone
		t.Fatalf("never saw the mid-copy sentinel on the pty; output so far:\n%s", snapshot())
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		exited = true
		<-readerDone
		t.Fatalf("SIGINT to copy child: %v", err)
	}

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waitCh:
		exited = true
	case <-time.After(2 * time.Minute):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		exited = true
		<-readerDone
		t.Fatalf("copy child did not exit within 2 minutes of the SIGINT (pty output: %s)", snapshot())
	}
	<-readerDone // the master read has terminated; the buffer is final
	errOut := snapshot()

	// --- the interruption contract, unchanged from the plain-stderr e2e:
	// graceful exit 130 with the interruption message, and the run's
	// report still lands on stdout (FR-046).
	exitErr, ok := waitErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("copy child did not exit with an error after SIGINT: %v (pty output: %s)", waitErr, errOut)
	}
	if got := exitErr.ExitCode(); got != report.ExitInterrupted {
		t.Errorf("exit code after SIGINT = %d, want 130 (pty output: %s)", got, errOut)
	}
	if !strings.Contains(errOut, "interrupted by signal") {
		t.Errorf("pty output lacks the interruption message: %q", errOut)
	}
	if strings.Contains(errOut, "forcing exit") {
		t.Errorf("a SINGLE SIGINT wrongly triggered the second-signal hard escape: %q", errOut)
	}
	if !strings.Contains(stdout.String(), "copy: set "+f.setID) {
		t.Errorf("stdout lacks the interrupted run's report header: %q", stdout.String())
	}

	// --- THE FOLD-IN: the interactive renderer output of the
	// interrupted run must contain NO 100% bar frame. The block hook
	// parks the run at the THIRD of three single-chunk items, so the
	// bar could legitimately show at most ~66% (2 of 3 items) before
	// the signal - a "100%" frame can only come from progressbar's
	// Finish() completing a bar the run never earned.
	if strings.Contains(errOut, "100%") {
		t.Errorf("the interrupted run's interactive output contains a 100%% frame "+
			"(the bar must be CLEARED on Done.Interrupted, event.go):\n%q", errOut)
	}
	// The interrupted summary still prints - exactly once. The marker
	// is a summary-only phrase ("metadata warnings"): the bar's
	// DESCRIPTION repeats "copy: files N/M" on every repaint, but only
	// the final summary line carries the metadata-warnings tail.
	if n := strings.Count(errOut, "metadata warnings"); n != 1 {
		t.Errorf("the interrupted summary line appears %d times in the interactive "+
			"output, want exactly 1:\n%q", n, errOut)
	}
}
