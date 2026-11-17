package catalog

// T3.3: the copy-side catalog surface (DESIGN_SPEC 1.2 layering - the
// catalog package is the ONLY component issuing SQL; 5.1 step 6 marks;
// 5.2 chunk mirror; 5.3 the per-image inventory the writer consumes;
// 6.6 image_contents at write time; 10.1/10.2 interrupted-run
// finalization; IMPLEMENTATION_PLAN T3.3).
//
// This file supplies everything the copy-set writer (internal/copyengine)
// and the `copy` command need, and nothing else:
//
//   - CopyInventory: the COMPLETE inventory of what image N must
//     contain per 5.3 - whole files and symlinks assigned to N
//     (files.image_id), chunk rows for chunked parents assigned to N
//     (chunks.image_id) joined with the parent's metadata and
//     whole-file hash, empty dirs assigned to N (directories.image_id),
//     and the hard-link SECONDARIES whose family primary sits on N
//     (secondaries carry no image_id - the balancer excludes them from
//     items entirely, 2.4 - and the copy tree recreates them with
//     link(2) next to the primary, D-04). THE T3.1 MANIFEST-HONESTY
//     RULE (fix round, the UNIFIED secondary-claim rule): the
//     inventory claims only members whose BYTES ARE WRITTEN ON IMAGE N
//     - a whole-file row that is a family SECONDARY whose PRIMARY is
//     ASSIGNED (on THIS image or any other - the family derivation,
//     not the assignment column, decides) is excluded from the Files
//     projection and claimed only as link(2) on the PRIMARY's image
//     (see excludeSecondaryWholeFileClaims), and CopyPlacement applies
//     the matching filter so the placement claim follows the same truth;
//   - the mark functions (SetFileCopied, SetChunkCopied with the lazy
//     hash, SetChunkHash, SetDirContent on CopyStore): each commits
//     ONE small transaction containing BOTH the copied-flag write AND
//     the image_contents row, so the FR-020 finalized content list can
//     never disagree with the flag; SetChunkCopied additionally flips
//     the parent files.copied mirror in the SAME transaction when the
//     marked chunk completes the parent (5.2);
//   - RecordEmbedded: the image_contents 'manifest' / 'export' self
//     rows the writer appends when it embeds those members (6.6 - the
//     manifest cannot list its own hash, so the writer records the
//     rows here; see internal/media/manifest.go's self-entry reading);
//   - the whole-set reads the MANIFEST.json input needs (6.6):
//     CopyPlacement (which image holds which payload paths) and
//     CopyHardLinkFamilies (the D-04 link groups);
//   - runs bookkeeping for the copy command (StartCopyRun) and the
//     interrupted-run finalization closure (FinishRunDetached).
//
// SQL stays here; the services never import this package (the
// copyengine Store interface is satisfied by *CopyStore STRUCTURALLY -
// its signatures are primitives on purpose, so neither package
// imports the other; the cli wiring layer maps the row shapes, the
// scanStoreTx precedent).
//
// Error classification mirrors the established pattern:
// statement/commit failures inside an open transaction are plain
// wrapped errors (honest fatal class, DESIGN_SPEC 10.1); the set
// lookups fail with sql.ErrNoRows-sentinel-wrapped errors the cli maps
// to usage input.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// finalizeRunTimeout bounds FinishRunDetached: the run row write is a
// single tiny UPDATE issued AFTER the run context may already be
// cancelled (see its doc - the T0.5/T1.6 interrupted-run deferral
// closure for the copy command).
const finalizeRunTimeout = 5 * time.Second

// --- the mark surface (CopyStore) --------------------------------------------

// CopyStore is the set/image-bound mark handle the copyengine Store
// interface needs: every method writes the copied flag and the
// image_contents row of ONE set+image in ONE transaction. Construct
// with (*DB).CopyStore; the handle is cheap (two fields) and follows
// the run's connection pool.
type CopyStore struct {
	db      *DB
	setID   string
	imageID int64
}

// CopyStore returns the mark handle for one image of one set.
func (db *DB) CopyStore(setID string, imageID int64) *CopyStore {
	return &CopyStore{db: db, setID: setID, imageID: imageID}
}

