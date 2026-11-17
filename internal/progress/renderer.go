package progress

// T1.1: the Renderer seam and the renderer-selection matrix of
// DESIGN_SPEC 9.2 (FR-045, FR-043), plus the Run/Start lifecycle.
//
// The CLI layer constructs the renderer ONCE at command startup and
// hands the Bus to the service; services emit, they never render
// (DESIGN_SPEC 9.1). All renderer output goes to stderr only
// (FR-046, DESIGN_SPEC 10.3): this package writes NOTHING to the
// process's standard output, a guarantee asserted both structurally (no
// file in the package references the stdout handle; deps-style test in
// renderer_test.go) and end-to-end (stdout pipe-swap test in
// progress_test.go).

import (
	"io"
	"os"
	"time"

	"golang.org/x/term"
)

// Renderer consumes events from a Bus on exactly one goroutine until a
// Done event arrives, renders them per the renderer's policy, and
// returns the final Done event so callers can observe the summary and
// the interruption flag (DESIGN_SPEC 9.1, 9.3).
type Renderer interface {
	// Run consumes events until the first Done event, which it returns.
	// Exactly one goroutine may call Run per Bus (DESIGN_SPEC 9.1); a
	// second concurrent Run on the same Bus panics rather than
	// interleaving output.
	Run(bus *Bus) Done
}

// Mode selects a concrete renderer. The production path is ModeAuto,
// which applies the DESIGN_SPEC 9.2 matrix. The explicit modes exist so
// tests and future embedding callers can pin a renderer directly; the
// silent renderer has no row in the 9.2 matrix (the matrix maps to
// interactive or plain only), which is why it is reachable explicitly.
type Mode int

const (
	// ModeAuto applies the DESIGN_SPEC 9.2 selection matrix.
	ModeAuto Mode = iota
	// ModeInteractive forces the progressbar-based renderer.
	ModeInteractive
	// ModePlain forces the plain renderer.
	ModePlain
	// ModeSilent forces the silent renderer (zero bytes of output).
	ModeSilent
)

// Options configure renderer construction. The zero value is valid for
// production use: no quiet flag, ModeAuto, stderr as the output writer,
// x/term probing of os.Stderr for TTY state, and time.Now as the clock.
type Options struct {
	// Quiet mirrors the global --quiet flag (DESIGN_SPEC 9.2): with
	// ModeAuto, quiet always selects the plain renderer.
	Quiet bool
	// Mode pins the renderer; ModeAuto (zero value) applies the 9.2
	// matrix.
	Mode Mode
	// Stderr is the writer all renderer output goes to (FR-046:
	// stderr only). nil means os.Stderr. This package never writes to
	// stdout, so there is deliberately no stdout option.
	Stderr io.Writer
	// IsTerminal probes whether stderr is a TTY (DESIGN_SPEC 9.2:
	// detection is golang.org/x/term.IsTerminal(int(os.Stderr.Fd()))).
	// Injectable for the renderer-matrix tests; nil means the
	// production probe against os.Stderr.
	IsTerminal func() bool
	// Now is the clock the plain renderer throttles against (the
	// fake-clock seam required by IMPLEMENTATION_PLAN T1.1 and
	// DESIGN_SPEC 12.1). nil means time.Now.
	Now func() time.Time
}

// New selects and constructs the renderer per the DESIGN_SPEC 9.2
// matrix (FR-045, FR-043):
//
//	TTY  && !quiet -> interactive (determinate bars where totals are
//	                  known, spinner otherwise, FR-042)
//	!TTY            -> plain AUTOMATICALLY (pipe, cron, systemd,
//	                  redirect; no flag needed)
//	TTY  &&  quiet  -> plain (the flag's documented purpose)
//	any  &&  quiet  -> plain
//
// An explicit opts.Mode overrides the matrix. The returned renderer is
// not yet consuming; bind it with Start (or a bare go r.Run(bus)).
func New(opts Options) Renderer {
	w := opts.Stderr
	if w == nil {
		w = os.Stderr
	}
	isTTY := opts.IsTerminal
	if isTTY == nil {
		isTTY = DefaultIsTerminal
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	switch opts.Mode {
	case ModeInteractive:
		return &interactiveRenderer{w: w}
	case ModePlain:
		return &plainRenderer{w: w, now: now}
	case ModeSilent:
		return &silentRenderer{}
	default: // ModeAuto: the 9.2 matrix.
		if opts.Quiet {
			return &plainRenderer{w: w, now: now}
		}
		if isTTY() {
			return &interactiveRenderer{w: w}
		}
		return &plainRenderer{w: w, now: now}
	}
}

// DefaultIsTerminal is the production TTY probe of DESIGN_SPEC 9.2:
// progress goes to stderr (FR-046), so stderr's TTY state is the
// deciding one - a user piping stdout to a file still gets bars; a user
// redirecting stderr into a cron log does not.
func DefaultIsTerminal() bool {
	return term.IsTerminal(int(os.Stderr.Fd()))
}

// Start runs r on its own goroutine consuming bus and returns a wait
// function that blocks until the renderer has processed the terminal
// Done event and yields it. This is the lifecycle the cli layer uses:
//
//	bus := progress.NewBus()
//	wait := progress.Start(progress.New(progress.Options{Quiet: quiet}), bus)
//	// ... service runs, emitting on bus ...
//	bus.Finish(progress.Done{Summary: summary})
//	final := wait()
//
// The goroutine exits when the renderer returns, so no resources leak
// after Finish.
func Start(r Renderer, bus *Bus) func() Done {
	done := make(chan Done, 1)
	go func() { done <- r.Run(bus) }()
	return func() Done { return <-done }
}
