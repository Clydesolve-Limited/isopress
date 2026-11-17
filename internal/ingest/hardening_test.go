package ingest

// T1.3 hardening tests: adversarial-pattern and edge-case coverage that
// complements matcher_test.go (DESIGN_SPEC 3.2 semantic table) and
// walker_test.go (walk integration). Everything here pins behaviour that
// the base suite leaves uncovered:
//
//   - '**' vs directory-boundary subtleties: '**/foo' matches a
//     top-level 'foo'; 'a/**' does NOT match 'a' itself (documented in
//     matcher.go); repeated/deeply-nested '**' collapses, never panics.
//   - Adversarial patterns ('a**b', '***', many stars, regexp
//     metacharacters) stay CORRECT and FAST. Go's RE2 engine is
//     linear-time, so there is no catastrophic backtracking to fear,
//     but the translation is in-tree and must be proven panic-free and
//     bounded anyway (the wall-clock bound is a generous tripwire, not
//     a benchmark).
//   - Unicode names (DESIGN_SPEC 12.2 spirit): multi-byte runes in
//     patterns ('?', literals, '*') and in walked trees, plus a
//     raw-byte (invalid UTF-8) filename handled without panic.
//   - Exclude-from files with CRLF line endings (real-world files from
//     other systems) parse identically to LF files.
//   - Walker: the root-refusal rail keys off the EFFECTIVE pattern set,
//     so an --exclude-from file of only comments still refuses "/".
//   - Walker: symlink loops are never followed (termination asserted
//     via a watchdog); 100-level nesting walks cleanly with exact
//     counts; a trailing-slash pattern does NOT treat a
//     symlink-to-directory as a directory (lstat semantics: the walker
//     classifies by DirEntry type, and a dir-only pattern only ever
//     reaches MatchDir for real directories).
//   - Walker: job emission order is deterministic (WalkDir is lexical),
//     so two walks of one tree yield byte-identical job sequences.
//   - Walker: a regular-file scan root is rejected with the
//     not-a-directory message (classification consistent with T1.2:
//     the root shape check fires before entry classification).
//
// Stdlib only; every fixture lives in t.TempDir().

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"isopress/internal/report"
)

