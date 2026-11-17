package cli

// T2.4 end-to-end acceptance tests for the real `clear-assignments`
// command (IMPLEMENTATION_PLAN T2.4; DESIGN_SPEC 4.5 single-transaction
// clear, 2.4 locking enforcement, 2.1 amended reader-during-writer,
// FR-010 retention, FR-011 unlock; PLAN_SPEC FR-010, FR-011). Every
// test drives the REAL production entry path: a re-exec child running
// execute() over the real command tree (the scan_test.go pattern). The
// catalog side is asserted with the SQLite driver directly (tests
// only - production SQL stays inside internal/catalog).
//
// The centerpiece (TestClearE2EUnlocksAndReplanMatchesFreshPlan) pins
// the exact-fresh-plan-equivalence acceptance criterion end to end:
// scan -> plan (assignments asserted) -> seed a written+verified chunk
// state (T3/T5 sprints do not exist yet; the seeding simulates their
// catalog effect) -> clear -> assert EVERY assignment NULL/copied 0,
// chunk ROWS PRESENT with byte-identical hashes (FR-010), images rows
// deleted (the clearstore.go decision) -> remove the source tree
// entirely (the instrumented-filesystem proof of zero source opens) ->
// re-plan -> IDENTICAL plan (same media count, same per-destination
// fill, same per-relpath assignments) with the stored chunk hashes
// REUSED (rechunked_files 0, rehashed_at_next_write 0).
//
// The mid-transaction hold (clearMidTxMain, dispatched from
// cli_test.go's TestMain) replays the ClearPlan statement sequence in
// ONE open transaction, so the concurrent-reader test reads a
// deterministic in-flight clear: a read-only open plus the real
// `report` command must succeed over the WAL snapshot (amended 2.1:
// readers take NO advisory flock), observing the pre-clear snapshot
// (no block, no torn read) and, after the helper commits, the fully
// cleared state.

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"isopress/internal/report"
)

// runClearChild runs the real clear-assignments command as a child
// process.
func runClearChild(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	return runCLIChild(t, append([]string{"clear-assignments"}, args...)...)
}

// --- helpers -----------------------------------------------------------------

// seedWrittenChunkState simulates the catalog effect of a completed
// copy+verify cycle (T3 first-write chunk hashing, T5 verify-media
// stamping - neither sprint exists yet): every chunk row of the set
// gets a deterministic stored hash, copied=1, and verified_at; every
// assigned files row gets copied=1. It returns the seeded per-index
// hashes so tests can assert byte-identical retention (FR-010).
func seedWrittenChunkState(t *testing.T, cat, setID string) map[int][]byte {
	t.Helper()
	db := catRW(t, cat)
	rows, err := db.Query(`SELECT chunk_id, chunk_index FROM chunks WHERE set_id = ? ORDER BY chunk_index`, setID)
	if err != nil {
		t.Fatalf("query chunk ids: %v", err)
	}
	type pair struct{ id, idx int64 }
	var pairs []pair
	for rows.Next() {
		var p pair
		if err := rows.Scan(&p.id, &p.idx); err != nil {
			t.Fatalf("scan chunk id: %v", err)
		}
		pairs = append(pairs, p)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate chunk ids: %v", err)
	}
	rows.Close()
	hashes := make(map[int][]byte, len(pairs))
	for _, p := range pairs {
		h := bytes.Repeat([]byte{byte(p.idx) + 1}, 32)
		hashes[int(p.idx)] = h
		if _, err := db.Exec(`UPDATE chunks SET hash = ?, copied = 1, verified_at = 4242 WHERE chunk_id = ?`,
			h, p.id); err != nil {
			t.Fatalf("seeding chunk %d written state: %v", p.id, err)
		}
	}
	if _, err := db.Exec(`UPDATE files SET copied = 1 WHERE set_id = ? AND image_id IS NOT NULL`, setID); err != nil {
		t.Fatalf("seeding files copied marks: %v", err)
	}
	return hashes
}

// readAssignments reads the full placement of a set from the catalog
// (tests-only SQL): per-relpath files image_id, per (relpath, chunk
// index) chunks image_id, per-relpath directories image_id - NULLs
// normalize to 0 so two snapshots compare with ==.
func readAssignments(t *testing.T, cat, setID string) (files, chunks, dirs map[string]int64) {
	t.Helper()
	db := catOpen(t, cat)
	files = map[string]int64{}
	chunks = map[string]int64{}
	dirs = map[string]int64{}
	collect := func(dest map[string]int64, query, what string) {
		t.Helper()
		rows, err := db.Query(query, setID)
		if err != nil {
			t.Fatalf("query %s assignments: %v", what, err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				key string
				id  int64
			)
			if err := rows.Scan(&key, &id); err != nil {
				t.Fatalf("scan %s assignment: %v", what, err)
			}
			dest[key] = id
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate %s assignments: %v", what, err)
		}
	}
	collect(files, `SELECT relpath, COALESCE(image_id, 0) FROM files WHERE set_id = ?`, "file")
	collect(chunks, `SELECT f.relpath || '#' || c.chunk_index, COALESCE(c.image_id, 0)
	                 FROM chunks c JOIN files f ON f.file_id = c.file_id
	                 WHERE c.set_id = ? ORDER BY f.relpath, c.chunk_index`, "chunk")
	collect(dirs, `SELECT relpath, COALESCE(image_id, 0) FROM directories WHERE set_id = ?`, "dir")
	return files, chunks, dirs
}