// SetFileCopied marks one files row copied=1 and records its
// image_contents row, ONE transaction (5.1 step 6, 6.6).
//
// The image_contents KIND is derived from the row's own entry_kind:
// 'regular' -> 'file', 'symlink' -> 'symlink' (the writer uses this
// one function for whole files, symlinks, and hard-link secondaries
// alike - all three are files rows marked through the same 5.1
// verify-then-mark ordering). Size and hash are read FROM the row
// inside the transaction, so the content list records exactly what the
// catalog holds: files.size and files.hash (NULL for symlinks, the
// 2.3 convention).
//
// The FR-011 lock invariant is not at issue here: copied is write-once
// state of an already-locked assignment; a zero-row UPDATE means the
// row does not exist in this set (a caller bug or a corrupt catalog)
// and fails loudly rather than silently recording an orphan content
// row.
func (cs *CopyStore) SetFileCopied(ctx context.Context, fileID int64, mediaPath string) error {
	if mediaPath == "" {
		return fmt.Errorf("catalog: SetFileCopied for file %d: empty media path", fileID)
	}
	return cs.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE files SET copied = 1 WHERE file_id = ? AND set_id = ?`, fileID, cs.setID)
		if err != nil {
			return fmt.Errorf("catalog: marking file %d copied (set %q): %w", fileID, cs.setID, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("catalog: marking file %d copied: no such file_id in set %q", fileID, cs.setID)
		}
		var (
			entryKind string
			size      int64
			hash      []byte
		)
		if err := tx.QueryRowContext(ctx,
			`SELECT entry_kind, size, hash FROM files WHERE file_id = ? AND set_id = ?`,
			fileID, cs.setID).Scan(&entryKind, &size, &hash); err != nil {
			return fmt.Errorf("catalog: reading file %d for its content row: %w", fileID, err)
		}
		kind := "file"
		if entryKind == "symlink" {
			kind = "symlink"
		}
		return replaceImageContent(ctx, tx, imageContentRow{
			setID: cs.setID, imageID: cs.imageID, mediaPath: mediaPath,
			kind: kind, fileID: fileID, size: size, hash: hash,
		})
	})
}

// SetChunkCopied marks one chunks row copied=1, stores hash on the row
// when non-nil (the FIRST-WRITE capture of 4.3 flows in here and is
// persisted BEFORE the flag - one transaction, so a crash can never
// leave a stored hash without its flag or the reverse), records the
// image_contents 'chunk' row, and - 5.2 - flips the parent
// files.copied mirror in the SAME transaction when this marked chunk
// is the LAST unmarked chunk of the parent.
//
// hash semantics: nil = leave the stored hash untouched (a re-copy
// verifies against the stored anchor and re-asserts it); non-nil =
// store it (validated as a 32-byte sha256, D-09).
//
// The mirror is a COUNT PROOF inside the transaction, not a caller
// signal: after this chunk's UPDATE, the parent is complete exactly
// when every stored chunk row of the parent carries copied=1. A caller
// cannot get the mirror wrong, and a crash between two chunks'
// transactions leaves the mirror honestly false until the last chunk
// commits (chunk-granularity resumability, FR-041).
func (cs *CopyStore) SetChunkCopied(ctx context.Context, fileID int64, chunkIndex int, hash []byte, mediaPath string) error {
	if mediaPath == "" {
		return fmt.Errorf("catalog: SetChunkCopied for chunk %d of file %d: empty media path", chunkIndex, fileID)
	}
	if len(hash) != 0 && len(hash) != 32 {
		return fmt.Errorf("catalog: SetChunkCopied for chunk %d of file %d: hash is %d bytes, want 32 (D-09)",
			chunkIndex, fileID, len(hash))
	}
	return cs.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE chunks SET copied = 1, hash = COALESCE(?, hash)
			 WHERE set_id = ? AND file_id = ? AND chunk_index = ?`,
			hash, cs.setID, fileID, chunkIndex)
		if err != nil {
			return fmt.Errorf("catalog: marking chunk %d of file %d copied (set %q): %w",
				chunkIndex, fileID, cs.setID, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return fmt.Errorf("catalog: marking chunk %d of file %d copied: no such chunk row in set %q",
				chunkIndex, fileID, cs.setID)
		}
		var (
			stored []byte
			length int64
		)
		if err := tx.QueryRowContext(ctx,
			`SELECT hash, length FROM chunks WHERE set_id = ? AND file_id = ? AND chunk_index = ?`,
			cs.setID, fileID, chunkIndex).Scan(&stored, &length); err != nil {
			return fmt.Errorf("catalog: reading chunk %d of file %d for its content row: %w",
				chunkIndex, fileID, err)
		}
		if len(stored) != 32 {
			// A marked chunk MUST carry its 32-byte anchor (first
			// write stores the capture; a re-copy re-asserts the
			// stored one). Anything else is catalog corruption, and
			// recording a content row with a NULL chunk hash would
			// lie to restore (8.4 verifies every chunk against it).
			return fmt.Errorf("catalog: chunk %d of file %d has no stored hash after mark (corrupt catalog?)",
				chunkIndex, fileID)
		}
		// 5.2 mirror, SAME transaction: complete exactly when every
		// stored chunk of the parent is now copied.
		var total, copied int
		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*), SUM(copied) FROM chunks WHERE set_id = ? AND file_id = ?`,
			cs.setID, fileID).Scan(&total, &copied); err != nil {
			return fmt.Errorf("catalog: counting chunk rows of file %d for the parent mirror: %w", fileID, err)
		}
		if total > 0 && copied == total {
			if _, err := tx.ExecContext(ctx,
				`UPDATE files SET copied = 1 WHERE file_id = ? AND set_id = ?`, fileID, cs.setID); err != nil {
				return fmt.Errorf("catalog: mirroring completed chunks onto file %d: %w", fileID, err)
			}
		}
		return replaceImageContent(ctx, tx, imageContentRow{
			setID: cs.setID, imageID: cs.imageID, mediaPath: mediaPath,
			kind: "chunk", fileID: fileID, chunkIndex: chunkIndex, size: length, hash: stored,
		})
	})
}

// SetChunkHash stores a chunk hash WITHOUT marking copied (the
// standalone lazy-hash store of the Store surface). It only ever fills
// a NULL hash: an already-stored hash is never overwritten (the 4.2
// boundary-stability contract - stored hashes are the reuse budget of
// re-plans), and a differing already-stored hash is a loud error
// rather than a silent rewrite.
func (cs *CopyStore) SetChunkHash(ctx context.Context, fileID int64, chunkIndex int, hash []byte) error {
	if len(hash) != 32 {
		return fmt.Errorf("catalog: SetChunkHash for chunk %d of file %d: hash is %d bytes, want 32 (D-09)",
			chunkIndex, fileID, len(hash))
	}
	return cs.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`UPDATE chunks SET hash = ?
			 WHERE set_id = ? AND file_id = ? AND chunk_index = ? AND hash IS NULL`,
			hash, cs.setID, fileID, chunkIndex)
		if err != nil {
			return fmt.Errorf("catalog: storing hash of chunk %d of file %d: %w", chunkIndex, fileID, err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
		// Zero rows: either no such chunk, or the hash was already
		// stored. Distinguish honestly.
		var existing []byte
		if err := tx.QueryRowContext(ctx,
			`SELECT hash FROM chunks WHERE set_id = ? AND file_id = ? AND chunk_index = ?`,
			cs.setID, fileID, chunkIndex).Scan(&existing); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("catalog: SetChunkHash: no chunk %d of file %d in set %q",
					chunkIndex, fileID, cs.setID)
			}
			return fmt.Errorf("catalog: reading stored hash of chunk %d of file %d: %w", chunkIndex, fileID, err)
		}
		if string(existing) == string(hash) {
			return nil // idempotent re-store of the same anchor
		}
		return fmt.Errorf(
			"catalog: chunk %d of file %d already stores hash %x; refusing to overwrite with %x "+
				"(stored chunk hashes are the 4.2 re-plan reuse budget)",
			chunkIndex, fileID, existing, hash)
	})
}

// SetDirContent records the image_contents 'dir' row for one empty
// directory (FR-037, 6.6). Dirs carry NO copied flag (mkdir is
// idempotent, 2.3), so this row - written after the directory exists
// with its metadata - is the durable record of the member. The row is
// upserted: a resumed run re-records it at zero cost.
func (cs *CopyStore) SetDirContent(ctx context.Context, dirID int64, mediaPath string) error {
	if mediaPath == "" {
		return fmt.Errorf("catalog: SetDirContent for dir %d: empty media path", dirID)
	}
	return cs.tx(ctx, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM directories WHERE dir_id = ? AND set_id = ?`, dirID, cs.setID).
			Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("catalog: SetDirContent: no dir_id %d in set %q", dirID, cs.setID)
			}
			return fmt.Errorf("catalog: reading directory %d for its content row: %w", dirID, err)
		}
		return replaceImageContent(ctx, tx, imageContentRow{
			setID: cs.setID, imageID: cs.imageID, mediaPath: mediaPath,
			kind: "dir", size: 0, hash: nil,
		})
	})
}

// EmbeddedContent describes one embedded image member whose
// image_contents self-row the WRITER records after embedding it (6.6:
// the manifest and export rows are appended when the writer embeds
// those files - they have no files rows of their own).
type EmbeddedContent struct {
	MediaPath string // "MANIFEST.json" / "catalog-export.sqlite" (media.* constants)
	Size      int64  // published byte size
	Hash      []byte // 32-byte sha256 of the published bytes
}

// RecordEmbedded writes the image_contents 'manifest' or 'export'
// self-row (one transaction, upserted). kind must be 'manifest' or
// 'export' - the only two self members of 5.3/6.6; payload kinds flow
// through the mark functions above and are deliberately NOT accepted
// here.
func (cs *CopyStore) RecordEmbedded(ctx context.Context, kind string, c EmbeddedContent) error {
	switch kind {
	case "manifest", "export":
	default:
		return fmt.Errorf("catalog: RecordEmbedded kind %q must be 'manifest' or 'export' (payload kinds flow through the mark functions)", kind)
	}
	if c.MediaPath == "" {
		return fmt.Errorf("catalog: RecordEmbedded(%s): empty media path", kind)
	}
	if c.Size < 0 {
		return fmt.Errorf("catalog: RecordEmbedded(%s) %s: negative size %d", kind, c.MediaPath, c.Size)
	}
	if len(c.Hash) != 32 {
		return fmt.Errorf("catalog: RecordEmbedded(%s) %s: hash is %d bytes, want 32 (D-09)",
			kind, c.MediaPath, len(c.Hash))
	}
	return cs.tx(ctx, func(tx *sql.Tx) error {
		return replaceImageContent(ctx, tx, imageContentRow{
			setID: cs.setID, imageID: cs.imageID, mediaPath: c.MediaPath,
			kind: kind, size: c.Size, hash: c.Hash,
		})
	})
}

