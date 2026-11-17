package verify

// The build(extract) byte-watcher (DESIGN_SPEC 6.2 four-phase build
// progress; the 2026-10-20 "looks hung" amendment). While xorriso
// osirrox writes the extraction tree - a multi-minute write on real
// images - a goroutine sums the growing temp tree on a poll interval
// and emits progress.BytesWritten{absolute sum} on the Bus, driving
// the determinate build(extract) bar/line against the EXACT payload
// total from the content list (extract.go, Totals.Bytes).
//
// Design points, per the architect's ruling (DESIGN_SPEC 6.2's
// progress paragraph):
//
//   - A TREE-SUM, not a single-file stat: the extraction has no single
//     output file to stat, so each poll walks the tree with LSTAT only
//     (no opens, no hashes - the hash walk is build(verify)'s job) and
//     sums regular-file sizes.
//   - The interval is "about 2s" (extractPollInterval), injectable:
//     the watcher function takes it explicitly so tests drive it at
//     millisecond cadence (the established fake-clock/seam pattern;
//     production callers use the const through ExtractAndHash).
//   - HONEST FALLBACK (6.2): a pathological file count makes the
//     tree-sum itself too costly - each poll would burn CPU/IO that
//     competes with the extraction it is measuring. The gate is
//     maxTreeSumEntries, compared against the content-list ENTRY COUNT
//     UP FRONT (extract.go decides the phase's TotalKnown from it):
//     above the gate the phase is declared indeterminate and the
//     watcher emits Tick heartbeats instead of byte positions - a
//     spinner, still non-silent (the liveness concern is satisfied; a
//     percent the watcher cannot afford to compute honestly is not
//     shown).
//   - A sum ERROR (a transient lstat race while xorriso renames a
//     member) holds the last position: progress is advisory, so one
//     missed sample never fails - or stalls - the build.
//   - The FIRST sample is unconditional (the header contract): a
//     cancel that races the watcher goroutine's start - an extraction
//     that finishes before the scheduler first runs it - must not skip
//     the phase's opening frame, because the plain renderer prints
//     nothing on a bare Phase transition (the 5s cadence policy) and
//     the phase would open silently. Only that first determinate
//     sample runs on an uncancelable context; every later sample
//     honors ctx per poll.
//   - Lifecycle: the watcher goroutine runs exactly as long as the
//     xorriso extraction (ExtractAndHash stops it the moment
//     tool.Run returns, on EVERY exit path - success, non-zero exit,
//     exec failure, cancellation); stop waits for the goroutine to
//     exit, so the settle-poll leak tests see no residue (the T1.1
//     pattern).

import (
	"context"
	"io/fs"
	"path/filepath"
	"sync"
	"time"

	"isopress/internal/progress"
)

const (
	// extractPollInterval is the default build(extract) tree-sum poll
	// interval (DESIGN_SPEC 6.2: "about 2s, injectable"). Tests pass
	// their own interval; production goes through ExtractAndHash,
	// which uses this const.
	extractPollInterval = 2 * time.Second

	// maxTreeSumEntries is the pathological-file-count gate of the
	// documented honest fallback (DESIGN_SPEC 6.2): above this many
	// members in the extraction tree the per-poll lstat walk is judged
	// too costly to run every extractPollInterval, and the phase
	// degrades to an indeterminate spinner (Tick heartbeats) instead
	// of a percent. The budget rationale: a cold-cache lstat walk costs
	// roughly 5-20us per member, so 100k members is ~0.5-2s per poll -
	// at the edge of the 2s cadence; beyond it each poll would overlap
	// the next. The gate compares the CONTENT-LIST entry count (the
	// tree's entry count tracks it, plus the 5.3 implied parents -
	// asymptotically identical), which is knowable up front, so the
	// phase never flickers between determinate and spinner.
	maxTreeSumEntries = 100_000
)

