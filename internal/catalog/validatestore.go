package catalog

// T5.2: the catalog read side of the `validate` command (DESIGN_SPEC
// 7.3, FR-022; IMPLEMENTATION_PLAN 4.6 T5.2). One read-only fetch of
// the catalog-ACTIVE files rows of a set, carrying exactly what the
// 7.3 comparison consumes:
//
//   - the CONTENT anchor: hash (regular rows, sha256, D-09) or
//     link_target (symlink rows, D-03) - the whole-file hash is the
//     validate anchor for chunked files too, so no chunk column
//     travels here;
//   - the resolved D-10 metadata the Mode-aware fsmeta.DiffFields
//     compares (size, mtime_ns, dev, inode) - the 7.3 gate that splits
//     `changed` (metadata differs AND hash differs) from `corrupt`
//     (metadata matches AND hash differs);
//   - relpath and entry_kind: the record's identity and comparison
//     shape (regular rows hash-compare, symlink rows compare their
//     target verbatim).
//
// WHY A THIRD ACTIVE-ROW READER EXISTS (the alternatives checked per
// T5.2): CopyInventory is image-scoped and carries placement/copied
// state validate never consults (7.3 compares source versus CATALOG
// CONTENT - unassigned and unplanned records are compared exactly like
// locked ones); ListActiveForPlan carries dev/inode/nlink but NOT
// mtime_ns, hash, or link_target, so it cannot drive either the
// metadata gate or the content anchor; ScanTx.ListActive is
// transaction-scoped (the mutating scan path) and its ActiveFile
// projection likewise lacks dev/inode. The fetch below is therefore
// the minimal new read: a *DB method usable in NO transaction and NO
// advisory lock, matching ListActiveForPlan's read-side shape.
//
// Read-only contract (DESIGN_SPEC 2.1 Option B, amended 2026-10-18):
// validate opens the catalog mode=ro with NO flock at all, so this
// query runs over a WAL snapshot during a copy/build run without
// blocking or being blocked. SQL stays in this package (DESIGN_SPEC
// 1.2).

import (
	"context"
	"fmt"
)

// ValidateRecord is one catalog-ACTIVE files row (the v_active_files
// predicate, spelled out: status='active' AND superseded_by IS NULL)
// as the 7.3 comparison consumes it. Assignment columns (image_id,
// copied) and chunk columns are deliberately absent - see the file doc.
type ValidateRecord struct {
	FileID    int64
	RelPath   string
	EntryKind string // 'regular' | 'symlink' (the 2.3 CHECK vocabulary)
	// Size is the recorded entry size: bytes for regular rows,
	// len(target) for symlink rows (the 2.3 convention). It feeds both
	// the DiffFields size field and validate's progress byte totals.
	Size int64
	// MtimeNS is the recorded modification time in Unix nanoseconds
	// (the DiffFields mtime_ns field).
	MtimeNS int64
	// Dev and Inode are the recorded D-10 identifiers. They are
	// NULL-able in the DDL (nothing in the scan path leaves them NULL
	// - fsmeta.Capture always fills them); the COALESCE keeps that
	// DDL nullability from failing the scan, reading a NULL as 0 -
	// which can only ever make a full-mode comparison see MORE drift,
	// never less, and both fields are masked entirely by a metadata
	// change-detection mode anyway.
	Dev   int64
	Inode int64
	// LinkTarget is the recorded symlink target (D-03); "" for regular
	// rows (NULL in the catalog).
	LinkTarget string
	// Hash is the recorded whole-file sha256 (D-09) for regular rows
	// (nil for symlink rows, which store no hash). It is the validate
	// anchor for chunked files as well: 7.3 compares content against
	// files.hash and never walks the chunk rows.
	Hash []byte
}

// ActiveRecords reads every catalog-ACTIVE files row of the set,
// ordered by file_id (the deterministic encounter order validate
// reports in). No rows is NOT an error: a set whose scan recorded
// nothing compares clean (every source entry simply reports `new`), so
// validate deliberately has no "no records" usage gate.
//
// The projection selects the two content anchors (hash, link_target)
// and the four DiffFields metadata columns (size, mtime_ns, dev,
// inode) exactly as DESIGN_SPEC 7.3 consumes them; see the file doc for
// why neither CopyInventory nor ListActiveForPlan fits.
func (db *DB) ActiveRecords(ctx context.Context, setID string) ([]ValidateRecord, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT file_id, relpath, entry_kind, size, mtime_ns,
		        COALESCE(dev, 0), COALESCE(inode, 0),
		        COALESCE(link_target, ''), hash
		 FROM files
		 WHERE set_id = ? AND status = 'active' AND superseded_by IS NULL
		 ORDER BY file_id`, setID)
	if err != nil {
		return nil, fmt.Errorf("catalog: listing active records for validate, set %q: %w", setID, err)
	}
	defer rows.Close() //nolint:errcheck // read-only close; rows.Err dominates
	var out []ValidateRecord
	for rows.Next() {
		var rec ValidateRecord
		if err := rows.Scan(&rec.FileID, &rec.RelPath, &rec.EntryKind, &rec.Size, &rec.MtimeNS,
			&rec.Dev, &rec.Inode, &rec.LinkTarget, &rec.Hash); err != nil {
			return nil, fmt.Errorf("catalog: scanning active validate row: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: iterating active validate rows: %w", err)
	}
	return out, nil
}
