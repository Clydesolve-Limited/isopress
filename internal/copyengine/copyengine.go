package copyengine

// T3.3: the copy-set writer core (DESIGN_SPEC 5.1 per-file ordering,
// 5.2 chunked files, 4.3 lazy first-write chunk hashing, 5.4 metadata
// after content, 10.1 interrupted class, 12.7 crash/recovery seam;
// IMPLEMENTATION_PLAN T3.3).
//
// # The strict 5.1 ordering (per item, per attempt)
//
// CopyItem runs the DESIGN_SPEC 5.1 steps EXACTLY in this order:
//
//  1. lstat the source and compare the RESOLVED identifier set
//     against the catalog snapshot (fsmeta.DiffFields - the single
//     3.5/5.1 comparison, never a hand-rolled variant, parameterised
//     by the run's resolved Deps.Mode: full = size + mtime_ns + dev +
//     inode on stable filesystems, metadata = size + mtime_ns on
//     CIFS-class sources or per --change-detection, the 2026-10-20
//     amendment). A mismatch skips the item with a source-changed
//     result: no bytes are read, nothing is written, nothing is
//     marked. The skip is a WARNING class event, not a catalog failure
//     row (5.1 "record a source-changed event, continue"); the run
//     still reports it in the partial-failure class (10.2 exit 10)
//     through the caller. The relaxation removes only this EARLY SKIP,
//     never the content guarantee: step 5's read-back verify (FR-014)
//     remains the authoritative gate even under the metadata set.
//  2. Remove any leftover "<final>.partial" (crash recovery: 5.1
//     "any *.partial left by a crash is detected at the next copy
//     run, deleted, and the file re-copied" - detection at the next
//     run of THIS item is the honest, tree-size-independent place to
//     do it), then write the payload to "<final>.partial".
//  3. fsync the partial file; close it.
//  4. Atomically rename the partial to its final name; fsync the
//     containing directory (crash durability of the rename).
//  5. Read-back verify: re-open the FINAL file, hash it, compare with
//     the verify anchor (step 7's note below and the first-write rule).
//  6. On match: mark copied=1 in ONE small committed transaction
//     (Store.SetFileCopied / Store.SetChunkCopied; the catalog layer
//     implements the Store interface). The mark also writes the
//     FR-020 image_contents row INSIDE the same transaction, so the
//     finalized content list can never disagree with the copied flag.
//  7. On mismatch: re-lstat the source. If it changed between steps 1
//     and 5, classify source-changed (skip, delete the final file).
//     If the source is stable, classify a WRITE FAULT: delete the
//     final file and retry the whole ordering up to 3 times with
//     backoff; when the attempts are exhausted the failure is returned
//     for the caller to record - the run continues (exit 10 at the
//     end).
//
// THE INVARIANT: copied is NEVER set before the read-back verify
// passes. It is structurally obvious here: the only code paths that
// reach the Store mark calls sit strictly after a successful
// byte-for-byte comparison of the final file against the anchor, and
// every mismatch path deletes the final file before retrying. A
// verify-fail on a STABLE source therefore leaves copied=0 AND no
// final-name file behind (pinned by the never-mark-before-verify
// test).
//
// # Verify anchors and the 4.3 lazy-hash resolution
//
//   - Whole files: the anchor is the catalog files.hash. The source
//     stream is NOT hashed during the write (nothing would compare it
//     to - the catalog hash already describes the source); the
//     read-back hash of the final file is compared against the
//     catalog hash.
//   - First-write chunks (chunks.hash IS NULL): the source stream is
//     hashed WHILE it is written (io.MultiWriter into the partial and
//     the hasher - the 4.3 "chunk hashes are computed lazily at FIRST
//     WRITE" resolution). The read-back is compared against THAT
//     captured hash (not a pre-existing catalog value), and a match
//     stores it on the chunk row together with the copied mark in one
//     transaction (Store.SetChunkCopied receives the captured hash).
//     Every later verification of the chunk uses the stored hash.
//   - Re-copied chunks (chunks.hash already stored): the anchor is
//     the stored hash (5.2 "an already-written chunk re-copied on a
//     later run verifies against that stored hash").
//
// # The chunked whole-file hardening seam (2026-10-20, CIFS ruling)
//
// The 3.5 safety analysis surfaced a documented residual: a CHUNKED
// file replaced between scan and copy (same size + mtime_ns on an
// unstable-identifier filesystem) is captured self-consistently at
// first write (4.3), so its per-chunk hashes reflect the NEW content
// and the copy run would report clean; only the restore-time
// whole-file hash (FR-041) catches it. The architect's near-free
// hardening closes this at copy time and lives ENTIRELY in the
// pipeline layer (cli/copy.go), which owns the running whole-file
// accumulation and the final decision; this engine owns the two
// ordering seams that decision needs:
//
//   - Item.DeferMark (chunk items only): CopyItem runs 5.1 steps 1-5
//     (lstat-compare, write, fsync, rename, dir-fsync, read-back
//     verify) and returns OutcomeCopied with the verified Anchor but
//     performs NO mark and emits NO ItemDone - the caller decides.
//     The caller then either marks via MarkChunk (whole-file hash
//     matched: identical transaction to the normal path) or leaves
//     the verified bytes on media UNMARKED and reports the file-level
//     failure (whole-file mismatch). NO mark reversal ever happens:
//     the copied flag's meaning - "verified bytes on media" - stays
//     true for what was written, and the honest end state is
//     "verified-but-unmarked final chunk", which the next run
//     re-verifies and re-marks (or the rescan resolves).
//   - MarkChunk: the deferred 5.1 step-6 mark (the detached-mark 10.1
//     policy), so the caller's match branch runs the EXACT transaction
//     the normal path would have.
//
// The running whole-file accumulation deliberately does NOT live in
// this engine: a tee into a caller-owned hash cannot be made
// retry-safe across ITEMS (sha256 is not composable, an attempt-level
// reset would wipe earlier chunks' state, and a failed attempt's feed
// cannot be un-fed), so the pipeline accumulates from each chunk's
// VERIFIED media bytes after its successful copy - bytes the read-back
// verify has already proven equal to the source stream, which makes
// the accumulated sum the FR-041 reassembly anchor computed at copy
// time. The pipeline runs the whole-file compare ONLY when EVERY chunk
// of the parent was first-written in THIS run (a resume skips
// already-marked chunks, so their bytes are never seen; for resumed
// files the FR-041 restore anchor remains the detector - documented in
// cli/copy.go and DESIGN_SPEC 3.5).
//
// # Resumability and the copied-flag trust model (12.7, FR-013)
//
// Items whose copied flag is already set are NOT re-verified on a
// re-run: the flag IS the trust. A completed mark means the data was
// verified at mark time, and re-verifying completed work on every
// resume would make resumes O(full set) instead of O(unfinished).
// Resumability is at file granularity for whole files and at CHUNK
// granularity for chunked files (5.2: the parent mirror flips only
// with the last chunk, so a run killed mid-file resumes exactly at
// the first unmarked chunk). This is deliberate and documented: a
// destination file silently corrupted AFTER a completed copy run is
// verify-media's detection domain (FR-021), not the copy re-run's.
//
// # The T3.4 metadata seam (DECISION, documented both sides; T3.4
// outcomes recorded)
//
// 5.4 fixes "metadata after content": metadata is applied AFTER
// content verification, so a metadata failure never leaves verified
// content marked unverified - it is a per-file WARNING (Result.Warning,
// surfaced by the caller as a Warn event and a run_events row). T3.3
// applies metadata itself, right after the mark, through fsmeta.Apply
// (the EPERM-downgrade contract lives there). The split with T3.4:
//
//   - T3.3 owns: content writes (whole files, chunks), verify, mark,
//     and the fsmeta.Apply call after the mark for whole files; it
//     also ships MINIMAL correct implementations of the three special
//     writes (WriteSymlink, WriteEmptyDir, LinkSecondary) because the
//     copy command must produce a faithful tree end to end.
//   - T3.4 owns: the SYMLINK/HARD-LINK/EMPTY-DIR CREATION SEMANTICS
//     and their test matrix (valid + dangling symlink round-trips,
//     link(2) inode-identity assertions, injected metadata-failure
//     ordering). T3.4 refines the specials in place and must NOT
//     re-apply metadata where this file already does (the Apply call
//     sites are named "T3.4 seam" below).
//
// T3.4 OUTCOMES (this file, specials.go, fsmeta):
//
//   - MODE CONVERSION: catalog `mode` columns hold RAW POSIX bits;
//     fsmeta.Apply/os.Chmod consume GO fs.FileMode semantics. The
//     conversion happens exactly once, at metaFromRaw below - the
//     single applyMetadata seam every writer (whole files and all
//     three specials) funnels through - via fsmeta.GoMode, the
//     canonical inverse of fsmeta.UnixMode (invariant doc lives on
//     GoMode; the exhaustive round-trip proof in fsmeta's tests, the
//     on-disk proofs in specials_test.go and cli's copy_specials
//     e2e).
//   - WriteEmptyDir ORDERING: kept as mkdir -> Apply -> fsync(parent)
//     -> SetDirContent (NOT the file path's metadata-last shape).
//     Rationale and the crash-repair argument are documented on the
//     specials.go file doc; the observable half - metadata precedes
//     the durable content row, and a row failure leaves the
//     re-assertable state - is pinned by tests.
//
// Chunk PARTS get no metadata application: a chunk is not a restored
// object. The parent's mode/uid/gid/mtime travel in the manifest and
// the catalog export, and RESTORE applies them to the reassembled
// file (DESIGN_SPEC 8.4).
//
// # Dependency seams (all injectable, per the T3.3 brief)
//
// Source (lstat + ranged open), Store (the catalog mark functions -
// *catalog.CopyStore satisfies this interface directly, no adapter),
// the progress Bus (nil-safe), the partial writer and read-back
// openers (fault injection), a sleep function for the retry backoff
// (fake-clock testable), and the 12.7 fail-after-N Hook. Defaults are
// the production OS implementations.

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

	"isopress/internal/fsmeta"
	"isopress/internal/progress"
)

