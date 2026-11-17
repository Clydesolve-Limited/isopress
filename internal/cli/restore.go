package cli

// T5.3: the real `restore` command (DESIGN_SPEC 8.1 D-07 latest-state, 8.2
// selection model, 8.3 multi-image orchestration, 10.2 exit scheme;
// IMPLEMENTATION_PLAN 4.6 T5.3). It replaces the LAST T0.5 placeholder in
// commands.go.
//
// The command owns flags, the read-only catalog open, stream/TTY decisions,
// the report, and exit classification; internal/restore owns selection,
// supply, the one-at-a-time image loop, and the per-image CONTENT restoration
// (the real ImageRestorer - extraction, chunked reassembly, metadata - which
// restoreRestorerFactory constructs from the T5.4 engine).
//
// WIRING CONTRACTS:
//
//   - READ-ONLY catalog open (catalog.OpenReadOnly, URI mode=ro): NO
//     advisory flock at all (DESIGN_SPEC 2.1 Option B as amended
//     2026-10-18), so restore runs DURING a copy/build run over a WAL
//     snapshot - pinned by the concurrent-reader tests below. There is
//     deliberately NO restore-state in the schema (8.x): the catalog is
//     never mutated, restore takes no runs row and no run_events (the
//     read-only policy of the verify-media/validate precedent), and a
//     restore run leaves the catalog file BYTE-IDENTICAL (asserted by
//     TestRestoreLeavesCatalogByteIdentical).
//   - MEDIA-ALONE FLOW (T5.5, 8.5): --from-media PATH REPLACES the
//     --catalog file with the media's own FR-034 export. The flow is:
//     (1) MANIFEST introspection FIRST - restore.ResolveMedia parses
//     the media's MANIFEST.json (set id, image list, whole-set
//     placement) WITHOUT opening the export; (2) the export is
//     TEMP-COPIED under --work-dir and the COPY is opened read-only
//     (the T5.2 lesson: a direct mode=ro open can materialize
//     -wal/-shm sidecars ON THE MEDIA; the copy means nothing is ever
//     written there - pinned by the media byte-identical + no-sidecar
//     assertions); (3) PRAGMA integrity_check runs on the copy (the
//     T5.1 precedent, 8.5's "the export's internal hashes are the deep
//     anchor"); (4) the set id is derived FROM the export (set-scoped
//     by construction) and cross-checked against the manifest and, when
//     given, against --set-id (mismatch = usage error naming both,
//     never a silent override); (5) the media's trees become the base
//     image supply (restore.SupplySpec.Media, consulted FIRST); (6) the
//     manifest anchor check (verifyMediaManifestAnchor in
//     restore_media.go) compares the media's MANIFEST.json against the
//     export's recorded manifest self-row when present. Everything
//     after the swap proceeds through the IDENTICAL engine (the T5.3
//     selection and the T5.4 restorer speak to the export exactly as
//     they speak to the original catalog: the export carries the same
//     schema and the full set placement, 6.6). ONE adjustment rides in
//     the media layer (restore_media.go): the run-start export snapshots
//     chunks BEFORE first-write stores their lazily-hashed digests
//     (2.3/4.3), so a NULL export chunk hash is filled from a media
//     manifest's chunk table (6.6's media-alone reassembly record); an
//     export hash always wins. Because the inventory is built BEFORE
//     supply resolution, that fill has two seats: mediaStore at
//     inventory time over the manifests the media itself carried, and
//     sidecarManifestRestorer after the supply pass, where whatever
//     SourceCopyTree directory resolved for an image - the media base,
//     an --image mapping, a --copy-tree-root supplement, or a prompted
//     path - contributes its sidecar MANIFEST.json. The schema-version
//     gate applies naturally to the temp copy: a future-version export
//     fails with the clear ErrSchemaTooNew message (exit 4).
//   - --target is REQUIRED and is NEVER the original source: this command
//     never reads sets.source_root, so there is no implicit-source restore
//     (8.2) - the structural test TestRestoreNeverTouchesSourceRoot pins the
//     absence at the source level.
//   - FLAG SURFACE (8.2/8.3): --set-id (global) names the set; --path is
//     repeatable exact relpaths; --subtree is a directory prefix;
//     --image-dir (global) discovers <SET-ID>-NNN.iso per 6.4; --image N=PATH
//     is repeatable and stat-classified (file = ISO, directory = copy tree);
//     --copy-tree-root supplies <SET-ID>-NNN/ trees; --work-dir (global)
//     seeds the single temp workspace; --json switches the report. Supply
//     sources are NOT stat-checked as roots: a supply root that does not
//     exist yields the 8.3 missing-media path, whose listing names the exact
//     candidates sought - strictly more diagnostic than an eager flag error,
//     and it keeps every "the media is not there" outcome on one exit code.
//   - TTY GATE (8.3, RCR-3): stderr's terminal state decides whether the run
//     may prompt (the same detector the 9.2 renderer matrix uses, taken from
//     the command's own stderr writer so an injected test writer counts as
//     non-TTY). A TTY run prompts "Image N of set S is required for <path>.
//     Provide its path (or 'q' to abort):" on stderr and reads the answer
//     from stdin; a non-TTY run prints one machine-readable
//     "required-image: ..." line per missing image (through the progress bus,
//     so it lands on stderr) and fails. In BOTH cases the failure class is
//     10.1's DependencyError -> exit 3: see MISSING_MEDIA_EXIT below.
//   - XORRISO (T4.1 seam, exit 3): the probe runs ONCE, up-front, and only
//     when a supplied image is an .iso - T5.4's extraction needs it, so a
//     copy-tree restore never pays the check and an ISO restore fails BEFORE
//     any target write (restore.ProbeISOOnce drives it from inside the
//     orchestration).
//   - EXIT SEMANTICS (10.2), the full matrix for this command:
//     0 = every selected member restored; 2 = usage (missing --set-id/
//     --target, a --path/--subtree matching nothing, malformed --image,
//     unplanned set); 3 = required media unavailable (non-TTY listing or an
//     interactive 'q') OR xorriso absent with an ISO in scope; 10 = the run
//     completed but the report enumerates failures (per-file failures from
//     the seam, skipped members, unclaimed paths, a failed image) with
//     --strict promoting that to 1; 130 = interrupted; 1 = fatal.
//   - REPORT (10.3): stdout carries the report on every path EXCEPT the
//     abort-BEFORE-WORK classes (nothing ran there, so - like every other
//     abort-before-work path in the tool - stdout stays empty): the
//     selection/supply input failures (exit 2), the two media-supply aborts
//     and the up-front xorriso refusal (exit 3). A terminal error that
//     strikes MID-LOOP (store read, workspace) still reports what DID
//     restore before returning its own class; --json emits one document with
//     a FROZEN key set.
//
// MISSING_MEDIA_EXIT (the 8.3 "dedicated code" question): 8.3 defers the
// non-TTY missing-image code to section 10, whose table has no dedicated
// restore-media row, so the ratified IMPLEMENTATION_PLAN 5.5 resolution 3
// governs - "a machine-readable required-images listing and a non-zero exit
// per the 10.2 scheme without inventing a new code". This run picks 3,
// the scheme's DependencyError class: the operation could not proceed
// because something it requires is unavailable, which is exactly what 3
// already means for xorriso (and restores share that meaning: 3 = "supply
// something"). 10 was rejected because it asserts a COMPLETED run with
// per-item failures and this run completed NOTHING; 1 was rejected as the
// bug class. The machine-readable listing is what tells the two 3-cause
// cases apart, which is the RCR-3 reading.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"isopress/internal/catalog"
	"isopress/internal/media"
	"isopress/internal/progress"
	"isopress/internal/report"
	"isopress/internal/restore"
	"isopress/internal/units"
	"isopress/internal/xorriso"
)

