package cli

// U.3 (DESIGN_SPEC 4.6.1 - THE COPY-TIME WIPE GATE): before copy
// writes an image whose image_contents list is EMPTY, it REMOVES the
// image's existing <dest>/<SET-ID>-NNN tree - loudly, after an
// ownership check - and then rewrites it. This is mechanism (b) of
// 4.6 "Rewriting the released image (copy mode)"; mechanism (a)
// (NO-SKIP) is structural in the unlock transaction (4.6 steps 1-4).
//
// PLACEMENT (4.6.1): the helper is invoked at the top of copyImage,
// AFTER imageRoot := filepath.Join(r.destAbs, imageDirName) and BEFORE
// CopyInventory and any writer call, so a refused image has NOTHING
// written to it (return before CopyInventory) while the other images
// of the run continue. It is structurally copy-mode-only: build never
// calls it (ISO needs no wipe - atomic rename-over; a failed build
// keeps the old .iso), and the gate keys on COPY-MODE generations
// only (the load-bearing invariant below).
//
// THE SIX ORDERED CHECKS (4.6.1 verbatim order - the order is the
// safety argument, so it is load-bearing):
//
//  1. ImageContentCount(set, N): a read ERROR returns through as a
//     CatalogError (exit 4) - the gate is NEVER bypassed, and an
//     error is NEVER treated as zero. n > 0 returns without a wipe:
//     resume, mixed-mode, and first-generation-in-progress cases all
//     land here (crash idempotency: a wipe happens before any mark, so
//     a crash after marks leaves contents non-empty and the next run
//     trusts the tree instead of destroying it).
//  2. os.Stat(imageRoot): absent -> fresh write, return; not a dir ->
//     counted refusal (exit-10 class).
//  3. OWNERSHIP: <imageRoot>/MANIFEST.json parsed with
//     media.ParseManifest (internal/media/manifest_parse.go - the
//     format authority, REUSED rather than a raw unmarshal, so shape
//     enforcement - a JSON object carrying a STRING set_id and a
//     POSITIVE image_number plus the schema_version check - fails
//     closed): ENOENT -> ours-by-namespace (the path is tool-derived
//     via media.ImageDirName; interrupted first copies legitimately
//     have no manifest); any other read error, a parse error, or a
//     set/image mismatch -> REFUSE. Nothing is written to a refused
//     image. The read itself is BOUNDED (U.4): os.Lstat first, so a
//     planted FIFO/directory at MANIFEST.json is refused without ever
//     being opened (an open FIFO would block this lock-holding run
//     forever), then a 16 MiB size cap, then the read.
//  4. COUNT: filepath.WalkDir collecting entries + regular-file bytes
//     (the accounting numbers of channel 1/2/3). A walk error -> REFUSE
//     (fail-closed: a tree we cannot enumerate is a tree we do not
//     destroy - RemoveAll would fail on the same subtree anyway).
//  5. os.RemoveAll(imageRoot): an error -> counted refusal (exit-10
//     class), its message disclosing that the removal may already be
//     PARTIAL and naming the pre-removal entry count as its upper
//     bound (U.4).
//  6. LOUD ACCOUNTING (the four channels below).
//
// REFUSALS all return a detail string the caller records through
// recordFail (counted failure -> exit 10, or 1 with --strict; image
// incomplete, manifest deferred, other images continue) - a wipe is
// never a failure itself, and a refusal never writes.
//
// THE FOUR LOUD-ACCOUNTING CHANNELS (4.6.1), one helper (recordWipe),
// buffer on the copyRunner (the failureLines pattern - NOT a
// copyImageReport field, per-image reports stay untouched):
//
//  1. the human stdout line, emitted by writeCopyReport between the
//     totals line and the metadata-warning line, before the failure
//     lines - "wiped: image N: removed X entries (Y) from <path>
//     before rewrite"; ZERO wipes = zero lines, so no-wipe stdout
//     stays byte-identical;
//  2. the copy --json `wipes` record (copyReport.Wipes, ALWAYS present,
//     [] when empty - the ratified frozen-key addition, 12 -> 13);
//  3. the run_events row (level warn, relpath = <imageRoot>, detail =
//     "wiped before rewrite: removed X entries (Y) from <path>
//     (image N of set S)");
//  4. the live stderr progress.Warn line (the channel-1 message minus
//     the "wiped: " prefix - safe under --json, progress is stderr).
//
// LOAD-BEARING SAFETY INVARIANT (copy-mode scope): for a copy-mode
// image, "image_contents(N) is empty" <=> nothing was finalized for N
// in this generation <=> no copied=1 mark describes N's media. The
// reverse direction is FALSE on the build path (BeginBuildTx
// materializes the content list at build START), which is why the
// gate is defined for COPY-MODE generations only: unlock -> plan ->
// copy wipes; unlock -> build never wipes; unlock -> build -> copy
// (mixed mode, out of scope) does not wipe and fails loudly at verify.

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"isopress/internal/media"
	"isopress/internal/progress"
	"isopress/internal/units"
)

