package plan

// T2.1: the mandatory-only chunker (DESIGN_SPEC 4.2 chunk size and
// boundary stability; 4.3 item construction, "Chunk hash timing";
// PLAN_SPEC FR-041 and D-01).
//
// This file is the metadata-only heart of chunk planning: it
// decomposes an oversized file into placement items from the file
// SIZE alone. It never opens, reads, stats, or otherwise touches the
// source filesystem. Chunk hashes are computed lazily at FIRST WRITE
// in the copy/build paths (DESIGN_SPEC 4.3 "Chunk hash timing"), so
// planning a multi-terabyte set costs arithmetic, not I/O. Tests pin
// this two ways: an AST import assertion over this very file (no
// filesystem package is imported) and a flow over real sparse files
// made unreadable (mode 0000), where any open attempt would fail the
// test with EACCES.
//
// Layering (DESIGN_SPEC 1.2): like internal/ingest, this package does
// NOT import internal/catalog. It declares its own shapes (ChunkSpec,
// ChunkPlan, Decision); the cli wiring layer maps them onto the
// structurally identical catalog shapes (catalog.ChunkSpec in
// internal/catalog/planstore.go). All SQL stays in the catalog layer.
// The one internal dependency is internal/units - the established
// leaf for size math - which owns the canonical capacity estimator
// (units.RequiredImageBytes and its FixedImageOverhead /
// ImageEntryOverhead terms) that the whole-file-vs-chunk threshold and
// the effective chunk size below are defined against (the 2026-10-20
// capacity amendment, DESIGN_SPEC 4.3).
//
// The three decisions encoded here, in one place:
//
//  1. Mandatory-only chunking (FR-041, NOTES.md NUANCE 1, stakeholder
//     2026-10-18; honest-fit threshold as amended 2026-10-20): a file
//     is chunked ONLY when it does not fit ALONE on an empty
//     destination once the reserved image overhead is counted
//     (ShouldChunk: size + FixedImageOverhead + ImageEntryOverhead >
//     usable_size). The top band - files between
//     usable - FixedImageOverhead - 512 and usable, which the old raw
//     usable_size threshold admitted - is now chunked because such a
//     file genuinely does not fit: placing it whole would leave the
//     image over the builder's 6.3 step-5 budget. A file at exactly
//     usable - Fixed - 512 (the exact fit) is a whole-file item. The
//     chunker never chooses destinations (that is the T2.2 balancer);
//     it only receives the threshold and decomposes oversized files.
//
//  2. The decomposition formula (DESIGN_SPEC 4.3): chunk_count =
//     ceil(size / chunk_size) is the TOTAL number of chunks; the first
//     chunk_count-1 chunks have length exactly chunk_size and the
//     final chunk has length size - (chunk_count-1)*chunk_size, which
//     is always > 0 and <= chunk_size (exactly chunk_size when size is
//     an exact multiple). Worked example: a 50 GiB file with 20 GiB
//     chunks yields ceil(50/20) = 3 chunks of 20 + 20 + 10 GiB.
//
//  3. Boundary stability (DESIGN_SPEC 4.2, NOTES.md NUANCE 2): the
//     chunk size is FIXED (min(20 GiB, usable - reserved overhead)),
//     so a re-plan with the same effective size reproduces identical
//     boundaries and REUSES stored chunk rows and their hashes with
//     zero source reads; only a profile change that alters the
//     effective size re-chunks. DecideChunking encodes that choice per
//     file.

import (
	"errors"
	"fmt"
	"math"

	"isopress/internal/units"
)

// DefaultChunkSize is the fixed default chunk size: 20 GiB, exactly
// 21,474,836,480 bytes (DESIGN_SPEC 4.2). A fixed size - rather than
// an adaptive one - keeps chunk boundaries identical across runs, so
// clear-assignments plus re-plan reuses stored chunk hashes instead of
// re-reading terabytes (NOTES.md NUANCE 2). It also keeps every chunk
// far below xorriso's per-file ISO 9660 level-3 extent ceiling
// (~400 GiB, DESIGN_SPEC 4.2).
const DefaultChunkSize int64 = 20 << 30 // 21_474_836_480 bytes

// ErrInvalidDecompose classifies invalid PlanChunks inputs (negative
// size, non-positive chunk size). It is a caller-bug sentinel, not a
// user-error class: every value reaching this package comes from
// catalog size columns and units profile resolution, which validate at
// their own boundary.
var ErrInvalidDecompose = errors.New("plan: invalid chunk decomposition input")

