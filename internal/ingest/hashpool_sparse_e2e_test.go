//go:build e2e

package ingest

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestHashPoolSparseMultiGiB: a 4 GiB SPARSE file (truncate, cheap on
// disk) hashes correctly WITHOUT the pool allocating anything near its
// size. The reference is computed by streaming a 1 MiB zero buffer
// 4096 times into an independent sha256 - never by allocating 4 GiB.
// The memory bound itself is structural: the worker loop holds exactly
// one HashReadBufferSize buffer (pinned at 1 MiB by
// TestHashReadBufferSizeIsOneMiB) for the file's whole life, so the
// only way to regress this test into an OOM would be replacing the
// streaming loop with a whole-file read, which the buffer-size pin and
// this digest check jointly catch.
func TestHashPoolSparseMultiGiB(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the 4 GiB sparse hash in -short mode")
	}
	root := t.TempDir()
	const mib = 1 << 20
	path := filepath.Join(root, "sparse4g.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(4096 * mib); err != nil {
		f.Close()
		t.Skipf("filesystem refused a 4 GiB sparse truncate: %v", err)
	}
	f.Close()

	// Sparseness sanity: the fixture must be TRUNCATE-based, never
	// materialized. A 4 GiB hole allocates a negligible block count (0
	// on tmpfs, a handful on ext4); allocated bytes anywhere near 4 GiB
	// mean the fixture accidentally wrote real bytes, which would
	// silently turn this test into a disk-space and IO-bandwidth hog
	// and weaken the memory-bound claim it exists to pin.
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if alloc := st.Sys().(*syscall.Stat_t).Blocks * 512; alloc > 64<<20 {
		t.Fatalf("sparse fixture has %d allocated bytes (> 64 MiB): truncate-based sparseness is broken, the test is materializing 4 GiB", alloc)
	}

	// Reference: SHA-256 of 4 GiB of zeros, streamed - 1 MiB at a time.
	ref := sha256.New()
	zeros := make([]byte, mib)
	for i := 0; i < 4096; i++ {
		ref.Write(zeros)
	}
	var want [32]byte
	copy(want[:], ref.Sum(nil))

	// Timing sanity: a sparse streaming hash of 4 GiB is a few seconds
	// of sequential reads; the generous bound is a tripwire against a
	// regression that materializes the fixture or reads it in tiny
	// per-call allocations (the block-count check above is the primary
	// sparseness guard; this one catches "accidentally slow" loudly).
	startHash := time.Now()

	pool := NewHashPool(context.Background(), PoolOptions{Workers: 2})
	if err := pool.Submit(Job{RelPath: "sparse4g.bin", AbsPath: path}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	pool.Close()
	results := drainResults(pool.Results())
	if err := pool.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if elapsed := time.Since(startHash); elapsed > 2*time.Minute {
		t.Errorf("hashing the 4 GiB SPARSE file took %v, want well under 2 min: the fixture or the streaming loop regressed", elapsed)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	r := results[0]
	if r.Err != nil {
		t.Fatalf("sparse hash failed: %v", r.Err)
	}
	if r.Bytes != 4096*mib {
		t.Errorf("Bytes = %d, want %d (the full logical size)", r.Bytes, 4096*mib)
	}
	if r.Digest != want {
		t.Errorf("digest %x != streamed reference %x", r.Digest, want)
	}
}

// --- cancellation ------------------------------------------------------------
