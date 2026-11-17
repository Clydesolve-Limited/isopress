package cli

// T1.6: the real `scan` command - the Sprint 1 capstone that wires the
// walker (T1.3), hash pool (T1.4), failure classification (T1.5), the
// reconciler (T1.6), the catalog scan store (T1.6), and the progress
// bus (T1.1) into the first working command (DESIGN_SPEC 1.4 ingest
// data flow, 3.7 reconciliation, 10.2 exit codes, 10.3 stream
// separation). It replaces the T0.5 placeholder command in
// commands.go; the other eight stubs are untouched.
//
// Layering (DESIGN_SPEC 1.2): this file holds flag parsing, service
// invocation, and report presentation - no SQL (the catalog package is
// the only SQL issuer) and no rendering (progress events flow through
// the bus; the final report goes to stdout, progress to stderr,
// FR-046). The scanStoreTx adapter at the bottom is pure type
// mapping between the ingest.Store interface (which keeps
// internal/ingest free of catalog imports) and catalog.ScanTx.
//
// Wiring contracts honored here:
//
//   - usageArgs (see below): the cobra Args-validator -> UsageError
//     wrap convention, first applied here because scan is the first
//     command with real arguments (T0.5 review note N2: cobra's
//     untyped Args errors used to classify as exit 1 instead of 2).
//   - MUTATING catalog open (catalog.Open ReadWrite): the exclusive
//     advisory flock is taken before any database work; contention
//     surfaces as *catalog.CatalogBusy and report.Classify maps it to
//     exit 5 automatically (T0.4 wiring). A --catalog path that does
//     not exist is auto-created first (catalog.Create, PM-ratified
//     2026-10-19; race design in openScanCatalog below).
//   - Runs bookkeeping (DESIGN_SPEC 2.4): a runs row is inserted in
//     'running' BEFORE the walk and finalized 'ok'/'partial'/'failed'/
//     'interrupted' on every exit path via the DETACHED finalizer
//     (catalog.FinishRunDetached - a signal-cancelled run stamps
//     'interrupted' and exits 130 per 10.1, the T6.1 interrupt-semantics
//     closure); walker warnings and T1.5 failures land in
//     run_events; the effective pattern set is snapshotted into
//     runs.exclude_json (D-08); the resolved change-detection mode and
//     detected filesystem class are recorded into runs.stats_json
//     (3.5 audit, DB-side only - the frozen --json key-set is
//     untouched), and an auto-detected CIFS-family degradation emits
//     its once-per-run warning (progress.Warn + a run_events warn row)
//     at resolution.
//   - Pipeline (T1.4 wiring story, single-producer contracts
//     restated): the walk goroutine is the ONLY Pool.Submit caller;
//     the single drain goroutine below is the ONLY Batcher.Add caller
//     (it classifies via Failures.Process and feeds the batcher), and
//     the batcher's Flush callback is one reconciler batch = one
//     BEGIN IMMEDIATE catalog transaction.
//   - Progress (T1.1 contract): a FRESH Bus per operation, renderer
//     selected once at startup (9.2 matrix via progress.New), phase
//     name "scan(hash)" (9.3 example), and exactly one Done summary
//     line on EVERY exit path - success, failure, or abort - through
//     the deferred Finish/wait pair.
//   - Not-seen safety (3.7): the missing pass runs ONLY on clean
//     completion - walk returned no error, the pool drained, and every
//     batch flushed. Any abort skips it and the run is finalized
//     'failed', so a broken scan can never mark the catalog missing
//     (reconciler double-gates this via ErrScanIncomplete).
//   - Exit codes (10.2): Failures.Err() returns nil on a clean run
//     (exit 0) and a report.ItemFailures otherwise (exit 10); --strict
//     promotes item failures to a fatal exit 1 (10.2 partial-failure
//     clause); usage, catalog, and busy classes flow through
//     report.Classify unchanged.
//   - Set identity (6.4): --set-id if given, else
//     <sanitized-basename-of-root>-<ingest-date-UTC>. A set id already
//     bound to a different source root is a usage error (the catalog
//     refuses the re-bind; see catalog.EnsureSet). Rescans reconcile
//     by passing the SAME --set-id (the derived default changes daily
//     by design, per 6.4's ingest-date derivation).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"isopress/internal/catalog"
	"isopress/internal/ingest"
	"isopress/internal/progress"
	"isopress/internal/report"
	"isopress/internal/units"
)

