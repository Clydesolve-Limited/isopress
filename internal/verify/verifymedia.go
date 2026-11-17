package verify

// T5.1: the verify-media SERVICE (DESIGN_SPEC 7.2, FR-021;
// IMPLEMENTATION_PLAN 4.6 T5.1). VerifyImage re-reads ONE built image
// and compares it - hash-first, mount-free - against the catalog's
// finalized content list (image_contents), through ONE comparison for
// both source modes:
//
//   - ISO (images.final_path names a .iso file): a unique-scratch
//     extraction dir under --work-dir (the T3.2/T4.3 scratch
//     convention), the 7.2 free-space PRE-CHECK (Statfs seam; required
//     ~= the payload size; refusal is up-front, BEFORE xorriso runs),
//     then ONE xorriso osirrox invocation via the T4.1 Tool
//     (-indev <image> -osirrox on -extract / <scratch> - the T4.3
//     whole-tree shape; see extract.go for why the /files+/chunks pair
//     is not used), then the hash walk over the extraction.
//   - COPY TREE (images.final_path names the S-NNN directory): the
//     SAME hash walk runs DIRECTLY over the tree - no xorriso, no
//     temp, no extraction (7.2: "Copy-tree verification reads the tree
//     directly (no extraction), same comparison logic").
//
// The entries of either walk feed CompareContents against the same
// content list with the catalog hashes as the anchor, so missing AND
// unexpected payloads are reported symmetrically in both modes and the
// osirrox symlink contract (links restored AS links, dangling
// faithful, walk never follows) is honored by construction: symlinks
// match by TARGET in both modes.
//
// # Reading an unreadable image as a READ failure (the zero-entry gate)
//
// xorriso 1.5.6 does not always FAIL an unparseable image: on an ISO
// whose volume descriptors are destroyed it prints "No ISO 9660 image
// at LBA 0 / Creating blank image" to stderr, EXITS 0, and extracts an
// EMPTY tree - so Tool.Run answers with no error and (per the exit-0
// contract) keeps both streams out of the message. Answering that with
// "every listed member is missing" would be the RIGHT verdict with a
// WRONG diagnosis (the payloads were never even read - it is the IMAGE
// that could not be read). readISO therefore gates it: an extraction that
// yielded ZERO entries against a NON-EMPTY content list is a READ
// FAILURE of the image - a per-image error naming the image
// unreadable/not an ISO 9660 image and carrying the xorriso output
// tail (ExtractResult.ToolOutput, retained for exactly this path) - and
// never N missing payloads. An extraction that yields SOME entries is
// compared normally: the zero-vs-non-empty gate is the whole rule.
//
// # Stamping (7.2: "On full match, the image row gets verified_at and
// state verified") and the documented state policy
//
//   - FULL match (Comparison.Clean): deps.Store.StampVerified writes
//     images.verified_at + state 'verified' in ONE small transaction,
//     and the result reports Stamped=true with the stamp value.
//     The stamp applies from WHATEVER state the row carried: a build
//     image reaches it from 'built', a copy image from 'built' too
//     (since T6.5 the copy completion stamps mode='copy' +
//     state='built' through catalog.RecordCopyFinalPath; rows written
//     before T6.5 still carry 'planned' and heal exactly the same
//     way), and a crash-window image from 'building'/'failed' - which
//     makes a full verify-media the 6.3 crash reconciliation for the
//     stale-row-over-valid-.iso case build.go's file doc assigns to
//     T5.1.
//   - PARTIAL match or any anomaly: the state is left UNCHANGED. This
//     is a DOCUMENTED choice: a partial verify does NOT downgrade
//     'built' (or 'verified') to 'failed' - the media verdict belongs
//     to the run's REPORT, not to a state transition; state is
//     write-side provenance (who published the bytes), and the report
//     is the honest record of what a later read found. Consequence
//     (documented): a re-verify of media corrupted AFTER a clean
//     verify leaves the previous 'verified' state in place while the
//     new report says otherwise; consumers must read the report, and
//     the next full-match verify re-stamps verified_at.
//   - FULL match whose STAMP WRITE FAILS: NON-FATAL (DESIGN_SPEC 2.1,
//     Clarified 2026-10-21: "a stamp that ever exceeded busy_timeout is
//     non-fatal: the content result still reports verified, and a later
//     verify re-stamps" - the same shape for any stamp write failure,
//     because the stamp is re-derivable and the CONTENT verdict is what
//     this service was asked to produce). The Comparison the run just
//     computed SHIPS intact (counts and per-file lines), Stamped stays
//     FALSE so no stamp is ever claimed, and the failure surfaces as a
//     WARNING: progress.Warn on the bus plus ImageVerifyResult.StampWarning
//     for the report - NOT an anomaly, NOT a missing/mismatched
//     classification, and NOT a failed image (the media content is
//     fine; only the catalog stamp failed). Consequently the image's
//     status stays 'verified' and a clean-but-unstamped run exits 0
//     (the metadata-warning precedent: warnings never create exit 10;
//     a real anomaly beside it still carries its own exit 10). The row
//     was never written, so a LATER verify re-stamps it. The
//     never-exit-5 half of the ruling holds structurally: this path
//     cannot produce the catalog-busy class (the cli's stamping locker
//     is a no-op one and SQLITE_BUSY is never typed as CatalogBusy).
//
// # READ-ONLY contract (2.1 Option B) and the stamping connection
//
// The service NEVER opens a catalog handle itself: reads go through
// deps.Store (the cli wires it onto the READ-ONLY, no-flock catalog
// open, so verify-media runs DURING a copy/build run over a WAL
// snapshot), and the single stamp on full match is the service's only
// write. The cli supplies a separate, deliberately lock-free
// read-write handle for that one transaction (see internal/cli/
// verifymedia.go for the 2.1-vs-7.2 tension ruling); a partial verify
// performs ZERO catalog writes.
//
// # What this service deliberately does NOT do
//
//   - No runs row and no run_events (the read-only open cannot write;
//     the stdout report is the record - documented consequence of the
//     2.1 read-only policy for this command).
//   - No -check_media / -check_md5_r invocation (7.2: xorriso's own
//     recorded-MD5 scans operate on data the mastering step recorded;
//     the catalog SHA-256 anchor is strictly stronger and
//     format-independent, so the Go re-hash IS the V1 verification;
//     a physical-layer readability scan for burned media is documented
//     as a future supplement in the command help).
//   - No metadata comparison (mode/uid/gid): the anchor is content
//     (7.2), same as the build self-verify.
//
// Cleanup: the extraction scratch is removed on EVERY path (success,
// mismatch, xorriso failure, the zero-entry unreadable-image failure,
// walk failure, cancellation) - the caller keeps the media verdict in
// the result, never a temp tree. Cancellation is honored by the xorriso
// run and the hash walk, and a cancelled run NEVER stamps (the walk
// aborts, so no full match can be reached).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"time"

	"isopress/internal/progress"
	"isopress/internal/xorriso"
)

