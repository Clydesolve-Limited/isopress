package restore

// T5.4 acceptance for the REAL ImageRestorer (DESIGN_SPEC 8.4 extraction and
// chunked reassembly with double verification, 5.4 metadata; IMPLEMENTATION_PLAN
// 4.6 T5.4). Every test drives the production pipeline - ResolveSelection over
// the Store seam, restore.Run's one-at-a-time loop, NewImageRestorer writing
// real bytes into a real target - against copy trees built in the 5.3 layout
// (files/<relpath>, chunks/<relpath>/NNNNN), because a copy tree IS one of the
// two supply kinds and needs no xorriso.
//
// The pinned contracts, one test each (see doc.go's T5.4 section):
//   - the whole-file write discipline (partial -> hash -> fsync -> rename ->
//     dir fsync -> metadata) and the progress line it fills;
//   - the 8.4 mismatch deletion (whole file) and BOTH chunked verification
//     gates (first: stored chunk hash; second: the assembled read-back);
//   - NB-03 cross-image assembly: the workspace is wiped between calls while
//     the target's .partial survives and GROWS (the probeRestorer witnesses);
//   - NB-04 count-once: a spanning member counts at its completion image, a
//     failed one at the detecting image, never twice;
//   - the FR-041 incomplete detail (missing/corrupt chunk indices listed, no
//     final file, .partial left vs deleted per the documented decision);
//   - hard-link families both ways (selected primary; secondary-only), the
//     symlink variants and the no-clobber collision, empty-dir idempotence;
//   - metadata fidelity incl. the 04755 setuid conversion chain and the
//     non-root EPERM chown warning;
//   - cancellation mid-assembly, the B1-mirror partial-collision gate;
//   - the ISO source path through a fake xorriso (the single -extract /
//     argv shape), the 7.2 free-space pre-check, and the image-level faults.

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"

	"isopress/internal/media"
	"isopress/internal/progress"
	"isopress/internal/xorriso"
)

// fixtureMtimeNS is the recorded mtime every fixture member carries
// (nanosecond precision - the round-trip must not flatten it).
const fixtureMtimeNS int64 = 1_650_000_000_123_456_789

// --- fixture ------------------------------------------------------------------

// restoreFixture builds copy trees plus the catalog-shaped views the
// selection reads (the fakeStore of helpers_test.go), with the source bytes
// planted exactly where the 5.3 layout puts them.
type restoreFixture struct {
	t         *testing.T
	setID     string
	store     *fakeStore
	treeRoot  string
	nextFile  int64
	nextChunk int64
	placement map[int64][]string
	claim     map[string]int64 // relpath -> the image claiming it (first claim wins)
	bus       *progress.Bus
}

func newRestoreFixture(t *testing.T, setID string) *restoreFixture {
	return &restoreFixture{
		t:         t,
		setID:     setID,
		store:     &fakeStore{},
		treeRoot:  t.TempDir(),
		placement: map[int64][]string{},
		claim:     map[string]int64{},
	}
}

// tree is one image's copy-tree directory (the <root>/<SET-ID>-NNN shape, so
// --copy-tree-root discovery supplies it through the real supply path).
func (f *restoreFixture) tree(image int64) string {
	f.t.Helper()
	name, err := media.ImageDirName(f.setID, image)
	if err != nil {
		f.t.Fatalf("ImageDirName: %v", err)
	}
	dir := filepath.Join(f.treeRoot, name)
	mkdir(f.t, dir)
	if _, ok := f.placement[image]; !ok {
		f.placement[image] = nil
	}
	return dir
}

// claimPath records one payload relpath under one image (the placement
// summary plus the patchHash lookup key).
func (f *restoreFixture) claimPath(image int64, rel string) {
	if _, ok := f.claim[rel]; !ok {
		f.claim[rel] = image
	}
	f.placement[image] = append(f.placement[image], rel)
}

// writeMedia writes one member at a 5.3 media path (relative to the image
// root) inside one image's tree.
func (f *restoreFixture) writeMedia(image int64, mediaRel string, content []byte) {
	f.t.Helper()
	dir := filepath.Join(f.tree(image), filepath.Dir(filepath.FromSlash(mediaRel)))
	mkdir(f.t, dir)
	if err := os.WriteFile(filepath.Join(f.tree(image), filepath.FromSlash(mediaRel)), content, 0o644); err != nil {
		f.t.Fatalf("writing tree member %s: %v", mediaRel, err)
	}
}

// addInv mutates one image's inventory in place (the map returns copies).
func (f *restoreFixture) addInv(image int64, mutate func(*Inventory)) {
	if f.store.inventory == nil {
		f.store.inventory = map[int64]Inventory{}
	}
	inv := f.store.inventory[image]
	mutate(&inv)
	f.store.inventory[image] = inv
}

// addFile plants one whole regular file with the fixture-default metadata
// (0644, the test user's uid/gid - so the unconditional chown succeeds).
func (f *restoreFixture) addFile(image int64, rel string, content []byte) {
	f.addFileMeta(image, rel, content, 0o644, uint32(os.Getuid()), uint32(os.Getgid()), fixtureMtimeNS)
}

// addFileMeta plants a whole regular file with explicit RAW POSIX metadata
// (the catalog column shape).
func (f *restoreFixture) addFileMeta(image int64, rel string, content []byte, mode, uid, gid uint32, mtimeNS int64) {
	f.t.Helper()
	sum := sha256.Sum256(content)
	f.writeMedia(image, "files/"+rel, content)
	id := f.nextFile
	f.nextFile++
	f.store.records = append(f.store.records, Record{
		FileID: id, RelPath: rel, EntryKind: "regular",
		Size: int64(len(content)), Hash: sum[:],
	})
	f.addInv(image, func(inv *Inventory) {
		inv.Files = append(inv.Files, FileItem{
			FileID: id, RelPath: rel, Size: int64(len(content)),
			Mode: mode, UID: uid, GID: gid, MtimeNS: mtimeNS, Nlink: 1, Hash: sum[:],
		})
	})
	f.claimPath(image, rel)
}

// addSymlink plants a symlink record plus the link itself in the tree (the
// restorer reads the TARGET from the record, never from the source - this
// write is fixture realism only).
func (f *restoreFixture) addSymlink(image int64, rel, target string) {
	f.t.Helper()
	link := filepath.Join(f.tree(image), "files", filepath.FromSlash(rel))
	mkdir(f.t, filepath.Dir(link))
	if err := os.Symlink(target, link); err != nil {
		f.t.Fatalf("planting tree symlink %s -> %s: %v", rel, target, err)
	}
	id := f.nextFile
	f.nextFile++
	f.store.records = append(f.store.records, Record{
		FileID: id, RelPath: rel, EntryKind: "symlink",
		Size: int64(len(target)), LinkTarget: target,
	})
	f.addInv(image, func(inv *Inventory) {
		inv.Symlinks = append(inv.Symlinks, SymlinkItem{
			FileID: id, RelPath: rel, LinkTarget: target, Size: int64(len(target)),
			Mode: 0o777, UID: uint32(os.Getuid()), GID: uint32(os.Getgid()), MtimeNS: fixtureMtimeNS,
		})
	})
	f.claimPath(image, rel)
}

// addDir plants an EMPTY directory record (FR-037) and its tree directory.
func (f *restoreFixture) addDir(image int64, rel string, mode uint32) {
	f.t.Helper()
	mkdir(f.t, filepath.Join(f.tree(image), "files", filepath.FromSlash(rel)))
	id := f.nextFile
	f.nextFile++
	f.store.dirs = append(f.store.dirs, DirRecord{DirID: id, RelPath: rel})
	f.addInv(image, func(inv *Inventory) {
		inv.Dirs = append(inv.Dirs, DirItem{
			DirID: id, RelPath: rel, Mode: mode,
			UID: uint32(os.Getuid()), GID: uint32(os.Getgid()), MtimeNS: fixtureMtimeNS,
		})
	})
	f.claimPath(image, rel)
}

