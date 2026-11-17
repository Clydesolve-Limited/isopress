package cli

// T3.4: specials fidelity e2e through the REAL copy command. The
// fixture concentrates every T3.3 special into one set: a 04755
// setuid file (the POSIX->Go MODE CONVERSION proof end-to-end - the
// raw catalog bits must survive scan -> inventory -> metaFromRaw ->
// os.Chmod and land on the destination verbatim), a 1777 sticky empty
// dir, a 0750 empty dir, valid AND dangling symlinks (D-03: restored
// as links with recorded targets, the dangle carried faithfully), and
// a 3-name hard-link family (D-04: primary written once, both
// secondaries link(2), nlink == 3 on the tree). Scan and copy run the
// real re-exec child; the plan runs the REAL pipeline with only the
// capacity profile substituted (the copy_test.go T3.3 pattern).
//
// The acceptance is copy-tree fidelity, stat-for-stat (restore is
// T5): destination raw modes, dir mtimes at nanosecond precision,
// link targets, family inode identity + nlink, then a resume re-run
// that copies nothing but the idempotent dir re-asserts.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"isopress/internal/media"
)

// specialsRawMode lstats path and returns the kernel's raw POSIX
// permission bits (rwx + setuid/setgid/sticky) - the ground truth for
// the mode-conversion asserts.
func specialsRawMode(t *testing.T, path string) uint32 {
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

// specialsNlink returns the hard-link count of path.
func specialsNlink(t *testing.T, path string) uint64 {
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

func TestCopySpecialsFidelityEndToEnd(t *testing.T) {
	root := t.TempDir()
	cat := filepath.Join(t.TempDir(), "catalog.sqlite")
	dest := filepath.Join(t.TempDir(), "dest")
	setID := "S-SPX"

	// --- the fixture ---------------------------------------------------------
	// syscall.Chmod applies raw POSIX bits verbatim; os.Chmod would
	// translate Go FileMode flags and silently drop setuid from 0o4755
	// (the T1.2 QA trap, exactly what this e2e proves fixed on the
	// WRITE side too).
	setuidPayload := pattern(11, 256)
	setuidPath := filepath.Join(root, "setuid.bin")
	writeFileP(t, setuidPath, setuidPayload, 0o644)
	if err := syscall.Chmod(setuidPath, 0o4755); err != nil {
		t.Fatalf("fixture setuid chmod: %v", err)
	}

	famPayload := pattern(12, 128)
	famA := filepath.Join(root, "fam-a.bin")
	writeFileP(t, famA, famPayload, 0o644)
	for _, name := range []string{"fam-b.bin", "fam-c.bin"} {
		if err := os.Link(famA, filepath.Join(root, name)); err != nil {
			t.Fatalf("building the 3-name hard-link family (%s): %v", name, err)
		}
	}

	if err := os.Symlink("setuid.bin", filepath.Join(root, "link-valid")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("does/not/exist.bin", filepath.Join(root, "link-dangling")); err != nil {
		t.Fatal(err)
	}

	stickyDir := filepath.Join(root, "sticky-dir")
	if err := os.Mkdir(stickyDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Chmod(stickyDir, 0o1777); err != nil {
		t.Fatalf("fixture sticky chmod: %v", err)
	}
	empty750 := filepath.Join(root, "empty-750")
	if err := os.Mkdir(empty750, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Chmod(empty750, 0o750); err != nil {
		t.Fatalf("fixture 0750 chmod: %v", err)
	}
	srcStickyMtime := fileMtime(t, stickyDir)
	src750Mtime := fileMtime(t, empty750)

	// --- scan -> REAL plan pipeline -> copy ----------------------------------
	stdout, stderr, code := runScanChildForCopy(t, "--catalog", cat, "--set-id", setID, root)
	if code != 0 {
		t.Fatalf("scan exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	_ = stdout
	// A large profile: every member lands on image 1, so the asserts
	// below are single-image and the dir skeleton goes to the lowest
	// image (4.3) - image 1.
	planSynthetic(t, cat, setID, 10<<20)

	stdout, stderr, code = runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json")
	if code != 0 {
		t.Fatalf("copy exit = %d, want 0 (stderr: %s)\nstdout: %s", code, stderr, stdout)
	}
	var rep copyReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("copy --json output does not parse: %v\n%s", err, stdout)
	}
	if rep.Status != "ok" || rep.Failed != 0 || rep.Skipped != 0 {
		t.Fatalf("report = %+v, want a clean ok run", rep)
	}
	if len(rep.Images) != 1 {
		t.Fatalf("images in report = %d, want 1 (10 MiB profile holds the whole fixture)", len(rep.Images))
	}
	// 2 whole files + 2 secondaries + 2 symlinks + 2 dirs.
	if rep.Copied != 8 || rep.Dirs != 2 {
		t.Fatalf("report copied=%d dirs=%d, want 8 copied (4 files/links + 2 symlinks + 2 dirs) and 2 dirs",
			rep.Copied, rep.Dirs)
	}
	if rep.MetadataWarnings != 0 {
		t.Fatalf("metadata_warnings = %d, want 0 (the fixture is self-owned; no EPERM class fires)", rep.MetadataWarnings)
	}

	// --- destination tree, stat-for-stat -------------------------------------
	img1 := destImageDir(dest, setID, 1)

	// THE CONVERSION PROOF: the setuid file carries the recorded raw
	// POSIX bits 04755 on the destination - setuid included.
	if got := specialsRawMode(t, filepath.Join(img1, "files", "setuid.bin")); got != 0o4755 {
		t.Fatalf("setuid.bin dest raw mode = %#o, want 04755 (POSIX->Go conversion must survive end-to-end)", got)
	}
	assertBytes(t, filepath.Join(img1, "files", "setuid.bin"), setuidPayload)

	// The sticky dir and the 0750 dir: raw modes AND nanosecond mtimes
	// reproduced on the copy tree (FR-037/FR-036, stat-based).
	for _, tc := range []struct {
		rel      string
		mode     uint32
		srcMtime time.Time
	}{
		{"sticky-dir", 0o1777, srcStickyMtime},
		{"empty-750", 0o750, src750Mtime},
	} {
		destPath := filepath.Join(img1, "files", tc.rel)
		fi, err := os.Lstat(destPath)
		if err != nil {
			t.Fatalf("lstat %s: %v", tc.rel, err)
		}
		if !fi.IsDir() {
			t.Fatalf("%s is %v, want a directory", tc.rel, fi.Mode())
		}
		if got := specialsRawMode(t, destPath); got != tc.mode {
			t.Fatalf("%s dest raw mode = %#o, want the recorded %#o", tc.rel, got, tc.mode)
		}
		if !fi.ModTime().Equal(tc.srcMtime) {
			t.Fatalf("%s dest mtime = %v, want the source mtime %v (ns precision)", tc.rel, fi.ModTime(), tc.srcMtime)
		}
	}

	// The hard-link family: one inode, three names, nlink == 3 on the
	// tree, the same bytes readable under every name.
	famADest := filepath.Join(img1, "files", "fam-a.bin")
	primaryInfo, err := os.Lstat(famADest)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"fam-b.bin", "fam-c.bin"} {
		info, err := os.Lstat(filepath.Join(img1, "files", name))
		if err != nil {
			t.Fatalf("lstat %s: %v", name, err)
		}
		if !os.SameFile(primaryInfo, info) {
			t.Fatalf("%s does not share the primary's inode on the destination (D-04 link(2))", name)
		}
	}
	if got := specialsNlink(t, famADest); got != 3 {
		t.Fatalf("family primary nlink = %d, want 3 (FR-039 family size on the tree)", got)
	}
	assertBytes(t, famADest, famPayload)

	// The symlinks: links with their recorded targets; the dangling
	// one stays dangling in the copy tree (D-03 restored-as-link - the
	// dangle is carried faithfully, never materialized).
	validDest := filepath.Join(img1, "files", "link-valid")
	if fi, err := os.Lstat(validDest); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link-valid is %v (err %v), want a symlink", fi.Mode(), err)
	}
	if got, err := os.Readlink(validDest); err != nil || got != "setuid.bin" {
		t.Fatalf("link-valid readlink = %q (%v), want setuid.bin", got, err)
	}
	danglingDest := filepath.Join(img1, "files", "link-dangling")
	if fi, err := os.Lstat(danglingDest); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("link-dangling is %v (err %v), want a symlink", fi.Mode(), err)
	}
	if got, err := os.Readlink(danglingDest); err != nil || got != "does/not/exist.bin" {
		t.Fatalf("link-dangling readlink = %q (%v), want the recorded dangling target", got, err)
	}
	if _, err := os.Stat(danglingDest); !os.IsNotExist(err) {
		t.Fatalf("link-dangling stat = %v, want ENOENT (the dangle must survive the copy)", err)
	}

	// --- catalog and manifest sides ------------------------------------------
	db := catOpen(t, cat)
	assertAllCopied(t, db, setID)
	assertContentKinds(t, db, setID, map[int64]map[string]int{
		1: {"file": 4, "symlink": 2, "dir": 2, "manifest": 1, "export": 1},
	})

	m := readManifest(t, filepath.Join(img1, media.ManifestFileName), setID, 1)
	if len(m.Entries) != 8 { // 2 whole files + 2 secondaries + 2 symlinks + 2 dirs
		t.Fatalf("manifest entries = %d, want 8 payload entries: %+v", len(m.Entries), m.Entries)
	}
	// The manifest's dir entries carry the RAW POSIX mode (the
	// conversion happens exactly at the Apply seam, never before - the
	// T3.4 invariant, asserted from the restore-facing artifact).
	for _, tc := range []struct {
		path string
		mode uint32
	}{{"files/sticky-dir", 0o1777}, {"files/empty-750", 0o750}} {
		var found bool
		for _, e := range m.Entries {
			if e.Kind == media.KindDir && e.MediaPath == tc.path {
				found = true
				if e.Mode != tc.mode {
					t.Fatalf("manifest dir %s mode = %#o, want the raw POSIX %#o (catalog shape, not Go semantics)",
						e.MediaPath, e.Mode, tc.mode)
				}
			}
		}
		if !found {
			t.Fatalf("manifest lacks the dir entry %s: %+v", tc.path, m.Entries)
		}
	}
	// The symlink entries carry their targets (D-03) and the NULL-hash
	// convention.
	for _, tc := range []struct {
		path   string
		target string
	}{{"files/link-valid", "setuid.bin"}, {"files/link-dangling", "does/not/exist.bin"}} {
		var found bool
		for _, e := range m.Entries {
			if e.Kind == media.KindSymlink && e.MediaPath == tc.path {
				found = true
				if e.LinkTarget != tc.target {
					t.Fatalf("manifest symlink %s target = %q, want %q", tc.path, e.LinkTarget, tc.target)
				}
				if e.SHA256 != "" {
					t.Fatalf("manifest symlink %s carries hash %q (symlinks are NULL-hash, 2.3/D-03)", tc.path, e.SHA256)
				}
			}
		}
		if !found {
			t.Fatalf("manifest lacks the symlink entry %s: %+v", tc.path, m.Entries)
		}
	}
	// The full family is recorded for restore (D-04: all names, shared
	// hash).
	if len(m.HardLinkGroups) != 1 || len(m.HardLinkGroups[0].Names) != 3 ||
		m.HardLinkGroups[0].Names[0] != "fam-a.bin" {
		t.Fatalf("manifest hard_link_groups = %+v, want the 3-name fam family", m.HardLinkGroups)
	}

	// --- the resume re-run ----------------------------------------------------
	// Only the idempotent dirs are re-asserted (they carry no copied
	// flag, 2.3); every payload member is trusted and skipped (12.7).
	setuidBefore := fileMtime(t, filepath.Join(img1, "files", "setuid.bin"))
	stdout, stderr, code = runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json")
	if code != 0 {
		t.Fatalf("re-run exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var rep2 copyReport
	if err := json.Unmarshal([]byte(stdout), &rep2); err != nil {
		t.Fatal(err)
	}
	if rep2.Copied != 2 || rep2.AlreadyCopied != 6 || rep2.Failed != 0 || rep2.Skipped != 0 {
		t.Fatalf("re-run report = %+v, want copied=2 (the idempotent dir re-asserts), "+
			"already_copied=6 (2 whole + 2 secondaries + 2 symlinks), nothing failed or skipped", rep2)
	}
	if fileMtime(t, filepath.Join(img1, "files", "setuid.bin")) != setuidBefore {
		t.Fatal("the re-run touched the setuid payload file (resume must skip it entirely)")
	}
	if got := specialsRawMode(t, filepath.Join(img1, "files", "setuid.bin")); got != 0o4755 {
		t.Fatalf("setuid.bin mode after the re-run = %#o, want 04755 untouched", got)
	}
	// The dir re-assert keeps the recorded sticky mode and target
	// mtime (metadata re-applied every run, the T3.4 ordering doc).
	if got := specialsRawMode(t, filepath.Join(img1, "files", "sticky-dir")); got != 0o1777 {
		t.Fatalf("sticky-dir mode after the re-run = %#o, want the re-asserted 01777", got)
	}
	if got, err := os.Readlink(danglingDest); err != nil || got != "does/not/exist.bin" {
		t.Fatalf("link-dangling readlink after the re-run = %q (%v), want the recorded target", got, err)
	}
}
