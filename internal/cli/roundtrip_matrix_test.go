package cli

// T5.6 — THE FULL ROUND-TRIP SUITE (DESIGN_SPEC 12.4 over the 12.2
// fixture strategy), composed end to end at the COMMAND level for BOTH
// modes:
//
//	copy tree: scan -> plan -> copy -> verify-media -> restore -> tree compare
//	ISO:       scan -> plan -> build -> verify-media -> restore -> tree compare
//	           (gated on xorriso, DESIGN_SPEC 12.6 — skips cleanly when absent)
//
// Each mode then repeats the loop's FINAL STEP as the DISTINCT
// media-alone case (the acceptance's wording): the original catalog
// file is MOVED AWAY and restore runs from media alone — export +
// manifest only — through the copy-set directory flow (copy mode) and
// the extracted-ISO-tree flow (ISO mode).
//
// COMPOSITION GAPS THIS FILE FILLS (Phase-1 audit): verify-media never
// ran BEFORE a restore in any committed test; no single test composed
// the write -> verify -> restore chain; no CLI round-trip fixture
// carried a unicode or dot filename member; the ISO and media-alone
// targets were compared only by nlink, never by INODE-RELATIONSHIP on
// the restored names. One composed fixture per mode covers the whole
// matrix, and each mode's media is built ONCE and shared by both of its
// cases (only ONE xorriso build for both ISO legs — no duplicate ISO
// mastering in this suite).
//
// The composed fixture (roundTripRichTree) carries every 12.2/12.4
// member kind in one tree: nested dirs (3 levels), an empty dir (0750)
// AND a sticky empty dir (01777), a valid AND a dangling symlink, a
// hard-link family asserted on the TARGET by INODE-RELATIONSHIP (same
// st_dev+st_ino, nlink correct), a unicode filename, dot filenames (a
// leading-dot file and a dot directory component), a zero-length
// boundary file, setuid 04755 and setgid 02664 files asserted as RAW
// POSIX bits through syscall.Stat_t, and a chunked parent spanning
// BOTH images under the established copySyntheticUsable plan profile.
//
// Assertions on every restored target: assertSameRestoredTree
// (byte-identical content, symlinks by target, metadata STAT-FOR-STAT
// mode/uid/gid/nanosecond-mtime, hard-link nlink, and the reverse
// no-stray check), the absolute raw POSIX modes, the family's dev+ino
// relationship, the unicode/dot/zero members by content, and the
// report's count-once totals. No sleeps anywhere: every step is a real
// command child or the in-process seam, in chain order.

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"isopress/internal/report"
)

// --- the composed rich fixture (DESIGN_SPEC 12.2 member spread) ---------------

const (
	// rtUnicodeName is the unicode member of the rich fixture: multi-byte
	// UTF-8 (Latin diacritics + CJK), under 31 bytes so it is legal at
	// every ISO level even before Rock Ridge, and valid UTF-8 so the T3.1
	// manifest gate accepts it (raw-byte names are refused by design).
	rtUnicodeName = "ünïcödé-日本語.txt"

	// rtUnicodeBody / rtHiddenBody / rtDotDirBody are the members' exact
	// payloads — asserted BY CONTENT on the restored targets so a member
	// that restored under a sanitized name (U+FFFD) or empty bytes fails
	// loudly even though a walk-compare alone might phrase it differently.
	rtUnicodeBody = 10 // pattern seed
	rtHiddenBody  = 11 // pattern seed (leading-dot file)
	rtDotDirBody  = 12 // pattern seed (dot-directory member)
)

