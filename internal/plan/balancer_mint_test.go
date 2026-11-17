package plan

// U.2 (DESIGN_SPEC 4.6 "Image identity (lowest-unused minting)"): the
// image-id allocator unit set plus its two Balance-level mint sites.
// The three properties the design keys on:
//
//   - CONTIGUOUS-CATALOG EQUIVALENCE: with rows {1..k} the first free
//     id IS k+1, so the allocator is numerically identical to the old
//     max+1 rule and every pre-existing expectation (the T2.4
//     exact-fresh-plan equivalence, the clear_gaps counterfactual
//     pin) holds unchanged;
//   - HOLE RE-FILL: a released id is re-minted FIRST (the reopened
//     slot fills before any id above it), lowest-first when several
//     holes exist, so "rewrite disk 3" stays disk 3;
//   - STRICTLY INCREASING WITHIN A RUN: the cursor never rewinds, no
//     id is handed out twice, and spillover still opens above the
//     maximum.
//
// The map is a membership LOOKUP only (never ranged), so map
// iteration order can never reach a minted id - the determinism trap
// the balancer's 12.5 property suite guards.

import (
	"context"
	"testing"
)

// lockedStates is the tiny helper building a locked set from ids.
func lockedStates(ids ...int64) []DestinationState {
	out := make([]DestinationState, 0, len(ids))
	for _, id := range ids {
		out = append(out, DestinationState{
			ImageID: id, NominalSize: 2000, UsableSize: 1000,
			MediaProfile: "disk",
		})
	}
	return out
}

// TestImageIDAllocatorContiguousCatalogMatchesMaxPlusOne is the
// equivalence proof: a contiguous catalog {1..k} mints k+1, k+2, ...
// - byte-identical to the retired max+1 rule (so every existing
// expectation holds with no edit), and an EMPTY catalog mints 1, 2,
// ... (1-based, 6.4).
func TestImageIDAllocatorContiguousCatalogMatchesMaxPlusOne(t *testing.T) {
	empty := newImageIDAllocator(nil)
	for want := int64(1); want <= 4; want++ {
		if got := empty.mint(); got != want {
			t.Fatalf("empty catalog mint = %d, want %d (1-based sequence)", got, want)
		}
	}

	full := newImageIDAllocator(lockedStates(1, 2, 3))
	// The old rule's output for this input: max(locked)+1 = 4, then 5.
	for want := int64(4); want <= 7; want++ {
		if got := full.mint(); got != want {
			t.Errorf("contiguous {1,2,3} mint = %d, want %d (identical to max+1)", got, want)
		}
	}
}

// TestImageIDAllocatorSingleHoleRefillsReleasedIDFirst pins the
// headline identity: unlock image 2 of {1,2,3} -> the next mints are
// 2 (the reopened slot, FIRST), then 4, 5 - never 4 first.
func TestImageIDAllocatorSingleHoleRefillsReleasedIDFirst(t *testing.T) {
	a := newImageIDAllocator(lockedStates(1, 3)) // image 2 released
	want := []int64{2, 4, 5, 6}
	for i, w := range want {
		if got := a.mint(); got != w {
			t.Fatalf("mint #%d = %d, want %d (the hole fills first)", i, got, w)
		}
	}
}

// TestImageIDAllocatorMultipleHolesRefillLowestFirst pins the
// multi-hole corollary of 4.6: with MORE THAN ONE hole the ids refill
// in ascending order (2, then 3, before anything above the top
// hole), and ids in use are never handed out.
func TestImageIDAllocatorMultipleHolesRefillLowestFirst(t *testing.T) {
	a := newImageIDAllocator(lockedStates(1, 4, 6)) // holes 2, 3, 5
	want := []int64{2, 3, 5, 7, 8}
	for i, w := range want {
		if got := a.mint(); got != w {
			t.Fatalf("mint #%d = %d, want %d (lowest hole first, in-use ids skipped)", i, got, w)
		}
	}
}

// TestImageIDAllocatorCursorNeverRemintsWithinARun is the monotonic-
// cursor contract: over a mixed in-use set, a long run of mints is
// strictly increasing, contains no id the seed held, and repeats no
// id - whatever order the seed ids arrived in.
func TestImageIDAllocatorCursorNeverRemintsWithinARun(t *testing.T) {
	// Deliberately unsorted input: construction is membership-only.
	inUse := []int64{7, 2, 9, 4}
	a := newImageIDAllocator(lockedStates(inUse...))
	inUseSet := make(map[int64]bool, len(inUse))
	for _, id := range inUse {
		inUseSet[id] = true
	}
	seen := make(map[int64]bool)
	var prev int64
	for i := 0; i < 40; i++ {
		got := a.mint()
		if got <= prev {
			t.Fatalf("mint #%d = %d after %d: the cursor must be strictly increasing", i, got, prev)
		}
		if inUseSet[got] {
			t.Fatalf("mint #%d = %d: an id in use by a locked images row was handed out", i, got)
		}
		if seen[got] {
			t.Fatalf("mint #%d = %d: id handed out twice within one run", i, got)
		}
		seen[got] = true
		prev = got
	}
}

