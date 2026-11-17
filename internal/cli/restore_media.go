package cli

// T5.5: the media-alone restore plumbing of DESIGN_SPEC 8.5 at the cli
// seam (IMPLEMENTATION_PLAN 4.6 T5.5). restore.go owns the flags and
// the report; this file owns the pieces the 8.5 flow adds to the
// command:
//
//   - openRestoreCatalog: THE CATALOG SWAP. With --from-media the
//     catalog is the media's own FR-034 export, temp-copied under
//     --work-dir and opened READ-ONLY (the schema-version gate of 2.1
//     applies naturally to the copy: a future-version export fails with
//     the clear ErrSchemaTooNew message, exit 4). The set identity is
//     derived FROM the export (set-scoped by construction, one sets
//     row) and cross-checked against the manifest and --set-id. Reads
//     of the copy that the PRAGMA gate passed but cannot answer are
//     classified through catalog.MediaReadErr (the catalog class,
//     exit 4).
//   - mediaExportScratch: the TEMP-COPY step and its rationale - see
//     the function doc.
//   - verifyMediaManifestAnchor: 8.5's manifest-integrity step over the
//     open export copy, with the honest chain documentation.
//   - the chunk-hash fill's SECOND seat, sidecarManifestRestorer: the
//     supply-following half of the lazily-hashed-chunk fill (see
//     mediaStore's type doc for the finding and the seat map).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"isopress/internal/catalog"
	"isopress/internal/media"
	"isopress/internal/progress"
	"isopress/internal/report"
	"isopress/internal/restore"
)

// --- the media store wrapper (the lazily-hashed-chunk finding) ----------------

// mediaStore wraps the export-backed restore.Store with the one 8.5
// adjustment the media-alone flow needs. THE FINDING, measured
// end-to-end and reported per the brief:
//
// THE EXPORT'S CHUNK ROWS CARRY hash=NULL. FR-034's export is produced
// by copy/build at RUN START (produceRunExport - one snapshot per run,
// embedded identically in every image, FR-035), but DESIGN_SPEC 2.3's
// chunks.hash is LAZILY STORED AT FIRST WRITE (4.3 "lazy hashing":
// the chunk's bytes are hashed as they are first written and verified,
// then stored). The two facts compose to: a run-start export snapshots
// the chunks rows BEFORE their hashes exist, so the export alone
// cannot run 8.4's FIRST verification (per-chunk hash) for a chunked
// parent - the second verification (the whole-file hash) anchors fine
// through files.hash, which scan wrote long before.
//
// THE HONEST RESOLUTION, using what the media itself records: the
// MANIFEST.json embedded next to the export carries per-image CHUNK
// TABLES with every chunk's read-back-verified hash (6.6: "chunk tables
// for chunked parents (index, length, hash, and reassembly order)" -
// exactly the media-alone reassembly record FR-033 exists for). This
// wrapper fills a NULL export chunk hash from a media manifest's chunk
// table (same parent relpath, same catalog 0-based index). When the
// export DOES carry a hash (a resume-run export snapshots the
// previous run's stored hashes), the export value wins untouched - the
// catalog stays the primary anchor, the manifest fills its known gap.
// A chunk with no hash in either place stays NULL and fails in the
// engine with the existing honest "no stored hash" per-chunk failure.
//
// THE TWO SEATS OF THE FILL (one implementation, fillChunkHashes; both
// are NULL-only, so they compose in any order and neither can override
// the export):
//
//   - THIS seat runs at inventory time, over the manifests the media
//     itself carried beside the export (ResolveMedia) plus the
//     --image N=DIR sidecars augmentManifestsWithExplicit read: the
//     anchors that exist BEFORE supply is resolved.
//   - sidecarManifestRestorer runs per image, AFTER the supply pass
//     resolved that image's source, and consults the sidecar
//     MANIFEST.json of WHATEVER SourceCopyTree directory the supply
//     actually resolved - the --copy-tree-root and prompted-path
//     supplements this seat cannot know about (the T5.5 QA finding:
//     a fill that did not follow the supplement left a first-run
//     chunked span unanchored even though its tree sat right there).
//
// THE MEDIA CLASS SEAT (T5.5 fold-in): mediaStore is also where the
// SELECTION-PHASE export reads - restore.Store's ActiveRecords,
// ActiveDirectories, Placement, and ImageInventory - take their error
// class on a --from-media run. See the classification section below the
// ImageInventory method for the contract and the conditional.
type mediaStore struct {
	*restoreStore
	manifests map[int64]media.Manifest
}