// addFamily plants a hard-link family the way the catalog derives it: the
// primary is an ordinary whole-file member, every other name arrives as a
// Work Secondary carrying PrimaryRelPath and the shared family hash.
func (f *restoreFixture) addFamily(image int64, primary string, secondaries []string, content []byte) {
	f.t.Helper()
	sum := sha256.Sum256(content)
	f.writeMedia(image, "files/"+primary, content)

	pid := f.nextFile
	f.nextFile++
	f.store.records = append(f.store.records, Record{
		FileID: pid, RelPath: primary, EntryKind: "regular",
		Size: int64(len(content)), Hash: sum[:],
	})
	f.addInv(image, func(inv *Inventory) {
		inv.Files = append(inv.Files, FileItem{
			FileID: pid, RelPath: primary, Size: int64(len(content)),
			Mode: 0o644, UID: uint32(os.Getuid()), GID: uint32(os.Getgid()),
			MtimeNS: fixtureMtimeNS, Nlink: int64(1 + len(secondaries)), Hash: sum[:],
		})
	})
	f.claimPath(image, primary)

	for _, sec := range secondaries {
		// The copy tree holds the secondary as link(2) of the primary
		// (D-04); restore never reads it - the bytes live at the primary's
		// name - but the tree mirrors production either way.
		secPath := filepath.Join(f.tree(image), "files", filepath.FromSlash(sec))
		mkdir(f.t, filepath.Dir(secPath))
		if err := os.Link(filepath.Join(f.tree(image), "files", filepath.FromSlash(primary)), secPath); err != nil {
			f.t.Fatalf("planting tree hard link %s: %v", sec, err)
		}
		id := f.nextFile
		f.nextFile++
		f.store.records = append(f.store.records, Record{
			FileID: id, RelPath: sec, EntryKind: "regular",
			Size: int64(len(content)), Hash: sum[:],
		})
		f.addInv(image, func(inv *Inventory) {
			inv.Secondaries = append(inv.Secondaries, LinkItem{
				FileID: id, RelPath: sec, Size: int64(len(content)),
				Mode: 0o644, UID: uint32(os.Getuid()), GID: uint32(os.Getgid()),
				MtimeNS: fixtureMtimeNS, Hash: sum[:],
				PrimaryFileID: pid, PrimaryRelPath: primary,
			})
		})
		f.claimPath(image, sec)
	}
}

// chunkPart is one image's share of a chunked parent.
type chunkPart struct {
	image   int64
	content []byte
}

// addChunked plants a chunked parent split across images (parts in ascending
// catalog index order; part i goes to parts[i].image), with every anchor the
// 8.4 engine consumes: per-chunk stored hash, offsets, parent metadata, the
// parent's whole-file hash, and ParentNumChunks.
func (f *restoreFixture) addChunked(rel string, parts []chunkPart) {
	f.addChunkedHash(rel, parts, nil)
}

// addChunkedHash is addChunked with an explicit parent whole-file hash (the
// SYNTHETIC second-verification seed: chunks individually valid, assembly
// judged against a hash they cannot produce).
func (f *restoreFixture) addChunkedHash(rel string, parts []chunkPart, parentHash []byte) {
	f.t.Helper()
	var whole []byte
	for _, p := range parts {
		whole = append(whole, p.content...)
	}
	sum := sha256.Sum256(whole)
	hash := sum[:]
	if parentHash != nil {
		hash = parentHash
	}
	fileID := f.nextFile
	f.nextFile++
	f.store.records = append(f.store.records, Record{
		FileID: fileID, RelPath: rel, EntryKind: "regular",
		Size: int64(len(whole)), Hash: hash,
	})

	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	off := int64(0)
	for i, p := range parts {
		name, err := media.ChunkFileName(int64(i))
		if err != nil {
			f.t.Fatalf("ChunkFileName: %v", err)
		}
		f.writeMedia(p.image, media.ChunksRoot+"/"+rel+"/"+name, p.content)
		csum := sha256.Sum256(p.content)
		chunkID := f.nextChunk
		f.nextChunk++
		image := p.image
		f.addInv(image, func(inv *Inventory) {
			inv.Chunks = append(inv.Chunks, ChunkItem{
				ChunkID: chunkID, FileID: fileID, ChunkIndex: i,
				Offset: off, Length: int64(len(p.content)), Hash: csum[:],
				ParentRelPath: rel, ParentSize: int64(len(whole)),
				ParentMode: 0o644, ParentUID: uid, ParentGID: gid,
				ParentMtimeNS: fixtureMtimeNS,
				ParentHash:    hash, ParentNumChunks: len(parts),
			})
		})
		off += int64(len(p.content))
		f.claimPath(p.image, rel)
	}
}

// patchHash rewrites the stored whole-file hash of one WHOLE-FILE member
// (record + inventory row): the synthetic "the anchor says X, the media
// holds Y" seed. Chunked parents seed through addChunkedHash instead.
func (f *restoreFixture) patchHash(rel string, hash []byte) {
	f.t.Helper()
	found := false
	for i := range f.store.records {
		if f.store.records[i].RelPath == rel && f.store.records[i].EntryKind == "regular" {
			f.store.records[i].Hash = hash
			found = true
		}
	}
	image, ok := f.claim[rel]
	if !ok {
		f.t.Fatalf("patchHash: %q is not claimed by any fixture image", rel)
	}
	inv := f.store.inventory[image]
	for i := range inv.Files {
		if inv.Files[i].RelPath == rel {
			inv.Files[i].Hash = hash
			found = true
		}
	}
	f.store.inventory[image] = inv
	if !found {
		f.t.Fatalf("patchHash: no whole-file row for %q", rel)
	}
}

// corruptTreeFile flips ONE byte of a tree member in place, preserving its
// length: media corruption the stored hashes must catch.
func (f *restoreFixture) corruptTreeFile(image int64, mediaRel string) {
	f.t.Helper()
	path := filepath.Join(f.tree(image), filepath.FromSlash(mediaRel))
	data, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatalf("reading %s to corrupt it: %v", mediaRel, err)
	}
	if len(data) == 0 {
		f.t.Fatalf("corruptTreeFile: %s is empty; flip a byte needs content", mediaRel)
	}
	data[0] ^= 0xFF
	if err := os.WriteFile(path, data, 0o644); err != nil {
		f.t.Fatalf("writing corrupted %s: %v", mediaRel, err)
	}
}

// finish rebuilds the placement summary (ascending images, sorted deduped
// paths) - what catalog.CopyPlacement hands the selection.
func (f *restoreFixture) finish() {
	imgs := make([]int64, 0, len(f.placement))
	for id := range f.placement {
		imgs = append(imgs, id)
	}
	sort.Slice(imgs, func(i, j int) bool { return imgs[i] < imgs[j] })
	out := make([]ImagePaths, 0, len(imgs))
	for _, id := range imgs {
		paths := append([]string(nil), f.placement[id]...)
		sort.Strings(paths)
		dedup := make([]string, 0, len(paths))
		for i, p := range paths {
			if i == 0 || paths[i-1] != p {
				dedup = append(dedup, p)
			}
		}
		out = append(out, ImagePaths{ImageID: id, Paths: dedup})
	}
	f.store.placement = out
}

// resolve runs the REAL selection resolution over the fixture's views.
func (f *restoreFixture) resolve(t *testing.T, sel Selector) ResolvedSelection {
	t.Helper()
	f.finish()
	if sel.SetID == "" {
		sel.SetID = f.setID
	}
	out, err := ResolveSelection(context.Background(), f.store, sel)
	if err != nil {
		t.Fatalf("ResolveSelection: %v", err)
	}
	return out
}

// runInto performs a REAL restore run (the T5.4 restorer, no fakes) into
// target through the full Run orchestration.
func (f *restoreFixture) runInto(t *testing.T, sel Selector, target string) (Result, error) {
	t.Helper()
	return f.runWith(t, sel, target, NewImageRestorer(RestorerDeps{Bus: f.bus}))
}

// runWith is runInto with an injected seam (the NB-03 probe wrapper).
func (f *restoreFixture) runWith(t *testing.T, sel Selector, target string, restorer ImageRestorer) (Result, error) {
	t.Helper()
	f.finish()
	if sel.SetID == "" {
		sel.SetID = f.setID
	}
	return Run(context.Background(), Input{
		Store:    f.store,
		Restorer: restorer,
		Selector: sel,
		Target:   target,
		Supply:   SupplySpec{CopyTreeRoot: f.treeRoot},
		WorkDir:  tempWorkspaceDir(t),
		Bus:      f.bus,
	})
}

// --- assertion helpers --------------------------------------------------------

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return data
}

func assertAbsent(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s must not exist (%s): stat err = %v", path, why, err)
	}
}

// assertNoPartials fails when any *.partial remains under root.
func assertNoPartials(t *testing.T, root string) {
	t.Helper()
	err := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.IsDir() && strings.HasSuffix(path, partialSuffix) {
			t.Errorf("leftover partial %s (a completed restore leaves only final names)", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
}

// lstatPOSIX returns the raw POSIX mode bits (permission class incl.
// setuid/setgid/sticky), uid, gid, and nanosecond mtime of one entry.
func lstatPOSIX(t *testing.T, path string) (mode uint32, uid, gid uint32, mtimeNS int64) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("lstat %s: FileInfo.Sys() is not *syscall.Stat_t", path)
	}
	return st.Mode & 0o7777, st.Uid, st.Gid, st.Mtim.Nano() // Nano() = Sec*1e9+Nsec, the full epoch-ns
}

