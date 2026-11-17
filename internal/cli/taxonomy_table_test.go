package cli

// T6.1 error-taxonomy sweep (DESIGN_SPEC 10.1/10.2/10.3, PLAN_SPEC
// NFR-003): a single TABLE test asserting one real command invocation
// per (command x taxonomy class) cell that is cheaply reachable in the
// DEFAULT suite - exit code AND that stderr names the cause. The heavy
// per-command item-failure and interruption fixtures keep living in
// their own suites (they are pinned there end to end); this table is
// the sweep's at-a-glance coverage map, so every row documents where
// its classification happens and what already covered the class.
//
// The sweep's audit outcome, per class:
//
//   - UsageError (2): every command's Args validator goes through the
//     usageArgs wrap (the T0.5 N2 debt - CLOSED); flag-parse failures
//     are typed via SetFlagErrorFunc; unknown subcommands and (since
//     this task) unknown help topics are typed at the root.
//   - CatalogError (4): every catalog open failure surfaces the
//     catalog package's typed CatalogError/CatalogBusy unchanged.
//   - CatalogBusy (5): the mutating opens only; read-only commands
//     (report/verify-media/validate, and restore's read path) take no
//     flock and can never produce the class.
//   - DependencyError (3): build/verify-media/restore probe xorriso
//     through the T4.1 seam only when an ISO is in scope.
//   - ItemFailures (10): per-command finalize (scan fails.Err(),
//     copy/build/verify-media/validate/restore ItemFailures carriers).
//   - Interrupted (130): the commands' own finalize decisions
//     (copy/build/scan) or execute's context override (plan/report/
//     clear and any residual cancellation-derived error), with the
//     original chain preserved since T6.1 (NB N1).
//   - fatal (1): the Classify default, plus the T6.1 --verbose stack
//     dump (TestFatalUnexpectedExit1VerboseStackDump).
//
// The sweep's UNREACHABLE cells (audited T6.1, each with a structural
// reason - a new code path that makes one reachable must fail HERE
// first):
//
//   - DependencyError (3) x {scan, plan, report, clear, copy}: xorriso
//     is never invoked by these commands (copy mode never touches
//     images, NFR-004); build/verify-media/restore cells live in
//     their suites (TestTaxonomyDependencyClassBuild,
//     verifymedia_adversarial_e2e_test.go, restore_frommedia_test.go).
//   - CatalogBusy (5) x {report, validate, verify-media, restore}:
//     read-only opens take no flock (2.1 Option B) - probed below
//     with the flock HELD, so any 5 fails the table.
//
// Cells deliberately NOT in the table because a stronger suite owns
// them (a removal here loses nothing):
//
//   - Interrupted (130) x {plan, report, clear, copy, build, verify-
//     media, validate, restore}: the no-debris sweep and the
//     signal-after-completion pins live in
//     interrupt_shape_matrix_test.go; the real-signal shapes live in
//     the per-command suites (scan_abort_e2e_test.go, copy_test.go,
//     interrupt_shapes_e2e_test.go). The table carries the
//     representative scan cell.
//   - ItemFailures (10): per-command carriers pinned in their own
//     suites (scan and copy at the table level).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"isopress/internal/catalog"
	"isopress/internal/report"
)

