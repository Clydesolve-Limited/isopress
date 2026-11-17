package imagebuild

// T4.2: the staging subsystem (DESIGN_SPEC 6.2 staging directory and
// path-list, 6.3 step 1 and step 5, 5.3 logical tree, 5.4 metadata,
// 3.5 point 3 build-time change detection, D-04 ISO hard-link rule;
// IMPLEMENTATION_PLAN T4.2).
//
// Stage materializes the 5.3 logical tree of ONE image as a staging
// directory that the T4.3 builder hands to xorriso via the graft-points
// pathlist (6.2). This file owns the staging WORKSPACE and the staging
// WRITER; precheck.go owns the two capacity gates and pathlist.go owns
// the -graft-points pathlist file.
//
// # The boundary with T4.3 (BUILD phase)
//
// This task stages PAYLOAD items only: whole files, chunks, symlinks,
// and empty directories - the members the CATALOG inventories. The two
// image-root members of 5.3 (MANIFEST.json, catalog-export.sqlite) are
// added by T4.3 BEFORE xorriso runs: it embeds them into the staging
// tree with copyengine.EmbedFile's discipline (5.1 write-verify minus
// the mark), appends their graft lines to the pathlist via
// Staged.EmbeddedPathListEntries + WritePathList, and only then invokes
// xorriso. Nothing in the STAGER invokes any process - its only effects
// are on the staging workspace and the chunk-hash seam (the package's
// only process execution is the T4.3 builder's Master seam, whose exec
// site remains internal/xorriso; see build.go).
//
// # Stage FROM SOURCE (the double-I/O decision, 6.2)
//
// Staged bytes are re-read from the SOURCE tree, exactly like copy mode
// reads the source: DESIGN_SPEC 6.2's staging paragraph calls the cost
// "the same double-I/O cost that disqualified go-diskfs" - source read
// plus xorriso's read of the staging tree - so the design's own reading
// is stage-from-source. Staging from an existing copy tree was
// considered and rejected: the copy tree exists only in copy mode (an
// ISO-mode plan has none), which would make build depend on a
// destination that its own workflow never produces. Consequence: the
// lstat-vs-catalog comparison of 3.5 point 3 applies to the SOURCE at
// staging time, identically to copy mode's 5.1 step 1 (fsmeta.DiffFields
// is the single comparison, parameterised by the run's resolved
// StageInput.Mode - full vs metadata, the 2026-10-20 filesystem-class
// amendment).
//
// # Source-changed at staging FAILS the image build (3.5 point 3)
//
// DESIGN_SPEC 3.5 point 3 fixes the build-time behavior: "a changed
// file fails that image build with a clear message (do not master stale
// bytes under a current record)", restated by IMPLEMENTATION_PLAN T4.2
// ("a changed file fails that image build with a clear message"). This
// is DELIBERATELY stricter than copy mode, where a changed file is a
// skip-and-continue (5.1 step 1) because each copy item is independent:
// an ISO is mastered from ONE tree in ONE xorriso run at the END of
// staging, so a silently skipped payload member would either yield an
// incomplete ISO or abort the build anyway - failing at the changed
// item, before any mastering, is the honest and cheap point. A
// source-changed item returns the typed *SourceChangedError and aborts
// THAT image's staging (other images under --all are T4.4's loop).
//
// # ISO-mode hard-link families: PRIMARY ONLY (D-04)
//
// D-04 (confirmed): "ISO mode: the image contains only the primary
// name's file; the manifest and catalog export record the full link
// group". The STAGED tree therefore contains the family PRIMARY and
// never a secondary. This is structural here: StageInventory has NO
// secondaries projection at all (contrast catalog.CopyInventory, whose
// Secondaries drive copy mode's link(2) writes). The T4.4 cli mapping
// derives the ISO inventory from catalog.CopyInventory by copying Files,
// Symlinks, Chunks, and Dirs verbatim and DROPPING .Secondaries
// entirely - the catalog's T3.4 unified family rule already excludes
// secondaries with an assigned primary from .Files, so the primary is
// staged once under its own name and no other member carries its bytes.
// Hard links cannot survive into an ISO tree anyway: xorriso stores
// files by path, and the restore side recreates the family from the
// manifest/export link groups (FR-026, FR-039).
//
// # Metadata fidelity (5.4, FR-036): what xorriso reads is what the
// catalog recorded
//
// xorriso reads the STAGED files' metadata through Rock Ridge (6.2:
// uid/gid/mtime are preserved by Rock Ridge defaults; -r is never
// passed). So the staged tree must carry the CATALOG's metadata, not
// the staging process's umask-and-now defaults. Every staged member
// (files, symlinks, empty dirs - not chunk parts, which are not
// restored objects, matching copyengine) is passed through the T3.4
// conversion chain: catalog raw POSIX bits -> fsmeta.GoMode -> Apply
// (lchown, chmod, utimensat no-follow) on the STAGED path, exactly the
// copyengine.metaFromRaw shape re-derived here via fsmeta (copyengine's
// helper is unexported by design; the conversion invariant - GoMode is
// the canonical inverse of UnixMode - lives on fsmeta and is proven
// there, so both writers share the chain by construction, not by
// copy-paste). The setuid proof in the tests pins the chain end to end:
// a catalog 0o4755 lands as setuid on the staged file. The 5.4 EPERM
// downgrade is honored: a non-root chown denial is a WARNING collected
// in Staged.Warnings (and a progress Warn), never a failure; any OTHER
// metadata error fails the item - metadata is payload here (a silently
// wrong mode in the ISO violates FR-036), so it is NOT downgraded to a
// copy-style per-file warning the way 5.4 downgrades it for copy trees,
// where repair paths (verify-media/restore) exist. Metadata is applied
// AFTER the content read-back verify (5.4 "metadata after content"):
// the staged file's bytes are hash-verified before chmod/lchown/utimes
// touch it.
//
// # Chunk staging is the 4.3 FIRST WRITE (6.3 step 1)
//
// DESIGN_SPEC 6.3 step 1: "The staging copy is itself a first write for
// image-mode chunks: each chunk's source stream is hashed as it is
// staged and the hash stored on its chunk row". A chunk whose catalog
// hash is NULL is streamed through a hasher (the copyengine 4.3 capture
// shape), read-back-verified against that capture, and the capture is
// stored through the ChunkHashStore seam (*catalog.CopyStore
// satisfies it structurally - SetChunkHash, store-only, never
// overwrite); a chunk with a STORED hash verifies against it. So by
// the time T4.3 computes the 6.6 content list, every chunk hash exists.
// NO copied flags are ever set here: image mode records FR-020 through
// the image_contents rows T4.3 writes in the state=building transaction
// (6.6); `copied` is copy-mode resume state.
//
// # The write discipline (5.1 minus the mark, EmbedFile's shape)
//
// copyengine.CopyItem cannot be reused verbatim: it REQUIRES the mark
// seam (Store) and always marks, while staging never marks (see above).
// The 5.1 DISCIPLINE is therefore re-implemented here in EmbedFile's
// minimal shape: remove the stale "<final>.partial", stream to the
// partial (ctx-aware), fsync, close, atomic rename, fsync the
// containing directory (EINVAL/ENOTSUP tolerated), READ-BACK hash the
// staged file, compare against the anchor. A mismatch re-lstats the
// source: changed -> SourceChangedError; stable -> write fault, retried
// up to 3 times with backoff (5.1 step 7's budget), then the image
// build fails. Whole files anchor on the catalog hash (a missing anchor
// is a corrupt-catalog refusal); chunks on the capture or stored hash.
// Empty dirs and symlinks take no retry budget (the copyengine specials
// convention: a mkdir/symlink fault is recorded once, the run fails, the
// next build restages from a fresh scratch).
//
// # Workspace, cleanup, cancellation (6.2)
//
// The workspace is ONE os.MkdirTemp under the caller's --work-dir
// (StageInput.WorkDir; empty means the OS temp directory, per 6.2) -
// MkdirTemp is unique-per-call BY CONSTRUCTION, so concurrent or
// crashed builds can never share a staging tree; no naming scheme or
// counter exists to get wrong. Layout: <scratch>/files/...,
// <scratch>/chunks/... (the 5.3 tree rooted at the scratch itself, so
// the 6.2 graft line "/files/a.txt=<scratch>/files/a.txt" reads
// naturally), plus <scratch>/pathlist.txt (never a graft member, so it
// never reaches the ISO). On ANY failure - validation, either
// pre-check, an item failure, or ctx cancellation - the scratch is
// removed before Stage returns (no leaked scratch, asserted by test).
// On SUCCESS the caller owns the workspace and MUST Cleanup it once
// xorriso has consumed the pathlist (T4.3).
//
// # Dependency seams
//
// FreeSpace (the 6.2 Statfs pre-check; default stdlib syscall.Statfs,
// see precheck.go - no direct x/sys import, DESIGN_SPEC 11 gate),
// Source (copyengine.OSSource by default), HashStore (the 4.3 chunk
// capture persistence; required iff the inventory carries a
// first-write chunk), Bus (progress, nil-safe), and OpenPartial (the
// fault-injection seam for the retry path, same signature as
// copyengine's). The inventory arrives as row-shaped STRUCTS exactly
// like copyengine's: the catalog is fetched by the cli layer and mapped
// here, so this package - like copyengine, plan, and media - never
// issues SQL and never imports the catalog (DESIGN_SPEC 1.2).
//
// # The graft-points gate (a.k.a. why '=' and newlines are refused)
//
// The pathlist is LINE-ORIENTED: xorriso reads "-path-list" one line at
// a time and splits each at the FIRST '=' (the 6.2 iso_path=disk_path
// form). A member whose media path contains a newline (or CR) would
// split into two bogus lines, and one containing '=' would misparse its
// graft - both would silently corrupt or drop the member. Such names
// are legal Linux filename bytes (copy mode writes them faithfully),
// so this is an ISO-mode FORMAT limitation, not a security gate: staging
// refuses the member with ErrUngraftablePath naming it, and the image
// build fails loudly rather than mastering a mangled tree. The UTF-8
// gate (media.ValidateRelPath, via the media path builders) and the
// symlink-target UTF-8 rule carry over from the T3.1 copy-tree
// convention for the same manifest-fidelity reason (6.6's manifest is
// JSON). The gate on the DISK side of the pathlist is the upfront
// --work-dir byte gate (validateInput, before MkdirTemp): a WorkDir
// carrying '\n', '\r', or NUL would corrupt every graft line wholesale
// and is refused as a Usage-class flag error. A '=' in the WorkDir is
// deliberately ALLOWED (xorriso splits at the FIRST '=', and the disk
// side is a tool-absolute path that parses whole under that split); the
// member-side '=' refusal above stays the conservative gate it is and
// is not extended to the flag.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unicode/utf8"

	"isopress/internal/copyengine"
	"isopress/internal/fsmeta"
	"isopress/internal/media"
	"isopress/internal/progress"
	"isopress/internal/units"
)

