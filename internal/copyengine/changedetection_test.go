package copyengine

// Change-detection mode threading + the chunked whole-file hardening
// seams (DESIGN_SPEC 3.5, RESOLVED 2026-10-20; the architect's pinned
// safety matrix):
//
//   - (b/c) Deps.Mode drives step 1 AND the metadata-mode PIN: on a
//     metadata-mode source a REPLACED file (same size + mtime_ns,
//     different content) FAILS at read-back verify (FR-014) and is
//     NEVER marked copied - the content guarantee is independent of
//     the identifier relaxation, pinned explicitly so nobody later
//     simplifies the verify away.
//   - the 2026-10-20 hardening seams: Item.DeferMark withholds the
//     final-chunk mark until the caller's whole-file decision, and
//     MarkChunk performs the deferred mark in the normal transaction
//     (the running whole-file accumulation itself lives in the cli
//     pipeline, which feeds it from each chunk's VERIFIED media bytes
//     - see the copyengine file doc for why the engine cannot own a
//     retry-safe cross-item tee).

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"isopress/internal/fsmeta"
)

// metaMode is the forced-metadata mode a CIFS-class source resolves to.
var metaMode = fsmeta.Mode{Set: fsmeta.ComparisonMetadata, Class: fsmeta.ClassCIFS}

// TestCopyItemMetadataModeIgnoresSynthesizedIdentifiers pins the step-1
// relaxation: an item whose catalog snapshot carries DIFFERENT
// dev/inode from the live source (the CIFS cross-session shape -
// identical size and mtime_ns) is SKIPPED under the full set and
// COPIED under the metadata set.
func TestCopyItemMetadataModeIgnoresSynthesizedIdentifiers(t *testing.T) {
	src := makeSource(t, "doc.bin", []byte("stable content"))

	// The catalog snapshot remembers the PREVIOUS SMB session's
	// synthesized identifiers; size and mtime_ns match the live file.
	drifted := src.meta
	drifted.Dev = src.meta.Dev + 1
	drifted.Inode = src.meta.Inode + 1_000_000
	if drifted.MtimeNS != src.meta.MtimeNS || drifted.Size != src.meta.Size {
		t.Fatal("fixture broken: drift must not touch size/mtime_ns")
	}
	item := Item{
		Kind: ItemWholeFile, FileID: 1, RelPath: "doc.bin", MediaPath: "files/doc.bin",
		SourcePath: src.path, DestPath: filepath.Join(t.TempDir(), "doc.bin"),
		SourceSize: drifted.Size, MtimeNS: drifted.MtimeNS,
		Dev: int64(drifted.Dev), Inode: int64(drifted.Inode),
		Size: drifted.Size, Hash: sha256Of(src.content), Mode: 0o644,
	}

	// Full set (the zero-value default): the identifier drift skips.
	store := newFakeStore()
	res := CopyItem(context.Background(), Deps{Store: store}, item)
	if res.Outcome != OutcomeSkipped {
		t.Fatalf("full mode: outcome = %v, want OutcomeSkipped", res.Outcome)
	}
	if len(store.chunkCopied) != 0 || len(store.fileCopied) != 0 {
		t.Fatal("full mode: skipped item left marks behind")
	}

	// Metadata set: the same item copies and marks (the stakeholder's
	// fix).
	store2 := newFakeStore()
	res2 := CopyItem(context.Background(), Deps{Store: store2, Mode: metaMode}, item)
	if res2.Outcome != OutcomeCopied {
		t.Fatalf("metadata mode: outcome = %v (%v), want OutcomeCopied", res2.Outcome, res2.Err)
	}
	if store2.fileCopied[1] != "files/doc.bin" {
		t.Errorf("metadata mode: mark = %v, want files/doc.bin", store2.fileCopied)
	}
}

// TestCopyItemMetadataModeVerifyStillGatesContent is THE PIN (the
// architect's matrix (c)): on a metadata-mode source a REPLACED file -
// same size, same mtime_ns, different bytes - passes the (degraded)
// step-1 compare, but the read-back verify (FR-014) compares the
// written bytes against the SCAN-TIME catalog hash and REFUSES to
// mark. After the 5.1 step-7 budget the item fails, nothing is marked,
// and the final file is deleted: the content guarantee is independent
// of the identifier relaxation.
func TestCopyItemMetadataModeVerifyStillGatesContent(t *testing.T) {
	src := makeSource(t, "replaced.bin", []byte("original payload"))
	catalogHash := sha256Of(src.content)
	catalogMeta := src.meta

	// Replace the source IN PLACE with same-size different bytes and
	// restore the ORIGINAL mtime: size and mtime_ns match the catalog
	// exactly, so only the (ignored) inode and the bytes themselves
	// betray the replacement.
	replacement := []byte("hijackedpayload!")
	if len(replacement) != len(src.content) {
		t.Fatalf("fixture broken: replacement length %d != %d", len(replacement), len(src.content))
	}
	if err := os.WriteFile(src.path, replacement, 0o644); err != nil {
		t.Fatal(err)
	}
	orig := catalogMeta
	mt := int64Time(orig.MtimeNS)
	if err := os.Chtimes(src.path, mt, mt); err != nil {
		t.Fatal(err)
	}

	item := Item{
		Kind: ItemWholeFile, FileID: 7, RelPath: "replaced.bin", MediaPath: "files/replaced.bin",
		SourcePath: src.path, DestPath: filepath.Join(t.TempDir(), "replaced.bin"),
		SourceSize: catalogMeta.Size, MtimeNS: catalogMeta.MtimeNS,
		Dev: int64(catalogMeta.Dev), Inode: int64(catalogMeta.Inode),
		Size: catalogMeta.Size, Hash: catalogHash, Mode: 0o644,
	}

	store := newFakeStore()
	deps := Deps{Store: store, Mode: metaMode, Sleep: func(context.Context, time.Duration) error { return nil }}
	res := CopyItem(context.Background(), deps, item)

	if res.Outcome != OutcomeFailed {
		t.Fatalf("outcome = %v (%v), want OutcomeFailed (the replaced content must fail at verify)", res.Outcome, res.Err)
	}
	if res.Err == nil || !bytes.Contains([]byte(res.Err.Error()), []byte("read-back hash mismatch")) {
		t.Errorf("error = %v, want the read-back hash mismatch cause", res.Err)
	}
	// THE PIN: zero marks - the copied flag is NEVER set for content
	// that does not match the scan-time hash.
	if len(store.fileCopied) != 0 || len(store.chunkCopied) != 0 {
		t.Errorf("marks recorded for unverified content: %v / %v", store.fileCopied, store.chunkCopied)
	}
	// And the final file is deleted (never-mark-before-verify's
	// filesystem half).
	if _, err := os.Stat(item.DestPath); !os.IsNotExist(err) {
		t.Errorf("final file exists after verify failure (stat err = %v)", err)
	}
}

