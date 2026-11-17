//go:build perf

package cli

// T6.3 (DESIGN_SPEC 12.8 "Performance smoke tests", quoted verbatim:
// "Bounded-time (seconds) CI checks: walk+hash of a 100k-file tree
// under a fixed wall-clock budget on CI hardware; plan of 1M items
// under budget; these are regression tripwires with generous
// thresholds, not benchmarks." Also IMPLEMENTATION_PLAN 4.7 T6.3 and
// PLAN_SPEC NFR-005 as cited by the delegation).
//
// Build tag: `perf` (PM gating decision in the T6.3 delegation,
// precedent: the e2e and large tags). The timing tripwires must not
// destabilize the default (~46s) or -tags e2e (~94s) gates with load
// sensitivity, so this file is EXCLUDED from both by build tag - not
// by a runtime skip - and runs as its own gate:
//
//	go test -tags perf ./... -timeout 30m
//
// LAYER CHOICE (documented judgment call): the timed phase runs the
// REAL `scan` command through the real CLI seam - a re-exec child
// running execute() over the real command tree with real process
// streams (the cli_test.go runCLIChild pattern the e2e tests use) -
// over the generated tree into a REAL, freshly created catalog. That
// is the whole 12.8-relevant surface end to end: walker + exclusion
// matcher + hashpool + failure classification + reconciler + catalog
// batch inserts + runs bookkeeping. A narrower ingest-level call
// (walker+hashpool directly) was rejected: it would skip the
// reconciler and the catalog write path, which is exactly where a
// walk+hash regression would hide, and the brief allows the full
// command path, which is both practical here and the most faithful.
//
// FIXTURE DISCIPLINE (DESIGN_SPEC 12.2; T6.3 acceptance): the tree is
// generated ON THE FLY into t.TempDir() - removed by the testing
// package when the test completes - and NEVER committed. Generation
// is PARALLELIZED (a worker pool, capped) and happens OUTSIDE the
// timed window: time.Since wraps ONLY the child scan run. The test
// asserts the fixture root does not live inside the repository working
// tree, so a future refactor cannot silently start dropping 100k-file
// trees into version control.
//
// FAILURE MODES (bounded-time contract): the child runs under
// exec.CommandContext with walkHashBudget as its deadline, so a hang
// or an over-budget scan is KILLED at the budget and the test FAILS
// with the observed evidence (elapsed >= budget, stderr tail) instead
// of blocking the suite; go test's own -timeout is the backstop
// behind that. A successful run must also prove it did the full
// workload: the frozen scan report must state scanned == regular ==
// perfTreeFiles (work evidence, the T6.2 discipline), otherwise the
// budget would be measuring an accidentally-empty scan.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"isopress/internal/report"
)

const (
	// perfTreeFiles is the DESIGN_SPEC 12.8 walk+hash smoke size: a
	// 100k-file tree ("walk+hash of a 100k-file tree").
	perfTreeFiles = 100_000

	// perfTreeDirs splits the tree across 100 directories (1,000 files
	// each): one flat 100k-entry directory would measure readdir over
	// a single inode rather than the realistic nested walk the walker
	// is built for, and it keeps per-directory costs honest.
	perfTreeDirs = 100

	// perfTreeFileBytes: ~1 KiB deterministic payloads - small enough
	// that fixture generation stays outside the interesting time
	// budget, large enough that SHA-256 hashes real bytes rather than
	// empty files (the hashpool is part of the measured surface).
	perfTreeFileBytes = 1024

	// perfScanSetID is the scanned set (6.4 naming is scan's own when
	// --set-id is absent; the smoke pins it explicitly so the work-
	// evidence query is stable).
	perfScanSetID = "PERF-WALK-100K"

	// walkHashBudget is the wall-clock budget of the TIMED walk+hash
	// phase (12.8: "under a fixed wall-clock budget"; "regression
	// tripwires with generous thresholds, not benchmarks").
	//
	// RATIONALE (measured on the QA machine, go1.26.10, Linux, tmpfs
	// t.TempDir):
	//
	//	observed local walk+hash of the generated 100k-file tree:
	//	4.73 s (the test log's "timed scan"; fixture generation
	//	0.43 s is outside the window);
	//	multiplier applied: 40 s ~= 8.5x the observed local time
	//	(the brief's 5-10x generosity band);
	//	CI-hardness argument: CI runners are routinely 3-4x slower
	//	this class of local box (noisy neighbours, slower page
	//	cache, fewer guaranteed cores) - a healthy CI run then lands
	//	at ~15-19 s, still >2x under budget. The tripwire fails
	//	loudly on an ORDER-OF-MAGNITUDE walk+hash regression (10x =
	//	47 s locally, ~140 s on CI - over budget in both places),
	//	which is the class of bug this smoke exists to catch, while
	//	never becoming a benchmark that ranks machines.
	//
	// The child is killed AT this deadline (exec.CommandContext), so
	// a hang costs the budget, never the suite.
	walkHashBudget = 40 * time.Second

	// walkHashGenWorkers caps the fixture-generation worker pool.
	// Generation is NOT the measured surface; the cap keeps it from
	// stealing the whole box (and its page cache) from the timed
	// phase and from other tests on a busy CI runner.
	walkHashGenWorkers = 16
)

// perfRelPath returns the deterministic slash-separated relpath of
// file i: round-robin across the perfTreeDirs directories (parallel
// workers therefore touch different directories instead of piling
// onto one), with a per-directory file index.
func perfRelPath(i, dirs int) string {
	return fmt.Sprintf("d%03d/f%06d", i%dirs, i/dirs)
}

