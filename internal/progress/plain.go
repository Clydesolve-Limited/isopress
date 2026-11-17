package progress

// T1.1: the plain renderer, the log-friendly progress surface of
// DESIGN_SPEC 9.3 (FR-044, RESOLVED). The format and cadence are fixed
// by the spec:
//
//	Format: one line per emission, phase-prefixed with counts and bytes
//	and percent where known:
//	  copy: files 1234/5678, 4.2 GiB/23.0 GiB (18%)
//	  scan(hash): files 45210, 88.3 GiB
//	  scan(hash): files 12340/56283, 30.1 GiB/210.3 GiB (14%)
//
//	scan(hash) has TWO windows (percent-after-walk, DESIGN_SPEC
//	3.6/9.3, stakeholder 2026-10-19): counts-only until the walk
//	completes, determinate totals-plus-percent on the next scheduled
//	line after the walk-completion totals event arrives.
//
//	Byte-driven phases (four-phase build progress, DESIGN_SPEC 6.2 /
//	9.3, stakeholder 2026-10-20): build(master) and build(extract)
//	progress via the BytesWritten event and have NO file count, so
//	their lines are bytes-only, pinned byte-exact by the amendment:
//	  build(master): 12.3 GiB/21.0 GiB (58%)
//	  build(extract): 8.1 GiB/21.0 GiB (38%)
//	A byte-driven phase switches to these forms the moment its first
//	BytesWritten arrives; the per-entry build phases (build(stage),
//	build(verify)) reuse the determinate counts+bytes form above
//	(ItemDone-driven), so no new format exists for them. The percent
//	derives from bytes, truncated, per the established convention
//	(12.3/21.0 = 58 truncated) and is SATURATED at 100 (6.2's
//	saturate-and-hold: a byte position meeting or exceeding the phase
//	total - e.g. a .partial overshooting its conservative
//	RequiredImageBytes estimate - holds at 100%, never rolls back and
//	never prints above 100).
//
//	Cadence: at most one line every 5 seconds PER OPERATION, plus
//	exactly one final summary line at completion (and one at
//	interruption). 5 seconds is dense enough for cron observability
//	without flooding logs over multi-hour runs; PlainInterval is the
//	named constant 9.3 requires so it can be tuned without redesign.
//	All four build phases share this cadence through the same events.
//
//	Tick heartbeats (DESIGN_SPEC 9.1; the 2026-10-20 QA heartbeat
//	fold-in, see the Tick case in handle): a Tick emits the current
//	phase's line with unchanged counts - the repetition of the last
//	state, which tells the cron user the run is alive through a long
//	await point - and it respects the SAME PlainInterval throttle, so
//	heartbeat lines are ordinary cadence citizens and can never double
//	up with ItemDone lines. Before any phase is declared a Tick prints
//	nothing (no state to repeat).
//
// Byte formatting uses internal/units.FormatBytes (T0.2); the 9.3
// example strings were locked to its output in that task.

import (
	"fmt"
	"io"
	"time"

	"isopress/internal/units"
)

// PlainInterval is the cadence constant of DESIGN_SPEC 9.3: at most one
// plain progress line every PlainInterval per operation.
const PlainInterval = 5 * time.Second

// plainRenderer renders phase-prefixed counts-and-bytes lines to its
// writer, throttled to PlainInterval. It is selected automatically for
// non-TTY stderr and for --quiet runs (DESIGN_SPEC 9.2).
type plainRenderer struct {
	w   io.Writer
	now func() time.Time // the fake-clock seam (DESIGN_SPEC 12.1)

	// Render state accumulated from the event stream.
	lastLine   time.Time // zero => the first progress event prints immediately
	phase      string
	totalKnown bool
	byteDriven bool // this phase reports via BytesWritten (bytes-only line forms)
	files      int64
	totalFiles int64
	bytes      int64
	totalBytes int64
}

func (r *plainRenderer) Run(bus *Bus) Done {
	acquireBus(bus)
	defer releaseBus(bus)
	for {
		ev := <-bus.ch
		if done, ok := ev.(Done); ok {
			r.writeSummary(done)
			return done
		}
		r.handle(ev)
	}
}

