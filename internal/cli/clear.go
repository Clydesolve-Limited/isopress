package cli

// T2.4: the real `clear-assignments` command (DESIGN_SPEC 1.1 command
// tree, 4.5 clear semantics, 2.4 locking enforcement - clear is the
// only other writer; PLAN_SPEC FR-010, FR-011; IMPLEMENTATION_PLAN
// T2.4). It replaces the T0.5 placeholder in commands.go. U.1 (the
// selective unlock, DESIGN_SPEC 4.6 / FR-047) extends it with the
// `--image N` (repeatable) and `--last` selectors.
//
// Wiring contracts:
//
//   - MUTATING catalog open (catalog.OpenReadWrite): the exclusive
//     advisory flock is held for the whole run; contention surfaces as
//     *catalog.CatalogBusy and report.Classify maps it to exit 5
//     (T0.4 wiring). Like plan/report and unlike scan, a missing
//     --catalog path is NOT auto-created (there is nothing to clear in
//     an empty database; creation is scan's job): a missing path is a
//     KindOpen CatalogError (exit 4).
//   - Flags: the global set only (--catalog, --set-id, --json, --quiet,
//     --strict) PLUS the 4.6 selectors --image (Int64Slice,
//     repeatable, the copy.go shape) and --last (bool). --set-id is
//     required (same documented reason as plan/report: no ROOT
//     argument to derive a default from, no set enumeration read); a
//     never-scanned set is a usage error (exit 2, "run scan first").
//   - Selector validation (DESIGN 4.6) runs BEFORE the transaction,
//     after the open and the set-existence gate: the resolved set is
//     the UNION of --image values and (with --last) the highest
//     planned id, deduplicated and sorted ascending; every id must be
//     >= 1 (ids are 1-based - 0/-1 gets the explicit 1-based message)
//     and present in the set's planned images, else usage error exit 2
//     naming the planned ids. Row absence (an image unlocked twice)
//     is the SAME exit-2 class as never-planned. `--last` on a set
//     with no planned images is exit 2 with plan-first guidance.
//     NO SELECTOR FLAGS = the full clear, behaviorally byte-for-byte
//     identical to the pre-feature command (AC-08).
//   - Service call: catalog.DB.ClearPlan - ONE BEGIN IMMEDIATE
//     transaction (DESIGN_SPEC 4.5 / FR-010, 4.6 for the scoped
//     form): files.image_id/copied cleared, chunks placements cleared
//     (ROWS, HASHES, AND the files.chunk_size stamp RETAINED - FR-010,
//     4.2 reuse), directories.image_id cleared, the destination rows
//     deleted (the documented clearstore.go decision that makes the
//     subsequent plan behave exactly like a fresh plan), runs row kind
//     'clear'. A failed clear rolls back to a bit-identical catalog; a
//     clear with nothing to clear rolls its empty transaction back (no
//     run row, zero changes) and exits 0 with a "nothing to clear"
//     report - the T2.2 plan no-op shape.
//   - Report on stdout (FR-046, 10.3): what was cleared (files, chunks,
//     dirs), destinations removed/unlocked, the retained chunk-hash
//     count, and the frozen `unlocked_images` array (9 -> 10 keys, the
//     deliberate T6.2 addition - selected ids when scoped, ALL removed
//     ids when full, present in BOTH modes and as [] on a completed
//     no-op); --json emits the frozen-schema document (clear_test.go
//     pins the 10-key set). Scoped runs add ONE human line,
//     "unlocked images: <ids>"; a full clear's human bytes stay
//     identical (AC-08). Progress phase + one summary line on stderr
//     through the T1.1 bus, Done on every exit path.
//   - Exit codes: nil -> 0; usage inputs (incl. every selector
//     validation failure) -> 2; catalog problems -> 4; lock contention
//     -> 5; everything unexpected -> 1. NO NEW exit codes in either
//     mode, and never 10 (the clear is one atomic transaction with NO
//     per-item failure path). --strict has no promotion target: the
//     flag is accepted and inert (the documented plan/report judgment,
//     T2.3).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"isopress/internal/catalog"
	"isopress/internal/progress"
	"isopress/internal/report"
)

