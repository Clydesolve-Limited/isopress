package cli

// Gap-closure tests for the D1 leading-tilde path detection (checkTilde,
// paths.go; PM decision D1 of 2026-10-19) and for the cli-side
// classification contract of the D2 missing-parent enrichment. The
// baseline acceptance suite lives in paths_test.go (trigger rule,
// actionable message, every path-typed input through the real commands,
// the stakeholder repro child process, the four catalog commands'
// missing-parent exit 4, and the auto-create regression); this file
// pins the remaining audit points:
//
//   - trigger-rule corners the baseline list omits: "~//double" and the
//     bare "~/ " token ("~/ with nothing after" IS "~/"), plus Unicode
//     homoglyph tildes as DOCUMENTED non-triggers;
//   - the root-level validation reaches the FIVE STUB commands' global
//     --catalog too (copy/build/verify-media/validate/restore share
//     root's PersistentPreRunE, so the check is command-agnostic);
//   - flag-position independence: the tilde value after other parsed
//     flags, in --flag=value form, and a ROOT positional followed by
//     another flag all still reject with exit 2;
//   - the enriched missing-parent error still classifies to exit 4 via
//     report.Classify directly (errors.As chain through the enrichment
//     wrap) and unwraps to fs.ErrNotExist, from the cli layer against
//     the real catalog API.

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"isopress/internal/catalog"
	"isopress/internal/report"
)

// TestCheckTildeEdgeCasesBeyondBaseline adds the trigger-rule corners
// not covered by TestCheckTildeTriggerRule in paths_test.go:
//
//   - "~//double" (an empty path segment right after the "~/" prefix):
//     still the unambiguous "~/..." form, so it TRIGGERS - the shell
//     would have expanded the leading ~ regardless of what follows;
//   - "~/" with nothing after it: the same token as the baseline's
//     trailing-slash case ("~/" IS "~/ with nothing after"); re-asserted
//     here so the bare-dir form is pinned next to its corners;
//   - Unicode homoglyph tildes (U+02DC SMALL TILDE, U+223C TILDE
//     OPERATOR, U+FF5E FULLWIDTH TILDE): NON-TRIGGERS, deliberately -
//     shell tilde expansion recognizes ONLY the ASCII byte 0x7E at the
//     start of a word, so a homoglyph-leading path would never be
//     expanded by any shell; it is a legitimate literal filename (e.g.
//     CJK-locale paths using the fullwidth tilde) that the tool must
//     honor as-is, exactly like any other relative path.
func TestCheckTildeEdgeCasesBeyondBaseline(t *testing.T) {
	trigger := []string{
		"~//double",  // empty segment after the "~/" prefix: still unambiguous
		"~/",         // bare-dir form; equivalently "~/ with nothing after"
		"~///triple", // same, deeper
	}
	for _, value := range trigger {
		if err := checkTilde(value, "--catalog"); err == nil {
			t.Errorf("checkTilde(%q) = nil, want an error (unambiguous ~/... prefix)", value)
		}
	}

	nonTrigger := []string{
		"˜/homoglyph-small-tilde",         // U+02DC: not ASCII 0x7E, never shell-expanded
		"∼/homoglyph-tilde-op",            // U+223C: same
		"～/homoglyph-fullwidth",           // U+FF5E: same (CJK-locale literal paths)
		"˜~/leading-homoglyph-then-ascii", // U+02DC first: the ASCII ~ is NOT leading, no trigger
		"mid/˜/homoglyph-mid",             // homoglyph mid-path: doubly harmless
	}
	for _, value := range nonTrigger {
		if err := checkTilde(value, "--catalog"); err != nil {
			t.Errorf("checkTilde(%q) = %v, want nil (a homoglyph is not a shell tilde)", value, err)
		}
	}
}

