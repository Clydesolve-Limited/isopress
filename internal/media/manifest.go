package media

// T3.1: MANIFEST.json generation (DESIGN_SPEC 6.6 per-image content
// list; FR-033 manifest on every image; FR-035 redundancy; D-04
// hard-link groups; FR-037 dirs as kind='dir').
//
// This file is pure generation: ManifestInput carries row-shaped data
// the CALLER read from the catalog (image_contents-style rows for this
// image, whole-file anchors for chunked parents, hard-link families,
// and the whole-set placement summary), and GenerateManifest validates
// it and assembles the Manifest value. MarshalManifest renders the
// deterministic JSON bytes. No SQL, no filesystem, no clock of its own:
// the timestamp is injected (DESIGN_SPEC 1.2 layering, same catalog-free
// seam as internal/plan).
//
// # The self-entry reading (6.6 "every entry of this image")
//
// DESIGN_SPEC 6.6 says the image_contents TABLE holds "every payload
// file, chunk, symlink, the manifest, and the export", and that the
// manifest is "generated from image_contents plus set-level placement".
// Read literally, the manifest would have to list itself - which is
// arithmetically impossible for an entry carrying a sha256 (a file
// cannot contain its own hash). The only self-consistent reading, and
// the one implemented here:
//
//   - MANIFEST.json lists the PAYLOAD entries of the image: kind
//     'file', 'chunk', 'symlink', 'dir'. GenerateManifest REJECTS kind
//     'manifest' and 'export' inputs with an error saying exactly where
//     those belong.
//   - The manifest and export self-entries are recorded in
//     image_contents (the table) by the WRITER at write time (T3.3 copy
//     mode, T4.3 image mode), which is precisely what 6.6's
//     "the builder computes the final image_contents rows ... (every
//     payload file, chunk, symlink, the manifest, and the export)"
//     describes. image_contents is the FR-020 finalized content list;
//     the manifest is its human-readable mirror minus the two entries
//     that cannot carry a hash of themselves.
//   - The manifest self-exclusion is pinned by
//     TestGenerateManifestExcludesSelfEntries.
//
// # Hard-link families are INPUT (the link_group finding, restated)
//
// DESIGN_SPEC 2.4 gives hard-link families a shared link_group column,
// but the scan pipeline as shipped NEVER populates it: the catalog
// insert hard-codes it NULL "until hard-link family grouping lands with
// the copy/plan work" (internal/catalog/scanstore.go insertValues), and
// the S2 balancer therefore derives families from (dev, inode) over
// active rows with nlink > 1, primary = lowest file_id, inode != 0
// (internal/plan/balancer.go "Hard-link families" section). This
// generator does the same thing the balancer does NOT do - it cannot
// query - it ACCEPTS the families: the caller derives them from
// dev+inode+nlink exactly as the balancer does (or from link_group if
// that column is ever populated; the input shape is identical either
// way) and passes HardLinkGroups. Per D-04 (confirmed 2026-10-18) the
// manifest records ALL names plus the shared hash REGARDLESS of write
// mode: ISO stores only the primary name on the tree, copy mode stores
// the primary once with the secondaries as link(2) - the group below is
// the mode-independent authority a restore recreates the family from.
//
// # Chunk indices: catalog 0-based recorded, media name derived
//
// 6.6 asks the chunk table for "index, length, hash, and reassembly
// order". The INDEX recorded here is the CATALOG 0-based chunk_index
// (DESIGN_SPEC 2.3), because that is the value every catalog query,
// planner property test, and restore selection speaks. The on-media
// file name is a pure function of it (ChunkFileName: index+1, 5-digit),
// and the entry's own "path" field already carries the concrete media
// path (chunks/<relpath>/00001), so recording the 1-based name as a
// separate field would duplicate derivable data and invite drift.
// "Length" is the entry's size field of the same entry in the chunk
// table. "Reassembly order" is the ASCENDING INDEX ORDER of the chunks
// array: array order IS the order (documented format rule); chunks of
// one parent may be spread across several images, so per-image tables
// need not be contiguous - contiguity over the whole set is a planner
// invariant (DESIGN_SPEC 12.5), not a manifest one.
//
// # Raw-byte values are refused at the gate (the UTF-8 invariant)
//
// MANIFEST.json is JSON, and JSON cannot represent raw bytes:
// encoding/json SANITIZES any string that is not valid UTF-8, silently
// replacing each invalid byte with U+FFFD and returning err=nil
// (pinned empirically by TestMarshalManifestTotality). A record whose
// path, link target, or set id is not valid UTF-8 can therefore never
// be faithfully recorded - the manifest would name a value that DOES
// NOT exist on media, silently breaking manifest-driven verification
// and FR-026 media-alone restore for exactly the trees internal/ingest
// deliberately preserves (the walker carries raw-byte invalid-UTF-8
// filenames verbatim; TestWalkInvalidUTF8Filename pins it). So the
// gate is UPSTREAM, consistent with the unreadable-class convention
// ("rows that cannot be faithfully represented never reach media"):
// ValidateRelPath refuses non-UTF-8 relpaths, buildEntries refuses
// non-UTF-8 symlink targets (symlink targets are raw bytes too), and
// validateSetID refuses non-UTF-8 set ids - all classifying via the
// ErrUnsafeRelPath sentinel with a message naming "not valid UTF-8",
// distinguishable from the NUL case. This package is the GATE only;
// the writers (T3.3 copy mode, T4.3 image mode) warn-and-skip such
// rows per the same unreadable-class convention.
//
// # Determinism and versioning (6.6 field list)
//
// MANIFEST.json is a 10-years-from-now artifact a human may open, so
// it is indented with two spaces and trailing-newline terminated. The
// struct field declaration order below IS the wire format: encoding/json
// marshals struct fields in declaration order, so REORDERING FIELDS IS A
// FORMAT CHANGE - treat the structs as frozen once released. Every
// slice is sorted by GenerateManifest (entries by media path, chunked
// files by parent path, chunks by index, link-group names ascending,
// groups by first name, placement by image number then path), so equal
// inputs yield byte-identical output (pinned by the determinism tests).
// schema_version carries the CATALOG schema version (catalog.
// SchemaVersion at generation time) exactly as 6.6 names it; the caller
// injects it together with catalog.ToolVersion, keeping this package
// free of a catalog import (the internal/plan layering precedent).
//
// Top-level sections (entries, chunked_files, hard_link_groups,
// placement) are ALWAYS present, empty array when nil - the shape does
// not change with the data. Entry-level optional fields (sha256,
// link_target, parent, chunk_index) use omitempty; chunk_index is a
// pointer because index 0 is a real value that omitempty would drop.

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// The payload entry kinds MANIFEST.json knows (DESIGN_SPEC 2.3
// image_contents CHECK minus 'manifest' and 'export' - see the file doc
// for the self-entry reading).
const (
	KindFile    = "file"
	KindChunk   = "chunk"
	KindSymlink = "symlink"
	KindDir     = "dir"
)

