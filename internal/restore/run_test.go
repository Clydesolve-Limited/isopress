package restore

// T5.3 acceptance: the multi-image orchestration (DESIGN_SPEC 8.3;
// IMPLEMENTATION_PLAN 4.6 T5.3) exercised through the RECORDING ImageRestorer
// fake - call order, ONE workspace reused and cleaned between images, the
// TTY-gated prompt, the non-TTY machine-readable listing and its exit class,
// the up-front xorriso probe, per-image continuation, and interruption.
//
// Nothing here writes bytes: the fake stands in for T5.4, which is exactly
// what the seam is for.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"isopress/internal/progress"
)

// runHarness wires one Run over the two-image fixture: real supply trees (so
// the stat pass is exercised), an isolated --work-dir, and a target directory
// that does NOT exist yet (T5.4 creates it; nothing in T5.3 does).
type runHarness struct {
	store    *fakeStore
	restorer *recordingRestorer
	target   string
	workDir  string
	bus      *progress.Bus
	finish   func(summary string) string
	spec     SupplySpec
	// sp is the scripted prompter; the interface passed to Run stays NIL
	// unless a test sets it (a typed-nil in the interface would defeat
	// Run's non-TTY gate - the same trap the cli avoids by only ever
	// building a prompter on a real terminal).
	sp     *scriptedPrompter
	probes int
	// probesAtCalls records len(restorer.calls) at the moment each probe
	// fired, so a test can pin the probe happens BEFORE any image runs.
	probesAtCalls []int
	probeErr      error
	runErr        error
	res           Result
}

func newRunHarness(t *testing.T) *runHarness {
	t.Helper()
	h := &runHarness{
		store:    multiImageStore(),
		restorer: &recordingRestorer{},
		target:   filepath.Join(t.TempDir(), "target"),
		workDir:  tempWorkspaceDir(t),
	}
	h.bus, h.finish = wireBus()
	t.Cleanup(func() { h.finish("cleanup") })
	return h
}

// supplyTrees creates one copy-tree directory per image and maps them through
// explicit --image N=PATH entries (the brief's multi-image fixture shape).
func (h *runHarness) supplyTrees(t *testing.T, images ...int64) {
	t.Helper()
	root := t.TempDir()
	for _, n := range images {
		dir := filepath.Join(root, fmt.Sprintf("S-%03d-tree", n))
		mkdir(t, dir)
		h.spec.Explicit = append(h.spec.Explicit, ExplicitImage{Image: n, Path: dir})
	}
}

// run executes the Run under test and records its error.
func (h *runHarness) run(t *testing.T) {
	t.Helper()
	var prompter Prompter
	if h.sp != nil {
		prompter = h.sp
	}
	res, err := Run(context.Background(), Input{
		Store:    h.store,
		Restorer: h.restorer,
		Selector: Selector{SetID: "S"},
		Target:   h.target,
		Supply:   h.spec,
		WorkDir:  h.workDir,
		Bus:      h.bus,
		Prompter: prompter,
		ProbeISO: h.probe,
	})
	h.res, h.runErr = res, err
}

func (h *runHarness) probe(ctx context.Context) error {
	h.probes++
	h.probesAtCalls = append(h.probesAtCalls, len(h.restorer.calls))
	return h.probeErr
}

// --- the per-image loop ------------------------------------------------------

func TestRunProcessesImagesOneAtATimeInAscendingOrder(t *testing.T) {
	h := newRunHarness(t)
	h.supplyTrees(t, 2, 1) // deliberately OUT of order: the run must sort
	h.restorer.bus = h.bus
	h.run(t)

	if h.runErr != nil {
		t.Fatalf("Run: %v", h.runErr)
	}
	if len(h.restorer.calls) != 2 {
		t.Fatalf("restorer calls = %d, want exactly 2 (one per required image)", len(h.restorer.calls))
	}
	if h.restorer.calls[0].Image != 1 || h.restorer.calls[1].Image != 2 {
		t.Errorf("call order = [%d %d], want ascending [1 2]", h.restorer.calls[0].Image, h.restorer.calls[1].Image)
	}
	// Results are collected per image, in the same order.
	if len(h.res.Images) != 2 || h.res.Images[0].Image != 1 || h.res.Images[1].Image != 2 {
		t.Errorf("result images = %+v, want [1 2]", h.res.Images)
	}
	// Each call saw the run's target and the full selection (the seam's
	// contract: target on the selection, names to create from Paths).
	for _, c := range h.restorer.calls {
		if c.Target != h.target {
			t.Errorf("call %d target = %q, want %q (never the original source)", c.Image, c.Target, h.target)
		}
		if len(c.Selected) != 6 {
			t.Errorf("call %d selected = %v, want the 6 selected paths", c.Image, c.Selected)
		}
		if len(c.WorkImages) != 2 {
			t.Errorf("call %d work images = %v, want both required images", c.Image, c.WorkImages)
		}
	}
}

