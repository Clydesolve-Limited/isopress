package fsmeta

// T1.2 tests: classification of all four entry kinds, metadata capture
// round-trip, change-detection independence of the four fixed fields
// (size, mtime_ns, dev, inode), symlink target reading for valid AND
// dangling links (D-03), and metadata application including the
// EPERM-as-non-root chown downgrade rule of DESIGN_SPEC 5.4.
//
// Stdlib only; every fixture lives in t.TempDir().

import (
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// --- test doubles for the graceful-failure paths -------------------------

// fakeDirEntry implements fs.DirEntry with controllable Type/Info so the
// "type unknown" fallback of ClassifyDirEntry can be exercised without
// depending on a filesystem that reports DT_UNKNOWN.
type fakeDirEntry struct {
	fi      fs.FileInfo
	typ     fs.FileMode
	infoErr error
}

func (f fakeDirEntry) Name() string               { return f.fi.Name() }
func (f fakeDirEntry) IsDir() bool                { return f.fi.IsDir() }
func (f fakeDirEntry) Type() fs.FileMode          { return f.typ }
func (f fakeDirEntry) Info() (fs.FileInfo, error) { return f.fi, f.infoErr }

// nonStatFileInfo wraps a real FileInfo but reports a Sys() value that is
// not *syscall.Stat_t, proving Capture returns an error instead of
// panicking on the Linux stat assertion.
type nonStatFileInfo struct{ fs.FileInfo }

func (nonStatFileInfo) Sys() any { return nil }

// --- classification (DESIGN_SPEC 3.1) ------------------------------------

func TestClassifyAllFourKinds(t *testing.T) {
	dir := t.TempDir()

	regular := filepath.Join(dir, "regular.txt")
	if err := os.WriteFile(regular, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(dir, "subdir")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink("regular.txt", link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		if errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.EPERM) {
			t.Skipf("mkfifo unsupported in this environment: %v", err)
		}
		t.Fatalf("mkfifo: %v", err)
	}

	cases := []struct {
		name string
		path string
		want EntryKind
	}{
		{"regular", regular, KindRegular},
		{"directory", subdir, KindDirectory},
		{"symlink", link, KindSymlink},
		{"fifo is special", fifo, KindSpecial},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fi, err := os.Lstat(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			if got := Classify(fi); got != tc.want {
				t.Errorf("Classify = %v, want %v", got, tc.want)
			}
		})
	}

	// String values must match the catalog entry_kind vocabulary for the
	// two kinds the catalog stores (DESIGN_SPEC 2.3).
	if got := KindRegular.String(); got != "regular" {
		t.Errorf("KindRegular.String() = %q, want %q", got, "regular")
	}
	if got := KindSymlink.String(); got != "symlink" {
		t.Errorf("KindSymlink.String() = %q, want %q", got, "symlink")
	}
}

func TestClassifySocketIsSpecial(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix socket unavailable: %v", err)
	}
	// Keep the socket file after close so it can be lstat'ed.
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	fi, err := os.Lstat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if got := Classify(fi); got != KindSpecial {
		t.Errorf("Classify(socket) = %v, want %v", got, KindSpecial)
	}
}

func TestClassifyDirEntryWalkerPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("f", filepath.Join(dir, "l")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	haveFIFO := true
	if err := syscall.Mkfifo(filepath.Join(dir, "fifo"), 0o644); err != nil {
		if errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.EPERM) {
			haveFIFO = false
		} else {
			t.Fatalf("mkfifo: %v", err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]EntryKind{}
	for _, de := range entries {
		k, err := ClassifyDirEntry(de)
		if err != nil {
			t.Fatalf("ClassifyDirEntry(%q): %v", de.Name(), err)
		}
		got[de.Name()] = k
	}
	if got["f"] != KindRegular {
		t.Errorf("dir entry f = %v, want %v", got["f"], KindRegular)
	}
	if got["l"] != KindSymlink {
		t.Errorf("dir entry l = %v, want %v", got["l"], KindSymlink)
	}
	if got["d"] != KindDirectory {
		t.Errorf("dir entry d = %v, want %v", got["d"], KindDirectory)
	}
	if haveFIFO && got["fifo"] != KindSpecial {
		t.Errorf("dir entry fifo = %v, want %v", got["fifo"], KindSpecial)
	}
}

func TestClassifyDirEntryInfoFallback(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}

	// Unknown type (ModeIrregular from Type()) + working Info(): the
	// lstat-equivalent fallback classifies correctly (DESIGN_SPEC 3.1).
	k, err := ClassifyDirEntry(fakeDirEntry{fi: fi, typ: fs.ModeIrregular})
	if err != nil {
		t.Fatalf("fallback classify: %v", err)
	}
	if k != KindRegular {
		t.Errorf("fallback classify = %v, want %v", k, KindRegular)
	}

	// Known type: Info() is never consulted, so an Info error is moot.
	k, err = ClassifyDirEntry(fakeDirEntry{fi: fi, typ: 0, infoErr: errors.New("must not be called")})
	if err != nil || k != KindRegular {
		t.Errorf("known-type classify = (%v, %v), want (%v, nil)", k, err, KindRegular)
	}

	// Unknown type + failing Info(): a clean error, no panic.
	if _, err := ClassifyDirEntry(fakeDirEntry{fi: fi, typ: fs.ModeIrregular, infoErr: errors.New("boom")}); err == nil {
		t.Error("expected error when type unknown and Info fails")
	}
}

func TestClassifyTypeTable(t *testing.T) {
	cases := []struct {
		t    fs.FileMode
		want EntryKind
	}{
		{0, KindRegular},
		{fs.ModeDir, KindDirectory},
		{fs.ModeSymlink, KindSymlink},
		{fs.ModeNamedPipe, KindSpecial},
		{fs.ModeSocket, KindSpecial},
		{fs.ModeDevice, KindSpecial},
		{fs.ModeCharDevice, KindSpecial},
		{fs.ModeIrregular, KindSpecial},
	}
	for _, tc := range cases {
		if got := ClassifyType(tc.t); got != tc.want {
			t.Errorf("ClassifyType(%v) = %v, want %v", tc.t, got, tc.want)
		}
	}
}