func assertMeta(t *testing.T, path string, wantMode, wantUID, wantGID uint32, wantMtimeNS int64) {
	t.Helper()
	mode, uid, gid, mtimeNS := lstatPOSIX(t, path)
	if mode != wantMode {
		t.Errorf("%s mode = %#o, want %#o", path, mode, wantMode)
	}
	if uid != wantUID || gid != wantGID {
		t.Errorf("%s uid:gid = %d:%d, want %d:%d", path, uid, gid, wantUID, wantGID)
	}
	if mtimeNS != wantMtimeNS {
		t.Errorf("%s mtime = %d, want %d (nanoseconds must survive)", path, mtimeNS, wantMtimeNS)
	}
}

// statNlink returns the hard-link count of one entry.
func statNlink(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("lstat %s: FileInfo.Sys() is not *syscall.Stat_t", path)
	}
	return st.Nlink
}

// mkWorkspace creates a standalone workspace for DIRECT RestoreImage calls
// (tests that inspect state BETWEEN images - Run's own workspace is removed
// when the run returns).
func mkWorkspace(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "workspace")
	mkdir(t, dir)
	return dir
}

// --- the whole file: 5.1 discipline + 8.4 anchor + progress -------------------

func TestRestoreWholeFileFromCopyTree(t *testing.T) {
	f := newRestoreFixture(t, "S-RW1")
	content := []byte("hello, restored world\n")
	f.addFileMeta(1, "dir/hello.txt", content, 0o640,
		uint32(os.Getuid()), uint32(os.Getgid()), fixtureMtimeNS)
	target := filepath.Join(t.TempDir(), "target")

	bus, finish := wireBus()
	t.Cleanup(func() { finish("cleanup") }) // once-guarded: drains even on a fatal path
	f.bus = bus
	res, err := f.runInto(t, Selector{}, target)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	stderr := finish("done")

	if res.Images == nil || len(res.Images) != 1 || res.Images[0].Err != nil {
		t.Fatalf("images = %+v, want exactly image 1 with no error", res.Images)
	}
	out := res.Images[0].Out
	if out.Restored != 1 || out.Failed != 0 || out.Skipped != 0 || len(out.Files) != 0 {
		t.Errorf("outcome = %+v, want a clean single-member restore", out)
	}
	if out.Bytes != int64(len(content)) {
		t.Errorf("bytes = %d, want %d", out.Bytes, len(content))
	}

	final := filepath.Join(target, "dir", "hello.txt")
	if got := mustRead(t, final); string(got) != string(content) {
		t.Errorf("restored content = %q, want %q", got, content)
	}
	assertNoPartials(t, target)

	wantUID, wantGID := uint32(os.Getuid()), uint32(os.Getgid())
	assertMeta(t, final, 0o640, wantUID, wantGID, fixtureMtimeNS)

	// The phase T5.3 declared fills with this engine's ItemDone stream.
	if !strings.Contains(stderr, "restore: files 1/1") {
		t.Errorf("progress must render the phase line restore: files 1/1:\n%s", stderr)
	}
}

func TestRestoreWholeFileHashMismatchDeletesPartial(t *testing.T) {
	f := newRestoreFixture(t, "S-RW2")
	f.addFile(1, "data.txt", []byte("what the media actually holds"))
	// The anchor says something else entirely: the 8.4 comparison must fail,
	// delete the partial, and record the member - never publish a final.
	f.patchHash("data.txt", sha256Sum([]byte("what the catalog claims")))
	target := filepath.Join(t.TempDir(), "target")

	res, err := f.runInto(t, Selector{}, target)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := res.Images[0].Out
	if out.Failed != 1 || out.Restored != 0 || len(out.Files) != 1 {
		t.Fatalf("outcome = %+v, want exactly one failed member", out)
	}
	if out.Files[0].Status != statusFailed || !strings.Contains(out.Files[0].Detail, "hash mismatch") {
		t.Errorf("finding = %+v, want a failed member whose detail names the hash mismatch", out.Files[0])
	}
	assertAbsent(t, filepath.Join(target, "data.txt"), "a mismatched member never becomes a final file")
	assertNoPartials(t, target) // 8.4: on mismatch, delete the partial
}

// sha256Sum is the 32-byte digest helper (Sum256 returns an array that cannot
// be sliced at the call site of a fixture literal).
func sha256Sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// --- the second verification: chunks valid, assembly wrong --------------------

func TestRestoreAssembledHashCatchAtSecondVerification(t *testing.T) {
	f := newRestoreFixture(t, "S-RASM")
	p0 := []byte(strings.Repeat("A", 50))
	p1 := []byte(strings.Repeat("B", 30))
	// Every chunk row carries the CORRECT hash of its own bytes (the first
	// verification passes), while the parent's whole-file anchor names a
	// stream the concatenation cannot equal - only the read-back SECOND
	// verification can catch this.
	f.addChunkedHash("big.bin", []chunkPart{{1, p0}, {1, p1}},
		sha256Sum([]byte("a stream that is not the concatenation of the chunks")))
	target := filepath.Join(t.TempDir(), "target")

	res, err := f.runInto(t, Selector{}, target)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := res.Images[0].Out
	if out.Failed != 1 || out.Restored != 0 || len(out.Files) != 1 {
		t.Fatalf("outcome = %+v, want exactly one incomplete member", out)
	}
	got := out.Files[0]
	if got.Status != statusIncomplete {
		t.Errorf("status = %q, want %q (the design's word for the chunked failure)", got.Status, statusIncomplete)
	}
	for _, want := range []string{"second verification", "every chunk passed its stored hash", "was deleted"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail %q must contain %q", got.Detail, want)
		}
	}
	assertAbsent(t, filepath.Join(target, "big.bin"), "the failed assembly must never become a final file")
	// Single-image assembly: the failing call CREATED the partial, so the
	// documented decision DELETES it (nothing verified was lost).
	assertNoPartials(t, target)
}

// --- missing chunk: FR-041's index list ---------------------------------------

func TestRestoreMissingChunkListsIndices(t *testing.T) {
	f := newRestoreFixture(t, "S-RMISS")
	p0 := []byte(strings.Repeat("C", 40))
	p1 := []byte(strings.Repeat("D", 24))
	f.addChunked("big.bin", []chunkPart{{1, p0}, {1, p1}})
	// Media loss: the second chunk file is simply not there.
	if err := os.Remove(filepath.Join(f.tree(1), "chunks", "big.bin", "00002")); err != nil {
		t.Fatalf("removing the chunk from the tree: %v", err)
	}
	target := filepath.Join(t.TempDir(), "target")

	res, err := f.runInto(t, Selector{}, target)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := res.Images[0].Out
	if out.Failed != 1 || len(out.Files) != 1 || out.Files[0].Status != statusIncomplete {
		t.Fatalf("outcome = %+v, want one incomplete member", out)
	}
	detail := out.Files[0].Detail
	for _, want := range []string{"missing chunks [1]", "no final file"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q must contain %q", detail, want)
		}
	}
	assertAbsent(t, filepath.Join(target, "big.bin"), "an incomplete member has no final file")
	assertNoPartials(t, target)
}

// --- NB-03 + NB-04: the cross-image assembly ----------------------------------

// probeRestorer wraps the REAL restorer and snapshots - at EVERY seam call
// entry - the workspace contents and the size of the target's growing
// .partial. It is the NB-03 witness: the workspace must be empty at each
// later entry (8.3 wipe) while the target's assembly grows across calls.
type probeRestorer struct {
	t      *testing.T
	inner  ImageRestorer
	target string
	rel    string

	entries [][]string
	partial []int64 // -1 = absent at that entry
}

