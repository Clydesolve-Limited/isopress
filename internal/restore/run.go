package restore

// T5.3: the multi-image orchestration of DESIGN_SPEC 8.3 (IMPLEMENTATION_PLAN
// 4.6 T5.3). Run is the whole restore workflow in the design's own order:
//
//  1. resolve the selection against the LATEST ACTIVE records (8.1/8.2);
//  2. resolve the required image numbers (ascending) and their work lists;
//  3. resolve image supply through the 8.3 preference order;
//  4. supply any MISSING image: prompt when the run has a prompter (the
//     cli supplies one only when stderr is a TTY - 8.3's gate, which is what
//     keeps NFR-003's non-interactive contract intact), otherwise print the
//     machine-readable required-image listing and fail (RCR-3: the ratified
//     "machine-readable failure mode for non-TTY runs");
//  5. probe the xorriso dependency ONCE when an ISO is in scope (the T4.1
//     seam - the T5.4 extraction needs it, so the run fails up-front with
//     exit 3 rather than after restoring half a set);
//  6. create THE single temp workspace, then process the required images ONE
//     AT A TIME through the ImageRestorer seam, cleaning the workspace
//     BETWEEN images and removing it on every exit path (8.3).
//
// Nothing here writes to stdout, reads stdin, or renders: the listing and
// per-image failures travel as progress events (stderr through the renderer),
// the prompt travels through the injected Prompter (the cli owns the streams),
// and the report is assembled by the cli from Result.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"isopress/internal/progress"
)

// workspacePrefix is the os.MkdirTemp pattern of the ONE restore workspace
// (same convention as verify's extraction scratch and build's staging dirs).
// It carries no user data, so a set id never reaches the temp name.
const workspacePrefix = "isopress-restore-"

// PromptRequest is what the 8.3 prompt needs: the design's exact sentence is
// "Image N of set S is required for <path>. Provide its path (or 'q' to
// abort):" - image number, set id, and one selected path requiring it.
type PromptRequest struct {
	SetID     string
	Image     int64
	NeededFor string
}

// Prompter is the TTY-gated supply prompt of 8.3. The cli implements it over
// the command's stderr (write) and stdin (read) and passes it ONLY when
// stderr is a terminal; a nil Prompter is the non-TTY run, which takes the
// listing path instead. PromptImage returns:
//
//	(path, nil)              the user supplied a path (not yet validated)
//	ErrPromptAborted         the user answered 'q'
//	ErrPromptClosed          no answer is obtainable (stdin at EOF)
//	ctx.Err()                the run was interrupted while waiting
type Prompter interface {
	PromptImage(ctx context.Context, req PromptRequest) (string, error)
}

// The two prompt failure classes (see Prompter's contract). They are separate
// so the abort message can be honest about WHICH happened: a deliberate 'q'
// and an input stream that simply ended are different stories for the log.
var (
	ErrPromptAborted = errors.New("prompt answered 'q'")
	ErrPromptClosed  = errors.New("prompt input closed before an answer")
)

// Input is one restore run's inputs.
type Input struct {
	// Store is the read-only catalog seam (the cli adapts *catalog.DB).
	Store Store
	// Restorer is the T5.3/T5.4 per-image seam (the recording fake in
	// T5.3's tests; the real extraction/reassembly engine - restore.
	// NewImageRestorer - in production, wired by the cli factory).
	Restorer ImageRestorer
	// Selector is the --set-id / --path / --subtree selection.
	Selector Selector
	// Target is --target: required, and never the original source - this
	// package never reads sets.source_root (8.2).
	Target string
	// Supply is the --image-dir / --image N=PATH / --copy-tree-root input.
	Supply SupplySpec
	// WorkDir is --work-dir: the parent of the single temp workspace ("" =
	// the OS temp directory).
	WorkDir string
	// Bus carries progress (9.1). nil disables progress events (unit tests).
	Bus *progress.Bus
	// Prompter is non-nil only on a TTY (see Prompter); nil is the non-TTY
	// listing path of 8.3.
	Prompter Prompter
	// ProbeISO is the T4.1 xorriso dependency check, called at most once and
	// only when an ISO image is in scope (nil skips it - copy-tree runs
	// never pay it, and unit tests inject their own).
	ProbeISO func(ctx context.Context) error
}