// imageContentRow is the internal image_contents write shape (2.3,
// used verbatim; fileID 0 means NULL).
type imageContentRow struct {
	setID      string
	imageID    int64
	mediaPath  string
	kind       string
	fileID     int64 // 0 -> NULL
	chunkIndex int   // only meaningful when kind == "chunk"; -1 -> NULL
	size       int64
	hash       []byte
}

// replaceImageContent upserts one image_contents row (the PRIMARY KEY
// (set_id, image_id, media_path) makes re-runs and resumed runs
// idempotent: a re-marked item re-records its row inside the SAME
// transaction as the flag, so the two can never diverge).
//
// KNOWN FORWARD-LOOKING CAVEAT (pairs with clearstore.go's recorded
// caveat): clear-assignments deletes images rows but NOT
// image_contents rows, so a clear + re-plan that produces a DIFFERENT
// image layout can leave stale content rows of the previous plan under
// an old (set, image, path) key. The upsert cannot remove rows of
// paths this plan no longer places. Revisiting clear-assignments for
// sets with write history is the recorded T2.4 scope item; the copy
// writer neither hides nor works around it here.
func replaceImageContent(ctx context.Context, tx *sql.Tx, r imageContentRow) error {
	var fileID, chunkIndex any
	if r.fileID != 0 {
		fileID = r.fileID
	}
	if r.kind == "chunk" {
		chunkIndex = r.chunkIndex
	}
	_, err := tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO image_contents
		   (set_id, image_id, media_path, kind, file_id, chunk_index, size, hash)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.setID, r.imageID, r.mediaPath, r.kind, fileID, chunkIndex, r.size, r.hash)
	if err != nil {
		return fmt.Errorf("catalog: recording image_contents row %q (image %d, set %q): %w",
			r.mediaPath, r.imageID, r.setID, err)
	}
	return nil
}

// RecordCopyFinalPath records where one image's copy tree landed (2.3
// images.final_path: "copy dir or .iso path once built") - the T5.1
// completion-path addition: the copy writer is the only writer that
// knows the destination directory, and verify-media resolves every
// image's location from this column (the build path already records
// its .iso path through FinishBuildTx). ONE idempotent statement, run
// when the image COMPLETES (its whole inventory is written and its
// manifest is on the tree); a zero-row update is a loud wiring error.
//
// T6.5 (the catalog-honesty gap): the SAME completion statement also
// stamps the two 2.3 columns no copy-path write ever set before -
//
//	mode  = 'copy'  - 2.3's mode comment says "set at first write",
//	and the copy completion IS the copy image's first (and only)
//	write of its row; the build path stamps mode='iso' in
//	FinishBuildTx, so the column now tells the truth about BOTH
//	modes instead of reading NULL for every copy image.
//
//	state = 'built' - THE DESIGN-VOCABULARY CHOICE, flagged for PM
//	ratification. DESIGN_SPEC section 6.3 defines 'built' as "the
//	image is finalized at its final_path" (the ISO wording), and
//	section 5 never names a copy-side state at all, so the sanctioned
//	vocabulary (the schema's own CHECK: planned | building | built |
//	verified | failed) leaves 'built' vs 'verified' as the only two
//	candidates a completed copy image could carry. 'built' wins on
//	design consistency: 7.2 reserves state 'verified' for the
//	verify-media FULL-MATCH stamp, and the copy writer must not
//	pre-claim a content-list verification it never performed (its
//	5.1 read-back verifies single FILES as they are written, not the
//	finalized image_contents list). 'building'/'failed' are
//	transient failure-path states the copy path does not own (its
//	failures live in run_events + the report), and 'planned' is the
//	very default this gap left behind.
//
// Consequences, all documented rather than papered over:
//
//   - Idempotent re-completion (a resume copy run re-writes the
//     manifest and re-records) sets state='built' UNCONDITIONALLY,
//     exactly as FinishBuildTx does on a rebuild: a row previously
//     stamped 'verified' by verify-media returns to 'built' with its
//     verified_at untouched, and the next full verify re-stamps it
//     (StampImageVerified applies from WHATEVER state the row
//     carries, its documented policy).
//   - Rows written BEFORE this change keep final_path but carry
//     mode NULL and state 'planned'; nothing rewrites them - the
//     stamp still heals them to 'verified', and verify-media derives
//     the source mode from a stat of final_path (never from this
//     column), so legacy rows stay fully readable.
//   - built_at is deliberately NOT stamped here: 2.3 pairs it with
//     the build path's rename-and-fsync finalize, which the copy
//     path has no direct analogue of (observations for the PM, not a
//     design claim).
func (db *DB) RecordCopyFinalPath(ctx context.Context, setID string, imageID int64, finalPath string) error {
	if finalPath == "" {
		return fmt.Errorf("catalog: RecordCopyFinalPath (set %q, image %d): empty final path", setID, imageID)
	}
	res, err := db.sql.ExecContext(ctx,
		`UPDATE images SET final_path = ?, mode = 'copy', state = 'built'
		 WHERE set_id = ? AND image_id = ?`,
		finalPath, setID, imageID)
	if err != nil {
		return fmt.Errorf("catalog: recording copy final path for image %d of set %q: %w", imageID, setID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("catalog: RecordCopyFinalPath: no images row for set %q image %d (run plan first)", setID, imageID)
	}
	return nil
}

// tx runs fn inside ONE small transaction and commits it - the 5.1
// step-6 "small transaction, committed immediately" shape. A plain
// driver BEGIN (not BEGIN IMMEDIATE) is deliberate here: these are
// single-statement-ish write bursts from the ONE flock-admitted writer
// (D-11), so the read-then-upgrade busy class of 2.1 cannot occur
// against a competing writer, and BeginTx keeps the pooling semantics
// simple. Any fn failure rolls back; the flag and the content row are
// atomic together or not at all.
func (cs *CopyStore) tx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := cs.db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("catalog: beginning copy mark transaction (set %q, image %d): %w",
			cs.setID, cs.imageID, err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("catalog: committing copy mark transaction (set %q, image %d): %w",
			cs.setID, cs.imageID, err)
	}
	return nil
}

// --- the per-image inventory (5.3) -------------------------------------------

// CopyFile is one whole regular file (or the primary of a hard-link
// family) assigned to the image: the files row's item-construction
// columns plus the copied flag that drives 12.7 resume (flag set =
// verified at mark time, trust it - see internal/copyengine's
// resumability contract).
type CopyFile struct {
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
	Hash     []byte // 32-byte sha256 (active regular rows always carry one)
	Copied   bool
	Chunked  bool // parent with a chunk decomposition (its bytes live in chunks/)
	ChunkCnt int  // stored chunk row count when Chunked
}