// roundTripRichTree writes the T5.6 rich source tree: every fixture
// member the 12.4 acceptance names, in ONE tree (12.2). The special
// bits are planted through chmod(2)'s raw mode — the T1.2 lesson: a raw
// 0o4755 handed to os.WriteFile/os.Chmod is silently masked by the os
// package, so the plants are VERIFIED on the source before the run
// starts (a dead plant would otherwise show as a vacuous round trip).
func roundTripRichTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()

	// The chunked parent: 1,352,000 bytes = chunk 901,312 + 450,688
	// under copySyntheticUsable — neither chunk fits beside the other,
	// so the file SPANS both images in either packing order.
	writeFileP(t, filepath.Join(root, "big.bin"), pattern(1, copyBigChunkedLen), 0o644)
	writeFileP(t, filepath.Join(root, "small1.txt"), pattern(2, 1000), 0o644)
	writeFileP(t, filepath.Join(root, "zero.txt"), nil, 0o644) // zero-length boundary
	// Nested dirs, three levels deep (12.2 "nested directories").
	writeFileP(t, filepath.Join(root, "nested", "deep.txt"), pattern(3, 700), 0o644)
	writeFileP(t, filepath.Join(root, "nested", "deeper", "leaf.txt"), pattern(8, 1337), 0o644)
	// The unicode member (12.4: unicode filenames).
	writeFileP(t, filepath.Join(root, rtUnicodeName), pattern(rtUnicodeBody, 1777), 0o644)
	// Dot filenames (12.4): a leading-dot file at the root and a dot
	// DIRECTORY component — both legal per media.ValidateRelPath (only
	// "." and ".." components are refused).
	writeFileP(t, filepath.Join(root, ".hidden"), pattern(rtHiddenBody, 42), 0o600)
	writeFileP(t, filepath.Join(root, ".dotdir", "inside.txt"), pattern(rtDotDirBody, 55), 0o644)

	// setuid 04755 file — chmod(2) raw mode, plant verified below.
	writeFileP(t, filepath.Join(root, "suid.sh"), pattern(4, 64), 0o755)
	if err := os.Chmod(filepath.Join(root, "suid.sh"), os.FileMode(0o755)|os.ModeSetuid); err != nil {
		t.Fatalf("planting setuid: %v", err)
	}
	// setgid 02664 file and a STICKY empty dir (01777): the full special
	// bit spread the metadata chain must reproduce.
	writeFileP(t, filepath.Join(root, "setgid.sh"), pattern(7, 64), 0o644)
	if err := os.Chmod(filepath.Join(root, "setgid.sh"), os.FileMode(0o664)|os.ModeSetgid); err != nil {
		t.Fatalf("planting setgid: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "sticky-dir"), 0o777); err != nil {
		t.Fatalf("sticky dir: %v", err)
	}
	if err := os.Chmod(filepath.Join(root, "sticky-dir"), os.FileMode(0o777)|os.ModeSticky); err != nil {
		t.Fatalf("planting sticky: %v", err)
	}
	// A hard-link family (2 names -> nlink 2 on both sides).
	writeFileP(t, filepath.Join(root, "fam-a.txt"), pattern(5, 800), 0o644)
	if err := os.Link(filepath.Join(root, "fam-a.txt"), filepath.Join(root, "fam-b.txt")); err != nil {
		t.Fatalf("hard-link family: %v", err)
	}
	// Valid AND dangling symlinks (D-03: both restore as links with the
	// recorded target).
	if err := os.Symlink("small1.txt", filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.Symlink("nowhere.txt", filepath.Join(root, "dangling")); err != nil {
		t.Fatalf("dangling symlink: %v", err)
	}
	// A plain empty dir (0750), the recorded kind (FR-037).
	if err := os.Mkdir(filepath.Join(root, "empty-dir"), 0o750); err != nil {
		t.Fatalf("empty dir: %v", err)
	}

	// Verify the PLANTS on the source (raw bits): a silently dropped
	// special bit would otherwise surface as a confusing round-trip diff
	// — or, with a relative comparison, as a vacuous pass.
	assertRawPOSIXMode(t, filepath.Join(root, "suid.sh"), 0o4755)
	assertRawPOSIXMode(t, filepath.Join(root, "setgid.sh"), 0o2664)
	assertRawPOSIXMode(t, filepath.Join(root, "sticky-dir"), 0o1777)
	return root
}

// roundTripMatrixFixture scans and PLANS the rich tree through the real
// commands (the copy_test.go pattern: the REAL plan pipeline with only
// the capacity profile substituted), returning the catalog path and the
// source root. The mode-specific WRITE step (copy or build) is driven by
// each composed test so the chain order stays visible in the test body.
func roundTripMatrixFixture(t *testing.T, setID string) (cat, root string) {
	t.Helper()
	root = roundTripRichTree(t)
	cat = createCatalog(t)
	scanBuildFixture(t, root, cat, setID)
	planSynthetic(t, cat, setID, copySyntheticUsable)
	return cat, root
}

// --- the restored-target contract ---------------------------------------------

// assertFamilyInodeRelationship stats every restored family name on the
// TARGET and requires ONE st_dev+st_ino shared by all of them with the
// exact hard-link count — the acceptance's INODE-RELATIONSHIP comparison
// read straight from syscall.Stat_t (os.SameFile is the dev+ino test;
// this spells the raw stat fields out so the assertion states exactly
// what it proves: link(2) recreation, never byte-identical copies).
func assertFamilyInodeRelationship(t *testing.T, target string, names []string, wantNlink uint64) {
	t.Helper()
	var wantDev, wantIno uint64
	for i, name := range names {
		fi, err := os.Lstat(filepath.Join(target, name))
		if err != nil {
			t.Fatalf("lstat restored family member %s: %v", name, err)
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			t.Fatalf("%s: Sys() is not *syscall.Stat_t", name)
		}
		if i == 0 {
			wantDev, wantIno = uint64(st.Dev), st.Ino
		} else if uint64(st.Dev) != wantDev || st.Ino != wantIno {
			t.Errorf("%s dev:ino = %d:%d, want the family's %d:%d (INODE-RELATIONSHIP on the restored target)",
				name, uint64(st.Dev), st.Ino, wantDev, wantIno)
		}
		if st.Nlink != wantNlink {
			t.Errorf("%s nlink = %d, want %d (the family must share exactly these links)", name, st.Nlink, wantNlink)
		}
	}
}

// assertRoundTripTarget is THE 12.4 target contract over the rich
// fixture:
//
//   - assertSameRestoredTree: byte-identical content for every member,
//     symlinks compared BY TARGET, metadata STAT-FOR-STAT (raw POSIX
//     mode incl. special bits, uid/gid, nanosecond mtime), source nlink
//     preserved, and the reverse walk proving no stray name (a leaked
//     .partial or a tool-owned member fails here);
//   - the hard-link family BY INODE-RELATIONSHIP on the restored names
//     (dev+ino identity + nlink), content pinned;
//   - the special bits ABSOLUTE on the target (raw syscall.Stat_t
//     st_mode & 0o7777 — never a source/target comparison alone, which
//     would let a dropped bit pass when both sides degrade together);
//   - the boundary members: zero-length stays zero bytes, the unicode
//     and dot members exist under their EXACT names with their exact
//     bytes (a U+FFFD-sanitized rename or an empty body fails here).
func assertRoundTripTarget(t *testing.T, root, target string) {
	t.Helper()

	// Bytes, symlink targets, stat-for-stat metadata, nlink, no strays.
	assertSameRestoredTree(t, root, target)

	// THE ACCEPTANCE'S INODE-RELATIONSHIP HARD LINE on the target.
	assertFamilyInodeRelationship(t, target, []string{"fam-a.txt", "fam-b.txt"}, 2)
	assertBytes(t, filepath.Join(target, "fam-a.txt"), pattern(5, 800))
	assertBytes(t, filepath.Join(target, "fam-b.txt"), pattern(5, 800))

	// Raw POSIX bits, absolute, on the restored names.
	assertRawPOSIXMode(t, filepath.Join(target, "suid.sh"), 0o4755)
	assertRawPOSIXMode(t, filepath.Join(target, "setgid.sh"), 0o2664)
	assertRawPOSIXMode(t, filepath.Join(target, "sticky-dir"), 0o1777)

	// The unicode and dot members, by content, under their exact names.
	assertBytes(t, filepath.Join(target, rtUnicodeName), pattern(rtUnicodeBody, 1777))
	assertBytes(t, filepath.Join(target, ".hidden"), pattern(rtHiddenBody, 42))
	assertBytes(t, filepath.Join(target, ".dotdir", "inside.txt"), pattern(rtDotDirBody, 55))

	// The zero-length boundary member really is zero bytes.
	if fi, err := os.Lstat(filepath.Join(target, "zero.txt")); err != nil {
		t.Errorf("zero.txt missing: %v", err)
	} else if fi.Size() != 0 {
		t.Errorf("zero.txt size = %d, want 0", fi.Size())
	}
}

// assertRestoredTotalsCountOnce pins the NB-04 count-once contract from
// the report seat: every selected member counts exactly once, the
// two-image chunked span included.
func assertRestoredTotalsCountOnce(t *testing.T, stdout, root string) {
	t.Helper()
	if n, ok := restoredTotal(t, stdout); !ok || n != countRestoreMembers(t, root) {
		t.Errorf("report restored total = %d (parsed=%v), want %d (one count per selected member)\nstdout: %s",
			n, ok, countRestoreMembers(t, root), stdout)
	}
}

// assertVerifyMediaClean drives verify-media over the set and requires
// the FULL clean verdict for both images plus the 7.2 stamp in the
// catalog — the chain-order proof that verification PASSED before any
// restore below it runs.
func assertVerifyMediaClean(t *testing.T, cat, setID, workDir, source string) {
	t.Helper()
	stdout, stderr, code := runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID, "--work-dir", workDir)
	if code != report.ExitOK {
		t.Fatalf("verify-media exit = %d, want 0 (media must verify CLEAN before the restore chain proceeds)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	for _, want := range []string{
		"verify-media: set " + setID,
		"image 1: verified ", "image 2: verified ",
		"(" + source + ")",
		"totals: images 2, verified 2",
		"stamped true",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("verify-media stdout lacks %q:\n%s", want, stdout)
		}
	}
	// The catalog half of the stamp: BOTH images landed 'verified' with
	// verified_at set — recorded before the first restore runs.
	db := catOpen(t, cat)
	var stamped int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM images WHERE set_id = ? AND state = 'verified' AND verified_at IS NOT NULL`,
		setID).Scan(&stamped); err != nil {
		t.Fatalf("reading the verify stamp: %v", err)
	}
	if stamped != 2 {
		t.Errorf("stamped-verified images = %d, want 2 (verify-media must stamp both before restore)", stamped)
	}
}

// TestRoundTripMatrixCopyTree composes the 12.4 chain for COPY-TREE mode
// over the rich fixture in ONE test:
//
//	scan -> plan -> copy -> verify-media -> restore (catalog present)
//	then the DISTINCT media-alone case: the original catalog is MOVED
//	AWAY and restore runs from the copy-set directory alone.
//
// Both cases byte-compare the fresh target against the source with the
// full contract of assertRoundTripTarget (inode relationship, symlink
// targets, stat-for-stat metadata, raw POSIX bits, unicode/dot/zero
// members, count-once totals), and the media comes out byte-identical
// (the read-only proof across verify AND both restores).
func TestRoundTripMatrixCopyTree(t *testing.T) {
	const setID = "S-RTCOPY"
	cat, root := roundTripMatrixFixture(t, setID)
	dest := filepath.Join(t.TempDir(), "dest")
	work := tempWorkDir(t)

	// WRITE: the real copy child through the production execute path.
	stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest)
	if code != report.ExitOK {
		t.Fatalf("copy exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// The write really spanned BOTH images (the fixture's premise).
	for _, n := range []int64{1, 2} {
		if fi, err := os.Stat(destImageDir(dest, setID, n)); err != nil || !fi.IsDir() {
			t.Fatalf("copy set dir for image %d missing: %v", n, err)
		}
	}
	// Media read-only baseline: every later phase (verify, restore,
	// media-alone restore) must leave the copy set byte-identical.
	mediaBefore := snapshotTree(t, dest)

	// VERIFY-MEDIA, BETWEEN the write and the FIRST restore — the 12.4
	// chain order no committed test composed before T5.6.
	assertVerifyMediaClean(t, cat, setID, work, "copy")

	t.Run("restore with the catalog present", func(t *testing.T) {
		target := restoreTarget(t)
		stdout, stderr, code := runInProcess(t, "restore",
			"--catalog", cat, "--set-id", setID, "--target", target,
			"--copy-tree-root", dest, "--work-dir", work)
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

	// THE DISTINCT MEDIA-ALONE CASE: move the original catalog file
	// away (the acceptance's wording) and prove it absent at its path
	// before the media-only restore starts.
	movedCat := cat + ".moved-away"
	if err := os.Rename(cat, movedCat); err != nil {
		t.Fatalf("moving the original catalog away (the decisive media-alone step): %v", err)
	}
	if _, err := os.Stat(cat); !os.IsNotExist(err) {
		t.Fatalf("the original catalog must be ABSENT at %s (stat err = %v)", cat, err)
	}

	t.Run("media-alone restore with the catalog moved away", func(t *testing.T) {
		target := restoreTarget(t)
		stdout, stderr, code := runInProcess(t, "restore",
			"--from-media", dest, "--target", target, "--work-dir", work)
		if code != report.ExitOK {
			t.Fatalf("restore --from-media exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		// No --set-id either: the set identity came FROM the media.
		for _, want := range []string{"restore: set " + setID, "image 1:", "image 2:", "status: ok"} {
			if !strings.Contains(stdout, want) {
				t.Errorf("media-alone stdout lacks %q:\n%s", want, stdout)
			}
		}
		assertRoundTripTarget(t, root, target)
		assertRestoredTotalsCountOnce(t, stdout, root)
	})

	// THE MEDIA READ-ONLY PROOF across the whole composed loop.
	assertTreeUnchanged(t, mediaBefore, dest)
}

// --- ISO MODE: the composed full loop (gated on xorriso) -----------------------
