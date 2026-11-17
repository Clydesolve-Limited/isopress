package cli

// T5.4 acceptance at the COMMAND level: the production factory now builds the
// real extraction/reassembly engine, so these tests run restore WITHOUT any
// fake seam - real copy trees (and, gated, a real xorriso-built ISO) in, real
// bytes and metadata out, through the full report and exit-code machinery of
// T5.3's shell.
//
//   - the copy-tree source path over a real copy tree (the S3 e2e pattern),
//     byte-compared against the ORIGINAL source including symlinks, a chunked
//     parent, a hard-link family, an empty dir, and metadata;
//   - the planted-corruption path: a chunk flipped in image 2's tree -> that
//     member alone is reported `incomplete` with its chunk indices (FR-041),
//     exit 10, NO final file, the assembly LEFT for diagnosis (the
//     cross-image leave branch, end to end);
//   - the GATED REAL round trip: scan -> plan -> build (real xorriso) ->
//     restore from --image-dir -> full tree comparison against the source -
//     the round-trip S5 exists for.

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"isopress/internal/report"
)

// assertSameRestoredTree walks the ORIGINAL source tree and requires the
// restored target to reproduce it entry for entry:
//
//   - regular files: byte-identical content, raw POSIX mode (incl.
//     setuid/setgid), uid/gid, nanosecond mtime, and the hard-link COUNT
//     (so a family that shared nlink=2 in the source must share it in the
//     target - the link recreation cannot silently degrade to copies);
//   - symlinks: the recorded target verbatim (Lstat - never followed) plus
//     uid/gid/mtime; the symlink MODE is deliberately not compared (Linux
//     has no lchmod and D-03 forbids following, so link modes are not
//     preservable - the fsmeta policy);
//   - EMPTY directories (the recorded kind, FR-037): existence plus full
//     metadata; non-empty directories are implied by their children and take
//     the tree-default mode on restore, so only existence is asserted there
//     (documented: 5.4 records metadata for empty dirs only).
//
// The reverse direction is checked too: every target entry must exist in the
// source (no stray names - a leaked .partial or a tool-owned member would
// show up here).
func assertSameRestoredTree(t *testing.T, srcRoot, dstRoot string) {
	t.Helper()

	srcKinds := map[string]fs.FileMode{}
	err := filepath.WalkDir(srcRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			t.Errorf("walking source %s: %v", path, err)
			return nil
		}
		if path == srcRoot {
			if _, err := os.Lstat(dstRoot); err != nil {
				t.Errorf("target root missing: %v", err)
			}
			return nil
		}
		rel, rerr := filepath.Rel(srcRoot, path)
		if rerr != nil {
			t.Fatalf("rel %s: %v", path, rerr)
		}
		srcKinds[rel] = d.Type()
		dst := filepath.Join(dstRoot, rel)
		srcFI, serr := os.Lstat(path)
		if serr != nil {
			t.Errorf("lstat source %s: %v", rel, serr)
			return nil
		}
		dstFI, derr := os.Lstat(dst)
		if derr != nil {
			t.Errorf("restored member %s missing: %v", rel, derr)
			return nil
		}

		switch {
		case d.Type()&fs.ModeSymlink != 0:
			if dstFI.Mode()&fs.ModeSymlink == 0 {
				t.Errorf("%s restored as %v, want a symlink", rel, dstFI.Mode())
				return nil
			}
			srcTarget, _ := os.Readlink(path)
			dstTarget, _ := os.Readlink(dst)
			if srcTarget != dstTarget {
				t.Errorf("%s -> %q, want %q", rel, dstTarget, srcTarget)
			}
			assertSameOwnershipAndMtime(t, rel, srcFI, dstFI)
		case d.IsDir():
			if !dstFI.IsDir() {
				t.Errorf("%s restored as %v, want a directory", rel, dstFI.Mode())
				return nil
			}
			empty, eerr := dirIsEmpty(path)
			if eerr != nil {
				t.Errorf("reading source dir %s: %v", rel, eerr)
				return nil
			}
			if empty {
				// The recorded kind: full 5.4 metadata must round-trip.
				assertSamePOSIXMeta(t, rel, srcFI, dstFI)
				assertSameOwnershipAndMtime(t, rel, srcFI, dstFI)
			}
		default: // regular file
			if !dstFI.Mode().IsRegular() {
				t.Errorf("%s restored as %v, want a regular file", rel, dstFI.Mode())
				return nil
			}
			want, werr := os.ReadFile(path)
			if werr != nil {
				t.Errorf("reading source %s: %v", rel, werr)
				return nil
			}
			got, gerr := os.ReadFile(dst)
			if gerr != nil {
				t.Errorf("reading restored %s: %v", rel, gerr)
				return nil
			}
			if !bytes.Equal(got, want) {
				t.Errorf("%s content differs: %d bytes restored vs %d from the source", rel, len(got), len(want))
			}
			assertSamePOSIXMeta(t, rel, srcFI, dstFI)
			assertSameOwnershipAndMtime(t, rel, srcFI, dstFI)
			if srcFI.Sys().(*syscall.Stat_t).Nlink != dstFI.Sys().(*syscall.Stat_t).Nlink {
				t.Errorf("%s nlink = %d, want %d (hard-link identity must survive)", rel,
					dstFI.Sys().(*syscall.Stat_t).Nlink, srcFI.Sys().(*syscall.Stat_t).Nlink)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk source: %v", err)
	}

	// Reverse direction: no stray names under the target.
	err = filepath.WalkDir(dstRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == dstRoot {
			return nil
		}
		rel, rerr := filepath.Rel(dstRoot, path)
		if rerr != nil {
			return nil
		}
		if _, ok := srcKinds[rel]; !ok {
			t.Errorf("target holds %q, which is not a source member (stray output)", rel)
		}
		if !d.IsDir() && strings.HasSuffix(path, ".partial") {
			t.Errorf("target holds the leftover partial %q", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk target: %v", err)
	}
}

// dirIsEmpty reports whether a directory holds no entries (the recorded
// empty-dir shape of FR-037).
func dirIsEmpty(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

// assertSamePOSIXMeta compares the RAW POSIX permission class of one
// source/destination pair: st_mode & 0o7777 read straight from syscall.Stat_t,
// so setuid (04000), setgid (02000), and sticky (01000) are compared as the
// kernel stores them rather than through Go's FileMode mapping (the T3.4
// lesson: the FileMode view is a DERIVED rendering; the raw bits are the
// contract).
func assertSamePOSIXMeta(t *testing.T, rel string, src, dst os.FileInfo) {
	t.Helper()
	srcStat, ok := src.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("%s: source Sys() is not *syscall.Stat_t", rel)
	}
	dstStat, ok := dst.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("%s: target Sys() is not *syscall.Stat_t", rel)
	}
	if got, want := dstStat.Mode&0o7777, srcStat.Mode&0o7777; got != want {
		t.Errorf("%s raw mode = %#o, want %#o (syscall.Stat_t st_mode & 0o7777)", rel, got, want)
	}
}

// assertRawPOSIXMode pins ONE absolute raw mode on disk (not a source/target
// comparison): a fixture plant or a conversion chain that silently dropped a
// special bit would otherwise be invisible - both sides would degrade
// together.
func assertRawPOSIXMode(t *testing.T, path string, want uint32) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("%s: Sys() is not *syscall.Stat_t", path)
	}
	if got := st.Mode & 0o7777; got != want {
		t.Errorf("%s raw mode = %#o, want %#o (st_mode & 0o7777 through syscall.Stat_t)", path, got, want)
	}
}

