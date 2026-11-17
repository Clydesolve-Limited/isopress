package cli

// T3.3: the copy command's acceptance matrix (DESIGN_SPEC 5.1/5.2/5.3/
// 6.6, 10.2 exit codes, 12.7 crash/resume; IMPLEMENTATION_PLAN T3.3
// and the delegated brief's e2e list). Everything drives the REAL
// production entry path - a re-exec child running execute() over the
// real command tree with real process streams (the cli_test.go
// pattern) - against a real scanned + planned set.
//
// THE PLAN STEP'S PROFILE INJECTION (documented judgment call): the
// e2e needs a plan whose destinations are KILOBYTES so one fixture
// spans two images with chunk boundaries (the brief's "chunked file
// spanning 2 images"). units.ParseSize accepts whole GB only (4.1's
// documented minimum), so the CLI-level `plan --size` cannot express a
// 10 KB destination. Seeding assignments via raw SQL was the other
// option; this test instead drives the REAL plan pipeline
// (plan.Balance + the plan command's own writeAssignments hook, the
// very code `isopress plan` runs) with only the capacity profile
// substituted - assignments still flow through the production
// transaction, never through test-side SQL. The CLI-level plan path
// itself is covered by plan_test.go.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"isopress/internal/catalog"
	"isopress/internal/media"
	"isopress/internal/plan"
	"isopress/internal/units"
)

// --- helpers -----------------------------------------------------------------

// runCopyChild runs the real copy command with EXTRA environment
// variables (the 12.7 test seam) through the production execute path.
func runCopyChild(t *testing.T, extraEnv []string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(append(os.Environ(), cliHelperEnv+"=1"), extraEnv...)
	outBuf, errBuf := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = outBuf, errBuf
	err := cmd.Run()
	code := 0
	if err != nil {
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("running copy child %v: %v (stderr: %s)", args, err, errBuf.String())
		}
		code = exitErr.ExitCode()
	}
	return outBuf.String(), errBuf.String(), code
}

// planSynthetic runs the REAL plan pipeline (Balance + writeAssignments,
// the plan command's own hooks) with a synthetic capacity profile -
// see the file doc for why the profile is injected.
func planSynthetic(t *testing.T, catPath, setID string, usableBytes int64) {
	t.Helper()
	db, err := catalog.Open(catPath, catalog.OpenReadWrite)
	if err != nil {
		t.Fatalf("opening catalog for the synthetic plan: %v", err)
	}
	defer db.Close()
	profile := units.Profile{
		Name:         "disk",
		NominalBytes: usableBytes * 100 / 98,
		UsableBytes:  usableBytes,
	}
	balanced, err := plan.Balance(context.Background(),
		planCatalogStore{db: db, setID: setID}, profile)
	if err != nil {
		t.Fatalf("plan.Balance: %v", err)
	}
	if err := writeAssignments(context.Background(), db, setID, &balanced); err != nil {
		t.Fatalf("writeAssignments: %v", err)
	}
}

// Synthetic usable sizes under the 2026-10-20 capacity amendment
// (DESIGN_SPEC 4.3): plan.Balance now refuses profiles whose
// usable_size cannot hold the reserved image overhead
// (units.FixedImageOverhead + units.ImageEntryOverhead = 2,098,688
// bytes), so the pre-amendment 10,000/100,000-byte profiles are gone.
//
//   - copySyntheticUsable (3,000,000) yields a one-image payload
//     budget of 901,824 and an effective chunk size of 901,312
//     (usable - fixed overhead - entry charge);
//   - copyBigChunkedLen (1,352,000) is the fixtures' "big" spanning
//     file under that profile: it chunks into 901,312 + 450,688, and
//     chunk 0's charge (901,312 + 512) EXACTLY fills image 1's budget
//   - the "chunk 0 fills image 1" premise the copy fixtures keep.
const (
	copySyntheticUsable = 3_000_000
	copyBigChunkedLen   = 1_352_000
	copyChunk0Len       = 901_312
)

// writeFileP writes a file with the given mode (fixture builder).
func writeFileP(t *testing.T, path string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatal(err)
	}
}

// sha256Hex is the lowercase-hex digest helper (Sum256 returns an
// array value that cannot be sliced inline).
func copySha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// pattern returns a deterministic byte pattern of n bytes.
func pattern(seed byte, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = seed + byte(i%251)
	}
	return b
}

// destImageDir renders the copy-tree directory of one image (5.3/6.4).
func destImageDir(dest, setID string, image int64) string {
	return filepath.Join(dest, fmt.Sprintf("%s-%03d", setID, image))
}

// --- the full-fixture e2e ----------------------------------------------------

