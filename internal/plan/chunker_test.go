package plan

// T2.1: chunker tests (DESIGN_SPEC 12.2 boundary fixtures, 12.5
// property style, 4.2 boundary stability, 4.3 worked example; PLAN_SPEC
// FR-041 no-truncate guarantee). The chunker is pure arithmetic, so
// every "large" case uses logical sizes only - no test ever allocates
// or writes file CONTENT (the metadata-only guarantee is what makes
// that possible).

import (
	"go/parser"
	"go/token"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"isopress/internal/units"
)

const (
	gib       int64 = 1 << 30 // GiB in bytes
	defaultCS       = DefaultChunkSize
	// bd-r-sl usable_size (units.bdRSLUsableBytes, decimal per D-06).
	bdRSLUsable int64 = 23_000_000_000
)

// --- constants and effective-size arithmetic ---------------------------------

func TestDefaultChunkSizeExactValue(t *testing.T) {
	// Pin both the binary and the decimal spelling from the design
	// (DESIGN_SPEC 4.2: "20 GiB = 21,474,836,480 bytes").
	if DefaultChunkSize != 21_474_836_480 {
		t.Errorf("DefaultChunkSize = %d, want 21474836480", DefaultChunkSize)
	}
	if DefaultChunkSize != 20*gib {
		t.Errorf("DefaultChunkSize = %d, want 20<<30 = %d", DefaultChunkSize, 20*gib)
	}
}

func TestEffectiveChunkSizeArithmetic(t *testing.T) {
	// The 2026-10-20 capacity amendment: the effective size is capped
	// at the one-image PAYLOAD budget (usable - fixed overhead - entry
	// charge = units.FixedImageOverhead + units.ImageEntryOverhead),
	// not at the raw usable bytes - a chunk must fit an image that
	// also reserves the builder's 6.3 overhead, or the plan would be
	// unbuildable by construction.
	reserve := units.FixedImageOverhead + units.ImageEntryOverhead // 2,098,688
	cases := []struct {
		name   string
		usable int64
		want   int64
		why    string
	}{
		{
			name:   "bd-r-sl 23 GB usable",
			usable: bdRSLUsable,
			want:   defaultCS,
			why: "min(20 GiB = 21,474,836,480; 23 GB - 2,098,688 = 22,997,901,312) = 20 GiB: " +
				"the fixed default still caps large destinations",
		},
		{
			name:   "bd-r-dl 46 GB usable",
			usable: 46_000_000_000,
			want:   defaultCS,
			why:    "the fixed default caps the effective size for large destinations",
		},
		{
			name:   "disk 128 GB nominal (125.44 GB usable)",
			usable: 125_440_000_000,
			want:   defaultCS,
			why:    "any destination larger than 20 GiB + reserve uses the fixed default",
		},
		{
			name:   "disk 1 TB nominal (980 GB usable)",
			usable: 980_000_000_000,
			want:   defaultCS,
			why: "a very large destination still caps at the fixed 20 GiB default " +
				"(effective = min(20 GiB, usable - reserve), DESIGN_SPEC 4.2 + 4.3)",
		},
		{
			name:   "1 GB decimal destination",
			usable: 1_000_000_000,
			want:   1_000_000_000 - reserve,
			why: "a small destination lowers the effective size to its one-image payload " +
				"budget (usable - reserve), NOT its raw usable bytes - the pre-amendment " +
				"'usable bytes exactly' chunk could never fit a reserved-overhead image",
		},
		{
			name:   "MaxInt64 usable falls back to default",
			usable: math.MaxInt64,
			want:   defaultCS,
			why:    "extreme defensive input: the default cap holds, no overflow",
		},
		{
			name:   "exactly 20 GiB usable",
			usable: defaultCS,
			want:   defaultCS - reserve,
			why:    "the reserve shaves the budget below the default on the nose",
		},
		{
			name:   "1 GiB target",
			usable: gib,
			want:   gib - reserve,
			why:    "a smaller destination lowers the effective size to its payload budget (4.2)",
		},
		{
			name:   "budget of exactly one byte",
			usable: reserve + 1,
			want:   1,
			why:    "the smallest non-degenerate destination: a 1-byte effective size",
		},
		{
			name:   "degenerate 100-byte target falls back to default",
			usable: 100,
			want:   defaultCS,
			why: "usable below the reserve leaves no payload capacity: caller-bug defense " +
				"(plan.Balance refuses such profiles in validateProfile); never a zero chunk size",
		},
		{
			name:   "non-positive usable falls back to default",
			usable: 0,
			want:   defaultCS,
			why:    "caller bug defense; never yields a zero chunk size",
		},
		{
			name:   "negative usable falls back to default",
			usable: -1,
			want:   defaultCS,
			why:    "caller bug defense; a negative usable_size must never produce a zero/negative chunk size",
		},
	}
	for _, c := range cases {
		if got := EffectiveChunkSize(c.usable); got != c.want {
			t.Errorf("EffectiveChunkSize(%d) = %d, want %d (%s)", c.usable, got, c.want, c.why)
		}
	}
}