// usageArgs wraps a cobra positional-args validator so its failures are
// returned as a typed report.UsageError (DESIGN_SPEC 10.1), which
// report.Classify maps to exit 2. This is the N2 wrap convention the
// T0.5 review deferred to T1.6 (the first command with real
// arguments): cobra's built-in validators (ExactArgs, ...) return
// untyped errors that would otherwise classify as exit 1 (fatal)
// instead of the spec-mandated exit 2 for missing/extra arguments.
// Every command with Args validation must go through this helper.
func usageArgs(validate cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := validate(cmd, args); err != nil {
			return &report.UsageError{Command: cmd.CommandPath(), Err: err}
		}
		return nil
	}
}

// scanArgs validates scan's positional arguments: exactly one ROOT
// (cobra.ExactArgs), whose leading "~" must not be a literal shell
// token that quoting kept unexpanded (checkTilde, paths.go; PM decision
// D1 of 2026-10-19). usageArgs wraps any failure in a typed
// report.UsageError so it classifies to exit 2 (DESIGN_SPEC 10.1).
func scanArgs(cmd *cobra.Command, args []string) error {
	if err := cobra.ExactArgs(1)(cmd, args); err != nil {
		return err
	}
	return checkTilde(args[0], "ROOT")
}

// newScanCommand builds the real scan command (replaces the T0.5
// stub). ROOT is required; every other input arrives through the global
// flags (--catalog, --exclude, --exclude-from, --set-id, --quiet,
// --json, --strict).
func newScanCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "scan ROOT",
		Short: "scan a source tree into the catalog (hash and record metadata)",
		Long: "Walk ROOT, apply the exclusion filters at walk time, SHA-256 every included " +
			"regular file, and reconcile the results into the SQLite catalog: new paths get " +
			"unassigned rows, changed content gets versioned rows (FR-030), unchanged paths " +
			"only touch last_seen_run, catalog-active paths not seen by a completed scan " +
			"are marked missing (FR-032), and currently-empty directories are recorded " +
			"durably in the directories table (FR-037). Progress renders on stderr; the " +
			"end-of-run report lands on stdout (JSON with --json). If the catalog file does not exist " +
			"at the --catalog path, it is created there (schema v1) before the scan begins.\n\n" +
			"Exit codes: 0 success; 1 fatal unexpected error (bug class; stack trace under --verbose); " +
			"2 usage, including a filesystem-root scan with no exclusion patterns; 4 catalog error; " +
			"5 catalog busy (another mutating run holds the lock); 10 completed with item failures " +
			"(unreadable or changed-during-scan files; --strict promotes this to 1); 130 interrupted. " +
			"JSON: --json prints the final report as exactly one JSON document on stdout; a run that " +
			"aborts before doing work prints no document at all; --json never changes the exit code.\n\n" +
			"Recommended exclusions for broad roots: proc/ sys/ dev/ run/ tmp/ mnt/ media/ (a trailing " +
			"slash prunes the subtree), passed with --exclude (repeatable) or through --exclude-from " +
			"FILE. Scanning the filesystem root without at least one exclusion argument is refused " +
			"with exit 2.",
		Args: usageArgs(scanArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runScan(cmd.Context(), cmd, opts, args[0])
		},
	}
}

// scanReport is the end-of-run report model - human lines and the
// --json document are two presentations of the same numbers
// (FR-021-style reporting; DESIGN_SPEC 10.3: this is the command
// RESULT, so it goes to stdout only).
//
// Dirs (T2.2, 2026-10-19): the count of currently-empty directories
// recorded in the durable directories table by this run. Adding the
// key is a DELIBERATE --json schema change - the frozen key-set tests
// in scan_hardening_test.go were updated consciously alongside it.
type scanReport struct {
	Root       string   `json:"root"`
	SetID      string   `json:"set_id"`
	RunID      int64    `json:"run_id"`
	Kind       string   `json:"kind"`
	Status     string   `json:"status"`
	Scanned    int      `json:"scanned"`
	Regular    int      `json:"regular"`
	Symlinks   int      `json:"symlinks"`
	Added      int      `json:"added"`
	Modified   int      `json:"modified"`
	Unchanged  int      `json:"unchanged"`
	Missing    int      `json:"missing"`
	Unreadable int      `json:"unreadable"`
	Changed    int      `json:"changed"`
	Dirs       int      `json:"dirs"`
	Bytes      int64    `json:"bytes"`
	Failures   []string `json:"failures,omitempty"`
}

// scanPhaseName is the progress phase name of scan's hash phase. It is
// used for BOTH the pool's initial indeterminate Phase event and the
// walk-completion totals event (percent-after-walk, DESIGN_SPEC
// 3.6/9.1/9.2). The names must match exactly: renderers recognise the
// totals event as an in-place upgrade of the RUNNING phase by name
// equality, so one constant owns both emissions.
const scanPhaseName = "scan(hash)"

