package restore

// T5.4: the per-image content restoration - the real ImageRestorer behind the
// T5.3 seam (DESIGN_SPEC 8.4 extraction and chunked reassembly with double
// verification; 5.4 metadata after content; 5.1 write discipline;
// IMPLEMENTATION_PLAN 4.6 T5.4).
//
// # Source dispatch (the seam's ImageSupply.Kind)
//
//   - SourceCopyTree: the tree IS the source root - this package reads
//     <tree>/files/<relpath> and <tree>/chunks/<relpath>/NNNNN directly, no
//     xorriso (verify-media's copy-tree precedent). The path must stat as a
//     directory or the call fails as an IMAGE-level failure (the loop
//     continues over the remaining images, 8.4/FR-041).
//   - SourceISO: ONE xorriso osirrox invocation through the T4.1
//     *xorriso.Tool extracts the whole image into a UNIQUE SUBDIRECTORY
//     INSIDE the run's workspace (-indev <iso> -osirrox on -extract /
//     <workspace>/extract-NNN-XXXXXX - the T4.3/T5.1 single `-extract /`
//     precedent: the /files+/chunks pair hard-fails on chunkless images and
//     misses the image-root members), after the 7.2 free-space pre-check
//     (Statfs seam, verify-media's pattern). Every payload byte then flows
//     from the EXTRACTION to the target.
//
// The extraction dir lives inside the workspace on purpose: the ORCHESTRATION
// wipes the workspace BETWEEN images (8.3), so per-image scratch must be
// workspace-resident to be cleaned - and, symmetrically, NOTHING this engine
// needs to survive may live there. The ONLY durable outputs of a restore are
// the target's final files and the target's growing `<relpath>.partial`
// assemblies (see the cross-image section below); a restoration write never
// depends on the workspace surviving past its own RestoreImage call.
//
// # The target write layout (8.4, 5.1)
//
// Every payload member is written through the 5.1 discipline at the path the
// brief fixes: `<target>/<relpath>.partial` (mkdir -p of the parent on the
// way, T1.2/T3.3 pattern), hashed WHILE reading, fsync, atomic rename to
// `<target>/<relpath>`, fsync of the containing directory where practical,
// and only THEN the 5.4 metadata pass (fsmeta.Apply) on the final path.
// DESIGN_SPEC 8.4's prose spells the partial as `<target>/files/<relpath>
// .partial` (a slip that reuses the media tree's files/ root); the operative
// contract - brief, T5.3 seam doc, and IMPLEMENTATION_PLAN T5.4's mechanics
// line - is `<target>/<relpath>.partial` -> `<target>/<relpath>`, so restore
// output IS the original tree under --target and a round-trip comparison is
// a plain tree-to-tree diff (documented reading of the spec's shorthand).
//
// The whole-file verification IS the hash-while-reading comparison against
// the catalog whole-file hash (8.4: no separate read-back for whole files);
// the chunked path gets its second, read-back verification at completion.
// On a whole-file mismatch the partial is DELETED (8.4's explicit rule) and
// the failure recorded; per-file failures never abort the run.
//
// # The cross-image assembly (NB-3) and the count-once contract (NB-04)
//
// A chunked parent can span several images, and the shared workspace is WIPED
// between those RestoreImage calls - so the growing assembly CANNOT live in
// the workspace. It lives in the TARGET as `<target>/<relpath>.partial`,
// which survives every wipe: image 1 creates it (first chunk of this run
// opens it with O_TRUNC - which is also the honest crash-recovery semantics
// of the next RUN, whose first chunk discards any stale assembly from an
// aborted run), later images OPEN IT WITHOUT TRUNCATION and write their
// chunks at their recorded offsets. The run tracks which chunk indices it
// has written per parent in memory (this package's per-instance state - one
// restorer instance == one run, the factory contract); when every index in
// [0, ParentNumChunks) has landed, the SECOND verification reads the
// assembled file back and hashes it against the parent's whole-file catalog
// hash (the read-back is the authority: a file whose bytes are wrong - a
// hole, a stale tail, media lying after the per-chunk gates passed - can
// never pass it), then rename + metadata. The in-memory index set decides
// WHEN to verify; the assembled hash decides WHETHER it passes.
//
// COUNT CONTRACT (pinned): a spanning member is counted ONCE - at its
// COMPLETION image. The images before completion record NOTHING for it (no
// Restored, no Failed, no Files entry): a mid-assembly member is neither
// restored nor failed, and inventing a skipped/failed row at image N would
// turn a healthy span into an exit-10 "partial" run. A parent that FAILS
// (missing/corrupt chunk, assembled-hash mismatch) is recorded FAILED exactly
// ONCE, at the detecting image; later images holding more of its chunks skip
// it silently (its assembly is abandoned) - never a second failure row for
// the same member, never a double count. Mid-assembly bytes this image
// appended still count in ImageRestoreResult.Bytes (they are bytes this call
// wrote into the target) and in the phase's ItemDone (the phase denominators
// counted them), so per-image progress and byte totals stay honest while the
// member's COUNT lands at completion.
//
// # The .partial leave-vs-delete decision (documented, pinned by tests)
//
//   - Whole file, verification failed: DELETE the partial (8.4's explicit
//     "on mismatch, delete the partial" + the copyengine 5.1 precedent).
//   - Chunked member, verification failed, and the .partial was CREATED BY
//     THE FAILING CALL: DELETE it. It holds only this call's unverified
//     bytes; nothing durable is lost, and the copyengine "unmarked partials
//     are removed" precedent applies unchanged.
//   - Chunked member, verification failed, and the .partial already held
//     EARLIER IMAGES' VERIFIED chunks of this run: LEAVE IT. Deleting would
//     destroy verified work from other media for no trust benefit, because
//     the trust model lives on FINAL names only: an unmarked .partial is
//     never a restored file, the report has already declared the member
//     incomplete (no final file appears), and the next run truncates the
//     assembly on its first chunk and re-verifies from scratch.
//   - Cancellation: the in-flight partial is always left in place (the
//     interrupted run's own crash-recovery state; see the writePayload
//     abort path) - a cancelled run never deletes or renames around a
//     half-streamed member.
//
// The outcome detail names which branch fired, so a report line is never
// ambiguous about what remains on disk.
//
// # The other member kinds (5.4, D-03, D-04, FR-037)
//
//   - Symlinks: os.Symlink with the RECORDED target, never followed, dangling
//     targets carried verbatim, read-back target equality verified; an
//     existing destination is NEVER clobbered - EEXIST tolerates only an
//     existing symlink with the same recorded target (idempotent re-run,
//     copyengine's present-but-unmarked rule), anything else is a failure.
//   - Empty directories: mkdir (EEXIST-tolerated, idempotent - another image
//     or a prior run may have created it) then the metadata pass.
//   - Hard-link families: the content is restored ONCE. When the family
//     PRIMARY is in this run's selection it is an ordinary member of the
//     Work inventory and gets the bytes at its own name; every REQUESTED
//     secondary then becomes link(2) to the primary's final path with a
//     SameFile identity read-back (no separate metadata - links share the
//     inode state the primary's Apply already wrote), and a secondary whose
//     primary FAILED records a "cannot link" failure with no bytes written.
//     When the primary is NOT selected (T5.3's partial-family rule: the Work
//     then carries ONLY Secondaries{RelPath, PrimaryRelPath, Hash}), the
//     bytes are read from the primary's stored name (files/<PrimaryRelPath>)
//     but written at the FIRST requested secondary's name, and the remaining
//     requested names link(2) to it - so the target gains exactly the
//     selected names, sharing one inode, content written once (8.2's "links
//     for the requested names"; the unselected primary never appears).
//   - Metadata (5.4, AFTER content): fsmeta.Apply consumes Go semantics, so
//     the catalog's RAW POSIX mode goes through fsmeta.GoMode at this single
//     apply site (the T3.4 lesson: handing a raw 0o4755 to os.Chmod silently
//     drops setuid). EPERM chown is the 5.4 WARNING (recorded in the outcome
//     as a restored member's Detail, emitted as progress.Warn, never a
//     failure and never exit-10 material); any other metadata failure after
//     verified content is reported as a per-file WARNING the same way (5.4:
//     "a metadata failure never leaves verified content marked unverified; it
//     is reported as a per-file warning").
//
// # The target partial-collision gate (the B1 mirror)
//
// cli/copy.go's ratified B1 gate refuses, PER IMAGE, a media item whose
// derived `<final>.partial` equals another member's final path (the real
// repro'd `a` / `a.partial` pair). Because those gates are per-image, a pair
// split across two images can both reach media - and at restore time the
// stale-partial removal for `a` would silently DELETE the completed
// `<target>/a.partial` of its sibling. This engine therefore applies the
// same structural check over the run's WHOLE selection (selection.Paths, the
// only names this run may create): an item whose derived partial is another
// SELECTED member's final is refused BEFORE any write (an honest failure
// naming both paths), while the sibling proceeds. Selection-wide, so it
// closes the cross-image hole the copy gate structurally cannot see.
//
// # Progress and cancellation
//
// One ItemDone per distinct member of this image's work list, inside the
// "restore" phase T5.3 declares (the workTotals denominators: a chunked
// parent contributes this image's chunk bytes once), progress.Error per
// failed member, progress.Warn per warning - all nil-safe (a nil Bus means
// no events). A long xorriso extraction carries no intra-member events; the
// cli heartbeat (startHeartbeat) keeps that span alive (9.1). ctx is honored
// at every member boundary and inside every stream copy (ctxReader): a
// cancelled run stops without writing another byte, renames nothing, returns
// ctx.Err(), and touches no catalog state - this package never imports
// internal/catalog, so a restore run is structurally incapable of a catalog
// mutation (the read-only policy, doc.go).
//
// Concurrency: one instance serves restore.Run's SEQUENTIAL loop (8.3), and
// the cross-image assembly state makes that a contract - concurrent
// RestoreImage calls are not supported.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"isopress/internal/fsmeta"
	"isopress/internal/media"
	"isopress/internal/progress"
)

