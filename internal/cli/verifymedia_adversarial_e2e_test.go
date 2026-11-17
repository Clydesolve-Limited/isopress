//go:build e2e

package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"isopress/internal/report"
	"isopress/internal/verify"
)

// TestVerifyMediaUnparseableISOReportsUnreadableImage is the FLIPPED
// pin (it used to ship as TestVerifyMediaUnparseableISOShipsAsAllMissing,
// which pinned an honest verdict with a WRONG diagnosis): corrupting the
// ISO's VOLUME DESCRIPTORS (every "CD001" magic) leaves the image
// unparseable.
//
// WHAT SHIPS (pinned; empirically verified against xorriso 1.5.6 on
// 2026-10-21): xorriso does NOT fail here. It warns "No ISO 9660 image
// at LBA 0 / Creating blank image", EXITS 0, and extracts an EMPTY
// tree - Tool.Run answers without an error and (per the exit-0
// contract) keeps both streams out of its message. The zero-entry gate
// in internal/verify's readISO turns that into the honest DIAGNOSIS: a
// per-image READ failure naming the image unreadable / not an ISO 9660
// image, carrying the zero-entries-vs-expected count AND xorriso's own
// output tail - instead of "every listed payload is missing". The
// verdict half never changes (an empty extraction cannot satisfy a
// non-empty content list), so the run still exits 10 (the per-image
// failure class, the loop continuing over the remaining images), no
// payload classification is invented, and the build's row stays
// untouched.
func TestVerifyMediaUnparseableISOReportsUnreadableImage(t *testing.T) {
	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skip("xorriso not on PATH; skipping the gated parse-failure edge (DESIGN_SPEC 12.6)")
	}
	const setID = "S-VPARSE"
	root, _ := verifyISOTree(t)
	cat := createCatalog(t)
	scanBuildFixture(t, root, cat, setID)
	planSynthetic(t, cat, setID, 25<<20)
	dest := filepath.Join(t.TempDir(), "dest")
	if stdout, stderr, code := runBuildChild(t,
		"build", "--catalog", cat, "--set-id", setID, "--dest", dest); code != report.ExitOK {
		t.Fatalf("build fixture exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	iso := filepath.Join(dest, fmt.Sprintf("%s-001.iso", setID))

	// DESTROY the volume descriptors: xorriso cannot parse the image.
	img, err := os.ReadFile(iso)
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(img, []byte("CD001")); n == 0 {
		t.Fatalf("%s carries no ISO9660 volume descriptor magic - the parse-failure edge cannot be provoked", iso)
	}
	img = bytes.ReplaceAll(img, []byte("CD001"), []byte("XXXXX"))
	if err := os.WriteFile(iso, img, 0o644); err != nil {
		t.Fatal(err)
	}

	// The content rows the extraction is about to yield nothing against
	// (the N of the zero-entries-vs-N gate).
	var expected int
	if err := catOpen(t, cat).QueryRow(
		`SELECT count(*) FROM image_contents WHERE set_id = ? AND image_id = 1`, setID).Scan(&expected); err != nil {
		t.Fatalf("counting content rows: %v", err)
	}
	if expected == 0 {
		t.Fatal("the build left no content rows - the zero-entry gate cannot be provoked")
	}

	work := t.TempDir()
	stdout, stderr, code := runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID, "--json", "--work-dir", work)
	if code != report.ExitItemFailures {
		t.Fatalf("parse-broken verify exit = %d, want 10 (anomaly class, never 1, never 5)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if strings.Contains(stderr, "panic") {
		t.Errorf("an unparseable ISO must not panic:\n%s", stderr)
	}
	var rep verifyMediaReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("report does not parse: %v\n%s", err, stdout)
	}
	imgRep := verifyMediaImageOf(t, rep, 1)
	if imgRep.Status != "failed" || imgRep.Failure == "" {
		t.Fatalf("image report = %+v, want a per-image READ failure (not an all-missing mismatch)", imgRep)
	}
	// THE HONEST DIAGNOSIS: the image is unreadable and NOTHING was
	// extracted against the expected content rows.
	for _, want := range []string{
		"no extractable entries",
		fmt.Sprintf("against %d expected", expected),
		"unreadable",
		"not an ISO 9660 image",
	} {
		if !strings.Contains(imgRep.Failure, want) {
			t.Errorf("failure = %q, want it to contain %q", imgRep.Failure, want)
		}
	}
	// NOT N MISSING PAYLOADS: no per-member classification at all.
	if imgRep.Matched+imgRep.Mismatched+imgRep.Missing+imgRep.Unexpected+imgRep.Unreadable != 0 {
		t.Errorf("counts = %+v, want an unreadable image to report no payload verdicts", imgRep)
	}
	if len(imgRep.Problems) != 0 {
		t.Errorf("problems = %v, want none (nobody read the payloads, so nobody classified them)", imgRep.Problems)
	}
	// THE SURFACED XIRRISO DIAGNOSTIC (the flipped half of the old pin):
	// xorriso's own words ride the failure detail now.
	for _, want := range []string{"No ISO 9660 image at LBA 0", "Creating blank image"} {
		if !strings.Contains(imgRep.Failure, want) {
			t.Errorf("failure = %q, want xorriso's %q in the surfaced output tail", imgRep.Failure, want)
		}
	}
	if combined := stdout + stderr; strings.Contains(combined, "missing from extraction") {
		t.Errorf("an unreadable image must never be reported as missing payloads:\n%s", combined)
	}
	if imgRep.Stamped || rep.Verified != 0 || imgRep.Source != "iso" {
		t.Errorf("image report = %+v (run %+v), want an unstamped iso failure", imgRep, rep)
	}
	if rep.Status != "partial" {
		t.Errorf("run status = %q, want 'partial'", rep.Status)
	}

	// The build's provenance row is untouched and no scratch leaked.
	state, verifiedAt, _, _ := imageStateRow(t, cat, setID, 1)
	if state != "built" || verifiedAt.Valid {
		t.Errorf("images row = (state %q, verified_at %v), want ('built', NULL): a failed read writes nothing", state, verifiedAt)
	}
	entries, err := os.ReadDir(work)
	if err != nil {
		t.Fatalf("reading --work-dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("--work-dir holds %d entries after the failed extraction (leaked scratch)", len(entries))
	}
}

// TestVerifyMediaGarbageISOReportsUnreadableImage is the Finding-2
// e2e on a GARBAGE image path: an .iso holding random bytes (real
// xorriso 1.5.6 is on PATH, so the gated zero-entry path runs - it
// warns "No ISO 9660 image at LBA 0", exits 0, extracts nothing).
// The report must name the IMAGE unreadable with the
// zero-entries-vs-expected detail - NOT "every payload is missing" -
// keep the state untouched, exit 10 (the per-image failure class), and
// leak no scratch.
func TestVerifyMediaGarbageISOReportsUnreadableImage(t *testing.T) {
	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skip("xorriso not on PATH; skipping the gated garbage-image edge (DESIGN_SPEC 12.6)")
	}
	const setID = "S-VGARBAGE"
	cat, _ := verifyCopyFixture(t, setID)
	expected := contentListRows(t, cat, setID, 1)

	// Point the image at a GARBAGE .iso: random bytes, no ISO 9660
	// structure at all (the store's final_path is the only reference
	// that matters - source mode is resolved by STAT).
	blob := make([]byte, 4096)
	if _, err := rand.Read(blob); err != nil {
		t.Fatalf("generating garbage bytes: %v", err)
	}
	iso := filepath.Join(t.TempDir(), setID+"-001.iso")
	if err := os.WriteFile(iso, blob, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := catRW(t, cat).Exec(
		`UPDATE images SET final_path = ? WHERE set_id = ? AND image_id = 1`, iso, setID); err != nil {
		t.Fatalf("pointing the image at the garbage iso: %v", err)
	}

	work := t.TempDir()
	stdout, stderr, code := runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID, "--json", "--work-dir", work)
	if code != report.ExitItemFailures {
		t.Fatalf("garbage-iso verify exit = %d, want 10 (per-image failure, anomaly class)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if strings.Contains(stderr, "panic") {
		t.Errorf("an unreadable image must not panic:\n%s", stderr)
	}
	var rep verifyMediaReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("report does not parse: %v\n%s", err, stdout)
	}
	img := verifyMediaImageOf(t, rep, 1)
	if img.Status != "failed" || img.Failure == "" {
		t.Fatalf("image report = %+v, want a per-image READ failure", img)
	}
	if img.Source != verify.SourceISO {
		t.Errorf("source = %q, want %q (final_path names a regular file)", img.Source, verify.SourceISO)
	}
	// THE HONEST DIAGNOSIS: unreadable, zero entries against N expected.
	for _, want := range []string{
		"no extractable entries",
		fmt.Sprintf("against %d expected", expected),
		"unreadable",
		"not an ISO 9660 image",
	} {
		if !strings.Contains(img.Failure, want) {
			t.Errorf("failure = %q, want it to contain %q", img.Failure, want)
		}
	}
	// The surfaced xorriso tail (the diagnosis's evidence).
	if !strings.Contains(img.Failure, "No ISO 9660 image at LBA 0") {
		t.Errorf("failure = %q, want xorriso's parse diagnostic in the surfaced output tail", img.Failure)
	}
	// NOT N MISSING ENTRIES: no payload classification at all.
	if rep.Missing != 0 || img.Missing != 0 {
		t.Errorf("counts = (run %+v, image %+v), want zero missing - the payloads were never read", rep, img)
	}
	if img.Matched+img.Mismatched+img.Unexpected+img.Unreadable != 0 {
		t.Errorf("counts = %+v, want an unreadable image to classify nothing", img)
	}
	if len(img.Problems) != 0 {
		t.Errorf("problems = %v, want none (nobody read the payloads)", img.Problems)
	}
	if combined := stdout + stderr; strings.Contains(combined, "missing from extraction") {
		t.Errorf("an unreadable image must never be reported as missing payloads:\n%s", combined)
	}
	if img.Stamped || rep.Verified != 0 || rep.Status != "partial" {
		t.Errorf("report = %+v, want an unstamped partial run", rep)
	}

	// STATE UNCHANGED (a failed read writes nothing) and no scratch left.
	state, verifiedAt, _, _ := imageStateRow(t, cat, setID, 1)
	if state != "built" || verifiedAt.Valid {
		t.Errorf("images row = (state %q, verified_at %v), want ('built', NULL): a failed read writes nothing", state, verifiedAt)
	}
	entries, err := os.ReadDir(work)
	if err != nil {
		t.Fatalf("reading --work-dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("--work-dir holds %d entries after the failed extraction (leaked scratch)", len(entries))
	}
}

// --- the stamp-failure paths: THE 2.1 CARVE-OUT AS SHIPPED (2026-10-21) ----------

// TestVerifyMediaStampBusyTimeoutIsNonFatal is the FLIPPED literal
// carve-out probe (it shipped as ...ShipsAsPerImageFailure): a
// competing writer holds the WAL write lock across the whole run, so
// the stamp's UPDATE exhausts the 5s busy_timeout (the exact SQLITE_BUSY
// condition DESIGN_SPEC 2.1 (2026-10-21) declares non-fatal). Pins the
// RULING shape: never exit 5, never exit 10 - exit 0 with the content
// verdict intact, status "verified", stamped false, and a warning
// naming the busy stamp; the row stays stampable, and a LATER verify
// re-stamps. Expect a ~5s busy wait in this test.
func TestVerifyMediaStampBusyTimeoutIsNonFatal(t *testing.T) {
	const setID = "S-VSTAMPBUSY"
	cat, _ := verifyCopyFixture(t, setID)
	expected := contentListRows(t, cat, setID, 1)

	// A competing writer holds the WAL write lock for the whole run.
	blocker := catRW(t, cat)
	tx, err := blocker.Begin()
	if err != nil {
		t.Fatalf("beginning the blocking write txn: %v", err)
	}
	if _, err := tx.ExecContext(context.Background(),
		`UPDATE images SET bytes_written = COALESCE(bytes_written, 0) WHERE set_id = ? AND image_id = 1`, setID); err != nil {
		t.Fatalf("acquiring the WAL write lock: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	stdout, stderr, code := runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID, "--json")
	t.Logf("stamp busy-timeout: exit=%d\nstdout:%s\nstderr:%s", code, stdout, stderr)

	if code == report.ExitCatalogBusy {
		t.Errorf("exit = 5: a stamp that exhausts busy_timeout must never surface the busy class")
	}
	if code != report.ExitOK {
		t.Errorf("exit = %d, want 0 (the carve-out: the busy stamp is a WARNING, not an anomaly)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}

	var rep verifyMediaReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("report does not parse: %v\n%s", err, stdout)
	}
	if rep.Status != "ok" {
		t.Errorf("run status = %q, want 'ok' (a warning is not an anomaly)", rep.Status)
	}
	img := verifyMediaImageOf(t, rep, 1)
	if img.Status != "verified" || img.Failure != "" {
		t.Errorf("image report = %+v, want status 'verified' with no per-image failure", img)
	}
	// THE CONTENT VERDICT SHIPS under the competing writer.
	if img.Matched != expected || img.Mismatched != 0 || img.Missing != 0 ||
		img.Unexpected != 0 || img.Unreadable != 0 {
		t.Errorf("counts = %+v, want all %d listed rows matched", img, expected)
	}
	if img.Stamped || rep.Verified != 0 {
		t.Errorf("stamped = (%v, verified %d), want (false, 0): the busy stamp is not claimed", img.Stamped, rep.Verified)
	}
	// THE WARNING names the busy stamp cause.
	if len(img.Warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly the busy-timeout stamp warning", img.Warnings)
	}
	if !strings.Contains(img.Warnings[0], "stamp failed") ||
		!(strings.Contains(img.Warnings[0], "database is locked") || strings.Contains(img.Warnings[0], "SQLITE_BUSY")) {
		t.Errorf("warning = %q, want the stamp failure carrying the SQLITE_BUSY cause", img.Warnings[0])
	}
	if !strings.Contains(stderr, "warning: ") {
		t.Errorf("stderr lacks the progress.Warn line:\n%s", stderr)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("releasing the blocking write txn: %v", err)
	}
	// The content was clean and the stamp never landed: zero writes
	// beyond the copy completion's own state ('built', T6.5).
	state, verifiedAt, _, _ := imageStateRow(t, cat, setID, 1)
	if state != "built" || verifiedAt.Valid {
		t.Errorf("images row = (state %q, verified_at %v), want ('built', NULL): the busy stamp wrote nothing", state, verifiedAt)
	}

	// A LATER verify re-stamps cleanly (the ruling's recovery clause).
	stdout, stderr, code = runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID, "--json")
	if code != report.ExitOK {
		t.Fatalf("the follow-up verify exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	rep = verifyMediaReport{} // decode into a FRESH value: an omitted omitempty key must not inherit the previous run's field values
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("follow-up report does not parse: %v\n%s", err, stdout)
	}
	img = verifyMediaImageOf(t, rep, 1)
	if !img.Stamped || len(img.Warnings) != 0 || rep.Verified != 1 {
		t.Errorf("follow-up image report = %+v, want the re-stamp with no warning", img)
	}
	state, verifiedAt, _, _ = imageStateRow(t, cat, setID, 1)
	if state != "verified" || !verifiedAt.Valid || verifiedAt.Int64 <= 0 {
		t.Errorf("images row = (state %q, verified_at %v), want the re-stamp after the blocking writer left", state, verifiedAt)
	}
}

// TestVerifyMediaCleanISOStampTriggerFailureKeepsContentVerdict is the
// Finding-1 e2e on the REAL ISO path: a fully-built, CLEAN image whose
// stamp write is refused at the DATABASE (the BEFORE UPDATE trigger
// technique - the store seam has no injection point of its own). The
// content verdict must SHIP (every listed row matched, status
// "verified", no per-image failure), the warning naming the stamp
// failure must reach the report and stderr, the run must exit 0 (no
// exit 10 for a warning), the row must keep ('built', NULL), and a
// LATER verify - obstruction removed - must stamp it (the ruling's
// re-stamp clause). Gated on the real xorriso (12.6): the fixture is a
// built ISO.
func TestVerifyMediaCleanISOStampTriggerFailureKeepsContentVerdict(t *testing.T) {
	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skip("xorriso not on PATH; skipping the gated ISO stamp-failure e2e (DESIGN_SPEC 12.6)")
	}
	const setID = "S-VISOFAIL"
	root, _ := verifyISOTree(t)
	cat := createCatalog(t)
	scanBuildFixture(t, root, cat, setID)
	planSynthetic(t, cat, setID, 25<<20)
	dest := filepath.Join(t.TempDir(), "dest")
	if stdout, stderr, code := runBuildChild(t,
		"build", "--catalog", cat, "--set-id", setID, "--dest", dest); code != report.ExitOK {
		t.Fatalf("build fixture exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	expected := contentListRows(t, cat, setID, 1)

	rw := catRW(t, cat)
	const trigger = `CREATE TRIGGER test_block_verify_stamp BEFORE UPDATE OF state ON images
	FOR EACH ROW WHEN NEW.state = 'verified'
	BEGIN SELECT RAISE(ABORT, 'verify stamp blocked (test seam)'); END`
	if _, err := rw.Exec(trigger); err != nil {
		t.Fatalf("creating the stamp-blocking trigger: %v", err)
	}
	t.Cleanup(func() { _, _ = rw.Exec(`DROP TRIGGER IF EXISTS test_block_verify_stamp`) })

	// 1) CLEAN ISO, stamp refused: the full content verdict ships.
	stdout, stderr, code := runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID, "--json")
	if code == report.ExitCatalogBusy {
		t.Errorf("exit = 5: a stamp failure must never surface the busy class (2.1 carve-out)")
	}
	if code != report.ExitOK {
		t.Fatalf("clean-but-unstamped verify exit = %d, want 0 (warnings never create exit 10)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	var rep verifyMediaReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("report does not parse: %v\n%s", err, stdout)
	}
	if rep.Status != "ok" {
		t.Errorf("run status = %q, want 'ok'", rep.Status)
	}
	img := verifyMediaImageOf(t, rep, 1)
	if img.Source != verify.SourceISO || img.Status != "verified" || img.Failure != "" {
		t.Errorf("image report = %+v, want a verified iso image with no failure", img)
	}
	if img.Matched != expected || img.Mismatched != 0 || img.Missing != 0 ||
		img.Unexpected != 0 || img.Unreadable != 0 {
		t.Errorf("counts = %+v, want all %d listed rows matched", img, expected)
	}
	if img.Stamped || rep.Verified != 0 {
		t.Errorf("stamped = (%v, verified %d), want (false, 0): no stamp was written", img.Stamped, rep.Verified)
	}
	if len(img.Warnings) != 1 ||
		!strings.Contains(img.Warnings[0], "matched its content list fully") ||
		!strings.Contains(img.Warnings[0], "verify stamp blocked") {
		t.Errorf("warnings = %v, want the stamp-failure warning naming the full match and its cause", img.Warnings)
	}
	if !strings.Contains(stderr, "warning: ") || !strings.Contains(stderr, "verify stamp blocked") {
		t.Errorf("stderr lacks the progress.Warn line naming the stamp failure:\n%s", stderr)
	}

	// The build's provenance row is untouched (zero writes).
	state, verifiedAt, _, _ := imageStateRow(t, cat, setID, 1)
	if state != "built" || verifiedAt.Valid {
		t.Errorf("images row = (state %q, verified_at %v), want ('built', NULL): a failed stamp writes nothing", state, verifiedAt)
	}

	// 2) A LATER VERIFY RE-STAMPS (the ruling's recovery clause).
	if _, err := rw.Exec(`DROP TRIGGER IF EXISTS test_block_verify_stamp`); err != nil {
		t.Fatalf("dropping the stamp-blocking trigger: %v", err)
	}
	stdout, stderr, code = runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID, "--json")
	if code != report.ExitOK {
		t.Fatalf("the follow-up verify exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	rep = verifyMediaReport{} // decode into a FRESH value: an omitted omitempty key must not inherit the previous run's field values
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("follow-up report does not parse: %v\n%s", err, stdout)
	}
	img = verifyMediaImageOf(t, rep, 1)
	if img.Status != "verified" || !img.Stamped || len(img.Warnings) != 0 || rep.Verified != 1 {
		t.Errorf("follow-up image report = %+v (run %+v), want the stamp to land on the later verify", img, rep)
	}
	state, verifiedAt, _, _ = imageStateRow(t, cat, setID, 1)
	if state != "verified" || !verifiedAt.Valid || verifiedAt.Int64 <= 0 {
		t.Errorf("images row = (state %q, verified_at %v), want the re-stamp", state, verifiedAt)
	}
}

// TestVerifyMediaCleanISOStampBusyTimeoutKeepsContentVerdict is the
// Finding-1 CONCURRENCY e2e: the same clean ISO verified while a
// competing writer holds the WAL write lock, so the stamp's UPDATE
// exhausts the 5s busy_timeout (SQLITE_BUSY - the literal condition
// DESIGN_SPEC 2.1 (2026-10-21) declares non-fatal). Same shape as the
// trigger case: exit 0 (never 5, never 10), full content verdict, status
// "verified", stamped false, a warning naming the busy stamp, zero
// writes to the row, and a LATER verify re-stamping once the writer
// leaves. Expect a ~5s busy wait.
func TestVerifyMediaCleanISOStampBusyTimeoutKeepsContentVerdict(t *testing.T) {
	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skip("xorriso not on PATH; skipping the gated ISO stamp-busy e2e (DESIGN_SPEC 12.6)")
	}
	const setID = "S-VISOBUSY"
	root, _ := verifyISOTree(t)
	cat := createCatalog(t)
	scanBuildFixture(t, root, cat, setID)
	planSynthetic(t, cat, setID, 25<<20)
	dest := filepath.Join(t.TempDir(), "dest")
	if stdout, stderr, code := runBuildChild(t,
		"build", "--catalog", cat, "--set-id", setID, "--dest", dest); code != report.ExitOK {
		t.Fatalf("build fixture exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	expected := contentListRows(t, cat, setID, 1)

	// A competing writer holds the WAL write lock for the whole run.
	blocker := catRW(t, cat)
	tx, err := blocker.Begin()
	if err != nil {
		t.Fatalf("beginning the blocking write txn: %v", err)
	}
	if _, err := tx.ExecContext(context.Background(),
		`UPDATE images SET bytes_written = COALESCE(bytes_written, 0) WHERE set_id = ? AND image_id = 1`, setID); err != nil {
		t.Fatalf("acquiring the WAL write lock: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	stdout, stderr, code := runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID, "--json")
	t.Logf("ISO stamp busy-timeout: exit=%d\nstdout:%s\nstderr:%s", code, stdout, stderr)
	if code == report.ExitCatalogBusy {
		t.Errorf("exit = 5: a stamp that exhausts busy_timeout must never surface the busy class")
	}
	if code != report.ExitOK {
		t.Errorf("exit = %d, want 0 (the carve-out: the busy stamp is a WARNING)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}

	var rep verifyMediaReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("report does not parse: %v\n%s", err, stdout)
	}
	if rep.Status != "ok" {
		t.Errorf("run status = %q, want 'ok'", rep.Status)
	}
	img := verifyMediaImageOf(t, rep, 1)
	if img.Source != verify.SourceISO || img.Status != "verified" || img.Failure != "" {
		t.Errorf("image report = %+v, want a verified iso image with no failure", img)
	}
	if img.Matched != expected || img.Mismatched != 0 || img.Missing != 0 ||
		img.Unexpected != 0 || img.Unreadable != 0 {
		t.Errorf("counts = %+v, want all %d listed rows matched", img, expected)
	}
	if img.Stamped || rep.Verified != 0 {
		t.Errorf("stamped = (%v, verified %d), want (false, 0): the busy stamp is not claimed", img.Stamped, rep.Verified)
	}
	if len(img.Warnings) != 1 || !strings.Contains(img.Warnings[0], "stamp failed") ||
		!(strings.Contains(img.Warnings[0], "database is locked") || strings.Contains(img.Warnings[0], "SQLITE_BUSY")) {
		t.Errorf("warnings = %v, want the busy-timeout stamp warning carrying its cause", img.Warnings)
	}
	if !strings.Contains(stderr, "warning: ") {
		t.Errorf("stderr lacks the progress.Warn line:\n%s", stderr)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("releasing the blocking write txn: %v", err)
	}
	// Zero writes while the stamp was refused.
	state, verifiedAt, _, _ := imageStateRow(t, cat, setID, 1)
	if state != "built" || verifiedAt.Valid {
		t.Errorf("images row = (state %q, verified_at %v), want ('built', NULL): the busy stamp wrote nothing", state, verifiedAt)
	}

	// A LATER verify re-stamps (the ruling's recovery clause).
	stdout, stderr, code = runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID, "--json")
	if code != report.ExitOK {
		t.Fatalf("the follow-up verify exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	rep = verifyMediaReport{} // decode into a FRESH value: an omitted omitempty key must not inherit the previous run's field values
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("follow-up report does not parse: %v\n%s", err, stdout)
	}
	img = verifyMediaImageOf(t, rep, 1)
	if !img.Stamped || len(img.Warnings) != 0 || rep.Verified != 1 {
		t.Errorf("follow-up image report = %+v, want the stamp to land after the writer left", img)
	}
	state, verifiedAt, _, _ = imageStateRow(t, cat, setID, 1)
	if state != "verified" || !verifiedAt.Valid || verifiedAt.Int64 <= 0 {
		t.Errorf("images row = (state %q, verified_at %v), want the re-stamp", state, verifiedAt)
	}
}

// TestVerifyMediaBrokenRootDirFailsExtractionHonestly is the SECOND
// parse-corruption shape: destroying the ROOT DIRECTORY RECORD of every
// volume descriptor (xorriso 1.5.6 answers exit 5, "Read error ...
// Cannot read ISO image tree") makes the EXTRACTION ITSELF fail - the
// shape the design brief expects. Pinned: per-image failure naming the
// extraction and the xorriso cause, exit 10 (anomaly class), no stamp,
// the build's row untouched, and the scratch removed from --work-dir.
func TestVerifyMediaBrokenRootDirFailsExtractionHonestly(t *testing.T) {
	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skip("xorriso not on PATH; skipping the gated extraction-failure edge (DESIGN_SPEC 12.6)")
	}
	const setID = "S-VROOTDIR"
	root, _ := verifyISOTree(t)
	cat := createCatalog(t)
	scanBuildFixture(t, root, cat, setID)
	planSynthetic(t, cat, setID, 25<<20)
	dest := filepath.Join(t.TempDir(), "dest")
	if stdout, stderr, code := runBuildChild(t,
		"build", "--catalog", cat, "--set-id", setID, "--dest", dest); code != report.ExitOK {
		t.Fatalf("build fixture exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	iso := filepath.Join(dest, fmt.Sprintf("%s-001.iso", setID))

	// Destroy the ROOT DIRECTORY RECORD of every volume descriptor
	// (PVD/Joliet SVD/terminator live in the first 64 KiB): xorriso
	// then fails the tree load itself - "Read error ... Cannot read ISO
	// image tree", exit 5 - which is the extraction-failure shape.
	img, err := os.ReadFile(iso)
	if err != nil {
		t.Fatal(err)
	}
	corrupted := 0
	for i := 0; i+5 <= len(img) && i < 64<<10; {
		j := bytes.Index(img[i:], []byte("CD001"))
		if j < 0 {
			break
		}
		pvd := i + j - 1 // the descriptor type byte precedes the magic
		if pvd >= 0 && pvd+190 <= len(img) {
			for k := pvd + 156; k < pvd+190; k++ {
				img[k] = 0xff
			}
			corrupted++
		}
		i += j + 5
	}
	if corrupted == 0 {
		t.Fatalf("%s: no primary volume descriptor found - the edge cannot be provoked", iso)
	}
	if err := os.WriteFile(iso, img, 0o644); err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	stdout, stderr, code := runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID, "--json", "--work-dir", work)
	if code != report.ExitItemFailures {
		t.Fatalf("exit = %d, want 10 (per-image failure, anomaly class)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if strings.Contains(stderr, "panic") {
		t.Errorf("a broken image must not panic:\n%s", stderr)
	}
	var rep verifyMediaReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("report does not parse: %v\n%s", err, stdout)
	}
	imgRep := verifyMediaImageOf(t, rep, 1)
	if imgRep.Status != "failed" || imgRep.Failure == "" {
		t.Fatalf("image report = %+v, want a per-image failure", imgRep)
	}
	if !strings.Contains(imgRep.Failure, "extracting image") ||
		!strings.Contains(imgRep.Failure, setID) ||
		!strings.Contains(imgRep.Failure, "xorriso build failed") {
		t.Errorf("failure = %q, want the extraction failure carrying xorriso's own cause", imgRep.Failure)
	}
	if imgRep.Stamped || rep.Verified != 0 || rep.Status != "partial" {
		t.Errorf("report = %+v, want an unstamped partial run", rep)
	}
	if imgRep.Matched+imgRep.Mismatched+imgRep.Missing+imgRep.Unexpected+imgRep.Unreadable != 0 {
		t.Errorf("counts = %+v, want a failed read to report no payload verdicts", imgRep)
	}

	state, verifiedAt, _, _ := imageStateRow(t, cat, setID, 1)
	if state != "built" || verifiedAt.Valid {
		t.Errorf("images row = (state %q, verified_at %v), want ('built', NULL): a failed read writes nothing", state, verifiedAt)
	}
	entries, err := os.ReadDir(work)
	if err != nil {
		t.Fatalf("reading --work-dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("--work-dir holds %d entries after the failed extraction (leaked scratch)", len(entries))
	}
}

// --- the remaining exit classes of the documented matrix --------------------------

// TestVerifyMediaCrashedBuildWindowStampsISOFromBuilding is the LITERAL
// 6.3 crash-window reconciliation the carve-out names: a build that died
// AFTER the .iso and its content rows were written but BEFORE it could
// record state 'built' - the row still says 'building', built_at is
// still NULL, and the image is a valid ISO. The next full verify-media
// must stamp it to 'verified' (verified_at only; verify-media never
// fabricates the build's own built_at, and the mode column keeps the
// value the build recorded).
func TestVerifyMediaCrashedBuildWindowStampsISOFromBuilding(t *testing.T) {
	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skip("xorriso not on PATH; skipping the gated crash-window ISO acceptance (DESIGN_SPEC 12.6)")
	}
	const setID = "S-VCRASH"
	root, _ := verifyISOTree(t)
	cat := createCatalog(t)
	scanBuildFixture(t, root, cat, setID)
	planSynthetic(t, cat, setID, 25<<20)
	dest := filepath.Join(t.TempDir(), "dest")
	if stdout, stderr, code := runBuildChild(t,
		"build", "--catalog", cat, "--set-id", setID, "--dest", dest); code != report.ExitOK {
		t.Fatalf("build fixture exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	// Re-create the crash window: the ISO and the content rows stay,
	// the build's completion records do not exist yet.
	rw := catRW(t, cat)
	if _, err := rw.Exec(
		`UPDATE images SET state = 'building', built_at = NULL, verified_at = NULL
		 WHERE set_id = ? AND image_id = 1`, setID); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := catOpen(t, cat).QueryRow(
		`SELECT count(*) FROM image_contents WHERE set_id = ? AND image_id = 1`, setID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows == 0 {
		t.Fatal("the build left no content list - the crash window cannot be provoked")
	}

	stdout, stderr, code := runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID, "--json")
	if code != report.ExitOK {
		t.Fatalf("crash-window verify exit = %d, want 0 (a full match heals the stale state)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	var rep verifyMediaReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("report does not parse: %v\n%s", err, stdout)
	}
	img := verifyMediaImageOf(t, rep, 1)
	if img.Source != verify.SourceISO || img.Status != "verified" || !img.Stamped {
		t.Fatalf("image report = %+v, want a stamped iso image", img)
	}

	state, verifiedAt, builtAt, mode := imageStateRow(t, cat, setID, 1)
	if state != "verified" || !verifiedAt.Valid || verifiedAt.Int64 <= 0 {
		t.Errorf("images row = (state %q, verified_at %v), want the crash-window heal to 'verified'", state, verifiedAt)
	}
	if builtAt.Valid {
		t.Errorf("built_at = %d, want it still unset: verify-media stamps verified_at only", builtAt.Int64)
	}
	if !mode.Valid || mode.String != "iso" {
		t.Errorf("mode = %v, want the build's 'iso' untouched", mode)
	}
}