// runScan is the scan service invocation (see the file doc for the
// wiring contracts; each stage below names the contract it honors).
// The named return feeds the deferred progress Done event's Failed
// signal (progress_done.go).
func runScan(ctx context.Context, cmd *cobra.Command, opts *rootOptions, rootArg string) (runErr error) {
	// --catalog is scan's one required input beyond ROOT (exit 2 when
	// missing: a scan with no catalog has nowhere to record anything).
	if opts.catalog == "" {
		return &report.UsageError{
			Command: cmd.CommandPath(),
			Msg:     "--catalog is required (path to the SQLite catalog file)",
		}
	}
	m, err := ingest.NewMatcher(opts.exclude, opts.excludeFrom)
	if err != nil {
		return err
	}
	rootAbs, err := filepath.Abs(rootArg)
	if err != nil {
		return fmt.Errorf("scan: resolve root %q: %w", rootArg, err)
	}
	rootAbs = filepath.Clean(rootAbs)

	// The run's ONE change-detection resolution (DESIGN_SPEC 3.5,
	// 2026-10-20): statfs the ROOT positional once, resolve the
	// comparison set (auto's CIFS-family rule, or the forced
	// --change-detection value), and thread the mode into the ingest
	// pipeline below. Resolution failure here is usage-class only (an
	// invalid flag value - normally caught eagerly by
	// validateGlobalFlags); a statfs failure on an unreadable root
	// falls back to the safe FULL set inside the resolver.
	cdMode, err := resolveChangeDetection(cmd, opts, rootAbs)
	if err != nil {
		return err
	}

	// MUTATING open: exclusive flock for the whole run (D-11);
	// contention classifies to exit 5 through report.Classify (T0.4).
	// AUTO-CREATE (PM-ratified 2026-10-19): a --catalog path that does
	// not exist is initialized as a fresh v1 catalog before the open;
	// see openScanCatalog for the concurrent-create race design.
	db, err := openScanCatalog(opts.catalog)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck // best-effort; the pipeline result dominates

	now := time.Now()
	setID := opts.setID
	if setID == "" {
		setID = deriveSetID(rootAbs, now)
	}
	if err := db.EnsureSet(ctx, setID, rootAbs, now.UnixNano(), catalog.ToolVersion); err != nil {
		if errors.Is(err, catalog.ErrSetRootMismatch) {
			// The user's --set-id (or a derived collision) disagrees
			// with the catalog's binding: an input mistake, exit 2.
			return &report.UsageError{Command: cmd.CommandPath(), Err: err}
		}
		return err
	}
	prior, err := db.PriorScanRuns(ctx, setID)
	if err != nil {
		return err
	}
	kind := "ingest"
	if prior > 0 {
		kind = "rescan"
	}
	patterns := m.Patterns()
	if patterns == nil {
		patterns = []string{} // exclude_json defaults to '[]', never 'null' (2.3)
	}
	excludeJSON, err := json.Marshal(patterns)
	if err != nil {
		return fmt.Errorf("scan: serialising exclusion snapshot: %w", err)
	}
	runID, err := db.StartScanRun(ctx, catalog.ScanRun{
		SetID:       setID,
		Kind:        kind,
		Root:        rootAbs,
		StartedAt:   now.UnixNano(),
		ExcludeJSON: string(excludeJSON),
	})
	if err != nil {
		return err
	}

	// Fresh progress bus per operation (T1.1 contract); renderer chosen
	// once here per the 9.2 matrix. The deferred Finish/wait pair
	// guarantees exactly one Done summary line on EVERY exit path.
	bus := progress.NewBus()
	wait := progress.Start(progress.New(progress.Options{
		Quiet:  opts.quiet,
		Stderr: cmd.ErrOrStderr(),
	}), bus)
	summary := fmt.Sprintf("scan: run %d aborted before any result was reconciled", runID)
	defer func() {
		// Failed / Interrupted mark the abort path so the interactive
		// renderer CLEARS its bar instead of rendering a misleading
		// 100% (progress_done.go). Since T6.1 scan DOES make the
		// explicit interrupted-status decision (run row 'interrupted'
		// stamped detached below, result wrapped in
		// report.ErrInterrupted); runInterrupted classifies either
		// shape. The two flags are mutually exclusive by construction
		// (runFailed is runInterrupted's complement).
		bus.Finish(progress.Done{Summary: summary, Failed: runFailed(runErr), Interrupted: runInterrupted(runErr)})
		wait()
	}()

	// The 3.5 once-per-run degradation warning (progress.Warn +
	// run_events), emitted at resolution - before any phase work -
	// and ONLY for an auto-detected CIFS-family degradation (see
	// changedetection.go for the forced-vs-detected distinction).
	warnChangeDetection(ctx, db, runID, bus, rootAbs, cdMode)

	// --- pipeline (T1.4 wiring story; single-producer contracts) ---
	// The resolved mode threads into the failure accumulator, whose
	// Process performs the ingest-side (point 1) post-hash comparison
	// under it.
	fails := ingest.NewFailures(bus, cdMode)
	recon := ingest.NewReconciler(scanTxFactory(db, runID, setID), runID)
	batcher := ingest.NewBatcher(ingest.BatcherOptions[ingest.ClassifiedResult]{
		// One flush = one reconciler batch = ONE BEGIN IMMEDIATE
		// catalog transaction (atomic batch, 3.7 + 3.3).
		Flush: func(batch []ingest.ClassifiedResult) error {
			return recon.ReconcileBatch(ctx, batch)
		},
	})
	// PhaseName scanPhaseName ties the pool's phase to the totals
	// emission below (9.3 example prefix); Workers 0 selects
	// min(GOMAXPROCS, 8) (3.3). The pool emits the phase's initial
	// indeterminate Phase event here, synchronously, BEFORE the walk
	// starts - so the totals event below can never precede it.
	pool := ingest.NewHashPool(ctx, ingest.PoolOptions{Bus: bus, PhaseName: scanPhaseName})

	// The single drain goroutine: the ONLY Batcher.Add caller
	// (single-producer contract). Classification happens here, at
	// result-processing time, which is where DESIGN_SPEC 3.5 point 1
	// places the post-hash re-lstat.
	//
	// Abort accounting (PM decision, defect C of 2026-10-19; comment
	// corrected in the 2026-10-19 review round - behavior unchanged,
	// the counts are honestly NAMED, this note just describes them
	// truthfully): completed counts the hash results that COMPLETED and
	// were DELIVERED to this drain, i.e. non-aborted results - the
	// aborted results that are delivered are excluded below. It can
	// UNDERCOUNT the progress line's completed-files count: a file that
	// finished hashing (its ItemDone was already emitted, so the
	// progress line counted it) can still have its result dropped by
	// the pool when the results channel is full at cancellation time,
	// and this drain never sees it. Conversely, aborted in-flight
	// hashes emit NO ItemDone (the pool godoc's progress policy), but
	// they DO deliver a ctx-canceled result when the results channel
	// has room - the "no result at all" claim this comment used to
	// make was wrong. It is written only by this goroutine and read
	// only after <-drainDone, so no extra synchronisation is needed.
	// ABORTED results (ctx cancelled mid-file) are dropped BEFORE
	// classification: an interrupted hash says nothing verifiable about
	// the file, so it gets no catalog row, no failure warning, and no
	// run_events entry (FR-014: never mark unverified data; the rescan
	// captures the file later).
	drainDone := make(chan error, 1)
	completed := 0
	go func() {
		var addErr error
		for r := range pool.Results() {
			if r.Aborted() {
				continue // cancellation-aborted in-flight hash: drop, never record
			}
			completed++
			c := fails.Process(r, nil)
			if addErr == nil {
				if err := batcher.Add(c); err != nil {
					addErr = err
				}
			}
		}
		if err := batcher.Close(); addErr == nil {
			addErr = err
		}
		drainDone <- addErr
	}()

	// The walk goroutine is the ONLY Pool.Submit caller (single-
	// producer contract); Walk is synchronous by T1.3 design.
	wres, walkErr := ingest.Walk(rootAbs, m, pool.Submit, bus)
	if walkErr == nil {
		// Percent-after-walk (DESIGN_SPEC 3.6/9.1/9.2, stakeholder
		// decision 2026-10-19): the walk completed, so the hash
		// phase's totals are now honestly known. Emit ONE totals
		// Phase event for the STILL-RUNNING hash phase: emission
		// only, no barrier - workers keep draining concurrently and
		// the buffered bus decouples the send. Renderers upgrade the
		// in-flight display in place (determinate percent + ETA for
		// the ~95 percent of the run that remains). A FAILED walk
		// emits nothing: the run is dying anyway and no totals are
		// honest.
		emitScanWalkTotals(bus, wres)
	}
	pool.Close()           // intake over (idempotent)
	waitErr := pool.Wait() // returns once every result has been consumed
	drainErr := <-drainDone

	var terminal error
	switch {
	case walkErr != nil:
		terminal = walkErr
	case waitErr != nil:
		terminal = waitErr
	case drainErr != nil:
		terminal = drainErr
	}

	// --- reconciliation tail (3.7; not-seen pass gated on clean completion) ---
	if terminal == nil {
		if err := recon.ReconcileSymlinks(ctx, wres.Symlinks); err != nil {
			terminal = err
		}
	}
	if terminal == nil {
		// CLEAN COMPLETION ONLY: walk clean + pool drained + every
		// batch flushed. An aborted scan never reaches this line, so
		// it can never mark rows missing (SAFETY, 3.7).
		recon.ScanComplete()
		// T2.2 empty-dir durability (2026-10-19 architect ruling): the
		// walk's empty-dir set is reconciled into the durable
		// directories table in the SAME run bookkeeping - each empty
		// dir upserted, every existing row no longer in the empty set
		// marked missing (deleted dirs AND dirs that gained files) -
		// and, like the file not-seen pass, only on clean completion.
		if _, err := recon.ReconcileEmptyDirs(ctx, wres.EmptyDirs); err != nil {
			terminal = err
		}
	}
	if terminal == nil {
		if _, err := recon.MarkMissing(ctx); err != nil {
			terminal = err
		}
	}

	// --- runs bookkeeping finalization (2.4) ---
	counts := recon.Counts()
	frep := fails.Report()
	// T6.1 interrupt semantics (DESIGN_SPEC 10.1, the T0.5/T1.6
	// interrupted-run deferral closure): a signal-cancelled run context
	// now stamps status='interrupted' - mirroring copy's and build's
	// finalize - instead of leaving the row 'running' forever (the
	// empirically-confirmed 2026-10-19 gap: FinishRun under the
	// ALREADY-CANCELLED run context is rejected by the SQLite driver).
	// interrupted outranks failed and partial, exactly like copy/build:
	// a cancelled context is the one state no other outcome can mask.
	interrupted := ctx.Err() != nil
	status := "ok"
	switch {
	case interrupted:
		status = "interrupted"
	case terminal != nil:
		status = "failed"
	case frep.Total > 0:
		status = "partial"
	}
	stats, merr := json.Marshal(struct {
		ingest.Counts
		Failures        int                  `json:"failures"`
		ChangeDetection changeDetectionAudit `json:"change_detection"`
	}{counts, frep.Total, auditOf(cdMode)})
	if merr != nil {
		stats = []byte("{}")
	}
	// FinishRunDetached (catalog/copystore.go): the terminal UPDATE runs
	// on a fresh, bounded, Background-derived context so it lands even
	// when the run context is already cancelled - the same detached
	// finalization the copy and build commands use on every exit path.
	// A finish failure on the abort/interrupt path is surfaced through
	// the returned abort error instead of masking the dominant verdict.
	finishErr := db.FinishRunDetached(runID, status, time.Now().UnixNano(), string(stats))
	if finishErr != nil {
		terminal = errors.Join(terminal, finishErr)
	}
	for _, wmsg := range wres.Warnings {
		if err := db.InsertRunEvent(ctx, catalog.RunEvent{
			RunID: runID, At: time.Now().UnixNano(), Level: "warn", Detail: wmsg,
		}); err != nil && terminal == nil {
			terminal = err
		}
	}
	for _, e := range fails.Entries() {
		if err := db.InsertRunEvent(ctx, catalog.RunEvent{
			RunID: runID, At: time.Now().UnixNano(), Level: "warn",
			RelPath: e.RelPath, Detail: e.Detail,
		}); err != nil && terminal == nil {
			terminal = err
		}
	}

	if terminal != nil {
		// Abort-summary accounting (PM decision, defect C of
		// 2026-10-19): the old wording said "aborted after N files"
		// with N = RECONCILED catalog rows, while the progress line's
		// N = COMPLETED (ItemDone-emitted) hashes - on an abort the
		// two legitimately differ (in-flight hashes abort without a
		// result, delivered results can be dropped or left
		// unflushed), which read as an accounting bug. The summary
		// now names BOTH available counts: reconciled rows
		// (counts.Scanned, speculative counts roll back with a failed
		// batch) and completed files (the delivered-hash count the
		// drain goroutine accumulated above).
		summary = fmt.Sprintf(
			"scan(hash): aborted: %d of %d completed files reconciled (remaining work discarded; a rescan will capture it): %v",
			counts.Scanned, completed, terminal)
		// T6.1 (10.1 Interrupted): a signal-aborted scan returns the
		// INTERRUPTED class explicitly - run row already stamped
		// 'interrupted' by the detached finalization above - carrying
		// the ORIGINAL abort cause in the chain (NB N1), so the
		// stderr output names what was actually interrupted. The
		// historical "interrupted by signal" wording comes from
		// report.ErrInterrupted itself; execute's context override
		// recognises this chain and never double-wraps it.
		if interrupted {
			return errors.Join(
				fmt.Errorf("%w: scan run %d interrupted (run status 'interrupted' recorded; a rescan resumes the set)",
					report.ErrInterrupted, runID),
				terminal)
		}
		return terminal
	}

	// --- end-of-run report (stdout only, FR-046; human + --json) ---
	rep := scanReport{
		Root: rootAbs, SetID: setID, RunID: runID, Kind: kind, Status: status,
		Scanned: counts.Scanned, Regular: counts.Regular, Symlinks: counts.Symlinks,
		Added: counts.Added, Modified: counts.Modified, Unchanged: counts.Unchanged,
		Missing: counts.Missing, Unreadable: counts.Unreadable, Changed: counts.Changed,
		Dirs: counts.Dirs, Bytes: counts.Bytes, Failures: frep.Lines,
	}
	// The Done summary is set BEFORE the report write (T6.1, the
	// deferred shared fix of TODO: a writeReport failure after a
	// successful reconciliation/commit must not leave a stale
	// "aborted before any result was reconciled" line claiming the run
	// never happened). If the stdout write itself fails, the line is
	// replaced by what actually happened: the reconciliation stands,
	// the stdout artifact failed.
	summary = fmt.Sprintf(
		"scan(hash): files %d, %s; added %d, modified %d, unchanged %d, missing %d, unreadable %d, changed %d, empty dirs %d",
		counts.Scanned, units.FormatBytes(counts.Bytes),
		counts.Added, counts.Modified, counts.Unchanged,
		counts.Missing, counts.Unreadable, counts.Changed, counts.Dirs)
	if err := writeScanReport(cmd.OutOrStdout(), rep, opts.json); err != nil {
		summary = fmt.Sprintf(
			"scan: run %d reconciled (status %s, %d files), but writing the report to stdout failed: %v",
			runID, status, counts.Scanned, err)
		return err
	}

	// Exit code (10.2): Failures.Err() is nil on a clean run (exit 0)
	// and a report.ItemFailures otherwise (exit 10); --strict promotes
	// item failures to a fatal exit 1.
	if f := fails.Err(); f != nil {
		if opts.strict {
			return fmt.Errorf("scan completed with %d item failure(s); --strict promotes this to a fatal error", frep.Total)
		}
		return f
	}
	return nil
}

