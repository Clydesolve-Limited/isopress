package xorriso

// T4.1 version-parse unit tests (DESIGN_SPEC 11 version floor, T4.1
// acceptance "version probe against the real binary" plus the
// table-driven shapes: floor passes, below-floor fails, future majors
// pass, garbage rejected, partial-version shapes tolerated).

import (
	"testing"
)

func TestParseVersionShapes(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   Version
		ok     bool
	}{
		{
			// The real stderr banner, verbatim from xorriso 1.5.6
			// on the dev machine (probe output verification, T4.1).
			name:   "stderr banner line of the real binary",
			output: "xorriso 1.5.6 : RockRidge filesystem manipulator, libburnia project.\n",
			want:   Version{1, 5, 6},
			ok:     true,
		},
		{
			// The real stdout first line, also verbatim.
			name:   "stdout first line of the real binary",
			output: "xorriso 1.5.6\nISO 9660 Rock Ridge filesystem manipulator and CD/DVD/BD burn program\n",
			want:   Version{1, 5, 6},
			ok:     true,
		},
		{
			name:   "combined output with the banner on both streams",
			output: "xorriso 1.5.6 : RockRidge filesystem manipulator, libburnia project.\n\nxorriso 1.5.6\nISO 9660 Rock Ridge filesystem manipulator\nxorriso version   :  1.5.6\nlibisofs   in use :  1.5.6  (min. 1.5.6)\n",
			want:   Version{1, 5, 6},
			ok:     true,
		},
		{
			name:   "exact floor version passes the parse",
			output: "xorriso 1.5.4 : RockRidge filesystem manipulator, libburnia project.\n",
			want:   Version{1, 5, 4},
			ok:     true,
		},
		{
			name:   "future major version shape",
			output: "xorriso 2.0.0 : RockRidge filesystem manipulator, libburnia project.\n",
			want:   Version{2, 0, 0},
			ok:     true,
		},
		{
			name:   "future minor with two components only",
			output: "xorriso 1.6 : someday-shape\n",
			want:   Version{1, 6, 0},
			ok:     true,
		},
		{
			name:   "partial two-component shape",
			output: "xorriso 1.5 : partial-shape\n",
			want:   Version{1, 5, 0},
			ok:     true,
		},
		{
			name:   "single-component shape",
			output: "xorriso 2\n",
			want:   Version{2, 0, 0},
			ok:     true,
		},
		{
			name:   "four-component shape keeps the leading triple",
			output: "xorriso 1.5.6.1 distro-extra\n",
			want:   Version{1, 5, 6},
			ok:     true,
		},
		{
			name:   "distro package suffix",
			output: "xorriso 1.5.6-1ubuntu1 : RockRidge\n",
			want:   Version{1, 5, 6},
			ok:     true,
		},
		{
			// A version REFERENCE line is not the banner: the
			// parser must not accept "xorriso version : X.Y.Z".
			name:   "version reference line alone is rejected",
			output: "xorriso version   :  1.5.6\n",
			ok:     false,
		},
		{
			name:   "library version line alone is rejected",
			output: "libisofs   in use :  1.5.6  (min. 1.5.6)\n",
			ok:     false,
		},
		{
			name:   "garbage is rejected",
			output: "banana phone\n",
			ok:     false,
		},
		{
			name:   "empty output is rejected",
			output: "",
			ok:     false,
		},
		{
			// A digit string no int can hold is not a version we
			// can reason about; refuse rather than guess.
			name:   "absurdly large component is rejected",
			output: "xorriso 99999999999999999999999999 : overflow\n",
			ok:     false,
		},

		// --- adversarial beyond the shape table ---------------------------

		{
			// The banner is not required to be the first bytes of
			// the output: leading whitespace/blank lines (a locale
			// preamble, a progress line) must not hide it.
			name:   "leading whitespace before the banner is tolerated",
			output: "\n\n \txorriso 1.5.6 : RockRidge\n",
			want:   Version{1, 5, 6},
			ok:     true,
		},
		{
			// A banner appearing in the MIDDLE of a long transcript
			// still parses (the regex scans the whole output, it is
			// not line-anchored).
			name:   "banner buried in the middle of long output",
			output: "progress: loading image\nprogress: writing table\necho xorriso-payload-not-a-banner\nxorriso 1.5.6 : RockRidge filesystem manipulator\nprogress: done\n",
			want:   Version{1, 5, 6},
			ok:     true,
		},
		{
			// FIRST-OCCURRENCE RULE: when the banner appears twice
			// with different versions (a wrapped or tampered
			// binary), the FIRST wins, so parsing fails
			// conservatively toward the older version rather than
			// letting a later banner upgrade itself past the floor.
			name:   "first banner wins over a newer later banner",
			output: "xorriso 1.5.4 : banner\n...\nxorriso 1.5.6 : banner\n",
			want:   Version{1, 5, 4},
			ok:     true,
		},
		{
			// A reference line mentioning an OLDER version before
			// the real banner is not a banner (non-digit after the
			// tool name) and must not shadow it; the parser scans
			// past it to the real banner.
			name:   "reference line before the banner does not shadow it",
			output: "compiled against xorriso (libburn 0.9.4 headers)\nxorriso 1.5.6 : RockRidge\n",
			want:   Version{1, 5, 6},
			ok:     true,
		},
		{
			// Go-style "v" prefix: NOT a xorriso banner shape (the
			// real binary has never printed one). The parser must
			// fail CONSERVATIVELY (unknown xorriso is never a
			// silent pass, NFR-004); the DependencyError carries
			// the raw output so an operator can see it instantly.
			name:   "v-prefixed version is conservatively rejected",
			output: "xorriso v1.5.6 : RockRidge\n",
			ok:     false,
		},
		{
			// Locale-injected output: translated prose around the
			// banner is fine, but prose with NO banner shape is a
			// hard failure - a localized xorriso must not
			// accidentally pass the floor via its translation.
			name:   "locale-translated output without a banner is rejected",
			output: "xorriso wird gestartet...\nVersion unbekannt\n",
			ok:     false,
		},
		{
			// A locale line that MENTIONS xorriso plus an old
			// number before the real banner triggers the
			// first-occurrence rule and yields the older version:
			// conservative (fails the floor loudly) rather than
			// riskily skipping to the newer banner.
			name:   "locale line with an old version number wins first",
			output: "xorriso 0.9 wird empfohlen\nxorriso 1.5.6 : RockRidge\n",
			want:   Version{0, 9, 0},
			ok:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := parseVersion(tt.output)
			if ok != tt.ok {
				t.Fatalf("parseVersion(%q) ok = %v, want %v (got %v)", tt.output, ok, tt.ok, got)
			}
			if ok && got != tt.want {
				t.Errorf("parseVersion(%q) = %v, want %v", tt.output, got, tt.want)
			}
		})
	}
}

