package restore

// T5.3: the restore engine's data surface - the read-only Store seam, the
// row shapes it speaks, the selection result, the per-image inventory mirror,
// the T5.3/T5.4 ImageRestorer seam, and the typed errors this package
// returns (DESIGN_SPEC 8.1-8.3, 10.1; IMPLEMENTATION_PLAN 4.6 T5.3).
//
// WHY EVERYTHING HERE IS PRIMITIVE-SHAPED: DESIGN_SPEC 1.2 makes the catalog
// package the only SQL issuer and keeps services off the concrete driver, so
// internal/restore declares these shapes and the cli layer - the one layer
// allowed to know both sides - maps *catalog.DB's rows onto them (the
// copyengine Store / verify.VerifyStore precedent). The inventory mirror
// below is the copy-side inventory (catalog.CopyInventory, minus the copied
// flags restore has no use for: restore takes no runs row and has no resume
// state), which already carries the T3.4 unified hard-link claim rule, so
// what arrives here claims only members whose BYTES live on that image.

import (
	"context"
	"fmt"

	"isopress/internal/progress"
	"isopress/internal/xorriso"
)

// --- the read-only store seam ----------------------------------------------

// Store is the restore engine's read-only catalog seam. Every method is a
// pure read usable in NO transaction and under NO advisory lock (DESIGN_SPEC
// 2.1 Option B); the cli adapts *catalog.DB onto it.
type Store interface {
	// ActiveRecords returns every catalog-ACTIVE files row of the set (the
	// v_active_files predicate: status='active' AND superseded_by IS NULL),
	// which is what makes the run LATEST-STATE only (8.2, D-07): a modified
	// or deleted path is never selected, because its older row is no longer
	// active.
	ActiveRecords(ctx context.Context, setID string) ([]Record, error)
	// ActiveDirectories returns every catalog-ACTIVE directories row (the
	// durable empty-dir records of 2.3/FR-037), assigned or not - an
	// unassigned one surfaces as a selected-but-unclaimed report entry
	// instead of vanishing from the run.
	ActiveDirectories(ctx context.Context, setID string) ([]DirRecord, error)
	// Placement returns the whole-set placement summary: the payload
	// relpaths each image claims (catalog.CopyPlacement - files, chunked
	// parents under EVERY image holding a chunk, empty dirs, and hard-link
	// members under their PRIMARY's image, i.e. the unified T3.4 claim rule
	// already applied). This is what resolves a selection to image numbers.
	Placement(ctx context.Context, setID string) ([]ImagePaths, error)
	// ImageInventory returns one image's complete write inventory
	// (catalog.CopyInventory) for the per-image work list.
	ImageInventory(ctx context.Context, setID string, imageID int64) (Inventory, error)
}

// Record is one catalog-ACTIVE files row as the selection consumes it.
type Record struct {
	FileID    int64
	RelPath   string
	EntryKind string // "regular" | "symlink" (the 2.3 CHECK vocabulary)
	Size      int64  // bytes for regular rows, len(target) for symlink rows
	Hash      []byte // whole-file sha256 (nil for symlink rows)
	// LinkTarget is the recorded symlink target (D-03), "" for regular rows.
	LinkTarget string
}

// DirRecord is one catalog-ACTIVE directories row (an empty directory is a
// first-class record: 2.3, FR-037).
type DirRecord struct {
	DirID   int64
	RelPath string
}

// ImagePaths is one image's claimed payload paths (the placement summary of
// FR-033). Paths are ascending; images arrive ascending.
type ImagePaths struct {
	ImageID int64
	Paths   []string
}

// --- the per-image inventory mirror (catalog.CopyInventory, minus copied) ---

// FileItem is one whole regular file (or a hard-link family PRIMARY) assigned
// to the image.
type FileItem struct {
	FileID   int64
	RelPath  string
	Size     int64
	Mode     uint32
	UID      uint32
	GID      uint32
	MtimeNS  int64
	Dev      int64
	Inode    int64
	Nlink    int64
	Hash     []byte // 32-byte sha256 (D-09)
	Chunked  bool   // defensive stamp: a chunked parent normally has no image_id
	ChunkCnt int
}

// SymlinkItem is one symlink row assigned to the image (D-03: target verbatim).
type SymlinkItem struct {
	FileID     int64
	RelPath    string
	LinkTarget string
	Size       int64 // len(target) per 2.3
	Mode       uint32
	UID        uint32
	GID        uint32
	MtimeNS    int64
}