// --- mandatory-only (FR-041, honest fit threshold) ------------------------------

func TestShouldChunkMandatoryOnly(t *testing.T) {
	// The whole-file threshold of the 2026-10-20 capacity amendment:
	// whole iff size + FixedImageOverhead + ImageEntryOverhead <=
	// usable, i.e. size <= usable - 2,098,688. The TOP BAND between
	// that line and usable_size - which the old `size > usable_size`
	// predicate kept whole - is chunked because such a file genuinely
	// does not fit a buildable image.
	reserve := units.FixedImageOverhead + units.ImageEntryOverhead // 2,098,688
	threshold := bdRSLUsable - reserve
	cases := []struct {
		size   int64
		usable int64
		want   bool
	}{
		{0, bdRSLUsable, false},
		{1, bdRSLUsable, false},
		{threshold - 1, bdRSLUsable, false},  // one below the honest fit line: whole
		{threshold, bdRSLUsable, false},      // the exact fit: whole (never chunked)
		{threshold + 1, bdRSLUsable, true},   // one above: the top band, chunked
		{bdRSLUsable - 1, bdRSLUsable, true}, // deep in the top band: chunked
		{bdRSLUsable, bdRSLUsable, true},     // the old "exact fit" is now the top band
		{bdRSLUsable + 1, bdRSLUsable, true}, // one byte over usable: chunked
		{50 * gib, bdRSLUsable, true},        // clearly oversized
		{gib - reserve, gib, false},          // small destination, exact fit
		{gib - reserve + 1, gib, true},       // small destination, one into the top band
		{gib, gib, true},                     // small destination, whole usable: top band
	}
	for _, c := range cases {
		if got := ShouldChunk(c.size, c.usable); got != c.want {
			t.Errorf("ShouldChunk(%d, %d) = %v, want %v", c.size, c.usable, got, c.want)
		}
	}
	// Overflow guard: a corrupt-catalog size near MaxInt64 chunks
	// instead of wrapping the sum negative.
	if !ShouldChunk(math.MaxInt64, bdRSLUsable) {
		t.Error("ShouldChunk(MaxInt64, ...) = false; the overflow guard must chunk absurd sizes")
	}
}

// --- the decomposition formula ------------------------------------------------

// assertDecompositionInvariants pins the DESIGN_SPEC 4.3 formula on one
// plan: count = ceil(size/chunkSize), 0-based ascending indices,
// contiguous offsets, every full chunk exactly chunkSize, the final
// chunk > 0 and <= chunkSize, and the whole decomposition reassembling
// to exactly size (FR-041 no-truncate).
func assertDecompositionInvariants(t *testing.T, p ChunkPlan, size, chunkSize int64) {
	t.Helper()
	if p.ChunkSize != chunkSize {
		t.Errorf("plan ChunkSize = %d, want %d", p.ChunkSize, chunkSize)
	}
	if p.Count() > 0 && p.TotalLength() != size {
		t.Errorf("sum(lengths) = %d, want file size %d (no-truncate, FR-041)", p.TotalLength(), size)
	}
	var wantCount int
	if size > 0 {
		wantCount = int((size + chunkSize - 1) / chunkSize)
	}
	if p.Count() != wantCount {
		t.Errorf("count = %d, want ceil(%d/%d) = %d", p.Count(), size, chunkSize, wantCount)
	}
	var offset int64
	for i, c := range p.Chunks {
		if c.Index != i {
			t.Errorf("chunk %d carries index %d: indices must be 0-based ascending", i, c.Index)
		}
		if c.Offset != offset {
			t.Errorf("chunk %d offset = %d, want contiguous offset %d", i, c.Offset, offset)
		}
		if c.Length <= 0 {
			t.Errorf("chunk %d length = %d, must be > 0 (never zero-length)", i, c.Length)
		}
		if c.Length > chunkSize {
			t.Errorf("chunk %d length = %d, exceeds chunk size %d", i, c.Length, chunkSize)
		}
		if i < p.Count()-1 && c.Length != chunkSize {
			t.Errorf("chunk %d length = %d, non-final chunks must be exactly chunk_size %d",
				i, c.Length, chunkSize)
		}
		offset += c.Length
	}
	if p.Count() > 0 && offset != size {
		t.Errorf("last offset+length = %d, want file size %d", offset, size)
	}
}

