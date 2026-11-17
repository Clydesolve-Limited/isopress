package verify

// T5.2 HARDENING: adversarial gaps the original matrix left open. A
// validate verdict diagnoses BACKUP HEALTH, so every classification
// boundary this file touches is pinned THROUGH the real comparison, not
// just reviewed:
//
//   - the changed/corrupt boundary under EVERY drift combination the
//     resolved comparison set can see (no drift, mtime-only, size-only,
//     inode-only, size+mtime+inode) - each subtest PRECONDITION-asserts
//     the drift through the same fsmeta.DiffFields the comparison uses,
//     so a fixture that fails to drift fails LOUDLY instead of silently
//     validating the wrong branch of the boundary;
//   - the bit-rot fixture's exactness precondition: a same-length write
//     plus a restored mtime is only an honest corrupt fixture when the
//     metadata comparison is genuinely ZERO under both modes - on a
//     coarse-mtime filesystem the Chtimes restore would not round-trip
//     and the fixture would (correctly) classify changed, so the
//     precondition is asserted, not assumed;
//   - symlink edges the matrix left out: kind mismatch in BOTH
//     directions (symlink->regular and regular->symlink), a symlink
//     whose target is a DIRECTORY (ok - validate never follows), and
//     two symlinks with SWAPPED targets (both changed);
//   - the determinate-phase arithmetic against mixed record kinds: the
//     byte totals must equal the sum of the RECORDED sizes (symlink
//     len(target) included, missing records reporting their recorded
//     size), and the ItemDone count must equal the record count;
//   - a vanished source root: every record reports missing, the run
//     does not abort, and `new` discovery reports nothing.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"isopress/internal/fsmeta"
	"isopress/internal/progress"
	"isopress/internal/units"
)

// --- precondition helper -------------------------------------------------------------

// assertRecordDrift PRECONDITION-asserts the fixture's metadata drift
// through the ONE fsmeta.DiffFields the comparison consumes: the full
// set and the metadata set must both show exactly wantFull / wantMeta
// BEFORE the classification can be trusted. A fixture that cannot
// produce its intended drift (for example a same-inode truncation on a
// filesystem that rewrites through a fresh inode) fails here with the
// observed fields named - loudly - instead of letting the validate run
// "pass" while actually exercising a different branch of the boundary.
func assertRecordDrift(t *testing.T, rec SourceRecord, path string, wantFull, wantMeta fsmeta.Diff) {
	t.Helper()
	cur, err := fsmeta.PathMeta(path)
	if err != nil {
		t.Fatalf("re-stat %s: %v", path, err)
	}
	if got := fsmeta.DiffFields(recordMeta(rec), cur, fsmeta.ModeFull); got != wantFull {
		t.Fatalf("fixture precondition (full set): drift = %s, want %s (the filesystem did not honour the intended mutation)", got, wantFull)
	}
	if got := fsmeta.DiffFields(recordMeta(rec), cur,
		fsmeta.Mode{Set: fsmeta.ComparisonMetadata, Forced: true}); got != wantMeta {
		t.Fatalf("fixture precondition (metadata set): drift = %s, want %s (the filesystem did not honour the intended mutation)", got, wantMeta)
	}
}

// flipSameLen rewrites path with content of the SAME length through
// O_CREATE|O_TRUNC on the existing path (os.WriteFile), which on Linux
// local filesystems reuses the inode - the same-length flip whose
// inode survival the corrupt fixtures depend on. The precondition
// assertion in each subtest verifies the reuse actually happened.
func flipSameLen(t *testing.T, path, content string) {
	t.Helper()
	vWrite(t, path, content)
}

// --- the boundary under every drift combination --------------------------------------