// TestTaxonomyCoverageTable drives the REAL command tree (in-process
// execute seam) through one invocation per reachable class and asserts
// the 10.2 exit code plus a stderr cause fragment per row.
func TestTaxonomyCoverageTable(t *testing.T) {
	tmp := t.TempDir()
	missing := filepath.Join(tmp, "missing.sqlite")
	dest := filepath.Join(tmp, "dest")
	fixtureRoot := makeFixture(t)
	lockedCat := filepath.Join(tmp, "held.sqlite")
	if err := catalog.Create(lockedCat); err != nil {
		t.Fatalf("create held catalog: %v", err)
	}

	type cell struct {
		name       string
		args       []string
		wantCode   int
		wantStderr string // substring that names the cause
	}

	// --- UsageError rows (exit 2): stray args on every command (the
	// N2 usageArgs convention), unknown subcommand, unknown help
	// topic, flag-parse failure, and each command's required-flag
	// check.
	usage := []cell{
		{"scan/stray-args", []string{"scan", fixtureRoot, "extra"}, report.ExitUsage, "usage"},
		{"plan/stray-args", []string{"plan", "stray"}, report.ExitUsage, "usage"},
		{"report/stray-args", []string{"report", "stray"}, report.ExitUsage, "usage"},
		{"clear/stray-args", []string{"clear-assignments", "stray"}, report.ExitUsage, "usage"},
		{"copy/stray-args", []string{"copy", "stray"}, report.ExitUsage, "usage"},
		{"build/stray-args", []string{"build", "stray"}, report.ExitUsage, "usage"},
		{"verify-media/stray-args", []string{"verify-media", "stray"}, report.ExitUsage, "usage"},
		{"validate/stray-args", []string{"validate", "stray"}, report.ExitUsage, "usage"},
		{"restore/stray-args", []string{"restore", "stray"}, report.ExitUsage, "usage"},
		{"root/unknown-subcommand", []string{"frobnicate"}, report.ExitUsage, "unknown command"},
		{"root/unknown-help-topic", []string{"help", "frobnicate"}, report.ExitUsage, "unknown help topic"},
		{"root/flag-parse", []string{"scan", "--bogus-flag"}, report.ExitUsage, "bogus-flag"},
		{"scan/missing-catalog-flag", []string{"scan", fixtureRoot}, report.ExitUsage, "--catalog is required"},
		{"plan/missing-size", []string{"plan"}, report.ExitUsage, "--size is required"},
		{"report/missing-size", []string{"report"}, report.ExitUsage, "--size is required"},
		// Each command checks --catalog before --set-id, so the later
		// usage gates are probed with --catalog present.
		{"clear/missing-set-id", []string{"clear-assignments", "--catalog", missing}, report.ExitUsage, "--set-id is required"},
		{"copy/missing-set-id", []string{"copy", "--catalog", missing}, report.ExitUsage, "--set-id is required"},
		{"build/missing-set-id", []string{"build", "--catalog", missing}, report.ExitUsage, "--set-id is required"},
		{"verify-media/missing-set-id", []string{"verify-media", "--catalog", missing}, report.ExitUsage, "--set-id is required"},
		{"validate/missing-set-id", []string{"validate", "--catalog", missing}, report.ExitUsage, "--set-id is required"},
		{"restore/missing-target", []string{"restore", "--catalog", missing, "--set-id", "S"}, report.ExitUsage, "--target is required"},
	}

	// --- CatalogError rows (exit 4): a missing catalog file for every
	// command that does NOT auto-create (everything except scan - the
	// PM-ratified scan auto-create path instead fails with a
	// CatalogError when the catalog directory itself is unusable).
	catalog4 := []cell{
		{"plan/missing-catalog", []string{"plan", "--catalog", missing, "--set-id", "S", "--size", "25"}, report.ExitCatalog, "catalog"},
		{"report/missing-catalog", []string{"report", "--catalog", missing, "--set-id", "S", "--size", "25"}, report.ExitCatalog, "catalog"},
		{"clear/missing-catalog", []string{"clear-assignments", "--catalog", missing, "--set-id", "S"}, report.ExitCatalog, "catalog"},
		{"copy/missing-catalog", []string{"copy", "--catalog", missing, "--set-id", "S", "--dest", dest}, report.ExitCatalog, "catalog"},
		{"build/missing-catalog", []string{"build", "--catalog", missing, "--set-id", "S", "--dest", dest}, report.ExitCatalog, "catalog"},
		{"verify-media/missing-catalog", []string{"verify-media", "--catalog", missing, "--set-id", "S"}, report.ExitCatalog, "catalog"},
		{"validate/missing-catalog", []string{"validate", "--catalog", missing, "--set-id", "S"}, report.ExitCatalog, "catalog"},
		{"restore/missing-catalog", []string{"restore", "--catalog", missing, "--set-id", "S", "--target", filepath.Join(tmp, "out")}, report.ExitCatalog, "catalog"},
		// scan auto-creates, but a --catalog path whose PARENT is a
		// regular file cannot be created: the Create failure is a
		// KindOpen CatalogError (openScanCatalog -> catalog.Create).
		{"scan/uncreatable-catalog", []string{"scan", "--catalog", filepath.Join(missing, "cat.sqlite"), fixtureRoot}, report.ExitCatalog, "catalog"},
	}

	// --- CatalogError rows (exit 4), the CORRUPT-catalog half: every
	// command's open funnels through catalog.Open's typed detection
	// (schema/corruption), so a corrupt file is classified exactly
	// like the missing one - swept per command so no open path can
	// regress to a raw driver error (which would classify 1).
	corrupt := filepath.Join(tmp, "corrupt.sqlite")
	if err := os.WriteFile(corrupt, []byte("garbage, not a sqlite database\n"), 0o644); err != nil {
		t.Fatalf("write corrupt catalog: %v", err)
	}
	corrupt4 := []cell{
		{"scan/corrupt-catalog", []string{"scan", "--catalog", corrupt, fixtureRoot}, report.ExitCatalog, "corrupt"},
		{"plan/corrupt-catalog", []string{"plan", "--catalog", corrupt, "--set-id", "S", "--size", "25"}, report.ExitCatalog, "corrupt"},
		{"report/corrupt-catalog", []string{"report", "--catalog", corrupt, "--set-id", "S", "--size", "25"}, report.ExitCatalog, "corrupt"},
		{"clear/corrupt-catalog", []string{"clear-assignments", "--catalog", corrupt, "--set-id", "S"}, report.ExitCatalog, "corrupt"},
		{"copy/corrupt-catalog", []string{"copy", "--catalog", corrupt, "--set-id", "S", "--dest", dest}, report.ExitCatalog, "corrupt"},
		{"build/corrupt-catalog", []string{"build", "--catalog", corrupt, "--set-id", "S", "--dest", dest}, report.ExitCatalog, "corrupt"},
		{"verify-media/corrupt-catalog", []string{"verify-media", "--catalog", corrupt, "--set-id", "S"}, report.ExitCatalog, "corrupt"},
		{"validate/corrupt-catalog", []string{"validate", "--catalog", corrupt, "--set-id", "S"}, report.ExitCatalog, "corrupt"},
		{"restore/corrupt-catalog", []string{"restore", "--catalog", corrupt, "--set-id", "S", "--target", filepath.Join(tmp, "out")}, report.ExitCatalog, "corrupt"},
	}

	cells := append(usage, catalog4...)
	cells = append(cells, corrupt4...)

	// --- CatalogBusy rows (exit 5): every MUTATING command with the
	// exclusive flock held by this very process (in-process
	// acquisition on a second descriptor - the documented Linux flock
	// semantics the CatalogBusy godoc names). The read-only commands
	// are asserted NOT-busy at the bottom of the test.
	hold, err := catalog.Open(lockedCat, catalog.OpenReadWrite)
	if err != nil {
		t.Fatalf("opening the lock holder: %v", err)
	}
	defer hold.Close()
	busy := []cell{
		{"scan/busy", []string{"scan", "--catalog", lockedCat, fixtureRoot}, report.ExitCatalogBusy, "busy"},
		{"plan/busy", []string{"plan", "--catalog", lockedCat, "--set-id", "S", "--size", "25"}, report.ExitCatalogBusy, "busy"},
		{"clear/busy", []string{"clear-assignments", "--catalog", lockedCat, "--set-id", "S"}, report.ExitCatalogBusy, "busy"},
		{"copy/busy", []string{"copy", "--catalog", lockedCat, "--set-id", "S", "--dest", dest}, report.ExitCatalogBusy, "busy"},
		{"build/busy", []string{"build", "--catalog", lockedCat, "--set-id", "S", "--dest", dest}, report.ExitCatalogBusy, "busy"},
	}
	cells = append(cells, busy...)

	// --- Interrupted rows (exit 130): a PRE-CANCELLED run context
	// through the real tree. scan is the representative mutating
	// command (copy/build/scan classify explicitly in their finalize;
	// plan/report/clear fall to execute's context override, pinned by
	// cli_test.go's Execute-cancellation tests).
	interruptedCtx, cancel := context.WithCancel(context.Background())
	cancel()
	interruptCells := []cell{
		{"scan/signal-cancelled", []string{"scan", "--catalog", filepath.Join(tmp, "interrupt.sqlite"), fixtureRoot}, report.ExitInterrupted, "interrupted by signal"},
	}
	for _, c := range interruptCells {
		t.Run(c.name, func(t *testing.T) {
			root := NewRootCmd()
			errBuf := &bytes.Buffer{}
			code := execute(interruptedCtx, root, c.args, errBuf)
			if code != c.wantCode {
				t.Errorf("exit = %d, want %d\nstderr: %s", code, c.wantCode, errBuf.String())
			}
			if !strings.Contains(errBuf.String(), c.wantStderr) {
				t.Errorf("stderr lacks the cause %q:\n%s", c.wantStderr, errBuf.String())
			}
		})
	}

	// --- the in-process table rows (usage + catalog + busy) ---
	for _, c := range cells {
		t.Run(c.name, func(t *testing.T) {
			_, stderr, code := runInProcess(t, c.args...)
			if code != c.wantCode {
				t.Errorf("exit = %d, want %d\nstderr: %s", code, c.wantCode, stderr)
			}
			if !strings.Contains(stderr, c.wantStderr) {
				t.Errorf("stderr lacks the cause %q:\n%s", c.wantStderr, stderr)
			}
		})
	}

	// --- report can NEVER be busy (exit 5 is structurally unreachable:
	// read-only opens take no flock, DESIGN_SPEC 2.1 Option B). With
	// the flock held above and no scanned set, report fails on the
	// set-existence usage gate instead - any 5 here would mean a
	// reader took the lock.
	_, stderr, code := runInProcess(t, "report", "--catalog", lockedCat, "--set-id", "S", "--size", "25")
	if code == report.ExitCatalogBusy {
		t.Errorf("report exited 5 with the flock held; read-only opens must take no flock (stderr: %s)", stderr)
	}
	if code != report.ExitUsage {
		t.Errorf("report exit = %d, want 2 (the never-scanned-set usage gate)\nstderr: %s", code, stderr)
	}
	// The same never-busy contract for the other three read-only
	// commands (validate and verify-media open read-only per 2.1
	// Option B; restore's main path likewise - its media-path hash
	// fill writes through a deliberately FLOCK-FREE locker,
	// verifymedia.go/restore_media.go, so busy stays unreachable for
	// the whole command). A 5 on any of these would mean a reader
	// took the advisory lock.
	for _, probe := range []struct {
		name string
		args []string
	}{
		{"validate", []string{"validate", "--catalog", lockedCat, "--set-id", "S"}},
		{"verify-media", []string{"verify-media", "--catalog", lockedCat, "--set-id", "S"}},
		{"restore", []string{"restore", "--catalog", lockedCat, "--set-id", "S", "--target", filepath.Join(tmp, "out")}},
	} {
		_, stderr, code := runInProcess(t, probe.args...)
		if code == report.ExitCatalogBusy {
			t.Errorf("%s exited 5 with the flock held; read-only opens must take no flock (stderr: %s)", probe.name, stderr)
		}
		if code != report.ExitUsage {
			t.Errorf("%s exit = %d, want 2 (the never-scanned-set usage gate)\nstderr: %s", probe.name, code, stderr)
		}
	}
}

