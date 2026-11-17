package catalog

// T4.3: the build-side catalog surface tests against a REAL temp
// catalog (the 12.1 SQLite tier). They pin the 6.6 atomicity contract
// at the SQL layer (the trigger-injection proof, the copystore
// precedent), the replace (delete-then-insert) semantics against stale
// rows of a previous plan, the chunk-hash read, the failure and
// finalize transactions, and the runs row of the build command.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// imageState reads one images row's state (or fails the test).
func imageState(t *testing.T, db *DB, setID string, imageID int64) string {
	t.Helper()
	var state string
	if err := db.sql.QueryRowContext(context.Background(),
		`SELECT state FROM images WHERE set_id = ? AND image_id = ?`, setID, imageID).
		Scan(&state); err != nil {
		t.Fatalf("reading image %d state of set %q: %v", imageID, setID, err)
	}
	return state
}

// contentRowSet returns the image's image_contents rows as one
// deterministic "path kind size hash" string for comparisons.
func contentRowSet(t *testing.T, db *DB, setID string, imageID int64) string {
	t.Helper()
	rows, err := db.sql.QueryContext(context.Background(),
		`SELECT media_path, kind, size, COALESCE(hex(hash), 'NULL')
		 FROM image_contents WHERE set_id = ? AND image_id = ? ORDER BY media_path`,
		setID, imageID)
	if err != nil {
		t.Fatalf("reading image_contents of image %d: %v", imageID, err)
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var path, kind, hash string
		var size int64
		if err := rows.Scan(&path, &kind, &size, &hash); err != nil {
			t.Fatalf("scanning image_contents row: %v", err)
		}
		parts = append(parts, path+" "+kind+" "+itoa(size)+" "+hash)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterating image_contents: %v", err)
	}
	return strings.Join(parts, "\n")
}

// itoa is the int64 renderer for the row-set comparison.
func itoa(v int64) string { return strconv.FormatInt(v, 10) }

// hexText renders a digest the way the row-set comparison spells it
// (SQLite CAST(hash AS TEXT) is raw bytes, so compare on the hex form
// of BOTH sides: the query returns the hex of the stored bytes via
// hex()).
func hexText(b []byte) string { return strings.ToUpper(fmt.Sprintf("%x", b)) } // SQLite hex() is uppercase

// manifestHashA/exportHashA are the fixed 32-byte self-row digests of
// the fixture content lists.
var (
	buildManifestHash = bytes.Repeat([]byte{0xa1}, 32)
	buildExportHash   = bytes.Repeat([]byte{0xb2}, 32)
)

