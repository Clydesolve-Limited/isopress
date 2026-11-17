package cli

// QUALITY-GATE CONFIG PARITY pins (the deps_test.go go.mod-pin
// precedent: repo-root gate files are asserted, not trusted).
//
// Two T6.5 acceptance properties have no enforcement until now and
// were verified by hand only:
//
//  1. .golangci.yml stays the MINIMAL config the fix cycle was
//     ratified with: ONE exclusion (errcheck, _test.go files only),
//     no linter disabled, no linter set redefined. A future edit that
//     quietly widens the exclusion - or turns a default linter off -
//     fails here instead of at the next real bug it misses.
//  2. The Makefile gate targets keep running exactly what README.md's
//     "Development and testing" section documents. That pairing
//     already drifted once (the README/Makefile micro-sync of T6.5
//     ratification (3)), so the recipes are asserted verbatim on both
//     sides rather than re-read by a human each time.

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestGolangciConfigExcludesErrcheckForTestsOnly pins the ONE sanctioned
// exclusion of .golangci.yml (T6.5 part C) and nothing else.
func TestGolangciConfigExcludesErrcheckForTestsOnly(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(moduleRoot, ".golangci.yml"))
	if err != nil {
		t.Fatalf("read .golangci.yml: %v", err)
	}

	// Strip shell-style comments so the (rationale-heavy) comment block
	// never satisfies - or breaks - a structural assertion.
	var kept []string
	for _, line := range strings.Split(string(raw), "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		if strings.TrimSpace(line) != "" {
			kept = append(kept, line)
		}
	}
	cfg := strings.Join(kept, "\n")

	if !strings.Contains(cfg, `version: "2"`) {
		t.Errorf(".golangci.yml must stay a golangci-lint v2 config (version: \"2\") - a v1 file would be rejected or silently misread")
	}

	// Exactly ONE exclusion rule exists...
	if n := strings.Count(cfg, "- path:"); n != 1 {
		t.Errorf(".golangci.yml carries %d exclusion rules, want exactly 1 (the test-only errcheck rule); any further exclusion must be ratified, not slipped in", n)
	}
	// ...and it targets _test.go files only.
	ruleAt := strings.Index(cfg, "- path:")
	if ruleAt < 0 {
		t.Fatalf("no exclusion rule found in .golangci.yml:\n%s", cfg)
	}
	if n := strings.Count(cfg, `_test\.go`); n != 1 {
		t.Errorf(".golangci.yml mentions _test\\.go %d times, want exactly 1 (the single sanctioned exclusion path)", n)
	}
	// ...and the linters list that follows the rule's path line is
	// exactly [errcheck]: no second linter may ride along inside the
	// rule.
	rest := cfg[ruleAt:]
	if !strings.HasPrefix(rest, "- path: '_test\\.go'") {
		t.Errorf("the single exclusion rule does not target _test.go files; its first line is: %q",
			strings.SplitN(rest, "\n", 2)[0])
	}
	_, afterPath, ok := strings.Cut(rest, "\n")
	if !ok {
		t.Fatalf(".golangci.yml's exclusion rule has no linters list:\n%s", rest)
	}
	linterRE := regexp.MustCompile(`(?m)^\s*-\s*([A-Za-z0-9_]+)\s*$`)
	var excluded []string
	for _, m := range linterRE.FindAllStringSubmatch(afterPath, -1) {
		excluded = append(excluded, m[1])
	}
	if len(excluded) != 1 || excluded[0] != "errcheck" {
		t.Errorf("the _test.go exclusion lists linters %v, want exactly [errcheck] - no other linter may be excluded for tests", excluded)
	}

	// No construct that changes WHICH linters run: neither a linter
	// disable list nor a `default:` redefinition of the default set
	// (golangci-lint v2's `default: none` turns every default linter
	// off in one line - the silent-disabling shape this pin exists for).
	for _, forbidden := range []string{"disable", "default:"} {
		if strings.Contains(cfg, forbidden) {
			t.Errorf(".golangci.yml contains %q: only the single test-only errcheck exclusion is sanctioned (no linter may be disabled or the default set redefined)", forbidden)
		}
	}
}

// makeTargetRE matches the Makefile's (single-line) target headers.
var makeTargetRE = regexp.MustCompile(`(?m)^([a-z][a-z0-9-]*):`)

