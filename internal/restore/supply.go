package restore

// T5.3: image supply (DESIGN_SPEC 8.3; IMPLEMENTATION_PLAN 4.6 T5.3).
//
// A required image is resolved against the three 8.3 sources IN THE
// DESIGN'S STATED ORDER OF PREFERENCE - --image-dir, then explicit
// --image N=PATH mappings, then --copy-tree-root - and each source has its
// own shape, which is why the outcome carries a SourceKind:
//
//   - --image-dir supplies .iso FILES: it discovers
//     <image-dir>/<SET-ID>-NNN.iso through media.ImageISOFileName (the 6.4
//     deterministic naming, the one implementation of that scheme), and a
//     discovered entry only counts when it EXISTS and is a regular file. An
//     image-dir never supplies a copy tree: 8.3 gives it the .iso wording
//     deliberately, and mixing the two shapes into one directory would make
//     "what did I point --image-dir at" undiagnosable.
//   - --copy-tree-root supplies TREES: it discovers
//     <root>/<SET-ID>-NNN/ through media.ImageDirName, counting only an
//     existing directory.
//   - --image N=PATH supplies EITHER: the design lists no shape for it, so
//     stat decides - a regular file is an ISO, a directory is a copy tree
//     (the same rule verify-media derives images.final_path with). Because
//     the user NAMED this path, a path that cannot be read, does not exist,
//     or is neither shape is an INPUT error (exit 2 in the cli), never a
//     "missing media" prompt: the tool must not prompt for something the
//     user already attempted to supply.
//   - --from-media (T5.5, 8.5) supplies TREES FROM THE MEDIA ITSELF: the
//     per-image copy-tree roots ResolveMedia derived from the media's own
//     layout (media.go). It is consulted FIRST, before all three 8.3
//     sources - 8.5's reading is that the media IS the set, so its own
//     resolution is the base supply, and every other flag is a SUPPLEMENT
//     consulted only where the media does not resolve an image (a
//     single-image-tree PATH of a multi-image set, an ISO the user prefers
//     over the media's tree, a sibling disc not in the PATH). 8.5 is silent
//     on coexistence; the honest precedence documented here keeps the 8.3
//     RATIFIED RELATIVE ORDER untouched (image-dir, explicit, copy-tree-
//     root - the brief's recommendation to hoist explicit mappings was
//     considered and rejected: re-ordering the 8.3 sources depending on
//     another flag would make one flag pair behave two ways, breaking the
//     literal-8.3 pin above) and simply prepends the media base.
//
// A source that misses simply falls through to the next one (reason
// recorded); when every source misses, the image is Missing with Sought
// listing every candidate consulted - that list is what the 8.3 prompt and
// the non-TTY machine-readable listing render.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"isopress/internal/media"
)

// shape is how one supply candidate classifies a path it finds.
type shape int

const (
	shapeISO  shape = iota // discovery candidate: regular file only (.iso)
	shapeTree              // discovery candidate: directory only (S-NNN)
	shapeAny               // explicit --image N=PATH: stat decides
)

// supplyCandidate is one path consulted for one image.
type supplyCandidate struct {
	path  string
	kind  shape
	image int64
}

// ParseExplicitImage parses one --image N=PATH value. N must be a positive
// decimal image number (images are 1-based, 6.4) and PATH must be non-empty.
// The value form is fixed here rather than by pflag so a malformed mapping
// fails as a usage error naming the value.
func ParseExplicitImage(v string) (ExplicitImage, error) {
	idx := strings.IndexByte(v, '=')
	if idx < 0 {
		return ExplicitImage{}, inputErrorf("--image %q must have the form N=PATH (e.g. --image 1=/mnt/S-001.iso)", v)
	}
	num, path := v[:idx], v[idx+1:]
	if path == "" {
		return ExplicitImage{}, inputErrorf("--image %q: empty PATH after N=", v)
	}
	n, err := strconv.ParseInt(num, 10, 64)
	if err != nil || n < 1 {
		return ExplicitImage{}, inputErrorf("--image %q: image number %q must be a positive integer (images are 1-based, DESIGN_SPEC 6.4)", v, num)
	}
	return ExplicitImage{Image: n, Path: path}, nil
}

// ResolveSupply resolves every required image (ascending, as ResolveSelection
// produced them) against the SupplySpec. Returns *InputError for a bad
// explicit mapping and plain context errors on interruption.
func ResolveSupply(ctx context.Context, setID string, required []int64, spec SupplySpec) ([]ResolvedSupply, error) {
	out := make([]ResolvedSupply, 0, len(required))
	for _, id := range required {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		res, err := resolveOneSupply(setID, id, spec)
		if err != nil {
			return nil, err
		}
		out = append(out, res)
	}
	return out, nil
}

