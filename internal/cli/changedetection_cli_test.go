package cli

// Change-detection wiring acceptance (DESIGN_SPEC 3.5, RESOLVED
// 2026-10-20 - the stakeholder CIFS failure; the architect's pinned
// safety matrix at the CLI tier):
//
//   - (b) an invalid --change-detection value is a usage error (exit
//     2); the valid values flow through the real command tree;
//   - (d) THREE-POINT CONSISTENCY: scan, copy, and build all resolve
//     ONE mode per run from their source root and record the SAME
//     audit figures into runs.stats_json (the resolved mode, the
//     detected class, the provenance);
//   - (e) the 3.5 warning fires exactly once per run and ONLY for an
//     auto-detected degradation (never for a user-forced metadata
//     mode, never for full modes) - seam-pinned here, message
//     content included;
//   - (f) THE CHUNKED HARDENING: a chunked file replaced between scan
//     and copy (same size + mtime_ns - invisible to the identifier
//     comparison) FAILS AT COPY TIME with the whole-file mismatch
//     classification: the final chunk's verified bytes stay on media
//     UNMARKED, the earlier chunks' marks stand, files.copied stays 0,
//     and the file-level skip drives exit 10; the RESUMED-file case
//     (a marked chunk exists, so the all-first-write condition fails)
//     skips the check and re-marks normally - FR-041's restore anchor
//     remains the detector there, documented and pinned.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"isopress/internal/catalog"
	"isopress/internal/fsmeta"
	"isopress/internal/progress"
	"isopress/internal/report"
)

// --- flag validation (matrix (b)) --------------------------------------------

