package copyengine

// T3.4: the specials' extended creation-semantics matrix (the T3.3
// minimal contracts stay in copyengine_test.go; this file owns the
// delegated brief's acceptance list: dangling-symlink faithful carry,
// symlink-to-directory and chain/cycle lstat semantics, the 3-name
// hard-link family with nlink identity, empty-dir mode/uid/gid/mtime
// round-trips at nanosecond precision, the POSIX->Go MODE CONVERSION
// proof through both the whole-file T3.4 seam and the dir special,
// the 5.4 EPERM-chown warning class on all three specials, and the
// WriteEmptyDir ordering pin - metadata applied BEFORE the durable
// content row, and a row failure leaving the re-assertable state the
// next run repairs).
//
// Every test drives the REAL writers against t.TempDir with only the
// catalog (fakeStore) faked - the filesystem discipline (mkdir,
// link(2), chmod, lchown, utimensat, fsync) is production code.
//
// MODE VALUES ON THE ITEM STRUCTS ARE RAW POSIX BITS (the catalog
// files.mode/directories.mode convention, DESIGN_SPEC 2.3): 0o4755
// means setuid+0755 here, NOT Go's fs.ModeSetuid. metaFromRaw
// (copyengine.go, the single applyMetadata seam) converts them to Go
// fs.FileMode semantics via fsmeta.GoMode before fsmeta.Apply sees
// them; passing a raw POSIX value to os.Chmod unconverted would drop
// the special bits (Perm() only yields the low 9 bits) - the tests
// below pin that the conversion happens by stat-asserting the raw
// POSIX bits ON DISK after the write.

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

// --- helpers ----------------------------------------------------------------

// rawModeOf lstats path and returns the kernel's raw POSIX permission
// bits (all 12: rwx + setuid/setgid/sticky) - the ground truth the
// mode-conversion tests compare against.
func rawModeOf(t *testing.T, path string) uint32 {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("Sys() for %s is not *syscall.Stat_t: %T", path, fi.Sys())
	}
	return st.Mode & 0o7777
}

// statOwnerMtime returns uid, gid, and mtime in unix nanoseconds as
// the kernel records them (the FR-036 field set WriteEmptyDir must
// reproduce exactly).
func statOwnerMtime(t *testing.T, path string) (uid, gid uint32, mtimeNS int64) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("Sys() for %s is not *syscall.Stat_t: %T", path, fi.Sys())
	}
	return st.Uid, st.Gid, int64(st.Mtim.Sec)*1_000_000_000 + int64(st.Mtim.Nsec)
}

// nlinkOf returns the hard-link count of path.
func nlinkOf(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("Sys() for %s is not *syscall.Stat_t: %T", path, fi.Sys())
	}
	return uint64(st.Nlink)
}

func selfIDs() (uid, gid uint32) {
	return uint32(os.Getuid()), uint32(os.Getgid())
}

// --- symlink semantics (D-03: preserve, never follow) ------------------------

