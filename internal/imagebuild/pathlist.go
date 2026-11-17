package imagebuild

// T4.2: the -graft-points pathlist file (DESIGN_SPEC 6.2: "-path-list
// reads pathspecs line by line from a file, and -graft-points maps each
// line as iso_path=disk_path"; IMPLEMENTATION_PLAN T4.2 pathlist
// generation).
//
// One line per member: "<iso path>=<staged disk path>", for example
//
//	/chunks/docs/report.pdf/00001=/tmp/isopress-imagebuild-123/chunks/docs/report.pdf/00001
//	/files/photos/cat.jpg=/tmp/isopress-imagebuild-123/files/photos/cat.jpg
//
// The ISO paths are the 5.3/6.4 logical tree paths with a leading
// slash (/files/<relpath>, /chunks/<relpath>/NNNNN); the disk paths are
// the ABSOLUTE staged paths under the workspace root. The lines are
// SORTED by ISO path: xorriso's input order is then deterministic
// (stable image layout for byte-comparisons and golden tests) whatever
// order the inventory happened to carry. The pathlist file itself is
// NEVER a member: only graft-listed paths are mastered, so the file can
// safely live at the staging root (the 6.2 template's
// "<workdir>/pathlist.txt").
//
// T4.3 extends the payload pathlist Stage wrote with the two embedded
// image-root members before invoking xorriso (Staged.EmbeddedPathListEntries
// + WritePathList; the staging file doc's boundary section). Because
// WritePathList always sorts, the extended file stays deterministic.
//
// GATE (defense in depth, mirroring the staging validation): a line
// must carry a leading-slash ISO path, and neither side may contain a
// newline, CR, '=' or NUL - the line-orientation and first-'=' split of
// the -graft-points format make such bytes unrepresentable (see
// staging.go's graft gate; entries reaching here from Stage already
// passed it, but WritePathList is exported and T4.3 feeds it entries
// from another source - the manifest/export names are constants, but
// the gate costs nothing and keeps the invariant local).

import (
	"fmt"
	"os"
	"sort"
	"strings"
)

// PathListEntry is one "-graft-points" pathspec: the ISO path that
// must appear in the image, mapped to the disk path holding its bytes.
type PathListEntry struct {
	// ISOPath is the member's path inside the image, slash-separated
	// with a leading slash ("/files/a/b.txt", "/chunks/x/00001",
	// "/MANIFEST.json").
	ISOPath string
	// DiskPath is the absolute staged path xorriso reads for the
	// member's bytes.
	DiskPath string
}

// SortPathList sorts entries by ISO path ascending (the deterministic
// order of the pathlist file and of every consumer built on it).
func SortPathList(entries []PathListEntry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].ISOPath < entries[j].ISOPath })
}

// WritePathList writes entries to path, sorted by ISO path, one
// "<iso>=<disk>" line per entry with a trailing newline (xorriso's
// line reader accepts it; a missing final newline would make the last
// pathspec fragile under some readers). Entries are validated per the
// file-doc gate; a duplicate ISO path is refused (a corrupt inventory
// claiming one member twice would otherwise be mastered twice, silently
// last-wins).
func WritePathList(path string, entries []PathListEntry) error {
	sorted := make([]PathListEntry, len(entries))
	copy(sorted, entries)
	SortPathList(sorted)

	var b strings.Builder
	for i, e := range sorted {
		if err := validatePathListEntry(e); err != nil {
			return err
		}
		if i > 0 && sorted[i-1].ISOPath == e.ISOPath {
			return fmt.Errorf("imagebuild: pathlist duplicate ISO path %q (claimed twice; corrupt inventory)", e.ISOPath)
		}
		b.WriteString(e.ISOPath)
		b.WriteByte('=')
		b.WriteString(e.DiskPath)
		b.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("imagebuild: writing pathlist %s: %w", path, err)
	}
	return nil
}

// validatePathListEntry enforces the graft-points line invariants (see
// the file doc gate).
func validatePathListEntry(e PathListEntry) error {
	switch {
	case e.ISOPath == "":
		return fmt.Errorf("imagebuild: pathlist entry has an empty ISO path")
	case !strings.HasPrefix(e.ISOPath, "/"):
		return fmt.Errorf("imagebuild: pathlist ISO path %q must start with \"/\" (the 6.2 graft form)", e.ISOPath)
	case e.DiskPath == "":
		return fmt.Errorf("imagebuild: pathlist entry for %q has an empty disk path", e.ISOPath)
	}
	for _, s := range []struct{ side, value string }{{"ISO path", e.ISOPath}, {"disk path", e.DiskPath}} {
		if i := strings.IndexAny(s.value, "\n\r=\x00"); i >= 0 {
			return fmt.Errorf("imagebuild: pathlist %s %q contains %q, which the line-oriented -graft-points format cannot express",
				s.side, s.value, string(s.value[i]))
		}
	}
	return nil
}

// buildPathList converts staged member refs into sorted pathlist
// entries (the Stage-internal bridge to WritePathList; the disk paths
// were derived from the workspace root at staging time).
func buildPathList(members []memberRef) []PathListEntry {
	out := make([]PathListEntry, 0, len(members))
	for _, m := range members {
		out = append(out, PathListEntry{ISOPath: m.isoPath, DiskPath: m.diskPath})
	}
	SortPathList(out)
	return out
}
