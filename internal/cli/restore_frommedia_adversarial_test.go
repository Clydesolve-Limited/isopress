package cli

// T5.5 ADVERSARIAL QA at the COMMAND seat: the media-alone restore is the
// LAST-RESORT path (DESIGN_SPEC 8.5) - the whole point is surviving TOTAL
// CATALOG LOSS - so every test here drives the REAL command against REAL
// copy-set media produced by the real scan/plan/copy pipeline, with the
// original catalog DELETED.
//
// The matrix this file closes (items the acceptance file
// restore_frommedia_test.go did not cover):
//
//   - DELETED-CATALOG COMPLETENESS: every member kind the fixtures can
//     plant (a chunked parent SPANNING both images, a hard-link family,
//     a valid AND a dangling symlink, empty dirs incl. a sticky one,
//     setuid 04755 / setgid 02664, a zero-length file), byte-exact with
//     metadata, NO stray names, and the media dir byte-identical after -
//     export included, zero sidecars;
//   - THE NULL-CHUNK-HASH FILL: first-run media (export chunks hash=NULL,
//     manifest carries the read-back-verified hashes) must restore, the
//     RESUME-run precedence must be EXPORT-WINS in both directions, and
//     no hash anywhere must fail honestly (exit 10, the engine's per-chunk
//     reason - never a panic, never silent success);
//   - THE ANCHOR TRICHOTOMY through the command: self-row ABSENT -> normal
//     and silent; MATCH -> verified and silent; MISMATCH -> LOUD stderr
//     warning, NOT fatal (the restore completes);
//   - THE GATES: a past-version (user_version=0, schemaless) export named
//     as the export fails with the CATALOG class (exit 4, catalog-class
//     reads of the copy) and ZERO target writes; a structurally
//     damaged export is caught by the PRAGMA integrity_check gate; an
//     export holding TWO sets rows is KindCorrupt (exit 4); a media dir
//     with a manifest but NO export fails before any write;
//   - SUPPLY PRECEDENCE through the command: the media tree beats a
//     --copy-tree-root that also offers the same image, and a media MISS
//     falls through to the supplement;
//   - THE TEMP-COPY DISCIPLINE: the scratch lands under --work-dir (never
//     on the media) and is removed on success, on the corrupt-export
//     failure, and on CANCELLATION (SIGINT, exit 130).
//
// THE SUPPLY-FOLLOWING FILL (flipped in the T5.5 fix round - see
// TestRestoreFromMediaCopyTreeRootSuppliedSpanFailsWithoutManifestFillKnownGap,
// kept under its QA pin name): the manifest chunk-hash fill follows a
// --copy-tree-root-supplied tree - it consults the sidecar MANIFEST.json
// of whatever SourceCopyTree directory the supply pass resolved - so a
// first-run copy-set media whose chunked span is supplied that way
// restores byte-exact instead of failing honestly (exit 10).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"isopress/internal/catalog"
	"isopress/internal/media"
	"isopress/internal/report"
	"isopress/internal/restore"
)

// --- test-side media helpers -------------------------------------------------

