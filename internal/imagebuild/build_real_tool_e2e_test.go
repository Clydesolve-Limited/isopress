//go:build e2e || large

package imagebuild

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"isopress/internal/progress"
	"isopress/internal/verify"
	"isopress/internal/xorriso"
)

// realXorriso adapts the discovered *xorriso.Tool onto the builder's
// two seams (the production cli adapter's shape, build.go in
// internal/cli).
type realXorriso struct{ tool *xorriso.Tool }

func (x realXorriso) Run(ctx context.Context, args []string) ([]byte, error) {
	res, err := x.tool.Run(ctx, args)
	return res.Output, err
}

func (x realXorriso) ExtractAndHash(ctx context.Context, imagePath, destDir string, bus *progress.Bus, totals verify.Totals) (verify.ExtractResult, error) {
	return verify.ExtractAndHash(ctx, x.tool, imagePath, destDir, bus, totals)
}

// realToolOrSkip gates the whole file on the real binary (12.6).
func realToolOrSkip(t *testing.T) realXorriso {
	t.Helper()
	path, err := exec.LookPath("xorriso")
	if err != nil {
		t.Skip("xorriso not on PATH; skipping the gated build integration (DESIGN_SPEC 12.6)")
	}
	return realXorriso{tool: &xorriso.Tool{Path: path}}
}

// mustTime is the injected manifest clock (deterministic).
func mustTime(t *testing.T) time.Time {
	t.Helper()
	return time.Unix(1700000000, 0).UTC()
}