// wipeRecord is ONE recorded removal - the copy --json `wipes` element
// (4.6.1 channel 2). Field names are frozen with the key set:
// entries_removed counts filesystem entries removed (regular files,
// symlinks, and directories, the root included - every inode
// RemoveAll destroys), bytes_removed counts REGULAR-FILE bytes only.
type wipeRecord struct {
	Image          int64  `json:"image"`
	Path           string `json:"path"`
	EntriesRemoved int    `json:"entries_removed"`
	BytesRemoved   int64  `json:"bytes_removed"`
}

// wipeBeforeRewrite runs the 4.6.1 gate for ONE image. It returns:
//
//   - ("", nil): write may proceed - either nothing needed wiping
//     (contents non-empty, path absent, fresh write) or the tree was
//     removed and accounted (channels 2-4 fire here; channel 1 fires
//     at report assembly);
//   - (detail, nil): REFUSAL - the caller records detail through
//     recordFail (exit-10 class) and must NOT write this image. detail
//     is already prefixed "refusing to wipe: " for the three verbatim
//     4.6.1 messages (foreign manifest / unparseable manifest /
//     cannot enumerate);
//   - ("", err): the gate's catalog read failed - a *catalog.CatalogError
//     (exit 4). The gate is NEVER bypassed, so copyImage returns this
//     as its terminal error.
func (r *copyRunner) wipeBeforeRewrite(ctx context.Context, imageID int64, imageRoot string) (string, error) {
	// --- check 1: the catalog gate (4.6.1 step 1) ---
	n, err := r.db.ImageContentCount(ctx, r.setID, imageID)
	if err != nil {
		return "", err // CatalogError -> exit 4; never bypassed, never "treated as zero"
	}
	if n > 0 {
		return "", nil // resume / mixed-mode / first-generation-in-progress: NO wipe
	}

	// --- check 2: the path (4.6.1 step 2) ---
	st, err := os.Stat(imageRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil // fresh write; nothing to wipe
		}
		// Any other stat failure means we cannot even see the tree -
		// fail closed with the enumerable message (a tree we cannot
		// inspect is a tree we do not destroy). U.4 (U.3-L1): the SAME
		// remediation suffix the step-4 walk branch carries for the
		// same reason, so both "cannot enumerate" renderings are one
		// message (4.6.1's own verbatim example includes the suffix).
		return wipeRefusal("cannot enumerate the directory (%s); remove or rename the directory, or choose a different --dest",
			wipeReason(err)), nil
	}
	if !st.IsDir() {
		return wipeRefusal("not a directory; remove or rename it, or choose a different --dest"), nil
	}

	// --- check 3: OWNERSHIP (4.6.1 step 3, condition i) ---
	manifestPath := filepath.Join(imageRoot, media.ManifestFileName)
	manifestBytes, err := readManifestBounded(manifestPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// ABSENT -> ours-by-namespace: the path is tool-derived
		// (media.ImageDirName, 5.3/6.4) and interrupted first copies
		// legitimately have no manifest. Known accepted exposure
		// (4.6.1): a manifest-less FOREIGN tree at our exact path is
		// still wiped - now loudly, through all four channels.
	case err != nil:
		return wipeRefusal("%s is not a readable manifest (%s); remove or rename the directory, or choose a different --dest",
			media.ManifestFileName, wipeReason(err)), nil
	default:
		m, perr := media.ParseManifest(manifestBytes)
		if perr != nil {
			// Unparseable = foreign (fail-closed): bad JSON, non-object
			// shape, wrong-typed or missing fields, schema_version
			// failure - ParseManifest enforces the shape rule and more.
			return wipeRefusal("%s is not a readable manifest (%s); remove or rename the directory, or choose a different --dest",
				media.ManifestFileName, wipeReason(perr)), nil
		}
		if m.SetID != r.setID || m.ImageNumber != imageID {
			return wipeRefusal("%s names set %q image %d (this is set %q image %d); remove or rename the directory, or choose a different --dest",
				media.ManifestFileName, m.SetID, m.ImageNumber, r.setID, imageID), nil
		}
		// parses and matches (set, image) -> OURS: proceed to wipe.
	}

	// --- check 4: COUNT (4.6.1 step 4) ---
	entries, nbytes, err := countTreeEntries(imageRoot)
	if err != nil {
		return wipeRefusal("cannot enumerate the directory (%s); remove or rename the directory, or choose a different --dest", wipeReason(err)), nil
	}

	// --- check 5: REMOVE (4.6.1 step 5) ---
	if err := os.RemoveAll(imageRoot); err != nil {
		// HONEST PARTIAL ACCOUNTING (U.4, U.3-L4): RemoveAll can fail
		// AFTER destroying part of the tree, and the old wording
		// ("cannot remove the directory (...)") implied nothing had
		// been removed at all. The walk count from check 4 is the
		// PRE-removal total - the exact number of entries actually
		// removed is not recoverable once the delete has failed - so
		// the message states the upper bound it does know ("up to N")
		// instead of either staying silent or claiming a precise
		// removed count it does not have.
		return wipeRefusal("cannot remove the directory (%s); up to %d entries may already have been removed; "+
			"remove or rename the directory, or choose a different --dest",
			wipeReason(err), entries), nil
	}

	// --- check 6: LOUD ACCOUNTING (4.6.1 step 6), channels 2-4 ---
	r.recordWipe(ctx, imageID, imageRoot, entries, nbytes)
	return "", nil
}