// ImageInventory fills NULL chunk hashes from the image's media
// manifest (see the type doc for the finding) and classifies a failed
// export read with the catalog class (see the classification section
// below).
func (s *mediaStore) ImageInventory(ctx context.Context, setID string, imageID int64) (restore.Inventory, error) {
	inv, err := s.restoreStore.ImageInventory(ctx, setID, imageID)
	if err != nil {
		return restore.Inventory{}, s.db.MediaReadErr("reading an image's inventory from the media export", err)
	}
	man, ok := s.manifests[imageID]
	if !ok {
		return inv, nil // no manifest for this image on the media: no fill possible
	}
	fillChunkHashes(inv.Chunks, man)
	return inv, nil
}

// --- the selection-phase export reads: THE MEDIA CLASS SEAT -------------------
//
// ResolveSelection's four Store reads run AFTER openRestoreCatalog's
// gates, so an export that opens and PASSES the PRAGMA integrity gate
// yet cannot answer them would leave the run with a bare driver error -
// the generic fatal (exit 1). That state is real and reachable BY
// DESIGN: the read-only open accepts a PAST-version export (only a
// FUTURE user_version is hard-gated, DESIGN_SPEC 2.1 - the FR-035
// aged-media case), and an aged export can have lost a table or column
// since it was written (e.g. a directories table dropped, or a files
// table missing status/superseded_by). Every OTHER export refusal of
// this flow - missing, damaged, future-version, two sets rows, the
// open/identity reads - is the catalog class (exit 4), and
// openRestoreCatalog's godoc promises no export failure of this flow
// leaves as the generic fatal. catalog.MediaReadErr closes that gap at
// THIS seat, which is the only Store a --from-media run ever uses.
//
// THE CONDITIONAL, exactly like runRestore's CopyImageIDs conditional:
// the plain --catalog path KEEPS its pre-existing class. Aligning it
// would touch the whole tool's foreign-catalog behavior (validate and
// report answer a foreign sqlite the same way) and is out of scope, so
// the wrap lives ONLY on mediaStore - restoreStore's four reads are
// untouched, and each method below documents the contrast. Context
// errors pass through MediaReadErr unchanged: an interrupted read
// (context.Canceled / DeadlineExceeded) never masquerades as a corrupt
// export.

// ActiveRecords classifies the media-path active-files read (the
// section doc above has the contract; the plain --catalog path keeps
// its pre-existing class).
func (s *mediaStore) ActiveRecords(ctx context.Context, setID string) ([]restore.Record, error) {
	recs, err := s.restoreStore.ActiveRecords(ctx, setID)
	if err != nil {
		return nil, s.db.MediaReadErr("reading the media export's active records", err)
	}
	return recs, nil
}

// ActiveDirectories classifies the media-path active-directories read
// (the section doc above has the contract; the plain --catalog path
// keeps its pre-existing class).
func (s *mediaStore) ActiveDirectories(ctx context.Context, setID string) ([]restore.DirRecord, error) {
	dirs, err := s.restoreStore.ActiveDirectories(ctx, setID)
	if err != nil {
		return nil, s.db.MediaReadErr("reading the media export's active directories", err)
	}
	return dirs, nil
}

// Placement classifies the media-path placement read (the section doc
// above has the contract; the plain --catalog path keeps its
// pre-existing class).
func (s *mediaStore) Placement(ctx context.Context, setID string) ([]restore.ImagePaths, error) {
	places, err := s.restoreStore.Placement(ctx, setID)
	if err != nil {
		return nil, s.db.MediaReadErr("reading the media export's placement summary", err)
	}
	return places, nil
}