// The retry policy of 5.1 step 7: up to MaxAttempts attempts per item,
// Backoff(attempt) slept between a write fault and the next attempt.
const (
	// MaxAttempts is the default per-item attempt budget (5.1:
	// "retry up to 3 times with backoff").
	MaxAttempts = 3
	// MarkDetachTimeout bounds the post-cancellation mark transaction
	// (the 10.1 "finishes the in-flight item's marking transaction"
	// clause): once the read-back has passed, the mark runs on a FRESH
	// short-timeout context derived from context.Background(), because
	// the run context is already cancelled and the driver would reject
	// every statement under it. The write is tiny and the flock (D-11)
	// guarantees no competing writer, so a 5-second bound is generous.
	// INFO-5 (fix round): this applies to EVERY verify-then-mark writer
	// - CopyItem's content mark AND the specials' marks in specials.go
	// (WriteSymlink, LinkSecondary) - so a SIGINT in any verify->mark
	// window completes the mark instead of recording a Failed item
	// under an interrupted run.
	MarkDetachTimeout = 5 * time.Second
)

// markContext returns the context ONE mark transaction must run on:
// the run context while it is live, and - once the run context has
// been CANCELLED (the 10.1 window between the read-back verify and the
// mark) - a FRESH Background-derived context with the
// MarkDetachTimeout bound, because the driver rejects statements under
// a cancelled context. The returned CancelFunc must be called after
// the mark returns (a no-op in the live case). This is the shared
// detached-mark primitive: CopyItem's content marks and the specials'
// marks (WriteSymlink, LinkSecondary - INFO-5) all route through it,
// so the interrupted-run mark window behaves identically everywhere.
func markContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}
	return context.WithTimeout(context.Background(), MarkDetachTimeout)
}

