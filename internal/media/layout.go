package media

// T3.1: the on-media logical tree layout (DESIGN_SPEC 5.3 - ONE tree
// model shared by copy mode, ISO mode, verify, and restore; 6.4 -
// deterministic image naming; PM resolution recorded in WORK_LOG:
// chunk FILES on media are named 1-based zero-padded 5-digit while
// catalog chunk indices stay 0-based).
//
// This file is pure path arithmetic over catalog-supplied relpaths. It
// performs NO filesystem I/O and issues NO SQL (DESIGN_SPEC 1.2 layering:
// like internal/plan, this package consumes row-shaped inputs the caller
// adapts from the catalog package; the cli wiring layer maps between the
// structurally identical shapes). The one piece of behavior beyond
// arithmetic is defensive relpath validation - see ValidateRelPath.
//
// The tree, for image number N of set S (DESIGN_SPEC 5.3):
//
//	S-NNN/
//	  files/<relpath>            whole files AND empty dirs, original
//	                             relative tree preserved
//	  chunks/<relpath>/00001     chunk files for chunked parent <relpath>
//	  MANIFEST.json              FR-033 manifest (manifest.go)
//	  catalog-export.sqlite      FR-034 snapshot export (T3.2)
//
// Collision immunity is structural, not procedural: files/ and chunks/
// are DISJOINT roots, so no source-tree path - including a file
// literally named "chunks" or "MANIFEST.json" - can ever collide with
// another member. A source file named "chunks" lives at files/chunks;
// the chunk data of a source file named "chunks" lives at
// chunks/chunks/00001. No filename escaping scheme is needed (this is
// exactly why 5.3 put chunks in a parallel tree instead of sidecar
// names next to the parent). layout_test.go pins this with adversarial
// relpaths.

