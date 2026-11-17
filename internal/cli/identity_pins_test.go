package cli

// IDENTITY PIN - the tool-rename sweep (the pre-rename name -> isopress).
//
// root.go's error wrapper carries THREE user-facing identity literals
// that are independent of cobra's Use field:
//
//	error line   "isopress: error: %v"          (pinned 12x: json_contract_test, taxonomy_table_test)
//	usage hint   "run 'isopress --help' ..."     (NOT pinned before this file)
//	fatal dump   "isopress: fatal unexpected..." (pinned via the stack-dump shape)
//
// The usage hint is the gap this file closes: TestUnknownFlagExits2 only
// requires the substring "usage" to appear, which a hint reading
// `run '<old-tool-name> --help' ... for usage` would still satisfy, and the
// taxonomy table pins only the CAUSE wording ("unknown command"), never
// the identity. A rename regression in that one line would therefore have
// shipped with a fully green suite.
//
// Both spellings of the hint are pinned byte-for-byte on a REAL
// usage-class error, together with the error line's prefix - the two
// literals are printed back to back by the same wrapper, so they must
// agree on the identity.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"isopress/internal/catalog"
	"isopress/internal/report"
)

// usageHintLine is root.go's usage hint, verbatim, as the tool must print
// it for every usage-class error (exit 2).
const usageHintLine = "run 'isopress --help' or 'isopress <command> --help' for usage"

// TestUsageHintCarriesIsopressIdentity pins the hint and the error prefix
// on the unknown-subcommand path (the same cell the taxonomy table uses
// for its cause wording).
func TestUsageHintCarriesIsopressIdentity(t *testing.T) {
	_, stderr, code := runInProcess(t, "frobnicate")
	if code != report.ExitUsage {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, report.ExitUsage, stderr)
	}

	lines := strings.Split(strings.TrimRight(stderr, "\n"), "\n")
	if len(lines) < 2 {
		t.Fatalf("stderr must be [error line, usage hint], got %d line(s): %q", len(lines), stderr)
	}

	// The final line is the hint, byte-for-byte: both spellings carry the
	// isopress identity, so a drifted name fails here.
	if got := lines[len(lines)-1]; got != usageHintLine {
		t.Errorf("usage hint drifted from the isopress identity\n got: %q\nwant: %q", got, usageHintLine)
	}

	// The error line immediately above it carries the SAME identity: two
	// separate literals in root.go, printed by one wrapper, must agree.
	if errLine := lines[len(lines)-2]; !strings.HasPrefix(errLine, "isopress: error: ") {
		t.Errorf("error line lacks the isopress prefix (hint and error line disagree on identity): %q", errLine)
	}
}

// TestUsageHintAbsentOnNonUsageErrors is the negative half: the hint is a
// usage-class-only line (root.go prints it under code == ExitUsage), so a
// non-usage failure must not carry it.
func TestUsageHintAbsentOnNonUsageErrors(t *testing.T) {
	// A missing catalog file is the CatalogError (exit 4) cell - no hint.
	missing := filepath.Join(t.TempDir(), "missing.sqlite")
	_, stderr, code := runInProcess(t, "plan", "--catalog", missing, "--set-id", "S", "--size", "10G")
	if code != report.ExitCatalog {
		t.Fatalf("exit = %d, want %d (stderr: %s)", code, report.ExitCatalog, stderr)
	}
	if strings.Contains(stderr, "for usage") {
		t.Errorf("a catalog error must not print the usage hint:\n%s", stderr)
	}
}

// --- pre-release R1 (2026-11-09): the --version flag line -------------------