// The two source modes of 7.2, as reported in ImageVerifyResult.Source
// (resolved from images.final_path: a regular file is an ISO, a
// directory is a copy tree - the 2.3 column comment "copy dir or .iso
// path once built").
const (
	SourceISO      = "iso"
	SourceCopyTree = "copy"
)

// The verify-media progress phase names (the T5.1 names for the two
// phases ExtractAndHashPhases emits around an ISO re-read, plus the
// single hash phase a direct copy-tree read emits - 9.3's plain-line
// formats key on these strings, so verify-media never renders
// "build(...)" labels it does not run).
const (
	PhaseMediaExtract = "verify(extract)"
	PhaseMediaHash    = "verify(hash)"
)

const (
	// verifyScratchPrefix is the os.MkdirTemp pattern of the
	// extraction scratch under --work-dir. It carries no user data
	// (set ids are user-controlled), exactly like the build
	// staging/extract prefixes.
	verifyScratchPrefix = "isopress-verify-media-"
	// extractSlack is the free-space cushion added to the 7.2
	// pre-check requirement (directory entries, xorriso scratch).
	extractSlack = 16 << 20 // 16 MiB
)

// FreeSpaceFunc reports the unprivileged available bytes on the
// filesystem holding path (7.2: "the command pre-checks via Statfs on
// the temp root"). Default: statfsFreeSpace (stdlib syscall, the
// imagebuild precedent); tests inject a seam.
type FreeSpaceFunc func(path string) (availBytes uint64, err error)

// statfsFreeSpace is the production FreeSpaceFunc.
func statfsFreeSpace(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %q: %w", path, err)
	}
	return st.Bavail * uint64(st.Bsize), nil
}

