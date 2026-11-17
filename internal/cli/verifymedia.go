package cli

// T5.1: the real `verify-media` command (DESIGN_SPEC 7.2, FR-021;
// IMPLEMENTATION_PLAN 4.6 T5.1). It replaces the T0.5 placeholder in
// commands.go.
//
// The command re-reads each selected image and compares it - hash
// first, mount free - against the catalog's finalized content list,
// through the ONE internal/verify comparison for both source modes:
// an ISO image re-reads via ONE xorriso osirrox extraction to a unique
// scratch under --work-dir (7.2 free-space Statfs pre-check first), a
// copy tree reads DIRECTLY with no xorriso and no temp. Per-image
// report lines and totals land on stdout (human or --json); progress
// stays on stderr (FR-046).
//
// WIRING CONTRACTS:
//
//   - READ-ONLY catalog open (catalog.OpenReadOnly, URI mode=ro): NO
//     advisory flock at all (DESIGN_SPEC 2.1 Option B as amended
//     2026-10-18), so verify-media runs DURING a copy/build run over
//     a WAL snapshot - pinned by the concurrent-reader tests below
//     (the established lock-holder and blocked-copy patterns).
//   - THE STAMPING CONNECTION (documented 2.1-vs-7.2 tension): 2.1
//     lists verify-media among the mode=ro readers, yet 7.2 requires
//     the full-match stamp (images.verified_at + state 'verified') -
//     a WRITE. The ruling shipped here: the read path NEVER writes
//     (the handle above is mode=ro), and the ONE small stamp
//     transaction of a FULL match runs on a SEPARATE, lazily-opened
//     read-write connection that deliberately takes NO advisory flock
//     (catalog.WithLocker with a no-op Locker). SQLite's WAL
//     single-writer discipline plus the 5s busy_timeout serializes
//     that tiny UPDATE against any concurrent writer, so a full-match
//     verify during a copy run stamps instead of failing the run with
//     the busy class (which 2.1 explicitly promises readers never
//     see); a PARTIAL verify performs zero catalog writes. The
//     consequence is recorded honestly: verify-media is a no-flock
//     reader that may interleave one idempotent single-row UPDATE with
//     a writer - exactly what 7.2 demands of it.
//   - A STAMP THAT FAILS IS NON-FATAL (DESIGN_SPEC 2.1, Clarified
//     2026-10-21: "a stamp that ever exceeded busy_timeout is
//     non-fatal: the content result still reports verified, and a
//     later verify re-stamps"): the service returns the content verdict
//     with Stamped=false and a StampWarning instead of an error, so
//     the image keeps its computed status ('verified' for a clean
//     match), the report carries a WARNING line naming the stamp
//     failure (progress.Warn on stderr + the image's "warnings" entry
//     in the report - never an anomaly classification, never a
//     'failed' image), and the run exits 0 - the metadata-warning
//     precedent (warnings never create exit 10; a real anomaly beside
//     it still carries its own). The row was never written, so a LATER
//     verify re-stamps it.
//   - No runs row and no run_events: the read-only policy of 2.1 keeps
//     this command out of the runs table (the kind 'verify_media' is
//     reserved for a future run-recording revision); the stdout report
//     is the record of the run.
//   - Exit semantics (7.2 is SILENT on exit codes; the validate
//     precedent of 7.3 governs, matching IMPLEMENTATION_PLAN T5.2):
//     exit 0 when every selected image verifies clean; exit 10 when
//     the report enumerates ANY anomaly - mismatched, missing,
//     unexpected, or unreadable payloads, or a per-image failure
//     (unresolvable location, extraction failure, or an image the
//     extraction could not read at all: zero entries against a
//     non-empty content list) - with --strict promoting that tail to
//     exit 1; exit 3 (the T4.1 DependencyError seam) when an ISO image
//     is in scope and xorriso is absent or older than 1.5.4 - copy-tree
//     verification needs NO xorriso and never pays that check; exit 130
//     on SIGINT/SIGTERM. A stamp write failure is none of these: it is
//     a WARNING that leaves the exit at 0 (see above).
//   - Image location: images.final_path (2.3: "copy dir or .iso path
//     once built"), which the build path has always recorded
//     (FinishBuildTx) and which the copy path records on completion
//     (catalog.RecordCopyFinalPath - the T5.1 gap closure; since T6.5
//     that same write also stamps mode='copy' + state='built', so the
//     mode column is no longer NULL for copy images). The source mode
//     of THIS command is nevertheless resolved by STAT of final_path,
//     never read from the mode column: the column is catalog
//     provenance (and rows written before T6.5 still carry NULL), and
//     the filesystem kind at final_path is what decides xorriso-vs-
//     direct-read below. An image with an empty final_path is an
//     honest per-image failure (exit 10), never a guess.
//   - --check_md5_r is NOT implemented (7.2: xorriso's recorded-MD5
//     scans operate on data the mastering step recorded; the catalog
//     SHA-256 re-hash is strictly stronger and format-independent,
//     so the Go re-hash IS the V1 verification). The command help
//     documents it as a future/optional physical-layer supplement for
//     burned media.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"isopress/internal/catalog"
	"isopress/internal/progress"
	"isopress/internal/report"
	"isopress/internal/verify"
	"isopress/internal/xorriso"
)

