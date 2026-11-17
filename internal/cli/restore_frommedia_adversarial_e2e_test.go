//go:build e2e

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"isopress/internal/report"
)

// TestRestoreFromMediaExplicitISOBesideMediaRealXorriso (gated on xorriso,
// DESIGN_SPEC 12.6) pins the documented coexistence clause for the ISO
// shape: PATH is image 1's EXTRACTED tree (media base) while image 2 is an
// .iso sitting BESIDE it, supplied through the literal 8.3 --image N=PATH
// mapping (stat classifies it as an ISO, xorriso extracts it). The set is
// restored with the ORIGINAL CATALOG DELETED.
func TestRestoreFromMediaExplicitISOBesideMediaRealXorriso(t *testing.T) {
	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skip("xorriso not on PATH; skipping the gated ISO-beside-media restore (DESIGN_SPEC 12.6)")
	}

	root := makeBuildFixtureTree(t)
	cat := createCatalog(t)
	const setID = "S-FMISOB"
	scanBuildFixture(t, root, cat, setID)
	planSynthetic(t, cat, setID, buildFixtureProfile)
	dest := filepath.Join(t.TempDir(), "dest")
	if stdout, stderr, code := runBuildChild(t,
		"build", "--catalog", cat, "--set-id", setID, "--dest", dest); code != 0 {
		t.Fatalf("build exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	iso2 := filepath.Join(dest, fmt.Sprintf("%s-002.iso", setID))
	tree1 := xorrisoExtractTree(t, filepath.Join(dest, fmt.Sprintf("%s-001.iso", setID)))
	assertExtractedTreeShape(t, tree1)
	if err := os.Remove(cat); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog: %v", err)
	}
	before := snapshotTree(t, tree1)
	target := filepath.Join(t.TempDir(), "target")

	stdout, stderr, code := runInProcess(t, "restore",
		"--from-media", tree1, "--image", "2="+iso2,
		"--target", target, "--work-dir", tempWorkDir(t))
	if code != report.ExitOK {
		t.Fatalf("ISO-beside-media restore exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "image 1: restored "+tree1) {
		t.Errorf("stdout must name the MEDIA TREE as image 1's source:\n%s", stdout)
	}
	if !strings.Contains(stdout, "image 2: restored "+iso2) {
		t.Errorf("stdout must name the ISO as image 2's source:\n%s", stdout)
	}
	if !strings.Contains(stdout, "(iso)") {
		t.Errorf("stdout must show image 2's ISO source kind:\n%s", stdout)
	}
	assertSameRestoredTree(t, root, target)
	assertTreeUnchanged(t, before, tree1)
}

// --- the mediaStore unit seat (the fill's three branches) ---------------------