// TestBeginBuildTxReplacesRowsAndFlipsStateAtomically is THE 6.6
// atomicity proof at the SQL layer: the FINAL content list and the
// state flip commit together, the replace is delete-then-insert (stale
// rows of a previous plan are GONE), and a mid-transaction failure -
// a BEFORE-INSERT trigger tripping AFTER some rows already inserted -
// rolls EVERYTHING back: stale rows intact, state untouched.
func TestBeginBuildTxReplacesRowsAndFlipsStateAtomically(t *testing.T) {
	f := newCopyFixture(t)
	ctx := context.Background()
	bs := f.db.BuildStore(f.setID, 1, f.runID)

	// Seed the PREVIOUS write's content rows through the copystore
	// marks (the S3 shape): a payload row plus a dir row the new plan
	// will NOT place - the stale halves the replace must remove.
	cs := f.db.CopyStore(f.setID, 1)
	if err := cs.SetFileCopied(ctx, f.fileA, "files/docs/a.txt"); err != nil {
		t.Fatalf("seeding stale file content row: %v", err)
	}
	if err := cs.SetDirContent(ctx, 1, "files/empty"); err != nil {
		t.Fatalf("seeding stale dir content row: %v", err)
	}
	stale := contentRowSet(t, f.db, f.setID, 1)
	if !strings.Contains(stale, "files/empty dir") {
		t.Fatalf("fixture stale rows missing the dir row:\n%s", stale)
	}

	// The FINAL list of THIS build: the file (still placed), no dir,
	// plus the two self-rows.
	rows := []BuildContentRow{
		{MediaPath: "files/docs/a.txt", Kind: "file", FileID: f.fileA, ChunkIndex: -1, Size: 10, Hash: hashA[:]},
		{MediaPath: "MANIFEST.json", Kind: "manifest", ChunkIndex: -1, Size: 2048, Hash: buildManifestHash},
		{MediaPath: "catalog-export.sqlite", Kind: "export", ChunkIndex: -1, Size: 4096, Hash: buildExportHash},
	}

	// THE TRIGGER-INJECTION PROOF: the third insert trips a
	// BEFORE-INSERT trigger, so the tx fails AFTER the delete and two
	// inserts already ran - and everything must roll back together
	// with the state flip that never ran.
	f.installContentTrigger(t, "catalog-export.sqlite")
	if err := bs.BeginBuildTx(ctx, rows); err == nil {
		t.Fatal("BeginBuildTx must fail when a content insert fails")
	}
	f.dropContentTrigger(t)
	if state := imageState(t, f.db, f.setID, 1); state != "planned" {
		t.Errorf("state after the failed BeginBuildTx = %q, want 'planned' (the whole tx rolled back)", state)
	}
	if after := contentRowSet(t, f.db, f.setID, 1); after != stale {
		t.Errorf("content rows after the failed BeginBuildTx = \n%s\nwant the stale rows verbatim:\n%s", after, stale)
	}

	// The clean run: stale rows REPLACED (the dir row is gone), state
	// 'building', exactly the FINAL list present.
	if err := bs.BeginBuildTx(ctx, rows); err != nil {
		t.Fatalf("BeginBuildTx: %v", err)
	}
	if state := imageState(t, f.db, f.setID, 1); state != "building" {
		t.Errorf("state after BeginBuildTx = %q, want 'building' (6.6: the flip shares the content transaction)", state)
	}
	got := contentRowSet(t, f.db, f.setID, 1)
	want := strings.Join([]string{
		"MANIFEST.json manifest 2048 " + hexText(buildManifestHash),
		"catalog-export.sqlite export 4096 " + hexText(buildExportHash),
		"files/docs/a.txt file 10 " + hexText(hashA[:]),
	}, "\n")
	if got != want {
		t.Errorf("content rows after BeginBuildTx = \n%s\nwant the FINAL list (stale dir row removed):\n%s", got, want)
	}
}

// TestBeginBuildTxRejectsMissingImageAndBadRows pins the loud refusals:
// an image with no images row (run plan first), a bad kind, a chunk
// row without an index, a hashed kind without a 32-byte hash.
func TestBeginBuildTxRejectsMissingImageAndBadRows(t *testing.T) {
	f := newCopyFixture(t)
	ctx := context.Background()

	ghost := f.db.BuildStore(f.setID, 99, f.runID)
	err := ghost.BeginBuildTx(ctx, []BuildContentRow{
		{MediaPath: "files/x", Kind: "file", FileID: f.fileA, ChunkIndex: -1, Size: 1, Hash: hashA[:]},
	})
	if err == nil || !strings.Contains(err.Error(), "no images row") {
		t.Fatalf("BeginBuildTx without an images row: err = %v, want the 'run plan first' refusal", err)
	}

	bs := f.db.BuildStore(f.setID, 1, f.runID)
	bad := []struct {
		name string
		row  BuildContentRow
		want string
	}{
		{"empty media path", BuildContentRow{Kind: "dir"}, "empty media path"},
		{"bad kind", BuildContentRow{MediaPath: "files/x", Kind: "blob", ChunkIndex: -1}, "not one of the 2.3"},
		{"chunk without index", BuildContentRow{MediaPath: "chunks/x/00001", Kind: "chunk", ChunkIndex: -1, Size: 1, Hash: hashA[:]}, "without a catalog chunk index"},
		{"index on non-chunk", BuildContentRow{MediaPath: "files/x", Kind: "file", FileID: f.fileA, ChunkIndex: 0, Size: 1, Hash: hashA[:]}, "carries a chunk index"},
		{"file without hash", BuildContentRow{MediaPath: "files/x", Kind: "file", FileID: f.fileA, ChunkIndex: -1, Size: 1}, "want 32"},
		{"dir with hash", BuildContentRow{MediaPath: "files/d", Kind: "dir", ChunkIndex: -1, Hash: hashA[:]}, "NULL hash"},
	}
	for _, tc := range bad {
		err := bs.BeginBuildTx(ctx, []BuildContentRow{tc.row})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: BeginBuildTx err = %v, want a message containing %q", tc.name, err, tc.want)
		}
		// Every refusal rolled back: the image never left 'planned'
		// and no row landed.
		if state := imageState(t, f.db, f.setID, 1); state != "planned" {
			t.Fatalf("%s: state after the refused BeginBuildTx = %q, want 'planned'", tc.name, state)
		}
		if rows := contentRowSet(t, f.db, f.setID, 1); rows != "" {
			t.Fatalf("%s: content rows after the refused BeginBuildTx = %q, want none", tc.name, rows)
		}
	}
}

