package units

// Tests for the canonical capacity estimator (capacity.go): exact
// value pins, the saturation contract, and - the crux of the 2026-10-20
// capacity amendment - the NUMERICAL IDENTITY between the old 6.3
// spelling (payload + 2 MiB + 512*(entries+2), which counted the two
// embedded image-root members in the entry term) and the new canonical
// form (payload + FixedImageOverhead + 512*payloadEntries, which folds
// those two entries into the fixed term). The identity is asserted
// across a value matrix so the re-basing of internal/imagebuild's old
// RequiredImageBytes onto this function is PROVABLY a no-op on totals.

import (
	"math"
	"testing"
)

func TestRequiredImageBytesPinnedValues(t *testing.T) {
	// The fixed term spelled out: 2 MiB + 512*2 = 2,098,176.
	if FixedImageOverhead != 2_098_176 {
		t.Errorf("FixedImageOverhead = %d, want 2098176 (2 MiB + two embedded entries)", FixedImageOverhead)
	}
	if ImageEntryOverhead != 512 {
		t.Errorf("ImageEntryOverhead = %d, want 512", ImageEntryOverhead)
	}
	cases := []struct {
		payload int64
		entries int64
		want    int64
	}{
		{0, 0, 2_098_176},                                     // fixed overhead alone
		{100, 1, 100 + 2_098_176 + 512},                       // one payload entry
		{1, 4, 1 + 2_098_176 + 4*512},                         // several entries
		{-5, -3, 2_098_176 - 5},                               // defensive clamps (negative payload passes through, entries clamp to 0)
		{math.MaxInt64 - 2_098_176 - 3*512, 3, math.MaxInt64}, // exact boundary: the true total overflows to MaxInt64
		{math.MaxInt64, 3, math.MaxInt64},                     // saturated, never wrapped negative
	}
	for _, tc := range cases {
		if got := RequiredImageBytes(tc.payload, tc.entries); got != tc.want {
			t.Errorf("RequiredImageBytes(%d, %d) = %d, want %d", tc.payload, tc.entries, got, tc.want)
		}
	}
	// Entry-count saturation: an absurd count saturates before the
	// multiply can wrap.
	if got := RequiredImageBytes(0, math.MaxInt64); got != math.MaxInt64 {
		t.Errorf("RequiredImageBytes(0, MaxInt64) = %d, want MaxInt64 (saturated, not wrapped)", got)
	}
}

// TestRequiredImageBytesNumericalIdentity is the re-basing proof: for
// every (payload, entries) in the matrix,
//
//	old 6.3 spelling: payload + (2<<20) + 512*(entries+2)
//	new canonical:    payload + FixedImageOverhead + 512*entries
//
// are EQUAL. internal/imagebuild's pre-amendment estimator computed
// exactly the left-hand side; it now calls RequiredImageBytes, and this
// matrix is what makes that substitution provably a no-op on the
// builder's totals (DESIGN_SPEC 4.3: "This is numerically identical to
// the 6.3 formula ... only the manifest/export entries moved into the
// fixed term").
func TestRequiredImageBytesNumericalIdentity(t *testing.T) {
	// The payloads are capped well below MaxInt64 so the OLD
	// left-hand-side spelling itself stays representable for every
	// entry count in the matrix (its saturation behavior is pinned
	// separately above; the identity is about the arithmetic, not the
	// clamp).
	payloads := []int64{0, 1, 511, 512, 1 << 20, 23_000_000_000, math.MaxInt64 / 4}
	entryCounts := []int64{0, 1, 2, 7, 9, 1000, 1 << 20}
	for _, payload := range payloads {
		for _, n := range entryCounts {
			old := payload + (2 << 20) + 512*(n+2)
			new := RequiredImageBytes(payload, n)
			if old != new {
				t.Fatalf("identity broken for payload=%d entries=%d: old spelling %d != canonical %d",
					payload, n, old, new)
			}
		}
	}
}
