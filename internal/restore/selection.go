package restore

// T5.3: selection resolution (DESIGN_SPEC 8.1 D-07 latest-state semantics,
// 8.2 selection model; IMPLEMENTATION_PLAN 4.6 T5.3).
//
// Resolve turns a Selector into a ResolvedSelection in four steps:
//
//  1. READ the three catalog views the run needs - the ACTIVE files rows
//     (the v_active_files predicate, which is what makes the run
//     latest-state only), the ACTIVE directories rows (empty dirs are
//     records too, 2.3/FR-037), and the whole-set placement summary;
//  2. MATCH the selector against those records: whole set (no selector),
//     exact --path entries (repeatable), a component-aware --subtree prefix,
//     or the union of both selectors. Every --path entry and the --subtree
//     must match AT LEAST ONE active record or directory: a selector that
//     matches nothing is the user's typo and fails as an input error (exit 2
//     in the cli), never a silent empty run;
//  3. RESOLVE images: an image is required when the placement summary shows
//     it CLAIMS at least one selected path. Because catalog.CopyPlacement
//     already applies the unified T3.4 secondary-claim rule, this single step
//     gives every hard-link and chunk case for free - a selected family
//     member resolves to the PRIMARY's image (its bytes live there), and a
//     selected chunked record resolves to EVERY image holding one of its
//     chunks (the parent relpath is claimed by each). Selected paths that no
//     image claims are collected as Unclaimed (honest per-item failures, not
//     silent drops);
//  4. BUILD the per-image work lists: each required image's inventory
//     (files, symlinks, chunks, dirs, secondaries) filtered to the selected
//     relpaths, so a partially-selected hard-link family reaches T5.4 as
//     exactly the requested names (content once, links for the requested
//     names - an unselected sibling must never be created).
//
// The totals (Files, Bytes) come from the matched records, which is what
// makes 8.4's determinate restore progress possible: both numbers are known
// before the first image is supplied.

import (
	"context"
	"sort"
	"strings"

	"isopress/internal/media"
)

// ResolveSelection resolves sel against the catalog views behind store.
// Store read failures return plain wrapped errors (the family convention -
// validate's ActiveRecords failure classifies the same way); selector input
// failures return *InputError so the cli maps them to exit 2.
func ResolveSelection(ctx context.Context, store Store, sel Selector) (ResolvedSelection, error) {
	out := ResolvedSelection{SetID: sel.SetID, Mode: sel.Mode()}
	if sel.SetID == "" {
		return out, inputErrorf("--set-id is required (the set to restore)")
	}
	matcher, err := buildMatcher(sel)
	if err != nil {
		return out, err
	}

	records, err := store.ActiveRecords(ctx, sel.SetID)
	if err != nil {
		return out, err
	}
	dirs, err := store.ActiveDirectories(ctx, sel.SetID)
	if err != nil {
		return out, err
	}
	placement, err := store.Placement(ctx, sel.SetID)
	if err != nil {
		return out, err
	}

	selected := make(map[string]struct{})
	for _, rec := range records {
		if !matcher.matches(rec.RelPath) {
			continue
		}
		selected[rec.RelPath] = struct{}{}
		out.Files++
		out.Bytes += rec.Size
	}
	for _, d := range dirs {
		if matcher.matches(d.RelPath) {
			selected[d.RelPath] = struct{}{}
		}
	}
	if err := matcher.gate(sel, records, dirs); err != nil {
		return out, err
	}

	// The placement walk doubles as the claim/unclaim split: hits are the
	// required images (ascending - placement is ordered by image id) and
	// claimedUnion is what separates a restored path from an Unclaimed one.
	claimedUnion := make(map[string]struct{}, len(selected))
	type needed struct {
		image   int64
		forPath string
	}
	var required []needed
	for _, p := range placement {
		var first string
		for _, rel := range p.Paths {
			if _, ok := selected[rel]; !ok {
				continue
			}
			claimedUnion[rel] = struct{}{}
			if first == "" {
				first = rel // placement paths are ascending: first is deterministic
			}
		}
		if first != "" {
			required = append(required, needed{image: p.ImageID, forPath: first})
		}
	}

	for _, n := range required {
		inv, err := store.ImageInventory(ctx, sel.SetID, n.image)
		if err != nil {
			return out, err
		}
		out.Work = append(out.Work, ImageWork{
			Image:     n.image,
			NeededFor: n.forPath,
			Inv:       filterInventory(inv, selected),
		})
	}

	for rel := range selected {
		if _, ok := claimedUnion[rel]; !ok {
			out.Unclaimed = append(out.Unclaimed, rel)
		}
	}
	sort.Strings(out.Unclaimed)

	out.Paths = make([]string, 0, len(selected))
	for rel := range selected {
		out.Paths = append(out.Paths, rel)
	}
	sort.Strings(out.Paths)
	return out, nil
}

// matcher is the compiled selector (exact paths + the subtree prefix).
type matcher struct {
	paths   map[string]struct{}
	subtree string
}