const (
	// partialSuffix is the 5.1 "<final>.partial" suffix. It is deliberately
	// re-declared here instead of importing internal/copyengine: the copy
	// engine is the WRITE-side mark engine (its own Store seam, retries,
	// crash bookkeeping), restore is a read-only-media service, and the only
	// thing they share is this one string derivation - importing the engine
	// would invert the layering for a constant (cross-reference:
	// copyengine.PartialPath).
	partialSuffix = ".partial"

	// extractSlack is the free-space cushion of the 7.2 pre-check (the
	// verify-media extractSlack value and rationale: directory entries and
	// xorriso scratch above the payload).
	extractSlack = 16 << 20 // 16 MiB

	// extractPrefix is the os.MkdirTemp pattern of the PER-IMAGE extraction
	// directory inside the run's workspace (unique per call; the
	// orchestration's between-images clean removes it).
	extractPrefix = "extract-"
)

// The FileOutcome.Status vocabulary T5.4 uses (types.go): a member that came
// back fully restored is "restored" (counted, listed only when it carries a
// warning Detail), an ordinary failure is "failed", and the chunked 8.4
// member whose assembly could not be completed is "incomplete" - the
// design's own word (8.4: "marks only that file `incomplete` in the restore
// report"), carrying the missing/corrupt chunk indices of FR-041.
const statusIncomplete = "incomplete"

// statusFailed is the ordinary FileOutcome failure status (the incomplete
// class of the chunked path uses statusIncomplete).
const statusFailed = "failed"

// NewImageRestorer returns the T5.4 ImageRestorer. ONE instance is built per
// run (the cli's restoreRestorerFactory calls it once per command
// invocation): the instance owns the cross-image assembly state, so the
// one-instance-per-run contract is what keeps that state scoped to exactly
// one run over exactly one target.
func NewImageRestorer(deps RestorerDeps) ImageRestorer {
	return &imageRestorer{
		deps:       deps,
		assemblies: map[string]*assembly{},
	}
}

// imageRestorer implements ImageRestorer (see the file doc).
type imageRestorer struct {
	deps RestorerDeps
	// assemblies is the run-scoped cross-image chunk state (NB-03/NB-04),
	// keyed by target+relpath so even a misuse with two targets cannot mix
	// two growing files.
	assemblies map[string]*assembly
}

// assembly is ONE chunked parent's state across RestoreImage calls of this
// run. have[] is the honest cross-image chunk counter (the .partial itself
// cannot say which indices it holds); failed/complete enforce the count-once
// contract (NB-04) on later images.
type assembly struct {
	started   bool         // this run created/wrote the .partial
	have      map[int]bool // chunk indices written and first-verification-passed this run
	numChunks int          // ParentNumChunks (max over rows seen; rows of one parent agree)
	failed    bool         // abandoned: failure recorded once at the detecting image
	complete  bool         // renamed to final: counted once at the completion image
}

// assemblyFor returns (creating on first use) the state of one parent.
func (r *imageRestorer) assemblyFor(target, rel string) *assembly {
	key := target + "\x00" + rel
	st, ok := r.assemblies[key]
	if !ok {
		st = &assembly{}
		r.assemblies[key] = st
	}
	return st
}

// covered reports whether every chunk index of the parent has landed this
// run - the completion trigger for the second verification.
func (st *assembly) covered() bool {
	if st.numChunks <= 0 {
		return false
	}
	for i := 0; i < st.numChunks; i++ {
		if !st.have[i] {
			return false
		}
	}
	return true
}

// --- the seam implementation -------------------------------------------------

// RestoreImage implements ImageRestorer (the contract in doc.go/types.go).
// It returns an error ONLY for image-level faults (source dispatch failure,
// cancellation): per-member problems are data, recorded in the returned
// ImageRestoreResult so the loop continues over the remaining images.
func (r *imageRestorer) RestoreImage(ctx context.Context, image ImageSupply, selection ResolvedSelection, workspace string) (ImageRestoreResult, error) {
	res := ImageRestoreResult{Image: image.Image}
	switch {
	case selection.Target == "":
		return res, fmt.Errorf("restore: image %d: the selection carries no --target (wiring bug: Run sets Target before the loop)", image.Image)
	case workspace == "":
		return res, fmt.Errorf("restore: image %d: no workspace (wiring bug: Run creates one temp workspace for the whole loop)", image.Image)
	}
	work, ok := selection.WorkFor(image.Image)
	if !ok {
		return res, fmt.Errorf("restore: image %d: no work list in this selection (wiring bug: Run iterates selection.Work)", image.Image)
	}
	if !hasMembers(work.Inv) {
		return res, nil // defensive: this image contributes nothing
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}

	src, err := r.sourceRoot(ctx, image, workspace)
	if err != nil {
		return res, err // image-level failure: recorded per image, loop continues
	}

	c := &imageCall{
		restorer:        r,
		ctx:             ctx,
		src:             src,
		target:          selection.Target,
		work:            work,
		res:             &res,
		blocked:         partialCollisionGate(selection.Paths),
		primarySelected: make(map[string]struct{}, len(work.Inv.Files)),
		restoredPrimary: make(map[string]bool, len(work.Inv.Files)),
		families:        map[string]*familyState{},
	}
	for _, f := range work.Inv.Files {
		c.primarySelected[f.RelPath] = struct{}{}
	}
	if err := c.run(); err != nil {
		return res, err // only cancellation reaches here (see run)
	}
	return res, nil
}

