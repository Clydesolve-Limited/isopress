package restore

// T5.4 adversarial acceptance for the REAL ImageRestorer: the failure shapes
// that are invisible in a single healthy call and the contracts the package
// doc PINNS (doc.go / restorer.go):
//
//   - CRASH RECOVERY ACROSS RUNS (the O_TRUNC truncation contract): a run
//     killed between images leaves the span's verified chunks in the target's
//     <rel>.partial; the NEXT run of the set must discard that stale assembly
//     on its FIRST chunk write and rebuild the whole file from scratch across
//     its own images - no stale tail, no stale bytes, exactly the recorded
//     content;
//   - COUNT-ONCE on a HEALTHY span whose every image carries whole files as
//     well as the spanning parent (the mixed selection: per-image counts sum
//     honestly, the parent lands ONCE at its completion image);
//   - the LEAVE-VS-DELETE decision on BOTH gates and BOTH branches: a FRESH
//     single-image corruption DELETES (detail says so), a CROSS-IMAGE second
//     verification failure LEAVES the assembly holding earlier verified
//     chunks (detail says so);
//   - the B1-MIRROR selection-wide gate in BOTH orders, for a whole file AND
//     for a chunked parent, with unrelated members still restoring and the
//     refusal naming both colliding paths;
//   - the extraction contract: ONE xorriso invocation per ISO image (into a
//     UNIQUE workspace subdirectory - the subdir shape itself is pinned by
//     TestRestoreISOSourceThroughXorrisoTool, the free-space pre-check order
//     by TestRestoreISOFreeSpacePreCheckRefusesBeforeExtraction);

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"isopress/internal/xorriso"
)

// --- crash between images, then a second run rebuilds the assembly -----------

// cancelAfter wraps the REAL restorer and cancels the run context once the
// Nth seam call returns: the deterministic "crash between images" of the
// crash-recovery contract, with no sleeps (Run honours ctx at the next image
// boundary, exactly as a SIGKILLed process would simply stop calling).
type cancelAfter struct {
	inner  ImageRestorer
	after  int
	cancel context.CancelFunc
	calls  int
}

func (c *cancelAfter) RestoreImage(ctx context.Context, image ImageSupply, selection ResolvedSelection, workspace string) (ImageRestoreResult, error) {
	out, err := c.inner.RestoreImage(ctx, image, selection, workspace)
	c.calls++
	if c.calls >= c.after {
		c.cancel()
	}
	return out, err
}

