package ingest

// T1.3 walker tests: a DESIGN_SPEC 12.2-spirit fixture tree walked with
// a live matcher, plus the walk-level contracts: pruned subtrees yield
// no jobs and no empty-dir entries, special files (FIFO) warn and
// produce no job, symlinks (valid AND dangling) are captured with their
// raw targets and never followed, empty directories are collected,
// unreadable directories warn and the walk continues (DESIGN_SPEC 3.4),
// the scan-root safety rail refuses "/" without exclusions and walks
// past it with one (FR-005), and emit failures abort the walk verbatim.
//
// Stdlib only; every fixture lives in t.TempDir().

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"isopress/internal/progress"
	"isopress/internal/report"
)

// emptyDirPaths extracts the relpaths from a walk's EmptyDirs result
// (T2.2: the entries now carry the directory's own lstat metadata
// alongside the relpath, for the durable directories table).
func emptyDirPaths(res WalkResult) []string {
	out := make([]string, 0, len(res.EmptyDirs))
	for _, d := range res.EmptyDirs {
		out = append(out, d.RelPath)
	}
	return out
}

// buildWalkFixture creates the fixture tree (DESIGN_SPEC 12.2 spirit:
// nested dirs, an empty dir, valid and dangling symlinks, a FIFO, a
// .tmp file at depth, a pruned dir with content AND an empty dir inside
// the pruned subtree) and returns its root:
//
//	root/
//	  docs/readme.txt          "hello"
//	  docs/empty/              (empty directory)
//	  docs/sub/deep/note.md    "note"
//	  var/log/app.log          "app"
//	  var/log/debug.tmp        "dbg"
//	  src/main.go              "package main"
//	  src/a.tmp                "tmp-top"      <- excluded by src/*.tmp
//	  src/gen/a.tmp            "tmp-deep"     <- NOT excluded (*'s limit)
//	  bin/tool.iso             "iso"          <- excluded by *.iso
//	  cache/old/x.dat          "old"          <- cache/ pruned
//	  cache/inner-empty/       (empty dir INSIDE the pruned subtree)
//	  link/valid   -> ../docs/readme.txt
//	  link/dangle  -> ../nowhere
//	  special/fifo             (FIFO)
func buildWalkFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	dirs := []string{
		"docs", "docs/empty", "docs/sub", "docs/sub/deep",
		"var", "var/log", "src", "src/gen", "bin",
		"link", "special", "cache", "cache/old", "cache/inner-empty",
	}
	for _, d := range dirs {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	files := map[string]string{
		"docs/readme.txt":       "hello",
		"docs/sub/deep/note.md": "note",
		"var/log/app.log":       "app",
		"var/log/debug.tmp":     "dbg",
		"src/main.go":           "package main",
		"src/a.tmp":             "tmp-top",
		"src/gen/a.tmp":         "tmp-deep",
		"bin/tool.iso":          "iso",
		"cache/old/x.dat":       "old",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}

	if err := os.Symlink("../docs/readme.txt", filepath.Join(root, "link/valid")); err != nil {
		t.Fatalf("symlink valid: %v", err)
	}
	if err := os.Symlink("../nowhere", filepath.Join(root, "link/dangle")); err != nil {
		t.Fatalf("symlink dangle: %v", err)
	}

	if err := syscall.Mkfifo(filepath.Join(root, "special/fifo"), 0o644); err != nil {
		if errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.EPERM) {
			t.Skipf("mkfifo unsupported in this environment: %v", err)
		}
		t.Fatalf("mkfifo: %v", err)
	}
	return root
}

// fixtureMatcher returns the matcher used by the integration walk:
// '*.iso' (basename glob), 'cache/' (dir-only prune), 'src/*.tmp'
// (anchored, '*' not crossing '/').
func fixtureMatcher(t *testing.T) Matcher {
	t.Helper()
	m, err := NewMatcher([]string{"*.iso", "cache/", "src/*.tmp"}, "")
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}
	return m
}

