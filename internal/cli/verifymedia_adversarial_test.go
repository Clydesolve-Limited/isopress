package cli

// T5.1 adversarial additions (TEST ONLY): the verify-media edges the
// acceptance matrix in verifymedia_test.go did not pin yet.
//
//   - the stamping connection never takes the advisory flock (the 2.1
//     carve-out of 2026-10-21, asserted against a HELD exclusive lock);
//   - the stamp applies from ANY image state (the 6.3 crash-window
//     reconciliation) and writes nothing else (built_at, mode, runs);
//   - a partial re-verify of previously STAMPED media leaves state and
//     verified_at byte-identical (unchanged, not re-stamped, not
//     cleared);
//   - a dangling final_path (copy tree deleted) and an empty one are
//     honest per-image failures with ZERO catalog writes;
//   - two verify-media runs stamping the SAME image concurrently
//     (WAL + busy_timeout serialization);
//   - MANIFEST.json / catalog-export.sqlite corruption inside the copy
//     tree (they are content-list rows);
//   - chunk corruption isolated to ONE image of a two-image chunked set,
//     asserted end to end through the COMMAND;
//   - an ISO whose volume descriptors are destroyed (xorriso cannot even
//     PARSE it) and a garbage ISO path: an extraction that yields ZERO
//     entries against a non-empty content list is a per-image READ
//     failure naming the image UNREADABLE (exit 10, honest diagnosis) -
//     never N missing payloads; the second corruption shape (destroyed
//     ROOT DIRECTORY RECORD) fails the extraction itself: per-image
//     extraction failure, exit 10, honest;
//   - an idempotent re-copy keeps the recorded final_path valid;
//   - the stamp-write failure paths: the 2.1 carve-out (2026-10-21) -
//     a stamp that fails (a trigger-aborted write OR a busy_timeout
//     exhausted under a competing writer) keeps the content verdict,
//     ships status "verified" with stamped false and a WARNING line,
//     exits 0, and a LATER verify re-stamps - including on the REAL ISO
//     path (the two gated stamp e2e tests below).

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"isopress/internal/catalog"
	"isopress/internal/media"
	"isopress/internal/report"
)

// verifyMediaImageOf returns the report entry of one image (the report
// order is the selection order; tests look images UP by id so an
// ordering change never silently re-points an assertion).
func verifyMediaImageOf(t *testing.T, rep verifyMediaReport, imageID int64) verifyMediaImageReport {
	t.Helper()
	for _, img := range rep.Images {
		if img.Image == imageID {
			return img
		}
	}
	t.Fatalf("image %d missing from the report: %+v", imageID, rep.Images)
	return verifyMediaImageReport{}
}

// imageStateRow reads the images row's state/verified_at/built_at/mode.
func imageStateRow(t *testing.T, cat, setID string, imageID int64) (state string, verifiedAt, builtAt sql.NullInt64, mode sql.NullString) {
	t.Helper()
	db := catOpen(t, cat)
	err := db.QueryRow(`SELECT state, verified_at, built_at, mode FROM images WHERE set_id = ? AND image_id = ?`,
		setID, imageID).Scan(&state, &verifiedAt, &builtAt, &mode)
	if err != nil {
		t.Fatalf("reading images row (set %s image %d): %v", setID, imageID, err)
	}
	return state, verifiedAt, builtAt, mode
}

// --- the stamping connection (2.1 carve-out, 2026-10-21) -----------------------

// TestVerifyMediaStampConnectionTakesNoFlock pins the carving property
// at its strongest: while THIS process holds the exclusive mutating
// flock (what a running copy/build holds for its whole run), the
// stamping connection still writes its one full-match stamp. A control
// open with the default locker proves the flock really conflicts here,
// so the stamp's success can only come from the NO-OP locker.
func TestVerifyMediaStampConnectionTakesNoFlock(t *testing.T) {
	const setID = "S-VNOFLOCK"
	cat, _ := verifyCopyFixture(t, setID)

	// Hold the mutating flock the way a copy/build run does.
	holder, err := catalog.Open(cat, catalog.OpenReadWrite)
	if err != nil {
		t.Fatalf("taking the mutating flock: %v", err)
	}
	defer func() { _ = holder.Close() }()

	// CONTROL: a second MUTATING open with the DEFAULT locker must be
	// refused with the busy class - proving the conflict is real in
	// this process (two independent open file descriptions).
	second, err := catalog.Open(cat, catalog.OpenReadWrite)
	if err == nil {
		_ = second.Close()
		t.Fatal("the control mutating open succeeded while the flock was held; the control is vacuous")
	}
	var busy *catalog.CatalogBusy
	if !errors.As(err, &busy) {
		t.Fatalf("control open err = %v (%T), want *catalog.CatalogBusy", err, err)
	}

	// THE STAMP PATH: the lazily-opened RW handle with the no-op
	// Locker must succeed while the flock is held elsewhere.
	ro, err := catalog.Open(cat, catalog.OpenReadOnly)
	if err != nil {
		t.Fatalf("read-only open: %v", err)
	}
	store := &verifyStore{ro: ro, catalogPath: cat}
	defer func() { _ = store.Close() }()

	stampAt := time.Now().UnixNano()
	// THE LAZY, SEPARATE STAMPING CONNECTION: no RW handle exists until
	// the first full match needs one; the read handle is never it.
	if store.rw != nil {
		t.Fatal("the stamping connection must be opened lazily, not up front")
	}
	if err := store.StampVerified(context.Background(), setID, 1, stampAt); err != nil {
		t.Fatalf("the stamp must not contend with the advisory flock (2.1 carve-out 2026-10-21): %v", err)
	}
	if store.rw == nil {
		t.Fatal("the stamp did not open the separate RW connection")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("closing the stamping connection: %v", err)
	}
	if store.rw != nil {
		t.Error("Close must release the stamping connection")
	}
	if err := ro.Close(); err != nil {
		t.Fatalf("closing the read-only handle: %v", err)
	}

	state, verifiedAt, _, _ := imageStateRow(t, cat, setID, 1)
	if state != "verified" || verifiedAt.Int64 != stampAt {
		t.Errorf("images row = (state %q, verified_at %d), want ('verified', %d)", state, verifiedAt.Int64, stampAt)
	}
}