// --- the two Balance-level mint sites --------------------------------------

// TestBalanceGappedCatalogMintsLowestFreeFirst pins the WFD-open mint
// site against a HOLE: locked {1,3} (image 2 released) with new
// payload must open destination 2 - the reopened slot - not 4 (the
// old max+1 answer), and two computes over the same state must mint
// byte-identical ids (AC-36's plan-tier determinism half).
func TestBalanceGappedCatalogMintsLowestFreeFirst(t *testing.T) {
	s := newFakeStore(testProfile(1200))
	addLockedImage(s, 1, budgetUsable(1200))
	addLockedImage(s, 3, budgetUsable(1200)) // image 2 is the hole
	addFile(s, 1, "new/a", 60)
	addFile(s, 2, "new/b", 40)

	planA, err := Balance(context.Background(), s, s.profile)
	if err != nil {
		t.Fatalf("Balance #1: %v", err)
	}
	planB, err := Balance(context.Background(), s, s.profile)
	if err != nil {
		t.Fatalf("Balance #2: %v", err)
	}
	if resultDigest(planA.Result) != resultDigest(planB.Result) {
		t.Fatalf("gapped-catalog compute is nondeterministic:\n%s\nvs\n%s",
			resultDigest(planA.Result), resultDigest(planB.Result))
	}
	if planA.Result.NewDestinations != 1 {
		t.Fatalf("new destinations = %d, want 1 (both items fit the reopened slot)", planA.Result.NewDestinations)
	}
	if err := planA.Write(context.Background(), s.factory()); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if imageOf(s, 1) != 2 || imageOf(s, 2) != 2 {
		t.Errorf("placement = (%d, %d), want the reopened slot 2 (lowest free), not 4",
			imageOf(s, 1), imageOf(s, 2))
	}
	for _, id := range []int64{1, 2, 3} {
		if _, ok := s.images[id]; !ok {
			t.Errorf("images row %d missing after the gapped plan", id)
		}
	}
	if _, ok := s.images[4]; ok {
		t.Error("spillover id 4 minted although the reopened slot fits (no-new-disk violation)")
	}
}

// TestBalanceDirsOnlyFallbackMintsLowestFreeID pins the SECOND mint
// site: the dirs-only fallback of 4.3. Two FULL locked destinations
// (1 and 3, image 2 released) cannot absorb the dir entry charges, so
// the fallback destination must be the lowest id not in use - 2, not
// the old max+1 answer 4.
func TestBalanceDirsOnlyFallbackMintsLowestFreeID(t *testing.T) {
	// Payload budget 1200: each locked image holds a 688-byte file
	// whose charge (688 + 512) fills the budget EXACTLY - zero entry
	// slack, so lowestDirTarget rejects both and the fallback fires.
	s := newFakeStore(testProfile(1200))
	addLockedImage(s, 1, budgetUsable(1200))
	addLockedImage(s, 3, budgetUsable(1200)) // image 2 is the hole
	addFile(s, 1, "locked/full-a", 688)
	s.files[1].ImageID = 1
	addFile(s, 2, "locked/full-b", 688)
	s.files[2].ImageID = 3
	s.dirs[9] = &fakeDir{relPath: "empty/x", active: true}

	res, err := runPlan(t, s)
	if err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	if s.dirs[9].imageID != 2 {
		t.Errorf("dirs-only fallback opened image %d, want the lowest free id 2 (hole), not 4",
			s.dirs[9].imageID)
	}
	if res.NewDestinations != 1 {
		t.Errorf("new destinations = %d, want 1", res.NewDestinations)
	}
	if st := s.stateOf(2); st.DirCount != 1 || st.UsedBytes != 0 {
		t.Errorf("image 2 = %+v, want the dir only (dirs carry zero bytes)", st)
	}
}

// --- the change matters: gapped != max+1, and the two mint sites never collide -----