// restoreOptions holds the command-local flag values.
type restoreOptions struct {
	target       string   // --target (8.2, required)
	paths        []string // --path (repeatable, exact relpaths)
	subtree      string   // --subtree (directory prefix)
	images       []string // --image N=PATH (repeatable, raw values)
	copyTreeRoot string   // --copy-tree-root (<SET-ID>-NNN trees)
	fromMedia    string   // --from-media PATH (8.5: media-alone restore)
}

// newRestoreCommand builds the real restore command (replaces the T0.5 stub).
func newRestoreCommand(opts *rootOptions) *cobra.Command {
	ro := restoreOptions{}
	cmd := &cobra.Command{
		Use:   "restore",
		Short: "restore files from a backup set to a fresh target",
		Long: "Restore part or all of the --set-id set into --target (DESIGN_SPEC 8.1-8.4). " +
			"V1 restores LATEST STATE ONLY (D-07, ratified 2026-10-18): the selection resolves " +
			"against the catalog's active records, so an as-of-date / point-in-time restore is " +
			"explicitly DEFERRED and not accepted by any flag. Selection: no selector restores " +
			"the whole set; --path names exact relpaths (repeatable); --subtree takes a " +
			"directory prefix (component-aware: 'docs' matches 'docs/a', never 'docs-old'). " +
			"--target is required and restore NEVER writes onto the original source path " +
			"implicitly. A hard-link family selected partially still restores correctly: the " +
			"content comes from the primary's image and only the REQUESTED names are created. " +
			"Chunked files resolve to every image holding their chunks. Images are supplied in " +
			"the 8.3 preference order: --image-dir discovers <SET-ID>-NNN.iso (6.4 naming), " +
			"--image N=PATH supplies one path (a file is an ISO, a directory is a copy tree), " +
			"--copy-tree-root supplies <SET-ID>-NNN/ trees - all three may be combined. " +
			"MEDIA-ALONE RESTORE (8.5): --from-media PATH restores WITHOUT the original catalog, " +
			"where PATH is a copy-set directory (or a directory of S-NNN trees, or an extracted " +
			"ISO tree) carrying MANIFEST.json and catalog-export.sqlite. The tool introspects the " +
			"manifest first (set id, image list, placement - no export open), TEMP-COPIES the " +
			"export under --work-dir (nothing is ever written to the media) and opens the copy " +
			"read-only as the catalog: the FR-034 set-scoped export IS the catalog, the schema " +
			"gate applies (a future-version export is rejected, exit 4), and the media's trees " +
			"supply the payload - consulted FIRST, before the --image / --image-dir / " +
			"--copy-tree-root supplements. --catalog is then mutually exclusive and --set-id " +
			"becomes optional, acting as a cross-check against the media's own set identity. " +
			"Required images are processed ONE AT A TIME through a single temp workspace that is " +
			"cleaned between images (--work-dir chooses its root). When an image is missing and " +
			"stderr is a terminal the tool prompts for its path ('q' aborts cleanly before any " +
			"write); on a non-interactive run it prints one 'required-image:' line per missing " +
			"image. The catalog is opened read-only with no advisory lock, so restore " +
			"during a copy or build works and never mutates the catalog.\n\n" +
			"Exit codes: 0 success; 1 fatal unexpected error; 2 usage (missing --target, --set-id or " +
			"--catalog, a selector that matches nothing, a malformed --image, an unplanned set); " +
			"3 required media unavailable on a non-interactive run or an aborted supply prompt, or " +
			"xorriso missing or older than 1.5.4 when an ISO is in scope; 4 catalog error, including " +
			"a future-version catalog-export.sqlite; 10 completed with failures, enumerated above " +
			"(--strict promotes this to 1); 130 interrupted. restore never exits 5: it opens the " +
			"catalog read-only. JSON: --json prints the final report as exactly one JSON document on " +
			"stdout; a run that aborts before doing work prints no document at all; --json never " +
			"changes the exit code.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRestore(cmd.Context(), cmd, opts, ro)
		},
	}
	cmd.Flags().StringVar(&ro.target, "target", "",
		"directory to restore into (required; never the original source path)")
	cmd.Flags().StringArrayVar(&ro.paths, "path", nil,
		"restore this exact catalog relpath (repeatable; --subtree for a directory prefix)")
	cmd.Flags().StringVar(&ro.subtree, "subtree", "",
		"restore every record under this relpath prefix (component-aware directory match)")
	// --path and --subtree deliberately take NO tilde gate (paths.go): their
	// values are CATALOG RELPATHS, never filesystem paths this tool resolves
	// against a root, so there is no shell-expansion question to detect - a
	// relpath starting with "~" is a literal catalog name. Every path the run
	// actually touches the filesystem with (--target, --image, --copy-tree-
	// root, --image-dir, --work-dir, --catalog) IS tilde-gated.
	cmd.Flags().StringArrayVar(&ro.images, "image", nil,
		"supply image N from PATH (repeatable, N=PATH; stat decides: a file is an ISO, a directory is a copy tree)")
	cmd.Flags().StringVar(&ro.copyTreeRoot, "copy-tree-root", "",
		"directory containing <SET-ID>-NNN copy trees for restore")
	cmd.Flags().StringVar(&ro.fromMedia, "from-media", "",
		"media-alone restore (DESIGN_SPEC 8.5): PATH is a copy-set directory or an extracted ISO tree carrying MANIFEST.json and catalog-export.sqlite; "+
			"the export is temp-copied and opened read-only as the catalog, --catalog is not used, --set-id becomes an optional cross-check. "+
			"CHUNK-HASH FILL / ISO ENDPOINT: a first-run export's NULL chunk hashes are filled from the sidecar MANIFEST.json of whichever copy-tree "+
			"directory the supply resolves for that image, while an .iso has no readable sidecar manifest - so a first-run chunked member whose image "+
			"is supplied only as an ISO fails honestly with 'the chunk row carries no stored hash': per-chunk first-verification has no anchor in that "+
			"shape (those chunks can be neither verified nor diagnosed individually) and the member is not published. Whole-file members still verify "+
			"against the whole-file hash, and a chunked member whose chunks all carry a stored hash is checked chunk by chunk (8.4's first verification) "+
			"and then as a whole file against that same anchor (8.4's second gate): a correct assembly is published, a wrong one fails there")
	return cmd
}