// --- the stamp's state policy ---------------------------------------------------

// TestVerifyMediaStampAppliesFromAnyState pins the 6.3 crash-window
// reconciliation: whatever state the row carries ('building' = a build
// that died after writing the bytes, 'failed', or 'planned' = the
// pre-T6.5 copy shape, which T6.5's completion stamp no longer writes
// but EXISTING rows still carry), a FULL match stamps it - while
// writing NOTHING else (built_at preserved, mode untouched - the copy
// path records it at completion, T6.5, and verify-media never does -
// and no runs/run_events row, per the 2.1 read-only policy).
func TestVerifyMediaStampAppliesFromAnyState(t *testing.T) {
	const setID = "S-VSTATE"
	cat, _ := verifyCopyFixture(t, setID)
	db := catRW(t, cat)

	// The read-only policy baseline: verify-media must not add a runs row.
	var runsBefore int
	if err := catOpen(t, cat).QueryRow(`SELECT count(*) FROM runs`).Scan(&runsBefore); err != nil {
		t.Fatalf("counting runs: %v", err)
	}

	cases := []struct {
		name  string
		state string
	}{
		{"crash-window 'building' (6.3 reconciliation)", "building"},
		{"failed", "failed"},
		{"planned (the pre-T6.5 copy row)", "planned"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.Exec(
				`UPDATE images SET state = ?, verified_at = NULL, built_at = 4242, mode = NULL WHERE set_id = ? AND image_id = 1`,
				tc.state, setID); err != nil {
				t.Fatalf("seeding state %q: %v", tc.state, err)
			}

			stdout, stderr, code := runVerifyMediaChild(t, nil,
				"verify-media", "--catalog", cat, "--set-id", setID, "--json")
			if code != report.ExitOK {
				t.Fatalf("verify-media exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
			}
			var rep verifyMediaReport
			if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
				t.Fatalf("report does not parse: %v\n%s", err, stdout)
			}
			if rep.Verified != 1 || !verifyMediaImageOf(t, rep, 1).Stamped {
				t.Fatalf("report = %+v, want the image stamped", rep)
			}

			state, verifiedAt, builtAt, mode := imageStateRow(t, cat, setID, 1)
			if state != "verified" || !verifiedAt.Valid || verifiedAt.Int64 <= 0 {
				t.Errorf("images row = (state %q, verified_at %v), want ('verified', stamped)", state, verifiedAt)
			}
			if !builtAt.Valid || builtAt.Int64 != 4242 {
				t.Errorf("built_at = %v, want it untouched (4242): the stamp writes verified_at/state only", builtAt)
			}
			if mode.Valid && mode.String != "" {
				t.Errorf("mode = %v, want it still unset: verify-media never records mode (the copy completion path does - T6.5 - this command does not)", mode)
			}
		})
	}

	// NO runs row / run_events for verify-media (2.1 read-only policy;
	// images.verified_at is the durable audit, the kind stays reserved).
	var runsAfter int
	if err := catOpen(t, cat).QueryRow(`SELECT count(*) FROM runs`).Scan(&runsAfter); err != nil {
		t.Fatalf("counting runs: %v", err)
	}
	if runsAfter != runsBefore {
		t.Errorf("runs = %d after the verifies, want %d: verify-media writes no run row", runsAfter, runsBefore)
	}
	var verifyRuns int
	if err := catOpen(t, cat).QueryRow(`SELECT count(*) FROM runs WHERE kind = 'verify_media'`).
		Scan(&verifyRuns); err != nil {
		t.Fatalf("counting verify_media runs: %v", err)
	}
	if verifyRuns != 0 {
		t.Errorf("runs of kind verify_media = %d, want 0 (reserved)", verifyRuns)
	}
}