// familyState is one hard-link family's content anchor inside this call:
// path is the target path carrying the bytes for later link(2)s ("" until a
// content write succeeds), err is the failure detail to repeat for the
// family's remaining requested names (never a second source read).
type familyState struct {
	path string
	err  string
}

// hasMembers reports whether the filtered inventory has anything to restore.
func hasMembers(inv Inventory) bool {
	return len(inv.Files)+len(inv.Symlinks)+len(inv.Chunks)+len(inv.Dirs)+len(inv.Secondaries) > 0
}

// sourceRoot resolves where this image's bytes are read FROM: the copy tree
// itself, or - for an ISO - a fresh extraction directory inside the
// workspace (see the file doc's dispatch section).
func (r *imageRestorer) sourceRoot(ctx context.Context, image ImageSupply, workspace string) (string, error) {
	switch image.Kind {
	case SourceCopyTree:
		fi, err := os.Stat(image.Path)
		switch {
		case err != nil:
			return "", fmt.Errorf("restore: image %d copy tree %s: %w", image.Image, image.Path, err)
		case !fi.IsDir():
			return "", fmt.Errorf("restore: image %d: %s is not a directory (a copy tree must be one)", image.Image, image.Path)
		}
		return image.Path, nil
	case SourceISO:
		return r.extractISO(ctx, image, workspace)
	default:
		return "", fmt.Errorf("restore: image %d has unknown source kind %q (the supply classifies a file as %q and a directory as %q)",
			image.Image, image.Kind, SourceISO, SourceCopyTree)
	}
}

// extractISO performs the ONE xorriso osirrox extraction of this image into a
// unique subdirectory of the workspace (T4.1 Tool seam - this package never
// execs itself - over the T4.3/T5.1 single `-extract /` argv shape), after
// the 7.2 free-space pre-check. The directory is left in place on return: the
// orchestration cleans the workspace between images and removes it on every
// exit path, and keeping the extraction until then costs nothing while a
// failing run keeps it diagnosable.
func (r *imageRestorer) extractISO(ctx context.Context, image ImageSupply, workspace string) (string, error) {
	if r.deps.Tool == nil || r.deps.Tool.Path == "" {
		return "", fmt.Errorf("restore: image %d is an ISO but no xorriso tool is configured (wiring bug: the T4.1 probe runs before the loop)", image.Image)
	}
	if err := checkFreeSpace(r.deps.FreeSpace, workspace, image.Path); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(workspace, fmt.Sprintf("%s%d-", extractPrefix, image.Image))
	if err != nil {
		return "", fmt.Errorf("restore: creating the extraction directory inside the workspace %s: %w", workspace, err)
	}
	argv := []string{"-indev", image.Path, "-osirrox", "on", "-extract", "/", dir}
	if _, err := r.deps.Tool.Run(ctx, argv); err != nil {
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("restore: xorriso extraction of image %d (%s): %w", image.Image, image.Path, err)
	}
	return dir, nil
}

// checkFreeSpace is the 7.2 extraction pre-check over the Statfs seam
// (verify-media's pattern; default statfsFreeSpace below). The required
// figure is the ISO FILE size: an ISO is payload plus sector/rock-ridge
// overhead and trailing padding, so its own size is a safe upper bound on
// what the extraction will write under the workspace. An unreadable image
// stat is left to xorriso, which reports it with its own words.
func checkFreeSpace(free FreeSpaceFunc, workspace, imagePath string) error {
	if free == nil {
		free = statfsFreeSpace
	}
	avail, err := free(workspace)
	if err != nil {
		return fmt.Errorf("restore: free-space pre-check of the workspace %s: %w", workspace, err)
	}
	fi, err := os.Stat(imagePath)
	if err != nil {
		return nil // xorriso will refuse the unreadable image honestly
	}
	var required uint64
	if fi.Size() > 0 {
		required = uint64(fi.Size()) + extractSlack
	}
	if required > avail {
		return fmt.Errorf("restore: extracting %s needs about %d bytes free under %q, %d available (DESIGN_SPEC 7.2 free-space pre-check); "+
			"choose a --work-dir with more space or free the filesystem",
			imagePath, required, workspace, avail)
	}
	return nil
}