// verifyMediaOptions holds the command-local flag values.
type verifyMediaOptions struct {
	images []int64 // --image: repeatable image selection (default: every image)
}

// newVerifyMediaCommand builds the real verify-media command (replaces
// the T0.5 stub).
func newVerifyMediaCommand(opts *rootOptions) *cobra.Command {
	vo := verifyMediaOptions{}
	cmd := &cobra.Command{
		Use:   "verify-media",
		Short: "verify built media against the catalog hashes (read-only)",
		Long: "Re-read every selected image of the --set-id set and compare it against the " +
			"catalog's finalized content list (DESIGN_SPEC 7.2, FR-021), hash first and " +
			"without mounting anything: an ISO image is re-read through ONE xorriso osirrox " +
			"extraction into a temp directory (free-space pre-checked before extraction; " +
			"--work-dir chooses the scratch root), a copy tree is read DIRECTLY - no xorriso, " +
			"no temp. Every file and chunk is SHA-256 re-hashed; symlinks compare by target; " +
			"missing and unexpected payloads are reported per image with counts and per-file " +
			"lines (matched, mismatched, missing, unexpected, unreadable). On a FULL match " +
			"the image is stamped verified_at + state 'verified'; a partial match leaves the " +
			"image state unchanged (the report is the honest record); a stamp that cannot be " +
			"written is a WARNING, not a failure - the content verdict still reports verified, " +
			"stamped false, and a later verify re-stamps (DESIGN_SPEC 2.1). The catalog is opened " +
			"read-only with no advisory lock, so verify-media runs during a copy or build " +
			"run. NOTE: xorriso's own -check_media / -check_md5_r recorded-MD5 scans are a " +
			"possible FUTURE supplement for burned-media physical-layer readability - not a " +
			"substitute for this Go re-hash (the catalog SHA-256 is the integrity anchor), " +
			"and not implemented in V1.\n\n" +
			"Exit codes: 0 success; 1 fatal unexpected error; 2 usage (bad flags, a set that has " +
			"never been scanned); 3 xorriso missing or older than 1.5.4 when an ISO image is in " +
			"scope (copy-tree verification never needs it); 4 catalog error; 10 completed with " +
			"anomalies, enumerated above (--strict promotes this to 1); 130 interrupted. " +
			"verify-media never exits 5: it opens the catalog read-only and takes no advisory lock. " +
			"JSON: --json prints the final report as exactly one JSON document on stdout; a run that " +
			"aborts before doing work prints no document at all; --json never changes the exit code.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVerifyMedia(cmd.Context(), cmd, opts, vo)
		},
	}
	cmd.Flags().Int64SliceVar(&vo.images, "image", nil,
		"verify only this image number (repeatable; default: every image of the set)")
	return cmd
}

