package cli

// T5.2: the real `validate` command (DESIGN_SPEC 7.3, FR-022;
// IMPLEMENTATION_PLAN 4.6 T5.2). It replaces the T0.5 placeholder in
// commands.go and drives the internal/verify comparison core (see
// verify/validate.go for the category rules and every 7.3 reading).
//
// The command compares the SOURCE tree of one set against the CATALOG
// and categorises every catalog-ACTIVE record plus every uncovered
// source entry as ok / changed / corrupt / broken / missing / new.
// Counts and per-file finding lines land on stdout (human or --json);
// progress stays on stderr (FR-046).
//
// WIRING CONTRACTS:
//
//   - READ-ONLY catalog open (catalog.OpenReadOnly, URI mode=ro): NO
//     advisory flock at all (DESIGN_SPEC 2.1 Option B as amended
//     2026-10-18), so validate runs DURING a copy/build run over a WAL
//     snapshot - pinned by the concurrent-reader tests below (the
//     established blocked-copy and lock-holder patterns). validate
//     takes NO runs row and NO run_events (the read-only policy: the
//     stdout report is the record of the run, the verify-media
//     precedent).
//   - THE ROOT: 7.3 names no root argument ("compares a source
//     directory against the catalog for a chosen set"), so the target
//     root is the catalog's sets.source_root (db.SetSourceRoot) - the
//     catalog is what binds a set to its source tree, and inventing a
//     second, contradictory root input would let validate check a tree
//     the catalog never recorded. An unknown --set-id is a usage error
//     (ErrNoSuchSet -> exit 2, the typo'd-input class).
//   - CHANGE-DETECTION (3.5, the CIFS fix): the run resolves its ONE
//     fsmeta.Mode from that source root through the shared
//     resolveChangeDetection (auto | full | metadata), and the Mode
//     threads into the comparison - it gates exactly the 7.3
//     changed-vs-corrupt split through the single fsmeta.DiffFields. A
//     degradation warning (metadata via auto-detection) is emitted on
//     the bus like scan/copy/build's, with ONE documented carve-out:
//     there is no runs row to hang a run_events warn row on, so
//     warnChangeDetection is called with a nil db (bus emission only).
//   - No local flags at all: --set-id, --json, --strict,
//     --change-detection and --catalog are the inputs (7.3 defines no
//     others, and the root comes from the catalog).
//   - Exit semantics (7.3): exit 0 when clean, 10 when ANY anomaly is
//     found (machine-checkable from cron), --strict promoting that tail
//     to exit 1 (the family convention), 130 on SIGINT/SIGTERM. The
//     anomaly set is changed+corrupt+broken+missing; `new` is
//     INFORMATIONAL per 7.3's parenthetical and moves NO exit code in
//     either direction (verify.Result.Anomalies() owns that split - a
//     new-only run exits 0 and --strict has nothing to promote).
//   - Progress (9.1): a fresh Bus, the 9.2 renderer, one determinate
//     "validate" phase (totals known from the catalog), one ItemDone
//     per compared record, and exactly one Done summary line on EVERY
//     exit path through the deferred Finish/wait pair (runFailed /
//     runInterrupted wiring, progress_done.go). The run-level
//     HEARTBEAT (startHeartbeat, build.go's pattern) wraps the run for
//     the same reason build's does: validate hashes every file with no
//     intra-file events, so one 20 GiB record freezes the per-ItemDone
//     flow for tens of seconds - the heartbeat goroutine repaints
//     every HeartbeatInterval instead (registered after the Finish
//     defer, so a Tick can never trail the summary).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"isopress/internal/catalog"
	"isopress/internal/fsmeta"
	"isopress/internal/progress"
	"isopress/internal/report"
	"isopress/internal/verify"
)

