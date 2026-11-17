package verify

// Adversarial audit tests for the build(extract) tree-sum watcher and
// the extract->verify phase boundary (DESIGN_SPEC 6.2 four-phase
// progress; the 2026-10-20 "looks hung" amendment). These close the
// audit gaps left by watch_test.go:
//
//   - the immediate-first-sample contract on an EMPTY extraction dir:
//     EXACTLY one frame (the non-silent 0-byte first frame), not a
//     flood - with a long poll interval only the immediate sample can
//     fire, so the count is deterministic.
//   - the pathological-fallback gate BOUNDARY: Entries == exactly
//     maxTreeSumEntries stays DETERMINATE (the <= gate); one row more
//     degrades to the indeterminate Tick fallback (the +1 side is
//     pinned in watch_test.go; the == side and the pair belong
//     together).
//   - the extract->verify ANTI-INTERLEAVING contract: the extract
//     watcher's stop() waits (WaitGroup) before the build(verify)
//     Phase is emitted, and the Bus is FIFO - so NO build(extract)
//     line may render after the first build(verify) line. A late
//     BytesWritten there would flip the verify phase's bar to its
//     byte-driven form in the (phase-agnostic) renderers and corrupt
//     the verify display - pinned observably here.
//   - the Run-ERROR termination path at the ExtractAndHash level: a
//     failing xorriso extraction must still stop() the watcher (no
//     settle-poll residue), the one exit path not covered by the
//     watcher-level lifecycle tests.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"isopress/internal/progress"
	"isopress/internal/units"
)

// TestExtractWatcherEmptyDirEmitsExactlyOneFirstFrame: a fresh, empty
// extraction dir gives the phase its non-silent first frame - the
// immediate sample, a 0-byte BytesWritten - and with a poll interval
// far beyond the test window EXACTLY ONE frame must arrive (the
// immediate sample; no flood, no duplicates).
func TestExtractWatcherEmptyDirEmitsExactlyOneFirstFrame(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	var got []progress.Event
	emit := func(ev progress.Event) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	}

	// One hour interval: only the immediate first sample can fire.
	stop := watchExtraction(context.Background(), dir, emit, time.Hour, true, sumExtractionTree)
	if !waitEvents(func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) > 0
	}, 10*time.Second) {
		stop()
		mu.Lock()
		t.Fatalf("the empty extraction dir produced no first frame (the phase must open non-silent)")
	}
	time.Sleep(25 * time.Millisecond) // well under the 1h interval
	stop()                            // waits for the goroutine; the frame count is final after this

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 {
		t.Fatalf("empty-dir first window emitted %d frames (first: %+v), want EXACTLY the one immediate 0-byte sample", len(got), got[0])
	}
	bw, ok := got[0].(progress.BytesWritten)
	if !ok || bw.Bytes != 0 {
		t.Errorf("first frame = %+v, want BytesWritten{Bytes: 0}", got[0])
	}
}

