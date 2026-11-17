package cli

// T0.5 gap-closing tests (test-suite audit): these close the coverage
// holes the acceptance audit found in cli_test.go.
//
//   1. Flag round-trip: the existing tests prove the stubs PARSE the ten
//      global flags of DESIGN_SPEC 1.1 (they exit 0 with all of them
//      given), but never prove the parsed values are RETRIEVABLE. A
//      probe subcommand reads every value back through cobra's flag
//      lookup - the exact mechanism every later command will use - and
//      the test asserts the parsed values, including the repeatable
//      --exclude accumulation (three --exclude flags -> three patterns).
//   2. Missing flag value: SetFlagErrorFunc must turn "flag needs an
//      argument" into a typed UsageError (exit 2), for every value-taking
//      global flag.
//   3. Help surface: `isopress help`, `isopress -h`, and the bare
//      invocation all exit 0 and put the help (the requested result) on
//      stdout (DESIGN_SPEC 10.3); the command surface is exactly the
//      nine design commands plus cobra's built-in help - no completion
//      command, no tenth command.
//   4. Resolved module versions: go.mod pinning is asserted by
//      deps_test.go, but a replace directive or graph drift could still
//      change what the toolchain actually builds against;
//      `go list -m` pins the RESOLVED versions too.

import (
	"bytes"
	"context"
	"os/exec"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"isopress/internal/report"
)

// --- 1. global flag values round-trip ----------------------------------------

// TestGlobalFlagValuesRoundTrip proves the ten global persistent flags of
// DESIGN_SPEC 1.1 parse into values a consuming layer can read back: a
// probe subcommand (built on the same cobra tree execute() runs) reads
// each flag via cmd.Flags().Get..., records it, and the test asserts the
// recorded values against what was given on the command line. This is the
// read path T1.6+ commands use; if a flag were registered against a dead
// variable or with a mismatched name, this test fails where the parse-only
// tests would stay green.
func TestGlobalFlagValuesRoundTrip(t *testing.T) {
	excludeFile := writeExcludeFile(t, "*.tmp\n")

	root := NewRootCmd()
	got := map[string]any{}
	probe := &cobra.Command{
		Use: "flag-probe",
		RunE: func(cmd *cobra.Command, args []string) error {
			record := func(name string, err error) {
				if err != nil {
					t.Fatalf("reading back flag --%s: %v", name, err)
				}
			}
			var (
				vStr  string
				vBool bool
				vArr  []string
				err   error
			)
			vStr, err = cmd.Flags().GetString("catalog")
			record("catalog", err)
			got["catalog"] = vStr

			vStr, err = cmd.Flags().GetString("exclude-from")
			record("exclude-from", err)
			got["exclude-from"] = vStr

			vStr, err = cmd.Flags().GetString("set-id")
			record("set-id", err)
			got["set-id"] = vStr

			vStr, err = cmd.Flags().GetString("work-dir")
			record("work-dir", err)
			got["work-dir"] = vStr

			vStr, err = cmd.Flags().GetString("image-dir")
			record("image-dir", err)
			got["image-dir"] = vStr

			vArr, err = cmd.Flags().GetStringArray("exclude")
			record("exclude", err)
			got["exclude"] = vArr

			for _, b := range []string{"quiet", "verbose", "json", "strict"} {
				vBool, err = cmd.Flags().GetBool(b)
				record(b, err)
				got[b] = vBool
			}
			return nil
		},
	}
	root.AddCommand(probe)

	errBuf := &bytes.Buffer{}
	code := execute(context.Background(), root, []string{
		"--catalog", "c.sqlite",
		"--exclude", "*.iso",
		"--exclude", "var/log/**",
		"--exclude", "tmp/**",
		"--exclude-from", excludeFile,
		"--set-id", "S-042",
		"--json",
		"--strict",
		"--work-dir", "/tmp/w",
		"--image-dir", "/tmp/i",
		"--quiet",
		"--verbose",
		"flag-probe",
	}, errBuf)
	if code != report.ExitOK {
		t.Fatalf("probe run exit code = %d, want 0 (stderr: %s)", code, errBuf.String())
	}

	strs := map[string]string{
		"catalog":      "c.sqlite",
		"exclude-from": excludeFile,
		"set-id":       "S-042",
		"work-dir":     "/tmp/w",
		"image-dir":    "/tmp/i",
	}
	for name, want := range strs {
		if got[name] != want {
			t.Errorf("flag --%s round-trip = %v, want %v", name, got[name], want)
		}
	}
	for _, name := range []string{"quiet", "verbose", "json", "strict"} {
		if got[name] != true {
			t.Errorf("flag --%s round-trip = %v, want true", name, got[name])
		}
	}
	exclude, ok := got["exclude"].([]string)
	if !ok {
		t.Fatalf("--exclude read-back has type %T, want []string", got["exclude"])
	}
	// Repeatable accumulation (DESIGN_SPEC 1.1/3.2): three --exclude flags
	// yield three patterns, in command-line order, with no splitting or
	// overwriting.
	wantExclude := []string{"*.iso", "var/log/**", "tmp/**"}
	if len(exclude) != len(wantExclude) {
		t.Fatalf("--exclude accumulated %d patterns, want %d (%v)", len(exclude), len(wantExclude), exclude)
	}
	for i := range wantExclude {
		if exclude[i] != wantExclude[i] {
			t.Errorf("--exclude[%d] = %q, want %q (full: %v)", i, exclude[i], wantExclude[i], exclude)
		}
	}
}