// dumpClearStateCLI reads the full assignment state (files, chunks,
// directories, images) into one deterministic string for the
// bit-identical atomicity comparisons.
func dumpClearStateCLI(t *testing.T, db *sql.DB) string {
	t.Helper()
	var buf bytes.Buffer
	dump := func(query, what string) {
		t.Helper()
		rows, err := db.Query(query)
		if err != nil {
			t.Fatalf("dump %s: %v", what, err)
		}
		defer rows.Close()
		cols, err := rows.Columns()
		if err != nil {
			t.Fatalf("columns of %s: %v", what, err)
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				t.Fatalf("scan %s row: %v", what, err)
			}
			for i, v := range vals {
				switch tv := v.(type) {
				case []byte:
					fmt.Fprintf(&buf, "%x", tv)
				case nil:
					buf.WriteString("NULL")
				default:
					fmt.Fprintf(&buf, "%v", tv)
				}
				if i < len(vals)-1 {
					buf.WriteString("|")
				}
			}
			buf.WriteString("\n")
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate %s: %v", what, err)
		}
	}
	dump(`SELECT file_id, relpath, status, image_id, copied, chunked, chunk_count, chunk_size
	      FROM files ORDER BY file_id`, "files")
	dump(`SELECT chunk_id, file_id, chunk_index, offset, length, hash, image_id, copied, verified_at
	      FROM chunks ORDER BY chunk_id`, "chunks")
	dump(`SELECT dir_id, relpath, status, image_id FROM directories ORDER BY dir_id`, "directories")
	dump(`SELECT image_id, nominal_size, usable_size, media_profile, state
	      FROM images ORDER BY image_id`, "images")
	return buf.String()
}

// --- the centerpiece: clear unlocks everything and re-plan is a fresh plan ----

