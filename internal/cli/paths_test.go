package cli

// Acceptance tests for the leading-tilde path detection (checkTilde,
// paths.go; PM decision D1 of 2026-10-19) and for the missing-parent-
// directory message enrichment (PM decision D2, catalog-side
// enrichMissingParentDir), both driven through the REAL command tree:
// unit coverage of the trigger rule plus end-to-end coverage of every
// path-typed CLI input the tool currently accepts (--catalog,
// --exclude-from, --work-dir, --image-dir, and scan's ROOT positional)
// and of the four catalog-touching commands (scan auto-creates, plan,
// report, and clear-assignments refuse to).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"isopress/internal/report"
)

// --- checkTilde unit tests (the exact trigger rule) --------------------------

// TestCheckTildeTriggerRule pins the documented trigger: ONLY the bare
// "~" and the "~/..." prefix are rejected; "~user/name" (the
// login-name shell feature the tool does not support and treats
// literally), mid-path tildes, absolute paths, and the empty string all
// pass untouched.
func TestCheckTildeTriggerRule(t *testing.T) {
	trigger := []string{"~", "~/", "~/x", "~/a/b/c.db", "~/tmp/test-catalog.db"}
	for _, value := range trigger {
		if err := checkTilde(value, "--catalog"); err == nil {
			t.Errorf("checkTilde(%q) = nil, want an error (bare ~ or ~/ prefix)", value)
		}
	}
	noTrigger := []string{
		"",              // empty: nothing to check (unset flag)
		"x",             // plain relative path
		"~user/x",       // login-name expansion: a DIFFERENT shell feature; treated literally
		"~user",         // ditto, bare
		"foo/~/bar",     // mid-path tilde: no shell expands it
		"./~/x",         // mid-path tilde after a dot segment
		"/home/alice/x", // absolute path
		"~~",            // leading but not bare and not tilde-slash
		"~.hidden",      // leading tilde but not tilde-slash
		"a~",            // trailing tilde
	}
	for _, value := range noTrigger {
		if err := checkTilde(value, "--catalog"); err != nil {
			t.Errorf("checkTilde(%q) = %v, want nil (only ~ and ~/ trigger)", value, err)
		}
	}
}

// The rejection message must carry the actionable guidance the brief
// demands: the input's NAME (flag or positional), the literal value the
// tool received, and the three remedies (unquoted ~, $HOME, absolute
// path) plus the explanation that quoting suppressed shell expansion.
func TestCheckTildeMessageIsActionable(t *testing.T) {
	err := checkTilde("~/tmp/test-catalog.db", "--catalog")
	if err == nil {
		t.Fatal("checkTilde(\"~/tmp/test-catalog.db\") = nil, want an error")
	}
	msg := err.Error()
	for _, want := range []string{
		"--catalog",             // names the input
		"~/tmp/test-catalog.db", // the literal value received
		"not shell-expanded",    // the explanation
		"unquoted ~",            // remedy 1
		"$HOME",                 // remedy 2
		"absolute path",         // remedy 3
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
}

// --- through the REAL commands: leading tilde -> exit 2 ----------------------

// Every path-typed input the CLI accepts rejects a leading-tilde
// literal with the usage class (exit 2): stderr names the flag or
// positional and carries the guidance. Covered inputs: --catalog
// (through scan, plan, report, and clear-assignments), scan's ROOT
// positional, --exclude-from, --work-dir, and --image-dir (the latter
// three exercised through the copy command - the validation is
// root-level PersistentPreRunE, so it fires before the command's own
// required-flag checks; T3.3 promoted copy from stub to real command
// and the root-level guarantee is unchanged).
func TestLeadingTildeRejectedThroughCommands(t *testing.T) {
	root := t.TempDir() // a real, absolute scan ROOT (never reached)
	cleanCatalog := filepath.Join(t.TempDir(), "clean.sqlite")

	cases := []struct {
		name  string
		args  []string
		input string // the flag/positional the stderr must name
		value string // the literal value stderr must echo
	}{
		{"scan --catalog", []string{"scan", "--catalog", "~/x.db", root}, "--catalog", "~/x.db"},
		{"scan --catalog bare tilde", []string{"scan", "--catalog", "~", root}, "--catalog", "~"},
		{"plan --catalog", []string{"plan", "--catalog", "~/x.db", "--set-id", "S", "--size", "25"}, "--catalog", "~/x.db"},
		{"report --catalog", []string{"report", "--catalog", "~/x.db", "--set-id", "S", "--size", "25"}, "--catalog", "~/x.db"},
		{"clear --catalog", []string{"clear-assignments", "--catalog", "~/x.db", "--set-id", "S"}, "--catalog", "~/x.db"},
		{"scan ROOT positional", []string{"scan", "--catalog", cleanCatalog, "~/data"}, "ROOT", "~/data"},
		{"exclude-from", []string{"copy", "--exclude-from", "~/list.txt"}, "--exclude-from", "~/list.txt"},
		{"work-dir", []string{"copy", "--work-dir", "~/w"}, "--work-dir", "~/w"},
		{"image-dir", []string{"copy", "--image-dir", "~/i"}, "--image-dir", "~/i"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code := runInProcess(t, tc.args...)
			if code != report.ExitUsage {
				t.Fatalf("exit code = %d, want 2 (usage)\nstderr: %s", code, stderr)
			}
			if !strings.Contains(stderr, tc.input) {
				t.Errorf("stderr does not name the input %q:\n%s", tc.input, stderr)
			}
			if !strings.Contains(stderr, tc.value) {
				t.Errorf("stderr does not echo the literal value %q:\n%s", tc.value, stderr)
			}
			if !strings.Contains(stderr, "shell-expanded") || !strings.Contains(stderr, "$HOME") {
				t.Errorf("stderr lacks the actionable guidance:\n%s", stderr)
			}
		})
	}
}