// resolveOneSupply walks the preference order for ONE image.
func resolveOneSupply(setID string, imageID int64, spec SupplySpec) (ResolvedSupply, error) {
	res := ResolvedSupply{Image: imageID}
	for _, cand := range supplyCandidates(setID, imageID, spec) {
		res.Sought = append(res.Sought, cand.path)
		fi, err := os.Stat(cand.path)

		switch cand.kind {
		case shapeISO:
			if err != nil {
				res.Reason = statReason(cand.path, err)
				continue
			}
			if !fi.Mode().IsRegular() {
				res.Reason = fmt.Sprintf("%s exists but is not a regular file", cand.path)
				continue
			}
			res.Kind, res.Path = SourceISO, cand.path
			return res, nil

		case shapeTree:
			if err != nil {
				res.Reason = statReason(cand.path, err)
				continue
			}
			if !fi.IsDir() {
				res.Reason = fmt.Sprintf("%s exists but is not a directory", cand.path)
				continue
			}
			res.Kind, res.Path = SourceCopyTree, cand.path
			return res, nil

		default: // shapeAny: the user named this path, so failures are inputs
			if err != nil {
				return res, inputErrorf("--image %d=%s cannot be read: %v", cand.image, cand.path, err)
			}
			switch {
			case fi.Mode().IsRegular():
				res.Kind, res.Path = SourceISO, cand.path
			case fi.IsDir():
				res.Kind, res.Path = SourceCopyTree, cand.path
			default:
				return res, inputErrorf("--image %d=%s: %s is neither a regular file (.iso) nor a directory (copy tree)",
					cand.image, cand.path, fi.Mode())
			}
			return res, nil
		}
	}
	res.Missing = true
	return res, nil
}

// supplyCandidates builds the preference-ordered candidate list for one
// image: the --from-media media tree first (T5.5, 8.5's base supply -
// see the file doc), then the literal 8.3 order - --image-dir, then the
// explicit mapping, then the copy-tree root.
func supplyCandidates(setID string, imageID int64, spec SupplySpec) []supplyCandidate {
	var out []supplyCandidate
	if tree, ok := spec.Media[imageID]; ok && tree != "" {
		out = append(out, supplyCandidate{path: tree, kind: shapeTree, image: imageID})
	}
	if spec.ImageDir != "" {
		if name, err := media.ImageISOFileName(setID, imageID); err == nil {
			out = append(out, supplyCandidate{
				path: filepath.Join(spec.ImageDir, name), kind: shapeISO, image: imageID,
			})
		}
	}
	for _, e := range spec.Explicit {
		if e.Image == imageID {
			out = append(out, supplyCandidate{path: e.Path, kind: shapeAny, image: imageID})
		}
	}
	if spec.CopyTreeRoot != "" {
		if name, err := media.ImageDirName(setID, imageID); err == nil {
			out = append(out, supplyCandidate{
				path: filepath.Join(spec.CopyTreeRoot, name), kind: shapeTree, image: imageID,
			})
		}
	}
	return out
}

// statReason renders a discovery miss (a candidate the tool went looking for,
// so absence is normal, not an input error).
func statReason(path string, err error) string {
	if os.IsNotExist(err) {
		return fmt.Sprintf("%s not found", path)
	}
	return fmt.Sprintf("%s: %v", path, err)
}

// MissingImageLine renders ONE machine-readable listing line of the 8.3
// non-TTY path (RCR-3: "a machine-readable required-images error"). Fixed
// key=value form, one line per missing image, printed on the run's stderr
// through the progress bus (the renderer prefixes it with "error: ", and the
// non-TTY gate means the plain renderer - so the shape is deterministic for
// cron to grep):
//
//	required-image: image=2 set=<SET-ID> sought=<candidate[,candidate]> needed-for=<relpath>
//
// "sought" is "-" when no supply source was configured at all (nothing could
// even be looked for), and every value is a catalog relpath or a filesystem
// path.
func MissingImageLine(m MissingImage) string {
	sought := "-"
	if len(m.Sought) > 0 {
		sought = strings.Join(m.Sought, ",")
	}
	needed := m.NeededFor
	if needed == "" {
		needed = "-"
	}
	return fmt.Sprintf("required-image: image=%d set=%s sought=%s needed-for=%s", m.Image, m.SetID, sought, needed)
}