// --- capture round-trip (DESIGN_SPEC 5.4, files-table columns of 2.3) ----

func TestCaptureRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	content := []byte("hello world")
	if err := os.WriteFile(p, content, 0o640); err != nil {
		t.Fatal(err)
	}

	m1, err := PathMeta(p)
	if err != nil {
		t.Fatalf("PathMeta: %v", err)
	}

	fi, err := os.Stat(p) // regular file: stat == lstat, per task spec
	if err != nil {
		t.Fatal(err)
	}
	m2, err := Capture(fi)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	if m1 != m2 {
		t.Errorf("two captures of the same file differ:\n PathMeta: %+v\n Capture:  %+v", m1, m2)
	}
	// Every field asserted against its known fixture value.
	if m1.Size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", m1.Size, len(content))
	}
	if m1.Mode.Perm() != 0o640 {
		t.Errorf("Mode.Perm() = %04o, want 0640", m1.Mode.Perm())
	}
	if m1.Nlink != 1 {
		t.Errorf("Nlink = %d, want 1", m1.Nlink)
	}
	if m1.UID != uint32(os.Getuid()) {
		t.Errorf("UID = %d, want %d", m1.UID, os.Getuid())
	}
	if m1.Dev == 0 {
		t.Error("Dev = 0, want non-zero")
	}
	if m1.Inode == 0 {
		t.Error("Inode = 0, want non-zero")
	}
	if m1.MtimeNS <= 0 {
		t.Errorf("MtimeNS = %d, want positive", m1.MtimeNS)
	}

	// A symlink capture describes the LINK itself (D-03): size is
	// len(target), never the target's size.
	link := filepath.Join(t.TempDir(), "l")
	if err := os.Symlink("some/target/path", link); err != nil {
		t.Fatal(err)
	}
	lm, err := PathMeta(link)
	if err != nil {
		t.Fatalf("PathMeta(symlink): %v", err)
	}
	if lm.Size != int64(len("some/target/path")) {
		t.Errorf("symlink Size = %d, want %d", lm.Size, len("some/target/path"))
	}
}