// verifyTarget is one resolved verification target (the pre-pass
// outcome: the image row plus the source mode derived from its
// final_path, or the honest per-image resolution failure).
type verifyTarget struct {
	id      int64
	meta    catalog.ImageMeta
	source  string // verify.SourceISO | verify.SourceCopyTree | "" when unresolved
	failure string // non-empty: per-image failure line (exit-10 class)
}

// runVerifyMedia is the verify-media service invocation (see the file
// doc for the wiring contracts). The named return feeds the deferred
// progress Done event's Failed signal (progress_done.go).
func runVerifyMedia(ctx context.Context, cmd *cobra.Command, opts *rootOptions, vo verifyMediaOptions) (runErr error) {
	// --- usage-class inputs (exit 2, 10.1) ---
	if opts.catalog == "" {
		return &report.UsageError{Command: cmd.CommandPath(), Msg: "--catalog is required (path to the SQLite catalog file)"}
	}
	if opts.setID == "" {
		return &report.UsageError{Command: cmd.CommandPath(), Msg: "--set-id is required (the set whose media is verified)"}
	}

	// READ-ONLY open: NO advisory flock (2.1 Option B) - this command
	// must work during a running copy/build over a WAL snapshot.
	db, err := catalog.Open(opts.catalog, catalog.OpenReadOnly)
	if err != nil {
		return err
	}
	defer db.Close() //nolint:errcheck // best-effort; the run result dominates

	ids, err := db.CopyImageIDs(ctx, opts.setID)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return &report.UsageError{
			Command: cmd.CommandPath(),
			Msg: fmt.Sprintf("set %q has no planned images (run 'isopress scan' and 'isopress plan' first)",
				opts.setID),
		}
	}
	selected, err := selectCopyImages(cmd, ids, vo.images)
	if err != nil {
		return err
	}

	// THE RESOLUTION PRE-PASS: read each image's row and derive its
	// source mode from final_path (stat), so the xorriso dependency
	// decision below is made ONCE, before any extraction work, and a
	// set of copy trees never pays it.
	targets := make([]verifyTarget, 0, len(selected))
	needsXorriso := false
	for _, id := range selected {
		tgt := verifyTarget{id: id}
		meta, err := db.ImageMeta(ctx, opts.setID, id)
		if err != nil {
			return err // catalog plumbing (the ids came from this catalog) - terminal
		}
		tgt.meta = meta
		switch meta.FinalPath {
		case "":
			tgt.failure = "images.final_path is empty - this image has no recorded destination " +
				"(it never completed a copy or build; images written before T5.1 predate the copy-side recording), so there is nothing to re-read"
		default:
			fi, err := os.Stat(meta.FinalPath)
			switch {
			case err != nil:
				tgt.failure = fmt.Sprintf("%s: %v", meta.FinalPath, err)
			case fi.Mode().IsRegular():
				tgt.source = verify.SourceISO
				needsXorriso = true
			case fi.IsDir():
				tgt.source = verify.SourceCopyTree
			default:
				tgt.failure = fmt.Sprintf("%s is neither a regular file (.iso) nor a directory (copy tree): %v",
					meta.FinalPath, fi.Mode())
			}
		}
		targets = append(targets, tgt)
	}

	// NFR-004 fail-fast: xorriso discovery + the >= 1.5.4 floor ONLY
	// when an ISO image is in scope (the T4.1 DependencyError seam ->
	// exit 3); copy-tree verification needs no xorriso at all.
	var tool *xorriso.Tool
	if needsXorriso {
		tool, err = xorriso.Discover()
		if err != nil {
			return err
		}
		if _, err := tool.Probe(ctx); err != nil {
			return err
		}
	}

	// Fresh progress bus per operation (T1.1); renderer per the 9.2
	// matrix. The deferred Finish/wait pair guarantees exactly one
	// Done summary line on EVERY exit path.
	bus := progress.NewBus()
	wait := progress.Start(progress.New(progress.Options{
		Quiet:  opts.quiet,
		Stderr: cmd.ErrOrStderr(),
	}), bus)
	summary := fmt.Sprintf("verify-media: set %s aborted before any image was verified", opts.setID)
	defer func() {
		bus.Finish(progress.Done{Summary: summary, Failed: runFailed(runErr), Interrupted: runInterrupted(runErr)})
		wait()
	}()
	// The run-level heartbeat (HeartbeatInterval, build.go): one Tick
	// per interval filling the per-FILE silent stretches of the hash
	// walk. Registered AFTER the Finish defer (LIFO: it stops before
	// the terminal Done - a Tick can never trail the summary).
	stopHeartbeat := startHeartbeat(ctx, bus)
	defer stopHeartbeat()

	store := &verifyStore{ro: db, catalogPath: opts.catalog}
	defer store.Close() //nolint:errcheck // closes the lazily-opened stamping handle

	r := &verifyRunner{
		cmd:      cmd,
		setID:    opts.setID,
		workDir:  opts.workDir,
		bus:      bus,
		store:    store,
		tool:     tool,
		jsonFlag: opts.json,
		strict:   opts.strict,
	}
	for _, tgt := range targets {
		if ctx.Err() != nil {
			break // interrupted: stop scheduling new images (10.1)
		}
		r.verifyImage(ctx, tgt)
	}
	return r.finalize(ctx, &summary, nil)
}

