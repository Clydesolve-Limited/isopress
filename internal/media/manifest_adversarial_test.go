package media

// T3.1 adversarial and real-world shape tests for MANIFEST.json - the
// 10-years-from-now artifact (DESIGN_SPEC 6.6). The golden tests in
// manifest_test.go pin the ASCII happy-path bytes; this file pins the
// shapes a decade-old artifact is most likely to meet and a corrupt
// catalog can throw at it:
//
//   - unicode / very-long relpaths and JSON escaping (HTML escaping of
//     less-than, greater-than, ampersand is ON by default in
//     encoding/json - a future SetEscapeHTML(false) would change
//     artifact bytes; U+2028/U+2029 are escaped for JS-viewer safety;
//     both pinned here);
//   - symlink targets with unicode/newline round-tripping byte-exact
//     (and size being BYTE length, not rune count);
//   - a chunked parent whose chunks SPAN several images: each image's
//     reassembly table lists only its own parts, non-contiguous parts
//     are permitted (contiguity is a planner invariant, 12.5, not a
//     manifest one), and the per-image placement lists the parent iff
//     that image holds any of its chunks;
//   - a hard-link family whose names live on DIFFERENT images: every
//     image carrying the group records ALL names plus the shared hash
//     (D-04 mode-independence - the group is the restore authority,
//     not an inventory of this image);
//   - image numbers beyond 999 (S-1000 unpadded) and the sortability
//     reality past 999;
//   - nil input sections rendering [] (never null) end to end, and the
//     MarshalManifest boundary for hand-built Manifests;
//   - payload files named like the image-root members (MANIFEST.json,
//     catalog-export.sqlite) or like the chunks/ root - shadowing is
//     structurally impossible;
//   - chunk-index boundaries through the manifest pipeline and the
//     chunk_index 0 omitempty hazard (pointer survives marshaling);
//   - MarshalManifest totality (its error return is defensive; see
//     TestMarshalManifestTotality for what that means for raw-byte
//     relpaths).
//
// Escape-sequence note for readers: this file deliberately constructs
// every JSON escape sequence (backslash + uXXXX, backslash + n/t)
// at runtime from the rune 92, so the source itself contains no
// backslash-escapes that an editor or generator could silently
// reinterpret.

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

// backslash and the JSON escape sequences built from it: six-char
// forms like backslash-u003c are what encoding/json emits for the
// HTML-special characters, and the artifact bytes must contain them.
var (
	backslash = string(rune(92))
	escLT     = backslash + "u003c" // less-than
	escGT     = backslash + "u003e" // greater-than
	escAmp    = backslash + "u0026" // ampersand
	escLS     = backslash + "u2028" // line separator
	escPS     = backslash + "u2029" // paragraph separator
	escNL     = backslash + "n"     // newline
	escTAB    = backslash + "t"     // tab
	escFFFD   = backslash + "ufffd" // the U+FFFD replacement char
)

