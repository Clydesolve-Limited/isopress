package imagebuild

// T4.3: the build phase - stage, embed, content-list transaction,
// master, self-verify, atomic finalize (DESIGN_SPEC 6.2 invocation,
// 6.3 atomic finalize and image verification, 6.4 naming, 6.6
// per-image content list, 7.1/7.2 extraction-based re-hash;
// IMPLEMENTATION_PLAN T4.3).
//
// Build runs the DESIGN_SPEC 6.3 flow as numbered, for ONE image:
//
//	(0) validateInput gates + the F2 set-id gate + ctx check (F4 #1)
//	(1) Stage (T4.2, reused as-is; staging is the 4.3 first write)
//	(2) embed MANIFEST.json + catalog-export.sqlite (5.1 discipline
//	    minus the mark, copyengine.EmbedFile) into the staging tree
//	(3) THE 6.6 TRANSACTION: replace the image's image_contents rows
//	    with the FINAL content list AND set state 'building' - one
//	    catalog transaction (Store.BeginBuild)
//	(4) extend the pathlist with the two image-root graft entries
//	(5) ctx check (F4 #2 - right before the exec)
//	(6) xorriso master via the T4.1 Invocation template -> .partial
//	(7) SELF-VERIFY (6.3 step 2, 7.1): extract the .partial via the
//	    internal/verify primitive, hash every member, compare against
//	    the step-3 content list
//	(8) ATOMIC FINALIZE: fsync the .partial, rename to
//	    <dest>/<SET-ID>-NNN.iso, fsync the dir, state 'built' +
//	    built_at (Store.FinishBuild)
//
// Any failure after step 3 (xorriso non-zero, hook, verification
// mismatch, interruption) takes ONE failure path: state 'failed'
// (Store.MarkFailed, with the transcript tail in run_events), the
// .partial kept for inspection unless CleanFailed, the typed error
// returned for the cli's ItemFailures accounting (the N3 wiring
// contract: SourceChangedError from step 1 and build failures are both
// per-image failures; the cli loop continues other images under --all
// and exits 10).
//
// # Progress: FOUR NAMED PHASES, determinate where known (FR-042,
// DESIGN_SPEC 6.2's 2026-10-20 "looks hung" amendment)
//
// Build owns the phase sequence through the Bus (Deps.Bus, nil-safe);
// every phase opens DETERMINATE because every total is knowable up
// front:
//
//	build(stage)   - per-file ItemDone against the inventory totals
//	                 (the T4.2 staging events under this Phase).
//	build(master)  - the RequiredImageBytes-estimate Phase plus a
//	                 byte-watcher on the growing .partial (watch.go),
//	                 emitting BytesWritten{absolute size} while xorriso
//	                 runs. NO xorriso-output parsing.
//	build(extract) - emitted by internal/verify around the osirrox
//	                 extraction, driven by a tree-sum byte-watcher
//	                 against the EXACT content-list byte total (the
//	                 honest indeterminate fallback for pathological
//	                 file counts lives there, verify's watch.go).
//	build(verify)  - emitted by internal/verify BEFORE the re-hash
//	                 walk, exact totals, then per-entry ItemDone.
//
// The saturate-and-hold clamp (a byte position meeting or exceeding
// its total holds at 100%, no rollback, no >100%) is the RENDERER's
// concern (internal/progress); this package always reports raw
// absolute positions and exact totals. The Failed/Interrupted
// clear-the-bar contract applies to all four phases alike.
//
// # Dependency seams (DESIGN_SPEC 1.2: services depend on interfaces,
// never on os/exec; the T4.1 exec site stays singular)
//
// Master and Extractor are INTERFACES over the xorriso wrapper: the
// cli layer adapts one *xorriso.Tool onto both (Master.Run returns the
// captured transcript; Extractor wraps verify.ExtractAndHash). This
// package imports internal/xorriso ONLY for the typed Invocation
// argument builder (T4.1's 6.2 template - the argv contract, which
// T4.1's own docs assign to "the imagebuild service" as caller); it
// never imports os/exec and never spawns a process directly. Store is
// the catalog seam (imagebuild.BuildStore, primitive-shaped like
// copyengine.Store; *catalog.BuildStore is adapted onto it at the cli
// layer). FreeSpace/Source/HashStore/Bus/OpenPartial carry over from
// T4.2's staging seams unchanged.
//
// # The --verify-after-build V1 reading (documented decision)
//
// DESIGN_SPEC 6.3 step 3 gates a SECOND verify-media run on
// "--verify-after-build (default on for optical profiles)";
// verify-media is T5. The honest V1 semantics shipped here: the flag
// gates THE STEP-7 SELF-VERIFY PASS itself. Default ON: every build
// extracts and re-hashes its .partial against the content list before
// the rename (6.3 step 2's guarantee). --verify-after-build=false
// SKIPS step 7 for speed - the build then renames and marks 'built'
// WITHOUT the extraction guarantee, trading FR-020's "verify against a
// finalized content list" for time on huge images; the report marks
// such images (BuildResult.VerifySkipped). When T5.1 lands, the flag
// gains its full 6.3-step-3 meaning (an additional verify-media run
// after finalize) and this skip-mode remains documented as the
// deliberate speed trade it is.
//
// # Interruption and crash states (documented decisions)
//
//   - Cancel BEFORE step 3: state never left 'planned' (F4 #1 checks
//     ctx before Stage; Stage itself is cancel-aware and removes its
//     scratch).
//   - Cancel between step 3 and the exec (F4 #2 - the T4.2
//     cancel-after-last-member window: Stage returned a COMPLETE tree
//     under a cancelled ctx, so an unchecked ctx would master an image
//     from a cancelled run): Build refuses to exec and takes the
//     failure path - state 'building' -> 'failed' via the DETACHED
//     mark (a cancelled ctx would be rejected by the driver; the
//     5-second background context of copyengine's markContext shape).
//   - Cancel DURING xorriso: T4.1's Tool.Run kills the process and
//     returns the raw ctx error; same detached failure path. There is
//     never a final .iso, and the .partial is kept (an interrupted
//     build is inspection-worthy; CleanFailed still applies if the
//     user asked for cleanup).
//   - Crash between rename and FinishBuild: a valid .iso exists whose
//     catalog state says 'building'. The next build run re-stages and
//     rebuilds from scratch (there is no build resume); its BeginBuild
//     transaction replaces the content rows and flips the state again,
//     so the debris self-heals on rebuild. (The 12.7-style reconciliation
//     of a BUILT file with a stale 'building' row is T5.1 scope.)
//
// # Volume id and naming (6.4)
//
// The -V value is "<SET-ID>-<NNN>": the set id plus the SAME <NNN>
// the file name carries (media.ImageISOFileName), so the discs of a
// multi-disc set mount with distinguishable labels (6.4, AMENDED
// 2026-11-04 by the stakeholder; before the amendment -V was the set
// id alone). The label is capped at 32 BYTES (the ISO 9660 volume-id
// limit, volumeIDMax): when set id plus suffix would overflow, the
// SET-ID portion is byte-truncated to 32-len(suffix) bytes
// (deterministic - a multi-byte rune may be cut, which is inert for a
// volume label) and the suffix is appended LAST, so the image number
// always survives. The output path is media.ImageISOFileName:
// <dest>/<SET-ID>-NNN.iso.partial while mastering, renamed to
// <dest>/<SET-ID>-NNN.iso at step 8.
//
// The F2 gate (the T4.2 review carry-in): a SET-ID containing '\n' or
// '\r' is refused at build entry with a Usage-class error - the byte
// breaks BOTH the -V label (it travels into the PVD as a line break)
// and the -o FILENAME (a newline in the output path shatters every
// downstream tool and the 6.4 naming scheme). media.validateSetID
// allows newlines (a newline is a legal Linux filename byte for a
// COPY-tree directory), so ISO mode gates it here. A '=' in the set id
// is DELIBERATELY ALLOWED: xorriso -V takes free text as one argv
// element (no parsing, no injection surface - the T4.1 no-shell argv
// contract), and a '=' in the -o filename is a legal Linux byte.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"isopress/internal/copyengine"
	"isopress/internal/fsmeta"
	"isopress/internal/media"
	"isopress/internal/progress"
	"isopress/internal/report"
	"isopress/internal/units"
	"isopress/internal/verify"
	"isopress/internal/xorriso"
)