// stagingDirPrefix is the fixed os.MkdirTemp pattern of the staging
// workspace. It deliberately carries NO user data (set ids are
// user-controlled): the set/image identity travels in error messages,
// not in scratch names.
const stagingDirPrefix = "isopress-imagebuild-"

// PathListFileName is the graft-points pathlist inside the staging
// workspace (6.2: "-path-list <workdir>/pathlist.txt"). It is tool
// scratch, never a graft member, so it never reaches the ISO.
const PathListFileName = "pathlist.txt"

// stageMaxAttempts is the per-item write-fault attempt budget (5.1 step
// 7's "retry up to 3 times with backoff").
const stageMaxAttempts = 3

// ErrUnsafeSymlinkTarget classifies a symlink whose recorded target is
// not valid UTF-8: the T3.1 copy-tree convention (the cli copy command
// refuses such targets) carries into staging because the target travels
// into the JSON MANIFEST (6.6), and JSON cannot represent raw bytes -
// the same manifest-fidelity reason media.ValidateRelPath refuses
// non-UTF-8 relpaths.
var ErrUnsafeSymlinkTarget = errors.New("imagebuild: symlink target is not valid UTF-8")

// ErrUngraftablePath classifies a member whose media path cannot be
// expressed in the line-oriented -graft-points pathlist (see the file
// doc's graft gate): a newline, CR, or '=' in the media path. Copy mode
// writes such names faithfully; ISO mode refuses them loudly.
var ErrUngraftablePath = errors.New("imagebuild: media path cannot be expressed in the graft-points pathlist")

// SourceChangedError is the typed 3.5 point 3 failure: the source entry
// behind a catalog record changed since the scan (the fixed D-10 field
// set differs), so staging refuses to master stale bytes under a
// current record. It fails THAT image's staging; the remedy is a rescan
// and rebuild.
//
// EXIT-CLASS WIRING CONTRACT (T4.4): this is the DESIGN_SPEC 6.2
// IMAGE-failure shape, NOT the bug class. An image whose member changed
// mid-build contributes to the run's item-failure accounting: with
// --all the T4.4 loop continues staging the other images and the run
// exits 10 (report.ExitItemFailures - the partial-failure class), and
// without --all the run carries the one total failure. T4.4 MUST
// classify this error through report.ItemFailures; letting it fall
// through report.Classify's default would mislabel a user-actionable
// rescan-and-rebuild as the bug-class report.ExitFatal (exit 1).
type SourceChangedError struct {
	// RelPath is the source relpath whose entry changed.
	RelPath string
	// Diff names the differing D-10 fields (size, mtime_ns, dev,
	// inode). For symlinks the inventory carries size and mtime only
	// (see StageSymlink), so the diff never names dev/inode there.
	Diff fsmeta.Diff
}

