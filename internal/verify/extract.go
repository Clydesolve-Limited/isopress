package verify

// T4.3: the extraction-and-hash primitive (DESIGN_SPEC 7.2, 7.1
// image-mode half; IMPLEMENTATION_PLAN T4.3 "The extraction-and-hash
// primitive is created here as the first content of internal/verify and
// is completed by T5.1").
//
// T5.1 COMPLETION: verify-media reuses this primitive through
// ExtractAndHashPhases (caller-chosen phase names - the media run
// never renders "build(...)" labels), the walk gained the 7.2
// UNREADABLE class (Entry.Unreadable: a file whose bytes cannot be
// read is recorded and the walk continues; cancellation still aborts),
// and the comparison moved into this package (compare.go) so both
// verifications - build self-verify and verify-media - share ONE
// comparison over these entries.
//
// ExtractAndHash re-reads an ISO image WITHOUT mounting it: one xorriso
// osirrox invocation extracts the payload tree to a caller-supplied
// directory, then Go walks the extraction and SHA-256-hashes every
// regular file. The comparison against the catalog's finalized content
// list (image_contents) is the CALLER's job (T4.3 build self-verify,
// T5.1 verify-media): this primitive owns extraction plus hashing and
// returns the entries, nothing more.
//
// # Progress: the four-phase build model's self-verify half (6.2,
// 9.1/9.3; the 2026-10-20 "looks hung" amendment)
//
// ExtractAndHash emits the progress events of the two self-verify
// phases through the caller-supplied Bus (nil-safe - the established
// convention; a nil Bus means no events at all):
//
//   - build(extract): a Phase event with the EXACT payload byte total
//     from the content list (Totals.Bytes), then a tree-sum
//     byte-watcher (watch.go) that emits BytesWritten as xorriso
//     osirrox writes the extraction - a multi-minute write on real
//     images. Honest fallback (6.2): when the content-list entry count
//     exceeds maxTreeSumEntries (a pathological file count that would
//     make the per-poll tree-sum too costly), the phase is declared
//     INDETERMINATE (TotalKnown false) up front and the watcher emits
//     Tick heartbeats instead of byte positions - an indeterminate
//     spinner, still non-silent.
//   - build(verify): a Phase event with EXACT TotalFiles/TotalBytes
//     (Totals.Files/Totals.Bytes) emitted BEFORE the hash walk starts -
//     unlike scan(hash), whose totals arrive mid-phase, these exist up
//     front (the content list is finalized), so the bar is determinate
//     with ETA from the first entry - then one ItemDone per re-hashed
//     regular file.
//
// Totals are FIRST-CLASS PARAMETERS (T5.1 note): verify-media passes
// the same content-list totals when it reuses this primitive; a caller
// that wants the pre-amendment silent behavior passes a nil Bus and the
// zero Totals (with a nil Bus the zero totals are never rendered). The
// phase names default to the build command's (9.3's pinned line
// formats); T5.1's verify-media passes its OWN names through
// ExtractAndHashPhases, so verify-media never renders "build(...)"
// labels it does not run.
//
// # The exec seam: *xorriso.Tool, never os/exec (T4.1)
//
// The xorriso invocation goes through the T4.1 wrapper - this package
// takes a *xorriso.Tool and calls Tool.Run with a direct argv vector.
// os/exec is imported ONLY by internal/xorriso (the T4.1 deps test
// asserts it repo-wide); internal/verify never spawns a process itself.
//
// # The extract shape: ONE `-extract /` of the whole tree (documented
// deviation from 7.2's V1 default pair, with the empirical reasons)
//
// DESIGN_SPEC 7.2 sketches the verify-media default as "extract /files
// and /chunks wholesale, which is a single pair of commands". This
// primitive - which serves the BUILD SELF-VERIFY first (6.3 step 2,
// 7.1 "verified by extraction-based re-hash before the .partial is
// renamed") - deliberately extracts the image ROOT ("/") with a single
// -extract command instead, for three empirically verified reasons
// (real xorriso 1.5.6, probed on the dev machine 2026-10-20):
//
//  1. The self-verify must cover the WHOLE tree, including the two
//     image-root members MANIFEST.json and catalog-export.sqlite (the
//     6.6 content list records them, and 6.3 step 2 hashes "every file
//     and chunk" of the planned list against the extraction). The
//     /files + /chunks pair does not extract the image root at all.
//  2. The /files + /chunks pair HARD-FAILS on an image that has no
//     chunks tree (the common small-image case): xorriso answers
//     "FAILURE : Cannot determine attributes of (ISO) source file
//     '/chunks' : No such file or directory" and exits 5 - a build
//     self-verify of a perfectly valid chunkless image would fail.
//  3. A single `-extract / <dir>` invocation keeps the one property
//     7.2 actually cares about: ONE xorriso invocation per image
//     regardless of file count.
//
// T5.1's verify-media follows this SAME single `-extract /` shape
// (the WORK_LOG carry-in ruling: the /files+/chunks pair hard-fails on
// chunkless images and misses the root members, so the whole-tree
// extraction is the verify-media shape too).
//
// # Temp-dir management: CALLER-SUPPLIED (the build phase owns
// lifecycle)
//
// destDir is caller-supplied and must EXIST and be EMPTY (xorriso
// extraction refuses nothing about stale files - a non-empty dest
// would silently mix old members into the hashed tree and misreport
// "unexpected" entries; the emptiness gate turns that into a loud
// wiring error). The caller creates a fresh os.MkdirTemp under its
// --work-dir and removes it after the comparison - the build phase
// owns the lifecycle because it also owns the staging workspace and
// the .partial image (DESIGN_SPEC 6.2 --work-dir semantics). This
// package never creates or deletes the directory itself, so a future
// caller can keep the extraction for inspection on failure.
//
// # What the walk returns
//
// One Entry per extracted member, keyed by the media path relative to
// destDir ("files/a/b.txt", "chunks/x/00001", "MANIFEST.json",
// "catalog-export.sqlite" - the 5.3 logical tree): regular files carry
// the streaming SHA-256 and byte size, symlinks carry the restored
// target (osirrox restores symlinks as symlinks - verified), dirs are
// recorded by existence. Metadata (mode/uid/gid) is deliberately NOT
// compared here: extraction as an unprivileged user cannot chown, and
// 7.2's anchor is content hashing, not metadata (restore re-applies
// metadata from the manifest). A member of any other type (a device
// socket, ... - impossible in a xorriso-produced ISO) is a loud error,
// never a silent skip.
//
// Cancellation: ctx is honored by the xorriso run (Tool.Run kills the
// process) and by the hashing walk (each streamed read checks ctx).

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"isopress/internal/progress"
	"isopress/internal/xorriso"
)