// TestValidateCorruptChangedBoundaryUnderEveryMode drives a SAME-LENGTH
// content flip (plus one deliberate size drift) through every drift
// combination the resolved comparison set can see and asserts BOTH
// mode verdicts (the 7.3 two-gate rule and the CIFS fix's Mode
// threading, at the service tier):
//
//	drift                 full set    metadata set
//	(nothing)             corrupt     corrupt   (both gates open: hash differs)
//	mtime_ns only         changed     changed   (mtime is in BOTH sets)
//	size only             changed     changed   (size is in BOTH sets)
//	inode only            changed     corrupt   (the CIFS seam: the set
//	                                            masks the identifier)
//	size+mtime+inode      changed     changed   (each set sees size+mtime)
//
// Dev drift is deliberately not fabricated: dev is constant for every
// filesystem a test fixture can live on, and it threads through the
// identical mode gate as inode (DiffFields masks both under the
// metadata set) - pinned by fsmeta's own DiffFields tests.
func TestValidateCorruptChangedBoundaryUnderEveryMode(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(t *testing.T, path string, origModTime time.Time)
		wantFull fsmeta.Diff // precondition drift under the full set
		wantMeta fsmeta.Diff // precondition drift under the metadata set
		verdict  string      // CatChanged or CatCorrupt in BOTH modes
		detail   string      // pinned finding detail (changed cases only)
	}{
		{
			name: "no metadata drift",
			mutate: func(t *testing.T, path string, orig time.Time) {
				flipSameLen(t, path, "BBBBBBBB")
				if err := os.Chtimes(path, orig, orig); err != nil {
					t.Fatal(err)
				}
			},
			verdict: CatCorrupt,
		},
		{
			name: "mtime-only drift",
			mutate: func(t *testing.T, path string, _ time.Time) {
				flipSameLen(t, path, "BBBBBBBB")
				moved := time.Now().Add(2 * time.Hour)
				if err := os.Chtimes(path, moved, moved); err != nil {
					t.Fatal(err)
				}
			},
			wantFull: fsmeta.DiffMtimeNS,
			wantMeta: fsmeta.DiffMtimeNS,
			verdict:  CatChanged,
			detail:   "metadata: mtime_ns; hash differs",
		},
		{
			name: "size-only drift",
			mutate: func(t *testing.T, path string, orig time.Time) {
				flipSameLen(t, path, "BBBBBBBBBBBB") // 8 -> 12 bytes
				if err := os.Chtimes(path, orig, orig); err != nil {
					t.Fatal(err)
				}
			},
			wantFull: fsmeta.DiffSize,
			wantMeta: fsmeta.DiffSize,
			verdict:  CatChanged,
			detail:   "metadata: size; hash differs",
		},
		{
			name: "inode-only drift",
			mutate: func(t *testing.T, path string, orig time.Time) {
				// Whole-file replacement through a rename: a fresh
				// inode, then the original mtime restored - the CIFS
				// class's synthesized identifier in miniature. Same
				// length, same mtime: ONLY the inode separates the
				// two verdicts.
				tmp := path + ".tmp-replace"
				vWrite(t, tmp, "BBBBBBBB")
				if err := os.Rename(tmp, path); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(path, orig, orig); err != nil {
					t.Fatal(err)
				}
			},
			wantFull: fsmeta.DiffInode,
			verdict:  "",
			detail:   "",
		},
		{
			name: "size and mtime and inode drift",
			mutate: func(t *testing.T, path string, orig time.Time) {
				// The kitchen sink: a longer replacement through a
				// rename with a fresh mtime - every field of the full
				// set differs, both visible sets see size+mtime.
				tmp := path + ".tmp-replace"
				vWrite(t, tmp, "BBBBBBBBBBBBBBBB")
				if err := os.Rename(tmp, path); err != nil {
					t.Fatal(err)
				}
				// EXPLICIT mtime move (>= 2s, absolute): the rename's
				// fresh inode alone does not move mtime if the new
				// file lands in the same timestamp tick as the record
				// - CI filesystems may round mtimes to 1s granularity,
				// so the mtime_ns leg of the fixture PRECONDITION is
				// guaranteed by an absolute Chtimes (+10s for margin)
				// rather than by the replacement write.
				moved := orig.Add(10 * time.Second)
				if err := os.Chtimes(path, moved, moved); err != nil {
					t.Fatal(err)
				}
			},
			wantFull: fsmeta.DiffSize | fsmeta.DiffMtimeNS | fsmeta.DiffInode,
			wantMeta: fsmeta.DiffSize | fsmeta.DiffMtimeNS,
			verdict:  CatChanged,
			// The detail names the per-set drifted fields (they differ
			// per set: the full set adds inode), so it is left to the
			// mode-specific pins; the substring below must hold in
			// BOTH sets.
			detail: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			path := filepath.Join(root, "target.bin")
			vWrite(t, path, "AAAAAAAA")
			rec := vRec(t, root, "target.bin", 1)
			orig, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}

			tc.mutate(t, path, orig.ModTime())

			// THE PRECONDITION: the drift the case claims must be the
			// drift the ONE DiffFields actually sees, under both sets.
			assertRecordDrift(t, rec, path, tc.wantFull, tc.wantMeta)

			modes := []struct {
				mode fsmeta.Mode
				name string
			}{
				{fsmeta.ModeFull, "full"},
				{fsmeta.Mode{Set: fsmeta.ComparisonMetadata, Forced: true}, "metadata"},
			}
			// The inode-only case splits the verdicts per set; every
			// other case shares one verdict. expected() maps the set
			// name to the category.
			expected := func(set string) string {
				if tc.name == "inode-only drift" {
					if set == "full" {
						return CatChanged
					}
					return CatCorrupt
				}
				return tc.verdict
			}
			for _, m := range modes {
				res, err := Validate(context.Background(), ValidateInput{
					Root: root, Records: []SourceRecord{rec}, Mode: m.mode,
				}, nil)
				if err != nil {
					t.Fatalf("Validate (%s): %v", m.name, err)
				}
				want := expected(m.name)
				if want == CatCorrupt {
					if res.Corrupt != 1 || res.Changed != 0 || res.Anomalies() != 1 {
						t.Errorf("%s mode: %+v (findings: %s); want corrupt=1 (hash differs, the %s set sees no drift)",
							m.name, res, findingsBrief(res), m.name)
						continue
					}
					if f := mustFinding(t, res, "target.bin"); f.Detail != "bit rot: metadata matches, hash differs" {
						t.Errorf("%s mode detail = %q, want the bit-rot detail", m.name, f.Detail)
					}
					continue
				}
				if res.Changed != 1 || res.Corrupt != 0 || res.Anomalies() != 1 {
					t.Errorf("%s mode: %+v (findings: %s); want changed=1 (hash differs and the %s set sees drift %s)",
						m.name, res, findingsBrief(res), m.name, tc.wantFull)
					continue
				}
				f := mustFinding(t, res, "target.bin")
				if tc.detail != "" {
					if f.Detail != tc.detail {
						t.Errorf("%s mode detail = %q, want %q", m.name, f.Detail, tc.detail)
					}
				} else if !strings.Contains(f.Detail, "hash differs") || !strings.HasPrefix(f.Detail, "metadata: ") {
					// The unpinned-detail case still must render the
					// two-gate finding shape.
					t.Errorf("%s mode detail = %q, want the \"metadata: ...; hash differs\" shape", m.name, f.Detail)
				}
			}
		})
	}
}