// ImageOutcome is ONE image's orchestration outcome for the report: what was
// supplied, what the seam restored, and the seam's error when it failed (a
// per-image failure never aborts the loop - 8.4/FR-041's continue rule).
type ImageOutcome struct {
	Image int64
	Kind  SourceKind
	Path  string
	Out   ImageRestoreResult
	Err   error
}

// Result is one run's outcome. Images holds every processed image in
// ascending order; Selection carries the resolved selection (target,
// selected paths, totals, unclaimed paths) for the report.
type Result struct {
	Selection ResolvedSelection
	Supply    []ResolvedSupply
	Images    []ImageOutcome
}

// Run executes one restore end to end (see the file doc for the sequence).
//
// Error classes returned: *InputError (usage, exit 2 in the cli),
// *MissingImagesError and *AbortedError (the media-supply class, exit 3),
// plain errors from store reads / workspace / xorriso probe (the xorriso
// probe arrives already typed as report.DependencyError through in.ProbeISO),
// and ctx.Err() when the run is interrupted. Per-image restorer failures are
// NOT returned: they live in Result.Images for the exit-10 report.
func Run(ctx context.Context, in Input) (Result, error) {
	var res Result
	switch {
	case in.Store == nil:
		return res, fmt.Errorf("restore: Run called without a Store (wiring bug)")
	case in.Restorer == nil:
		return res, fmt.Errorf("restore: Run called without an ImageRestorer (wiring bug)")
	case in.Target == "":
		// 8.2: --target is REQUIRED and the tool never restores onto the
		// original source path implicitly - there is deliberately no
		// fallback to sets.source_root anywhere in this package.
		return res, inputErrorf("--target is required (the directory to restore into; restore never writes to the original source path implicitly)")
	}

	sel, err := ResolveSelection(ctx, in.Store, in.Selector)
	if err != nil {
		return res, err
	}
	sel.Target = in.Target
	res.Selection = sel

	required := make([]int64, 0, len(sel.Work))
	for _, w := range sel.Work {
		required = append(required, w.Image)
	}
	supply, err := ResolveSupply(ctx, sel.SetID, required, in.Supply)
	if err != nil {
		return res, err
	}
	res.Supply = supply

	if err := supplyMissing(ctx, in, sel, supply); err != nil {
		return res, err
	}
	if err := probeISOOnce(ctx, in, supply); err != nil {
		return res, err
	}

	workspace, err := newWorkspace(in.WorkDir)
	if err != nil {
		return res, err
	}
	defer removeWorkspace(workspace) // every exit path, success or not (8.3)

	for i, w := range sel.Work {
		if err := ctx.Err(); err != nil {
			return res, err // interrupted: stop scheduling images (10.1)
		}
		if i > 0 {
			// 8.3: ONE workspace, cleaned BETWEEN images - the next
			// restorer call always starts from an empty directory.
			if err := cleanWorkspace(workspace); err != nil {
				return res, err
			}
		}
		sp := supplyFor(supply, w.Image)
		emitPhase(in.Bus, w)
		out, err := in.Restorer.RestoreImage(ctx, ImageSupply{
			SetID:  sel.SetID,
			Image:  w.Image,
			Kind:   sp.Kind,
			Path:   sp.Path,
			Sought: sp.Sought,
		}, sel, workspace)
		res.Images = append(res.Images, ImageOutcome{
			Image: w.Image, Kind: sp.Kind, Path: sp.Path, Out: out, Err: err,
		})
		if err != nil {
			emitError(in.Bus, fmt.Sprintf("restore: image %d: %v", w.Image, err))
		}
	}
	return res, nil
}