// TestManifestUnicodeAndEscapingRoundTrip feeds the manifest unicode,
// HTML-special, and line-separator characters in media paths and
// asserts BOTH the exact raw-byte rendering AND a lossless structural
// re-parse. This pins three encoding/json behaviors the artifact's
// byte format depends on:
//
//  1. less-than, greater-than, ampersand render as the escapes
//     u003c/u003e/u0026 (HTML escaping is ON by default in
//     json.Marshal/MarshalIndent). A future change to
//     json.Encoder.SetEscapeHTML(false) would silently change every
//     future manifest's bytes - this test makes that a loud failure.
//  2. Valid UTF-8 (accents, CJK, emoji) passes through RAW, unescaped.
//  3. U+2028/U+2029 are escaped as u2028/u2029 (valid UTF-8 but
//     line-separator-unsafe for JavaScript viewers of the JSON).
func TestManifestUnicodeAndEscapingRoundTrip(t *testing.T) {
	relHTML := "weird/<a>&b café/中文😀.txt"
	relSep := "u2028-" + string(rune(0x2028)) + "-" + string(rune(0x2029)) + ".txt"
	relLong := longRelPath(24, 200)

	in := ManifestInput{
		SetID:         "données-20261018",
		ImageNumber:   1,
		ToolVersion:   "0.1.0-dev",
		SchemaVersion: 1,
		CreatedAt:     fixedCreatedAt,
		Entries: []ImageEntry{
			// HTML-special characters in a filename - must render
			// escaped, never raw.
			{Kind: KindFile, RelPath: relHTML, Size: 3, SHA256: hashBytes(0x71),
				Mode: 0644, UID: 1000, GID: 1000, MtimeNS: 1729340000000000001},
			// Line-separator characters in a filename (legal on
			// Linux, hostile to naive JS consumers of the JSON).
			{Kind: KindFile, RelPath: relSep, Size: int64(len(relSep)), SHA256: hashBytes(0x72),
				Mode: 0644, UID: 1000, GID: 1000, MtimeNS: 1729340000000000002},
			// Very long path: 24 levels of 200-byte components
			// (~4.8 KiB total) - no length truncation anywhere in the
			// pipeline.
			{Kind: KindFile, RelPath: relLong, Size: 5, SHA256: hashBytes(0x73),
				Mode: 0644, UID: 1000, GID: 1000, MtimeNS: 1729340000000000003},
		},
		Placement: []PlacementInput{
			{ImageNumber: 1, Paths: []string{relHTML, relSep, relLong}},
		},
	}
	m, err := GenerateManifest(in)
	if err != nil {
		t.Fatalf("GenerateManifest: %v", err)
	}
	raw := string(mustMarshal(t, m))

	// 1. HTML escaping pinned ON.
	wantEsc := escLT + "a" + escGT + escAmp + "b café/中文😀.txt"
	if !strings.Contains(raw, wantEsc) {
		t.Errorf("HTML-escaping pin: raw manifest lacks %q:\n%s", wantEsc, raw)
	}
	// 2. Valid UTF-8 passes through raw.
	if !strings.Contains(raw, "café/中文😀.txt") {
		t.Errorf("unicode passthrough pin: raw manifest lacks literal cafe/CJK/emoji path:\n%s", raw)
	}
	// 3. U+2028/U+2029 escaped, never literal.
	if !strings.Contains(raw, escLS) || !strings.Contains(raw, escPS) {
		t.Errorf("U+2028/U+2029 escaping pin: raw manifest lacks the escapes:\n%s", raw)
	}
	if strings.ContainsRune(raw, rune(0x2028)) || strings.ContainsRune(raw, rune(0x2029)) {
		t.Errorf("literal U+2028/U+2029 leaked into the manifest bytes (JS-hostile):\n%s", raw)
	}

	// Structural re-parse: unmarshaling back yields EXACTLY the
	// generated value - no escaping surprise, no truncation.
	var back Manifest
	if err := json.Unmarshal(mustMarshal(t, m), &back); err != nil {
		t.Fatalf("re-parse manifest: %v", err)
	}
	if !reflect.DeepEqual(back, m) {
		t.Errorf("round-trip mismatch\n--- generated ---\n%#v\n--- re-parsed ---\n%#v", m, back)
	}

	// Format-shape re-parse: exactly the nine documented top-level
	// keys; entries is an array of objects carrying the documented
	// file-entry keys.
	var doc map[string]any
	if err := json.Unmarshal(mustMarshal(t, m), &doc); err != nil {
		t.Fatalf("re-parse manifest as map: %v", err)
	}
	wantKeys := []string{
		"set_id", "image_number", "tool_version", "schema_version", "created_at",
		"entries", "chunked_files", "hard_link_groups", "placement",
	}
	if len(doc) != len(wantKeys) {
		t.Errorf("top-level key count = %d, want %d (%v)", len(doc), len(wantKeys), doc)
	}
	for _, k := range wantKeys {
		if _, ok := doc[k]; !ok {
			t.Errorf("top-level key %q missing", k)
		}
	}
	ents, ok := doc["entries"].([]any)
	if !ok || len(ents) != 3 {
		t.Fatalf("entries = %T len %v, want array of 3", doc["entries"], doc["entries"])
	}
	entryKeys := []string{"path", "kind", "size", "sha256", "mtime_ns", "mode", "uid", "gid"}
	for i, e := range ents {
		em, ok := e.(map[string]any)
		if !ok {
			t.Fatalf("entries[%d] = %T, want object", i, e)
		}
		for _, k := range entryKeys {
			if _, ok := em[k]; !ok {
				t.Errorf("entries[%d] (kind file) key %q missing", i, k)
			}
		}
	}
}

// longRelPath builds a deep-and-wide relative path of n components of
// size bytes each - real trees hit long paths (mail archives, npm
// caches), and the manifest must carry them untruncated.
func longRelPath(n, size int) string {
	comp := strings.Repeat("x", size)
	return strings.Repeat(comp+"/", n) + comp + ".bin"
}

