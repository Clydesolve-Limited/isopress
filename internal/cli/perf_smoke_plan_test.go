//go:build perf

package cli

// T6.3 part 2 - the 1M-item plan smoke (DESIGN_SPEC 12.8 verbatim:
// "Bounded-time (seconds) CI checks: ... plan of 1M items under
// budget; these are regression tripwires with generous thresholds,
// not benchmarks." Plus DESIGN_SPEC 4.3: "Complexity: O(n log k); a
// 1M-file tree plans in seconds in memory." IMPLEMENTATION_PLAN 4.7
// T6.3 acceptance: both checks run under their budgets and FAIL the
// suite on regression; thresholds are NAMED CONSTANTS with rationale
// comments; the 1M-item plan check asserts the O(n log k) expectation
// holds IN PRACTICE and completes without excessive memory (failure
// on OOM or timeout).
//
// Build tag: `perf` (PM gating decision; precedent: e2e/large). The
// perf gate is `go test -tags perf ./... -timeout 30m`, run once per
// task at QA during S6 and at sprint gates; the default (~46s) and
// e2e (~94s) gates stay untouched because timing tripwires must not
// destabilize them with load sensitivity.
//
// LAYER CHOICE (documented judgment call): the timed phase drives the
// REAL plan pipeline exactly as the `isopress plan` command does -
// plan.Balance (compute) followed by the plan command's OWN
// writeAssignments hook (the single-transaction write of 4.5) - over a
// real catalog, through the planCatalogStore wiring adapter. This is
// the established synthetic-profile pattern (copy_test.go
// planSynthetic): only the capacity profile is injected. Injection is
// required because units.ParseSize (4.1) accepts whole GB/GiB only,
// so the CLI flag layer cannot express the sub-GB destination that
// makes a 100k-item fixture span k>=3 images; the balancer surface
// 12.8 measures (catalog load, item construction, sort, WFD heap,
// recordAssignment, the assignment transaction) is byte-for-byte the
// code `isopress plan` runs. Driving the cobra command instead
// would also make the two timed phases (Balance vs write) invisible,
// and phase-bisectable timing is exactly what the T6.3 watch-point
// diagnosis needs if a budget trips.
//
// WHAT EACH LEG ASSERTS (T6.3 acceptance):
//   - (a) wall clock of each leg <= planBudget (named constant,
//     rationale-commented; the 1M leg is the 12.8 contract check);
//   - (b) O(n log k) IN PRACTICE: plan time at 1M items vs the 100k
//     leg from the SAME fixture builder - 10x items must not cost
//     ~100x time; planRatioMax is the asserted bound;
//   - (c) MEMORY: the peak live heap sampled across the 1M timed
//     window stays under planMemCeilingBytes (named, rationale-
//     commented); an actual OOM aborts the process, which fails the
//     suite by construction;
//   - (d) OOM/TIMEOUT = FAILURE: each leg runs under a context
//     deadline (planBudget) AND the test's own watchdog
//     (planBudget + planWatchdogGrace). If the pipeline honors the
//     context, the leg returns a deadline error and fails cleanly; if
//     it wedges past its own deadline, the watchdog fires instead,
//     dumps the leg goroutine's STACK into the test log (the "top N
//     frames" diagnosis), and preserves the CPU profile outside
//     t.TempDir (TempDir is removed even when the test fails). go
//     test's own -timeout remains the backstop behind both. Every
//     balancer loop probes ctx since T6.3 (ctxCheckInterval in
//     balancer.go), so a WATCHDOG trip on a current build names a
//     probe that was removed or never reached - or a ctx-blind
//     stretch OUTSIDE the balancer - not a known open debt: the two
//     TODO.md items this smoke was written to catch (the WFD placement
//     loop's missing ctx checks and recordAssignment's O(n^2) linear
//     scan) were CLOSED by T6.3, and the fix-round's measured results
//     (1M leg 28.3s, balance 7.3s, peak 478 MB) are the healthy
//     baseline a trip is measured against.
//
// WATCH-POINT (what a budget trip means NOW): both original watch
// points are CLOSED (T6.3: opsIndex makes recordAssignment O(1); the
// five ctx probes bound cancellation), so a trip on a post-T6.3 build
// is a NEW regression and is diagnosed with the evidence this file
// already assembles: the phase bisect in failPerfLeg (balance vs
// write), the preserved CPU profile (the /tmp/isopress-perf-*.prof
// mechanism below - profile it, do not guess), and the ratio leg. Two
// signatures to check first: scaling ~10x per 10x items between the
// 100k and 1M legs means something re-introduced super-linear work -
// verify that recordAssignment still goes through the opsIndex map
// (the pre-fix linear scan survives only as the test-only oracle
// plan.TestRecordAssignmentIndexedMatchesLinearScan); ~100x is the
// quadratic signature the bound exists to fail. A watchdog trip with
// a balance-phase stack means a ctx probe is missing or unreachable
// in the loop the stack names.
//
// FIXTURE DISCIPLINE (DESIGN_SPEC 12.2): both catalogs are built on
// the fly under t.TempDir() (bulk insert in ONE transaction, outside
// every timed window), never committed; the test asserts the fixture
// roots do not live inside the repository working tree.

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"isopress/internal/catalog"
	"isopress/internal/plan"
	"isopress/internal/units"
)

