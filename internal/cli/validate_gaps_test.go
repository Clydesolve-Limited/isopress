package cli

// T5.2 HARDENING: adversarial gaps in the validate command's
// acceptance matrix, driven through the REAL production entry path
// (re-exec children over the real command tree and the real catalog).
// A wrong category here is a wrong backup-health DIAGNOSIS, so each
// case pins the shipped behaviour end-to-end:
//
//   - a T1.5 failure-class row (status='unreadable') whose source is
//     back in readable shape: the row is not catalog-ACTIVE, so the
//     source entry counts as `new` - asserted, not assumed;
//   - exclusions are NEVER re-applied by validate, in either
//     direction: a scan-time exclusion shows as `new` (honest for a
//     source-vs-catalog comparison), and a validate-time --exclude flag
//     hides nothing (an active record is compared regardless);
//   - an all-new run (zero catalog records, N source entries): exit 0,
//     N informational new findings, "compared 0 records";
//   - the read-only proof under a HELD EXCLUSIVE FLOCK on the real
//     sidecar (the single-writer arbitration lock): a mutating scan is
//     refused with the busy class (exit 5) - proving the lock is the
//     arbitration lock - while validate succeeds on both report paths,
//     the catalog file stays BYTE-IDENTICAL, and the WAL sidecar the
//     reader's open materializes carries zero written bytes;
//   - the chunked whole-file anchor's CHANGED direction: a source
//     whose size drifted (with the mtime restored) is `changed` under
//     BOTH comparison sets - the size field is in both, so the
//     corrupt/changed boundary never flips on a size drift;
//   - an unreadable directory during the `new` walk (T1.3 warn +
//     prune): the record findings, the exit code, and the finding
//     ORDER (anomalies lead, informational `new` tails) are all
//     unaffected.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"isopress/internal/report"
)

// --- the unreadable (T1.5 failure-class) record's source ------------------------------

