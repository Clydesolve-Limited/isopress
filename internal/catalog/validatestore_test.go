package catalog

// T5.2: the validate read side (validatestore.go, DESIGN_SPEC 7.3,
// FR-022). These tests pin the projection AND the predicate against a
// REAL temp catalog: the four DiffFields metadata columns plus the two
// content anchors come back verbatim (regular rows carry hash, symlink
// rows carry link_target with a NULL hash), and only v_active_files
// rows (status='active' AND superseded_by IS NULL) are selected -
// missing, unreadable, and superseded rows never reach validate, so a
// stale version can never be compared or double-counted.

import (
	"bytes"
	"context"
	"testing"
)

func TestActiveRecordsProjectsValidateColumnsAndFiltersToActive(t *testing.T) {
	f := newScanFixture(t)
	ctx := context.Background()

	// One regular and one symlink row (the two record kinds), a
	// missing row, an unreadable row (Hash NULL - the D-09 shape), and
	// a modification pair whose OLD row is superseded.
	f.mustTx(t, func(tx *ScanTx) {
		if _, err := tx.InsertScanFile(ctx, regularRow("docs/a.txt", 11, 0xAB)); err != nil {
			t.Fatalf("InsertScanFile regular: %v", err)
		}
		sym := FileRow{
			RelPath: "links/l", EntryKind: "symlink", Size: 3,
			Mode: 0o777, UID: 1000, GID: 1000, MtimeNS: 99,
			Dev: 7, Inode: 55, Nlink: 1, LinkTarget: "../a", Status: "active",
		}
		if _, err := tx.InsertScanFile(ctx, sym); err != nil {
			t.Fatalf("InsertScanFile symlink: %v", err)
		}
		gone := regularRow("gone.txt", 4, 0x0C)
		gone.Status = "missing"
		if _, err := tx.InsertScanFile(ctx, gone); err != nil {
			t.Fatalf("InsertScanFile missing: %v", err)
		}
		bad := regularRow("bad.txt", 5, 0x0D)
		bad.Status = "unreadable"
		bad.Hash = nil
		if _, err := tx.InsertScanFile(ctx, bad); err != nil {
			t.Fatalf("InsertScanFile unreadable: %v", err)
		}
		// The versioned pair: replace docs/v.txt's first version, so
		// the old row gains superseded_by.
		oldID, err := tx.InsertScanFile(ctx, regularRow("docs/v.txt", 20, 0x0E))
		if err != nil {
			t.Fatalf("InsertScanFile v1: %v", err)
		}
		if _, err := tx.InsertModification(ctx, oldID, regularRow("docs/v.txt", 25, 0x0F)); err != nil {
			t.Fatalf("InsertModification: %v", err)
		}
	})

	recs, err := f.db.ActiveRecords(ctx, f.setID)
	if err != nil {
		t.Fatalf("ActiveRecords: %v", err)
	}
	// Exactly the active rows, in file_id order: docs/a.txt, links/l,
	// docs/v.txt (v2). Missing/unreadable/superseded never appear.
	wantOrder := []string{"docs/a.txt", "links/l", "docs/v.txt"}
	if len(recs) != len(wantOrder) {
		t.Fatalf("ActiveRecords returned %d rows (%s), want %d", len(recs), briefRels(recs), len(wantOrder))
	}
	for i, want := range wantOrder {
		if recs[i].RelPath != want {
			t.Errorf("row %d = %q, want %q (file_id order; got %s)", i, recs[i].RelPath, want, briefRels(recs))
		}
	}

	// The regular projection: hash anchor + the four DiffFields
	// metadata columns verbatim (regularRow's fixed values).
	regular := recs[0]
	if regular.EntryKind != "regular" || len(regular.Hash) != 32 || regular.Hash[0] != 0xAB {
		t.Errorf("regular row = %+v, want entry_kind regular with the 32-byte 0xAB hash", regular)
	}
	if regular.Size != 11 || regular.MtimeNS != 123456789 || regular.Dev != 42 || regular.Inode != 1001 {
		t.Errorf("regular metadata = size %d mtime %d dev %d inode %d, want 11/123456789/42/1001 (the DiffFields columns)",
			regular.Size, regular.MtimeNS, regular.Dev, regular.Inode)
	}
	if regular.LinkTarget != "" {
		t.Errorf("regular LinkTarget = %q, want empty", regular.LinkTarget)
	}

	// The symlink projection: target anchor, NULL hash, len(target)
	// size (schema 2.3 convention).
	link := recs[1]
	if link.EntryKind != "symlink" || link.LinkTarget != "../a" || link.Hash != nil || link.Size != 3 {
		t.Errorf("symlink row = %+v, want entry_kind symlink, target %q, nil hash, size 3", link, "../a")
	}

	// The surviving version only: the v2 row's id and hash (0x0F), not
	// the superseded v1 (0x0E).
	v2 := recs[2]
	if v2.Size != 25 || v2.Hash[0] != 0x0F {
		t.Errorf("version row = size %d hash %#v, want the v2 row (size 25, 0x0F hash)", v2.Size, v2.Hash)
	}
	if v2.FileID == 0 {
		t.Error("version row lost its file_id")
	}

	// An unknown set is an EMPTY result, not an error (validate gates
	// the set through SetSourceRoot first; zero records compares clean).
	none, err := f.db.ActiveRecords(ctx, "S-NOPE")
	if err != nil {
		t.Fatalf("ActiveRecords of an unknown set = %v, want empty, no error", err)
	}
	if len(none) != 0 {
		t.Errorf("ActiveRecords of an unknown set = %d rows, want 0", len(none))
	}
}

// briefRels renders record relpaths for failure messages.
func briefRels(recs []ValidateRecord) string {
	var b bytes.Buffer
	for i, r := range recs {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(r.RelPath)
	}
	return b.String()
}
