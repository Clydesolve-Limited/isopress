package cli

// T6.1 acceptance test for the DESIGN_SPEC 10.3 / PLAN_SPEC FR-046
// stream-separation constraint, in its grep-assertion form: NO file in
// the service layer or the catalog layer writes directly to stdout or
// stderr. Only the CLI layer (this package), the progress renderers
// (internal/progress - the designated stderr renderer, which writes
// exclusively through its INJECTED writer and has its own os.Stdout
// assertion in progress/renderer_test.go), and the final report writer
// (this package) touch the process streams.
//
// The scan is AST-based (the deps_test.go go/parser precedent) so a
// COMMENT mentioning os.Stderr never trips it; imports are RESOLVED
// (an aliased `import f "fmt"` is caught, a local variable named "log"
// is not), and the forbidden shapes are exactly the direct ones:
//
//   - os.Stdout / os.Stderr field references (under any import alias);
//   - fmt.Print / fmt.Printf / fmt.Println (no writer parameter - they
//     write to os.Stdout unconditionally); fmt.Fprint* variants take an
//     injected writer and are FINE;
//   - the log package (its default output is os.Stderr), and a
//     dot-import of os/fmt/log outright;
//   - the builtin print / println (os.Stderr unconditionally).
//
// A NEW service or catalog file that needs a stream is a layering
// violation by construction (DESIGN_SPEC 1.2: services emit events on
// the progress bus; the catalog logs through the bus as well) and must
// fail here FIRST, before review.
//
// MUTATION-CHECK (T6.1 adversarial sweep): a detector that never fires
// is worse than none at all, so TestStreamSeparationDetectorFlags-
// SyntheticViolations feeds the EXACT detector synthetic source files
// carrying every forbidden shape (and their legal counterparts) and
// asserts each violation is flagged and each legal shape is not - the
// detector's own true-positive/true-negative contract.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
)

// streamExcludedPackages is the DOCUMENTED exclusion table of the
// internal/ sweep (T6.5 replaced the hardcoded package list with a
// directory walk, so a NEW internal package is covered without an
// edit here). Every internal/ package directory that holds .go files
// and is not named below is swept for direct stream writes. Reasons -
// each exclusion is design-sanctioned, none is convenience:
//
//   - cli: the layer UNDER test. DESIGN_SPEC 1.2 makes the CLI layer
//     the one that prints results to stdout, so it is stream-touching
//     BY DESIGN and sweeping it would assert nothing;
//   - progress: IS the renderer layer (1.2: "only the renderer and
//     the final report writer" write to the streams). It writes
//     through its INJECTED writer only, and its own os.Stdout
//     absence is pinned in progress/renderer_test.go;
//   - report: DESIGN_SPEC 1.2 exempts "the final report writer" from
//     the never-write rule and internal/report's package doc claims
//     that designation; it writes nothing today, but the design places
//     it outside the stream-free contract, so the sweep does not own
//     it.
//
// The swept set is DERIVED (T6.5): streamFreePackagesBaseline below is
// only the migration pin, never the source of the list.
var streamExcludedPackages = map[string]string{
	"cli":      "the layer under test: DESIGN_SPEC 1.2 makes it the stdout-owning layer",
	"progress": "the renderer layer itself (writes through its injected writer; own os.Stdout pin)",
	"report":   "the final report writer DESIGN_SPEC 1.2 allows to touch stdout",
}

// streamFreePackagesBaseline is the pre-walk hardcoded list, kept as a
// MIGRATION PIN: the derivation must still sweep every one of these
// packages (a drop = lost coverage = failure), while a package that
// merely JOINS the sweep (internal/ grew) needs no edit - it is
// checked below by construction.
var streamFreePackagesBaseline = []string{
	"catalog", "ingest", "plan", "copyengine", "imagebuild",
	"verify", "restore", "media", "fsmeta", "xorriso", "units",
}

// deriveStreamFreePackages walks internal/ and returns, in ascending
// order, every package directory containing at least one .go file minus
// streamExcludedPackages - the T6.5 replacement for the hardcoded list.
// It fails the test when internal/ cannot be read (a broken walk must
// never silently sweep nothing), and a swept package with no NON-TEST
// sources keeps the pre-walk ParseDir-era failure ("no Go sources
// found in ...") so the two eras fail the same way.
func deriveStreamFreePackages(t *testing.T) []string {
	t.Helper()
	root := filepath.Join(moduleRoot, "internal")
	dirs, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading the internal/ package root: %v", err)
	}
	var out []string
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(root, d.Name()))
		if err != nil {
			t.Fatalf("reading internal/%s: %v", d.Name(), err)
		}
		hasGo := false
		for _, f := range files {
			if !f.IsDir() && strings.HasSuffix(f.Name(), ".go") {
				hasGo = true
				break
			}
		}
		if !hasGo {
			continue // no Go sources at all: not a package directory
		}
		if _, excluded := streamExcludedPackages[d.Name()]; excluded {
			continue
		}
		out = append(out, d.Name())
	}
	sort.Strings(out)
	return out
}

