//go:build e2e

package cli

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"isopress/internal/report"
)

// verifyISOTree writes a small fixture with a UNIQUE payload file (the
// targeted-corruption needle).
func verifyISOTree(t *testing.T) (root, payload string) {
	t.Helper()
	root = t.TempDir()
	payload = "verify-media CLI unique payload :: 9f8e7d6c5b4a3928 :: unique-bytes-marker\n"
	writeFileP(t, filepath.Join(root, "payload.txt"), []byte(payload), 0o644)
	writeFileP(t, filepath.Join(root, "small2.txt"), []byte("second file\n"), 0o644)
	if err := os.Symlink("payload.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "empty-dir"), 0o750); err != nil {
		t.Fatal(err)
	}
	return root, payload
}

// TestVerifyMediaRealISOCorruptionDetectedThenClean is the plan's
// acceptance pair through the REAL command tree and the REAL xorriso:
// a built ISO with a byte flipped INSIDE its unique payload's data
// extent (the T4.3 precedent - the image stays parseable, so detection
// provably comes from the Go hash walk) reports exactly that file as
// mismatched, exits 10, and leaves the state 'built' with no
// verified_at; the restored byte then full-matches and stamps
// verified.
func TestVerifyMediaRealISOCorruptionDetectedThenClean(t *testing.T) {
	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skip("xorriso not on PATH; skipping the gated verify-media ISO acceptance (DESIGN_SPEC 12.6)")
	}
	const setID = "S-VREAL"
	root, payload := verifyISOTree(t)
	cat := createCatalog(t)
	scanBuildFixture(t, root, cat, setID)
	planSynthetic(t, cat, setID, 25<<20)
	dest := filepath.Join(t.TempDir(), "dest")
	if stdout, stderr, code := runBuildChild(t,
		"build", "--catalog", cat, "--set-id", setID, "--dest", dest); code != report.ExitOK {
		t.Fatalf("build fixture exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	iso := filepath.Join(dest, fmt.Sprintf("%s-001.iso", setID))

	// THE TARGETED CORRUPTION: locate payload.txt's data extent by its
	// unique bytes and flip ONE byte inside it.
	img, err := os.ReadFile(iso)
	if err != nil {
		t.Fatal(err)
	}
	needle := []byte(payload)
	first := bytes.Index(img, needle)
	if first < 0 {
		t.Fatalf("payload %q not found in the image", payload)
	}
	if bytes.Index(img[first+1:], needle) >= 0 {
		t.Fatalf("payload %q is not unique in the image; the targeted flip cannot pin a data region", payload)
	}
	f, err := os.OpenFile(iso, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{0xff}, int64(first+5)); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// Corrupted: exit 10, EXACTLY payload.txt mismatched by content
	// hash, state stays 'built', verified_at stays NULL.
	stdout, stderr, code := runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID, "--json")
	if code != report.ExitItemFailures {
		t.Fatalf("corrupted ISO verify exit = %d, want 10\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	var rep verifyMediaReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("verify-media --json does not parse: %v\n%s", err, stdout)
	}
	if rep.Mismatched != 1 || rep.Verified != 0 || rep.Status != "partial" {
		t.Fatalf("report = %+v, want exactly one mismatch and no stamp", rep)
	}
	imgRep := rep.Images[0]
	if len(imgRep.Problems) != 1 ||
		!strings.Contains(imgRep.Problems[0], "files/payload.txt") ||
		!strings.Contains(imgRep.Problems[0], "content hash") {
		t.Errorf("image problems = %v, want exactly files/payload.txt by content hash", imgRep.Problems)
	}
	if imgRep.Matched == 0 {
		t.Errorf("the rest of the image must be verified (matched count 0):\n%+v", imgRep)
	}
	if imgRep.Source != "iso" || imgRep.Stamped {
		t.Errorf("image report = %+v, want an unstamped iso image", imgRep)
	}
	db := catOpen(t, cat)
	var state string
	var verifiedAt sql.NullInt64
	if err := db.QueryRow(`SELECT state, verified_at FROM images WHERE set_id = ? AND image_id = 1`, setID).
		Scan(&state, &verifiedAt); err != nil {
		t.Fatal(err)
	}
	if state != "built" || verifiedAt.Valid {
		t.Errorf("images row = (state %q, verified_at %v), want ('built', NULL) after a partial verify", state, verifiedAt)
	}

	// Restore the flipped byte (the original byte at that offset IS
	// the payload's own byte): the clean verify exits 0 and stamps.
	f2, err := os.OpenFile(iso, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f2.WriteAt(needle[5:6], int64(first+5)); err != nil {
		_ = f2.Close()
		t.Fatal(err)
	}
	if err := f2.Close(); err != nil {
		t.Fatal(err)
	}

	stdout, _, code = runVerifyMediaChild(t, nil,
		"verify-media", "--catalog", cat, "--set-id", setID)
	if code != report.ExitOK {
		t.Fatalf("clean ISO verify exit = %d, want 0\nstdout: %s", code, stdout)
	}
	if !strings.Contains(stdout, "image 1: verified") || !strings.Contains(stdout, "(iso)") {
		t.Errorf("stdout lacks the verified iso line:\n%s", stdout)
	}
	if err := db.QueryRow(`SELECT state, verified_at FROM images WHERE set_id = ? AND image_id = 1`, setID).
		Scan(&state, &verifiedAt); err != nil {
		t.Fatal(err)
	}
	if state != "verified" || !verifiedAt.Valid || verifiedAt.Int64 <= 0 {
		t.Errorf("images row = (state %q, verified_at %v), want the 7.2 stamp after a full match", state, verifiedAt)
	}
}

// --- helpers ---------------------------------------------------------------------------
