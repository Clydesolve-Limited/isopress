package cli

// B1 ORDERING-FIX acceptance (the 2026-10-20 CIFS change-detection
// hardening, the go-reviewer's blocking rejection): the whole-file
// accumulation used to feed chunks in the run's PROCESSING order
// (images ascending, then (file_id, chunk_index) per image), which
// equals the file's BYTE order only when the balancer assigned chunks
// in CONTIGUOUS ASCENDING BLOCKS. It need not: chunks are independent
// WFD placement items, so equal-size chunks can be INTERLEAVED across
// open images, and a final chunk processed LAST over permuted earlier
// feeds hashed a PERMUTED stream - a guaranteed mismatch that
// classified UNCHANGED content source-changed (exit 10, final chunk
// verified-but-unmarked, image incomplete): the spurious-failure class
// the hardening exists to eliminate. Every prior hardening test used
// contiguous splits (chunk ~= usable forces one chunk per image), so
// the non-contiguous assignment shape was uncovered.
//
//   - TestCopyInterleavedChunkAssignmentNoSpuriousSourceChanged is the
//     DECISIVE regression pin: a 3-chunk parent assigned interleaved
//     across two images, seeded directly in the catalog through the
//     exported PlanTx API (the crossLinkFixture / copyAdvFixture e2e
//     pattern - the balancer's own deterministic fixtures never
//     exercised this shape, so the fixture reproduces the legal
//     placement directly), driven through the REAL copy command. On
//     UNCHANGED content every interleaving must end clean: no
//     source-changed failure, no whole-file mismatch event, every
//     chunk marked, the parent mirror flipped, both images complete -
//     because the in-order feed gate (wholeFileCheck.nextExpected /
//     broken) can never let the compare run over a non-complete
//     in-order accumulation, so a permuted stream is never hashed.
//
//   - TestCopyContiguousChunkAssignmentRunsWholeFileCheckAndMatches is
//     the POSITIVE control on the same fixture: contiguous in-image
//     assignment keeps the check OPERATING - the final chunk is
//     DeferMark'd and can only be marked via decideWholeFile's MATCH,
//     which the clean run plus the absence of the fallback's
//     broken-feed warning prove (the two are the only DeferMark
//     completions in a clean run).
//
//   - TestCopyFinalChunkItemDoneExactlyOnceBothPaths pins N1: the
//     engine withholds the final chunk's ItemDone under DeferMark, and
//     BOTH mark-completion paths (the MATCH and the fallback MarkChunk)
//     must emit exactly that one completion, so the progress counters
//     stay consistent between them.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"isopress/internal/catalog"
	"isopress/internal/copyengine"
	"isopress/internal/fsmeta"
	"isopress/internal/media"
	"isopress/internal/progress"
)