// statfsFreeSpace is the production FreeSpaceFunc (stdlib syscall, the
// imagebuild/verify precedent - no x/sys per the DESIGN_SPEC 11 gate).
func statfsFreeSpace(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %q: %w", path, err)
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// --- one image's restoration pass --------------------------------------------

// imageCall is ONE RestoreImage pass (its context, source root, target, and
// the per-family/per-parent state that does not outlive the call - the
// cross-image parent state lives on imageRestorer, see assemblyFor).
type imageCall struct {
	restorer *imageRestorer
	ctx      context.Context
	src      string // source root: copy tree dir or this image's extraction dir
	target   string
	work     ImageWork
	res      *ImageRestoreResult

	blocked         map[string]string       // B1-mirror gate: rel -> the sibling rel whose final it would clobber
	primarySelected map[string]struct{}     // names that are family primaries IN THIS WORK
	restoredPrimary map[string]bool         // primary rel -> whether its content restore succeeded here
	families        map[string]*familyState // family primary rel -> content anchor for link(2)
}

// run processes this image's members in dependency order: whole files first
// (a family primary's bytes must exist before its secondaries link), then the
// chunked parents (the cross-image assembly), then symlinks, empty dirs, and
// finally the hard-link secondaries. It returns non-nil ONLY on
// cancellation - per-member problems are recorded and the pass continues
// (8.4: per-file failures never abort the run).
func (c *imageCall) run() error {
	// A parent that appears BOTH as a whole file and as chunks (the
	// defensive files.image_id edge) is restored through its chunks only -
	// the same "counted through its chunks" rule workTotals applies, so
	// bytes are never written or counted twice.
	chunkParents := make(map[string]struct{}, len(c.work.Inv.Chunks))
	for _, ch := range c.work.Inv.Chunks {
		chunkParents[ch.ParentRelPath] = struct{}{}
	}

	for _, f := range c.work.Inv.Files {
		if _, viaChunks := chunkParents[f.RelPath]; viaChunks {
			continue
		}
		c.wholeFile(f)
		if err := c.ctx.Err(); err != nil {
			return err
		}
	}
	for _, group := range c.chunkGroups() {
		c.chunkParent(group)
		if err := c.ctx.Err(); err != nil {
			return err
		}
	}
	for _, s := range c.work.Inv.Symlinks {
		c.symlink(s)
		if err := c.ctx.Err(); err != nil {
			return err
		}
	}
	for _, d := range c.work.Inv.Dirs {
		c.emptyDir(d)
		if err := c.ctx.Err(); err != nil {
			return err
		}
	}
	for _, l := range c.work.Inv.Secondaries {
		c.secondary(l)
		if err := c.ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

// chunkGroups partitions this image's chunk rows by parent, each group sorted
// by ascending chunk index (8.4: "extract each chunk in index order"), with
// parents in ascending relpath order for a deterministic pass.
func (c *imageCall) chunkGroups() [][]ChunkItem {
	groups := map[string][]ChunkItem{}
	var order []string
	for _, ch := range c.work.Inv.Chunks {
		if _, ok := groups[ch.ParentRelPath]; !ok {
			order = append(order, ch.ParentRelPath)
		}
		groups[ch.ParentRelPath] = append(groups[ch.ParentRelPath], ch)
	}
	sort.Strings(order)
	out := make([][]ChunkItem, 0, len(order))
	for _, rel := range order {
		g := groups[rel]
		sort.Slice(g, func(i, j int) bool { return g[i].ChunkIndex < g[j].ChunkIndex })
		out = append(out, g)
	}
	return out
}

// --- whole files (8.4) --------------------------------------------------------

// payload is ONE regular-file content write: where the bytes go (dstRel), the
// catalog-anchored size/hash they must match, the 5.4 metadata to apply after
// the rename, and - for a hard-link secondary carrying its family's bytes -
// where the bytes are read FROM when that differs from dstRel.
type payload struct {
	dstRel  string
	srcRel  string // relpath under the source files/ root ("" = dstRel)
	size    int64
	hash    []byte
	mode    uint32 // RAW POSIX (the catalog column shape); converted via fsmeta.GoMode at the apply site
	uid     uint32
	gid     uint32
	mtimeNS int64
}

// payloadState is writePayload's outcome.
type payloadState int

const (
	payloadRestored payloadState = iota
	payloadFailed
	payloadAborted // context cancelled mid-stream: no outcome, no progress
)

// wholeFile restores one whole regular file (or refuses it honestly).
func (c *imageCall) wholeFile(f FileItem) {
	state, detail, warning := c.writePayload(payload{
		dstRel: f.RelPath, size: f.Size, hash: f.Hash,
		mode: f.Mode, uid: f.UID, gid: f.GID, mtimeNS: f.MtimeNS,
	})
	switch state {
	case payloadRestored:
		c.restoredPrimary[f.RelPath] = true
		c.recordRestored(f.RelPath, f.Size, warning)
		c.itemDone(f.RelPath, f.Size)
	case payloadFailed:
		c.restoredPrimary[f.RelPath] = false
		c.recordFailed(f.RelPath, statusFailed, detail, f.Size)
		c.itemDone(f.RelPath, f.Size)
	default: // payloadAborted: the run is ending; nothing is counted or rendered
	}
}

// writePayload runs the 5.1/8.4 whole-file discipline for p (see the file
// doc): derive the partial, remove any stale one (5.1 step 2a), stream the
// source through SHA-256 into it (hash WHILE reading), compare against the
// catalog anchor, fsync, rename, fsync the directory, then the 5.4 metadata.
// On any verification failure the partial is deleted (8.4) and the detail
// says what mismatched; on cancellation the partial is abandoned in place.
func (c *imageCall) writePayload(p payload) (payloadState, string, string) {
	dstRel := p.dstRel
	srcRel := p.srcRel
	if srcRel == "" {
		srcRel = dstRel
	}
	final, err := targetPath(c.target, dstRel)
	if err != nil {
		return payloadFailed, fmt.Sprintf("unsafe relpath: %v", err), ""
	}
	if sibling, blocked := c.blocked[dstRel]; blocked {
		return payloadFailed, fmt.Sprintf(
			"partial-path collision: writing %q derives the partial path %s, which is the FINAL path of the selected member %q; "+
				"refusing before any write (the B1 collision rule of the copy pipeline, applied over the whole selection)",
			dstRel, final+partialSuffix, sibling), ""
	}
	partial := final + partialSuffix
	// Parent dirs on demand (the T1.2/T3.3 pattern); 0755 is the tree
	// default - only RECORDED empty dirs ever get their mode applied (5.4).
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return payloadFailed, fmt.Sprintf("creating the destination directory: %v", err), ""
	}
	// 5.1 step 2a crash recovery (the copyengine.attempt precedent): only
	// OUR derived path is removed, never a user file.
	if err := os.Remove(partial); err != nil && !os.IsNotExist(err) {
		return payloadFailed, fmt.Sprintf("removing the stale partial %s: %v", partial, err), ""
	}

	mediaPath, err := media.FileMediaPath(srcRel)
	if err != nil {
		return payloadFailed, fmt.Sprintf("unsafe source relpath: %v", err), ""
	}
	src, err := os.Open(filepath.Join(c.src, filepath.FromSlash(mediaPath)))
	if err != nil {
		return payloadFailed, fmt.Sprintf("reading the source payload files/%s: %v", srcRel, err), ""
	}
	defer src.Close() //nolint:errcheck // read-only close; the write result dominates

	out, err := os.OpenFile(partial, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return payloadFailed, fmt.Sprintf("creating the partial %s: %v", partial, err), ""
	}

	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), &ctxReader{ctx: c.ctx, r: src})
	if err != nil {
		_ = out.Close()
		if c.ctx.Err() != nil {
			// Cancellation mid-stream: the partial is ABANDONED in place
			// (documented trust model: an unmarked .partial is never a
			// final file; the next run's step-2a removes it before
			// rewriting). No rename, no outcome, no progress event.
			return payloadAborted, "", ""
		}
		_ = os.Remove(partial)
		return payloadFailed, fmt.Sprintf("streaming the payload: %v", err), ""
	}
	if n != p.size {
		_ = out.Close()
		_ = os.Remove(partial)
		return payloadFailed, fmt.Sprintf("length mismatch (8.4): restored %d bytes, the record says %d", n, p.size), ""
	}
	if len(p.hash) == 0 {
		_ = out.Close()
		_ = os.Remove(partial)
		return payloadFailed, "the record carries no stored whole-file hash, so the 8.4 verification cannot run", ""
	}
	if sum := h.Sum(nil); !bytes.Equal(sum, p.hash) {
		// 8.4: mismatch -> delete the partial, record, continue.
		_ = out.Close()
		_ = os.Remove(partial)
		return payloadFailed, fmt.Sprintf("hash mismatch (8.4 verification): record %x, restored stream %x; the partial was deleted", p.hash, sum), ""
	}

	// 5.1 steps 3-4: fsync, close, atomic rename, fsync the directory
	// ("where practical": an EINVAL/ENOTSUP dir-sync is tolerated, the
	// copyengine syncDir convention).
	var notes []string
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(partial)
		return payloadFailed, fmt.Sprintf("fsync of the partial: %v", err), ""
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(partial)
		return payloadFailed, fmt.Sprintf("closing the partial: %v", err), ""
	}
	if err := os.Rename(partial, final); err != nil {
		_ = os.Remove(partial)
		return payloadFailed, fmt.Sprintf("renaming the verified partial into place: %v", err), ""
	}
	if err := syncDir(filepath.Dir(final)); err != nil {
		notes = append(notes, fmt.Sprintf("fsync of the containing directory failed after the rename (%v); the rename itself succeeded and the content is verified", err))
	}
	// 5.4 metadata AFTER content - on the FINAL path, post-rename.
	if w := applyMeta(final, p.mode, p.uid, p.gid, p.mtimeNS); w != "" {
		notes = append(notes, w)
	}
	return payloadRestored, "", strings.Join(notes, "; ")
}