// fillChunkHashes fills every NULL export chunk hash in chunks from man's
// chunk tables - the SAME parent relpath at the SAME catalog 0-based index
// (the manifest's Index IS the catalog 0-based index, media/manifest.go's
// ChunkIndex contract) - and returns how many rows it filled.
//
// PRECEDENCE, in one place for both fill seats: a row already carrying a
// 32-byte hash is never touched (an EXPORT hash always wins, and a value
// an earlier seat set stands), and a manifest part whose hash is absent or
// malformed leaves the row NULL - the engine's honest "the chunk row
// carries no stored hash" per-chunk failure is what an unanchored chunk
// must reach; this helper never synthesizes an anchor.
func fillChunkHashes(chunks []restore.ChunkItem, man media.Manifest) int {
	type chunkKey struct {
		parent string
		index  int64
	}
	parts := make(map[chunkKey]string, len(man.ChunkedFiles))
	for _, cf := range man.ChunkedFiles {
		for _, p := range cf.Chunks {
			parts[chunkKey{cf.RelPath, p.Index}] = p.SHA256
		}
	}
	filled := 0
	for i := range chunks {
		c := &chunks[i]
		if len(c.Hash) == 32 {
			continue // the export anchors this chunk itself (resume-run export)
		}
		hexSum, ok := parts[chunkKey{c.ParentRelPath, int64(c.ChunkIndex)}]
		if !ok {
			continue // neither anchor carries it: the engine fails it honestly
		}
		sum, err := hex.DecodeString(hexSum)
		if err != nil || len(sum) != 32 {
			continue // a malformed manifest hash is not an anchor: the engine fails the chunk
		}
		c.Hash = sum
		filled++
	}
	return filled
}

