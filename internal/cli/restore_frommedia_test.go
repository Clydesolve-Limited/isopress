package cli

// T5.5 acceptance at the COMMAND level: media-alone restore
// (DESIGN_SPEC 8.5) through the REAL production path - real scan, plan,
// and copy (and, gated, a real xorriso build + extraction), then
// restore with the ORIGINAL CATALOG DELETED, from the media's own
// MANIFEST.json + catalog-export.sqlite alone.
//
// The decisive round-trips, the adversarial matrix, and the proofs:
//
//   - the deleted-catalog e2e over the two-image copy set (the S-NNN
//     sibling-dir flow): full tree restored byte-exact with metadata,
//     through the real ImageRestorer and the export-as-catalog;
//   - the single-image-tree flow (PATH itself is one S-NNN directory);
//   - the gated REAL-xorriso extracted-ISO-tree flow (a built ISO
//     extracted with xorriso osirrox; the extracted tree IS the
//     image's copy-tree equivalent);
//   - a FUTURE-VERSION export stamped into the media is rejected with
//     the clear schema-too-new message (exit 4) and no target writes;
//   - the MEDIA READ-ONLY PROOF: the media tree is byte-identical
//     after every restore (mode + nanosecond mtime + content hash per
//     member), with ZERO -wal/-shm/-journal sidecars on it, and the
//     temp export copy is gone from --work-dir (the temp-copy
//     rationale pinned);
//   - a TRUNCATED export fails honestly (exit 4);
//   - introspection still answers when the export is unreadable, and
//     the open then fails loudly (exit 4);
//   - a TAMPERED MANIFEST.json fails loudly naming it (corrupt JSON ->
//     exit 3 integrity; a valid-JSON set-id edit -> exit 3 naming both
//     ids through the manifest-vs-export anchor);
//   - the --set-id CROSS-CHECK: match passes, mismatch is a usage
//     error naming both ids (exit 2) - never a silent override;
//   - coexistence with the supply flags per the documented reading:
//     --from-media's trees are the base, --image N=PATH still supplies
//     what the media does not (an extracted tree of another image).

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"isopress/internal/media"
	"isopress/internal/report"
	"isopress/internal/restore"
)

// singleImageProfile is the synthetic usable size that plans the
// single-image fixture onto exactly ONE image (everything fits; the
// 2026-10-20 capacity amendment's minimum overhead is far below it).
const singleImageProfile = 50 << 20

// --- helpers -----------------------------------------------------------------

// fromMediaSingleImageFixture scans, plans, and copies a SINGLE-image
// set (regular files incl. a nested dir, a hard-link family, a
// symlink, and an empty dir), returning the catalog path, the copy
// destination, and the source root.
func fromMediaSingleImageFixture(t *testing.T, setID string) (cat, dest, root string) {
	t.Helper()
	root = t.TempDir()
	writeFileP(t, filepath.Join(root, "small1.txt"), pattern(2, 1000), 0o644)
	writeFileP(t, filepath.Join(root, "docs", "nested.txt"), pattern(3, 700), 0o644)
	writeFileP(t, filepath.Join(root, "fam-a.txt"), pattern(5, 800), 0o644)
	if err := os.Link(filepath.Join(root, "fam-a.txt"), filepath.Join(root, "fam-b.txt")); err != nil {
		t.Fatalf("hard-link family: %v", err)
	}
	if err := os.Symlink("small1.txt", filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "empty-dir"), 0o750); err != nil {
		t.Fatalf("empty dir: %v", err)
	}
	cat = createCatalog(t)
	scanBuildFixture(t, root, cat, setID)
	planSynthetic(t, cat, setID, singleImageProfile)
	dest = filepath.Join(t.TempDir(), "dest")
	if stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest); code != 0 {
		t.Fatalf("copy fixture exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	return cat, dest, root
}

// mediaMember is one media member's identity for the READ-ONLY PROOF:
// the raw st_mode as the kernel stores it, the size, the content hash
// (regular files), the symlink target, and the NANOSECOND mtime. Atime
// is deliberately absent: reads legitimately move it, and the tool
// never writes media.
type mediaMember struct {
	mode  uint32
	size  int64
	sha   string
	link  string
	mtime int64
}

// snapshotTree captures every member under root (Lstat-based, so
// symlinks are snapshotted as links).
func snapshotTree(t *testing.T, root string) map[string]mediaMember {
	t.Helper()
	out := map[string]mediaMember{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
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
		fi, lerr := os.Lstat(path)
		if lerr != nil {
			return lerr
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("%s: Sys() is not *syscall.Stat_t", path)
		}
		m := mediaMember{mode: st.Mode, size: fi.Size(), mtime: st.Mtim.Nano()}
		switch {
		case fi.Mode()&os.ModeSymlink != 0:
			tgt, rerr := os.Readlink(path)
			if rerr != nil {
				return rerr
			}
			m.link = tgt
		case fi.Mode().IsRegular():
			b, rerr := os.ReadFile(path)
			if rerr != nil {
				return rerr
			}
			sum := sha256.Sum256(b)
			m.sha = hex.EncodeToString(sum[:])
		}
		out[rel] = m
		return nil
	})
	if err != nil {
		t.Fatalf("snapshotting %s: %v", root, err)
	}
	return out
}

