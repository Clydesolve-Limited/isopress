package xorriso

// Version model and the DESIGN_SPEC 11 minimum-version floor (T4.1).
//
// The version probe parses the banner that `xorriso -version` prints.
// Parsing is deliberately tolerant about the SHAPE (major, major.minor,
// or major.minor.patch, optional distro suffixes) but deliberately
// STRICT about failure: an output from which no version can be parsed is
// a DependencyError carrying the raw output, never a silent pass
// (NFR-004 fail-fast; an unparsable xorriso is an unknown xorriso).

import (
	"regexp"
	"strconv"
)

// MinVersion is the DESIGN_SPEC 11 recommended-minimum xorriso version:
// >= 1.5.4 (Debian bookworm ships 1.5.4, so that is the tested floor).
// An older binary is refused with a DependencyError naming the found and
// required versions (DESIGN_SPEC 10.1 DependencyError, exit 3). Treat as
// read-only.
var MinVersion = Version{Major: 1, Minor: 5, Patch: 4}

// Version is a semantic-ish xorriso version triple. Missing components
// parse as zero ("xorriso 1.5" becomes 1.5.0), so floor comparisons stay
// total and predictable.
type Version struct {
	Major int
	Minor int
	Patch int
}

// String renders the canonical "major.minor.patch" form used in error
// messages ("found xorriso 1.4.2, need >= 1.5.4").
func (v Version) String() string {
	return strconv.Itoa(v.Major) + "." + strconv.Itoa(v.Minor) + "." + strconv.Itoa(v.Patch)
}

// AtLeast reports whether v satisfies the floor comparison v >= min,
// component by component.
func (v Version) AtLeast(min Version) bool {
	if v.Major != min.Major {
		return v.Major > min.Major
	}
	if v.Minor != min.Minor {
		return v.Minor > min.Minor
	}
	return v.Patch >= min.Patch
}

// versionRE matches the first "xorriso <major>[.<minor>[.<patch>]]"
// occurrence in probe output. It requires a non-digit right after the
// tool name, so reference lines such as "xorriso version   :  1.5.6" or
// "libisofs in use : 1.5.6" never match; only banner lines of the shape
// "xorriso 1.5.6 ..." do. Optional minor/patch cover partial shapes
// ("xorriso 1.5 : ..."); distro suffixes ("1.5.6-1ubuntu1") and extra
// components ("1.5.6.1") parse to the leading triple. When the banner
// appears multiple times (the real binary prints it on BOTH stdout and
// stderr), the FIRST occurrence wins; a mixed-version output therefore
// fails conservatively toward the older version.
var versionRE = regexp.MustCompile(`xorriso\s+(\d+)(?:\.(\d+))?(?:\.(\d+))?`)

// parseVersion extracts the version triple from `xorriso -version`
// output. ok is false when no banner line exists at all, or when a
// banner carries components no int can hold; both are DependencyError-
// class conditions at the Probe level, never a pass.
func parseVersion(output string) (v Version, ok bool) {
	m := versionRE.FindStringSubmatch(output)
	if m == nil {
		return Version{}, false
	}
	components := []*int{&v.Major, &v.Minor, &v.Patch}
	for i, dst := range components {
		if m[i+1] == "" {
			continue // absent component stays zero
		}
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			// \d+ that does not fit an int: not a version we can
			// reason about, so refuse rather than guess.
			return Version{}, false
		}
		*dst = n
	}
	return v, true
}
