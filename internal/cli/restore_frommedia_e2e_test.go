//go:build e2e

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"isopress/internal/media"
	"isopress/internal/report"
)

// TestRestoreFromMediaExtractedISOTreeRealXorriso (gated on xorriso,
// DESIGN_SPEC 12.6) is the extracted-ISO-tree flow of 8.5 end to end
// with the ORIGINAL CATALOG DELETED: a real build masters the ISOs,
// real xorriso osirrox extracts each to a tree (`-extract /` yields
// files/, chunks/, MANIFEST.json, catalog-export.sqlite at the root -
// so the extracted tree IS the image's copy-tree equivalent), and
// restore --from-media reads the trees alone. Two scenarios:
//
//   - a SINGLE-image set whose ONE extracted tree supplies the whole
//     restore by itself;
//   - the two-image set where PATH is image 1's extracted tree and the
//     still-missing image 2 comes through --image (coexistence).
func TestRestoreFromMediaExtractedISOTreeRealXorriso(t *testing.T) {
	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skip("xorriso not on PATH; skipping the gated extracted-tree media restore (DESIGN_SPEC 12.6)")
	}

	// --- scenario 1: the single extracted tree alone ---
	root1 := makeBuildSingleImageTree(t)
	cat1 := createCatalog(t)
	const singleSet = "S-FMISO1"
	scanBuildFixture(t, root1, cat1, singleSet)
	planSynthetic(t, cat1, singleSet, buildFixtureProfile)
	dest1 := filepath.Join(t.TempDir(), "dest")
	if stdout, stderr, code := runBuildChild(t,
		"build", "--catalog", cat1, "--set-id", singleSet, "--dest", dest1); code != 0 {
		t.Fatalf("build exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	tree1 := xorrisoExtractTree(t, filepath.Join(dest1, singleSet+"-001.iso"))
	assertExtractedTreeShape(t, tree1)
	if err := os.Remove(cat1); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog: %v", err)
	}
	before1 := snapshotTree(t, tree1)
	target1 := filepath.Join(t.TempDir(), "target")
	stdout, stderr, code := runInProcess(t, "restore",
		"--from-media", tree1, "--target", target1, "--work-dir", tempWorkDir(t))
	if code != report.ExitOK {
		t.Fatalf("single-extracted-tree restore exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	assertSameRestoredTree(t, root1, target1)
	assertTreeUnchanged(t, before1, tree1)

	// --- scenario 2: the two-image set, PATH is image 1's tree ---
	root2 := makeBuildFixtureTree(t)
	cat2 := createCatalog(t)
	const multiSet = "S-FMISO2"
	scanBuildFixture(t, root2, cat2, multiSet)
	planSynthetic(t, cat2, multiSet, buildFixtureProfile)
	dest2 := filepath.Join(t.TempDir(), "dest")
	if stdout, stderr, code := runBuildChild(t,
		"build", "--catalog", cat2, "--set-id", multiSet, "--dest", dest2); code != 0 {
		t.Fatalf("build exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	isoTree1 := xorrisoExtractTree(t, filepath.Join(dest2, multiSet+"-001.iso"))
	isoTree2 := xorrisoExtractTree(t, filepath.Join(dest2, multiSet+"-002.iso"))
	if err := os.Remove(cat2); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog: %v", err)
	}
	before2 := snapshotTree(t, isoTree1)
	target2 := filepath.Join(t.TempDir(), "target")
	stdout, stderr, code = runInProcess(t, "restore",
		"--from-media", isoTree1, "--image", "2="+isoTree2,
		"--target", target2, "--work-dir", tempWorkDir(t))
	if code != report.ExitOK {
		t.Fatalf("extracted-tree coexistence restore exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	assertSameRestoredTree(t, root2, target2)
	assertTreeUnchanged(t, before2, isoTree1)
}

// xorrisoExtractTree extracts ONE ISO with the real xorriso osirrox
// (the T4.3/T5.4 `-extract /` shape) and returns the extraction root.
func xorrisoExtractTree(t *testing.T, iso string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "extracted")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("extraction dir: %v", err)
	}
	cmd := exec.Command("xorriso", "-indev", iso, "-osirrox", "on", "-extract", "/", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("xorriso extraction of %s: %v\n%s", iso, err, out)
	}
	return dir
}

// assertExtractedTreeShape pins the 8.5 extracted-tree contract: the
// manifest and export at the ROOT, plus the two disjoint payload roots
// (the copy-tree-equivalent layout the restore engine reads).
func assertExtractedTreeShape(t *testing.T, tree string) {
	t.Helper()
	for _, name := range []string{media.ManifestFileName, media.ExportFileName, media.FilesRoot} {
		if _, err := os.Lstat(filepath.Join(tree, name)); err != nil {
			t.Errorf("extracted tree lacks %s at its root: %v", name, err)
		}
	}
}