import (
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// The two disjoint roots of the logical tree (DESIGN_SPEC 5.3). Every
// payload member of an image lives under exactly one of them; the two
// remaining members (MANIFEST.json, catalog-export.sqlite) sit at the
// image root and are named by the constants below.
const (
	FilesRoot  = "files"
	ChunksRoot = "chunks"
)

// ManifestFileName is the FR-033 manifest member at the image root
// (DESIGN_SPEC 5.3, 6.6). Generation of its content lives in
// manifest.go; writing it to the destination is T3.3/T4.3's concern.
const ManifestFileName = "MANIFEST.json"

// ExportFileName is the FR-034 catalog-export member at the image root
// (DESIGN_SPEC 5.3, 6.6). Export PRODUCTION (VACUUM INTO) is T3.2; this
// constant is defined here so T3.2, T3.3, and T4.3 all agree on the
// single on-media name.
const ExportFileName = "catalog-export.sqlite"

// chunkNameDigits is the fixed width of the on-media chunk file name
// (DESIGN_SPEC 5.3 shows chunks/<relpath>/00001; the PM resolution in
// WORK_LOG fixes 1-based, zero-padded, 5-digit names on media while
// catalog chunk_index columns stay 0-based).
const chunkNameDigits = 5

// maxChunkNameIndex is the largest catalog (0-based) chunk index that
// fits the 5-digit 1-based media scheme: index 99999 maps to media name
// "100000", which breaks the fixed-width format, so it is refused. The
// ceiling bounds a single file at 100000 chunks; at the 20 GiB default
// chunk size (DESIGN_SPEC 4.2) that is ~1.95 PB per file - far beyond
// any V1 use, and refusing keeps the name format an invariant rather
// than letting it silently widen.
const maxChunkNameIndex = 99998

// ErrUnsafeRelPath classifies a relpath that fails ValidateRelPath, and
// the same-class raw-byte refusals of a non-UTF-8 symlink link target
// (manifest.go buildEntries) and a non-UTF-8 set id (validateSetID):
// the raw-byte trio corrupts the JSON MANIFEST rather than the
// filesystem - see the UTF-8 invariant in manifest.go's file doc. The
// relpaths this package consumes come from catalog rows, which are
// produced by the ingest walker and are relative by construction; a
// path reaching this error therefore indicates CORRUPT or hostile
// catalog content, and every builder refuses to encode it rather than
// emitting a media path that could escape the image root when a later
// stage joins it onto a destination directory (defense in depth - the
// writers of T3.3/T4.3 additionally never join unvalidated strings
// onto filesystem roots).
var ErrUnsafeRelPath = errors.New("media: unsafe relpath")

// ValidateRelPath defends against catalog rows that are not honest
// slash-separated relative paths: it refuses the empty path, absolute
// paths, any "." or ".." component, empty components ("a//b", a leading
// or trailing slash), NUL bytes, and names that are not valid UTF-8
// (the JSON-fidelity gate - see the UTF-8 invariant in manifest.go's
// file doc; the message names "not valid UTF-8", distinguishable from
// the NUL case). A backslash is NOT refused: on the Linux-only target
// (NFR-001) it is a legal filename byte, and this tool interprets
// nothing platform-specific into relpaths.
func ValidateRelPath(rel string) error {
	if rel == "" {
		return fmt.Errorf("%w: empty path", ErrUnsafeRelPath)
	}
	if strings.IndexByte(rel, 0) >= 0 {
		return fmt.Errorf("%w: %q contains a NUL byte", ErrUnsafeRelPath, rel)
	}
	if !utf8.ValidString(rel) {
		// JSON-fidelity gate (UTF-8 invariant, manifest.go file doc):
		// the ingest walker deliberately preserves raw-byte filenames
		// (e.g. latin-1 names; internal/ingest TestWalkInvalidUTF8Filename),
		// and the media TREE writes their exact bytes - but MANIFEST.json
		// is JSON, and JSON cannot represent raw bytes: marshaling would
		// sanitize each invalid byte to U+FFFD with err=nil and record a
		// path that DOES NOT exist on media, silently breaking
		// manifest-driven verification and FR-026 media-alone restore for
		// exactly those files. Refused at the gate instead, consistent
		// with the unreadable-class convention (rows that cannot be
		// faithfully recorded never reach media).
		return fmt.Errorf("%w: %q is not valid UTF-8 (a raw-byte name cannot be faithfully recorded in the JSON manifest)",
			ErrUnsafeRelPath, rel)
	}
	if strings.HasPrefix(rel, "/") {
		return fmt.Errorf("%w: %q is absolute", ErrUnsafeRelPath, rel)
	}
	for _, comp := range strings.Split(rel, "/") {
		switch comp {
		case "":
			return fmt.Errorf("%w: %q has an empty path component", ErrUnsafeRelPath, rel)
		case ".", "..":
			return fmt.Errorf("%w: %q contains a %q component", ErrUnsafeRelPath, rel, comp)
		}
	}
	return nil
}

// FileMediaPath builds the media path of a whole file OR an empty
// directory under the files/ root: "files/<relpath>" (DESIGN_SPEC 5.3).
// Empty directories share the files/ tree because they are part of the
// ORIGINAL relative tree (the directories table's relpath, DESIGN_SPEC
// 2.3 / FR-037): a non-empty directory is implied by the file paths
// under it, an empty one is recorded explicitly, but both are created
// at the same place on restore. Non-empty directories need no entry at
// all (5.3: the tree is created by the paths it contains).
func FileMediaPath(rel string) (string, error) {
	if err := ValidateRelPath(rel); err != nil {
		return "", err
	}
	return FilesRoot + "/" + rel, nil
}

// ChunkFileName renders the on-media file name for one chunk from its
// CATALOG index: the media name is 1-based and zero-padded to 5 digits
// (catalog index 0 -> "00001", index 12 -> "00013"), per the PM
// resolution in WORK_LOG and the 5.3 example. Catalog chunk_index
// columns stay 0-based everywhere (DESIGN_SPEC 2.3); the +1 exists ONLY
// at this media-name boundary and is documented here, in manifest.go's
// ChunkIndex field, and pinned by layout_test.go.
func ChunkFileName(chunkIndex0 int64) (string, error) {
	if chunkIndex0 < 0 {
		return "", fmt.Errorf("media: chunk index %d must be >= 0 (catalog chunk indices are 0-based)", chunkIndex0)
	}
	if chunkIndex0 > maxChunkNameIndex {
		return "", fmt.Errorf("media: chunk index %d exceeds the %d-name 5-digit media scheme (DESIGN_SPEC 5.3)",
			chunkIndex0, maxChunkNameIndex+1)
	}
	return fmt.Sprintf("%0*d", chunkNameDigits, chunkIndex0+1), nil
}

// ChunkMediaPath builds the media path of one chunk of the file at
// relpath: "chunks/<relpath>/NNNNN" with NNNNN the 1-based 5-digit
// on-media name (ChunkFileName). The parent relpath is reused verbatim
// under the chunks/ root, which is what keeps the two trees disjoint
// even when a source file is literally named "chunks" (5.3 collision
// rationale).
func ChunkMediaPath(rel string, chunkIndex0 int64) (string, error) {
	if err := ValidateRelPath(rel); err != nil {
		return "", err
	}
	name, err := ChunkFileName(chunkIndex0)
	if err != nil {
		return "", err
	}
	return ChunksRoot + "/" + rel + "/" + name, nil
}

// ImageDirName renders the copy-set directory name (and the ISO
// directory-side name) for image N of set S: "<SET-ID>-NNN", image
// numbers 1-based and zero-padded to 3 digits (DESIGN_SPEC 6.4; the
// numbering decides FR-024's "image 1" phrasing in favor of 1-based).
// Numbers above 999 render with more digits (fmt's %03d is a minimum
// width): the scheme stays deterministic, and refusing a 1000th image
// would invent a capacity policy the spec does not have. Past 999 the
// names are NOT lexicographically sortable: bytewise "S-1000" sorts
// BELOW "S-999" (the digit '1' precedes '9'), so a plain string sort
// misplaces every 4-digit image among the 3-digit ones - consumers
// must sort NUMERICALLY on the parsed image number, which is what the
// tool's own discovery does (8.3 finds <SET-ID>-NNN by pattern and
// parses the number). Pinned by TestImageDirNameSortabilityPast999.
func ImageDirName(setID string, imageNumber int64) (string, error) {
	if err := validateSetID(setID); err != nil {
		return "", err
	}
	if imageNumber < 1 {
		return "", fmt.Errorf("media: image number %d must be >= 1 (images are 1-based, DESIGN_SPEC 6.4)", imageNumber)
	}
	return fmt.Sprintf("%s-%03d", setID, imageNumber), nil
}

// ImageISOFileName renders the finalized ISO file name for image N of
// set S: "<SET-ID>-NNN.iso" (DESIGN_SPEC 6.4, 6.3 step 3; the same
// deterministic scheme --image-dir discovery relies on in 8.3). It is
// defined next to ImageDirName so the one naming scheme has exactly one
// implementation across copy, build, and restore discovery.
func ImageISOFileName(setID string, imageNumber int64) (string, error) {
	dir, err := ImageDirName(setID, imageNumber)
	if err != nil {
		return "", err
	}
	return dir + ".iso", nil
}

// validateSetID refuses a set id that cannot be a single safe path
// component: empty, containing a slash or NUL, not valid UTF-8, or
// literally "." or ".." (which would make ImageDirName escape the
// destination root when a writer joins it onto <dest-root>). Set ids
// come from --set-id or the 6.4 derivation, so an offending value is a
// bug or a hostile catalog, never user intent. The non-UTF-8 case
// carries the ErrUnsafeRelPath sentinel: the id becomes the image
// directory name RECORDED IN MANIFEST.json, and a raw-byte id can never
// be faithfully recorded there (UTF-8 invariant, manifest.go file doc).
func validateSetID(setID string) error {
	switch {
	case setID == "":
		return fmt.Errorf("media: empty set id")
	case strings.IndexByte(setID, 0) >= 0:
		return fmt.Errorf("media: set id %q contains a NUL byte", setID)
	case !utf8.ValidString(setID):
		// JSON-fidelity gate (UTF-8 invariant, manifest.go file doc):
		// set_id and the on-media directory name would be sanitized to
		// U+FFFD by JSON marshaling, recording a name that does not
		// match anything on media.
		return fmt.Errorf("%w: set id %q is not valid UTF-8 (a raw-byte id cannot be faithfully recorded in the JSON manifest)",
			ErrUnsafeRelPath, setID)
	case strings.ContainsAny(setID, "/"):
		return fmt.Errorf("media: set id %q contains a slash", setID)
	case setID == "." || setID == "..":
		return fmt.Errorf("media: set id %q is not a usable path component", setID)
	}
	return nil
}
