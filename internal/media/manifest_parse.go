package media

// T5.5: MANIFEST.json PARSING (DESIGN_SPEC 8.5's quick-introspection
// layer; IMPLEMENTATION_PLAN 4.6 T5.5). ParseManifest is the read-side
// mirror of MarshalManifest: the media-alone restore reads a media
// tree's manifest BEFORE opening its catalog export (set id, image
// number, per-image entries, the whole-set placement - "what images
// exist, what a selection would need" without the export), and this
// function is the one place that knows the wire format on the way IN.
//
// VALIDATION POLICY (deliberately the mirror image of the generator's
// strictness): GenerateManifest refuses everything that would land a
// LIE on media; ParseManifest refuses everything that cannot be a
// faithful manifest VALUE, and nothing else:
//
//   - the bytes must be valid JSON carrying a MANIFEST object;
//   - set_id must be a usable path component (validateSetID - the same
//     gate generation applies, including the UTF-8 fidelity rule);
//   - image_number must be >= 1 and schema_version >= 1 (the 6.6
//     header fields; a document without them is not a manifest of this
//     tool).
//
// UNKNOWN FIELDS ARE TOLERATED on purpose: a media tree may outlive
// this binary by a decade (FR-035's motivation), so a manifest written
// by a FUTURE tool version must still introspect - the hard version
// gate belongs to the EXPORT's user_version (8.5's schema-version
// check), not to the human-readable sidecar. Entry-level payloads are
// NOT re-validated against the generator's invariants either (hash
// shapes, placement coverage): introspection READS the document; the
// restore engine's truth is the export's catalog rows, verified per
// member against their hashes (FR-041) - the manifest never becomes a
// data source for bytes.

import (
	"encoding/json"
	"fmt"
)

// ParseManifest reads one MANIFEST.json document (see the file doc for
// the validation policy). The returned Manifest is the same value
// shape MarshalManifest writes, so tests can round-trip it.
func ParseManifest(data []byte) (Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("media: parsing MANIFEST.json: %w", err)
	}
	if err := validateSetID(m.SetID); err != nil {
		return Manifest{}, fmt.Errorf("media: MANIFEST.json set_id: %w", err)
	}
	if m.ImageNumber < 1 {
		return Manifest{}, fmt.Errorf("media: MANIFEST.json image_number %d must be >= 1 (DESIGN_SPEC 6.4)", m.ImageNumber)
	}
	if m.SchemaVersion < 1 {
		return Manifest{}, fmt.Errorf("media: MANIFEST.json schema_version %d must be >= 1 (DESIGN_SPEC 6.6)", m.SchemaVersion)
	}
	return m, nil
}