func (p *probeRestorer) RestoreImage(ctx context.Context, image ImageSupply, selection ResolvedSelection, workspace string) (ImageRestoreResult, error) {
	entries, err := os.ReadDir(workspace)
	if err != nil {
		p.t.Fatalf("reading the workspace at image %d entry: %v", image.Image, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	p.entries = append(p.entries, names)
	size := int64(-1)
	if fi, err := os.Stat(filepath.Join(p.target, p.rel+partialSuffix)); err == nil {
		size = fi.Size()
	}
	p.partial = append(p.partial, size)
	return p.inner.RestoreImage(ctx, image, selection, workspace)
}

func TestRestoreChunkedAssemblesAcrossWipedWorkspaceAndCountsOnce(t *testing.T) {
	f := newRestoreFixture(t, "S-R3A")
	p0 := []byte(strings.Repeat("1", 400))
	p1 := []byte(strings.Repeat("2", 300))
	p2 := []byte(strings.Repeat("3", 200))
	f.addFile(1, "keep-1.txt", []byte("image 1 also carries this whole file"))
	f.addChunked("big.bin", []chunkPart{{1, p0}, {2, p1}, {3, p2}})
	target := filepath.Join(t.TempDir(), "target")

	probe := &probeRestorer{t: t, inner: NewImageRestorer(RestorerDeps{}), target: target, rel: "big.bin"}
	res, err := f.runWith(t, Selector{}, target, probe)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Images) != 3 {
		t.Fatalf("images = %d, want 3", len(res.Images))
	}

	// NB-03: the workspace is empty at EVERY entry after the first (8.3's
	// between-images wipe), while the TARGET's .partial survived every wipe
	// and grew by exactly each predecessor's share.
	if len(probe.entries) != 3 || len(probe.partial) != 3 {
		t.Fatalf("probe saw %d entries / %d sizes, want 3 / 3", len(probe.entries), len(probe.partial))
	}
	if len(probe.entries[0]) != 0 {
		t.Errorf("image 1 workspace at entry = %v, want empty (fresh run)", probe.entries[0])
	}
	for i := 1; i < 3; i++ {
		if len(probe.entries[i]) != 0 {
			t.Errorf("image %d workspace at entry = %v, want empty (cleaned between images)", i+1, probe.entries[i])
		}
	}
	wantSizes := []int64{-1, int64(len(p0)), int64(len(p0) + len(p1))}
	for i, want := range wantSizes {
		if probe.partial[i] != want {
			t.Errorf("target .partial size at image %d entry = %d, want %d (the assembly persists in the TARGET across the wiped workspace)",
				i+1, probe.partial[i], want)
		}
	}

	// NB-04: counted ONCE, at the completion image. Image 1 counts its own
	// whole file only (the pending parent counts for nothing), image 2
	// counts nothing, image 3 counts the completed parent exactly once.
	restores := []int{res.Images[0].Out.Restored, res.Images[1].Out.Restored, res.Images[2].Out.Restored}
	if restores[0] != 1 || restores[1] != 0 || restores[2] != 1 {
		t.Errorf("per-image Restored = %v, want [1 0 1]: the pending parent counts only at its completion image", restores)
	}
	total := 0
	for _, img := range res.Images {
		total += img.Out.Restored
		if img.Out.Failed != 0 {
			t.Errorf("image %d reported failures on a healthy span: %+v", img.Image, img.Out)
		}
		if len(img.Out.Files) != 0 {
			t.Errorf("image %d listed members on a healthy span: %+v", img.Image, img.Out.Files)
		}
	}
	if total != 2 {
		t.Errorf("summed Restored = %d, want 2 (one whole file + the parent counted ONCE)", total)
	}

	want := append(append(append([]byte{}, p0...), p1...), p2...)
	if got := mustRead(t, filepath.Join(target, "big.bin")); string(got) != string(want) {
		t.Errorf("assembled content wrong: %d bytes, want %d", len(got), len(want))
	}
	assertNoPartials(t, target)
	if got := mustRead(t, filepath.Join(target, "keep-1.txt")); string(got) != "image 1 also carries this whole file" {
		t.Errorf("keep-1.txt = %q", got)
	}
}

func TestRestoreChunkedCorruptChunkIncompleteOnlyThatFile(t *testing.T) {
	f := newRestoreFixture(t, "S-R3I")
	p0 := []byte(strings.Repeat("x", 400))
	p1 := []byte(strings.Repeat("y", 300))
	p2 := []byte(strings.Repeat("z", 200))
	f.addFile(1, "keep-1.txt", []byte("first image keeps this"))
	f.addFile(3, "keep-3.txt", []byte("third image keeps this"))
	f.addChunked("big.bin", []chunkPart{{1, p0}, {2, p1}, {3, p2}})
	// Deliberate media corruption on IMAGE 2 (same length, different bytes):
	// the FIRST verification must reject exactly that chunk.
	f.corruptTreeFile(2, "chunks/big.bin/00002")
	target := filepath.Join(t.TempDir(), "target")

	res, err := f.runInto(t, Selector{}, target)
	if err != nil {
		t.Fatalf("Run (per-file failures never abort the run): %v", err)
	}
	if len(res.Images) != 3 {
		t.Fatalf("images = %d, want 3 (the run continues over every image)", len(res.Images))
	}

	// Image 1: its file restored; the pending parent is neither counted nor
	// listed (NB-04).
	if out := res.Images[0].Out; out.Restored != 1 || out.Failed != 0 || len(out.Files) != 0 {
		t.Errorf("image 1 outcome = %+v, want only its own file restored", out)
	}
	// Image 2: THE failure, recorded once, indices listed (FR-041).
	out2 := res.Images[1].Out
	if out2.Failed != 1 || out2.Restored != 0 || len(out2.Files) != 1 {
		t.Fatalf("image 2 outcome = %+v, want exactly the one incomplete member", out2)
	}
	got := out2.Files[0]
	if got.RelPath != "big.bin" || got.Status != statusIncomplete {
		t.Errorf("finding = %+v, want big.bin marked incomplete", got)
	}
	for _, want := range []string{"corrupt chunks [1]", "first verification", "LEFT IN PLACE", "no final file"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail %q must contain %q", got.Detail, want)
		}
	}
	// Image 3: keeps its own file, and NEVER counts or lists the already-
	// failed parent (count-once for failures too).
	out3 := res.Images[2].Out
	if out3.Restored != 1 || out3.Failed != 0 || len(out3.Files) != 0 {
		t.Errorf("image 3 outcome = %+v, want only its own file restored and the failed parent silent", out3)
	}

	// Totals: the whole run reports ONE failed member and the two healthy
	// files - exit 10 material, nothing more.
	var restored, failed int
	for _, img := range res.Images {
		restored += img.Out.Restored
		failed += img.Out.Failed
	}
	if restored != 2 || failed != 1 {
		t.Errorf("run totals = restored %d / failed %d, want 2 / 1", restored, failed)
	}

	// The target never holds a final file for the incomplete member; the
	// assembly is LEFT IN PLACE (it holds image 1's VERIFIED chunk - the
	// documented leave branch), and it already carries this call's
	// streamed (unverified) bytes at their offsets: the write lands before
	// the hash gate can judge it, so the file is fully sized but inert.
	assertAbsent(t, filepath.Join(target, "big.bin"), "incomplete member must have no final file")
	partial := filepath.Join(target, "big.bin.partial")
	fi, err := os.Stat(partial)
	if err != nil {
		t.Fatalf("the abandoned assembly must be left for diagnosis: %v", err)
	}
	if want := int64(len(p0) + len(p1)); fi.Size() != want {
		t.Errorf("abandoned assembly size = %d, want %d (image 1's verified chunk + this call's streamed bytes)", fi.Size(), want)
	}
	// And the healthy members are all present.
	for _, rel := range []string{"keep-1.txt", "keep-3.txt"} {
		if _, err := os.Stat(filepath.Join(target, rel)); err != nil {
			t.Errorf("healthy member %s missing from the target: %v", rel, err)
		}
	}
}

// --- non-contiguous per-image chunk splits (T5.4 reviewer follow-up) ----------

// TestRestoreChunkedNonContiguousSplitAssemblesAcrossImages pins, permanently,
// the layout the T5.4 go-reviewer verified only in throwaway scratch tests: a
// chunked parent whose chunks land OUT of image order - IMAGE 2 holds chunk 0
// (the first catalog index) while IMAGE 1 holds chunks 1 and 2. Nothing in the
// engine may assume the lowest-numbered image holds the lowest-numbered chunk,
// that a parent's chunks arrive in index order, or that a parent's FIRST
// contribution is its first chunk: image 1 opens the assembly with a HOLE
// where chunk 0 will land later, its share survives the between-images
// workspace wipe, and image 2 - the LAST image processed - completes and
// second-verifies the whole file.
func TestRestoreChunkedNonContiguousSplitAssemblesAcrossImages(t *testing.T) {
	f := newRestoreFixture(t, "S-RNC1")
	c0 := []byte(strings.Repeat("a", 400))
	c1 := []byte(strings.Repeat("b", 300))
	c2 := []byte(strings.Repeat("c", 200))
	f.addFile(1, "keep-1.txt", []byte("image 1 also carries a whole file"))
	// parts are in ascending CATALOG index order, so this reads chunk 0 ->
	// image 2 and chunks 1, 2 -> image 1: the non-contiguous split.
	f.addChunked("big.bin", []chunkPart{{image: 2, content: c0}, {image: 1, content: c1}, {image: 1, content: c2}})
	target := filepath.Join(t.TempDir(), "target")

	res, err := f.runInto(t, Selector{}, target)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Images) != 2 {
		t.Fatalf("images = %d, want 2 (image 1 then image 2)", len(res.Images))
	}
	var restored, failed int
	for _, img := range res.Images {
		if img.Err != nil {
			t.Fatalf("image %d error: %v", img.Image, img.Err)
		}
		restored += img.Out.Restored
		failed += img.Out.Failed
		if img.Out.Failed != 0 || len(img.Out.Files) != 0 {
			t.Errorf("image %d outcome = %+v, want a healthy span with no findings", img.Image, img.Out)
		}
	}
	// NB-04 on the non-contiguous shape: image 1 holds chunks 1 and 2 but
	// NOT chunk 0, so its parent stays PENDING (only its own whole file
	// counts there); image 2 delivers chunk 0, completes the parent, and
	// counts it exactly once.
	if res.Images[0].Image != 1 || res.Images[0].Out.Restored != 1 {
		t.Errorf("image 1 outcome = %+v, want its own whole file only (the parent must not complete without chunk 0)", res.Images[0].Out)
	}
	if res.Images[1].Image != 2 || res.Images[1].Out.Restored != 1 {
		t.Errorf("image 2 outcome = %+v, want the completed parent counted exactly once", res.Images[1].Out)
	}
	if restored != 2 || failed != 0 {
		t.Fatalf("totals = restored %d / failed %d, want 2 / 0", restored, failed)
	}

	want := append(append(append([]byte{}, c0...), c1...), c2...)
	if got := mustRead(t, filepath.Join(target, "big.bin")); string(got) != string(want) {
		t.Errorf("assembled content = %d bytes, want %d: chunk 0 supplied by IMAGE 2 must land under image 1's chunks at its recorded offset",
			len(got), len(want))
	}
	if got := mustRead(t, filepath.Join(target, "keep-1.txt")); string(got) != "image 1 also carries a whole file" {
		t.Errorf("keep-1.txt = %q", got)
	}
	assertNoPartials(t, target)
}

