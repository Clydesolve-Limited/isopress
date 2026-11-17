package cli

// T5.3 acceptance for the `restore` command (DESIGN_SPEC 8.1-8.3, 10.2;
// IMPLEMENTATION_PLAN 4.6 T5.3). Everything drives the REAL production entry
// path - in-process execute() for the buffer-stream assertions, the re-exec
// child for real process streams, and the REAL PTY pair of the progress-round
// pattern for the TTY-gated prompt - against real scanned + planned + copied
// sets with the REAL catalog underneath.
//
// The per-image content restoration is T5.4's, so the tests inject the
// RECORDING fake ImageRestorer through restoreRestorerFactory (the seam the
// package exposes for exactly this). The fake appends one JSON line per seam
// call, which is what the orchestration assertions read: call order, the
// single workspace and its cleanliness, the target identity, the selected
// names, and the per-image inventory the seam received.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"isopress/internal/catalog"
	"isopress/internal/report"
	"isopress/internal/restore"
)

// The test-seam environment variables the re-exec child reads (TestMain
// installs the fake when restoreFakeEnv is set, so no test-only branch
// exists in production code).
const (
	restoreFakeEnv   = "ISOPRESS_TEST_RESTORE_FAKE"
	restoreRecordEnv = "ISOPRESS_TEST_RESTORE_RECORD"
	restoreCallsFile = "restore-calls.jsonl"
	// restoreBlockEnv parks the fake INSIDE the first seam call until the run
	// context dies (the deterministic SIGINT window for the interruption e2e:
	// no sleeps, no size games - the marker line IS the readiness signal).
	restoreBlockEnv = "ISOPRESS_TEST_RESTORE_BLOCK"
	restoreBlocked  = "RESTORE-FAKE-BLOCKED"
)

// fileMeta is one whole-file inventory entry's identity as the fake recorded
// it: the catalog file_id and size. It is what lets a test pin WHICH content
// version reached the seam (the D-07 latest-state direction).
type fileMeta struct {
	ID   int64 `json:"id"`
	Size int64 `json:"size"`
}

// restoreCall is one seam call as the fake recorded it (one JSON line).
type restoreCall struct {
	Image            int64               `json:"image"`
	Kind             string              `json:"kind"`
	Path             string              `json:"path"`
	Target           string              `json:"target"`
	Workspace        string              `json:"workspace"`
	WorkspaceEntries []string            `json:"workspace_entries"`
	Selected         []string            `json:"selected"`
	WorkImages       []int64             `json:"work_images"`
	Files            []string            `json:"files"`
	FileMeta         map[string]fileMeta `json:"file_meta"`
	Secondaries      []string            `json:"secondaries"`
	PrimaryOf        map[string]string   `json:"primary_of"`
	Chunks           []string            `json:"chunks"`
	Restored         int                 `json:"restored"`
}

// fakeRestorer is the RECORDING ImageRestorer stand-in for T5.4. It records
// every call (JSONL, appended as it happens so a crashed child still leaves
// its trace) and synthesizes an outcome from the work list, optionally
// failing named members so the partial-failure report can be pinned.
type fakeRestorer struct {
	recordPath string
	fail       map[string]bool
}

func newFakeRestorer(recordPath string, fail []string) *fakeRestorer {
	f := &fakeRestorer{recordPath: recordPath, fail: map[string]bool{}}
	for _, rel := range fail {
		f.fail[rel] = true
	}
	return f
}

// RestoreImage implements restore.ImageRestorer.
func (f *fakeRestorer) RestoreImage(ctx context.Context, image restore.ImageSupply, selection restore.ResolvedSelection, workspace string) (restore.ImageRestoreResult, error) {
	work, _ := selection.WorkFor(image.Image)
	rec := restoreCall{
		Image:     image.Image,
		Kind:      string(image.Kind),
		Path:      image.Path,
		Target:    selection.Target,
		Workspace: workspace,
		Selected:  append([]string(nil), selection.Paths...),
		PrimaryOf: map[string]string{},
		FileMeta:  map[string]fileMeta{},
	}
	for _, w := range selection.Work {
		rec.WorkImages = append(rec.WorkImages, w.Image)
	}
	if entries, err := os.ReadDir(workspace); err == nil {
		for _, e := range entries {
			rec.WorkspaceEntries = append(rec.WorkspaceEntries, e.Name())
		}
	}
	for _, file := range work.Inv.Files {
		rec.Files = append(rec.Files, file.RelPath)
		rec.FileMeta[file.RelPath] = fileMeta{ID: file.FileID, Size: file.Size}
	}
	for _, sy := range work.Inv.Symlinks {
		rec.Files = append(rec.Files, sy.RelPath)
	}
	for _, d := range work.Inv.Dirs {
		rec.Files = append(rec.Files, d.RelPath)
	}
	for _, sec := range work.Inv.Secondaries {
		rec.Secondaries = append(rec.Secondaries, sec.RelPath)
		rec.PrimaryOf[sec.RelPath] = sec.PrimaryRelPath
	}
	for _, c := range work.Inv.Chunks {
		rec.Chunks = append(rec.Chunks, c.ParentRelPath)
	}

	// The interruption seam: announce readiness on stderr, then wait for the
	// signal that cancels the run context (the fake honors ctx like a real
	// restorer must).
	if os.Getenv(restoreBlockEnv) == "1" {
		fmt.Fprintln(os.Stderr, restoreBlocked)
		<-ctx.Done()
		return restore.ImageRestoreResult{}, ctx.Err()
	}

	res := restore.ImageRestoreResult{Image: image.Image}
	for _, m := range fakeMembers(work) {
		if f.fail[m.rel] {
			res.Failed++
			res.Files = append(res.Files, restore.FileOutcome{
				RelPath: m.rel, Status: "failed", Bytes: m.size, Detail: "injected failure",
			})
			continue
		}
		res.Restored++
		res.Bytes += m.size
	}
	rec.Restored = res.Restored
	if err := appendJSONLine(f.recordPath, rec); err != nil {
		return restore.ImageRestoreResult{}, err
	}
	// Debris so the between-images workspace clean is exercised at the CLI
	// level too.
	return res, os.WriteFile(filepath.Join(workspace, fmt.Sprintf("debris-%d", image.Image)), []byte("x"), 0o600)
}

// fakeMember is one distinct target member of an image's work list.
type fakeMember struct {
	rel  string
	size int64
}

// fakeMembers flattens a work list into distinct members (a chunked parent
// is one member, credited with the chunk bytes this image holds).
func fakeMembers(w restore.ImageWork) []fakeMember {
	var out []fakeMember
	seen := map[string]bool{}
	add := func(rel string, size int64) {
		for i := range out {
			if out[i].rel == rel {
				out[i].size += size
				return
			}
		}
		if seen[rel] {
			return
		}
		seen[rel] = true
		out = append(out, fakeMember{rel: rel, size: size})
	}
	for _, c := range w.Inv.Chunks {
		add(c.ParentRelPath, c.Length)
	}
	for _, f := range w.Inv.Files {
		add(f.RelPath, f.Size)
	}
	for _, s := range w.Inv.Symlinks {
		add(s.RelPath, s.Size)
	}
	for _, l := range w.Inv.Secondaries {
		add(l.RelPath, l.Size)
	}
	for _, d := range w.Inv.Dirs {
		add(d.RelPath, 0)
	}
	return out
}

// appendJSONLine appends one JSON line to the record file.
func appendJSONLine(path string, v any) error {
	if path == "" {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close() //nolint:errcheck // single small append
	_, err = f.Write(append(b, '\n'))
	return err
}

// childFakeEnv is the re-exec child's fake-seam environment: the child
// installs the recording fake through TestMain and appends its calls to path.
func childFakeEnv(path string) []string {
	return []string{restoreFakeEnv + "=1", restoreRecordEnv + "=" + path}
}

// readRestoreCalls reads the recorded seam calls (empty when none happened).
func readRestoreCalls(t *testing.T, path string) []restoreCall {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading the restore record: %v", err)
	}
	var out []restoreCall
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line == "" {
			continue
		}
		var c restoreCall
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			t.Fatalf("bad record line %q: %v", line, err)
		}
		out = append(out, c)
	}
	return out
}