// supplyMissing handles step 4: nothing to do when every required image was
// supplied; a prompter present means the TTY path (prompt each missing image
// ascending, updating supply in place); no prompter is the non-TTY path -
// emit one machine-readable listing line per missing image and fail with
// *MissingImagesError, before ANY workspace or target write happens.
func supplyMissing(ctx context.Context, in Input, sel ResolvedSelection, supply []ResolvedSupply) error {
	var missingIdx []int
	for i, s := range supply {
		if s.Missing {
			missingIdx = append(missingIdx, i)
		}
	}
	if len(missingIdx) == 0 {
		return nil
	}

	if in.Prompter == nil {
		var missing []MissingImage
		for _, i := range missingIdx {
			m := MissingImage{
				SetID:     sel.SetID,
				Image:     supply[i].Image,
				Sought:    supply[i].Sought,
				NeededFor: neededFor(sel, supply[i].Image),
			}
			missing = append(missing, m)
			emitError(in.Bus, MissingImageLine(m))
		}
		return &MissingImagesError{Missing: missing}
	}

	// TTY: guide the user through each required image IN TURN (FR-024),
	// before any image is processed, so a 'q' at image 3 still leaves the
	// target untouched (no image ran yet).
	for _, i := range missingIdx {
		sp, err := promptOne(ctx, in, sel, supply[i])
		if err != nil {
			return err
		}
		supply[i] = sp
	}
	return nil
}

// promptOne asks for ONE missing image's path, re-prompting while the answer
// is not an existing regular file or directory (an interactive typo is a
// warning and another question, never an input error - the user is being
// guided, 8.3). The prompt sits on the same failure classes as Prompter's
// contract: 'q' and closed input both abort the run through *AbortedError,
// which by construction happens before the image loop, so this run has
// written nothing.
func promptOne(ctx context.Context, in Input, sel ResolvedSelection, missing ResolvedSupply) (ResolvedSupply, error) {
	req := PromptRequest{
		SetID:     sel.SetID,
		Image:     missing.Image,
		NeededFor: neededFor(sel, missing.Image),
	}
	for {
		path, err := in.Prompter.PromptImage(ctx, req)
		switch {
		case errors.Is(err, ErrPromptAborted):
			return ResolvedSupply{}, &AbortedError{SetID: sel.SetID, Image: req.Image, Reason: "the user answered 'q'"}
		case errors.Is(err, ErrPromptClosed):
			return ResolvedSupply{}, &AbortedError{SetID: sel.SetID, Image: req.Image, Reason: ErrPromptClosed.Error()}
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return ResolvedSupply{}, err
		case err != nil:
			return ResolvedSupply{}, err
		}
		kind, verr := classifySupplyPath(path)
		if verr != nil {
			emitWarn(in.Bus, fmt.Sprintf("image %d: %s", req.Image, verr))
			continue
		}
		return ResolvedSupply{Image: req.Image, Kind: kind, Path: path, Sought: []string{path}}, nil
	}
}

// classifySupplyPath is the stat rule the prompted path (and any supply path)
// must pass: an existing regular file is an ISO, an existing directory is a
// copy tree, anything else - including absence - is refused with a reason the
// prompt loop can show before asking again.
func classifySupplyPath(path string) (SourceKind, error) {
	fi, err := os.Stat(path)
	switch {
	case err != nil:
		return "", fmt.Errorf("%s: %v", path, err)
	case fi.Mode().IsRegular():
		return SourceISO, nil
	case fi.IsDir():
		return SourceCopyTree, nil
	default:
		return "", fmt.Errorf("%s is neither a regular file (.iso) nor a directory (copy tree): %v", path, fi.Mode())
	}
}

// probeISOOnce runs the T4.1 dependency probe at most once, and only when at
// least one supplied image is an ISO: extraction is T5.4's job, but the
// dependency decision belongs up-front here, so a copy-tree restore never pays
// it and an ISO restore fails BEFORE any target write (exit 3, the
// DependencyError class the probe itself constructs).
func probeISOOnce(ctx context.Context, in Input, supply []ResolvedSupply) error {
	if in.ProbeISO == nil {
		return nil
	}
	for _, s := range supply {
		if !s.Missing && s.Kind == SourceISO {
			return in.ProbeISO(ctx)
		}
	}
	return nil
}

// neededFor is the 8.3 prompt's <path>: one selected relpath this image
// holds, from the selection's per-image work (alphabetically first, already
// deterministic from ResolveSelection).
func neededFor(sel ResolvedSelection, imageID int64) string {
	if w, ok := sel.WorkFor(imageID); ok {
		return w.NeededFor
	}
	return ""
}

