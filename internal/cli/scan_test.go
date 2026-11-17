package cli

// T1.6 end-to-end acceptance tests for the real `scan` command
// (IMPLEMENTATION_PLAN T1.6 acceptance criteria; DESIGN_SPEC 12.2
// fixture spirit, 10.2 exit codes, 10.3 stream separation). Every
// test drives the REAL production entry path: a re-exec child running
// execute() over the real command tree with real process streams, the
// same pattern the T0.5 and T0.4 suites established. The catalog side
// is asserted with the SQLite driver directly (tests only - production
// SQL stays inside internal/catalog): the driver is registered by the
// catalog package import.

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"isopress/internal/catalog"
	"isopress/internal/report"
)

// --- helpers ------------------------------------------------------------------

// makeFixture builds the 12.2-spirit fixture tree in a fresh temp dir:
// nested files, a symlink, an empty directory, and two EXCLUDED paths
// (a *.tmp basename match and a pruned directory subtree). It returns
// the root and a cleanup-restorable mode for the (possibly chmod'ed)
// tree.
func makeFixture(t *testing.T) string {
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
	write("a.txt", "hello a\n")
	write("sub/b.bin", "binary-b")
	write("sub/deep/c.dat", "deep c data")
	write("junk.tmp", "excluded by basename pattern")
	write("skipme/x.log", "excluded by pruned subtree")
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatalf("mkdir empty: %v", err)
	}
	if err := os.Symlink("a.txt", filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	return root
}

// fixtureExcludes are the patterns the fixture is scanned with.
func fixtureExcludes() []string { return []string{"*.tmp", "skipme/"} }

// createCatalog creates a fresh v1 catalog via catalog.Create (the
// tests that need a PRE-EXISTING catalog use it; scan itself now
// auto-creates a missing --catalog path - see openScanCatalog in
// scan.go and the auto-create tests in scan_hardening_test.go).
func createCatalog(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "catalog.sqlite")
	if err := catalog.Create(path); err != nil {
		t.Fatalf("catalog.Create: %v", err)
	}
	return path
}

// runScanChild runs the real scan command as a child process.
func runScanChild(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	return runCLIChild(t, append([]string{"scan"}, args...)...)
}