// TestRestoreCrashBetweenImagesThenRerunRebuildsAssembly is THE crash-recovery
// contract of the cross-image assembly: the growing file lives in the TARGET
// (it survives the workspace wipe and the death of the run), and the doc's
// "the first chunk of the NEXT RUN opens O_TRUNC" is what makes a fresh run
// honest again. The stale assembly is deliberately POISONED with a longer
// garbage tail before the second run: only a real truncation can make the
// second run's file come out exactly ParentSize bytes with the recorded hash -
// without it the second verification would fail on the stale tail (or worse,
// publish it).
func TestRestoreCrashBetweenImagesThenRerunRebuildsAssembly(t *testing.T) {
	for _, crashAfter := range []int{1, 2} {
		t.Run(fmt.Sprintf("crash after image %d", crashAfter), func(t *testing.T) {
			setID := fmt.Sprintf("S-RCR%d", crashAfter)
			f := newRestoreFixture(t, setID)
			p0 := []byte(strings.Repeat("1", 400))
			p1 := []byte(strings.Repeat("2", 300))
			p2 := []byte(strings.Repeat("3", 200))
			f.addFile(1, "keep-1.txt", []byte("first image keeps this"))
			f.addFile(2, "keep-2.txt", []byte("second image keeps this"))
			f.addFile(3, "keep-3.txt", []byte("third image keeps this"))
			f.addChunked("big.bin", []chunkPart{{1, p0}, {2, p1}, {3, p2}})
			want := append(append(append([]byte{}, p0...), p1...), p2...)
			target := filepath.Join(t.TempDir(), "target")
			f.finish() // the placement summary the selection reads

			// RUN 1: dies after the Nth image, mid-span.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			crasher := &cancelAfter{
				inner:  NewImageRestorer(RestorerDeps{}),
				after:  crashAfter,
				cancel: cancel,
			}
			res1, err := Run(ctx, Input{
				Store:    f.store,
				Restorer: crasher,
				Selector: Selector{SetID: setID},
				Target:   target,
				Supply:   SupplySpec{CopyTreeRoot: f.treeRoot},
				WorkDir:  tempWorkspaceDir(t),
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("run 1 err = %v, want context.Canceled (the crash)", err)
			}
			if len(res1.Images) != crashAfter {
				t.Fatalf("run 1 processed %d images, want %d (the crash must land between images)", len(res1.Images), crashAfter)
			}
			// The partial holds exactly the images that DID run - and is
			// durable across the crashed run (the trust model: an unmarked
			// .partial, never a final file).
			partial := filepath.Join(target, "big.bin.partial")
			wantPartial := []int64{int64(len(p0)), int64(len(p0) + len(p1))}[crashAfter-1]
			fi, err := os.Stat(partial)
			if err != nil {
				t.Fatalf("the crashed run must leave the assembly for the next run: %v", err)
			}
			if fi.Size() != wantPartial {
				t.Fatalf("assembly after the crash = %d bytes, want %d (image(s) 1..%d verified)", fi.Size(), wantPartial, crashAfter)
			}
			assertAbsent(t, filepath.Join(target, "big.bin"), "a crashed run never publishes an assembly")

			// POISON the crashed assembly with a stale tail: a process that
			// died mid-write can leave MORE bytes than the record says, and
			// the next run's truncation is the only thing that discards them.
			out, err := os.OpenFile(partial, os.O_WRONLY|os.O_APPEND, 0o644)
			if err != nil {
				t.Fatalf("opening the assembly to poison it: %v", err)
			}
			if _, err := out.Write([]byte(strings.Repeat("\x00", 511))); err != nil {
				t.Fatalf("poisoning the assembly: %v", err)
			}
			if err := out.Close(); err != nil {
				t.Fatalf("closing the poisoned assembly: %v", err)
			}
			if fi, err := os.Stat(partial); err != nil || fi.Size() != wantPartial+511 {
				t.Fatalf("poisoned assembly = (%v, %d), want %d bytes", err, fi.Size(), wantPartial+511)
			}

			// RUN 2: a FRESH run of the SAME set (a new restorer instance -
			// the one-instance-per-run factory contract) over the same target.
			// The selection supplies every image holding a chunk of the
			// parent, so the first chunk of image 1 truncates the stale
			// assembly and the whole file is rebuilt across the run's images.
			res2, err := f.runInto(t, Selector{}, target)
			if err != nil {
				t.Fatalf("run 2: %v", err)
			}
			if len(res2.Images) != 3 {
				t.Fatalf("run 2 processed %d images, want 3", len(res2.Images))
			}
			// The rebuild is EXACT: recorded size and recorded bytes, no
			// stale tail, no hole - the truncation contract end to end.
			final := filepath.Join(target, "big.bin")
			got := mustRead(t, final)
			if int64(len(got)) != int64(len(want)) {
				t.Errorf("rebuilt big.bin = %d bytes, want %d (a stale tail would survive without the first-chunk O_TRUNC)", len(got), len(want))
			}
			if string(got) != string(want) {
				t.Errorf("rebuilt big.bin differs from the recorded content (%d vs %d bytes)", len(got), len(want))
			}
			assertNoPartials(t, target)
			// Count-once holds on the rebuild: each image counts its own
			// whole file, the parent lands ONCE at image 3 (its completion),
			// and nothing is listed anywhere.
			wantRestored := []int{1, 1, 2}
			var total int
			for i, img := range res2.Images {
				if img.Err != nil || img.Out.Failed != 0 || len(img.Out.Files) != 0 {
					t.Errorf("run 2 image %d = %+v (err %v), want a clean outcome with no findings", img.Image, img.Out, img.Err)
				}
				if img.Out.Restored != wantRestored[i] {
					t.Errorf("run 2 image %d Restored = %d, want %d", img.Image, img.Out.Restored, wantRestored[i])
				}
				total += img.Out.Restored
			}
			if total != 4 {
				t.Errorf("run 2 total Restored = %d, want 4 (three whole files + the parent counted ONCE)", total)
			}
			// Exactly the recorded set under the target, nothing else.
			assertTargetNames(t, target, "big.bin", "keep-1.txt", "keep-2.txt", "keep-3.txt")
		})
	}
}

