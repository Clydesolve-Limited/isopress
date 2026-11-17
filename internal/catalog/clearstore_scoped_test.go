package catalog

// U.1 (DESIGN_SPEC 4.6 selective unlock): the SCOPED ClearPlan tests
// at the catalog-SQL tier (test plan Tier 1). They pin the
// nine-statement sequence, the predicate disjointness, the family
// claim rule, the non-active-row coverage (AC-16), the hash/stamp
// retention, the zero-row loud error, the unlocked_images audit array
// (M6), and - the critical AC-09 - a trigger-injected failure at EACH
// of the nine statements of the 4.6 transaction table rolling back to
// a bit-identical catalog (all tables) with NO runs row.
//
// The full-clear regression lives in clearstore_test.go /
// clearstore_gaps_test.go (unchanged predicates, nil imageIDs).

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// --- fixture -----------------------------------------------------------------

// The fixture row names, so tests can talk about rows without
// re-deriving ids. selected = the destination every scoped test
// unlocks.
const (
	scopedSelectedImage = int64(2)

	scopedParent   = "parent.dat"  // chunked, image_id NULL, copied=1, chunks {2,1}
	scopedFamPri   = "fam-a.txt"   // family primary (lowest file_id), image 2, copied 1
	scopedFamCross = "fam-b.txt"   // family member on a SIBLING image (cross-image shape)
	scopedFamNull  = "fam-c.txt"   // family member with image_id NULL (the D-04 secondary)
	scopedFamSup   = "fam-d.txt"   // SUPERSEDED family member (the M9 limitation row)
	scopedNegPri   = "neg-a.txt"   // family whose primary sits on image 1 (negative case)
	scopedNegMem   = "neg-b.txt"   // ... its member must stay copied=1
	scopedWhole2   = "whole2.txt"  // assigned to image 2 (step 3)
	scopedWhole1   = "whole1.txt"  // assigned to image 1 (untouched)
	scopedFree     = "free.txt"    // never assigned (untouched)
	scopedVerOld   = "ver-old.txt" // superseded lineage row carrying image 2 (AC-16)
	scopedVerNew   = "ver-new.txt" // the row that superseded it (untouched)
	scopedGone     = "gone.txt"    // missing-status row carrying image 2 (AC-16)
)

// newScopedClearFixture builds the DESIGN 4.6 scoped-unlock fixture:
// destinations 1 and 2 (2 = the selected image), a chunked parent
// whose chunks SPAN both destinations (the mirror-fix row), a hard-link
// family whose primary sits on image 2 with a NULL member, a
// cross-image member and a superseded member (the M9 limitation), a
// second family whose primary sits on image 1 (the predicate's
// negative), whole-file rows on both images, the AC-16 non-active
// rows, directories and image_contents rows for both destinations, and
// written+verified marks everywhere a completed copy run leaves them.
//
// The expected scoped-unlock counts for image 2: FilesCleared 7
// (1 mirror + 2 family + 4 blanket), ChunksCleared 1, DirsCleared 1,
// ImagesRemoved 1, ContentsCleared 2, ChunkHashesRetained 2.
func newScopedClearFixture(t *testing.T) (*scanFixture, map[int][]byte) {
	t.Helper()
	f := newScanFixture(t)
	ctx := context.Background()

	// --- files ----------------------------------------------------------------
	insert := func(rel string, size int64, dev, inode, nlink int64) int64 {
		t.Helper()
		var id int64
		f.mustTx(t, func(tx *ScanTx) {
			row := regularRow(rel, size, 0xAB)
			row.Dev, row.Inode, row.Nlink = dev, inode, nlink
			var err error
			if id, err = tx.InsertScanFile(ctx, row); err != nil {
				t.Fatalf("InsertScanFile(%q): %v", rel, err)
			}
		})
		return id
	}
	const famDev, famInode = int64(7), int64(701)
	const negDev, negInode = int64(8), int64(801)

	parent := insert(scopedParent, 4*gib, 42, 1001, 1)
	famPri := insert(scopedFamPri, 10, famDev, famInode, 2)
	famCross := insert(scopedFamCross, 10, famDev, famInode, 2)
	famNull := insert(scopedFamNull, 10, famDev, famInode, 2)
	famSup := insert(scopedFamSup, 10, famDev, famInode, 2)
	famSupNew := insert("fam-d2.txt", 10, famDev, famInode, 2) // the row that superseded fam-d
	negPri := insert(scopedNegPri, 10, negDev, negInode, 2)
	negMem := insert(scopedNegMem, 10, negDev, negInode, 2)
	whole2 := insert(scopedWhole2, 100, 42, 1002, 1)
	whole1 := insert(scopedWhole1, 100, 42, 1003, 1)
	insert(scopedFree, 100, 42, 1004, 1) // never assigned: the untouched control row
	verNew := insert(scopedVerNew, 100, 42, 1005, 1)
	verOld := insert(scopedVerOld, 100, 42, 1006, 1)
	gone := insert(scopedGone, 100, 42, 1007, 1)

	sdb := rawOpen(t, f.db.Path())
	stamp := func(query string, args ...any) {
		t.Helper()
		if _, err := sdb.Exec(query, args...); err != nil {
			t.Fatalf("fixture stamp %q: %v", query, err)
		}
	}
	// The non-active rows of AC-16: a superseded lineage row and a
	// missing-status row, each carrying image 2. fam-d is SUPERSEDED
	// (its active claim was taken over by fam-d2) while keeping its
	// historical copied mark - the M9 limitation row.
	stamp(`UPDATE files SET superseded_by = ? WHERE file_id = ?`, verNew, verOld)
	stamp(`UPDATE files SET status = 'missing' WHERE file_id = ?`, gone)
	stamp(`UPDATE files SET superseded_by = ? WHERE file_id = ?`, famSupNew, famSup)

	// --- destinations ---------------------------------------------------------
	planTxCommit(t, f, func(tx *PlanTx) {
		for _, id := range []int64{1, scopedSelectedImage} {
			if err := tx.InsertImage(ctx, ImageSpec{
				ImageID: id, NominalSize: 25_000_000_000, UsableSize: 23_000_000_000,
				MediaProfile: "bd-r-sl",
			}); err != nil {
				t.Fatalf("InsertImage(%d): %v", id, err)
			}
		}
	})

	// --- assignments ----------------------------------------------------------
	specs := []ChunkSpec{{Index: 0, Offset: 0, Length: 2 * gib}, {Index: 1, Offset: 2 * gib, Length: 2 * gib}}
	hashes := map[int][]byte{0: repeatByte(0x11), 1: repeatByte(0x22)}
	planTxCommit(t, f, func(tx *PlanTx) {
		// The mirror-fix parent: chunks span BOTH destinations (chunk 0
		// on the selected image, chunk 1 on the sibling).
		if err := tx.EnsureChunkRows(ctx, parent, specs); err != nil {
			t.Fatalf("EnsureChunkRows: %v", err)
		}
		if err := tx.StoreChunkSize(ctx, parent, 2*gib, len(specs)); err != nil {
			t.Fatalf("StoreChunkSize: %v", err)
		}
		if err := tx.AssignChunk(ctx, parent, 0, scopedSelectedImage); err != nil {
			t.Fatalf("AssignChunk(0, 2): %v", err)
		}
		if err := tx.AssignChunk(ctx, parent, 1, 1); err != nil {
			t.Fatalf("AssignChunk(1, 1): %v", err)
		}
		// The family with its primary on the selected image (S2's
		// target rows: the NULL member and the cross-image member).
		if err := tx.AssignFile(ctx, famPri, scopedSelectedImage); err != nil {
			t.Fatalf("AssignFile(fam-a): %v", err)
		}
		if err := tx.AssignFile(ctx, famCross, 1); err != nil {
			t.Fatalf("AssignFile(fam-b): %v", err)
		}
		// The negative family: primary on the SIBLING - its member must
		// keep its mark.
		if err := tx.AssignFile(ctx, negPri, 1); err != nil {
			t.Fatalf("AssignFile(neg-a): %v", err)
		}
		// Blanket step-3 rows and untouched siblings.
		if err := tx.AssignFile(ctx, whole2, scopedSelectedImage); err != nil {
			t.Fatalf("AssignFile(whole2): %v", err)
		}
		if err := tx.AssignFile(ctx, whole1, 1); err != nil {
			t.Fatalf("AssignFile(whole1): %v", err)
		}
		// AC-16: both non-active rows carry image 2.
		if err := tx.AssignFile(ctx, verOld, scopedSelectedImage); err != nil {
			t.Fatalf("AssignFile(ver-old): %v", err)
		}
		if err := tx.AssignFile(ctx, gone, scopedSelectedImage); err != nil {
			t.Fatalf("AssignFile(gone): %v", err)
		}
	})

	// --- written+verified marks (the state a completed copy run leaves) ------
	for _, p := range []struct {
		fileID int64
		idx    int
	}{{parent, 0}, {parent, 1}} {
		if _, err := sdb.Exec(`UPDATE chunks SET hash = ?, copied = 1, verified_at = 111
		                       WHERE file_id = ? AND chunk_index = ?`,
			hashes[p.idx], p.fileID, p.idx); err != nil {
			t.Fatalf("seeding chunk %d state: %v", p.idx, err)
		}
	}
	// The parent MIRROR is written (the 5.2 count-proof state): copied=1
	// while its image_id stays NULL (chunked files never carry one).
	stamp(`UPDATE files SET copied = 1 WHERE file_id = ?`, parent)
	// Every assigned whole file is written...
	stamp(`UPDATE files SET copied = 1 WHERE image_id IS NOT NULL`)
	// ... and the family claim marks (the NULL member is the D-04
	// secondary; fam-b is the cross-image member; neg-b belongs to the
	// sibling family).
	stamp(`UPDATE files SET copied = 1 WHERE file_id IN (?, ?, ?, ?)`,
		famPri, famCross, famNull, negMem)
	// The superseded family member keeps ITS mark too (M9: the family
	// claim's MEMBER side is active-only - the active-only member
	// guard - so the scoped unlock deliberately does not touch a
	// superseded MEMBER row; a superseded ANCHOR is the opposite
	// story: it still TRIGGERS the unmark of active members, the
	// historical-anchor rule - pinned by shape G below).
	stamp(`UPDATE files SET copied = 1 WHERE file_id = ?`, famSup)

	// --- directories ----------------------------------------------------------
	dir2 := insertEmptyDir(t, f, "empty-two")
	dir1 := insertEmptyDir(t, f, "empty-one")
	planTxCommit(t, f, func(tx *PlanTx) {
		if err := tx.AssignDir(ctx, dir2, scopedSelectedImage); err != nil {
			t.Fatalf("AssignDir(dir2): %v", err)
		}
		if err := tx.AssignDir(ctx, dir1, 1); err != nil {
			t.Fatalf("AssignDir(dir1): %v", err)
		}
	})

	// --- image_contents (both destinations) -----------------------------------
	for _, r := range []struct {
		image int64
		path  string
		kind  string
	}{
		{scopedSelectedImage, "files/whole2.txt", "file"},
		{scopedSelectedImage, "MANIFEST.json", "manifest"},
		{1, "files/whole1.txt", "file"},
		{1, "MANIFEST.json", "manifest"},
	} {
		if _, err := sdb.Exec(`INSERT INTO image_contents
		        (set_id, image_id, media_path, kind, size)
		        VALUES (?, ?, ?, ?, 10)`, f.setID, r.image, r.path, r.kind); err != nil {
			t.Fatalf("seeding image_contents %d/%s: %v", r.image, r.path, err)
		}
	}

	return f, hashes
}

func repeatByte(b byte) []byte {
	out := make([]byte, 32)
	for i := range out {
		out[i] = b
	}
	return out
}

// --- the all-tables snapshot --------------------------------------------------