// TestManifestSymlinkTargetRoundTrip: a symlink target with unicode,
// newlines, tabs, quotes, and backslashes must survive the manifest
// byte-for-byte (D-03: the recorded target IS what restore recreates),
// and Size is the target's BYTE length, not its rune count.
func TestManifestSymlinkTargetRoundTrip(t *testing.T) {
	target := "ドキュメント/../café.txt" + string(rune(10)) +
		"with" + string(rune(9)) + `tab"quote` + backslash + "back ä"
	in := ManifestInput{
		SetID:         "S",
		ImageNumber:   1,
		ToolVersion:   "0.1.0-dev",
		SchemaVersion: 1,
		CreatedAt:     fixedCreatedAt,
		Entries: []ImageEntry{
			{Kind: KindSymlink, RelPath: "docs/link", LinkTarget: target, Size: int64(len(target)),
				Mode: 0777, UID: 1000, GID: 1000, MtimeNS: 1},
		},
		Placement: []PlacementInput{{ImageNumber: 1, Paths: []string{"docs/link"}}},
	}
	m, err := GenerateManifest(in)
	if err != nil {
		t.Fatalf("GenerateManifest: %v", err)
	}
	raw := string(mustMarshal(t, m))
	// Newline and tab are escaped in the artifact; the target is never
	// emitted as a raw control character that would break
	// line-oriented consumers of the JSON.
	if !strings.Contains(raw, escNL) || !strings.Contains(raw, escTAB) {
		t.Errorf("target control characters not escaped:\n%s", raw)
	}
	var back Manifest
	if err := json.Unmarshal(mustMarshal(t, m), &back); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if got := back.Entries[0].LinkTarget; got != target {
		t.Errorf("link_target round-trip:\n got %q\nwant %q", got, target)
	}
	if back.Entries[0].Size != int64(len(target)) {
		t.Errorf("size = %d, want byte length %d", back.Entries[0].Size, len(target))
	}

	// Size is BYTE length: a rune-count mistake (len of the two-byte
	// "ä" is 2 bytes, 1 rune) must be refused, not silently accepted.
	runeMistake := comprehensiveInput()
	runeMistake.Entries[1].LinkTarget = "ä" // 2 bytes
	runeMistake.Entries[1].Size = 1         // 1 rune - wrong
	if _, err := GenerateManifest(runeMistake); err == nil || !strings.Contains(err.Error(), "len(target)") {
		t.Errorf("rune-count symlink size: err = %v, want refusal naming len(target)", err)
	}
}

// TestManifestChunkedFileSpanningImages: a chunked parent whose chunks
// SPAN images. Each image's reassembly table lists ONLY that image's
// parts (restore collects the rest via the whole-set placement); the
// per-image part list is NOT required contiguous (contiguity over the
// set is a planner invariant, DESIGN_SPEC 12.5, not a manifest one);
// the whole-file anchor is identical on every image holding a part.
func TestManifestChunkedFileSpanningImages(t *testing.T) {
	const parent = "big/archive.bin"
	// Per-image chunk indices: image 1 holds 0 and 2 (non-contiguous),
	// image 2 holds 1 and 3. Union 0..3 is complete.
	type shard struct {
		image   int64
		indices []int64
	}
	shards := []shard{{1, []int64{0, 2}}, {2, []int64{1, 3}}}
	wholeHash := hashBytes(0xAA)

	// The whole-set placement: the parent appears on every image
	// holding ANY of its chunks (ImagePlacement doc, 6.6).
	placement := []PlacementInput{
		{ImageNumber: 1, Paths: []string{parent}},
		{ImageNumber: 2, Paths: []string{parent}},
	}

	covered := map[int64]bool{}
	for _, sh := range shards {
		in := ManifestInput{
			SetID:         "S",
			ImageNumber:   sh.image,
			ToolVersion:   "0.1.0-dev",
			SchemaVersion: 1,
			CreatedAt:     fixedCreatedAt,
			ChunkedFiles: []ChunkedFileInput{
				{RelPath: parent, WholeSize: 4 * 4096, WholeSHA256: wholeHash},
			},
			Placement: placement,
		}
		// Deliberately reverse the entry order to re-verify sorting.
		for i := len(sh.indices) - 1; i >= 0; i-- {
			idx := sh.indices[i]
			in.Entries = append(in.Entries, ImageEntry{
				Kind: KindChunk, RelPath: parent, ChunkIndex: idx,
				Size: 4096, SHA256: hashBytes(byte(0x10 + idx)),
			})
		}
		m, err := GenerateManifest(in)
		if err != nil {
			t.Fatalf("image %d: GenerateManifest: %v", sh.image, err)
		}
		cfs := m.ChunkedFiles
		if len(cfs) != 1 {
			t.Fatalf("image %d: %d chunked_files, want 1", sh.image, len(cfs))
		}
		cf := cfs[0]
		if cf.RelPath != parent || cf.Size != 4*4096 || cf.SHA256 != hashHex(0xAA) {
			t.Errorf("image %d: whole-file anchor = %+v, want the shared parent anchor", sh.image, cf)
		}
		// ONLY this image's parts, in ascending reassembly order.
		if len(cf.Chunks) != len(sh.indices) {
			t.Fatalf("image %d: %d parts, want %d (this image's share only)",
				sh.image, len(cf.Chunks), len(sh.indices))
		}
		for i, part := range cf.Chunks {
			if part.Index != sh.indices[i] {
				t.Errorf("image %d: parts[%d].Index = %d, want %d (ascending, own parts only)",
					sh.image, i, part.Index, sh.indices[i])
			}
			if part.Length != 4096 {
				t.Errorf("image %d: parts[%d].Length = %d, want 4096", sh.image, i, part.Length)
			}
			if part.SHA256 != hashHex(byte(0x10+part.Index)) {
				t.Errorf("image %d: parts[%d].SHA256 = %s, want hash of its own chunk", sh.image, i, part.SHA256)
			}
			covered[part.Index] = true
		}
		// Each part entry also records its own catalog index (the
		// entry list and the table cannot drift: parts are collected
		// FROM the entries).
		if len(m.Entries) != len(sh.indices) {
			t.Errorf("image %d: %d chunk entries, want %d", sh.image, len(m.Entries), len(sh.indices))
		}
	}
	// The two per-image tables TOGETHER cover every index - the
	// set-level contiguity the manifest itself does not require.
	for i := int64(0); i < 4; i++ {
		if !covered[i] {
			t.Errorf("chunk index %d missing from every per-image table", i)
		}
	}

	// Image 3 holds NONE of the parent's chunks: it must have NO
	// chunked_files record (a parent appears only where this image
	// holds a part), and the placement summary must NOT list the
	// parent under image 3 (FR-033: which image holds which paths).
	in3 := ManifestInput{
		SetID:         "S",
		ImageNumber:   3,
		ToolVersion:   "0.1.0-dev",
		SchemaVersion: 1,
		CreatedAt:     fixedCreatedAt,
		Entries: []ImageEntry{
			{Kind: KindFile, RelPath: "other/stuff.bin", Size: 7, SHA256: hashBytes(0x77),
				Mode: 0644, UID: 1000, GID: 1000},
		},
		// Parent listed on images 1 and 2 ONLY.
		Placement: []PlacementInput{
			{ImageNumber: 1, Paths: []string{parent}},
			{ImageNumber: 2, Paths: []string{parent}},
			{ImageNumber: 3, Paths: []string{"other/stuff.bin"}},
		},
	}
	m3, err := GenerateManifest(in3)
	if err != nil {
		t.Fatalf("image 3 (no parts): GenerateManifest: %v", err)
	}
	if len(m3.ChunkedFiles) != 0 {
		t.Errorf("image 3: chunked_files = %+v, want none (holds no part of %q)", m3.ChunkedFiles, parent)
	}
	for _, p := range m3.Placement {
		if p.ImageNumber == 3 {
			for _, path := range p.Paths {
				if path == parent {
					t.Errorf("placement lists %q on image 3, which holds none of its chunks", parent)
				}
			}
		}
	}
}

