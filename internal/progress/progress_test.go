package progress

// T1.1 tests: the Bus (emit path, overflow policy, guaranteed terminal
// delivery), the Run/Start lifecycle, the interruption-signaling design
// (Done.Interrupted), the DESIGN_SPEC 10.3 / FR-046 stdout guarantee,
// and the silent renderer. Plain-format and cadence tests live in
// plain_test.go; the 9.2 selection matrix in renderer_test.go; the
// DESIGN_SPEC 11 dependency conditions in deps_test.go.

import (
	"bytes"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeClock is the injectable-clock seam (DESIGN_SPEC 12.1:
// "cadence throttling with a fake clock"). It is mutex-guarded because
// the renderer goroutine reads Now() while the test goroutine advances
// the clock; -race would flag an unsynchronized pair.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// lockedWriter is a bytes.Buffer guarded for cross-goroutine use: the
// renderer goroutine writes while the test goroutine reads only after
// wait() returns, but the interactive renderer's spinner ticker writes
// from its own goroutine, so the lock is required under -race.
type lockedWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *lockedWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func (w *lockedWriter) Len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Len()
}

// lines splits rendered output into non-empty lines.
func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// TestEmitNeverBlocksAndDropsWithCounter pins the documented overflow
// policy: Emit never blocks the producer unboundedly; a full channel
// drops the event and increments Dropped. The test drives ChannelBuffer
// successful emits plus 100 more with NO consumer; completion of the
// producer goroutine within the timeout is the non-blocking proof.
func TestEmitNeverBlocksAndDropsWithCounter(t *testing.T) {
	bus := NewBus()
	produced := make(chan struct{})
	go func() {
		defer close(produced)
		for i := 0; i < ChannelBuffer+100; i++ {
			bus.Emit(Tick{})
		}
	}()
	select {
	case <-produced:
	case <-time.After(30 * time.Second):
		t.Fatal("Emit blocked the producer; the emit path must never block unboundedly")
	}
	if got := bus.Dropped(); got != 100 {
		t.Errorf("Dropped() = %d, want exactly 100 (the 100 emits beyond the %d-slot buffer)",
			got, ChannelBuffer)
	}
}

// TestFinishDeliversThroughFullBuffer: the terminal Done event survives
// a completely full bus - Finish blocks until the (just-started)
// renderer drains the buffer, and the summary line lands exactly once.
// This is the guarantee that makes drop-with-counter safe: only
// advisory incremental events are droppable, never the final summary.
func TestFinishDeliversThroughFullBuffer(t *testing.T) {
	w := &lockedWriter{}
	bus := NewBus()
	bus.Emit(Phase{Name: "copy"})
	for i := 0; i < ChannelBuffer; i++ {
		bus.Emit(Tick{})
	}
	wait := Start(New(Options{Mode: ModePlain, Stderr: w, Now: newFakeClock().Now}), bus)
	bus.Finish(Done{Summary: "copy: complete"})
	final := wait()

	if final.Summary != "copy: complete" {
		t.Errorf("Run returned Done %+v, want Summary %q", final, "copy: complete")
	}
	got := lines(w.String())
	if len(got) == 0 || got[len(got)-1] != "copy: complete" {
		t.Errorf("final summary line missing from output:\n%q", w.String())
	}
	if n := strings.Count(w.String(), "copy: complete"); n != 1 {
		t.Errorf("summary printed %d times, want exactly 1", n)
	}
}

// TestInterruptionSignalsThroughDoneFlag documents and pins the
// interruption-signaling design: Done{Interrupted: true} is the sole
// interruption signal at the progress layer, Run returns it faithfully,
// and the plain renderer prints exactly ONE interruption summary line
// (DESIGN_SPEC 9.3: "and one at interruption").
func TestInterruptionSignalsThroughDoneFlag(t *testing.T) {
	w := &lockedWriter{}
	bus := NewBus()
	wait := Start(New(Options{Mode: ModePlain, Stderr: w, Now: newFakeClock().Now}), bus)

	bus.Emit(Phase{Name: "copy", TotalKnown: true, TotalFiles: 100, TotalBytes: 100})
	bus.Emit(ItemDone{Bytes: 10, Path: "a"})
	bus.Finish(Done{Summary: "interrupted: copy stopped by signal", Interrupted: true})
	final := wait()

	if !final.Interrupted {
		t.Error("Run returned Interrupted=false, want true")
	}
	got := lines(w.String())
	if len(got) != 2 {
		t.Fatalf("interrupted run produced %d lines, want exactly 2 (one progress + one interruption summary):\n%q",
			len(got), w.String())
	}
	if got[1] != "interrupted: copy stopped by signal" {
		t.Errorf("interruption summary line = %q, want %q", got[1], "interrupted: copy stopped by signal")
	}
}

// TestServiceOperationWritesNothingToStdout is the DESIGN_SPEC 10.3 /
// FR-046 acceptance test (IMPLEMENTATION_PLAN T1.1): a service-style
// operation emitting the full event stream through a live Bus and a
// plain renderer writes NOTHING to stdout. os.Stdout is swapped for a
// pipe for the duration; the stderr-side buffer is asserted non-empty
// so the test cannot pass vacuously.
func TestServiceOperationWritesNothingToStdout(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	old := os.Stdout
	os.Stdout = pr
	defer func() {
		os.Stdout = old
		pr.Close()
	}()

	w := &lockedWriter{}
	bus := NewBus()
	wait := Start(New(Options{
		Quiet:      false,
		IsTerminal: func() bool { return false }, // non-TTY -> plain (9.2 matrix)
		Stderr:     w,
	}), bus)

	// A service-style operation: phase, items, heartbeat, warn, error,
	// completion. Exactly what ingest/copy services will emit (T1.6+).
	bus.Emit(Phase{Name: "scan(hash)"})
	for i := 0; i < 50; i++ {
		bus.Emit(ItemDone{Bytes: 1024, Path: "some/file"})
	}
	bus.Emit(Tick{})
	bus.Emit(Warn{Msg: "skipping special file: /tmp/fifo"})
	bus.Emit(Error{Msg: "unreadable: /tmp/secret (permission denied)"})
	bus.Finish(Done{Summary: "scan: complete, 50 files"})
	wait()

	os.Stdout = old
	pw.Close()
	n, err := io.Copy(io.Discard, pr)
	if err != nil {
		t.Fatalf("reading captured stdout: %v", err)
	}
	if n != 0 {
		t.Errorf("progress pipeline wrote %d bytes to stdout, want 0 (FR-046)", n)
	}
	if !strings.Contains(w.String(), "scan: complete, 50 files") {
		t.Errorf("renderer produced no summary on its stderr writer (vacuous test?):\n%q", w.String())
	}
}

// TestSilentRendererWritesZeroBytes: a full event stream - determinate
// and indeterminate phases, items, ticks, warnings, errors, and a
// Done-with-interruption - produces ZERO bytes of output, while Run
// still returns the terminal state faithfully.
func TestSilentRendererWritesZeroBytes(t *testing.T) {
	w := &lockedWriter{}
	bus := NewBus()
	wait := Start(New(Options{Mode: ModeSilent, Stderr: w}), bus)

	bus.Emit(Phase{Name: "copy", TotalKnown: true, TotalFiles: 3, TotalBytes: 300})
	bus.Emit(ItemDone{Bytes: 100, Path: "a"})
	bus.Emit(Phase{Name: "build"}) // indeterminate
	bus.Emit(ItemDone{Bytes: 0, Path: "image1.iso"})
	bus.Emit(Tick{})
	bus.Emit(Warn{Msg: "a warning"})
	bus.Emit(Error{Msg: "an error"})
	bus.Finish(Done{Summary: "silent but observed", Interrupted: true})
	final := wait()

	if w.Len() != 0 {
		t.Errorf("silent renderer wrote %d bytes, want 0; output: %q", w.Len(), w.String())
	}
	if final.Summary != "silent but observed" || !final.Interrupted {
		t.Errorf("silent renderer returned %+v, want the Done event verbatim", final)
	}
}

// TestBusEmitOfDoneUsesGuaranteedPath: Emit(Done{...}) must not be
// droppable - it routes through the Finish guarantee so the
// exactly-one-summary contract holds regardless of entry point.
func TestBusEmitOfDoneUsesGuaranteedPath(t *testing.T) {
	w := &lockedWriter{}
	bus := NewBus()
	wait := Start(New(Options{Mode: ModePlain, Stderr: w, Now: newFakeClock().Now}), bus)
	bus.Emit(Done{Summary: "via Emit"})
	final := wait()
	if final.Summary != "via Emit" {
		t.Errorf("Run returned %+v, want the emitted Done verbatim", final)
	}
	if !strings.Contains(w.String(), "via Emit") {
		t.Errorf("summary missing from output:\n%q", w.String())
	}
}