// emitScanWalkTotals is the percent-after-walk emission site
// (DESIGN_SPEC 3.6/9.1/9.2, stakeholder decision 2026-10-19): one
// totals Phase event for the still-running hash phase the moment the
// walk completes. TotalFiles reuses the walk's Files count (every
// counted file emitted exactly one job - see WalkResult.Files);
// TotalBytes is the walker's sum of walk-time lstat sizes. The event
// names scanPhaseName, matching the pool's initial indeterminate
// Phase, so renderers treat it as an IN-PLACE upgrade of the current
// phase - never as a phase reset (no counter wipe, no new bar).
//
// Emission only, NO barrier: hash workers keep draining concurrently
// after this returns; the buffered bus (DESIGN_SPEC 9.1) makes the
// send safe from the walk goroutine with no synchronisation. Callers
// invoke it ONLY on successful walk completion - a failed walk keeps
// the indeterminate window because no totals are honest for a walk
// that did not finish.
func emitScanWalkTotals(bus *progress.Bus, wres ingest.WalkResult) {
	bus.Emit(progress.Phase{
		Name:       scanPhaseName,
		TotalKnown: true,
		TotalFiles: int64(wres.Files),
		TotalBytes: wres.TotalBytes,
	})
}

// openScanCatalog opens the scan catalog MUTATING, auto-creating a
// fresh v1 catalog at path when the file does not exist (PM-ratified
// scope item, 2026-10-19). An existing catalog is opened as before.
//
// The create sequence is os.Stat -> catalog.Create -> catalog.Open,
// designed so two concurrent scans racing on the same missing path
// always end with at least one completed scan (exit 0) and every loser
// in a cleanly classified state - exit 4 (*catalog.CatalogError) or
// exit 5 (*catalog.CatalogBusy) - never a panic and never a torn file:
//
//   - Baseline: Stat reports the path absent, Create initializes the
//     file in place (schema v1 applied as ONE transactional migration
//     under the 2.1 connection policy, busy_timeout=5000 serializing
//     concurrent SQLite writers), then Open takes the exclusive flock
//     and proceeds exactly as before.
//   - Stat-then-Create race: if another scan creates the file between
//     our Stat and our Create, Create's OWN re-stat refuses to
//     overwrite any existing NON-EMPTY file with a clean
//     *catalog.CatalogError (exit 4) - the loser fails before opening,
//     so it can never truncate or double-initialize the winner's
//     catalog.
//   - Create-then-Create race (both creations observe the still-empty
//     file): their migration transactions serialize on the SQLite
//     write lock; the loser's DDL then fails ("table already exists")
//     INSIDE its transaction, which rolls back completely - the file
//     holds exactly the winner's full schema and the loser surfaces a
//     clean CatalogError (KindMigration, exit 4).
//   - Create-then-Open race: a scan that finds the catalog already
//     created skips Create and goes straight to Open, where the
//     exclusive flock admits exactly one mutating run - the contended
//     scan gets *catalog.CatalogBusy (exit 5), never a blocking wait.
//   - Residual window (the one retry below): a Stat that observed the
//     file mid-INITIALIZATION - SQLite has already written the header,
//     so the file is non-empty, but the creator's schema transaction
//     has not committed yet - skips Create and lets Open see a
//     version-0 file; Open's own migration path then races the
//     creator's in-flight DDL and fails with KindMigration once the
//     creator commits ("table ... already exists"), transiently
//     holding the flock while it does so (which can push the creator
//     itself into a clean busy-5). The single retry closes this: by
//     the time the KindMigration failure surfaces, the creator's
//     transaction has committed, so re-Opening sees schema v1, skips
//     migration, and succeeds. Every other error (busy, corrupt,
//     unreadable, schema-too-new) is returned unchanged - the retry
//     can only turn the mid-initialization race into the clean open it
//     was always meant to be, and a genuine migration failure of an
//     old catalog simply fails twice with the same clean exit 4.
func openScanCatalog(path string) (*catalog.DB, error) {
	// Any Stat failure other than "not exist" (permission, IO) falls
	// through to Open, which classifies it as a KindOpen CatalogError
	// ("not found or unreadable") - no error is invented here.
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if cerr := catalog.Create(path); cerr != nil {
			return nil, cerr
		}
	}
	db, err := catalog.Open(path, catalog.OpenReadWrite)
	if err == nil {
		return db, nil
	}
	var cerr *catalog.CatalogError
	if errors.As(err, &cerr) && cerr.Kind == catalog.KindMigration {
		// See the residual-window note above: one retry, no wait - the
		// peer's create has fully committed by the time this error can
		// occur.
		return catalog.Open(path, catalog.OpenReadWrite)
	}
	return nil, err
}