// restoreRestorerFactory builds the per-image content restorer for a run.
// T5.4: this is where the REAL extraction/reassembly engine (restore.
// NewImageRestorer) is constructed; tests inject a RECORDING fake through
// this variable, so no test-only branch exists in production (the
// injectable-seam pattern, without an env check in the command).
//
// The T4.1 xorriso handle rides in the deps for SourceISO images:
// xorriso.Discover is an exec.LookPath only (no process), and an ISO-scope
// run has ALREADY passed probeXorrisoOnce (exit 3, before any image) by the
// time the restorer sees an ISO - so a discover failure here is simply left
// nil and would surface as an honest per-image failure only for a run that
// bypassed the probe.
var restoreRestorerFactory = func(deps restore.RestorerDeps) restore.ImageRestorer {
	if deps.Tool == nil {
		if tool, err := xorriso.Discover(); err == nil {
			deps.Tool = tool
		}
	}
	return restore.NewImageRestorer(deps)
}

// runRestore is the restore service invocation (see the file doc for the
// wiring contracts). The named return feeds the deferred progress Done
// event's Failed signal (progress_done.go).
func runRestore(ctx context.Context, cmd *cobra.Command, opts *rootOptions, ro restoreOptions) (runErr error) {
	// --- usage-class inputs (exit 2, 10.1) ---
	if ro.fromMedia != "" && opts.catalog != "" {
		return &report.UsageError{Command: cmd.CommandPath(), Msg: "--catalog and --from-media are mutually exclusive: with --from-media the catalog IS the media's catalog-export.sqlite (DESIGN_SPEC 8.5), temp-copied and opened read-only"}
	}
	if opts.catalog == "" && ro.fromMedia == "" {
		return &report.UsageError{Command: cmd.CommandPath(), Msg: "--catalog is required (path to the SQLite catalog file; with --from-media it is taken from the media instead)"}
	}
	if opts.setID == "" && ro.fromMedia == "" {
		return &report.UsageError{Command: cmd.CommandPath(), Msg: "--set-id is required (the set to restore; with --from-media it is derived from the media and --set-id becomes an optional cross-check)"}
	}
	if ro.target == "" {
		return &report.UsageError{Command: cmd.CommandPath(), Msg: "--target is required (the directory to restore into; restore never writes to the original source path)"}
	}
	if err := checkTilde(ro.target, "--target"); err != nil {
		return &report.UsageError{Command: cmd.CommandPath(), Err: err}
	}
	if err := checkTilde(ro.copyTreeRoot, "--copy-tree-root"); err != nil {
		return &report.UsageError{Command: cmd.CommandPath(), Err: err}
	}
	if err := checkTilde(ro.fromMedia, "--from-media"); err != nil {
		return &report.UsageError{Command: cmd.CommandPath(), Err: err}
	}
	// The set id becomes the on-media image name (6.4): gate it once, before
	// any database or filesystem work (media is the naming authority). With
	// --from-media the id is derived from the media and gated in
	// openRestoreCatalog instead (same gate, same point: before any work).
	if opts.setID != "" {
		if _, err := media.ImageDirName(opts.setID, 1); err != nil {
			return &report.UsageError{Command: cmd.CommandPath(), Err: err}
		}
	}
	explicit, err := parseRestoreImages(cmd, ro.images)
	if err != nil {
		return err
	}
	if fi, err := os.Stat(ro.target); err == nil && !fi.IsDir() {
		return &report.UsageError{
			Command: cmd.CommandPath(),
			Err:     fmt.Errorf("--target %q exists and is not a directory: %v", ro.target, fi.Mode()),
		}
	}

	// THE CATALOG: the --catalog file read-only (NO advisory flock, 2.1
	// Option B - this command must work during a running copy/build over a
	// WAL snapshot and performs ZERO catalog writes), or - with
	// --from-media - the media's FR-034 export, temp-copied and opened
	// read-only per the 8.5 flow documented in the file header. The
	// returned cleanup removes the export scratch (media runs only).
	db, setID, mediaInfo, cleanup, err := openRestoreCatalog(ctx, cmd, opts, ro)
	if err != nil {
		// Pre-bus failures classify through the SAME mapping the run
		// errors take: restore.InputError -> usage (2),
		// restore.MediaIntegrityError -> media class (3), and the
		// catalog classes pass through to report.Classify (4/5).
		return classifyRestoreError(cmd, err)
	}
	defer db.Close() //nolint:errcheck // best-effort; the run result dominates
	if cleanup != nil {
		defer cleanup()
	}

	// An unplanned set is the user's step error, not a silent no-op
	// (the verify-media/copy precedent).
	ids, err := db.CopyImageIDs(ctx, setID)
	if err != nil {
		// The MEDIA flow classifies every read of the export copy it just
		// opened and gate-checked as the catalog class (exit 4): an export
		// failure is never the generic fatal (catalog.MediaReadErr). The
		// plain --catalog path keeps its pre-existing class - it is out of
		// scope here, and aligning it would touch the whole tool's
		// foreign-catalog behavior (validate/report answer the same way).
		if ro.fromMedia != "" {
			err = db.MediaReadErr("listing the set's planned image ids from the media export", err)
		}
		return err
	}
	if len(ids) == 0 {
		return &report.UsageError{
			Command: cmd.CommandPath(),
			Msg: fmt.Sprintf("set %q has no planned images (run 'isopress scan' and 'isopress plan' first)",
				setID),
		}
	}

	// Fresh progress bus per operation (T1.1); renderer per the 9.2 matrix.
	// The deferred Finish/wait pair guarantees exactly one Done summary line
	// on EVERY exit path.
	bus := progress.NewBus()
	wait := progress.Start(progress.New(progress.Options{
		Quiet:  opts.quiet,
		Stderr: cmd.ErrOrStderr(),
	}), bus)
	summary := fmt.Sprintf("restore: set %s aborted before any image was processed", setID)
	defer func() {
		bus.Finish(progress.Done{Summary: summary, Failed: runFailed(runErr), Interrupted: runInterrupted(runErr)})
		wait()
	}()
	// The run-level heartbeat (HeartbeatInterval, build.go): a long ISO
	// extraction inside an image has no intra-member events at T5.4 yet, so
	// one Tick per interval keeps the phase alive. Registered AFTER the
	// Finish defer (LIFO: it stops before the terminal Done - a Tick can
	// never trail the summary).
	stopHeartbeat := startHeartbeat(ctx, bus)
	defer stopHeartbeat()

	// 8.5's manifest anchor, over the OPEN export copy (media runs only):
	// the media's MANIFEST.json against the export's recorded manifest
	// self-row - the honest extent of the media-alone chain, documented on
	// verifyMediaManifestAnchor (restore_media.go).
	if ro.fromMedia != "" {
		augmentManifestsWithExplicit(&mediaInfo, explicit)
		if err := verifyMediaManifestAnchor(ctx, db, mediaInfo, bus); err != nil {
			// CLASS CONSISTENCY (T5.5 fold-in): the anchor step's errors are
			// the SAME classes openRestoreCatalog returns - a
			// *restore.MediaIntegrityError (the manifest re-read failed on
			// resumed media whose export carries the self-row: a TOCTOU
			// manifest-vanish) must map to the media class (exit 3), and a
			// *catalog.CatalogError (the export's content list is
			// unreadable) must keep its class (exit 4) - so the error goes
			// through the SAME classifyRestoreError route every pre-work
			// error takes, never out raw (which would land the media class
			// as the generic fatal, exit 1).
			return classifyRestoreError(cmd, err)
		}
	}

	selector := restore.Selector{SetID: setID, Paths: ro.paths, Subtree: ro.subtree}
	// The catalog seam: the plain restoreStore over --catalog, or the
	// mediaStore over the export for a media run (the manifest
	// chunk-hash fill of the lazily-hashed-chunk finding, documented on
	// mediaStore in restore_media.go).
	var store restore.Store = &restoreStore{db: db}
	restorer := restoreRestorerFactory(restore.RestorerDeps{Bus: bus})
	if ro.fromMedia != "" {
		store = &mediaStore{restoreStore: &restoreStore{db: db}, manifests: mediaInfo.Manifests}
		// The fill's SUPPLY-FOLLOWING seat (restore_media.go): the
		// inventory above was built before supply resolution, so the
		// sidecar MANIFEST.json of whatever SourceCopyTree directory the
		// supply pass resolves for each image - including a
		// --copy-tree-root supplement or a prompted path - is consulted
		// here, just before that image is restored.
		restorer = newSidecarManifestRestorer(restorer)
	}
	res, err := restore.Run(ctx, restore.Input{
		Store:    store,
		Restorer: restorer,
		Selector: selector,
		Target:   ro.target,
		Supply: restore.SupplySpec{
			ImageDir:     opts.imageDir,
			CopyTreeRoot: ro.copyTreeRoot,
			Explicit:     explicit,
			Media:        mediaInfo.Trees, // nil for plain runs; the 8.5 base supply otherwise
		},
		WorkDir:  opts.workDir,
		Bus:      bus,
		Prompter: newRestorePrompter(cmd),
		ProbeISO: probeXorrisoOnce,
	})

	return finalizeRestore(ctx, &summary, restoreRun{
		cmd:       cmd,
		setID:     setID,
		selection: selector.Mode(),
		jsonFlag:  opts.json,
		strict:    opts.strict,
		res:       res,
	}, err)
}

