// Package restore implements the restore engine of DESIGN_SPEC section 8:
// selection (8.1 D-07 latest-state semantics, 8.2 selection model), image
// supply and multi-image orchestration (8.3), and - arriving with T5.4 -
// the per-image content restoration (extraction, chunked reassembly with
// double verification, metadata application, 8.4; media-alone supply, 8.5).
//
// # Scope split: T5.3 versus T5.4 (the seam this package is built around)
//
// T5.3 (this task) owns everything UP TO the byte-writing boundary:
//
//   - SELECTION: which catalog records the run restores (whole set, repeatable
//     --path, --subtree), resolved against the LATEST ACTIVE records only
//     (the v_active_files predicate - 8.2, D-07);
//   - RESOLUTION: which image numbers hold those records, including the T3.4
//     unified hard-link claim rule (a selected family member's bytes live on
//     the PRIMARY's image) and the chunked case (a chunked record resolves to
//     EVERY image holding one of its chunks);
//   - SUPPLY: finding each required image through the 8.3 preference order
//     (--image-dir discovery, explicit --image N=PATH, --copy-tree-root), the
//     TTY-gated prompt, and the non-TTY machine-readable listing;
//   - ORCHESTRATION: the one-at-a-time image loop over ONE temp workspace
//     cleaned between images, progress phases, and the report shell.
//
// T5.4 owns the CONTENT RESTORATION of one image: reading bytes out of an ISO
// (xorriso osirrox extraction) or a copy tree, chunked reassembly with the
// two 8.4 verification gates, and applying metadata to --target. T5.4
// implements ImageRestorer; T5.3 calls it once per required image and never
// touches a payload byte itself.
//
// The seam, exactly as the T5.3 brief fixes it:
//
//	type ImageRestorer interface {
//	    RestoreImage(ctx context.Context, image ImageSupply,
//	        selection ResolvedSelection, workspace string) (ImageRestoreResult, error)
//	}
//
// What each argument carries - T5.4's contract:
//
//   - image (ImageSupply): the IMAGE NUMBER, the SOURCE KIND (SourceISO for an
//     .iso file, SourceCopyTree for an S-NNN copy-tree directory - an ISO file
//     and a copy tree have different shapes, so the kind travels with the path
//     and --image N=PATH is classified by stat), that path, and what supply
//     discovery Sought. The TARGET path travels on selection.Target (one
//     target per run, 8.2 "--target required"), so the two arguments cannot
//     disagree about the destination.
//   - selection (ResolvedSelection): the ordered selected RELPATHS (the only
//     names T5.4 may create under --target - an unselected hard-link sibling
//     must NOT appear, 8.2 "links for the requested names"), the run's target,
//     the selection totals, and Work - the per-image inventory of EVERY
//     required image (ascending), already filtered to the selection. Use
//     ResolvedSelection.WorkFor(image.Image) for the entry this call needs; it
//     carries files, symlinks, chunk rows (with the parent's reassembly
//     anchors), empty dirs, and hard-link secondaries with their primary's
//     relpath - everything the 8.4 engine and the 5.4 metadata rules consume.
//   - workspace: THE single temp directory this run created under --work-dir
//     (or the OS temp root). The SAME path is passed for every image, the
//     orchestration cleans it between images, and it removes the whole
//     workspace on every exit path. T5.4 must do its extraction scratch work
//     INSIDE this directory rather than minting a parallel workspace, and may
//     leave debris in it after its image returns (the orchestration cleans).
//
// What T5.4 must implement, concretely:
//
//   - restore.NewImageRestorer(RestorerDeps{...}) returning an
//     ImageRestorer that (per image) reads the image through the supply kind,
//     writes to <target> via .partial + rename, verifies per 8.4 (chunk hash
//     on read, whole-file hash after assembly), applies 5.4 metadata, and
//     returns an ImageRestoreResult enumerating every non-restored member
//     (skipped/failed) with counts and byte totals. Per-member progress goes
//     on Deps.Bus (ItemDone/BytesWritten) inside the phase T5.3 declares.
//   - the CLI layer wires that constructor through restoreRestorerFactory
//     in internal/cli/restore.go; no orchestration or selection code changed
//     with it (the pending placeholder and its ErrNotImplemented were
//     deleted once T5.4 landed).
//
// # What T5.4 shipped: the obligations this package now discharges (restorer.go)
//
// The engine behind NewImageRestorer implements the seam above. Beyond the
// mechanics the plan states (source dispatch, .partial + fsync + rename +
// dir-fsync + post-rename metadata, per-chunk first verification, whole-file
// second verification, FR-041's per-file incompleteness, symlinks as links,
// idempotent empty dirs, family link(2) recreation), the following contracts
// are PINNED here because they are invisible in any single call and a later
// change could break them silently:
//
//   - CROSS-IMAGE ASSEMBLY (the T5.3 reviewer's NB-3, a standing
//     requirement): a chunked parent spanning several images is assembled
//     ACROSS RestoreImage calls whose shared workspace is WIPED between
//     images (8.3). The growing assembly therefore lives IN THE TARGET as
//     <target>/<relpath>.partial - it survives every wipe - created by the
//     run's FIRST chunk write (O_TRUNC: also how a new run honestly discards
//     an aborted run's stale assembly) and appended by later images at their
//     recorded offsets WITHOUT truncation. Per-image extraction scratch goes
//     INSIDE the workspace (a unique subdir per ISO image), so the
//     orchestration's per-image clean handles it; no restoration write ever
//     depends on the workspace surviving past its own call. The file's size
//     alone cannot say WHICH chunks it holds, so the restorer counts the
//     indices it has written per parent for the run, and only a fully
//     covered parent runs the second verification (read-back of the assembled
//     file against the parent's whole-file hash - the read-back is what
//     catches a "covered" assembly whose bytes are wrong).
//   - COUNT ONCE (NB-04): a spanning member is counted ONCE, at its
//     COMPLETION image (Restored). The images before completion record
//     nothing at all for it - no Restored, no Failed, no Files row - because
//     a pending member is neither, and a skipped/failed row there would turn
//     a healthy span into an exit-10 partial run. A member that FAILS
//     (missing/corrupt chunk, assembled-hash mismatch) is recorded exactly
//     ONCE too, at the detecting image; later images holding more of its
//     chunks skip it silently. Its bytes and progress still count per image
//     (ImageRestoreResult.Bytes, ItemDone), only the MEMBER count waits for
//     completion.
//   - THE .partial LEAVE-VS-DELETE DECISION (documented, test-pinned):
//     a whole-file verification failure DELETES its partial (8.4's explicit
//     rule); a chunked member's failure DELETES the assembly only when the
//     failing call created it (nothing verified is lost - the copyengine
//     unmarked-partial precedent applies), and LEAVES it in place when it
//     already holds earlier images' VERIFIED chunks (deleting would destroy
//     verified work for no trust benefit). Either way the target NEVER holds
//     a FINAL file for an incomplete member: the trust model lives entirely
//     on final names, an unmarked .partial reads as "assembly in progress or
//     abandoned, re-verified from scratch by the next run", and the outcome
//     detail names which branch fired. Cancellation always leaves the
//     in-flight partial in place and renames nothing.
//   - TARGET LAYOUT: <target>/<relpath>.partial -> <target>/<relpath> (8.4's
//     prose says <target>/files/<relpath>.partial; the operative contract -
//     this brief, the mechanics line of the delegated task - is the
//     root-relative form, so restore output IS the original tree under
//     --target and a round-trip diff needs no prefix remapping).
//   - PARTIAL-FAMILY VERDICT: when the family PRIMARY is selected it is an
//     ordinary Work member (bytes at its own name; requested secondaries
//     link(2) to it; a failed primary makes every secondary a "cannot link"
//     failure with no bytes); when only SECONDARIES are selected - what
//     T5.3's filter actually produces (Work.Files then does NOT contain the
//     primary) - the bytes are read from files/<PrimaryRelPath> but written
//     at the FIRST requested name, the rest link(2) to it, and the
//     unselected primary never appears under --target.
//   - The selection-wide partial-collision gate (the B1 mirror): an item
//     whose derived .partial equals another SELECTED member's final path is
//     refused before any write - the copy pipeline's gate is per-image and
//     cannot see a pair split across two images.
//   - 5.4 warnings (EPERM chown and any post-content metadata failure) are
//     recorded as the restored member's Files Detail plus a progress.Warn:
//     never a failure, never exit-10 material; the stdout report lists only
//     non-restored members (the no-100k-lines convention), so warnings
//     surface live on stderr.
//   - One restorer instance serves one run (the factory contract): the
//     cross-image assembly state lives on the instance, and concurrent
//     RestoreImage calls are not supported (8.3's loop is sequential).
//
// # Layering and the read-only contract
//
// Like every service (DESIGN_SPEC 1.2), this package NEVER imports
// internal/catalog and never issues SQL: Store is a primitive-shaped read
// seam the cli adapts onto *catalog.DB (the copyengine/verify precedent).
// The run opens the catalog MODE=RO with no advisory lock (2.1 Option B), so
// restore runs DURING a copy or build over a WAL snapshot; it takes no runs
// row and no run_events (the read-only policy: the stdout report is the
// record of the run, the verify-media/validate precedent), and NOTHING in the
// catalog is mutated - a restore run leaves the catalog file byte-identical.
// There is deliberately no restore-state column in the schema (8.x): restore
// is a pure reader of media, so a re-run simply restores again.
//
// There is also no implicit original-source restore: --target is a required
// input (8.2) and this package never reads sets.source_root at all.
package restore
