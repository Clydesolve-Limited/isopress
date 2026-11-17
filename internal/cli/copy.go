package cli

// T3.3 (with T3.5's wiring scope folded in per the delegated brief):
// the real `copy` command - the Sprint 3 capstone that wires the
// copy-set writer (internal/copyengine), the copy-side catalog surface
// (internal/catalog copystore.go), the T3.1 media layout + manifest,
// and the T3.2 catalog export into the working write workflow
// (DESIGN_SPEC 1.4 write data flow, 5.1/5.2 ordering via the engine,
// 5.3 layout, 6.6 manifest + export + content list, 9.1/9.3 progress,
// 10.2 exit codes, 10.3 stream separation). It replaces the T0.5
// placeholder command in commands.go; the other stubs are untouched.
//
// Layering (DESIGN_SPEC 1.2): flag parsing, service invocation, and
// report presentation only - no SQL (the catalog package is the only
// SQL issuer), no rendering (progress flows through the bus; the
// result lands on stdout, progress on stderr, FR-046). The row-shape
// mapping between catalog inventory types and copyengine items lives
// HERE, at the one layer allowed to know both sides (the scanStoreTx
// precedent).
//
// Wiring contracts honored here:
//
//   - MUTATING catalog open (exclusive flock, D-11): contention
//     surfaces as *catalog.CatalogBusy and report.Classify maps it to
//     exit 5. A missing catalog is NOT auto-created (scan's job).
//   - Flags: --set-id and --catalog required; --dest (the 5.3
//     <dest-root>, this implementation's documented flag name - the
//     design fixes the concept, not the spelling, exactly like T2.3's
//     --size); --image N repeatable to write a subset of the planned
//     images (default: every planned image, ascending). --work-dir
//     (global) seeds the export scratch directory.
//   - Change detection (DESIGN_SPEC 3.5, the 2026-10-20 CIFS
//     amendment): the run resolves ONE fsmeta.Mode from the catalog's
//     source_root (statfs; --change-detection auto|full|metadata),
//     emits the once-per-run degradation warning when auto detection
//     degraded it (progress.Warn + run_events), threads it into the
//     engine's step-1/step-7 comparisons (Deps.Mode), and records it
//     into runs.stats_json (audit only; the frozen --json key-set is
//     untouched). The chunked whole-file HARDENING rides the same
//     resolution: while every chunk of a parent is first-written in
//     this run, each chunk's verified media bytes accumulate into a
//     running whole-file sha256 - fed STRICTLY IN CHUNK-INDEX ORDER
//     (the in-order feed gate; see wholeFileCheck - chunks may sit on
//     different images in interleaved order, and a permuted stream must
//     never be hashed) - and the parent's FINAL chunk defers its mark
//     (copyengine.DeferMark) until the accumulated sum is
//     compared against the scan-time whole-file hash - a mismatch
//     classifies the FILE source-changed and leaves the final chunk
//     verified-but-unmarked (no mark reversal; files.copied stays 0;
//     FR-041 remains the end-to-end anchor; a resume skips the check,
//     documented on wholeFileCheck).
//   - Per image (5.3): fetch the COMPLETE inventory (files, chunks,
//     symlinks, empty dirs, hard-link secondaries), then write every
//     pending item through the engine's strict verify-then-mark
//     ordering; items with copied=1 are trusted and skipped (12.7
//     resume: the flag IS the trust - internal/copyengine documents
//     the model). THE T3.1 HONESTY BOUNDARY (fix round, the UNIFIED
//     secondary-claim rule): the inventory ITSELF excludes hard-link
//     family secondaries whose family PRIMARY is ASSIGNED - same image
//     or different (catalog.CopyInventory /
//     excludeSecondaryWholeFileClaims - the family derivation, not the
//     assignment column, decides; a member is always claimed as
//     link(2) on the primary's image, never as an ordinary whole-file
//     item anywhere), so everything this pipeline derives from it - the
//     writer items, the MANIFEST.json entries, the placement claims,
//     the totals - can never claim a member twice or claim a member
//     whose bytes live only as a link(2) on another image. This layer
//     needs no extra filter: it never sees the excluded member.
//   - The UTF-8 writer convention (T3.1): relpaths, link targets, and
//     the set id are gated through the media validators BEFORE any
//     filesystem effect; an unsafe row warn-and-skips (per-file
//     warning + run_events + the partial-failure class, exit 10) - the
//     media TREE could hold raw bytes but the JSON MANIFEST never
//     could, so the row never reaches either.
//   - THE B1 PARTIAL-COLLISION GATE (fix round, blocking; see
//     partialPathCollision): before the writer is invoked for a
//     whole-file item, the pipeline checks - against this image's
//     already-loaded inventory - whether the item's derived
//     "<final>.partial" path equals another member's FINAL media path
//     (the real-repro'd pair: `a` whose partial would be the completed
//     files/a.partial of sibling `a.partial`, which copyengine step
//     2a's os.Remove would DELETE - marked+verified bytes gone, and
//     resume never repairs because the copied flag is the trust). On
//     collision the item warn-and-skips BEFORE any write (per-file
//     warning naming both paths and the sibling relpath + run_events
//     + the partial-failure class, exit 10; the item is reported
//     skipped). The gate lives in this pipeline layer, not in
//     copyengine.CopyItem: the engine is inventory-blind (single-item
//     contract, DESIGN_SPEC 1.2 - it does not hold the image
//     inventory and must not). Chunks and the manifest/export embeds
//     are structurally exempt (chunk names are fixed 5-digit under the
//     disjoint chunks/ root; the image-root members are tool-owned) -
//     the exemption reasoning is pinned in the gate's tests.
//   - Export ONCE, embed EVERYWHERE (6.6/FR-035): ONE set-scoped
//     VACUUM-INTO snapshot (catalog.ExportSnapshot, T3.2 fresh-path
//     contract) is produced per copy run into a temp directory BEFORE
//     any image is written - planning has long completed by then, so
//     the snapshot carries the full placement of every image - and the
//     SAME bytes are embedded into every image as
//     catalog-export.sqlite (copyengine.EmbedFile: the 5.1 write
//     discipline minus the mark), then the temp is removed. One
//     export serves the whole run; byte-identical across images by
//     construction.
//   - MANIFEST.json per image, AFTER that image's payload writes
//     (6.6): media.GenerateManifest over the inventory (stored chunk
//     hashes - present by then via the 4.3 first-write capture), the
//     whole-set placement (catalog.CopyPlacement), and the D-04
//     hard-link families (catalog.CopyHardLinkFamilies). An image with
//     ANY incomplete payload item DEFERS its manifest to the run that
//     completes it (a manifest listing unwritten members would lie),
//     reported as an incomplete image.
//   - image_contents (FR-020, 6.6): the payload rows are written
//     inside the SAME transaction as each copied flag (catalog
//     copystore.go); the manifest/export self-rows are appended by
//     this layer after embedding (catalog.CopyStore.RecordEmbedded).
//   - Runs (2.4): a runs row (kind 'copy', root = destination) opens
//     before the first write; every exit path finalizes it - 'ok',
//     'partial' (item failures), 'failed' (terminal error), or
//     'interrupted' when the run context was cancelled. THE
//     INTERRUPTED-RUN DEFERRAL CLOSURE (T0.5/T1.6): the finalization
//     uses catalog.FinishRunDetached, which runs the tiny post-cancel
//     UPDATE on a fresh short-timeout context - the driver would
//     reject it under the cancelled run context, which is exactly why
//     interrupted copy runs used to stay 'running' forever. FIX
//     ROUND: "every exit path" is now literally true - the run-scoped
//     export production failure finalizes 'failed' before returning
//     (NB1; it used to return bare and strand the row 'running'), and
//     the 12.7 env seam is constructed+validated BEFORE StartCopyRun
//     as a usage-class input (NB2; a malformed
//     ISOPRESS_TEST_COPY_FAIL_* value can no longer mint a run row
//     it abandons).
//   - Progress (T1.1, FR-042): one Phase per image with that image's
//     determinate totals (pending payload items), one ItemDone per
//     completed item from the engine, Warn/Error per skip/failure, and
//     exactly one Done summary line on EVERY exit path.
//   - Exit codes (10.2): nil -> 0; any skip or failure -> 10 via
//     report.ItemFailures (skips are the partial-failure class per the
//     T3.3 brief: source-changed runs and UTF-8 refusals are reported
//     like every other per-item problem); --strict promotes that to a
//     fatal exit 1; a cancelled context -> 130 (report.ErrInterrupted).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"isopress/internal/catalog"
	"isopress/internal/copyengine"
	"isopress/internal/fsmeta"
	"isopress/internal/media"
	"isopress/internal/progress"
	"isopress/internal/report"
	"isopress/internal/units"
)

// copyOptions holds the copy-local flag values.
type copyOptions struct {
	dest   string  // --dest: destination root (5.3 <dest-root>)
	images []int64 // --image: repeatable image selection (default: all planned)
}

// newCopyCommand builds the real copy command (replaces the T0.5 stub).
func newCopyCommand(opts *rootOptions) *cobra.Command {
	co := copyOptions{}
	cmd := &cobra.Command{
		Use:   "copy",
		Short: "copy assigned items to destination trees with verify-then-mark ordering",
		Long: "Write every planned image of the --set-id set as a copy tree under --dest: " +
			"<dest>/<SET-ID>-NNN/ carrying files/, chunks/, MANIFEST.json, and " +
			"catalog-export.sqlite (DESIGN_SPEC 5.3). Each file and chunk is written to a " +
			".partial, fsynced, atomically renamed, read-back hash-verified, and only then " +
			"marked copied with its image_contents row in one transaction (5.1, FR-014 - " +
			"nothing is ever marked before its bytes verify). Write faults retry 3 times " +
			"with backoff; a source that changed since the scan is skipped with a warning; " +
			"both contribute to exit 10. Crashed runs resume: copied items are trusted and " +
			"skipped, stale .partial remnants are cleaned (12.7, FR-013). An interrupted " +
			"run (SIGINT/SIGTERM) finalizes its run row as 'interrupted'. " +
			"--image N writes only that image.\n\n" +
			"Exit codes: 0 success; 1 fatal unexpected error; 2 usage; 4 catalog error; 5 catalog " +
			"busy (another mutating run holds the lock); 10 completed with the per-item problems " +
			"listed above (--strict promotes this to 1); 130 interrupted. copy never exits 3: it " +
			"never needs xorriso (NFR-004). JSON: --json prints the final report as exactly one JSON " +
			"document on stdout; a run that aborts before doing work prints no document at all; " +
			"--json never changes the exit code.\n\n" +
			"Before writing an image whose catalog content list is empty (a first write, or an image " +
			"released by `clear-assignments --image`), copy REMOVES the existing `<dest>/<SET-ID>-NNN` " +
			"directory and records the removal (report line, `wipes` in `--json`, and a catalog " +
			"run-log row naming the path and the number of entries removed). A directory whose " +
			"`MANIFEST.json` names a different set or image, or is unreadable as a manifest, is " +
			"refused with exit 10 instead of removed: remove or rename it yourself, or choose a " +
			"different `--dest`.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCopy(cmd.Context(), cmd, opts, co)
		},
	}
	cmd.Flags().StringVar(&co.dest, "dest", "",
		"destination root for the copy trees (required; e.g. /mnt/backup)")
	cmd.Flags().Int64SliceVar(&co.images, "image", nil,
		"write only this planned image number (repeatable; default: every planned image). "+
			"Hard-link family members are always claimed by their primary's image: with only a "+
			"cross-image member's own image selected, its bytes land nowhere until the primary's "+
			"image is written (the whole-set placement summary still names the primary's image)")
	return cmd
}

