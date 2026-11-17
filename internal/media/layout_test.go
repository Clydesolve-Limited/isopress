package media

// T3.1 layout tests: the media-path builders of DESIGN_SPEC 5.3 and
// 6.4, the 0-based -> 1-based 5-digit chunk name mapping (PM resolution,
// WORK_LOG), the structural collision immunity of the disjoint files/
// and chunks/ roots, and the hostile-relpath defense.

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestFileMediaPath(t *testing.T) {
	got, err := FileMediaPath("a/b.txt")
	if err != nil || got != "files/a/b.txt" {
		t.Fatalf("FileMediaPath(a/b.txt) = %q, %v; want files/a/b.txt, nil", got, err)
	}
	// A source file literally named "chunks" lives at files/chunks -
	// never colliding with the chunks/ root (5.3 collision rationale).
	got, err = FileMediaPath("chunks")
	if err != nil || got != "files/chunks" {
		t.Fatalf("FileMediaPath(chunks) = %q, %v; want files/chunks, nil", got, err)
	}
	// Empty dirs share the files/ tree at their source relpath.
	got, err = FileMediaPath("docs/empty")
	if err != nil || got != "files/docs/empty" {
		t.Fatalf("FileMediaPath(docs/empty) = %q, %v; want files/docs/empty, nil", got, err)
	}
}

// TestChunkFileNameIndexMapping pins the media-name mapping: catalog
// chunk_index is 0-based, the media name is 1-based zero-padded to 5
// digits (catalog 0 -> media 00001, catalog 12 -> media 00013), per the
// PM resolution and the DESIGN_SPEC 5.3 example chunks/<relpath>/00001.
func TestChunkFileNameIndexMapping(t *testing.T) {
	cases := []struct {
		index0 int64
		want   string
	}{
		{0, "00001"},
		{1, "00002"},
		{12, "00013"},
		{9999, "10000"},
		{99998, "99999"},
	}
	for _, c := range cases {
		got, err := ChunkFileName(c.index0)
		if err != nil || got != c.want {
			t.Errorf("ChunkFileName(%d) = %q, %v; want %q, nil", c.index0, got, err, c.want)
		}
	}
	// Refusals: negative (catalog indices are 0-based) and beyond the
	// fixed 5-digit scheme (index 99999 would render 6 digits).
	if _, err := ChunkFileName(-1); err == nil {
		t.Error("ChunkFileName(-1) succeeded, want error")
	}
	if _, err := ChunkFileName(99999); err == nil {
		t.Error("ChunkFileName(99999) succeeded, want error (breaks the 5-digit name scheme)")
	}
}

func TestChunkMediaPath(t *testing.T) {
	cases := []struct {
		rel    string
		index0 int64
		want   string
	}{
		{"a/b.txt", 0, "chunks/a/b.txt/00001"},
		{"a/b.txt", 12, "chunks/a/b.txt/00013"},
		{"big/archive.bin", 1, "chunks/big/archive.bin/00002"},
	}
	for _, c := range cases {
		got, err := ChunkMediaPath(c.rel, c.index0)
		if err != nil || got != c.want {
			t.Errorf("ChunkMediaPath(%q, %d) = %q, %v; want %q, nil", c.rel, c.index0, got, err, c.want)
		}
	}
	if _, err := ChunkMediaPath("a", -1); err == nil {
		t.Error("ChunkMediaPath(a, -1) succeeded, want error")
	}
	if _, err := ChunkMediaPath("../a", 0); !errors.Is(err, ErrUnsafeRelPath) {
		t.Errorf("ChunkMediaPath(../a, 0) err = %v, want ErrUnsafeRelPath", err)
	}
}