// verifyRunner carries the run-scoped state through the image loop.
type verifyRunner struct {
	cmd     *cobra.Command
	setID   string
	workDir string
	bus     *progress.Bus
	store   *verifyStore
	tool    *xorriso.Tool

	jsonFlag bool
	strict   bool

	// Accumulators (single goroutine - the image loop is sequential).
	images       []verifyMediaImageReport
	failureLines []string
}

// verifyImage verifies ONE resolved target. Per-image failures are
// RECORDED here (the N3 continue semantics of the build/copy loops:
// the run continues with the remaining images and exits 10).
// Warnings - today only the stamp-failure carve-out - are recorded as
// report warnings instead: they never set status "failed", never add a
// failure line, and never move the exit code (the metadata-warning
// precedent).
func (r *verifyRunner) verifyImage(ctx context.Context, tgt verifyTarget) {
	rep := verifyMediaImageReport{Image: tgt.id, Path: tgt.meta.FinalPath, Source: tgt.source, Status: "failed"}
	defer func() {
		r.images = append(r.images, rep)
	}()

	if tgt.failure != "" {
		rep.Failure = tgt.failure
		r.recordFailure(tgt.id, tgt.failure)
		return
	}

	res, err := verify.VerifyImage(ctx, verify.Deps{
		Store: r.store,
		Tool:  r.tool,
		Bus:   r.bus,
	}, verify.VerifyInput{
		SetID:   r.setID,
		ImageID: tgt.id,
		Path:    tgt.meta.FinalPath,
		WorkDir: r.workDir,
	})
	if err != nil {
		rep.Failure = err.Error()
		r.recordFailure(tgt.id, err.Error())
		return
	}

	rep.Matched = len(res.Comparison.Matched)
	rep.Mismatched = len(res.Comparison.Mismatched)
	rep.Missing = len(res.Comparison.Missing)
	rep.Unexpected = len(res.Comparison.Unexpected)
	rep.Unreadable = len(res.Comparison.Unreadable)
	rep.Stamped = res.Stamped
	rep.Problems = res.Comparison.Problems()
	// THE STAMP WARNING (2.1 carve-out): a clean content verdict whose
	// stamp write failed keeps its status, its counts, and its exit
	// class - the warning rides along in the report and on stderr (the
	// service already emitted progress.Warn) and changes NOTHING else.
	if res.StampWarning != "" {
		rep.Warnings = append(rep.Warnings, res.StampWarning)
	}
	if res.Comparison.Clean() {
		rep.Status = "verified"
	} else {
		rep.Status = "mismatch"
		for _, p := range rep.Problems {
			r.failureLines = append(r.failureLines, fmt.Sprintf("verify-media: image %d: %s", tgt.id, p))
		}
	}
}