// ChunkItem is one chunks row assigned to the image, joined with its parent's
// reassembly anchors (the 8.4 engine consumes exactly these: offset, length,
// the stored chunk hash for the FIRST verification, and the parent's
// whole-file hash for the SECOND).
type ChunkItem struct {
	ChunkID    int64
	FileID     int64
	ChunkIndex int // catalog 0-based (media name = index+1, media.ChunkFileName)
	Offset     int64
	Length     int64
	Hash       []byte // stored chunk hash; nil only while first-write is pending

	ParentRelPath   string
	ParentSize      int64
	ParentMode      uint32
	ParentUID       uint32
	ParentGID       uint32
	ParentMtimeNS   int64
	ParentDev       int64
	ParentInode     int64
	ParentHash      []byte // whole-file sha256 anchor
	ParentNumChunks int    // the parent's TOTAL chunk count (this image may hold a subset)
}

// DirItem is one empty directory assigned to the image (FR-037).
type DirItem struct {
	DirID   int64
	RelPath string
	Mode    uint32
	UID     uint32
	GID     uint32
	MtimeNS int64
}

// LinkItem is one hard-link SECONDARY whose family PRIMARY is on this image:
// the bytes exist as link(2) next to the primary in a copy tree, and as the
// PRIMARY's name only in an ISO (D-04), so PrimaryRelPath is where T5.4
// reads the content from while RelPath is the name it must create (the 8.2
// partial-selection rule: content once, links for the requested names).
type LinkItem struct {
	FileID         int64
	RelPath        string
	Size           int64
	Mode           uint32
	UID            uint32
	GID            uint32
	MtimeNS        int64
	Hash           []byte // shared family hash
	PrimaryFileID  int64
	PrimaryRelPath string
}

// Inventory is one image's complete payload inventory (5.3), filtered to the
// selection before it reaches the T5.4 seam.
type Inventory struct {
	Files       []FileItem
	Symlinks    []SymlinkItem
	Chunks      []ChunkItem
	Dirs        []DirItem
	Secondaries []LinkItem
}

// --- selection --------------------------------------------------------------

// Selector names WHAT to restore: a whole set (neither field set), repeatable
// exact --path entries, a --subtree prefix, or both selectors together (their
// union - the design shows them as alternatives, nothing forbids combining,
// and a union is the only interpretation that keeps each selector's
// "must match something" gate meaningful).
type Selector struct {
	SetID string
	Paths []string // --path: exact catalog relpaths
	// Subtree is --subtree: a component-aware relpath prefix ("docs" matches
	// "docs" and "docs/a/b", never "docs-old" - a bare byte prefix would
	// silently pull a sibling tree into a restore, DESIGN_SPEC 8.2's
	// "prefix match on relpath" read as a directory prefix).
	Subtree string
}

// Mode reports the selection vocabulary for the report's frozen "selection"
// value: "whole-set" | "path" | "subtree" | "path+subtree".
func (s Selector) Mode() string {
	switch {
	case len(s.Paths) > 0 && s.Subtree != "":
		return "path+subtree"
	case len(s.Paths) > 0:
		return "path"
	case s.Subtree != "":
		return "subtree"
	default:
		return "whole-set"
	}
}

// ImageWork is ONE required image's contribution to the run: its number, a
// representative selected path that needs it (the 8.3 prompt's <path>), and
// its selection-filtered inventory.
type ImageWork struct {
	Image int64
	// NeededFor is one selected relpath this image holds - the path the 8.3
	// prompt and the non-TTY listing name ("Image N of set S is required for
	// <path>"), chosen as the alphabetically first selected path the image
	// claims so both presentations are deterministic.
	NeededFor string
	Inv       Inventory
}

// ResolvedSelection is the outcome of selection resolution: the target, the
// selected names, the required images (ascending, with their work lists), the
// progress totals, and the selected paths NO image claims.
//
// Note there is no source root here: the run never reads sets.source_root and
// can never restore onto the original source implicitly (8.2).
type ResolvedSelection struct {
	SetID  string
	Target string // absolute --target root, filled by Run (8.2: required, never implicit)
	Mode   string // Selector.Mode()
	// Paths are the selected relpaths (records AND empty dirs), ascending.
	Paths []string
	// Work holds every required image, ascending, with its filtered
	// inventory. Images holding no selected payload are not required and do
	// not appear (the loop restores nothing from an image with nothing to
	// give).
	Work []ImageWork
	// Files and Bytes are the SELECTION totals for progress (8.4:
	// "determinate by bytes and files, both known from the selection"):
	// matched records (regular + symlink) and their recorded sizes.
	Files int64
	Bytes int64
	// Unclaimed are selected paths no image claims - records scanned after
	// the last plan, unassigned dirs, a set never planned. They are honest
	// per-item failures (exit 10), never a silent skip.
	Unclaimed []string
}