// openRestoreCatalog opens THE catalog of one restore run and returns the
// read-only handle, the resolved set id, the media info (zero value for
// plain --catalog runs), and a cleanup that removes the export scratch
// (non-nil for media runs only; nil-safe for the caller's defer).
//
// The PLAIN flow is unchanged: catalog.Open(OpenReadOnly) of --catalog,
// the set id from --set-id. The MEDIA flow implements 8.5 in the
// design's own order:
//
//  1. MANIFEST INTROSPECTION FIRST (restore.ResolveMedia): the set id,
//     image list, and whole-set placement are answered from the media's
//     MANIFEST.json WITHOUT opening the export - the quick layer must
//     work even when the export is unreadable (its corruption then
//     fails loudly one step later, at the open).
//  2. --set-id CROSS-CHECK (never a silent override): a mismatch is a
//     usage error naming both ids. 8.5 itself is silent on --set-id; the
//     cross-check reading is documented in the file's flag contract.
//  3. TEMP-COPY the export and open the COPY read-only.
//  4. PRAGMA integrity_check on the copy (8.5: "the export's internal
//     hashes are the deep anchor"; the T5.1 precedent).
//  5. SET ID FROM THE EXPORT: exactly one sets row (the FR-034
//     set-scoped snapshot, 6.6); it must agree with the manifest's set
//     id (a disagreement is mixed or tampered media - the loud
//     media-integrity class), and the manifest's image number must be a
//     planned image of the export's set.
//
// Error classes: *report.UsageError (flag conflicts, cross-check
// mismatch), *restore.InputError / *restore.MediaIntegrityError (media
// path and content; the cli maps the latter to exit 3), and
// *catalog.CatalogError (the export itself: missing, corrupt,
// future-version - exit 4). A read of the export copy that the PRAGMA
// gate passed but the schema cannot answer (a schemaless or foreign
// database) is classified as KindCorrupt by catalog.MediaReadErr, so
// no export failure of this flow leaves as the generic fatal.
func openRestoreCatalog(ctx context.Context, cmd *cobra.Command, opts *rootOptions, ro restoreOptions) (*catalog.DB, string, restore.MediaInfo, func(), error) {
	none := restore.MediaInfo{}
	if ro.fromMedia == "" {
		db, err := catalog.Open(opts.catalog, catalog.OpenReadOnly)
		if err != nil {
			return nil, "", none, nil, err
		}
		return db, opts.setID, none, nil, nil
	}

	// Step 1: the quick manifest layer, BEFORE any export touch.
	info, err := restore.ResolveMedia(ro.fromMedia)
	if err != nil {
		return nil, "", none, nil, err
	}

	// Step 2: the --set-id cross-check.
	if opts.setID != "" && opts.setID != info.SetID {
		return nil, "", info, nil, &report.UsageError{Command: cmd.CommandPath(), Msg: fmt.Sprintf(
			"--set-id %q does not match the media's set %q (from %s); with --from-media, --set-id is a cross-check against the media's own set identity, never an override (DESIGN_SPEC 8.5)",
			opts.setID, info.SetID, filepath.Join(info.ManifestDir, media.ManifestFileName))}
	}
	// The 6.4 naming gate on the DERIVED id (same gate, same point as
	// the explicit-id flow in runRestore).
	if _, err := media.ImageDirName(info.SetID, 1); err != nil {
		return nil, "", info, nil, &report.UsageError{Command: cmd.CommandPath(), Err: err}
	}

	// Steps 3-5 over the temp copy.
	scratch, copyPath, err := mediaExportScratch(info.ExportPath, opts.workDir)
	if err != nil {
		return nil, "", info, nil, err
	}
	discard := func() { _ = os.RemoveAll(scratch) }
	fail := func(err error) (*catalog.DB, string, restore.MediaInfo, func(), error) {
		discard()
		return nil, "", info, nil, err
	}

	db, err := catalog.Open(copyPath, catalog.OpenReadOnly)
	if err != nil {
		return fail(err)
	}
	if err := db.IntegrityCheck(ctx); err != nil {
		_ = db.Close()
		return fail(err)
	}
	ids, err := db.SetIDs(ctx)
	if err != nil {
		_ = db.Close()
		return fail(err)
	}
	if len(ids) != 1 {
		_ = db.Close()
		return fail(&catalog.CatalogError{
			Kind: catalog.KindCorrupt,
			Path: copyPath,
			Err: fmt.Errorf("the export holds %d sets rows, want exactly 1 (a set-scoped FR-034 export, DESIGN_SPEC 6.6/8.5)",
				len(ids)),
		})
	}
	setID := ids[0]
	if setID != info.SetID {
		_ = db.Close()
		return fail(&restore.MediaIntegrityError{
			Path: filepath.Join(info.ManifestDir, media.ManifestFileName),
			Msg: fmt.Sprintf(
				"manifest set id %q disagrees with the export's set id %q (mixed or tampered media: one image ships its manifest and export together, DESIGN_SPEC 6.6)",
				info.SetID, setID),
		})
	}
	planned, err := db.CopyImageIDs(ctx, setID)
	if err != nil {
		_ = db.Close()
		// A media-path read of the export copy that cannot be answered is
		// the catalog class (exit 4), exactly like the integrity gate and
		// the row-count refusals above - see catalog.MediaReadErr.
		return fail(db.MediaReadErr("listing the export's planned image ids", err))
	}
	if !containsInt64(planned, info.Manifest.ImageNumber) {
		_ = db.Close()
		return fail(&restore.MediaIntegrityError{
			Path: filepath.Join(info.ManifestDir, media.ManifestFileName),
			Msg: fmt.Sprintf(
				"manifest claims image %d of set %q, but the export plans images [%s] (mixed or tampered media)",
				info.Manifest.ImageNumber, setID, joinInt64s(planned)),
		})
	}
	return db, setID, info, discard, nil
}

// containsInt64 reports whether ids (ascending) contains v.
func containsInt64(ids []int64, v int64) bool {
	for _, id := range ids {
		if id == v {
			return true
		}
	}
	return false
}