func (e *SourceChangedError) Error() string {
	return fmt.Sprintf("imagebuild: source %q changed since scan (%s); a build must not master stale bytes under a current record (DESIGN_SPEC 3.5 point 3) - rescan and rebuild",
		e.RelPath, e.Diff)
}

// --- the per-image staging input (the cli's row-shape mapping target) --------

// StageFile is one whole regular file (or the primary of a hard-link
// family - D-04 ISO rule, see the file doc) to stage under files/. The
// fields mirror the catalog files-row columns the copyengine.Item and
// catalog.CopyFile shapes carry: Mode/UID/GID are raw POSIX catalog
// values (the 2.3 INTEGER convention), MtimeNS is unix nanoseconds, and
// Dev/Inode feed the 3.5 point 3 comparison.
type StageFile struct {
	FileID  int64
	RelPath string
	Size    int64
	Mode    uint32
	UID     uint32
	GID     uint32
	MtimeNS int64
	Dev     int64
	Inode   int64
	// Hash is the catalog files.hash anchor (32-byte sha256). Active
	// regular rows always carry one (unreadable rows never reach
	// media); a missing anchor is a corrupt-catalog refusal.
	Hash []byte
}

// StageChunk is one chunk of a chunked parent to stage under chunks/.
// ChunkIndex is the CATALOG 0-based index (the on-media name is
// index+1, media.ChunkFileName). The Parent* fields describe the SOURCE
// parent file: the 3.5 point 3 comparison runs against them, and the
// ranged source read starts at Offset for Length bytes.
//
// The T4.3 additive (metadata for the 6.6 manifest): ParentMode /
// ParentUID / ParentGID carry the parent's catalog metadata so the
// builder can assemble the manifest's chunk entries (6.6: "every entry
// of this image with ... mtime_ns, mode, uid, gid") from the SAME
// structs Stage consumed - staging itself ignores them (chunk parts get
// no metadata application, the copyengine convention; the parent's
// metadata travels in the manifest and is applied by restore, 8.4).
type StageChunk struct {
	FileID     int64
	ChunkIndex int
	Offset     int64
	Length     int64
	// Hash is the STORED chunks.hash; nil means first write (4.3): the
	// source stream is captured during the stage and the capture is
	// stored through Deps.HashStore (6.3 step 1).
	Hash []byte

	ParentRelPath string
	ParentSize    int64
	ParentMode    uint32
	ParentUID     uint32
	ParentGID     uint32
	ParentMtimeNS int64
	ParentDev     int64
	ParentInode   int64
}

// StageSymlink is one symlink to recreate verbatim under files/ (D-03:
// preserve, never follow). Size is len(target) per 2.3. Mode/UID/GID are
// informational on Linux symlinks (fsmeta.Apply skips chmod for links
// and never follows); they flow through the same conversion chain for
// uniformity.
type StageSymlink struct {
	FileID     int64
	RelPath    string
	LinkTarget string
	Size       int64
	Mode       uint32
	UID        uint32
	GID        uint32
	MtimeNS    int64
}

// StageDir is one EMPTY directory to recreate under files/ (FR-037: the
// durable empty-dir skeleton; non-empty parents are implied by the
// member paths and carry the tree-default 0755 with no catalog record,
// exactly like copy mode).
type StageDir struct {
	DirID   int64
	RelPath string
	Mode    uint32
	UID     uint32
	GID     uint32
	MtimeNS int64
}

// StageInventory is the complete PAYLOAD inventory of one image to
// stage. NO secondaries projection exists here by design (the D-04 ISO
// rule, see the file doc): the cli derives it from catalog.CopyInventory
// by copying Files/Symlinks/Chunks/Dirs verbatim and dropping
// .Secondaries - the ISO tree carries the family primary only, and the
// manifest/export record the full link group.
type StageInventory struct {
	Files    []StageFile
	Chunks   []StageChunk
	Symlinks []StageSymlink
	Dirs     []StageDir
}

// StageInput is everything Stage needs for one image: no catalog handle,
// no xorriso tool - row-shaped data and numbers the cli read (the
// copyengine contract, DESIGN_SPEC 1.2).
type StageInput struct {
	// SetID and ImageID identify the image in error messages and
	// pre-check reports (6.4: images are 1-based).
	SetID   string
	ImageID int64
	// SourceRoot is the absolute path the inventory relpaths resolve
	// against (sets.source_root).
	SourceRoot string
	// WorkDir is the --work-dir value: the parent of the staging
	// scratch. Empty means the OS temp directory (6.2's default).
	WorkDir string
	// UsableSize is the image's usable_size (4.1 profiles): the 6.3
	// step 5 pre-check refuses a payload that cannot fit it.
	UsableSize int64
	// Mode is the run's resolved change-detection mode (DESIGN_SPEC
	// 3.5; the cli resolves it once per run from the source root via
	// fsmeta.ResolveChangeDetection). It parameterises the 3.5 point 3
	// comparisons staging runs against each staged file and chunk
	// parent (the same fsmeta.DiffFields the copy path uses). The zero
	// value is the FULL set - the safe pre-amendment default.
	Mode fsmeta.Mode
	// Inventory is the payload to stage.
	Inventory StageInventory
}

// ChunkHashStore is the 4.3 first-write chunk-hash persistence seam
// (6.3 step 1: staging IS the first write for image-mode chunks).
// *catalog.CopyStore satisfies it structurally (SetChunkHash - the
// store-only, never-overwrite surface); the imagebuild package never
// imports the catalog.
type ChunkHashStore interface {
	SetChunkHash(ctx context.Context, fileID int64, chunkIndex int, hash []byte) error
}

// FreeSpaceFunc reports the unprivileged available bytes on the
// filesystem holding path (the 6.2 Statfs pre-check seam; default
// stdlib syscall.Statfs, precheck.go).
type FreeSpaceFunc func(path string) (availBytes uint64, err error)

// Deps are the injectable staging dependencies. The zero value selects
// production defaults for every optional seam; HashStore is required
// iff the inventory carries a first-write chunk (the stager fails that
// chunk loudly rather than dropping the capture silently).
//
// The T4.3 BUILD-phase seams (Master, Extractor, AfterMaster - see
// build.go) travel on this same struct so one Deps value flows through
// the whole 6.3 pipeline; Stage ignores them.
type Deps struct {
	// FreeSpace overrides the free-space probe of the staging root.
	// Default: statfsFreeSpace.
	FreeSpace FreeSpaceFunc
	// Source is the source filesystem access (lstat + ranged open).
	// Default: copyengine.OSSource{}.
	Source copyengine.SourceFS
	// HashStore persists first-write chunk captures (4.3, 6.3 step 1).
	HashStore ChunkHashStore
	// Bus receives one ItemDone per staged payload member and one Warn
	// per metadata downgrade (FR-042; nil-safe).
	Bus *progress.Bus
	// OpenPartial overrides the staging ".partial" creation. Fault
	// seam for the 5.1-step-7 retry path. Default: os.OpenFile
	// (O_CREATE|O_WRONLY|O_TRUNC, 0644), copyengine's writer shape.
	OpenPartial func(path string) (copyengine.WriteSyncCloser, error)

	// Master executes the T4.1 xorriso invocation (build.go; required
	// for Build, ignored by Stage).
	Master Master
	// Extractor runs the step-7 extraction self-verify (build.go;
	// required iff BuildInput.VerifyAfterBuild, ignored by Stage).
	Extractor Extractor
	// MasterWatch configures the build(master) byte-watcher (watch.go;
	// the zero value is the production configuration - the
	// masterPollInterval const and os.Stat - and tests inject a fast
	// interval plus a fake stat source; ignored by Stage).
	MasterWatch MasterWatch
	// AfterMaster is the TEST-ONLY fault-injection seam between master
	// and self-verify (build.go; ignored by Stage).
	AfterMaster func(ctx context.Context, partialPath string) error
}