// writeScanReport renders the end-of-run report: --json emits exactly
// one JSON document on stdout; the human form is counts lines plus one
// line per failure (FR-021-style enumeration; 10.3: stdout carries the
// result only).
//
// The human form's second line is the set/run identity ("set: <set_id>
// (run <run_id>)"), placed after the counts headline in the family
// style of plan's and clear's "plan: set S1 (...)"/"clear: set S1"
// headers. The set id is REQUIRED by every follow-up command
// (plan/report/clear-assignments take --set-id) yet the 6.4
// date-derived default is never echoed anywhere else, so the finished
// scan output is where the user learns it (stakeholder request
// 2026-10-19). The identity already travels in the report struct -
// no plumbing. The --json path is UNTOUCHED (frozen key-set schema).
func writeScanReport(w io.Writer, rep scanReport, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	if _, err := fmt.Fprintf(w,
		"scanned %d entries (%d regular, %d symlinks), %s hashed\n",
		rep.Scanned, rep.Regular, rep.Symlinks, units.FormatBytes(rep.Bytes)); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "set: %s (run %d)\n", rep.SetID, rep.RunID); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w,
		"added %d, modified %d, unchanged %d, missing %d, unreadable %d, changed %d\n",
		rep.Added, rep.Modified, rep.Unchanged, rep.Missing, rep.Unreadable, rep.Changed); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "empty dirs %d\n", rep.Dirs); err != nil {
		return err
	}
	for _, line := range rep.Failures {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

// deriveSetID implements the 6.4 default: <sanitized-basename-of-
// source-root>-<ingest-date-UTC> (example: home-martin-20261018).
// Sanitization maps every character outside [A-Za-z0-9._-] to '-'; a
// basename that sanitizes to nothing (or to ".") becomes "root" so the
// id is never empty.
func deriveSetID(absRoot string, at time.Time) string {
	base := filepath.Base(absRoot)
	san := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '.', r == '_':
			return r
		default:
			return '-'
		}
	}, base)
	if san == "" || san == "." || san == ".." {
		san = "root"
	}
	return fmt.Sprintf("%s-%s", san, at.UTC().Format("20060102"))
}

