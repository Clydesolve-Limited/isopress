package xorriso

// T4.1 acceptance tests: discovery and version probe against fake xorriso
// scripts on PATH (DESIGN_SPEC 10.1 DependencyError, 11 runtime version
// probe >= 1.5.4, NFR-004 fail-fast), plus the gated test against the
// REAL xorriso binary per DESIGN_SPEC 12.6 ("a version probe test
// asserts the >= 1.5.4 floor").
//
// The fakes are executable shell scripts named "xorriso" in a temp dir
// that is prepended to PATH via t.Setenv - the established script-on-PATH
// seam for this repository. Every dependency-shaped failure asserts the
// typed report.DependencyError, the install hints, and the exit-3
// classification (report.Classify).

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"isopress/internal/report"
)

// originalPATH is captured before any test mutates PATH with t.Setenv, so
// fake-xorriso scripts can still resolve system tools (sleep, printf)
// through the remainder of the real PATH.
var originalPATH = os.Getenv("PATH")

// installFakeXorriso writes an executable shell script named "xorriso"
// (body appended after the shebang and a newline) into a fresh temp dir,
// prepends that dir to PATH, and returns the script path and its
// directory. PATH is prepended rather than replaced so the fake itself
// can exec system tools.
func installFakeXorriso(t *testing.T, body string) (path, dir string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, ToolName)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("write fake xorriso: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+originalPATH)
	return path, dir
}

// blockXorrisoOnPATH points PATH at a fresh EMPTY directory, simulating a
// host with no xorriso installed anywhere.
func blockXorrisoOnPATH(t *testing.T) {
	t.Helper()
	t.Setenv("PATH", t.TempDir())
}

// wantDependencyError classifies err as the DependencyError class, checks
// it maps to exit 3, and checks the message carries each wanted fragment.
func wantDependencyError(t *testing.T, err error, wantFragments ...string) *report.DependencyError {
	t.Helper()
	if err == nil {
		t.Fatal("want a *report.DependencyError, got nil error")
	}
	var dep *report.DependencyError
	if !errors.As(err, &dep) {
		t.Fatalf("err = %v (%T), want a *report.DependencyError", err, err)
	}
	if code := report.Classify(err); code != report.ExitDependency {
		t.Errorf("report.Classify(err) = %d, want %d (DependencyError)", code, report.ExitDependency)
	}
	if dep.Missing != ToolName {
		t.Errorf("DependencyError.Missing = %q, want %q", dep.Missing, ToolName)
	}
	msg := err.Error()
	for _, want := range wantFragments {
		if !strings.Contains(msg, want) {
			t.Errorf("DependencyError message %q does not contain %q", msg, want)
		}
	}
	return dep
}

// --- Discover --------------------------------------------------------------

// Discover resolves the fake xorriso from PATH to its absolute path.
func TestDiscoverResolvesFakeOnPATH(t *testing.T) {
	want, _ := installFakeXorriso(t, `echo "xorriso 1.5.6 : fake"`)

	tool, err := Discover()
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if tool.Path != want {
		t.Errorf("Discover().Path = %q, want %q", tool.Path, want)
	}
}

// With no xorriso anywhere on PATH, Discover fails with the typed
// DependencyError carrying ALL per-distro install hints (DESIGN_SPEC
// 10.1) and mapping to exit 3.
func TestDiscoverMissingBinaryIsDependencyErrorWithHints(t *testing.T) {
	blockXorrisoOnPATH(t)

	_, err := Discover()
	dep := wantDependencyError(t, err,
		"not found on PATH",
		"Debian/Ubuntu: apt install xorriso",
		"Fedora: dnf install xorriso",
		"Arch: pacman -S xorriso",
		"openSUSE: zypper install xorriso",
	)
	if len(dep.Hints) != 4 {
		t.Errorf("DependencyError.Hints = %v, want the 4 per-distro hints", dep.Hints)
	}
	if dep.Err == nil {
		t.Error("DependencyError.Err is nil, want the underlying LookPath error")
	}
}