// TestReadChunkHashesParallelContract pins the primitive-shaped seam:
// request order, the loud refusals (missing row, NULL hash - staging
// is the first write, so a NULL here is corrupt state).
func TestReadChunkHashes(t *testing.T) {
	f := newCopyFixture(t)
	ctx := context.Background()
	bs := f.db.BuildStore(f.setID, 1, f.runID)

	// A mismatched parallel-slice request is a wiring error.
	if _, err := bs.ReadChunkHashes(ctx, []int64{f.big}, nil); err == nil {
		t.Fatal("ReadChunkHashes with mismatched slice lengths must fail")
	}
	// The fixture chunks carry NULL hashes until a first write stores
	// them (4.3): a NULL at build time is corrupt state, refused with
	// the chunk named.
	_, err := bs.ReadChunkHashes(ctx, []int64{f.big}, []int{0})
	if err == nil || !strings.Contains(err.Error(), "chunk 0 of file") {
		t.Fatalf("ReadChunkHashes over a NULL hash: err = %v, want the named-chunk refusal", err)
	}
	// An unknown chunk is refused by existence, not by hash shape.
	_, err = bs.ReadChunkHashes(ctx, []int64{f.big}, []int{42})
	if err == nil || !strings.Contains(err.Error(), "no chunk 42") {
		t.Fatalf("ReadChunkHashes over a missing chunk: err = %v, want 'no chunk 42'", err)
	}

	// The happy path: store a first-write capture through the copystore
	// seam, read it back in request order.
	cs1 := f.db.CopyStore(f.setID, 1)
	if err := cs1.SetChunkHash(ctx, f.big, 0, chunk0[:]); err != nil {
		t.Fatalf("SetChunkHash: %v", err)
	}
	got, err := bs.ReadChunkHashes(ctx, []int64{f.big, f.big}, []int{0, 1})
	if err == nil {
		t.Fatalf("ReadChunkHashes with one stored and one NULL hash must fail, got %x", got)
	}
	if err := cs1.SetChunkHash(ctx, f.big, 0, chunk0[:]); err != nil {
		t.Fatalf("SetChunkHash idempotent re-store: %v", err)
	}
	// Store chunk 1 on the image-2 handle (its assignment image).
	if err := f.db.CopyStore(f.setID, 2).SetChunkHash(ctx, f.big, 1, chunk1[:]); err != nil {
		t.Fatalf("SetChunkHash #1: %v", err)
	}
	got, err = bs.ReadChunkHashes(ctx, []int64{f.big, f.big}, []int{0, 1})
	if err != nil {
		t.Fatalf("ReadChunkHashes: %v", err)
	}
	if len(got) != 2 || !bytes.Equal(got[0], chunk0[:]) || !bytes.Equal(got[1], chunk1[:]) {
		t.Fatalf("ReadChunkHashes = %x, want [chunk0 chunk1] in request order", got)
	}
}