// assertTreeUnchanged is the MEDIA READ-ONLY PROOF: every member keeps
// its exact bytes, mode, and nanosecond mtime, nothing was added (a
// sidecar would show as an added member), and no SQLite sidecar name
// (-wal, -shm, -journal) exists anywhere under the tree - the
// temp-copy rationale of 8.5 pinned.
func assertTreeUnchanged(t *testing.T, before map[string]mediaMember, root string) {
	t.Helper()
	after := snapshotTree(t, root)
	for rel, want := range before {
		got, ok := after[rel]
		if !ok {
			t.Errorf("media member %q disappeared during the restore", rel)
			continue
		}
		if got != want {
			t.Errorf("media member %q changed during the restore:\n before %+v\n after  %+v", rel, want, got)
		}
	}
	for rel := range after {
		if _, ok := before[rel]; !ok {
			t.Errorf("media gained a new member %q during the restore (nothing may be written to the media)", rel)
		}
		if strings.HasSuffix(rel, "-wal") || strings.HasSuffix(rel, "-shm") || strings.HasSuffix(rel, "-journal") {
			t.Errorf("media holds the SQLite sidecar %q (the temp-copy rationale forbids ANY write to the media)", rel)
		}
	}
}

// tempWorkDir returns an isolated --work-dir and asserts it is EMPTY
// afterwards (the temp export copy and the restore workspace are
// removed on every exit path).
func tempWorkDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("work dir: %v", err)
	}
	t.Cleanup(func() {
		if left := restoreDirNames(t, dir); len(left) != 0 {
			t.Errorf("--work-dir holds residue after the run: %v", left)
		}
	})
	return dir
}