// defaultBackoff is the production retry backoff: 100ms, 200ms (the
// transient-NAS-fault window of DESIGN_SPEC 13, without stalling a
// long run on a permanent fault).
func defaultBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	return 100 * time.Millisecond << (attempt - 1)
}

// ErrHookAbort is the sentinel a test hook returns/wraps to abort the
// run deterministically (the in-process half of the 12.7 crash seam;
// the kill mode never returns at all). The copy pipeline treats a
// result carrying it as a terminal test abort, never as an item
// failure.
var ErrHookAbort = errors.New("copyengine: test hook abort")

// ItemKind distinguishes the two content item classes of 5.1/5.2.
type ItemKind int

const (
	// ItemWholeFile is a non-chunked regular file: one write, one
	// verify against the catalog files.hash, one mark.
	ItemWholeFile ItemKind = iota
	// ItemChunk is one chunk of a chunked parent: identical ordering
	// per chunk (5.2), lazy first-write hash capture (4.3), and the
	// parent mirror flip inside the last chunk's mark transaction.
	ItemChunk
)

// Item is one assigned content item the writer copies: a whole file or
// a chunk. Every field is row-shaped data the CALLER read from the
// catalog (the cli wiring layer maps catalog.CopyFile / catalog.CopyChunk
// onto this shape; DESIGN_SPEC 1.2 layering - this package never issues
// SQL and never imports the catalog).
type Item struct {
	Kind       ItemKind
	FileID     int64
	RelPath    string // source relpath; for chunks the PARENT relpath
	MediaPath  string // path inside the logical tree (image_contents row, events)
	SourcePath string // absolute path of the source (parent file for chunks)
	DestPath   string // absolute final destination path (never the .partial)

	// The 5.1 step-1 catalog snapshot of the SOURCE file (parent file
	// for chunks): compare fields only, against the RESOLVED D-10 set
	// (Deps.Mode; full = size + mtime_ns + dev + inode, metadata =
	// size + mtime_ns - the 2026-10-20 CIFS amendment).
	SourceSize int64
	MtimeNS    int64
	Dev        int64
	Inode      int64

	// Size is the payload byte count to write: the whole-file size, or
	// the chunk length. The write must produce EXACTLY this many bytes
	// (a short or long stream is an error that the step-7 re-lstat then
	// classifies).
	Size int64
	// Offset is the byte offset the source read starts at: 0 for whole
	// files, the catalog chunks.offset for chunk items.
	Offset int64

	// Hash is the verify anchor: the catalog files.hash for whole
	// files; the STORED chunks.hash for re-copied chunks; nil for a
	// FIRST-WRITE chunk (the anchor is then captured from the write
	// stream, 4.3).
	Hash []byte

	ChunkIndex int // catalog 0-based; only for ItemChunk

	// DeferMark (chunk items only, validated): run 5.1 steps 1-5 and
	// return the verified Anchor WITHOUT marking - the caller decides
	// between MarkChunk and leaving the verified bytes unmarked (the
	// 2026-10-20 whole-file hardening's ordering; see the file doc).
	// A DeferMark whole file is refused (validateItem): the
	// whole-file path applies 5.4 metadata after its mark, and no
	// caller needs a deferred whole-file mark.
	DeferMark bool

	// Metadata for the 5.4 post-verify Apply (whole files only; chunk
	// parts carry none - see the file doc's T3.4 seam section). Raw
	// POSIX permission bits in Mode (catalog files.mode convention).
	Mode uint32
	UID  uint32
	GID  uint32
}

// Outcome classifies one CopyItem result (the caller folds outcomes
// into run counters, run_events rows, and the 10.2 exit code).
type Outcome int

const (
	// OutcomeCopied: written, verified, and marked (copied=1 + the
	// image_contents row, one transaction).
	OutcomeCopied Outcome = iota
	// OutcomeSkipped: source-changed (5.1 step 1 or step 7) or a
	// writer-convention refusal (the media UTF-8 gate is enforced
	// upstream by the caller). NOT a catalog failure row; the run
	// reports it in the partial-failure class (10.2 exit 10).
	OutcomeSkipped
	// OutcomeFailed: write fault after the retry budget, an unreadable
	// source, or a store failure. Nothing is marked; the run continues
	// and the failure contributes to exit 10.
	OutcomeFailed
	// OutcomeAborted: the run context was cancelled mid-item (the
	// SIGINT/SIGTERM path) or a test hook aborted the run. The partial
	// is cleaned up best-effort; nothing is marked.
	OutcomeAborted
)

// Result is one CopyItem outcome plus the numbers the caller reports.
type Result struct {
	Outcome  Outcome
	Bytes    int64       // payload bytes written (OutcomeCopied)
	Attempts int         // attempts consumed
	Diff     fsmeta.Diff // OutcomeSkipped: the differing 5.1 fields
	Err      error       // skip/failure/abort cause
	Warning  string      // non-fatal metadata note (5.4 per-file warning)
	// Anchor is the effective verify anchor on OutcomeCopied: the
	// hash that was compared against the read-back AND stored on the
	// row - the catalog hash re-asserted for whole files, the 4.3
	// first-write capture (or the re-asserted stored hash on a
	// re-copy) for chunks. The manifest builder consumes it so a
	// chunk entry can never carry a NULL hash.
	Anchor []byte
}