const (
	// volumeIDMax is the ISO 9660 volume-id limit in BYTES (6.4: the
	// volume id is "truncated to 32 characters"). Since the
	// 2026-11-04 amendment the id is "<SET-ID>-<NNN>" and only the
	// set-id portion yields bytes when the cap bites - see volumeID.
	volumeIDMax = 32
	// extractDirPrefix is the os.MkdirTemp pattern of the step-7
	// extraction scratch under --work-dir. It carries no user data
	// (set ids are user-controlled), exactly like stagingDirPrefix.
	extractDirPrefix = "isopress-imageverify-"
	// extractSlack is the free-space cushion added to the step-7
	// extraction pre-check (directory entries, xorriso scratch).
	extractSlack = 16 << 20 // 16 MiB
	// finalizeDetachTimeout bounds the detached state writes (the
	// MarkFailed/FinishBuild transactions after the run context was
	// cancelled) - copyengine.MarkDetachTimeout's shape: the write is
	// tiny, the flock excludes competing writers.
	finalizeDetachTimeout = 5 * time.Second
	// transcriptDetailCap bounds the xorriso transcript tail folded
	// into the MarkFailed detail (the run_events row). The catalog
	// caps its own rows at 64 KiB; Build trims before passing so the
	// cause line stays visible at the HEAD of the detail.
	transcriptDetailCap = 32 << 10 // 32 KiB
	// verifyProblemCap bounds the per-entry problems listed in one
	// verification-mismatch error (a wholesale corruption reports
	// every member; the first N lines carry the diagnosis).
	verifyProblemCap = 20
)

// The two image-root self-member kinds of the 6.6 content list (the
// media.Kind* payload kinds cover the rest). T5.1: the spellings live
// in internal/verify (with the comparison that classifies them); the
// unexported names stay for the content-list derivation.
const (
	kindManifest = verify.KindManifest
	kindExport   = verify.KindExport
)

// The build command's four named progress phases (DESIGN_SPEC 6.2
// four-phase progress; 9.3 pins the plain-line formats keyed on these
// names). build(stage) and build(master) are emitted HERE (the build
// phase owns both); build(extract) and build(verify) are emitted by the
// internal/verify primitive (verify.PhaseExtract / verify.PhaseVerify),
// which owns the extraction-to-hash boundary.
const (
	PhaseStage  = "build(stage)"
	PhaseMaster = "build(master)"
)

// Master is the mastering seam over the xorriso wrapper (DESIGN_SPEC
// 1.2: services depend on an interface, never on os/exec or the
// concrete driver). The cli layer adapts *xorriso.Tool onto it:
// Run receives the T4.1 Invocation argv verbatim and returns the
// captured combined output; a non-zero exit returns the typed
// *xorriso.BuildError through the interface (Build treats every error
// identically - state 'failed', .partial kept - so no type assertion
// is needed here).
type Master interface {
	Run(ctx context.Context, args []string) (output []byte, err error)
}

// Extractor is the self-verify seam (the internal/verify primitive,
// adapted by the cli layer over the same *xorriso.Tool). Bus and
// totals flow through so the primitive emits the build(extract) and
// build(verify) progress events with the caller's EXACT content-list
// figures (DESIGN_SPEC 6.2 four-phase progress; verify.Totals are
// first-class parameters - T5.1's verify-media passes the same totals
// later). Both are nil/zero-safe: a nil Bus means no events.
type Extractor interface {
	ExtractAndHash(ctx context.Context, imagePath, destDir string, bus *progress.Bus, totals verify.Totals) (verify.ExtractResult, error)
}

// ContentRow is one FINAL image_contents row the builder computes per
// 6.6 (the 2.3 column set; the catalog implementation validates it
// again at insert time; FileID 0 means "no file" - dir, manifest,
// export rows - and ChunkIndex -1 means "not a chunk").
//
// It is a TYPE ALIAS for verify.ContentRow: T5.1's §5.5
// generalization moved the comparison - and with it the row shape it
// consumes - into internal/verify, the layer-correct direction
// (imagebuild already imports verify; verify never imports
// imagebuild). The alias keeps this package's content-list derivation
// and the T4.3 pin tests source-compatible.
type ContentRow = verify.ContentRow

// BuildStore is the catalog seam of the build phase (primitive-shaped
// so neither this package nor the catalog imports the other - the
// copyengine.Store precedent; *catalog.BuildStore is adapted onto it
// at the cli layer).
type BuildStore interface {
	// ChunkHashes returns the STORED hash of each requested chunk
	// (parallel slices), read AFTER staging so the 4.3 first-write
	// captures of step 1 are visible. A missing or malformed hash is
	// a loud error naming the chunk.
	ChunkHashes(ctx context.Context, fileIDs []int64, chunkIndexes []int) ([][]byte, error)
	// BeginBuild runs THE 6.6 TRANSACTION: replace the image's
	// image_contents rows with the FINAL list AND set state
	// 'building', atomically.
	BeginBuild(ctx context.Context, rows []ContentRow) error
	// MarkFailed is the post-'building' failure transaction: state
	// 'failed' plus the run_events error row carrying the detail.
	MarkFailed(ctx context.Context, detail string) error
	// FinishBuild is the 6.3 step-3 finalize transaction: state
	// 'built', built_at, final_path, bytes_written.
	FinishBuild(ctx context.Context, finalPath string, builtAt int64, bytesWritten int64) error
}