func TestImageDirName(t *testing.T) {
	cases := []struct {
		setID string
		num   int64
		want  string
	}{
		{"home-data-20261018", 1, "home-data-20261018-001"},
		{"home-data-20261018", 42, "home-data-20261018-042"},
		{"S", 999, "S-999"},
		// %03d is a minimum width: the scheme stays deterministic
		// beyond 999 rather than refusing a 1000th image. Past 999 the
		// names are NOT string-sortable (S-1000 bytewise below S-999) -
		// sorting must be numeric on the parsed image number, pinned by
		// TestImageDirNameSortabilityPast999.
		{"S", 1000, "S-1000"},
	}
	for _, c := range cases {
		got, err := ImageDirName(c.setID, c.num)
		if err != nil || got != c.want {
			t.Errorf("ImageDirName(%q, %d) = %q, %v; want %q, nil", c.setID, c.num, got, err, c.want)
		}
	}
	for _, bad := range []struct {
		setID string
		num   int64
	}{
		{"S", 0},   // images are 1-based (6.4)
		{"S", -3},  //
		{"", 1},    // empty set id
		{"a/b", 1}, // slash would escape the destination root
		{".", 1},   //
		{"..", 1},  //
		// Non-UTF-8 set id: becomes the on-media directory name and the
		// manifest's set_id, neither of which can faithfully record raw
		// bytes (UTF-8 invariant, manifest.go file doc).
		{"S\xffT", 1},
	} {
		if _, err := ImageDirName(bad.setID, bad.num); err == nil {
			t.Errorf("ImageDirName(%q, %d) succeeded, want error", bad.setID, bad.num)
		}
	}
}

func TestImageISOFileName(t *testing.T) {
	got, err := ImageISOFileName("home-data-20261018", 1)
	if err != nil || got != "home-data-20261018-001.iso" {
		t.Fatalf("ImageISOFileName = %q, %v; want home-data-20261018-001.iso, nil", got, err)
	}
	if _, err := ImageISOFileName("S", 0); err == nil {
		t.Error("ImageISOFileName(S, 0) succeeded, want error")
	}
}

// TestMediaPathCollisionImpossibility constructs the adversarial source
// names of the plan acceptance criteria and asserts the logical tree
// cannot collide: files/ and chunks/ are disjoint roots (DESIGN_SPEC
// 5.3), so a source file named like a chunk directory, the manifest,
// or the export still gets a unique media path - no escaping scheme
// needed.
func TestMediaPathCollisionImpossibility(t *testing.T) {
	// Pairs of source paths that a naive sidecar layout WOULD collide
	// on; each must produce distinct media paths.
	adversarial := []struct {
		a, b string // two relpaths
	}{
		{"chunks", "chunks"}, // file named "chunks" vs chunked file named "chunks"
		{"MANIFEST.json", "MANIFEST.json"},
		{"catalog-export.sqlite", "catalog-export.sqlite"},
		{"a/b", "a/b"}, // whole file a/b vs chunks of a/b
	}
	seenRoot := map[string]string{} // media path -> originating root
	for _, pair := range adversarial {
		fa, err := FileMediaPath(pair.a)
		if err != nil {
			t.Fatalf("FileMediaPath(%q): %v", pair.a, err)
		}
		ca, err := ChunkMediaPath(pair.a, 0)
		if err != nil {
			t.Fatalf("ChunkMediaPath(%q, 0): %v", pair.a, err)
		}
		if fa == ca {
			t.Errorf("collision: file %q and its chunk both map to %q", pair.a, fa)
		}
		if prev, dup := seenRoot[fa]; dup {
			t.Errorf("media path %q produced twice (roots %s and files)", fa, prev)
		}
		if prev, dup := seenRoot[ca]; dup {
			t.Errorf("media path %q produced twice (roots %s and chunks)", ca, prev)
		}
		seenRoot[fa] = "files"
		seenRoot[ca] = "chunks"
	}

	// Structural property over a corpus of nasty names: every files/
	// path starts with "files/" and every chunks/ path with "chunks/",
	// and the two prefixes never overlap, so no two builds can agree.
	corpus := []string{
		"chunks", "files", "MANIFEST.json", "catalog-export.sqlite",
		"a", "a/b/c/d", "chunks/x", "files/x", "00001",
	}
	for _, rel := range corpus {
		f, err := FileMediaPath(rel)
		if err != nil {
			t.Fatalf("FileMediaPath(%q): %v", rel, err)
		}
		if !strings.HasPrefix(f, FilesRoot+"/") {
			t.Errorf("FileMediaPath(%q) = %q lacks files/ prefix", rel, f)
		}
		c, err := ChunkMediaPath(rel, 0)
		if err != nil {
			t.Fatalf("ChunkMediaPath(%q, 0): %v", rel, err)
		}
		if !strings.HasPrefix(c, ChunksRoot+"/") {
			t.Errorf("ChunkMediaPath(%q, 0) = %q lacks chunks/ prefix", rel, c)
		}
		if f == c {
			t.Errorf("collision on %q: %q", rel, f)
		}
	}
	// The image-root members are not under either root at all.
	rootMembers := []string{ManifestFileName, ExportFileName}
	for _, m := range rootMembers {
		if strings.HasPrefix(m, FilesRoot+"/") || strings.HasPrefix(m, ChunksRoot+"/") {
			t.Errorf("root member %q collides with a payload root", m)
		}
	}
}

