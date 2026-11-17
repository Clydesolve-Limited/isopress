package cli

// Percent-after-walk acceptance tests through the REAL scan command
// (DESIGN_SPEC 3.6/9.3, stakeholder decision 2026-10-19), added by
// the verification pass of 2026-10-19. scan_totals_test.go pins the
// production emission helper (emitScanWalkTotals) against the real
// interactive renderer; plain_test.go pins both 9.3 windows byte-exact
// against the fake clock. What THIS file adds is the full production
// entry path - the re-exec child running execute() over the real
// command tree with real process streams (the cli suite's established
// pattern) - in the three shapes the unit tests cannot reach:
//
//  1. A LARGE fixture where walk and hash interleave NATURALLY (jobs
//     stream to the pool while the walk is still running, so some
//     hashes complete before the walk's totals exist). The plain
//     renderer's 5s cadence (a documented constant, not a
//     determinism demand this test may make) means a sub-5s scan
//     prints exactly one progress line, and WHETHER that first line
//     lands in the pre-walk or post-walk window is a scheduling
//     race - so the assertion is order-insensitive: every scan(hash)
//     progress line must be coherent (exactly one of the two frozen
//     9.3 forms), and any determinate line must carry the walk's
//     exact totals.
//
//  2. A fixture whose WALK robustly beats its hashes (a handful of
//     large sparse files): the totals event precedes the first
//     ItemDone, so the real command's first printed line is the
//     post-walk determinate form, byte-exact.
//
//  3. A FAILED walk: no totals event, and no totals-shaped artifact
//     anywhere on stderr - the run's only stderr line is the aborted
//     summary (emission-only-on-success, pinned from the output side
//     of the real command).

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"isopress/internal/report"
	"isopress/internal/units"
)

// buildScanPercentFixture creates n small files spread over subdirs
// (dNN/fMMM) and returns the root plus the exact total byte count.
func buildScanPercentFixture(t *testing.T, n int) (root string, total int64) {
	t.Helper()
	root = t.TempDir()
	for i := 0; i < n; i++ {
		content := fmt.Sprintf("payload %04d\n", i)
		p := filepath.Join(root, fmt.Sprintf("d%02d", i%20), fmt.Sprintf("f%04d", i))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", p, err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
		total += int64(len(content))
	}
	return root, total
}

// TestScanE2ELargeFixtureProgressLinesCoherent: 2000 files across 20
// subdirs, hashed while the walk is still submitting (the pool's
// buffered intake starts workers on the first jobs). The scan finishes
// well inside one cadence window, so stderr holds one progress line
// plus the summary; the progress line may legitimately fall in EITHER
// 9.3 window depending on the walk-vs-first-hash scheduling, and both
// forms are asserted coherent rather than forced (see file doc, gap
// 1).
func TestScanE2ELargeFixtureProgressLinesCoherent(t *testing.T) {
	const nFiles = 2000
	root, total := buildScanPercentFixture(t, nFiles)
	cat := filepath.Join(t.TempDir(), "catalog.sqlite")

	stdout, stderr, code := runScanChild(t, "--catalog", cat, "--set-id", "S-PCT-LARGE", root)
	if code != report.ExitOK {
		t.Fatalf("scan exit code = %d, want 0 (stderr: %s)", code, stderr)
	}

	// The end-of-run report (stdout only, FR-046) carries the same
	// totals the walk counted: every file read cleanly, so the
	// read-based report bytes equal the lstat-based walk totals.
	wantReport := fmt.Sprintf("scanned %d entries (%d regular, 0 symlinks), %s hashed",
		nFiles, nFiles, units.FormatBytes(total))
	if !strings.Contains(stdout, wantReport) {
		t.Errorf("stdout lacks the report line %q:\n%s", wantReport, stdout)
	}
	if !strings.Contains(stdout, fmt.Sprintf("added %d,", nFiles)) {
		t.Errorf("stdout does not report all %d files added:\n%s", nFiles, stdout)
	}

	// Every scan(hash) progress line is one of the two frozen 9.3
	// forms; any determinate line carries the walk's EXACT totals.
	determinateSeen := false
	progressLines := 0
	for _, l := range linesOfChild(stderr) {
		if !strings.HasPrefix(l, "scan(hash): files ") {
			continue
		}
		progressLines++
		if !strings.Contains(l, "/") {
			// Counts-only window form: no totals, no percent.
			if strings.Contains(l, "%") {
				t.Errorf("counts-only line carries a percent: %q", l)
			}
			continue
		}
		determinateSeen = true
		wantTotals := fmt.Sprintf("/%d, %s/", nFiles, units.FormatBytes(total))
		if !strings.Contains(l, wantTotals) {
			t.Errorf("determinate line lacks the exact walk totals %q: %q", wantTotals, l)
		}
		m := regexp.MustCompile(`\((\d+)%\)$`).FindStringSubmatch(l)
		if m == nil {
			t.Errorf("determinate line lacks the trailing percent bracket: %q", l)
		} else if p, _ := strconv.Atoi(m[1]); p > 100 {
			t.Errorf("determinate line claims %d%%, over the byte-total bound: %q", p, l)
		}
	}
	if progressLines == 0 {
		t.Errorf("stderr carries no scan(hash) progress line at all:\n%s", stderr)
	}
	if determinateSeen {
		// Documented bonus observation, not required: with the
		// interleave this normally does NOT happen (the first
		// ItemDone usually precedes the walk's totals), but when the
		// walk wins the race the post-walk window is observable even
		// under the 5s cadence - and it was asserted exact above.
		t.Logf("walk completed before the first hash: post-walk window observable in line count %d", progressLines)
	}

	// The final summary is the last stderr line (the cadence never
	// suppresses it, 9.3).
	all := linesOfChild(stderr)
	last := all[len(all)-1]
	if !strings.HasPrefix(last, fmt.Sprintf("scan(hash): files %d, ", nFiles)) ||
		!strings.Contains(last, fmt.Sprintf("; added %d,", nFiles)) {
		t.Errorf("last stderr line = %q, want the end-of-run summary naming %d files added", last, nFiles)
	}
}