// buildMatcher validates and compiles the selector inputs. Both selectors go
// through media.ValidateRelPath (the single naming authority of the media
// layer): restore paths are CATALOG relpaths, so an absolute path, a ".."
// escape, or a NUL is an input error, not something to resolve against the
// filesystem. --subtree additionally tolerates the two cosmetic spellings a
// user naturally types - a trailing slash ("docs/") and a leading "./" -
// because those do not change which directory was meant; every other
// malformed shape is refused with the media validator's reason.
func buildMatcher(sel Selector) (matcher, error) {
	m := matcher{paths: make(map[string]struct{}, len(sel.Paths))}
	for _, p := range sel.Paths {
		norm, err := normalizeRelPath(p)
		if err != nil {
			return m, inputErrorf("--path %q: %v", p, err)
		}
		m.paths[norm] = struct{}{}
	}
	if sel.Subtree != "" {
		norm, err := normalizeRelPath(sel.Subtree)
		if err != nil {
			return m, inputErrorf("--subtree %q: %v", sel.Subtree, err)
		}
		m.subtree = norm
	}
	return m, nil
}

// normalizeRelPath trims the cosmetic spellings (a trailing "/", a leading
// "./") and runs the media validator on the remainder.
func normalizeRelPath(p string) (string, error) {
	norm := strings.TrimPrefix(p, "./")
	for strings.HasSuffix(norm, "/") && len(norm) > 1 {
		norm = norm[:len(norm)-1]
	}
	if norm == "" || norm == "/" {
		return "", inputErrorf("empty path")
	}
	if err := media.ValidateRelPath(norm); err != nil {
		return "", err
	}
	return norm, nil
}

// matches reports whether one catalog relpath is selected. An empty selector
// (whole set, 8.2: "the latest state of every active record") matches
// everything.
func (m matcher) matches(rel string) bool {
	if len(m.paths) == 0 && m.subtree == "" {
		return true
	}
	if _, ok := m.paths[rel]; ok {
		return true
	}
	if m.subtree != "" && (rel == m.subtree || strings.HasPrefix(rel, m.subtree+"/")) {
		return true
	}
	return false
}

// gate enforces "every selector names something real": each --path entry and
// the --subtree must match at least one active record or directory, and a run
// with no selector at all needs a set that has active records. The messages
// name the offending input and point at --subtree for directory prefixes
// (DESIGN_SPEC 8.2: --path restores EXPLICIT paths).
func (m matcher) gate(sel Selector, records []Record, dirs []DirRecord) error {
	if len(m.paths) == 0 && m.subtree == "" {
		if len(records) == 0 && len(dirs) == 0 {
			return inputErrorf("set %q has no active records (run 'isopress scan' first, or check --set-id)", sel.SetID)
		}
		return nil
	}
	// One pass over the active universe (records + dirs) serves EVERY
	// selector entry: membership is O(1) per --path and one O(records) sweep
	// for the subtree, so a long --path list can never make the gate
	// quadratic in the set's size.
	active := make(map[string]struct{}, len(records)+len(dirs))
	for _, rec := range records {
		active[rec.RelPath] = struct{}{}
	}
	for _, d := range dirs {
		active[d.RelPath] = struct{}{}
	}
	for _, p := range sel.Paths {
		norm, err := normalizeRelPath(p)
		if err != nil {
			return inputErrorf("--path %q: %v", p, err)
		}
		if _, ok := active[norm]; !ok {
			return inputErrorf(
				"--path %q matches no active record of set %q (paths are catalog relpaths; use --subtree to restore a directory prefix)",
				p, sel.SetID)
		}
	}
	if m.subtree != "" {
		sub := m.subtree
		matched := false
		for rel := range active {
			if rel == sub || strings.HasPrefix(rel, sub+"/") {
				matched = true
				break
			}
		}
		if !matched {
			return inputErrorf("--subtree %q matches no active record of set %q", sel.Subtree, sel.SetID)
		}
	}
	return nil
}

// filterInventory projects one image's inventory onto the selected relpaths:
// whole files, symlinks, and hard-link secondaries by their OWN name (the
// name that must appear under --target), chunk rows by their PARENT's relpath
// (a selected chunked record needs every chunk this image holds of it), and
// empty dirs by their path.
//
// The hard-link consequence is the 8.2 rule in code: a family whose primary
// is NOT selected still reaches the primary's image through its selected
// secondary (placement claimed it there), carrying PrimaryRelPath so T5.4 can
// read the bytes from the primary's stored copy while creating ONLY the
// requested name.
func filterInventory(inv Inventory, selected map[string]struct{}) Inventory {
	var out Inventory
	for _, f := range inv.Files {
		if _, ok := selected[f.RelPath]; ok {
			out.Files = append(out.Files, f)
		}
	}
	for _, s := range inv.Symlinks {
		if _, ok := selected[s.RelPath]; ok {
			out.Symlinks = append(out.Symlinks, s)
		}
	}
	for _, c := range inv.Chunks {
		if _, ok := selected[c.ParentRelPath]; ok {
			out.Chunks = append(out.Chunks, c)
		}
	}
	for _, d := range inv.Dirs {
		if _, ok := selected[d.RelPath]; ok {
			out.Dirs = append(out.Dirs, d)
		}
	}
	for _, l := range inv.Secondaries {
		if _, ok := selected[l.RelPath]; ok {
			out.Secondaries = append(out.Secondaries, l)
		}
	}
	return out
}
