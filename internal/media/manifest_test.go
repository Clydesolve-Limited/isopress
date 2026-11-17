package media

// T3.1 manifest tests (DESIGN_SPEC 6.6, FR-033, FR-035, D-04):
// golden MANIFEST.json bytes for a mixed payload and for a minimal
// image, determinism under input shuffling, the self-exclusion
// reading (the manifest never lists itself or the export), the
// hostile-relpath defense at manifest level, and the strict input
// validation of GenerateManifest.

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fixedCreatedAt is the injected clock for every golden test; rendered
// as RFC 3339Nano UTC ("2026-10-19T12:00:00Z" - no fractional part
// when the timestamp has none).
var fixedCreatedAt = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

// hashHex renders a fake 32-byte sha256 digest as lowercase hex: every
// byte is fill. Keeps goldens readable without embedding 64-char
// literals by hand.
func hashHex(fill byte) string {
	return strings.Repeat(fmt.Sprintf("%02x", fill), 32)
}

func hashBytes(fill byte) []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = fill
	}
	return b
}

// comprehensiveInput is the golden fixture: a mixed image with a whole
// file, a two-chunk chunked parent, a symlink, an empty dir, a D-04
// hard-link family, and a two-image placement summary. Slices are
// deliberately UNSORTED to pin GenerateManifest's own sorting.
func comprehensiveInput() ManifestInput {
	return ManifestInput{
		SetID:         "home-data-20261018",
		ImageNumber:   1,
		ToolVersion:   "0.1.0-dev", // catalog.ToolVersion, injected
		SchemaVersion: 1,           // catalog.SchemaVersion, injected
		CreatedAt:     fixedCreatedAt,
		Entries: []ImageEntry{
			{Kind: KindDir, RelPath: "empty-dir", Mode: 0755, UID: 1000, GID: 1000, MtimeNS: 1729340000000000001},
			{Kind: KindSymlink, RelPath: "docs/link", LinkTarget: "readme.txt", Size: 10,
				Mode: 0777, UID: 1000, GID: 1000, MtimeNS: 1729340000000000002},
			{Kind: KindFile, RelPath: "docs/readme.txt", Size: 12, SHA256: hashBytes(0x11),
				Mode: 0644, UID: 1000, GID: 1000, MtimeNS: 1729340000000000003},
			{Kind: KindChunk, RelPath: "big/archive.bin", ChunkIndex: 1, Size: 20, SHA256: hashBytes(0x33)},
			{Kind: KindChunk, RelPath: "big/archive.bin", ChunkIndex: 0, Size: 30, SHA256: hashBytes(0x22)},
		},
		ChunkedFiles: []ChunkedFileInput{
			{RelPath: "big/archive.bin", WholeSize: 50, WholeSHA256: hashBytes(0x00)},
		},
		HardLinkGroups: []HardLinkGroupInput{
			{SHA256: hashBytes(0x44), Names: []string{"data/dup-b", "data/dup-a"}},
		},
		Placement: []PlacementInput{
			{ImageNumber: 2, Paths: []string{"other/stuff.bin"}},
			{ImageNumber: 1, Paths: []string{
				"empty-dir", "docs/link", "docs/readme.txt", "big/archive.bin", "data/dup-b", "data/dup-a",
			}},
		},
	}
}