// treeSumFunc sums the extraction tree at root once: the absolute byte
// total of its regular files, the member count (the cost proxy), and an
// error if the walk failed. Injectable so the watcher tests drive
// pathological and failing sources without building pathological trees
// (the seam pattern; production uses sumExtractionTree).
type treeSumFunc func(ctx context.Context, root string) (bytes, entries int64, err error)

// watchExtraction runs the build(extract) byte-watcher until stop is
// called or ctx is cancelled, whichever comes first. emit is the
// event sink (bus.Emit; nil means silent); sum is the tree-sum source
// (sumExtractionTree in production; tests inject fakes - the
// established seam pattern). The watcher takes its FIRST sample
// immediately (a fresh extraction dir sums to 0, which still gives the
// phase a non-silent first frame), then one per interval.
//
// determinate selects the mode:
//
//	true  - emit progress.BytesWritten{absolute sum} per sample
//	false - the fallback mode (totals.Entries > maxTreeSumEntries):
//	        emit progress.Tick heartbeats per sample, NO sums (the
//	        sum is exactly what the fallback avoids).
//
// stop is idempotent and WAITS for the watcher goroutine to exit (the
// no-leak contract the settle-poll tests pin).
//
// The FIRST determinate sample is UNCONDITIONAL (see the loop comment):
// it runs without cancellation even when stop's cancel races the
// goroutine's start, so the phase's opening frame can never be lost to
// that race; samples after the first honor ctx as documented.
func watchExtraction(ctx context.Context, root string, emit func(progress.Event), interval time.Duration, determinate bool, sum treeSumFunc) (stop func()) {
	if emit == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for i := 0; ; i++ {
			if determinate {
				// THE UNCONDITIONAL FIRST FRAME (the header
				// contract): the watcher's first sample gives the
				// phase its non-silent opening frame, so it must
				// not be skippable by a cancel that races this
				// goroutine's start. When extraction finishes
				// before the scheduler first runs the goroutine
				// (an instant tool, a ~1ms run - routine at
				// GOMAXPROCS=1, what a CPU-quota'd CI container
				// gets), stopWatch's cancel has already landed by
				// the time sum runs, and sumExtractionTree would
				// answer ctx.Err() at the root: the `err == nil`
				// guard would then silently drop the phase's ONLY
				// first frame, and the plain renderer deliberately
				// prints nothing on a bare Phase transition (the
				// 5s cadence policy), so the phase would open
				// SILENTLY. Only sample 0 runs without
				// cancellation; every later sample honors ctx as
				// before (the per-poll rule: a cancelled run must
				// not sit inside a pathological walk).
				sumCtx := ctx
				if i == 0 {
					sumCtx = context.WithoutCancel(ctx)
				}
				if b, _, err := sum(sumCtx, root); err == nil {
					emit(progress.BytesWritten{Bytes: b})
				}
				// On error: hold the last position (file doc).
			} else {
				emit(progress.Tick{})
			}
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

// busEmit adapts the Bus onto the watcher's emit seam, nil-safe (a nil
// Bus runs the watcher nowhere: nil emit, no goroutine at all - the
// nil-bus convention of the progress seam).
func busEmit(bus *progress.Bus) func(progress.Event) {
	if bus == nil {
		return nil
	}
	return bus.Emit
}

// sumExtractionTree is the production treeSumFunc: one LSTAT-only
// WalkDir over root summing regular-file sizes and counting every
// member. It never follows symlinks (WalkDir lstats), never opens a
// file, and honors ctx per directory read - a cancelled run must not
// sit inside a pathological walk.
func sumExtractionTree(ctx context.Context, root string) (bytes, entries int64, err error) {
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// A transient race with the extracting xorriso (a member
			// renamed between readdir and lstat) surfaces here; the
			// caller holds the last position.
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == root {
			return nil
		}
		// Every member counts as a cost proxy entry (dirs and
		// symlinks are lstat'ed too); only regular files add bytes.
		entries++
		if d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			return nil // dirs and symlinks add no file bytes
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			bytes += info.Size()
		}
		return nil
	})
	if walkErr != nil {
		return 0, 0, walkErr
	}
	return bytes, entries, nil
}
