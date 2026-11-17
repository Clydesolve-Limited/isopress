package progress

// T1.1 tests for renderer selection (DESIGN_SPEC 9.2 matrix, FR-045,
// FR-043), the interactive renderer smoke path (FR-042), and the
// structural stdout guarantee of DESIGN_SPEC 10.3: no production file
// in this package may reference os.Stdout at all.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rendererKind names the concrete renderer behind an interface value so
// matrix assertions stay readable.
func rendererKind(r Renderer) string {
	switch r.(type) {
	case *interactiveRenderer:
		return "interactive"
	case *plainRenderer:
		return "plain"
	case *silentRenderer:
		return "silent"
	default:
		return "unknown"
	}
}

// TestRendererSelectionMatrix92 covers ALL FOUR rows of the
// DESIGN_SPEC 9.2 matrix with an injected TTY probe (the production
// probe is x/term on os.Stderr; Options.IsTerminal is the test seam):
//
//	TTY and not --quiet -> interactive
//	not TTY             -> plain (automatically; pipe/cron/systemd/redirect)
//	TTY with --quiet    -> plain
//	any stream, quiet   -> plain
func TestRendererSelectionMatrix92(t *testing.T) {
	rows := []struct {
		name     string
		tty      bool
		quiet    bool
		wantKind string
	}{
		{"TTY and not quiet -> interactive", true, false, "interactive"},
		{"not TTY -> plain automatically", false, false, "plain"},
		{"TTY with quiet -> plain", true, true, "plain"},
		{"not TTY with quiet -> plain", false, true, "plain"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			r := New(Options{
				Quiet:      row.quiet,
				IsTerminal: func() bool { return row.tty },
			})
			if got := rendererKind(r); got != row.wantKind {
				t.Errorf("New(Quiet=%v, tty=%v) selected %s renderer, want %s (DESIGN_SPEC 9.2)",
					row.quiet, row.tty, got, row.wantKind)
			}
		})
	}
}

// TestRendererExplicitModesPinRenderer: the explicit Mode values bypass
// the matrix so tests and embedding callers can pin a renderer; ModeAuto
// (zero value) remains the production default that applies 9.2.
func TestRendererExplicitModesPinRenderer(t *testing.T) {
	cases := []struct {
		mode     Mode
		wantKind string
	}{
		{ModeAuto, "plain"}, // default probe runs against os.Stderr: a pipe under go test
		{ModeInteractive, "interactive"},
		{ModePlain, "plain"},
		{ModeSilent, "silent"},
	}
	for _, c := range cases {
		if got := rendererKind(New(Options{Mode: c.mode})); got != c.wantKind {
			t.Errorf("New(Mode=%d) selected %s, want %s", c.mode, got, c.wantKind)
		}
	}
}

// TestDefaultIsTerminalMatchesStderrShape: the production probe must be
// callable and must agree with the state of os.Stderr (x/term on stderr
// per DESIGN_SPEC 9.2). Under go test stderr is a pipe, so the expected
// value is false; the assertion is about wiring, not about the ambient
// environment, hence the tolerance: only a panic fails here.
func TestDefaultIsTerminalMatchesStderrShape(t *testing.T) {
	got := DefaultIsTerminal()
	if fi, err := os.Stderr.Stat(); err == nil {
		// os.ModeCharDevice is set for character devices (terminals).
		want := fi.Mode()&os.ModeCharDevice != 0
		if got != want {
			t.Errorf("DefaultIsTerminal() = %v, want %v (x/term probe of os.Stderr)", got, want)
		}
	}
}

// TestInteractiveRendererFullStream drives a complete event stream -
// determinate phase, indeterminate phase, ticks, a warning, and the
// completion - through the progressbar-backed renderer and asserts it
// produces bar output, surfaces warnings verbatim, and returns the
// terminal Done. Exact bar formatting belongs to progressbar/v3, not to
// this tool's spec, so assertions stay loose on shape and strict on
// content that the design fixes (phase prefixes, warning lines,
// summary).
func TestInteractiveRendererFullStream(t *testing.T) {
	w := &lockedWriter{}
	bus := NewBus()
	wait := Start(New(Options{Mode: ModeInteractive, Stderr: w}), bus)

	bus.Emit(Phase{Name: "copy", TotalKnown: true, TotalFiles: 3, TotalBytes: 300})
	bus.Emit(ItemDone{Bytes: 100, Path: "a"})
	bus.Emit(ItemDone{Bytes: 100, Path: "b"})
	bus.Emit(Phase{Name: "build"}) // indeterminate: totals unknown (FR-042)
	bus.Emit(ItemDone{Bytes: 0, Path: "S-001.iso"})
	bus.Emit(Tick{})
	bus.Emit(Warn{Msg: "image 2 rebuilt from staging"})
	bus.Finish(Done{Summary: "build: complete (1 images)"})
	final := wait()

	out := w.String()
	if out == "" {
		t.Fatal("interactive renderer produced no output at all")
	}
	if !strings.Contains(out, "copy: files") {
		t.Errorf("interactive output lacks the determinate phase description:\n%q", out)
	}
	if !strings.Contains(out, "build: files") {
		t.Errorf("interactive output lacks the indeterminate phase description:\n%q", out)
	}
	if !strings.Contains(out, "warning: image 2 rebuilt from staging") {
		t.Errorf("interactive output lacks the warning line:\n%q", out)
	}
	if !strings.Contains(out, "build: complete (1 images)") {
		t.Errorf("interactive output lacks the final summary line:\n%q", out)
	}
	if final.Summary != "build: complete (1 images)" {
		t.Errorf("Run returned %+v, want the completion Done", final)
	}
}

// TestNoStdoutReferenceInPackage is the structural half of the FR-046
// guarantee (DESIGN_SPEC 10.3): no production file in internal/progress
// references os.Stdout - renderers can only write through the injected
// stderr writer, so a stdout leak is impossible by construction. Test
// files are excluded because the pipe-swap acceptance test in
// progress_test.go must reference os.Stdout to capture it.
func TestNoStdoutReferenceInPackage(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "os.Stdout") {
			t.Errorf("%s references os.Stdout; FR-046 confines this package to stderr", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking package: %v", err)
	}
}
