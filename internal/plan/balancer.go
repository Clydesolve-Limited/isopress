package plan

// T2.2: the worst-fit-decreasing (WFD) balancer of DESIGN_SPEC 4.3
// (balancing algorithm), 4.4 (fill strategy D-05), 4.5
// (single-transaction assignment), 2.4 (hard-link families, FR-011
// locking); PLAN_SPEC FR-007, FR-008, FR-011, FR-029, FR-041.
//
// The balancer turns the catalog's ACTIVE, UNASSIGNED records into a
// deterministic placement - one destination (images row) per unit of
// media - and persists it in ONE transaction. Like internal/ingest,
// this package does NOT import internal/catalog: it declares its own
// shapes and Store interfaces (DESIGN_SPEC 1.2, "services depend on
// interfaces for catalog access"), the cli wiring layer maps them onto
// the structurally identical catalog shapes, and all SQL stays in the
// catalog layer. The one internal dependency is internal/units - a
// leaf package (no SQL, no I/O) that owns the D-06 capacity figures,
// so the profile numbers have a single source of truth.
//
// Planning is METADATA-ONLY (DESIGN_SPEC 4.3 "Chunk hash timing",
// 12.5): the balancer never opens, reads, stats, or otherwise touches
// the source filesystem. Dirs come from the directories TABLE, never
// from an in-memory walk (the 2026-10-19 durability ruling), and chunk
// hashes are computed lazily at first write, never here. Tests pin
// this two ways: an AST import-assertion over this file (only
// filesystem-blind packages allowed) and a flow over real source files
// made unreadable (mode 0000), where any open attempt would fail with
// EACCES.
//
// # Item construction (DESIGN_SPEC 4.3)
//
// From the active unassigned rows:
//
//   - regular files that fit ALONE on an empty destination
//     (size + FIXED_OVERHEAD + 512 <= usable_size - the honest fit
//     test, ShouldChunk) are WHOLE-FILE items;
//   - regular files that do NOT fit alone are decomposed by the T2.1
//     chunker (ShouldChunk / PlanChunks / DecideChunking); each CHUNK
//     is an independent placement item (the shorter final chunk serves
//     as packing filler). The top band - files between
//     usable - FIXED_OVERHEAD - 512 and usable - is chunked because
//     such a file genuinely does not fit: the image must also reserve
//     the builder's 6.3 overhead or the build is refused (4.3
//     capacity accounting). The stored decomposition is REUSED when
//     files.chunk_size equals the current effective size and the
//     stored rows verify against it (boundary stability, 4.2 - zero
//     source reads, stored hashes preserved); otherwise the rows are
//     created fresh or replaced (re-chunk), inside the assignment
//     transaction;
//   - symlinks are items with size = len(target) (they never chunk,
//     they must merely land somewhere so the tree is faithful); a
//     target longer than one image's whole payload budget aborts the
//     plan with ErrItemTooLarge (corrupt-catalog class);
//   - ACTIVE empty directories (from the directories table) are NOT
//     placement items: they carry zero payload bytes and never enter
//     the WFD stream - they are assigned by the separate
//     deterministic pass of "The empty-dir special rule" below;
//   - hard-link secondaries are EXCLUDED entirely (2.4: the primary
//     represents the family; space accounting counts the family once).
//
// # Capacity accounting: the planner reserves the builder's overhead
// (DESIGN_SPEC 4.3 as amended 2026-10-20)
//
// The planner and the builder share ONE canonical estimator,
// units.RequiredImageBytes(payload, entries) = payload +
// FixedImageOverhead + 512*entries, where FixedImageOverhead = 2 MiB +
// 512*2 covers the ISO system area plus the two entries every image
// carries (MANIFEST.json, catalog-export.sqlite). The balancer
// RESERVES that overhead at fill time so every planned image satisfies
// RequiredImageBytes <= usable_size by construction (the 12.5
// buildability invariant):
//
//   - an image OPENS with remaining = usable_size - FixedImageOverhead;
//   - every placement decrements remaining by (size + 512) - the
//     per-entry charge of the canonical estimator; the WFD heap keys
//     on this reduced remaining, and a chunk item charges size + 512
//     like any other item;
//   - a whole file is placed only when size + 512 fits the budget,
//     which for a fresh image is exactly the ShouldChunk fit test.
//
// This is what ended the 2026-10-20 stakeholder failure: the OLD
// balancer packed to the raw usable_size while the builder's 6.3
// step-5 pre-check reserved overhead from the same budget, so
// well-packed plans were routinely refused at build time. Costs remain
// deterministic (FIXED_OVERHEAD, the 512 entry charge, and every item
// size are deterministic), so the 12.5 determinism property is
// unaffected. A profile whose usable_size cannot even hold the fixed
// overhead plus one entry is refused up front (validateProfile).
//
// # Hard-link families: the link_group finding (T2.2 hand-off note)
//
// DESIGN_SPEC 2.4 says ingest keys families on (dev, inode) with
// nlink > 1 and gives every member the shared link_group. The scan
// pipeline as shipped populates dev, inode, and nlink faithfully
// (internal/fsmeta Capture; catalog.FileRow) but NEVER populates
// link_group: the catalog insert hard-codes it NULL ("link_group NULL
// until hard-link family grouping lands with the copy/plan work",
// internal/catalog/scanstore.go insertValues). Rather than invent
// data, this balancer derives families from what the data DOES
// support: among catalog-ACTIVE rows with nlink > 1 and a known inode
// (inode != 0 - a zero inode marks an uncaptured sentinel, not a real
// family), rows sharing (dev, inode) form one family and the LOWEST
// file_id is its primary. An unassigned non-primary row is excluded
// from items entirely; if the primary is already assigned (locked on
// media from an earlier plan), the family's bytes are already counted
// there, so its unassigned secondaries are excluded too - the family
// is always accounted exactly once (FR-039). When link_group
// population lands with the copy/plan work, this grouping can switch
// to the column without changing any interface here.
//
// # Determinism (DESIGN_SPEC 4.3)
//
// Items are sorted by DESCENDING size; ties break by ASCENDING
// relpath; chunks of one file (same relpath, equal lengths) break by
// ASCENDING chunk index. That key is TOTAL over the active set, but
// the uniqueness it rests on - one ACTIVE row per relpath - is the
// reconciler's versioning invariant (2.4: a modification supersedes
// the old row in the same transaction), NOT a database constraint:
// files carries only idx_files_path, no UNIQUE(set_id, relpath).
// Identical inputs still yield a byte-identical plan (the 12.5
// determinism property) even if a hand-edited catalog broke that
// invariant, because items enter the sort in file_id-ASCENDING order
// (Store.ActiveFiles' ORDER BY file_id) and sort.Slice is
// deterministic for a fixed input sequence - a relpath tie resolves
// the same way every run. Placement opens destinations
// with ids from the lowest-unused allocator (4.6): ascending, skipping
// every id in use by a locked images row - strictly increasing within
// a run, and byte-identical across runs for the same inputs - and the
// WFD heap breaks equal-remaining ties by SMALLEST image_id,
// keeping the tie path deterministic too.
//
// # Incremental / locked semantics (DESIGN_SPEC 4.3, FR-011, FR-029)
//
// Destinations that already exist (have images rows) are LOCKED:
// they are never in the open set, receive no new payload, and their
// recorded assignments are never touched. New unassigned items only
// ever open NEW destinations. A run with zero unassigned items is a
// NO-OP: no transaction, no empty destinations created. The one
// deliberate exception is the empty-dir rule below, which writes a
// first-time directories.image_id (a NULL -> value assignment, exactly
// the write the FR-011 lock allows) and adds NO payload bytes - only
// the 512-byte entry charge against the destination's RESERVED budget,
// which the rule itself checks before attaching (see the rule below),
// so it can never overflow a locked destination's capacity.
//
// # The empty-dir special rule (DESIGN_SPEC 4.3, 2.3 directories DDL;
// capacity twist of the 2026-10-20 amendment)
//
// Active empty directories are assigned deterministically to the
// LOWEST-NUMBERED destination of the final plan - a separate
// deterministic pass after file placement (ordered by dir_id). If the
// plan has destinations (locked or new) that number is the minimum
// image_id among them; a dirs-ONLY plan takes that SAME route - its
// dirs attach to the lowest-numbered LOCKED destination that has entry
// slack, and a NEW destination opens (the LOWEST id not in use by an
// images row - the 4.6 lowest-unused rule - or destination 1 when the
// plan has none) only when NO destination can absorb the
// dir charges. The one capacity concession (the
// amendment's "by construction" invariant extends to the dir ENTRIES):
// each dir charges its 512-byte entry cost against the target's
// reserved budget (the builder counts empty dirs as ISO entries in its
// 6.3 step-5 pre-check), so when the lowest-numbered destination
// cannot absorb the dir charges, the pass falls forward to the
// LOWEST-NUMBERED destination that can - opening a new destination
// (the dirs-only precedent) when none can. In the overwhelmingly
// common case the lowest destination has slack and the rule is
// literally the design text; the fallback exists so a fully-packed
// lowest destination never receives dir entries that would make the
// image unbuildable. Dirs still consume zero BYTES, so this never
// conflicts with the FR-011 capacity lock on payload. Documented
// judgment call: in an INCREMENTAL run the lowest-numbered destination
// is usually a locked one, and new dirs attach to it when it has
// entry slack - this keeps the tree-fidelity goal (every empty dir
// lands on a durable image id) intact. The writers' concern of
// rewriting an already-built lowest-numbered image is out of T2.2
// scope and is recorded here as the known incremental wrinkle.
//
// # Fill strategy (DESIGN_SPEC 4.4, D-05 accept-waste)
//
// The planner honors locking as specified: gaps on locked destinations
// are permanent until clear-assignments + re-plan. What it does is
// QUANTIFY them: Result.WastedResidualBytes / WastedResidualImages is
// the "residual space on locked destinations" figure T2.3 renders
// (4.4), so the cost of accept-waste is visible rather than silent.
//
// # Single-transaction assignment (DESIGN_SPEC 4.5)
//
// Plan.Write runs the whole write side in ONE BeginPlanTx
// transaction: insert images rows (state 'planned'), create/replace
// chunk rows, stamp files.chunk_size, set files.image_id (whole
// items), set chunks.image_id (chunk items, mirrored files.copied =
// 0), and PlanTx.AssignDir for dirs. Any failure rolls the whole
// transaction back: a failed plan leaves the catalog bit-identical
// (FR-010). Balance (the compute phase) is side-effect free, which is
// exactly what lets the read-only `report` command run the identical
// pipeline in no transaction (4.3 media-count clause; T2.3 wiring).

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"sort"

	"isopress/internal/units"
)