// A NON-EXECUTABLE file named xorriso on PATH must not be resolved as a
// tool: LookPath refuses it (ErrPermission), and Discover surfaces that
// honestly inside the DependencyError chain rather than pretending the
// binary is merely missing.
func TestDiscoverNonExecutableFileIsHonestDependencyError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ToolName), []byte("#!/bin/sh\necho hi\n"), 0o644); err != nil {
		t.Fatalf("write non-executable xorriso: %v", err)
	}
	t.Setenv("PATH", dir)

	_, err := Discover()
	dep := wantDependencyError(t, err, ToolName)
	// Modern exec.LookPath SKIPS non-executable files on PATH and
	// reports the honest "executable file not found in $PATH"
	// (ErrNotFound); either way the underlying LookPath error must be
	// preserved in the DependencyError chain, never masked.
	if !errors.Is(dep.Err, exec.ErrNotFound) && !errors.Is(dep.Err, fs.ErrPermission) {
		t.Errorf("DependencyError.Err = %v, want it to wrap exec.ErrNotFound or fs.ErrPermission (honest LookPath error)", dep.Err)
	}
}

// A DIRECTORY named xorriso on PATH is not a binary: LookPath skips
// non-regular files (ErrNotFound), and Discover reports the typed
// DependencyError.
func TestDiscoverDirectoryNamedXorrisoIsDependencyError(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ToolName), 0o755); err != nil {
		t.Fatalf("mkdir xorriso: %v", err)
	}
	t.Setenv("PATH", dir)

	_, err := Discover()
	dep := wantDependencyError(t, err, ToolName)
	if !errors.Is(dep.Err, exec.ErrNotFound) {
		t.Errorf("DependencyError.Err = %v, want it to wrap exec.ErrNotFound", dep.Err)
	}
}

// Discover's Detail is SHAPE-AWARE (QA follow-up): it must only claim
// "not found on PATH" when LookPath actually reported ErrNotFound, and
// must describe permission-shaped and relative-path (ErrDot) failures
// honestly. The typed classification chain is untouched: the raw
// LookPath error is still preserved in DependencyError.Err.
func TestLookPathDetailIsShapeAware(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			"ErrNotFound says not found on PATH",
			&exec.Error{Name: ToolName, Err: exec.ErrNotFound},
			"not found on PATH",
		},
		{
			"permission-shaped failure does not claim absence",
			&exec.Error{Name: ToolName, Err: fs.ErrPermission},
			"not executable (permission denied)",
		},
		{
			"ErrDot names the relative PATH refusal",
			&exec.Error{Name: "./" + ToolName, Err: exec.ErrDot},
			"relative PATH entry",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := lookPathDetail(tt.err); !strings.Contains(got, tt.want) {
				t.Errorf("lookPathDetail(%v) = %q, want it to contain %q", tt.err, got, tt.want)
			}
		})
	}
	// The catch-all must embed the underlying error text, so an
	// unrecognised LookPath failure is never flattened to "not found".
	unrecognised := &exec.Error{Name: ToolName, Err: errors.New("EIO: i/o error")}
	if got := lookPathDetail(unrecognised); !strings.Contains(got, "EIO") {
		t.Errorf("lookPathDetail(unrecognised) = %q, want the underlying error text preserved", got)
	}
}

// END-TO-END ErrDot: an executable ./xorriso found via a RELATIVE PATH
// entry is refused by exec.LookPath (Go 1.19+ command-injection
// hardening), and Discover's message must describe that refusal rather
// than claiming the binary is absent, while keeping the DependencyError
// classification and the install hints.
func TestDiscoverErrDotDescribesRelativePATHHit(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ToolName), []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatalf("write xorriso: %v", err)
	}
	t.Chdir(dir)
	t.Setenv("PATH", ".")

	_, err := Discover()
	dep := wantDependencyError(t, err,
		"relative PATH entry",
		"Debian/Ubuntu: apt install xorriso",
	)
	if !errors.Is(dep.Err, exec.ErrDot) {
		t.Errorf("DependencyError.Err = %v, want it to wrap exec.ErrDot", dep.Err)
	}
	if dep.Detail == "not found on PATH" {
		t.Error("Detail claims the binary is absent although LookPath hit ./xorriso (ErrDot)")
	}
}

// --- Probe -----------------------------------------------------------------