// The same probe with NO global flags given pins the registered defaults:
// empty strings, false bools, an empty --exclude pattern list. A later
// task changing a default must do so deliberately (DESIGN_SPEC 1.1 gives
// no defaults that differ).
func TestGlobalFlagDefaults(t *testing.T) {
	root := NewRootCmd()
	got := map[string]any{}
	probe := &cobra.Command{
		Use: "flag-probe",
		RunE: func(cmd *cobra.Command, args []string) error {
			cat, err := cmd.Flags().GetString("catalog")
			if err != nil {
				t.Fatalf("reading --catalog: %v", err)
			}
			got["catalog"] = cat
			ex, err := cmd.Flags().GetStringArray("exclude")
			if err != nil {
				t.Fatalf("reading --exclude: %v", err)
			}
			got["exclude"] = ex
			for _, b := range []string{"quiet", "verbose", "json", "strict"} {
				v, err := cmd.Flags().GetBool(b)
				if err != nil {
					t.Fatalf("reading --%s: %v", b, err)
				}
				got[b] = v
			}
			return nil
		},
	}
	root.AddCommand(probe)
	errBuf := &bytes.Buffer{}
	if code := execute(context.Background(), root, []string{"flag-probe"}, errBuf); code != report.ExitOK {
		t.Fatalf("probe run exit code = %d, want 0 (stderr: %s)", code, errBuf.String())
	}
	if got["catalog"] != "" {
		t.Errorf("--catalog default = %v, want empty string", got["catalog"])
	}
	// pflag may hand back an empty non-nil slice for an unset repeatable
	// flag; the contract is "no patterns", not a specific nil-ness.
	if ex, _ := got["exclude"].([]string); len(ex) != 0 {
		t.Errorf("--exclude default = %v, want an empty pattern list", ex)
	}
	for _, b := range []string{"quiet", "verbose", "json", "strict"} {
		if got[b] != false {
			t.Errorf("--%s default = %v, want false", b, got[b])
		}
	}
}

// --- 2. missing flag value (SetFlagErrorFunc -> UsageError, exit 2) ----------

// A value-taking flag with no argument ("flag needs an argument") must
// route through SetFlagErrorFunc into a typed UsageError: exit 2, the
// parse error on stderr, the usage hint, and nothing on stdout
// (DESIGN_SPEC 10.1, 10.3). Covered for every value-taking global flag.
func TestMissingFlagValueExits2(t *testing.T) {
	valueFlags := []string{
		"--catalog", "--exclude", "--exclude-from",
		"--set-id", "--work-dir", "--image-dir",
	}
	for _, flag := range valueFlags {
		t.Run(flag, func(t *testing.T) {
			stdout, stderr, code := runInProcess(t, "scan", flag)
			if code != report.ExitUsage {
				t.Fatalf("%s without a value: exit code = %d, want 2 (stderr: %s)", flag, code, stderr)
			}
			if !strings.Contains(stderr, "needs an argument") {
				t.Errorf("%s without a value: stderr lacks the parse error:\n%s", flag, stderr)
			}
			if !strings.Contains(stderr, "usage") {
				t.Errorf("%s without a value: stderr lacks the usage hint (DESIGN_SPEC 10.1):\n%s", flag, stderr)
			}
			if stdout != "" {
				t.Errorf("%s without a value: stdout must stay empty on usage errors (FR-046), got: %q", flag, stdout)
			}
		})
	}
}

