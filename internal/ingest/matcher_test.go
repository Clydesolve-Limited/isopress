package ingest

// T1.3 matcher tests: table-driven coverage of EVERY semantic row of
// DESIGN_SPEC 3.2 (D-08), plus the documented judgment calls (no
// negation, case sensitivity, literal brackets/backslashes, trimming,
// empty-pattern rejection) and the runs.exclude_json snapshot contract
// (Patterns -> JSON round-trip, consumed by T1.6).
//
// Stdlib only; every fixture lives in t.TempDir().

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"isopress/internal/report"
)

// matchKind selects which matcher method a table row exercises.
type matchKind int

const (
	matchFile matchKind = iota
	matchDir
)

func (k matchKind) String() string {
	if k == matchDir {
		return "MatchDir"
	}
	return "MatchFile"
}

// TestMatcherSemantics is the DESIGN_SPEC 3.2 semantic table. Each row
// pins one documented behaviour; "want" is the expected result of the
// named method for the given relpath.
func TestMatcherSemantics(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		kind     matchKind
		relpath  string
		want     bool
	}{
		// (a) Pattern WITHOUT '/': basename match at any depth.
		{"star-iso matches top-level", []string{"*.iso"}, matchFile, "tool.iso", true},
		{"star-iso matches at depth a/b/c", []string{"*.iso"}, matchFile, "a/b/c.iso", true},
		{"star-iso matches a directory name too", []string{"*.iso"}, matchDir, "a/images.iso", true},
		{"star-iso does not match .iso.gz suffix", []string{"*.iso"}, matchFile, "a/b.iso.gz", false},
		{"star-iso does not match iso without dot", []string{"*.iso"}, matchFile, "a/xiso", false},
		{"plain basename matches at depth", []string{"tmp"}, matchFile, "a/tmp", true},
		{"plain basename matches top-level", []string{"tmp"}, matchDir, "tmp", true},
		{"plain basename does not match longer component", []string{"tmp"}, matchFile, "a/tmp.old/x", false},

		// (b) Pattern WITH '/': anchored to the scan root.
		{"anchored prefix matches child", []string{"var/log/**"}, matchFile, "var/log/app.log", true},
		{"anchored prefix matches nested child", []string{"var/log/**"}, matchFile, "var/log/a/b/c.log", true},
		{"anchored does not match sibling prefix var/logx", []string{"var/log/**"}, matchFile, "var/logx/app.log", false},
		{"anchored does not match at deeper root", []string{"var/log/**"}, matchFile, "x/var/log/app.log", false},
		{"anchored ** does not match the anchor dir itself", []string{"var/log/**"}, matchDir, "var/log", false},

		// (b) '*' does NOT cross '/'.
		{"single star matches within src", []string{"src/*.tmp"}, matchFile, "src/a.tmp", true},
		{"single star does not cross one segment", []string{"src/*.tmp"}, matchFile, "src/gen/a.tmp", false},
		{"single star does not cross several segments", []string{"src/*.tmp"}, matchFile, "src/a/b/c.tmp", false},

		// (b) '**' DOES cross '/'.
		{"doublestar crosses segments", []string{"var/log/**"}, matchDir, "var/log/x", true},
		{"mid doublestar matches zero segments (a/b)", []string{"a/**/b"}, matchFile, "a/b", true},
		{"mid doublestar matches one segment (a/x/b)", []string{"a/**/b"}, matchFile, "a/x/b", true},
		{"mid doublestar matches several segments", []string{"a/**/b"}, matchFile, "a/x/y/z/b", true},
		{"mid doublestar requires segment boundary", []string{"a/**/b"}, matchFile, "a/bb", false},
		{"lone doublestar matches everything", []string{"**"}, matchFile, "anything/at/all", true},
		{"lone doublestar matches a bare name", []string{"**"}, matchDir, "anything", true},

		// '**' also crosses '\n' (F1): RE2 '.' excludes newline
		// by default, so the translation scopes the (?s:) flag to
		// the '**' arms; without it 'var/**' would silently fail
		// to exclude a filename containing a newline (an
		// over-inclusion / privacy-class edge).
		{"doublestar crosses newline in a segment", []string{"var/**"}, matchFile, "var/a\nb", true},
		{"mid doublestar crosses newline", []string{"a/**/b"}, matchFile, "a/x\ny/b", true},

		// Basename patterns were ALREADY newline-safe: '[^/]' and
		// '[^/]*' match '\n', so no (?s:) flag is needed outside the
		// '**' arms (pinned so the flag scope stays minimal).
		{"basename glob matches filename containing newline", []string{"*.log"}, matchFile, "var/a\nb.log", true},

		// (c) Trailing '/': directories ONLY (subtree pruning is the
		// walker's job; the anchored row shows the boundary behaviour).
		{"trailing slash matches top-level dir", []string{"cache/"}, matchDir, "cache", true},
		{"trailing slash matches dir at depth", []string{"cache/"}, matchDir, "a/cache", true},
		{"trailing slash does NOT match a file named cache", []string{"cache/"}, matchFile, "cache", false},
		{"trailing slash does NOT match file at depth", []string{"cache/"}, matchFile, "a/cache", false},
		{"trailing slash requires component boundary", []string{"cache/"}, matchDir, "a/cache.old", false},
		{"anchored trailing slash matches its dir", []string{"var/cache/"}, matchDir, "var/cache", true},
		{"anchored trailing slash is exact (children pruned by walker)", []string{"var/cache/"}, matchDir, "var/cache/x", false},

		// (d) No negation in V1: '!' is a literal character.
		{"bang pattern matches literally", []string{"!foo"}, matchFile, "!foo", true},
		{"bang pattern does not re-include anything", []string{"!foo"}, matchFile, "foo", false},
		{"plain pattern ignores bang names", []string{"foo"}, matchFile, "!foo", false},

		// Case sensitivity (documented: patterns are case-sensitive,
		// matching gitignore-like tools on a case-sensitive filesystem).
		{"uppercase iso pattern misses lowercase file", []string{"*.ISO"}, matchFile, "a.iso", false},
		{"uppercase iso pattern matches uppercase file", []string{"*.ISO"}, matchFile, "a.ISO", true},
		{"case-sensitive basename", []string{"README"}, matchFile, "readme", false},

		// '?' matches exactly one non-separator character.
		{"question matches one char", []string{"a?c"}, matchFile, "abc", true},
		{"question requires exactly one char", []string{"a?c"}, matchFile, "ac", false},
		{"question does not cross /", []string{"a?c"}, matchFile, "a/c", false},

		// Leading '/' anchor notation (gitignore-compat).
		{"leading slash anchored match", []string{"/etc/backups"}, matchFile, "etc/backups", true},
		{"leading slash stays anchored", []string{"/etc/backups"}, matchFile, "srv/etc/backups", false},

		// Literal special characters: '[', ']' and '\' are NOT glob
		// syntax in V1 (documented in matcher.go).
		{"brackets are literal", []string{"foo[1]"}, matchFile, "foo[1]", true},
		{"brackets do not form a character class", []string{"foo[1]"}, matchFile, "foo1", false},
		{"backslash is literal", []string{`a\b`}, matchFile, `a\b`, true},
		{"backslash does not escape", []string{`a\b`}, matchFile, "ab", false},
		{"dot is literal", []string{"a.b"}, matchFile, "axb", false},
		{"dot matches dot", []string{"a.b"}, matchFile, "a.b", true},

		// Any-match-wins union semantics across multiple patterns.
		{"union of patterns", []string{"*.iso", "cache/"}, matchFile, "x/y.iso", true},
		{"union of patterns dir row", []string{"*.iso", "cache/"}, matchDir, "x/cache", true},
		{"union of patterns negative row", []string{"*.iso", "cache/"}, matchFile, "x/y.img", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := NewMatcher(tc.patterns, "")
			if err != nil {
				t.Fatalf("NewMatcher(%v): %v", tc.patterns, err)
			}
			var got bool
			if tc.kind == matchDir {
				got = m.MatchDir(tc.relpath)
			} else {
				got = m.MatchFile(tc.relpath)
			}
			if got != tc.want {
				t.Errorf("%s(%q) with patterns %v = %v, want %v",
					tc.kind, tc.relpath, tc.patterns, got, tc.want)
			}
		})
	}
}