// newValidateCommand builds the real validate command (replaces the
// T0.5 stub). It takes no positional arguments and no local flags:
// --catalog, --set-id, --json, --strict and --change-detection are the
// whole input surface, and the source root comes from the catalog.
func newValidateCommand(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "compare a source directory against the catalog (read-only)",
		Long: "Compare the source tree of the --set-id set against the SQLite catalog " +
			"(DESIGN_SPEC 7.3, FR-022) and categorise every catalog-active record: ok (content " +
			"hash matches - metadata drift with matching content is still ok), changed (metadata " +
			"differs AND the hash differs - a plausible modification a rescan captures), corrupt " +
			"(metadata matches AND the hash differs - bit rot), broken (stat succeeds but the " +
			"content cannot be read), missing (catalog record with no source entry), and new " +
			"(source entry with no catalog record - INFORMATIONAL, it never changes the exit " +
			"code). Symlinks compare by target (a retarget is changed; a dangling link with its " +
			"recorded target intact is ok); chunked files compare by their whole-file catalog " +
			"hash. The source root is the catalog's recorded source_root - validate takes no root " +
			"argument. The comparison set follows --change-detection (auto|full|metadata; the " +
			"CIFS-class source ignores synthesized dev/inode drift exactly like scan/copy/build). " +
			"The catalog is opened read-only with no advisory lock, so validate runs during a " +
			"copy or build run and never writes a byte. Counts and per-file finding lines land on " +
			"stdout (JSON with --json); progress renders on stderr.\n\n" +
			"Exit codes: 0 clean; 1 fatal unexpected error; 2 usage; 4 catalog error; 10 completed " +
			"with anomalies, enumerated above (--strict promotes this to 1); 130 interrupted. " +
			"validate never exits 3 or 5: it needs no xorriso and opens the catalog read-only. " +
			"JSON: --json prints the final report as exactly one JSON document on stdout; a run that " +
			"aborts before doing work prints no document at all; --json never changes the exit code.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runValidate(cmd.Context(), cmd, opts)
		},
	}
}

