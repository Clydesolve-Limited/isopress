package cli

// T2.3 end-to-end acceptance tests for the real `plan` command
// (IMPLEMENTATION_PLAN T2.3; DESIGN_SPEC 4.3 balancing and media
// count, 4.4 D-05 waste quantification, 4.5 single-transaction
// assignment, 6.5 margin echo, 10.2 exit codes, 10.3 stream
// separation). Every test drives the REAL production entry path: a
// re-exec child running execute() over the real command tree (the
// scan_test.go pattern). The catalog side is asserted with the SQLite
// driver directly (tests only - production SQL stays inside
// internal/catalog).
//
// The fixture deliberately matches the T2.3 brief: mixed sizes
// including ONE OVERSIZED file (a sparse 1.05 GB logical file - cheap
// to create, and the planner cares about logical size), a symlink, and
// an empty directory, all scanned first so plan operates on real
// catalog state. With --size 1 (disk profile: nominal 1 GB, usable
// 980 MB) the oversized file chunks under the 2026-10-20 capacity
// model into 977,901,312 + 72,098,688 bytes (the effective chunk size
// is usable - reserved overhead = 977,901,312, and chunk 0's charge
// exactly fills image 1's payload budget), which exercises the
// mandatory-only chunker (FR-041), the two-destination media count
// (FR-008/009), the lowest-destination-with-slack rule for empty dirs
// (4.3 as amended - image 1 is exactly packed, so the dir's entry
// falls to image 2), and - on the incremental re-run - the locked
// semantics and the amended D-05 residual figure on LOCKED
// destinations.

import (
	"bytes"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"isopress/internal/report"
)

// planFixtureUsableBytes is the usable capacity of the fixture plan:
// --size 1 under the disk profile = 98% of 1 GB (units.ResolveProfile,
// D-06). The oversized fixture file is bigger than this, so it MUST
// chunk; under the capacity model its decomposition is 977,901,312 +
// 72,098,688 bytes (effective chunk size = usable - FixedImageOverhead
// - ImageEntryOverhead). The constants are UNTYPED so the same
// literals serve int64 catalog assertions and float64 --json
// assertions.
const (
	planFixtureSize    = "1" // --size argument: bare number = decimal GB (4.1)
	planFixtureNominal = 1_000_000_000
	planFixtureUsable  = 980_000_000 // 98% of 1 GB
	planFixtureBig     = 1_050_000_000
	// planFixtureChunk0 is the effective chunk size under the fixture
	// profile: 980,000,000 - 2,098,688 (the 2026-10-20 reserved
	// overhead). Chunk 0's charge (chunk0 + 512) exactly fills image
	// 1's payload budget (980,000,000 - 2,098,176).
	planFixtureChunk0 = 977_901_312
)

// makePlanFixture builds the mixed-size fixture tree: two small files,
// one oversized SPARSE file (1.05 GB logical - creating it is
// instant, hashing its holes reads zeros quickly), a symlink, and an
// empty directory.
func makePlanFixture(t *testing.T) string {
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
	write("a.txt", "hello a\n")    // 8 bytes
	write("sub/b.bin", "binary-b") // 8 bytes
	big := filepath.Join(root, "big.dat")
	f, err := os.Create(big)
	if err != nil {
		t.Fatalf("create big.dat: %v", err)
	}
	if err := f.Truncate(planFixtureBig); err != nil { // sparse, zeros
		t.Fatalf("truncate big.dat: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close big.dat: %v", err)
	}
	if err := os.Symlink("a.txt", filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatalf("mkdir empty: %v", err)
	}
	return root
}

// runPlanChild / runReportChild run the real commands as child
// processes.
func runPlanChild(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	return runCLIChild(t, append([]string{"plan"}, args...)...)
}

func runReportChild(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	return runCLIChild(t, append([]string{"report"}, args...)...)
}

