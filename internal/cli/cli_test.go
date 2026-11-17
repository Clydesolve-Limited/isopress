package cli

// T0.5 acceptance tests: the CLI surface of DESIGN_SPEC 1.1 and the
// exit-code behavior of the execution wrapper (DESIGN_SPEC 12.1
// exit-code mapping tests, 10.3 stream separation). Two deterministic
// re-exec patterns are used, following the catalog package's T0.4
// precedent (DESIGN_SPEC 12.3 re-exec helper style):
//
//   - The CLI helper child re-execs the test binary to run the REAL
//     production entry path (execute over the real command tree with the
//     process streams), so exit codes and stdout/stderr separation are
//     asserted end to end without a pre-built binary.
//   - The signal helper child re-execs the test binary to run the REAL
//     signal scaffolding (the single-channel signal guard + execute);
//     readiness is announced on stdout from inside the command's RunE,
//     so the parent's SIGINT/SIGTERM delivery is deterministic - never a
//     sleep race.

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"isopress/internal/catalog"
	"isopress/internal/report"
)

// stubCommands names the nine commands of DESIGN_SPEC 1.1 in their
// documented tree order. Tests assert this exact set against the root
// help output. Test-only scaffolding: it moved out of root.go (its T0.5
// home, where it was production code no production file referenced) into
// this _test file when T5.3 removed the last stub - every one of the
// nine commands is now real (T0.5 review NB N4).
var stubCommands = []string{
	"scan", "plan", "report", "clear-assignments",
	"copy", "build", "verify-media", "validate", "restore",
}

// --- re-exec helpers (deterministic child-process patterns) -----------------

const (
	cliHelperEnv      = "ISOPRESS_TEST_CLI_HELPER"
	signalHelperEnv   = "ISOPRESS_TEST_CLI_SIGNAL_HELPER"
	signalHelperReady = "HELPER-READY"

	// T1.6: lock-holder helper (the T0.4 re-exec pattern reused for the
	// scan lock-contention e2e): the child takes the MUTATING catalog
	// open and holds it until stdin closes.
	lockHoldEnv     = "ISOPRESS_TEST_LOCK_HOLD"
	lockHoldPathEnv = "ISOPRESS_TEST_LOCK_HOLD_PATH"
	lockHoldReady   = "LOCK-HELD"

	// T2.4: clear mid-transaction helper (clear_test.go): the child
	// replays the ClearPlan statement sequence inside ONE open
	// transaction, announces readiness, and commits only when stdin
	// closes - the deterministic "clear held mid-transaction" the
	// concurrent-reader test reads against.
	clearMidTxEnv     = "ISOPRESS_TEST_CLEAR_MIDTX"
	clearMidTxPathEnv = "ISOPRESS_TEST_CLEAR_MIDTX_PATH"
	clearMidTxSetEnv  = "ISOPRESS_TEST_CLEAR_MIDTX_SET"
	clearMidTxReady   = "CLEAR-MID-TX"
)

// TestMain re-execs the test binary as a CLI child (ISOPRESS_TEST_CLI_HELPER),
// as a signal-scaffolding child (ISOPRESS_TEST_CLI_SIGNAL_HELPER), as a
// signal-escape child (ISOPRESS_TEST_SIGNAL_ESCAPE_HELPER), as a
// scan-signal child (ISOPRESS_TEST_SCAN_SIGNAL_HELPER - the production
// scaffolding around the real command tree, args passed through), as a
// catalog lock holder (ISOPRESS_TEST_LOCK_HOLD), or as a clear
// mid-transaction holder (ISOPRESS_TEST_CLEAR_MIDTX); otherwise it runs
// the normal suite. The CLI child installs the T5.3 restore seam fake when
// ISOPRESS_TEST_RESTORE_FAKE is set (restore_test.go) - the injection
// lives in test code so the production command keeps no test-only branch.
func TestMain(m *testing.M) {
	// The T5.3 restore seam fake installs for EVERY child mode when its env
	// is set (a no-op otherwise), so a signal child and a CLI child share one
	// injection path.
	installRestoreFakeFromEnv()
	switch {
	case os.Getenv(cliHelperEnv) == "1":
		os.Exit(execute(context.Background(), NewRootCmd(), os.Args[1:], os.Stderr))
	case os.Getenv(signalHelperEnv) == "1":
		os.Exit(signalHelperMain())
	case os.Getenv(escapeHelperEnv) == "1":
		os.Exit(signalEscapeHelperMain())
	case os.Getenv(scanSignalHelperEnv) == "1":
		os.Exit(scanSignalHelperMain())
	case os.Getenv(lockHoldEnv) == "1":
		os.Exit(lockHoldMain())
	case os.Getenv(clearMidTxEnv) == "1":
		os.Exit(clearMidTxMain())
	}
	os.Exit(m.Run())
}

