package cli

// Percent-after-walk acceptance tests (DESIGN_SPEC 3.6/9.1/9.2,
// stakeholder decision 2026-10-19): the scan command emits ONE totals
// Phase event for the still-running hash phase the moment the walk
// completes, and the renderer upgrades the in-flight display in place.
//
// The deterministic unit coverage of the event's rendering lives in
// internal/progress (plain_test.go pins both 9.3 windows byte-exact
// against the fake clock; interactive_test.go pins the in-place bar
// upgrade). What THIS file adds is the scan-side wiring: the PRODUCTION
// emission function (emitScanWalkTotals - the exact call runScan makes)
// driven over a real progress.Bus with the real interactive renderer,
// a real hash pool, and a real walk of the standard fixture, asserting
// the upgrade fires with the walker's true totals and lands as an
// in-place upgrade (no extra bar/line artifacts). A capturing bus stub
// over runScan itself is not reachable from this package (progress.Bus
// exposes no event-read API and runScan owns bus construction), so the
// emission contract is pinned here through the shared production
// helper instead.
//
// Stdlib only; every fixture lives in t.TempDir().

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"

	"isopress/internal/ingest"
	"isopress/internal/progress"
)

// lockedBuffer is a mutex-guarded writer for the renderer's stderr:
// the renderer goroutine and progressbar's spinner ticker both write
// while the test goroutine reads only after wait() returns, so -race
// requires the lock (the progress package's own tests use the same
// shape).
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestScanTotalsEmissionUpgradesRendererEndToEnd wires the real scan
// progress pipeline exactly as runScan does - real Bus, real
// interactive renderer (the 9.2 TTY row), the real hash pool emitting
// the indeterminate scanPhaseName Phase at construction, the real
// drain goroutine over pool.Results, the real walker feeding
// pool.Submit - and fires the PRODUCTION totals emission
// (emitScanWalkTotals) at the exact point runScan fires it:
// immediately after a successful Walk, BEFORE the pool is closed and
// drained. The captured output must show:
//
//   - the determinate post-upgrade description with the walker's TRUE
//     totals - the fixture's three regular files total 27 bytes
//     (8 + 8 + 11; the symlink target and the excluded paths add
//     nothing) - i.e. "scan(hash): files 3/3, 27 B/27 B";
//   - an in-place upgrade: exactly two newlines in the whole output
//     (end-of-bar + summary) - a renderer that tore the spinner down
//     and started a fresh determinate bar would emit an extra
//     end-of-bar newline.
//
// The files/bytes continuity across the upgrade (completed counts
// carried, not reset) is pinned exactly in the progress package's
// interactive test; here the final 3/3 state is deterministic
// regardless of whether hashes completed before or after the walk did.
func TestScanTotalsEmissionUpgradesRendererEndToEnd(t *testing.T) {
	root := makeFixture(t)
	m, err := ingest.NewMatcher(fixtureExcludes(), "")
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}

	w := &lockedBuffer{}
	bus := progress.NewBus()
	wait := progress.Start(progress.New(progress.Options{Mode: progress.ModeInteractive, Stderr: w}), bus)

	// Same wiring order as runScan: pool first, then the single drain
	// goroutine (the only Results consumer), then the walk.
	pool := ingest.NewHashPool(context.Background(), ingest.PoolOptions{Bus: bus, PhaseName: scanPhaseName})
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for range pool.Results() {
		}
	}()

	wres, walkErr := ingest.Walk(root, m, pool.Submit, bus)
	if walkErr != nil {
		t.Fatalf("Walk: %v", walkErr)
	}
	// The production emission, at the production point: right after a
	// successful walk, no barrier, workers still draining.
	emitScanWalkTotals(bus, wres)
	pool.Close()
	_ = pool.Wait()
	bus.Finish(progress.Done{Summary: "scan(hash): complete"})
	wait()
	<-drainDone

	if wres.Files != 3 || wres.TotalBytes != 27 {
		t.Fatalf("walk totals = %d files / %d bytes, want 3 / 27 (fixture sanity)", wres.Files, wres.TotalBytes)
	}

	out := w.String()
	if out == "" {
		t.Fatal("interactive renderer produced no output at all")
	}
	if !strings.Contains(out, "scan(hash): files 3/3, 27 B/27 B") {
		t.Errorf("output lacks the determinate post-upgrade description with the walk totals:\n%q", out)
	}
	if !strings.Contains(out, "%") {
		t.Errorf("output carries no rendered percent after the upgrade:\n%q", out)
	}
	if n := strings.Count(out, "\n"); n != 2 {
		t.Errorf("output holds %d newlines, want exactly 2 (end-of-bar + summary); a duplicated bar would add more:\n%q",
			n, out)
	}
	if !strings.Contains(out, "scan(hash): complete") {
		t.Errorf("final summary line missing:\n%q", out)
	}
}