// TestBalanceGappedCatalogDiffersFromMaxPlusOneCounterfactual is the
// NEGATIVE half of the equivalence proof: the contiguous-catalog tests
// above show lowest-unused == max+1 whenever there is no hole, so this
// test proves the two rules DIVERGE the moment there IS one - the
// reason the feature exists. It runs the OLD rule's expectation
// (max(locked)+1) as a counterfactual against the same gapped catalog
// ({1,3} -> old rule says 4) and pins that Balance produces the hole
// (2) instead, at BOTH the placement and the image-row levels.
func TestBalanceGappedCatalogDiffersFromMaxPlusOneCounterfactual(t *testing.T) {
	s := newFakeStore(testProfile(1200))
	addLockedImage(s, 1, budgetUsable(1200))
	addLockedImage(s, 3, budgetUsable(1200)) // image 2 released: max in use = 3
	addFile(s, 1, "new/a", 60)
	addFile(s, 2, "new/b", 40)

	res, err := runPlan(t, s)
	if err != nil {
		t.Fatalf("runPlan: %v", err)
	}
	// The retired max+1 rule's answer for this exact input.
	const maxInUse = int64(3)
	oldRuleNext := maxInUse + 1 // = 4

	got := imageOf(s, 1)
	if got != 2 || imageOf(s, 2) != 2 {
		t.Fatalf("placement = (%d, %d), want both items on the reopened slot 2", imageOf(s, 1), imageOf(s, 2))
	}
	if got == oldRuleNext {
		t.Errorf("gapped catalog minted %d = max+1 (the OLD rule): the hole was not refilled - "+
			"the 4.6 change is unproven", got)
	}
	if res.NewDestinations != 1 {
		t.Errorf("new destinations = %d, want 1 (the hole absorbs the payload)", res.NewDestinations)
	}
	// The image rows prove it at the write level too: {1,2,3} and NO id
	// at or above oldRuleNext.
	for _, id := range []int64{1, 2, 3} {
		if _, ok := s.images[id]; !ok {
			t.Errorf("images row %d missing after the gapped plan", id)
		}
	}
	for id := oldRuleNext; id <= oldRuleNext+4; id++ {
		if _, ok := s.images[id]; ok {
			t.Errorf("images row %d exists: under the old max+1 rule this id would be the first "+
				"mint (%d); lowest-unused must fill the hole %d first", id, oldRuleNext, got)
		}
	}
}

// TestBalanceBothMintSitesNeverMintTheSameID forces BOTH mint sites of
// 4.3/4.6 to fire in ONE Balance run - the WFD placement opener and the
// empty-dir fallback - over a gapped, fully-packed catalog, and pins
// that they share ONE allocator: every new id is distinct (a double
// mint would collide on the images PRIMARY KEY inside the single
// transaction and roll the plan back), and the ids still come out
// lowest-unused ({2, 4} around the locked {1, 3}).
func TestBalanceBothMintSitesNeverMintTheSameID(t *testing.T) {
	// Payload budget 1200; a 688-byte file's charge (688+512) fills an
	// image EXACTLY, so every destination below has ZERO entry slack.
	s := newFakeStore(testProfile(1200))
	addLockedImage(s, 1, budgetUsable(1200))
	addLockedImage(s, 3, budgetUsable(1200)) // image 2 is the hole
	addFile(s, 1, "locked/full-a", 688)
	s.files[1].ImageID = 1
	addFile(s, 2, "locked/full-b", 688)
	s.files[2].ImageID = 3
	addFile(s, 3, "new/payload", 688)                      // fills a NEW image exactly -> site 1 mints 2
	s.dirs[9] = &fakeDir{relPath: "empty/x", active: true} // needs 512 of entry slack

	res, err := runPlan(t, s)
	if err != nil {
		t.Fatalf("runPlan (both mint sites): %v", err)
	}
	// Site 1 (placement) minted the hole 2; site 2 (dirs-only fallback)
	// minted 4 - past the now-in-use 2 and the locked 3. Distinct, and
	// none of them an id the other site already handed out.
	if res.NewDestinations != 2 {
		t.Fatalf("new destinations = %d, want 2 (placement + dir fallback)", res.NewDestinations)
	}
	if imageOf(s, 3) != 2 {
		t.Errorf("new payload landed on image %d, want 2 (the hole, mint site 1)", imageOf(s, 3))
	}
	if s.dirs[9].imageID != 4 {
		t.Errorf("dir fallback opened image %d, want 4 (mint site 2, past the in-use {1,2,3})", s.dirs[9].imageID)
	}
	if s.dirs[9].imageID == imageOf(s, 3) {
		t.Fatalf("both mint sites produced id %d: an id was minted twice in one run", imageOf(s, 3))
	}
	for _, id := range []int64{1, 2, 3, 4} {
		if _, ok := s.images[id]; !ok {
			t.Errorf("images row %d missing after the plan", id)
		}
	}
	if _, ok := s.images[5]; ok {
		t.Errorf("images row 5 exists: spillover beyond the two legitimate mints {2, 4}")
	}
}