// Staged is the successful staging result the T4.3 builder consumes:
// the workspace tree, the pathlist, and the accounting the pre-checks
// and progress reporting need. The caller OWNS the workspace after a
// successful Stage and must call Cleanup once xorriso no longer needs
// it; a FAILED Stage already removed its scratch.
type Staged struct {
	// Root is the staging workspace and the 5.3 tree root: files/ and
	// chunks/ live directly under it.
	Root string
	// PathListPath is the graft-points pathlist file
	// (<Root>/pathlist.txt), one "<iso path>=<staged disk path>" line
	// per staged PAYLOAD member, sorted (pathlist.go). T4.3 appends
	// the manifest/export lines before invoking xorriso.
	PathListPath string
	// PathList is the exact content of PathListPath.
	PathList []PathListEntry
	// PayloadBytes is the sum of staged payload bytes (whole files +
	// chunk lengths): the "about equal to payload size" figure of the
	// 6.2 free-space pre-check, re-usable by T4.3/T4.4 reports.
	PayloadBytes int64
	// EntryCount is the number of staged payload members (whole files,
	// chunks, symlinks, empty dirs). The 6.3 step 5 pre-check's entry
	// count n is this figure plus the two image-root members T4.3
	// embeds (the +2 of validateInput).
	EntryCount int
	// Warnings are the non-fatal metadata downgrades (5.4 EPERM chown
	// as non-root) collected during staging.
	Warnings []string
}

// Cleanup removes the staging workspace (best-effort). It is the
// caller's obligation after a successful Stage - the tree must exist
// until xorriso has consumed the pathlist (T4.3).
func (s *Staged) Cleanup() error {
	if s == nil || s.Root == "" {
		return nil
	}
	return os.RemoveAll(s.Root)
}

// EmbeddedPathListEntries returns the graft entries of the two
// image-root members T4.3 embeds into <Root> before mastering (6.2, 6.6,
// 5.3): "/MANIFEST.json=<Root>/MANIFEST.json" and
// "/catalog-export.sqlite=<Root>/catalog-export.sqlite". The files do
// not exist yet at staging time - the entries describe where T4.3 will
// EmbedFile them, so the builder writes the FINAL pathlist as
//
//	WritePathList(st.PathListPath, append(st.PathList, st.EmbeddedPathListEntries()...))
//
// after the embeds and before the xorriso invocation. Exposed here so
// the full 6.2 pathlist (payload + manifest + export) is one golden
// test away even before T4.3 exists.
func (s *Staged) EmbeddedPathListEntries() []PathListEntry {
	return []PathListEntry{
		{ISOPath: "/" + media.ManifestFileName, DiskPath: filepath.Join(s.Root, media.ManifestFileName)},
		{ISOPath: "/" + media.ExportFileName, DiskPath: filepath.Join(s.Root, media.ExportFileName)},
	}
}

// Stage materializes the image's payload tree into a fresh staging
// workspace and writes the payload pathlist. Order of operations, all
// before any payload byte is staged and - structurally - before any
// xorriso invocation exists in this package:
//
//  1. validateInput: the --work-dir byte gate (\n, \r, or NUL in the
//     flag is refused - it is the disk side of every graft line), every
//     relpath through the media UTF-8 gate, every media path through the
//     graft gate, duplicate-media-path refusal, inventory shape checks;
//     computes the payload sum and entry count;
//  2. unique scratch via os.MkdirTemp under in.WorkDir (6.2);
//  3. pre-check (a): free space on the staging root (6.2 Statfs);
//  4. pre-check (b): payload + conservative overhead <= UsableSize
//     (6.3 step 5, the clear plan error);
//  5. stage every member with the 5.1 discipline (see the file doc);
//  6. write the sorted payload pathlist.
//
// On any error - including ctx cancellation mid-stage - the scratch is
// removed before returning. On success the caller owns Cleanup.
func Stage(ctx context.Context, deps Deps, in StageInput) (*Staged, error) {
	d := deps.withDefaults()

	payloadBytes, entries, err := validateInput(in)
	if err != nil {
		return nil, err
	}

	// Pre-check (b) needs no filesystem at all, so it runs before the
	// scratch exists: an over-capacity plan fails with zero side
	// effects (the 6.3 step 5 "clear plan error rather than letting
	// xorriso fail at the end of a long build"). The arithmetic is the
	// CANONICAL estimator shared with the planner (units.
	// RequiredImageBytes): since the 2026-10-20 capacity amendment the
	// planner reserves exactly this overhead at fill time, so a
	// planner-produced image always passes; this check remains as the
	// defense-in-depth safety net against estimator drift.
	required := units.RequiredImageBytes(payloadBytes, int64(entries))
	if required > in.UsableSize {
		return nil, capacityError(in, payloadBytes, entries, required)
	}

	root, err := os.MkdirTemp(in.WorkDir, stagingDirPrefix)
	if err != nil {
		return nil, fmt.Errorf("imagebuild: creating staging workspace under %q: %w", workDirLabel(in.WorkDir), err)
	}
	ok := false
	defer func() {
		if !ok {
			// Every failure path - validation surprises, pre-check
			// (a), item failures, ctx cancellation - leaves no
			// scratch behind (the no-leaked-scratch contract).
			_ = os.RemoveAll(root)
		}
	}()

	// Pre-check (a): free space on the filesystem holding the staging
	// root, required ~= payload size (plus the same conservative
	// overhead - one arithmetic, both gates; 6.2).
	avail, err := d.FreeSpace(root)
	if err != nil {
		return nil, fmt.Errorf("imagebuild: free-space pre-check of the staging workspace: %w", err)
	}
	if uint64(required) > avail {
		return nil, freeSpaceError(root, in, payloadBytes, entries, required, avail)
	}

	st := &Staged{Root: root, PathListPath: filepath.Join(root, PathListFileName)}

	s := stager{deps: d, in: in, staged: st}
	if err := s.stageAll(ctx); err != nil {
		return nil, err
	}

	entries2 := buildPathList(s.members)
	st.PathList = entries2
	st.PayloadBytes = payloadBytes
	st.EntryCount = len(s.members)
	if err := WritePathList(st.PathListPath, entries2); err != nil {
		return nil, fmt.Errorf("imagebuild: writing the staging pathlist: %w", err)
	}
	ok = true
	return st, nil
}