// collectJobs is the recording emit callback for Walk.
func collectJobs(into *[]Job) func(Job) error {
	return func(j Job) error {
		*into = append(*into, j)
		return nil
	}
}

func jobRelPaths(jobs []Job) []string {
	out := make([]string, len(jobs))
	for i, j := range jobs {
		out[i] = j.RelPath
	}
	sort.Strings(out)
	return out
}

// TestWalkFixtureIntegration is the walk-level acceptance test of
// IMPLEMENTATION_PLAN T1.3: exact job list, exact symlink entries, exact
// empty-dir list, exact counts, warnings for the FIFO, and the pruned
// subtree contributing NOTHING (no jobs, no empty dirs, no counts).
func TestWalkFixtureIntegration(t *testing.T) {
	root := buildWalkFixture(t)

	var jobs []Job
	res, err := Walk(root, fixtureMatcher(t), collectJobs(&jobs), nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	// Exact job list: relpaths AND kinds (only regular files become
	// jobs; symlinks, specials, and excluded paths must be absent).
	wantJobs := []string{
		"docs/readme.txt",
		"docs/sub/deep/note.md",
		"src/gen/a.tmp",
		"src/main.go",
		"var/log/app.log",
		"var/log/debug.tmp",
	}
	if got := jobRelPaths(jobs); !reflect.DeepEqual(got, wantJobs) {
		t.Errorf("job relpaths = %v, want %v", got, wantJobs)
	}
	if res.Files != len(wantJobs) {
		t.Errorf("Files = %d, want %d", res.Files, len(wantJobs))
	}
	// No job may reference the pruned or excluded paths.
	for _, j := range jobs {
		if strings.HasPrefix(j.RelPath, "cache/") {
			t.Errorf("pruned subtree leaked a job: %s", j.RelPath)
		}
		if j.RelPath == "bin/tool.iso" || j.RelPath == "src/a.tmp" {
			t.Errorf("excluded file leaked a job: %s", j.RelPath)
		}
		if !strings.HasPrefix(j.AbsPath, root) {
			t.Errorf("job %s AbsPath %q is not under the scan root", j.RelPath, j.AbsPath)
		}
	}

	// Job metadata is captured at walk time (DESIGN_SPEC 3.1).
	for _, j := range jobs {
		if j.RelPath != "docs/readme.txt" {
			continue
		}
		if j.Meta.Size != 5 {
			t.Errorf("readme.txt Meta.Size = %d, want 5", j.Meta.Size)
		}
		if j.Meta.Mode.Perm() != 0o644 {
			t.Errorf("readme.txt Meta.Mode.Perm() = %o, want 644", j.Meta.Mode.Perm())
		}
		if j.Meta.Inode == 0 || j.Meta.Dev == 0 {
			t.Errorf("readme.txt Meta lacks dev/inode: %+v", j.Meta)
		}
	}

	// Symlinks: both captured with their RAW targets, never followed,
	// and never emitted as jobs.
	wantLinks := map[string]string{
		"link/valid":  "../docs/readme.txt",
		"link/dangle": "../nowhere",
	}
	gotLinks := map[string]string{}
	for _, s := range res.Symlinks {
		gotLinks[s.RelPath] = s.Target
		if containsRel(jobs, s.RelPath) {
			t.Errorf("symlink %s also emitted a hash job", s.RelPath)
		}
		if !strings.HasPrefix(s.AbsPath, root) {
			t.Errorf("symlink %s AbsPath %q is not under the scan root", s.RelPath, s.AbsPath)
		}
	}
	if !reflect.DeepEqual(gotLinks, wantLinks) {
		t.Errorf("symlink entries = %v, want %v", gotLinks, wantLinks)
	}

	// Pruning: exactly one directory pruned (cache), and the pruned
	// subtree contributes no empty dirs.
	if res.ExcludedDirsPruned != 1 {
		t.Errorf("ExcludedDirsPruned = %d, want 1 (cache/)", res.ExcludedDirsPruned)
	}
	if res.ExcludedFiles != 2 {
		t.Errorf("ExcludedFiles = %d, want 2 (bin/tool.iso, src/a.tmp)", res.ExcludedFiles)
	}

	// Dirs: every visited non-pruned, non-root directory.
	wantDirs := []string{
		"bin", "docs", "docs/empty", "docs/sub", "docs/sub/deep",
		"link", "special", "src", "src/gen", "var", "var/log",
	}
	if res.Dirs != len(wantDirs) {
		t.Errorf("Dirs = %d, want %d (%v)", res.Dirs, len(wantDirs), wantDirs)
	}

	// Empty dirs: docs/empty only; cache/inner-empty is inside the
	// pruned subtree and must NOT appear. T2.2: each entry carries the
	// directory's own lstat metadata (mode/uid/gid/mtime_ns feed the
	// durable directories table, DESIGN_SPEC 2.3).
	if !reflect.DeepEqual(emptyDirPaths(res), []string{"docs/empty"}) {
		t.Errorf("EmptyDirs = %v, want [docs/empty]", emptyDirPaths(res))
	}
	if len(res.EmptyDirs) == 1 {
		ed := res.EmptyDirs[0]
		if ed.Meta.Mode.Perm() != 0o755 {
			t.Errorf("empty dir mode = %v, want 0755 (the directory's own metadata)", ed.Meta.Mode.Perm())
		}
		if ed.Meta.MtimeNS == 0 {
			t.Errorf("empty dir mtime_ns = 0, want the directory's captured mtime")
		}
	}

	// Special file: one warning, one skipped, no job.
	if res.SpecialsSkipped != 1 {
		t.Errorf("SpecialsSkipped = %d, want 1 (special/fifo)", res.SpecialsSkipped)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "special/fifo") {
		t.Errorf("Warnings = %v, want exactly one naming special/fifo", res.Warnings)
	}
}

// TestWalkWithoutExclusions walks the same fixture with the zero-value
// matcher: nothing is pruned or dropped, and the cache/inner-empty dir
// becomes a legitimate empty-dir entry.
func TestWalkWithoutExclusions(t *testing.T) {
	root := buildWalkFixture(t)

	var jobs []Job
	res, err := Walk(root, Matcher{}, collectJobs(&jobs), nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if res.Files != 9 { // every regular file: 9 in the fixture
		t.Errorf("Files = %d, want 9", res.Files)
	}
	if res.ExcludedDirsPruned != 0 || res.ExcludedFiles != 0 {
		t.Errorf("zero matcher excluded something: dirs=%d files=%d",
			res.ExcludedDirsPruned, res.ExcludedFiles)
	}
	wantEmpty := []string{"cache/inner-empty", "docs/empty"}
	got := append([]string{}, emptyDirPaths(res)...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, wantEmpty) {
		t.Errorf("EmptyDirs = %v, want %v", got, wantEmpty)
	}
	if len(res.Symlinks) != 2 {
		t.Errorf("Symlinks = %d, want 2", len(res.Symlinks))
	}
}

// TestWalkEmptyRoot: a root with no entries yields an all-zero result
// and does NOT list the root itself as an empty dir (FR-037 concerns
// the tree below the root).
func TestWalkEmptyRoot(t *testing.T) {
	root := t.TempDir()
	res, err := Walk(root, Matcher{}, nil, nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if res.Dirs != 0 || res.Files != 0 || res.SpecialsSkipped != 0 ||
		res.ExcludedDirsPruned != 0 || res.ExcludedFiles != 0 ||
		len(res.Symlinks) != 0 || len(res.EmptyDirs) != 0 || len(res.Warnings) != 0 {
		t.Errorf("Walk of empty root = %+v, want all-zero result", res)
	}
}

// TestWalkUnreadableDirectory: chmod-000 directory (DESIGN_SPEC 3.4) -
// warning with the subtree skipped, sibling files still walked, no
// abort. Skipped as root, where chmod-000 does not block reads.
func TestWalkUnreadableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; chmod 000 does not restrict directory reads")
	}
	root := t.TempDir()
	for _, d := range []string{"blocked", "blocked/sub", "blocked/inner", "keep"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"blocked/sub.txt", "blocked/inner/deep.txt", "keep/good.txt"} {
		if err := os.WriteFile(filepath.Join(root, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(filepath.Join(root, "blocked"), 0o000); err != nil {
		t.Fatal(err)
	}
	// Restore permissions so t.TempDir cleanup succeeds.
	defer func() {
		_ = os.Chmod(filepath.Join(root, "blocked"), 0o755)
	}()

	var jobs []Job
	res, err := Walk(root, Matcher{}, collectJobs(&jobs), nil)
	if err != nil {
		t.Fatalf("Walk: %v (unreadable subtree must not abort the walk)", err)
	}
	if got := jobRelPaths(jobs); !reflect.DeepEqual(got, []string{"keep/good.txt"}) {
		t.Errorf("job relpaths = %v, want [keep/good.txt]", got)
	}
	if len(res.Warnings) != 1 ||
		!strings.Contains(res.Warnings[0], "cannot read directory") ||
		!strings.Contains(res.Warnings[0], "blocked") {
		t.Errorf("Warnings = %v, want exactly one 'cannot read directory ... blocked'", res.Warnings)
	}
	// The unreadable dir was visited (counted) but is NEVER claimed
	// empty: its contents are unknown.
	if res.Dirs != 2 { // keep + blocked
		t.Errorf("Dirs = %d, want 2 (keep, blocked)", res.Dirs)
	}
	if len(res.EmptyDirs) != 0 {
		t.Errorf("EmptyDirs = %v, want none (unreadable dir must not be claimed empty)", res.EmptyDirs)
	}
}

// TestWalkUnreadableRootIsOperationalError pins the OPERATIONAL half of
// the root-error classification: a chmod-000 root is shape-valid (it
// lstat'ed as a directory) but its ReadDir is denied (EACCES), so the
// error must NOT be a report.UsageError and must classify to
// report.ExitFatal (1) - the environment denied access, not the user's
// wrong input. TestWalkRejectsBadRoots pins the SHAPE half (exit 2).
// Skipped as root, where chmod-000 does not block reads.
func TestWalkUnreadableRootIsOperationalError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; chmod 000 does not restrict directory reads")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o000); err != nil {
		t.Fatal(err)
	}
	// Restore permissions so t.TempDir cleanup succeeds.
	defer func() {
		_ = os.Chmod(root, 0o755)
	}()

	_, err := Walk(root, Matcher{}, nil, nil)
	if err == nil {
		t.Fatal("Walk on a chmod-000 root succeeded, want an operational error")
	}
	var usage *report.UsageError
	if errors.As(err, &usage) {
		t.Fatalf("error %v is a report.UsageError; an unreadable (EACCES) but "+
			"shape-valid root must be an operational error (exit 1), not usage (exit 2)", err)
	}
	if got := report.Classify(err); got != report.ExitFatal {
		t.Errorf("classification = %d, want %d (ExitFatal -> exit 1)", got, report.ExitFatal)
	}
	if !strings.Contains(err.Error(), root) {
		t.Errorf("error %q does not name the scan root %q", err, root)
	}
}

// TestWalkScanRootRefusal: scanning "/" with no effective exclusions
// refuses with a usage-class error (FR-005, DESIGN_SPEC 3.2), the
// refusal happens before any walk result is produced, and the message
// explains the required flags.
func TestWalkScanRootRefusal(t *testing.T) {
	m, err := NewMatcher(nil, "")
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}
	res, err := Walk("/", m, nil, nil)
	if err == nil {
		t.Fatal("Walk(\"/\") without exclusions succeeded, want usage error")
	}
	var usage *report.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("error %v is not a report.UsageError", err)
	}
	if got := report.Classify(err); got != report.ExitUsage {
		t.Errorf("classification = %d, want %d (ExitUsage -> exit 2)", got, report.ExitUsage)
	}
	for _, want := range []string{"--exclude", "--exclude-from", "filesystem root"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message %q does not mention %q", err, want)
		}
	}
	if !reflect.DeepEqual(res, WalkResult{}) {
		t.Errorf("refused walk returned result %+v, want zero value (rail fires before any walk)", res)
	}
}

