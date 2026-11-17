package ingest

// T1.3: the exclusion matcher of DESIGN_SPEC 3.2 (D-08, RESOLVED) and
// IMPLEMENTATION_PLAN T1.3. One matcher consumes both scan-time filter
// inputs -- the repeatable --exclude PATTERN flag and the --exclude-from
// FILE (one pattern per line, '#' comments, blank lines ignored) -- and
// answers the two questions the walker needs at walk time: does this
// DIRECTORY get pruned (fs.SkipDir, subtree never read), and does this
// FILE get dropped (no hash job, no catalog record).
//
// Semantic rows fixed by DESIGN_SPEC 3.2, implemented exactly:
//
//   - A pattern WITHOUT '/' matches any single path component (basename
//     match at any depth, gitignore-like): '*.iso' excludes every .iso
//     at any depth.
//   - A pattern WITH '/' is anchored to the scan root: 'var/log/**'
//     matches 'var/log/x' but never 'var/logx' or 'x/var/log/y'.
//     Inside a pattern, '*' does NOT cross '/' and '**' DOES.
//   - A trailing '/' matches DIRECTORIES ONLY and prunes the subtree.
//   - No negation in V1: '!' has NO special meaning; a pattern is used
//     verbatim (documented judgment call: the simplest honest reading of
//     "no !rules" - a pattern '!foo' excludes entries literally named
//     '!foo', and '!foo' never re-includes anything).
//
// Additional documented decisions (all testable, all in matcher_test.go):
//
//   - Case-SENSITIVE matching, like the gitignore-style tools this
//     mirrors on a case-sensitive Linux filesystem (NFR-001): '*.ISO'
//     does not exclude 'archive.iso'.
//   - A leading '/' is accepted anchor notation (gitignore-compat) and
//     stripped: '/var/log' is identical to 'var/log'.
//   - Character classes ([...]) and backslash escaping are NOT
//     supported; both are literal characters. The DESIGN_SPEC 3.2
//     translation contract (and the T1.3 task) fixes the metacharacters
//     at '*', '**', and '?' only - keeping '[', ']', and '\' literal is
//     the honest reading and avoids path.Match's ErrBadPattern corner
//     cases.
//   - Surrounding whitespace is trimmed from every pattern on input
//     (both the --exclude value and the --exclude-from line), so
//     '  *.tmp  ' and '*.tmp' are the same pattern.
//   - An EMPTY pattern (--exclude '' or an all-whitespace value) is a
//     usage-class error; empty LINES in --exclude-from files are
//     ignored per the file format's own rule.
//   - The compiled matcher is a snapshot: Patterns() returns the
//     effective pattern set in input order (--exclude values first,
//     then --exclude-from lines), which is what runs.exclude_json
//     serialises (DESIGN_SPEC 3.2, consumed by T1.6).
//
// Path convention: every relpath handed to MatchDir/MatchFile is
// SLASH-SEPARATED and RELATIVE TO THE SCAN ROOT, with no leading or
// trailing slash ("var/log/app.log", not "/root/var/log/app.log"). The
// walker (walker.go) produces exactly this form via filepath.Rel plus
// filepath.ToSlash; callers outside the walk layer must respect it or
// anchored patterns will not match.

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"isopress/internal/report"
)

// pattern is one compiled exclusion pattern.
type pattern struct {
	raw     string         // the original (trimmed) pattern, for Patterns()
	re      *regexp.Regexp // compiled matcher over slash-separated relpaths
	dirOnly bool           // trailing '/': applies to directories only
}

// Matcher is the immutable, compiled set of exclusion patterns
// (DESIGN_SPEC 3.2). The zero value is valid and matches nothing, which
// is what the scan command passes when the user supplied no exclusions.
// A Matcher is safe for concurrent use once built (methods only read).
type Matcher struct {
	patterns []pattern
}