// TestHelpTopicAndCompletionBlastRadius pins the N7 custom-help
// command's full blast radius: every legitimate help behavior stays
// exit 0 with REAL help (root help for no args, the named command's
// help for a known topic), the unknown topic is a typed usage error
// (the table row above), and cobra's hidden __complete machinery is
// undisturbed - shell completion of the command list and of help
// topics keeps working (a regression here would silently break every
// shell user's TAB).
func TestHelpTopicAndCompletionBlastRadius(t *testing.T) {
	// bare help -> the root help, exit 0.
	stdout, stderr, code := runInProcess(t, "help")
	if code != report.ExitOK {
		t.Fatalf("help exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "Available Commands") || !strings.Contains(stdout, "scan") {
		t.Errorf("bare help does not carry the root help:\n%s", stdout)
	}

	// help <known> -> that command's REAL help, exit 0.
	stdout, stderr, code = runInProcess(t, "help", "scan")
	if code != report.ExitOK {
		t.Fatalf("help scan exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "isopress scan") || !strings.Contains(stdout, "--catalog") {
		t.Errorf("help scan does not carry scan's own help:\n%s", stdout)
	}

	// help <bogus> -> typed usage error, exit 2 (the N7 fix; the
	// table cell above pins the class and the cause wording).
	_, stderr, code = runInProcess(t, "help", "frobnicate")
	if code != report.ExitUsage {
		t.Fatalf("help frobnicate exit = %d, want 2 (stderr: %s)", code, stderr)
	}

	// __complete of the root (the shell passes the word being
	// completed as the final argument): the command list still
	// completes, exit 0 with the NoFileComp directive - and the
	// custom help command is itself a candidate (the N7 rewrite
	// registers it like any other command).
	stdout, stderr, code = runInProcess(t, "__complete", "")
	if code != report.ExitOK {
		t.Fatalf("__complete root exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	for _, cand := range []string{"scan", "copy", "restore", "help"} {
		if !strings.Contains(stdout, cand+"\t") && !strings.Contains(stdout, cand+"\n") {
			t.Errorf("__complete root output lacks the %q candidate:\n%s\nstderr: %s", cand, stdout, stderr)
		}
	}
	if !strings.Contains(stdout, ":4") { // ShellCompDirectiveNoFileComp
		t.Errorf("__complete root output lacks the NoFileComp directive line:\n%s", stdout)
	}

	// __complete help <TAB>: DELIBERATE CHANGE (T6.2 addendum F2, the
	// reviewer's one-liner): the custom ValidArgsFunction now offers
	// command-name candidates, cobra's built-in parity that the N7
	// rewrite dropped. The T6.1-era pin recorded the then-honest
	// no-candidates behavior "so any future change is a deliberate
	// one, not an accident of the N7 rewrite" - this IS that deliberate
	// change: root.go's help command mirrors the builtin (command
	// names of the topic typed so far, prefix-filtered), and
	// help_parity_test.go pins the richer candidate/prefix contract.
	stdout, stderr, code = runInProcess(t, "__complete", "help", "")
	if code != report.ExitOK {
		t.Fatalf("__complete help exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, ":4") { // ShellCompDirectiveNoFileComp
		t.Errorf("__complete help lacks the NoFileComp directive line:\nstdout: %s\nstderr: %s", stdout, stderr)
	}
	for _, cand := range []string{"scan", "copy"} {
		if !strings.Contains(stdout, cand+"\t") && !strings.Contains(stdout, cand+"\n") {
			t.Errorf("__complete help lacks the %q candidate (F2 cobra parity):\nstdout: %s\nstderr: %s",
				cand, stdout, stderr)
		}
	}
}

// TestTaxonomyDependencyClassBuild pins the DependencyError row (exit 3)
// at the table level: with xorriso absent from PATH, build fails
// through the T4.1 seam BEFORE any run row is minted. (verify-media and
// restore pay the same seam only when an ISO is in scope; those paths
// are pinned in their own suites - verifymedia_adversarial_e2e_test.go
// and the restore supply tests.)
func TestTaxonomyDependencyClassBuild(t *testing.T) {
	buildRoot := makeBuildSingleImageTree(t)
	cat := createCatalog(t)
	scanBuildFixture(t, buildRoot, cat, "S-TAXDEP")
	planSynthetic(t, cat, "S-TAXDEP", buildFixtureProfile)

	stdout, stderr, code := runBuildChildEnv(t,
		[]string{"PATH=" + t.TempDir()},
		"build", "--catalog", cat, "--set-id", "S-TAXDEP", "--dest", filepath.Join(t.TempDir(), "dest"))
	if code != report.ExitDependency {
		t.Fatalf("exit = %d, want 3 (dependency)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "xorriso") {
		t.Errorf("stderr does not name the missing tool:\n%s", stderr)
	}
}

// TestTaxonomyItemFailuresClassScan pins the ItemFailures row (exit 10)
// at the table level through the real command: one unreadable source
// file never aborts the run, is enumerated on stdout, and exits 10
// (the per-command carriers for copy/build/verify-media/validate/
// restore are pinned in their own suites).
func TestTaxonomyItemFailuresClassScan(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 000 does not make files unreadable")
	}
	root := makeFixture(t)
	locked := filepath.Join(root, "locked.dat")
	if err := os.WriteFile(locked, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write locked.dat: %v", err)
	}
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod 000: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o644) })

	cat := createCatalog(t)
	args := []string{"--catalog", cat, "--set-id", "S-TAX10"}
	for _, p := range fixtureExcludes() {
		args = append(args, "--exclude", p)
	}
	stdout, stderr, code := runScanChild(t, append(args, root)...)
	if code != report.ExitItemFailures {
		t.Fatalf("exit = %d, want 10 (item failures)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "unreadable") {
		t.Errorf("stdout report does not enumerate the failure:\n%s", stdout)
	}
	if !strings.Contains(stderr, "item failure") {
		t.Errorf("stderr does not name the class:\n%s", stderr)
	}
}

// TestFatalUnexpectedExit1VerboseStackDump pins the fatal-unexpected
// row (exit 1) AND the T6.1 --verbose stack-trace clause of
// DESIGN_SPEC 10.2 ("Fatal unexpected error (bug class; stack trace
// under --verbose)"). The failure is produced by the REAL command tree
// through the copy 12.7 env seam (an untyped terminal error - the bug
// class by construction): without --verbose stderr carries only the
// error line; with --verbose it additionally carries the goroutine
// stack dump.
func TestFatalUnexpectedExit1VerboseStackDump(t *testing.T) {
	root := makeFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-TAXFATAL")
	if _, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", "S-TAXFATAL", "--size", planFixtureSize); code != report.ExitOK {
		t.Fatalf("plan fixture exit = %d, want 0\nstderr: %s", code, stderr)
	}
	dest := filepath.Join(t.TempDir(), "dest")

	run := func(extraArgs ...string) (string, string, int) {
		t.Helper()
		args := append(extraArgs, "copy", "--catalog", cat,
			"--set-id", "S-TAXFATAL", "--dest", dest)
		cmd := exec.Command(os.Args[0], args...)
		cmd.Env = append(os.Environ(), cliHelperEnv+"=1", "ISOPRESS_TEST_COPY_FAIL_AFTER=1")
		outBuf, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
		cmd.Stdout, cmd.Stderr = outBuf, errBuf
		code := report.ExitOK
		if err := cmd.Run(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("running CLI child %v: %v (stderr: %s)", args, err, errBuf.String())
			}
			code = exitErr.ExitCode()
		}
		return outBuf.String(), errBuf.String(), code
	}

	stdout, stderr, code := run()
	if code != report.ExitFatal {
		t.Fatalf("exit = %d, want 1 (fatal unexpected)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "isopress: error:") {
		t.Errorf("stderr lacks the error line:\n%s", stderr)
	}
	if strings.Contains(stderr, "stack dump") {
		t.Errorf("a non-verbose run must not print the stack dump:\n%s", stderr)
	}
	if strings.Contains(stdout, "stack dump") || strings.Contains(stdout, "goroutine ") {
		t.Errorf("the dump must never leak to stdout (machine-readable results only, FR-046):\n%s", stdout)
	}

	stdout, stderr, code = run("--verbose")
	if code != report.ExitFatal {
		t.Fatalf("--verbose exit = %d, want 1\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "goroutine stack dump (--verbose)") {
		t.Errorf("stderr lacks the --verbose stack-dump header:\n%s", stderr)
	}
	if !strings.Contains(stderr, "goroutine ") {
		t.Errorf("stderr lacks an actual goroutine stack:\n%s", stderr)
	}
	// THE STREAM-SIDE OF THE STACK-DUMP CONTRACT: the dump is a
	// stderr-only diagnostic (10.3: verbose transcripts on stderr);
	// stdout stays machine-parseable even under --verbose.
	if strings.Contains(stdout, "stack dump") || strings.Contains(stdout, "goroutine ") {
		t.Errorf("the --verbose stack dump leaked to stdout (FR-046):\n%s", stdout)
	}
}

// TestInterruptOverridePreservesOriginalChain pins the T6.1 NB N1 fix
// at the unit level: when the context override fires (a signal-cancelled
// run whose command returned a non-interrupt error), the ORIGINAL error
// chain survives - stderr names what was actually interrupted, not just
// the interrupt wrapper (the pre-T6.1 override discarded the cause).
func TestInterruptOverridePreservesOriginalChain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the "signal" has already fired before the command starts

	// scan with a pre-cancelled context: the pipeline aborts with a
	// cancellation-derived walk/pool error (a plain, fatal-class error
	// by Classify), so execute's override fires and must preserve it.
	cat := filepath.Join(t.TempDir(), "chain.sqlite")
	errBuf := &bytes.Buffer{}
	code := execute(ctx, NewRootCmd(), []string{"scan", "--catalog", cat, makeFixture(t)}, errBuf)
	if code != report.ExitInterrupted {
		t.Fatalf("exit = %d, want 130\nstderr: %s", code, errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "interrupted by signal") {
		t.Errorf("stderr lacks the interrupt class wording:\n%s", errBuf.String())
	}
	if !strings.Contains(errBuf.String(), "context canceled") {
		t.Errorf("stderr lost the ORIGINAL interrupted cause (NB N1 regression):\n%s", errBuf.String())
	}
}

// TestWriteReportFailAfterCommitHonesty pins the T6.1 fold of the
// deferred shared fix (TODO: "writeReport-fail-after-commit leaves
// stale 'aborted' Done line in clear/plan/report"): when the stdout
// report write fails AFTER the operation committed (clear's
// transaction, plan's assignment transaction) or after the read-only
// projection was computed (report) or after the reconciliation
// completed (scan), the stderr Done line must state what actually
// happened and must NOT claim the operation aborted.
func TestWriteReportFailAfterCommitHonesty(t *testing.T) {
	fixtureRoot := makeFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, fixtureRoot, cat, "S-WRF")

	run := func(t *testing.T, args ...string) (string, int) {
		t.Helper()
		root := NewRootCmd()
		// The command RESULT goes to stdout; a writer that fails every
		// byte simulates the broken stdout pipe / full disk.
		root.SetOut(failingWriter{})
		errBuf := &bytes.Buffer{}
		code := execute(context.Background(), root, args, errBuf)
		return errBuf.String(), code
	}

	t.Run("clear", func(t *testing.T) {
		// Plan the set FIRST so the clear has real assignments to
		// commit away - the honesty of "the clear transaction
		// COMMITTED" is only meaningful when something WAS cleared.
		if _, stderr, code := runPlanChild(t,
			"--catalog", cat, "--set-id", "S-WRF", "--size", planFixtureSize); code != report.ExitOK {
			t.Fatalf("plan fixture exit = %d, want 0\nstderr: %s", code, stderr)
		}
		stderr, code := run(t, "clear-assignments", "--catalog", cat, "--set-id", "S-WRF")
		if code != report.ExitFatal {
			t.Errorf("exit = %d, want 1 (the report write failed after the commit)\nstderr: %s", code, stderr)
		}
		if !strings.Contains(stderr, "the clear transaction COMMITTED") {
			t.Errorf("stderr lacks the honest committed line:\n%s", stderr)
		}
		if strings.Contains(stderr, "aborted before any assignment was cleared") {
			t.Errorf("stderr still claims the clear aborted (the stale-line defect):\n%s", stderr)
		}
		// TRUTH CHECK: the line says COMMITTED, so the catalog must
		// PROVE it - the clear's runs row is minted INSIDE the
		// transaction with terminal status (clearstore.go), and the
		// assignments are gone. If either is absent, the honest-line
		// claim itself would be a lie.
		db := catOpen(t, cat)
		var clearRuns int
		if err := db.QueryRow(`SELECT COUNT(*) FROM runs WHERE kind = 'clear' AND status = 'ok'`).Scan(&clearRuns); err != nil {
			t.Fatal(err)
		}
		if clearRuns != 1 {
			t.Errorf("committed clear runs rows = %d, want 1 (the line claims COMMITTED; the in-transaction runs row is the proof)", clearRuns)
		}
		var imagesLeft int
		if err := db.QueryRow(`SELECT COUNT(*) FROM images WHERE set_id = 'S-WRF'`).Scan(&imagesLeft); err != nil {
			t.Fatal(err)
		}
		if imagesLeft != 0 {
			t.Errorf("%d images rows survive the 'COMMITTED' clear; the line must state reality", imagesLeft)
		}
	})

	t.Run("plan", func(t *testing.T) {
		// A fresh set so the plan has real work to commit.
		root2 := makeFixture(t)
		cat2 := createCatalog(t)
		scanPlanFixture(t, root2, cat2, "S-WRF-PLAN")
		stderr, code := run(t, "plan", "--catalog", cat2, "--set-id", "S-WRF-PLAN", "--size", planFixtureSize)
		if code != report.ExitFatal {
			t.Errorf("exit = %d, want 1\nstderr: %s", code, stderr)
		}
		if !strings.Contains(stderr, "the assignment transaction COMMITTED") {
			t.Errorf("stderr lacks the honest committed line:\n%s", stderr)
		}
		if strings.Contains(stderr, "aborted before a result was computed") {
			t.Errorf("stderr still claims the plan aborted (the stale-line defect):\n%s", stderr)
		}
		// TRUTH CHECK: the line claims the assignment transaction
		// COMMITTED - the catalog proves it (InsertImage rows exist
		// for the set; they are written inside that transaction).
		var planned int
		if err := catOpen(t, cat2).QueryRow(
			`SELECT COUNT(*) FROM images WHERE set_id = 'S-WRF-PLAN'`).Scan(&planned); err != nil {
			t.Fatal(err)
		}
		if planned == 0 {
			t.Error("no images rows after the 'COMMITTED' plan; the honest-line claim would be false")
		}
	})

	t.Run("report", func(t *testing.T) {
		stderr, code := run(t, "report", "--catalog", cat, "--set-id", "S-WRF", "--size", planFixtureSize)
		if code != report.ExitFatal {
			t.Errorf("exit = %d, want 1\nstderr: %s", code, stderr)
		}
		if !strings.Contains(stderr, "the projection was computed read-only") {
			t.Errorf("stderr lacks the honest computed line:\n%s", stderr)
		}
		if strings.Contains(stderr, "aborted before a result was computed") {
			t.Errorf("stderr still claims the report aborted (the stale-line defect):\n%s", stderr)
		}
		// TRUTH CHECK: "computed read-only" - the projection wrote
		// NOTHING (no new images/files rows, no run row) between the
		// before/after counts.
		var filesBefore, imagesBefore int
		db := catOpen(t, cat)
		if err := db.QueryRow(`SELECT COUNT(*) FROM files WHERE set_id = 'S-WRF'`).Scan(&filesBefore); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM images WHERE set_id = 'S-WRF'`).Scan(&imagesBefore); err != nil {
			t.Fatal(err)
		}
		// (the clear subtest ran first: images is 0, files is the
		// scanned count - both must be IDENTICAL after the
		// failing-write report.)
		stderr2, code2 := run(t, "report", "--catalog", cat, "--set-id", "S-WRF", "--size", planFixtureSize)
		if code2 != report.ExitFatal {
			t.Errorf("second report exit = %d, want 1\nstderr: %s", code2, stderr2)
		}
		var filesAfter, imagesAfter int
		if err := db.QueryRow(`SELECT COUNT(*) FROM files WHERE set_id = 'S-WRF'`).Scan(&filesAfter); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT COUNT(*) FROM images WHERE set_id = 'S-WRF'`).Scan(&imagesAfter); err != nil {
			t.Fatal(err)
		}
		if filesAfter != filesBefore || imagesAfter != imagesBefore {
			t.Errorf("report mutated the catalog (files %d->%d, images %d->%d); the line claims read-only", filesBefore, filesAfter, imagesBefore, imagesAfter)
		}
	})

	t.Run("scan", func(t *testing.T) {
		root3 := makeFixture(t)
		cat3 := createCatalog(t)
		stderr, code := run(t, "scan", "--catalog", cat3, "--set-id", "S-WRF-SCAN", root3)
		if code != report.ExitFatal {
			t.Errorf("exit = %d, want 1\nstderr: %s", code, stderr)
		}
		if !strings.Contains(stderr, "reconciled (status ok") {
			t.Errorf("stderr lacks the honest reconciled line:\n%s", stderr)
		}
		if strings.Contains(stderr, "aborted before any result was reconciled") {
			t.Errorf("stderr still claims the scan aborted (the stale-line defect):\n%s", stderr)
		}
		// TRUTH CHECK: the line says "reconciled (status ok" - the
		// catalog's runs row for the set agrees: 'ok', finalized.
		var status string
		if err := catOpen(t, cat3).QueryRow(
			`SELECT status FROM runs WHERE set_id = 'S-WRF-SCAN'`).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if status != "ok" {
			t.Errorf("runs row status = %q, want 'ok' (the honest line claims the scan reconciled)", status)
		}
	})
}

