//go:build e2e

package imagebuild

// T4.3: the GATED REAL-xorriso integration tier (DESIGN_SPEC 12.6,
// IMPLEMENTATION_PLAN T4.3 acceptance). Every test here is gated on
// exec.LookPath("xorriso") and skipped otherwise; on this dev machine
// the real binary (1.5.6) is present, so they RUN.
//
// The fixtures go through the REAL pipeline end to end: real source
// tree (a setuid file, a symlink, an empty dir, a chunked parent, a
// unicode name) -> Stage -> real xorriso master -> extraction
// self-verify via the internal/verify primitive -> atomic finalize.
// Only the catalog seam stays fake (fakeStore records the same
// transactions *catalog.BuildStore commits; the SQL layer itself is
// pinned in internal/catalog/buildstore_test.go).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"isopress/internal/fsmeta"
	"isopress/internal/media"
	"isopress/internal/progress"
	"isopress/internal/units"
	"isopress/internal/verify"
)

// realFixture is a build fixture over a REAL source tree.
type realFixture struct {
	in      BuildInput
	store   *fakeStore
	extract *verify.ExtractResult // filled after a successful build
	big     []byte                // the chunked parent's content
	setuid  []byte                // the setuid file's content
}

// newRealFixture writes the fixture tree and assembles the BuildInput
// (real stat metadata - the 3.5 point 3 comparison runs against it).
func newRealFixture(t *testing.T) *realFixture {
	t.Helper()
	srcRoot := t.TempDir()
	work := t.TempDir()
	dest := filepath.Join(t.TempDir(), "images")

	hello := []byte("hello backup tool\n")
	big := make([]byte, 1500)
	for i := range big {
		big[i] = byte(i % 251)
	}
	setuid := []byte("#!/bin/sh\necho hi\n")
	nested := []byte("nested content\n")
	unicodeName := "üñí-ø.txt"
	unicodeContent := []byte("unicode payload\n")

	writeFile(t, filepath.Join(srcRoot, "hello.txt"), hello, 0o644)
	setuidPath := filepath.Join(srcRoot, "setuid.sh")
	writeFile(t, setuidPath, setuid, 0o755)
	if err := os.Chmod(setuidPath, 0o4755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(srcRoot, "sub", "nested.txt"), nested, 0o644)
	writeFile(t, filepath.Join(srcRoot, unicodeName), unicodeContent, 0o644)
	writeFile(t, filepath.Join(srcRoot, "big.bin"), big, 0o644)
	if err := os.Symlink("hello.txt", filepath.Join(srcRoot, "link")); err != nil {
		t.Fatal(err)
	}
	// THE SYMLINK VARIANTS (the osirrox contract, probed and pinned):
	// a DANGLING link (target absent from the image), a link to a real
	// DIRECTORY, and an ABSOLUTE-target link. Real xorriso 1.5.6
	// restores all three as symlinks with their exact targets
	// (osirrox never dereferences or drops them) - the extraction
	// walk records them as symlinks and the step-7 comparison matches
	// on target, so the full pipeline must round-trip them.
	if err := os.Symlink("missing-target.txt", filepath.Join(srcRoot, "link-dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub", filepath.Join(srcRoot, "link-todir")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(srcRoot, "link-abs")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(srcRoot, "emptydir"), 0o750); err != nil {
		t.Fatal(err)
	}

	metaOf := func(rel string) fsmeta.Meta {
		m, err := fsmeta.PathMeta(filepath.Join(srcRoot, rel))
		if err != nil {
			t.Fatalf("stat %s: %v", rel, err)
		}
		return m
	}
	helloMeta := metaOf("hello.txt")
	setuidMeta := metaOf("setuid.sh")
	nestedMeta := metaOf("sub/nested.txt")
	unicodeMeta := metaOf(unicodeName)
	bigMeta := metaOf("big.bin")
	linkMeta := metaOf("link")
	danglingMeta := metaOf("link-dangling")
	todirMeta := metaOf("link-todir")
	absMeta := metaOf("link-abs")

	bigHash := sha256.Sum256(big)
	c0 := StageChunk{FileID: 20, ChunkIndex: 0, Offset: 0, Length: 1000,
		ParentRelPath: "big.bin", ParentSize: bigMeta.Size, ParentMode: 0o644, ParentUID: uint32(os.Getuid()), ParentGID: uint32(os.Getgid()),
		ParentMtimeNS: bigMeta.MtimeNS, ParentDev: int64(bigMeta.Dev), ParentInode: int64(bigMeta.Inode)}
	c1 := StageChunk{FileID: 20, ChunkIndex: 1, Offset: 1000, Length: 500,
		ParentRelPath: "big.bin", ParentSize: bigMeta.Size, ParentMode: 0o644, ParentUID: uint32(os.Getuid()), ParentGID: uint32(os.Getgid()),
		ParentMtimeNS: bigMeta.MtimeNS, ParentDev: int64(bigMeta.Dev), ParentInode: int64(bigMeta.Inode)}

	placementPaths := []string{
		"big.bin", "emptydir", "hello.txt", "link", "link-abs", "link-dangling", "link-todir",
		"setuid.sh", "sub/nested.txt", unicodeName,
	}

	// The catalog records the REAL owner (the scan's lstat), so the
	// staged tree's chown is a self-chown no-op and the fixture stays
	// warning-free as non-root.
	ownerUID, ownerGID := uint32(os.Getuid()), uint32(os.Getgid())

	store := &fakeStore{}
	f := &realFixture{
		store:  store,
		big:    big,
		setuid: setuid,
	}
	f.in = BuildInput{
		SetID:            "S-REAL",
		ImageID:          1,
		SourceRoot:       srcRoot,
		WorkDir:          work,
		DestDir:          dest,
		UsableSize:       100 << 20,
		VerifyAfterBuild: true,
		Inventory: StageInventory{
			Files: []StageFile{
				{FileID: 1, RelPath: "hello.txt", Size: helloMeta.Size, Mode: 0o644, UID: ownerUID, GID: ownerGID,
					MtimeNS: helloMeta.MtimeNS, Dev: int64(helloMeta.Dev), Inode: int64(helloMeta.Inode),
					Hash: hashOf(hello)},
				{FileID: 2, RelPath: "setuid.sh", Size: setuidMeta.Size, Mode: 0o4755, UID: ownerUID, GID: ownerGID,
					MtimeNS: setuidMeta.MtimeNS, Dev: int64(setuidMeta.Dev), Inode: int64(setuidMeta.Inode),
					Hash: hashOf(setuid)},
				{FileID: 3, RelPath: "sub/nested.txt", Size: nestedMeta.Size, Mode: 0o644, UID: ownerUID, GID: ownerGID,
					MtimeNS: nestedMeta.MtimeNS, Dev: int64(nestedMeta.Dev), Inode: int64(nestedMeta.Inode),
					Hash: hashOf(nested)},
				{FileID: 4, RelPath: unicodeName, Size: unicodeMeta.Size, Mode: 0o644, UID: ownerUID, GID: ownerGID,
					MtimeNS: unicodeMeta.MtimeNS, Dev: int64(unicodeMeta.Dev), Inode: int64(unicodeMeta.Inode),
					Hash: hashOf(unicodeContent)},
			},
			Chunks: []StageChunk{c0, c1},
			Symlinks: []StageSymlink{
				{
					FileID: 5, RelPath: "link", LinkTarget: "hello.txt",
					Size: linkMeta.Size, Mode: 0o777, UID: ownerUID, GID: ownerGID, MtimeNS: linkMeta.MtimeNS,
				},
				{
					FileID: 30, RelPath: "link-dangling", LinkTarget: "missing-target.txt",
					Size: danglingMeta.Size, Mode: 0o777, UID: ownerUID, GID: ownerGID, MtimeNS: danglingMeta.MtimeNS,
				},
				{
					FileID: 31, RelPath: "link-todir", LinkTarget: "sub",
					Size: todirMeta.Size, Mode: 0o777, UID: ownerUID, GID: ownerGID, MtimeNS: todirMeta.MtimeNS,
				},
				{
					FileID: 32, RelPath: "link-abs", LinkTarget: "/etc/passwd",
					Size: absMeta.Size, Mode: 0o777, UID: ownerUID, GID: ownerGID, MtimeNS: absMeta.MtimeNS,
				},
			},
			Dirs: []StageDir{{DirID: 6, RelPath: "emptydir", Mode: 0o750, UID: ownerUID, GID: ownerGID, MtimeNS: 4000}},
		},
		Export: func() EmbeddedExport {
			b := []byte("real fake export bytes")
			p := filepath.Join(t.TempDir(), media.ExportFileName)
			writeFile(t, p, b, 0o644)
			return EmbeddedExport{Path: p, Size: int64(len(b)), Hash: hashOf(b)}
		}(),
		Manifest: ManifestSeed{
			ToolVersion:   "test",
			SchemaVersion: 1,
			CreatedAt:     mustTime(t),
			ChunkedFiles: []media.ChunkedFileInput{{
				RelPath: "big.bin", WholeSize: bigMeta.Size, WholeSHA256: bigHash[:],
			}},
			Placement: []media.PlacementInput{{ImageNumber: 1, Paths: placementPaths}},
		},
		Store: store,
	}
	return f
}

// hashOf is the raw sha256 of a byte slice.
func hashOf(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// TestBuildRealXorrisoEndToEnd is the T4.3 acceptance e2e (12.6):
// master the fixture with the REAL xorriso, then prove the finalized
// image round-trips through the verify primitive against the STORED
// content list - files, chunks, the symlink, the empty dir, the
// manifest, and the export are all inside the .iso and hash-match.
func TestBuildRealXorrisoEndToEnd(t *testing.T) {
	x := realToolOrSkip(t)
	f := newRealFixture(t)
	deps := Deps{Master: x, Extractor: x, HashStore: f.store}

	res, err := Build(context.Background(), deps, f.in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	finalISO := filepath.Join(f.in.DestDir, "S-REAL-001.iso")
	// The atomic finalize: the final name exists, no .partial, and the
	// result carries the honest figures.
	if res.ISOPath != finalISO {
		t.Fatalf("ISOPath = %q, want %q", res.ISOPath, finalISO)
	}
	if fi, err := os.Stat(finalISO); err != nil {
		t.Fatalf("final .iso missing: %v", err)
	} else if fi.Size() != res.BytesWritten || res.BytesWritten == 0 {
		t.Fatalf("BytesWritten = %d, stat says %d", res.BytesWritten, fi.Size())
	}
	if _, err := os.Stat(finalISO + ".partial"); !os.IsNotExist(err) {
		t.Fatalf(".partial left behind after finalize: %v", err)
	}

	// THE CONTENT LIST the step-3 transaction stored (read back from
	// the fake store - the SQL half is pinned in the catalog tier).
	if len(f.store.begins) != 1 {
		t.Fatalf("BeginBuild calls = %d, want 1", len(f.store.begins))
	}
	rows := f.store.begins[0]
	rowByPath := map[string]ContentRow{}
	for _, r := range rows {
		rowByPath[r.MediaPath] = r
	}
	for _, want := range []string{
		"files/hello.txt", "files/setuid.sh", "files/sub/nested.txt",
		"files/" + "üñí-ø.txt", "files/link", "files/emptydir",
		"chunks/big.bin/00001", "chunks/big.bin/00002",
		media.ManifestFileName, media.ExportFileName,
	} {
		if _, ok := rowByPath[want]; !ok {
			t.Errorf("content list lacks %q:\n%s", want, describeRows(rows))
		}
	}
	manifestRow := rowByPath[media.ManifestFileName]
	if manifestRow.Size == 0 || len(manifestRow.Hash) != 32 {
		t.Errorf("manifest self-row = %+v, want a size and a 32-byte hash", manifestRow)
	}
	exportRow := rowByPath[media.ExportFileName]
	if exportRow.Size != f.in.Export.Size || !bytes.Equal(exportRow.Hash, f.in.Export.Hash) {
		t.Errorf("export self-row = %+v, want the produced export's size and hash", exportRow)
	}
	if len(f.store.fails) != 0 || len(f.store.ends) != 1 {
		t.Fatalf("state transitions = (fails %d, finishes %d), want (0, 1)", len(f.store.fails), len(f.store.ends))
	}

	// THE ROUND-TRIP: extract the FINAL .iso through the verify
	// primitive and compare it against the STORED content list with
	// the production comparison - files and chunks by hash, symlink by
	// target, dirs by existence, manifest and export by hash.
	extractDir := filepath.Join(t.TempDir(), "extract")
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ext, err := verify.ExtractAndHash(context.Background(), x.tool, finalISO, extractDir, nil, verify.Totals{})
	if err != nil {
		t.Fatalf("extracting the finalized image: %v", err)
	}
	targets := map[string]string{
		"files/link":          "hello.txt",
		"files/link-dangling": "missing-target.txt",
		"files/link-todir":    "sub",
		"files/link-abs":      "/etc/passwd",
	}
	if problems := compareContents(rows, ext.Entries, targets); len(problems) != 0 {
		t.Fatalf("the finalized image does not round-trip against its content list:\n  %s",
			strings.Join(problems, "\n  "))
	}

	// THE SYMLINK EXTRACTION VERDICT, pinned end to end through the
	// REAL pipeline: osirrox restores symlinks AS SYMLINKS - the
	// dangling link is not dropped and its target is not rewritten,
	// the to-dir link is not materialized as a directory copy, and
	// the absolute-target link keeps its target verbatim. The step-7
	// hash walk must have recorded each as KindSymlink (compareContents
	// above already matched targets); assert the on-disk extraction
	// shape directly.
	for _, tc := range []struct{ rel, wantTarget string }{
		{"link-dangling", "missing-target.txt"},
		{"link-todir", "sub"},
		{"link-abs", "/etc/passwd"},
	} {
		fi, err := os.Lstat(filepath.Join(extractDir, "files", tc.rel))
		if err != nil {
			t.Fatalf("lstat extracted %s: %v (osirrox dropped it?)", tc.rel, err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("extracted %s mode = %v, want a SYMLINK (osirrox dereferenced or materialized it)", tc.rel, fi.Mode())
			continue
		}
		got, err := os.Readlink(filepath.Join(extractDir, "files", tc.rel))
		if err != nil {
			t.Fatalf("readlink extracted %s: %v", tc.rel, err)
		}
		if got != tc.wantTarget {
			t.Errorf("extracted %s target = %q, want %q", tc.rel, got, tc.wantTarget)
		}
	}

	// The setuid fidelity through master + Rock Ridge + extraction
	// (the metadata chain the fixture pins end to end). Go's
	// fs.FileMode carries setuid in os.ModeSetuid (bit 23), NOT in the
	// POSIX 0o4000 bit - the kernel stat is the ground truth (the
	// staged tree carries raw 4755; fsmeta's chain is pinned there).
	setuidInfo, err := os.Lstat(filepath.Join(extractDir, "files", "setuid.sh"))
	if err != nil {
		t.Fatalf("stat of the extracted setuid file: %v", err)
	}
	if setuidInfo.Mode()&os.ModeSetuid == 0 {
		t.Errorf("extracted setuid.sh mode = %v, want the setuid bit preserved through the ISO", setuidInfo.Mode())
	}

	// The manifest carries the D-04 families/whole-set sections we
	// seeded: parse it back out of the image and spot-check the header.
	manifestBytes, err := os.ReadFile(filepath.Join(extractDir, media.ManifestFileName))
	if err != nil {
		t.Fatalf("reading the extracted manifest: %v", err)
	}
	if !bytes.Contains(manifestBytes, []byte(`"set_id": "S-REAL"`)) ||
		!bytes.Contains(manifestBytes, []byte(`"image_number": 1`)) ||
		!bytes.Contains(manifestBytes, []byte("chunks/big.bin/00001")) {
		t.Errorf("extracted manifest lacks the expected header/chunk entries:\n%s", manifestBytes)
	}
	// The export member embeds the produced bytes verbatim.
	exportBytes, err := os.ReadFile(filepath.Join(extractDir, media.ExportFileName))
	if err != nil {
		t.Fatalf("reading the extracted export: %v", err)
	}
	wantExport, err := os.ReadFile(f.in.Export.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(exportBytes, wantExport) {
		t.Error("the extracted catalog-export.sqlite differs from the produced export")
	}
}

// TestBuildRealVolumeIDIsSetIDPlusNumber (gated) pins the 2026-11-04
// stakeholder amendment against the REAL bytes: the mastered image's
// PVD volume id is "<SET-ID>-<NNN>" - the same <NNN> the file name
// carries - read back with `xorriso -indev <iso> -pvd_info` through
// the Tool seam (this package never execs directly; the only os/exec
// here is the LookPath gate in build_real_tool_e2e_test.go). Before
// the amendment the label was the BARE set id, which is exactly the
// multi-disc ambiguity the stakeholder complained about.
func TestBuildRealVolumeIDIsSetIDPlusNumber(t *testing.T) {
	x := realToolOrSkip(t)
	f := newRealFixture(t)
	deps := Deps{Master: x, Extractor: x, HashStore: f.store}
	if _, err := Build(context.Background(), deps, f.in); err != nil {
		t.Fatalf("Build: %v", err)
	}
	finalISO := filepath.Join(f.in.DestDir, "S-REAL-001.iso")

	res, err := x.tool.Run(context.Background(), []string{"-indev", finalISO, "-pvd_info"})
	if err != nil {
		t.Fatalf("xorriso -indev %s -pvd_info: %v", finalISO, err)
	}
	// File-name parity is asserted by construction: the label must
	// equal the file's base name minus ".iso" (<SET-ID>-001.iso ->
	// <SET-ID>-001).
	want := strings.TrimSuffix(filepath.Base(finalISO), ".iso")
	got, ok := pvdVolumeID(string(res.Output))
	if !ok {
		t.Fatalf("no \"Volume Id\" line in xorriso -pvd_info output:\n%s", res.Output)
	}
	if got != want {
		t.Errorf("PVD volume id = %q, want %q (2026-11-04 <SET-ID>-<NNN> amendment; file name %q)",
			got, want, filepath.Base(finalISO))
	}
	// The complaint that started this, spelled out: a bare set id is
	// the OLD label and must never be what mounts.
	if got == f.in.SetID {
		t.Errorf("PVD volume id %q is the bare set id; multi-disc sets would mount indistinguishably", got)
	}
	t.Logf("xorriso -pvd_info volume id: %q (image %s)", got, filepath.Base(finalISO))
}

// pvdVolumeID extracts the PVD's volume id from
// `xorriso -indev <iso> -pvd_info` output: the "Volume Id ..." line of
// the PVD block. The earlier summary line "Volume id    : '<value>'"
// is deliberately NOT matched (different case, quoted - it carries the
// same value, so matching either would be honest, but one pinned shape
// is what a regression should trip on).
func pvdVolumeID(out string) (string, bool) {
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "Volume Id") {
			if _, v, found := strings.Cut(line, ":"); found {
				return strings.TrimSpace(v), true
			}
		}
	}
	return "", false
}

// TestBuildRealCorruptedImageFailsVerification is the T4.3 acceptance
// corruption test (12.6, FR-021 failure reporting): bytes of the
// .partial are flipped in the window BETWEEN master and self-verify
// (the Deps.AfterMaster seam - the exact window a crash-corrupted
// image occupies), and step 7 must fail the image: state 'failed', the
// .partial KEPT, and no final .iso name.
func TestBuildRealCorruptedImageFailsVerification(t *testing.T) {
	x := realToolOrSkip(t)
	f := newRealFixture(t)

	deps := Deps{
		Master: x, Extractor: x, HashStore: f.store,
		AfterMaster: func(ctx context.Context, partialPath string) error {
			corruptFile(t, partialPath)
			return nil
		},
	}
	_, err := Build(context.Background(), deps, f.in)
	if err == nil {
		t.Fatal("Build of a corrupted image must fail at the self-verify")
	}
	var mismatch *VerifyMismatchError
	if !errors.As(err, &mismatch) && !strings.Contains(err.Error(), "extracting image") {
		// Either a hash mismatch list (the image is still parseable)
		// or an extraction failure (the corruption hit xorriso's own
		// structures); both are honest step-7 failures, anything else
		// is not the verification catching it.
		t.Fatalf("err = %v, want a verification failure (mismatch or extraction failure)", err)
	}
	if len(f.store.ends) != 0 {
		t.Fatal("a corrupted image must never reach FinishBuild")
	}
	if len(f.store.fails) != 1 {
		t.Errorf("MarkFailed calls = %d, want 1 (state 'failed')", len(f.store.fails))
	}
	partial := filepath.Join(f.in.DestDir, "S-REAL-001.iso.partial")
	if _, err := os.Stat(partial); err != nil {
		t.Errorf(".partial must be KEPT after a verification failure (6.3 step 4): %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.in.DestDir, "S-REAL-001.iso")); !os.IsNotExist(err) {
		t.Error("a verification failure must never produce a final .iso name")
	}
}

// TestBuildRealFileDataCorruptionFailsHashWalk is the STRENGTHENED
// corruption test (the blind-many-flip recipe above proves broad
// damage is caught SOMEHOW - and its first flip lands exactly on the
// PVD at offset 32768, so detection usually comes from xorriso failing
// to PARSE the image, not from the data hash walk). This test proves
// the hash walk itself: a byte is flipped INSIDE hello.txt's file
// DATA, located by the payload's unique byte pattern (the image holds
// the content exactly once - asserted), so the ISO stays fully
// parseable and every structure (PVD, directory records, path tables,
// Rock Ridge) is untouched. Step 7 MUST catch it as a per-file
// content-hash mismatch naming exactly that file - an extraction
// failure here would be the wrong kind of right.
func TestBuildRealFileDataCorruptionFailsHashWalk(t *testing.T) {
	x := realToolOrSkip(t)
	f := newRealFixture(t)

	const payload = "hello backup tool\n" // hello.txt's unique content
	deps := Deps{
		Master: x, Extractor: x, HashStore: f.store,
		AfterMaster: func(ctx context.Context, partialPath string) error {
			img, err := os.ReadFile(partialPath)
			if err != nil {
				return err
			}
			needle := []byte(payload)
			first := bytes.Index(img, needle)
			if first < 0 {
				t.Fatalf("payload %q not found in the image", payload)
			}
			if bytes.Index(img[first+1:], needle) >= 0 {
				t.Fatalf("payload %q is not unique in the image; the targeted flip cannot pin a data region", payload)
			}
			// Flip ONE byte INSIDE the file's data extent.
			f, err := os.OpenFile(partialPath, os.O_RDWR, 0)
			if err != nil {
				return err
			}
			defer f.Close()
			off := int64(first + 3)
			if _, err := f.WriteAt([]byte{0xff}, off); err != nil {
				return err
			}
			return f.Sync()
		},
	}

	_, err := Build(context.Background(), deps, f.in)
	var mismatch *VerifyMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("err = %v (%T), want a *VerifyMismatchError from the hash walk (the image parses; a data flip must surface as a hash mismatch, not an extraction failure)", err, err)
	}
	// The problems name EXACTLY the corrupted file, by content hash -
	// and nothing else: the rest of the image is intact and verified.
	if len(mismatch.Problems) != 1 ||
		!strings.Contains(mismatch.Problems[0], "files/hello.txt") ||
		!strings.Contains(mismatch.Problems[0], "content hash") {
		t.Errorf("mismatch problems = %v, want exactly one content-hash problem on files/hello.txt", mismatch.Problems)
	}
	// The failure contract: state 'failed', the .partial KEPT, no
	// final .iso, no FinishBuild.
	if len(f.store.fails) != 1 || len(f.store.ends) != 0 {
		t.Errorf("state writes = (fails %d, finishes %d), want (1, 0)", len(f.store.fails), len(f.store.ends))
	}
	partial := filepath.Join(f.in.DestDir, "S-REAL-001.iso.partial")
	if _, err := os.Stat(partial); err != nil {
		t.Errorf(".partial must be KEPT after the hash-walk failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(f.in.DestDir, "S-REAL-001.iso")); !os.IsNotExist(err) {
		t.Error("a hash-walk failure must never produce a final .iso name")
	}
}

// TestBuildRealXorrisoFourPhaseProgress (gated) pins the 6.2
// four-phase progress model through the REAL pipeline (requirement f):
// the captured plain-renderer stream shows all four named phases IN
// ORDER - build(stage) -> build(master) -> build(extract) ->
// build(verify) - and each phase's line carries its determinate form
// and its EXACT/estimate total: stage and verify in the counts+bytes
// form (ItemDone-driven), master and extract in the pinned bytes-only
// form (BytesWritten-driven). The master total is the RequiredImageBytes
// ESTIMATE over the STORED content list; the extract and verify totals
// are the EXACT regular-file byte figure of the same list.
//
// Cadence note ("where cadence allows" of the brief): the renderer
// runs with a STEP CLOCK (every event advances the fake clock by
// PlainInterval), so every event lands in its own 5s window and the
// whole stream is observable regardless of how fast the real xorriso
// completes. The cadence itself is pinned in internal/progress; this
// test pins the SERVICE's event stream, rendered.
func TestBuildRealXorrisoFourPhaseProgress(t *testing.T) {
	x := realToolOrSkip(t)
	f := newRealFixture(t)

	w := &syncBuffer{}
	clk := &stepClock{}
	bus := progress.NewBus()
	wait := progress.Start(progress.New(progress.Options{Mode: progress.ModePlain, Stderr: w, Now: clk.Now}), bus)
	deps := Deps{
		Master: x, Extractor: x, HashStore: f.store, Bus: bus,
		// A 1ms watcher interval so the short real mastering still
		// yields byte positions (production default: 1s - watch.go).
		MasterWatch: MasterWatch{Interval: time.Millisecond},
	}

	if _, err := Build(context.Background(), deps, f.in); err != nil {
		t.Fatalf("Build: %v", err)
	}
	bus.Finish(progress.Done{Summary: "build: 1 image(s) built (real four-phase)"})
	wait()
	out := w.String()

	// THE PHASE ORDER: each named phase's line appears, in sequence.
	prev := -1
	for _, prefix := range []string{"build(stage): ", "build(master): ", "build(extract): ", "build(verify): "} {
		idx := strings.Index(out, prefix)
		if idx < 0 {
			t.Fatalf("no %q line in the captured stream:\n%s", prefix, out)
		}
		if idx <= prev {
			t.Fatalf("phase %q does not follow the previous phase in the stream:\n%s", prefix, out)
		}
		prev = idx
	}

	// The EXACT totals, derived from the STORED content list (the
	// same rows the step-3 transaction wrote).
	tot := verifyTotalsOfRows(f.store.begins[0])
	stageFiles, stageBytes := stageTotals(f.in.Inventory)

	lineWith := func(prefix string) string {
		i := strings.Index(out, prefix)
		if i < 0 {
			t.Fatalf("no %q line in the captured stream:\n%s", prefix, out)
		}
		rest := out[i:]
		if j := strings.IndexByte(rest, '\n'); j >= 0 {
			rest = rest[:j]
		}
		return rest
	}

	// build(stage): counts+bytes form against the inventory totals.
	if l := lineWith("build(stage): "); !strings.Contains(l, "files 1/"+fmt.Sprint(stageFiles)+", ") {
		t.Errorf("stage line %q lacks the inventory file total %d", l, stageFiles)
	} else if want := units.FormatBytes(stageBytes); !strings.Contains(l, "/"+want+" (") {
		t.Errorf("stage line %q lacks the inventory byte total %s", l, want)
	}

	// build(master): the pinned BYTES-ONLY form; the total is the
	// canonical 4.3 RequiredImageBytes estimate over the content list.
	estimate := units.RequiredImageBytes(tot.Bytes, tot.Entries-2)
	if l := lineWith("build(master): "); strings.Contains(l, "files ") {
		t.Errorf("master line carries a file count; byte-driven phases are bytes-only (9.1): %q", l)
	} else if want := units.FormatBytes(estimate); !strings.Contains(l, "/"+want+" (") {
		t.Errorf("master line %q lacks the RequiredImageBytes estimate total %s", l, want)
	}

	// build(extract): the pinned BYTES-ONLY form; the total is the
	// EXACT regular-file byte figure (the tree-sum converges on it).
	if l := lineWith("build(extract): "); strings.Contains(l, "files ") {
		t.Errorf("extract line carries a file count; byte-driven phases are bytes-only (9.1): %q", l)
	} else if want := units.FormatBytes(tot.Bytes); !strings.Contains(l, "/"+want+" (") {
		t.Errorf("extract line %q lacks the exact byte total %s", l, want)
	}

	// build(verify): counts+bytes form with the EXACT totals from the
	// first line (the phase opens determinate, 6.2).
	if l := lineWith("build(verify): "); !strings.Contains(l, "files 1/"+fmt.Sprint(tot.Files)+", ") ||
		!strings.Contains(l, "/"+units.FormatBytes(tot.Bytes)+" (") {
		t.Errorf("verify line %q lacks the exact content-list totals (%d files, %d bytes)", l, tot.Files, tot.Bytes)
	}

	if !strings.Contains(out, "build: 1 image(s) built (real four-phase)") {
		t.Errorf("summary line missing:\n%s", out)
	}
}

// corruptFile flips many bytes across the image's data region (after
// the 32 KiB ISO system area), the brief's robust recipe: whether the
// damage surfaces as a per-file hash mismatch or as an xorriso
// extraction FAILURE, step 7 must catch it.
func corruptFile(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("opening the image for corruption: %v", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	const systemArea = 32 << 10 // 16 sectors of ISO system area stay untouched
	if fi.Size() <= systemArea+16 {
		t.Fatalf("image too small to corrupt: %d bytes", fi.Size())
	}
	// 64 flips spread across the file body.
	buf := []byte{0xff}
	for i := 0; i < 64; i++ {
		off := systemArea + int64(i)*(fi.Size()-systemArea)/64
		if _, err := f.WriteAt(buf, off); err != nil {
			t.Fatalf("flipping a byte at %d: %v", off, err)
		}
	}
	if err := f.Sync(); err != nil {
		t.Fatal(err)
	}
}