// parseRestoreImages parses and tilde-gates every --image N=PATH value up
// front (paths.go: every path-typed input wires checkTilde), so a malformed
// mapping is a usage error naming the value before any catalog work.
func parseRestoreImages(cmd *cobra.Command, raw []string) ([]restore.ExplicitImage, error) {
	out := make([]restore.ExplicitImage, 0, len(raw))
	for _, v := range raw {
		e, err := restore.ParseExplicitImage(v)
		if err != nil {
			var input *restore.InputError
			if errors.As(err, &input) {
				return nil, &report.UsageError{Command: cmd.CommandPath(), Msg: input.Msg}
			}
			return nil, err
		}
		if err := checkTilde(e.Path, "--image"); err != nil {
			return nil, &report.UsageError{Command: cmd.CommandPath(), Err: err}
		}
		out = append(out, e)
	}
	return out, nil
}

// probeXorrisoOnce is the T4.1 dependency seam restore.Run calls once, before
// any image, when at least one supplied image is an ISO. Discover+Probe build
// the 10.1 DependencyError (exit 3) themselves, with the per-distro install
// hints; copy-tree runs never reach it.
func probeXorrisoOnce(ctx context.Context) error {
	tool, err := xorriso.Discover()
	if err != nil {
		return err
	}
	if _, err := tool.Probe(ctx); err != nil {
		return err
	}
	return nil
}