// assertTargetNames requires the target to hold EXACTLY these relative names
// (the "no stray output" half of the round-trip contract at restore scale).
func assertTargetNames(t *testing.T, root string, want ...string) {
	t.Helper()
	var got []string
	err := filepath.Walk(root, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		got = append(got, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatalf("walking the target: %v", err)
	}
	wantSet := map[string]bool{}
	for _, w := range want {
		wantSet[w] = true
	}
	gotSet := map[string]bool{}
	for _, g := range got {
		gotSet[g] = true
		if !wantSet[g] {
			t.Errorf("target holds the stray entry %q (want exactly %v)", g, want)
		}
	}
	for w := range wantSet {
		if !gotSet[w] {
			t.Errorf("target is missing %q (want exactly %v)", w, want)
		}
	}
}

// TestRestoreMissingImageLeavesPreviousRunAssemblyUntouched: after a crash the
// target holds an abandoned assembly; a subsequent run that CANNOT obtain the
// media must abort before the loop (8.3/RCR-3, exit 3 at the cli) and leave
// those bytes byte-identical - an aborted supply attempt never writes, never
// truncates, never renames.
func TestRestoreMissingImageLeavesPreviousRunAssemblyUntouched(t *testing.T) {
	f := newRestoreFixture(t, "S-RMISSIMG")
	p0 := []byte(strings.Repeat("z", 256))
	p1 := []byte(strings.Repeat("y", 128))
	f.addChunked("big.bin", []chunkPart{{1, p0}, {2, p1}})
	target := filepath.Join(t.TempDir(), "target")

	// A first run's image 1 leaves the span's verified chunk in the target.
	restorer := NewImageRestorer(RestorerDeps{})
	if _, err := restorer.RestoreImage(context.Background(),
		ImageSupply{SetID: f.setID, Image: 1, Kind: SourceCopyTree, Path: f.tree(1)},
		mustResolved(t, f, target), mkWorkspace(t)); err != nil {
		t.Fatalf("image 1: %v", err)
	}
	partial := filepath.Join(target, "big.bin.partial")
	before, err := os.ReadFile(partial)
	if err != nil {
		t.Fatalf("reading the abandoned assembly: %v", err)
	}
	if int64(len(before)) != int64(len(p0)) {
		t.Fatalf("assembly = %d bytes, want %d", len(before), len(p0))
	}

	// The second run cannot obtain ANY image: the non-TTY supply aborts
	// before the loop, so nothing on the target may move.
	_, err = Run(context.Background(), Input{
		Store:    f.store,
		Restorer: NewImageRestorer(RestorerDeps{}),
		Selector: Selector{SetID: f.setID},
		Target:   target,
		Supply:   SupplySpec{CopyTreeRoot: filepath.Join(t.TempDir(), "no-media-here")},
		WorkDir:  tempWorkspaceDir(t),
	})
	var missing *MissingImagesError
	if !errors.As(err, &missing) {
		t.Fatalf("err = %v, want the non-TTY MissingImagesError abort", err)
	}
	after, err := os.ReadFile(partial)
	if err != nil {
		t.Fatalf("the assembly must survive the aborted run: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("the aborted supply run modified the assembly (%d vs %d bytes)", len(after), len(before))
	}
	assertAbsent(t, filepath.Join(target, "big.bin"), "an aborted supply run publishes nothing")
}

// mustResolved resolves the fixture's selection with an explicit target.
func mustResolved(t *testing.T, f *restoreFixture, target string) ResolvedSelection {
	t.Helper()
	sel := f.resolve(t, Selector{})
	sel.Target = target
	return sel
}

// --- count-once on a healthy MIXED selection ---------------------------------

// TestRestoreMixedSelectionCountsSpanOnceAcrossAllImages pins the top-level
// count of a 3-image span whose EVERY image also carries whole files: the
// pending parent contributes nothing at images 1 and 2 (no row, no count),
// then lands exactly ONCE at image 3, and the summed report adds up honestly.
func TestRestoreMixedSelectionCountsSpanOnceAcrossAllImages(t *testing.T) {
	f := newRestoreFixture(t, "S-RMIX")
	p0 := []byte(strings.Repeat("a", 512))
	p1 := []byte(strings.Repeat("b", 256))
	p2 := []byte(strings.Repeat("c", 128))
	f.addFile(1, "keep-1.txt", []byte("one"))
	f.addFile(2, "keep-2.txt", []byte("two"))
	f.addFile(3, "keep-3.txt", []byte("three"))
	f.addChunked("big.bin", []chunkPart{{1, p0}, {2, p1}, {3, p2}})
	target := filepath.Join(t.TempDir(), "target")

	res, err := f.runInto(t, Selector{}, target)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Images) != 3 {
		t.Fatalf("images = %d, want 3", len(res.Images))
	}
	// Per-image: image 1 and image 2 count their own file ONLY (the pending
	// parent is neither restored nor failed there), image 3 counts its file
	// plus the completed parent.
	want := []int{1, 1, 2}
	var total int
	for i, img := range res.Images {
		if img.Err != nil {
			t.Fatalf("image %d error: %v", img.Image, img.Err)
		}
		if img.Out.Restored != want[i] || img.Out.Failed != 0 {
			t.Errorf("image %d outcome = %+v, want Restored %d / Failed 0", img.Image, img.Out, want[i])
		}
		if len(img.Out.Files) != 0 {
			t.Errorf("image %d listed members on a healthy span: %+v", img.Image, img.Out.Files)
		}
		total += img.Out.Restored
	}
	// THE top-level count: three whole files + the parent counted ONCE.
	if total != 4 {
		t.Errorf("summed Restored = %d, want 4 (3 whole files + the span counted once at completion)", total)
	}
	wantBytes := int64(len(p0) + len(p1) + len(p2) + len("one") + len("two") + len("three"))
	var bytes int64
	for _, img := range res.Images {
		bytes += img.Out.Bytes
	}
	if bytes != wantBytes {
		t.Errorf("summed Bytes = %d, want %d (each byte counted at the image that wrote it)", bytes, wantBytes)
	}
	assertTargetNames(t, target, "big.bin", "keep-1.txt", "keep-2.txt", "keep-3.txt")
	if got := mustRead(t, filepath.Join(target, "big.bin")); string(got) != string(p0)+string(p1)+string(p2) {
		t.Errorf("assembled content wrong: %d bytes", len(got))
	}
}

// --- leave-vs-delete, both gates, both branches ------------------------------

// TestRestoreFreshSingleImageChunkCorruptionDeletesItsPartial pins the
// DELETE branch of a chunked failure: the failing call CREATED the assembly,
// so nothing verified is lost - and the outcome detail must SAY which branch
// fired (a report line is never ambiguous about what remains on disk).
func TestRestoreFreshSingleImageChunkCorruptionDeletesItsPartial(t *testing.T) {
	f := newRestoreFixture(t, "S-RDEL")
	p0 := []byte(strings.Repeat("Q", 100))
	p1 := []byte(strings.Repeat("R", 60))
	f.addChunked("big.bin", []chunkPart{{1, p0}, {1, p1}})
	f.corruptTreeFile(1, "chunks/big.bin/00001") // index 0 is corrupt
	target := filepath.Join(t.TempDir(), "target")

	res, err := f.runInto(t, Selector{}, target)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := res.Images[0].Out
	if out.Failed != 1 || out.Restored != 0 || len(out.Files) != 1 {
		t.Fatalf("outcome = %+v, want exactly one incomplete member", out)
	}
	detail := out.Files[0].Detail
	if out.Files[0].Status != statusIncomplete {
		t.Errorf("status = %q, want %q", out.Files[0].Status, statusIncomplete)
	}
	for _, want := range []string{"corrupt chunks [0]", "first verification", "was deleted", "no final file"} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail %q must contain %q", detail, want)
		}
	}
	if strings.Contains(detail, "LEFT IN PLACE") {
		t.Errorf("detail %q must NOT claim the leave branch for a fresh assembly", detail)
	}
	assertAbsent(t, filepath.Join(target, "big.bin"), "a failed member never becomes a final file")
	assertNoPartials(t, target) // the fresh branch DELETES: it held only this call's bytes
}