// WorkFor returns the work list of one required image.
func (r ResolvedSelection) WorkFor(imageID int64) (ImageWork, bool) {
	for _, w := range r.Work {
		if w.Image == imageID {
			return w, true
		}
	}
	return ImageWork{}, false
}

// --- image supply -----------------------------------------------------------

// SourceKind classifies where an image's bytes come from. An ISO file and a
// copy TREE have different shapes on disk, so the kind always travels with
// the path: --image-dir discovers .iso files only, --copy-tree-root supplies
// S-NNN directories only, and --image N=PATH is classified by stat (a regular
// file is an ISO, a directory is a copy tree).
type SourceKind string

const (
	// SourceISO is an .iso file, read through xorriso extraction (T5.4).
	SourceISO SourceKind = "iso"
	// SourceCopyTree is an S-NNN copy-set directory, read directly (T5.4).
	SourceCopyTree SourceKind = "copy"
)

// ExplicitImage is one parsed --image N=PATH mapping.
type ExplicitImage struct {
	Image int64
	Path  string
}

// SupplySpec is the 8.3 image-supply input: the three sources, in the
// design's stated order of preference - --image-dir, then explicit
// --image N=PATH mappings, then --copy-tree-root (DESIGN_SPEC 8.3 lists them
// in that order, and this package follows the text literally: the first
// source that yields an existing path for image N wins; the documented
// consequence is that a --image-dir copy of image N beats a same-number
// explicit mapping, so the two should not be used to point at different
// shapes of one image number).
type SupplySpec struct {
	ImageDir     string // --image-dir: <dir>/<SET-ID>-NNN.iso discovery
	CopyTreeRoot string // --copy-tree-root: <root>/<SET-ID>-NNN/ discovery
	Explicit     []ExplicitImage
	// Media is the T5.5 --from-media base supply: image number -> the
	// copy-tree directory ResolveMedia derived from the media's own
	// layout (restore/media.go). Consulted FIRST, before the three 8.3
	// sources above - see supply.go's file doc for the precedence
	// reading and why the 8.3 relative order stays untouched. nil (or
	// an image the map does not carry) means no media base for that
	// image: the 8.3 sources decide exactly as before.
	Media map[int64]string
}

// ResolvedSupply is one required image's supply outcome: a found image
// (Kind and Path set, Missing false) or a missing one (Kind "", Missing true,
// Sought listing every candidate the preference order consulted).
type ResolvedSupply struct {
	Image   int64
	Kind    SourceKind
	Path    string
	Sought  []string
	Missing bool
	// Reason explains a miss for diagnostics ("" when found), e.g. "exists
	// but is not a regular file".
	Reason string
}

// --- the T5.3/T5.4 seam -----------------------------------------------------

// ImageRestorer is THE T5.3/T5.4 seam: T5.3 resolves which images are needed
// and supplies them one at a time; T5.4 implements this interface to restore
// the CONTENT of one image (extraction from an ISO or a copy tree, chunked
// reassembly with the 8.4 double verification, metadata applied to the
// target). See the package doc for each argument's contract.
type ImageRestorer interface {
	RestoreImage(ctx context.Context, image ImageSupply, selection ResolvedSelection, workspace string) (ImageRestoreResult, error)
}

// ImageSupply is where ONE image's bytes come from (the first seam argument).
type ImageSupply struct {
	SetID string
	Image int64
	Kind  SourceKind
	Path  string // absolute .iso path or copy-tree directory
	// Sought is what supply discovery looked for before it found this path
	// (the 8.3 candidate list), carried for the report's diagnostics.
	Sought []string
}

// FileOutcome is ONE member's restoration outcome as T5.4 records it.
type FileOutcome struct {
	RelPath string
	Status  string // restored | skipped | failed | incomplete (the chunked 8.4/FR-041 member class)
	Bytes   int64
	// Detail carries the skip/failure reason (empty for a clean restore):
	// the hash mismatch text, the missing chunk indices of 8.4/FR-041, the
	// EPERM chown warning, and so on. A RESTORED member carries a Detail
	// only when it completed with a 5.4 warning (metadata downgrade), which
	// the report counts as restored and never lists as a failure.
	Detail string
}