// TestMatcherDoubleStarBoundarySemantics pins the '**' versus
// directory-boundary contract of the in-tree translation:
//
//	'**/' -> '(?:.*/)?'  (zero or more whole segments)
//	'**'  -> '.*'        (crosses '/')
//
// so '**/foo' matches a TOP-LEVEL 'foo' (zero segments), while
// 'a/**' matches everything strictly BELOW 'a' but never 'a' itself
// (the prune-the-dir-itself behaviour comes from MatchDir on the
// anchored form, walker_test.go covers the prune).
func TestMatcherDoubleStarBoundarySemantics(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		kind     matchKind
		relpath  string
		want     bool
	}{
		{"**/foo matches a top-level file", []string{"**/foo"}, matchFile, "foo", true},
		{"**/foo matches a top-level dir", []string{"**/foo"}, matchDir, "foo", true},
		{"**/foo matches a deeply nested foo", []string{"**/foo"}, matchFile, "a/b/foo", true},
		{"**/foo never matches foo's children", []string{"**/foo"}, matchFile, "a/foo/bar", false},
		{"**/foo does not match the foobar name", []string{"**/foo"}, matchFile, "a/foobar", false},

		{"a/** does not match the anchor dir itself", []string{"a/**"}, matchDir, "a", false},
		{"a/** does not match the anchor as a file", []string{"a/**"}, matchFile, "a", false},
		{"a/** matches a direct child", []string{"a/**"}, matchFile, "a/b", true},
		{"a/** matches a nested child", []string{"a/**"}, matchFile, "a/b/c/d", true},
		{"a/** does not match a sibling prefix", []string{"a/**"}, matchFile, "ab/c", false},

		// Repeated '**' collapses: every '(?:.*/)?' group can match
		// zero segments, so 'a/**/**/b' still matches 'a/b'.
		{"a/**/**/b matches zero segments", []string{"a/**/**/b"}, matchFile, "a/b", true},
		{"a/**/**/b matches several segments", []string{"a/**/**/b"}, matchFile, "a/x/y/z/b", true},
		{"a/**/**/**/**/b (deep) matches a/b", []string{"a/**/**/**/**/b"}, matchFile, "a/b", true},
		{"a/**/**/**/**/b (deep) matches deep", []string{"a/**/**/**/**/b"}, matchFile, "a/1/2/3/4/b", true},
		{"a/**/**/**/**/b (deep) stays anchored", []string{"a/**/**/**/**/b"}, matchFile, "x/a/1/b", false},

		// A trailing '**/' is dir-only and matches every non-empty
		// relpath as a directory, never as a file.
		{"**/ dir-only matches a nested dir", []string{"**/"}, matchDir, "x/y", true},
		{"**/ dir-only matches a top dir", []string{"**/"}, matchDir, "x", true},
		{"**/ dir-only never matches a file", []string{"**/"}, matchFile, "x/y", false},
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

// TestMatcherAdversarialPatternsCorrectAndFast throws regex-injection and
// pathological-glob shapes at the in-tree translation. Two obligations:
// (1) no panic, correct pinned results; (2) the whole battery completes
// fast. Go's regexp is RE2 (linear-time, no backtracking), so the time
// bound is deliberately generous (a tripwire against an accidental
// engine change or an O(pattern^2) translation, not a benchmark).
func TestMatcherAdversarialPatternsCorrectAndFast(t *testing.T) {
	cases := []struct {
		pattern string
		relpath string
		want    bool
	}{
		// '**' glued between literals inside a basename pattern: still
		// crosses '/' (documented: '**' crosses wherever it appears).
		{"a**b", "ax/b", true},
		{"a**b", "aXb", true},
		{"a**b", "dir/ab", true},
		{"a**b", "a/b", true},
		{"a**b", "xaXb", false}, // no component boundary before the 'a'
		{"a**b", "a/bb", true},  // '**' crosses '/', so only the final 'b' must anchor the end
		{"**a", "xa", true},
		{"**a", "x/a", true},
		{"**a", "a/b", false}, // does not end in 'a'
		{"***", "anything/at/all", true},

		// Fixed segment counts and mixed stars.
		{"*/*/*/*", "a/b/c/d", true},
		{"*/*/*/*", "a/b/c", false},
		{"**/**/**", "a", true},
		{"**/**/**", "a/b/c/d", true},
		{"?*?*?*?*?", "abcde", true},
		{"?*?*?*?*?", "abcdefg", true},
		{"?*?*?*?*?", "abcd", false},

		// Regexp metacharacters in patterns are LITERAL: the
		// translation QuoteMeta's every non-glob rune, so no pattern
		// can inject regex syntax.
		{"a.*b", "a.*b", true},
		{"a.*b", "axxb", false},
		{"a+b", "a+b", true},
		{"a+b", "aab", false},
		{"a{2,3}", "a{2,3}", true},
		{"a{2,3}", "aaa", false},
		{"a(b", "a(b", true},
		{"a)b", "a)b", true},
		{"[a-", "[a-", true},
		{"[a-", "a", false},
		{`a\b`, `a\b`, true},
		{"a|b", "a|b", true},
		{"a|b", "a", false},
		{"^a$", "^a$", true},
	}

	// Correctness first, then the same battery hammered for speed.
	check := func(iter int) {
		for i := 0; i < iter; i++ {
			for _, tc := range cases {
				m, err := NewMatcher([]string{tc.pattern}, "")
				if err != nil {
					t.Fatalf("NewMatcher(%q) on iteration %d: %v", tc.pattern, i, err)
				}
				if got := m.MatchFile(tc.relpath); got != tc.want {
					t.Fatalf("iteration %d: MatchFile(%q) with pattern %q = %v, want %v",
						i, tc.relpath, tc.pattern, got, tc.want)
				}
				// Patterns of any shape must also survive the
				// directory path without panicking.
				m.MatchDir(tc.relpath)
			}
		}
	}

	check(1) // correctness pass (failure reports exact iteration-free diffs)

	start := time.Now()
	check(300) // ~7800 matches total
	elapsed := time.Since(start)
	if elapsed > 5*time.Second {
		t.Errorf("adversarial matcher battery took %v, want < 5s (RE2 is linear-time; this is a tripwire)", elapsed)
	}

	// Pathologically long patterns and paths: 30 nested '**' segments
	// and a 64-star pattern must compile, match correctly, and stay
	// fast (the regex translation is iterative, no recursion).
	deepPat := strings.Repeat("**/", 30) + "x"
	deepPath := strings.Repeat("a/", 50) + "x"
	m, err := NewMatcher([]string{deepPat}, "")
	if err != nil {
		t.Fatalf("NewMatcher(deep ** pattern): %v", err)
	}
	if !m.MatchFile(deepPath) {
		t.Errorf("30-fold '**/' pattern did not match a 50-segment path ending in x")
	}
	if m.MatchFile(deepPath + "y") {
		t.Errorf("30-fold '**/' pattern matched a path not ending in x")
	}

	m, err = NewMatcher([]string{strings.Repeat("*", 64)}, "")
	if err != nil {
		t.Fatalf("NewMatcher(64 stars): %v", err)
	}
	if !m.MatchFile("anything/at/all") {
		t.Errorf("64-star pattern did not match a plain relpath")
	}
	if !m.MatchDir("anything") {
		t.Errorf("64-star pattern did not match a plain dir relpath")
	}
}

// TestMatcherUnicodeNames: multi-byte UTF-8 in patterns and paths
// (DESIGN_SPEC 12.2 fixture spirit: "unicode and dot filenames").
// '?' must match exactly one RUNE (not one byte), '*'/'**' must treat
// multi-byte names as ordinary components, and matching stays
// case-sensitive for non-ASCII too.
func TestMatcherUnicodeNames(t *testing.T) {
	cases := []struct {
		name     string
		patterns []string
		kind     matchKind
		relpath  string
		want     bool
	}{
		{"basename glob over unicode dir", []string{"*.txt"}, matchFile, "ドキュメント/café.txt", true},
		{"literal unicode basename matches", []string{"café"}, matchDir, "ドキュメント/café", true},
		{"literal unicode basename is exact", []string{"café"}, matchFile, "ドキュメント/cafe", false},
		{"question matches one multi-byte rune", []string{"?afé"}, matchFile, "café", true},
		{"question requires exactly one rune", []string{"??afé"}, matchFile, "café", false},
		{"anchored unicode prefix", []string{"ドキュメント/**"}, matchFile, "ドキュメント/naïve/café.txt", true},
		{"anchored unicode prefix stays anchored", []string{"ドキュメント/**"}, matchFile, "x/ドキュメント/café.txt", false},
		{"star around unicode", []string{"*é*"}, matchFile, "résumé", true},
		{"star around unicode at depth", []string{"*é*"}, matchFile, "a/b/é.txt", true},
		{"emoji filename with anchored glob", []string{"emoji-*.bin"}, matchFile, "emoji-😀.bin", true},
		{"case-sensitive for non-ASCII", []string{"café"}, matchFile, "CAFÉ", false},
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

// TestMatcherExcludeFromCRLF: exclude-from files authored on other
// systems carry CRLF line endings. TrimSpace strips the '\r', so the
// parse must be byte-identical to the LF version, including a lone-'\r'
// line (blank after trimming) and a final line with no newline at all.
func TestMatcherExcludeFromCRLF(t *testing.T) {
	path := writeExcludeFile(t,
		"# comment\r\n"+
			"\r\n"+
			"\r\n"+
			"  *.tmp  \r\n"+
			"var/log/\r\n"+
			"*.bak\r\n"+
			"plain") // no trailing newline, no \r
	m, err := NewMatcher(nil, path)
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}

	wantPatterns := []string{"*.tmp", "var/log/", "*.bak", "plain"}
	if got := m.Patterns(); !reflect.DeepEqual(got, wantPatterns) {
		t.Errorf("Patterns() = %v, want %v (CRLF trimmed identically to LF)", got, wantPatterns)
	}
	if !m.MatchFile("a/b.tmp") {
		t.Error("MatchFile(\"a/b.tmp\") = false, want true (CRLF pattern not live)")
	}
	if !m.MatchDir("var/log") {
		t.Error("MatchDir(\"var/log\") = false, want true (CRLF dir-only pattern not live)")
	}
	if m.MatchFile("var/log") {
		t.Error("MatchFile(\"var/log\") = true, want false ('var/log/' is dir-only)")
	}
	if !m.MatchFile("plain") {
		t.Error("MatchFile(\"plain\") = false, want true (final line without newline lost)")
	}
}

// TestWalkRefusesRootWhenExcludeFromHasNoEffectivePatterns: the FR-005
// rail fires on the EFFECTIVE pattern set, not on the flag count. An
// --exclude-from file containing only comments and blanks compiles to
// zero patterns, so scanning "/" still refuses.
func TestWalkRefusesRootWhenExcludeFromHasNoEffectivePatterns(t *testing.T) {
	path := writeExcludeFile(t, "# only comments\n\n   \n# still nothing\n")
	m, err := NewMatcher(nil, path)
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}
	if got := m.Patterns(); len(got) != 0 {
		t.Fatalf("Patterns() = %v, want empty", got)
	}
	_, err = Walk("/", m, nil, nil)
	if err == nil {
		t.Fatal("Walk(\"/\") with comment-only --exclude-from succeeded, want usage error")
	}
	var usage *report.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("error %v is not a report.UsageError", err)
	}
	if got := report.Classify(err); got != report.ExitUsage {
		t.Errorf("classification = %d, want %d (ExitUsage -> exit 2)", got, report.ExitUsage)
	}
}