// ErrInvalidProfile classifies a Balance call whose profile cannot
// drive a plan: non-positive usable or nominal capacity, or an empty
// profile name (the images.media_profile column is NOT NULL, and D-06
// resolves margins exactly once, upstream, in units.ResolveProfile).
var ErrInvalidProfile = errors.New("plan: invalid media profile for balancing")

// ErrItemTooLarge classifies an unassignable item: a payload item
// (file, chunk, or symlink) whose size exceeds the plan's usable
// destination capacity. With mandatory-only chunking (FR-041) every
// regular item is <= usable by construction, so this fires only on
// corrupt catalog data or an absurd symlink target - it aborts the
// plan before any transaction is opened rather than spinning out
// endless destinations.
var ErrItemTooLarge = errors.New("plan: item exceeds usable destination capacity")

// ErrPartialChunkReplan classifies a re-plan that cannot complete a
// PARTIALLY-assigned chunk set without rewriting LOCKED rows. The
// state is the transient one DESIGN_SPEC 4.6 creates when a scoped
// unlock (clear-assignments --image/--last) releases only part of a
// chunked file's chunks; it is user-correctable, so the cli wraps it
// in report.UsageError -> exit 2 (10.1 "unsatisfiable combinations"),
// NOT the bare fatal class ErrItemTooLarge takes (a corrupt-catalog
// condition) - a deliberate, documented deviation (4.3
// PARTIALLY-ASSIGNED CHUNK SETS; the plan/report help exit-code
// clauses). Every message names the file, the locked image ids, and
// the remedy(s) of the refusing branch; the refusal is raised at
// COMPUTE time, before any transaction opens, so a refused plan
// leaves the catalog bit-identical.
var ErrPartialChunkReplan = errors.New("plan: partially-assigned chunk set")

// --- input shapes (structurally identical to the catalog shapes; the
// cli wiring layer maps between the two, DESIGN_SPEC 1.2) -----------

// ActiveFile is one catalog-ACTIVE files row the balancer builds items
// from (status='active' AND superseded_by IS NULL). ImageID is the
// current assignment: 0 = unassigned (SQL NULL), non-zero = LOCKED.
// ChunkSize mirrors files.chunk_size: 0 means never chunked (NULL).
type ActiveFile struct {
	FileID    int64
	RelPath   string
	EntryKind string // 'regular' | 'symlink'
	Size      int64  // bytes; for symlinks len(target) (2.3)
	ChunkSize int64  // stored effective chunk size; 0 = never chunked
	Dev       int64  // hard-link family key part (2.4)
	Inode     int64  // hard-link family key part; 0 = uncaptured sentinel
	Nlink     int64  // nlink > 1 marks a hard-link family (2.4)
	ImageID   int64  // assignment; 0 = unassigned, else the locked destination
}

// StoredChunk is one stored chunks row of a previously-chunked file,
// joined with the parent's stored files.chunk_size (the
// boundary-stability input of 4.2). ImageID is the chunk's own
// assignment: a chunked file never carries files.image_id (4.5
// assigns chunked files via chunks.image_id only), so a chunk set
// whose rows ALL carry an image_id is a LOCKED, already-placed file.
// HashPresent reports whether the row carries a stored hash (hash NOT
// NULL): it feeds Decision.RehashedAtNextWrite so the read-only
// report can state the re-hash cost of a profile change (4.2)
// without a write.
type StoredChunk struct {
	FileID      int64
	ChunkSize   int64 // parent files.chunk_size (stored effective size)
	Index       int
	Offset      int64
	Length      int64
	ImageID     int64 // assignment; 0 = unassigned (NULL)
	HashPresent bool
}

// UnassignedDir is one ACTIVE, UNASSIGNED directories row (the size-0
// empty-dir items; DESIGN_SPEC 2.3, 4.3).
type UnassignedDir struct {
	DirID   int64
	RelPath string
}

// DestinationState is one EXISTING images row with its current fill -
// the LOCKED destinations a new plan must leave bit-identical (FR-011)
// and the baseline the wasted-residual figure is computed over
// (D-05). UsedBytes counts assigned whole-file and symlink sizes plus
// assigned chunk lengths; ItemCount counts those payload assignments;
// DirCount counts assigned directories (zero capacity by design).
type DestinationState struct {
	ImageID      int64
	NominalSize  int64
	UsableSize   int64
	MediaProfile string
	UsedBytes    int64
	ItemCount    int
	DirCount     int
}

// Store is the read side of the planning pipeline. The wiring supplies
// a set-bound catalog adapter over *catalog.DB (usable in NO
// transaction, so `report` runs the identical pipeline read-only,
// 4.3); tests supply fakes. Method result order is deterministic
// (file_id / dir_id / image_id ascending) - the same order the
// catalog SQL promises - because determinism is a plan property, not
// an accident of map iteration.
type Store interface {
	// ActiveFiles returns every catalog-ACTIVE files row of the set,
	// ordered by file_id. This is the balancer's ONLY file source:
	// planning runs as a separate process days after the scan and
	// reads only the catalog.
	ActiveFiles(ctx context.Context) ([]ActiveFile, error)
	// ChunkStates returns the stored chunk rows (with their own
	// assignments) of every ACTIVE previously-chunked file
	// (files.chunk_size NOT NULL), ordered by (file_id,
	// chunk_index). A chunk set whose rows ALL carry an image_id is
	// a LOCKED, already-placed file (chunked files never carry
	// files.image_id - 4.5 assigns them via chunks.image_id only).
	ChunkStates(ctx context.Context) ([]StoredChunk, error)
	// UnassignedDirs returns every ACTIVE, UNASSIGNED directories
	// row, ordered by dir_id (ListActiveUnassignedDirs; the empty-dir
	// durability ruling of 2026-10-19).
	UnassignedDirs(ctx context.Context) ([]UnassignedDir, error)
	// Destinations returns every existing images row with its fill,
	// ordered by image_id - the LOCKED set.
	Destinations(ctx context.Context) ([]DestinationState, error)
}

// --- output shapes (T2.3 renders these numbers) ---------------------

