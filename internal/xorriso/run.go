package xorriso

// The xorriso exec wrapper (T4.1, DESIGN_SPEC 6.2 exit-code handling,
// 1.2 external integration layer).
//
// This is the ONLY place in the tool that executes a process
// (DESIGN_SPEC 1.2: services depend on interfaces, never on os/exec
// directly; the deps test asserts no other package imports os/exec).
// Execution is exec.CommandContext with a direct argv vector - no shell,
// ever - so argument content can never change the command's shape.

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// MaxCapturedOutput bounds the combined stdout+stderr bytes retained in
// memory per xorriso invocation (10 MiB). xorriso's default verbosity is
// a few KiB per build, so the cap only engages for pathological output
// (for example a -debug storm or an error loop); it exists so a runaway
// child cannot exhaust the tool's memory. The TAIL is what gets kept
// because xorriso prints failure diagnostics at the END of its output;
// when anything is dropped, Result.Output starts with an explicit
// truncation marker carrying the dropped and total byte counts.
const MaxCapturedOutput = 10 << 20 // 10 MiB

// errorOutputPrintLimit bounds what BuildError.Error() renders on
// stderr. The full capped transcript stays available on Result.Output
// for the run_events log (DESIGN_SPEC 6.2: "capture stdout+stderr to the
// run log ... plus full transcript under --verbose on stderr").
const errorOutputPrintLimit = 8 << 10 // 8 KiB

// Result is the outcome of one successful xorriso run (exit 0). It is
// also attached to BuildError on non-zero exits so the captured output
// and duration are reachable from the failure path too.
type Result struct {
	// Output is the combined stdout+stderr, capped at
	// MaxCapturedOutput bytes of tail (see Truncated).
	Output []byte
	// Truncated reports whether output beyond the tail cap was
	// dropped; Output then begins with a marker naming the dropped
	// and total byte counts.
	Truncated bool
	// TotalBytes is the number of bytes xorriso wrote across both
	// streams, including any dropped to stay under the cap.
	TotalBytes int64
	// Duration is the wall-clock time the process ran.
	Duration time.Duration
}

// BuildError is the typed failure of one xorriso invocation that STARTED
// but exited non-zero (DESIGN_SPEC 6.2: "xorriso exit 0 = success;
// anything else = build failure"). It carries the exit code and the
// captured output tail the failure report needs. Services wrap it into
// their per-item failure accounting (exit 10 class) or map it to a
// fatal build error; report.Classify intentionally has no BuildError
// row - classification beyond exit-0-vs-not belongs to the calling
// service.
type BuildError struct {
	// ExitCode is the process exit status (always non-zero here).
	ExitCode int
	// Result carries the captured output tail, truncation info, and
	// duration of the failed run.
	Result Result
	// Err is the underlying *exec.ExitError, unwrappable for
	// errors.Is/As.
	Err error
}

func (e *BuildError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "xorriso build failed: exited with status %d", e.ExitCode)
	tail := e.Result.Output
	if len(tail) > errorOutputPrintLimit {
		omitted := len(tail) - errorOutputPrintLimit
		fmt.Fprintf(&b, " [... %d earlier output bytes omitted ...]", omitted)
		tail = tail[omitted:]
	}
	if s := strings.TrimSpace(string(tail)); s != "" {
		fmt.Fprintf(&b, "; output tail:\n%s", s)
	}
	return b.String()
}

// Unwrap exposes the underlying *exec.ExitError to errors.Is/As.
func (e *BuildError) Unwrap() error { return e.Err }