// NewMatcher builds a Matcher from the two spec inputs: the repeatable
// --exclude values and the optional --exclude-from file (one pattern per
// line, '#' comment lines and blank lines ignored, surrounding
// whitespace trimmed). --exclude values come first in the effective
// order, then the file's patterns; the order is preserved in
// Patterns() so the runs.exclude_json snapshot is reproducible.
//
// Errors are usage-class (report.UsageError, exit 2 via the cli's
// report.Classify path): an unreadable or missing --exclude-from file,
// and any empty pattern (see the file-level doc comment).
func NewMatcher(excludes []string, excludeFromFile string) (Matcher, error) {
	effective := make([]string, 0, len(excludes))
	effective = append(effective, excludes...)
	if excludeFromFile != "" {
		fromFile, err := readExcludeFile(excludeFromFile)
		if err != nil {
			return Matcher{}, err
		}
		effective = append(effective, fromFile...)
	}

	m := Matcher{patterns: make([]pattern, 0, len(effective))}
	for _, p := range effective {
		cp, err := compilePattern(p)
		if err != nil {
			return Matcher{}, err
		}
		m.patterns = append(m.patterns, cp)
	}
	return m, nil
}

// readExcludeFile reads and parses an --exclude-from file: one pattern
// per line; lines whose first non-whitespace character is '#' are
// comments; blank (or whitespace-only) lines are ignored; everything
// else is trimmed and used verbatim. Read failures are usage-class
// errors with the file path in the message (DESIGN_SPEC 3.2 input).
func readExcludeFile(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &report.UsageError{
			Msg: fmt.Sprintf("--exclude-from %q cannot be read", path),
			Err: err,
		}
	}
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out, nil
}

// compilePattern translates one pattern into an anchored regular
// expression. The translation (DESIGN_SPEC 3.2 "shell-style globs ...
// extended as below"; the explicit table required by T1.3):
//
//	pattern -> regexp, translated then anchored per the rules below
//	'**/'   -> '(?s:.*/)?' (zero or more whole path segments, so
//	                         'a/**/b' also matches 'a/b'; the scoped
//	                         (?s:) flag makes the '.'s match '\n' as
//	                         well - RE2's '.' excludes newline by
//	                         default and '**' must cross ANY byte
//	                         sequence, so without it 'var/**' would
//	                         silently fail to exclude 'var/a\nb',
//	                         an over-inclusion)
//	'**'    -> '(?s:.*)'   (crosses '/' AND '\n'; same (?s:) reason)
//	'*'     -> '[^/]*'      (does NOT cross '/'; '[^/]' already
//	                         matches '\n', no flag needed)
//	'?'     -> '[^/]'       (does NOT cross '/'; matches '\n')
//	any other rune -> regexp.QuoteMeta(rune) (literal)
//
// Anchoring rules:
//
//	no '/' in the pattern -> '(?:^|/)' prefix plus '\z' suffix: the
//	                         pattern matches exactly one path component
//	                         (basename semantics); the start is left
//	                         unanchored so the component boundary can
//	                         be any '/' in the relpath
//	'/' in the pattern    -> '\A' ... '\z': anchored to the scan root
//
// A trailing '/' sets dirOnly and is stripped before translation; a
// leading '/' is stripped as anchor notation (gitignore-compat, since
// any pattern containing '/' is anchored anyway).
func compilePattern(p string) (pattern, error) {
	raw := strings.TrimSpace(p)
	p = raw

	dirOnly := strings.HasSuffix(p, "/")
	p = strings.TrimSuffix(p, "/")
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return pattern{}, &report.UsageError{
			Msg: fmt.Sprintf("exclusion pattern %q is empty after removing anchor slashes; "+
				"an exclusion pattern must name a file or directory", raw),
		}
	}

	anchored := strings.Contains(p, "/")

	// Anchoring: an anchored (contains '/') pattern matches the WHOLE
	// relpath (\A ... \z). A basename pattern is anchored only at the
	// END; its (?:^|/) component-boundary prefix must be free to match
	// a '/' anywhere in the relpath, which requires the regexp engine's
	// default unanchored start (MatchString tries every start offset).
	var reSrc string
	if anchored {
		reSrc = `\A` + globToRegexp(p) + `\z`
	} else {
		reSrc = `(?:^|/)` + globToRegexp(p) + `\z`
	}

	re, err := regexp.Compile(reSrc)
	if err != nil {
		// Unreachable for translated patterns (all literal runes are
		// quoted), but never swallow a compile failure silently.
		return pattern{}, &report.UsageError{
			Msg: fmt.Sprintf("exclusion pattern %q does not compile", raw),
			Err: err,
		}
	}
	return pattern{raw: raw, re: re, dirOnly: dirOnly}, nil
}