// ContentList is one image's finalized content list plus the symlink
// targets the comparison needs (the image_contents row carries the
// target only through its files row - the builder reads its inventory,
// the read side joins files.link_target).
type ContentList struct {
	Rows []ContentRow
	// SymlinkTargets maps media path -> recorded target for the
	// listed symlink rows; empty entries never match a non-empty
	// target, so a missing target is a loud mismatch, never a silent
	// pass.
	SymlinkTargets map[string]string
}

// VerifyStore is the catalog seam of verify-media (the copyengine /
// imagebuild Store shape: primitive signatures, so the catalog
// package never imports this one and the cli adapts *catalog.DB onto
// it).
type VerifyStore interface {
	// ImageContents returns the image's finalized content list (the
	// 6.6 rows) with symlink targets resolved and the catalog
	// hashes as the anchor. An empty list is refused LOUDLY by the
	// service (nothing finalized can be verified).
	ImageContents(ctx context.Context, setID string, imageID int64) (ContentList, error)
	// StampVerified writes images.verified_at + state 'verified'
	// for a FULL match, one small transaction (7.2). The service
	// calls it exactly once per fully-matched image and never
	// otherwise. A failure of that one call is NON-FATAL (the 2.1
	// carve-out above): the content verdict still ships with
	// Stamped=false and a warning instead of an error.
	StampVerified(ctx context.Context, setID string, imageID int64, verifiedAt int64) error
}

// Deps are the service dependencies: the catalog seam (required), the
// xorriso Tool (required for ISO images, IGNORED for copy trees -
// copy-tree verification needs no xorriso), the progress Bus (nil-safe:
// no events), the free-space seam (default Statfs), and the stamping
// clock (default time.Now).
type Deps struct {
	Store     VerifyStore
	Tool      *xorriso.Tool
	Bus       *progress.Bus
	FreeSpace FreeSpaceFunc
	Now       func() time.Time
}

// VerifyInput is one image to verify: the catalog identity plus the
// RESOLVED location (images.final_path - the cli reads the row and
// hands the path over, so the service never opens a catalog itself)
// and the --work-dir scratch parent (ISO mode only; "" is the OS temp
// directory, 6.2).
type VerifyInput struct {
	SetID   string
	ImageID int64
	Path    string
	WorkDir string
}

// ImageVerifyResult is the verdict on ONE image. It is NOT an error
// when anomalies are found - mismatches are data, reported through the
// Comparison (the cli turns any anomaly into exit 10 per the validate
// precedent); the returned error is reserved for wiring faults,
// unusable locations, and extraction/walk failures - a stamp write
// failure is NOT one of them (it is the non-fatal StampWarning below,
// per the 2.1 carve-out in the file doc).
type ImageVerifyResult struct {
	SetID   string
	ImageID int64
	// Path is the verified location (the images.final_path value).
	Path string
	// Source is SourceISO | SourceCopyTree (resolved by stat).
	Source string
	// Rows is the content-list row count the comparison covered.
	Rows int
	// Comparison carries the per-category findings (matched /
	// mismatched / missing / unexpected / unreadable). On a stamp
	// failure it is COMPLETE and authoritative: the content verdict
	// this run computed ships exactly as rendered.
	Comparison Comparison
	// Stamped reports that the 7.2 full-match stamp (verified_at +
	// state 'verified') was written; StampAt is its nanosecond value.
	// It is true only on a full match AND only after the stamp
	// transaction committed. A full match whose stamp FAILED reports
	// Stamped=false with StampWarning set - never a claimed stamp.
	Stamped bool
	StampAt int64
	// StampWarning is non-empty only when the content verdict was
	// CLEAN but the 7.2 stamp transaction failed (a refused write, a
	// busy_timeout exhausted against a competing writer): it names
	// the stamp failure and states that the content verdict stands
	// and a later verify re-stamps. It is a WARNING, not an error
	// and not an anomaly - the service emits it as progress.Warn on
	// Deps.Bus and the cli records it as a report warning, leaving
	// the image's status and the run's exit code alone (the 2.1
	// carve-out; the file doc spells out the exit consequence).
	StampWarning string
}