// TestValidateUnreadableStatusRecordCountsAsNew pins what ships for a
// record whose catalog row carries the T1.5 failure-class status
// ('unreadable'): the active predicate (status='active' AND
// superseded_by IS NULL) excludes it from the comparison, and its
// still-present, now-readable source entry counts as informational
// `new` - never missing, never broken, never compared.
func TestValidateUnreadableStatusRecordCountsAsNew(t *testing.T) {
	const setID = "S-VUNRD"
	root := t.TempDir()
	writeFileP(t, filepath.Join(root, "keep.txt"), []byte("keep content\n"), 0o644)
	writeFileP(t, filepath.Join(root, "recovered.txt"), []byte("was unreadable at scan time\n"), 0o644)
	cat := createCatalog(t)
	scanBuildFixture(t, root, cat, setID)

	// Flip the row into the T1.5 failure class through direct SQL (the
	// fold the rescan workflow owns; validate must simply not compare
	// it). The source stays present and readable.
	db := catRW(t, cat)
	res, err := db.Exec(`UPDATE files SET status = 'unreadable', hash = NULL
		WHERE set_id = ? AND relpath = ?`, setID, "recovered.txt")
	if err != nil {
		t.Fatalf("seeding the unreadable row: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("unreadable seed affected %d rows, want 1", n)
	}

	stdout, stderr, code := runValidateChild(t, nil, "validate", "--catalog", cat, "--set-id", setID)
	if code != report.ExitOK {
		t.Fatalf("validate exit = %d, want 0 (new is informational, never an anomaly)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if !strings.Contains(stdout, "ok 1, changed 0, corrupt 0, broken 0, missing 0, new 1, anomalies 0") {
		t.Errorf("stdout lacks the counts line with recovered.txt as the single new entry:\n%s", stdout)
	}
	if !strings.Contains(stdout, "new: recovered.txt (informational; a rescan would capture it)\n") {
		t.Errorf("stdout lacks the informational new line for the unreadable row's source:\n%s", stdout)
	}
	for _, banned := range []string{"missing: recovered.txt", "broken: recovered.txt"} {
		if strings.Contains(stdout, banned) {
			t.Errorf("stdout carries %q; a non-active row's source must never be compared:\n%s", banned, stdout)
		}
	}

	// The --json form: records counts ACTIVE rows only (1), the single
	// finding is the informational new tail, and the run is status ok.
	stdout, _, code = runValidateChild(t, nil, "validate", "--catalog", cat, "--set-id", setID, "--json")
	if code != report.ExitOK {
		t.Fatalf("--json validate exit = %d, want 0\nstdout: %s", code, stdout)
	}
	var rep validateReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("--json does not parse: %v\n%s", err, stdout)
	}
	if rep.Records != 1 || rep.OK != 1 || rep.New != 1 || rep.Anomalies != 0 || rep.Status != "ok" {
		t.Errorf("json report = %+v, want records 1, ok 1, new 1, anomalies 0, status ok", rep)
	}
	if len(rep.Files) != 1 || rep.Files[0].Category != "new" || rep.Files[0].Path != "recovered.txt" {
		t.Errorf("json findings = %+v, want exactly the new/recovered.txt tail entry", rep.Files)
	}
}

// --- exclusions are never re-applied --------------------------------------------------

// TestValidateExclusionsNotReapplied pins the documented behaviour in
// BOTH directions:
//
//   - scan-time exclusion: a file the original scan skipped (via the
//     global --exclude) has no catalog record, so validate discovers it
//     as informational `new` - validate carries no filter and must not
//     pretend the exclusion still applies to a source-vs-catalog
//     comparison;
//   - validate-time exclusion: passing --exclude to validate hides
//     NOTHING - an active record is compared exactly as without the
//     flag (the flag belongs to scan; validate's input surface is
//     --catalog/--set-id/--json/--strict/--change-detection).
func TestValidateExclusionsNotReapplied(t *testing.T) {
	const setID = "S-VEXCL"
	root := t.TempDir()
	writeFileP(t, filepath.Join(root, "a.txt"), []byte("recorded content\n"), 0o644)
	writeFileP(t, filepath.Join(root, "skip.log"), []byte("excluded at scan time\n"), 0o644)
	cat := createCatalog(t)
	if stdout, stderr, code := runScanChildForCopy(t,
		"--catalog", cat, "--set-id", setID, "--exclude", "*.log", root); code != 0 {
		t.Fatalf("scan --exclude exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	// Scan-time exclusion: skip.log has no record -> new (exit 0).
	stdout, stderr, code := runValidateChild(t, nil, "validate", "--catalog", cat, "--set-id", setID)
	if code != report.ExitOK {
		t.Fatalf("validate exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "ok 1, changed 0, corrupt 0, broken 0, missing 0, new 1, anomalies 0") {
		t.Errorf("stdout lacks the counts line (a.txt ok, skip.log new):\n%s", stdout)
	}
	if !strings.Contains(stdout, "new: skip.log (informational; a rescan would capture it)\n") {
		t.Errorf("stdout lacks the new line for the scan-time-excluded file:\n%s", stdout)
	}

	// Validate-time exclusion: --exclude 'a.txt' hides nothing - a.txt
	// is still compared (ok), never new, never dropped from the counts.
	stdout, _, code = runValidateChild(t, nil,
		"validate", "--catalog", cat, "--set-id", setID, "--exclude", "a.txt")
	if code != report.ExitOK {
		t.Fatalf("validate --exclude exit = %d, want 0\nstdout: %s", code, stdout)
	}
	if strings.Contains(stdout, "new: a.txt") {
		t.Errorf("validate-time --exclude leaked into the comparison (a.txt must stay ok):\n%s", stdout)
	}
	if !strings.Contains(stdout, "ok 1, changed 0, corrupt 0, broken 0, missing 0, new 1, anomalies 0") {
		t.Errorf("counts changed under a validate-time --exclude; the flag must have no effect:\n%s", stdout)
	}
}

// --- the all-new run ------------------------------------------------------------------

// TestValidateAllNewRunExitsZero pins the zero-record edge: a set whose
// scan recorded nothing (an empty source at scan time) whose tree has
// since grown N entries. Every entry is informational new, the report
// says "compared 0 records", and the exit is 0 on both report paths.
func TestValidateAllNewRunExitsZero(t *testing.T) {
	const setID = "S-VALLNEW"
	root := t.TempDir() // empty at scan time: zero records
	cat := createCatalog(t)
	scanBuildFixture(t, root, cat, setID)
	for _, name := range []string{"alpha.txt", "mid.txt", "zeta.txt"} {
		writeFileP(t, filepath.Join(root, name), []byte("grown since the scan\n"), 0o644)
	}

	stdout, stderr, code := runValidateChild(t, nil, "validate", "--catalog", cat, "--set-id", setID)
	if code != report.ExitOK {
		t.Fatalf("all-new validate exit = %d, want 0 (new is informational)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if !strings.Contains(stdout, "ok 0, changed 0, corrupt 0, broken 0, missing 0, new 3, anomalies 0") {
		t.Errorf("stdout lacks the all-new counts line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "totals: compared 0 records, status ok") {
		t.Errorf("stdout lacks the zero-record totals line:\n%s", stdout)
	}

	stdout, _, code = runValidateChild(t, nil, "validate", "--catalog", cat, "--set-id", setID, "--json")
	if code != report.ExitOK {
		t.Fatalf("--json all-new exit = %d, want 0\nstdout: %s", code, stdout)
	}
	var rep validateReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("--json does not parse: %v\n%s", err, stdout)
	}
	if rep.Records != 0 || rep.New != 3 || rep.Anomalies != 0 || rep.Status != "ok" {
		t.Errorf("json report = %+v, want records 0, new 3, anomalies 0, status ok", rep)
	}
	if len(rep.Files) != 3 {
		t.Fatalf("json findings = %d, want 3", len(rep.Files))
	}
	for i, want := range []string{"alpha.txt", "mid.txt", "zeta.txt"} {
		if f := rep.Files[i]; f.Category != "new" || f.Path != want {
			t.Errorf("json finding %d = %+v, want new/%s (walk order)", i, f, want)
		}
	}
}

// --- the read-only proof under a held EXCLUSIVE flock ----------------------------------

// TestValidateHeldExclusiveFlockNeverBlocks holds the REAL advisory
// LOCK_EX on the catalog's sidecar lock - the single-writer arbitration
// lock - and proves, in order:
//
//  1. the lock IS the arbitration lock (a mutating scan child is
//     refused with the busy class, exit 5);
//  2. validate (both the human and the --json path) succeeds anyway,
//     with no busy error and no flock attempt of its own;
//  3. the catalog file is BYTE-IDENTICAL after both runs, and the WAL
//     sidecar SQLite's read-only WAL open materializes carries ZERO
//     written bytes (the reader performed no catalog writes at all).
func TestValidateHeldExclusiveFlockNeverBlocks(t *testing.T) {
	const setID = "S-VFLOCK"
	root := t.TempDir()
	writeFileP(t, filepath.Join(root, "data.txt"), []byte("locked-down content\n"), 0o644)
	cat := createCatalog(t)
	scanBuildFixture(t, root, cat, setID)

	before, err := os.ReadFile(cat)
	if err != nil {
		t.Fatalf("snapshot catalog: %v", err)
	}

	// Take the REAL arbitration lock (the sidecar the mutating open
	// contends on) and hold it for the rest of the test.
	lockFile, err := os.OpenFile(cat+".lock", os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open lock sidecar: %v", err)
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("holding LOCK_EX on the sidecar: %v", err)
	}
	defer func() { _ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN) }()

	// (1) The lock is the arbitration lock: a MUTATING open is refused
	// with the busy class immediately (LOCK_NB), before any write.
	_, stderr, code := runScanChildForCopy(t, "--catalog", cat, "--set-id", setID, root)
	if code != report.ExitCatalogBusy {
		t.Fatalf("scan under the held lock exit = %d, want %d (CatalogBusy)\nstderr: %s",
			code, report.ExitCatalogBusy, stderr)
	}

	// (2)+(3) validate succeeds on the human path; the catalog is
	// byte-identical afterwards.
	stdout, stderr, code := runValidateChild(t, nil, "validate", "--catalog", cat, "--set-id", setID)
	if code != report.ExitOK {
		t.Fatalf("validate under the held lock exit = %d, want 0 (read-only, no flock)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if strings.Contains(stderr, "busy") || strings.Contains(stderr, "locked") {
		t.Errorf("validate saw the busy class under a held writer lock:\n%s", stderr)
	}
	after, err := os.ReadFile(cat)
	if err != nil {
		t.Fatalf("re-read catalog: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("validate modified the catalog file; the read-only contract demands a byte-identical catalog")
	}

	// The same proof on the --json path, then the WAL sidecar check.
	if stdout, _, code := runValidateChild(t, nil,
		"validate", "--catalog", cat, "--set-id", setID, "--json"); code != report.ExitOK {
		t.Fatalf("--json validate under the held lock exit = %d, want 0\nstdout: %s", code, stdout)
	}
	after, err = os.ReadFile(cat)
	if err != nil {
		t.Fatalf("re-read catalog: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("--json validate modified the catalog file")
	}
	// SQLite's read-only WAL open may materialize an empty -wal
	// sidecar (the documented WAL-reader behaviour); it must carry
	// ZERO bytes - any non-empty wal would mean the reader wrote into
	// the log.
	if fi, err := os.Stat(cat + "-wal"); err == nil && fi.Size() != 0 {
		t.Errorf("the validate-materialized -wal sidecar carries %d bytes; a read-only run must write nothing", fi.Size())
	}
}

// --- the chunked anchor's changed direction ---------------------------------------------

// TestValidateChunkedSourceChangedDirection plans a REALLY-chunked
// file (chunk rows exist, the file row is stamped chunked), then drifts
// the SOURCE's size with the mtime restored: the whole-file hash
// differs and the size differs - `changed` under BOTH comparison sets,
// because size belongs to both resolved sets and the corrupt branch
// (metadata MATCHES) can never open on a size drift.
func TestValidateChunkedSourceChangedDirection(t *testing.T) {
	const setID = "S-VCHGCHUNK"
	root := t.TempDir()
	big := pattern(1, copyBigChunkedLen)
	writeFileP(t, filepath.Join(root, "big.bin"), big, 0o644)
	cat := createCatalog(t)
	scanBuildFixture(t, root, cat, setID)
	planSynthetic(t, cat, setID, copySyntheticUsable)

	// The chunked premise: chunk rows really exist.
	db := catOpen(t, cat)
	var chunks int
	if err := db.QueryRow(`SELECT COUNT(*) FROM chunks WHERE set_id = ?`, setID).Scan(&chunks); err != nil {
		t.Fatalf("counting chunks: %v", err)
	}
	if chunks < 2 {
		t.Fatalf("chunk rows = %d, want >= 2 (the fixture must really chunk)", chunks)
	}

	// The drift: APPEND four bytes (size differs) and restore the
	// original mtime, so the ONLY visible metadata drift is the size.
	bigPath := filepath.Join(root, "big.bin")
	info, err := os.Stat(bigPath)
	if err != nil {
		t.Fatal(err)
	}
	writeFileP(t, bigPath, append(append([]byte(nil), big...), 0xde, 0xad, 0xbe, 0xef), 0o644)
	if err := os.Chtimes(bigPath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	stdout, _, code := runValidateChild(t, nil, "validate", "--catalog", cat, "--set-id", setID)
	if code != report.ExitItemFailures {
		t.Fatalf("changed chunked validate exit = %d, want 10\nstdout: %s", code, stdout)
	}
	if !strings.Contains(stdout, "changed: big.bin (metadata: size; hash differs)\n") {
		t.Errorf("stdout lacks the size-only changed line against the whole-file anchor:\n%s", stdout)
	}
	if strings.Contains(stdout, "corrupt: big.bin") {
		t.Errorf("a size drift must never classify corrupt (the metadata set sees the size):\n%s", stdout)
	}

	// The metadata set sees the same size drift: same verdict, same
	// detail - the boundary does not flip on a size drift.
	stdout, _, code = runValidateChild(t, nil,
		"validate", "--catalog", cat, "--set-id", setID, "--change-detection=metadata")
	if code != report.ExitItemFailures {
		t.Fatalf("metadata-mode changed chunked validate exit = %d, want 10\nstdout: %s", code, stdout)
	}
	if !strings.Contains(stdout, "(comparison set: metadata)") {
		t.Errorf("stdout lacks the metadata comparison-set line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "changed: big.bin (metadata: size; hash differs)\n") {
		t.Errorf("metadata mode must classify the size drift `changed` too:\n%s", stdout)
	}
}

// --- the walk's warn+prune leaves the record findings and the exit honest ---------------

// TestValidateUnreadableWalkDirKeepsExitHonest pins the T1.3 walk
// convention end-to-end: a directory the new-entry walk cannot read
// warns and is pruned, while the CATALOG side of the comparison stays
// fully intact - the records under the pruned subtree still reach their
// verdicts (a missing deletion and a permission-blocked lstat), the
// exit is 10 for exactly those anomalies, and the report ORDER holds:
// record findings (anomalies) lead, the informational `new` tail last.
// Skipped as root, which bypasses the permission model.
func TestValidateUnreadableWalkDirKeepsExitHonest(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 000 is bypassed, unreadable-directory fixture unavailable")
	}
	const setID = "S-VWALK"
	root := t.TempDir()
	writeFileP(t, filepath.Join(root, "gone.txt"), []byte("will be deleted\n"), 0o644)
	hidden := filepath.Join(root, "hidden")
	writeFileP(t, filepath.Join(hidden, "extra.txt"), []byte("under the pruned dir\n"), 0o644)
	cat := createCatalog(t)
	scanBuildFixture(t, root, cat, setID)

	// Mutations: the hidden subtree closes to the walk, one record's
	// source is deleted outright, and a fresh source entry appears.
	if err := os.Chmod(hidden, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(hidden, 0o755) }) // TempDir cleanup needs the dir back
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	writeFileP(t, filepath.Join(root, "fresh-new.txt"), []byte("discovered by the walk\n"), 0o644)

	stdout, stderr, code := runValidateChild(t, nil, "validate", "--catalog", cat, "--set-id", setID)
	if code != report.ExitItemFailures {
		t.Fatalf("validate exit = %d, want 10 (the record anomalies stand)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	// Both record verdicts are intact despite the pruned walk: the
	// deletion is missing, the permission-blocked lstat is broken.
	for _, line := range []string{
		"missing: gone.txt\n",
		"broken: hidden/extra.txt (lstat failed: permission denied)\n",
		"new: fresh-new.txt (informational; a rescan would capture it)\n",
	} {
		if !strings.Contains(stdout, line) {
			t.Errorf("stdout lacks %q:\n%s", line, stdout)
		}
	}
	if !strings.Contains(stdout, "ok 0, changed 0, corrupt 0, broken 1, missing 1, new 1, anomalies 2") {
		t.Errorf("stdout lacks the exact counts line:\n%s", stdout)
	}
	// `new` under-reports in the pruned subtree (documented), never
	// invents entries from it.
	if strings.Contains(stdout, "new: hidden") {
		t.Errorf("the pruned subtree must not surface as new:\n%s", stdout)
	}
	// The walk warning reached stderr.
	if !strings.Contains(stderr, "warning: validate: walking "+hidden) ||
		!strings.Contains(stderr, "new-entry discovery pruned there") {
		t.Errorf("stderr lacks the pruned-walk warning naming %s:\n%s", hidden, stderr)
	}

	// The --json ORDER: record findings (anomalies) lead in record
	// order, the informational new tail closes the report.
	stdout, _, code = runValidateChild(t, nil, "validate", "--catalog", cat, "--set-id", setID, "--json")
	if code != report.ExitItemFailures {
		t.Fatalf("--json validate exit = %d, want 10\nstdout: %s", code, stdout)
	}
	var rep validateReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("--json does not parse: %v\n%s", err, stdout)
	}
	if len(rep.Files) != 3 {
		t.Fatalf("json findings = %d (%+v), want 3", len(rep.Files), rep.Files)
	}
	// The CONTRACTUAL order: every record finding (anomaly) leads, the
	// informational `new` tail is LAST. The relative order of the two
	// anomalies themselves follows file_id (the catalog read's ORDER
	// BY), and a real scan assigns file_ids in concurrent-hash
	// completion order - so it is asserted as a set, not a sequence.
	if f := rep.Files[2]; f.Category != "new" || f.Path != "fresh-new.txt" {
		t.Errorf("json finding 2 = %+v, want new/fresh-new.txt as the informational tail", f)
	}
	gotAnomalies := map[string]string{}
	for _, f := range rep.Files[:2] {
		if f.Category == "new" {
			t.Errorf("a new finding sits in the anomaly lead: %+v", f)
		}
		gotAnomalies[f.Category+":"+f.Path] = f.Detail
	}
	for _, want := range []string{"missing:gone.txt", "broken:hidden/extra.txt"} {
		if _, ok := gotAnomalies[want]; !ok {
			t.Errorf("anomaly lead lacks %q (got %+v)", want, gotAnomalies)
		}
	}
}