// TestRestoreChunkedNonContiguousSplitFailsEarlyOnCorruptChunk pins the
// EARLY-FAILURE half of the same layout: one corrupt chunk inside IMAGE 1's
// share (chunk 1 - a later index, media name 00002) must fail the parent at
// IMAGE 1, BEFORE image 2 ever contributes chunk 0, with exactly one finding
// recorded there (NB-04 count-once: image 2 stays silent for the already-
// failed parent), no final file, and - because image 1's contribution was
// this run's FIRST - the documented fresh branch deletes the .partial (it
// held only this run's bytes, so nothing verified was lost).
func TestRestoreChunkedNonContiguousSplitFailsEarlyOnCorruptChunk(t *testing.T) {
	f := newRestoreFixture(t, "S-RNC2")
	c0 := []byte(strings.Repeat("a", 400))
	c1 := []byte(strings.Repeat("b", 300))
	c2 := []byte(strings.Repeat("c", 200))
	f.addFile(1, "keep-1.txt", []byte("image 1 also carries a whole file"))
	f.addChunked("big.bin", []chunkPart{{image: 2, content: c0}, {image: 1, content: c1}, {image: 1, content: c2}})
	// Corrupt chunk 1 where it lives - IMAGE 1's non-contiguous share - so
	// the first verification rejects it during the FIRST image of the run.
	f.corruptTreeFile(1, "chunks/big.bin/00002")
	target := filepath.Join(t.TempDir(), "target")

	res, err := f.runInto(t, Selector{}, target)
	if err != nil {
		t.Fatalf("Run (per-file failures never abort the run): %v", err)
	}
	if len(res.Images) != 2 {
		t.Fatalf("images = %d, want 2 (the run continues over image 2)", len(res.Images))
	}

	// Image 1: its whole file restores AND the parent fails here, once,
	// naming the non-contiguous chunk it detected.
	out1 := res.Images[0].Out
	if out1.Restored != 1 || out1.Failed != 1 || len(out1.Files) != 1 {
		t.Fatalf("image 1 outcome = %+v, want its file restored and exactly the one incomplete parent", out1)
	}
	finding := out1.Files[0]
	if finding.RelPath != "big.bin" || finding.Status != statusIncomplete {
		t.Errorf("finding = %+v, want big.bin marked incomplete at IMAGE 1 (the early failure)", finding)
	}
	for _, want := range []string{"missing chunks []", "corrupt chunks [1]", "first verification", "no final file was written"} {
		if !strings.Contains(finding.Detail, want) {
			t.Errorf("detail %q must contain %q", finding.Detail, want)
		}
	}

	// Image 2 (the chunk-0 holder): the failed parent is silent - no
	// second finding, no count, no invented outcome.
	out2 := res.Images[1].Out
	if out2.Restored != 0 || out2.Failed != 0 || len(out2.Files) != 0 {
		t.Errorf("image 2 outcome = %+v, want silence for the parent that failed at image 1", out2)
	}

	var restored, failed int
	for _, img := range res.Images {
		restored += img.Out.Restored
		failed += img.Out.Failed
	}
	if restored != 1 || failed != 1 {
		t.Errorf("run totals = restored %d / failed %d, want 1 / 1", restored, failed)
	}

	// No final file for the incomplete member, the fresh-branch deletion
	// left no assembly behind, and image 1's healthy file is intact.
	assertAbsent(t, filepath.Join(target, "big.bin"), "the early-failed parent must have no final file")
	assertNoPartials(t, target)
	if got := mustRead(t, filepath.Join(target, "keep-1.txt")); string(got) != "image 1 also carries a whole file" {
		t.Errorf("keep-1.txt = %q", got)
	}
}

// --- hard-link families (8.2, D-04) -------------------------------------------

func TestRestoreHardLinkFamilyWholeSet(t *testing.T) {
	f := newRestoreFixture(t, "S-RFAM")
	content := []byte("one inode's worth of bytes")
	f.addFamily(1, "fam-a.txt", []string{"fam-b.txt"}, content)
	target := filepath.Join(t.TempDir(), "target")

	res, err := f.runInto(t, Selector{}, target)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out := res.Images[0].Out; out.Restored != 2 || out.Failed != 0 {
		t.Fatalf("outcome = %+v, want both names restored", out)
	}
	a := filepath.Join(target, "fam-a.txt")
	b := filepath.Join(target, "fam-b.txt")
	ai, err := os.Lstat(a)
	if err != nil {
		t.Fatalf("primary missing: %v", err)
	}
	bi, err := os.Lstat(b)
	if err != nil {
		t.Fatalf("secondary missing: %v", err)
	}
	if !os.SameFile(ai, bi) {
		t.Errorf("the restored names must share one inode (FR-039): %+v vs %+v", ai.Sys(), bi.Sys())
	}
	if got := statNlink(t, a); got != 2 {
		t.Errorf("nlink = %d, want 2", got)
	}
	if got := statNlink(t, b); got != 2 {
		t.Errorf("nlink = %d, want 2", got)
	}
	if string(mustRead(t, a)) != string(content) || string(mustRead(t, b)) != string(content) {
		t.Errorf("family content must round-trip once at both names")
	}
	assertNoPartials(t, target)
}

func TestRestorePartialFamilySelectedSecondariesOnly(t *testing.T) {
	f := newRestoreFixture(t, "S-RPFAM")
	content := []byte("family bytes land at the REQUESTED names only")
	f.addFamily(1, "fam-a.txt", []string{"fam-b.txt", "fam-c.txt"}, content)
	target := filepath.Join(t.TempDir(), "target")

	// Select ONLY two of the three names: T5.3's Work then carries the
	// Secondaries with PrimaryRelPath and NO Files row for the primary.
	sel := f.resolve(t, Selector{Paths: []string{"fam-b.txt", "fam-c.txt"}})
	work, ok := sel.WorkFor(1)
	if !ok {
		t.Fatal("image 1 must be required (the primary's image holds the family)")
	}
	if len(work.Inv.Files) != 0 || len(work.Inv.Secondaries) != 2 {
		t.Fatalf("work files = %v, secondaries = %v: want no primary and both requested names",
			rels(work.Inv.Files), rels2(work.Inv.Secondaries))
	}

	res, err := f.runInto(t, Selector{Paths: []string{"fam-b.txt", "fam-c.txt"}}, target)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out := res.Images[0].Out; out.Restored != 2 || out.Failed != 0 {
		t.Fatalf("outcome = %+v, want both requested names restored", out)
	}
	// The UNSELECTED primary must never appear under --target (8.2: links
	// for the requested names).
	assertAbsent(t, filepath.Join(target, "fam-a.txt"), "the unselected family primary must not be created")

	bi, err := os.Lstat(filepath.Join(target, "fam-b.txt"))
	if err != nil {
		t.Fatalf("fam-b.txt missing: %v", err)
	}
	ci, err := os.Lstat(filepath.Join(target, "fam-c.txt"))
	if err != nil {
		t.Fatalf("fam-c.txt missing: %v", err)
	}
	if !os.SameFile(bi, ci) {
		t.Error("both requested names must share ONE inode - content written once, then linked")
	}
	if got := string(mustRead(t, filepath.Join(target, "fam-b.txt"))); got != string(content) {
		t.Errorf("content = %q, want the family bytes", got)
	}
	assertNoPartials(t, target)
}