// gateRecipes are the single-line gate commands that must appear
// VERBATIM in both the Makefile recipe and the README's Development
// section (README: "the first column pairs each target with the recipe
// it runs, copied verbatim from the Makefile").
//
// The match is EXACT on both sides - a whole tab-prefixed Makefile
// line, a whole backtick-wrapped README span - because a substring
// pin silently accepts any LONGER command: when `make test` gained
// `-timeout 30m` while the pin still said `go test ./...`, both
// Contains checks kept passing on that prefix and the README lag was
// not test-gated (flagged twice by the CI-hardening rounds). Exact
// matching makes the pairing real for all five recipes: a future
// `go vet ./... -extra` or a suffixed README row fails here.
var gateRecipes = map[string]string{
	"test":      "go test ./... -timeout 30m",
	"vet":       "go vet ./...",
	"test-e2e":  "go test -tags e2e ./... -timeout 30m",
	"test-perf": "go test -tags perf ./... -timeout 10m -run '^TestPerf'",
	"test-race": "go test -tags e2e -race -timeout 30m -count=1 ./...",
}

// TestMakefileGateTargetsDocumentedInReadme pins the make<->README
// pairing of the Development and testing section: every Makefile
// target is named there, and each single-line gate recipe appears
// byte-identical on both sides.
func TestMakefileGateTargetsDocumentedInReadme(t *testing.T) {
	mkBytes, err := os.ReadFile(filepath.Join(moduleRoot, "Makefile"))
	if err != nil {
		t.Fatalf("read Makefile: %v", err)
	}
	readmeBytes, err := os.ReadFile(filepath.Join(moduleRoot, "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	mk := string(mkBytes)
	readme := string(readmeBytes)
	mkRecipes := makeRecipes(mk)

	var targets []string
	for _, m := range makeTargetRE.FindAllStringSubmatch(mk, -1) {
		targets = append(targets, m[1])
	}
	if len(targets) < 8 {
		t.Fatalf("Makefile target headers found = %v, want the full gate set (build/test/vet/test-e2e/test-perf/test-race/verify/vulncheck/lint/help)", targets)
	}
	for _, target := range targets {
		if !strings.Contains(readme, "`"+target+"`") && !strings.Contains(readme, "make "+target) {
			t.Errorf("Makefile target %q is not documented in README.md's Development and testing section", target)
		}
	}

	for target, recipe := range gateRecipes {
		// Makefile side: the target's OWN first recipe line must equal
		// the pin exactly (anchored to the target header, so the bare
		// `go vet ./...` line inside `verify` cannot satisfy a drifted
		// `vet` recipe - a plain Contains would let `go vet ./... -extra`
		// slip through on that second occurrence).
		got, ok := mkRecipes[target]
		if !ok {
			t.Errorf("Makefile has no %q target (gate set changed?) - the README table pairs it with %q", target, recipe)
		} else if got != recipe {
			t.Errorf("Makefile %s recipe = %q, want %q - the README table quotes this command verbatim; change both together", target, got, recipe)
		}
		// README side: the recipe must appear as the TABLE ROW's whole
		// parenthesised span - `(` + backtick + recipe + backtick + `)`.
		// Backtick-wrapping makes the match exact in both directions (a
		// longer README command lacks the closing backtick after the
		// pin's last byte; a shorter one lacks the pin's suffix - the
		// plain Contains that let the stale `go test ./...` row pass the
		// `-timeout 30m` pin), and the parens anchor it to the pairing
		// ROW itself, so a second occurrence elsewhere in the prose
		// (README's `go vet ./...` sentence) cannot mask a drifted row.
		if !strings.Contains(readme, "(`"+recipe+"`)") {
			t.Errorf("README.md's gate table no longer documents %q (the `make %s` recipe) - the README/Makefile pairing must not drift again", recipe, target)
		}
	}
}

// makeRecipes maps each Makefile target header to its FIRST recipe
// line (every gate in gateRecipes is a single-line target; later
// recipe lines of multi-line targets like verify are ignored). A line
// is a recipe only while it follows a target header and starts with a
// tab, so GOPATH_BIN assignments, comments and .PHONY never match.
func makeRecipes(mk string) map[string]string {
	recipes := make(map[string]string)
	cur := ""
	for _, line := range strings.Split(mk, "\n") {
		if m := makeTargetRE.FindStringSubmatch(line); m != nil {
			cur = m[1]
			continue
		}
		if cur != "" && strings.HasPrefix(line, "\t") {
			if _, seen := recipes[cur]; !seen {
				recipes[cur] = strings.TrimPrefix(line, "\t")
			}
		}
	}
	return recipes
}
