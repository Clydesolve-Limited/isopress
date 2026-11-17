package ingest

// Percent-after-walk gap tests (DESIGN_SPEC 3.6/9.1 amendment,
// stakeholder decision 2026-10-19), added by the verification pass of
// 2026-10-19. walker_test.go's
// TestWalkTotalBytesAndFiles already pins TotalBytes == sum of Job
// Meta.Size on the standard fixture (which includes a pruned subtree,
// two excluded files, two symlinks, and a FIFO); hashpool tests pin
// the per-file ItemDone byte contract. THIS file closes the remaining
// cross-cutting gaps:
//
//  1. TotalBytes exactness on a DEDICATED mixed-noise fixture, with
//     every non-job category arithmetically accounted: pruned dirs
//     (content and an empty dir inside the pruned subtree), excluded
//     files, multiple symlinks (valid + dangling, whose OWN lstat
//     sizes are recorded in the entries but excluded from the byte
//     total), and specials. The exclusion proof does not ride on the
//     standard fixture's byte layout.
//
//  2. The unreadable-file percent edge, end to end across walker +
//     pool + totals event + plain renderer: the totals are lstat-based
//     ESTIMATES (the walk stats every file it counts, readable or
//     not), the completed bytes are read-based REALITY (an open
//     failure yields ItemDone with Bytes 0). The determinate line must
//     undershoot honestly (70% below) with the failed file still
//     counted in BOTH files totals, and must never overshoot 100 in
//     this direction.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"syscall"
	"testing"
	"time"

	"isopress/internal/progress"
)

// buildTotalsFixture builds the dedicated mixed-noise tree:
//
//	root/
//	  keep/a.txt          "aaaaa"        (5 bytes)
//	  keep/b.txt          "bbbbbbb"      (7 bytes)
//	  prune/c.txt         (100 bytes)    <- prune/ is excluded wholesale
//	  prune/empty/        (empty dir INSIDE the pruned subtree)
//	  junk.tmp            (excluded by *.tmp)
//	  ln/a -> ../keep/a.txt              (valid symlink)
//	  ln/b -> nowhere                    (dangling symlink)
//	  special/fifo                       (FIFO)
func buildTotalsFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	for _, d := range []string{"keep", "prune", "prune/empty", "ln", "special"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	write := func(rel, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("keep/a.txt", "aaaaa")
	write("keep/b.txt", "bbbbbbb")
	write("junk.tmp", "excluded")
	if err := os.WriteFile(filepath.Join(root, "prune", "c.txt"), make([]byte, 100), 0o644); err != nil {
		t.Fatalf("write prune/c.txt: %v", err)
	}

	if err := os.Symlink("../keep/a.txt", filepath.Join(root, "ln/a")); err != nil {
		t.Fatalf("symlink ln/a: %v", err)
	}
	if err := os.Symlink("nowhere", filepath.Join(root, "ln/b")); err != nil {
		t.Fatalf("symlink ln/b: %v", err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "special", "fifo"), 0o644); err != nil {
		if errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.EPERM) {
			t.Skipf("mkfifo unsupported in this environment: %v", err)
		}
		t.Fatalf("mkfifo: %v", err)
	}
	return root
}

