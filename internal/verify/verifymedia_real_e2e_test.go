//go:build e2e

package verify

// T5.1: the GATED REAL-xorriso tier of verify-media (DESIGN_SPEC 12.6,
// 7.2; the plan's acceptance corruption test). Skipped unless xorriso
// is on PATH; on this dev machine the real binary (1.5.6) runs them.
//
// The corruption recipe is the T4.3 STRENGTHENED precedent
// (imagebuild/build_real_test.go TestBuildRealFileDataCorruptionFailsHashWalk):
// locate a payload's data extent by its UNIQUE bytes (uniqueness
// asserted - a second occurrence fails the test), flip ONE byte INSIDE
// it, so the ISO stays fully parseable and every structure (PVD,
// directory records, path tables, Rock Ridge) is untouched. Detection
// must then come from the HASH WALK naming exactly that file - an
// extraction failure here would be the wrong kind of right.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"isopress/internal/media"
	"isopress/internal/xorriso"
)

// verifyRealPayload is the deliberately unique content of files/a.txt
// in the real fixture (the corruption flip targets its data extent).
const verifyRealPayload = "verify-media REAL fixture payload :: 0123456789abcdef :: unique-bytes-marker\n"

// masterVerifyFixtureISO masters the real fixture ISO with the REAL
// xorriso (-as mkisofs + -graft-points, the extract_test shape) and
// returns the image path plus the matching content list.
func masterVerifyFixtureISO(t *testing.T, tool *xorriso.Tool, dir string) (iso string, list ContentList) {
	t.Helper()
	stage := filepath.Join(dir, "stage")
	for _, d := range []string{"files/sub", "chunks/big.bin", "files/empty"} {
		if err := os.MkdirAll(filepath.Join(stage, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	payload := map[string]string{
		"files/a.txt":          verifyRealPayload,
		"files/sub/deep.txt":   "deep down\n",
		"chunks/big.bin/00001": "chunk one",
		"chunks/big.bin/00002": "chunk two!",
		media.ManifestFileName: `{"set_id":"S-REALV"}`,
		media.ExportFileName:   "fake export bytes",
	}
	for rel, content := range payload {
		if err := os.WriteFile(filepath.Join(stage, filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The pinned osirrox symlink variants (dangling / to-dir /
	// absolute) - verify-media matches them by TARGET.
	links := []struct{ name, target string }{
		{"link-ok", "a.txt"},
		{"link-dangling", "missing.txt"},
		{"link-todir", "sub"},
		{"link-abs", "/etc/passwd"},
	}
	var pathlist strings.Builder
	for rel := range payload {
		pathlist.WriteString("/" + rel + "=" + filepath.Join(stage, filepath.FromSlash(rel)) + "\n")
	}
	pathlist.WriteString("/files/empty=" + filepath.Join(stage, "files", "empty") + "\n")
	for _, l := range links {
		if err := os.Symlink(l.target, filepath.Join(stage, "files", l.name)); err != nil {
			t.Fatal(err)
		}
		pathlist.WriteString("/files/" + l.name + "=" + filepath.Join(stage, "files", l.name) + "\n")
	}
	plPath := filepath.Join(dir, "pathlist.txt")
	if err := os.WriteFile(plPath, []byte(pathlist.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	iso = filepath.Join(dir, "S-REALV-001.iso")
	if _, err := tool.Run(context.Background(), []string{
		"-as", "mkisofs", "-iso-level", "3", "-J", "-V", "S-REALV",
		"-o", iso, "-graft-points", "-path-list", plPath,
	}); err != nil {
		t.Fatalf("mastering the fixture ISO: %v", err)
	}

	// The content list the catalog would hold (the walk hashes the
	// same bytes the master wrote).
	list.SymlinkTargets = map[string]string{}
	for rel, content := range payload {
		kind := media.KindFile
		switch {
		case rel == media.ManifestFileName:
			kind = KindManifest
		case rel == media.ExportFileName:
			kind = KindExport
		case strings.HasPrefix(rel, "chunks/"):
			kind = media.KindChunk
		}
		list.Rows = append(list.Rows, ContentRow{
			MediaPath: rel, Kind: kind, ChunkIndex: -1,
			Size: int64(len(content)), Hash: hashOfBytes([]byte(content)),
		})
	}
	for _, l := range links {
		list.Rows = append(list.Rows, ContentRow{
			MediaPath: "files/" + l.name, Kind: media.KindSymlink, ChunkIndex: -1,
			Size: int64(len(l.target)),
		})
		list.SymlinkTargets["files/"+l.name] = l.target
	}
	list.Rows = append(list.Rows, ContentRow{MediaPath: "files/empty", Kind: media.KindDir, ChunkIndex: -1})
	return iso, list
}

// TestVerifyRealISOCleanThenTargetedCorruption is the plan's
// acceptance pair on the real binary: a clean real ISO FULL-matches and
// stamps; a byte flipped INSIDE a payload's unique data extent is
// detected by the hash walk as exactly that one file's content-hash
// mismatch - everything else still matches, and no stamp is written.
func TestVerifyRealISOCleanThenTargetedCorruption(t *testing.T) {
	tool := realXorrisoOrSkip(t)
	dir := t.TempDir()
	iso, list := masterVerifyFixtureISO(t, tool, dir)

	st := newMemStore()
	st.lists[1] = list
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	deps := Deps{Store: st, Tool: tool}

	// 1) CLEAN: full match, stamp, scratch cleaned.
	res, err := VerifyImage(context.Background(), deps, VerifyInput{
		SetID: "S-REALV", ImageID: 1, Path: iso, WorkDir: work,
	})
	if err != nil {
		t.Fatalf("VerifyImage (clean): %v", err)
	}
	if res.Source != SourceISO || !res.Comparison.Clean() || !res.Stamped {
		t.Fatalf("clean result = (source %s, clean %v, stamped %v), want (iso, true, true): %v",
			res.Source, res.Comparison.Clean(), res.Stamped, res.Comparison.Problems())
	}
	assertDirEmpty(t, work)

	// 2) THE TARGETED CORRUPTION (T4.3 precedent): locate the payload
	// in the IMAGE BYTES, assert uniqueness, flip ONE byte inside it.
	img, err := os.ReadFile(iso)
	if err != nil {
		t.Fatal(err)
	}
	needle := []byte(verifyRealPayload)
	first := bytes.Index(img, needle)
	if first < 0 {
		t.Fatalf("payload %q not found in the image", verifyRealPayload)
	}
	if bytes.Index(img[first+1:], needle) >= 0 {
		t.Fatalf("payload %q is not unique in the image; the targeted flip cannot pin a data region", verifyRealPayload)
	}
	f, err := os.OpenFile(iso, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt([]byte{0xff}, int64(first+3)); err != nil {
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

	res2, err := VerifyImage(context.Background(), deps, VerifyInput{
		SetID: "S-REALV", ImageID: 1, Path: iso, WorkDir: work,
	})
	if err != nil {
		// The image must stay PARSEABLE: a detection from a hash
		// mismatch is the point of the targeted flip.
		t.Fatalf("VerifyImage (corrupted): %v (detection must come from the hash walk, not an extraction failure)", err)
	}
	if res2.Comparison.Clean() || res2.Stamped {
		t.Fatalf("corrupted result = (clean %v, stamped %v), want (false, false)", res2.Comparison.Clean(), res2.Stamped)
	}
	if len(res2.Comparison.Mismatched) != 1 || res2.Comparison.Mismatched[0].MediaPath != "files/a.txt" ||
		!strings.Contains(res2.Comparison.Mismatched[0].Reasons[0], "content hash") {
		t.Errorf("mismatched = %+v, want EXACTLY files/a.txt by content hash", res2.Comparison.Mismatched)
	}
	if len(res2.Comparison.Missing) != 0 || len(res2.Comparison.Unexpected) != 0 || len(res2.Comparison.Unreadable) != 0 {
		t.Errorf("other categories = (%v, %v, %v), want empty - the rest of the image is verified",
			res2.Comparison.Missing, res2.Comparison.Unexpected, res2.Comparison.Unreadable)
	}
	if len(res2.Comparison.Matched) != len(list.Rows)-1 {
		t.Errorf("matched %d entries, want %d (every listed member but the corrupted file)",
			len(res2.Comparison.Matched), len(list.Rows)-1)
	}
	// Only the clean run ever stamped.
	if st.stampCount() != 1 || st.stamps[0].at != res.StampAt {
		t.Errorf("stamps = %+v, want exactly the clean run's stamp", st.stamps)
	}
	assertDirEmpty(t, work)
}
