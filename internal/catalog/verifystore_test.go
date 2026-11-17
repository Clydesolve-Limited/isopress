package catalog

// T5.1: the verify-media catalog surface (verifystore.go) against a
// REAL temp catalog (the 12.1 SQLite tier): the images-row read, the
// content-list read with its hash ANCHOR rules and symlink-target
// resolution, the 7.2 stamp transaction, and the copy-side
// final_path recording (the completion-path gap closure).

import (
	"context"
	"strings"
	"testing"
)

// TestImageMetaReadsRow pins the read-side view: NULL columns decode
// to zero values, RecordCopyFinalPath lands final_path AND the T6.5
// mode='copy' + state='built' stamps, and the stamp lands
// state/verified_at.
func TestImageMetaReadsRow(t *testing.T) {
	f := newCopyFixture(t)
	ctx := context.Background()

	m, err := f.db.ImageMeta(ctx, f.setID, 1)
	if err != nil {
		t.Fatalf("ImageMeta: %v", err)
	}
	if m.ImageID != 1 || m.State != "planned" || m.FinalPath != "" || m.Mode != "" || m.VerifiedAt != 0 {
		t.Errorf("fresh ImageMeta = %+v, want the planned row with NULL columns as zero values", m)
	}

	if err := f.db.RecordCopyFinalPath(ctx, f.setID, 1, "/mnt/backup/S-COPY-001"); err != nil {
		t.Fatalf("RecordCopyFinalPath: %v", err)
	}
	// Idempotent re-record (a resumed copy run completes again).
	if err := f.db.RecordCopyFinalPath(ctx, f.setID, 1, "/mnt/backup/S-COPY-001"); err != nil {
		t.Fatalf("RecordCopyFinalPath (re-run): %v", err)
	}
	m, err = f.db.ImageMeta(ctx, f.setID, 1)
	if err != nil {
		t.Fatalf("ImageMeta after RecordCopyFinalPath: %v", err)
	}
	if m.FinalPath != "/mnt/backup/S-COPY-001" {
		t.Errorf("final_path = %q, want the recorded copy dir", m.FinalPath)
	}
	// T6.5: the completion write stamps BOTH provenance columns (2.3
	// "mode ... set at first write"; state per the RecordCopyFinalPath
	// godoc's design-vocabulary choice), on the FIRST record and on
	// the idempotent re-record alike.
	if m.Mode != "copy" || m.State != "built" {
		t.Errorf("after the copy completion: mode = %q, state = %q, want ('copy', 'built') (T6.5)", m.Mode, m.State)
	}
	// Re-record is a full re-finalize: it sets state='built' again
	// (the FinishBuildTx rebuild precedent) and writes nothing else.
	if m.VerifiedAt != 0 || m.BuiltAt != 0 {
		t.Errorf("after the re-record: verified_at %d / built_at %d, want both 0 (the copy completion writes three columns only)",
			m.VerifiedAt, m.BuiltAt)
	}

	// A zero-row write is a loud wiring error.
	err = f.db.RecordCopyFinalPath(ctx, f.setID, 99, "/nowhere")
	if err == nil || !strings.Contains(err.Error(), "no images row") {
		t.Errorf("RecordCopyFinalPath with no images row: err = %v, want the loud refusal", err)
	}
	if err = f.db.RecordCopyFinalPath(ctx, f.setID, 1, ""); err == nil || !strings.Contains(err.Error(), "empty final path") {
		t.Errorf("RecordCopyFinalPath with an empty path: err = %v, want the refusal", err)
	}
}

