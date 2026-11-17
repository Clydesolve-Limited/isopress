package restore

// T5.3 test scaffolding: the programmatic Store, the RECORDING ImageRestorer
// fake (the seam the brief fixes for these tests), the scripted Prompter, and
// a plain-renderer bus so assertions can pin what the run put on stderr.
//
// The fake records EVERY argument the seam receives - including the workspace
// contents at entry and the selection's paths - because the T5.3 acceptance
// criteria are about the ORCHESTRATION contract (call order, one workspace,
// target identity, which names are selected), not about bytes.

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"isopress/internal/progress"
)

// --- fake Store --------------------------------------------------------------

type fakeStore struct {
	records    []Record
	dirs       []DirRecord
	placement  []ImagePaths
	inventory  map[int64]Inventory
	reads      []string
	failRecord error // injected read failure (nil: healthy)
}

func (s *fakeStore) ActiveRecords(ctx context.Context, setID string) ([]Record, error) {
	s.reads = append(s.reads, "records")
	if s.failRecord != nil {
		return nil, s.failRecord
	}
	return s.records, nil
}

func (s *fakeStore) ActiveDirectories(ctx context.Context, setID string) ([]DirRecord, error) {
	s.reads = append(s.reads, "dirs")
	return s.dirs, nil
}

func (s *fakeStore) Placement(ctx context.Context, setID string) ([]ImagePaths, error) {
	s.reads = append(s.reads, "placement")
	return s.placement, nil
}

func (s *fakeStore) ImageInventory(ctx context.Context, setID string, imageID int64) (Inventory, error) {
	s.reads = append(s.reads, "inventory")
	inv, ok := s.inventory[imageID]
	if !ok {
		return Inventory{}, errors.New("no such image")
	}
	return inv, nil
}

// --- recording ImageRestorer fake --------------------------------------------

// restoreCall is one seam invocation as the fake recorded it.
type restoreCall struct {
	SetID     string
	Image     int64
	Kind      SourceKind
	Path      string
	Sought    []string
	Target    string
	Workspace string
	// Selected is the selection.Paths the call received (the names T5.4 may
	// create - the hard-link partial-selection assertions read it here).
	Selected []string
	// WorkImages is every required image number the call received (the fake
	// sees the WHOLE selection on every call).
	WorkImages []int64
	// InventoryFiles/Secondaries are this image's filtered work list, as the
	// seam received it (the hard-link claim assertions read them here).
	InventoryFiles       []string
	InventorySecondaries []string
	InventoryPrimaryOf   map[string]string // secondary relpath -> primary relpath
	InventoryChunkParent []string
	// WorkspaceEntries lists the workspace contents AT ENTRY: the 8.3
	// contract says every call starts from a clean workspace.
	WorkspaceEntries []string
}

// recordingRestorer is the RECORDING fake ImageRestorer of the T5.3 brief.
// It writes deliberate debris into the workspace (so the between-images clean
// is exercised, not vacuous) and synthesizes an ImageRestoreResult from the
// work list it received, honoring injected per-image errors and per-file
// failures.
type recordingRestorer struct {
	calls     []restoreCall
	imageErrs map[int64]error
	failFiles map[string]string // relpath -> failure detail
	// bus, when set, makes the fake behave like a real restorer for
	// progress: one ItemDone per completed member inside the phase the
	// orchestration declared (the 9.1 stream T5.4 will produce).
	bus *progress.Bus
	// onCall runs after the call is recorded (context-cancel seams, ...).
	onCall func(call restoreCall) error
}

