package restore

// T5.5 acceptance at the ENGINE seam: ResolveMedia (8.5's quick
// manifest-introspection layer) over both PATH shapes - the ROOT MODE
// single tree (an extracted ISO tree or one S-NNN copy directory) and
// the SIBLING MODE directory of S-NNN trees - plus the failure classes,
// the introspection-without-export contract (the manifest must answer
// even when the export is absent or unreadable), and the supply
// precedence the media base introduces (media first, then the 8.3
// order - supply.go's documented reading).

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"isopress/internal/media"
)

// writeTestManifest writes a minimal valid MANIFEST.json for one image
// into dir and returns the parsed value: one whole-file payload entry
// (file-<image>.txt) placed on this image, plus a placement record per
// extra image so the whole-set summary spans them (FR-033). The export
// file is deliberately NOT written here - each test plants (or refuses
// to plant) its own, because the export's presence is not ResolveMedia's
// concern.
func writeTestManifest(t *testing.T, dir, setID string, image int64, placementImages ...int64) media.Manifest {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("creating the media dir %s: %v", dir, err)
	}
	rel := "file-" + itoa(image) + ".txt"
	hash := make([]byte, 32)
	for i := range hash {
		hash[i] = byte(image) + byte(i)
	}
	images := append([]int64{image}, placementImages...)
	in := media.ManifestInput{
		SetID:         setID,
		ImageNumber:   image,
		ToolVersion:   "test",
		SchemaVersion: 1,
		CreatedAt:     time.Unix(1_700_000_000, 0),
		Entries: []media.ImageEntry{{
			Kind: media.KindFile, RelPath: rel, Size: 4, SHA256: hash,
		}},
	}
	for _, p := range images {
		in.Placement = append(in.Placement, media.PlacementInput{
			ImageNumber: p,
			Paths:       []string{"file-" + itoa(p) + ".txt"},
		})
	}
	m, err := media.GenerateManifest(in)
	if err != nil {
		t.Fatalf("generating the test manifest: %v", err)
	}
	blob, err := media.MarshalManifest(m)
	if err != nil {
		t.Fatalf("marshaling the test manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, media.ManifestFileName), blob, 0o600); err != nil {
		t.Fatalf("writing the test manifest: %v", err)
	}
	return m
}

// --- ROOT MODE (PATH itself holds MANIFEST.json) -----------------------------

func TestResolveMediaRootModeSingleTree(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "S-001")
	want := writeTestManifest(t, dir, "S-RM", 1)
	export := filepath.Join(dir, media.ExportFileName)
	if err := os.WriteFile(export, []byte("export bytes"), 0o600); err != nil {
		t.Fatalf("planting the export: %v", err)
	}

	info, err := ResolveMedia(dir)
	if err != nil {
		t.Fatalf("ResolveMedia: %v", err)
	}
	if info.SetID != "S-RM" || info.Manifest.ImageNumber != 1 {
		t.Errorf("introspection = set %q image %d, want S-RM/1", info.SetID, info.Manifest.ImageNumber)
	}
	if info.ManifestDir != dir || info.ExportPath != export {
		t.Errorf("locations = (%q, %q), want (%q, %q)", info.ManifestDir, info.ExportPath, dir, export)
	}
	if got := info.Trees[1]; got != dir {
		t.Errorf("tree of image 1 = %q, want the PATH itself %q", got, dir)
	}
	if nums := info.ImageNumbers(); len(nums) != 1 || nums[0] != 1 {
		t.Errorf("ImageNumbers = %v, want [1]", nums)
	}
	// The introspection answers WITHOUT the export being consulted: the
	// image list and the whole-set placement record (8.5's quick layer).
	if len(info.Manifest.Placement) != len(want.Placement) {
		t.Errorf("placement records = %d, want %d (the whole-set summary)", len(info.Manifest.Placement), len(want.Placement))
	}
	if len(info.Manifest.Entries) != 1 || info.Manifest.Entries[0].Kind != media.KindFile {
		t.Errorf("entries = %+v, want this image's one file entry", info.Manifest.Entries)
	}
}

// The quick layer must answer even when the export is ABSENT or
// GARBAGE: the manifest is the introspection layer; a broken export
// then fails loudly when the command flow opens it (the
// unreadable-export acceptance, asserted at this seam).
func TestResolveMediaIntrospectionWorksWithoutReadableExport(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "S-001")
	writeTestManifest(t, dir, "S-NOEXP", 1)

	// No export at all: introspection still answers.
	info, err := ResolveMedia(dir)
	if err != nil {
		t.Fatalf("ResolveMedia without an export: %v", err)
	}
	if info.SetID != "S-NOEXP" || info.ExportPath != filepath.Join(dir, media.ExportFileName) {
		t.Errorf("introspection = (%q, %q), want S-NOEXP and the expected export path", info.SetID, info.ExportPath)
	}

	// A garbage export: introspection still answers (the export's
	// corruption surfaces at the OPEN, not here).
	if err := os.WriteFile(info.ExportPath, []byte("this is not a sqlite database"), 0o600); err != nil {
		t.Fatalf("corrupting the export: %v", err)
	}
	info2, err := ResolveMedia(dir)
	if err != nil {
		t.Fatalf("ResolveMedia with a garbage export: %v", err)
	}
	if info2.SetID != "S-NOEXP" {
		t.Errorf("set id = %q, want S-NOEXP (introspection must not touch the export)", info2.SetID)
	}
}