// --- the TTY gate and the prompt (8.3) ---------------------------------------

// newRestorePrompter returns the interactive prompter ONLY when stderr is a
// terminal (8.3's gate; the 9.2 detector applied to the command's own stderr
// writer, so an injected buffer counts as non-TTY exactly like a pipe). A nil
// result is what makes restore.Run take the machine-readable listing path.
func newRestorePrompter(cmd *cobra.Command) restore.Prompter {
	w := cmd.ErrOrStderr()
	f, ok := w.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return nil
	}
	return &restorePrompter{err: w, in: cmd.InOrStdin()}
}

// restorePrompter implements restore.Prompter over the command's streams: the
// 8.3 sentence on stderr, one line of input from stdin. The read runs in a
// goroutine so a SIGINT/SIGTERM (which cancels the command context) can
// abandon a parked prompt - the process exits anyway, and without this the
// first ^C at a prompt would have had to wait for a second signal.
type restorePrompter struct {
	err io.Writer
	in  io.Reader

	reader *bufio.Reader // one buffered reader across prompts (answers may arrive batched)
}

// promptText is DESIGN_SPEC 8.3's wording, verbatim (including the colon and
// the quoted 'q').
func promptText(req restore.PromptRequest) string {
	return fmt.Sprintf("Image %d of set %s is required for %s. Provide its path (or 'q' to abort):",
		req.Image, req.SetID, req.NeededFor)
}

// PromptImage implements restore.Prompter.
func (p *restorePrompter) PromptImage(ctx context.Context, req restore.PromptRequest) (string, error) {
	if p.reader == nil {
		p.reader = bufio.NewReader(p.in)
	}
	for {
		if _, err := fmt.Fprintln(p.err, promptText(req)); err != nil {
			return "", fmt.Errorf("%w: writing the prompt: %v", restore.ErrPromptClosed, err)
		}
		line, err := p.readLine(ctx)
		if err != nil {
			return "", err
		}
		answer := strings.TrimSpace(line)
		switch answer {
		case "":
			continue // a blank line re-asks; it is neither an answer nor an abort
		case "q", "Q":
			return "", restore.ErrPromptAborted
		default:
			return answer, nil
		}
	}
}

// readLine reads one line without letting an idle prompt block the signal
// path: the goroutine parks on stdin while the select honours ctx cancellation
// (see the type doc).
func (p *restorePrompter) readLine(ctx context.Context) (string, error) {
	type answer struct {
		line string
		err  error
	}
	ch := make(chan answer, 1)
	go func() {
		line, err := p.reader.ReadString('\n')
		ch <- answer{line: line, err: err}
	}()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case a := <-ch:
		if a.err != nil && a.line == "" {
			return "", fmt.Errorf("%w: %v", restore.ErrPromptClosed, a.err)
		}
		return a.line, nil // a trailing partial line at EOF is still an answer
	}
}

// --- the catalog seam (read-only, no SQL outside internal/catalog) -----------

// restoreStore adapts the READ-ONLY catalog handle onto restore.Store. Field
// for field, the mapping is mechanical (the cli is the one layer that knows
// both sides - DESIGN_SPEC 1.2, the scanStoreTx/copyengine precedent), and
// every read it exposes is a no-flock, no-transaction read.
type restoreStore struct {
	db *catalog.DB
}

// ActiveRecords maps catalog.ValidateRecord (the v_active_files read) onto
// the restore selection shape.
func (s *restoreStore) ActiveRecords(ctx context.Context, setID string) ([]restore.Record, error) {
	recs, err := s.db.ActiveRecords(ctx, setID)
	if err != nil {
		return nil, err
	}
	out := make([]restore.Record, 0, len(recs))
	for _, r := range recs {
		out = append(out, restore.Record{
			FileID:     r.FileID,
			RelPath:    r.RelPath,
			EntryKind:  r.EntryKind,
			Size:       r.Size,
			Hash:       r.Hash,
			LinkTarget: r.LinkTarget,
		})
	}
	return out, nil
}

// ActiveDirectories maps catalog.ActiveDir onto the restore selection shape.
func (s *restoreStore) ActiveDirectories(ctx context.Context, setID string) ([]restore.DirRecord, error) {
	dirs, err := s.db.ActiveDirs(ctx, setID)
	if err != nil {
		return nil, err
	}
	out := make([]restore.DirRecord, 0, len(dirs))
	for _, d := range dirs {
		out = append(out, restore.DirRecord{DirID: d.DirID, RelPath: d.RelPath})
	}
	return out, nil
}

// Placement maps catalog.CopyPlacement (the placement summary with the unified
// secondary-claim rule already applied) onto the restore shape.
func (s *restoreStore) Placement(ctx context.Context, setID string) ([]restore.ImagePaths, error) {
	places, err := s.db.CopyPlacement(ctx, setID)
	if err != nil {
		return nil, err
	}
	out := make([]restore.ImagePaths, 0, len(places))
	for _, p := range places {
		out = append(out, restore.ImagePaths{ImageID: p.ImageID, Paths: p.Paths})
	}
	return out, nil
}