// TestMarkBuildFailedAndFinishBuildTx pin the two follow-up
// transactions: 'failed' + the run_events error row in ONE tx, and the
// finalize 'built' row with every 2.3 column stamped.
func TestMarkBuildFailedAndFinishBuildTx(t *testing.T) {
	f := newCopyFixture(t)
	ctx := context.Background()
	bs := f.db.BuildStore(f.setID, 1, f.runID)

	// The failure half: state 'failed' and the transcript event under
	// the handle's run, ONE transaction.
	if err := bs.MarkBuildFailed(ctx, "xorriso exploded:\nfatal: nope"); err != nil {
		t.Fatalf("MarkBuildFailed: %v", err)
	}
	if state := imageState(t, f.db, f.setID, 1); state != "failed" {
		t.Errorf("state after MarkBuildFailed = %q, want 'failed'", state)
	}
	var (
		level  string
		detail string
		runID  int64
	)
	if err := f.db.sql.QueryRowContext(ctx,
		`SELECT run_id, level, detail FROM run_events WHERE run_id = ?`, f.runID).
		Scan(&runID, &level, &detail); err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("reading the failure event: %v", err)
		}
		t.Fatal("MarkBuildFailed recorded no run_events row")
	}
	if level != "error" || !strings.Contains(detail, "xorriso exploded") {
		t.Errorf("failure event = (level %q, detail %q), want level 'error' carrying the transcript", level, detail)
	}

	// The finalize half: 'built' with built_at, final_path,
	// bytes_written, and mode 'iso' (2.3 "set at first write").
	finalPath := "/dest/S-X-001.iso"
	if err := bs.FinishBuildTx(ctx, finalPath, 1234567, 999); err != nil {
		t.Fatalf("FinishBuildTx: %v", err)
	}
	var (
		state    string
		builtAt  sql.NullInt64
		gotPath  sql.NullString
		gotBytes sql.NullInt64
		gotMode  sql.NullString
	)
	if err := f.db.sql.QueryRowContext(ctx,
		`SELECT state, built_at, final_path, bytes_written, mode
		 FROM images WHERE set_id = ? AND image_id = 1`, f.setID).
		Scan(&state, &builtAt, &gotPath, &gotBytes, &gotMode); err != nil {
		t.Fatalf("reading the built image row: %v", err)
	}
	if state != "built" || !builtAt.Valid || builtAt.Int64 != 1234567 ||
		!gotPath.Valid || gotPath.String != finalPath ||
		!gotBytes.Valid || gotBytes.Int64 != 999 ||
		!gotMode.Valid || gotMode.String != "iso" {
		t.Errorf("built row = (state %q, built_at %v, path %v, bytes %v, mode %v), want (built, 1234567, %s, 999, iso)",
			state, builtAt, gotPath, gotBytes, gotMode, finalPath)
	}

	// Empty path and unknown image are loud refusals.
	if err := bs.FinishBuildTx(ctx, "", 1, 1); err == nil {
		t.Error("FinishBuildTx with an empty final path must fail")
	}
	ghost := f.db.BuildStore(f.setID, 7, f.runID)
	if err := ghost.FinishBuildTx(ctx, finalPath, 1, 1); err == nil || !strings.Contains(err.Error(), "no images row") {
		t.Errorf("FinishBuildTx without an images row: err = %v, want 'no images row'", err)
	}
	if err := ghost.MarkBuildFailed(ctx, "x"); err == nil || !strings.Contains(err.Error(), "no images row") {
		t.Errorf("MarkBuildFailed without an images row: err = %v, want 'no images row'", err)
	}
}