// EffectiveChunkSize returns the effective chunk size for a plan
// (DESIGN_SPEC 4.2, as tied to the 4.3 capacity model by the
// 2026-10-20 amendment): min(DefaultChunkSize, usableSize - the
// reserved image overhead). The second term is the whole one-image
// payload budget under the canonical estimator: a chunk is a placement
// item that charges size + ImageEntryOverhead against the destination's
// remaining = usable - FixedImageOverhead, so a chunk of length
// usable - Fixed - 512 exactly fills an empty image and every chunk is
// placeable by construction.
//
// Arithmetic, pinned by TestEffectiveChunkSizeArithmetic:
// DefaultChunkSize is 20 GiB = 21,474,836,480 bytes; the default
// bd-r-sl profile's usable_size is 23 GB = 23,000,000,000 bytes
// (decimal, per D-06 / units.Profile.UsableBytes); the reserved budget
// 23,000,000,000 - 2,098,688 = 22,997,901,312 still exceeds 20 GiB,
// so min(21_474_836_480, 22_997_901_312) = 21_474_836_480: on
// single-layer Blu-ray (and every profile at or above ~20 GiB + 2.1
// MB usable) the fixed 20 GiB default applies. Only destinations
// smaller than that shave the 2,098,688-byte reservation off their
// effective chunk size - the pre-amendment "usable bytes exactly"
// behavior for such targets was UNBUILDABLE by construction (a chunk
// of the full usable_size cannot fit an image that also reserves the
// overhead), which is exactly what the amendment removed.
//
// usableSize must exceed units.FixedImageOverhead+units.ImageEntryOverhead
// (the planner's validateProfile enforces this for real plans); a
// degenerate value here is a caller bug and falls back to the fixed
// default rather than producing a zero chunk size that would divide by
// zero downstream.
func EffectiveChunkSize(usableSize int64) int64 {
	budget := usableSize - units.FixedImageOverhead - units.ImageEntryOverhead
	if budget <= 0 {
		return DefaultChunkSize
	}
	if budget < DefaultChunkSize {
		return budget
	}
	return DefaultChunkSize
}

// ShouldChunk is the mandatory-only rule of FR-041 in one predicate,
// with the honest fit test of the 2026-10-20 capacity amendment: a
// file is chunked ONLY when it does not fit alone on an empty
// destination once the reserved image overhead is counted, i.e. when
//
//	size + FixedImageOverhead + ImageEntryOverhead > usable_size.
//
// The threshold is exactly the builder's 6.3 step-5 requirement for a
// one-file image, so "whole file" and "this image is buildable"
// coincide. The TOP BAND - files in
// (usable - Fixed - 512, usable], which the old `size > usable_size`
// predicate kept whole - is now chunked because such a file genuinely
// does not fit: placing it whole would push RequiredImageBytes over
// usable_size and the build would be refused. A file at exactly
// usable - Fixed - 512 (the exact fit) is NEVER chunked; it stays a
// whole-file item even when splitting it would pack better (stakeholder
// decision, NOTES.md NUANCE 1, unchanged in spirit).
//
// A zero-size file never chunks (ShouldChunk(0, x) is false for every
// usable x - the empty file is a free 512-byte entry), and the sum is
// overflow-guarded so a corrupt-catalog size near MaxInt64 chunks
// rather than wrapping negative.
func ShouldChunk(size, usableSize int64) bool {
	if size <= 0 {
		return false
	}
	required := units.FixedImageOverhead + units.ImageEntryOverhead
	if size > math.MaxInt64-required {
		return true
	}
	return size+required > usableSize
}

// ChunkSpec is ONE chunk of a decomposition: its 0-based index, its
// byte offset in the parent file, and its byte length. The struct is
// shape-compatible with catalog.ChunkSpec (DESIGN_SPEC 1.2 interface
// mapping at the cli layer).
type ChunkSpec struct {
	Index  int   // 0-based, ascending file order (files.chunk_index)
	Offset int64 // byte offset in the parent file
	Length int64 // byte length; always > 0 and <= the chunk size
}

// ChunkPlan is the metadata-only decomposition of one file. It is
// computed from (fileID, size, effectiveChunkSize) alone and carries
// everything the planner needs to create or reuse chunk rows: the
// per-chunk offsets and lengths. It deliberately carries NO hash: chunk
// hashes are computed lazily at first write (DESIGN_SPEC 4.3), never
// here.
type ChunkPlan struct {
	FileID    int64       // the files row this decomposition belongs to
	ChunkSize int64       // the fixed/effective chunk size used
	Chunks    []ChunkSpec // len == Count(); see Count()
}

// Count is the TOTAL chunk count, ceil(size / chunkSize)
// (DESIGN_SPEC 4.3: "chunk_count = ceil(size / chunk_size) is the
// TOTAL number of chunks"). Zero for a zero-size input (the planner
// never chunks an empty file - ShouldChunk(0, x) is false - but the
// decomposition stays well-defined so the reassembly property holds
// trivially).
func (p ChunkPlan) Count() int { return len(p.Chunks) }