// --- SIBLING MODE (PATH holds S-NNN subdirectories) --------------------------

func TestResolveMediaSiblingModeTrees(t *testing.T) {
	root := t.TempDir()
	writeTestManifest(t, filepath.Join(root, "S-SIB-001"), "S-SIB", 1, 2)
	writeTestManifest(t, filepath.Join(root, "S-SIB-002"), "S-SIB", 2)
	// Skew that must be IGNORED, not refused: a subdirectory without a
	// manifest (a partially copied image defers MANIFEST.json) and a
	// plain file at the root.
	if err := os.MkdirAll(filepath.Join(root, "S-SIB-003"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "stray.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	info, err := ResolveMedia(root)
	if err != nil {
		t.Fatalf("ResolveMedia: %v", err)
	}
	if info.SetID != "S-SIB" {
		t.Errorf("set id = %q, want S-SIB", info.SetID)
	}
	if got := info.ImageNumbers(); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("ImageNumbers = %v, want [1 2] (the manifest-less dir adds no tree)", got)
	}
	if info.Trees[1] != filepath.Join(root, "S-SIB-001") || info.Trees[2] != filepath.Join(root, "S-SIB-002") {
		t.Errorf("trees = %v, want the two manifest-bearing subdirectories", info.Trees)
	}
	// The LEAD (lowest-numbered) image's manifest introspects, and its
	// directory carries the export location.
	if info.Manifest.ImageNumber != 1 {
		t.Errorf("lead manifest image = %d, want 1 (lowest number leads)", info.Manifest.ImageNumber)
	}
	if info.ManifestDir != filepath.Join(root, "S-SIB-001") {
		t.Errorf("manifest dir = %q, want the lead image's directory", info.ManifestDir)
	}
	if info.ExportPath != filepath.Join(root, "S-SIB-001", media.ExportFileName) {
		t.Errorf("export path = %q, want the lead image's export", info.ExportPath)
	}
}

// --- the failure classes ------------------------------------------------------

func TestResolveMediaPathGates(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		path string
		want string
	}{
		{"missing path", filepath.Join(t.TempDir(), "absent"), "cannot be read"},
		{"not a directory", file, "is not a directory"},
		{"no manifest anywhere", t.TempDir(), media.ManifestFileName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveMedia(tc.path)
			var input *InputError
			if !errors.As(err, &input) {
				t.Fatalf("error = %v (%T), want *InputError", err, err)
			}
			if !strings.Contains(input.Error(), tc.want) {
				t.Errorf("error %q lacks %q", input.Error(), tc.want)
			}
		})
	}
}