// ImageRestoreResult is ONE image's restoration outcome (T5.4 fills it; the
// recording fake used by T5.3's tests fills the same shape).
//
// Count contract: Restored / Skipped / Failed are the image's totals;
// Files enumerates the members that carry DETAIL. T5.4 must record EVERY
// non-restored member in Files (the report enumerates exactly those), while
// Restored may count members that are not enumerated - clean restores are
// counted, never listed (the validate/verify-media report convention: a
// 100k-file restore must not print 100k lines into a cron log).
type ImageRestoreResult struct {
	Image    int64
	Restored int
	Skipped  int
	Failed   int
	Bytes    int64 // bytes written to the target for this image
	Files    []FileOutcome
}

// RestorerDeps are the per-image restorer's injected dependencies. T5.4's
// NewImageRestorer consumes them; T5.3's orchestration is unaffected (the
// seam signature and every orchestration code path stand).
type RestorerDeps struct {
	// Bus is the run's progress bus (9.1): the restorer emits ItemDone per
	// member, Warn for 5.4 metadata downgrades, and Error per failed member
	// inside the phase T5.3 declares per image. nil means no progress.
	Bus *progress.Bus
	// Tool is the T4.1 xorriso handle used for SourceISO images (the one
	// osirrox extraction per image). The cli discovers it alongside the
	// up-front probe; a copy-tree-only run never needs it, and an ISO image
	// reaching the restorer without one is an honest per-image failure (the
	// probe runs before the loop, so production cannot get there).
	Tool *xorriso.Tool
	// FreeSpace reports the unprivileged available bytes under a path: the
	// DESIGN_SPEC 7.2 extraction pre-check (verify-media's FreeSpaceFunc
	// seam, same signature and default). nil means statfsFreeSpace.
	FreeSpace FreeSpaceFunc
}

// FreeSpaceFunc is the 7.2 free-space pre-check seam the restorer runs over
// the extraction workspace before touching xorriso (default statfs; tests
// inject a sentinel).
type FreeSpaceFunc func(path string) (availBytes uint64, err error)

// --- typed errors (the cli maps these to the 10.2 exit classes) -------------

// InputError classifies a restore INPUT failure - a selection that matches
// nothing, a --path/--subtree value that is not a usable catalog relpath, a
// malformed --image N=PATH, or a supply directory that does not exist. The cli
// maps it to the usage class, exit 2 (DESIGN_SPEC 10.1). This package never
// imports internal/report: it returns classes, not codes.
type InputError struct {
	Msg string
}

func (e *InputError) Error() string { return "restore: " + e.Msg }

// inputErrorf builds an InputError.
func inputErrorf(format string, args ...any) error {
	return &InputError{Msg: fmt.Sprintf(format, args...)}
}

// MissingImage is one required image the supply pass could not provide.
type MissingImage struct {
	SetID     string
	Image     int64
	Sought    []string
	NeededFor string
}

// MissingImagesError classifies the NON-TTY missing-media abort of 8.3 /
// RCR-3: stderr is not a terminal, so no prompt is issued; the run prints the
// machine-readable required-image listing (one "required-image:" line per
// entry, emitted on the progress bus) and fails. The cli maps this to the
// dependency class, exit 3 - see internal/cli/restore.go for why 3 is the
// code the ratified 5.5 resolution points at.
type MissingImagesError struct {
	Missing []MissingImage
}

func (e *MissingImagesError) Error() string {
	return fmt.Sprintf("%d required image(s) unavailable and stderr is not a terminal, so no prompt was issued "+
		"(DESIGN_SPEC 8.3, RCR-3); see the required-image lines above - supply them with --image-dir, "+
		"--image N=PATH, or --copy-tree-root", len(e.Missing))
}

// AbortedError classifies the interactive abort: the user answered 'q' (or
// the prompt input closed) before every required image was supplied. By
// construction it happens BEFORE the image loop starts, so this run has
// written nothing to --target. The cli maps it to exit 3 like the non-TTY
// listing: in both cases the run could not obtain the media it needs.
type AbortedError struct {
	SetID  string
	Image  int64
	Reason string
}

func (e *AbortedError) Error() string {
	return fmt.Sprintf("restore of set %q aborted at the image-supply prompt (image %d: %s); "+
		"this run restored nothing and wrote nothing to the target", e.SetID, e.Image, e.Reason)
}