// TotalLength is the sum of all chunk lengths; it always equals the
// parent file size (FR-041's no-truncate guarantee, pinned by the
// property test).
func (p ChunkPlan) TotalLength() int64 {
	var total int64
	for _, c := range p.Chunks {
		total += c.Length
	}
	return total
}

// PlanChunks decomposes a file of size bytes into chunks of
// effectiveChunkSize per the DESIGN_SPEC 4.3 formula:
//
//	chunk_count  = ceil(size / chunk_size)              (TOTAL)
//	chunk[i].len = chunk_size for i < chunk_count - 1    (full chunks)
//	chunk[last]  = size - (chunk_count-1)*chunk_size     (> 0, <= chunk_size)
//
// Worked example (asserted exactly by TestPlanChunksWorkedExample): a
// 50 GiB file with 20 GiB chunks yields ceil(50/20) = 3 chunks of
// 20 + 20 + 10 GiB at offsets 0, 20 GiB, 40 GiB.
//
// The general formula also covers small destinations where
// effectiveChunkSize < size (for example a 1 GiB disk target with
// usable 1,073,741,824 bytes: a 3 GiB file becomes 3 chunks of 1 GiB)
// and the sub-chunk-size case (size < chunkSize yields exactly ONE
// chunk of length size - the planner never asks for that decomposition
// because ShouldChunk is false there, but the math is total so callers
// and tests can rely on it).
//
// This function is pure arithmetic over the catalog's size column. It
// never opens, reads, or stats the source file; see the package file
// doc for how that is asserted.
func PlanChunks(fileID, size, effectiveChunkSize int64) (ChunkPlan, error) {
	if size < 0 {
		return ChunkPlan{}, fmt.Errorf("plan: negative size %d for file %d: %w",
			size, fileID, ErrInvalidDecompose)
	}
	if effectiveChunkSize <= 0 {
		return ChunkPlan{}, fmt.Errorf("plan: non-positive chunk size %d for file %d: %w",
			effectiveChunkSize, fileID, ErrInvalidDecompose)
	}
	if size == 0 {
		return ChunkPlan{FileID: fileID, ChunkSize: effectiveChunkSize}, nil
	}

	// ceil(size / chunkSize) computed as quotient + remainder test so
	// the addition cannot overflow int64 for catalog-supplied sizes
	// near MaxInt64 (the plain (size + chunkSize - 1) / chunkSize form
	// would wrap negative and silently yield a zero-chunk plan).
	count := int(size / effectiveChunkSize)
	if size%effectiveChunkSize > 0 {
		count++
	}
	chunks := make([]ChunkSpec, count)
	var offset int64
	for i := 0; i < count; i++ {
		length := effectiveChunkSize
		if i == count-1 {
			// Final chunk: the remainder. Always > 0 (size is a
			// positive multiple-shift away) and <= chunk_size;
			// exactly chunk_size on an exact multiple.
			length = size - offset
		}
		chunks[i] = ChunkSpec{Index: i, Offset: offset, Length: length}
		offset += length
	}
	return ChunkPlan{FileID: fileID, ChunkSize: effectiveChunkSize, Chunks: chunks}, nil
}

// Action classifies the per-file chunk-row work the planner must do.
type Action int

const (
	// ActionWholeFile: the file fits alone on an empty destination
	// (NOT ShouldChunk), so it is a whole-file item and is NEVER
	// chunked (FR-041 mandatory-only). No chunk rows are created,
	// read, or touched.
	ActionWholeFile Action = iota
	// ActionCreateFresh: oversized and never chunked before
	// (files.chunk_size IS NULL): create the chunk rows with
	// hash NULL - they take their hashes at first write.
	ActionCreateFresh
	// ActionReuse: oversized and files.chunk_size equals the current
	// effective size: the stored chunk rows (indices, offsets,
	// lengths) are identical by construction and their stored hashes
	// are preserved. Zero source reads. This is the boundary-
	// stability guarantee of DESIGN_SPEC 4.2 and the reason the
	// default chunk size is fixed.
	ActionReuse
	// ActionRechunk: oversized and the stored chunk_size DIFFERS from
	// the current effective size (the user changed destination
	// profiles): the boundaries shift, so the stored decomposition is
	// replaced. See DecideChunking for the written-vs-unwritten row
	// policy.
	ActionRechunk
)

// String renders the action for logs and test failures.
func (a Action) String() string {
	switch a {
	case ActionWholeFile:
		return "whole-file"
	case ActionCreateFresh:
		return "create-fresh"
	case ActionReuse:
		return "reuse"
	case ActionRechunk:
		return "rechunk"
	default:
		return fmt.Sprintf("Action(%d)", int(a))
	}
}