// scanPlanFixture scans the fixture into a fresh catalog under setID
// and returns the catalog path.
func scanPlanFixture(t *testing.T, root, cat, setID string) {
	t.Helper()
	if stdout, stderr, code := runScanChild(t, "--catalog", cat, "--set-id", setID, root); code != report.ExitOK {
		t.Fatalf("scan fixture exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
}

// fileBytes reads a file for the byte-identical catalog comparisons.
func fileBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

// destFillRows reads the computed fill per destination from the
// catalog (files + chunks + directories sums per image_id), the
// assertion shape for "per-destination fill within usable".
type destFillRow struct {
	imageID   int64
	usedBytes int64
	items     int
	dirs      int
}

func queryDestFill(t *testing.T, db *sql.DB, setID string) map[int64]destFillRow {
	t.Helper()
	out := map[int64]destFillRow{}
	add := func(query, what string) {
		t.Helper()
		rows, err := db.Query(query, setID)
		if err != nil {
			t.Fatalf("query %s fill: %v", what, err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				imageID int64
				nbytes  sql.NullInt64
				count   int
			)
			if err := rows.Scan(&imageID, &nbytes, &count); err != nil {
				t.Fatalf("scan %s fill: %v", what, err)
			}
			r := out[imageID]
			r.imageID = imageID
			r.usedBytes += nbytes.Int64
			r.items += count
			out[imageID] = r
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate %s fill: %v", what, err)
		}
	}
	add(`SELECT image_id, SUM(size), COUNT(*) FROM files
	     WHERE set_id = ? AND image_id IS NOT NULL GROUP BY image_id`, "file")
	add(`SELECT image_id, SUM(length), COUNT(*) FROM chunks
	     WHERE set_id = ? AND image_id IS NOT NULL GROUP BY image_id`, "chunk")
	rows, err := db.Query(`SELECT image_id, COUNT(*) FROM directories
	     WHERE set_id = ? AND image_id IS NOT NULL GROUP BY image_id`, setID)
	if err != nil {
		t.Fatalf("query dir fill: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var imageID int64
		var count int
		if err := rows.Scan(&imageID, &count); err != nil {
			t.Fatalf("scan dir fill: %v", err)
		}
		r := out[imageID]
		r.imageID = imageID
		r.dirs = count
		out[imageID] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate dir fill: %v", err)
	}
	return out
}

// --- the acceptance centerpiece ------------------------------------------------

// TestPlanE2EAssignsAndPersists drives the full scan -> plan flow and
// asserts every T2.3 acceptance criterion that lives on the plan side:
// exit 0; the human report on stdout (margin echo, media count, fill
// percentage format, residual line) with the progress summary on
// stderr (FR-046); images rows created with the D-06 profile figures;
// files/symlinks/chunks/directories assigned; per-destination fill
// within usable; the report's media count MATCHING the actual images
// row count; the frozen --json key set; and the T2.2 no-op contract -
// a re-run reports "nothing to plan" and creates zero new destinations.
func TestPlanE2EAssignsAndPersists(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-PLAN")

	// --- first leg: --json (fresh set, work to do) ---
	stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", "S-PLAN", "--size", planFixtureSize, "--json")
	if code != report.ExitOK {
		t.Fatalf("plan --json exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// Progress renders on stderr even under --json (FR-046).
	if !strings.Contains(stderr, "plan: media 2") {
		t.Errorf("stderr lacks the plan summary line:\n%s", stderr)
	}
	doc := decodeSingleJSONDoc(t, stdout)
	// The frozen --json key set (the pin lives in planreport_test.go;
	// this asserts the REAL command emits it).
	wantKeys := append([]string(nil), planReportJSONKeys...)
	sort.Strings(wantKeys)
	if got := sortedKeys(doc); !sameStrings(got, wantKeys) {
		t.Fatalf("plan --json key set = %v\nwant exactly %v (schema freeze)", got, wantKeys)
	}
	if doc["mode"] != "plan" || doc["set_id"] != "S-PLAN" {
		t.Errorf("json identity = %v/%v, want plan/S-PLAN", doc["mode"], doc["set_id"])
	}
	// The profile margin echo (D-06, 6.5) in machine form.
	if doc["media_profile"] != "disk" ||
		doc["nominal_size"].(float64) != planFixtureNominal ||
		doc["usable_size"].(float64) != planFixtureUsable ||
		doc["margin_percent"].(float64) != 2 {
		t.Errorf("json profile echo = %v/%v/%v/%v, want disk/%d/%d/2",
			doc["media_profile"], doc["nominal_size"], doc["usable_size"],
			doc["margin_percent"], planFixtureNominal, planFixtureUsable)
	}
	// FR-008/009 media count: 2 destinations, both new; the per-kind
	// assignment counts; nothing locked, hence zero residual.
	if doc["media_count"].(float64) != 2 || doc["new_destinations"].(float64) != 2 {
		t.Errorf("json media count = %v new %v, want 2/2", doc["media_count"], doc["new_destinations"])
	}
	if doc["assigned_files"].(float64) != 2 || doc["assigned_chunks"].(float64) != 2 ||
		doc["assigned_symlinks"].(float64) != 1 || doc["assigned_dirs"].(float64) != 1 {
		t.Errorf("json assignments = files %v chunks %v symlinks %v dirs %v, want 2/2/1/1",
			doc["assigned_files"], doc["assigned_chunks"],
			doc["assigned_symlinks"], doc["assigned_dirs"])
	}
	if doc["residual_bytes"].(float64) != 0 || doc["residual_images"].(float64) != 0 {
		t.Errorf("json residual = %v across %v, want 0 across 0 (no locked destinations yet)",
			doc["residual_bytes"], doc["residual_images"])
	}
	if doc["nothing_to_plan"] != false {
		t.Errorf("json nothing_to_plan = %v, want false on the first plan", doc["nothing_to_plan"])
	}
	dests, ok := doc["destinations"].([]any)
	if !ok || len(dests) != 2 {
		t.Fatalf("json destinations = %v, want 2 entries", doc["destinations"])
	}
	for i, d := range dests {
		dest := d.(map[string]any)
		wantKeys := append([]string(nil), destFillJSONKeys...)
		sort.Strings(wantKeys)
		if got := sortedKeys(dest); !sameStrings(got, wantKeys) {
			t.Errorf("destination %d key set = %v, want %v", i, got, wantKeys)
		}
		if dest["locked"] != false {
			t.Errorf("destination %d locked = %v, want false (first plan)", i, dest["locked"])
		}
	}

	// --- catalog assertions over the PERSISTED plan ---
	db := catOpen(t, cat)

	// images rows: 2, sequential, D-06 figures, state 'planned' (2.3).
	var images int
	if err := db.QueryRow(`SELECT COUNT(*) FROM images WHERE set_id = 'S-PLAN'`).Scan(&images); err != nil {
		t.Fatalf("count images: %v", err)
	}
	if images != 2 {
		t.Errorf("images rows = %d, want 2 (the report's media count)", images)
	}
	rows, err := db.Query(`SELECT image_id, nominal_size, usable_size, media_profile, state
	                       FROM images WHERE set_id = 'S-PLAN' ORDER BY image_id`)
	if err != nil {
		t.Fatalf("query images: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var (
			id              int64
			nominal, usable int64
			profile, state  string
		)
		if err := rows.Scan(&id, &nominal, &usable, &profile, &state); err != nil {
			t.Fatalf("scan image row: %v", err)
		}
		ids = append(ids, id)
		if nominal != planFixtureNominal || usable != planFixtureUsable ||
			profile != "disk" || state != "planned" {
			t.Errorf("image %d = nominal %d usable %d profile %s state %s, want %d/%d/disk/planned",
				id, nominal, usable, profile, state, planFixtureNominal, planFixtureUsable)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate images: %v", err)
	}
	if len(ids) != 2 || ids[0] != 1 || ids[1] != 2 {
		t.Errorf("image ids = %v, want [1 2] (1-based sequential, 6.4)", ids)
	}

	// Whole items: both small files and the symlink carry an
	// assignment; big.dat stays files.image_id NULL (chunked files are
	// assigned via chunks.image_id ONLY, 4.5).
	for _, tc := range []struct {
		rel      string
		assigned bool
	}{
		{"a.txt", true}, {"sub/b.bin", true}, {"link", true}, {"big.dat", false},
	} {
		rows := queryFiles(t, db, tc.rel)
		if len(rows) != 1 {
			t.Fatalf("%s rows = %d, want 1", tc.rel, len(rows))
		}
		if got := rows[0].ImageID.Valid; got != tc.assigned {
			t.Errorf("%s image_id valid = %v, want %v", tc.rel, got, tc.assigned)
		}
	}

	// big.dat's chunk state: 2 contiguous chunks of 977,901,312 +
	// 72,098,688 bytes (the effective chunk size is usable minus the
	// reserved overhead), BOTH assigned (FR-041: never partially
	// planned), stamped with the effective chunk size (4.2 boundary
	// stability).
	chunks := catChunkRows(t, db, "S-PLAN", "big.dat")
	if len(chunks) != 2 {
		t.Fatalf("big.dat chunks = %d (%+v), want 2", len(chunks), chunks)
	}
	wantLengths := []int64{planFixtureChunk0, planFixtureBig - planFixtureChunk0}
	var offset int64
	for i, c := range chunks {
		if c.index != i || c.offset != offset || c.length != wantLengths[i] || !c.assigned {
			t.Errorf("chunk %d = %+v, want index %d offset %d length %d assigned",
				i, c, i, offset, wantLengths[i])
		}
		offset += c.length
	}
	var chunked, chunkCount int
	var chunkSize int64
	if err := db.QueryRow(`SELECT chunked, chunk_count, chunk_size FROM files
	                       WHERE set_id = 'S-PLAN' AND relpath = 'big.dat'`).
		Scan(&chunked, &chunkCount, &chunkSize); err != nil {
		t.Fatalf("read big.dat chunk stamp: %v", err)
	}
	if chunked != 1 || chunkCount != 2 || chunkSize != planFixtureChunk0 {
		t.Errorf("big.dat chunk stamp = chunked %d count %d size %d, want 1/2/%d",
			chunked, chunkCount, chunkSize, planFixtureChunk0)
	}

	// The empty directory lands on the LOWEST-NUMBERED destination
	// whose RESERVED budget absorbs its entry charge (4.3 as amended
	// 2026-10-20): image 1 is EXACTLY packed by chunk 0's charge, so
	// the dir falls to image 2 - and is durably assigned in the
	// directories table.
	var dirImage sql.NullInt64
	if err := db.QueryRow(`SELECT image_id FROM directories
	                       WHERE set_id = 'S-PLAN' AND relpath = 'empty'`).
		Scan(&dirImage); err != nil {
		t.Fatalf("read empty dir assignment: %v", err)
	}
	if !dirImage.Valid || dirImage.Int64 != 2 {
		t.Errorf("empty dir image_id = %v, want 2 (the lowest destination with entry slack; "+
			"image 1 is exactly packed by the reservation)", dirImage)
	}

	// Per-destination fill WITHIN usable (the 4.3 capacity invariant,
	// asserted over the real rows): image 1 holds exactly the
	// 977,901,312-byte chunk (its reserved budget to the byte), image
	// 2 the remainder plus the small payload and the dir entry.
	fills := queryDestFill(t, db, "S-PLAN")
	for id, f := range fills {
		if f.usedBytes > planFixtureUsable {
			t.Errorf("destination %d fill %d exceeds usable %d (capacity violation)",
				id, f.usedBytes, planFixtureUsable)
		}
	}
	if f := fills[1]; f.usedBytes != planFixtureChunk0 || f.items != 1 || f.dirs != 0 {
		t.Errorf("destination 1 fill = %+v, want %d bytes, 1 item, 0 dirs", f, planFixtureChunk0)
	}
	// Destination 2 holds the 72,098,688-byte remainder chunk plus
	// a.txt (8), sub/b.bin (8), and the symlink, whose planner size is
	// len("a.txt") = 5 bytes (2.3: "for symlinks, len(target)").
	if f := fills[2]; f.usedBytes != planFixtureBig-planFixtureChunk0+8+8+5 || f.items != 4 || f.dirs != 1 {
		t.Errorf("destination 2 fill = %+v, want %d bytes (chunk + 2 files + symlink), 4 items, 1 dir",
			f, planFixtureBig-planFixtureChunk0+21)
	}

	// --- second leg: re-run plan (human) -> the T2.2 no-op contract ---
	stdout, stderr, code = runPlanChild(t,
		"--catalog", cat, "--set-id", "S-PLAN", "--size", planFixtureSize)
	if code != report.ExitOK {
		t.Fatalf("re-run plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "nothing to plan: no unassigned items") {
		t.Errorf("re-run report lacks the nothing-to-plan line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "media: 2 destinations total (0 new, 2 pre-existing)") {
		t.Errorf("re-run report lacks the zero-new media line:\n%s", stdout)
	}
	// Zero NEW destinations: still exactly the two images rows (no-op).
	if err := db.QueryRow(`SELECT COUNT(*) FROM images WHERE set_id = 'S-PLAN'`).Scan(&images); err != nil {
		t.Fatalf("count images after re-run: %v", err)
	}
	if images != 2 {
		t.Errorf("images rows after re-run = %d, want 2 (no-op must create none)", images)
	}
}

// chunkRow is the assertion shape of one chunks row.
type chunkRow struct {
	index    int
	offset   int64
	length   int64
	assigned bool
}

// catChunkRows reads the chunk rows of one file (tests-only SQL; the
// production rule is that only internal/catalog issues SQL).
func catChunkRows(t *testing.T, db *sql.DB, setID, relpath string) []chunkRow {
	t.Helper()
	rows, err := db.Query(`SELECT c.chunk_index, c.offset, c.length, c.image_id
	                       FROM chunks c JOIN files f ON f.file_id = c.file_id
	                       WHERE c.set_id = ? AND f.relpath = ?
	                       ORDER BY c.chunk_index`, setID, relpath)
	if err != nil {
		t.Fatalf("query chunks of %s: %v", relpath, err)
	}
	defer rows.Close()
	var out []chunkRow
	for rows.Next() {
		var (
			r     chunkRow
			image sql.NullInt64
		)
		if err := rows.Scan(&r.index, &r.offset, &r.length, &image); err != nil {
			t.Fatalf("scan chunk row: %v", err)
		}
		r.assigned = image.Valid
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate chunk rows: %v", err)
	}
	return out
}

// --- incremental plan: locked destinations and the D-05 residual ---------------

// TestPlanMissingCatalogExits4: plan does NOT auto-create a missing
// catalog (unlike scan): a plan with no catalog has nothing to plan,
// and creation is scan's job. The open failure is a CatalogError
// (exit 4) whose message says "create one first".
func TestPlanMissingCatalogExits4(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such.sqlite")
	stdout, stderr, code := runPlanChild(t,
		"--catalog", missing, "--set-id", "S-404", "--size", planFixtureSize)
	if code != report.ExitCatalog {
		t.Fatalf("plan with missing catalog exit = %d, want 4\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "create one first") {
		t.Errorf("stderr lacks the catalog open guidance:\n%s", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout must stay empty on catalog errors, got: %q", stdout)
	}
}

// TestPlanLockContentionExit5: while another process holds the
// MUTATING catalog open (the T0.4 re-exec holder pattern), plan fails
// fast with exit 5 - plan is a mutating command and takes the
// exclusive flock (D-11, DESIGN_SPEC 2.1).
func TestPlanLockContentionExit5(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-BUSY")

	holder := exec.Command(os.Args[0])
	holder.Env = append(os.Environ(), lockHoldEnv+"=1", lockHoldPathEnv+"="+cat)
	holderStdin, err := holder.StdinPipe()
	if err != nil {
		t.Fatalf("holder stdin pipe: %v", err)
	}
	holderOut, err := holder.StdoutPipe()
	if err != nil {
		t.Fatalf("holder stdout pipe: %v", err)
	}
	holderStderr := &bytes.Buffer{}
	holder.Stderr = holderStderr
	if err := holder.Start(); err != nil {
		t.Fatalf("starting lock holder: %v", err)
	}
	defer func() {
		_ = holderStdin.Close() // EOF releases the holder's read loop
		_ = holder.Wait()
	}()
	buf := make([]byte, len(lockHoldReady)+2)
	if _, err := holderOut.Read(buf); err != nil {
		t.Fatalf("holder never became ready: %v (stderr: %s)", err, holderStderr.String())
	}
	if !strings.Contains(string(buf), lockHoldReady) {
		t.Fatalf("holder readiness = %q, want %q", string(buf), lockHoldReady)
	}

	stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", "S-BUSY", "--size", planFixtureSize)
	if code != report.ExitCatalogBusy {
		t.Fatalf("plan against a locked catalog exit = %d, want 5\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "busy") {
		t.Errorf("stderr lacks the busy message:\n%s", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout must stay empty on catalog-busy, got: %q", stdout)
	}
}