// dumpScopedState reads EVERY table the unlock touches - files,
// chunks, directories, images, image_contents, runs - into one
// deterministic string (each line prefixed with its table name) for
// the bit-identical rollback comparisons (AC-09: "bit-identical
// catalog (all tables)").
func dumpScopedState(t *testing.T, db *DB) string {
	t.Helper()
	sdb := rawOpen(t, db.Path())
	var buf strings.Builder
	dump := func(query, table string) {
		t.Helper()
		rows, err := sdb.Query(query)
		if err != nil {
			t.Fatalf("dump %s: %v", table, err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns of %s: %v", table, err)
		}
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("scan %s row: %v", table, err)
			}
			buf.WriteString(table)
			for _, v := range vals {
				buf.WriteString("|")
				switch tv := v.(type) {
				case []byte:
					fmt.Fprintf(&buf, "%x", tv)
				case nil:
					buf.WriteString("NULL")
				default:
					buf.WriteString(strings.TrimSpace(toString(tv)))
				}
			}
			buf.WriteString("\n")
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate %s rows: %v", table, err)
		}
	}
	dump(`SELECT file_id, relpath, entry_kind, size, dev, inode, nlink, hash, status,
	             superseded_by, image_id, copied, chunked, chunk_count, chunk_size
	      FROM files ORDER BY file_id`, "files")
	dump(`SELECT chunk_id, file_id, chunk_index, offset, length, hash, image_id, copied, verified_at
	      FROM chunks ORDER BY chunk_id`, "chunks")
	dump(`SELECT dir_id, relpath, status, image_id FROM directories ORDER BY dir_id`, "directories")
	dump(`SELECT image_id, nominal_size, usable_size, media_profile, state
	      FROM images ORDER BY image_id`, "images")
	dump(`SELECT image_id, media_path, kind, size FROM image_contents
	      ORDER BY image_id, media_path`, "image_contents")
	dump(`SELECT run_id, set_id, kind, root, started_at, finished_at, status, stats_json
	      FROM runs ORDER BY run_id`, "runs")
	return buf.String()
}

// Column positions inside a dumpScopedState line's data fields (after
// the "table|" prefix), pinned here so the structural diff below reads
// the same columns the dump writes.
const (
	filesRelpathCol    = 1
	filesDevCol        = 4
	filesInodeCol      = 5
	filesNlinkCol      = 6
	filesKindCol       = 2
	filesStatusCol     = 8
	filesSupersededCol = 9
	filesImageIDCol    = 10
	filesCopiedCol     = 11
	chunksFileIDCol    = 1
	chunksImageIDCol   = 6
	dirsImageIDCol     = 3
	contentsImageIDCol = 0
	contentsPathCol    = 1
	runsKindCol        = 2
)

