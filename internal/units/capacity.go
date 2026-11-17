package units

// The canonical image-capacity estimator (DESIGN_SPEC 4.3 capacity
// accounting, as amended 2026-10-20; 6.3 step 5). This is the ONE
// arithmetic the planner (internal/plan, the WFD balancer's reserved
// budget) and the builder (internal/imagebuild, the 6.3 step-5
// pre-check) share. It lives here, in internal/units, because units is
// the established leaf for size math (DESIGN_SPEC 1.3: size parsing,
// formatting, and the D-06 capacity profiles) - both services already
// depend on it, so the estimator is shared by a plain import with no
// layering inversion and no cross-package duplication to drift.
//
// The stakeholder failure this fixes (2026-10-20): the planner packed
// destinations to the RAW usable_size while the builder's pre-check
// required payload + overhead <= usable_size - two different budgets
// over the same number, so a well-packed plan was routinely refused by
// the builder at build time. Since the amendment the planner RESERVES
// this overhead at fill time (an image opens with
// usable - FixedImageOverhead remaining and every placement charges
// size + ImageEntryOverhead), so every planner-produced image satisfies
// RequiredImageBytes(payload, entries) <= usable_size by construction.

import "math"

const (
	// FixedImageOverhead is the fixed half of the conservative ISO
	// metadata overhead of 6.3 step 5: 2 MiB (ISO system area, path
	// tables, Joliet twin tree, staging scratch metadata) PLUS the
	// 512-byte entries of the two members every image carries
	// regardless of payload (MANIFEST.json and catalog-export.sqlite).
	// The +2 moved into this fixed term (from the old
	// 2 MiB + 512*(payloadEntries+2) spelling) so the planner can
	// reserve the full fixed cost the moment it CREATES an image -
	// before any payload items exist. Value: 2,097,152 + 1,024.
	FixedImageOverhead int64 = 2<<20 + 512*2

	// ImageEntryOverhead is the per-payload-member half of the 6.3
	// step 5 overhead: 512 bytes per file, chunk, symlink, and empty
	// dir staged onto the image (directory records and Rock Ridge
	// system-use entries).
	ImageEntryOverhead int64 = 512
)

// RequiredImageBytes is THE canonical capacity estimator of DESIGN_SPEC
// 4.3/6.3:
//
//	RequiredImageBytes(payloadBytes, payloadEntries) =
//		payloadBytes + FixedImageOverhead + ImageEntryOverhead*payloadEntries
//
// where payloadEntries counts the payload items on the image (files +
// chunks + symlinks + empty dirs; the manifest and catalog-export
// entries are inside FixedImageOverhead). This is numerically
// IDENTICAL to the pre-amendment 6.3 spelling
// `payload + 2 MiB + 512*(payloadEntries+2)` - only the "+2 entries"
// moved into the fixed term (asserted across a value matrix by
// TestRequiredImageBytesNumericalIdentity in units_test.go, which is
// what makes the re-basing of internal/imagebuild's old estimator a
// provable no-op on totals).
//
// The planner's balancer and the builder's pre-check MUST both derive
// their budgets from this function (plan/balancer.go and
// imagebuild/staging.go import it directly), so the two budgets can
// never drift apart again.
//
// SATURATION CONTRACT (carried over from the old imagebuild
// estimator): the estimator is TOTAL over int64 - it never wraps. A
// corrupt-catalog payload near math.MaxInt64 would otherwise overflow
// payload + overhead to a NEGATIVE figure, which the builder's
// pre-check would then vacuously pass. Instead, when the sum cannot be
// represented the estimator returns math.MaxInt64: the image is
// UNFAILABLY over capacity and is refused up front with a positive,
// printable byte figure.
func RequiredImageBytes(payloadBytes, payloadEntries int64) int64 {
	if payloadEntries < 0 {
		// Defensive: a negative count would subtract overhead. The
		// caller's accounting bug, clamped rather than wrapped.
		payloadEntries = 0
	}
	// Defensive: an entry count whose per-entry product alone exceeds
	// MaxInt64 - fixed saturates before the multiply can wrap.
	if payloadEntries > (math.MaxInt64-FixedImageOverhead)/ImageEntryOverhead {
		return math.MaxInt64
	}
	overhead := FixedImageOverhead + payloadEntries*ImageEntryOverhead
	// Saturation, not wrap: a payload above MaxInt64 - overhead cannot
	// be represented, and the honest figure is "over capacity by more
	// than int64 can say" = MaxInt64.
	if payloadBytes > math.MaxInt64-overhead {
		return math.MaxInt64
	}
	return payloadBytes + overhead
}
