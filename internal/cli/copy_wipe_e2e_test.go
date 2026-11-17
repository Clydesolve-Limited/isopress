//go:build e2e

package cli

// U.3 at the ISO / mixed-mode tier (DESIGN_SPEC 4.6 + 4.6.1's
// COPY-MODE scoping; xorriso-gated per 12.6):
//
//	AC-12  unlock -> plan -> build renames over the old .iso at the
//	       same path; a failed build leaves the old .iso byte-identical;
//	       verify-media full-matches; and NO wipe code path is ever
//	       exercised in build (the build path never calls the gate -
//	       observable here as: zero wipe run_events rows AND the
//	       copy-mode trees under the same --dest left byte-identical).
//	AC-25  mixed mode (M3): unlock -> build -> copy -> verify exits 10
//	       naming the unexpected files (loud, NO wipe - BeginBuildTx
//	       materializes the content list at build start, so the gate
//	       stands down); unlock -> build -> verify full-matches (the
//	       ISO path is atomic rename-over and never wipes).

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"isopress/internal/media"
	"isopress/internal/report"
)

// isoInode is the .iso file's inode - the rename-over proof (a
// regenerated file at the SAME path carries a NEW inode; an untouched
// one keeps it).
func isoInode(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("%s: Sys() is not *syscall.Stat_t", path)
	}
	return st.Ino
}

