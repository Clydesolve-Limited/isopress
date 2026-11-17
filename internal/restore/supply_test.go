package restore

// T5.3 acceptance: image supply (DESIGN_SPEC 8.3 preference order, 6.4
// deterministic naming; IMPLEMENTATION_PLAN 4.6 T5.3 "image supply in order
// of preference --image-dir ... explicit --image N=PATH ... --copy-tree-root").

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"isopress/internal/media"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}

const supplySet = "set-20261021"

func resolveSupply(t *testing.T, required []int64, spec SupplySpec) []ResolvedSupply {
	t.Helper()
	out, err := ResolveSupply(context.Background(), supplySet, required, spec)
	if err != nil {
		t.Fatalf("ResolveSupply: %v", err)
	}
	return out
}

// The literal 8.3 order: when several sources can supply one image, the
// --image-dir candidate wins over the explicit mapping, which wins over the
// copy-tree root.
func TestSupplyPreferenceOrderIsImageDirThenExplicitThenCopyTree(t *testing.T) {
	root := t.TempDir()
	imageDir := filepath.Join(root, "isos")
	copyRoot := filepath.Join(root, "trees")
	explicitDir := filepath.Join(root, "explicit")

	isoName, err := media.ImageISOFileName(supplySet, 1)
	if err != nil {
		t.Fatal(err)
	}
	dirName, err := media.ImageDirName(supplySet, 1)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(imageDir, isoName), "iso")
	explicitPath := filepath.Join(explicitDir, "chosen.iso")
	writeFile(t, explicitPath, "iso")
	mkdir(t, filepath.Join(copyRoot, dirName))

	spec := SupplySpec{
		ImageDir:     imageDir,
		CopyTreeRoot: copyRoot,
		Explicit:     []ExplicitImage{{Image: 1, Path: explicitPath}},
	}
	got := resolveSupply(t, []int64{1}, spec)[0]
	if got.Missing {
		t.Fatal("image 1 must be supplied")
	}
	if got.Kind != SourceISO {
		t.Errorf("kind = %q, want iso", got.Kind)
	}
	if got.Path != filepath.Join(imageDir, isoName) {
		t.Errorf("path = %q, want the --image-dir candidate %q (8.3 order)", got.Path, filepath.Join(imageDir, isoName))
	}
	// Sought stops at the source that answered (the candidates consulted
	// before it are what the listing would have shown).
	if len(got.Sought) != 1 || got.Sought[0] != filepath.Join(imageDir, isoName) {
		t.Errorf("sought = %v, want only the winning candidate", got.Sought)
	}

	// Remove the image-dir candidate: the EXPLICIT mapping wins next.
	if err := os.Remove(filepath.Join(imageDir, isoName)); err != nil {
		t.Fatal(err)
	}
	got = resolveSupply(t, []int64{1}, spec)[0]
	if got.Path != explicitPath {
		t.Errorf("after the image-dir miss, path = %q, want the explicit %q", got.Path, explicitPath)
	}
	if len(got.Sought) != 2 {
		t.Errorf("sought = %v, want both consulted candidates", got.Sought)
	}

	// Remove the explicit path too: the run REFUSES rather than silently
	// falling through to the copy-tree root - a broken explicit mapping is
	// the user's input error, and masking it with a different source would
	// quietly restore from somewhere they did not ask for.
	if err := os.Remove(explicitPath); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveSupply(context.Background(), supplySet, []int64{1}, spec); err == nil {
		t.Error("a broken --image path must fail the run even when a later source could answer")
	} else {
		var input *InputError
		if !errors.As(err, &input) {
			t.Errorf("err = %T, want *InputError", err)
		}
	}

	// Drop the broken mapping entirely: the copy-tree root answers last.
	spec.Explicit = nil
	got = resolveSupply(t, []int64{1}, spec)[0]
	if got.Kind != SourceCopyTree || got.Path != filepath.Join(copyRoot, dirName) {
		t.Errorf("supply = %q %q, want the copy-tree candidate", got.Kind, got.Path)
	}
}