// TestClearE2EUnlocksAndReplanMatchesFreshPlan is the T2.4 acceptance
// centerpiece. See the file doc for the flow.
func TestClearE2EUnlocksAndReplanMatchesFreshPlan(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-CLEAR")

	// --- first plan (asserts the assignments the clear will unlock) ---
	firstOut, firstErr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", "S-CLEAR", "--size", planFixtureSize, "--json")
	if code != report.ExitOK {
		t.Fatalf("first plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, firstOut, firstErr)
	}
	firstDoc := decodeSingleJSONDoc(t, firstOut)
	if firstDoc["media_count"].(float64) != 2 || firstDoc["new_destinations"].(float64) != 2 {
		t.Fatalf("first plan media = %v new %v, want 2/2", firstDoc["media_count"], firstDoc["new_destinations"])
	}
	if firstDoc["assigned_files"].(float64) != 2 || firstDoc["assigned_chunks"].(float64) != 2 ||
		firstDoc["assigned_symlinks"].(float64) != 1 || firstDoc["assigned_dirs"].(float64) != 1 {
		t.Fatalf("first plan assignments = files %v chunks %v symlinks %v dirs %v, want 2/2/1/1",
			firstDoc["assigned_files"], firstDoc["assigned_chunks"],
			firstDoc["assigned_symlinks"], firstDoc["assigned_dirs"])
	}
	firstFiles, firstChunks, firstDirs := readAssignments(t, cat, "S-CLEAR")
	for _, rel := range []string{"a.txt", "sub/b.bin", "link"} {
		if firstFiles[rel] == 0 {
			t.Fatalf("first plan did not assign %q", rel)
		}
	}
	if firstChunks["big.dat#0"] == 0 || firstChunks["big.dat#1"] == 0 {
		t.Fatalf("first plan did not assign big.dat chunks: %v", firstChunks)
	}
	if firstDirs["empty"] == 0 {
		t.Fatalf("first plan did not assign the empty dir: %v", firstDirs)
	}

	// Seed a written+verified chunk state (simulated T3/T5 effect).
	seededHashes := seedWrittenChunkState(t, cat, "S-CLEAR")

	// --- the clear, through the REAL command ---
	stdout, stderr, code := runClearChild(t, "--catalog", cat, "--set-id", "S-CLEAR", "--json")
	if code != report.ExitOK {
		t.Fatalf("clear --json exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// Progress summary on stderr (FR-046).
	if !strings.Contains(stderr, "clear: cleared 3 files, 2 chunks, 1 dirs; removed 2 destinations") {
		t.Errorf("stderr lacks the clear summary line:\n%s", stderr)
	}
	// stdout carries EXACTLY one JSON document (FR-046).
	doc := decodeSingleJSONDoc(t, stdout)
	// The frozen --json key set (the pin lives below; this asserts the
	// REAL command emits it).
	wantKeys := append([]string(nil), clearReportJSONKeys...)
	sort.Strings(wantKeys)
	if got := sortedKeys(doc); !sameStrings(got, wantKeys) {
		t.Fatalf("clear --json key set = %v\nwant exactly %v (schema freeze)", got, wantKeys)
	}
	if doc["mode"] != "clear" || doc["set_id"] != "S-CLEAR" {
		t.Errorf("json identity = %v/%v, want clear/S-CLEAR", doc["mode"], doc["set_id"])
	}
	// a.txt + sub/b.bin + link carried files.image_id (big.dat is
	// chunked and carries files.image_id NULL); 2 chunks; 1 dir; the
	// set's 2 destinations; 2 retained hashes; a real run id.
	if doc["files_cleared"].(float64) != 3 || doc["chunks_cleared"].(float64) != 2 ||
		doc["dirs_cleared"].(float64) != 1 || doc["destinations_removed"].(float64) != 2 ||
		doc["chunk_hashes_retained"].(float64) != 2 {
		t.Errorf("json cleared counts = files %v chunks %v dirs %v dest %v retained %v, want 3/2/1/2/2",
			doc["files_cleared"], doc["chunks_cleared"], doc["dirs_cleared"],
			doc["destinations_removed"], doc["chunk_hashes_retained"])
	}
	if doc["nothing_to_clear"] != false || doc["run_id"].(float64) <= 0 {
		t.Errorf("json nothing_to_clear/run_id = %v/%v, want false and a positive run id",
			doc["nothing_to_clear"], doc["run_id"])
	}

	// --- catalog assertions: everything unlocked, nothing deleted (FR-010) ---
	db := catOpen(t, cat)
	for _, rel := range []string{"a.txt", "sub/b.bin", "big.dat", "link"} {
		rows := queryFiles(t, db, rel)
		if len(rows) != 1 {
			t.Fatalf("%s rows after clear = %d, want 1 (file records retained)", rel, len(rows))
		}
		if rows[0].ImageID.Valid || rows[0].Copied {
			t.Errorf("%s after clear = image_id %v copied %v, want NULL/0", rel, rows[0].ImageID, rows[0].Copied)
		}
	}
	// CHUNK ROWS PRESENT WITH HASHES (FR-010 - counted explicitly).
	var chunkRows, chunkHashes int
	if err := db.QueryRow(`SELECT COUNT(*), SUM(CASE WHEN hash IS NOT NULL THEN 1 ELSE 0 END)
	                       FROM chunks WHERE set_id = 'S-CLEAR'`).Scan(&chunkRows, &chunkHashes); err != nil {
		t.Fatalf("count chunk rows after clear: %v", err)
	}
	if chunkRows != 2 || chunkHashes != 2 {
		t.Errorf("chunk rows after clear = %d (%d with hashes), want 2/2 (FR-010: nothing deleted)", chunkRows, chunkHashes)
	}
	gotHashes := map[int][]byte{}
	hrows, err := db.Query(`SELECT c.chunk_index, c.hash FROM chunks c
	                        JOIN files f ON f.file_id = c.file_id
	                        WHERE c.set_id = 'S-CLEAR' AND f.relpath = 'big.dat' ORDER BY c.chunk_index`)
	if err != nil {
		t.Fatalf("read retained chunk hashes: %v", err)
	}
	for hrows.Next() {
		var (
			idx  int
			hash []byte
		)
		if err := hrows.Scan(&idx, &hash); err != nil {
			t.Fatalf("scan chunk hash: %v", err)
		}
		gotHashes[idx] = hash
	}
	if err := hrows.Err(); err != nil {
		t.Fatalf("iterate chunk hashes: %v", err)
	}
	hrows.Close()
	for idx, want := range seededHashes {
		if !bytes.Equal(gotHashes[idx], want) {
			t.Errorf("chunk %d hash after clear = %x, want the stored %x (FR-010)", idx, gotHashes[idx], want)
		}
	}
	// The chunk rows' placement state is cleared, the stamp retained.
	chunksAfter := catChunkRows(t, db, "S-CLEAR", "big.dat")
	if len(chunksAfter) != 2 || chunksAfter[0].assigned || chunksAfter[1].assigned {
		t.Errorf("big.dat chunk rows after clear = %+v, want 2 unassigned rows (retained)", chunksAfter)
	}
	var chunked, chunkCount int
	var chunkSize int64
	if err := db.QueryRow(`SELECT chunked, chunk_count, chunk_size FROM files
	                       WHERE set_id = 'S-CLEAR' AND relpath = 'big.dat'`).
		Scan(&chunked, &chunkCount, &chunkSize); err != nil {
		t.Fatalf("read chunk stamp after clear: %v", err)
	}
	// The retained stamp names the EFFECTIVE chunk size the plan used
	// (usable - reserved overhead since the 2026-10-20 amendment).
	if chunked != 1 || chunkCount != 2 || chunkSize != planFixtureChunk0 {
		t.Errorf("chunk stamp after clear = %d/%d/%d, want 1/2/%d (retained for reuse)",
			chunked, chunkCount, chunkSize, planFixtureChunk0)
	}
	// Directories unassigned; images rows DELETED (the documented
	// clearstore.go decision - fresh-plan equivalence).
	var assignedDirs, images int
	if err := db.QueryRow(`SELECT COUNT(*) FROM directories WHERE set_id = 'S-CLEAR' AND image_id IS NOT NULL`).
		Scan(&assignedDirs); err != nil {
		t.Fatalf("count assigned dirs after clear: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM images WHERE set_id = 'S-CLEAR'`).Scan(&images); err != nil {
		t.Fatalf("count images after clear: %v", err)
	}
	if assignedDirs != 0 || images != 0 {
		t.Errorf("after clear: %d assigned dirs, %d images rows; want 0/0", assignedDirs, images)
	}
	// Run bookkeeping: kind 'clear' (the 2.3 CHECK admits it), 'ok'.
	var kind, status string
	if err := db.QueryRow(`SELECT kind, status FROM runs WHERE kind = 'clear'`).Scan(&kind, &status); err != nil {
		t.Fatalf("read clear run row: %v", err)
	}
	if kind != "clear" || status != "ok" {
		t.Errorf("clear run row = %q/%q, want clear/ok", kind, status)
	}

	// --- the instrumented filesystem: the source tree is GONE ---
	// Planning is metadata-only (4.3, 12.5); with the source removed
	// any source open would fail with ENOENT, so a successful re-plan
	// below PROVES zero source reads.
	if err := os.RemoveAll(root); err != nil {
		t.Fatalf("removing the source tree: %v", err)
	}

	// --- re-plan: IDENTICAL to the fresh plan ---
	secondOut, secondErr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", "S-CLEAR", "--size", planFixtureSize, "--json")
	if code != report.ExitOK {
		t.Fatalf("re-plan (source removed) exit = %d, want 0 - a source read leaked into planning?\nstdout: %s\nstderr: %s",
			code, secondOut, secondErr)
	}
	secondDoc := decodeSingleJSONDoc(t, secondOut)
	if secondDoc["media_count"].(float64) != 2 || secondDoc["new_destinations"].(float64) != 2 {
		t.Errorf("re-plan media = %v new %v, want the fresh-plan 2/2",
			secondDoc["media_count"], secondDoc["new_destinations"])
	}
	// The boundary-stability reuse proof (4.2): no re-chunk, no
	// re-hash-at-next-write - the stored decomposition and hashes were
	// reused.
	if secondDoc["rechunked_files"].(float64) != 0 || secondDoc["rehashed_at_next_write"].(float64) != 0 {
		t.Errorf("re-plan rechunk/rehash = %v/%v, want 0/0 (stored state reused)",
			secondDoc["rechunked_files"], secondDoc["rehashed_at_next_write"])
	}
	if secondDoc["assigned_files"].(float64) != firstDoc["assigned_files"].(float64) ||
		secondDoc["assigned_chunks"].(float64) != firstDoc["assigned_chunks"].(float64) ||
		secondDoc["assigned_symlinks"].(float64) != firstDoc["assigned_symlinks"].(float64) ||
		secondDoc["assigned_dirs"].(float64) != firstDoc["assigned_dirs"].(float64) {
		t.Errorf("re-plan assignments = files %v chunks %v symlinks %v dirs %v, want the first plan's %v/%v/%v/%v",
			secondDoc["assigned_files"], secondDoc["assigned_chunks"],
			secondDoc["assigned_symlinks"], secondDoc["assigned_dirs"],
			firstDoc["assigned_files"], firstDoc["assigned_chunks"],
			firstDoc["assigned_symlinks"], firstDoc["assigned_dirs"])
	}
	// Same destinations: count AND per-destination fill numbers.
	if !reflect.DeepEqual(firstDoc["destinations"], secondDoc["destinations"]) {
		t.Errorf("re-plan destinations = %+v\nwant the first plan's %+v (identical fill numbers)",
			secondDoc["destinations"], firstDoc["destinations"])
	}
	// Same per-relpath placement, files AND chunks AND dirs.
	secondFiles, secondChunks, secondDirs := readAssignments(t, cat, "S-CLEAR")
	if !reflect.DeepEqual(firstFiles, secondFiles) || !reflect.DeepEqual(firstChunks, secondChunks) ||
		!reflect.DeepEqual(firstDirs, secondDirs) {
		t.Errorf("re-plan placement differs from the first plan:\nfiles %v vs %v\nchunks %v vs %v\ndirs %v vs %v",
			secondFiles, firstFiles, secondChunks, firstChunks, secondDirs, firstDirs)
	}
	// Chunk hashes STILL byte-identical after the re-plan.
	finalHashes := map[int][]byte{}
	frows, err := db.Query(`SELECT chunk_index, hash FROM chunks WHERE set_id = 'S-CLEAR' ORDER BY chunk_index`)
	if err != nil {
		t.Fatalf("read chunk hashes after re-plan: %v", err)
	}
	for frows.Next() {
		var (
			idx  int
			hash []byte
		)
		if err := frows.Scan(&idx, &hash); err != nil {
			t.Fatalf("scan chunk hash after re-plan: %v", err)
		}
		finalHashes[idx] = hash
	}
	if err := frows.Err(); err != nil {
		t.Fatalf("iterate chunk hashes after re-plan: %v", err)
	}
	frows.Close()
	for idx, want := range seededHashes {
		if !bytes.Equal(finalHashes[idx], want) {
			t.Errorf("chunk %d hash after re-plan = %x, want the byte-identical stored %x", idx, finalHashes[idx], want)
		}
	}
}

// TestClearStrictIsAccepted documents the --strict judgment call for
// this command (mirroring TestPlanStrictIsAccepted): the clear is ONE
// atomic transaction with NO per-item failure path, so --strict has
// nothing to promote and stays inert - a clear with --strict behaves
// EXACTLY as one without it (same exit code, same report shape).
func TestClearStrictIsAccepted(t *testing.T) {
	// Two IDENTICAL fixtures, cleared once WITHOUT --strict and once
	// WITH it: the reports must be indistinguishable modulo the set id.
	cat := createCatalog(t)
	var reports []string
	for _, leg := range []struct {
		name   string
		setID  string
		strict bool
	}{
		{name: "without --strict", setID: "S-CSTRICT-PLAIN"},
		{name: "with --strict", setID: "S-CSTRICT-STRICT", strict: true},
	} {
		root := makePlanFixture(t)
		scanPlanFixture(t, root, cat, leg.setID)
		if _, stderr, code := runPlanChild(t,
			"--catalog", cat, "--set-id", leg.setID, "--size", planFixtureSize); code != report.ExitOK {
			t.Fatalf("plan fixture (%s) exit = %d, want 0\nstderr: %s", leg.name, code, stderr)
		}

		args := []string{"--catalog", cat, "--set-id", leg.setID}
		if leg.strict {
			args = append(args, "--strict")
		}
		stdout, stderr, code := runClearChild(t, args...)
		if code != report.ExitOK {
			t.Fatalf("clear %s exit = %d, want 0 (no item-failure path to promote)\nstdout: %s\nstderr: %s",
				leg.name, code, stdout, stderr)
		}
		if !strings.Contains(stdout, "cleared: 3 files, 2 chunks, 1 dirs") {
			t.Errorf("clear %s lost the cleared line:\n%s", leg.name, stdout)
		}
		// Normalize the set id so the two reports compare byte for byte.
		reports = append(reports, strings.ReplaceAll(stdout, leg.setID, "S-CSTRICT"))
	}
	if reports[0] != reports[1] {
		t.Errorf("--strict changed the clear report shape (inert-by-design violation):\nwithout:\n%s\nwith:\n%s",
			reports[0], reports[1])
	}
}

// --- atomicity: a forced mid-transaction failure -------------------------------

// TestClearForcedMidTransactionFailureIsAtomic: a BEFORE DELETE trigger
// on images aborts the clear AFTER the NULLing updates ran - the
// latest failure point SQL can inject. The catalog must be
// bit-identical (assignments untouched AND no run row), and a clean
// re-run after dropping the trigger must fully succeed.
func TestClearForcedMidTransactionFailureIsAtomic(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-ATOM")
	if stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", "S-ATOM", "--size", planFixtureSize); code != report.ExitOK {
		t.Fatalf("plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	seedWrittenChunkState(t, cat, "S-ATOM")

	rw := catRW(t, cat)
	if _, err := rw.Exec(`CREATE TRIGGER inject_clear_failure BEFORE DELETE ON images
		BEGIN SELECT RAISE(ABORT, 'injected clear failure'); END`); err != nil {
		t.Fatalf("install clear-failure trigger: %v", err)
	}

	before := dumpClearStateCLI(t, catOpen(t, cat))
	stdout, stderr, code := runClearChild(t, "--catalog", cat, "--set-id", "S-ATOM")
	if code != report.ExitFatal {
		t.Fatalf("triggered clear exit = %d, want 1 (fatal class)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if stdout != "" {
		t.Errorf("a failed clear must print NO report on stdout, got: %q", stdout)
	}
	if !strings.Contains(stderr, "injected clear failure") {
		t.Errorf("stderr lacks the injected failure detail:\n%s", stderr)
	}
	// Bit-identical: files, chunks, directories AND images all
	// untouched; the NULLing updates rolled back with the DELETE
	// failure.
	if after := dumpClearStateCLI(t, catOpen(t, cat)); after != before {
		t.Errorf("failed clear changed the catalog (atomicity violation):\nbefore:\n%s\nafter:\n%s", before, after)
	}
	// No audit row for the rolled-back run (the in-transaction runs
	// insert rolls back with everything else).
	db := catOpen(t, cat)
	var clearRuns int
	if err := db.QueryRow(`SELECT COUNT(*) FROM runs WHERE kind = 'clear'`).Scan(&clearRuns); err != nil {
		t.Fatalf("count clear runs: %v", err)
	}
	if clearRuns != 0 {
		t.Errorf("failed clear left %d 'clear' run rows; want 0 (bit-identical rollback)", clearRuns)
	}

	// Recovery: drop the trigger, the same clear fully succeeds.
	if _, err := rw.Exec(`DROP TRIGGER inject_clear_failure`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	stdout, stderr, code = runClearChild(t, "--catalog", cat, "--set-id", "S-ATOM")
	if code != report.ExitOK {
		t.Fatalf("recovery clear exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "cleared: 3 files, 2 chunks, 1 dirs") {
		t.Errorf("recovery clear report lacks the cleared line:\n%s", stdout)
	}
}

// --- concurrent reader (the T2.4 acceptance criterion) -------------------------

// clearMidTxMain replays the ClearPlan statement sequence (see
// internal/catalog/clearstore.go) inside ONE open transaction: it
// executes every statement, announces readiness, and commits ONLY when
// the parent closes stdin - the deterministic "clear held
// mid-transaction" the reader test reads against. It opens the
// database directly (test helper, no flock): the flock itself is
// covered by TestClearLockContentionExit5; this helper exercises the
// WAL layer the amended DESIGN_SPEC 2.1 reader guarantee lives on.
func clearMidTxMain() int {
	path := os.Getenv(clearMidTxPathEnv)
	setID := os.Getenv(clearMidTxSetEnv)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "clear mid-tx: open: %v\n", err)
		return 1
	}
	defer db.Close()
	conn, err := db.Conn(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "clear mid-tx: conn: %v\n", err)
		return 1
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		fmt.Fprintf(os.Stderr, "clear mid-tx: BEGIN IMMEDIATE: %v\n", err)
		return 1
	}
	exec := func(query string, args ...any) {
		if _, err := conn.ExecContext(context.Background(), query, args...); err != nil {
			fmt.Fprintf(os.Stderr, "clear mid-tx: %v\n", err)
			os.Exit(1)
		}
	}
	var root string
	if err := conn.QueryRowContext(context.Background(),
		`SELECT source_root FROM sets WHERE set_id = ?`, setID).Scan(&root); err != nil {
		fmt.Fprintf(os.Stderr, "clear mid-tx: set root: %v\n", err)
		return 1
	}
	// The ClearPlan statement sequence, verbatim (pinned at the SQL
	// tier by internal/catalog/clearstore_test.go).
	exec(`UPDATE files SET image_id = NULL, copied = 0
	      WHERE set_id = ? AND (image_id IS NOT NULL OR copied != 0)`, setID)
	exec(`UPDATE chunks SET image_id = NULL, copied = 0, verified_at = NULL
	      WHERE set_id = ? AND (image_id IS NOT NULL OR copied != 0 OR verified_at IS NOT NULL)`, setID)
	exec(`UPDATE directories SET image_id = NULL WHERE set_id = ? AND image_id IS NOT NULL`, setID)
	exec(`DELETE FROM images WHERE set_id = ?`, setID)
	now := time.Now().UnixNano()
	exec(`INSERT INTO runs (set_id, kind, root, started_at, finished_at, status,
	                       exclude_json, stats_json, tool_version)
	      VALUES (?, 'clear', ?, ?, ?, 'ok', '[]', '{}', 'test-helper')`,
		setID, root, now, now)
	fmt.Println(clearMidTxReady)
	// Hold the transaction open until the parent closes stdin.
	buf := make([]byte, 1)
	_, _ = os.Stdin.Read(buf)
	if _, err := conn.ExecContext(context.Background(), "COMMIT"); err != nil {
		fmt.Fprintf(os.Stderr, "clear mid-tx: COMMIT: %v\n", err)
		return 1
	}
	return 0
}

// TestClearConcurrentReaderSeesConsistentSnapshot drives the amended
// DESIGN_SPEC 2.1 guarantee through real components while a clear is
// held MID-TRANSACTION: read-only opens (the direct ro open and the
// real `report` command) must SUCCEED without blocking - the reader
// takes no advisory flock - and observe the consistent PRE-clear WAL
// snapshot (assignments still present, no torn read); after the helper
// commits, a fresh reader sees the fully cleared state atomically.
func TestClearConcurrentReaderSeesConsistentSnapshot(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-MIDTX")
	if stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", "S-MIDTX", "--size", planFixtureSize); code != report.ExitOK {
		t.Fatalf("plan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// Seed the written+verified chunk state so the FR-010 retention
	// assertion below has stored hashes to check.
	seedWrittenChunkState(t, cat, "S-MIDTX")

	holder := exec.Command(os.Args[0])
	holder.Env = append(os.Environ(),
		clearMidTxEnv+"=1", clearMidTxPathEnv+"="+cat, clearMidTxSetEnv+"=S-MIDTX")
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
		t.Fatalf("starting clear mid-tx holder: %v", err)
	}
	defer func() {
		_ = holderStdin.Close() // EOF releases the holder's commit
		_ = holder.Wait()
	}()
	buf := make([]byte, len(clearMidTxReady)+2)
	if _, err := holderOut.Read(buf); err != nil {
		t.Fatalf("holder never became ready: %v (stderr: %s)", err, holderStderr.String())
	}
	if !strings.Contains(string(buf), clearMidTxReady) {
		t.Fatalf("holder readiness = %q, want %q (stderr: %s)", string(buf), clearMidTxReady, holderStderr.String())
	}

	// The clear is IN FLIGHT (uncommitted). A direct read-only open
	// succeeds - no flock conflict, no block - and sees the PRE-clear
	// snapshot: assignments intact (the uncommitted NULLs are
	// invisible to a WAL reader).
	mid := catOpen(t, cat)
	for _, rel := range []string{"a.txt", "sub/b.bin", "link"} {
		rows := queryFiles(t, mid, rel)
		if len(rows) != 1 || !rows[0].ImageID.Valid {
			t.Errorf("mid-clear snapshot for %q = %+v, want the PRE-clear assigned row (torn read)", rel, rows)
		}
	}
	var midImages int
	if err := mid.QueryRow(`SELECT COUNT(*) FROM images WHERE set_id = 'S-MIDTX'`).Scan(&midImages); err != nil {
		t.Fatalf("count images mid-clear: %v", err)
	}
	if midImages != 2 {
		t.Errorf("images rows mid-clear = %d, want 2 (pre-clear snapshot)", midImages)
	}

	// The real `report` command (read-only, no flock per amended 2.1)
	// SUCCEEDS during the in-flight clear over the same snapshot.
	stdout, stderr, code := runReportChild(t,
		"--catalog", cat, "--set-id", "S-MIDTX", "--size", planFixtureSize)
	if code != report.ExitOK {
		t.Fatalf("report during an in-flight clear exit = %d, want 0 (readers never block, amended 2.1)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if !strings.Contains(stdout, "report (projection): set S-MIDTX") ||
		!strings.Contains(stdout, "media: 2 destinations total (0 new, 2 pre-existing)") {
		t.Errorf("mid-clear report lacks the pre-clear projection:\n%s", stdout)
	}

	// Release the clear: the helper commits on stdin EOF.
	if err := holderStdin.Close(); err != nil {
		t.Fatalf("closing holder stdin: %v", err)
	}
	if err := holder.Wait(); err != nil {
		t.Fatalf("clear holder failed: %v (stderr: %s)", err, holderStderr.String())
	}

	// A fresh read-only snapshot AFTER the commit shows the fully
	// cleared state ATOMICALLY: assignments gone AND images gone
	// together - the reader sees all of the clear or none of it, never
	// a torn mixture.
	after := catOpen(t, cat)
	var residual int
	if err := after.QueryRow(`SELECT COUNT(*) FROM files
	                          WHERE set_id = 'S-MIDTX' AND (image_id IS NOT NULL OR copied != 0)`).
		Scan(&residual); err != nil {
		t.Fatalf("count residual assignments: %v", err)
	}
	var postImages int
	if err := after.QueryRow(`SELECT COUNT(*) FROM images WHERE set_id = 'S-MIDTX'`).Scan(&postImages); err != nil {
		t.Fatalf("count images after commit: %v", err)
	}
	if residual != 0 || postImages != 0 {
		t.Errorf("post-commit snapshot = %d residual assignments, %d images rows; want 0/0 (all of the clear, atomically)",
			residual, postImages)
	}
	var retained int
	if err := after.QueryRow(`SELECT COUNT(*) FROM chunks
	                          WHERE set_id = 'S-MIDTX' AND hash IS NOT NULL`).Scan(&retained); err != nil {
		t.Fatalf("count retained chunk rows: %v", err)
	}
	if retained != 2 {
		t.Errorf("chunk rows with hashes after commit = %d, want 2 (FR-010 retention across the concurrent read)", retained)
	}
}

// --- contention (D-11 single-writer, exit 5) -----------------------------------

// TestClearLockContentionExit5: while another process holds the
// MUTATING catalog open (the T0.4 re-exec holder - the same flock a
// concurrent clear run would hold), clear fails fast with exit 5.
func TestClearLockContentionExit5(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-CBUSY")
	if stdout, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", "S-CBUSY", "--size", planFixtureSize); code != report.ExitOK {
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
		t.Fatalf("holder readiness = %q, want %q", string(buf), lockHoldReady)
	}

	stdout, stderr, code := runClearChild(t, "--catalog", cat, "--set-id", "S-CBUSY")
	if code != report.ExitCatalogBusy {
		t.Fatalf("clear against a locked catalog exit = %d, want 5\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "busy") {
		t.Errorf("stderr lacks the busy message:\n%s", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout must stay empty on catalog-busy, got: %q", stdout)
	}

	// AC-07: the SCOPED form takes the identical path - the flock is
	// acquired before any selector validation, so a selector-driven
	// clear against a held lock is the same exit 5 (flock behavior
	// unchanged by U.1).
	stdout, stderr, code = runClearChild(t, "--catalog", cat, "--set-id", "S-CBUSY", "--image", "1")
	if code != report.ExitCatalogBusy {
		t.Fatalf("scoped clear against a locked catalog exit = %d, want 5\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if !strings.Contains(stderr, "busy") {
		t.Errorf("scoped stderr lacks the busy message:\n%s", stderr)
	}
	if stdout != "" {
		t.Errorf("scoped stdout must stay empty on catalog-busy, got: %q", stdout)
	}
}

// --- usage and error paths (DESIGN_SPEC 10.1/10.2) ------------------------------

// TestClearUsageErrors pins the usage-class inputs (exit 2): the
// required flags (--set-id first, per the T2.4 brief), a stray
// positional through the usageArgs wrap, and the unknown-set gate;
// plus the no-auto-create divergence (missing catalog -> exit 4).
func TestClearUsageErrors(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-CUSAGE")

	for _, tc := range []struct {
		name    string
		args    []string
		wantMsg string
	}{
		{
			name:    "no --set-id",
			args:    []string{"--catalog", cat},
			wantMsg: "--set-id is required",
		},
		{
			name:    "no --catalog",
			args:    []string{"--set-id", "S-CUSAGE"},
			wantMsg: "--catalog is required",
		},
		{
			name:    "stray positional argument",
			args:    []string{"--catalog", cat, "--set-id", "S-CUSAGE", "oops"},
			wantMsg: "unknown command",
		},
		{
			name:    "unknown set",
			args:    []string{"--catalog", cat, "--set-id", "S-NOPE"},
			wantMsg: "no scanned set",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runClearChild(t, tc.args...)
			if code != report.ExitUsage {
				t.Fatalf("clear %s exit = %d, want 2\nstdout: %s\nstderr: %s", tc.name, code, stdout, stderr)
			}
			if !strings.Contains(stderr, tc.wantMsg) {
				t.Errorf("clear %s stderr lacks %q:\n%s", tc.name, tc.wantMsg, stderr)
			}
			if stdout != "" {
				t.Errorf("clear %s: stdout must stay empty on usage errors, got: %q", tc.name, stdout)
			}
		})
	}

	// No auto-create (the plan/report divergence from scan): a missing
	// catalog is a CatalogError (exit 4) whose message says "create
	// one first".
	missing := filepath.Join(t.TempDir(), "no-such.sqlite")
	stdout, stderr, code := runClearChild(t, "--catalog", missing, "--set-id", "S-404")
	if code != report.ExitCatalog {
		t.Fatalf("clear with missing catalog exit = %d, want 4\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "create one first") {
		t.Errorf("stderr lacks the catalog open guidance:\n%s", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout must stay empty on catalog errors, got: %q", stdout)
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("clear auto-created the missing catalog (stat err = %v); must NOT (creation is scan's job)", err)
	}
}

// --- nothing to clear (the fresh-scan case) -------------------------------------

// TestClearNothingAssignedExits0: a set that was scanned but never
// planned has nothing to clear - exit 0 with the "nothing to clear"
// report, ZERO changes (the catalog file stays byte-identical, no
// 'clear' run row), and the subsequent plan is a pristine fresh plan.
func TestClearNothingAssignedExits0(t *testing.T) {
	root := makePlanFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-CNONE")

	before := fileBytes(t, cat)
	stdout, stderr, code := runClearChild(t, "--catalog", cat, "--set-id", "S-CNONE")
	if code != report.ExitOK {
		t.Fatalf("clear on a fresh scan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "clear: set S-CNONE") ||
		!strings.Contains(stdout, "nothing to clear: no assignments present") {
		t.Errorf("nothing-to-clear report malformed:\n%s", stdout)
	}
	if !strings.Contains(stderr, "clear: nothing to clear (set S-CNONE)") {
		t.Errorf("stderr lacks the nothing-to-clear summary:\n%s", stderr)
	}
	// ZERO changes, end to end.
	if !bytes.Equal(before, fileBytes(t, cat)) {
		t.Fatal("catalog bytes changed across a nothing-to-clear run (zero-change violation)")
	}
	db := catOpen(t, cat)
	var clearRuns int
	if err := db.QueryRow(`SELECT COUNT(*) FROM runs WHERE kind = 'clear'`).Scan(&clearRuns); err != nil {
		t.Fatalf("count clear runs: %v", err)
	}
	if clearRuns != 0 {
		t.Errorf("nothing-to-clear run recorded %d 'clear' run rows; want 0", clearRuns)
	}
	// The subsequent plan is a pristine fresh plan (nothing was
	// consumed: no phantom locked destinations).
	stdout, stderr, code = runPlanChild(t,
		"--catalog", cat, "--set-id", "S-CNONE", "--size", planFixtureSize, "--json")
	if code != report.ExitOK {
		t.Fatalf("plan after nothing-to-clear exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	doc := decodeSingleJSONDoc(t, stdout)
	if doc["media_count"].(float64) != 2 || doc["new_destinations"].(float64) != 2 {
		t.Errorf("plan after nothing-to-clear media = %v new %v, want fresh-plan 2/2",
			doc["media_count"], doc["new_destinations"])
	}
}

// --- renderer model (golden human + frozen --json keys) --------------------------

// TestClearReportGoldenHuman pins the exact human output of the
// renderer: the cleared line, the destinations-removed line, the
// FR-010 retained-hash line, and the nothing-to-clear report.
func TestClearReportGoldenHuman(t *testing.T) {
	t.Run("cleared", func(t *testing.T) {
		rep := clearReport{
			Mode: "clear", SetID: "S-G", RunID: 7,
			FilesCleared: 3, ChunksCleared: 2, DirsCleared: 1,
			DestinationsRemoved: 2, ChunkHashesRetained: 2,
		}
		want := "clear: set S-G\n" +
			"cleared: 3 files, 2 chunks, 1 dirs\n" +
			"destinations removed: 2\n" +
			"chunk hashes retained: 2 (rows and hashes survive, FR-010; a same-profile re-plan reuses them)\n"
		var buf bytes.Buffer
		if err := writeClearReport(&buf, rep, false); err != nil {
			t.Fatalf("writeClearReport: %v", err)
		}
		if got := buf.String(); got != want {
			t.Errorf("golden human clear report mismatch:\ngot:\n%s\nwant:\n%s", got, want)
		}
	})
	t.Run("nothing to clear", func(t *testing.T) {
		rep := clearReport{Mode: "clear", SetID: "S-G", NothingToClear: true}
		want := "clear: set S-G\nnothing to clear: no assignments present\n"
		var buf bytes.Buffer
		if err := writeClearReport(&buf, rep, false); err != nil {
			t.Fatalf("writeClearReport: %v", err)
		}
		if got := buf.String(); got != want {
			t.Errorf("golden nothing-to-clear report mismatch:\ngot:\n%s\nwant:\n%s", got, want)
		}
	})
	// DESIGN 4.6: a SCOPED run adds EXACTLY one line, "unlocked
	// images: <ids>" (sorted, comma-space joined), between the
	// destinations-removed line and the retained-hash line. The full
	// clear above stays byte-identical without it (AC-08).
	t.Run("scoped unlock", func(t *testing.T) {
		rep := clearReport{
			Mode: "clear", SetID: "S-G", RunID: 7,
			FilesCleared: 4, ChunksCleared: 2, DirsCleared: 1,
			DestinationsRemoved: 2, ChunkHashesRetained: 4,
			UnlockedImages: []int64{1, 3},
			scoped:         true,
		}
		want := "clear: set S-G\n" +
			"cleared: 4 files, 2 chunks, 1 dirs\n" +
			"destinations removed: 2\n" +
			"unlocked images: 1, 3\n" +
			"chunk hashes retained: 4 (rows and hashes survive, FR-010; a same-profile re-plan reuses them)\n"
		var buf bytes.Buffer
		if err := writeClearReport(&buf, rep, false); err != nil {
			t.Fatalf("writeClearReport: %v", err)
		}
		if got := buf.String(); got != want {
			t.Errorf("golden scoped human report mismatch:\ngot:\n%s\nwant:\n%s", got, want)
		}
	})
}

// TestClearReportJSONSchemaFrozen pins the --json schema: the frozen
// 10-key top-level set - the deliberate T6.2 addition of
// `unlocked_images` on top of the T2.4 9-key set (documented in
// clear.go's clearReportJSONKeys, the ratified frozen-key protocol) -
// identical for the cleared and nothing-to-clear cases, with a value
// spot-check. The e2e test above asserts the REAL command emits this
// same set.
func TestClearReportJSONSchemaFrozen(t *testing.T) {
	marshal := func(rep clearReport) map[string]any {
		t.Helper()
		var buf bytes.Buffer
		if err := writeClearReport(&buf, rep, true); err != nil {
			t.Fatalf("writeClearReport --json: %v", err)
		}
		return decodeSingleJSONDoc(t, buf.String())
	}
	cleared := marshal(clearReport{
		Mode: "clear", SetID: "S-J", RunID: 9,
		FilesCleared: 3, ChunksCleared: 2, DirsCleared: 1,
		DestinationsRemoved: 2, ChunkHashesRetained: 2,
		UnlockedImages: []int64{1, 2},
	})
	noop := marshal(clearReport{Mode: "clear", SetID: "S-J", NothingToClear: true})

	wantKeys := append([]string(nil), clearReportJSONKeys...)
	sort.Strings(wantKeys)
	for _, doc := range []map[string]any{cleared, noop} {
		if got := sortedKeys(doc); !sameStrings(got, wantKeys) {
			t.Errorf("clear --json key set = %v\nwant exactly %v (schema freeze)", got, wantKeys)
		}
	}
	if cleared["run_id"].(float64) != 9 || cleared["files_cleared"].(float64) != 3 ||
		cleared["nothing_to_clear"] != false {
		t.Errorf("cleared json values = %v, want run_id 9, files 3, nothing false", cleared)
	}
	if noop["nothing_to_clear"] != true || noop["run_id"].(float64) != 0 {
		t.Errorf("noop json = nothing %v run %v, want true/0", noop["nothing_to_clear"], noop["run_id"])
	}
	// THE T6.2 FROZEN-KEY ADDITION (9 -> 10): `unlocked_images` is
	// present in BOTH modes and always a JSON ARRAY - sorted ids on a
	// real clear, a non-nil empty array on a completed no-op (a nil
	// slice would encode as null and break the type pin).
	gotIDs, ok := cleared["unlocked_images"].([]any)
	if !ok || len(gotIDs) != 2 || gotIDs[0] != float64(1) || gotIDs[1] != float64(2) {
		t.Errorf("cleared unlocked_images = %#v, want the sorted array [1 2]", cleared["unlocked_images"])
	}
	noopIDs, ok := noop["unlocked_images"].([]any)
	if !ok || len(noopIDs) != 0 {
		t.Errorf("noop unlocked_images = %#v, want a non-nil empty array []", noop["unlocked_images"])
	}
}

// TestClearAssignmentsIsRealCommand pins the wiring through the real
// cobra tree: the command exists, is no longer the T0.5 stub, and its
// help documents the FR-010 retention contract plus the no-auto-create
// divergence the T2.3 review asked for.
func TestClearAssignmentsIsRealCommand(t *testing.T) {
	var found bool
	for _, c := range NewRootCmd().Commands() {
		if c.Name() != "clear-assignments" {
			continue
		}
		found = true
		for _, want := range []string{"FR-010", "deletes NO file records", "NOT created"} {
			if !strings.Contains(c.Long, want) {
				t.Errorf("clear-assignments Long text lacks %q:\n%s", want, c.Long)
			}
		}
	}
	if !found {
		t.Fatal("clear-assignments missing from the root command tree")
	}
}