// catOpen opens the catalog read-only for assertions.
func catOpen(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatalf("open catalog for assertions: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// catRW opens the catalog read-write for test-side seeding (direct SQL
// per the task: prove old-row preservation by setting an assignment
// before the rescan).
func catRW(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open catalog read-write: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// fileRow is the assertion shape for one files row.
type fileRow struct {
	FileID       int64
	EntryKind    string
	Hash         []byte
	LinkTarget   sql.NullString
	Status       string
	StatusDetail sql.NullString
	LastSeenRun  int64
	ModifiedOf   sql.NullInt64
	SupersededBy sql.NullInt64
	ImageID      sql.NullInt64
	Copied       bool
}

// queryFiles reads the files rows for one relpath.
func queryFiles(t *testing.T, db *sql.DB, relpath string) []fileRow {
	t.Helper()
	rows, err := db.Query(`
		SELECT file_id, entry_kind, hash, link_target, status, status_detail,
		       last_seen_run, modified_of, superseded_by, image_id, copied
		FROM files WHERE relpath = ? ORDER BY file_id`, relpath)
	if err != nil {
		t.Fatalf("query files %q: %v", relpath, err)
	}
	defer rows.Close()
	var out []fileRow
	for rows.Next() {
		var r fileRow
		if err := rows.Scan(&r.FileID, &r.EntryKind, &r.Hash, &r.LinkTarget, &r.Status,
			&r.StatusDetail, &r.LastSeenRun, &r.ModifiedOf, &r.SupersededBy,
			&r.ImageID, &r.Copied); err != nil {
			t.Fatalf("scan files row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate files: %v", err)
	}
	return out
}

// sha256Hex is the external-verifiability cross-check (D-09: sha256sum
// compatible).
func sha256Hex(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func hexOf(b []byte) string { return hex.EncodeToString(b) }

// --- first scan: fixture tree + exclusions ------------------------------------

// TestScanE2EFirstScan is the acceptance centerpiece's first half: a
// fresh scan of the fixture tree populates files/runs with correct
// hashes (one cross-checked against an independent sha256), symlinks
// get rows with targets and NULL hashes, excluded paths NEVER enter
// the catalog, the empty directory gets NO row (documented decision,
// reconciler.go), the run row is 'ingest'/'ok' with the exclude
// snapshot, and progress renders on stderr while stdout carries only
// the report (FR-046).
func TestScanE2EFirstScan(t *testing.T) {
	root := makeFixture(t)
	cat := createCatalog(t)

	args := []string{"--catalog", cat, "--set-id", "S-001"}
	for _, p := range fixtureExcludes() {
		args = append(args, "--exclude", p)
	}
	stdout, stderr, code := runScanChild(t, append(args, root)...)
	if code != report.ExitOK {
		t.Fatalf("scan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	// Progress stayed on stderr (the 9.3 plain line + the Done summary).
	if !strings.Contains(stderr, "scan(hash): files") {
		t.Errorf("stderr lacks the scan(hash) progress line:\n%s", stderr)
	}
	// stdout carries the report and NOTHING else (FR-046).
	if !strings.Contains(stdout, "scanned 4 entries") {
		t.Errorf("stdout lacks the report summary:\n%s", stdout)
	}

	db := catOpen(t, cat)

	// Regular files, hashes verified; one cross-checked via an
	// independent sha256 (sha256sum equivalent).
	for _, rel := range []string{"a.txt", "sub/b.bin", "sub/deep/c.dat"} {
		rows := queryFiles(t, db, rel)
		if len(rows) != 1 || rows[0].EntryKind != "regular" || rows[0].Status != "active" {
			t.Fatalf("rows for %s = %+v, want one active regular row", rel, rows)
		}
		want := sha256Hex(t, filepath.Join(root, filepath.FromSlash(rel)))
		if got := hexOf(rows[0].Hash); got != want {
			t.Errorf("hash for %s = %s, want %s", rel, got, want)
		}
	}

	// Symlink row: entry_kind symlink, raw target, hash NULL (2.3, D-03).
	syms := queryFiles(t, db, "link")
	if len(syms) != 1 || syms[0].EntryKind != "symlink" ||
		!syms[0].LinkTarget.Valid || syms[0].LinkTarget.String != "a.txt" ||
		syms[0].Hash != nil {
		t.Fatalf("symlink row = %+v, want entry_kind symlink, target a.txt, hash NULL", syms)
	}

	// Excluded paths NEVER enter the catalog (D-08).
	for _, rel := range []string{"junk.tmp", "skipme/x.log"} {
		if rows := queryFiles(t, db, rel); len(rows) != 0 {
			t.Errorf("excluded path %s has catalog rows: %+v", rel, rows)
		}
	}
	// The empty directory is NOT a catalog row (2.3 entry_kind CHECK
	// basis; documented in reconciler.go - the walker's EmptyDirs list
	// carries it to the later media layers).
	if rows := queryFiles(t, db, "empty"); len(rows) != 0 {
		t.Errorf("empty directory has catalog rows: %+v", rows)
	}

	// Run bookkeeping: one 'ingest' run, status ok, exclude snapshot.
	var kind, status, excludeJSON string
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&n); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if n != 1 {
		t.Errorf("runs = %d, want 1", n)
	}
	if err := db.QueryRow(`SELECT kind, status, exclude_json FROM runs`).Scan(&kind, &status, &excludeJSON); err != nil {
		t.Fatalf("read run row: %v", err)
	}
	if kind != "ingest" || status != "ok" {
		t.Errorf("run = %s/%s, want ingest/ok", kind, status)
	}
	if !strings.Contains(excludeJSON, "*.tmp") || !strings.Contains(excludeJSON, "skipme/") {
		t.Errorf("exclude_json = %s, want the effective pattern snapshot", excludeJSON)
	}
}

// --- rescan: all four 3.7 outcomes + lineage preservation ----------------------

// TestScanE2ERescan is the acceptance centerpiece's second half: after
// {add, modify, delete, leave-unchanged}, the rescan asserts every
// 3.7 outcome - new unassigned row, modified pair with
// modified_of/superseded_by where the OLD row keeps the image_id and
// copied flag seeded via direct SQL, an unchanged last_seen touch, and
// status='missing' for the deleted path.
func TestScanE2ERescan(t *testing.T) {
	root := makeFixture(t)
	cat := createCatalog(t)
	base := []string{"--catalog", cat, "--set-id", "S-001"}
	for _, p := range fixtureExcludes() {
		base = append(base, "--exclude", p)
	}

	if _, stderr, code := runScanChild(t, append(base, root)...); code != report.ExitOK {
		t.Fatalf("first scan exit = %d, want 0 (stderr: %s)", code, stderr)
	}

	// Seed a PLANNED + COPIED state on the file we are about to
	// modify, via direct SQL (the planner lands in T2.2; this proves
	// the 2.4 preservation clause through the real command path).
	rw := catRW(t, cat)
	if _, err := rw.Exec(`UPDATE files SET image_id = 1, copied = 1 WHERE relpath = 'a.txt'`); err != nil {
		t.Fatalf("seeding assignment: %v", err)
	}
	oldA := queryFiles(t, rw, "a.txt")
	if len(oldA) != 1 || !oldA[0].ImageID.Valid || !oldA[0].Copied {
		t.Fatalf("seeded row = %+v, want image_id 1 and copied", oldA)
	}

	// Mutate the tree: add, modify, delete; b.bin stays untouched.
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("brand new\n"), 0o644); err != nil {
		t.Fatalf("add new.txt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello a EDITED\n"), 0o644); err != nil {
		t.Fatalf("modify a.txt: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "sub", "deep", "c.dat")); err != nil {
		t.Fatalf("delete c.dat: %v", err)
	}

	stdout, stderr, code := runScanChild(t, append(base, root)...)
	if code != report.ExitOK {
		t.Fatalf("rescan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "modified 1") || !strings.Contains(stdout, "missing 1") {
		t.Errorf("rescan report lacks the expected counts:\n%s", stdout)
	}

	// The rescan is recorded as kind 'rescan'.
	var kind, status string
	if err := catOpen(t, cat).QueryRow(
		`SELECT kind, status FROM runs ORDER BY run_id DESC LIMIT 1`).Scan(&kind, &status); err != nil {
		t.Fatalf("read rescan run row: %v", err)
	}
	if kind != "rescan" || status != "ok" {
		t.Errorf("second run = %s/%s, want rescan/ok", kind, status)
	}

	db := catOpen(t, cat)

	// NEW: an unassigned active row.
	nw := queryFiles(t, db, "new.txt")
	if len(nw) != 1 || nw[0].Status != "active" || nw[0].ImageID.Valid || nw[0].ModifiedOf.Valid {
		t.Errorf("new.txt rows = %+v, want one active unassigned row", nw)
	}

	// MODIFIED: the versioned pair with preserved assignment.
	pair := queryFiles(t, db, "a.txt")
	if len(pair) != 2 {
		t.Fatalf("a.txt rows = %d (%+v), want 2 (old + new version)", len(pair), pair)
	}
	oldRow, newRow := pair[0], pair[1]
	if !oldRow.SupersededBy.Valid || oldRow.SupersededBy.Int64 != newRow.FileID {
		t.Errorf("old a.txt superseded_by = %v, want %d", oldRow.SupersededBy, newRow.FileID)
	}
	if !oldRow.ImageID.Valid || oldRow.ImageID.Int64 != 1 || !oldRow.Copied {
		t.Errorf("old a.txt assignment = image_id %v copied %v, want 1/true (2.4 preservation)",
			oldRow.ImageID, oldRow.Copied)
	}
	if !newRow.ModifiedOf.Valid || newRow.ModifiedOf.Int64 != oldRow.FileID ||
		newRow.ImageID.Valid || newRow.Copied || newRow.SupersededBy.Valid {
		t.Errorf("new a.txt = %+v, want unassigned lineage child of %d", newRow, oldRow.FileID)
	}
	wantNew := sha256Hex(t, filepath.Join(root, "a.txt"))
	if hexOf(newRow.Hash) != wantNew {
		t.Errorf("new a.txt hash = %s, want %s", hexOf(newRow.Hash), wantNew)
	}

	// UNCHANGED: last_seen_run touch to the rescan's run id.
	var rescanRun int64
	if err := db.QueryRow(`SELECT MAX(run_id) FROM runs WHERE kind = 'rescan'`).Scan(&rescanRun); err != nil {
		t.Fatalf("rescan run id: %v", err)
	}
	keep := queryFiles(t, db, "sub/b.bin")
	if len(keep) != 1 || keep[0].LastSeenRun != rescanRun {
		t.Errorf("sub/b.bin = %+v, want last_seen_run %d (unchanged touch)", keep, rescanRun)
	}

	// MISSING: the deleted path, status stamped, digest preserved.
	gone := queryFiles(t, db, "sub/deep/c.dat")
	if len(gone) != 1 || gone[0].Status != "missing" {
		t.Errorf("c.dat rows = %+v, want one row with status missing", gone)
	}
}

// --- --json: machine-parseable stdout only ------------------------------------

// TestScanJSONOnStdoutOnly: --json emits EXACTLY one JSON document on
// stdout (parses, carries the expected fields), and nothing else -
// progress and warnings stay on stderr (FR-046, T1.6 acceptance).
func TestScanJSONOnStdoutOnly(t *testing.T) {
	root := makeFixture(t)
	cat := createCatalog(t)
	args := []string{"--catalog", cat, "--set-id", "S-JSON", "--json"}
	for _, p := range fixtureExcludes() {
		args = append(args, "--exclude", p)
	}
	stdout, stderr, code := runScanChild(t, append(args, root)...)
	if code != report.ExitOK {
		t.Fatalf("scan --json exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if stderr == "" {
		t.Error("stderr is empty; progress must render there even under --json")
	}
	var rep struct {
		Root       string `json:"root"`
		SetID      string `json:"set_id"`
		RunID      int64  `json:"run_id"`
		Kind       string `json:"kind"`
		Status     string `json:"status"`
		Scanned    int    `json:"scanned"`
		Regular    int    `json:"regular"`
		Symlinks   int    `json:"symlinks"`
		Added      int    `json:"added"`
		Modified   int    `json:"modified"`
		Unchanged  int    `json:"unchanged"`
		Missing    int    `json:"missing"`
		Unreadable int    `json:"unreadable"`
		Changed    int    `json:"changed"`
		Bytes      int64  `json:"bytes"`
	}
	dec := json.NewDecoder(strings.NewReader(stdout))
	if err := dec.Decode(&rep); err != nil {
		t.Fatalf("stdout is not parseable JSON: %v\n%s", err, stdout)
	}
	// NOTHING else on stdout: the decoder must be at EOF after the one
	// document.
	if dec.More() {
		t.Errorf("stdout carries more than one JSON document:\n%s", stdout)
	}
	if rep.SetID != "S-JSON" || rep.Kind != "ingest" || rep.Status != "ok" {
		t.Errorf("report identity = %s/%s/%s, want S-JSON/ingest/ok", rep.SetID, rep.Kind, rep.Status)
	}
	if rep.Scanned != 4 || rep.Regular != 3 || rep.Symlinks != 1 || rep.Added != 4 {
		t.Errorf("report counts = %+v, want scanned 4, regular 3, symlinks 1, added 4", rep)
	}
	if rep.Missing != 0 || rep.Unreadable != 0 || rep.Changed != 0 {
		t.Errorf("report failure counts = %+v, want all zero", rep)
	}
	if rep.Bytes <= 0 {
		t.Errorf("report bytes = %d, want the hashed byte total", rep.Bytes)
	}
}

// --- unreadable file: exit 10, partial run --------------------------------------

// TestScanUnreadableExit10: a chmod-000 file yields an 'unreadable'
// row with hash NULL and the errno detail, the scan CONTINUES (the
// other files land), the run finalizes 'partial', and the command
// exits 10 with the failure enumerated (FR-003, 3.4, 10.2 row 7).
// Skipped as root, which reads through chmod 000.
func TestScanUnreadableExit10(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 000 does not make files unreadable")
	}
	root := makeFixture(t)
	locked := filepath.Join(root, "locked.dat")
	if err := os.WriteFile(locked, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write locked.dat: %v", err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod 000: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })

	cat := createCatalog(t)
	args := []string{"--catalog", cat, "--set-id", "S-LOCK"}
	for _, p := range fixtureExcludes() {
		args = append(args, "--exclude", p)
	}
	stdout, stderr, code := runScanChild(t, append(args, root)...)
	if code != report.ExitItemFailures {
		t.Fatalf("scan exit = %d, want 10\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "unreadable") {
		t.Errorf("report does not enumerate the unreadable file:\n%s", stdout)
	}

	// The report writer runs on the PARTIAL path too (scan.go writes
	// the end-of-run report before the exit-10 decision): the set/run
	// identity line must appear here exactly as on a clean run - a
	// failed scan is precisely when the user needs the set id to file
	// or re-run against the partial result.
	if m := setLineRE.FindStringSubmatch(stdout); m == nil {
		t.Errorf("failure-path (exit 10) report lacks the identity line 'set: <set_id> (run <run_id>)':\n%s", stdout)
	} else if m[1] != "S-LOCK" || m[2] != "1" {
		t.Errorf("failure-path identity = set %q run %q, want S-LOCK/1", m[1], m[2])
	}

	db := catOpen(t, cat)
	rows := queryFiles(t, db, "locked.dat")
	if len(rows) != 1 || rows[0].Status != "unreadable" || rows[0].Hash != nil {
		t.Fatalf("locked.dat rows = %+v, want one unreadable row with hash NULL", rows)
	}
	if !rows[0].StatusDetail.Valid || !strings.Contains(rows[0].StatusDetail.String, "permission denied") {
		t.Errorf("status_detail = %v, want the errno text", rows[0].StatusDetail)
	}
	// The run completed PARTIAL, and the healthy files still landed.
	var status string
	if err := db.QueryRow(`SELECT status FROM runs`).Scan(&status); err != nil {
		t.Fatalf("read run status: %v", err)
	}
	if status != "partial" {
		t.Errorf("run status = %q, want partial", status)
	}
	if healthy := queryFiles(t, db, "a.txt"); len(healthy) != 1 || healthy[0].Status != "active" {
		t.Errorf("a.txt = %+v, want an active row (the scan continued)", healthy)
	}
}

// --- scan-root refusal rail (T1.3 through the real command) --------------------

// TestScanRootRefusalExit2: scanning the filesystem root "/" without
// any exclusion argument refuses with exit 2 and the explanatory
// message (FR-005 safety rail, T1.3 acceptance through the T1.6
// command).
func TestScanRootRefusalExit2(t *testing.T) {
	cat := createCatalog(t)
	stdout, stderr, code := runScanChild(t, "--catalog", cat, "/")
	if code != report.ExitUsage {
		t.Fatalf("scan / exit = %d, want 2\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "refusing to scan the filesystem root") {
		t.Errorf("stderr lacks the rail's explanatory message:\n%s", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout must stay empty on the refusal, got: %q", stdout)
	}
}

// TestScanHelpDocumentsRecommendedExclusions pins the DESIGN_SPEC 3.2
// documentation contract at the help surface: scan's help carries the
// recommended exclusion list for broad roots (proc/ sys/ dev/ run/
// tmp/ mnt/ media/), names the flags that pass it (--exclude,
// --exclude-from), and states the filesystem-root-without-exclusions
// refusal rule with its exit-2 classification - the documented contract
// the walker's refusal rail (TestScanRootRefusalExit2) implements.
func TestScanHelpDocumentsRecommendedExclusions(t *testing.T) {
	stdout, stderr, code := runScanChild(t, "--help")
	if code != report.ExitOK {
		t.Fatalf("scan --help exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	for _, want := range []string{
		"proc/ sys/ dev/ run/ tmp/ mnt/ media/",
		"--exclude (repeatable)",
		"--exclude-from",
		"refused with exit 2",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("scan --help lacks the DESIGN_SPEC 3.2 fragment %q:\n%s", want, stdout)
		}
	}
}

// --- lock contention (T0.4 wiring through the real command) --------------------

// TestScanLockContentionExit5: while another process holds the
// MUTATING catalog open (the T0.4 re-exec holder pattern), scan fails
// fast with exit 5 and the busy message (DESIGN_SPEC 2.1, 10.2).
func TestScanLockContentionExit5(t *testing.T) {
	root := makeFixture(t)
	cat := createCatalog(t)

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
	line, err := bufio.NewReader(holderOut).ReadString('\n')
	if err != nil {
		t.Fatalf("holder never became ready: %v (stderr: %s)", err, holderStderr.String())
	}
	if strings.TrimSpace(line) != lockHoldReady {
		t.Fatalf("holder readiness = %q, want %q", line, lockHoldReady)
	}

	stdout, stderr, code := runScanChild(t, "--catalog", cat, "--set-id", "S-BUSY", root)
	if code != report.ExitCatalogBusy {
		t.Fatalf("scan against a locked catalog exit = %d, want 5\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "busy") {
		t.Errorf("stderr lacks the busy message:\n%s", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout must stay empty on catalog-busy, got: %q", stdout)
	}
}

// --- argument validation (the usageArgs convention, T0.5 NB N2) ----------------

// TestScanArgsValidation: `scan` with no ROOT and `scan` with two
// arguments both exit 2 through the usageArgs wrap - proving cobra's
// positional-args errors classify as UsageError, the N2 fix the T0.5
// review deferred to this task.
func TestScanArgsValidation(t *testing.T) {
	cat := createCatalog(t)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no args", nil},
		{"two args", []string{"a", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runScanChild(t, append([]string{"--catalog", cat}, tc.args...)...)
			if code != report.ExitUsage {
				t.Fatalf("scan %s exit = %d, want 2\nstdout: %s\nstderr: %s", tc.name, code, stdout, stderr)
			}
			if !strings.Contains(stderr, "accepts 1 arg(s)") {
				t.Errorf("stderr lacks cobra's args error:\n%s", stderr)
			}
			if !strings.Contains(stderr, "usage") {
				t.Errorf("stderr lacks the usage hint (DESIGN_SPEC 10.1):\n%s", stderr)
			}
			if stdout != "" {
				t.Errorf("stdout must stay empty on usage errors, got: %q", stdout)
			}
		})
	}
	// --catalog is scan's required flag: missing it is a usage error
	// even with a ROOT argument.
	root := makeFixture(t)
	stdout, stderr, code := runScanChild(t, root)
	if code != report.ExitUsage {
		t.Fatalf("scan without --catalog exit = %d, want 2 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "--catalog is required") {
		t.Errorf("stderr lacks the --catalog message:\n%s", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout must stay empty, got: %q", stdout)
	}
}

// --- set binding -----------------------------------------------------------------

// TestScanSetRootMismatchExits2: reusing a set id against a DIFFERENT
// source root is refused (exit 2) with both roots in the message -
// the catalog's EnsureSet binding surfaced through the command.
func TestScanSetRootMismatchExits2(t *testing.T) {
	rootA := makeFixture(t)
	rootB := t.TempDir()
	if err := os.WriteFile(filepath.Join(rootB, "x"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cat := createCatalog(t)
	if _, stderr, code := runScanChild(t, "--catalog", cat, "--set-id", "S-DUP", rootA); code != report.ExitOK {
		t.Fatalf("first scan exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	stdout, stderr, code := runScanChild(t, "--catalog", cat, "--set-id", "S-DUP", rootB)
	if code != report.ExitUsage {
		t.Fatalf("mismatched root exit = %d, want 2\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "bound to source root") {
		t.Errorf("stderr lacks the binding message:\n%s", stderr)
	}
}

// --- set/run identity in the human report (stakeholder request 2026-10-19) ---

// TestScanHumanReportGolden pins the FULL human report format,
// including the "set: <set_id> (run <run_id>)" identity line added
// after the counts headline (family style: plan/report/clear print
// "plan: set S1 (...)"/"clear: set S1" headers). The set id is the
// required argument of every follow-up command (plan/report/
// clear-assignments take --set-id) and the 6.4 date-derived default is
// never echoed elsewhere, so the finished scan output is where the
// user learns it. The --json document is a FROZEN schema (pinned in
// scan_hardening_test.go) and is deliberately untouched by this line.
func TestScanHumanReportGolden(t *testing.T) {
	rep := scanReport{
		Root: "/src", SetID: "SHOWID", RunID: 3, Kind: "ingest", Status: "ok",
		Scanned: 4, Regular: 3, Symlinks: 1,
		Added: 4, Dirs: 1, Bytes: 27,
	}
	want := "scanned 4 entries (3 regular, 1 symlinks), 27 B hashed\n" +
		"set: SHOWID (run 3)\n" +
		"added 4, modified 0, unchanged 0, missing 0, unreadable 0, changed 0\n" +
		"empty dirs 1\n"
	var buf bytes.Buffer
	if err := writeScanReport(&buf, rep, false); err != nil {
		t.Fatalf("writeScanReport: %v", err)
	}
	if got := buf.String(); got != want {
		t.Errorf("golden human report mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// setLineRE parses the identity line of the human report; ^...$ is
// multiline so it anchors at the line boundaries inside stdout.
var setLineRE = regexp.MustCompile(`(?m)^set: (\S+) \(run ([0-9]+)\)$`)

// TestScanE2ESetLineShowsSetAndRunID: a FINISHED scan echoes its
// identity on stdout - "set: SHOWID (run 1)" for the explicit
// --set-id SHOWID, then "set: SHOWID (run 2)" for the rescan (run ids
// autoincrement in the runs table; a fresh catalog + one run row per
// scan makes 1-then-2 deterministic). The printed ids are cross-
// checked against the runs table so the report can never drift from
// its durable record.
func TestScanE2ESetLineShowsSetAndRunID(t *testing.T) {
	root := makeFixture(t)
	cat := createCatalog(t)
	args := []string{"--catalog", cat, "--set-id", "SHOWID"}
	for _, p := range fixtureExcludes() {
		args = append(args, "--exclude", p)
	}

	stdout, stderr, code := runScanChild(t, append(args, root)...)
	if code != report.ExitOK {
		t.Fatalf("first scan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	m := setLineRE.FindStringSubmatch(stdout)
	if m == nil {
		t.Fatalf("first scan stdout lacks the identity line 'set: <set_id> (run <run_id>)':\n%s", stdout)
	}
	if m[1] != "SHOWID" || m[2] != "1" {
		t.Errorf("first scan identity = set %q run %q, want SHOWID/1", m[1], m[2])
	}

	// Rescan: same set, the run id increments.
	stdout, stderr, code = runScanChild(t, append(args, root)...)
	if code != report.ExitOK {
		t.Fatalf("rescan exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	m = setLineRE.FindStringSubmatch(stdout)
	if m == nil {
		t.Fatalf("rescan stdout lacks the identity line:\n%s", stdout)
	}
	if m[1] != "SHOWID" || m[2] != "2" {
		t.Errorf("rescan identity = set %q run %q, want SHOWID/2", m[1], m[2])
	}

	// Run-id determinism precision: the rescan changed NOTHING (same
	// fixture, no edits) and STILL got its own run id - the run row is
	// minted per scan INVOCATION (StartScanRun before the walk), not
	// per change, so "one runs-row-per-scan even for a scan that adds
	// nothing" is exactly what run 2 above proves. Pin the
	// zero-change counts so a regression that re-adds the fixture
	// (which would also mint run 2, for the wrong reason) cannot
	// satisfy this test.
	if !strings.Contains(stdout,
		"added 0, modified 0, unchanged 4, missing 0, unreadable 0, changed 0\n") {
		t.Errorf("rescan was not a zero-change scan (unchanged-counts precision):\n%s", stdout)
	}

	// The printed ids match the durable runs table (report and audit
	// record never disagree).
	db := catOpen(t, cat)
	var maxRun int64
	var setID string
	if err := db.QueryRow(`SELECT MAX(run_id), MIN(set_id) FROM runs GROUP BY set_id`).
		Scan(&maxRun, &setID); err != nil {
		t.Fatalf("read runs table: %v", err)
	}
	if maxRun != 2 || setID != "SHOWID" {
		t.Errorf("runs table = max run %d / set %q, want 2/SHOWID", maxRun, setID)
	}
	// Exactly ONE run row per scan: two scans, two rows - no scan
	// mutates another's row and none is duplicated.
	var runRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&runRows); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runRows != 2 {
		t.Errorf("runs table has %d rows after two scans, want exactly 2 (one row per scan)", runRows)
	}
}

// TestScanE2ESetLineShowsDerivedSetID: a scan WITHOUT --set-id echoes
// the 6.4 date-derived default ("sanitized-basename-YYYYMMDD") in the
// identity line - the only place the user ever sees it. The parsed id
// is cross-checked against the sets table (the durable truth written
// by EnsureSet in the child process) and against deriveSetID computed
// HERE from the fixture path; the in-process derivation is accepted
// for today OR the adjacent day so a UTC-midnight rollover between the
// child's clock read and this assertion cannot flake the test.
func TestScanE2ESetLineShowsDerivedSetID(t *testing.T) {
	root := makeFixture(t)
	cat := createCatalog(t)
	args := []string{"--catalog", cat}
	for _, p := range fixtureExcludes() {
		args = append(args, "--exclude", p)
	}
	stdout, stderr, code := runScanChild(t, append(args, root)...)
	if code != report.ExitOK {
		t.Fatalf("scan without --set-id exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	m := setLineRE.FindStringSubmatch(stdout)
	if m == nil {
		t.Fatalf("stdout lacks the identity line 'set: <set_id> (run <run_id>)':\n%s", stdout)
	}
	gotSet, gotRun := m[1], m[2]

	// Durable cross-check: the child's EnsureSet row for this scan.
	db := catOpen(t, cat)
	var dbSet string
	if err := db.QueryRow(`SELECT set_id FROM sets`).Scan(&dbSet); err != nil {
		t.Fatalf("read sets table: %v", err)
	}
	if gotSet != dbSet {
		t.Errorf("identity line set = %q, catalog sets row = %q", gotSet, dbSet)
	}

	// The id carries the derived shape <sanitized-basename>-<UTC date>,
	// computed from the same deriveSetID logic with the midnight
	// tolerance described above.
	absRoot, err := filepath.Abs(root)
	if err != nil {
		t.Fatalf("abs root: %v", err)
	}
	now := time.Now()
	ok := false
	for _, at := range []time.Time{now, now.Add(-24 * time.Hour), now.Add(24 * time.Hour)} {
		if gotSet == deriveSetID(absRoot, at) {
			ok = true
			break
		}
	}
	if !ok {
		t.Errorf("identity line set = %q, want the date-derived default for %q", gotSet, absRoot)
	}

	// The run id is a positive integer matching the runs table.
	runID, err := strconv.ParseInt(gotRun, 10, 64)
	if err != nil || runID <= 0 {
		t.Errorf("identity line run = %q, want a positive integer", gotRun)
	}
	var dbRun int64
	if err := db.QueryRow(`SELECT MAX(run_id) FROM runs`).Scan(&dbRun); err != nil {
		t.Fatalf("read runs table: %v", err)
	}
	if runID != dbRun {
		t.Errorf("identity line run = %d, runs table max = %d", runID, dbRun)
	}
}
