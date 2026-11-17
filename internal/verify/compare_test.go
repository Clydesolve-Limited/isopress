package verify

// T5.1: the comparison tier of the moved compareContents (§5.5: the
// logic lives here now; internal/imagebuild's TestCompareContents and
// the real e2e keep pinning the byte-identical Problems() render
// through that package's one-line delegate). This file pins the NEW
// half the move added: the per-category result verify-media reports
// (matched / mismatched-with-reason / missing / unexpected /
// unreadable), the Clean/Anomalies predicates, and the totals
// derivation.

import (
	"crypto/sha256"
	"strings"
	"testing"

	"isopress/internal/media"
)

// cmpFixtureRows is a representative finalized content list: a file, a
// chunk, a symlink, an empty dir, and the two image-root self-rows.
func cmpFixtureRows() []ContentRow {
	return []ContentRow{
		{MediaPath: "files/a.txt", Kind: media.KindFile, ChunkIndex: -1, Size: 5, Hash: hashOfBytes([]byte("hello"))},
		{MediaPath: "files/link", Kind: media.KindSymlink, FileID: 7, ChunkIndex: -1, Size: 5},
		{MediaPath: "files/empty", Kind: media.KindDir, ChunkIndex: -1},
		{MediaPath: "chunks/big/00001", Kind: media.KindChunk, FileID: 2, ChunkIndex: 0, Size: 6, Hash: hashOfBytes([]byte("chunk!"))},
		{MediaPath: "MANIFEST.json", Kind: KindManifest, ChunkIndex: -1, Size: 2, Hash: hashOfBytes([]byte("{}"))},
	}
}

// cmpFixtureEntries is the matching hashed tree (implied parents and
// all).
func cmpFixtureEntries() []Entry {
	return []Entry{
		{MediaPath: "MANIFEST.json", Kind: KindFile, Size: 2, Hash: hashOfBytes([]byte("{}"))},
		{MediaPath: "chunks", Kind: KindDir},
		{MediaPath: "chunks/big", Kind: KindDir},
		{MediaPath: "chunks/big/00001", Kind: KindFile, Size: 6, Hash: hashOfBytes([]byte("chunk!"))},
		{MediaPath: "files", Kind: KindDir},
		{MediaPath: "files/a.txt", Kind: KindFile, Size: 5, Hash: hashOfBytes([]byte("hello"))},
		{MediaPath: "files/empty", Kind: KindDir},
		{MediaPath: "files/link", Kind: KindSymlink, Size: 5, LinkTarget: "a.txt"},
	}
}

func cmpTargets() map[string]string { return map[string]string{"files/link": "a.txt"} }

func TestCompareContentsCleanIsFullMatch(t *testing.T) {
	c := CompareContents(cmpFixtureRows(), cmpFixtureEntries(), cmpTargets())
	if !c.Clean() || c.Anomalies() != 0 {
		t.Fatalf("clean fixture = (clean %v, anomalies %d), want (true, 0): %v", c.Clean(), c.Anomalies(), c.Problems())
	}
	if len(c.Matched) != len(cmpFixtureRows()) {
		t.Errorf("matched = %v, want all %d listed rows", c.Matched, len(cmpFixtureRows()))
	}
	if len(c.Problems()) != 0 {
		t.Errorf("clean comparison produced problems: %v", c.Problems())
	}
}

