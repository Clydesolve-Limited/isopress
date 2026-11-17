package cli

// T2.3: the shared plan/report pipeline and the single report renderer
// (DESIGN_SPEC 4.3 media count, 4.4 fill strategy D-05 waste
// quantification, 4.5 single-transaction assignment, 6.5 profile
// margin echo D-06, 9.3 stream separation; IMPLEMENTATION_PLAN T2.3).
//
// The `plan` command (plan.go) and the `report` command (reportcmd.go)
// run the IDENTICAL pipeline: resolve the capacity profile, load the
// set's planning state from the catalog, and run plan.Balance. The ONLY
// difference is the write step, and that difference is mode-gated in
// runPlanReport below: plan mode persists the computed plan in ONE
// transaction via the supplied write hook; report mode NEVER calls the
// hook, so a report run performs ZERO writes (DESIGN_SPEC 4.3: "The
// `report` command runs the identical pipeline read-only (no
// transaction)"; FR-009). The report command wires a hook that ERRORS
// if it is ever invoked (refuseWrites), and the T2.3 test suite injects
// a hook that fails the test if invoked - if the mode gate were ever
// removed, both would trip loudly. The byte-identical catalog
// comparison in reportcmd_test.go proves the property end to end.
//
// Catalog open modes (DESIGN_SPEC 2.1 as amended 2026-10-18):
//
//   - plan is MUTATING: catalog.OpenReadWrite takes the exclusive
//     advisory flock for the whole run; contention surfaces as
//     *catalog.CatalogBusy and report.Classify maps it to exit 5.
//     Unlike scan, plan does NOT auto-create a missing catalog (there
//     is nothing to plan in an empty database; creation is scan's
//     PM-ratified job) - a missing path is a KindOpen CatalogError
//     (exit 4) whose message already says "create one first".
//   - report is READ-ONLY: catalog.OpenReadOnly opens mode=ro via the
//     URI flag and takes NO advisory lock at all, so it runs
//     concurrently with any mutating run over a WAL snapshot (the
//     Option B amendment; exit 5 can never come from report).
//
// Set selection (judgment call, documented): scan derives its default
// set id from the source ROOT plus the ingest date (6.4), but plan and
// report take no ROOT argument, so that derivation is not available.
// The catalog also exposes no set-enumeration read (finding recorded
// with T2.3), so both commands REQUIRE --set-id and verify the set was
// actually scanned (PriorScanRuns > 0); a typo'd or never-scanned set
// is a usage error (exit 2) instead of a silent "nothing to plan".
//
// Runs provenance (finding recorded with T2.3): DESIGN_SPEC 2.3's runs
// table anticipates kind 'plan', and BeginPlanTx accepts a run id for
// provenance, but the catalog layer exposes no plan-run starter and no
// set-root read (runs.root is NOT NULL and means "source root or
// destination root"). Rather than issuing SQL outside the catalog
// layer (DESIGN_SPEC 1.2 forbids it), the transaction runs with runID
// 0 and no runs row is inserted; a future catalog helper (StartPlanRun
// plus a set read) should replace that without touching this file's
// pipeline shape.
//
// Stream separation (FR-046, 10.3): the rendered report is the command
// RESULT and goes to stdout only; the progress phase and the one final
// summary line go to stderr through the T1.1 bus (9.3).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"isopress/internal/catalog"
	"isopress/internal/plan"
	"isopress/internal/progress"
	"isopress/internal/report"
	"isopress/internal/units"
)

// planReportMode distinguishes the two consumers of the shared
// pipeline. The mode is carried into the rendered output ("mode" in
// JSON, the heading prefix in human form) so a plan report (post-write
// actual) and a report projection are never confused downstream.
type planReportMode string

const (
	// modePlan: the plan command - the assignment is WRITTEN (one
	// transaction, DESIGN_SPEC 4.5) before the report renders.
	modePlan planReportMode = "plan"
	// modeReport: the report command - the identical pipeline runs
	// strictly read-only and the output is a PROJECTION (FR-009).
	modeReport planReportMode = "report"
)