// editMediaExport runs a TEST-SIDE mutation of the media's own export (the
// fixture is ours; these edits plant the adversarial states) and strips any
// SQLite sidecar the write itself left behind, so every later read-only
// assertion measures the TOOL's behavior, not this test's writes.
func editMediaExport(t *testing.T, exportPath string, fn func(t *testing.T, db *sql.DB)) {
	t.Helper()
	db, err := sql.Open("sqlite", exportPath)
	if err != nil {
		t.Fatalf("opening %s for a test-side edit: %v", exportPath, err)
	}
	fn(t, db)
	if err := db.Close(); err != nil {
		t.Fatalf("closing the edited export: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		_ = os.Remove(exportPath + suffix)
	}
}

// withMediaExportCopy inspects the media's export through a byte COPY in a
// temp dir: opening the media's own file read-only could still materialize
// -wal/-shm ON THE MEDIA (the exact hazard 8.5's temp-copy exists to avoid),
// so a test that only wants to READ the export must not become the one
// writer the assertions forbid.
func withMediaExportCopy(t *testing.T, exportPath string, fn func(db *sql.DB)) {
	t.Helper()
	blob, err := os.ReadFile(exportPath)
	if err != nil {
		t.Fatalf("reading %s: %v", exportPath, err)
	}
	tmp := filepath.Join(t.TempDir(), "export-inspect.sqlite")
	if err := os.WriteFile(tmp, blob, 0o600); err != nil {
		t.Fatalf("writing the export inspection copy: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+tmp+"?mode=ro")
	if err != nil {
		t.Fatalf("opening the export inspection copy: %v", err)
	}
	defer db.Close() //nolint:errcheck // read-only inspection handle
	fn(db)
}

// exportChunkHashCounts returns (total, NULL) chunk rows of the export -
// the lazy-chunk-hash finding's measurable state (first-run: NULL == total;
// resume-run: NULL == 0).
func exportChunkHashCounts(t *testing.T, exportPath string) (total, nulls int) {
	t.Helper()
	withMediaExportCopy(t, exportPath, func(db *sql.DB) {
		if err := db.QueryRow("SELECT count(*) FROM chunks").Scan(&total); err != nil {
			t.Fatalf("counting the export's chunk rows: %v", err)
		}
		if err := db.QueryRow("SELECT count(*) FROM chunks WHERE hash IS NULL").Scan(&nulls); err != nil {
			t.Fatalf("counting the export's NULL chunk hashes: %v", err)
		}
	})
	return total, nulls
}

// exportSelfRowCount counts the export's 'manifest' self-rows for one image
// (6.6: the writer records MANIFEST.json's hash at write time - the anchor
// verifyMediaManifestAnchor reads).
func exportSelfRowCount(t *testing.T, exportPath, setID string, image int64) (n int) {
	t.Helper()
	withMediaExportCopy(t, exportPath, func(db *sql.DB) {
		err := db.QueryRow(
			`SELECT count(*) FROM image_contents WHERE set_id = ? AND image_id = ? AND kind = 'manifest' AND media_path = ?`,
			setID, image, media.ManifestFileName).Scan(&n)
		if err != nil {
			t.Fatalf("counting the export's manifest self-row: %v", err)
		}
	})
	return n
}

// plantManifestSelfRow writes (or overwrites) the export's manifest self-row
// for one image: hash == sha256 of the media's MANIFEST.json is the MATCH
// leg, any other 32 bytes is the MISMATCH leg, absence is the ABSENT leg
// (the natural first-run state, asserted rather than assumed).
func plantManifestSelfRow(t *testing.T, exportPath, setID string, image int64, hash []byte) {
	t.Helper()
	editMediaExport(t, exportPath, func(t *testing.T, db *sql.DB) {
		if _, err := db.Exec(
			`INSERT OR REPLACE INTO image_contents (set_id, image_id, media_path, kind, size, hash)
			 VALUES (?, ?, ?, 'manifest', ?, ?)`,
			setID, image, media.ManifestFileName, int64(len(hash)), hash); err != nil {
			t.Fatalf("planting the manifest self-row: %v", err)
		}
	})
}

// readMediaManifestFile parses one on-media MANIFEST.json.
func readMediaManifestFile(t *testing.T, path string) media.Manifest {
	t.Helper()
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	m, err := media.ParseManifest(blob)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return m
}

// writeMediaManifestFile re-serializes a (tampered) manifest onto the media.
func writeMediaManifestFile(t *testing.T, path string, m media.Manifest) {
	t.Helper()
	blob, err := media.MarshalManifest(m)
	if err != nil {
		t.Fatalf("marshaling %s: %v", path, err)
	}
	if err := os.WriteFile(path, blob, 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// setManifestChunkHashes rewrites EVERY chunk-part hash of every chunked
// parent in one image's manifest to hex (the "manifest disagrees" leg) or
// to "" (the "no anchor anywhere" leg). Returns the number of chunk parts
// touched so a test can assert the plant was not vacuous.
func setManifestChunkHashes(t *testing.T, manifestPath, hexHash string) (parts int) {
	t.Helper()
	m := readMediaManifestFile(t, manifestPath)
	for i := range m.ChunkedFiles {
		for j := range m.ChunkedFiles[i].Chunks {
			m.ChunkedFiles[i].Chunks[j].SHA256 = hexHash
			parts++
		}
	}
	writeMediaManifestFile(t, manifestPath, m)
	return parts
}

// exportUserVersion reads PRAGMA user_version of the media's export through
// a byte copy (see withMediaExportCopy for why a direct open is refused).
func exportUserVersion(t *testing.T, exportPath string) (v int) {
	t.Helper()
	withMediaExportCopy(t, exportPath, func(db *sql.DB) {
		if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
			t.Fatalf("reading the export's user_version: %v", err)
		}
	})
	return v
}

// --- THE DELETED-CATALOG E2E, full member spread -----------------------------

// fromMediaRichTwoImageFixture scans, plans, and copies a two-image set
// carrying every member kind the media-alone restore must reproduce:
// a chunked parent that SPANS both images, a hard-link family, a valid AND
// a dangling symlink, a zero-length file, an empty dir, a STICKY empty dir,
// and setuid/setgid files planted through chmod(2)'s raw mode (the T1.2
// lesson: Go's os package masks those bits otherwise).
func fromMediaRichTwoImageFixture(t *testing.T, setID string) (cat, dest, root string) {
	t.Helper()
	root = t.TempDir()
	writeFileP(t, filepath.Join(root, "big.bin"), pattern(1, copyBigChunkedLen), 0o644)
	writeFileP(t, filepath.Join(root, "small1.txt"), pattern(2, 1000), 0o644)
	writeFileP(t, filepath.Join(root, "zero.txt"), nil, 0o644)
	writeFileP(t, filepath.Join(root, "nested", "deep.txt"), pattern(3, 700), 0o644)
	writeFileP(t, filepath.Join(root, "suid.sh"), pattern(4, 64), 0o755)
	if err := os.Chmod(filepath.Join(root, "suid.sh"), os.FileMode(0o755)|os.ModeSetuid); err != nil {
		t.Fatalf("planting setuid: %v", err)
	}
	writeFileP(t, filepath.Join(root, "setgid.sh"), pattern(7, 64), 0o644)
	if err := os.Chmod(filepath.Join(root, "setgid.sh"), os.FileMode(0o664)|os.ModeSetgid); err != nil {
		t.Fatalf("planting setgid: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "sticky-dir"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "sticky-dir"), os.FileMode(0o777)|os.ModeSticky); err != nil {
		t.Fatalf("planting sticky: %v", err)
	}
	writeFileP(t, filepath.Join(root, "fam-a.txt"), pattern(5, 800), 0o644)
	if err := os.Link(filepath.Join(root, "fam-a.txt"), filepath.Join(root, "fam-b.txt")); err != nil {
		t.Fatalf("hard-link family: %v", err)
	}
	if err := os.Symlink("small1.txt", filepath.Join(root, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.Symlink("nowhere.txt", filepath.Join(root, "dangling")); err != nil {
		t.Fatalf("dangling symlink: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "empty-dir"), 0o750); err != nil {
		t.Fatalf("empty dir: %v", err)
	}
	// Verify the plants on the SOURCE: a silently dropped special bit would
	// otherwise show up as a vacuous source/target comparison.
	assertRawPOSIXMode(t, filepath.Join(root, "suid.sh"), 0o4755)
	assertRawPOSIXMode(t, filepath.Join(root, "setgid.sh"), 0o2664)
	assertRawPOSIXMode(t, filepath.Join(root, "sticky-dir"), 0o1777)

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

// TestRestoreFromMediaDeletedCatalogFullMemberSpread is the completeness
// half of THE T5.5 acceptance criterion: the same decisive deleted-catalog
// round trip, but over the FULL member spread, with the span itself asserted
// from the media (not assumed) and the media dir compared member-for-member
// before and after.
func TestRestoreFromMediaDeletedCatalogFullMemberSpread(t *testing.T) {
	const setID = "S-FMRICH"
	cat, dest, root := fromMediaRichTwoImageFixture(t, setID)

	// THE SPAN PROOF: BOTH images' manifests must carry big.bin's chunk
	// table - a chunked parent spanning two images is what FR-033's
	// media-alone reassembly record exists for.
	for _, img := range []int64{1, 2} {
		m := readMediaManifestFile(t, filepath.Join(destImageDir(dest, setID, img), media.ManifestFileName))
		found := false
		for _, cf := range m.ChunkedFiles {
			if cf.RelPath == "big.bin" {
				found = true
				if len(cf.Chunks) == 0 {
					t.Errorf("image %d's manifest carries an EMPTY chunk table for big.bin", img)
				}
			}
		}
		if !found {
			t.Errorf("image %d's MANIFEST.json carries no chunk table for big.bin (the span is not recorded)", img)
		}
	}

	// THE FINDING'S PREMISE, asserted: this is FIRST-RUN media, so the
	// export embedded at RUN START carries chunks rows with hash=NULL. The
	// restore below can therefore only succeed if mediaStore's manifest
	// fill fires (the export alone cannot run 8.4's first verification).
	leadExport := filepath.Join(destImageDir(dest, setID, 1), media.ExportFileName)
	total, nulls := exportChunkHashCounts(t, leadExport)
	if total == 0 || nulls != total {
		t.Fatalf("first-run export chunk hashes = %d NULL of %d, want every row NULL (the fill's premise)", nulls, total)
	}

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
	if strings.Contains(stderr, "warning:") {
		t.Errorf("first-run media must restore WITHOUT an anchor warning:\n%s", stderr)
	}

	// THE REAL ROUND TRIP over every member kind: byte-exact content,
	// raw POSIX modes (setuid/setgid/sticky), uid/gid, nanosecond mtimes,
	// the hard-link family's nlink, both symlinks, both empty dirs, the
	// zero-length file - and NO stray name in the reverse direction
	// (assertSameRestoredTree checks both directions).
	assertSameRestoredTree(t, root, target)
	for _, tc := range []struct {
		rel  string
		want uint32
	}{
		{"suid.sh", 0o4755},
		{"setgid.sh", 0o2664},
		{"sticky-dir", 0o1777},
	} {
		assertRawPOSIXMode(t, filepath.Join(target, tc.rel), tc.want)
	}
	if fi, err := os.Lstat(filepath.Join(target, "zero.txt")); err != nil {
		t.Errorf("zero.txt missing from the target: %v", err)
	} else if fi.Size() != 0 {
		t.Errorf("zero.txt size = %d, want 0", fi.Size())
	}
	// The dangling symlink really is dangling and carries its target.
	if got, err := os.Readlink(filepath.Join(target, "dangling")); err != nil || got != "nowhere.txt" {
		t.Errorf("dangling -> %q (%v), want a faithful dangling link to nowhere.txt", got, err)
	}
	// Count-once report: the chunked parent spanning both images counts once.
	if n, ok := restoredTotal(t, stdout); !ok || n != countRestoreMembers(t, root) {
		t.Errorf("report restored total = %d (parsed=%v), want %d\nstdout: %s",
			n, ok, countRestoreMembers(t, root), stdout)
	}

	// THE MEDIA READ-ONLY PROOF over the WHOLE directory: every member
	// keeps bytes/mode/mtime, nothing was added (the temp copy would show
	// up here), no SQLite sidecar exists anywhere - the export included.
	assertTreeUnchanged(t, before, dest)
}

// --- THE ANCHOR TRICHOTOMY through the command --------------------------------

// TestRestoreFromMediaManifestAnchorTrichotomy drives all three documented
// outcomes of 8.5's manifest-anchor step over ONE fixture:
//
//	ABSENT  (first-run media: the run-start export predates the manifest
//	         write) -> normal, NO warning;
//	MATCH   (the export recorded THIS manifest's hash) -> verified,
//	         silently (no warning on the success path);
//	MISMATCH (the export's self-row attests other bytes - the expected
//	         state of resumed media and equally the signature of tampering)
//	         -> LOUD warning on STDERR, but NOT fatal: the restore completes
//	         and the report on stdout is the normal successful one (FR-041's
//	         per-member hashes remain the byte-level arbiter).
func TestRestoreFromMediaManifestAnchorTrichotomy(t *testing.T) {
	const setID = "S-FMANCH"
	cat, dest, root := restoreTwoImageFixture(t, setID)
	if err := os.Remove(cat); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog: %v", err)
	}
	leadDir := destImageDir(dest, setID, 1)
	manPath := filepath.Join(leadDir, media.ManifestFileName)
	exportPath := filepath.Join(leadDir, media.ExportFileName)
	manBlob, err := os.ReadFile(manPath)
	if err != nil {
		t.Fatalf("reading the lead manifest: %v", err)
	}
	manSum := sha256.Sum256(manBlob)
	anchorPhrase := "does not match the export's recorded manifest self-row"

	type leg struct {
		name     string
		plant    []byte // nil = leave the export's row as it is
		wantWarn bool
	}
	// The stale self-row bytes of the mismatch leg: 32 bytes that cannot be
	// this manifest's hash.
	stale := []byte{0xDE, 0xAD, 0xBE, 0xEF, 0x01, 0x02, 0x03, 0x04}
	for len(stale) < 32 {
		stale = append(stale, byte(len(stale)))
	}
	legs := []leg{
		{name: "absent", plant: nil, wantWarn: false},
		{name: "match", plant: manSum[:], wantWarn: false},
		{name: "mismatch", plant: stale, wantWarn: true},
	}

	// The ABSENT leg's premise, asserted before the first run: a first-run
	// export carries NO manifest self-row at all.
	if n := exportSelfRowCount(t, exportPath, setID, 1); n != 0 {
		t.Fatalf("fixture anomaly: first-run export already holds %d manifest self-row(s)", n)
	}

	for _, lc := range legs {
		t.Run(lc.name, func(t *testing.T) {
			if lc.plant != nil {
				plantManifestSelfRow(t, exportPath, setID, 1, lc.plant)
			}
			before := snapshotTree(t, dest)
			target := restoreTarget(t)
			stdout, stderr, code := runInProcess(t, "restore",
				"--from-media", dest, "--target", target, "--work-dir", tempWorkDir(t))
			if code != report.ExitOK {
				t.Fatalf("exit = %d, want 0 (the anchor step must never fail the run)\nstdout: %s\nstderr: %s",
					code, stdout, stderr)
			}
			if got := strings.Contains(stderr, anchorPhrase); got != lc.wantWarn {
				t.Errorf("anchor warning on stderr = %t, want %t\nstderr: %s", got, lc.wantWarn, stderr)
			}
			if lc.wantWarn {
				// LOUD and COMPLETE: the warning names the manifest and both
				// hashes (the export's recorded one is what the user needs to
				// compare against).
				if !strings.Contains(stderr, manPath) {
					t.Errorf("the warning must name the manifest:\n%s", stderr)
				}
				if !strings.Contains(stderr, hex.EncodeToString(stale)) {
					t.Errorf("the warning must print the export's recorded hash %s:\n%s",
						hex.EncodeToString(stale), stderr)
				}
				// ... and it stays off stdout: the report is the user's data
				// channel (10.3), warnings ride stderr only.
				if strings.Contains(stdout, "warning:") {
					t.Errorf("the anchor warning leaked onto stdout:\n%s", stdout)
				}
			}
			// NOT FATAL in either direction: the report is the normal
			// success report and the whole tree came back.
			if !strings.Contains(stdout, "status: ok") {
				t.Errorf("stdout lacks the ok status (the run must complete):\n%s", stdout)
			}
			assertSameRestoredTree(t, root, target)
			assertTreeUnchanged(t, before, dest)
		})
	}
}

// TestRestoreFromMediaAnchorManifestVanishMapsToMediaClass closes the T5.5
// fold-in class-consistency gap at the anchor step: verifyMediaManifestAnchor's
// error used to bypass classifyRestoreError and land the generic fatal
// (exit 1), while the SAME error class returned one step earlier by
// openRestoreCatalog (a manifest/export set-id disagreement) mapped to the
// media class (exit 3). THE REACHABLE SHAPE it exists for is the resumed-media
// TOCTOU: a re-run copy/build embeds its RUN-START export carrying the
// PREVIOUS manifest's self-row, so the anchor step re-reads MANIFEST.json -
// and if that manifest vanishes between the quick-layer read (8.5 step 1,
// which answered from it) and the re-read, verifyMediaManifestAnchor returns
// a *restore.MediaIntegrityError. Both error ORIGINS must now map to the
// SAME class - the media class, exit 3 "supply other or repaired media" -
// and this test pins them SIDE BY SIDE through the command's own mapping
// (classifyRestoreError). The documented boundary is pinned too: the re-read
// only exists for the self-row shape - on FIRST-RUN media (no self-row) a
// vanished manifest is NOT an anchor failure (export integrity is the
// fallback anchor, no warning noise).
func TestRestoreFromMediaAnchorManifestVanishMapsToMediaClass(t *testing.T) {
	const setID = "S-FMANVAN"
	cat, dest, _ := restoreTwoImageFixture(t, setID)
	if err := os.Remove(cat); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog: %v", err)
	}
	leadDir := destImageDir(dest, setID, 1)
	exportPath := filepath.Join(leadDir, media.ExportFileName)
	manPath := filepath.Join(leadDir, media.ManifestFileName)

	// RESUMED-MEDIA SHAPE, planted: the export carries a manifest self-row
	// (recorded != nil - the ONLY shape in which the re-read runs at all;
	// asserted, so the leg below cannot go vacuous).
	stale := bytes.Repeat([]byte{0x5A}, 32)
	plantManifestSelfRow(t, exportPath, setID, 1, stale)

	// 8.5's own order: the quick layer answers FIRST, manifest still
	// present...
	info, err := restore.ResolveMedia(dest)
	if err != nil {
		t.Fatalf("ResolveMedia must answer while the manifest is present: %v", err)
	}
	// ...then the manifest vanishes before the anchor re-read (the TOCTOU).
	if err := os.Remove(manPath); err != nil {
		t.Fatalf("vanishing the manifest: %v", err)
	}

	// The flow's own discipline: the anchor runs over a temp COPY of the
	// export, never the media's file directly.
	blob, err := os.ReadFile(exportPath)
	if err != nil {
		t.Fatalf("reading the export: %v", err)
	}
	exportCopy := filepath.Join(t.TempDir(), media.ExportFileName)
	if err := os.WriteFile(exportCopy, blob, 0o600); err != nil {
		t.Fatalf("writing the export copy: %v", err)
	}
	db, err := catalog.Open(exportCopy, catalog.OpenReadOnly)
	if err != nil {
		t.Fatalf("opening the export copy: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	err = verifyMediaManifestAnchor(t.Context(), db, info, nil)
	var mit *restore.MediaIntegrityError
	if !errors.As(err, &mit) {
		t.Fatalf("anchor error = %v (%T), want a *restore.MediaIntegrityError (the self-row shape's re-read failure)", err, err)
	}
	if !strings.Contains(err.Error(), media.ManifestFileName) {
		t.Errorf("the anchor error must name the vanished manifest: %v", err)
	}

	// THE CLASS CONSISTENCY, both origins side by side through the
	// command's own mapping: media class, exit 3 - the same class
	// openRestoreCatalog's MediaIntegrityError already takes (the
	// root-mode tampered-manifest leg above pins that one end to end).
	root := NewRootCmd()
	cmd, _, ferr := root.Find([]string{"restore"})
	if ferr != nil {
		t.Fatalf("finding the restore command: %v", ferr)
	}
	if code := report.Classify(classifyRestoreError(cmd, err)); code != report.ExitDependency {
		t.Errorf("the anchor's MediaIntegrityError classifies to %d, want 3 (the media class, same as openRestoreCatalog's)", code)
	}
	early := &restore.MediaIntegrityError{Path: manPath, Msg: "manifest set id \"x\" disagrees with the export's set id \"y\""}
	if code := report.Classify(classifyRestoreError(cmd, early)); code != report.ExitDependency {
		t.Errorf("openRestoreCatalog's MediaIntegrityError classifies to %d, want 3 (the class both origins must share)", code)
	}

	// THE DOCUMENTED BOUNDARY: on first-run media (NO self-row recorded)
	// the re-read never runs, so a manifest that vanished after the quick
	// layer is NOT an anchor failure there - the class above is precisely
	// the resumed-media shape.
	t.Run("first-run media without a self-row is not an anchor failure", func(t *testing.T) {
		cat2, tree2, _ := fromMediaSingleImageFixture(t, "S-FMANVAN2")
		if err := os.Remove(cat2); err != nil {
			t.Fatalf("deleting the ORIGINAL catalog: %v", err)
		}
		info2, err := restore.ResolveMedia(tree2)
		if err != nil {
			t.Fatalf("ResolveMedia: %v", err)
		}
		blob2, err := os.ReadFile(info2.ExportPath)
		if err != nil {
			t.Fatalf("reading the export: %v", err)
		}
		copy2 := filepath.Join(t.TempDir(), media.ExportFileName)
		if err := os.WriteFile(copy2, blob2, 0o600); err != nil {
			t.Fatalf("writing the export copy: %v", err)
		}
		db2, err := catalog.Open(copy2, catalog.OpenReadOnly)
		if err != nil {
			t.Fatalf("opening the export copy: %v", err)
		}
		t.Cleanup(func() { _ = db2.Close() })
		if err := os.Remove(filepath.Join(info2.ManifestDir, media.ManifestFileName)); err != nil {
			t.Fatalf("vanishing the manifest: %v", err)
		}
		if err := verifyMediaManifestAnchor(t.Context(), db2, info2, nil); err != nil {
			t.Errorf("first-run media (no manifest self-row) must not fail the anchor on a vanished manifest: %v", err)
		}
	})
}

// --- THE NULL-CHUNK-HASH FILL: precedence pins --------------------------------

// TestRestoreFromMediaChunkHashPrecedenceExportWins pins the fill's
// precedence in the direction that matters, over RESUMED media (a second
// copy run over the same destination re-embeds the run-2 export, which
// snapshots run-1's STORED chunk hashes - the premise is asserted):
//
//	leg 0: resumed media with a NATURAL self-row mismatch -> loud warning,
//	       still exit 0 (the expected-on-resumed-media state);
//	leg A: export hash CORRECT, manifest hash DISAGREEING (planted) ->
//	       restore SUCCEEDS byte-exact -> THE EXPORT WINS;
//	leg B1: export hash WRONG (planted), manifest hash CORRECT -> the
//	        export's OWN internal anchor (image_contents' mirrored chunk
//	        hash vs chunks.hash, read by ImageContentList during the
//	        anchor step) refuses the file as a CORRUPT CATALOG, exit 4,
//	        before any target write - 8.5's "the export's internal hashes
//	        are the deep anchor" made literal;
//	leg B2: the same wrong export hash, but made CONSISTENT across the
//	        export's own two tables -> the gate passes and the fill still
//	        defers to the EXPORT: the engine's first-verification mismatch
//	        fires (exit 10, no final file). A manifest-wins fill would have
//	        made this leg succeed - THE EXPORT WINS even when wrong.
func TestRestoreFromMediaChunkHashPrecedenceExportWins(t *testing.T) {
	const setID = "S-FMPREC"
	cat, dest, root := restoreTwoImageFixture(t, setID)

	// RESUME RUN: same catalog, same destination - the re-embedded export
	// now carries the stored chunk hashes the first run wrote lazily.
	if stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest); code != 0 {
		t.Fatalf("resume copy exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if err := os.Remove(cat); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog: %v", err)
	}
	leadDir := destImageDir(dest, setID, 1)
	exportPath := filepath.Join(leadDir, media.ExportFileName)
	total, nulls := exportChunkHashCounts(t, exportPath)
	if total == 0 || nulls != 0 {
		t.Fatalf("resume-run export chunk hashes = %d NULL of %d, want a hash on every row (the export-wins premise)",
			nulls, total)
	}
	// The resume run's own MANIFEST bytes (correct chunk hashes), saved so
	// leg B can put the TRUTH back beside a lying export.
	pristine := map[int64][]byte{}
	for _, img := range []int64{1, 2} {
		blob, err := os.ReadFile(filepath.Join(destImageDir(dest, setID, img), media.ManifestFileName))
		if err != nil {
			t.Fatalf("reading image %d's manifest: %v", img, err)
		}
		pristine[img] = blob
	}

	// Leg 0: the NATURAL resumed-media state (the run-1 manifest self-row
	// attests the manifest the run-2 rewrite replaced) -> warning, not fatal.
	t.Run("resume self-row mismatch is loud and non-fatal", func(t *testing.T) {
		before := snapshotTree(t, dest)
		target := restoreTarget(t)
		stdout, stderr, code := runInProcess(t, "restore",
			"--from-media", dest, "--target", target, "--work-dir", tempWorkDir(t))
		if code != report.ExitOK {
			t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
		}
		if !strings.Contains(stderr, "does not match the export's recorded manifest self-row") {
			t.Errorf("resumed media must produce the loud anchor warning:\n%s", stderr)
		}
		if !strings.Contains(stdout, "status: ok") {
			t.Errorf("stdout lacks the ok status:\n%s", stdout)
		}
		assertSameRestoredTree(t, root, target)
		assertTreeUnchanged(t, before, dest)
	})

	// Leg A: the MANIFEST lies, the export tells the truth -> success.
	t.Run("manifest disagrees with the export", func(t *testing.T) {
		plantedLie := strings.Repeat("ab", 32)
		for _, img := range []int64{1, 2} {
			manPath := filepath.Join(destImageDir(dest, setID, img), media.ManifestFileName)
			if n := setManifestChunkHashes(t, manPath, plantedLie); n == 0 {
				t.Fatalf("image %d's manifest holds no chunk parts (nothing planted)", img)
			}
		}
		before := snapshotTree(t, dest)
		target := restoreTarget(t)
		stdout, stderr, code := runInProcess(t, "restore",
			"--from-media", dest, "--target", target, "--work-dir", tempWorkDir(t))
		if code != report.ExitOK {
			t.Fatalf("the export's hash must win over the manifest's lie (exit = %d, want 0)\nstdout: %s\nstderr: %s",
				code, stdout, stderr)
		}
		if strings.Contains(stdout, "hash mismatch") {
			t.Errorf("the manifest's planted hash was used as an anchor:\n%s", stdout)
		}
		assertSameRestoredTree(t, root, target)
		assertTreeUnchanged(t, before, dest)
	})

	// Leg B1: the EXPORT lies (manifest restored to the truth) -> the
	// export's own internal anchor catches it BEFORE any work.
	t.Run("export hash wrong is refused as a corrupt catalog", func(t *testing.T) {
		for _, img := range []int64{1, 2} {
			if err := os.WriteFile(filepath.Join(destImageDir(dest, setID, img), media.ManifestFileName),
				pristine[img], 0o644); err != nil {
				t.Fatalf("restoring image %d's manifest: %v", img, err)
			}
		}
		wrong := bytes.Repeat([]byte{0x5A}, 32)
		editMediaExport(t, exportPath, func(t *testing.T, db *sql.DB) {
			if _, err := db.Exec(`UPDATE chunks SET hash = ?`, wrong); err != nil {
				t.Fatalf("planting a wrong export chunk hash: %v", err)
			}
		})
		before := snapshotTree(t, dest)
		target := restoreTarget(t)
		stdout, stderr, code := runInProcess(t, "restore",
			"--from-media", dest, "--target", target, "--work-dir", tempWorkDir(t))
		if code != report.ExitCatalog {
			t.Fatalf("exit = %d, want 4 (the export's mirrored chunk anchor must refuse it)\nstdout: %s\nstderr: %s",
				code, stdout, stderr)
		}
		for _, want := range []string{"corrupt catalog", "chunks.hash anchor"} {
			if !strings.Contains(stderr, want) {
				t.Errorf("stderr lacks %q (the export's internal anchor must name itself):\n%s", want, stderr)
			}
		}
		if stdout != "" {
			t.Errorf("an abort-before-work refusal must keep stdout empty, got: %q", stdout)
		}
		if _, err := os.Stat(filepath.Join(target, "big.bin")); !os.IsNotExist(err) {
			t.Errorf("a refused export must write NO target files: %v", err)
		}
		assertTreeUnchanged(t, before, dest)
	})

	// Leg B2: the SAME wrong export hash, made consistent across the
	// export's own two tables (image_contents mirrors it) -> the gate
	// passes, and the fill must still prefer the EXPORT over the manifest's
	// correct hash: the engine reports the first-verification mismatch.
	t.Run("consistent wrong export hash beats the manifest's truth", func(t *testing.T) {
		wrong := bytes.Repeat([]byte{0x5A}, 32)
		editMediaExport(t, exportPath, func(t *testing.T, db *sql.DB) {
			if _, err := db.Exec(`UPDATE chunks SET hash = ?`, wrong); err != nil {
				t.Fatalf("planting a wrong export chunk hash: %v", err)
			}
			res, err := db.Exec(`UPDATE image_contents SET hash = ? WHERE kind = 'chunk'`, wrong)
			if err != nil {
				t.Fatalf("aligning the export's mirrored chunk hashes: %v", err)
			}
			if n, err := res.RowsAffected(); err != nil || n == 0 {
				t.Fatalf("mirrored %d chunk rows (err %v), want at least 1", n, err)
			}
		})
		before := snapshotTree(t, dest)
		target := restoreTarget(t)
		stdout, stderr, code := runInProcess(t, "restore",
			"--from-media", dest, "--target", target, "--work-dir", tempWorkDir(t))
		if code != report.ExitItemFailures {
			t.Fatalf("exit = %d, want 10 (the wrong EXPORT hash must fail the chunk, not be overridden)\nstdout: %s\nstderr: %s",
				code, stdout, stderr)
		}
		if !strings.Contains(stdout, "hash mismatch (first verification)") {
			t.Errorf("stdout lacks the engine's first-verification mismatch reason:\n%s", stdout)
		}
		if !strings.Contains(stdout, "status: partial") {
			t.Errorf("stdout lacks the partial status:\n%s", stdout)
		}
		if _, err := os.Stat(filepath.Join(target, "big.bin")); !os.IsNotExist(err) {
			t.Errorf("big.bin must NOT reach its final name on a failed first verification: %v", err)
		}
		assertTreeUnchanged(t, before, dest)
	})
}

// TestRestoreFromMediaNoChunkHashAnywhereFailsHonestly covers the third
// branch of the finding: export hash NULL (first-run media) AND the
// manifest's chunk hashes blanked -> NOTHING carries a hash. The engine's
// honest per-chunk failure must fire: exit 10 with its reason in the report,
// no final file, no panic, and certainly no silent success.
func TestRestoreFromMediaNoChunkHashAnywhereFailsHonestly(t *testing.T) {
	const setID = "S-FMNOH"
	cat, dest, _ := restoreTwoImageFixture(t, setID)
	leadExport := filepath.Join(destImageDir(dest, setID, 1), media.ExportFileName)

	// Premise 1: the export's chunk hashes are NULL (first-run).
	total, nulls := exportChunkHashCounts(t, leadExport)
	if total == 0 || nulls != total {
		t.Fatalf("first-run export = %d NULL of %d, want every chunk hash NULL", nulls, total)
	}
	// Premise 2: the manifest carries no anchor either (blanked).
	for _, img := range []int64{1, 2} {
		manPath := filepath.Join(destImageDir(dest, setID, img), media.ManifestFileName)
		if n := setManifestChunkHashes(t, manPath, ""); n == 0 {
			t.Fatalf("image %d's manifest holds no chunk parts (nothing blanked)", img)
		}
	}
	if err := os.Remove(cat); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog: %v", err)
	}
	before := snapshotTree(t, dest)
	target := restoreTarget(t)

	stdout, stderr, code := runInProcess(t, "restore",
		"--from-media", dest, "--target", target, "--work-dir", tempWorkDir(t))
	if code != report.ExitItemFailures {
		t.Fatalf("exit = %d, want 10 (an unanchored chunk must fail honestly)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	// The engine's own reason, in the report the user reads.
	for _, want := range []string{
		"the chunk row carries no stored hash",
		"no final file was written for this member",
		"status: partial",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("stdout lacks %q:\n%s", want, stdout)
		}
	}
	if _, err := os.Stat(filepath.Join(target, "big.bin")); !os.IsNotExist(err) {
		t.Errorf("big.bin must not reach its final name without any hash anchor: %v", err)
	}
	// The report exists on stdout and the run ended in its own class - a
	// panic would have produced neither.
	if !strings.Contains(stdout, "restore: set "+setID) {
		t.Errorf("stdout lacks the report header:\n%s", stdout)
	}
	assertTreeUnchanged(t, before, dest)
}

// --- the gates: schema/structure, set identity, missing export ---------------

// TestRestoreFromMediaPastVersionSchemalessExportFailsHonestly is the
// PAST-version cousin of the future-version gate: a brand-new SQLite
// database (user_version = 0 and NO schema at all) renamed as the media's
// export. catalog.Open read-only ACCEPTS it - read-only never migrates and
// only a FUTURE user_version is hard-gated (DESIGN_SPEC 2.1) - and PRAGMA
// integrity_check cannot condemn a database that is internally consistent,
// so the run must fail one step further down, at the first catalog read.
// THE CLASS IS PINNED HERE (T5.5 fix round): an export that opens but
// cannot answer a catalog query is an EXPORT failure, and every export
// failure of this flow is the catalog class - exit 4 through
// catalog.MediaReadErr - never the generic fatal (exit 1) the QA first
// observed for "no such table: sets". Also asserted: the failing read is
// named on stderr, stdout is EMPTY (abort before work), ZERO target
// writes, a byte-identical media, and a cleaned scratch.
func TestRestoreFromMediaPastVersionSchemalessExportFailsHonestly(t *testing.T) {
	const setID = "S-FMVER0"
	cat, dest, _ := restoreTwoImageFixture(t, setID)
	if err := os.Remove(cat); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog: %v", err)
	}
	leadExport := filepath.Join(destImageDir(dest, setID, 1), media.ExportFileName)
	if err := os.WriteFile(leadExport, nil, 0o644); err != nil {
		t.Fatalf("planting the schemaless export: %v", err)
	}
	if got := exportUserVersion(t, leadExport); got != 0 {
		t.Fatalf("fixture anomaly: user_version = %d, want 0", got)
	}
	before := snapshotTree(t, dest)
	target := restoreTarget(t)

	stdout, stderr, code := runInProcess(t, "restore",
		"--from-media", dest, "--target", target, "--work-dir", tempWorkDir(t))
	// THE CLASS: catalog-class, exit 4 - the same refusal class the
	// damaged-export integrity gate and the two-sets-row refusal return
	// (a schemaless export is a database that is not a usable CATALOG:
	// KindCorrupt, catalog.MediaReadErr).
	if code != report.ExitCatalog {
		t.Fatalf("exit = %d, want 4 (a non-catalog export is a catalog-class failure, not the generic fatal)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	// The refusal names its class AND the read that could not be answered.
	for _, want := range []string{"catalog", "corrupt", "sets"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if stdout != "" {
		t.Errorf("an abort-before-work refusal must keep stdout empty, got: %q", stdout)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("a refused export must write NO target files: %v", err)
	}
	assertTreeUnchanged(t, before, dest)
}

// TestRestoreFromMediaSelectionPhaseReadsFailCatalogClass closes the T5.5
// fold-in contract gap at the COMMAND seat: openRestoreCatalog's godoc
// promises that no export failure of the media flow leaves as the generic
// fatal, but the SELECTION-PHASE export reads (restore.Store's
// ActiveRecords / ActiveDirectories / Placement / ImageInventory, as
// reached through the mediaStore seat) used to return bare driver errors
// -> exit 1. THE REACHABLE REPRO, pinned here: the FR-035 aged-media case
// the read-only open accepts BY DESIGN (only a FUTURE user_version is
// hard-gated, DESIGN_SPEC 2.1) - a past-version export whose schema lost
// a table after it was written. It opens, passes PRAGMA integrity_check,
// and answers the sets/images/image_contents reads of the open and anchor
// steps, then fails one of the four selection reads. THE CLASS IS PINNED:
// catalog class, exit 4, through catalog.MediaReadErr at the mediaStore
// seat - never the generic fatal. Also asserted for each failing read:
// the refusal names its class AND the read, ZERO target writes, media
// byte-identical, scratch cleaned (tempWorkDir's cleanup). NOTE ON
// STDOUT: unlike the pre-bus gate tests above (which assert empty
// stdout), this failure strikes AFTER the abort-before-work gates - a
// post-bus terminal store read - and 10.3's report contract for those
// (the restore.go file doc) is that the report still lands on stdout
// ("a terminal error that strikes MID-LOOP (store read, ...) still
// reports what DID restore before returning its own class"), carrying
// the failed status; the CLASS is what this test pins.
func TestRestoreFromMediaSelectionPhaseReadsFailCatalogClass(t *testing.T) {
	cases := []struct {
		name         string
		mutate       []string // the test-side schema mutations on the export
		failingRead  string   // phrase the stderr must carry (the failing read's op)
		driverPhrase string   // the surviving driver reason
	}{
		{
			name:         "dropped directories table",
			mutate:       []string{"DROP TABLE directories"},
			failingRead:  "active directories",
			driverPhrase: "no such table",
		},
		{
			// The reviewer's second repro shape: a files table MISSING a
			// column (a past-version schema that predated it). The VIEW
			// referencing the column goes first - SQLite refuses a drop
			// column a view depends on - and the anchor step SURVIVES the
			// mutation (ImageContentList joins files only on
			// file_id/hash/link_target), so the run really reaches the
			// SELECTION-phase ActiveRecords read, one step further down
			// than every read the fix round already classified.
			name: "files missing superseded_by",
			mutate: []string{
				"DROP VIEW v_active_files",
				"ALTER TABLE files DROP COLUMN superseded_by",
			},
			failingRead:  "active records",
			driverPhrase: "no such column",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const setID = "S-FMSEL"
			cat, dest, _ := restoreTwoImageFixture(t, setID)
			if err := os.Remove(cat); err != nil {
				t.Fatalf("deleting the ORIGINAL catalog: %v", err)
			}
			leadExport := filepath.Join(destImageDir(dest, setID, 1), media.ExportFileName)
			editMediaExport(t, leadExport, func(t *testing.T, db *sql.DB) {
				for _, stmt := range tc.mutate {
					if _, err := db.Exec(stmt); err != nil {
						t.Fatalf("applying the aged-media plant %q: %v", stmt, err)
					}
				}
			})
			// THE REPRO'S PREMISE, asserted: the aged export still answers
			// the open/identity reads (a surviving sets row), so the run
			// reaches the SELECTION phase - one step beyond every read the
			// fix round already classified.
			withMediaExportCopy(t, leadExport, func(db *sql.DB) {
				var n int
				if err := db.QueryRow(`SELECT count(*) FROM sets`).Scan(&n); err != nil || n != 1 {
					t.Fatalf("fixture anomaly: the mutated export must still answer the sets read (n=%d, err=%v)", n, err)
				}
			})
			before := snapshotTree(t, dest)
			target := restoreTarget(t)

			stdout, stderr, code := runInProcess(t, "restore",
				"--from-media", dest, "--target", target, "--work-dir", tempWorkDir(t))
			// THE CLASS: catalog class, exit 4 - never the generic fatal
			// (exit 1) this fold-in closed.
			if code != report.ExitCatalog {
				t.Fatalf("exit = %d, want 4 (a media-path selection read of a broken export is a catalog-class failure)\nstdout: %s\nstderr: %s",
					code, stdout, stderr)
			}
			// The refusal names its class AND the read that could not be
			// answered (the driver reason survives inside the class).
			for _, want := range []string{"catalog", "corrupt", tc.failingRead, tc.driverPhrase} {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr lacks %q:\n%s", want, stderr)
				}
			}
			// The report on stdout carries the FAILED status (the 10.3
			// contract quoted in the function doc) - never a success.
			if !strings.Contains(stdout, "status: failed") {
				t.Errorf("stdout lacks the failed-status report:\n%s", stdout)
			}
			// ZERO target writes: this failure precedes workspace and
			// target creation.
			if _, err := os.Stat(target); !os.IsNotExist(err) {
				t.Errorf("a refused export must write NO target files: %v", err)
			}
			// Media untouched (snapshot taken AFTER the test-side plant),
			// scratch cleaned (tempWorkDir's cleanup re-asserts empty).
			assertTreeUnchanged(t, before, dest)
		})
	}

	// THE CONTRAST the seat's conditional documents: the PLAIN --catalog
	// path reading the very same broken database keeps its PRE-EXISTING
	// class - the bare driver error reaches report.Classify unchanged and
	// exits 1 (the generic fatal). Aligning the plain path would touch the
	// whole tool's foreign-catalog behavior (validate/report answer a
	// foreign sqlite the same way) and is deliberately out of scope, so
	// the media class is the mediaStore seat's wrap only.
	t.Run("the plain --catalog path keeps its pre-existing class", func(t *testing.T) {
		const setID = "S-FMSELPLAIN"
		cat, dest, _ := restoreTwoImageFixture(t, setID)
		leadExport := filepath.Join(destImageDir(dest, setID, 1), media.ExportFileName)
		editMediaExport(t, leadExport, func(t *testing.T, db *sql.DB) {
			if _, err := db.Exec("DROP TABLE directories"); err != nil {
				t.Fatalf("dropping the directories table: %v", err)
			}
		})
		// The broken database travels as the --catalog FILE (a byte copy:
		// the test never writes the media).
		blob, err := os.ReadFile(leadExport)
		if err != nil {
			t.Fatalf("reading the mutated export: %v", err)
		}
		brokenCatalog := filepath.Join(t.TempDir(), "broken-catalog.sqlite")
		if err := os.WriteFile(brokenCatalog, blob, 0o600); err != nil {
			t.Fatalf("writing the broken catalog copy: %v", err)
		}
		if err := os.Remove(cat); err != nil {
			t.Fatalf("deleting the ORIGINAL catalog: %v", err)
		}
		target := restoreTarget(t)
		stdout, stderr, code := runInProcess(t, "restore",
			"--catalog", brokenCatalog, "--set-id", setID, "--target", target, "--work-dir", tempWorkDir(t))
		if code != report.ExitFatal {
			t.Fatalf("exit = %d, want 1 (the plain --catalog path keeps its PRE-EXISTING class; the media wrap is the mediaStore seat's conditional)\nstdout: %s\nstderr: %s",
				code, stdout, stderr)
		}
		// The catalog CLASS marker must NOT appear: this is the bare
		// driver error, not a classified KindCorrupt refusal.
		if strings.Contains(stderr, "corrupt error") {
			t.Errorf("the plain --catalog path must not gain the media class:\n%s", stderr)
		}
		if !strings.Contains(stderr, "no such table") {
			t.Errorf("stderr must carry the bare driver reason:\n%s", stderr)
		}
	})
}

// TestRestoreFromMediaDamagedExportFailsIntegrityGate pins the PRAGMA
// integrity_check gate that runs on the TEMP COPY (frommedia.IntegrityCheck,
// 8.5's cheap first gate before any row is read): the export is cut inside
// its LAST page, so the header and schema still read (catalog.Open
// succeeds) while the tree is inconsistent - only the integrity gate can
// catch it. Exit 4, no target writes, media byte-identical.
func TestRestoreFromMediaDamagedExportFailsIntegrityGate(t *testing.T) {
	const setID = "S-FMDMG"
	cat, dest, _ := restoreTwoImageFixture(t, setID)
	if err := os.Remove(cat); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog: %v", err)
	}
	leadExport := filepath.Join(destImageDir(dest, setID, 1), media.ExportFileName)
	blob, err := os.ReadFile(leadExport)
	if err != nil {
		t.Fatalf("reading the export: %v", err)
	}
	if len(blob) <= 2000 {
		t.Fatalf("fixture anomaly: export is %d bytes, too small to cut meaningfully", len(blob))
	}
	// Cut ONE BYTE short of the whole last page (4096 - 1): the schema and
	// every earlier page still read, so catalog.Open ACCEPTS the copy and
	// the run reaches the integrity gate - only PRAGMA integrity_check can
	// see that the last page's tree is gone. The premise is asserted below
	// against a byte copy, so a future SQLite build that rejects at the
	// open instead fails this test with that explanation in hand.
	damaged := blob[:len(blob)-4095]
	if err := os.WriteFile(leadExport, damaged, 0o644); err != nil {
		t.Fatalf("damaging the export: %v", err)
	}
	dmgCopy := filepath.Join(t.TempDir(), "damaged.sqlite")
	if err := os.WriteFile(dmgCopy, damaged, 0o600); err != nil {
		t.Fatalf("writing the premise copy: %v", err)
	}
	if db, oerr := catalog.Open(dmgCopy, catalog.OpenReadOnly); oerr != nil {
		t.Fatalf("FIXTURE PREMISE: the damaged export must still open so the run reaches the integrity gate (this SQLite build refuses it earlier: %v)", oerr)
	} else {
		_ = db.Close()
	}
	before := snapshotTree(t, dest)
	target := restoreTarget(t)

	stdout, stderr, code := runInProcess(t, "restore",
		"--from-media", dest, "--target", target, "--work-dir", tempWorkDir(t))
	if code != report.ExitCatalog {
		t.Fatalf("exit = %d, want 4 (a damaged export is a catalog-class failure)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	for _, want := range []string{"catalog", "corrupt", "integrity_check"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q (the integrity gate must name itself):\n%s", want, stderr)
		}
	}
	if stdout != "" {
		t.Errorf("an abort-before-work refusal must keep stdout empty, got: %q", stdout)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("a damaged export must write NO target files: %v", err)
	}
	assertTreeUnchanged(t, before, dest)
}

// TestRestoreFromMediaTwoSetsRowsRejected pins the single-row guarantee the
// set identity rests on: the FR-034 export is set-scoped, so its sets table
// holds EXACTLY ONE row. An export carrying two (a whole-catalog file
// renamed as the export) is KindCorrupt, exit 4, before any work.
func TestRestoreFromMediaTwoSetsRowsRejected(t *testing.T) {
	const setID = "S-FMTWO"
	cat, dest, _ := restoreTwoImageFixture(t, setID)
	if err := os.Remove(cat); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog: %v", err)
	}
	leadExport := filepath.Join(destImageDir(dest, setID, 1), media.ExportFileName)
	editMediaExport(t, leadExport, func(t *testing.T, db *sql.DB) {
		if _, err := db.Exec(
			`INSERT INTO sets (set_id, source_root, created_at, tool_version) VALUES (?, ?, ?, ?)`,
			"S-INTRUDER", "/tmp/intruder", 1, "test"); err != nil {
			t.Fatalf("inserting the second sets row: %v", err)
		}
	})
	// The set identity must really see TWO rows (the plant is not vacuous).
	var rows int
	withMediaExportCopy(t, leadExport, func(db *sql.DB) {
		if err := db.QueryRow(`SELECT count(*) FROM sets`).Scan(&rows); err != nil {
			t.Fatalf("counting sets rows: %v", err)
		}
	})
	if rows != 2 {
		t.Fatalf("fixture anomaly: export holds %d sets rows, want 2", rows)
	}
	before := snapshotTree(t, dest)
	target := restoreTarget(t)

	stdout, stderr, code := runInProcess(t, "restore",
		"--from-media", dest, "--target", target, "--work-dir", tempWorkDir(t))
	if code != report.ExitCatalog {
		t.Fatalf("exit = %d, want 4 (corrupt catalog class)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	for _, want := range []string{"2 sets rows", "exactly 1"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if stdout != "" {
		t.Errorf("an abort-before-work refusal must keep stdout empty, got: %q", stdout)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("a two-sets export must write NO target files: %v", err)
	}
	assertTreeUnchanged(t, before, dest)
}

// TestRestoreFromMediaMissingExportFailsBeforeAnyWrite pins the two-layer
// split on the "manifest present, export ABSENT" media: the quick layer
// answers from MANIFEST.json alone, then the command refuses (catalog class,
// exit 4) naming the export - with the scratch dir it just created removed
// (the export-open failure path of mediaExportScratch).
func TestRestoreFromMediaMissingExportFailsBeforeAnyWrite(t *testing.T) {
	const setID = "S-FMNOEXP"
	cat, dest, _ := restoreTwoImageFixture(t, setID)
	if err := os.Remove(cat); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog: %v", err)
	}
	leadDir := destImageDir(dest, setID, 1)
	if err := os.Remove(filepath.Join(leadDir, media.ExportFileName)); err != nil {
		t.Fatalf("removing the export: %v", err)
	}

	// THE QUICK LAYER still answers without the export (8.5's contract).
	info, err := restore.ResolveMedia(dest)
	if err != nil {
		t.Fatalf("ResolveMedia must answer from the manifest alone: %v", err)
	}
	if info.SetID != setID {
		t.Errorf("introspected set = %q, want %s", info.SetID, setID)
	}

	before := snapshotTree(t, dest)
	target := restoreTarget(t)
	stdout, stderr, code := runInProcess(t, "restore",
		"--from-media", dest, "--target", target, "--work-dir", tempWorkDir(t))
	if code != report.ExitCatalog {
		t.Fatalf("exit = %d, want 4 (the export is the catalog of this flow)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if !strings.Contains(stderr, media.ExportFileName) {
		t.Errorf("stderr must name the missing export:\n%s", stderr)
	}
	if stdout != "" {
		t.Errorf("an abort-before-work refusal must keep stdout empty, got: %q", stdout)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Errorf("a media without an export must write NO target files: %v", err)
	}
	// The scratch dir mediaExportScratch created before the failed open is
	// removed (tempWorkDir's cleanup asserts the whole work dir is empty).
	assertTreeUnchanged(t, before, dest)
}

// --- supply precedence through the command -----------------------------------

// fromMediaWholeFileTwoImageFixture scans, plans, and copies a TWO-image set
// with NO chunked member: under the synthetic profile ShouldChunk refuses
// files that fit alone on an empty image (mandatory-only chunking, FR-041),
// so two 600,000-byte files plus a small one spread over two destinations as
// WHOLE files. Supply-precedence tests use it when they must not drag the
// chunk-hash fill into the picture - and its premises are asserted below,
// not assumed.
func fromMediaWholeFileTwoImageFixture(t *testing.T, setID string) (cat, dest, root string) {
	t.Helper()
	root = t.TempDir()
	writeFileP(t, filepath.Join(root, "first.bin"), pattern(21, 600_000), 0o644)
	writeFileP(t, filepath.Join(root, "second.bin"), pattern(22, 600_000), 0o644)
	writeFileP(t, filepath.Join(root, "small.txt"), pattern(23, 1000), 0o644)
	cat = createCatalog(t)
	scanBuildFixture(t, root, cat, setID)
	planSynthetic(t, cat, setID, copySyntheticUsable)
	dest = filepath.Join(t.TempDir(), "dest")
	if stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest); code != 0 {
		t.Fatalf("copy fixture exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	// Premises: the set really spans two images, and carries NO chunk rows
	// (nothing in the flow below can depend on the manifest chunk-hash fill).
	for _, img := range []int64{1, 2} {
		if fi, err := os.Stat(destImageDir(dest, setID, img)); err != nil || !fi.IsDir() {
			t.Fatalf("fixture anomaly: image %d's tree is missing (%v)", img, err)
		}
	}
	var chunks int
	withMediaExportCopy(t, filepath.Join(destImageDir(dest, setID, 1), media.ExportFileName), func(db *sql.DB) {
		if err := db.QueryRow("SELECT count(*) FROM chunks").Scan(&chunks); err != nil {
			t.Fatalf("counting the fixture's chunk rows: %v", err)
		}
	})
	if chunks != 0 {
		t.Fatalf("fixture anomaly: the whole-file fixture carries %d chunk rows, want 0", chunks)
	}
	return cat, dest, root
}

// TestRestoreFromMediaMediaTreeBeatsCopyTreeRoot pins the FIRST half of the
// documented 8.5 precedence at the COMMAND seat: PATH resolves BOTH images
// AND a --copy-tree-root also offers <SET>-001 -> the MEDIA tree wins (the
// report names the media's path, never the decoy's - whose payload is an
// empty directory, so a supplement-first resolution could not even restore).
func TestRestoreFromMediaMediaTreeBeatsCopyTreeRoot(t *testing.T) {
	const setID = "S-FMSUP"
	cat, dest, root := restoreTwoImageFixture(t, setID)
	tree1 := destImageDir(dest, setID, 1)
	tree2 := destImageDir(dest, setID, 2)
	decoyRoot := filepath.Join(t.TempDir(), "decoy")
	if err := os.MkdirAll(filepath.Join(decoyRoot, setID+"-001"), 0o750); err != nil {
		t.Fatalf("creating the decoy tree: %v", err)
	}
	if err := os.Remove(cat); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog: %v", err)
	}
	target := restoreTarget(t)
	stdout, stderr, code := runInProcess(t, "restore",
		"--from-media", dest, "--copy-tree-root", decoyRoot,
		"--target", target, "--work-dir", tempWorkDir(t))
	if code != report.ExitOK {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "image 1: restored "+tree1) {
		t.Errorf("stdout must name the MEDIA tree as image 1's source:\n%s", stdout)
	}
	if !strings.Contains(stdout, "image 2: restored "+tree2) {
		t.Errorf("stdout must name the MEDIA tree as image 2's source:\n%s", stdout)
	}
	if strings.Contains(stdout, decoyRoot) {
		t.Errorf("the --copy-tree-root supplement must not be consulted while the media resolves the image:\n%s", stdout)
	}
	assertSameRestoredTree(t, root, target)
}

// TestRestoreFromMediaCopyTreeRootSuppliesMediaMiss pins the SECOND half:
// PATH is image 1's tree alone (root mode), so the media does NOT resolve
// image 2 - the miss falls through to the supplement (--copy-tree-root),
// while image 1 still comes from the media base. The fixture carries no
// chunked member, so this test measures SUPPLY precedence alone.
func TestRestoreFromMediaCopyTreeRootSuppliesMediaMiss(t *testing.T) {
	const setID = "S-FMSFALL"
	cat, dest, root := fromMediaWholeFileTwoImageFixture(t, setID)
	tree1 := destImageDir(dest, setID, 1)
	tree2 := destImageDir(dest, setID, 2)
	if err := os.Remove(cat); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog: %v", err)
	}
	target := restoreTarget(t)
	stdout, stderr, code := runInProcess(t, "restore",
		"--from-media", tree1, "--copy-tree-root", dest,
		"--target", target, "--work-dir", tempWorkDir(t))
	if code != report.ExitOK {
		t.Fatalf("exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "image 1: restored "+tree1) {
		t.Errorf("stdout must name the MEDIA tree as image 1's source:\n%s", stdout)
	}
	if !strings.Contains(stdout, "image 2: restored "+tree2) {
		t.Errorf("stdout must name the copy-tree-root supplement as image 2's source:\n%s", stdout)
	}
	assertSameRestoredTree(t, root, target)
}

// TestRestoreFromMediaCopyTreeRootSuppliedSpanFailsWithoutManifestFillKnownGap
//
// FLIPPED (T5.5 FIX ROUND) - this test WAS the known-gap pin: the
// manifest chunk-hash fill did not follow a --copy-tree-root-supplied
// tree, so the run below failed honestly (exit 10, big.bin incomplete,
// "the chunk row carries no stored hash") even though image 2's tree sat
// right beside PATH with its own MANIFEST.json. THE GAP IS CLOSED: the
// fill now consults the sidecar MANIFEST.json of WHATEVER SourceCopyTree
// directory the supply pass actually resolved for each required image
// (restore_media.go's sidecarManifestRestorer), identity-checked exactly
// like augmentManifestsWithExplicit - so image 2's supplement sidecar
// anchors its rows and the span restores byte-exact.
//
// The NAME is kept (the QA finding referenced this pin by name); the
// assertions below are the flip its original doc promised: exit 0 +
// assertSameRestoredTree instead of the honest failure. The premises that
// made the old failure meaningful still stand and are still asserted:
// FIRST-RUN media (every export chunk hash NULL, so the fill is the only
// anchor available) and a chunked span that only the supplement supplies.
// The sibling of this case - no hash ANYWHERE - remains pinned by
// TestRestoreFromMediaNoChunkHashAnywhereFailsHonestly, and the supply
// precedence itself by TestRestoreFromMediaCopyTreeRootSuppliesMediaMiss.
func TestRestoreFromMediaCopyTreeRootSuppliedSpanFailsWithoutManifestFillKnownGap(t *testing.T) {
	const setID = "S-FMGAP"
	cat, dest, root := restoreTwoImageFixture(t, setID)
	tree1 := destImageDir(dest, setID, 1)
	tree2 := destImageDir(dest, setID, 2)
	leadExport := filepath.Join(tree1, media.ExportFileName)
	// Premise: FIRST-RUN media - the embedded export carries NULL chunk
	// hashes, so the fill is the ONLY anchor available for the span.
	total, nulls := exportChunkHashCounts(t, leadExport)
	if total == 0 || nulls != total {
		t.Fatalf("first-run export = %d NULL of %d, want every chunk hash NULL", nulls, total)
	}
	// Premises of the SUPPLEMENT shape: the media base does NOT resolve
	// image 2 (PATH is image 1's tree) while image 2's sidecar manifest
	// sits in the --copy-tree-root supplement - so the fill can only have
	// got image 2's chunk hashes by following the supply resolution.
	info, err := restore.ResolveMedia(tree1)
	if err != nil {
		t.Fatalf("ResolveMedia(%s): %v", tree1, err)
	}
	if _, ok := info.Manifests[2]; ok {
		t.Fatalf("fixture anomaly: the media base already carries image 2's manifest (this test would not exercise the supplement)")
	}
	if _, err := os.Stat(filepath.Join(tree2, media.ManifestFileName)); err != nil {
		t.Fatalf("fixture anomaly: image 2's supplement sidecar is missing: %v", err)
	}
	if err := os.Remove(cat); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog: %v", err)
	}
	before := snapshotTree(t, dest)
	target := restoreTarget(t)
	stdout, stderr, code := runInProcess(t, "restore",
		"--from-media", tree1, "--copy-tree-root", dest,
		"--target", target, "--work-dir", tempWorkDir(t))
	if code != report.ExitOK {
		t.Fatalf("exit = %d, want 0 (the manifest fill must FOLLOW the --copy-tree-root supplement)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	// Image 2 came from the SUPPLEMENT's tree (the media base does not
	// resolve it), and the whole span is byte-exact at the target.
	if !strings.Contains(stdout, "image 2: restored "+tree2) {
		t.Errorf("stdout must name the supplement tree as image 2's source:\n%s", stdout)
	}
	assertSameRestoredTree(t, root, target)
	assertTreeUnchanged(t, before, dest)
}

// --- the temp-copy discipline: scratch placement and lifetime -----------------

// TestRestoreFromMediaExportScratchLivesUnderWorkDirUntilCancellation is
// the read-only proof's runtime half: while a media restore is PARKED inside
// its first image (the recording fake's deterministic block, keyed off the
// marker line - no sleeps), the export copy must exist UNDER --work-dir
// (never on the media), the media itself must be untouched mid-run, and the
// SIGINT path (exit 130) must remove BOTH the export scratch and the
// restore workspace - the "scratch removed on EVERY path" clause.
func TestRestoreFromMediaExportScratchLivesUnderWorkDirUntilCancellation(t *testing.T) {
	const setID = "S-FMSIG"
	cat, dest, _ := restoreTwoImageFixture(t, setID)
	if err := os.Remove(cat); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog: %v", err)
	}
	before := snapshotTree(t, dest)
	workDir := tempWorkDir(t)
	record := filepath.Join(t.TempDir(), restoreCallsFile)
	target := restoreTarget(t)

	cmd := exec.Command(os.Args[0],
		"restore", "--from-media", dest, "--target", target, "--work-dir", workDir)
	cmd.Env = append(os.Environ(), scanSignalHelperEnv+"=1",
		restoreFakeEnv+"=1", restoreRecordEnv+"="+record, restoreBlockEnv+"=1")
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("stderr pipe: %v", err)
	}
	stdoutBuf := &bytes.Buffer{}
	cmd.Stdout = stdoutBuf
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the restore child: %v", err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	// Readiness: the fake's marker line means the run is INSIDE image 1 -
	// i.e. the catalog swap (temp copy + open + integrity + set identity +
	// anchor) has already happened and the scratch EXISTS.
	sentinel := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		tmp := make([]byte, 1)
		for {
			n, rerr := stderrPipe.Read(tmp)
			if n > 0 {
				buf.Write(tmp[:n])
				if strings.Contains(buf.String(), restoreBlocked) {
					sentinel <- buf.String()
					return
				}
			}
			if rerr != nil {
				sentinel <- buf.String()
				return
			}
		}
	}()
	select {
	case out := <-sentinel:
		if !strings.Contains(out, restoreBlocked) {
			t.Fatalf("restore child never reached the block seam (stderr so far: %s)", out)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the restore child never reached the block seam")
	}

	// MID-RUN: the temp export copy lives UNDER --work-dir...
	var scratch string
	for _, name := range restoreDirNames(t, workDir) {
		if strings.HasPrefix(name, "isopress-restore-export-") {
			scratch = filepath.Join(workDir, name)
		}
	}
	if scratch == "" {
		t.Fatalf("--work-dir holds no export scratch mid-run: %v", restoreDirNames(t, workDir))
	}
	if _, err := os.Stat(filepath.Join(scratch, media.ExportFileName)); err != nil {
		t.Errorf("the export copy is not in the scratch: %v", err)
	}
	// ...and the MEDIA gained nothing (the copy is NOT on the media, and no
	// sidecar appeared next to the media's own export).
	assertTreeUnchanged(t, before, dest)

	// CANCELLATION: one SIGINT -> exit 130, scratch + workspace removed.
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signalling the restore child: %v", err)
	}
	waitErr := cmd.Wait()
	exitCode := 0
	if waitErr != nil {
		exitErr, ok := waitErr.(*exec.ExitError)
		if !ok {
			t.Fatalf("restore child: %v", waitErr)
		}
		exitCode = exitErr.ExitCode()
	}
	if exitCode != report.ExitInterrupted {
		t.Fatalf("exit = %d, want 130\nstdout: %s", exitCode, stdoutBuf.String())
	}
	if !strings.Contains(stdoutBuf.String(), "status: interrupted") {
		t.Errorf("stdout lacks the interrupted report:\n%s", stdoutBuf.String())
	}
	// The export scratch AND the workspace are gone (tempWorkDir's cleanup
	// re-asserts the empty work dir at test end).
	if names := restoreDirNames(t, workDir); len(names) != 0 {
		t.Errorf("--work-dir holds residue after the interrupted media run: %v", names)
	}
	assertTreeUnchanged(t, before, dest)
}

// TestMediaExportScratchPlacementAndFailureCleanup pins the temp-copy step
// itself (restore_media.go): the copy lands UNDER --work-dir (never beside
// the media), its bytes equal the media export's, the copy step writes
// NOTHING to the media, and the export-open failure path removes the
// scratch directory it just created (the branch runRestore's defer covers
// on every other path).
func TestMediaExportScratchPlacementAndFailureCleanup(t *testing.T) {
	const setID = "S-FMSCR"
	_, dest, _ := restoreTwoImageFixture(t, setID)
	leadExport := filepath.Join(destImageDir(dest, setID, 1), media.ExportFileName)
	workDir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		t.Fatalf("work dir: %v", err)
	}
	before := snapshotTree(t, dest)

	// Placement: under --work-dir, at the documented name, byte-identical.
	scratch, copyPath, err := mediaExportScratch(leadExport, workDir)
	if err != nil {
		t.Fatalf("mediaExportScratch: %v", err)
	}
	if !strings.HasPrefix(scratch, workDir+string(os.PathSeparator)) {
		t.Errorf("scratch %q is not under --work-dir %q", scratch, workDir)
	}
	if want := filepath.Join(scratch, media.ExportFileName); copyPath != want {
		t.Errorf("copy path = %q, want %q", copyPath, want)
	}
	if strings.HasPrefix(scratch, dest+string(os.PathSeparator)) {
		t.Errorf("the scratch landed ON THE MEDIA (%q) - nothing may be written there", scratch)
	}
	wantBytes, err := os.ReadFile(leadExport)
	if err != nil {
		t.Fatalf("reading the media export: %v", err)
	}
	gotBytes, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatalf("reading the export copy: %v", err)
	}
	if !bytes.Equal(wantBytes, gotBytes) {
		t.Errorf("the export copy differs from the media's export (%d vs %d bytes)", len(gotBytes), len(wantBytes))
	}
	// The copy step itself wrote nothing to the media (mode/mtime/content
	// of every member, and no new member).
	assertTreeUnchanged(t, before, dest)
	if err := os.RemoveAll(scratch); err != nil { // runRestore's deferred cleanup, exercised by hand
		t.Fatalf("removing the scratch: %v", err)
	}
	if _, err := os.Stat(scratch); !os.IsNotExist(err) {
		t.Errorf("the scratch survived its cleanup: %v", err)
	}

	// Failure path: an export that is not there -> the catalog class, and
	// NO scratch directory is left under --work-dir.
	missing := filepath.Join(dest, "no-such-dir", media.ExportFileName)
	if _, _, err := mediaExportScratch(missing, workDir); err == nil {
		t.Fatal("mediaExportScratch accepted a missing export")
	} else {
		var cerr *catalog.CatalogError
		if !errors.As(err, &cerr) || cerr.Kind != catalog.KindOpen {
			t.Errorf("missing export error = %v (%T), want a KindOpen CatalogError", err, err)
		}
	}
	if names := restoreDirNames(t, workDir); len(names) != 0 {
		t.Errorf("the failed scratch left residue under --work-dir: %v", names)
	}
	assertTreeUnchanged(t, before, dest)
}

// --- the gated real round trip: an ISO beside the media -----------------------

// TestMediaStoreChunkHashFillBranches pins the fill itself where it lives
// (restore_media.go's mediaStore), against a temp copy of a REAL first-run
// export and the REAL manifests beside it:
//
//   - export hash NULL + manifest hash present -> FILLED (and the value is
//     the manifest's own bytes, decoded);
//   - export hash present -> the EXPORT's value is returned untouched, even
//     when the manifest disagrees (precedence pinned at the source);
//   - neither -> the row stays empty, which is what the engine's honest
//     per-chunk failure consumes (command-level above).
func TestMediaStoreChunkHashFillBranches(t *testing.T) {
	const setID = "S-FMUNIT"
	cat, dest, _ := restoreTwoImageFixture(t, setID)
	// The original catalog is not consulted at all: the store reads a
	// TEMP COPY of the on-media export, exactly as the command flow does.
	if err := os.Remove(cat); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog: %v", err)
	}
	info, err := restore.ResolveMedia(dest)
	if err != nil {
		t.Fatalf("ResolveMedia: %v", err)
	}
	// The flow's own discipline: work from a COPY of the media's export.
	blob, err := os.ReadFile(info.ExportPath)
	if err != nil {
		t.Fatalf("reading the export: %v", err)
	}
	exportCopy := filepath.Join(t.TempDir(), media.ExportFileName)
	if err := os.WriteFile(exportCopy, blob, 0o600); err != nil {
		t.Fatalf("writing the export copy: %v", err)
	}
	// Premise: a first-run export carries NULL chunk hashes.
	var total, nulls int
	withMediaExportCopy(t, exportCopy, func(db *sql.DB) {
		if err := db.QueryRow("SELECT count(*) FROM chunks").Scan(&total); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow("SELECT count(*) FROM chunks WHERE hash IS NULL").Scan(&nulls); err != nil {
			t.Fatal(err)
		}
	})
	if total == 0 || nulls != total {
		t.Fatalf("fixture anomaly: export chunk hashes = %d NULL of %d, want all NULL", nulls, total)
	}

	openStore := func(t *testing.T) *mediaStore {
		t.Helper()
		db, err := catalog.Open(exportCopy, catalog.OpenReadOnly)
		if err != nil {
			t.Fatalf("opening the export copy: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		return &mediaStore{restoreStore: &restoreStore{db: db}, manifests: info.Manifests}
	}

	// Branch 1: NULL export hash -> the manifest's value fills the row.
	t.Run("fill fires from the manifest", func(t *testing.T) {
		store := openStore(t)
		filled := 0
		for _, img := range []int64{1, 2} {
			inv, err := store.ImageInventory(t.Context(), setID, img)
			if err != nil {
				t.Fatalf("ImageInventory(%d): %v", img, err)
			}
			if len(inv.Chunks) == 0 {
				t.Fatalf("image %d's inventory holds no chunks", img)
			}
			man := info.Manifests[img]
			for _, c := range inv.Chunks {
				if len(c.Hash) != 32 {
					t.Errorf("image %d chunk %d (%s) hash = %d bytes after the fill, want 32",
						img, c.ChunkIndex, c.ParentRelPath, len(c.Hash))
					continue
				}
				// The filled value IS the manifest's chunk hash.
				want, err := hex.DecodeString(manifestChunkHash(t, man, c.ParentRelPath, c.ChunkIndex))
				if err != nil {
					t.Fatalf("decoding the manifest chunk hash: %v", err)
				}
				if !bytes.Equal(c.Hash, want) {
					t.Errorf("image %d chunk %d filled hash = %x, want the manifest's %x",
						img, c.ChunkIndex, c.Hash, want)
				}
				filled++
			}
		}
		if filled == 0 {
			t.Fatal("the fill never fired (no chunk received a hash)")
		}
	})

	// Branch 2: the export carries a hash -> THE EXPORT WINS, even against
	// a disagreeing manifest value.
	t.Run("export hash wins", func(t *testing.T) {
		wrong := bytes.Repeat([]byte{0x77}, 32)
		editMediaExport(t, exportCopy, func(t *testing.T, db *sql.DB) {
			if _, err := db.Exec(`UPDATE chunks SET hash = ?`, wrong); err != nil {
				t.Fatalf("stamping the export hash: %v", err)
			}
		})
		store := openStore(t)
		inv, err := store.ImageInventory(t.Context(), setID, 1)
		if err != nil {
			t.Fatalf("ImageInventory(1): %v", err)
		}
		if len(inv.Chunks) == 0 {
			t.Fatal("image 1's inventory holds no chunks")
		}
		for _, c := range inv.Chunks {
			if !bytes.Equal(c.Hash, wrong) {
				t.Errorf("chunk %d hash = %x, want the EXPORT's %x (an export hash always wins)",
					c.ChunkIndex, c.Hash, wrong)
			}
		}
	})

	// Branch 3: neither side carries a hash -> the row stays EMPTY (the
	// engine's honest failure consumes it; pinned at the command above).
	t.Run("no anchor anywhere stays empty", func(t *testing.T) {
		// A pristine (NULL-hashed) export copy plus blanked manifests.
		if err := os.WriteFile(exportCopy, blob, 0o600); err != nil {
			t.Fatalf("restoring the pristine export copy: %v", err)
		}
		bare := map[int64]media.Manifest{}
		for img, m := range info.Manifests {
			for i := range m.ChunkedFiles {
				for j := range m.ChunkedFiles[i].Chunks {
					m.ChunkedFiles[i].Chunks[j].SHA256 = ""
				}
			}
			bare[img] = m
		}
		db, err := catalog.Open(exportCopy, catalog.OpenReadOnly)
		if err != nil {
			t.Fatalf("opening the pristine export copy: %v", err)
		}
		defer db.Close() //nolint:errcheck // assertion handle
		store := &mediaStore{restoreStore: &restoreStore{db: db}, manifests: bare}
		inv, err := store.ImageInventory(t.Context(), setID, 1)
		if err != nil {
			t.Fatalf("ImageInventory(1): %v", err)
		}
		if len(inv.Chunks) == 0 {
			t.Fatal("image 1's inventory holds no chunks")
		}
		for _, c := range inv.Chunks {
			if len(c.Hash) != 0 {
				t.Errorf("chunk %d hash = %x, want EMPTY (no anchor anywhere)", c.ChunkIndex, c.Hash)
			}
		}
	})
}

// TestMediaStoreSelectionReadsClassifiedCatalogCorrupt pins the MEDIA
// CLASS SEAT itself (restore_media.go's mediaStore): every SELECTION-PHASE
// export read reached through the seat - ActiveRecords, ActiveDirectories,
// Placement, ImageInventory - classifies a failed read as the catalog
// class (KindCorrupt through catalog.MediaReadErr), and the SAME broken
// database read through the PLAIN restoreStore (the --catalog path's
// store) keeps its pre-existing class - the documented conditional, the
// twin of runRestore's CopyImageIDs wrap. Context errors pass through the
// seat unchanged (an interrupted read never masquerades as a corrupt
// export). The command-level consequences (exit 4, zero target writes,
// media untouched) are pinned by
// TestRestoreFromMediaSelectionPhaseReadsFailCatalogClass.
func TestMediaStoreSelectionReadsClassifiedCatalogCorrupt(t *testing.T) {
	const setID = "S-FMSEAT"
	cat, dest, _ := restoreTwoImageFixture(t, setID)
	if err := os.Remove(cat); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog: %v", err)
	}
	info, err := restore.ResolveMedia(dest)
	if err != nil {
		t.Fatalf("ResolveMedia: %v", err)
	}
	blob, err := os.ReadFile(info.ExportPath)
	if err != nil {
		t.Fatalf("reading the export: %v", err)
	}

	// Each case drives ONE wrap site against a FRESH byte copy of the
	// export whose named table was dropped (the aged-media shape), through
	// BOTH stores over the SAME handle.
	cases := []struct {
		name     string
		drop     string
		media    func(*mediaStore) error
		plain    func(*restoreStore) error
		opPhrase string // the site's MediaReadErr op phrase
	}{
		{
			name: "active records", drop: "files",
			media:    func(s *mediaStore) error { _, err := s.ActiveRecords(t.Context(), setID); return err },
			plain:    func(s *restoreStore) error { _, err := s.ActiveRecords(t.Context(), setID); return err },
			opPhrase: "active records",
		},
		{
			name: "active directories", drop: "directories",
			media:    func(s *mediaStore) error { _, err := s.ActiveDirectories(t.Context(), setID); return err },
			plain:    func(s *restoreStore) error { _, err := s.ActiveDirectories(t.Context(), setID); return err },
			opPhrase: "active directories",
		},
		{
			name: "placement", drop: "images",
			media:    func(s *mediaStore) error { _, err := s.Placement(t.Context(), setID); return err },
			plain:    func(s *restoreStore) error { _, err := s.Placement(t.Context(), setID); return err },
			opPhrase: "placement summary",
		},
		{
			name: "image inventory", drop: "files",
			media:    func(s *mediaStore) error { _, err := s.ImageInventory(t.Context(), setID, 1); return err },
			plain:    func(s *restoreStore) error { _, err := s.ImageInventory(t.Context(), setID, 1); return err },
			opPhrase: "inventory",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			exportCopy := filepath.Join(t.TempDir(), media.ExportFileName)
			if err := os.WriteFile(exportCopy, blob, 0o600); err != nil {
				t.Fatalf("writing the export copy: %v", err)
			}
			editMediaExport(t, exportCopy, func(t *testing.T, db *sql.DB) {
				if _, err := db.Exec("DROP TABLE " + tc.drop); err != nil {
					t.Fatalf("dropping the %s table: %v", tc.drop, err)
				}
			})
			db, err := catalog.Open(exportCopy, catalog.OpenReadOnly)
			if err != nil {
				t.Fatalf("opening the mutated export copy: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			store := &mediaStore{restoreStore: &restoreStore{db: db}, manifests: info.Manifests}

			// THE MEDIA SEAT: KindCorrupt CatalogError, carrying the site's
			// op AND the surviving driver reason.
			err = tc.media(store)
			var cerr *catalog.CatalogError
			if !errors.As(err, &cerr) || cerr.Kind != catalog.KindCorrupt {
				t.Fatalf("media-seat error = %v (%T), want a KindCorrupt CatalogError", err, err)
			}
			for _, want := range []string{tc.opPhrase, "no such table"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("media-seat error lacks %q: %v", want, err)
				}
			}

			// THE PLAIN SEAT, same database: the pre-existing class is
			// kept (the documented conditional) - NOT the catalog class.
			plainErr := tc.plain(&restoreStore{db: db})
			if plainErr == nil {
				t.Fatal("the plain store answered a dropped table (the plant is vacuous)")
			}
			var perr *catalog.CatalogError
			if errors.As(plainErr, &perr) {
				t.Errorf("the plain --catalog store must keep its pre-existing class, got the catalog class: %v", plainErr)
			}
		})
	}

	// CONTEXT PASSTHROUGH: a cancelled read through the media seat is the
	// cancellation itself - never a corrupt export (MediaReadErr's
	// contract, pinned at the seat that now consumes it).
	t.Run("a cancelled read passes through unchanged", func(t *testing.T) {
		exportCopy := filepath.Join(t.TempDir(), media.ExportFileName)
		if err := os.WriteFile(exportCopy, blob, 0o600); err != nil {
			t.Fatalf("writing the export copy: %v", err)
		}
		db, err := catalog.Open(exportCopy, catalog.OpenReadOnly)
		if err != nil {
			t.Fatalf("opening the export copy: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		store := &mediaStore{restoreStore: &restoreStore{db: db}, manifests: info.Manifests}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err = store.ActiveDirectories(ctx, setID)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled media-seat read = %v, want context.Canceled itself", err)
		}
		var cerr *catalog.CatalogError
		if errors.As(err, &cerr) {
			t.Errorf("a cancelled read must not masquerade as a corrupt export: %v", err)
		}
	})
}

// manifestChunkHash returns the manifest's chunk-part hash for one parent
// and catalog 0-based index (the fill's lookup key).
func manifestChunkHash(t *testing.T, m media.Manifest, parent string, index int) string {
	t.Helper()
	for _, cf := range m.ChunkedFiles {
		if cf.RelPath != parent {
			continue
		}
		for _, p := range cf.Chunks {
			if p.Index == int64(index) {
				return p.SHA256
			}
		}
	}
	t.Fatalf("the manifest carries no chunk %d of %q", index, parent)
	return ""
}

// --- the supply-following fill seat (sidecarManifestRestorer) ----------------

// sidecarCaptureRestorer records the selection it received: the seam the
// supply-following fill runs at is exactly where the real engine would read
// the inventory, so the capture IS the seat's observable behavior.
type sidecarCaptureRestorer struct {
	got *restore.ResolvedSelection
}

// RestoreImage implements restore.ImageRestorer.
func (r *sidecarCaptureRestorer) RestoreImage(_ context.Context, _ restore.ImageSupply, sel restore.ResolvedSelection, _ string) (restore.ImageRestoreResult, error) {
	r.got = &sel
	return restore.ImageRestoreResult{}, nil
}

// TestSidecarManifestRestorerFillsFromResolvedDirectory pins the SECOND
// fill seat at its own seam (restore_media.go): after the supply pass
// resolved an image to a SourceCopyTree DIRECTORY, the sidecar MANIFEST.json
// of THAT directory is read, identity-checked, and used to fill the NULL
// export chunk hashes of that image's rows - nothing else. The branches:
//
//   - the resolved tree's sidecar fills every still-NULL row (the
//     --copy-tree-root supplement case the T5.5 QA finding broke on);
//   - an ISO supply is not consulted at all (no readable sidecar exists);
//   - a directory whose sidecar claims ANOTHER image (or another set) is
//     not an anchor - opportunistic, never fatal;
//   - a row the export already anchors is never overwritten (the export
//     always wins);
//   - the caller's selection is never mutated - only the copy handed to
//     the inner restorer carries the filled rows.
func TestSidecarManifestRestorerFillsFromResolvedDirectory(t *testing.T) {
	const setID = "S-FMSIDE"
	cat, dest, _ := restoreTwoImageFixture(t, setID)
	if err := os.Remove(cat); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog: %v", err)
	}
	tree1 := destImageDir(dest, setID, 1)
	tree2 := destImageDir(dest, setID, 2)
	iso2 := filepath.Join(dest, setID+"-002.iso")

	// The flow's own discipline: read the inventory from a TEMP COPY of the
	// export (never from the media's file directly).
	info, err := restore.ResolveMedia(dest)
	if err != nil {
		t.Fatalf("ResolveMedia: %v", err)
	}
	man2, ok := info.Manifests[2]
	if !ok {
		t.Fatalf("fixture anomaly: sibling-mode media carries no manifest for image 2")
	}
	blob, err := os.ReadFile(info.ExportPath)
	if err != nil {
		t.Fatalf("reading the export: %v", err)
	}
	exportCopy := filepath.Join(t.TempDir(), media.ExportFileName)
	if err := os.WriteFile(exportCopy, blob, 0o600); err != nil {
		t.Fatalf("writing the export copy: %v", err)
	}
	db, err := catalog.Open(exportCopy, catalog.OpenReadOnly)
	if err != nil {
		t.Fatalf("opening the export copy: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// REAL image-2 chunk rows out of the FIRST-RUN export: every hash NULL,
	// so the seat's fill is the only anchor these rows can get.
	inv, err := (&restoreStore{db: db}).ImageInventory(t.Context(), setID, 2)
	if err != nil {
		t.Fatalf("ImageInventory(2): %v", err)
	}
	if len(inv.Chunks) == 0 {
		t.Fatalf("fixture anomaly: image 2's inventory carries no chunks (nothing to fill)")
	}
	for _, c := range inv.Chunks {
		if len(c.Hash) != 0 {
			t.Fatalf("fixture anomaly: first-run export chunk %d of %q carries a %d-byte hash, want NULL",
				c.ChunkIndex, c.ParentRelPath, len(c.Hash))
		}
	}

	// One selection per case: a single work list for image 2 carrying the
	// fresh NULL-hashed rows.
	selection := func() restore.ResolvedSelection {
		return restore.ResolvedSelection{
			SetID: setID,
			Work: []restore.ImageWork{{
				Image: 2,
				Inv:   restore.Inventory{Chunks: append([]restore.ChunkItem(nil), inv.Chunks...)},
			}},
		}
	}
	// run drives the seat with one supply and returns what the inner
	// restorer received.
	run := func(t *testing.T, sup restore.ImageSupply, sel restore.ResolvedSelection) restore.ResolvedSelection {
		t.Helper()
		capture := &sidecarCaptureRestorer{}
		if _, err := newSidecarManifestRestorer(capture).RestoreImage(t.Context(), sup, sel, t.TempDir()); err != nil {
			t.Fatalf("RestoreImage: %v", err)
		}
		if capture.got == nil {
			t.Fatal("the seat never reached the inner restorer (it must never abort a run)")
		}
		return *capture.got
	}
	assertAllFilled := func(t *testing.T, chunks []restore.ChunkItem, wantFilled bool) {
		t.Helper()
		for _, c := range chunks {
			switch wantFilled {
			case true:
				want, err := hex.DecodeString(manifestChunkHash(t, man2, c.ParentRelPath, c.ChunkIndex))
				if err != nil {
					t.Fatalf("decoding the manifest chunk hash: %v", err)
				}
				if !bytes.Equal(c.Hash, want) {
					t.Errorf("chunk %d of %q hash = %x, want the resolved tree's manifest hash %x",
						c.ChunkIndex, c.ParentRelPath, c.Hash, want)
				}
			case false:
				if len(c.Hash) != 0 {
					t.Errorf("chunk %d of %q hash = %x, want EMPTY (no anchor may be invented here)",
						c.ChunkIndex, c.ParentRelPath, c.Hash)
				}
			}
		}
	}

	t.Run("the resolved copy-tree directory's sidecar fills every NULL row", func(t *testing.T) {
		sel := selection()
		got := run(t, restore.ImageSupply{SetID: setID, Image: 2, Kind: restore.SourceCopyTree, Path: tree2}, sel)
		assertAllFilled(t, got.Work[0].Inv.Chunks, true)
	})

	t.Run("an ISO supply is never consulted", func(t *testing.T) {
		sel := selection()
		got := run(t, restore.ImageSupply{SetID: setID, Image: 2, Kind: restore.SourceISO, Path: iso2}, sel)
		assertAllFilled(t, got.Work[0].Inv.Chunks, false)
	})

	t.Run("a sidecar claiming another image is not an anchor", func(t *testing.T) {
		// tree1's MANIFEST.json is IMAGE 1's record: identity-checked
		// against image 2 it must be refused - opportunistic, never fatal.
		sel := selection()
		got := run(t, restore.ImageSupply{SetID: setID, Image: 2, Kind: restore.SourceCopyTree, Path: tree1}, sel)
		assertAllFilled(t, got.Work[0].Inv.Chunks, false)
	})

	t.Run("a sidecar claiming another set is not an anchor", func(t *testing.T) {
		sel := selection()
		got := run(t, restore.ImageSupply{SetID: "S-INTRUDER", Image: 2, Kind: restore.SourceCopyTree, Path: tree2}, sel)
		assertAllFilled(t, got.Work[0].Inv.Chunks, false)
	})

	t.Run("an export-anchored row is never overwritten", func(t *testing.T) {
		sel := selection()
		exportHash := bytes.Repeat([]byte{0x42}, 32)
		sel.Work[0].Inv.Chunks[0].Hash = exportHash
		got := run(t, restore.ImageSupply{SetID: setID, Image: 2, Kind: restore.SourceCopyTree, Path: tree2}, sel)
		if !bytes.Equal(got.Work[0].Inv.Chunks[0].Hash, exportHash) {
			t.Errorf("the export-anchored row changed to %x, want the export's %x (the export always wins)",
				got.Work[0].Inv.Chunks[0].Hash, exportHash)
		}
		assertAllFilled(t, got.Work[0].Inv.Chunks[1:], true)
	})

	t.Run("the caller's selection is never mutated", func(t *testing.T) {
		sel := selection()
		_ = run(t, restore.ImageSupply{SetID: setID, Image: 2, Kind: restore.SourceCopyTree, Path: tree2}, sel)
		for _, c := range sel.Work[0].Inv.Chunks {
			if len(c.Hash) != 0 {
				t.Errorf("the seat mutated the CALLER's row: chunk %d of %q hash = %d bytes",
					c.ChunkIndex, c.ParentRelPath, len(c.Hash))
			}
		}
	})

	t.Run("an image the selection does not carry is a no-op", func(t *testing.T) {
		sel := restore.ResolvedSelection{SetID: setID} // no Work at all
		got := run(t, restore.ImageSupply{SetID: setID, Image: 2, Kind: restore.SourceCopyTree, Path: tree2}, sel)
		if len(got.Work) != 0 {
			t.Errorf("the seat invented work rows: %d", len(got.Work))
		}
	})
}
