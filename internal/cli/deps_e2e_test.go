//go:build e2e

package cli

import (
	"os/exec"
	"testing"
)

// TestGoModVerify runs `go mod verify` over the module (DESIGN_SPEC 11:
// exact pins in go.mod with go mod verify run in CI).
func TestGoModVerify(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH; skipping go mod verify")
	}
	cmd := exec.Command("go", "mod", "verify")
	cmd.Dir = moduleRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go mod verify failed: %v\n%s", err, out)
	}
}