// globToRegexp performs the per-rune translation documented on
// compilePattern. It iterates runes (not bytes) so multi-byte UTF-8
// names pass through QuoteMeta unchanged.
func globToRegexp(p string) string {
	var b strings.Builder
	for i := 0; i < len(p); {
		switch p[i] {
		case '*':
			if i+1 < len(p) && p[i+1] == '*' {
				if i+2 < len(p) && p[i+2] == '/' {
					// '**/' crosses '/' and matches zero or more
					// whole segments: 'a/**/b' matches 'a/b'.
					// The scoped (?s:) flag makes '.' match '\n'
					// too (RE2 excludes newline by default), so a
					// '**' pattern cannot silently under-exclude a
					// filename containing a newline.
					b.WriteString(`(?s:.*/)?`)
					i += 3
					continue
				}
				b.WriteString(`(?s:.*)`)
				i += 2
				continue
			}
			b.WriteString(`[^/]*`)
			i++
		case '?':
			b.WriteString(`[^/]`)
			i++
		default:
			r, size := utf8.DecodeRuneInString(p[i:])
			b.WriteString(regexp.QuoteMeta(string(r)))
			i += size
		}
	}
	return b.String()
}

// MatchDir reports whether the DIRECTORY at relpath is excluded and must
// be pruned: the walker returns fs.SkipDir for a true result, so the
// pruned subtree is never read (Go 1.26 WalkDir calls back before
// ReadDir; see walker.go). ALL patterns apply to directories, including
// dirOnly (trailing-slash) patterns; note that an anchored dirOnly
// pattern such as 'var/log/' matches the directory itself, and the
// subtree below it disappears through the prune, not through matching
// every child.
func (m Matcher) MatchDir(relpath string) bool {
	return m.match(relpath, true)
}

// MatchFile reports whether the FILE (or symlink) at relpath is excluded:
// the walker drops the entry without emitting a hash job or recording
// it. dirOnly (trailing-slash) patterns never apply to files, by
// DESIGN_SPEC 3.2 ("matches directories only").
func (m Matcher) MatchFile(relpath string) bool {
	return m.match(relpath, false)
}

func (m Matcher) match(relpath string, isDir bool) bool {
	for _, p := range m.patterns {
		if !isDir && p.dirOnly {
			continue
		}
		if p.re.MatchString(relpath) {
			return true
		}
	}
	// No negation exists (V1 non-goal), so the FIRST match wins outright:
	// an empty pattern list matches nothing.
	return false
}

// Patterns returns the effective pattern set in input order (--exclude
// values first, then --exclude-from lines), each verbatim as supplied
// (trimmed). This is the snapshot T1.6 marshals into runs.exclude_json
// for reproducibility (DESIGN_SPEC 3.2). The returned slice is a copy:
// mutating it does not affect the Matcher.
func (m Matcher) Patterns() []string {
	if len(m.patterns) == 0 {
		return nil
	}
	out := make([]string, len(m.patterns))
	for i, p := range m.patterns {
		out[i] = p.raw
	}
	return out
}