// Deps are the injectable writer dependencies. The zero value selects
// the production defaults for every OPTIONAL seam; Store is required
// for any item that reaches a mark (a nil Store fails the item loudly
// rather than writing unverified-unmarked data silently).
type Deps struct {
	// Source is the source filesystem access (lstat + ranged open).
	// Default: OSSource{} (os.Lstat via fsmeta + os.Open).
	Source SourceFS
	// Store is the catalog mark surface (implemented by
	// *catalog.CopyStore; declared here so the writer stays
	// catalog-free and unit-testable against a fake).
	Store Store
	// Bus receives one ItemDone per completed item, one Warn per
	// skip, and one Error per failure (FR-042; nil-safe: nil silences
	// all progress output).
	Bus *progress.Bus
	// OpenPartial overrides the creation of the ".partial" file. Test
	// seam for injected write faults. Default: os.OpenFile
	// (O_CREATE|O_WRONLY|O_TRUNC, 0644).
	OpenPartial func(path string) (WriteSyncCloser, error)
	// ReadBack overrides the read-back verify open of the FINAL file.
	// Test seam for injected verify mismatches. Default: os.Open.
	ReadBack func(path string) (io.ReadCloser, error)
	// Sleep implements the retry backoff wait, ctx-aware. Test seam
	// (fake clock). Default: select on time.After and ctx.Done.
	Sleep func(ctx context.Context, d time.Duration) error
	// Backoff returns the delay before attempt+1. Default:
	// defaultBackoff (100ms, 200ms).
	Backoff func(attempt int) time.Duration
	// MaxAttempts overrides the per-item attempt budget. Default:
	// MaxAttempts (3, per 5.1 step 7).
	MaxAttempts int
	// Mode is the run's resolved change-detection mode (DESIGN_SPEC
	// 3.5; the cli resolves it once per run from the source root via
	// fsmeta.ResolveChangeDetection). It parameterises BOTH step-1 and
	// step-7 comparisons. The zero value is the FULL set - the safe
	// pre-amendment default (fsmeta.ComparisonFull), so a Deps that
	// carries no mode keeps the strongest comparison.
	Mode fsmeta.Mode
	// Hook is the 12.7 crash/fault injection seam, called at the
	// documented points below with the event kind. A non-nil error
	// aborts the item WITHOUT the write-fault retry loop (hook errors
	// are instrumentation, not NAS faults); wrapping ErrHookAbort
	// makes the caller treat the whole run as a test abort. The ctx
	// is the run context, so a blocking test hook can park until the
	// parent signals (the deterministic SIGINT e2e).
	Hook Hook
}

// WriteSyncCloser is the partial-file writer seam: a plain io.Writer
// that can fsync (step 3) and close. *os.File satisfies it.
type WriteSyncCloser interface {
	io.Writer
	Sync() error
	Close() error
}

// SourceFS is the source filesystem access the writer needs: lstat
// (the 5.1 step-1 and step-7 comparison) and a RANGED open that reads
// exactly length bytes from offset (0 length means "to EOF"), which is
// how a chunk write reads its byte range without materializing the
// parent.
type SourceFS interface {
	Lstat(path string) (fsmeta.Meta, error)
	Open(path string, offset, length int64) (io.ReadCloser, error)
}

// Store is the catalog mark surface (DESIGN_SPEC 5.1 step 6, 5.2
// mirror, 6.6 image_contents). *catalog.CopyStore - a set/image-bound
// handle - satisfies this interface with NO adapter: the signatures
// are primitives on purpose, so the catalog layer can implement them
// without importing this package (DESIGN_SPEC 1.2).
//
// Contract: every method commits ONE small transaction that contains
// BOTH the copied-flag write AND the image_contents row (the
// finalized content list can never disagree with the flag), and
// SetChunkCopied additionally flips the parent files.copied mirror in
// the SAME transaction when the marked chunk completes the parent
// (5.2).
type Store interface {
	// SetFileCopied marks one whole-file (or symlink, or hard-link
	// secondary) files row copied=1 and records its image_contents
	// row (kind derived by the catalog from entry_kind).
	SetFileCopied(ctx context.Context, fileID int64, mediaPath string) error
	// SetChunkCopied marks one chunk copied=1, stores hash on the
	// chunk row when non-nil (the first-write capture, 4.3), records
	// the image_contents 'chunk' row, and flips the parent mirror in
	// the same transaction when this was the last unmarked chunk
	// (5.2).
	SetChunkCopied(ctx context.Context, fileID int64, chunkIndex int, hash []byte, mediaPath string) error
	// SetChunkHash stores a chunk hash WITHOUT marking copied (the
	// standalone lazy-hash store; the writer's mark path stores the
	// hash through SetChunkCopied, but the surface exists for callers
	// that verify-then-store separately).
	SetChunkHash(ctx context.Context, fileID int64, chunkIndex int, hash []byte) error
	// SetDirContent records the image_contents 'dir' row for an empty
	// directory (dirs have no copied flag: mkdir is idempotent, 2.3).
	SetDirContent(ctx context.Context, dirID int64, mediaPath string) error
}

// Hook is the 12.7 injection seam's function type: called with the
// run context (so a blocking test hook can park until the parent
// signals) and the event. See Deps.Hook.
type Hook func(ctx context.Context, ev HookEvent) error

// HookKind names the injection points of the 12.7 seam.
type HookKind int

