package verify

// The build(extract) byte-watcher and the four-phase progress contract
// of ExtractAndHash (DESIGN_SPEC 6.2/9.1/9.3; the 2026-10-20 "looks
// hung" amendment). Layers:
//
//  1. The watcher itself through its injectable seams (fake tree-sum,
//     millisecond interval, recording emit): absolute sums, the
//     Tick-heartbeat fallback mode (NO sums - the fallback exists
//     precisely because the sum is too costly), lifecycle (stop waits,
//     ctx cancellation, idempotent stop, nil emit), and the
//     settle-poll goroutine leak checks (the T1.1 pattern, stdlib-only
//     - the internal/progress precedent, renamed local copies).
//  2. ExtractAndHash's phase contract, driven through the FAKE-tool
//     tier with a Bus consumed by the plain renderer (the established
//     cross-package capture pattern; the step clock gives every event
//     its own cadence window so the whole stream is observable):
//     build(extract) opens with the pinned bytes-only determinate
//     line against the EXACT total; build(verify) opens DETERMINATE -
//     the totals are present BEFORE the walk's first ItemDone - and
//     the pathological-fallback path renders the honest indeterminate
//     heartbeat line instead of a percent.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"isopress/internal/progress"
	"isopress/internal/units"
)

// --- test harness helpers -----------------------------------------------------

// syncBuffer is the mutex-guarded writer the renderer goroutine writes
// while the test goroutine reads (the progress package's lockedWriter
// shape; test-local because progress's copy is unexported).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// stepClock is the fake-clock harness for the stream assertions: every
// Now() read advances by PlainInterval, so EVERY progress event lands
// in its own cadence window and the captured stream shows the whole
// phase sequence (the renderer's cadence itself is pinned in
// internal/progress; here it is stepped so the order becomes
// observable).
type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(progress.PlainInterval)
	return c.t
}

// settleVerifyGoroutines waits for the goroutine count to stabilize
// (three consecutive equal samples) - the T1.1 settle-then-poll
// leak-check pattern (internal/progress hardening_test.go, renamed
// local copy).
func settleVerifyGoroutines(t *testing.T) int {
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

// waitForVerifyGoroutineBaseline polls until the goroutine count drops
// to baseline or fails loudly with the current count.
func waitForVerifyGoroutineBaseline(t *testing.T, baseline int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		n := runtime.NumGoroutine()
		if n <= baseline {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: %d goroutines after %v, baseline %d", n, within, baseline)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// waitEvents polls until pred holds or the deadline passes.
func waitEvents(pred func() bool, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if pred() {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return pred()
}

// --- the watcher's emission contract ------------------------------------------

// TestExtractWatcherEmitsTreeSums: determinate mode emits
// BytesWritten carrying the ABSOLUTE tree-sum, tracking a tree that
// grows while the watcher runs (the production sum source - LSTAT
// only - is exercised too, not just a fake).
func TestExtractWatcherEmitsTreeSums(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	var got []int64
	emit := func(ev progress.Event) {
		if bw, ok := ev.(progress.BytesWritten); ok {
			mu.Lock()
			got = append(got, bw.Bytes)
			mu.Unlock()
		}
	}
	stop := watchExtraction(context.Background(), dir, emit, time.Millisecond, true, sumExtractionTree)

	// The watcher's FIRST sample is the empty dir (0) - the
	// non-silent first frame; wait for it BEFORE growing the tree so
	// the sequence below is deterministic.
	if !waitEvents(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) > 0 && got[0] == 0
	}, 10*time.Second) {
		stop()
		mu.Lock()
		t.Fatalf("watcher emitted no immediate empty-tree sample; positions: %v", got)
	}

	// Grow the extraction tree while the watcher polls: 4 KiB, then
	// 12 KiB, then the final 20 KiB.
	write := func(name string, n int) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, n), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}
	write("a.bin", 4<<10)
	time.Sleep(5 * time.Millisecond)
	write("b.bin", 8<<10)
	time.Sleep(5 * time.Millisecond)
	write("c.bin", 8<<10)

	if !waitEvents(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) > 0 && got[len(got)-1] == 20<<10
	}, 10*time.Second) {
		mu.Lock()
		t.Fatalf("watcher never observed the full 20 KiB tree sum; positions: %v", got)
	}
	stop()

	mu.Lock()
	defer mu.Unlock()
	for i := 1; i < len(got); i++ {
		if got[i] < got[i-1] {
			t.Errorf("sums wobble downward: %v", got)
		}
	}
}