// streamViolations returns one description per forbidden direct-stream
// shape in the file (see the file doc for the exact list). It is the
// ONE detector both the package sweep and the synthetic self-test
// share, so the self-test proves the real detector's behavior.
//
// T6.1 mutation-check refinement: the detector resolves each file's
// IMPORTS instead of pattern-matching the identifier spelling, so an
// aliased import (`import f "fmt"`) cannot evade it and a local
// variable merely named "log" is not a false positive. A dot-import
// of a stream package is flagged outright (a service file dot-
// importing fmt/log/os is a layering smell the sweep refuses to
// disambiguate).
func streamViolations(fset *token.FileSet, file *ast.File) []string {
	// Resolve os/fmt/log to the identifier spellings used in this file.
	alias := map[string]string{} // ident -> package path
	for _, imp := range file.Imports {
		pkg := strings.Trim(imp.Path.Value, `"`)
		if pkg != "os" && pkg != "fmt" && pkg != "log" {
			continue
		}
		name := ""
		if imp.Name != nil {
			name = imp.Name.Name
			if name == "." {
				pos := fset.Position(imp.Pos()).String()
				alias["\x00dot:"+pkg] = pos // sentinel: flagged below
				continue
			}
			if name == "_" {
				continue
			}
		} else {
			base := filepath.Base(pkg)
			name = base[:len(base)-len(filepath.Ext(base))]
		}
		alias[name] = pkg
	}

	var found []string
	for key, pos := range alias {
		if strings.HasPrefix(key, "\x00dot:") {
			found = append(found, pos+": dot-import of "+strings.TrimPrefix(key, "\x00dot:")+
				" is a stream-layering smell; import it named or use the progress bus")
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.SelectorExpr:
			base, ok := node.X.(*ast.Ident)
			if !ok {
				return true
			}
			switch alias[base.Name] {
			case "os":
				if node.Sel.Name == "Stdout" || node.Sel.Name == "Stderr" {
					found = append(found, fset.Position(node.Pos()).String()+
						": "+base.Name+"."+node.Sel.Name+" is a direct stream reference")
				}
			case "fmt":
				if strings.HasPrefix(node.Sel.Name, "Print") { // Print/Printf/Println only
					found = append(found, fset.Position(node.Pos()).String()+
						": fmt."+node.Sel.Name+" (imported as "+base.Name+") writes to os.Stdout unconditionally")
				}
			case "log":
				found = append(found, fset.Position(node.Pos()).String()+
					": the log package (imported as "+base.Name+") writes to os.Stderr by default")
			}
		case *ast.CallExpr:
			if id, ok := node.Fun.(*ast.Ident); ok && (id.Name == "print" || id.Name == "println") {
				found = append(found, fset.Position(node.Pos()).String()+
					": the builtin "+id.Name+"() writes to os.Stderr")
			}
		}
		return true
	})
	return found
}

// TestNoDirectStreamWritesBelowTheCLILayer sweeps every non-test
// source file of the derived service and catalog package set for
// direct stdout/stderr writes (see the file doc for the exact
// forbidden shapes and streamExcludedPackages for the documented
// exclusions).
func TestNoDirectStreamWritesBelowTheCLILayer(t *testing.T) {
	fset := token.NewFileSet()
	pkgs := deriveStreamFreePackages(t)

	// MIGRATION PIN (T6.5: hardcoded list -> directory walk): every
	// package the pre-walk list swept is still swept - a drop is a
	// coverage regression and fails here. A package that merely JOINS
	// the sweep (internal/ grew) needs NO edit: it is checked below by
	// construction, which is the point of the walk; the only sanctioned
	// way to keep a package out is an entry in streamExcludedPackages
	// with a design reason.
	for _, want := range streamFreePackagesBaseline {
		if !slices.Contains(pkgs, want) {
			t.Errorf("internal/%s dropped out of the stream-free sweep (derived set: %v); only a design-sanctioned entry in streamExcludedPackages may exclude a package, and it must keep this baseline",
				want, pkgs)
		}
	}

	for _, pkg := range pkgs {
		dir := filepath.Join(moduleRoot, "internal", pkg)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading package %s: %v", pkg, err)
		}
		parsed := 0
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
			if err != nil {
				t.Fatalf("parsing %s: %v", filepath.Join(dir, name), err)
			}
			parsed++
			for _, v := range streamViolations(fset, file) {
				t.Errorf("%s: %s (services emit progress events, they never write to a process stream, FR-046)", pkg, v)
			}
		}
		if parsed == 0 {
			t.Fatalf("no Go sources found in %s", dir)
		}
	}
}