// wipeRefusal formats one 4.6.1 refusal detail (the caller runs it
// through recordFail -> counted failure -> exit 10, nothing written).
func wipeRefusal(format string, args ...any) string {
	return "refusing to wipe: " + fmt.Sprintf(format, args...)
}

// wipeReason renders the wrapped error of a refusal message the way
// the 4.6.1 examples spell it - the bare REASON, not the full error
// string: a PathError contributes only its reason ("permission denied",
// not "open /dest/S-003/files/sub: permission denied" - the report
// line already names the image path), and ParseManifest's redundant
// "media: parsing MANIFEST.json: " wrapper is dropped (the message
// already says which file is unreadable). The examples:
//
//	cannot enumerate the directory (permission denied)
//	MANIFEST.json is not a readable manifest (invalid character 'x' after object key)
func wipeReason(err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err.Error()
	}
	return strings.TrimPrefix(err.Error(), "media: parsing MANIFEST.json: ")
}

// maxManifestBytes bounds the ownership read of <imageRoot>/MANIFEST.json
// (U.4, U.3-L5). Real manifests are KILOBYTES - the 6.6 header plus one
// row per image entry - so 16 MiB is several orders of magnitude of
// headroom; the cap exists to stop a planted multi-gigabyte file from
// being slurped into memory while this run holds the catalog lock, not
// to constrain any manifest a real image produces.
const maxManifestBytes = 16 << 20

