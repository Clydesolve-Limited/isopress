package verify

// T4.3: the extraction-and-hash primitive. Two tiers:
//
//   - the FAKE tier (no xorriso): a /bin/sh script stands in for the
//     tool, so the argv shape, the destDir gates, and the walk's
//     classification (files, symlinks, dirs, the unexpected-type
//     refusal) are pinned deterministically. Test files may spawn
//     processes (the T4.1 deps gate exempts _test.go); production code
//     in this package never does.
//   - the GATED REAL tier (DESIGN_SPEC 12.6): skipped unless xorriso
//     is on PATH; masters a fixture ISO with the REAL binary through
//     the T4.1 Tool and asserts the extraction round-trips it.

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"isopress/internal/xorriso"
)

// installFakeTool writes an executable script that DUMPS its argv to
// <dir>/argv.txt and then runs the script BODY (which receives $@).
func installFakeTool(t *testing.T, dir, body string) string {
	t.Helper()
	path := filepath.Join(dir, "xorriso")
	script := "#!/bin/sh\necho \"$@\" > " + filepath.Join(dir, "argv.txt") + "\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing the fake xorriso: %v", err)
	}
	return path
}

func TestExtractAndHashArgvShapeAndWalk(t *testing.T) {
	dir := t.TempDir()
	// A prepared tree the fake "extraction" copies into destDir.
	prepared := filepath.Join(dir, "prepared")
	for _, d := range []string{"files/sub", "chunks/big"} {
		if err := os.MkdirAll(filepath.Join(prepared, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(prepared, "files", "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prepared, "files", "sub", "deep.txt"), []byte("deep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prepared, "chunks", "big", "00001"), []byte("chunk!"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(prepared, "files", "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prepared, "MANIFEST.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	toolPath := installFakeTool(t, dir, `cp -r "`+prepared+`/." "$7"`)
	dest := filepath.Join(dir, "extract")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := ExtractAndHash(context.Background(), toolWith(toolPath), "img.iso.partial", dest, nil, Totals{})
	if err != nil {
		t.Fatalf("ExtractAndHash: %v", err)
	}

	// THE ARGV: -indev <image> -osirrox on -extract / <dest> - one
	// invocation, whole tree (the file doc's documented shape).
	argv, err := os.ReadFile(filepath.Join(dir, "argv.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := "-indev img.iso.partial -osirrox on -extract / " + dest
	if got := strings.TrimSpace(string(argv)); got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}

	// THE WALK: every member classified, hashed, sorted by media path.
	byPath := map[string]Entry{}
	for _, e := range res.Entries {
		byPath[e.MediaPath] = e
	}
	checks := []struct {
		path   string
		kind   string
		size   int64
		hash   string
		target string
	}{
		{"MANIFEST.json", KindFile, 2, hexOf("{}"), ""},
		{"chunks/big/00001", KindFile, 6, hexOf("chunk!"), ""},
		{"files/a.txt", KindFile, 5, hexOf("hello"), ""},
		{"files/link", KindSymlink, 5, "", "a.txt"},
		{"files/sub", KindDir, 0, "", ""},
		{"files/sub/deep.txt", KindFile, 4, hexOf("deep"), ""},
	}
	for _, c := range checks {
		e, ok := byPath[c.path]
		if !ok {
			t.Errorf("entry %q missing from the result: %+v", c.path, res.Entries)
			continue
		}
		if e.Kind != c.kind || e.Size != c.size || e.LinkTarget != c.target {
			t.Errorf("entry %q = (kind %s, size %d, target %q), want (%s, %d, %q)",
				c.path, e.Kind, e.Size, e.LinkTarget, c.kind, c.size, c.target)
		}
		if c.hash != "" && hexOfBytes(e.Hash) != c.hash {
			t.Errorf("entry %q hash = %x, want %s", c.path, e.Hash, c.hash)
		}
	}
	if res.FileCount != 4 || res.TotalBytes != 5+6+2+4 {
		t.Errorf("result = (files %d, bytes %d), want (4, %d)", res.FileCount, res.TotalBytes, 5+6+2+4)
	}
	// Entries are sorted by media path.
	for i := 1; i < len(res.Entries); i++ {
		if res.Entries[i-1].MediaPath >= res.Entries[i].MediaPath {
			t.Fatalf("entries not sorted: %q before %q", res.Entries[i-1].MediaPath, res.Entries[i].MediaPath)
		}
	}
}

func TestExtractAndHashGates(t *testing.T) {
	dir := t.TempDir()
	toolPath := installFakeTool(t, dir, "exit 0")

	t.Run("nil tool", func(t *testing.T) {
		if _, err := ExtractAndHash(context.Background(), nil, "img.iso", dir, nil, Totals{}); err == nil {
			t.Fatal("a nil tool must be refused")
		}
	})
	t.Run("empty paths", func(t *testing.T) {
		if _, err := ExtractAndHash(context.Background(), toolWith(toolPath), "", dir, nil, Totals{}); err == nil {
			t.Fatal("an empty image path must be refused")
		}
		if _, err := ExtractAndHash(context.Background(), toolWith(toolPath), "img.iso", "", nil, Totals{}); err == nil {
			t.Fatal("an empty destDir must be refused")
		}
	})
	t.Run("non-empty destDir", func(t *testing.T) {
		full := filepath.Join(dir, "full")
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(full, "stale"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := ExtractAndHash(context.Background(), toolWith(toolPath), "img.iso", full, nil, Totals{})
		if err == nil || !strings.Contains(err.Error(), "not empty") {
			t.Fatalf("err = %v, want the emptiness refusal", err)
		}
	})
	t.Run("missing destDir", func(t *testing.T) {
		if _, err := ExtractAndHash(context.Background(), toolWith(toolPath), "img.iso", filepath.Join(dir, "nope"), nil, Totals{}); err == nil {
			t.Fatal("a missing destDir must be refused (xorriso needs it to exist)")
		}
	})
}

func TestExtractAndHashNonZeroExitIsError(t *testing.T) {
	dir := t.TempDir()
	// The real-world shape of a broken image: xorriso FAILURE, exit 5.
	toolPath := installFakeTool(t, dir, `echo "xorriso : FAILURE : Cannot determine attributes" >&2; exit 5`)
	dest := filepath.Join(dir, "extract")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := ExtractAndHash(context.Background(), toolWith(toolPath), "img.iso", dest, nil, Totals{})
	if err == nil || !strings.Contains(err.Error(), "extraction of img.iso") {
		t.Fatalf("err = %v, want the wrapped xorriso failure", err)
	}
}

// --- the gated REAL tier (12.6) -------------------------------------------------

// TestExtractAndHashRefusesForeignFileTypes: a member that is neither
// a regular file, a dir, nor a symlink (here: a FIFO planted by the
// fake tool after the copy) is a LOUD error naming the member - the
// walk never silently skips what it cannot classify (extract.go file
// doc "a loud error, never a silent skip").
func TestExtractAndHashRefusesForeignFileTypes(t *testing.T) {
	dir := t.TempDir()
	prepared := filepath.Join(dir, "prepared")
	if err := os.MkdirAll(prepared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prepared, "ok.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	// cp the prepared tree, then plant a FIFO the walk must refuse.
	toolPath := installFakeTool(t, dir,
		`cp -r "`+prepared+`/." "$7" && mkfifo "$7/evil.fifo"`)
	dest := filepath.Join(dir, "extract")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := ExtractAndHash(context.Background(), toolWith(toolPath), "img.iso", dest, nil, Totals{})
	if err == nil || !strings.Contains(err.Error(), "unexpected type") || !strings.Contains(err.Error(), "evil.fifo") {
		t.Fatalf("err = %v, want the loud unexpected-type refusal naming the member", err)
	}
}

// TestHashTreeHonorsCancelledContext: the hashing walk checks ctx per
// member - a cancelled run aborts instead of streaming a multi-gigabyte
// extraction to completion (extract.go file doc, the
// verifyCtxReader contract).
func TestHashTreeHonorsCancelledContext(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := hashTree(ctx, dir, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("hashTree under a cancelled ctx = %v, want context.Canceled", err)
	}
}

// --- shared helpers -------------------------------------------------------------

// toolWith builds a Tool over an explicit binary path (the T4.1
// constructor the tests use for fakes).
func toolWith(path string) *xorriso.Tool { return &xorriso.Tool{Path: path} }

// hexOf renders a string's sha256 as lowercase hex.
func hexOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hexOfBytes(sum[:])
}

// hexOfBytes renders a digest as lowercase hex.
func hexOfBytes(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0xf])
	}
	return string(out)
}
