package cli

// U.4 (partial-RemoveAll accounting): the ONE message Track A added
// without a pin, pinned here - see the note below for why a production
// seam is NOT needed after all.
//
// os.RemoveAll can fail AFTER it has already destroyed part of the
// tree, and U.4's wording ("up to N entries may already have been
// removed") is what stops the old message ("cannot remove the
// directory (...)") from implying nothing had happened. The dev
// flagged it unpinned, assuming a failure can only be forced with a
// seam (injectable remover), a root-only trick (chattr +i) or a mount
// point. It can be forced cheaper, as an UNPRIVILEGED user: make every
// DIRECTORY of the released tree read-only (mode &^ 0222).
//
// Why that is deterministic at ANY readdir order:
//   - check 2 (Stat) needs only search permission on the parents -> ok;
//   - check 3 (manifest read) needs read on the file -> ok;
//   - check 4 (countTreeEntries walk) needs read+execute on every dir
//     -> ok, so the pre-removal count N the message quotes is still
//     produced by the real walk;
//   - check 5 (RemoveAll) needs write permission on the PARENT of
//     whatever it unlinks/rmdirs first - and every parent in the tree
//     is read-only - so the very FIRST removal attempt fails with
//     EACCES, at any depth, and the branch renders the new wording
//     with zero entries actually removed ("up to N" stays a true
//     upper bound).
//
// The pin therefore asserts the exact shipped sentence, the entry
// count N the walk produced, no `wipes` record, exit 10, and a
// byte-identical tree. If a future edit reverts the message to the
// pre-U.4 wording (or drops the count), this fails.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestCopyWipeRemoveAllFailureDisclosesPartialRemoval(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root (CAP_DAC_OVERRIDE) would unlink through the read-only dirs this pin needs")
	}
	const setID = "S-WIPE-PARTIAL"
	cat, _, dest := singleImageWipeFixture(t, setID)
	unlockAndReplan(t, cat, setID)
	imageRoot := destImageDir(dest, setID, 1)

	// The pre-removal entry count = the step-4 walk's own number; the
	// message must quote exactly this N.
	wantEntries, _, err := countTreeEntries(imageRoot)
	if err != nil {
		t.Fatalf("counting the fixture tree: %v", err)
	}
	if wantEntries < 2 {
		t.Fatalf("fixture tree has %d entries, want a tree worth counting", wantEntries)
	}

	// Snapshot FIRST (it records modes), then strip write permission
	// from every DIRECTORY (snapshotTree skips the root itself, but the
	// root must be read-only too - it is the first unlink's parent).
	before := snapshotTree(t, imageRoot)
	type dirMode struct {
		path string
		perm fs.FileMode
	}
	var dirs []dirMode
	if err := filepath.WalkDir(imageRoot, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if !d.IsDir() {
			return nil
		}
		fi, ierr := os.Stat(path)
		if ierr != nil {
			return ierr
		}
		if ierr := os.Chmod(path, fi.Mode().Perm()&^0o222); ierr != nil {
			return ierr
		}
		dirs = append(dirs, dirMode{path, fi.Mode().Perm()})
		return nil
	}); err != nil {
		t.Fatalf("making the released tree read-only: %v", err)
	}
	// Restore BEFORE any assertion (the tree checks must walk it, and
	// the snapshot compares modes) - same ordering as the stat-branch
	// pin above.
	defer func() {
		for _, d := range dirs {
			if rerr := os.Chmod(d.path, d.perm); rerr != nil {
				t.Errorf("restoring %s permissions: %v", d.path, rerr)
			}
		}
	}()

	stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json")

	// Restore now (before the assertions, like the defer does at test
	// end) so the comparisons below see the original modes.
	for _, d := range dirs {
		if rerr := os.Chmod(d.path, d.perm); rerr != nil {
			t.Fatalf("restoring %s permissions: %v", d.path, rerr)
		}
	}
	// Nothing was removed (the first removal attempt is the one that
	// fails) and nothing was written: data loss first, message second.
	assertTreeUnchanged(t, before, imageRoot)
	assertWipeRefused(t, cat, setID, code, stdout, stderr,
		fmt.Sprintf("refusing to wipe: cannot remove the directory (permission denied); "+
			"up to %d entries may already have been removed; remove or rename the directory, "+
			"or choose a different --dest", wantEntries))
}