// TestCompareContentsCategories drives every anomaly class in ONE
// comparison and asserts the STRUCTURED view verify-media reports:
// mismatched carries the expected-vs-actual reason, missing carries
// the row kind, unexpected carries the walked kind, unreadable carries
// the read failure - and Matched holds exactly the untouched rows.
func TestCompareContentsCategories(t *testing.T) {
	rows := append(cmpFixtureRows(),
		ContentRow{MediaPath: "files/gone.txt", Kind: media.KindFile, ChunkIndex: -1, Size: 4, Hash: hashOfBytes([]byte("gone"))},
		ContentRow{MediaPath: "files/unread.bin", Kind: media.KindFile, ChunkIndex: -1, Size: 4, Hash: hashOfBytes([]byte("nope"))},
		ContentRow{MediaPath: "export.sqlite", Kind: KindExport, ChunkIndex: -1, Size: 1, Hash: hashOfBytes([]byte("e"))},
	)
	entries := cmpFixtureEntries()
	// Corrupt one listed file (hash AND size, to pin the two-reason
	// mismatch), add a rogue file, mark one unreadable.
	for i := range entries {
		if entries[i].MediaPath == "files/a.txt" {
			entries[i].Hash = hashOfBytes([]byte("HELLO"))
			entries[i].Size = 99
		}
	}
	entries = append(entries,
		Entry{MediaPath: "files/unread.bin", Kind: KindFile, Size: 4,
			Unreadable: "open files/unread.bin: permission denied"},
		Entry{MediaPath: "export.sqlite", Kind: KindFile, Size: 1, Hash: hashOfBytes([]byte("e"))},
		Entry{MediaPath: "files/rogue.bin", Kind: KindFile, Size: 3, Hash: hashOfBytes([]byte("bad"))},
		Entry{MediaPath: "stray", Kind: KindDir},
	)

	c := CompareContents(rows, entries, cmpTargets())
	if c.Clean() || c.Anomalies() != 5 {
		t.Fatalf("anomalous fixture = (clean %v, anomalies %d), want (false, 5: 1 mismatch + 1 missing + 1 unreadable + 2 unexpected): %v",
			c.Clean(), c.Anomalies(), c.Problems())
	}

	if len(c.Mismatched) != 1 || c.Mismatched[0].MediaPath != "files/a.txt" || len(c.Mismatched[0].Reasons) != 2 ||
		!strings.Contains(c.Mismatched[0].Reasons[0], "content hash") || !strings.Contains(c.Mismatched[0].Reasons[1], "size 99, want 5") {
		t.Errorf("mismatched = %+v, want files/a.txt with the hash AND size reasons", c.Mismatched)
	}
	if len(c.Missing) != 1 || c.Missing[0].MediaPath != "files/gone.txt" || c.Missing[0].Kind != media.KindFile {
		t.Errorf("missing = %+v, want the one listed file row", c.Missing)
	}
	if len(c.Unreadable) != 1 || c.Unreadable[0].MediaPath != "files/unread.bin" ||
		!strings.Contains(c.Unreadable[0].Detail, "permission denied") {
		t.Errorf("unreadable = %+v, want files/unread.bin with its read failure", c.Unreadable)
	}
	if len(c.Unexpected) != 2 ||
		c.Unexpected[0] != (Unexpected{MediaPath: "files/rogue.bin", Kind: KindFile}) ||
		c.Unexpected[1] != (Unexpected{MediaPath: "stray", Kind: KindDir}) {
		t.Errorf("unexpected = %+v, want the rogue file and the stray dir", c.Unexpected)
	}
	// Matched is EXACTLY the untouched listed rows (row order).
	wantMatched := []string{"files/link", "files/empty", "chunks/big/00001", "MANIFEST.json", "export.sqlite"}
	if len(c.Matched) != len(wantMatched) {
		t.Fatalf("matched = %v, want %v", c.Matched, wantMatched)
	}
	for i, p := range wantMatched {
		if c.Matched[i] != p {
			t.Errorf("matched[%d] = %q, want %q (row order)", i, c.Matched[i], p)
		}
	}
	// The flat render keeps the historical interleaving: per-row order
	// (mismatch reasons, then missing, then unreadable - the row
	// positions decide), then unexpected in walk order.
	probs := c.Problems()
	if len(probs) != 6 ||
		!strings.HasPrefix(probs[0], "files/a.txt: content hash") ||
		!strings.HasPrefix(probs[1], "files/a.txt: size 99, want 5") ||
		!strings.HasPrefix(probs[2], "missing from extraction: files/gone.txt") ||
		!strings.HasPrefix(probs[3], "files/unread.bin: unreadable (") ||
		!strings.HasPrefix(probs[4], "unexpected file in extraction: files/rogue.bin") ||
		!strings.HasPrefix(probs[5], "unexpected dir in extraction: stray") {
		t.Errorf("problems = %v, want the six historical lines in row/walk order", probs)
	}
}

// TestCompareContentsKindAndTargetMismatches pins the kind and
// symlink-target classes at the category level.
func TestCompareContentsKindAndTargetMismatches(t *testing.T) {
	entries := cmpFixtureEntries()
	for i := range entries {
		if entries[i].MediaPath == "files/link" {
			entries[i].LinkTarget = "other.txt"
		}
	}
	c := CompareContents(cmpFixtureRows(), entries, cmpTargets())
	if len(c.Mismatched) != 1 || c.Mismatched[0].MediaPath != "files/link" ||
		!strings.Contains(c.Mismatched[0].Reasons[0], `symlink target "other.txt", want "a.txt"`) {
		t.Errorf("target mismatch = %+v", c.Mismatched)
	}

	entries = cmpFixtureEntries()
	for i := range entries {
		if entries[i].MediaPath == "files/empty" {
			entries[i].Kind = KindFile // the dir row extracted as a file
		}
	}
	c = CompareContents(cmpFixtureRows(), entries, cmpTargets())
	if len(c.Mismatched) != 1 || c.Mismatched[0].MediaPath != "files/empty" ||
		!strings.Contains(c.Mismatched[0].Reasons[0], "extracted as file, want dir") {
		t.Errorf("kind mismatch = %+v", c.Mismatched)
	}
}

// TestTotalsOf pins the progress-totals derivation the phases use
// (Entries = every row; Files/Bytes = the regular-file rows only).
func TestTotalsOf(t *testing.T) {
	tot := TotalsOf(cmpFixtureRows())
	if tot.Entries != 5 || tot.Files != 3 || tot.Bytes != 5+6+2 {
		t.Errorf("TotalsOf = %+v, want {Entries 5 Files 3 Bytes 13}", tot)
	}
}

// hashOfBytes is the digest helper of this file (extract_test's hexOf
// returns the hex STRING; the comparison needs the raw digest).
func hashOfBytes(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}
