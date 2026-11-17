package cli

// T1.6 hardening tests: gap-closing additions from the sprint-capstone
// test audit. They extend scan_test.go's acceptance coverage with the
// integration-danger-zone probes the capstone review identified:
//
//  1. NOT-SEEN SAFETY under a failing FINAL BATCH FLUSH: a scan whose
//     only (and final) flush dies must skip the not-seen pass, leave
//     every previously active row untouched, finalize the run 'failed',
//     and exit non-zero. Injected deterministically with a SQL trigger
//     on the files table (no sleeps, no timing races) - the trigger
//     lives in the catalog file itself, so the re-exec child hits it
//     exactly at the flush site, and the dropped-trigger rescan proves
//     the catalog was left intact by the rolled-back batch.
//  2. SYMLINK KIND-CHANGE through the REAL scanstore path (both
//     directions: regular -> symlink via ReconcileSymlinks, symlink ->
//     regular via the hash-batch path), with 2.4 assignment
//     preservation asserted on the old row - the reconciler unit tests
//     pin the decision table over a fake store; these pin the same
//     versioning through catalog.ScanTx's real SQL.
//  3. SET-ID SEMANTICS: a rescan WITHOUT --set-id derives a fresh
//     date-based set id (6.4), so it starts a NEW set ('ingest' run)
//     and never touches the rows of the --set-id scan - documented
//     behavior, now pinned.
//  4. CRASHED PRIOR RUN ROW: a prior run left at status='running' does
//     not stop the next scan; it classifies as 'rescan' (PriorScanRuns
//     counts scan-run rows regardless of status - pinned deliberately;
//     the T1.6 fix round aligned the scanstore.go comment to this
//     behavior, see the test comment there).
//  5. --json SCHEMA STABILITY: the exact key SET of the --json report
//     is frozen for both the clean case (16 keys; "failures" omitted by
//     omitempty) and the failure case (17 keys), stdout carries exactly
//     one JSON document and nothing else (no progress leakage), and the
//     document's numbers are asserted CONSISTENT with the run row's
//     stats_json (same source of truth, two presentations). T2.2
//     (2026-10-19): "dirs" joined the schema with the empty-dir
//     durability slice - deliberate addition, frozen sets moved
//     15/16 -> 16/17.
//  6. --strict PROMOTION through the real command: item failures exit 1
//     (not 10) with the report still on stdout, in both human and
//     --json form.
//  7. CATALOG AUTO-CREATE (PM scope item, ratified 2026-10-19; landed
//     in the T1.6 fix round, flipping the earlier gap pin): scan against
//     a --catalog path that does not exist auto-creates a fresh v1
//     catalog there and proceeds - exit 0 with the ingest run row
//     reflecting reality. Pinned three ways: the single-scan flip, a
//     two-children create race on the SAME missing path (clean
//     interleavings only: exits 0/4/5, valid catalog afterwards, no
//     torn state), and run-row veracity on the created catalog.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"isopress/internal/catalog"
	"isopress/internal/report"
)

// --- 7. catalog auto-create (PM scope item, ratified 2026-10-19) ----------------

// TestScanMissingCatalogAutoCreates pins the PM-ratified auto-create
// behavior (the T1.6 fix round flipped this test from the earlier gap
// pin): scan against a --catalog path that does not exist initializes a
// fresh v1 catalog THERE, the scan proceeds to completion (exit 0), and
// the created catalog's ingest run row reflects reality - kind
// 'ingest', status 'ok', the scanned root - with the fixture's files
// recorded as active rows (run-row veracity half of the fix-round
// acceptance).
func TestScanMissingCatalogAutoCreates(t *testing.T) {
	root := makeFixture(t)
	cat := filepath.Join(t.TempDir(), "auto-created.sqlite")
	args := []string{"--catalog", cat, "--set-id", "S-AUTO"}
	for _, p := range fixtureExcludes() {
		args = append(args, "--exclude", p)
	}

	stdout, stderr, code := runScanChild(t, append(args, root)...)
	if code != report.ExitOK {
		t.Fatalf("scan with missing catalog exit = %d, want 0 (auto-create)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}

	// The catalog file now EXISTS at the requested path and is a real
	// (non-empty) initialized database, not a stub.
	fi, err := os.Stat(cat)
	if err != nil {
		t.Fatalf("catalog file was not created at %s: %v", cat, err)
	}
	if fi.Size() == 0 {
		t.Errorf("catalog file at %s is empty; want an initialized v1 database", cat)
	}

	// The scan actually ran: the human report on stdout carries the
	// fixture's additions.
	if !strings.Contains(stdout, "added 4") || !strings.Contains(stdout, "missing 0") {
		t.Errorf("auto-create scan report lacks the expected counts:\n%s", stdout)
	}

	// RUN-ROW VERACITY: the created catalog carries exactly one scan
	// run, and it truthfully describes what was scanned.
	db := catOpen(t, cat)
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("auto-created catalog integrity_check = %q, want ok", integrity)
	}
	var runs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runs); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs != 1 {
		t.Errorf("runs rows = %d, want 1 (the auto-created scan)", runs)
	}
	var kind, status, runRoot string
	if err := db.QueryRow(`SELECT kind, status, root FROM runs`).Scan(&kind, &status, &runRoot); err != nil {
		t.Fatalf("read run row: %v", err)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("abs root: %v", err)
	}
	if kind != "ingest" || status != "ok" || runRoot != filepath.Clean(absRoot) {
		t.Errorf("run row = kind %q / status %q / root %q, want ingest / ok / %q",
			kind, status, runRoot, filepath.Clean(absRoot))
	}

	// Files present: the four included fixture entries are catalog
	// rows under the right set, active, seen by this run.
	var active int
	if err := db.QueryRow(`SELECT COUNT(*) FROM files WHERE set_id = 'S-AUTO' AND status = 'active'`).
		Scan(&active); err != nil {
		t.Fatalf("count active files: %v", err)
	}
	if active != 4 {
		t.Errorf("active file rows = %d, want 4 (the fixture with exclusions applied)", active)
	}
	rows := queryFiles(t, db, "a.txt")
	if len(rows) != 1 || rows[0].Status != "active" || rows[0].Hash == nil {
		t.Errorf("a.txt rows = %+v, want one active row with a hash", rows)
	}
	// The set was bound to the scanned root in the created catalog.
	var setRoot string
	if err := db.QueryRow(`SELECT source_root FROM sets WHERE set_id = 'S-AUTO'`).Scan(&setRoot); err != nil {
		t.Fatalf("read set binding: %v", err)
	}
	if setRoot != filepath.Clean(absRoot) {
		t.Errorf("set source_root = %q, want %q", setRoot, filepath.Clean(absRoot))
	}
}

