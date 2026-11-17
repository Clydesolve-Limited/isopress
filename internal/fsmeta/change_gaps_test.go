package fsmeta

// Adversarial gap-closure pins for the 2026-10-20 change-detection
// amendment (DESIGN_SPEC 3.5, the D-10 row as amended). The existing
// change_test.go pins the COMBINED dev+inode masking and the
// size/mtime survival; this file pins the INDEPENDENT cases the brief
// calls out, so a future refactor can neither drop one masked field
// from the metadata set nor lose one field from the full set:
//
//   - dev-ONLY differences are ignored by the metadata set and fire
//     (alone) in the full set;
//   - inode-ONLY differences likewise;
//   - a masked identifier never suppresses a real content signal: a
//     dev difference PLUS a size difference fires DiffSize in the
//     metadata set, an inode difference PLUS an mtime difference fires
//     DiffMtimeNS;
//   - the full set fires ALL FOUR bits when all four fields differ.

import "testing"

// TestDiffFieldsIndependentIdentifierMasking pins the per-field masking
// of both comparison sets, one field at a time.
func TestDiffFieldsIndependentIdentifierMasking(t *testing.T) {
	base := Meta{Size: 100, MtimeNS: 1_000_000_000, Dev: 7, Inode: 99}
	meta := Mode{Set: ComparisonMetadata}

	// dev-ONLY: the synthesized-device half of the CIFS incident.
	devOnly := Meta{Size: 100, MtimeNS: 1_000_000_000, Dev: 8, Inode: 99}
	if d := DiffFields(base, devOnly, meta); d != 0 {
		t.Errorf("metadata mode: dev-only diff = %v, want 0 (dev is dropped, 3.5)", d)
	}
	if d := DiffFields(base, devOnly, ModeFull); d != DiffDev {
		t.Errorf("full mode: dev-only diff = %v, want %v", d, DiffDev)
	}

	// inode-ONLY: the synthesized-inode half.
	inodeOnly := Meta{Size: 100, MtimeNS: 1_000_000_000, Dev: 7, Inode: 1_796_385}
	if d := DiffFields(base, inodeOnly, meta); d != 0 {
		t.Errorf("metadata mode: inode-only diff = %v, want 0 (inode is dropped, 3.5)", d)
	}
	if d := DiffFields(base, inodeOnly, ModeFull); d != DiffInode {
		t.Errorf("full mode: inode-only diff = %v, want %v", d, DiffInode)
	}

	// A masked identifier can NEVER suppress a content signal: dev
	// noise + a size change fires DiffSize (and nothing else) in the
	// metadata set.
	devNoiseSize := Meta{Size: 101, MtimeNS: 1_000_000_000, Dev: 42, Inode: 42}
	if d := DiffFields(base, devNoiseSize, meta); d != DiffSize {
		t.Errorf("metadata mode: dev-noise + size diff = %v, want %v", d, DiffSize)
	}
	// inode noise + an mtime change likewise fires DiffMtimeNS alone.
	inodeNoiseMtime := Meta{Size: 100, MtimeNS: 2_000_000_000, Dev: 7, Inode: 42}
	if d := DiffFields(base, inodeNoiseMtime, meta); d != DiffMtimeNS {
		t.Errorf("metadata mode: inode-noise + mtime diff = %v, want %v", d, DiffMtimeNS)
	}

	// The full set fires ALL FOUR bits when every field differs - the
	// complete D-10 set, bit-exact.
	allFour := Meta{Size: 101, MtimeNS: 2_000_000_000, Dev: 8, Inode: 1_796_385}
	wantAll := DiffSize | DiffMtimeNS | DiffDev | DiffInode
	if d := DiffFields(base, allFour, ModeFull); d != wantAll {
		t.Errorf("full mode: all-four diff = %v, want %v", d, wantAll)
	}
	// The same pair under the metadata set can never name the dropped
	// fields (the combined pin of change_test.go, restated here against
	// the SAME all-four input for completeness).
	if d := DiffFields(base, allFour, meta); d != (DiffSize | DiffMtimeNS) {
		t.Errorf("metadata mode: all-four input = %v, want %v (dev/inode must never surface)", d, DiffSize|DiffMtimeNS)
	}
}
