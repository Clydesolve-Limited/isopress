package cli

// U.1 (DESIGN_SPEC 4.6 selective unlock): the CLI-tier tests of the
// clear-assignments selectors (test plan Tier 3 + the Tier-1/3 halves
// of my ACs: AC-03's SQL-diff + payload half, AC-07, AC-08, AC-09's
// SIGINT leg, AC-10, AC-15, AC-18). Everything drives the REAL
// production command through the re-exec child (scan_test.go pattern);
// the catalog side is asserted with the SQLite driver directly
// (tests-only SQL - production SQL stays inside internal/catalog).
//
// The Tier-4 halves (AC-03's re-plan/copy/verify round trip) belong to
// the balancer (U.2) and wipe-gate (U.3) tasks.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"isopress/internal/media"
	"isopress/internal/report"
)

// --- helpers -----------------------------------------------------------------

// makeUnlockTree builds the AC-03 scope-isolation fixture tree: three
// whole files, a hard-link family, a chunked oversized file, and two
// empty directories - enough shape that every table carries BOTH
// selected and non-selected rows.
func makeUnlockTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("a.txt", "alpha\n")
	write("b.txt", "bravo\n")
	write("c.txt", "charlie\n")
	write("fam-a.txt", "family payload\n")
	if err := os.Link(filepath.Join(root, "fam-a.txt"), filepath.Join(root, "fam-b.txt")); err != nil {
		t.Fatalf("hard link fam-b: %v", err)
	}
	big := filepath.Join(root, "big.dat")
	f, err := os.Create(big)
	if err != nil {
		t.Fatalf("create big.dat: %v", err)
	}
	if err := f.Truncate(planFixtureBig); err != nil { // sparse, instant
		t.Fatalf("truncate big.dat: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close big.dat: %v", err)
	}
	for _, d := range []string{"empty1", "empty2"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	return root
}

// plannedImageIDs reads the set's destination ids (tests-only SQL).
func plannedImageIDs(t *testing.T, cat, setID string) []int64 {
	t.Helper()
	db := catOpen(t, cat)
	rows, err := db.Query(`SELECT image_id FROM images WHERE set_id = ? ORDER BY image_id`, setID)
	if err != nil {
		t.Fatalf("query planned ids: %v", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan planned id: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate planned ids: %v", err)
	}
	return out
}

// dumpUnlockStateCLI reads files/chunks/directories/images/
// image_contents/runs into table-prefixed lines (tests-only SQL) for
// the scope-isolation diff and the SIGINT atomicity comparison. Column
// positions are pinned by the ...Col constants below.
func dumpUnlockStateCLI(t *testing.T, db *sql.DB) string {
	t.Helper()
	var buf strings.Builder
	dump := func(query, table string) {
		t.Helper()
		rows, err := db.Query(query)
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
					fmt.Fprintf(&buf, "%v", tv)
				}
			}
			buf.WriteString("\n")
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate %s rows: %v", table, err)
		}
	}
	dump(`SELECT file_id, relpath, entry_kind, status, superseded_by, dev, inode, nlink,
	             image_id, copied, hash, chunked, chunk_count, chunk_size
	      FROM files ORDER BY file_id`, "files")
	dump(`SELECT chunk_id, file_id, chunk_index, offset, length, hash, image_id, copied, verified_at
	      FROM chunks ORDER BY chunk_id`, "chunks")
	dump(`SELECT dir_id, relpath, status, image_id FROM directories ORDER BY dir_id`, "directories")
	dump(`SELECT image_id, nominal_size, usable_size, media_profile, state
	      FROM images ORDER BY image_id`, "images")
	dump(`SELECT image_id, media_path, kind, size, hash FROM image_contents
	      ORDER BY image_id, media_path`, "image_contents")
	dump(`SELECT run_id, set_id, kind, root, started_at, finished_at, status, stats_json
	      FROM runs ORDER BY run_id`, "runs")
	return buf.String()
}

// Column positions of dumpUnlockStateCLI's data fields (after the
// "table|" prefix).
const (
	uiFilesRelpath    = 1
	uiFilesKind       = 2
	uiFilesStatus     = 3
	uiFilesSuperseded = 4
	uiFilesDev        = 5
	uiFilesInode      = 6
	uiFilesNlink      = 7
	uiFilesImageID    = 8
	uiFilesCopied     = 9
	uiChunksFileID    = 1
	uiChunksImageID   = 6
	uiChunksCopied    = 7
	uiChunksVerified  = 8
	uiDirsImageID     = 3
	uiContentsImageID = 0
	uiContentsPath    = 1
	uiRunsKind        = 2
)

// parseUnlockDump turns the dump into table -> pk -> data columns.
func parseUnlockDump(dump string) map[string]map[string][]string {
	out := map[string]map[string][]string{}
	for _, line := range strings.Split(strings.TrimSuffix(dump, "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Split(line, "|")
		table, cols := parts[0], parts[1:]
		var pk string
		switch table {
		case "image_contents":
			pk = cols[uiContentsImageID] + ":" + cols[uiContentsPath]
		default:
			pk = cols[0]
		}
		if out[table] == nil {
			out[table] = map[string][]string{}
		}
		out[table][pk] = cols
	}
	return out
}

func uiSameCols(a, b []string) bool {
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

// assertScopeIsolationDiff is AC-03's SQL diff: every row NOT carrying
// the selected image's id must be byte-identical, EXCEPT the stated
// carve-out - `copied` may move 1 -> 0 only on an active family member
// of a family that CLAIMED the selected image (the claim side takes
// ANY status and has no MIN(file_id) anchor role - the 2026-11-03
// historical-anchor rule). Rows that DID carry the id are required to
// be released exactly (image NULL / marks dropped / destination rows
// deleted). The only other delta is the committed clear run row. Every
// expected change is required to be PRESENT, so a predicate that
// silently stops matching fails here too.
func assertScopeIsolationDiff(t *testing.T, before, after string, selected int64) {
	t.Helper()
	b := parseUnlockDump(before)
	a := parseUnlockDump(after)
	selStr := fmt.Sprintf("%d", selected)

	// --- runs: prior rows identical, exactly ONE added clear run -------------
	if len(a["runs"]) != len(b["runs"])+1 {
		t.Errorf("runs rows = %d after, want %d (the one committed clear run)",
			len(a["runs"]), len(b["runs"])+1)
	}
	for pk, bcols := range b["runs"] {
		acols, ok := a["runs"][pk]
		if !ok || !uiSameCols(bcols, acols) {
			t.Errorf("prior run row %s changed across the unlock:\nbefore %v\nafter %v", pk, bcols, acols)
		}
	}
	for pk, acols := range a["runs"] {
		if _, ok := b["runs"][pk]; ok {
			continue
		}
		if acols[uiRunsKind] != "clear" {
			t.Errorf("unexpected added run row %s (kind %q)", pk, acols[uiRunsKind])
		}
	}

	// --- derive the AC-03 carve-out set from the BEFORE state ---------------
	// Active members of a family that CLAIMED the selected image.
	//
	// THE DELIBERATE DIVERGENCE FROM hardLinkRows (2026-11-03 ruling):
	// the member side below mirrors hardLinkRows / hardLinkPrimaries
	// (active, superseded_by IS NULL, regular, nlink > 1); the claim
	// side accepts ANY status and has NO MIN(file_id) anchor role - one
	// row of the family carrying image_id == selected claims it all. Do
	// not "re-align" this with hardLinkRows.
	familyFlip := map[string]bool{}
	{
		claimedKey := map[string]bool{} // famKey(dev|inode) claimed by an ANY-status row on the selected image
		for _, line := range strings.Split(strings.TrimSuffix(before, "\n"), "\n") {
			if !strings.HasPrefix(line, "files|") {
				continue
			}
			fc := strings.Split(line, "|")[1:]
			// The `a` row predicate of S2 (this helper is single-set).
			if fc[uiFilesKind] != "regular" || fc[uiFilesNlink] == "1" || fc[uiFilesInode] == "0" {
				continue
			}
			if fc[uiFilesImageID] == selStr {
				claimedKey[fc[uiFilesDev]+"|"+fc[uiFilesInode]] = true
			}
		}
		for _, line := range strings.Split(strings.TrimSuffix(before, "\n"), "\n") {
			if !strings.HasPrefix(line, "files|") {
				continue
			}
			fc := strings.Split(line, "|")[1:]
			// The `m` row predicate of S2: the active-only MEMBER
			// guard (M9) - the row S2 may UNMARK is always active.
			if fc[uiFilesStatus] != "active" || fc[uiFilesSuperseded] != "NULL" ||
				fc[uiFilesKind] != "regular" || fc[uiFilesNlink] == "1" {
				continue
			}
			if claimedKey[fc[uiFilesDev]+"|"+fc[uiFilesInode]] {
				familyFlip[fc[0]] = true
			}
		}
	}

	carveOutUsed := 0
	// --- files ----------------------------------------------------------------
	for pk, bcols := range b["files"] {
		acols, ok := a["files"][pk]
		if !ok {
			t.Errorf("files row %s vanished - the unlock never deletes file records (FR-010)", pk)
			continue
		}
		if uiSameCols(bcols, acols) {
			continue
		}
		rel := bcols[uiFilesRelpath]
		switch {
		case bcols[uiFilesImageID] == selStr:
			if acols[uiFilesImageID] != "NULL" || acols[uiFilesCopied] != "0" {
				t.Errorf("selected files row %s (%s) after unlock = image %s copied %s, want NULL/0",
					pk, rel, acols[uiFilesImageID], acols[uiFilesCopied])
			}
			b2 := append([]string{}, bcols...)
			b2[uiFilesImageID], b2[uiFilesCopied] = "NULL", "0"
			if !uiSameCols(b2, acols) {
				t.Errorf("selected files row %s (%s) changed MORE than image_id+copied:\nbefore %v\nafter %v",
					pk, rel, bcols, acols)
			}
		case familyFlip[pk] && bcols[uiFilesCopied] == "1" && acols[uiFilesCopied] == "0" &&
			bcols[uiFilesImageID] == acols[uiFilesImageID]:
			// THE STATED CARVE-OUT: copied 1 -> 0 on an active family
			// member of a family that claimed the selected image -
			// and nothing else on the row.
			carveOutUsed++
			b2 := append([]string{}, bcols...)
			b2[uiFilesCopied] = "0"
			if !uiSameCols(b2, acols) {
				t.Errorf("carve-out row %s (%s) changed MORE than copied 1->0:\nbefore %v\nafter %v",
					pk, rel, bcols, acols)
			}
		default:
			t.Errorf("non-selected files row %s (%s) changed - scope leak:\nbefore %v\nafter %v",
				pk, rel, bcols, acols)
		}
	}
	// The carve-out must be PRESENT (the fixture's family member really
	// flipped), not merely tolerated.
	flipped := 0
	for pk := range familyFlip {
		bcols, acols := b["files"][pk], a["files"][pk]
		if bcols == nil || bcols[uiFilesImageID] == selStr || bcols[uiFilesCopied] != "1" {
			continue // the primary carries the selected id; others were already 0
		}
		if acols == nil || acols[uiFilesCopied] != "0" {
			t.Errorf("family member %s kept copied=1 - the carve-out (step 2) never fired", pk)
		} else {
			flipped++
		}
	}
	if flipped == 0 && carveOutUsed == 0 {
		t.Error("the fixture produced no family carve-out flip - AC-03's stated difference is absent")
	}

	// --- chunks ---------------------------------------------------------------
	for pk, bcols := range b["chunks"] {
		acols, ok := a["chunks"][pk]
		if !ok {
			t.Errorf("chunk row %s vanished - the unlock never deletes chunk rows (FR-010)", pk)
			continue
		}
		if uiSameCols(bcols, acols) {
			continue
		}
		if bcols[uiChunksImageID] != selStr {
			t.Errorf("non-selected chunk %s changed:\nbefore %v\nafter %v", pk, bcols, acols)
			continue
		}
		b2 := append([]string{}, bcols...)
		b2[uiChunksImageID], b2[uiChunksCopied], b2[uiChunksVerified] = "NULL", "0", "NULL"
		if !uiSameCols(b2, acols) {
			t.Errorf("selected chunk %s must change ONLY image/copied/verified:\nbefore %v\nafter %v",
				pk, bcols, acols)
		}
	}

	// --- directories ----------------------------------------------------------
	for pk, bcols := range b["directories"] {
		acols, ok := a["directories"][pk]
		if !ok {
			t.Errorf("directory row %s vanished - the unlock never deletes directory rows", pk)
			continue
		}
		if uiSameCols(bcols, acols) {
			continue
		}
		if bcols[uiDirsImageID] != selStr {
			t.Errorf("non-selected directory %s changed:\nbefore %v\nafter %v", pk, bcols, acols)
			continue
		}
		b2 := append([]string{}, bcols...)
		b2[uiDirsImageID] = "NULL"
		if !uiSameCols(b2, acols) {
			t.Errorf("selected directory %s must change ONLY image_id:\nbefore %v\nafter %v", pk, bcols, acols)
		}
	}

	// --- images / image_contents (fills + finalized lists) --------------------
	for pk, bcols := range b["images"] {
		if pk == selStr {
			if acols, ok := a["images"][pk]; ok {
				t.Errorf("selected images row %s survived: %v", pk, acols)
			}
			continue
		}
		if acols, ok := a["images"][pk]; !ok || !uiSameCols(bcols, acols) {
			t.Errorf("sibling images row %s (fill) changed or vanished:\nbefore %v\nafter %v", pk, bcols, acols)
		}
	}
	for pk, bcols := range b["image_contents"] {
		if bcols[uiContentsImageID] == selStr {
			if acols, ok := a["image_contents"][pk]; ok {
				t.Errorf("selected image_contents row %s survived: %v", pk, acols)
			}
			continue
		}
		if acols, ok := a["image_contents"][pk]; !ok || !uiSameCols(bcols, acols) {
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
				t.Errorf("%s row %s appeared across the unlock", table, pk)
			}
		}
	}
}

// snapshotPayloadTree fingerprints the SOURCE tree (content, inode,
// mtime) so the diff can prove the unlock never touches the disk
// payload (AC-03's sibling-payload half).
func snapshotPayloadTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			out[rel+"/"] = fmt.Sprintf("dir|ino=%d|mtime=%d", inodeOf(info), info.ModTime().UnixNano())
			return nil
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(content)
		out[rel] = fmt.Sprintf("file|ino=%d|mtime=%d|sha=%s",
			inodeOf(info), info.ModTime().UnixNano(), hex.EncodeToString(sum[:]))
		return nil
	})
	if err != nil {
		t.Fatalf("walk payload tree: %v", err)
	}
	return out
}