// ImageSpec is one images row to insert: 1-based sequential image_id
// (6.4), the profile's nominal and usable figures (D-06), the profile
// name, state 'planned' (2.3).
type ImageSpec struct {
	ImageID      int64
	NominalSize  int64
	UsableSize   int64
	MediaProfile string
}

// DestFill is one destination's fill line of the plan result: bytes
// and payload-item counts plus the empty dirs that landed there.
// Locked marks a pre-existing destination (its fill is historical,
// its capacity was never in the open set).
type DestFill struct {
	ImageID      int64
	NominalSize  int64
	UsableSize   int64
	MediaProfile string
	UsedBytes    int64
	ItemCount    int
	DirCount     int
	Locked       bool
}

// Result is the plan outcome - the numbers the plan and report
// commands print (FR-008/FR-009 media count, D-05 waste visibility,
// 4.2 re-hash statement). Destinations is ordered by ascending
// image_id and includes BOTH locked and new destinations, so
// len(Destinations) is the media count of the whole plan.
type Result struct {
	Destinations    []DestFill
	NewDestinations int // destinations created by THIS run

	FilesAssigned       int // whole-file items assigned
	ChunksAssigned      int // chunk items assigned
	SymlinksAssigned    int // symlink items assigned
	DirsAssigned        int // empty dirs assigned (lowest-numbered rule)
	SecondariesExcluded int // hard-link secondaries excluded (2.4)

	// WastedResidualBytes / WastedResidualImages quantify the D-05
	// accept-waste cost: total bytes that can NEVER be used on LOCKED
	// destinations, and how many locked destinations hold such a
	// residual (the 4.4 report line: "residual space on locked
	// destinations: X GiB across N images").
	WastedResidualBytes  int64
	WastedResidualImages int

	// RechunkedFiles / RehashedAtNextWrite state the 4.2 profile-
	// change cost: how many files were re-chunked under the current
	// effective size and how many of their ALREADY-WRITTEN chunks
	// lose their stored hash (re-read and re-hashed at next write).
	RechunkedFiles      int
	RehashedAtNextWrite int
}

// --- write seam (transaction-scoped; the cli adapter maps onto
// *catalog.PlanTx, tests supply fakes) -----------------------------

// RechunkOutcome mirrors catalog.RechunkResult: what a re-chunk
// replaced, in row counts (unwritten rows replaced for free; written
// rows re-read and re-hashed at their next write, 4.2).
type RechunkOutcome struct {
	RemovedUnwritten int
	RemovedWritten   int
}

// AssignStore is the write side of one planning transaction. Every
// method MUST run inside the transaction the wiring began with
// catalog.DB.BeginPlanTx (DESIGN_SPEC 4.5: one BEGIN IMMEDIATE for
// the whole plan) and every assignment method upholds the FR-011 lock
// mechanically: only a NULL assignment is ever written, and a
// zero-row update is an error, never a silent no-op.
type AssignStore interface {
	// InsertImage stores one images row (state 'planned').
	InsertImage(ctx context.Context, img ImageSpec) error
	// EnsureChunkRows creates the file's chunk rows from the
	// decomposition, or confirms the stored ones match exactly
	// (idempotent; a disagreement is an error - the re-chunk path
	// owns replacement).
	EnsureChunkRows(ctx context.Context, fileID int64, specs []ChunkSpec) error
	// ReplaceAllChunks re-chunks the file: the ENTIRE stored
	// decomposition is replaced (4.2; written rows lose their stored
	// hash with their old-boundary row and are re-hashed at next
	// write). A row with image_id set aborts (FR-011).
	ReplaceAllChunks(ctx context.Context, fileID int64, specs []ChunkSpec) (RechunkOutcome, error)
	// DropChunkState deletes the file's chunk rows and un-stamps
	// files.chunked/chunk_count/chunk_size in the same transaction
	// (folded T2.1 review note NB2: a file that was chunked under a
	// smaller profile and is now assigned WHOLE must leave ZERO chunk
	// rows and a clean stamp behind, or restore - which reads chunk
	// rows, 8.4 - would see a catalog that lies). A row with
	// image_id set aborts (FR-011).
	DropChunkState(ctx context.Context, fileID int64) (int, error)
	// StoreChunkSize stamps files.chunked/chunk_count/chunk_size
	// (4.2 boundary-stability input for the NEXT plan).
	StoreChunkSize(ctx context.Context, fileID int64, chunkSize int64, chunkCount int) error
	// AssignFile sets files.image_id for a whole-file item and mirrors
	// files.copied = 0 (4.5).
	AssignFile(ctx context.Context, fileID, imageID int64) error
	// AssignChunk sets chunks.image_id for one chunk item.
	AssignChunk(ctx context.Context, fileID int64, chunkIndex int, imageID int64) error
	// AssignDir persists one empty-dir assignment to
	// directories.image_id (4.3 lowest-numbered rule).
	AssignDir(ctx context.Context, dirID, imageID int64) error
}

// AssignTx pairs the transaction-scoped AssignStore with Commit and
// Rollback (the ingest.Tx pattern, T1.6). Exactly one of Commit /
// Rollback runs per factory call; the balancer commits only after
// every statement of the plan succeeded (4.5 atomicity).
type AssignTx struct {
	AssignStore
	Commit   func() error
	Rollback func() error
}

// AssignTxFactory begins one planning transaction. The wiring supplies
// catalog.DB.BeginPlanTx adapted to this shape; tests supply fakes.
type AssignTxFactory func(ctx context.Context) (*AssignTx, error)

// --- the computed (uncommitted) plan --------------------------------

// itemKind classifies a placement item.
type itemKind int

const (
	itemWhole   itemKind = iota // whole-file item
	itemChunk                   // chunk of an oversized file
	itemSymlink                 // symlink (size = len(target))
)

// placeItem is ONE placement item (DESIGN_SPEC 4.3 item construction).
// chunkIndex is the 0-based chunk index for itemChunk and -1
// otherwise (it completes the total sort key: chunks of one file share
// the parent relpath).
type placeItem struct {
	kind       itemKind
	relPath    string
	fileID     int64
	chunkIndex int
	size       int64
}

// chunkFileOp is the per-chunked-file write work: the boundary-
// stability action decided at compute time, the decomposition, the
// effective chunk size it was computed with (the value stamped back
// onto files.chunk_size), and the per-chunk destination assignments
// (ordered by chunk index).
type chunkFileOp struct {
	fileID      int64
	relPath     string
	action      Action // ActionCreateFresh | ActionReuse | ActionRechunk
	effective   int64
	specs       []ChunkSpec
	assignments []chunkAssign
}

// chunkAssign is one chunk's destination.
type chunkAssign struct {
	index   int
	imageID int64
}

// wholeFileOp is one whole-file (or symlink) assignment. dropStale
// marks the NB2 transition: a file that was previously chunked (under
// a profile where it did not fit) and is now assigned WHOLE must shed
// its stale chunk rows and stamp inside the same transaction, or the
// catalog would contradict itself (restore reads chunk rows).
type wholeFileOp struct {
	fileID    int64
	relPath   string
	imageID   int64
	dropStale bool
}

// dirOp is one empty-dir assignment to the lowest-numbered destination.
type dirOp struct {
	dirID   int64
	relPath string
	imageID int64
}

// planOps is the ordered write work of one plan (each slice in
// deterministic ascending id order - the write order itself is part
// of the determinism contract).
type planOps struct {
	images     []ImageSpec
	chunkFiles []chunkFileOp
	wholeFiles []wholeFileOp
	dirs       []dirOp
}

func (o planOps) empty() bool {
	return len(o.images) == 0 && len(o.chunkFiles) == 0 &&
		len(o.wholeFiles) == 0 && len(o.dirs) == 0
}

// Plan is one computed, not-yet-persisted assignment. Result carries
// the reportable figures (identical whether or not the plan is ever
// written - `report` computes the same Plan read-only). Write
// persists it in ONE transaction (4.5).
type Plan struct {
	Result Result
	ops    planOps
}

// --- the balancing algorithm -----------------------------------------