// --- chunked parents: the cross-image assembly (8.4, NB-03/NB-04) -------------

// chunkParent restores ONE chunked parent from this image's share of its
// chunks (see the file doc for the cross-image and count-once contracts).
func (c *imageCall) chunkParent(chunks []ChunkItem) {
	rel := chunks[0].ParentRelPath
	var contrib int64
	for _, ch := range chunks {
		contrib += ch.Length
	}

	st := c.restorer.assemblyFor(c.target, rel)
	for _, ch := range chunks {
		if ch.ParentNumChunks > st.numChunks {
			st.numChunks = ch.ParentNumChunks
		}
	}
	if st.failed || st.complete {
		// NB-04: counted ONCE (failed at the detecting image, restored at
		// the completion image). This image contributes nothing to the
		// member now - the phase counted its chunks, so progress advances,
		// but no outcome row and no count is ever invented here.
		c.itemDone(rel, contrib)
		return
	}
	if st.numChunks <= 0 {
		st.failed = true
		c.recordFailed(rel, statusIncomplete, "the parent record carries no chunk count, so the assembly cannot be completed (DESIGN_SPEC 8.4); no final file written", contrib)
		c.itemDone(rel, contrib)
		return
	}

	final, err := targetPath(c.target, rel)
	if err != nil {
		st.failed = true
		c.recordFailed(rel, statusIncomplete, fmt.Sprintf("unsafe relpath: %v; no final file written", err), contrib)
		c.itemDone(rel, contrib)
		return
	}
	if sibling, blocked := c.blocked[rel]; blocked {
		st.failed = true
		c.recordFailed(rel, statusIncomplete, fmt.Sprintf(
			"partial-path collision: assembling %q derives the partial path %s, which is the FINAL path of the selected member %q; "+
				"refusing before any write (the B1 collision rule applied over the whole selection); no final file written",
			rel, final+partialSuffix, sibling), contrib)
		c.itemDone(rel, contrib)
		return
	}

	partial := final + partialSuffix
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		st.failed = true
		c.recordFailed(rel, statusFailed, fmt.Sprintf("creating the destination directory: %v", err), contrib)
		c.itemDone(rel, contrib)
		return
	}
	// FIRST chunk of this run -> truncate: the assembly starts (or
	// re-starts, discarding an aborted run's stale one - the honest
	// cross-image crash recovery). Later images open WITHOUT truncation:
	// their predecessors' verified bytes live here and must survive.
	fresh := !st.started
	flags := os.O_CREATE | os.O_RDWR
	if fresh {
		flags |= os.O_TRUNC
	}
	out, err := os.OpenFile(partial, flags, 0o644)
	if err != nil {
		// OpenFile is one syscall: a failure means NO truncation and NO
		// new file - there is nothing this call wrote to clean up (and a
		// stale previous-run assembly, if any, stays diagnosable; the
		// detail below promises no branch either way).
		st.failed = true
		c.recordFailed(rel, statusFailed, fmt.Sprintf("opening the assembly %s: %v", partial, err), contrib)
		c.itemDone(rel, contrib)
		return
	}
	if fresh {
		st.started = true
		st.have = map[int]bool{}
	}

	// FIRST VERIFICATION (8.4): each chunk's bytes are checked against its
	// STORED chunk hash as it streams into its recorded offset.
	var (
		missingIdx, corruptIdx []int
		reasons                []string
	)
	for _, ch := range chunks {
		if err := c.ctx.Err(); err != nil {
			_ = out.Close()
			return // cancelled mid-assembly: the .partial stays (see the file doc)
		}
		ce := c.appendChunk(out, ch)
		if c.ctx.Err() != nil {
			// Cancellation surfaced from inside the stream: abandon the
			// growing file in place and end the run (no outcome, no count).
			_ = out.Close()
			return
		}
		if ce != nil {
			if ce.missing {
				missingIdx = append(missingIdx, ch.ChunkIndex)
			} else {
				corruptIdx = append(corruptIdx, ch.ChunkIndex)
			}
			reasons = append(reasons, fmt.Sprintf("chunk %d (%s): %v", ch.ChunkIndex, ce.kind(), ce.reason))
			continue
		}
		st.have[ch.ChunkIndex] = true
	}

	if len(missingIdx) > 0 || len(corruptIdx) > 0 {
		st.failed = true
		abandon := c.abandonAssembly(partial, fresh)
		_ = out.Close()
		if fresh {
			_ = os.Remove(partial)
		}
		detail := fmt.Sprintf("chunked file incomplete (DESIGN_SPEC 8.4, FR-041): missing chunks %s; corrupt chunks %s; %s; %s; no final file was written for this member",
			indexList(missingIdx), indexList(corruptIdx),
			strings.Join(reasons, "; "), abandon)
		c.recordFailed(rel, statusIncomplete, detail, contrib)
		c.itemDone(rel, contrib)
		return
	}

	// This image's share is durable: fsync before the next image's write
	// can land on top of it (the cross-image write discipline).
	if err := out.Sync(); err != nil {
		st.failed = true
		abandon := c.abandonAssembly(partial, fresh)
		_ = out.Close()
		if fresh {
			_ = os.Remove(partial)
		}
		c.recordFailed(rel, statusFailed, fmt.Sprintf("fsync of the assembly: %v; %s", err, abandon), contrib)
		c.itemDone(rel, contrib)
		return
	}
	if err := out.Close(); err != nil {
		st.failed = true
		abandon := c.abandonAssembly(partial, fresh)
		if fresh {
			_ = os.Remove(partial)
		}
		c.recordFailed(rel, statusFailed, fmt.Sprintf("closing the assembly: %v; %s", err, abandon), contrib)
		c.itemDone(rel, contrib)
		return
	}

	if !st.covered() {
		// NB-03/NB-04 PENDING: the growing file lives in the TARGET (it
		// survives the workspace wipe), the parent is counted at its
		// completion image - this one contributes bytes and progress only.
		c.res.Bytes += contrib
		c.itemDone(rel, contrib)
		return
	}

	// SECOND VERIFICATION (8.4): read the assembled file back, size and
	// whole-file hash against the parent's catalog anchor.
	state, detail, warning, drop := c.verifyAssembly(final, partial, chunks[0], fresh)
	switch state {
	case payloadRestored:
		st.complete = true
		c.recordRestored(rel, contrib, warning)
	case payloadFailed:
		st.failed = true
		if fresh && drop {
			// The documented fresh-branch deletion: this call created the
			// assembly and its bytes failed (or could not pass) the second
			// verification - nothing verified is lost. A failure that
			// leaves VERIFIED bytes behind (a rename fault) does not drop.
			_ = os.Remove(partial)
		}
		// Assembly untrusted = the 8.4 incompleteness class; a publish-only
		// fault (bytes verified, rename refused) is an ordinary failure.
		status := statusIncomplete
		if !drop {
			status = statusFailed
		}
		c.recordFailed(rel, status, detail, contrib)
	default: // aborted mid-read-back: leave everything, end the run
		return
	}
	c.itemDone(rel, contrib)
}