// --- ingest.Store adapter (pure type mapping; no SQL here) ------------------

// scanStoreTx adapts *catalog.ScanTx to the ingest.Store interface
// field for field. The duplication exists by design: internal/ingest
// declares the interface and its own record types so the reconciler
// never imports the catalog (DESIGN_SPEC 1.2: services depend on
// interfaces for catalog access, never on the SQL layer directly), and
// this wiring layer - the one place allowed to know both sides -
// translates.
type scanStoreTx struct{ tx *catalog.ScanTx }

func (s scanStoreTx) ActiveByRelPaths(ctx context.Context, relpaths []string) (map[string]ingest.ActiveRecord, error) {
	afs, err := s.tx.ActiveByRelPaths(ctx, relpaths)
	if err != nil {
		return nil, err
	}
	out := make(map[string]ingest.ActiveRecord, len(afs))
	for rel, af := range afs {
		out[rel] = ingest.ActiveRecord{
			FileID:     af.FileID,
			RelPath:    af.RelPath,
			EntryKind:  af.EntryKind,
			Hash:       af.Hash,
			LinkTarget: af.LinkTarget,
			Status:     af.Status,
		}
	}
	return out, nil
}

func (s scanStoreTx) InsertScanFile(ctx context.Context, rec ingest.FileRow) (int64, error) {
	return s.tx.InsertScanFile(ctx, toCatalogRow(rec))
}