// TestRestoreCrossImageSecondGateMismatchLeavesAssembly pins the LEAVE branch
// on the SECOND verification: both chunks pass their stored hashes (the first
// gate), the span completes at image 2, and only the read-back whole-file
// hash refuses it. The assembly then holds image 1's VERIFIED chunk, so it is
// LEFT IN PLACE - and image 1, which recorded nothing while pending, must
// still record nothing afterwards (count-once for the failure lands at the
// detecting image only).
func TestRestoreCrossImageSecondGateMismatchLeavesAssembly(t *testing.T) {
	f := newRestoreFixture(t, "S-R2G")
	p0 := []byte(strings.Repeat("E", 300))
	p1 := []byte(strings.Repeat("F", 200))
	// Synthetic anchor: every chunk row hashes its own bytes correctly, so
	// only the assembled read-back can refuse the file.
	f.addChunkedHash("big.bin", []chunkPart{{1, p0}, {2, p1}},
		sha256Sum([]byte("a whole-file hash the concatenation can never equal")))
	target := filepath.Join(t.TempDir(), "target")

	res, err := f.runInto(t, Selector{}, target)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Images) != 2 {
		t.Fatalf("images = %d, want 2", len(res.Images))
	}
	// Image 1: pending, therefore silent (no row, no count).
	if out := res.Images[0].Out; out.Restored != 0 || out.Failed != 0 || len(out.Files) != 0 {
		t.Errorf("image 1 outcome = %+v, want nothing recorded for the pending parent", out)
	}
	// Image 2: THE failure, once, on the leave branch.
	out2 := res.Images[1].Out
	if out2.Failed != 1 || out2.Restored != 0 || len(out2.Files) != 1 {
		t.Fatalf("image 2 outcome = %+v, want exactly the one incomplete member", out2)
	}
	got := out2.Files[0]
	if got.RelPath != "big.bin" || got.Status != statusIncomplete {
		t.Errorf("finding = %+v, want big.bin incomplete", got)
	}
	for _, want := range []string{"assembled hash mismatch", "second verification", "every chunk passed its stored hash", "LEFT IN PLACE", "no final file"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("detail %q must contain %q", got.Detail, want)
		}
	}
	// The target: no final file, and the assembly SURVIVES with both
	// images' verified bytes (deleting would destroy image 1's verified
	// work for no trust benefit).
	assertAbsent(t, filepath.Join(target, "big.bin"), "a refused assembly never becomes a final file")
	partial := filepath.Join(target, "big.bin.partial")
	fi, err := os.Stat(partial)
	if err != nil {
		t.Fatalf("the assembly must be LEFT IN PLACE for diagnosis: %v", err)
	}
	if fi.Size() != int64(len(p0)+len(p1)) {
		t.Errorf("left assembly = %d bytes, want %d (both images' chunks)", fi.Size(), len(p0)+len(p1))
	}
	// Count-once across the whole run: one failure, nothing else.
	var restored, failed int
	for _, img := range res.Images {
		restored += img.Out.Restored
		failed += img.Out.Failed
	}
	if restored != 0 || failed != 1 {
		t.Errorf("run totals = restored %d / failed %d, want 0 / 1", restored, failed)
	}
}