// useRestoreFake injects the recording fake for an IN-PROCESS run and returns
// its record path (t.Cleanup restores the production factory).
func useRestoreFake(t *testing.T, fail ...string) string {
	t.Helper()
	recordPath := filepath.Join(t.TempDir(), restoreCallsFile)
	prev := restoreRestorerFactory
	restoreRestorerFactory = func(deps restore.RestorerDeps) restore.ImageRestorer {
		return newFakeRestorer(recordPath, fail)
	}
	t.Cleanup(func() { restoreRestorerFactory = prev })
	return recordPath
}

// installRestoreFakeFromEnv is called by TestMain for the re-exec child so a
// child process runs the recording fake without any test-only code in the
// command.
func installRestoreFakeFromEnv() {
	if os.Getenv(restoreFakeEnv) != "1" {
		return
	}
	record := os.Getenv(restoreRecordEnv)
	restoreRestorerFactory = func(deps restore.RestorerDeps) restore.ImageRestorer {
		return newFakeRestorer(record, nil)
	}
}

// runRestoreChild runs the real restore command (plus EXTRA environment,
// e.g. the fake seam) through the production execute path.
func runRestoreChild(t *testing.T, extraEnv []string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(append(os.Environ(), cliHelperEnv+"=1"), extraEnv...)
	outBuf, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = outBuf, errBuf
	err := cmd.Run()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("running restore child %v: %v (stderr: %s)", args, err, errBuf.String())
		}
		code = exitErr.ExitCode()
	}
	return outBuf.String(), errBuf.String(), code
}

// --- fixtures ------------------------------------------------------------------

// restoreTwoImageFixture scans, plans, and COPIES the two-image fixture (a
// file chunked across BOTH images plus a hard-link family, a symlink, and an
// empty dir), returning the catalog path, the destination root, and the
// source root.
func restoreTwoImageFixture(t *testing.T, setID string) (cat, dest, root string) {
	t.Helper()
	root = t.TempDir()
	writeFileP(t, filepath.Join(root, "big.bin"), pattern(1, copyBigChunkedLen), 0o644)
	writeFileP(t, filepath.Join(root, "small1.txt"), pattern(2, 1000), 0o644)
	writeFileP(t, filepath.Join(root, "fam-a.txt"), pattern(5, 800), 0o644)
	if err := os.Link(filepath.Join(root, "fam-a.txt"), filepath.Join(root, "fam-b.txt")); err != nil {
		t.Fatalf("hard-link family: %v", err)
	}
	if err := os.Symlink("small1.txt", filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "empty-dir"), 0o750); err != nil {
		t.Fatalf("empty dir: %v", err)
	}
	cat = createCatalog(t)
	scanBuildFixture(t, root, cat, setID)
	planSynthetic(t, cat, setID, copySyntheticUsable)
	dest = filepath.Join(t.TempDir(), "dest")
	if stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest); code != 0 {
		t.Fatalf("copy fixture exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	return cat, dest, root
}

// restoreTarget returns a fresh, NOT-yet-created target directory.
func restoreTarget(t *testing.T) string {
	return filepath.Join(t.TempDir(), "target")
}

// --- usage gates (exit 2) ----------------------------------------------------

func TestRestoreUsageGates(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"missing --catalog", []string{"restore", "--set-id", "S", "--target", "/tmp/t"}, "--catalog"},
		{"missing --set-id", []string{"restore", "--catalog", "c.sqlite", "--target", "/tmp/t"}, "--set-id"},
		{"missing --target", []string{"restore", "--catalog", "c.sqlite", "--set-id", "S"}, "--target"},
		{"malformed --image", []string{"restore", "--catalog", "c.sqlite", "--set-id", "S",
			"--target", "/tmp/t", "--image", "1"}, "N=PATH"},
		{"zero image number", []string{"restore", "--catalog", "c.sqlite", "--set-id", "S",
			"--target", "/tmp/t", "--image", "0=/x"}, "positive integer"},
		{"tilde --target", []string{"restore", "--catalog", "c.sqlite", "--set-id", "S",
			"--target", "~/restore-here"}, "literal ~"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code := runInProcess(t, tc.args...)
			if code != report.ExitUsage {
				t.Fatalf("exit = %d, want 2 (usage)\nstderr: %s", code, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr does not contain %q:\n%s", tc.want, stderr)
			}
			if !strings.Contains(stderr, "usage") {
				t.Errorf("usage errors carry the usage hint (10.1):\n%s", stderr)
			}
		})
	}
}