// Manifest is the assembled MANIFEST.json document (DESIGN_SPEC 6.6).
// The field declaration order IS the JSON format - see the file doc.
type Manifest struct {
	SetID          string           `json:"set_id"`
	ImageNumber    int64            `json:"image_number"`
	ToolVersion    string           `json:"tool_version"`
	SchemaVersion  int              `json:"schema_version"`
	CreatedAt      string           `json:"created_at"` // RFC 3339, UTC
	Entries        []Entry          `json:"entries"`
	ChunkedFiles   []ChunkedFile    `json:"chunked_files"`
	HardLinkGroups []HardLinkGroup  `json:"hard_link_groups"`
	Placement      []ImagePlacement `json:"placement"`
}

// Entry is one payload entry of THIS image (6.6 "every entry of this
// image with path, kind, size, sha256, mtime_ns, mode, uid, gid").
// MediaPath is the path INSIDE the logical tree ("files/a/b.txt",
// "chunks/a/b.txt/00001"); for kind 'dir' and 'file' it is the source
// relpath under files/, for 'chunk' it embeds the parent relpath plus
// the derived media name. Mode/UID/GID/MtimeNS come from the files or
// directories row (image_contents does not carry them; the manifest
// does, per 6.6 and FR-036/FR-037 restore needs).
type Entry struct {
	MediaPath  string `json:"path"`
	Kind       string `json:"kind"`
	Size       int64  `json:"size"`             // bytes; symlinks: len(target); dirs: 0
	SHA256     string `json:"sha256,omitempty"` // lowercase hex; ABSENT for symlink/dir (catalog NULL-hash convention)
	MtimeNS    int64  `json:"mtime_ns"`
	Mode       uint32 `json:"mode"` // raw POSIX permission bits, decimal
	UID        uint32 `json:"uid"`
	GID        uint32 `json:"gid"`
	LinkTarget string `json:"link_target,omitempty"` // symlink only (D-03)
	ParentPath string `json:"parent,omitempty"`      // chunk only: parent file relpath
	ChunkIndex *int64 `json:"chunk_index,omitempty"` // chunk only: CATALOG 0-based index (see file doc)
}