func (s scanStoreTx) UpdateSeen(ctx context.Context, fileID int64) error {
	return s.tx.UpdateSeen(ctx, fileID)
}

func (s scanStoreTx) InsertModification(ctx context.Context, oldFileID int64, rec ingest.FileRow) (int64, error) {
	return s.tx.InsertModification(ctx, oldFileID, toCatalogRow(rec))
}

func (s scanStoreTx) ListActive(ctx context.Context) ([]ingest.ActiveRecord, error) {
	afs, err := s.tx.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ingest.ActiveRecord, len(afs))
	for i, af := range afs {
		out[i] = ingest.ActiveRecord{
			FileID:     af.FileID,
			RelPath:    af.RelPath,
			EntryKind:  af.EntryKind,
			Hash:       af.Hash,
			LinkTarget: af.LinkTarget,
			Status:     af.Status,
		}
	}
	return out, nil
}

func (s scanStoreTx) MarkMissing(ctx context.Context, fileIDs []int64) (int, error) {
	return s.tx.MarkMissing(ctx, fileIDs)
}

// T2.2 empty-dir durability: the dir half of the ingest.Store surface,
// mapped field for field onto catalog.ScanTx (same pure-mapping
// contract as the file half above).

func (s scanStoreTx) UpsertEmptyDir(ctx context.Context, rec ingest.DirRow) error {
	return s.tx.UpsertEmptyDir(ctx, toCatalogDirRow(rec))
}