// CopySymlink is one symlink row assigned to the image (D-03: target
// carried verbatim; NULL hash by convention).
type CopySymlink struct {
	FileID     int64
	RelPath    string
	LinkTarget string
	Size       int64 // len(target) per 2.3
	Mode       uint32
	UID        uint32
	GID        uint32
	MtimeNS    int64
	Copied     bool
}

// CopyChunk is one chunks row assigned to the image, joined with the
// parent file's metadata and whole-file hash (the 6.6 ChunkedFile
// anchor and the restore reassembly target, FR-041).
type CopyChunk struct {
	ChunkID    int64
	FileID     int64
	ChunkIndex int // catalog 0-based (media name = index+1, media.ChunkFileName)
	Offset     int64
	Length     int64
	Hash       []byte // STORED hash; nil = first write pending (4.3 lazy)
	Copied     bool

	ParentRelPath string
	ParentSize    int64
	ParentMode    uint32
	ParentUID     uint32
	ParentGID     uint32
	ParentMtimeNS int64
	ParentDev     int64
	ParentInode   int64
	ParentHash    []byte // whole-file sha256 anchor
	// ParentNumChunks is the parent's TOTAL chunk row count
	// (files.chunk_count, stamped at assignment). Because the planner
	// assigns chunked files via chunks.image_id only (4.5), one image
	// can hold a SUBSET of a parent's chunks; the copy pipeline uses
	// this total to recognize the parent's FINAL chunk and to gate the
	// 2026-10-20 whole-file hardening (the check runs only when every
	// chunk of the parent is first-written in the same run -
	// DESIGN_SPEC 3.5's chunked residual).
	ParentNumChunks int
}

// CopyDir is one empty directory assigned to the image (FR-037; the
// directories table's durable rows, read exactly like the planner's).
type CopyDir struct {
	DirID   int64
	RelPath string
	Mode    uint32
	UID     uint32
	GID     uint32
	MtimeNS int64
}

// CopySecondary is one hard-link SECONDARY of a family whose primary is
// assigned to this image: the writer recreates it with link(2) against
// the primary's destination (D-04 copy mode, FR-039). Secondaries
// carry no image_id (the balancer excludes them from items, 2.4), so
// they are derived here from the dev+inode family grouping - the SAME
// derivation the balancer applies (see hardLinkRows).
type CopySecondary struct {
	FileID         int64
	RelPath        string
	Size           int64 // family size (shared content length)
	Mode           uint32
	UID            uint32
	GID            uint32
	MtimeNS        int64
	Hash           []byte // shared family hash (2.4)
	Copied         bool
	PrimaryFileID  int64
	PrimaryRelPath string
}