const (
	// HookAfterPartialWrite fires when the payload has been copied
	// into the .partial but before fsync/rename: a SIGKILL here (the
	// 12.7 crash child) leaves exactly a .partial remnant for the
	// in-flight item and nothing else.
	HookAfterPartialWrite HookKind = iota
	// HookAfterRename fires after the atomic rename, before the
	// directory fsync: a kill here leaves the final file
	// present-but-unmarked (the other legal crash state of 5.1).
	HookAfterRename
	// HookAfterDirSync fires after the directory fsync, before the
	// read-back verify.
	HookAfterDirSync
)

// HookEvent is one hook invocation.
type HookEvent struct {
	Kind HookKind
	Item Item
}

// OSSource is the production SourceFS: fsmeta.PathMeta (os.Lstat,
// never following a final symlink - D-03) and a ranged os.Open read.
type OSSource struct{}

// Lstat implements SourceFS via fsmeta.PathMeta.
func (OSSource) Lstat(path string) (fsmeta.Meta, error) {
	return fsmeta.PathMeta(path)
}

// Open implements SourceFS: offset seeks, length limits (length <= 0
// reads to EOF). The ranged form is what lets a chunk write stream
// exactly its byte range of the parent file.
func (OSSource) Open(path string, offset, length int64) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("seek %q to %d: %w", path, offset, err)
		}
	}
	if length <= 0 {
		return f, nil
	}
	return &limitedReadCloser{r: io.LimitReader(f, length), c: f}, nil
}

// limitedReadCloser pairs a limited reader with the underlying closer
// so the file descriptor is closed exactly once at stream end.
type limitedReadCloser struct {
	r io.Reader
	c io.Closer
}

func (l *limitedReadCloser) Read(p []byte) (int, error) { return l.r.Read(p) }
func (l *limitedReadCloser) Close() error               { return l.c.Close() }

// openPartialOS is the production partial writer: truncate-or-create
// at the .partial path with the destination file mode.
func openPartialOS(path string) (WriteSyncCloser, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
}

// defaultSleep waits for d, or until ctx is done (a cancelled run must
// never sit out a backoff delay).
func defaultSleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// withDefaults fills the optional seams with their production
// implementations.
func (d Deps) withDefaults() Deps {
	if d.Source == nil {
		d.Source = OSSource{}
	}
	if d.OpenPartial == nil {
		d.OpenPartial = openPartialOS
	}
	if d.ReadBack == nil {
		d.ReadBack = func(path string) (io.ReadCloser, error) { return os.Open(path) }
	}
	if d.Sleep == nil {
		d.Sleep = defaultSleep
	}
	if d.Backoff == nil {
		d.Backoff = defaultBackoff
	}
	if d.MaxAttempts <= 0 {
		d.MaxAttempts = MaxAttempts
	}
	return d
}

// emitWarn/emitError/emitDone are the nil-safe bus helpers.
func (d Deps) emitWarn(msg string) {
	if d.Bus != nil {
		d.Bus.Emit(progress.Warn{Msg: msg})
	}
}

func (d Deps) emitError(msg string) {
	if d.Bus != nil {
		d.Bus.Emit(progress.Error{Msg: msg})
	}
}

func (d Deps) emitDone(bytes int64, path string) {
	if d.Bus != nil {
		d.Bus.Emit(progress.ItemDone{Bytes: bytes, Path: path})
	}
}

// hook fires the 12.7 seam (nil-safe).
func (d Deps) hook(ctx context.Context, ev HookEvent) error {
	if d.Hook == nil {
		return nil
	}
	return d.Hook(ctx, ev)
}

// PartialPath returns the crash-recovery partial path of a final
// destination path (5.1: "<final>.partial"). Exported so the cli tests
// and the pipeline agree on the one derivation.
func PartialPath(final string) string { return final + ".partial" }

// CopyItem copies ONE item through the strict 5.1 ordering (see the
// file doc). It never panics on bad input: malformed items (missing
// anchor hash, non-positive size for a chunk, empty paths) fail
// cleanly as OutcomeFailed so the run records and continues.
func CopyItem(ctx context.Context, deps Deps, item Item) Result {
	d := deps.withDefaults()
	if d.Store == nil {
		return d.fail(item, 0, fmt.Errorf("copyengine: nil Store (the mark seam is required; nothing can be verified-then-marked without it)"))
	}
	if err := validateItem(item); err != nil {
		return d.fail(item, 0, err)
	}

	// The catalog comparison snapshot (5.1 step 1): the RESOLVED field
	// set only (Deps.Mode) - fsmeta.DiffFields is the single 3.5/5.1
	// comparison.
	catalogMeta := fsmeta.Meta{
		Size:    item.SourceSize,
		MtimeNS: item.MtimeNS,
		Dev:     uint64(item.Dev),
		Inode:   uint64(item.Inode),
	}

	var lastErr error
	for attempt := 1; attempt <= d.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return d.aborted(item, attempt, err)
		}

		// --- step 1: lstat + compare -----------------------------------
		// Re-lstat on EVERY attempt (not only the first): between
		// retries the source may change, and writing bytes that no
		// longer match the record is exactly what 5.1 forbids.
		meta, lerr := d.Source.Lstat(item.SourcePath)
		if lerr != nil {
			// An unreadable/vanished source at step 1 is an
			// ItemFailures-class problem (10.1 "unreadable source"):
			// record and continue, no write-fault retries (retrying a
			// permission error cannot heal it).
			return d.fail(item, attempt, fmt.Errorf("lstat source: %w", lerr))
		}
		if diff := fsmeta.DiffFields(meta, catalogMeta, d.Mode); diff.Any() {
			return d.skip(item, attempt, diff)
		}

		// --- steps 2 to 7 ---------------------------------------------
		written, warning, anchor, err := d.attempt(ctx, item)
		if err == nil {
			// Step 6 happened inside attempt (verify passed, mark
			// committed); attempt returns nil only after the mark.
			return Result{Outcome: OutcomeCopied, Bytes: written, Attempts: attempt, Warning: warning, Anchor: anchor}
		}
		var abort *abortError
		if errors.As(err, &abort) {
			// Context cancellation or a hook abort: not a write fault,
			// never retried.
			return d.aborted(item, attempt, abort.cause)
		}
		if ctx.Err() != nil {
			return d.aborted(item, attempt, ctx.Err())
		}
		lastErr = err

		// --- step 7: re-lstat and classify ----------------------------
		meta2, lerr := d.Source.Lstat(item.SourcePath)
		if diff := fsmeta.DiffFields(meta2, catalogMeta, d.Mode); lerr == nil && diff.Any() {
			// The source changed between steps 1 and 5: source-changed
			// skip (attempt() already deleted the final file).
			return d.skip(item, attempt, diff)
		}
		// Stable source (or the re-lstat itself failed, which proves
		// nothing about a change): WRITE FAULT. attempt() deleted the
		// final file; back off and retry per 5.1 step 7.
		if attempt < d.MaxAttempts {
			if err := d.Sleep(ctx, d.Backoff(attempt)); err != nil {
				return d.aborted(item, attempt, err)
			}
		}
	}
	return d.fail(item, d.MaxAttempts, lastErr)
}