// TestScanE2ESingleLargeFileDeterminateFromFirstLine: three 32 MiB
// sparse files - the walk (three lstats) robustly completes long
// before the first sha256 finishes streaming 32 MiB, so the totals
// event precedes the first ItemDone and the real command's FIRST
// printed line is the post-walk determinate form, byte-exact. All
// three files are identical, so the first-completed frame is
// deterministic regardless of worker scheduling.
func TestScanE2ESingleLargeFileDeterminateFromFirstLine(t *testing.T) {
	const (
		nFiles  = 3
		fileLen = int64(32) << 20 // 32 MiB, sparse (holes hash as zeros)
	)
	root := t.TempDir()
	for i := 0; i < nFiles; i++ {
		p := filepath.Join(root, fmt.Sprintf("big%d.bin", i))
		f, err := os.Create(p)
		if err != nil {
			t.Fatalf("create %s: %v", p, err)
		}
		if err := f.Truncate(fileLen); err != nil {
			f.Close()
			t.Fatalf("truncate %s: %v", p, err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("close %s: %v", p, err)
		}
	}
	total := fileLen * nFiles
	cat := filepath.Join(t.TempDir(), "catalog.sqlite")

	stdout, stderr, code := runScanChild(t, "--catalog", cat, "--set-id", "S-PCT-BIG", root)
	if code != report.ExitOK {
		t.Fatalf("scan exit code = %d, want 0 (stderr: %s)", code, stderr)
	}

	wantFirst := fmt.Sprintf("scan(hash): files 1/%d, %s/%s (33%%)",
		nFiles, units.FormatBytes(fileLen), units.FormatBytes(total))
	all := linesOfChild(stderr)
	if len(all) < 2 {
		t.Fatalf("stderr holds %d lines, want at least progress + summary:\n%s", len(all), stderr)
	}
	if all[0] != wantFirst {
		t.Errorf("first stderr line = %q, want the byte-exact determinate window line %q "+
			"(the walk must beat the hashes and the totals event must precede the first ItemDone)", all[0], wantFirst)
	}
	// Exactly one progress line: the rest of the run falls inside the
	// documented 5s cadence window, and the summary closes the output.
	if len(all) != 2 {
		t.Errorf("stderr holds %d lines, want exactly 2 (one cadence-gated progress line + summary):\n%s", len(all), stderr)
	}
	last := all[len(all)-1]
	if !strings.HasPrefix(last, fmt.Sprintf("scan(hash): files %d, %s; added %d,", nFiles, units.FormatBytes(total), nFiles)) {
		t.Errorf("last stderr line = %q, want the end-of-run summary naming %d files and the full byte total", last, nFiles)
	}
	if !strings.Contains(stdout, fmt.Sprintf("scanned %d entries (%d regular, 0 symlinks), %s hashed",
		nFiles, nFiles, units.FormatBytes(total))) {
		t.Errorf("stdout lacks the byte-exact report line:\n%s", stdout)
	}
}

// TestScanE2EFailedWalkEmitsNoTotalsArtifact: a missing root fails the
// walk (SHAPE error, exit 2), and emitScanWalkTotals never runs
// (emission-only-on-success). The plain renderer prints NO progress
// line at all on this path - Phase events render no line and no
// ItemDone ever happens - so nothing totals-shaped survives: the only
// scan(hash)-flavored stderr line is the aborted summary of the
// terminal-error path, alongside execute()'s error print and usage
// hint.
func TestScanE2EFailedWalkEmitsNoTotalsArtifact(t *testing.T) {
	cat := filepath.Join(t.TempDir(), "catalog.sqlite")
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	stdout, stderr, code := runScanChild(t, "--catalog", cat, "--set-id", "S-PCT-FAIL", missing)
	if code != report.ExitUsage {
		t.Fatalf("scan exit code = %d, want %d (usage; stderr: %s)", code, report.ExitUsage, stderr)
	}
	percentRe := regexp.MustCompile(`\(\d+%\)`)
	summarySeen, errorSeen := false, false
	for _, l := range linesOfChild(stderr) {
		// The two frozen progress-line shapes ("scan(hash): files
		// N, ..." and ".../... (N%)") must not appear anywhere: the
		// walk died before any totals existed.
		if strings.HasPrefix(l, "scan(hash): files") || percentRe.MatchString(l) {
			t.Errorf("failed-walk stderr carries a progress/percent artifact line: %q", l)
		}
		if strings.Contains(l, "aborted: 0 of 0 completed files reconciled") {
			summarySeen = true
		}
		if strings.Contains(l, "cannot open scan root") {
			errorSeen = true
		}
	}
	if !summarySeen {
		t.Errorf("stderr lacks the aborted-run summary line:\n%s", stderr)
	}
	if !errorSeen {
		t.Errorf("stderr lacks the scan-root error line:\n%s", stderr)
	}
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("failed walk wrote to stdout (result stream must stay clean, FR-046):\n%s", stdout)
	}
}

// linesOfChild splits captured child stderr into non-empty lines (the
// progress package's lines() helper is not exported to this package).
func linesOfChild(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l != "" {
			out = append(out, l)
		}
	}
	return out
}