const (
	// planItemsFull is the DESIGN_SPEC 12.8 plan smoke size: "plan of
	// 1M items under budget".
	planItemsFull = 1_000_000

	// planItemsRatioLeg is the second timed leg of the O(n log k)
	// in-practice check (T6.3 acceptance (b)): the SAME fixture
	// builder at 1/10th the size, so the ratio compares like with
	// like and only n differs.
	planItemsRatioLeg = 100_000

	// planSetIDRatio / planSetIDFull: one set per fixture catalog.
	planSetIDRatio = "PERF-PLAN-100K"
	planSetIDFull  = "PERF-PLAN-1M"

	// planItemSizeMin / planItemSizeStep build deterministic item
	// sizes of 40,000-78,400 bytes (97 steps of 400, average
	// ~59,200): varied enough that the descending-size sort
	// exercises real comparisons and tie-breaks, all far below the
	// profile's per-image payload budget so every item is a
	// WHOLE-FILE placement (the WFD heap path, no chunking state to
	// seed - chunked-file planning has its own suites).
	planItemSizeMin  = 40_000
	planItemSizeStep = 400 // size = min + (i%97)*step -> max 78_400

	// planProfileUsableBytes is the injected synthetic destination
	// capacity (the planSynthetic pattern; see the file doc for why
	// the profile is injected rather than passed through --size).
	//
	// SIZING (the k>=3 acceptance of the delegation): average item
	// charge is size + 512 (4.3's per-entry cost) ~= 59.7 KB, so the
	// 100k leg packs ~6.0 GB and the 1M leg ~60 GB of payload. At
	// 500 MB usable per image that is ~13 images for the 100k leg and
	// ~122 for the 1M leg - both comfortably >= 3, so the WFD
	// open-destination heap, the new-destination path, and cross-image
	// gap-filling are genuinely exercised at BOTH scales, and k grows
	// with n exactly as the O(n log k) claim assumes. Also well above
	// 4.3's capacity floor (usable must hold FIXED_OVERHEAD + one
	// entry), and every item fits an empty image whole (no ErrItemTooLarge).
	planProfileUsableBytes = 500_000_000

	// planWatchdogGrace is how long PAST planBudget the test's own
	// watchdog waits before declaring the timed phase stuck. The leg's
	// context deadline (also planBudget) fires first when the pipeline
	// honors cancellation - the leg then returns a
	// context.DeadlineExceeded error and fails cleanly; the watchdog
	// only beats it when the pipeline WEDGED past its own deadline -
	// which, since T6.3 every balancer loop probes ctx (the WFD
	// placement-loop debt this grace was originally sized for is
	// CLOSED), means a probe that was removed or never reached, or a
	// ctx-blind stretch outside the balancer - and is precisely the
	// case whose stack must reach the test log. The grace must exceed
	// normal cancellation propagation (sub-second) by a wide margin so
	// the two failure modes never race.
	planWatchdogGrace = 30 * time.Second

	// planBudget is the wall-clock budget of EACH timed plan leg
	// (12.8: "plan of 1M items under budget"; "regression tripwires
	// with generous thresholds, not benchmarks").
	//
	// RATIONALE (measured on the QA machine, go1.26.10, Linux; five
	// sizes 10k/25k/50k/100k/200k, the T6.3 scaling probe):
	//
	//	observed local 100k leg (Balance + write): 6.28 s total
	//	(balance 4.28 s, write 2.00 s);
	//	measured scaling: the write phase is linear (~2.0x per
	//	doubling); the Balance phase doubles at 4.2x/3.6x/3.9x -
	//	fit balance(n) = 2.32 us*n + 4.05e-10*n^2 (validates at all
	//	five sizes). Under the O(n log k) contract the quadratic
	//	term is zero - but the linear-term projection (~2.3 s
	//	Balance at 1M) UNDERSHOT the real healthy leg by ~3x: the
	//	OBSERVED post-fix 1M leg (T6.3 fix-round + the two QA
	//	re-runs, WORK_LOG) is Balance 6.92-7.38 s + ~21 s write =
	//	27.25-28.31 s total, and those OBSERVED numbers - not the
	//	projection - are the healthy baseline the multiplier is
	//	sized against. The quadratic term AS MEASURED BEFORE THE
	//	T6.3 MAP FIX (the recordAssignment watch-point) projected
	//	~407 s Balance, ~428 s total at 1M - precisely the
	//	regression this tripwire must fail on, and the reason a
	//	post-fix trip is read as that debt's RE-REGRESSION (the
	//	WATCH-POINT note of the file doc names the check);
	//	multiplier applied: 180 s ~= 6.4x the healthy 1M total
	//	(the brief's 5-10x generosity band);
	//	CI-hardness argument: a 3-4x slower CI runner puts the
	//	healthy 1M plan at ~82-113 s, still ~2x under budget at
	//	the 3x end (~1.6x at 4x), while the quadratic regression
	//	lands at ~430 s local / ~1300 s CI - over budget everywhere
	//	by a wide margin, so the debt trips the gate
	//	deterministically instead of hovering at it. The design's
	//	own claim (4.3: "a 1M-file tree plans in seconds
	//	in memory") bounds the healthy order of magnitude; anything
	//	an order past the generous bound is a regression, not noise.
	//
	// The watchdog (planBudget + planWatchdogGrace) and go test's
	// -timeout are the hang backstops behind this number.
	planBudget = 180 * time.Second

	// planRatioMax is the asserted O(n log k)-IN-PRACTICE bound of
	// acceptance (b): plan time(1M items) / plan time(100k items).
	//
	// RATIONALE (from the five-size scaling probe): 10x items with
	// ~10x the destinations (media 12 at 100k, ~122 at 1M, see
	// planProfileUsableBytes) is ~10x the work under O(n log k) -
	// the n log n term alone is 10x log(1e6)/log(1e5) ~= 11x,
	// heap ops add log k with tiny constants - so a fixed plan
	// lands near ~10x; the bound carries ~2.5x headroom for cache
	// and GC noise. A RE-REGRESSED recordAssignment quadratic
	// instead projects ~68-100x at 1M (428 s vs the observed 6.28 s
	// 100k leg), which this bound fails by ~3-4x - the signature this
	// tripwire exists to catch. Generous, per 12.8: a tripwire, not
	// a benchmark.
	planRatioMax = 25.0

	// planMemCeilingBytes is the peak-live-heap ceiling of acceptance
	// (c), sampled every 25ms across the 1M timed window.
	//
	// RATIONALE (measured): peak HeapAlloc scales linearly at ~424 B
	// per item - 41 MB at the 100k leg, 85 MB at 200k, 360 MB at the
	// PRE-FIX 1M leg's watchdog trip (the items and write-ops slices
	// are fully built by then, so that IS the full planning state),
	// and 478 MB observed on a COMPLETED 1M leg after the T6.3 fix -
	// the figure this ceiling is sized against. The ceiling is 1 GiB
	// ~= 2.1x that observed peak: the headroom the 13 "slow or
	// memory-hostile plans" risk (DESIGN_SPEC 13) asks for, plus room
	// for CI GC/allocator variance (a tripwire that flaks on allocator
	// noise is a flake factory). An actual OOM kills the process and
	// fails the suite regardless of this ceiling.
	planMemCeilingBytes = 1 << 30 // 1 GiB
)

