package media

// T5.5: ParseManifest, the read-side mirror (see manifest_parse.go for
// the validation policy). The round-trip pin matters most: whatever
// MarshalManifest writes must parse back to the same value - the
// media-alone restore (8.5) reads the very bytes generation wrote,
// potentially a decade later.

import (
	"strings"
	"testing"
	"time"
)

// parseGoldenManifest builds the smallest valid manifest input (one
// file entry placed on its own image).
func parseGoldenManifest(t *testing.T) Manifest {
	t.Helper()
	hash := make([]byte, 32)
	m, err := GenerateManifest(ManifestInput{
		SetID:         "S-PARSE",
		ImageNumber:   1,
		ToolVersion:   "test",
		SchemaVersion: 1,
		CreatedAt:     time.Unix(1_700_000_000, 0),
		Entries: []ImageEntry{{
			Kind: KindFile, RelPath: "a.txt", Size: 3, SHA256: hash,
		}},
		Placement: []PlacementInput{{ImageNumber: 1, Paths: []string{"a.txt"}}},
	})
	if err != nil {
		t.Fatalf("generating: %v", err)
	}
	return m
}

func TestParseManifestRoundTrip(t *testing.T) {
	want := parseGoldenManifest(t)
	blob, err := MarshalManifest(want)
	if err != nil {
		t.Fatalf("marshaling: %v", err)
	}
	got, err := ParseManifest(blob)
	if err != nil {
		t.Fatalf("parsing the marshaled manifest: %v", err)
	}
	if got.SetID != want.SetID || got.ImageNumber != want.ImageNumber ||
		got.SchemaVersion != want.SchemaVersion || got.CreatedAt != want.CreatedAt {
		t.Errorf("header round-trip = %+v, want %+v", got, want)
	}
	if len(got.Entries) != 1 || got.Entries[0].MediaPath != "files/a.txt" || got.Entries[0].SHA256 != want.Entries[0].SHA256 {
		t.Errorf("entries round-trip = %+v, want the generated entry", got.Entries)
	}
	if len(got.Placement) != 1 || got.Placement[0].ImageNumber != 1 {
		t.Errorf("placement round-trip = %+v, want the whole-set record", got.Placement)
	}
}

func TestParseManifestRefusals(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"not JSON", `{not json`, "parsing MANIFEST.json"},
		{"not an object", `[]`, "parsing MANIFEST.json"},
		{"empty set id", `{"set_id":"","image_number":1,"schema_version":1}`, "set_id"},
		{"slash in set id", `{"set_id":"a/b","image_number":1,"schema_version":1}`, "slash"},
		{"zero image number", `{"set_id":"S","image_number":0,"schema_version":1}`, "image_number"},
		{"zero schema version", `{"set_id":"S","image_number":1,"schema_version":0}`, "schema_version"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseManifest([]byte(tc.raw))
			if err == nil {
				t.Fatal("ParseManifest accepted a document it must refuse")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q lacks %q", err, tc.want)
			}
		})
	}
}

// A FUTURE manifest (extra fields from a newer tool) must still
// introspect: the hard version gate belongs to the export's
// user_version (8.5), never to the human-readable sidecar.
func TestParseManifestToleratesUnknownFields(t *testing.T) {
	raw := `{"set_id":"S-FUT","image_number":2,"schema_version":1,"future_section":[{"x":1}],"entries":[],"chunked_files":[],"hard_link_groups":[],"placement":[]}`
	m, err := ParseManifest([]byte(raw))
	if err != nil {
		t.Fatalf("a future manifest must introspect: %v", err)
	}
	if m.SetID != "S-FUT" || m.ImageNumber != 2 {
		t.Errorf("parsed = %+v, want S-FUT/2", m)
	}
}