func TestCaptureErrorPaths(t *testing.T) {
	// Non-existent path: a clean error, no panic.
	if _, err := PathMeta(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("PathMeta(missing) = nil error, want error")
	}

	// Sys() not *syscall.Stat_t: a clean error, no panic.
	fi, err := os.Lstat(filepath.Join(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Capture(nonStatFileInfo{fi}); err == nil {
		t.Error("Capture(bad Sys) = nil error, want error")
	}
}

func TestUnixMode(t *testing.T) {
	cases := []struct {
		in   fs.FileMode
		want uint32
	}{
		{fs.FileMode(0o644), 0o644},
		{fs.FileMode(0o755) | fs.ModeSetuid, 0o4755},
		{fs.FileMode(0o755) | fs.ModeSetgid, 0o2755},
		{fs.FileMode(0o755) | fs.ModeSticky, 0o1755},
		{fs.ModeDir | fs.FileMode(0o700), 0o700}, // type bits dropped
	}
	for _, tc := range cases {
		if got := UnixMode(tc.in); got != tc.want {
			t.Errorf("UnixMode(%v) = %#o, want %#o", tc.in, got, tc.want)
		}
	}
}

// GoMode is the canonical POSIX->Go inverse (T3.4): named values in
// both directions, type-bit masking on the input, and - the property
// the copy writer's mode fidelity depends on - EXACT round-trip
// identity with UnixMode over the whole permission-class subspace.
// The exhaustive half matters: a single wrong bit anywhere in
// [0, 0o7777] (for example a mis-mapped setuid) would corrupt every
// setuid/setgid/sticky member the copy tree restores, and no
// hand-picked table would necessarily catch it.
func TestGoModeInverseOfUnixMode(t *testing.T) {
	cases := []struct {
		name string
		in   uint32
		want fs.FileMode
	}{
		{"plain", 0o644, fs.FileMode(0o644)},
		{"setuid", 0o4755, fs.FileMode(0o755) | fs.ModeSetuid},
		{"setgid", 0o2755, fs.FileMode(0o755) | fs.ModeSetgid},
		{"sticky", 0o1755, fs.FileMode(0o755) | fs.ModeSticky},
		{"all specials", 0o7777, fs.FileMode(0o777) | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky},
		{"tmp-like sticky dir", 0o1777, fs.FileMode(0o777) | fs.ModeSticky},
		{"zero", 0, 0},
		{"file-type bits ignored", syscall.S_IFREG | 0o4755, fs.FileMode(0o755) | fs.ModeSetuid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := GoMode(tc.in); got != tc.want {
				t.Errorf("GoMode(%#o) = %v (perm %04o), want %v (perm %04o)",
					tc.in, got, got.Perm(), tc.want, tc.want.Perm())
			}
		})
	}

	// Exhaustive inverse identity: UnixMode(GoMode(x)) == x for every
	// raw value the permission-class subspace can hold.
	for x := uint32(0); x <= 0o7777; x++ {
		if got := UnixMode(GoMode(x)); got != x {
			t.Fatalf("UnixMode(GoMode(%#o)) = %#o, want %#o (round-trip must be lossless)", x, got, x)
		}
	}

	// And the other direction over the Go subspace the writer can
	// construct: GoMode(UnixMode(m)) == m for every permission-class
	// fs.FileMode (type bits are UnixMode's documented loss).
	for perm := fs.FileMode(0); perm <= 0o777; perm++ {
		for _, flag := range []fs.FileMode{0, fs.ModeSetuid, fs.ModeSetgid, fs.ModeSticky,
			fs.ModeSetuid | fs.ModeSetgid, fs.ModeSetuid | fs.ModeSticky,
			fs.ModeSetgid | fs.ModeSticky, fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky} {
			m := perm | flag
			if got := GoMode(UnixMode(m)); got != m {
				t.Fatalf("GoMode(UnixMode(%v)) = %v, want %v (round-trip must be lossless)", m, got, m)
			}
		}
	}
}

// --- change detection (DESIGN_SPEC 3.5 / 5.1 step 1, D-10) ---------------

func TestChangedIndependentFlags(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("aaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := PathMeta(p)
	if err != nil {
		t.Fatal(err)
	}

	// Unchanged file: nothing detected (either mode).
	if Changed(base, base, ModeFull) {
		t.Error("Changed(base, base) = true, want false")
	}
	if d := DiffFields(base, base, ModeFull); d != 0 {
		t.Errorf("DiffFields(base, base) = %v, want 0", d)
	}

	// mtime alone (via os.Chtimes to a distinct odd time).
	odd := time.Unix(946_684_800, 123_456_789)
	if err := os.Chtimes(p, odd, odd); err != nil {
		t.Fatal(err)
	}
	afterMtime, err := PathMeta(p)
	if err != nil {
		t.Fatal(err)
	}
	if d := DiffFields(base, afterMtime, ModeFull); d != DiffMtimeNS {
		t.Errorf("mtime-only diff = %v, want %v", d, DiffMtimeNS)
	}
	if !Changed(base, afterMtime, ModeFull) {
		t.Error("Changed detected no mtime change")
	}

	// size alone: append bytes, then restore the ORIGINAL mtime so only
	// the size differs.
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("bb"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	origTime := time.Unix(0, base.MtimeNS)
	if err := os.Chtimes(p, origTime, origTime); err != nil {
		t.Fatal(err)
	}
	afterSize, err := PathMeta(p)
	if err != nil {
		t.Fatal(err)
	}
	if afterSize.MtimeNS != base.MtimeNS {
		t.Fatalf("fixture setup: mtime not restored (%d != %d)", afterSize.MtimeNS, base.MtimeNS)
	}
	if d := DiffFields(base, afterSize, ModeFull); d != DiffSize {
		t.Errorf("size-only diff = %v, want %v", d, DiffSize)
	}

	// inode alone: replace the file (new file + rename over), same size
	// ("aaa") and restored mtime, same directory (same device).
	q := filepath.Join(dir, "g")
	if err := os.WriteFile(q, []byte("aaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(q, origTime, origTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(q, p); err != nil {
		t.Fatal(err)
	}
	afterInode, err := PathMeta(p)
	if err != nil {
		t.Fatal(err)
	}
	if d := DiffFields(base, afterInode, ModeFull); d != DiffInode {
		t.Errorf("inode-only diff = %v, want %v", d, DiffInode)
	}
	// BOTH MODES PIN (2026-10-20 amendment): the same inode-only
	// replacement is INVISIBLE in metadata mode - the CIFS incident's
	// exact false-positive shape (stable size+mtime_ns, synthesized
	// inode). change_test.go pins the full metadata decision table;
	// this asserts it against the REAL capture path.
	if d := DiffFields(base, afterInode, Mode{Set: ComparisonMetadata}); d != 0 {
		t.Errorf("metadata mode: inode-only diff = %v, want 0 (dev/inode are ignored, 3.5)", d)
	}

	// Combined diagnostics render each field name.
	if got := (DiffSize | DiffInode).String(); got != "size inode" {
		t.Errorf("Diff.String() = %q, want %q", got, "size inode")
	}
	if got := Diff(0).String(); got != "none" {
		t.Errorf("Diff(0).String() = %q, want %q", got, "none")
	}
}

// --- symlink targets (D-03: preserve, never follow) ----------------------

func TestReadTarget(t *testing.T) {
	dir := t.TempDir()

	// Valid symlink.
	if err := os.WriteFile(filepath.Join(dir, "target.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	valid := filepath.Join(dir, "valid")
	if err := os.Symlink("target.txt", valid); err != nil {
		t.Fatal(err)
	}
	got, err := ReadTarget(valid)
	if err != nil {
		t.Fatalf("ReadTarget(valid): %v", err)
	}
	if got != "target.txt" {
		t.Errorf("ReadTarget(valid) = %q, want %q", got, "target.txt")
	}

	// Dangling symlink: readlink never touches the target, so the raw
	// target string comes back with a nil error.
	dangling := filepath.Join(dir, "dangling")
	if err := os.Symlink("does/not/exist", dangling); err != nil {
		t.Fatal(err)
	}
	got, err = ReadTarget(dangling)
	if err != nil {
		t.Fatalf("ReadTarget(dangling): %v", err)
	}
	if got != "does/not/exist" {
		t.Errorf("ReadTarget(dangling) = %q, want %q", got, "does/not/exist")
	}

	// Non-symlink: an error.
	if _, err := ReadTarget(filepath.Join(dir, "target.txt")); err == nil {
		t.Error("ReadTarget(regular file) = nil error, want error")
	}
}

// --- metadata application (DESIGN_SPEC 5.4, EPERM downgrade) --------------

func TestApplyMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := PathMeta(p)
	if err != nil {
		t.Fatal(err)
	}
	m.Mode = fs.FileMode(0o640)

	res, err := Apply(p, m)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if res.ChownSkipped || res.Warning != "" {
		t.Errorf("unexpected warning on self-own apply: %+v", res)
	}

	after, err := PathMeta(p)
	if err != nil {
		t.Fatal(err)
	}
	if after.Mode.Perm() != 0o640 {
		t.Errorf("perm after Apply = %04o, want 0640", after.Mode.Perm())
	}
	if after.UID != m.UID || after.GID != m.GID {
		t.Errorf("ownership changed unexpectedly: %+v", after)
	}
}

func TestApplyMtimeNanosecondPrecision(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	odd := time.Unix(1_600_000_123, 987_654_321)
	if err := os.Chtimes(src, odd, odd); err != nil {
		t.Fatal(err)
	}
	m, err := PathMeta(src)
	if err != nil {
		t.Fatal(err)
	}
	if m.MtimeNS != odd.UnixNano() {
		t.Fatalf("fixture setup: mtime_ns = %d, want %d", m.MtimeNS, odd.UnixNano())
	}

	dst := filepath.Join(t.TempDir(), "dst")
	if err := os.WriteFile(dst, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Apply(dst, m); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	after, err := PathMeta(dst)
	if err != nil {
		t.Fatal(err)
	}
	if after.MtimeNS != odd.UnixNano() {
		t.Errorf("mtime_ns after Apply = %d, want %d (nanosecond precision)", after.MtimeNS, odd.UnixNano())
	}
}

// TestApplyPreservesAtime is the regression test for review finding B1:
// the atime slot of the utimensat call must carry UTIME_OMIT
// ((1<<30)-2 = 0x3FFFFFFE). The old wrong constant 0x380 (896) is a
// LEGAL tv_nsec, so the kernel never saw UTIME_OMIT — it treated the
// slot as a real timestamp and every Apply silently reset atime to
// Unix-epoch+896ns. This test seeds a DISTINCT known atime, runs Apply
// (which must OMIT atime), and asserts both that mtime moved to the
// Apply value AND that atime is EXACTLY the seeded value at nanosecond
// precision via syscall.Stat_t.Atim.
func TestApplyPreservesAtime(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "dst")
	if err := os.WriteFile(dst, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Distinct seeded atime and mtime (os.Chtimes sets both slots).
	seededAtime := time.Unix(1_234_567_890, 111_222_333)
	seededMtime := time.Unix(946_684_800, 444_555_666)
	if err := os.Chtimes(dst, seededAtime, seededMtime); err != nil {
		t.Fatal(err)
	}

	// Fixture sanity: the seed landed, checked at nanosecond precision.
	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("Sys() not *syscall.Stat_t: %T", fi.Sys())
	}
	if got := statAtimeNS(st); got != seededAtime.UnixNano() {
		t.Fatalf("fixture setup: seeded atime = %d, want %d", got, seededAtime.UnixNano())
	}

	// The Meta carries a NEW mtime; atime is not part of Meta (FR-036
	// records mode, owner, group, and mtime only), so Apply must OMIT
	// the atime slot.
	m, err := PathMeta(dst)
	if err != nil {
		t.Fatal(err)
	}
	applyMtime := time.Unix(1_600_000_123, 987_654_321)
	m.MtimeNS = applyMtime.UnixNano()

	if _, err := Apply(dst, m); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	fi, err = os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	st, ok = fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("Sys() not *syscall.Stat_t: %T", fi.Sys())
	}
	// The regression assertion: atime is EXACTLY the seeded value —
	// not Unix-epoch+896ns (the 0x380 corruption), not "close enough".
	if got := statAtimeNS(st); got != seededAtime.UnixNano() {
		t.Errorf("atime after Apply = %d, want %d (UTIME_OMIT must preserve atime exactly)",
			got, seededAtime.UnixNano())
	}
	// And the mtime DID move to the Apply value (the omit must not
	// have disabled the mtime write).
	if got := statMtimeNS(st); got != applyMtime.UnixNano() {
		t.Errorf("mtime after Apply = %d, want %d", got, applyMtime.UnixNano())
	}
}

// statAtimeNS/statMtimeNS read nanosecond-precision timestamps from a
// kernel stat result.
func statAtimeNS(st *syscall.Stat_t) int64 { return st.Atim.Sec*1e9 + st.Atim.Nsec }
func statMtimeNS(st *syscall.Stat_t) int64 { return st.Mtim.Sec*1e9 + st.Mtim.Nsec }

// TestApplyHardErrorAfterEPERMDowngradeCarriesWarning closes the
// reviewer NB on the documented contract at apply.go lines ~81-82: "When
// a hard error occurs after an EPERM downgrade, the returned ApplyResult
// still carries the warning". The scenario is honest and end-to-end:
// /proc/self/comm exists for the lstat, the unconditional lchown to a
// foreign uid is denied EPERM (the downgrade fires and populates res),
// and the FOLLOWING chmod is denied EPERM as well (non-owner, no
// CAP_FOWNER — procfs never allows chmod), which is a HARD error.
// Apply must return both the hard error AND the populated result.
// Nothing is mutated: both syscalls are denied by the kernel.
func TestApplyHardErrorAfterEPERMDowngradeCarriesWarning(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root (euid 0): lchown to a foreign uid succeeds, the EPERM downgrade cannot be provoked")
	}
	const path = "/proc/self/comm"
	if _, err := os.Lstat(path); err != nil {
		t.Skipf("%s not lstat-able (no procfs?): %v", path, err)
	}

	m := Meta{
		Mode: fs.FileMode(0o644),
		UID:  65534, // nobody: foreign owner; chown EPERM as non-root
		GID:  65534,
	}
	res, err := Apply(path, m)
	if err == nil {
		t.Fatal("Apply(/proc/self/comm, foreign uid) = nil error; chmod on procfs must fail EPERM as non-root")
	}
	if !errors.Is(err, syscall.EPERM) {
		t.Errorf("Apply error = %v, want the chmod EPERM as the hard error", err)
	}
	// The documented contract: the hard error does NOT discard the
	// downgrade warning already collected in res (callers check err
	// first, then surface res.Warning).
	if !res.ChownSkipped {
		t.Error("ChownSkipped = false on the returned result, want true (EPERM downgrade fired before the hard error)")
	}
	if res.Warning == "" {
		t.Error("Warning empty on the returned result, want populated despite the subsequent hard error")
	}
}

func TestApplySelfChownNoWarning(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The file's OWN uid/gid: self-chown is allowed for the owner, root or
	// not, so this must run without any warning on every machine.
	m, err := PathMeta(p)
	if err != nil {
		t.Fatal(err)
	}
	m.Mode = fs.FileMode(0o600)
	m.MtimeNS = time.Unix(1_234_567_890, 1).UnixNano()

	res, err := Apply(p, m)
	if err != nil {
		t.Fatalf("Apply(self uid/gid): %v", err)
	}
	if res.ChownSkipped {
		t.Errorf("ChownSkipped = true for self-chown; warning: %q", res.Warning)
	}
	if res.Warning != "" {
		t.Errorf("Warning = %q, want empty", res.Warning)
	}
	after, err := PathMeta(p)
	if err != nil {
		t.Fatal(err)
	}
	if after.UID != m.UID || after.GID != m.GID {
		t.Errorf("self-chown changed ownership: %+v", after)
	}
	if after.Mode.Perm() != 0o600 || after.MtimeNS != m.MtimeNS {
		t.Errorf("metadata not fully applied: %+v", after)
	}
}

func TestApplyChownEPERMDowngrade(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root (euid 0): chown to another uid succeeds, EPERM cannot be provoked; the downgrade path only fires for non-root runs")
	}
	const nobodyUID, nobodyGID = 65534, 65534 // nobody/nogroup on Linux

	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := PathMeta(p)
	if err != nil {
		t.Fatal(err)
	}

	m := before
	m.UID = nobodyUID
	m.GID = nobodyGID
	m.Mode = fs.FileMode(0o640)
	odd := time.Unix(1_111_111_111, 222_222_223)
	m.MtimeNS = odd.UnixNano()

	// The EPERM rule (DESIGN_SPEC 5.4): chown attempted unconditionally;
	// EPERM as non-root is a WARNING, never a failure. err must be nil.
	res, err := Apply(p, m)
	if err != nil {
		t.Fatalf("Apply with foreign uid/gid as non-root: err = %v, want nil (EPERM downgrade)", err)
	}
	if !res.ChownSkipped {
		t.Error("ChownSkipped = false, want true (EPERM downgrade fired)")
	}
	if res.Warning == "" {
		t.Error("Warning empty, want populated for the EPERM downgrade")
	}

	// The file still completed: other metadata applied, ownership
	// untouched (chown did not happen).
	after, err := PathMeta(p)
	if err != nil {
		t.Fatal(err)
	}
	if after.UID != before.UID || after.GID != before.GID {
		t.Errorf("ownership changed despite EPERM: %+v", after)
	}
	if after.Mode.Perm() != 0o640 {
		t.Errorf("perm not applied after EPERM downgrade: %04o, want 0640", after.Mode.Perm())
	}
	if after.MtimeNS != odd.UnixNano() {
		t.Errorf("mtime not applied after EPERM downgrade: %d, want %d", after.MtimeNS, odd.UnixNano())
	}
}

func TestApplyMissingPathIsError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := Apply(missing, Meta{}); err == nil {
		t.Fatal("Apply(missing path) = nil error, want error")
	} else if !errors.Is(err, syscall.ENOENT) {
		t.Errorf("Apply(missing path) error = %v, want ENOENT", err)
	}
}

func TestApplySymlinkNeverFollows(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	targetTime := time.Unix(946_684_800, 0)
	if err := os.Chtimes(target, targetTime, targetTime); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink("target.txt", link); err != nil {
		t.Fatal(err)
	}

	m, err := PathMeta(link) // lstat: describes the link itself
	if err != nil {
		t.Fatal(err)
	}
	desired := time.Unix(1_500_000_000, 456_789)
	m.MtimeNS = desired.UnixNano()

	res, err := Apply(link, m)
	if err != nil {
		t.Fatalf("Apply(symlink): %v", err)
	}
	if res.ChownSkipped {
		t.Errorf("unexpected chown downgrade on own symlink: %q", res.Warning)
	}

	after, err := PathMeta(link)
	if err != nil {
		t.Fatal(err)
	}
	if after.MtimeNS != desired.UnixNano() {
		t.Errorf("symlink mtime_ns = %d, want %d", after.MtimeNS, desired.UnixNano())
	}
	// The TARGET must be untouched: chmod/chtimes followed the link would
	// have mutated it (D-03 violation). The comparison is a precise
	// equality against the fixture timestamp, not a "changed" check.
	targetAfter, err := PathMeta(target)
	if err != nil {
		t.Fatal(err)
	}
	if targetAfter.MtimeNS != targetTime.UnixNano() {
		t.Errorf("target mtime mutated through symlink apply: %d, want %d", targetAfter.MtimeNS, targetTime.UnixNano())
	}
	targetContent, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(targetContent) != "x" {
		t.Errorf("target content mutated through symlink apply: %q, want %q", targetContent, "x")
	}

	// The LINK OBJECT itself must still be the same symlink with the same
	// target (metadata application never replaces the entry, D-03).
	if tgt, err := ReadTarget(link); err != nil || tgt != "target.txt" {
		t.Errorf("ReadTarget(link) after Apply = (%q, %v), want (\"target.txt\", nil)", tgt, err)
	}
	fiLink, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fiLink.Mode()&fs.ModeSymlink == 0 {
		t.Error("entry stopped being a symlink after Apply")
	}
}

// === Gap-closing tests (T1.2 verification round 2) ========================
//
// These tests close fidelity and contract gaps identified in the T1.2
// audit: setuid/setgid/sticky bit round-trip through Capture and
// UnixMode, the lchown-before-chmod ordering that preserves those bits,
// hard-link nlink capture (the T1.4 link_group seeding field), the
// exhaustive Diff bitmask rendering used in diagnostics, the
// same-length-symlink-retarget assessment, the exact os sentinel value
// for unknown dirent types, and the raw-syscall utimensat error path.

// TestCaptureSpecialBitsRoundTrip verifies that the setuid, setgid, and
// sticky bits survive Capture (fs.FileMode relocation) AND convert back
// to exact POSIX bits via UnixMode. DESIGN_SPEC 2.3 stores `mode` as
// "permission bits"; a lost setuid bit would silently strip restore
// fidelity for executables (FR-036).
func TestCaptureSpecialBitsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name   string
		isDir  bool
		chmod  uint32 // POSIX bits applied via chmod
		wantGo fs.FileMode
	}{
		{"setuid file", false, 0o4755, 0o755 | fs.ModeSetuid},
		{"setgid file", false, 0o2755, 0o755 | fs.ModeSetgid},
		{"sticky file", false, 0o1755, 0o755 | fs.ModeSticky},
		{"all specials file", false, 0o7777, 0o777 | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky},
		{"sticky dir like tmp", true, 0o1777, 0o777 | fs.ModeSticky},
		{"setgid dir", true, 0o2775, 0o775 | fs.ModeSetgid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, tc.name)
			if tc.isDir {
				if err := os.Mkdir(p, 0o755); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			// syscall.Chmod applies the raw POSIX bits verbatim; os.Chmod
			// would translate Go FileMode flags instead and silently drop
			// the setuid bit from a value like 0o4755.
			if err := syscall.Chmod(p, tc.chmod); err != nil {
				t.Fatal(err)
			}

			m, err := PathMeta(p)
			if err != nil {
				t.Fatalf("PathMeta: %v", err)
			}
			if m.Mode != tc.wantGo {
				t.Errorf("captured Mode = %v (perm %04o), want %v (perm %04o)",
					m.Mode, m.Mode.Perm(), tc.wantGo, tc.wantGo.Perm())
			}
			// The POSIX round-trip: the exact chmod'ed bits come back.
			if got := UnixMode(m.Mode); got != tc.chmod {
				t.Errorf("UnixMode(Capture) = %#o, want %#o (setuid/setgid/sticky fidelity)", got, tc.chmod)
			}
			// Cross-check against the kernel's own raw stat bits so a
			// Capture bug cannot hide behind an equally wrong expectation.
			fi, err := os.Lstat(p)
			if err != nil {
				t.Fatal(err)
			}
			st, ok := fi.Sys().(*syscall.Stat_t)
			if !ok {
				t.Fatalf("Sys() not *syscall.Stat_t: %T", fi.Sys())
			}
			if raw := st.Mode & 0o7777; raw != tc.chmod {
				t.Fatalf("fixture: kernel mode %#o != requested %#o", raw, tc.chmod)
			}
		})
	}
}