// EmbeddedExport is the run's single FR-034 catalog export, produced ONCE
// per build RUN by the cli (os.MkdirTemp + catalog.ExportSnapshot, the
// T3.2 fresh-path contract) and embedded identically into every image
// of the run (FR-035). Path/Size/Hash describe the produced file; Build
// embeds it with copyengine.EmbedFile's read-back discipline and
// re-checks the published hash against Hash.
type EmbeddedExport struct {
	Path string
	Size int64
	Hash []byte // 32-byte sha256 of the produced bytes
}

// ManifestSeed carries the manifest inputs Build CANNOT derive from the
// staging inventory: the injected clock and versions, and the whole-set
// derivations the cli fetched from the catalog (placement per 6.6 /
// FR-033, D-04 hard-link groups, and the per-parent reassembly
// anchors). Build assembles the media.ManifestInput entries from the
// SAME StageInventory structs staging consumed (plus the post-stage
// chunk hashes), so the manifest and the step-3 content list can never
// disagree about which members the image holds.
type ManifestSeed struct {
	ToolVersion    string                     // pass catalog.ToolVersion
	SchemaVersion  int                        // pass catalog.SchemaVersion
	CreatedAt      time.Time                  // injected clock (RFC 3339 Nano UTC in the manifest)
	ChunkedFiles   []media.ChunkedFileInput   // one per chunked parent touching this image
	HardLinkGroups []media.HardLinkGroupInput // the D-04 families of the whole set
	Placement      []media.PlacementInput     // the whole-set placement summary
}

// BuildInput is everything Build needs for one image: row-shaped data
// the cli read (the copyengine contract, DESIGN_SPEC 1.2), the run's
// export, the manifest seed, and the catalog seam. No catalog handle,
// no xorriso tool.
type BuildInput struct {
	// SetID and ImageID identify the image (6.4). SetID travels into
	// -V (as "<SetID>-<NNN>", truncated to volumeIDMax bytes) and
	// the output filename.
	SetID   string
	ImageID int64
	// SourceRoot is the absolute path inventory relpaths resolve
	// against (sets.source_root).
	SourceRoot string
	// WorkDir is the --work-dir value: the parent of the staging and
	// extraction scratches. Empty means the OS temp directory (6.2).
	WorkDir string
	// DestDir is the directory the finalized <SET-ID>-NNN.iso lands
	// in (--dest). Created if missing.
	DestDir string
	// UsableSize is the image's usable_size (the 6.3 step 5 pre-check
	// inside Stage).
	UsableSize int64
	// CleanFailed implements 6.3 step 4's flag: remove the .partial
	// on failure instead of keeping it for inspection.
	CleanFailed bool
	// VerifyAfterBuild gates the step-7 self-verify (see the file doc
	// for the V1 reading). Default true at the cli layer.
	VerifyAfterBuild bool
	// Mode is the run's resolved change-detection mode (DESIGN_SPEC
	// 3.5), threaded into staging (StageInput.Mode) so the 3.5 point 3
	// comparisons share the ONE mode the run resolved from its source
	// root. Zero = the full set (safe default).
	Mode fsmeta.Mode
	// Inventory is the payload to stage (the D-04 ISO rule: no
	// hard-link secondaries - the CLI derives StageInventory from
	// catalog.CopyInventory by dropping them).
	Inventory StageInventory
	// Export is the run's single catalog export to embed as
	// catalog-export.sqlite in EVERY image of the run.
	Export EmbeddedExport
	// Manifest carries the injected versions/clock and the whole-set
	// derivations for the per-image MANIFEST.json.
	Manifest ManifestSeed
	// Store is the catalog seam (required).
	Store BuildStore
}

// BuildResult is the successful outcome of one image build.
type BuildResult struct {
	// ISOPath is the finalized <dest>/<SET-ID>-NNN.iso.
	ISOPath string
	// PartialPath is the mastering path that was renamed (echoed for
	// reports; it no longer exists on success).
	PartialPath string
	// BytesWritten is the finalized image size.
	BytesWritten int64
	// ContentEntries is the number of FINAL image_contents rows the
	// step-3 transaction wrote (payload + manifest + export).
	ContentEntries int
	// VerifiedEntries is the number of content entries the step-7
	// extraction verified (equal to ContentEntries on a verified
	// build; 0 when verification was skipped).
	VerifiedEntries int
	// VerifySkipped reports that --verify-after-build=false skipped
	// the self-verify (the image was renamed without the step-7
	// guarantee - the documented V1 speed trade).
	VerifySkipped bool
	// Warnings are the non-fatal metadata downgrades collected by
	// staging (5.4 EPERM chown as non-root).
	Warnings []string
}

// Deps (the T4.2 staging deps, extended in staging.go with the T4.3
// Master/Extractor/AfterMaster seams) carries the build-phase
// dependencies. FreeSpace/Source/HashStore/Bus/OpenPartial serve
// staging; Master is required; Extractor is required iff
// BuildInput.VerifyAfterBuild.

