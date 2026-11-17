package catalog

// T5.1: the verify-media catalog surface (DESIGN_SPEC 1.2 layering -
// the catalog package is the ONLY component issuing SQL; 2.1 Option B
// read-only opens; 2.3 images/image_contents; 6.6 finalized content
// list; 7.2 verify-media; IMPLEMENTATION_PLAN T5.1).
//
// Three additions, nothing else:
//
//   - ImageMeta: the read-side view of one images row (state,
//     final_path, mode, timestamps) - the cli resolves each image's
//     location from it before verifying;
//   - ImageContentList: the read-side view of one image's finalized
//     6.6 content list, WITH the symlink targets resolved through the
//     files row (the image_contents row carries no target column) and
//     the EXPECTED hash anchored against files.hash / chunks.hash (7.2:
//     "compare against image_contents ... catalog hashes as the
//     anchor") - a disagreement between the mirrored content row and
//     its anchor is LOUD catalog corruption, never a silent pick;
//   - StampImageVerified: the 7.2 full-match stamp (verified_at +
//     state 'verified', ONE small transaction) - called only through
//     the verify service on a FULL match.
//
// SQL stays here; internal/verify never imports this package (its
// VerifyStore seam is primitive-shaped; the cli adapts *catalog.DB
// onto it - the copyengine/imagebuild Store precedent).

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ImageMeta is the read-side view of one images row (2.3). BuiltAt /
// VerifiedAt are 0 when the column is NULL.
type ImageMeta struct {
	ImageID    int64
	State      string // planned | building | built | verified | failed
	Mode       string // "" until a write records it (2.3: "set at first write")
	FinalPath  string // "" until a write records it ("copy dir or .iso path once built")
	BuiltAt    int64
	VerifiedAt int64
}

// ImageMeta reads ONE image's row. sql.ErrNoRows (wrapped) means the
// set has no such image.
func (db *DB) ImageMeta(ctx context.Context, setID string, imageID int64) (ImageMeta, error) {
	var (
		m                   ImageMeta
		builtAt, verifiedAt sql.NullInt64
		mode, finalPath     sql.NullString
		state               string
	)
	err := db.sql.QueryRowContext(ctx,
		`SELECT image_id, state, mode, final_path, built_at, verified_at
		 FROM images WHERE set_id = ? AND image_id = ?`, setID, imageID).
		Scan(&m.ImageID, &state, &mode, &finalPath, &builtAt, &verifiedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ImageMeta{}, fmt.Errorf("catalog: no images row for set %q image %d: %w", setID, imageID, err)
		}
		return ImageMeta{}, fmt.Errorf("catalog: reading images row for set %q image %d: %w", setID, imageID, err)
	}
	m.State = state
	m.Mode = mode.String
	m.FinalPath = finalPath.String
	m.BuiltAt = builtAt.Int64
	m.VerifiedAt = verifiedAt.Int64
	return m, nil
}

// ImageContentRow is one image_contents row as the verify-media read
// side consumes it.
type ImageContentRow struct {
	MediaPath  string
	Kind       string // file | chunk | symlink | dir | manifest | export
	FileID     int64  // 0 when the row carries no files link (dir/manifest/export)
	ChunkIndex int    // -1 when not a chunk
	Size       int64
	// Hash is the ANCHORED expected digest (see ImageContentList):
	// files.hash for file rows, chunks.hash for chunk rows, the
	// content row's own digest for the manifest/export self-rows;
	// nil for dirs and symlinks (the 2.3 NULL-hash convention).
	Hash []byte
	// LinkTarget is the recorded symlink target (files.link_target),
	// symlink rows only.
	LinkTarget string
}

// ImageContentList is one image's finalized content list, read-side.
type ImageContentList struct {
	Rows []ImageContentRow
	// SymlinkTargets maps media path -> recorded target for the
	// symlink rows (the shape verify.CompareContents consumes; the
	// target lives on the files row, not on image_contents).
	SymlinkTargets map[string]string
}

