//go:build e2e

package xorriso

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// The REAL xorriso on this machine (1.5.6 since pre-flight; the suite is
// gated so other hosts skip cleanly) probes successfully and satisfies
// the >= 1.5.4 floor. The exact version is intentionally NOT pinned:
// the invariant is "parses and clears the floor", per 12.6.
func TestRealXorrisoProbeMeetsFloor(t *testing.T) {
	if _, err := exec.LookPath(ToolName); err != nil {
		t.Skipf("real %s not on PATH; skipping gated probe test (DESIGN_SPEC 12.6)", ToolName)
	}

	tool, err := Discover()
	if err != nil {
		t.Fatalf("Discover (real binary): %v", err)
	}
	info, err := tool.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe against the real binary: %v", err)
	}
	if !info.Version.AtLeast(MinVersion) {
		t.Fatalf("real xorriso version %v does not satisfy the floor %v", info.Version, MinVersion)
	}
	if !strings.Contains(info.Raw, "xorriso") {
		t.Errorf("probe transcript %q does not mention xorriso", info.Raw)
	}
	t.Logf("real xorriso: path=%s version=%s", info.Path, info.Version)
}