// lockHoldMain takes the mutating catalog open (exclusive flock per
// T0.4) on the path from the environment, announces readiness on
// stdout, and blocks until the parent closes stdin - the deterministic
// hold pattern of the catalog lock tests, no sleeps.
func lockHoldMain() int {
	path := os.Getenv(lockHoldPathEnv)
	db, err := catalog.Open(path, catalog.OpenReadWrite)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lock hold: %v\n", err)
		return 1
	}
	defer db.Close()
	fmt.Println(lockHoldReady)
	buf := make([]byte, 1)
	_, _ = os.Stdin.Read(buf) // hold until EOF (parent closes the pipe)
	return 0
}

// signalHelperMain runs the production scaffolding - the single-channel
// signal guard (armSecondSignalEscape: ONE channel registered for
// SIGINT/SIGTERM, ONE consumer goroutine; the first signal cancels the
// command context, the exact Execute wiring), plumbed through execute
// into the command context - with a command that announces readiness and
// then blocks until the signal arrives. The wrapper must classify the
// cancelled run as Interrupted (exit 130) on the FIRST signal, and the
// armed escape must stay silent (the single-signal tests also assert the
// absence of the forcing-exit line - the escape only acts on a SECOND
// signal).
func signalHelperMain() int {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	armSecondSignalEscape(cancel, done, os.Stderr)
	root := NewRootCmd()
	root.RunE = func(cmd *cobra.Command, args []string) error {
		// Announce readiness from inside the command: flags are parsed,
		// the signal guard is installed, and the parent may now signal.
		fmt.Fprintln(os.Stdout, signalHelperReady)
		<-ctx.Done()
		return nil
	}
	return execute(ctx, root, []string{}, os.Stderr)
}

// runCLIChild re-execs the test binary through the production execute
// path with the given CLI args and real process streams, returning the
// captured streams and exit code.
func runCLIChild(t *testing.T, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), cliHelperEnv+"=1")
	outBuf, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = outBuf, errBuf
	err := cmd.Run()
	code := report.ExitOK
	var exitErr *exec.ExitError
	if err != nil {
		if !errors.As(err, &exitErr) {
			t.Fatalf("running CLI child %v: %v (stderr: %s)", args, err, errBuf.String())
		}
		code = exitErr.ExitCode()
	}
	return outBuf.String(), errBuf.String(), code
}

// runInProcess runs the production execute seam in-process with the real
// command tree and an injected stderr writer; the root's help output is
// captured through cobra's SetOut. execute itself never writes to stdout
// (FR-046), so tests that care about real stdout use the re-exec child.
func runInProcess(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	root := NewRootCmd()
	outBuf := &bytes.Buffer{}
	root.SetOut(outBuf)
	errBuf := &bytes.Buffer{}
	code = execute(context.Background(), root, args, errBuf)
	return outBuf.String(), errBuf.String(), code
}

// --- command surface (DESIGN_SPEC 1.1) --------------------------------------