func TestRunReusesOneWorkspaceAndCleansItBetweenImages(t *testing.T) {
	h := newRunHarness(t)
	h.supplyTrees(t, 1, 2)
	h.restorer.bus = h.bus
	h.run(t)
	if h.runErr != nil {
		t.Fatalf("Run: %v", h.runErr)
	}

	first, second := h.restorer.calls[0], h.restorer.calls[1]
	if first.Workspace == "" || first.Workspace != second.Workspace {
		t.Fatalf("workspaces = %q vs %q, want ONE path reused for every image (8.3)", first.Workspace, second.Workspace)
	}
	// Every call starts from a clean workspace: the fake leaves debris
	// behind, so an empty entry for image 2 proves the between-images clean.
	if len(first.WorkspaceEntries) != 0 {
		t.Errorf("image 1 workspace at entry = %v, want empty (fresh)", first.WorkspaceEntries)
	}
	if len(second.WorkspaceEntries) != 0 {
		t.Errorf("image 2 workspace at entry = %v, want empty (cleaned between images)", second.WorkspaceEntries)
	}
	// And the whole workspace is gone when the run returns.
	if left := listDirNames(t, h.workDir); len(left) != 0 {
		t.Errorf("work dir after the run = %v, want no leftover workspace", left)
	}
	if _, err := os.Stat(first.Workspace); !os.IsNotExist(err) {
		t.Errorf("workspace %q still exists after the run: %v", first.Workspace, err)
	}
}

func TestRunReportsPerImageResultsAndPhaseTotals(t *testing.T) {
	h := newRunHarness(t)
	h.supplyTrees(t, 1, 2)
	h.restorer.bus = h.bus
	h.run(t)
	if h.runErr != nil {
		t.Fatalf("Run: %v", h.runErr)
	}

	// Image 1 presents 5 members (big.bin's chunk, small.txt, fam-a,
	// fam-b, empty-dir) totalling 800 bytes; image 2 presents 2 (big.bin's
	// second chunk, the symlink) totalling 409. The plain lines prove the
	// per-image Phase carried DETERMINATE totals and that the ItemDone
	// stream filled them (9.3's frozen format).
	out := h.finish("restore: set S done")
	for _, want := range []string{
		"restore: files 5/5, 800 B/800 B (100%)",
		"restore: files 2/2, 409 B/409 B (100%)",
		"restore: set S done",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered progress lacks %q:\n%s", want, out)
		}
	}
	if h.res.Images[0].Out.Restored != 5 || h.res.Images[1].Out.Restored != 2 {
		t.Errorf("restored counts = %d, %d; want 5, 2", h.res.Images[0].Out.Restored, h.res.Images[1].Out.Restored)
	}
	if h.res.Images[0].Kind != SourceCopyTree || h.res.Images[1].Path == "" {
		t.Errorf("outcome supply = %+v, want kind/path reported per image", h.res.Images[0])
	}
}

// --- the non-TTY path (8.3, RCR-3) ------------------------------------------