func (r *recordingRestorer) RestoreImage(ctx context.Context, image ImageSupply, selection ResolvedSelection, workspace string) (ImageRestoreResult, error) {
	work, _ := selection.WorkFor(image.Image)
	call := restoreCall{
		SetID:              image.SetID,
		Image:              image.Image,
		Kind:               image.Kind,
		Path:               image.Path,
		Sought:             append([]string(nil), image.Sought...),
		Target:             selection.Target,
		Workspace:          workspace,
		Selected:           append([]string(nil), selection.Paths...),
		InventoryPrimaryOf: map[string]string{},
	}
	for _, w := range selection.Work {
		call.WorkImages = append(call.WorkImages, w.Image)
	}
	for _, f := range work.Inv.Files {
		call.InventoryFiles = append(call.InventoryFiles, f.RelPath)
	}
	for _, s := range work.Inv.Symlinks {
		call.InventoryFiles = append(call.InventoryFiles, s.RelPath)
	}
	for _, d := range work.Inv.Dirs {
		call.InventoryFiles = append(call.InventoryFiles, d.RelPath)
	}
	for _, sec := range work.Inv.Secondaries {
		call.InventorySecondaries = append(call.InventorySecondaries, sec.RelPath)
		call.InventoryPrimaryOf[sec.RelPath] = sec.PrimaryRelPath
	}
	for _, c := range work.Inv.Chunks {
		call.InventoryChunkParent = append(call.InventoryChunkParent, c.ParentRelPath)
	}
	if entries, err := os.ReadDir(workspace); err == nil {
		for _, e := range entries {
			call.WorkspaceEntries = append(call.WorkspaceEntries, e.Name())
		}
	}
	r.calls = append(r.calls, call)

	// Debris: a real restorer leaves scratch in the workspace; the
	// orchestration must clean it before the NEXT image.
	if err := os.WriteFile(filepath.Join(workspace, debrisName(image.Image)), []byte("scratch"), 0o600); err != nil && !os.IsNotExist(err) {
		return ImageRestoreResult{}, err
	}
	if r.onCall != nil {
		if err := r.onCall(call); err != nil {
			return ImageRestoreResult{Image: image.Image}, err
		}
	}
	if err := r.imageErrs[image.Image]; err != nil {
		return ImageRestoreResult{Image: image.Image}, err
	}

	res := ImageRestoreResult{Image: image.Image}
	for _, m := range seamMembers(work) {
		if r.bus != nil {
			r.bus.Emit(progress.ItemDone{Bytes: m.size, Path: m.rel})
		}
		if detail, failed := r.failFiles[m.rel]; failed {
			res.Failed++
			res.Files = append(res.Files, FileOutcome{RelPath: m.rel, Status: "failed", Bytes: m.size, Detail: detail})
			continue
		}
		res.Restored++
		res.Bytes += m.size
	}
	return res, nil
}

// debrisName is the scratch file the fake leaves behind for each image.
func debrisName(imageID int64) string {
	return "debris-" + itoa(imageID)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// seamMember is one distinct target member of an image's work list.
type seamMember struct {
	rel  string
	size int64
}

// seamMembers flattens a work list into distinct target members (a chunked
// parent is one member assembled from its chunks on this image).
func seamMembers(w ImageWork) []seamMember {
	var out []seamMember
	seen := map[string]bool{}
	add := func(rel string, size int64) {
		if seen[rel] {
			for i := range out {
				if out[i].rel == rel {
					out[i].size += size
				}
			}
			return
		}
		seen[rel] = true
		out = append(out, seamMember{rel: rel, size: size})
	}
	for _, c := range w.Inv.Chunks {
		add(c.ParentRelPath, c.Length)
	}
	for _, f := range w.Inv.Files {
		if seen[f.RelPath] {
			continue
		}
		add(f.RelPath, f.Size)
	}
	for _, s := range w.Inv.Symlinks {
		add(s.RelPath, s.Size)
	}
	for _, l := range w.Inv.Secondaries {
		add(l.RelPath, l.Size)
	}
	for _, d := range w.Inv.Dirs {
		add(d.RelPath, 0)
	}
	return out
}

// --- scripted Prompter -------------------------------------------------------

type promptAnswer struct {
	path string
	err  error
}

type scriptedPrompter struct {
	answers []promptAnswer
	prompts []PromptRequest
}

func (p *scriptedPrompter) PromptImage(ctx context.Context, req PromptRequest) (string, error) {
	p.prompts = append(p.prompts, req)
	if len(p.answers) == 0 {
		return "", ErrPromptClosed
	}
	a := p.answers[0]
	p.answers = p.answers[1:]
	return a.path, a.err
}

// --- bus wiring (plain renderer over a buffer, stepped clock) ----------------

// wireBus starts a plain renderer over an isolated buffer with a clock that
// steps past the 9.3 cadence on every read, so every progress line lands.
// The returned finisher emits the terminal Done and returns the rendered
// stderr text.
func wireBus() (*progress.Bus, func(summary string) string) {
	var buf bytes.Buffer
	var mu sync.Mutex
	base := time.Now()
	ticks := 0
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		ticks++
		return base.Add(time.Duration(ticks) * (progress.PlainInterval + time.Second))
	}
	bus := progress.NewBus()
	wait := progress.Start(progress.New(progress.Options{
		Mode:   progress.ModePlain,
		Stderr: &buf,
		Now:    clock,
	}), bus)
	// Idempotent: tests may finish in the body and again from a cleanup, so
	// a fatal path never leaks the renderer goroutine.
	var once sync.Once
	return bus, func(summary string) string {
		once.Do(func() {
			bus.Finish(progress.Done{Summary: summary})
			wait()
		})
		return buf.String()
	}
}

// sortedCopy returns a sorted copy of a string slice (assertion helper).
func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// tempWorkspaceDir returns an isolated --work-dir so leftover-workspace
// assertions never see another test's scratch.
func tempWorkspaceDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("work dir: %v", err)
	}
	return dir
}

// listDirNames returns the sorted names under dir (nil when absent).
func listDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("reading %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return sortedCopy(names)
}
