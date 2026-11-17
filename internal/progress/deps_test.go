package progress

// T1.1 dependency conditions (DESIGN_SPEC 11 gate, NOTES.md dependency
// evaluation): go.mod pins github.com/schollz/progressbar/v3 at
// v3.19.1 EXACTLY and golang.org/x/term at v0.47.0 exactly;
// golang.org/x/sys is an indirect dependency only, lifted by MVS to
// v0.49.0 when x/term v0.47.0 landed at this task (the scheduled bump
// recorded in NOTES.md); and progressbar may be imported ONLY inside
// internal/progress (the binding condition "confined strictly behind
// the internal/progress event-bus seam"). The go.mod checks follow the
// exact-pin pattern established by the catalog (T0.3) and cli (T0.5)
// dependency tests; the resolved-version guard uses `go list -m`.

import (
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// moduleRoot is the repository root relative to internal/progress.
var moduleRoot = filepath.Join("..", "..")

// TestGoModPinsProgressbarAndTermExactly asserts the exact approved
// pins of DESIGN_SPEC 11 in go.mod: progressbar v3.19.1 and x/term
// v0.47.0, each on exactly one require line (a range or a second
// version is a gate violation).
func TestGoModPinsProgressbarAndTermExactly(t *testing.T) {
	gomod, err := os.ReadFile(filepath.Join(moduleRoot, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	checks := []struct {
		name    string
		exactRe string
	}{
		{
			name:    "github.com/schollz/progressbar/v3",
			exactRe: `(?m)^[ \t]*github\.com/schollz/progressbar/v3[ \t]+v3\.19\.1[ \t]*(// .*)?$`,
		},
		{
			name:    "golang.org/x/term",
			exactRe: `(?m)^[ \t]*golang\.org/x/term[ \t]+v0\.47\.0[ \t]*(// .*)?$`,
		},
	}
	for _, c := range checks {
		if !regexp.MustCompile(c.exactRe).Match(gomod) {
			t.Errorf("go.mod does not pin %s at the approved exact version:\n%s", c.name, gomod)
		}
		anyVersion := regexp.MustCompile(`(?m)^[ \t]*` +
			regexp.QuoteMeta(c.name) + `[ \t]+v[0-9]`)
		if n := len(anyVersion.FindAll(gomod, -1)); n != 1 {
			t.Errorf("go.mod has %d %s require lines, want exactly 1", n, c.name)
		}
	}
}

// TestGoModSysIndirectLiftedToV049 asserts the scheduled x/sys bump
// from the NOTES.md dependency gate landed with this task: go.mod
// carries golang.org/x/sys v0.49.0 on exactly one require line, and the
// line is marked "// indirect" - x/sys must remain an indirect
// dependency (no direct imports in V1, DESIGN_SPEC 11).
func TestGoModSysIndirectLiftedToV049(t *testing.T) {
	gomod, err := os.ReadFile(filepath.Join(moduleRoot, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	exact := regexp.MustCompile(`(?m)^[ \t]*golang\.org/x/sys[ \t]+v0\.49\.0[ \t]+// indirect$`)
	if !exact.Match(gomod) {
		t.Errorf("go.mod does not carry golang.org/x/sys v0.49.0 as indirect-only:\n%s", gomod)
	}
	anyVersion := regexp.MustCompile(`(?m)^[ \t]*golang\.org/x/sys[ \t]+v[0-9]`)
	if n := len(anyVersion.FindAll(gomod, -1)); n != 1 {
		t.Errorf("go.mod has %d golang.org/x/sys require lines, want exactly 1", n)
	}
}

// TestResolvedModuleVersions guards the module graph, not just the
// text of go.mod: `go list -m` must resolve progressbar v3.19.1,
// x/term v0.47.0, and x/sys v0.49.0 (the MVS-lifted indirect).
func TestResolvedModuleVersions(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; skipping resolved-version guard")
	}
	want := map[string]string{
		"github.com/schollz/progressbar/v3": "v3.19.1",
		"golang.org/x/term":                 "v0.47.0",
		"golang.org/x/sys":                  "v0.49.0",
	}
	for mod, ver := range want {
		cmd := exec.Command("go", "list", "-m", mod)
		cmd.Dir = moduleRoot
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list -m %s failed: %v", mod, err)
		}
		fields := strings.Fields(string(out))
		if len(fields) != 2 || fields[1] != ver {
			t.Errorf("go list -m %s = %q, want resolved version %s", mod, strings.TrimSpace(string(out)), ver)
		}
	}
}

// TestNoProgressbarImportOutsideInternalProgress parses the import
// declarations of every Go file in the repository and asserts that
// github.com/schollz/progressbar/v3 appears ONLY under internal/progress
// (DESIGN_SPEC 11 binding condition, same AST-walk style as the
// cobra/doc and x/sys/unix bans of S0). The seam is what keeps the
// plain and silent renderers library-independent (DESIGN_SPEC 9.4).
func TestNoProgressbarImportOutsideInternalProgress(t *testing.T) {
	const banned = "github.com/schollz/progressbar/v3"
	const allowedDir = "internal/progress"
	err := filepath.WalkDir(moduleRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip VCS metadata and vendored trees; everything else
			// in this repository is first-party code under review.
			if d.Name() == ".git" || d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		rel, err := filepath.Rel(moduleRoot, path)
		if err != nil {
			return err
		}
		inAllowed := filepath.ToSlash(filepath.Dir(rel)) == allowedDir
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range file.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			if importPath == banned && !inAllowed {
				t.Errorf("%s imports %s; the DESIGN_SPEC 11 gate confines progressbar strictly to %s",
					path, banned, allowedDir)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking repository for imports: %v", err)
	}
}