// chunkOrderFixture seeds a scanned + planned set whose ONLY payload
// is one chunked parent ("big.bin") whose chunk rows are assigned to
// images exactly as placement describes (placement[i] = the image of
// chunk i), through the EXPORTED catalog API - the established
// crossLinkFixture / copyAdvFixture e2e pattern (BeginScanTx +
// BeginPlanTx seeding, never test-side SQL). The real source file is
// written first so the copy's 5.1 lstat comparison mirrors the scan
// row exactly (fsmeta.PathMeta of the very file), and the scan row
// carries the whole-file hash the hardening compares against.
//
// An interleaved placement (chunk i and i+2 on one image, i+1 on
// another) is a LEGAL balancer outcome - chunks are independent WFD
// placement items - that the balancer's own deterministic fixtures
// never exercised: this fixture reproduces the shape directly, exactly
// like the both-assigned family fixture reproduced ITS reject shape.
func chunkOrderFixture(t *testing.T, setID string, chunkSize, total int64, placement []int) (catPath, sourceRoot string, content []byte) {
	t.Helper()
	sourceRoot = t.TempDir()
	catPath = filepath.Join(t.TempDir(), "catalog.sqlite")
	if err := catalog.Create(catPath); err != nil {
		t.Fatal(err)
	}
	db, err := catalog.Open(catPath, catalog.OpenReadWrite)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if err := db.EnsureSet(ctx, setID, sourceRoot, time.Now().UnixNano(), catalog.ToolVersion); err != nil {
		t.Fatal(err)
	}
	runID, err := db.StartScanRun(ctx, catalog.ScanRun{
		SetID: setID, Kind: "ingest", Root: sourceRoot, StartedAt: time.Now().UnixNano(),
	})
	if err != nil {
		t.Fatal(err)
	}

	content = pattern(41, int(total))
	bigPath := filepath.Join(sourceRoot, "big.bin")
	writeFileP(t, bigPath, content, 0o644)
	meta, err := fsmeta.PathMeta(bigPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)

	tx, err := db.BeginScanTx(ctx, runID, setID)
	if err != nil {
		t.Fatal(err)
	}
	fileID, err := tx.InsertScanFile(ctx, catalog.FileRow{
		RelPath: "big.bin", EntryKind: "regular", Size: meta.Size, Mode: 0o644,
		MtimeNS: meta.MtimeNS, Dev: int64(meta.Dev), Inode: int64(meta.Inode),
		Nlink: 1, Hash: sum[:], Status: "active",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// The plan leg, in ONE PlanTx (the 4.5 shape the real planner
	// writes): the images the placement names, the chunk rows of the
	// full decomposition (offsets contiguous from 0, hash NULL - the
	// 4.3 lazy first-write contract), the files.chunked stamp, and the
	// per-chunk assignments.
	ptx, err := db.BeginPlanTx(ctx, 0, setID)
	if err != nil {
		t.Fatal(err)
	}
	maxImage := 0
	for _, img := range placement {
		if img > maxImage {
			maxImage = img
		}
	}
	for img := 1; img <= maxImage; img++ {
		if err := ptx.InsertImage(ctx, catalog.ImageSpec{
			ImageID: int64(img), NominalSize: 100000, UsableSize: 98000, MediaProfile: "disk",
		}); err != nil {
			t.Fatal(err)
		}
	}
	var specs []catalog.ChunkSpec
	for offset, i := int64(0), 0; offset < total; i++ {
		length := chunkSize
		if offset+length > total {
			length = total - offset
		}
		specs = append(specs, catalog.ChunkSpec{Index: i, Offset: offset, Length: length})
		offset += length
	}
	if len(specs) != len(placement) {
		t.Fatalf("fixture: decomposition has %d chunks but placement describes %d",
			len(specs), len(placement))
	}
	if err := ptx.EnsureChunkRows(ctx, fileID, specs); err != nil {
		t.Fatal(err)
	}
	if err := ptx.StoreChunkSize(ctx, fileID, chunkSize, len(specs)); err != nil {
		t.Fatal(err)
	}
	for i, img := range placement {
		if err := ptx.AssignChunk(ctx, fileID, i, int64(img)); err != nil {
			t.Fatalf("AssignChunk chunk %d -> image %d: %v", i, img, err)
		}
	}
	if err := ptx.Commit(); err != nil {
		t.Fatal(err)
	}
	return catPath, sourceRoot, content
}

// runInterleavedChunkOrderScenario drives the REAL copy command over a
// chunkOrderFixture interleaved placement and asserts the in-order
// gate's observable contract on UNCHANGED content: the whole-file
// compare NEVER fires (it cannot run over a non-complete in-order
// accumulation), so there is no spurious source-changed failure, and
// the run converges clean - every chunk marked through a normal or
// fallback mark path, the parent mirror flipped, every planned image
// complete with its MANIFEST.json, exit 0.
func runInterleavedChunkOrderScenario(t *testing.T, setID string, placement []int) {
	t.Helper()
	cat, _, content := chunkOrderFixture(t, setID, 10000, 25000, placement)
	dest := filepath.Join(t.TempDir(), "dest")

	stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json")
	if code != 0 {
		t.Fatalf("copy exit = %d, want 0 (an interleaved assignment of UNCHANGED content must never "+
			"produce a spurious source-changed failure; stdout: %s\nstderr: %s)", code, stdout, stderr)
	}
	var rep copyReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("copy --json does not parse: %v\n%s", err, stdout)
	}
	if rep.Status != "ok" || rep.Skipped != 0 || rep.Failed != 0 {
		t.Fatalf("report = %+v, want a clean ok run (a permuted feed must never reach the compare)", rep)
	}

	// Every chunk ended MARKED and the parent mirror flipped: the
	// gate's fail-closed path completes the final chunk through the
	// normal/fallback mark path, never the compare.
	db := catOpen(t, cat)
	assertAllCopied(t, db, setID)

	// Every planned image completed and wrote its MANIFEST.json - the
	// pre-fix spurious mismatch left the final chunk's image
	// incomplete with a deferred manifest.
	if len(rep.Images) != 2 {
		t.Fatalf("report images = %+v, want one entry per planned image", rep.Images)
	}
	for _, img := range rep.Images {
		if !img.Complete || !img.Manifest {
			t.Fatalf("image %d report = %+v, want complete=true manifest_written=true "+
				"(the gate's fail-closed path must not leave the image incomplete)", img.Image, img)
		}
	}

	// The media truth: each chunk's verified bytes landed on ITS
	// image (the run reads the UNCHANGED source, so every chunk holds
	// the source slice the catalog offsets describe).
	for i, img := range placement {
		start := int64(i) * 10000
		end := start + 10000
		if end > int64(len(content)) {
			end = int64(len(content))
		}
		mediaPath, err := media.ChunkMediaPath("big.bin", int64(i))
		if err != nil {
			t.Fatal(err)
		}
		assertBytes(t, filepath.Join(destImageDir(dest, setID, int64(img)),
			filepath.FromSlash(mediaPath)), content[start:end])
	}

	// THE SPURIOUS-FAILURE PIN: the whole-file compare never fired on
	// this run - no mismatch warning reached run_events (pre-fix, the
	// permuted accumulation of the "final processed last" interleave
	// produced exactly such a row, the verified-but-unmarked final
	// chunk, and exit 10).
	if n := countWarnEvents(t, db, newestRunID(t, db, "copy"), "whole-file sha256 mismatch"); n != 0 {
		t.Errorf("%d whole-file mismatch run_events rows on UNCHANGED interleaved content, want 0 "+
			"(the in-order feed gate must keep a permuted stream from ever being compared)", n)
	}
}

// TestCopyInterleavedChunkAssignmentNoSpuriousSourceChanged is THE
// regression pin of the B1 rejection, in three interleavings of a
// 3-chunk parent (10000 + 10000 + 5000 over 25000 bytes) across two
// images:
//
//   - "final processed last" ({chunk0,chunk2} -> image 2, chunk1 ->
//     image 1; processing order 1, 0, 2): THE pre-fix repro - chunk1's
//     permuted feed plus chunk0's, then the DeferMark'd final chunk2,
//     hashed [chunk1][chunk0][chunk2] -> guaranteed mismatch on
//     unchanged content. Post-fix the feed breaks at chunk1 (index 1
//     before the needed 0), so the compare never runs;
//   - the reviewer's example ({chunk1,chunk2} -> image 1, chunk0 ->
//     image 2; processing order 1, 2, 0): the final chunk2 sits in the
//     FIRST-processed image over a permuted prefix;
//   - the reverse interleave ({chunk0,chunk2} -> image 1, chunk1 ->
//     image 2; processing order 0, 2, 1): chunk2 arrives out of order
//     between two in-order-feel feeds.
//
// All three end clean on unchanged content (see the scenario helper).
func TestCopyInterleavedChunkAssignmentNoSpuriousSourceChanged(t *testing.T) {
	t.Run("final processed last (chunk1 -> image 1; chunk0+final -> image 2)", func(t *testing.T) {
		runInterleavedChunkOrderScenario(t, "S-ILD", []int{2, 1, 2})
	})
	t.Run("final in first image (chunk1+final -> image 1; chunk0 -> image 2)", func(t *testing.T) {
		runInterleavedChunkOrderScenario(t, "S-ILE", []int{2, 1, 1})
	})
	t.Run("reverse interleave (chunk0+final -> image 1; chunk1 -> image 2)", func(t *testing.T) {
		runInterleavedChunkOrderScenario(t, "S-ILR", []int{1, 2, 1})
	})
}

// TestCopyContiguousChunkAssignmentRunsWholeFileCheckAndMatches is the
// POSITIVE control on the SAME fixture: contiguous in-image assignment
// (all three chunks ascending on one image) keeps the hardening
// OPERATING - the processing order IS the byte order, the gate feeds
// the whole file, and the compare runs and matches on unchanged
// content.
//
// What makes the "check ran" claim PROVABLE in a clean run: the final
// chunk was DeferMark'd (its count estimate held - chunk0 and chunk1
// were fed in order), so it could only be marked through one of the
// two DeferMark completions: decideWholeFile's MATCH, or the fallback
// MarkChunk, which requires a broken final feed and always emits the
// "cannot re-read chunk" warning first. Clean run + that warning
// absent therefore pins the MATCH path - i.e. the compare executed
// over the complete in-order accumulation and agreed with the
// scan-time whole-file hash.
func TestCopyContiguousChunkAssignmentRunsWholeFileCheckAndMatches(t *testing.T) {
	const setID = "S-CONTFIX"
	cat, _, content := chunkOrderFixture(t, setID, 10000, 25000, []int{1, 1, 1})
	dest := filepath.Join(t.TempDir(), "dest")

	stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json")
	if code != 0 {
		t.Fatalf("copy exit = %d, want 0 (stdout: %s\nstderr: %s)", code, stdout, stderr)
	}
	var rep copyReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("copy --json does not parse: %v\n%s", err, stdout)
	}
	if rep.Status != "ok" || rep.Skipped != 0 || rep.Failed != 0 {
		t.Fatalf("report = %+v, want a clean ok run", rep)
	}
	db := catOpen(t, cat)
	assertAllCopied(t, db, setID)
	if len(rep.Images) != 1 || !rep.Images[0].Complete || !rep.Images[0].Manifest {
		t.Fatalf("report images = %+v, want the single planned image complete with its manifest", rep.Images)
	}
	// The media truth: the whole file reassembles from its chunks.
	for i := 0; i < 3; i++ {
		start := int64(i) * 10000
		end := start + 10000
		if end > int64(len(content)) {
			end = int64(len(content))
		}
		mediaPath, err := media.ChunkMediaPath("big.bin", int64(i))
		if err != nil {
			t.Fatal(err)
		}
		assertBytes(t, filepath.Join(destImageDir(dest, setID, 1),
			filepath.FromSlash(mediaPath)), content[start:end])
	}
	// THE "CHECK RAN" DISJUNCTION: no broken-feed warning anywhere -
	// the fallback completion is excluded, so the DeferMark'd final
	// chunk was marked by decideWholeFile's MATCH, which only the
	// compare can produce.
	if strings.Contains(stderr, "cannot re-read chunk") {
		t.Fatalf("the contiguous control emitted the fallback's broken-feed warning:\n%s", stderr)
	}
	if n := countWarnEvents(t, db, newestRunID(t, db, "copy"), "cannot re-read chunk"); n != 0 {
		t.Errorf("%d broken-feed warn rows in run_events, want 0 (they would place the final chunk on the fallback path)", n)
	}
}