func inodeOf(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return st.Ino
	}
	return 0
}

// --- AC-03: scope isolation with the family carve-out --------------------------

// TestClearScopedUnlockIsolatesSiblingsWithFamilyCarveOut builds the
// disclosed AC-03 fixture - set {1,2,3} of whole files plus a hard-link
// family whose primary sits on image 2 (chunks moved off image 2: no
// chunked parent is assigned to the unlocked image in this fixture,
// per the AC-03 boundary note; chunk-parent mirror behavior is pinned
// by AC-10 instead) - and runs the REAL `clear --image 2`. The SQL
// diff must show every row NOT carrying image 2's id byte-identical
// EXCEPT `copied` 1 -> 0 on the active family member of a family that
// claimed image 2, and the source payload on disk must be
// byte-identical (content, inode, mtime).
func TestClearScopedUnlockIsolatesSiblingsWithFamilyCarveOut(t *testing.T) {
	root := makeUnlockTree(t)
	cat := createCatalog(t)
	const setID = "S-ISO"
	scanPlanFixture(t, root, cat, setID)
	if stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", setID, "--size", planFixtureSize); code != report.ExitOK {
		t.Fatalf("plan fixture exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// The fresh plan holds {1,2}; the fixture adds a third destination
	// and HAND-PLACES rows so every table carries selected and
	// non-selected rows (fixture shaping, not product behavior - the
	// balancer's placement is U.2's subject).
	planned := plannedImageIDs(t, cat, setID)
	if len(planned) < 2 {
		t.Fatalf("fixture plan produced %d destinations, want >= 2", len(planned))
	}
	db := catRW(t, cat)
	if _, err := db.Exec(`INSERT INTO images (set_id, image_id, nominal_size, usable_size, media_profile)
	                      VALUES (?, 3, ?, ?, 'disk')`,
		setID, int64(planFixtureNominal), int64(planFixtureUsable)); err != nil {
		t.Fatalf("seeding destination 3: %v", err)
	}
	// Family primary = the LOWER file_id (the catalog's family rule).
	var primary, sibling int64
	rows, err := db.Query(`SELECT file_id, relpath FROM files WHERE set_id = ? AND relpath IN ('fam-a.txt', 'fam-b.txt')
	                       ORDER BY file_id`, setID)
	if err != nil {
		t.Fatalf("query family rows: %v", err)
	}
	var famIDs []int64
	for rows.Next() {
		var id int64
		var rel string
		if err := rows.Scan(&id, &rel); err != nil {
			t.Fatalf("scan family row: %v", err)
		}
		famIDs = append(famIDs, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate family rows: %v", err)
	}
	rows.Close()
	if len(famIDs) != 2 {
		t.Fatalf("family rows = %d, want 2", len(famIDs))
	}
	primary, sibling = famIDs[0], famIDs[1]

	// Reshape to the AC-03 shape: c.txt on image 3; ALL chunks on image
	// 1 (no chunked parent assigned to the unlocked image - the
	// disclosed fixture boundary); the family primary on image 2 and
	// its member on image 1 (the cross-image member shape); empty2 on
	// image 1.
	reshape := []struct {
		query string
		args  []any
		what  string
	}{
		{`UPDATE files SET image_id = 3 WHERE set_id = ? AND relpath = 'c.txt'`, []any{setID}, "c.txt -> 3"},
		{`UPDATE chunks SET image_id = 1 WHERE set_id = ?`, []any{setID}, "all chunks -> 1"},
		{`UPDATE files SET image_id = 2 WHERE file_id = ?`, []any{primary}, "family primary -> 2"},
		{`UPDATE files SET image_id = 1 WHERE file_id = ?`, []any{sibling}, "family member -> 1"},
		{`UPDATE directories SET image_id = 1 WHERE set_id = ? AND relpath = 'empty2'`, []any{setID}, "empty2 -> 1"},
	}
	for _, r := range reshape {
		if _, err := db.Exec(r.query, r.args...); err != nil {
			t.Fatalf("reshape (%s): %v", r.what, err)
		}
	}
	// Written+verified marks a completed copy run leaves (the seeding
	// helper stamps chunks and every assigned file).
	seedWrittenChunkState(t, cat, setID)
	// Finalized content lists for all three destinations (6.6 shape).
	for _, image := range []int64{1, 2, 3} {
		for _, p := range []string{"files/a.txt", "MANIFEST.json"} {
			if _, err := db.Exec(`INSERT INTO image_contents
			        (set_id, image_id, media_path, kind, size)
			        VALUES (?, ?, ?, 'file', 6)`, setID, image, p); err != nil {
				t.Fatalf("seeding image_contents %d/%s: %v", image, p, err)
			}
		}
	}
	// Sanity: the family primary really sits on image 2 pre-unlock.
	var primImage any
	if err := db.QueryRow(`SELECT image_id FROM files WHERE file_id = ?`, primary).Scan(&primImage); err != nil {
		t.Fatalf("read primary image: %v", err)
	}
	if primImage == nil || primImage.(int64) != 2 {
		t.Fatalf("fixture family primary image = %v, want 2", primImage)
	}

	beforeState := dumpUnlockStateCLI(t, catOpen(t, cat))
	beforePayload := snapshotPayloadTree(t, root)

	// THE REAL COMMAND.
	stdout, stderr, code := runClearChild(t, "--catalog", cat, "--set-id", setID, "--image", "2")
	if code != report.ExitOK {
		t.Fatalf("clear --image 2 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// The scoped report: the ONE added human line names the released id.
	if !strings.Contains(stdout, "unlocked images: 2") {
		t.Errorf("scoped report lacks the 'unlocked images: 2' line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "destinations removed: 1") {
		t.Errorf("scoped report lacks the destinations-removed line:\n%s", stdout)
	}
	if !strings.Contains(stderr, "clear: cleared") {
		t.Errorf("stderr lacks the clear summary:\n%s", stderr)
	}

	afterState := dumpUnlockStateCLI(t, catOpen(t, cat))
	assertScopeIsolationDiff(t, beforeState, afterState, 2)

	// The carve-out row, asserted directly: the active family member
	// of the family that claimed image 2 lost ONLY its copied mark and
	// kept its own assignment.
	var memberImage any
	var memberCopied int
	if err := db.QueryRow(`SELECT image_id, copied FROM files WHERE file_id = ?`, sibling).
		Scan(&memberImage, &memberCopied); err != nil {
		t.Fatalf("read family member: %v", err)
	}
	if memberImage == nil || memberImage.(int64) != 1 || memberCopied != 0 {
		t.Errorf("family member after unlock = image %v copied %d, want image 1 copied 0 (the stated carve-out)",
			memberImage, memberCopied)
	}
	// Sibling payload on disk: byte-identical (content, inode, mtime).
	afterPayload := snapshotPayloadTree(t, root)
	if !reflect.DeepEqual(beforePayload, afterPayload) {
		t.Errorf("the unlock touched the source payload:\nbefore %+v\nafter %+v", beforePayload, afterPayload)
	}
}

// --- AC-07 + AC-18(b): the validation / flag matrix ---------------------------

// newPlannedFixture scans + plans a fresh fixture tree under setID and
// returns the catalog path.
func newPlannedFixture(t *testing.T, setID string) string {
	t.Helper()
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, setID)
	if stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", setID, "--size", planFixtureSize); code != report.ExitOK {
		t.Fatalf("plan fixture (%s) exit = %d, want 0\nstdout: %s\nstderr: %s", setID, code, stdout, stderr)
	}
	return cat
}

// TestClearScopedValidationMatrix is AC-07's validation matrix plus
// AC-18(b)'s flag-matrix run: every selector refusal class exits 2/4/5
// with ZERO catalog changes, the success legs exit 0 (dedupe, union,
// --last, --strict inert), and NO invocation anywhere in the matrix
// produces any code outside the pre-feature set {0,2,4,5} - clear
// NEVER exits 10 in either mode.
func TestClearScopedValidationMatrix(t *testing.T) {
	observed := map[string]int{}
	record := func(name string, code int) {
		observed[name] = code
		switch code {
		case report.ExitOK, report.ExitUsage, report.ExitCatalog, report.ExitCatalogBusy:
		default:
			t.Errorf("leg %q exited %d - clear-assignments has no such exit class (pre-feature codes: 0/2/4/5; never 10)",
				name, code)
		}
		if code == report.ExitItemFailures {
			t.Errorf("leg %q exited 10 - the clear is one atomic transaction (never exits 10)", name)
		}
	}

	// --- Phase A: REFUSAL legs on ONE planned fixture (all pre-transaction,
	// so the catalog must stay byte-identical across the whole phase).
	cat := newPlannedFixture(t, "S-MATRIX")
	before := fileBytes(t, cat)
	base := []string{"--catalog", cat, "--set-id", "S-MATRIX"}
	refusals := []struct {
		name    string
		args    []string
		wantMsg string
	}{
		{"--image 999 not planned", []string{"--image", "999"}, "is not a planned image"},
		{"--image 999 names the planned ids", []string{"--image", "999"}, "planned: 1, 2"},
		{"--image 0 is 1-based", []string{"--image", "0"}, "1-based"},
		{"--image=-1 is 1-based", []string{"--image=-1"}, "1-based"},
		{"--image -1 separate args is 1-based", []string{"--image", "-1"}, "1-based"},
		{"--image abc is a flag parse failure", []string{"--image", "abc"}, "invalid argument"},
		{"unknown flag", []string{"--bogus"}, "unknown flag"},
	}
	for _, leg := range refusals {
		t.Run("refuse/"+leg.name, func(t *testing.T) {
			stdout, stderr, code := runClearChild(t, append(append([]string{}, base...), leg.args...)...)
			record("refuse/"+leg.name, code)
			if code != report.ExitUsage {
				t.Fatalf("exit = %d, want 2\nstdout: %s\nstderr: %s", code, stdout, stderr)
			}
			if !strings.Contains(stderr, leg.wantMsg) {
				t.Errorf("stderr lacks %q:\n%s", leg.wantMsg, stderr)
			}
			if stdout != "" {
				t.Errorf("stdout must stay empty on a refusal, got: %q", stdout)
			}
		})
	}
	// Unknown set WITH a selector: the set-existence gate still wins
	// (exit 2, same class).
	t.Run("refuse/unknown set with selector", func(t *testing.T) {
		stdout, stderr, code := runClearChild(t, "--catalog", cat, "--set-id", "S-NOPE", "--image", "1")
		record("refuse/unknown set", code)
		if code != report.ExitUsage || !strings.Contains(stderr, "no scanned set") {
			t.Errorf("exit = %d stderr = %s, want 2 + the scan-first gate", code, stderr)
		}
		if stdout != "" {
			t.Errorf("stdout must stay empty, got: %q", stdout)
		}
	})
	// Missing catalog WITH a selector: exit 4 (open happens before any
	// selector validation - there is no catalog to validate against).
	t.Run("refuse/missing catalog with selector", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "no-such.sqlite")
		stdout, stderr, code := runClearChild(t, "--catalog", missing, "--set-id", "S-404", "--image", "1")
		record("refuse/missing catalog", code)
		if code != report.ExitCatalog || !strings.Contains(stderr, "create one first") {
			t.Errorf("exit = %d stderr = %s, want 4 + the no-auto-create guidance", code, stderr)
		}
		if stdout != "" {
			t.Errorf("stdout must stay empty on a catalog error, got: %q", stdout)
		}
		if _, err := os.Stat(missing); !os.IsNotExist(err) {
			t.Errorf("clear auto-created the missing catalog (stat err = %v)", err)
		}
	})
	// The whole refusal phase left the catalog byte-identical.
	if !bytes.Equal(before, fileBytes(t, cat)) {
		t.Error("the refusal legs changed the catalog bytes - selector validation must run BEFORE the transaction")
	}
	// A scoped unlock under a held flock is still exit 5 (the flock is
	// taken before any selector work - pinned with the real holder in
	// TestClearLockContentionExit5's scoped leg; recorded here for the
	// code-set summary).

	// --- Phase B: --last on a scan-only (never-planned) set -> exit 2 with
	// plan-first guidance (AC-34's cell, exercised through the matrix).
	t.Run("refuse/--last on never-planned set", func(t *testing.T) {
		root := makePlanFixture(t)
		scanOnly := createCatalog(t)
		scanPlanFixture(t, root, scanOnly, "S-SCANONLY")
		stdout, stderr, code := runClearChild(t,
			"--catalog", scanOnly, "--set-id", "S-SCANONLY", "--last")
		record("refuse/--last unplanned", code)
		if code != report.ExitUsage {
			t.Fatalf("--last on a scan-only set exit = %d, want 2\nstderr: %s", code, stderr)
		}
		if !strings.Contains(stderr, "has no planned images") || !strings.Contains(stderr, "isopress plan") {
			t.Errorf("stderr lacks the plan-first guidance:\n%s", stderr)
		}
		if stdout != "" {
			t.Errorf("stdout must stay empty, got: %q", stdout)
		}
	})

	// --- Phase C: SUCCESS legs, each on its own fresh fixture (they
	// consume the selection). Dedupe, union, --last, --strict inert.
	t.Run("success/--image 1", func(t *testing.T) {
		cat := newPlannedFixture(t, "S-C1")
		stdout, stderr, code := runClearChild(t, "--catalog", cat, "--set-id", "S-C1", "--image", "1")
		record("success/--image 1", code)
		if code != report.ExitOK {
			t.Fatalf("clear --image 1 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "unlocked images: 1") {
			t.Errorf("scoped report lacks the unlocked-images line:\n%s", stdout)
		}
		if ids := plannedImageIDs(t, cat, "S-C1"); !reflect.DeepEqual(ids, []int64{2}) {
			t.Errorf("planned ids after --image 1 = %v, want [2]", ids)
		}
	})
	t.Run("success/dedupe repeated --image", func(t *testing.T) {
		cat := newPlannedFixture(t, "S-C2")
		stdout, stderr, code := runClearChild(t,
			"--catalog", cat, "--set-id", "S-C2", "--image", "1", "--image", "1", "--json")
		record("success/dedupe", code)
		if code != report.ExitOK {
			t.Fatalf("clear --image 1 --image 1 exit = %d, want 0 (a repeated selector is harmless)\nstderr: %s", code, stderr)
		}
		doc := decodeSingleJSONDoc(t, stdout)
		// Counts for ONE image - the repeated flag must not double the
		// unlock (the JSON document of a duplicate-flag run equals a
		// single-flag run's).
		if got := doc["unlocked_images"]; !reflect.DeepEqual(got, []any{float64(1)}) {
			t.Errorf("dedupe unlocked_images = %v, want [1]", got)
		}
		if doc["destinations_removed"].(float64) != 1 {
			t.Errorf("dedupe destinations_removed = %v, want 1 (exactly one image unlocked)", doc["destinations_removed"])
		}
	})
	t.Run("success/--image 1 --last union", func(t *testing.T) {
		cat := newPlannedFixture(t, "S-C3")
		stdout, stderr, code := runClearChild(t,
			"--catalog", cat, "--set-id", "S-C3", "--image", "1", "--last", "--json")
		record("success/union", code)
		if code != report.ExitOK {
			t.Fatalf("union clear exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		doc := decodeSingleJSONDoc(t, stdout)
		if !reflect.DeepEqual(doc["unlocked_images"], []any{float64(1), float64(2)}) {
			t.Errorf("union unlocked_images = %v, want [1 2] (the UNION, sorted, deduped)", doc["unlocked_images"])
		}
		if doc["destinations_removed"].(float64) != 2 {
			t.Errorf("union destinations_removed = %v, want 2", doc["destinations_removed"])
		}
	})
	t.Run("success/--last", func(t *testing.T) {
		cat := newPlannedFixture(t, "S-C4")
		stdout, stderr, code := runClearChild(t, "--catalog", cat, "--set-id", "S-C4", "--last")
		record("success/--last", code)
		if code != report.ExitOK {
			t.Fatalf("clear --last exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "unlocked images: 2") {
			t.Errorf("--last report lacks 'unlocked images: 2' (the highest planned id):\n%s", stdout)
		}
	})
	t.Run("success/--strict inert", func(t *testing.T) {
		// Two IDENTICAL fixtures, one unlocked WITH --strict, one
		// without: the reports must be indistinguishable modulo the set
		// id (the documented inertness judgment).
		var reports []string
		for _, leg := range []struct {
			setID  string
			strict bool
		}{{"S-C5A", false}, {"S-C5B", true}} {
			cat := newPlannedFixture(t, leg.setID)
			args := []string{"--catalog", cat, "--set-id", leg.setID, "--image", "1", "--json"}
			if leg.strict {
				args = append(args, "--strict")
			}
			stdout, stderr, code := runClearChild(t, args...)
			record("success/strict-"+leg.setID, code)
			if code != report.ExitOK {
				t.Fatalf("clear %s exit = %d, want 0\nstdout: %s\nstderr: %s", leg.setID, code, stdout, stderr)
			}
			reports = append(reports, strings.ReplaceAll(stdout, leg.setID, "S-C5"))
		}
		if reports[0] != reports[1] {
			t.Errorf("--strict changed the scoped clear report (inert-by-design violation):\nwithout:\n%s\nwith:\n%s",
				reports[0], reports[1])
		}
	})

	// The matrix-wide code-set claim (AC-07: no new exit codes
	// anywhere; AC-18(b): clear never exits 10).
	if len(observed) == 0 {
		t.Fatal("the matrix observed no legs - it proves nothing")
	}
	for name, code := range observed {
		switch code {
		case report.ExitOK, report.ExitUsage, report.ExitCatalog, report.ExitCatalogBusy:
		default:
			t.Errorf("matrix leg %q exited %d: clear-assignments must never produce a new exit code (nor 10)", name, code)
		}
	}
}

// --- AC-18(b): help disclosure + the never-10 pin ------------------------------

// TestClearHelpDisclosureAndNeverExits10 pins the design Block 6a/6b/6c
// help texts (verbatim, pasted from the approved round-3 source) and
// AC-18(b)'s "clear never exits 10" help substring.
func TestClearHelpDisclosureAndNeverExits10(t *testing.T) {
	var cmdName = "clear-assignments"
	for _, c := range NewRootCmd().Commands() {
		if c.Name() != cmdName {
			continue
		}
		// 6a: the scope-aware Short.
		const short6a = "clear plan assignments in one transaction (whole set, or selected --image/--last; keeps rows and hashes)"
		if c.Short != short6a {
			t.Errorf("Short = %q\nwant the design 6a text %q", c.Short, short6a)
		}
		// 6b: the scope-aware Long opening (verbatim sentence), with the
		// retained "deletes NO file records" remainder still intact.
		for _, want := range []string{
			"Clear plan assignments of the --set-id set in ONE transaction (DESIGN_SPEC 4.5, FR-010, 4.6). " +
				"With NO selector the command clears EVERY plan assignment:",
			"With --image N (repeatable) or --last the command clears ONLY the selected images' assignments, " +
				"marks, and destination rows; every other image keeps its assignments, marks, chunk placements, " +
				"and media untouched, and the next plan re-mints the released id at its lowest-unused slot " +
				"(DESIGN_SPEC 4.6).",
			"deletes NO file records",
			// 6c: the appended unlock/wipe paragraph (verbatim).
			"Unlocking images with `--image N` (repeatable) or `--last` releases only those images' assignments " +
				"in the catalog; it never touches the media on disk.",
			// 6c MIDDLE SENTENCE - the U.1 release-gate pin (TODO: "pin
			// 6c's middle sentence so U.3's landing is forced to touch
			// the test"): the REMOVES-the-directory claim the wipe gate
			// makes TRUE, cross-pinned by copy's Block 6d paragraph
			// (help_disclosure_test.go).
			"The next `copy` run of a rewritten image first REMOVES the existing `<dest>/<SET-ID>-NNN` " +
				"directory (it is about to be rewritten in that same run) and records the removal " +
				"in the run report and the catalog's run log.",
			"A directory whose `MANIFEST.json` names a different set or image, or cannot be parsed as a " +
				"manifest, is refused instead of removed; other images' trees are never removed (a copy run " +
				"still refreshes each tree's manifest/export, as before).",
			// AC-18(b): the never-10 pin.
			"never exits 10",
			// The no-auto-create gate the T2.3 review asked for.
			"NOT created",
		} {
			if !strings.Contains(c.Long, want) {
				t.Errorf("Long lacks the pinned text %q:\n%s", want, c.Long)
			}
		}
		// The OLD pre-4.6 opening must be GONE (6b REPLACED it).
		if strings.Contains(c.Long, "Clear every plan assignment of the --set-id set") {
			t.Errorf("Long still carries the pre-4.6 opening - Block 6b must REPLACE it:\n%s", c.Long)
		}
		return
	}
	t.Fatal("clear-assignments missing from the root command tree")
}

// --- AC-08: the full clear is behaviorally byte-for-byte unchanged -------------

// TestClearFullClearGoldenUnchanged pins AC-08: with NO selector flags
// the command's exit code, counts, and HUMAN stdout bytes are exactly
// the pre-feature output (the golden the T2.4 renderer shipped), and
// --json carries exactly the frozen 10 keys with `unlocked_images` =
// the sorted array of ALL removed ids.
func TestClearFullClearGoldenUnchanged(t *testing.T) {
	// The golden human stdout for the plan fixture cleared after the
	// written-state seed - the pre-feature byte sequence (the
	// centerpiece's counts: 3 files, 2 chunks, 1 dir, 2 destinations,
	// 2 retained hashes).
	const wantHuman = "clear: set S-FULL\n" +
		"cleared: 3 files, 2 chunks, 1 dirs\n" +
		"destinations removed: 2\n" +
		"chunk hashes retained: 2 (rows and hashes survive, FR-010; a same-profile re-plan reuses them)\n"

	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-FULL")
	if stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", "S-FULL", "--size", planFixtureSize); code != report.ExitOK {
		t.Fatalf("plan fixture exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	seedWrittenChunkState(t, cat, "S-FULL")

	stdout, stderr, code := runClearChild(t, "--catalog", cat, "--set-id", "S-FULL")
	if code != report.ExitOK {
		t.Fatalf("full clear exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if stdout != wantHuman {
		t.Errorf("full-clear human stdout changed (AC-08 byte-for-byte):\ngot:\n%s\nwant:\n%s", stdout, wantHuman)
	}
	if !strings.Contains(stderr, "clear: cleared 3 files, 2 chunks, 1 dirs; removed 2 destinations") {
		t.Errorf("stderr lacks the pre-feature summary line:\n%s", stderr)
	}
	// The scoped line must NOT appear on a full clear.
	if strings.Contains(stdout, "unlocked images:") {
		t.Errorf("full-clear stdout gained the scoped line (byte-identity violation):\n%s", stdout)
	}

	// --json: exactly the frozen keys (the symbol is the pin) ...
	jsonRoot := makePlanFixture(t)
	jsonCat := createCatalog(t)
	scanPlanFixture(t, jsonRoot, jsonCat, "S-FULLJ")
	if stdout, stderr, code := runPlanChild(t,
		"--catalog", jsonCat, "--set-id", "S-FULLJ", "--size", planFixtureSize); code != report.ExitOK {
		t.Fatalf("plan fixture exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	seedWrittenChunkState(t, jsonCat, "S-FULLJ")
	jout, jerr, jcode := runClearChild(t, "--catalog", jsonCat, "--set-id", "S-FULLJ", "--json")
	if jcode != report.ExitOK {
		t.Fatalf("full clear --json exit = %d, want 0\nstdout: %s\nstderr: %s", jcode, jout, jerr)
	}
	doc := goldenJSONDoc(t, jout, clearReportJSONKeys) // exactly 10 keys
	assertJSONTypes(t, doc, clearJSONTypes)
	if len(clearReportJSONKeys) != 10 {
		t.Errorf("clearReportJSONKeys has %d keys, want exactly 10 (the frozen 9 -> 10 addition)", len(clearReportJSONKeys))
	}
	if !reflect.DeepEqual(doc["unlocked_images"], []any{float64(1), float64(2)}) {
		t.Errorf("full-clear unlocked_images = %v, want ALL removed ids sorted [1 2]", doc["unlocked_images"])
	}
}

// --- AC-10: NO-SKIP completeness (zero surviving marks) ------------------------

// TestClearNoSurvivingMarksForUnlockedImage builds a fixture where
// image 1 carries every mark shape - assigned whole files, chunk rows
// with copied+verified_at, the 5.2 parent mirror, and a hard-link
// family whose primary sits on image 1 - runs the REAL
// `clear --image 1`, and asserts via direct SQL that ZERO surviving
// `copied=1`/`verified_at` marks still describe image 1.
func TestClearNoSurvivingMarksForUnlockedImage(t *testing.T) {
	// Fixture: the plan fixture plus a hard-link family.
	root := makePlanFixture(t)
	famA := filepath.Join(root, "fam-a.txt")
	if err := os.WriteFile(famA, []byte("family\n"), 0o644); err != nil {
		t.Fatalf("write fam-a: %v", err)
	}
	if err := os.Link(famA, filepath.Join(root, "fam-b.txt")); err != nil {
		t.Fatalf("hard link fam-b: %v", err)
	}
	cat := createCatalog(t)
	const setID = "S-NOSKIP"
	scanPlanFixture(t, root, cat, setID)
	if stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", setID, "--size", planFixtureSize); code != report.ExitOK {
		t.Fatalf("plan fixture exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// Shape the fixture so image 1 holds the marks: the family primary
	// (LOWER file_id) moves to image 1, its member becomes the D-04
	// secondary (image_id NULL, claim mark copied=1), and the chunked
	// parent's mirror goes to copied=1 (the 5.2 count-proof state of a
	// completed copy).
	db := catRW(t, cat)
	var famIDs []int64
	rows, err := db.Query(`SELECT file_id FROM files WHERE set_id = ? AND relpath IN ('fam-a.txt','fam-b.txt')
	                       ORDER BY file_id`, setID)
	if err != nil {
		t.Fatalf("query family: %v", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan family id: %v", err)
		}
		famIDs = append(famIDs, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate family: %v", err)
	}
	rows.Close()
	if len(famIDs) != 2 {
		t.Fatalf("family rows = %d, want 2", len(famIDs))
	}
	primary, member := famIDs[0], famIDs[1]
	if _, err := db.Exec(`UPDATE files SET image_id = 1 WHERE file_id = ?`, primary); err != nil {
		t.Fatalf("family primary -> image 1: %v", err)
	}
	if _, err := db.Exec(`UPDATE files SET image_id = NULL WHERE file_id = ?`, member); err != nil {
		t.Fatalf("family member -> NULL: %v", err)
	}
	seedWrittenChunkState(t, cat, setID)
	// The member (image NULL) and the parent mirror need their marks
	// stamped directly - a completed copy leaves them exactly so.
	if _, err := db.Exec(`UPDATE files SET copied = 1 WHERE file_id IN (?, (
		SELECT file_id FROM files WHERE set_id = ? AND relpath = 'big.dat'))`, member, setID); err != nil {
		t.Fatalf("stamping member + parent mirror: %v", err)
	}

	// Snapshot the sets of rows whose marks DESCRIBE image 1.
	ro := catOpen(t, cat)
	collect := func(query string, args ...any) []int64 {
		t.Helper()
		rs, err := ro.Query(query, args...)
		if err != nil {
			t.Fatalf("snapshot query: %v", err)
		}
		defer rs.Close()
		var out []int64
		for rs.Next() {
			var id int64
			if err := rs.Scan(&id); err != nil {
				t.Fatalf("scan snapshot id: %v", err)
			}
			out = append(out, id)
		}
		if err := rs.Err(); err != nil {
			t.Fatalf("iterate snapshot: %v", err)
		}
		return out
	}
	assignedFiles := collect(`SELECT file_id FROM files WHERE set_id = ? AND image_id = 1`, setID)
	assignedChunks := collect(`SELECT chunk_id FROM chunks WHERE set_id = ? AND image_id = 1`, setID)
	mirrorParents := collect(`SELECT DISTINCT file_id FROM files WHERE set_id = ?
		AND EXISTS (SELECT 1 FROM chunks c WHERE c.file_id = files.file_id AND c.image_id = 1)`, setID)
	membersOfPrimaryOn1 := []int64{}
	{
		// Members = active nlink>1 rows sharing (dev,inode) with the
		// primary, whose primary's image is 1.
		type fam struct {
			dev, inode int64
		}
		primaries := map[fam]int64{}
		members := []struct {
			id    int64
			dev   int64
			inode int64
			image sql.NullInt64
		}{}
		ms, err := ro.Query(`SELECT file_id, dev, inode, image_id FROM files WHERE set_id = ?
			AND status = 'active' AND superseded_by IS NULL AND entry_kind = 'regular' AND nlink > 1`, setID)
		if err != nil {
			t.Fatalf("query family: %v", err)
		}
		for ms.Next() {
			var m struct {
				id    int64
				dev   int64
				inode int64
				image sql.NullInt64
			}
			if err := ms.Scan(&m.id, &m.dev, &m.inode, &m.image); err != nil {
				t.Fatalf("scan family: %v", err)
			}
			k := fam{m.dev, m.inode}
			if cur, ok := primaries[k]; !ok || m.id < cur {
				primaries[k] = m.id
			}
			members = append(members, m)
		}
		if err := ms.Err(); err != nil {
			t.Fatalf("iterate family: %v", err)
		}
		ms.Close()
		// Re-read primaries' images.
		for k, pid := range primaries {
			var img sql.NullInt64
			if err := ro.QueryRow(`SELECT image_id FROM files WHERE file_id = ?`, pid).Scan(&img); err != nil {
				t.Fatalf("read primary image: %v", err)
			}
			if img.Valid && img.Int64 == 1 {
				for _, m := range members {
					key := fam{m.dev, m.inode}
					if key == k && m.id != pid {
						membersOfPrimaryOn1 = append(membersOfPrimaryOn1, m.id)
					}
				}
			}
		}
	}

	if len(assignedFiles) == 0 || len(assignedChunks) == 0 || len(mirrorParents) == 0 {
		t.Fatalf("fixture too thin: files %v chunks %v parents %v - the AC-10 assertions would be vacuous",
			assignedFiles, assignedChunks, mirrorParents)
	}
	if len(membersOfPrimaryOn1) == 0 {
		t.Fatal("fixture has no family member of a primary on image 1 - the family half would be vacuous")
	}

	// THE REAL COMMAND.
	stdout, stderr, code := runClearChild(t, "--catalog", cat, "--set-id", setID, "--image", "1")
	if code != report.ExitOK {
		t.Fatalf("clear --image 1 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	// --- ZERO surviving marks describing image 1 ------------------------------
	count := func(query string, args ...any) int {
		t.Helper()
		var n int
		if err := ro.QueryRow(query, args...).Scan(&n); err != nil {
			t.Fatalf("count query %q: %v", query, err)
		}
		return n
	}
	// Assigned rows (files/chunks/directories): zero carry image 1.
	if n := count(`SELECT COUNT(*) FROM files WHERE set_id = ? AND image_id = 1`, setID); n != 0 {
		t.Errorf("files rows still assigned to image 1: %d, want 0", n)
	}
	if n := count(`SELECT COUNT(*) FROM chunks WHERE set_id = ? AND image_id = 1`, setID); n != 0 {
		t.Errorf("chunk rows still assigned to image 1: %d, want 0", n)
	}
	if n := count(`SELECT COUNT(*) FROM directories WHERE set_id = ? AND image_id = 1`, setID); n != 0 {
		t.Errorf("directory rows still assigned to image 1: %d, want 0", n)
	}
	// Chunk rows: every chunk that described image 1 has image NULL,
	// copied 0, verified_at NULL.
	for _, id := range assignedChunks {
		var (
			imageID  sql.NullInt64
			copied   int
			verified sql.NullInt64
		)
		if err := ro.QueryRow(`SELECT image_id, copied, verified_at FROM chunks WHERE chunk_id = ?`, id).
			Scan(&imageID, &copied, &verified); err != nil {
			t.Fatalf("read chunk %d: %v", id, err)
		}
		if imageID.Valid || copied != 0 || verified.Valid {
			t.Errorf("chunk %d still marks image 1: image %v copied %d verified %v",
				id, imageID, copied, verified)
		}
	}
	// Assigned whole files: image NULL, copied 0.
	for _, id := range assignedFiles {
		var imageID sql.NullInt64
		var copied int
		if err := ro.QueryRow(`SELECT image_id, copied FROM files WHERE file_id = ?`, id).
			Scan(&imageID, &copied); err != nil {
			t.Fatalf("read file %d: %v", id, err)
		}
		if imageID.Valid || copied != 0 {
			t.Errorf("file %d still marks image 1: image %v copied %d", id, imageID, copied)
		}
	}
	// The 5.2 parent mirrors: copied 0.
	for _, id := range mirrorParents {
		var copied int
		if err := ro.QueryRow(`SELECT copied FROM files WHERE file_id = ?`, id).Scan(&copied); err != nil {
			t.Fatalf("read parent %d: %v", id, err)
		}
		if copied != 0 {
			t.Errorf("chunk-parent %d kept copied=%d - the mirror describing image 1 survives", id, copied)
		}
	}
	// Active family members of families whose primary was on image 1.
	for _, id := range membersOfPrimaryOn1 {
		var copied int
		if err := ro.QueryRow(`SELECT copied FROM files WHERE file_id = ?`, id).Scan(&copied); err != nil {
			t.Fatalf("read member %d: %v", id, err)
		}
		if copied != 0 {
			t.Errorf("family member %d of a primary on image 1 kept copied=%d", id, copied)
		}
	}
	// The destination row is gone (no future skip source at all).
	if n := count(`SELECT COUNT(*) FROM images WHERE set_id = ? AND image_id = 1`, setID); n != 0 {
		t.Errorf("images row 1 survived: %d", n)
	}
	// And the sibling's marks are NOT collateral damage (scoped, not
	// blanket): image 2 keeps its assigned rows.
	if n := count(`SELECT COUNT(*) FROM files WHERE set_id = ? AND image_id = 2`, setID); n == 0 {
		t.Error("image 2 lost all assigned rows - the unlock was not scoped")
	}
}

// --- AC-15: audit rows ---------------------------------------------------------

// TestClearRunRowCarriesStatsAndUnlockedImages pins the AC-15 audit
// contract through the REAL command: the unlock writes ONE runs row
// (kind 'clear', status 'ok') whose stats_json carries the counts AND
// `unlocked_images` (equal to the --json document - M6/AC-40), while
// PRIOR run rows (seeded build/copy rows incl. their run_events) and
// every event survive untouched.
func TestClearRunRowCarriesStatsAndUnlockedImages(t *testing.T) {
	cat := newPlannedFixture(t, "S-RUNS")
	seedWrittenChunkState(t, cat, "S-RUNS")

	// Seed a prior COPY run row with two run_events (the AC-15 "prior
	// build/copy run rows and their run_events survive untouched"
	// half - build/copy do not run at this tier, so the rows are
	// inserted directly).
	rw := catRW(t, cat)
	res, err := rw.Exec(`INSERT INTO runs (set_id, kind, root, started_at, finished_at, status,
	                                       exclude_json, stats_json, tool_version)
	                     VALUES ('S-RUNS', 'copy', '/mnt/dest', 111, 222, 'ok', '[]',
	                             '{"copied": 5}', 'test')`)
	if err != nil {
		t.Fatalf("seed prior copy run: %v", err)
	}
	priorRun, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("prior copy run id: %v", err)
	}
	for _, ev := range []struct{ rel, detail string }{
		{"a.txt", "copied 8 bytes"},
		{"sub/b.bin", "copied 8 bytes"},
	} {
		if _, err := rw.Exec(`INSERT INTO run_events (run_id, at, level, relpath, detail)
		                      VALUES (?, 333, 'info', ?, ?)`, priorRun, ev.rel, ev.detail); err != nil {
			t.Fatalf("seed run_event %s: %v", ev.rel, err)
		}
	}

	// Snapshots of the prior audit state.
	dumpRuns := func() string {
		t.Helper()
		db := catOpen(t, cat)
		rows, err := db.Query(`SELECT run_id, set_id, kind, root, started_at, finished_at, status, stats_json
		                       FROM runs ORDER BY run_id`)
		if err != nil {
			t.Fatalf("query runs: %v", err)
		}
		defer rows.Close()
		var buf strings.Builder
		for rows.Next() {
			var (
				id                               int64
				setID, kind, root, status, stats string
				started                          int64
				finished                         sql.NullInt64
			)
			if err := rows.Scan(&id, &setID, &kind, &root, &started, &finished, &status, &stats); err != nil {
				t.Fatalf("scan run: %v", err)
			}
			fmt.Fprintf(&buf, "%d|%s|%s|%s|%d|%v|%s|%s\n", id, setID, kind, root, started, finished, status, stats)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate runs: %v", err)
		}
		return buf.String()
	}
	dumpEvents := func() string {
		t.Helper()
		db := catOpen(t, cat)
		rows, err := db.Query(`SELECT e.event_id, e.run_id, e.at, e.level, e.file_id, e.relpath, e.detail
		                       FROM run_events e ORDER BY e.event_id`)
		if err != nil {
			t.Fatalf("query run_events: %v", err)
		}
		defer rows.Close()
		var buf strings.Builder
		for rows.Next() {
			var (
				id, runID, at int64
				level, detail string
				fileID        sql.NullInt64
				rel           sql.NullString
			)
			if err := rows.Scan(&id, &runID, &at, &level, &fileID, &rel, &detail); err != nil {
				t.Fatalf("scan event: %v", err)
			}
			fmt.Fprintf(&buf, "%d|%d|%d|%s|%v|%v|%s\n", id, runID, at, level, fileID, rel, detail)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate events: %v", err)
		}
		return buf.String()
	}
	runsBefore, eventsBefore := dumpRuns(), dumpEvents()

	// THE REAL COMMAND.
	stdout, stderr, code := runClearChild(t, "--catalog", cat, "--set-id", "S-RUNS", "--image", "2", "--json")
	if code != report.ExitOK {
		t.Fatalf("clear --image 2 --json exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	doc := goldenJSONDoc(t, stdout, clearReportJSONKeys)
	runID := int64(doc["run_id"].(float64))
	if runID <= 0 {
		t.Fatalf("run_id = %d, want a positive runs row", runID)
	}

	// The runs row: kind 'clear', status 'ok', stats carrying the
	// counts AND unlocked_images == the document's array.
	var kind, status, stats string
	ro := catOpen(t, cat)
	if err := ro.QueryRow(`SELECT kind, status, stats_json FROM runs WHERE run_id = ?`, runID).
		Scan(&kind, &status, &stats); err != nil {
		t.Fatalf("read clear run row: %v", err)
	}
	if kind != "clear" || status != "ok" {
		t.Errorf("clear run row = %q/%q, want clear/ok", kind, status)
	}
	var cs struct {
		FilesCleared        int     `json:"files_cleared"`
		ChunksCleared       int     `json:"chunks_cleared"`
		DirsCleared         int     `json:"dirs_cleared"`
		ImagesRemoved       int     `json:"images_removed"`
		ContentsCleared     int     `json:"contents_cleared"`
		ChunkHashesRetained int     `json:"chunk_hashes_retained"`
		UnlockedImages      []int64 `json:"unlocked_images"`
	}
	if err := json.Unmarshal([]byte(stats), &cs); err != nil {
		t.Fatalf("stats_json %q: %v", stats, err)
	}
	if cs.FilesCleared != int(doc["files_cleared"].(float64)) ||
		cs.ChunksCleared != int(doc["chunks_cleared"].(float64)) ||
		cs.DirsCleared != int(doc["dirs_cleared"].(float64)) ||
		cs.ImagesRemoved != int(doc["destinations_removed"].(float64)) ||
		cs.ChunkHashesRetained != int(doc["chunk_hashes_retained"].(float64)) {
		t.Errorf("stats_json = %+v, want the --json document's counts %v/%v/%v/%v/%v", cs,
			doc["files_cleared"], doc["chunks_cleared"], doc["dirs_cleared"],
			doc["destinations_removed"], doc["chunk_hashes_retained"])
	}
	if !reflect.DeepEqual(cs.UnlockedImages, []int64{2}) || !reflect.DeepEqual(doc["unlocked_images"], []any{float64(2)}) {
		t.Errorf("stats unlocked_images = %v, document = %v, want [2] in both (M6 agreement)", cs.UnlockedImages, doc["unlocked_images"])
	}

	// PRIOR audit rows untouched: the runs snapshot minus the new clear
	// row equals the before snapshot, and run_events are byte-identical.
	runsAfter := dumpRuns()
	var kept []string
	for _, line := range strings.Split(strings.TrimSuffix(runsAfter, "\n"), "\n") {
		if strings.Contains(line, "|clear|") {
			continue
		}
		kept = append(kept, line)
	}
	if strings.Join(kept, "\n") != strings.TrimSuffix(runsBefore, "\n") {
		t.Errorf("prior runs rows changed:\nbefore:\n%s\nafter:\n%s", runsBefore, strings.Join(kept, "\n"))
	}
	if eventsAfter := dumpEvents(); eventsAfter != eventsBefore {
		t.Errorf("run_events changed across the unlock:\nbefore:\n%s\nafter:\n%s", eventsBefore, eventsAfter)
	}
	// Exactly ONE clear run row.
	var clearRuns int
	if err := ro.QueryRow(`SELECT COUNT(*) FROM runs WHERE kind = 'clear' AND set_id = 'S-RUNS'`).Scan(&clearRuns); err != nil {
		t.Fatalf("count clear runs: %v", err)
	}
	if clearRuns != 1 {
		t.Errorf("clear runs rows = %d, want exactly 1", clearRuns)
	}
}

// --- AC-18(a): --last on a 1-image set -----------------------------------------

// TestClearLastOnSingleImageSetUnlocksImageOne is AC-18(a): on a set
// planned onto exactly ONE destination, `clear --last` unlocks image 1
// with exit 0, the explicit report line, and
// unlocked_images == [1] (both presentations).
func TestClearLastOnSingleImageSetUnlocksImageOne(t *testing.T) {
	// leg A: the human report (the explicit row).
	catA := createCatalog(t)
	scanBuildFixture(t, makeBuildSingleImageTree(t), catA, "S-LAST-A")
	planSynthetic(t, catA, "S-LAST-A", buildFixtureProfile)
	if ids := plannedImageIDs(t, catA, "S-LAST-A"); !reflect.DeepEqual(ids, []int64{1}) {
		t.Fatalf("fixture planned ids = %v, want exactly [1] (the 1-image set)", ids)
	}
	stdout, stderr, code := runClearChild(t, "--catalog", catA, "--set-id", "S-LAST-A", "--last")
	if code != report.ExitOK {
		t.Fatalf("clear --last on a 1-image set exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "unlocked images: 1") {
		t.Errorf("report lacks the explicit 'unlocked images: 1' row:\n%s", stdout)
	}
	if !strings.Contains(stdout, "destinations removed: 1") {
		t.Errorf("report lacks the destinations-removed line:\n%s", stdout)
	}
	if ids := plannedImageIDs(t, catA, "S-LAST-A"); len(ids) != 0 {
		t.Errorf("planned ids after --last = %v, want none (image 1 unlocked)", ids)
	}
	if !strings.Contains(stderr, "clear: cleared") {
		t.Errorf("stderr lacks the summary line:\n%s", stderr)
	}

	// leg B: the --json document carries unlocked_images == [1].
	catB := createCatalog(t)
	scanBuildFixture(t, makeBuildSingleImageTree(t), catB, "S-LAST-B")
	planSynthetic(t, catB, "S-LAST-B", buildFixtureProfile)
	jout, jerr, jcode := runClearChild(t, "--catalog", catB, "--set-id", "S-LAST-B", "--last", "--json")
	if jcode != report.ExitOK {
		t.Fatalf("clear --last --json exit = %d, want 0\nstdout: %s\nstderr: %s", jcode, jout, jerr)
	}
	doc := goldenJSONDoc(t, jout, clearReportJSONKeys)
	if !reflect.DeepEqual(doc["unlocked_images"], []any{float64(1)}) {
		t.Errorf("unlocked_images = %v, want [1] (AC-18(a))", doc["unlocked_images"])
	}
	if doc["destinations_removed"].(float64) != 1 {
		t.Errorf("destinations_removed = %v, want 1", doc["destinations_removed"])
	}
}

// --- AC-09 (CLI leg): SIGINT mid-unlock ----------------------------------------

// runClearSigintCase is the shared AC-09 SIGINT body: ONE real SIGINT
// into a REAL clear child whose unlock transaction is parked at its
// BEGIN IMMEDIATE: the parent holds the SQLite write lock on a raw
// connection (no flock conflict - the flock is a separate,
// already-passed gate; WAL readers stay unaffected), so the child
// blocks INSIDE the one unlock transaction for its whole busy_timeout
// window. The signal then cancels the run context mid-unlock: exit
// 130, no report on stdout, and a BIT-IDENTICAL catalog (all tables +
// no runs row + file bytes). extraArgs selects the mode: none = the
// full clear, "--image 1" = the scoped U.1 unlock (the selector is
// validated pre-transaction, then parks in the SAME transaction).
//
// Timing note (documented judgment call): there is no in-production
// sync point between the child's catalog open and its transaction, so
// the test waits a 3-second margin after spawning. QA widened it from
// the original 2s with this stated rationale: the ONLY failing
// interleaving is a signal delivered before the child arms its signal
// guard (the process would die by signal instead of exiting 130) -
// arming happens milliseconds after exec, so a larger margin strictly
// shrinks that window; and the SIGINT must land inside the child's
// 5-second BEGIN IMMEDIATE busy window (which starts after re-exec +
// startup + the pre-transaction reads, sub-second normally), so any
// margin below 5s still lands mid-park - 3s buys arm-headroom in BOTH
// directions (pre-arm safety AND mid-park likelihood) for one second
// of runtime. Every interleaving the margin can still produce (signal
// after the guard is armed but before the transaction) classifies to
// the same 130 fold in execute(); the per-statement rollback itself
// is pinned by the catalog-tier nine-point trigger matrix.
func runClearSigintCase(t *testing.T, setID string, extraArgs ...string) {
	t.Helper()
	cat := newPlannedFixture(t, setID)
	seedWrittenChunkState(t, cat, setID)

	// Hold the SQLite write lock from the test process (BEGIN IMMEDIATE
	// writes nothing; readers keep working over the WAL).
	held := catRW(t, cat)
	conn, err := held.Conn(context.Background())
	if err != nil {
		t.Fatalf("raw conn for the write lock: %v", err)
	}
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("BEGIN IMMEDIATE (the mid-unlock park): %v", err)
	}
	released := false
	release := func() {
		if released {
			return
		}
		released = true
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		_ = conn.Close()
	}
	t.Cleanup(release)

	// Snapshots taken while the write lock is held (WAL readers are
	// unaffected by an idle writer).
	stateBefore := dumpUnlockStateCLI(t, catOpen(t, cat))
	bytesBefore := fileBytes(t, cat)
	runsBefore := countRunsRows(t, cat)

	// The REAL clear child under the production signal scaffolding
	// (scanSignalHelperMain: the signal guard + execute over the real
	// command tree, args through).
	child := exec.Command(os.Args[0],
		append([]string{"clear-assignments", "--catalog", cat, "--set-id", setID}, extraArgs...)...)
	child.Env = append(os.Environ(), scanSignalHelperEnv+"=1")
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	child.Stdout, child.Stderr = stdout, stderr
	if err := child.Start(); err != nil {
		t.Fatalf("starting clear-signal child: %v", err)
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- child.Wait() }()
	exited := false
	defer func() {
		if !exited {
			_ = child.Process.Kill()
			<-waitCh
		}
	}()

	// The margin: the child is inside BEGIN IMMEDIATE against the held
	// write lock (see the func doc).
	time.Sleep(3 * time.Second)
	if err := child.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("SIGINT to clear child: %v", err)
	}

	var waitErr error
	select {
	case waitErr = <-waitCh:
		exited = true
	case <-time.After(30 * time.Second):
		t.Fatal("the clear child never exited after SIGINT")
	}
	code := report.ExitOK
	if waitErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) {
			t.Fatalf("waiting for the clear child: %v", waitErr)
		}
		code = exitErr.ExitCode()
	}
	release() // free the park before the assertions' connections

	if code != report.ExitInterrupted {
		t.Errorf("clear child exit = %d, want 130 (interrupted class)\nstdout: %s\nstderr: %s",
			code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "interrupted by signal") {
		t.Errorf("stderr lacks the interrupt class wording:\n%s", stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("an interrupted clear must print NO report on stdout, got: %q", stdout.String())
	}
	// THE CATALOG: bit-identical - every table, no runs row, file bytes.
	if stateAfter := dumpUnlockStateCLI(t, catOpen(t, cat)); stateAfter != stateBefore {
		t.Errorf("the interrupted unlock changed the catalog (atomicity violation):\nbefore:\n%s\nafter:\n%s",
			stateBefore, stateAfter)
	}
	if runsAfter := countRunsRows(t, cat); runsAfter != runsBefore {
		t.Errorf("runs rows = %d after the interrupt, want %d (no audit row for a rolled-back unlock)",
			runsAfter, runsBefore)
	}
	if !bytes.Equal(bytesBefore, fileBytes(t, cat)) {
		t.Error("the interrupted unlock changed the catalog file bytes")
	}
}

// TestClearSigintMidUnlockExits130AndLeavesCatalogUnchanged is the
// FULL-clear leg (no selector flags) of the shared body above.
func TestClearSigintMidUnlockExits130AndLeavesCatalogUnchanged(t *testing.T) {
	runClearSigintCase(t, "S-SIGINT")
}

// TestClearScopedSigintMidUnlockExits130AndLeavesCatalogUnchanged is
// the SCOPED U.1 leg: the real command with --image 1 validates the
// selector before the transaction, then parks in the SAME
// one-transaction unlock - the interrupt contract is identical (exit
// 130, NO stdout report including no "unlocked images" line,
// bit-identical catalog with no runs row). The per-statement rollback
// of the scoped sequence itself is pinned separately by the
// catalog-tier nine-point trigger matrix.
func TestClearScopedSigintMidUnlockExits130AndLeavesCatalogUnchanged(t *testing.T) {
	runClearSigintCase(t, "S-SIGINT-SCOPE", "--image", "1")
}

// --- U.1-F1: the superseded-anchor family claim, end to end -------------------

// supersededAnchorFixture builds the U.1-F1 shape through the REAL
// commands: scan, plan, copy, PRODUCTION supersede (rescan ->
// InsertModification), scoped unlock, re-plan, re-copy.
//
// THE SHAPE (the QA's nasty-shapes "G" grown into a full CLI flow):
//
//  1. scan a tree holding a THREE-name hard-link family (one inode,
//     nlink 3) plus two filler files; plan it onto TWO images;
//     fixture-shape the placement so the family's PRIMARY (lowest
//     file_id - the hardLinkRows rule; which NAME wins it is the hash
//     pool's insert order, so the fixture DERIVES it) sits on image 2
//     and the members carry no image_id (the normal D-04 shape);
//  2. copy -> image 2's tree holds the primary WHOLE and both members
//     as link(2); every family row ends up copied=1 (the member marks
//     are written bound to image 2 by LinkSecondary);
//  3. REPLACE the primary's name in the source (same relpath, NEW
//     inode - write-to-temp + rename) and RE-SCAN: the reconciler's
//     MODIFIED outcome runs the PRODUCTION InsertModification pair -
//     the old primary row is superseded while KEEPING image_id=2 and
//     copied=1 (2.4 modification versioning), the two members stay
//     UNCHANGED with their copied=1 marks;
//  4. scoped unlock `clear-assignments --image 2`;
//  5. RE-PLAN, then copy again.
//
// THREE NAMES, not two, is deliberate: after the supersede the active
// family has TWO surviving members, so exactly one of them is the new
// family PRIMARY (lowest file_id) and the other is a surplus
// SECONDARY. A two-name family leaves a single survivor that becomes
// its own primary and heals itself at re-assignment; the surplus
// secondary is the row whose stale mark the ruling (option c) exists
// to clear.
type supersededAnchorFixture struct {
	setID     string
	cat       string
	root      string
	dest      string
	anchorRel string // the name whose row is superseded (the released primary)
	members   []string
	// The two surviving members, after the supersede.
	newPrimaryRel string // lowest file_id of the survivors - re-assigned by the re-plan
	surplusRel    string // the surplus secondary a stale copied=1 could survive on
	allNames      []string
}

func newSupersededAnchorFixture(t *testing.T, setID string) supersededAnchorFixture {
	t.Helper()
	fx := supersededAnchorFixture{setID: setID}
	names := []string{"anchor.txt", "anchorb.txt", "anchorc.txt"}
	fx.allNames = names
	fx.root = t.TempDir()
	writeFileP(t, filepath.Join(fx.root, "anchor.txt"), pattern(7, 512), 0o644)
	for _, name := range names[1:] {
		if err := os.Link(filepath.Join(fx.root, "anchor.txt"), filepath.Join(fx.root, name)); err != nil {
			t.Fatalf("hard link %s: %v", name, err)
		}
	}
	writeFileP(t, filepath.Join(fx.root, "fill1.txt"), pattern(8, 600_000), 0o644)
	writeFileP(t, filepath.Join(fx.root, "fill2.txt"), pattern(9, 600_000), 0o644)

	fx.cat = createCatalog(t)
	scanBuildFixture(t, fx.root, fx.cat, setID)

	// The scan inserts in the hash pool's completion order, so the
	// family's primary (lowest file_id) is NOT necessarily anchor.txt.
	// Derive it: the primary is the row that must sit on image 2 for
	// the D-04 write topology to hold.
	fileIDOf := func(rel string) int64 {
		t.Helper()
		var id int64
		if err := catOpen(t, fx.cat).QueryRow(
			`SELECT file_id FROM files WHERE set_id = ? AND relpath = ?`, setID, rel).
			Scan(&id); err != nil {
			t.Fatalf("file_id of %q: %v", rel, err)
		}
		return id
	}
	ids := map[string]int64{}
	for _, n := range names {
		ids[n] = fileIDOf(n)
	}
	fx.anchorRel = names[0]
	for _, n := range names[1:] {
		if ids[n] < ids[fx.anchorRel] {
			fx.anchorRel = n
		}
	}
	fx.members = []string{}
	for _, n := range names {
		if n != fx.anchorRel {
			fx.members = append(fx.members, n)
		}
	}

	// A synthetic capacity profile is the only way to plan a ~1.2MB
	// tree onto TWO images (the real --size floor is 1 decimal GB);
	// planSynthetic runs the REAL Balance + writeAssignments pipeline.
	const usable = int64(3_000_000) // ~901,824 payload bytes per image
	planSynthetic(t, fx.cat, setID, usable)

	db := catRW(t, fx.cat)
	// Fixture shaping (the AC-03 pattern, not product behavior): the
	// family's primary on image 2, the members unassigned, the fillers
	// on image 1.
	if planned := plannedImageIDs(t, fx.cat, setID); len(planned) != 2 ||
		planned[0] != 1 || planned[1] != 2 {
		t.Fatalf("plan produced images %v, want exactly [1 2]", planned)
	}
	for _, q := range []struct {
		query string
		args  []any
		what  string
	}{
		{`UPDATE files SET image_id = 2 WHERE set_id = ? AND relpath = ?`,
			[]any{setID, fx.anchorRel}, "family primary -> image 2"},
		{`UPDATE files SET image_id = 1 WHERE set_id = ? AND relpath IN ('fill1.txt', 'fill2.txt')`,
			[]any{setID}, "fillers -> image 1"},
		{`UPDATE files SET image_id = NULL WHERE set_id = ? AND relpath IN (?, ?)`,
			[]any{setID, fx.members[0], fx.members[1]}, "members unassigned (D-04)"},
	} {
		if _, err := db.Exec(q.query, q.args...); err != nil {
			t.Fatalf("reshape (%s): %v", q.what, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close reshape connection: %v", err)
	}

	// --- leg 1: copy the seeded plan ------------------------------------------
	fx.dest = filepath.Join(t.TempDir(), "dest")
	stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", fx.cat, "--set-id", setID, "--dest", fx.dest, "--json")
	if code != report.ExitOK {
		t.Fatalf("copy 1 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	img2 := destImageDir(fx.dest, setID, 2)
	for _, name := range names {
		if _, err := os.Lstat(filepath.Join(img2, "files", name)); err != nil {
			t.Fatalf("copy 1 did not write %s on image 2: %v", name, err)
		}
	}

	// --- THE PRODUCTION SUPERSEDE FLOW (same relpath, new inode) --------------
	writeFileP(t, filepath.Join(fx.root, fx.anchorRel+".new"), pattern(7, 513), 0o644)
	if err := os.Rename(filepath.Join(fx.root, fx.anchorRel+".new"), filepath.Join(fx.root, fx.anchorRel)); err != nil {
		t.Fatalf("replacing %s (the rescan modification): %v", fx.anchorRel, err)
	}
	scanBuildFixture(t, fx.root, fx.cat, setID)

	// The seeded shape: the superseded primary keeps image 2 + copied 1;
	// both members keep copied 1 with no image_id of their own.
	type row struct {
		fileID     int64
		status     string
		superseded bool
		image      any
		copied     int
	}
	read := func(rel string) row {
		t.Helper()
		var r row
		var sup any
		if err := catOpen(t, fx.cat).QueryRow(
			`SELECT file_id, status, superseded_by, image_id, copied FROM files
			  WHERE set_id = ? AND relpath = ?`, setID, rel).
			Scan(&r.fileID, &r.status, &sup, &r.image, &r.copied); err != nil {
			t.Fatalf("read row %q: %v", rel, err)
		}
		r.superseded = sup != nil
		return r
	}
	if r := read(fx.anchorRel); r.status != "active" || !r.superseded || r.image != int64(2) || r.copied != 1 {
		t.Fatalf("superseded primary row %s = %+v, want (active, superseded, image 2, copied 1)", fx.anchorRel, r)
	}
	for _, name := range fx.members {
		if r := read(name); r.status != "active" || r.superseded || r.image != nil || r.copied != 1 {
			t.Fatalf("member %s = %+v, want (active, not superseded, image NULL, copied 1)", name, r)
		}
	}
	a, b := read(fx.members[0]), read(fx.members[1])
	if a.fileID < b.fileID {
		fx.newPrimaryRel, fx.surplusRel = fx.members[0], fx.members[1]
	} else {
		fx.newPrimaryRel, fx.surplusRel = fx.members[1], fx.members[0]
	}
	t.Logf("SEEDED SHAPE OK: superseded primary %s carries image 2 + copied 1; both members carry copied 1 with no image_id; new family primary = %s, surplus secondary = %s",
		fx.anchorRel, fx.newPrimaryRel, fx.surplusRel)
	return fx
}

// fileState reads one files row's (image_id, copied).
func fileState(t *testing.T, cat, setID, rel string) (image any, copied int) {
	t.Helper()
	if err := catOpen(t, cat).QueryRow(
		`SELECT image_id, copied FROM files WHERE set_id = ? AND relpath = ?`, setID, rel).
		Scan(&image, &copied); err != nil {
		t.Fatalf("read %q: %v", rel, err)
	}
	return image, copied
}

// seedImageContentRow inserts ONE image_contents row for an image: the
// test-side stand-in for a PARTIALLY-RESUMED generation, which is the
// exact state gate step 1 keys on (DESIGN_SPEC 4.6.1: contents
// non-empty -> the wipe gate returns without touching the tree). The
// gate reads ONLY this list, so one row IS the bypass condition -
// production SQL stays inside internal/catalog; this is the established
// tests-only SQL seam (unlock_replan_test's UPDATE pattern).
func seedImageContentRow(t *testing.T, cat, setID string, imageID int64, mediaPath string) {
	t.Helper()
	if _, err := catRW(t, cat).Exec(
		`INSERT INTO image_contents (set_id, image_id, media_path, kind, size) VALUES (?, ?, ?, 'file', 0)`,
		setID, imageID, mediaPath); err != nil {
		t.Fatalf("seeding the image_contents row (the resumed-generation stand-in): %v", err)
	}
}

// shaOfHex is the content fingerprint of a file (or an error marker).
func shaOfHex(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return "MISSING:" + err.Error()
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func slicesContains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// TestClearScopedUnlockSupersededAnchorFamilyClaim is the U.1-F1
// failure-chain reconcile probe promoted to a real test at the CLI
// tier (the tier runnable TODAY - no e2e tag, no xorriso): REAL scan,
// REAL InsertModification supersede, REAL scoped unlock, REAL
// re-plan, REAL copy, REAL verify-media, REAL media-alone restore.
//
// THE RECONCILE IT RECORDS (Step 0 ran this shape against the
// PRE-fix code and produced the verdicts quoted below):
//
//   - the QA's chain ("the stale mark ends in verify-media exit 10")
//     was REFUTED: verify-media anchors on image_contents, a skipped
//     member leaves NO content row, so verify was CLEAN - exit 0,
//     image 2 stamped verified - while MANIFEST.json's Entries claimed
//     the absent member. The MANIFEST (not verify) was the artifact
//     that over-claimed;
//   - the architect's second half ("the loud failure surfaces at
//     media-alone restore") was ALSO refuted by the run: the restore
//     exited 0, because the manifest's hard_link_groups (D-04
//     reconstruction metadata) recreated the absent member as a link
//     of the primary with the right bytes;
//   - the ONE leg that did exit 10 pre-fix was the unwashed-media
//     variant below, and for the OPPOSITE signature ("unexpected file
//     in extraction" - the stale leftover - not a missing claimed
//     member).
//
// Post-fix the member is written for real: image 2's already_copied is
// 0, and the tree, the manifest Entries and image_contents all agree.
//
// U.3 RECONCILIATION (the wipe gate landed): the probe's three legs
// now pin the THREE honest shapes of the released-media question:
//
//   - "released media replaced" - the tree is gone before the re-copy
//     (the old operator-wipes-it fixture shape): the gate's ENOENT
//     branch runs, nothing is wiped, the rewrite is a fresh write;
//   - "released media left in place" - THE NORMAL FLOW NOW: the gate
//     owns the tree (run 1 wrote a matching MANIFEST.json for this set
//   - image 2), so copy REMOVES it loudly (all four accounting
//     channels) and rewrites the image to the SAME clean outcome -
//     exit 0, already_copied 0, manifest written. The U.1 leg's loud
//     clobber refusal is SUPERSEDED here exactly as its comment
//     promised ("U.3 removes this case from the normal flow");
//   - "gate bypassed on a non-empty content list" - what STILL
//     applies: gate step 1 returns before any filesystem effect when
//     image_contents is non-empty, so a partially-resumed generation
//     still meets the stale entries and the writer's clobber guard
//     still refuses LOUD (exit 10, nothing clobbered). That state is
//     seeded with ONE image_contents row (tests-only SQL) - the gate
//     reads exactly that list, so one row IS the bypass condition.
func TestClearScopedUnlockSupersededAnchorFamilyClaim(t *testing.T) {
	t.Run("released media replaced (fresh image-2 tree)", func(t *testing.T) {
		runSupersededAnchorProbe(t, "S-SUPER-A", probeWashBeforeCopy)
	})
	t.Run("released media left in place (the wipe gate removes it)", func(t *testing.T) {
		runSupersededAnchorProbe(t, "S-SUPER-B", probeWipeGate)
	})
	t.Run("gate bypassed (non-empty content list keeps the loud clobber)", func(t *testing.T) {
		runSupersededAnchorProbe(t, "S-SUPER-C", probeGateBypass)
	})
}

// anchorMediaMode selects what the released image's re-copy MEETS on
// disk - the three dispositions of the U.3 wipe-gate reconciliation.
type anchorMediaMode int

const (
	// probeWashBeforeCopy: the test removed the released tree (the
	// pre-U.3 operator-wipes-it shape); the gate finds no path.
	probeWashBeforeCopy anchorMediaMode = iota
	// probeWipeGate: the released tree survives the unlock; the gate's
	// ownership check passes (OURS) and the wipe removes it loudly -
	// the NORMAL flow with U.3 in place.
	probeWipeGate
	// probeGateBypass: the released tree survives AND image_contents is
	// non-empty, so gate step 1 returns without touching it - the
	// writer meets the stale entries and the clobber guard refuses.
	probeGateBypass
)

func runSupersededAnchorProbe(t *testing.T, setID string, mode anchorMediaMode) {
	fx := newSupersededAnchorFixture(t, setID)

	// --- leg 2: the scoped unlock of image 2 ----------------------------------
	stdout, stderr, code := runClearChild(t, "--catalog", fx.cat, "--set-id", setID, "--image", "2")
	if code != report.ExitOK {
		t.Fatalf("clear --image 2 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	t.Logf("CLEAR: %s", strings.Join(strings.Fields(stdout), " "))
	// THE FIX AT THE CLI TIER: both surviving members are unmarked -
	// the superseded primary's historical image-2 claim triggers the
	// unmark (the historical-anchor rule), and S3 releases that row
	// itself.
	for _, name := range fx.members {
		image, copied := fileState(t, fx.cat, setID, name)
		if image != nil || copied != 0 {
			t.Errorf("POST-UNLOCK member %s = image %v copied %d, want NULL/0 (the historical-anchor claim must unmark it)",
				name, image, copied)
		}
	}
	if image, copied := fileState(t, fx.cat, setID, fx.anchorRel); image != nil || copied != 0 {
		t.Errorf("POST-UNLOCK superseded primary %s = image %v copied %d, want NULL/0 (S3's any-status blanket)",
			fx.anchorRel, image, copied)
	}

	// --- leg 3: re-plan -------------------------------------------------------
	planSynthetic(t, fx.cat, setID, 3_000_000)
	planned := plannedImageIDs(t, fx.cat, setID)
	t.Logf("RE-PLAN images: %v", planned)
	if len(planned) != 2 || planned[0] != 1 || planned[1] != 2 {
		t.Fatalf("re-plan produced images %v, want [1 2] (the old balancer re-mints the released id)", planned)
	}
	// The new family PRIMARY is re-assigned (AssignFile resets copied
	// to 0); the surplus secondary stays unassigned with copied 0 -
	// there is no stale mark left to skip on.
	if image, copied := fileState(t, fx.cat, setID, fx.newPrimaryRel); image != int64(2) || copied != 0 {
		t.Errorf("re-planned primary %s = image %v copied %d, want image 2 / copied 0", fx.newPrimaryRel, image, copied)
	}
	if image, copied := fileState(t, fx.cat, setID, fx.surplusRel); image != nil || copied != 0 {
		t.Errorf("surplus secondary %s = image %v copied %d, want NULL / 0 (no stale mark survives the fix)",
			fx.surplusRel, image, copied)
	}

	// The released image's media: what this leg's re-copy MEETS on disk
	// (the U.3 wipe-gate reconciliation - see the test doc above).
	img2 := destImageDir(fx.dest, setID, 2)
	var staleBefore os.FileInfo // the refused entry's pre-copy identity (the "nothing clobbered" pin)
	switch mode {
	case probeWashBeforeCopy:
		if err := os.RemoveAll(img2); err != nil {
			t.Fatalf("removing the released image-2 media: %v", err)
		}
	case probeWipeGate:
		// Left in place: run 1 wrote MANIFEST.json for THIS set and
		// image 2, so the ownership check passes as OURS and the gate
		// removes the tree (loudly) before the rewrite. Nothing to do.
		//
		// OWNERSHIP HONESTY (QA): prove this leg is the step-3
		// OWNERSHIP-MATCH wipe ("parses, matches -> OURS"), not the
		// manifest-less ours-by-namespace row - otherwise the leg
		// would pass for the wrong reason.
		mb, err := os.ReadFile(filepath.Join(img2, media.ManifestFileName))
		if err != nil {
			t.Fatalf("leg B needs the run-1 MANIFEST.json in place: %v", err)
		}
		m, perr := media.ParseManifest(mb)
		if perr != nil {
			t.Fatalf("leg B's MANIFEST.json does not parse (the wipe would then be a different ownership row): %v", perr)
		}
		if m.SetID != setID || m.ImageNumber != 2 {
			t.Fatalf("leg B's MANIFEST.json names set %q image %d, want set %q image 2 (the wipe must come from the OWNERSHIP-MATCH row, not ours-by-namespace)",
				m.SetID, m.ImageNumber, setID)
		}
	case probeGateBypass:
		// Left in place AND a non-empty content list: gate step 1
		// returns before any filesystem effect - the stale tree
		// survives into the writer exactly as it did pre-U.3.
		seedImageContentRow(t, fx.cat, setID, 2, "files/"+fx.newPrimaryRel)
		var err error
		if staleBefore, err = os.Lstat(filepath.Join(img2, "files", fx.surplusRel)); err != nil {
			t.Fatalf("stale refused entry %s: %v", fx.surplusRel, err)
		}
	}

	// --- leg 4: the re-copy ---------------------------------------------------
	stdout, stderr, code = runCopyChild(t, nil,
		"copy", "--catalog", fx.cat, "--set-id", setID, "--dest", fx.dest, "--json")

	if mode == probeGateBypass {
		// THE BYPASSED GATE (what still applies of the U.1 policy): a
		// non-empty image_contents list means the gate never touches
		// the tree, so a partially-resumed generation still meets the
		// stale run-1 entries: the writer's clobber guard refuses to
		// replace a file that does not share the new primary's inode -
		// LOUD (exit 10), naming the path - and nothing under the
		// released tree is modified.
		if code == report.ExitOK {
			t.Fatalf("copy over a bypassed gate exited 0, want the loud clobber refusal\nstdout: %s", stdout)
		}
		if code != report.ExitItemFailures {
			t.Errorf("copy over a bypassed gate exit = %d, want %d (the loud clobber refusal, not some other failure)",
				code, report.ExitItemFailures)
		}
		if !strings.Contains(stderr, "exists and does not share the primary's inode") {
			t.Errorf("copy refusal stderr lacks the clobber-guard wording:\n%s", stderr)
		}
		var bypass copyReport
		if err := json.Unmarshal([]byte(stdout), &bypass); err != nil {
			t.Fatalf("copy --json does not parse: %v\n%s", err, stdout)
		}
		if len(bypass.Wipes) != 0 {
			t.Errorf("the bypassed gate recorded wipes %+v, want none (step 1 returns before any filesystem effect)",
				bypass.Wipes)
		}
		if n := countWarnEvents(t, catOpen(t, fx.cat), newestRunID(t, catOpen(t, fx.cat), "copy"),
			"wiped before rewrite"); n != 0 {
			t.Errorf("%d wipe run_events rows on the bypassed gate, want 0", n)
		}
		// NOTHING was clobbered: the refused entry still IS the run-1
		// inode - LinkSecondary refused BEFORE touching it (the
		// writer's "never clobber unowned data" doctrine, specials.go).
		staleAfter, err := os.Lstat(filepath.Join(img2, "files", fx.surplusRel))
		if err != nil {
			t.Fatalf("refused entry vanished (the refusal must touch nothing): %v", err)
		}
		if !os.SameFile(staleBefore, staleAfter) {
			t.Errorf("refused entry inode changed: before %+v, after %+v - nothing may be clobbered",
				staleBefore, staleAfter)
		}
		// Image 2 is honestly INCOMPLETE with its manifest deferred.
		seen := false
		for _, img := range bypass.Images {
			if img.Image != 2 {
				continue
			}
			seen = true
			if img.Complete || img.Manifest {
				t.Errorf("image 2 report = %+v, want complete=false manifest=false (refused image stays incomplete)", img)
			}
		}
		if !seen {
			t.Errorf("copy report has no image-2 entry: %+v", bypass.Images)
		}
		t.Logf("COPY (gate bypassed) exit = %d: %s", code, strings.Join(strings.Fields(stderr), " "))
		return
	}

	// The NORMAL flow: both remaining legs converge on a clean rewrite.
	if code != report.ExitOK {
		t.Fatalf("copy 2 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// THE WIPE GATE'S OBSERVABLE (channel 2 + channel 3): the released
	// image's removal is recorded iff the tree was there to remove.
	var rep copyReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("copy 2 --json does not parse: %v\n%s", err, stdout)
	}
	switch mode {
	case probeWipeGate:
		if len(rep.Wipes) != 1 {
			t.Fatalf("wipes = %+v, want exactly the image-2 removal", rep.Wipes)
		}
		w := rep.Wipes[0]
		if w.Image != 2 || w.Path != img2 || w.EntriesRemoved < 1 || w.BytesRemoved < 1 {
			t.Errorf("wipe record = %+v, want image 2 / path %s / entries >= 1 / bytes >= 1", w, img2)
		}
		if n := countWarnEvents(t, catOpen(t, fx.cat), newestRunID(t, catOpen(t, fx.cat), "copy"),
			"wiped before rewrite"); n != 1 {
			t.Errorf("wipe run_events rows = %d, want 1 (channel 3)", n)
		}
		if !strings.Contains(stderr, "removed") || !strings.Contains(stderr, "before rewrite") {
			t.Errorf("stderr lacks the live wipe Warn line (channel 4):\n%s", stderr)
		}
	case probeWashBeforeCopy:
		if len(rep.Wipes) != 0 {
			t.Errorf("wipes = %+v, want none (the released tree was already gone - ENOENT branch)", rep.Wipes)
		}
	}
	// THE NO-SKIP PIN: image 2 re-wrote everything - nothing was
	// trusted-skip on the released placement.
	seenImage2 := false
	for _, img := range rep.Images {
		if img.Image != 2 {
			continue
		}
		seenImage2 = true
		if img.AlreadyCopied != 0 || img.Copied != 3 || !img.Complete || !img.Manifest {
			t.Errorf("copy 2 image 2 report = %+v, want copied 3 / already_copied 0 / complete / manifest (nothing skipped)",
				img)
		}
	}
	if !seenImage2 {
		t.Errorf("copy 2 report has no image-2 entry: %+v", rep.Images)
	}
	t.Logf("COPY 2: %s", strings.Join(strings.Fields(stdout), " "))

	manifest := readManifest(t, filepath.Join(img2, "MANIFEST.json"), setID, 2)
	claimed := []string{}
	for _, e := range manifest.Entries {
		claimed = append(claimed, e.MediaPath)
	}
	sort.Strings(claimed)
	tree := []string{}
	entries, err := os.ReadDir(filepath.Join(img2, "files"))
	if err != nil {
		t.Fatalf("read image-2 tree: %v", err)
	}
	for _, e := range entries {
		tree = append(tree, "files/"+e.Name())
	}
	sort.Strings(tree)
	rows, err := catOpen(t, fx.cat).Query(`SELECT media_path FROM image_contents
	    WHERE set_id = ? AND image_id = 2 ORDER BY media_path`, setID)
	if err != nil {
		t.Fatalf("read image_contents: %v", err)
	}
	contents := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatalf("scan image_contents: %v", err)
		}
		contents = append(contents, p)
	}
	rows.Close()
	t.Logf("IMAGE 2 MANIFEST claims: %v", claimed)
	t.Logf("IMAGE 2 tree holds:       %v", tree)
	t.Logf("IMAGE 2 image_contents:   %v", contents)
	// THE HONESTY CHAIN: manifest Entries == tree == image_contents for
	// the three family paths (the over-claim the pre-fix run recorded).
	for _, n := range fx.allNames {
		w := "files/" + n
		if !slicesContains(claimed, w) {
			t.Errorf("MANIFEST does not claim %s: %v", w, claimed)
		}
		if !slicesContains(tree, w) {
			t.Errorf("image-2 tree does not hold %s: %v", w, tree)
		}
		if !slicesContains(contents, w) {
			t.Errorf("image_contents does not record %s: %v", w, contents)
		}
	}

	// --- leg 5: verify-media --------------------------------------------------
	stdout, stderr, code = runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", fx.cat, "--set-id", setID)
	if code != report.ExitOK {
		t.Errorf("verify-media exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "image 2: verified") {
		t.Errorf("verify-media stdout lacks the image-2 verified line:\n%s", stdout)
	}
	t.Logf("VERIFY-MEDIA exit = %d (clean for the right reason: every claimed member is on the media)", code)

	// --- leg 6: the MEDIA-ALONE restore ---------------------------------------
	target := restoreTarget(t)
	stdout, stderr, code = runInProcess(t, "restore",
		"--from-media", fx.dest, "--target", target, "--work-dir", tempWorkDir(t))
	if code != report.ExitOK {
		t.Fatalf("media-alone restore exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// All three family names come back with the RIGHT bytes; the two
	// SURVIVING members are links of the new primary (D-04 topology) -
	// the superseded name is its own inode again, exactly as the
	// source now has it.
	wantNew := shaOfHex(t, filepath.Join(fx.root, fx.anchorRel))
	wantOld := shaOfHex(t, filepath.Join(fx.root, fx.members[0]))
	primInfo, primErr := os.Lstat(filepath.Join(target, fx.newPrimaryRel))
	if primErr != nil {
		t.Fatalf("restored primary %s: %v", fx.newPrimaryRel, primErr)
	}
	for _, name := range fx.allNames {
		want := wantOld
		if name == fx.anchorRel {
			want = wantNew
		}
		if got := shaOfHex(t, filepath.Join(target, name)); got != want {
			t.Errorf("restored %s content = %s, want %s", name, got, want)
		}
	}
	for _, name := range fx.members {
		info, err := os.Lstat(filepath.Join(target, name))
		if err != nil {
			t.Fatalf("restored %s: %v", name, err)
		}
		if !os.SameFile(info, primInfo) {
			t.Errorf("restored %s is not a hard link of %s (D-04 topology lost)", name, fx.newPrimaryRel)
		}
	}
	t.Logf("MEDIA-ALONE RESTORE exit = 0, all three family names byte-exact with the D-04 link topology")
}