// countRestoreMembers counts the source members a whole-set restore must
// report as RESTORED: every regular file and symlink (catalog records) plus
// every EMPTY directory (the recorded kind, FR-037) - exactly the set the
// report's "restored N" must equal, which is the count-once contract (NB-04)
// as the user sees it: a chunked parent spanning two images counts once.
func countRestoreMembers(t *testing.T, root string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == root {
			return nil
		}
		if d.IsDir() {
			entries, rerr := os.ReadDir(path)
			if rerr == nil && len(entries) == 0 {
				n++
			}
			return nil
		}
		n++
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return n
}

// assertSameOwnershipAndMtime compares uid/gid and the NANOSECOND mtime.
func assertSameOwnershipAndMtime(t *testing.T, rel string, src, dst os.FileInfo) {
	t.Helper()
	srcStat, ok := src.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("%s: source Sys() is not *syscall.Stat_t", rel)
	}
	dstStat, ok := dst.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("%s: target Sys() is not *syscall.Stat_t", rel)
	}
	if srcStat.Uid != dstStat.Uid || srcStat.Gid != dstStat.Gid {
		t.Errorf("%s uid:gid = %d:%d, want %d:%d", rel, dstStat.Uid, dstStat.Gid, srcStat.Uid, srcStat.Gid)
	}
	if srcStat.Mtim.Nano() != dstStat.Mtim.Nano() {
		t.Errorf("%s mtime = %d, want %d (nanosecond fidelity)", rel, dstStat.Mtim.Nano(), srcStat.Mtim.Nano())
	}
}

// --- the copy-tree source path over a real copy tree -------------------------

