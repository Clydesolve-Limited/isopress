package restore

// T5.5: media-alone restore, the media side (DESIGN_SPEC 8.5;
// IMPLEMENTATION_PLAN 4.6 T5.5). ResolveMedia turns the --from-media
// PATH into the MediaInfo the command flow needs: the parsed manifest
// (the QUICK INTROSPECTION LAYER of 8.5 - set id, image number, this
// image's entries, and the whole-set placement answer "what images
// exist and what a selection would need" WITHOUT opening the export),
// the export's location on the media, and the per-image copy-tree
// sources the supply pass consults FIRST.
//
// THE TWO PATH SHAPES 8.5 NAMES (both are directories containing a
// MANIFEST.json; the export's presence is checked by the command flow,
// deliberately NOT here - introspection must answer even when the
// export is absent or unreadable, which is exactly the quick-layer
// contract):
//
//   - ROOT MODE: PATH itself holds MANIFEST.json and
//     catalog-export.sqlite at its root. That is an EXTRACTED ISO TREE
//     (the T4.3/T5.4 `-extract /` shape: files/, chunks/, MANIFEST.json,
//     catalog-export.sqlite at the root) or a SINGLE copy-set image
//     directory (S-NNN, the same logical tree - 5.3 is ONE layout in
//     both modes). The tree itself is that image's copy-tree source
//     (SourceCopyTree reads files/ and chunks/ off the tree root, so
//     the extracted tree IS the image's copy-tree equivalent), and the
//     manifest's image_number says WHICH image it is.
//   - SIBLING MODE: PATH holds S-NNN-style subdirectories whose roots
//     carry MANIFEST.json (the copy destination root, every disc of
//     the set copied side by side). Each manifest-bearing subdirectory
//     is that manifest's image's copy-tree source. Subdirectories
//     WITHOUT a manifest are skipped (a copy run defers MANIFEST.json
//     for an incomplete image - absence is normal; such a tree can
//     still be supplied explicitly through --image N=PATH), but a
//     subdirectory whose manifest exists and does not PARSE is a loud
//     media-integrity failure, never a silent skip: the anchor is
//     broken where it exists.
//
// All failure classes are documented on the errors themselves:
// *InputError for a PATH that is not a usable media directory (exit 2
// in the cli), *MediaIntegrityError for media content that cannot be
// trusted (exit 3 - the media-supply class: this media cannot back the
// run, supply other or repaired media).

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"isopress/internal/media"
)

// MediaInfo is one resolved --from-media PATH: everything the quick
// layer read from the manifests, plus the two filesystem locations the
// rest of the flow consumes.
type MediaInfo struct {
	// SetID and Manifest are the introspection manifest's values (the
	// lowest-numbered image's manifest in sibling mode - every image's
	// manifest carries the same set id and whole-set placement by
	// construction, 6.6/FR-033).
	SetID    string
	Manifest media.Manifest
	// Manifests carries EVERY parsed manifest keyed by its image number
	// (root mode: the one tree; sibling mode: every manifest-bearing
	// subdirectory). Each image's manifest holds THAT image's payload
	// entries and chunk tables (6.6), which is what lets the media
	// flow fill the run-start export's lazily-hashed chunk rows - see
	// the cli's mediaStore (restore_media.go) for the finding and the
	// fill.
	Manifests map[int64]media.Manifest
	// ManifestDir is the directory holding the introspection
	// MANIFEST.json; ExportPath is that directory's
	// catalog-export.sqlite (8.5: the two travel together).
	ManifestDir string
	ExportPath  string
	// Trees maps image number -> the copy-tree source directory on the
	// media (the PATH itself in root mode; each manifest-bearing
	// subdirectory in sibling mode). This is SupplySpec.Media - the
	// base supply consulted FIRST (see supply.go).
	Trees map[int64]string
}