// --- the B1-mirror selection-wide gate: both orders, both member kinds --------

// TestRestorePartialCollisionGateBothOrders drives the cross-image pair the
// copy pipeline's PER-IMAGE gates cannot see, in BOTH orders. In both the
// colliding member is refused BEFORE any write, the sibling's bytes are
// untouched, unrelated members on the same images still restore, and the
// refusal names BOTH paths (the report must be unambiguous about the pair).
func TestRestorePartialCollisionGateBothOrders(t *testing.T) {
	t.Run("sibling final first", func(t *testing.T) {
		// The destructive order: the sibling's final (target/a.partial)
		// lands at image 1, the colliding member arrives from image 2.
		f := newRestoreFixture(t, "S-RB1A")
		f.addFile(1, "a.partial", []byte("the sibling's verified bytes"))
		f.addFile(2, "a", []byte("the colliding member"))
		f.addFile(2, "unrelated.txt", []byte("unaffected by the gate"))
		target := filepath.Join(t.TempDir(), "target")

		res, err := f.runInto(t, Selector{}, target)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		restored, failed, finding := tallyOutcome(t, res, "a")
		if restored != 2 || failed != 1 {
			t.Fatalf("totals = restored %d / failed %d, want 2 / 1 (sibling + unrelated restore, the collision fails)", restored, failed)
		}
		assertGateDetail(t, finding, "a", "a.partial", target)
		// The sibling's final is INTACT - this is the write step-2a would
		// otherwise have deleted.
		if got := mustRead(t, filepath.Join(target, "a.partial")); string(got) != "the sibling's verified bytes" {
			t.Errorf("the sibling's final was damaged: %q", got)
		}
		if got := mustRead(t, filepath.Join(target, "unrelated.txt")); string(got) != "unaffected by the gate" {
			t.Errorf("an unrelated member must still restore: %q", got)
		}
		assertAbsent(t, filepath.Join(target, "a"), "the colliding member is refused, never half-written")
		// Exactly the two selected names that may exist - a stray derived
		// partial would show up here (a sibling legitimately NAMED
		// *.partial is a final file, so assertNoPartials is the wrong
		// instrument for this fixture).
		assertTargetNames(t, target, "a.partial", "unrelated.txt")
	})

	t.Run("colliding member first", func(t *testing.T) {
		// The mirrored order: the colliding member sits on image 1 and the
		// sibling's final would land on image 2 - the gate must refuse the
		// pair from the FIRST image it sees, before any write at all.
		f := newRestoreFixture(t, "S-RB1B")
		f.addFile(1, "a", []byte("the colliding member"))
		f.addFile(1, "unrelated.txt", []byte("unaffected by the gate"))
		f.addFile(2, "a.partial", []byte("the sibling's verified bytes"))
		target := filepath.Join(t.TempDir(), "target")

		res, err := f.runInto(t, Selector{}, target)
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		restored, failed, finding := tallyOutcome(t, res, "a")
		if restored != 2 || failed != 1 {
			t.Fatalf("totals = restored %d / failed %d, want 2 / 1 (unrelated + sibling restore, the collision fails)", restored, failed)
		}
		assertGateDetail(t, finding, "a", "a.partial", target)
		if got := mustRead(t, filepath.Join(target, "a.partial")); string(got) != "the sibling's verified bytes" {
			t.Errorf("the sibling's final was damaged: %q", got)
		}
		if got := mustRead(t, filepath.Join(target, "unrelated.txt")); string(got) != "unaffected by the gate" {
			t.Errorf("an unrelated member must still restore: %q", got)
		}
		assertAbsent(t, filepath.Join(target, "a"), "the colliding member is refused before any write")
		assertTargetNames(t, target, "a.partial", "unrelated.txt")
	})
}