// recordFailure records one per-image failure line (exit-10 class):
// report line + a progress Error event (stderr). run_events does not
// exist for this command (no run row - the read-only policy).
func (r *verifyRunner) recordFailure(imageID int64, detail string) {
	line := fmt.Sprintf("verify-media: image %d: %s", imageID, detail)
	r.failureLines = append(r.failureLines, line)
	r.bus.Emit(progress.Error{Msg: line})
}

// finalize stamps nothing but the report: it lands on stdout on EVERY
// exit path (FR-046: the result is the stdout artifact even on failure
// paths), then the command result is classified:
// interrupted (130) > terminal error > anomalies (10, or 1 with
// --strict) > nil (0) - the exit semantics documented in the file doc
// (7.2 is silent; the 7.3 validate precedent governs). summary is
// written back through the pointer so the deferred Done emission
// carries the final numbers (the scan.go convention).
func (r *verifyRunner) finalize(ctx context.Context, summary *string, runErr error) error {
	interrupted := ctx.Err() != nil
	verified, anomalies, totals := r.totals()
	status := "ok"
	switch {
	case interrupted:
		status = "interrupted"
	case runErr != nil:
		status = "failed"
	case anomalies > 0:
		status = "partial"
	}

	rep := verifyMediaReport{
		SetID: r.setID, Status: status,
		Matched: totals.matched, Mismatched: totals.mismatched, Missing: totals.missing,
		Unexpected: totals.unexpected, Unreadable: totals.unreadable,
		Verified: verified, Images: r.images,
	}
	if err := writeVerifyMediaReport(r.cmd.OutOrStdout(), rep, r.jsonFlag); err != nil {
		runErr = errors.Join(runErr, err)
	}

	*summary = fmt.Sprintf("verify-media: %d image(s) verified, %d anomalous (matched %d entries, mismatched %d, missing %d, unexpected %d, unreadable %d)",
		verified, anomalies, totals.matched, totals.mismatched, totals.missing, totals.unexpected, totals.unreadable)

	if interrupted {
		return fmt.Errorf("%w: verify-media of set %s interrupted; no stamp was written for the in-flight image",
			report.ErrInterrupted, r.setID)
	}
	if runErr != nil {
		return runErr
	}
	if anomalies > 0 {
		if r.strict {
			return fmt.Errorf("verify-media completed with %d anomalous image(s)/payload(s); --strict promotes this to a fatal error", anomalies)
		}
		var errs []error
		for _, line := range r.failureLines {
			errs = append(errs, errors.New(line))
		}
		return &report.ItemFailures{Failures: errs}
	}
	return nil
}

// runTotals are the folded per-image figures.
type verifyTotals struct {
	matched, mismatched, missing, unexpected, unreadable int
}

// totals folds the per-image reports: the number of CLEAN-STAMPED
// images, the anomaly count (images with any anomaly OR a per-image
// failure), and the summed payload counts. Warnings are deliberately
// NOT folded in: an image that matched fully but could not be stamped
// (the 2.1 carve-out) counts as neither verified (no stamp landed) nor
// anomalous (the content is clean), so warnings never move the exit -
// the metadata-warning precedent.
func (r *verifyRunner) totals() (verified, anomalies int, t verifyTotals) {
	for _, img := range r.images {
		t.matched += img.Matched
		t.mismatched += img.Mismatched
		t.missing += img.Missing
		t.unexpected += img.Unexpected
		t.unreadable += img.Unreadable
		if img.Stamped {
			verified++
		}
		if img.Status != "verified" || img.Mismatched+img.Missing+img.Unexpected+img.Unreadable > 0 {
			anomalies++
		}
	}
	return verified, anomalies, t
}

// --- the catalog seam (read-only handle + the lock-free stamp) ----------------