// parseScopedDump turns the dump into table -> pk -> data columns.
func parseScopedDump(dump string) map[string]map[string][]string {
	out := map[string]map[string][]string{}
	for _, line := range strings.Split(strings.TrimSuffix(dump, "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		table := parts[0]
		cols := parts[1:]
		var pk string
		switch table {
		case "files", "chunks", "directories":
			pk = cols[0]
		case "images":
			pk = cols[0]
		case "image_contents":
			pk = cols[contentsImageIDCol] + ":" + cols[contentsPathCol]
		case "runs":
			pk = cols[0]
		default:
			pk = strings.Join(cols, "|")
		}
		if out[table] == nil {
			out[table] = map[string][]string{}
		}
		out[table][pk] = cols
	}
	return out
}

func sameCols(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// assertScopedUnlockDiff is the structural SQL diff: every row NOT
// carrying the selected image's id must be byte-identical, EXCEPT the
// two documented files-side flips (the mirror parent and the active
// family members of a family that CLAIMED the selected image); rows
// that DID carry the id must be released exactly as the nine-step
// sequence specifies; the only other delta is the committed clear run
// row. Every expected change is also required to be PRESENT - a
// predicate that silently stops matching fails the same check.
func assertScopedUnlockDiff(t *testing.T, before, after string, selected int64) {
	t.Helper()
	b := parseScopedDump(before)
	a := parseScopedDump(after)

	// --- runs: prior rows byte-identical, exactly ONE added clear row --------
	if len(a["runs"]) != len(b["runs"])+1 {
		t.Errorf("runs rows = %d after, want %d (the one committed clear run)",
			len(a["runs"]), len(b["runs"])+1)
	}
	for pk, bcols := range b["runs"] {
		acols, ok := a["runs"][pk]
		if !ok || !sameCols(bcols, acols) {
			t.Errorf("prior run row %s changed across the unlock:\nbefore %v\nafter %v", pk, bcols, acols)
		}
	}
	addedClear := 0
	for pk, acols := range a["runs"] {
		if _, ok := b["runs"][pk]; ok {
			continue
		}
		if acols[runsKindCol] == "clear" {
			addedClear++
		} else {
			t.Errorf("unexpected added run row %s (kind %q): the unlock may write ONLY its own clear row",
				pk, acols[runsKindCol])
		}
	}
	if addedClear != 1 {
		t.Errorf("added clear run rows = %d, want exactly 1", addedClear)
	}

	// --- derive the permitted files-side flips from the BEFORE state ---------
	// (a) parents of chunks on the selected image (the mirror fix) ...
	mirrorParents := map[string]bool{}
	for _, c := range b["chunks"] {
		if c[chunksImageIDCol] == fmt.Sprintf("%d", selected) {
			mirrorParents[c[chunksFileIDCol]] = true
		}
	}
	// (b) ... and active members of a family that CLAIMED the selected
	// image (the family-claim unmark, step 2).
	//
	// THE DELIBERATE DIVERGENCE FROM hardLinkRows (2026-11-03 ruling):
	// the MEMBER side below mirrors the hardLinkRows /
	// hardLinkPrimaries derivation line-for-line (active,
	// superseded_by IS NULL, regular, nlink > 1, inode != 0), but the
	// CLAIM side deliberately does NOT - it accepts ANY status and has
	// NO MIN(file_id) anchor role: ONE row of the family carrying
	// image_id == selected claims the whole family. A future pass must
	// not "re-align" this scan with hardLinkRows; shape G's superseded
	// anchor flip is exactly what that widening buys.
	familyFlip := map[string]bool{}
	{
		claimedKey := map[string]bool{} // famKey(dev|inode) claimed by ANY-status row on the selected image
		for _, line := range strings.Split(strings.TrimSuffix(before, "\n"), "\n") {
			if !strings.HasPrefix(line, "files|") {
				continue
			}
			fc := strings.Split(line, "|")[1:]
			// The `a` row predicate of S2: regular, nlink > 1,
			// inode != 0, image_id == selected - ANY status (this
			// helper is single-set, so no set filter is needed; an
			// inode == 0 row can never be claimed, which is also what
			// keeps zero-inode members out of the flip set).
			if fc[filesKindCol] != "regular" || fc[filesNlinkCol] == "1" || fc[filesInodeCol] == "0" {
				continue
			}
			if fc[filesImageIDCol] == fmt.Sprintf("%d", selected) {
				claimedKey[fc[filesDevCol]+"|"+fc[filesInodeCol]] = true
			}
		}
		for _, line := range strings.Split(strings.TrimSuffix(before, "\n"), "\n") {
			if !strings.HasPrefix(line, "files|") {
				continue
			}
			fc := strings.Split(line, "|")[1:]
			// The `m` row predicate of S2: the active-only MEMBER
			// guard (M9) - the row S2 may UNMARK is always active.
			if fc[filesStatusCol] != "active" || fc[filesSupersededCol] != "NULL" ||
				fc[filesKindCol] != "regular" || fc[filesNlinkCol] == "1" {
				continue
			}
			if claimedKey[fc[filesDevCol]+"|"+fc[filesInodeCol]] {
				familyFlip[fc[0]] = true
			}
		}
	}

	selStr := fmt.Sprintf("%d", selected)
	// --- files ----------------------------------------------------------------
	for pk, bcols := range b["files"] {
		acols, ok := a["files"][pk]
		if !ok {
			t.Errorf("files row %s vanished - the unlock never deletes file records (FR-010)", pk)
			continue
		}
		if sameCols(bcols, acols) {
			continue
		}
		rel := bcols[filesRelpathCol]
		switch {
		case bcols[filesImageIDCol] == selStr:
			// Step 3: assignment released, mark dropped, EVERY other
			// column byte-identical (status, lineage, hash, stamp).
			if acols[filesImageIDCol] != "NULL" || acols[filesCopiedCol] != "0" {
				t.Errorf("selected files row %s (%s) after unlock = image %s copied %s, want NULL/0",
					pk, rel, acols[filesImageIDCol], acols[filesCopiedCol])
			}
			b2, a2 := append([]string{}, bcols...), append([]string{}, acols...)
			b2[filesImageIDCol], b2[filesCopiedCol] = "NULL", "0"
			if !sameCols(b2, a2) {
				t.Errorf("selected files row %s (%s) changed MORE than image_id+copied:\nbefore %v\nafter %v",
					pk, rel, bcols, acols)
			}
		case bcols[filesCopiedCol] == "1" && acols[filesCopiedCol] == "0" &&
			bcols[filesImageIDCol] == acols[filesImageIDCol] &&
			(mirrorParents[pk] || familyFlip[pk]):
			// The documented flip: the mirror parent (step 1) or an
			// active member of a family that claimed the selected
			// image (step 2) - ONLY copied moves 1 -> 0.
			b2 := append([]string{}, bcols...)
			b2[filesCopiedCol] = "0"
			if !sameCols(b2, acols) {
				t.Errorf("files flip row %s (%s) changed MORE than copied 1->0:\nbefore %v\nafter %v",
					pk, rel, bcols, acols)
			}
		default:
			t.Errorf("non-selected files row %s (%s) changed but carries no qualifying mark:\nbefore %v\nafter %v",
				pk, rel, bcols, acols)
		}
	}
	// The flips must be PRESENT, not merely tolerated: the mirror
	// parent and every qualifying family member actually moved.
	for pk := range mirrorParents {
		bcols, acols := b["files"][pk], a["files"][pk]
		if bcols != nil && bcols[filesCopiedCol] == "1" && (acols == nil || acols[filesCopiedCol] != "0") {
			t.Errorf("mirror parent %s kept copied after unlock - step 1 must clear it", pk)
		}
	}
	for pk := range familyFlip {
		bcols, acols := b["files"][pk], a["files"][pk]
		if bcols == nil {
			continue
		}
		onSelected := bcols[filesImageIDCol] == selStr
		wantFlip := bcols[filesCopiedCol] == "1" && !onSelected && bcols[filesImageIDCol] != selStr
		if wantFlip && (acols == nil || acols[filesCopiedCol] != "0") {
			t.Errorf("family member %s kept copied after unlock - step 2 must clear it", pk)
		}
	}

	// --- chunks ---------------------------------------------------------------
	for pk, bcols := range b["chunks"] {
		acols, ok := a["chunks"][pk]
		if !ok {
			t.Errorf("chunk row %s vanished - the unlock never deletes chunk rows (FR-010)", pk)
			continue
		}
		if sameCols(bcols, acols) {
			continue
		}
		if bcols[chunksImageIDCol] != selStr {
			t.Errorf("non-selected chunk %s changed:\nbefore %v\nafter %v", pk, bcols, acols)
			continue
		}
		if acols[chunksImageIDCol] != "NULL" || acols[7] != "0" || acols[8] != "NULL" {
			t.Errorf("selected chunk %s after unlock = image %s copied %s verified %s, want NULL/0/NULL",
				pk, acols[chunksImageIDCol], acols[7], acols[8])
		}
		b2 := append([]string{}, bcols...)
		b2[chunksImageIDCol], b2[7], b2[8] = "NULL", "0", "NULL"
		if !sameCols(b2, acols) {
			t.Errorf("selected chunk %s changed MORE than placement:\nbefore %v\nafter %v", pk, bcols, acols)
		}
	}

	// --- directories ----------------------------------------------------------
	for pk, bcols := range b["directories"] {
		acols, ok := a["directories"][pk]
		if !ok {
			t.Errorf("directory row %s vanished - the unlock never deletes directory rows", pk)
			continue
		}
		if sameCols(bcols, acols) {
			continue
		}
		if bcols[dirsImageIDCol] != selStr {
			t.Errorf("non-selected directory %s changed:\nbefore %v\nafter %v", pk, bcols, acols)
			continue
		}
		if acols[dirsImageIDCol] != "NULL" {
			t.Errorf("selected directory %s after unlock = image %s, want NULL", pk, acols[dirsImageIDCol])
		}
		b2 := append([]string{}, bcols...)
		b2[dirsImageIDCol] = "NULL"
		if !sameCols(b2, acols) {
			t.Errorf("selected directory %s changed MORE than image_id:\nbefore %v\nafter %v", pk, bcols, acols)
		}
	}

	// --- images ---------------------------------------------------------------
	for pk, bcols := range b["images"] {
		acols, ok := a["images"][pk]
		if pk == selStr {
			if ok {
				t.Errorf("selected images row %s survived the unlock: %v", pk, acols)
			}
			continue
		}
		if !ok || !sameCols(bcols, acols) {
			t.Errorf("sibling images row %s changed or vanished:\nbefore %v\nafter %v", pk, bcols, acols)
		}
	}

	// --- image_contents -------------------------------------------------------
	for pk, bcols := range b["image_contents"] {
		acols, ok := a["image_contents"][pk]
		if bcols[contentsImageIDCol] == selStr {
			if ok {
				t.Errorf("selected image_contents row %s survived: %v", pk, acols)
			}
			continue
		}
		if !ok || !sameCols(bcols, acols) {
			t.Errorf("sibling image_contents row %s changed or vanished:\nbefore %v\nafter %v", pk, bcols, acols)
		}
	}

	// --- no additions anywhere except the clear run --------------------------
	for table, rows := range a {
		if table == "runs" {
			continue
		}
		for pk := range rows {
			if _, ok := b[table][pk]; !ok {
				t.Errorf("%s row %s appeared across the unlock - the unlock only removes/edits", table, pk)
			}
		}
	}
}

// countClearRuns counts the committed 'clear' runs rows of the
// catalog (the AC-09 "NO runs row" assertion).
func countClearRuns(t *testing.T, db *DB) int {
	t.Helper()
	sdb := rawOpen(t, db.Path())
	var n int
	if err := sdb.QueryRow(`SELECT COUNT(*) FROM runs WHERE kind = 'clear'`).Scan(&n); err != nil {
		t.Fatalf("count clear runs: %v", err)
	}
	return n
}

// --- the success path ---------------------------------------------------------

// TestClearPlanScopedUnlockSequenceAndCounts pins the committed scoped
// unlock end to end at the SQL tier: the exact per-statement counts of
// the nine-step sequence (FilesCleared = 1 mirror + 2 family + 4
// blanket = 7 distinct rows), the structural all-tables diff (every
// non-selected row byte-identical except the documented flips), the
// retained hashes/stamp, the SCOPED image_contents delete, and the
// runs row whose stats_json carries the counts AND `unlocked_images`
// (AC-15, M6).
func TestClearPlanScopedUnlockSequenceAndCounts(t *testing.T) {
	f, hashes := newScopedClearFixture(t)
	ctx := context.Background()

	before := dumpScopedState(t, f.db)
	res, err := f.db.ClearPlan(ctx, f.setID, []int64{scopedSelectedImage}, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClearPlan([2]): %v", err)
	}
	// The nine-step counts: 1 mirror parent + 2 family members + 4
	// blanket rows; 1 chunk on image 2; 1 dir; 1 destination; 2 content
	// rows.
	if res.FilesCleared != 7 || res.ChunksCleared != 1 || res.DirsCleared != 1 ||
		res.ImagesRemoved != 1 || res.ContentsCleared != 2 ||
		res.ChunkHashesRetained != 2 || res.NothingToClear || res.RunID == 0 {
		t.Fatalf("ClearPlan([2]) result = %+v, want files 7, chunks 1, dirs 1, images 1, contents 2, retained 2, run id set", res)
	}
	if len(res.UnlockedImages) != 1 || res.UnlockedImages[0] != scopedSelectedImage {
		t.Errorf("UnlockedImages = %v, want [2] (the selected ids, sorted)", res.UnlockedImages)
	}

	// THE STRUCTURAL DIFF: the whole catalog changed in exactly the
	// documented ways (and every expected flip is present).
	after := dumpScopedState(t, f.db)
	if before == after {
		t.Fatal("the scoped unlock changed nothing - the fixture or the predicates are broken")
	}
	assertScopedUnlockDiff(t, before, after, scopedSelectedImage)

	sdb := rawOpen(t, f.db.Path())
	// Step 3: BOTH non-active rows released (AC-16) and counted - the
	// blanket any-status rule of step 3.
	for _, rel := range []string{scopedGone, scopedVerOld} {
		var imageID any // NULL expected
		var copied int
		if err := sdb.QueryRow(`SELECT image_id, copied FROM files WHERE set_id = ? AND relpath = ?`,
			f.setID, rel).Scan(&imageID, &copied); err != nil {
			t.Fatalf("read %s after unlock: %v", rel, err)
		}
		if imageID != nil || copied != 0 {
			t.Errorf("%s after unlock = image_id %v copied %d, want NULL/0", rel, imageID, copied)
		}
	}
	// The superseded row's LINEAGE survives (clear changes placement,
	// never lineage - the FR-010 philosophy).
	var supBy any
	if err := sdb.QueryRow(`SELECT superseded_by FROM files WHERE relpath = ?`, scopedVerOld).
		Scan(&supBy); err != nil {
		t.Fatalf("read ver-old lineage: %v", err)
	}
	if supBy == nil {
		t.Errorf("ver-old superseded_by after unlock = NULL, want the lineage intact")
	}
	// The M9 limitation row keeps its mark (the family claim's MEMBER
	// side is active-only - the active-only member guard) - asserted
	// explicitly alongside the structural diff.
	var supMemCopied int
	if err := sdb.QueryRow(`SELECT copied FROM files WHERE relpath = ?`, scopedFamSup).Scan(&supMemCopied); err != nil {
		t.Fatalf("read fam-d: %v", err)
	}
	if supMemCopied != 1 {
		t.Errorf("superseded member copied after unlock = %d, want 1 (M9: the active-only member guard keeps it)", supMemCopied)
	}

	// Step 9: the runs row's stats_json carries the counts AND the M6
	// unlocked_images array (cross-pinned with the CLI document by
	// AC-15/AC-40).
	var stats string
	if err := sdb.QueryRow(`SELECT stats_json FROM runs WHERE run_id = ?`, res.RunID).Scan(&stats); err != nil {
		t.Fatalf("read clear run stats: %v", err)
	}
	var cs clearStats
	if err := json.Unmarshal([]byte(stats), &cs); err != nil {
		t.Fatalf("stats_json %q: %v", stats, err)
	}
	if cs.FilesCleared != 7 || cs.ChunksCleared != 1 || cs.DirsCleared != 1 ||
		cs.ImagesRemoved != 1 || cs.ContentsCleared != 2 || cs.ChunkHashesRetained != 2 {
		t.Errorf("stats_json = %+v, want the committed counts 7/1/1/1/2/2", cs)
	}
	if len(cs.UnlockedImages) != 1 || cs.UnlockedImages[0] != scopedSelectedImage {
		t.Errorf("stats_json unlocked_images = %v, want [2] (M6)", cs.UnlockedImages)
	}
	// The retained hashes are byte-identical after the unlock (FR-010
	// at the audit level too).
	rows := rawChunkRows(t, f.db, mustFileID(t, f, scopedParent))
	for i, want := range hashes {
		if i >= len(rows) || string(rows[i].Hash) != string(want) {
			t.Errorf("chunk %d hash after unlock = missing/different, want the byte-identical stored %x", i, want)
		}
	}
}

// mustFileID reads one fixture row's file_id by relpath.
func mustFileID(t *testing.T, f *scanFixture, relpath string) int64 {
	t.Helper()
	sdb := rawOpen(t, f.db.Path())
	var id int64
	if err := sdb.QueryRow(`SELECT file_id FROM files WHERE set_id = ? AND relpath = ?`,
		f.setID, relpath).Scan(&id); err != nil {
		t.Fatalf("file_id of %q: %v", relpath, err)
	}
	return id
}

// --- AC-09: trigger-injected failure at EACH of the nine statements ------------

// TestClearPlanTriggerFailureAtEachOfTheNineStatements runs the AC-09
// injection matrix: for every statement of the 4.6 transaction table
// a trigger-injected failure must roll the transaction back to a
// BIT-IDENTICAL catalog (all six tables) with NO runs row, and a clean
// re-run after dropping the trigger must fully succeed (no latent
// damage).
//
// Judgment call, stated openly: statement 8 of the table ("full clear
// only: all counts zero -> ROLLBACK") is a Go-level branch, not an SQL
// statement - SQLite cannot fire a trigger on it. Its contract (ROLLBACK
// -> bit-identical, no runs row) is asserted through the no-op leg at
// the end, which executes exactly that branch. Statements 1-7 and 9
// take real BEFORE triggers; each trigger's WHEN clause names a row
// shape only the target statement can touch, so the injection point is
// precise.
func TestClearPlanTriggerFailureAtEachOfTheNineStatements(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		stmt string // the 4.6 table row
		trig string // trigger name
		ddl  string // full CREATE TRIGGER
	}{
		{
			stmt: "1 mirror fix",
			trig: "inj_s1",
			ddl: `CREATE TRIGGER inj_s1 BEFORE UPDATE ON files
				WHEN OLD.relpath = 'parent.dat' AND OLD.copied = 1
				BEGIN SELECT RAISE(ABORT, 'inj-statement-1'); END`,
		},
		{
			stmt: "2 family-claim unmark",
			trig: "inj_s2",
			ddl: `CREATE TRIGGER inj_s2 BEFORE UPDATE ON files
				WHEN OLD.relpath = 'fam-c.txt' AND OLD.copied = 1
				BEGIN SELECT RAISE(ABORT, 'inj-statement-2'); END`,
		},
		{
			stmt: "3 files unassign",
			trig: "inj_s3",
			ddl: `CREATE TRIGGER inj_s3 BEFORE UPDATE ON files
				WHEN OLD.relpath = 'whole2.txt'
				BEGIN SELECT RAISE(ABORT, 'inj-statement-3'); END`,
		},
		{
			stmt: "4 chunks",
			trig: "inj_s4",
			ddl: `CREATE TRIGGER inj_s4 BEFORE UPDATE ON chunks
				WHEN OLD.image_id = 2
				BEGIN SELECT RAISE(ABORT, 'inj-statement-4'); END`,
		},
		{
			stmt: "5 directories",
			trig: "inj_s5",
			ddl: `CREATE TRIGGER inj_s5 BEFORE UPDATE ON directories
				WHEN OLD.image_id = 2
				BEGIN SELECT RAISE(ABORT, 'inj-statement-5'); END`,
		},
		{
			stmt: "6 DELETE images",
			trig: "inj_s6",
			ddl: `CREATE TRIGGER inj_s6 BEFORE DELETE ON images
				WHEN OLD.image_id = 2
				BEGIN SELECT RAISE(ABORT, 'inj-statement-6'); END`,
		},
		{
			stmt: "7 DELETE image_contents",
			trig: "inj_s7",
			ddl: `CREATE TRIGGER inj_s7 BEFORE DELETE ON image_contents
				WHEN OLD.image_id = 2
				BEGIN SELECT RAISE(ABORT, 'inj-statement-7'); END`,
		},
		{
			stmt: "9 runs row",
			trig: "inj_s9",
			ddl: `CREATE TRIGGER inj_s9 BEFORE INSERT ON runs
				WHEN NEW.kind = 'clear'
				BEGIN SELECT RAISE(ABORT, 'inj-statement-9'); END`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.stmt, func(t *testing.T) {
			f, _ := newScopedClearFixture(t)
			before := dumpScopedState(t, f.db)
			sdb := rawOpen(t, f.db.Path())
			if _, err := sdb.Exec(tc.ddl); err != nil {
				t.Fatalf("install %s trigger: %v", tc.stmt, err)
			}

			_, err := f.db.ClearPlan(ctx, f.setID, []int64{scopedSelectedImage}, time.Now().UnixNano())
			marker := "inj-statement-" + tc.trig[len(tc.trig)-1:]
			if err == nil || !strings.Contains(err.Error(), marker) {
				t.Fatalf("ClearPlan under the %s trigger = %v, want the injected failure %q", tc.stmt, err, marker)
			}
			if after := dumpScopedState(t, f.db); after != before {
				t.Errorf("%s injection changed the catalog (atomicity violation):\nbefore:\n%s\nafter:\n%s",
					tc.stmt, before, after)
			}
			if n := countClearRuns(t, f.db); n != 0 {
				t.Errorf("%s injection left %d 'clear' run rows; want 0 (the insert rolls back with everything else)",
					tc.stmt, n)
			}

			// Recovery: drop the trigger, the same unlock fully succeeds
			// with the fixture's exact counts (no latent damage).
			if _, err := sdb.Exec(`DROP TRIGGER ` + tc.trig); err != nil {
				t.Fatalf("drop %s trigger: %v", tc.stmt, err)
			}
			res, err := f.db.ClearPlan(ctx, f.setID, []int64{scopedSelectedImage}, time.Now().UnixNano())
			if err != nil {
				t.Fatalf("recovery ClearPlan after %s: %v", tc.stmt, err)
			}
			if res.FilesCleared != 7 || res.ChunksCleared != 1 || res.ImagesRemoved != 1 {
				t.Errorf("recovery result after %s = %+v, want the exact fixture counts", tc.stmt, res)
			}
		})
	}

	// Statement 8's leg: the full-clear zero-counts ROLLBACK (the T2.2
	// no-op shape) - a fresh, never-planned set clears nothing, rolls
	// back bit-identical, and writes NO runs row. This is the table's
	// step-8 contract; it has no SQL to trigger against (see the func
	// doc), and the scoped path cannot reach it (step 6's exact-count
	// error subsumes it - pinned by TestClearPlanZeroRowImagesDeleteIsLoud).
	t.Run("8 full-clear zero-counts rollback", func(t *testing.T) {
		f := newScanFixture(t)
		insertPlanFile(t, f, "a.txt", 10) // scanned, NEVER planned
		before := dumpScopedState(t, f.db)
		res, err := f.db.ClearPlan(ctx, f.setID, nil, time.Now().UnixNano())
		if err != nil {
			t.Fatalf("full ClearPlan no-op: %v", err)
		}
		if !res.NothingToClear || res.RunID != 0 || res.FilesCleared != 0 {
			t.Errorf("no-op result = %+v, want NothingToClear with zero counts", res)
		}
		if len(res.UnlockedImages) != 0 {
			t.Errorf("no-op UnlockedImages = %v, want an empty (non-nil) array", res.UnlockedImages)
		}
		if after := dumpScopedState(t, f.db); after != before {
			t.Errorf("the step-8 rollback changed the catalog:\nbefore:\n%s\nafter:\n%s", before, after)
		}
		if n := countClearRuns(t, f.db); n != 0 {
			t.Errorf("no-op left %d 'clear' run rows; want 0", n)
		}
	})
}

// --- predicate separation, disjointness, and the family rule -------------------

// TestClearPlanScopedVsFullPredicateSeparation pins the two modes on
// ONE fixture: the scoped unlock leaves every non-selected row (marks
// included) byte-identical, while the full clear afterwards sweeps the
// survivors with the set-wide blanket predicates - the scoped
// predicates are strictly narrower, the full ones unchanged.
func TestClearPlanScopedVsFullPredicateSeparation(t *testing.T) {
	f, _ := newScopedClearFixture(t)
	ctx := context.Background()

	// Leg 1: scoped unlock of image 2.
	if _, err := f.db.ClearPlan(ctx, f.setID, []int64{scopedSelectedImage}, time.Now().UnixNano()); err != nil {
		t.Fatalf("scoped ClearPlan: %v", err)
	}
	sdb := rawOpen(t, f.db.Path())
	// The sibling's marks SURVIVE the scoped unlock (the full clear's
	// blanket would have swept them).
	var siblingCopied, siblingImage int
	if err := sdb.QueryRow(`SELECT copied, image_id FROM files WHERE relpath = ?`, scopedWhole1).
		Scan(&siblingCopied, &siblingImage); err != nil {
		t.Fatalf("read whole1: %v", err)
	}
	if siblingCopied != 1 || siblingImage != 1 {
		t.Errorf("whole1 after scoped unlock = copied %d image %d, want 1/1 (non-selected predicates never touch it)",
			siblingCopied, siblingImage)
	}
	var siblingChunks int
	if err := sdb.QueryRow(`SELECT COUNT(*) FROM chunks WHERE set_id = ? AND image_id = 1`, f.setID).
		Scan(&siblingChunks); err != nil {
		t.Fatalf("count sibling chunks: %v", err)
	}
	if siblingChunks != 1 {
		t.Errorf("sibling chunks after scoped unlock = %d, want 1 (placements untouched)", siblingChunks)
	}

	// Leg 2: the full clear sweeps EVERYTHING (the pre-4.6 blanket).
	res, err := f.db.ClearPlan(ctx, f.setID, nil, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("full ClearPlan: %v", err)
	}
	if res.FilesCleared == 0 {
		t.Fatalf("full clear after a scoped unlock swept nothing: %+v", res)
	}
	var residual, residualChunks, residualDirs, images int
	if err := sdb.QueryRow(`SELECT COUNT(*) FROM files
	                       WHERE set_id = ? AND (image_id IS NOT NULL OR copied != 0)`, f.setID).
		Scan(&residual); err != nil {
		t.Fatalf("count residual state: %v", err)
	}
	if err := sdb.QueryRow(`SELECT COUNT(*) FROM chunks
	                       WHERE set_id = ? AND (image_id IS NOT NULL OR copied != 0 OR verified_at IS NOT NULL)`,
		f.setID).Scan(&residualChunks); err != nil {
		t.Fatalf("count residual chunks: %v", err)
	}
	if err := sdb.QueryRow(`SELECT COUNT(*) FROM directories WHERE set_id = ? AND image_id IS NOT NULL`,
		f.setID).Scan(&residualDirs); err != nil {
		t.Fatalf("count residual dirs: %v", err)
	}
	if err := sdb.QueryRow(`SELECT COUNT(*) FROM images WHERE set_id = ?`, f.setID).Scan(&images); err != nil {
		t.Fatalf("count images: %v", err)
	}
	if residual != 0 || residualChunks != 0 || residualDirs != 0 || images != 0 {
		t.Errorf("after the full clear: files %d, chunks %d, dirs %d, images %d; want 0/0/0/0",
			residual, residualChunks, residualDirs, images)
	}
	// The full clear's audit array is the ids THIS run removed: image
	// 2 went with the scoped leg, so only image 1 remains.
	if len(res.UnlockedImages) != 1 || res.UnlockedImages[0] != 1 {
		t.Errorf("full UnlockedImages after the scoped leg = %v, want [1] (the ids THIS run removed)", res.UnlockedImages)
	}
}

// TestClearPlanScopedFilesClearedIsExactDistinctRows is the
// predicate-disjointness mutation pin: FilesCleared must equal the
// number of DISTINCT files rows whose state changed (a before/after
// row diff). A double-count (two statements matching one row) or a
// missed row breaks the equality.
func TestClearPlanScopedFilesClearedIsExactDistinctRows(t *testing.T) {
	f, _ := newScopedClearFixture(t)
	ctx := context.Background()

	sdb := rawOpen(t, f.db.Path())
	var filesCleared int
	changed := changedFileIDs(t, sdb, f.setID, func() {
		res, err := f.db.ClearPlan(ctx, f.setID, []int64{scopedSelectedImage}, time.Now().UnixNano())
		if err != nil {
			t.Fatalf("ClearPlan([2]): %v", err)
		}
		filesCleared = res.FilesCleared
	})
	if filesCleared != len(changed) {
		t.Errorf("FilesCleared = %d, distinct changed rows = %d (the sum-of-RowsAffected exactness proof)",
			filesCleared, len(changed))
	}
	// The exact expected set: 1 mirror parent + 2 family members +
	// 4 blanket rows.
	want := map[string]bool{
		scopedParent:  true,                       // step 1 (mirror)
		scopedFamNull: true, scopedFamCross: true, // step 2 (family)
		scopedWhole2: true, scopedFamPri: true, scopedGone: true, scopedVerOld: true, // step 3
	}
	if len(changed) != len(want) {
		t.Fatalf("changed files rows = %v, want exactly the 7 documented rows", sortedKeys(changed))
	}
	for rel := range want {
		if !changed[rel] {
			t.Errorf("expected row %q was not cleared (changed: %v)", rel, sortedKeys(changed))
		}
	}
	// No OTHER row changed: the untouched siblings prove the predicates
	// never widened.
	for _, rel := range []string{scopedWhole1, scopedFree, scopedVerNew, scopedNegPri, scopedNegMem, scopedFamSup} {
		if changed[rel] {
			t.Errorf("non-selected row %q changed - the predicates are not disjoint from the selection", rel)
		}
	}
}

// changedFileIDs snapshots (image_id, copied) per relpath, runs fn,
// and returns the relpaths whose state changed.
func changedFileIDs(t *testing.T, sdb *sql.DB, setID string, fn func()) map[string]bool {
	t.Helper()
	read := func() map[string]string {
		rows, err := sdb.Query(`SELECT relpath, COALESCE(image_id, -1), copied FROM files WHERE set_id = ? ORDER BY relpath`, setID)
		if err != nil {
			t.Fatalf("snapshot files: %v", err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var rel string
			var image, copied int64
			if err := rows.Scan(&rel, &image, &copied); err != nil {
				t.Fatalf("scan files snapshot: %v", err)
			}
			out[rel] = fmt.Sprintf("%d/%d", image, copied)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate files snapshot: %v", err)
		}
		return out
	}
	before := read()
	fn()
	after := read()
	changed := map[string]bool{}
	for rel, b := range before {
		if after[rel] != b {
			changed[rel] = true
		}
	}
	return changed
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestClearPlanFamilyClaimPredicate is the focused family-rule unit:
// against a seeded family incl. the CROSS-IMAGE member shape, the
// family-claim subquery unmarks exactly the active members of families
// that CLAIM the selected image - the MEMBER side mirrors hardLinkRows
// / hardLinkPrimaries line-for-line, while the CLAIM side takes ANY
// status and has NO MIN(file_id) anchor role (the historical-anchor
// rule) - and nothing else (sibling families, superseded MEMBER rows,
// the primary itself).
func TestClearPlanFamilyClaimPredicate(t *testing.T) {
	f, _ := newScopedClearFixture(t)
	ctx := context.Background()
	if _, err := f.db.ClearPlan(ctx, f.setID, []int64{scopedSelectedImage}, time.Now().UnixNano()); err != nil {
		t.Fatalf("ClearPlan([2]): %v", err)
	}
	sdb := rawOpen(t, f.db.Path())

	copiedOf := func(rel string) int {
		t.Helper()
		var c int
		if err := sdb.QueryRow(`SELECT copied FROM files WHERE set_id = ? AND relpath = ?`,
			f.setID, rel).Scan(&c); err != nil {
			t.Fatalf("copied of %q: %v", rel, err)
		}
		return c
	}
	imageOf := func(rel string) any {
		t.Helper()
		var v any
		if err := sdb.QueryRow(`SELECT image_id FROM files WHERE set_id = ? AND relpath = ?`,
			f.setID, rel).Scan(&v); err != nil {
			t.Fatalf("image_id of %q: %v", rel, err)
		}
		return v
	}

	// Selected family (primary on image 2): the primary is released by
	// step 3 (blanket), the members unmarked by step 2.
	if img := imageOf(scopedFamPri); img != nil || copiedOf(scopedFamPri) != 0 {
		t.Errorf("primary after unlock = image %v copied %d, want NULL/0 (step 3 owns it)",
			img, copiedOf(scopedFamPri))
	}
	if copiedOf(scopedFamNull) != 0 {
		t.Errorf("NULL member copied = %d, want 0 (step 2)", copiedOf(scopedFamNull))
	}
	if img := imageOf(scopedFamCross); img == nil || img.(int64) != 1 || copiedOf(scopedFamCross) != 0 {
		t.Errorf("cross-image member = image %v copied %d, want image 1 kept + copied 0 (step 2, NOT IN selected)",
			img, copiedOf(scopedFamCross))
	}
	// Negative family (primary on image 1): member untouched.
	if copiedOf(scopedNegMem) != 1 {
		t.Errorf("sibling-family member copied = %d, want 1 (primary not selected)", copiedOf(scopedNegMem))
	}
	// M9: the superseded MEMBER keeps its mark (the active-only
	// member guard - the row S2 may unmark is always active).
	if copiedOf(scopedFamSup) != 1 {
		t.Errorf("superseded member copied = %d, want 1 (M9 limitation)", copiedOf(scopedFamSup))
	}
	// The chunk-parent keeps image_id NULL and was owned by step 1.
	if img := imageOf(scopedParent); img != nil {
		t.Errorf("chunk-parent image_id = %v, want NULL", img)
	}
	if copiedOf(scopedParent) != 0 {
		t.Errorf("chunk-parent copied = %d, want 0 (step 1 mirror fix)", copiedOf(scopedParent))
	}
}

// --- AC-16: non-active row clearing -------------------------------------------

// TestClearPlanClearsNonActiveRowsOnSelectedImage is AC-16 at the SQL
// tier: a SUPERSEDED row and a MISSING-status row each carrying the
// selected image_id are both cleared (image_id NULL, copied 0) and BOTH
// counted in FilesCleared - step 3 is blanket, any status.
func TestClearPlanClearsNonActiveRowsOnSelectedImage(t *testing.T) {
	f, _ := newScopedClearFixture(t)
	ctx := context.Background()

	// Fixture sanity: both rows really carry image 2 pre-unlock.
	sdb := rawOpen(t, f.db.Path())
	for _, rel := range []string{scopedVerOld, scopedGone} {
		var imageID any
		if err := sdb.QueryRow(`SELECT image_id FROM files WHERE set_id = ? AND relpath = ?`,
			f.setID, rel).Scan(&imageID); err != nil {
			t.Fatalf("pre-unlock %s: %v", rel, err)
		}
		if imageID == nil || imageID.(int64) != scopedSelectedImage {
			t.Fatalf("fixture %s image_id = %v, want 2", rel, imageID)
		}
	}

	res, err := f.db.ClearPlan(ctx, f.setID, []int64{scopedSelectedImage}, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClearPlan([2]): %v", err)
	}
	// Both non-active rows are part of the blanket step-3 count (the
	// fixture's total is 7; without them it would be 5).
	if res.FilesCleared != 7 {
		t.Errorf("FilesCleared = %d, want 7 (the two non-active rows counted in the blanket step 3)", res.FilesCleared)
	}
	for _, rel := range []string{scopedVerOld, scopedGone} {
		var imageID any
		var copied int
		if err := sdb.QueryRow(`SELECT image_id, copied FROM files WHERE set_id = ? AND relpath = ?`,
			f.setID, rel).Scan(&imageID, &copied); err != nil {
			t.Fatalf("post-unlock %s: %v", rel, err)
		}
		if imageID != nil || copied != 0 {
			t.Errorf("%s after unlock = image %v copied %d, want NULL/0", rel, imageID, copied)
		}
	}
	// The missing row's status and record hash survive (FR-010: mark,
	// never delete).
	var status string
	var hash []byte
	if err := sdb.QueryRow(`SELECT status, hash FROM files WHERE set_id = ? AND relpath = ?`,
		f.setID, scopedGone).Scan(&status, &hash); err != nil {
		t.Fatalf("read gone.txt: %v", err)
	}
	if status != "missing" {
		t.Errorf("gone.txt status after unlock = %q, want 'missing' (clear never rewrites lineage status)", status)
	}
	if len(hash) != 32 {
		t.Errorf("gone.txt hash after unlock = %d bytes, want the 32-byte record hash retained", len(hash))
	}
}

// --- the loud zero-row error ---------------------------------------------------

// TestClearPlanZeroRowImagesDeleteIsLoud pins DESIGN 4.6 step 6 at the
// catalog tier: unlocking an id with NO destination row (already
// unlocked / never planned) is a LOUD error with a full rollback -
// bit-identical catalog, no runs row - never a silent no-op. The
// exact-count rule (not just the zero-row floor) also rejects a MIXED
// selection with one missing id, so `unlocked_images` can never
// over-claim.
func TestClearPlanZeroRowImagesDeleteIsLoud(t *testing.T) {
	f, _ := newScopedClearFixture(t)
	ctx := context.Background()
	before := dumpScopedState(t, f.db)

	_, err := f.db.ClearPlan(ctx, f.setID, []int64{99}, time.Now().UnixNano())
	if err == nil {
		t.Fatal("ClearPlan([99]) on a set without destination 99 must fail loudly")
	}
	if !strings.Contains(err.Error(), "99") || !strings.Contains(err.Error(), "loud error") {
		t.Errorf("zero-row error = %v, want the message naming the id and the loud-error rule", err)
	}
	if after := dumpScopedState(t, f.db); after != before {
		t.Errorf("the refused unlock changed the catalog:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if n := countClearRuns(t, f.db); n != 0 {
		t.Errorf("refused unlock left %d 'clear' run rows; want 0", n)
	}
	// A MIXED selection with one missing id fails the same way.
	if _, err := f.db.ClearPlan(ctx, f.setID, []int64{1, 99}, time.Now().UnixNano()); err == nil {
		t.Fatal("ClearPlan([1 99]) must fail: id 99 has no destination row")
	}
	if after := dumpScopedState(t, f.db); after != before {
		t.Errorf("the mixed refused unlock changed the catalog:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	// The empty non-nil selection is a caller error (nil means full -
	// promoting an empty selection to a full clear would be
	// catastrophic).
	if _, err := f.db.ClearPlan(ctx, f.setID, []int64{}, time.Now().UnixNano()); err == nil ||
		!strings.Contains(err.Error(), "empty image selection") {
		t.Errorf("ClearPlan([]) = %v, want the empty-selection refusal", err)
	}
	if after := dumpScopedState(t, f.db); after != before {
		t.Errorf("the empty-selection refusal changed the catalog:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// --- hash retention and selection normalization (Tier-3 units) -----------------

// TestClearPlanScopedRetainsHashesAndStamp: chunk hashes are byte-
// unchanged after a scoped unlock (the 4.2 reuse budget survives the
// selective form exactly like the full clear's FR-010 contract), and
// the parent's chunk stamp is untouched.
func TestClearPlanScopedRetainsHashesAndStamp(t *testing.T) {
	f, hashes := newScopedClearFixture(t)
	ctx := context.Background()

	res, err := f.db.ClearPlan(ctx, f.setID, []int64{scopedSelectedImage}, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClearPlan([2]): %v", err)
	}
	if res.ChunkHashesRetained != 2 {
		t.Fatalf("ChunkHashesRetained = %d, want 2 (set-wide retention)", res.ChunkHashesRetained)
	}
	sdb := rawOpen(t, f.db.Path())
	rows := rawChunkRows(t, f.db, mustFileID(t, f, scopedParent))
	if len(rows) != 2 {
		t.Fatalf("chunk rows after unlock = %d, want 2 RETAINED", len(rows))
	}
	for i, want := range hashes {
		if string(rows[i].Hash) != string(want) {
			t.Errorf("chunk %d hash after unlock = %x, want the byte-identical stored %x", i, rows[i].Hash, want)
		}
		if rows[i].Length != 2*gib {
			t.Errorf("chunk %d length = %d, want %d (rows verbatim)", i, rows[i].Length, 2*gib)
		}
	}
	var chunked, chunkCount int
	var chunkSize int64
	if err := sdb.QueryRow(`SELECT chunked, chunk_count, chunk_size FROM files WHERE relpath = ?`,
		scopedParent).Scan(&chunked, &chunkCount, &chunkSize); err != nil {
		t.Fatalf("read stamp: %v", err)
	}
	if chunked != 1 || chunkCount != 2 || chunkSize != 2*gib {
		t.Errorf("stamp after unlock = %d/%d/%d, want 1/2/%d", chunked, chunkCount, chunkSize, 2*gib)
	}
}

// --- AC-09 completion: the step-9 COMMIT failure path ---------------------------

// TestClearPlanCommitFailureRollsBackLoudly injects a failure at the
// ONE point SQLite triggers cannot reach directly: the COMMIT of step
// 9. SQLite has no COMMIT trigger, so the injection is a schema-level
// DEFERRABLE INITIALLY DEFERRED foreign key armed by a BEFORE INSERT
// trigger on runs (statement 9's insert): the insert itself SUCCEEDS
// (the violation is deferred to commit time), the COMMIT then fails
// with the deferred-constraint error, and ClearPlan's commit-failure
// branch must roll the WHOLE transaction back - bit-identical catalog
// (all six dump tables), NO runs row - exactly like the
// statement-level injections of the matrix above; a clean re-run after
// dropping the injection must fully succeed with the fixture's exact
// counts (no latent damage).
//
// This closes the last uncovered failure point of the 4.6 table:
// statements 1-7 take real triggers, statement 8 is the Go branch
// pinned by the no-op leg, statement 9's INSERT takes inj_s9 - and
// this test pins statement 9's COMMIT itself.
func TestClearPlanCommitFailureRollsBackLoudly(t *testing.T) {
	ctx := context.Background()
	f, _ := newScopedClearFixture(t)
	before := dumpScopedState(t, f.db)
	sdb := rawOpen(t, f.db.Path())
	for _, q := range []string{
		`CREATE TABLE inj_commit_parent (x INTEGER PRIMARY KEY)`,
		`CREATE TABLE inj_commit_child (x INTEGER REFERENCES inj_commit_parent(x) DEFERRABLE INITIALLY DEFERRED)`,
		`CREATE TRIGGER inj_commit BEFORE INSERT ON runs WHEN NEW.kind = 'clear'
		 BEGIN INSERT INTO inj_commit_child VALUES (999); END`,
	} {
		if _, err := sdb.Exec(q); err != nil {
			t.Fatalf("install COMMIT-failure injection (%s): %v", q, err)
		}
	}

	_, err := f.db.ClearPlan(ctx, f.setID, []int64{scopedSelectedImage}, time.Now().UnixNano())
	if err == nil || !strings.Contains(err.Error(), "committing clear transaction") {
		t.Fatalf("ClearPlan under the COMMIT injection = %v, want the commit-failure error", err)
	}
	if after := dumpScopedState(t, f.db); after != before {
		t.Errorf("the failed COMMIT changed the catalog (atomicity violation):\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if n := countClearRuns(t, f.db); n != 0 {
		t.Errorf("failed COMMIT left %d 'clear' run rows; want 0 (the insert rolls back with everything else)", n)
	}

	// Recovery: drop the injection, the same unlock fully succeeds.
	for _, q := range []string{
		`DROP TRIGGER inj_commit`,
		`DROP TABLE inj_commit_child`,
		`DROP TABLE inj_commit_parent`,
	} {
		if _, err := sdb.Exec(q); err != nil {
			t.Fatalf("drop COMMIT injection (%s): %v", q, err)
		}
	}
	res, err := f.db.ClearPlan(ctx, f.setID, []int64{scopedSelectedImage}, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("recovery ClearPlan after the COMMIT injection: %v", err)
	}
	if res.FilesCleared != 7 || res.ChunksCleared != 1 || res.ImagesRemoved != 1 || res.RunID == 0 {
		t.Errorf("recovery result = %+v, want the exact fixture counts 7/1/1 with a run id", res)
	}
}

// --- predicate-disjointness guards against the overlap shapes ------------------

// TestClearPlanPredicateDisjointnessGuardsOnOverlapShapes seeds the
// three overlap shapes the three files-side guards exist to keep
// pairwise disjoint, then pins FilesCleared == DISTINCT changed rows
// (the sum-of-RowsAffected exactness proof) plus each shape's exact
// end state and the structural all-tables scope diff:
//
//   - shape A (S1's `image_id IS NULL` guard): a row carrying the
//     SELECTED image_id AND chunks on the selected image. Unreachable
//     by construction - the planner never assigns files.image_id to a
//     chunked parent (4.5) - seeded as defense-in-depth: without the
//     guard S1 and S3 would BOTH count this row.
//   - shape B (S2's NOT-a-chunk-parent guard): a chunked hard-link
//     MEMBER whose family primary sits on the selected image - the
//     REACHABLE overlap (a big hardlinked file chunked onto the same
//     image). S1 owns it with the identical copied=0 effect; without
//     S2's guard S2 would count it a second time.
//   - shape C (S1's `copied != 0` guard): a chunk parent on the
//     selected image whose mirror mark is ALREADY 0. SQLite counts a
//     matched value-preserving UPDATE (RowsAffected counts matches,
//     not value changes), so without the guard FilesCleared would
//     exceed the distinct changed rows.
func TestClearPlanPredicateDisjointnessGuardsOnOverlapShapes(t *testing.T) {
	f, _ := newScopedClearFixture(t)
	ctx := context.Background()
	sdb := rawOpen(t, f.db.Path())

	insert := func(rel string, dev, inode, nlink int64) int64 {
		t.Helper()
		var id int64
		f.mustTx(t, func(tx *ScanTx) {
			row := regularRow(rel, 64, 0xAB)
			row.Dev, row.Inode, row.Nlink = dev, inode, nlink
			var err error
			if id, err = tx.InsertScanFile(ctx, row); err != nil {
				t.Fatalf("InsertScanFile(%q): %v", rel, err)
			}
		})
		return id
	}
	stamp := func(query string, args ...any) {
		t.Helper()
		if _, err := sdb.Exec(query, args...); err != nil {
			t.Fatalf("fixture stamp %q: %v", query, err)
		}
	}

	// Shape A: selected id + chunks on the selected image.
	chunkCarrier := insert("chunkcarrier.dat", 42, 2001, 1)
	// Shape B: linkprim.txt is inserted FIRST so it is the family's
	// MIN(file_id) primary; it lands on the selected image, its
	// chunked sibling carries image_id NULL (the D-04 shape).
	linkPrim := insert("linkprim.txt", 9, 901, 2)
	linkBig := insert("linkbig.dat", 9, 901, 2)
	// Shape C: a chunk parent whose mirror mark is already 0.
	zeroMirror := insert("zeromirror.dat", 42, 2002, 1)

	one := []ChunkSpec{{Index: 0, Offset: 0, Length: 64}}
	planTxCommit(t, f, func(tx *PlanTx) {
		if err := tx.AssignFile(ctx, chunkCarrier, scopedSelectedImage); err != nil {
			t.Fatalf("AssignFile(chunkcarrier): %v", err)
		}
		if err := tx.AssignFile(ctx, linkPrim, scopedSelectedImage); err != nil {
			t.Fatalf("AssignFile(linkprim): %v", err)
		}
		for _, id := range []int64{chunkCarrier, linkBig, zeroMirror} {
			if err := tx.EnsureChunkRows(ctx, id, one); err != nil {
				t.Fatalf("EnsureChunkRows(%d): %v", id, err)
			}
			if err := tx.AssignChunk(ctx, id, 0, scopedSelectedImage); err != nil {
				t.Fatalf("AssignChunk(%d, 0): %v", id, err)
			}
		}
	})
	// Written-state marks; zeroMirror deliberately keeps copied = 0.
	stamp(`UPDATE files SET copied = 1 WHERE file_id IN (?, ?, ?)`, chunkCarrier, linkPrim, linkBig)

	before := dumpScopedState(t, f.db)
	var res ClearResult
	changed := changedFileIDs(t, sdb, f.setID, func() {
		var err error
		res, err = f.db.ClearPlan(ctx, f.setID, []int64{scopedSelectedImage}, time.Now().UnixNano())
		if err != nil {
			t.Fatalf("ClearPlan([2]): %v", err)
		}
	})

	// THE EXACTNESS PIN: the sum of the three files-side
	// RowsAffected equals the DISTINCT changed rows - every guard
	// held, no row double-counted or missed.
	if res.FilesCleared != len(changed) {
		t.Errorf("FilesCleared = %d, distinct changed rows = %d (a guard no longer keeps the predicates disjoint)",
			res.FilesCleared, len(changed))
	}
	// The exact expected set: the fixture's 7 + shapes A and B; shape
	// C must NOT change (its mirror mark is already 0).
	want := map[string]bool{
		scopedParent: true, scopedFamNull: true, scopedFamCross: true,
		scopedWhole2: true, scopedFamPri: true, scopedGone: true, scopedVerOld: true,
		"chunkcarrier.dat": true, "linkprim.txt": true, "linkbig.dat": true,
	}
	if len(changed) != len(want) {
		t.Fatalf("changed files rows = %v, want exactly the 10 documented rows", sortedKeys(changed))
	}
	for rel := range want {
		if !changed[rel] {
			t.Errorf("expected row %q was not cleared (changed: %v)", rel, sortedKeys(changed))
		}
	}
	if changed["zeromirror.dat"] {
		t.Errorf("shape C (already-0 mirror) changed: %v - S1's copied != 0 guard must keep it out of the count", sortedKeys(changed))
	}
	// The other counts stay the fixture's (shape A/B chunks add to the
	// scoped chunk sweep: fixture 1 + three seeded parents on image 2).
	if res.DirsCleared != 1 || res.ImagesRemoved != 1 || res.ContentsCleared != 2 {
		t.Errorf("clear result = %+v, want dirs 1 / images 1 / contents 2 (fixture counts)", res)
	}
	if res.ChunksCleared != 4 {
		t.Errorf("ChunksCleared = %d, want 4 (fixture 1 + the three seeded chunk parents on image 2)", res.ChunksCleared)
	}

	// The structural all-tables diff: siblings byte-identical, every
	// documented flip present (scope pin alongside the count pin).
	after := dumpScopedState(t, f.db)
	if before == after {
		t.Fatal("the unlock changed nothing - the fixture or the predicates are broken")
	}
	assertScopedUnlockDiff(t, before, after, scopedSelectedImage)

	// Per-shape end states.
	type state struct {
		image  any
		copied int
	}
	read := func(rel string) state {
		t.Helper()
		var st state
		if err := sdb.QueryRow(`SELECT image_id, copied FROM files WHERE set_id = ? AND relpath = ?`,
			f.setID, rel).Scan(&st.image, &st.copied); err != nil {
			t.Fatalf("read %s after unlock: %v", rel, err)
		}
		return st
	}
	// A: S3 owns it (blanket any-status) - assignment released, mark
	// dropped even though it also carried chunks.
	if st := read("chunkcarrier.dat"); st.image != nil || st.copied != 0 {
		t.Errorf("shape A after unlock = image %v copied %d, want NULL/0 (S3 owns the selected-id row)", st.image, st.copied)
	}
	// B: S1 owns it - image stays NULL, mirror mark dropped exactly
	// once (the count pin proves S2 did NOT also count it).
	if st := read("linkbig.dat"); st.image != nil || st.copied != 0 {
		t.Errorf("shape B after unlock = image %v copied %d, want NULL/0 (S1 owns the cleared chunk-parent)", st.image, st.copied)
	}
	if st := read("linkprim.txt"); st.image != nil || st.copied != 0 {
		t.Errorf("shape B primary after unlock = image %v copied %d, want NULL/0", st.image, st.copied)
	}
	// C: untouched - already-0 mirror stays out of S1 entirely.
	if st := read("zeromirror.dat"); st.image != nil || st.copied != 0 {
		t.Errorf("shape C after unlock = image %v copied %d, want NULL/0 with NO count effect", st.image, st.copied)
	}
}

// --- the family predicate against the real-world nasty shapes ------------------

// TestClearPlanFamilyPredicateNastyShapes compares the S2 family
// subquery against the catalog's OWN hard-link derivations on the
// shapes that rule meets in the wild.
//
// THE DELIBERATE DIVERGENCE (2026-11-03 ruling - recorded here so a
// future pass does NOT "re-align" the claim side with hardLinkRows):
// S2's MEMBER side mirrors hardLinkRows / hardLinkPrimaries
// line-for-line (status='active' AND superseded_by IS NULL AND
// entry_kind='regular' AND nlink > 1 AND inode != 0, set-scoped), but
// the CLAIM side (the `a` row of the EXISTS) deliberately takes ANY
// status and has NO MIN(file_id) anchor role - ONE row of the family,
// however old or superseded, carrying image_id IN (selected) claims
// the whole family (the historical-anchor rule).
//
//   - a member carrying its OWN selected image_id (the T3.4 "the plan
//     assigned it before the family formed" shape): S2 skips it
//     (image_id IN selected) and S3 owns it - counted ONCE;
//   - the inode == 0 sentinel with nlink > 1: never a family (the
//     shared inode != 0 rule on BOTH sides) - both the lone sentinel
//     and the zero-inode PAIR (a selected-image row + a member
//     carrying the secondary-shaped mark, which would false-family if
//     the sentinel guard were dropped) behave as documented - marks
//     survive;
//   - an inode-reuse lookalike: an nlink == 1 row carrying the
//     SELECTED family's exact (dev, inode) - not a family member on
//     either side (nlink > 1 rule) - mark survives;
//   - the SUPERSEDED ANCHOR (shape G, the historical-anchor rule): the
//     family's anchor row is superseded while keeping its historical
//     image-2 claim - S3's any-status blanket releases the CLAIM row,
//     and the anchor still TRIGGERS step 2: the active member's mark
//     is unmarked because its link(2) was written inside image 2's
//     tree (this is exactly the widening the ruling buys; before it
//     the member kept copied=1);
//   - a CROSS-SET twin sharing the selected family's exact (dev,
//     inode): the subquery is set-scoped (a.set_id) - the other set's
//     row and full state stay byte-untouched;
//   - a MIXED-NLINK family: an nlink == 1 row with a LOWER file_id
//     sharing the family key (a stale nlink capture) must not anchor
//     the family - the nlink > 1 filter on BOTH sides keeps it out of
//     the claim, so the real member's mark survives;
//   - the ANCHOR-SIDE NEGATIVE: a family whose only historical claim
//     sits on the SIBLING image keeps its member's mark (a.image_id IN
//     selected - not "any historical row with an image_id");
//   - the CROSS-SET LOOKALIKE (shape K): an in-set family with NO
//     in-set claim at all, while ANOTHER set's row shares its exact
//     (dev, inode) AND carries image_id = selected - the member keeps
//     its mark (a.set_id).
//
// The distinct-changed-rows exactness pin runs across all shapes.
func TestClearPlanFamilyPredicateNastyShapes(t *testing.T) {
	f, _ := newScopedClearFixture(t)
	ctx := context.Background()
	sdb := rawOpen(t, f.db.Path())

	insert := func(rel string, dev, inode, nlink int64) int64 {
		t.Helper()
		var id int64
		f.mustTx(t, func(tx *ScanTx) {
			row := regularRow(rel, 32, 0xCD)
			row.Dev, row.Inode, row.Nlink = dev, inode, nlink
			var err error
			if id, err = tx.InsertScanFile(ctx, row); err != nil {
				t.Fatalf("InsertScanFile(%q): %v", rel, err)
			}
		})
		return id
	}
	stamp := func(query string, args ...any) {
		t.Helper()
		if _, err := sdb.Exec(query, args...); err != nil {
			t.Fatalf("fixture stamp %q: %v", query, err)
		}
	}

	// Shape D: BOTH family rows carry the selected image (the member's
	// own assignment predates the family).
	selfPri := insert("selfpri.txt", 13, 1301, 2)
	selfMem := insert("selfmem.txt", 13, 1301, 2)
	// Shape E: the inode == 0 sentinel with nlink > 1 - twice over:
	// (a) a lone sentinel row (its "family" would be itself), and
	// (b) a ZERO-INODE PAIR X/Y that would false-family if the
	// inode != 0 sentinel were dropped: X sits on the selected image,
	// Y carries the secondary-shaped mark. Correct code never derives
	// the pair, so Y's mark survives; a broken guard would let S2
	// unmark Y off a bogus primary.
	sentinel := insert("sentinel.txt", 14, 0, 2)
	zeroPri := insert("zerolink-a.txt", 16, 0, 2) // selected image, copied below
	zeroMem := insert("zerolink-b.txt", 16, 0, 2) // image NULL, mark survives
	// Shape J: a MIXED-NLINK family - jline1 is an nlink == 1 row
	// sharing the family key with a LOWER file_id than the real
	// member (a stale nlink capture after a partial unlink). The
	// nlink > 1 filter on BOTH sides of the claim must keep it out:
	// as an `m` it is not a member, as an `a` it never claims - so its
	// selected image must not wrongly unmark the real member.
	jLine1 := insert("jline1.txt", 17, 1701, 1) // nlink 1, lowest file_id, selected image
	jMem := insert("jmem.txt", 17, 1701, 2)     // the real member, image NULL, mark survives
	// Shape F: nlink == 1 row sharing the SELECTED fixture family's
	// exact (dev, inode) (newScopedClearFixture's famDev/famInode).
	const famDev, famInode = int64(7), int64(701)
	stale := insert("stale.txt", famDev, famInode, 1)
	// Shape G: the superseded ANCHOR - and the inode-reuse lookalike
	// of the brief: SAME path name, DIFFERENT inode (the production
	// supersede shape, InsertModification: the new row carries the same
	// relpath with a fresh inode and modified_of, the old row keeps its
	// historical image-2 claim with superseded_by set). anchor is
	// inserted FIRST (the lowest file_id), its active member shares
	// its (dev, inode) - the member's link(2) was written INSIDE image
	// 2's tree by the copy run, so the historical claim fires and step
	// 2 unmarks the member (the 2026-11-03 widening; before it the
	// member kept copied=1).
	anchor := insert("anchor.txt", 15, 1501, 2)
	anchorMem := insert("anchormem.txt", 15, 1501, 2)
	var anchorNew int64
	f.mustTx(t, func(tx *ScanTx) {
		row := regularRow("anchor.txt", 32, 0xCD) // SAME relpath, fresh inode
		row.Dev, row.Inode, row.Nlink = 15, 1599, 1
		var err error
		if anchorNew, err = tx.InsertModification(ctx, anchor, row); err != nil {
			t.Fatalf("InsertModification(anchor.txt): %v", err)
		}
	})
	// Shape I (the ANCHOR-SIDE NEGATIVE): a family whose only
	// historical claim sits on the SIBLING image - and on a SUPERSEDED
	// row, so the any-status widening is exercised on the negative
	// side too. The member must KEEP its mark: the claim side reads
	// a.image_id IN (selected), not "any historical row".
	sibOld := insert("sibanchor.txt", 18, 1801, 2)
	sibMem := insert("sibmem.txt", 18, 1801, 2)
	var sibNew int64
	f.mustTx(t, func(tx *ScanTx) {
		row := regularRow("sibanchor.txt", 32, 0xCD) // SAME relpath, fresh inode
		row.Dev, row.Inode, row.Nlink = 18, 1899, 1
		var err error
		if sibNew, err = tx.InsertModification(ctx, sibOld, row); err != nil {
			t.Fatalf("InsertModification(sibanchor.txt): %v", err)
		}
	})
	// Shape K (the CROSS-SET LOOKALIKE): an in-set family with NO
	// in-set claim at all, while ANOTHER set's row shares its exact
	// (dev, inode) AND carries image_id = selected. Without a.set_id
	// the EXISTS would reach across sets and wrongly unmark the in-set
	// member (pinned by mutation m3).
	kMem := insert("kmem.txt", 19, 1901, 2) // in-set, image NULL, mark survives

	// Shape H: the cross-set twin - SAME (dev, inode) as the selected
	// fixture family, in a second set of the same catalog (two scans
	// of one tree share dev+inode in reality).
	if err := f.db.EnsureSet(ctx, "S-OTHER", "/other", time.Now().UnixNano(), ToolVersion); err != nil {
		t.Fatalf("EnsureSet(S-OTHER): %v", err)
	}
	runB, err := f.db.StartScanRun(ctx, ScanRun{
		SetID: "S-OTHER", Kind: "ingest", Root: "/other",
		StartedAt: time.Now().UnixNano(), ExcludeJSON: "[]",
	})
	if err != nil {
		t.Fatalf("StartScanRun(S-OTHER): %v", err)
	}
	var twin int64
	var kTwin int64
	stx, err := f.db.BeginScanTx(ctx, runB, "S-OTHER")
	if err != nil {
		t.Fatalf("BeginScanTx(S-OTHER): %v", err)
	}
	{
		row := regularRow("twin.txt", 32, 0xEE)
		row.Dev, row.Inode, row.Nlink = famDev, famInode, 2
		if twin, err = stx.InsertScanFile(ctx, row); err != nil {
			t.Fatalf("InsertScanFile(twin): %v", err)
		}
		// Shape K's cross-set lookalike: the SAME (dev, inode) as the
		// in-set kMem family, carrying image_id = selected.
		row = regularRow("klook.txt", 32, 0xEE)
		row.Dev, row.Inode, row.Nlink = 19, 1901, 2
		if kTwin, err = stx.InsertScanFile(ctx, row); err != nil {
			t.Fatalf("InsertScanFile(klook): %v", err)
		}
	}
	if err := stx.Commit(); err != nil {
		t.Fatalf("commit S-OTHER scan tx: %v", err)
	}
	// The lookalike carries the SELECTED id in ITS OWN set - legal
	// (files.image_id has no cross-set meaning; the sets number their
	// destinations independently) and exactly the row a missing
	// a.set_id would latch onto.
	stamp(`UPDATE files SET image_id = ? WHERE file_id = ?`, scopedSelectedImage, kTwin)

	planTxCommit(t, f, func(tx *PlanTx) {
		for _, id := range []int64{selfPri, selfMem, anchor, zeroPri, jLine1} {
			if err := tx.AssignFile(ctx, id, scopedSelectedImage); err != nil {
				t.Fatalf("AssignFile(%d): %v", id, err)
			}
		}
		// Shape I's historical claim sits on the SIBLING image, not
		// the selected one.
		if err := tx.AssignFile(ctx, sibOld, 1); err != nil {
			t.Fatalf("AssignFile(sibanchor): %v", err)
		}
	})
	// The written-state marks: every seeded row except anchorNew and
	// sibNew (the fresh, unassigned superseding rows) and the
	// already-cleared fixture rows.
	stamp(`UPDATE files SET copied = 1 WHERE file_id IN (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		selfPri, selfMem, sentinel, stale, anchor, anchorMem, zeroPri, jMem, sibOld, sibMem)
	stamp(`UPDATE files SET copied = 1 WHERE file_id IN (?, ?, ?)`, zeroMem, twin, kMem)

	// The S-OTHER snapshot is taken AFTER all seeding so the unlock is
	// the only possible delta.
	otherBefore := dumpOtherSetState(t, f.db, "S-OTHER")
	before := dumpScopedState(t, f.db)

	// changedRowsByID keys the before/after diff by file_id (not
	// relpath): shape G's supersede leaves TWO rows under one path
	// name, which a relpath-keyed map could not tell apart.
	changedRowsByID := func(fn func()) map[int64]bool {
		t.Helper()
		read := func() map[int64]string {
			rows, err := sdb.Query(`SELECT file_id, COALESCE(image_id, -1), copied FROM files
			                        WHERE set_id = ? ORDER BY file_id`, f.setID)
			if err != nil {
				t.Fatalf("snapshot files: %v", err)
			}
			defer rows.Close()
			out := map[int64]string{}
			for rows.Next() {
				var id int64
				var image, copied int64
				if err := rows.Scan(&id, &image, &copied); err != nil {
					t.Fatalf("scan files snapshot: %v", err)
				}
				out[id] = fmt.Sprintf("%d/%d", image, copied)
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("iterate files snapshot: %v", err)
			}
			return out
		}
		b := read()
		fn()
		a := read()
		changed := map[int64]bool{}
		for id, before := range b {
			if a[id] != before {
				changed[id] = true
			}
		}
		return changed
	}

	var res ClearResult
	changed := changedRowsByID(func() {
		var err error
		res, err = f.db.ClearPlan(ctx, f.setID, []int64{scopedSelectedImage}, time.Now().UnixNano())
		if err != nil {
			t.Fatalf("ClearPlan([2]): %v", err)
		}
	})
	after := dumpScopedState(t, f.db)
	if before == after {
		t.Fatal("the unlock changed nothing - the fixture or the predicates are broken")
	}
	// NOTE: assertScopedUnlockDiff is deliberately NOT used here - its
	// family-flip derivation reads the whole files dump without a set
	// filter (fine for the single-set fixtures), and this test plants a
	// cross-set twin on purpose. The set-scoped equivalents are the
	// changed-rows exactness pin below and the S-OTHER byte-diff at the
	// end.

	// Exactness across every nasty shape: fixture 7 + D (both rows) +
	// G's superseded anchor AND its now-unmarked member (the
	// historical-anchor flip: 12 -> 13 rows) + E's zero-inode assigned
	// row + J's nlink-1 anchor row = 13 distinct rows, counted once
	// each. Shapes F, I (sibling-image claim), K (cross-set lookalike)
	// and the sentinel/pair deliberately do NOT change.
	if res.FilesCleared != len(changed) {
		t.Errorf("FilesCleared = %d, distinct changed rows = %d (a guard no longer keeps the predicates disjoint)",
			res.FilesCleared, len(changed))
	}
	want := map[int64]bool{
		mustFileID(t, f, scopedParent):   true,
		mustFileID(t, f, scopedFamNull):  true,
		mustFileID(t, f, scopedFamCross): true,
		mustFileID(t, f, scopedWhole2):   true,
		mustFileID(t, f, scopedFamPri):   true,
		mustFileID(t, f, scopedGone):     true,
		mustFileID(t, f, scopedVerOld):   true,
		selfPri:                          true, selfMem: true, anchor: true, anchorMem: true, zeroPri: true, jLine1: true,
	}
	if len(changed) != len(want) {
		t.Fatalf("changed files rows = %v, want exactly the 13 documented rows (ids %v)", changed, want)
	}
	for id := range want {
		if !changed[id] {
			t.Errorf("expected row file_id %d was not cleared (changed: %v)", id, changed)
		}
	}

	type state struct {
		image  any
		copied int
	}
	readID := func(id int64) state {
		t.Helper()
		var st state
		if err := sdb.QueryRow(`SELECT image_id, copied FROM files WHERE file_id = ?`, id).
			Scan(&st.image, &st.copied); err != nil {
			t.Fatalf("read file_id %d after unlock: %v", id, err)
		}
		return st
	}

	// D: the member carrying its OWN selected image_id is released by
	// S3 (NOT double-unmarked by S2) and counted once; the primary is
	// released too.
	if st := readID(selfMem); st.image != nil || st.copied != 0 {
		t.Errorf("self-selected member after unlock = image %v copied %d, want NULL/0 (S3 owns it, counted once)", st.image, st.copied)
	}
	if st := readID(selfPri); st.image != nil || st.copied != 0 {
		t.Errorf("self-selected primary after unlock = image %v copied %d, want NULL/0", st.image, st.copied)
	}
	// E: the inode == 0 sentinel is never a family - mark survives.
	if st := readID(sentinel); st.image != nil || st.copied != 1 {
		t.Errorf("inode==0 sentinel after unlock = image %v copied %d, want NULL/1 (never a family, mark survives)", st.image, st.copied)
	}
	// E (pair): the zero-inode X/Y rows never family - X is released
	// by S3 like any selected row, Y's secondary-shaped mark survives
	// because the inode != 0 guard keeps the pair out of the family
	// derivation entirely.
	if st := readID(zeroPri); st.image != nil || st.copied != 0 {
		t.Errorf("zero-inode selected row after unlock = image %v copied %d, want NULL/0 (S3 owns it)", st.image, st.copied)
	}
	if st := readID(zeroMem); st.image != nil || st.copied != 1 {
		t.Errorf("zero-inode member after unlock = image %v copied %d, want NULL/1 (inode!=0 guard: never a family, mark survives)", st.image, st.copied)
	}
	// J: the nlink == 1 row is released by S3 (it carries the selected
	// image) but never CLAIMS the family - the real member keeps its
	// mark because the nlink > 1 filter on BOTH sides keeps the stale
	// row out of the claim.
	if st := readID(jLine1); st.image != nil || st.copied != 0 {
		t.Errorf("nlink=1 anchor row after unlock = image %v copied %d, want NULL/0 (S3 owns it)", st.image, st.copied)
	}
	if st := readID(jMem); st.image != nil || st.copied != 1 {
		t.Errorf("mixed-nlink member after unlock = image %v copied %d, want NULL/1 (nlink>1 on both sides keeps the stale row from claiming)", st.image, st.copied)
	}
	// F: the nlink == 1 inode-reuse lookalike is never a family - mark
	// survives.
	if st := readID(stale); st.image != nil || st.copied != 1 {
		t.Errorf("nlink=1 lookalike after unlock = image %v copied %d, want NULL/1 (not a family member, mark survives)", st.image, st.copied)
	}
	// G: S3's any-status blanket releases the SUPERSEDED anchor's
	// historical image-2 claim (same path name, different inode)...
	if st := readID(anchor); st.image != nil || st.copied != 0 {
		t.Errorf("superseded anchor after unlock = image %v copied %d, want NULL/0 (step 3 is blanket, any status)", st.image, st.copied)
	}
	// ... AND the superseded anchor still TRIGGERS step 2: the active
	// member's link(2) was written inside image 2's tree, so the
	// historical claim unmarks it (the 2026-11-03 widening - the ONE
	// new flip this round; before it the member kept copied=1 and the
	// re-plan skipped it forever).
	if st := readID(anchorMem); st.image != nil || st.copied != 0 {
		t.Errorf("active member of a superseded anchor after unlock = image %v copied %d, want NULL/0 (the historical-anchor rule unmarks it)", st.image, st.copied)
	}
	// The superseding row (same relpath, fresh inode) is untouched -
	// never assigned, never a family member.
	if st := readID(anchorNew); st.image != nil || st.copied != 0 {
		t.Errorf("superseding row after unlock = image %v copied %d, want NULL/0 (untouched)", st.image, st.copied)
	}
	// H: the cross-set twin keeps its mark - the subquery never leaves
	// its set.
	var twinImage any
	var twinCopied int
	if err := sdb.QueryRow(`SELECT image_id, copied FROM files WHERE set_id = 'S-OTHER' AND relpath = 'twin.txt'`).
		Scan(&twinImage, &twinCopied); err != nil {
		t.Fatalf("read twin after unlock: %v", err)
	}
	if twinImage != nil || twinCopied != 1 {
		t.Errorf("cross-set twin after unlock = image %v copied %d, want NULL/1 (set-scoped subquery)", twinImage, twinCopied)
	}
	// I (the ANCHOR-SIDE NEGATIVE): the family's only historical claim
	// sits on the SIBLING image - and on a SUPERSEDED row, so the
	// any-status widening is exercised on the negative side too. The
	// member keeps its mark: the claim side reads a.image_id IN
	// (selected), not "any historical row with an image_id".
	if st := readID(sibMem); st.image != nil || st.copied != 1 {
		t.Errorf("sibling-image-claim member after unlock = image %v copied %d, want NULL/1 (the only claim is on image 1, not selected)",
			st.image, st.copied)
	}
	if st := readID(sibOld); st.image == nil || st.image.(int64) != 1 || st.copied != 1 {
		t.Errorf("sibling-image claim row after unlock = image %v copied %d, want image 1 / copied 1 (untouched: not selected)",
			st.image, st.copied)
	}
	// Shape I's superseding row (same relpath, fresh inode) is
	// untouched: never assigned, never a claim.
	if st := readID(sibNew); st.image != nil || st.copied != 0 {
		t.Errorf("sibling superseding row after unlock = image %v copied %d, want NULL/0 (untouched)", st.image, st.copied)
	}
	// K (the CROSS-SET LOOKALIKE): the in-set family has NO in-set
	// claim at all - only ANOTHER set's row carries the same
	// (dev, inode) AND image_id = selected. a.set_id keeps the EXISTS
	// inside the set, so the member's mark survives (mutation m3 must
	// fail exactly this pin). The lookalike row itself is covered by
	// the whole-S-OTHER dump comparison below.
	if st := readID(kMem); st.image != nil || st.copied != 1 {
		t.Errorf("cross-set-lookalike member after unlock = image %v copied %d, want NULL/1 (a.set_id keeps the claim in-set)",
			st.image, st.copied)
	}
	if after := dumpOtherSetState(t, f.db, "S-OTHER"); after != otherBefore {
		t.Errorf("the unlock changed the OTHER set (set-scoping violation):\nbefore:\n%s\nafter:\n%s", otherBefore, after)
	}
}

// TestClearPlanFamilyClaimAnchorRequiresRegularRow pins the `a`-side
// entry_kind guard (the U.1-F1 recorded coverage gap): a NON-regular
// row that shares a regular member's exact (dev, inode) and carries
// the SELECTED image id must NOT claim the family. One inode is one
// entry kind on a real filesystem, so the shape is seed-only - but the
// ruling gives the `a` row the SAME member-side guards as `m`
// (regular, nlink > 1, inode != 0, set-scoped), and this pins that
// promise: without `a.entry_kind = 'regular'` the seeded symlink row
// would claim the family and unmark the member (mutation: drop the
// clause -> this test must fail). The symlink row itself is released
// by step 3 like any selected row, counted once.
func TestClearPlanFamilyClaimAnchorRequiresRegularRow(t *testing.T) {
	f, _ := newScopedClearFixture(t)
	ctx := context.Background()
	sdb := rawOpen(t, f.db.Path())

	const dev, inode = int64(21), int64(2101)
	var kindMem, kindAnchor int64
	f.mustTx(t, func(tx *ScanTx) {
		row := regularRow("kindmem.txt", 32, 0xCD)
		row.Dev, row.Inode, row.Nlink = dev, inode, 2
		var err error
		if kindMem, err = tx.InsertScanFile(ctx, row); err != nil {
			t.Fatalf("InsertScanFile(kindmem.txt): %v", err)
		}
		// The same (dev, inode), NON-regular, carrying the selected id.
		row = regularRow("kindanchor.lnk", 12, 0xCD)
		row.EntryKind = "symlink"
		row.LinkTarget = "kindmem.txt"
		row.Dev, row.Inode, row.Nlink = dev, inode, 2
		if kindAnchor, err = tx.InsertScanFile(ctx, row); err != nil {
			t.Fatalf("InsertScanFile(kindanchor.lnk): %v", err)
		}
	})
	planTxCommit(t, f, func(tx *PlanTx) {
		if err := tx.AssignFile(ctx, kindAnchor, scopedSelectedImage); err != nil {
			t.Fatalf("AssignFile(kindanchor.lnk): %v", err)
		}
	})
	stamp := func(query string, args ...any) {
		t.Helper()
		if _, err := sdb.Exec(query, args...); err != nil {
			t.Fatalf("fixture stamp %q: %v", query, err)
		}
	}
	stamp(`UPDATE files SET copied = 1 WHERE file_id IN (?, ?)`, kindMem, kindAnchor)

	var res ClearResult
	changed := changedFileIDs(t, sdb, f.setID, func() {
		var err error
		res, err = f.db.ClearPlan(ctx, f.setID, []int64{scopedSelectedImage}, time.Now().UnixNano())
		if err != nil {
			t.Fatalf("ClearPlan([2]): %v", err)
		}
	})
	// Exactness: the fixture's 7 rows + the symlink row (step 3) = 8,
	// counted once each - the member never moves.
	if res.FilesCleared != len(changed) {
		t.Errorf("FilesCleared = %d, distinct changed rows = %d (the anchor-kind row double-counted or a member flipped)",
			res.FilesCleared, len(changed))
	}
	if changed["kindmem.txt"] {
		t.Errorf("regular member kindmem.txt changed (%v) - a symlink row must not claim its family", sortedKeys(changed))
	}
	type state struct {
		image  any
		copied int
	}
	read := func(rel string) state {
		t.Helper()
		var st state
		if err := sdb.QueryRow(`SELECT image_id, copied FROM files WHERE set_id = ? AND relpath = ?`,
			f.setID, rel).Scan(&st.image, &st.copied); err != nil {
			t.Fatalf("read %s after unlock: %v", rel, err)
		}
		return st
	}
	// The member keeps its mark: no `a` row of its (dev, inode) family
	// is a regular row on the selected image.
	if st := read("kindmem.txt"); st.image != nil || st.copied != 1 {
		t.Errorf("member after unlock = image %v copied %d, want NULL/1 (the symlink anchor must not claim)", st.image, st.copied)
	}
	// The symlink row is S3's (step 3 blanket): released, counted once.
	if st := read("kindanchor.lnk"); st.image != nil || st.copied != 0 {
		t.Errorf("symlink anchor after unlock = image %v copied %d, want NULL/0 (step 3 owns the selected row)", st.image, st.copied)
	}
}

// TestClearPlanNormalizesSelection: the catalog tier accepts any order
// and repeats - the resolved selection (and the audit array) is the
// sorted, deduplicated list, so `unlocked_images` never over-claims
// and duplicate inputs unlock each destination exactly once.
func TestClearPlanNormalizesSelection(t *testing.T) {
	f, _ := newScopedClearFixture(t)
	ctx := context.Background()

	// Duplicated and REVERSED input: [2 1] with 2 repeated.
	res, err := f.db.ClearPlan(ctx, f.setID, []int64{2, 1, 2}, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("ClearPlan([2 1 2]): %v", err)
	}
	if len(res.UnlockedImages) != 2 || res.UnlockedImages[0] != 1 || res.UnlockedImages[1] != 2 {
		t.Errorf("UnlockedImages = %v, want the sorted deduplicated [1 2]", res.UnlockedImages)
	}
	if res.ImagesRemoved != 2 {
		t.Errorf("ImagesRemoved = %d, want 2 (deduped: two distinct ids, not three)", res.ImagesRemoved)
	}
	// Both destinations gone (the union really unlocked both).
	sdb := rawOpen(t, f.db.Path())
	var images int
	if err := sdb.QueryRow(`SELECT COUNT(*) FROM images WHERE set_id = ?`, f.setID).Scan(&images); err != nil {
		t.Fatalf("count images: %v", err)
	}
	if images != 0 {
		t.Errorf("images rows after the union unlock = %d, want 0", images)
	}
}