// Build masters, self-verifies, and atomically finalizes ONE image per
// the 6.3 flow (see the file doc). On success the .iso exists at
// BuildResult.ISOPath with state 'built'; on failure the state is
// 'failed' (or untouched 'planned' when the failure preceded step 3),
// the .partial is kept unless CleanFailed, and the error is returned
// for the cli's per-image accounting.
func Build(ctx context.Context, deps Deps, in BuildInput) (BuildResult, error) {
	d := deps

	// Step 0: gates. validateBuildInput runs the F2 set-id gate and
	// every wiring check BEFORE any filesystem or catalog effect; the
	// required seams are checked before Stage so a wiring bug never
	// pays for a full staging pass.
	if err := validateBuildInput(in); err != nil {
		return BuildResult{}, err
	}
	if d.Master == nil {
		return BuildResult{}, errors.New("imagebuild: nil Master (the xorriso exec seam is required)")
	}
	if d.Extractor == nil && in.VerifyAfterBuild {
		return BuildResult{}, errors.New("imagebuild: nil Extractor with VerifyAfterBuild=true (the self-verify seam is required; pass an extractor or disable the flag)")
	}
	// F4 #1: refuse to start a build on a cancelled context (the
	// T4.2 cancel-after-last-member contract: an unchecked ctx would
	// stage and master a full image from a cancelled run).
	if err := ctx.Err(); err != nil {
		return BuildResult{}, err
	}
	if err := os.MkdirAll(in.DestDir, 0o755); err != nil {
		return BuildResult{}, fmt.Errorf("imagebuild: creating destination directory %s: %w", in.DestDir, err)
	}

	// Step 1: Stage (T4.2, reused as-is). Stage removes its scratch on
	// every failure; on success Build owns the workspace and removes
	// it before returning (the T4.2 caller contract - xorriso has
	// consumed the pathlist by then on every path).
	//
	// The build(stage) Phase event (FR-042, the 6.2 four-phase
	// progress model): determinate totals from the inventory, emitted
	// BEFORE the first ItemDone so the phase opens determinate - the
	// cli used to emit a single "build" phase here; the four named
	// phases are now service-owned so the sequence is consistent for
	// every caller.
	files, payload := stageTotals(in.Inventory)
	emitPhase(d.Bus, progress.Phase{
		Name: PhaseStage, TotalKnown: true, TotalFiles: files, TotalBytes: payload,
	})
	staged, err := Stage(ctx, d, in.stageInput())
	if err != nil {
		return BuildResult{}, err // SourceChangedError travels typed (N3 wiring)
	}
	defer staged.Cleanup() //nolint:errcheck // best-effort scratch removal; the run result dominates

	b := &builder{d: d, in: in, staged: staged}
	finalISO, err := media.ImageISOFileName(in.SetID, in.ImageID)
	if err != nil {
		return BuildResult{}, err // unreachable: gated in validateBuildInput
	}
	b.finalPath = filepath.Join(in.DestDir, finalISO)
	b.partialPath = b.finalPath + ".partial"

	// Step 2: the effective chunk hashes (post-stage, so the 4.3
	// captures of step 1 are visible), then the two embeds.
	if err := b.fetchChunkHashes(ctx); err != nil {
		return b.result(), err
	}
	if err := b.embedManifest(ctx); err != nil {
		return b.fail(ctx, err, nil)
	}
	if err := b.embedExport(ctx); err != nil {
		return b.fail(ctx, err, nil)
	}

	// Step 3: THE 6.6 TRANSACTION - the FINAL content list and the
	// state flip 'planned'/'built' -> 'building' commit together.
	b.rows, err = b.contentRows()
	if err != nil {
		return b.fail(ctx, err, nil)
	}
	if err := in.Store.BeginBuild(ctx, b.rows); err != nil {
		// The transaction rolled back: state is untouched ('planned'
		// or the previous terminal state), so no MarkFailed - the
		// error is a plain build failure for the cli's accounting.
		return b.result(), err
	}
	b.began = true

	// Step 4: the FINAL pathlist - payload plus the two image-root
	// graft entries (T4.2's Staged.EmbeddedPathListEntries contract).
	// The append copies (never aliases staged.PathList's backing
	// array): the staged slice keeps describing the payload-only set.
	finalPathList := make([]PathListEntry, 0, len(staged.PathList)+2)
	finalPathList = append(finalPathList, staged.PathList...)
	finalPathList = append(finalPathList, staged.EmbeddedPathListEntries()...)
	if err := WritePathList(staged.PathListPath, finalPathList); err != nil {
		return b.fail(ctx, err, nil)
	}

	// Step 5: F4 #2 - the ctx check RIGHT BEFORE the exec. The T4.2
	// cancel-after-last-member window means Stage can return a
	// COMPLETE tree under a cancelled ctx; without this check Build
	// would master an image from a cancelled run.
	if err := ctx.Err(); err != nil {
		return b.fail(ctx, fmt.Errorf("imagebuild: refusing to master image %d of set %q on a cancelled context: %w",
			in.ImageID, in.SetID, err), nil)
	}

	// Step 6: the xorriso master (the T4.1 6.2 template; Rock Ridge
	// stays on by default - Invocation never emits --norock, -r, or
	// -f).
	//
	// The build(master) Phase event + byte-watcher (FR-042, the 6.2
	// four-phase progress model): the Phase carries the RequiredImageBytes
	// ESTIMATE as its total (the 4.3 canonical estimator over the
	// content list - a conservative upper bound, so real ISOs stall the
	// bar honestly just below 100% until finalize; a pathological
	// overshoot saturates at 100% in the renderer, never rolls back),
	// and is emitted BEFORE the watcher starts. The watcher then stats
	// the growing .partial every masterPollInterval (watch.go) and
	// emits BytesWritten{absolute size} until Tool.Run returns - tied
	// to ctx, so cancellation/interrupt/error paths all stop it.
	argv, err := (xorriso.Invocation{
		VolumeID:     volumeID(in.SetID, in.ImageID),
		OutputPath:   b.partialPath,
		PathListPath: staged.PathListPath,
		Joliet:       true,
	}).Argv()
	if err != nil {
		return b.fail(ctx, err, nil)
	}
	emitPhase(d.Bus, progress.Phase{
		Name: PhaseMaster, TotalKnown: true, TotalBytes: b.masterEstimateBytes(),
	})
	stopWatch := startMasterWatcher(ctx, b.partialPath, busEmit(d.Bus), d.MasterWatch)
	transcript, runErr := d.Master.Run(ctx, argv)
	stopWatch()
	if runErr != nil {
		return b.fail(ctx, fmt.Errorf("imagebuild: mastering image %d of set %q: %w", in.ImageID, in.SetID, runErr), transcript)
	}
	if d.AfterMaster != nil {
		if err := d.AfterMaster(ctx, b.partialPath); err != nil {
			return b.fail(ctx, fmt.Errorf("imagebuild: after-master hook: %w", err), transcript)
		}
	}

	// Step 7: SELF-VERIFY (6.3 step 2 / 7.1) - extraction re-hash of
	// the .partial against the step-3 content list.
	if in.VerifyAfterBuild {
		verified, err := b.verifyImage(ctx)
		if err != nil {
			return b.fail(ctx, err, transcript)
		}
		b.verified = verified
	} else {
		b.skippedVerify = true
	}

	// Step 8: ATOMIC FINALIZE (6.3 step 3) - fsync, rename, dir fsync,
	// then the 'built' transaction.
	size, err := b.finalize(ctx)
	if err != nil {
		return b.fail(ctx, err, transcript)
	}
	finCtx, cancelFin := detachCtx(ctx)
	defer cancelFin()
	if err := in.Store.FinishBuild(finCtx, b.finalPath, time.Now().UnixNano(), size); err != nil {
		// The .iso is finalized on disk but the catalog says
		// 'building' - the next rebuild self-heals (file doc). Report
		// honestly: the build SUCCEEDED on media, the state write
		// failed.
		return b.result(), fmt.Errorf("imagebuild: image %d of set %q finalized at %s but the built-state transaction failed: %w",
			in.ImageID, in.SetID, b.finalPath, err)
	}
	res := b.result()
	res.BytesWritten = size
	return res, nil
}

// builder carries the per-Build state through the 6.3 steps.
type builder struct {
	d      Deps
	in     BuildInput
	staged *Staged

	chunkHashes   [][]byte          // parallel to in.Inventory.Chunks (stored hashes)
	manifestBlob  []byte            // the marshaled MANIFEST.json bytes
	manifestSum   [sha256.Size]byte // sha256 of manifestBlob
	rows          []ContentRow      // the step-3 FINAL content list
	partialPath   string
	finalPath     string
	began         bool // the step-3 transaction committed
	verified      int
	skippedVerify bool
}

