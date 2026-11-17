//go:build large

package imagebuild

// T4.3: the MULTI-EXTENT round-trip under the plan's `large` build tag
// (DESIGN_SPEC 12.2/12.6, IMPLEMENTATION_PLAN T4.3 acceptance "a
// sparse source of at least 5 GiB round-trips byte-exact through
// multi-extent ISO 9660 level 3"). Run with:
//
//	go test -tags large ./internal/imagebuild/ -run Large -v
//
// The test is skipped unless the tag is set, because it writes ~15 GiB
// transiently (staged tree + .partial + extraction) and takes minutes.
// The workspace defaults to the user cache directory (typically a real
// disk, NOT the often-tmpfs OS temp dir); override with
// ISOPRESS_LARGE_DIR. It self-skips with a clear message when the
// workspace filesystem has less than ~20 GiB free.
//
// Byte-exactness is asserted THROUGH SHA-256: the extraction's digest
// of the restored 5+ GiB file must equal the catalog digest computed
// from the SPARSE SOURCE before the build - a sha256 collision is the
// only way a differing byte stream passes, which is the same anchor
// FR-020/FR-041 build their guarantees on.

import (
	"context"
	"crypto/sha256"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"isopress/internal/fsmeta"
	"isopress/internal/media"
	"isopress/internal/verify"
)

// largeSize is the sparse source size: > 4 GiB forces ISO 9660
// MULTI-EXTENT records under -iso-level 3 (a level-1 image cannot
// hold a file this size at all).
const largeSize = 5<<30 + 12345

// largeFreeFloor is the free-space floor the workspace must satisfy
// before the test starts (staged copy + .partial + extraction, with
// margin).
const largeFreeFloor = 20 << 30

