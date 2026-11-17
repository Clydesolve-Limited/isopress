package copyengine

// T3.4 QA gap-closers (test-only): the delegated audit identified four
// behaviors the T3.3/T3.4 matrix did not yet pin at the writer level:
//
//  1. DEAD PRIMARY (missing): every secondary fails as an item
//     ("stat primary"), nothing lands on the tree, nothing is marked -
//     the family never ends up PARTIAL on the destination.
//  2. UNREADABLE PRIMARY (mode 0000): link(2) does not READ the
//     primary - only the containing directory needs +wx - so the
//     family still completes. The writer must not add a read it does
//     not need.
//  3. CROSS-FILESYSTEM primary (EXDEV from link(2)): the documented
//     degradation is a hard per-item failure naming the errno; the
//     partial-creation-free semantics of link(2) mean no stray file is
//     left, and the row stays unmarked for the next run.
//  4. NO-FOLLOW mtime with a LIVE target: the dangling-symlink test
//     cannot distinguish lstat from stat (stat fails there); this test
//     proves the 5.4 utimensat AT_SYMLINK_NOFOLLOW path touches the
//     LINK and leaves the resolvable target's mtime untouched.
//
// Plus the 4-name nlink extension (the matrix only pinned 3).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// --- dead primary: no partial family ever lands --------------------------------

// A missing primary fails EVERY secondary as an item ("stat primary"),
// leaves no destination file behind, and marks nothing: the writer
// degrades to per-item failures (5.1 crash-recovery: the next run
// re-verifies and re-writes the unmarked members) and never materializes
// a partial family on the tree.
func TestLinkSecondaryMissingPrimaryFailsFamilyWithNoPartial(t *testing.T) {
	store := newFakeStore()
	deps := Deps{Store: store}
	root := t.TempDir()
	deadPrimary := filepath.Join(root, "files", "never-written.bin") // does not exist

	var dests []string
	for i, name := range []string{"sec-b.bin", "sec-c.bin"} {
		dest := filepath.Join(root, "files", name)
		dests = append(dests, dest)
		res := LinkSecondary(context.Background(), deps, LinkItem{
			FileID: int64(301 + i), RelPath: name, MediaPath: "files/" + name,
			DestPath: dest, PrimaryDestPath: deadPrimary, Size: 10,
			Mode: 0o644, MtimeNS: time.Now().UnixNano(),
		})
		if res.Outcome != OutcomeFailed {
			t.Fatalf("secondary %s outcome = %v, want Failed against a dead primary", name, res.Outcome)
		}
		if res.Err == nil || !strings.Contains(res.Err.Error(), "stat primary") {
			t.Fatalf("secondary %s err = %v, want the 'stat primary' classification", name, res.Err)
		}
	}
	// No partial family on the tree: neither secondary path exists.
	for _, dest := range dests {
		if _, err := os.Lstat(dest); !os.IsNotExist(err) {
			t.Fatalf("%s exists after the dead-primary failure (err %v); the writer must leave nothing behind", dest, err)
		}
	}
	// Nothing marked: the next run re-derives and re-writes the family.
	for id := int64(301); id <= 302; id++ {
		if store.fileCopied[id] != "" {
			t.Fatalf("secondary %d marked despite the dead primary", id)
		}
	}
}

// An UNREADABLE primary (mode 0000) is still a valid link(2) source:
// link(2) needs no read permission on the target inode, only +wx on the
// containing directory. The family must complete - the writer must not
// open (and thereby fail on) the primary it only needs to link against.
func TestLinkSecondaryUnreadablePrimaryStillLinks(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("chmod 000 does not deny access to root")
	}
	primary := makeSource(t, "unreadable-family.txt", []byte("bytes nobody may read"))
	primaryDest := filepath.Join(t.TempDir(), "files", "unreadable-family.txt")
	if err := os.MkdirAll(filepath.Dir(primaryDest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFileForTest(primary.path, primaryDest); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(primaryDest, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(primaryDest, 0o644) }) // let TempDir remove it

	store := newFakeStore()
	dest := filepath.Join(t.TempDir(), "files", "unreadable-family-b.txt")
	res := LinkSecondary(context.Background(), Deps{Store: store}, LinkItem{
		FileID: 311, RelPath: "unreadable-family-b.txt", MediaPath: "files/unreadable-family-b.txt",
		DestPath: dest, PrimaryDestPath: primaryDest, Size: primary.meta.Size,
		Mode: 0o644, MtimeNS: primary.meta.MtimeNS,
	})
	if res.Outcome != OutcomeCopied {
		t.Fatalf("outcome = %v, want Copied (link(2) needs no read on the primary; err %v)", res.Outcome, res.Err)
	}
	pi, err1 := os.Lstat(primaryDest)
	si, err2 := os.Lstat(dest)
	if err1 != nil || err2 != nil || !os.SameFile(pi, si) {
		t.Fatalf("secondary does not share the unreadable primary's inode (err %v/%v)", err1, err2)
	}
	if store.fileCopied[311] == "" {
		t.Fatal("secondary row never marked")
	}
}