// planOptions holds the plan-local flag values (DESIGN_SPEC 1.1 fixes
// only the global flags; 4.1 names --media-profile and requires a
// user-stated destination size per FR-006, but fixes no flag name for
// it - "--size" is this implementation's documented choice, matching
// the terse style of the existing global flags).
type planOptions struct {
	size         string // --size: destination nominal size ("25", "25GB", "25Gi"; bare = GB, 4.1)
	mediaProfile string // --media-profile: capacity profile name (4.1, D-06)
}

// addPlanFlags registers the plan-local flags on cmd. Both `plan` and
// `report` register the identical pair: a report projects "media count
// if planned now" (FR-009), which needs the same destination size and
// profile a plan would use.
func addPlanFlags(cmd *cobra.Command, po *planOptions) {
	cmd.Flags().StringVar(&po.size, "size", "",
		"destination nominal size (e.g. 25, 25GB, 25Gi; a bare number means decimal GB, DESIGN_SPEC 4.1)")
	cmd.Flags().StringVar(&po.mediaProfile, "media-profile", units.DefaultProfile,
		"media capacity profile: bd-r-sl (25 GB), bd-r-dl (50 GB), or disk (98% of nominal)")
}

// resolvePlanOptions validates the plan-flag inputs and resolves the
// D-06 capacity profile. Every failure is a usage-class error (exit 2,
// DESIGN_SPEC 10.1): a missing/garbled --size or an unknown/mismatched
// --media-profile is an input mistake, not a runtime fault. The margin
// is applied exactly once, here, in units.ResolveProfile; everything
// downstream sees the resolved Profile only (4.1).
func resolvePlanOptions(cmd *cobra.Command, po planOptions) (units.Profile, error) {
	if po.size == "" {
		return units.Profile{}, &report.UsageError{
			Command: cmd.CommandPath(),
			Msg:     "--size is required (destination nominal size, e.g. 25, 25GB, 25Gi)",
		}
	}
	nominal, err := units.ParseSize(po.size)
	if err != nil {
		return units.Profile{}, &report.UsageError{Command: cmd.CommandPath(), Err: err}
	}
	profile, err := units.ResolveProfile(po.mediaProfile, nominal)
	if err != nil {
		return units.Profile{}, &report.UsageError{Command: cmd.CommandPath(), Err: err}
	}
	return profile, nil
}

// planWriteHook persists a computed plan. The plan command supplies
// the real single-transaction write (writeAssignments below); report
// mode never calls its hook (mode gate in runPlanReport), and the
// report command wires refuseWrites as a tripwire on top of the gate.
type planWriteHook func(ctx context.Context, db *catalog.DB, setID string, p *plan.Plan) error

// writeAssignments is the plan command's hook: Plan.Write runs the
// whole write side in ONE BeginPlanTx transaction (DESIGN_SPEC 4.5);
// any failure rolls everything back, so a failed plan leaves the
// catalog bit-identical (FR-010). A plan with no work opens NO
// transaction at all (T2.2 no-op contract).
func writeAssignments(ctx context.Context, db *catalog.DB, setID string, p *plan.Plan) error {
	return p.Write(ctx, planTxFactory(db, setID))
}

// errReportWrite is the tripwire of the report command's write hook.
var errReportWrite = fmt.Errorf("internal: the report command must never write assignments " +
	"(DESIGN_SPEC 4.3 read-only pipeline, FR-009)")

// refuseWrites is the report command's hook: it can only be reached if
// the mode gate in runPlanReport were removed or inverted, which is
// exactly the regression it exists to fail loudly on.
func refuseWrites(context.Context, *catalog.DB, string, *plan.Plan) error {
	return errReportWrite
}