// TestBeginBuildTxRebuildOfBuiltImageReplacesContentList pins the
// DOCUMENTED re-build behavior (build.go file doc: "the debris
// self-heals on rebuild"; buildstore.go BeginBuildTx: the REPLACE over
// an upsert): an image already in state 'built' may be rebuilt -
// BeginBuildTx flips 'built' -> 'building' and REPLACES the previous
// plan's content rows wholesale (a re-plan that placed different paths
// must not keep stale rows), and the re-finish stamps the NEW
// final_path/bytes over the old ones.
func TestBeginBuildTxRebuildOfBuiltImageReplacesContentList(t *testing.T) {
	f := newCopyFixture(t)
	ctx := context.Background()
	bs := f.db.BuildStore(f.setID, 1, f.runID)

	// First build: one payload row + the two self-rows, then finalize.
	rowsA := []BuildContentRow{
		{MediaPath: "files/docs/a.txt", Kind: "file", FileID: f.fileA, ChunkIndex: -1, Size: 10, Hash: hashA[:]},
		{MediaPath: "MANIFEST.json", Kind: "manifest", ChunkIndex: -1, Size: 2048, Hash: buildManifestHash},
		{MediaPath: "catalog-export.sqlite", Kind: "export", ChunkIndex: -1, Size: 4096, Hash: buildExportHash},
	}
	if err := bs.BeginBuildTx(ctx, rowsA); err != nil {
		t.Fatalf("BeginBuildTx (first build): %v", err)
	}
	if err := bs.FinishBuildTx(ctx, "/dest/S-001-001.iso", 111, 555); err != nil {
		t.Fatalf("FinishBuildTx (first build): %v", err)
	}
	if state := imageState(t, f.db, f.setID, 1); state != "built" {
		t.Fatalf("state after the first build = %q, want 'built'", state)
	}

	// THE REBUILD: a NEW plan whose content list no longer places
	// files/docs/a.txt. BeginBuildTx must flip 'built' -> 'building'
	// and REPLACE the rows - the stale payload row is GONE, the
	// self-rows carry the new run's digests.
	newManifestHash := bytes.Repeat([]byte{0xc3}, 32)
	rowsB := []BuildContentRow{
		{MediaPath: "MANIFEST.json", Kind: "manifest", ChunkIndex: -1, Size: 1024, Hash: newManifestHash},
		{MediaPath: "catalog-export.sqlite", Kind: "export", ChunkIndex: -1, Size: 4096, Hash: buildExportHash},
	}
	if err := bs.BeginBuildTx(ctx, rowsB); err != nil {
		t.Fatalf("BeginBuildTx (rebuild of a built image): %v", err)
	}
	if state := imageState(t, f.db, f.setID, 1); state != "building" {
		t.Errorf("state after the rebuild's BeginBuildTx = %q, want 'building' (a built image may be rebuilt)", state)
	}
	got := contentRowSet(t, f.db, f.setID, 1)
	want := strings.Join([]string{
		"MANIFEST.json manifest 1024 " + hexText(newManifestHash),
		"catalog-export.sqlite export 4096 " + hexText(buildExportHash),
	}, "\n")
	if got != want {
		t.Errorf("rebuild content rows = \n%s\nwant exactly the NEW list (stale a.txt row replaced):\n%s", got, want)
	}

	// The re-finish stamps the NEW outcome over the old one (the
	// self-heal the file doc promises).
	if err := bs.FinishBuildTx(ctx, "/dest/S-001-002.iso", 222, 777); err != nil {
		t.Fatalf("FinishBuildTx (rebuild): %v", err)
	}
	var builtAt, bytesWritten int64
	var finalPath string
	if err := f.db.sql.QueryRowContext(ctx,
		`SELECT built_at, final_path, bytes_written FROM images WHERE set_id = ? AND image_id = 1`, f.setID).
		Scan(&builtAt, &finalPath, &bytesWritten); err != nil {
		t.Fatalf("reading the rebuilt image row: %v", err)
	}
	if builtAt != 222 || finalPath != "/dest/S-001-002.iso" || bytesWritten != 777 {
		t.Errorf("rebuilt row = (built_at %d, path %q, bytes %d), want the NEW finalize stamp (222, /dest/S-001-002.iso, 777)",
			builtAt, finalPath, bytesWritten)
	}
}

// TestStartBuildRunKindAndStatus pins the runs row of the build
// command: kind 'build' (the 2.3 CHECK), status 'running', and the
// ErrNoSuchSet refusal.
func TestStartBuildRun(t *testing.T) {
	f := newCopyFixture(t)
	ctx := context.Background()

	id, err := f.db.StartBuildRun(ctx, BuildRun{
		SetID: f.setID, Root: "/dest", StartedAt: 42,
	})
	if err != nil {
		t.Fatalf("StartBuildRun: %v", err)
	}
	var (
		kind   string
		root   string
		status string
	)
	if err := f.db.sql.QueryRowContext(ctx,
		`SELECT kind, root, status, started_at FROM runs WHERE run_id = ?`, id).
		Scan(&kind, &root, &status, new(int64)); err != nil {
		t.Fatalf("reading the build run row: %v", err)
	}
	if kind != "build" || root != "/dest" || status != "running" {
		t.Errorf("build run row = (%q, %q, %q), want (build, /dest, running)", kind, root, status)
	}
	if _, err := f.db.StartBuildRun(ctx, BuildRun{SetID: "S-GHOST", Root: "/dest", StartedAt: 1}); !errors.Is(err, ErrNoSuchSet) {
		t.Errorf("StartBuildRun of an unknown set: err = %v, want ErrNoSuchSet", err)
	}
	if _, err := f.db.StartBuildRun(ctx, BuildRun{SetID: "", Root: "/dest"}); err == nil {
		t.Error("StartBuildRun with an empty set id must fail")
	}
}
