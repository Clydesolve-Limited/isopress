package cli

// T4.3 (wiring half) + T4.4 scope: the real `build` command - the
// Sprint 4 capstone that wires the xorriso discovery (T4.1), the build
// phase (internal/imagebuild build.go), the T3.2 catalog export, the
// T3.1 manifest input, and the new internal/verify extraction
// primitive into the working mastering workflow (DESIGN_SPEC 1.4 write
// data flow in image mode, 6.2 to 6.6, 7.1/7.2, 10.2 exit codes,
// 10.3/FR-046 stream separation). It replaces the T0.5 placeholder
// command in commands.go.
//
// Layering (DESIGN_SPEC 1.2): flag parsing, row-shape mapping, service
// invocation, and report presentation only - no SQL here (the catalog
// package is the only SQL issuer), no rendering (progress flows
// through the bus; the result lands on stdout, progress on stderr).
// The row-shape mappings between catalog types and imagebuild types
// live HERE, at the one layer allowed to know both sides (the copy.go
// precedent); the same applies to the two seams over the xorriso
// wrapper and the catalog store (adapters below).
//
// Wiring contracts honored here:
//
//   - MUTATING catalog open (exclusive flock, D-11); contention maps
//     to exit 5. A missing catalog is NOT auto-created (scan's job).
//   - Flags: --set-id and --catalog required; --dest required (the
//     directory <SET-ID>-NNN.iso lands in - the design names "dest"
//     for images per the copy precedent and 6.3/6.4's <dest> path);
//     --image N repeatable to build a subset (default: every planned
//     image, ascending); --clean-failed (6.3 step 4: remove the
//     .partial on failure instead of keeping it for inspection);
//     --verify-after-build (default TRUE - see the V1 reading below);
//     --work-dir (global) seeds the staging and extraction scratches.
//   - The F2 gate (the T4.2 review carry-in, mirrored at CLI entry so
//     a bad --set-id fails before ANY database or filesystem work):
//     a set id containing '\n' or '\r' is a usage error; '=' is
//     allowed (xorriso -V takes it as free text; the argv is
//     injection-proof by the T4.1 no-shell contract).
//   - Dependency fail-fast (NFR-004, T4.1): xorriso Discover + Probe
//     run ONLY here (image operations), before the run row - a missing
//     or too-old binary is a typed DependencyError (exit 3) with the
//     per-distro install hints, and copy-only workflows never pay it.
//   - Runs (2.4): kind 'build', root = --dest; every exit path
//     finalizes via FinishRunDetached (the T0.5/T1.6 interrupted-run
//     closure).
//   - The run's single FR-034 export (6.6/FR-035): one set-scoped
//     VACUUM-INTO snapshot per RUN (the T3.2 fresh-path contract),
//     embedded identically into every image of the run by the builder,
//     removed at run end.
//   - Per image: catalog.CopyInventory -> the D-04 ISO inventory
//     (Secondaries DROPPED - ISO mode stores the family primary only;
//     the manifest/export record the full link group), the whole-set
//     placement + hard-link families fetched ONCE per run, then
//     imagebuild.Build runs the 6.3 pipeline (stage, embed, content
//     transaction, master, extraction self-verify, atomic finalize).
//   - N3 exit-class wiring: a per-image build failure (including
//     SourceChangedError from staging and the xorriso BuildError) is
//     ITEM-FAILURE accounting - recorded, the run continues with the
//     remaining images, exit 10 via report.ItemFailures at the end
//     (--strict promotes to 1). A usage-class failure from the builder
//     (the 6.3 step 5 capacity pre-check, the --work-dir gate) stops
//     the run as terminal (exit 2): the input is unsatisfiable and the
//     remaining images of a homogeneous set would fail identically.
//     Interruption (SIGINT/SIGTERM) stops the loop; the interrupted
//     image's state was already failed detached by the builder, and
//     the run exits 130.
//   - Progress (FR-042): the FOUR NAMED per-image phases of the 6.2
//     four-phase model (build(stage), build(master), build(extract),
//     build(verify) - all determinate where known; service-owned since
//     the 2026-10-20 "looks hung" amendment, see buildImage), ItemDone
//     events from staging through the bus, Warn per metadata
//     downgrade, Error per image failure, and exactly one Done summary
//     line on EVERY exit path.
//   - Report (FR-046): human and --json on stdout with a FROZEN key
//     set (the scan/plan/copy convention): image numbers, iso paths,
//     bytes, content/verified entry counts, warnings, failures.
//
// The --verify-after-build V1 reading (documented decision, restated
// at the flag): the flag gates the builder's EXTRACTION SELF-VERIFY
// PASS (6.3 step 2 / 7.1) - default ON. --verify-after-build=false
// skips that pass for speed on huge images, trading the extraction
// guarantee (the image is renamed and marked 'built' on xorriso's exit
// 0 alone); the report marks such images (verify_skipped). T5.1's
// verify-media gives the flag its full 6.3-step-3 meaning later.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"isopress/internal/catalog"
	"isopress/internal/fsmeta"
	"isopress/internal/imagebuild"
	"isopress/internal/media"
	"isopress/internal/progress"
	"isopress/internal/report"
	"isopress/internal/units"
	"isopress/internal/verify"
	"isopress/internal/xorriso"
)

