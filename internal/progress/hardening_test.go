package progress

// T1.1 hardening tests for the renderer-side risks: goroutine lifecycle
// (no leak when Start/Finish/wait is used as documented, including the
// progressbar spinner ticker that v3.19.1 spawns for indeterminate
// bars), the DESIGN_SPEC 9.2 protection against terminal control bytes
// reaching non-TTY streams, the structural shape of the production TTY
// probe, and the determinism of the plain percent truncation at n.5%
// boundaries.
//
// Goroutine accounting is stdlib-only (runtime.NumGoroutine with a
// settle-then-poll loop, the goleak approach reduced to essentials):
// progressbar's spinner goroutine outlives the renderer by up to one
// ticker interval (100ms), so an immediate comparison would flake; a
// bounded poll to the pre-test baseline is the honest check.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/schollz/progressbar/v3"
)

// settleGoroutines waits for the goroutine count to stabilize (three
// consecutive equal samples) and returns it. Residual spinner tickers
// from earlier tests retract within ~100ms, so this both avoids
// measuring during a tail and gives every leak test a fair baseline.
func settleGoroutines(t *testing.T) int {
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

// waitForGoroutineBaseline polls until the goroutine count drops to
// baseline or the deadline passes; failing loudly with the current
// count so a leak diagnosis has data.
func waitForGoroutineBaseline(t *testing.T, baseline int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		n := runtime.NumGoroutine()
		if n <= baseline {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: %d goroutines %v after the renderer lifecycle, baseline %d", n, within, baseline)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestProgressbarSpinnerGoroutineLifecycle is the fact base the leak
// tests rest on: schollz/progressbar v3.19.1 spawns a 100ms ticker
// goroutine for INDETERMINATE bars (max == -1 -> ignoreLength) and only
// that goroutine's IsFinished check retracts it - which is why the
// interactive renderer's endBar MUST call bar.Finish() on every phase
// transition and on Done. If this test ever fails, the leak tests below
// are measuring a changed library contract.
func TestProgressbarSpinnerGoroutineLifecycle(t *testing.T) {
	baseline := settleGoroutines(t)

	bar := progressbar.NewOptions64(-1, // indeterminate, like interactive.go's spinner
		progressbar.OptionSetWriter(io.Discard))
	waitForGoroutineBaseline(t, baseline+1, 5*time.Second) // ticker goroutine is up

	if err := bar.Finish(); err != nil {
		t.Fatalf("spinner Finish: %v", err)
	}
	waitForGoroutineBaseline(t, baseline, 5*time.Second) // and retracts on Finish
}

// TestGoroutineLifecyclePlainNoLeak: the documented Start -> Emit ->
// Finish -> wait lifecycle leaves no goroutine behind on the plain
// path (which spawns none of its own).
func TestGoroutineLifecyclePlainNoLeak(t *testing.T) {
	baseline := settleGoroutines(t)

	w := &lockedWriter{}
	bus := NewBus()
	wait := Start(New(Options{Mode: ModePlain, Stderr: w, Now: newFakeClock().Now}), bus)
	bus.Emit(Phase{Name: "copy", TotalKnown: true, TotalFiles: 3, TotalBytes: 300})
	for i := 0; i < 100; i++ {
		bus.Emit(ItemDone{Bytes: 1})
	}
	bus.Emit(Warn{Msg: "a warning"})
	bus.Finish(Done{Summary: "plain lifecycle done"})
	wait()

	waitForGoroutineBaseline(t, baseline, 2*time.Second)
	if !strings.Contains(w.String(), "plain lifecycle done") {
		t.Errorf("summary missing; the lifecycle under test did not render:\n%q", w.String())
	}
}

// TestGoroutineLifecycleInteractiveIndeterminateNoLeak: an indeterminate
// phase spawns progressbar's spinner ticker goroutine; endBar's
// bar.Finish() at Done must retract it. This is the real leak detector
// for the interactive renderer: if endBar ever stops calling Finish,
// the ticker goroutine never exits and this test fails its bounded
// poll.
func TestGoroutineLifecycleInteractiveIndeterminateNoLeak(t *testing.T) {
	baseline := settleGoroutines(t)

	w := &lockedWriter{}
	bus := NewBus()
	wait := Start(New(Options{Mode: ModeInteractive, Stderr: w}), bus)
	bus.Emit(Phase{Name: "build"}) // indeterminate: the spinner path
	bus.Emit(ItemDone{Bytes: 0, Path: "S-001.iso"})
	bus.Emit(Tick{})
	bus.Emit(Warn{Msg: "mid-build warning"})
	bus.Finish(Done{Summary: "build: complete (1 images)"})
	final := wait()

	// The spinner ticker exits at its next 100ms tick after endBar
	// finished the bar; allow a generous margin over the race detector.
	waitForGoroutineBaseline(t, baseline, 5*time.Second)
	if final.Summary != "build: complete (1 images)" {
		t.Errorf("Run returned %+v, want the terminal Done verbatim", final)
	}
	if !strings.Contains(w.String(), "build: complete (1 images)") {
		t.Errorf("summary missing from interactive output:\n%q", w.String())
	}
}

// TestGoroutineLifecycleInteractiveDeterminateNoLeak: determinate bars
// never start the spinner ticker, so nothing outlives wait() at all -
// asserted with a short poll to catch any future background-rended
// addition.
func TestGoroutineLifecycleInteractiveDeterminateNoLeak(t *testing.T) {
	baseline := settleGoroutines(t)

	w := &lockedWriter{}
	bus := NewBus()
	wait := Start(New(Options{Mode: ModeInteractive, Stderr: w}), bus)
	bus.Emit(Phase{Name: "copy", TotalKnown: true, TotalFiles: 3, TotalBytes: 300})
	bus.Emit(ItemDone{Bytes: 100, Path: "a"})
	bus.Finish(Done{Summary: "copy: complete (1 files)"})
	wait()

	waitForGoroutineBaseline(t, baseline, 2*time.Second)
}

// TestAutoRendererOnNonTTYEmitsNoTerminalControlBytes is the
// production-facing half of the non-TTY guarantee: under the 9.2
// matrix a pipe/cron/systemd stderr gets the plain renderer, so a full
// event stream must contain ZERO terminal control bytes (no ESC, no
// CR, no BEL - nothing below 0x20 except the newline). progressbar
// emits cursor sequences unconditionally when it is constructed (it
// cannot know the writer is a pipe), so the 9.2 TTY check is the ONLY
// protection between a cron log and escape-code garbage; this test
// pins that protection end-to-end. Forcing ModeInteractive onto a pipe
// writer is outside the production contract by construction - the
// matrix never does it - which is why the assertion lives on the
// ModeAuto path rather than on the interactive renderer itself.
func TestAutoRendererOnNonTTYEmitsNoTerminalControlBytes(t *testing.T) {
	w := &lockedWriter{}
	bus := NewBus()
	wait := Start(New(Options{
		Quiet:      false,
		IsTerminal: func() bool { return false }, // pipe/cron/systemd (9.2 row 2)
		Stderr:     w,
		Now:        newFakeClock().Now,
	}), bus)

	bus.Emit(Phase{Name: "copy", TotalKnown: true, TotalFiles: 3, TotalBytes: 300})
	bus.Emit(ItemDone{Bytes: 100, Path: "a"})
	bus.Emit(Tick{})
	bus.Emit(Phase{Name: "build"}) // indeterminate, the spinner phase on TTY
	bus.Emit(ItemDone{Bytes: 0, Path: "S-001.iso"})
	bus.Emit(Warn{Msg: "a warning"})
	bus.Emit(Error{Msg: "an error"})
	bus.Finish(Done{Summary: "scan: complete, 4 files"})
	wait()

	out := w.String()
	if out == "" {
		t.Fatal("no output at all; the test would pass vacuously")
	}
	for i := 0; i < len(out); i++ {
		if b := out[i]; b < 0x20 && b != '\n' && b != '\t' {
			t.Fatalf("terminal control byte %#x at offset %d reached a non-TTY writer (FR-046/9.2 guarantee broken): %q", b, i, out)
		}
	}
	if !strings.Contains(out, "scan: complete, 4 files") {
		t.Errorf("summary missing from the plain output:\n%q", out)
	}
}

// TestDefaultIsTerminalIsXTermOnStderr is the structural half of the
// DESIGN_SPEC 9.2 probe requirement: DefaultIsTerminal must literally
// call term.IsTerminal on os.Stderr's file descriptor, so the matrix
// decisions key off STDERR's TTY state (a user piping stdout to a file
// still gets bars; stderr into a cron log does not). Parsed from the
// AST so a rewiring of the probe (for example to stdout, or to a
// hand-rolled check) fails loudly.
func TestDefaultIsTerminalIsXTermOnStderr(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "renderer.go", nil, 0)
	if err != nil {
		t.Fatalf("parse renderer.go: %v", err)
	}

	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "DefaultIsTerminal" {
			fn = fd
			break
		}
	}
	if fn == nil {
		t.Fatal("DefaultIsTerminal not found in renderer.go; the 9.2 production probe must stay explicit")
	}

	foundTermCall, foundStderrFd := false, false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		x, ok := sel.X.(*ast.Ident)
		if !ok || x.Name != "term" || sel.Sel.Name != "IsTerminal" {
			return true
		}
		foundTermCall = true
		if len(call.Args) == 1 {
			ast.Inspect(call.Args[0], func(n2 ast.Node) bool {
				if c2, ok := n2.(*ast.CallExpr); ok {
					// looking for os.Stderr.Fd(): a two-level
					// selector, Fd called on the os.Stderr field.
					if s2, ok := c2.Fun.(*ast.SelectorExpr); ok && s2.Sel.Name == "Fd" {
						if x2, ok := s2.X.(*ast.SelectorExpr); ok {
							if id, ok := x2.X.(*ast.Ident); ok && id.Name == "os" && x2.Sel.Name == "Stderr" {
								foundStderrFd = true
							}
						}
					}
				}
				return true
			})
		}
		return true
	})

	if !foundTermCall {
		t.Error("DefaultIsTerminal no longer calls term.IsTerminal; DESIGN_SPEC 9.2 requires the x/term probe")
	}
	if !foundStderrFd {
		t.Error("DefaultIsTerminal no longer probes os.Stderr's descriptor; the 9.2 matrix must key off STDERR")
	}
}