// augmentManifestsWithExplicit extends the media flow's chunk-hash
// records (mediaStore's fill, its type doc has the finding) with the
// MANIFEST.json sitting next to any --image N=PATH DIRECTORY mapping of
// a media run. A chunked parent can span images whose trees are NOT
// inside the --from-media PATH (an extracted-ISO-tree flow: PATH is
// image 1's tree, --image 2= is image 2's tree), and each image's
// manifest is the FR-033 record of THAT image's chunks - so the fill
// consults it too. This is a hash-FILL ONLY, never supply: the mapping
// still resolves through the normal 8.3/8.5 supply pass, and a
// manifest that is absent, unparseable, or belongs to another
// set/image is silently NOT a record (the chunk then fails honestly in
// the engine if the export carries no hash either) - an explicit
// mapping's payload is verified per member, so its sidecar manifest is
// consulted opportunistically, never trusted fatally. The supply-side
// seat (sidecarManifestRestorer) consults the same directory again
// after resolution, so the fill follows the supply whichever way this
// pre-supply record went.
func augmentManifestsWithExplicit(info *restore.MediaInfo, explicit []restore.ExplicitImage) {
	for _, e := range explicit {
		if _, ok := info.Manifests[e.Image]; ok {
			continue // the media itself already carries this image's record
		}
		man, ok := readSidecarManifest(e.Path, info.SetID, e.Image)
		if !ok {
			continue // no usable sidecar manifest: no fill possible for this image
		}
		info.Manifests[e.Image] = man
	}
}

// readSidecarManifest reads <dir>/MANIFEST.json and answers it ONLY when
// it parses AND identity-checks against the run's set id and image number
// - THE opportunistic sidecar-manifest read of the media flow, shared by
// the pre-supply augment and by the supply-following fill below.
//
// It is never fatal and never trusts: absent, unreadable, unparseable,
// foreign (another set), or stale (another image number) all answer
// false, and the caller simply records no anchor - the chunk then fails
// honestly in the engine if the export carries no hash either. The
// identity check is what makes a supplement's sidecar admissible: the
// supply resolved this DIRECTORY as image N of THIS set, so a manifest
// that claims another set or another image is not this image's record.
func readSidecarManifest(dir, setID string, image int64) (media.Manifest, bool) {
	blob, err := os.ReadFile(filepath.Join(dir, media.ManifestFileName))
	if err != nil {
		return media.Manifest{}, false
	}
	man, err := media.ParseManifest(blob)
	if err != nil || man.SetID != setID || man.ImageNumber != image {
		return media.Manifest{}, false
	}
	return man, true
}

// --- the supply-following fill seat ------------------------------------------

// sidecarManifestRestorer is the SECOND seat of the media flow's chunk-hash
// fill (mediaStore's type doc has the finding and the seat map): it runs
// per image, AFTER restore.Run's supply pass resolved that image's source,
// and consults the sidecar MANIFEST.json of WHATEVER SourceCopyTree
// directory the supply actually resolved - the --from-media base tree, an
// --image N=DIR mapping, a <copy-tree-root>/<SET-ID>-NNN supplement, or a
// path the 8.3 prompt supplied.
//
// WHY A SEAT AT THIS SEAM: the inventory (and with it mediaStore's fill)
// is built during selection resolution, which happens BEFORE supply is
// resolved - so a fill that must FOLLOW the supply can only run where both
// are known, which is exactly this seam: restore.Run hands the resolved
// ImageSupply and the selection to ImageRestorer together. The fill here
// is the same NULL-only, identity-checked, opportunistic fill: it reads one
// sidecar per copy-tree image, touches only rows with no hash at all (an
// export hash, or a value the pre-supply seat already set, always wins),
// and never fails the run - an ISO supply is NOT consulted at all, because
// an .iso file carries no READABLE sidecar manifest (see the --from-media
// help text for what that shape can and cannot verify).
type sidecarManifestRestorer struct {
	inner restore.ImageRestorer
}

