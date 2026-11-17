package plan

// U.2 adversarial follow-up: the refusal branches of DESIGN_SPEC 4.3
// "PARTIALLY-ASSIGNED CHUNK SETS" / 4.6 "Partial chunk sets" that the
// re-scoped balancer_gaps block does NOT reach, so every refusal
// reason has its own zero-transaction pin:
//
//   - ROWS-DON'T-VERIFY: files.chunk_size equals the run's effective
//     size (so DecideChunking decides REUSE) but the STORED ROWS do
//     not verify against the decomposition. Only the partial-set
//     branch refuses here - the non-partial path deliberately falls
//     back to re-chunk (balancer_unit_test's liar case), so this test
//     is the only catcher for the decompositionMatches gate at line
//     703 of balancer.go (mutation target m4).
//   - LYING STAMP: rows that DO match the effective decomposition but
//     a files.chunk_size that does not (the stamp lies, not the rows).
//     Only the effective-size refusal can stop this - the rows verify,
//     so removing the Action != ActionReuse gate would let the reuse
//     path complete over a stamp the catalog will keep misreporting
//     (mutation target m5).
//   - SYMLINK CARRY-OVER: the whole-fit/symlink entry condition of the
//     partial branch on a symlink row - a shape only raw SQL can build
//     (symlinks never chunk, D-03), so the plan tier is its natural
//     home; the regular-file whole-fit half of the same condition is
//     pinned at both tiers by ...WholeFileTransitionRefused and the
//     AC-23 CLI test.
//
// Each test asserts the full 4.3/4.6 message contract (file, locked
// image ids, branch reason, branch remedy(s)) and the zero-tx
// contract: no transaction opened, store digest bit-identical, no row
// deleted or rewritten.

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestBalancerPartialChunkSetRowsDoNotVerifyRefused pins the third
// refusal reason: the stamp matches the effective size (reuse decides)
// but the stored rows' geometry has drifted, so completing the set
// would place chunks by a decomposition the stored rows contradict.
// Refused at COMPUTE with ErrPartialChunkReplan, both remedies, zero
// transactions, rows untouched.
func TestBalancerPartialChunkSetRowsDoNotVerifyRefused(t *testing.T) {
	s := newFakeStore(testProfile(612))
	addLockedImage(s, 1, budgetUsable(612))
	addFile(s, 1, "big/partial.img", 250)
	// Stored state: chunk_size 100 == the effective size (DecideChunking
	// will decide REUSE), rows 100+100+50, chunk 0 locked on image 1.
	stampDecomposition(t, s, 1, 250, 100, 3)
	s.chunkRows[1][0].imageID = 1
	// Drift the ROW geometry while leaving the stamp alone: chunk 1's
	// offset no longer matches the decomposition (a hand-edited catalog
	// or a partial write outside the planner).
	s.chunkRows[1][1].offset = 101
	before := s.digest()

	_, err := Balance(context.Background(), s, s.profile)
	if !errors.Is(err, ErrPartialChunkReplan) {
		t.Fatalf("Balance = %v, want ErrPartialChunkReplan (rows do not verify)", err)
	}
	for _, want := range []string{
		"big/partial.img",
		"locked on image(s) 1",
		"do not verify against the current decomposition",
		remedyKeepProfile,
		remedyClearWholeSet,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message %q lacks %q", err.Error(), want)
		}
	}
	if s.began != 0 {
		t.Errorf("refused plan opened %d transactions, want 0 (pre-tx refusal)", s.began)
	}
	if s.digest() != before {
		t.Errorf("refused plan mutated the store:\n--- before ---\n%s\n--- after ---\n%s",
			before, s.digest())
	}
	// The drifted row survived byte-identically - nothing rewrote or
	// deleted it (ReplaceAllChunks never ran).
	rows := s.chunkRows[1]
	if len(rows) != 3 || rows[1].offset != 101 || rows[0].imageID != 1 {
		t.Errorf("rows after refusal = %+v, want the original 3 with the drifted offset and locked chunk 0", rows)
	}
}