// chunkErr is appendChunk's classification of one chunk's read.
type chunkErr struct {
	missing bool   // the source chunk could not be opened (FR-041 "missing")
	reason  string // what went wrong (first-verification mismatch text, errno, ...)
}

func (e *chunkErr) kind() string {
	if e.missing {
		return "missing"
	}
	return "corrupt"
}

// appendChunk streams ONE chunk from the source to its recorded offset in the
// assembly while hashing it (the FIRST verification), then checks length and
// the stored chunk hash. Bytes of a chunk that fails verification may remain
// in the abandoned .partial at that offset - harmless, because a failed
// parent never reaches a final name and the next run truncates the assembly.
func (c *imageCall) appendChunk(out *os.File, ch ChunkItem) *chunkErr {
	rel := ch.ParentRelPath
	mediaPath, err := media.ChunkMediaPath(rel, int64(ch.ChunkIndex))
	if err != nil {
		return &chunkErr{missing: false, reason: fmt.Sprintf("media path: %v", err)}
	}
	src, err := os.Open(filepath.Join(c.src, filepath.FromSlash(mediaPath)))
	if err != nil {
		return &chunkErr{missing: true, reason: fmt.Sprintf("opening source chunk %s: %v", mediaPath, err)}
	}
	defer src.Close() //nolint:errcheck // read-only close

	if _, err := out.Seek(ch.Offset, io.SeekStart); err != nil {
		return &chunkErr{missing: false, reason: fmt.Sprintf("seeking the assembly to offset %d: %v", ch.Offset, err)}
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), &ctxReader{ctx: c.ctx, r: src})
	if err != nil {
		if c.ctx.Err() != nil {
			return nil // the caller sees ctx.Err() and aborts the run
		}
		return &chunkErr{missing: false, reason: fmt.Sprintf("streaming: %v", err)}
	}
	switch {
	case n != ch.Length:
		return &chunkErr{missing: false, reason: fmt.Sprintf("length mismatch: read %d bytes, the record says %d", n, ch.Length)}
	case len(ch.Hash) == 0:
		return &chunkErr{missing: false, reason: "the chunk row carries no stored hash, so the first verification cannot run"}
	default:
		if sum := h.Sum(nil); !bytes.Equal(sum, ch.Hash) {
			return &chunkErr{missing: false, reason: fmt.Sprintf("hash mismatch (first verification): stored %x, read %x", ch.Hash, sum)}
		}
	}
	return nil
}

// abandonAssembly renders the documented leave-vs-delete decision of the file
// doc as the outcome detail's tail. The caller performs the delete when fresh.
func (c *imageCall) abandonAssembly(partial string, fresh bool) string {
	if fresh {
		return fmt.Sprintf("the partial %s was deleted: it held only this image's bytes, so nothing verified was lost", partial)
	}
	return fmt.Sprintf("the partial %s was LEFT IN PLACE: it holds verified chunks from an earlier image of this run, and an unmarked .partial is never a final file - the next run re-creates and re-verifies the assembly", partial)
}

// verifyAssembly runs the 8.4 SECOND verification over the completed
// assembly: the file must be exactly ParentSize bytes and hash (streamed
// from disk - the read-back IS the point: it proves the bytes earlier images
// left behind and this image appended form the recorded whole file) to the
// parent's whole-file catalog hash. Pass -> rename + dir fsync + 5.4
// metadata; fail -> (detail, drop) tells the caller whether the documented
// leave-vs-delete branch applies (drop=true for the verification failures,
// whose fresh-branch detail promises a deletion; false when the bytes are
// verified and only the publish step faulted - those stay for diagnosis).
func (c *imageCall) verifyAssembly(final, partial string, parent ChunkItem, fresh bool) (payloadState, string, string, bool) {
	// Every drop=true failure below states WHICH leave-vs-delete branch its
	// caller takes, so the report never describes a deletion ambiguously
	// (except the stat failure - when the file cannot even be measured,
	// neither branch's claim can be made honestly).
	abandon := c.abandonAssembly(partial, fresh)
	fi, err := os.Stat(partial)
	if err != nil {
		return payloadFailed, fmt.Sprintf("stat of the assembled file: %v; no final file was written", err), "", true
	}
	if fi.Size() != parent.ParentSize {
		return payloadFailed, fmt.Sprintf(
			"assembled size mismatch (8.4 second verification): assembled %d bytes, the record says %d; no final file was written; %s",
			fi.Size(), parent.ParentSize, abandon), "", true
	}
	f, err := os.Open(partial)
	if err != nil {
		return payloadFailed, fmt.Sprintf("re-opening the assembled file for the second verification: %v; no final file was written; %s", err, abandon), "", true
	}
	h := sha256.New()
	_, err = io.Copy(h, &ctxReader{ctx: c.ctx, r: f})
	_ = f.Close() //nolint:errcheck // read-only close; the streamed hash dominates
	if err != nil {
		if c.ctx.Err() != nil {
			return payloadAborted, "", "", false
		}
		return payloadFailed, fmt.Sprintf("hashing the assembled file: %v; no final file was written; %s", err, abandon), "", true
	}
	if len(parent.ParentHash) == 0 {
		return payloadFailed, "the parent record carries no whole-file hash, so the second verification cannot run; no final file was written; " + abandon, "", true
	}
	if sum := h.Sum(nil); !bytes.Equal(sum, parent.ParentHash) {
		return payloadFailed, fmt.Sprintf(
			"assembled hash mismatch (8.4 second verification): every chunk passed its stored hash, but the assembled stream is %x while the record says %x; "+
				"no final file was written; %s",
			sum, parent.ParentHash, abandon), "", true
	}
	if err := os.Rename(partial, final); err != nil {
		return payloadFailed, fmt.Sprintf("renaming the verified assembly into place: %v", err), "", false
	}
	var notes []string
	if err := syncDir(filepath.Dir(final)); err != nil {
		notes = append(notes, fmt.Sprintf("fsync of the containing directory failed after the rename (%v); the rename itself succeeded and the content is verified", err))
	}
	if w := applyMeta(final, parent.ParentMode, parent.ParentUID, parent.ParentGID, parent.ParentMtimeNS); w != "" {
		notes = append(notes, w)
	}
	return payloadRestored, "", strings.Join(notes, "; "), false
}

// --- symlinks (D-03, the pinned osirrox contract) -----------------------------