// ImageContentCount is the copy-time wipe gate's ONE catalog read
// (DESIGN_SPEC 4.6.1, step 1): how many image_contents rows the set's
// image N carries. image_contents is the FINALIZED per-image content
// list (2.3, 6.6, FR-020 - every mark commits its content row in the
// SAME transaction), so an empty list means nothing was finalized for
// this image in the current COPY-MODE generation and the gate may
// remove an existing <dest>/<SET-ID>-NNN tree before rewriting it.
//
// ERROR CONTRACT (4.6.1 step 1): a failed read is returned as a
// *CatalogError so report.Classify maps it to exit 4 - there is NO
// "treat error as zero" fallback, the gate is never bypassed. KindCorrupt
// is the closest 10.1 kind for an open catalog that cannot answer a
// read (the file is open, but not in a usable state for this query).
func (db *DB) ImageContentCount(ctx context.Context, setID string, imageID int64) (int, error) {
	var n int
	if err := db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM image_contents WHERE set_id = ? AND image_id = ?`, setID, imageID).
		Scan(&n); err != nil {
		return 0, &CatalogError{
			Kind: KindCorrupt,
			Path: db.path,
			Err:  fmt.Errorf("counting image_contents rows for set %q image %d: %w", setID, imageID, err),
		}
	}
	return n, nil
}

// CopyInventory is the complete write inventory of one image (5.3):
// what the writer must materialize under <dest-root>/S-NNN/ before the
// MANIFEST.json and catalog-export.sqlite members land. It is also the
// honesty boundary (T3.1, fix round): everything derived from it - the
// writer's items, the MANIFEST.json entries, the image_contents rows,
// the totals - claims only members whose bytes are written on THIS
// image (see excludeSecondaryWholeFileClaims for the hard-link rule).
type CopyInventory struct {
	Files       []CopyFile      // whole files assigned to the image (files.image_id)
	Symlinks    []CopySymlink   // symlink rows assigned to the image
	Chunks      []CopyChunk     // chunk rows assigned to the image
	Dirs        []CopyDir       // empty dirs assigned to the image
	Secondaries []CopySecondary // hard-link secondaries of this image's families
}

// CopyInventory reads the image's complete inventory (see the type
// doc). Ordering is deterministic (file_id / (file_id, chunk_index) /
// dir_id ascending) so two runs iterate identically - the crash tests
// and the report compare run over run.
//
// Only catalog-ACTIVE rows participate (the v_active_files predicate,
// spelled out): a missing or superseded row is not media this image
// must hold.
//
// THE T3.1 MANIFEST-HONESTY FILTER (fix round; option (a), the
// documented truth-source choice): the hard-link family derivation is
// read ONCE here and drives BOTH the secondaries projection AND
// excludeSecondaryWholeFileClaims, so the two can never disagree by
// construction. The inventory - and everything downstream of it (the
// writer, the MANIFEST.json entries, the image_contents rows, the
// placement claims via CopyPlacement's matching filter) - claims only
// members whose BYTES ARE WRITTEN ON THIS IMAGE. See
// excludeSecondaryWholeFileClaims for the full rule.
func (db *DB) CopyInventory(ctx context.Context, setID string, imageID int64) (CopyInventory, error) {
	var out CopyInventory
	if imageID <= 0 {
		return out, fmt.Errorf("catalog: CopyInventory: image id %d must be >= 1 (images are 1-based, 6.4)", imageID)
	}
	// One derivation per inventory read (the honesty filter and the
	// secondaries projection below consume the SAME families slice).
	families, err := db.hardLinkRows(ctx, setID)
	if err != nil {
		return out, err
	}
	if err := db.copyWholeFiles(ctx, setID, imageID, &out); err != nil {
		return out, err
	}
	out.Files = excludeSecondaryWholeFileClaims(out.Files, families)
	if err := db.copyChunks(ctx, setID, imageID, &out); err != nil {
		return out, err
	}
	if err := db.copyDirs(ctx, setID, imageID, &out); err != nil {
		return out, err
	}
	copySecondaries(imageID, families, &out)
	return out, nil
}

// excludeSecondaryWholeFileClaims is THE T3.1 MANIFEST-HONESTY FILTER
// of the per-image inventory, GENERALIZED by the T3.4 unified
// secondary-claim rule (the PM decision that closed the second
// family-claim defect; it subsumes the earlier excludeCrossImageSecondaries,
// whose cross-image-only shape applied the round's own rule - "an image
// claims only members whose BYTES are written on it" - only halfway).
//
// THE UNIFIED RULE: from the Files projection it drops every whole-file
// row that is a hard-link family SECONDARY whose family PRIMARY is
// ASSIGNED - on THIS image or on ANY other - because such a member is
// never an ordinary whole-file claim on ANY image: it is always written
// as link(2) on the PRIMARY's image (copySecondaries - same-image:
// next to the primary on that very image, the normal D-04 topology;
// cross-image: dropped from its own image entirely, its bytes existing
// only as the link on the primary's image). The member's own image_id
// is IRRELEVANT to where its claim lands - with ONE deliberate
// exception, the unassigned-primary edge below.
//
// THE DEFECT THIS GENERALIZATION KILLS (the QA reject, empirically
// probed): the same-image BOTH-ASSIGNED family (primary and member
// BOTH carrying image_id = the same image - seedable through
// direct-catalog assignment; the balancer never produces it, but the
// catalog accepts it) was left unfiltered by the cross-image-only
// shape. The member was then claimed TWICE in MUTUALLY EXCLUSIVE
// forms: written WHOLE via the Files projection, after which
// LinkSecondary hit EEXIST against that very whole file and refused
// to clobber (run 1: exit 10, image incomplete, manifest deferred,
// topology lost); and on run 2+ manifest generation failed
// PERMANENTLY - buildManifestInput added the member from both Files
// and Secondaries, tripping the generator's duplicate media-path
// refusal. The image could NEVER complete. One image claims a family
// member exactly ONCE - either the whole file (a primary) or the link
// (a secondary), never both.
//
// THE CROSS-IMAGE LIE the pre-generalization filter already pinned
// away (still covered, now by the unified rule): a family can form
// BETWEEN plan and copy (or between two copy runs) - plan-1 assigned
// A -> image 1 and B -> image 2 as distinct whole files, then B was
// replaced by a hard link to A. The catalog rows now derive as one
// family (shared dev+inode, nlink>1; each NAME is its OWN row,
// scanstore). Image 1's copy writes B as link(2) next to A and marks
// B's OWN file row (LinkSecondary marks item.FileID - the secondary's
// row, not the primary's). B's row on image 2 then reads copied=1 -
// and without this filter image 2's inventory (hence its MANIFEST.json
// entries and its placement claim) still listed B: an entry whose
// BYTES LIVE ONLY AS A LINK ON IMAGE 1. No data loss (image 1 holds
// everything), but a restore from image 2 alone would fail for that
// entry - the exact lie T3.1's gates exist to prevent.
//
// THE TRUTH SOURCE (option (a), chosen): the catalog's hard-link
// family derivation - hardLinkRows, the SAME dev+inode grouping the
// balancer applies (active nlink>1 regular rows, known inode, lowest
// file_id = primary) - is the ONE truth source, read once per
// inventory. An image claims exactly the members whose bytes are
// written on it:
//
//   - the family PRIMARY is an ordinary whole-file item on its
//     assigned image (files.image_id) - KEPT;
//   - every non-primary member is physically written as link(2) NEXT
//     TO ITS PRIMARY, on the primary's image (D-04 copy mode) - so a
//     member row carrying its OWN image assignment (the same image or
//     a different one) is DROPPED here: it is written, marked,
//     manifested, and placed only by the primary's image
//     (copySecondaries). The writer never sees the excluded member, so
//     it cannot diverge: skipping a member it was never asked to write
//     IS the correct behavior - its bytes exist as the link on the
//     primary's image.
//
// WHAT STAYS EXACTLY AS BEFORE:
//   - the normal D-04 shape (secondaries carry NO image_id, the
//     balancer excludes them from items, 2.4): such rows never appear
//     in the per-image files projection in the first place;
//   - a member whose primary is UNASSIGNED (image_id NULL): KEPT on
//     its own image - no image writes the family link in that state,
//     so the member's whole-file write is the only place its bytes
//     would land anywhere, and dropping it would lose data, not lies;
//   - non-family rows (nlink <= 1, or the inode==0 uncaptured
//     sentinel): the derivation never includes them, so they pass
//     through untouched.
//
// THE --image SUBSET HONESTY NOTE: the rule is SET-scoped, not
// selection-scoped. With --image selecting only the NON-PRIMARY image
// of a cross-image family, the member's bytes land NOWHERE in that
// run - its own image drops the whole-file claim, and the primary's
// image (which writes the link) is not selected. The whole-set
// placement summary embedded in the written image's manifest still
// names the primary's image, honestly pointing at where the bytes
// will live once that image is written.
func excludeSecondaryWholeFileClaims(files []CopyFile, families []linkFamily) []CopyFile {
	// member file_id -> the primary's assigned image (0 = primary
	// unassigned). Non-members are absent.
	primaryImage := make(map[int64]int64)
	for _, fam := range families {
		for _, m := range fam.members {
			if m.fileID != fam.primary.fileID {
				primaryImage[m.fileID] = fam.primary.imageID
			}
		}
	}
	if len(primaryImage) == 0 {
		return files // no families at all: nothing to filter
	}
	kept := files[:0]
	for _, f := range files {
		pimg, isMember := primaryImage[f.FileID]
		// THE UNIFIED RULE: a secondary with an ASSIGNED primary is
		// claimed by the primary's image only, whichever image both
		// rows name. Only the unassigned-primary edge keeps the
		// member's own whole-file claim (no-data-loss pin).
		if isMember && pimg != 0 {
			continue
		}
		kept = append(kept, f)
	}
	return kept
}

// copyWholeFiles reads whole files AND symlinks: rows with
// files.image_id = N. A chunked parent NEVER carries files.image_id
// (the planner assigns chunked files via chunks.image_id only, 4.5),
// so this projection cannot accidentally return a chunked parent as a
// whole file; the Chunked stamp is read anyway and carried for the
// caller's defense-in-depth checks.
func (db *DB) copyWholeFiles(ctx context.Context, setID string, imageID int64, out *CopyInventory) error {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT f.file_id, f.relpath, f.entry_kind, f.size, f.mode, f.uid, f.gid, f.mtime_ns,
		        f.dev, f.inode, f.nlink, f.link_target, f.hash, f.copied,
		        f.chunked, f.chunk_count
		 FROM files f
		 WHERE f.set_id = ? AND f.image_id = ? AND f.status = 'active' AND f.superseded_by IS NULL
		 ORDER BY f.file_id`, setID, imageID)
	if err != nil {
		return fmt.Errorf("catalog: reading file inventory of set %q image %d: %w", setID, imageID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only close; rows.Err dominates
	for rows.Next() {
		var (
			f          CopyFile
			s          CopySymlink
			entryKind  string
			linkTarget sql.NullString
			hash       []byte
			chunked    int64
		)
		if err := rows.Scan(&f.FileID, &f.RelPath, &entryKind, &f.Size, &f.Mode, &f.UID, &f.GID,
			&f.MtimeNS, &f.Dev, &f.Inode, &f.Nlink, &linkTarget, &hash, &f.Copied,
			&chunked, &f.ChunkCnt); err != nil {
			return fmt.Errorf("catalog: scanning file inventory row: %w", err)
		}
		if entryKind == "symlink" {
			s = CopySymlink{
				FileID: f.FileID, RelPath: f.RelPath, LinkTarget: linkTarget.String,
				Size: f.Size, Mode: f.Mode, UID: f.UID, GID: f.GID,
				MtimeNS: f.MtimeNS, Copied: f.Copied,
			}
			out.Symlinks = append(out.Symlinks, s)
			continue
		}
		f.Hash = hash
		f.Chunked = chunked != 0
		out.Files = append(out.Files, f)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("catalog: iterating file inventory of set %q image %d: %w", setID, imageID, err)
	}
	return nil
}

// copyChunks reads the image's chunk rows joined with the parent's
// metadata and whole-file hash (one query; the parent columns are what
// let the writer lstat-compare the source per 5.1 and the manifest
// builder anchor the 6.6 reassembly table without a second read).
func (db *DB) copyChunks(ctx context.Context, setID string, imageID int64, out *CopyInventory) error {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT c.chunk_id, c.file_id, c.chunk_index, c.offset, c.length, c.hash, c.copied,
		        f.relpath, f.size, f.mode, f.uid, f.gid, f.mtime_ns, f.dev, f.inode, f.hash,
		        f.chunk_count
		 FROM chunks c
		 JOIN files f ON f.file_id = c.file_id AND f.set_id = c.set_id
		 WHERE c.set_id = ? AND c.image_id = ? AND f.status = 'active' AND f.superseded_by IS NULL
		 ORDER BY c.file_id, c.chunk_index`, setID, imageID)
	if err != nil {
		return fmt.Errorf("catalog: reading chunk inventory of set %q image %d: %w", setID, imageID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only close; rows.Err dominates
	for rows.Next() {
		var (
			c          CopyChunk
			hash       []byte
			parentHash []byte
		)
		if err := rows.Scan(&c.ChunkID, &c.FileID, &c.ChunkIndex, &c.Offset, &c.Length,
			&hash, &c.Copied, &c.ParentRelPath, &c.ParentSize, &c.ParentMode, &c.ParentUID,
			&c.ParentGID, &c.ParentMtimeNS, &c.ParentDev, &c.ParentInode, &parentHash,
			&c.ParentNumChunks); err != nil {
			return fmt.Errorf("catalog: scanning chunk inventory row: %w", err)
		}
		c.Hash = hash
		c.ParentHash = parentHash
		out.Chunks = append(out.Chunks, c)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("catalog: iterating chunk inventory of set %q image %d: %w", setID, imageID, err)
	}
	return nil
}

// copyDirs reads the image's active empty directories (the durable
// FR-037 rows, active-only like everything else).
func (db *DB) copyDirs(ctx context.Context, setID string, imageID int64, out *CopyInventory) error {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT dir_id, relpath, mode, uid, gid, mtime_ns
		 FROM directories
		 WHERE set_id = ? AND image_id = ? AND status = 'active'
		 ORDER BY dir_id`, setID, imageID)
	if err != nil {
		return fmt.Errorf("catalog: reading dir inventory of set %q image %d: %w", setID, imageID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only close; rows.Err dominates
	for rows.Next() {
		var d CopyDir
		if err := rows.Scan(&d.DirID, &d.RelPath, &d.Mode, &d.UID, &d.GID, &d.MtimeNS); err != nil {
			return fmt.Errorf("catalog: scanning dir inventory row: %w", err)
		}
		out.Dirs = append(out.Dirs, d)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("catalog: iterating dir inventory of set %q image %d: %w", setID, imageID, err)
	}
	return nil
}

// copySecondaries appends this image's hard-link secondaries: rows of
// families whose PRIMARY (lowest file_id among active nlink>1 members
// sharing dev+inode) is assigned to THIS image. The derivation is the
// balancer's, replicated deliberately (see hardLinkRows for why it is
// replicated rather than imported); the families slice comes from the
// ONE per-inventory derivation CopyInventory performs, so this
// projection and excludeSecondaryWholeFileClaims always agree.
//
// NOTE (the honesty filter's mirror): a member row may ALSO carry its
// own files.image_id (the plan assigned it before the family formed,
// or a direct-catalog seeding shape). Its claim still lands HERE, on
// the primary's image - link(2) is physically created next to the
// primary (D-04), and the member's own image assignment is overridden
// by the derivation for every claim purpose (inventory, manifest,
// placement, content rows). SAME-IMAGE BOTH-ASSIGNED INCLUDED
// (verified, not assumed): the primary-image check below keys on the
// PRIMARY's assignment alone, so a member carrying the SAME image_id
// as its primary is emitted here exactly once - next to the primary on
// that very image - and the unified filter drops its whole-file claim
// from Files, leaving the image exactly ONE claim of the member: the
// link. The cross-image e2e (both orientations) and the both-assigned
// same-image e2e pin the whole chain.
func copySecondaries(imageID int64, families []linkFamily, out *CopyInventory) {
	for _, fam := range families {
		if fam.primary.imageID != imageID {
			continue
		}
		for _, m := range fam.members {
			if m.fileID == fam.primary.fileID {
				continue
			}
			out.Secondaries = append(out.Secondaries, CopySecondary{
				FileID: m.fileID, RelPath: m.relPath, Size: m.size, Mode: m.mode,
				UID: m.uid, GID: m.gid, MtimeNS: m.mtimeNS, Hash: m.hash, Copied: m.copied,
				PrimaryFileID: fam.primary.fileID, PrimaryRelPath: fam.primary.relPath,
			})
		}
	}
	sort.Slice(out.Secondaries, func(i, j int) bool {
		return out.Secondaries[i].FileID < out.Secondaries[j].FileID
	})
}

// --- the hard-link family derivation (shared by inventory, placement, manifest)

// linkMember is one active nlink>1 regular row of the derivation.
type linkMember struct {
	fileID  int64
	relPath string
	size    int64
	mode    uint32
	uid     uint32
	gid     uint32
	mtimeNS int64
	dev     int64
	inode   int64
	nlink   int64
	hash    []byte
	imageID int64 // 0 = unassigned (secondaries are always unassigned, 2.4)
	copied  bool
}

// linkFamily is one (dev, inode) family plus its primary.
type linkFamily struct {
	primary linkMember
	members []linkMember
}

// hardLinkRows reads every catalog-ACTIVE regular row with nlink > 1
// and a KNOWN inode (inode != 0 - a zero inode is an uncaptured
// sentinel, never a family) and derives the families.
//
// WHY REPLICATED: internal/plan's balancer derives the same families
// from the same predicate, but its helpers are deliberately
// unexported (plan exposes only the placement pipeline, and its
// ActiveFile shape is plan-local). Importing internal/plan from the
// catalog layer would invert the layering (1.2: catalog issues SQL,
// services consume it - not the other way round), and the alternative
// - the cli importing plan's internals - does not exist either. So the
// derivation is REPLICATED HERE, verbatim from the balancer's
// documented rule (balancer.go "Hard-link families: the link_group
// finding"): group active nlink>1 regular rows by (dev, inode), the
// LOWEST file_id is the primary. link_group stays unpopulated at scan
// time; when grouping ever lands in the column, BOTH derivations
// switch to it in one place each. The two sites are pinned to each
// other by the shared comment and by the plan e2e (secondaries are
// excluded from placement but written next to their primary).
func (db *DB) hardLinkRows(ctx context.Context, setID string) ([]linkFamily, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT file_id, relpath, size, mode, uid, gid, mtime_ns, dev, inode, nlink,
		        hash, COALESCE(image_id, 0), copied
		 FROM files
		 WHERE set_id = ? AND status = 'active' AND superseded_by IS NULL
		   AND entry_kind = 'regular' AND nlink > 1 AND inode != 0
		 ORDER BY file_id`, setID)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading hard-link rows of set %q: %w", setID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only close; rows.Err dominates
	type key struct{ dev, inode int64 }
	byKey := map[key]*linkFamily{}
	var order []key
	for rows.Next() {
		var m linkMember
		if err := rows.Scan(&m.fileID, &m.relPath, &m.size, &m.mode, &m.uid, &m.gid,
			&m.mtimeNS, &m.dev, &m.inode, &m.nlink, &m.hash, &m.imageID, &m.copied); err != nil {
			return nil, fmt.Errorf("catalog: scanning hard-link row: %w", err)
		}
		k := key{m.dev, m.inode}
		fam, ok := byKey[k]
		if !ok {
			fam = &linkFamily{primary: m}
			byKey[k] = fam
			order = append(order, k)
		} else if m.fileID < fam.primary.fileID {
			// Lowest file_id wins the primary role (the balancer's
			// rule: first-seen row, deterministic).
			fam.primary = m
		}
		fam.members = append(fam.members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: iterating hard-link rows of set %q: %w", setID, err)
	}
	// Deterministic family order (ascending primary file_id).
	var families []linkFamily
	for _, k := range order {
		families = append(families, *byKey[k])
	}
	sort.Slice(families, func(i, j int) bool { return families[i].primary.fileID < families[j].primary.fileID })
	return families, nil
}

// --- the whole-set reads the MANIFEST.json input needs (6.6) -----------------

// CopyImagePaths is one image's payload paths in the whole-set
// placement summary (FR-033 "which image number(s) hold which files";
// FR-035 - every image's manifest carries the whole summary).
type CopyImagePaths struct {
	ImageID int64
	Paths   []string // source relpaths: whole files, symlinks, chunked PARENTS, empty dirs, secondaries
}

// CopyPlacement reads the whole-set placement summary: every payload
// path per image, derived from the LOCKED assignment columns only
// (files.image_id, chunks.image_id - folded to the PARENT relpath,
// directories.image_id) plus the hard-link secondaries, which
// materialize next to their primary's image (D-04 copy mode). Every
// images row of the set appears (even one holding no payload), so the
// manifest gate "every image has a placement record" holds by
// construction.
//
// THE T3.1 MANIFEST-HONESTY FILTER (fix round, the mirror of
// CopyInventory's excludeSecondaryWholeFileClaims - same derivation,
// same unified rule, same truth source): a whole-file row that is a
// family SECONDARY whose PRIMARY is ASSIGNED (same image or
// different) is listed under the PRIMARY's image ONLY (via the
// secondaries loop below), never as an ordinary claim of its own
// assignment image. The assignment column alone could claim the path
// on an image whose tree never holds its bytes (the bytes exist as
// the link(2) on the primary's image) - the placement half of the
// hard-link claim lie the finding describes; in the same-image
// BOTH-ASSIGNED shape the map below dedupes anyway, but the filter
// still runs so the placement derivation is ONE rule everywhere.
func (db *DB) CopyPlacement(ctx context.Context, setID string) ([]CopyImagePaths, error) {
	ids, err := db.CopyImageIDs(ctx, setID)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]map[string]struct{}, len(ids))
	out := make([]CopyImagePaths, 0, len(ids))
	for _, id := range ids {
		byID[id] = map[string]struct{}{}
		out = append(out, CopyImagePaths{ImageID: id})
	}
	add := func(imageID int64, rel string) error {
		set, ok := byID[imageID]
		if !ok {
			// A payload row pointing at an image with no images row
			// would be catalog corruption; fail loudly rather than
			// emit a placement the manifest gate would choke on.
			return fmt.Errorf("catalog: placement of set %q references image %d, which has no images row",
				setID, imageID)
		}
		set[rel] = struct{}{}
		return nil
	}

	// THE HONESTY FILTER'S DERIVATION (read ONCE, consumed by both the
	// files loop below and the secondaries loop at the end): family
	// members whose PRIMARY is ASSIGNED are excluded from their OWN
	// image's claim - their path re-lands on the primary's image via
	// the secondaries loop (the unified secondary-claim rule; for the
	// same-image shape the two add the same path to the same set, so
	// the map dedupes, but the derivation stays ONE rule).
	families, err := db.hardLinkRows(ctx, setID)
	if err != nil {
		return nil, err
	}
	secondaryOnPrimary := make(map[int64]struct{})
	for _, fam := range families {
		for _, m := range fam.members {
			if m.fileID == fam.primary.fileID {
				continue
			}
			if fam.primary.imageID != 0 {
				secondaryOnPrimary[m.fileID] = struct{}{}
			}
		}
	}

	rows, err := db.sql.QueryContext(ctx,
		`SELECT file_id, image_id, relpath FROM files
		 WHERE set_id = ? AND image_id IS NOT NULL AND status = 'active' AND superseded_by IS NULL
		 ORDER BY file_id`, setID)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading file placement of set %q: %w", setID, err)
	}
	for rows.Next() {
		var fileID, imageID int64
		var rel string
		if err := rows.Scan(&fileID, &imageID, &rel); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("catalog: scanning file placement row: %w", err)
		}
		if _, excluded := secondaryOnPrimary[fileID]; excluded {
			continue // claimed by the primary's image below, never here
		}
		if err := add(imageID, rel); err != nil {
			_ = rows.Close()
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("catalog: iterating file placement of set %q: %w", setID, err)
	}
	_ = rows.Close()

	// Chunked parents appear under EVERY image holding any of their
	// chunks (6.6 ChunkedFile note; the manifest gate checks the
	// parent relpath, not the chunk path).
	rows, err = db.sql.QueryContext(ctx,
		`SELECT c.image_id, f.relpath
		 FROM chunks c JOIN files f ON f.file_id = c.file_id AND f.set_id = c.set_id
		 WHERE c.set_id = ? AND c.image_id IS NOT NULL AND f.status = 'active' AND f.superseded_by IS NULL
		 ORDER BY c.file_id, c.chunk_index`, setID)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading chunk placement of set %q: %w", setID, err)
	}
	for rows.Next() {
		var imageID int64
		var rel string
		if err := rows.Scan(&imageID, &rel); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("catalog: scanning chunk placement row: %w", err)
		}
		if err := add(imageID, rel); err != nil {
			_ = rows.Close()
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("catalog: iterating chunk placement of set %q: %w", setID, err)
	}
	_ = rows.Close()

	rows, err = db.sql.QueryContext(ctx,
		`SELECT image_id, relpath FROM directories
		 WHERE set_id = ? AND image_id IS NOT NULL AND status = 'active'
		 ORDER BY dir_id`, setID)
	if err != nil {
		return nil, fmt.Errorf("catalog: reading dir placement of set %q: %w", setID, err)
	}
	for rows.Next() {
		var imageID int64
		var rel string
		if err := rows.Scan(&imageID, &rel); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("catalog: scanning dir placement row: %w", err)
		}
		if err := add(imageID, rel); err != nil {
			_ = rows.Close()
			return nil, err
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("catalog: iterating dir placement of set %q: %w", setID, err)
	}
	_ = rows.Close()

	// Secondaries materialize on their primary's image (D-04 copy
	// mode), so the summary lists them there - the manifest's
	// one-directional gate (every entry's relpath in OWN placement)
	// needs exactly this. This is also where a secondary excluded from
	// its own assignment image above re-lands: the claim follows the
	// BYTES, which live as the link(2) next to the primary (the
	// same-image BOTH-ASSIGNED shape lands on the same image here - one
	// path, one set entry, one claim).
	for _, fam := range families {
		if fam.primary.imageID == 0 {
			continue // unassigned primary: no image holds the family
		}
		for _, m := range fam.members {
			if m.fileID == fam.primary.fileID {
				continue
			}
			if err := add(fam.primary.imageID, m.relPath); err != nil {
				return nil, err
			}
		}
	}

	// Sort each image's paths ascending and return (ascending image
	// ids from CopyImageIDs).
	for i := range out {
		set := byID[out[i].ImageID]
		paths := make([]string, 0, len(set))
		for rel := range set {
			paths = append(paths, rel)
		}
		sort.Strings(paths)
		out[i].Paths = paths
	}
	return out, nil
}

// CopyLinkFamily is one D-04 hard-link group for the manifest: the
// shared content hash and ALL of the family's names (ascending).
type CopyLinkFamily struct {
	Hash  []byte
	Names []string
}

// CopyHardLinkFamilies reads the whole set's hard-link families (6.6
// "hard-link groups (all names plus shared hash)"; D-04 - recorded
// regardless of write mode so a media-alone restore recreates the
// family from one stored copy). Families of ONE member are not
// families and are dropped (a scan raced by a link-count change can
// produce one; the manifest gate requires >= 2 names).
func (db *DB) CopyHardLinkFamilies(ctx context.Context, setID string) ([]CopyLinkFamily, error) {
	families, err := db.hardLinkRows(ctx, setID)
	if err != nil {
		return nil, err
	}
	out := make([]CopyLinkFamily, 0, len(families))
	for _, fam := range families {
		if len(fam.members) < 2 {
			continue
		}
		names := make([]string, 0, len(fam.members))
		for _, m := range fam.members {
			names = append(names, m.relPath)
		}
		sort.Strings(names)
		out = append(out, CopyLinkFamily{Hash: fam.primary.hash, Names: names})
	}
	return out, nil
}

// CopyImageIDs lists the set's planned image ids in ascending order
// (the `copy` command's iteration order; a light read - the write
// path needs the ids, not the fill figures Destinations computes).
func (db *DB) CopyImageIDs(ctx context.Context, setID string) ([]int64, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT image_id FROM images WHERE set_id = ? ORDER BY image_id`, setID)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing image ids of set %q: %w", setID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only close; rows.Err dominates
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("catalog: scanning image id of set %q: %w", setID, err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: iterating image ids of set %q: %w", setID, err)
	}
	return out, nil
}

// SetSourceRoot reads sets.source_root - the absolute path the files
// rows' relpaths resolve against (2.3). The `copy` command joins it
// with each relpath to lstat/open the source (5.1 step 1). An unknown
// set is a sentinel-wrapped error (ErrNoSuchSet) the cli maps to a
// usage input.
func (db *DB) SetSourceRoot(ctx context.Context, setID string) (string, error) {
	var root string
	err := db.sql.QueryRowContext(ctx,
		`SELECT source_root FROM sets WHERE set_id = ?`, setID).Scan(&root)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("catalog: no set %q in catalog: %w", setID, ErrNoSuchSet)
	case err != nil:
		return "", fmt.Errorf("catalog: reading source root of set %q: %w", setID, err)
	}
	return root, nil
}