// stampExportUserVersion stamps PRAGMA user_version onto the media's
// export (the test-side tamper of the future-version case) and removes
// any sidecar the stamping itself left behind - the subsequent
// read-only assertions must measure the TOOL's behavior, not this
// test's own write.
func stampExportUserVersion(t *testing.T, exportPath string, version int) {
	t.Helper()
	db, err := sql.Open("sqlite", exportPath)
	if err != nil {
		t.Fatalf("opening the export for stamping: %v", err)
	}
	if _, err := db.Exec("PRAGMA user_version = " + itoa64(version)); err != nil {
		_ = db.Close()
		t.Fatalf("stamping user_version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("closing the stamped export: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		_ = os.Remove(exportPath + suffix)
	}
	// The stamp must have LANDED, or the test would prove nothing.
	check, err := sql.Open("sqlite", "file:"+exportPath+"?mode=ro")
	if err != nil {
		t.Fatalf("re-opening the stamped export: %v", err)
	}
	defer check.Close() //nolint:errcheck // read-only assertion helper
	var got int
	if err := check.QueryRow("PRAGMA user_version").Scan(&got); err != nil || got != version {
		t.Fatalf("user_version = %d (err %v), want %d (the stamp did not land)", got, err, version)
	}
}

func itoa64(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// --- THE DECISIVE E2E: deleted catalog, sibling-dir copy set ------------------

// TestRestoreFromMediaDeletedCatalogRoundTrip is THE T5.5 acceptance
// criterion: scan -> plan -> copy, then DELETE THE ORIGINAL CATALOG and
// restore the whole two-image set from the copy destination alone -
// every selection query answered by the export (active records,
// placement, per-image inventories incl. the chunked span, the
// hard-link family, the empty dir), the real engine restoring byte-exact
// with metadata. The media must come out byte-identical with zero
// sidecars, and the --work-dir must come out empty (the temp copy is
// gone).
func TestRestoreFromMediaDeletedCatalogRoundTrip(t *testing.T) {
	cat, dest, root := restoreTwoImageFixture(t, "S-FM2")
	if err := os.Remove(cat); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog (the decisive step): %v", err)
	}
	before := snapshotTree(t, dest)
	workDir := tempWorkDir(t)
	target := restoreTarget(t)

	stdout, stderr, code := runInProcess(t, "restore",
		"--from-media", dest, "--target", target, "--work-dir", workDir)
	if code != report.ExitOK {
		t.Fatalf("restore --from-media exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// No --set-id was given: the set id came FROM the media.
	for _, want := range []string{"restore: set S-FM2", "image 1:", "image 2:", "status: ok"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	// THE REAL ROUND TRIP: the full tree, byte-exact, with metadata -
	// the chunked parent spanning both images, the hard-link family,
	// the symlink, the empty dir, and the raw POSIX modes.
	assertSameRestoredTree(t, root, target)

	// THE MEDIA READ-ONLY PROOF (the temp-copy rationale pinned).
	assertTreeUnchanged(t, before, dest)

	// And the cross-check PASSTHROUGH: the same restore with a MATCHING
	// --set-id succeeds identically (the cross-check is not a hurdle
	// for honest input).
	target2 := restoreTarget(t)
	stdout, stderr, code = runInProcess(t, "restore",
		"--from-media", dest, "--set-id", "S-FM2", "--target", target2, "--work-dir", tempWorkDir(t))
	if code != report.ExitOK {
		t.Fatalf("restore with a matching --set-id exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	assertSameRestoredTree(t, root, target2)
	assertTreeUnchanged(t, before, dest)
}

// TestRestoreFromMediaSetIDCrossCheckMismatch: --set-id with
// --from-media is a CROSS-CHECK, never an override (8.5's set identity
// comes from the media). A mismatch is a usage error (exit 2) naming
// BOTH ids, with the target untouched.
func TestRestoreFromMediaSetIDCrossCheckMismatch(t *testing.T) {
	cat, dest, _ := restoreTwoImageFixture(t, "S-FMX")
	if err := os.Remove(cat); err != nil {
		t.Fatalf("deleting the catalog: %v", err)
	}
	target := restoreTarget(t)
	stdout, stderr, code := runInProcess(t, "restore",
		"--from-media", dest, "--set-id", "S-EVIL", "--target", target)
	if code != report.ExitUsage {
		t.Fatalf("exit = %d, want 2 (usage)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	for _, want := range []string{"S-EVIL", "S-FMX", "cross-check"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q (both ids and the cross-check reading):\n%s", want, stderr)
		}
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("the target must stay untouched after a cross-check failure: %v", err)
	}
}

// --- the single-image-tree flow (PATH itself is one S-NNN directory) ----------

// TestRestoreFromMediaSingleImageTreeAlone: PATH is the set's ONLY
// copy-set directory (root mode - the extracted-tree SHAPE without
// needing xorriso). The whole set restores from that one tree with the
// catalog deleted, and a --subtree selector rides the same media flow.
func TestRestoreFromMediaSingleImageTreeAlone(t *testing.T) {
	cat, dest, root := fromMediaSingleImageFixture(t, "S-FM1")
	if err := os.Remove(cat); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog: %v", err)
	}
	tree := destImageDir(dest, "S-FM1", 1)
	before := snapshotTree(t, tree)
	target := restoreTarget(t)

	stdout, stderr, code := runInProcess(t, "restore",
		"--from-media", tree, "--target", target, "--work-dir", tempWorkDir(t))
	if code != report.ExitOK {
		t.Fatalf("restore --from-media <single tree> exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "restore: set S-FM1") || !strings.Contains(stdout, "status: ok") {
		t.Errorf("stdout lacks the set line or the ok status:\n%s", stdout)
	}
	assertSameRestoredTree(t, root, target)
	assertTreeUnchanged(t, before, tree)

	// A --subtree selection through the same media (the T5.3 selection
	// against the export-as-catalog, unchanged).
	subTarget := restoreTarget(t)
	stdout, stderr, code = runInProcess(t, "restore",
		"--from-media", tree, "--subtree", "docs", "--target", subTarget)
	if code != report.ExitOK {
		t.Fatalf("subtree restore from media exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(subTarget, "docs", "nested.txt")); err != nil {
		t.Errorf("the subtree member did not restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(subTarget, "small1.txt")); !os.IsNotExist(err) {
		t.Errorf("the subtree selector must not pull sibling files: %v", err)
	}
}

// --- the future-version export (the schema gate through the command) ---------

// TestRestoreFromMediaFutureSchemaExportRejected stamps user_version
// 999 onto the media's export copy: the schema-version gate applies to
// the TEMP COPY exactly as to any catalog open - the clear
// ErrSchemaTooNew message, exit 4, and NO target writes, with the
// scratch removed and the media byte-identical (modulo the test's own
// stamp, taken after it).
func TestRestoreFromMediaFutureSchemaExportRejected(t *testing.T) {
	cat, dest, _ := restoreTwoImageFixture(t, "S-FM999")
	leadExport := filepath.Join(destImageDir(dest, "S-FM999", 1), media.ExportFileName)
	stampExportUserVersion(t, leadExport, 999)
	before := snapshotTree(t, dest) // AFTER the test's own stamp
	if err := os.Remove(cat); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog: %v", err)
	}
	target := restoreTarget(t)

	stdout, stderr, code := runInProcess(t, "restore",
		"--from-media", dest, "--target", target, "--work-dir", tempWorkDir(t))
	if code != report.ExitCatalog {
		t.Fatalf("exit = %d, want 4 (catalog class)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	for _, want := range []string{"newer than this tool", "no downgrade path", itoa64(999)} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks the clear schema-too-new message %q:\n%s", want, stderr)
		}
	}
	if stdout != "" {
		t.Errorf("stdout must stay empty on the abort-before-work refusal, got: %q", stdout)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("a rejected export must write NO target files: %v", err)
	}
	assertTreeUnchanged(t, before, dest)
}

// --- a truncated export fails honestly ---------------------------------------

func TestRestoreFromMediaTruncatedExportFails(t *testing.T) {
	cat, dest, _ := restoreTwoImageFixture(t, "S-FMTR")
	leadExport := filepath.Join(destImageDir(dest, "S-FMTR", 1), media.ExportFileName)
	fi, err := os.Stat(leadExport)
	if err != nil {
		t.Fatalf("stat export: %v", err)
	}
	if fi.Size() <= 128 {
		t.Fatalf("fixture anomaly: export is %d bytes, too small to truncate meaningfully", fi.Size())
	}
	if err := os.Truncate(leadExport, 100); err != nil {
		t.Fatalf("truncating the export: %v", err)
	}
	if err := os.Remove(cat); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog: %v", err)
	}
	target := restoreTarget(t)

	stdout, stderr, code := runInProcess(t, "restore",
		"--from-media", dest, "--target", target, "--work-dir", tempWorkDir(t))
	if code != report.ExitCatalog {
		t.Fatalf("exit = %d, want 4 (the truncated export is a catalog-class failure)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if !strings.Contains(stderr, "catalog") {
		t.Errorf("stderr must identify the catalog failure:\n%s", stderr)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("a truncated export must write NO target files: %v", err)
	}
}

// --- introspection survives an unreadable export; the open fails loudly ------

// TestRestoreFromMediaCorruptExportIntrospectionStillAnswers pins the
// 8.5 two-layer split: the manifest is the QUICK layer and answers
// (set id, image list, placement) with the export full of garbage; the
// command then fails LOUDLY when it opens the export.
func TestRestoreFromMediaCorruptExportIntrospectionStillAnswers(t *testing.T) {
	cat, dest, _ := restoreTwoImageFixture(t, "S-FMCOR")
	leadExport := filepath.Join(destImageDir(dest, "S-FMCOR", 1), media.ExportFileName)
	if err := os.WriteFile(leadExport, []byte("garbage, not a sqlite database at all"), 0o644); err != nil {
		t.Fatalf("corrupting the export: %v", err)
	}
	if err := os.Remove(cat); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog: %v", err)
	}

	// THE QUICK LAYER: introspection answers WITHOUT opening the export.
	info, err := restore.ResolveMedia(dest)
	if err != nil {
		t.Fatalf("ResolveMedia must answer from the manifest alone: %v", err)
	}
	if info.SetID != "S-FMCOR" {
		t.Errorf("introspected set id = %q, want S-FMCOR", info.SetID)
	}
	if nums := info.ImageNumbers(); len(nums) != 2 || nums[0] != 1 || nums[1] != 2 {
		t.Errorf("introspected image list = %v, want [1 2] (what a selection would need)", nums)
	}
	if len(info.Manifest.Placement) != 2 {
		t.Errorf("introspected whole-set placement = %d records, want 2", len(info.Manifest.Placement))
	}

	// THE DEEP LAYER: the command then fails loudly at the open.
	target := restoreTarget(t)
	stdout, stderr, code := runInProcess(t, "restore",
		"--from-media", dest, "--target", target, "--work-dir", tempWorkDir(t))
	if code != report.ExitCatalog {
		t.Fatalf("exit = %d, want 4 (a corrupt export is a catalog-class failure)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if !strings.Contains(stderr, "catalog") {
		t.Errorf("stderr must identify the catalog failure:\n%s", stderr)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("a corrupt export must write NO target files: %v", err)
	}
}

// --- a tampered MANIFEST.json fails loudly, naming it ------------------------

// TestRestoreFromMediaTamperedManifestFailsLoudly covers the tamper
// classes the media-alone flow catches at its own layers:
//
//   - corrupt JSON: the parse layer refuses, naming MANIFEST.json
//     (exit 3, the media class - this media cannot back the run);
//   - a valid-JSON SET-ID edit: two independent anchors fire - in
//     SIBLING MODE the sibling cross-check refuses the mixed set
//     (exit 2, naming both ids and the manifest), and in ROOT MODE the
//     manifest-vs-EXPORT anchor fails naming both ids and the manifest
//     (exit 3).
//
// The third tamper class - a payload-value edit that keeps the set id -
// is deliberately NOT detectable from a first-run media at this layer
// (its export carries no manifest self-row; see
// verifyMediaManifestAnchor's chain documentation): the engine's
// per-member hash verification (FR-041) remains the byte-level anchor,
// which is why the chain is documented honestly instead of overclaimed.
func TestRestoreFromMediaTamperedManifestFailsLoudly(t *testing.T) {
	cat, dest, _ := restoreTwoImageFixture(t, "S-FMTAM")
	leadManifest := filepath.Join(destImageDir(dest, "S-FMTAM", 1), media.ManifestFileName)
	if err := os.Remove(cat); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog: %v", err)
	}
	// The pristine bytes, captured BEFORE the (a) tamper consumes the
	// file: (b) re-writes a parseable manifest with a foreign set id.
	orig, err := os.ReadFile(leadManifest)
	if err != nil {
		t.Fatalf("reading the pristine manifest: %v", err)
	}

	// (a) Corrupt JSON.
	if err := os.WriteFile(leadManifest, []byte("{definitely not json"), 0o644); err != nil {
		t.Fatalf("tampering the manifest: %v", err)
	}
	target := restoreTarget(t)
	stdout, stderr, code := runInProcess(t, "restore",
		"--from-media", dest, "--target", target, "--work-dir", tempWorkDir(t))
	if code != report.ExitDependency {
		t.Fatalf("corrupt-manifest exit = %d, want 3 (the media class)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	for _, want := range []string{media.ManifestFileName, "media integrity"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("a tampered manifest must write NO target files: %v", err)
	}

	// (b) A valid-JSON SET-ID edit: parseable, but the media's own
	// anchors fail - and there are TWO of them, one per PATH shape.
	man, err := media.ParseManifest(orig)
	if err != nil {
		t.Fatalf("parsing the pristine manifest: %v", err)
	}
	man.SetID = "S-EVIL"
	edited, err := media.MarshalManifest(man)
	if err != nil {
		t.Fatalf("re-marshaling the tampered manifest: %v", err)
	}
	if err := os.WriteFile(leadManifest, edited, 0o644); err != nil {
		t.Fatalf("writing the tampered manifest: %v", err)
	}
	// (b1) SIBLING MODE: the sibling cross-check refuses the mixed set
	// at the media's own gate - naming both ids and the manifest.
	target2 := restoreTarget(t)
	stdout, stderr, code = runInProcess(t, "restore",
		"--from-media", dest, "--target", target2, "--work-dir", tempWorkDir(t))
	if code != report.ExitUsage {
		t.Fatalf("set-id-tampered (sibling) exit = %d, want 2 (usage: the media mixes sets)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	for _, want := range []string{media.ManifestFileName, "S-EVIL", "S-FMTAM", "mixes backup sets"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q (the sibling cross-check must name the manifest and both ids):\n%s", want, stderr)
		}
	}

	// (b2) ROOT MODE (a single tree, no sibling to disagree with): the
	// manifest-vs-EXPORT anchor in the command flow fails naming both
	// ids and the manifest (exit 3, the media class).
	catB, destB, _ := fromMediaSingleImageFixture(t, "S-FMTAM2")
	if err := os.Remove(catB); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog: %v", err)
	}
	treeB := destImageDir(destB, "S-FMTAM2", 1)
	manifestB := filepath.Join(treeB, media.ManifestFileName)
	blobB, err := os.ReadFile(manifestB)
	if err != nil {
		t.Fatalf("reading the single-tree manifest: %v", err)
	}
	manB, err := media.ParseManifest(blobB)
	if err != nil {
		t.Fatalf("parsing the single-tree manifest: %v", err)
	}
	manB.SetID = "S-EVIL"
	editedB, err := media.MarshalManifest(manB)
	if err != nil {
		t.Fatalf("re-marshaling: %v", err)
	}
	if err := os.WriteFile(manifestB, editedB, 0o644); err != nil {
		t.Fatalf("writing the tampered single-tree manifest: %v", err)
	}
	target3 := restoreTarget(t)
	stdout, stderr, code = runInProcess(t, "restore",
		"--from-media", treeB, "--target", target3, "--work-dir", tempWorkDir(t))
	if code != report.ExitDependency {
		t.Fatalf("set-id-tampered (root) exit = %d, want 3 (the media class)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	for _, want := range []string{media.ManifestFileName, "S-EVIL", "S-FMTAM2", "disagrees"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q (the export anchor must name the manifest and both ids):\n%s", want, stderr)
		}
	}
	if _, err := os.Stat(target3); !os.IsNotExist(err) {
		t.Errorf("a tampered manifest must write NO target files: %v", err)
	}
}

// --- coexistence with the supply flags (the documented reading) ---------------

// TestRestoreFromMediaCoexistenceExplicitImage: PATH is ONE image's
// tree of a multi-image set (the media base supplies image 1), and the
// still-missing image 2 comes through the EXPLICIT --image N=PATH
// mapping (8.5 is silent on coexistence; the documented reading: the
// media's derived supply is the base consulted FIRST, the 8.3 sources
// supplement what it does not resolve).
func TestRestoreFromMediaCoexistenceExplicitImage(t *testing.T) {
	cat, dest, root := restoreTwoImageFixture(t, "S-FMCO")
	if err := os.Remove(cat); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog: %v", err)
	}
	tree1 := destImageDir(dest, "S-FMCO", 1)
	tree2 := destImageDir(dest, "S-FMCO", 2)
	target := restoreTarget(t)

	stdout, stderr, code := runInProcess(t, "restore",
		"--from-media", tree1, "--image", "2="+tree2,
		"--target", target, "--work-dir", tempWorkDir(t))
	if code != report.ExitOK {
		t.Fatalf("restore exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// The report shows WHERE each image came from: the media tree for
	// image 1, the explicit mapping for image 2.
	if !strings.Contains(stdout, "image 1: restored "+tree1) {
		t.Errorf("stdout must show image 1 restored FROM THE MEDIA TREE:\n%s", stdout)
	}
	if !strings.Contains(stdout, "image 2: restored "+tree2) {
		t.Errorf("stdout must show image 2 restored from the explicit mapping:\n%s", stdout)
	}
	assertSameRestoredTree(t, root, target)
}

// --- the usage gates -----------------------------------------------------------

func TestRestoreFromMediaUsageGates(t *testing.T) {
	mediaDir := t.TempDir() // exists but holds no manifest: only the flag-shape gates run here
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"catalog and from-media are exclusive",
			[]string{"restore", "--catalog", "c.sqlite", "--from-media", "/m", "--set-id", "S", "--target", "/tmp/t"},
			"--catalog and --from-media are mutually exclusive"},
		{"from-media without --target",
			[]string{"restore", "--from-media", "/m"}, "--target is required"},
		{"tilde from-media",
			[]string{"restore", "--from-media", "~/media", "--target", "/tmp/t"}, "literal ~"},
		{"missing from-media path",
			[]string{"restore", "--from-media", "/nonexistent/media/dir", "--target", "/tmp/t"}, "cannot be read"},
		{"no manifest in the media",
			[]string{"restore", "--from-media", mediaDir, "--target", "/tmp/t"}, media.ManifestFileName},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code := runInProcess(t, tc.args...)
			if code != report.ExitUsage {
				t.Fatalf("exit = %d, want 2 (usage)\nstderr: %s", code, stderr)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr lacks %q:\n%s", tc.want, stderr)
			}
		})
	}
}

// --- the GATED REAL round trip: build -> extract -> restore from the tree ----