// ImageInventory maps catalog.CopyInventory onto the restore work-list shape.
// The copied flags of the catalog shape are deliberately dropped: restore has
// no resume state (there is no restore-state in the schema, so the flag means
// nothing to this run).
func (s *restoreStore) ImageInventory(ctx context.Context, setID string, imageID int64) (restore.Inventory, error) {
	inv, err := s.db.CopyInventory(ctx, setID, imageID)
	if err != nil {
		return restore.Inventory{}, err
	}
	out := restore.Inventory{
		Files:       make([]restore.FileItem, 0, len(inv.Files)),
		Symlinks:    make([]restore.SymlinkItem, 0, len(inv.Symlinks)),
		Chunks:      make([]restore.ChunkItem, 0, len(inv.Chunks)),
		Dirs:        make([]restore.DirItem, 0, len(inv.Dirs)),
		Secondaries: make([]restore.LinkItem, 0, len(inv.Secondaries)),
	}
	for _, f := range inv.Files {
		out.Files = append(out.Files, restore.FileItem{
			FileID: f.FileID, RelPath: f.RelPath, Size: f.Size,
			Mode: f.Mode, UID: f.UID, GID: f.GID, MtimeNS: f.MtimeNS,
			Dev: f.Dev, Inode: f.Inode, Nlink: f.Nlink, Hash: f.Hash,
			Chunked: f.Chunked, ChunkCnt: f.ChunkCnt,
		})
	}
	for _, sy := range inv.Symlinks {
		out.Symlinks = append(out.Symlinks, restore.SymlinkItem{
			FileID: sy.FileID, RelPath: sy.RelPath, LinkTarget: sy.LinkTarget,
			Size: sy.Size, Mode: sy.Mode, UID: sy.UID, GID: sy.GID, MtimeNS: sy.MtimeNS,
		})
	}
	for _, c := range inv.Chunks {
		out.Chunks = append(out.Chunks, restore.ChunkItem{
			ChunkID: c.ChunkID, FileID: c.FileID, ChunkIndex: c.ChunkIndex,
			Offset: c.Offset, Length: c.Length, Hash: c.Hash,
			ParentRelPath: c.ParentRelPath, ParentSize: c.ParentSize,
			ParentMode: c.ParentMode, ParentUID: c.ParentUID, ParentGID: c.ParentGID,
			ParentMtimeNS: c.ParentMtimeNS, ParentDev: c.ParentDev, ParentInode: c.ParentInode,
			ParentHash: c.ParentHash, ParentNumChunks: c.ParentNumChunks,
		})
	}
	for _, d := range inv.Dirs {
		out.Dirs = append(out.Dirs, restore.DirItem{
			DirID: d.DirID, RelPath: d.RelPath, Mode: d.Mode,
			UID: d.UID, GID: d.GID, MtimeNS: d.MtimeNS,
		})
	}
	for _, l := range inv.Secondaries {
		out.Secondaries = append(out.Secondaries, restore.LinkItem{
			FileID: l.FileID, RelPath: l.RelPath, Size: l.Size,
			Mode: l.Mode, UID: l.UID, GID: l.GID, MtimeNS: l.MtimeNS,
			Hash: l.Hash, PrimaryFileID: l.PrimaryFileID, PrimaryRelPath: l.PrimaryRelPath,
		})
	}
	return out, nil
}

// --- error classification (10.1 -> 10.2) -------------------------------------

// classifyRestoreError maps the restore package's typed errors onto the 10.2
// scheme. It is called with the run error from restore.Run and returns the
// error the command should return (already typed for report.Classify).
func classifyRestoreError(cmd *cobra.Command, err error) error {
	if err == nil {
		return nil
	}
	var input *restore.InputError
	if errors.As(err, &input) {
		// Selection/supply INPUT failures are the user's flag mistake:
		// usage class, exit 2 (the same route every other InputError-ish
		// validation in this command takes).
		return &report.UsageError{Command: cmd.CommandPath(), Msg: input.Msg}
	}
	var missing *restore.MissingImagesError
	if errors.As(err, &missing) {
		nums := make([]int64, 0, len(missing.Missing))
		for _, m := range missing.Missing {
			nums = append(nums, m.Image)
		}
		return &report.DependencyError{
			Missing: fmt.Sprintf("required backup media (image %s of set %s)", joinInt64s(nums), setOfFirstMissing(missing)),
			Detail: "stderr is not a terminal, so no prompt was issued (DESIGN_SPEC 8.3 / RCR-3); " +
				"see the required-image lines above and supply them with --image-dir, --image N=PATH, or --copy-tree-root",
		}
	}
	var mediaIntegrity *restore.MediaIntegrityError
	if errors.As(err, &mediaIntegrity) {
		// Media content that cannot be trusted (a MANIFEST.json that does
		// not parse; a manifest/export set-id disagreement): the media
		// class again - exit 3, "supply other or repaired media" - never
		// a silent proceed on an anchor that failed (8.5).
		return &report.DependencyError{
			Missing: "backup media with a trustworthy " + media.ManifestFileName,
			Detail:  mediaIntegrity.Error(),
		}
	}
	var aborted *restore.AbortedError
	if errors.As(err, &aborted) {
		return &report.DependencyError{
			Missing: fmt.Sprintf("required backup media (image %d of set %s)", aborted.Image, aborted.SetID),
			Detail: fmt.Sprintf("the supply prompt ended without a path (%s); nothing was restored and the target was not written",
				aborted.Reason),
		}
	}
	return err
}

// setOfFirstMissing names the set of a missing-media error (an empty Missing
// slice cannot happen, but the message must not panic if it ever does).
func setOfFirstMissing(m *restore.MissingImagesError) string {
	if len(m.Missing) == 0 {
		return "(unknown)"
	}
	return m.Missing[0].SetID
}

// --- the report model (human and --json; the key set is FROZEN) ----------------

// restoreFileFinding is one member that did NOT come back restored (skipped or
// failed), as the seam recorded it. The JSON key set is a deliberate frozen
// schema (the scan/plan/copy/build/verify-media report precedent): new keys
// are a conscious schema change with the pinning tests updated in the same
// commit. Clean restores are COUNTED, never listed - the validate rationale
// (a large clean restore must not print one line per file into a cron log).
type restoreFileFinding struct {
	Path   string `json:"path"`
	Status string `json:"status"` // skipped | failed
	Bytes  int64  `json:"bytes"`
	Detail string `json:"detail,omitempty"`
}