// --- the bit-rot fixture's honesty precondition --------------------------------------

// TestValidateBitRotFixturePreconditionPinned pins the precondition
// the WHOLE bit-rot test family rests on: the same-length write plus
// the restored original mtime must leave the metadata comparison
// EXACTLY ZERO under BOTH modes before the corrupt classification can
// be trusted. A filesystem whose mtime granularity would not round-trip
// the Chtimes restore (coarse mtime) would show a residual drift here
// and classify the fixture changed - a false-negative for bit rot that
// the fixture must surface honestly rather than mask. The corrupt
// verdict is then asserted under BOTH modes (nothing drifts in either
// set, so the two-gate rule opens the corrupt branch identically).
func TestValidateBitRotFixturePreconditionPinned(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "rot.bin")
	vWrite(t, path, "0123456789abcdef")
	rec := vRec(t, root, "rot.bin", 1)
	orig, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	// The T5.2 bit-rot mutation: same 16-byte length, different bytes,
	// ORIGINAL mtime restored.
	flipSameLen(t, path, "fedcba9876543210")
	if err := os.Chtimes(path, orig.ModTime(), orig.ModTime()); err != nil {
		t.Fatal(err)
	}

	// THE precondition, asserted BEFORE any classification is trusted:
	// the drift is genuinely zero under both resolved sets (size,
	// mtime_ns, dev AND inode all match the catalog).
	assertRecordDrift(t, rec, path, 0, 0)

	for _, m := range []struct {
		name string
		mode fsmeta.Mode
	}{
		{"full", fsmeta.ModeFull},
		{"metadata", fsmeta.Mode{Set: fsmeta.ComparisonMetadata, Forced: true}},
	} {
		res, err := Validate(context.Background(), ValidateInput{
			Root: root, Records: []SourceRecord{rec}, Mode: m.mode,
		}, nil)
		if err != nil {
			t.Fatalf("Validate (%s): %v", m.name, err)
		}
		if res.Corrupt != 1 || res.Changed != 0 {
			t.Errorf("%s mode: %+v (findings: %s); want corrupt=1 (metadata matches exactly, hash differs)",
				m.name, res, findingsBrief(res))
			continue
		}
		if f := mustFinding(t, res, "rot.bin"); f.Detail != "bit rot: metadata matches, hash differs" {
			t.Errorf("%s mode detail = %q, want the bit-rot detail", m.name, f.Detail)
		}
	}
}