// verifyStore adapts the READ-ONLY catalog handle (every read) plus a
// LAZILY-OPENED lock-free read-write handle (the single full-match
// stamp) onto verify.VerifyStore. See the file doc for the 2.1-vs-7.2
// stamping ruling.
type verifyStore struct {
	ro          *catalog.DB
	catalogPath string

	rw *catalog.DB // opened on the first stamp, closed with the run
}

// ImageContents implements verify.VerifyStore through the read-only
// handle.
func (s *verifyStore) ImageContents(ctx context.Context, setID string, imageID int64) (verify.ContentList, error) {
	list, err := s.ro.ImageContentList(ctx, setID, imageID)
	if err != nil {
		return verify.ContentList{}, err
	}
	out := verify.ContentList{
		Rows:           make([]verify.ContentRow, 0, len(list.Rows)),
		SymlinkTargets: list.SymlinkTargets,
	}
	for _, r := range list.Rows {
		out.Rows = append(out.Rows, verify.ContentRow{
			MediaPath: r.MediaPath, Kind: r.Kind, FileID: r.FileID,
			ChunkIndex: r.ChunkIndex, Size: r.Size, Hash: r.Hash,
		})
	}
	return out, nil
}

// StampVerified implements verify.VerifyStore: the ONE full-match
// transaction, on the lazily-opened lock-free read-write handle (the
// file doc's stamping ruling; a partial verify never reaches here).
// The handle runs catalog.Open's standard mutating path, which for a
// current-version catalog is a no-op beyond this transaction (no
// migration, no folded-object repair); the stamp itself is one
// single-row UPDATE that SQLite's WAL serialization bounds against any
// concurrent writer. An error from this call is NON-FATAL upstream
// (the service turns it into the report warning; see the file doc's
// "A STAMP THAT FAILS IS NON-FATAL" clause) - notably it is never
// re-typed into the exit-5 busy class: the no-op Locker keeps
// CatalogBusy out of this path entirely.
func (s *verifyStore) StampVerified(ctx context.Context, setID string, imageID int64, verifiedAt int64) error {
	if s.rw == nil {
		rw, err := catalog.Open(s.catalogPath, catalog.OpenReadWrite, catalog.WithLocker(flockFreeLocker{}))
		if err != nil {
			return fmt.Errorf("opening the stamping connection: %w", err)
		}
		s.rw = rw
	}
	return s.rw.StampImageVerified(ctx, setID, imageID, verifiedAt)
}

// Close releases the lazily-opened stamping handle (the read-only
// handle is the command's own defer).
func (s *verifyStore) Close() error {
	if s.rw == nil {
		return nil
	}
	err := s.rw.Close()
	s.rw = nil
	return err
}

// flockFreeLocker is the Locker seam for the stamping connection: it
// takes NO advisory lock (Acquire returns a no-op release). See the
// file doc ("THE STAMPING CONNECTION") for why verify-media's single
// full-match UPDATE must not contend with the single-writer flock -
// and why SQLite's WAL serialization is the consistency mechanism
// that covers it instead.
type flockFreeLocker struct{}

// Acquire implements catalog.Locker without touching any lock file.
func (flockFreeLocker) Acquire(catalog.LockMode) (func() error, error) {
	return func() error { return nil }, nil
}

// --- the report model (human and --json; the key set is FROZEN) ----------------

