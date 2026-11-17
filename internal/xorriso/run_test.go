package xorriso

// T4.1 exec-wrapper tests (DESIGN_SPEC 6.2 exit handling, 1.2 external
// integration layer): success and failure classification against fake
// xorriso scripts, context cancellation, the end-to-end ARGV INJECTION
// proof (a fake xorriso dumps "$@" to a file; a hostile VolumeID arrives
// as one verbatim element and nothing is ever evaluated), and direct
// unit tests of the capped tail buffer that backs output capture.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// TestRunSuccessCapturesBothStreams: exit 0 yields a Result carrying the
// combined stdout+stderr transcript, exact byte accounting, and a
// positive duration.
func TestRunSuccessCapturesBothStreams(t *testing.T) {
	path, _ := installFakeXorriso(t, `
echo "progress: writing image"
echo "warning: something minor" >&2
exit 0
`)

	res, err := (&Tool{Path: path}).Run(context.Background(), []string{"-as", "mkisofs"})
	if err != nil {
		t.Fatalf("Run on exit 0: %v", err)
	}
	out := string(res.Output)
	for _, want := range []string{"progress: writing image", "warning: something minor"} {
		if !strings.Contains(out, want) {
			t.Errorf("Result.Output %q does not contain %q", out, want)
		}
	}
	if res.Truncated {
		t.Error("Result.Truncated = true for tiny output, want false")
	}
	if res.TotalBytes != int64(len(res.Output)) {
		t.Errorf("Result.TotalBytes = %d, want len(Output) = %d", res.TotalBytes, len(res.Output))
	}
	if res.Duration <= 0 {
		t.Errorf("Result.Duration = %v, want a positive duration", res.Duration)
	}
}

// A non-zero exit is a typed BuildError carrying the exact exit code and
// the captured output tail (T4.1 acceptance "failure (exit 9, emits
// stderr content -> BuildError carries code + output)").
func TestRunNonZeroExitIsBuildErrorWithCodeAndOutput(t *testing.T) {
	path, _ := installFakeXorriso(t, `
echo "fatal: cannot open staging tree"
exit 9
`)

	res, err := (&Tool{Path: path}).Run(context.Background(), []string{"-as", "mkisofs"})
	if err == nil {
		t.Fatal("Run on exit 9 succeeded, want a BuildError")
	}
	var buildErr *BuildError
	if !errors.As(err, &buildErr) {
		t.Fatalf("err = %v (%T), want a *BuildError", err, err)
	}
	if buildErr.ExitCode != 9 {
		t.Errorf("BuildError.ExitCode = %d, want 9", buildErr.ExitCode)
	}
	if !strings.Contains(string(buildErr.Result.Output), "fatal: cannot open staging tree") {
		t.Errorf("BuildError.Result.Output %q does not carry the stderr content", buildErr.Result.Output)
	}
	// The Result returned alongside the error carries the same tail.
	if !strings.Contains(string(res.Output), "fatal: cannot open staging tree") {
		t.Errorf("returned Result.Output %q does not carry the stderr content", res.Output)
	}
	// The printable message names the status and a bounded tail.
	msg := err.Error()
	if !strings.Contains(msg, "9") || !strings.Contains(msg, "fatal: cannot open staging tree") {
		t.Errorf("BuildError message %q lacks the exit code or output tail", msg)
	}
	// Unwrap reaches the *exec.ExitError.
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 9 {
		t.Errorf("errors.As(*exec.ExitError) on BuildError failed or wrong code: %v", err)
	}
}