// TestLargeMultiExtentRoundTrip builds one image whose whole file is
// > 4 GiB and asserts the extraction round-trips it byte-exact.
func TestLargeMultiExtentRoundTrip(t *testing.T) {
	x := realToolOrSkip(t)

	work := largeWorkspace(t)

	// The sparse source: a 5+ GiB hole reads as zeros, hashing reads
	// zeros quickly, and xorriso -iso-level 3 masters it as a
	// multi-extent file (DESIGN_SPEC 6.2).
	srcRoot := filepath.Join(work, "src")
	if err := os.MkdirAll(srcRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	bigPath := filepath.Join(srcRoot, "huge.bin")
	big, err := os.Create(bigPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := big.Truncate(largeSize); err != nil {
		_ = big.Close()
		t.Fatalf("creating the sparse %d-byte source: %v", largeSize, err)
	}
	if err := big.Close(); err != nil {
		t.Fatal(err)
	}

	t.Logf("hashing the sparse source (%d bytes of zeros)...", largeSize)
	bigHash, err := hashFileStreaming(bigPath)
	if err != nil {
		t.Fatalf("hashing the sparse source: %v", err)
	}
	meta, err := fsmeta.PathMeta(bigPath)
	if err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(work, "images")
	store := &fakeStore{}
	in := BuildInput{
		SetID:            "S-LARGE",
		ImageID:          1,
		SourceRoot:       srcRoot,
		WorkDir:          filepath.Join(work, "scratch"),
		DestDir:          dest,
		UsableSize:       largeSize + 1<<30, // payload + the 2 MiB/512n overhead
		VerifyAfterBuild: true,
		Inventory: StageInventory{
			Files: []StageFile{{
				FileID: 1, RelPath: "huge.bin", Size: meta.Size,
				Mode: 0o644, UID: uint32(os.Getuid()), GID: uint32(os.Getgid()),
				MtimeNS: meta.MtimeNS, Dev: int64(meta.Dev), Inode: int64(meta.Inode),
				Hash: bigHash,
			}},
		},
		Export: func() EmbeddedExport {
			b := []byte("large-test export")
			p := filepath.Join(work, "export-scratch", "e.bin")
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, b, 0o644); err != nil {
				t.Fatal(err)
			}
			return EmbeddedExport{Path: p, Size: int64(len(b)), Hash: sha256Sum(b)}
		}(),
		Manifest: ManifestSeed{
			ToolVersion:   "test",
			SchemaVersion: 1,
			CreatedAt:     mustTime(t),
			Placement: []media.PlacementInput{{
				ImageNumber: 1, Paths: []string{"huge.bin"},
			}},
		},
		Store: store,
	}
	if err := os.MkdirAll(in.WorkDir, 0o755); err != nil {
		t.Fatal(err)
	}

	deps := Deps{Master: x, Extractor: x, HashStore: store}
	res, err := Build(context.Background(), deps, in)
	if err != nil {
		t.Fatalf("Build of the %d-byte multi-extent image: %v", largeSize, err)
	}
	finalISO := filepath.Join(dest, "S-LARGE-001.iso")
	if res.BytesWritten < largeSize {
		t.Fatalf("final image is %d bytes, smaller than its %d-byte payload", res.BytesWritten, largeSize)
	}

	// THE ROUND-TRIP: extract the finalized image and compare the
	// restored file's sha256 against the SOURCE digest (byte-exact by
	// the D-09 anchor).
	t.Logf("extracting and hashing the %d-byte image...", largeSize)
	extDir := filepath.Join(work, "extract")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ext, err := verify.ExtractAndHash(context.Background(), x.tool, finalISO, extDir, nil, verify.Totals{})
	if err != nil {
		t.Fatalf("extracting the multi-extent image: %v", err)
	}
	var restored *verify.Entry
	for i := range ext.Entries {
		if ext.Entries[i].MediaPath == "files/huge.bin" {
			restored = &ext.Entries[i]
		}
	}
	if restored == nil {
		t.Fatalf("files/huge.bin missing from the extraction (%d entries)", len(ext.Entries))
	}
	if restored.Size != largeSize {
		t.Errorf("restored size = %d, want %d (multi-extent file truncated?)", restored.Size, largeSize)
	}
	if string(restored.Hash) != string(bigHash) {
		t.Errorf("restored hash %x != source hash %x (NOT byte-exact)", restored.Hash, bigHash)
	}
	// The extraction itself streamed the file: its byte count proves
	// the full payload arrived.
	if ext.TotalBytes < largeSize {
		t.Errorf("extraction hashed %d bytes total, less than the %d-byte file", ext.TotalBytes, largeSize)
	}
}

// TestLargeWorkspaceHonorsEnvDir pins the ISOPRESS_LARGE_DIR
// override of the file doc: when the env var is set, the workspace is
// created UNDER it (a roomier filesystem of the operator's choice).
// On a workspace filesystem without the free-space floor the helper
// skips with the documented message - the same honest self-skip the
// main large test performs.
func TestLargeWorkspaceHonorsEnvDir(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ISOPRESS_LARGE_DIR", base)
	dir := largeWorkspace(t) // skips when base's filesystem lacks the floor
	if !strings.HasPrefix(dir, base+string(os.PathSeparator)) {
		t.Fatalf("largeWorkspace = %q, want a run dir under $ISOPRESS_LARGE_DIR (%q)", dir, base)
	}
}

// largeWorkspace picks the workspace for the large test (see the file
// doc): $ISOPRESS_LARGE_DIR, else <user cache>/isopress-large,
// else the OS temp dir. It asserts the free-space floor.
func largeWorkspace(t *testing.T) string {
	t.Helper()
	base := os.Getenv("ISOPRESS_LARGE_DIR")
	if base == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			cache = os.TempDir()
		}
		base = filepath.Join(cache, "isopress-large")
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatalf("creating the large-test workspace %s: %v", base, err)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(base, &st); err != nil {
		t.Fatalf("statfs %s: %v", base, err)
	}
	if avail := st.Bavail * uint64(st.Bsize); avail < largeFreeFloor {
		t.Skipf("large-test workspace %s has %d bytes free, need >= %d (set ISOPRESS_LARGE_DIR to a roomier filesystem)",
			base, avail, largeFreeFloor)
	}
	dir, err := os.MkdirTemp(base, "run-")
	if err != nil {
		t.Fatalf("creating the large-test run dir under %s: %v", base, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// hashFileStreaming is the plain whole-file sha256 (a bare io.Copy
// through the stdlib buffer - 5 GiB of zeros stream in seconds).
func hashFileStreaming(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	return h.Sum(nil), nil
}

// sha256Sum digests a small in-memory value.
func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}