// ctxCheckInterval is the number of per-item iterations the balancer's
// hot loops run between context probes (T6.3 debt closed: the WFD
// placement loop - and the item-construction and empty-dir loops with
// it - never checked ctx, so a cancelled run (^C, or the 12.8 perf
// smoke's deadline) could not abort a long plan).
//
// Cost argument: a probe is one ctx.Err() - a mutex read plus a
// branch, tens of ns - amortized over 4096 iterations that each cost
// heap ops, a map lookup and fill arithmetic, i.e. well under a tenth
// of a percent of loop time. Benefit: the cancellation latency is
// bounded by the WORK between two probes, which has two shapes:
//
//   - per-item loops (item construction, the WFD placement loop, the
//     empty-dir pass): a probe every ctxCheckInterval iterations, so
//     an interrupt lands within 4096 items of work - single-digit
//     milliseconds at the 1M-item scale the perf smoke measures;
//   - the three UNINTERRUPTIBLE stretches - the two file_id sorts of
//     the op slices (and the size-guard pass right behind them), the
//     descending-size sortItems, and the empty-dir sort - carry no
//     probe inside, so an interrupt fires only at the NEXT probe
//     site, i.e. after that stretch finishes: seconds-scale at 1M
//     items, the accepted worst case (one million items sorting is
//     seconds of the "plans in seconds" budget of 4.3, not an
//     unbounded stall).
//
// Either shape lands an interrupt promptly instead of after the whole
// plan. The probe sits at loop boundaries only - never inside the
// heap/gap arithmetic.
const ctxCheckInterval = 4096

// balanceCtxCheck returns nil while ctx is live and a ctx-anchored
// abort error once it is done. The %w wrap keeps errors.Is(err,
// context.Canceled / context.DeadlineExceeded) true, which is what
// the cli's shared runInterrupted predicate (progress_done.go:
// Interrupted Done flag, cleared bar) and execute's run-context
// interrupt override (root.go: cancelled run context -> report.
// ErrInterrupted -> exit 130) key on. Balance never wraps this in a
// usage or catalog class (it is returned bare from Balance and from
// runPlanReport), so it cannot be reclassified as exit 2/4 on the way
// out; report.Classify alone sees a plain fatal-class error and the
// wrapper's interrupt override promotes it to 130.
func balanceCtxCheck(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("plan: balance aborted: %w", err)
	}
	return nil
}