// ChunkedFile is the reassembly table for one chunked parent whose
// chunks live on this image (6.6 "chunk tables for chunked parents").
// A parent may be spread across several images; the table then lists
// only this image's chunks, and restore collects the rest via the
// placement summary. WholeSHA256 is the parent's whole-file hash - the
// end-to-end reassembly anchor (FR-041 double verification).
type ChunkedFile struct {
	RelPath string      `json:"path"`   // parent file relpath (source-relative)
	Size    int64       `json:"size"`   // whole-file logical size
	SHA256  string      `json:"sha256"` // whole-file hash, lowercase hex
	Chunks  []ChunkPart `json:"chunks"` // ASCENDING catalog index; array order IS reassembly order
}

// ChunkPart is one chunk row of a ChunkedFile table: catalog index,
// length in bytes, and the chunk hash (6.6's "index, length, hash").
type ChunkPart struct {
	Index  int64  `json:"index"`  // catalog 0-based chunk_index (media name = index+1, 5-digit)
	Length int64  `json:"length"` // bytes
	SHA256 string `json:"sha256"` // lowercase hex
}

// HardLinkGroup is one hard-link family: ALL of its names plus the
// shared content hash (6.6 "hard-link groups (all names plus shared
// hash)"; D-04 confirmed 2026-10-18). Names are SOURCE relpaths, sorted
// ascending; the family is recorded regardless of write mode so a
// media-alone restore recreates the family from one stored copy
// (FR-026 + FR-039).
type HardLinkGroup struct {
	SHA256 string   `json:"sha256"` // shared hash of every name, lowercase hex
	Names  []string `json:"names"`  // ALL names, ascending
}

// ImagePlacement is one image's slice of the whole-set placement
// summary (6.6 "the placement summary for the whole set"; FR-033
// "which image number(s) hold which files"; FR-035 redundancy - every
// image carries the record for EVERY image). Paths are SOURCE relpaths
// (payload only: whole files, chunked parents, symlinks, empty dirs),
// sorted ascending. A chunked parent appears under every image holding
// any of its chunks. The manifest/export rows of image_contents are NOT
// placement data and must be filtered out by the caller before building
// the input (they are members of every image by construction).
type ImagePlacement struct {
	ImageNumber int64    `json:"image"`
	Paths       []string `json:"paths"`
}

// ManifestInput is everything GenerateManifest needs, injected by the
// caller (T3.3 copy writer / T4.3 image builder) after fetching rows
// from the catalog. ToolVersion and SchemaVersion are injected rather
// than imported (pass catalog.ToolVersion and catalog.SchemaVersion)
// to keep this package catalog-free, the internal/plan precedent.
type ManifestInput struct {
	SetID          string
	ImageNumber    int64
	ToolVersion    string
	SchemaVersion  int
	CreatedAt      time.Time          // injected clock; rendered RFC 3339Nano UTC
	Entries        []ImageEntry       // payload entries of THIS image
	ChunkedFiles   []ChunkedFileInput // one per chunked parent touching this image
	HardLinkGroups []HardLinkGroupInput
	Placement      []PlacementInput // whole-set summary, all images
}

// ImageEntry is one payload entry of this image, shaped like the
// image_contents row the writer will persist (DESIGN_SPEC 2.3) plus the
// metadata columns image_contents lacks but 6.6's manifest entry needs
// (mode/uid/gid/mtime live on the files/directories rows). RelPath is
// the SOURCE relpath; for chunks it is the PARENT relpath and the media
// path is derived here via ChunkMediaPath, so a hostile relpath is
// refused at manifest time exactly as at layout time.
type ImageEntry struct {
	Kind       string // KindFile | KindChunk | KindSymlink | KindDir
	RelPath    string // parent relpath for chunks; dir relpath for dirs
	ChunkIndex int64  // catalog 0-based; only for KindChunk
	Size       int64  // bytes; symlinks: len(target); dirs: 0
	SHA256     []byte // 32-byte sha256; nil for symlink/dir (catalog NULL convention)
	LinkTarget string // symlink only
	Mode       uint32
	UID        uint32
	GID        uint32
	MtimeNS    int64
}