// TestManifestHardLinkFamilyCrossImage: a D-04 family whose primary
// lives on a different image than its secondary. Copy mode stores the
// primary once with secondaries as link(2); ISO mode stores only the
// primary name. In BOTH modes the manifest records ALL names plus the
// shared hash on every image that carries the group - the group is the
// mode-independent authority a media-alone restore recreates the
// family from, NOT an inventory of this image's payload. Consequently
// the generator must NOT gate group names on this image's entries or
// placement.
func TestManifestHardLinkFamilyCrossImage(t *testing.T) {
	const primary, secondary = "data/primary", "data/secondary"
	family := HardLinkGroupInput{SHA256: hashBytes(0x42), Names: []string{primary, secondary}}
	placement := []PlacementInput{
		{ImageNumber: 1, Paths: []string{primary}},
		{ImageNumber: 2, Paths: []string{secondary}},
		{ImageNumber: 3, Paths: []string{}}, // iso-style image: no payload of its own
	}

	build := func(image int64, entries []ImageEntry, groups []HardLinkGroupInput) Manifest {
		t.Helper()
		m, err := GenerateManifest(ManifestInput{
			SetID: "S", ImageNumber: image, ToolVersion: "0.1.0-dev", SchemaVersion: 1,
			CreatedAt: fixedCreatedAt, Entries: entries, HardLinkGroups: groups, Placement: placement,
		})
		if err != nil {
			t.Fatalf("image %d: GenerateManifest: %v", image, err)
		}
		return m
	}

	fileEntry := func(rel string) ImageEntry {
		return ImageEntry{Kind: KindFile, RelPath: rel, Size: 100, SHA256: family.SHA256,
			Mode: 0644, UID: 1000, GID: 1000, MtimeNS: 42}
	}

	// Image 1 holds ONLY the primary's entry, image 2 only the
	// secondary's - yet both manifests must record the FULL family.
	m1 := build(1, []ImageEntry{fileEntry(primary)}, []HardLinkGroupInput{family})
	m2 := build(2, []ImageEntry{fileEntry(secondary)}, []HardLinkGroupInput{family})
	for _, m := range []Manifest{m1, m2} {
		if len(m.HardLinkGroups) != 1 {
			t.Fatalf("image %d: %d groups, want 1", m.ImageNumber, len(m.HardLinkGroups))
		}
		g := m.HardLinkGroups[0]
		if !reflect.DeepEqual(g.Names, []string{primary, secondary}) {
			t.Errorf("image %d: group names = %v, want ALL family names ascending (D-04: all names + shared hash)",
				m.ImageNumber, g.Names)
		}
		if g.SHA256 != hashHex(0x42) {
			t.Errorf("image %d: group sha256 = %s, want shared hash", m.ImageNumber, g.SHA256)
		}
	}
	// The cross-image family record is identical on both images: the
	// mode-independent restore authority cannot depend on which image
	// a reader happens to hold.
	if !reflect.DeepEqual(m1.HardLinkGroups, m2.HardLinkGroups) {
		t.Errorf("cross-image family differs between manifests:\nimg1 %+v\nimg2 %+v",
			m1.HardLinkGroups, m2.HardLinkGroups)
	}
	// Note both images accepted the group although the secondary name
	// is neither an entry nor a placement path of image 1 (and vice
	// versa): the group is deliberately wider than the image, and the
	// gates are one-directional - pinned by the successful builds.

	// ISO-mode shape: an image that stores NO member of the family
	// still records the group (caller-supplied; no entry gate).
	m3 := build(3, nil, []HardLinkGroupInput{family})
	if !reflect.DeepEqual(m3.HardLinkGroups[0].Names, []string{primary, secondary}) {
		t.Errorf("iso-style image 3: group names = %v, want the full family", m3.HardLinkGroups[0].Names)
	}
	// But the placement summary still needs a record for the image
	// itself (FR-033 whole-set record on every image).
	if _, err := GenerateManifest(ManifestInput{
		SetID: "S", ImageNumber: 3, ToolVersion: "0.1.0-dev", SchemaVersion: 1, CreatedAt: fixedCreatedAt,
		HardLinkGroups: []HardLinkGroupInput{family},
		Placement:      placement[:2], // no record for image 3
	}); err == nil {
		t.Error("image 3 absent from placement: GenerateManifest succeeded, want refusal")
	}
}