// buildOptions holds the build-local flag values.
type buildOptions struct {
	dest             string  // --dest: directory the .iso files land in
	images           []int64 // --image: repeatable image selection (default: all planned)
	cleanFailed      bool    // --clean-failed: remove the .partial on failure (6.3 step 4)
	verifyAfterBuild bool    // --verify-after-build: the step-7 self-verify (default true)
}

// newBuildCommand builds the real build command (replaces the T0.5
// stub).
func newBuildCommand(opts *rootOptions) *cobra.Command {
	bo := buildOptions{verifyAfterBuild: true}
	cmd := &cobra.Command{
		Use:   "build",
		Short: "master planned destinations into verified ISO 9660 images",
		Long: "Master every planned image of the --set-id set into <dest>/<SET-ID>-NNN.iso " +
			"(DESIGN_SPEC 6.4 naming; ISO 9660 level 3 + Rock Ridge + Joliet per 6.1/6.2). " +
			"Each image is staged, embedded with MANIFEST.json and the run's single catalog " +
			"export, recorded in the image_contents content list in the SAME transaction that " +
			"sets state 'building' (6.6), mastered into a .partial, extraction-self-verified " +
			"against that content list (6.3 step 2, 7.1/7.2 - xorriso osirrox, no mount), and " +
			"only then fsynced, atomically renamed, and stamped state 'built' with built_at " +
			"(6.3 step 3). A failed image keeps its .partial for inspection unless " +
			"--clean-failed and is stamped state 'failed'; --image N " +
			"builds only that image (repeatable; default: every planned image). " +
			"--verify-after-build defaults ON and gates the extraction self-verify pass; " +
			"--verify-after-build=false skips it for speed, trading the content guarantee " +
			"(documented V1 semantics). An interrupted run finalizes its run row as " +
			"'interrupted'.\n\n" +
			"Exit codes: 0 success; 1 fatal unexpected error; 2 usage (bad flags, an unplanned set); " +
			"3 xorriso missing or older than 1.5.4; 4 catalog error; 5 catalog busy (another mutating " +
			"run holds the lock); 10 completed with failed images, with the failed .partial kept " +
			"unless --clean-failed (--strict promotes this to 1); 130 interrupted. JSON: --json " +
			"prints the final report as exactly one JSON document on stdout; a run that aborts before " +
			"doing work prints no document at all; --json never changes the exit code.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBuild(cmd.Context(), cmd, opts, bo)
		},
	}
	cmd.Flags().StringVar(&bo.dest, "dest", "",
		"directory the <SET-ID>-NNN.iso images land in (required; e.g. /mnt/backup/images)")
	cmd.Flags().Int64SliceVar(&bo.images, "image", nil,
		"build only this planned image number (repeatable; default: every planned image)")
	cmd.Flags().BoolVar(&bo.cleanFailed, "clean-failed", false,
		"remove the .partial image of a failed build instead of keeping it for inspection (DESIGN_SPEC 6.3 step 4)")
	cmd.Flags().BoolVar(&bo.verifyAfterBuild, "verify-after-build", true,
		"run the extraction self-verify pass after mastering (DESIGN_SPEC 6.3 step 2, default on); "+
			"--verify-after-build=false skips it for speed, trading the content verification guarantee")
	return cmd
}