func TestPlanChunksWorkedExample(t *testing.T) {
	// DESIGN_SPEC 4.3 worked example, asserted exactly:
	// a 50 GiB file with 20 GiB chunks = 3 chunks of 20 + 20 + 10 GiB.
	p, err := PlanChunks(7, 50*gib, defaultCS)
	if err != nil {
		t.Fatalf("PlanChunks: %v", err)
	}
	if p.FileID != 7 {
		t.Errorf("FileID = %d, want 7", p.FileID)
	}
	if p.Count() != 3 {
		t.Fatalf("count = %d, want 3", p.Count())
	}
	want := []ChunkSpec{
		{Index: 0, Offset: 0, Length: 20 * gib},
		{Index: 1, Offset: 20 * gib, Length: 20 * gib},
		{Index: 2, Offset: 40 * gib, Length: 10 * gib},
	}
	for i, w := range want {
		if p.Chunks[i] != w {
			t.Errorf("chunk %d = %+v, want %+v", i, p.Chunks[i], w)
		}
	}
	assertDecompositionInvariants(t, p, 50*gib, defaultCS)
}

func TestPlanChunksExactMultiple(t *testing.T) {
	// 40 GiB / 20 GiB = 2 exact chunks; the final chunk is exactly
	// chunk_size per the 4.3 formula.
	p, err := PlanChunks(1, 40*gib, defaultCS)
	if err != nil {
		t.Fatalf("PlanChunks: %v", err)
	}
	if p.Count() != 2 ||
		p.Chunks[0] != (ChunkSpec{Index: 0, Offset: 0, Length: 20 * gib}) ||
		p.Chunks[1] != (ChunkSpec{Index: 1, Offset: 20 * gib, Length: 20 * gib}) {
		t.Fatalf("40/20 decomposition = %+v, want 2x20 GiB", p.Chunks)
	}
	assertDecompositionInvariants(t, p, 40*gib, defaultCS)
}

