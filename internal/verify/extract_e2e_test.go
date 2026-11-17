//go:build e2e

package verify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"isopress/internal/xorriso"
)

// TestExtractAndHashRealRoundTrip masters a fixture ISO with the REAL
// xorriso (gated on LookPath per DESIGN_SPEC 12.6) and asserts the
// extraction round-trips it: files hashed byte-exact, the symlink
// restored as a symlink, the empty dir present, the image-root members
// extracted by the whole-tree shape.
func TestExtractAndHashRealRoundTrip(t *testing.T) {
	tool := realXorrisoOrSkip(t)
	dir := t.TempDir()

	stage := filepath.Join(dir, "stage")
	for _, d := range []string{"files/sub", "chunks/big.bin", "files/empty"} {
		if err := os.MkdirAll(filepath.Join(stage, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	payload := map[string]string{
		"files/a.txt":           "hello world",
		"files/sub/deep.txt":    "deep down",
		"chunks/big.bin/00001":  "chunk one",
		"chunks/big.bin/00002":  "chunk two!",
		"MANIFEST.json":         `{"set_id":"S-V"}`,
		"catalog-export.sqlite": "not really sqlite",
	}
	for rel, content := range payload {
		if err := os.WriteFile(filepath.Join(stage, filepath.FromSlash(rel)), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("a.txt", filepath.Join(stage, "files", "link")); err != nil {
		t.Fatal(err)
	}

	var pathlist strings.Builder
	for rel := range payload {
		pathlist.WriteString("/" + rel + "=" + filepath.Join(stage, filepath.FromSlash(rel)) + "\n")
	}
	pathlist.WriteString("/files/empty=" + filepath.Join(stage, "files", "empty") + "\n")
	plPath := filepath.Join(dir, "pathlist.txt")
	if err := os.WriteFile(plPath, []byte(pathlist.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	linkLine := "/files/link=" + filepath.Join(stage, "files", "link") + "\n"
	f, err := os.OpenFile(plPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(linkLine); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	iso := filepath.Join(dir, "fixture.iso")
	if _, err := tool.Run(context.Background(), []string{
		"-as", "mkisofs", "-iso-level", "3", "-J", "-V", "S-V",
		"-o", iso, "-graft-points", "-path-list", plPath,
	}); err != nil {
		t.Fatalf("mastering the fixture ISO: %v", err)
	}

	dest := filepath.Join(dir, "extract")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := ExtractAndHash(context.Background(), tool, iso, dest, nil, Totals{})
	if err != nil {
		t.Fatalf("ExtractAndHash: %v", err)
	}

	byPath := map[string]Entry{}
	for _, e := range res.Entries {
		byPath[e.MediaPath] = e
	}
	for rel, content := range payload {
		e, ok := byPath[rel]
		if !ok || e.Kind != KindFile {
			t.Errorf("%s missing or not a file: %+v", rel, e)
			continue
		}
		sum := sha256.Sum256([]byte(content))
		if !bytes.Equal(e.Hash, sum[:]) || e.Size != int64(len(content)) {
			t.Errorf("%s = (size %d, hash %x), want (%d, %x)", rel, e.Size, e.Hash, len(content), sum)
		}
	}
	link, ok := byPath["files/link"]
	if !ok || link.Kind != KindSymlink || link.LinkTarget != "a.txt" {
		t.Errorf("symlink round-trip = %+v, want a symlink to a.txt", link)
	}
	if e, ok := byPath["files/empty"]; !ok || e.Kind != KindDir {
		t.Errorf("empty dir round-trip = %+v, want a dir entry", e)
	}
	if res.FileCount != len(payload) {
		t.Errorf("FileCount = %d, want %d", res.FileCount, len(payload))
	}
}

// TestExtractAndHashRealSymlinkVariants pins the OSIRROX SYMLINK
// CONTRACT the doc claims (extract.go file doc "osirrox restores
// symlinks as symlinks - verified") at the PRIMITIVE tier with the
// three adversarial variants: a DANGLING link (target absent from the
// image), a link to a real DIRECTORY, and an ABSOLUTE-target link.
// Real xorriso 1.5.6 restores all three as symlinks with their exact
// targets - nothing is dropped, dereferenced, materialized as a dir,
// or rewritten - and the walk records each as KindSymlink without
// following (a to-dir link must NOT appear as a dir or pull its target
// into the walk). If a future xorriso changes this behavior, this test
// fails loudly before a backup operator discovers it in restore.
func TestExtractAndHashRealSymlinkVariants(t *testing.T) {
	tool := realXorrisoOrSkip(t)
	dir := t.TempDir()

	stage := filepath.Join(dir, "stage", "files")
	if err := os.MkdirAll(filepath.Join(stage, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "real.txt"), []byte("real"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "sub", "x.txt"), []byte("sub"), 0o644); err != nil {
		t.Fatal(err)
	}
	links := []struct{ name, target string }{
		{"link-ok", "real.txt"},
		{"link-dangling", "missing.txt"},
		{"link-todir", "sub"},
		{"link-abs", "/etc/passwd"},
	}
	var pathlist strings.Builder
	fmt.Fprintf(&pathlist, "/files/real.txt=%s\n/files/sub/x.txt=%s\n",
		filepath.Join(stage, "real.txt"), filepath.Join(stage, "sub", "x.txt"))
	for _, l := range links {
		if err := os.Symlink(l.target, filepath.Join(stage, l.name)); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(&pathlist, "/files/%s=%s\n", l.name, filepath.Join(stage, l.name))
	}
	plPath := filepath.Join(dir, "pathlist.txt")
	if err := os.WriteFile(plPath, []byte(pathlist.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	iso := filepath.Join(dir, "fixture.iso")
	if _, err := tool.Run(context.Background(), []string{
		"-as", "mkisofs", "-iso-level", "3", "-J", "-V", "S-SYM",
		"-o", iso, "-graft-points", "-path-list", plPath,
	}); err != nil {
		t.Fatalf("mastering the fixture ISO: %v", err)
	}

	dest := filepath.Join(dir, "extract")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := ExtractAndHash(context.Background(), tool, iso, dest, nil, Totals{})
	if err != nil {
		t.Fatalf("ExtractAndHash of the symlink-variant image: %v", err)
	}

	byPath := map[string]Entry{}
	for _, e := range res.Entries {
		byPath[e.MediaPath] = e
	}
	for _, l := range links {
		e, ok := byPath["files/"+l.name]
		if !ok || e.Kind != KindSymlink || e.LinkTarget != l.target {
			t.Errorf("link %s (target %q) extracted as %+v, want a symlink with the exact target", l.name, l.target, e)
			continue
		}
		// The on-disk extraction shape matches the recorded entry
		// (Lstat: never followed).
		fi, err := os.Lstat(filepath.Join(dest, "files", l.name))
		if err != nil {
			t.Fatalf("lstat extracted %s: %v", l.name, err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("extracted %s on disk = %v, want a symlink", l.name, fi.Mode())
		}
	}
	// The walk did NOT follow the to-dir link: "sub" appears exactly
	// once (as the real dir), and x.txt only under files/sub/.
	if e, ok := byPath["files/sub"]; !ok || e.Kind != KindDir {
		t.Errorf("files/sub = %+v, want the real dir entry (not swallowed by the link)", e)
	}
	if _, ok := byPath["files/link-todir/x.txt"]; ok {
		t.Error("the walk followed the to-dir symlink into its target (entries under files/link-todir/ must not exist)")
	}
	// Only the two regular files were HASHED (symlinks carry no
	// digest - the D-09 NULL-hash convention).
	if res.FileCount != 2 || res.TotalBytes != 4+3 {
		t.Errorf("result = (files %d, bytes %d), want (2, 7) - symlinks must not be hashed as files", res.FileCount, res.TotalBytes)
	}
}

// realXorrisoOrSkip gates the REAL tier on exec.LookPath (12.6).
func realXorrisoOrSkip(t *testing.T) *xorriso.Tool {
	t.Helper()
	if _, err := exec.LookPath("xorriso"); err != nil {
		t.Skip("xorriso not on PATH; skipping the gated extraction integration (DESIGN_SPEC 12.6)")
	}
	return &xorriso.Tool{Path: mustLookPath(t, "xorriso")}
}

func mustLookPath(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
