package verify

// T5.1 adversarial additions (test-only): the copy-tree edges the
// acceptance matrix did not cover yet.
//
//   - a symlink RETARGETED inside the destination after the copy (the
//     path and the kind still match - only the recorded target differs,
//     which is exactly the attribute the osirrox contract lets us
//     compare);
//   - the read-only/refusal honesty of a tree whose listed payload was
//     REPLACED by a directory (kind drift), closing the mirror of the
//     existing kind-mismatch coverage.
//
// The manifest/export-corruption, chunk-isolation, dangling-location,
// and concurrency edges are pinned end-to-end at the cli tier
// (internal/cli/verifymedia_adversarial_test.go) where the real
// *catalog.DB adapter, the report, and the exit code are in scope.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVerifyCopyTreeSymlinkRetargetedIsMismatch: the recorded target is
// the ONLY thing a symlink row can be verified against, so a retargeted
// link in the destination must be a loud target mismatch (never a
// silent pass) and must suppress the stamp exactly like a payload
// corruption.
func TestVerifyCopyTreeSymlinkRetargetedIsMismatch(t *testing.T) {
	f := makeCopyTreeFixture(t)
	deps, st := verifyFixtureDeps(f)

	link := filepath.Join(f.root, "files", "link")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	// Same path, same kind, plausible target - just NOT the recorded one.
	if err := os.Symlink("sub/deep.txt", link); err != nil {
		t.Fatal(err)
	}

	res, err := VerifyImage(context.Background(), deps, VerifyInput{SetID: "S-V", ImageID: 1, Path: f.root})
	if err != nil {
		t.Fatalf("VerifyImage: %v", err)
	}
	if res.Comparison.Clean() || res.Stamped {
		t.Fatalf("retargeted symlink = (clean %v, stamped %v), want (false, false)",
			res.Comparison.Clean(), res.Stamped)
	}
	if len(res.Comparison.Mismatched) != 1 || res.Comparison.Mismatched[0].MediaPath != "files/link" {
		t.Fatalf("mismatched = %+v, want exactly files/link", res.Comparison.Mismatched)
	}
	reason := strings.Join(res.Comparison.Mismatched[0].Reasons, "; ")
	if !strings.Contains(reason, "symlink target") || !strings.Contains(reason, "a.txt") {
		t.Errorf("reason = %q, want the symlink target line naming the RECORDED target a.txt", reason)
	}
	if len(res.Comparison.Missing) != 0 || len(res.Comparison.Unexpected) != 0 || len(res.Comparison.Unreadable) != 0 {
		t.Errorf("other categories = (%v, %v, %v), want empty", res.Comparison.Missing, res.Comparison.Unexpected, res.Comparison.Unreadable)
	}
	// Everything else still verifies: the finding is scoped to the link.
	if len(res.Comparison.Matched) != len(f.list.Rows)-1 {
		t.Errorf("matched %d entries, want %d (all but the retargeted link)",
			len(res.Comparison.Matched), len(f.list.Rows)-1)
	}
	if st.stampCount() != 0 {
		t.Errorf("a retargeted symlink is an anomaly: stamps = %+v, want none", st.stamps)
	}
}

// TestVerifyCopyTreeListedFileReplacedByDirIsKindMismatch: the reverse
// kind drift (a listed FILE whose path now holds a directory) - the
// comparison must report a kind mismatch, never hash bytes that are not
// there, and never stamp.
func TestVerifyCopyTreeListedFileReplacedByDirIsKindMismatch(t *testing.T) {
	f := makeCopyTreeFixture(t)
	deps, st := verifyFixtureDeps(f)

	p := filepath.Join(f.root, "files", "a.txt")
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := VerifyImage(context.Background(), deps, VerifyInput{SetID: "S-V", ImageID: 1, Path: f.root})
	if err != nil {
		t.Fatalf("VerifyImage: %v", err)
	}
	if res.Comparison.Clean() || res.Stamped {
		t.Fatalf("file replaced by dir = (clean %v, stamped %v), want (false, false)",
			res.Comparison.Clean(), res.Stamped)
	}
	if len(res.Comparison.Mismatched) != 1 || res.Comparison.Mismatched[0].MediaPath != "files/a.txt" {
		t.Fatalf("mismatched = %+v, want exactly files/a.txt", res.Comparison.Mismatched)
	}
	if reason := strings.Join(res.Comparison.Mismatched[0].Reasons, "; "); !strings.Contains(reason, "want file") {
		t.Errorf("reason = %q, want the kind line (extracted as dir, want file)", reason)
	}
	// The empty directory the file became is a LISTED path, so it is
	// mismatched - not double-reported as unexpected.
	if len(res.Comparison.Unexpected) != 0 {
		t.Errorf("unexpected = %+v, want none (the path IS listed)", res.Comparison.Unexpected)
	}
	if st.stampCount() != 0 {
		t.Errorf("stamps = %+v, want none", st.stamps)
	}
}