// MarkChunk performs the 5.1 step-6 mark for a CHUNK item whose write
// was verified by a DeferMark CopyItem call: SetChunkCopied with the
// verified anchor and media path, inside the detached-mark policy of
// 10.1 (markContext - the mark completes even if the run context died
// between the verify and this call). This is the SAME transaction the
// normal path would have run (copied flag + 4.3 hash + image_contents
// row + the 5.2 parent mirror flip when the chunk completes the
// parent); it exists as a separate call ONLY so the 2026-10-20
// whole-file hardening can decide between marking and the honest
// verified-but-unmarked state after its final-chunk compare (file
// doc). A caller must invoke it ONLY on a whole-file-hash MATCH.
func MarkChunk(ctx context.Context, deps Deps, item Item, anchor []byte) error {
	d := deps.withDefaults()
	if item.Kind != ItemChunk {
		return fmt.Errorf("copyengine: MarkChunk is defined for chunk items only (item %q is kind %v)", item.RelPath, item.Kind)
	}
	if d.Store == nil {
		return fmt.Errorf("copyengine: nil Store (the mark seam is required; item %q)", item.RelPath)
	}
	markCtx, cancelMark := markContext(ctx)
	defer cancelMark()
	return d.Store.SetChunkCopied(markCtx, item.FileID, item.ChunkIndex, anchor, item.MediaPath)
}

