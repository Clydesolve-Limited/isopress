//go:build e2e

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"isopress/internal/report"
	"isopress/internal/verify"
	"isopress/internal/xorriso"
)

// TestBuildEndToEndRealXorriso (gated) drives the REAL command tree
// with the REAL xorriso: both images build, verify, and land with the
// full catalog contract (state 'built', mode 'iso', final_path,
// bytes_written, built_at; the image_contents rows including the
// manifest and export self-rows), the ISOs carry their payload (the
// extraction round-trips through verify.ExtractAndHash), and the
// report is the frozen JSON schema.
func TestBuildEndToEndRealXorriso(t *testing.T) {
	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skip("xorriso not on PATH; skipping the gated build e2e (DESIGN_SPEC 12.6)")
	}
	root := makeBuildFixtureTree(t)
	cat := createCatalog(t)
	const setID = "S-E2E"
	scanBuildFixture(t, root, cat, setID)
	planSynthetic(t, cat, setID, buildFixtureProfile)
	dest := filepath.Join(t.TempDir(), "dest")

	stdout, stderr, code := runBuildChild(t,
		"build", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json")
	if code != report.ExitOK {
		t.Fatalf("build exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	var rep buildReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("build --json does not parse: %v\n%s", err, stdout)
	}
	assertFrozenJSONKeys(t, stdout, []string{
		"built", "bytes", "dest", "failed", "images", "run_id", "set_id",
		"status", "verified_entries", "warnings",
	})
	if rep.Status != "ok" || rep.Built != 2 || rep.Failed != 0 || rep.VerifiedEntries == 0 {
		t.Fatalf("report = %+v, want two clean verified builds", rep)
	}

	// The atomic finalize on disk: both final names, no .partial.
	for _, n := range []int64{1, 2} {
		iso := filepath.Join(dest, fmt.Sprintf("%s-%03d.iso", setID, n))
		if _, err := os.Stat(iso); err != nil {
			t.Fatalf("image %d final .iso missing: %v", n, err)
		}
		if _, err := os.Stat(iso + ".partial"); !os.IsNotExist(err) {
			t.Fatalf("image %d left a .partial behind: %v", n, err)
		}
	}

	// The catalog contract (6.3 step 3, 2.3).
	db := catOpen(t, cat)
	for _, n := range []int64{1, 2} {
		var (
			state, finalPath, mode string
			builtAt                sql.NullInt64
			bytesWritten           sql.NullInt64
		)
		if err := db.QueryRow(`SELECT state, final_path, bytes_written, built_at, mode
			FROM images WHERE set_id = ? AND image_id = ?`, setID, n).
			Scan(&state, &finalPath, &bytesWritten, &builtAt, &mode); err != nil {
			t.Fatalf("reading images row %d: %v", n, err)
		}
		if state != "built" || mode != "iso" || !builtAt.Valid || !bytesWritten.Valid || bytesWritten.Int64 == 0 {
			t.Errorf("images row %d = (state %q, mode %q, built_at %v, bytes %v), want (built, iso, stamped, >0)",
				n, state, mode, builtAt, bytesWritten)
		}
		if want := fmt.Sprintf("%s-%03d.iso", setID, n); !strings.HasSuffix(finalPath, want) {
			t.Errorf("images row %d final_path = %q, want it to end with %q", n, finalPath, want)
		}

		// The 6.6 content list: payload rows plus the manifest and
		// export self-rows.
		contents, err := readImageContents(db, setID, n)
		if err != nil {
			t.Fatal(err)
		}
		if len(contents) == 0 {
			t.Fatalf("image %d has no image_contents rows", n)
		}
		kinds := map[string]int{}
		for _, r := range contents {
			kinds[r.kind]++
		}
		if kinds["manifest"] != 1 || kinds["export"] != 1 {
			t.Errorf("image %d self-rows = (manifest %d, export %d), want one each: %v",
				n, kinds["manifest"], kinds["export"], contents)
		}
		if kinds["file"] == 0 {
			t.Errorf("image %d has no payload file rows", n)
		}
	}

	// THE PAYLOAD ROUND-TRIP: extract each finalized ISO and re-hash
	// every content row against it (files, chunks, and the two self
	// rows by hash; symlinks by existence; dirs by existence) - the
	// 7.1/7.2 anchor through the verify primitive, driven by the
	// STORED content list rather than hardcoded expectations.
	tool, err := xorriso.Discover()
	if err != nil {
		t.Fatal(err)
	}
	var extractDirs []string
	for _, n := range []int64{1, 2} {
		contents, err := readImageContents(db, setID, n)
		if err != nil {
			t.Fatal(err)
		}
		iso := filepath.Join(dest, fmt.Sprintf("%s-%03d.iso", setID, n))
		ext := extractISO(t, tool, iso)
		extractDirs = append(extractDirs, ext)
		assertImageContentsRoundTrip(t, ext, contents)
	}

	// FR-035 DIRECT: the run's SINGLE export embeds BYTE-IDENTICALLY
	// into every image of the run - compare the two extracted
	// catalog-export.sqlite members directly (the transitive proof
	// above pins each against the catalog row; this pins them against
	// EACH OTHER).
	exp0, err := os.ReadFile(filepath.Join(extractDirs[0], "catalog-export.sqlite"))
	if err != nil {
		t.Fatalf("reading image 1's extracted export: %v", err)
	}
	exp1, err := os.ReadFile(filepath.Join(extractDirs[1], "catalog-export.sqlite"))
	if err != nil {
		t.Fatalf("reading image 2's extracted export: %v", err)
	}
	if !bytes.Equal(exp0, exp1) {
		t.Errorf("the two images embed DIFFERENT catalog exports (%d vs %d bytes, digests %x vs %x); FR-035 requires the run's single export verbatim in every image",
			len(exp0), len(exp1), sha256.Sum256(exp0), sha256.Sum256(exp1))
	}

	// The hard-link family is recorded (D-04) even though only the
	// primary's bytes land on media.
	var groups int
	if err := db.QueryRow(`SELECT COUNT(*) FROM image_contents
		WHERE set_id = ? AND kind IN ('manifest','export')`, setID).Scan(&groups); err != nil {
		t.Fatal(err)
	}
	if groups < 4 { // both images, both self-kinds each
		t.Errorf("manifest+export self-rows across the set = %d, want >= 4", groups)
	}
}

// TestBuildEndToEndPartialFailureRealXorriso (gated) pins the N3
// partial semantics through the REAL tree: a source file of image 2 is
// MUTATED after the plan, so image 2 fails with the typed
// SourceChangedError (staging's 3.5 point 3 gate), image 1 still
// builds and verifies, the run exits 10, and image 2's state stays
// 'planned' (its build failed BEFORE the 6.6 transaction - the
// documented pre-begin failure contract).
func TestBuildEndToEndPartialFailureRealXorriso(t *testing.T) {
	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skip("xorriso not on PATH; skipping the gated partial-failure e2e (DESIGN_SPEC 12.6)")
	}
	root := makeBuildFixtureTree(t)
	cat := createCatalog(t)
	const setID = "S-PART"
	scanBuildFixture(t, root, cat, setID)
	planSynthetic(t, cat, setID, buildFixtureProfile)

	// Pick the file of the SECOND planned image adaptively (the WFD
	// distribution is deterministic but the test asserts the CONTRACT,
	// not the balancer's packing): query which image holds b.bin, then
	// mutate it (size + mtime change, the copy_test.go source-changed
	// shape).
	db0 := catOpen(t, cat)
	var failImage int64
	if err := db0.QueryRow(`SELECT image_id FROM files WHERE set_id = ? AND relpath = 'b.bin'`, setID).
		Scan(&failImage); err != nil {
		t.Fatalf("locating b.bin's image: %v", err)
	}
	if failImage == 0 {
		t.Fatal("b.bin is unassigned; the fixture plan must span two images")
	}
	writeFileP(t, filepath.Join(root, "b.bin"), pattern(9, 20<<20+1234), 0o644)

	dest := filepath.Join(t.TempDir(), "dest")
	stdout, stderr, code := runBuildChild(t,
		"build", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json")
	if code != report.ExitItemFailures {
		t.Fatalf("build exit = %d, want 10 (partial)\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	var rep buildReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("build --json does not parse: %v\n%s", err, stdout)
	}
	if rep.Status != "partial" || rep.Built != 1 || rep.Failed != 1 {
		t.Fatalf("report = %+v, want image 1 built and image 2 failed", rep)
	}
	byImage := map[int64]buildImageReport{}
	for _, img := range rep.Images {
		byImage[img.Image] = img
	}
	builtImage := int64(3) - failImage // the fixture spans exactly two images
	if byImage[builtImage].Status != "built" {
		t.Errorf("image %d report = %+v, want built", builtImage, byImage[builtImage])
	}
	if byImage[failImage].Status != "failed" || !strings.Contains(byImage[failImage].Failure, "changed since scan") {
		t.Errorf("image %d report = %+v, want the source-changed failure", failImage, byImage[failImage])
	}

	// The surviving image is complete and UNCORRUPTED; the failed one
	// produced neither a final .iso nor a .partial (its failure
	// predates mastering).
	if _, err := os.Stat(filepath.Join(dest, fmt.Sprintf("%s-%03d.iso", setID, builtImage))); err != nil {
		t.Fatalf("image %d .iso missing: %v", builtImage, err)
	}
	if _, err := os.Stat(filepath.Join(dest, fmt.Sprintf("%s-%03d.iso", setID, failImage))); !os.IsNotExist(err) {
		t.Errorf("image %d produced a final .iso despite failing: %v", failImage, err)
	}
	if _, err := os.Stat(filepath.Join(dest, fmt.Sprintf("%s-%03d.iso.partial", setID, failImage))); !os.IsNotExist(err) {
		t.Errorf("image %d produced a .partial despite failing at staging: %v", failImage, err)
	}

	db := catOpen(t, cat)
	var sBuilt, sFailed string
	if err := db.QueryRow(`SELECT state FROM images WHERE set_id = ? AND image_id = ?`, setID, builtImage).Scan(&sBuilt); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT state FROM images WHERE set_id = ? AND image_id = ?`, setID, failImage).Scan(&sFailed); err != nil {
		t.Fatal(err)
	}
	if sBuilt != "built" || sFailed != "planned" {
		t.Errorf("image states = (%q, %q) for (built %d, failed %d), want (built, planned) - a pre-begin failure never leaves 'planned'",
			sBuilt, sFailed, builtImage, failImage)
	}
	var status string
	if err := db.QueryRow(`SELECT status FROM runs WHERE kind = 'build' AND set_id = ?`, setID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "partial" {
		t.Errorf("run status = %q, want 'partial'", status)
	}
}

// --- helpers --------------------------------------------------------------------

// contentRow is one image_contents row as the round-trip helper reads
// it (path, kind, size, and the hex hash - NULL for dirs/symlinks).
type contentRow struct {
	kind string
	size int64
	hash string // lowercase hex; "" for NULL
}

// readImageContents returns one image's content rows keyed by media
// path.
func readImageContents(db *sql.DB, setID string, imageID int64) (map[string]contentRow, error) {
	rows, err := db.Query(`SELECT media_path, kind, size, COALESCE(hex(hash), '') FROM image_contents
		WHERE set_id = ? AND image_id = ? ORDER BY media_path`, setID, imageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]contentRow{}
	for rows.Next() {
		var path string
		var r contentRow
		if err := rows.Scan(&path, &r.kind, &r.size, &r.hash); err != nil {
			return nil, err
		}
		out[path] = r
	}
	return out, rows.Err()
}

// assertImageContentsRoundTrip re-hashes every content row of ONE
// image against its extraction: hashed kinds (file, chunk, manifest,
// export) by sha256 and size, symlinks by existence as a symlink, dirs
// by existence. SQLite hex() is uppercase, so digests compare
// uppercased.
func assertImageContentsRoundTrip(t *testing.T, extractRoot string, contents map[string]contentRow) {
	t.Helper()
	if len(contents) == 0 {
		t.Fatal("no content rows to round-trip")
	}
	for rel, want := range contents {
		path := filepath.Join(extractRoot, filepath.FromSlash(rel))
		switch want.kind {
		case "dir":
			if fi, err := os.Lstat(path); err != nil || !fi.IsDir() {
				t.Errorf("%s: extracted as %v, want a dir", rel, err)
			}
		case "symlink":
			if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Errorf("%s: extracted as %v, want a symlink", rel, err)
			}
		default: // file, chunk, manifest, export
			got, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("%s: missing from the extraction: %v", rel, err)
				continue
			}
			sum := sha256.Sum256(got)
			if strings.ToUpper(fmt.Sprintf("%x", sum)) != want.hash {
				t.Errorf("%s: extracted hash %x does not match the content row %s", rel, sum, want.hash)
			}
			if int64(len(got)) != want.size {
				t.Errorf("%s: extracted size %d, content row says %d", rel, len(got), want.size)
			}
		}
	}
}

// extractISO extracts an ISO through the verify primitive (the T4.1
// tool, discovered from PATH) into a fresh dir.
func extractISO(t *testing.T, tool *xorriso.Tool, iso string) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := verify.ExtractAndHash(context.Background(), tool, iso, dir, nil, verify.Totals{}); err != nil {
		t.Fatalf("extracting %s: %v", iso, err)
	}
	return dir
}