// symlink recreates one symlink with its RECORDED target (never followed;
// a dangling target is carried verbatim) and refuses any destination it did
// not create - the copyengine specials precedent: EEXIST is tolerated only
// when the existing entry already IS this exact symlink (idempotent re-run).
func (c *imageCall) symlink(s SymlinkItem) {
	final, err := targetPath(c.target, s.RelPath)
	if err != nil {
		c.recordFailed(s.RelPath, statusFailed, fmt.Sprintf("unsafe relpath: %v", err), s.Size)
		c.itemDone(s.RelPath, s.Size)
		return
	}
	if s.LinkTarget == "" {
		c.recordFailed(s.RelPath, statusFailed, "the symlink record carries no target (the catalog row is malformed); no link written", s.Size)
		c.itemDone(s.RelPath, s.Size)
		return
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		c.recordFailed(s.RelPath, statusFailed, fmt.Sprintf("creating the destination directory: %v", err), s.Size)
		c.itemDone(s.RelPath, s.Size)
		return
	}
	if err := os.Symlink(s.LinkTarget, final); err != nil {
		if !errors.Is(err, os.ErrExist) {
			c.recordFailed(s.RelPath, statusFailed, fmt.Sprintf("creating the symlink: %v", err), s.Size)
			c.itemDone(s.RelPath, s.Size)
			return
		}
		existing, lerr := os.Readlink(final)
		if lerr != nil || existing != s.LinkTarget {
			// NEVER clobber: the destination holds an entry this run did
			// not write (or a link with a different target).
			c.recordFailed(s.RelPath, statusFailed, fmt.Sprintf(
				"destination %s exists and is not a symlink with the recorded target %q (refusing to clobber)", final, s.LinkTarget), s.Size)
			c.itemDone(s.RelPath, s.Size)
			return
		}
	}
	if err := syncDir(filepath.Dir(final)); err != nil {
		c.recordRestored(s.RelPath, s.Size, fmt.Sprintf("fsync of the containing directory failed: %v", err))
		c.itemDone(s.RelPath, s.Size)
		return
	}
	// Read-back target equality (D-03 fidelity; the copyengine precedent).
	readback, err := os.Readlink(final)
	if err != nil {
		c.recordFailed(s.RelPath, statusFailed, fmt.Sprintf("read-back verify: %v", err), s.Size)
		c.itemDone(s.RelPath, s.Size)
		return
	}
	if readback != s.LinkTarget {
		_ = os.Remove(final)
		c.recordFailed(s.RelPath, statusFailed, fmt.Sprintf("read-back target %q != recorded %q", readback, s.LinkTarget), s.Size)
		c.itemDone(s.RelPath, s.Size)
		return
	}
	// 5.4 metadata on the LINK (fsmeta.Apply never follows it; chmod is
	// skipped for symlinks on Linux - no lchmod in the kernel).
	warning := applyMeta(final, s.Mode, s.UID, s.GID, s.MtimeNS)
	c.recordRestored(s.RelPath, s.Size, warning)
	c.itemDone(s.RelPath, s.Size)
}

// --- empty directories (FR-037, 5.4) ------------------------------------------

// emptyDir creates one recorded empty directory with its metadata. EEXIST is
// tolerated (mkdir is the idempotent publication of 2.3: another image or a
// prior run may already have created it); a non-directory occupying the path
// fails honestly.
func (c *imageCall) emptyDir(d DirItem) {
	final, err := targetPath(c.target, d.RelPath)
	if err != nil {
		c.recordFailed(d.RelPath, statusFailed, fmt.Sprintf("unsafe relpath: %v", err), 0)
		c.itemDone(d.RelPath, 0)
		return
	}
	if err := os.MkdirAll(final, 0o755); err != nil {
		c.recordFailed(d.RelPath, statusFailed, fmt.Sprintf("creating the directory: %v", err), 0)
		c.itemDone(d.RelPath, 0)
		return
	}
	// mkdir -> metadata -> parent fsync (the copyengine WriteEmptyDir
	// ordering: the fsync's journal commit then carries the metadata too).
	var notes []string
	if w := applyMeta(final, d.Mode, d.UID, d.GID, d.MtimeNS); w != "" {
		notes = append(notes, w)
	}
	if err := syncDir(filepath.Dir(final)); err != nil {
		notes = append(notes, fmt.Sprintf("fsync of the parent directory failed: %v", err))
	}
	c.recordRestored(d.RelPath, 0, strings.Join(notes, "; "))
	c.itemDone(d.RelPath, 0)
}

// --- hard-link families (D-04, 8.2 "links for the requested names") -----------

// secondary recreates ONE requested family name (see the file doc for both
// branches: link(2) to a selected primary, or the content-carrying first
// requested name when the primary is unselected).
func (c *imageCall) secondary(l LinkItem) {
	final, err := targetPath(c.target, l.RelPath)
	if err != nil {
		c.recordFailed(l.RelPath, statusFailed, fmt.Sprintf("unsafe relpath: %v", err), l.Size)
		c.itemDone(l.RelPath, l.Size)
		return
	}

	if _, primaryIsSelected := c.primarySelected[l.PrimaryRelPath]; primaryIsSelected {
		if !c.restoredPrimary[l.PrimaryRelPath] {
			// The primary failed (or was refused) in THIS pass: a link to
			// a primary whose bytes never landed would be a lie.
			c.recordFailed(l.RelPath, statusFailed, fmt.Sprintf(
				"cannot create the hard link: the family primary %q was not restored (its content failed or was refused; see its own finding); no bytes written for this name",
				l.PrimaryRelPath), l.Size)
			c.itemDone(l.RelPath, l.Size)
			return
		}
		anchor, _ := targetPath(c.target, l.PrimaryRelPath)
		if detail, ok := c.linkTo(anchor, final, l.RelPath, l.PrimaryRelPath); !ok {
			c.recordFailed(l.RelPath, statusFailed, detail, l.Size)
			c.itemDone(l.RelPath, l.Size)
			return
		}
		c.recordRestored(l.RelPath, l.Size, "")
		c.itemDone(l.RelPath, l.Size)
		return
	}

	// The primary is NOT in this selection (T5.3's partial-family rule):
	// the bytes come from the primary's stored name but must land at a
	// REQUESTED name - the unselected primary never appears in the target.
	st := c.families[l.PrimaryRelPath]
	if st == nil {
		st = &familyState{}
		c.families[l.PrimaryRelPath] = st
	}
	switch {
	case st.err != "":
		c.recordFailed(l.RelPath, statusFailed, st.err, l.Size)
		c.itemDone(l.RelPath, l.Size)
		return
	case st.path != "":
		if detail, ok := c.linkTo(st.path, final, l.RelPath, l.PrimaryRelPath); !ok {
			c.recordFailed(l.RelPath, statusFailed, detail, l.Size)
			c.itemDone(l.RelPath, l.Size)
			return
		}
		c.recordRestored(l.RelPath, l.Size, "")
		c.itemDone(l.RelPath, l.Size)
		return
	}

	// First requested name of the family: restore the content HERE (one
	// write for the whole family), then anchor the rest to it.
	state, detail, warning := c.writePayload(payload{
		dstRel: l.RelPath, srcRel: l.PrimaryRelPath, size: l.Size, hash: l.Hash,
		mode: l.Mode, uid: l.UID, gid: l.GID, mtimeNS: l.MtimeNS,
	})
	switch state {
	case payloadRestored:
		st.path = final
		c.recordRestored(l.RelPath, l.Size, warning)
		c.itemDone(l.RelPath, l.Size)
	case payloadFailed:
		st.err = fmt.Sprintf("cannot create the hard link: the family content could not be restored at %q (%s); no bytes written for this name", l.RelPath, detail)
		c.recordFailed(l.RelPath, statusFailed, st.err, l.Size)
		c.itemDone(l.RelPath, l.Size)
	default: // aborted: end the run without inventing an outcome
	}
}

