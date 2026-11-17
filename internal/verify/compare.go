package verify

// T5.1: the content comparison, MOVED here from internal/imagebuild
// (IMPLEMENTATION_PLAN 5.5 resolution 4: the comparison generalizes out
// of the build self-verify into internal/verify, the layer-correct
// direction - imagebuild already depends on verify for ExtractAndHash,
// and verify must NEVER import imagebuild).
//
// CompareContents is the ONE comparison both verifications use:
//
//   - the T4.3 build self-verify (6.3 step 2, 7.1) compares the
//     step-7 extraction against the 6.6 content list the builder
//     computed;
//   - T5.1's verify-media (7.2) compares an ISO extraction - or a copy
//     tree read DIRECTLY, same walk, same entries - against the
//     catalog's image_contents list.
//
// The rules are byte-for-byte the behavior the T4.3 tests pinned
// (internal/imagebuild/build_test.go TestCompareContents and the real
// e2e in build_real_test.go - both still exercise this code through
// imagebuild's one-line delegate):
//
//   - every content row must be present with the right kind;
//   - file, chunk, manifest, and export rows must match by SHA-256 AND
//     byte size (the strongest attribute of each kind);
//   - symlink rows must carry the RECORDED target verbatim (the osirrox
//     contract: links restore AS links - dangling ones included - so a
//     target comparison is both possible and sufficient);
//   - dir rows must exist as dirs (existence only - extraction as an
//     unprivileged user cannot reproduce ownership, and 7.2's anchor is
//     content, not metadata);
//   - the extraction must contain NO member outside the list: a rogue
//     file or symlink is the unexpected class FR-021 reports; an
//     unexpected DIRECTORY is tolerated only when it is an implied
//     parent of a listed path (the 5.3 tree's non-empty parents, which
//     the content list never records).
//
// The result type carries the per-category lists verify-media reports
// (matched / mismatched-with-reason / missing / unexpected /
// unreadable) in addition to the flat Problems() render the build
// error message uses - one derivation, two presentations.

import (
	"bytes"
	"fmt"

	"isopress/internal/media"
)

// The two image-root self-member kinds of the 6.6 content list (the
// media.Kind* payload kinds cover the rest). Exported since T5.1: the
// read side (verify-media) classifies MANIFEST.json and
// catalog-export.sqlite rows through these, and the catalog read and
// the builder share the one spelling.
const (
	KindManifest = "manifest"
	KindExport   = "export"
)

// ContentRow is one FINAL image_contents row (the 2.3 column set) as
// the comparison consumes it. FileID 0 means "no file" (dir, manifest,
// export rows); ChunkIndex -1 means "not a chunk". The symlink TARGET
// deliberately does NOT travel on the row (the imagebuild behavior,
// kept): the builder derives it from its inventory and the read side
// joins files.link_target - both hand it over through the targets map
// CompareContents takes.
type ContentRow struct {
	MediaPath  string
	Kind       string // media.Kind* | KindManifest | KindExport
	FileID     int64
	ChunkIndex int
	Size       int64
	Hash       []byte // 32-byte sha256; nil for dir/symlink (2.3)
}

// Mismatch is one listed member that EXISTS in the tree under
// verification but does not match its content row. Reasons carries the
// expected-vs-actual detail lines (a file can fail BOTH the hash and
// the size check); the per-file failure lines on stdout are built from
// it.
type Mismatch struct {
	MediaPath string
	Reasons   []string
}

// Missing is one listed member absent from the tree under
// verification.
type Missing struct {
	MediaPath string
	Kind      string // the content row's kind
}

// Unexpected is one tree member outside the content list (the FR-021
// unexpected class; implied parent dirs never reach this list).
type Unexpected struct {
	MediaPath string
	Kind      string // the WALKED kind: file | dir | symlink
}