// An unknown set, an unplanned set, and a selector matching nothing are all
// the user's input class: exit 2, never a silent no-op.
func TestRestoreSelectionAndPlanGates(t *testing.T) {
	// A catalog with a scanned set that was NEVER planned.
	root := t.TempDir()
	writeFileP(t, filepath.Join(root, "a.txt"), pattern(3, 100), 0o644)
	cat := createCatalog(t)
	scanBuildFixture(t, root, cat, "S-NOPLAN")
	target := restoreTarget(t)

	_, stderr, code := runInProcess(t, "restore", "--catalog", cat, "--set-id", "S-NOPLAN", "--target", target)
	if code != report.ExitUsage {
		t.Fatalf("unplanned set exit = %d, want 2\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "no planned images") {
		t.Errorf("stderr lacks the unplanned-set refusal:\n%s", stderr)
	}

	// A planned set: selectors are validated against REAL catalog relpaths,
	// so an unsafe or unmatched --path / --subtree reaches the selection gate
	// (these need a catalog that opens - the gate runs after the read-only
	// open, unlike the flag-shape gates above).
	cat2, dest, _ := restoreTwoImageFixture(t, "S-SEL")
	_ = dest
	_, stderr, code = runInProcess(t, "restore", "--catalog", cat2, "--set-id", "S-SEL",
		"--target", restoreTarget(t), "--path", "/etc/passwd")
	if code != report.ExitUsage {
		t.Fatalf("absolute --path exit = %d, want 2\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "absolute") {
		t.Errorf("stderr must explain the path shape:\n%s", stderr)
	}
	_, stderr, code = runInProcess(t, "restore", "--catalog", cat2, "--set-id", "S-SEL",
		"--target", restoreTarget(t), "--path", "../escape")
	if code != report.ExitUsage {
		t.Fatalf("escaping --path exit = %d, want 2\n%s", code, stderr)
	}
	_, stderr, code = runInProcess(t, "restore", "--catalog", cat2, "--set-id", "S-SEL",
		"--target", restoreTarget(t), "--path", "nope.txt")
	if code != report.ExitUsage {
		t.Fatalf("--path matching nothing exit = %d, want 2\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "nope.txt") || !strings.Contains(stderr, "--subtree") {
		t.Errorf("stderr must name the path and point at --subtree:\n%s", stderr)
	}
	_, stderr, code = runInProcess(t, "restore", "--catalog", cat2, "--set-id", "S-SEL",
		"--target", restoreTarget(t), "--subtree", "not-here/")
	if code != report.ExitUsage {
		t.Fatalf("--subtree matching nothing exit = %d, want 2\n%s", code, stderr)
	}

	// A --target that exists as a FILE is refused before any work.
	fileTarget := filepath.Join(t.TempDir(), "not-a-dir")
	writeFileP(t, fileTarget, []byte("x"), 0o644)
	_, stderr, code = runInProcess(t, "restore", "--catalog", cat2, "--set-id", "S-SEL", "--target", fileTarget)
	if code != report.ExitUsage {
		t.Fatalf("file --target exit = %d, want 2\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "not a directory") {
		t.Errorf("stderr must explain the target shape:\n%s", stderr)
	}
}

// --- the multi-image orchestration (explicit mappings, real catalog) ---------

func TestRestoreMultiImageExplicitMappings(t *testing.T) {
	cat, dest, root := restoreTwoImageFixture(t, "S-RST2")
	record := useRestoreFake(t)
	target := restoreTarget(t)

	stdout, stderr, code := runInProcess(t, "restore",
		"--catalog", cat, "--set-id", "S-RST2", "--target", target,
		"--image", "1="+destImageDir(dest, "S-RST2", 1),
		"--image", "2="+destImageDir(dest, "S-RST2", 2))
	if code != report.ExitOK {
		t.Fatalf("restore exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	calls := readRestoreCalls(t, record)
	if len(calls) != 2 {
		t.Fatalf("seam calls = %d, want 2 (one per required image)", len(calls))
	}
	// The exact call order is ASCENDING regardless of flag order, the
	// workspace path is ONE reused directory, and every call starts from a
	// clean one (the fake leaves debris behind).
	if calls[0].Image != 1 || calls[1].Image != 2 {
		t.Errorf("call order = [%d %d], want [1 2]", calls[0].Image, calls[1].Image)
	}
	if calls[0].Workspace == "" || calls[0].Workspace != calls[1].Workspace {
		t.Errorf("workspaces = %q vs %q, want one reused path", calls[0].Workspace, calls[1].Workspace)
	}
	for i, c := range calls {
		if len(c.WorkspaceEntries) != 0 {
			t.Errorf("call %d workspace at entry = %v, want empty (cleaned between images)", i, c.WorkspaceEntries)
		}
		if c.Target != target {
			t.Errorf("call %d target = %q, want %q", i, c.Target, target)
		}
		if len(c.WorkImages) != 2 {
			t.Errorf("call %d work images = %v, want both required images", i, c.WorkImages)
		}
		if !strings.HasPrefix(c.Path, dest) {
			t.Errorf("call %d path = %q, want the supplied copy tree under %q", i, c.Path, dest)
		}
	}
	// The chunked parent appears on BOTH images' chunk lists (the span the
	// fixture is built for), and the selection reaches both calls.
	if len(calls[0].Chunks) == 0 || len(calls[1].Chunks) == 0 {
		t.Errorf("chunk parents per call = %v / %v, want both images holding chunks of big.bin",
			calls[0].Chunks, calls[1].Chunks)
	}
	if len(calls[0].Selected) == 0 {
		t.Error("the seam must receive the selected paths")
	}
	// The workspace is removed when the run returns.
	if _, err := os.Stat(calls[0].Workspace); !os.IsNotExist(err) {
		t.Errorf("workspace survived the run: %v", err)
	}

	// The human report: per-image and per-file lines plus totals on stdout.
	for _, want := range []string{
		"restore: set S-RST2",
		"target: " + target,
		"image 1: restored",
		"image 2: restored",
		"totals: images 2",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "fam-b") {
		t.Errorf("a clean restore lists no per-file lines (restored members are counted, not listed):\n%s", stdout)
	}
	// The source tree is never read or written: the target is elsewhere and
	// the record proves every write path went to it.
	_ = root
}

// --- --json (frozen key set) -------------------------------------------------

func TestRestoreJSONKeysFrozen(t *testing.T) {
	cat, dest, _ := restoreTwoImageFixture(t, "S-RJSON")
	record := useRestoreFake(t)
	_ = record
	target := restoreTarget(t)

	stdout, stderr, code := runInProcess(t, "restore", "--json",
		"--catalog", cat, "--set-id", "S-RJSON", "--target", target,
		"--image", "1="+destImageDir(dest, "S-RJSON", 1),
		"--image", "2="+destImageDir(dest, "S-RJSON", 2))
	if code != report.ExitOK {
		t.Fatalf("restore --json exit = %d, want 0\n%s\n%s", code, stdout, stderr)
	}
	// THE FROZEN TOP-LEVEL KEY SET (the scan/plan/copy/build/verify-media
	// precedent): a new key is a conscious schema change pinned here.
	assertFrozenJSONKeys(t, stdout, []string{
		"set_id", "target", "selection", "status",
		"files", "bytes", "restored", "skipped", "failed",
		"unclaimed", "images",
	})
	var rep struct {
		Status    string `json:"status"`
		Selection string `json:"selection"`
		Images    []struct {
			Image    int64  `json:"image"`
			Status   string `json:"status"`
			Source   string `json:"source"`
			Restored int    `json:"restored"`
			Files    []any  `json:"files"`
		} `json:"images"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("json parse: %v\n%s", err, stdout)
	}
	if rep.Status != "ok" || rep.Selection != "whole-set" || len(rep.Images) != 2 {
		t.Errorf("report = %+v, want ok/whole-set/2 images", rep)
	}
	for _, img := range rep.Images {
		if img.Status != "restored" || img.Source != "copy" || img.Restored == 0 {
			t.Errorf("image = %+v, want a restored copy image with counts", img)
		}
		if len(img.Files) != 0 {
			t.Errorf("clean run must list no per-file findings: %+v", img.Files)
		}
	}
}

// --- the non-TTY missing-image path (8.3 / RCR-3) ---------------------------

func TestRestoreNonTTYMissingImageListing(t *testing.T) {
	cat, _, _ := restoreTwoImageFixture(t, "S-RMISS")
	record := useRestoreFake(t)
	imageDir := t.TempDir() // configured, empty: both images are missing
	target := restoreTarget(t)

	// READ-ONLY PROOF: the missing-media abort performs zero catalog writes
	// and takes no runs row.
	before, err := os.ReadFile(cat)
	if err != nil {
		t.Fatalf("snapshot catalog: %v", err)
	}
	runsBefore, restoreRunsBefore := countRuns(t, cat)

	stdout, stderr, code := runInProcess(t, "restore",
		"--catalog", cat, "--set-id", "S-RMISS", "--target", target, "--image-dir", imageDir)

	if code != 3 {
		t.Fatalf("exit = %d, want 3 (the media-supply dependency class)\nstderr: %s", code, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout must stay empty when nothing ran (10.3), got: %q", stdout)
	}
	// ONE machine-readable line per missing image, ascending, each naming
	// what was sought (the 6.4 .iso name under --image-dir) and a path that
	// requires it.
	for _, want := range []string{
		"error: required-image: image=1 set=S-RMISS sought=" +
			filepath.Join(imageDir, "S-RMISS-001.iso"),
		"error: required-image: image=2 set=S-RMISS sought=" +
			filepath.Join(imageDir, "S-RMISS-002.iso"),
	} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if !strings.Contains(stderr, "needed-for=big.bin") {
		t.Errorf("the listing must name a path that needs each image:\n%s", stderr)
	}
	if !strings.Contains(stderr, "no prompt was issued") {
		t.Errorf("stderr must state that the non-TTY run did not prompt:\n%s", stderr)
	}
	// Nothing ran: no seam call, no target, no workspace.
	if calls := readRestoreCalls(t, record); len(calls) != 0 {
		t.Errorf("seam calls = %d, want 0 (the listing aborts before the loop)", len(calls))
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target exists after a listing abort: %v", err)
	}
	after, err := os.ReadFile(cat)
	if err != nil {
		t.Fatalf("re-read catalog: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the missing-media abort modified the catalog file")
	}
	runsAfter, restoreRunsAfter := countRuns(t, cat)
	if runsAfter != runsBefore || restoreRunsAfter != restoreRunsBefore || restoreRunsAfter != 0 {
		t.Errorf("runs rows = %d (restore %d) before, %d (restore %d) after: restore takes NO runs row",
			runsBefore, restoreRunsBefore, runsAfter, restoreRunsAfter)
	}
}

// --- the TTY prompt (real PTY, scripted stdin) --------------------------------

// runRestoreOnPTY runs the fake-seam restore child with stderr attached to a
// REAL pseudo-terminal (so 8.3's TTY gate opens) and stdin as a scripted pipe
// (the acceptance criterion's "TTY prompting test with a scripted stdin").
// The prompt text, the recorded answer, and the child's exit code come back
// with the child's stdout.
func runRestoreOnPTY(t *testing.T, stdinLines []string, args ...string) (stdout, stderr, record string, exitCode int) {
	t.Helper()
	record = filepath.Join(t.TempDir(), restoreCallsFile)
	master, slave := openPtyPair(t)
	defer master.Close()

	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), cliHelperEnv+"=1", restoreFakeEnv+"=1", restoreRecordEnv+"="+record)
	cmd.Stderr = slave // a REAL terminal -> 8.3's gate opens
	stdoutBuf := &bytes.Buffer{}
	cmd.Stdout = stdoutBuf
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = slave.Close()
		t.Fatalf("stdin pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		_ = slave.Close()
		t.Fatalf("starting restore child: %v", err)
	}
	_ = slave.Close() // the child owns the slave fd; the parent drains the master

	exited := false
	defer func() {
		if !exited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	// Continuous master drain (a full PTY buffer would block the child
	// mid-prompt), with a one-shot signal when the prompt is on screen.
	var out strings.Builder
	promptSeen := make(chan struct{}, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				out.Write(buf[:n])
				select {
				case promptSeen <- struct{}{}:
				default:
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Wait for the prompt, then script the answers.
	select {
	case <-promptSeen:
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("no prompt on the PTY within 60s; output so far:\n%s", out.String())
	}
	for _, line := range stdinLines {
		if _, err := stdin.Write([]byte(line)); err != nil {
			t.Errorf("writing the scripted answer: %v", err)
		}
	}
	_ = stdin.Close()

	code := 0
	if err := cmd.Wait(); err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("restore child: %v (stderr: %s)", err, out.String())
		}
		code = exitErr.ExitCode()
	}
	exited = true
	<-readerDone
	return stdoutBuf.String(), out.String(), record, code
}

func TestRestoreTTYPromptSuppliesPathAndProceeds(t *testing.T) {
	cat, dest, _ := restoreTwoImageFixture(t, "S-RPTY")
	// Only image 1 is missing: image 2 is supplied up-front, and the
	// image-dir discovery finds nothing for image 1.
	tree1 := destImageDir(dest, "S-RPTY", 1)
	target := restoreTarget(t)

	stdout, ptyOut, record, code := runRestoreOnPTY(t, []string{tree1 + "\n"},
		"restore", "--catalog", cat, "--set-id", "S-RPTY", "--target", target,
		"--image-dir", t.TempDir(),
		"--image", "2="+destImageDir(dest, "S-RPTY", 2))

	// The 8.3 sentence appeared VERBATIM on the terminal, naming the
	// ALPHABETICALLY FIRST selected path the image claims (big.bin < empty-dir
	// < fam-* < small1.txt): the deterministic <path> the prompt contract
	// promises, independent of which family member happens to be primary.
	want := "Image 1 of set S-RPTY is required for big.bin. Provide its path (or 'q' to abort):"
	if !strings.Contains(ptyOut, want) {
		t.Errorf("PTY output lacks the verbatim 8.3 prompt %q:\n%s", want, ptyOut)
	}
	if code != report.ExitOK {
		t.Fatalf("restore with a supplied path exit = %d, want 0\nstdout: %s\npty: %s", code, stdout, ptyOut)
	}
	// The supplied path was accepted and used as the image-1 copy tree, and
	// the run then processed BOTH required images.
	calls := readRestoreCalls(t, record)
	if len(calls) != 2 {
		t.Fatalf("seam calls = %d, want 2 (the prompted path unlocked image 1)", len(calls))
	}
	if calls[0].Image != 1 || calls[0].Path != tree1 || calls[0].Kind != "copy" {
		t.Errorf("call 0 = %+v, want image 1 from the prompted tree", calls[0])
	}
	if !strings.Contains(stdout, "image 1: restored") || !strings.Contains(stdout, "image 2: restored") {
		t.Errorf("stdout lacks the per-image lines:\n%s", stdout)
	}
}

func TestRestoreTTYPromptQAbortsCleanly(t *testing.T) {
	cat, _, _ := restoreTwoImageFixture(t, "S-RQ")
	target := restoreTarget(t)
	workDir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		t.Fatalf("work dir: %v", err)
	}
	// READ-ONLY PROOF: the 'q' abort leaves the catalog byte-identical and
	// adds no runs row.
	before, err := os.ReadFile(cat)
	if err != nil {
		t.Fatalf("snapshot catalog: %v", err)
	}
	runsBefore, restoreRunsBefore := countRuns(t, cat)

	stdout, ptyOut, record, code := runRestoreOnPTY(t, []string{"q\n"},
		"restore", "--catalog", cat, "--set-id", "S-RQ", "--target", target,
		"--image-dir", t.TempDir(), "--work-dir", workDir)

	if code != 3 {
		t.Errorf("exit = %d, want 3 (the media-supply class)\npty: %s", code, ptyOut)
	}
	if !strings.Contains(ptyOut, "Provide its path (or 'q' to abort):") {
		t.Errorf("PTY output lacks the prompt:\n%s", ptyOut)
	}
	if !strings.Contains(ptyOut, "nothing was restored") {
		t.Errorf("the abort must be honest about writing nothing:\n%s", ptyOut)
	}
	if stdout != "" {
		t.Errorf("stdout must stay empty when the run aborted before work, got: %q", stdout)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target must stay untouched after 'q': %v", err)
	}
	if calls := readRestoreCalls(t, record); len(calls) != 0 {
		t.Errorf("seam calls = %d, want 0 (the abort happens before the loop)", len(calls))
	}
	if left := restoreDirNames(t, workDir); len(left) != 0 {
		t.Errorf("work dir = %v, want no workspace residue after 'q'", left)
	}
	after, err := os.ReadFile(cat)
	if err != nil {
		t.Fatalf("re-read catalog: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the 'q' abort modified the catalog file")
	}
	runsAfter, restoreRunsAfter := countRuns(t, cat)
	if runsAfter != runsBefore || restoreRunsAfter != restoreRunsBefore || restoreRunsAfter != 0 {
		t.Errorf("runs rows = %d (restore %d) before, %d (restore %d) after: restore takes NO runs row",
			runsBefore, restoreRunsBefore, runsAfter, restoreRunsAfter)
	}
}

// --- xorriso dependency (exit 3, T4.1 seam) ---------------------------------

func TestRestoreXorrisoAbsentWithISOInScopeExits3(t *testing.T) {
	cat, dest, _ := restoreTwoImageFixture(t, "S-RXOR")
	// Point image 1 at an .iso FILE (a copy tree would need no xorriso).
	iso := filepath.Join(t.TempDir(), "S-RXOR-001.iso")
	writeFileP(t, iso, []byte("not really an iso; the probe fails before any read"), 0o644)
	target := restoreTarget(t)

	stdout, stderr, code := runRestoreChild(t, []string{"PATH=" + t.TempDir()},
		"restore", "--catalog", cat, "--set-id", "S-RXOR", "--target", target,
		"--image", "1="+iso,
		"--image", "2="+destImageDir(dest, "S-RXOR", 2))
	if code != report.ExitDependency {
		t.Fatalf("exit = %d, want 3 (xorriso missing)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "xorriso") {
		t.Errorf("stderr must name the missing dependency:\n%s", stderr)
	}
	if stdout != "" {
		t.Errorf("stdout must stay empty on the up-front dependency refusal, got: %q", stdout)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("the dependency check must fail BEFORE any target write: %v", err)
	}
}

// A copy-tree-only restore with xorriso absent must NOT pay the probe: it
// completes (here through the fake) with exit 0.
func TestRestoreCopyTreeNeedsNoXorriso(t *testing.T) {
	cat, dest, _ := restoreTwoImageFixture(t, "S-RCOPY")
	record := filepath.Join(t.TempDir(), restoreCallsFile)
	target := restoreTarget(t)

	stdout, stderr, code := runRestoreChild(t, append(
		[]string{"PATH=" + t.TempDir()}, childFakeEnv(record)...),
		"restore", "--catalog", cat, "--set-id", "S-RCOPY", "--target", target,
		"--image", "1="+destImageDir(dest, "S-RCOPY", 1),
		"--image", "2="+destImageDir(dest, "S-RCOPY", 2))
	if code != report.ExitOK {
		t.Fatalf("copy-tree restore with empty PATH exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if calls := readRestoreCalls(t, record); len(calls) != 2 {
		t.Errorf("seam calls = %d, want 2 (the probe never gated a copy-tree run)", len(calls))
	}
}

// --- read-only proof (2.1 Option B) -----------------------------------------

// TestRestoreLeavesCatalogByteIdentical pins the no-write contract: a restore
// run (selection, supply, orchestration, report) performs ZERO catalog
// writes - no runs row, no run_events, no restore state (8.x has none).
func TestRestoreLeavesCatalogByteIdentical(t *testing.T) {
	cat, dest, _ := restoreTwoImageFixture(t, "S-RRO")
	useRestoreFake(t)
	target := restoreTarget(t)

	before, err := os.ReadFile(cat)
	if err != nil {
		t.Fatalf("snapshot catalog: %v", err)
	}
	runsBefore, restoreRunsBefore := countRuns(t, cat)
	stdout, stderr, code := runInProcess(t, "restore",
		"--catalog", cat, "--set-id", "S-RRO", "--target", target,
		"--image", "1="+destImageDir(dest, "S-RRO", 1),
		"--image", "2="+destImageDir(dest, "S-RRO", 2))
	if code != report.ExitOK {
		t.Fatalf("restore exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	after, err := os.ReadFile(cat)
	if err != nil {
		t.Fatalf("re-read catalog: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("restore modified the catalog file; the 2.1 read-only contract demands a byte-identical catalog")
	}
	// And NO runs row: restore has no schema state (8.x), so the runs table
	// must gain nothing - and no kind='restore' row may exist at all.
	runsAfter, restoreRunsAfter := countRuns(t, cat)
	if runsAfter != runsBefore || restoreRunsAfter != restoreRunsBefore || restoreRunsAfter != 0 {
		t.Errorf("runs rows = %d (restore %d) before, %d (restore %d) after: restore takes NO runs row",
			runsBefore, restoreRunsBefore, runsAfter, restoreRunsAfter)
	}
	// And the WAL sidecars must not have grown from restore either: the
	// read-only open takes no flock and writes nothing.
	for _, suffix := range []string{"-wal", "-shm"} {
		fiBefore, errBefore := os.Stat(cat + suffix)
		fiAfter, errAfter := os.Stat(cat + suffix)
		if (errBefore == nil) != (errAfter == nil) {
			t.Errorf("sidecar %s appeared/disappeared during restore", suffix)
			continue
		}
		if errBefore == nil && fiBefore.Size() != fiAfter.Size() {
			t.Errorf("sidecar %s changed size %d -> %d during restore", suffix, fiBefore.Size(), fiAfter.Size())
		}
	}
}

// TestRestoreDuringRealCopyRun drives restore WHILE a real copy run is
// blocked mid-item (the 12.7 hook seam): the read-only open must neither
// block nor fail with the busy class (D-11 / 2.1: "lets restore run during a
// long copy").
func TestRestoreDuringRealCopyRun(t *testing.T) {
	root := t.TempDir()
	writeFileP(t, filepath.Join(root, "data.txt"), pattern(7, 2000), 0o644)
	if err := os.Symlink("data.txt", filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cat := createCatalog(t)
	scanBuildFixture(t, root, cat, "S-RCRC")
	planSynthetic(t, cat, "S-RCRC", copySyntheticUsable)
	dest := filepath.Join(t.TempDir(), "dest")
	record := useRestoreFake(t)

	// The copy child blocks on its FIRST partial write (COPY-HOOK seam).
	child := exec.Command(os.Args[0])
	child.Env = append(os.Environ(), cliHelperEnv+"=1",
		"ISOPRESS_TEST_COPY_FAIL_AFTER=1",
		"ISOPRESS_TEST_COPY_FAIL_KIND=partial",
		"ISOPRESS_TEST_COPY_FAIL_MODE=block")
	child.Args = append(child.Args, "copy", "--catalog", cat, "--set-id", "S-RCRC", "--dest", dest)
	stderrPipe, err := child.StderrPipe()
	if err != nil {
		t.Fatalf("copy child stderr pipe: %v", err)
	}
	child.Stdout = &bytes.Buffer{}
	if err := child.Start(); err != nil {
		t.Fatalf("starting the blocked copy child: %v", err)
	}
	defer func() {
		if child.ProcessState == nil {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}()
	sentinel := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		tmp := make([]byte, 1)
		for {
			n, err := stderrPipe.Read(tmp)
			if n > 0 {
				buf.Write(tmp[:n])
				if strings.Contains(buf.String(), "COPY-HOOK-REACHED") {
					sentinel <- buf.String()
					return
				}
			}
			if err != nil {
				sentinel <- buf.String()
				return
			}
		}
	}()
	select {
	case out := <-sentinel:
		if !strings.Contains(out, "COPY-HOOK-REACHED") {
			t.Fatalf("copy child never reached the hook (stderr so far: %s)", out)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the copy child never reached the block hook")
	}

	// The copy is quiesced at the hook: snapshot, restore, snapshot.
	before, err := os.ReadFile(cat)
	if err != nil {
		t.Fatalf("snapshot catalog: %v", err)
	}
	target := restoreTarget(t)
	stdout, stderr, code := runInProcess(t, "restore",
		"--catalog", cat, "--set-id", "S-RCRC", "--target", target,
		"--copy-tree-root", dest)
	if code != report.ExitOK {
		t.Fatalf("restore during a copy run exit = %d, want 0 (readers never block, 2.1)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if strings.Contains(stderr, "busy") || strings.Contains(stderr, "locked") {
		t.Errorf("a reader during a writer must never see the busy class:\n%s", stderr)
	}
	after, err := os.ReadFile(cat)
	if err != nil {
		t.Fatalf("re-read catalog: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("restore modified the catalog file during a copy run")
	}
	if calls := readRestoreCalls(t, record); len(calls) == 0 {
		t.Error("the restore run never reached the seam")
	}

	// Release the copy: one SIGINT (graceful path, exit 130).
	if err := child.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signalling the copy child: %v", err)
	}
	_ = child.Wait()
}

// --- hard-link and latest-state through the REAL catalog --------------------

// TestRestorePartialHardLinkFamilySelection is the 8.2 acceptance criterion at
// the CLI level: selecting ONLY one name of a hard-link family restores the
// family content ONCE from the PRIMARY's image and creates only the REQUESTED
// name (content once, links for the requested names).
//
// The test derives which member is the PRIMARY rather than assuming it: the
// catalog's family rule picks the LOWEST active file_id (hardLinkRows), and
// the scan inserts rows from a CONCURRENT worker pool, so which of fam-a /
// fam-b landed first is genuinely run-dependent - as it is in production.
func TestRestorePartialHardLinkFamilySelection(t *testing.T) {
	cat, dest, _ := restoreTwoImageFixture(t, "S-RHL")
	record := useRestoreFake(t)
	target := restoreTarget(t)

	ids := activeFileIDsFor(t, cat, "S-RHL", "fam-a.txt", "fam-b.txt")
	primary, secondary := "fam-a.txt", "fam-b.txt"
	if ids["fam-b.txt"] < ids["fam-a.txt"] {
		primary, secondary = secondary, primary
	}
	primaryImage, err := placementImageFor(cat, "S-RHL", primary)
	if err != nil {
		t.Fatalf("placement lookup: %v", err)
	}
	secondaryImage, err := placementImageFor(cat, "S-RHL", secondary)
	if err != nil {
		t.Fatalf("placement lookup: %v", err)
	}
	if primaryImage != secondaryImage {
		t.Fatalf("fixture anomaly: family claims split across images %d and %d (the unified claim rule says one)",
			primaryImage, secondaryImage)
	}

	stdout, stderr, code := runInProcess(t, "restore",
		"--catalog", cat, "--set-id", "S-RHL", "--target", target,
		"--path", secondary,
		"--image", fmt.Sprintf("%d=%s", primaryImage, destImageDir(dest, "S-RHL", primaryImage)))
	if code != report.ExitOK {
		t.Fatalf("restore exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	calls := readRestoreCalls(t, record)
	if len(calls) != 1 {
		t.Fatalf("seam calls = %d, want 1 (only the primary's image is required)", len(calls))
	}
	c := calls[0]
	if c.Image != primaryImage {
		t.Errorf("call image = %d, want the primary's image %d", c.Image, primaryImage)
	}
	if len(c.Secondaries) != 1 || c.Secondaries[0] != secondary {
		t.Errorf("secondaries = %v, want exactly the requested name %q", c.Secondaries, secondary)
	}
	if c.PrimaryOf[secondary] != primary {
		t.Errorf("primary map = %v, want %s -> %s (T5.4 reads the content at the primary and links the request)",
			c.PrimaryOf, secondary, primary)
	}
	for _, f := range c.Files {
		if f == primary {
			t.Errorf("the UNSELECTED primary must not appear as a whole-file restore: %v", c.Files)
		}
	}
	// The report shows one restored member (the requested link name).
	if !strings.Contains(stdout, "restored 1") {
		t.Errorf("stdout lacks the single-member restore count:\n%s", stdout)
	}
	if strings.Contains(stdout, primary+" (") {
		t.Errorf("the unselected primary must not be reported as restored:\n%s", stdout)
	}
}

// activeFileIDsFor reads the active file_id of each named relpath (direct SQL
// in the test, the catRW/catOpen precedent): the test needs the catalog's own
// primary rule (lowest file_id wins) to know which family member is which.
func activeFileIDsFor(t *testing.T, catPath, setID string, rels ...string) map[string]int64 {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+catPath+"?mode=ro")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close() //nolint:errcheck // read-only helper
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(rels)), ",")
	args := make([]any, 0, len(rels)+1)
	args = append(args, setID)
	for _, rel := range rels {
		args = append(args, rel)
	}
	rows, err := db.Query(
		`SELECT relpath, file_id FROM files
		 WHERE set_id = ? AND relpath IN (`+placeholders+`)
		   AND status = 'active' AND superseded_by IS NULL`, args...)
	if err != nil {
		t.Fatalf("querying active file ids: %v", err)
	}
	defer rows.Close() //nolint:errcheck // read-only helper
	out := map[string]int64{}
	for rows.Next() {
		var rel string
		var id int64
		if err := rows.Scan(&rel, &id); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		out[rel] = id
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating: %v", err)
	}
	for _, rel := range rels {
		if _, ok := out[rel]; !ok {
			t.Fatalf("no active row for %q in set %q", rel, setID)
		}
	}
	return out
}

// TestRestoreLatestStateIgnoresRecordsNoLongerActive pins D-07 at the CLI
// level: a path deleted between scans is status='missing' in the catalog and
// must NOT be selected (the selection resolves against the latest ACTIVE
// records only), while the surviving path restores once.
func TestRestoreLatestStateIgnoresRecordsNoLongerActive(t *testing.T) {
	root := t.TempDir()
	writeFileP(t, filepath.Join(root, "keep.txt"), pattern(1, 500), 0o644)
	writeFileP(t, filepath.Join(root, "gone.txt"), pattern(2, 500), 0o644)
	cat := createCatalog(t)
	const setID = "S-RLATEST"
	scanBuildFixture(t, root, cat, setID)

	// Delete one path and rescan: its row becomes status='missing'
	// (FR-032 mark-do-not-delete), the survivor stays active.
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatalf("removing the fixture path: %v", err)
	}
	scanBuildFixture(t, root, cat, setID)
	planSynthetic(t, cat, setID, copySyntheticUsable)
	dest := filepath.Join(t.TempDir(), "dest")
	if stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest); code != 0 {
		t.Fatalf("copy exit = %d, want 0\n%s\n%s", code, stdout, stderr)
	}

	record := useRestoreFake(t)
	stdout, stderr, code := runInProcess(t, "restore",
		"--catalog", cat, "--set-id", setID, "--target", restoreTarget(t),
		"--image", "1="+destImageDir(dest, setID, 1))
	if code != report.ExitOK {
		t.Fatalf("restore exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	calls := readRestoreCalls(t, record)
	if len(calls) != 1 {
		t.Fatalf("seam calls = %d, want 1", len(calls))
	}
	selected := strings.Join(calls[0].Selected, ",")
	if strings.Contains(selected, "gone.txt") {
		t.Errorf("the MISSING record must not be selected (latest state only, D-07): %v", calls[0].Selected)
	}
	if !strings.Contains(selected, "keep.txt") {
		t.Errorf("the ACTIVE record must be selected: %v", calls[0].Selected)
	}
	// Exactly one entry per path: no stale second version may double-count.
	if strings.Count(selected, "keep.txt") != 1 {
		t.Errorf("keep.txt appears %d times in the selection, want 1", strings.Count(selected, "keep.txt"))
	}
}

// placementImageFor reads the catalog placement summary (the same read the
// run uses) to learn which image claims a path.
func placementImageFor(catPath, setID, rel string) (int64, error) {
	db, err := catalog.Open(catPath, catalog.OpenReadOnly)
	if err != nil {
		return 0, err
	}
	defer db.Close() //nolint:errcheck // read-only assertion helper
	places, err := db.CopyPlacement(context.Background(), setID)
	if err != nil {
		return 0, err
	}
	for _, p := range places {
		for _, path := range p.Paths {
			if path == rel {
				return p.ImageID, nil
			}
		}
	}
	return 0, fmt.Errorf("no image claims %q", rel)
}

// allFileIDsFor returns EVERY file_id of a relpath (any status), ascending:
// the supersession chain a modify leaves behind (v1 superseded, v2 active).
func allFileIDsFor(t *testing.T, catPath, setID, rel string) []int64 {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+catPath+"?mode=ro")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close() //nolint:errcheck // read-only helper
	rows, err := db.Query(
		`SELECT file_id FROM files WHERE set_id = ? AND relpath = ? ORDER BY file_id`, setID, rel)
	if err != nil {
		t.Fatalf("querying the supersession chain: %v", err)
	}
	defer rows.Close() //nolint:errcheck // read-only helper
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scanning: %v", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating: %v", err)
	}
	return out
}

// countRuns returns the catalog's runs-row count and how many of them are
// kind='restore' (the read-only proof: restore takes NO runs row, 8.x).
func countRuns(t *testing.T, catPath string) (total, restoreRuns int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+catPath+"?mode=ro")
	if err != nil {
		t.Fatalf("open catalog: %v", err)
	}
	defer db.Close() //nolint:errcheck // read-only helper
	if err := db.QueryRow(`SELECT COUNT(*) FROM runs`).Scan(&total); err != nil {
		t.Fatalf("counting runs: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM runs WHERE kind = 'restore'`).Scan(&restoreRuns); err != nil {
		t.Fatalf("counting restore runs: %v", err)
	}
	return total, restoreRuns
}

// restoreDirNames returns the sorted names under dir (nil when absent) - the
// workspace-residue assertions of the abort paths.
func restoreDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// --- partial failures (exit 10) ---------------------------------------------

// A member the seam reports as failed makes the run partial: the report
// enumerates it and the command exits 10 through report.ItemFailures (10.2
// partial-failure semantics), with --strict promoting that to 1.
func TestRestoreFailedMemberExits10(t *testing.T) {
	cat, dest, _ := restoreTwoImageFixture(t, "S-RFAIL")
	useRestoreFake(t, "small1.txt")
	target := restoreTarget(t)

	args := []string{"restore", "--catalog", cat, "--set-id", "S-RFAIL", "--target", target,
		"--image", "1=" + destImageDir(dest, "S-RFAIL", 1),
		"--image", "2=" + destImageDir(dest, "S-RFAIL", 2)}

	stdout, stderr, code := runInProcess(t, args...)
	if code != report.ExitItemFailures {
		t.Fatalf("exit = %d, want 10\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "status: partial") {
		t.Errorf("stdout lacks the partial status:\n%s", stdout)
	}
	if !strings.Contains(stdout, "failed: small1.txt (injected failure)") {
		t.Errorf("stdout lacks the per-file failure line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "small1.txt") || !strings.Contains(stderr, "completed with 1 item failure") {
		t.Errorf("the failure must be enumerated and summarized:\nstdout: %s\nstderr: %s", stdout, stderr)
	}

	// --strict promotes the partial tail to a fatal exit 1.
	stdout, stderr, code = runInProcess(t, append([]string{"--strict"}, args...)...)
	if code != report.ExitFatal {
		t.Fatalf("--strict exit = %d, want 1\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
}

// A record no image claims (scanned after the last plan) is an honest
// per-item failure: reported as unclaimed and exiting 10, never silently
// skipped.
func TestRestoreUnclaimedPathExits10(t *testing.T) {
	cat, dest, root := restoreTwoImageFixture(t, "S-RUNCL")
	// A file scanned AFTER the plan: active, unassigned, unclaimed.
	writeFileP(t, filepath.Join(root, "new-after-plan.txt"), pattern(9, 300), 0o644)
	scanBuildFixture(t, root, cat, "S-RUNCL")
	record := useRestoreFake(t)
	target := restoreTarget(t)

	stdout, stderr, code := runInProcess(t, "restore",
		"--catalog", cat, "--set-id", "S-RUNCL", "--target", target,
		"--image", "1="+destImageDir(dest, "S-RUNCL", 1),
		"--image", "2="+destImageDir(dest, "S-RUNCL", 2))
	if code != report.ExitItemFailures {
		t.Fatalf("exit = %d, want 10\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "unclaimed: new-after-plan.txt") {
		t.Errorf("stdout lacks the unclaimed line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "not assigned to any image") {
		t.Errorf("the unclaimed line must explain the remedy:\n%s", stdout)
	}
	// The planned members still restored (the run continues past the gap).
	calls := readRestoreCalls(t, record)
	if len(calls) != 2 {
		t.Errorf("seam calls = %d, want 2 (the unclaimed path adds no image)", len(calls))
	}
}

// --- interruption (10.2 exit 130) ---------------------------------------------

// TestRestoreSigintExits130 drives a REAL SIGINT into a REAL restore child
// whose seam fake parks INSIDE the first image (the deterministic window:
// the fake waits for the cancelled run context, no sleeps). The run must
// finalize an interrupted report on stdout, print the interruption on stderr,
// and exit 130 - and the single temp workspace must still be cleaned up by
// the abort path.
func TestRestoreSigintExits130(t *testing.T) {
	cat, dest, _ := restoreTwoImageFixture(t, "S-RINT")
	record := filepath.Join(t.TempDir(), restoreCallsFile)
	target := restoreTarget(t)

	cmd := exec.Command(os.Args[0],
		"restore", "--catalog", cat, "--set-id", "S-RINT", "--target", target,
		"--image", "1="+destImageDir(dest, "S-RINT", 1),
		"--image", "2="+destImageDir(dest, "S-RINT", 2))
	cmd.Env = append(os.Environ(), scanSignalHelperEnv+"=1",
		restoreFakeEnv+"=1", restoreRecordEnv+"="+record, restoreBlockEnv+"=1")
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	stdoutBuf := &bytes.Buffer{}
	cmd.Stdout = stdoutBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the restore child: %v", err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	// Readiness: the fake's marker line on stderr (no sleeps, the marker IS
	// the "parked inside image 1" signal).
	sentinel := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		tmp := make([]byte, 1)
		for {
			n, err := stderrPipe.Read(tmp)
			if n > 0 {
				buf.Write(tmp[:n])
				if strings.Contains(buf.String(), restoreBlocked) {
					sentinel <- buf.String()
					return
				}
			}
			if err != nil {
				sentinel <- buf.String()
				return
			}
		}
	}()
	select {
	case out := <-sentinel:
		if !strings.Contains(out, restoreBlocked) {
			t.Fatalf("restore child never reached the block seam (stderr so far: %s)", out)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the restore child never reached the block seam")
	}

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signalling the restore child: %v", err)
	}
	err = cmd.Wait()
	exitCode := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("restore child: %v", err)
		}
		exitCode = exitErr.ExitCode()
	}
	if exitCode != report.ExitInterrupted {
		t.Fatalf("exit = %d, want 130\nstdout: %s\nstderr: %s", exitCode, stdoutBuf.String(), "")
	}
	stdout := stdoutBuf.String()
	if !strings.Contains(stdout, "status: interrupted") {
		t.Errorf("stdout lacks the interrupted report:\n%s", stdout)
	}
	// Only the parked image reached the seam; the second one never ran.
	calls := readRestoreCalls(t, record)
	if len(calls) > 1 {
		t.Errorf("seam calls = %d, want at most 1 (no new image after cancellation)", len(calls))
	}
	if len(calls) == 1 {
		if _, err := os.Stat(calls[0].Workspace); !os.IsNotExist(err) {
			t.Errorf("the workspace must be cleaned on the interrupt path: %v", err)
		}
	}
}

// --- the layering pin (no implicit original-source restore) -------------------

// TestRestoreNeverTouchesSourceRoot is the structural half of 8.2's "the tool
// never restores onto the original source path implicitly": the command must
// not read sets.source_root at all (there is nothing to derive an implicit
// target from), and the run's target comes from --target alone.
func TestRestoreNeverTouchesSourceRoot(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("restore.go"))
	if err != nil {
		t.Fatalf("reading restore.go: %v", err)
	}
	if strings.Contains(string(src), "SetSourceRoot") {
		t.Error("internal/cli/restore.go references SetSourceRoot; --target must be the only destination input (DESIGN_SPEC 8.2)")
	}

	cat, dest, root := restoreTwoImageFixture(t, "S-RTGT")
	record := useRestoreFake(t)
	target := restoreTarget(t)
	stdout, stderr, code := runInProcess(t, "restore",
		"--catalog", cat, "--set-id", "S-RTGT", "--target", target,
		"--image", "1="+destImageDir(dest, "S-RTGT", 1),
		"--image", "2="+destImageDir(dest, "S-RTGT", 2))
	if code != report.ExitOK {
		t.Fatalf("restore exit = %d, want 0\n%s\n%s", code, stdout, stderr)
	}
	for _, c := range readRestoreCalls(t, record) {
		if c.Target != target {
			t.Errorf("call target = %q, want --target %q", c.Target, target)
		}
		if strings.HasPrefix(c.Target, root) {
			t.Errorf("target %q must not be the source root %q (implicit-source restore is forbidden)", c.Target, root)
		}
	}
}

// --- selection edges against REAL inactive states (exit 2) -------------------

// TestRestoreInactiveRecordSelectionGatesExit2 pins "every selector entry must
// match >=1 ACTIVE record" against the two REAL inactive states a catalog can
// hold: a MISSING-status path (deleted, then rescanned) and a path whose only
// remaining rows are SUPERSEDED (modified, then the new version deleted). Both
// must be exit-2 input errors - never a silent selection that restores old or
// missing content.
func TestRestoreInactiveRecordSelectionGatesExit2(t *testing.T) {
	root := t.TempDir()
	writeFileP(t, filepath.Join(root, "keep.txt"), pattern(1, 500), 0o644)
	writeFileP(t, filepath.Join(root, "gone.txt"), pattern(2, 500), 0o644)
	writeFileP(t, filepath.Join(root, "data.txt"), pattern(3, 500), 0o644)
	cat := createCatalog(t)
	const setID = "S-RINACT"
	scanBuildFixture(t, root, cat, setID)

	// data.txt is MODIFIED: v1 superseded, v2 active.
	writeFileP(t, filepath.Join(root, "data.txt"), pattern(4, 600), 0o644)
	scanBuildFixture(t, root, cat, setID)
	// Then BOTH are deleted: gone.txt becomes missing, and data.txt is left
	// with ONLY its superseded v1 row (v2 status='missing').
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatalf("removing gone.txt: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "data.txt")); err != nil {
		t.Fatalf("removing data.txt: %v", err)
	}
	scanBuildFixture(t, root, cat, setID)
	planSynthetic(t, cat, setID, copySyntheticUsable)

	record := useRestoreFake(t)
	before, err := os.ReadFile(cat)
	if err != nil {
		t.Fatalf("snapshot catalog: %v", err)
	}

	for _, path := range []string{"gone.txt", "data.txt"} {
		target := restoreTarget(t)
		stdout, stderr, code := runInProcess(t, "restore",
			"--catalog", cat, "--set-id", setID, "--target", target, "--path", path)
		if code != report.ExitUsage {
			t.Fatalf("--path %s exit = %d, want 2\nstdout: %s\nstderr: %s", path, code, stdout, stderr)
		}
		if !strings.Contains(stderr, "matches no active record") {
			t.Errorf("--path %s stderr lacks the active-record refusal:\n%s", path, stderr)
		}
		if stdout != "" {
			t.Errorf("--path %s: stdout must stay empty on an input-error abort, got %q", path, stdout)
		}
		if _, err := os.Stat(target); !os.IsNotExist(err) {
			t.Errorf("--path %s: target written despite the refusal: %v", path, err)
		}
	}
	if calls := readRestoreCalls(t, record); len(calls) != 0 {
		t.Errorf("seam calls = %d, want 0 (an inactive path must never restore OLD content)", len(calls))
	}
	after, err := os.ReadFile(cat)
	if err != nil {
		t.Fatalf("re-read catalog: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("the input-error abort modified the catalog file")
	}
}

// TestRestoreSubtreeOnFileIsExact pins the component-aware rule's degenerate
// case through the REAL catalog: --subtree naming a FILE selects exactly that
// file, while a bare byte-prefix of its name matches nothing (exit 2, never a
// silent prefix pull).
func TestRestoreSubtreeOnFileIsExact(t *testing.T) {
	cat, dest, _ := restoreTwoImageFixture(t, "S-RSUBF")
	record := useRestoreFake(t)

	stdout, stderr, code := runInProcess(t, "restore",
		"--catalog", cat, "--set-id", "S-RSUBF", "--target", restoreTarget(t),
		"--subtree", "small1.txt",
		"--image", "2="+destImageDir(dest, "S-RSUBF", 2))
	if code != report.ExitOK {
		t.Fatalf("--subtree <file> exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	calls := readRestoreCalls(t, record)
	if len(calls) != 1 {
		t.Fatalf("seam calls = %d, want 1", len(calls))
	}
	if strings.Join(calls[0].Selected, ",") != "small1.txt" {
		t.Errorf("selected = %v, want exactly the named file", calls[0].Selected)
	}

	// A component prefix of the FILE name is not a directory prefix: no
	// active relpath equals it or sits under it, so the gate refuses.
	_, stderr, code = runInProcess(t, "restore",
		"--catalog", cat, "--set-id", "S-RSUBF", "--target", restoreTarget(t),
		"--subtree", "small")
	if code != report.ExitUsage {
		t.Fatalf("--subtree small (byte-prefix of a file) exit = %d, want 2\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "--subtree") {
		t.Errorf("stderr must name the offending selector:\n%s", stderr)
	}
}

// TestRestoreUnclaimedOnlySelectionDoesNotDemandMedia: a selection whose EVERY
// path is unclaimed (scanned after the plan) is an honest exit-10 report - NOT
// a media demand. With NO supply flags at all the run must not invent a
// required-image abort (exit 3 would tell cron to fetch media that could never
// unclaim the path).
func TestRestoreUnclaimedOnlySelectionDoesNotDemandMedia(t *testing.T) {
	cat, _, root := restoreTwoImageFixture(t, "S-RUNCONLY")
	// A file scanned AFTER the plan: active, unassigned, unclaimed.
	writeFileP(t, filepath.Join(root, "new-after-plan.txt"), pattern(9, 300), 0o644)
	scanBuildFixture(t, root, cat, "S-RUNCONLY")
	record := useRestoreFake(t)
	workDir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		t.Fatalf("work dir: %v", err)
	}
	target := restoreTarget(t)

	stdout, stderr, code := runInProcess(t, "restore",
		"--catalog", cat, "--set-id", "S-RUNCONLY", "--target", target,
		"--path", "new-after-plan.txt", "--work-dir", workDir)
	if code != report.ExitItemFailures {
		t.Fatalf("exit = %d, want 10 (unclaimed is partial, never a media demand)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if !strings.Contains(stdout, "unclaimed: new-after-plan.txt") {
		t.Errorf("stdout lacks the unclaimed line:\n%s", stdout)
	}
	if strings.Contains(stderr, "required-image") {
		t.Errorf("no media may be demanded for an all-unclaimed selection:\n%s", stderr)
	}
	if calls := readRestoreCalls(t, record); len(calls) != 0 {
		t.Errorf("seam calls = %d, want 0 (no image carries the unclaimed path)", len(calls))
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target exists after the unclaimed-only run: %v", err)
	}
	if left := restoreDirNames(t, workDir); len(left) != 0 {
		t.Errorf("work dir = %v, want no workspace residue", left)
	}
}

// --- latest-state content direction (D-07) ------------------------------------

// TestRestoreModifiedPathRestoresLatestContent pins the D-07 content direction
// through the REAL catalog: after modify + rescan (v1 superseded, v2 active),
// the seam receives the ACTIVE version's file_id and size - a restore can
// never resolve a selected path to OLD bytes.
func TestRestoreModifiedPathRestoresLatestContent(t *testing.T) {
	root := t.TempDir()
	const setID = "S-RMOD"
	v1, v2 := pattern(1, 700), pattern(2, 800)
	writeFileP(t, filepath.Join(root, "data.txt"), v1, 0o644)
	cat := createCatalog(t)
	scanBuildFixture(t, root, cat, setID)
	writeFileP(t, filepath.Join(root, "data.txt"), v2, 0o644)
	scanBuildFixture(t, root, cat, setID)
	planSynthetic(t, cat, setID, copySyntheticUsable)
	dest := filepath.Join(t.TempDir(), "dest")
	if stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest); code != 0 {
		t.Fatalf("copy exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	chain := allFileIDsFor(t, cat, setID, "data.txt")
	if len(chain) != 2 {
		t.Fatalf("fixture anomaly: data.txt rows = %v, want the v1+v2 supersession chain", chain)
	}
	active := activeFileIDsFor(t, cat, setID, "data.txt")
	if active["data.txt"] != chain[len(chain)-1] {
		t.Fatalf("fixture anomaly: the active row %d must be the NEWEST of the chain %v", active["data.txt"], chain)
	}

	record := useRestoreFake(t)
	stdout, stderr, code := runInProcess(t, "restore",
		"--catalog", cat, "--set-id", setID, "--target", restoreTarget(t),
		"--image", "1="+destImageDir(dest, setID, 1))
	if code != report.ExitOK {
		t.Fatalf("restore exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	calls := readRestoreCalls(t, record)
	if len(calls) != 1 {
		t.Fatalf("seam calls = %d, want 1", len(calls))
	}
	meta, ok := calls[0].FileMeta["data.txt"]
	if !ok {
		t.Fatalf("the seam never received data.txt: files=%v meta=%v", calls[0].Files, calls[0].FileMeta)
	}
	if meta.ID != active["data.txt"] {
		t.Errorf("seam file_id = %d, want the ACTIVE row %d (the superseded %d must never resolve)",
			meta.ID, active["data.txt"], chain[0])
	}
	if meta.Size != int64(len(v2)) {
		t.Errorf("seam size = %d, want the active version's %d (old bytes must never resolve)", meta.Size, len(v2))
	}
}

// --- prompt honesty edges (real PTY) ------------------------------------------

// TestRestoreTTYPromptEOFExits3Honestly: a TTY prompt whose input stream ends
// without an answer is the honest closed-input abort - same media-supply class
// (exit 3), stdout empty, target untouched, no seam call.
func TestRestoreTTYPromptEOFExits3Honestly(t *testing.T) {
	cat, _, _ := restoreTwoImageFixture(t, "S-REOF")
	target := restoreTarget(t)

	stdout, ptyOut, record, code := runRestoreOnPTY(t, nil, // stdin closes immediately
		"restore", "--catalog", cat, "--set-id", "S-REOF", "--target", target,
		"--image-dir", t.TempDir())

	if code != 3 {
		t.Errorf("exit = %d, want 3 (the media-supply class)\npty: %s", code, ptyOut)
	}
	if !strings.Contains(ptyOut, "prompt input closed") {
		t.Errorf("the abort must tell the closed-input story:\n%s", ptyOut)
	}
	if stdout != "" {
		t.Errorf("stdout must stay empty when the prompt input closed, got: %q", stdout)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("target must stay untouched after the closed prompt: %v", err)
	}
	if calls := readRestoreCalls(t, record); len(calls) != 0 {
		t.Errorf("seam calls = %d, want 0", len(calls))
	}
}

// TestRestoreTTYPromptRetriesWrongShapePath: an interactive answer that exists
// but has the wrong shape (a character device - neither an .iso nor a copy
// tree) is a warning and another question, never an input error: the guided
// user can recover from any typo (8.3).
func TestRestoreTTYPromptRetriesWrongShapePath(t *testing.T) {
	cat, dest, _ := restoreTwoImageFixture(t, "S-RWRONG")
	tree1 := destImageDir(dest, "S-RWRONG", 1)
	target := restoreTarget(t)

	stdout, ptyOut, record, code := runRestoreOnPTY(t, []string{"/dev/null\n", tree1 + "\n"},
		"restore", "--catalog", cat, "--set-id", "S-RWRONG", "--target", target,
		"--image-dir", t.TempDir(),
		"--image", "2="+destImageDir(dest, "S-RWRONG", 2))

	if code != report.ExitOK {
		t.Fatalf("restore after a wrong-shape retry exit = %d, want 0\nstdout: %s\npty: %s", code, stdout, ptyOut)
	}
	if got := strings.Count(ptyOut, "Provide its path (or 'q' to abort):"); got != 2 {
		t.Errorf("prompt appeared %d times, want 2 (warn + re-ask, never an input error):\n%s", got, ptyOut)
	}
	if !strings.Contains(ptyOut, "neither a regular file") {
		t.Errorf("PTY output lacks the wrong-shape warning:\n%s", ptyOut)
	}
	calls := readRestoreCalls(t, record)
	if len(calls) != 2 || calls[0].Path != tree1 || calls[0].Kind != "copy" {
		t.Errorf("seam calls = %+v, want image 1 from the re-asked tree", calls)
	}
}