// TestRecordCopyFinalPathLegacyRowShapeStillHeals pins the T6.5
// regression half: an images row written by a PRE-T6.5 copy run -
// final_path recorded, mode still NULL, state still the 'planned'
// default ("state unset" on the copy path's side) - must keep behaving
// exactly as documented: verify-media's stamp heals it to 'verified'
// from ANY prior state, and neither the stamp nor the later reads
// depend on mode. Nothing rewrites such rows (see the
// RecordCopyFinalPath godoc), so this is the permanent shape pin for
// them.
func TestRecordCopyFinalPathLegacyRowShapeStillHeals(t *testing.T) {
	f := newCopyFixture(t)
	ctx := context.Background()

	// The pre-T6.5 copy completion: final_path ONLY, no mode, no state.
	if _, err := f.db.sql.ExecContext(ctx,
		`UPDATE images SET final_path = ? WHERE set_id = ? AND image_id = 1`,
		"/mnt/backup/S-LEGACY-001", f.setID); err != nil {
		t.Fatalf("seeding the legacy row: %v", err)
	}
	m, err := f.db.ImageMeta(ctx, f.setID, 1)
	if err != nil {
		t.Fatalf("ImageMeta (legacy row): %v", err)
	}
	if m.FinalPath != "/mnt/backup/S-LEGACY-001" || m.Mode != "" || m.State != "planned" {
		t.Fatalf("legacy ImageMeta = %+v, want final_path set with mode NULL and state 'planned'", m)
	}

	// THE HEAL: the 7.2 stamp lands from that untouched legacy state
	// and rewrites nothing else.
	if err := f.db.StampImageVerified(ctx, f.setID, 1, 4242); err != nil {
		t.Fatalf("StampImageVerified (legacy row): %v", err)
	}
	m, err = f.db.ImageMeta(ctx, f.setID, 1)
	if err != nil {
		t.Fatalf("ImageMeta after the stamp: %v", err)
	}
	if m.State != "verified" || m.VerifiedAt != 4242 {
		t.Errorf("stamped legacy ImageMeta = %+v, want state 'verified' + verified_at 4242", m)
	}
	if m.Mode != "" || m.FinalPath != "/mnt/backup/S-LEGACY-001" {
		t.Errorf("stamped legacy ImageMeta = %+v, want mode still NULL and final_path untouched", m)
	}

	// Completing the SAME legacy row through the T6.5 path upgrades it
	// to the new provenance (mode is set at the next write, per 2.3).
	if err := f.db.RecordCopyFinalPath(ctx, f.setID, 1, "/mnt/backup/S-LEGACY-001"); err != nil {
		t.Fatalf("RecordCopyFinalPath (legacy row): %v", err)
	}
	m, err = f.db.ImageMeta(ctx, f.setID, 1)
	if err != nil {
		t.Fatalf("ImageMeta after the re-completion: %v", err)
	}
	if m.Mode != "copy" || m.State != "built" || m.VerifiedAt != 4242 {
		t.Errorf("re-completed legacy ImageMeta = %+v, want mode 'copy', state 'built', verified_at preserved", m)
	}
}