// TestVerifyMediaPartialAfterCleanVerifyLeavesStateUnchanged pins the
// documented consequence of the state policy: media stamped 'verified'
// by an earlier clean run keeps state AND verified_at byte-identical
// after a later PARTIAL verify - the new report is the honest record,
// the state is not downgraded and not re-stamped.
func TestVerifyMediaPartialAfterCleanVerifyLeavesStateUnchanged(t *testing.T) {
	const setID = "S-VREVERIFY"
	cat, dest := verifyCopyFixture(t, setID)

	// 1) Clean verify: stamp lands.
	stdout, stderr, code := runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID, "--json")
	if code != report.ExitOK {
		t.Fatalf("clean verify exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	state, verifiedAt, _, _ := imageStateRow(t, cat, setID, 1)
	if state != "verified" || !verifiedAt.Valid || verifiedAt.Int64 <= 0 {
		t.Fatalf("after the clean verify: (state %q, verified_at %v), want the stamp", state, verifiedAt)
	}
	firstStamp := verifiedAt.Int64

	// 2) Corrupt one payload AFTER the stamp.
	small1 := filepath.Join(destImageDir(dest, setID, 1), "files", "small1.txt")
	orig, err := os.ReadFile(small1)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), orig...)
	corrupt[0] ^= 0xff
	if err := os.WriteFile(small1, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}

	// 3) Re-verify: the report flips, the row does NOT.
	stdout, stderr, code = runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID, "--json")
	if code != report.ExitItemFailures {
		t.Fatalf("partial re-verify exit = %d, want 10\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	var rep verifyMediaReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("report does not parse: %v\n%s", err, stdout)
	}
	img := verifyMediaImageOf(t, rep, 1)
	if img.Status != "mismatch" || img.Stamped || rep.Verified != 0 {
		t.Errorf("image report = %+v (run %+v), want an unstamped mismatch", img, rep)
	}

	state, verifiedAt, _, _ = imageStateRow(t, cat, setID, 1)
	if state != "verified" {
		t.Errorf("state = %q, want 'verified' unchanged (a partial verify never downgrades)", state)
	}
	if verifiedAt.Int64 != firstStamp {
		t.Errorf("verified_at = %d, want the ORIGINAL stamp %d unchanged (no re-stamp, no clear)", verifiedAt.Int64, firstStamp)
	}
}

// --- location resolution honesty -------------------------------------------------

// TestVerifyMediaDanglingFinalPathFailsHonestly: final_path points at a
// copy tree that no longer exists (deleted destination). The command
// reports the missing location per image, exits 10, writes NOTHING to
// the catalog, and does not panic.
func TestVerifyMediaDanglingFinalPathFailsHonestly(t *testing.T) {
	const setID = "S-VDANGLING"
	cat, dest := verifyCopyFixture(t, setID)
	imgDir := destImageDir(dest, setID, 1)
	if err := os.RemoveAll(imgDir); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID, "--json")
	if code != report.ExitItemFailures {
		t.Fatalf("exit = %d, want 10 (a dangling location is an anomaly)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if strings.Contains(stderr, "panic") {
		t.Errorf("a missing copy tree must never panic:\n%s", stderr)
	}
	var rep verifyMediaReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("report does not parse: %v\n%s", err, stdout)
	}
	img := verifyMediaImageOf(t, rep, 1)
	if img.Status != "failed" || img.Failure == "" {
		t.Fatalf("image report = %+v, want an honest per-image failure", img)
	}
	if !strings.Contains(img.Failure, imgDir) || !strings.Contains(img.Failure, "no such file") {
		t.Errorf("failure = %q, want the missing path named", img.Failure)
	}
	if img.Stamped || rep.Verified != 0 {
		t.Errorf("a dangling location must stamp nothing: %+v", rep)
	}

	// ZERO WRITES: the row is exactly what the copy left behind (T6.5:
	// the copy completion stamped state 'built'; verify-media's failure
	// path writes nothing on top of it).
	state, verifiedAt, _, _ := imageStateRow(t, cat, setID, 1)
	if state != "built" || verifiedAt.Valid {
		t.Errorf("images row = (state %q, verified_at %v), want ('built', NULL) - no writes on a failure path", state, verifiedAt)
	}
}