// VerifyImage re-reads ONE image and compares it against its finalized
// content list (see the file doc for the modes, the stamping policy,
// and the read-only contract).
func VerifyImage(ctx context.Context, deps Deps, in VerifyInput) (ImageVerifyResult, error) {
	res := ImageVerifyResult{SetID: in.SetID, ImageID: in.ImageID, Path: in.Path}
	switch {
	case in.SetID == "":
		return res, fmt.Errorf("verify: empty set id")
	case in.Path == "":
		return res, fmt.Errorf("verify: image %d of set %q has no location to verify (images.final_path is empty)",
			in.ImageID, in.SetID)
	case deps.Store == nil:
		return res, fmt.Errorf("verify: nil Store (the catalog seam is required)")
	}
	// F4-style entry gate: a cancelled run verifies nothing (and
	// stamps nothing - see the file doc).
	if err := ctx.Err(); err != nil {
		return res, err
	}

	// Source mode: the 2.3 column contract in reverse - a regular
	// file is the .iso, a directory is the copy tree.
	fi, err := os.Stat(in.Path)
	if err != nil {
		return res, fmt.Errorf("verify: image %d of set %q: %s: %w", in.ImageID, in.SetID, in.Path, err)
	}
	source := SourceCopyTree
	if fi.Mode().IsRegular() {
		source = SourceISO
	} else if !fi.IsDir() {
		return res, fmt.Errorf("verify: image %d of set %q: %s is neither a regular file (.iso) nor a directory (copy tree): %v",
			in.ImageID, in.SetID, in.Path, fi.Mode())
	}
	res.Source = source

	// The finalized content list (the anchor): 6.6 image_contents
	// with the catalog hashes and symlink targets resolved by the
	// store seam.
	list, err := deps.Store.ImageContents(ctx, in.SetID, in.ImageID)
	if err != nil {
		return res, err
	}
	if len(list.Rows) == 0 {
		return res, fmt.Errorf(
			"verify: image %d of set %q has no finalized content list (nothing was ever written to it - run copy or build first)",
			in.ImageID, in.SetID)
	}
	res.Rows = len(list.Rows)
	totals := TotalsOf(list.Rows)

	var entries []Entry
	switch source {
	case SourceISO:
		entries, err = readISO(ctx, deps, in, totals)
	default:
		entries, err = readCopyTree(ctx, deps, in, totals)
	}
	if err != nil {
		return res, err
	}

	res.Comparison = CompareContents(list.Rows, entries, list.SymlinkTargets)
	if !res.Comparison.Clean() {
		// Documented state policy (file doc): a partial match leaves
		// the image row UNCHANGED - no stamp, no downgrade.
		return res, nil
	}

	// FULL match: the 7.2 stamp, one small transaction.
	nowFn := deps.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	now := nowFn()
	if err := deps.Store.StampVerified(ctx, in.SetID, in.ImageID, now.UnixNano()); err != nil {
		// THE 2.1 CARVE-OUT (Clarified 2026-10-21), implemented: a
		// stamp write that fails - a refused UPDATE, a busy_timeout
		// exhausted against a competing writer - is NON-FATAL. The
		// content verdict this run computed SHIPS (Comparison above
		// stays intact, so the cli renders the full counts and the
		// per-file lines), Stamped stays false so no stamp is ever
		// claimed, and the failure becomes a WARNING: progress.Warn
		// here (nil-safe Bus) plus StampWarning for the report. It is
		// deliberately NOT an error return: that would discard an
		// already-computed verdict and fail an image whose media is
		// provably fine - the deviation QA proved empirically. The
		// row was never written, so a LATER verify re-stamps it.
		res.StampWarning = fmt.Sprintf(
			"verify: image %d of set %q matched its content list fully but the verified_at/state stamp failed: %v (the content verdict stands; a later verify re-stamps)",
			in.ImageID, in.SetID, err)
		if deps.Bus != nil {
			deps.Bus.Emit(progress.Warn{Msg: res.StampWarning})
		}
		return res, nil
	}
	res.Stamped = true
	res.StampAt = now.UnixNano()
	return res, nil
}