// TestScanConcurrentCreateRaceIsTornFree races two re-exec children
// against the SAME missing --catalog path (same set id, same root),
// both started before either is waited on. Per the auto-create race
// design (openScanCatalog in scan.go), every interleaving must end
// cleanly: each child exits 0 (created or opened the catalog), 4 (lost
// the create race: clean CatalogError), or 5 (lost the exclusive flock:
// CatalogBusy) - never a panic exit and never a torn file. Afterwards
// the catalog must be a valid v1 database whose rows come from at least
// one completed run.
func TestScanConcurrentCreateRaceIsTornFree(t *testing.T) {
	root := makeFixture(t)
	// Widen the genuine-overlap window: a bigger tree keeps both
	// children busy between their Stat/Create/Open sequences.
	wide := filepath.Join(root, "wide")
	if err := os.MkdirAll(wide, 0o755); err != nil {
		t.Fatalf("mkdir wide: %v", err)
	}
	for i := 0; i < 200; i++ {
		p := filepath.Join(wide, fmt.Sprintf("f%03d.bin", i))
		if err := os.WriteFile(p, []byte(fmt.Sprintf("wide payload %d\n", i)), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}
	cat := filepath.Join(t.TempDir(), "race.sqlite")

	startChild := func() (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
		t.Helper()
		cmd := exec.Command(os.Args[0], "scan", "--catalog", cat, "--set-id", "S-RACE", root)
		cmd.Env = append(os.Environ(), cliHelperEnv+"=1")
		outBuf, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
		cmd.Stdout, cmd.Stderr = outBuf, errBuf
		if err := cmd.Start(); err != nil {
			t.Fatalf("starting race child: %v", err)
		}
		return cmd, outBuf, errBuf
	}
	waitChild := func(cmd *exec.Cmd, outBuf, errBuf *bytes.Buffer) (stderr string, code int) {
		t.Helper()
		if err := cmd.Wait(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("race child %v failed to run: %v (stderr: %s)", cmd.Args, err, errBuf.String())
			}
			return errBuf.String(), exitErr.ExitCode()
		}
		return errBuf.String(), report.ExitOK
	}

	// Both children START before either is waited on: the Stat ->
	// Create -> Open sequences genuinely overlap.
	cmdA, outA, errA := startChild()
	cmdB, outB, errB := startChild()
	stderrA, codeA := waitChild(cmdA, outA, errA)
	stderrB, codeB := waitChild(cmdB, outB, errB)

	winner := false
	for i, res := range []struct {
		stderr string
		code   int
	}{{stderrA, codeA}, {stderrB, codeB}} {
		switch res.code {
		case report.ExitOK:
			winner = true
		case report.ExitCatalog, report.ExitCatalogBusy:
			// The clean loser paths: lost the create race (exit 4, from
			// Create's non-empty refusal or the serialized DDL-race
			// migration error) or lost the exclusive flock (exit 5,
			// CatalogBusy).
		default:
			t.Errorf("race child %d exit = %d, want 0, 4, or 5 (clean create-race outcomes)\nstderr: %s",
				i, res.code, res.stderr)
		}
	}
	if !winner {
		t.Fatalf("neither child completed the scan (codes %d/%d); the create race must always admit a winner\nchild A stderr: %s\nchild B stderr: %s",
			codeA, codeB, stderrA, stderrB)
	}

	// NO TORN STATE: the raced-over catalog is a valid v1 database.
	db := catOpen(t, cat)
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("raced catalog integrity_check = %q, want ok (torn create)", integrity)
	}
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if version != catalog.SchemaVersion {
		t.Errorf("raced catalog user_version = %d, want %d", version, catalog.SchemaVersion)
	}

	// Expected rows from at least one run: every run row is a completed
	// scan ('ok' - a race loser never inserts a run row, it fails at
	// Create or at the flock), the FIRST run is the ingest that created
	// the rows, and it recorded the whole fixture.
	var runs, okRuns int
	if err := db.QueryRow(`SELECT COUNT(*), COUNT(*) FILTER (WHERE status = 'ok') FROM runs`).
		Scan(&runs, &okRuns); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runs == 0 || okRuns != runs {
		t.Fatalf("raced catalog runs = %d (ok %d), want at least one, all ok", runs, okRuns)
	}
	var firstRunID int64
	var firstKind, firstStatus string
	if err := db.QueryRow(`SELECT run_id, kind, status FROM runs ORDER BY run_id LIMIT 1`).
		Scan(&firstRunID, &firstKind, &firstStatus); err != nil {
		t.Fatalf("read first run: %v", err)
	}
	if firstKind != "ingest" || firstStatus != "ok" {
		t.Errorf("first run = kind %q / status %q, want ingest / ok", firstKind, firstStatus)
	}
	var ingested int
	if err := db.QueryRow(`SELECT COUNT(*) FROM files WHERE first_seen_run = ?`, firstRunID).
		Scan(&ingested); err != nil {
		t.Fatalf("count first-run rows: %v", err)
	}
	if ingested != 206 {
		t.Errorf("rows ingested by the first run = %d, want 206 (unfiltered fixture + wide tree)", ingested)
	}
	// The winner's scan completed clean: every row is active, none
	// missing, and exactly the expected relpaths are present.
	var total, missing int
	if err := db.QueryRow(`SELECT (SELECT COUNT(*) FROM files),
	                              (SELECT COUNT(*) FROM files WHERE status = 'missing')`).
		Scan(&total, &missing); err != nil {
		t.Fatalf("count files: %v", err)
	}
	if total != 206 || missing != 0 {
		t.Errorf("raced catalog files = %d (missing %d), want 206 and none missing", total, missing)
	}
	for _, rel := range []string{"a.txt", "sub/b.bin", "sub/deep/c.dat", "link", "wide/f000.bin", "wide/f199.bin"} {
		if rows := queryFiles(t, db, rel); len(rows) != 1 || rows[0].Status != "active" {
			t.Errorf("raced catalog rows for %q = %+v, want exactly one active row", rel, rows)
		}
	}
}