// supplyFor returns the (prompt-resolved) supply entry of one image.
func supplyFor(supply []ResolvedSupply, imageID int64) ResolvedSupply {
	for _, s := range supply {
		if s.Image == imageID {
			return s
		}
	}
	return ResolvedSupply{Image: imageID, Missing: true}
}

// emitPhase declares the per-image determinate progress phase (9.1/9.4): one
// Phase per image, carrying that image's work totals - member count (distinct
// selected relpaths this image contributes to) and this image's byte
// contribution (whole-file sizes, chunk lengths, symlink/secondary sizes;
// dirs are zero-byte members). T5.4's ItemDone/BytesWritten stream fills
// inside this phase.
func emitPhase(bus *progress.Bus, w ImageWork) {
	if bus == nil {
		return
	}
	files, bytes := workTotals(w.Inv)
	bus.Emit(progress.Phase{Name: "restore", TotalKnown: true, TotalFiles: files, TotalBytes: bytes})
}

// workTotals derives the phase denominators from the filtered inventory (see
// emitPhase). A chunked parent counts once as a member but contributes the
// CHUNK BYTES this image holds of it - and a parent that (by a defensive
// files.image_id edge) appears both as a whole file and as chunks is counted
// through its chunks only, so no byte is ever double-counted.
func workTotals(inv Inventory) (files int64, bytes int64) {
	seen := make(map[string]struct{})
	count := func(rel string) {
		if _, ok := seen[rel]; !ok {
			seen[rel] = struct{}{}
			files++
		}
	}
	chunkParents := make(map[string]struct{}, len(inv.Chunks))
	for _, c := range inv.Chunks {
		chunkParents[c.ParentRelPath] = struct{}{}
		count(c.ParentRelPath)
		bytes += c.Length
	}
	for _, f := range inv.Files {
		if _, ok := chunkParents[f.RelPath]; ok {
			continue // accounted through its chunks above
		}
		count(f.RelPath)
		bytes += f.Size
	}
	for _, s := range inv.Symlinks {
		count(s.RelPath)
		bytes += s.Size
	}
	for _, l := range inv.Secondaries {
		count(l.RelPath)
		bytes += l.Size
	}
	for _, d := range inv.Dirs {
		count(d.RelPath)
	}
	return files, bytes
}

// --- the single temp workspace (8.3) ----------------------------------------

// newWorkspace creates THE run workspace under --workDir ("" = OS temp).
func newWorkspace(workDir string) (string, error) {
	path, err := os.MkdirTemp(workDir, workspacePrefix)
	if err != nil {
		return "", fmt.Errorf("restore: creating the temp workspace under %q: %w", workDir, err)
	}
	return path, nil
}

// removeWorkspace deletes the whole workspace (deferred on every exit path).
func removeWorkspace(path string) {
	_ = os.RemoveAll(path)
}

// cleanWorkspace empties the workspace BETWEEN images while keeping the SAME
// directory path (the 8.3 "single temp workspace" contract the seam hands
// every restorer call). A restorer that removed the directory itself is
// tolerated: the workspace is recreated at the same path.
func cleanWorkspace(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		if os.IsNotExist(err) {
			if mkErr := os.MkdirAll(path, 0o700); mkErr != nil {
				return fmt.Errorf("restore: recreating the temp workspace %q: %w", path, mkErr)
			}
			return nil
		}
		return fmt.Errorf("restore: reading the temp workspace %q: %w", path, err)
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(path, e.Name())); err != nil {
			return fmt.Errorf("restore: cleaning the temp workspace %q (%s): %w", path, e.Name(), err)
		}
	}
	return nil
}

// --- progress helpers (nil-bus safe, the Deps convention) --------------------

func emitWarn(bus *progress.Bus, msg string) {
	if bus != nil {
		bus.Emit(progress.Warn{Msg: msg})
	}
}

func emitError(bus *progress.Bus, msg string) {
	if bus != nil {
		bus.Emit(progress.Error{Msg: msg})
	}
}