// A current-version fake passes the probe; the probe runs exactly
// `xorriso -version` (the fake refuses any other argv), the parsed
// version and raw transcript are returned, and Info carries the resolved
// path.
func TestProbeAcceptsCurrentVersion(t *testing.T) {
	path, _ := installFakeXorriso(t, `
if [ "$1" != "-version" ] || [ "$#" -ne 1 ]; then
  echo "unexpected argv: $@" >&2
  exit 7
fi
echo "xorriso 1.5.6 : RockRidge filesystem manipulator, libburnia project." >&2
echo "xorriso 1.5.6"
echo "ISO 9660 Rock Ridge filesystem manipulator"
exit 0
`)

	tool := &Tool{Path: path}
	info, err := tool.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if info.Version != (Version{1, 5, 6}) {
		t.Errorf("info.Version = %v, want 1.5.6", info.Version)
	}
	if !info.Version.AtLeast(MinVersion) {
		t.Errorf("parsed version %v does not satisfy the floor %v", info.Version, MinVersion)
	}
	if info.Path != path {
		t.Errorf("info.Path = %q, want %q", info.Path, path)
	}
	if !strings.Contains(info.Raw, "xorriso 1.5.6") {
		t.Errorf("info.Raw %q does not carry the banner transcript", info.Raw)
	}
}

// An old xorriso is refused with a DependencyError naming BOTH the found
// and the required versions (T4.1 acceptance).
func TestProbeRejectsOldVersionNamingBothVersions(t *testing.T) {
	path, _ := installFakeXorriso(t, `echo "xorriso 1.4.2 : RockRidge filesystem manipulator, libburnia project."`)

	tool := &Tool{Path: path}
	_, err := tool.Probe(context.Background())
	wantDependencyError(t, err, "1.4.2", "1.5.4")
}

// The floor boundary is exact: 1.5.3 fails, 1.5.4 passes.
func TestProbeFloorBoundary(t *testing.T) {
	t.Run("1.5.3 below floor", func(t *testing.T) {
		path, _ := installFakeXorriso(t, `echo "xorriso 1.5.3 : old"`)
		_, err := (&Tool{Path: path}).Probe(context.Background())
		wantDependencyError(t, err, "1.5.3", "1.5.4")
	})
	t.Run("1.5.4 at floor passes", func(t *testing.T) {
		path, _ := installFakeXorriso(t, `echo "xorriso 1.5.4 : floor"`)
		info, err := (&Tool{Path: path}).Probe(context.Background())
		if err != nil {
			t.Fatalf("Probe at the exact floor: %v", err)
		}
		if info.Version != (Version{1, 5, 4}) {
			t.Errorf("info.Version = %v, want 1.5.4", info.Version)
		}
	})
}

// Output with no parseable version is a DependencyError carrying the raw
// output in the message - an unknown xorriso never silently passes
// (NFR-004 fail-fast, T4.1 acceptance "unparseable").
func TestProbeUnparseableOutputIsDependencyErrorWithRawOutput(t *testing.T) {
	path, _ := installFakeXorriso(t, `echo "banana phone"`)

	_, err := (&Tool{Path: path}).Probe(context.Background())
	wantDependencyError(t, err, "could not parse a version", "banana phone")
}

// A xorriso whose -version exits non-zero is a DependencyError naming
// the exit status (the probe cannot validate what it cannot run).
func TestProbeNonZeroExitIsDependencyError(t *testing.T) {
	path, _ := installFakeXorriso(t, `
echo "xorriso exploded during startup"
exit 3
`)

	_, err := (&Tool{Path: path}).Probe(context.Background())
	wantDependencyError(t, err, "exited with status 3", "xorriso exploded during startup")
}

// A HUNG xorriso must not hang the tool: the probe's own ProbeTimeout
// fires, the child is killed, and the result is a DependencyError naming
// the timeout - all well under the fake's sleep duration.
func TestProbeTimesOutOnHungBinary(t *testing.T) {
	oldTimeout := ProbeTimeout
	ProbeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { ProbeTimeout = oldTimeout })

	path, _ := installFakeXorriso(t, `
echo "xorriso 1.5.6 : pre-hang banner"
exec sleep 30
`)

	start := time.Now()
	_, err := (&Tool{Path: path}).Probe(context.Background())
	elapsed := time.Since(start)
	if elapsed >= 10*time.Second {
		t.Fatalf("probe took %v; the hung binary was not bounded by ProbeTimeout=%v", elapsed, ProbeTimeout)
	}
	wantDependencyError(t, err, "timed out", "300ms")
}