// TestApplyOrderingSetuidPreserved pins the ordering contract documented
// in apply.go: lchown FIRST, then chmod. On Linux a chown/chgrp that
// actually changes ownership clears setuid/setgid for non-capable
// callers, so chmod must run AFTER the chown to guarantee the recorded
// mode lands. If a refactor ever flips the order, the "successful chgrp"
// subtest below fails (the kernel would strip the bits after chmod ran).
func TestApplyOrderingSetuidPreserved(t *testing.T) {
	t.Run("eperm downgrade still applies full special mode", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root (euid 0): chown to another uid always succeeds, EPERM cannot be provoked; the downgrade path only fires for non-root runs")
		}
		p := filepath.Join(t.TempDir(), "f")
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Chmod(p, 0o4755); err != nil {
			t.Fatal(err)
		}
		m, err := PathMeta(p)
		if err != nil {
			t.Fatal(err)
		}
		m.UID, m.GID = 65534, 65534 // nobody/nogroup: foreign owner, EPERM as non-root

		res, err := Apply(p, m)
		if err != nil {
			t.Fatalf("Apply: %v (EPERM must downgrade to warning)", err)
		}
		if !res.ChownSkipped {
			t.Error("ChownSkipped = false, want true (foreign uid as non-root)")
		}
		after, err := PathMeta(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := UnixMode(after.Mode); got != 0o4755 {
			t.Errorf("mode after EPERM downgrade = %#o, want %#o (setuid must survive the failed chown)", got, 0o4755)
		}
		if after.Mode&fs.ModeSetuid == 0 {
			t.Error("fs.ModeSetuid bit lost after EPERM downgrade")
		}
	})

	t.Run("self chown preserves setuid", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "f")
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Chmod(p, 0o4755); err != nil {
			t.Fatal(err)
		}
		m, err := PathMeta(p)
		if err != nil {
			t.Fatal(err)
		}
		res, err := Apply(p, m) // own uid/gid: self-chown succeeds, no bits cleared
		if err != nil {
			t.Fatalf("Apply(self uid/gid): %v", err)
		}
		if res.ChownSkipped {
			t.Errorf("ChownSkipped = true on self-chown: %q", res.Warning)
		}
		after, err := PathMeta(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := UnixMode(after.Mode); got != 0o4755 {
			t.Errorf("mode after self-chown Apply = %#o, want %#o", got, 0o4755)
		}
		if after.UID != m.UID || after.GID != m.GID {
			t.Errorf("ownership differs after Apply: %+v", after)
		}
	})

	// The ordering PROOF: a chgrp that genuinely succeeds (to a group the
	// caller belongs to) makes the kernel clear setuid AND setgid
	// mid-Apply; the chmod that runs afterwards restores them. With
	// chmod-before-chown the final mode would be 0755 and this fails.
	t.Run("successful chgrp restores kernel-cleared special bits", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root (euid 0): root chowns are CAP_FSETID-capable and never clear setuid/setgid, so the ordering cannot be exercised; non-root runs only")
		}
		groups, err := syscall.Getgroups()
		if err != nil {
			t.Fatalf("Getgroups: %v", err)
		}
		primary := os.Getgid()
		supp := -1
		for _, g := range groups {
			if g != primary {
				supp = g
				break
			}
		}
		if supp < 0 {
			t.Skip("no supplementary group distinct from the primary gid: cannot perform a successful non-root group change to prove the ordering")
		}

		p := filepath.Join(t.TempDir(), "f")
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Chmod(p, 0o6755); err != nil {
			t.Fatal(err)
		}
		m, err := PathMeta(p)
		if err != nil {
			t.Fatal(err)
		}
		m.GID = uint32(supp) // own supplementary group: chgrp succeeds without privileges

		res, err := Apply(p, m)
		if err != nil {
			t.Fatalf("Apply with own supplementary gid: %v", err)
		}
		if res.ChownSkipped {
			t.Fatalf("ChownSkipped = true for an own supplementary group: %q", res.Warning)
		}
		after, err := PathMeta(p)
		if err != nil {
			t.Fatal(err)
		}
		if after.GID != uint32(supp) {
			t.Errorf("gid = %d, want %d (chgrp should have succeeded)", after.GID, supp)
		}
		if got := UnixMode(after.Mode); got != 0o6755 {
			t.Errorf("mode after successful chgrp = %#o, want %#o: the kernel cleared the special bits and chmod-after-chown must restore them (ordering bug if this is %#o)",
				got, 0o6755, 0o755)
		}
		if after.Mode&(fs.ModeSetuid|fs.ModeSetgid) != (fs.ModeSetuid | fs.ModeSetgid) {
			t.Errorf("special bits lost: Mode = %v, want both setuid and setgid set", after.Mode)
		}
	})
}