// --- 3. help surface: help / -h / bare invocation ----------------------------

// `isopress help` and `isopress -h` both exit 0 with the root help
// (the requested result) on stdout (DESIGN_SPEC 10.3).
func TestHelpAliasAndShortFlagExit0(t *testing.T) {
	for _, args := range [][]string{{"help"}, {"-h"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			stdout, stderr, code := runInProcess(t, args...)
			if code != report.ExitOK {
				t.Fatalf("%v exit code = %d, want 0 (stderr: %s)", args, code, stderr)
			}
			if !strings.Contains(stdout, "Available Commands:") {
				t.Errorf("%v: stdout lacks the command list:\n%s", args, stdout)
			}
			for _, name := range stubCommands {
				if !strings.Contains(stdout, name) {
					t.Errorf("%v: help does not list command %q:\n%s", args, name, stdout)
				}
			}
		})
	}
}

// Bare `isopress` with no arguments prints help (cobra's default for a
// root with no Run of its own beyond the help fallback) and exits 0 -
// through the real production entry path with REAL process streams, so
// the stdout placement is asserted end to end.
func TestBareInvocationPrintsHelpAndExits0(t *testing.T) {
	stdout, stderr, code := runCLIChild(t)
	if code != report.ExitOK {
		t.Fatalf("bare invocation exit code = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(stdout, "Available Commands:") {
		t.Errorf("bare invocation: stdout lacks the help/command list: %q", stdout)
	}
	if stderr != "" {
		t.Errorf("bare invocation: stderr must stay empty, got: %q", stderr)
	}
}

// The command surface is EXACTLY the nine design commands plus cobra's
// built-in help: programmatically asserted over root.Commands() after a
// real execute, so an accidentally registered tenth command (or cobra's
// completion command resurfacing) fails here even if the help-text
// substring checks happen to pass.
func TestCommandSurfaceIsExactlyNinePlusHelp(t *testing.T) {
	root := NewRootCmd()
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	if code := execute(context.Background(), root, []string{"--help"}, &bytes.Buffer{}); code != report.ExitOK {
		t.Fatalf("--help exit code = %d, want 0", code)
	}

	want := append([]string{"help"}, stubCommands...)
	var got []string
	for _, c := range root.Commands() {
		got = append(got, c.Name())
	}
	sort.Strings(want)
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("command surface = %v, want exactly %v (nine design commands + help; no completion)", got, want)
	}
}

// --- 4. resolved module versions ---------------------------------------------

// TestResolvedCobraAndPflagVersions pins what the toolchain actually
// RESOLVES for cobra and pflag (go list -m reports the selected module
// versions, which a replace directive or MVS bump would change without
// touching the pinned require lines deps_test.go checks).
func TestResolvedCobraAndPflagVersions(t *testing.T) {
	path, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH; skipping module resolution check")
	}
	out, err := exec.Command(path, "list", "-m",
		"github.com/spf13/cobra", "github.com/spf13/pflag").Output()
	if err != nil {
		t.Fatalf("go list -m: %v", err)
	}
	want := map[string]string{
		"github.com/spf13/cobra": "v1.10.2",
		"github.com/spf13/pflag": "v1.0.10",
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			t.Fatalf("unexpected go list -m line: %q", line)
		}
		if w, ok := want[fields[0]]; ok && fields[1] != w {
			t.Errorf("resolved %s = %s, want %s (DESIGN_SPEC 11 exact pin)", fields[0], fields[1], w)
		}
	}
	if t.Failed() {
		return
	}
	for mod := range want {
		if !strings.Contains(string(out), mod+" ") {
			t.Errorf("go list -m output lacks module %s:\n%s", mod, out)
		}
	}
}