func TestPlanChunksBoundarySizes(t *testing.T) {
	// DESIGN_SPEC 12.2 boundary fixtures: size exactly chunk_size,
	// chunk_size - 1, usable_size, usable_size + 1, and
	// 2 * chunk_size + remainder - with the REAL 20 GiB default plus
	// small synthetic sizes for cheap exhaustive checks. Sizes are
	// just numbers; nothing is allocated or read.
	const tiny = int64(1000)
	cases := []struct {
		name      string
		size      int64
		chunkSize int64
		wantCount int
		wantLens  []int64 // exact lengths when small enough to spell out
	}{
		{
			name: "exactly chunk_size (1 full chunk)",
			size: defaultCS, chunkSize: defaultCS, wantCount: 1,
		},
		{
			name: "chunk_size - 1 (1 chunk)",
			size: defaultCS - 1, chunkSize: defaultCS, wantCount: 1,
		},
		{
			name: "chunk_size + 1 (full + 1 byte)",
			size: defaultCS + 1, chunkSize: defaultCS, wantCount: 2,
		},
		{
			name: "usable_size boundary with effective 20 GiB (2 chunks)",
			size: bdRSLUsable, chunkSize: defaultCS, wantCount: 2,
		},
		{
			name: "usable_size + 1 (still 2 chunks)",
			size: bdRSLUsable + 1, chunkSize: defaultCS, wantCount: 2,
		},
		{
			name: "2 * chunk_size + remainder (3 chunks, 45 GiB)",
			size: 45 * gib, chunkSize: defaultCS, wantCount: 3,
		},
		{
			name: "tiny: 1 byte (1 sub-size chunk)",
			size: 1, chunkSize: tiny, wantCount: 1, wantLens: []int64{1},
		},
		{
			name: "tiny: exactly chunk_size",
			size: tiny, chunkSize: tiny, wantCount: 1, wantLens: []int64{tiny},
		},
		{
			name: "tiny: chunk_size - 1",
			size: tiny - 1, chunkSize: tiny, wantCount: 1, wantLens: []int64{tiny - 1},
		},
		{
			name: "tiny: chunk_size + 1",
			size: tiny + 1, chunkSize: tiny, wantCount: 2, wantLens: []int64{tiny, 1},
		},
		{
			name: "tiny: exact multiple 2x",
			size: 2 * tiny, chunkSize: tiny, wantCount: 2, wantLens: []int64{tiny, tiny},
		},
		{
			name: "tiny: 2 * chunk_size + remainder",
			size: 2*tiny + 3, chunkSize: tiny, wantCount: 3, wantLens: []int64{tiny, tiny, 3},
		},
		{
			name: "zero-size yields zero chunks (well-defined, never planned)",
			size: 0, chunkSize: tiny, wantCount: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := PlanChunks(42, c.size, c.chunkSize)
			if err != nil {
				t.Fatalf("PlanChunks(%d, %d): %v", c.size, c.chunkSize, err)
			}
			if p.Count() != c.wantCount {
				t.Fatalf("count = %d, want %d", p.Count(), c.wantCount)
			}
			if len(c.wantLens) > 0 {
				for i, want := range c.wantLens {
					if p.Chunks[i].Length != want {
						t.Errorf("chunk %d length = %d, want %d", i, p.Chunks[i].Length, want)
					}
				}
			}
			assertDecompositionInvariants(t, p, c.size, c.chunkSize)
		})
	}
}

func TestPlanChunksInvalidInputs(t *testing.T) {
	if _, err := PlanChunks(1, -1, defaultCS); err == nil {
		t.Error("negative size returned nil error, want ErrInvalidDecompose")
	}
	if _, err := PlanChunks(1, defaultCS, 0); err == nil {
		t.Error("zero chunk size returned nil error, want ErrInvalidDecompose")
	}
	if _, err := PlanChunks(1, defaultCS, -5); err == nil {
		t.Error("negative chunk size returned nil error, want ErrInvalidDecompose")
	}
}

// --- property: chunks reassemble to the file size (FR-041) --------------------

func TestPlanChunksReassemblyProperty(t *testing.T) {
	// Seeded, printed on failure (DESIGN_SPEC 12.5 style). 5000
	// (size, chunkSize) pairs across tiny/small/huge scales with a
	// boundary-biased size generator: exact multiples, k*chunkSize +
	// remainder, and base +/- 2 near multiples.
	const seed = int64(20261019)
	r := rand.New(rand.NewSource(seed))

	boundaryBiasedSize := func(r *rand.Rand, chunkSize int64) int64 {
		// The chunk COUNT is kept bounded (<= ~10k) so the property
		// loop exercises boundaries without materializing absurd
		// slice counts; sizes are logical numbers, never content.
		switch r.Intn(4) {
		case 0: // exact multiple (includes 0 * chunkSize)
			return int64(r.Intn(5)) * chunkSize
		case 1: // k * chunkSize + random remainder
			return int64(r.Intn(4))*chunkSize + int64(r.Int63n(chunkSize))
		case 2: // k * chunkSize +/- 2 (the boundary neighbourhood)
			base := (int64(r.Intn(4)) + 1) * chunkSize
			size := base + int64(r.Intn(5)) - 2
			if size < 0 {
				return 0
			}
			return size
		default: // arbitrary position with bounded chunk count
			return int64(r.Intn(10_001))*chunkSize + int64(r.Int63n(chunkSize))
		}
	}

	for i := 0; i < 5000; i++ {
		var chunkSize int64
		switch r.Intn(3) {
		case 0:
			chunkSize = int64(r.Intn(1024)) + 1 // tiny
		case 1:
			chunkSize = int64(r.Intn(1<<20)) + 1 // small
		default:
			chunkSize = r.Int63n(1<<42) + 1 // huge (multi-TiB scale)
		}
		size := boundaryBiasedSize(r, chunkSize)

		p, err := PlanChunks(int64(i), size, chunkSize)
		if err != nil {
			t.Fatalf("seed %d iteration %d: PlanChunks(%d, %d): %v", seed, i, size, chunkSize, err)
		}

		// The headline property: chunks exactly reassemble to size.
		if p.TotalLength() != size {
			t.Fatalf("seed %d iteration %d: size %d chunk_size %d: sum(lengths) = %d "+
				"(no-truncate guarantee broken)", seed, i, size, chunkSize, p.TotalLength())
		}
		// Full invariants, with seed context on any failure.
		if !decompositionInvariantsHold(p, size, chunkSize) {
			t.Fatalf("seed %d iteration %d: size %d chunk_size %d: decomposition invariants broken: %+v",
				seed, i, size, chunkSize, p.Chunks)
		}
	}
}