// handle applies one non-terminal event. It is the synchronous core of
// Run; the format and cadence tests drive it directly on the test
// goroutine so the fake clock is advanced in lockstep with event
// processing (driving the async Run from a test would let the test
// goroutine race ahead of the renderer and defeat the fake clock).
func (r *plainRenderer) handle(ev Event) {
	switch e := ev.(type) {
	case Phase:
		// Mid-phase totals upgrade (DESIGN_SPEC 3.6/9.3,
		// percent-after-walk): a totals event naming the
		// ALREADY-RUNNING phase adopts the totals IN PLACE - the
		// counters keep their accumulated values, no line is emitted
		// here (the cadence and the immediate-line rules are
		// unchanged: the upgrade only affects the NEXT scheduled
		// emission, which switches to the determinate
		// totals-plus-percent form). A totals event for any other
		// phase name, or before any phase, is an ordinary phase
		// transition.
		if e.TotalKnown && !r.totalKnown && e.Name == r.phase {
			r.totalKnown = true
			r.totalFiles, r.totalBytes = e.TotalFiles, e.TotalBytes
			return
		}
		// A genuine phase transition resets the counters and the line
		// prefix, but NOT the throttle clock: the cadence is
		// "per operation" (9.3), and printing a line at every
		// phase change would flood multi-phase operations.
		r.phase = e.Name
		r.totalKnown = e.TotalKnown
		r.byteDriven = false
		r.files, r.totalFiles = 0, e.TotalFiles
		r.bytes, r.totalBytes = 0, e.TotalBytes
	case ItemDone:
		r.files++
		r.bytes += e.Bytes
		r.maybeLine()
	case BytesWritten:
		// Byte-driven phases (build master/extract, 6.2): the event
		// carries the ABSOLUTE cumulative position (9.1), so it
		// REPLACES the byte counter - with a monotonic hold: a
		// pathological shrink (a stat racing a recreate) may never
		// roll the displayed position back (6.2's no-rollback
		// clamp). The first BytesWritten switches the line to the
		// pinned bytes-only forms (the phase has no file count).
		r.byteDriven = true
		if e.Bytes > r.bytes {
			r.bytes = e.Bytes
		}
		r.maybeLine()
	case Tick:
		// THE HEARTBEAT LINE (DESIGN_SPEC 9.1 Tick / 9.3 cadence; the
		// 2026-10-20 QA heartbeat fold-in): the current phase's line
		// with UNCHANGED counts - a repetition of the last state. That
		// is the honest content: a Tick carries no new progress (it
		// fills the silent await points where a single huge member
		// stalls the per-FILE ItemDone flow - build(verify)'s hash
		// walk, build(stage)'s intra-member copy), so the line states
		// exactly what the run last knew and that it is still alive -
		// the liveness signal the cron/log user of 9.3 needs. It routes
		// through the SAME PlainInterval throttle as an ItemDone line
		// (maybeLine), so heartbeats can never double up with or crowd
		// out real progress lines; at HeartbeatInterval == PlainInterval
		// (internal/cli/build.go, deliberately equal values) each
		// heartbeat that falls in an empty cadence window prints once
		// and the next ItemDone picks the window back up. A Tick before
		// any phase has been declared has no state to repeat and prints
		// nothing (the build run's pre-phase window - catalog export,
		// inventory reads - is covered by the first phase's immediate
		// line instead).
		if r.phase == "" {
			return
		}
		r.maybeLine()
	case Warn:
		// Warnings and errors bypass the throttle (see the
		// Warn/Error event docs).
		fmt.Fprintf(r.w, "warning: %s\n", e.Msg) //nolint:errcheck // best-effort render write: FR-046 makes the stdout report the artifact, a failed stderr write must not perturb the render loop
	case Error:
		fmt.Fprintf(r.w, "error: %s\n", e.Msg) //nolint:errcheck // best-effort render write: FR-046 makes the stdout report the artifact, a failed stderr write must not perturb the render loop
	}
}