// TestValidateRelPathHostile exercises the in-bad-catalog defense: any
// of these relpaths would escape or corrupt the image root if encoded -
// or, for the raw-byte case, corrupt the JSON manifest - so every
// builder must refuse them with ErrUnsafeRelPath.
func TestValidateRelPathHostile(t *testing.T) {
	hostile := []string{
		"",
		"/etc/passwd",     // absolute
		"/",               // absolute root
		"..",              // parent traversal
		"../escape",       // leading traversal
		"a/../../etc",     // mid-path traversal
		"a/../b",          // mid-path traversal (cleaning is NOT enough: refuse)
		"a/.",             // current-dir component
		".",               //
		"a//b",            // empty component
		"a/b/",            // trailing slash -> empty component
		"a\x00b",          // NUL byte
		"bad\xffname.txt", // raw-byte latin-1 name (the ingest walker preserves these; JSON cannot)
	}
	for _, rel := range hostile {
		if err := ValidateRelPath(rel); !errors.Is(err, ErrUnsafeRelPath) {
			t.Errorf("ValidateRelPath(%q) err = %v, want ErrUnsafeRelPath", rel, err)
		}
		if _, err := FileMediaPath(rel); !errors.Is(err, ErrUnsafeRelPath) {
			t.Errorf("FileMediaPath(%q) err = %v, want ErrUnsafeRelPath", rel, err)
		}
		if _, err := ChunkMediaPath(rel, 0); !errors.Is(err, ErrUnsafeRelPath) {
			t.Errorf("ChunkMediaPath(%q, 0) err = %v, want ErrUnsafeRelPath", rel, err)
		}
	}
	// Honest paths pass, including ones with dots INSIDE a component
	// and backslashes (legal filename bytes on the Linux-only target).
	honest := []string{
		"a", "a/b.txt", "a/.hidden", "..hidden", `a\b`, "unicode-\u00e9\u4e2d",
		"00001", "MANIFEST.json.bak",
	}
	for _, rel := range honest {
		if err := ValidateRelPath(rel); err != nil {
			t.Errorf("ValidateRelPath(%q) err = %v, want nil", rel, err)
		}
	}
	// The two byte-level refusals are DISTINGUISHABLE for diagnostics:
	// the NUL case names the NUL byte, the raw-byte case names the
	// UTF-8 reason (the fix-gate assertions rely on this text).
	nulErr := ValidateRelPath("a\x00b")
	utfErr := ValidateRelPath("bad\xffname.txt")
	if nulErr == nil || utfErr == nil {
		t.Fatalf("byte-level refusals: nulErr = %v, utfErr = %v, want both non-nil", nulErr, utfErr)
	}
	if !strings.Contains(nulErr.Error(), "NUL byte") {
		t.Errorf("NUL refusal = %v, want it to name the NUL byte", nulErr)
	}
	if !strings.Contains(utfErr.Error(), "not valid UTF-8") {
		t.Errorf("raw-byte refusal = %v, want it to name the UTF-8 reason", utfErr)
	}
	if nulErr.Error() == utfErr.Error() {
		t.Errorf("NUL and non-UTF-8 refusals share one message %q; diagnostics must distinguish them", nulErr)
	}
}

// TestChunkFileNameFormatting pins the zero-padding width itself so a
// future edit cannot silently change 5 digits.
func TestChunkFileNameFormatting(t *testing.T) {
	name, err := ChunkFileName(7)
	if err != nil {
		t.Fatalf("ChunkFileName(7): %v", err)
	}
	if name != fmt.Sprintf("%05d", 8) || len(name) != 5 {
		t.Fatalf("ChunkFileName(7) = %q, want 5-digit 00008", name)
	}
}