// TestWalkScanRootProceedsWithExclusion: with one exclusion the rail
// lets "/" through and the walk actually RUNS. Safety and speed: the
// single pattern '**' matches every path, so every top-level directory
// is pruned on the PRE-ReadDir callback (zero subtree reads) and every
// top-level file is excluded - the walk reads only the root directory
// listing and finishes immediately. This is the "tiny fake-root
// equivalence" the task asks for: it exercises the real "/" path without
// scanning any of it.
func TestWalkScanRootProceedsWithExclusion(t *testing.T) {
	m, err := NewMatcher([]string{"**"}, "")
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}
	var jobs []Job
	res, err := Walk("/", m, collectJobs(&jobs), nil)
	if err != nil {
		t.Fatalf("Walk(\"/\") with one exclusion: %v (the rail must let it past)", err)
	}
	if len(jobs) != 0 || res.Files != 0 || res.Dirs != 0 {
		t.Errorf("Walk(\"/\") with '**' produced jobs=%v files=%d dirs=%d, want all zero (everything pruned or excluded)",
			jobs, res.Files, res.Dirs)
	}
	if res.ExcludedDirsPruned == 0 {
		t.Error("ExcludedDirsPruned = 0; expected the top-level directories of / to be pruned (proof the walk ran)")
	}
}