// TestImageContentListAnchorsAndTargets pins the anchor rules: the
// payload rows carry the files/chunks hashes (the mirror is
// cross-checked against them), symlink rows resolve their target
// through files.link_target, dirs carry nothing, and the self-rows
// carry their own digests.
func TestImageContentListAnchorsAndTargets(t *testing.T) {
	f := newCopyFixture(t)
	ctx := context.Background()
	cs := f.db.CopyStore(f.setID, 1)

	if err := cs.SetFileCopied(ctx, f.fileA, "files/docs/a.txt"); err != nil {
		t.Fatalf("SetFileCopied a.txt: %v", err)
	}
	if err := cs.SetChunkCopied(ctx, f.big, 0, chunk0[:], "chunks/big.bin/00001"); err != nil {
		t.Fatalf("SetChunkCopied 0: %v", err)
	}
	var dirID int64
	if err := f.db.sql.QueryRowContext(ctx,
		`SELECT dir_id FROM directories WHERE set_id = ? AND relpath = 'empty'`, f.setID).Scan(&dirID); err != nil {
		t.Fatal(err)
	}
	if err := cs.SetDirContent(ctx, dirID, "files/empty"); err != nil {
		t.Fatalf("SetDirContent: %v", err)
	}
	manifestHash := buildManifestHash
	if err := cs.RecordEmbedded(ctx, "manifest", EmbeddedContent{
		MediaPath: "MANIFEST.json", Size: 2048, Hash: manifestHash,
	}); err != nil {
		t.Fatalf("RecordEmbedded: %v", err)
	}

	// The symlink row: seeded on IMAGE 2 by the fixture's plan.
	cs2 := f.db.CopyStore(f.setID, 2)
	if err := cs2.SetFileCopied(ctx, f.link, "files/link"); err != nil {
		t.Fatalf("SetFileCopied link: %v", err)
	}

	list, err := f.db.ImageContentList(ctx, f.setID, 1)
	if err != nil {
		t.Fatalf("ImageContentList(1): %v", err)
	}
	byPath := map[string]ImageContentRow{}
	for _, r := range list.Rows {
		byPath[r.MediaPath] = r
	}
	if r := byPath["files/docs/a.txt"]; r.Kind != "file" || string(r.Hash) != string(hashA[:]) {
		t.Errorf("file row = %+v, want the files.hash anchor", r)
	}
	if r := byPath["chunks/big.bin/00001"]; r.Kind != "chunk" || r.ChunkIndex != 0 || string(r.Hash) != string(chunk0[:]) {
		t.Errorf("chunk row = %+v, want the chunks.hash anchor and index 0", r)
	}
	if r := byPath["files/empty"]; r.Kind != "dir" || r.Hash != nil {
		t.Errorf("dir row = %+v, want a hash-free existence row", r)
	}
	if r := byPath["MANIFEST.json"]; r.Kind != "manifest" || string(r.Hash) != string(manifestHash) {
		t.Errorf("manifest row = %+v, want its own digest", r)
	}

	list2, err := f.db.ImageContentList(ctx, f.setID, 2)
	if err != nil {
		t.Fatalf("ImageContentList(2): %v", err)
	}
	if list2.SymlinkTargets["files/link"] != "docs/a.txt" {
		t.Errorf("symlink targets = %v, want files/link -> docs/a.txt (resolved through files.link_target)", list2.SymlinkTargets)
	}

	// THE ANCHOR DISAGREEMENT IS LOUD: a mirror that diverges from
	// its files.hash is corrupt-catalog, never a silent pick.
	rw := f.db
	if _, err := rw.sql.ExecContext(ctx,
		`UPDATE image_contents SET hash = ? WHERE set_id = ? AND image_id = 1 AND media_path = 'files/docs/a.txt'`,
		strings.Repeat("\x77", 32), f.setID); err != nil {
		t.Fatal(err)
	}
	_, err = f.db.ImageContentList(ctx, f.setID, 1)
	if err == nil || !strings.Contains(err.Error(), "corrupt catalog") || !strings.Contains(err.Error(), "files/docs/a.txt") {
		t.Errorf("diverging mirror: err = %v, want the loud corruption error naming the row", err)
	}
	// Restore the mirror so the unanchored case below is the only fault.
	if _, err := rw.sql.ExecContext(ctx,
		`UPDATE image_contents SET hash = ? WHERE set_id = ? AND image_id = 1 AND media_path = 'files/docs/a.txt'`,
		hashA[:], f.setID); err != nil {
		t.Fatal(err)
	}

	// A hashed row with NEITHER mirror NOR anchor is refused (null the
	// mirror AND the anchor row's hash - the anchor alone would
	// legitimately carry the expected digest).
	if _, err := rw.sql.ExecContext(ctx,
		`UPDATE image_contents SET hash = NULL WHERE set_id = ? AND image_id = 1 AND media_path = 'files/docs/a.txt'`,
		f.setID); err != nil {
		t.Fatal(err)
	}
	if _, err := rw.sql.ExecContext(ctx,
		`UPDATE files SET hash = NULL WHERE set_id = ? AND file_id = ?`, f.setID, f.fileA); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ImageContentList(ctx, f.setID, 1); err == nil || !strings.Contains(err.Error(), "no verifiable hash") {
		t.Errorf("unanchored hashed row: err = %v, want the loud refusal", err)
	}

	// A symlink row WITHOUT a files link cannot be verified - refused,
	// never silently skipped.
	if _, err := rw.sql.ExecContext(ctx,
		`UPDATE image_contents SET file_id = NULL WHERE set_id = ? AND image_id = 2 AND kind = 'symlink'`,
		f.setID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.ImageContentList(ctx, f.setID, 2); err == nil || !strings.Contains(err.Error(), "no files link") {
		t.Errorf("unlinked symlink row: err = %v, want the loud refusal", err)
	}
}

// TestStampImageVerified pins the 7.2 transaction: state 'verified'
// with the exact verified_at, from ANY prior state; built_at/final_path
// untouched; a zero-row update is loud.
func TestStampImageVerified(t *testing.T) {
	f := newCopyFixture(t)
	ctx := context.Background()

	// Zero-row: no such image.
	err := f.db.StampImageVerified(ctx, f.setID, 99, 12345)
	if err == nil || !strings.Contains(err.Error(), "no images row") {
		t.Fatalf("stamp with no images row: err = %v, want the loud refusal", err)
	}
	// Non-positive timestamps are refused.
	if err := f.db.StampImageVerified(ctx, f.setID, 1, 0); err == nil {
		t.Error("a non-positive verified_at must be refused")
	}

	// A build-shaped row: state 'built' + built_at + final_path, then
	// the stamp changes ONLY state and verified_at.
	bs := f.db.BuildStore(f.setID, 1, f.runID)
	if err := bs.FinishBuildTx(ctx, "/mnt/backup/S-COPY-001.iso", 1111, 2222); err != nil {
		t.Fatalf("FinishBuildTx: %v", err)
	}
	if err := f.db.StampImageVerified(ctx, f.setID, 1, 987654321); err != nil {
		t.Fatalf("StampImageVerified: %v", err)
	}
	m, err := f.db.ImageMeta(ctx, f.setID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if m.State != "verified" || m.VerifiedAt != 987654321 || m.BuiltAt != 1111 || m.FinalPath != "/mnt/backup/S-COPY-001.iso" {
		t.Errorf("stamped ImageMeta = %+v, want state/verified_at stamped with built_at/final_path untouched", m)
	}

	// A copy-shaped row: 'planned' -> 'verified' (the documented
	// policy: the stamp applies from whatever state the row carries).
	if err := f.db.StampImageVerified(ctx, f.setID, 2, 42); err != nil {
		t.Fatalf("StampImageVerified (planned row): %v", err)
	}
	if state := imageState(t, f.db, f.setID, 2); state != "verified" {
		t.Errorf("state of the copy image after the stamp = %q, want 'verified'", state)
	}

	// The stamp is a plain UPDATE on a write handle: a mode=ro handle
	// refuses it (the cli wires the stamp through a separate
	// read-write connection - the documented 2.1-vs-7.2 ruling).
	ro := mustOpen(t, f.db.path, OpenReadOnly)
	defer ro.Close()
	err = ro.StampImageVerified(ctx, f.setID, 1, 55)
	if err == nil || (!strings.Contains(err.Error(), "readonly") && !strings.Contains(err.Error(), "read-only")) {
		t.Errorf("read-only stamp error = %v, want the driver's readonly refusal", err)
	}
}