func TestResolveMediaCorruptManifestIsLoudIntegrityFailure(t *testing.T) {
	// ROOT MODE: a manifest that does not parse must NOT be skipped.
	dir := filepath.Join(t.TempDir(), "S-001")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, media.ManifestFileName), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ResolveMedia(dir)
	var integrity *MediaIntegrityError
	if !errors.As(err, &integrity) {
		t.Fatalf("root-mode error = %v (%T), want *MediaIntegrityError", err, err)
	}
	if !strings.Contains(integrity.Error(), media.ManifestFileName) {
		t.Errorf("the integrity failure must NAME the manifest: %q", integrity.Error())
	}

	// SIBLING MODE: one broken subdirectory manifest poisons the whole
	// media - a broken anchor where it EXISTS is never a silent skip.
	root := t.TempDir()
	writeTestManifest(t, filepath.Join(root, "S-001"), "S-COR", 1)
	if err := os.MkdirAll(filepath.Join(root, "S-002"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "S-002", media.ManifestFileName), []byte("[wrong shape"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = ResolveMedia(root)
	if !errors.As(err, &integrity) {
		t.Fatalf("sibling-mode error = %v (%T), want *MediaIntegrityError", err, err)
	}
	if !strings.Contains(integrity.Error(), filepath.Join(root, "S-002", media.ManifestFileName)) {
		t.Errorf("the integrity failure must name the broken file: %q", integrity.Error())
	}
}

func TestResolveMediaMixedSetsAndDuplicateImages(t *testing.T) {
	root := t.TempDir()
	writeTestManifest(t, filepath.Join(root, "A-001"), "S-A", 1)
	writeTestManifest(t, filepath.Join(root, "B-001"), "S-B", 1)
	_, err := ResolveMedia(root)
	var input *InputError
	if !errors.As(err, &input) {
		t.Fatalf("mixed-sets error = %v (%T), want *InputError", err, err)
	}
	if !strings.Contains(input.Error(), "S-A") || !strings.Contains(input.Error(), "S-B") {
		t.Errorf("the mixed-sets refusal must name BOTH ids: %q", input.Error())
	}

	root2 := t.TempDir()
	writeTestManifest(t, filepath.Join(root2, "X-001"), "S-DUP", 1)
	writeTestManifest(t, filepath.Join(root2, "Y-001"), "S-DUP", 1)
	_, err = ResolveMedia(root2)
	if !errors.As(err, &input) {
		t.Fatalf("duplicate-image error = %v (%T), want *InputError", err, err)
	}
	if !strings.Contains(input.Error(), "image 1") {
		t.Errorf("the duplicate refusal must name the image number: %q", input.Error())
	}
}

// --- the supply precedence (media base first, then 8.3) -----------------------

// TestResolveSupplyMediaBaseFirst pins the documented 8.5 precedence:
// the media-derived tree wins for its image even when the 8.3 sources
// ALSO have an offering, a media miss (image absent from the map, or a
// tree path that vanished) falls through to the 8.3 order untouched,
// and a fully unsupplied image lists the media candidate in Sought.
func TestResolveSupplyMediaBaseFirst(t *testing.T) {
	setID := "S-SUP"
	root := t.TempDir()
	mediaTree := filepath.Join(root, "media", setID+"-001")
	if err := os.MkdirAll(mediaTree, 0o700); err != nil {
		t.Fatal(err)
	}
	isoDir := filepath.Join(root, "iso")
	if err := os.MkdirAll(isoDir, 0o700); err != nil {
		t.Fatal(err)
	}
	iso := filepath.Join(isoDir, setID+"-001.iso")
	if err := os.WriteFile(iso, []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}
	explicitTree := filepath.Join(root, "explicit")
	if err := os.MkdirAll(explicitTree, 0o700); err != nil {
		t.Fatal(err)
	}
	copyRoot := filepath.Join(root, "copyroot")
	if err := os.MkdirAll(filepath.Join(copyRoot, setID+"-002"), 0o700); err != nil {
		t.Fatal(err)
	}

	spec := SupplySpec{
		ImageDir:     isoDir,
		CopyTreeRoot: copyRoot,
		Explicit:     []ExplicitImage{{Image: 2, Path: explicitTree}},
		Media:        map[int64]string{1: mediaTree},
	}
	out, err := ResolveSupply(t.Context(), setID, []int64{1, 2}, spec)
	if err != nil {
		t.Fatalf("ResolveSupply: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("resolved = %d images, want 2", len(out))
	}
	// Image 1: the media tree beats --image-dir's .iso (media is the
	// base, consulted FIRST).
	if out[0].Kind != SourceCopyTree || out[0].Path != mediaTree {
		t.Errorf("image 1 = %+v, want the media tree %q", out[0], mediaTree)
	}
	// Image 2: no media entry - the 8.3 order applies untouched
	// (explicit mapping before copy-tree-root here; no .iso present).
	if out[1].Kind != SourceCopyTree || out[1].Path != explicitTree {
		t.Errorf("image 2 = %+v, want the explicit tree %q", out[1], explicitTree)
	}

	// A media tree that vanished falls through (discovery-class miss),
	// and an image nothing else supplies is Missing with the MEDIA
	// candidate leading its Sought list and the 8.3 sources completing
	// it (here --image-dir's .iso name).
	spec = SupplySpec{
		ImageDir: isoDir,
		Media:    map[int64]string{2: filepath.Join(root, "also-gone")},
	}
	out, err = ResolveSupply(t.Context(), setID, []int64{2}, spec)
	if err != nil {
		t.Fatalf("ResolveSupply (miss): %v", err)
	}
	if !out[0].Missing {
		t.Fatalf("image 2 = %+v, want missing", out[0])
	}
	if len(out[0].Sought) == 0 || out[0].Sought[0] != filepath.Join(root, "also-gone") {
		t.Errorf("Sought = %v, want the media candidate first", out[0].Sought)
	}
	last := out[0].Sought[len(out[0].Sought)-1]
	if last != filepath.Join(isoDir, setID+"-002.iso") {
		t.Errorf("last Sought = %q, want the --image-dir candidate %q (the 8.3 order completed the list)", last,
			filepath.Join(isoDir, setID+"-002.iso"))
	}
}