// ErrNoSuchSet classifies the unknown-set lookup (the cli maps it to
// a usage-class input error, exit 2 - a typo'd --set-id is the user's
// mistake, not a catalog fault).
var ErrNoSuchSet = errors.New("catalog: no such set")

// --- runs bookkeeping for the copy command (2.4, 10.1) -----------------------

// CopyRun describes the runs row a copy command opens before writing
// (kind 'copy', allowed by the 2.3 CHECK). Root is the DESTINATION
// root (runs.root means "source root or destination root").
type CopyRun struct {
	SetID     string
	Root      string // absolute destination root the copy trees land under
	StartedAt int64  // unix nanoseconds, UTC (the caller's clock, T1.1 convention)
}

// StartCopyRun inserts the runs row in status 'running' and returns
// its run_id (2.4 provenance: failures and skips land in run_events
// under it, and the final status is stamped on every exit path below).
// An unknown set fails before the insert with ErrNoSuchSet rather than
// surfacing the raw FK error.
func (db *DB) StartCopyRun(ctx context.Context, run CopyRun) (int64, error) {
	if run.SetID == "" || run.Root == "" {
		return 0, fmt.Errorf("catalog: StartCopyRun requires a set id and a destination root")
	}
	var one int
	if err := db.sql.QueryRowContext(ctx,
		`SELECT 1 FROM sets WHERE set_id = ?`, run.SetID).Scan(&one); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("catalog: no set %q in catalog: %w", run.SetID, ErrNoSuchSet)
		}
		return 0, fmt.Errorf("catalog: checking set %q for the copy run: %w", run.SetID, err)
	}
	res, err := db.sql.ExecContext(ctx,
		`INSERT INTO runs (set_id, kind, root, started_at, finished_at, status,
		                  exclude_json, stats_json, tool_version)
		 VALUES (?, 'copy', ?, ?, NULL, 'running', '[]', '{}', ?)`,
		run.SetID, run.Root, run.StartedAt, ToolVersion)
	if err != nil {
		return 0, fmt.Errorf("catalog: starting copy run for set %q: %w", run.SetID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("catalog: copy run id after insert: %w", err)
	}
	return id, nil
}