func TestRestoreSecondaryFailsWhenPrimaryFails(t *testing.T) {
	f := newRestoreFixture(t, "S-RFAM2")
	f.addFamily(1, "fam-a.txt", []string{"fam-b.txt"}, []byte("the real family bytes"))
	// The primary's anchor lies: its content restore fails, so there is
	// nothing to link the requested secondary to.
	f.patchHash("fam-a.txt", sha256Sum([]byte("something else entirely")))
	target := filepath.Join(t.TempDir(), "target")

	res, err := f.runInto(t, Selector{}, target)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := res.Images[0].Out
	if out.Failed != 2 || out.Restored != 0 || len(out.Files) != 2 {
		t.Fatalf("outcome = %+v, want both family names failed", out)
	}
	byName := map[string]FileOutcome{}
	for _, fo := range out.Files {
		byName[fo.RelPath] = fo
	}
	primary, ok := byName["fam-a.txt"]
	if !ok || primary.Status != statusFailed {
		t.Errorf("primary finding = %+v, want a failure", primary)
	}
	sec, ok := byName["fam-b.txt"]
	if !ok || sec.Status != statusFailed {
		t.Fatalf("secondary finding = %+v, want a failure", sec)
	}
	if !strings.Contains(sec.Detail, "cannot create the hard link") || !strings.Contains(sec.Detail, "fam-a.txt") {
		t.Errorf("secondary detail %q must explain that the primary's failure blocks the link", sec.Detail)
	}
	assertAbsent(t, filepath.Join(target, "fam-a.txt"), "a failed member has no final file")
	assertAbsent(t, filepath.Join(target, "fam-b.txt"), "the secondary must not exist without its primary")
	assertNoPartials(t, target)
}

// --- symlinks (D-03, the pinned osirrox contract) -----------------------------

func TestRestoreSymlinkVariants(t *testing.T) {
	f := newRestoreFixture(t, "S-RSL")
	f.addFile(1, "small.txt", []byte("link target content"))
	f.addFile(1, "sub/f.txt", []byte("deep"))
	f.addSymlink(1, "ok-link", "small.txt")    // valid
	f.addSymlink(1, "dangling", "nowhere.txt") // dangling: faithful
	f.addSymlink(1, "to-dir", "sub")           // to a directory
	target := filepath.Join(t.TempDir(), "target")

	res, err := f.runInto(t, Selector{}, target)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out := res.Images[0].Out; out.Restored != 5 || out.Failed != 0 {
		t.Fatalf("outcome = %+v, want all five members restored", out)
	}
	for rel, want := range map[string]string{
		"ok-link":  "small.txt",
		"dangling": "nowhere.txt",
		"to-dir":   "sub",
	} {
		path := filepath.Join(target, rel)
		fi, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s restored as %v, want a symlink (links are links - D-03)", rel, fi.Mode())
			continue
		}
		got, err := os.Readlink(path)
		if err != nil {
			t.Fatalf("readlink %s: %v", rel, err)
		}
		if got != want {
			t.Errorf("%s -> %q, want the recorded target %q", rel, got, want)
		}
	}
	// The dangling link is still a link (never "resolved" to a file), and
	// to-dir actually resolves to the directory the tree created.
	if fi, err := os.Stat(filepath.Join(target, "to-dir")); err != nil || !fi.IsDir() {
		t.Errorf("to-dir must resolve to a directory: %v", err)
	}
	if fi, err := os.Lstat(filepath.Join(target, "dangling")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("dangling must stay a symlink: %v", err)
	}
	assertNoPartials(t, target)
}

func TestRestoreSymlinkCollisionNeverClobbers(t *testing.T) {
	f := newRestoreFixture(t, "S-RSL2")
	f.addFile(1, "small.txt", []byte("link target content"))
	f.addSymlink(1, "taken", "small.txt") // a regular file will sit here
	f.addSymlink(1, "same", "small.txt")  // the exact link already sits here
	target := filepath.Join(t.TempDir(), "target")
	mkdir(t, target)
	// Pre-populated target: one foreign file at the member's path, one
	// already-correct symlink (the idempotent re-run shape).
	if err := os.WriteFile(filepath.Join(target, "taken"), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("small.txt", filepath.Join(target, "same")); err != nil {
		t.Fatal(err)
	}

	res, err := f.runInto(t, Selector{}, target)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := res.Images[0].Out
	if out.Failed != 1 || out.Restored != 2 { // same + small.txt restore; taken fails
		t.Fatalf("outcome = %+v, want exactly the collision failed", out)
	}
	var finding FileOutcome
	for _, fo := range out.Files {
		if fo.RelPath == "taken" {
			finding = fo
		}
	}
	if finding.Status != statusFailed || !strings.Contains(finding.Detail, "refusing to clobber") {
		t.Errorf("collision finding = %+v, want a no-clobber failure", finding)
	}
	// The foreign file is untouched - byte for byte.
	if got := mustRead(t, filepath.Join(target, "taken")); string(got) != "keep me" {
		t.Errorf("the collision clobbered the existing file: %q", got)
	}
	// The already-correct symlink is tolerated (idempotent re-run).
	if got, err := os.Readlink(filepath.Join(target, "same")); err != nil || got != "small.txt" {
		t.Errorf("same -> %q (%v), want the recorded target", got, err)
	}
}

// --- empty directories (FR-037, 5.4) ------------------------------------------

func TestRestoreEmptyDirsIdempotent(t *testing.T) {
	f := newRestoreFixture(t, "S-RDIR")
	const dirMode uint32 = 0o750
	f.addFile(1, "payload.txt", []byte("x"))
	f.addDir(1, "empty-dir", dirMode)
	f.addDir(1, "taken", 0o700) // a file will occupy this path
	target := filepath.Join(t.TempDir(), "target")
	mkdir(t, target)
	if err := os.WriteFile(filepath.Join(target, "taken"), []byte("a file, not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := f.runInto(t, Selector{}, target)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := res.Images[0].Out
	if out.Restored != 2 || out.Failed != 1 {
		t.Fatalf("outcome = %+v, want payload + empty-dir restored, taken failed", out)
	}
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	assertMeta(t, filepath.Join(target, "empty-dir"), dirMode, uid, gid, fixtureMtimeNS)

	// Second run over the same target: EEXIST on the recorded dir is
	// tolerated (idempotent - another image or a prior run may have made
	// it), the foreign file still refuses honestly.
	res2, err := f.runInto(t, Selector{}, target)
	if err != nil {
		t.Fatalf("second Run: %v", err)
	}
	out2 := res2.Images[0].Out
	if out2.Restored != 2 || out2.Failed != 1 {
		t.Errorf("second run outcome = %+v, want the same shape (EEXIST tolerated)", out2)
	}
	if got := mustRead(t, filepath.Join(target, "taken")); string(got) != "a file, not a dir" {
		t.Errorf("the dir attempt damaged the occupying file: %q", got)
	}
}

// --- metadata: the POSIX -> Go conversion chain, EPERM warning (5.4) ----------

func TestRestoreMetadataFidelityIncludesSetuid(t *testing.T) {
	f := newRestoreFixture(t, "S-RMETA")
	uid, gid := uint32(os.Getuid()), uint32(os.Getgid())
	otherMtime := fixtureMtimeNS + 4_000
	f.addFileMeta(1, "suid.sh", []byte("#!/bin/sh\necho hi\n"), 0o4755, uid, gid, fixtureMtimeNS)
	f.addFileMeta(1, "private.txt", []byte("0640 bytes"), 0o640, uid, gid, otherMtime)
	f.addFile(1, "plain.txt", []byte("default 0644"))
	target := filepath.Join(t.TempDir(), "target")

	res, err := f.runInto(t, Selector{}, target)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out := res.Images[0].Out; out.Restored != 3 || out.Failed != 0 || len(out.Files) != 0 {
		t.Fatalf("outcome = %+v, want a clean restore with no warnings (chown to self succeeds)", out)
	}
	// THE conversion chain end to end: the catalog's RAW POSIX 04755 must
	// land as setuid on disk (a raw 04755 handed to os.Chmod silently
	// becomes 0755 - fsmeta.GoMode at the apply site is what prevents it).
	assertMeta(t, filepath.Join(target, "suid.sh"), 0o4755, uid, gid, fixtureMtimeNS)
	assertMeta(t, filepath.Join(target, "private.txt"), 0o640, uid, gid, otherMtime)
	assertMeta(t, filepath.Join(target, "plain.txt"), 0o644, uid, gid, fixtureMtimeNS)
}

func TestRestoreNonRootChownEPERMIsWarningNotFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: EPERM cannot be provoked (fsmeta precedent)")
	}
	f := newRestoreFixture(t, "S-RPERM")
	// Recorded ownership we may not grant ourselves: the unconditional
	// chown fires EPERM, which 5.4 downgrades to a WARNING.
	f.addFileMeta(1, "root-owned.txt", []byte("content is verified either way"), 0o600, 0, 0, fixtureMtimeNS)
	target := filepath.Join(t.TempDir(), "target")

	bus, finish := wireBus()
	t.Cleanup(func() { finish("cleanup") })
	f.bus = bus
	res, err := f.runInto(t, Selector{}, target)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	stderr := finish("done")

	out := res.Images[0].Out
	if out.Restored != 1 || out.Failed != 0 {
		t.Fatalf("outcome = %+v, want the member RESTORED with a warning, never failed", out)
	}
	if len(out.Files) != 1 || out.Files[0].Status != "restored" {
		t.Fatalf("files = %+v, want the restored member carrying its warning Detail", out.Files)
	}
	detail := out.Files[0].Detail
	if !strings.Contains(detail, "chown") || !strings.Contains(detail, "operation not permitted") {
		t.Errorf("warning detail %q must name the EPERM chown downgrade", detail)
	}
	// The warning is also a progress event (5.4's "warning event").
	if !strings.Contains(stderr, "warning: ") || !strings.Contains(stderr, "operation not permitted") {
		t.Errorf("the EPERM downgrade must render as a warning line:\n%s", stderr)
	}
	// The rest of the metadata still landed - ownership stays OURS (the
	// chown was denied), mode and nanosecond mtime are the record's.
	assertMeta(t, filepath.Join(target, "root-owned.txt"), 0o600,
		uint32(os.Getuid()), uint32(os.Getgid()), fixtureMtimeNS)
}

// --- cancellation mid-assembly (NB-03's trust model) --------------------------

func TestRestoreCancellationMidAssemblyLeavesPartialNoFinal(t *testing.T) {
	f := newRestoreFixture(t, "S-RCXL")
	p0 := []byte(strings.Repeat("m", 512))
	p1 := []byte(strings.Repeat("n", 256))
	f.addChunked("big.bin", []chunkPart{{1, p0}, {2, p1}})
	target := filepath.Join(t.TempDir(), "target")

	sel := f.resolve(t, Selector{})
	sel.Target = target
	restorer := NewImageRestorer(RestorerDeps{})

	// Image 1 completes its share: the assembly is pending in the target.
	out1, err := restorer.RestoreImage(context.Background(),
		ImageSupply{SetID: f.setID, Image: 1, Kind: SourceCopyTree, Path: f.tree(1)},
		sel, mkWorkspace(t))
	if err != nil {
		t.Fatalf("image 1: %v", err)
	}
	if out1.Restored != 0 {
		t.Errorf("image 1 Restored = %d, want 0 (pending assembly counts at completion)", out1.Restored)
	}
	partial := filepath.Join(target, "big.bin.partial")
	if fi, err := os.Stat(partial); err != nil || fi.Size() != int64(len(p0)) {
		t.Fatalf("assembly after image 1 = (%v, %d), want the file at %d bytes", err, fi.Size(), len(p0))
	}

	// Cancelled before image 2 runs its chunks: the run stops without
	// another byte, without a rename, and without a second count.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out2, err := restorer.RestoreImage(ctx,
		ImageSupply{SetID: f.setID, Image: 2, Kind: SourceCopyTree, Path: f.tree(2)},
		sel, mkWorkspace(t))
	if err != context.Canceled {
		t.Fatalf("cancelled call err = %v, want context.Canceled", err)
	}
	if out2.Restored != 0 || out2.Failed != 0 || len(out2.Files) != 0 {
		t.Errorf("cancelled outcome = %+v, want no invented counts or findings", out2)
	}
	assertAbsent(t, filepath.Join(target, "big.bin"), "a cancelled run never renames an assembly into place")
	if fi, err := os.Stat(partial); err != nil || fi.Size() != int64(len(p0)) {
		t.Errorf("assembly after cancellation = (%v, %d), want it abandoned unchanged at %d bytes", err, fi.Size(), len(p0))
	}
	// The catalog is structurally untouched: this package has no write
	// surface at all (the Store seam is read-only - doc.go), and the fake
	// store above exposes no mutating method to call.
}