// TestWalkRejectsBadRoots: root shape violations are usage-class
// errors - a regular file, a missing path, and a symlink root (D-03:
// the tool never follows symlinks, not even the root).
func TestWalkRejectsBadRoots(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		root string
	}{
		{"regular file root", file},
		{"missing root", filepath.Join(dir, "missing")},
		{"symlink root", link},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Walk(tc.root, Matcher{}, nil, nil)
			if err == nil {
				t.Fatalf("Walk(%q) succeeded, want usage error", tc.root)
			}
			var usage *report.UsageError
			if !errors.As(err, &usage) {
				t.Fatalf("error %v is not a report.UsageError", err)
			}
			if got := report.Classify(err); got != report.ExitUsage {
				t.Errorf("classification = %d, want %d (ExitUsage -> exit 2)", got, report.ExitUsage)
			}
		})
	}
}

// TestWalkEmitErrorAborts: a failure inside the T1.4 seam (emit
// returning an error, e.g. the hash pool cancelled) aborts the walk and
// is returned verbatim so callers can errors.Is it.
func TestWalkEmitErrorAborts(t *testing.T) {
	root := buildWalkFixture(t)
	sentinel := errors.New("hash pool closed")

	var seen []Job
	_, err := Walk(root, Matcher{}, func(j Job) error {
		seen = append(seen, j)
		if len(seen) == 2 {
			return sentinel
		}
		return nil
	}, nil)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Walk error = %v, want the sentinel from emit", err)
	}
	if len(seen) != 2 {
		t.Errorf("emit called %d times, want 2 (walk aborts at the failing job)", len(seen))
	}
}