// genPerfTree builds the 100k-file fixture tree under root with a
// parallel worker pool and returns the generation duration. It is
// called OUTSIDE every timed window (T6.3: the budget measures
// walk+hash itself, never fixture construction). Errors from workers
// are collected and reported from the test goroutine (t.Fatal must
// not be called from a worker).
func genPerfTree(t *testing.T, root string, nfiles, ndirs int) time.Duration {
	t.Helper()
	if nfiles%ndirs != 0 {
		t.Fatalf("perf fixture: nfiles %d must be a multiple of ndirs %d", nfiles, ndirs)
	}
	genStart := time.Now()

	// Directories are pre-created sequentially (100 mkdirs); the pool
	// then only creates files.
	for d := 0; d < ndirs; d++ {
		if err := os.MkdirAll(filepath.Join(root, fmt.Sprintf("d%03d", d)), 0o755); err != nil {
			t.Fatalf("perf fixture: mkdir d%03d: %v", d, err)
		}
	}

	workers := runtime.NumCPU()
	if workers > walkHashGenWorkers {
		workers = walkHashGenWorkers
	}
	var (
		next     atomic.Int64
		mu       sync.Mutex
		firstErr error
	)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= nfiles {
					return
				}
				rel := perfRelPath(i, ndirs)
				content := make([]byte, perfTreeFileBytes)
				for j := range content {
					content[j] = byte((i*31 + j) % 251)
				}
				if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), content, 0o644); err != nil {
					mu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("write %s: %w", rel, err)
					}
					mu.Unlock()
					return
				}
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		t.Fatalf("perf fixture generation failed after %v: %v", time.Since(genStart), firstErr)
	}
	return time.Since(genStart)
}

// runPerfCLIChild is runCLIChild with a DEADLINE: the child runs under
// exec.CommandContext, so a scan that hangs or overruns walkHashBudget
// is killed at the deadline instead of blocking the suite (the T6.3
// bounded-time contract; go test's -timeout is the backstop behind
// this). killed=true means the deadline fired (exit code is meaningless
// then).
func runPerfCLIChild(t *testing.T, ctx context.Context, args ...string) (stdout, stderr string, code int, killed bool) {
	t.Helper()
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	cmd.Env = append(os.Environ(), cliHelperEnv+"=1")
	outBuf, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = outBuf, errBuf
	err := cmd.Run()
	if ctx.Err() != nil {
		return outBuf.String(), errBuf.String(), -1, true
	}
	code = report.ExitOK
	var exitErr *exec.ExitError
	if err != nil {
		if !errors.As(err, &exitErr) {
			t.Fatalf("running perf CLI child %v: %v (stderr: %s)", args, err, errBuf.String())
		}
		code = exitErr.ExitCode()
	}
	return outBuf.String(), errBuf.String(), code, false
}

// tailOf truncates s to its last n bytes for failure messages (a
// 100k-file scan's stderr must not flood a failure report).
func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

// TestPerfScan100kWalkHashWithinBudget is the 12.8 walk+hash smoke:
// walk + SHA-256 hash of a generated 100k-file tree, end to end
// through the real scan command, inside walkHashBudget. See the file
// doc for the layer choice, fixture discipline, and failure modes.
func TestPerfScan100kWalkHashWithinBudget(t *testing.T) {
	root := t.TempDir()

	// 12.2 hygiene tripwire: fixtures live in t.TempDir() only. cwd is
	// this package's source directory, inside the repository, so a
	// fixture root under cwd would mean generated trees are landing in
	// the working tree (where they could enter version control).
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if strings.HasPrefix(root+string(filepath.Separator), cwd+string(filepath.Separator)) {
		t.Fatalf("perf fixture root %q is inside the repository %q: generated trees must live under t.TempDir(), never the working tree (DESIGN_SPEC 12.2)", root, cwd)
	}

	// Fixture generation - OUTSIDE the timed window.
	genDur := genPerfTree(t, root, perfTreeFiles, perfTreeDirs)
	catPath := filepath.Join(t.TempDir(), "perf-walk.sqlite")

	// The timed phase: the REAL scan command, deadline = the budget.
	ctx, cancel := context.WithTimeout(context.Background(), walkHashBudget)
	defer cancel()
	start := time.Now()
	stdout, stderr, code, killed := runPerfCLIChild(t, ctx,
		"scan", "--catalog", catPath, "--set-id", perfScanSetID, "--json", root)
	elapsed := time.Since(start)

	t.Logf("perf walk+hash: fixture generation %v (outside budget); timed scan %v (walkHashBudget %v)",
		genDur, elapsed, walkHashBudget)

	if killed {
		t.Fatalf("walk+hash of %d files did not finish within walkHashBudget=%v (child killed at the deadline, elapsed >= %v). "+
			"A hang here is the bounded-time failure the smoke exists to catch; stderr tail: %s",
			perfTreeFiles, walkHashBudget, elapsed, tailOf(stderr, 2000))
	}
	if code != report.ExitOK {
		t.Fatalf("scan exit code = %d, want 0 (stderr: %s)", code, tailOf(stderr, 2000))
	}
	if elapsed > walkHashBudget {
		t.Fatalf("walk+hash elapsed %v exceeds walkHashBudget=%v", elapsed, walkHashBudget)
	}

	// Work evidence: the budget is only meaningful if the scan really
	// hashed every file. The frozen scan report (9.3/FR-046, one JSON
	// document on stdout) carries the counts.
	var rep scanReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("scan --json stdout is not one scanReport document: %v\nstdout: %s", err, tailOf(stdout, 2000))
	}
	if rep.Scanned != perfTreeFiles || rep.Regular != perfTreeFiles {
		t.Fatalf("scan reported scanned=%d regular=%d, want %d/%d - the timed phase must do the full 12.8 workload",
			rep.Scanned, rep.Regular, perfTreeFiles, perfTreeFiles)
	}
}