// result renders the partial BuildResult known at the current point of
// the flow (warnings from staging always travel).
func (b *builder) result() BuildResult {
	return BuildResult{
		ISOPath:         b.finalPath,
		PartialPath:     b.partialPath,
		ContentEntries:  len(b.rows),
		VerifiedEntries: b.verified,
		VerifySkipped:   b.skippedVerify,
		Warnings:        b.staged.Warnings,
	}
}

// emitPhase is the nil-safe Phase helper (the Deps.Bus nil-safety
// convention; Stage's emitDone shape).
func emitPhase(bus *progress.Bus, p progress.Phase) {
	if bus != nil {
		bus.Emit(p)
	}
}

// stageTotals counts the inventory's members and payload bytes for the
// build(stage) Phase event (files + chunk lengths + symlink target
// lengths; dirs are members with zero bytes - the cli buildTotals
// shape, moved here so the four phases are service-owned).
func stageTotals(inv StageInventory) (files, bytes int64) {
	for _, f := range inv.Files {
		files++
		bytes += f.Size
	}
	for _, c := range inv.Chunks {
		files++
		bytes += c.Length
	}
	for _, sy := range inv.Symlinks {
		files++
		bytes += sy.Size
	}
	files += int64(len(inv.Dirs))
	return files, bytes
}

// masterEstimateBytes is the build(master) bar total: the canonical
// 4.3 RequiredImageBytes estimator over the step-3 content list - a
// CONSERVATIVE upper bound on the finalized ISO size (DESIGN_SPEC 6.2:
// real ISOs land under it, so the bar stalls honestly just below 100%
// until finalize resolves the phase; a pathological overshoot saturates
// at 100% in the renderer). Judgment call (documented): the payload
// figure passed to the estimator is the content list's regular-file
// bytes INCLUDING the manifest and export members - their ENTRIES live
// in the estimator's FixedImageOverhead, but their BYTES do not, and an
// upper bound that skipped a possibly-large catalog export would stop
// being an upper bound. payloadEntries is the row count minus those two
// image-root members.
func (b *builder) masterEstimateBytes() int64 {
	tot := b.selfVerifyTotals()
	payloadEntries := tot.Entries - 2 // manifest + export rows live in FixedImageOverhead
	if payloadEntries < 0 {
		payloadEntries = 0
	}
	return units.RequiredImageBytes(tot.Bytes, payloadEntries)
}

// selfVerifyTotals derives the EXACT build(extract)/build(verify)
// progress totals from the step-3 content list (6.2; verify.Totals are
// first-class parameters - T5.1 passes the same figures).
func (b *builder) selfVerifyTotals() verify.Totals {
	return verifyTotalsOfRows(b.rows)
}

// verifyTotalsOfRows is the pure derivation behind selfVerifyTotals
// (T5.1: the derivation moved into internal/verify as verify.TotalsOf,
// next to the comparison it feeds; this delegate keeps the T4.3 pin
// tests exercising the moved code unchanged). Entries is every row;
// Files/Bytes count only the REGULAR-FILE rows (see verify.TotalsOf).
func verifyTotalsOfRows(rows []ContentRow) verify.Totals {
	return verify.TotalsOf(rows)
}

// stageInput maps the BuildInput onto the T4.2 StageInput (the same
// structs Stage consumed - the manifest assembly below consumes them
// too, so staging, manifest, and content list share ONE inventory
// projection and cannot drift).
func (in BuildInput) stageInput() StageInput {
	return StageInput{
		SetID:      in.SetID,
		ImageID:    in.ImageID,
		SourceRoot: in.SourceRoot,
		WorkDir:    in.WorkDir,
		UsableSize: in.UsableSize,
		Mode:       in.Mode,
		Inventory:  in.Inventory,
	}
}

// fetchChunkHashes reads the STORED hashes of this image's chunks
// AFTER staging (6.3 step 1: staging stored every 4.3 capture), and
// cross-checks them against the inventory snapshot: a snapshot hash
// that disagrees with the stored one means the catalog changed under
// the single-writer flock - corrupt state, refused loudly rather than
// manifested with whichever value arrived last.
func (b *builder) fetchChunkHashes(ctx context.Context) error {
	chunks := b.in.Inventory.Chunks
	if len(chunks) == 0 {
		return nil
	}
	fileIDs := make([]int64, len(chunks))
	indexes := make([]int, len(chunks))
	for i, c := range chunks {
		fileIDs[i] = c.FileID
		indexes[i] = c.ChunkIndex
	}
	hashes, err := b.in.Store.ChunkHashes(ctx, fileIDs, indexes)
	if err != nil {
		return fmt.Errorf("imagebuild: reading stored chunk hashes (staging is the first write; a missing hash here is corrupt state): %w", err)
	}
	if len(hashes) != len(chunks) {
		return fmt.Errorf("imagebuild: chunk hash read returned %d hashes for %d chunks", len(hashes), len(chunks))
	}
	for i, c := range chunks {
		if len(hashes[i]) != 32 {
			return fmt.Errorf("imagebuild: stored hash of chunk %d of %q is %d bytes, want 32 (D-09)",
				c.ChunkIndex, c.ParentRelPath, len(hashes[i]))
		}
		if len(c.Hash) != 0 && !bytes.Equal(c.Hash, hashes[i]) {
			return fmt.Errorf("imagebuild: chunk %d of %q stores hash %x but the inventory snapshot carried %x (catalog changed under the single-writer lock)",
				c.ChunkIndex, c.ParentRelPath, hashes[i], c.Hash)
		}
	}
	b.chunkHashes = hashes
	return nil
}