// runCopy is the copy service invocation (see the file doc for the
// wiring contracts). The named return feeds the deferred progress Done
// event's Failed signal (progress_done.go).
func runCopy(ctx context.Context, cmd *cobra.Command, opts *rootOptions, co copyOptions) (runErr error) {
	// --- usage-class inputs (exit 2, 10.1) ---
	if opts.catalog == "" {
		return &report.UsageError{Command: cmd.CommandPath(), Msg: "--catalog is required (path to the SQLite catalog file)"}
	}
	if opts.setID == "" {
		return &report.UsageError{Command: cmd.CommandPath(), Msg: "--set-id is required (the planned set to write)"}
	}
	if co.dest == "" {
		return &report.UsageError{Command: cmd.CommandPath(), Msg: "--dest is required (destination root for the copy trees)"}
	}
	if err := checkTilde(co.dest, "--dest"); err != nil {
		return &report.UsageError{Command: cmd.CommandPath(), Err: err}
	}
	destAbs, err := filepath.Abs(co.dest)
	if err != nil {
		return &report.UsageError{Command: cmd.CommandPath(), Err: fmt.Errorf("--dest %q: %w", co.dest, err)}
	}
	destAbs = filepath.Clean(destAbs)
	// The set id becomes the on-media directory name (6.4); gate it
	// once here so a bad id fails before any database or filesystem
	// work (the media validator is the single naming authority).
	if _, err := media.ImageDirName(opts.setID, 1); err != nil {
		return &report.UsageError{Command: cmd.CommandPath(), Err: err}
	}

	// NB2 (fix round): the 12.7 env seam is CONSTRUCTED and VALIDATED
	// BEFORE any database work - a malformed
	// ISOPRESS_TEST_COPY_FAIL_* value is a usage-class input (exit
	// 2, like every other malformed tool input) and must never mint a
	// runs row it then abandons: the previous placement returned the
	// validation error AFTER StartCopyRun, stranding a 'running' row
	// forever (the same debris class NB1 fixes on the export path).
	hook, err := copyTestHookFromEnv()
	if err != nil {
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
	// every item's SourcePath resolves against - runtime derivation,
	// not the stored scan-time class). Threaded into BOTH the
	// copyengine step-1/step-7 comparisons (Deps.Mode) and the
	// chunked whole-file hardening below.
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
	selected, err := selectCopyImages(cmd, ids, co.images)
	if err != nil {
		return err
	}

	// Runs bookkeeping (2.4): kind 'copy', root = the destination root.
	runID, err := db.StartCopyRun(ctx, catalog.CopyRun{
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
	summary := fmt.Sprintf("copy: run %d aborted before any item was written", runID)
	defer func() {
		// Failed / Interrupted mark the abort path so the interactive
		// renderer CLEARS its bar instead of rendering a misleading
		// 100% (progress_done.go). copy's finalize makes the EXPLICIT
		// interrupted-status decision (ctx.Err() != nil -> run row
		// 'interrupted', result wrapped in report.ErrInterrupted);
		// runInterrupted classifies exactly that shape, and
		// runFailed - its complement - covers the rest, so the two
		// flags are mutually exclusive by construction.
		bus.Finish(progress.Done{Summary: summary, Failed: runFailed(runErr), Interrupted: runInterrupted(runErr)})
		wait()
	}()

	// The 3.5 once-per-run degradation warning (progress.Warn +
	// run_events); auto-detected degradations only (see
	// changedetection.go).
	warnChangeDetection(ctx, db, runID, bus, sourceRoot, cdMode)

	// The hook was constructed and validated above (NB2: BEFORE
	// StartCopyRun, so a malformed seam can never leave run debris).
	r := &copyRunner{
		cmd:        cmd,
		db:         db,
		setID:      opts.setID,
		sourceRoot: sourceRoot,
		cdMode:     cdMode,
		destAbs:    destAbs,
		runID:      runID,
		bus:        bus,
		deps:       copyengine.Deps{Bus: bus, Hook: hook, Mode: cdMode},
		jsonFlag:   opts.json,
		strict:     opts.strict,
		wholeFiles: map[int64]*wholeFileCheck{},
	}

	// THE EXPORT, ONCE PER RUN (6.6/FR-035, the file doc): one
	// set-scoped VACUUM-INTO snapshot into a fresh temp path (T3.2's
	// fresh-destPath contract), its bytes embedded into every image
	// below, the temp removed at the end.
	export, err := produceRunExport(ctx, db, opts.setID, opts.workDir, "isopress-copy-export-")
	if err != nil {
		// NB1 (fix round): EVERY exit path finalizes the run row (2.4,
		// the file doc) - this path used to return bare and strand the
		// runs row 'running' forever. finalize stamps 'failed' (or
		// 'interrupted' when the run ctx died - the same detached
		// FinishRunDetached path) and prints the honest (empty) report.
		return r.finalize(ctx, &summary, err)
	}
	defer os.RemoveAll(export.dir) //nolint:errcheck // scratch; the embedded copies are published

	// --- the per-image write loop -------------------------------------
	for _, imageID := range selected {
		if ctx.Err() != nil {
			break // interrupted: stop scheduling new images (5.1/10.1)
		}
		if err := r.copyImage(ctx, imageID, export); err != nil {
			if ctx.Err() != nil || errors.Is(err, copyengine.ErrHookAbort) {
				// Interruption or the deterministic test-abort seam:
				// stop the loop; the finalization below records state.
				break
			}
			// A terminal (non-item) error - inventory read, export
			// embed plumbing, manifest assembly structural failure -
			// aborts the remaining images but still finalizes the run
			// and prints the report of what DID copy.
			r.terminal = errors.Join(r.terminal, err)
			break
		}
	}

	return r.finalize(ctx, &summary, nil)
}

// selectCopyImages resolves --image selections against the planned
// ids: no --image means every planned image (ascending); a requested
// id that was never planned is a usage input (exit 2), not a silent
// no-op.
func selectCopyImages(cmd *cobra.Command, planned, requested []int64) ([]int64, error) {
	if len(requested) == 0 {
		return planned, nil
	}
	plannedSet := make(map[int64]struct{}, len(planned))
	for _, id := range planned {
		plannedSet[id] = struct{}{}
	}
	seen := map[int64]struct{}{}
	var out []int64
	for _, id := range requested {
		if _, ok := plannedSet[id]; !ok {
			return nil, &report.UsageError{
				Command: cmd.CommandPath(),
				Msg: fmt.Sprintf("--image %d is not a planned image of this set (planned: %s)",
					id, joinInt64s(planned)),
			}
		}
		if _, dup := seen[id]; dup {
			continue // repeated --image is harmless, not an error
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}

func joinInt64s(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = fmt.Sprintf("%d", id)
	}
	return strings.Join(parts, ", ")
}

// runExport is the one-produced-per-run catalog snapshot plus its
// hash/size, ready to embed into every image.
type runExport struct {
	path string // fresh temp path of the produced export
	size int64
	hash []byte // sha256 of the published bytes
	dir  string // temp dir holding it; removed at run end
}

// produceRunExport builds the run's single FR-034 export (the T3.2
// fresh-path contract: ExportSnapshot refuses to overwrite, so the
// temp directory is created empty around a never-existing file).
// scratchPrefix names the temp directory pattern per command family
// ("isopress-copy-export-" / "isopress-build-export-").
func produceRunExport(ctx context.Context, db *catalog.DB, setID, workDir, scratchPrefix string) (*runExport, error) {
	dir, err := os.MkdirTemp(workDir, scratchPrefix)
	if err != nil {
		return nil, fmt.Errorf("export: creating export scratch directory: %w", err)
	}
	path := filepath.Join(dir, media.ExportFileName)
	size, err := db.ExportSnapshot(ctx, setID, path)
	if err != nil {
		_ = os.RemoveAll(dir)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("producing the catalog export: %w", err)
	}
	hash, err := hashFileForEmbed(path)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("hashing the catalog export: %w", err)
	}
	return &runExport{path: path, size: size, hash: hash, dir: dir}, nil
}

// hashFileForEmbed is the plain source-side sha256 of an embed file
// (EmbedFile re-hashes the PUBLISHED copy during its read-back; this
// pre-computed digest feeds the image_contents 'export' row).
func hashFileForEmbed(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// copyRunner carries the run-scoped state through the image loop.
type copyRunner struct {
	cmd        *cobra.Command
	db         *catalog.DB
	setID      string
	sourceRoot string
	cdMode     fsmeta.Mode // the run's resolved change-detection mode (3.5)
	destAbs    string
	runID      int64
	bus        *progress.Bus
	deps       copyengine.Deps
	jsonFlag   bool // --json: the report lands on stdout as JSON
	strict     bool // --strict: item failures promote to a fatal exit 1

	// Accumulators (single goroutine - the image loop is sequential by
	// design: one writer, deterministic order, honest resume).
	images           []copyImageReport
	metadataWarnings int
	failureLines     []string
	// wipes buffers the 4.6.1 wipe records (channel 2) - the
	// failureLines pattern: a RUN-level buffer, deliberately NOT a
	// copyImageReport field (per-image reports stay untouched). It is
	// copied into copyReport.Wipes at report assembly (finalize) as a
	// NON-NIL slice, so the frozen `wipes` key is always present ([] when
	// no wipe happened - the ratified 12 -> 13 addition).
	wipes    []wipeRecord
	terminal error

	// wholeFiles carries the 2026-10-20 chunked whole-file hardening
	// state ACROSS the per-image loop (a chunked parent's chunks may
	// live on DIFFERENT images - the planner assigns via chunks.image_id
	// - so the running hash and the first-write accounting are
	// RUN-scoped, keyed by file_id; see newWholeFileCheck).
	wholeFiles map[int64]*wholeFileCheck
}

// wholeFileCheck is the running state of ONE chunked parent under the
// whole-file hardening (DESIGN_SPEC 3.5's chunked residual, the
// 2026-10-20 architect ruling). The check runs ONLY when EVERY chunk
// of the parent is first-written in THIS run: a resume skips
// already-marked chunks (their bytes are never accumulated) and a
// re-copied chunk (stored hash) verifies against its own stored
// anchor, so neither can feed a whole-file comparison - for any parent
// that fails the all-first-write condition the FR-041 restore-time
// whole-file hash remains the end-to-end detector (documented).
//
// ACCUMULATION (judgment call, documented): the running sha256 is fed
// from each chunk's VERIFIED MEDIA BYTES after its successful copy
// (feedFrom re-reads the just-written chunk file), NOT from a source
// tee inside the write. Rationale: sha256 is not composable and a
// caller-owned hash cannot be fed retry-safely across ITEMS from
// inside the engine (an attempt-level reset would wipe earlier
// chunks' state; a failed attempt's feed cannot be un-fed), while the
// media read is exactly-once per SUCCESSFUL copy and is strictly
// stronger evidence - the read-back verify (FR-041's per-chunk gate)
// has already proven those bytes equal the source stream, so the
// accumulated sum IS the FR-041 reassembly anchor computed at copy
// time, over the bytes restore will actually consume. The read cost
// is one extra pass over freshly-written (page-cached) chunk files.
//
// THE IN-ORDER FEED GATE (the B1 ordering fix): the accumulated sum
// equals the whole-file hash ONLY when the feeds arrive in the file's
// BYTE order, and the run's processing order (images ascending, then
// (file_id, chunk_index) per image - the inventory query's ORDER BY)
// equals byte order only when the balancer assigned the parent's
// chunks in CONTIGUOUS ASCENDING BLOCKS. It need not: every chunk is
// an independent WFD placement item, so equal-size chunks can be
// INTERLEAVED across open images (e.g. chunk1 -> image 1 while chunk0
// and the final chunk2 -> image 2; processed order 1, 0, 2). Before
// the gate, a final chunk processed LAST over permuted earlier feeds
// hashed a PERMUTED stream - a guaranteed mismatch that classified an
// UNCHANGED file source-changed (exit 10, final chunk
// verified-but-unmarked, image incomplete): the spurious-failure class
// the hardening exists to eliminate (not an integrity breach - a
// permuted stream can never match, so no wrong data is ever marked -
// but an over-strict false positive all the same). The gate ENFORCES
// the invariant the compare used to silently assume: feedFrom folds
// chunk I only when I == nextExpected (then nextExpected++), and an
// out-of-order feed marks the check broken INSTEAD of feeding. Once
// broken the check can never run (eligibleAtFinalChunk), so a
// permuted stream is NEVER hashed and a non-contiguously-assigned
// parent simply takes the fail-closed path: the final chunk completes
// through the normal/fallback mark path and FR-041's restore-time
// whole-file hash remains its detector (documented). Order within ONE
// image is guaranteed ascending by the inventory query; the gate
// covers the cross-image case uniformly.
type wholeFileCheck struct {
	relPath string // the parent's relpath, for the file-level report line
	hasher  hash.Hash
	// seenFirst counts chunks of this parent that were FIRST-WRITTEN
	// and copied in THIS run; eligibility at the final chunk requires
	// seenFirst == NumChunks (the all-first-write condition).
	seenFirst int
	// numChunks is the parent's TOTAL chunk count (files.chunk_count
	// via CopyChunk.ParentNumChunks) - the eligibility denominator
	// and the final-chunk locator (index == numChunks-1).
	numChunks int
	// expected is the parent's catalog whole-file hash (the compare
	// anchor; validated 32 bytes when the check is created).
	expected []byte
	// broken disqualifies the check for the rest of the run: either a
	// successfully-copied chunk whose bytes could not be accumulated
	// (re-reading the just-written media file failed) or an
	// OUT-OF-ORDER feed (the gate above) - in both cases the final sum
	// could never be the file's whole-file hash, so fail closed to the
	// pre-amendment behavior (no compare; FR-041 remains the
	// detector), never to a false mismatch.
	broken bool
	// nextExpected is the in-order feed gate's cursor: the 0-based
	// chunk INDEX the accumulation needs next, starting at 0 (see the
	// struct doc). A complete in-order accumulation ends at
	// nextExpected == numChunks, which eligibleAtFinalChunk requires
	// alongside the seenFirst count.
	nextExpected int
	// decided is set once the final chunk has been processed - a
	// (structurally impossible) later chunk must never re-decide.
	decided bool
}

// newWholeFileCheck creates the per-parent hardening state, or nil
// when the parent cannot qualify: a malformed inventory row (no
// total-chunk count, no whole-file hash) simply never runs the check
// - the copy proceeds normally and the restore anchor stays the
// detector (fail-closed toward the pre-amendment behavior).
func (r *copyRunner) newWholeFileCheck(c catalog.CopyChunk) *wholeFileCheck {
	if c.ParentNumChunks <= 0 || len(c.ParentHash) != 32 {
		return nil
	}
	return &wholeFileCheck{
		relPath:   c.ParentRelPath,
		hasher:    sha256.New(),
		numChunks: c.ParentNumChunks,
		expected:  c.ParentHash,
	}
}

// feedFrom folds one JUST-COPIED chunk's verified media bytes into the
// running whole-file hash, GATED ON BYTE ORDER (the in-order feed gate
// documented on wholeFileCheck): the bytes are fed only when
// chunkIndex is the exact next index the accumulation needs
// (nextExpected), and only then does the cursor advance. The chunk
// file was read-back verified in the same CopyItem call (its bytes
// equal the source stream), so a fed chunk consumes exactly what
// restore would reassemble. An OUT-OF-ORDER chunk (structurally: a
// cross-image interleaved assignment; an index below the cursor is
// impossible - each chunk row processes once per run) marks the check
// broken WITHOUT feeding: a permuted stream must never be hashed,
// because it can never match and would misclassify an unchanged file
// as source-changed. A read failure likewise marks the check broken
// (never a false mismatch - see the struct doc) and returns the error
// for the caller's warning.
func (wf *wholeFileCheck) feedFrom(destPath string, chunkIndex int) error {
	if chunkIndex != wf.nextExpected {
		wf.broken = true
		return nil
	}
	f, err := os.Open(destPath)
	if err != nil {
		wf.broken = true
		return err
	}
	defer f.Close() //nolint:errcheck // read-only close
	if _, err := io.Copy(wf.hasher, f); err != nil {
		wf.broken = true
		return err
	}
	wf.nextExpected++
	return nil
}

// noteFirstWrite records a first-written chunk copy of the parent.
func (wf *wholeFileCheck) noteFirstWrite() { wf.seenFirst++ }

// eligibleAtFinalChunk reports whether the whole-file compare may run
// at the parent's FINAL chunk: every chunk of the parent was
// first-written in this run (seenFirst reached the TOTAL count, so no
// chunk was skipped-already-copied, re-copied from a stored hash, or
// failed), the accumulation is COMPLETE AND IN ORDER (nextExpected
// reached the TOTAL - the feed gate's enforced byte-order invariant; a
// broken or short accumulation never compares), and no earlier
// decision happened.
func (wf *wholeFileCheck) eligibleAtFinalChunk() bool {
	return wf != nil && !wf.decided && !wf.broken &&
		wf.seenFirst == wf.numChunks && wf.nextExpected == wf.numChunks
}

// copyImage writes ONE image: the complete 5.3 tree under
// <dest>/<SET-ID>-NNN/ - payload items first (files, chunks, symlinks,
// empty dirs, hard-link secondaries), then the export embed, then the
// MANIFEST.json (deferred when the payload is incomplete). Returns a
// terminal error only for plumbing failures that must stop the run;
// per-item problems are recorded and the loop continues (10.1).
func (r *copyRunner) copyImage(ctx context.Context, imageID int64, export *runExport) error {
	imageDirName, err := media.ImageDirName(r.setID, imageID)
	if err != nil {
		return err // unreachable: gated at run start
	}
	imageRoot := filepath.Join(r.destAbs, imageDirName)

	// THE COPY-TIME WIPE GATE (DESIGN_SPEC 4.6.1), BEFORE CopyInventory
	// and any writer (see copy_wipe.go for the six ordered checks and
	// the four loud-accounting channels): an image whose image_contents
	// list is EMPTY gets its existing tree removed - ownership-checked,
	// loudly accounted - before this rewrite. Three exits:
	//
	//   - err: the gate's catalog read failed (CatalogError -> exit 4).
	//     Returning it as the image's terminal error aborts the run
	//     (the gate is NEVER bypassed - 4.6.1 step 1).
	//   - a refusal detail: recorded as a counted failure (exit-10
	//     class) and this image is NOT written - return before
	//     CopyInventory, so a foreign tree is never partially written
	//     into; the other images of the run continue.
	//   - empty: proceed (a wipe may have happened and been accounted).
	refuse, err := r.wipeBeforeRewrite(ctx, imageID, imageRoot)
	if err != nil {
		return err
	}
	if refuse != "" {
		// Nothing is written to a refused image: the report carries an
		// incomplete image (no payload, no manifest - deferred to the
		// run that clears the refusal) plus the counted failure line.
		rep := copyImageReport{Image: imageID}
		r.recordFail(ctx, &rep, imageID, imageRoot, refuse)
		r.images = append(r.images, rep)
		return nil
	}

	inv, err := r.db.CopyInventory(ctx, r.setID, imageID)
	if err != nil {
		return err
	}
	cs := r.db.CopyStore(r.setID, imageID)
	r.deps.Store = cs

	rep := copyImageReport{Image: imageID}
	// The image report lands in the run report on EVERY path -
	// including the interruption early-returns below - so what copied
	// before the signal is honestly accounted (10.1).
	defer func() {
		r.images = append(r.images, rep)
	}()

	// Chunk effective hashes: chunks WRITTEN during this run carry the
	// 4.3 first-write capture (Result.Anchor - the inventory snapshot
	// predates it); already-copied chunks carry their stored hash in
	// the snapshot. The manifest assembly reads through this map so it
	// can never list a NULL chunk hash.
	chunkHashes := map[chunkKey][]byte{}

	// The per-image determinate phase (FR-042): totals are the PENDING
	// payload items (already-copied items are trusted, skipped, and
	// emit no ItemDone - counting them would make the bar never reach
	// 100% on a resume). Dirs are always re-asserted (idempotent
	// mkdir), so they count as pending work with zero bytes.
	totFiles, totBytes := pendingTotals(inv)
	r.bus.Emit(progress.Phase{
		Name: "copy", TotalKnown: true, TotalFiles: totFiles, TotalBytes: totBytes,
	})

	// THE B1 GATE'S PATH SET (fix round): every final media path of
	// THIS image's already-loaded inventory, built once per image -
	// the whole-files loop then consults it O(1) per item (see
	// partialPathCollision for the defect, the placement rationale,
	// and the chunk/embed exemptions).
	finalMediaPaths := imageFinalMediaPaths(inv)

	complete := true
	stop := func() bool { return ctx.Err() != nil }

	// --- whole files (5.1 ordering via the engine) ---
	for _, f := range inv.Files {
		if stop() {
			return nil
		}
		if f.Copied {
			rep.AlreadyCopied++
			continue
		}
		mediaPath, ok := r.refuseUnsafeRelPath(ctx, &rep, imageID, f.RelPath)
		if !ok {
			complete = false
			continue
		}
		// THE B1 GATE: never write an item whose derived partial path
		// sits on another member's FINAL media path - copyengine step
		// 2a removes that path as a stale partial before writing and
		// would DELETE the sibling's marked+verified bytes (the flag
		// is the trust, so resume would never repair them). The gate
		// skips the colliding item BEFORE any filesystem effect, so
		// the sibling - copied or not - is never at risk.
		if sibling, collided := partialPathCollision(finalMediaPaths, mediaPath); collided {
			r.recordSkip(ctx, &rep, imageID, f.RelPath, fmt.Sprintf(
				"%s: its temporary path %s collides with the media file of source '%s' on this image",
				mediaPath, copyengine.PartialPath(mediaPath), sibling))
			complete = false
			continue
		}
		destPath := filepath.Join(imageRoot, filepath.FromSlash(mediaPath))
		res := copyengine.CopyItem(ctx, r.deps, copyengine.Item{
			Kind: copyengine.ItemWholeFile, FileID: f.FileID, RelPath: f.RelPath,
			MediaPath: mediaPath, SourcePath: filepath.Join(r.sourceRoot, filepath.FromSlash(f.RelPath)),
			DestPath:   destPath,
			SourceSize: f.Size, MtimeNS: f.MtimeNS, Dev: f.Dev, Inode: f.Inode,
			Size: f.Size, Hash: f.Hash, Mode: f.Mode, UID: f.UID, GID: f.GID,
		})
		if done := r.classify(ctx, &rep, imageID, f.RelPath, res); !done {
			complete = false
			if res.Outcome == copyengine.OutcomeAborted {
				return nil
			}
		}
	}

	// --- chunks (identical per-chunk ordering, 5.2; parent mirror in
	// the mark transaction, chunk-granularity resume) ---
	//
	// THE CHUNKED WHOLE-FILE HARDENING (DESIGN_SPEC 3.5's chunked
	// residual, the 2026-10-20 architect ruling): while every chunk of
	// a parent is FIRST-WRITTEN in this run, each chunk's VERIFIED
	// media bytes are folded into a running whole-file sha256
	// (wholeFileCheck.feedFrom - see its doc for the retry-safety and
	// FR-041-anchor rationale); at the parent's FINAL chunk the
	// accumulated hash is compared against the parent's catalog
	// whole-file hash BEFORE that chunk's mark:
	//
	//   - MATCH -> MarkChunk (the normal mark transaction: flag + 4.3
	//     hash + image_contents + parent mirror) and the file copies
	//     clean;
	//   - MISMATCH -> the file is classified SOURCE-CHANGED at the
	//     FILE level (one skip line naming the parent, run_events,
	//     exit 10) and the final chunk is left VERIFIED-BUT-UNMARKED
	//     on media: its bytes were written and read-back verified
	//     (they are true for what they are - new content), so nothing
	//     is unmarked or rolled back; the unmarked state means the
	//     next run re-verifies and re-marks it (as a first write -
	//     its chunks.hash is still NULL) or the rescan resolves the
	//     lineage. files.copied stays 0 (the 5.2 mirror flips only
	//     when ALL chunks are marked), so the parent is honestly NOT
	//     "copied". The earlier chunks' marks STAND - their bytes and
	//     stored hashes are true; the media honestly holds
	//     new-content chunks under the old rows, the restore-time
	//     whole-file anchor (FR-041) remains the end-to-end detector
	//     for them, and the next scan's modification versioning (3.7)
	//     records the new lineage.
	//
	// The check runs ONLY when EVERY chunk of the parent was
	// first-written in THIS run (wholeFileCheck.eligibleAtFinalChunk:
	// seenFirst == the parent's total chunk count at the final
	// chunk). A RESUME skips already-marked chunks - the running
	// hasher cannot see their bytes - and a re-copied chunk (stored
	// hash) verifies against its own anchor, so for any parent that
	// fails the all-first-write condition NO check runs and the FR-041
	// restore anchor remains the detector (documented in 3.5 and on
	// copyengine's hardening-seam doc). The state is RUN-scoped
	// (r.wholeFiles) because a chunked parent's chunks may live on
	// DIFFERENT images - the planner assigns via chunks.image_id - and
	// the check must span them.
	//
	// THE IN-ORDER FEED GATE (the B1 ordering fix; see wholeFileCheck
	// for the full rationale): because chunks are independent WFD
	// placement items, a parent's chunks may sit INTERLEAVED across
	// images (e.g. chunk1 -> image 1, chunk0 + final chunk2 -> image
	// 2), so this loop's processing order (images ascending, then
	// (file_id, chunk_index)) is NOT necessarily the file's byte
	// order. The accumulation therefore feeds only the exact next
	// chunk index and BREAKS on any out-of-order feed: a
	// non-contiguously-assigned parent never compares (the final chunk
	// completes through the normal/fallback mark path below), so a
	// permuted stream is never hashed and an UNCHANGED file can never
	// be misclassified source-changed by the compare.
	for _, c := range inv.Chunks {
		if stop() {
			return nil
		}
		if c.Copied {
			rep.AlreadyCopied++
			chunkHashes[chunkKey{c.FileID, c.ChunkIndex}] = c.Hash
			continue
		}
		if _, ok := r.refuseUnsafeRelPath(ctx, &rep, imageID, c.ParentRelPath); !ok {
			complete = false
			continue
		}
		chunkMediaPath, err := media.ChunkMediaPath(c.ParentRelPath, int64(c.ChunkIndex))
		if err != nil {
			r.recordSkip(ctx, &rep, imageID, c.ParentRelPath, err.Error())
			complete = false
			continue
		}
		destPath := filepath.Join(imageRoot, filepath.FromSlash(chunkMediaPath))

		// --- the whole-file hardening's per-chunk decision ----------
		wf := r.wholeFiles[c.FileID]
		if wf == nil {
			wf = r.newWholeFileCheck(c)
			if wf != nil {
				r.wholeFiles[c.FileID] = wf
			}
		}
		firstWrite := len(c.Hash) == 0
		isFinal := c.ParentNumChunks > 0 && c.ChunkIndex == c.ParentNumChunks-1
		// Eligibility INCLUDING this chunk: it is the final chunk, it
		// is a first write, and every OTHER chunk of the parent was
		// already first-written in this run. If the chunk then fails
		// or skips, the compare never runs (outcome != copied) - the
		// count invariant keeps every non-eligible path on the
		// pre-amendment behavior. When the estimate HOLDS, the in-order
		// gate guarantees the final feed is the exact next index
		// (nextExpected == numChunks-1, because !broken implies every
		// earlier feed was in order), so authoritative post-copy
		// eligibility can only fail on a final-feed READ error - the
		// fallback mark path below.
		eligible := isFinal && firstWrite && wf != nil && !wf.decided && !wf.broken &&
			wf.seenFirst+1 == wf.numChunks

		item := copyengine.Item{
			Kind: copyengine.ItemChunk, FileID: c.FileID, RelPath: c.ParentRelPath,
			MediaPath: chunkMediaPath, SourcePath: filepath.Join(r.sourceRoot, filepath.FromSlash(c.ParentRelPath)),
			DestPath:   destPath,
			SourceSize: c.ParentSize, MtimeNS: c.ParentMtimeNS, Dev: c.ParentDev, Inode: c.ParentInode,
			Size: c.Length, Offset: c.Offset, Hash: c.Hash, ChunkIndex: c.ChunkIndex,
			DeferMark: eligible,
		}
		res := copyengine.CopyItem(ctx, r.deps, item)

		if wf != nil && !wf.decided && firstWrite && res.Outcome == copyengine.OutcomeCopied {
			// Accumulate this chunk's verified media bytes into the
			// running whole-file hash (exactly-once per successful
			// copy - the retry-safe feed point, GATED ON BYTE ORDER:
			// feedFrom folds only the exact next chunk index and
			// breaks the check on an out-of-order feed, so a
			// cross-image interleaved assignment can never hash a
			// permuted stream; see wholeFileCheck).
			if err := wf.feedFrom(destPath, c.ChunkIndex); err != nil {
				r.warn(ctx, fmt.Sprintf(
					"image %d: %s: the copy-time whole-file check is skipped for this file (cannot re-read chunk %d after its verified copy: %v); the restore-time whole-file hash (FR-041) remains its end-to-end detector",
					imageID, c.ParentRelPath, c.ChunkIndex, err))
			}
			wf.noteFirstWrite()
		}

		if isFinal && wf != nil && !wf.decided {
			// THE DECISION POINT: the final chunk was processed; no
			// later chunk can ever re-decide this parent. Eligibility
			// is evaluated BEFORE decided flips - eligibleAtFinalChunk
			// requires the not-yet-decided state, which is exactly the
			// single-decision invariant.
			eligibleNow := res.Outcome == copyengine.OutcomeCopied && wf.eligibleAtFinalChunk()
			wf.decided = true
			if eligibleNow {
				if done := r.decideWholeFile(ctx, &rep, imageID, c, item, wf, res, chunkHashes); !done {
					complete = false
				}
				continue // the decision path accounted the chunk
			}
			if res.Outcome == copyengine.OutcomeCopied && item.DeferMark {
				// The DeferMark ESTIMATE held at write time but the
				// authoritative post-copy eligibility failed (the
				// final feed broke the accumulation): the bytes are
				// verified, so complete the NORMAL mark - the check
				// simply does not run (FR-041 remains the detector).
				// Verified bytes are never left unmarked for a check
				// that will not happen.
				if err := copyengine.MarkChunk(ctx, r.deps, item, res.Anchor); err != nil {
					r.recordFail(ctx, &rep, imageID, fmt.Sprintf("%s#%d", c.ParentRelPath, c.ChunkIndex),
						fmt.Sprintf("marking final chunk copied: %v", err))
					complete = false
					continue
				}
				chunkHashes[chunkKey{c.FileID, c.ChunkIndex}] = res.Anchor
				// N1 (fix round): the completion the DeferMark
				// withheld, emitted with the SAME shape the match
				// path uses (decideWholeFile): both mark-completion
				// paths now count the final chunk exactly once, so
				// the progress counters stay consistent between them.
				r.bus.Emit(progress.ItemDone{Bytes: c.Length, Path: c.ParentRelPath})
			}
			// Otherwise: not eligible (a sibling chunk was already
			// copied, had a stored hash, failed, or this one
			// failed/skipped); the normal classify below reports the
			// chunk, and a DeferMark only ever happened under the
			// estimate - re-checked above - so no other mark is owed.
		}

		if res.Outcome == copyengine.OutcomeCopied {
			chunkHashes[chunkKey{c.FileID, c.ChunkIndex}] = res.Anchor
		}
		if done := r.classify(ctx, &rep, imageID,
			fmt.Sprintf("%s#%d", c.ParentRelPath, c.ChunkIndex), res); !done {
			complete = false
			if res.Outcome == copyengine.OutcomeAborted {
				return nil
			}
		}
	}

	// --- symlinks (D-03; the T3.4 seam specials, target-verified) ---
	for _, sy := range inv.Symlinks {
		if stop() {
			return nil
		}
		if sy.Copied {
			rep.AlreadyCopied++
			continue
		}
		mediaPath, ok := r.refuseUnsafeRelPath(ctx, &rep, imageID, sy.RelPath)
		if !ok {
			complete = false
			continue
		}
		// The T3.1 UTF-8 gate for LINK TARGETS: the tree could hold a
		// raw-byte target, the JSON manifest never could - warn-and-
		// skip (the unreadable-class convention, exit-10 class).
		if !utf8.ValidString(sy.LinkTarget) {
			r.recordSkip(ctx, &rep, imageID, sy.RelPath,
				"skipped: symlink target is not valid UTF-8 (a raw-byte target cannot be faithfully recorded in the JSON manifest)")
			complete = false
			continue
		}
		destPath := filepath.Join(imageRoot, filepath.FromSlash(mediaPath))
		res := copyengine.WriteSymlink(ctx, r.deps, copyengine.SymlinkItem{
			FileID: sy.FileID, RelPath: sy.RelPath, MediaPath: mediaPath, DestPath: destPath,
			LinkTarget: sy.LinkTarget, Mode: sy.Mode, UID: sy.UID, GID: sy.GID, MtimeNS: sy.MtimeNS,
		})
		if done := r.classify(ctx, &rep, imageID, sy.RelPath, res); !done {
			complete = false
			if res.Outcome == copyengine.OutcomeAborted {
				return nil
			}
		}
	}

	// --- empty dirs (FR-037; mkdir idempotent, so they are re-asserted
	// EVERY run - their "copied" count is the durable dir skeleton) ---
	for _, d := range inv.Dirs {
		if stop() {
			return nil
		}
		mediaPath, ok := r.refuseUnsafeRelPath(ctx, &rep, imageID, d.RelPath)
		if !ok {
			complete = false
			continue
		}
		destPath := filepath.Join(imageRoot, filepath.FromSlash(mediaPath))
		res := copyengine.WriteEmptyDir(ctx, r.deps, copyengine.DirItem{
			DirID: d.DirID, RelPath: d.RelPath, MediaPath: mediaPath, DestPath: destPath,
			Mode: d.Mode, UID: d.UID, GID: d.GID, MtimeNS: d.MtimeNS,
		})
		if done := r.classify(ctx, &rep, imageID, d.RelPath, res); !done {
			complete = false
			if res.Outcome == copyengine.OutcomeAborted {
				return nil
			}
		}
	}
	rep.Dirs = len(inv.Dirs)

	// --- hard-link secondaries (D-04 link(2), AFTER their primaries) ---
	for _, sec := range inv.Secondaries {
		if stop() {
			return nil
		}
		if sec.Copied {
			rep.AlreadyCopied++
			continue
		}
		mediaPath, ok := r.refuseUnsafeRelPath(ctx, &rep, imageID, sec.RelPath)
		if !ok {
			complete = false
			continue
		}
		primaryMediaPath, err := media.FileMediaPath(sec.PrimaryRelPath)
		if err != nil {
			r.recordSkip(ctx, &rep, imageID, sec.PrimaryRelPath, err.Error())
			complete = false
			continue
		}
		destPath := filepath.Join(imageRoot, filepath.FromSlash(mediaPath))
		primaryDest := filepath.Join(imageRoot, filepath.FromSlash(primaryMediaPath))
		res := copyengine.LinkSecondary(ctx, r.deps, copyengine.LinkItem{
			FileID: sec.FileID, RelPath: sec.RelPath, MediaPath: mediaPath,
			DestPath: destPath, PrimaryDestPath: primaryDest, Size: sec.Size,
			Mode: sec.Mode, UID: sec.UID, GID: sec.GID, MtimeNS: sec.MtimeNS,
		})
		if done := r.classify(ctx, &rep, imageID, sec.RelPath, res); !done {
			complete = false
			if res.Outcome == copyengine.OutcomeAborted {
				return nil
			}
		}
	}

	// --- the export embed (same write-verify discipline, no mark) ---
	if stop() {
		return nil
	}
	exportDest := filepath.Join(imageRoot, media.ExportFileName)
	if _, _, err := copyengine.EmbedFile(ctx, r.deps, export.path, exportDest); err != nil {
		if stop() {
			return nil
		}
		r.recordFail(ctx, &rep, imageID, media.ExportFileName, fmt.Sprintf("embedding catalog export: %v", err))
		complete = false
	} else if err := cs.RecordEmbedded(ctx, "export", catalog.EmbeddedContent{
		MediaPath: media.ExportFileName, Size: export.size, Hash: export.hash,
	}); err != nil {
		if stop() {
			return nil
		}
		r.recordFail(ctx, &rep, imageID, media.ExportFileName, fmt.Sprintf("recording export content row: %v", err))
		complete = false
	}

	// --- MANIFEST.json (6.6), only for a COMPLETE image ---
	rep.Complete = complete
	if !complete {
		r.warn(ctx, fmt.Sprintf(
			"image %d is incomplete (see failures above); %s is deferred to the run that completes it",
			imageID, media.ManifestFileName))
	} else {
		if err := r.writeManifest(ctx, imageID, imageRoot, inv, chunkHashes); err != nil {
			if stop() {
				return nil
			}
			r.recordFail(ctx, &rep, imageID, media.ManifestFileName, fmt.Sprintf("writing manifest: %v", err))
			rep.Complete = false
		} else {
			rep.Manifest = true
			// T5.1 COMPLETION-PATH ADDITION (2.3 images.final_path,
			// "copy dir or .iso path once built"): record where this
			// image's tree LANDED - the column verify-media resolves
			// every image from (the build path has always recorded
			// its .iso path through FinishBuildTx; the copy path
			// recorded nothing, the reported gap). T6.5: the SAME
			// statement stamps the two provenance columns the copy
			// path never wrote, mode='copy' + state='built' (see
			// catalog.RecordCopyFinalPath for the design-vocabulary
			// choice). One idempotent statement; a failure is an
			// honest run failure (exit 10) and never unwrites the
			// completed tree.
			if err := r.db.RecordCopyFinalPath(ctx, r.setID, imageID, imageRoot); err != nil {
				r.recordFail(ctx, &rep, imageID, ".", fmt.Sprintf("recording the image's final_path: %v", err))
			}
		}
	}
	return nil
}

// decideWholeFile is the whole-file hardening's FINAL-CHUNK decision
// (see the chunk-loop doc): the final chunk's bytes are written and
// read-back verified (the DeferMark CopyItem succeeded) and the
// running whole-file hash covers every chunk of the parent (the
// all-first-write eligibility gate). It returns done=true when the
// chunk COMPLETED (marked via the normal transaction) so the caller's
// completeness bit stays honest.
//
// MATCH: MarkChunk runs the exact 5.1 step-6 transaction the normal
// path would have (copied flag + 4.3 hash + image_contents + the 5.2
// parent mirror), the captured anchor joins the manifest's effective
// hashes, the withheld ItemDone is emitted (the fallback mark path in
// copyImage emits the same completion - the N1 consistency pin), and
// the chunk classifies copied.
//
// MISMATCH: the parent's content changed since the scan in a way the
// (possibly degraded) identifier comparison could not see - the bytes
// on media are TRUE new-content chunks, so NOTHING is unmarked or
// deleted (no mark reversal: the media truth stands; the copied flag
// simply never rose, so files.copied stays 0 and the mirror cannot
// flip). The failure is recorded at the FILE level: one
// source-changed skip naming the parent (run_events + exit 10 + the
// image goes incomplete, deferring its manifest). The earlier
// chunks' marks STAND - their bytes and stored hashes are true; the
// media honestly holds new-content chunks under the old rows, the
// restore-time whole-file hash (FR-041) remains the end-to-end
// detector for them, and the next scan's modification versioning
// (3.7) resolves the lineage. The unmarked final chunk is re-copied
// (and re-verified) by the next run.
func (r *copyRunner) decideWholeFile(ctx context.Context, rep *copyImageReport, imageID int64,
	c catalog.CopyChunk, item copyengine.Item, wf *wholeFileCheck, res copyengine.Result,
	chunkHashes map[chunkKey][]byte) bool {

	rel := fmt.Sprintf("%s#%d", c.ParentRelPath, c.ChunkIndex)
	got := wf.hasher.Sum(nil)
	if bytes.Equal(got, wf.expected) {
		if err := copyengine.MarkChunk(ctx, r.deps, item, res.Anchor); err != nil {
			// Verified bytes but unmarked (the engine's mark-failure
			// class): recorded, run continues, the next run re-verifies
			// and re-marks. The bytes are NOT deleted - they are
			// verified content.
			r.recordFail(ctx, rep, imageID, rel,
				fmt.Sprintf("marking final chunk copied after whole-file verify: %v", err))
			return false
		}
		chunkHashes[chunkKey{c.FileID, c.ChunkIndex}] = res.Anchor
		// The completion the DeferMark withheld, emitted exactly where
		// the engine would have emitted it (parent relpath, payload
		// bytes).
		r.bus.Emit(progress.ItemDone{Bytes: c.Length, Path: c.ParentRelPath})
		r.classify(ctx, rep, imageID, rel, res)
		return true
	}
	r.recordSkip(ctx, rep, imageID, c.ParentRelPath, fmt.Sprintf(
		"source changed (whole-file sha256 mismatch after first-writing all %d chunks: chunk bytes hash to %x, the scan-time whole-file hash is %x; the final chunk is left verified-but-unmarked on media and the earlier chunks' marks stand - the restore-time whole-file hash (FR-041) remains the end-to-end anchor)",
		c.ParentNumChunks, got, wf.expected))
	return false
}

// chunkKey identifies one chunk row for the run-local effective-hash
// map (file_id + catalog 0-based index).
type chunkKey struct {
	fileID int64
	index  int
}

// writeManifest assembles and embeds one image's MANIFEST.json (6.6):
// GenerateManifest validates the input (including the placement gate
// that every entry sits on its own image), MarshalManifest renders the
// deterministic bytes, EmbedFile publishes them under the same
// write-verify discipline as payload, and the 'manifest' self-row is
// recorded (6.6 - the manifest cannot list its own hash, so the writer
// records the row here; the media package's documented self-entry
// reading).
func (r *copyRunner) writeManifest(ctx context.Context, imageID int64, imageRoot string,
	inv catalog.CopyInventory, chunkHashes map[chunkKey][]byte) error {

	placement, err := r.db.CopyPlacement(ctx, r.setID)
	if err != nil {
		return err
	}
	families, err := r.db.CopyHardLinkFamilies(ctx, r.setID)
	if err != nil {
		return err
	}
	in, err := buildManifestInput(r.setID, imageID, inv, chunkHashes, placement, families, time.Now())
	if err != nil {
		return err
	}
	m, err := media.GenerateManifest(in)
	if err != nil {
		return err
	}
	blob, err := media.MarshalManifest(m)
	if err != nil {
		return err
	}
	// The manifest bytes come from memory, so they travel through a
	// scratch file to share EmbedFile's write-verify discipline (5.3:
	// the members at the image root are "covered by the same
	// write-verify discipline"). The scratch lives in the OS temp dir
	// (one small JSON document) and is removed immediately after the
	// embed consumes it - the PUBLISHED copy is what carries the
	// read-back guarantee.
	tmp, err := os.CreateTemp("", "isopress-manifest-*.json")
	if err != nil {
		return fmt.Errorf("creating manifest scratch: %w", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(blob); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("writing manifest scratch: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("closing manifest scratch: %w", err)
	}
	dest := filepath.Join(imageRoot, media.ManifestFileName)
	_, _, embedErr := copyengine.EmbedFile(ctx, r.deps, tmpPath, dest)
	_ = os.Remove(tmpPath)
	if embedErr != nil {
		return embedErr
	}
	// The 6.6 'manifest' self-row: the manifest cannot list its own
	// hash, so the writer records it here (media/manifest.go's
	// documented self-entry reading).
	sum := sha256.Sum256(blob)
	return r.db.CopyStore(r.setID, imageID).RecordEmbedded(ctx, "manifest", catalog.EmbeddedContent{
		MediaPath: media.ManifestFileName, Size: int64(len(blob)), Hash: sum[:],
	})
}

// buildManifestInput maps the catalog inventory shapes onto the media
// generator's input shapes (the cli wiring layer's job, 1.2). Entries
// cover this image's payload: whole files (including hard-link
// secondaries - copy mode stores them, D-04), chunks (with their
// effective stored/captured hashes), symlinks, and empty dirs.
func buildManifestInput(setID string, imageID int64, inv catalog.CopyInventory,
	chunkHashes map[chunkKey][]byte, placement []catalog.CopyImagePaths,
	families []catalog.CopyLinkFamily, createdAt time.Time) (media.ManifestInput, error) {

	in := media.ManifestInput{
		SetID:         setID,
		ImageNumber:   imageID,
		ToolVersion:   catalog.ToolVersion,
		SchemaVersion: catalog.SchemaVersion,
		CreatedAt:     createdAt,
	}
	for _, f := range inv.Files {
		if len(f.Hash) != 32 {
			return media.ManifestInput{}, fmt.Errorf(
				"file %q carries no catalog hash; unreadable-class rows never reach media", f.RelPath)
		}
		in.Entries = append(in.Entries, media.ImageEntry{
			Kind: media.KindFile, RelPath: f.RelPath, Size: f.Size, SHA256: f.Hash,
			Mode: f.Mode, UID: f.UID, GID: f.GID, MtimeNS: f.MtimeNS,
		})
	}
	for _, s := range inv.Secondaries {
		if len(s.Hash) != 32 {
			return media.ManifestInput{}, fmt.Errorf(
				"hard-link secondary %q carries no catalog hash", s.RelPath)
		}
		in.Entries = append(in.Entries, media.ImageEntry{
			Kind: media.KindFile, RelPath: s.RelPath, Size: s.Size, SHA256: s.Hash,
			Mode: s.Mode, UID: s.UID, GID: s.GID, MtimeNS: s.MtimeNS,
		})
	}
	for _, c := range inv.Chunks {
		hash := chunkHashes[chunkKey{c.FileID, c.ChunkIndex}]
		if len(hash) != 32 {
			return media.ManifestInput{}, fmt.Errorf(
				"chunk %d of %q has no stored hash (a first write that never completed?)", c.ChunkIndex, c.ParentRelPath)
		}
		in.Entries = append(in.Entries, media.ImageEntry{
			Kind: media.KindChunk, RelPath: c.ParentRelPath, ChunkIndex: int64(c.ChunkIndex),
			Size: c.Length, SHA256: hash,
			Mode: c.ParentMode, UID: c.ParentUID, GID: c.ParentGID, MtimeNS: c.ParentMtimeNS,
		})
	}
	for _, s := range inv.Symlinks {
		in.Entries = append(in.Entries, media.ImageEntry{
			Kind: media.KindSymlink, RelPath: s.RelPath, Size: s.Size, LinkTarget: s.LinkTarget,
			Mode: s.Mode, UID: s.UID, GID: s.GID, MtimeNS: s.MtimeNS,
		})
	}
	for _, d := range inv.Dirs {
		in.Entries = append(in.Entries, media.ImageEntry{
			Kind: media.KindDir, RelPath: d.RelPath,
			Mode: d.Mode, UID: d.UID, GID: d.GID, MtimeNS: d.MtimeNS,
		})
	}
	// The reassembly tables: one per chunked parent touching this
	// image, anchored on the whole-file size + hash (FR-041's second
	// verification target).
	byParent := map[string]media.ChunkedFileInput{}
	var parentOrder []string
	for _, c := range inv.Chunks {
		if _, ok := byParent[c.ParentRelPath]; !ok {
			if len(c.ParentHash) != 32 {
				return media.ManifestInput{}, fmt.Errorf(
					"chunked parent %q carries no whole-file hash", c.ParentRelPath)
			}
			byParent[c.ParentRelPath] = media.ChunkedFileInput{
				RelPath: c.ParentRelPath, WholeSize: c.ParentSize, WholeSHA256: c.ParentHash,
			}
			parentOrder = append(parentOrder, c.ParentRelPath)
		}
	}
	for _, rel := range parentOrder {
		in.ChunkedFiles = append(in.ChunkedFiles, byParent[rel])
	}
	for _, fam := range families {
		in.HardLinkGroups = append(in.HardLinkGroups, media.HardLinkGroupInput{SHA256: fam.Hash, Names: fam.Names})
	}
	for _, p := range placement {
		in.Placement = append(in.Placement, media.PlacementInput{ImageNumber: p.ImageID, Paths: p.Paths})
	}
	return in, nil
}

// refuseUnsafeRelPath is THE UTF-8 writer gate (the T3.1 convention):
// a relpath the media validators refuse (non-UTF-8, absolute, dot
// components, ...) warn-and-skips WITHOUT any filesystem effect and
// contributes to the partial-failure class (exit 10). Returns ok=false
// when the item must be skipped.
func (r *copyRunner) refuseUnsafeRelPath(ctx context.Context, rep *copyImageReport, imageID int64, rel string) (string, bool) {
	mediaPath, err := media.FileMediaPath(rel)
	if err != nil {
		r.recordSkip(ctx, rep, imageID, rel, fmt.Sprintf("%v", err))
		return "", false
	}
	return mediaPath, true
}

// --- the B1 partial-collision gate (fix round, pipeline-owned) ---------------

// THE B1 DEFECT (blocking, real-repro'd): two legal source files `a`
// and `a.partial` on one image. When `a.partial` is
// copied+verified+marked first (copied=1 + image_contents row +
// MANIFEST entry), the subsequent copy of sibling `a` derives its
// partial path as files/a.partial - the COMPLETED FINAL of the other
// item - and copyengine step 2a's os.Remove(partial) DELETES it:
// marked+verified bytes gone, and resume never repairs because the
// copied flag IS the trust (12.7). The wipe hit ~50% of runs because
// the file_id (writing) order is hash-pool nondeterministic.
//
// WHY THE GATE LIVES IN THIS PIPELINE LAYER AND NOT IN
// copyengine.CopyItem: the engine is deliberately inventory-blind -
// CopyItem receives ONE Item at a time (row-shaped data from the
// caller, DESIGN_SPEC 1.2) and structurally CANNOT know whether its
// derived partial path is another member's final media path; it holds
// no image inventory, and giving it one would break the pure
// single-item verify-then-mark contract the trust model rests on. The
// PIPELINE (this file) loads each image's complete inventory
// (catalog.CopyInventory) before invoking the writer, so the
// inventory-aware check belongs HERE: one path-set build per image,
// one O(1) map lookup per item.
//
// SCOPE - the exemption reasoning (pinned by the gate unit test):
//   - Whole-file items are the ONLY gated writers: they are the only
//     items whose derived partial path (files/<rel>.partial) can
//     equal another member's final.
//   - Chunks are EXEMPT: chunk finals live under the disjoint chunks/
//     root with fixed 5-digit names (chunks/<rel>/NNNNN), while every
//     whole-file partial lives under files/ - the two can never be
//     equal (the 5.3 disjoint-roots collision immunity), and a chunk
//     partial (…/NNNNN.partial) is structurally never a media name.
//   - The MANIFEST.json / catalog-export.sqlite embeds are EXEMPT:
//     they are TOOL-OWNED image-root members; payload finals and
//     their partials always sit under files/ or chunks/, never at the
//     image root, so the embeds' derived partials
//     (MANIFEST.json.partial etc.) can never be a payload final.
//   - Symlinks, empty dirs, and hard-link secondaries take NO partial
//     write (nothing derived is ever removed), so they are never
//     GATED - but they occupy final media paths, so they join the
//     path-set as collision OWNERS.

// imageFinalMediaPaths builds the mediaPath -> owner-relpath set of
// EVERY final member path of one image's complete inventory (files,
// symlinks, empty dirs, hard-link secondaries under files/; chunks
// under chunks/). It runs once per image, over the inventory the
// pipeline has already loaded (no extra catalog read). Rows the media
// validators refuse are OMITTED - the UTF-8 gate refuses them before
// any write, so they can never be present on media to collide with.
func imageFinalMediaPaths(inv catalog.CopyInventory) map[string]string {
	finals := make(map[string]string, len(inv.Files)+len(inv.Symlinks)+
		len(inv.Dirs)+len(inv.Secondaries)+len(inv.Chunks))
	add := func(mediaPath, ownerRel string) {
		if _, dup := finals[mediaPath]; !dup {
			finals[mediaPath] = ownerRel
		}
	}
	for _, f := range inv.Files {
		if mp, err := media.FileMediaPath(f.RelPath); err == nil {
			add(mp, f.RelPath)
		}
	}
	for _, s := range inv.Symlinks {
		if mp, err := media.FileMediaPath(s.RelPath); err == nil {
			add(mp, s.RelPath)
		}
	}
	for _, d := range inv.Dirs {
		if mp, err := media.FileMediaPath(d.RelPath); err == nil {
			add(mp, d.RelPath)
		}
	}
	for _, s := range inv.Secondaries {
		if mp, err := media.FileMediaPath(s.RelPath); err == nil {
			add(mp, s.RelPath)
		}
	}
	for _, c := range inv.Chunks {
		if mp, err := media.ChunkMediaPath(c.ParentRelPath, int64(c.ChunkIndex)); err == nil {
			add(mp, c.ParentRelPath)
		}
	}
	return finals
}

// partialPathCollision is THE B1 gate check: given one image's final
// media-path set (imageFinalMediaPaths) and one whole-file item's
// VALIDATED media path, it reports whether the item's DERIVED PARTIAL
// PATH (mediaPath + ".partial", copyengine.PartialPath - the exact
// path CopyItem's step 2a removes as a stale partial) equals another
// inventory member's FINAL media path; on collision it returns that
// sibling's source relpath for the warning message.
//
// The check is purely structural (name arithmetic over the complete
// inventory), so it is ORDER-INDEPENDENT and STATE-INDEPENDENT: for a
// colliding pair, the item whose partial lands on its sibling's final
// (e.g. `a`, partial files/a.partial) is ALWAYS the gated one - on
// every run, in either file_id order - and the sibling
// (e.g. `a.partial`, partial files/a.partial.partial, which collides
// with nothing) always proceeds. The already-copied sibling is never
// at risk either way: the gate skips the colliding item before any
// write, and a previously-copied colliding item is trusted-skipped by
// the resume model before the gate is even consulted.
func partialPathCollision(finals map[string]string, mediaPath string) (siblingRel string, collided bool) {
	siblingRel, collided = finals[copyengine.PartialPath(mediaPath)]
	return siblingRel, collided
}

// recordSkip is the warn-and-skip accounting of the UTF-8 gate: a
// counted skip (exit-10 class), a report line, a Warn event (the
// engine emits its own for 5.1 skips; pipeline-level refusals emit
// here), and a run_events row (2.4 provenance).
func (r *copyRunner) recordSkip(ctx context.Context, rep *copyImageReport, imageID int64, rel, detail string) {
	rep.Skipped++
	line := fmt.Sprintf("copy: image %d: %s: skipped: %s", imageID, rel, detail)
	r.failureLines = append(r.failureLines, line)
	r.bus.Emit(progress.Warn{Msg: fmt.Sprintf("image %d: %s: %s", imageID, rel, detail)})
	r.insertEvent(ctx, rel, detail)
}

// recordFail is the pipeline-level failure accounting (export embed,
// manifest plumbing): a counted failure (exit-10 class), a report
// line, an Error event, and a run_events row.
func (r *copyRunner) recordFail(ctx context.Context, rep *copyImageReport, imageID int64, rel, detail string) {
	rep.Failed++
	line := fmt.Sprintf("copy: image %d: %s: %s", imageID, rel, detail)
	r.failureLines = append(r.failureLines, line)
	r.bus.Emit(progress.Error{Msg: line})
	r.insertEvent(ctx, rel, detail)
}

// warn records a non-fatal per-image note (5.4 metadata warnings,
// deferred manifests): Warn event + run_events, NO count - warnings
// do not flip the exit code (5.4's "per-file warning" class).
func (r *copyRunner) warn(ctx context.Context, msg string) {
	r.bus.Emit(progress.Warn{Msg: msg})
	r.insertEvent(ctx, "", msg)
}

// insertEvent appends one run_events row (best-effort: during the
// interruption tail the run context is cancelled and the driver
// rejects statements - the report lines on stdout remain the
// authoritative record of that window).
func (r *copyRunner) insertEvent(ctx context.Context, rel, detail string) {
	if ctx.Err() != nil {
		return
	}
	_ = r.db.InsertRunEvent(ctx, catalog.RunEvent{
		RunID: r.runID, At: time.Now().UnixNano(), Level: "warn", RelPath: rel, Detail: detail,
	})
}

// classify folds one engine Result into the image report and the run
// accumulators. It returns true when the item COMPLETED (copied), so
// the caller flips its completeness bit; skips and failures record
// their lines here (the engine already emitted the Warn/Error events
// through the nil-safe bus), and an abort carrying ErrHookAbort
// propagates as the deterministic test-seam terminal.
func (r *copyRunner) classify(ctx context.Context, rep *copyImageReport, imageID int64, rel string, res copyengine.Result) bool {
	switch res.Outcome {
	case copyengine.OutcomeCopied:
		rep.Copied++
		rep.Bytes += res.Bytes
		if res.Warning != "" {
			// 5.4 per-file metadata warning: counted, reported, never
			// an exit-code flip (the content is verified and marked).
			r.metadataWarnings++
			r.warn(ctx, fmt.Sprintf("image %d: %s: %s", imageID, rel, res.Warning))
		}
		return true
	case copyengine.OutcomeSkipped:
		rep.Skipped++
		r.failureLines = append(r.failureLines,
			fmt.Sprintf("copy: image %d: %s: skipped: %v", imageID, rel, res.Err))
		r.insertEvent(ctx, rel, res.Err.Error())
		return false
	case copyengine.OutcomeAborted:
		if errors.Is(res.Err, copyengine.ErrHookAbort) {
			// The deterministic test seam: propagate as terminal.
			r.terminal = errors.Join(r.terminal, res.Err)
		}
		return false
	default: // OutcomeFailed
		rep.Failed++
		r.failureLines = append(r.failureLines,
			fmt.Sprintf("copy: image %d: %s: failed after %d attempt(s): %v", imageID, rel, res.Attempts, res.Err))
		r.insertEvent(ctx, rel, res.Err.Error())
		return false
	}
}

// finalize stamps the run row on EVERY exit path (2.4) and returns the
// command result: interrupted (130) > terminal error (1) > item
// failures (10, or 1 with --strict) > nil (0). The report lands on
// stdout first (FR-046: the result is the stdout artifact even on the
// failure paths, so cron logs capture what happened). summary is
// written back through the pointer so the caller's deferred Done
// emission carries the final numbers (the scan.go convention).
func (r *copyRunner) finalize(ctx context.Context, summary *string, runErr error) error {
	// THE INTERRUPTED-RUN DEFERRAL CLOSURE (the file doc): the status
	// decision happens FIRST, because a cancelled context is the one
	// state no other outcome can mask.
	interrupted := ctx.Err() != nil
	tot := r.totals()
	status := "ok"
	switch {
	case interrupted:
		status = "interrupted"
	case r.terminal != nil || runErr != nil:
		status = "failed"
	case tot.Failed > 0 || tot.Skipped > 0:
		status = "partial"
	}
	stats, merr := json.Marshal(struct {
		Copied          int                  `json:"copied"`
		Skipped         int                  `json:"skipped"`
		Failed          int                  `json:"failed"`
		Bytes           int64                `json:"bytes"`
		ChangeDetection changeDetectionAudit `json:"change_detection"`
	}{tot.Copied, tot.Skipped, tot.Failed, tot.Bytes, auditOf(r.cdMode)})
	if merr != nil {
		stats = []byte("{}")
	}
	// FinishRunDetached: the post-cancel UPDATE on a fresh short-timeout
	// context - the T0.5/T1.6 interrupted-run deferral closure (the
	// driver rejects the write under the cancelled run context, which
	// is exactly why interrupted runs used to stay 'running').
	if err := r.db.FinishRunDetached(r.runID, status, time.Now().UnixNano(), string(stats)); err != nil {
		r.terminal = errors.Join(r.terminal, err)
	}

	rep := copyReport{
		SetID: r.setID, RunID: r.runID, Dest: r.destAbs, Status: status,
		Copied: tot.Copied, AlreadyCopied: tot.AlreadyCopied, Skipped: tot.Skipped,
		Failed: tot.Failed, Dirs: tot.Dirs, Bytes: tot.Bytes,
		MetadataWarnings: r.metadataWarnings, Images: r.images, Failures: r.failureLines,
	}
	// THE NON-NIL DEFAULT (4.6.1 channel 2): the frozen `wipes` key is
	// ALWAYS present, `[]` when no wipe happened (a nil slice would
	// encode as `null` and break the type pin).
	rep.Wipes = r.wipes
	if rep.Wipes == nil {
		rep.Wipes = []wipeRecord{}
	}
	if err := writeCopyReport(r.cmd.OutOrStdout(), rep, r.jsonFlag); err != nil {
		r.terminal = errors.Join(r.terminal, err)
	}

	*summary = fmt.Sprintf(
		"copy: files %d copied (%d already present), %s; skipped %d, failed %d, dirs %d, metadata warnings %d",
		tot.Copied, tot.AlreadyCopied, units.FormatBytes(tot.Bytes),
		tot.Skipped, tot.Failed, tot.Dirs, r.metadataWarnings)
	if interrupted {
		// T6.2 addendum F1 (scan.go's reference shape, scan.go:457-459
		// and 500-505): JOIN r.terminal instead of dropping it. A
		// FinishRunDetached (or report-write) failure collected on the
		// interrupt path used to be discarded here - the lost stamp was
		// silent - while scan surfaced both causes. The join keeps the
		// interrupted class (exit 130: report.ErrInterrupted is the
		// typed head) AND carries the original failure to stderr.
		return errors.Join(fmt.Errorf("%w: copy run %d interrupted; completed items are marked and resume on the next run",
			report.ErrInterrupted, r.runID), r.terminal)
	}
	if runErr != nil {
		return runErr
	}
	if r.terminal != nil {
		return r.terminal
	}
	if tot.Failed > 0 || tot.Skipped > 0 {
		if r.strict {
			return fmt.Errorf("copy completed with %d item failure(s) and %d skip(s); --strict promotes this to a fatal error",
				tot.Failed, tot.Skipped)
		}
		var errs []error
		for _, line := range r.failureLines {
			errs = append(errs, errors.New(line))
		}
		return &report.ItemFailures{Failures: errs}
	}
	return nil
}

// totals folds the per-image reports into run totals.
func (r *copyRunner) totals() copyImageReport {
	var t copyImageReport
	for _, img := range r.images {
		t.Copied += img.Copied
		t.AlreadyCopied += img.AlreadyCopied
		t.Skipped += img.Skipped
		t.Failed += img.Failed
		t.Bytes += img.Bytes
		t.Dirs += img.Dirs
	}
	return t
}

// --- the report model (human and --json; the key set is FROZEN) --------------

// copyImageReport is one image's write outcome. The JSON key set is a
// deliberate frozen schema (the scan/plan report precedent): new keys
// are a conscious schema change with the pinning tests updated in the
// same commit.
type copyImageReport struct {
	Image         int64 `json:"image"`
	Copied        int   `json:"copied"`
	AlreadyCopied int   `json:"already_copied"`
	Skipped       int   `json:"skipped"`
	Failed        int   `json:"failed"`
	Dirs          int   `json:"dirs"`
	Bytes         int64 `json:"bytes"`
	Complete      bool  `json:"complete"`
	Manifest      bool  `json:"manifest_written"`
}

// pendingTotals counts the PENDING payload items and bytes of an
// inventory (already-copied items are trusted skips and emit no
// ItemDone, so they must not inflate the determinate phase totals).
// Dirs are always re-asserted (idempotent mkdir) and carry zero bytes;
// they are counted in the totals so the bar reaches completion.
func pendingTotals(inv catalog.CopyInventory) (files, bytes int64) {
	for _, f := range inv.Files {
		if !f.Copied {
			files++
			bytes += f.Size
		}
	}
	for _, s := range inv.Symlinks {
		if !s.Copied {
			files++
			bytes += s.Size
		}
	}
	for _, c := range inv.Chunks {
		if !c.Copied {
			files++
			bytes += c.Length
		}
	}
	for _, s := range inv.Secondaries {
		if !s.Copied {
			files++
			bytes += s.Size
		}
	}
	files += int64(len(inv.Dirs))
	return files, bytes
}

// copyReport is the end-of-run report of the copy command (human lines
// and --json are two presentations of the same numbers, FR-046: stdout
// only).
type copyReport struct {
	SetID  string `json:"set_id"`
	RunID  int64  `json:"run_id"`
	Dest   string `json:"dest"`
	Status string `json:"status"` // ok | partial | failed | interrupted (runs.status CHECK)
	Copied int    `json:"copied"`
	// AlreadyCopied is the resume dividend: items whose copied flag
	// already stood (12.7/FR-013 - trusted, not re-verified).
	AlreadyCopied    int               `json:"already_copied"`
	Skipped          int               `json:"skipped"`
	Failed           int               `json:"failed"`
	Dirs             int               `json:"dirs"`
	Bytes            int64             `json:"bytes"`
	MetadataWarnings int               `json:"metadata_warnings"`
	Images           []copyImageReport `json:"images"`
	// Wipes is the 4.6.1 channel-2 record of every copy-time wipe (the
	// ratified frozen-key addition that moved copy's key set 12 -> 13).
	// It is NEVER nil at assembly (finalize substitutes an empty slice),
	// so the `wipes` key is always present and `[]` when nothing was
	// wiped - the frozen-key contract, not an omitempty field.
	Wipes    []wipeRecord `json:"wipes"`
	Failures []string     `json:"failures,omitempty"`
}

// writeCopyReport renders the report: --json emits exactly one JSON
// document; the human form is the set/run identity line, one line per
// image, a totals line, the wipe lines (4.6.1 channel 1 - absent when
// nothing was wiped), the metadata-warnings line, and one line per
// failure (the plan/scan family style).
func writeCopyReport(w io.Writer, rep copyReport, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	if _, err := fmt.Fprintf(w, "copy: set %s (run %d)\n", rep.SetID, rep.RunID); err != nil {
		return err
	}
	for _, img := range rep.Images {
		if _, err := fmt.Fprintf(w,
			"image %d: copied %d, already-copied %d, skipped %d, failed %d, dirs %d, %s (complete: %v, manifest: %v)\n",
			img.Image, img.Copied, img.AlreadyCopied, img.Skipped, img.Failed, img.Dirs,
			units.FormatBytes(img.Bytes), img.Complete, img.Manifest); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w,
		"totals: copied %d, already-copied %d, skipped %d, failed %d, dirs %d, %s (status: %s)\n",
		rep.Copied, rep.AlreadyCopied, rep.Skipped, rep.Failed, rep.Dirs,
		units.FormatBytes(rep.Bytes), rep.Status); err != nil {
		return err
	}
	// 4.6.1 CHANNEL 1, the EXACT position: after the totals line,
	// BEFORE the metadata-warnings line, and before the failure lines -
	// one line per wipe; ZERO wipes = zero lines, so a no-wipe run's
	// human stdout stays byte-identical to the pre-wipe-gate output.
	for _, wp := range rep.Wipes {
		if _, err := fmt.Fprintf(w,
			"wiped: image %d: removed %d entries (%s) from %s before rewrite\n",
			wp.Image, wp.EntriesRemoved, units.FormatBytes(wp.BytesRemoved), wp.Path); err != nil {
			return err
		}
	}
	if rep.MetadataWarnings > 0 {
		if _, err := fmt.Fprintf(w, "metadata warnings: %d\n", rep.MetadataWarnings); err != nil {
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

// --- the 12.7 test-only injection seam ---------------------------------------

// The fail-after-N hook of DESIGN_SPEC 12.7 is an INJECTABLE SEAM on
// the engine; the crash/resume and interrupted e2e tests need it
// reachable from a re-exec child running the REAL command tree, so
// this env-var adapter wires it in the production command. It is
// inert unless ISOPRESS_TEST_COPY_FAIL_AFTER is set (a positive
// integer), it is documented as test-only in every consumer, and it
// cannot fire in production use: the variables carry the
// ISOPRESS_TEST_ prefix the whole suite reserves for re-exec
// children (cli_test.go's helper envs).
//
//	ISOPRESS_TEST_COPY_FAIL_AFTER=N   trigger on the N-th event
//	ISOPRESS_TEST_COPY_FAIL_KIND      "partial" (default) | "rename"
//	ISOPRESS_TEST_COPY_FAIL_MODE      "error" (default) | "block" | "kill"
//
// Modes: "error" returns an ErrHookAbort-wrapped error (the run stops
// deterministically, in-process); "block" prints COPY-HOOK-REACHED to
// stderr and parks until the context is cancelled (the deterministic
// SIGINT e2e - the parent signals once the sentinel arrives); "kill"
// prints the sentinel and SIGKILLs this process mid-item (the 12.7
// crash child - nothing after the hook runs, exactly like a real
// crash).
func copyTestHookFromEnv() (copyengine.Hook, error) {
	after := os.Getenv("ISOPRESS_TEST_COPY_FAIL_AFTER")
	if after == "" {
		return nil, nil
	}
	var n int64
	if _, err := fmt.Sscanf(after, "%d", &n); err != nil || n <= 0 {
		return nil, fmt.Errorf("copy: ISOPRESS_TEST_COPY_FAIL_AFTER=%q is not a positive integer", after)
	}
	kind := copyengine.HookAfterPartialWrite
	switch os.Getenv("ISOPRESS_TEST_COPY_FAIL_KIND") {
	case "", "partial":
	case "rename":
		kind = copyengine.HookAfterRename
	default:
		return nil, fmt.Errorf("copy: ISOPRESS_TEST_COPY_FAIL_KIND must be partial or rename")
	}
	mode := os.Getenv("ISOPRESS_TEST_COPY_FAIL_MODE")
	switch mode {
	case "", "error", "block", "kill":
	default:
		return nil, fmt.Errorf("copy: ISOPRESS_TEST_COPY_FAIL_MODE must be error, block, or kill")
	}
	var seen int64
	return func(ctx context.Context, ev copyengine.HookEvent) error {
		if ev.Kind != kind {
			return nil
		}
		seen++
		if seen < n {
			return nil
		}
		switch mode {
		case "block":
			fmt.Fprintln(os.Stderr, "COPY-HOOK-REACHED")
			<-ctx.Done()
			return fmt.Errorf("%w: unblocked by cancellation", copyengine.ErrHookAbort)
		case "kill":
			fmt.Fprintln(os.Stderr, "COPY-HOOK-REACHED")
			// The 12.7 crash child: SIGKILL this process AT the hook.
			// Nothing between here and the kernel's exit runs - no
			// defer, no cleanup - which is exactly the crash window
			// the resume test needs.
			_ = syscall.Kill(syscall.Getpid(), syscall.SIGKILL)
			return fmt.Errorf("%w: SIGKILL failed", copyengine.ErrHookAbort)
		default: // "error"
			return fmt.Errorf("%w: fail-after-%d requested by test env", copyengine.ErrHookAbort, n)
		}
	}, nil
}