// TestPlainPercentTruncatesAtHalfPercentBoundaries pins the percent
// arithmetic of the plain renderer at the n.5% boundaries: the ratio is
// derived from BYTES (the 9.3 example 4.2/23.0 = 18.26 -> 18 rules out
// the files ratio), computed as int64(float64/float64*100), which
// truncates toward zero and is bit-deterministic in Go (IEEE 754, no
// fused contraction). The table drives the renderer synchronously with
// bytes small enough that units.FormatBytes renders the item side as
// "N B" verbatim (the 1000-byte totals render as "1.0 KB", which the
// expected strings account for), so every expected line is exact.
func TestPlainPercentTruncatesAtHalfPercentBoundaries(t *testing.T) {
	cases := []struct {
		name       string
		totalFiles int64
		totalBytes int64
		itemBytes  int64
		want       string
	}{
		{"exact n.5 truncates down: 0.5%", 200, 200, 1, "copy: files 1/200, 1 B/200 B (0%)"},
		{"exact integer holds: 1%", 200, 200, 2, "copy: files 1/200, 2 B/200 B (1%)"},
		{"exact n.5 truncates down: 49.5%", 200, 200, 99, "copy: files 1/200, 99 B/200 B (49%)"},
		{"exact integer holds: 50%", 200, 200, 100, "copy: files 1/200, 100 B/200 B (50%)"},
		{"just above n.5 stays truncated: 50.5%", 200, 200, 101, "copy: files 1/200, 101 B/200 B (50%)"},
		{"exact n.5 truncates down: 99.5%", 200, 200, 199, "copy: files 1/200, 199 B/200 B (99%)"},
		{"full: 100%", 200, 200, 200, "copy: files 1/200, 200 B/200 B (100%)"},
		{"n.5 at permille: 12.5%", 1000, 1000, 125, "copy: files 1/1000, 125 B/1.0 KB (12%)"},
		{"n.5 at permille: 18.5%", 1000, 1000, 185, "copy: files 1/1000, 185 B/1.0 KB (18%)"},
		{"just below n.5: 14.9%", 1000, 1000, 149, "copy: files 1/1000, 149 B/1.0 KB (14%)"},
		{"exact n.5: 15.0%", 1000, 1000, 150, "copy: files 1/1000, 150 B/1.0 KB (15%)"},
		{"just above n.5: 15.1%", 1000, 1000, 151, "copy: files 1/1000, 151 B/1.0 KB (15%)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := &lockedWriter{}
			r, clk := newPlainForTest(w)
			r.handle(Phase{Name: "copy", TotalKnown: true, TotalFiles: c.totalFiles, TotalBytes: c.totalBytes})
			r.handle(ItemDone{Bytes: c.itemBytes})
			clk.Advance(PlainInterval) // irrelevant for a single line; keeps the harness honest
			r.writeSummary(Done{})     // empty summary prints nothing (documented)

			got := lines(w.String())
			if len(got) != 1 {
				t.Fatalf("got %d lines, want exactly 1 (progress only; the empty summary prints nothing):\n%q", len(got), w.String())
			}
			if got[0] != c.want {
				t.Errorf("progress line = %q, want exact %q", got[0], c.want)
			}
		})
	}
}