// TestExtractAndHashFallbackGateBoundary pins the gate arithmetic at
// its exact boundary: Entries == maxTreeSumEntries opens the phase
// DETERMINATE (the <= comparison), one row more opens the indeterminate
// Tick fallback. The gate counts ALL content rows (symlink and dir
// rows included - verifyTotalsOfRows' Entries), which is what makes it
// a pure up-front decision.
func TestExtractAndHashFallbackGateBoundary(t *testing.T) {
	// Exactly at the gate: determinate - the extract line carries the
	// exact byte total and a percent.
	at := Totals{Entries: maxTreeSumEntries, Files: 5, Bytes: 5 + 4 + 6 + 2 + 3}
	out, err := runPhaseStream(t, at)
	if err != nil {
		t.Fatalf("ExtractAndHash at the gate: %v", err)
	}
	wantTotal := units.FormatBytes(at.Bytes)
	extractLine := ""
	for _, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.HasPrefix(l, "build(extract): ") {
			extractLine = l
			break
		}
	}
	if extractLine == "" {
		t.Fatalf("no build(extract) line at the gate boundary (Entries == maxTreeSumEntries):\n%s", out)
	}
	if !strings.Contains(extractLine, "/"+wantTotal+" (") {
		t.Errorf("extract line at Entries == maxTreeSumEntries is not determinate (lacks %q): %q", "/"+wantTotal+" (", extractLine)
	}

	// One row above the gate: indeterminate - no total, no percent.
	above := Totals{Entries: maxTreeSumEntries + 1, Files: 5, Bytes: at.Bytes}
	out2, err := runPhaseStream(t, above)
	if err != nil {
		t.Fatalf("ExtractAndHash above the gate: %v", err)
	}
	for _, l := range strings.Split(strings.TrimRight(out2, "\n"), "\n") {
		if !strings.HasPrefix(l, "build(extract): ") {
			continue
		}
		if strings.Contains(l, "%") || strings.Contains(l, "/") {
			t.Errorf("extract line one row ABOVE the gate renders a percent/total (the fallback must be indeterminate): %q", l)
		}
	}
}

// TestExtractAndHashNoExtractFrameAfterVerifyPhase pins the
// extract->verify ANTI-INTERLEAVING contract (see the file doc): every
// build(extract) line must precede every build(verify) line in the
// captured stream. The existing phase-order pins check FIRST
// occurrences; this checks the LAST extract line against the FIRST
// verify line - the property that a late tree-sum frame (or Tick
// re-render) cannot land inside the verify phase, where the
// phase-agnostic renderers would let it drive the verify bar with byte
// positions.
func TestExtractAndHashNoExtractFrameAfterVerifyPhase(t *testing.T) {
	// The fixture's content-list shape (runPhaseStream's tree).
	totals := Totals{Entries: 7, Files: 5, Bytes: 5 + 4 + 6 + 2 + 3}
	out, err := runPhaseStream(t, totals)
	if err != nil {
		t.Fatalf("ExtractAndHash: %v", err)
	}

	lastExtract, firstVerify := -1, -1
	for i, l := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.HasPrefix(l, "build(extract): ") {
			lastExtract = i
		}
		if firstVerify < 0 && strings.HasPrefix(l, "build(verify): ") {
			firstVerify = i
		}
	}
	if lastExtract < 0 || firstVerify < 0 {
		t.Fatalf("missing phase lines (extract-last=%d, verify-first=%d):\n%s", lastExtract, firstVerify, out)
	}
	if lastExtract > firstVerify {
		t.Fatalf("CROSS-PHASE INTERLEAVING: a build(extract) frame landed at line %d, after the first build(verify) line at %d "+
			"(the extract watcher outlived stop(); its BytesWritten would drive the verify bar):\n%s",
			lastExtract, firstVerify, out)
	}
}

// TestExtractAndHashRunFailureLeavesNoWatcherLeak pins the Run-ERROR
// termination path at the ExtractAndHash level: a failing xorriso
// extraction returns an error AND stops the watcher - the settle-poll
// baseline must return, no residue from the tree-sum goroutine.
func TestExtractAndHashRunFailureLeavesNoWatcherLeak(t *testing.T) {
	baseline := settleVerifyGoroutines(t)

	dir := t.TempDir()
	toolPath := installFakeTool(t, dir, "echo 'injected xorriso failure' >&2\nexit 3")
	dest := filepath.Join(dir, "extract")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := ExtractAndHash(context.Background(), toolWith(toolPath), "img.iso.partial", dest,
		progress.NewBus(), Totals{Entries: 7, Files: 5, Bytes: 20})
	if err == nil {
		t.Fatal("ExtractAndHash succeeded on a failing xorriso (exit 3)")
	}
	if !strings.Contains(err.Error(), "xorriso extraction") {
		t.Errorf("error %q lacks the extraction wrap", err)
	}
	waitForVerifyGoroutineBaseline(t, baseline, 5*time.Second)
}