// --- symlink comparison edges ---------------------------------------------------------

// TestValidateSymlinkComparisonEdges pins the four symlink readings
// the original matrix left open:
//
//   - kind mismatch in BOTH directions: a symlink REPLACED by a regular
//     file, and a regular record REPLACED by a symlink, are each
//     `changed` (the structural difference alone proves a modification;
//     content is not comparable across kinds);
//   - a symlink whose Readlink succeeds and whose recorded target is a
//     DIRECTORY is `ok` - validate compares the target STRING and never
//     follows it (D-03), so the target's own kind is irrelevant;
//   - two symlinks whose targets are SWAPPED are BOTH `changed` - each
//     individually sees a different target string, unconditionally.
func TestValidateSymlinkComparisonEdges(t *testing.T) {
	root := t.TempDir()
	vWrite(t, filepath.Join(root, "a.txt"), "A content\n")
	vWrite(t, filepath.Join(root, "b.txt"), "B content\n")
	if err := os.Mkdir(filepath.Join(root, "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	mklink := func(name, target string) {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatalf("symlink %s: %v", name, err)
		}
	}
	mklink("l-kindswap", "a.txt") // -> replaced by a regular file
	mklink("l-dir", "adir")       // -> target is a directory: ok
	mklink("l-swap1", "a.txt")    // -> swapped with l-swap2
	mklink("l-swap2", "b.txt")    // -> swapped with l-swap1
	vWrite(t, filepath.Join(root, "r-kindswap.txt"), "regular for now\n")

	rels := []string{"a.txt", "b.txt", "l-kindswap", "l-dir", "l-swap1", "l-swap2", "r-kindswap.txt"}
	recs := make([]SourceRecord, 0, len(rels))
	for i, rel := range rels {
		recs = append(recs, vRec(t, root, rel, int64(i+1)))
	}

	// Mutation 1: the symlink becomes a REGULAR file at the same path.
	if err := os.Remove(filepath.Join(root, "l-kindswap")); err != nil {
		t.Fatal(err)
	}
	vWrite(t, filepath.Join(root, "l-kindswap"), "regular file took the symlink path\n")
	// Mutation 2: the regular record becomes a SYMLINK.
	if err := os.Remove(filepath.Join(root, "r-kindswap.txt")); err != nil {
		t.Fatal(err)
	}
	mklink("r-kindswap.txt", "a.txt")
	// Mutation 3: swap the two symlink targets.
	if err := os.Remove(filepath.Join(root, "l-swap1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "l-swap2")); err != nil {
		t.Fatal(err)
	}
	mklink("l-swap1", "b.txt")
	mklink("l-swap2", "a.txt")

	res, err := Validate(context.Background(), ValidateInput{
		Root: root, Records: recs, Mode: fsmeta.ModeFull,
	}, nil)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	// Kind mismatch, both directions: `changed` with the transition
	// named (the default branch of the comparison).
	if f := mustFinding(t, res, "l-kindswap"); f.Category != CatChanged ||
		f.Detail != "entry kind changed: symlink -> regular (content not comparable across kinds)" {
		t.Errorf("l-kindswap finding = %+v, want the symlink -> regular kind-mismatch changed", f)
	}
	if f := mustFinding(t, res, "r-kindswap.txt"); f.Category != CatChanged ||
		f.Detail != "entry kind changed: regular -> symlink (content not comparable across kinds)" {
		t.Errorf("r-kindswap.txt finding = %+v, want the regular -> symlink kind-mismatch changed", f)
	}
	// Swapped targets: BOTH changed, each naming its own transition.
	if f := mustFinding(t, res, "l-swap1"); f.Category != CatChanged ||
		f.Detail != "symlink target differs: recorded \"a.txt\", now \"b.txt\"" {
		t.Errorf("l-swap1 finding = %+v, want changed with the a.txt -> b.txt transition", f)
	}
	if f := mustFinding(t, res, "l-swap2"); f.Category != CatChanged ||
		f.Detail != "symlink target differs: recorded \"b.txt\", now \"a.txt\"" {
		t.Errorf("l-swap2 finding = %+v, want changed with the b.txt -> a.txt transition", f)
	}
	// The directory-target symlink is OK and absent from the findings:
	// the target STRING matches, and validate never follows it.
	for _, f := range res.Findings {
		if f.Path == "l-dir" {
			t.Errorf("l-dir must be ok (target string intact; D-03 never follows) but was categorised %q (%s)", f.Category, f.Detail)
		}
	}
	if res.OK != 3 { // a.txt, b.txt, l-dir
		t.Errorf("ok = %d, want 3 (a.txt, b.txt, and the directory-target symlink)", res.OK)
	}
	if res.Changed != 4 || res.Anomalies() != 4 {
		t.Errorf("counts = %+v, want 4 changed anomalies", res)
	}
}

// --- the determinate phase: totals arithmetic and ItemDone count ----------------------

// TestValidateProgressTotalsMatchRecordedArithmetic pins the T5.2
// determinate-phase contract against MIXED record kinds with a known
// catalog: the phase totals must be exactly the sum of the RECORDED
// sizes (the 2.3 symlink len(target) convention included, missing
// records reporting their recorded size), and there must be EXACTLY one
// ItemDone line per compared record - the byte figure per line follows
// the documented baseline (bytes actually hashed for readable content,
// the recorded size for missing/broken, the target length read for
// symlinks).
func TestValidateProgressTotalsMatchRecordedArithmetic(t *testing.T) {
	root := t.TempDir()
	vWrite(t, filepath.Join(root, "f1.txt"), "0123456789")                 // 10 B, ok
	vWrite(t, filepath.Join(root, "f2.txt"), "abcd")                       // 4 B, deleted -> missing
	vWrite(t, filepath.Join(root, "g1.bin"), "GfGfGf")                     // 6 B, corrupt flip
	if err := os.Symlink("f1.txt", filepath.Join(root, "l")); err != nil { // recorded size = len(target)
		t.Fatalf("symlink l: %v", err)
	}

	recs := []SourceRecord{
		vRec(t, root, "f1.txt", 1),
		vRec(t, root, "f2.txt", 2),
		vRec(t, root, "l", 3),
		vRec(t, root, "g1.bin", 4),
	}
	// The recorded sizes that MUST add up to the phase totals: 10 (f1)
	// + 4 (f2, its RECORDED size though the file is gone) + len(target)
	// (l, the 2.3 symlink convention) + 6 (g1). Computed from the
	// records themselves, so the arithmetic is asserted against the
	// catalog projection exactly as the catalog stores it.
	var totalRecorded int64
	for _, rec := range recs {
		totalRecorded += rec.Size
	}
	if recs[2].Size != int64(len("f1.txt")) {
		t.Fatalf("symlink recorded size = %d, want len(target) = %d (the 2.3 convention)", recs[2].Size, len("f1.txt"))
	}
	if totalRecorded != 10+4+int64(len("f1.txt"))+6 {
		t.Fatalf("fixture arithmetic: recorded total = %d, want %d", totalRecorded, 10+4+len("f1.txt")+6)
	}

	// Mutations: f2 deleted (missing), g1 flipped in place with its
	// mtime restored (corrupt). f1 and l stay clean.
	if err := os.Remove(filepath.Join(root, "f2.txt")); err != nil {
		t.Fatal(err)
	}
	g1Path := filepath.Join(root, "g1.bin")
	g1Info, err := os.Stat(g1Path)
	if err != nil {
		t.Fatal(err)
	}
	vWrite(t, g1Path, "HgHgHg")
	if err := os.Chtimes(g1Path, g1Info.ModTime(), g1Info.ModTime()); err != nil {
		t.Fatal(err)
	}

	w := &syncBuffer{}
	clk := &stepClock{}
	bus := progress.NewBus()
	wait := progress.Start(progress.New(progress.Options{Mode: progress.ModePlain, Stderr: w, Now: clk.Now}), bus)
	res, err := Validate(context.Background(), ValidateInput{
		Root: root, Records: recs, Mode: fsmeta.ModeFull,
	}, bus)
	bus.Finish(progress.Done{Summary: "validate: complete"})
	wait()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if res.OK != 2 || res.Missing != 1 || res.Corrupt != 1 || res.Anomalies() != 2 {
		t.Fatalf("verdicts = %+v (findings: %s); want ok 2, missing 1, corrupt 1", res, findingsBrief(res))
	}

	out := w.String()
	// The exact per-record stream: the byte figure on each line is the
	// documented baseline - hashed bytes for readable content, the
	// RECORDED size for the missing record, the target length for the
	// symlink - cumulative over the four records, against the ONE
	// catalog-derived total.
	cum := int64(0)
	for i, rec := range recs {
		var perRecord int64
		switch rec.RelPath {
		case "f2.txt":
			perRecord = rec.Size // missing: the recorded size
		case "l":
			perRecord = rec.Size // symlink: the target string length read
		default:
			perRecord = rec.Size // readable content that matched its recorded length
		}
		cum += perRecord
		want := fmt.Sprintf("validate: files %d/%d, %s/%s (%d%%)",
			i+1, len(recs), units.FormatBytes(cum), units.FormatBytes(totalRecorded),
			int64(100)*cum/totalRecorded)
		if !strings.Contains(out, want+"\n") {
			t.Errorf("progress stream lacks the exact line %q:\n%s", want, out)
		}
	}
	// EXACTLY one ItemDone line per compared record (no drops, no
	// double-count): four "validate: files" lines, period.
	if n := strings.Count(out, "validate: files"); n != len(recs) {
		t.Errorf("progress stream carries %d ItemDone lines, want exactly %d (one per compared record):\n%s", n, len(recs), out)
	}
}

// --- a vanished source root -----------------------------------------------------------

// TestValidateVanishedRootReportsAllMissing pins the walk's tolerance
// at its most extreme: the source root itself is GONE (unmounted,
// renamed, deleted). The run must not abort - each record lstat fails
// with ENOENT and reports `missing`, `new` discovery reports nothing
// (there is nothing to discover), and no verdict error escapes.
func TestValidateVanishedRootReportsAllMissing(t *testing.T) {
	root := t.TempDir()
	vWrite(t, filepath.Join(root, "a.txt"), "A\n")
	vWrite(t, filepath.Join(root, "b.txt"), "B\n")
	recs := []SourceRecord{vRec(t, root, "a.txt", 1), vRec(t, root, "b.txt", 2)}
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}

	res, err := Validate(context.Background(), ValidateInput{
		Root: root, Records: recs, Mode: fsmeta.ModeFull,
	}, nil)
	if err != nil {
		t.Fatalf("Validate over a vanished root must report, not abort: %v", err)
	}
	if res.Missing != 2 || res.New != 0 || res.OK != 0 || res.Anomalies() != 2 {
		t.Errorf("counts = %+v (findings: %s); want missing 2, new 0, no other class", res, findingsBrief(res))
	}
	for _, rel := range []string{"a.txt", "b.txt"} {
		if f := mustFinding(t, res, rel); f.Category != CatMissing || f.Detail != "" {
			t.Errorf("%s finding = %+v, want bare missing", rel, f)
		}
	}
}