// ImageNumbers returns the media's image numbers ascending (the
// whole-set placement record of 8.5's introspection: what images
// exist).
func (m MediaInfo) ImageNumbers() []int64 {
	out := make([]int64, 0, len(m.Trees))
	for id := range m.Trees {
		out = append(out, id)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// MediaIntegrityError classifies media content that cannot be trusted:
// a MANIFEST.json that exists but does not parse, and (in the command
// flow) a manifest whose set id disagrees with the export's. The cli
// maps it to the dependency/media-supply class, exit 3: the run cannot
// proceed with THIS media, and the remedy is other or repaired media -
// never a silent proceed on an anchor that failed.
type MediaIntegrityError struct {
	Path string // the media file the failure refers to ("" when unknown)
	Msg  string
}

func (e *MediaIntegrityError) Error() string {
	if e.Path != "" {
		return "restore: media integrity at " + e.Path + ": " + e.Msg
	}
	return "restore: media integrity: " + e.Msg
}

func mediaIntegrityErrorf(path, format string, args ...any) error {
	return &MediaIntegrityError{Path: path, Msg: fmt.Sprintf(format, args...)}
}

// ResolveMedia resolves the --from-media PATH (see the file doc for the
// two shapes). It performs NO export open and NO catalog work: the
// manifest is the quick layer, and it must answer - or fail loudly on
// its own terms - before the export is ever touched.
func ResolveMedia(path string) (MediaInfo, error) {
	fi, err := os.Stat(path)
	switch {
	case err != nil:
		return MediaInfo{}, inputErrorf("--from-media %q cannot be read: %v", path, err)
	case !fi.IsDir():
		return MediaInfo{}, inputErrorf("--from-media %q is not a directory (a copy-set directory or an extracted ISO tree, DESIGN_SPEC 8.5)", path)
	}

	rootManifest := filepath.Join(path, media.ManifestFileName)
	if _, err := os.Stat(rootManifest); err == nil {
		return resolveMediaRoot(path)
	} else if !os.IsNotExist(err) {
		return MediaInfo{}, inputErrorf("--from-media %q: checking %s: %v", path, media.ManifestFileName, err)
	}
	return resolveMediaSiblings(path)
}

// resolveMediaRoot handles the ROOT MODE tree (see the file doc).
func resolveMediaRoot(path string) (MediaInfo, error) {
	man, err := readMediaManifest(filepath.Join(path, media.ManifestFileName))
	if err != nil {
		return MediaInfo{}, err
	}
	return MediaInfo{
		SetID:       man.SetID,
		Manifest:    man,
		Manifests:   map[int64]media.Manifest{man.ImageNumber: man},
		ManifestDir: path,
		ExportPath:  filepath.Join(path, media.ExportFileName),
		Trees:       map[int64]string{man.ImageNumber: path},
	}, nil
}

// resolveMediaSiblings handles the SIBLING MODE directory (see the file
// doc): every subdirectory with a parseable MANIFEST.json becomes that
// image's tree; the lowest-numbered image's manifest is the
// introspection manifest.
func resolveMediaSiblings(path string) (MediaInfo, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return MediaInfo{}, inputErrorf("--from-media %q cannot be listed: %v", path, err)
	}
	trees := map[int64]string{}
	manifests := map[int64]media.Manifest{}
	manifestPaths := map[int64]string{}
	setID := ""
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(path, e.Name())
		manPath := filepath.Join(dir, media.ManifestFileName)
		if _, err := os.Stat(manPath); err != nil {
			if os.IsNotExist(err) {
				continue // no manifest here: not a media tree root (see the file doc)
			}
			return MediaInfo{}, inputErrorf("--from-media %q: checking %s: %v", dir, media.ManifestFileName, err)
		}
		man, err := readMediaManifest(manPath)
		if err != nil {
			return MediaInfo{}, err
		}
		if setID == "" {
			setID = man.SetID
		} else if man.SetID != setID {
			return MediaInfo{}, inputErrorf(
				"--from-media %q mixes backup sets: %s holds set %q while another subdirectory holds set %q "+
					"(point --from-media at one set's trees, DESIGN_SPEC 8.5)",
				path, manPath, man.SetID, setID)
		}
		if _, dup := manifests[man.ImageNumber]; dup {
			return MediaInfo{}, inputErrorf(
				"--from-media %q holds two subdirectories both claiming image %d of set %q (%s and %s)",
				path, man.ImageNumber, setID, manifestPaths[man.ImageNumber], manPath)
		}
		trees[man.ImageNumber] = dir
		manifests[man.ImageNumber] = man
		manifestPaths[man.ImageNumber] = manPath
	}
	if len(manifests) == 0 {
		return MediaInfo{}, inputErrorf(
			"--from-media %q holds no %s at its root or in any subdirectory "+
				"(DESIGN_SPEC 8.5: PATH is a copy-set directory or an extracted ISO tree containing MANIFEST.json and %s)",
			path, media.ManifestFileName, media.ExportFileName)
	}
	// Ascending image numbers are deterministic introspection: the
	// lowest image's manifest leads (its placement summary covers the
	// whole set by construction, FR-033).
	nums := make([]int64, 0, len(manifests))
	for id := range manifests {
		nums = append(nums, id)
	}
	sort.Slice(nums, func(i, j int) bool { return nums[i] < nums[j] })
	lead := nums[0]
	return MediaInfo{
		SetID:       setID,
		Manifest:    manifests[lead],
		Manifests:   manifests,
		ManifestDir: filepath.Dir(manifestPaths[lead]),
		ExportPath:  filepath.Join(filepath.Dir(manifestPaths[lead]), media.ExportFileName),
		Trees:       trees,
	}, nil
}

// readMediaManifest reads and parses one MANIFEST.json, wrapping any
// parse failure as the loud media-integrity class naming the file (a
// broken anchor where it EXISTS is never a silent skip).
func readMediaManifest(path string) (media.Manifest, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return media.Manifest{}, mediaIntegrityErrorf(path, "reading MANIFEST.json: %v", err)
	}
	man, err := media.ParseManifest(blob)
	if err != nil {
		return media.Manifest{}, mediaIntegrityErrorf(path, "%v", err)
	}
	return man, nil
}