// Unreadable is one listed member whose bytes could not be read or
// hashed (7.2's unreadable class: EACCES, I/O error). The walk records
// these instead of aborting the whole run (extract.go), so one
// unreadable member never hides the verdicts of every other member.
type Unreadable struct {
	MediaPath string
	Detail    string
}

// Comparison is the categorized outcome of CompareContents. The
// category slices carry the structured data verify-media reports per
// image (matched / mismatched / missing / unexpected / unreadable
// counts plus per-file lines); Problems() carries the flat,
// byte-identical render of the same findings the T4.3 error message
// pins.
type Comparison struct {
	// Matched are the content rows fully satisfied by the tree, in
	// row order.
	Matched []string
	// Mismatched are the rows present but wrong, in row order.
	Mismatched []Mismatch
	// Missing are the rows absent from the tree, in row order.
	Missing []Missing
	// Unexpected are the tree members outside the list, in walk
	// order (implied parents excluded).
	Unexpected []Unexpected
	// Unreadable are the listed members whose bytes could not be
	// hashed, in row order.
	Unreadable []Unreadable

	// problems is the flattened render (the T4.3 problem lines),
	// built DURING the single comparison pass so its ordering is
	// exactly the historical one: per content row in row order
	// (missing / unreadable / mismatch lines), then the unexpected
	// entries in walk order. Problems() returns a copy.
	problems []string
}

// Clean reports that every listed member matched and nothing
// unexpected was found - the 7.2 "full match" predicate that gates the
// verified_at stamp.
func (c Comparison) Clean() bool {
	return len(c.Mismatched) == 0 && len(c.Missing) == 0 &&
		len(c.Unexpected) == 0 && len(c.Unreadable) == 0
}

// Anomalies is the total number of findings across the four anomaly
// categories (the CLI's exit-10 accounting).
func (c Comparison) Anomalies() int {
	return len(c.Mismatched) + len(c.Missing) + len(c.Unexpected) + len(c.Unreadable)
}

// Problems returns the flat problem lines, byte-identical to the T4.3
// comparison's output (the build error message and its pin tests read
// this shape).
func (c Comparison) Problems() []string {
	out := make([]string, len(c.problems))
	copy(out, c.problems)
	return out
}