// FinishRunDetached finalizes a run row with a context that is NOT the
// (possibly already-cancelled) run context - THE T0.5/T1.6
// INTERRUPTED-RUN DEFERRAL CLOSURE, insofar as the COPY command is
// concerned (the empirically-confirmed gap: FinishRun under a
// cancelled ctx is rejected by the SQLite driver, so an interrupted
// run used to stay 'running' forever).
//
// THE HONEST SOLUTION CHOSEN: a fresh context derived from
// context.Background() with a short (5s) timeout. context.Background()
// alone would be unbounded - a wedged database would hang the shutdown
// path forever; the bounded descendant keeps the post-cancel write
// tiny (one UPDATE) while guaranteeing the interruption record lands.
// The run row write is the LAST database act of the command, it holds
// no locks beyond the pool's own, and the exclusive flock (D-11)
// already excludes competing writers, so running it post-cancel is
// safe by construction.
//
// status must be one of the 2.3 CHECK values ('ok', 'partial',
// 'failed', 'interrupted'); FinishRun validates it again.
func (db *DB) FinishRunDetached(runID int64, status string, finishedAt int64, statsJSON string) error {
	ctx, cancel := context.WithTimeout(context.Background(), finalizeRunTimeout)
	defer cancel()
	if err := db.FinishRun(ctx, runID, status, finishedAt, statsJSON); err != nil {
		return fmt.Errorf("catalog: finalizing run %d detached (status %q): %w", runID, status, err)
	}
	return nil
}
