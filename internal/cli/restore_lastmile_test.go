package cli

// T5.4 last-mile acceptance: restore through the REAL catalog and the REAL
// extraction/reassembly engine (no recording fake) for the contracts that are
// only observable at the command level - the target's actual inode layout for
// a hard-link family, and the report's own count of a multi-image run.
//
//   - the PARTIAL FAMILY, both branches of the doc.go verdict, driven through
//     scan -> plan -> copy -> restore over a real set:
//       * family PRIMARY selected: every requested name restores, all sharing
//         ONE inode (nlink = the family size, i.e. link(2) - not copies);
//       * family PRIMARY UNSELECTED: the bytes land at the FIRST requested
//         secondary's name (the inventory order the engine iterates - pinned
//         with a sentinel file at the second name, which a link(2) must
//         refuse while a content write would have replaced it), the rest
//         link(2) to it, and the unselected primary NEVER appears under
//         --target;
//   - the report's restored TOTAL across a span: exactly one count per
//     selected member, so a chunked parent that completes on a later image
//     still counts once (the NB-04 count-once contract as the user sees it).

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"isopress/internal/catalog"
	"isopress/internal/report"
)

// restoreFamilyFixture scans, plans, and COPIES a set whose hard-link family
// has a primary plus TWO secondaries, plus one unrelated file - the smallest
// fixture in which "first requested name" is distinguishable from "second".
func restoreFamilyFixture(t *testing.T, setID string) (cat, dest, root string) {
	t.Helper()
	root = t.TempDir()
	writeFileP(t, filepath.Join(root, "fam-a.txt"), pattern(5, 800), 0o644)
	for _, sec := range []string{"fam-b.txt", "fam-c.txt"} {
		if err := os.Link(filepath.Join(root, "fam-a.txt"), filepath.Join(root, sec)); err != nil {
			t.Fatalf("hard-link family %s: %v", sec, err)
		}
	}
	writeFileP(t, filepath.Join(root, "solo.txt"), pattern(6, 1200), 0o644)

	cat = createCatalog(t)
	scanBuildFixture(t, root, cat, setID)
	planSynthetic(t, cat, setID, copySyntheticUsable)
	dest = filepath.Join(t.TempDir(), "dest")
	if stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest); code != 0 {
		t.Fatalf("copy fixture exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	return cat, dest, root
}

// catalogSecondaryOrder returns the image's hard-link SECONDARIES in the exact
// order the catalog projects them - which the cli adapter passes through
// unchanged and the engine iterates, so entry 0 is the name that carries the
// family bytes when the primary is unselected ("the FIRST requested name").
func catalogSecondaryOrder(t *testing.T, catPath, setID string, image int64) []string {
	t.Helper()
	db, err := catalog.Open(catPath, catalog.OpenReadOnly)
	if err != nil {
		t.Fatalf("opening the catalog read-only: %v", err)
	}
	defer db.Close() //nolint:errcheck // read-only helper
	inv, err := db.CopyInventory(context.Background(), setID, image)
	if err != nil {
		t.Fatalf("CopyInventory(image %d): %v", image, err)
	}
	out := make([]string, 0, len(inv.Secondaries))
	for _, s := range inv.Secondaries {
		out = append(out, s.RelPath)
	}
	return out
}

// assertTargetExactly requires the target to hold EXACTLY these relative names
// - the "no stray output" half of the last mile (an unselected primary, a
// leaked .partial, or a tool-owned file would show up here).
func assertTargetExactly(t *testing.T, root string, want ...string) {
	t.Helper()
	wantSet := map[string]bool{}
	for _, w := range want {
		wantSet[w] = true
	}
	seen := map[string]bool{}
	err := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		seen[rel] = true
		if !wantSet[rel] {
			t.Errorf("target holds the stray entry %q (want exactly %v)", rel, want)
		}
		if !fi.IsDir() && strings.HasSuffix(rel, ".partial") {
			t.Errorf("target holds the leftover partial %q", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the target: %v", err)
	}
	for w := range wantSet {
		if !seen[w] {
			t.Errorf("target is missing %q (want exactly %v)", w, want)
		}
	}
}

// requireSameInode asserts two restored names are ONE inode (link(2)) with the
// given hard-link count, and that both hold want's bytes.
func requireSameInode(t *testing.T, a, b string, wantNlink uint64, wantContent []byte) {
	t.Helper()
	ai, err := os.Lstat(a)
	if err != nil {
		t.Fatalf("lstat %s: %v", a, err)
	}
	bi, err := os.Lstat(b)
	if err != nil {
		t.Fatalf("lstat %s: %v", b, err)
	}
	if !os.SameFile(ai, bi) {
		t.Errorf("%s and %s must share ONE inode (FR-039): %+v vs %+v", a, b, ai.Sys(), bi.Sys())
	}
	if n := ai.Sys().(*syscall.Stat_t).Nlink; n != wantNlink {
		t.Errorf("%s nlink = %d, want %d", a, n, wantNlink)
	}
	if got := mustRead(t, a); string(got) != string(wantContent) {
		t.Errorf("%s content = %d bytes, want %d", a, len(got), len(wantContent))
	}
	if got := mustRead(t, b); string(got) != string(wantContent) {
		t.Errorf("%s content = %d bytes, want %d", b, len(got), len(wantContent))
	}
}

// familyPrimary returns the catalog's family PRIMARY (the lowest file_id wins
// - the T3.4 rule the selection and the placement follow) and the two
// secondaries in file_id order (the inventory order the engine iterates).
// The scan assigns ids in its own order, so the primary is DERIVED, never
// assumed from a fixture name.
func familyPrimaryAndRest(t *testing.T, catPath, setID string) (string, []string) {
	t.Helper()
	ids := activeFileIDsFor(t, catPath, setID, "fam-a.txt", "fam-b.txt", "fam-c.txt")
	names := []string{"fam-a.txt", "fam-b.txt", "fam-c.txt"}
	primary := names[0]
	for _, n := range names[1:] {
		if ids[n] < ids[primary] {
			primary = n
		}
	}
	rest := make([]string, 0, 2)
	for _, n := range names {
		if n != primary {
			rest = append(rest, n)
		}
	}
	if ids[rest[0]] > ids[rest[1]] {
		rest[0], rest[1] = rest[1], rest[0]
	}
	return primary, rest
}

// --- branch A: the family PRIMARY is selected --------------------------------

// TestRestoreFamilyWithSelectedPrimaryRealEngine: whole-set restore over the
// real set. The primary gets the bytes at its own name and every requested
// secondary becomes link(2) of it - one inode, nlink = 3, content written
// once.
func TestRestoreFamilyWithSelectedPrimaryRealEngine(t *testing.T) {
	cat, dest, root := restoreFamilyFixture(t, "S-RHLA")
	target := restoreTarget(t)

	stdout, stderr, code := runInProcess(t, "restore",
		"--catalog", cat, "--set-id", "S-RHLA", "--target", target,
		"--copy-tree-root", dest)
	if code != report.ExitOK {
		t.Fatalf("restore exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	assertTargetExactly(t, target, "fam-a.txt", "fam-b.txt", "fam-c.txt", "solo.txt")

	want := mustRead(t, filepath.Join(root, "fam-a.txt"))
	requireSameInode(t, filepath.Join(target, "fam-a.txt"), filepath.Join(target, "fam-b.txt"), 3, want)
	requireSameInode(t, filepath.Join(target, "fam-a.txt"), filepath.Join(target, "fam-c.txt"), 3, want)
	if got := mustRead(t, filepath.Join(target, "solo.txt")); string(got) != string(pattern(6, 1200)) {
		t.Errorf("solo.txt content wrong (%d bytes)", len(got))
	}
	// The report counts every selected member once (4 records: 3 family
	// names + solo; the copy set has no empty dir).
	if n, ok := restoredTotal(t, stdout); !ok || n != 4 {
		t.Errorf("report restored total = %d (parsed=%v), want 4\nstdout: %s", n, ok, stdout)
	}
}

// --- branch B: only the SECONDARIES are selected -----------------------------

// TestRestorePartialFamilySecondariesOnlyRealEngine: the shape T5.3's filter
// actually produces (Work carries Secondaries{RelPath, PrimaryRelPath} and NO
// Files row for the primary). The bytes are read from the primary's stored
// name but written at the FIRST requested secondary's name, the remaining
// requested name links(2) to it, and the unselected primary never appears
// under --target.
func TestRestorePartialFamilySecondariesOnlyRealEngine(t *testing.T) {
	cat, dest, root := restoreFamilyFixture(t, "S-RHLB")
	target := restoreTarget(t)

	// The catalog's own family rule (lowest file_id wins) decides which
	// name is the PRIMARY - derive it, don't assume it: the branch under
	// test is "the primary is NOT selected". The secondaries come back in
	// the catalog's inventory order, which is the order the engine iterates.
	primary, secondaries := familyPrimaryAndRest(t, cat, "S-RHLB")
	order := catalogSecondaryOrder(t, cat, "S-RHLB", 1)
	if len(order) != 2 || order[0] != secondaries[0] || order[1] != secondaries[1] {
		t.Fatalf("catalog secondaries = %v, want the non-primary family names in file_id order %v (primary %s)",
			order, secondaries, primary)
	}

	args := []string{"restore", "--catalog", cat, "--set-id", "S-RHLB", "--target", target,
		"--copy-tree-root", dest, "--path", order[0], "--path", order[1], "--path", "solo.txt"}
	stdout, stderr, code := runInProcess(t, args...)
	if code != report.ExitOK {
		t.Fatalf("restore exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// THE partial-selection contract: exactly the requested names - the
	// unselected primary never appears (and no .partial leaks).
	assertTargetExactly(t, target, order[0], order[1], "solo.txt")
	if _, err := os.Lstat(filepath.Join(target, primary)); !os.IsNotExist(err) {
		t.Errorf("the UNSELECTED primary %q must not exist under --target (stat err = %v)", primary, err)
	}
	want := mustRead(t, filepath.Join(root, "fam-a.txt"))
	requireSameInode(t, filepath.Join(target, order[0]), filepath.Join(target, order[1]), 2, want)
	if n, ok := restoredTotal(t, stdout); !ok || n != 3 {
		t.Errorf("report restored total = %d (parsed=%v), want 3\nstdout: %s", n, ok, stdout)
	}
	if strings.Contains(stdout, primary) {
		t.Errorf("the unselected primary %q must not be reported:\n%s", primary, stdout)
	}
}

// TestRestorePartialFamilyAnchorIsFirstRequestedName pins WHICH requested name
// carries the bytes: the engine's first processed secondary (the catalog's
// inventory order - entry 0 above). The second name is pre-occupied by a
// foreign regular file, which distinguishes the two possible anchors
// observably: a content write at the occupied name REPLACES it (a later link
// then succeeds), while a link(2) at the occupied name is REFUSED (no-clobber)
// and leaves the sentinel byte-identical. The engine must take the FIRST
// branch shape only for order[0].
func TestRestorePartialFamilyAnchorIsFirstRequestedName(t *testing.T) {
	cat, dest, root := restoreFamilyFixture(t, "S-RHLC")
	target := restoreTarget(t)

	// The family primary is unselected here by construction: only the two
	// secondaries are requested (derived from the catalog's own rule).
	primary, _ := familyPrimaryAndRest(t, cat, "S-RHLC")
	order := catalogSecondaryOrder(t, cat, "S-RHLC", 1)
	if len(order) != 2 {
		t.Fatalf("catalog secondaries = %v, want two requested names", order)
	}
	if order[0] == primary || order[1] == primary {
		t.Fatalf("fixture anomaly: the primary %q is among the secondaries %v", primary, order)
	}
	anchor, occupied := order[0], order[1]
	// Pre-create the SECOND name as a foreign file: the sentinel.
	mkdirCLI(t, target)
	sentinel := []byte("a file this run did not write")
	if err := os.WriteFile(filepath.Join(target, occupied), sentinel, 0o644); err != nil {
		t.Fatalf("planting the sentinel: %v", err)
	}

	stdout, stderr, code := runInProcess(t, "restore",
		"--catalog", cat, "--set-id", "S-RHLC", "--target", target,
		"--copy-tree-root", dest,
		"--path", anchor, "--path", occupied)
	if code != report.ExitItemFailures {
		t.Fatalf("restore exit = %d, want 10 (the occupied SECONDARY cannot link)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	// The anchor is the FIRST requested name: it restored its bytes here...
	if !strings.Contains(stdout, "restored 1") {
		t.Errorf("stdout must report the one restored anchor:\n%s", stdout)
	}
	if !strings.Contains(stdout, "failed: "+occupied) {
		t.Errorf("stdout must report the occupied second name as failed:\n%s", stdout)
	}
	if !strings.Contains(stdout, "does not share") && !strings.Contains(stdout, "refusing to clobber") {
		t.Errorf("stdout must explain the no-clobber refusal:\n%s", stdout)
	}
	// ... the sentinel is untouched byte for byte, ...
	if got := mustRead(t, filepath.Join(target, occupied)); string(got) != string(sentinel) {
		t.Errorf("the sentinel was clobbered: %q", got)
	}
	// ... the anchor holds the family bytes under its own name ...
	want := mustRead(t, filepath.Join(root, "fam-a.txt"))
	if got := mustRead(t, filepath.Join(target, anchor)); string(got) != string(want) {
		t.Errorf("anchor %s content = %d bytes, want the family bytes (%d)", anchor, len(got), len(want))
	}
	// ... and the unselected primary never appears.
	if _, err := os.Lstat(filepath.Join(target, primary)); !os.IsNotExist(err) {
		t.Errorf("the UNSELECTED primary %q must not exist under --target (stat err = %v)", primary, err)
	}
	assertTargetExactly(t, target, anchor, occupied)
}

// mkdirCLI creates a directory (cli-side helper; restore's is package-private).
func mkdirCLI(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("creating %s: %v", path, err)
	}
}

// restoredTotal parses the report's frozen totals line ("... restored N, ...")
// - the user-visible count of the whole run.
func restoredTotal(t *testing.T, stdout string) (int, bool) {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.HasPrefix(line, "totals: ") {
			continue
		}
		fields := strings.Fields(line)
		for i, f := range fields {
			if f == "restored" && i+1 < len(fields) {
				n, err := strconv.Atoi(strings.TrimSuffix(fields[i+1], ","))
				if err != nil {
					return 0, false
				}
				return n, true
			}
		}
	}
	return 0, false
}