// TestHardLinkNlinkCapture asserts that a hard-linked file's Meta shows
// nlink=2 on BOTH names with identical (dev, inode). These are exactly
// the fields DESIGN_SPEC 2.4 keys hard-link families on ("ingest keys on
// (dev, inode) with nlink > 1") and that T1.4/T1.6 will use to seed
// `link_group`; a Capture bug here would silently duplicate family
// members on media (FR-039).
func TestHardLinkNlinkCapture(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	if err := os.WriteFile(a, []byte("shared content"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := filepath.Join(dir, "b")
	if err := os.Link(a, b); err != nil {
		t.Fatalf("os.Link: %v", err)
	}

	ma, err := PathMeta(a)
	if err != nil {
		t.Fatal(err)
	}
	mb, err := PathMeta(b)
	if err != nil {
		t.Fatal(err)
	}
	if ma.Nlink != 2 || mb.Nlink != 2 {
		t.Errorf("Nlink = %d/%d, want 2/2 on both names of a hard-link pair", ma.Nlink, mb.Nlink)
	}
	if ma.Dev != mb.Dev || ma.Inode != mb.Inode {
		t.Errorf("hard-linked names disagree on identity: a=(dev %d, ino %d), b=(dev %d, ino %d)",
			ma.Dev, ma.Inode, mb.Dev, mb.Inode)
	}
	if ma.Size != mb.Size || ma.Size == 0 {
		t.Errorf("sizes of hard-linked names: %d/%d, want equal non-zero", ma.Size, mb.Size)
	}
	// Dropping one name must be observed dynamically: re-capture after
	// unlink shows nlink back to 1 (the family-dissolution signal).
	if err := os.Remove(b); err != nil {
		t.Fatal(err)
	}
	ma2, err := PathMeta(a)
	if err != nil {
		t.Fatal(err)
	}
	if ma2.Nlink != 1 {
		t.Errorf("Nlink after unlinking the second name = %d, want 1", ma2.Nlink)
	}
}

// TestDiffFieldsIgnoresModeOwnerNlinkByDesign pins the contract that the
// change-detection set is EXACTLY {size, mtime_ns, dev, inode}
// (DESIGN_SPEC 3.5, 5.1 step 1). Mode, uid, gid, and nlink differences
// deliberately do NOT invalidate a captured hash; if a refactor ever
// widens the set, this test fails and forces a design amendment instead
// of a silent semantic change at the ingest/copy/build detection points.
func TestDiffFieldsIgnoresModeOwnerNlinkByDesign(t *testing.T) {
	a := Meta{Mode: 0o644, UID: 1000, GID: 1000, Nlink: 1,
		Size: 42, MtimeNS: 1_000_000_000, Dev: 7, Inode: 99}
	b := Meta{Mode: 0o600 | fs.ModeSetuid, UID: 0, GID: 0, Nlink: 3,
		Size: 42, MtimeNS: 1_000_000_000, Dev: 7, Inode: 99}
	if d := DiffFields(a, b, ModeFull); d != 0 {
		t.Errorf("DiffFields differing only in mode/uid/gid/nlink = %v, want 0 (fixed field set of 3.5)", d)
	}
	if Changed(a, b, ModeFull) {
		t.Error("Changed = true for mode/uid/gid/nlink-only differences, want false")
	}
	// The metadata mode is a strict SUBSET of the full set, so it also
	// ignores mode/uid/gid/nlink (and everything else the full set
	// ignores).
	if d := DiffFields(a, b, Mode{Set: ComparisonMetadata}); d != 0 {
		t.Errorf("metadata-mode DiffFields differing only in mode/uid/gid/nlink/dev/inode = %v, want 0", d)
	}
}

// TestDiffStringExhaustive locks the exact rendered field names and
// order. Diff.String feeds D-10 change warnings and per-file diagnostics,
// so its output is a de-facto contract with the report layer.
func TestDiffStringExhaustive(t *testing.T) {
	cases := []struct {
		d    Diff
		want string
	}{
		{0, "none"},
		{DiffSize, "size"},
		{DiffMtimeNS, "mtime_ns"},
		{DiffDev, "dev"},
		{DiffInode, "inode"},
		{DiffSize | DiffMtimeNS, "size mtime_ns"},
		{DiffDev | DiffInode, "dev inode"},
		{DiffSize | DiffDev, "size dev"},
		{DiffSize | DiffMtimeNS | DiffDev | DiffInode, "size mtime_ns dev inode"},
	}
	for _, tc := range cases {
		if got := tc.d.String(); got != tc.want {
			t.Errorf("Diff(%#x).String() = %q, want %q", uint8(tc.d), got, tc.want)
		}
	}
}

// TestDiffFieldsCombined covers a multi-field diff through the REAL
// capture path (size + mtime change together), complementing the
// single-field independence cases of TestChangedIndependentFlags.
func TestDiffFieldsCombined(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte("aaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	base, err := PathMeta(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("bbbb"), 0o644); err != nil { // rewrite: size and mtime both move
		t.Fatal(err)
	}
	// EXPLICIT mtime move (>= 2s, absolute): the rewrite alone only
	// moves mtime if the filesystem's timestamp granularity lets a
	// same-instant rewrite differ from the base stamp - CI filesystems
	// may round mtimes to 1s, so the mtime half of the expected diff
	// is guaranteed by an absolute Chtimes instead of by the write.
	// +10s for margin; the direction is irrelevant (PathMeta reads
	// whatever st_mtime the kernel reports).
	if err := os.Chtimes(p, time.Unix(0, base.MtimeNS).Add(10*time.Second),
		time.Unix(0, base.MtimeNS).Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	after, err := PathMeta(p)
	if err != nil {
		t.Fatal(err)
	}
	if d := DiffFields(base, after, ModeFull); d != (DiffSize | DiffMtimeNS) {
		t.Errorf("size+mtime diff = %v, want %v", d, DiffSize|DiffMtimeNS)
	}
	// Both modes agree on the size+mtime_ns fields (the metadata set is
	// a subset that drops only dev/inode).
	if d := DiffFields(base, after, Mode{Set: ComparisonMetadata}); d != (DiffSize | DiffMtimeNS) {
		t.Errorf("metadata-mode size+mtime diff = %v, want %v", d, DiffSize|DiffMtimeNS)
	}
}

// TestSymlinkRetargetChangeDetection documents and asserts what the
// fixed 3.5 field set does on a symlink retarget to a SAME-LENGTH target
// (size == len(target) does not move).
//
// Assessment: on Linux a symlink's target lives IN its inode and the
// kernel offers no in-place retarget syscall. Every real retarget
// replaces the directory entry (unlink+symlink or rename), which yields
// a NEW inode at the path, so DiffInode (and DiffMtimeNS) fire and the
// rescan/change-detection points of 3.5 cannot miss it. A same-inode
// retarget is not producible through POSIX APIs, so adding link_target
// to the diff set would not close a real gap (it would change the
// design's fixed field set for no observable benefit).
func TestSymlinkRetargetChangeDetection(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "l")
	if err := os.Symlink("target-a", link); err != nil { // 8 bytes
		t.Fatal(err)
	}
	base, err := PathMeta(link)
	if err != nil {
		t.Fatal(err)
	}

	// Case 1: atomic retarget via rename-over of a same-length target.
	tmp := filepath.Join(dir, "tmp")
	if err := os.Symlink("target-b", tmp); err != nil { // same length: size field cannot move
		t.Fatal(err)
	}
	if err := os.Rename(tmp, link); err != nil {
		t.Fatal(err)
	}
	after, err := PathMeta(link)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size != base.Size {
		t.Fatalf("fixture broken: size moved (%d -> %d); test would not exercise the same-length case", base.Size, after.Size)
	}
	if tgt, err := ReadTarget(link); err != nil || tgt != "target-b" {
		t.Fatalf("fixture broken: retarget did not happen (%q, %v)", tgt, err)
	}
	d := DiffFields(base, after, ModeFull)
	if d&DiffInode == 0 {
		t.Errorf("rename-over retarget not flagged as changed via inode: diff = %v (rename swaps the dirent's inode; this cannot be missed)", d)
	}
	if !d.Any() {
		t.Error("rename-over retarget reported unchanged")
	}

	// Case 2: unlink+symlink retarget (the non-atomic way). The new
	// symlink usually gets a different inode number; even when the
	// allocator reuses the freed number, the creation mtime moves. The
	// assertion is "some fixed field flagged it"; a skip documents the
	// only theoretical blind spot: a filesystem with timestamp
	// granularity so coarse that size, mtime_ns, dev, AND the reused
	// inode all coincide.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target-c", link); err != nil {
		t.Fatal(err)
	}
	after2, err := PathMeta(link)
	if err != nil {
		t.Fatal(err)
	}
	d2 := DiffFields(after, after2, ModeFull) // compare against the post-rename state
	if !d2.Any() {
		t.Skipf("unlink+symlink retarget of a same-length target produced identical (size, mtime_ns, dev, inode): only possible if the fs reused the inode AND its mtime granularity collapsed the timestamp; current design (3.5 fixed field set) cannot distinguish this from an unchanged link")
	}
}

// TestClassifyDirEntryUnknownSentinel models the EXACT sentinel the Go
// stdlib produces for an unknown d_type on Linux: os.direntType returns
// ^fs.FileMode(0) (all bits set) for DT_UNKNOWN, and only the io/fs
// CONTRACT generalizes that to fs.ModeIrregular. ClassifyDirEntry must
// fall back to Info() for BOTH representations. (Note: go's own
// newUnixDirent eagerly lstats unknown types before handing the entry
// to callers, so through os.ReadDir/filepath.WalkDir the fallback is
// defensive; it becomes load-bearing for any other fs.DirEntry
// implementation and is mandated by DESIGN_SPEC 3.1 "Info() fallback
// only when type is unknown".)
func TestClassifyDirEntryUnknownSentinel(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sock")
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Skipf("unix socket unavailable: %v", err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}

	// The os-internal DT_UNKNOWN sentinel: every bit set.
	k, err := ClassifyDirEntry(fakeDirEntry{fi: fi, typ: ^fs.FileMode(0)})
	if err != nil {
		t.Fatalf("fallback classify with ^FileMode(0) sentinel: %v", err)
	}
	if k != KindSpecial {
		t.Errorf("sentinel fallback classify = %v, want %v (socket via Info lstat)", k, KindSpecial)
	}
	// Unknown-type entry whose Info() fails: the vanished-entry path
	// (readdir saw it, lstat no longer can), a clean error not a panic.
	if _, err := ClassifyDirEntry(fakeDirEntry{fi: fi, typ: ^fs.FileMode(0), infoErr: os.ErrNotExist}); err == nil {
		t.Error("expected error when sentinel type and Info fails")
	}
}

// TestUtinesNoFollowMissingPath exercises the raw utimensat syscall's
// error path directly (ENOENT on a vanished path) so the hand-rolled
// syscall.Syscall6 wrapper is covered for failure, not just success via
// Apply. Guards against a wrapper that swallows errno (for example by
// misreading the kernel's return convention).
func TestUtinesNoFollowMissingPath(t *testing.T) {
	err := utimesNoFollow(filepath.Join(t.TempDir(), "gone"), 0)
	if err == nil {
		t.Fatal("utimesNoFollow(missing path) = nil, want ENOENT")
	}
	if !errors.Is(err, syscall.ENOENT) {
		t.Errorf("utimesNoFollow(missing path) = %v, want ENOENT", err)
	}
}