// perfPlanPhase is one leg goroutine's result: the two timed phases
// bisected (the T6.3 watch-point asks for phase-level evidence when a
// budget trips - "bisect by phase" is the cheapest split that needs
// no production hooks).
type perfPlanPhase struct {
	balance, write time.Duration
	filesAssigned  int
	mediaCount     int
	err            error
}

// perfPlanLegBody is the timed phase of one plan leg: the REAL
// pipeline - plan.Balance (load + item construction + sort + WFD
// placement) then the plan command's own writeAssignments hook (the
// ONE transaction of 4.5). It runs in its own goroutine so the test
// can watchdog it; the function NAME is the stack-extraction marker
// used when the watchdog fires.
func perfPlanLegBody(ctx context.Context, store plan.Store, db *catalog.DB,
	setID string, profile units.Profile) perfPlanPhase {
	var p perfPlanPhase
	b0 := time.Now()
	balanced, err := plan.Balance(ctx, store, profile)
	p.balance = time.Since(b0)
	if err != nil {
		p.err = err
		return p
	}
	p.filesAssigned = balanced.Result.FilesAssigned
	p.mediaCount = len(balanced.Result.Destinations)
	w0 := time.Now()
	p.err = writeAssignments(ctx, db, setID, &balanced)
	p.write = time.Since(w0)
	return p
}

