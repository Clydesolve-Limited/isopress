//go:build e2e

package cli

import (
	"bytes"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"isopress/internal/report"
)

// rechunkBigBytes is deliberately wedged between the effective chunk
// sizes of --size 1 (980,000,000 - 2,098,688 = 977,901,312) and
// --size 2 (1,960,000,000 - 2,098,688 = 1,957,901,312, the 2026-10-20
// reservation): the file is OVERSIZED under both (it chunks with a
// different effective size - the ReplaceAllChunks trigger) but fits
// WHOLE under --size 4 (3,920,000,000 - the NB2 DropChunkState
// trigger).
const rechunkBigBytes int64 = 1_970_000_000

// makeRechunkFixture builds the one-file tree for the changed-profile
// test: a sparse big.dat (instant to create; scan reads its zeros
// once).
func makeRechunkFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	f, err := os.Create(filepath.Join(root, "big.dat"))
	if err != nil {
		t.Fatalf("create big.dat: %v", err)
	}
	if err := f.Truncate(rechunkBigBytes); err != nil {
		t.Fatalf("truncate big.dat: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close big.dat: %v", err)
	}
	return root
}

// TestClearReplanWithChangedProfileRechunksThroughRealCommands: the
// full profile-change round trip through the real commands.
//
//	leg A: plan --size 1 -> effective chunk size 977,901,312, 3 chunks;
//	seed written state (hashes + copied + verified_at), clear -> FR-010
//	retains rows/hashes; leg B: plan --size 2 -> effective 1,957,901,312
//	!= stored -> ActionRechunk -> ReplaceAllChunks under the new
//	PlanTx (rechunked_files 1, rehashed_at_next_write 3: the 3 stored
//	hashes described the OLD boundaries and are dropped, the rows
//	replaced by the 2-chunk decomposition); clear again (rows with
//	NULL hashes now); leg C: plan --size 4 -> the file fits whole ->
//	NB2 DropChunkState: rows deleted, stamp cleared, whole-file
//	assignment - no stale rows in either direction.
func TestClearReplanWithChangedProfileRechunksThroughRealCommands(t *testing.T) {
	root := makeRechunkFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-RE")

	// --- leg A: chunk at effective 977,901,312 (disk, --size 1) ---
	stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", "S-RE", "--size", "1", "--json")
	if code != report.ExitOK {
		t.Fatalf("leg A plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	docA := decodeSingleJSONDoc(t, stdout)
	if docA["assigned_chunks"].(float64) != 3 || docA["rechunked_files"].(float64) != 0 {
		t.Fatalf("leg A plan = chunks %v rechunk %v, want 3/0 (fresh decomposition)",
			docA["assigned_chunks"], docA["rechunked_files"])
	}
	// A full chunk's charge (977,901,312 + 512) fills each
	// destination's reserved budget exactly, so the 3 chunks open 3
	// destinations.
	if docA["media_count"].(float64) != 3 {
		t.Fatalf("leg A media_count = %v, want 3", docA["media_count"])
	}
	seeded := seedWrittenChunkState(t, cat, "S-RE")
	if len(seeded) != 3 {
		t.Fatalf("seeded %d chunk hashes, want 3", len(seeded))
	}

	// --- clear 1: FR-010 retention of the written state ---
	stdout, stderr, code = runClearChild(t, "--catalog", cat, "--set-id", "S-RE", "--json")
	if code != report.ExitOK {
		t.Fatalf("clear 1 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	docC1 := decodeSingleJSONDoc(t, stdout)
	if docC1["chunks_cleared"].(float64) != 3 || docC1["chunk_hashes_retained"].(float64) != 3 {
		t.Fatalf("clear 1 = chunks %v retained %v, want 3/3 (FR-010)",
			docC1["chunks_cleared"], docC1["chunk_hashes_retained"])
	}
	db := catOpen(t, cat)
	// Rows retained byte-identical, placement cleared, and the
	// verified_at judgment call (a verification stamp describing
	// released media would mislead) asserted through the real flow.
	rowsCleared := queryChunkHashes(t, db, "S-RE", "big.dat")
	for idx, want := range seeded {
		if !bytes.Equal(rowsCleared[idx], want) {
			t.Errorf("chunk %d hash after clear 1 = %x, want the stored %x (FR-010)", idx, rowsCleared[idx], want)
		}
	}
	var stamped int
	if err := db.QueryRow(`SELECT COUNT(*) FROM chunks WHERE set_id = 'S-RE' AND verified_at IS NOT NULL`).
		Scan(&stamped); err != nil {
		t.Fatalf("count residual verified_at: %v", err)
	}
	if stamped != 0 {
		t.Errorf("%d chunks kept a verified_at stamp after clear; want 0 (a verification stamp must not outlive its placement)", stamped)
	}

	// --- leg B: changed effective size -> ReplaceAllChunks ---
	stdout, stderr, code = runPlanChild(t,
		"--catalog", cat, "--set-id", "S-RE", "--size", "2", "--json")
	if code != report.ExitOK {
		t.Fatalf("leg B plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	docB := decodeSingleJSONDoc(t, stdout)
	if docB["rechunked_files"].(float64) != 1 || docB["rehashed_at_next_write"].(float64) != 3 {
		t.Errorf("leg B plan = rechunk %v rehash-at-next-write %v, want 1/3 (the stored hashes describe the OLD boundaries)",
			docB["rechunked_files"], docB["rehashed_at_next_write"])
	}
	if docB["assigned_chunks"].(float64) != 2 || docB["media_count"].(float64) != 2 {
		t.Errorf("leg B plan = chunks %v media %v, want the new 2-chunk decomposition on 2 destinations",
			docB["assigned_chunks"], docB["media_count"])
	}
	// Catalog consistency: exactly the NEW decomposition, no stale
	// rows, no stale hashes, new stamp.
	rowsAfter := catChunkRows(t, db, "S-RE", "big.dat")
	wantSpecs := []chunkRow{
		{index: 0, offset: 0, length: 1_957_901_312, assigned: true},
		{index: 1, offset: 1_957_901_312, length: 12_098_688, assigned: true},
	}
	if len(rowsAfter) != len(wantSpecs) {
		t.Fatalf("chunk rows after leg B = %d (%+v), want the 2-row replacement decomposition", len(rowsAfter), rowsAfter)
	}
	for i, w := range wantSpecs {
		got := rowsAfter[i]
		if got.index != w.index || got.offset != w.offset || got.length != w.length || got.assigned != w.assigned {
			t.Errorf("chunk row %d after leg B = %+v, want %+v (ReplaceAllChunks, no stale rows)", i, got, w)
		}
	}
	var residualHashes int
	if err := db.QueryRow(`SELECT COUNT(*) FROM chunks WHERE set_id = 'S-RE' AND hash IS NOT NULL`).
		Scan(&residualHashes); err != nil {
		t.Fatalf("count residual hashes after leg B: %v", err)
	}
	if residualHashes != 0 {
		t.Errorf("%d chunk hashes survived the boundary shift; want 0 (a hash describes bytes of the OLD range - retaining it would corrupt verification)",
			residualHashes)
	}
	var chunked, chunkCount int
	var chunkSize int64
	if err := db.QueryRow(`SELECT chunked, chunk_count, chunk_size FROM files
	                       WHERE set_id = 'S-RE' AND relpath = 'big.dat'`).
		Scan(&chunked, &chunkCount, &chunkSize); err != nil {
		t.Fatalf("read chunk stamp after leg B: %v", err)
	}
	if chunked != 1 || chunkCount != 2 || chunkSize != 1_957_901_312 {
		t.Errorf("chunk stamp after leg B = %d/%d/%d, want 1/2/1957901312 (the new effective size)",
			chunked, chunkCount, chunkSize)
	}

	// --- clear 2 (unwritten rows this time) then leg C: whole file ---
	stdout, stderr, code = runClearChild(t, "--catalog", cat, "--set-id", "S-RE", "--json")
	if code != report.ExitOK {
		t.Fatalf("clear 2 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	docC2 := decodeSingleJSONDoc(t, stdout)
	if docC2["chunks_cleared"].(float64) != 2 || docC2["chunk_hashes_retained"].(float64) != 0 {
		t.Errorf("clear 2 = chunks %v retained %v, want 2/0 (never-written rows retain no hashes)",
			docC2["chunks_cleared"], docC2["chunk_hashes_retained"])
	}
	stdout, stderr, code = runPlanChild(t,
		"--catalog", cat, "--set-id", "S-RE", "--size", "4", "--json")
	if code != report.ExitOK {
		t.Fatalf("leg C plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	docC := decodeSingleJSONDoc(t, stdout)
	if docC["media_count"].(float64) != 1 || docC["assigned_files"].(float64) != 1 ||
		docC["assigned_chunks"].(float64) != 0 {
		t.Errorf("leg C plan = media %v files %v chunks %v, want 1/1/0 (whole-file placement)",
			docC["media_count"], docC["assigned_files"], docC["assigned_chunks"])
	}
	// NB2 DropChunkState: ZERO chunk rows left, the stamp cleared, and
	// the assignment now lives on the files row.
	if leftover := catChunkRows(t, db, "S-RE", "big.dat"); len(leftover) != 0 {
		t.Errorf("leg C left %d stale chunk rows (%+v), want 0 (the file fits whole)", len(leftover), leftover)
	}
	var wholeImage sql.NullInt64
	var chunkSizeNull sql.NullInt64
	if err := db.QueryRow(`SELECT chunked, chunk_count, chunk_size, image_id FROM files
	                       WHERE set_id = 'S-RE' AND relpath = 'big.dat'`).
		Scan(&chunked, &chunkCount, &chunkSizeNull, &wholeImage); err != nil {
		t.Fatalf("read whole-file state after leg C: %v", err)
	}
	if chunked != 0 || chunkCount != 0 || chunkSizeNull.Valid || !wholeImage.Valid {
		t.Errorf("leg C file state = chunked %d count %d size %v image %v, want un-stamped 0/0/NULL with a whole-file assignment",
			chunked, chunkCount, chunkSizeNull, wholeImage)
	}
}

// queryChunkHashes reads the per-chunk-index stored hashes of one file
// (tests-only SQL).
func queryChunkHashes(t *testing.T, db *sql.DB, setID, relpath string) map[int][]byte {
	t.Helper()
	rows, err := db.Query(`SELECT c.chunk_index, c.hash FROM chunks c
	                       JOIN files f ON f.file_id = c.file_id
	                       WHERE c.set_id = ? AND f.relpath = ? ORDER BY c.chunk_index`, setID, relpath)
	if err != nil {
		t.Fatalf("query chunk hashes of %s: %v", relpath, err)
	}
	defer rows.Close()
	out := map[int][]byte{}
	for rows.Next() {
		var (
			idx  int
			hash []byte
		)
		if err := rows.Scan(&idx, &hash); err != nil {
			t.Fatalf("scan chunk hash: %v", err)
		}
		out[idx] = hash
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate chunk hashes: %v", err)
	}
	return out
}

// --- the images-row DELETE decision, pinned mechanically -----------------------

// TestClearAndPlanRefusedWhileMutatingRunHoldsFlock: the holder takes
// the SAME exclusive advisory flock a real mutating run takes
// (catalog.Open OpenReadWrite over <catalog>.lock - the T0.4
// lockHoldMain helper), so this pins BOTH directions of the brief's
// contention matrix with REAL commands on the receiving end: a clear
// refused while another mutating run is active (exit 5) AND a plan
// refused the same way - the single-writer model is symmetric because
// both commands acquire the identical lock through the identical
// path.
func TestClearAndPlanRefusedWhileMutatingRunHoldsFlock(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-CBOTH")
	if stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", "S-CBOTH", "--size", planFixtureSize); code != report.ExitOK {
		t.Fatalf("plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

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
		_ = holderStdin.Close()
		_ = holder.Wait()
	}()
	buf := make([]byte, len(lockHoldReady)+2)
	if _, err := holderOut.Read(buf); err != nil {
		t.Fatalf("holder never became ready: %v (stderr: %s)", err, holderStderr.String())
	}
	if !strings.Contains(string(buf), lockHoldReady) {
		t.Fatalf("holder readiness = %q, want %q (stderr: %s)", string(buf), lockHoldReady, holderStderr.String())
	}

	for _, tc := range []struct {
		name string
		run  func() (string, string, int)
	}{
		{"clear", func() (string, string, int) {
			return runClearChild(t, "--catalog", cat, "--set-id", "S-CBOTH")
		}},
		{"plan", func() (string, string, int) {
			return runPlanChild(t, "--catalog", cat, "--set-id", "S-CBOTH", "--size", planFixtureSize)
		}},
	} {
		t.Run(tc.name+" vs held flock", func(t *testing.T) {
			stdout, stderr, code := tc.run()
			if code != report.ExitCatalogBusy {
				t.Fatalf("%s against a locked catalog exit = %d, want 5\nstdout: %s\nstderr: %s",
					tc.name, code, stdout, stderr)
			}
			if !strings.Contains(stderr, "busy") {
				t.Errorf("%s stderr lacks the busy message:\n%s", tc.name, stderr)
			}
			if stdout != "" {
				t.Errorf("%s stdout must stay empty on catalog-busy, got: %q", tc.name, stdout)
			}
		})
	}
}

// --- the S2 reachability boundary (forward caveat) ------------------------------

// TestClearAfterBuiltImageKeepsIsoOnDisk (gated on xorriso, 12.6)
// pins the documented ClearPlan-vs-BUILT-media interaction through the
// REAL commands: clearing a set whose image was BUILT removes the
// images row (state 'built' included - the clearstore.go images-row
// decision has no state guard, deliberately) and the set's
// image_contents rows in one transaction, while the .iso FILE on disk
// is left untouched - the catalog's clear never touches files; media
// hygiene is the operator's call (documented behavior; the
// clearstore.go scope note records the provenance trade).
func TestClearAfterBuiltImageKeepsIsoOnDisk(t *testing.T) {
	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skip("xorriso not on PATH; skipping the gated clear-after-build test (DESIGN_SPEC 12.6)")
	}
	root := makeBuildSingleImageTree(t)
	cat := createCatalog(t)
	const setID = "S-CLRBD"
	scanBuildFixture(t, root, cat, setID)
	planSynthetic(t, cat, setID, buildFixtureProfile)
	dest := filepath.Join(t.TempDir(), "dest")

	// Build the set for real (exit 0: one built, self-verified image).
	stdout, stderr, code := runBuildChild(t,
		"build", "--catalog", cat, "--set-id", setID, "--dest", dest)
	if code != report.ExitOK {
		t.Fatalf("build exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	iso := filepath.Join(dest, setID+"-001.iso")
	if _, err := os.Stat(iso); err != nil {
		t.Fatalf("built .iso missing before the clear: %v", err)
	}
	db := catOpen(t, cat)
	var contents int
	if err := db.QueryRow(`SELECT COUNT(*) FROM image_contents WHERE set_id = ?`, setID).Scan(&contents); err != nil {
		t.Fatal(err)
	}
	if contents == 0 {
		t.Fatal("the built image has no image_contents rows (6.6); the fixture is broken")
	}

	// THE CLEAR: exit 0, catalog placement rows gone...
	stdout, stderr, code = runClearChild(t, "--catalog", cat, "--set-id", setID)
	if code != report.ExitOK {
		t.Fatalf("clear exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	var images, remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM images WHERE set_id = ?`, setID).Scan(&images); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM image_contents WHERE set_id = ?`, setID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if images != 0 || remaining != 0 {
		t.Errorf("after clearing a BUILT set: images rows = %d, image_contents rows = %d, want 0/0", images, remaining)
	}

	// ...AND THE .ISO FILE STAYS ON DISK (the documented behavior:
	// ClearPlan deletes catalog rows, never files).
	if _, err := os.Stat(iso); err != nil {
		t.Errorf("the built .iso must REMAIN on disk after clear-assignments (the catalog never touches files): %v", err)
	}
}
