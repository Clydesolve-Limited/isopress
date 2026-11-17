package xorriso

// T4.1 structural tests (DESIGN_SPEC 1.2 layering rules, 10.1
// DependencyError, IMPLEMENTATION_PLAN T4.1 acceptance "A grep assertion
// that no service package imports os/exec directly (only internal/xorriso
// does)" and the no-argument-injection structural pin).
//
// The structural claim being pinned: this package never invokes a shell.
// It is asserted by parsing the REAL import declarations and call
// expressions of the package's non-test files:
//   - the only process-spawning calls are exec.CommandContext, and their
//     command argument is never a string literal (so no hardcoded shell
//     or any other fixed binary can appear);
//   - no string literal anywhere in the package names a shell
//     (/bin/sh, sh, bash, ...).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// moduleRoot is the repository root relative to internal/xorriso.
var moduleRoot = filepath.Join("..", "..")

// packageDir is this package's directory (non-test sources only are
// parsed by the structural checks).
const packageDir = "."

// nonTestGoFiles lists the package's production .go files.
func nonTestGoFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		files = append(files, filepath.Join(dir, e.Name()))
	}
	if len(files) == 0 {
		t.Fatalf("no production .go files found in %s", dir)
	}
	return files
}

// shellNames are the string literals that would indicate a shell
// invocation anywhere in the package.
var shellNames = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "csh": true,
}

// TestPackageNeverInvokesShell is the structural no-injection pin:
// production code in this package spawns exactly one kind of process
// (exec.CommandContext), always with a non-literal command argument, and
// no string literal names a shell. Combined with Run taking a direct
// argv slice, this is what makes argument injection structurally
// impossible rather than merely untriggered.
func TestPackageNeverInvokesShell(t *testing.T) {
	fset := token.NewFileSet()
	sawCommandContext := 0

	for _, file := range nonTestGoFiles(t, packageDir) {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}

		// Every string literal must be shell-free.
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			base := strings.ToLower(strings.TrimPrefix(s, "/usr/bin/"))
			base = strings.TrimPrefix(base, "/bin/")
			if shellNames[base] || strings.Contains(s, "/bin/sh") || strings.Contains(s, "/bin/bash") {
				t.Errorf("%s: string literal %q names a shell; this package must never invoke one", file, lit.Value)
			}
			return true
		})

		// Process spawning: exec.Command is banned (everything goes
		// through CommandContext for cancellation), and the command
		// argument must never be a hardcoded string literal.
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkgIdent, ok := sel.X.(*ast.Ident)
			if !ok || pkgIdent.Name != "exec" {
				return true
			}
			switch sel.Sel.Name {
			case "Command":
				t.Errorf("%s: calls exec.Command; only exec.CommandContext is allowed (ctx plumbing everywhere)", file)
			case "CommandContext":
				sawCommandContext++
				if len(call.Args) < 2 {
					t.Errorf("%s: exec.CommandContext with %d args, want at least (ctx, path, argv...)", file, len(call.Args))
					return true
				}
				if lit, ok := call.Args[1].(*ast.BasicLit); ok {
					t.Errorf("%s: exec.CommandContext command argument is the string literal %s; it must be the resolved Tool.Path variable so no fixed binary (especially no shell) can be spawned", file, lit.Value)
				}
			}
			return true
		})
	}

	if sawCommandContext == 0 {
		t.Fatal("no exec.CommandContext call found in the package; the structural pin has rotted - update it deliberately alongside the code")
	}
}

// TestNoExecImportOutsideXorrisoPackage is the T4.1 acceptance grep
// assertion: internal/xorriso is the ONLY package in the repository that
// imports os/exec. Services (ingest, plan, copyengine, imagebuild,
// verify, restore) and every other layer depend on interfaces per
// DESIGN_SPEC 1.2; test files are exempt because they re-exec the test
// binary for two-process fixtures (DESIGN_SPEC 12.3, 12.7) and invoke
// build tooling.
func TestNoExecImportOutsideXorrisoPackage(t *testing.T) {
	const banned = "os/exec"
	err := filepath.WalkDir(moduleRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") || strings.HasSuffix(d.Name(), "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(moduleRoot, path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(filepath.ToSlash(rel), "internal/xorriso/") {
			return nil // the one allowed home of os/exec
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			importPath, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if importPath == banned {
				t.Errorf("%s imports %s; only internal/xorriso may exec external processes (DESIGN_SPEC 1.2, T4.1 acceptance)", rel, banned)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking repository for imports: %v", err)
	}
}