// runPlanReport is the shared service invocation of `plan` and
// `report` (see the file doc for the wiring contracts). The write hook
// is invoked ONLY in modePlan; in modeReport the pipeline is strictly
// read-only: no transaction is opened, no row is written, and the
// rendered output is a projection.
func runPlanReport(ctx context.Context, cmd *cobra.Command, opts *rootOptions,
	po planOptions, mode planReportMode, write planWriteHook) (runErr error) {

	profile, err := resolvePlanOptions(cmd, po)
	if err != nil {
		return err
	}
	if opts.catalog == "" {
		return &report.UsageError{
			Command: cmd.CommandPath(),
			Msg:     "--catalog is required (path to the SQLite catalog file)",
		}
	}
	if opts.setID == "" {
		// See the file doc: scan's date-derived default needs the
		// source root, which plan/report do not take, and the catalog
		// exposes no set enumeration - so --set-id is mandatory here.
		return &report.UsageError{
			Command: cmd.CommandPath(),
			Msg:     "--set-id is required (plan and report operate on a set created by scan)",
		}
	}

	// Catalog open (DESIGN_SPEC 2.1): MUTATING+flock for plan (exit 5
	// on contention via Classify), READ-ONLY no-flock for report (it
	// can never fail with the busy class, and runs during a writer).
	openMode := catalog.OpenReadWrite
	if mode == modeReport {
		openMode = catalog.OpenReadOnly
	}
	db, err := catalog.Open(opts.catalog, openMode)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck // best-effort; the pipeline result dominates

	// Set existence: the set must have been scanned, or there is
	// nothing to plan/report. PriorScanRuns counts ingest/rescan runs
	// (scanstore.go); zero means a typo'd set id or a scan that never
	// reached its run row - a usage-class input problem (exit 2).
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

	// Fresh progress bus per operation (T1.1 contract); renderer chosen
	// once here per the 9.2 matrix. The balance is metadata-only and
	// runs in memory (4.3, 12.5), so the whole operation is one short
	// phase - "plan"/"report" for the interactive renderer, and the
	// one final summary line on stderr for cron logs (9.3).
	bus := progress.NewBus()
	wait := progress.Start(progress.New(progress.Options{
		Quiet:  opts.quiet,
		Stderr: cmd.ErrOrStderr(),
	}), bus)
	phase := string(mode)
	summary := fmt.Sprintf("%s: aborted before a result was computed", phase)
	defer func() {
		// Failed / Interrupted mark the abort path so the interactive
		// renderer CLEARS its bar instead of rendering a misleading
		// 100% (progress_done.go). plan/report have NO explicit
		// interrupted-status decision of their own (fast, metadata-only,
		// in-memory; they map nothing to report.ErrInterrupted): a
		// SIGINT mid-balance surfaces as the cancellation-derived
		// error of the in-flight plan.Balance / catalog call, which
		// the SHARED runInterrupted predicate classifies - wired
		// consistently so an interrupted plan/report never renders a
		// completed bar with neither abort flag set. runFailed (its
		// complement) covers every other error.
		bus.Finish(progress.Done{Summary: summary, Failed: runFailed(runErr), Interrupted: runInterrupted(runErr)})
		wait()
	}()
	bus.Emit(progress.Phase{Name: phase})

	// --- the shared pipeline: load + balance (compute-only, 4.3) ---
	balanced, err := plan.Balance(ctx, planCatalogStore{db: db, setID: opts.setID}, profile)
	if err != nil {
		// ErrPartialChunkReplan (4.3/4.6) is the ONE Balance error
		// that is usage-class rather than fatal-class: a
		// partially-assigned chunk set whose stored chunks cannot be
		// reused - a changed chunk profile, a whole-file fit, stored
		// rows that do not verify against the current decomposition,
		// or a secondary the hard-link family rule excludes (that leg
		// carries only the clear-whole-set remedy, no profile choice
		// can place a secondary row) - is a user-correctable
		// state/flag conflict, so plan AND report wrap it at this
		// shared seam into a report.UsageError -> exit 2 under 10.1's
		// "unsatisfiable combinations" - a deliberate deviation from the
		// ErrItemTooLarge fatal precedent, documented in both help
		// texts. Balance raised it before any transaction opened, so
		// the catalog is untouched either way.
		if errors.Is(err, plan.ErrPartialChunkReplan) {
			return &report.UsageError{Command: cmd.CommandPath(), Err: err}
		}
		return err
	}

	// --- the one mode-gated difference: persist (plan) or not (report) ---
	if mode == modePlan {
		if err := write(ctx, db, opts.setID, &balanced); err != nil {
			return err
		}
	}

	// --- the shared renderer: result on stdout only (FR-046) ---
	rep := buildPlanReport(mode, opts.setID, profile, balanced.Result)
	// T6.1 (the deferred shared fix, TODO: writeReport-fail-after-commit
	// leaves a stale "aborted" Done line): the summary is computed NOW,
	// BEFORE the report write - in plan mode the assignment transaction
	// above already COMMITTED, and in report mode the projection is
	// fully computed, so a report-write failure must not leave the
	// stale "aborted before a result was computed" line claiming the
	// operation never happened. If the stdout write itself fails, the
	// line names what actually happened.
	var body string
	if rep.NothingToPlan {
		body = fmt.Sprintf("nothing to plan (media %d)", rep.MediaCount)
	} else {
		body = fmt.Sprintf("media %d (%d new); files %d, chunks %d, symlinks %d, dirs %d",
			rep.MediaCount, rep.NewDestinations,
			rep.AssignedFiles, rep.AssignedChunks, rep.AssignedSymlinks, rep.AssignedDirs)
	}
	summary = phase + ": " + body
	if err := writePlanReport(cmd.OutOrStdout(), rep, opts.json); err != nil {
		if mode == modePlan {
			summary = fmt.Sprintf(
				"plan: the assignment transaction COMMITTED (%s), but writing the report to stdout failed: %v",
				body, err)
		} else {
			summary = fmt.Sprintf(
				"report: the projection was computed read-only (%s), but writing the report to stdout failed: %v",
				body, err)
		}
		return err
	}
	return nil
}