// The member kinds an extraction can produce (the 2.3 image_contents
// kinds minus chunk/manifest/export, which are DERIVED distinctions of
// the content list, not filesystem types: a chunk is a regular file, a
// manifest/export is a regular file - the caller's comparison maps
// them onto these).
const (
	KindFile    = "file"
	KindDir     = "dir"
	KindSymlink = "symlink"
)

// The phase names ExtractAndHash emits by default (DESIGN_SPEC 6.2's
// four-phase build progress; 9.3 pins the plain-line formats keyed on
// these names). T5.1's verify-media reuses the primitive through
// ExtractAndHashPhases and passes its OWN names, so a verify-media run
// never renders "build(...)" labels.
const (
	PhaseExtract = "build(extract)"
	PhaseVerify  = "build(verify)"
)

// PhaseNames is the caller-chosen pair of progress phase names one
// extract+hash run emits (T5.1's generalization seam: verify-media
// passes verify(extract)/verify(hash) so its progress lines name the
// command actually running). The zero value means the build defaults.
type PhaseNames struct {
	Extract string
	Verify  string
}

// withDefaults fills the build's phase names into the unset fields.
func (p PhaseNames) withDefaults() PhaseNames {
	if p.Extract == "" {
		p.Extract = PhaseExtract
	}
	if p.Verify == "" {
		p.Verify = PhaseVerify
	}
	return p
}

// Totals are the EXACT content-list figures (DESIGN_SPEC 6.6
// image_contents) the caller derives BEFORE the run and hands to
// ExtractAndHash so both self-verify phases open DETERMINATE (6.2;
// first-class parameters per the 2026-10-20 four-phase amendment -
// T5.1's verify-media passes the same totals later).
//
// Judgment call (documented): the build(extract) byte total is the
// summed size of the image's REGULAR-FILE rows (Bytes), not of every
// row - the `-extract /` tree-sum converges exactly on regular-file
// bytes (symlinks materialize as symlinks and dirs add no bytes), so
// this figure is the one that reaches 100% precisely when the
// extraction completes. Counting symlink-target "sizes" (the 2.3
// convention) would push the total above what any tree-sum can reach
// and stall the bar artificially.
type Totals struct {
	// Entries is the content-list row count: the build(extract)
	// phase's file total AND the tree-sum cost proxy the
	// pathological fallback gate (maxTreeSumEntries) compares against
	// (the extracted tree's entry count tracks the content list plus
	// the 5.3 tree's implied parents).
	Entries int64
	// Files is the number of REGULAR-FILE rows the re-hash walk
	// re-hashes (file/chunk/manifest/export rows; symlinks and dirs
	// are not re-hashed): the build(verify) file total.
	Files int64
	// Bytes is the summed size of those regular-file rows: exactly
	// what the extraction tree converges to and exactly what the hash
	// walk's ItemDone bytes sum to - the byte total of BOTH
	// self-verify phases.
	Bytes int64
}