// runValidate is the validate service invocation (see the file doc for
// the wiring contracts). The named return feeds the deferred progress
// Done event's Failed signal (progress_done.go).
func runValidate(ctx context.Context, cmd *cobra.Command, opts *rootOptions) (runErr error) {
	// --- usage-class inputs (exit 2, 10.1) ---
	if opts.catalog == "" {
		return &report.UsageError{Command: cmd.CommandPath(), Msg: "--catalog is required (path to the SQLite catalog file)"}
	}
	if opts.setID == "" {
		return &report.UsageError{Command: cmd.CommandPath(), Msg: "--set-id is required (the set whose source is compared)"}
	}

	// READ-ONLY open: NO advisory flock (2.1 Option B) - this command
	// must work during a running copy/build over a WAL snapshot.
	db, err := catalog.Open(opts.catalog, catalog.OpenReadOnly)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck // best-effort; the run result dominates

	// The source root from the CATALOG (file doc): the set binds it,
	// and an unknown --set-id is the user's typo (exit 2).
	root, err := db.SetSourceRoot(ctx, opts.setID)
	if err != nil {
		if errors.Is(err, catalog.ErrNoSuchSet) {
			return &report.UsageError{Command: cmd.CommandPath(), Err: err}
		}
		return err
	}
	records, err := db.ActiveRecords(ctx, opts.setID)
	if err != nil {
		return err
	}

	// The run's ONE change-detection resolution (3.5): resolved from
	// the catalog's source root once per run, threaded into the
	// comparison where it gates changed-vs-corrupt (the CIFS fix).
	cdMode, err := resolveChangeDetection(cmd, opts, root)
	if err != nil {
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
	summary := fmt.Sprintf("validate: set %s aborted before any record was compared", opts.setID)
	defer func() {
		bus.Finish(progress.Done{Summary: summary, Failed: runFailed(runErr), Interrupted: runInterrupted(runErr)})
		wait()
	}()
	// The run-level heartbeat (HeartbeatInterval, build.go): one Tick
	// per interval, filling the per-FILE silent stretches of the hash
	// pass (validate hashes every file with no intra-file events - the
	// 20 GiB-file freeze the T5.1-era heartbeat lesson names). It
	// must be registered AFTER the Finish defer above: LIFO runs it
	// FIRST on exit, so a Tick can never trail the summary.
	stopHeartbeat := startHeartbeat(ctx, bus)
	defer stopHeartbeat()

	// The 3.5 once-per-run degradation warning, bus-only: this
	// command has NO runs row (read-only policy), so there is no
	// run_events row to write and warnChangeDetection gets a nil db -
	// the warning itself (source path, class, degraded set, retained
	// hash guarantee) still reaches stderr, and ONLY for an
	// auto-detected degradation (a forced flag stays silent, 3.5).
	warnChangeDetection(ctx, nil, 0, bus, root, cdMode)

	// The comparison (verify/validate.go owns the category rules).
	res, verr := verify.Validate(ctx, verify.ValidateInput{
		Root:    root,
		Records: sourceRecords(records),
		Mode:    cdMode,
	}, bus)

	return finalizeValidate(ctx, &summary, validateRun{
		cmd:      cmd,
		setID:    opts.setID,
		root:     root,
		cdMode:   cdMode,
		jsonFlag: opts.json,
		strict:   opts.strict,
		res:      res,
	}, verr)
}

// validateRun carries one run's inputs and outcome to the finalize
// step (the verifymedia runner shape, minus the per-item loop: validate
// performs exactly one comparison pass).
type validateRun struct {
	cmd      *cobra.Command
	setID    string
	root     string
	cdMode   fsmeta.Mode
	jsonFlag bool
	strict   bool
	res      verify.Result
}

// finalizeValidate stamps nothing but the report: it lands on stdout on
// EVERY exit path (FR-046: the result is the stdout artifact even on
// failure paths), then the command result is classified:
// interrupted (130) > terminal error > anomalies (10, or 1 with
// --strict) > nil (0) - the 7.3 exit semantics. summary is written back
// through the pointer so the deferred Done emission carries the final
// numbers (the scan.go convention).
func finalizeValidate(ctx context.Context, summary *string, r validateRun, runErr error) error {
	interrupted := ctx.Err() != nil
	anomalies := r.res.Anomalies()
	status := "ok"
	switch {
	case interrupted:
		status = "interrupted"
	case runErr != nil:
		status = "failed"
	case anomalies > 0:
		status = "partial"
	}

	rep := validateReport{
		SetID:         r.setID,
		Root:          r.root,
		Status:        status,
		ComparisonSet: r.cdMode.SetName(),
		Records:       r.res.Compared,
		OK:            r.res.OK,
		Changed:       r.res.Changed,
		Corrupt:       r.res.Corrupt,
		Broken:        r.res.Broken,
		Missing:       r.res.Missing,
		New:           r.res.New,
		Anomalies:     anomalies,
		Files:         make([]validateFileFinding, 0, len(r.res.Findings)),
	}
	for _, f := range r.res.Findings {
		rep.Files = append(rep.Files, validateFileFinding{Category: f.Category, Path: f.Path, Detail: f.Detail})
	}
	if err := writeValidateReport(r.cmd.OutOrStdout(), rep, r.jsonFlag); err != nil {
		runErr = errors.Join(runErr, err)
	}

	*summary = fmt.Sprintf(
		"validate: set %s: compared %d records, ok %d, changed %d, corrupt %d, broken %d, missing %d, new %d (anomalies %d, status %s)",
		r.setID, r.res.Compared, r.res.OK, r.res.Changed, r.res.Corrupt, r.res.Broken,
		r.res.Missing, r.res.New, anomalies, status)

	switch {
	case interrupted:
		return fmt.Errorf("%w: validate of set %s interrupted after %d compared records; the report above is partial",
			report.ErrInterrupted, r.setID, r.res.Compared)
	case runErr != nil:
		return runErr
	}
	if anomalies > 0 {
		if r.strict {
			return fmt.Errorf("validate completed with %d anomalous record(s); --strict promotes this to a fatal error", anomalies)
		}
		// The exit-10 carrier enumerates the ANOMALIES only (changed,
		// corrupt, broken, missing): `new` findings are informational
		// by 7.3's rule and never join the failure list, though they
		// stay fully visible in the stdout report above.
		var errs []error
		for _, f := range r.res.Findings {
			if f.Category == verify.CatNew {
				continue
			}
			if f.Detail != "" {
				errs = append(errs, fmt.Errorf("%s: %s (%s)", f.Category, f.Path, f.Detail))
				continue
			}
			errs = append(errs, fmt.Errorf("%s: %s", f.Category, f.Path))
		}
		return &report.ItemFailures{Failures: errs}
	}
	return nil
}

// sourceRecords maps the catalog read onto the verify core's record
// shape, field for field (the scanStoreTx pure-mapping pattern: the
// cli layer is the one place allowed to know both sides, so
// internal/verify never imports internal/catalog).
func sourceRecords(records []catalog.ValidateRecord) []verify.SourceRecord {
	out := make([]verify.SourceRecord, 0, len(records))
	for _, rec := range records {
		out = append(out, verify.SourceRecord{
			FileID:     rec.FileID,
			RelPath:    rec.RelPath,
			EntryKind:  rec.EntryKind,
			Size:       rec.Size,
			MtimeNS:    rec.MtimeNS,
			Dev:        rec.Dev,
			Inode:      rec.Inode,
			LinkTarget: rec.LinkTarget,
			Hash:       rec.Hash,
		})
	}
	return out
}

// --- the report model (human and --json; the key set is FROZEN) ----------------

// validateFileFinding is one non-ok categorisation in the report. The
// JSON key set is a deliberate frozen schema (the scan/plan/copy/build/
// verify-media report precedent): new keys are a conscious schema
// change with the pinning tests updated in the same commit. Detail is
// omitempty and joins the set exactly when the category renders a
// parenthetical tail (missing carries none).
type validateFileFinding struct {
	Category string `json:"category"` // changed | corrupt | broken | missing | new
	Path     string `json:"path"`
	Detail   string `json:"detail,omitempty"`
}

// validateReport is the end-of-run report (human lines and --json are
// two presentations of the same numbers, FR-046: stdout only). The six
// counts are 7.3's categories; Anomalies is changed+corrupt+broken+
// missing (verify.Result.Anomalies - `new` excluded by design); Records
// is the catalog-active record count the comparison covered; the five
// record categories therefore sum to Records and New counts source
// entries beyond them.
type validateReport struct {
	SetID  string `json:"set_id"`
	Root   string `json:"root"`
	Status string `json:"status"` // ok | partial | failed | interrupted (runs.status spelling)
	// ComparisonSet names the resolved 3.5 comparison set that gated
	// changed-vs-corrupt ("full" | "metadata"): material to
	// interpreting any corrupt/changed line, so it is report data, not
	// just a run-time decision.
	ComparisonSet string `json:"comparison_set"`
	Records       int    `json:"records"`
	OK            int    `json:"ok"`
	Changed       int    `json:"changed"`
	Corrupt       int    `json:"corrupt"`
	Broken        int    `json:"broken"`
	Missing       int    `json:"missing"`
	New           int    `json:"new"`
	Anomalies     int    `json:"anomalies"`
	// Files enumerates every non-ok finding (changed, corrupt, broken,
	// missing, new) in record order then walk order. Ok records are
	// COUNTED, never listed - the scan/verify family reports findings,
	// not successes, and a large clean set would otherwise print one
	// line per file into what is supposed to be a cron-readable
	// summary. Never nil (always at least []), so the frozen top-level
	// key set is byte-identical for clean and dirty runs alike.
	Files []validateFileFinding `json:"files"`
}

// writeValidateReport renders the report: --json emits exactly one
// JSON document; the human form is the set identity line, the
// root/comparison-set line, the category counts line, one
// category-prefixed line per finding, and the totals line (the
// family's plan/scan/copy/build/verify-media shape).
func writeValidateReport(w io.Writer, rep validateReport, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	if _, err := fmt.Fprintf(w, "validate: set %s\n", rep.SetID); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "root: %s (comparison set: %s)\n", rep.Root, rep.ComparisonSet); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w,
		"ok %d, changed %d, corrupt %d, broken %d, missing %d, new %d, anomalies %d\n",
		rep.OK, rep.Changed, rep.Corrupt, rep.Broken, rep.Missing, rep.New, rep.Anomalies); err != nil {
		return err
	}
	for _, f := range rep.Files {
		if f.Detail != "" {
			if _, err := fmt.Fprintf(w, "%s: %s (%s)\n", f.Category, f.Path, f.Detail); err != nil {
				return err
			}
			continue
		}
		if _, err := fmt.Fprintf(w, "%s: %s\n", f.Category, f.Path); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w,
		"totals: compared %d records, status %s (ok %d, changed %d, corrupt %d, broken %d, missing %d, new %d)\n",
		rep.Records, rep.Status, rep.OK, rep.Changed, rep.Corrupt, rep.Broken, rep.Missing, rep.New)
	return err
}
