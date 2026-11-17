//go:build e2e

package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"isopress/internal/media"
	"isopress/internal/report"
)

// assertExtractedMediaTreeShape pins 8.5's media-tree contract for the
// extracted trees of THIS fixture: MANIFEST.json and
// catalog-export.sqlite at the root, plus at least one payload root.
//
// T5.5's assertExtractedTreeShape additionally requires files/ — true
// for its fixtures (their images carry whole files), but under the rich
// fixture chunk 0 EXACTLY fills image 1's payload budget, so image 1 is
// legitimately a CHUNK-ONLY image whose extracted tree has no files/
// root at all ("the tree is created by the paths it contains", 5.3 —
// an image root holds no empty payload directories). The media-alone
// restore below proves both trees supply their payload byte-exact, so
// this shape check stays honest without demanding a root an image
// never held.
func assertExtractedMediaTreeShape(t *testing.T, tree string) {
	t.Helper()
	for _, name := range []string{media.ManifestFileName, media.ExportFileName} {
		if _, err := os.Lstat(filepath.Join(tree, name)); err != nil {
			t.Errorf("extracted tree lacks %s at its root: %v", name, err)
		}
	}
	payloadRoots := 0
	for _, name := range []string{media.FilesRoot, media.ChunksRoot} {
		if _, err := os.Lstat(filepath.Join(tree, name)); err == nil {
			payloadRoots++
		}
	}
	if payloadRoots == 0 {
		t.Errorf("extracted tree %s has neither %s/ nor %s/ at its root", tree, media.FilesRoot, media.ChunksRoot)
	}
}

// --- COPY-TREE MODE: the composed full loop -----------------------------------

// TestRoundTripMatrixISO composes the 12.4 chain for ISO mode over the
// rich fixture shape of the copy-mode case (a fresh instance per test,
// same member spread) in ONE test (gated on xorriso per DESIGN_SPEC
// 12.6 — t.Skip when absent, never a hard fail):
//
//	scan -> plan -> build (real xorriso) -> verify-media -> restore
//	(from --image-dir discovery; catalog present)
//	then the DISTINCT media-alone case: the original catalog is MOVED
//	AWAY and restore runs from the EXTRACTED ISO TREES alone
//	(--from-media + --image 2=… coexistence flow of 8.5).
//
// The ISOs are mastered ONCE and shared by both cases — this suite adds
// exactly one build, not one per leg.
func TestRoundTripMatrixISO(t *testing.T) {
	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skip("xorriso not on PATH; skipping the gated ISO round-trip matrix (DESIGN_SPEC 12.6)")
	}
	const setID = "S-RTISO"
	cat, root := roundTripMatrixFixture(t, setID)
	dest := filepath.Join(t.TempDir(), "dest")
	work := tempWorkDir(t)

	// WRITE: the real build child mastering both ISOs with real xorriso.
	stdout, stderr, code := runBuildChild(t,
		"build", "--catalog", cat, "--set-id", setID, "--dest", dest)
	if code != report.ExitOK {
		t.Fatalf("build exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	var iso1, iso2 string
	for i, dst := range []*string{&iso1, &iso2} {
		*dst = filepath.Join(dest, fmt.Sprintf("%s-%03d.iso", setID, i+1))
		if fi, err := os.Stat(*dst); err != nil || !fi.Mode().IsRegular() {
			t.Fatalf("image %d final .iso missing: %v", i+1, err)
		}
		if _, err := os.Stat(*dst + ".partial"); !os.IsNotExist(err) {
			t.Fatalf("image %d left a .partial behind (stat err = %v)", i+1, err)
		}
	}
	// The media read-only baseline: verify (extraction!) and both
	// restores must leave the ISO directory byte-identical.
	mediaBefore := snapshotTree(t, dest)

	// VERIFY-MEDIA, BETWEEN the build and the FIRST restore — the
	// xorriso-extraction verification leg of the 12.4 chain.
	assertVerifyMediaClean(t, cat, setID, work, "iso")

	t.Run("restore with the catalog present", func(t *testing.T) {
		target := restoreTarget(t)
		stdout, stderr, code := runInProcess(t, "restore",
			"--catalog", cat, "--set-id", setID, "--target", target,
			"--image-dir", dest, "--work-dir", work)
		if code != report.ExitOK {
			t.Fatalf("restore exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		for _, want := range []string{"image 1:", "image 2:", "status: ok"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("restore stdout lacks %q:\n%s", want, stdout)
			}
		}
		assertRoundTripTarget(t, root, target)
		assertRestoredTotalsCountOnce(t, stdout, root)
	})

	// THE DISTINCT MEDIA-ALONE CASE: extract both ISOs to trees (the
	// 8.5 extracted-ISO-tree flow), then move the original catalog file
	// away and prove it absent before the media-only restore starts.
	tree1 := xorrisoExtractTree(t, iso1)
	tree2 := xorrisoExtractTree(t, iso2)
	assertExtractedMediaTreeShape(t, tree1)
	assertExtractedMediaTreeShape(t, tree2)
	movedCat := cat + ".moved-away"
	if err := os.Rename(cat, movedCat); err != nil {
		t.Fatalf("moving the original catalog away (the decisive media-alone step): %v", err)
	}
	if _, err := os.Stat(cat); !os.IsNotExist(err) {
		t.Fatalf("the original catalog must be ABSENT at %s (stat err = %v)", cat, err)
	}
	treeBefore1 := snapshotTree(t, tree1)
	treeBefore2 := snapshotTree(t, tree2)

	t.Run("media-alone restore from the extracted trees with the catalog moved away", func(t *testing.T) {
		target := restoreTarget(t)
		stdout, stderr, code := runInProcess(t, "restore",
			"--from-media", tree1, "--image", "2="+tree2,
			"--target", target, "--work-dir", work)
		if code != report.ExitOK {
			t.Fatalf("restore --from-media (extracted trees) exit = %d, want 0\nstdout: %s\nstderr: %s",
				code, stdout, stderr)
		}
		for _, want := range []string{"restore: set " + setID, "image 1:", "image 2:", "status: ok"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("media-alone stdout lacks %q:\n%s", want, stdout)
			}
		}
		assertRoundTripTarget(t, root, target)
		assertRestoredTotalsCountOnce(t, stdout, root)
	})

	// THE MEDIA READ-ONLY PROOF: both extracted trees and the ISO
	// directory come through the whole composed loop untouched.
	assertTreeUnchanged(t, treeBefore1, tree1)
	assertTreeUnchanged(t, treeBefore2, tree2)
	assertTreeUnchanged(t, mediaBefore, dest)
}