// TestVerifyMediaEmptyFinalPathIsExitTen pins the classification that
// ships for the location gap's visible half: an image row with no
// recorded location is a per-image failure (exit 10, run status
// "partial"), naming the empty column - never a guess, never exit 5.
func TestVerifyMediaEmptyFinalPathIsExitTen(t *testing.T) {
	const setID = "S-VNOPATH"
	cat, _ := verifyCopyFixture(t, setID)
	if _, err := catRW(t, cat).Exec(`UPDATE images SET final_path = NULL WHERE set_id = ? AND image_id = 1`, setID); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID, "--json")
	if code != report.ExitItemFailures {
		t.Fatalf("exit = %d, want 10 (pinning what ships: the empty location joins the anomaly class)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	var rep verifyMediaReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("report does not parse: %v\n%s", err, stdout)
	}
	if rep.Status != "partial" {
		t.Errorf("run status = %q, want 'partial'", rep.Status)
	}
	img := verifyMediaImageOf(t, rep, 1)
	if img.Status != "failed" || img.Source != "" || img.Stamped {
		t.Errorf("image report = %+v, want an unresolved failure with no stamp", img)
	}
	if !strings.Contains(img.Failure, "images.final_path is empty") {
		t.Errorf("failure = %q, want the empty-column explanation", img.Failure)
	}
	state, verifiedAt, _, _ := imageStateRow(t, cat, setID, 1)
	if state != "built" || verifiedAt.Valid {
		t.Errorf("images row = (state %q, verified_at %v), want ('built', NULL): zero writes", state, verifiedAt)
	}
}

// --- concurrency -----------------------------------------------------------------

// TestVerifyMediaConcurrentDoubleVerifyBothStamp: two verify-media runs
// over the SAME image at the SAME time, both wanting the stamp. WAL
// single-writer plus the 5s busy_timeout must serialize the two tiny
// UPDATEs: both runs report verified and stamped, and the catalog ends
// in 'verified'. (The never-exit-5 half of the ruling is structural:
// the stamping locker is the no-op one, so the busy class cannot be
// produced from this path at all.)
func TestVerifyMediaConcurrentDoubleVerifyBothStamp(t *testing.T) {
	const setID = "S-VDOUBLE"
	cat, _ := verifyCopyFixture(t, setID)

	type child struct {
		cmd *exec.Cmd
		out *bytes.Buffer
		err *bytes.Buffer
	}
	kids := make([]child, 2)
	for i := range kids {
		out, errB := &bytes.Buffer{}, &bytes.Buffer{}
		cmd := exec.Command(os.Args[0],
			"verify-media", "--catalog", cat, "--set-id", setID, "--json")
		cmd.Env = append(os.Environ(), cliHelperEnv+"=1")
		cmd.Stdout, cmd.Stderr = out, errB
		if err := cmd.Start(); err != nil {
			t.Fatalf("starting verify child %d: %v", i, err)
		}
		kids[i] = child{cmd: cmd, out: out, err: errB}
	}

	for i, k := range kids {
		waitErr := k.cmd.Wait()
		code := 0
		if waitErr != nil {
			exitErr, ok := waitErr.(*exec.ExitError)
			if !ok {
				t.Fatalf("verify child %d: %v (stderr: %s)", i, waitErr, k.err.String())
			}
			code = exitErr.ExitCode()
		}
		if code != report.ExitOK {
			t.Errorf("concurrent verify %d exit = %d, want 0 (WAL serializes the two stamps)\nstdout: %s\nstderr: %s",
				i, code, k.out.String(), k.err.String())
			continue
		}
		var rep verifyMediaReport
		if err := json.Unmarshal(k.out.Bytes(), &rep); err != nil {
			t.Fatalf("verify %d report does not parse: %v\n%s", i, err, k.out.String())
		}
		if len(rep.Images) != 1 {
			t.Fatalf("verify %d report = %+v, want the single fixture image", i, rep.Images)
		}
		img := rep.Images[0]
		if img.Image != 1 || img.Status != "verified" || !img.Stamped {
			t.Errorf("verify %d image report = %+v, want verified + stamped", i, img)
		}
	}

	state, verifiedAt, _, _ := imageStateRow(t, cat, setID, 1)
	if state != "verified" || !verifiedAt.Valid || verifiedAt.Int64 <= 0 {
		t.Errorf("images row = (state %q, verified_at %v), want the stamp after both runs", state, verifiedAt)
	}
}

// --- content-list self-members (MANIFEST.json / catalog-export.sqlite) -----------