// perfPlanLegOutcome is the evidence one leg hands back to the test
// goroutine (everything needed for the assertions AND for the report
// when a budget trips).
type perfPlanLegOutcome struct {
	balance, write, total time.Duration
	peakHeap              uint64 // max sampled HeapAlloc across the window
	totalAllocDelta       uint64 // cumulative allocation churn across the window
	filesAssigned         int
	mediaCount            int
	err                   error
	watchdogFired         bool
	stackDump             string // watchdog path: the leg goroutine's stacks
	profPath              string // CPU profile of the timed window (t.TempDir)
}

// extractPerfLegStack captures all goroutine stacks and keeps only the
// blocks containing marker - the "failure-mode that prints the top N
// frames to the test log" the T6.3 brief asks for. Falls back to the
// full dump when the marker is somehow absent (better too much than
// nothing in a diagnosis path).
func extractPerfLegStack(marker string) string {
	buf := make([]byte, 1<<20) // 1 MiB, grown until it fits
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	blocks := strings.Split(string(buf), "\ngoroutine ")
	var kept []string
	for _, b := range blocks {
		if strings.Contains(b, marker) {
			kept = append(kept, "goroutine "+b)
		}
	}
	if len(kept) == 0 {
		return string(buf)
	}
	out := strings.Join(kept, "\n")
	// Cap the log: enough for the top frames plus the hot production
	// frames, without flooding a CI log.
	const maxStackLog = 16 << 10
	if len(out) > maxStackLog {
		out = out[:maxStackLog] + "\n... (stack truncated)"
	}
	return out
}

// preservePerfProfile copies a CPU profile to a path that SURVIVES
// t.TempDir cleanup (the testing package removes TempDir even when
// the test fails), so a budget trip leaves a usable profile on disk,
// and returns the path for the failure message.
func preservePerfProfile(t *testing.T, src string) string {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Logf("perf: could not read CPU profile %s: %v", src, err)
		return ""
	}
	safe := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())
	dst := filepath.Join(os.TempDir(), fmt.Sprintf("isopress-perf-%s-%d.prof", safe, time.Now().UnixNano()))
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Logf("perf: could not preserve CPU profile to %s: %v", dst, err)
		return ""
	}
	return dst
}