// The stakeholder scenario through the REAL process entry path: the
// rejected --catalog "~/..." must leave NOTHING on disk - the tool
// refuses the input before any filesystem effect (no auto-expansion, no
// accidental "~" directory created under the CWD).
func TestScanTildeCatalogNotCreated(t *testing.T) {
	root := t.TempDir()
	t.Chdir(t.TempDir()) // the literal "~/..." would resolve HERE if it ever reached the filesystem

	stdout, stderr, code := runCLIChild(t, "scan", "--catalog", "~/never-created.db", root)
	if code != report.ExitUsage {
		t.Fatalf("exit code = %d, want 2 (usage)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "--catalog") || !strings.Contains(stderr, "~/never-created.db") {
		t.Errorf("stderr does not name --catalog and the literal value:\n%s", stderr)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(wd, "~", "never-created.db")); !os.IsNotExist(statErr) {
		t.Errorf("tilde rejection still produced a catalog file under the CWD (stat err = %v)", statErr)
	}
}

// --- through the REAL commands: missing parent directory -> exit 4 -----------

// A catalog path whose parent directory does not exist names THAT
// directory in the error and keeps the catalog class (exit 4) for every
// command: scan (whose Create path fails before any auto-create
// writes), plan, report (read-only open), and clear-assignments - the
// catalog-side enrichment covers them uniformly. The directory itself
// is never created (PM decision D2: no auto-creation of parents).
func TestMissingCatalogParentDirNamedThroughCommands(t *testing.T) {
	root := t.TempDir() // a real, absolute scan ROOT
	missingDir := filepath.Join(t.TempDir(), "nonexistent-dir-xyz")
	catalogPath := filepath.Join(missingDir, "test.db")

	cases := []struct {
		name string
		args []string
	}{
		{"scan", []string{"scan", "--catalog", catalogPath, root}},
		{"plan", []string{"plan", "--catalog", catalogPath, "--set-id", "S", "--size", "25"}},
		{"report", []string{"report", "--catalog", catalogPath, "--set-id", "S", "--size", "25"}},
		{"clear-assignments", []string{"clear-assignments", "--catalog", catalogPath, "--set-id", "S"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := runCLIChild(t, tc.args...)
			if code != report.ExitCatalog {
				t.Fatalf("exit code = %d, want 4 (catalog)\nstdout: %s\nstderr: %s", code, stdout, stderr)
			}
			if !strings.Contains(stderr, "nonexistent-dir-xyz") {
				t.Errorf("stderr does not name the missing directory:\n%s", stderr)
			}
			if !strings.Contains(stderr, "catalog directory does not exist") {
				t.Errorf("stderr lacks the missing-directory diagnosis:\n%s", stderr)
			}
			if stdout != "" {
				t.Errorf("stdout must stay empty on errors (FR-046), got: %q", stdout)
			}
		})
	}
	if _, statErr := os.Stat(missingDir); !os.IsNotExist(statErr) {
		t.Fatalf("a missing parent directory was created (stat err = %v); D2 forbids auto-creation", statErr)
	}
}

// Regression guard alongside the enrichment: a VALID existing parent
// directory still works end to end - scan auto-creates the catalog
// there and succeeds (the T1.6 PM-ratified auto-create contract; the
// deep form lives in scan_hardening_test.go's
// TestScanMissingCatalogAutoCreates, this pins the no-interference
// property next to the missing-parent cases above).
func TestScanAutoCreateWithExistingParentStillWorks(t *testing.T) {
	root := t.TempDir()
	cat := filepath.Join(t.TempDir(), "auto-created.sqlite")

	stdout, stderr, code := runCLIChild(t, "scan", "--catalog", cat, root)
	if code != report.ExitOK {
		t.Fatalf("scan with missing catalog in EXISTING dir exit = %d, want 0 (auto-create)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if strings.Contains(stderr, "catalog directory does not exist") {
		t.Errorf("existing-parent auto-create wrongly enriched:\n%s", stderr)
	}
	if _, err := os.Stat(cat); err != nil {
		t.Errorf("catalog was not auto-created in the existing directory: %v", err)
	}
}