// clearOptions carries the DESIGN 4.6 selector flags of one
// clear-assignments invocation.
type clearOptions struct {
	images []int64 // --image, repeatable (Int64Slice, the copy.go shape)
	last   bool    // --last: the highest planned image id
}

// newClearAssignmentsCommand builds the real clear-assignments command
// (replaces the T0.5 placeholder). No positional arguments; the input
// arrives through the global flags plus the 4.6 selectors.
func newClearAssignmentsCommand(opts *rootOptions) *cobra.Command {
	co := clearOptions{}
	cmd := &cobra.Command{
		Use:   "clear-assignments",
		Short: "clear plan assignments in one transaction (whole set, or selected --image/--last; keeps rows and hashes)",
		Long: "Clear plan assignments of the --set-id set in ONE transaction " +
			"(DESIGN_SPEC 4.5, FR-010, 4.6). With NO selector the command clears " +
			"EVERY plan assignment: file and empty-directory assignments (image_id) " +
			"are set to NULL, copied flags and chunk placements (chunks.image_id) are " +
			"cleared, and the set's destination rows are removed, so a subsequent plan " +
			"behaves EXACTLY like a fresh plan on unchanged inputs (FR-011: this is the " +
			"only unlock path). With --image N (repeatable) or --last the command clears " +
			"ONLY the selected images' assignments, marks, and destination rows; every " +
			"other image keeps its assignments, marks, chunk placements, and media " +
			"untouched, and the next plan re-mints the released id at its lowest-unused " +
			"slot (DESIGN_SPEC 4.6). The clear " +
			"deletes NO file records, NO hashes, and NO chunk rows: records, " +
			"chunk rows, and stored chunk hashes are retained verbatim (FR-010), " +
			"so a re-plan with the same effective chunk size reuses them with " +
			"zero source reads (DESIGN_SPEC 4.2 boundary stability). Like plan " +
			"and unlike scan, a missing catalog file at --catalog is NOT created " +
			"(exit 4): run scan first. Progress renders on stderr; the clear " +
			"report lands on stdout (JSON with --json). A run with nothing to " +
			"clear exits 0 with a 'nothing to clear' report and changes nothing.\n\n" +
			"Exit codes: 0 success, including 'nothing to clear'; 1 fatal unexpected error; 2 usage " +
			"(missing --set-id, a set that has never been scanned); 4 catalog error, including a " +
			"missing catalog file; 5 catalog busy (another mutating run holds the lock); 130 " +
			"interrupted. The clear is one atomic transaction, so it has no per-item failure class, " +
			"never exits 10, and --strict has no effect. JSON: --json prints the final report as " +
			"exactly one JSON document on stdout; a run that aborts before doing work prints no " +
			"document at all; --json never changes the exit code.\n\n" +
			"Unlocking images with `--image N` (repeatable) or `--last` releases only those " +
			"images' assignments in the catalog; it never touches the media on disk. The next " +
			"`copy` run of a rewritten image first REMOVES the existing `<dest>/<SET-ID>-NNN` " +
			"directory (it is about to be rewritten in that same run) and records the removal " +
			"in the run report and the catalog's run log. A directory whose `MANIFEST.json` " +
			"names a different set or image, or cannot be parsed as a manifest, is refused " +
			"instead of removed; other images' trees are never removed (a copy run still " +
			"refreshes each tree's manifest/export, as before).",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runClearAssignments(cmd.Context(), cmd, opts, co)
		},
	}
	cmd.Flags().Int64SliceVar(&co.images, "image", nil,
		"unlock only this planned image number (repeatable; default: the whole set). "+
			"Ids are 1-based and must be planned images of the set (exit 2 otherwise, "+
			"naming the planned ids)")
	cmd.Flags().BoolVar(&co.last, "last", false,
		"unlock the highest planned image id (unioned with any --image; default: false)")
	return cmd
}

