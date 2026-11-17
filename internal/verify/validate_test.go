package verify

// T5.2: the validate SERVICE tier (DESIGN_SPEC 7.3, FR-022;
// IMPLEMENTATION_PLAN 4.6 T5.2 acceptance). The matrix below drives
// Validate over REAL temp fixtures - real files, real symlinks, real
// chmod/mkfifo/rename mutations - against hand-built SourceRecords that
// mirror what the catalog stores (vRec captures the exact projection
// catalog.ActiveRecords reads: metadata columns, hash or link_target).
//
// The centerpiece assertions are the 7.3 readings the task pins:
//
//   - metadata drift with MATCHING content is ok (7.3 "ok: hash
//     matches") - asserted even though the resolved comparison set
//     provably sees the drift, and even for dev/inode drift under the
//     full set;
//   - same-length byte flips with the original mtime restored classify
//     CORRUPT, not changed (the bit-rot case the command exists for);
//   - the resolved Mode threads the changed/corrupt split (full set:
//     inode drift => changed; metadata set: inode ignored => corrupt);
//   - symlinks compare by target: retarget => changed, dangling with
//     an intact recorded target => ok;
//   - `new` covers source files/symlinks only (dirs, fifos, and implied
//     parents never count) and never becomes an anomaly (Result
//     .Anomalies excludes it);
//   - chunked files ride the whole-file anchor: their record is an
//     ordinary regular record (files.hash), the chunk rows are never
//     consulted - the CLI tier pins that against a real planned+chunked
//     catalog (internal/cli/validate_test.go), because at this tier the
//     absence of any chunk input to Validate is structural.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"isopress/internal/fsmeta"
	"isopress/internal/progress"
	"isopress/internal/units"
)

// --- fixture helpers -----------------------------------------------------------------

// vWrite writes content at path, creating parent directories (fixture
// builder; 0644 - the tests never execute fixture content).
func vWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// vRec captures the CATALOG projection for an existing entry at
// root/rel: the four DiffFields metadata columns plus the content
// anchor (hash for regular, target for symlink) - exactly the shape
// catalog.ActiveRecords returns, which is what Validate consumes.
func vRec(t *testing.T, root, rel string, id int64) SourceRecord {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	fi, err := os.Lstat(abs)
	if err != nil {
		t.Fatalf("lstat %s: %v", abs, err)
	}
	meta, err := fsmeta.Capture(fi)
	if err != nil {
		t.Fatalf("capture %s: %v", abs, err)
	}
	rec := SourceRecord{
		FileID:  id,
		RelPath: rel,
		Size:    meta.Size,
		MtimeNS: meta.MtimeNS,
		Dev:     int64(meta.Dev),
		Inode:   int64(meta.Inode),
	}
	switch fsmeta.Classify(fi) {
	case fsmeta.KindSymlink:
		rec.EntryKind = "symlink"
		target, err := os.Readlink(abs)
		if err != nil {
			t.Fatalf("readlink %s: %v", abs, err)
		}
		rec.LinkTarget = target
	case fsmeta.KindRegular:
		rec.EntryKind = "regular"
		b, err := os.ReadFile(abs)
		if err != nil {
			t.Fatalf("read %s: %v", abs, err)
		}
		sum := sha256.Sum256(b)
		rec.Hash = sum[:]
	default:
		t.Fatalf("%s is neither regular nor symlink", abs)
	}
	return rec
}