// runBuild is the build service invocation (see the file doc for the
// wiring contracts). The named return feeds the deferred progress Done
// event's Failed signal (progress_done.go).
func runBuild(ctx context.Context, cmd *cobra.Command, opts *rootOptions, bo buildOptions) (runErr error) {
	// --- usage-class inputs (exit 2, 10.1) ---
	if opts.catalog == "" {
		return &report.UsageError{Command: cmd.CommandPath(), Msg: "--catalog is required (path to the SQLite catalog file)"}
	}
	if opts.setID == "" {
		return &report.UsageError{Command: cmd.CommandPath(), Msg: "--set-id is required (the planned set to master)"}
	}
	if bo.dest == "" {
		return &report.UsageError{Command: cmd.CommandPath(), Msg: "--dest is required (directory for the <SET-ID>-NNN.iso images)"}
	}
	// THE F2 GATE at CLI entry (the file doc): a newline in the set id
	// breaks both -V and the 6.4 output filename; refuse before any
	// database or filesystem work. The builder re-checks (defense in
	// depth). '=' is deliberately allowed.
	if i := strings.IndexAny(opts.setID, "\n\r"); i >= 0 {
		return &report.UsageError{Command: cmd.CommandPath(), Msg: fmt.Sprintf(
			"--set-id %q contains the byte %q (0x%02x), which breaks both the ISO volume id (-V) and the "+
				"image filename of the 6.4 naming scheme; pass a set id without newline or carriage return "+
				"(nothing was built - '=' is allowed: xorriso -V takes it as free text)",
			opts.setID, string(opts.setID[i]), opts.setID[i])}
	}
	if err := checkTilde(bo.dest, "--dest"); err != nil {
		return &report.UsageError{Command: cmd.CommandPath(), Err: err}
	}
	destAbs, err := filepath.Abs(bo.dest)
	if err != nil {
		return &report.UsageError{Command: cmd.CommandPath(), Err: fmt.Errorf("--dest %q: %w", bo.dest, err)}
	}
	destAbs = filepath.Clean(destAbs)
	// The set id becomes the image filename (6.4); gate it once here
	// (the media validator is the single naming authority).
	if _, err := media.ImageISOFileName(opts.setID, 1); err != nil {
		return &report.UsageError{Command: cmd.CommandPath(), Err: err}
	}

	// MUTATING open: exclusive flock for the whole run (D-11);
	// contention classifies to exit 5 through report.Classify (T0.4).
	db, err := catalog.Open(opts.catalog, catalog.OpenReadWrite)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck // best-effort; the run result dominates

	prior, err := db.PriorScanRuns(ctx, opts.setID)
	if err != nil {
		return err
	}
	if prior == 0 {
		return &report.UsageError{
			Command: cmd.CommandPath(),
			Msg: fmt.Sprintf("no scanned set %q in catalog %q (run 'isopress scan --catalog %s --set-id %s ROOT' first)",
				opts.setID, opts.catalog, opts.catalog, opts.setID),
		}
	}
	sourceRoot, err := db.SetSourceRoot(ctx, opts.setID)
	if err != nil {
		if errors.Is(err, catalog.ErrNoSuchSet) {
			return &report.UsageError{Command: cmd.CommandPath(), Err: err}
		}
		return err
	}
	// The run's ONE change-detection resolution (DESIGN_SPEC 3.5,
	// 2026-10-20), taken against the CATALOG's source_root (the path
	// the staging reads resolve against - runtime derivation, not the
	// stored scan-time class). Threaded into every staging comparison
	// through BuildInput.Mode.
	cdMode, err := resolveChangeDetection(cmd, opts, sourceRoot)
	if err != nil {
		return err
	}
	ids, err := db.CopyImageIDs(ctx, opts.setID)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return &report.UsageError{
			Command: cmd.CommandPath(),
			Msg: fmt.Sprintf("set %q has no planned images (run 'isopress plan --catalog %s --set-id %s --size ...' first)",
				opts.setID, opts.catalog, opts.setID),
		}
	}
	selected, err := selectCopyImages(cmd, ids, bo.images)
	if err != nil {
		return err
	}
	usable, err := db.Destinations(ctx, opts.setID)
	if err != nil {
		return err
	}
	usableByImage := make(map[int64]int64, len(usable))
	for _, d := range usable {
		usableByImage[d.ImageID] = d.UsableSize
	}

	// NFR-004 fail-fast: xorriso discovery + the >= 1.5.4 version
	// floor BEFORE the run row - a dependency failure mints no run
	// debris, and copy-only workflows never reach this line.
	tool, err := xorriso.Discover()
	if err != nil {
		return err
	}
	if _, err := tool.Probe(ctx); err != nil {
		return err
	}

	// Runs bookkeeping (2.4): kind 'build', root = the destination.
	runID, err := db.StartBuildRun(ctx, catalog.BuildRun{
		SetID: opts.setID, Root: destAbs, StartedAt: time.Now().UnixNano(),
	})
	if err != nil {
		if errors.Is(err, catalog.ErrNoSuchSet) {
			return &report.UsageError{Command: cmd.CommandPath(), Err: err}
		}
		return err
	}

	// Fresh progress bus per operation (T1.1); renderer per the 9.2
	// matrix. The deferred Finish/wait pair guarantees exactly one
	// Done summary line on EVERY exit path.
	bus := progress.NewBus()
	wait := progress.Start(progress.New(progress.Options{
		Quiet:  opts.quiet,
		Stderr: cmd.ErrOrStderr(),
	}), bus)
	summary := fmt.Sprintf("build: run %d aborted before any image was mastered", runID)
	defer func() {
		// Failed / Interrupted mark the abort path so the interactive
		// renderer CLEARS its bar instead of rendering a misleading
		// 100% (progress_done.go; the 2026-10-20 stakeholder failure
		// saw "0 B/21.4 GiB 100%" on a refused build, and an
		// interrupted run rendered the identical lie). build's
		// finalize makes the EXPLICIT interrupted-status decision
		// (ctx.Err() != nil -> run row 'interrupted', result wrapped
		// in report.ErrInterrupted); runInterrupted classifies
		// exactly that shape, and runFailed - its complement - covers
		// the rest (including the per-image failure tail that exits
		// 10), so the two flags are mutually exclusive by
		// construction.
		bus.Finish(progress.Done{Summary: summary, Failed: runFailed(runErr), Interrupted: runInterrupted(runErr)})
		wait()
	}()

	// THE HEARTBEAT (DESIGN_SPEC 9.1 Tick, the 2026-10-20 QA
	// heartbeat fold-in): ONE goroutine for the whole run, emitting
	// progress.Tick every HeartbeatInterval. It fills the silent await
	// points the phases' own events cannot: ItemDone is per-FILE, so
	// build(verify)'s hash walk of one huge member (a 20 GiB image
	// entry) and build(stage)'s intra-member copy freeze the display
	// for tens of seconds between events - the exact "is it hung?"
	// stretch the run-level heartbeat exists to cover. The phases' own
	// watchers (imagebuild's master byte-watcher, verify's tree-sum
	// watcher) already emit where they exist; the heartbeat only fills
	// the gaps, so one run-level goroutine is sufficient and simpler
	// than per-phase tickers.
	//
	// Registered AFTER the Finish defer above, so LIFO runs it FIRST:
	// the heartbeat stops before the terminal Done is emitted - a Tick
	// can never trail the summary (the no-heartbeat-after-Done
	// contract, pinned by build_heartbeat_test.go).
	stopHeartbeat := startHeartbeat(ctx, bus)
	defer stopHeartbeat()

	// The 3.5 once-per-run degradation warning (progress.Warn +
	// run_events); auto-detected degradations only.
	warnChangeDetection(ctx, db, runID, bus, sourceRoot, cdMode)

	r := &buildRunner{
		cmd:              cmd,
		db:               db,
		setID:            opts.setID,
		sourceRoot:       sourceRoot,
		cdMode:           cdMode,
		destAbs:          destAbs,
		workDir:          opts.workDir,
		runID:            runID,
		bus:              bus,
		xor:              buildXorriso{tool: tool},
		cleanFailed:      bo.cleanFailed,
		verifyAfterBuild: bo.verifyAfterBuild,
		usableByImage:    usableByImage,
		jsonFlag:         opts.json,
		strict:           opts.strict,
	}

	// THE EXPORT, ONCE PER RUN (6.6/FR-035, the file doc): one
	// set-scoped VACUUM-INTO snapshot into a fresh temp path; the SAME
	// bytes embed into every image below; the temp is removed at run
	// end.
	export, err := produceRunExport(ctx, db, opts.setID, opts.workDir, "isopress-build-export-")
	if err != nil {
		return r.finalize(ctx, &summary, err)
	}
	defer os.RemoveAll(export.dir) //nolint:errcheck // scratch; the embedded copies are published

	// The whole-set manifest derivations, fetched ONCE per run (FR-033:
	// every image's manifest carries the whole-set placement; D-04:
	// every manifest carries the full link groups).
	placement, err := db.CopyPlacement(ctx, opts.setID)
	if err != nil {
		return r.finalize(ctx, &summary, err)
	}
	families, err := db.CopyHardLinkFamilies(ctx, opts.setID)
	if err != nil {
		return r.finalize(ctx, &summary, err)
	}
	r.placeInputs = copyPlacementInputs(placement)
	r.familyInputs = copyFamilyInputs(families)

	// --- the per-image build loop -----------------------------------
	for _, imageID := range selected {
		if ctx.Err() != nil {
			break // interrupted: stop scheduling new images (10.1)
		}
		if err := r.buildImage(ctx, imageID, export); err != nil {
			// buildImage records per-image failures itself and returns
			// nil for them (the N3 continue semantics); a RETURNED
			// error is terminal-class plumbing or usage-class input -
			// stop the loop, finalize, and let the exit mapping speak.
			r.terminal = errors.Join(r.terminal, err)
			break
		}
	}

	return r.finalize(ctx, &summary, nil)
}