// linesOf splits rendered output into non-empty lines (the progress
// package's lines() helper is not exported; ingest tests own their
// copy).
func linesOf(s string) []string {
	var out []string
	for _, l := range regexp.MustCompile("\n").Split(s, -1) {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}

// TestWalkTotalBytesExcludesEveryNonJobEntry: the byte total counts
// EXACTLY the emitted jobs' lstat sizes (5 + 7 = 12); every other
// entry category - pruned subtree (100 bytes), excluded file,
// symlinks (whose own lstat sizes ARE len(target), recorded in the
// entries), specials - contributes zero, asserted by exact constant
// and by the per-category counters.
func TestWalkTotalBytesExcludesEveryNonJobEntry(t *testing.T) {
	root := buildTotalsFixture(t)
	m, err := NewMatcher([]string{"*.tmp", "prune/"}, "")
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}

	var jobs []Job
	res, err := Walk(root, m, collectJobs(&jobs), nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if got := jobRelPaths(jobs); !reflect.DeepEqual(got, []string{"keep/a.txt", "keep/b.txt"}) {
		t.Fatalf("job relpaths = %v, want the two keep/ files only", got)
	}
	// The exact pin: only the two included regular files count.
	if res.Files != 2 || res.TotalBytes != 12 {
		t.Errorf("totals = %d files / %d bytes, want 2 files / 12 bytes (5+7; pruned, excluded, symlinked, and special entries contribute nothing)",
			res.Files, res.TotalBytes)
	}

	// Symlink entries carry their own lstat Size (= len(target)) -
	// proof the sizes EXIST to have been counted and were not.
	linkSizes := map[string]int64{}
	for _, s := range res.Symlinks {
		linkSizes[s.RelPath] = s.Meta.Size
	}
	if linkSizes["ln/a"] != int64(len("../keep/a.txt")) || linkSizes["ln/b"] != int64(len("nowhere")) {
		t.Errorf("symlink lstat sizes = %v, want len(target) for both entries", linkSizes)
	}

	// Category counters: one pruned dir, one excluded file, one
	// special, and the pruned subtree's empty dir is NOT an empty-dir
	// entry (out of scope below an exclusion).
	if res.ExcludedDirsPruned != 1 || res.ExcludedFiles != 1 || res.SpecialsSkipped != 1 {
		t.Errorf("category counters = pruned %d, excluded %d, specials %d, want 1/1/1",
			res.ExcludedDirsPruned, res.ExcludedFiles, res.SpecialsSkipped)
	}
	if len(res.EmptyDirs) != 0 {
		t.Errorf("EmptyDirs = %v, want none (the only empty dir is inside the pruned subtree)", emptyDirPaths(res))
	}

	// Count-only walk: identical totals (a property of the walk, not
	// of the consumer).
	empty, err := Walk(root, m, nil, nil)
	if err != nil {
		t.Fatalf("Walk (count-only): %v", err)
	}
	if empty.Files != res.Files || empty.TotalBytes != res.TotalBytes {
		t.Errorf("count-only totals = %d files / %d bytes, want %d / %d",
			empty.Files, empty.TotalBytes, res.Files, res.TotalBytes)
	}
}

// TestHashPhasePercentUndershootsWithUnreadableFile drives the REAL
// percent-after-walk pipeline for the scan command's shape: the walker
// feeds the REAL hash pool (one file's open injected to fail via the
// OpenFile seam), the walk-completion totals event is emitted exactly
// as internal/cli/scan.go's emitScanWalkTotals does (same name,
// TotalFiles from Files, TotalBytes from the walk), and the REAL plain
// renderer renders it. Assertions:
//
//   - the walk's totals are lstat estimates: the unreadable file is
//     counted in full (2 files / 1000 bytes) - the walk cannot know
//     the hash will fail;
//   - the final determinate line is EXACTLY
//     "scan(hash): files 2/2, 700 B/1.0 KB (70%)": the failed file
//     completed as an item (one ItemDone, 0 bytes read), so the files
//     count is whole while the byte percent undershoots honestly;
//   - no rendered percent exceeds 70 (this direction can never
//     overshoot: read bytes <= lstat size).
func TestHashPhasePercentUndershootsWithUnreadableFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "good.bin"), make([]byte, 700), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bad.bin"), make([]byte, 300), 0o644); err != nil {
		t.Fatal(err)
	}
	// The injected unreadable file: lstat succeeds (the walk counts
	// it), open fails (the hash reads nothing).
	bus := progress.NewBus()
	var buf bytes.Buffer
	// Advancing clock: every ItemDone passes the plain cadence
	// throttle (the ingest harness pattern; the renderer goroutine is
	// the only Now caller after Run starts).
	var tick time.Duration
	r := progress.New(progress.Options{
		Mode:   progress.ModePlain,
		Stderr: &buf,
		Now: func() time.Time {
			tick += progress.PlainInterval
			return time.Unix(0, 0).Add(tick)
		},
	})
	done := make(chan progress.Done, 1)
	go func() { done <- r.Run(bus) }()

	pool := NewHashPool(context.Background(), PoolOptions{
		Workers:   2,
		Bus:       bus,
		PhaseName: "scan(hash)",
		OpenFile: func(path string) (io.ReadCloser, error) {
			if filepath.Base(path) == "bad.bin" {
				return nil, errors.New("injected: permission denied")
			}
			return osOpen(path)
		},
	})
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for range pool.Results() {
		}
	}()

	wres, walkErr := Walk(root, Matcher{}, pool.Submit, bus)
	if walkErr != nil {
		t.Fatalf("Walk: %v", walkErr)
	}
	if wres.Files != 2 || wres.TotalBytes != 1000 {
		t.Fatalf("walk totals = %d files / %d bytes, want 2 / 1000 (lstat estimates include the unreadable file)", wres.Files, wres.TotalBytes)
	}
	// The emitScanWalkTotals event shape, at the emitScanWalkTotals
	// point (walk success, no barrier, workers still draining).
	bus.Emit(progress.Phase{
		Name:       "scan(hash)",
		TotalKnown: true,
		TotalFiles: int64(wres.Files),
		TotalBytes: wres.TotalBytes,
	})
	pool.Close()
	if err := pool.Wait(); err != nil {
		t.Fatalf("pool Wait: %v", err)
	}
	bus.Finish(progress.Done{Summary: "scan complete"})
	<-done
	<-drainDone

	got := linesOf(buf.String())
	if len(got) != 3 {
		t.Fatalf("got %d lines, want 3 (two progress + summary):\n%q", len(got), buf.String())
	}
	// The last progress line is deterministic under any interleaving
	// of the two ItemDone events: both have landed by now.
	wantLast := "scan(hash): files 2/2, 700 B/1.0 KB (70%)"
	if got[1] != wantLast {
		t.Errorf("final determinate line = %q, want exact %q (failed file counted in files, its bytes honestly missing from the percent)", got[1], wantLast)
	}
	// The bound, asserted independently: no rendered percent exceeds
	// the honest 70.
	pctRe := regexp.MustCompile(`\((\d+)%\)`)
	for _, l := range got {
		if m := pctRe.FindStringSubmatch(l); m != nil {
			if p, _ := strconv.Atoi(m[1]); p > 70 {
				t.Errorf("line %q claims %d%%, over the honest 70%% bound", l, p)
			}
		}
	}
	if got[2] != "scan complete" {
		t.Errorf("summary line = %q, want %q", got[2], "scan complete")
	}
}