// embedManifest assembles and embeds this image's MANIFEST.json (6.6,
// FR-033): GenerateManifest validates the input (placement gate
// included), MarshalManifest renders the deterministic bytes, and
// copyengine.EmbedFile publishes them into the staging tree under the
// 5.1 write-verify discipline minus the mark. The published hash is
// re-checked against the in-memory digest - a mismatch means the bytes
// changed between marshal and read-back, which can only be fault.
func (b *builder) embedManifest(ctx context.Context) error {
	mi := manifestInputFor(b.in, b.chunkHashes)
	m, err := media.GenerateManifest(mi)
	if err != nil {
		return fmt.Errorf("imagebuild: generating MANIFEST.json for image %d of set %q: %w", b.in.ImageID, b.in.SetID, err)
	}
	blob, err := media.MarshalManifest(m)
	if err != nil {
		return fmt.Errorf("imagebuild: marshaling MANIFEST.json for image %d of set %q: %w", b.in.ImageID, b.in.SetID, err)
	}
	want := sha256.Sum256(blob)
	b.manifestBlob, b.manifestSum = blob, want

	// The bytes come from memory, so they travel through a scratch file
	// to share EmbedFile's write-verify discipline (the copy.go
	// precedent; one small JSON document in the OS temp dir, removed
	// immediately after the embed consumes it).
	tmp, err := os.CreateTemp(b.in.WorkDir, "isopress-manifest-*.json")
	if err != nil {
		return fmt.Errorf("imagebuild: creating manifest scratch: %w", err)
	}
	tmpPath := tmp.Name()
	_, werr := tmp.Write(blob)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmpPath)
		if werr != nil {
			return fmt.Errorf("imagebuild: writing manifest scratch: %w", werr)
		}
		return fmt.Errorf("imagebuild: closing manifest scratch: %w", cerr)
	}
	dest := filepath.Join(b.staged.Root, media.ManifestFileName)
	pubHash, _, embedErr := copyengine.EmbedFile(ctx, copyengine.Deps{}, tmpPath, dest)
	_ = os.Remove(tmpPath)
	if embedErr != nil {
		return fmt.Errorf("imagebuild: embedding MANIFEST.json: %w", embedErr)
	}
	if !bytes.Equal(pubHash, want[:]) {
		return fmt.Errorf("imagebuild: embedded MANIFEST.json hash %x != marshaled digest %x (bytes changed between marshal and read-back)",
			pubHash, want[:])
	}
	return nil
}

// embedExport embeds the run's single FR-034 export as
// catalog-export.sqlite (FR-035: the SAME bytes every image of the
// run). The published hash is re-checked against the produced one (the
// run-scoped export file changed under us is the only honest cause).
func (b *builder) embedExport(ctx context.Context) error {
	exp := b.in.Export
	dest := filepath.Join(b.staged.Root, media.ExportFileName)
	pubHash, pubSize, err := copyengine.EmbedFile(ctx, copyengine.Deps{}, exp.Path, dest)
	if err != nil {
		return fmt.Errorf("imagebuild: embedding catalog export %s: %w", exp.Path, err)
	}
	if !bytes.Equal(pubHash, exp.Hash) {
		return fmt.Errorf("imagebuild: embedded catalog export hash %x != produced export hash %x (the export file changed during the run)",
			pubHash, exp.Hash)
	}
	if pubSize != exp.Size {
		return fmt.Errorf("imagebuild: embedded catalog export is %d bytes, produced export was %d bytes", pubSize, exp.Size)
	}
	return nil
}

// manifestInputFor assembles the media.ManifestInput of one image from
// the BuildInput's inventory + manifest seed and the POST-STAGE chunk
// hashes (the 4.3 captures staging stored - 6.6 needs a hash on every
// chunk entry). It is a pure function of its arguments over the SAME
// structs staging consumed, so staging, manifest, and the step-3
// content list share ONE inventory projection and cannot drift; the
// tests re-derive the expected manifest bytes through it, pinning the
// end-to-end determinism of MarshalManifest.
func manifestInputFor(in BuildInput, chunkHashes [][]byte) media.ManifestInput {
	mi := media.ManifestInput{
		SetID:          in.SetID,
		ImageNumber:    in.ImageID,
		ToolVersion:    in.Manifest.ToolVersion,
		SchemaVersion:  in.Manifest.SchemaVersion,
		CreatedAt:      in.Manifest.CreatedAt,
		ChunkedFiles:   in.Manifest.ChunkedFiles,
		HardLinkGroups: in.Manifest.HardLinkGroups,
		Placement:      in.Manifest.Placement,
	}
	for _, f := range in.Inventory.Files {
		mi.Entries = append(mi.Entries, media.ImageEntry{
			Kind: media.KindFile, RelPath: f.RelPath, Size: f.Size, SHA256: f.Hash,
			Mode: f.Mode, UID: f.UID, GID: f.GID, MtimeNS: f.MtimeNS,
		})
	}
	for i, c := range in.Inventory.Chunks {
		mi.Entries = append(mi.Entries, media.ImageEntry{
			Kind: media.KindChunk, RelPath: c.ParentRelPath, ChunkIndex: int64(c.ChunkIndex),
			Size: c.Length, SHA256: chunkHashes[i],
			Mode: c.ParentMode, UID: c.ParentUID, GID: c.ParentGID, MtimeNS: c.ParentMtimeNS,
		})
	}
	for _, sy := range in.Inventory.Symlinks {
		mi.Entries = append(mi.Entries, media.ImageEntry{
			Kind: media.KindSymlink, RelPath: sy.RelPath, Size: sy.Size, LinkTarget: sy.LinkTarget,
			Mode: sy.Mode, UID: sy.UID, GID: sy.GID, MtimeNS: sy.MtimeNS,
		})
	}
	for _, dir := range in.Inventory.Dirs {
		mi.Entries = append(mi.Entries, media.ImageEntry{
			Kind: media.KindDir, RelPath: dir.RelPath,
			Mode: dir.Mode, UID: dir.UID, GID: dir.GID, MtimeNS: dir.MtimeNS,
		})
	}
	return mi
}

// contentRows computes THE 6.6 FINAL content list: every staged file,
// chunk, symlink, and empty dir, plus the manifest and export
// self-rows. This is the list the step-3 transaction writes and the
// step-7 self-verify compares against - one derivation, both consumers.
func (b *builder) contentRows() ([]ContentRow, error) {
	in := b.in
	var rows []ContentRow
	for _, f := range in.Inventory.Files {
		mp, err := media.FileMediaPath(f.RelPath)
		if err != nil {
			return nil, err
		}
		rows = append(rows, ContentRow{
			MediaPath: mp, Kind: media.KindFile, FileID: f.FileID, ChunkIndex: -1,
			Size: f.Size, Hash: f.Hash,
		})
	}
	for i, c := range in.Inventory.Chunks {
		mp, err := media.ChunkMediaPath(c.ParentRelPath, int64(c.ChunkIndex))
		if err != nil {
			return nil, err
		}
		rows = append(rows, ContentRow{
			MediaPath: mp, Kind: media.KindChunk, FileID: c.FileID, ChunkIndex: c.ChunkIndex,
			Size: c.Length, Hash: b.chunkHashes[i],
		})
	}
	for _, sy := range in.Inventory.Symlinks {
		mp, err := media.FileMediaPath(sy.RelPath)
		if err != nil {
			return nil, err
		}
		rows = append(rows, ContentRow{
			MediaPath: mp, Kind: media.KindSymlink, FileID: sy.FileID, ChunkIndex: -1, Size: sy.Size,
		})
	}
	for _, dir := range in.Inventory.Dirs {
		mp, err := media.FileMediaPath(dir.RelPath)
		if err != nil {
			return nil, err
		}
		rows = append(rows, ContentRow{
			MediaPath: mp, Kind: media.KindDir, ChunkIndex: -1,
		})
	}
	rows = append(rows,
		ContentRow{MediaPath: media.ManifestFileName, Kind: kindManifest, ChunkIndex: -1,
			Size: int64(len(b.manifestBlob)), Hash: b.manifestSum[:]},
		ContentRow{MediaPath: media.ExportFileName, Kind: kindExport, ChunkIndex: -1,
			Size: b.in.Export.Size, Hash: b.in.Export.Hash},
	)
	return rows, nil
}

