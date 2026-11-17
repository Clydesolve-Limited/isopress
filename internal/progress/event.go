package progress

// T1.1: the event model of DESIGN_SPEC 9.1. Services emit Event values on
// a buffered channel (the Bus) consumed by EXACTLY ONE renderer goroutine.
// Services never render: they know nothing about TTY state, quiet flags,
// or output formats (DESIGN_SPEC 9.1, FR-042 to FR-046). This is the seam
// that keeps NFR-003's cron model honest - no service can accidentally
// print escape codes.
//
// Event kinds, exactly per DESIGN_SPEC 9.1: Phase{name, total_known},
// ItemDone{bytes, path}, BytesWritten{bytes}, Tick (heartbeat), Warn,
// Error, Done{summary}.

import (
	"sync"
	"sync/atomic"
)

// Event is the sealed interface implemented by every event kind of
// DESIGN_SPEC 9.1. The marker method keeps the set closed: renderers
// type-switch over the seven concrete kinds below and a default that is
// unreachable from outside this package.
type Event interface {
	isEvent()
}

// Phase declares a new phase of the current operation (for example
// "walk", "hash", "copy", or the build command's four named phases
// "build(stage)", "build(master)", "build(extract)", "build(verify)",
// DESIGN_SPEC 6.2). The renderer resets its counters on a NEW
// phase; a totals event for the already-running phase upgrades in place
// without resetting (per amended DESIGN_SPEC 9.1 - see the renderers'
// Phase case docs). The phase name becomes the plain-line prefix
// ("copy: files ...", "scan(hash): files ...", and the bytes-only
// "build(master): ..." of DESIGN_SPEC 9.3's byte-driven phases).
//
// Judgment call (documented): DESIGN_SPEC 9.1 spells the fields as
// "{name, total_known}", but the locked plain-line format of 9.3
// ("copy: files 1234/5678, 4.2 GiB/23.0 GiB (18%)") cannot be rendered
// from a boolean alone - the actual totals must reach the renderer.
// TotalFiles and TotalBytes therefore carry the totals, and TotalKnown
// says whether they are meaningful (false for full-tree scans whose file
// count is unknown until the walk completes, DESIGN_SPEC 3.6; in that
// case renderers show counts-and-bytes only, no totals and no percent).
type Phase struct {
	Name       string // phase name, used verbatim as the line prefix
	TotalKnown bool   // whether TotalFiles/TotalBytes are meaningful
	TotalFiles int64  // total items in this phase (valid iff TotalKnown)
	TotalBytes int64  // total bytes in this phase (valid iff TotalKnown)
}

func (Phase) isEvent() {}

// ItemDone reports one completed item (a file, a chunk, or another
// single unit of work). Path is carried for the interactive renderer's
// description and for future use; the plain format of 9.3 does not
// include it.
type ItemDone struct {
	Bytes int64  // byte size of the completed item
	Path  string // relpath of the completed item, optional
}

func (ItemDone) isEvent() {}

// BytesWritten reports the position of a BYTE-DRIVEN phase (the build
// master and extract watchers, DESIGN_SPEC 6.2). Per the amended
// DESIGN_SPEC 9.1 (four-phase build progress, 2026-10-20):
//
//   - Bytes carries the ABSOLUTE CUMULATIVE byte position of the
//     phase's output (the growing .partial's stat size, or the summed
//     bytes of the growing extraction tree) - NOT an increment. The
//     watcher samples an absolute file offset; the architect's naming
//     ruling fixes the semantics here so no delta arithmetic can
//     accumulate off-by-delta bugs between watcher and renderer.
//   - It is a DISTINCT kind, not a reuse of ItemDone, because those
//     phases have no per-item completion events (their progress is a
//     byte offset sampled by a watcher, not a stream of finished
//     items); folding them into ItemDone would pollute the file-count
//     accounting that ItemDone feeds.
//   - The renderer combines it with the current Phase's total_bytes to
//     render a determinate byte bar/line (on a byte-driven phase the
//     completed-count context is bytes, not file count - the renderers
//     switch to their bytes-only forms once a BytesWritten arrives).
//     Saturation at 100% (a position meeting or exceeding the phase
//     total, e.g. a .partial overshooting its RequiredImageBytes
//     estimate) is the RENDERER's concern: it caps the displayed
//     percent, holds the position, and never rolls back (6.2's
//     saturate-and-hold ruling).
type BytesWritten struct {
	Bytes int64 // ABSOLUTE cumulative byte position of the phase, not an increment
}

func (BytesWritten) isEvent() {}

// Tick is a heartbeat: services that make progress without completing
// items (or that simply want a render opportunity on a timer) emit a
// Tick; renderers treat it exactly like an ItemDone for cadence
// purposes but do not change counters.
type Tick struct{}

func (Tick) isEvent() {}

// Warn carries a warning message. Unlike progress lines, warnings and
// errors are NOT cadence-throttled by the plain renderer: they are rare,
// important, and also recorded to run_events by the services, so the
// stderr copy must be complete (DESIGN_SPEC 10.3).
type Warn struct {
	Msg string
}

func (Warn) isEvent() {}

// Error carries an error message (an item-level failure that does not
// abort the run, DESIGN_SPEC 10.1 ItemFailures). Same no-throttle
// contract as Warn.
type Error struct {
	Msg string
}

func (Error) isEvent() {}