// stager carries the per-Stage state (the accumulating member list for
// the pathlist and the warning collector).
type stager struct {
	deps    Deps
	in      StageInput
	staged  *Staged
	members []memberRef // staging order; buildPathList sorts by ISO path
}

// memberRef is one staged member on its way to the pathlist: the media
// path (relative, slash-separated) plus the ISO path and the absolute
// staged disk path.
type memberRef struct {
	mediaPath string
	isoPath   string
	diskPath  string
}

// stageAll stages every inventory member, in the copyengine order
// (files, chunks, symlinks, empty dirs), aborting the image on the
// first failure.
func (s *stager) stageAll(ctx context.Context) error {
	for _, f := range s.in.Inventory.Files {
		if err := ctx.Err(); err != nil {
			return err
		}
		warning, err := s.stageWholeFile(ctx, f)
		if err != nil {
			return fmt.Errorf("imagebuild: staging file %q (image %d of set %q): %w",
				f.RelPath, s.in.ImageID, s.in.SetID, err)
		}
		s.note(warning)
	}
	for _, c := range s.in.Inventory.Chunks {
		if err := ctx.Err(); err != nil {
			return err
		}
		warning, err := s.stageChunk(ctx, c)
		if err != nil {
			return fmt.Errorf("imagebuild: staging chunk %d of %q (image %d of set %q): %w",
				c.ChunkIndex, c.ParentRelPath, s.in.ImageID, s.in.SetID, err)
		}
		s.note(warning)
	}
	for _, sy := range s.in.Inventory.Symlinks {
		if err := ctx.Err(); err != nil {
			return err
		}
		warning, err := s.stageSymlink(ctx, sy)
		if err != nil {
			return fmt.Errorf("imagebuild: staging symlink %q (image %d of set %q): %w",
				sy.RelPath, s.in.ImageID, s.in.SetID, err)
		}
		s.note(warning)
	}
	for _, dir := range s.in.Inventory.Dirs {
		if err := ctx.Err(); err != nil {
			return err
		}
		warning, err := s.stageEmptyDir(dir)
		if err != nil {
			return fmt.Errorf("imagebuild: staging empty dir %q (image %d of set %q): %w",
				dir.RelPath, s.in.ImageID, s.in.SetID, err)
		}
		s.note(warning)
	}
	return nil
}

// note records a metadata warning (progress + collector); a "" warning
// is the common clean case.
func (s *stager) note(warning string) {
	if warning != "" {
		s.staged.Warnings = append(s.staged.Warnings, warning)
		if s.deps.Bus != nil {
			s.deps.Bus.Emit(progress.Warn{Msg: warning})
		}
	}
}

// stagedPath derives the absolute staged path of one media path under
// the workspace root.
func (s *stager) stagedPath(mediaPath string) string {
	return filepath.Join(s.staged.Root, filepath.FromSlash(mediaPath))
}

// emitDone is the nil-safe ItemDone helper (payload members only; dirs
// carry zero bytes and emit none, the copyengine convention).
func (s *stager) emitDone(bytes int64, relPath string) {
	if s.deps.Bus != nil {
		s.deps.Bus.Emit(progress.ItemDone{Bytes: bytes, Path: relPath})
	}
}