// attempt runs 5.1 steps 2 to 6 for ONE try and returns the verified
// byte count plus the 5.4 metadata warning on success. Every failure
// path after the rename DELETES the final file (the
// never-mark-before-verify invariant's filesystem half), so a retry
// always starts from a clean destination state.
func (d Deps) attempt(ctx context.Context, item Item) (int64, string, []byte, error) {
	partial := PartialPath(item.DestPath)

	// Step 2a: crash recovery - a leftover partial of an earlier
	// crashed run is deleted before this write recreates it (5.1
	// "detected at the next copy run, deleted, and the file
	// re-copied"). Only OUR derived path is ever removed.
	if err := os.Remove(partial); err != nil && !os.IsNotExist(err) {
		return 0, "", nil, fmt.Errorf("removing stale partial %s: %w", partial, err)
	}
	// The logical tree's directories are created on demand (files/
	// subtrees, chunks/ subtrees); 0755 is the tree-default mode,
	// metadata application (5.4) never touches non-empty parents.
	if err := os.MkdirAll(filepath.Dir(item.DestPath), 0o755); err != nil {
		return 0, "", nil, fmt.Errorf("creating destination directory for %s: %w", item.DestPath, err)
	}

	// First-write chunks capture the SOURCE-STREAM hash as they write
	// (4.3 lazy hashing): the read-back compares against this capture,
	// and a match stores it on the chunk row.
	captureHash := item.Kind == ItemChunk && len(item.Hash) == 0

	src, err := d.Source.Open(item.SourcePath, item.Offset, item.Size)
	if err != nil {
		return 0, "", nil, fmt.Errorf("opening source: %w", err)
	}
	partialFile, err := d.OpenPartial(partial)
	if err != nil {
		_ = src.Close()
		return 0, "", nil, fmt.Errorf("creating partial %s: %w", partial, err)
	}

	var hasher interface{ Sum([]byte) []byte }
	var sink io.Writer = partialFile
	if captureHash {
		h := sha256.New()
		hasher = h
		sink = io.MultiWriter(partialFile, h)
	}
	// Step 2b: stream the payload, ctx-aware (a cancelled run must not
	// sit inside a multi-gigabyte io.Copy).
	n, cerr := io.Copy(sink, &ctxReader{ctx: ctx, r: src})
	if cerr != nil {
		_ = partialFile.Close()
		_ = os.Remove(partial)
		if ctx.Err() != nil {
			return 0, "", nil, &abortError{cause: ctx.Err()}
		}
		return 0, "", nil, fmt.Errorf("writing payload: %w", cerr)
	}
	if n != item.Size {
		_ = partialFile.Close()
		_ = os.Remove(partial)
		// A short/long stream means the source no longer matches the
		// record (it shrank/grew mid-read) or the read seam lied; the
		// step-7 re-lstat classifies it honestly.
		return 0, "", nil, fmt.Errorf("payload length mismatch: wrote %d bytes, catalog record says %d", n, item.Size)
	}
	if err := src.Close(); err != nil {
		_ = partialFile.Close()
		_ = os.Remove(partial)
		return 0, "", nil, fmt.Errorf("closing source: %w", err)
	}
	if err := d.hook(ctx, HookEvent{Kind: HookAfterPartialWrite, Item: item}); err != nil {
		_ = partialFile.Close()
		_ = os.Remove(partial)
		return 0, "", nil, hookFailure(err)
	}

	// Step 3: fsync the partial, close.
	if err := partialFile.Sync(); err != nil {
		_ = partialFile.Close()
		_ = os.Remove(partial)
		return 0, "", nil, fmt.Errorf("fsync partial %s: %w", partial, err)
	}
	if err := partialFile.Close(); err != nil {
		_ = os.Remove(partial)
		return 0, "", nil, fmt.Errorf("closing partial %s: %w", partial, err)
	}

	// Step 4: atomic rename, then fsync the containing directory.
	if err := os.Rename(partial, item.DestPath); err != nil {
		_ = os.Remove(partial)
		return 0, "", nil, fmt.Errorf("renaming %s to %s: %w", partial, item.DestPath, err)
	}
	if err := d.hook(ctx, HookEvent{Kind: HookAfterRename, Item: item}); err != nil {
		_ = os.Remove(item.DestPath)
		return 0, "", nil, hookFailure(err)
	}
	if err := syncDir(filepath.Dir(item.DestPath)); err != nil {
		_ = os.Remove(item.DestPath)
		return 0, "", nil, fmt.Errorf("fsync directory of %s: %w", item.DestPath, err)
	}
	if err := d.hook(ctx, HookEvent{Kind: HookAfterDirSync, Item: item}); err != nil {
		_ = os.Remove(item.DestPath)
		return 0, "", nil, hookFailure(err)
	}

	// Step 5: read-back verify - re-open the FINAL file and hash it.
	readHash, err := d.hashFile(item.DestPath)
	if err != nil {
		_ = os.Remove(item.DestPath)
		return 0, "", nil, fmt.Errorf("read-back verify: %w", err)
	}

	// Step 6: match -> mark (NEVER before this point).
	var anchor []byte
	switch {
	case captureHash:
		anchor = hasher.Sum(nil)
	case len(item.Hash) > 0:
		anchor = item.Hash
	default:
		// validateItem guarantees a whole file always carries its
		// catalog hash; unreachable for well-formed items.
		_ = os.Remove(item.DestPath)
		return 0, "", nil, errors.New("internal: no verify anchor after write")
	}
	if !bytes.Equal(readHash, anchor) {
		// Step 7's write-fault half: delete the final file and let the
		// caller classify (re-lstat) and retry.
		_ = os.Remove(item.DestPath)
		return 0, "", nil, fmt.Errorf("read-back hash mismatch: written %x, expected %x", readHash, anchor)
	}

	if item.DeferMark {
		// The 2026-10-20 whole-file hardening's deferred-mark path
		// (file doc): steps 1-5 are complete and the bytes are
		// verified, but the mark - and the ItemDone emission - belong
		// to the caller's decision point (MarkChunk after the
		// whole-file compare, or an honest verified-but-unmarked end
		// state on mismatch). Chunk parts carry no 5.4 metadata, so
		// there is nothing else to apply here.
		return n, "", anchor, nil
	}

	// The 10.1 "finishes the in-flight item's marking transaction"
	// clause: the read-back has passed, so the mark runs even when the
	// run context just got cancelled - on a FRESH short-timeout
	// context, because the driver rejects statements under a
	// cancelled one (markContext). If this detached mark fails, the
	// item stays present-but-unmarked and the next run re-verifies and
	// re-marks (5.1 crash-recovery text).
	markCtx, cancelMark := markContext(ctx)
	defer cancelMark()
	switch item.Kind {
	case ItemChunk:
		err = d.Store.SetChunkCopied(markCtx, item.FileID, item.ChunkIndex, anchor, item.MediaPath)
	default:
		err = d.Store.SetFileCopied(markCtx, item.FileID, item.MediaPath)
	}
	if err != nil {
		// Data verified but unmarked: an honest item failure (recorded,
		// run continues; the next run re-verifies and re-marks). The
		// final file is NOT deleted - it is verified content, and
		// deleting verified data because a SQL statement failed would
		// destroy exactly what the flag protects.
		return 0, "", nil, fmt.Errorf("marking copied: %w", err)
	}

	// 5.4 metadata AFTER content (whole files only; see the T3.4 seam
	// in the file doc). A metadata failure is a per-file WARNING: the
	// content is verified and marked, and the report notes the file
	// could not be fully restored. It never unmarks.
	warning := ""
	if item.Kind == ItemWholeFile {
		warning = applyMetadata(item.DestPath, metaFromRaw(item.Mode, item.UID, item.GID, item.MtimeNS))
	}

	d.emitDone(n, item.RelPath)
	return n, warning, anchor, nil
}

// applyMetadata runs the 5.4 post-verify metadata application through
// fsmeta.Apply (the EPERM-downgrade contract lives there) and returns
// the warning text for a per-file problem, or "" when clean.
//
// T3.4 SEAM: this is the ONE place the copy writer applies file
// metadata. T3.4's hardening (injected metadata failures, ordering
// assertions) refines this call site and must not add a second one.
func applyMetadata(destPath string, meta fsmeta.Meta) string {
	res, err := fsmeta.Apply(destPath, meta)
	if err != nil {
		return fmt.Sprintf("metadata: %v (content is verified and marked; re-run does not re-apply - see verify-media/restore for repair paths)", err)
	}
	return res.Warning
}