// Context cancellation stops a long-running build promptly and surfaces
// the raw context error (Interrupted class mapping upstream), not a
// BuildError.
func TestRunRespectsCallerCancellation(t *testing.T) {
	path, _ := installFakeXorriso(t, `exec sleep 30`)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := (&Tool{Path: path}).Run(ctx, []string{"-as", "mkisofs"})
	elapsed := time.Since(start)
	if elapsed >= 10*time.Second {
		t.Fatalf("Run ignored cancellation for %v", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	var buildErr *BuildError
	if errors.As(err, &buildErr) {
		t.Fatal("cancellation must not classify as BuildError")
	}
}

// THE PROCESS MUST ACTUALLY DIE, not just stop being waited on: the
// fake records its PID, then execs into a long sleep. After Run returns
// the context error, the PID must become signal-invisible (ESRCH) -
// exec.CommandContext killed it AND Run reaped it. Checked under -race
// by the standard race run of the suite.
func TestRunCancellationKillsAndReapsTheChild(t *testing.T) {
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "pid")
	path, _ := installFakeXorriso(t, `
echo $$ > "`+pidfile+`"
exec sleep 300
`)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	if _, err := (&Tool{Path: path}).Run(ctx, []string{"-as", "mkisofs"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run err = %v, want context.Canceled", err)
	}

	raw, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse pid %q: %v", raw, err)
	}

	// The child is reaped by the time Run returns, so kill(pid, 0)
	// must report ESRCH immediately; poll briefly to absorb any
	// scheduler slack on a loaded machine.
	deadline := time.Now().Add(5 * time.Second)
	for {
		proc, err := os.FindProcess(pid)
		sigErr := proc.Signal(syscall.Signal(0))
		if errors.Is(sigErr, os.ErrProcessDone) || errors.Is(sigErr, syscall.ESRCH) {
			break // process is gone and reaped
		}
		if time.Now().After(deadline) {
			t.Fatalf("child pid %d still alive after cancellation (signal err = %v, find err = %v)", pid, sigErr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A context cancelled BEFORE the run starts never spawns a process and
// returns the context error promptly - no BuildError, no start-up error
// confusion.
func TestRunWithPreCancelledContextReturnsPromptly(t *testing.T) {
	path, _ := installFakeXorriso(t, `echo "must not run"; exit 0`)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := (&Tool{Path: path}).Run(ctx, []string{"-as", "mkisofs"})
	elapsed := time.Since(start)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed >= 10*time.Second {
		t.Fatalf("pre-cancelled Run took %v", elapsed)
	}
}

// Cancelling AFTER a successful completion is a no-op for the finished
// run (its Result and nil error are already final), and calling cancel
// TWICE is safe. The next run on the now-cancelled context behaves like
// a pre-cancelled one.
func TestRunCancelAfterCompletionAndDoubleCancel(t *testing.T) {
	path, _ := installFakeXorriso(t, `echo "ok"; exit 0`)

	ctx, cancel := context.WithCancel(context.Background())
	res, err := (&Tool{Path: path}).Run(ctx, []string{"-as", "mkisofs"})
	if err != nil || !strings.Contains(string(res.Output), "ok") {
		t.Fatalf("baseline run: res=%q err=%v", res.Output, err)
	}

	cancel()
	cancel() // double-cancel must not panic or corrupt anything

	// The completed run's outcome does not retroactively change.
	if err != nil {
		t.Fatalf("completed run reported a spurious error after late cancellation: %v", err)
	}
	// The next run on the cancelled context is refused as cancelled.
	if _, err := (&Tool{Path: path}).Run(ctx, []string{"-as", "mkisofs"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("run on a cancelled context: err = %v, want context.Canceled", err)
	}
}

// CONCURRENT RUNS ON ONE SHARED TOOL must not cross-contaminate: the
// tail buffer is allocated PER Run (run.go), so two interleaved builds
// each see only their own output. Run under -race by the suite; the
// distinct long markers make any interleaving into the wrong Result
// visible.
func TestRunConcurrentRunsOnSharedToolDoNotCrossContaminate(t *testing.T) {
	// The fake echoes its single argument 200 times, producing two
	// easily distinguishable multi-KiB streams.
	path, _ := installFakeXorriso(t, `
i=0
while [ $i -lt 200 ]; do echo "$1"; i=$((i+1)); done
exit 0
`)
	tool := &Tool{Path: path}

	const markerA = "AAAA-UNIQUE-STREAM-MARKER"
	const markerB = "BBBB-UNIQUE-STREAM-MARKER"
	const markerC = "CCCC-UNIQUE-STREAM-MARKER"
	markers := []string{markerA, markerB, markerC}

	var wg sync.WaitGroup
	errs := make([]error, len(markers))
	outputs := make([]string, len(markers))
	for i, m := range markers {
		wg.Add(1)
		go func(i int, m string) {
			defer wg.Done()
			for round := 0; round < 10; round++ {
				res, err := tool.Run(context.Background(), []string{m})
				if err != nil {
					errs[i] = err
					return
				}
				out := string(res.Output)
				if strings.Contains(out, "UNIQUE-STREAM-MARKER") && !strings.Contains(out, m) {
					errs[i] = fmt.Errorf("run %d saw foreign marker output: %.80q", i, out)
					return
				}
				outputs[i] = out
			}
		}(i, m)
	}
	wg.Wait()

	for i, m := range markers {
		if errs[i] != nil {
			t.Errorf("goroutine %d (%s): %v", i, m, errs[i])
			continue
		}
		if got := strings.Count(outputs[i], m); got != 200 {
			t.Errorf("goroutine %d output has %d occurrences of its own marker, want 200 (cross-contamination?)", i, got)
		}
		for _, other := range markers {
			if other != m && strings.Contains(outputs[i], other) {
				t.Errorf("goroutine %d output contains foreign marker %s", i, other)
			}
		}
	}
}

// END-TO-END PATH PASS-THROUGH: OutputPath and PathListPath containing
// spaces, tabs, and NEWLINES reach the child as single argv elements,
// byte-for-byte verbatim. The fake dumps each argv element to its own
// file (newline-safe) and asserts the flags it was invoked with.
func TestRunPathsWithWhitespaceArriveAsSingleArgvElements(t *testing.T) {
	dir := t.TempDir()
	argvDir := filepath.Join(dir, "argv")
	if err := os.Mkdir(argvDir, 0o755); err != nil {
		t.Fatalf("mkdir argvDir: %v", err)
	}
	path, _ := installFakeXorriso(t, `
i=0
for a in "$@"; do
  printf '%s' "$a" > "`+argvDir+`/arg.$i"
  i=$((i+1))
done
exit 0
`)

	out := filepath.Join(dir, "my backup (final)")
	out += "\nnewline.iso.partial" // a filename with a real newline
	list := filepath.Join(dir, "path list\ttab.txt")
	inv := Invocation{
		VolumeID:     "SET-20261018",
		OutputPath:   out,
		PathListPath: list,
		Joliet:       false,
	}
	argv, err := inv.Argv()
	if err != nil {
		t.Fatalf("Argv: %v", err)
	}

	if _, err := (&Tool{Path: path}).Run(context.Background(), argv); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Read each dumped element back and compare against the exact argv.
	for i, want := range argv {
		got, err := os.ReadFile(filepath.Join(argvDir, "arg."+strconv.Itoa(i)))
		if err != nil {
			t.Fatalf("read dumped argv element %d: %v", i, err)
		}
		if string(got) != want {
			t.Errorf("argv[%d] at the child = %q, want %q (verbatim, unsplit)", i, got, want)
		}
	}
	// And the newline-carrying -o value arrived as ONE element (its
	// file contains the embedded newline, and no extra element was
	// written).
	if _, err := os.Stat(filepath.Join(argvDir, "arg."+strconv.Itoa(len(argv)))); !os.IsNotExist(err) {
		t.Fatalf("child received MORE argv elements than built (%d); a path was split", len(argv))
	}
}

// A Tool without a discovered path refuses to run (wiring-bug guard).
func TestRunRejectsEmptyPath(t *testing.T) {
	if _, err := new(Tool).Run(context.Background(), []string{"-version"}); err == nil {
		t.Fatal("Run with an empty Tool.Path succeeded, want an error")
	}
}

// END-TO-END INJECTION PROOF: the fake xorriso writes each argv element
// it received, one per line, to a file named by an environment variable.
// A hostile VolumeID (command substitution, semicolon, rm, comments,
// quotes, globs) arrives as exactly ONE verbatim line, and the
// substitution payloads execute NOTHING: no "pwned" marker file appears
// anywhere. This pins the structural claim that no shell ever parses the
// argument vector.
func TestRunNoArgumentInjectionEndToEnd(t *testing.T) {
	dir := t.TempDir()
	argsOut := filepath.Join(dir, "args.txt")
	path, fakeDir := installFakeXorriso(t, `
printf '%s\n' "$@" > "`+argsOut+`"
echo "xorriso 1.5.6 : fake mastering run"
exit 0
`)

	hostile := `SET-$(touch ` + fakeDir + `/pwned);rm -rf /#" & 'quoted'`
	inv := Invocation{
		VolumeID:     hostile,
		OutputPath:   filepath.Join(dir, "out.iso.partial"),
		PathListPath: filepath.Join(dir, "pathlist.txt"),
		Joliet:       true,
	}
	argv, err := inv.Argv()
	if err != nil {
		t.Fatalf("Argv: %v", err)
	}

	res, err := (&Tool{Path: path}).Run(context.Background(), argv)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(string(res.Output), "fake mastering run") {
		t.Errorf("output %q lacks the fake's banner", res.Output)
	}

	dumped, err := os.ReadFile(argsOut)
	if err != nil {
		t.Fatalf("read dumped argv: %v", err)
	}
	got := strings.Split(strings.TrimRight(string(dumped), "\n"), "\n")
	if len(got) != len(argv) {
		t.Fatalf("fake xorriso received %d argv elements %q, want %d %q", len(got), got, len(argv), argv)
	}
	for i := range argv {
		if got[i] != argv[i] {
			t.Errorf("argv[%d] = %q, want %q (verbatim, unexpanded)", i, got[i], argv[i])
		}
	}

	// The command substitution would have created this file had a
	// shell ever evaluated the VolumeID.
	if _, err := os.Stat(filepath.Join(fakeDir, "pwned")); !os.IsNotExist(err) {
		t.Fatalf("command substitution in VolumeID was EVALUATED (stat err = %v); the wrapper invoked a shell", err)
	}
}

// --- tail buffer (capped capture) ------------------------------------------

func TestTailBufferRetainsEverythingUnderCapacity(t *testing.T) {
	b := newTailBuffer(64)
	if _, err := b.Write([]byte("0123456789")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	data, total, truncated := b.snapshot()
	if string(data) != "0123456789" || total != 10 || truncated {
		t.Fatalf("snapshot = (%q, %d, %v), want (0123456789, 10, false)", data, total, truncated)
	}
}

// Overflowing writes keep the LAST capacity bytes in chronological
// order, with the true total and the truncation flag reported.
func TestTailBufferKeepsTailInOrderAcrossWraps(t *testing.T) {
	b := newTailBuffer(10)
	writes := []string{"01234", "56789", "abcde", "fghij"}
	for _, w := range writes {
		if _, err := b.Write([]byte(w)); err != nil {
			t.Fatalf("Write(%q): %v", w, err)
		}
	}
	data, total, truncated := b.snapshot()
	// Full stream is "0123456789abcdefghij"; the tail is its last 10.
	if string(data) != "abcdefghij" {
		t.Fatalf("tail = %q, want %q", data, "abcdefghij")
	}
	if total != 20 || !truncated {
		t.Fatalf("total = %d, truncated = %v, want (20, true)", total, truncated)
	}
}

// A single write larger than the window replaces it wholesale: only its
// own last capacity bytes survive. A subsequent small write wraps into
// the front of the ring and the snapshot stays chronological.
func TestTailBufferWindowReplacement(t *testing.T) {
	b := newTailBuffer(10)
	if _, err := b.Write([]byte("abcdefghijklmnopqrst")); err != nil { // 20 bytes
		t.Fatalf("Write: %v", err)
	}
	data, total, truncated := b.snapshot()
	if string(data) != "klmnopqrst" || total != 20 || !truncated {
		t.Fatalf("snapshot = (%q, %d, %v), want (klmnopqrst, 20, true)", data, total, truncated)
	}

	// "UV" overwrites the two oldest window bytes (k, l) at the front
	// of the ring; the chronological tail becomes "mnopqrstUV".
	if _, err := b.Write([]byte("UV")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	data, total, truncated = b.snapshot()
	if string(data) != "mnopqrstUV" || total != 22 || !truncated {
		t.Fatalf("post-wrap snapshot = (%q, %d, %v), want (mnopqrstUV, 22, true)", data, total, truncated)
	}
}

// The truncation marker is prepended to the retained tail with the
// dropped and total byte counts spelled out.
func TestTruncationMarkerNamesDroppedAndTotalBytes(t *testing.T) {
	tail := []byte("last bytes")
	marker := truncationMarker(len(tail), 12345)
	if !strings.HasPrefix(marker, "[xorriso output truncated:") ||
		!strings.Contains(marker, "10 of 12345 bytes") {
		t.Fatalf("marker %q lacks the dropped/total byte counts", marker)
	}
	if !strings.HasSuffix(marker, "\n") {
		t.Errorf("marker %q should end with a newline", marker)
	}
}