// Done signals the end of the operation and terminates the renderer's
// Run loop. Summary is the one final summary line of DESIGN_SPEC 9.3,
// composed by the service (only the service knows the end-of-run
// numbers); the plain renderer prints it verbatim, exactly once, and the
// cadence throttle never suppresses it. Interrupted marks the
// interruption path (SIGINT/SIGTERM, DESIGN_SPEC 10.1): the service
// sets it, composes an interruption-flavored summary, and the renderer
// still prints exactly one summary line (DESIGN_SPEC 9.3: "plus exactly
// one final summary line at completion (and one at interruption)").
//
// Failed marks the ABORT-DUE-TO-ERROR path (cosmetic-honesty fix,
// 2026-10-20): the interactive renderer's progressbar Finish() COMPLETES
// the bar (renders 100%), which is misleading when the command is
// dying with an error - the stakeholder saw "files 0/1315, 0 B/21.4 GiB
// 100%" on a failed build. A Done with Failed=true tells the
// interactive renderer to CLEAR the bar instead of completing it; the
// summary line still prints. The SAME clearing applies to an
// Interrupted Done (the QA fold-in of 2026-10-20: an interrupted run
// completing its bar to 100% was the identical cosmetic lie). Failed
// and Interrupted are DISTINCT, MUTUALLY EXCLUSIVE signals: the cli
// layer derives both from the command's returned error through
// complementary predicates (internal/cli/progress_done.go), so a Done
// can never carry both, and both are false on success. The plain
// renderer is unaffected: its counts-and-percent lines were always
// honest about how far the run actually got.
//
// Interruption signaling design (task requirement): Done.Interrupted is
// the ONLY interruption signal at the progress layer. The cli layer
// still owns the exit-code decision (report.Classify, exit 130); the
// interactive renderer uses the flag to CLEAR the bar (never claim a
// 100% completion the run did not reach - see the Failed doc above,
// which shares the clearing path), and every renderer returns it
// faithfully from Run so callers can assert the interruption was
// observed.
type Done struct {
	Summary     string // the one final summary line; empty prints nothing
	Interrupted bool   // true on the SIGINT/SIGTERM interruption path (bar cleared, summary prints)
	Failed      bool   // true when the command aborts due to an error (not interruption; bar cleared, summary prints)
}

func (Done) isEvent() {}

// ChannelBuffer is the capacity of a Bus channel (DESIGN_SPEC 9.1:
// "buffered channel"). It decouples service throughput from renderer
// speed: at the plain renderer's 5-second cadence the renderer touches
// the buffer rarely, so a healthy margin means Emit never drops in
// practice.
const ChannelBuffer = 1024

// Bus is the one-way event channel between services and the single
// renderer goroutine (DESIGN_SPEC 9.1). Services hold the Bus; they
// emit, they never render.
//
// Overflow policy (documented choice, tested in progress_test.go):
// Emit NEVER blocks the producer. If the channel is full the event is
// DROPPED and a counter is incremented (Dropped). Rationale: progress
// is advisory - a blocking emit would couple service throughput to
// renderer speed and could stall a hash worker or copy loop on a slow
// terminal, and a block-bounded emit would still stall producers for the
// bound. A dropped incremental event loses at most one display update;
// the final summary line is exempt from dropping (see Finish), so the
// authoritative end-of-run numbers are never lost.
//
// Finish is the guaranteed-delivery path for the terminal Done event:
// it blocks until the single renderer goroutine has consumed the event
// (the renderer always drains until Done, so this cannot block
// indefinitely once a renderer is running - see Start). Callers MUST
// have started a renderer (Start, or go r.Run(bus)) before calling
// Finish; Emit of a Done value routes here automatically so the
// guarantee cannot be bypassed by accident.
type Bus struct {
	ch      chan Event
	dropped atomic.Int64
}

// NewBus returns a Bus with the ChannelBuffer-sized buffer.
func NewBus() *Bus {
	return &Bus{ch: make(chan Event, ChannelBuffer)}
}

// Emit delivers ev to the renderer goroutine without ever blocking the
// producer: on a full channel the event is dropped and counted (see the
// overflow policy above). Emit of a Done event is equivalent to Finish
// (guaranteed delivery) so the terminal-event guarantee holds no matter
// which entry point a service uses.
func (b *Bus) Emit(ev Event) {
	if d, ok := ev.(Done); ok {
		b.Finish(d)
		return
	}
	select {
	case b.ch <- ev:
	default:
		b.dropped.Add(1)
	}
}

// Finish delivers the terminal Done event with guaranteed delivery: it
// blocks until the renderer consumes it. Contract: a renderer goroutine
// must be running (Start or go Renderer.Run); the renderer drains the
// bus until Done, so the send completes promptly. Exactly one Finish
// per Bus is the intended usage; a second Done simply becomes the
// renderer's return value on a subsequent receive and is ignored by the
// Run loop contract (Run returns on the first Done it sees).
func (b *Bus) Finish(ev Done) {
	b.ch <- ev
}

// Dropped reports how many events Emit dropped due to a full channel.
// Renderers and tests use it to observe overflow; production services
// may fold it into their run report.
func (b *Bus) Dropped() int64 {
	return b.dropped.Load()
}

// busConsumers guards the "exactly one renderer goroutine" invariant of
// DESIGN_SPEC 9.1 at the package level: Renderer.Run acquires the bus
// once and panics on a second concurrent Run over the same bus, because
// two consumers would interleave writes to the output stream.
var busConsumers sync.Map // *Bus -> struct{}

func acquireBus(b *Bus) {
	if _, loaded := busConsumers.LoadOrStore(b, struct{}{}); loaded {
		panic("progress: a second renderer started on the same Bus; " +
			"DESIGN_SPEC 9.1 allows exactly one renderer goroutine per bus")
	}
}

func releaseBus(b *Bus) {
	busConsumers.Delete(b)
}