// TestWalkSymlinkLoopNeverFollowed: a symlink cycle (self -> . and
// sub/loop -> ..) must not be followed. WalkDir never follows symlinks,
// so the walk must terminate, emit one job, and capture the two links
// verbatim. Termination is asserted with a watchdog so a regression
// that starts following links fails loudly instead of hanging CI.
func TestWalkSymlinkLoopNeverFollowed(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "keep.txt"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".", filepath.Join(root, "self")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..", filepath.Join(root, "sub", "loop")); err != nil {
		t.Fatal(err)
	}

	type outcome struct {
		res WalkResult
		err error
		j   []Job
	}
	done := make(chan outcome, 1)
	go func() {
		var jobs []Job
		res, err := Walk(root, Matcher{}, collectJobs(&jobs), nil)
		done <- outcome{res: res, err: err, j: jobs}
	}()

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("Walk: %v", got.err)
		}
		if got.j == nil {
			got.j = []Job{}
		}
		if !reflect.DeepEqual(jobRelPaths(got.j), []string{"sub/keep.txt"}) {
			t.Errorf("job relpaths = %v, want [sub/keep.txt] (loop entries must not be followed)", jobRelPaths(got.j))
		}
		wantLinks := map[string]string{"self": ".", "sub/loop": ".."}
		gotLinks := map[string]string{}
		for _, s := range got.res.Symlinks {
			gotLinks[s.RelPath] = s.Target
		}
		if !reflect.DeepEqual(gotLinks, wantLinks) {
			t.Errorf("symlink entries = %v, want %v", gotLinks, wantLinks)
		}
		if got.res.Dirs != 1 {
			t.Errorf("Dirs = %d, want 1 (sub)", got.res.Dirs)
		}
		if len(got.res.Warnings) != 0 {
			t.Errorf("Warnings = %v, want none on a clean loop walk", got.res.Warnings)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("Walk did not terminate within 60s: symlink loop appears to be followed")
	}
}