// writeSummary emits the one final summary line of DESIGN_SPEC 9.3.
// It always prints, throttle or not: the summary is the authoritative
// end-of-run record. An empty Summary prints nothing (documented: the
// service owns the end-of-run text).
func (r *plainRenderer) writeSummary(d Done) {
	if d.Summary != "" {
		fmt.Fprintln(r.w, d.Summary) //nolint:errcheck // best-effort render write: FR-046 makes the stdout report the artifact, a failed stderr write must not perturb the render loop
	}
}

// maybeLine emits a progress line when the cadence allows it. The first
// progress event of the operation always prints (lastLine is zero), so
// even a short cron operation leaves exactly one observable progress
// line; subsequent lines require PlainInterval to have elapsed.
func (r *plainRenderer) maybeLine() {
	now := r.now()
	if !r.lastLine.IsZero() && now.Sub(r.lastLine) < PlainInterval {
		return
	}
	r.lastLine = now
	fmt.Fprintln(r.w, r.line()) //nolint:errcheck // best-effort render write: FR-046 makes the stdout report the artifact, a failed stderr write must not perturb the render loop
}

// line renders the current state in the exact DESIGN_SPEC 9.3 formats:
//
//	with known totals: "<phase>: files <done>/<total>, <bytes>/<total> (<pct>%)"
//	without totals:   "<phase>: files <done>, <bytes>"
//
//	byte-driven with known totals:
//	  "<phase>: <bytes>/<total> (<pct>%)"   (e.g. "build(master): 12.3 GiB/21.0 GiB (58%)")
//	byte-driven without totals:
//	  "<phase>: <bytes>"                    (e.g. "build(extract): 8.1 GiB")
//
// Judgment call (documented): the spec example "4.2 GiB/23.0 GiB (18%)"
// shows 4.2/23.0 = 18.3% rendered as 18%, while the files ratio
// 1234/5678 is 21.7% - so the percent is derived from BYTES, truncated
// to an integer. The percent is omitted when totals are unknown or when
// total bytes is zero (nothing to compute a ratio against), and capped
// at 100 by cappedPercent (the 6.2 saturate-and-hold ruling: a byte
// position meeting or exceeding the total holds at 100, never rolls
// back, never prints above 100).
func (r *plainRenderer) line() string {
	if r.byteDriven {
		// Bytes-only forms: a byte-driven phase has no file count
		// (9.1: "on a byte-driven phase the completed-count context
		// is bytes, not file count").
		if !r.totalKnown {
			return fmt.Sprintf("%s: %s", r.phase, units.FormatBytes(r.bytes))
		}
		pct := ""
		if r.totalBytes > 0 {
			pct = fmt.Sprintf(" (%d%%)", cappedPercent(r.bytes, r.totalBytes))
		}
		return fmt.Sprintf("%s: %s/%s%s",
			r.phase, units.FormatBytes(r.bytes), units.FormatBytes(r.totalBytes), pct)
	}
	if !r.totalKnown {
		return fmt.Sprintf("%s: files %d, %s",
			r.phase, r.files, units.FormatBytes(r.bytes))
	}
	pct := ""
	if r.totalBytes > 0 {
		pct = fmt.Sprintf(" (%d%%)", cappedPercent(r.bytes, r.totalBytes))
	}
	return fmt.Sprintf("%s: files %d/%d, %s/%s%s",
		r.phase, r.files, r.totalFiles,
		units.FormatBytes(r.bytes), units.FormatBytes(r.totalBytes), pct)
}

// cappedPercent is the byte-ratio percent of DESIGN_SPEC 9.3 -
// derived from bytes, truncated toward zero (the established
// convention, bit-deterministic in IEEE 754) - SATURATED at 100. The
// cap is the plain half of the 6.2 saturate-and-hold ruling (the
// interactive half caps the bar's Set64): completed bytes can exceed
// walk-time-estimate totals or a conservative master estimate, and the
// honest display holds at 100% rather than rolling back or claiming
// more than completion.
func cappedPercent(bytes, total int64) int64 {
	if total <= 0 {
		return 0
	}
	p := int64(float64(bytes) / float64(total) * 100)
	if p > 100 {
		return 100
	}
	if p < 0 {
		return 0
	}
	return p
}