// TestManifestImageNumberBeyond999: image numbers above 999 render
// unpadded (S-1000) - deterministic and refused-by-nothing, exactly as
// layout.go documents. The manifest itself carries image_number as an
// integer, so ordering INSIDE the artifact is numeric regardless.
func TestManifestImageNumberBeyond999(t *testing.T) {
	in := comprehensiveInput()
	in.ImageNumber = 1000
	in.Placement = []PlacementInput{{ImageNumber: 1000, Paths: comprehensiveInput().Placement[1].Paths}}
	m, err := GenerateManifest(in)
	if err != nil {
		t.Fatalf("GenerateManifest(image 1000): %v", err)
	}
	if !strings.Contains(string(mustMarshal(t, m)), `"image_number": 1000`) {
		t.Errorf("image_number 1000 not rendered verbatim:\n%s", mustMarshal(t, m))
	}
	// The matching directory/ISO names are deterministic pure
	// functions.
	for _, c := range []struct {
		num  int64
		want string
	}{
		{999, "S-999"},
		{1000, "S-1000"},
		{9999, "S-9999"},
		{10000, "S-10000"},
	} {
		got, err := ImageDirName("S", c.num)
		if err != nil || got != c.want {
			t.Errorf("ImageDirName(S, %d) = %q, %v; want %q, nil", c.num, got, err, c.want)
		}
		iso, err := ImageISOFileName("S", c.num)
		if err != nil || iso != c.want+".iso" {
			t.Errorf("ImageISOFileName(S, %d) = %q, %v; want %q.iso, nil", c.num, iso, err, c.want)
		}
	}
}

// TestImageDirNameSortabilityPast999 documents the REAL byte-wise
// ordering of the unpadded names beyond 999. layout.go's doc comment
// (corrected per the T3.1 review finding) now states that past 999 the
// names are NOT lexicographically sortable - bytewise the digit 1
// precedes 9, so S-1000 sorts BELOW S-999 and a plain string sort
// misplaces every 4-digit image among the 3-digit ones. The naming
// behavior itself is correct (deterministic, spec-conformant 6.4);
// consumers - including humans running ls - must sort NUMERICALLY on
// the parsed image number, which is what the tool's own discovery does
// (8.3 finds <SET-ID>-NNN by pattern and parses the number). This test
// pins that reality so no one "fixes" the corrected comment's claim
// into truth through an accidental padding change.
func TestImageDirNameSortabilityPast999(t *testing.T) {
	lo, err := ImageDirName("S", 1000)
	if err != nil {
		t.Fatalf("ImageDirName(S, 1000): %v", err)
	}
	hi, err := ImageDirName("S", 999)
	if err != nil {
		t.Fatalf("ImageDirName(S, 999): %v", err)
	}
	if lo >= hi {
		t.Fatalf("precondition: %q should be bytewise below %q", lo, hi)
	}
	// Numeric order (999 < 1000) diverges from bytewise order here:
	// any consumer sorting these names as plain strings is WRONG past
	// 999.
	if strings.Compare(hi, lo) < 0 {
		t.Errorf("bytewise %q < %q - contradicts image numbering; padding scheme changed?", hi, lo)
	}
	// Within the padded range the bytewise and numeric orders agree
	// (this is what the zero-padding buys up to 999).
	a, _ := ImageDirName("S", 98)
	b, _ := ImageDirName("S", 999)
	if a >= b {
		t.Errorf("padded range not bytewise sortable: %q >= %q", a, b)
	}
}