// verifyMediaImageReport is one image's verification outcome. The JSON
// key set is a deliberate frozen schema (the scan/plan/copy/build
// report precedent): new keys are a conscious schema change with the
// pinning tests updated in the same commit. "failure", "problems",
// and "warnings" are omitempty and join the set exactly when the image
// carries them.
type verifyMediaImageReport struct {
	Image   int64  `json:"image"`
	Path    string `json:"path"`
	Source  string `json:"source"` // "iso" | "copy" | "" (unresolved)
	Status  string `json:"status"` // "verified" | "mismatch" | "failed"
	Matched int    `json:"matched"`
	// The four anomaly counts of 7.2 plus the matched count.
	Mismatched int `json:"mismatched"`
	Missing    int `json:"missing"`
	Unexpected int `json:"unexpected"`
	Unreadable int `json:"unreadable"`
	// Stamped reports the 7.2 full-match stamp (verified_at + state
	// 'verified'); false on any anomaly (state unchanged, file doc)
	// AND false on a clean match whose stamp WRITE failed (the 2.1
	// carve-out: the content verdict still reports verified).
	Stamped  bool     `json:"stamped"`
	Failure  string   `json:"failure,omitempty"`
	Problems []string `json:"problems,omitempty"`
	// Warnings are non-fatal notices attached to this image (today
	// only the stamp-failure carve-out). They are NOT anomalies: they
	// never change Status, never add to the five counts, never create
	// a failure line, and never produce exit 10 on their own - the
	// metadata-warning precedent (warnings exit 0 unless a real
	// anomaly exists elsewhere in the run). Omitempty keeps the
	// frozen key set byte-identical for runs without warnings, and a
	// populated array is a deliberate schema addition pinned by the
	// stamp-failure tests.
	Warnings []string `json:"warnings,omitempty"`
}

// verifyMediaReport is the end-of-run report (human lines and --json
// are two presentations of the same numbers, FR-046: stdout only).
type verifyMediaReport struct {
	SetID  string `json:"set_id"`
	Status string `json:"status"` // ok | partial | failed | interrupted (runs.status spelling)
	// The five 7.2 figures, summed across the run's images.
	Matched    int `json:"matched"`
	Mismatched int `json:"mismatched"`
	Missing    int `json:"missing"`
	Unexpected int `json:"unexpected"`
	Unreadable int `json:"unreadable"`
	// Verified is the number of images stamped this run (full match).
	// A clean match whose stamp WRITE failed (the 2.1 carve-out) is
	// NOT counted here: no stamp landed - the image's warnings entry
	// is the record of that run's verdict-without-stamp.
	Verified int                      `json:"verified"`
	Images   []verifyMediaImageReport `json:"images"`
}

// writeVerifyMediaReport renders the report: --json emits exactly one
// JSON document; the human form is the set identity line, one line per
// image (7.2: path + the five counts) followed by that image's
// warnings, per-file problem lines, and failure detail, and a totals
// line (the plan/scan/copy/build family style).
func writeVerifyMediaReport(w io.Writer, rep verifyMediaReport, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	if _, err := fmt.Fprintf(w, "verify-media: set %s\n", rep.SetID); err != nil {
		return err
	}
	for _, img := range rep.Images {
		if _, err := fmt.Fprintf(w,
			"image %d: %s %s (%s) matched %d, mismatched %d, missing %d, unexpected %d, unreadable %d, stamped %v\n",
			img.Image, img.Status, img.Path, sourceLabel(img.Source),
			img.Matched, img.Mismatched, img.Missing, img.Unexpected, img.Unreadable, img.Stamped); err != nil {
			return err
		}
		for _, warn := range img.Warnings {
			if _, err := fmt.Fprintf(w, "  warning: %s\n", warn); err != nil {
				return err
			}
		}
		for _, p := range img.Problems {
			if _, err := fmt.Fprintf(w, "  %s\n", p); err != nil {
				return err
			}
		}
		if img.Failure != "" {
			if _, err := fmt.Fprintf(w, "  %s\n", img.Failure); err != nil {
				return err
			}
		}
	}
	if _, err := fmt.Fprintf(w,
		"totals: images %d, verified %d, matched %d, mismatched %d, missing %d, unexpected %d, unreadable %d (status: %s)\n",
		len(rep.Images), rep.Verified, rep.Matched, rep.Mismatched, rep.Missing, rep.Unexpected, rep.Unreadable,
		rep.Status); err != nil {
		return err
	}
	return nil
}

// sourceLabel renders a source mode for the human report ("" is an
// unresolved image).
func sourceLabel(source string) string {
	switch source {
	case verify.SourceISO:
		return "iso"
	case verify.SourceCopyTree:
		return "copy"
	default:
		return "unknown"
	}
}