// ChunkedFileInput anchors one chunked parent: the WHOLE-file size and
// hash that reassembly verifies against (FR-041 second verification).
// The per-chunk parts are NOT passed here - they are collected from
// this image's chunk entries, so the chunk table can never drift from
// the entries list.
type ChunkedFileInput struct {
	RelPath     string
	WholeSize   int64 // > 0
	WholeSHA256 []byte
}

// HardLinkGroupInput is one family the caller derived (dev+inode over
// active rows with nlink > 1, balancer semantics - see the file doc).
// Names are source relpaths; SHA256 is the shared content hash.
type HardLinkGroupInput struct {
	SHA256 []byte
	Names  []string
}

// PlacementInput is one image's payload paths in the whole-set summary.
type PlacementInput struct {
	ImageNumber int64
	Paths       []string
}

// sha256HexLen is the exact byte length of a stored sha256 digest
// (DESIGN_SPEC 2.3: hash BLOB, 32 bytes, D-09).
const sha256HexLen = 32

// GenerateManifest validates the input and assembles the Manifest
// (DESIGN_SPEC 6.6). Validation is deliberately strict: everything this
// function refuses is something that would otherwise land silently on
// media a human opens in ten years. Refusals:
//
//   - header: empty set id / non-positive image number / empty tool
//     version / non-positive schema version / zero CreatedAt (a forgot
//     to inject the clock);
//   - kind 'manifest' or 'export' (the self-entry reading above);
//   - any unsafe relpath (ValidateRelPath - the in-bad-catalog defense)
//     or unsafe set id, including any non-UTF-8 path, link target, or
//     set id (the JSON-fidelity gate, UTF-8 invariant in the file doc);
//   - duplicate media paths (image_contents PRIMARY KEY forbids them);
//   - hash shape: file and chunk entries REQUIRE a 32-byte hash;
//     symlink and dir entries must carry NONE (catalog NULL-hash
//     convention);
//   - chunk shape: index >= 0, length > 0; every chunked parent has at
//     least one chunk entry on this image; every chunk entry has a
//     chunked parent record (no orphans);
//   - hard-link groups: at least two names, 32-byte hash;
//   - placement: this image must have a placement record, its numbers
//     are unique, its per-image path lists are duplicate-free, and
//     every entry's parent relpath appears in THIS image's placement
//     list (one-directional consistency gate: placement may list a path
//     only the other images hold, but never the reverse).
//
// The returned Manifest's slices are fully sorted; MarshalManifest is
// therefore deterministic for equal inputs.
func GenerateManifest(in ManifestInput) (Manifest, error) {
	if err := validateSetID(in.SetID); err != nil {
		return Manifest{}, err
	}
	if in.ImageNumber < 1 {
		return Manifest{}, fmt.Errorf("media: image number %d must be >= 1 (DESIGN_SPEC 6.4)", in.ImageNumber)
	}
	if in.ToolVersion == "" {
		return Manifest{}, fmt.Errorf("media: empty tool version (pass catalog.ToolVersion)")
	}
	if in.SchemaVersion < 1 {
		return Manifest{}, fmt.Errorf("media: schema version %d must be >= 1 (pass catalog.SchemaVersion)", in.SchemaVersion)
	}
	if in.CreatedAt.IsZero() {
		return Manifest{}, fmt.Errorf("media: zero CreatedAt (inject the caller's clock)")
	}

	entries, err := buildEntries(in.Entries)
	if err != nil {
		return Manifest{}, err
	}
	chunked, err := buildChunkedFiles(in.ChunkedFiles, entries)
	if err != nil {
		return Manifest{}, err
	}
	groups, err := buildHardLinkGroups(in.HardLinkGroups)
	if err != nil {
		return Manifest{}, err
	}
	placement, err := buildPlacement(in.Placement, in.ImageNumber)
	if err != nil {
		return Manifest{}, err
	}
	if err := checkPlacementCoversEntries(placement, in.ImageNumber, entries); err != nil {
		return Manifest{}, err
	}

	return Manifest{
		SetID:          in.SetID,
		ImageNumber:    in.ImageNumber,
		ToolVersion:    in.ToolVersion,
		SchemaVersion:  in.SchemaVersion,
		CreatedAt:      in.CreatedAt.UTC().Format(time.RFC3339Nano),
		Entries:        entries,
		ChunkedFiles:   chunked,
		HardLinkGroups: groups,
		Placement:      placement,
	}, nil
}