func TestRunNonTTYPrintsRequiredImageListingAndFails(t *testing.T) {
	h := newRunHarness(t)
	h.spec.ImageDir = t.TempDir() // discovery configured, nothing there
	// Prompter nil == stderr is not a terminal (the cli's gate).
	h.run(t)

	var missing *MissingImagesError
	if !errors.As(h.runErr, &missing) {
		t.Fatalf("err = %v, want *MissingImagesError (the cli maps it to exit 3)", h.runErr)
	}
	if len(missing.Missing) != 2 {
		t.Fatalf("missing = %+v, want both required images", missing.Missing)
	}
	if missing.Missing[0].Image != 1 || missing.Missing[1].Image != 2 {
		t.Errorf("missing order = [%d %d], want ascending", missing.Missing[0].Image, missing.Missing[1].Image)
	}
	if missing.Missing[0].NeededFor == "" {
		t.Error("each listing entry must name a path that requires the image")
	}

	// The machine-readable listing reached stderr through the bus.
	out := h.finish("abort")
	wantLine := "error: required-image: image=1 set=S sought=" +
		filepath.Join(h.spec.ImageDir, "S-001.iso") + " needed-for=big.bin"
	if !strings.Contains(out, wantLine) {
		t.Errorf("stderr lacks the required-image line %q:\n%s", wantLine, out)
	}
	if !strings.Contains(out, "error: required-image: image=2 set=S") {
		t.Errorf("stderr lacks the second required-image line:\n%s", out)
	}

	// Nothing ran: no restorer call, no workspace, no target.
	if len(h.restorer.calls) != 0 {
		t.Errorf("restorer calls = %d, want 0 (the listing aborts BEFORE the loop)", len(h.restorer.calls))
	}
	if left := listDirNames(t, h.workDir); len(left) != 0 {
		t.Errorf("work dir = %v, want no workspace on the listing path", left)
	}
	if _, err := os.Stat(h.target); !os.IsNotExist(err) {
		t.Errorf("target exists after a listing abort: %v", err)
	}
	// Supply is still reported on the Result for the caller's diagnostics.
	if len(h.res.Supply) != 2 || !h.res.Supply[0].Missing {
		t.Errorf("result supply = %+v, want the two missing entries", h.res.Supply)
	}
}

// --- the TTY prompt (8.3 exact wording) -------------------------------------

func TestRunTTYPromptSuppliesMissingImageAndProceeds(t *testing.T) {
	h := newRunHarness(t)
	h.supplyTrees(t, 2)           // image 2 is supplied up-front...
	h.spec.ImageDir = t.TempDir() // ...image 1 cannot be discovered...
	tree := filepath.Join(t.TempDir(), "S-001-tree")
	mkdir(t, tree)
	// ...but the user answers the prompt with a copy tree.
	h.sp = &scriptedPrompter{answers: []promptAnswer{{path: tree}}}
	h.restorer.bus = h.bus
	h.run(t)

	if h.runErr != nil {
		t.Fatalf("Run: %v", h.runErr)
	}
	if len(h.sp.prompts) != 1 {
		t.Fatalf("prompts = %+v, want exactly one (image 1)", h.sp.prompts)
	}
	p := h.sp.prompts[0]
	if p.SetID != "S" || p.Image != 1 || p.NeededFor == "" {
		t.Errorf("prompt = %+v, want set S, image 1, and a representative path", p)
	}
	// The supplied path was validated by stat: a directory means copy tree.
	if h.restorer.calls[0].Kind != SourceCopyTree || h.restorer.calls[0].Path != tree {
		t.Errorf("call supply = %q %q, want the prompted copy tree", h.restorer.calls[0].Kind, h.restorer.calls[0].Path)
	}
	if len(h.res.Images) != 2 {
		t.Errorf("processed images = %d, want 2 (the prompt unlocked the run)", len(h.res.Images))
	}
}

func TestRunPromptQAbortsCleanlyWithNoWrites(t *testing.T) {
	h := newRunHarness(t)
	h.spec.ImageDir = t.TempDir()
	// The 'q' answer is ErrPromptAborted, exactly Prompter's contract.
	h.sp = &scriptedPrompter{answers: []promptAnswer{{err: ErrPromptAborted}}}
	h.run(t)

	var aborted *AbortedError
	if !errors.As(h.runErr, &aborted) {
		t.Fatalf("err = %v, want *AbortedError", h.runErr)
	}
	if !strings.Contains(aborted.Error(), "q") || !strings.Contains(aborted.Error(), "nothing") {
		t.Errorf("abort message must be honest about 'q' and about writing nothing: %v", aborted)
	}
	if len(h.restorer.calls) != 0 {
		t.Errorf("restorer calls = %d, want 0 (all supply happens before the loop)", len(h.restorer.calls))
	}
	if _, err := os.Stat(h.target); !os.IsNotExist(err) {
		t.Errorf("target must stay untouched after 'q': %v", err)
	}
	if left := listDirNames(t, h.workDir); len(left) != 0 {
		t.Errorf("work dir = %v, want no workspace created", left)
	}
}