// TestBalancerPartialChunkSetLyingStampRefused pins the second refusal
// reason's inner gate: the stored ROWS verify against this run's
// effective decomposition (so decompositionMatches would PASS) but
// files.chunk_size reports a different size - the stamp lies. The
// refusal must still fire at the effective-size gate, because the
// reuse path must never re-stamp or re-place over a decomposition the
// catalog's own stamp disowns (mutation target m5: dropping the
// Action != ActionReuse gate completes this set silently).
func TestBalancerPartialChunkSetLyingStampRefused(t *testing.T) {
	s := newFakeStore(testProfile(612))
	addLockedImage(s, 1, budgetUsable(612))
	addFile(s, 1, "big/liar.img", 250)
	// Rows = the EFFECTIVE decomposition (100+100+50 at effective 100,
	// so decompositionMatches passes) ...
	stampDecomposition(t, s, 1, 250, 100, 3)
	// ... but the stamp claims 50. Chunk 0 is locked: 0 < assigned < total.
	s.files[1].ChunkSize = 50
	s.chunkRows[1][0].imageID = 1
	before := s.digest()

	_, err := Balance(context.Background(), s, s.profile)
	if !errors.Is(err, ErrPartialChunkReplan) {
		t.Fatalf("Balance = %v, want ErrPartialChunkReplan (lying stamp)", err)
	}
	for _, want := range []string{
		"big/liar.img",
		"locked on image(s) 1",
		"the effective chunk size changed",
		"stored chunk size 50",
		remedyKeepProfile,
		remedyClearWholeSet,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message %q lacks %q", err.Error(), want)
		}
	}
	if s.began != 0 {
		t.Errorf("refused plan opened %d transactions, want 0", s.began)
	}
	if s.digest() != before {
		t.Errorf("refused plan mutated the store:\n--- before ---\n%s\n--- after ---\n%s",
			before, s.digest())
	}
	if rows := s.chunkRows[1]; len(rows) != 3 || rows[0].imageID != 1 {
		t.Errorf("rows after refusal = %+v, want the original 3 with chunk 0 locked", rows)
	}
}

// TestBalancerPartialChunkSetOnSymlinkRefused pins the ENTRY condition
// of the partial branch for EntryKind == "symlink": a symlink carrying
// a partially-assigned chunk set is corrupt-catalog state (symlinks
// never chunk, D-03 - the whole-file transition would DropChunkState),
// so Balance refuses before the rewrite can delete the locked rows.
// Zero transactions, store bit-identical, both remedies (the message
// clause is shared with the whole-file-fit branch of the same `if`).
func TestBalancerPartialChunkSetOnSymlinkRefused(t *testing.T) {
	s := newFakeStore(testProfile(612))
	addLockedImage(s, 1, budgetUsable(612))
	addLink(s, 1, "link/legacy-chunked", 8)
	// Hand-built corruption: the symlink carries two chunk rows, one
	// assigned (0 < assigned < total -> PARTIAL).
	s.files[1].chunked = true
	s.files[1].chunkCount = 2
	s.files[1].ChunkSize = 100
	s.chunkRows[1] = []fakeChunk{
		{index: 0, offset: 0, length: 4, hasHash: true},
		{index: 1, offset: 4, length: 4, imageID: 1, hasHash: true},
	}
	before := s.digest()

	_, err := Balance(context.Background(), s, s.profile)
	if !errors.Is(err, ErrPartialChunkReplan) {
		t.Fatalf("Balance = %v, want ErrPartialChunkReplan (symlink with a partial chunk set)", err)
	}
	for _, want := range []string{
		"link/legacy-chunked",
		"locked on image(s) 1",
		"now fits one destination",
		remedyKeepProfile,
		remedyClearWholeSet,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal message %q lacks %q", err.Error(), want)
		}
	}
	if s.began != 0 {
		t.Errorf("refused plan opened %d transactions, want 0", s.began)
	}
	if s.digest() != before {
		t.Errorf("refused plan mutated the store:\n--- before ---\n%s\n--- after ---\n%s",
			before, s.digest())
	}
	if rows := s.chunkRows[1]; len(rows) != 2 || rows[1].imageID != 1 {
		t.Errorf("rows after refusal = %+v, want the original 2 with chunk 1 locked", rows)
	}
}