// buildEntries converts and sorts this image's payload entries and
// rejects duplicates by media path.
func buildEntries(in []ImageEntry) ([]Entry, error) {
	out := make([]Entry, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, e := range in {
		var entry Entry
		var err error
		switch e.Kind {
		case KindFile:
			if e.SHA256 == nil {
				return nil, fmt.Errorf("media: file entry %q has no sha256 (unreadable-class rows never reach media)", e.RelPath)
			}
			if e.Size < 0 {
				return nil, fmt.Errorf("media: file entry %q has negative size %d", e.RelPath, e.Size)
			}
			entry = Entry{
				Kind:   KindFile,
				SHA256: hex.EncodeToString(e.SHA256),
			}
		case KindChunk:
			if e.ChunkIndex < 0 {
				return nil, fmt.Errorf("media: chunk entry %q has negative catalog index %d", e.RelPath, e.ChunkIndex)
			}
			if e.Size <= 0 {
				return nil, fmt.Errorf("media: chunk entry %q (index %d) has non-positive length %d", e.RelPath, e.ChunkIndex, e.Size)
			}
			if e.SHA256 == nil {
				return nil, fmt.Errorf("media: chunk entry %q (index %d) has no sha256", e.RelPath, e.ChunkIndex)
			}
			idx := e.ChunkIndex
			entry = Entry{
				Kind:       KindChunk,
				SHA256:     hex.EncodeToString(e.SHA256),
				ParentPath: e.RelPath,
				ChunkIndex: &idx,
			}
		case KindSymlink:
			if !utf8.ValidString(e.LinkTarget) {
				// JSON-fidelity gate (UTF-8 invariant, file doc):
				// readlink(2) returns targets as raw bytes, and the media
				// TREE writes them verbatim - but JSON cannot represent
				// raw bytes, so the recorded link_target would be
				// sanitized to U+FFFD and restore would recreate a link
				// pointing somewhere else (or nowhere). Same sentinel
				// class as ValidateRelPath; refused before the shape
				// checks so the diagnostic names the fidelity reason.
				return nil, fmt.Errorf(
					"%w: symlink entry %q has a link target that is not valid UTF-8 (a raw-byte target cannot be faithfully recorded in the JSON manifest)",
					ErrUnsafeRelPath, e.RelPath)
			}
			if e.SHA256 != nil {
				return nil, fmt.Errorf("media: symlink entry %q carries a sha256 (symlinks are NULL-hash by convention, DESIGN_SPEC 2.3 D-03)", e.RelPath)
			}
			if e.Size != int64(len(e.LinkTarget)) {
				return nil, fmt.Errorf("media: symlink entry %q size %d != len(target) %d (DESIGN_SPEC 2.3: symlinks store len(target))",
					e.RelPath, e.Size, len(e.LinkTarget))
			}
			entry = Entry{
				Kind:       KindSymlink,
				LinkTarget: e.LinkTarget,
			}
		case KindDir:
			if e.SHA256 != nil {
				return nil, fmt.Errorf("media: dir entry %q carries a sha256 (directories have no content hash)", e.RelPath)
			}
			if e.Size != 0 {
				return nil, fmt.Errorf("media: dir entry %q carries size %d (directories have no content size)", e.RelPath, e.Size)
			}
			entry = Entry{Kind: KindDir}
		case "manifest", "export":
			return nil, fmt.Errorf(
				"media: entry kind %q is not a payload entry: MANIFEST.json and %s self-entries are recorded in "+
					"image_contents by the writer at write time (DESIGN_SPEC 6.6), never listed inside the manifest itself",
				e.Kind, ExportFileName)
		default:
			return nil, fmt.Errorf("media: unknown entry kind %q for %q (want file|chunk|symlink|dir)", e.Kind, e.RelPath)
		}

		entry.MediaPath, err = mediaPathFor(e)
		if err != nil {
			return nil, err
		}
		entry.Size = e.Size
		entry.MtimeNS = e.MtimeNS
		entry.Mode = e.Mode
		entry.UID = e.UID
		entry.GID = e.GID
		if e.SHA256 != nil && len(e.SHA256) != sha256HexLen {
			return nil, fmt.Errorf("media: entry %q hash is %d bytes, want %d", e.RelPath, len(e.SHA256), sha256HexLen)
		}
		if _, dup := seen[entry.MediaPath]; dup {
			return nil, fmt.Errorf("media: duplicate media path %q in image entries", entry.MediaPath)
		}
		seen[entry.MediaPath] = struct{}{}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MediaPath < out[j].MediaPath })
	return out, nil
}