// newSidecarManifestRestorer wraps inner with the supply-following fill of
// the media flow (runRestore wires it for --from-media runs only).
func newSidecarManifestRestorer(inner restore.ImageRestorer) restore.ImageRestorer {
	return &sidecarManifestRestorer{inner: inner}
}

// RestoreImage implements restore.ImageRestorer: fill from the resolved
// copy-tree directory's sidecar, then delegate unchanged.
func (r *sidecarManifestRestorer) RestoreImage(ctx context.Context, sup restore.ImageSupply, sel restore.ResolvedSelection, workspace string) (restore.ImageRestoreResult, error) {
	if sup.Kind == restore.SourceCopyTree {
		sel = fillSelectionFromSidecar(sel, sup)
	}
	return r.inner.RestoreImage(ctx, sup, sel, workspace)
}

// fillSelectionFromSidecar returns sel with the chunks of sup.Image filled
// from <sup.Path>/MANIFEST.json when that sidecar exists and identity-
// checks; otherwise sel is returned untouched (same slice, no work).
//
// The chunk rows are COPIED before the fill: the selection belongs to
// restore.Run (it is also reported from there), so this seat must never
// mutate the caller's data through a value copy - only the selection handed
// to the inner restorer carries the filled rows.
func fillSelectionFromSidecar(sel restore.ResolvedSelection, sup restore.ImageSupply) restore.ResolvedSelection {
	man, ok := readSidecarManifest(sup.Path, sup.SetID, sup.Image)
	if !ok {
		return sel // no usable sidecar in the resolved directory: no fill possible
	}
	idx := -1
	for i := range sel.Work {
		if sel.Work[i].Image == sup.Image {
			idx = i
			break
		}
	}
	if idx < 0 {
		return sel // this image contributes nothing to the selection: nothing to fill
	}
	chunks := append([]restore.ChunkItem(nil), sel.Work[idx].Inv.Chunks...)
	if fillChunkHashes(chunks, man) == 0 {
		return sel // every row is already anchored (export or pre-supply seat)
	}
	work := append([]restore.ImageWork(nil), sel.Work...)
	work[idx].Inv.Chunks = chunks
	sel.Work = work
	return sel
}

// mediaExportScratch copies the media's catalog-export.sqlite into a
// UNIQUE scratch directory under --work-dir ("" = the OS temp
// directory, the same convention as the restore workspace) and returns
// the scratch dir plus the copy's path.
//
// THE TEMP-COPY RATIONALE (8.5's own words - "copies the export to a
// temp location and opens it read-only" - plus the T5.2 lesson): opening
// the MEDIA's export directly with mode=ro can still materialize
// -wal/-shm sidecars ON THE MEDIA (SQLite read-only connections may
// create the shared-memory sidecar, and a WAL db may be recoverable
// read-only). Copying first and opening the COPY read-only means NOTHING
// is ever written to the media - pinned by the media byte-identical +
// zero-sidecar test assertions. The scratch is unique per run
// (MkdirTemp), lives under --work-dir when one is configured, and the
// caller removes the WHOLE directory on every exit path.
func mediaExportScratch(exportPath, workDir string) (scratchDir, copyPath string, err error) {
	dir, err := os.MkdirTemp(workDir, "isopress-restore-export-")
	if err != nil {
		return "", "", fmt.Errorf("restore: creating the export scratch directory under %q: %w", workDir, err)
	}
	src, err := os.Open(exportPath)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", "", &catalog.CatalogError{
			Kind: catalog.KindOpen,
			Path: exportPath,
			Err: fmt.Errorf("media export not found or unreadable (DESIGN_SPEC 8.5: the media carries %s next to MANIFEST.json): %w",
				media.ExportFileName, err),
		}
	}
	defer src.Close() //nolint:errcheck // best-effort read-only close

	dstPath := filepath.Join(dir, media.ExportFileName)
	dst, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", "", fmt.Errorf("restore: creating the export copy %s: %w", dstPath, err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		_ = os.RemoveAll(dir)
		return "", "", fmt.Errorf("restore: copying the media export %s to the scratch: %w", exportPath, err)
	}
	if err := dst.Close(); err != nil {
		_ = os.RemoveAll(dir)
		return "", "", fmt.Errorf("restore: closing the export copy %s: %w", dstPath, err)
	}
	return dir, dstPath, nil
}