// --- the report model (human and --json are two presentations) ----------------

// destFillReport is one destination's line of the plan report. The
// JSON form is the frozen per-destination schema (planreport_test.go
// pins the key set); the human form is the "image N: ..." line.
//
// Items counts PAYLOAD assignments (whole files, symlinks, and chunks
// combined): plan.Result carries per-destination counts only in that
// combined form (DestFill.ItemCount), so a per-destination
// files-vs-chunks split is not renderable without changing
// internal/plan - recorded as a T2.3 finding, deliberately not worked
// around here (no SQL outside the catalog layer, DESIGN_SPEC 1.2).
type destFillReport struct {
	Image        int64  `json:"image"`
	MediaProfile string `json:"media_profile"`
	NominalSize  int64  `json:"nominal_size"`
	UsableSize   int64  `json:"usable_size"`
	UsedBytes    int64  `json:"used_bytes"`
	FillPercent  int64  `json:"fill_percent"`
	Items        int    `json:"items"`
	Dirs         int    `json:"dirs"`
	Locked       bool   `json:"locked"`
}

// planReport is the end-of-run report model of BOTH plan and report
// (FR-008/FR-009 media count, D-05 residual visibility, 4.2 re-hash
// statement, D-06 margin echo).
//
// The --json key set is FROZEN for both commands (19 top-level keys,
// 9 per destination; planreport_test.go pins them): both share this
// one struct, so the key sets cannot drift. Changing a key is a
// deliberate schema change, exactly like the scan report's frozen set
// in scan_hardening_test.go - the "mode" key ("plan" | "report") is
// the only value that differs between the two commands.
type planReport struct {
	Mode  string `json:"mode"` // "plan" (written) | "report" (projection)
	SetID string `json:"set_id"`
	// The D-06 margin echo (6.5): the CURRENT run's profile figures,
	// nominal next to usable so the margin is never silent.
	MediaProfile        string `json:"media_profile"`
	NominalSize         int64  `json:"nominal_size"`
	UsableSize          int64  `json:"usable_size"`
	MarginPercent       int64  `json:"margin_percent"`
	MediaCount          int    `json:"media_count"`      // FR-008/FR-009: destinations after placement
	NewDestinations     int    `json:"new_destinations"` // destinations THIS run would create
	AssignedFiles       int    `json:"assigned_files"`
	AssignedChunks      int    `json:"assigned_chunks"`
	AssignedSymlinks    int    `json:"assigned_symlinks"`
	AssignedDirs        int    `json:"assigned_dirs"`
	SecondariesExcluded int    `json:"secondaries_excluded"`
	// The D-05 accept-waste cost (4.4): bytes that can never be used
	// on LOCKED destinations, and how many locked destinations hold
	// such a residual.
	ResidualBytes  int64 `json:"residual_bytes"`
	ResidualImages int   `json:"residual_images"`
	// The 4.2 profile-change cost: files re-chunked under the current
	// effective size, and their already-written chunks whose stored
	// hashes are discarded - those byte ranges are re-read and
	// re-hashed at their next write.
	RechunkedFiles      int `json:"rechunked_files"`
	RehashedAtNextWrite int `json:"rehashed_at_next_write"`
	// NothingToPlan marks the zero-unassigned no-op: exit 0 with a
	// clear "nothing to plan" report (T2.2's no-op contract surfaced
	// through the command).
	NothingToPlan bool             `json:"nothing_to_plan"`
	Destinations  []destFillReport `json:"destinations"` // ordered by ascending image_id
}