// mediaPathFor derives an entry's media path through the layout.go
// builders - the single place the files/ and chunks/ mapping lives -
// so every hostile-relpath refusal applies here too.
func mediaPathFor(e ImageEntry) (string, error) {
	switch e.Kind {
	case KindFile, KindSymlink, KindDir:
		// Whole files, symlinks, and empty dirs all live in the files/
		// tree at their source relpath (DESIGN_SPEC 5.3; dirs share the
		// files/ tree per layout.go FileMediaPath).
		return FileMediaPath(e.RelPath)
	case KindChunk:
		return ChunkMediaPath(e.RelPath, e.ChunkIndex)
	default:
		// Unreachable: buildEntries validates the kind before calling.
		return "", fmt.Errorf("media: unknown entry kind %q", e.Kind)
	}
}

// buildChunkedFiles assembles the reassembly tables: for every chunked
// parent record, collect THIS image's chunk entries (already sorted by
// media path, which for equal parents equals ascending index), and
// cross-check both directions between records and entries.
func buildChunkedFiles(in []ChunkedFileInput, entries []Entry) ([]ChunkedFile, error) {
	// Index this image's chunk entries by parent relpath.
	byParent := make(map[string][]ChunkPart)
	for _, e := range entries {
		if e.Kind != KindChunk {
			continue
		}
		byParent[e.ParentPath] = append(byParent[e.ParentPath], ChunkPart{
			Index:  *e.ChunkIndex,
			Length: e.Size,
			SHA256: e.SHA256,
		})
	}

	seen := make(map[string]struct{}, len(in))
	out := make([]ChunkedFile, 0, len(in))
	for _, cf := range in {
		if err := ValidateRelPath(cf.RelPath); err != nil {
			return nil, fmt.Errorf("media: chunked file %q: %w", cf.RelPath, err)
		}
		if cf.WholeSize <= 0 {
			return nil, fmt.Errorf("media: chunked file %q has non-positive whole size %d", cf.RelPath, cf.WholeSize)
		}
		if len(cf.WholeSHA256) != sha256HexLen {
			return nil, fmt.Errorf("media: chunked file %q whole-file hash is %d bytes, want %d",
				cf.RelPath, len(cf.WholeSHA256), sha256HexLen)
		}
		if _, dup := seen[cf.RelPath]; dup {
			return nil, fmt.Errorf("media: duplicate chunked file record for %q", cf.RelPath)
		}
		seen[cf.RelPath] = struct{}{}

		parts := byParent[cf.RelPath]
		if len(parts) == 0 {
			return nil, fmt.Errorf(
				"media: chunked file %q has a reassembly record but no chunk entries among this image's payload "+
					"(a parent only appears in a manifest when this image holds at least one of its chunks)",
				cf.RelPath)
		}
		// Media-path order equals ascending 5-digit name order, but sort
		// by index explicitly: array order IS reassembly order (file doc),
		// and that guarantee must not depend on the path sort above.
		sort.Slice(parts, func(i, j int) bool { return parts[i].Index < parts[j].Index })

		out = append(out, ChunkedFile{
			RelPath: cf.RelPath,
			Size:    cf.WholeSize,
			SHA256:  hex.EncodeToString(cf.WholeSHA256),
			Chunks:  parts,
		})
		delete(byParent, cf.RelPath)
	}
	if len(byParent) > 0 {
		var orphans []string
		for parent := range byParent {
			orphans = append(orphans, parent)
		}
		sort.Strings(orphans)
		return nil, fmt.Errorf(
			"media: chunk entries for %v have no chunked-file record (pass one ChunkedFileInput per chunked parent on this image)",
			orphans)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RelPath < out[j].RelPath })
	return out, nil
}