// verifyMediaManifestAnchor performs 8.5's manifest-integrity step over
// the OPEN export copy. THE CHAIN, precisely as the data actually
// supports it (no overclaim):
//
//   - image_contents carries a 'manifest' self-row per written image
//     (6.6; the writer records it at write time because the manifest
//     cannot list its own hash - media/manifest.go's documented
//     self-entry reading). Its hash attests the manifest as of the
//     EXPORT'S snapshot.
//   - HASH MATCH: the media's MANIFEST.json is anchored to the export -
//     verified, silently (the normal success path carries no noise).
//   - SELF-ROW ABSENT: the NORMAL state of first-run media - copy and
//     build snapshot the run's export BEFORE the image loop writes the
//     manifest and its self-row, so a single-run export legitimately
//     carries no self-rows. The export's own integrity then rests on
//     PRAGMA integrity_check (openRestoreCatalog ran it on the copy)
//     plus the schema gate - the honest fallback anchor, no warning
//     noise for the common case.
//   - HASH MISMATCH: LOUD (a stderr warning naming the manifest and
//     both hashes) but NOT fatal. It is the EXPECTED state of RESUMED
//     media - a re-run copy/build re-embeds its RUN-START export next
//     to a freshly rewritten manifest, so the embedded self-row attests
//     the PREVIOUS manifest - and it is equally the signature of
//     tampering. The tool cannot distinguish the two from the media
//     alone, so it says so and lets the per-member hash verification
//     (FR-041's double gate, already inside the engine) remain the
//     byte-level arbiter. This is the honest extent of the media-alone
//     chain: the export can POSITIVELY verify the manifest; it can
//     never positively condemn it.
func verifyMediaManifestAnchor(ctx context.Context, db *catalog.DB, info restore.MediaInfo, bus *progress.Bus) error {
	manifestPath := filepath.Join(info.ManifestDir, media.ManifestFileName)
	contents, err := db.ImageContentList(ctx, info.SetID, info.Manifest.ImageNumber)
	if err != nil {
		return &catalog.CatalogError{
			Kind: catalog.KindCorrupt,
			Path: db.Path(),
			Err:  fmt.Errorf("reading the export's content list for the manifest anchor: %w", err),
		}
	}
	var recorded []byte
	for _, row := range contents.Rows {
		if row.Kind == "manifest" && row.MediaPath == media.ManifestFileName {
			recorded = row.Hash
			break
		}
	}
	if recorded == nil {
		return nil // absent self-row: documented above - export integrity is the anchor
	}
	blob, err := os.ReadFile(manifestPath)
	if err != nil {
		return &restore.MediaIntegrityError{Path: manifestPath, Msg: fmt.Sprintf("re-reading MANIFEST.json for the anchor check: %v", err)}
	}
	sum := sha256.Sum256(blob)
	if bytes.Equal(sum[:], recorded) {
		return nil // anchor VERIFIED
	}
	if bus != nil {
		bus.Emit(progress.Warn{Msg: fmt.Sprintf(
			"%s sha256 %x does not match the export's recorded manifest self-row %x (set %s image %d): "+
				"either the manifest was rewritten after the export was embedded (a resumed copy/build run does exactly that - "+
				"the run-start export predates the freshly written manifest) or the media was tampered with; "+
				"the run proceeds and the per-member hash verification (FR-041) remains the byte-level anchor - "+
				"run 'isopress verify-media' or re-copy the set to resolve the ambiguity",
			manifestPath, sum[:], recorded, info.SetID, info.Manifest.ImageNumber)})
	}
	return nil
}
