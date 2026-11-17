package cli

// T3.3 adversarial depth pass (the delegated verification brief): the
// copy command's wiring-level gaps the base matrix left open.
//
//   - D-11 FLOCK: copy is a mutating command - while another process
//     holds the exclusive catalog lock, the copy command fails fast
//     with exit 5 BEFORE any run row or filesystem effect.
//   - The 12.7 ENV SEAM: the adapter is inert without
//     ISOPRESS_TEST_COPY_FAIL_AFTER (setting KIND/MODE alone can
//     never activate it), validates its inputs, and behaves per the
//     documented mode contract (block parks-until-cancel with the
//     ErrHookAbort sentinel; error aborts deterministically in-process;
//     kill's RUNTIME behavior is pinned by the SIGKILL crash e2e,
//     here only its construction is checked).
//   - ERROR MODE e2e (the third seam mode, previously unused by the
//     suite): a deterministic in-process hook abort finalizes the run
//     'failed' (exit 1, the fatal class), marks nothing, and leaves no
//     partial residue - while the report still lands on stdout (FR-046).
//   - 5.4 METADATA WARNINGS at the report layer: a copied item's
//     metadata warning is COUNTED and RENDERED but never flips the
//     exit code - the exit-10 decision (and --strict) considers only
//     skips and failures.
//   - INCOMPLETE-IMAGE DEFERRAL (6.6): a MANIFEST.json embed failure
//     mid-image makes THAT image incomplete and recorded-failed, and
//     the run CONTINUES with the other images (copyImage returns a
//     terminal error only for plumbing failures, never per-item
//     problems).
//
// Fix-round additions (B1 / NB1 / NB2, the go-reviewer rejection):
//
//   - B1 PARTIAL-COLLISION GATE: the colliding pair `a` vs `a.partial`
//     is detected (unit on the pure gate function), the near misses
//     pass, the chunk/embed exemptions are asserted structurally, and
//     the E2E pins the order-independent outcome end to end (survivor
//     byte-identical, colliding item warned + skipped + exit 10,
//     stable across re-runs).
//   - NB1: the run-scoped export failure path now finalizes the run
//     row 'failed' (it used to strand it 'running').
//   - NB2: the 12.7 env seam is validated BEFORE StartCopyRun as a
//     usage-class input (exit 2), so a malformed seam value can never
//     mint an abandoned runs row.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"isopress/internal/catalog"
	"isopress/internal/copyengine"
	"isopress/internal/fsmeta"
	"isopress/internal/media"
	"isopress/internal/progress"
	"isopress/internal/report"
)

// --- D-11: one mutating run at a time ----------------------------------------