func TestRunPromptInputClosedAborts(t *testing.T) {
	h := newRunHarness(t)
	h.spec.ImageDir = t.TempDir()
	h.sp = &scriptedPrompter{} // no answers: stdin at EOF
	h.run(t)

	var aborted *AbortedError
	if !errors.As(h.runErr, &aborted) {
		t.Fatalf("err = %v, want *AbortedError", h.runErr)
	}
	if !strings.Contains(aborted.Reason, "closed") {
		t.Errorf("reason = %q, want the closed-input story", aborted.Reason)
	}
	if len(h.restorer.calls) != 0 {
		t.Errorf("restorer calls = %d, want 0", len(h.restorer.calls))
	}
}

func TestRunPromptRetriesAnInvalidPath(t *testing.T) {
	h := newRunHarness(t)
	h.supplyTrees(t, 2) // only image 1 is missing
	h.spec.ImageDir = t.TempDir()
	tree := filepath.Join(t.TempDir(), "S-001-tree")
	mkdir(t, tree)
	h.sp = &scriptedPrompter{answers: []promptAnswer{
		{path: filepath.Join(t.TempDir(), "does-not-exist")}, // typo
		{path: tree}, // correct on the second ask
	}}
	h.restorer.bus = h.bus
	h.run(t)

	if h.runErr != nil {
		t.Fatalf("Run: %v", h.runErr)
	}
	if len(h.sp.prompts) != 2 {
		t.Errorf("prompts = %d, want 2 (re-ask after an invalid answer)", len(h.sp.prompts))
	}
	out := h.finish("done")
	if !strings.Contains(out, "warning: image 1:") || !strings.Contains(out, "does-not-exist") {
		t.Errorf("stderr lacks the invalid-path warning:\n%s", out)
	}
	if h.restorer.calls[0].Path != tree {
		t.Errorf("supplied path = %q, want the second answer", h.restorer.calls[0].Path)
	}
}

// --- the xorriso dependency probe (T4.1 seam, exit 3) ------------------------

func TestRunProbeISOFiresOnlyForISOsAndOnlyOnce(t *testing.T) {
	// Copy trees only: no probe at all (NFR-004: copy paths never pay it).
	h := newRunHarness(t)
	h.supplyTrees(t, 1, 2)
	h.run(t)
	if h.runErr != nil {
		t.Fatalf("Run: %v", h.runErr)
	}
	if h.probes != 0 {
		t.Errorf("probe count = %d for copy trees, want 0", h.probes)
	}

	// One ISO in scope: exactly one probe before any image runs.
	h2 := newRunHarness(t)
	iso := filepath.Join(t.TempDir(), "S-001.iso")
	writeFile(t, iso, "iso")
	h2.spec.Explicit = []ExplicitImage{{Image: 1, Path: iso}, {Image: 2, Path: makeTree(t)}}
	h2.run(t)
	if h2.runErr != nil {
		t.Fatalf("Run (iso): %v", h2.runErr)
	}
	if h2.probes != 1 {
		t.Errorf("probe count = %d with two ISOs in scope, want 1 (once, up-front)", h2.probes)
	}
	// The probe fired BEFORE any image ran: zero seam calls at probe time
	// (the up-front dependency gate - an absent xorriso can never leave a
	// half-restored target).
	if len(h2.probesAtCalls) != 1 || h2.probesAtCalls[0] != 0 {
		t.Errorf("probe fired with %v seam calls already made, want [0] (up-front, before any image)", h2.probesAtCalls)
	}
	if len(h2.restorer.calls) != 2 {
		t.Errorf("calls = %d, want 2 (the probe gates, it does not stop a healthy run)", len(h2.restorer.calls))
	}

	// A failing probe (xorriso absent/old) aborts before any image: the
	// DependencyError the T4.1 seam returns surfaces verbatim.
	h3 := newRunHarness(t)
	iso3 := filepath.Join(t.TempDir(), "S-001.iso")
	writeFile(t, iso3, "iso")
	h3.spec.Explicit = []ExplicitImage{{Image: 1, Path: iso3}, {Image: 2, Path: makeTree(t)}}
	h3.probeErr = errors.New(`missing external dependency "xorriso"`)
	h3.run(t)
	if h3.runErr == nil || !strings.Contains(h3.runErr.Error(), "xorriso") {
		t.Errorf("err = %v, want the xorriso DependencyError", h3.runErr)
	}
	if len(h3.restorer.calls) != 0 {
		t.Errorf("calls = %d, want 0 (dependency check is up-front)", len(h3.restorer.calls))
	}
}

// makeTree creates one copy-tree directory and returns its path.
func makeTree(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "S-002-tree")
	mkdir(t, dir)
	return dir
}

// --- failure, interruption, and the placeholder ------------------------------