// --image-dir discovers <SET-ID>-NNN.iso per 6.4 and supplies FILES only;
// --copy-tree-root supplies <SET-ID>-NNN/ TREES only.
func TestSupplyDiscoveryShapesAreDistinct(t *testing.T) {
	root := t.TempDir()
	imageDir := filepath.Join(root, "isos")
	copyRoot := filepath.Join(root, "trees")

	iso2, _ := media.ImageISOFileName(supplySet, 2)
	writeFile(t, filepath.Join(imageDir, iso2), "iso")
	// The tree for image 3 exists only under the copy-tree root.
	tree3 := filepath.Join(copyRoot, mustDirName(t, 3))
	mkdir(t, tree3)

	spec := SupplySpec{ImageDir: imageDir}
	got := resolveSupply(t, []int64{2, 3}, spec)
	if got[0].Missing || got[0].Kind != SourceISO || got[0].Path != filepath.Join(imageDir, iso2) {
		t.Errorf("image 2 = %+v, want the discovered .iso", got[0])
	}
	if !got[1].Missing {
		t.Errorf("image 3 must MISS under --image-dir (a tree is not an .iso): %+v", got[1])
	}
	if !strings.Contains(got[1].Reason, "not found") {
		t.Errorf("image 3 reason = %q, want the not-found explanation", got[1].Reason)
	}

	// With only the copy-tree root: image 3 answers as a TREE, and image 2
	// misses (no tree exists for it - discovery is per image number).
	got = resolveSupply(t, []int64{2, 3}, SupplySpec{CopyTreeRoot: copyRoot})
	if !got[0].Missing {
		t.Errorf("image 2 must miss without a tree for it: %+v", got[0])
	}
	if got[1].Missing || got[1].Kind != SourceCopyTree {
		t.Errorf("image 3 = %+v, want the copy tree", got[1])
	}
}

// A discovered entry that exists but has the WRONG shape falls through with a
// recorded reason instead of being handed to the extraction engine.
func TestSupplyDiscoveredWrongShapeFallsThrough(t *testing.T) {
	root := t.TempDir()
	imageDir := filepath.Join(root, "isos")
	iso1, _ := media.ImageISOFileName(supplySet, 1)
	mkdir(t, filepath.Join(imageDir, iso1)) // a DIRECTORY named like the .iso

	got := resolveSupply(t, []int64{1}, SupplySpec{ImageDir: imageDir})[0]
	if !got.Missing {
		t.Fatalf("image 1 = %+v, want missing (a directory is not an .iso)", got)
	}
	if !strings.Contains(got.Reason, "not a regular file") {
		t.Errorf("reason = %q, want the wrong-shape explanation", got.Reason)
	}
}

// --image N=PATH is classified by stat: a regular file is an ISO, a directory
// is a copy tree.
func TestSupplyExplicitStatDecidesKind(t *testing.T) {
	root := t.TempDir()
	isoPath := filepath.Join(root, "one.iso")
	writeFile(t, isoPath, "iso")
	treePath := filepath.Join(root, "S-002")
	mkdir(t, treePath)

	spec := SupplySpec{Explicit: []ExplicitImage{
		{Image: 1, Path: isoPath},
		{Image: 2, Path: treePath},
	}}
	got := resolveSupply(t, []int64{1, 2}, spec)
	if got[0].Kind != SourceISO || got[0].Path != isoPath {
		t.Errorf("image 1 = %+v, want iso from the explicit file", got[0])
	}
	if got[1].Kind != SourceCopyTree || got[1].Path != treePath {
		t.Errorf("image 2 = %+v, want copy from the explicit directory", got[1])
	}
}

// An explicit mapping the user got wrong is an INPUT error (exit 2), never a
// media prompt: the tool must not ask for a path the user already named.
func TestSupplyExplicitBadPathIsInputError(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "nope.iso")
	_, err := ResolveSupply(context.Background(), supplySet, []int64{1},
		SupplySpec{Explicit: []ExplicitImage{{Image: 1, Path: missing}}})
	var input *InputError
	if !errors.As(err, &input) {
		t.Fatalf("err = %v, want *InputError", err)
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("message must name the path: %v", err)
	}

	// Neither shape: /dev/null is a character device on Linux.
	_, err = ResolveSupply(context.Background(), supplySet, []int64{1},
		SupplySpec{Explicit: []ExplicitImage{{Image: 1, Path: "/dev/null"}}})
	if !errors.As(err, &input) {
		t.Errorf("char-device err = %v, want *InputError", err)
	}
}

