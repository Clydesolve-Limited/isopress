package cli

// T0.5 dependency conditions (DESIGN_SPEC 11 gate, NOTES.md dependency
// evaluation): go.mod pins spf13/cobra at v1.10.2 and spf13/pflag at
// v1.0.10 exactly; the pflag floor is v1.0.9 and v1.0.8 (the breaking
// ParseErrorsWhitelist rename) must never appear; and no file anywhere in
// the repository imports github.com/spf13/cobra/doc (it would pull
// go-md2man, blackfriday, and yaml into linked dependencies, requiring
// separate evaluation). The go.mod checks follow the pattern established
// by the catalog package's sqlite pin test (T0.3).

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// moduleRoot is the repository root relative to internal/cli.
var moduleRoot = filepath.Join("..", "..")

// TestGoModPinsCobraAndPflagExactly asserts the exact approved pins of
// DESIGN_SPEC 11 in go.mod: cobra v1.10.2 and pflag v1.0.10, each on
// exactly one require line (a range or a second version is a gate
// violation). The pflag require may carry the "// indirect" marker (only
// cobra imports pflag directly), which is still an exact pin.
func TestGoModPinsCobraAndPflagExactly(t *testing.T) {
	gomod, err := os.ReadFile(filepath.Join(moduleRoot, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}

	checks := []struct {
		name    string
		exactRe string
		anyRe   string
	}{
		{
			name:    "cobra",
			exactRe: `(?m)^[ \t]*github\.com/spf13/cobra[ \t]+v1\.10\.2[ \t]*(// .*)?$`,
			anyRe:   `(?m)^[ \t]*github\.com/spf13/cobra[ \t]+v[0-9]`,
		},
		{
			name:    "pflag",
			exactRe: `(?m)^[ \t]*github\.com/spf13/pflag[ \t]+v1\.0\.10[ \t]*(// .*)?$`,
			anyRe:   `(?m)^[ \t]*github\.com/spf13/pflag[ \t]+v[0-9]`,
		},
	}
	for _, c := range checks {
		if !regexp.MustCompile(c.exactRe).Match(gomod) {
			t.Errorf("go.mod does not pin github.com/spf13/%s at the approved exact version:\n%s", c.name, gomod)
		}
		if n := len(regexp.MustCompile(c.anyRe).FindAll(gomod, -1)); n != 1 {
			t.Errorf("go.mod has %d github.com/spf13/%s require lines, want exactly 1", n, c.name)
		}
	}
}

// TestPflagVersionAboveBreakingFloor parses the pinned pflag version from
// go.mod and asserts it is >= v1.0.9 and never v1.0.8 (DESIGN_SPEC 11:
// v1.0.8 shipped the breaking ParseErrorsWhitelist ->
// ParseErrorsAllowlist rename).
func TestPflagVersionAboveBreakingFloor(t *testing.T) {
	gomod, err := os.ReadFile(filepath.Join(moduleRoot, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	m := regexp.MustCompile(`(?m)^[ \t]*github\.com/spf13/pflag[ \t]+v([0-9]+)\.([0-9]+)\.([0-9]+)`).
		FindSubmatch(gomod)
	if m == nil {
		t.Fatal("go.mod has no github.com/spf13/pflag require line to parse")
	}
	var parts [3]int
	for i, group := range m[1:] {
		v, err := strconv.Atoi(string(group))
		if err != nil {
			t.Fatalf("pflag version component %q is not numeric: %v", group, err)
		}
		parts[i] = v
	}
	floor := [3]int{1, 0, 9}
	never := [3]int{1, 0, 8}
	if parts == never {
		t.Fatalf("go.mod pins pflag v1.0.8, which is FORBIDDEN (breaking rename; DESIGN_SPEC 11)")
	}
	if compareTriple(parts, floor) < 0 {
		t.Fatalf("go.mod pins pflag v%d.%d.%d, below the v1.0.9 floor (DESIGN_SPEC 11)", parts[0], parts[1], parts[2])
	}
}

// compareTriple orders version triples lexicographically.
func compareTriple(a, b [3]int) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

// TestNoCobraDocImportAnywhere parses the import declarations of every Go
// file in the repository and asserts none imports
// github.com/spf13/cobra/doc (DESIGN_SPEC 11 binding condition; the same
// import-parsing style as the catalog package's x/sys/unix assertion,
// extended to the whole repository because the ban is repo-wide).
func TestNoCobraDocImportAnywhere(t *testing.T) {
	const banned = "github.com/spf13/cobra/doc"
	err := filepath.WalkDir(moduleRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip VCS metadata and vendored trees; everything else in
			// this repository is first-party code under review.
			if d.Name() == ".git" || d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range file.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			if importPath == banned {
				t.Errorf("%s imports %s; the DESIGN_SPEC 11 gate forbids cobra/doc until man-page generation is separately evaluated", path, banned)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking repository for imports: %v", err)
	}
}
