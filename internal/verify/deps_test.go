package verify

// T5.1 structural test (test-only): the layering contract of the
// compareContents move (IMPLEMENTATION_PLAN 5.5 resolution 4).
//
// internal/verify is the LOW-LEVEL side of the comparison: it must
// never import internal/imagebuild (that would close the cycle imagebuild
// already depends on verify through), and imagebuild must reach the
// comparison through verify's types (type aliases + a delegate), not by
// carrying a second copy of them. Both directions are asserted from the
// REAL source files, so a silent re-fork of the comparison fails here.

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestVerifyNeverImportsImagebuild pins the import direction at the
// verify seam: no package file (test or not) may import
// isopress/internal/imagebuild.
func TestVerifyNeverImportsImagebuild(t *testing.T) {
	const banned = "isopress/internal/imagebuild"
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
				t.Errorf("%s imports %s; internal/verify must never import internal/imagebuild (the dependency runs the other way)",
					e.Name(), banned)
			}
		}
	}
	if checked == 0 {
		t.Fatal("no .go files found; the gate has rotted")
	}
}

// TestImagebuildReachesComparisonThroughVerify pins the receiving half
// of the move: imagebuild TYPE-ALIASES verify.ContentRow and delegates
// its shim to verify.CompareContents (the T4.3 pin tests keep calling
// the package-local compareContents and stay byte-identical-green
// through that one-line delegate). A second, forked copy of either in
// internal/imagebuild breaks this pin.
func TestImagebuildReachesComparisonThroughVerify(t *testing.T) {
	const buildGo = "../imagebuild/build.go"
	src, err := os.ReadFile(buildGo)
	if err != nil {
		t.Fatalf("reading %s: %v", buildGo, err)
	}
	pins := []string{
		"type ContentRow = verify.ContentRow",
		"return verify.CompareContents(rows, extracted, targets).Problems()",
	}
	for _, want := range pins {
		if !strings.Contains(string(src), want) {
			t.Errorf("%s lacks the delegate line %q (the comparison must live in internal/verify)", buildGo, want)
		}
	}
	// No forked ContentRow definition may reappear in imagebuild (the
	// alias above is the only spelling allowed).
	entries, err := os.ReadDir(filepath.Dir(buildGo))
	if err != nil {
		t.Fatalf("read imagebuild dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(filepath.Dir(buildGo), e.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", e.Name(), err)
		}
		if strings.Contains(string(b), "type ContentRow struct") {
			t.Errorf("%s redefines type ContentRow; imagebuild must alias verify.ContentRow", e.Name())
		}
	}
}