// TestMatcherEmptyPatternListMatchesNothing: with no exclusion
// arguments the matcher excludes nothing (DESIGN_SPEC 3.2 "Defaults:
// none shipped as implicit magic").
func TestMatcherEmptyPatternListMatchesNothing(t *testing.T) {
	m, err := NewMatcher(nil, "")
	if err != nil {
		t.Fatalf("NewMatcher(nil, \"\"): %v", err)
	}
	paths := []string{"x", "a/b.iso", "var/log", "proc/kcore"}
	for _, p := range paths {
		if m.MatchFile(p) {
			t.Errorf("empty matcher MatchFile(%q) = true, want false", p)
		}
		if m.MatchDir(p) {
			t.Errorf("empty matcher MatchDir(%q) = true, want false", p)
		}
	}
	if got := m.Patterns(); len(got) != 0 {
		t.Errorf("empty matcher Patterns() = %v, want empty", got)
	}

	// The zero value behaves identically.
	var zero Matcher
	if zero.MatchFile("anything") || zero.MatchDir("anything") {
		t.Error("zero-value Matcher must match nothing")
	}
}

// TestMatcherZeroLengthPatternRejected: an explicit empty or
// all-whitespace --exclude pattern is a usage-class error (the flag
// spelling is a mistake; blank LINES inside --exclude-from files are
// ignored per that file format's rule, which the file-parsing test
// covers separately).
func TestMatcherZeroLengthPatternRejected(t *testing.T) {
	bad := []string{"", "   ", "\t", "/"}
	for _, p := range bad {
		_, err := NewMatcher([]string{p}, "")
		if err == nil {
			t.Errorf("NewMatcher(%q) succeeded, want usage error", p)
		}
		var usage *report.UsageError
		if !errors.As(err, &usage) {
			t.Errorf("NewMatcher(%q) error %v is not a report.UsageError", p, err)
		}
		if got := report.Classify(err); got != report.ExitUsage {
			t.Errorf("NewMatcher(%q) classifies to %d, want %d (ExitUsage)", p, got, report.ExitUsage)
		}
	}
	// A valid pattern after a bad one still fails the whole build.
	_, err := NewMatcher([]string{"*.iso", ""}, "")
	if err == nil {
		t.Error("NewMatcher with a trailing empty pattern succeeded, want usage error")
	}
}