// HeartbeatInterval is the build run's heartbeat cadence (the
// DESIGN_SPEC 9.1 Tick event; the 2026-10-20 QA heartbeat fold-in):
// one progress.Tick every HeartbeatInterval for the WHOLE run, filling
// the silent await points where a single long operation would freeze
// the progress display - chiefly build(verify)'s hash walk of one huge
// member and build(stage)'s intra-member copy, both per-FILE-silent
// stretches of 10-25s on real images.
//
// Relationship to progress.PlainInterval (the DESIGN_SPEC 9.3 line
// cadence): the two constants are DELIBERATELY equal in value (5s) but
// distinct in role and tuning. PlainInterval throttles the plain
// renderer's LINE frequency (at most one line per interval per
// operation); HeartbeatInterval sets the EMIT frequency of this
// goroutine. They are not the same constant because they tune
// independently: emitting heartbeats at 2s would still yield one plain
// line per PlainInterval window (the renderer's throttle owns line
// frequency), while raising PlainInterval to 10s would still let the
// interactive bar repaint every HeartbeatInterval (that renderer has no
// time throttle). At the equal defaults the two compose to exactly one
// honest plain heartbeat line per interval: a Tick line routes through
// the same maybeLine throttle as an ItemDone line, so heartbeats can
// never double up with or crowd out real progress lines (pinned by
// internal/progress/heartbeat_test.go and build_heartbeat_test.go).
const HeartbeatInterval = 5 * time.Second

// startHeartbeat starts the run's one heartbeat goroutine (see
// HeartbeatInterval for the design): every HeartbeatInterval it emits
// progress.Tick{} on the bus until stop is called or ctx is
// cancellation-stopped. The FIRST tick fires only after one full
// interval (time.Ticker semantics): a run that is not yet silent gets
// no gratuitous pre-phase tick, so "no line before the first tick"
// holds by construction.
//
// nil-bus safe (the Deps nil-safety convention, the imagebuild
// watcher pattern): with no bus there is nothing to emit on, no
// goroutine is started, and the returned stop is a callable no-op.
//
// stop is idempotent and WAITS for the goroutine to exit (the no-leak
// contract; the settle-poll tests see no residue), and ctx
// cancellation stops it promptly (the select in startHeartbeatOn) - an
// interrupted build's heartbeat retracts on the same signal as the
// rest of the run.
func startHeartbeat(ctx context.Context, bus *progress.Bus) (stop func()) {
	if bus == nil {
		return func() {}
	}
	// The ticker lives HERE (not inside the goroutine) so the core
	// (startHeartbeatOn) can be driven by a test-controlled tick
	// channel - the fake-clock seam for cadence (DESIGN_SPEC 12.1's
	// pattern). stopCore waits for the goroutine exit before the
	// ticker is stopped, so the two lifecycles cannot race.
	ticker := time.NewTicker(HeartbeatInterval)
	stopCore := startHeartbeatOn(ctx, ticker.C, bus.Emit)
	return func() {
		stopCore()
		ticker.Stop()
	}
}