// buildPlanReport folds a computed plan.Result into the render model.
// It is pure: plan and report produce the same numbers for the same
// inputs, which is what lets one renderer serve both (4.3).
func buildPlanReport(mode planReportMode, setID string, profile units.Profile, res plan.Result) planReport {
	rep := planReport{
		Mode:                string(mode),
		SetID:               setID,
		MediaProfile:        profile.Name,
		NominalSize:         profile.NominalBytes,
		UsableSize:          profile.UsableBytes,
		MarginPercent:       marginPercent(profile),
		MediaCount:          len(res.Destinations),
		NewDestinations:     res.NewDestinations,
		AssignedFiles:       res.FilesAssigned,
		AssignedChunks:      res.ChunksAssigned,
		AssignedSymlinks:    res.SymlinksAssigned,
		AssignedDirs:        res.DirsAssigned,
		SecondariesExcluded: res.SecondariesExcluded,
		ResidualBytes:       res.WastedResidualBytes,
		ResidualImages:      res.WastedResidualImages,
		RechunkedFiles:      res.RechunkedFiles,
		RehashedAtNextWrite: res.RehashedAtNextWrite,
		NothingToPlan: res.FilesAssigned == 0 && res.ChunksAssigned == 0 &&
			res.SymlinksAssigned == 0 && res.DirsAssigned == 0,
		Destinations: make([]destFillReport, 0, len(res.Destinations)),
	}
	for _, d := range res.Destinations {
		var pct int64
		if d.UsableSize > 0 {
			// Truncated integer percent (same rule the plain renderer
			// uses for its "(18%)" suffix, DESIGN_SPEC 9.3).
			pct = d.UsedBytes * 100 / d.UsableSize
		}
		rep.Destinations = append(rep.Destinations, destFillReport{
			Image:        d.ImageID,
			MediaProfile: d.MediaProfile,
			NominalSize:  d.NominalSize,
			UsableSize:   d.UsableSize,
			UsedBytes:    d.UsedBytes,
			FillPercent:  pct,
			Items:        d.ItemCount,
			Dirs:         d.DirCount,
			Locked:       d.Locked,
		})
	}
	return rep
}