// decompositionInvariantsHold is assertDecompositionInvariants in boolean
// form for the property loop (reporting per-case details instead of
// t.Helper messages).
func decompositionInvariantsHold(p ChunkPlan, size, chunkSize int64) bool {
	if size > 0 && p.Count() != int((size+chunkSize-1)/chunkSize) {
		return false
	}
	if size == 0 && p.Count() != 0 {
		return false
	}
	var offset int64
	for i, c := range p.Chunks {
		if c.Index != i || c.Offset != offset || c.Length <= 0 || c.Length > chunkSize {
			return false
		}
		if i < p.Count()-1 && c.Length != chunkSize {
			return false
		}
		offset += c.Length
	}
	return p.Count() == 0 || offset == size
}

// --- single-chunk decomposition (small-destination sub-chunk-size case) --------

func TestPlanChunksSingleChunkOffsetZeroLengthSize(t *testing.T) {
	// size < effective chunk size (but the file WOULD be chunked by a
	// caller whenever size > usable_size and the usable_size is above
	// it; the planner itself never asks for this decomposition because
	// ShouldChunk is false there - PlanChunks' math stays total so
	// callers and tests can rely on it, per the function contract):
	// exactly ONE chunk at offset 0 with length == size.
	cases := []struct {
		name      string
		size      int64
		chunkSize int64
	}{
		{"small destination: size 900 < chunk size 1024", 900, 1024},
		{"single byte", 1, 1024},
		{"size one below chunk size", 1023, 1024},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := PlanChunks(7, c.size, c.chunkSize)
			if err != nil {
				t.Fatalf("PlanChunks(%d, %d): %v", c.size, c.chunkSize, err)
			}
			if p.Count() != 1 {
				t.Fatalf("count = %d, want exactly 1", p.Count())
			}
			want := ChunkSpec{Index: 0, Offset: 0, Length: c.size}
			if p.Chunks[0] != want {
				t.Errorf("chunk 0 = %+v, want %+v (offset 0, length == size)", p.Chunks[0], want)
			}
			if p.TotalLength() != c.size {
				t.Errorf("TotalLength = %d, want %d", p.TotalLength(), c.size)
			}
		})
	}
}

// --- overflow-safe ceil (MaxInt64-adjacent sizes) ------------------------------