// --- the B1-mirror partial-collision gate -------------------------------------

func TestRestorePartialCollisionGateRefusesBeforeWrite(t *testing.T) {
	f := newRestoreFixture(t, "S-RB1")
	// The DESTRUCTIVE order: the sibling's completed final lands at image 1
	// (target/a.partial), the colliding item arrives from image 2 - exactly
	// the pair the copy pipeline's PER-IMAGE gates cannot see. Without the
	// selection-wide gate, step-2a's stale-partial removal for `a` would
	// silently delete the sibling's verified bytes.
	f.addFile(1, "a.partial", []byte("the sibling's verified bytes"))
	f.addFile(2, "a", []byte("the colliding member"))
	target := filepath.Join(t.TempDir(), "target")

	res, err := f.runInto(t, Selector{}, target)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var restored, failed int
	var finding FileOutcome
	for _, img := range res.Images {
		restored += img.Out.Restored
		failed += img.Out.Failed
		for _, fo := range img.Out.Files {
			if fo.RelPath == "a" {
				finding = fo
			}
		}
	}
	if restored != 1 || failed != 1 {
		t.Fatalf("totals = restored %d / failed %d, want 1 / 1", restored, failed)
	}
	if finding.Status != statusFailed || !strings.Contains(finding.Detail, "partial-path collision") {
		t.Errorf("finding = %+v, want the refusal before any write", finding)
	}
	assertAbsent(t, filepath.Join(target, "a"), "the colliding member is refused, never half-written")
	if got := mustRead(t, filepath.Join(target, "a.partial")); string(got) != "the sibling's verified bytes" {
		t.Errorf("the sibling's final was damaged: %q", got)
	}
}

// --- the B1 gate's exact-suffix precision (T5.4 reviewer follow-up) ----------

// TestPartialCollisionGateExactSuffixPrecision pins BOTH directions of the
// gate's name arithmetic at the unit level (the T5.4 reviewer verified them
// by inspection; this makes them permanent):
//
//   - NEGATIVE: a member merely NAMED "...partial..." - `a/partial.txt` -
//     is NEVER refused while its derived `a/partial.txt.partial` is not a
//     selected final. A gate built on substring/suffix CONTAINMENT ("does
//     this name contain 'partial'?") would falsely refuse it and strand a
//     perfectly restorable member;
//   - POSITIVE: the real pair is caught - `a/x`, whose derived
//     `a/x.partial` IS a selected final, is the refused item (the
//     documented direction: the item whose partial lands on its sibling's
//     final is gated), while the sibling `a/x.partial` itself PROCEEDS -
//     its own derived `a/x.partial.partial` collides with nothing.
//
// The run-level halves: TestRestorePartialCollisionGateRefusesBeforeWrite
// (the pair end to end) and TestRestoreNameContainingPartialIsNotRefused
// (the negative end to end).
func TestPartialCollisionGateExactSuffixPrecision(t *testing.T) {
	cases := []struct {
		name     string
		selected []string
		want     map[string]string
	}{
		{"a plain name is never gated", []string{"a/x"}, nil},
		{"the word 'partial' inside a NAME is not a collision", []string{"a/partial.txt", "keep.txt"}, nil},
		{"a dot-less 'partial' suffix is not a collision", []string{"a/x", "a/xpartial"}, nil},
		{"the exact-suffix pair gates the item whose partial lands on the sibling's final",
			[]string{"a/x", "a/x.partial"},
			map[string]string{"a/x": "a/x.partial"}},
		{"an exact suffix inside a name that already contains 'partial' still gates",
			[]string{"a/partial.txt", "a/partial.txt.partial"},
			map[string]string{"a/partial.txt": "a/partial.txt.partial"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := partialCollisionGate(tc.selected)
			if len(got) != len(tc.want) {
				t.Fatalf("partialCollisionGate(%v) = %v, want %v", tc.selected, got, tc.want)
			}
			for rel, sibling := range tc.want {
				if got[rel] != sibling {
					t.Errorf("partialCollisionGate(%v)[%q] = %q, want %q (full map %v)",
						tc.selected, rel, got[rel], sibling, got)
				}
			}
		})
	}

	// THE SIBLING DIRECTION, stated explicitly: in the gated pair the
	// *.partial-named member is the one that PROCEEDS (its derived
	// "<name>.partial.partial" is nobody's final), so the gate must never
	// key it - refusing it would banish a member for a path nobody wrote.
	got := partialCollisionGate([]string{"a/x", "a/x.partial"})
	if sibling, refused := got["a/x.partial"]; refused {
		t.Errorf("a/x.partial must PROCEED (its sibling a/x is the gated item), but the gate refused it for %q", sibling)
	}
}