// TestMatcherExcludeFromParsing covers the --exclude-from file format of
// DESIGN_SPEC 3.2: '#' comment lines, blank and whitespace-only lines
// ignored, surrounding whitespace trimmed, effective order preserved
// (--exclude values first, then file lines).
func TestMatcherExcludeFromParsing(t *testing.T) {
	path := writeExcludeFile(t,
		"# leading comment\n"+
			"\n"+
			"   \t \n"+
			"  *.tmp  \n"+
			"# another comment\n"+
			"var/log/\n"+
			"*.bak")
	m, err := NewMatcher([]string{"*.iso"}, path)
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}

	wantPatterns := []string{"*.iso", "*.tmp", "var/log/", "*.bak"}
	if got := m.Patterns(); !reflect.DeepEqual(got, wantPatterns) {
		t.Errorf("Patterns() = %v, want %v (order preserved, lines trimmed)", got, wantPatterns)
	}

	// The parsed patterns are live, not just snapshot data.
	if !m.MatchFile("a/b.tmp") {
		t.Error("MatchFile(\"a/b.tmp\") = false, want true from --exclude-from '*.tmp'")
	}
	if !m.MatchFile("x/y.iso") {
		t.Error("MatchFile(\"x/y.iso\") = false, want true from --exclude '*.iso'")
	}
	if !m.MatchDir("var/log") {
		t.Error("MatchDir(\"var/log\") = false, want true from --exclude-from 'var/log/'")
	}
	if m.MatchFile("var/log") {
		t.Error("MatchFile(\"var/log\") = true, want false ('var/log/' is dir-only)")
	}
}

// TestMatcherExcludeFromMissingFile: a missing or unreadable
// --exclude-from file is a usage-class error (DESIGN_SPEC 3.2 input;
// the cli already validates existence at the flag layer, this pins the
// matcher's own contract for direct callers such as T1.6).
func TestMatcherExcludeFromMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-excludes.txt")
	_, err := NewMatcher(nil, missing)
	if err == nil {
		t.Fatal("NewMatcher with missing --exclude-from file succeeded, want usage error")
	}
	var usage *report.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("error %v is not a report.UsageError", err)
	}
	if got := report.Classify(err); got != report.ExitUsage {
		t.Errorf("classification = %d, want %d (ExitUsage)", got, report.ExitUsage)
	}
	// The message names the file so the user can fix the path.
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error %q does not name the offending file %q", err, missing)
	}
}

// TestMatcherExcludeFromDirectoryIsError: os.ReadFile on a directory
// fails (EISDIR); that failure must surface as a usage error, not an
// empty pattern set silently scanning everything.
func TestMatcherExcludeFromDirectoryIsError(t *testing.T) {
	dir := t.TempDir() // a directory, not a pattern file
	_, err := NewMatcher(nil, dir)
	if err == nil {
		t.Fatal("NewMatcher with directory as --exclude-from succeeded, want usage error")
	}
	var usage *report.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("error %v is not a report.UsageError", err)
	}
}

// TestMatcherPatternsSnapshotSerialisable: Patterns() is the data
// T1.6 marshals into runs.exclude_json (DESIGN_SPEC 3.2). Assert the
// JSON round-trip and that the returned slice is a defensive copy.
func TestMatcherPatternsSnapshotSerialisable(t *testing.T) {
	path := writeExcludeFile(t, "proc/\n# comment\nsys/\n")
	m, err := NewMatcher([]string{"*.iso", "var/log/**"}, path)
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}

	want := []string{"*.iso", "var/log/**", "proc/", "sys/"}
	if got := m.Patterns(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Patterns() = %v, want %v", got, want)
	}

	// JSON round-trip exactly as runs.exclude_json will store it.
	data, err := json.Marshal(m.Patterns())
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var round []string
	if err := json.Unmarshal(data, &round); err != nil {
		t.Fatalf("json.Unmarshal(%s): %v", data, err)
	}
	if !reflect.DeepEqual(round, want) {
		t.Errorf("JSON round-trip = %v, want %v", round, want)
	}

	// The snapshot is a copy: mutating it must not affect the matcher.
	got := m.Patterns()
	got[0] = "MUTATED"
	if m.Patterns()[0] != "*.iso" {
		t.Error("mutating the Patterns() slice changed the matcher's effective set")
	}
}

// writeExcludeFile writes patterns to a temp file and returns its path
// (same helper pattern the cli tests use).
func writeExcludeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "excludes.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing exclude file: %v", err)
	}
	return path
}