// Balance computes the deterministic WFD assignment for the set bound
// to store, under the given media profile. It performs NO writes:
// side effects live exclusively in Plan.Write, which is what lets the
// read-only `report` command run the identical pipeline in no
// transaction (DESIGN_SPEC 4.3).
//
// ctx is honored throughout (T6.3): every load, every per-item loop,
// and the empty-dir pass probe it (see ctxCheckInterval) and abort
// with a ctx-anchored error. Because the assignment transaction is
// opened only LATER, by Plan.Write, an abort at ANY point here leaves
// the catalog bit-identical - no images rows, no assignments, no run
// rows (FR-010, 4.5).
func Balance(ctx context.Context, store Store, profile units.Profile) (Plan, error) {
	if err := validateProfile(profile); err != nil {
		return Plan{}, err
	}
	files, err := store.ActiveFiles(ctx)
	if err != nil {
		return Plan{}, err
	}
	chunkRows, err := store.ChunkStates(ctx)
	if err != nil {
		return Plan{}, err
	}
	dirs, err := store.UnassignedDirs(ctx)
	if err != nil {
		return Plan{}, err
	}
	locked, err := store.Destinations(ctx)
	if err != nil {
		return Plan{}, err
	}

	// The load phase is done; probe the run context ONCE before the
	// compute phase starts. Balance is side-effect free and Plan.Write
	// owns the ONLY transaction, so aborting here (or anywhere below)
	// persists nothing.
	if err := balanceCtxCheck(ctx); err != nil {
		return Plan{}, err
	}

	usable := profile.UsableBytes
	sort.Slice(locked, func(i, j int) bool { return locked[i].ImageID < locked[j].ImageID })
	storedByFile := groupStoredChunks(chunkRows)
	primaries := hardLinkPrimaries(files)

	// --- item construction (4.3) ---
	var (
		items               []placeItem
		ops                 planOps
		secondariesExcluded int
		symlinksAssigned    int
		rechunkedFiles      int
		rehashedAtNextWrite int
	)
	for i, f := range files {
		// Batched context probe: iteration 0 doubles as this loop's
		// entry check, then every ctxCheckInterval files (item
		// construction is per-file work - chunking decisions included).
		if i%ctxCheckInterval == 0 {
			if err := balanceCtxCheck(ctx); err != nil {
				return Plan{}, err
			}
		}
		if f.ImageID != 0 {
			continue // LOCKED whole item: never re-placed (FR-011)
		}
		// The stored decomposition is read ONCE, ahead of the family
		// rule, so the PARTIAL check below can run BEFORE the
		// secondary exclusion: a partially-assigned set must never be
		// silently left behind by a row the family rule skips (4.3
		// PARTIALLY-ASSIGNED CHUNK SETS, 4.6).
		stored := storedByFile[f.FileID]
		if isSecondary(f, primaries) {
			secondariesExcluded++
			if isPartialChunkSet(stored) {
				return Plan{}, partialChunkRefusal(f, stored,
					"the hard-link family rule excludes this row from placement",
					remedyClearWholeSet)
			}
			continue
		}
		if allChunksAssigned(stored) {
			// LOCKED chunked file: a chunked file never carries
			// files.image_id (4.5 assigns it via chunks.image_id
			// only), so its lock lives on the chunk rows. All rows
			// assigned = fully placed by an earlier plan; the family
			// bytes are on media and must not be re-placed (FR-011).
			continue
		}
		if isPartialChunkSet(stored) {
			// PARTIALLY-ASSIGNED CHUNK SET (4.6's transient state:
			// some of this file's chunks were released by a scoped
			// unlock). REUSE is the only legal outcome: every path
			// that could rewrite the LOCKED half - the whole-file
			// transition below (DropChunkState deletes every stored
			// row), a changed effective chunk size (ReplaceAllChunks
			// replaces every row), a decomposition that no longer
			// verifies, or the family rule abandoning the row - REFUSES
			// here, at compute time, before any transaction opens.
			if f.EntryKind == "symlink" || !ShouldChunk(f.Size, usable) {
				return Plan{}, partialChunkRefusal(f, stored,
					"the file now fits one destination; completing it whole would rewrite the locked rows",
					remedyKeepProfileOrClearWholeSet)
			}
			decision := DecideChunking(f.Size, usable, summarizeStored(f.ChunkSize, stored))
			specs, err := planSpecs(f.FileID, f.Size, decision.Effective)
			if err != nil {
				return Plan{}, err
			}
			if decision.Action != ActionReuse {
				return Plan{}, partialChunkRefusal(f, stored,
					fmt.Sprintf("the effective chunk size changed: stored chunk size %d, this run's effective %d",
						f.ChunkSize, decision.Effective),
					remedyKeepProfileOrClearWholeSet)
			}
			if !decompositionMatches(stored, specs) {
				return Plan{}, partialChunkRefusal(f, stored,
					"the stored chunk rows do not verify against the current decomposition",
					remedyKeepProfileOrClearWholeSet)
			}
			// REUSE: place ONLY the unassigned rows. The assigned
			// half produces NO item, NO assignment, and NO mark
			// change: its rows, hashes, and copied/verified_at marks
			// survive bit-for-bit (4.3/4.6). decompositionMatches
			// just proved specs == stored, so either may supply the
			// item geometry; specs keeps the existing op shape.
			op := chunkFileOp{
				fileID: f.FileID, relPath: f.RelPath,
				action: ActionReuse, effective: decision.Effective, specs: specs,
			}
			for i, s := range specs {
				if stored[i].ImageID != 0 {
					continue // LOCKED half: no item, no assignment
				}
				items = append(items, placeItem{
					kind: itemChunk, relPath: f.RelPath, fileID: f.FileID,
					chunkIndex: s.Index, size: s.Length,
				})
			}
			ops.chunkFiles = append(ops.chunkFiles, op)
			continue
		}
		if f.EntryKind == "symlink" || !ShouldChunk(f.Size, usable) {
			// Whole-file item (symlinks always: they never chunk, by
			// D-03 - a symlink is a target string, not a byte stream).
			if f.EntryKind == "symlink" {
				symlinksAssigned++
			}
			items = append(items, placeItem{
				kind: kindFor(f.EntryKind), relPath: f.RelPath,
				fileID: f.FileID, chunkIndex: -1, size: f.Size,
			})
			ops.wholeFiles = append(ops.wholeFiles, wholeFileOp{
				fileID: f.FileID, relPath: f.RelPath,
				// NB2: a previously-chunked file that now fits must
				// shed its stale chunk rows + stamp in the same tx
				// (the rows are unassigned here: a set with ANY
				// assigned row was refused above - ErrPartialChunkReplan
				// - and a fully assigned set was skipped as LOCKED).
				dropStale: f.ChunkSize > 0,
			})
			continue
		}
		// Oversized regular file: mandatory-only chunking (FR-041).
		decision := DecideChunking(f.Size, usable, summarizeStored(f.ChunkSize, stored))
		specs, err := planSpecs(f.FileID, f.Size, decision.Effective)
		if err != nil {
			return Plan{}, err
		}
		op := chunkFileOp{
			fileID: f.FileID, relPath: f.RelPath,
			action: decision.Action, effective: decision.Effective, specs: specs,
		}
		for _, s := range specs {
			items = append(items, placeItem{
				kind: itemChunk, relPath: f.RelPath, fileID: f.FileID,
				chunkIndex: s.Index, size: s.Length,
			})
		}
		if decision.Action == ActionReuse && !decompositionMatches(stored, specs) {
			// Catalog-integrity edge (documented in DecideChunking):
			// files.chunk_size matches the effective size but the rows
			// do not verify. The plan falls back to the re-chunk path
			// - the same recovery ReuseExistingChunkRows performs on
			// the SQL side - instead of trusting rows that lie.
			op.action = ActionRechunk
		}
		if op.action == ActionRechunk {
			rechunkedFiles++
			rehashedAtNextWrite += countWritten(stored)
		}
		ops.chunkFiles = append(ops.chunkFiles, op)
	}
	sort.Slice(ops.wholeFiles, func(i, j int) bool {
		return ops.wholeFiles[i].fileID < ops.wholeFiles[j].fileID
	})
	sort.Slice(ops.chunkFiles, func(i, j int) bool {
		return ops.chunkFiles[i].fileID < ops.chunkFiles[j].fileID
	})

	// Defensive size guard (the 2026-10-20 form): with the honest fit
	// test every regular item fits an empty image WITH its entry charge
	// by construction; a violation here means corrupt data (or an
	// absurd symlink target) and aborts before any transaction opens,
	// rather than opening endless destinations or an image whose
	// remaining capacity would go negative.
	for _, it := range items {
		if it.size+units.ImageEntryOverhead > usable-units.FixedImageOverhead {
			return Plan{}, fmt.Errorf("%w: %q (%s) is %d bytes; one image holds at most %d payload bytes "+
				"(usable %d - fixed overhead %d - entry %d)",
				ErrItemTooLarge, it.relPath, itemKindName(it.kind), it.size,
				usable-units.FixedImageOverhead-units.ImageEntryOverhead,
				usable, units.FixedImageOverhead, units.ImageEntryOverhead)
		}
	}

	// --- sort (4.3: descending size, ascending relpath, ascending
	// chunk index - total over the active set, see the Determinism
	// note in the package doc, hence deterministic) ---
	// Probe once more before the (uninterruptible) sort: an already-
	// cancelled context must not pay for sorting a million items.
	if err := balanceCtxCheck(ctx); err != nil {
		return Plan{}, err
	}
	sortItems(items)

	// --- placement: WFD over the OPEN destinations (4.3 capacity
	// accounting as amended 2026-10-20). An image OPENS with the
	// reserved overhead subtracted (remaining = usable - FIXED_OVERHEAD)
	// and every placement decrements by (size + 512); the heap keys on
	// the reduced remaining. Locked destinations are not in the open
	// set at all: their remaining capacity is effectively zero and
	// locked, so new items automatically create new destinations
	// (FR-029). The ids come from the LOWEST-UNUSED allocator (4.6):
	// ascending, skipping every id an images row still holds - a
	// contiguous catalog {1..k} mints exactly k+1, k+2, ... (the T2.4
	// exact-fresh-plan equivalence), a released slot is re-minted
	// first, and spillover still opens above the maximum. ---
	idAlloc := newImageIDAllocator(locked)
	open := make(destHeap, 0, 8)
	fills := make(map[int64]*DestFill, 8)
	var newIDs []int64
	// The fileID -> position index over the op slices (T6.3: the
	// pre-fix recordAssignment linearly SCANNED these slices once per
	// placed item - O(n^2) over a 1M-item plan). Built ONCE, here,
	// where ops.wholeFiles / ops.chunkFiles are final: they were built
	// in the item-construction loop above, sorted by file_id, one op
	// per file, and recordAssignment only MUTATES elements in place -
	// it never appends to them - so every recorded position stays
	// valid for the whole pass. The SLICES remain the source of truth
	// (order, contents, iteration); the maps are lookup-only and are
	// never ranged, so map iteration order cannot reach any placement
	// decision (the determinism trap this fix must not introduce).
	idx := indexOps(&ops)
	for i, it := range items {
		// Batched context probe (entry check at i==0): this is the
		// loop that ran past the perf smoke's 180s deadline before the
		// fix; the probe bounds a cancellation to ctxCheckInterval
		// items instead of the whole plan.
		if i%ctxCheckInterval == 0 {
			if err := balanceCtxCheck(ctx); err != nil {
				return Plan{}, err
			}
		}
		charge := it.size + units.ImageEntryOverhead
		var dest *openDest
		if open.Len() > 0 && open[0].remaining >= charge {
			dest = heap.Pop(&open).(*openDest)
			dest.remaining -= charge
		} else {
			// Nothing open fits (or nothing is open): open a NEW
			// destination - worst-fit-decreasing's only way forward.
			// The fresh image reserves the fixed overhead first, so
			// its payload budget is usable - FIXED_OVERHEAD. Its id
			// is the lowest not in use (4.6 lowest-unused minting).
			dest = &openDest{imageID: idAlloc.mint(),
				remaining: usable - units.FixedImageOverhead - charge}
			newIDs = append(newIDs, dest.imageID)
			fills[dest.imageID] = &DestFill{
				ImageID: dest.imageID, NominalSize: profile.NominalBytes,
				UsableSize: usable, MediaProfile: profile.Name,
			}
		}
		fills[dest.imageID].UsedBytes += it.size
		fills[dest.imageID].ItemCount++
		recordAssignment(&idx, it, dest.imageID)
		heap.Push(&open, dest)
	}

	// --- empty-dir pass (4.3, with the 2026-10-20 entry-charge
	// capacity twist): every ACTIVE unassigned dir goes to the
	// LOWEST-numbered destination of the final plan whose RESERVED
	// budget can absorb the dirs' 512-byte entry charges; when none
	// can, the dirs open a new destination (the dirs-only precedent,
	// continued per destination until every dir has a home with entry
	// slack). Dirs are ordered by dir_id and consume zero BYTES. The
	// per-destination dir counts of THIS run are tracked separately
	// because the target may be a LOCKED one (whose historical fill
	// lives in the locked states, not in the new-destination fills).
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].DirID < dirs[j].DirID })
	newDirCounts := make(map[int64]int, 1)
	if len(dirs) > 0 {
		totalCharge := int64(len(dirs)) * units.ImageEntryOverhead
		target, remaining := lowestDirTarget(locked, fills, newIDs, totalCharge)
		openFallback := func() (int64, int64) {
			// A fresh destination for overflow dirs: its whole payload
			// budget (usable - FIXED_OVERHEAD) is entry capacity, since
			// dirs carry zero bytes. The id is the lowest not in use
			// (4.6 lowest-unused minting, the second mint site).
			id := idAlloc.mint()
			newIDs = append(newIDs, id)
			fills[id] = &DestFill{
				ImageID: id, NominalSize: profile.NominalBytes,
				UsableSize: usable, MediaProfile: profile.Name,
			}
			return id, usable - units.FixedImageOverhead
		}
		if target == 0 {
			target, remaining = openFallback()
		}
		for i, d := range dirs {
			// Batched context probe: dirs are per-item work too (a
			// multi-million-dir set gets the same prompt abort).
			if i%ctxCheckInterval == 0 {
				if err := balanceCtxCheck(ctx); err != nil {
					return Plan{}, err
				}
			}
			if remaining < units.ImageEntryOverhead {
				target, remaining = openFallback()
			}
			ops.dirs = append(ops.dirs, dirOp{dirID: d.DirID, relPath: d.RelPath, imageID: target})
			newDirCounts[target]++
			remaining -= units.ImageEntryOverhead
		}
	}

	// --- the images rows for every NEW destination (4.5 writes them
	// inside the transaction; the list stays deterministic) ---
	sort.Slice(newIDs, func(i, j int) bool { return newIDs[i] < newIDs[j] })
	for _, id := range newIDs {
		f := fills[id]
		ops.images = append(ops.images, ImageSpec{
			ImageID: id, NominalSize: f.NominalSize,
			UsableSize: f.UsableSize, MediaProfile: f.MediaProfile,
		})
	}

	result := Result{
		NewDestinations:     len(newIDs),
		FilesAssigned:       len(ops.wholeFiles) - symlinksAssigned,
		ChunksAssigned:      countChunkAssigns(ops.chunkFiles),
		SymlinksAssigned:    symlinksAssigned,
		DirsAssigned:        len(ops.dirs),
		SecondariesExcluded: secondariesExcluded,
		RechunkedFiles:      rechunkedFiles,
		RehashedAtNextWrite: rehashedAtNextWrite,
	}
	result.Destinations = assembleDestinations(locked, fills, newDirCounts)
	// The D-05 wasted-residual figure (4.4) under the 2026-10-20
	// capacity amendment (4.4 consequence c): for a LOCKED destination
	// the residual is the TRUE free space that will remain on the
	// built image - usable_size minus the reserved overhead
	// (FixedImageOverhead + 512 * entries, dirs included, the builder's
	// entry accounting) minus the payload. Judgment call, documented:
	// the amended spec's parenthetical spells `usable_size -
	// FIXED_OVERHEAD - payload` while its prose says "the true free
	// space that will remain on the built image"; the entry term is
	// part of that free space and of the builder's budget, so it is
	// subtracted here - a pre-amendment locked image that overflows
	// its usable_size yields a negative figure, clamped to zero (the
	// builder's pre-check, not this visibility line, owns refusing it).
	for _, d := range result.Destinations {
		if !d.Locked {
			continue
		}
		free := d.UsableSize - units.FixedImageOverhead -
			units.ImageEntryOverhead*int64(d.ItemCount+d.DirCount) - d.UsedBytes
		if free > 0 {
			result.WastedResidualBytes += free
			result.WastedResidualImages++
		}
	}
	return Plan{Result: result, ops: ops}, nil
}