// readISO runs the 7.2 ISO re-read: scratch + free-space pre-check +
// ONE xorriso osirrox extraction + the hash walk over it, ending with
// the zero-entry read-failure gate (file doc: an exit-0 extraction that
// produced nothing means the IMAGE is unreadable, not that every
// payload is missing). The scratch is removed on EVERY path.
func readISO(ctx context.Context, deps Deps, in VerifyInput, totals Totals) ([]Entry, error) {
	if deps.Tool == nil || deps.Tool.Path == "" {
		return nil, fmt.Errorf(
			"verify: image %d of set %q is an ISO and needs xorriso to re-read it (no tool wired - the cli discovers it from PATH; exit 3 when absent)",
			in.ImageID, in.SetID)
	}
	dir, err := os.MkdirTemp(in.WorkDir, verifyScratchPrefix)
	if err != nil {
		return nil, fmt.Errorf("verify: creating the extraction scratch for image %d of set %q: %w",
			in.ImageID, in.SetID, err)
	}
	defer os.RemoveAll(dir) //nolint:errcheck // scratch; the result carries the verdict, not the tree

	// THE 7.2 FREE-SPACE PRE-CHECK: extraction requires free space up
	// to the payload size (plus the directory-entry cushion), and the
	// refusal happens BEFORE xorriso is ever invoked - a
	// space-starved host fails up front with the requirement stated,
	// never mid-extraction with a half-written temp tree.
	free := deps.FreeSpace
	if free == nil {
		free = statfsFreeSpace
	}
	avail, err := free(dir)
	if err != nil {
		return nil, fmt.Errorf("verify: free-space pre-check of the extraction scratch: %w", err)
	}
	if uint64(totals.Bytes)+extractSlack > avail {
		return nil, fmt.Errorf(
			"verify: re-reading image %d of set %q needs about %d bytes free under %q for the extraction, %d available (DESIGN_SPEC 7.2); "+
				"choose a --work-dir with more space or free the filesystem",
			in.ImageID, in.SetID, totals.Bytes+extractSlack, scratchParent(in.WorkDir), avail)
	}

	res, err := ExtractAndHashPhases(ctx, deps.Tool, in.Path, dir, deps.Bus, totals,
		PhaseNames{Extract: PhaseMediaExtract, Verify: PhaseMediaHash})
	if err != nil {
		return nil, fmt.Errorf("verify: extracting image %d of set %q: %w", in.ImageID, in.SetID, err)
	}
	// THE ZERO-ENTRY READ-FAILURE GATE (file doc): xorriso 1.5.6 on an
	// UNPARSEABLE ISO warns, EXITS 0, and extracts nothing (Tool.Run
	// swallows both streams on exit 0). An empty extraction can never
	// satisfy a non-empty content list, so the verdict "nothing
	// matched" would be right while the DIAGNOSIS "your payloads are
	// missing" would be wrong: the image could not be read at all.
	// Zero entries against a NON-EMPTY content list is therefore a
	// per-image READ failure naming the image unreadable, carrying
	// xorriso's own words (the retained success tail) when they exist.
	if len(res.Entries) == 0 && totals.Entries > 0 {
		msg := fmt.Sprintf(
			"verify: image %d of set %q yielded no extractable entries against %d expected content rows - the ISO at %s is unreadable or not an ISO 9660 image",
			in.ImageID, in.SetID, totals.Entries, in.Path)
		if tail := strings.TrimSpace(string(res.ToolOutput)); tail != "" {
			msg += fmt.Sprintf(" (xorriso output tail: %s)", tail)
		}
		return nil, errors.New(msg)
	}
	return res.Entries, nil
}

// readCopyTree hashes the copy tree DIRECTLY (7.2: no extraction, no
// temp, no xorriso) - the same walk ExtractAndHash runs over an
// extraction, opened by the same one determinate progress phase.
func readCopyTree(ctx context.Context, deps Deps, in VerifyInput, totals Totals) ([]Entry, error) {
	// The single determinate phase of the direct read: exact totals
	// from the content list BEFORE the walk starts (FR-042), then one
	// ItemDone per hashed file inside hashTree.
	if deps.Bus != nil {
		deps.Bus.Emit(progress.Phase{
			Name:       PhaseMediaHash,
			TotalKnown: true,
			TotalFiles: totals.Files,
			TotalBytes: totals.Bytes,
		})
	}
	res, err := hashTree(ctx, in.Path, deps.Bus)
	if err != nil {
		return nil, fmt.Errorf("verify: reading copy tree %s for image %d of set %q: %w",
			in.Path, in.ImageID, in.SetID, err)
	}
	return res.Entries, nil
}

// scratchParent renders the scratch root for error messages ("" means
// the OS temp directory).
func scratchParent(workDir string) string {
	if workDir == "" {
		return "the OS temp directory"
	}
	return workDir
}
