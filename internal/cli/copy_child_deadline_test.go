package cli

// DEADLINE-GUARDED CHILD RUNNER for the one pin that can block.
//
// The bounded-manifest FIFO subtest of TestCopyWipeManifestReadIsBounded
// plants a FIFO at <imageRoot>/MANIFEST.json with NO writer attached.
// Today's code Lstats first and refuses without opening, so the child
// returns in milliseconds. If a future edit drops that pre-check the
// child blocks in open(2) forever - and a plain runCopyChild would
// block the PARENT too, so the suite would burn its whole package
// timeout (30m in the release gate) before failing with a timeout
// panic instead of a named assertion.
//
// Verified in the U.4 verification round: with the Lstat pre-check
// removed, the unguarded subtest hung for the full -timeout. This
// helper converts that regression into a fast, self-explanatory
// failure (kill the child, name the likely cause). The deadline is a
// watchdog for a child that should finish in well under a second; it
// is deliberately generous (60s) so a loaded -race box cannot make it
// flaky, and it is used ONLY by the FIFO subtest - every other child
// run keeps the unbounded helper.

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"testing"
	"time"
)

// fifoRefusalWatchdog bounds the FIFO subtest's copy child.
const fifoRefusalWatchdog = 60 * time.Second

// runCopyChildDeadline is runCopyChild with a wall-clock deadline: the
// child is killed when it exceeds limit, and a kill is reported as a
// watchdog failure naming the blocking suspect rather than as a bare
// exit-code mismatch.
func runCopyChildDeadline(t *testing.T, limit time.Duration, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), limit)
	defer cancel()

	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	cmd.Env = append(os.Environ(), cliHelperEnv+"=1")
	outBuf, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = outBuf, errBuf
	err := cmd.Run()

	if ctx.Err() != nil {
		// The deadline - not the exit code - is the story: the child
		// never finished. The only unbounded open in this path is the
		// planted FIFO at MANIFEST.json.
		t.Fatalf("copy child killed after %v: it was still blocked - the MANIFEST.json "+
			"read is almost certainly opening the planted FIFO (the Lstat-before-open "+
			"pre-check is gone or not reached)\nstdout: %s\nstderr: %s",
			limit, outBuf.String(), errBuf.String())
	}
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("running deadline-guarded copy child %v: %v (stderr: %s)", args, err, errBuf.String())
		}
		code = exitErr.ExitCode()
	}
	return outBuf.String(), errBuf.String(), code
}