// --- 1. not-seen safety: final batch flush fails --------------------------------

// TestScanFinalBatchFlushFailureSkipsNotSeenPass probes the SAFETY gate
// of DESIGN_SPEC 3.7 through the real command: a rescan whose flush
// dies (injected via a BEFORE INSERT trigger on files - deterministic,
// no timing) must never run the not-seen pass. The catastrophic failure
// mode is exactly this: the rescan only saw part of the tree before the
// flush aborted, so a naive implementation would stamp every unseen
// active row 'missing'. Asserted: exit 1, empty stdout, run 'failed',
// zero rows stamped missing, the rolled-back batch leaves the catalog
// bit-identical, and a clean re-run after dropping the trigger recovers.
func TestScanFinalBatchFlushFailureSkipsNotSeenPass(t *testing.T) {
	root := makeFixture(t)
	cat := createCatalog(t)
	base := []string{"--catalog", cat, "--set-id", "S-FLUSH"}
	for _, p := range fixtureExcludes() {
		base = append(base, "--exclude", p)
	}

	if _, stderr, code := runScanChild(t, append(base, root)...); code != report.ExitOK {
		t.Fatalf("first scan exit = %d, want 0 (stderr: %s)", code, stderr)
	}

	// Snapshot the committed state: the four fixture rows and the first
	// run's id (asserted bit-identical after the failed rescan).
	db := catOpen(t, cat)
	var run1 int64
	if err := db.QueryRow(`SELECT MIN(run_id) FROM runs`).Scan(&run1); err != nil {
		t.Fatalf("first run id: %v", err)
	}
	var rowsBefore int
	if err := db.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&rowsBefore); err != nil {
		t.Fatalf("count files before: %v", err)
	}
	if rowsBefore != 4 {
		t.Fatalf("fixture produced %d rows, want 4", rowsBefore)
	}

	// A new file so the rescan's batch has work to do, then inject the
	// flush failure: the INSERT of boom.txt aborts inside the batch
	// transaction. The trigger is part of the catalog file, so the
	// re-exec child hits it at exactly the flush site.
	if err := os.WriteFile(filepath.Join(root, "boom.txt"), []byte("boom\n"), 0o644); err != nil {
		t.Fatalf("add boom.txt: %v", err)
	}
	rw := catRW(t, cat)
	if _, err := rw.Exec(`CREATE TRIGGER inject_flush_failure BEFORE INSERT ON files
		WHEN NEW.relpath = 'boom.txt'
		BEGIN SELECT RAISE(ABORT, 'injected batch flush failure'); END`); err != nil {
		t.Fatalf("install flush-failure trigger: %v", err)
	}

	stdout, stderr, code := runScanChild(t, append(base, root)...)
	if code != report.ExitFatal {
		t.Fatalf("failed-flush rescan exit = %d, want 1 (fatal class)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if stdout != "" {
		t.Errorf("a failed scan must print NO report on stdout, got: %q", stdout)
	}
	// Abort-summary accounting (PM decision, defect C of 2026-10-19):
	// the summary names BOTH counts - reconciled rows (0: the failing
	// batch rolled back wholesale, so nothing was committed) of the
	// completed hashes (4: a.txt, sub/b.bin, sub/deep/c.dat and
	// boom.txt were all hashed; the fixture symlink is never hashed).
	if !strings.Contains(stderr, "aborted: 0 of 4 completed files reconciled") {
		t.Errorf("stderr lacks the reconciled-of-completed abort summary:\n%s", stderr)
	}
	if !strings.Contains(stderr, "a rescan will capture it") {
		t.Errorf("stderr lacks the rescan reassurance clause of the abort summary:\n%s", stderr)
	}

	// Run bookkeeping: the run is finalized 'failed' (2.4) ...
	var status string
	if err := db.QueryRow(`SELECT status FROM runs ORDER BY run_id DESC LIMIT 1`).Scan(&status); err != nil {
		t.Fatalf("read failed-run status: %v", err)
	}
	if status != "failed" {
		t.Errorf("failed rescan run status = %q, want failed", status)
	}

	// ... the not-seen pass NEVER ran: nothing was stamped missing, and
	// every pre-existing row is untouched - including last_seen_run,
	// which proves the aborted batch rolled back completely (3.7
	// atomic batch).
	var missing int
	if err := db.QueryRow(`SELECT COUNT(*) FROM files WHERE status = 'missing'`).Scan(&missing); err != nil {
		t.Fatalf("count missing: %v", err)
	}
	if missing != 0 {
		t.Errorf("%d rows were stamped missing by an incomplete scan (SAFETY VIOLATION, 3.7)", missing)
	}
	keep := queryFiles(t, db, "a.txt")
	if len(keep) != 1 || keep[0].Status != "active" || keep[0].SupersededBy.Valid ||
		keep[0].LastSeenRun != run1 {
		t.Errorf("a.txt after aborted rescan = %+v, want the untouched run-1 active row", keep)
	}
	if rows := queryFiles(t, db, "boom.txt"); len(rows) != 0 {
		t.Errorf("boom.txt has rows after the rolled-back batch: %+v", rows)
	}
	var rowsAfter int
	if err := db.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&rowsAfter); err != nil {
		t.Fatalf("count files after: %v", err)
	}
	if rowsAfter != rowsBefore {
		t.Errorf("files rows = %d after the failed rescan, want %d (atomic rollback)", rowsAfter, rowsBefore)
	}

	// Recovery: with the trigger dropped the same rescan succeeds, so
	// the failure left no latent damage (the catalog is intact).
	if _, err := rw.Exec(`DROP TRIGGER inject_flush_failure`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}
	stdout, stderr, code = runScanChild(t, append(base, root)...)
	if code != report.ExitOK {
		t.Fatalf("recovery rescan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "added 1") {
		t.Errorf("recovery rescan report lacks 'added 1':\n%s", stdout)
	}
	if rows := queryFiles(t, db, "boom.txt"); len(rows) != 1 || rows[0].Status != "active" {
		t.Errorf("boom.txt after recovery = %+v, want one active row", rows)
	}
}