// TestGenerateManifestGolden pins the exact MANIFEST.json bytes for the
// comprehensive fixture. The struct field declaration order IS the
// format (manifest.go file doc); this golden is the regression anchor
// against accidental field reordering, omitted fields, or sort drift.
func TestGenerateManifestGolden(t *testing.T) {
	m, err := GenerateManifest(comprehensiveInput())
	if err != nil {
		t.Fatalf("GenerateManifest: %v", err)
	}
	got, err := MarshalManifest(m)
	if err != nil {
		t.Fatalf("MarshalManifest: %v", err)
	}

	want := fmt.Sprintf(`{
  "set_id": "home-data-20261018",
  "image_number": 1,
  "tool_version": "0.1.0-dev",
  "schema_version": 1,
  "created_at": "2026-10-19T12:00:00Z",
  "entries": [
    {
      "path": "chunks/big/archive.bin/00001",
      "kind": "chunk",
      "size": 30,
      "sha256": %q,
      "mtime_ns": 0,
      "mode": 0,
      "uid": 0,
      "gid": 0,
      "parent": "big/archive.bin",
      "chunk_index": 0
    },
    {
      "path": "chunks/big/archive.bin/00002",
      "kind": "chunk",
      "size": 20,
      "sha256": %q,
      "mtime_ns": 0,
      "mode": 0,
      "uid": 0,
      "gid": 0,
      "parent": "big/archive.bin",
      "chunk_index": 1
    },
    {
      "path": "files/docs/link",
      "kind": "symlink",
      "size": 10,
      "mtime_ns": 1729340000000000002,
      "mode": 511,
      "uid": 1000,
      "gid": 1000,
      "link_target": "readme.txt"
    },
    {
      "path": "files/docs/readme.txt",
      "kind": "file",
      "size": 12,
      "sha256": %q,
      "mtime_ns": 1729340000000000003,
      "mode": 420,
      "uid": 1000,
      "gid": 1000
    },
    {
      "path": "files/empty-dir",
      "kind": "dir",
      "size": 0,
      "mtime_ns": 1729340000000000001,
      "mode": 493,
      "uid": 1000,
      "gid": 1000
    }
  ],
  "chunked_files": [
    {
      "path": "big/archive.bin",
      "size": 50,
      "sha256": %q,
      "chunks": [
        {
          "index": 0,
          "length": 30,
          "sha256": %q
        },
        {
          "index": 1,
          "length": 20,
          "sha256": %q
        }
      ]
    }
  ],
  "hard_link_groups": [
    {
      "sha256": %q,
      "names": [
        "data/dup-a",
        "data/dup-b"
      ]
    }
  ],
  "placement": [
    {
      "image": 1,
      "paths": [
        "big/archive.bin",
        "data/dup-a",
        "data/dup-b",
        "docs/link",
        "docs/readme.txt",
        "empty-dir"
      ]
    },
    {
      "image": 2,
      "paths": [
        "other/stuff.bin"
      ]
    }
  ]
}
`,
		hashHex(0x22), hashHex(0x33), // entries: chunk 00001, chunk 00002
		hashHex(0x11),                // files/docs/readme.txt
		hashHex(0x00),                // chunked_files: whole-file anchor
		hashHex(0x22), hashHex(0x33), // chunk parts in reassembly order
		hashHex(0x44), // hard_link_groups
	)

	if string(got) != want {
		t.Errorf("golden mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestGenerateManifestMinimalGolden pins an image with a single
// zero-length file: the three non-entry sections render as empty
// arrays (the shape never changes with the data), and all-zero
// metadata is emitted verbatim.
func TestGenerateManifestMinimalGolden(t *testing.T) {
	in := ManifestInput{
		SetID:         "S",
		ImageNumber:   2,
		ToolVersion:   "0.1.0-dev",
		SchemaVersion: 1,
		CreatedAt:     fixedCreatedAt,
		Entries: []ImageEntry{
			{Kind: KindFile, RelPath: "a.txt", Size: 0, SHA256: hashBytes(0x55)},
		},
		Placement: []PlacementInput{
			{ImageNumber: 2, Paths: []string{"a.txt"}},
		},
	}
	m, err := GenerateManifest(in)
	if err != nil {
		t.Fatalf("GenerateManifest: %v", err)
	}
	got, err := MarshalManifest(m)
	if err != nil {
		t.Fatalf("MarshalManifest: %v", err)
	}

	want := fmt.Sprintf(`{
  "set_id": "S",
  "image_number": 2,
  "tool_version": "0.1.0-dev",
  "schema_version": 1,
  "created_at": "2026-10-19T12:00:00Z",
  "entries": [
    {
      "path": "files/a.txt",
      "kind": "file",
      "size": 0,
      "sha256": %q,
      "mtime_ns": 0,
      "mode": 0,
      "uid": 0,
      "gid": 0
    }
  ],
  "chunked_files": [],
  "hard_link_groups": [],
  "placement": [
    {
      "image": 2,
      "paths": [
        "a.txt"
      ]
    }
  ]
}
`, hashHex(0x55))

	if string(got) != want {
		t.Errorf("minimal golden mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestGenerateManifestDeterministic: equal inputs render byte-identical
// JSON, and the ORDER THE CALLER SUPPLIES the slices in is irrelevant
// (GenerateManifest sorts everything; the map usage is for duplicate
// detection only, never for output ordering).
func TestGenerateManifestDeterministic(t *testing.T) {
	base := comprehensiveInput()
	baseBytes := marshalOrFail(t, base)

	// Re-run with freshly built identical input.
	again := marshalOrFail(t, comprehensiveInput())
	if !bytes.Equal(baseBytes, again) {
		t.Fatalf("second run differs:\n--- first ---\n%s\n--- second ---\n%s", baseBytes, again)
	}

	// Shuffle every input slice; output must not change.
	shuffled := comprehensiveInput()
	reverseEntries := make([]ImageEntry, len(shuffled.Entries))
	for i, e := range shuffled.Entries {
		reverseEntries[len(shuffled.Entries)-1-i] = e
	}
	shuffled.Entries = reverseEntries
	shuffled.Placement[0], shuffled.Placement[1] = shuffled.Placement[1], shuffled.Placement[0]
	// After the swap the image-1 record is at index 0; shuffle its paths.
	shuffled.Placement[0].Paths = []string{
		"big/archive.bin", "data/dup-b", "data/dup-a", "empty-dir", "docs/readme.txt", "docs/link",
	}
	shuffled.HardLinkGroups[0].Names = []string{"data/dup-a", "data/dup-b"}
	shuffledBytes := marshalOrFail(t, shuffled)
	if !bytes.Equal(baseBytes, shuffledBytes) {
		t.Errorf("shuffled input changed the output:\n--- sorted ---\n%s\n--- shuffled ---\n%s",
			baseBytes, shuffledBytes)
	}
}

func marshalOrFail(t *testing.T, in ManifestInput) []byte {
	t.Helper()
	m, err := GenerateManifest(in)
	if err != nil {
		t.Fatalf("GenerateManifest: %v", err)
	}
	b, err := MarshalManifest(m)
	if err != nil {
		t.Fatalf("MarshalManifest: %v", err)
	}
	return b
}

// TestGenerateManifestExcludesSelfEntries pins the 6.6 self-entry
// reading: MANIFEST.json lists payload entries only. The manifest and
// export self-entries belong to the image_contents TABLE, recorded by
// the writer at write time (T3.3/T4.3); a self-hash inside the
// manifest is arithmetically impossible anyway.
func TestGenerateManifestExcludesSelfEntries(t *testing.T) {
	m, err := GenerateManifest(comprehensiveInput())
	if err != nil {
		t.Fatalf("GenerateManifest: %v", err)
	}
	for _, e := range m.Entries {
		if e.MediaPath == ManifestFileName || e.MediaPath == ExportFileName {
			t.Errorf("manifest lists its own member %q; payload entries only", e.MediaPath)
		}
	}
	b, err := MarshalManifest(m)
	if err != nil {
		t.Fatalf("MarshalManifest: %v", err)
	}
	for _, s := range []string{ManifestFileName, ExportFileName} {
		if strings.Contains(string(b), s) {
			t.Errorf("marshaled manifest mentions %q; self-entries belong in image_contents only", s)
		}
	}

	// And the generator REFUSES manifest/export input kinds outright,
	// with a message naming the correct home for them.
	for _, kind := range []string{"manifest", "export"} {
		in := comprehensiveInput()
		in.Entries = append(in.Entries, ImageEntry{Kind: kind, RelPath: ManifestFileName})
		_, err := GenerateManifest(in)
		if err == nil || !strings.Contains(err.Error(), "image_contents") {
			t.Errorf("entry kind %q: err = %v, want refusal naming image_contents", kind, err)
		}
	}
}

// TestGenerateManifestHostileInputs: the in-bad-catalog defense holds
// at manifest level - hostile relpaths in entries, chunked records,
// hard-link names, and placement are all refused via ErrUnsafeRelPath
// (the manifest generator validates through the SAME layout.go
// builders, so the tree can never encode them).
func TestGenerateManifestHostileInputs(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ManifestInput)
	}{
		{"entry absolute", func(in *ManifestInput) {
			in.Entries[2].RelPath = "/etc/passwd"
		}},
		{"entry traversal", func(in *ManifestInput) {
			in.Entries[2].RelPath = "../../evil"
		}},
		{"chunk parent traversal", func(in *ManifestInput) {
			in.Entries[4].RelPath = "big/../../evil"
		}},
		{"chunked record traversal", func(in *ManifestInput) {
			in.ChunkedFiles[0].RelPath = ".."
		}},
		{"hard-link name hostile", func(in *ManifestInput) {
			in.HardLinkGroups[0].Names = []string{"data/dup-a", "../escape"}
		}},
		{"placement path absolute", func(in *ManifestInput) {
			in.Placement[1].Paths = append(in.Placement[1].Paths, "/abs")
		}},
		{"placement path traversal", func(in *ManifestInput) {
			in.Placement[1].Paths[0] = "a/../b"
		}},
		// The UTF-8 fidelity gates (UTF-8 invariant, manifest.go file
		// doc): raw-byte values cannot be faithfully recorded in the
		// JSON manifest, so all three are refused upstream with the
		// same sentinel class - the writers then warn-and-skip such
		// rows per the unreadable-class convention.
		{"entry non-UTF-8 name", func(in *ManifestInput) {
			in.Entries[2].RelPath = "bad\xffname.txt"
		}},
		{"symlink target non-UTF-8", func(in *ManifestInput) {
			in.Entries[1].LinkTarget = "read\xffme.txt"
			in.Entries[1].Size = int64(len("read\xffme.txt")) // shape otherwise valid: UTF-8 is the only failing gate
		}},
		{"set id non-UTF-8", func(in *ManifestInput) {
			in.SetID = "home-\xffdata"
		}},
	}
	for _, c := range cases {
		in := comprehensiveInput()
		c.mutate(&in)
		_, err := GenerateManifest(in)
		if !errors.Is(err, ErrUnsafeRelPath) {
			t.Errorf("%s: err = %v, want ErrUnsafeRelPath", c.name, err)
		}
	}
	// Each raw-byte refusal names its reason, distinguishable from the
	// NUL case and from each other (entry name vs link target vs set id).
	for _, c := range []struct {
		name    string
		mutate  func(*ManifestInput)
		wantSub string
	}{
		{"entry non-UTF-8 name", func(in *ManifestInput) { in.Entries[2].RelPath = "bad\xffname.txt" }, "not valid UTF-8"},
		{"symlink target non-UTF-8", func(in *ManifestInput) {
			in.Entries[1].LinkTarget = "read\xffme.txt"
			in.Entries[1].Size = int64(len("read\xffme.txt"))
		}, "link target that is not valid UTF-8"},
		{"set id non-UTF-8", func(in *ManifestInput) { in.SetID = "home-\xffdata" }, "set id"},
	} {
		in := comprehensiveInput()
		c.mutate(&in)
		_, err := GenerateManifest(in)
		if err == nil || !strings.Contains(err.Error(), c.wantSub) {
			t.Errorf("%s: err = %v, want it to contain %q", c.name, err, c.wantSub)
		}
	}
}

// TestGenerateManifestValidation covers the strict input contract: each
// case is a mutation that must be refused with a message naming the
// cause.
func TestGenerateManifestValidation(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*ManifestInput)
		wantSub string
	}{
		{"empty set id", func(in *ManifestInput) { in.SetID = "" }, "empty set id"},
		{"set id with slash", func(in *ManifestInput) { in.SetID = "a/b" }, "slash"},
		{"image number zero", func(in *ManifestInput) { in.ImageNumber = 0 }, "image number"},
		{"empty tool version", func(in *ManifestInput) { in.ToolVersion = "" }, "tool version"},
		{"schema version zero", func(in *ManifestInput) { in.SchemaVersion = 0 }, "schema version"},
		{"zero clock", func(in *ManifestInput) { in.CreatedAt = time.Time{} }, "CreatedAt"},
		{"unknown kind", func(in *ManifestInput) { in.Entries[2].Kind = "device" }, "unknown entry kind"},
		{"file without hash", func(in *ManifestInput) { in.Entries[2].SHA256 = nil }, "no sha256"},
		{"file short hash", func(in *ManifestInput) { in.Entries[2].SHA256 = []byte{1, 2, 3} }, "want 32"},
		{"file negative size", func(in *ManifestInput) { in.Entries[2].Size = -1 }, "negative size"},
		{"chunk without hash", func(in *ManifestInput) { in.Entries[3].SHA256 = nil }, "no sha256"},
		{"chunk negative index", func(in *ManifestInput) { in.Entries[3].ChunkIndex = -1 }, "negative catalog index"},
		{"chunk zero length", func(in *ManifestInput) { in.Entries[3].Size = 0 }, "non-positive length"},
		{"symlink with hash", func(in *ManifestInput) { in.Entries[1].SHA256 = hashBytes(0x99) }, "NULL-hash"},
		{"symlink size mismatch", func(in *ManifestInput) { in.Entries[1].Size = 8 }, "len(target)"},
		{"dir with hash", func(in *ManifestInput) { in.Entries[0].SHA256 = hashBytes(0x99) }, "no content hash"},
		{"dir with size", func(in *ManifestInput) { in.Entries[0].Size = 5 }, "no content size"},
		{"duplicate media path", func(in *ManifestInput) {
			in.Entries = append(in.Entries, ImageEntry{
				Kind: KindFile, RelPath: "docs/readme.txt", Size: 12, SHA256: hashBytes(0x11),
			})
		}, "duplicate media path"},
		{"chunked parent without parts", func(in *ManifestInput) {
			in.ChunkedFiles = append(in.ChunkedFiles, ChunkedFileInput{
				RelPath: "never-on-this-image.bin", WholeSize: 10, WholeSHA256: hashBytes(0x66),
			})
		}, "no chunk entries"},
		{"orphan chunk entries", func(in *ManifestInput) { in.ChunkedFiles = nil }, "no chunked-file record"},
		{"chunked whole size zero", func(in *ManifestInput) { in.ChunkedFiles[0].WholeSize = 0 }, "non-positive whole size"},
		{"chunked whole hash short", func(in *ManifestInput) { in.ChunkedFiles[0].WholeSHA256 = nil }, "want 32"},
		{"duplicate chunked record", func(in *ManifestInput) {
			in.ChunkedFiles = append(in.ChunkedFiles, in.ChunkedFiles[0])
		}, "duplicate chunked file record"},
		{"hard-link group one name", func(in *ManifestInput) {
			in.HardLinkGroups[0].Names = []string{"data/dup-a"}
		}, "want >= 2"},
		{"hard-link group dup name", func(in *ManifestInput) {
			in.HardLinkGroups[0].Names = []string{"data/dup-a", "data/dup-a"}
		}, "repeats name"},
		{"hard-link group short hash", func(in *ManifestInput) { in.HardLinkGroups[0].SHA256 = nil }, "want 32"},
		{"placement without own image", func(in *ManifestInput) { in.Placement = in.Placement[:1] }, "no record for image 1"},
		{"placement duplicate image", func(in *ManifestInput) {
			in.Placement = append(in.Placement, in.Placement[1])
		}, "duplicate placement record"},
		{"placement duplicate path", func(in *ManifestInput) {
			in.Placement[1].Paths = append(in.Placement[1].Paths, "docs/link")
		}, "repeats path"},
		{"placement image number zero", func(in *ManifestInput) { in.Placement[0].ImageNumber = 0 }, "must be >= 1"},
		{"entry missing from own placement", func(in *ManifestInput) {
			var paths []string
			for _, p := range in.Placement[1].Paths {
				if p != "docs/link" {
					paths = append(paths, p)
				}
			}
			in.Placement[1].Paths = paths
		}, "placement summary does not list it"},
	}
	for _, c := range cases {
		in := comprehensiveInput()
		c.mutate(&in)
		_, err := GenerateManifest(in)
		if err == nil {
			t.Errorf("%s: GenerateManifest succeeded, want error", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.wantSub) {
			t.Errorf("%s: err = %v, want it to contain %q", c.name, err, c.wantSub)
		}
	}
}

// TestGenerateManifestChunkIndexRecording pins the chunk-index
// recording choice: the CATALOG 0-based index lands in chunk_index
// (index 0 is emitted, not dropped by omitempty - hence the pointer),
// and the 1-based media name appears only as the derived "path"
// (chunks/<relpath>/00001), never as a second index field that could
// drift.
func TestGenerateManifestChunkIndexRecording(t *testing.T) {
	m, err := GenerateManifest(comprehensiveInput())
	if err != nil {
		t.Fatalf("GenerateManifest: %v", err)
	}
	first := m.Entries[0]
	if first.Kind != KindChunk || first.ParentPath != "big/archive.bin" {
		t.Fatalf("first entry = %+v, want chunk of big/archive.bin", first)
	}
	if first.ChunkIndex == nil || *first.ChunkIndex != 0 {
		t.Errorf("chunk_index = %v, want pointer to catalog index 0", first.ChunkIndex)
	}
	if first.MediaPath != "chunks/big/archive.bin/00001" {
		t.Errorf("media path = %q, want 1-based 00001", first.MediaPath)
	}
	if strings.Contains(string(mustMarshal(t, m)), `"media_index"`) {
		t.Error("manifest carries a redundant media index field; the name is derived from chunk_index")
	}
}

// TestGenerateManifestTimezoneInjection: a non-UTC injected clock is
// normalized to UTC so two hosts in different zones produce identical
// bytes for identical instants.
func TestGenerateManifestTimezoneInjection(t *testing.T) {
	loc := time.FixedZone("CEST", 2*3600)
	in := comprehensiveInput()
	in.CreatedAt = fixedCreatedAt.In(loc)
	m, err := GenerateManifest(in)
	if err != nil {
		t.Fatalf("GenerateManifest: %v", err)
	}
	if m.CreatedAt != "2026-10-19T12:00:00Z" {
		t.Errorf("CreatedAt = %q, want UTC rendering 2026-10-19T12:00:00Z", m.CreatedAt)
	}
}

func mustMarshal(t *testing.T, m Manifest) []byte {
	t.Helper()
	b, err := MarshalManifest(m)
	if err != nil {
		t.Fatalf("MarshalManifest: %v", err)
	}
	return b
}
