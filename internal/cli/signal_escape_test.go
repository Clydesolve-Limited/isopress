package cli

// Signal guard tests (PM decision, defect B of 2026-10-19, review round
// 2). The guard (armSecondSignalEscape in root.go) registers ONE channel
// for SIGINT/SIGTERM and runs ONE consumer goroutine: the first signal
// cancels the command context (graceful path, exit 130), and while
// shutdown is in progress the default disposition for those signals
// stays suppressed - a command stuck in a long uninterruptible stretch
// would be unkillable by ^C, ^C^C, kill, and kill -TERM. The phase-2
// hard escape restores killability: a SECOND signal after the first was
// consumed resets the default disposition and re-raises the signal to
// self, so the kernel terminates the process.
//
// Test strategy (following the T0.5 re-exec pattern of cli_test.go):
//
//   - in-process, INJECTED core (startSecondSignalEscape): the signal
//     channel, the graceful-path cancel, and the kill action are
//     parameters, so the guard's ordering is pinned deterministically -
//     the first signal is consumed as the graceful trigger only, the
//     genuine second fires the force action and prints the
//     forcing-exit line - without ever killing the test process itself;
//   - REAL signals to a REAL stuck child (re-exec): the child arms the
//     production guard, announces readiness only AFTER the channel is
//     registered (arm is synchronous), parks "stuck" after the first
//     signal exactly like a blocked in-flight hash, and the parent
//     asserts the child dies BY SIGNAL when the second SIGINT/SIGTERM
//     arrives;
//   - goroutine lifecycle: settle-poll leak checks pin that the guard
//     goroutine retires on Execute's done channel in both the clean
//     no-signal run and the normal single-signal shutdown (the 99.9%
//     cases), plus ONE real in-process self-signal for the production
//     wiring's single-signal path.

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// escapeHelperEnv selects the escape child in TestMain; the child arms
// the production guard around a "stuck" blocking operation.
const (
	escapeHelperEnv          = "ISOPRESS_TEST_SIGNAL_ESCAPE_HELPER"
	escapeHelperReady        = "ESCAPE-READY"
	escapeHelperShuttingDown = "ESCAPE-SHUTDOWN"
)

// signalEscapeHelperMain is the re-exec child: it arms the production
// guard (armSecondSignalEscape - the exact Execute wiring: manual
// context + cancel, done retirement channel, ONE registered
// SIGINT/SIGTERM channel consumed by ONE goroutine), announces
// readiness only AFTER that (arm registers the channel synchronously,
// so every readiness line the parent reads guarantees both signals
// will be caught), then simulates a command whose graceful shutdown is
// STUCK: the first signal cancels ctx, the child announces the
// shutdown-in-progress on stdout, and then blocks forever without ever
// returning from Execute - the exact stuck-in-flight-hash shape only
// the hard escape can end.
func signalEscapeHelperMain() int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	armSecondSignalEscape(cancel, done, os.Stderr)
	fmt.Println(escapeHelperReady)
	<-ctx.Done() // first signal observed: "graceful shutdown" begins...
	fmt.Fprintln(os.Stdout, escapeHelperShuttingDown)
	// ...and never finishes; only the second signal can end this. The
	// shutdown-ack line lets the parent rule out the kernel's
	// standard-signal COALESCING (two SIGTERMs sent within the same
	// delivery window merge into one delivery - nothing in userspace
	// can observe the second), which would otherwise make the test's
	// double-signal race the scheduler.
	select {}
}

// --- in-process unit tests of the injected guard core -------------------------

// waitGuardCancel waits (bounded) for the injected graceful-path cancel
// to fire - the deterministic sync point standing in for ctx.Done().
func waitGuardCancel(t *testing.T, cancelled <-chan struct{}) {
	t.Helper()
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("first signal never triggered the graceful path (cancel)")
	}
}