// tallyOutcome sums the run's counts and extracts one member's finding.
func tallyOutcome(t *testing.T, res Result, rel string) (restored, failed int, finding FileOutcome) {
	t.Helper()
	found := false
	for _, img := range res.Images {
		if img.Err != nil {
			t.Fatalf("image %d error: %v", img.Image, img.Err)
		}
		restored += img.Out.Restored
		failed += img.Out.Failed
		for _, fo := range img.Out.Files {
			if fo.RelPath == rel {
				finding, found = fo, true
			}
		}
	}
	if !found {
		t.Fatalf("no finding recorded for %q (the gate must refuse it, not skip it silently)", rel)
	}
	return restored, failed, finding
}

// assertGateDetail pins the B1 refusal's wording: it is a failure (not a
// skip), it names the collision, and it names BOTH paths - the refused
// member and the selected final it would have clobbered.
func assertGateDetail(t *testing.T, finding FileOutcome, blocked, sibling, target string) {
	t.Helper()
	if finding.Status != statusFailed {
		t.Errorf("finding = %+v, want a failed member (never a silent skip)", finding)
	}
	for _, want := range []string{"partial-path collision", "refusing before any write", fmt.Sprintf("%q", blocked), fmt.Sprintf("%q", sibling)} {
		if !strings.Contains(finding.Detail, want) {
			t.Errorf("detail %q must contain %q (both colliding paths named)", finding.Detail, want)
		}
	}
	if !strings.Contains(finding.Detail, filepath.Join(target, sibling)) {
		t.Errorf("detail %q must name the derived partial path %s", finding.Detail, filepath.Join(target, sibling))
	}
}

// TestRestoreChunkedParentCollisionWithSelectedPartialIsRefused pins the
// GATE'S OWN BRANCH inside the chunked path: a chunked parent whose derived
// partial is another selected member's final is refused before the assembly
// is opened - no .partial created, no chunk streamed, one failure row at the
// detecting image, later images silent (count-once), sibling intact.
func TestRestoreChunkedParentCollisionWithSelectedPartialIsRefused(t *testing.T) {
	f := newRestoreFixture(t, "S-RB1C")
	p0 := []byte(strings.Repeat("k", 128))
	p1 := []byte(strings.Repeat("l", 96))
	f.addChunked("big.bin", []chunkPart{{1, p0}, {2, p1}})
	f.addFile(2, "big.bin.partial", []byte("the sibling's verified bytes"))
	target := filepath.Join(t.TempDir(), "target")

	res, err := f.runInto(t, Selector{}, target)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	restored, failed, finding := tallyOutcome(t, res, "big.bin")
	if restored != 1 || failed != 1 {
		t.Fatalf("totals = restored %d / failed %d, want 1 / 1 (the sibling restores, the assembly is refused)", restored, failed)
	}
	if finding.Status != statusIncomplete {
		t.Errorf("finding = %+v, want the incomplete class", finding)
	}
	for _, want := range []string{"partial-path collision", "refusing before any write", "no final file written", fmt.Sprintf("%q", "big.bin.partial")} {
		if !strings.Contains(finding.Detail, want) {
			t.Errorf("detail %q must contain %q", finding.Detail, want)
		}
	}
	// Refused BEFORE any write: no assembly, no final, sibling untouched,
	// and the second image records nothing more for the failed parent.
	assertAbsent(t, filepath.Join(target, "big.bin"), "the refused assembly never becomes a final file")
	if _, err := os.Stat(filepath.Join(target, "big.bin.partial.partial")); !os.IsNotExist(err) {
		t.Errorf("the gate must fire before the assembly open (stat err = %v)", err)
	}
	if got := mustRead(t, filepath.Join(target, "big.bin.partial")); string(got) != "the sibling's verified bytes" {
		t.Errorf("the sibling's final was damaged: %q", got)
	}
	assertTargetNames(t, target, "big.bin.partial")
	// Image 2 holds more chunks of the refused parent: still silent.
	for _, img := range res.Images {
		if img.Image == 2 && len(img.Out.Files) != 0 {
			t.Errorf("image 2 listed rows for the already-refused parent: %+v", img.Out.Files)
		}
	}
}