// Write persists the plan in ONE planning transaction
// (DESIGN_SPEC 4.5): insert images rows, create/replace/drop chunk
// state, stamp files.chunk_size, write files.image_id and
// chunks.image_id assignments, persist directories.image_id, commit.
// Any failure rolls everything back - a failed plan leaves the
// catalog bit-identical (FR-010). A plan with no work to do (zero
// unassigned items) opens NO transaction at all and creates no empty
// destinations.
func (p *Plan) Write(ctx context.Context, factory AssignTxFactory) error {
	if p.ops.empty() {
		return nil
	}
	tx, err := factory(ctx)
	if err != nil {
		return fmt.Errorf("plan: beginning assignment transaction: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	for _, img := range p.ops.images {
		if err := tx.InsertImage(ctx, img); err != nil {
			return err
		}
	}
	for _, op := range p.ops.chunkFiles {
		if err := writeChunkFile(ctx, tx, op); err != nil {
			return err
		}
	}
	for _, op := range p.ops.wholeFiles {
		if op.dropStale {
			// NB2: the stale decomposition dies with the whole-file
			// transition, INSIDE this transaction.
			if _, err := tx.DropChunkState(ctx, op.fileID); err != nil {
				return err
			}
		}
		if err := tx.AssignFile(ctx, op.fileID, op.imageID); err != nil {
			return err
		}
	}
	for _, op := range p.ops.dirs {
		if err := tx.AssignDir(ctx, op.dirID, op.imageID); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// writeChunkFile runs one chunked file's write work in order: row
// lifecycle (create / reuse-verify / replace / recover-by-replace),
// the files.chunk_size stamp for every path that changed rows, then
// the per-chunk assignments. The reuse path writes NOTHING beyond the
// assignments: the stored rows and their hashes survive bit-for-bit
// (the 4.2 stakeholder contract: re-plan with the same profile = zero
// re-reads).
func writeChunkFile(ctx context.Context, tx *AssignTx, op chunkFileOp) error {
	switch op.action {
	case ActionCreateFresh:
		if err := tx.EnsureChunkRows(ctx, op.fileID, op.specs); err != nil {
			return err
		}
		if err := tx.StoreChunkSize(ctx, op.fileID, op.effective, len(op.specs)); err != nil {
			return err
		}
	case ActionReuse:
		// Rows verified at compute time (decompositionMatches); no row
		// write is needed or allowed here - assignments only.
	case ActionRechunk:
		if _, err := tx.ReplaceAllChunks(ctx, op.fileID, op.specs); err != nil {
			return err
		}
		if err := tx.StoreChunkSize(ctx, op.fileID, op.effective, len(op.specs)); err != nil {
			return err
		}
	default:
		return fmt.Errorf("plan: unhandled chunk action %v for file %d", op.action, op.fileID)
	}
	for _, a := range op.assignments {
		if err := tx.AssignChunk(ctx, op.fileID, a.index, a.imageID); err != nil {
			return err
		}
	}
	return nil
}

// --- helpers ----------------------------------------------------------

// validateProfile enforces the D-06 preconditions Balance relies on,
// plus the 2026-10-20 capacity floor: usable_size must leave at least
// one entry's worth of payload room after the reserved image overhead
// (units.FixedImageOverhead + units.ImageEntryOverhead), or NO image
// could ever satisfy the 12.5 buildability invariant
// RequiredImageBytes(payload, entries) <= usable_size.
func validateProfile(p units.Profile) error {
	switch {
	case p.Name == "":
		return fmt.Errorf("%w: empty media profile name", ErrInvalidProfile)
	case p.NominalBytes <= 0:
		return fmt.Errorf("%w: non-positive nominal size %d", ErrInvalidProfile, p.NominalBytes)
	case p.UsableBytes <= 0:
		return fmt.Errorf("%w: non-positive usable size %d", ErrInvalidProfile, p.UsableBytes)
	case p.UsableBytes > p.NominalBytes:
		return fmt.Errorf("%w: usable size %d exceeds nominal size %d",
			ErrInvalidProfile, p.UsableBytes, p.NominalBytes)
	case p.UsableBytes <= units.FixedImageOverhead+units.ImageEntryOverhead:
		return fmt.Errorf("%w: usable size %d leaves no payload capacity after the reserved "+
			"image overhead (%d bytes fixed + %d per entry, DESIGN_SPEC 4.3/6.3)",
			ErrInvalidProfile, p.UsableBytes,
			units.FixedImageOverhead, units.ImageEntryOverhead)
	}
	return nil
}

// kindFor maps a files.entry_kind onto the item kind (symlinks never
// chunk, D-03).
func kindFor(entryKind string) itemKind {
	if entryKind == "symlink" {
		return itemSymlink
	}
	return itemWhole
}

// itemKindName renders an item kind for error messages.
func itemKindName(k itemKind) string {
	switch k {
	case itemWhole:
		return "whole file"
	case itemChunk:
		return "chunk"
	case itemSymlink:
		return "symlink"
	default:
		return fmt.Sprintf("itemKind(%d)", int(k))
	}
}

// planSpecs is PlanChunks with the error already contextualized.
func planSpecs(fileID, size, effective int64) ([]ChunkSpec, error) {
	p, err := PlanChunks(fileID, size, effective)
	if err != nil {
		return nil, err
	}
	return p.Chunks, nil
}

// sortItems applies the 4.3 determinism sort: descending size,
// ascending relpath, ascending chunk index. The key is TOTAL over the
// active set (unique relpaths are the reconciler's versioning
// invariant, not a DB constraint - see the Determinism note in the
// package doc; chunkIndex disambiguates chunks of one file), so the
// same inputs always produce the same plan - and a relpath tie could
// not make two runs differ either, since the input order is
// file_id-ascending and sort.Slice's behavior is deterministic for a
// fixed input sequence.
func sortItems(items []placeItem) {
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if a.size != b.size {
			return a.size > b.size
		}
		if a.relPath != b.relPath {
			return a.relPath < b.relPath
		}
		return a.chunkIndex < b.chunkIndex
	})
}

// groupStoredChunks folds the (file_id, chunk_index)-ordered state
// read into a per-file map.
func groupStoredChunks(rows []StoredChunk) map[int64][]StoredChunk {
	out := make(map[int64][]StoredChunk)
	for _, r := range rows {
		out[r.FileID] = append(out[r.FileID], r)
	}
	return out
}

// allChunksAssigned reports whether stored is a complete assigned
// chunk set: rows exist and every one carries an image_id - the
// LOCKED skip of FR-011 (a fully placed chunked file's family bytes
// are on media and must never be re-placed).
//
// A PARTIAL set - some rows assigned, some not - is NOT catalog
// corruption: it is the transient state DESIGN_SPEC 4.6 creates when
// a scoped unlock releases only the chunks that sat on the unlocked
// image. The balancer places the UNASSIGNED half on the reuse path
// (4.3's PARTIALLY-ASSIGNED CHUNK SETS rule, 4.6 "Partial chunk
// sets") and REFUSES - naming the file, the locked image ids, and the
// remedy(s) - on every path that would rewrite the locked half
// (whole-file transition, changed effective chunk size, failed
// decomposition verify, hard-link family exclusion). The write path's
// FR-011 row guards (ErrChunkRowAssigned, the image_id IS NULL
// assignment guards) remain as defense-in-depth beneath that
// compute-time refusal.
func allChunksAssigned(stored []StoredChunk) bool {
	if len(stored) == 0 {
		return false
	}
	for _, r := range stored {
		if r.ImageID == 0 {
			return false
		}
	}
	return true
}

// isPartialChunkSet reports whether stored is a PARTIALLY-assigned
// chunk set: rows exist, at least one carries an image_id, and at
// least one does not (0 < assigned < total). This is the 4.6
// transient state a scoped unlock creates - the balancer's reuse path
// completes it, and every rewrite path refuses it (see
// allChunksAssigned).
func isPartialChunkSet(stored []StoredChunk) bool {
	if len(stored) == 0 {
		return false
	}
	assigned := 0
	for _, r := range stored {
		if r.ImageID != 0 {
			assigned++
		}
	}
	return assigned > 0 && assigned < len(stored)
}

// The refusal remedies of 4.3/4.6, quoted in substance: the profile
// remedy (re-plan under the profile that produced the stored
// decomposition) and the whole-set remedy (clear-assignments over the
// WHOLE set releases every locked row, then re-plan). The partial-set
// compute branches offer BOTH; the hard-link family branch offers the
// whole-set remedy only, because no profile choice can make the
// family rule place a secondary row (4.3 PARTIALLY-ASSIGNED CHUNK
// SETS).
const (
	remedyKeepProfile                = "keep the current media profile"
	remedyClearWholeSet              = "run clear-assignments (whole set)"
	remedyKeepProfileOrClearWholeSet = remedyKeepProfile +
		" to place the unassigned chunks, or " + remedyClearWholeSet
)

// partialChunkRefusal builds the ErrPartialChunkReplan error: it
// always names the file, the count and image ids of the LOCKED half,
// the branch's reason, and that branch's remedy(s) - the message
// contract 4.3/4.6 pins (and AC-23 asserts through the real
// commands). The locked ids are collected in stored-row order (which
// the catalog read fixes as (file_id, chunk_index) ascending) and
// de-duplicated through a LOOKUP-ONLY map, then sorted ascending, so
// the message is byte-identical across runs: map iteration never
// reaches the output.
func partialChunkRefusal(f ActiveFile, stored []StoredChunk, reason, remedy string) error {
	assigned := 0
	seen := make(map[int64]bool, len(stored))
	var lockedIDs []int64
	for _, r := range stored {
		if r.ImageID == 0 {
			continue
		}
		assigned++
		if !seen[r.ImageID] {
			seen[r.ImageID] = true
			lockedIDs = append(lockedIDs, r.ImageID)
		}
	}
	sort.Slice(lockedIDs, func(i, j int) bool { return lockedIDs[i] < lockedIDs[j] })
	idList := ""
	for i, id := range lockedIDs {
		if i > 0 {
			idList += ", "
		}
		idList += fmt.Sprintf("%d", id)
	}
	return fmt.Errorf("%w: file %q has %d of %d chunks locked on image(s) %s: %s; %s",
		ErrPartialChunkReplan, f.RelPath, assigned, len(stored),
		idList, reason, remedy)
}

// familyKey identifies a hard-link family (2.4: same dev+inode).
// inode == 0 marks an uncaptured sentinel and never forms a family.
type familyKey struct {
	dev   int64
	inode int64
}

// hardLinkPrimaries resolves one primary per hard-link family among
// the catalog-ACTIVE rows: families are regular files with nlink > 1
// sharing (dev, inode) and a known inode; the primary is the member
// with the lowest file_id (2.4: "one representative ('primary')
// carries content"; lowest file_id = first-seen row, deterministic).
// The map is keyed by family; isSecondary consults it per file.
func hardLinkPrimaries(files []ActiveFile) map[familyKey]int64 {
	primaryOf := make(map[familyKey]int64)
	for _, f := range files {
		if f.Nlink <= 1 || f.Inode == 0 || f.EntryKind != "regular" {
			continue
		}
		key := familyKey{dev: f.Dev, inode: f.Inode}
		if prim, ok := primaryOf[key]; !ok || f.FileID < prim {
			primaryOf[key] = f.FileID
		}
	}
	return primaryOf
}

// isSecondary reports whether f is a hard-link SECONDARY: a regular
// file with nlink > 1 whose (dev, inode) family's primary is a
// DIFFERENT (lower file_id) active row. Secondaries are excluded from
// items entirely (2.4): the primary represents the family and byte
// accounting counts the family once. If the primary is itself already
// assigned (locked on media from an earlier plan), its unassigned
// secondaries are excluded too - the family's bytes are already on
// media, and recreating the extra names is the copy engine's
// link(2)-preserving concern, not a placement concern.
func isSecondary(f ActiveFile, primaryOf map[familyKey]int64) bool {
	if f.Nlink <= 1 || f.Inode == 0 || f.EntryKind != "regular" {
		return false
	}
	prim, ok := primaryOf[familyKey{dev: f.Dev, inode: f.Inode}]
	return ok && prim != f.FileID
}

// summarizeStored folds the stored rows of one file into the
// chunker's StoredChunks decision input (4.2 boundary stability).
func summarizeStored(chunkSize int64, rows []StoredChunk) StoredChunks {
	s := StoredChunks{ChunkSize: chunkSize}
	if chunkSize == 0 {
		// Never stamped: any orphan rows (a corrupt-catalog edge) are
		// not this file's stored decomposition.
		return s
	}
	s.WrittenChunks = countWritten(rows)
	s.UnwrittenChunks = len(rows) - s.WrittenChunks
	return s
}

// countWritten counts stored rows carrying a hash (hash NOT NULL):
// the rows a re-chunk forces to be re-read and re-hashed (4.2).
func countWritten(rows []StoredChunk) int {
	n := 0
	for _, r := range rows {
		if r.HashPresent {
			n++
		}
	}
	return n
}

// decompositionMatches verifies the stored rows against the
// decomposition (count, indices, offsets, lengths) - the same check
// catalog verifyDecomposition performs, computed here from the compute-
// phase read so the REUSE decision needs no SQL round-trip. A mismatch
// means files.chunk_size lies about the rows (catalog-integrity edge
// documented in DecideChunking): the caller falls back to re-chunk.
func decompositionMatches(stored []StoredChunk, specs []ChunkSpec) bool {
	if len(stored) != len(specs) {
		return false
	}
	for i, r := range stored {
		s := specs[i]
		if r.Index != s.Index || r.Offset != s.Offset || r.Length != s.Length {
			return false
		}
	}
	return true
}

// openDest is one OPEN destination in the WFD heap: its id and its
// remaining capacity. Locked destinations never appear here (4.3).
type openDest struct {
	imageID   int64
	remaining int64
}

// destHeap is the max-heap of open destinations keyed by remaining
// capacity, with ties broken by SMALLEST image_id - the deterministic
// tie path that keeps identical inputs producing identical plans.
type destHeap []*openDest

func (h destHeap) Len() int { return len(h) }

func (h destHeap) Less(i, j int) bool {
	if h[i].remaining != h[j].remaining {
		return h[i].remaining > h[j].remaining
	}
	return h[i].imageID < h[j].imageID
}

func (h destHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *destHeap) Push(x any) { *h = append(*h, x.(*openDest)) }

func (h *destHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	old[n-1] = nil // avoid holding a reference
	*h = old[:n-1]
	return x
}

// imageIDAllocator mints destination ids by the LOWEST-UNUSED rule of
// DESIGN_SPEC 4.6 (the replacement for the old max+1 cursor): ids are
// handed out in ASCENDING order, skipping every id in use by an
// existing images row, starting at 1. Consequences, all pinned by the
// allocator unit set (balancer_mint_test.go):
//
//   - a CONTIGUOUS catalog {1..k} mints exactly k+1, k+2, ... - the
//     numerically identical result of the old max+1 rule, so the T2.4
//     exact-fresh-plan equivalence holds by construction;
//   - a mid-set unlock re-mints the RELEASED id first ("rewrite disk
//     3" stays disk 3), and with more than one hole the holes refill
//     LOWEST-first (an unlocked higher image receives payload only
//     after every lower hole is filled);
//   - spillover above the maximum is unchanged (the cursor runs past
//     the last in-use id).
//
// Construction folds the locked destinations into an in-use
// membership SET; mint() only ever advances a monotonic cursor, so ids
// are strictly increasing within a run and no id is ever handed out
// twice (a minted id joins the in-use set as well). The map is a
// LOOKUP ONLY - never ranged - so map iteration order can never reach
// a placement decision (the 12.5 determinism trap), and total cursor
// work is O(max id + mints).
type imageIDAllocator struct {
	inUse map[int64]bool
	next  int64
}

// newImageIDAllocator builds the allocator over the LOCKED
// destinations: every existing images row is an id in use. The input
// order is irrelevant (membership only), so the caller's sort of
// `locked` plays no part here. INVARIANT: the in-use set is exactly
// the images-row set - product commands never leave assignments
// referencing a row-less id (both clear modes NULL the assignments
// before deleting the row), so an assignment whose id is missing from
// this set can only be a pre-existing hand-edit (the residual class,
// pre-existing because foldPayloadFill silently drops orphan
// assignments).
func newImageIDAllocator(locked []DestinationState) *imageIDAllocator {
	a := &imageIDAllocator{
		inUse: make(map[int64]bool, len(locked)),
		next:  1,
	}
	for _, d := range locked {
		a.inUse[d.ImageID] = true
	}
	return a
}

// mint returns the LOWEST id not in use, advancing the cursor past
// every in-use id it meets. The cursor never rewinds, so successive
// calls are strictly increasing within the run and a released hole is
// filled before any id above it is considered.
func (a *imageIDAllocator) mint() int64 {
	for a.inUse[a.next] {
		a.next++
	}
	id := a.next
	a.inUse[id] = true // belt and braces: a minted id is in use too
	a.next++
	return id
}

// lowestDirTarget is the empty-dir pass's capacity-aware destination
// chooser (4.3 as amended 2026-10-20): the LOWEST-numbered destination
// (locked or new) whose RESERVED budget - usable - FixedImageOverhead
// - UsedBytes - 512*(payload items + already-assigned dirs) - absorbs
// the whole dir-charge totalCharge. ok-style zero return means no
// existing destination qualifies (the caller opens a fallback). A
// pre-amendment locked image that overflows its usable_size has a
// negative budget and is never chosen.
func lowestDirTarget(locked []DestinationState, fills map[int64]*DestFill,
	newIDs []int64, totalCharge int64) (int64, int64) {
	for _, d := range locked {
		remaining := d.UsableSize - units.FixedImageOverhead - d.UsedBytes -
			units.ImageEntryOverhead*int64(d.ItemCount+d.DirCount)
		if remaining >= totalCharge {
			return d.ImageID, remaining
		}
	}
	for _, id := range newIDs {
		f := fills[id]
		if f == nil {
			continue
		}
		remaining := f.UsableSize - units.FixedImageOverhead - f.UsedBytes -
			units.ImageEntryOverhead*int64(f.ItemCount)
		if remaining >= totalCharge {
			return id, remaining
		}
	}
	return 0, 0
}

// opsIndex is the fileID -> POSITION lookup over ONE planOps' whole-
// file and chunked-file op slices (T6.3: the pre-fix recordAssignment
// linearly scanned those slices once per placed item, which measured
// 4.2x/3.6x/3.9x doubling - O(n^2) - across the 12.8 perf smoke's
// 10k..200k probe).
//
// The index is a pure LOOKUP accelerator: the SLICES stay the source
// of truth for order, contents and iteration (write order is part of
// the determinism contract), so every downstream read of
// ops.wholeFiles / ops.chunkFiles - Plan.Write's ordered write-out
// above all - sees byte-identical data to the pre-fix code. The maps
// are never ranged in any placement decision, so map iteration order
// can never reach a plan.
//
// Validity: indexOps runs ONCE in Balance, at the point where the two
// slices are final (built in item construction, sorted by file_id,
// one op per file); recordAssignment only mutates elements in place
// and never appends, so a recorded position stays valid for the whole
// placement pass. The invariant that makes the map total - at most
// ONE whole-file op and ONE chunked-file op per file_id - holds
// because item construction visits each active file exactly once and
// takes exactly one branch; it is pinned by
// TestOpsIndexAgreesWithOpSlices.
type opsIndex struct {
	ops   *planOps
	whole map[int64]int // fileID -> index into ops.wholeFiles
	chunk map[int64]int // fileID -> index into ops.chunkFiles
}

// indexOps builds the position index over the (already-sorted) op
// slices. Slice order is not derived from the maps - they are filled
// FROM the slices - so construction order can never perturb it.
func indexOps(ops *planOps) opsIndex {
	idx := opsIndex{
		ops:   ops,
		whole: make(map[int64]int, len(ops.wholeFiles)),
		chunk: make(map[int64]int, len(ops.chunkFiles)),
	}
	for i, op := range ops.wholeFiles {
		idx.whole[op.fileID] = i
	}
	for i, op := range ops.chunkFiles {
		idx.chunk[op.fileID] = i
	}
	return idx
}

// recordAssignment folds one placed item into the write ops - O(1)
// (T6.3 fix; the pre-fix linear scan survives verbatim as the test-
// only oracle recordAssignmentLinear in balancer_fix_round_test.go,
// which the indexed path is pinned byte-identical to). Semantics are
// exactly the scan's: a chunk item appends {chunkIndex, imageID} to
// its parent file's assignment list (in placement order), a whole /
// symlink item stamps the file's imageID, and an item whose fileID
// has no matching op (unreachable through Balance, kept for parity)
// is a silent no-op.
func recordAssignment(idx *opsIndex, it placeItem, imageID int64) {
	ops := idx.ops
	switch it.kind {
	case itemChunk:
		if i, ok := idx.chunk[it.fileID]; ok {
			ops.chunkFiles[i].assignments = append(
				ops.chunkFiles[i].assignments,
				chunkAssign{index: it.chunkIndex, imageID: imageID})
		}
	default:
		if i, ok := idx.whole[it.fileID]; ok {
			ops.wholeFiles[i].imageID = imageID
		}
	}
}

// countChunkAssigns counts the chunk assignments across chunked files.
func countChunkAssigns(ops []chunkFileOp) int {
	n := 0
	for _, op := range ops {
		n += len(op.assignments)
	}
	return n
}

// assembleDestinations merges the LOCKED destinations (historical
// fill, Locked=true) with the newly computed fills, ordered by
// ascending image_id: the media count of the whole plan is
// len(Destinations) (FR-008/FR-009). newDirCounts adds THIS run's
// empty-dir assignments per destination - they may land on a locked
// destination (the 4.3 lowest-numbered rule) whose historical fill
// comes from DestinationState instead.
func assembleDestinations(locked []DestinationState, fills map[int64]*DestFill,
	newDirCounts map[int64]int) []DestFill {
	out := make([]DestFill, 0, len(locked)+len(fills))
	for _, d := range locked {
		out = append(out, DestFill{
			ImageID: d.ImageID, NominalSize: d.NominalSize,
			UsableSize: d.UsableSize, MediaProfile: d.MediaProfile,
			UsedBytes: d.UsedBytes, ItemCount: d.ItemCount,
			DirCount: d.DirCount + newDirCounts[d.ImageID], Locked: true,
		})
	}
	ids := make([]int64, 0, len(fills))
	for id := range fills {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, id := range ids {
		f := *fills[id]
		f.DirCount = newDirCounts[id]
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ImageID < out[j].ImageID })
	return out
}