// StoredChunks summarizes a file's existing chunk rows for the
// boundary-stability decision. The planner fills it from the catalog
// (files.chunk_size plus a count of chunks rows by hash NULL-ness);
// the chunker itself performs no SQL.
type StoredChunks struct {
	// ChunkSize is files.chunk_size: the effective chunk size this
	// file was decomposed with previously. 0 means "never chunked"
	// (the column is NULL for whole-file items).
	ChunkSize int64
	// WrittenChunks counts stored rows with hash NOT NULL: chunks
	// whose first write already stored a hash (DESIGN_SPEC 4.3 lazy
	// hashing).
	WrittenChunks int
	// UnwrittenChunks counts stored rows with hash NULL: chunks not
	// yet written; re-chunking them is free (no hash exists to lose).
	UnwrittenChunks int
}

// Decision is the boundary-stability outcome for one file
// (DESIGN_SPEC 4.2).
type Decision struct {
	// Action is the chunk-row work to perform.
	Action Action
	// Effective is the chunk size the decomposition uses:
	// EffectiveChunkSize(usableSize).
	Effective int64
	// RehashedAtNextWrite is the number of ALREADY-WRITTEN chunks a
	// re-chunk invalidates: their stored hashes cover the OLD byte
	// ranges and are discarded with the old rows, so those ranges are
	// re-read and re-hashed at their next write (DESIGN_SPEC 4.2:
	// "re-chunked and re-hashed"). The plan report states exactly this
	// ("re-hashed at next write"); T2.3 formats the phrase. Zero for
	// every other action.
	RehashedAtNextWrite int
}

// DecideChunking is the boundary-stability decision for one file
// (DESIGN_SPEC 4.2): given the file size, the destination threshold
// (usable_size of the plan), and the stored chunk state, it returns
// what the planner must do with the file's chunk rows.
//
// The rules, in order:
//
//   - the file fits alone on an empty destination (NOT ShouldChunk,
//     i.e. size + FixedImageOverhead + ImageEntryOverhead <=
//     usable_size) -> ActionWholeFile. Mandatory-only (FR-041): stored
//     state is irrelevant, no chunk row is ever touched. (A file row
//     that was previously chunked always failed the fit test under
//     some profile; if a profile change makes it FIT, the caller
//     simply builds a whole-file item and the stale chunk rows are
//     superseded by the new plan state - the planner never reads them
//     again.)
//
//   - does not fit, stored.ChunkSize == 0 -> ActionCreateFresh.
//     First time this file is chunked; rows are created with hash NULL
//     and take their hashes at first write.
//
//   - does not fit, stored.ChunkSize == effective -> ActionReuse.
//     The SAME effective size reproduces identical boundaries (the
//     size is fixed, not adaptive - NOTES.md NUANCE 2), so the stored
//     rows and their hashes are reused without any source read.
//     Stakeholder-visible contract: re-plan with the same profile =
//     zero re-reads, stored hashes preserved bit-for-bit.
//
//   - does not fit, stored.ChunkSize != effective -> ActionRechunk.
//     The profile changed, so the boundaries shift. Row policy
//     (documented here and enforced by
//     catalog.PlanTx.ReplaceAllChunks): the ENTIRE stored
//     decomposition is replaced, because stored rows describe the OLD
//     boundaries and cannot be mixed with a new decomposition without
//     breaking contiguity. UNWRITTEN rows (hash NULL) are replaced for
//     free - no hash exists to lose. WRITTEN rows (hash NOT NULL)
//     lose their stored hash and copied mark with their old row; the
//     source bytes of those ranges are re-read and re-hashed at their
//     next write. Decision.RehashedAtNextWrite carries that count so
//     the plan report can state it (DESIGN_SPEC 4.2: "The plan report
//     states explicitly when a profile change forces this
//     re-hashing"). The catalog integrity guard refuses to re-chunk a
//     file whose chunk rows still carry an assignment (FR-011 locking:
//     clear-assignments first).
//
// Known catalog-integrity edge: a file whose files.chunk_size matches
// the effective size but whose chunk rows are missing or inconsistent
// still decides ActionReuse here (this function sees only the stored
// SIZE); the mismatch is detected by
// catalog.PlanTx.ReuseExistingChunkRows, which verifies count,
// offsets, and lengths against the decomposition and returns
// ErrChunkMismatch - the caller then falls back to the re-chunk path.
func DecideChunking(size, usableSize int64, stored StoredChunks) Decision {
	effective := EffectiveChunkSize(usableSize)
	if !ShouldChunk(size, usableSize) {
		return Decision{Action: ActionWholeFile, Effective: effective}
	}
	if stored.ChunkSize == 0 {
		return Decision{Action: ActionCreateFresh, Effective: effective}
	}
	if stored.ChunkSize == effective {
		return Decision{Action: ActionReuse, Effective: effective}
	}
	return Decision{
		Action:              ActionRechunk,
		Effective:           effective,
		RehashedAtNextWrite: stored.WrittenChunks,
	}
}