// A garbage --change-detection value is a usage error before any work
// happens (exit 2, the eager validateGlobalFlags gate); the three valid
// values parse and run (each drives a real scan of a one-file fixture).
func TestChangeDetectionFlagValidation(t *testing.T) {
	_, stderr, code := runInProcess(t, "scan", "--catalog",
		filepath.Join(t.TempDir(), "c.sqlite"), "--change-detection", "relaxed",
		filepath.Join(t.TempDir(), "root"))
	if code != report.ExitUsage {
		t.Fatalf("garbage --change-detection exit = %d, want 2 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "--change-detection") || !strings.Contains(stderr, "must be one of") {
		t.Errorf("stderr does not name the flag and its vocabulary:\n%s", stderr)
	}

	// The valid values, through the REAL tree (auto is the default and
	// is passed explicitly here for symmetry).
	for _, flag := range []string{fsmeta.FlagAuto, fsmeta.FlagFull, fsmeta.FlagMetadata} {
		t.Run(flag, func(t *testing.T) {
			root := t.TempDir()
			writeFileP(t, filepath.Join(root, "a.txt"), pattern(1, 16), 0o644)
			cat := filepath.Join(t.TempDir(), "c.sqlite")
			stdout, stderr, code := runCLIChild(t,
				"scan", "--catalog", cat, "--set-id", "S-CD",
				"--change-detection", flag, root)
			if code != 0 {
				t.Fatalf("scan --change-detection=%s exit = %d\nstdout: %s\nstderr: %s", flag, code, stdout, stderr)
			}
		})
	}
}

// --- the once-per-run warning (matrix (e)) ------------------------------------

// openRunCatalog builds a minimal catalog with one 'running' scan run
// for the warning-row assertions; it returns the mutating handle plus a
// raw sql.DB for test-side reads (the catalog package exposes no query
// surface for run_events - InsertRunEvent is the write seam).
func openRunCatalog(t *testing.T, root string) (*catalog.DB, *sql.DB, int64) {
	t.Helper()
	cat := filepath.Join(t.TempDir(), "catalog.sqlite")
	if err := catalog.Create(cat); err != nil {
		t.Fatalf("creating catalog: %v", err)
	}
	db, err := catalog.Open(cat, catalog.OpenReadWrite)
	if err != nil {
		t.Fatalf("opening catalog: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.EnsureSet(context.Background(), "S-WARN", root, time.Now().UnixNano(), catalog.ToolVersion); err != nil {
		t.Fatalf("EnsureSet: %v", err)
	}
	runID, err := db.StartScanRun(context.Background(), catalog.ScanRun{
		SetID: "S-WARN", Kind: "ingest", Root: root, StartedAt: time.Now().UnixNano(),
	})
	if err != nil {
		t.Fatalf("StartScanRun: %v", err)
	}
	return db, catOpen(t, cat), runID
}

// countWarnEvents returns the run's warn-level run_events rows whose
// detail contains needle.
func countWarnEvents(t *testing.T, db *sql.DB, runID int64, needle string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM run_events WHERE run_id = ? AND level = 'warn' AND detail LIKE '%' || ? || '%'`,
		runID, needle).Scan(&n); err != nil {
		t.Fatalf("querying run_events: %v", err)
	}
	return n
}

// TestChangeDetectionWarningOnlyForAutoDetectedDegradation pins the 3.5
// warning rule at the seam: the auto-detected CIFS degradation emits
// ONE progress.Warn (rendered on stderr) AND ONE run_events warn row,
// both carrying the spec'd content (source path, class, the degraded
// set, the retained FR-014 guarantee); a user-FORCED metadata mode and
// a detected FULL mode emit NOTHING.
func TestChangeDetectionWarningOnlyForAutoDetectedDegradation(t *testing.T) {
	cases := []struct {
		name        string
		mode        fsmeta.Mode
		wantWarned  bool
		wantDisplay string // the class as the warning renders it
	}{
		{"auto-detected cifs degradation warns", fsmeta.Mode{Set: fsmeta.ComparisonMetadata, Class: fsmeta.ClassCIFS}, true, "CIFS"},
		{"auto-detected smb3 degradation warns", fsmeta.Mode{Set: fsmeta.ComparisonMetadata, Class: fsmeta.ClassSMB3}, true, "SMB3"},
		{"user-forced metadata stays silent", fsmeta.Mode{Set: fsmeta.ComparisonMetadata, Class: fsmeta.ClassCIFS, Forced: true}, false, ""},
		{"detected full stays silent", fsmeta.Mode{Set: fsmeta.ComparisonFull, Class: fsmeta.ClassLocal}, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := "/mnt/whitebox/manga"
			db, rawDB, runID := openRunCatalog(t, root)

			bus := progress.NewBus()
			var buf bytes.Buffer
			r := progress.New(progress.Options{Mode: progress.ModePlain, Stderr: &buf})
			done := make(chan progress.Done, 1)
			go func() { done <- r.Run(bus) }()

			// EXACTLY the per-run call shape of scan/copy/build: one
			// resolution, one warn emission.
			warnChangeDetection(context.Background(), db, runID, bus, root, tc.mode)

			bus.Finish(progress.Done{Summary: "done"})
			<-done

			out := buf.String()
			rows := countWarnEvents(t, rawDB, runID, "change detection degraded")
			if !tc.wantWarned {
				if strings.Contains(out, "change detection degraded") {
					t.Errorf("stderr carries a degradation warning:\n%s", out)
				}
				if rows != 0 {
					t.Errorf("run_events warn rows = %d, want 0", rows)
				}
				return
			}
			// The message content contract (3.5's example wording):
			// source path, class name, the degraded set, and the
			// retained hash-verify guarantee - on BOTH surfaces.
			for _, needle := range []string{
				root, tc.wantDisplay, "size+mtime", "FR-014", "SHA-256",
			} {
				if !strings.Contains(out, needle) {
					t.Errorf("stderr warning lacks %q:\n%s", needle, out)
				}
			}
			if strings.Count(out, "change detection degraded") != 1 {
				t.Errorf("stderr carries %d degradation warnings, want exactly 1:\n%s",
					strings.Count(out, "change detection degraded"), out)
			}
			if rows != 1 {
				t.Errorf("run_events warn rows = %d, want exactly 1", rows)
			}
		})
	}
}

// --- stats_json audit + three-point consistency (matrix (d)) ------------------

// runStatsJSON reads the newest run row of the given kind's
// change_detection audit record.
func runStatsAudit(t *testing.T, catPath, kind string) map[string]string {
	t.Helper()
	db := catOpen(t, catPath)
	var statsJSON string
	if err := db.QueryRow(
		`SELECT stats_json FROM runs WHERE kind = ? ORDER BY run_id DESC LIMIT 1`, kind).
		Scan(&statsJSON); err != nil {
		t.Fatalf("reading %s run stats_json: %v", kind, err)
	}
	var doc struct {
		ChangeDetection map[string]string `json:"change_detection"`
	}
	if err := json.Unmarshal([]byte(statsJSON), &doc); err != nil {
		t.Fatalf("stats_json %q does not parse: %v", statsJSON, err)
	}
	if doc.ChangeDetection == nil {
		t.Fatalf("stats_json %q carries no change_detection audit record", statsJSON)
	}
	return doc.ChangeDetection
}

// TestChangeDetectionThreePointConsistency pins that scan, copy, and
// build each resolve ONE mode per run from their source root and
// record IDENTICAL audit figures (mode, detected class, provenance) -
// the same resolved rule consumed at all three detection points,
// asserted through the runs.stats_json audit trail (3.5: DB-side
// audit only; the frozen --json report key-sets are untouched).
func TestChangeDetectionThreePointConsistency(t *testing.T) {
	root := t.TempDir()
	writeFileP(t, filepath.Join(root, "a.txt"), pattern(2, 64), 0o644)
	cat := filepath.Join(t.TempDir(), "c.sqlite")
	const setID = "S-3PT"

	if _, stderr, code := runCLIChild(t,
		"scan", "--catalog", cat, "--set-id", setID, root); code != 0 {
		t.Fatalf("scan exit = %d (stderr: %s)", code, stderr)
	}
	planSynthetic(t, cat, setID, buildFixtureProfile) // a profile the 6.3 step-5 build pre-check accepts
	if _, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", filepath.Join(t.TempDir(), "dest")); code != 0 {
		t.Fatalf("copy exit = %d (stderr: %s)", code, stderr)
	}

	scanAudit := runStatsAudit(t, cat, "ingest")
	copyAudit := runStatsAudit(t, cat, "copy")
	// Auto on a local filesystem: full set, detected provenance, and
	// the same detected class everywhere (one source root, one rule).
	for _, k := range []string{"mode", "class", "provenance"} {
		if scanAudit[k] == "" {
			t.Fatalf("scan audit lacks %q: %v", k, scanAudit)
		}
		if scanAudit[k] != copyAudit[k] {
			t.Errorf("audit %q: scan = %q, copy = %q (the three points must share ONE resolved mode)",
				k, scanAudit[k], copyAudit[k])
		}
	}
	if scanAudit["mode"] != "full" || scanAudit["provenance"] != "detected" {
		t.Errorf("auto scan audit = %v, want mode=full provenance=detected", scanAudit)
	}

	// A FORCED resolution is likewise recorded (and consistent)
	// wherever it is threaded.
	forcedRoot := t.TempDir()
	writeFileP(t, filepath.Join(forcedRoot, "b.txt"), pattern(4, 32), 0o644)
	forcedCat := filepath.Join(t.TempDir(), "c.sqlite")
	if _, stderr, code := runCLIChild(t,
		"scan", "--catalog", forcedCat, "--set-id", setID,
		"--change-detection", "metadata", forcedRoot); code != 0 {
		t.Fatalf("forced scan exit = %d (stderr: %s)", code, stderr)
	}
	forced := runStatsAudit(t, forcedCat, "ingest")
	if forced["mode"] != "metadata" || forced["provenance"] != "forced" {
		t.Errorf("forced scan audit = %v, want mode=metadata provenance=forced", forced)
	}

	// The THIRD detection point (build, gated on the real xorriso like
	// every build e2e): same source root, same auto resolution, same
	// audit figures as scan and copy.
	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skip("xorriso not on PATH; the build leg of the three-point pin is gated (DESIGN_SPEC 12.6)")
	}
	dest := filepath.Join(t.TempDir(), "isodest")
	if _, stderr, code := runBuildChild(t,
		"build", "--catalog", cat, "--set-id", setID, "--dest", dest); code != 0 {
		t.Fatalf("build exit = %d (stderr: %s)", code, stderr)
	}
	buildAudit := runStatsAudit(t, cat, "build")
	for _, k := range []string{"mode", "class", "provenance"} {
		if buildAudit[k] != scanAudit[k] {
			t.Errorf("audit %q: scan = %q, build = %q (the three points must share ONE resolved mode)",
				k, scanAudit[k], buildAudit[k])
		}
	}
}

// --- the chunked whole-file hardening e2e (matrix (f)) ------------------------

// TestCopyChunkedWholeFileMismatchFailsAtCopyTime is the architect's
// matrix (f) end to end through the REAL command tree: a chunked
// parent (1,352,000 bytes -> chunks of 901,312 + 450,688 across two images)
// is replaced BETWEEN scan and copy with same-size different bytes and
// the original mtime restored - the identifier comparison (even the
// FULL set: same size, same mtime_ns, same dev, same inode via an
// in-place O_TRUNC rewrite) cannot see it. The copy-time whole-file
// hardening must:
//   - copy and MARK the first chunk (its bytes and stored hash are
//     true - the marks STAND);
//   - write + read-back-verify the FINAL chunk but leave it
//     UNMARKED on media (no mark reversal), classify the FILE
//     source-changed with the whole-file mismatch detail, keep
//     files.copied at 0 (the 5.2 mirror cannot flip), defer image 2's
//     manifest, and exit 10;
//   - on the RESUMED run the check is skipped (a chunk is already
//     marked, so the all-first-write condition fails) and the final
//     chunk re-copies and marks normally - the FR-041 restore anchor
//     remains the detector for the lineage, as documented.
func TestCopyChunkedWholeFileMismatchFailsAtCopyTime(t *testing.T) {
	root := t.TempDir()
	cat := filepath.Join(t.TempDir(), "catalog.sqlite")
	dest := filepath.Join(t.TempDir(), "dest")
	const setID = "S-WFH"

	big := pattern(9, copyBigChunkedLen) // 1,352,000 = chunks 901,312 + 450,688 under the synthetic profile (the 2026-10-20 re-baseline of 15000 = 10000 + 5000)
	writeFileP(t, filepath.Join(root, "big.bin"), big, 0o644)
	writeFileP(t, filepath.Join(root, "small.txt"), pattern(2, 64), 0o644)
	if _, stderr, code := runScanChildForCopy(t, "--catalog", cat, "--set-id", setID, root); code != 0 {
		t.Fatalf("scan exit = %d (stderr: %s)", code, stderr)
	}
	planSynthetic(t, cat, setID, copySyntheticUsable) // chunk0 fills image 1; chunk1 + small.txt land on image 2

	// The replacement: SAME length, in-place (the inode survives), the
	// ORIGINAL mtime restored - only the bytes differ from the catalog
	// record.
	bigPath := filepath.Join(root, "big.bin")
	mtime := fileMtime(t, bigPath)
	replacement := pattern(77, copyBigChunkedLen)
	if err := os.WriteFile(bigPath, replacement, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(bigPath, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json")
	if code != report.ExitItemFailures {
		t.Fatalf("copy exit = %d, want 10 (stdout: %s\nstderr: %s)", code, stdout, stderr)
	}
	var rep copyReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("copy --json does not parse: %v\n%s", err, stdout)
	}
	// The FILE-level classification: one whole-file-mismatch skip
	// naming the parent.
	if len(rep.Failures) != 1 || !strings.Contains(rep.Failures[0], "big.bin") ||
		!strings.Contains(rep.Failures[0], "whole-file sha256 mismatch") {
		t.Fatalf("failures = %v, want the big.bin whole-file mismatch line", rep.Failures)
	}

	// The catalog truth: chunk 0 marked (its bytes are true), the
	// FINAL chunk unmarked, the parent NOT copied (mirror never
	// flipped).
	db := catOpen(t, cat)
	var copied0, copied1 int
	if err := db.QueryRow(`SELECT copied FROM chunks WHERE set_id = ? AND chunk_index = 0`, setID).
		Scan(&copied0); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT copied FROM chunks WHERE set_id = ? AND chunk_index = 1`, setID).
		Scan(&copied1); err != nil {
		t.Fatal(err)
	}
	if copied0 != 1 || copied1 != 0 {
		t.Errorf("chunk copied flags = (%d, %d), want (1, 0): first mark stands, final chunk unmarked", copied0, copied1)
	}
	var parentCopied int
	if err := db.QueryRow(`SELECT copied FROM files WHERE set_id = ? AND relpath = 'big.bin'`, setID).
		Scan(&parentCopied); err != nil {
		t.Fatal(err)
	}
	if parentCopied != 0 {
		t.Errorf("files.copied = %d, want 0 (the 5.2 mirror must not flip on a whole-file mismatch)", parentCopied)
	}

	// The media truth: the final chunk's VERIFIED bytes are present
	// (honest new-content chunks; nothing rolled back). The copy reads
	// the LIVE source, so BOTH chunks hold the replacement's bytes -
	// the media honestly holds new-content chunks under the old
	// scan-time rows, the earlier chunk's mark stands (its captured
	// per-chunk hash is true for what it holds), and the restore-time
	// whole-file anchor (FR-041) is what refuses this lineage.
	assertBytes(t, filepath.Join(destImageDir(dest, setID, 1), "chunks", "big.bin", "00001"), replacement[:copyChunk0Len])
	assertBytes(t, filepath.Join(destImageDir(dest, setID, 2), "chunks", "big.bin", "00002"), replacement[copyChunk0Len:])

	// Image 2 went incomplete: no MANIFEST.json for it (the mismatch
	// deferred it); image 1 completed and wrote its manifest.
	if _, err := os.Stat(filepath.Join(destImageDir(dest, setID, 2), "MANIFEST.json")); !os.IsNotExist(err) {
		t.Errorf("image 2 manifest exists after a whole-file mismatch (stat err = %v)", err)
	}
	if _, err := os.Stat(filepath.Join(destImageDir(dest, setID, 1), "MANIFEST.json")); err != nil {
		t.Errorf("image 1 manifest missing after its completed copy: %v", err)
	}

	// The audit trail: the whole-file mismatch landed in run_events.
	if n := countWarnEvents(t, db, newestRunID(t, db, "copy"), "whole-file sha256 mismatch"); n == 0 {
		t.Error("no run_events row carries the whole-file mismatch detail")
	}

	// --- THE RESUMED RUN (the documented skip): a chunk is already
	// marked, so the all-first-write condition fails and the check
	// does not run; the final chunk re-copies (its capture verifies
	// self-consistently) and marks, the mirror flips, image 2
	// completes, and the run exits 0.
	stdout, stderr, code = runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json")
	if code != 0 {
		t.Fatalf("resumed copy exit = %d, want 0 (stdout: %s\nstderr: %s)", code, stdout, stderr)
	}
	if err := db.QueryRow(`SELECT copied FROM chunks WHERE set_id = ? AND chunk_index = 1`, setID).
		Scan(&copied1); err != nil {
		t.Fatal(err)
	}
	if copied1 != 1 {
		t.Errorf("resumed run: final chunk copied = %d, want 1 (the resume path marks normally)", copied1)
	}
	if err := db.QueryRow(`SELECT copied FROM files WHERE set_id = ? AND relpath = 'big.bin'`, setID).
		Scan(&parentCopied); err != nil {
		t.Fatal(err)
	}
	if parentCopied != 1 {
		t.Errorf("resumed run: files.copied = %d, want 1 (all chunks marked; FR-041 remains the lineage anchor)", parentCopied)
	}
	if _, err := os.Stat(filepath.Join(destImageDir(dest, setID, 2), "MANIFEST.json")); err != nil {
		t.Errorf("resumed run: image 2 manifest missing after completion: %v", err)
	}
}

// newestRunID returns the newest run id of a kind (the warning-row
// lookup above).
func newestRunID(t *testing.T, db *sql.DB, kind string) int64 {
	t.Helper()
	var id int64
	if err := db.QueryRow(`SELECT run_id FROM runs WHERE kind = ? ORDER BY run_id DESC LIMIT 1`, kind).
		Scan(&id); err != nil {
		t.Fatalf("reading newest %s run: %v", kind, err)
	}
	return id
}

// --- the metadata-mode content pin at the CLI tier (matrix (c)) ----------------

// TestCopyMetadataModeVerifyGatesReplacedWholeFile pins the (c) matrix
// end to end: on a FORCED metadata source, a replaced WHOLE file (same
// size, in-place rewrite, original mtime restored - the identifier
// comparison cannot see it even under the full set) FAILS at the
// read-back verify (FR-014), is never marked copied, leaves no final
// file behind, and the rest of the set copies cleanly (exit 10). The
// --change-detection relaxation removes only the early skip; it can
// never weaken the content gate.
func TestCopyMetadataModeVerifyGatesReplacedWholeFile(t *testing.T) {
	root := t.TempDir()
	cat := filepath.Join(t.TempDir(), "catalog.sqlite")
	dest := filepath.Join(t.TempDir(), "dest")
	const setID = "S-CDV"

	volatile := pattern(5, 512)
	writeFileP(t, filepath.Join(root, "volatile.txt"), volatile, 0o644)
	writeFileP(t, filepath.Join(root, "stable.txt"), pattern(6, 512), 0o644)
	if _, stderr, code := runScanChildForCopy(t, "--catalog", cat, "--set-id", setID, root); code != 0 {
		t.Fatalf("scan exit = %d (stderr: %s)", code, stderr)
	}
	planSynthetic(t, cat, setID, copySyntheticUsable)

	vPath := filepath.Join(root, "volatile.txt")
	mtime := fileMtime(t, vPath)
	replacement := pattern(66, 512)
	if err := os.WriteFile(vPath, replacement, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(vPath, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest,
		"--change-detection", "metadata", "--json")
	if code != report.ExitItemFailures {
		t.Fatalf("copy exit = %d, want 10 (stdout: %s\nstderr: %s)", code, stdout, stderr)
	}
	var rep copyReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("copy --json does not parse: %v\n%s", err, stdout)
	}
	if len(rep.Failures) != 1 || !strings.Contains(rep.Failures[0], "volatile.txt") ||
		!strings.Contains(rep.Failures[0], "read-back hash mismatch") {
		t.Fatalf("failures = %v, want the volatile.txt read-back mismatch", rep.Failures)
	}
	// The replaced file is never marked and never lands on media; the
	// stable file copies fine (the run continues - FR-003).
	db := catOpen(t, cat)
	var copied int
	if err := db.QueryRow(`SELECT copied FROM files WHERE set_id = ? AND relpath = 'volatile.txt'`, setID).
		Scan(&copied); err != nil {
		t.Fatal(err)
	}
	if copied != 0 {
		t.Errorf("volatile.txt copied = %d, want 0 (the verify gate must refuse the replaced content)", copied)
	}
	if _, err := os.Stat(filepath.Join(destImageDir(dest, setID, 1), "files", "volatile.txt")); !os.IsNotExist(err) {
		t.Errorf("volatile.txt exists on media after a verify failure (stat err = %v)", err)
	}
	assertBytes(t, filepath.Join(destImageDir(dest, setID, 1), "files", "stable.txt"), pattern(6, 512))
}

// --- the eligibility unit (the all-first-write gate) --------------------------

// TestWholeFileCheckEligibility pins the hardening's gate arithmetic:
// the check runs ONLY when every chunk of the parent was first-written
// in this run (seenFirst == the total), the accumulation is intact AND
// COMPLETELY IN ORDER (nextExpected == the total - the B1 feed gate's
// enforced byte-order invariant), and no decision happened - the
// fail-closed construction that keeps resumes, cross-image subsets,
// interleaved assignments, and re-copies on the pre-amendment behavior
// (FR-041 remains their detector).
func TestWholeFileCheckEligibility(t *testing.T) {
	r := &copyRunner{}
	ok := catalog.CopyChunk{ParentRelPath: "big.bin", ParentNumChunks: 3, ParentHash: make([]byte, 32)}
	wf := r.newWholeFileCheck(ok)
	if wf == nil {
		t.Fatal("newWholeFileCheck refused a well-formed parent")
	}
	if wf.eligibleAtFinalChunk() {
		t.Error("fresh state reports eligible before any chunk was copied")
	}
	wf.noteFirstWrite()
	wf.noteFirstWrite()
	if wf.eligibleAtFinalChunk() {
		t.Error("2 of 3 chunks first-written must not be eligible (a resume subset cannot feed the check)")
	}
	wf.noteFirstWrite()
	if wf.eligibleAtFinalChunk() {
		t.Error("3 of 3 first-written chunks must not be eligible while the in-order feed cursor " +
			"has not consumed all 3 chunks (the count alone never authorizes the compare)")
	}
	// The three in-order feeds advance the gate's cursor to the total
	// (feedFrom's per-chunk gating is pinned in
	// changedetection_gaps_test.go's feed test; the cursor arithmetic
	// is asserted here).
	wf.nextExpected = 3
	if !wf.eligibleAtFinalChunk() {
		t.Error("3 of 3 first-written chunks with a complete in-order accumulation must be eligible at the final chunk")
	}
	// A broken accumulation fails CLOSED to no-check.
	wf.broken = true
	if wf.eligibleAtFinalChunk() {
		t.Error("a broken accumulation must disqualify the check (never a false mismatch)")
	}
	wf.broken = false
	wf.decided = true
	if wf.eligibleAtFinalChunk() {
		t.Error("a decided parent must never re-decide")
	}

	// Malformed inventory rows never mint a check (fail-closed to the
	// normal copy path).
	if c := r.newWholeFileCheck(catalog.CopyChunk{ParentNumChunks: 0, ParentHash: make([]byte, 32)}); c != nil {
		t.Error("zero chunk count minted a check")
	}
	if c := r.newWholeFileCheck(catalog.CopyChunk{ParentNumChunks: 2, ParentHash: nil}); c != nil {
		t.Error("missing whole-file hash minted a check")
	}
}