// TestWalkDeepNesting: a 100-level-deep chain with a file at the bottom
// and an empty sibling dir at depth 97. WalkDir is iterative (no
// recursion depth limit); the counts, the deep job relpath, and the
// deep empty-dir relpath must all be exact.
func TestWalkDeepNesting(t *testing.T) {
	root := t.TempDir()

	var chain []string
	dir := root
	for i := 0; i < 100; i++ {
		name := fmt.Sprintf("d%02d", i)
		dir = filepath.Join(dir, name)
		chain = append(chain, name)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("mkdir level %d: %v", i, err)
		}
	}
	// File at the very bottom.
	if err := os.WriteFile(filepath.Join(dir, "leaf.txt"), []byte("leaf"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Empty dir at depth 97 (inside d96).
	emptyRel := filepath.Join(filepath.Join(chain[:97]...), "sib-empty")
	if err := os.Mkdir(filepath.Join(root, emptyRel), 0o755); err != nil {
		t.Fatal(err)
	}

	var jobs []Job
	res, err := Walk(root, Matcher{}, collectJobs(&jobs), nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if res.Dirs != 101 { // 100 chain dirs + the empty sibling
		t.Errorf("Dirs = %d, want 101", res.Dirs)
	}
	if res.Files != 1 {
		t.Errorf("Files = %d, want 1", res.Files)
	}
	leafRel := strings.Join(append(append([]string{}, chain...), "leaf.txt"), "/")
	if len(jobs) != 1 || jobs[0].RelPath != leafRel {
		t.Errorf("jobs = %v, want exactly [%s]", jobRelPaths(jobs), leafRel)
	}
	if len(jobs) == 1 && !strings.Contains(jobs[0].AbsPath, filepath.Join(chain...)) {
		t.Errorf("job AbsPath %q does not end with the deep chain", jobs[0].AbsPath)
	}
	wantEmpty := strings.ReplaceAll(emptyRel, string(filepath.Separator), "/")
	if !reflect.DeepEqual(emptyDirPaths(res), []string{wantEmpty}) {
		t.Errorf("EmptyDirs = %v, want [%s]", emptyDirPaths(res), wantEmpty)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("Warnings = %v, want none", res.Warnings)
	}
}

// TestWalkTrailingSlashAgainstSymlinkToDir pins the lstat semantics at
// the seam between matcher and walker: a trailing-slash pattern means
// "directories only", and a symlink-to-directory is a SYMLINK (the
// walker classifies via DirEntry type and never resolves links, D-03),
// so 'cachelink/' never matches the link while 'cache/' prunes the real
// directory. Both entries coexist without confusing the matcher.
func TestWalkTrailingSlashAgainstSymlinkToDir(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "cache", "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("cache", filepath.Join(root, "cachelink")); err != nil {
		t.Fatal(err)
	}

	t.Run("dir-only pattern on the link name does not match", func(t *testing.T) {
		m, err := NewMatcher([]string{"cachelink/"}, "")
		if err != nil {
			t.Fatalf("NewMatcher: %v", err)
		}
		var jobs []Job
		res, err := Walk(root, m, collectJobs(&jobs), nil)
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if res.ExcludedDirsPruned != 0 {
			t.Errorf("ExcludedDirsPruned = %d, want 0 (a symlink is not a directory for a dir-only pattern)", res.ExcludedDirsPruned)
		}
		if res.ExcludedFiles != 0 {
			t.Errorf("ExcludedFiles = %d, want 0 (dir-only patterns never apply to files or links)", res.ExcludedFiles)
		}
		if got := jobRelPaths(jobs); !reflect.DeepEqual(got, []string{"cache/x.txt"}) {
			t.Errorf("job relpaths = %v, want [cache/x.txt]", got)
		}
		if len(res.Symlinks) != 1 || res.Symlinks[0].RelPath != "cachelink" || res.Symlinks[0].Target != "cache" {
			t.Errorf("symlinks = %+v, want exactly [cachelink -> cache]", res.Symlinks)
		}
	})

	t.Run("dir-only pattern on the real dir prunes it; the link survives", func(t *testing.T) {
		m, err := NewMatcher([]string{"cache/"}, "")
		if err != nil {
			t.Fatalf("NewMatcher: %v", err)
		}
		var jobs []Job
		res, err := Walk(root, m, collectJobs(&jobs), nil)
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if res.ExcludedDirsPruned != 1 {
			t.Errorf("ExcludedDirsPruned = %d, want 1 (the real cache dir)", res.ExcludedDirsPruned)
		}
		if len(jobs) != 0 {
			t.Errorf("jobs = %v, want none (pruned subtree)", jobRelPaths(jobs))
		}
		if len(res.Symlinks) != 1 || res.Symlinks[0].RelPath != "cachelink" {
			t.Errorf("symlinks = %+v, want the cachelink entry (never followed, never pruned)", res.Symlinks)
		}
	})
}

// TestWalkJobOrderDeterministic: WalkDir visits entries in lexical
// order, so two walks of one unchanged tree must emit byte-identical
// job sequences (and identical symlink/empty-dir order). This pins the
// determinism T1.6's reconciler and the runs.exclude_json snapshot rely
// on for reproducible reports.
func TestWalkJobOrderDeterministic(t *testing.T) {
	root := buildWalkFixture(t) // nested dirs, symlinks, FIFO, unicode-free
	// Add a few more sibling names to stress lexical ordering (mixed
	// prefixes, dotfiles).
	for _, name := range []string{"docs/0-first.txt", "docs/.hidden", "docs/Z-upper.txt", "docs/a-lower.txt"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	walkOnce := func() ([]Job, WalkResult) {
		t.Helper()
		var jobs []Job
		res, err := Walk(root, Matcher{}, collectJobs(&jobs), nil)
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		return jobs, res
	}

	jobs1, res1 := walkOnce()
	jobs2, res2 := walkOnce()

	if len(jobs1) == 0 {
		t.Fatal("fixture produced no jobs; the determinism test is vacuous")
	}
	rel1 := make([]string, len(jobs1))
	for i, j := range jobs1 {
		rel1[i] = j.RelPath
	}
	rel2 := make([]string, len(jobs2))
	for i, j := range jobs2 {
		rel2[i] = j.RelPath
	}
	if !reflect.DeepEqual(rel1, rel2) {
		t.Errorf("job emission order differs between walks:\n1: %v\n2: %v", rel1, rel2)
	}
	// WalkDir is lexical: the emission order must be sorted.
	if !sortedStrings(rel1) {
		t.Errorf("job emission order is not lexical: %v", rel1)
	}
	// Symlinks and empty dirs are equally deterministic and ordered.
	l1, l2 := symlinkMap(res1.Symlinks), symlinkMap(res2.Symlinks)
	if !reflect.DeepEqual(l1, l2) {
		t.Errorf("symlink capture order/content differs between walks: %v vs %v", res1.Symlinks, res2.Symlinks)
	}
	if !reflect.DeepEqual(res1.EmptyDirs, res2.EmptyDirs) {
		t.Errorf("EmptyDirs differ between walks: %v vs %v", res1.EmptyDirs, res2.EmptyDirs)
	}
	if !sortedStrings(emptyDirPaths(res1)) {
		t.Errorf("EmptyDirs are not in lexical walk order: %v", emptyDirPaths(res1))
	}
}

func sortedStrings(s []string) bool {
	for i := 1; i < len(s); i++ {
		if s[i-1] >= s[i] {
			return false
		}
	}
	return true
}

func symlinkMap(links []SymlinkEntry) map[string]string {
	m := make(map[string]string, len(links))
	for _, s := range links {
		m[s.RelPath] = s.Target
	}
	return m
}

// TestWalkRegularFileRootMessage: a regular-file scan root is a
// usage-class error naming the problem. Classification stays consistent
// with T1.2: the walker's root-shape check fires BEFORE entry
// classification, so a regular file (fsmeta.KindRegular) can never be
// walked as a tree, and the message says exactly why.
func TestWalkRegularFileRootMessage(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-dir.bin")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Walk(file, Matcher{}, nil, nil)
	if err == nil {
		t.Fatal("Walk(regular file) succeeded, want usage error")
	}
	var usage *report.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("error %v is not a report.UsageError", err)
	}
	if got := report.Classify(err); got != report.ExitUsage {
		t.Errorf("classification = %d, want %d (ExitUsage -> exit 2)", got, report.ExitUsage)
	}
	for _, want := range []string{"not a directory", file} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestWalkUnicodeTree: the DESIGN_SPEC 12.2 fixture spirit at the walk
// layer - unicode directories, files, and symlinks, with unicode-aware
// exclusion patterns applied at walk time. Exact job list, exact
// symlink target, excluded unicode names dropped.
func TestWalkUnicodeTree(t *testing.T) {
	root := t.TempDir()
	dirs := []string{"ドキュメント", "ドキュメント/naïve", "ドキュメント/empty-é"}
	for _, d := range dirs {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	files := map[string]string{
		"ドキュメント/café.txt":         "coffee",
		"ドキュメント/naïve/naïve.txt":  "naive",
		"ドキュメント/emoji-😀.bin":      "emoji",
		"ドキュメント/old.tmp":          "old",
		"ドキュメント/naïve/résumé.tmp": "resume",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := os.Symlink("café.txt", filepath.Join(root, "ドキュメント", "link-café")); err != nil {
		t.Fatal(err)
	}

	m, err := NewMatcher([]string{"*.tmp", "emoji-*.bin"}, "")
	if err != nil {
		t.Fatalf("NewMatcher: %v", err)
	}
	var jobs []Job
	res, err := Walk(root, m, collectJobs(&jobs), nil)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	wantJobs := []string{
		"ドキュメント/café.txt",
		"ドキュメント/naïve/naïve.txt",
	}
	if got := jobRelPaths(jobs); !reflect.DeepEqual(got, wantJobs) {
		t.Errorf("job relpaths = %v, want %v", got, wantJobs)
	}
	if res.Files != len(wantJobs) {
		t.Errorf("Files = %d, want %d", res.Files, len(wantJobs))
	}
	if res.ExcludedFiles != 3 { // old.tmp, résumé.tmp, emoji-😀.bin
		t.Errorf("ExcludedFiles = %d, want 3 (unicode exclusions applied by pattern, not by mangled bytes)", res.ExcludedFiles)
	}
	wantLinks := map[string]string{"ドキュメント/link-café": "café.txt"}
	gotLinks := symlinkMap(res.Symlinks)
	if !reflect.DeepEqual(gotLinks, wantLinks) {
		t.Errorf("symlinks = %v, want %v", gotLinks, wantLinks)
	}
	// The unicode empty dir is preserved (FR-037 at depth).
	if !reflect.DeepEqual(emptyDirPaths(res), []string{"ドキュメント/empty-é"}) {
		t.Errorf("EmptyDirs = %v, want [ドキュメント/empty-é]", emptyDirPaths(res))
	}
}

// TestWalkInvalidUTF8Filename: Linux allows filenames that are not
// valid UTF-8 (raw bytes). The walker must not panic, wildcard patterns
// must still match such names (Go's regexp matches each invalid byte as
// U+FFFD, symmetrically on both sides), and the job relpath must carry
// the bytes through unchanged.
func TestWalkInvalidUTF8Filename(t *testing.T) {
	root := t.TempDir()
	rawName := []byte("bad\xffname.txt")
	if err := os.WriteFile(filepath.Join(root, string(rawName)), []byte("raw"), 0o644); err != nil {
		t.Fatalf("write raw-name file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "plain.txt"), []byte("plain"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("wildcard pattern excludes the raw-byte name", func(t *testing.T) {
		m, err := NewMatcher([]string{"*.txt"}, "")
		if err != nil {
			t.Fatalf("NewMatcher: %v", err)
		}
		var jobs []Job
		res, err := Walk(root, m, collectJobs(&jobs), nil)
		if err != nil {
			t.Fatalf("Walk: %v", err)
		}
		if len(jobs) != 0 || res.ExcludedFiles != 2 {
			t.Errorf("jobs=%v excluded=%d, want no jobs and 2 exclusions", jobRelPaths(jobs), res.ExcludedFiles)
		}
	})

	t.Run("empty matcher emits the raw bytes verbatim", func(t *testing.T) {
		var jobs []Job
		if _, err := Walk(root, Matcher{}, collectJobs(&jobs), nil); err != nil {
			t.Fatalf("Walk: %v", err)
		}
		got := jobRelPaths(jobs)
		want := []string{"bad\xffname.txt", "plain.txt"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("job relpaths = %q, want %q (raw bytes preserved, no panic)", got, want)
		}
	})
}