// stageWholeFile stages one whole file under files/ with the 5.1
// discipline (see the file doc): D-10 compare, write, verify against
// the CATALOG hash, retry write faults, then metadata.
func (s *stager) stageWholeFile(ctx context.Context, f StageFile) (string, error) {
	mediaPath, err := media.FileMediaPath(f.RelPath)
	if err != nil {
		return "", err
	}
	sourcePath := filepath.Join(s.in.SourceRoot, filepath.FromSlash(f.RelPath))
	dest := s.stagedPath(mediaPath)
	catalogMeta := fsmeta.Meta{Size: f.Size, MtimeNS: f.MtimeNS, Dev: uint64(f.Dev), Inode: uint64(f.Inode)}

	var lastErr error
	for attempt := 1; attempt <= stageMaxAttempts; attempt++ {
		// 3.5 point 3: the same comparison as 5.1 step 1, re-lstat on
		// every attempt (a change between retries must not be staged).
		meta, lerr := s.deps.Source.Lstat(sourcePath)
		if lerr != nil {
			return "", fmt.Errorf("lstat source: %w", lerr)
		}
		if diff := fsmeta.DiffFields(meta, catalogMeta, s.in.Mode); diff.Any() {
			return "", &SourceChangedError{RelPath: f.RelPath, Diff: diff}
		}

		_, err := s.deps.stageContent(ctx, sourcePath, dest, 0, f.Size, f.Hash, false)
		if err == nil {
			warning, err := applyStageMetadata(dest, stageMeta(f.Mode, f.UID, f.GID, f.MtimeNS))
			if err != nil {
				return "", err
			}
			s.addMember(mediaPath)
			s.emitDone(f.Size, f.RelPath)
			return warning, nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		// 5.1 step 7's classification half: re-lstat distinguishes a
		// source that changed mid-stage (build failure, 3.5 point 3)
		// from a write fault on the scratch (retryable).
		if meta2, lerr := s.deps.Source.Lstat(sourcePath); lerr == nil {
			if diff := fsmeta.DiffFields(meta2, catalogMeta, s.in.Mode); diff.Any() {
				return "", &SourceChangedError{RelPath: f.RelPath, Diff: diff}
			}
		}
		lastErr = err
		if attempt < stageMaxAttempts {
			if serr := stageSleep(ctx, stageBackoff(attempt)); serr != nil {
				return "", serr
			}
		}
	}
	return "", lastErr
}

// stageChunk stages one chunk under chunks/ (media.ChunkMediaPath,
// 1-based on-media name). The D-10 compare runs against the PARENT's
// catalog snapshot; a first-write chunk (Hash nil) is captured during
// the write and the capture stored through HashStore (4.3, 6.3 step 1).
// The source read is the chunk's TRUE byte range: c.Offset is threaded
// into Source.Open (the copyengine ranged-read seam, exactly like
// copyengine.CopyItem passes item.Offset), so chunk N stages
// parent[Offset:Offset+Length] and never the parent's prefix - a
// hardcoded-0 offset would stage (and for first-write chunks PERSIST to
// the catalog) the wrong bytes for every chunk past the first, so
// TestStageNonUniformChunkRangesPinOffsets pins it with non-uniform
// parent content. Chunk parts get NO metadata application - a chunk is
// not a restored object (the copyengine convention; the parent's
// metadata travels in the manifest/export and is applied by restore,
// 8.4).
func (s *stager) stageChunk(ctx context.Context, c StageChunk) (string, error) {
	mediaPath, err := media.ChunkMediaPath(c.ParentRelPath, int64(c.ChunkIndex))
	if err != nil {
		return "", err
	}
	sourcePath := filepath.Join(s.in.SourceRoot, filepath.FromSlash(c.ParentRelPath))
	dest := s.stagedPath(mediaPath)
	catalogMeta := fsmeta.Meta{
		Size: c.ParentSize, MtimeNS: c.ParentMtimeNS,
		Dev: uint64(c.ParentDev), Inode: uint64(c.ParentInode),
	}
	firstWrite := len(c.Hash) == 0

	var lastErr error
	for attempt := 1; attempt <= stageMaxAttempts; attempt++ {
		meta, lerr := s.deps.Source.Lstat(sourcePath)
		if lerr != nil {
			return "", fmt.Errorf("lstat source parent: %w", lerr)
		}
		if diff := fsmeta.DiffFields(meta, catalogMeta, s.in.Mode); diff.Any() {
			return "", &SourceChangedError{RelPath: c.ParentRelPath, Diff: diff}
		}

		captured, err := s.deps.stageContent(ctx, sourcePath, dest, c.Offset, c.Length, c.Hash, firstWrite)
		if err == nil {
			if firstWrite {
				// 6.3 step 1: persist the capture BEFORE the member
				// counts as staged, so T4.3's content list (6.6) can
				// never see a NULL chunk hash.
				if s.deps.HashStore == nil {
					return "", fmt.Errorf(
						"chunk %d of %q is a first-write (no stored hash) but Deps.HashStore is nil; the 6.3 step 1 capture cannot be stored",
						c.ChunkIndex, c.ParentRelPath)
				}
				if err := s.deps.HashStore.SetChunkHash(ctx, c.FileID, c.ChunkIndex, captured); err != nil {
					return "", fmt.Errorf("storing first-write chunk hash: %w", err)
				}
			}
			s.addMember(mediaPath)
			s.emitDone(c.Length, c.ParentRelPath)
			return "", nil
		}
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if meta2, lerr := s.deps.Source.Lstat(sourcePath); lerr == nil {
			if diff := fsmeta.DiffFields(meta2, catalogMeta, s.in.Mode); diff.Any() {
				return "", &SourceChangedError{RelPath: c.ParentRelPath, Diff: diff}
			}
		}
		lastErr = err
		if attempt < stageMaxAttempts {
			if serr := stageSleep(ctx, stageBackoff(attempt)); serr != nil {
				return "", serr
			}
		}
	}
	return "", lastErr
}

// stageSymlink recreates one symlink with its recorded target (D-03:
// preserve, never follow) and applies the link's metadata (lchown +
// utimes no-follow; fsmeta.Apply skips chmod for symlinks). The D-10
// compare is limited to the fields the symlink inventory carries -
// size (= len(target)) and mtime_ns; catalog.CopySymlink carries no
// dev/inode, so a target swap that preserves both is not detectable at
// staging (documented; copy mode does no symlink compare at all, so
// staging is strictly stronger, never weaker).
func (s *stager) stageSymlink(ctx context.Context, sy StageSymlink) (string, error) {
	mediaPath, err := media.FileMediaPath(sy.RelPath)
	if err != nil {
		return "", err
	}
	sourcePath := filepath.Join(s.in.SourceRoot, filepath.FromSlash(sy.RelPath))
	dest := s.stagedPath(mediaPath)

	meta, err := s.deps.Source.Lstat(sourcePath)
	if err != nil {
		return "", fmt.Errorf("lstat source: %w", err)
	}
	var diff fsmeta.Diff
	if meta.Size != sy.Size {
		diff |= fsmeta.DiffSize
	}
	if meta.MtimeNS != sy.MtimeNS {
		diff |= fsmeta.DiffMtimeNS
	}
	if diff.Any() {
		return "", &SourceChangedError{RelPath: sy.RelPath, Diff: diff}
	}

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("creating staged directory: %w", err)
	}
	if err := os.Symlink(sy.LinkTarget, dest); err != nil {
		return "", fmt.Errorf("creating symlink: %w", err)
	}
	// Read-back verify (the specials convention: no hash exists for a
	// link, so target equality IS the verification).
	readback, err := os.Readlink(dest)
	if err != nil {
		return "", fmt.Errorf("read-back verify: %w", err)
	}
	if readback != sy.LinkTarget {
		_ = os.Remove(dest)
		return "", fmt.Errorf("read-back target %q != recorded %q", readback, sy.LinkTarget)
	}
	warning, err := applyStageMetadata(dest, stageMeta(sy.Mode, sy.UID, sy.GID, sy.MtimeNS))
	if err != nil {
		return "", err
	}
	s.addMember(mediaPath)
	s.emitDone(sy.Size, sy.RelPath)
	return warning, nil
}

// stageEmptyDir recreates one empty directory with its recorded
// metadata (FR-037): mkdir -> Apply -> parent fsync, the copyengine
// WriteEmptyDir ordering (metadata before the parent fsync so the
// journal commit carries it). Idempotent like every mkdir (2.3).
func (s *stager) stageEmptyDir(dir StageDir) (string, error) {
	mediaPath, err := media.FileMediaPath(dir.RelPath)
	if err != nil {
		return "", err
	}
	dest := s.stagedPath(mediaPath)
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", fmt.Errorf("creating staged directory: %w", err)
	}
	warning, err := applyStageMetadata(dest, stageMeta(dir.Mode, dir.UID, dir.GID, dir.MtimeNS))
	if err != nil {
		return "", err
	}
	if err := syncDir(filepath.Dir(dest)); err != nil {
		return "", fmt.Errorf("fsync parent of staged dir: %w", err)
	}
	s.addMember(mediaPath)
	return warning, nil
}

// addMember registers a successfully staged member for the pathlist.
func (s *stager) addMember(mediaPath string) {
	s.members = append(s.members, memberRef{
		mediaPath: mediaPath,
		isoPath:   "/" + mediaPath,
		diskPath:  s.stagedPath(mediaPath),
	})
}

// --- the 5.1-minus-mark write core (EmbedFile's shape) -----------------------