// TestSumExtractionTreeCountsFilesNotDirs pins the sum source: regular
// file BYTES only; dirs and symlinks contribute zero bytes; symlinks
// are never followed (a link to a big file outside the tree must not
// pull its target's bytes in); ctx cancellation aborts the walk.
func TestSumExtractionTreeCountsFilesNotDirs(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.bin"), make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "b.bin"), make([]byte, 50), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.bin", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	// A dangling symlink: counted as a member, zero bytes, no error.
	if err := os.Symlink("missing.bin", filepath.Join(dir, "dangling")); err != nil {
		t.Fatal(err)
	}

	bytes, entries, err := sumExtractionTree(context.Background(), dir)
	if err != nil {
		t.Fatalf("sumExtractionTree: %v", err)
	}
	if bytes != 150 {
		t.Errorf("bytes = %d, want 150 (regular-file bytes only; symlinks add none)", bytes)
	}
	if entries != 5 { // a.bin, sub, b.bin, link, dangling - every member is a cost proxy
		t.Errorf("entries = %d, want 5 (every non-root member counts)", entries)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := sumExtractionTree(ctx, dir); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled sum = %v, want context.Canceled", err)
	}
}

// TestExtractWatcherFallbackHeartbeatsNeverSum pins the fallback
// mode (6.2's honest degradation): determinate=false emits Tick
// heartbeats per poll - the phase stays NON-SILENT - and NEVER calls
// the tree-sum (the sum is exactly what the fallback exists to avoid;
// the injected fake panics the test if it is ever reached).
func TestExtractWatcherFallbackHeartbeatsNeverSum(t *testing.T) {
	var mu sync.Mutex
	var ticks, sums int
	emit := func(ev progress.Event) {
		mu.Lock()
		defer mu.Unlock()
		if _, ok := ev.(progress.Tick); ok {
			ticks++
		}
		if _, ok := ev.(progress.BytesWritten); ok {
			ticks += 100 // distinct failure signal
		}
	}
	sum := func(context.Context, string) (int64, int64, error) {
		mu.Lock()
		sums++
		mu.Unlock()
		return 0, 0, errors.New("the fallback must never sum")
	}
	stop := watchExtraction(context.Background(), t.TempDir(), emit, time.Millisecond, false, sum)
	if !waitEvents(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return ticks >= 3
	}, 10*time.Second) {
		stop()
		mu.Lock()
		t.Fatalf("fallback watcher emitted %d heartbeats, want a steady tick stream (non-silent)", ticks)
	}
	stop()

	mu.Lock()
	defer mu.Unlock()
	if sums != 0 {
		t.Errorf("the fallback called the tree-sum %d times; the sum is exactly what the fallback avoids", sums)
	}
}

// TestExtractWatcherLifecycleNoLeak pins the goroutine contract: stop
// WAITS for the watcher goroutine (settle-poll back to baseline), ctx
// cancellation ends it, stop is idempotent, nil emit starts nothing.
func TestExtractWatcherLifecycleNoLeak(t *testing.T) {
	emit := func(progress.Event) {}
	sum := func(context.Context, string) (int64, int64, error) { return 0, 0, nil }

	t.Run("stop retracts the goroutine", func(t *testing.T) {
		baseline := settleVerifyGoroutines(t)
		stop := watchExtraction(context.Background(), t.TempDir(), emit, time.Millisecond, true, sum)
		stop() // must block until the goroutine exited
		waitForVerifyGoroutineBaseline(t, baseline, 2*time.Second)
	})
	t.Run("ctx cancellation ends the goroutine", func(t *testing.T) {
		baseline := settleVerifyGoroutines(t)
		ctx, cancel := context.WithCancel(context.Background())
		stop := watchExtraction(ctx, t.TempDir(), emit, time.Millisecond, true, sum)
		cancel()
		stop()
		waitForVerifyGoroutineBaseline(t, baseline, 2*time.Second)
	})
	t.Run("stop is idempotent", func(t *testing.T) {
		stop := watchExtraction(context.Background(), t.TempDir(), emit, time.Millisecond, true, sum)
		stop()
		stop()
	})
	t.Run("nil emit starts nothing", func(t *testing.T) {
		baseline := settleVerifyGoroutines(t)
		stop := watchExtraction(context.Background(), t.TempDir(), nil, time.Millisecond, true, sum)
		stop()
		waitForVerifyGoroutineBaseline(t, baseline, time.Second)
	})
}

// --- ExtractAndHash's phase contract (the fake-tool tier) ---------------------