// TestRestoreNameContainingPartialIsNotRefused pins the gate's NEGATIVE
// direction THROUGH THE REAL RUN: members whose names merely contain (or even
// end with the letters of) "partial" - `a/partial.txt` and `a/xpartial` -
// restore normally because their DERIVED partials (`a/partial.txt.partial`,
// `a/xpartial.partial`) are not selected. Zero gate findings, both files
// intact, no `.partial` debris: a containment-based gate would fail exactly
// here, before any write.
func TestRestoreNameContainingPartialIsNotRefused(t *testing.T) {
	f := newRestoreFixture(t, "S-RNCP")
	f.addFile(1, "a/partial.txt", []byte("a name that merely contains 'partial'"))
	f.addFile(2, "a/xpartial", []byte("dot-less 'partial' suffix, still not a collision"))
	target := filepath.Join(t.TempDir(), "target")

	res, err := f.runInto(t, Selector{}, target)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var restored, failed int
	for _, img := range res.Images {
		if img.Err != nil {
			t.Fatalf("image %d error: %v", img.Image, img.Err)
		}
		restored += img.Out.Restored
		failed += img.Out.Failed
		for _, fo := range img.Out.Files {
			t.Errorf("the gate must not touch these members, but image %d recorded %+v", img.Image, fo)
		}
	}
	if restored != 2 || failed != 0 {
		t.Fatalf("totals = restored %d / failed %d, want 2 / 0", restored, failed)
	}
	// Both members landed under their recorded paths and no derived
	// .partial debris exists anywhere in the target.
	assertNoPartials(t, target)
	if got := mustRead(t, filepath.Join(target, "a", "partial.txt")); string(got) != "a name that merely contains 'partial'" {
		t.Errorf("a/partial.txt = %q", got)
	}
	if got := mustRead(t, filepath.Join(target, "a", "xpartial")); string(got) != "dot-less 'partial' suffix, still not a collision" {
		t.Errorf("a/xpartial = %q", got)
	}
}

// --- the ISO source path (the T4.1 Tool seam) ---------------------------------

// fakeXorriso writes a POSIX-sh stand-in for xorriso honoring the ONE argv
// shape restore uses (-indev <iso> -osirrox on -extract / <dir>): it copies
// the prepared tree into the destination, exactly as osirrox would.
func fakeXorriso(t *testing.T, preparedTree string) *xorriso.Tool {
	t.Helper()
	script := filepath.Join(t.TempDir(), "fake-xorriso")
	body := "#!/bin/sh\n" +
		"[ \"$1\" = \"-indev\" ] || { echo 'fake xorriso: expected -indev' >&2; exit 9; }\n" +
		"[ \"$3\" = \"-osirrox\" ] && [ \"$4\" = \"on\" ] || { echo 'fake xorriso: expected -osirrox on' >&2; exit 9; }\n" +
		"[ \"$5\" = \"-extract\" ] && [ \"$6\" = \"/\" ] || { echo 'fake xorriso: expected -extract /' >&2; exit 9; }\n" +
		"dest=\"$7\"\n" +
		"mkdir -p \"$dest\" || exit 8\n" +
		"cp -R " + shellQuote(preparedTree) + "/. \"$dest\"/ || exit 9\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("writing the fake xorriso: %v", err)
	}
	return &xorriso.Tool{Path: script}
}

// shellQuote wraps a path in POSIX single quotes (fixture paths carry no
// quotes, but the escaping keeps the helper honest).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func TestRestoreISOSourceThroughXorrisoTool(t *testing.T) {
	f := newRestoreFixture(t, "S-RISO")
	f.addFile(1, "hello.txt", []byte("via extraction"))
	f.addFile(1, "sub/deep.txt", []byte("nested via extraction"))
	f.addSymlink(1, "link", "hello.txt")
	f.addChunked("big.bin", []chunkPart{{1, []byte("assembled from an ISO's chunks A")}, {1, []byte("+B")}})

	sel := f.resolve(t, Selector{})
	target := filepath.Join(t.TempDir(), "target")
	sel.Target = target
	workspace := mkWorkspace(t)
	// The free-space pre-check stats the image file, so a stand-in .iso
	// exists; its CONTENT never matters to the fake.
	iso := filepath.Join(t.TempDir(), "S-RISO-001.iso")
	if err := os.WriteFile(iso, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}

	restorer := NewImageRestorer(RestorerDeps{Tool: fakeXorriso(t, f.tree(1))})
	out, err := restorer.RestoreImage(context.Background(),
		ImageSupply{SetID: f.setID, Image: 1, Kind: SourceISO, Path: iso},
		sel, workspace)
	if err != nil {
		t.Fatalf("RestoreImage over an ISO: %v", err)
	}
	if out.Restored != 4 || out.Failed != 0 || len(out.Files) != 0 {
		t.Fatalf("outcome = %+v, want all four members restored from the extraction", out)
	}
	if got := mustRead(t, filepath.Join(target, "hello.txt")); string(got) != "via extraction" {
		t.Errorf("hello.txt = %q", got)
	}
	if got := mustRead(t, filepath.Join(target, "sub", "deep.txt")); string(got) != "nested via extraction" {
		t.Errorf("sub/deep.txt = %q", got)
	}
	want := "assembled from an ISO's chunks A+B"
	if got := string(mustRead(t, filepath.Join(target, "big.bin"))); got != want {
		t.Errorf("big.bin = %q, want %q", got, want)
	}
	if got, err := os.Readlink(filepath.Join(target, "link")); err != nil || got != "hello.txt" {
		t.Errorf("link -> %q (%v)", got, err)
	}
	assertNoPartials(t, target)

	// The extraction scratch lived INSIDE the workspace (the seam's rule) -
	// it is left there for the orchestration's per-image clean to remove.
	entries := listDirNames(t, workspace)
	if len(entries) != 1 || !strings.HasPrefix(entries[0], "extract-1-") {
		t.Errorf("workspace entries = %v, want exactly one extract-1-* directory", entries)
	}
	if _, err := os.Stat(filepath.Join(workspace, entries[0], "files", "hello.txt")); err != nil {
		t.Errorf("the reader must consume the extraction inside the workspace: %v", err)
	}
}

func TestRestoreISOFreeSpacePreCheckRefusesBeforeExtraction(t *testing.T) {
	f := newRestoreFixture(t, "S-RFS")
	f.addFile(1, "hello.txt", []byte("irrelevant"))
	sel := f.resolve(t, Selector{})
	target := filepath.Join(t.TempDir(), "target")
	sel.Target = target
	workspace := mkWorkspace(t)
	iso := filepath.Join(t.TempDir(), "S-RFS-001.iso")
	if err := os.WriteFile(iso, make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}

	restorer := NewImageRestorer(RestorerDeps{
		Tool: fakeXorriso(t, f.tree(1)),
		// The 7.2 pre-check seam: nowhere near enough room.
		FreeSpace: func(string) (uint64, error) { return 1024, nil },
	})
	_, err := restorer.RestoreImage(context.Background(),
		ImageSupply{SetID: f.setID, Image: 1, Kind: SourceISO, Path: iso},
		sel, workspace)
	if err == nil || !strings.Contains(err.Error(), "free-space pre-check") {
		t.Fatalf("err = %v, want the DESIGN_SPEC 7.2 free-space refusal", err)
	}
	// The refusal fires BEFORE any extraction write: no scratch, no target.
	if entries := listDirNames(t, workspace); len(entries) != 0 {
		t.Errorf("workspace = %v, want no extraction after a refused pre-check", entries)
	}
	assertAbsent(t, filepath.Join(target, "hello.txt"), "no target write may precede the pre-check")
}

func TestRestoreImageLevelSourceFaultsAreReportedNotRecorded(t *testing.T) {
	f := newRestoreFixture(t, "S-RSRC")
	f.addFile(1, "hello.txt", []byte("x"))
	sel := f.resolve(t, Selector{})
	target := filepath.Join(t.TempDir(), "target")
	sel.Target = target

	restorer := NewImageRestorer(RestorerDeps{}) // no xorriso tool at all
	if _, err := restorer.RestoreImage(context.Background(),
		ImageSupply{SetID: f.setID, Image: 1, Kind: SourceISO, Path: filepath.Join(t.TempDir(), "x.iso")},
		sel, mkWorkspace(t)); err == nil || !strings.Contains(err.Error(), "no xorriso tool") {
		t.Errorf("ISO without a tool: err = %v, want the honest wiring failure", err)
	}
	if _, err := restorer.RestoreImage(context.Background(),
		ImageSupply{SetID: f.setID, Image: 1, Kind: SourceCopyTree, Path: filepath.Join(t.TempDir(), "gone")},
		sel, mkWorkspace(t)); err == nil || !strings.Contains(err.Error(), "copy tree") {
		t.Errorf("vanishing copy tree: err = %v, want an image-level failure", err)
	}
	if _, err := restorer.RestoreImage(context.Background(),
		ImageSupply{SetID: f.setID, Image: 1, Kind: SourceKind("tape"), Path: "/x"},
		sel, mkWorkspace(t)); err == nil || !strings.Contains(err.Error(), "unknown source kind") {
		t.Errorf("unknown kind: err = %v, want the dispatch refusal", err)
	}
	// Image-level faults return an error and record NOTHING (the loop
	// reports them per image; the cli turns that into exit 10).
	assertAbsent(t, filepath.Join(target, "hello.txt"), "a source fault must not half-restore")
}