// While a holder process owns the MUTATING flock (the T0.4 re-exec
// holder pattern every other mutating command pins), the copy command
// fails fast with exit 5, prints the busy message on stderr, emits
// NOTHING on stdout, and records NO copy run row - the refusal happens
// before StartCopyRun, so a busy catalog cannot leave run debris.
func TestCopyLockContentionExitsBusy5(t *testing.T) {
	f := newCopyCrashFixture(t)

	holder := exec.Command(os.Args[0])
	holder.Env = append(os.Environ(), lockHoldEnv+"=1", lockHoldPathEnv+"="+f.cat)
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

	stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", f.cat, "--set-id", f.setID, "--dest", f.dest)
	if code != report.ExitCatalogBusy {
		t.Fatalf("copy against a locked catalog exit = %d, want 5\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "busy") {
		t.Errorf("stderr lacks the busy message:\n%s", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout must stay empty on catalog-busy, got: %q", stdout)
	}
	db := catOpen(t, f.cat)
	var copyRuns int
	if err := db.QueryRow(`SELECT COUNT(*) FROM runs WHERE kind = 'copy'`).Scan(&copyRuns); err != nil {
		t.Fatal(err)
	}
	if copyRuns != 0 {
		t.Fatalf("%d copy runs rows exist despite the busy refusal; the flock check must precede StartCopyRun", copyRuns)
	}
}

// --- the 12.7 env seam: inertness, validation, mode behavior -----------------

// The env adapter must be INERT without ISOPRESS_TEST_COPY_FAIL_
// AFTER (the other two variables can never activate it alone), must
// reject malformed inputs as usage-class errors before any write, and
// must implement the documented modes: block parks until cancellation
// and unwraps ErrHookAbort; error aborts deterministically with the
// sentinel; kill only ever builds (its SIGKILL runtime is pinned by
// the crash e2e - invoking it here would kill the test binary).
func TestCopyHookEnvAdapterInertAndValidated(t *testing.T) {
	// Inert: KIND/MODE without the AFTER trigger - nil hook, no error.
	t.Setenv("ISOPRESS_TEST_COPY_FAIL_KIND", "kill")
	t.Setenv("ISOPRESS_TEST_COPY_FAIL_MODE", "kill")
	t.Setenv("ISOPRESS_TEST_COPY_FAIL_AFTER", "")
	if hook, err := copyTestHookFromEnv(); err != nil || hook != nil {
		t.Fatalf("adapter without the AFTER trigger = (%v, %v), want (nil, nil) - no accidental activation", hook, err)
	}

	// Validation: malformed trigger and out-of-contract kind/mode are
	// errors before any filesystem effect.
	for _, tc := range []struct{ name, after, kind, mode string }{
		{"zero after", "0", "partial", "error"},
		{"garbage after", "abc", "partial", "error"},
		{"bad kind", "1", "boom", "error"},
		{"bad mode", "1", "partial", "boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("ISOPRESS_TEST_COPY_FAIL_AFTER", tc.after)
			t.Setenv("ISOPRESS_TEST_COPY_FAIL_KIND", tc.kind)
			t.Setenv("ISOPRESS_TEST_COPY_FAIL_MODE", tc.mode)
			if _, err := copyTestHookFromEnv(); err == nil {
				t.Fatalf("adapter accepted AFTER=%q KIND=%q MODE=%q", tc.after, tc.kind, tc.mode)
			}
		})
	}

	// Block mode: fires on the N-th matching event only, passes other
	// kinds through, and returns the ErrHookAbort sentinel once the
	// context is cancelled (the parked run unblocks).
	t.Setenv("ISOPRESS_TEST_COPY_FAIL_AFTER", "2")
	t.Setenv("ISOPRESS_TEST_COPY_FAIL_KIND", "partial")
	t.Setenv("ISOPRESS_TEST_COPY_FAIL_MODE", "block")
	hook, err := copyTestHookFromEnv()
	if err != nil || hook == nil {
		t.Fatalf("valid block-mode adapter = (%v, %v)", hook, err)
	}
	ctx := context.Background()
	if err := hook(ctx, copyengine.HookEvent{Kind: copyengine.HookAfterRename}); err != nil {
		t.Fatalf("wrong-kind event must pass through, got: %v", err)
	}
	if err := hook(ctx, copyengine.HookEvent{Kind: copyengine.HookAfterPartialWrite}); err != nil {
		t.Fatalf("first matching event must count silently (n=2), got: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := hook(cancelled, copyengine.HookEvent{Kind: copyengine.HookAfterPartialWrite}); !isHookAbort(err) {
		t.Fatalf("second matching event under a cancelled ctx = %v, want an ErrHookAbort-wrapped error", err)
	}

	// Error mode: deterministic in-process abort with the sentinel.
	t.Setenv("ISOPRESS_TEST_COPY_FAIL_AFTER", "1")
	t.Setenv("ISOPRESS_TEST_COPY_FAIL_MODE", "error")
	hookErr, err := copyTestHookFromEnv()
	if err != nil || hookErr == nil {
		t.Fatalf("valid error-mode adapter = (%v, %v)", hookErr, err)
	}
	if err := hookErr(ctx, copyengine.HookEvent{Kind: copyengine.HookAfterPartialWrite}); !isHookAbort(err) {
		t.Fatalf("error-mode hook = %v, want an ErrHookAbort-wrapped error", err)
	}

	// Kill mode: CONSTRUCTS cleanly (runtime SIGKILL is the crash
	// e2e's assertion; never invoked in-process).
	t.Setenv("ISOPRESS_TEST_COPY_FAIL_MODE", "kill")
	if hookKill, err := copyTestHookFromEnv(); err != nil || hookKill == nil {
		t.Fatalf("valid kill-mode adapter = (%v, %v)", hookKill, err)
	}
}

func isHookAbort(err error) bool {
	return err != nil && strings.Contains(err.Error(), "test hook") &&
		errors.Is(err, copyengine.ErrHookAbort)
}

// --- error mode through the REAL command tree --------------------------------

// ISOPRESS_TEST_COPY_FAIL_MODE=error is the deterministic in-process
// half of the 12.7 seam: the first payload write aborts with the
// sentinel, the run finalizes 'failed' (the fatal class, exit 1), no
// item is marked, no partial survives, and the report still lands on
// stdout (FR-046: the result is the stdout artifact even on abort).
func TestCopyErrorModeHookAbortsRunFailed(t *testing.T) {
	f := newCopyCrashFixture(t)
	stdout, stderr, code := runCopyChild(t, []string{
		"ISOPRESS_TEST_COPY_FAIL_AFTER=1",
		"ISOPRESS_TEST_COPY_FAIL_KIND=partial",
		"ISOPRESS_TEST_COPY_FAIL_MODE=error",
	}, "copy", "--catalog", f.cat, "--set-id", f.setID, "--dest", f.dest, "--json")
	if code != 1 {
		t.Fatalf("error-mode exit = %d, want 1 (fatal class; stderr: %s)\nstdout: %s", code, stderr, stdout)
	}

	// The run row finalizes 'failed', not 'running' and not 'ok'.
	db := catOpen(t, f.cat)
	var status string
	if err := db.QueryRow(`SELECT status FROM runs WHERE kind = 'copy'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("run status = %q, want failed", status)
	}
	// Nothing was marked and the aborted item's partial was cleaned.
	for rel, copied := range copiedFlags(t, f.cat, f.setID) {
		if copied {
			t.Fatalf("item %s marked despite the hook abort", rel)
		}
	}
	partials, err := filepath.Glob(filepath.Join(f.dest, "*", "files", "*.partial"))
	if err != nil {
		t.Fatal(err)
	}
	if len(partials) != 0 {
		t.Fatalf("partials survived the hook abort: %v", partials)
	}
	// The report still parses on stdout with the aborted image honest
	// about its incompleteness.
	var rep copyReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("stdout is not a JSON report after the abort: %v\n%s", err, stdout)
	}
	if rep.Status != "failed" {
		t.Fatalf("report status = %q, want failed", rep.Status)
	}
	if len(rep.Images) != 1 || rep.Images[0].Complete || rep.Images[0].Manifest {
		t.Fatalf("report images = %+v, want one incomplete image without a manifest", rep.Images)
	}
}

// --- 5.4 metadata warnings at the report layer --------------------------------

// A COPIED item carrying a metadata warning is counted as a warning -
// never a skip, never a failure - and the exit code stays 0 for
// metadata-only problems, even under --strict. The warning surfaces in
// the report counts (JSON key, human line, and the Done summary).
func TestCopyMetadataWarningKeepsExitZero(t *testing.T) {
	catPath := createCatalog(t)
	db, err := catalog.Open(catPath, catalog.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.EnsureSet(ctx, "S-META", "/data", time.Now().UnixNano(), catalog.ToolVersion); err != nil {
		t.Fatal(err)
	}
	runID, err := db.StartCopyRun(ctx, catalog.CopyRun{
		SetID: "S-META", Root: "/mnt/backup", StartedAt: time.Now().UnixNano(),
	})
	if err != nil {
		t.Fatal(err)
	}

	newWarnRunner := func(strict, asJSON bool) (*copyRunner, *bytes.Buffer) {
		var out bytes.Buffer
		cmd := &cobra.Command{}
		cmd.SetOut(&out)
		r := &copyRunner{
			cmd: cmd, db: db, setID: "S-META", destAbs: "/mnt/backup", runID: runID,
			bus: progress.NewBus(), strict: strict, jsonFlag: asJSON,
		}
		return r, &out
	}
	warnRes := copyengine.Result{
		Outcome: copyengine.OutcomeCopied, Bytes: 10, Attempts: 1,
		Warning: "metadata: chown \"docs/a.txt\" to uid 1 gid 0 skipped: operation not permitted (non-root EPERM downgrade, DESIGN_SPEC 5.4)",
	}

	// classify: counted as a warning, never a skip/failure; the item
	// still counts as COMPLETED.
	r, out := newWarnRunner(false, false)
	var rep copyImageReport
	if done := r.classify(ctx, &rep, 1, "docs/a.txt", warnRes); !done {
		t.Fatal("a metadata-warning item must classify as completed")
	}
	if rep.Copied != 1 || rep.Skipped != 0 || rep.Failed != 0 {
		t.Fatalf("image report = %+v, want 1 copied, 0 skipped/failed", rep)
	}
	if r.metadataWarnings != 1 {
		t.Fatalf("metadata warnings = %d, want 1", r.metadataWarnings)
	}
	r.images = append(r.images, rep)

	// finalize: nil (exit 0) - the exit-10 decision excludes metadata
	// warnings - and the human report renders the count.
	summary := ""
	if err := r.finalize(ctx, &summary, nil); err != nil {
		t.Fatalf("finalize with a metadata-only warning = %v, want nil (exit 0)", err)
	}
	if !strings.Contains(out.String(), "metadata warnings: 1") {
		t.Fatalf("human report lacks the metadata-warnings line:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "(status: ok)") {
		t.Fatalf("human report status is not ok:\n%s", out.String())
	}
	if !strings.Contains(summary, "metadata warnings 1") {
		t.Fatalf("Done summary lacks the warning count: %q", summary)
	}

	// --strict does NOT promote metadata-only warnings either.
	rStrict, outStrict := newWarnRunner(true, false)
	var repStrict copyImageReport
	rStrict.classify(ctx, &repStrict, 1, "docs/a.txt", warnRes)
	rStrict.images = append(rStrict.images, repStrict)
	if err := rStrict.finalize(ctx, &summary, nil); err != nil {
		t.Fatalf("--strict finalize with a metadata-only warning = %v, want nil (exit 0)", err)
	}
	if !strings.Contains(outStrict.String(), "metadata warnings: 1") {
		t.Fatal("--strict report lost the warning count")
	}

	// The JSON presentation carries the same count with status ok.
	rJSON, outJSON := newWarnRunner(false, true)
	var repJSON copyImageReport
	rJSON.classify(ctx, &repJSON, 1, "docs/a.txt", warnRes)
	rJSON.images = append(rJSON.images, repJSON)
	if err := rJSON.finalize(ctx, &summary, nil); err != nil {
		t.Fatalf("json finalize with a metadata-only warning = %v, want nil", err)
	}
	var parsed copyReport
	if err := json.Unmarshal(outJSON.Bytes(), &parsed); err != nil {
		t.Fatalf("json report does not parse: %v\n%s", err, outJSON.String())
	}
	if parsed.MetadataWarnings != 1 || parsed.Status != "ok" || parsed.Copied != 1 {
		t.Fatalf("json report = %+v, want metadata_warnings=1, status=ok, copied=1", parsed)
	}
}

// --- 6.6: a failed MANIFEST embed defers THAT image, the run continues ------

// copyAdvFixture seeds a scanned + planned two-image set through the
// EXPORTED catalog API (no child processes): f1.txt -> image 1,
// f2.txt -> image 2, one real source file each whose lstat fields the
// scan row mirrors exactly (so the copy's step-1 comparison matches).
func copyAdvFixture(t *testing.T, setID string) (catPath, sourceRoot string, fileIDs map[string]int64) {
	t.Helper()
	sourceRoot = t.TempDir()
	catPath = filepath.Join(t.TempDir(), "catalog.sqlite")
	if err := catalog.Create(catPath); err != nil {
		t.Fatal(err)
	}
	db, err := catalog.Open(catPath, catalog.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.EnsureSet(ctx, setID, sourceRoot, time.Now().UnixNano(), catalog.ToolVersion); err != nil {
		t.Fatal(err)
	}
	runID, err := db.StartScanRun(ctx, catalog.ScanRun{
		SetID: setID, Kind: "ingest", Root: sourceRoot, StartedAt: time.Now().UnixNano(),
	})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.BeginScanTx(ctx, runID, setID)
	if err != nil {
		t.Fatal(err)
	}
	fileIDs = map[string]int64{}
	for _, name := range []string{"f1.txt", "f2.txt"} {
		content := []byte("payload of " + name)
		writeFileP(t, filepath.Join(sourceRoot, name), content, 0o644)
		meta, err := fsmeta.PathMeta(filepath.Join(sourceRoot, name))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(content)
		id, err := tx.InsertScanFile(ctx, catalog.FileRow{
			RelPath: name, EntryKind: "regular", Size: meta.Size,
			Mode: 0o644, MtimeNS: meta.MtimeNS, Dev: int64(meta.Dev), Inode: int64(meta.Inode),
			Nlink: 1, Hash: sum[:], Status: "active",
		})
		if err != nil {
			t.Fatal(err)
		}
		fileIDs[name] = id
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	ptx, err := db.BeginPlanTx(ctx, 0, setID)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{1, 2} {
		if err := ptx.InsertImage(ctx, catalog.ImageSpec{
			ImageID: id, NominalSize: 100000, UsableSize: 98000, MediaProfile: "disk",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := ptx.AssignFile(ctx, fileIDs["f1.txt"], 1); err != nil {
		t.Fatal(err)
	}
	if err := ptx.AssignFile(ctx, fileIDs["f2.txt"], 2); err != nil {
		t.Fatal(err)
	}
	if err := ptx.Commit(); err != nil {
		t.Fatal(err)
	}
	return catPath, sourceRoot, fileIDs
}

// A disk-full (injected) during image 1's MANIFEST.json embed: image 1
// is recorded incomplete-failed with NO manifest on disk, its PAYLOAD
// remains verified and marked, and the run CONTINUES - image 2 lands
// completely (payload + export + manifest + content rows). The embed
// failure is a per-image failure, never a terminal run error.
func TestCopyManifestFailureDefersOnlyFailingImage(t *testing.T) {
	setID := "S-ADV"
	catPath, sourceRoot, _ := copyAdvFixture(t, setID)
	db, err := catalog.Open(catPath, catalog.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	runID, err := db.StartCopyRun(ctx, catalog.CopyRun{
		SetID: setID, Root: "/adv-dest", StartedAt: time.Now().UnixNano(),
	})
	if err != nil {
		t.Fatal(err)
	}

	// The run's single export (6.6 shape, hand-fed).
	exportBytes := []byte("pretend catalog export bytes")
	exportSum := sha256.Sum256(exportBytes)
	exportDir := t.TempDir()
	exportPath := filepath.Join(exportDir, media.ExportFileName)
	if err := os.WriteFile(exportPath, exportBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	export := &runExport{
		path: exportPath, size: int64(len(exportBytes)), hash: exportSum[:], dir: exportDir,
	}

	dest := filepath.Join(t.TempDir(), "dest")
	manifestPartialSuffix := filepath.Join(setID+"-001", media.ManifestFileName+".partial")
	r := &copyRunner{
		db: db, setID: setID, sourceRoot: sourceRoot, destAbs: dest, runID: runID,
		bus: progress.NewBus(),
		deps: copyengine.Deps{
			// Disk full ONLY for image 1's manifest partial.
			OpenPartial: func(path string) (copyengine.WriteSyncCloser, error) {
				if strings.HasSuffix(path, manifestPartialSuffix) {
					return nil, fmt.Errorf("injected disk full on the image-1 manifest: %w", syscall.ENOSPC)
				}
				return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			},
		},
	}

	if err := r.copyImage(ctx, 1, export); err != nil {
		t.Fatalf("copyImage(image 1) = %v; a manifest embed failure must be a per-image failure, not a terminal run error", err)
	}
	if err := r.copyImage(ctx, 2, export); err != nil {
		t.Fatalf("copyImage(image 2) = %v; the run must CONTINUE after the failing image", err)
	}

	// The image reports: image 1 failed its manifest only; image 2 is
	// complete with its manifest.
	if len(r.images) != 2 {
		t.Fatalf("image reports = %d, want 2 (the second copyImage must have run)", len(r.images))
	}
	img1, img2 := r.images[0], r.images[1]
	if img1.Image != 1 || img1.Copied != 1 || img1.Failed != 1 || img1.Complete || img1.Manifest {
		t.Fatalf("image 1 report = %+v, want payload copied + manifest failed + incomplete", img1)
	}
	if img2.Image != 2 || img2.Copied != 1 || img2.Failed != 0 || !img2.Complete || !img2.Manifest {
		t.Fatalf("image 2 report = %+v, want complete with manifest", img2)
	}
	if len(r.failureLines) != 1 || !strings.Contains(r.failureLines[0], media.ManifestFileName) {
		t.Fatalf("failure lines = %v, want the image-1 MANIFEST.json failure", r.failureLines)
	}

	// Filesystem: image 1 keeps its verified payload and export but NO
	// manifest; image 2 is fully materialized.
	img1Root := destImageDir(dest, setID, 1)
	img2Root := destImageDir(dest, setID, 2)
	assertBytes(t, filepath.Join(img1Root, "files", "f1.txt"), []byte("payload of f1.txt"))
	assertBytes(t, filepath.Join(img1Root, media.ExportFileName), exportBytes)
	if _, err := os.Lstat(filepath.Join(img1Root, media.ManifestFileName)); !os.IsNotExist(err) {
		t.Fatalf("image 1 published a manifest despite the embed failure (err=%v)", err)
	}
	assertBytes(t, filepath.Join(img2Root, "files", "f2.txt"), []byte("payload of f2.txt"))
	assertBytes(t, filepath.Join(img2Root, media.ExportFileName), exportBytes)
	m2 := readManifest(t, filepath.Join(img2Root, media.ManifestFileName), setID, 2)
	f2MediaPath, err := media.FileMediaPath("f2.txt")
	if err != nil {
		t.Fatal(err)
	}
	if len(m2.Entries) != 1 || m2.Entries[0].MediaPath != f2MediaPath {
		t.Fatalf("image 2 manifest entries = %+v, want %s", m2.Entries, f2MediaPath)
	}

	// Catalog: both payload flags stand (the manifest failure never
	// unmarks), image 1 lacks ONLY the manifest self-row, image 2 has
	// the full 6.6 set.
	dbRO := catOpen(t, catPath)
	for rel, want := range map[string]bool{"f1.txt": true, "f2.txt": true} {
		var copied bool
		if err := dbRO.QueryRow(`SELECT copied FROM files WHERE set_id = ? AND relpath = ?`, setID, rel).
			Scan(&copied); err != nil {
			t.Fatal(err)
		}
		if copied != want {
			t.Fatalf("copied flag of %s = %v, want %v", rel, copied, want)
		}
	}
	assertContentKinds(t, dbRO, setID, map[int64]map[string]int{
		1: {"file": 1, "export": 1}, // NO manifest row: deferred
		2: {"file": 1, "export": 1, "manifest": 1},
	})
}

// --- B1 (fix round, blocking): the partial-collision gate --------------------

// THE B1 DEFECT (real-repro'd): two legal source files `a` and
// `a.partial` on one image. Once `a.partial` is copied+verified+marked
// (its final sits at files/a.partial), the subsequent copy of sibling
// `a` derives its temporary path as files/a.partial - the completed
// final of the OTHER item - and copyengine step 2a's os.Remove deletes
// it: marked+verified bytes gone, and resume never repairs (the copied
// flag IS the trust). The wipe hit ~50% of runs via the hash-pool
// file_id order. The pipeline's gate (copy.go
// partialPathCollision) skips the colliding item BEFORE any write.

// The GATE UNIT: the colliding pair is detected, the near misses pass,
// and the chunk/embed EXEMPTION reasoning is asserted structurally (the
// brief requires the reasoning pinned in a test comment).
func TestCopyPartialCollisionGateUnit(t *testing.T) {
	inv := catalog.CopyInventory{
		Files: []catalog.CopyFile{
			{RelPath: "a"},
			{RelPath: "a.partial"},
			{RelPath: "a.partialx"},
			{RelPath: "a.partials"},
			{RelPath: "sub/b"},
		},
		Chunks: []catalog.CopyChunk{
			{ParentRelPath: "big.bin", ChunkIndex: 0},
			{ParentRelPath: "a.partial", ChunkIndex: 3}, // a chunk of the sibling
		},
	}
	finals := imageFinalMediaPaths(inv)

	// THE colliding pair: `a`'s derived partial IS `a.partial`'s final
	// (the data-destruction repro); the sibling's relpath comes back
	// for the warning message.
	if sibling, collided := partialPathCollision(finals, "files/a"); !collided || sibling != "a.partial" {
		t.Fatalf("files/a: collision = (%q, %v), want (\"a.partial\", true)", sibling, collided)
	}
	// The ASYMMETRY that makes the gate order-independent: `a.partial`'s
	// own derived partial (files/a.partial.partial) collides with
	// nothing, so the sibling is NEVER gated - `a` is always the skipped
	// item, in either file_id order (pinned e2e below).
	if sibling, collided := partialPathCollision(finals, "files/a.partial"); collided {
		t.Fatalf("files/a.partial must pass the gate, collided with %q", sibling)
	}
	// Non-colliding near misses pass: a longer suffix name
	// (a.partialx, a.partials) and an unrelated subtree (sub/b).
	for _, mp := range []string{"files/a.partialx", "files/a.partials", "files/sub/b"} {
		if sibling, collided := partialPathCollision(finals, mp); collided {
			t.Fatalf("%s must pass the gate, collided with %q", mp, sibling)
		}
	}

	// EXEMPTION REASONING (pinned per the fix-round brief):
	//   - CHUNKS: a chunk's derived partial is chunks/<rel>/NNNNN.partial;
	//     chunk finals are fixed 5-digit names under the chunks/ root,
	//     disjoint from the files/ root every whole-file partial lives
	//     under - so a chunk media path can NEVER collide with a member
	//     final (and a files/ partial can never be a chunk name). Assert
	//     the concrete chunk of the very sibling in the colliding pair.
	if mp, err := media.ChunkMediaPath("a.partial", 3); err != nil {
		t.Fatal(err)
	} else if sibling, collided := partialPathCollision(finals, mp); collided {
		t.Fatalf("chunk %s must be structurally exempt, collided with %q", mp, sibling)
	}
	//   - MANIFEST.json / catalog-export.sqlite EMBEDS: they are
	//     TOOL-OWNED image-root members; every inventory member final
	//     sits under files/ or chunks/, never at the image root - so
	//     the embeds' derived partials (MANIFEST.json.partial,
	//     catalog-export.sqlite.partial) can never be a member final.
	//     The structural precondition: the set contains ONLY rooted
	//     payload paths.
	for mp := range finals {
		if !strings.HasPrefix(mp, media.FilesRoot+"/") && !strings.HasPrefix(mp, media.ChunksRoot+"/") {
			t.Fatalf("media path %q is not under a payload root; the image-root embed exemption premise is broken", mp)
		}
	}
}

// copyB1Fixture scans + REAL-plan-locks a two-file set whose names form
// the B1 collision pair (data.txt vs data.txt.partial) on one image.
func copyB1Fixture(t *testing.T, setID string) (cat, root, dest string) {
	t.Helper()
	root = t.TempDir()
	cat = filepath.Join(t.TempDir(), "catalog.sqlite")
	dest = filepath.Join(t.TempDir(), "dest")
	writeFileP(t, filepath.Join(root, "data.txt"), pattern(30, 333), 0o644)
	writeFileP(t, filepath.Join(root, "data.txt.partial"), pattern(31, 444), 0o644)
	if _, stderr, code := runScanChildForCopy(t, "--catalog", cat, "--set-id", setID, root); code != 0 {
		t.Fatalf("scan exit = %d (stderr: %s)", code, stderr)
	}
	planSynthetic(t, cat, setID, copySyntheticUsable)
	return cat, root, dest
}

// THE B1 E2E (the decisive test): the real copy command over the
// colliding pair - the survivor's data must survive BYTE-IDENTICAL
// (sha256), the colliding item must be warned + reported skipped +
// exit 10, and a re-run must stay stable (no data loss across runs).
//
// ORDER-INDEPENDENCE, and exactly what is asserted: the gate is a
// purely structural name check against the COMPLETE inventory, so the
// outcome does not depend on the hash-pool file_id order at all - the
// item whose derived partial lands on its sibling's final (data.txt,
// partial files/data.txt.partial) is ALWAYS the skipped one, and the
// sibling (data.txt.partial, partial files/data.txt.partial.partial,
// which collides with nothing) ALWAYS proceeds. The harness cannot
// force file_id order, so instead of pretending to control it, the
// test runs TWO fresh fixtures (a fresh scan re-rolls the order; two
// rolls sample both orders ~75% of the time) and asserts the SAME
// deterministic outcome both times - the guarantee the gate actually
// makes.
func TestCopyPartialCollisionSkipsInsteadOfDeletingSibling(t *testing.T) {
	for i := 0; i < 2; i++ {
		t.Run(fmt.Sprintf("fixture-%d", i), func(t *testing.T) {
			setID := fmt.Sprintf("S-B1-%d", i)
			cat, _, dest := copyB1Fixture(t, setID)

			stdout, stderr, code := runCopyChild(t, nil,
				"copy", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json")
			if code != 10 {
				t.Fatalf("copy exit = %d, want 10 (partial-failure class; stderr: %s)\nstdout: %s", code, stderr, stdout)
			}
			// The warning names BOTH paths and the sibling relpath.
			wantWarn := "files/data.txt: its temporary path files/data.txt.partial " +
				"collides with the media file of source 'data.txt.partial' on this image"
			if !strings.Contains(stderr, wantWarn) {
				t.Fatalf("stderr lacks the collision warning %q:\n%s", wantWarn, stderr)
			}
			var rep copyReport
			if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
				t.Fatal(err)
			}
			if rep.Skipped != 1 || rep.Copied != 1 || rep.Failed != 0 || rep.Status != "partial" {
				t.Fatalf("report = %+v, want 1 skip (data.txt) + 1 copy (data.txt.partial)", rep)
			}
			if len(rep.Failures) != 1 || !strings.Contains(rep.Failures[0], "data.txt") ||
				!strings.Contains(rep.Failures[0], "skipped") {
				t.Fatalf("failures = %v, want the data.txt collision skip", rep.Failures)
			}

			// THE SURVIVOR'S BYTES: byte-identical on the tree (sha256),
			// and the gated item never reached media.
			imgRoot := destImageDir(dest, setID, 1)
			survivor := filepath.Join(imgRoot, "files", "data.txt.partial")
			assertBytes(t, survivor, pattern(31, 444))
			if gotSum, wantSum := copySha256Hex(mustRead(t, survivor)), copySha256Hex(pattern(31, 444)); gotSum != wantSum {
				t.Fatalf("survivor sha256 = %s, want %s", gotSum, wantSum)
			}
			if _, err := os.Lstat(filepath.Join(imgRoot, "files", "data.txt")); !os.IsNotExist(err) {
				t.Fatalf("the gated item wrote files/data.txt anyway (err=%v)", err)
			}

			// THE CATALOG: survivor copied=1 + its content row intact;
			// the gated item copied=0 with NO content row.
			db := catOpen(t, cat)
			for rel, wantCopied := range map[string]bool{"data.txt.partial": true, "data.txt": false} {
				var copied bool
				if err := db.QueryRow(`SELECT copied FROM files WHERE set_id = ? AND relpath = ?`,
					setID, rel).Scan(&copied); err != nil {
					t.Fatal(err)
				}
				if copied != wantCopied {
					t.Fatalf("copied flag of %s = %v, want %v", rel, copied, wantCopied)
				}
			}
			for mediaPath, wantRows := range map[string]int{"files/data.txt.partial": 1, "files/data.txt": 0} {
				var rows int
				if err := db.QueryRow(`SELECT COUNT(*) FROM image_contents WHERE set_id = ? AND media_path = ?`,
					setID, mediaPath).Scan(&rows); err != nil {
					t.Fatal(err)
				}
				if rows != wantRows {
					t.Fatalf("content rows for %s = %d, want %d", mediaPath, rows, wantRows)
				}
			}

			// THE RE-RUN (stability): the collision is structural, so the
			// skip is stable across runs - the survivor's data survives
			// every additional run (no data loss), the gated item stays
			// skipped, and the resume trust model already-counts the
			// survivor.
			stdout2, stderr2, code2 := runCopyChild(t, nil,
				"copy", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json")
			if code2 != 10 {
				t.Fatalf("re-run exit = %d, want 10 (the structural skip persists; stderr: %s)", code2, stderr2)
			}
			var rep2 copyReport
			if err := json.Unmarshal([]byte(stdout2), &rep2); err != nil {
				t.Fatal(err)
			}
			if rep2.Skipped != 1 || rep2.Copied != 0 || rep2.AlreadyCopied != 1 || rep2.Status != "partial" {
				t.Fatalf("re-run report = %+v, want the same skip + 1 already present, nothing copied", rep2)
			}
			assertBytes(t, survivor, pattern(31, 444)) // still byte-identical
			if _, err := os.Lstat(filepath.Join(imgRoot, "files", "data.txt")); !os.IsNotExist(err) {
				t.Fatalf("the re-run resurrected the gated item anyway (err=%v)", err)
			}
		})
	}
}

// --- NB1 (fix round): the export-failure path finalizes the run row ---------

// A run-scoped export production failure (here: --work-dir naming a
// directory that cannot host the scratch dir) used to return WITHOUT
// r.finalize, stranding the runs row 'running' forever - contradicting
// the file doc's "every exit path finalizes it". It now finalizes
// 'failed' (exit 1, the fatal class) and still prints the honest
// (empty) report on stdout (FR-046).
func TestCopyExportFailureFinalizesRunFailed(t *testing.T) {
	f := newCopyCrashFixture(t)
	missingWorkDir := filepath.Join(t.TempDir(), "does-not-exist")
	stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", f.cat, "--set-id", f.setID, "--dest", f.dest,
		"--work-dir", missingWorkDir, "--json")
	if code != 1 {
		t.Fatalf("export-failure exit = %d, want 1 (fatal class; stderr: %s)\nstdout: %s", code, stderr, stdout)
	}
	if !strings.Contains(stderr, "export scratch directory") {
		t.Fatalf("stderr lacks the export failure cause: %s", stderr)
	}
	// THE NB1 ASSERTION: the run row is FINALIZED 'failed' - not the
	// 'running' debris the pre-fix bare return stranded.
	db := catOpen(t, f.cat)
	var status string
	if err := db.QueryRow(`SELECT status FROM runs WHERE kind = 'copy'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "failed" {
		t.Fatalf("copy run status = %q, want failed (NB1: the export failure must finalize the row)", status)
	}
	// The report still lands on stdout with the failed status (FR-046).
	var rep copyReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("stdout is not a JSON report: %v\n%s", err, stdout)
	}
	if rep.Status != "failed" {
		t.Fatalf("report status = %q, want failed", rep.Status)
	}
	// Nothing was written or marked: the failure precedes every image.
	for rel, copied := range copiedFlags(t, f.cat, f.setID) {
		if copied {
			t.Fatalf("item %s marked despite the export failure", rel)
		}
	}
	partials, err := filepath.Glob(filepath.Join(f.dest, "*", "files", "*.partial"))
	if err != nil {
		t.Fatal(err)
	}
	if len(partials) != 0 {
		t.Fatalf("partials survived the export failure: %v", partials)
	}
}

// --- NB2 (fix round): the env seam is validated BEFORE any run row ----------

// A malformed ISOPRESS_TEST_COPY_FAIL_* value is a usage-class
// input: it exits 2 with the actionable validation message BEFORE any
// database work - specifically before StartCopyRun - so it can never
// strand a 'running' runs row (the debris the pre-fix placement,
// after StartCopyRun, left behind).
func TestCopyMalformedHookEnvLeavesNoRunRow(t *testing.T) {
	f := newCopyCrashFixture(t)
	stdout, stderr, code := runCopyChild(t, []string{
		"ISOPRESS_TEST_COPY_FAIL_AFTER=not-a-number",
	}, "copy", "--catalog", f.cat, "--set-id", f.setID, "--dest", f.dest)
	if code != 2 {
		t.Fatalf("malformed seam env exit = %d, want 2 (usage class; stderr: %s)\nstdout: %s", code, stderr, stdout)
	}
	if !strings.Contains(stderr, "not a positive integer") {
		t.Fatalf("stderr lacks the seam validation message: %s", stderr)
	}
	// THE NB2 ASSERTION: no copy runs row was minted at all.
	db := catOpen(t, f.cat)
	var copyRuns int
	if err := db.QueryRow(`SELECT COUNT(*) FROM runs WHERE kind = 'copy'`).Scan(&copyRuns); err != nil {
		t.Fatal(err)
	}
	if copyRuns != 0 {
		t.Fatalf("%d copy run rows exist despite the malformed env; the seam validation must precede StartCopyRun", copyRuns)
	}
}