// CompareContents compares ONE image's finalized content list against
// the hashed members of the tree under verification: the extraction of
// a re-read ISO, or a copy tree read directly (7.2 - "same comparison
// logic" for both source modes; the entries come from the same hash
// walk either way).
//
// targets maps media path -> recorded symlink target for the listed
// symlink rows (see ContentRow).
func CompareContents(rows []ContentRow, extracted []Entry, targets map[string]string) Comparison {
	var c Comparison
	byPath := make(map[string]Entry, len(extracted))
	for _, e := range extracted {
		byPath[e.MediaPath] = e
	}
	listed := make(map[string]struct{}, len(rows))

	for _, r := range rows {
		listed[r.MediaPath] = struct{}{}
		e, ok := byPath[r.MediaPath]
		if !ok {
			c.Missing = append(c.Missing, Missing{MediaPath: r.MediaPath, Kind: r.Kind})
			c.problems = append(c.problems, fmt.Sprintf("missing from extraction: %s (kind %s)", r.MediaPath, r.Kind))
			continue
		}
		switch r.Kind {
		case media.KindDir:
			if e.Kind != KindDir {
				mismatch(&c, r.MediaPath, fmt.Sprintf("%s: extracted as %s, want dir", r.MediaPath, e.Kind))
				continue
			}
		case media.KindSymlink:
			if e.Kind != KindSymlink {
				mismatch(&c, r.MediaPath, fmt.Sprintf("%s: extracted as %s, want symlink", r.MediaPath, e.Kind))
				continue
			}
			if want := targets[r.MediaPath]; e.LinkTarget != want {
				mismatch(&c, r.MediaPath, fmt.Sprintf("%s: symlink target %q, want %q", r.MediaPath, e.LinkTarget, want))
				continue
			}
		default: // file, chunk, manifest, export: hashed kinds
			if e.Kind != KindFile {
				mismatch(&c, r.MediaPath, fmt.Sprintf("%s: extracted as %s, want file", r.MediaPath, e.Kind))
				continue
			}
			// An UNREADABLE member (7.2's class; the walk records the
			// read failure on the entry) is reported as such - never
			// as a hash mismatch against bytes nobody could read.
			if e.Unreadable != "" {
				c.Unreadable = append(c.Unreadable, Unreadable{MediaPath: r.MediaPath, Detail: e.Unreadable})
				c.problems = append(c.problems, fmt.Sprintf("%s: unreadable (%s)", r.MediaPath, e.Unreadable))
				continue
			}
			var reasons []string
			if !bytes.Equal(e.Hash, r.Hash) {
				reasons = append(reasons, fmt.Sprintf("%s: content hash %x, want %x", r.MediaPath, e.Hash, r.Hash))
			}
			if e.Size != r.Size {
				reasons = append(reasons, fmt.Sprintf("%s: size %d, want %d", r.MediaPath, e.Size, r.Size))
			}
			if len(reasons) > 0 {
				c.Mismatched = append(c.Mismatched, Mismatch{MediaPath: r.MediaPath, Reasons: reasons})
				c.problems = append(c.problems, reasons...)
				continue
			}
		}
		c.Matched = append(c.Matched, r.MediaPath)
	}

	// The unexpected half: nothing outside the content list may exist
	// in the tree (implied parent dirs excepted).
	for _, e := range extracted {
		if _, ok := listed[e.MediaPath]; ok {
			continue
		}
		if e.Kind == KindDir {
			if isImpliedParent(e.MediaPath, listed) {
				continue
			}
			c.Unexpected = append(c.Unexpected, Unexpected{MediaPath: e.MediaPath, Kind: e.Kind})
			c.problems = append(c.problems, fmt.Sprintf("unexpected dir in extraction: %s", e.MediaPath))
			continue
		}
		c.Unexpected = append(c.Unexpected, Unexpected{MediaPath: e.MediaPath, Kind: e.Kind})
		c.problems = append(c.problems, fmt.Sprintf("unexpected %s in extraction: %s", e.Kind, e.MediaPath))
	}
	return c
}

// mismatch records a one-reason mismatch (the historical single-line
// classes: kind, symlink target).
func mismatch(c *Comparison, mediaPath, problem string) {
	c.Mismatched = append(c.Mismatched, Mismatch{MediaPath: mediaPath, Reasons: []string{problem}})
	c.problems = append(c.problems, problem)
}

// isImpliedParent reports whether dir is a path-prefix ancestor of at
// least one listed media path (the 5.3 tree creates non-empty parents
// implicitly; they are never content-list rows).
func isImpliedParent(dir string, listed map[string]struct{}) bool {
	prefix := dir + "/"
	for p := range listed {
		if len(p) > len(prefix) && p[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

// TotalsOf derives the EXACT progress totals of a content list (the
// Totals contract of extract.go; moved here with the comparison so the
// read side computes them through the same derivation the build side
// pins): Entries is every row; Files/Bytes count only the
// REGULAR-FILE rows (file, chunk, manifest, export) - the rows a tree
// materializes as bytes and the hash walk re-hashes (symlinks restore
// as symlinks and dirs add no bytes, so Bytes is exactly what the
// extraction tree-sum and the ItemDone sum converge to; the 2.3
// symlink Size convention of len(target) is DELIBERATELY excluded -
// counting it would push the extract total past what any tree-sum can
// reach).
func TotalsOf(rows []ContentRow) Totals {
	var tot Totals
	tot.Entries = int64(len(rows))
	for _, r := range rows {
		switch r.Kind {
		case media.KindDir, media.KindSymlink:
			// Not re-hashed; adds no file bytes to the tree.
		default: // file, chunk, manifest, export: regular-file rows
			tot.Files++
			tot.Bytes += r.Size
		}
	}
	return tot
}