// TestWalkWarnEventsReachBus: warnings reach the progress bus as
// progress.Warn events (DESIGN_SPEC 9.1, unthrottled per 9.3), rendered
// by the real plain renderer; Walk itself never touches any stream.
func TestWalkWarnEventsReachBus(t *testing.T) {
	root := buildWalkFixture(t) // contains the special/fifo warning

	bus := progress.NewBus()
	var buf bytes.Buffer
	r := progress.New(progress.Options{
		Mode:   progress.ModePlain,
		Stderr: &buf,
		Now:    func() time.Time { return time.Unix(0, 0) },
	})
	done := make(chan progress.Done, 1)
	go func() { done <- r.Run(bus) }()

	if _, err := Walk(root, fixtureMatcher(t), nil, bus); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	// Done is the guaranteed-delivery terminal event; receiving it also
	// joins the renderer goroutine (the buffer is written by exactly one
	// goroutine and read here after the happens-before edge).
	bus.Finish(progress.Done{Summary: "walk complete"})
	d := <-done

	if d.Interrupted {
		t.Error("renderer reported interruption on a clean walk")
	}
	out := buf.String()
	if !strings.Contains(out, "warning: ") || !strings.Contains(out, "special/fifo") {
		t.Errorf("rendered output %q does not contain the FIFO warning", out)
	}
	if !strings.Contains(out, "walk complete") {
		t.Errorf("rendered output %q lacks the final summary", out)
	}
}