// marginPercent is the D-06 margin as an integer percentage of
// nominal, for the 6.5 echo ("nominal ... usable ..., margin N%").
// Exact for the fixed profiles (25/23 -> 8%, 50/46 -> 8%) and for
// disk's 98% (-> 2%); profile resolution guarantees nominal > 0.
func marginPercent(p units.Profile) int64 {
	return (p.NominalBytes - p.UsableBytes) * 100 / p.NominalBytes
}

// writePlanReport renders the report: --json emits exactly one JSON
// document on stdout; the human form is the header with the D-06
// margin echo, the FR-008/009 media count, one fill line per
// destination (the D-05 fill percentage - payload / usable_size, the
// honest disc usage under the 2026-10-20 capacity model - with
// "[locked]" marking pre-existing destinations) plus a SEPARATE line
// showing the reserved overhead (units.FixedImageOverhead + 512 per
// entry, the canonical estimator's overhead term) and the true free
// space that will remain on the built image; the 4.4 residual summary
// line; and the assignment / nothing-to-plan line plus the 4.2 re-hash
// statements. Formats are pinned by golden tests in planreport_test.go;
// byte rendering goes through units.FormatBytes (9.3). The reserved
// overhead is deliberately RENDERER-DERIVED (Items+Dirs+usable_size are
// already in the frozen per-destination JSON schema) rather than a new
// JSON key: the frozen key set of both commands stays untouched.
func writePlanReport(w io.Writer, rep planReport, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	heading := string(rep.Mode)
	if rep.Mode == string(modeReport) {
		// The projection is never mistaken for a written plan (FR-009:
		// report "without writing any media").
		heading = "report (projection)"
	}
	if _, err := fmt.Fprintf(w, "%s: set %s (profile %s: nominal %s, usable %s, margin %d%%)\n",
		heading, rep.SetID, rep.MediaProfile,
		units.FormatBytes(rep.NominalSize), units.FormatBytes(rep.UsableSize),
		rep.MarginPercent); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "media: %d destinations total (%d new, %d pre-existing)\n",
		rep.MediaCount, rep.NewDestinations, rep.MediaCount-rep.NewDestinations); err != nil {
		return err
	}
	for _, d := range rep.Destinations {
		locked := ""
		if d.Locked {
			locked = " [locked]"
		}
		// Headline fill = payload / usable_size: the honest data usage
		// of the disc (the 2026-10-20 capacity model caps payload at
		// usable - overhead, so this percentage no longer implies the
		// brim-packed, unbuildable 100% of the old planner).
		if _, err := fmt.Fprintf(w, "image %d: %d items, %d dirs, %s / %s (%d%%)%s\n",
			d.Image, d.Items, d.Dirs,
			units.FormatBytes(d.UsedBytes), units.FormatBytes(d.UsableSize),
			d.FillPercent, locked); err != nil {
			return err
		}
		// The SEPARATE reserved-overhead line (4.3 capacity
		// accounting): the fixed ISO/system-area overhead plus the
		// 512-byte entry charge per payload item AND per empty dir -
		// the exact overhead the builder's 6.3 step-5 pre-check
		// requires - next to the true free space that will remain on
		// the built image (clamped at zero for pre-amendment locked
		// plans that overfill their budget; the builder, not this
		// line, owns refusing those).
		overhead := units.FixedImageOverhead + units.ImageEntryOverhead*int64(d.Items+d.Dirs)
		free := d.UsableSize - d.UsedBytes - overhead
		if free < 0 {
			free = 0
		}
		if _, err := fmt.Fprintf(w, "  reserved overhead: %s (fixed %s + %d entries), free after build: %s\n",
			units.FormatBytes(overhead), units.FormatBytes(units.FixedImageOverhead),
			int64(d.Items+d.Dirs), units.FormatBytes(free)); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "residual space on locked destinations: %s across %d images\n",
		units.FormatBytes(rep.ResidualBytes), rep.ResidualImages); err != nil {
		return err
	}
	if rep.NothingToPlan {
		if _, err := fmt.Fprintln(w, "nothing to plan: no unassigned items"); err != nil {
			return err
		}
	} else if _, err := fmt.Fprintf(w,
		"assigned this run: %d files, %d chunks, %d symlinks, %d dirs (hard-link secondaries excluded: %d)\n",
		rep.AssignedFiles, rep.AssignedChunks, rep.AssignedSymlinks,
		rep.AssignedDirs, rep.SecondariesExcluded); err != nil {
		return err
	}
	// The 4.2 statements: how many files were re-chunked under the
	// current effective size, and how many of their ALREADY-WRITTEN
	// chunks lose their stored hashes - those ranges are re-read and
	// re-hashed at their next write. Both lines appear only when the
	// numbers are non-zero (a clean plan states neither).
	if rep.RechunkedFiles > 0 {
		if _, err := fmt.Fprintf(w, "re-chunked files: %d\n", rep.RechunkedFiles); err != nil {
			return err
		}
	}
	if rep.RehashedAtNextWrite > 0 {
		if _, err := fmt.Fprintf(w,
			"already-written chunks re-hashed at next write: %d\n", rep.RehashedAtNextWrite); err != nil {
			return err
		}
	}
	return nil
}