// TestVerifyMediaCorruptedManifestAndExportReportedMismatched: both
// image-root self-members are content-list rows, so corruption of
// either is a HASH MISMATCH of that row (nothing to "extract" in copy
// mode - the walk just hashes bytes), the run exits 10, and the state
// stays untouched.
func TestVerifyMediaCorruptedManifestAndExportReportedMismatched(t *testing.T) {
	const setID = "S-VSELF"
	cat, dest := verifyCopyFixture(t, setID)
	imgDir := destImageDir(dest, setID, 1)

	manifest := filepath.Join(imgDir, media.ManifestFileName)
	export := filepath.Join(imgDir, media.ExportFileName)
	for _, p := range []string{manifest, export} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("the copy tree must carry %s (it is a content-list row): %v", filepath.Base(p), err)
		}
	}
	corruptFirstByte := func(path string) {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(data) == 0 {
			t.Fatalf("%s is empty", path)
		}
		data[0] ^= 0xff
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	corruptFirstByte(manifest)
	corruptFirstByte(export)

	stdout, stderr, code := runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID, "--json")
	if code != report.ExitItemFailures {
		t.Fatalf("exit = %d, want 10\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	var rep verifyMediaReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("report does not parse: %v\n%s", err, stdout)
	}
	if rep.Mismatched != 2 || rep.Verified != 0 {
		t.Fatalf("report = %+v, want exactly the two self-member mismatches", rep)
	}
	img := verifyMediaImageOf(t, rep, 1)
	if img.Status != "mismatch" || img.Stamped {
		t.Errorf("image report = %+v, want an unstamped mismatch", img)
	}
	for _, want := range []string{
		media.ManifestFileName + ": content hash",
		media.ExportFileName + ": content hash",
	} {
		if !containsLineWith(img.Problems, want) {
			t.Errorf("problems lack %q: %v", want, img.Problems)
		}
	}
	if len(img.Problems) != 2 {
		t.Errorf("problems = %v, want exactly the two findings", img.Problems)
	}

	state, verifiedAt, _, _ := imageStateRow(t, cat, setID, 1)
	if state != "built" || verifiedAt.Valid {
		t.Errorf("images row = (state %q, verified_at %v), want ('built', NULL): a partial verify writes nothing", state, verifiedAt)
	}
}

// --- chunked multi-image isolation through the COMMAND ---------------------------