// seedPerfPlanCatalog builds one plan fixture catalog: schema via
// catalog.Create, then sets + runs + n files rows bulk-inserted in ONE
// transaction (the generation cost stays OUTSIDE every timed window;
// T6.3 reports it). Returns the generation duration.
//
// Test-side SQL is the established seeding pattern (catalog_gaps_test,
// scan_test catRW): production SQL stays in internal/catalog; this is
// fixture construction, not production behavior. The pragmas mirror
// the catalog's own 2.1 DSN policy (migrate.go catalogDSN).
func seedPerfPlanCatalog(t *testing.T, path, setID string, n int) time.Duration {
	t.Helper()
	genStart := time.Now()
	if err := catalog.Create(path); err != nil {
		t.Fatalf("perf plan fixture: catalog.Create(%s): %v", path, err)
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("perf plan fixture: open seed connection: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("perf plan fixture: begin: %v", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO sets (set_id, source_root, created_at, tool_version)
		 VALUES (?, '/perf/fixture', 1, 'perf-smoke')`, setID); err != nil {
		t.Fatalf("perf plan fixture: insert set: %v", err)
	}
	// One ingest run row: files.first_seen_run / last_seen_run carry
	// the FK (2.3), and a prior ingest run is also what the real plan
	// command's PriorScanRuns gate expects of a scannable set.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO runs (run_id, set_id, kind, root, started_at, status, tool_version)
		 VALUES (1, ?, 'ingest', '/perf/fixture', 1, 'ok', 'perf-smoke')`, setID); err != nil {
		t.Fatalf("perf plan fixture: insert run: %v", err)
	}

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO files (set_id, relpath, entry_kind, size, mode, uid, gid,
			mtime_ns, dev, inode, nlink, first_seen_run, last_seen_run, first_seen_at)
		 VALUES (?, ?, 'regular', ?, 420, 0, 0, 1700000000000000000, 1, ?, 1, 1, 1, 1700000000000000000)`)
	if err != nil {
		t.Fatalf("perf plan fixture: prepare insert: %v", err)
	}
	defer stmt.Close()

	for i := 0; i < n; i++ {
		rel := fmt.Sprintf("d%03d/f%06d", i%100, i/100)
		size := int64(planItemSizeMin) + int64(i%97)*planItemSizeStep
		if _, err := stmt.ExecContext(ctx, setID, rel, size, int64(i+1)); err != nil {
			t.Fatalf("perf plan fixture: insert file %d (%s): %v", i, rel, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("perf plan fixture: commit: %v", err)
	}
	committed = true

	// Work evidence for the fixture itself: the legs assert against n,
	// so a silently short seed would silently shrink the workload.
	var got int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM files WHERE set_id = ?`, setID).Scan(&got); err != nil {
		t.Fatalf("perf plan fixture: count: %v", err)
	}
	if got != n {
		t.Fatalf("perf plan fixture: seeded %d rows, want %d", got, n)
	}
	return time.Since(genStart)
}

// runPerfPlanLeg runs ONE timed plan leg (Balance + write) against the
// fixture catalog at path, under planBudget as BOTH the context
// deadline and - plus planWatchdogGrace - the test's own watchdog.
// Generation has already happened; only this pipeline is timed.
func runPerfPlanLeg(t *testing.T, catPath, setID string, budget time.Duration) perfPlanLegOutcome {
	t.Helper()
	db, err := catalog.Open(catPath, catalog.OpenReadWrite)
	if err != nil {
		t.Fatalf("perf plan leg: opening catalog: %v", err)
	}
	defer db.Close() //nolint:errcheck // best-effort; the leg result dominates
	store := planCatalogStore{db: db, setID: setID}
	profile := units.Profile{
		Name: units.ProfileDisk,
		// planSynthetic pattern: the disk profile's 98% margin
		// inverted exactly once, here (D-06: margin applied exactly
		// once, at resolution).
		NominalBytes: planProfileUsableBytes * 100 / 98,
		UsableBytes:  planProfileUsableBytes,
	}

	// CPU profile of the timed window (the T6.3 watch-point asks for
	// targeted profiling on a trip). Started/stopped around the
	// window; its ~1-2% overhead is INSIDE the timed phase and is
	// absorbed by the generous tripwire budgets - accepted, because a
	// trip without a profile is a diagnosis with no evidence.
	profPath := filepath.Join(t.TempDir(), "plan-cpu.prof")
	profFile, err := os.Create(profPath)
	if err != nil {
		t.Fatalf("perf plan leg: create profile file: %v", err)
	}
	if err := pprof.StartCPUProfile(profFile); err != nil {
		_ = profFile.Close()
		t.Fatalf("perf plan leg: start CPU profile: %v", err)
	}

	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)

	// Peak-heap sampler: HeapAlloc oscillates under GC, so one
	// post-run read would miss the peak; sample every 25ms across the
	// window (ReadMemStats' STW pause is well under a millisecond).
	stopSampler := make(chan struct{})
	samplerDone := make(chan struct{})
	var peakHeap atomic.Uint64
	peakHeap.Store(base.HeapAlloc)
	go func() {
		defer close(samplerDone)
		tick := time.NewTicker(25 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stopSampler:
				return
			case <-tick.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				for {
					old := peakHeap.Load()
					if m.HeapAlloc <= old || peakHeap.CompareAndSwap(old, m.HeapAlloc) {
						break
					}
				}
			}
		}
	}()

	// The leg's OWN deadline (acceptance (d)): a pipeline that
	// honors cancellation fails cleanly with DeadlineExceeded; one
	// that wedges is caught by the watchdog below.
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	start := time.Now()
	done := make(chan perfPlanPhase, 1)
	go func() { done <- perfPlanLegBody(ctx, store, db, setID, profile) }()

	var out perfPlanLegOutcome
	select {
	case p := <-done:
		out.balance, out.write = p.balance, p.write
		out.filesAssigned, out.mediaCount, out.err = p.filesAssigned, p.mediaCount, p.err
		out.total = time.Since(start)
	case <-time.After(budget + planWatchdogGrace):
		// The pipeline ignored its own deadline past the grace: the
		// wedge case. Every balancer loop probes ctx since T6.3, so a
		// current build wedging here means a probe that was removed or
		// never reached - or a ctx-blind stretch OUTSIDE the balancer.
		// Capture the leg goroutine's stacks NOW, while it is still
		// stuck in the hot function: that is the top-N-frames
		// diagnosis, and it names the loop to look at.
		out.watchdogFired = true
		out.total = time.Since(start)
		out.stackDump = extractPerfLegStack("perfPlanLegBody")
	}

	close(stopSampler)
	<-samplerDone
	pprof.StopCPUProfile()
	if err := profFile.Close(); err != nil {
		t.Logf("perf plan leg: closing profile file: %v", err)
	}
	out.peakHeap = peakHeap.Load()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	out.totalAllocDelta = after.TotalAlloc - base.TotalAlloc
	out.profPath = profPath
	return out
}