// No supply source at all: every required image is missing with an EMPTY
// sought list - the listing renders "sought=-" so cron can tell "nothing was
// configured" from "we looked and did not find".
func TestSupplyMissingWithoutAnySource(t *testing.T) {
	got := resolveSupply(t, []int64{1, 2}, SupplySpec{})[0]
	if !got.Missing {
		t.Fatalf("image 1 = %+v, want missing", got)
	}
	if len(got.Sought) != 0 {
		t.Errorf("sought = %v, want empty (no source configured)", got.Sought)
	}
}

func TestSupplyMissingSoughtListsEveryCandidate(t *testing.T) {
	root := t.TempDir()
	imageDir := filepath.Join(root, "isos")
	copyRoot := filepath.Join(root, "trees")
	explicit := filepath.Join(root, "still-here.iso")
	writeFile(t, explicit, "iso")

	spec := SupplySpec{
		ImageDir:     imageDir,
		CopyTreeRoot: copyRoot,
		Explicit:     []ExplicitImage{{Image: 5, Path: explicit}},
	}
	// Image 5 has an explicit mapping only; images 4 has discovery only.
	got := resolveSupply(t, []int64{4}, spec)[0]
	isoName, _ := media.ImageISOFileName(supplySet, 4)
	dirName, _ := media.ImageDirName(supplySet, 4)
	want := []string{
		filepath.Join(imageDir, isoName),
		filepath.Join(copyRoot, dirName),
	}
	if strings.Join(got.Sought, ",") != strings.Join(want, ",") {
		t.Errorf("sought = %v, want %v", got.Sought, want)
	}
	// The explicit mapping for another image never leaks into this one.
	for _, s := range got.Sought {
		if s == explicit {
			t.Error("image 4 must not consult image 5's explicit mapping")
		}
	}
}

// --- set-scoped and per-image supply edges -----------------------------------

// --image-dir discovery is SET-SCOPED: a different set's .iso sitting in the
// same directory (its own <SET-ID>-NNN.iso naming) is never picked up for
// this set, and each set resolves its own file.
func TestSupplyImageDirDiscoveryIsSetScoped(t *testing.T) {
	root := t.TempDir()
	imageDir := filepath.Join(root, "isos")
	const otherSet = "other-set-2026"
	otherISO, err := media.ImageISOFileName(otherSet, 1)
	if err != nil {
		t.Fatal(err)
	}
	iso1, err := media.ImageISOFileName(supplySet, 1)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(imageDir, otherISO), "the other set's iso")
	writeFile(t, filepath.Join(imageDir, iso1), "our iso")

	got := resolveSupply(t, []int64{1}, SupplySpec{ImageDir: imageDir})[0]
	if got.Path != filepath.Join(imageDir, iso1) {
		t.Errorf("path = %q, want OUR set's %q (discovery must be set-scoped)", got.Path, iso1)
	}
	other, err := ResolveSupply(context.Background(), otherSet, []int64{1}, SupplySpec{ImageDir: imageDir})
	if err != nil {
		t.Fatalf("ResolveSupply(%s): %v", otherSet, err)
	}
	if other[0].Path != filepath.Join(imageDir, otherISO) {
		t.Errorf("other set path = %q, want %q", other[0].Path, otherISO)
	}
}