// linkTo creates final as link(2) of anchor and verifies inode identity
// (FR-039's "the family shares an inode"); EEXIST is acceptable only when
// the existing entry already shares that inode (idempotent re-run), anything
// else is a refusal to clobber (the copyengine specials precedent). NO
// metadata is applied: links share the inode state the anchor's Apply wrote.
func (c *imageCall) linkTo(anchor, final, rel, anchorRel string) (string, bool) {
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return fmt.Sprintf("creating the destination directory: %v", err), false
	}
	anchorInfo, err := os.Lstat(anchor)
	if err != nil {
		return fmt.Sprintf("stat of the family anchor %s: %v", anchor, err), false
	}
	if err := os.Link(anchor, final); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return fmt.Sprintf("link(2) %s -> %s: %v", anchor, final, err), false
		}
		existing, lerr := os.Lstat(final)
		if lerr != nil || !os.SameFile(anchorInfo, existing) {
			return fmt.Sprintf("destination %s exists and does not share %s's inode (refusing to clobber)", final, anchorRel), false
		}
	}
	if err := syncDir(filepath.Dir(final)); err != nil {
		return fmt.Sprintf("fsync directory of %s: %v", final, err), false
	}
	written, lerr := os.Lstat(final)
	if lerr != nil {
		return fmt.Sprintf("read-back verify: %v", lerr), false
	}
	if !os.SameFile(anchorInfo, written) {
		_ = os.Remove(final)
		return fmt.Sprintf("read-back inode identity with %s failed", anchorRel), false
	}
	return "", true
}

// --- outcome and progress helpers (nil-bus safe, the Deps convention) ---------

// recordRestored counts one member restored at THIS image (the count-once
// contract means a spanning chunked parent lands here - at completion - and
// only here). A warning (the 5.4 EPERM downgrade, or any post-content
// metadata failure) travels in the outcome's Detail and as a progress.Warn:
// it is recorded, never counted as a failure (5.4: the run completes).
func (c *imageCall) recordRestored(rel string, size int64, warning string) {
	c.res.Restored++
	c.res.Bytes += size
	if warning != "" {
		c.res.Files = append(c.res.Files, FileOutcome{RelPath: rel, Status: "restored", Bytes: size, Detail: warning})
		c.warn(warning)
	}
}

// recordFailed records one member that did NOT restore (every non-restored
// member MUST appear in Files - the report enumerates exactly those), emits
// the item-level progress Error, and counts it toward the exit-10 class.
// FileOutcome.Bytes is the member's size (the report's per-finding figure);
// ImageRestoreResult.Bytes deliberately EXCLUDES failed members - the bytes
// they wrote were deleted or abandoned without a final file.
func (c *imageCall) recordFailed(rel, status, detail string, size int64) {
	c.res.Failed++
	c.res.Files = append(c.res.Files, FileOutcome{RelPath: rel, Status: status, Bytes: size, Detail: detail})
	c.emitError(fmt.Sprintf("%s: %s", rel, detail))
}

// itemDone emits the one per-member progress event inside T5.3's phase (one
// per distinct member of this image's work list, at its workTotals
// contribution - including members processed into a pending or failed state,
// which the phase's denominators counted).
func (c *imageCall) itemDone(rel string, bytes int64) {
	if c.restorer.deps.Bus != nil {
		c.restorer.deps.Bus.Emit(progress.ItemDone{Bytes: bytes, Path: rel})
	}
}

func (c *imageCall) warn(msg string) {
	if c.restorer.deps.Bus != nil {
		c.restorer.deps.Bus.Emit(progress.Warn{Msg: msg})
	}
}

func (c *imageCall) emitError(msg string) {
	if c.restorer.deps.Bus != nil {
		c.restorer.deps.Bus.Emit(progress.Error{Msg: msg})
	}
}

// --- shared primitives ---------------------------------------------------------

// targetPath joins one CATALOG relpath onto the run's target, refusing any
// relpath the media validator rejects (defense in depth: selection validated
// the SELECTED names, but inventory rows are catalog data - a corrupt or
// hostile ".."/absolute path must never escape --target).
func targetPath(root, rel string) (string, error) {
	if err := media.ValidateRelPath(rel); err != nil {
		return "", err
	}
	return filepath.Join(root, filepath.FromSlash(rel)), nil
}

// partialCollisionGate builds the B1-mirror refusal map from the run's whole
// SELECTION (selection.Paths - the only names this run may create): every
// selected rel whose derived partial is itself a selected member's final is
// refused before any write; the sibling proceeds (its own derived partial
// collides with nothing unless the names chain further, in which case each
// colliding owner is refused the same way). Selection-wide by design - see
// the file doc's gate section for why the copy pipeline's per-image gate
// cannot cover the cross-image pair.
func partialCollisionGate(selected []string) map[string]string {
	have := make(map[string]struct{}, len(selected))
	for _, rel := range selected {
		have[rel] = struct{}{}
	}
	var blocked map[string]string
	for _, rel := range selected {
		if _, ok := have[rel+partialSuffix]; ok {
			if blocked == nil {
				blocked = map[string]string{}
			}
			blocked[rel] = rel + partialSuffix
		}
	}
	return blocked
}

// indexList renders chunk indices for the FR-041 detail ("[0, 2]").
func indexList(idxs []int) string {
	if len(idxs) == 0 {
		return "[]"
	}
	sorted := append([]int(nil), idxs...)
	sort.Ints(sorted)
	var b strings.Builder
	b.WriteByte('[')
	for i, v := range sorted {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%d", v)
	}
	b.WriteByte(']')
	return b.String()
}

// applyMeta runs the 5.4 metadata pass (fsmeta.Apply) and renders its outcome
// as a WARNING string ("" = clean). Per 5.4's caller rule, a metadata failure
// AFTER verified content is a per-file WARNING - the final file stays, the
// member stays restored - while fsmeta.Apply itself already downgrades the
// non-root EPERM chown to a warning. The catalog stores RAW POSIX modes, so
// fsmeta.GoMode converts at this single apply site (the T3.4 invariant:
// handing a raw 0o4755 to os.Chmod silently drops setuid).
func applyMeta(path string, rawMode, uid, gid uint32, mtimeNS int64) string {
	res, err := fsmeta.Apply(path, fsmeta.Meta{
		Mode:    fsmeta.GoMode(rawMode),
		UID:     uid,
		GID:     gid,
		MtimeNS: mtimeNS,
	})
	if err != nil {
		return fmt.Sprintf("metadata could not be applied (content is verified and renamed): %v", err)
	}
	return res.Warning
}

// syncDir fsyncs a containing directory so a rename survives a crash
// (5.1 step 4, "where practical": EINVAL/ENOTSUP on filesystems that cannot
// dir-fsync are tolerated). This is the copyengine.syncDir convention
// re-declared here with a cross-reference - see partialSuffix's note on why
// restore does not import the copy engine.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close() //nolint:errcheck // best-effort close after Sync
	if err := d.Sync(); err != nil {
		if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) {
			return nil
		}
		return err
	}
	return nil
}

// ctxReader aborts a long stream the moment the context is cancelled (the
// verifyCtxReader / copyengine ctxReader shape: a cancelled run must never
// sit inside a multi-gigabyte io.Copy).
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
