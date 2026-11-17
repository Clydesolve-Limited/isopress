package plan

// Capacity-floor boundaries of the 2026-10-20 amendment (DESIGN_SPEC
// 4.3 capacity accounting): validateProfile refuses a usable_size that
// cannot hold the reserved overhead plus ONE entry, and accepts the
// very next byte - where an image can hold exactly one 1-byte entry at
// the invariant's equality (RequiredImageBytes(1, 1) == usable).

import (
	"context"
	"errors"
	"testing"

	"isopress/internal/units"
)

func TestBalancerCapacityFloorBoundaries(t *testing.T) {
	floor := units.FixedImageOverhead + units.ImageEntryOverhead // 2,098,688

	// AT the floor: even one 1-byte entry would exceed usable
	// (RequiredImageBytes(1,1) = floor + 1 > floor), so no plan can ever
	// be buildable - refused up front.
	atFloor := units.Profile{Name: "disk", NominalBytes: 2 * floor, UsableBytes: floor}
	s := newFakeStore(testProfile(1200))
	if _, err := Balance(context.Background(), s, atFloor); !errors.Is(err, ErrInvalidProfile) {
		t.Fatalf("Balance at usable == Fixed+512 = %v, want ErrInvalidProfile", err)
	}

	// ONE BYTE ABOVE the floor: exactly one 1-byte entry fits, at the
	// invariant's EQUALITY. The plan must succeed and pin the tightest
	// buildable image: RequiredImageBytes(1, 1) == usable.
	justOver := units.Profile{Name: "disk", NominalBytes: 2 * (floor + 1), UsableBytes: floor + 1}
	s2 := newFakeStore(justOver)
	addFile(s2, 1, "a", 1)
	if _, err := runPlan(t, s2); err != nil {
		t.Fatalf("Balance at usable == Fixed+513: %v", err)
	}
	if got := imageOf(s2, 1); got != 1 {
		t.Fatalf("1-byte file placed on image %d, want the single image 1", got)
	}
	st := s2.stateOf(1)
	if got := units.RequiredImageBytes(st.UsedBytes, int64(st.ItemCount+st.DirCount)); got != justOver.UsableBytes {
		t.Errorf("RequiredImageBytes(%d, %d) = %d, want exact equality with usable %d",
			st.UsedBytes, st.ItemCount+st.DirCount, got, justOver.UsableBytes)
	}

	// A 2-byte file at the same profile cannot fit one image (the
	// effective chunk size is 1, and each 1-byte chunk's 513-byte
	// charge exhausts the 1-byte payload budget): it chunks onto TWO
	// images, each satisfying the invariant at equality.
	s3 := newFakeStore(justOver)
	addFile(s3, 1, "b", 2)
	if _, err := runPlan(t, s3); err != nil {
		t.Fatalf("Balance of a 2-byte file at usable == Fixed+513: %v", err)
	}
	if len(s3.chunkRows[1]) != 2 {
		t.Fatalf("2-byte file decomposed into %d chunks, want [1 1]", len(s3.chunkRows[1]))
	}
	for _, id := range []int64{1, 2} {
		st := s3.stateOf(id)
		if got := units.RequiredImageBytes(st.UsedBytes, int64(st.ItemCount+st.DirCount)); got != justOver.UsableBytes {
			t.Errorf("image %d: RequiredImageBytes(%d, %d) = %d, want exact equality with usable %d",
				id, st.UsedBytes, st.ItemCount+st.DirCount, got, justOver.UsableBytes)
		}
	}
}