// Cancellation of the CALLER's context propagates as the raw context
// error, NOT as a DependencyError, so SIGINT during a probe maps to the
// Interrupted class (exit 130) rather than the dependency class (3).
func TestProbePropagatesCallerCancellation(t *testing.T) {
	path, _ := installFakeXorriso(t, `exec sleep 30`)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := (&Tool{Path: path}).Probe(ctx)
	elapsed := time.Since(start)
	if elapsed >= 10*time.Second {
		t.Fatalf("Probe ignored caller cancellation for %v", elapsed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled (not a DependencyError)", err)
	}
	var dep *report.DependencyError
	if errors.As(err, &dep) {
		t.Fatal("caller cancellation must not classify as DependencyError")
	}
}

// --- probe output capture cap ----------------------------------------------

// The PROBE capture is capped too: Probe goes through Tool.Run, so the
// 10 MiB tail ring buffer bounds Info.Raw even when a pathological
// xorriso floods `xorriso -version`. A banner at the END of >10 MiB of
// output still parses (the TAIL is what is retained), and the truncation
// marker with true byte accounting prefixes the transcript.
func TestProbeHugeOutputIsCappedAndTailPreservesBanner(t *testing.T) {
	path, _ := installFakeXorriso(t, `
dd if=/dev/zero bs=1048576 count=11 2>/dev/null | tr '\0' 'a'
echo "xorriso 1.5.6 : RockRidge filesystem manipulator, libburnia project."
exit 0
`)

	info, err := (&Tool{Path: path}).Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe with huge output: %v", err)
	}
	if info.Version != (Version{1, 5, 6}) {
		t.Errorf("info.Version = %v, want 1.5.6 parsed from the TAIL of the flood", info.Version)
	}
	const cap = MaxCapturedOutput
	// Upper bound: cap + the truncation marker as rendered for
	// realistic byte counts (8-digit shown/total).
	if len(info.Raw) > cap+len(truncationMarker(cap, cap*2)) {
		t.Errorf("Info.Raw is %d bytes, want <= %d + marker (probe capture must be capped)", len(info.Raw), cap)
	}
	if !strings.HasPrefix(info.Raw, "[xorriso output truncated:") {
		t.Errorf("Info.Raw does not start with the truncation marker; prefix %.80q", info.Raw)
	}
	const banner = "xorriso 1.5.6 : RockRidge filesystem manipulator, libburnia project.\n"
	if !strings.HasSuffix(info.Raw, banner) {
		t.Errorf("Info.Raw tail lost the banner: last 80 bytes %.80q", info.Raw[max(0, len(info.Raw)-80):])
	}
}

// When the flood carries NO parseable version, the DependencyError
// MESSAGE stays bounded by rawSnippetLimit (512 bytes) - the full
// transcript exists on Info.Raw for run_events, but the error message
// itself can never become a multi-megabyte transcript.
func TestProbeHugeUnparseableOutputKeepsErrorMessageBounded(t *testing.T) {
	path, _ := installFakeXorriso(t, `
dd if=/dev/zero bs=1048576 count=11 2>/dev/null | tr '\0' 'a'
exit 0
`)

	info, err := (&Tool{Path: path}).Probe(context.Background())
	wantDependencyError(t, err, "could not parse a version", "more bytes omitted")
	if msg := err.Error(); len(msg) > 4*rawSnippetLimit {
		t.Errorf("DependencyError message is %d bytes; want bounded by rawSnippetLimit=%d", len(msg), rawSnippetLimit)
	}
	if len(info.Raw) <= MaxCapturedOutput {
		t.Errorf("Info.Raw is %d bytes; the flood should have hit the %d cap", len(info.Raw), MaxCapturedOutput)
	}
}

// --- real binary (DESIGN_SPEC 12.6 gated test) -----------------------------

// The 12.6 gate must SKIP CLEANLY (not fail, not error) on a host whose
// PATH carries no xorriso: with PATH pointed at an empty directory the
// same LookPath condition the gated test uses fires, and the subtest
// skips with the diagnostic message. Simulated via t.Setenv so it runs
// everywhere, including CI images without xorriso.
func TestRealXorrisoProbeGateSkipsWhenAbsent(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no xorriso anywhere

	if _, err := exec.LookPath(ToolName); err == nil {
		t.Fatalf("exec.LookPath(%q) found a binary on an empty PATH", ToolName)
	}

	// Reproduce the exact gate of TestRealXorrisoProbeMeetsFloor inside
	// a subtest: the subtest must end as SKIPPED, never failed.
	skipped := t.Run("gated probe under empty PATH", func(t *testing.T) {
		if _, err := exec.LookPath(ToolName); err != nil {
			t.Skipf("real %s not on PATH; skipping gated probe test (DESIGN_SPEC 12.6)", ToolName)
		}
		t.Fatal("gate did not skip although xorriso is absent from PATH")
	})
	if !skipped {
		t.Fatal("the gated probe subtest did not skip cleanly on a PATH without xorriso")
	}
}