// --- catalog adapters (pure type mapping; no SQL here, DESIGN_SPEC 1.2) -------

// planCatalogStore adapts *catalog.DB's NO-transaction read surface to
// the plan.Store interface. Because every method runs outside any
// transaction, the same store serves both the mutating plan command
// and the read-only report command over a WAL snapshot (4.3).
type planCatalogStore struct {
	db    *catalog.DB
	setID string
}

func (s planCatalogStore) ActiveFiles(ctx context.Context) ([]plan.ActiveFile, error) {
	rows, err := s.db.ListActiveForPlan(ctx, s.setID)
	if err != nil {
		return nil, err
	}
	out := make([]plan.ActiveFile, len(rows))
	for i, r := range rows {
		out[i] = plan.ActiveFile{
			FileID: r.FileID, RelPath: r.RelPath, EntryKind: r.EntryKind,
			Size: r.Size, ChunkSize: r.ChunkSize,
			Dev: r.Dev, Inode: r.Inode, Nlink: r.Nlink, ImageID: r.ImageID,
		}
	}
	return out, nil
}

func (s planCatalogStore) ChunkStates(ctx context.Context) ([]plan.StoredChunk, error) {
	rows, err := s.db.ChunkStates(ctx, s.setID)
	if err != nil {
		return nil, err
	}
	out := make([]plan.StoredChunk, len(rows))
	for i, r := range rows {
		out[i] = plan.StoredChunk{
			FileID: r.FileID, ChunkSize: r.ChunkSize, Index: r.Index,
			Offset: r.Offset, Length: r.Length,
			ImageID: r.ImageID, HashPresent: r.HashPresent,
		}
	}
	return out, nil
}

func (s planCatalogStore) UnassignedDirs(ctx context.Context) ([]plan.UnassignedDir, error) {
	dirs, err := s.db.ListActiveUnassignedDirs(ctx, s.setID)
	if err != nil {
		return nil, err
	}
	out := make([]plan.UnassignedDir, len(dirs))
	for i, d := range dirs {
		out[i] = plan.UnassignedDir{DirID: d.DirID, RelPath: d.RelPath}
	}
	return out, nil
}

func (s planCatalogStore) Destinations(ctx context.Context) ([]plan.DestinationState, error) {
	rows, err := s.db.Destinations(ctx, s.setID)
	if err != nil {
		return nil, err
	}
	out := make([]plan.DestinationState, len(rows))
	for i, d := range rows {
		out[i] = plan.DestinationState{
			ImageID: d.ImageID, NominalSize: d.NominalSize, UsableSize: d.UsableSize,
			MediaProfile: d.MediaProfile, UsedBytes: d.UsedBytes,
			ItemCount: d.ItemCount, DirCount: d.DirCount,
		}
	}
	return out, nil
}