// clearReport is the end-of-run report model - the human lines and the
// --json document are two presentations of the same numbers
// (DESIGN_SPEC 10.3: this is the command RESULT, stdout only).
type clearReport struct {
	Mode  string `json:"mode"` // always "clear"
	SetID string `json:"set_id"`
	RunID int64  `json:"run_id"` // runs row (kind 'clear'); 0 on a nothing-to-clear no-op
	// What was cleared: rows that carried assignment state, and the
	// destination rows removed (unlocked) with them.
	FilesCleared        int `json:"files_cleared"`
	ChunksCleared       int `json:"chunks_cleared"`
	DirsCleared         int `json:"dirs_cleared"`
	DestinationsRemoved int `json:"destinations_removed"`
	// ChunkHashesRetained surfaces the FR-010 / 4.2 contract: stored
	// chunk hashes kept verbatim, the reuse budget of the next plan.
	ChunkHashesRetained int `json:"chunk_hashes_retained"`
	// UnlockedImages is the DESIGN 4.6 / M6 frozen-key addition (the
	// deliberate 9 -> 10 key move under the T6.2 protocol): sorted
	// image numbers - the selected ids when scoped, ALL removed ids
	// for a full clear - present in BOTH modes and as an empty array
	// on a completed no-op (never omitempty: the frozen key set
	// requires presence).
	UnlockedImages []int64 `json:"unlocked_images"`
	NothingToClear bool    `json:"nothing_to_clear"`
	// scoped marks a selector-driven run. It is a presentation-mode
	// flag only - deliberately UNEXPORTED so it never joins the frozen
	// JSON document: the human renderer adds exactly one line
	// ("unlocked images: <ids>") for scoped runs, while a full clear's
	// human bytes stay identical to the pre-feature output (AC-08).
	scoped bool
}

// clearReportJSONKeys is the frozen TOP-LEVEL key set of the
// clear-assignments --json document (10 keys). Deliberate T2.4 schema,
// frozen exactly like the scan report's (scan_hardening_test.go) and
// the plan/report document's (planreport_test.go): any addition,
// removal, or rename must update this slice consciously, alongside
// clear_test.go's pin through the REAL command.
//
// The move 9 -> 10 is the T6.2 frozen-key protocol's DELIBERATE
// addition: `unlocked_images` (DESIGN_SPEC 4.6, M6) - sorted []int,
// present in BOTH modes including [] on a completed no-op - ratified
// with the same reasoning as copy's `wipes` key. The type map pin
// lives in json_contract_test.go's clearJSONTypes.
var clearReportJSONKeys = []string{
	"chunk_hashes_retained", "chunks_cleared", "destinations_removed",
	"dirs_cleared", "files_cleared", "mode", "nothing_to_clear",
	"run_id", "set_id", "unlocked_images",
}

// resolveClearSelection resolves the DESIGN 4.6 selectors against the
// set's planned images, BEFORE the unlock transaction (the open and
// the set-existence gate already ran). It returns nil for NO SELECTOR
// FLAGS - the full clear, byte-for-byte the pre-feature command -
// otherwise the resolved set: the UNION of the --image values and
// (with --last) the highest planned id, deduplicated and sorted
// ascending, validated per the 4.6 rules:
//
//   - every id >= 1 (ids are 1-based, 6.4): 0/-1 is exit 2 with the
//     explicit 1-based message, checked on the raw --image values so
//     the message wins over the membership one;
//   - every id present in the set's planned images, else exit 2
//     naming the planned ids. Row absence (an image unlocked twice,
//     across two invocations) takes this SAME path: the catalog keeps
//     no per-image history, and both mean "not a planned image of
//     this set";
//   - a set with no planned images (the --last case, and --image on a
//     never-planned set) is exit 2 with plan-first guidance.
//
// --strict stays inert (accepted, no effect); no selector path can
// produce any exit code beyond {0, 2} before the transaction.
func resolveClearSelection(ctx context.Context, db *catalog.DB, cmd *cobra.Command, opts *rootOptions, co clearOptions) ([]int64, error) {
	if len(co.images) == 0 && !co.last {
		return nil, nil // NO SELECTOR FLAGS = FULL CLEAR
	}
	for _, id := range co.images {
		if id < 1 {
			return nil, &report.UsageError{
				Command: cmd.CommandPath(),
				Msg:     fmt.Sprintf("--image %d is invalid: image ids are 1-based (the lowest image id is 1)", id),
			}
		}
	}
	planned, err := db.CopyImageIDs(ctx, opts.setID)
	if err != nil {
		return nil, err
	}
	if len(planned) == 0 {
		return nil, &report.UsageError{
			Command: cmd.CommandPath(),
			Msg: fmt.Sprintf("set %q has no planned images (run 'isopress plan --catalog %s --set-id %s --size ...' first)",
				opts.setID, opts.catalog, opts.setID),
		}
	}
	var selected []int64
	if len(co.images) > 0 {
		// selectCopyImages is the copy.go resolver: dedupe (a repeated
		// --image is harmless), sort ascending, and refuse a
		// never-planned id with exit 2 naming the planned ids.
		if selected, err = selectCopyImages(cmd, planned, co.images); err != nil {
			return nil, err
		}
	}
	if co.last {
		// planned is ORDER BY image_id (CopyImageIDs), so the last
		// element is the highest planned id.
		selected = append(selected, planned[len(planned)-1])
		selected = dedupeSortedInt64(selected)
	}
	// The 1-based rule of the resolved set (covers the --last-derived
	// id against a corrupt catalog; the raw values were checked above).
	for _, id := range selected {
		if id < 1 {
			return nil, &report.UsageError{
				Command: cmd.CommandPath(),
				Msg:     fmt.Sprintf("image %d is invalid: image ids are 1-based (the lowest image id is 1)", id),
			}
		}
	}
	return selected, nil
}