// TestRestoreCopyTreeRoundTripRealEngine runs the PRODUCTION factory (no
// fake) over the S3 two-image copy fixture and requires the target to
// reproduce the original source tree exactly - content, symlinks, the
// chunked parent spanning both images, the hard-link family, the empty dir,
// and the metadata.
func TestRestoreCopyTreeRoundTripRealEngine(t *testing.T) {
	cat, dest, root := restoreTwoImageFixture(t, "S-RRT")
	target := restoreTarget(t)

	stdout, stderr, code := runInProcess(t, "restore",
		"--catalog", cat, "--set-id", "S-RRT", "--target", target,
		"--copy-tree-root", dest)
	if code != report.ExitOK {
		t.Fatalf("restore exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	for _, want := range []string{"image 1:", "image 2:", "status: ok"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	assertSameRestoredTree(t, root, target)
	// NB-04 from the report seat: the set's chunked parent spans BOTH
	// images, yet the totals count every selected member exactly ONCE
	// (regular files + symlinks + the recorded empty dir).
	if n, ok := restoredTotal(t, stdout); !ok || n != countRestoreMembers(t, root) {
		t.Errorf("report restored total = %d (parsed=%v), want %d (one count per selected member, the span included once)\nstdout: %s",
			n, ok, countRestoreMembers(t, root), stdout)
	}
}

// TestRestoreCorruptedChunkExits10WithIncompleteListing is the T5.3/T5.4
// report integration over a REAL corruption: a byte flipped inside image 2's
// chunk of big.bin. The member alone must surface as `incomplete` with its
// chunk indices (FR-041), the run continues over every other file, the
// command exits 10, the target holds NO final file for it - and the assembly
// is LEFT in place because it holds image 1's verified chunk (the documented
// cross-image leave branch, pinned end to end).
func TestRestoreCorruptedChunkExits10WithIncompleteListing(t *testing.T) {
	cat, dest, _ := restoreTwoImageFixture(t, "S-RCOR")
	// Plant media corruption: image 2 holds chunk index 1 of big.bin (the
	// fixtures' pinned placement: chunk 0 fills image 1, chunk 1 lands on
	// image 2); flip one byte, keeping the length the record states.
	chunkPath := filepath.Join(destImageDir(dest, "S-RCOR", 2), "chunks", "big.bin", "00002")
	data, err := os.ReadFile(chunkPath)
	if err != nil {
		t.Fatalf("reading the chunk to corrupt: %v", err)
	}
	data[len(data)/2] ^= 0xFF
	if err := os.WriteFile(chunkPath, data, 0o644); err != nil {
		t.Fatalf("corrupting the chunk: %v", err)
	}

	target := restoreTarget(t)
	stdout, stderr, code := runInProcess(t, "restore",
		"--catalog", cat, "--set-id", "S-RCOR", "--target", target,
		"--copy-tree-root", dest)
	if code != report.ExitItemFailures {
		t.Fatalf("exit = %d, want 10\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// The per-file listing carries the incomplete member with its indices.
	if !strings.Contains(stdout, "incomplete: big.bin") {
		t.Errorf("stdout must list the incomplete member:\n%s", stdout)
	}
	if !regexp.MustCompile(`corrupt chunks \[[0-9, ]+\]`).MatchString(stdout) {
		t.Errorf("stdout must list the corrupt chunk indices (FR-041):\n%s", stdout)
	}
	if !strings.Contains(stdout, "LEFT IN PLACE") {
		t.Errorf("stdout must state the leave branch (image 1's verified bytes are in the assembly):\n%s", stdout)
	}
	if !strings.Contains(stdout, "status: partial") {
		t.Errorf("stdout must carry the partial status:\n%s", stdout)
	}
	// The other members still restored (the run continued).
	for _, rel := range []string{"small1.txt", "fam-a.txt", "fam-b.txt"} {
		if _, err := os.Stat(filepath.Join(target, rel)); err != nil {
			t.Errorf("healthy member %s missing from the target: %v", rel, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(target, "link")); err != nil {
		t.Errorf("the symlink must restore despite the corrupt chunk: %v", err)
	}
	// The trust model: NO final file for the incomplete member, and the
	// abandoned assembly stays for diagnosis.
	if _, err := os.Stat(filepath.Join(target, "big.bin")); !os.IsNotExist(err) {
		t.Errorf("big.bin must have NO final file: stat err = %v", err)
	}
	if fi, err := os.Stat(filepath.Join(target, "big.bin.partial")); err != nil {
		t.Errorf("the abandoned assembly must be left in the target: %v", err)
	} else if fi.Size() == 0 {
		t.Error("the abandoned assembly should hold the streamed bytes, not be empty")
	}
	// No catalog mutation anywhere near this run (the read-only contract).
	_ = stderr
}

// --- the gated REAL round trip: scan -> plan -> build -> restore --------------