// The 8.3 preference order is evaluated PER IMAGE, not per run: one run may
// take an ISO from --image-dir for one image AND a copy tree from
// --copy-tree-root for another, because each image's candidate list walks the
// order independently.
func TestSupplyMixedSourcesResolvePerImage(t *testing.T) {
	root := t.TempDir()
	imageDir := filepath.Join(root, "isos")
	copyRoot := filepath.Join(root, "trees")
	iso1, err := media.ImageISOFileName(supplySet, 1)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(imageDir, iso1), "iso") // image 1's ISO
	tree2 := filepath.Join(copyRoot, mustDirName(t, 2))
	mkdir(t, tree2) // image 2's tree (its image-dir candidate misses)

	got := resolveSupply(t, []int64{1, 2}, SupplySpec{ImageDir: imageDir, CopyTreeRoot: copyRoot})
	if got[0].Missing || got[0].Kind != SourceISO || got[0].Path != filepath.Join(imageDir, iso1) {
		t.Errorf("image 1 = %+v, want the --image-dir ISO", got[0])
	}
	if got[1].Missing || got[1].Kind != SourceCopyTree || got[1].Path != tree2 {
		t.Errorf("image 2 = %+v, want the --copy-tree-root tree (per-image, not per-run, preference)", got[1])
	}
}

// An explicit mapping for a NON-required image is never consulted (a broken
// one cannot fail a run that does not need that image), and duplicate
// mappings for the SAME image number are consulted in order - the first that
// classifies wins.
func TestSupplyExtraAndDuplicateExplicitMappings(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first.iso")
	second := filepath.Join(root, "second.iso")
	writeFile(t, first, "iso")
	writeFile(t, second, "iso")
	spec := SupplySpec{Explicit: []ExplicitImage{
		{Image: 1, Path: first},
		{Image: 1, Path: second}, // duplicate N: consulted after the first
		{Image: 9, Path: filepath.Join(root, "never-consulted.iso")}, // unrequired AND broken
	}}

	got := resolveSupply(t, []int64{1}, spec)[0]
	if got.Missing || got.Path != first {
		t.Errorf("image 1 = %+v, want the FIRST duplicate mapping", got)
	}
	if len(got.Sought) != 1 || got.Sought[0] != first {
		t.Errorf("sought = %v, want only the winning first candidate", got.Sought)
	}
}

// --- parsing and presentation ------------------------------------------------

func TestParseExplicitImage(t *testing.T) {
	good := map[string]ExplicitImage{
		"1=/mnt/a.iso": {Image: 1, Path: "/mnt/a.iso"},
		"42=/x/y":      {Image: 42, Path: "/x/y"},
		"3=/a=b/c":     {Image: 3, Path: "/a=b/c"}, // only the FIRST '=' splits
		"10=relative":  {Image: 10, Path: "relative"},
	}
	for in, want := range good {
		got, err := ParseExplicitImage(in)
		if err != nil {
			t.Errorf("ParseExplicitImage(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseExplicitImage(%q) = %+v, want %+v", in, got, want)
		}
	}
	for _, bad := range []string{"", "a.iso", "=/x", "1=", "0=/x", "-1=/x", "x=/y", "1.5=/y", "01x=/y"} {
		if _, err := ParseExplicitImage(bad); err == nil {
			t.Errorf("ParseExplicitImage(%q): want an input error", bad)
		} else {
			var input *InputError
			if !errors.As(err, &input) {
				t.Errorf("ParseExplicitImage(%q): err = %T, want *InputError", bad, err)
			}
		}
	}
}

// The non-TTY listing line is a frozen, machine-readable format (8.3 / RCR-3).
func TestMissingImageLineIsMachineReadable(t *testing.T) {
	line := MissingImageLine(MissingImage{
		SetID:     "s-1",
		Image:     2,
		Sought:    []string{"/iso/s-1-002.iso", "/trees/s-1-002"},
		NeededFor: "docs/report.pdf",
	})
	want := "required-image: image=2 set=s-1 sought=/iso/s-1-002.iso,/trees/s-1-002 needed-for=docs/report.pdf"
	if line != want {
		t.Errorf("line = %q\nwant  %q", line, want)
	}
	// No source configured: the "-" placeholders keep the key set stable so
	// a parser never has to handle a missing field.
	bare := MissingImageLine(MissingImage{SetID: "s-1", Image: 7})
	if bare != "required-image: image=7 set=s-1 sought=- needed-for=-" {
		t.Errorf("bare line = %q", bare)
	}
}

func mustDirName(t *testing.T, n int64) string {
	t.Helper()
	name, err := media.ImageDirName(supplySet, n)
	if err != nil {
		t.Fatal(err)
	}
	return name
}