// metaFromRaw converts catalog-shaped metadata (raw POSIX permission
// bits, numeric ids, mtime in unix nanoseconds) into the fsmeta.Meta
// Apply expects.
//
// THE MODE CONVERSION INVARIANT (T3.4): the catalog stores RAW POSIX
// mode bits (DESIGN_SPEC 2.3 INTEGER column; fsmeta.UnixMode on the
// ingest write), while fsmeta.Apply/os.Chmod consume GO fs.FileMode
// semantics (special bits in their fs.ModeSetuid/Setgid/Sticky
// positions). The conversion between the two worlds happens EXACTLY
// HERE - the single applyMetadata seam of this package, which every
// metadata application (whole files, symlinks, empty dirs, hard-link
// secondaries) funnels through - via fsmeta.GoMode, the canonical
// inverse of fsmeta.UnixMode. Passing the raw POSIX value past this
// point (e.g. hand a catalog 0o4755 straight to os.Chmod) silently
// drops setuid/setgid/sticky: the T1.2 QA finding this seam exists
// to prevent. The catalog-mode column carries permission-class bits
// only (no file-type bits, 2.3), so GoMode's mask is lossless here.
func metaFromRaw(rawMode uint32, uid, gid uint32, mtimeNS int64) fsmeta.Meta {
	return fsmeta.Meta{
		Mode:    fsmeta.GoMode(rawMode),
		UID:     uid,
		GID:     gid,
		MtimeNS: mtimeNS,
	}
}

// hashFile streams the file at path through SHA-256 (the read-back
// verify of 5.1 step 5; a BARE io.Copy, i.e. the stdlib's own 32 KiB
// copy buffer - there is no explicit 1 MiB buffer on this path).
func (d Deps) hashFile(path string) ([]byte, error) {
	f, err := d.ReadBack(path)
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

// abortError marks the non-retryable abort class inside the retry
// loop (context cancellation, test-hook abort).
type abortError struct{ cause error }

func (e *abortError) Error() string { return "copyengine: aborted: " + e.cause.Error() }
func (e *abortError) Unwrap() error { return e.cause }

// hookFailure wraps a hook error so the retry loop never mistakes it
// for a NAS write fault; ErrHookAbort stays discoverable through
// errors.Is for the pipeline's terminal-abort decision.
func hookFailure(err error) error {
	if errors.Is(err, ErrHookAbort) {
		return &abortError{cause: fmt.Errorf("test hook: %w", err)}
	}
	return fmt.Errorf("test hook: %w", err)
}

// ctxReader aborts a long stream the moment the context is cancelled
// (a SIGINT must not wait for a multi-gigabyte io.Copy to drain).
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// syncDir fsyncs a directory (5.1 step 4's "crash durability of the
// rename"). EINVAL/ENOTSUP are tolerated: filesystems that do not
// implement directory fsync report them, and refusing to complete an
// otherwise verified copy on such filesystems would trade real
// durability (which the filesystem cannot provide either way) for a
// hard failure. Every OTHER dir-fsync error is an item failure.
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

// validateItem rejects malformed items before any filesystem effect.
func validateItem(item Item) error {
	switch {
	case item.RelPath == "":
		return errors.New("copyengine: item has no relpath")
	case item.SourcePath == "" || item.DestPath == "":
		return fmt.Errorf("copyengine: item %q lacks a source or destination path", item.RelPath)
	case item.Size < 0:
		return fmt.Errorf("copyengine: item %q has negative size %d", item.RelPath, item.Size)
	case item.Kind == ItemChunk && item.Size <= 0:
		return fmt.Errorf("copyengine: chunk %d of %q has non-positive length %d", item.ChunkIndex, item.RelPath, item.Size)
	case item.Kind == ItemChunk && item.ChunkIndex < 0:
		return fmt.Errorf("copyengine: %q has negative chunk index %d", item.RelPath, item.ChunkIndex)
	case item.Kind == ItemWholeFile && len(item.Hash) != sha256.Size:
		// An active regular file always carries its catalog hash
		// (unreadable rows are never active); a missing anchor is a
		// corrupt-catalog signal, and copying without an anchor would
		// write bytes nothing can verify - refused loudly.
		return fmt.Errorf("copyengine: whole file %q has no catalog hash (%d bytes); unreadable-class rows never reach media",
			item.RelPath, len(item.Hash))
	case item.Kind == ItemChunk && len(item.Hash) != 0 && len(item.Hash) != sha256.Size:
		return fmt.Errorf("copyengine: chunk %d of %q has a malformed stored hash (%d bytes)",
			item.ChunkIndex, item.RelPath, len(item.Hash))
	case item.DeferMark && item.Kind != ItemChunk:
		// The deferred-mark path exists for the chunked whole-file
		// hardening's final-chunk decision (file doc); the whole-file
		// path applies 5.4 metadata after its mark and has no caller
		// that defers, so a DeferMark whole file is a wiring bug -
		// refused loudly before any filesystem effect.
		return fmt.Errorf("copyengine: item %q requests DeferMark but is not a chunk item", item.RelPath)
	}
	return nil
}

// fail/aborted/skip are the outcome constructors that also emit the
// nil-safe progress events (Warn for skips, Error for failures) and
// fold the warning text for the caller.
func (d Deps) fail(item Item, attempts int, err error) Result {
	d.emitError(fmt.Sprintf("%s: %v", item.RelPath, err))
	return Result{Outcome: OutcomeFailed, Attempts: attempts, Err: err}
}

func (d Deps) aborted(item Item, attempts int, cause error) Result {
	return Result{Outcome: OutcomeAborted, Attempts: attempts, Err: cause}
}

func (d Deps) skip(item Item, attempts int, diff fsmeta.Diff) Result {
	d.emitWarn(fmt.Sprintf("%s: skipped, source changed (%s)", item.RelPath, diff))
	return Result{
		Outcome:  OutcomeSkipped,
		Attempts: attempts,
		Diff:     diff,
		Err:      fmt.Errorf("source changed (%s)", diff),
	}
}
