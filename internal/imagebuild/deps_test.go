package imagebuild

// T4.2 structural tests: the DESIGN_SPEC 11 dependency gate at the
// imagebuild seam. The free-space pre-check uses stdlib syscall.Statfs
// (the catalog lock's syscall.Flock precedent); a direct
// golang.org/x/sys import would silently promote x/sys from indirect
// to direct and bypass the approved-dependency conditions, so the ban
// is AST-asserted over the REAL import declarations of every package
// file - the established pattern (internal/catalog lock_test.go).

import (
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestNoDirectXSysUnixImport pins the dependency gate: no package file
// (test or not) directly imports golang.org/x/sys/unix; only stdlib
// syscall is sanctioned for Statfs/Flock-shaped needs.
func TestNoDirectXSysUnixImport(t *testing.T) {
	const banned = "golang.org/x/sys/unix"
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	checked := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		checked++
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, e.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("unquote import path in %s: %v", e.Name(), err)
			}
			if path == banned {
				t.Errorf("%s imports %s; only stdlib syscall is allowed (DESIGN_SPEC 11)", e.Name(), banned)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no .go files found; the gate has rotted")
	}
}