// --- cancellation DURING a whole-file stream (the writePayload abort path) ---

// TestRestoreCancellationMidStreamLeavesPartialNoCounts drives the cancellation
// the doc names explicitly for WHOLE FILES: "the in-flight partial is always
// left in place and renames nothing". The source payload is a FIFO, which is
// what makes the window deterministic - the copy stream can be parked inside
// Read while the context is cancelled, then released, exactly like a long
// stream cut by a signal (no sleeps, no size games: the partial's existence IS
// the "the write started" signal the test waits for).
func TestRestoreCancellationMidStreamLeavesPartialNoCounts(t *testing.T) {
	f := newRestoreFixture(t, "S-RCXW")
	f.addFile(1, "data.txt", []byte("a recorded payload long enough to outlive the first pipe write"))

	// Swap the media payload for a FIFO: os.Open on it succeeds only once a
	// writer holds it, and every Read after that is under the test's control.
	fifo := filepath.Join(f.tree(1), "files", "data.txt")
	if err := os.Remove(fifo); err != nil {
		t.Fatalf("removing the planted payload: %v", err)
	}
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	w, err := os.OpenFile(fifo, os.O_RDWR, 0) // O_RDWR: open never parks
	if err != nil {
		t.Fatalf("opening the FIFO writer: %v", err)
	}
	defer w.Close() //nolint:errcheck // released at the end of the stream

	target := filepath.Join(t.TempDir(), "target")
	sel := f.resolve(t, Selector{})
	sel.Target = target
	restorer := NewImageRestorer(RestorerDeps{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		res ImageRestoreResult
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := restorer.RestoreImage(ctx,
			ImageSupply{SetID: f.setID, Image: 1, Kind: SourceCopyTree, Path: f.tree(1)},
			sel, mkWorkspace(t))
		done <- outcome{res, err}
	}()

	partial := filepath.Join(target, "data.txt.partial")
	if _, err := w.Write(make([]byte, 100)); err != nil {
		t.Fatalf("priming the FIFO: %v", err)
	}
	// The partial's creation is the deterministic "the stream started"
	// marker: writePayload opens it right before io.Copy, and there is no
	// context check between the two.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(partial); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the whole-file partial never appeared - the write did not start")
		}
		time.Sleep(2 * time.Millisecond)
	}

	cancel()                                              // cut the stream while it is parked in Read...
	if _, err := w.Write(make([]byte, 100)); err != nil { // ... release it ...
		t.Fatalf("releasing the FIFO after the cancel: %v", err)
	}
	_ = w.Close() // ... and end the source (never before the cancel)

	got := <-done
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("RestoreImage err = %v, want context.Canceled", got.err)
	}
	// No outcome, no count, no progress for the interrupted member - a
	// cancelled run never invents a result.
	if got.res.Restored != 0 || got.res.Failed != 0 || got.res.Skipped != 0 || len(got.res.Files) != 0 {
		t.Errorf("outcome = %+v, want no counts and no findings for an aborted stream", got.res)
	}
	// The in-flight partial is LEFT in place (never renamed, never deleted)
	// and it can never be mistaken for a finished file.
	fi, err := os.Stat(partial)
	if err != nil {
		t.Fatalf("the in-flight partial must be left in place: %v", err)
	}
	if fi.Size() >= 400 {
		t.Errorf("partial size = %d, want a partial stream (the record says 400 bytes)", fi.Size())
	}
	assertAbsent(t, filepath.Join(target, "data.txt"), "a cancelled run renames nothing")
}

// --- exactly ONE xorriso invocation per ISO image ----------------------------