// --- cross-filesystem primary: the EXDEV verdict --------------------------------

// devOf returns the device id holding path, or 0 when unavailable.
func devOf(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("Sys() for %s is not *syscall.Stat_t: %T", path, fi.Sys())
	}
	return uint64(st.Dev)
}

// otherFSDir returns a writable directory on a DIFFERENT filesystem
// than t.TempDir, or skips the test when the host has none (a container
// with one mounted fs cannot probe EXDEV honestly).
func otherFSDir(t *testing.T) string {
	t.Helper()
	ref := devOf(t, t.TempDir())
	for _, cand := range []string{"/dev/shm", "/run/lock", "/var/tmp", "/tmp", os.TempDir()} {
		if fi, err := os.Stat(cand); err != nil || !fi.IsDir() {
			continue
		}
		if devOf(t, cand) == ref {
			continue
		}
		dir, err := os.MkdirTemp(cand, "bk-t34-exdev-")
		if err != nil {
			continue // not writable; try the next candidate
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		return dir
	}
	t.Skip("no second writable filesystem on this host; EXDEV cannot be probed honestly")
	return ""
}

// The EXDEV verdict (multi-filesystem destinations): link(2) fails with
// EXDEV when the primary and the secondary sit on different filesystems.
// The documented degradation is a HARD PER-ITEM failure that names the
// errno (the run continues; the image is marked incomplete; MANIFEST.json
// is deferred to the completing run), with NO stray file left by the
// atomic link(2) and NO mark on the row.
//
// Note on reachability: the copy pipeline always joins destPath and
// PrimaryDestPath under the SAME image root (cli/copy.go), and
// copySecondaries emits a family's secondaries only on the primary's
// image - so EXDEV is unreachable while an image root is one filesystem.
// It becomes reachable exactly when a single image root itself spans
// mounts (bind-mounts under the media tree). This test pins the
// behavior for that deployment shape.
func TestLinkSecondaryCrossFilesystemPrimaryFailsEXDEV(t *testing.T) {
	otherFS := otherFSDir(t)
	primary := makeSource(t, "exdev-family.txt", []byte("family on another filesystem"))
	primaryDest := filepath.Join(otherFS, "exdev-family.txt")
	if err := copyFileForTest(primary.path, primaryDest); err != nil {
		t.Fatal(err)
	}

	store := newFakeStore()
	dest := filepath.Join(t.TempDir(), "files", "exdev-family-b.txt")
	res := LinkSecondary(context.Background(), Deps{Store: store}, LinkItem{
		FileID: 321, RelPath: "exdev-family-b.txt", MediaPath: "files/exdev-family-b.txt",
		DestPath: dest, PrimaryDestPath: primaryDest, Size: primary.meta.Size,
		Mode: 0o644, MtimeNS: primary.meta.MtimeNS,
	})
	if res.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %v, want Failed (EXDEV is a hard per-item failure, err %v)", res.Outcome, res.Err)
	}
	if !errors.Is(res.Err, syscall.EXDEV) {
		t.Fatalf("err = %v, want it to wrap syscall.EXDEV (the report must name the real cause)", res.Err)
	}
	// link(2) is atomic: on failure nothing was created at the dest.
	if _, err := os.Lstat(dest); !os.IsNotExist(err) {
		t.Fatalf("%s exists after the EXDEV failure (err %v); link(2) must leave no stray file", dest, err)
	}
	if store.fileCopied[321] != "" {
		t.Fatal("secondary row marked despite the EXDEV failure")
	}
}

// --- nlink accounting beyond 3 names -------------------------------------------