// stageContent streams length bytes of source at srcPath, starting at
// offset, into <dest>.partial through the 5.1 discipline (stale-partial
// removal, write, fsync, close, atomic rename, directory fsync,
// read-back hash) and verifies the staged bytes against the anchor.
// capture==true (first-write chunks, 4.3) also returns the source-stream
// hash; the anchor for that case IS the capture, so the read-back
// compares against it. On any failure the partial and any renamed final
// are removed, leaving no unverified bytes at dest.
func (d Deps) stageContent(ctx context.Context, srcPath, dest string, offset, length int64, anchor []byte, capture bool) ([]byte, error) {
	partial := dest + ".partial"
	if err := os.Remove(partial); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("removing stale partial %s: %w", partial, err)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return nil, fmt.Errorf("creating staged directory for %s: %w", dest, err)
	}

	// The ranged read is THE byte range of this item: a whole file
	// starts at 0, a chunk at its catalog Offset (the copyengine
	// SourceFS seam - offset seeks, length limits).
	src, err := d.Source.Open(srcPath, offset, length)
	if err != nil {
		return nil, fmt.Errorf("opening source: %w", err)
	}
	partialFile, err := d.OpenPartial(partial)
	if err != nil {
		_ = src.Close()
		return nil, fmt.Errorf("creating partial %s: %w", partial, err)
	}

	var hasher interface{ Sum([]byte) []byte }
	var sink io.Writer = partialFile
	if capture {
		h := sha256.New()
		hasher = h
		sink = io.MultiWriter(partialFile, h)
	}
	n, cerr := io.Copy(sink, &stageCtxReader{ctx: ctx, r: src})
	if cerr != nil {
		_ = partialFile.Close()
		_ = os.Remove(partial)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("writing staged payload: %w", cerr)
	}
	if n != length {
		_ = partialFile.Close()
		_ = os.Remove(partial)
		return nil, fmt.Errorf("payload length mismatch: staged %d bytes, catalog record says %d", n, length)
	}
	if err := src.Close(); err != nil {
		_ = partialFile.Close()
		_ = os.Remove(partial)
		return nil, fmt.Errorf("closing source: %w", err)
	}
	if err := partialFile.Sync(); err != nil {
		_ = partialFile.Close()
		_ = os.Remove(partial)
		return nil, fmt.Errorf("fsync partial %s: %w", partial, err)
	}
	if err := partialFile.Close(); err != nil {
		_ = os.Remove(partial)
		return nil, fmt.Errorf("closing partial %s: %w", partial, err)
	}
	if err := os.Rename(partial, dest); err != nil {
		_ = os.Remove(partial)
		return nil, fmt.Errorf("renaming %s to %s: %w", partial, dest, err)
	}
	if err := syncDir(filepath.Dir(dest)); err != nil {
		_ = os.Remove(dest)
		return nil, fmt.Errorf("fsync directory of %s: %w", dest, err)
	}

	readHash, err := d.hashStaged(dest)
	if err != nil {
		_ = os.Remove(dest)
		return nil, fmt.Errorf("read-back verify: %w", err)
	}
	effective := anchor
	var captured []byte
	if capture {
		captured = hasher.Sum(nil)
		effective = captured
	}
	if !bytes.Equal(readHash, effective) {
		_ = os.Remove(dest)
		return nil, fmt.Errorf("read-back hash mismatch: staged %x, expected %x", readHash, effective)
	}
	return captured, nil
}

// hashStaged streams a staged file through SHA-256 (the read-back
// verify; a BARE io.Copy, i.e. the stdlib's own 32 KiB copy buffer -
// there is no explicit 1 MiB buffer on this path).
func (d Deps) hashStaged(path string) ([]byte, error) {
	f, err := d.OpenReadBack(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only close; the hash result dominates
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, fmt.Errorf("hashing %s: %w", path, err)
	}
	return h.Sum(nil), nil
}

// OpenReadBack is the read-back verify open seam: the default is
// os.Open. Split out as a Deps method so the fault seam stays minimal
// (OpenPartial covers the write side; tests that need read-back faults
// can wrap OpenReadBack via the same Deps field pattern when T4.3 needs
// it - at T4.2 the discipline is exercised through the real OS path).
func (d Deps) OpenReadBack(path string) (io.ReadCloser, error) {
	return os.Open(path)
}

// --- metadata and small shared helpers ---------------------------------------

// stageMeta converts catalog-shaped metadata (raw POSIX permission
// bits, numeric ids, unix-ns mtime - the 2.3 INTEGER column
// conventions) into the fsmeta.Meta that fsmeta.Apply consumes. This is
// the copyengine.metaFromRaw SHAPE re-derived through the same T3.4
// conversion chain (fsmeta.GoMode, the canonical inverse of
// fsmeta.UnixMode): the catalog stores RAW POSIX and Apply/Chmod read
// GO fs.FileMode semantics, so skipping GoMode would silently drop
// setuid from a raw 0o4755 - the T1.2 QA finding. The two writers share
// the CHAIN by construction (both call fsmeta.GoMode at their single
// apply seam), not by copy-paste; the staged-tree setuid proof pins it
// end to end.
func stageMeta(rawMode uint32, uid, gid uint32, mtimeNS int64) fsmeta.Meta {
	return fsmeta.Meta{
		Mode:    fsmeta.GoMode(rawMode),
		UID:     uid,
		GID:     gid,
		MtimeNS: mtimeNS,
	}
}

// applyStageMetadata runs fsmeta.Apply on a staged path and folds the
// result into the (warning, error) pair the stage* callers return: the
// 5.4 EPERM chown downgrade is a WARNING (non-root staging cannot
// chown to recorded owners - exactly the restore-side case), every
// OTHER metadata failure is a hard error (metadata is payload for Rock
// Ridge; see the file doc).
func applyStageMetadata(path string, meta fsmeta.Meta) (string, error) {
	res, err := fsmeta.Apply(path, meta)
	if err != nil {
		return "", fmt.Errorf("applying catalog metadata: %w", err)
	}
	return res.Warning, nil
}

// stageCtxReader aborts a long stream the moment the context is
// cancelled (a SIGINT must not wait inside a multi-gigabyte io.Copy).
type stageCtxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *stageCtxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// stageBackoff is the 5.1 step 7 retry wait (100ms, 200ms - the
// copyengine default's shape; staging retries only scratch write
// faults).
func stageBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	return 100 * time.Millisecond << (attempt - 1)
}

// stageSleep waits for d or until ctx is done (a cancelled stage must
// never sit out a backoff delay).
func stageSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// syncDir fsyncs a directory (the 5.1 step 4 crash-durability half).
// EINVAL/ENOTSUP are tolerated - filesystems that do not implement
// directory fsync report them, and refusing to complete an otherwise
// verified stage would trade no real durability for a hard failure
// (the copyengine syncDir contract, mirrored).
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close() //nolint:errcheck // best-effort close after Sync
	if err := d.Sync(); err != nil {
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
			return nil
		}
		return err
	}
	return nil
}

// withDefaults fills the optional seams with production implementations.
func (d Deps) withDefaults() Deps {
	if d.FreeSpace == nil {
		d.FreeSpace = statfsFreeSpace
	}
	if d.Source == nil {
		d.Source = copyengine.OSSource{}
	}
	if d.OpenPartial == nil {
		d.OpenPartial = func(path string) (copyengine.WriteSyncCloser, error) {
			return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
		}
	}
	return d
}

// --- input validation (the gates, run before any filesystem effect) ----------