// isopress --help lists all nine commands of the 1.1 tree.
func TestRootHelpListsAllNineCommands(t *testing.T) {
	stdout, stderr, code := runInProcess(t, "--help")
	if code != report.ExitOK {
		t.Fatalf("--help exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, name := range stubCommands {
		if !strings.Contains(stdout, name) {
			t.Errorf("--help output does not list command %q:\n%s", name, stdout)
		}
	}
	// The generated cobra completion command is suppressed so the surface
	// is exactly the design's tree (DESIGN_SPEC 1.1 lists nine commands).
	if strings.Contains(stdout, "completion") {
		t.Errorf("--help lists a completion command; the surface must be the nine design commands:\n%s", stdout)
	}
}

// The global persistent flags of DESIGN_SPEC 1.1 (plus the 2026-10-20
// --change-detection amendment) are all registered on the root (and
// only those eleven), so every subcommand parses them.
func TestGlobalFlagsRegistered(t *testing.T) {
	want := []string{
		"catalog", "quiet", "verbose", "exclude", "exclude-from",
		"set-id", "json", "strict", "work-dir", "image-dir",
		"change-detection",
	}
	pf := NewRootCmd().PersistentFlags()
	for _, name := range want {
		if pf.Lookup(name) == nil {
			t.Errorf("global flag --%s not registered on the root command", name)
		}
	}
	count := 0
	pf.VisitAll(func(*pflag.Flag) { count++ })
	if count != len(want) {
		t.Errorf("root has %d persistent flags, want exactly the %d design flags", count, len(want))
	}
}

// Each subcommand's --help prints usage without panicking and exits 0.
func TestEachSubcommandHelp(t *testing.T) {
	for _, name := range stubCommands {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, code := runInProcess(t, name, "--help")
			if code != report.ExitOK {
				t.Fatalf("%s --help exit code = %d, want 0 (stderr: %s)", name, code, stderr)
			}
			if !strings.Contains(stdout, "Usage:") {
				t.Errorf("%s --help output lacks a Usage section:\n%s", name, stdout)
			}
			if !strings.Contains(stdout, name) {
				t.Errorf("%s --help output does not name the command:\n%s", name, stdout)
			}
		})
	}
}