// restoreImageReport is one image's outcome line of the report.
type restoreImageReport struct {
	Image    int64  `json:"image"`
	Path     string `json:"path"`
	Source   string `json:"source"` // "iso" | "copy" | "" (never supplied)
	Status   string `json:"status"` // restored | partial | failed
	Restored int    `json:"restored"`
	Skipped  int    `json:"skipped"`
	Failed   int    `json:"failed"`
	Bytes    int64  `json:"bytes"`
	// Failure is the image-level seam error (one entry, the verify-media
	// per-image failure shape); the image's own counts stay whatever the
	// seam reported.
	Failure string               `json:"failure,omitempty"`
	Files   []restoreFileFinding `json:"files"`
}

// restoreReport is the end-of-run report (human lines and --json are two
// presentations of the same numbers, FR-046: stdout only).
type restoreReport struct {
	SetID  string `json:"set_id"`
	Target string `json:"target"`
	// Selection is the selector vocabulary: whole-set | path | subtree |
	// path+subtree (the run's selector mode - material to interpreting the
	// totals).
	Selection string `json:"selection"`
	Status    string `json:"status"` // ok | partial | failed | interrupted (runs.status spelling)
	// Files and Bytes are the SELECTION totals (8.4's determinate restore
	// progress denominators): selected records and their recorded sizes.
	Files int64 `json:"files"`
	Bytes int64 `json:"bytes"`
	// Restored / Skipped / Failed are the summed per-image outcomes.
	Restored int `json:"restored"`
	Skipped  int `json:"skipped"`
	Failed   int `json:"failed"`
	// Unclaimed are selected paths no image claims (scanned after the last
	// plan, never planned): honest failures of this run. Never nil - always
	// at least [] - so the frozen top-level key set is byte-identical for
	// clean and dirty runs alike (the validate report convention).
	Unclaimed []string             `json:"unclaimed"`
	Images    []restoreImageReport `json:"images"`
}

// restoreRun carries one run's inputs and outcome to finalize.
type restoreRun struct {
	cmd       *cobra.Command
	setID     string
	selection string
	jsonFlag  bool
	strict    bool
	res       restore.Result
}

// restoreOutcome folds the per-image counts and the partial-failure decision
// in ONE place (single goroutine: the image loop is sequential).
type restoreOutcome struct {
	restored int
	skipped  int
	failed   int
	// imageFailures counts images whose seam call errored; together with
	// skipped/failed/unclaimed they make the run partial (exit 10).
	imageFailures int
}

// finalizeRestore lands the report on stdout (except on the supply-abort
// paths, where nothing ran and stdout stays empty - see the file doc) and
// classifies the command result:
// interrupted (130) > terminal error > partial (10, or 1 with --strict) > nil
// (0) - the 10.2 semantics documented in the file doc. summary is written back
// through the pointer so the deferred Done emission carries the final numbers
// (the scan.go convention).
func finalizeRestore(ctx context.Context, summary *string, r restoreRun, runErr error) error {
	// The abort-BEFORE-WORK classes print NO report and take their own exit
	// class: nothing was processed, so there is no run to describe (the
	// tool's standing abort-before-work rule: stdout stays empty). That
	// covers the selection/supply INPUT failures, the two media-supply
	// aborts, AND a DEPENDENCY refusal - the up-front xorriso probe fires
	// before the first image, so its refusal too leaves nothing to report.
	var inputErr *restore.InputError
	var missing *restore.MissingImagesError
	var aborted *restore.AbortedError
	var mediaIntegrity *restore.MediaIntegrityError
	var depErr *report.DependencyError
	if errors.As(runErr, &inputErr) || errors.As(runErr, &missing) ||
		errors.As(runErr, &aborted) || errors.As(runErr, &mediaIntegrity) || errors.As(runErr, &depErr) {
		switch {
		case errors.As(runErr, &missing):
			*summary = fmt.Sprintf("restore: set %s aborted: %d required image(s) unavailable (no prompt: stderr is not a terminal)",
				r.setID, len(missing.Missing))
		case errors.As(runErr, &aborted):
			*summary = fmt.Sprintf("restore: set %s aborted at the supply prompt: nothing was restored", r.setID)
		case errors.As(runErr, &mediaIntegrity):
			*summary = fmt.Sprintf("restore: aborted: media integrity failure: %v", mediaIntegrity)
		case errors.As(runErr, &inputErr):
			*summary = fmt.Sprintf("restore: set %s aborted: %v", r.setID, inputErr)
		default:
			*summary = fmt.Sprintf("restore: set %s aborted before any image was processed: %v", r.setID, depErr)
		}
		return classifyRestoreError(r.cmd, runErr)
	}

	out := foldRestoreOutcome(r.res)
	unclaimed := r.res.Selection.Unclaimed
	rep := buildRestoreReport(r, out, unclaimed)
	interrupted := ctx.Err() != nil
	switch {
	case interrupted:
		rep.Status = "interrupted"
	case runErr != nil:
		rep.Status = "failed"
	case isPartialRestore(out, unclaimed):
		rep.Status = "partial"
	default:
		rep.Status = "ok"
	}

	if err := writeRestoreReport(r.cmd.OutOrStdout(), rep, r.jsonFlag); err != nil {
		runErr = errors.Join(runErr, err)
	}

	*summary = fmt.Sprintf(
		"restore: set %s: %d image(s), selected %d file(s) (%s), restored %d, skipped %d, failed %d, unclaimed %d (status %s)",
		r.setID, len(r.res.Images), r.res.Selection.Files, units.FormatBytes(r.res.Selection.Bytes),
		out.restored, out.skipped, out.failed, len(unclaimed), rep.Status)

	switch {
	case interrupted:
		return fmt.Errorf("%w: restore of set %s interrupted after %d of %d image(s); the report above is partial",
			report.ErrInterrupted, r.setID, len(r.res.Images), len(r.res.Selection.Work))
	case runErr != nil:
		// Terminal errors (store reads, workspace, xorriso probe) keep their
		// own class; the report above still records what DID restore.
		return runErr
	}
	if isPartialRestore(out, unclaimed) {
		if r.strict {
			return fmt.Errorf("restore completed with unrestored members; --strict promotes this to a fatal error")
		}
		return &report.ItemFailures{Failures: restoreFailureLines(r.res)}
	}
	return nil
}

