package ingest

// Change-detection mode threading (DESIGN_SPEC 3.5, RESOLVED 2026-10-20;
// detection point 1). THE PIN: the once-per-run resolved fsmeta.Mode
// the cli wires into NewFailures drives the post-hash comparison at
// EVERY classification - the ingest half of the three-point
// consistency matrix. A metadata-mode run does NOT flag dev/inode
// drift between the walk-time baseline and the post-hash lstat (the
// CIFS incident's false positive), while size/mtime_ns drift still
// classifies changed_during_scan; the zero value keeps the full set.

import (
	"testing"

	"isopress/internal/fsmeta"
)

// TestClassifyModeAwareComparison pins Classify's mode parameter at
// detection point 1: inode-only drift is invisible under a metadata
// mode and flagged under the full (zero-value) mode.
func TestClassifyModeAwareComparison(t *testing.T) {
	baseline := fsmeta.Meta{Size: 4096, MtimeNS: 1_000_000_000, Dev: 7, Inode: 111}
	result := okResult("doc.bin", 4096, 0x51)
	result.Meta = baseline

	// Synthesized-identifier drift (the CIFS shape): new dev+inode,
	// identical size+mtime_ns.
	drifted := fsmeta.Meta{Size: 4096, MtimeNS: 1_000_000_000, Dev: 8, Inode: 1_796_385}

	full := Classify(result, drifted, fsmeta.ModeFull)
	if full.Status != StatusChangedDuringScan {
		t.Fatalf("full mode: dev+inode drift classified %q, want %q", full.Status, StatusChangedDuringScan)
	}
	if full.Fields != (fsmeta.DiffDev | fsmeta.DiffInode) {
		t.Errorf("full mode: fields = %v, want dev+inode", full.Fields)
	}

	meta := Classify(result, drifted, fsmeta.Mode{Set: fsmeta.ComparisonMetadata})
	if meta.Status != StatusOK {
		t.Fatalf("metadata mode: dev+inode drift classified %q, want %q (synthesized identifiers are ignored, 3.5)", meta.Status, StatusOK)
	}

	// A real content-relevant change still fires in metadata mode.
	shrunk := fsmeta.Meta{Size: 10, MtimeNS: 1_000_000_000, Dev: 8, Inode: 5}
	if c := Classify(result, shrunk, fsmeta.Mode{Set: fsmeta.ComparisonMetadata}); c.Status != StatusChangedDuringScan {
		t.Fatalf("metadata mode: size drift classified %q, want %q", c.Status, StatusChangedDuringScan)
	}
}

// TestFailuresCarriesMode pins the accumulator wiring: Failures built
// with a metadata mode classifies through it (Process -> ClassifyPath
// -> DiffFields(mode)), while the zero-value Failures keeps the full
// comparison.
func TestFailuresCarriesMode(t *testing.T) {
	baseline := fsmeta.Meta{Size: 100, MtimeNS: 2_000_000_000, Dev: 7, Inode: 42}
	drifted := fsmeta.Meta{Size: 100, MtimeNS: 2_000_000_000, Dev: 9, Inode: 43}

	result := okResult("iso.img", 100, 0x61)
	result.Meta = baseline
	src := func(string) (fsmeta.Meta, error) { return drifted, nil }

	mf := NewFailures(nil, fsmeta.Mode{Set: fsmeta.ComparisonMetadata})
	if c := mf.Process(result, src); c.Status != StatusOK {
		t.Fatalf("metadata-mode Failures: inode drift classified %q, want %q", c.Status, StatusOK)
	}
	if mf.Len() != 0 {
		t.Errorf("metadata-mode Failures accumulated %d entries, want 0", mf.Len())
	}

	ff := NewFailures(nil, fsmeta.ModeFull) // zero-value-equivalent full mode
	if c := ff.Process(result, src); c.Status != StatusChangedDuringScan {
		t.Fatalf("full-mode Failures: inode drift classified %q, want %q", c.Status, StatusChangedDuringScan)
	}
	if ff.Len() != 1 {
		t.Errorf("full-mode Failures accumulated %d entries, want 1", ff.Len())
	}
}