// verifyImage is the step-7 self-verify (6.3 step 2, 7.1, 7.2): a
// fresh extraction scratch under --work-dir, the free-space pre-check
// (7.2's Statfs gate through the same FreeSpace seam staging used),
// ExtractAndHash of the .partial, and the comparison against the
// step-3 content list. Returns the verified entry count. The Bus and
// the EXACT content-list totals flow into the primitive, which emits
// the build(extract) (tree-sum watcher) and build(verify) (per-entry
// ItemDone) progress events of the 6.2 four-phase model.
func (b *builder) verifyImage(ctx context.Context) (int, error) {
	dir, err := os.MkdirTemp(b.in.WorkDir, extractDirPrefix)
	if err != nil {
		return 0, fmt.Errorf("imagebuild: creating extraction scratch for the self-verify: %w", err)
	}
	defer os.RemoveAll(dir) //nolint:errcheck // scratch; the .partial is the inspection artifact

	free := b.d.FreeSpace
	if free == nil {
		free = statfsFreeSpace
	}
	var need int64
	for _, r := range b.rows {
		need += r.Size
	}
	avail, err := free(dir)
	if err != nil {
		return 0, fmt.Errorf("imagebuild: free-space pre-check of the extraction scratch: %w", err)
	}
	if uint64(need)+extractSlack > avail {
		return 0, fmt.Errorf(
			"imagebuild: extraction self-verify of image %d needs about %d bytes free under %q, %d available (DESIGN_SPEC 7.2); "+
				"choose a --work-dir with more space or build to a larger filesystem",
			b.in.ImageID, need+extractSlack, b.in.WorkDir, avail)
	}

	res, err := b.d.Extractor.ExtractAndHash(ctx, b.partialPath, dir, b.d.Bus, b.selfVerifyTotals())
	if err != nil {
		return 0, fmt.Errorf("imagebuild: extracting image %d of set %q for self-verification: %w",
			b.in.ImageID, b.in.SetID, err)
	}
	// The comparison MOVED to internal/verify in T5.1 (§5.5): the
	// build self-verify and verify-media now run the ONE
	// verify.CompareContents; the per-entry problem lines keep the
	// exact T4.3 wording through Comparison.Problems().
	cmp := verify.CompareContents(b.rows, res.Entries, b.symlinkTargets())
	if !cmp.Clean() {
		return 0, &VerifyMismatchError{ImageID: b.in.ImageID, SetID: b.in.SetID, Problems: cmp.Problems()}
	}
	return len(b.rows), nil
}

// symlinkTargets maps media path -> recorded target for this image's
// symlinks (the target travels in the inventory and the manifest, but
// NOT in the image_contents row - the comparison needs it here).
func (b *builder) symlinkTargets() map[string]string {
	out := make(map[string]string, len(b.in.Inventory.Symlinks))
	for _, sy := range b.in.Inventory.Symlinks {
		if mp, err := media.FileMediaPath(sy.RelPath); err == nil {
			out[mp] = sy.LinkTarget
		}
	}
	return out
}

// VerifyMismatchError classifies a step-7 self-verify failure (FR-021
// failure reporting at build time): the extraction produced content
// that does not match the finalized 6.6 content list. Problems carries
// the per-entry lines (missing / unexpected / hash / size / kind /
// symlink-target mismatches), capped for the message.
type VerifyMismatchError struct {
	ImageID  int64
	SetID    string
	Problems []string
}

func (e *VerifyMismatchError) Error() string {
	shown := e.Problems
	suffix := ""
	if len(shown) > verifyProblemCap {
		suffix = fmt.Sprintf(" [... %d more problems omitted ...]", len(shown)-verifyProblemCap)
		shown = shown[:verifyProblemCap]
	}
	return fmt.Sprintf(
		"imagebuild: self-verification FAILED for image %d of set %q: %d problem(s):\n  %s%s "+
			"(the .partial is kept for inspection unless --clean-failed; DESIGN_SPEC 6.3 step 4)",
		e.ImageID, e.SetID, len(e.Problems), joinProblems(shown), suffix)
}

// joinProblems renders the problem lines for the error message.
func joinProblems(problems []string) string {
	out := ""
	for i, p := range problems {
		if i > 0 {
			out += "\n  "
		}
		out += p
	}
	return out
}

// compareContents is the T4.3-era step-7 comparison, now a THIN
// DELEGATE to verify.CompareContents (the T5.1 §5.5 move: the
// comparison's home is internal/verify, where verify-media's read side
// can share it without a layering inversion). Production code goes
// through verify.CompareContents directly (see verifyImage); this
// one-line wrapper exists so the T4.3 pin tests - which exercise every
// mismatch class through this name - keep exercising the moved code
// unchanged.
func compareContents(rows []ContentRow, extracted []verify.Entry, targets map[string]string) []string {
	return verify.CompareContents(rows, extracted, targets).Problems()
}

// finalize is the 6.3 step-3 atomic rename sequence: fsync the
// .partial file, rename it to the final <SET-ID>-NNN.iso, fsync the
// containing directory (EINVAL/ENOTSUP tolerated, the syncDir
// contract). Returns the finalized size. The catalog 'built'
// transaction is the CALLER's next act (Build step 8) - the rename
// itself never touches the catalog, so a crash between the two leaves
// a valid .iso whose state self-heals on rebuild (file doc).
func (b *builder) finalize(ctx context.Context) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, fmt.Errorf("imagebuild: refusing to finalize on a cancelled context: %w", err)
	}
	f, err := os.Open(b.partialPath)
	if err != nil {
		return 0, fmt.Errorf("imagebuild: opening %s for fsync: %w", b.partialPath, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return 0, fmt.Errorf("imagebuild: fsync %s: %w", b.partialPath, err)
	}
	if err := f.Close(); err != nil {
		return 0, fmt.Errorf("imagebuild: closing %s after fsync: %w", b.partialPath, err)
	}
	if err := syncDir(filepath.Dir(b.partialPath)); err != nil {
		return 0, fmt.Errorf("imagebuild: fsync directory of %s: %w", b.partialPath, err)
	}
	if err := os.Rename(b.partialPath, b.finalPath); err != nil {
		return 0, fmt.Errorf("imagebuild: renaming %s to %s: %w", b.partialPath, b.finalPath, err)
	}
	if err := syncDir(filepath.Dir(b.finalPath)); err != nil {
		return 0, fmt.Errorf("imagebuild: fsync directory of %s: %w", b.finalPath, err)
	}
	fi, err := os.Stat(b.finalPath)
	if err != nil {
		return 0, fmt.Errorf("imagebuild: stat finalized image %s: %w", b.finalPath, err)
	}
	return fi.Size(), nil
}