// Entry is one extracted member of the image tree.
type Entry struct {
	// MediaPath is the member's path relative to the extraction dir,
	// slash-separated: "files/a.txt", "chunks/x/00001",
	// "MANIFEST.json" (the 5.3 logical tree rooted at the image root).
	MediaPath string
	// Kind is KindFile | KindDir | KindSymlink.
	Kind string
	// Size is the byte size (regular files); len(target) for symlinks
	// (the 2.3 convention); 0 for dirs. For an unreadable regular
	// file it is the LSTAT size when the walk could see it.
	Size int64
	// Hash is the streaming SHA-256 of a regular file; nil for
	// symlinks and dirs (the catalog NULL-hash convention, D-09).
	Hash []byte
	// LinkTarget is the restored symlink target; empty otherwise.
	LinkTarget string
	// Unreadable is non-empty when the member is a regular file whose
	// bytes could not be read/hashed (open or read failure: EACCES,
	// I/O error). The walk RECORDS the failure on the entry instead
	// of aborting (T5.1, the 7.2 unreadable class) so one bad member
	// never hides the verdicts of every other member; Hash stays nil
	// and the comparison classifies the entry as unreadable, never as
	// a hash mismatch. A CANCELLED context is still a hard walk
	// abort, not an unreadable entry (cancellation is not a media
	// verdict).
	Unreadable string
}

// xorrisoDiagTailBytes bounds the extraction transcript tail retained
// on a SUCCESSFUL run for the diagnosis paths (8 KiB, the
// xorriso.errorOutputPrintLimit precedent: xorriso prints its
// diagnostics at the END of a run, so the tail is the part a failure
// message needs; the full capped transcript of a FAILED run already
// travels on *xorriso.BuildError and is not duplicated here).
const xorrisoDiagTailBytes = 8 << 10

// ExtractResult is the hashed content of one extraction.
type ExtractResult struct {
	// Dir is the extraction directory (the caller's destDir, echoed
	// for error messages).
	Dir string
	// Entries are every extracted member, sorted by MediaPath.
	Entries []Entry
	// FileCount is the number of regular files hashed.
	FileCount int
	// TotalBytes is the summed size of the hashed regular files (the
	// 7.2 free-space figure a future caller pre-checks).
	TotalBytes int64
	// ToolOutput is the bounded TAIL (xorrisoDiagTailBytes) of the
	// extraction invocation's combined stdout+stderr, retained ONLY
	// on a run that EXITED 0 - a non-zero run already carries its
	// full transcript on *xorriso.BuildError. It exists for ONE
	// honest-diagnosis path: an extraction that "succeeded" yet
	// produced nothing. xorriso 1.5.6 answers an UNPARSEABLE ISO
	// with "No ISO 9660 image at LBA 0 / Creating blank image" on
	// stderr, EXITS 0, and extracts an EMPTY tree - so without this
	// tail a caller would report every listed member as missing and
	// never mention that the image itself could not be read
	// (verify-media's zero-entry gate, verifymedia.go). Callers that
	// do not need the diagnosis ignore the field.
	ToolOutput []byte
}

// ExtractAndHash extracts the whole tree of the ISO at imagePath into
// destDir (one xorriso osirrox invocation - see the file doc for the
// `-extract /` shape) and hashes every regular file. destDir must exist
// and be empty; the caller owns its lifecycle. On success the result
// carries every member PLUS the bounded success-transcript tail
// (ExtractResult.ToolOutput) a caller needs to diagnose an exit-0
// extraction that produced nothing; any xorriso failure (non-zero exit
// - a typed *xorriso.BuildError - or an exec failure) and any walk
// failure is returned as a plain wrapped error.
//
// Progress (see the file doc): bus may be nil (no events); with a bus,
// the build(extract) Phase + tree-sum watcher run around the xorriso
// extraction and the build(verify) Phase is emitted BEFORE the hash
// walk starts, followed by one ItemDone per re-hashed regular file.
// totals carries the caller's exact content-list figures (first-class
// parameters, T5.1).
func ExtractAndHash(ctx context.Context, tool *xorriso.Tool, imagePath, destDir string, bus *progress.Bus, totals Totals) (ExtractResult, error) {
	return ExtractAndHashPhases(ctx, tool, imagePath, destDir, bus, totals, PhaseNames{})
}