// TestVersionFlagPrintsCatalogToolVersion pins cobra's built-in --version
// output byte-for-byte: the default template renders "<name> version <v>"
// from the root's Version field, which is catalog.ToolVersion (the SINGLE
// version source - the same const stamped into media manifests). Pinning
// against the CONST (not a hard literal) keeps the two halves honest: the
// identity prefix "isopress version " can drift only deliberately, and the
// version value can drift only by changing catalog.ToolVersion, which is
// exactly the release-bump point.
func TestVersionFlagPrintsCatalogToolVersion(t *testing.T) {
	stdout, stderr, code := runInProcess(t, "--version")
	if code != report.ExitOK {
		t.Fatalf("--version exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	want := "isopress version " + catalog.ToolVersion + "\n"
	if stdout != want {
		t.Errorf("--version stdout drifted\n got: %q\nwant: %q", stdout, want)
	}
	if stderr != "" {
		t.Errorf("--version must print to stdout only (FR-046), stderr = %q", stderr)
	}
}

// TestVersionFlagIsRegisteredWithoutStealingVerboseShorthand pins the
// FLAG SURFACE of the built-in: cobra registers --version as a LOCAL
// flag of the root at execute time (InitDefaultVersionFlag), it shows up
// in the root help's flags block, and it must NOT take the "-v"
// shorthand - cobra only adds that shorthand when "v" is free, and
// root's --verbose owns it (a version "-v" would silently turn every
// existing `isopress -v` invocation into a version print).
func TestVersionFlagIsRegisteredWithoutStealingVerboseShorthand(t *testing.T) {
	help, stderr, code := runInProcess(t, "--help")
	if code != report.ExitOK {
		t.Fatalf("--help exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if !strings.Contains(help, "--version") {
		t.Errorf("--help output lacks the --version flag entry:\n%s", help)
	}
	if strings.Contains(help, "-v, --version") {
		t.Errorf("--version stole the -v shorthand from --verbose:\n%s", help)
	}
	// The verbose shorthand must still be verbose's.
	if !strings.Contains(help, "-v, --verbose") {
		t.Errorf("--help output lost the -v, --verbose entry:\n%s", help)
	}
	// And the topic path renders the SAME flags block (root.go's custom
	// help command calls InitDefaultVersionFlag like cobra's builtin).
	topic, _, topicCode := runInProcess(t, "help")
	if topicCode != report.ExitOK {
		t.Fatalf("bare help exit = %d, want 0", topicCode)
	}
	if !strings.Contains(topic, "--version") {
		t.Errorf("bare 'help' output lacks the --version flag entry:\n%s", topic)
	}
}

// --- pre-release R1: the second-signal escape line (root.go escape) ---------

// secondSignalLine is root.go's second-signal forcing-exit line, verbatim:
// the WHOLE line, prefix included. The pre-existing pins only matched the
// message TAIL ("second signal received, forcing exit"), so a partial
// identity regression on the prefix would have shipped green.
const secondSignalLine = "isopress: second signal received, forcing exit"

// TestSecondSignalLineCarriesIsopressIdentity pins root.go's escape line
// byte-for-byte on a REAL second signal, through the same injected core
// the signal suite uses (startSecondSignalEscape): one first signal
// (graceful trigger), one genuine second (the escape), then the exact
// stderr bytes. The buffer receives ONLY this line, so equality (not a
// substring) is the pin - any prefix or wording drift fails here.
func TestSecondSignalLineCarriesIsopressIdentity(t *testing.T) {
	sigs := make(chan os.Signal, 2)
	done := make(chan struct{})
	defer close(done)
	cancelled := make(chan struct{}, 1)
	forced := make(chan os.Signal, 1)
	var errBuf bytes.Buffer

	startSecondSignalEscape(sigs, func() { cancelled <- struct{}{} }, done, &errBuf,
		func(s os.Signal) { forced <- s })

	sigs <- syscall.SIGTERM // the FIRST signal: graceful path only
	select {
	case <-cancelled:
	case <-time.After(10 * time.Second):
		t.Fatal("first signal never triggered the graceful path (cancel)")
	}

	sigs <- syscall.SIGINT // the genuine SECOND signal: the escape fires
	select {
	case <-forced:
	case <-time.After(10 * time.Second):
		t.Fatal("second signal did not fire the force action")
	}

	// Byte-exact: the escape prints this ONE line to stderr and nothing
	// else, so the whole buffer is the line (plus its newline).
	want := secondSignalLine + "\n"
	if got := errBuf.String(); got != want {
		t.Errorf("second-signal line drifted from the isopress identity\n got: %q\nwant: %q", got, want)
	}
}

// --- pre-release R1: the fatal/verbose stack-dump header (root.go) ----------

// fatalDumpLine is root.go's fatal-unexpected verbose dump header,
// verbatim (the format string's literal prefix; the goroutine dump
// itself follows on subsequent bytes). Pre-existing pins matched only
// "isopress: error:" (the line above) and the tail "goroutine stack
// dump (--verbose)", never this line as a whole.
const fatalDumpLine = "isopress: fatal unexpected error: goroutine stack dump (--verbose):"

// TestFatalDumpLineCarriesIsopressIdentity pins the dump header on a REAL
// fatal-unexpected run: a probe subcommand returns an untyped error (the
// bug class by construction - Classify falls through to ExitFatal) with
// --verbose set, and the wrapper must print the header byte-for-byte
// before the goroutine dump. Cheap in-process trigger - no fixtures, no
// re-exec - but it exercises the real execute() path, so a prefix
// regression fails here while the taxonomy table's tail-only pin would
// still pass.
func TestFatalDumpLineCarriesIsopressIdentity(t *testing.T) {
	root := NewRootCmd()
	root.AddCommand(&cobra.Command{
		Use: "fatal-probe",
		RunE: func(cmd *cobra.Command, args []string) error {
			return errors.New("probe boom (intentional, untyped)")
		},
	})
	errBuf := &bytes.Buffer{}
	code := execute(context.Background(), root, []string{"--verbose", "fatal-probe"}, errBuf)
	if code != report.ExitFatal {
		t.Fatalf("exit = %d, want %d (fatal unexpected)\nstderr: %s", code, report.ExitFatal, errBuf.String())
	}

	// Byte-exact on the header line: it is a whole line of stderr, so
	// line equality (not a substring) is the pin.
	found := false
	for _, line := range strings.Split(strings.TrimRight(errBuf.String(), "\n"), "\n") {
		if line == fatalDumpLine {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("fatal dump header drifted from the isopress identity (want whole line %q):\n%s",
			fatalDumpLine, errBuf.String())
	}
	// The dump that follows is real (the taxonomy table pins the shape;
	// here we only keep the pin honest that a header without a dump is
	// not the contract being asserted).
	if !strings.Contains(errBuf.String(), "\ngoroutine ") {
		t.Errorf("the header was printed without a goroutine dump following it:\n%s", errBuf.String())
	}
}

// --- pre-release QA (2026-11-09): --version edge behavior --------------------

// TestVersionFlagIsRootOnlyOnSubcommands pins cobra's registration
// SCOPE: InitDefaultVersionFlag adds --version to the command that
// CARRIES the Version field (the root) as a LOCAL flag at execute time,
// so a subcommand never gets it. `isopress scan --version` is therefore
// a plain unknown flag - exit 2, empty stdout (FR-046), the flag error on
// stderr - and NOT a version print. If Version ever leaked down the tree
// (persistent flag, Version on a child), this flips to exit 0 + version
// text and every caller that treats a subcommand's flags as that
// subcommand's contract would silently change shape.
func TestVersionFlagIsRootOnlyOnSubcommands(t *testing.T) {
	stdout, stderr, code := runInProcess(t, "scan", "--version")
	if code != report.ExitUsage {
		t.Fatalf("scan --version exit = %d, want 2 (unknown flag on a subcommand)", code)
	}
	if stdout != "" {
		t.Errorf("scan --version must print nothing to stdout, got %q", stdout)
	}
	if !strings.Contains(stderr, "unknown flag: --version") {
		t.Errorf("stderr lacks the unknown-flag error:\n%s", stderr)
	}
}

// TestVersionFlagShortCircuitsUnknownCommand pins the cobra builtin
// ORDERING at the root: ParseFlags -> version check -> arg validation.
// So `isopress bogus --version` answers the version request (exit 0, the
// exact version line, nothing on stderr) while plain `isopress bogus` is
// the unknown-command usage error (exit 2 - pinned by
// TestUnknownSubcommandExits2): the version flag wins because cobra
// handles it before ValidateArgs ever sees "bogus". Accepted as sane
// (--version is an information request that needs no valid command), but
// pinned so a cobra upgrade or a reordered flag/error handler turns any
// change here into a deliberate decision instead of silent drift.
func TestVersionFlagShortCircuitsUnknownCommand(t *testing.T) {
	stdout, stderr, code := runInProcess(t, "bogus", "--version")
	if code != report.ExitOK {
		t.Fatalf("bogus --version exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	if want := "isopress version " + catalog.ToolVersion + "\n"; stdout != want {
		t.Errorf("bogus --version stdout drifted\n got: %q\nwant: %q", stdout, want)
	}
	if stderr != "" {
		t.Errorf("bogus --version must stay silent on stderr, got %q", stderr)
	}
}