// TestVerifyMediaChunkedTwoImageIsolationThroughCommand: the two-image
// chunked fixture (big.bin splits across both images) with ONE corrupted
// chunk on image 2 - the run reports the anomaly ONLY for image 2 while
// image 1 verifies clean and stamps, end to end through the real
// command, the real report, and the real catalog.
func TestVerifyMediaChunkedTwoImageIsolationThroughCommand(t *testing.T) {
	const setID = "S-VCHUNK2"
	root := t.TempDir()
	cat := createCatalog(t)
	dest := filepath.Join(t.TempDir(), "dest")

	big := pattern(1, copyBigChunkedLen) // 901,312 + 450,688 across images 1 and 2
	writeFileP(t, filepath.Join(root, "big.bin"), big, 0o644)
	writeFileP(t, filepath.Join(root, "small.txt"), pattern(2, 64), 0o644)
	if _, stderr, code := runScanChildForCopy(t, "--catalog", cat, "--set-id", setID, root); code != 0 {
		t.Fatalf("scan exit = %d (stderr: %s)", code, stderr)
	}
	planSynthetic(t, cat, setID, copySyntheticUsable) // chunk0 -> image 1; chunk1 + small.txt -> image 2
	if stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest); code != 0 {
		t.Fatalf("copy exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	// Corrupt IMAGE 2's chunk ONLY.
	chunk2 := filepath.Join(destImageDir(dest, setID, 2), "chunks", "big.bin", "00002")
	data, err := os.ReadFile(chunk2)
	if err != nil {
		t.Fatalf("reading image 2's chunk: %v", err)
	}
	data[0] ^= 0xff
	if err := os.WriteFile(chunk2, data, 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID, "--json")
	if code != report.ExitItemFailures {
		t.Fatalf("exit = %d, want 10\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	var rep verifyMediaReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("report does not parse: %v\n%s", err, stdout)
	}
	if len(rep.Images) != 2 {
		t.Fatalf("report = %+v, want both images", rep)
	}
	if rep.Verified != 1 || rep.Mismatched != 1 || rep.Missing != 0 || rep.Unexpected != 0 {
		t.Errorf("totals = %+v, want one verified image and one mismatch", rep)
	}

	img1 := verifyMediaImageOf(t, rep, 1)
	if img1.Status != "verified" || !img1.Stamped || img1.Mismatched != 0 {
		t.Errorf("image 1 report = %+v, want a clean stamp (its own chunk is untouched)", img1)
	}
	img2 := verifyMediaImageOf(t, rep, 2)
	if img2.Status != "mismatch" || img2.Stamped {
		t.Errorf("image 2 report = %+v, want an unstamped mismatch", img2)
	}
	if len(img2.Problems) != 1 ||
		!strings.Contains(img2.Problems[0], "chunks/big.bin/00002") ||
		!strings.Contains(img2.Problems[0], "content hash") {
		t.Errorf("image 2 problems = %v, want exactly its corrupted chunk by content hash", img2.Problems)
	}
	if img1.Missing != 0 || img1.Unexpected != 0 || img1.Unreadable != 0 {
		t.Errorf("image 1 anomalies = (missing %d, unexpected %d, unreadable %d), want none",
			img1.Missing, img1.Unexpected, img1.Unreadable)
	}

	// The catalog agrees: image 1 stamped, image 2 untouched.
	state1, verifiedAt1, _, _ := imageStateRow(t, cat, setID, 1)
	if state1 != "verified" || !verifiedAt1.Valid {
		t.Errorf("image 1 = (state %q, verified_at %v), want the stamp", state1, verifiedAt1)
	}
	state2, verifiedAt2, _, _ := imageStateRow(t, cat, setID, 2)
	if state2 != "built" || verifiedAt2.Valid {
		t.Errorf("image 2 = (state %q, verified_at %v), want ('built', NULL): the anomaly never stamps", state2, verifiedAt2)
	}
}

// --- idempotent re-copy ----------------------------------------------------------

// TestVerifyMediaIdempotentRecopyKeepsFinalPath: a clean resume re-run
// of copy must not blank, move, or duplicate the final_path it recorded
// on the first completion - verify-media still resolves the image and
// still exits 0 afterwards.
func TestVerifyMediaIdempotentRecopyKeepsFinalPath(t *testing.T) {
	const setID = "S-VRECOPY"
	cat, dest := verifyCopyFixture(t, setID)
	want := destImageDir(dest, setID, 1)

	stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest)
	if code != 0 {
		t.Fatalf("resume re-run exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	var finalPath string
	if err := catOpen(t, cat).QueryRow(
		`SELECT final_path FROM images WHERE set_id = ? AND image_id = 1`, setID).Scan(&finalPath); err != nil {
		t.Fatalf("reading final_path: %v", err)
	}
	if finalPath != want {
		t.Fatalf("final_path = %q after the re-copy, want %q (idempotent)", finalPath, want)
	}

	stdout, stderr, code = runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID, "--json")
	if code != report.ExitOK {
		t.Fatalf("verify after the re-copy exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	var rep verifyMediaReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("report does not parse: %v\n%s", err, stdout)
	}
	if rep.Verified != 1 || !verifyMediaImageOf(t, rep, 1).Stamped {
		t.Errorf("report = %+v, want the image verified + stamped after the idempotent re-copy", rep)
	}
}

// --- the gated REAL-xorriso PARSE-failure edge (12.6) -----------------------------

// contentListRows reads the image's content-list row count - the N a
// clean match must still report when its stamp cannot be written.
func contentListRows(t *testing.T, cat, setID string, imageID int64) int {
	t.Helper()
	var n int
	if err := catOpen(t, cat).QueryRow(
		`SELECT count(*) FROM image_contents WHERE set_id = ? AND image_id = ?`, setID, imageID).Scan(&n); err != nil {
		t.Fatalf("counting content rows: %v", err)
	}
	if n == 0 {
		t.Fatalf("set %s image %d has no content rows - the fixture cannot exercise a full match", setID, imageID)
	}
	return n
}

// TestVerifyMediaStampWriteFailureShipsContentVerdict is the FLIPPED
// pin (it shipped as TestVerifyMediaStampWriteFailureShipsAsPerImageFailure,
// which pinned the deviation QA proved empirically): a stamp write
// refused at the DATABASE - a BEFORE UPDATE trigger aborting the
// state='verified' write, the store seam having no injection point of
// its own - must NOT fail the image.
//
// THE RULING as pinned here (DESIGN_SPEC 2.1, Clarified 2026-10-21:
// "a stamp that ever exceeded busy_timeout is non-fatal: the content
// result still reports verified, and a later verify re-stamps"): the
// content verdict SHIPS (full counts, status "verified", no per-image
// failure), Stamped is false, a WARNING naming the stamp failure lands
// in the report (the image's "warnings" entry) AND on stderr
// (progress.Warn), the run exits 0 - warnings never create exit 10 (the
// metadata-warning precedent) - the catalog keeps ('built', NULL) (the
// copy completion's row, T6.5; zero writes of its own), and a LATER
// verify with the obstruction gone STAMPS.
func TestVerifyMediaStampWriteFailureShipsContentVerdict(t *testing.T) {
	const setID = "S-VSTAMPFAIL"
	cat, _ := verifyCopyFixture(t, setID)
	expected := contentListRows(t, cat, setID, 1)

	rw := catRW(t, cat)
	const trigger = `CREATE TRIGGER test_block_verify_stamp BEFORE UPDATE OF state ON images
	FOR EACH ROW WHEN NEW.state = 'verified'
	BEGIN SELECT RAISE(ABORT, 'verify stamp blocked (test seam)'); END`
	if _, err := rw.Exec(trigger); err != nil {
		t.Fatalf("creating the stamp-blocking trigger: %v", err)
	}
	t.Cleanup(func() { _, _ = rw.Exec(`DROP TRIGGER IF EXISTS test_block_verify_stamp`) })
	dropTrigger := func() {
		t.Helper()
		if _, err := rw.Exec(`DROP TRIGGER IF EXISTS test_block_verify_stamp`); err != nil {
			t.Fatalf("dropping the stamp-blocking trigger: %v", err)
		}
	}

	stdout, stderr, code := runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID, "--json")
	t.Logf("stamp-write failure: exit=%d\nstdout:%s\nstderr:%s", code, stdout, stderr)
	if code == report.ExitCatalogBusy {
		t.Errorf("exit = 5: the stamp failure must NEVER surface the busy class (2.1 carve-out)")
	}
	if code != report.ExitOK {
		t.Errorf("exit = %d, want 0 (the ruling: a stamp failure is a WARNING; warnings exit 0)\nstdout: %s\nstderr: %s",
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
	if img.Status != "verified" {
		t.Errorf("image status = %q, want 'verified' (the content result still reports verified)", img.Status)
	}
	if img.Failure != "" {
		t.Errorf("failure = %q, want empty: a stamp failure is not a per-image failure", img.Failure)
	}
	// THE FULL CONTENT VERDICT SHIPS (the half the deviation discarded).
	if img.Matched != expected || img.Mismatched != 0 || img.Missing != 0 ||
		img.Unexpected != 0 || img.Unreadable != 0 {
		t.Errorf("counts = %+v, want all %d listed rows matched and nothing classified as an anomaly", img, expected)
	}
	if img.Stamped {
		t.Error("stamped = true, want false: no stamp was written, so none is claimed")
	}
	if rep.Verified != 0 {
		t.Errorf("verified = %d, want 0 (nothing was stamped this run)", rep.Verified)
	}
	// THE WARNING - in the report ...
	if len(img.Warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly the stamp-failure warning", img.Warnings)
	}
	for _, want := range []string{"matched its content list fully", "verify stamp blocked", "later verify re-stamps"} {
		if !strings.Contains(img.Warnings[0], want) {
			t.Errorf("warning = %q, want it to contain %q", img.Warnings[0], want)
		}
	}
	// ... and on stderr (progress.Warn).
	if !strings.Contains(stderr, "warning: ") || !strings.Contains(stderr, "verify stamp blocked") {
		t.Errorf("stderr lacks the progress.Warn line naming the stamp failure:\n%s", stderr)
	}

	// The catalog half of the ruling HOLDS: nothing was written - the
	// row keeps the copy completion's state ('built', T6.5) and stays
	// unstamped.
	state, verifiedAt, _, _ := imageStateRow(t, cat, setID, 1)
	if state != "built" || verifiedAt.Valid {
		t.Errorf("images row = (state %q, verified_at %v), want ('built', NULL): a failed stamp writes nothing", state, verifiedAt)
	}

	// The HUMAN report carries the same warning under the image line.
	stdout, stderr, code = runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID)
	if code != report.ExitOK {
		t.Fatalf("human-form verify exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	for _, want := range []string{"image 1: verified ", "stamped false", "  warning: ", "verify stamp blocked"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("human report lacks %q:\n%s", want, stdout)
		}
	}

	// A LATER VERIFY RE-STAMPS (the ruling's recovery clause).
	dropTrigger()
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
		t.Errorf("follow-up image report = %+v (run %+v), want a clean stamped image with no warning", img, rep)
	}
	state, verifiedAt, _, _ = imageStateRow(t, cat, setID, 1)
	if state != "verified" || !verifiedAt.Valid || verifiedAt.Int64 <= 0 {
		t.Errorf("images row = (state %q, verified_at %v), want the re-stamp after the obstruction was removed", state, verifiedAt)
	}
}

// TestVerifyMediaStrictPromotesAnomaliesToExitOne: without --strict an
// anomalous run exits 10 (automation decides policy); with --strict the
// SAME report is promoted to the fatal class (exit 1), while stdout
// still carries the full report (FR-046: the result is the stdout
// artifact on every exit path).
func TestVerifyMediaStrictPromotesAnomaliesToExitOne(t *testing.T) {
	const setID = "S-VSTRICT"
	cat, dest := verifyCopyFixture(t, setID)

	small1 := filepath.Join(destImageDir(dest, setID, 1), "files", "small1.txt")
	orig, err := os.ReadFile(small1)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), orig...)
	corrupt[0] ^= 0xff
	if err := os.WriteFile(small1, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID, "--json", "--strict")
	if code != report.ExitFatal {
		t.Fatalf("exit = %d, want 1 (--strict promotes the anomaly tail)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "--strict") {
		t.Errorf("stderr does not explain the promotion:\n%s", stderr)
	}
	var rep verifyMediaReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("report does not parse on the promoted path: %v\n%s", err, stdout)
	}
	if rep.Status != "partial" || rep.Mismatched != 1 || rep.Verified != 0 {
		t.Errorf("report = %+v, want the same partial findings as the unpromoted run", rep)
	}
}