// TestStubCommandsCatalogTildeRejected proves the tilde validation is
// ROOT-level and command-agnostic: the commands exercised here (the
// still-stub verify-media, validate, restore, plus `build`, real since
// T4.3) share the root's PersistentPreRunE, so a leading-tilde
// --catalog is rejected with exit 2 BEFORE the command's body runs -
// the check can never be lost when a stub is promoted to a real
// command, because it does not live in any command implementation.
// (copy graduated to a real command at T3.3 and build at T4.3; the
// root-level guarantee they rely on is the same one.)
func TestStubCommandsCatalogTildeRejected(t *testing.T) {
	for _, stub := range []string{"build", "verify-media", "validate", "restore"} {
		t.Run(stub, func(t *testing.T) {
			stdout, stderr, code := runInProcess(t, stub, "--catalog", "~/x.db")
			if code != report.ExitUsage {
				t.Fatalf("%s --catalog '~/x.db': exit = %d, want 2 (usage)\nstdout: %s\nstderr: %s",
					stub, code, stdout, stderr)
			}
			if !strings.Contains(stderr, "--catalog") || !strings.Contains(stderr, "~/x.db") {
				t.Errorf("stderr does not name --catalog and the literal value:\n%s", stderr)
			}
		})
	}
}

// TestTildeRejectedIndependentOfFlagPosition pins flag-position
// independence: pflag parses values before the validation sees them, so
// the tilde check fires identically when the path value sits after other
// parsed flags, in the --flag=value spelling, and on the ROOT positional
// FOLLOWED by another flag (interspersed parsing). All cases exit 2 with
// the offending input named.
func TestTildeRejectedIndependentOfFlagPosition(t *testing.T) {
	root := t.TempDir()
	cleanCatalog := filepath.Join(t.TempDir(), "clean.sqlite")

	cases := []struct {
		name  string
		args  []string
		input string
		value string
	}{
		{"--catalog value third, after bool flags",
			[]string{"scan", "--quiet", "--json", "--catalog", "~/x.db", root},
			"--catalog", "~/x.db"},
		{"--catalog=value equals form",
			[]string{"scan", "--catalog=~/equals.db", root},
			"--catalog", "~/equals.db"},
		{"--catalog last after real flags",
			[]string{"plan", "--set-id", "S", "--size", "25", "--catalog", "~/x.db"},
			"--catalog", "~/x.db"},
		{"ROOT positional followed by another flag",
			[]string{"scan", "--catalog", cleanCatalog, "~/data", "--strict"},
			"ROOT", "~/data"},
		{"bare-tilde ROOT positional",
			[]string{"scan", "--catalog", cleanCatalog, "~"},
			"ROOT", "~"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code := runInProcess(t, tc.args...)
			if code != report.ExitUsage {
				t.Fatalf("exit = %d, want 2 (usage)\nstderr: %s", code, stderr)
			}
			if !strings.Contains(stderr, tc.input) {
				t.Errorf("stderr does not name the input %q:\n%s", tc.input, stderr)
			}
			if !strings.Contains(stderr, tc.value) {
				t.Errorf("stderr does not echo the literal value %q:\n%s", tc.value, stderr)
			}
		})
	}
}

// TestEnrichedMissingParentClassifiesCatalogExit is the cli-layer mirror
// of the catalog-side chain test (enrich_gaps_test.go): the D2-enriched
// missing-parent error, obtained through the REAL catalog.Open API,
// still classifies to exit 4 through report.Classify - the enrichment
// wrap keeps the *CatalogError in the errors.As chain - and still
// unwraps to fs.ErrNotExist for automation that keys on the errno.
func TestEnrichedMissingParentClassifiesCatalogExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-dir", "catalog.sqlite")

	for _, mode := range []catalog.OpenMode{catalog.OpenReadWrite, catalog.OpenReadOnly} {
		_, err := catalog.Open(path, mode)
		if err == nil {
			t.Fatalf("catalog.Open(missing dir, mode %d) succeeded, want an error", mode)
		}
		if code := report.Classify(err); code != report.ExitCatalog {
			t.Errorf("report.Classify(enriched error, mode %d) = %d, want 4 (catalog class preserved "+
				"through the enrichment wrap)\nerr: %v", mode, code, err)
		}
		var cerr *catalog.CatalogError
		if !errors.As(err, &cerr) || cerr.Kind != catalog.KindOpen {
			t.Errorf("enriched error (mode %d) is not a KindOpen *CatalogError: %v", mode, err)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("enriched error (mode %d) lost fs.ErrNotExist from the wrap chain: %v", mode, err)
		}
		if !strings.Contains(err.Error(), "missing-dir") {
			t.Errorf("enriched error (mode %d) does not name the missing directory: %v", mode, err)
		}
	}
}