func TestPlanChunksOverflowSafeCeilNearMaxInt64(t *testing.T) {
	// The ceil in PlanChunks is computed as quotient + remainder test,
	// NOT as (size + chunkSize - 1) / chunkSize: the plain form wraps
	// negative for size near MaxInt64 and silently yields a
	// ZERO-chunk plan (the FR-041 no-truncate nightmare). These cases
	// pin the fixed behavior on MaxInt64-adjacent inputs.
	//
	// NOTE: the shared assertDecompositionInvariants helper computes
	// its reference count with the overflow-prone (size+chunkSize-1)
	// form, so it is deliberately NOT used here; the count reference
	// below uses the same overflow-safe quotient/remainder math and
	// every OTHER invariant (indices, contiguity, lengths, total) is
	// asserted independently.
	max := int64(math.MaxInt64) // 9_223_372_036_854_775_807

	cases := []struct {
		name      string
		size      int64
		chunkSize int64
		wantCount int
	}{
		{
			// Naive form: (max + max - 1) wraps to -2 -> count 0.
			name: "size == chunk_size == MaxInt64: exactly 1 full chunk",
			size: max, chunkSize: max, wantCount: 1,
		},
		{
			// Naive form: (max + max-1 - 1) wraps to -4 -> count 0.
			name: "size MaxInt64, chunk_size MaxInt64-1: full chunk + 1 byte",
			size: max, chunkSize: max - 1, wantCount: 2,
		},
		{
			// Naive form wraps here too; count must be 8192:
			// 8191 full chunks of 2^50 plus a final chunk of 2^50-1.
			name: "size MaxInt64, chunk_size 2^50: 8192 chunks",
			size: max, chunkSize: 1 << 50, wantCount: 8192,
		},
		{
			name: "size MaxInt64-1, chunk_size MaxInt64: 1 partial chunk",
			size: max - 1, chunkSize: max, wantCount: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := PlanChunks(1, c.size, c.chunkSize)
			if err != nil {
				t.Fatalf("PlanChunks(%d, %d): %v", c.size, c.chunkSize, err)
			}
			if p.Count() != c.wantCount {
				t.Fatalf("count = %d, want %d (overflow-safe ceil)", p.Count(), c.wantCount)
			}
			// Overflow-safe reference count: quotient + remainder test.
			wantCount := int(c.size / c.chunkSize)
			if c.size%c.chunkSize > 0 {
				wantCount++
			}
			if p.Count() != wantCount {
				t.Fatalf("count = %d, want ceil(%d/%d) = %d", p.Count(), c.size, c.chunkSize, wantCount)
			}

			// Per-chunk invariants. The accumulator can never exceed
			// size (lengths sum to size), so no intermediate overflow.
			var offset int64
			for i, ch := range p.Chunks {
				if ch.Index != i {
					t.Fatalf("chunk %d carries index %d", i, ch.Index)
				}
				if ch.Offset != offset {
					t.Fatalf("chunk %d offset = %d, want contiguous %d", i, ch.Offset, offset)
				}
				if ch.Length <= 0 || ch.Length > c.chunkSize {
					t.Fatalf("chunk %d length = %d, want (0, %d]", i, ch.Length, c.chunkSize)
				}
				if i < p.Count()-1 && ch.Length != c.chunkSize {
					t.Fatalf("chunk %d length = %d, non-final chunks must be exactly %d",
						i, ch.Length, c.chunkSize)
				}
				offset += ch.Length
			}
			// The headline no-truncate guarantee at the int64 extreme.
			if p.TotalLength() != c.size {
				t.Fatalf("sum(lengths) = %d, want size %d (no-truncate, FR-041)", p.TotalLength(), c.size)
			}

			// Spell out the two smallest cases exactly.
			switch c.chunkSize {
			case max:
				want := ChunkSpec{Index: 0, Offset: 0, Length: c.size}
				if p.Chunks[0] != want {
					t.Errorf("chunk 0 = %+v, want %+v", p.Chunks[0], want)
				}
			case max - 1:
				want := []ChunkSpec{
					{Index: 0, Offset: 0, Length: max - 1},
					{Index: 1, Offset: max - 1, Length: 1},
				}
				for i, w := range want {
					if p.Chunks[i] != w {
						t.Errorf("chunk %d = %+v, want %+v", i, p.Chunks[i], w)
					}
				}
			case 1 << 50:
				// 8191 chunks of 2^50, final chunk 2^50 - 1.
				if p.Chunks[8191] != (ChunkSpec{Index: 8191, Offset: 8191 << 50, Length: (1 << 50) - 1}) {
					t.Errorf("final chunk = %+v, want {index 8191, offset %d, length %d}",
						p.Chunks[8191], 8191<<50, (1<<50)-1)
				}
			}
		})
	}
}

// --- boundary stability (DESIGN_SPEC 4.2) -------------------------------------