// startHeartbeatOn is the testable core of startHeartbeat: the
// goroutine consumes tick values from ticks and emits exactly one
// progress.Tick per value through emit, until ctx is done or stop is
// called. The injected tick channel is the cadence seam - production
// feeds HeartbeatInterval's ticker; tests feed a fake clock's tick
// delivery, so cadence assertions never depend on wall-clock time.
// emit nil means silent (no goroutine at all); a nil ticks channel
// simply never fires, which is the honest degenerate case.
func startHeartbeatOn(ctx context.Context, ticks <-chan time.Time, emit func(progress.Event)) (stop func()) {
	if emit == nil {
		return func() {} // nil-safe sink: nothing to emit on (Deps convention)
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-ctx.Done(): // prompt stop on interruption/cancellation
				return
			case <-ticks:
				emit(progress.Tick{})
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			wg.Wait() // stop WAITS for the goroutine exit (the no-leak contract)
		})
	}
}

// buildXorriso adapts one *xorriso.Tool onto the builder's two seams
// (the DESIGN_SPEC 1.2 interface rule: the service never touches the
// concrete wrapper; the cli layer owns the mapping). Both methods pass
// the caller's context through, so T4.1's cancellation (process kill)
// and the raw ctx-error contract stay intact.
type buildXorriso struct{ tool *xorriso.Tool }

// Run implements imagebuild.Master: the captured combined output
// travels even on failure (the builder folds the tail into the
// run_events detail).
func (x buildXorriso) Run(ctx context.Context, args []string) ([]byte, error) {
	res, err := x.tool.Run(ctx, args)
	return res.Output, err
}

// ExtractAndHash implements imagebuild.Extractor via the internal/verify
// primitive (T4.3's first content of that package). Bus and totals pass
// through so the primitive emits the build(extract)/build(verify)
// progress events with the builder's exact content-list figures (6.2
// four-phase progress).
func (x buildXorriso) ExtractAndHash(ctx context.Context, imagePath, destDir string, bus *progress.Bus, totals verify.Totals) (verify.ExtractResult, error) {
	return verify.ExtractAndHash(ctx, x.tool, imagePath, destDir, bus, totals)
}

// cliBuildStore adapts *catalog.BuildStore onto imagebuild.BuildStore
// (the copyengine.Store structural-satisfaction pattern: primitives on
// the interface, the typed mapping here at the one layer that knows
// both sides).
type cliBuildStore struct{ bs *catalog.BuildStore }

func (a cliBuildStore) ChunkHashes(ctx context.Context, fileIDs []int64, chunkIndexes []int) ([][]byte, error) {
	return a.bs.ReadChunkHashes(ctx, fileIDs, chunkIndexes)
}

func (a cliBuildStore) BeginBuild(ctx context.Context, rows []imagebuild.ContentRow) error {
	out := make([]catalog.BuildContentRow, len(rows))
	for i, r := range rows {
		out[i] = catalog.BuildContentRow{
			MediaPath: r.MediaPath, Kind: r.Kind, FileID: r.FileID,
			ChunkIndex: r.ChunkIndex, Size: r.Size, Hash: r.Hash,
		}
	}
	return a.bs.BeginBuildTx(ctx, out)
}

func (a cliBuildStore) MarkFailed(ctx context.Context, detail string) error {
	return a.bs.MarkBuildFailed(ctx, detail)
}

func (a cliBuildStore) FinishBuild(ctx context.Context, finalPath string, builtAt, bytesWritten int64) error {
	return a.bs.FinishBuildTx(ctx, finalPath, builtAt, bytesWritten)
}

// buildRunner carries the run-scoped state through the image loop.
type buildRunner struct {
	cmd        *cobra.Command
	db         *catalog.DB
	setID      string
	sourceRoot string
	cdMode     fsmeta.Mode // the run's resolved change-detection mode (3.5)
	destAbs    string
	workDir    string
	runID      int64
	bus        *progress.Bus
	xor        buildXorriso

	cleanFailed      bool
	verifyAfterBuild bool
	usableByImage    map[int64]int64
	placeInputs      []media.PlacementInput
	familyInputs     []media.HardLinkGroupInput

	jsonFlag bool
	strict   bool

	// Accumulators (single goroutine - the image loop is sequential:
	// one xorriso at a time, deterministic order).
	images       []buildImageReport
	warnings     int
	failureLines []string
	terminal     error
}