// --- N1: the fallback mark path emits the withheld ItemDone ------------------

// TestCopyFinalChunkItemDoneExactlyOnceBothPaths pins the N1
// consistency fix at the progress layer: under DeferMark the engine
// withholds the final chunk's ItemDone, and BOTH pipeline
// mark-completion paths must emit exactly that one completion -
// decideWholeFile's MATCH (already did) and the fallback MarkChunk
// (the N1 addition) - so the progress counters stay consistent
// between the two.
//
// The fallback state is injected deterministically through the ONE
// seam the two reads do NOT share: the engine's read-back verify is
// injectable (Deps.ReadBack), while the pipeline's post-copy
// accumulation feed deliberately opens the published media file
// directly (wholeFileCheck.feedFrom's retry-safety doc). Reading the
// final chunk THROUGH the seam and then removing the published file
// reproduces the honest "the media vanished between verify and feed"
// state: the verify passes on the seam's bytes, feedFrom's os.Open
// fails, the check breaks, and the final chunk completes through the
// fallback mark path - no root-fragile permission tricks, no races.
// (The removed media file is test-side debris of the injection; the
// assertions cover the catalog and progress halves both paths share.)
func TestCopyFinalChunkItemDoneExactlyOnceBothPaths(t *testing.T) {
	cases := []struct {
		name           string
		breakFinalFeed bool
	}{
		{name: "match path (compare ran and matched)", breakFinalFeed: false},
		{name: "fallback path (final feed broken)", breakFinalFeed: true},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setID := fmt.Sprintf("S-N1-%d", i)
			cat, root, _ := chunkOrderFixture(t, setID, 10000, 20000, []int{1, 1})
			dest := filepath.Join(t.TempDir(), "dest")
			db, err := catalog.Open(cat, catalog.OpenReadWrite)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			ctx := context.Background()
			runID, err := db.StartCopyRun(ctx, catalog.CopyRun{
				SetID: setID, Root: dest, StartedAt: time.Now().UnixNano(),
			})
			if err != nil {
				t.Fatal(err)
			}

			// THE CAPTURING BUS: progress.Bus exposes no read API
			// (scan_totals_test.go's doc), so the ItemDone stream is
			// counted through the REAL plain renderer with an
			// always-advancing fake clock - 9.3's cadence then prints
			// exactly one "copy: files" progress line per ItemDone.
			var out lockedBuffer
			bus := progress.NewBus()
			tick := time.Now()
			wait := progress.Start(progress.New(progress.Options{
				Mode:   progress.ModePlain,
				Stderr: &out,
				Now: func() time.Time {
					tick = tick.Add(10 * time.Second)
					return tick
				},
			}), bus)

			// The hand-fed run export (the copy_adversarial in-process
			// pattern): any bytes serve - the embed path copies them.
			exportBytes := []byte("pretend catalog export bytes")
			exportSum := sha256.Sum256(exportBytes)
			exportDir := t.TempDir()
			exportPath := filepath.Join(exportDir, media.ExportFileName)
			if err := os.WriteFile(exportPath, exportBytes, 0o644); err != nil {
				t.Fatal(err)
			}
			export := &runExport{
				path: exportPath, size: int64(len(exportBytes)), hash: exportSum[:], dir: exportDir,
			}

			finalChunkPath := filepath.Join(destImageDir(dest, setID, 1), "chunks", "big.bin", "00002")
			r := &copyRunner{
				db: db, setID: setID, sourceRoot: root, destAbs: dest, runID: runID,
				bus:        bus,
				deps:       copyengine.Deps{Bus: bus},
				wholeFiles: map[int64]*wholeFileCheck{},
			}
			if tc.breakFinalFeed {
				r.deps.ReadBack = func(path string) (io.ReadCloser, error) {
					if filepath.Clean(path) == filepath.Clean(finalChunkPath) {
						b, rerr := os.ReadFile(path)
						if rerr != nil {
							return nil, rerr
						}
						if rerr := os.Remove(path); rerr != nil {
							return nil, rerr
						}
						return io.NopCloser(bytes.NewReader(b)), nil
					}
					return os.Open(path)
				}
			}

			if err := r.copyImage(ctx, 1, export); err != nil {
				t.Fatalf("copyImage(image 1) = %v; a broken final feed is an item-level problem, not a "+
					"terminal error", err)
			}
			bus.Finish(progress.Done{Summary: "copy: done"})
			wait()

			// EXACTLY ONE ItemDone per chunk: the final chunk is
			// counted once by the path under test (match:
			// decideWholeFile; fallback: the N1 emission) - a missing
			// emission undercounts (the pre-N1 fallback), a double
			// emission overcounts.
			var doneLines []string
			for _, l := range strings.Split(out.String(), "\n") {
				if strings.HasPrefix(l, "copy: files ") {
					doneLines = append(doneLines, l)
				}
			}
			if len(doneLines) != 2 {
				t.Fatalf("ItemDone events = %d (%v), want exactly 2 (one per chunk; the final chunk "+
					"counted exactly once - N1)", len(doneLines), doneLines)
			}
			if !strings.Contains(doneLines[1], "files 2/2") {
				t.Errorf("second progress line = %q, want the completed 2/2 cumulative count", doneLines[1])
			}

			// Both paths mark through the SAME transaction: both chunk
			// flags stand, the parent mirror flipped, and the image
			// completed with its manifest (the fallback warns - the
			// broken feed - but a warning never flips the exit class
			// nor image completeness).
			assertAllCopied(t, catOpen(t, cat), setID)
			if len(r.images) != 1 || !r.images[0].Complete || !r.images[0].Manifest {
				t.Fatalf("image report = %+v, want one complete image with its manifest", r.images)
			}
			if tc.breakFinalFeed {
				if n := countWarnEvents(t, catOpen(t, cat), runID, "cannot re-read chunk"); n != 1 {
					t.Errorf("fallback run: %d broken-feed warn rows, want exactly 1 (the injection's "+
						"honest warning)", n)
				}
			} else if n := countWarnEvents(t, catOpen(t, cat), runID, "cannot re-read chunk"); n != 0 {
				t.Errorf("match run: %d broken-feed warn rows, want 0", n)
			}
		})
	}
}
