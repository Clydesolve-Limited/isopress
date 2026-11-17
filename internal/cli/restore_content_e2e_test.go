//go:build e2e

package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"isopress/internal/report"
)

// TestRestoreISOImageDirRoundTripRealXorriso (gated on the xorriso binary,
// DESIGN_SPEC 12.6) is THE S5 round trip: a fixture tree goes through the
// real scan, the real plan, the real build (two ISOs mastered by xorriso),
// then a real restore from --image-dir discovery, and the target must
// reproduce the original tree byte for byte - including the chunked parent
// that spans both ISOs, a zero-length file, setuid 04755 AND setgid 02664
// files, a sticky 01777 empty dir (all three asserted as RAW st_mode bits
// through syscall.Stat_t), hard links, valid AND dangling symlinks, an empty
// dir, and nanosecond mtimes - with every selected member counted exactly
// once in the report's totals and no stray name on the target.
func TestRestoreISOImageDirRoundTripRealXorriso(t *testing.T) {
	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skip("xorriso not on PATH; skipping the gated ISO round trip (DESIGN_SPEC 12.6)")
	}

	root := t.TempDir()
	writeFileP(t, filepath.Join(root, "big.bin"), pattern(1, copyBigChunkedLen), 0o644)
	writeFileP(t, filepath.Join(root, "small1.txt"), pattern(2, 1000), 0o600)
	writeFileP(t, filepath.Join(root, "zero.txt"), nil, 0o644) // boundary: zero-length
	writeFileP(t, filepath.Join(root, "nested", "deep.txt"), pattern(3, 700), 0o644)
	writeFileP(t, filepath.Join(root, "suid.sh"), pattern(4, 64), 0o755)
	// Setuid needs the Go FileMode SEMANTICS at plant time: a raw 0o4755
	// handed to WriteFile/Chmod is silently masked to 0755 by the os
	// package (the T1.2 lesson, seen from the fixture side) - real setuid
	// files get their bit from chmod(2)'s raw mode, which this emulates.
	if err := os.Chmod(filepath.Join(root, "suid.sh"), os.FileMode(0o755)|os.ModeSetuid); err != nil {
		t.Fatalf("planting setuid: %v", err)
	}
	// The same conversion, from the other two special bits: a setgid file
	// and a STICKY EMPTY DIRECTORY (the recorded dir kind, FR-037) - the
	// full 04755/02664/01777 spread the metadata chain must reproduce.
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
	// Verify the PLANTS on the source (raw bits): a silent plant failure
	// would otherwise show up as a confusing round-trip diff - or, with a
	// relative comparison, as a vacuous pass.
	assertRawPOSIXMode(t, filepath.Join(root, "suid.sh"), 0o4755)
	assertRawPOSIXMode(t, filepath.Join(root, "setgid.sh"), 0o2664)
	assertRawPOSIXMode(t, filepath.Join(root, "sticky-dir"), 0o1777)
	writeFileP(t, filepath.Join(root, "fam-a.txt"), pattern(5, 800), 0o644)
	if err := os.Link(filepath.Join(root, "fam-a.txt"), filepath.Join(root, "fam-b.txt")); err != nil {
		t.Fatalf("hard-link family: %v", err)
	}
	if err := os.Symlink("small1.txt", filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.Symlink("nowhere.txt", filepath.Join(root, "dangling")); err != nil {
		t.Fatalf("dangling symlink: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "empty-dir"), 0o750); err != nil {
		t.Fatalf("empty dir: %v", err)
	}

	const setID = "S-RRTISO"
	cat := createCatalog(t)
	scanBuildFixture(t, root, cat, setID)
	planSynthetic(t, cat, setID, copySyntheticUsable)
	dest := filepath.Join(t.TempDir(), "dest")
	stdout, stderr, code := runBuildChild(t,
		"build", "--catalog", cat, "--set-id", setID, "--dest", dest)
	if code != report.ExitOK {
		t.Fatalf("build exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	target := filepath.Join(t.TempDir(), "target")
	stdout, stderr, code = runInProcess(t, "restore",
		"--catalog", cat, "--set-id", setID, "--target", target,
		"--image-dir", dest) // 6.4 discovery: <SET-ID>-NNN.iso in the dir
	if code != report.ExitOK {
		t.Fatalf("restore exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	for _, want := range []string{"image 1:", "image 2:", "status: ok"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	assertSameRestoredTree(t, root, target)

	// NB-04 from the report seat: the chunked parent spans BOTH ISOs and
	// still counts once - the totals must name every selected member
	// exactly one time.
	if n, ok := restoredTotal(t, stdout); !ok || n != countRestoreMembers(t, root) {
		t.Errorf("report restored total = %d (parsed=%v), want %d (one count per selected member; the two-image span counted once)\nstdout: %s",
			n, ok, countRestoreMembers(t, root), stdout)
	}

	// THE absolute POSIX contract on the TARGET, read as the kernel stores
	// it (syscall.Stat_t st_mode & 0o7777 - not the Go FileMode rendering):
	// the setuid, setgid, and sticky chains all survived scan -> plan ->
	// build (real xorriso) -> restore.
	assertRawPOSIXMode(t, filepath.Join(target, "suid.sh"), 0o4755)
	assertRawPOSIXMode(t, filepath.Join(target, "setgid.sh"), 0o2664)
	assertRawPOSIXMode(t, filepath.Join(target, "sticky-dir"), 0o1777)

	// The zero-length boundary member really is zero bytes on the target.
	if fi, err := os.Lstat(filepath.Join(target, "zero.txt")); err != nil {
		t.Errorf("zero.txt missing: %v", err)
	} else if fi.Size() != 0 {
		t.Errorf("zero.txt size = %d, want 0", fi.Size())
	}

	// uid/gid and the NANOSECOND mtime, absolute on the target (the fixture
	// files are ours, so a restore that lost the ownership or flattened the
	// mtime to seconds is a failure even when source and target agree on
	// the wrong value).
	assertOwnershipAndMtimeAbsolute(t, filepath.Join(target, "small1.txt"),
		uint32(os.Getuid()), uint32(os.Getgid()), filepath.Join(root, "small1.txt"))
}

// assertOwnershipAndMtimeAbsolute pins the target's OWN uid:gid and its
// nanosecond mtime against the source's recorded value (syscall.Stat_t).
func assertOwnershipAndMtimeAbsolute(t *testing.T, dst string, wantUID, wantGID uint32, src string) {
	t.Helper()
	dstFI, err := os.Lstat(dst)
	if err != nil {
		t.Fatalf("lstat %s: %v", dst, err)
	}
	dstStat, ok := dstFI.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("%s: Sys() is not *syscall.Stat_t", dst)
	}
	if dstStat.Uid != wantUID || dstStat.Gid != wantGID {
		t.Errorf("%s uid:gid = %d:%d, want %d:%d (absolute)", dst, dstStat.Uid, dstStat.Gid, wantUID, wantGID)
	}
	srcFI, err := os.Lstat(src)
	if err != nil {
		t.Fatalf("lstat %s: %v", src, err)
	}
	srcStat, ok := srcFI.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("%s: Sys() is not *syscall.Stat_t", src)
	}
	if dstStat.Mtim.Nano() == 0 || dstStat.Mtim.Nano() != srcStat.Mtim.Nano() {
		t.Errorf("%s mtime = %d ns, want the source's %d ns (nanosecond fidelity, never 0)", dst, dstStat.Mtim.Nano(), srcStat.Mtim.Nano())
	}
}