// TestManifestNilInputsRenderEmptyArrays: the artifact's SHAPE never
// changes with the data - nil and empty input sections both render as
// empty arrays, never as the JSON null literal (encoding/json renders
// a nil slice as null; the guarantee is GenerateManifest's, which
// always hands MarshalManifest non-nil slices). The minimal golden
// pins the non-entry sections with nil input; this pins the Entries
// section too and the structural (re-parsed) view.
func TestManifestNilInputsRenderEmptyArrays(t *testing.T) {
	in := ManifestInput{
		SetID:         "S",
		ImageNumber:   1,
		ToolVersion:   "0.1.0-dev",
		SchemaVersion: 1,
		CreatedAt:     fixedCreatedAt,
		// Everything payload-shaped is nil; placement carries the
		// image itself with an empty path list.
		Placement: []PlacementInput{{ImageNumber: 1, Paths: []string{}}},
	}
	m, err := GenerateManifest(in)
	if err != nil {
		t.Fatalf("GenerateManifest: %v", err)
	}
	raw := string(mustMarshal(t, m))
	for _, want := range []string{
		`"entries": []`, `"chunked_files": []`, `"hard_link_groups": []`, `"paths": []`,
	} {
		if !strings.Contains(raw, want) {
			t.Errorf("raw manifest lacks %s (nil must render as [], never null):\n%s", want, raw)
		}
	}
	if strings.Contains(raw, "null") {
		t.Errorf("manifest contains a null literal; the shape is arrays-only:\n%s", raw)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	for _, k := range []string{"entries", "chunked_files", "hard_link_groups", "placement"} {
		arr, ok := doc[k].([]any)
		if !ok {
			t.Errorf("re-parsed %s = %T, want array", k, doc[k])
			continue
		}
		if k == "entries" && len(arr) != 0 {
			t.Errorf("entries has %d elements, want 0", len(arr))
		}
	}

	// DOCUMENTED BOUNDARY: MarshalManifest is a pure renderer - it
	// does NOT normalize its input. A hand-built Manifest with nil
	// slices renders the JSON null literal (encoding/json's nil-slice
	// rule), which is exactly why the empty-array shape guarantee
	// lives in GenerateManifest and callers must not assemble
	// Manifest values by hand for writing.
	hand := Manifest{SetID: "S", ImageNumber: 1} // nil slices throughout
	raw2 := string(mustMarshal(t, hand))
	if !strings.Contains(raw2, `"entries": null`) {
		t.Errorf("hand-built Manifest rendering changed; MarshalManifest no longer pure?\n%s", raw2)
	}
}

// TestManifestPayloadNamedLikeRootMembers: the disjoint-roots claim of
// 5.3, asserted at MANIFEST level. A payload file literally named
// MANIFEST.json or catalog-export.sqlite lives at files/<name> and can
// never shadow the image-root members; a file named "chunks" and the
// chunked parent named "chunks" coexist in ONE image's manifest with
// distinct media paths (files/chunks vs chunks/chunks/00001).
func TestManifestPayloadNamedLikeRootMembers(t *testing.T) {
	in := ManifestInput{
		SetID:         "S",
		ImageNumber:   1,
		ToolVersion:   "0.1.0-dev",
		SchemaVersion: 1,
		CreatedAt:     fixedCreatedAt,
		Entries: []ImageEntry{
			{Kind: KindFile, RelPath: ManifestFileName, Size: 1, SHA256: hashBytes(0x01),
				Mode: 0644, UID: 1000, GID: 1000, MtimeNS: 1},
			{Kind: KindFile, RelPath: ExportFileName, Size: 2, SHA256: hashBytes(0x02),
				Mode: 0644, UID: 1000, GID: 1000, MtimeNS: 2},
			{Kind: KindFile, RelPath: "chunks", Size: 3, SHA256: hashBytes(0x03),
				Mode: 0644, UID: 1000, GID: 1000, MtimeNS: 3},
			// Chunked parent with the SAME name as the file above -
			// same image, distinct media paths, no duplicate refusal.
			{Kind: KindChunk, RelPath: "chunks", ChunkIndex: 0, Size: 4, SHA256: hashBytes(0x04)},
		},
		ChunkedFiles: []ChunkedFileInput{
			{RelPath: "chunks", WholeSize: 4, WholeSHA256: hashBytes(0x05)},
		},
		Placement: []PlacementInput{
			{ImageNumber: 1, Paths: []string{ManifestFileName, ExportFileName, "chunks"}},
		},
	}
	m, err := GenerateManifest(in)
	if err != nil {
		t.Fatalf("GenerateManifest: %v", err)
	}
	wantPaths := map[string]bool{
		"files/" + ManifestFileName: true,
		"files/" + ExportFileName:   true,
		"files/chunks":              true,
		"chunks/chunks/00001":       true,
	}
	seen := map[string]bool{}
	for _, e := range m.Entries {
		if !wantPaths[e.MediaPath] {
			t.Errorf("unexpected media path %q", e.MediaPath)
			continue
		}
		if seen[e.MediaPath] {
			t.Errorf("media path %q emitted twice", e.MediaPath)
		}
		seen[e.MediaPath] = true
	}
	for path := range wantPaths {
		if !seen[path] {
			t.Errorf("media path %q missing from entries", path)
		}
	}
	// THE shadowing property: no entry's media path IS a root member
	// name - payload can live at files/MANIFEST.json but never at the
	// bare root name the writers reserve (layout.go constants).
	for _, e := range m.Entries {
		if e.MediaPath == ManifestFileName || e.MediaPath == ExportFileName {
			t.Errorf("entry media path %q collides with an image-root member", e.MediaPath)
		}
	}
	// The chunked parent named "chunks" got its reassembly table, and
	// its derived media name is on the entry.
	if len(m.ChunkedFiles) != 1 || m.ChunkedFiles[0].RelPath != "chunks" {
		t.Fatalf("chunked_files = %+v, want the parent named %q", m.ChunkedFiles, "chunks")
	}
}

// TestManifestChunkIndexBoundaryViaManifest runs the 5-digit media-name
// scheme through the full manifest pipeline: the largest index
// (99998 -> media 99999) is accepted, one more is refused (the name
// would widen to 6 digits), and - the omitempty hazard - catalog index
// 0 SURVIVES marshaling because ChunkIndex is a pointer (a plain
// int64 plus omitempty would silently drop the first chunk of every
// file).
func TestManifestChunkIndexBoundaryViaManifest(t *testing.T) {
	const parent = "big/archive.bin"
	base := func(index int64) ManifestInput {
		return ManifestInput{
			SetID: "S", ImageNumber: 1, ToolVersion: "0.1.0-dev", SchemaVersion: 1,
			CreatedAt: fixedCreatedAt,
			Entries: []ImageEntry{
				{Kind: KindChunk, RelPath: parent, ChunkIndex: index, Size: 4096, SHA256: hashBytes(0x21)},
			},
			ChunkedFiles: []ChunkedFileInput{
				{RelPath: parent, WholeSize: 4096, WholeSHA256: hashBytes(0x22)},
			},
			Placement: []PlacementInput{{ImageNumber: 1, Paths: []string{parent}}},
		}
	}

	// Largest index of the 5-digit 1-based scheme: accepted, media
	// name 99999.
	m, err := GenerateManifest(base(maxChunkNameIndex))
	if err != nil {
		t.Fatalf("GenerateManifest(index %d): %v", maxChunkNameIndex, err)
	}
	if got := m.Entries[0].MediaPath; got != "chunks/"+parent+"/99999" {
		t.Errorf("media path = %q, want chunks/%s/99999", got, parent)
	}
	// One more: refused, naming the scheme.
	if _, err := GenerateManifest(base(maxChunkNameIndex + 1)); err == nil ||
		!strings.Contains(err.Error(), "5-digit") {
		t.Errorf("index %d: err = %v, want 5-digit-scheme refusal", maxChunkNameIndex+1, err)
	}

	// Index 0 omitempty survival: raw bytes carry the chunk_index
	// key with value 0, and a structural re-parse SEES the key (not
	// absent).
	m0, err := GenerateManifest(base(0))
	if err != nil {
		t.Fatalf("GenerateManifest(index 0): %v", err)
	}
	raw := string(mustMarshal(t, m0))
	if !strings.Contains(raw, `"chunk_index": 0`) {
		t.Errorf("chunk_index 0 dropped by omitempty:\n%s", raw)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	ents, ok := doc["entries"].([]any)
	if !ok || len(ents) != 1 {
		t.Fatalf("entries = %v, want one chunk entry", doc["entries"])
	}
	em, ok := ents[0].(map[string]any)
	if !ok {
		t.Fatalf("entries[0] = %T, want object", ents[0])
	}
	v, present := em["chunk_index"]
	if !present {
		t.Fatalf("chunk_index key absent after re-parse (omitempty hazard realized):\n%s", raw)
	}
	if v.(float64) != 0 {
		t.Errorf("chunk_index = %v, want 0", v)
	}
	// And the typed round-trip keeps a NON-NIL pointer to 0.
	var back Manifest
	if err := json.Unmarshal([]byte(raw), &back); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if back.Entries[0].ChunkIndex == nil || *back.Entries[0].ChunkIndex != 0 {
		t.Errorf("chunk_index pointer = %v, want non-nil pointer to 0", back.Entries[0].ChunkIndex)
	}
	// Full typed round-trip of the comprehensive golden fixture:
	// marshal then unmarshal reproduces the exact generated value,
	// pointers included.
	full, err := GenerateManifest(comprehensiveInput())
	if err != nil {
		t.Fatalf("GenerateManifest(comprehensive): %v", err)
	}
	var fullBack Manifest
	if err := json.Unmarshal(mustMarshal(t, full), &fullBack); err != nil {
		t.Fatalf("re-parse comprehensive: %v", err)
	}
	if !reflect.DeepEqual(fullBack, full) {
		t.Errorf("comprehensive round-trip mismatch\n--- generated ---\n%#v\n--- re-parsed ---\n%#v",
			full, fullBack)
	}
}

// TestMarshalManifestTotality answers the audit question "what input
// makes MarshalManifest fail?": NOTHING within this package's types.
// Manifest is a closed shape of strings, integers, and structs - no
// interfaces, no floats (no NaN/Inf), no Marshalers, no cycles - so
// encoding/json cannot reject it. In particular invalid UTF-8 does NOT
// error: encoding/json SANITIZES it, replacing each invalid byte with
// U+FFFD. The error return exists as defensive armor for future format
// evolution, and this test pins its totality so a future change that
// makes it partial is a deliberate, reviewed act.
//
// The sanitization is also why raw-byte (invalid UTF-8) relpaths cannot
// be handled in the renderer at all: the catalog walker deliberately
// preserves such names (internal/ingest hardening test), the media TREE
// stores their exact bytes, but JSON fundamentally cannot carry them -
// a manifest entry for a name containing a raw 0xff byte would silently
// render with U+FFFD and lie to a future restorer. That finding is now
// FIXED upstream in this package (the UTF-8 invariant, manifest.go file
// doc): ValidateRelPath refuses raw-byte relpaths, buildEntries refuses
// raw-byte symlink targets, and validateSetID refuses raw-byte set ids,
// all with the ErrUnsafeRelPath sentinel. This test therefore pins BOTH
// halves: the renderer below stays total-and-sanitizing for hand-built
// Manifest values (its documented contract), while GenerateManifest
// refuses the very values the renderer would corrupt.
func TestMarshalManifestTotality(t *testing.T) {
	extreme := Manifest{
		SetID:         "S",
		ImageNumber:   math.MaxInt64,
		ToolVersion:   "0.1.0-dev",
		SchemaVersion: math.MaxInt32,
		CreatedAt:     "2026-10-19T12:00:00Z",
		Entries: []Entry{{
			MediaPath:  "files/a" + string([]byte{0xff}) + "b.txt", // invalid UTF-8: sanitized, not an error
			Kind:       KindFile,
			Size:       math.MaxInt64,
			SHA256:     hashHex(0x01),
			MtimeNS:    math.MinInt64,
			Mode:       math.MaxUint32,
			UID:        math.MaxUint32,
			GID:        math.MaxUint32,
			ChunkIndex: func() *int64 { z := int64(0); return &z }(),
		}},
		ChunkedFiles: []ChunkedFile{{
			RelPath: "p", Size: math.MaxInt64, SHA256: hashHex(0x02),
			Chunks: []ChunkPart{{Index: 0, Length: math.MaxInt64, SHA256: hashHex(0x03)}},
		}},
		HardLinkGroups: []HardLinkGroup{{SHA256: hashHex(0x04), Names: []string{"a", "b"}}},
		Placement:      []ImagePlacement{{ImageNumber: math.MaxInt64, Paths: []string{"a"}}},
	}
	b, err := MarshalManifest(extreme)
	if err != nil {
		t.Fatalf("MarshalManifest returned %v; the error path is documented defensive-only - "+
			"making it reachable for these types is a format change requiring review", err)
	}
	// The invalid byte was REPLACED by U+FFFD, not preserved and not
	// errored on - documenting the sanitization the upstream UTF-8
	// gates exist to close (a hand-built Manifest bypasses
	// GenerateManifest; callers must not assemble Manifest values by
	// hand for writing - see also TestManifestNilInputsRenderEmptyArrays).
	if !strings.Contains(string(b), "a"+escFFFD+"b.txt") {
		t.Errorf("expected U+FFFD sanitization of the invalid byte:\n%s", b)
	}
	if bytes.Contains(b, []byte{0xff}) {
		t.Errorf("raw 0xff byte leaked into the JSON (invalid UTF-8 in output):\n%s", b)
	}
	// Max/min integers round-trip verbatim: no float64 precision path.
	if !strings.Contains(string(b), `"size": 9223372036854775807`) ||
		!strings.Contains(string(b), `"mtime_ns": -9223372036854775808`) {
		t.Errorf("extreme integers not rendered verbatim:\n%s", b)
	}

	// THE UPSTREAM GATE (the fix this sanitization documentation points
	// at): GenerateManifest refuses exactly the values the renderer
	// would silently corrupt above - a raw-byte relpath, a raw-byte
	// symlink target, and a raw-byte set id - with the ErrUnsafeRelPath
	// sentinel and a "not valid UTF-8" diagnostic. The renderer stays
	// total; record fidelity is enforced where the record is built.
	rawByte := string([]byte{0xff})
	for _, c := range []struct {
		name    string
		mutate  func(*ManifestInput)
		wantSub string
	}{
		{"relpath", func(in *ManifestInput) { in.Entries[2].RelPath = "a" + rawByte + "b.txt" }, "not valid UTF-8"},
		{"symlink target", func(in *ManifestInput) {
			in.Entries[1].LinkTarget = "t" + rawByte + "arget"
			in.Entries[1].Size = int64(len("t" + rawByte + "arget"))
		}, "link target that is not valid UTF-8"},
		{"set id", func(in *ManifestInput) { in.SetID = "S" + rawByte }, "set id"},
	} {
		in := comprehensiveInput()
		c.mutate(&in)
		_, err := GenerateManifest(in)
		if !errors.Is(err, ErrUnsafeRelPath) {
			t.Errorf("raw-byte %s: err = %v, want ErrUnsafeRelPath (the upstream gate)", c.name, err)
			continue
		}
		if !strings.Contains(err.Error(), c.wantSub) {
			t.Errorf("raw-byte %s: err = %v, want it to contain %q", c.name, err, c.wantSub)
		}
	}
}