// TestStreamSeparationDetectorFlagsSyntheticViolations is the
// detector's own contract (the mutation check, pinned): each FORBIDDEN
// shape must be flagged, each LEGAL counterpart must not be - so a
// future refactor that silently weakens the detector fails HERE, not
// in production review.
func TestStreamSeparationDetectorFlagsSyntheticViolations(t *testing.T) {
	fset := token.NewFileSet()
	parse := func(t *testing.T, name, src string) *ast.File {
		t.Helper()
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parsing synthetic %s: %v", name, err)
		}
		return f
	}

	// Every forbidden shape, one per synthetic file: each MUST be
	// caught (true positives).
	forbidden := []struct{ name, src, want string }{
		{"os_stdout.go", "package p\nimport \"os\"\nfunc W() { os.Stdout.WriteString(\"x\") }", "os.Stdout"},
		{"os_stderr.go", "package p\nimport \"os\"\nfunc W() { os.Stderr.WriteString(\"x\") }", "os.Stderr"},
		{"fmt_println.go", "package p\nimport \"fmt\"\nfunc W() { fmt.Println(\"x\") }", "fmt.Println"},
		{"fmt_printf.go", "package p\nimport \"fmt\"\nfunc W() { fmt.Printf(\"%d\", 1) }", "fmt.Printf"},
		{"fmt_print.go", "package p\nimport \"fmt\"\nfunc W() { fmt.Print(\"x\") }", "fmt.Print "},
		{"log_default.go", "package p\nimport \"log\"\nfunc W() { log.Print(\"x\") }", "log package"},
		{"builtin_print.go", "package p\nfunc W() { print(\"x\") }", "builtin print"},
		{"builtin_println.go", "package p\nfunc W() { println(\"x\") }", "builtin println"},
		{"aliased_fmt.go", "package p\nimport f \"fmt\"\nfunc W() { f.Println(\"x\") }", "fmt.Println"},
		{"aliased_log.go", "package p\nimport lg \"log\"\nfunc W() { lg.Print(\"x\") }", "log package"},
		// A dot-import of a stream package is refused outright (see
		// the detector doc): the sweep will not disambiguate bare
		// Println calls in a service file.
		{"dot_import_fmt.go", "package p\nimport . \"fmt\"\nfunc W() { Println(\"x\") }", "dot-import of fmt"},
	}
	for _, tc := range forbidden {
		t.Run("flags/"+tc.name, func(t *testing.T) {
			got := streamViolations(fset, parse(t, tc.name, tc.src))
			if len(got) == 0 {
				t.Fatalf("detector MISSED the forbidden shape in %s (src: %s)", tc.name, tc.src)
			}
			joined := strings.Join(got, "\n")
			if !strings.Contains(joined, tc.want) {
				t.Errorf("detector fired but not on %q:\n%s", tc.want, joined)
			}
		})
	}

	// The legal counterparts (true negatives): injected writers, the
	// fmt.Fprint family, aliased os with non-stream fields, comments
	// naming the forbidden shapes, and a local variable called
	// "log" - none may be flagged.
	clean := []struct{ name, src string }{
		{"fmt_fprintln.go", "package p\nimport (\n\"fmt\"\n\"io\"\n)\nfunc W(w io.Writer) { fmt.Fprintln(w, \"x\") }"},
		{"fmt_fprintf.go", "package p\nimport \"fmt\"\nfunc W(w io.Writer) { fmt.Fprintf(w, \"%d\", 1) }"},
		{"os_nonstream.go", "package p\nimport \"os\"\nfunc W() { _, _ = os.Open(\"x\"), os.Getpid() }"},
		{"comment_only.go", "package p\n// os.Stderr fmt.Println print(1234) log.Fatal must never appear here"},
		{"local_log_var.go", "package p\nfunc W() { log := struct{ X int }{}; _ = log.X }"},
		{"blank_import.go", "package p\nimport _ \"os\"\nfunc W() {}"},
		{"method_named_print.go", "package p\ntype T struct{}\nfunc (T) Println(a ...any) {}\nfunc W(t T) { t.Println(\"fine\") }"},
	}
	for _, tc := range clean {
		t.Run("clean/"+tc.name, func(t *testing.T) {
			if got := streamViolations(fset, parse(t, tc.name, tc.src)); len(got) != 0 {
				t.Errorf("detector flagged a LEGAL shape in %s:\n%s\n(src: %s)", tc.name, strings.Join(got, "\n"), tc.src)
			}
		})
	}
}