// ExtractAndHashPhases is ExtractAndHash with caller-chosen progress
// phase names (the T5.1 generalization seam): verify-media passes
// PhaseNames{Extract: "verify(extract)", Verify: "verify(hash)"} so
// its progress lines name the command actually running, while the
// build command keeps the 6.2 four-phase names through the
// ExtractAndHash wrapper. Everything else - the single `-extract /`
// invocation, the emptiness gate, the tree-sum watcher, the walk - is
// identical.
func ExtractAndHashPhases(ctx context.Context, tool *xorriso.Tool, imagePath, destDir string, bus *progress.Bus, totals Totals, phases PhaseNames) (ExtractResult, error) {
	phases = phases.withDefaults()
	if tool == nil || tool.Path == "" {
		return ExtractResult{}, fmt.Errorf("verify: nil xorriso tool (call Discover before ExtractAndHash)")
	}
	if imagePath == "" {
		return ExtractResult{}, fmt.Errorf("verify: empty image path")
	}
	if destDir == "" {
		return ExtractResult{}, fmt.Errorf("verify: empty extraction directory")
	}
	// The emptiness gate (file doc): a non-empty dest would silently
	// mix stale members into the hashed tree.
	stale, err := os.ReadDir(destDir)
	if err != nil {
		return ExtractResult{}, fmt.Errorf("verify: checking extraction directory %s: %w", destDir, err)
	}
	if len(stale) > 0 {
		return ExtractResult{}, fmt.Errorf(
			"verify: extraction directory %s is not empty (%d entries); pass a fresh empty directory (the caller creates it with os.MkdirTemp)",
			destDir, len(stale))
	}

	// The extract phase: the determinate byte phase over the EXACT
	// payload total (6.2), with the tree-sum watcher live while xorriso
	// writes. The pathological-fallback gate (watch.go) is decided UP
	// FRONT from the content-list entry count - the tree-sum's cost
	// proxy - so a degraded phase is honestly indeterminate from its
	// first frame instead of flickering determinate-then-spinner.
	determinate := totals.Entries <= maxTreeSumEntries
	if bus != nil {
		bus.Emit(progress.Phase{
			Name:       phases.Extract,
			TotalKnown: determinate,
			TotalFiles: totals.Entries,
			TotalBytes: totals.Bytes,
		})
	}
	stopWatch := watchExtraction(ctx, destDir, busEmit(bus), extractPollInterval, determinate, sumExtractionTree)

	// ONE invocation, whole tree (see the file doc): -indev opens the
	// image read-only, -osirrox on enables extraction, a single
	// -extract copies the ISO root into destDir. Every element is one
	// argv entry (no shell, T4.1).
	argv := []string{"-indev", imagePath, "-osirrox", "on", "-extract", "/", destDir}
	res, runErr := tool.Run(ctx, argv)
	stopWatch() // the watcher lives exactly as long as the extraction (no leak on any Run exit path)
	if runErr != nil {
		return ExtractResult{}, fmt.Errorf("verify: xorriso extraction of %s: %w", imagePath, runErr)
	}
	// The SUCCESS transcript tail is retained for the diagnosis paths
	// only (see ExtractResult.ToolOutput): an exit-0 run that wrote
	// nothing is a READ failure of the image, and the caller explains
	// it with xorriso's own words instead of inventing one.
	diagTail := xorrisoOutputTail(res.Output)

	// The verify phase: EXACT totals BEFORE the walk starts (6.2 -
	// unlike scan(hash), whose totals arrive mid-phase, these exist up
	// front), then one ItemDone per re-hashed entry inside hashTree.
	if bus != nil {
		bus.Emit(progress.Phase{
			Name:       phases.Verify,
			TotalKnown: true,
			TotalFiles: totals.Files,
			TotalBytes: totals.Bytes,
		})
	}

	result, err := hashTree(ctx, destDir, bus)
	if err != nil {
		return ExtractResult{}, err
	}
	result.ToolOutput = diagTail
	return result, nil
}