// buildImage masters ONE image through imagebuild.Build. Per-image
// build failures are RECORDED here (the N3 continue semantics) and
// returns nil; a non-nil error is terminal plumbing (catalog reads,
// inventory shape) that stops the run.
func (r *buildRunner) buildImage(ctx context.Context, imageID int64, export *runExport) error {
	rep := buildImageReport{Image: imageID, Status: "failed"}
	// The image report lands on EVERY path - including the interruption
	// early-returns - so what completed before a signal is honestly
	// accounted (10.1).
	defer func() {
		r.images = append(r.images, rep)
	}()

	inv, err := r.db.CopyInventory(ctx, r.setID, imageID)
	if err != nil {
		return err
	}
	stageInv, chunked, err := mapBuildInventory(inv)
	if err != nil {
		return err
	}

	// Per-image progress (FR-042): the FOUR NAMED PHASES of the 6.2
	// four-phase model (2026-10-20 "looks hung" amendment) are
	// service-owned: imagebuild.Build emits build(stage) and
	// build(master) (byte-watcher on the growing .partial against the
	// RequiredImageBytes estimate) and internal/verify emits
	// build(extract) (tree-sum watcher) and build(verify) (per-entry
	// re-hash with exact totals up front). The cli used to emit one
	// "build" phase here; the phases moved into the services so the
	// sequence is identical for every caller (the T1.1 "services own
	// their events" seam). Warn/Error/Done stay at this layer.
	res, buildErr := imagebuild.Build(ctx, imagebuild.Deps{
		HashStore: r.db.CopyStore(r.setID, imageID),
		Bus:       r.bus,
		Master:    r.xor,
		Extractor: r.xor,
	}, imagebuild.BuildInput{
		SetID:            r.setID,
		ImageID:          imageID,
		SourceRoot:       r.sourceRoot,
		Mode:             r.cdMode,
		WorkDir:          r.workDir,
		DestDir:          r.destAbs,
		UsableSize:       r.usableByImage[imageID],
		CleanFailed:      r.cleanFailed,
		VerifyAfterBuild: r.verifyAfterBuild,
		Inventory:        stageInv,
		Export:           imagebuild.EmbeddedExport{Path: export.path, Size: export.size, Hash: export.hash},
		Manifest: imagebuild.ManifestSeed{
			ToolVersion:    catalog.ToolVersion,
			SchemaVersion:  catalog.SchemaVersion,
			CreatedAt:      time.Now(),
			ChunkedFiles:   chunked,
			HardLinkGroups: r.familyInputs,
			Placement:      r.placeInputs,
		},
		Store: cliBuildStore{bs: r.db.BuildStore(r.setID, imageID, r.runID)},
	})
	if buildErr != nil {
		// N3 wiring: a build failure (stage source-changed, xorriso
		// BuildError, verification mismatch, finalize fault) is
		// ITEM-FAILURE accounting - recorded, loop continues. An
		// interruption is not a "failure line": the run reports
		// interrupted (exit 130) and the builder already failed the
		// image's state detached.
		line := fmt.Sprintf("build: image %d: %v", imageID, buildErr)
		rep.Failure = buildErr.Error()
		r.failureLines = append(r.failureLines, line)
		r.bus.Emit(progress.Error{Msg: line})
		r.insertEvent(ctx, "error", "", line)
		var usage *report.UsageError
		if errors.As(buildErr, &usage) {
			// Usage-class from the builder (capacity pre-check,
			// --work-dir gate): terminal, exit 2 - the input is
			// unsatisfiable, the remaining images would fail the same
			// way (the file doc).
			return buildErr
		}
		return nil
	}

	rep.Status = "built"
	rep.ISOPath = res.ISOPath
	rep.Bytes = res.BytesWritten
	rep.ContentEntries = res.ContentEntries
	rep.VerifiedEntries = res.VerifiedEntries
	rep.VerifySkipped = res.VerifySkipped
	rep.Warnings = len(res.Warnings)
	r.warnings += len(res.Warnings)
	for _, w := range res.Warnings {
		r.warn(ctx, fmt.Sprintf("image %d: %s", imageID, w))
	}
	return nil
}