// TestSecondSignalEscapeFiresForceOnSecondSignal pins the guard's core
// ordering with an injected signal channel, cancel, and kill action: the
// FIRST signal on the single channel is consumed as the graceful-path
// trigger ONLY (cancel fires, the force action does not); the genuine
// SECOND signal does fire the force action - with the forcing-exit line
// on stderr. The waitGuardCancel sync between the two sends is
// deterministic: by the time the test sends the second signal, the guard
// has provably consumed the first from the channel (FIFO head) - the
// exact production ordering, no drain, no cross-channel race.
func TestSecondSignalEscapeFiresForceOnSecondSignal(t *testing.T) {
	sigs := make(chan os.Signal, 2)
	done := make(chan struct{})
	defer close(done)
	cancelled := make(chan struct{}, 1)
	forced := make(chan os.Signal, 1)
	var errBuf bytes.Buffer

	startSecondSignalEscape(sigs, func() { cancelled <- struct{}{} }, done, &errBuf, func(s os.Signal) { forced <- s })

	sigs <- syscall.SIGTERM // the FIRST signal
	waitGuardCancel(t, cancelled)

	select {
	case got := <-forced:
		t.Fatalf("force fired on the FIRST signal (%v); only a genuine second signal may kill", got)
	case <-time.After(50 * time.Millisecond):
		// Expected: the first signal alone must never trigger the
		// escape. The wait is a negative-observation bound, not a
		// sleep race - the positive assertion below drives the
		// sequencing.
	}

	sigs <- syscall.SIGINT // the genuine SECOND signal
	select {
	case got := <-forced:
		if got != syscall.SIGINT {
			t.Errorf("forced signal = %v, want SIGINT", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("second signal did not fire the force action")
	}
	if !strings.Contains(errBuf.String(), "second signal received, forcing exit") {
		t.Errorf("stderr lacks the forcing-exit line: %q", errBuf.String())
	}
}

// settleEscapeGoroutines waits for the goroutine count to stabilize
// (three consecutive equal samples) - the T1.1 settle-then-poll leak
// pattern, renamed for this package.
func settleEscapeGoroutines(t *testing.T) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	prev, stable := -1, 0
	for time.Now().Before(deadline) {
		n := runtime.NumGoroutine()
		if n == prev {
			stable++
			if stable >= 3 {
				return n
			}
		} else {
			prev, stable = n, 0
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Skipf("goroutine count never settled at %d within the deadline", prev)
	return -1
}

// waitEscapeGoroutinesBaseline polls until the goroutine count drops to
// baseline, failing loudly on timeout.
func waitEscapeGoroutinesBaseline(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		n := runtime.NumGoroutine()
		if n <= baseline {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: %d goroutines, baseline %d", n, baseline)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestSecondSignalEscapeNoLeakOnCleanRun: with the REAL production
// wiring (armSecondSignalEscape - real signal.Notify registration, real
// guard goroutine) and no signal ever delivered, closing done (Execute's
// exit defer) retires the guard goroutine: no leak in the completely
// clean run.
func TestSecondSignalEscapeNoLeakOnCleanRun(t *testing.T) {
	// Warm up the os/signal package FIRST: its process-wide delivery
	// loop goroutine starts on the very first in-process Notify call and
	// never exits; without the warm-up the baseline below would sit one
	// goroutine too low and the leak check would fail forever.
	warm := make(chan os.Signal, 1)
	signal.Notify(warm, syscall.SIGTERM)
	signal.Stop(warm)

	baseline := settleEscapeGoroutines(t)
	_, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	armSecondSignalEscape(cancel, done, &bytes.Buffer{})
	close(done) // Execute's exit defer
	cancel()    // Execute's exit defer (idempotent safety net)
	waitEscapeGoroutinesBaseline(t, baseline)
}

// TestSecondSignalEscapeNoLeakOnSingleSignalShutdown: the 99.9% case -
// ONE REAL signal to THIS process through the REAL production wiring,
// shutdown runs to completion, Execute exits. The guard consumed the
// first signal (phase 2) but must still retire on done without ever
// forcing anything: no goroutine outlives Execute, and the forcing-exit
// line is never printed. Self-signal safety: arm registered the channel
// synchronously, so SIGTERM is always caught (a handler is installed -
// the default action can never fire), and no other test in this package
// runs in parallel.
func TestSecondSignalEscapeNoLeakOnSingleSignalShutdown(t *testing.T) {
	baseline := settleEscapeGoroutines(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var errBuf bytes.Buffer
	done := make(chan struct{})
	armSecondSignalEscape(cancel, done, &errBuf)

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatalf("self-signal: %v", err)
	}
	select {
	case <-ctx.Done(): // the guard consumed the FIRST signal: graceful path began
	case <-time.After(10 * time.Second):
		t.Fatal("the guard never observed the real first signal")
	}
	close(done) // shutdown completed: Execute's exit defer
	waitEscapeGoroutinesBaseline(t, baseline)
	if out := errBuf.String(); strings.Contains(out, "forcing exit") {
		t.Errorf("single-signal shutdown printed the forcing-exit line: %q", out)
	}
}

// REAL signals to a REAL stuck child: the first SIGINT/SIGTERM is
// consumed by the child's single-channel guard (graceful path,
// acknowledged on stdout), the second must kill the stuck child BY
// SIGNAL via the hard escape - asserted from the parent through the
// WaitStatus, plus the forcing-exit line on the child's stderr.
//
// Determinism (the FIFO story that replaced the defect-B drain):
// the child's guard consumes the first signal from its ONE registered
// channel and then parks in the phase-2 force select. The shutdown-ack
// line proves that consumption already happened (the ack is printed
// after ctx fired, which the guard does right after taking the signal
// off the channel), so signal #2 lands in the now-empty channel and is
// unconditionally treated as the second signal - ONE second signal
// suffices, no retries. The only irreducible corner is the kernel's
// standard-signal COALESCING (duplicates raised inside one delivery
// window merge); it cannot apply here, because the ack proves signal #1
// already left the kernel's pending set long before signal #2 is sent.
func TestSecondSignalEscapeKillsStuckChild(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			cmd := exec.Command(os.Args[0])
			cmd.Env = append(os.Environ(), escapeHelperEnv+"=1")
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatalf("helper stdout pipe: %v", err)
			}
			stderr := &bytes.Buffer{}
			cmd.Stderr = stderr
			waitCh := make(chan error, 1)
			if err := cmd.Start(); err != nil {
				t.Fatalf("starting escape helper: %v", err)
			}
			go func() { waitCh <- cmd.Wait() }()
			defer func() {
				if cmd.ProcessState == nil {
					_ = cmd.Process.Kill()
					select {
					case <-waitCh:
					case <-time.After(5 * time.Second):
					}
				}
			}()
			reader := bufio.NewReader(stdout)

			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("helper never became ready: %v (stderr: %s)", err, stderr.String())
			}
			if got := strings.TrimSpace(line); got != escapeHelperReady {
				t.Fatalf("helper readiness line = %q, want %q (stderr: %s)", got, escapeHelperReady, stderr.String())
			}
			// The readiness line guarantees the guard's channel is
			// registered, so the first signal is deterministically
			// caught and cancels the context.
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatalf("first %v to helper: %v", sig, err)
			}
			line, err = reader.ReadString('\n')
			if err != nil {
				t.Fatalf("helper never acknowledged the shutdown: %v (stderr: %s)", err, stderr.String())
			}
			if got := strings.TrimSpace(line); got != escapeHelperShuttingDown {
				t.Fatalf("helper shutdown ack = %q, want %q (stderr: %s)", got, escapeHelperShuttingDown, stderr.String())
			}

			// Second-signal hard escape: the child is stuck in select{}
			// and cannot exit any other way; one signal suffices (see the
			// FIFO note above).
			if err := cmd.Process.Signal(sig); err != nil {
				t.Fatalf("second %v to helper: %v", sig, err)
			}
			var waitErr error
			select {
			case waitErr = <-waitCh:
			case <-time.After(10 * time.Second):
				t.Fatalf("helper survived the second %v after the shutdown ack; the hard escape is broken (stderr: %s)", sig, stderr.String())
			}
			if waitErr == nil {
				t.Fatalf("stuck helper exited CLEANLY after %v; the second signal must kill it by signal", sig)
			}
			ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus)
			if !ok {
				t.Fatalf("helper WaitStatus unavailable: %v (waitErr: %v)", cmd.ProcessState, waitErr)
			}
			if !ws.Signaled() || ws.Signal() != sig {
				t.Errorf("helper exited %v (signaled=%v signal=%v), want KILLED by the second %v (default disposition re-raised)",
					cmd.ProcessState, ws.Signaled(), ws.Signal(), sig)
			}
			if !strings.Contains(stderr.String(), "second signal received, forcing exit") {
				t.Errorf("helper stderr lacks the forcing-exit line: %q", stderr.String())
			}
		})
	}
}