// --- 2. symlink kind-change through the REAL scanstore path --------------------

// TestScanSymlinkKindChangeVersioningThroughCatalog drives both
// kind-change directions through the real command and the real
// catalog.ScanTx SQL (the reconciler unit tests pin the decision over a
// fake store; this pins the versioning pair, lineage, and 2.4
// assignment preservation through the actual database):
//
//	regular -> symlink : the hash-batch path hands the active regular
//	                     row to ReconcileSymlinks, which versions it;
//	symlink -> regular : ReconcileBatch versions the active symlink row.
//
// In both directions the old row keeps its image_id/copied state (seeded
// via direct SQL) and its content identity (hash or target), and the new
// row is unassigned with modified_of set.
func TestScanSymlinkKindChangeVersioningThroughCatalog(t *testing.T) {
	root := t.TempDir()
	shape := filepath.Join(root, "shape")
	if err := os.WriteFile(shape, []byte("regular v1\n"), 0o644); err != nil {
		t.Fatalf("write shape: %v", err)
	}
	cat := createCatalog(t)
	base := []string{"--catalog", cat, "--set-id", "S-KIND"}

	if _, stderr, code := runScanChild(t, append(base, root)...); code != report.ExitOK {
		t.Fatalf("scan 1 exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	db := catOpen(t, cat)
	first := queryFiles(t, db, "shape")
	if len(first) != 1 || first[0].EntryKind != "regular" {
		t.Fatalf("scan 1 shape rows = %+v, want one regular row", first)
	}
	oldID, hashV1 := first[0].FileID, append([]byte(nil), first[0].Hash...)

	// Seed an assignment so 2.4 preservation is observable on the old
	// row after the kind change.
	rw := catRW(t, cat)
	if _, err := rw.Exec(`UPDATE files SET image_id = 7, copied = 1 WHERE file_id = ?`, oldID); err != nil {
		t.Fatalf("seeding assignment: %v", err)
	}

	// --- direction 1: regular -> symlink (ReconcileSymlinks path) ---
	if err := os.Remove(shape); err != nil {
		t.Fatalf("remove regular shape: %v", err)
	}
	if err := os.Symlink("target-a", shape); err != nil {
		t.Fatalf("symlink shape: %v", err)
	}
	stdout, stderr, code := runScanChild(t, append(base, root)...)
	if code != report.ExitOK {
		t.Fatalf("scan 2 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "modified 1") {
		t.Errorf("scan 2 report lacks 'modified 1':\n%s", stdout)
	}
	pair := queryFiles(t, db, "shape")
	if len(pair) != 2 {
		t.Fatalf("scan 2 shape rows = %d (%+v), want the versioned pair", len(pair), pair)
	}
	oldRow, newRow := pair[0], pair[1]
	if oldRow.FileID != oldID || !oldRow.SupersededBy.Valid || oldRow.SupersededBy.Int64 != newRow.FileID {
		t.Errorf("old row after kind change = %+v, want file %d superseded by %d", oldRow, oldID, newRow.FileID)
	}
	if oldRow.EntryKind != "regular" || hexOf(oldRow.Hash) != hexOf(hashV1) {
		t.Errorf("old row = %+v, want the regular row with its v1 hash preserved", oldRow)
	}
	if !oldRow.ImageID.Valid || oldRow.ImageID.Int64 != 7 || !oldRow.Copied {
		t.Errorf("old row assignment = image_id %v copied %v, want 7/true (2.4 preservation)",
			oldRow.ImageID, oldRow.Copied)
	}
	if newRow.EntryKind != "symlink" || !newRow.LinkTarget.Valid || newRow.LinkTarget.String != "target-a" ||
		newRow.Hash != nil || !newRow.ModifiedOf.Valid || newRow.ModifiedOf.Int64 != oldID ||
		newRow.ImageID.Valid || newRow.Copied || newRow.SupersededBy.Valid {
		t.Errorf("new row after kind change = %+v, want unassigned symlink child of %d, hash NULL", newRow, oldID)
	}

	// --- direction 2: symlink -> regular (ReconcileBatch path) ---
	if err := os.Remove(shape); err != nil {
		t.Fatalf("remove symlink shape: %v", err)
	}
	if err := os.WriteFile(shape, []byte("regular v2\n"), 0o644); err != nil {
		t.Fatalf("rewrite regular shape: %v", err)
	}
	stdout, stderr, code = runScanChild(t, append(base, root)...)
	if code != report.ExitOK {
		t.Fatalf("scan 3 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "modified 1") {
		t.Errorf("scan 3 report lacks 'modified 1':\n%s", stdout)
	}
	trio := queryFiles(t, db, "shape")
	if len(trio) != 3 {
		t.Fatalf("scan 3 shape rows = %d (%+v), want three versions", len(trio), trio)
	}
	symRow, regRow := trio[1], trio[2]
	if symRow.EntryKind != "symlink" || !symRow.SupersededBy.Valid || symRow.SupersededBy.Int64 != regRow.FileID {
		t.Errorf("symlink row after reverse kind change = %+v, want superseded by %d", symRow, regRow.FileID)
	}
	wantHash := sha256Hex(t, shape)
	if regRow.EntryKind != "regular" || hexOf(regRow.Hash) != wantHash {
		t.Errorf("new regular row = %+v, want entry_kind regular with hash %s", regRow, wantHash)
	}
	if !regRow.ModifiedOf.Valid || regRow.ModifiedOf.Int64 != symRow.FileID ||
		regRow.ImageID.Valid || regRow.Copied || regRow.SupersededBy.Valid {
		t.Errorf("new regular row = %+v, want unassigned lineage child of %d", regRow, symRow.FileID)
	}
}

// --- 3. rescan WITHOUT --set-id (6.4 default derivation) -----------------------

// TestScanWithoutSetIDDerivesNewSet pins the documented 6.4 behavior:
// the default set id is <sanitized-basename>-<ingest-date-UTC>, so a
// rescan that omits --set-id binds a DIFFERENT set and starts a fresh
// 'ingest' run. The rows of the --set-id scan are untouched - no
// not-seen pass can reach them (reconciliation is set-scoped) - so the
// practical contract is: reconcile by passing the SAME --set-id.
func TestScanWithoutSetIDDerivesNewSet(t *testing.T) {
	root := makeFixture(t)
	cat := createCatalog(t)
	base := []string{"--catalog", cat, "--set-id", "S-SET"}
	for _, p := range fixtureExcludes() {
		base = append(base, "--exclude", p)
	}

	if _, stderr, code := runScanChild(t, append(base, root)...); code != report.ExitOK {
		t.Fatalf("first scan exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	db := catOpen(t, cat)
	var run1 int64
	if err := db.QueryRow(`SELECT MIN(run_id) FROM runs`).Scan(&run1); err != nil {
		t.Fatalf("first run id: %v", err)
	}

	// Rescan WITHOUT --set-id but with the same exclusion set.
	scan2Args := []string{"--catalog", cat}
	for _, p := range fixtureExcludes() {
		scan2Args = append(scan2Args, "--exclude", p)
	}
	stdout, stderr, code := runScanChild(t, append(scan2Args, root)...) // no --set-id
	if code != report.ExitOK {
		t.Fatalf("default-set scan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "added 4") {
		t.Errorf("default-set scan report lacks the expected counts:\n%s", stdout)
	}

	// Two sets now exist; the derived one is not S-SET but carries the
	// same source root.
	var sets int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sets`).Scan(&sets); err != nil {
		t.Fatalf("count sets: %v", err)
	}
	if sets != 2 {
		t.Errorf("sets = %d, want 2 (S-SET plus the date-derived default)", sets)
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("abs root: %v", err)
	}
	var derived, derivedRoot string
	if err := db.QueryRow(`SELECT set_id, source_root FROM sets WHERE set_id != 'S-SET'`).
		Scan(&derived, &derivedRoot); err != nil {
		t.Fatalf("read derived set: %v", err)
	}
	if derived == "" || derived == "S-SET" {
		t.Errorf("derived set id = %q, want a fresh date-derived id", derived)
	}
	if derivedRoot != filepath.Clean(absRoot) {
		t.Errorf("derived set root = %q, want %q", derivedRoot, filepath.Clean(absRoot))
	}

	// Both runs are 'ingest': the derived set has no prior scan runs.
	kinds := map[string]int{}
	rows, err := db.Query(`SELECT kind, COUNT(*) FROM runs GROUP BY kind`)
	if err != nil {
		t.Fatalf("query run kinds: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		var n int
		if err := rows.Scan(&k, &n); err != nil {
			t.Fatalf("scan run kind: %v", err)
		}
		kinds[k] = n
	}
	if kinds["ingest"] != 2 || kinds["rescan"] != 0 {
		t.Errorf("run kinds = %v, want two ingest and zero rescan", kinds)
	}

	// The S-SET rows are untouched: still 4 active rows, still
	// last_seen_run = run1 (set-scoped reconciliation, 3.7).
	var active, seen1 int
	if err := db.QueryRow(`SELECT COUNT(*) FROM files WHERE set_id = 'S-SET' AND status = 'active'`).
		Scan(&active); err != nil {
		t.Fatalf("count S-SET active: %v", err)
	}
	if active != 4 {
		t.Errorf("S-SET active rows = %d, want 4 (untouched)", active)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM files WHERE set_id = 'S-SET' AND last_seen_run = ?`, run1).
		Scan(&seen1); err != nil {
		t.Fatalf("count S-SET seen: %v", err)
	}
	if seen1 != 4 {
		t.Errorf("S-SET rows still on run %d = %d, want 4 (no touch from the other set's scan)", run1, seen1)
	}
	// The derived set carries its own copies of the fixture.
	var derivedRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM files WHERE set_id = ?`, derived).Scan(&derivedRows); err != nil {
		t.Fatalf("count derived-set rows: %v", err)
	}
	if derivedRows != 4 {
		t.Errorf("derived set rows = %d, want 4", derivedRows)
	}
}

// --- 4. crashed prior run row left 'running' ------------------------------------

// TestScanAfterCrashedPriorRunRowIsRescan covers the crash-window: a
// prior scan process died after StartScanRun, leaving its run row at
// status='running'. The next scan must still proceed (no lock is held
// by a dead process), reconcile every file as unchanged, and mark
// nothing missing.
//
// It also PINS the PriorScanRuns classification: the prior run row is
// counted REGARDLESS of status, so the next scan is a 'rescan'. This
// behavior is deliberate (any prior scan INTENT for the set makes the
// next scan a rescan; a second 'ingest' would misrepresent the audit
// trail) and the T1.6 fix round aligned the scanstore.go
// PriorScanRuns comment to it - the earlier mismatch flagged in the
// hardening report is resolved on the comment side, the query was
// never changed.
func TestScanAfterCrashedPriorRunRowIsRescan(t *testing.T) {
	root := makeFixture(t)
	cat := createCatalog(t)
	base := []string{"--catalog", cat, "--set-id", "S-CRASH"}
	for _, p := range fixtureExcludes() {
		base = append(base, "--exclude", p)
	}

	if _, stderr, code := runScanChild(t, append(base, root)...); code != report.ExitOK {
		t.Fatalf("first scan exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	// Simulate the crash: the run row never got its FinishRun.
	rw := catRW(t, cat)
	if _, err := rw.Exec(`UPDATE runs SET status = 'running'`); err != nil {
		t.Fatalf("simulating crashed run row: %v", err)
	}

	stdout, stderr, code := runScanChild(t, append(base, root)...)
	if code != report.ExitOK {
		t.Fatalf("rescan after crashed run exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "unchanged 4") || !strings.Contains(stdout, "missing 0") {
		t.Errorf("rescan after crashed run report =\n%s, want unchanged 4 / missing 0", stdout)
	}

	db := catOpen(t, cat)
	var kind string
	if err := db.QueryRow(`SELECT kind FROM runs ORDER BY run_id DESC LIMIT 1`).Scan(&kind); err != nil {
		t.Fatalf("read second run kind: %v", err)
	}
	if kind != "rescan" {
		t.Errorf("run after crashed prior run = %q, want rescan (current PriorScanRuns behavior)", kind)
	}
	// Nothing was double-versioned or falsely missing; the crashed run
	// row is left as-is (it is the audit trail of the crash).
	var missing, rows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM files WHERE status = 'missing'`).Scan(&missing); err != nil {
		t.Fatalf("count missing: %v", err)
	}
	if missing != 0 {
		t.Errorf("missing rows = %d, want 0", missing)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM files`).Scan(&rows); err != nil {
		t.Fatalf("count files: %v", err)
	}
	if rows != 4 {
		t.Errorf("files rows = %d, want 4 (unchanged reconciling adds no rows)", rows)
	}
}

// --- 5. --json schema stability + stats_json consistency -------------------------

// jsonReportKeysClean is the frozen key SET of the scan --json document
// for a clean run: 16 keys ("failures" is omitted by omitempty). Any
// addition, removal, or rename is a schema change and must be a
// conscious decision - this slice is the pin.
//
// T2.2 CHANGE (2026-10-19, deliberate): "dirs" JOINED the schema - the
// empty-dir durability slice (directories table, DESIGN_SPEC 2.3/3.7)
// surfaced its count in the scan report. The frozen set was updated
// consciously from 15 to 16 keys (and the failure case from 16 to 17)
// alongside the scanReport struct change; this is an approved schema
// addition, not a regression.
var jsonReportKeysClean = []string{
	"root", "set_id", "run_id", "kind", "status", "scanned", "regular",
	"symlinks", "added", "modified", "unchanged", "missing", "unreadable",
	"changed", "dirs", "bytes",
}

// decodeSingleJSONDoc decodes EXACTLY one JSON document from s and
// asserts nothing else follows (the stdout-purity half of the FR-046
// contract).
func decodeSingleJSONDoc(t *testing.T, s string) map[string]any {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(s))
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("stdout is not parseable JSON: %v\n%s", err, s)
	}
	if dec.More() {
		t.Errorf("stdout carries more than one JSON document:\n%s", s)
	}
	if err := dec.Decode(&map[string]any{}); err != io.EOF {
		t.Errorf("decoding after the first document = %v, want io.EOF (nothing else on stdout)", err)
	}
	return doc
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sameStrings(a, b []string) bool {
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

// TestScanJSONSchemaKeySetFrozen freezes the --json document's key set
// (16 keys on a clean run since T2.2's "dirs" addition; "failures"
// appears only when there are failures - see
// TestScanStrictPromotesItemFailures for the 17-key case), asserts
// stdout carries EXACTLY one JSON document with no progress leakage,
// and cross-checks every number against the run row's stats_json -
// the report and the audit record must never disagree (they share
// recon.Counts(), and this pins it end to end).
func TestScanJSONSchemaKeySetFrozen(t *testing.T) {
	root := makeFixture(t)
	cat := createCatalog(t)
	args := []string{"--catalog", cat, "--set-id", "S-SCHEMA", "--json"}
	for _, p := range fixtureExcludes() {
		args = append(args, "--exclude", p)
	}
	stdout, stderr, code := runScanChild(t, append(args, root)...)
	if code != report.ExitOK {
		t.Fatalf("scan --json exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	// stdout purity (FR-046/10.3): the JSON document and NOTHING else -
	// no progress lines, no summary, no warnings.
	if strings.Contains(stdout, "scan(hash)") {
		t.Errorf("progress leaked onto stdout under --json:\n%s", stdout)
	}
	if got := strings.TrimSpace(stdout); !strings.HasPrefix(got, "{") || !strings.HasSuffix(got, "}") {
		t.Errorf("stdout is not exactly one JSON object:\n%s", stdout)
	}
	doc := decodeSingleJSONDoc(t, stdout)

	// The frozen key set.
	wantKeys := append([]string(nil), jsonReportKeysClean...)
	sort.Strings(wantKeys)
	if got := sortedKeys(doc); !sameStrings(got, wantKeys) {
		t.Errorf("--json key set = %v\nwant exactly %v (schema freeze: changing this must be deliberate)",
			got, wantKeys)
	}

	// Identity fields.
	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("abs root: %v", err)
	}
	if doc["root"] != filepath.Clean(absRoot) || doc["set_id"] != "S-SCHEMA" ||
		doc["kind"] != "ingest" || doc["status"] != "ok" {
		t.Errorf("report identity = root %v / set %v / kind %v / status %v",
			doc["root"], doc["set_id"], doc["kind"], doc["status"])
	}

	// Consistency with the run row: same run_id, same status, and every
	// stats_json number equal to the report number.
	db := catOpen(t, cat)
	var runID int64
	var status, statsJSON string
	if err := db.QueryRow(`SELECT run_id, status, stats_json FROM runs ORDER BY run_id DESC LIMIT 1`).
		Scan(&runID, &status, &statsJSON); err != nil {
		t.Fatalf("read run row: %v", err)
	}
	if int64(doc["run_id"].(float64)) != runID || doc["status"] != status {
		t.Errorf("report run identity = %v/%v, catalog row = %d/%s", doc["run_id"], doc["status"], runID, status)
	}
	var stats map[string]any
	if err := json.Unmarshal([]byte(statsJSON), &stats); err != nil {
		t.Fatalf("parse stats_json %q: %v", statsJSON, err)
	}
	for _, field := range []string{
		"scanned", "regular", "symlinks", "added", "modified", "unchanged",
		"missing", "unreadable", "changed", "dirs", "bytes",
	} {
		if doc[field] != stats[field] {
			t.Errorf("report %s = %v, stats_json %s = %v (inconsistent)", field, doc[field], field, stats[field])
		}
	}
	// The clean-run document omits "failures" (omitempty); stats_json
	// always carries it, at zero here.
	if doc["failures"] != nil {
		t.Errorf("clean-run report failures = %v, want absent (omitempty)", doc["failures"])
	}
	if stats["failures"] != float64(0) {
		t.Errorf("stats_json failures = %v, want 0", stats["failures"])
	}
	// And the numbers are the KNOWN truth for this fixture. T2.2: the
	// fixture's one empty directory ("empty") is the dirs truth.
	truth := map[string]float64{
		"scanned": 4, "regular": 3, "symlinks": 1, "added": 4,
		"modified": 0, "unchanged": 0, "missing": 0, "unreadable": 0, "changed": 0,
		"dirs": 1,
	}
	for field, want := range truth {
		if doc[field] != want {
			t.Errorf("report %s = %v, want %v", field, doc[field], want)
		}
	}
	if doc["bytes"].(float64) <= 0 {
		t.Errorf("report bytes = %v, want the hashed byte total", doc["bytes"])
	}

	// Progress still renders on stderr under --json.
	if !strings.Contains(stderr, "scan(hash)") {
		t.Errorf("stderr lacks the progress line under --json:\n%s", stderr)
	}
}

// --- 6. --strict promotion through the real command -----------------------------

// TestScanStrictPromotesItemFailures pins the 10.2 partial-failure
// clause for scan: --strict turns item failures into a fatal stop-at-end
// error (exit 1) while the end-of-run report still lands on stdout. The
// --json leg doubles as the FAILURE-case schema pin: the document gains
// the "failures" key (17 keys total since T2.2's "dirs" addition) and
// stays machine-parseable even
// when the command exits non-zero. Skipped as root (chmod 000 is
// permeable).
func TestScanStrictPromotesItemFailures(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 000 does not make files unreadable")
	}
	newLockedFixture := func(t *testing.T) (root, cat string) {
		t.Helper()
		root = makeFixture(t)
		locked := filepath.Join(root, "locked.dat")
		if err := os.WriteFile(locked, []byte("secret"), 0o600); err != nil {
			t.Fatalf("write locked.dat: %v", err)
		}
		if err := os.Chmod(locked, 0o000); err != nil {
			t.Fatalf("chmod 000: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })
		return root, createCatalog(t)
	}

	t.Run("human", func(t *testing.T) {
		root, cat := newLockedFixture(t)
		stdout, stderr, code := runScanChild(t, "--catalog", cat, "--set-id", "S-STRICT", "--strict", root)
		if code != report.ExitFatal {
			t.Fatalf("strict scan exit = %d, want 1\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		if !strings.Contains(stdout, "unreadable") {
			t.Errorf("strict scan must still print the report on stdout:\n%s", stdout)
		}
		if !strings.Contains(stderr, "--strict") {
			t.Errorf("stderr lacks the --strict promotion message:\n%s", stderr)
		}
		// The run itself COMPLETED partial; only the exit policy changed.
		var status string
		if err := catOpen(t, cat).QueryRow(`SELECT status FROM runs`).Scan(&status); err != nil {
			t.Fatalf("read run status: %v", err)
		}
		if status != "partial" {
			t.Errorf("run status = %q, want partial", status)
		}
	})

	t.Run("json", func(t *testing.T) {
		root, cat := newLockedFixture(t)
		stdout, stderr, code := runScanChild(t,
			"--catalog", cat, "--set-id", "S-STRICT-J", "--strict", "--json", root)
		if code != report.ExitFatal {
			t.Fatalf("strict --json scan exit = %d, want 1\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		doc := decodeSingleJSONDoc(t, stdout)

		// The failure-case schema: 17 keys - the clean 16 (T2.2)
		// plus "failures" (omitempty makes its appearance
		// data-dependent, so BOTH shapes are pinned).
		wantKeys := append([]string(nil), jsonReportKeysClean...)
		wantKeys = append(wantKeys, "failures")
		sort.Strings(wantKeys)
		if got := sortedKeys(doc); !sameStrings(got, wantKeys) {
			t.Errorf("failure-case --json key set = %v\nwant exactly %v (schema freeze)", got, wantKeys)
		}
		if doc["status"] != "partial" {
			t.Errorf("failure-case report status = %v, want partial", doc["status"])
		}
		if doc["unreadable"].(float64) != 1 {
			t.Errorf("failure-case report unreadable = %v, want 1", doc["unreadable"])
		}
		failures, ok := doc["failures"].([]any)
		if !ok || len(failures) != 1 {
			t.Errorf("failure-case report failures = %v, want one enumerated line", doc["failures"])
		} else if line, _ := failures[0].(string); !strings.Contains(line, "locked.dat") {
			t.Errorf("failure line %q does not name locked.dat", line)
		}
		if !strings.Contains(stderr, "--strict") {
			t.Errorf("stderr lacks the --strict promotion message:\n%s", stderr)
		}
	})
}

// --- 8. pre-T2.2 catalog repair through the real scan path ---------------------

// TestScanHealsPreT22Catalog pins the exact reported regression: a
// catalog stamped v1 by a PRE-T2.2 binary predates the `directories`
// table (folded into the v1 DDL with no migration, PM-ratified), and
// scan used to fail with "no such table: directories" at the first
// empty-dir upsert. The mutating Open now repairs the folded objects
// (catalog.ensureFoldedV1Objects), so the scan completes, the fixture's
// empty directory lands durably in the directories table, the report
// carries the dirs counter, and integrity_check stays ok.
func TestScanHealsPreT22Catalog(t *testing.T) {
	root := makeFixture(t)
	cat := createCatalog(t)

	// Simulate the old binary's catalog: v1-stamped, no directories
	// table. (The scenario: user upgraded the tool and rescanned an
	// existing dev catalog.)
	raw := catRW(t, cat)
	if _, err := raw.Exec(`DROP TABLE directories`); err != nil {
		t.Fatalf("simulating pre-T2.2 catalog: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close simulation connection: %v", err)
	}

	args := []string{"--catalog", cat, "--set-id", "S-PRE22"}
	for _, p := range fixtureExcludes() {
		args = append(args, "--exclude", p)
	}
	stdout, stderr, code := runScanChild(t, append(args, root)...)
	if code != report.ExitOK {
		t.Fatalf("scan against pre-T2.2 catalog exit = %d, want 0\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}

	// The operation that used to fail: the fixture's empty dir is
	// durably recorded, active.
	db := catOpen(t, cat)
	var n, active int
	if err := db.QueryRow(`SELECT COUNT(*), COALESCE(SUM(status = 'active'), 0)
		FROM directories WHERE set_id = 'S-PRE22' AND relpath = 'empty'`).Scan(&n, &active); err != nil {
		t.Fatalf("query directories: %v", err)
	}
	if n != 1 || active != 1 {
		t.Errorf("directories rows for 'empty' = %d (active %d), want exactly 1 active row", n, active)
	}
	var integrity string
	if err := db.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Errorf("integrity_check = %q, want ok", integrity)
	}
	// The human report carries the dirs counter (T2.2).
	if !strings.Contains(stdout, "empty dirs 1") {
		t.Errorf("scan report lacks the empty-dirs counter:\n%s", stdout)
	}
}