func (s scanStoreTx) ListActiveDirs(ctx context.Context) ([]ingest.ActiveDir, error) {
	dirs, err := s.tx.ListActiveDirs(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]ingest.ActiveDir, len(dirs))
	for i, d := range dirs {
		out[i] = ingest.ActiveDir{DirID: d.DirID, RelPath: d.RelPath}
	}
	return out, nil
}

func (s scanStoreTx) MarkDirsMissing(ctx context.Context, dirIDs []int64) (int, error) {
	return s.tx.MarkDirsMissing(ctx, dirIDs)
}

// toCatalogDirRow maps the ingest empty-dir record shape onto the
// catalog one.
func toCatalogDirRow(rec ingest.DirRow) catalog.DirRow {
	return catalog.DirRow{
		RelPath: rec.RelPath,
		Mode:    rec.Mode,
		UID:     rec.UID,
		GID:     rec.GID,
		MtimeNS: rec.MtimeNS,
	}
}

// toCatalogRow maps the ingest record shape onto the catalog one.
func toCatalogRow(rec ingest.FileRow) catalog.FileRow {
	return catalog.FileRow{
		RelPath:      rec.RelPath,
		EntryKind:    rec.EntryKind,
		Size:         rec.Size,
		Mode:         rec.Mode,
		UID:          rec.UID,
		GID:          rec.GID,
		MtimeNS:      rec.MtimeNS,
		Dev:          int64(rec.Dev),
		Inode:        int64(rec.Inode),
		Nlink:        int64(rec.Nlink),
		LinkTarget:   rec.LinkTarget,
		Hash:         rec.Hash,
		Status:       rec.Status,
		StatusDetail: rec.StatusDetail,
	}
}

// scanTxFactory binds a BeginScanTx transaction to the ingest.Tx
// shape: one BEGIN IMMEDIATE catalog transaction per reconciler batch
// (and one for the not-seen pass).
func scanTxFactory(db *catalog.DB, runID int64, setID string) ingest.TxFactory {
	return func(ctx context.Context) (*ingest.Tx, error) {
		tx, err := db.BeginScanTx(ctx, runID, setID)
		if err != nil {
			return nil, err
		}
		return &ingest.Tx{
			Store:    scanStoreTx{tx: tx},
			Commit:   tx.Commit,
			Rollback: func() { _ = tx.Rollback() },
		}, nil
	}
}