// foldRestoreOutcome sums the per-image seam counts and the image-level
// failures.
func foldRestoreOutcome(res restore.Result) restoreOutcome {
	var out restoreOutcome
	for _, img := range res.Images {
		if img.Err != nil {
			out.imageFailures++
		}
		out.restored += img.Out.Restored
		out.skipped += img.Out.Skipped
		out.failed += img.Out.Failed
	}
	return out
}

// isPartialRestore is the exit-10 decision: ANY selected member left
// unrestored (skipped or failed), ANY image whose seam call errored, or ANY
// selected path no image claims. A skip counts because a restore that did not
// put every selected name back on disk is partial by definition - automation
// must see it (10.2 partial-failure semantics).
func isPartialRestore(out restoreOutcome, unclaimed []string) bool {
	return out.skipped > 0 || out.failed > 0 || out.imageFailures > 0 || len(unclaimed) > 0
}

// buildRestoreReport assembles the frozen-schema report from the run result.
func buildRestoreReport(r restoreRun, out restoreOutcome, unclaimed []string) restoreReport {
	rep := restoreReport{
		SetID:     r.setID,
		Target:    r.res.Selection.Target,
		Selection: r.selection,
		Files:     r.res.Selection.Files,
		Bytes:     r.res.Selection.Bytes,
		Restored:  out.restored,
		Skipped:   out.skipped,
		Failed:    out.failed,
		Unclaimed: append([]string{}, unclaimed...),
		Images:    make([]restoreImageReport, 0, len(r.res.Images)),
	}
	for _, img := range r.res.Images {
		irep := restoreImageReport{
			Image:    img.Image,
			Path:     img.Path,
			Source:   string(img.Kind),
			Restored: img.Out.Restored,
			Skipped:  img.Out.Skipped,
			Failed:   img.Out.Failed,
			Bytes:    img.Out.Bytes,
			Files:    make([]restoreFileFinding, 0, len(img.Out.Files)),
		}
		for _, f := range img.Out.Files {
			if f.Status == "restored" {
				continue // counted, never listed (the family convention)
			}
			irep.Files = append(irep.Files, restoreFileFinding{
				Path: f.RelPath, Status: f.Status, Bytes: f.Bytes, Detail: f.Detail,
			})
		}
		switch {
		case img.Err != nil:
			irep.Status = "failed"
			irep.Failure = img.Err.Error()
		case irep.Failed > 0 || irep.Skipped > 0:
			irep.Status = "partial"
		default:
			irep.Status = "restored"
		}
		rep.Images = append(rep.Images, irep)
	}
	return rep
}

// restoreFailureLines builds the exit-10 carrier's entries: image-level
// failures first (one line each, the verify-media shape), then every
// non-restored member, then the unclaimed paths.
func restoreFailureLines(res restore.Result) []error {
	var errs []error
	for _, img := range res.Images {
		if img.Err != nil {
			errs = append(errs, fmt.Errorf("restore: image %d: %v", img.Image, img.Err))
		}
		for _, f := range img.Out.Files {
			if f.Status == "restored" {
				continue
			}
			if f.Detail != "" {
				errs = append(errs, fmt.Errorf("restore: %s: %s (%s)", f.RelPath, f.Status, f.Detail))
				continue
			}
			errs = append(errs, fmt.Errorf("restore: %s: %s", f.RelPath, f.Status))
		}
	}
	for _, rel := range res.Selection.Unclaimed {
		errs = append(errs, fmt.Errorf("restore: %s: not assigned to any image (run 'isopress plan' and 'isopress copy' first)", rel))
	}
	return errs
}

// writeRestoreReport renders the report: --json emits exactly one JSON
// document; the human form is the set identity line, the target/selection
// line, one line per image with its counts, that image's non-restored member
// lines and failure detail, any unclaimed paths, and the totals line (the
// plan/scan/copy/build/verify-media family shape).
func writeRestoreReport(w io.Writer, rep restoreReport, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	if _, err := fmt.Fprintf(w, "restore: set %s\n", rep.SetID); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "target: %s (selection: %s)\n", rep.Target, rep.Selection); err != nil {
		return err
	}
	for _, img := range rep.Images {
		if _, err := fmt.Fprintf(w, "image %d: %s %s (%s) - restored %d, skipped %d, failed %d (%s)\n",
			img.Image, img.Status, img.Path, sourceLabel(img.Source),
			img.Restored, img.Skipped, img.Failed, units.FormatBytes(img.Bytes)); err != nil {
			return err
		}
		for _, f := range img.Files {
			if f.Detail != "" {
				if _, err := fmt.Fprintf(w, "  %s: %s (%s)\n", f.Status, f.Path, f.Detail); err != nil {
					return err
				}
				continue
			}
			if _, err := fmt.Fprintf(w, "  %s: %s\n", f.Status, f.Path); err != nil {
				return err
			}
		}
		if img.Failure != "" {
			if _, err := fmt.Fprintf(w, "  %s\n", img.Failure); err != nil {
				return err
			}
		}
	}
	for _, rel := range rep.Unclaimed {
		if _, err := fmt.Fprintf(w,
			"unclaimed: %s (not assigned to any image; run 'isopress plan' and 'isopress copy' first)\n", rel); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w,
		"totals: images %d, selected %d file(s) (%s), restored %d, skipped %d, failed %d, unclaimed %d (status: %s)\n",
		len(rep.Images), rep.Files, units.FormatBytes(rep.Bytes), rep.Restored, rep.Skipped, rep.Failed,
		len(rep.Unclaimed), rep.Status)
	return err
}