// mapBuildInventory maps the catalog copy inventory onto the D-04 ISO
// inventory the builder stages: Files/Symlinks/Chunks/Dirs verbatim,
// Secondaries DROPPED (ISO mode stores the family primary only; the
// manifest and export record the full link group). It also derives the
// per-parent reassembly anchors (media.ChunkedFileInput) from the
// chunk rows - one record per chunked parent touching this image.
func mapBuildInventory(inv catalog.CopyInventory) (imagebuild.StageInventory, []media.ChunkedFileInput, error) {
	var out imagebuild.StageInventory
	for _, f := range inv.Files {
		out.Files = append(out.Files, imagebuild.StageFile{
			FileID: f.FileID, RelPath: f.RelPath, Size: f.Size,
			Mode: f.Mode, UID: f.UID, GID: f.GID, MtimeNS: f.MtimeNS,
			Dev: f.Dev, Inode: f.Inode, Hash: f.Hash,
		})
	}
	var chunked []media.ChunkedFileInput
	seenParent := map[string]bool{}
	for _, c := range inv.Chunks {
		out.Chunks = append(out.Chunks, imagebuild.StageChunk{
			FileID: c.FileID, ChunkIndex: c.ChunkIndex, Offset: c.Offset, Length: c.Length,
			Hash:          c.Hash,
			ParentRelPath: c.ParentRelPath, ParentSize: c.ParentSize,
			ParentMode: c.ParentMode, ParentUID: c.ParentUID, ParentGID: c.ParentGID,
			ParentMtimeNS: c.ParentMtimeNS, ParentDev: c.ParentDev, ParentInode: c.ParentInode,
		})
		if !seenParent[c.ParentRelPath] {
			if len(c.ParentHash) != 32 {
				return imagebuild.StageInventory{}, nil, fmt.Errorf(
					"chunked parent %q of image inventory carries no whole-file hash (%d bytes); unreadable-class rows never reach media",
					c.ParentRelPath, len(c.ParentHash))
			}
			chunked = append(chunked, media.ChunkedFileInput{
				RelPath: c.ParentRelPath, WholeSize: c.ParentSize, WholeSHA256: c.ParentHash,
			})
			seenParent[c.ParentRelPath] = true
		}
	}
	for _, sy := range inv.Symlinks {
		out.Symlinks = append(out.Symlinks, imagebuild.StageSymlink{
			FileID: sy.FileID, RelPath: sy.RelPath, LinkTarget: sy.LinkTarget,
			Size: sy.Size, Mode: sy.Mode, UID: sy.UID, GID: sy.GID, MtimeNS: sy.MtimeNS,
		})
	}
	for _, d := range inv.Dirs {
		out.Dirs = append(out.Dirs, imagebuild.StageDir{
			DirID: d.DirID, RelPath: d.RelPath,
			Mode: d.Mode, UID: d.UID, GID: d.GID, MtimeNS: d.MtimeNS,
		})
	}
	return out, chunked, nil
}

// buildTotals was the cli's per-image Phase totals helper; it moved
// into imagebuild (stageTotals) when the four named build phases became
// service-owned (6.2, 2026-10-20). Deliberately deleted here, not
// kept as a duplicate: one derivation, one owner.

// copyPlacementInputs maps the catalog placement onto the media
// generator's input (the row-shape mapping layer, once per run).
func copyPlacementInputs(placement []catalog.CopyImagePaths) []media.PlacementInput {
	out := make([]media.PlacementInput, 0, len(placement))
	for _, p := range placement {
		out = append(out, media.PlacementInput{ImageNumber: p.ImageID, Paths: p.Paths})
	}
	return out
}

// copyFamilyInputs maps the catalog D-04 link families onto the media
// generator's input, once per run.
func copyFamilyInputs(families []catalog.CopyLinkFamily) []media.HardLinkGroupInput {
	out := make([]media.HardLinkGroupInput, 0, len(families))
	for _, fam := range families {
		out = append(out, media.HardLinkGroupInput{SHA256: fam.Hash, Names: fam.Names})
	}
	return out
}

// warn records a non-fatal per-image note (5.4 metadata warnings):
// Warn event + run_events, NO exit-code count.
func (r *buildRunner) warn(ctx context.Context, msg string) {
	r.bus.Emit(progress.Warn{Msg: msg})
	r.insertEvent(ctx, "warn", "", msg)
}

// insertEvent appends one run_events row (best-effort: during the
// interruption tail the run context is cancelled and the driver
// rejects statements - the report lines on stdout remain the
// authoritative record of that window).
func (r *buildRunner) insertEvent(ctx context.Context, level, rel, detail string) {
	if ctx.Err() != nil {
		return
	}
	_ = r.db.InsertRunEvent(ctx, catalog.RunEvent{
		RunID: r.runID, At: time.Now().UnixNano(), Level: level, RelPath: rel, Detail: detail,
	})
}