// Run executes xorriso with the given argument vector and captures its
// combined output. Exit classification (DESIGN_SPEC 6.2):
//
//   - exit 0: the Result carries the captured output and duration;
//   - non-zero: a typed *BuildError with the exit code and the output
//     tail (the Result is attached to it as well);
//   - context cancellation or deadline: the raw context error (so the
//     Interrupted class maps to exit 130, and a probe deadline surfaces
//     as DeadlineExceeded);
//   - failure to start the process (binary vanished, not executable):
//     the plain exec error.
//
// Cancellation kills just the xorriso process (exec.CommandContext's
// default Cancel behavior). Documented assumption per T4.1: xorriso in
// `-as mkisofs` emulation mode does not spawn child processes - it is a
// single process writing one image file - so killing that process
// suffices; no process-group kill is needed. No internal timeout is
// applied: builds are long-running by nature and the CALLER's context
// governs their lifetime (only the version probe is time-bounded, see
// ProbeTimeout).
func (t *Tool) Run(ctx context.Context, args []string) (Result, error) {
	if t == nil || t.Path == "" {
		return Result{}, fmt.Errorf("xorriso: Tool.Path is empty; call Discover before Run")
	}

	buf := newTailBuffer(MaxCapturedOutput)
	cmd := exec.CommandContext(ctx, t.Path, args...)
	cmd.Stdout = buf
	cmd.Stderr = buf

	start := time.Now()
	runErr := cmd.Run()
	duration := time.Since(start)

	data, total, truncated := buf.snapshot()
	if truncated {
		data = append([]byte(truncationMarker(len(data), total)), data...)
	}
	res := Result{Output: data, Truncated: truncated, TotalBytes: total, Duration: duration}

	if runErr == nil {
		return res, nil
	}
	// Cancellation wins over the kill-induced ExitError ("signal:
	// killed"): the reason the process died is the context, and that
	// is the error callers must see.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return res, ctxErr
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return res, &BuildError{ExitCode: exitErr.ExitCode(), Result: res, Err: runErr}
	}
	// Start-up failure: exec.LookPath-level problem that occurred at
	// spawn time. Not a build failure (no exit code exists).
	return res, runErr
}

// truncationMarker renders the marker prepended to a truncated Output:
// it names how many tail bytes are shown and how many bytes xorriso
// wrote in total, so a reader of the transcript knows data was dropped.
func truncationMarker(shown int, total int64) string {
	return fmt.Sprintf("[xorriso output truncated: showing the last %d of %d bytes]\n", shown, total)
}

// tailBuffer is a capped writer that retains the LAST capacity bytes
// written to it plus the total byte count. stdout and stderr both point
// at one tailBuffer; os/exec serializes writes to a shared writer
// (identical Stdout/Stderr share one pipe), and the mutex makes that
// guarantee explicit rather than relied-upon.
type tailBuffer struct {
	mu    sync.Mutex
	ring  []byte // fixed-size window; valid bytes start at head when full
	head  int    // next write position; index of the oldest held byte when full
	total int64  // total bytes ever written, including dropped ones
}

// newTailBuffer returns a tailBuffer retaining the last capacity bytes.
func newTailBuffer(capacity int) *tailBuffer {
	return &tailBuffer{ring: make([]byte, capacity)}
}

// Write appends p, evicting the oldest bytes beyond the capacity. It
// never fails and always reports a full write (io.Writer contract for a
// memory sink).
func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.total += int64(len(p))
	size := len(b.ring)
	if size == 0 {
		return len(p), nil
	}
	if len(p) >= size {
		// p alone fills or overflows the window: only its last
		// `size` bytes can survive.
		copy(b.ring, p[len(p)-size:])
		b.head = 0
		return len(p), nil
	}
	n := copy(b.ring[b.head:], p)
	if n < len(p) {
		copy(b.ring, p[n:]) // wrap into the front of the ring
	}
	b.head = (b.head + len(p)) % size
	return len(p), nil
}

// snapshot returns the retained tail in chronological order, the total
// byte count ever written, and whether anything was dropped.
func (b *tailBuffer) snapshot() (data []byte, total int64, truncated bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	size := len(b.ring)
	total = b.total
	n := int(total)
	if n > size {
		n = size
	}
	truncated = total > int64(size)
	if n <= 0 {
		return nil, total, truncated
	}

	out := make([]byte, n)
	if n < size {
		// Never wrapped: everything held lives at the front of the
		// ring in order.
		copy(out, b.ring[:n])
		return out, total, truncated
	}
	// Full window: chronological order starts at head (the oldest
	// held byte) and wraps to the front.
	copy(out, b.ring[b.head:])
	copy(out[size-b.head:], b.ring[:b.head])
	return out, total, truncated
}
