//go:build e2e

package imagebuild

// THE END-TO-END CAPACITY CLOSER (DESIGN_SPEC 4.3/6.3 step 5): the
// "free after build" figure the plan report shows rests on the
// canonical estimator being CONSERVATIVE for real xorriso output -
// RequiredImageBytes(payload, entries) must be an upper bound on the
// bytes xorriso actually writes. This test pins that with the REAL
// tool: the fixture is built at the TIGHTEST buildable profile
// (UsableSize set to exactly the estimator's requirement, the 12.5
// invariant at equality), and the finalized .iso must be no larger
// than the usable_size it was planned under.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"isopress/internal/units"
)

func TestBuildRealXorrisoISOFitsPlannedUsable(t *testing.T) {
	x := realToolOrSkip(t)
	f := newRealFixture(t)

	// The payload and entry figures EXACTLY as validateInput (and the
	// planner's per-item charges) account for them: files + chunk
	// lengths are payload; files + chunks + symlinks + dirs are
	// entries; the manifest and export are inside the fixed term.
	var payload int64
	var entries int64
	for _, fl := range f.in.Inventory.Files {
		payload += fl.Size
		entries++
	}
	for _, c := range f.in.Inventory.Chunks {
		payload += c.Length
		entries++
	}
	entries += int64(len(f.in.Inventory.Symlinks)) + int64(len(f.in.Inventory.Dirs))
	required := units.RequiredImageBytes(payload, entries)

	// The tightest buildable profile: the pre-check passes at
	// equality, so if the estimator UNDERSTATES real xorriso output,
	// the finalized .iso will overshoot the figure below.
	f.in.UsableSize = required

	deps := Deps{Master: x, Extractor: x, HashStore: f.store}
	res, err := Build(context.Background(), deps, f.in)
	if err != nil {
		t.Fatalf("Build at the exact planned usable_size (%d): %v", required, err)
	}

	finalISO := filepath.Join(f.in.DestDir, "S-REAL-001.iso")
	fi, err := os.Stat(finalISO)
	if err != nil {
		t.Fatalf("final .iso missing: %v", err)
	}
	// THE CLOSER: the real built image fits the profile it was planned
	// under - and the report's "free after build" line (usable minus
	// payload minus reserved overhead) is honest because the estimator
	// over-approximates the real xorriso output, never under.
	if fi.Size() > f.in.UsableSize {
		t.Fatalf("real xorriso wrote %d bytes, exceeding the planned usable_size %d "+
			"(payload %d, entries %d, estimator %d): the canonical estimator is NOT "+
			"conservative for real output - the plan report's free-after-build figure would lie",
			fi.Size(), f.in.UsableSize, payload, entries, required)
	}
	if res.BytesWritten > f.in.UsableSize {
		t.Errorf("BytesWritten = %d, exceeding the planned usable_size %d", res.BytesWritten, f.in.UsableSize)
	}
	// The estimator's slack is visible but bounded: the fixed 2 MiB
	// term dominates, and the actual usage is at least the payload.
	if fi.Size() < payload {
		t.Errorf("final .iso is %d bytes, smaller than its payload %d (impossible)", fi.Size(), payload)
	}
	t.Logf("capacity closer: payload=%d entries=%d planned-usable=%d real-iso=%d slack=%d",
		payload, entries, f.in.UsableSize, fi.Size(), f.in.UsableSize-fi.Size())
}