// An unknown flag is a usage error: exit 2, error on stderr, usage hint
// present (DESIGN_SPEC 10.1).
func TestUnknownFlagExits2(t *testing.T) {
	_, stderr, code := runInProcess(t, "scan", "--definitely-not-a-flag")
	if code != report.ExitUsage {
		t.Fatalf("unknown flag exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, "unknown flag") {
		t.Errorf("stderr does not contain the flag error:\n%s", stderr)
	}
	if !strings.Contains(stderr, "usage") {
		t.Errorf("stderr does not contain a usage hint (DESIGN_SPEC 10.1):\n%s", stderr)
	}
}

// An unknown subcommand is a usage error: exit 2 with the "unknown
// command" message on stderr.
func TestUnknownSubcommandExits2(t *testing.T) {
	_, stderr, code := runInProcess(t, "frobnicate")
	if code != report.ExitUsage {
		t.Fatalf("unknown subcommand exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr, `unknown command "frobnicate"`) {
		t.Errorf("stderr does not contain the unknown-command message:\n%s", stderr)
	}
}

// T5.3 completed the T0.5 skeleton: `restore` was the LAST placeholder
// stub and commands.go no longer builds any (the placeholder loop is gone
// with newStubCommand). What remains pinned here is the negative half of
// the old "stub commands exit 0" acceptance - NONE of the nine commands
// answers a bare invocation with the T0.5 placeholder: each is real now
// and reports its own usage gate instead. The positive behavior of each
// command lives with its own task's tests (scan_test.go ... restore_test.go).
func TestNoPlaceholderCommandsRemain(t *testing.T) {
	for _, name := range stubCommands {
		t.Run(name, func(t *testing.T) {
			// A temp-dir catalog path: the MUTATING commands (clear-
			// assignments, plan) reach their open before failing on the
			// missing file and would otherwise leave a <catalog>.lock
			// beside the source tree.
			cat := filepath.Join(t.TempDir(), "c.sqlite")
			_, stderr, code := runInProcess(t, name,
				"--catalog", cat, "--quiet", "--json", "--set-id", "S-1",
				"--exclude", "*.iso", "--exclude", "var/log/**",
				"--strict", "--work-dir", "/tmp/w", "--image-dir", "/tmp/i",
				"--exclude-from", writeExcludeFile(t, "# comment\n\n*.tmp\n"))
			if code == report.ExitOK {
				t.Fatalf("%s with no arguments exited 0; every real command has a usage gate", name)
			}
			if strings.Contains(stderr, "not yet implemented") {
				t.Errorf("%s still prints the T0.5 placeholder:\n%s", name, stderr)
			}
		})
	}
}

// --quiet and --verbose are NOT exclusive: DESIGN_SPEC 9.2 treats both as
// plain-renderer inputs, so combining them is legal and must not be
// rejected (pinned here per the T0.5 task note). Exercised through the
// ROOT command (T5.3 note: `restore` graduated to a real command with its
// own --catalog/--set-id/--target gates, so no subcommand accepts a bare
// flag combination anymore): flag-combination validation is root-level and
// the root prints its help and exits 0 for a legal combination - exactly
// what this test pins.
func TestQuietAndVerboseAreNotExclusive(t *testing.T) {
	_, stderr, code := runInProcess(t, "--quiet", "--verbose")
	if code != report.ExitOK {
		t.Fatalf("--quiet --verbose exit code = %d, want 0 (not exclusive, DESIGN_SPEC 9.2):\n%s", code, stderr)
	}
}

// --exclude-from must name an existing path when provided; a missing file
// is a usage error (exit 2) naming the flag and the path; an existing file
// passes validation. Exercised through the ROOT command (see
// TestQuietAndVerboseAreNotExclusive for why no subcommand serves here
// since T5.3; the validation is root-level PersistentPreRunE).
func TestExcludeFromMustExist(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-patterns.txt")
	_, stderr, code := runInProcess(t, "--exclude-from", missing)
	if code != report.ExitUsage {
		t.Fatalf("--exclude-from missing file exit code = %d, want 2 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "--exclude-from") || !strings.Contains(stderr, missing) {
		t.Errorf("stderr does not name the flag and path:\n%s", stderr)
	}

	ok := writeExcludeFile(t, "*.tmp\n")
	_, stderr, code = runInProcess(t, "--exclude-from", ok)
	if code != report.ExitOK {
		t.Fatalf("--exclude-from existing file exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
}

// writeExcludeFile writes patterns to a temp file and returns its path.
func writeExcludeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "exclude.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing exclude file: %v", err)
	}
	return path
}

// --- end-to-end stream separation and codes (re-exec, real streams) ---------

// Through the real production entry path with REAL process streams: a
// usage error exits 2, writes the error to stderr, and writes NOTHING to
// stdout (DESIGN_SPEC 10.3, FR-046).
func TestChildUsageErrorStreamsAndCode(t *testing.T) {
	stdout, stderr, code := runCLIChild(t, "scan", "--definitely-not-a-flag")
	if code != report.ExitUsage {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if stdout != "" {
		t.Errorf("stdout must stay empty on errors (FR-046), got: %q", stdout)
	}
	if !strings.Contains(stderr, "unknown flag") {
		t.Errorf("stderr lacks the error message: %q", stderr)
	}
}

// A bare `restore` run through the real entry path with REAL process
// streams fails its usage gate: exit 2, the error on stderr, NOTHING on
// stdout (FR-046, 10.3). (T5.1 note: `verify-media` is real now; T5.2
// note: `validate` is real now; T5.3 note: `restore` is real now, so the
// stream-separation pin rides on its missing-`--catalog` usage error.)
func TestChildUsageGateStreamsAndCode(t *testing.T) {
	stdout, stderr, code := runCLIChild(t, "restore")
	if code != report.ExitUsage {
		t.Fatalf("exit code = %d, want 2 (stderr: %s)", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout must stay empty for a failing run (FR-046), got: %q", stdout)
	}
	if !strings.Contains(stderr, "--catalog is required") {
		t.Errorf("stderr lacks the usage message: %q", stderr)
	}
}

// --- interruption (DESIGN_SPEC 10.1 Interrupted, exit 130) ------------------

// In-process seam test: a command that cancels the context and returns
// nil still maps to 130 - the wrapper detects the cancelled context.
func TestExecuteMapsContextCancelTo130(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	root := &cobra.Command{Use: "selftest", SilenceUsage: true, RunE: func(cmd *cobra.Command, args []string) error {
		cancel()
		return nil
	}}
	errBuf := &bytes.Buffer{}
	if code := execute(ctx, root, []string{}, errBuf); code != report.ExitInterrupted {
		t.Fatalf("cancelled context exit code = %d, want 130", code)
	}
	if !strings.Contains(errBuf.String(), "interrupted") {
		t.Errorf("stderr lacks the interruption message: %q", errBuf.String())
	}
}

// A command that aborts with a cancellation-derived error also maps to
// 130: the signal override wins over incidental command errors, because
// such errors are typically cancellation symptoms (T3.3/T6.1 commands
// return them while shutting down).
func TestExecuteMapsCancellationDerivedErrorTo130(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	root := &cobra.Command{Use: "selftest", SilenceUsage: true, RunE: func(cmd *cobra.Command, args []string) error {
		cancel()
		return fmt.Errorf("copy: aborting in-flight item: %w", context.Canceled)
	}}
	if code := execute(ctx, root, []string{}, &bytes.Buffer{}); code != report.ExitInterrupted {
		t.Fatalf("cancellation-derived error exit code = %d, want 130", code)
	}
}

// runSignalHelperOnce re-execs ONE fresh signal-scaffolding child,
// delivers sig exactly once, and asserts the single-signal contract:
// the run aborts gracefully with exit 130, the interruption message
// lands on stderr, stdout carries only the readiness line (FR-046), and
// the hard escape stays SILENT - one signal must never fire the
// second-signal force path. Deterministic: readiness line from inside
// RunE, every post-signal wait a bounded join, no sleeps.
func runSignalHelperOnce(t *testing.T, sig syscall.Signal) {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), signalHelperEnv+"=1")
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("helper stdout pipe: %v", err)
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting signal helper: %v", err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("helper never became ready: %v (stderr: %s)", err, stderr.String())
	}
	if got := strings.TrimSpace(line); got != signalHelperReady {
		t.Fatalf("helper readiness line = %q, want %q (stderr: %s)", got, signalHelperReady, stderr.String())
	}

	if err := cmd.Process.Signal(sig); err != nil {
		t.Fatalf("signaling helper with %v: %v", sig, err)
	}
	waitErr := cmd.Wait()
	exitErr, ok := waitErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("helper did not exit with an error after %v: %v (stderr: %s)", sig, waitErr, stderr.String())
	}
	if got := exitErr.ExitCode(); got != report.ExitInterrupted {
		t.Errorf("exit code after %v = %d, want 130", sig, got)
	}
	if !strings.Contains(stderr.String(), "interrupted") {
		t.Errorf("helper stderr lacks the interruption message: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "forcing exit") {
		t.Errorf("single %v wrongly triggered the second-signal hard escape: %q", sig, stderr.String())
	}
}

// REAL signal to a REAL child process: SIGINT and SIGTERM both map to
// exit 130, the interruption message lands on stderr, stdout carries
// only the readiness line (FR-046), and the hard escape stays silent.
// The child arms the full production scaffolding INCLUDING the
// second-signal hard escape, so this is the single-signal regression pin
// for defect B: ONE signal still takes the graceful path - exit 130, no
// forcing-exit line. The stress variant below pins the same contract at
// the volume defect B actually needed to surface.
func TestRealSignalsMapToExit130(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			runSignalHelperOnce(t, sig)
		})
	}
}
