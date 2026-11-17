package imagebuild

// The build(master) byte-watcher (DESIGN_SPEC 6.2 four-phase build
// progress; the 2026-10-20 "looks hung" amendment). xorriso runs for
// minutes with no parseable output, so a goroutine os.Stat's the
// growing <SET-ID>-<NNN>.iso.partial on a poll interval and emits
// progress.BytesWritten{absolute size} on the Bus, driving the
// determinate build(master) bar/line against the RequiredImageBytes
// ESTIMATE (build.go emits the Phase event with TotalBytes set to the
// 4.3 canonical estimate BEFORE the watcher starts).
//
// Design points, per the architect's ruling (DESIGN_SPEC 6.2's
// progress paragraph):
//
//   - NO xorriso-output parsing: the watcher stats one file, so it is
//     robust across xorriso versions and needs no format discovery
//     (the deferred parse-xorriso-verbose enhancement stays deferred).
//   - A single-file os.Stat every second is negligible; the interval
//     is injectable (MasterWatch on Deps - the established
//     fake-clock/seam pattern; the production default is the package
//     const masterPollInterval).
//   - The emitted position is the ABSOLUTE size, never an increment
//     (9.1's BytesWritten semantics). A stat ERROR (the .partial does
//     not exist yet - xorriso has not created it - or it was just
//     renamed away) holds the last position: progress is advisory, one
//     missed sample never fails the build. A stalled xorriso simply
//     holds the last position, which is honest.
//   - Saturate-and-hold at 100% is the RENDERER's concern (a real ISO
//     lands under the conservative estimate; a pathological overshoot
//     holds at 100% rather than rolling back) - the watcher emits the
//     raw absolute position.
//   - Lifecycle: the goroutine starts BEFORE Tool.Run and stops when
//     it returns - tied to the run's ctx, so cancellation, interrupt,
//     and error paths all stop it; stop WAITS for the goroutine to
//     exit (the settle-poll leak tests see no residue, T1.1 pattern).

import (
	"context"
	"os"
	"sync"
	"time"

	"isopress/internal/progress"
)

// masterPollInterval is the default build(master) byte-watcher poll
// interval (DESIGN_SPEC 6.2: "every second (injectable interval)").
// A single os.Stat per second against a growing file is negligible
// overhead; Deps.MasterWatch overrides it for tests.
const masterPollInterval = time.Second

// MasterWatch configures the build(master) byte-watcher. The ZERO
// value is the production configuration: masterPollInterval and an
// os.Stat-derived size probe. Tests inject a fast interval and a fake
// stat source (the seam pattern; see watch_test.go).
type MasterWatch struct {
	// Interval overrides masterPollInterval; 0 means the default.
	Interval time.Duration
	// Stat overrides the size probe (os.Stat's Size); nil means the
	// production probe. A returned error holds the last position
	// (file doc).
	Stat func(path string) (size int64, err error)
}

// resolve returns the effective interval and stat probe.
func (w MasterWatch) resolve() (time.Duration, func(string) (int64, error)) {
	interval := w.Interval
	if interval <= 0 {
		interval = masterPollInterval
	}
	stat := w.Stat
	if stat == nil {
		stat = func(path string) (int64, error) {
			fi, err := os.Stat(path)
			if err != nil {
				return 0, err
			}
			return fi.Size(), nil
		}
	}
	return interval, stat
}

// startMasterWatcher starts the build(master) byte-watcher (file doc).
// emit is the event sink (bus.Emit; nil means silent). The watcher
// takes its first sample immediately (usually a miss - xorriso has not
// created the .partial yet - which is silently held), then one per
// interval, until stop is called or ctx is cancelled. stop is
// idempotent and WAITS for the goroutine to exit (the no-leak
// contract).
func startMasterWatcher(ctx context.Context, path string, emit func(progress.Event), cfg MasterWatch) (stop func()) {
	if emit == nil {
		return func() {}
	}
	interval, stat := cfg.resolve()
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			if size, err := stat(path); err == nil {
				emit(progress.BytesWritten{Bytes: size})
			}
			// On error: hold the last position (file doc).
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			wg.Wait()
		})
	}
}

// busEmit adapts the Bus onto the watchers' emit seam, nil-safe (a
// Build with no Bus runs the watchers nowhere: nil emit, no goroutine
// at all - the DEPS nil-safety convention).
func busEmit(bus *progress.Bus) func(progress.Event) {
	if bus == nil {
		return nil
	}
	return bus.Emit
}