// failPerfLeg reports a failed leg with every piece of evidence the
// T6.3 watch-point asks for: phase bisect (Balance vs write), the
// observed wall time against the budget, the preserved CPU profile,
// and - on a watchdog trip - the stuck goroutine's stack.
func failPerfLeg(t *testing.T, name string, out perfPlanLegOutcome, n int) {
	t.Helper()
	prof := preservePerfProfile(t, out.profPath)
	msg := fmt.Sprintf("%s leg failed: total %v (balance %v, write %v), budget %v, filesAssigned %d/%d, media %d, peakHeap %d B, totalAlloc +%d B",
		name, out.total, out.balance, out.write, planBudget, out.filesAssigned, n, out.mediaCount, out.peakHeap, out.totalAllocDelta)
	if prof != "" {
		msg += fmt.Sprintf("; CPU profile preserved at %s", prof)
	}
	if out.err != nil {
		msg += fmt.Sprintf("; leg error: %v", out.err)
	}
	if out.watchdogFired {
		msg += fmt.Sprintf("; WATCHDOG FIRED past planBudget+grace: the pipeline wedged past its own context deadline "+
			"(since T6.3 every balancer loop probes ctx - the stack below names a probe that was removed or never reached, or a ctx-blind stretch outside the balancer). Stack of the stuck leg goroutine:\n%s",
			out.stackDump)
	}
	t.Fatal(msg)
}