// TestWalkTotalBytesAndFiles pins the percent-after-walk totals
// (DESIGN_SPEC 3.6/9.1 amendment, 2026-10-19): WalkResult.TotalBytes
// is the exact sum of the emitted Jobs' walk-time lstat Meta.Size, and
// WalkResult.Files (the reused total-count field - every counted file
// emits exactly one job) matches the emitted-job count. The exact
// constant also proves the exclusions hold on the byte total: the
// pruned cache/ subtree (4 bytes), the excluded bin/tool.iso (3) and
// src/a.tmp (7), and the symlink targets (17 + 9) contribute ZERO -
// only hashable regular files count, matching the hash phase's
// ItemDone byte accounting.
func TestWalkTotalBytesAndFiles(t *testing.T) {
	root := buildWalkFixture(t)

	// Fixture bytes of the six INCLUDED regular files:
	// readme.txt "hello"=5, note.md "note"=4, app.log "app"=3,
	// debug.tmp "dbg"=3, main.go "package main"=12,
	// gen/a.tmp "tmp-deep"=8.
	const (
		wantFiles = 6
		wantBytes = int64(5 + 4 + 3 + 3 + 12 + 8)
	)

	// Emitting walk: totals derived from the collected jobs must agree
	// with the walker's own counters, byte for byte.
	var jobs []Job
	res, err := Walk(root, fixtureMatcher(t), collectJobs(&jobs), nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	var sum int64
	for _, j := range jobs {
		sum += j.Meta.Size
	}
	if res.TotalBytes != sum {
		t.Errorf("TotalBytes = %d, want the sum of emitted Job Meta.Size (%d)", res.TotalBytes, sum)
	}
	if res.TotalBytes != wantBytes || res.Files != wantFiles || len(jobs) != wantFiles {
		t.Errorf("totals = %d files / %d bytes (%d jobs), want %d files / %d bytes",
			res.Files, res.TotalBytes, len(jobs), wantFiles, wantBytes)
	}

	// Count-only walk (nil emit): the totals are a property of the
	// WALK, not of the consumer, and must be identical.
	empty, err := Walk(root, fixtureMatcher(t), nil, nil)
	if err != nil {
		t.Fatalf("Walk (count-only): %v", err)
	}
	if empty.TotalBytes != wantBytes || empty.Files != wantFiles {
		t.Errorf("count-only totals = %d files / %d bytes, want %d files / %d bytes",
			empty.Files, empty.TotalBytes, wantFiles, wantBytes)
	}
}

// --- small local helpers -------------------------------------------------

// containsRel reports whether any job carries the given relpath.
func containsRel(jobs []Job, rel string) bool {
	for _, j := range jobs {
		if j.RelPath == rel {
			return true
		}
	}
	return false
}