// int64Time converts mtime_ns to the time.Time Chtimes takes.
func int64Time(ns int64) time.Time { return time.Unix(0, ns) }

// --- the whole-file hardening seams -----------------------------------------

// TestCopyItemDeferMarkWithholdsMarkUntilCallerDecides pins the
// deferred-mark path: a DeferMark chunk writes AND verifies (steps
// 1-5) but records NO mark and emits NO completion; the verified bytes
// are on media, the anchor comes back for the caller's whole-file
// decision, and MarkChunk then performs the normal mark transaction.
func TestCopyItemDeferMarkWithholdsMarkUntilCallerDecides(t *testing.T) {
	content := []byte("final chunk bytes")
	src := makeSource(t, "tail.bin", content)
	store := newFakeStore()
	dest := filepath.Join(t.TempDir(), "00002")

	item := chunkItem(src, 9, 1, 0, int64(len(content)), nil, dest)
	item.DeferMark = true
	res := CopyItem(context.Background(), Deps{Store: store}, item)
	if res.Outcome != OutcomeCopied {
		t.Fatalf("outcome = %v (%v), want OutcomeCopied", res.Outcome, res.Err)
	}
	// No mark: the decision belongs to the caller.
	if len(store.chunkCopied) != 0 || len(store.chunkHash) != 0 {
		t.Fatalf("DeferMark recorded marks: %v / %v", store.chunkCopied, store.chunkHash)
	}
	// The verified bytes ARE on media (the honest verified-but-unmarked
	// state - no rollback of honest bytes).
	assertBytesEqual(t, dest, content)
	// The anchor is the captured first-write hash, for the caller's
	// MarkChunk call.
	if got, want := res.Anchor, sha256Of(content); !bytes.Equal(got, want) {
		t.Errorf("anchor = %x, want the captured chunk hash %x", got, want)
	}

	// The caller's decision on a whole-file MATCH: mark in the normal
	// transaction (flag + 4.3 hash + image_contents + parent mirror -
	// the store's own contract).
	if err := MarkChunk(context.Background(), Deps{Store: store}, item, res.Anchor); err != nil {
		t.Fatalf("MarkChunk: %v", err)
	}
	if store.chunkCopied[9][1] != item.MediaPath {
		t.Errorf("MarkChunk marks = %v, want chunk 9#1 at %s", store.chunkCopied, item.MediaPath)
	}
	if !bytes.Equal(store.chunkHash[9][1], res.Anchor) {
		t.Errorf("MarkChunk stored hash = %x, want the anchor", store.chunkHash[9][1])
	}
}

// TestMarkChunkRefusesBadInputs pins MarkChunk's guards: whole-file
// items and a nil Store are refused loudly (both would be wiring bugs
// of the hardening pipeline).
func TestMarkChunkRefusesBadInputs(t *testing.T) {
	src := makeSource(t, "f.bin", []byte("x"))
	whole := wholeItem(src, 1, filepath.Join(t.TempDir(), "f"))
	if err := MarkChunk(context.Background(), Deps{Store: newFakeStore()}, whole, nil); err == nil {
		t.Error("MarkChunk accepted a whole-file item")
	}
	chunk := chunkItem(src, 1, 0, 0, 1, nil, filepath.Join(t.TempDir(), "00000"))
	if err := MarkChunk(context.Background(), Deps{}, chunk, nil); err == nil {
		t.Error("MarkChunk accepted a nil Store")
	}
}

// TestCopyItemRefusesDeferMarkWholeFile pins the validateItem guard:
// DeferMark is defined for the chunk path only (the whole-file path
// applies 5.4 metadata after its mark and has no deferred caller).
func TestCopyItemRefusesDeferMarkWholeFile(t *testing.T) {
	src := makeSource(t, "f.bin", []byte("abc"))
	item := wholeItem(src, 1, filepath.Join(t.TempDir(), "f"))
	item.DeferMark = true
	res := CopyItem(context.Background(), Deps{Store: newFakeStore()}, item)
	if res.Outcome != OutcomeFailed || res.Err == nil ||
		!bytes.Contains([]byte(res.Err.Error()), []byte("not a chunk")) {
		t.Fatalf("outcome = %v (%v), want a refused DeferMark whole file", res.Outcome, res.Err)
	}
}

// assertBytesEqual is a local byte-compare helper (dest assertions
// after a deferred mark).
func assertBytesEqual(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s = %d bytes %x, want %d bytes", path, len(got), got, len(want))
	}
}