// xorrisoOutputTail returns the last xorrisoDiagTailBytes bytes of out
// (the bounded success-transcript tail retained for the zero-entry
// diagnosis path; xorriso prints diagnostics at the end of a run, so
// the tail is the informative half).
func xorrisoOutputTail(out []byte) []byte {
	if len(out) <= xorrisoDiagTailBytes {
		return out
	}
	return out[len(out)-xorrisoDiagTailBytes:]
}

// hashTree walks the extraction directory and hashes every regular
// file (streaming SHA-256), records symlink targets, and classifies
// every member. Media paths are relative to root, slash-separated.
// With a non-nil bus, every regular FILE emits one ItemDone (the
// per-entry progress; symlinks and dirs are not re-hashed and emit
// none - the copyengine payload-member convention; an UNREADABLE file
// also emits one, because the walk processed it - see Entry.Unreadable).
//
// A file whose bytes cannot be read is RECORDED as an unreadable entry
// and the walk continues (the 7.2 unreadable class, T5.1); a cancelled
// context still aborts the whole walk (cancellation is not a media
// verdict), and a member of any other type (device, socket, FIFO) is
// still a loud error, never a silent skip.
func hashTree(ctx context.Context, root string, bus *progress.Bus) (ExtractResult, error) {
	out := ExtractResult{Dir: root}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("verify: walking extraction tree at %s: %w", path, walkErr)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("verify: relative path of %s under %s: %w", path, root, err)
		}
		mediaPath := filepath.ToSlash(rel)

		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("verify: reading symlink %s: %w", mediaPath, err)
			}
			out.Entries = append(out.Entries, Entry{
				MediaPath: mediaPath, Kind: KindSymlink,
				Size: int64(len(target)), LinkTarget: target,
			})
			return nil
		case d.IsDir():
			out.Entries = append(out.Entries, Entry{MediaPath: mediaPath, Kind: KindDir})
			return nil
		case d.Type().IsRegular():
			hash, size, err := hashExtractedFile(ctx, path)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					// Cancellation is not a media verdict: abort the
					// whole walk (the pinned cancelled-context contract).
					return ctxErr
				}
				// UNREADABLE member (7.2's class, T5.1): record the
				// read failure on the entry and keep walking - one
				// unreadable member must never hide the verdicts of
				// every other member. Size is the LSTAT size when the
				// walk could see it, so the ItemDone below still
				// advances the determinate bar honestly (the walk DID
				// process the entry; its bytes simply could not be
				// read).
				var statSize int64
				if info, ierr := d.Info(); ierr == nil {
					statSize = info.Size()
				}
				out.Entries = append(out.Entries, Entry{
					MediaPath: mediaPath, Kind: KindFile, Size: statSize,
					Unreadable: err.Error(),
				})
				if bus != nil {
					bus.Emit(progress.ItemDone{Bytes: statSize, Path: mediaPath})
				}
				return nil
			}
			out.Entries = append(out.Entries, Entry{
				MediaPath: mediaPath, Kind: KindFile, Size: size, Hash: hash,
			})
			out.FileCount++
			out.TotalBytes += size
			if bus != nil {
				// The build(verify) per-entry progress (6.2): one
				// ItemDone per RE-HASHED entry.
				bus.Emit(progress.ItemDone{Bytes: size, Path: mediaPath})
			}
			return nil
		default:
			// A device, socket, or FIFO cannot come out of a xorriso
			// ISO image; anything here is corruption or a foreign tree
			// at destDir - refuse loudly rather than skip silently.
			return fmt.Errorf("verify: extracted member %s has unexpected type %v (only files, dirs, and symlinks can come out of an ISO)",
				mediaPath, d.Type())
		}
	})
	if err != nil {
		return ExtractResult{}, err
	}
	sort.Slice(out.Entries, func(i, j int) bool { return out.Entries[i].MediaPath < out.Entries[j].MediaPath })
	return out, nil
}

// hashExtractedFile streams one extracted file through SHA-256
// (ctx-aware: a cancelled run must not sit inside a multi-gigabyte
// read) and returns the digest plus the byte count read.
func hashExtractedFile(ctx context.Context, path string) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("verify: opening extracted file %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only close; the hash result dominates

	h := sha256.New()
	n, err := io.Copy(h, &verifyCtxReader{ctx: ctx, r: f})
	if err != nil {
		return nil, 0, fmt.Errorf("verify: hashing extracted file %s: %w", path, err)
	}
	return h.Sum(nil), n, nil
}

// verifyCtxReader aborts a long hash stream the moment the context is
// cancelled (the stageCtxReader shape of internal/imagebuild).
type verifyCtxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *verifyCtxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