// validateInput enforces the StageInput contract and computes the
// accounting both pre-checks share: the payload byte sum and the entry
// count. The --work-dir byte gate runs first (workDirGate, precheck.go),
// then every relpath goes through the media UTF-8 gate (carried via the
// media path builders), every media path through the graft gate, and
// duplicate media paths are refused (a corrupt catalog listing the same
// member twice would otherwise produce a pathlist xorriso masters
// twice, silently).
func validateInput(in StageInput) (payloadBytes int64, entries int, err error) {
	// The --work-dir byte gate runs FIRST and before os.MkdirTemp
	// (see workDirGate in precheck.go): a WorkDir carrying \n, \r, or
	// NUL would corrupt the DISK side of EVERY graft line, so it is a
	// Usage-class flag refusal with zero filesystem side effects.
	if err := workDirGate(in.WorkDir); err != nil {
		return 0, 0, err
	}
	if in.ImageID < 1 {
		return 0, 0, fmt.Errorf("imagebuild: image id %d must be >= 1 (images are 1-based, 6.4)", in.ImageID)
	}
	if in.SourceRoot == "" {
		return 0, 0, errors.New("imagebuild: empty source root (sets.source_root must be read before staging)")
	}
	if in.UsableSize <= 0 {
		// The images.usable_size column is NOT NULL and the planner
		// always writes a positive profile figure; a non-positive value
		// here is a wiring bug, not user input.
		return 0, 0, fmt.Errorf("imagebuild: usable size %d is not positive (the images row must be read before staging)", in.UsableSize)
	}

	seen := make(map[string]string) // mediaPath -> "kind relpath" for the duplicate message
	claim := func(kind, relPath, mediaPath string) error {
		if err := graftGate(mediaPath); err != nil {
			return fmt.Errorf("imagebuild: refusing to stage %s %q (media path %q): %w", kind, relPath, mediaPath, err)
		}
		if other, dup := seen[mediaPath]; dup {
			return fmt.Errorf("imagebuild: media path %q is claimed by both %s and %s %q (corrupt inventory)",
				mediaPath, other, kind, relPath)
		}
		seen[mediaPath] = kind + " " + relPath
		return nil
	}

	for _, f := range in.Inventory.Files {
		mp, err := media.FileMediaPath(f.RelPath)
		if err != nil {
			return 0, 0, fmt.Errorf("imagebuild: refusing to stage file %q: %w", f.RelPath, err)
		}
		if err := claim("file", f.RelPath, mp); err != nil {
			return 0, 0, err
		}
		switch {
		case f.Size < 0:
			return 0, 0, fmt.Errorf("imagebuild: file %q has negative size %d", f.RelPath, f.Size)
		case len(f.Hash) != sha256.Size:
			// The copyengine contract: an active regular row always
			// carries its catalog hash; staging without an anchor would
			// write bytes nothing can verify (and 6.6 would record a
			// NULL content hash) - refused loudly as corrupt catalog.
			return 0, 0, fmt.Errorf("imagebuild: whole file %q carries a %d-byte hash, want 32 (sha256); unreadable-class rows never reach media",
				f.RelPath, len(f.Hash))
		}
		payloadBytes += f.Size
		entries++
	}
	for _, c := range in.Inventory.Chunks {
		mp, err := media.ChunkMediaPath(c.ParentRelPath, int64(c.ChunkIndex))
		if err != nil {
			return 0, 0, fmt.Errorf("imagebuild: refusing to stage chunk %d of %q: %w", c.ChunkIndex, c.ParentRelPath, err)
		}
		if err := claim("chunk", fmt.Sprintf("%s#%d", c.ParentRelPath, c.ChunkIndex), mp); err != nil {
			return 0, 0, err
		}
		switch {
		case c.Length <= 0:
			return 0, 0, fmt.Errorf("imagebuild: chunk %d of %q has non-positive length %d", c.ChunkIndex, c.ParentRelPath, c.Length)
		case c.Offset < 0:
			return 0, 0, fmt.Errorf("imagebuild: chunk %d of %q has negative offset %d", c.ChunkIndex, c.ParentRelPath, c.Offset)
		case len(c.Hash) != 0 && len(c.Hash) != sha256.Size:
			return 0, 0, fmt.Errorf("imagebuild: chunk %d of %q carries a malformed stored hash (%d bytes)",
				c.ChunkIndex, c.ParentRelPath, len(c.Hash))
		}
		payloadBytes += c.Length
		entries++
	}
	for _, sy := range in.Inventory.Symlinks {
		mp, err := media.FileMediaPath(sy.RelPath)
		if err != nil {
			return 0, 0, fmt.Errorf("imagebuild: refusing to stage symlink %q: %w", sy.RelPath, err)
		}
		if err := claim("symlink", sy.RelPath, mp); err != nil {
			return 0, 0, err
		}
		if sy.LinkTarget == "" {
			return 0, 0, fmt.Errorf("imagebuild: symlink %q has no recorded target (a scan never stores one)", sy.RelPath)
		}
		if !utf8.ValidString(sy.LinkTarget) {
			// The T3.1 UTF-8 gate for link targets, carried into
			// staging: the target travels into the JSON MANIFEST (6.6).
			return 0, 0, fmt.Errorf("imagebuild: refusing to stage symlink %q: %w (target %q)",
				sy.RelPath, ErrUnsafeSymlinkTarget, sy.LinkTarget)
		}
		entries++
	}
	for _, dir := range in.Inventory.Dirs {
		mp, err := media.FileMediaPath(dir.RelPath)
		if err != nil {
			return 0, 0, fmt.Errorf("imagebuild: refusing to stage dir %q: %w", dir.RelPath, err)
		}
		if err := claim("dir", dir.RelPath, mp); err != nil {
			return 0, 0, err
		}
		entries++
	}
	// The two image-root members T4.3 embeds (MANIFEST.json,
	// catalog-export.sqlite) are also ISO entries, but their cost is
	// NOT added here: since the 2026-10-20 capacity amendment the
	// canonical estimator (units.RequiredImageBytes) carries those two
	// entries in its FIXED term (units.FixedImageOverhead = 2 MiB +
	// 512*2), so this count stays the PAYLOAD entry count - files +
	// chunks + symlinks + dirs, exactly what the planner reserved per
	// image (DESIGN_SPEC 4.3). Their BYTES are not counted anywhere -
	// their sizes are not knowable before T4.3 generates them; the
	// 2 MiB base allowance is the conservative cushion for both.
	return payloadBytes, entries, nil
}

// graftGate refuses a media path the line-oriented -graft-points
// pathlist cannot express (see the file doc): a newline or CR would
// split the line, a '=' would misparse the iso=disk split at xorriso's
// first '='.
func graftGate(mediaPath string) error {
	for i := 0; i < len(mediaPath); i++ {
		switch mediaPath[i] {
		case '\n', '\r', '=':
			return fmt.Errorf("%w: contains %q", ErrUngraftablePath, string(mediaPath[i]))
		}
	}
	return nil
}

// workDirLabel renders the --work-dir value for messages ("" is the OS
// temp directory, 6.2).
func workDirLabel(workDir string) string {
	if workDir == "" {
		return "OS temp dir (--work-dir default)"
	}
	return workDir
}
