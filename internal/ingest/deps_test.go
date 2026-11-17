package ingest

// T1.3 dependency conditions (DESIGN_SPEC 11 gate): internal/ingest is
// pure standard library plus first-party internal packages only. The
// scan pipeline must not add a single third-party module: the glob
// engine is in-tree (regex translation in matcher.go), and no glob
// library is permitted (T1.3 explicit constraint). The check parses the
// import declarations of every Go file in this package and rejects any
// import path that is neither a standard library package nor prefixed
// with "isopress/".

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// isStdlibImport reports whether an import path is a standard library
// package: no dot in the first path element ("fmt", "io/fs", "regexp"
// are stdlib; "example.com/x" and "github.com/y" are not). Module paths
// containing a dot in the domain element can never collide with stdlib
// element names, which by Go convention never contain dots.
func isStdlibImport(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

// TestNoThirdPartyImports asserts that every import in internal/ingest
// is standard library or first-party (isopress/...).
func TestNoThirdPartyImports(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package dir: %v", err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(".", name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		checked++
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if !isStdlibImport(path) && !strings.HasPrefix(path, "isopress/") {
				t.Errorf("%s imports %q: internal/ingest must be pure stdlib + first-party internal (DESIGN_SPEC 11, no glob libraries)",
					name, path)
			}
		}
	}
	if checked < 3 { // doc.go + matcher.go + walker.go, minimum
		t.Fatalf("only %d Go files checked; the test's file discovery is broken", checked)
	}
}