// fail takes the ONE post-'begin' failure path (see the file doc):
// apply the .partial policy (6.3 step 4 / --clean-failed), record the
// state 'failed' + transcript event through Store.MarkFailed (on the
// DETACHED context when the run ctx is already cancelled - the
// copyengine markContext shape, because the driver rejects statements
// under a cancelled context), and return the cause. It is a no-op
// about the catalog when the step-3 transaction never committed
// (b.began false): the state is still 'planned' and there is nothing
// to fail.
func (b *builder) fail(ctx context.Context, cause error, transcript []byte) (BuildResult, error) {
	if b.in.CleanFailed {
		// 6.3 step 4's flag: remove the .partial (best-effort - a
		// removal failure never masks the primary cause).
		_ = os.Remove(b.partialPath)
	}
	if b.began {
		mkCtx, cancelMark := detachCtx(ctx)
		defer cancelMark()
		if err := b.in.Store.MarkFailed(mkCtx, failureDetail(cause, transcript)); err != nil {
			cause = errors.Join(cause, fmt.Errorf("imagebuild: recording the failed state: %w", err))
		}
	}
	return b.result(), cause
}

// detachCtx returns the context ONE post-run state transaction may run
// on: the live run context, or - once it has been CANCELLED - a fresh
// Background-derived context bounded by finalizeDetachTimeout
// (copyengine.markContext's shape and reasoning: the driver rejects
// statements under a cancelled context, which is exactly why
// interrupted runs used to strand their state).
func detachCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}
	return context.WithTimeout(context.Background(), finalizeDetachTimeout)
}

// failureDetail renders the MarkFailed run_events detail: the cause at
// the HEAD (so a truncated tail never hides what happened) followed by
// the xorriso transcript tail (6.2 "capture stdout+stderr to the run
// log").
func failureDetail(cause error, transcript []byte) string {
	detail := fmt.Sprintf("image build failed: %v", cause)
	if len(transcript) == 0 {
		return detail
	}
	if len(transcript) > transcriptDetailCap {
		detail += fmt.Sprintf("\nxorriso output [... %d earlier bytes omitted ...]:\n",
			len(transcript)-transcriptDetailCap)
		transcript = transcript[len(transcript)-transcriptDetailCap:]
	} else {
		detail += "\nxorriso output:\n"
	}
	return detail + string(transcript)
}

// volumeID renders the -V value: "<SET-ID>-<NNN>", the set id plus the
// SAME <NNN> media.ImageISOFileName / media.ImageDirName put in the
// file name (6.4), so a multi-disc set mounts with one distinguishable
// label per disc (AMENDED 2026-11-04 by the stakeholder; before that
// -V was the set id alone). The result never exceeds volumeIDMax BYTES
// (the ISO 9660 volume-id limit): when len(setID)+len(suffix) overflows
// it, the SET-ID portion is cut to volumeIDMax-len(suffix) bytes using
// the deterministic BYTE truncation the old contract used (a multi-byte
// rune may be cut, which is inert for a volume label) and the suffix is
// appended LAST - the image NUMBER is never truncated, because a naive
// head-truncate would slice "-001" off a long set id and put two discs
// of a set back under the same label. The suffix is "-%03d": %03d is a
// MINIMUM width, so image 1000 renders "-1000" exactly like the file
// name (media.ImageDirName uses the same format, so the label suffix
// and the file-name suffix are the same string for every n - pinned by
// TestVolumeIDMatchesFileNamingSuffix). The cut can never go negative:
// for any int64 the suffix is at most 20 bytes ("-9223372036854775807"),
// well under volumeIDMax. An empty set id would render as just "-001",
// but that is UNREACHABLE in the pipeline: validateBuildInput rejects
// it before any mastering via media.ImageISOFileName (media.validateSetID
// refuses the empty id), so the old "empty stays empty" tolerance is
// replaced by "empty never gets here" - honest either way. NUL bytes
// cannot reach here either: media's set-id gate refuses them in
// validateBuildInput.
func volumeID(setID string, imageNumber int64) string {
	suffix := fmt.Sprintf("-%03d", imageNumber)
	if len(setID)+len(suffix) > volumeIDMax {
		return setID[:volumeIDMax-len(suffix)] + suffix
	}
	return setID + suffix
}

// validateBuildInput enforces the BuildInput contract - the step-0
// gates, run BEFORE any filesystem or catalog effect.
func validateBuildInput(in BuildInput) error {
	// THE F2 GATE (the T4.2 review carry-in, file doc): a newline or
	// CR in the set id breaks both -V and the -o filename; refuse it
	// as Usage-class input before anything else.
	for i := 0; i < len(in.SetID); i++ {
		if in.SetID[i] == '\n' || in.SetID[i] == '\r' {
			return &report.UsageError{Msg: fmt.Sprintf(
				"--set-id %q contains the byte %q (0x%02x), which breaks both the ISO volume id (-V) and the image filename "+
					"of the 6.4 naming scheme (a newline cannot appear in either); pass a set id without newline or carriage return "+
					"(nothing was staged - '=' is allowed: xorriso -V takes it as free text)",
				in.SetID, string(in.SetID[i]), in.SetID[i])}
		}
	}
	if _, err := media.ImageISOFileName(in.SetID, in.ImageID); err != nil {
		return &report.UsageError{Err: err}
	}
	switch {
	case in.ImageID < 1:
		return fmt.Errorf("imagebuild: image id %d must be >= 1 (images are 1-based, 6.4)", in.ImageID)
	case in.SourceRoot == "":
		return errors.New("imagebuild: empty source root (sets.source_root must be read before building)")
	case in.DestDir == "":
		return errors.New("imagebuild: empty destination directory (--dest must be resolved before building)")
	case in.UsableSize <= 0:
		return fmt.Errorf("imagebuild: usable size %d is not positive (the images row must be read before building)", in.UsableSize)
	case in.Store == nil:
		return errors.New("imagebuild: nil Store (the catalog seam is required: BeginBuild writes the 6.6 content list)")
	case in.Export.Path == "":
		return errors.New("imagebuild: empty export path (the run's FR-034 export must be produced before building)")
	case in.Export.Size < 0:
		return fmt.Errorf("imagebuild: negative export size %d", in.Export.Size)
	case len(in.Export.Hash) != sha256.Size:
		return fmt.Errorf("imagebuild: export hash is %d bytes, want 32 (D-09)", len(in.Export.Hash))
	}
	return nil
}