func TestRunPerImageFailureDoesNotAbortTheLoop(t *testing.T) {
	h := newRunHarness(t)
	h.supplyTrees(t, 1, 2)
	h.restorer.bus = h.bus
	h.restorer.imageErrs = map[int64]error{1: errors.New("chunk 3 hash mismatch")}
	h.run(t)

	if h.runErr != nil {
		t.Fatalf("Run = %v, want nil (per-image failures are collected, not fatal)", h.runErr)
	}
	if len(h.restorer.calls) != 2 {
		t.Fatalf("calls = %d, want 2: image 2 must still run", len(h.restorer.calls))
	}
	if h.res.Images[0].Err == nil || !strings.Contains(h.res.Images[0].Err.Error(), "chunk 3") {
		t.Errorf("image 1 outcome err = %v, want the recorded failure", h.res.Images[0].Err)
	}
	if h.res.Images[1].Err != nil {
		t.Errorf("image 2 outcome err = %v, want nil", h.res.Images[1].Err)
	}
	out := h.finish("done")
	if !strings.Contains(out, "error: restore: image 1: chunk 3 hash mismatch") {
		t.Errorf("stderr lacks the per-image failure line:\n%s", out)
	}
}

func TestRunInterruptionStopsBeforeTheNextImage(t *testing.T) {
	h := newRunHarness(t)
	h.supplyTrees(t, 1, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.restorer.onCall = func(call restoreCall) error {
		cancel() // SIGINT analogue: the run context dies mid-image 1
		return nil
	}
	res, err := Run(ctx, Input{
		Store: h.store, Restorer: h.restorer, Selector: Selector{SetID: "S"},
		Target: h.target, Supply: h.spec, WorkDir: h.workDir, Bus: h.bus,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled (the cli maps it to 130)", err)
	}
	if len(h.restorer.calls) != 1 {
		t.Errorf("calls = %d, want 1: no new image is scheduled after cancellation", len(h.restorer.calls))
	}
	if len(res.Images) != 1 {
		t.Errorf("partial result images = %d, want the processed one (the report shows what ran)", len(res.Images))
	}
	if left := listDirNames(t, h.workDir); len(left) != 0 {
		t.Errorf("work dir = %v, want the workspace removed on the interrupt path", left)
	}
}

func TestRunRequiresTarget(t *testing.T) {
	h := newRunHarness(t)
	_, err := Run(context.Background(), Input{
		Store: h.store, Restorer: h.restorer, Selector: Selector{SetID: "S"},
		WorkDir: h.workDir,
	})
	var input *InputError
	if !errors.As(err, &input) {
		t.Fatalf("err = %v, want *InputError (exit 2)", err)
	}
	if !strings.Contains(err.Error(), "--target") {
		t.Errorf("message must name --target: %v", err)
	}
}

// --- all-unclaimed and no-source supply edges --------------------------------

// A selection whose EVERY path is unclaimed (scanned after the last plan,
// never planned) is an honest exit-10 report, NOT a media demand: the run has
// no required images, so no supply source is consulted, no required-image
// listing may appear, and no workspace residue is left behind.
func TestRunAllUnclaimedSelectionNeedsNoMedia(t *testing.T) {
	h := newRunHarness(t)
	// The whole selection is unclaimed: no placement row names any of it.
	h.store.records = []Record{
		{FileID: 9, RelPath: "never-planned.txt", EntryKind: "regular", Size: 7, Hash: []byte{7}},
	}
	h.store.dirs = nil
	h.store.placement = nil
	h.store.inventory = map[int64]Inventory{}
	// NO supply source configured at all - and none may be demanded.

	h.run(t)
	if h.runErr != nil {
		t.Fatalf("Run = %v, want nil (unclaimed paths are the cli's exit-10 report, not a run error)", h.runErr)
	}
	if len(h.restorer.calls) != 0 {
		t.Errorf("seam calls = %d, want 0 (no image carries an unclaimed path)", len(h.restorer.calls))
	}
	if len(h.res.Selection.Work) != 0 || len(h.res.Images) != 0 || len(h.res.Supply) != 0 {
		t.Errorf("work = %+v, images = %+v, supply = %+v: an all-unclaimed selection requires NO media",
			h.res.Selection.Work, h.res.Images, h.res.Supply)
	}
	if strings.Join(h.res.Selection.Unclaimed, ",") != "never-planned.txt" {
		t.Errorf("unclaimed = %v, want [never-planned.txt]", h.res.Selection.Unclaimed)
	}
	out := h.finish("done")
	if strings.Contains(out, "required-image:") {
		t.Errorf("stderr must carry NO required-image listing when nothing is required (exit 10, not 3):\n%s", out)
	}
	if left := listDirNames(t, h.workDir); len(left) != 0 {
		t.Errorf("work dir = %v, want no workspace residue", left)
	}
}

// With NO supply source configured at all, the non-TTY listing still names
// every required image with the frozen "sought=-" placeholder (nothing could
// even be looked for) so a parser never handles a missing field.
func TestRunMissingListingWithNoSourceAtAll(t *testing.T) {
	h := newRunHarness(t)
	h.run(t) // no --image-dir, no --image, no --copy-tree-root, no prompter

	var missing *MissingImagesError
	if !errors.As(h.runErr, &missing) {
		t.Fatalf("err = %v, want *MissingImagesError", h.runErr)
	}
	if len(missing.Missing) != 2 || missing.Missing[0].Image != 1 || missing.Missing[1].Image != 2 {
		t.Errorf("missing = %+v, want both images ascending", missing.Missing)
	}
	out := h.finish("abort")
	if !strings.Contains(out, "error: required-image: image=1 set=S sought=- needed-for=big.bin") {
		t.Errorf("stderr lacks the no-source listing line for image 1:\n%s", out)
	}
	if !strings.Contains(out, "error: required-image: image=2 set=S sought=-") {
		t.Errorf("stderr lacks the no-source listing line for image 2:\n%s", out)
	}
}

// --- the prompt's shape validation (warn + re-ask, never an input error) -----

// A prompted answer that exists but has the WRONG shape (a character device
// - neither an .iso file nor a copy-tree directory) is a warning and another
// question: the user is being guided (8.3), so a bad interactive answer can
// never fail the run as an input error.
func TestRunPromptRetriesAWrongShapePath(t *testing.T) {
	h := newRunHarness(t)
	h.supplyTrees(t, 2) // only image 1 is missing
	h.spec.ImageDir = t.TempDir()
	tree := filepath.Join(t.TempDir(), "S-001-tree")
	mkdir(t, tree)
	h.sp = &scriptedPrompter{answers: []promptAnswer{
		{path: "/dev/null"}, // exists, but is a character device: wrong shape
		{path: tree},        // correct on the second ask
	}}
	h.restorer.bus = h.bus
	h.run(t)

	if h.runErr != nil {
		t.Fatalf("Run = %v, want nil (a wrong-shape answer is a warn + re-ask, not a failure)", h.runErr)
	}
	if len(h.sp.prompts) != 2 {
		t.Errorf("prompts = %d, want 2 (re-ask after the wrong-shape answer)", len(h.sp.prompts))
	}
	out := h.finish("done")
	if !strings.Contains(out, "neither a regular file") {
		t.Errorf("stderr lacks the wrong-shape warning:\n%s", out)
	}
	// Image 1 took the prompted tree; image 2 kept its up-front mapping.
	if len(h.restorer.calls) != 2 {
		t.Fatalf("seam calls = %d, want 2", len(h.restorer.calls))
	}
	if h.restorer.calls[0].Path != tree || h.restorer.calls[0].Kind != SourceCopyTree {
		t.Errorf("image 1 supply = %+v, want the second answer as a copy tree", h.restorer.calls[0])
	}
}

// --- work totals (the phase denominators) ------------------------------------

func TestWorkTotalsNeverDoubleCountsAChunkedParent(t *testing.T) {
	inv := Inventory{
		// The defensive edge: a chunked parent that also carries a
		// files.image_id row (the catalog accepts it; the planner does not
		// produce it). Its BYTES are accounted through the chunks only.
		Files: []FileItem{
			{RelPath: "big.bin", Size: 1000},
			{RelPath: "other.txt", Size: 10},
		},
		Chunks: []ChunkItem{
			{ParentRelPath: "big.bin", Length: 600},
			{ParentRelPath: "big.bin", Length: 400},
		},
		Dirs: []DirItem{{RelPath: "empty"}},
	}
	files, bytes := workTotals(inv)
	if files != 3 {
		t.Errorf("files = %d, want 3 distinct members (big.bin, other.txt, empty)", files)
	}
	if bytes != 1010 {
		t.Errorf("bytes = %d, want 1010 (600+400 chunks + 10, never 1000 again)", bytes)
	}
}