// A symlink whose target is a DIRECTORY is copied as a LINK, not
// descended into a tree: the destination entry is a symlink carrying
// the recorded target verbatim, and nothing else lands in its place.
func TestWriteSymlinkToDirectoryCopiedAsLink(t *testing.T) {
	targetDir := filepath.Join(t.TempDir(), "somewhere", "deep")
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()
	dest := filepath.Join(t.TempDir(), "files", "dir-link")
	res := WriteSymlink(context.Background(), Deps{Store: store}, SymlinkItem{
		FileID: 101, RelPath: "dir-link", MediaPath: "files/dir-link", DestPath: dest,
		LinkTarget: targetDir, Mode: 0o777, MtimeNS: time.Now().UnixNano(),
	})
	if res.Outcome != OutcomeCopied {
		t.Fatalf("outcome = %v, want Copied (err %v)", res.Outcome, res.Err)
	}
	fi, err := os.Lstat(dest)
	if err != nil {
		t.Fatalf("lstat of the recreated link: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("destination is %v, want a symlink (lstat semantics: a link to a dir is a link, not a dir)", fi.Mode())
	}
	if got, _ := os.Readlink(dest); got != targetDir {
		t.Fatalf("readlink = %q, want the recorded dir target %q", got, targetDir)
	}
	if store.fileCopied[101] == "" {
		t.Fatal("symlink row never marked")
	}
}

// A symlink CYCLE (a->b, b->a) is copied as two plain links: the
// writer never resolves targets, so there is nothing to hang on, and
// both links carry their recorded targets verbatim.
func TestWriteSymlinkChainCycleNoFollow(t *testing.T) {
	store := newFakeStore()
	root := t.TempDir()
	deps := Deps{Store: store}

	resA := WriteSymlink(context.Background(), deps, SymlinkItem{
		FileID: 111, RelPath: "a", MediaPath: "files/a", DestPath: filepath.Join(root, "files", "a"),
		LinkTarget: "b", Mode: 0o777, MtimeNS: time.Now().UnixNano(),
	})
	resB := WriteSymlink(context.Background(), deps, SymlinkItem{
		FileID: 112, RelPath: "b", MediaPath: "files/b", DestPath: filepath.Join(root, "files", "b"),
		LinkTarget: "a", Mode: 0o777, MtimeNS: time.Now().UnixNano(),
	})
	if resA.Outcome != OutcomeCopied || resB.Outcome != OutcomeCopied {
		t.Fatalf("cycle outcomes = %v/%v, want Copied/Copied (err %v/%v)",
			resA.Outcome, resB.Outcome, resA.Err, resB.Err)
	}
	for _, tc := range []struct{ path, want string }{
		{filepath.Join(root, "files", "a"), "b"},
		{filepath.Join(root, "files", "b"), "a"},
	} {
		fi, err := os.Lstat(tc.path)
		if err != nil {
			t.Fatalf("lstat %s: %v", tc.path, err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s is %v, want a symlink (a cycle must be carried as links, never walked)", tc.path, fi.Mode())
		}
		if got, _ := os.Readlink(tc.path); got != tc.want {
			t.Fatalf("readlink %s = %q, want %q", tc.path, got, tc.want)
		}
	}
}

// A dangling symlink target (D-03 restored-as-link semantics): the
// copy tree carries the BROKEN link faithfully - the link exists with
// the recorded target, the target does NOT exist in the copy tree
// (nothing was invented to "fix" the dangle), and the link's own
// mtime is preserved at nanosecond precision via the no-follow path.
func TestWriteSymlinkDanglingCarriedFaithfully(t *testing.T) {
	store := newFakeStore()
	dest := filepath.Join(t.TempDir(), "files", "dangling")
	recorded := time.Now().Add(-time.Hour).UnixNano()
	res := WriteSymlink(context.Background(), Deps{Store: store}, SymlinkItem{
		FileID: 121, RelPath: "dangling", MediaPath: "files/dangling", DestPath: dest,
		LinkTarget: "does/not/exist.txt", Mode: 0o777,
		UID: 0, GID: 0, MtimeNS: recorded,
	})
	if res.Outcome != OutcomeCopied {
		t.Fatalf("outcome = %v, want Copied (err %v)", res.Outcome, res.Err)
	}
	fi, err := os.Lstat(dest)
	if err != nil {
		t.Fatalf("lstat of the dangling link: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("destination is %v, want a symlink", fi.Mode())
	}
	if got, _ := os.Readlink(dest); got != "does/not/exist.txt" {
		t.Fatalf("readlink = %q, want the recorded dangling target verbatim", got)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("stat of the dangling link = %v, want ENOENT (the dangle must be carried, not materialized)", err)
	}
	// The link's OWN mtime (lstat, no-follow apply), ns precision.
	if _, _, gotNS := statOwnerMtime(t, dest); gotNS != recorded {
		t.Fatalf("link mtime_ns = %d, want the recorded %d (no-follow utimensat, 5.4)", gotNS, recorded)
	}
}

// The 5.4 EPERM class on a symlink: a foreign-uid lchown is a
// WARNING, the link still completes as Copied with its mark and its
// recorded target - never a failure, never an unmark.
func TestWriteSymlinkEPERMChownWarningStillCopied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("EPERM cannot be provoked as root")
	}
	store := newFakeStore()
	dest := filepath.Join(t.TempDir(), "files", "perm-link")
	res := WriteSymlink(context.Background(), Deps{Store: store}, SymlinkItem{
		FileID: 131, RelPath: "perm-link", MediaPath: "files/perm-link", DestPath: dest,
		LinkTarget: "target.txt", Mode: 0o777,
		UID: 65534, GID: 65534, // nobody/nogroup: foreign, EPERM as non-root
		MtimeNS: time.Now().UnixNano(),
	})
	if res.Outcome != OutcomeCopied {
		t.Fatalf("outcome = %v, want Copied (5.4: EPERM chown is a warning; err %v)", res.Outcome, res.Err)
	}
	if !strings.Contains(res.Warning, "chown") {
		t.Fatalf("warning = %q, want the chown downgrade note", res.Warning)
	}
	if store.fileCopied[131] == "" {
		t.Fatal("a metadata warning unmarked the symlink; 5.4 forbids it")
	}
	if got, _ := os.Readlink(dest); got != "target.txt" {
		t.Fatalf("readlink = %q, want the recorded target (link intact after the warning)", got)
	}
}

// --- hard-link families (D-04, FR-039) --------------------------------------

// The 3-name family through the real writers: the primary is written
// ONCE, both secondaries land via link(2), all three names share one
// inode (os.SameFile), the primary's nlink equals the family size,
// and every name reads the same bytes.
func TestLinkSecondaryFamilyOfThreeNlinkIdentity(t *testing.T) {
	primary := makeSource(t, "family.txt", []byte("shared family content"))
	primaryDest := filepath.Join(t.TempDir(), "files", "family.txt")
	if err := os.MkdirAll(filepath.Dir(primaryDest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFileForTest(primary.path, primaryDest); err != nil {
		t.Fatal(err)
	}
	uid, gid := selfIDs()
	store := newFakeStore()
	deps := Deps{Store: store}

	var secondaryDests []string
	for i, name := range []string{"family-b.txt", "family-c.txt"} {
		dest := filepath.Join(t.TempDir(), "files", name)
		res := LinkSecondary(context.Background(), deps, LinkItem{
			FileID: int64(141 + i), RelPath: name, MediaPath: "files/" + name,
			DestPath: dest, PrimaryDestPath: primaryDest, Size: primary.meta.Size,
			Mode: 0o644, UID: uid, GID: gid, MtimeNS: primary.meta.MtimeNS,
		})
		if res.Outcome != OutcomeCopied {
			t.Fatalf("secondary %s outcome = %v, want Copied (err %v)", name, res.Outcome, res.Err)
		}
		if store.fileCopied[int64(141+i)] == "" {
			t.Fatalf("secondary %s row never marked", name)
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
	// The tree-level nlink assertion: the primary's inode carries the
	// whole family - nlink == 3 (1 primary + 2 secondaries).
	if got := nlinkOf(t, primaryDest); got != 3 {
		t.Fatalf("primary nlink = %d, want 3 (the family size, FR-039)", got)
	}
	// Same bytes under every name.
	for _, p := range append([]string{primaryDest}, secondaryDests...) {
		got, err := os.ReadFile(p)
		if err != nil || string(got) != "shared family content" {
			t.Fatalf("content via %s = %q (%v), want the shared family bytes", p, got, err)
		}
	}
}

// The 5.4 EPERM class on a hard-link secondary: foreign-uid chown is
// a WARNING; the link is created, inode-identical, and marked.
func TestLinkSecondaryEPERMChownWarningStillCopied(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("EPERM cannot be provoked as root")
	}
	primary := makeSource(t, "perm-family.txt", []byte("shared"))
	primaryDest := filepath.Join(t.TempDir(), "files", "perm-family.txt")
	if err := os.MkdirAll(filepath.Dir(primaryDest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := copyFileForTest(primary.path, primaryDest); err != nil {
		t.Fatal(err)
	}
	store := newFakeStore()
	dest := filepath.Join(t.TempDir(), "files", "perm-family-b.txt")
	res := LinkSecondary(context.Background(), Deps{Store: store}, LinkItem{
		FileID: 151, RelPath: "perm-family-b.txt", MediaPath: "files/perm-family-b.txt",
		DestPath: dest, PrimaryDestPath: primaryDest, Size: primary.meta.Size,
		Mode: 0o644, UID: 65534, GID: 65534, MtimeNS: primary.meta.MtimeNS,
	})
	if res.Outcome != OutcomeCopied {
		t.Fatalf("outcome = %v, want Copied (5.4: EPERM chown is a warning; err %v)", res.Outcome, res.Err)
	}
	if !strings.Contains(res.Warning, "chown") {
		t.Fatalf("warning = %q, want the chown downgrade note", res.Warning)
	}
	if store.fileCopied[151] == "" {
		t.Fatal("a metadata warning unmarked the secondary; 5.4 forbids it")
	}
	pi, err1 := os.Lstat(primaryDest)
	si, err2 := os.Lstat(dest)
	if err1 != nil || err2 != nil || !os.SameFile(pi, si) {
		t.Fatalf("secondary does not share the primary's inode after the warning (err %v/%v)", err1, err2)
	}
}

// --- empty dirs (FR-037) -----------------------------------------------------

// WriteEmptyDir reproduces the recorded mode, uid, gid, AND mtime at
// nanosecond precision on the destination directory, for the full
// mode-edge table (plain, owner-only, sticky tmp-like, setgid, and
// the all-specials 07777) - the stat-based round-trip proof.
func TestWriteEmptyDirRecordedMetadataRoundTrip(t *testing.T) {
	uid, gid := selfIDs() // self-chown: applies cleanly as non-root
	recorded := time.Now().Add(-48 * time.Hour).UnixNano()
	for _, mode := range []uint32{0o755, 0o700, 0o1777, 0o2775, 0o7777} {
		t.Run(fmt.Sprintf("mode_%04o", mode), func(t *testing.T) {
			store := newFakeStore()
			dest := filepath.Join(t.TempDir(), "files", "empty-dir")
			res := WriteEmptyDir(context.Background(), Deps{Store: store}, DirItem{
				DirID: 161, RelPath: "empty-dir", MediaPath: "files/empty-dir", DestPath: dest,
				Mode: mode, UID: uid, GID: gid, MtimeNS: recorded,
			})
			if res.Outcome != OutcomeCopied {
				t.Fatalf("outcome = %v, want Copied (err %v)", res.Outcome, res.Err)
			}
			if res.Warning != "" {
				t.Fatalf("self-owned dir write warned: %q", res.Warning)
			}
			fi, err := os.Lstat(dest)
			if err != nil || !fi.IsDir() {
				t.Fatalf("directory not created (err=%v)", err)
			}
			if got := rawModeOf(t, dest); got != mode {
				t.Fatalf("dir raw mode = %#o, want the recorded %#o (POSIX->Go conversion, FR-037)", got, mode)
			}
			gotUID, gotGID, gotNS := statOwnerMtime(t, dest)
			if gotUID != uid || gotGID != gid {
				t.Fatalf("dir owner = %d:%d, want %d:%d (FR-036)", gotUID, gotGID, uid, gid)
			}
			if gotNS != recorded {
				t.Fatalf("dir mtime_ns = %d, want the recorded %d (nanosecond precision, 5.4)", gotNS, recorded)
			}
			if store.dirContent[161] != "files/empty-dir" {
				t.Fatalf("dir content rows = %v, want dir 161 recorded", store.dirContent)
			}
		})
	}
}

// Directories carry NO copied flag, so their metadata is re-asserted
// EVERY run: a destination dir whose mode drifted (or was never
// metadata'd because a crash landed between mkdir and Apply) is
// restored to the recorded state by the next run. This is the
// repair property the WriteEmptyDir ordering doc leans on.
func TestWriteEmptyDirReassertsMetadataEveryRun(t *testing.T) {
	store := newFakeStore()
	dest := filepath.Join(t.TempDir(), "files", "reassert")
	uid, gid := selfIDs()
	item := DirItem{
		DirID: 171, RelPath: "reassert", MediaPath: "files/reassert", DestPath: dest,
		Mode: 0o700, UID: uid, GID: gid, MtimeNS: time.Now().UnixNano(),
	}
	if res := WriteEmptyDir(context.Background(), Deps{Store: store}, item); res.Outcome != OutcomeCopied {
		t.Fatalf("first run: outcome = %v, want Copied (err %v)", res.Outcome, res.Err)
	}
	// Simulate drift: widen the mode and reset ownership/mtime as a
	// partial write or an external tool would.
	if err := os.Chmod(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	item.MtimeNS = time.Now().Add(-time.Hour).UnixNano()
	if res := WriteEmptyDir(context.Background(), Deps{Store: store}, item); res.Outcome != OutcomeCopied {
		t.Fatalf("re-run: outcome = %v, want Copied (err %v)", res.Outcome, res.Err)
	}
	if got := rawModeOf(t, dest); got != 0o700 {
		t.Fatalf("dir raw mode after re-run = %#o, want the re-asserted 0700", got)
	}
	if _, _, gotNS := statOwnerMtime(t, dest); gotNS != item.MtimeNS {
		t.Fatalf("dir mtime after re-run = %d, want the re-asserted %d", gotNS, item.MtimeNS)
	}
}

// The 5.4 EPERM class on an empty dir: foreign-uid chown is a
// WARNING; the dir is created with its recorded MODE (chmod runs
// after the failed lchown - the 5.4 ordering), the content row is
// still recorded, and the item completes as Copied.
func TestWriteEmptyDirEPERMChownWarningStillRecordsRow(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("EPERM cannot be provoked as root")
	}
	store := newFakeStore()
	dest := filepath.Join(t.TempDir(), "files", "perm-dir")
	res := WriteEmptyDir(context.Background(), Deps{Store: store}, DirItem{
		DirID: 181, RelPath: "perm-dir", MediaPath: "files/perm-dir", DestPath: dest,
		Mode: 0o2775, UID: 65534, GID: 65534, // nobody/nogroup: foreign, EPERM as non-root
		MtimeNS: time.Now().UnixNano(),
	})
	if res.Outcome != OutcomeCopied {
		t.Fatalf("outcome = %v, want Copied (5.4: EPERM chown is a warning; err %v)", res.Outcome, res.Err)
	}
	if !strings.Contains(res.Warning, "chown") {
		t.Fatalf("warning = %q, want the chown downgrade note", res.Warning)
	}
	if store.dirContent[181] == "" {
		t.Fatal("a metadata warning blocked the dir content row; 5.4 forbids it")
	}
	if got := rawModeOf(t, dest); got != 0o2775 {
		t.Fatalf("dir raw mode = %#o, want the recorded 02775 (chmod after failed chown, 5.4 ordering)", got)
	}
}

// orderProbeStore lstats the destination dir AT SetDirContent time
// and captures the raw mode it sees - the ordering witness for
// "metadata applied BEFORE the durable content row".
type orderProbeStore struct {
	*fakeStore
	probePath string
	seenMode  uint32
	modeSeen  bool
	onContent func() error
}

func (s *orderProbeStore) SetDirContent(ctx context.Context, dirID int64, mediaPath string) error {
	if s.onContent != nil {
		if err := s.onContent(); err != nil {
			return err
		}
	}
	if s.probePath != "" {
		if fi, err := os.Lstat(s.probePath); err == nil {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok {
				s.seenMode = st.Mode & 0o7777
				s.modeSeen = true
			}
		}
	}
	return s.fakeStore.SetDirContent(ctx, dirID, mediaPath)
}

// The WriteEmptyDir ordering pin: at the moment the durable content
// row is recorded, the directory ON DISK already carries its recorded
// mode - metadata precedes the row (mkdir -> Apply -> fsync -> row,
// the file doc's T3.4 decision), so the recorded member and its
// recorded state become durable together.
func TestWriteEmptyDirMetadataAppliedBeforeContentRow(t *testing.T) {
	probe := &orderProbeStore{fakeStore: newFakeStore()}
	dest := filepath.Join(t.TempDir(), "files", "ordered")
	probe.probePath = dest
	uid, gid := selfIDs()
	res := WriteEmptyDir(context.Background(), Deps{Store: probe}, DirItem{
		DirID: 191, RelPath: "ordered", MediaPath: "files/ordered", DestPath: dest,
		Mode: 0o2775, UID: uid, GID: gid, MtimeNS: time.Now().UnixNano(),
	})
	if res.Outcome != OutcomeCopied {
		t.Fatalf("outcome = %v, want Copied (err %v)", res.Outcome, res.Err)
	}
	if !probe.modeSeen {
		t.Fatal("probe never saw the destination dir at SetDirContent time")
	}
	if probe.seenMode != 0o2775 {
		t.Fatalf("dir raw mode at content-row time = %#o, want the recorded 02775 (metadata must precede the row)", probe.seenMode)
	}
}

// A content-row failure leaves exactly the re-assertable state the
// ordering doc promises: the dir EXISTS with its recorded metadata
// applied (metadata was not rolled back - it is a warning-only path),
// no row is recorded, and the next run completes the record without
// disturbing the dir.
func TestWriteEmptyDirRowFailureLeavesReassertableState(t *testing.T) {
	rowErr := errors.New("injected SetDirContent failure")
	failing := &orderProbeStore{fakeStore: newFakeStore()}
	failing.onContent = func() error { return rowErr }
	dest := filepath.Join(t.TempDir(), "files", "rowfail")
	failing.probePath = dest
	uid, gid := selfIDs()
	item := DirItem{
		DirID: 201, RelPath: "rowfail", MediaPath: "files/rowfail", DestPath: dest,
		Mode: 0o750, UID: uid, GID: gid, MtimeNS: time.Now().UnixNano(),
	}
	res := WriteEmptyDir(context.Background(), Deps{Store: failing}, item)
	if res.Outcome != OutcomeFailed || !errors.Is(res.Err, rowErr) {
		t.Fatalf("outcome = %v (err %v), want Failed naming the row failure", res.Outcome, res.Err)
	}
	if _, err := os.Lstat(dest); err != nil {
		t.Fatalf("destination dir missing after the row failure: %v (metadata-first makes this the re-assertable state)", err)
	}
	if got := rawModeOf(t, dest); got != 0o750 {
		t.Fatalf("dir raw mode after the row failure = %#o, want the applied 0750", got)
	}
	if failing.dirContent[201] != "" {
		t.Fatal("row recorded despite the injected failure")
	}
	// The next run (healthy store) completes the record.
	healthy := newFakeStore()
	res2 := WriteEmptyDir(context.Background(), Deps{Store: healthy}, item)
	if res2.Outcome != OutcomeCopied {
		t.Fatalf("re-run outcome = %v, want Copied (err %v)", res2.Outcome, res2.Err)
	}
	if healthy.dirContent[201] != "files/rowfail" {
		t.Fatalf("dir content rows after re-run = %v, want dir 201 recorded", healthy.dirContent)
	}
}

// --- the POSIX->Go MODE CONVERSION proof (the T1.2 QA carry-in) --------------

// The whole-file T3.4 seam (copyengine.go applyMetadata/metaFromRaw)
// converts the catalog's RAW POSIX mode to Go fs.FileMode semantics
// before os.Chmod sees it: every special-bit mode below survives to
// the destination FILE verbatim. If the conversion were skipped (raw
// 0o4755 handed to os.Chmod), Perm() would yield only 0o755 and the
// stat below would fail - this is the end-to-end unit proof that the
// T1.2 QA finding cannot regress.
func TestWholeFileSpecialModeRoundTripThroughSeam(t *testing.T) {
	uid, gid := selfIDs()
	for _, mode := range []uint32{0o4755, 0o2755, 0o1755, 0o7777} {
		t.Run(fmt.Sprintf("mode_%04o", mode), func(t *testing.T) {
			src := makeSource(t, "special.txt", []byte("special mode payload"))
			store := newFakeStore()
			item := wholeItem(src, 211, filepath.Join(t.TempDir(), "files", "special.txt"))
			item.Mode = mode // raw POSIX bits, catalog convention
			item.UID, item.GID = uid, gid
			res := CopyItem(context.Background(), Deps{Store: store}, item)
			if res.Outcome != OutcomeCopied {
				t.Fatalf("outcome = %v, want Copied (err %v)", res.Outcome, res.Err)
			}
			if res.Warning != "" {
				t.Fatalf("self-owned copy warned: %q", res.Warning)
			}
			if got := rawModeOf(t, item.DestPath); got != mode {
				t.Fatalf("dest raw mode = %#o, want the recorded %#o (POSIX->Go conversion must survive os.Chmod)", got, mode)
			}
		})
	}
}

// The same conversion proof through the DIR special: the recorded
// raw POSIX bits land verbatim on the destination directory.
func TestWriteEmptyDirSpecialModeConversionProof(t *testing.T) {
	uid, gid := selfIDs()
	for _, mode := range []uint32{0o4755, 0o1755, 0o2775, 0o7777} {
		t.Run(fmt.Sprintf("mode_%04o", mode), func(t *testing.T) {
			store := newFakeStore()
			dest := filepath.Join(t.TempDir(), "files", "conv-dir")
			res := WriteEmptyDir(context.Background(), Deps{Store: store}, DirItem{
				DirID: 221, RelPath: "conv-dir", MediaPath: "files/conv-dir", DestPath: dest,
				Mode: mode, UID: uid, GID: gid, MtimeNS: time.Now().UnixNano(),
			})
			if res.Outcome != OutcomeCopied {
				t.Fatalf("outcome = %v, want Copied (err %v)", res.Outcome, res.Err)
			}
			if got := rawModeOf(t, dest); got != mode {
				t.Fatalf("dir raw mode = %#o, want the recorded %#o", got, mode)
			}
		})
	}
}