// A 4-name family through the real writers: the primary plus THREE
// secondaries share one inode and the primary's nlink equals the family
// size (FR-039) - the general-N shape behind the 3-name matrix case.
func TestLinkSecondaryFamilyOfFourNlinkIdentity(t *testing.T) {
	primary := makeSource(t, "family4.txt", []byte("four-name family content"))
	primaryDest := filepath.Join(t.TempDir(), "files", "family4.txt")
	if err := os.MkdirAll(filepath.Dir(primaryDest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFileForTest(primary.path, primaryDest); err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()
	deps := Deps{Store: store}

	var secondaryDests []string
	for i, name := range []string{"family4-b.txt", "family4-c.txt", "family4-d.txt"} {
		dest := filepath.Join(t.TempDir(), "files", name)
		res := LinkSecondary(context.Background(), deps, LinkItem{
			FileID: int64(331 + i), RelPath: name, MediaPath: "files/" + name,
			DestPath: dest, PrimaryDestPath: primaryDest, Size: primary.meta.Size,
			Mode: 0o644, MtimeNS: primary.meta.MtimeNS,
		})
		if res.Outcome != OutcomeCopied {
			t.Fatalf("secondary %s outcome = %v, want Copied (err %v)", name, res.Outcome, res.Err)
		}
		secondaryDests = append(secondaryDests, dest)
	}

	primaryInfo, err := os.Lstat(primaryDest)
	if err != nil {
		t.Fatal(err)
	}
	for _, dest := range secondaryDests {
		info, err := os.Lstat(dest)
		if err != nil {
			t.Fatal(err)
		}
		if !os.SameFile(primaryInfo, info) {
			t.Fatalf("%s does not share the primary's inode (D-04 link(2))", dest)
		}
	}
	if got := nlinkOf(t, primaryDest); got != 4 {
		t.Fatalf("primary nlink = %d, want 4 (the 4-name family size, FR-039)", got)
	}
}

// --- symlink no-follow with a LIVE target ---------------------------------------

// The dangling-symlink test cannot distinguish lstat from stat (stat
// fails on a dangle), so the no-follow property was only proven on a
// target that does not exist. This test points the recorded target at a
// REAL file with a KNOWN mtime and proves the 5.4 apply path (utimensat
// AT_SYMLINK_NOFOLLOW) stamps the LINK and leaves the resolvable
// target's mtime untouched - the D-03 never-follow policy with an
// observable victim.
func TestWriteSymlinkLiveTargetMtimeUntouched(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, []byte("victim bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	targetMtime := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(target, targetMtime, targetMtime); err != nil {
		t.Fatal(err)
	}
	_, _, targetBefore := statOwnerMtime(t, target)

	store := newFakeStore()
	dest := filepath.Join(root, "files", "link-to-target")
	recorded := time.Now().Add(-time.Hour).UnixNano()
	res := WriteSymlink(context.Background(), Deps{Store: store}, SymlinkItem{
		FileID: 341, RelPath: "link-to-target", MediaPath: "files/link-to-target", DestPath: dest,
		LinkTarget: "../target.txt", Mode: 0o777,
		UID: 0, GID: 0, MtimeNS: recorded,
	})
	if res.Outcome != OutcomeCopied {
		t.Fatalf("outcome = %v, want Copied (err %v)", res.Outcome, res.Err)
	}
	// The LINK carries the recorded mtime (lstat, no-follow apply).
	if _, _, gotNS := statOwnerMtime(t, dest); gotNS != recorded {
		t.Fatalf("link mtime_ns = %d, want the recorded %d (no-follow utimensat)", gotNS, recorded)
	}
	// The resolvable TARGET is untouched: a following chmod/chtimes/
	// chown would have moved its mtime. (UID/GID 0 on the link is
	// self-owned when running unprivileged, so the lchown succeeds
	// and any follow would be observable here too via owner; mtime is
	// the sharp witness.)
	_, _, targetAfter := statOwnerMtime(t, target)
	if targetAfter != targetBefore {
		t.Fatalf("target mtime moved from %d to %d: the symlink apply FOLLOWS the link (D-03 violation)",
			targetBefore, targetAfter)
	}
	if got, err := os.Readlink(dest); err != nil || got != "../target.txt" {
		t.Fatalf("readlink = %q (%v), want the recorded target verbatim", got, err)
	}
}

// Guard against an accidental fixture drift in this file: the EXDEV and
// dead-primary tests assert FAILED outcomes that carry no warning - a
// metadata warning would mean the writer applied metadata to something
// it did not create.
func TestLinkSecondaryFailuresCarryNoWarning(t *testing.T) {
	store := newFakeStore()
	root := t.TempDir()
	res := LinkSecondary(context.Background(), Deps{Store: store}, LinkItem{
		FileID: 351, RelPath: "x", MediaPath: "files/x",
		DestPath:        filepath.Join(root, "files", "x"),
		PrimaryDestPath: filepath.Join(root, "files", "ghost"), Size: 1,
		Mode: 0o4755, UID: 65534, GID: 65534, MtimeNS: time.Now().UnixNano(),
	})
	if res.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %v, want Failed", res.Outcome)
	}
	if res.Warning != "" {
		t.Fatalf("warning = %q, want none: a failed link must not run the metadata apply (EPERM would mask the real error)", res.Warning)
	}
	if fmt.Sprint(res.Err) == "" {
		t.Fatal("failed link carries no error")
	}
}