// mustFinding returns the finding for path or fails the test.
func mustFinding(t *testing.T, res Result, path string) Finding {
	t.Helper()
	for _, f := range res.Findings {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("no finding for %q (findings: %s)", path, findingsBrief(res))
	return Finding{}
}

// findingsBrief renders findings as "category:path; ..." for failures.
func findingsBrief(res Result) string {
	parts := make([]string, 0, len(res.Findings))
	for _, f := range res.Findings {
		parts = append(parts, f.Category+":"+f.Path)
	}
	return strings.Join(parts, "; ")
}

// --- the category matrix (the T5.2 centerpiece) -------------------------------

// TestValidateCategoryMatrix builds one fixture set in its pristine
// state, captures every record, then mutates the source into all six
// 7.3 classes at once and asserts the categorisation - counts,
// finding order (records in record order, then new in walk order), and
// the exact pinned detail strings.
func TestValidateCategoryMatrix(t *testing.T) {
	root := t.TempDir()

	// Pristine fixture: 13 records - 10 regular files plus 3 symlinks.
	vWrite(t, filepath.Join(root, "ok.txt"), "ok content\n")
	vWrite(t, filepath.Join(root, "drift.txt"), "drift content stays\n")
	vWrite(t, filepath.Join(root, "inode-drift.txt"), "identical bytes\n")
	vWrite(t, filepath.Join(root, "changed.txt"), "original bytes\n")
	vWrite(t, filepath.Join(root, "rot.bin"), "0123456789abcdef")
	vWrite(t, filepath.Join(root, "gone.txt"), "gone content\n")
	vWrite(t, filepath.Join(root, "locked.txt"), "locked content\n")
	vWrite(t, filepath.Join(root, "dangle-target.txt"), "target content\n")
	vWrite(t, filepath.Join(root, "kindswap.txt"), "soon a directory\n")
	vWrite(t, filepath.Join(root, "nested", "inner.txt"), "nested content\n")
	for _, link := range []struct{ name, target string }{
		{"link-ok", "ok.txt"},
		{"link-retarget", "ok.txt"},
		{"link-dangle", "dangle-target.txt"},
	} {
		if err := os.Symlink(link.target, filepath.Join(root, link.name)); err != nil {
			t.Fatalf("symlink %s: %v", link.name, err)
		}
	}

	recs := []SourceRecord{}
	for i, rel := range []string{
		"ok.txt", "drift.txt", "inode-drift.txt", "changed.txt", "rot.bin",
		"gone.txt", "locked.txt", "dangle-target.txt", "kindswap.txt",
		"nested/inner.txt", "link-ok", "link-retarget", "link-dangle",
	} {
		recs = append(recs, vRec(t, root, rel, int64(i+1)))
	}

	// --- mutations: every class at once ---
	// ok (metadata drift, matching bytes): drift.txt's mtime moves an
	// hour out; inode-drift.txt's bytes are replaced wholesale (new
	// inode, new mtime) - BOTH stay ok because 7.3 anchors on content.
	if err := os.Chtimes(filepath.Join(root, "drift.txt"), time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	replaceIdentical(t, filepath.Join(root, "inode-drift.txt"), "identical bytes\n")

	// changed: different bytes AND a fresh mtime (metadata + content).
	// The explicit Chtimes makes the mtime_ns half UNAMBIGUOUS: the
	// pinned finding detail below is "metadata: size mtime_ns; hash
	// differs", and a rewrite alone only moves mtime when the
	// filesystem's granularity lets it - CI filesystems may round
	// mtimes to 1s, so the mtime_ns leg is guaranteed by an absolute
	// +10s stamp (>= 2s of margin) rather than by the write itself.
	changedPath := filepath.Join(root, "changed.txt")
	vWrite(t, changedPath, "completely different and longer bytes\n")
	moved := time.Now().Add(10 * time.Second)
	if err := os.Chtimes(changedPath, moved, moved); err != nil {
		t.Fatal(err)
	}

	// CORRUPT (bit rot): same length, different bytes, ORIGINAL mtime
	// restored - metadata matches the catalog while the hash differs.
	rotPath := filepath.Join(root, "rot.bin")
	rotInfo, err := os.Stat(rotPath)
	if err != nil {
		t.Fatal(err)
	}
	vWrite(t, rotPath, "fedcba9876543210") // same 16 bytes length
	if err := os.Chtimes(rotPath, rotInfo.ModTime(), rotInfo.ModTime()); err != nil {
		t.Fatal(err)
	}

	// missing: gone.txt deleted; dangle-target.txt deleted (its record
	// goes missing while link-dangle only becomes Dangling - still ok);
	// nested/inner.txt's path is severed by replacing nested/ with a
	// FILE (ENOTDIR: the entry as recorded cannot exist).
	if err := os.Remove(filepath.Join(root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "dangle-target.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(root, "nested")); err != nil {
		t.Fatal(err)
	}
	vWrite(t, filepath.Join(root, "nested"), "nested is now a file\n")

	// broken: chmod 000 (root bypasses permissions, so the class is
	// only exercised off-root - the locked counts below adjust).
	asRoot := os.Geteuid() == 0
	if !asRoot {
		if err := os.Chmod(filepath.Join(root, "locked.txt"), 0o000); err != nil {
			t.Fatal(err)
		}
	}

	// kind change: kindswap.txt becomes a DIRECTORY.
	if err := os.Remove(filepath.Join(root, "kindswap.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "kindswap.txt"), 0o755); err != nil {
		t.Fatal(err)
	}

	// symlink retarget: link-ok untouched; link-dangle dangled by the
	// deletion above; link-retarget repointed.
	if err := os.Remove(filepath.Join(root, "link-retarget")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("changed.txt", filepath.Join(root, "link-retarget")); err != nil {
		t.Fatal(err)
	}

	// new: two source files no record covers (plus a fifo and an empty
	// dir, which must NEVER classify as new - see the file doc).
	vWrite(t, filepath.Join(root, "new.txt"), "brand new\n")
	vWrite(t, filepath.Join(root, "sub", "new2.txt"), "new in a subdir\n")
	if err := syscall.Mkfifo(filepath.Join(root, "special.fifo"), 0o644); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}
	if err := os.Mkdir(filepath.Join(root, "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := Validate(context.Background(), ValidateInput{
		Root: root, Records: recs, Mode: fsmeta.ModeFull,
	}, nil)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}

	// Counts (the six classes; locked.txt folds into ok as root).
	// The new half is THREE entries: new.txt, sub/new2.txt, and
	// "nested" - the file that took over the former nested/ DIRECTORY
	// path (correctly new: no record covers that path as a file, and
	// rescan would record it; its inner record shows as missing below).
	wantOK, wantBroken := 5, 1
	if asRoot {
		wantOK, wantBroken = 6, 0
	}
	wantChanged, wantCorrupt, wantMissing, wantNew := 3, 1, 3, 3
	if res.Compared != len(recs) {
		t.Errorf("Compared = %d, want %d", res.Compared, len(recs))
	}
	if res.OK != wantOK || res.Changed != wantChanged || res.Corrupt != wantCorrupt ||
		res.Broken != wantBroken || res.Missing != wantMissing || res.New != wantNew {
		t.Errorf("counts = ok %d, changed %d, corrupt %d, broken %d, missing %d, new %d; want ok %d, changed %d, corrupt %d, broken %d, missing %d, new %d",
			res.OK, res.Changed, res.Corrupt, res.Broken, res.Missing, res.New,
			wantOK, wantChanged, wantCorrupt, wantBroken, wantMissing, wantNew)
	}
	// `new` is INFORMATIONAL: it never joins the anomaly total (7.3).
	wantAnomalies := wantChanged + wantCorrupt + wantBroken + wantMissing
	if got := res.Anomalies(); got != wantAnomalies {
		t.Errorf("Anomalies() = %d, want %d (changed+corrupt+broken+missing; new excluded)", got, wantAnomalies)
	}

	// --- per-finding details (the pinned line formats) ---
	if f := mustFinding(t, res, "changed.txt"); f.Category != CatChanged ||
		f.Detail != "metadata: size mtime_ns; hash differs" {
		t.Errorf("changed.txt finding = %+v, want changed with detail %q",
			f, "metadata: size mtime_ns; hash differs")
	}
	if f := mustFinding(t, res, "rot.bin"); f.Category != CatCorrupt ||
		f.Detail != "bit rot: metadata matches, hash differs" {
		t.Errorf("rot.bin finding = %+v, want corrupt with the bit-rot detail", f)
	}
	if f := mustFinding(t, res, "gone.txt"); f.Category != CatMissing || f.Detail != "" {
		t.Errorf("gone.txt finding = %+v, want missing with no detail", f)
	}
	if f := mustFinding(t, res, "dangle-target.txt"); f.Category != CatMissing {
		t.Errorf("dangle-target.txt finding = %+v, want missing", f)
	}
	if f := mustFinding(t, res, "nested/inner.txt"); f.Category != CatMissing {
		t.Errorf("nested/inner.txt finding = %+v, want missing (the ENOTDIR half of the missing rule)", f)
	}
	if f := mustFinding(t, res, "link-retarget"); f.Category != CatChanged ||
		!strings.HasPrefix(f.Detail, "symlink target differs: recorded \"ok.txt\", now \"changed.txt\"") {
		t.Errorf("link-retarget finding = %+v, want changed with the target-transition detail", f)
	}
	if f := mustFinding(t, res, "kindswap.txt"); f.Category != CatChanged ||
		f.Detail != "entry kind changed: regular -> directory (content not comparable across kinds)" {
		t.Errorf("kindswap.txt finding = %+v, want the kind-transition changed detail", f)
	}
	if f := mustFinding(t, res, "new.txt"); f.Category != CatNew ||
		f.Detail != "informational; a rescan would capture it" {
		t.Errorf("new.txt finding = %+v, want informational new", f)
	}
	if f := mustFinding(t, res, "sub/new2.txt"); f.Category != CatNew {
		t.Errorf("sub/new2.txt finding = %+v, want new (walk descends into real directories)", f)
	}
	if f := mustFinding(t, res, "nested"); f.Category != CatNew {
		t.Errorf("nested finding = %+v, want new (a file that took over a directory path has no record of its own)", f)
	}
	if !asRoot {
		if f := mustFinding(t, res, "locked.txt"); f.Category != CatBroken ||
			!strings.HasPrefix(f.Detail, "unreadable: permission denied") {
			t.Errorf("locked.txt finding = %+v, want broken with the T1.5 unreadable detail", f)
		}
	}

	// The 7.3 ok readings: untouched, mtime-drifted, and
	// inode+replaced content with matching bytes are ALL ok, and the
	// dangling symlink with its recorded target intact is ok - none of
	// them may appear as findings.
	for _, path := range []string{"ok.txt", "drift.txt", "inode-drift.txt", "link-ok", "link-dangle"} {
		for _, f := range res.Findings {
			if f.Path == path {
				t.Errorf("%s must be ok (7.3: hash/target matches) but was categorised %q (%s)", path, f.Category, f.Detail)
			}
		}
	}

	// Never-new: the fifo, the empty dir, and every recorded path.
	for _, path := range []string{"special.fifo", "empty-dir", "kindswap.txt", "ok.txt"} {
		for _, f := range res.Findings {
			if f.Path == path && f.Category == CatNew {
				t.Errorf("%q must never classify `new` but did", path)
			}
		}
	}

	// Finding ORDER: record findings first (record order), then the
	// walk-order new findings at the tail (nested, new.txt,
	// sub/new2.txt - lexical WalkDir order).
	nRecord := len(res.Findings) - res.New
	for _, f := range res.Findings[:nRecord] {
		if f.Category == CatNew {
			t.Errorf("finding %q sits before the new-entry tail; record findings must lead (%s)", f.Path, findingsBrief(res))
		}
	}
	if res.New >= 3 {
		n := len(res.Findings)
		tail := [3]string{res.Findings[n-3].Path, res.Findings[n-2].Path, res.Findings[n-1].Path}
		if tail != [3]string{"nested", "new.txt", "sub/new2.txt"} {
			t.Errorf("walk-order tail = %v, want [nested new.txt sub/new2.txt] (lexical WalkDir order)", tail)
		}
	}
}

// TestValidateMetadataDriftKeepsOK pins 7.3's ok definition head-on:
// the same bytes with a moved mtime stays `ok` EVEN THOUGH the
// resolved comparison set provably sees the drift (the DiffFields
// assertion proves the fixture drifted, so the ok can only come from
// the hash-wins rule).
func TestValidateMetadataDriftKeepsOK(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "steady.txt")
	vWrite(t, path, "unchanged content\n")
	rec := vRec(t, root, "steady.txt", 1)

	if err := os.Chtimes(path, time.Now().Add(2*time.Hour), time.Now().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	cur, err := fsmeta.PathMeta(path)
	if err != nil {
		t.Fatal(err)
	}
	if d := fsmeta.DiffFields(recordMeta(rec), cur, fsmeta.ModeFull); !d.Any() {
		t.Fatalf("fixture did not drift under the full set (diff = %s)", d)
	}

	res, err := Validate(context.Background(), ValidateInput{
		Root: root, Records: []SourceRecord{rec}, Mode: fsmeta.ModeFull,
	}, nil)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if res.OK != 1 || res.Anomalies() != 0 || len(res.Findings) != 0 {
		t.Errorf("metadata-drift run = %+v (findings: %s); want ok=1, zero anomalies, no findings (7.3: hash matches = ok)",
			res, findingsBrief(res))
	}
}

// TestValidateModeThreadedCorruptVsChanged pins the CIFS-fix Mode
// threading through the changed/corrupt gate: a same-size file
// replaced under a NEW inode with the original mtime restored is
//   - changed under the FULL set (inode drift + hash differ), and
//   - corrupt under the METADATA set (size+mtime match; the synthesized
//     or swapped inode is ignored; hash differs).
//
// Both classifications come from the one fsmeta.DiffFields(mode) -
// validate never hand-rolls a variant.
func TestValidateModeThreadedCorruptVsChanged(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "swap.bin")
	vWrite(t, path, "AAAAAAAA")
	rec := vRec(t, root, "swap.bin", 1)

	orig, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(root, "swap.bin.tmp-replace")
	vWrite(t, tmp, "BBBBBBBB") // same length, different bytes, NEW inode
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, orig.ModTime(), orig.ModTime()); err != nil {
		t.Fatal(err)
	}

	full, err := Validate(context.Background(), ValidateInput{
		Root: root, Records: []SourceRecord{rec}, Mode: fsmeta.ModeFull,
	}, nil)
	if err != nil {
		t.Fatalf("Validate (full): %v", err)
	}
	if full.Changed != 1 || full.Corrupt != 0 {
		t.Errorf("full mode: %+v (findings: %s); want changed=1 (inode drift + hash differ)",
			full, findingsBrief(full))
	} else if f := mustFinding(t, full, "swap.bin"); !strings.Contains(f.Detail, "inode") {
		t.Errorf("full-mode detail = %q, want the drifted inode named", f.Detail)
	}

	meta, err := Validate(context.Background(), ValidateInput{
		Root:    root,
		Records: []SourceRecord{rec},
		Mode:    fsmeta.Mode{Set: fsmeta.ComparisonMetadata, Forced: true},
	}, nil)
	if err != nil {
		t.Fatalf("Validate (metadata): %v", err)
	}
	if meta.Corrupt != 1 || meta.Changed != 0 {
		t.Errorf("metadata mode: %+v (findings: %s); want corrupt=1 (size+mtime match, inode ignored, hash differs)",
			meta, findingsBrief(meta))
	} else if f := mustFinding(t, meta, "swap.bin"); f.Detail != "bit rot: metadata matches, hash differs" {
		t.Errorf("metadata-mode detail = %q, want the bit-rot detail", f.Detail)
	}
}

// TestValidateProgressDeterminatePhaseAndPerRecordItemDone pins the
// T5.2 progress contract through the real plain renderer: ONE
// determinate "validate" phase carrying the catalog's record/byte
// totals, one ItemDone per compared RECORD (all three lines appear
// under the stepping clock - nothing is swallowed or double-counted),
// and the exact first-line format the 9.3 family style implies.
func TestValidateProgressDeterminatePhaseAndPerRecordItemDone(t *testing.T) {
	root := t.TempDir()
	content := "0123456789" // 10 bytes each
	rels := []string{"a.txt", "b.txt", "c.txt"}
	recs := make([]SourceRecord, 0, len(rels))
	for i, rel := range rels {
		vWrite(t, filepath.Join(root, rel), content)
		recs = append(recs, vRec(t, root, rel, int64(i+1)))
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
	if res.OK != 3 {
		t.Fatalf("ok = %d, want 3 (the fixture is clean)", res.OK)
	}

	out := w.String()
	// The determinate totals: 3 records, 30 B - rendered on the FIRST
	// ItemDone line (the plain renderer's first-event rule), with the
	// byte-ratio percent truncated per the 9.3 convention.
	wantFirst := fmt.Sprintf("validate: files 1/3, %s/%s (33%%)",
		units.FormatBytes(10), units.FormatBytes(30))
	if !strings.Contains(out, wantFirst) {
		t.Errorf("progress stream lacks the exact first line %q:\n%s", wantFirst, out)
	}
	// One ItemDone per compared record: lines 1/3, 2/3 and 3/3 all
	// landed (the stepping clock gives every event its own window).
	for i := 1; i <= 3; i++ {
		if want := fmt.Sprintf("validate: files %d/3, ", i); !strings.Contains(out, want) {
			t.Errorf("progress stream lacks %q (per-record ItemDone):\n%s", want, out)
		}
	}
	if !strings.Contains(out, "validate: complete") {
		t.Errorf("progress stream lacks the Done summary:\n%s", out)
	}
}

// --- abort and integrity paths ------------------------------------------------

// TestValidateRefusals pins the service's error contract: an empty root
// is a wiring fault, an already-cancelled context aborts before any
// comparison (and yields no partial verdict as an error), and an
// active regular record with NO stored hash aborts as a catalog fault
// rather than misreporting it as a source verdict.
func TestValidateRefusals(t *testing.T) {
	root := t.TempDir()
	vWrite(t, filepath.Join(root, "a.txt"), "content\n")
	rec := vRec(t, root, "a.txt", 1)

	if _, err := Validate(context.Background(), ValidateInput{Records: []SourceRecord{rec}}, nil); err == nil {
		t.Error("Validate with an empty root must fail (the cli always passes sets.source_root)")
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Validate(cancelled, ValidateInput{Root: root, Records: []SourceRecord{rec}}, nil); err == nil {
		t.Error("Validate on a cancelled context must abort, not compare")
	}

	noHash := rec
	noHash.Hash = nil
	_, err := Validate(context.Background(), ValidateInput{
		Root: root, Records: []SourceRecord{noHash}, Mode: fsmeta.ModeFull,
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "catalog integrity") {
		t.Errorf("nil-hash active regular record: err = %v, want the catalog-integrity abort", err)
	}
}

// TestValidateUnreadableWalkDirWarnsAndPrunes pins the T1.3 walk
// convention on the `new`-discovery half: a directory the run cannot
// read produces a progress WARNING and is pruned, the comparison of
// the catalog records still completes, and no error escapes (only a
// cancelled context aborts). Skipped as root, which bypasses the
// permission model entirely.
func TestValidateUnreadableWalkDirWarnsAndPrunes(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 000 is bypassed, unreadable-directory fixture unavailable")
	}
	root := t.TempDir()
	vWrite(t, filepath.Join(root, "known.txt"), "known\n")
	rec := vRec(t, root, "known.txt", 1)
	hidden := filepath.Join(root, "hidden")
	vWrite(t, filepath.Join(hidden, "extra.txt"), "undiscoverable\n")
	if err := os.Chmod(hidden, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(hidden, 0o755) }) // TempDir cleanup needs the dir back

	w := &syncBuffer{}
	clk := &stepClock{}
	bus := progress.NewBus()
	wait := progress.Start(progress.New(progress.Options{Mode: progress.ModePlain, Stderr: w, Now: clk.Now}), bus)
	res, err := Validate(context.Background(), ValidateInput{
		Root: root, Records: []SourceRecord{rec}, Mode: fsmeta.ModeFull,
	}, bus)
	bus.Finish(progress.Done{Summary: "validate: complete"})
	wait()
	if err != nil {
		t.Fatalf("Validate must survive an unreadable directory (warn + prune), got: %v", err)
	}
	if res.OK != 1 {
		t.Errorf("ok = %d, want 1 (the record comparison is unaffected by the walk)", res.OK)
	}
	if res.New != 0 {
		t.Errorf("new = %d, want 0 (extra.txt is undiscoverable; new under-reports there, documented)", res.New)
	}
	if out := w.String(); !strings.Contains(out, "warning: validate: walking "+hidden) {
		t.Errorf("stream lacks the walk warning naming %s:\n%s", hidden, out)
	}
}

// --- helpers -----------------------------------------------------------------------------

// replaceIdentical rewrites path with the SAME content through a
// temp-file rename - a fresh inode under unchanged bytes (the
// whole-file-replace case 7.3's hash-wins rule must absorb).
func replaceIdentical(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".tmp-replace"
	vWrite(t, tmp, content)
	if err := os.Rename(tmp, path); err != nil {
		t.Fatalf("rename-replace %s: %v", path, err)
	}
}