// dedupeSortedInt64 sorts ids ascending and drops repeats - the union
// step of the 4.6 selector resolution (--last may duplicate an
// already-selected --image id, and the catalog-side normalization
// expects a canonical list).
func dedupeSortedInt64(ids []int64) []int64 {
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]int64, 0, len(ids))
	for i, id := range ids {
		if i == 0 || id != ids[i-1] {
			out = append(out, id)
		}
	}
	return out
}

// runClearAssignments is the clear-assignments service invocation (see
// the file doc for the wiring contracts).
func runClearAssignments(ctx context.Context, cmd *cobra.Command, opts *rootOptions, co clearOptions) (runErr error) {
	if opts.catalog == "" {
		return &report.UsageError{
			Command: cmd.CommandPath(),
			Msg:     "--catalog is required (path to the SQLite catalog file)",
		}
	}
	if opts.setID == "" {
		// Same documented reason as plan/report (planreport.go): no
		// ROOT argument to derive a default set from, no set
		// enumeration read in the catalog.
		return &report.UsageError{
			Command: cmd.CommandPath(),
			Msg:     "--set-id is required (clear-assignments operates on a set created by scan)",
		}
	}

	// MUTATING open: exclusive flock for the whole run (D-11);
	// contention classifies to exit 5 through report.Classify (T0.4).
	// NO AUTO-CREATE (the plan/report divergence from scan, T2.3 NB): a
	// missing path is a KindOpen CatalogError (exit 4) whose message
	// already says "create one first".
	db, err := catalog.Open(opts.catalog, catalog.OpenReadWrite)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck // best-effort; the pipeline result dominates

	// Set existence: the set must have been scanned (the same usage
	// gate as plan/report) - a typo'd or never-scanned set is exit 2
	// with the scan-first guidance, never a silent "nothing to clear".
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

	// DESIGN 4.6: resolve and validate --image/--last BEFORE the
	// transaction (every usage-class failure above exits 2 with ZERO
	// catalog changes). nil = the full clear.
	selected, err := resolveClearSelection(ctx, db, cmd, opts, co)
	if err != nil {
		return err
	}
	scoped := selected != nil

	// Fresh progress bus per operation (T1.1 contract); renderer chosen
	// once here per the 9.2 matrix. The clear is one short transaction,
	// so the whole operation is one "clear" phase - and the deferred
	// Finish/wait pair guarantees exactly one Done summary line on
	// EVERY exit path.
	bus := progress.NewBus()
	wait := progress.Start(progress.New(progress.Options{
		Quiet:  opts.quiet,
		Stderr: cmd.ErrOrStderr(),
	}), bus)
	summary := fmt.Sprintf("clear: aborted before any assignment was cleared (set %s)", opts.setID)
	defer func() {
		// Failed / Interrupted mark the abort path so the interactive
		// renderer CLEARS its bar instead of rendering a misleading
		// 100% (progress_done.go). clear has NO explicit
		// interrupted-status decision of its own (one short
		// transaction; it maps nothing to report.ErrInterrupted): a
		// SIGINT mid-clear surfaces as the cancellation-derived error
		// of the in-flight ClearPlan call, which the SHARED
		// runInterrupted predicate classifies - wired consistently so
		// an interrupted clear never renders a completed bar with
		// neither abort flag set. runFailed (its complement) covers
		// every other error.
		bus.Finish(progress.Done{Summary: summary, Failed: runFailed(runErr), Interrupted: runInterrupted(runErr)})
		wait()
	}()
	bus.Emit(progress.Phase{Name: "clear"})

	// The ONE transaction (DESIGN_SPEC 4.5, FR-010; 4.6 for the
	// scoped form - catalog/clearstore.go).
	res, err := db.ClearPlan(ctx, opts.setID, selected, time.Now().UnixNano())
	if err != nil {
		return err
	}

	rep := clearReport{
		Mode:                "clear",
		SetID:               opts.setID,
		RunID:               res.RunID,
		FilesCleared:        res.FilesCleared,
		ChunksCleared:       res.ChunksCleared,
		DirsCleared:         res.DirsCleared,
		DestinationsRemoved: res.ImagesRemoved,
		ChunkHashesRetained: res.ChunkHashesRetained,
		UnlockedImages:      res.UnlockedImages,
		NothingToClear:      res.NothingToClear,
		scoped:              scoped,
	}
	// T6.1 (the deferred shared fix, TODO: "writeReport-fail-after-commit
	// leaves stale 'aborted' Done line"): the transaction above
	// COMMITTED (or provably had nothing to do), so the Done summary is
	// computed NOW, before the report write - a report-write failure
	// must not leave the stale "aborted before any assignment was
	// cleared" line claiming the operation never happened. If the
	// stdout write itself fails, the line names what actually
	// happened: the commit stands, the stdout artifact failed.
	var body string
	if rep.NothingToClear {
		body = fmt.Sprintf("nothing to clear (set %s)", opts.setID)
	} else {
		body = fmt.Sprintf("cleared %d files, %d chunks, %d dirs; removed %d destinations",
			rep.FilesCleared, rep.ChunksCleared, rep.DirsCleared, rep.DestinationsRemoved)
	}
	summary = "clear: " + body
	if err := writeClearReport(cmd.OutOrStdout(), rep, opts.json); err != nil {
		summary = fmt.Sprintf(
			"clear: the clear transaction COMMITTED (%s), but writing the report to stdout failed: %v",
			body, err)
		return err
	}
	return nil
}

