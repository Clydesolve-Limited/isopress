//go:build e2e

package cli

import (
	"syscall"
	"testing"
)

// realSignalsStressIterations is the per-signal depth of the
// single-signal stress loop: the defect-B race fired only INTERMITTENTLY
// (5 of 2000 iterations under -race), so the reviewer's acceptance bar
// is a few hundred fresh-child single-signal runs per invocation - at
// 200 per signal, one invocation is 400 real signal runs and -count=5
// is exactly the 2000-run volume at which B1 used to surface.
const realSignalsStressIterations = 200

// TestRealSignalsMapToExit130Stress is the reviewer-demanded STRESS
// version of the exit-130 pin: N iterations per signal, a FRESH child
// process each time (no shared state between runs), asserting exit 130
// AND the ABSENCE of the forcing-exit line on every single run. This is
// what the intermittent defect-B failure (a single ^C self-killed via
// the cross-channel drain race) looked like when it hit; this loop
// exists to make its recurrence loud and immediate.
func TestRealSignalsMapToExit130Stress(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			for i := 0; i < realSignalsStressIterations; i++ {
				runSignalHelperOnce(t, sig)
				if t.Failed() {
					t.Fatalf("single-signal contract violated at stress iteration %d/%d", i+1, realSignalsStressIterations)
				}
			}
		})
	}
}