// countingXorriso is fakeXorriso plus an invocation log: each run of the
// script appends one line, so the "ONE xorriso invocation per image" contract
// of the extraction doc is observable instead of merely plausible.
func countingXorriso(t *testing.T, preparedTree, logPath string) *xorriso.Tool {
	t.Helper()
	script := filepath.Join(t.TempDir(), "counting-xorriso")
	body := "#!/bin/sh\n" +
		"echo run >> " + shellQuote(logPath) + "\n" +
		"[ \"$1\" = \"-indev\" ] || { echo 'expected -indev' >&2; exit 9; }\n" +
		"[ \"$3\" = \"-osirrox\" ] && [ \"$4\" = \"on\" ] || { echo 'expected -osirrox on' >&2; exit 9; }\n" +
		"[ \"$5\" = \"-extract\" ] && [ \"$6\" = \"/\" ] || { echo 'expected -extract /' >&2; exit 9; }\n" +
		"dest=\"$7\"\n" +
		"mkdir -p \"$dest\" || exit 8\n" +
		"cp -R " + shellQuote(preparedTree) + "/. \"$dest\"/ || exit 9\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("writing the counting xorriso: %v", err)
	}
	return &xorriso.Tool{Path: script}
}

// countRuns reads the invocation log (0 when the tool was never exec'd).
func countXorrisoRuns(t *testing.T, logPath string) int {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("reading the xorriso invocation log: %v", err)
	}
	return len(strings.Fields(string(data)))
}

// TestRestoreISOExtractsExactlyOncePerImage pins the extraction contract:
// SourceISO images go through ONE osirrox invocation each (into a UNIQUE
// workspace subdirectory, after the free-space pre-check), no matter how many
// members the image carries - a second exec would double the extraction cost
// and could race its own scratch.
func TestRestoreISOExtractsExactlyOncePerImage(t *testing.T) {
	f := newRestoreFixture(t, "S-R1X")
	f.addFile(1, "a.txt", []byte("alpha"))
	f.addFile(1, "sub/b.txt", []byte("beta"))
	f.addSymlink(1, "link", "a.txt")
	f.addDir(1, "empty-dir", 0o750)
	f.addFile(2, "c.txt", []byte("gamma"))

	sel := f.resolve(t, Selector{})
	target := filepath.Join(t.TempDir(), "target")
	sel.Target = target
	workspace := mkWorkspace(t)

	log1 := filepath.Join(t.TempDir(), "xorriso-1.log")
	log2 := filepath.Join(t.TempDir(), "xorriso-2.log")
	iso1 := filepath.Join(t.TempDir(), "S-R1X-001.iso")
	iso2 := filepath.Join(t.TempDir(), "S-R1X-002.iso")
	for _, iso := range []string{iso1, iso2} {
		if err := os.WriteFile(iso, make([]byte, 4096), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Two ISO images in ONE run: one extraction each, same workspace path.
	restorer := NewImageRestorer(RestorerDeps{Tool: countingXorriso(t, f.tree(1), log1)})
	if _, err := restorer.RestoreImage(context.Background(),
		ImageSupply{SetID: f.setID, Image: 1, Kind: SourceISO, Path: iso1},
		sel, workspace); err != nil {
		t.Fatalf("image 1 over an ISO: %v", err)
	}
	if got := countXorrisoRuns(t, log1); got != 1 {
		t.Errorf("xorriso invocations for image 1 = %d, want exactly 1 (the single -extract / contract)", got)
	}
	// The orchestration wipes the workspace between images (8.3) - modelled
	// here by the same clean the loop performs.
	if err := cleanWorkspace(workspace); err != nil {
		t.Fatalf("cleaning the workspace: %v", err)
	}
	restorer2 := NewImageRestorer(RestorerDeps{Tool: countingXorriso(t, f.tree(2), log2)})
	if _, err := restorer2.RestoreImage(context.Background(),
		ImageSupply{SetID: f.setID, Image: 2, Kind: SourceISO, Path: iso2},
		sel, workspace); err != nil {
		t.Fatalf("image 2 over an ISO: %v", err)
	}
	if got := countXorrisoRuns(t, log2); got != 1 {
		t.Errorf("xorriso invocations for image 2 = %d, want exactly 1", got)
	}
	// Each extraction lived in ITS OWN subdirectory of the workspace (the
	// unique-per-call dir), and the members really came out of it.
	for _, rel := range []string{"a.txt", "sub/b.txt", "empty-dir", "link", "c.txt"} {
		if _, err := os.Lstat(filepath.Join(target, rel)); err != nil {
			t.Errorf("member %s did not restore from the extraction: %v", rel, err)
		}
	}
	if got := string(mustRead(t, filepath.Join(target, "a.txt"))); got != "alpha" {
		t.Errorf("a.txt = %q", got)
	}
	assertNoPartials(t, target)
}