// TestCopyStrictPromotion closes the --strict wiring matrix: the copy
// command (the remaining emitter of exit 10 without a default-suite
// promotion pin) exits 10 for an item failure WITHOUT --strict and 1
// WITH it, promoting at the END of the run - the run itself is not
// aborted mid-flight (the copied flag of the healthy file stands in
// both runs). scan/verify-media/validate/restore promotion is pinned in
// their suites; build's finalize shares copy's exact code shape.
func TestCopyStrictPromotion(t *testing.T) {
	root := makeFixture(t)
	cat := createCatalog(t)
	scanPlanFixture(t, root, cat, "S-COPYSTRICT")
	if _, stderr, code := runPlanChild(t,
		"--catalog", cat, "--set-id", "S-COPYSTRICT", "--size", planFixtureSize); code != report.ExitOK {
		t.Fatalf("plan fixture exit = %d, want 0\nstderr: %s", code, stderr)
	}
	// Tamper ONE file after the scan: its lstat (size+mtime) no longer
	// matches the catalog, so copy records a source-changed SKIP -
	// the per-item failure class that must NOT abort the run.
	victim := filepath.Join(root, "a.txt")
	data, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("read alpha.txt: %v", err)
	}
	if err := os.WriteFile(victim, append(data, []byte("tampered")...), 0o644); err != nil {
		t.Fatalf("tamper alpha.txt: %v", err)
	}

	copyArgs := func(extra ...string) []string {
		return append(extra, "copy", "--catalog", cat,
			"--set-id", "S-COPYSTRICT", "--dest", filepath.Join(t.TempDir(), "dest"))
	}

	_, stderr, code := runInProcess(t, copyArgs()...)
	if code != report.ExitItemFailures {
		t.Fatalf("copy exit = %d, want 10 (item failure without --strict)\nstderr: %s", code, stderr)
	}
	_, stderr, code = runInProcess(t, copyArgs("--strict")...)
	if code != report.ExitFatal {
		t.Fatalf("copy --strict exit = %d, want 1 (--strict promotes the tail at the end)\nstderr: %s", code, stderr)
	}
	if !strings.Contains(stderr, "--strict") {
		t.Errorf("stderr lacks the --strict promotion message:\n%s", stderr)
	}
}

// failingWriter fails every Write: the injected broken stdout of the
// writeReport-after-commit tests.
type failingWriter struct{}

func (failingWriter) Write(p []byte) (int, error) {
	return 0, fmt.Errorf("injected stdout write failure")
}