// The floor comparison itself: 1.5.4 is the DESIGN_SPEC 11 minimum, so
// 1.5.3 and older fail while 1.5.4, 1.5.6, 2.0.0, and any higher major
// pass.
func TestVersionAtLeastFloor(t *testing.T) {
	tests := []struct {
		v    Version
		want bool
	}{
		{Version{1, 4, 99}, false},
		{Version{1, 5, 3}, false},
		{Version{1, 5, 4}, true},
		{Version{1, 5, 6}, true},
		{Version{1, 6, 0}, true},
		{Version{2, 0, 0}, true},
		{Version{10, 0, 0}, true},
	}
	for _, tt := range tests {
		if got := tt.v.AtLeast(MinVersion); got != tt.want {
			t.Errorf("(%v).AtLeast(%v) = %v, want %v", tt.v, MinVersion, got, tt.want)
		}
	}
}

// The floor is exactly the DESIGN_SPEC 11 value, and String renders the
// canonical form used in DependencyError messages.
func TestMinVersionIsTheSpecFloor(t *testing.T) {
	if MinVersion != (Version{1, 5, 4}) {
		t.Fatalf("MinVersion = %v, want 1.5.4 (DESIGN_SPEC 11)", MinVersion)
	}
	if got := MinVersion.String(); got != "1.5.4" {
		t.Errorf("MinVersion.String() = %q, want %q", got, "1.5.4")
	}
	if got := (Version{2, 0, 0}).String(); got != "2.0.0" {
		t.Errorf("Version{2,0,0}.String() = %q, want %q", got, "2.0.0")
	}
}