// readManifestBounded is the ownership step's (check 3) manifest read,
// replacing the raw os.ReadFile with a bounded one. It refuses, as a
// *fs.PathError so the caller renders it through the SAME
// "MANIFEST.json is not a readable manifest (<reason>)" wording every
// other read/parse failure uses:
//
//   - os.Lstat FIRST (never follows a symlink - the same lstat
//     semantics the check-4 walk uses): a NON-REGULAR file at this path
//     (a planted FIFO, a directory) is refused BEFORE any open, because
//     opening a FIFO for reading blocks until a writer appears, and
//     this run holds the catalog lock while it blocks;
//   - then the size cap: a regular file larger than maxManifestBytes is
//     refused before a byte is read;
//   - then os.ReadFile.
//
// TOCTOU (the window between the Lstat and the ReadFile): the path can
// be swapped inside it. The STABLE obstacles this bound was added for
// fail closed in the refusal direction - whatever bytes come back are
// validated by media.ParseManifest (the format authority, unchanged)
// and compared against (set_id, image_number), so a swapped-in
// FOREIGN manifest is refused on its content, and a read that errors
// is refused as unreadable; the stable planted obstacle itself - a
// FIFO that would block the open, or a giant file that would exhaust
// memory - is now refused before it can bite. The residual
// two-syscall window, however, does NOT fail closed in every outcome:
// it can still block on a FIFO swapped in after the Lstat, read a
// file swapped in larger than the Lstat-based cap (ReadFile itself is
// unbounded), or map ENOENT in the window to ours-by-namespace ->
// WIPE (the deliberate wipe policy, not a refusal). That residual is
// the design's accepted TOCTOU class (writing under <dest> at all
// already implies the ability to destroy what is there); the
// design-blessed Lstat + size-cap + ReadFile shape stays as-is.
func readManifestBounded(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err // ENOENT -> ours-by-namespace; any other error refused by the caller
	}
	if !fi.Mode().IsRegular() {
		return nil, &fs.PathError{Op: "open", Path: path, Err: errors.New("not a regular file")}
	}
	if fi.Size() > maxManifestBytes {
		return nil, &fs.PathError{Op: "open", Path: path,
			Err: fmt.Errorf("size %d exceeds the %d-byte manifest cap", fi.Size(), maxManifestBytes)}
	}
	return os.ReadFile(path)
}

// countTreeEntries walks the tree once (4.6.1 step 4) and returns the
// entry count - every path WalkDir visits, the root included, i.e.
// every inode RemoveAll is about to destroy - plus the bytes of the
// REGULAR files among them (symlink targets and directory sizes are
// not payload bytes). WalkDir never follows symlinks, so a symlink
// loop or a link pointing out of the tree cannot widen the walk; any
// walk error is returned so the caller fails closed.
func countTreeEntries(root string) (entries int, bytes int64, err error) {
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		entries++
		info, ierr := d.Info() // lstat semantics: a symlink's own size, never its target's
		if ierr != nil {
			return ierr
		}
		if info.Mode().IsRegular() {
			bytes += info.Size()
		}
		return nil
	})
	if walkErr != nil {
		return 0, 0, walkErr
	}
	return entries, bytes, nil
}

// recordWipe is the 4.6.1 loud-accounting helper - modelled on
// recordFail (copy.go) but EXIT-CODE-NEUTRAL: a wipe is expected
// behavior, never a failure, so it counts nothing and adds no report
// line of its own. It appends the JSON record to r.wipes (channel 2,
// consumed by finalize -> copyReport.Wipes), emits the live stderr
// progress.Warn line (channel 4), and writes the run_events row
// (channel 3: level warn, relpath = the destroyed path, detail with
// the entry count, the formatted bytes, the path, and the image/set
// identity). Channel 1 (the human stdout line) is emitted later by
// writeCopyReport, from the same buffer, human mode only.
func (r *copyRunner) recordWipe(ctx context.Context, imageID int64, path string, entries int, bytes int64) {
	r.wipes = append(r.wipes, wipeRecord{
		Image: imageID, Path: path, EntriesRemoved: entries, BytesRemoved: bytes,
	})
	r.bus.Emit(progress.Warn{Msg: fmt.Sprintf(
		"image %d: removed %d entries (%s) from %s before rewrite",
		imageID, entries, units.FormatBytes(bytes), path)})
	r.insertEvent(ctx, path, fmt.Sprintf(
		"wiped before rewrite: removed %d entries (%s) from %s (image %d of set %s)",
		entries, units.FormatBytes(bytes), path, imageID, r.setID))
}