// planCatalogAssignTx adapts one catalog.PlanTx to the plan.AssignStore
// interface, field for field (the ingest scanStoreTx pattern): the plan
// service declares the interface and its own shapes so internal/plan
// never imports the catalog (DESIGN_SPEC 1.2), and this wiring layer -
// the one place allowed to know both sides - translates.
type planCatalogAssignTx struct{ tx *catalog.PlanTx }

func (a planCatalogAssignTx) InsertImage(ctx context.Context, img plan.ImageSpec) error {
	return a.tx.InsertImage(ctx, catalog.ImageSpec{
		ImageID: img.ImageID, NominalSize: img.NominalSize,
		UsableSize: img.UsableSize, MediaProfile: img.MediaProfile,
	})
}

func (a planCatalogAssignTx) EnsureChunkRows(ctx context.Context, fileID int64, specs []plan.ChunkSpec) error {
	return a.tx.EnsureChunkRows(ctx, fileID, toCatalogChunkSpecs(specs))
}

func (a planCatalogAssignTx) ReplaceAllChunks(ctx context.Context, fileID int64, specs []plan.ChunkSpec) (plan.RechunkOutcome, error) {
	res, err := a.tx.ReplaceAllChunks(ctx, fileID, toCatalogChunkSpecs(specs))
	if err != nil {
		return plan.RechunkOutcome{}, err
	}
	return plan.RechunkOutcome{
		RemovedUnwritten: res.RemovedUnwritten,
		RemovedWritten:   res.RemovedWritten,
	}, nil
}

func (a planCatalogAssignTx) DropChunkState(ctx context.Context, fileID int64) (int, error) {
	return a.tx.DropChunkState(ctx, fileID)
}

func (a planCatalogAssignTx) StoreChunkSize(ctx context.Context, fileID, chunkSize int64, chunkCount int) error {
	return a.tx.StoreChunkSize(ctx, fileID, chunkSize, chunkCount)
}

func (a planCatalogAssignTx) AssignFile(ctx context.Context, fileID, imageID int64) error {
	return a.tx.AssignFile(ctx, fileID, imageID)
}

func (a planCatalogAssignTx) AssignChunk(ctx context.Context, fileID int64, chunkIndex int, imageID int64) error {
	return a.tx.AssignChunk(ctx, fileID, chunkIndex, imageID)
}

func (a planCatalogAssignTx) AssignDir(ctx context.Context, dirID, imageID int64) error {
	return a.tx.AssignDir(ctx, dirID, imageID)
}

// toCatalogChunkSpecs maps the plan decomposition shape onto the
// catalog one (structurally identical).
func toCatalogChunkSpecs(specs []plan.ChunkSpec) []catalog.ChunkSpec {
	out := make([]catalog.ChunkSpec, len(specs))
	for i, s := range specs {
		out[i] = catalog.ChunkSpec{Index: s.Index, Offset: s.Offset, Length: s.Length}
	}
	return out
}

// planTxFactory binds BeginPlanTx to the plan.AssignTx shape: ONE
// BEGIN IMMEDIATE transaction for the whole assignment write
// (DESIGN_SPEC 4.5, the scanTxFactory pattern). runID is 0 - see the
// file doc's runs-provenance finding.
func planTxFactory(db *catalog.DB, setID string) plan.AssignTxFactory {
	return func(ctx context.Context) (*plan.AssignTx, error) {
		tx, err := db.BeginPlanTx(ctx, 0, setID)
		if err != nil {
			return nil, err
		}
		return &plan.AssignTx{
			AssignStore: planCatalogAssignTx{tx: tx},
			Commit:      tx.Commit,
			Rollback:    func() error { return tx.Rollback() },
		}, nil
	}
}