func TestDecideChunkingBoundaryStability(t *testing.T) {
	// reserve is the fixed+entry overhead of the canonical estimator;
	// the whole-file threshold is usable - reserve (the 2026-10-20
	// honest fit test).
	reserve := units.FixedImageOverhead + units.ImageEntryOverhead
	cases := []struct {
		name      string
		size      int64
		usable    int64
		stored    StoredChunks
		want      Action
		wantEff   int64
		wantHashR int
	}{
		{
			// The OLD model's "exact fit" now sits in the top band
			// (size + reserve > usable): a file filling the whole
			// usable_size genuinely does not fit a buildable image.
			name: "at usable_size exactly: top band, chunked (honest fit)",
			size: bdRSLUsable, usable: bdRSLUsable,
			stored: StoredChunks{},
			want:   ActionCreateFresh, wantEff: defaultCS,
		},
		{
			name: "at the honest fit line: whole file, stored state irrelevant",
			size: bdRSLUsable - reserve, usable: bdRSLUsable,
			stored: StoredChunks{ChunkSize: defaultCS, WrittenChunks: 3},
			want:   ActionWholeFile, wantEff: defaultCS,
		},
		{
			name: "below the honest fit line: whole file",
			size: 1000, usable: bdRSLUsable,
			want: ActionWholeFile, wantEff: defaultCS,
		},
		{
			name: "oversized, never chunked: create fresh",
			size: bdRSLUsable + 1, usable: bdRSLUsable,
			want: ActionCreateFresh, wantEff: defaultCS,
		},
		{
			name: "oversized, same stored size: reuse with zero re-reads",
			size: bdRSLUsable + 1, usable: bdRSLUsable,
			stored: StoredChunks{ChunkSize: defaultCS, WrittenChunks: 2, UnwrittenChunks: 1},
			want:   ActionReuse, wantEff: defaultCS, wantHashR: 0,
		},
		{
			name: "changed profile, written chunks: re-chunk, re-hashed at next write",
			size: bdRSLUsable + 1, usable: bdRSLUsable,
			stored: StoredChunks{ChunkSize: gib, WrittenChunks: 2, UnwrittenChunks: 1},
			want:   ActionRechunk, wantEff: defaultCS, wantHashR: 2,
		},
		{
			name: "changed profile, all chunks unwritten: free re-chunk",
			size: bdRSLUsable + 1, usable: bdRSLUsable,
			stored: StoredChunks{ChunkSize: gib, UnwrittenChunks: 3},
			want:   ActionRechunk, wantEff: defaultCS, wantHashR: 0,
		},
		{
			name: "smaller destination lowers effective size: stored 20 GiB -> re-chunk",
			size: 2*gib + 1, usable: gib,
			stored: StoredChunks{ChunkSize: defaultCS, WrittenChunks: 3},
			want:   ActionRechunk, wantEff: gib - reserve, wantHashR: 3,
		},
		{
			name: "profile change made the file fit: whole file again",
			size: 10 * gib, usable: 50 * gib,
			stored: StoredChunks{ChunkSize: defaultCS, WrittenChunks: 2},
			want:   ActionWholeFile, wantEff: defaultCS, wantHashR: 0,
		},
		{
			name: "defensive: zero usable_size never yields a zero effective size",
			size: gib, usable: 0,
			stored: StoredChunks{},
			want:   ActionCreateFresh, wantEff: defaultCS,
		},
		{
			name: "defensive: negative usable_size falls back to the default effective size",
			size: gib, usable: -1,
			stored: StoredChunks{ChunkSize: defaultCS, WrittenChunks: 1},
			want:   ActionReuse, wantEff: defaultCS, wantHashR: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := DecideChunking(c.size, c.usable, c.stored)
			if d.Action != c.want {
				t.Errorf("action = %s, want %s", d.Action, c.want)
			}
			if d.Effective != c.wantEff {
				t.Errorf("effective = %d, want %d", d.Effective, c.wantEff)
			}
			if d.RehashedAtNextWrite != c.wantHashR {
				t.Errorf("RehashedAtNextWrite = %d, want %d", d.RehashedAtNextWrite, c.wantHashR)
			}
		})
	}
}

// --- metadata-only: the chunker never opens a source file ----------------------