// TestPerfPlan1MWithinBudget is the 12.8 plan smoke: a real catalog
// with 1,000,000 file rows planned by the REAL pipeline (Balance +
// the plan command's write hook) under planBudget, with the O(n log k)
// in-practice ratio check and the peak-memory ceiling. See the file
// doc for the layer choice and acceptance mapping.
func TestPerfPlan1MWithinBudget(t *testing.T) {
	// 12.2 hygiene tripwire (same shape as the walk smoke): fixture
	// catalogs live under t.TempDir(), never the repository tree.
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	fixtureDir := t.TempDir()
	if strings.HasPrefix(fixtureDir+string(filepath.Separator), cwd+string(filepath.Separator)) {
		t.Fatalf("perf fixture root %q is inside the repository %q: fixtures must live under t.TempDir() (DESIGN_SPEC 12.2)", fixtureDir, cwd)
	}

	// --- fixtures: generation strictly OUTSIDE the timed windows ---
	ratioCat := filepath.Join(fixtureDir, "plan-100k.sqlite")
	fullCat := filepath.Join(fixtureDir, "plan-1m.sqlite")
	genRatio := seedPerfPlanCatalog(t, ratioCat, planSetIDRatio, planItemsRatioLeg)
	genFull := seedPerfPlanCatalog(t, fullCat, planSetIDFull, planItemsFull)
	t.Logf("perf plan: fixture generation 100k leg %v, 1M leg %v (outside the timed windows)", genRatio, genFull)

	// --- leg 1: 100k items (the O(n log k) ratio denominator) ---
	leg100 := runPerfPlanLeg(t, ratioCat, planSetIDRatio, planBudget)
	t.Logf("perf plan leg 100k: total %v (balance %v, write %v), media %d, peakHeap %d B, totalAlloc +%d B",
		leg100.total, leg100.balance, leg100.write, leg100.mediaCount, leg100.peakHeap, leg100.totalAllocDelta)
	if leg100.watchdogFired || leg100.err != nil || leg100.total > planBudget {
		failPerfLeg(t, "100k", leg100, planItemsRatioLeg)
	}
	if leg100.filesAssigned != planItemsRatioLeg {
		t.Fatalf("100k leg assigned %d files, want %d - the timed phase must do the full workload", leg100.filesAssigned, planItemsRatioLeg)
	}
	if leg100.mediaCount < 3 {
		t.Fatalf("100k leg planned %d images, want >= 3: the k>=3 multi-image WFD path must be exercised (planProfileUsableBytes sizing)", leg100.mediaCount)
	}

	// --- leg 2: 1M items (the 12.8 contract check) ---
	leg1M := runPerfPlanLeg(t, fullCat, planSetIDFull, planBudget)
	t.Logf("perf plan leg 1M: total %v (balance %v, write %v), media %d, peakHeap %d B, totalAlloc +%d B",
		leg1M.total, leg1M.balance, leg1M.write, leg1M.mediaCount, leg1M.peakHeap, leg1M.totalAllocDelta)
	if leg1M.watchdogFired || leg1M.err != nil || leg1M.total > planBudget {
		// The projection note: the first piece of evidence a would-be
		// quadratic regression needs. The debt this line used to name
		// is CLOSED (T6.3's opsIndex made recordAssignment O(1)), so a
		// ~100x projection landing here reads as that fix having
		// regressed - check that recordAssignment still goes through
		// the index (plan.TestRecordAssignmentIndexedMatchesLinearScan
		// is the oracle) and the preserved profile shows it directly.
		t.Logf("100k-leg reference: total %v (balance %v, write %v); a quadratic regression projects ~100x that at 1M items (first suspect: balancer.go recordAssignment re-scanning its op slices - the T6.3 opsIndex fix regressed)",
			leg100.total, leg100.balance, leg100.write)
		failPerfLeg(t, "1M", leg1M, planItemsFull)
	}
	if leg1M.filesAssigned != planItemsFull {
		t.Fatalf("1M leg assigned %d files, want %d - the timed phase must do the full workload", leg1M.filesAssigned, planItemsFull)
	}
	if leg1M.mediaCount < 3 {
		t.Fatalf("1M leg planned %d images, want >= 3: the k>=3 multi-image WFD path must be exercised", leg1M.mediaCount)
	}

	// --- (b) O(n log k) IN PRACTICE: the ratio tripwire ---
	ratio := float64(leg1M.total) / float64(leg100.total)
	t.Logf("perf plan ratio: t(1M)/t(100k) = %v/%v = %.2fx (bound %.0fx)", leg1M.total, leg100.total, ratio, planRatioMax)
	if ratio > planRatioMax {
		t.Fatalf("O(n log k) in practice violated: 10x items cost %.2fx time (t100k=%v, t1M=%v, bound %.0fx). "+
			"~10-15x is expected under O(n log k); ~100x is the signature of an O(n^2) regression (first suspect: balancer.go recordAssignment no longer going through its T6.3 opsIndex)",
			ratio, leg100.total, leg1M.total, planRatioMax)
	}

	// --- (c) MEMORY: peak live heap under the named ceiling ---
	if leg1M.peakHeap > planMemCeilingBytes {
		t.Fatalf("1M plan peak heap %d B exceeds planMemCeilingBytes=%d B (totalAlloc delta %d B): a memory-hostile plan regression (DESIGN_SPEC 13)",
			leg1M.peakHeap, planMemCeilingBytes, leg1M.totalAllocDelta)
	}
}