// TestVerifyMediaInterruptedDuringExtractionExits130 drives the third
// exit class through the REAL command path: an ISO image in scope whose
// (fake) xorriso parks inside the extraction until signaled. ONE SIGINT
// must abort with 130 through the interrupted class, stamp NOTHING, and
// clean the extraction scratch from --work-dir.
func TestVerifyMediaInterruptedDuringExtractionExits130(t *testing.T) {
	const setID = "S-VINT"
	cat, _ := verifyCopyFixture(t, setID)

	// Point image 1 at a regular file so the pre-pass classifies it as
	// an ISO (xorriso in scope). No real image is needed: the fake
	// tool below never really extracts anything.
	iso := filepath.Join(t.TempDir(), setID+"-001.iso")
	if err := os.WriteFile(iso, []byte("placeholder image"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := catRW(t, cat).Exec(
		`UPDATE images SET final_path = ? WHERE set_id = ? AND image_id = 1`, iso, setID); err != nil {
		t.Fatal(err)
	}

	// The fake xorriso: a parseable >= 1.5.4 banner for the T4.1
	// probe, then it announces the extraction and parks until killed.
	sentinel := filepath.Join(t.TempDir(), "extract-started")
	fakeDir := t.TempDir()
	script := "#!/bin/sh\n" +
		"case \"$1\" in\n" +
		"  -version) echo \"xorriso 1.5.6 : RockRidge filesystem manipulator, libburnia project.\"; exit 0;;\n" +
		"esac\n" +
		": > \"" + sentinel + "\"\n" +
		"exec sleep 60\n"
	if err := os.WriteFile(filepath.Join(fakeDir, "xorriso"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	child := exec.Command(os.Args[0],
		"verify-media", "--catalog", cat, "--set-id", setID, "--json", "--work-dir", work)
	// scanSignalHelperEnv selects the GENERIC Execute-wiring re-exec
	// child (scanSignalHelperMain: armed single-channel signal guard +
	// execute over the REAL command tree with the parent's args - the
	// name is the T2.x scan-abort heritage, the wiring it reproduces is
	// command-independent). The plain cliHelperEnv path passes
	// context.Background() and therefore has NO signal guard, so a real
	// SIGINT there would kill the child by default action instead of
	// running the graceful 130 path this test pins.
	child.Env = append(os.Environ(), scanSignalHelperEnv+"=1",
		"PATH="+fakeDir+":"+os.Getenv("PATH"))
	childOut, childErr := &bytes.Buffer{}, &bytes.Buffer{}
	child.Stdout, child.Stderr = childOut, childErr
	if err := child.Start(); err != nil {
		t.Fatalf("starting verify child: %v", err)
	}
	defer func() {
		if child.ProcessState == nil {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()

	// Wait for the extraction to be IN FLIGHT, then ONE SIGINT.
	deadline := time.Now().Add(60 * time.Second)
	for {
		if _, err := os.Stat(sentinel); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = child.Process.Kill()
			_, _ = child.Process.Wait()
			t.Fatalf("the fake extraction never started (stderr: %s)", childErr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := child.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("SIGINT to verify child: %v", err)
	}

	waitErr := child.Wait()
	exitCode := -1
	if waitErr != nil {
		exitErr, ok := waitErr.(*exec.ExitError)
		if !ok {
			t.Fatalf("verify child: %v (stderr: %s)", waitErr, childErr.String())
		}
		exitCode = exitErr.ExitCode()
	}
	if exitCode != report.ExitInterrupted {
		t.Errorf("exit = %d, want 130 (interrupted)\nstdout: %s\nstderr: %s",
			exitCode, childOut.String(), childErr.String())
	}
	if !strings.Contains(childErr.String(), "interrupted by signal") {
		t.Errorf("stderr lacks the interruption message:\n%s", childErr.String())
	}
	if strings.Contains(childErr.String(), "forcing exit") {
		t.Errorf("a SINGLE SIGINT must not trigger the second-signal hard escape:\n%s", childErr.String())
	}

	// The interrupted run stamps NOTHING and leaves no scratch behind
	// (the row keeps the copy completion's state, T6.5).
	state, verifiedAt, _, _ := imageStateRow(t, cat, setID, 1)
	if state != "built" || verifiedAt.Valid {
		t.Errorf("images row = (state %q, verified_at %v), want ('built', NULL): an interrupted run never stamps", state, verifiedAt)
	}
	entries, err := os.ReadDir(work)
	if err != nil {
		t.Fatalf("reading --work-dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("--work-dir holds %d entries after the interruption (leaked scratch)", len(entries))
	}
}

// TestVerifyMediaMissingCatalogExitsFour completes the documented exit
// matrix for THIS command: an unreadable --catalog is the CatalogError
// class (exit 4) out of the very first open, before any progress bus is
// started - so nothing lands on stdout (the report is written by the
// run, and this run never began).
func TestVerifyMediaMissingCatalogExitsFour(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such.sqlite")
	stdout, stderr, code := runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", missing, "--set-id", "S-404")
	if code != report.ExitCatalog {
		t.Fatalf("exit = %d, want 4 (catalog)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout must stay empty on a catalog-open failure, got: %q", stdout)
	}
	if !strings.Contains(stderr, missing) {
		t.Errorf("stderr does not name the missing catalog:\n%s", stderr)
	}
}