func TestChunkerNeverOpensSourceFiles(t *testing.T) {
	// Real sparse files on disk flow through the FULL decision path
	// (ShouldChunk -> DecideChunking -> PlanChunks) with zero opens.
	// The files are chmod 0000, so any open attempt would fail the
	// test with EACCES (a positive control asserts the guard is armed
	// for non-root runs; as root the control is vacuous and the AST
	// import test below remains the hard proof).
	dir := t.TempDir()
	files := map[string]int64{
		"huge.img":  50 * gib,        // sparse, logical only
		"over.img":  bdRSLUsable + 1, // one byte over the threshold
		"small.img": 1024,            // below threshold
	}
	paths := make(map[string]string, len(files))
	for name, size := range files {
		p := filepath.Join(dir, name)
		// Create the (empty) file, then set its logical size sparse:
		// content is never written or read.
		f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatalf("creating %s: %v", name, err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("closing %s: %v", name, err)
		}
		if err := os.Truncate(p, size); err != nil {
			t.Fatalf("creating sparse %s: %v", name, err)
		}
		if err := os.Chmod(p, 0); err != nil {
			t.Fatalf("chmod 0000 %s: %v", name, err)
		}
		paths[name] = p
	}

	// Positive control: the guard is armed (non-root only).
	if os.Geteuid() != 0 {
		if f, err := os.Open(paths["huge.img"]); err == nil {
			f.Close()
			t.Fatal("positive control failed: opening a mode-0000 file succeeded; " +
				"the EACCES proof would be vacuous")
		}
	} else {
		t.Log("running as root: mode-0000 open guard is vacuous; " +
			"the AST import assertion remains the metadata-only proof")
	}

	// Full decision path over stat-derived sizes. Any filesystem open
	// of the sources would abort with EACCES (non-root).
	for name, size := range files {
		// Size comes from Stat, the ONLY filesystem touch the planner
		// would ever do - and the ingest pipeline, not the chunker,
		// is what performs it.
		fi, err := os.Stat(paths[name])
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if fi.Size() != size {
			t.Fatalf("sparse %s size = %d, want %d", name, fi.Size(), size)
		}

		d := DecideChunking(fi.Size(), bdRSLUsable, StoredChunks{})
		p, err := PlanChunks(1, fi.Size(), d.Effective)
		if err != nil {
			t.Fatalf("%s: PlanChunks: %v (would require a source open to fail this way)",
				name, err)
		}
		assertDecompositionInvariants(t, p, fi.Size(), d.Effective)

		switch name {
		case "huge.img":
			if d.Action != ActionCreateFresh || p.Count() != 3 {
				t.Errorf("huge.img: action %s count %d, want create-fresh with 3 chunks",
					d.Action, p.Count())
			}
		case "over.img":
			if d.Action != ActionCreateFresh || p.Count() != 2 {
				t.Errorf("over.img: action %s count %d, want create-fresh with 2 chunks",
					d.Action, p.Count())
			}
		case "small.img":
			// Whole-file decision; PlanChunks' general math would
			// still decompose it into one chunk (totality) - the
			// planner simply never creates rows for it.
			if d.Action != ActionWholeFile || p.Count() != 1 {
				t.Errorf("small.img: action %s count %d, want whole-file (1 chunk in total math)",
					d.Action, p.Count())
			}
		}
	}
}

// TestChunkerSourceImportsNeverTouchFilesystem is the hard
// metadata-only proof: chunker.go may import only standard-library
// packages that cannot reach the filesystem or the network. This is
// the established AST import-assertion pattern (per the Sprint 0
// notes: x/sys and cobra/doc bans).
func TestChunkerSourceImportsNeverTouchFilesystem(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "chunker.go", nil, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parsing chunker.go: %v", err)
	}
	// Denylist: anything that could open, read, stat, or dial.
	denied := map[string]bool{
		"os": true, "io": true, "io/fs": true, "os/exec": true,
		"path": true, "path/filepath": true,
		"net": true, "net/http": true, "net/url": true, "net/rpc": true,
		"syscall": true, "unsafe": true,
		"database/sql": true, "modernc.org/sqlite": true,
	}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`) // strip quotes
		if denied[path] {
			t.Errorf("chunker.go imports %q: the chunker must stay metadata-only "+
				"(DESIGN_SPEC 4.3, 12.5)", path)
		}
		// No third-party imports either: the chunker is arithmetic.
		first, _, _ := strings.Cut(path, "/")
		if strings.Contains(first, ".") {
			t.Errorf("chunker.go imports non-stdlib package %q", path)
		}
	}
}