// shaOfPath is the byte-identity anchor for the failed-build leg.
func shaOfPath(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestUnlockBuildRenamesOverIsoAndNeverWipes is AC-12 end to end:
// scan -> plan -> COPY (so a real copy tree shares the --dest with the
// .iso files) -> build -> `clear --last` -> plan -> build again.
func TestUnlockBuildRenamesOverIsoNeverWipes(t *testing.T) {
	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skip("xorriso not on PATH; skipping the gated build e2e (DESIGN_SPEC 12.6)")
	}
	const setID = "S-AC12"
	cat := createCatalog(t)
	root := makeBuildFixtureTree(t)
	scanBuildFixture(t, root, cat, setID)
	planSynthetic(t, cat, setID, buildFixtureProfile)
	dest := filepath.Join(t.TempDir(), "dest")

	// A COPY run first: the copy-mode trees live under the same --dest
	// the .iso files will, so "build never wipes" is observable.
	stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json")
	if code != report.ExitOK {
		t.Fatalf("copy exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	ids := plannedImageIDs(t, cat, setID)
	if len(ids) != 2 {
		t.Fatalf("planned images = %v, want two (the fixture's premise)", ids)
	}
	copyBefore := map[int64]map[string]mediaMember{}
	for _, n := range ids {
		copyBefore[n] = snapshotTree(t, destImageDir(dest, setID, n))
	}

	// build run 1: both .iso files land at <dest>/<SET-ID>-NNN.iso.
	stdout, stderr, code = runBuildChild(t,
		"build", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json")
	if code != report.ExitOK {
		t.Fatalf("build 1 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	isoOf := func(n int64) string {
		name, err := media.ImageISOFileName(setID, n)
		if err != nil {
			t.Fatalf("iso file name for image %d: %v", n, err)
		}
		return filepath.Join(dest, name)
	}
	for _, n := range ids {
		if _, err := os.Stat(isoOf(n)); err != nil {
			t.Fatalf("image %d .iso missing after build 1: %v", n, err)
		}
	}
	image1IsoBefore := isoInode(t, isoOf(1))
	image2IsoBefore := isoInode(t, isoOf(2))

	// unlock --last (image 2), re-plan (media count unchanged), build 2.
	stdout, stderr, code = runClearChild(t, "--catalog", cat, "--set-id", setID, "--last")
	if code != report.ExitOK {
		t.Fatalf("clear --last exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	planSynthetic(t, cat, setID, buildFixtureProfile)
	if ids := plannedImageIDs(t, cat, setID); len(ids) != 2 || ids[1] != 2 {
		t.Fatalf("re-plan ids = %v, want [1 2] (the released id re-mints)", ids)
	}

	stdout, stderr, code = runBuildChild(t,
		"build", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json")
	if code != report.ExitOK {
		t.Fatalf("build 2 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// THE RENAME-OVER: image 2's .iso exists at the SAME path with a NEW
	// inode (atomic rename, 6.3 step 8); image 1's was rewritten too by
	// the all-images build, but never wiped or truncated in place.
	if got := isoInode(t, isoOf(2)); got == image2IsoBefore {
		t.Errorf("image 2 .iso inode unchanged (%d): the rebuilt image must arrive by atomic rename-over", got)
	}
	if _, err := os.Stat(isoOf(2) + ".partial"); !os.IsNotExist(err) {
		t.Errorf("build 2 left a .partial behind (err=%v)", err)
	}
	if got := isoInode(t, isoOf(1)); got == image1IsoBefore {
		t.Logf("image 1 .iso inode unchanged (%d): acceptable - rename-over is only required for the image actually rebuilt", got)
	}

	// NO WIPE ANYWHERE IN BUILD: zero wipe run_events rows in the whole
	// catalog, and the copy-mode trees under the same --dest are
	// byte-identical (the gate is structurally copy-only - build never
	// calls it).
	if n := countAllWipeEvents(t, cat); n != 0 {
		t.Errorf("wipe run_events rows after two builds = %d, want 0 (build never exercises the wipe gate)", n)
	}
	for _, n := range plannedImageIDs(t, cat, setID) {
		assertTreeUnchanged(t, copyBefore[n], destImageDir(dest, setID, n))
	}

	// verify-media full-match (7.2 stamp included) on the ISO media.
	assertVerifyFullMatch(t, cat, setID, 2)

	// A FAILED build leaves the old .iso BYTE-IDENTICAL (the fake xorriso
	// masters the .partial and then fails the extraction self-verify, so
	// the atomic rename never happens).
	beforeFailed := shaOfPath(t, isoOf(2))
	fakePath, marker := installFakeXorriso(t)
	stdout, stderr, code = runBuildChildEnv(t, []string{"PATH=" + fakePath},
		"build", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json")
	if code != report.ExitItemFailures {
		t.Fatalf("failing build exit = %d, want 10\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the fake extraction was never invoked: %v", err)
	}
	if after := shaOfPath(t, isoOf(2)); after != beforeFailed {
		t.Errorf("the failed build CHANGED the old .iso: %s -> %s (it must be byte-identical)", beforeFailed, after)
	}
	if n := countAllWipeEvents(t, cat); n != 0 {
		t.Errorf("wipe run_events rows after the failed build = %d, want 0", n)
	}
}

// TestMixedModeBuildCopyDoesNotWipe is AC-25: the gate keys on
// COPY-MODE generations only (BeginBuildTx materializes the content
// list at build START), so the mixed sequence writes additively over
// the stale copy tree and fails loudly at VERIFY - while the pure ISO
// sequence after an unlock is clean.
func TestMixedModeBuildCopyDoesNotWipe(t *testing.T) {
	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skip("xorriso not on PATH; skipping the gated build e2e (DESIGN_SPEC 12.6)")
	}

	// LEG 1: unlock -> build -> copy -> verify exits 10 naming the
	// unexpected files (the stale member the new content list no longer
	// holds), with NO wipe anywhere.
	t.Run("unlock -> build -> copy -> verify fails loudly, no wipe", func(t *testing.T) {
		const setID = "S-AC25A"
		cat := createCatalog(t)
		root := makeBuildSingleImageTree(t) // small1.txt, small2.txt, link, empty-dir
		scanBuildFixture(t, root, cat, setID)
		planSynthetic(t, cat, setID, buildFixtureProfile)
		dest := filepath.Join(t.TempDir(), "dest")

		// The FIRST copy: a complete copy tree, contents full.
		stdout, stderr, code := runCopyChild(t, nil,
			"copy", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json")
		if code != report.ExitOK {
			t.Fatalf("copy 1 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		// Release image 1, drop small2.txt from the source, rescan,
		// re-plan: the released image's NEW inventory no longer holds it,
		// but its bytes are still in the stale copy tree.
		if stdout, stderr, code := runClearChild(t,
			"--catalog", cat, "--set-id", setID, "--image", "1"); code != report.ExitOK {
			t.Fatalf("clear --image 1 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		if err := os.Remove(filepath.Join(root, "small2.txt")); err != nil {
			t.Fatalf("removing the source member: %v", err)
		}
		scanBuildFixture(t, root, cat, setID)
		planSynthetic(t, cat, setID, buildFixtureProfile)

		// The BUILD fills image 1's content list at build START (the
		// mixed-mode premise the gate's copy-mode scoping keys on).
		stdout, stderr, code = runBuildChild(t,
			"build", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json")
		if code != report.ExitOK {
			t.Fatalf("build exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		var contents int
		if err := catOpen(t, cat).QueryRow(`SELECT COUNT(*) FROM image_contents
			WHERE set_id = ? AND image_id = 1`, setID).Scan(&contents); err != nil {
			t.Fatal(err)
		}
		if contents == 0 {
			t.Fatal("the build left an empty content list: the mixed-mode premise is missing")
		}
		stalePath := filepath.Join(destImageDir(dest, setID, 1), "files", "small2.txt")
		if _, err := os.Stat(stalePath); err != nil {
			t.Fatalf("the stale copy-tree member is gone before the mixed run: %v", err)
		}

		// THE MIXED COPY: contents non-empty -> the gate stands down (NO
		// wipe), the copy writes additively over the stale tree.
		stdout, stderr, code = runCopyChild(t, nil,
			"copy", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json")
		if code != report.ExitOK {
			t.Fatalf("mixed copy exit = %d, want 0 (the write itself succeeds; verify is the "+
				"detector)\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		rep := parseCopyReport(t, stdout)
		if len(rep.Wipes) != 0 {
			t.Errorf("mixed-mode run recorded wipes %+v, want NONE (build filled the content list)", rep.Wipes)
		}
		if _, err := os.Stat(stalePath); err != nil {
			t.Errorf("the stale member vanished although the gate must not wipe (err=%v)", err)
		}
		if n := countAllWipeEvents(t, cat); n != 0 {
			t.Errorf("wipe run_events rows = %d, want 0 in the mixed sequence", n)
		}

		// VERIFY IS THE DETECTOR: exit 10 naming the unexpected file.
		stdout, stderr, code = runVerifyMediaChild(t, nil,
			"verify-media", "--catalog", cat, "--set-id", setID)
		if code != report.ExitItemFailures {
			t.Fatalf("verify after the mixed sequence exit = %d, want 10\nstdout: %s\nstderr: %s",
				code, stdout, stderr)
		}
		if !strings.Contains(stdout, "unexpected") && !strings.Contains(stderr, "unexpected") {
			t.Errorf("verify must name the unexpected file:\nstdout: %s\nstderr: %s", stdout, stderr)
		}
	})

	// LEG 2: unlock -> build -> verify full-matches (the ISO path is
	// atomic rename-over; it never wipes and never needs to).
	t.Run("unlock -> build -> verify full-matches", func(t *testing.T) {
		const setID = "S-AC25B"
		cat := createCatalog(t)
		root := makeBuildSingleImageTree(t)
		scanBuildFixture(t, root, cat, setID)
		planSynthetic(t, cat, setID, buildFixtureProfile)
		dest := filepath.Join(t.TempDir(), "dest")

		if stdout, stderr, code := runBuildChild(t,
			"build", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json"); code != report.ExitOK {
			t.Fatalf("build 1 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		if stdout, stderr, code := runClearChild(t,
			"--catalog", cat, "--set-id", setID, "--last"); code != report.ExitOK {
			t.Fatalf("clear --last exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		planSynthetic(t, cat, setID, buildFixtureProfile)
		if stdout, stderr, code := runBuildChild(t,
			"build", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json"); code != report.ExitOK {
			t.Fatalf("build 2 exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		if n := countAllWipeEvents(t, cat); n != 0 {
			t.Errorf("wipe run_events rows = %d, want 0 (the build path never wipes)", n)
		}
		assertVerifyFullMatch(t, cat, setID, 1)
	})
}