// preparedExtractionTree writes the tree the fake tool "extracts" and
// returns its path.
func preparedExtractionTree(t *testing.T) string {
	t.Helper()
	prepared := t.TempDir()
	for _, d := range []string{"files/sub", "chunks/big"} {
		if err := os.MkdirAll(filepath.Join(prepared, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for rel, content := range map[string]string{
		"files/a.txt":           "hello",
		"files/sub/deep.txt":    "deep",
		"chunks/big/00001":      "chunk!",
		"MANIFEST.json":         "{}",
		"catalog-export.sqlite": "exp",
	} {
		if err := os.WriteFile(filepath.Join(prepared, filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("a.txt", filepath.Join(prepared, "files", "link")); err != nil {
		t.Fatal(err)
	}
	return prepared
}

// runPhaseStream runs ExtractAndHash over the fake tool with a Bus
// consumed by the plain renderer (the step clock gives every event its
// own cadence window) and returns the captured stream.
func runPhaseStream(t *testing.T, totals Totals) (out string, err error) {
	t.Helper()
	dir := t.TempDir()
	prepared := preparedExtractionTree(t)
	toolPath := installFakeTool(t, dir, `cp -r "`+prepared+`/." "$7"`)
	dest := filepath.Join(dir, "extract")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}

	w := &syncBuffer{}
	clk := &stepClock{}
	bus := progress.NewBus()
	wait := progress.Start(progress.New(progress.Options{Mode: progress.ModePlain, Stderr: w, Now: clk.Now}), bus)

	_, err = ExtractAndHash(context.Background(), toolWith(toolPath), "img.iso.partial", dest, bus, totals)
	bus.Finish(progress.Done{Summary: "verify: done"})
	wait()
	return w.String(), err
}

// TestExtractAndHashVerifyPhaseOpensDeterminate pins requirement (d):
// the build(verify) Phase carries the EXACT content-list totals BEFORE
// the hash walk starts - the FIRST verify line is already the
// determinate counts+bytes form, and no counts-only verify line ever
// appears. The build(extract) phase opens with the pinned bytes-only
// determinate line against the exact total, and the two phases arrive
// in order.
func TestExtractAndHashVerifyPhaseOpensDeterminate(t *testing.T) {
	// The fixture's content-list shape: 7 rows, 5 regular files
	// ("hello", "deep", "chunk!", "{}", "exp" = 5+4+6+2+3 = 20 bytes).
	totals := Totals{Entries: 7, Files: 5, Bytes: 5 + 4 + 6 + 2 + 3}
	out, err := runPhaseStream(t, totals)
	if err != nil {
		t.Fatalf("ExtractAndHash: %v", err)
	}

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	extractIdx, verifyIdx := -1, -1
	for i, l := range lines {
		if extractIdx < 0 && strings.HasPrefix(l, "build(extract): ") {
			extractIdx = i
		}
		if verifyIdx < 0 && strings.HasPrefix(l, "build(verify): ") {
			verifyIdx = i
		}
	}
	if extractIdx < 0 || verifyIdx < 0 || extractIdx > verifyIdx {
		t.Fatalf("phase lines out of order (extract at %d, verify at %d):\n%s", extractIdx, verifyIdx, out)
	}

	// build(extract): the pinned BYTES-ONLY determinate form with the
	// EXACT total (the tree-sum watcher's first sample is the empty
	// dir, so the first line is 0 of the total - non-silent from the
	// first frame).
	wantTotal := units.FormatBytes(totals.Bytes)
	if !strings.Contains(lines[extractIdx], "/"+wantTotal+" (") {
		t.Errorf("extract line %q lacks the exact byte total %q", lines[extractIdx], wantTotal)
	}
	if strings.Contains(lines[extractIdx], "files ") {
		t.Errorf("extract line carries a file count; byte-driven phases are bytes-only (9.1): %q", lines[extractIdx])
	}

	// build(verify): the FIRST line already carries the file TOTAL
	// ("files 1/<files>," - the counts-ONLY form would read "files 1,")
	// and the byte total - the totals were present BEFORE the walk's
	// first ItemDone (a mid-phase upgrade would render a counts-only
	// line first).
	if !strings.Contains(lines[verifyIdx], "/"+wantTotal+" (") ||
		!strings.Contains(lines[verifyIdx], "files 1/"+strconv.FormatInt(totals.Files, 10)+", ") {
		t.Errorf("first verify line %q lacks the up-front totals (%d files, %s)", lines[verifyIdx], totals.Files, wantTotal)
	}
	for _, l := range lines {
		if strings.HasPrefix(l, "build(verify): files ") && !strings.Contains(l, "/") {
			t.Errorf("counts-only verify line rendered: the phase must open determinate (6.2): %q", l)
		}
	}
}

// TestExtractAndHashFallbackPathIsNonSilent pins requirement (g): a
// pathological content-list entry count degrades build(extract) to the
// HONEST INDETERMINATE fallback - no percent, no byte line - but the
// phase is NOT silent: the watcher's Tick heartbeats render the
// counts-only companion line every cadence window. build(verify) is
// unaffected (its totals are entry CHEAP - the walk reads files
// anyway).
func TestExtractAndHashFallbackPathIsNonSilent(t *testing.T) {
	// The pathological shape: > maxTreeSumEntries content rows. The
	// extraction itself stays small (the fake tool), because the gate
	// compares the DECLARED entry count, not a walked one.
	totals := Totals{Entries: maxTreeSumEntries + 1, Files: 5, Bytes: 5 + 4 + 6 + 2 + 3}
	out, err := runPhaseStream(t, totals)
	if err != nil {
		t.Fatalf("ExtractAndHash: %v", err)
	}

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	extractLines := 0
	for _, l := range lines {
		if !strings.HasPrefix(l, "build(extract): ") {
			continue
		}
		extractLines++
		// The fallback form: counts-only, NO percent, NO bytes-only
		// determinate shape.
		if strings.Contains(l, "%") || strings.Contains(l, "/") {
			t.Errorf("fallback extract line renders a percent or totals: %q", l)
		}
		if !strings.Contains(l, "files 0, ") {
			t.Errorf("fallback extract line lacks the honest counts-only shape: %q", l)
		}
	}
	if extractLines == 0 {
		t.Fatalf("the fallback extract phase rendered NOTHING (it must stay non-silent):\n%s", out)
	}
	// The verify phase is unaffected: still determinate.
	if !strings.Contains(out, "build(verify): files 1/5, ") {
		t.Errorf("verify phase lost its determinate totals under the extract fallback:\n%s", out)
	}
}

// TestExtractPhaseFirstFrameUnconditionalUnderCancelRace is the
// regression pin for the lost-first-frame race (CI-hardening QA P1):
// when extraction finishes BEFORE the watcher goroutine's first
// schedule - routine for an instant fake tool, near-certain at
// GOMAXPROCS=1 (the container-aware GOMAXPROCS a CPU-quota'd CI
// container gets) - stopWatch's cancel had already landed by the time
// the first sum ran. sumExtractionTree then answered ctx.Err() at the
// root, the `err == nil` guard silently dropped the phase's ONLY first
// frame, and the plain renderer deliberately prints NOTHING on a bare
// Phase transition (the 5s cadence policy), so the verify(extract)
// line never rendered - breaking watch.go's header contract (the
// watcher's first sample must give the phase a non-silent first
// frame). Event order was never at risk (stopWatch waits before the
// verify Phase); only the opening frame was.
//
// The harness is the QA bisect shape: an INSTANT fake tool (exits
// without extracting anything, so the whole run finishes inside the
// watcher's scheduling window) through a direct ExtractAndHashPhases
// call, plain renderer + step clock, asserting verify(extract) renders.
// The loop repeats the race enough times that the pre-fix skip fails
// reliably (the QA measured 17/20 iterations lost the frame at
// GOMAXPROCS=1) while staying bounded (20 instant runs).
func TestExtractPhaseFirstFrameUnconditionalUnderCancelRace(t *testing.T) {
	// The QA reproduction lever: serialize the scheduler so the
	// watcher goroutine is routinely first scheduled at stopWatch's
	// wg.Wait() - i.e. AFTER cancel has landed.
	prevProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(prevProcs)

	const iterations = 20
	for i := 0; i < iterations; i++ {
		dir := t.TempDir()
		toolPath := installFakeTool(t, dir, ":") // instant: exits 0, extracts nothing
		dest := filepath.Join(dir, "extract")
		if err := os.MkdirAll(dest, 0o755); err != nil {
			t.Fatal(err)
		}

		w := &syncBuffer{}
		clk := &stepClock{}
		bus := progress.NewBus()
		wait := progress.Start(progress.New(progress.Options{Mode: progress.ModePlain, Stderr: w, Now: clk.Now}), bus)
		_, err := ExtractAndHashPhases(context.Background(), toolWith(toolPath), "img.iso.partial", dest, bus,
			Totals{Entries: 7, Files: 5, Bytes: 20},
			PhaseNames{Extract: PhaseMediaExtract, Verify: PhaseMediaHash})
		bus.Finish(progress.Done{Summary: "verify: done"})
		wait()
		if err != nil {
			t.Fatalf("iteration %d/%d: ExtractAndHashPhases: %v", i+1, iterations, err)
		}
		if out := w.String(); !strings.Contains(out, PhaseMediaExtract+": ") {
			t.Fatalf("iteration %d/%d lost the verify(extract) FIRST FRAME (the phase opened silently):\n%s",
				i+1, iterations, out)
		}
	}
}