// finalize stamps the run row on EVERY exit path (2.4) and returns the
// command result in THIS FUNCTION'S return order: interrupted (130) >
// terminal error > item failures (10, or 1 with --strict) > nil (0).
// The order states which result the command hands to the exit-code
// classifier - it is NOT report.Classify's cross-class precedence.
// Classify maps whatever error it is given by its OWN class order
// (usage, dependency, catalog busy, catalog error, then item failures,
// then the interrupted sentinel), so an *report.ItemFailures that
// reaches it exits 10 even when the run context was also cancelled;
// build keeps 130 only because the interrupted result is returned
// FIRST here, ahead of any item-failure tail. The report lands on
// stdout first (FR-046: the result is the stdout artifact even on
// failure paths). summary is written back through the pointer so the
// deferred Done emission carries the final numbers (the scan.go
// convention).
func (r *buildRunner) finalize(ctx context.Context, summary *string, runErr error) error {
	interrupted := ctx.Err() != nil
	built, failed, bytes := r.totals()
	verified := 0
	for _, img := range r.images {
		verified += img.VerifiedEntries
	}
	status := "ok"
	switch {
	case interrupted:
		status = "interrupted"
	case r.terminal != nil || runErr != nil:
		status = "failed"
	case failed > 0:
		status = "partial"
	}
	stats, merr := json.Marshal(struct {
		Built           int                  `json:"built"`
		Failed          int                  `json:"failed"`
		Bytes           int64                `json:"bytes"`
		ChangeDetection changeDetectionAudit `json:"change_detection"`
	}{built, failed, bytes, auditOf(r.cdMode)})
	if merr != nil {
		stats = []byte("{}")
	}
	if err := r.db.FinishRunDetached(r.runID, status, time.Now().UnixNano(), string(stats)); err != nil {
		r.terminal = errors.Join(r.terminal, err)
	}

	rep := buildReport{
		SetID: r.setID, RunID: r.runID, Dest: r.destAbs, Status: status,
		Built: built, Failed: failed, Bytes: bytes,
		VerifiedEntries: verified, Warnings: r.warnings,
		Images: r.images, Failures: r.failureLines,
	}
	if err := writeBuildReport(r.cmd.OutOrStdout(), rep, r.jsonFlag); err != nil {
		r.terminal = errors.Join(r.terminal, err)
	}

	*summary = fmt.Sprintf("build: %d image(s) built (%s), %d failed, warnings %d",
		built, units.FormatBytes(bytes), failed, r.warnings)
	if interrupted {
		// T6.2 addendum F1 (scan.go's reference shape, scan.go:457-459
		// and 500-505): JOIN r.terminal instead of dropping it. A
		// FinishRunDetached (or report-write) failure collected on the
		// interrupt path used to be discarded here - the lost stamp was
		// silent - while scan surfaced both causes. The join keeps the
		// interrupted class (exit 130: report.ErrInterrupted is the
		// typed head) AND carries the original failure to stderr.
		return errors.Join(fmt.Errorf("%w: build run %d interrupted; the in-flight image is stamped failed and its .partial is kept for inspection",
			report.ErrInterrupted, r.runID), r.terminal)
	}
	if runErr != nil {
		return runErr
	}
	if r.terminal != nil {
		return r.terminal
	}
	if failed > 0 {
		if r.strict {
			return fmt.Errorf("build completed with %d failed image(s); --strict promotes this to a fatal error", failed)
		}
		var errs []error
		for _, line := range r.failureLines {
			errs = append(errs, errors.New(line))
		}
		return &report.ItemFailures{Failures: errs}
	}
	return nil
}

// totals folds the per-image reports into run counters.
func (r *buildRunner) totals() (built, failed int, bytes int64) {
	for _, img := range r.images {
		switch img.Status {
		case "built":
			built++
			bytes += img.Bytes
		default:
			failed++
		}
	}
	return built, failed, bytes
}

// --- the report model (human and --json; the key set is FROZEN) --------------

// buildImageReport is one image's build outcome. The JSON key set is a
// deliberate frozen schema (the scan/plan/copy report precedent): new
// keys are a conscious schema change with the pinning tests updated in
// the same commit.
type buildImageReport struct {
	Image   int64  `json:"image"`
	Status  string `json:"status"` // "built" | "failed"
	ISOPath string `json:"iso_path,omitempty"`
	Bytes   int64  `json:"bytes"`
	// ContentEntries is the size of the 6.6 content list the build
	// transaction wrote; VerifiedEntries is how many of them the
	// extraction self-verify matched (0 when the pass was skipped or
	// the image failed).
	ContentEntries  int    `json:"content_entries"`
	VerifiedEntries int    `json:"verified_entries"`
	VerifySkipped   bool   `json:"verify_skipped"`
	Warnings        int    `json:"warnings"`
	Failure         string `json:"failure,omitempty"`
}

// buildReport is the end-of-run report of the build command (human
// lines and --json are two presentations of the same numbers, FR-046:
// stdout only).
type buildReport struct {
	SetID  string `json:"set_id"`
	RunID  int64  `json:"run_id"`
	Dest   string `json:"dest"`
	Status string `json:"status"` // ok | partial | failed | interrupted (runs.status CHECK)
	Built  int    `json:"built"`
	Failed int    `json:"failed"`
	Bytes  int64  `json:"bytes"`
	// VerifiedEntries is the summed self-verified content entries
	// across the run's built images.
	VerifiedEntries int                `json:"verified_entries"`
	Warnings        int                `json:"warnings"`
	Images          []buildImageReport `json:"images"`
	Failures        []string           `json:"failures,omitempty"`
}

// writeBuildReport renders the report: --json emits exactly one JSON
// document; the human form is the set/run identity line, one line per
// image, and a totals line (the plan/scan/copy family style).
func writeBuildReport(w io.Writer, rep buildReport, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	if _, err := fmt.Fprintf(w, "build: set %s (run %d)\n", rep.SetID, rep.RunID); err != nil {
		return err
	}
	for _, img := range rep.Images {
		if img.Status == "built" {
			if _, err := fmt.Fprintf(w,
				"image %d: built %s (%s, content entries %d, verified %d, verify skipped: %v, warnings %d)\n",
				img.Image, img.ISOPath, units.FormatBytes(img.Bytes),
				img.ContentEntries, img.VerifiedEntries, img.VerifySkipped, img.Warnings); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(w, "image %d: failed\n", img.Image); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w,
		"totals: built %d, failed %d, %s (status: %s, verified entries %d)\n",
		rep.Built, rep.Failed, units.FormatBytes(rep.Bytes), rep.Status, rep.VerifiedEntries); err != nil {
		return err
	}
	if rep.Warnings > 0 {
		if _, err := fmt.Fprintf(w, "warnings: %d\n", rep.Warnings); err != nil {
			return err
		}
	}
	for _, line := range rep.Failures {
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}