// buildHardLinkGroups validates and sorts the D-04 families: all names
// ascending, groups ordered by their first (lowest) name.
func buildHardLinkGroups(in []HardLinkGroupInput) ([]HardLinkGroup, error) {
	out := make([]HardLinkGroup, 0, len(in))
	for _, g := range in {
		if len(g.SHA256) != sha256HexLen {
			return nil, fmt.Errorf("media: hard-link group hash is %d bytes, want %d", len(g.SHA256), sha256HexLen)
		}
		if len(g.Names) < 2 {
			return nil, fmt.Errorf("media: hard-link group %v has %d names, want >= 2 (a family of one is not a family)",
				g.Names, len(g.Names))
		}
		names := make([]string, len(g.Names))
		seen := make(map[string]struct{}, len(g.Names))
		for i, n := range g.Names {
			if err := ValidateRelPath(n); err != nil {
				return nil, fmt.Errorf("media: hard-link group name: %w", err)
			}
			if _, dup := seen[n]; dup {
				return nil, fmt.Errorf("media: hard-link group repeats name %q", n)
			}
			seen[n] = struct{}{}
			names[i] = n
		}
		sort.Strings(names)
		out = append(out, HardLinkGroup{
			SHA256: hex.EncodeToString(g.SHA256),
			Names:  names,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Names[0] < out[j].Names[0] })
	return out, nil
}

// buildPlacement validates and sorts the whole-set summary: ascending
// image number, duplicate-free ascending paths per image.
func buildPlacement(in []PlacementInput, ownImage int64) ([]ImagePlacement, error) {
	out := make([]ImagePlacement, 0, len(in))
	seenImg := make(map[int64]struct{}, len(in))
	for _, p := range in {
		if p.ImageNumber < 1 {
			return nil, fmt.Errorf("media: placement record for image number %d must be >= 1", p.ImageNumber)
		}
		if _, dup := seenImg[p.ImageNumber]; dup {
			return nil, fmt.Errorf("media: duplicate placement record for image %d", p.ImageNumber)
		}
		seenImg[p.ImageNumber] = struct{}{}
		paths := make([]string, len(p.Paths))
		seenPath := make(map[string]struct{}, len(p.Paths))
		for i, rel := range p.Paths {
			if err := ValidateRelPath(rel); err != nil {
				return nil, fmt.Errorf("media: placement path of image %d: %w", p.ImageNumber, err)
			}
			if _, dup := seenPath[rel]; dup {
				return nil, fmt.Errorf("media: placement of image %d repeats path %q", p.ImageNumber, rel)
			}
			seenPath[rel] = struct{}{}
			paths[i] = rel
		}
		sort.Strings(paths)
		out = append(out, ImagePlacement{ImageNumber: p.ImageNumber, Paths: paths})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ImageNumber < out[j].ImageNumber })

	if _, ok := seenImg[ownImage]; !ok {
		return nil, fmt.Errorf(
			"media: placement summary has no record for image %d (every image's manifest carries the whole-set placement, FR-033)",
			ownImage)
	}
	return out, nil
}

// checkPlacementCoversEntries is the one-directional consistency gate:
// every payload entry of this image must be placed on this image. The
// reverse is deliberately NOT required - the summary legitimately
// lists paths only other images hold (chunked parents spread across
// images; secondaries only materialized on one image).
func checkPlacementCoversEntries(placement []ImagePlacement, ownImage int64, entries []Entry) error {
	var own map[string]struct{}
	for _, p := range placement {
		if p.ImageNumber == ownImage {
			set := make(map[string]struct{}, len(p.Paths))
			for _, rel := range p.Paths {
				set[rel] = struct{}{}
			}
			own = set
			break
		}
	}
	for _, e := range entries {
		rel := e.ParentPath // chunks: check the parent relpath
		if e.Kind != KindChunk {
			rel = strings.TrimPrefix(e.MediaPath, FilesRoot+"/")
		}
		if _, ok := own[rel]; !ok {
			return fmt.Errorf(
				"media: entry %q (source path %q) is on image %d but the placement summary does not list it there",
				e.MediaPath, rel, ownImage)
		}
	}
	return nil
}

// MarshalManifest renders m as the MANIFEST.json bytes: two-space
// indented (a human-readable artifact, DESIGN_SPEC 6.6), one trailing
// newline (POSIX text-file convention). Field order inside objects is
// the Manifest struct's declaration order (file doc); object key order
// is therefore fixed, and GenerateManifest has sorted every slice, so
// equal inputs render byte-identical output.
func MarshalManifest(m Manifest) ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("media: marshaling manifest for set %q image %d: %w", m.SetID, m.ImageNumber, err)
	}
	return append(b, '\n'), nil
}