// writeClearReport renders the report: --json emits exactly one JSON
// document on stdout; the human form is the cleared-counts lines plus
// the destinations-removed and retained-hash lines (10.3: stdout
// carries the result only), with ONE extra "unlocked images: <ids>"
// line on scoped runs - a full clear's human bytes stay identical to
// the pre-feature output (DESIGN 4.6, AC-08). Formats are pinned by
// the golden tests in clear_test.go.
func writeClearReport(w io.Writer, rep clearReport, asJSON bool) error {
	if rep.UnlockedImages == nil {
		// The frozen key must always encode as a JSON array, never
		// null - even for a caller that never populated it.
		rep.UnlockedImages = []int64{}
	}
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	if _, err := fmt.Fprintf(w, "clear: set %s\n", rep.SetID); err != nil {
		return err
	}
	if rep.NothingToClear {
		_, err := fmt.Fprintln(w, "nothing to clear: no assignments present")
		return err
	}
	if _, err := fmt.Fprintf(w, "cleared: %d files, %d chunks, %d dirs\n",
		rep.FilesCleared, rep.ChunksCleared, rep.DirsCleared); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "destinations removed: %d\n", rep.DestinationsRemoved); err != nil {
		return err
	}
	// DESIGN 4.6: scoped runs add ONE line naming the released ids
	// (AC-40 cross-pins it against --json and stats_json); the full
	// clear prints no such line - byte-identical to the pre-feature
	// report.
	if rep.scoped {
		if _, err := fmt.Fprintf(w, "unlocked images: %s\n", joinInt64s(rep.UnlockedImages)); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w,
		"chunk hashes retained: %d (rows and hashes survive, FR-010; a same-profile re-plan reuses them)\n",
		rep.ChunkHashesRetained)
	return err
}