// The brief's e2e: a fixture tree (regular files, a chunked file
// SPANNING two images, a symlink, an empty dir, and a hard-link
// family) -> scan -> REAL plan pipeline (synthetic profile) -> copy ->
// assert tree fidelity, metadata, flags, content rows, manifests,
// exports, and a clean resume re-run.
func TestCopyEndToEndTwoImageFixture(t *testing.T) {
	root := t.TempDir()
	cat := filepath.Join(t.TempDir(), "catalog.sqlite")
	dest := filepath.Join(t.TempDir(), "dest")
	setID := "S-E2E"

	// copyBigChunkedLen = 1,352,000 = chunk 901,312 + 450,688 under
	// the copySyntheticUsable profile (chunk 0 exactly fills image 1's
	// reserved budget; the 2026-10-20 re-baseline of the old
	// 15000 = 10000 + 5000 fixture).
	big := pattern(1, copyBigChunkedLen)
	small1 := pattern(2, 1000)
	small2 := pattern(3, 1000)
	small3 := pattern(4, 1000)
	fam := pattern(5, 800)
	writeFileP(t, filepath.Join(root, "big.bin"), big, 0o644)
	writeFileP(t, filepath.Join(root, "small1.txt"), small1, 0o600)
	writeFileP(t, filepath.Join(root, "small2.txt"), small2, 0o644)
	writeFileP(t, filepath.Join(root, "small3.txt"), small3, 0o644)
	writeFileP(t, filepath.Join(root, "fam-a.txt"), fam, 0o644)
	if err := os.Link(filepath.Join(root, "fam-a.txt"), filepath.Join(root, "fam-b.txt")); err != nil {
		t.Fatalf("creating the hard-link family: %v", err)
	}
	if err := os.Symlink("small1.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "empty-dir"), 0o750); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := runScanChildForCopy(t, "--catalog", cat, "--set-id", setID, root)
	if code != 0 {
		t.Fatalf("scan exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	_ = stdout
	// The REAL plan pipeline with the synthetic usable destination: the
	// 1,352,000-byte file chunks into 901,312 + 450,688, and WFD places
	// the two chunks on different images (chunk0 exactly fills image 1's
	// reserved budget), so the fixture spans TWO images with a chunked
	// parent.
	planSynthetic(t, cat, setID, copySyntheticUsable)

	stdout, stderr, code = runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json")
	if code != 0 {
		t.Fatalf("copy exit = %d, want 0 (stderr: %s)\nstdout: %s", code, stderr, stdout)
	}
	var rep copyReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("copy --json output does not parse: %v\n%s", err, stdout)
	}
	// The FROZEN top-level key set (the deliberate schema pin). The
	// "failures" key is omitempty (the plan/scan report convention),
	// so it joins the frozen set exactly when the run has failures -
	// pinned on both sides below and by the failure-path tests. "wipes"
	// is ALWAYS present (4.6.1 channel 2; [] when nothing was wiped) -
	// the ratified 12 -> 13 addition.
	wantKeys := []string{
		"already_copied", "bytes", "copied", "dest", "dirs", "failed",
		"images", "metadata_warnings", "run_id", "set_id", "skipped", "status",
		"wipes",
	}
	if len(rep.Failures) > 0 {
		wantKeys = append(wantKeys, "failures")
		sort.Strings(wantKeys)
	}
	var keyMap map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &keyMap); err != nil {
		t.Fatal(err)
	}
	var gotKeys []string
	for k := range keyMap {
		gotKeys = append(gotKeys, k)
	}
	sort.Strings(gotKeys)
	if strings.Join(gotKeys, ",") != strings.Join(wantKeys, ",") {
		t.Fatalf("copy --json key set = %v, want the frozen set %v", gotKeys, wantKeys)
	}
	if rep.Status != "ok" || rep.Failed != 0 || rep.Skipped != 0 {
		t.Fatalf("report = %+v, want a clean ok run", rep)
	}
	if len(rep.Images) != 2 {
		t.Fatalf("images in report = %d, want 2", len(rep.Images))
	}

	// --- destination tree fidelity (5.3 + 5.1 content discipline) ---
	img1, img2 := destImageDir(dest, setID, 1), destImageDir(dest, setID, 2)
	assertBytes(t, filepath.Join(img1, "chunks", "big.bin", "00001"), big[:copyChunk0Len])
	assertBytes(t, filepath.Join(img2, "chunks", "big.bin", "00002"), big[copyChunk0Len:])
	assertBytes(t, filepath.Join(img2, "files", "small1.txt"), small1)
	assertBytes(t, filepath.Join(img2, "files", "small2.txt"), small2)
	assertBytes(t, filepath.Join(img2, "files", "small3.txt"), small3)
	assertBytes(t, filepath.Join(img2, "files", "fam-a.txt"), fam)
	assertBytes(t, filepath.Join(img2, "files", "fam-b.txt"), fam) // via link(2), same bytes

	// The chunked parent is NOT materialized under files/ (5.3: its
	// bytes live in the chunk tree).
	if _, err := os.Lstat(filepath.Join(img1, "files", "big.bin")); !os.IsNotExist(err) {
		t.Fatalf("chunked parent must not appear under files/ (err=%v)", err)
	}

	// The hard-link family shares an inode on the destination (D-04,
	// FR-039) and the symlink is a symlink with the recorded target
	// (D-03), never a copy of its target.
	famA, err := os.Lstat(filepath.Join(img2, "files", "fam-a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	famB, err := os.Lstat(filepath.Join(img2, "files", "fam-b.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(famA, famB) {
		t.Fatal("fam-a and fam-b do not share an inode on the destination (D-04 link(2))")
	}
	linkInfo, err := os.Lstat(filepath.Join(img2, "files", "link"))
	if err != nil {
		t.Fatal(err)
	}
	if linkInfo.Mode()&os.ModeSymlink == 0 {
		t.Fatal("link was not recreated as a symlink (D-03)")
	}
	target, err := os.Readlink(filepath.Join(img2, "files", "link"))
	if err != nil || target != "small1.txt" {
		t.Fatalf("symlink target = %q (%v), want small1.txt", target, err)
	}

	// The empty dir lands on the LOWEST-NUMBERED image whose reserved
	// budget absorbs its 512-byte entry charge (FR-037, 4.3 as amended
	// 2026-10-20): image 1 is EXACTLY packed by chunk 0's charge, so
	// the dir falls forward to image 2 - with its recorded mode.
	dirInfo, err := os.Lstat(filepath.Join(img2, "files", "empty-dir"))
	if err != nil || !dirInfo.IsDir() {
		t.Fatalf("empty dir missing on image 2 (err=%v)", err)
	}
	if dirInfo.Mode().Perm() != 0o750 {
		t.Fatalf("empty dir mode = %04o, want the recorded 0750", dirInfo.Mode().Perm())
	}

	// Metadata (5.4): whole files carry the recorded mode + exact mtime
	// (mode 0600 on small1 is deliberately distinct from the default).
	s1Info, err := os.Stat(filepath.Join(img2, "files", "small1.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if s1Info.Mode().Perm() != 0o600 {
		t.Fatalf("small1 mode = %04o, want the recorded 0600", s1Info.Mode().Perm())
	}
	srcInfo, err := os.Stat(filepath.Join(root, "small1.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !s1Info.ModTime().Equal(srcInfo.ModTime()) {
		t.Fatalf("small1 mtime = %v, want the source mtime %v (5.4)", s1Info.ModTime(), srcInfo.ModTime())
	}

	// --- the catalog side: flags and the 6.6 content list ---
	db := catOpen(t, cat)
	assertAllCopied(t, db, setID)
	assertContentKinds(t, db, setID, map[int64]map[string]int{
		// The reservation makes image 1 exactly full on chunk 0, so
		// the empty dir's entry lands on image 2 (the lowest with
		// entry slack) - the 2026-10-20 re-baseline.
		1: {"chunk": 1, "manifest": 1, "export": 1},
		2: {"file": 5, "symlink": 1, "chunk": 1, "dir": 1, "manifest": 1, "export": 1},
	})

	// --- MANIFEST.json per image, valid per T3.1's format ---
	m1 := readManifest(t, filepath.Join(img1, media.ManifestFileName), setID, 1)
	m2 := readManifest(t, filepath.Join(img2, media.ManifestFileName), setID, 2)
	if len(m1.Entries) != 1 { // just the chunk (the dir moved to image 2)
		t.Fatalf("image 1 manifest entries = %d, want 1 (the chunk): %+v", len(m1.Entries), m1.Entries)
	}
	if len(m2.Entries) != 8 { // 3 smalls + fam-a + fam-b + symlink + chunk + dir
		t.Fatalf("image 2 manifest entries = %d, want 8 payload entries: %+v", len(m2.Entries), m2.Entries)
	}
	// The reassembly table anchors the chunked parent on BOTH images.
	for _, m := range []media.Manifest{m1, m2} {
		if len(m.ChunkedFiles) != 1 || m.ChunkedFiles[0].RelPath != "big.bin" ||
			len(m.ChunkedFiles[0].Chunks) != 1 {
			t.Fatalf("manifest %d chunked_files = %+v, want big.bin with its one chunk here",
				m.ImageNumber, m.ChunkedFiles)
		}
		if m.ChunkedFiles[0].SHA256 != copySha256Hex(big) {
			t.Fatalf("manifest %d chunked parent anchor is wrong", m.ImageNumber)
		}
		if len(m.HardLinkGroups) != 1 || strings.Join(m.HardLinkGroups[0].Names, ",") != "fam-a.txt,fam-b.txt" {
			t.Fatalf("manifest %d hard_link_groups = %+v, want the fam family", m.ImageNumber, m.HardLinkGroups)
		}
		if len(m.Placement) != 2 {
			t.Fatalf("manifest %d placement = %+v, want both images (FR-035: whole set on every image)",
				m.ImageNumber, m.Placement)
		}
	}
	// Chunk entry hashes in the manifest equal the actual part bytes.
	if got := m2.Entries; !manifestHasChunk(got, copySha256Hex(big[copyChunk0Len:])) {
		t.Fatalf("image 2 manifest lacks the second chunk's hash: %+v", got)
	}

	// --- the export: ONE run's bytes, byte-identical in every image,
	// opens read-only, integrity ok, full placement inside (T3.2/FR-035) ---
	exp1, err := os.ReadFile(filepath.Join(img1, media.ExportFileName))
	if err != nil {
		t.Fatal(err)
	}
	exp2, err := os.ReadFile(filepath.Join(img2, media.ExportFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(exp1, exp2) {
		t.Fatal("the embedded exports differ across images; FR-035 embeds ONE snapshot everywhere")
	}
	expPath := filepath.Join(t.TempDir(), "exported.sqlite")
	if err := os.WriteFile(expPath, exp1, 0o644); err != nil {
		t.Fatal(err)
	}
	expDB := catOpen(t, expPath)
	var verdict string
	if err := expDB.QueryRow(`PRAGMA integrity_check`).Scan(&verdict); err != nil || verdict != "ok" {
		t.Fatalf("export integrity_check = %q (%v), want ok", verdict, err)
	}
	var imagesInExport int
	if err := expDB.QueryRow(`SELECT COUNT(*) FROM images WHERE set_id = ?`, setID).Scan(&imagesInExport); err != nil {
		t.Fatal(err)
	}
	if imagesInExport != 2 {
		t.Fatalf("export images rows = %d, want the full placement (2)", imagesInExport)
	}
	var placedChunks int
	if err := expDB.QueryRow(`SELECT COUNT(*) FROM chunks WHERE set_id = ? AND image_id IS NOT NULL`, setID).
		Scan(&placedChunks); err != nil {
		t.Fatal(err)
	}
	if placedChunks != 2 {
		t.Fatalf("export placed chunks = %d, want 2", placedChunks)
	}

	// --- the resume re-run: copies NOTHING (12.7/FR-013 trust model) ---
	beforeMtime := fileMtime(t, filepath.Join(img2, "files", "small1.txt"))
	stdout, stderr, code = runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json")
	if code != 0 {
		t.Fatalf("re-run exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var rep2 copyReport
	if err := json.Unmarshal([]byte(stdout), &rep2); err != nil {
		t.Fatal(err)
	}
	if rep2.Copied != 1 || rep2.AlreadyCopied != 8 || rep2.Failed != 0 || rep2.Skipped != 0 {
		t.Fatalf("re-run report = %+v, want copied=1 (the idempotent dir re-assert), "+
			"already_copied=8 (2 chunks + 5 files/links + 1 symlink), nothing failed or skipped", rep2)
	}
	if fileMtime(t, filepath.Join(img2, "files", "small1.txt")) != beforeMtime {
		t.Fatal("the re-run touched an already-copied payload file (resume must skip it entirely)")
	}
}

func runScanChildForCopy(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	return runCLIChild(t, append([]string{"scan"}, args...)...)
}

func assertBytes(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		sum := sha256.Sum256(got)
		t.Fatalf("%s content mismatch: got %d bytes sha256 %x, want %d bytes", path, len(got), sum, len(want))
	}
}

func fileMtime(t *testing.T, path string) time.Time {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

// assertAllCopied pins the resume trust model's precondition: every
// files and chunks row of the set carries copied=1 after a clean run.
func assertAllCopied(t *testing.T, db *sql.DB, setID string) {
	t.Helper()
	var unmarked int
	if err := db.QueryRow(`SELECT COUNT(*) FROM files WHERE set_id = ? AND copied = 0`, setID).
		Scan(&unmarked); err != nil {
		t.Fatal(err)
	}
	if unmarked != 0 {
		t.Fatalf("%d files rows still carry copied=0 after a clean run", unmarked)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM chunks WHERE set_id = ? AND copied = 0`, setID).
		Scan(&unmarked); err != nil {
		t.Fatal(err)
	}
	if unmarked != 0 {
		t.Fatalf("%d chunk rows still carry copied=0 after a clean run", unmarked)
	}
}

// assertContentKinds pins the 6.6 content list: the image_contents
// rows per image, by kind (payload rows written with their marks, the
// manifest/export self-rows appended at embed time).
func assertContentKinds(t *testing.T, db *sql.DB, setID string, want map[int64]map[string]int) {
	t.Helper()
	for image, wantKinds := range want {
		rows, err := db.Query(
			`SELECT kind, COUNT(*) FROM image_contents WHERE set_id = ? AND image_id = ? GROUP BY kind`,
			setID, image)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]int{}
		for rows.Next() {
			var kind string
			var n int
			if err := rows.Scan(&kind, &n); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			got[kind] = n
		}
		_ = rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if len(got) != len(wantKinds) {
			t.Fatalf("image %d content kinds = %v, want %v", image, got, wantKinds)
		}
		for kind, n := range wantKinds {
			if got[kind] != n {
				t.Fatalf("image %d content kind %q = %d, want %d (full map: %v)",
					image, kind, got[kind], n, got)
			}
		}
	}
}

func readManifest(t *testing.T, path, setID string, image int64) media.Manifest {
	t.Helper()
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading manifest %s: %v", path, err)
	}
	var m media.Manifest
	if err := json.Unmarshal(blob, &m); err != nil {
		t.Fatalf("manifest %s does not parse: %v", path, err)
	}
	if m.SetID != setID || m.ImageNumber != image {
		t.Fatalf("manifest %s header = (%s, %d), want (%s, %d)", path, m.SetID, m.ImageNumber, setID, image)
	}
	if m.ToolVersion == "" || m.SchemaVersion < 1 || m.CreatedAt == "" {
		t.Fatalf("manifest %s header incomplete: %+v", path, m)
	}
	return m
}

func manifestHasChunk(entries []media.Entry, wantHash string) bool {
	for _, e := range entries {
		if e.Kind == media.KindChunk && e.SHA256 == wantHash {
			return true
		}
	}
	return false
}

// --- the T6.5 completion stamps (mode + state, catalog honesty) ---------------

// TestCopyCompletionStampsImagesModeAndState pins the T6.5 gap
// closure through the REAL command chain (scan -> plan -> copy, the
// verifyCopyFixture path): every image the copy COMPLETES carries
// images.mode='copy' and images.state='built' beside final_path -
// 2.3's "mode ... set at first write" and the design-vocabulary state
// choice documented in catalog.RecordCopyFinalPath (flagged for PM
// ratification). The stamps ride the SAME idempotent statement as
// final_path, so a resume re-run keeps them exactly.
func TestCopyCompletionStampsImagesModeAndState(t *testing.T) {
	const setID = "S-VCOPYMODE"
	cat, dest := verifyCopyFixture(t, setID)

	type imageRow struct {
		id        int64
		state     string
		mode      sql.NullString
		finalPath sql.NullString
	}
	readRows := func(stage string) []imageRow {
		t.Helper()
		rows, err := catOpen(t, cat).Query(
			`SELECT image_id, state, mode, final_path FROM images WHERE set_id = ? ORDER BY image_id`, setID)
		if err != nil {
			t.Fatalf("%s: reading images rows: %v", stage, err)
		}
		defer func() { _ = rows.Close() }()
		var out []imageRow
		for rows.Next() {
			var r imageRow
			if err := rows.Scan(&r.id, &r.state, &r.mode, &r.finalPath); err != nil {
				t.Fatalf("%s: scanning images row: %v", stage, err)
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("%s: iterating images rows: %v", stage, err)
		}
		return out
	}
	assertStamped := func(stage string, rows []imageRow) {
		t.Helper()
		if len(rows) == 0 {
			t.Fatalf("%s: no images rows for set %s", stage, setID)
		}
		for _, r := range rows {
			if r.state != "built" || r.mode.String != "copy" || !r.mode.Valid {
				t.Errorf("%s: image %d = (state %q, mode %v), want ('built', 'copy') (T6.5 completion stamps)",
					stage, r.id, r.state, r.mode)
			}
			if want := destImageDir(dest, setID, r.id); r.finalPath.String != want {
				t.Errorf("%s: image %d final_path = %q, want the copy dir %q",
					stage, r.id, r.finalPath.String, want)
			}
		}
	}

	assertStamped("first copy", readRows("first copy"))

	// A resume re-run re-completes the image through the SAME
	// statement: the provenance columns stay exactly as recorded (and
	// nothing else on the row moves).
	if stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest); code != 0 {
		t.Fatalf("resume re-run exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	assertStamped("resume re-run", readRows("resume re-run"))
}

// --- the partial-failure classes (10.2 exit 10) -----------------------------

// A source mutated between scan and copy: the item is skipped with a
// warning (5.1 step 1), the run COMPLETES the rest, and the exit code
// is the partial-failure class (10). The changed file never reaches
// the destination.
func TestCopySourceChangedSkipExits10(t *testing.T) {
	root := t.TempDir()
	cat := filepath.Join(t.TempDir(), "catalog.sqlite")
	dest := filepath.Join(t.TempDir(), "dest")
	setID := "S-CHG"
	writeFileP(t, filepath.Join(root, "stable.txt"), pattern(7, 512), 0o644)
	writeFileP(t, filepath.Join(root, "volatile.txt"), pattern(8, 512), 0o644)
	if _, stderr, code := runScanChildForCopy(t, "--catalog", cat, "--set-id", setID, root); code != 0 {
		t.Fatalf("scan exit = %d (stderr: %s)", code, stderr)
	}
	planSynthetic(t, cat, setID, copySyntheticUsable)

	// Mutate AFTER the plan: new content, new size, new mtime - every
	// field of the fixed 5.1 comparison set fires.
	time.Sleep(10 * time.Millisecond)
	writeFileP(t, filepath.Join(root, "volatile.txt"), pattern(9, 600), 0o644)

	stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json")
	if code != 10 {
		t.Fatalf("copy exit = %d, want 10 (partial failure class; stderr: %s)\nstdout: %s", code, stderr, stdout)
	}
	var rep copyReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Skipped != 1 || rep.Copied != 1 || rep.Failed != 0 {
		t.Fatalf("report = %+v, want 1 skip (volatile) + 1 copy (stable)", rep)
	}
	if len(rep.Failures) != 1 || !strings.Contains(rep.Failures[0], "volatile.txt") ||
		!strings.Contains(rep.Failures[0], "skipped") {
		t.Fatalf("failures = %v, want the volatile.txt skip line", rep.Failures)
	}
	if rep.Status != "partial" {
		t.Fatalf("status = %q, want partial", rep.Status)
	}
	// The stable file copied; the changed one never reached media.
	assertBytes(t, filepath.Join(dest, setID+"-001", "files", "stable.txt"), pattern(7, 512))
	if _, err := os.Lstat(filepath.Join(dest, setID+"-001", "files", "volatile.txt")); !os.IsNotExist(err) {
		t.Fatalf("the source-changed file was written anyway (err=%v)", err)
	}
}

// The T3.1 UTF-8 writer gate, fed through the REAL pipeline: the
// walker preserves the raw-byte filename (its pinned contract), the
// plan places it, and the copy run warn-and-skips it (exit-10 class)
// WITHOUT writing anything - while the rest of the image completes.
func TestCopyUtf8RowWarnAndSkips(t *testing.T) {
	root := t.TempDir()
	cat := filepath.Join(t.TempDir(), "catalog.sqlite")
	dest := filepath.Join(t.TempDir(), "dest")
	setID := "S-UTF8"
	writeFileP(t, filepath.Join(root, "good.txt"), pattern(10, 256), 0o644)
	rawName := []byte("bad-\xff.txt") // invalid UTF-8 on purpose (latin-1 0xff)
	writeFileP(t, filepath.Join(root, string(rawName)), pattern(11, 256), 0o644)
	if _, stderr, code := runScanChildForCopy(t, "--catalog", cat, "--set-id", setID, root); code != 0 {
		t.Fatalf("scan exit = %d (stderr: %s)", code, stderr)
	}
	planSynthetic(t, cat, setID, copySyntheticUsable)

	stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json")
	if code != 10 {
		t.Fatalf("copy exit = %d, want 10 (stderr: %s)\nstdout: %s", code, stderr, stdout)
	}
	var rep copyReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Skipped != 1 || rep.Copied != 1 {
		t.Fatalf("report = %+v, want 1 UTF-8 skip + 1 copy", rep)
	}
	if len(rep.Failures) != 1 || !strings.Contains(rep.Failures[0], "not valid UTF-8") {
		t.Fatalf("failures = %v, want the UTF-8 refusal naming the fidelity reason", rep.Failures)
	}
	if !strings.Contains(stderr, "not valid UTF-8") {
		t.Fatalf("stderr lacks the UTF-8 warning: %s", stderr)
	}
	// The raw-byte file is NOT written anywhere under the image tree.
	filesRoot := filepath.Join(dest, setID+"-001", "files")
	assertBytes(t, filepath.Join(filesRoot, "good.txt"), pattern(10, 256))
	var leaked []string
	_ = filepath.Walk(filesRoot, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.Contains(path, "bad-") {
			leaked = append(leaked, path)
		}
		return nil
	})
	if len(leaked) != 0 {
		t.Fatalf("the non-UTF-8 row leaked onto media: %v", leaked)
	}
	// The run row is finalized 'partial' (the exit-10 class).
	db := catOpen(t, cat)
	var status string
	if err := db.QueryRow(`SELECT status FROM runs WHERE kind = 'copy'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "partial" {
		t.Fatalf("copy run status = %q, want partial", status)
	}
}

// --- 12.7 crash/resume (SIGKILL re-exec child) ------------------------------

// copyCrashFixture builds a 3-file single-image set and returns the
// paths the crash tests need.
type copyCrashFixture struct {
	root, cat, dest, setID string
	files                  []string // relpaths, in planned file_id order
}

func newCopyCrashFixture(t *testing.T) copyCrashFixture {
	t.Helper()
	f := copyCrashFixture{
		root:  t.TempDir(),
		cat:   filepath.Join(t.TempDir(), "catalog.sqlite"),
		dest:  filepath.Join(t.TempDir(), "dest"),
		setID: "S-CRASH",
	}
	for i, name := range []string{"f1.bin", "f2.bin", "f3.bin"} {
		writeFileP(t, filepath.Join(f.root, name), pattern(byte(20+i), 4096), 0o644)
		f.files = append(f.files, name)
	}
	if _, stderr, code := runScanChildForCopy(t, "--catalog", f.cat, "--set-id", f.setID, f.root); code != 0 {
		t.Fatalf("scan exit = %d (stderr: %s)", code, stderr)
	}
	planSynthetic(t, f.cat, f.setID, copySyntheticUsable)
	return f
}

// plannedOrder returns the set's file relpaths in WRITING order
// (file_id order - the writer's iteration order). The scan inserts
// rows in hash-completion order (the 3.3 pool), so the file_id order
// is NOT the lexical order and the crash tests key off THIS.
func plannedOrder(t *testing.T, catPath, setID string) []string {
	t.Helper()
	db := catOpen(t, catPath)
	rows, err := db.Query(`SELECT relpath FROM files WHERE set_id = ? ORDER BY file_id`, setID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var rel string
		if err := rows.Scan(&rel); err != nil {
			t.Fatal(err)
		}
		out = append(out, rel)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(out) != 3 {
		t.Fatalf("planned order = %v, want the 3 fixture files", out)
	}
	return out
}

// copiedFlags reads the per-file copied flags of the set (keyed by
// relpath) for the crash assertions.
func copiedFlags(t *testing.T, catPath, setID string) map[string]bool {
	t.Helper()
	db := catOpen(t, catPath)
	rows, err := db.Query(`SELECT relpath, copied FROM files WHERE set_id = ?`, setID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var rel string
		var copied bool
		if err := rows.Scan(&rel, &copied); err != nil {
			t.Fatal(err)
		}
		out[rel] = copied
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// THE 12.7 CRASH: a re-exec child SIGKILLs itself AT the fail-after-N
// hook (mid-item, after the .partial write, before the rename). The
// parent asserts the crash window exactly: only the in-flight item's
// .partial remains, every completed flag is verified-true, and the
// RESUMED run copies exactly the unfinished item - then the tree
// byte-matches the source and a further re-run copies nothing.
func TestCopyCrashSIGKILLResumesExactlyUnfinished(t *testing.T) {
	f := newCopyCrashFixture(t)
	hookEnv := []string{
		"ISOPRESS_TEST_COPY_FAIL_AFTER=2",
		"ISOPRESS_TEST_COPY_FAIL_KIND=partial",
		"ISOPRESS_TEST_COPY_FAIL_MODE=kill",
	}
	cmd := exec.Command(os.Args[0],
		"copy", "--catalog", f.cat, "--set-id", f.setID, "--dest", f.dest)
	cmd.Env = append(append(os.Environ(), cliHelperEnv+"=1"), hookEnv...)
	err := cmd.Run()
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("crash child did not die by signal: %v", err)
	}
	if exitErr.ExitCode() != -1 {
		t.Fatalf("crash child exit = %d, want -1 (SIGKILL)", exitErr.ExitCode())
	}

	// THE CRASH WINDOW: the hook fires on the N-th payload write
	// (FAIL_AFTER=2 = the SECOND written item), so exactly one
	// .partial remains - the second item's; the first is final and
	// marked, the third never started. The scan's hash pool makes the
	// file_id (= writing) order non-lexical, so the order is read
	// from the catalog (plannedOrder).
	order := plannedOrder(t, f.cat, f.setID)
	first, second, third := order[0], order[1], order[2]
	partials, err := filepath.Glob(filepath.Join(f.dest, "*", "files", "*.partial"))
	if err != nil {
		t.Fatal(err)
	}
	if len(partials) != 1 || !strings.HasSuffix(partials[0], second+".partial") {
		t.Fatalf("partials after the crash = %v, want exactly %s.partial (the in-flight item)", partials, second)
	}
	if _, err := os.Lstat(filepath.Join(f.dest, f.setID+"-001", "files", second)); !os.IsNotExist(err) {
		t.Fatalf("the in-flight item published a final file before the crash (err=%v)", err)
	}
	if _, err := os.Lstat(filepath.Join(f.dest, f.setID+"-001", "files", third)); !os.IsNotExist(err) {
		t.Fatalf("an item AFTER the crash point exists (err=%v)", err)
	}
	flags := copiedFlags(t, f.cat, f.setID)
	if !flags[first] {
		t.Fatalf("the completed item %s lost its verified-true flag: %v", first, flags)
	}
	if flags[second] || flags[third] {
		t.Fatalf("an uncompleted item carries a copied flag despite the crash: %v", flags)
	}

	// THE RESUME: a plain run copies exactly the unfinished item.
	stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", f.cat, "--set-id", f.setID, "--dest", f.dest, "--json")
	if code != 0 {
		t.Fatalf("resume exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var rep copyReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Copied != 2 || rep.AlreadyCopied != 1 {
		t.Fatalf("resume report = %+v, want exactly 2 copied (f2, f3) + 1 already present", rep)
	}
	// No partial survives the resume, every flag is set, and the tree
	// byte-matches the source (sha256 for one file, per the brief).
	partials, _ = filepath.Glob(filepath.Join(f.dest, "*", "files", "*.partial"))
	if len(partials) != 0 {
		t.Fatalf("partials survived the resume: %v", partials)
	}
	flags = copiedFlags(t, f.cat, f.setID)
	seeds := map[string]byte{"f1.bin": 20, "f2.bin": 21, "f3.bin": 22}
	for _, name := range f.files {
		if !flags[name] {
			t.Fatalf("flag of %s = false after the resume: %v", name, flags)
		}
		assertBytes(t, filepath.Join(f.dest, f.setID+"-001", "files", name), pattern(seeds[name], 4096))
	}
	// The brief's digest-form check on one file (sha256 over the
	// published bytes vs the source pattern).
	gotSum := copySha256Hex(mustRead(t, filepath.Join(f.dest, f.setID+"-001", "files", second)))
	if wantSum := copySha256Hex(pattern(seeds[second], 4096)); gotSum != wantSum {
		t.Fatalf("%s sha256 = %s, want %s (hash check via the digest form)", second, gotSum, wantSum)
	}
	// A further re-run copies NOTHING (the trust model).
	stdout, _, code = runCopyChild(t, nil,
		"copy", "--catalog", f.cat, "--set-id", f.setID, "--dest", f.dest, "--json")
	if code != 0 {
		t.Fatalf("second resume exit = %d, want 0", code)
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Copied != 0 || rep.AlreadyCopied != 3 {
		t.Fatalf("second resume report = %+v, want 0 copied + 3 already present", rep)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// --- the interrupted-run deferral closure (SIGINT/SIGTERM, exit 130) --------

// ONE real signal (SIGINT AND SIGTERM - 10.1 registers both on the one
// channel and maps both to the SAME 130 class with IDENTICAL
// finalization; the T6.1 sweep pins BOTH shapes, not just SIGINT) to a
// real child mid-copy (the block-mode hook parks the run
// deterministically at the third item's write): the run aborts
// gracefully with exit 130, THE RUN ROW IS FINALIZED 'interrupted'
// (the T0.5/T1.6 deferral closure, empirically-confirmed gap CLOSED
// for copy), the in-flight item carries NO copied flag, the summary
// and report still print, and a resumed run completes the remainder
// without re-copying anything the interrupted run verified.
func TestCopySigintFinalizesInterruptedRun(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			runCopyInterruptedRunCase(t, sig)
		})
	}
}

func runCopyInterruptedRunCase(t *testing.T, sig syscall.Signal) {
	t.Helper()
	f := newCopyCrashFixture(t)
	hookEnv := []string{
		"ISOPRESS_TEST_COPY_FAIL_AFTER=3",
		"ISOPRESS_TEST_COPY_FAIL_KIND=partial",
		"ISOPRESS_TEST_COPY_FAIL_MODE=block",
	}
	// The scan signal helper is COMMAND-AGNOSTIC (production signal
	// scaffolding around the real command tree, args passed through):
	// the copy SIGINT e2e reuses it.
	cmd := exec.Command(os.Args[0],
		"copy", "--catalog", f.cat, "--set-id", f.setID, "--dest", f.dest)
	cmd.Env = append(append(os.Environ(), scanSignalHelperEnv+"=1"), hookEnv...)
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout := &bytes.Buffer{}
	cmd.Stdout = stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := false
	defer func() {
		if !exited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	// Wait for the deterministic mid-copy sentinel, then ONE signal.
	lineCh := make(chan string, 16)
	go func() {
		defer close(lineCh)
		buf := make([]byte, 1)
		var line []byte
		for {
			n, rerr := stderrPipe.Read(buf)
			if n > 0 {
				if buf[0] == '\n' {
					lineCh <- string(line)
					line = line[:0]
				} else {
					line = append(line, buf[0])
				}
			}
			if rerr != nil {
				if len(line) > 0 {
					lineCh <- string(line)
				}
				return
			}
		}
	}()
	var stderrAll strings.Builder
	signaled := false
	deadline := time.After(2 * time.Minute)
interruptLoop:
	for {
		select {
		case line, ok := <-lineCh:
			if !ok {
				break interruptLoop
			}
			stderrAll.WriteString(line)
			stderrAll.WriteByte('\n')
			if !signaled && strings.Contains(line, "COPY-HOOK-REACHED") {
				signaled = true
				if err := cmd.Process.Signal(sig); err != nil {
					t.Errorf("%v to copy child: %v", sig, err)
					break interruptLoop
				}
			}
		case <-deadline:
			break interruptLoop
		}
	}
	if !signaled {
		t.Fatalf("never saw the mid-copy sentinel; stderr so far:\n%s", stderrAll.String())
	}
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	var waitErr error
	select {
	case waitErr = <-waitCh:
		exited = true
	case <-time.After(2 * time.Minute):
		t.Fatalf("copy child did not exit within 2 minutes of the %v (stderr: %s)", sig, stderrAll.String())
	}
	for line := range lineCh {
		stderrAll.WriteString(line)
		stderrAll.WriteByte('\n')
	}
	errOut := stderrAll.String()

	exitErr, ok := waitErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("copy child did not exit with an error after %v: %v (stderr: %s)", sig, waitErr, errOut)
	}
	if got := exitErr.ExitCode(); got != 130 {
		t.Errorf("exit code after %v = %d, want 130 (stderr: %s)", sig, got, errOut)
	}
	if !strings.Contains(errOut, "interrupted by signal") {
		t.Errorf("stderr lacks the interruption message: %q", errOut)
	}
	if strings.Contains(errOut, "forcing exit") {
		t.Errorf("a SINGLE %v wrongly triggered the second-signal hard escape: %q", sig, errOut)
	}
	// The interrupted run still prints its report on stdout (FR-046:
	// the result is the stdout artifact even on the abort path).
	if !strings.Contains(stdout.String(), "copy: set "+f.setID) {
		t.Errorf("stdout lacks the interrupted run's report header: %q", stdout.String())
	}

	// THE DEFERRAL CLOSURE: the run row says 'interrupted' - not the
	// 'running' leftover that used to be the empirically-confirmed
	// gap (the post-cancel UPDATE runs on a fresh context, catalog
	// FinishRunDetached).
	db := catOpen(t, f.cat)
	var status string
	if err := db.QueryRow(`SELECT status FROM runs WHERE kind = 'copy'`).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "interrupted" {
		t.Fatalf("copy run status after %v = %q, want interrupted", sig, status)
	}
	// The catalog is consistent: the two COMPLETED writes (the block
	// hook parks the THIRD item's write) are marked; the blocked item
	// is not. The writing order is read from the catalog (the scan
	// pool makes file_id order non-lexical).
	order := plannedOrder(t, f.cat, f.setID)
	flags := copiedFlags(t, f.cat, f.setID)
	if !flags[order[0]] || !flags[order[1]] || flags[order[2]] {
		t.Fatalf("flags after the interruption = %v, want %s/%s marked, %s unmarked (order %v)",
			flags, order[0], order[1], order[2], order)
	}

	// The resumed run completes the remainder.
	stdout2, stderr2, code := runCopyChild(t, nil,
		"copy", "--catalog", f.cat, "--set-id", f.setID, "--dest", f.dest, "--json")
	if code != 0 {
		t.Fatalf("resume after %v exit = %d, want 0 (stderr: %s)", sig, code, stderr2)
	}
	var rep copyReport
	if err := json.Unmarshal([]byte(stdout2), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Copied != 1 || rep.AlreadyCopied != 2 {
		t.Fatalf("resume report = %+v, want 1 copied + 2 already present", rep)
	}
	seeds := map[string]byte{"f1.bin": 20, "f2.bin": 21, "f3.bin": 22}
	assertBytes(t, filepath.Join(f.dest, f.setID+"-001", "files", order[2]), pattern(seeds[order[2]], 4096))

	// THE NO-RE-COPY PIN (T6.1 resume semantics, strengthened): a third
	// run over the now-complete set copies NOTHING - every chunk the
	// interrupted run and the resume verified stays trusted (the mark
	// is the promise; a re-copy would mean the marks lied).
	stdout3, stderr3, code := runCopyChild(t, nil,
		"copy", "--catalog", f.cat, "--set-id", f.setID, "--dest", f.dest, "--json")
	if code != 0 {
		t.Fatalf("post-resume run exit = %d, want 0 (stderr: %s)", code, stderr3)
	}
	var rep3 copyReport
	if err := json.Unmarshal([]byte(stdout3), &rep3); err != nil {
		t.Fatal(err)
	}
	if rep3.Copied != 0 || rep3.AlreadyCopied != 3 {
		t.Fatalf("post-resume report = %+v, want 0 copied + 3 already present (verified chunks must never be re-copied)", rep3)
	}
	// Final catalog consistency: every file of the set is flagged
	// copied after the interrupted+resumed pair.
	for rel, copied := range copiedFlags(t, f.cat, f.setID) {
		if !copied {
			t.Errorf("file %s unmarked after interrupt+resume+steady-state; the catalog must end fully consistent", rel)
		}
	}
}

// --- selection and usage (10.1/10.2) ----------------------------------------

// --image writes only the requested image; an unplanned --image is a
// usage input (exit 2), and missing required flags are usage errors.
func TestCopyImageSubsetAndUsage(t *testing.T) {
	f := newCopyCrashFixture(t)
	dest := filepath.Join(t.TempDir(), "dest-one")
	stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", f.cat, "--set-id", f.setID, "--dest", dest, "--image", "1", "--json")
	if code != 0 {
		t.Fatalf("--image 1 exit = %d, want 0 (stderr: %s)", code, stderr)
	}
	var rep copyReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatal(err)
	}
	if len(rep.Images) != 1 || rep.Images[0].Image != 1 {
		t.Fatalf("report images = %+v, want only image 1", rep.Images)
	}

	// An unplanned image number is a usage input, not a silent no-op.
	_, stderr, code = runCopyChild(t, nil,
		"copy", "--catalog", f.cat, "--set-id", f.setID, "--dest", dest, "--image", "99")
	if code != 2 {
		t.Fatalf("--image 99 exit = %d, want 2 (usage; stderr: %s)", code, stderr)
	}
	if !strings.Contains(stderr, "not a planned image") {
		t.Fatalf("stderr lacks the planned-images hint: %s", stderr)
	}

	// Missing required flags are usage errors (exit 2).
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no --catalog", []string{"copy", "--set-id", f.setID, "--dest", dest}},
		{"no --set-id", []string{"copy", "--catalog", f.cat, "--dest", dest}},
		{"no --dest", []string{"copy", "--catalog", f.cat, "--set-id", f.setID}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, code := runCopyChild(t, nil, tc.args...)
			if code != 2 {
				t.Fatalf("exit = %d, want 2 (stderr: %s)", code, stderr)
			}
		})
	}

	// A set that was never scanned, and one that was never planned,
	// are usage inputs too (exit 2) with actionable messages.
	stderrPath := filepath.Join(t.TempDir(), "empty.sqlite")
	if cerr := catalog.Create(stderrPath); cerr != nil {
		t.Fatal(cerr)
	}
	_, stderr, code = runCopyChild(t, nil,
		"copy", "--catalog", stderrPath, "--set-id", "S-NOPE", "--dest", dest)
	if code != 2 || !strings.Contains(stderr, "no scanned set") {
		t.Fatalf("unscanned set exit = %d (want 2), stderr: %s", code, stderr)
	}
	catWithSet := filepath.Join(t.TempDir(), "scanned.sqlite")
	if cerr := catalog.Create(catWithSet); cerr != nil {
		t.Fatal(cerr)
	}
	db, oerr := catalog.Open(catWithSet, catalog.OpenReadWrite)
	if oerr != nil {
		t.Fatal(oerr)
	}
	if eerr := db.EnsureSet(context.Background(), "S-SET", "/somewhere", time.Now().UnixNano(), catalog.ToolVersion); eerr != nil {
		t.Fatal(eerr)
	}
	// Seed a scan run so the set counts as scanned but has no plan.
	if _, eerr := db.StartScanRun(context.Background(), catalog.ScanRun{
		SetID: "S-SET", Kind: "ingest", Root: "/somewhere", StartedAt: time.Now().UnixNano(),
	}); eerr != nil {
		t.Fatal(eerr)
	}
	if cerr := db.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	_, stderr, code = runCopyChild(t, nil,
		"copy", "--catalog", catWithSet, "--set-id", "S-SET", "--dest", dest)
	if code != 2 || !strings.Contains(stderr, "no planned images") {
		t.Fatalf("unplanned set exit = %d (want 2), stderr: %s", code, stderr)
	}
}
