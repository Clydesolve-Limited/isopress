package catalog

// T5.5: the read helpers the media-alone restore (DESIGN_SPEC 8.5) needs
// over the temp copy of the on-media export - SetIDs and IntegrityCheck,
// plus MediaReadErr, the classifier that gives the media path's export
// reads their class. Per DESIGN_SPEC 1.2 the SQL lives here;
// internal/restore and the cli never issue any.
//
// Both are plain reads usable on a read-only handle in NO transaction
// and under NO advisory lock (2.1 Option B), exactly like the
// validate/verify-media read surface (the restore.Store reads this
// command already performs through OpenReadOnly).
//
// WHY A DEDICATED integrity_check HELPER: 8.5's chain for the export's
// own integrity rests on SQLite's internal integrity (the export cannot
// carry its own hash - it was produced before anything embedded it).
// verifyExportFile runs the same PRAGMA at export time, but only
// against the PRE-PUBLISH scratch; a restore re-runs it against the
// media's copy, because media ages on a shelf (FR-035) and the cheap
// check is the honest first gate before any row is read.
//
// WHY A MEDIA READ CLASSIFIER: the same reasoning applies to the reads
// that come AFTER the gate. A file can open as SQLite and pass PRAGMA
// integrity_check yet not be a catalog at all (a schemaless, foreign,
// or foreign-schema database - the T5.5 QA finding): the query then
// fails with a driver error that carries no class, and the command
// would report it as the generic fatal (exit 1). An EXPORT failure is
// the catalog class (10.1 CatalogError -> exit 4) exactly like the
// damaged-export integrity gate and the two-sets-row refusal, so
// MediaReadErr classifies every media-path read of the export copy -
// see its doc for the one exception (context cancellation).

import (
	"context"
	"errors"
	"fmt"
)

// IntegrityCheck runs PRAGMA integrity_check on the open database and
// returns nil only for the verdict "ok". A non-ok verdict (or a file so
// damaged the PRAGMA itself fails) is classified as a KindCorrupt
// CatalogError (10.1 CatalogError class, exit 4 in the cli): the file
// is a database-shaped object that no longer answers truthfully.
func (db *DB) IntegrityCheck(ctx context.Context) error {
	var verdict string
	if err := db.sql.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&verdict); err != nil {
		return &CatalogError{
			Kind: KindCorrupt,
			Path: db.path,
			Err:  fmt.Errorf("running PRAGMA integrity_check: %w", err),
		}
	}
	if verdict != "ok" {
		return &CatalogError{
			Kind: KindCorrupt,
			Path: db.path,
			Err:  fmt.Errorf("PRAGMA integrity_check reported %q, want ok", verdict),
		}
	}
	return nil
}

// SetIDs returns every set_id in the sets table, ascending. On the
// FR-034 set-scoped export of 8.5 this is EXACTLY ONE row - which is
// what lets a media-alone restore derive the set identity from the
// export alone (the export IS the authority; --set-id, when given, is a
// cross-check against it). A whole-catalog handle legitimately returns
// many rows; the caller decides what multiple means for its flow.
//
// A read that cannot be answered (a schemaless or foreign database that
// opened and passed the PRAGMA gate but has no sets table) is a
// MediaReadErr - the catalog class, exit 4 - never a bare driver error
// the command would report as a generic fatal.
func (db *DB) SetIDs(ctx context.Context) ([]string, error) {
	rows, err := db.sql.QueryContext(ctx, `SELECT set_id FROM sets ORDER BY set_id`)
	if err != nil {
		return nil, db.MediaReadErr("reading the set ids from the sets table", err)
	}
	defer rows.Close() //nolint:errcheck // read-only close; rows.Err dominates
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, db.MediaReadErr("scanning a set id", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, db.MediaReadErr("iterating the set ids", err)
	}
	return out, nil
}

// MediaReadErr classifies a failed READ of the temp-copied on-media export
// (DESIGN_SPEC 8.5's catalog swap, internal/cli/restore_media.go) as the
// catalog class's KindCorrupt CatalogError - the class the media path
// already uses for the export itself (the PRAGMA integrity gate, the
// two-sets-row refusal).
//
// THE CASE IT EXISTS FOR (T5.5 QA finding): a non-catalog or schemaless
// file named as the export opens fine (SQLite accepts it read-only, and a
// schemaless database passes PRAGMA integrity_check), so the run reaches a
// catalog query, which fails with a bare driver error ("no such table:
// sets"). Without this classification the command reports that as the
// generic fatal (exit 1) even though it is precisely an EXPORT failure -
// the media flow's documented intent is export failures -> catalog class
// (exit 4), which is what every other export refusal (missing, damaged,
// future-version, two sets rows) already returns.
//
// The classification is deliberately applied to READ failures, not to the
// context: an interrupted read (context.Canceled / DeadlineExceeded) is
// returned unchanged, so a cancelled run never masquerades as a corrupt
// export.
//
// THE SEAT MAP (where the media path applies it, so "every media-path
// read" is checkable): the open/identity reads - SetIDs, the
// openRestoreCatalog CopyImageIDs check, and runRestore's media-run
// CopyImageIDs re-read - and the SELECTION-PHASE reads reached through
// the mediaStore seat (internal/cli/restore_media.go: ActiveRecords,
// ActiveDirectories, Placement, ImageInventory). The plain --catalog
// path deliberately keeps its pre-existing class (the same conditional
// at each seat).
func (db *DB) MediaReadErr(op string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return &CatalogError{
		Kind: KindCorrupt,
		Path: db.path,
		Err:  fmt.Errorf("%s: %w", op, err),
	}
}