// ImageContentList reads one image's finalized 6.6 content list with
// the catalog hashes anchored (7.2):
//
//   - a 'file' row's expected hash is files.hash, a 'chunk' row's is
//     chunks.hash (the 2.3 source-of-truth columns; image_contents
//     MIRRORS them at write time);
//   - when both the mirror and the anchor are present and DISAGREE,
//     the read fails loudly naming the path - that is catalog
//     corruption, and verify-media must never silently pick a side;
//   - a hashed row with NEITHER mirror nor anchor is refused the same
//     way (a content list that cannot anchor a file cannot be
//     verified);
//   - symlink rows resolve their target through files.link_target; a
//     symlink row without a files link is refused (its target cannot
//     be verified, and an unverifiable member is not silently skipped).
func (db *DB) ImageContentList(ctx context.Context, setID string, imageID int64) (ImageContentList, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT ic.media_path, ic.kind,
		        COALESCE(ic.file_id, 0), COALESCE(ic.chunk_index, -1), ic.size,
		        ic.hash, f.hash, ch.hash,
		        CASE WHEN ic.kind = 'symlink' THEN f.link_target END
		 FROM image_contents ic
		 LEFT JOIN files f
		   ON f.set_id = ic.set_id AND f.file_id = ic.file_id
		 LEFT JOIN chunks ch
		   ON ch.set_id = ic.set_id AND ch.file_id = ic.file_id
		  AND ch.chunk_index = ic.chunk_index
		 WHERE ic.set_id = ? AND ic.image_id = ?
		 ORDER BY ic.media_path`, setID, imageID)
	if err != nil {
		return ImageContentList{}, fmt.Errorf("catalog: reading image_contents of set %q image %d: %w", setID, imageID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only close; rows.Err dominates

	out := ImageContentList{SymlinkTargets: map[string]string{}}
	for rows.Next() {
		var (
			r          ImageContentRow
			icHash     []byte
			fileHash   []byte
			chunkHash  []byte
			linkTarget sql.NullString
		)
		if err := rows.Scan(&r.MediaPath, &r.Kind, &r.FileID, &r.ChunkIndex, &r.Size,
			&icHash, &fileHash, &chunkHash, &linkTarget); err != nil {
			return ImageContentList{}, fmt.Errorf("catalog: scanning image_contents row of set %q image %d: %w", setID, imageID, err)
		}
		switch r.Kind {
		case "dir":
			// Existence-only member; no hash, no target.
		case "symlink":
			if r.FileID == 0 || !linkTarget.Valid {
				return ImageContentList{}, fmt.Errorf(
					"catalog: image_contents symlink row %q (set %q, image %d) has no files link, so its target cannot be verified",
					r.MediaPath, setID, imageID)
			}
			r.LinkTarget = linkTarget.String
			out.SymlinkTargets[r.MediaPath] = r.LinkTarget
		case "manifest", "export":
			// Self-rows: no files/chunks anchor; the content row's own
			// digest is the expected value.
			if len(icHash) != 32 {
				return ImageContentList{}, fmt.Errorf(
					"catalog: image_contents %s row %q (set %q, image %d) carries a %d-byte hash, want 32 (D-09)",
					r.Kind, r.MediaPath, setID, imageID, len(icHash))
			}
			r.Hash = icHash
		default: // file, chunk: anchored payload kinds
			anchor := fileHash
			if r.Kind == "chunk" {
				anchor = chunkHash
			}
			switch {
			case len(anchor) == 32 && len(icHash) == 32 && !bytes.Equal(anchor, icHash):
				return ImageContentList{}, fmt.Errorf(
					"catalog: image_contents row %q (set %q, image %d) mirrors hash %x but its %s anchor is %x (corrupt catalog)",
					r.MediaPath, setID, imageID, icHash, anchorKind(r.Kind), anchor)
			case len(anchor) == 32:
				r.Hash = anchor
			case len(icHash) == 32:
				// The mirror carries the digest (the writer read it
				// from the anchor row inside its own transaction);
				// the anchor row is gone or unhashed - use the mirror
				// rather than refusing a list the writers themselves
				// produced.
				r.Hash = icHash
			default:
				return ImageContentList{}, fmt.Errorf(
					"catalog: image_contents row %q (set %q, image %d) kind %q carries no verifiable hash (neither the row nor its %s anchor is a 32-byte sha256)",
					r.MediaPath, setID, imageID, r.Kind, anchorKind(r.Kind))
			}
		}
		out.Rows = append(out.Rows, r)
	}
	if err := rows.Err(); err != nil {
		return ImageContentList{}, fmt.Errorf("catalog: iterating image_contents of set %q image %d: %w", setID, imageID, err)
	}
	return out, nil
}

// anchorKind names the anchor column of a payload kind (messages).
func anchorKind(kind string) string {
	if kind == "chunk" {
		return "chunks.hash"
	}
	return "files.hash"
}

// StampImageVerified is the 7.2 full-match stamp for ONE image:
// images.verified_at + state 'verified', ONE small transaction. A
// zero-row update is a loud wiring error (no such image).
//
// The stamp applies from WHATEVER state the row carried (see the
// verify service's documented state policy): a build image reaches it
// from 'built', a copy image from 'built' too since T6.5
// (RecordCopyFinalPath stamps mode='copy' + state='built' at copy
// completion) - or from 'planned' for rows written before T6.5, which
// nothing rewrites - and a crash-window image from 'building' or
// 'failed' - which makes a full verify-media the 6.3 crash-window
// reconciliation for a valid .iso over a stale state row. built_at,
// final_path, and bytes_written are untouched; partial verifies never
// call this method.
func (db *DB) StampImageVerified(ctx context.Context, setID string, imageID int64, verifiedAt int64) error {
	if verifiedAt <= 0 {
		return fmt.Errorf("catalog: StampImageVerified (set %q, image %d): non-positive verified_at %d", setID, imageID, verifiedAt)
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("catalog: beginning the verify stamp transaction (set %q, image %d): %w", setID, imageID, err)
	}
	rollback := func(cause error) error {
		_ = tx.Rollback()
		return cause
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE images SET state = 'verified', verified_at = ? WHERE set_id = ? AND image_id = ?`,
		verifiedAt, setID, imageID)
	if err != nil {
		return rollback(fmt.Errorf("catalog: stamping image %d of set %q verified: %w", imageID, setID, err))
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return rollback(fmt.Errorf("catalog: StampImageVerified: no images row for set %q image %d", setID, imageID))
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("catalog: committing the verify stamp (set %q, image %d): %w", setID, imageID, err)
	}
	return nil
}
