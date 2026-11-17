package cli

// Adversarial gap-closure pins for the 2026-10-20 change-detection
// amendment (DESIGN_SPEC 3.5, the D-10 row; the brief's remaining
// matrix holes). Complements changedetection_cli_test.go:
//
//   - RESOLVED/WARNED ONCE PER RUN, structurally: an AST pin over the
//     package's production files asserts exactly ONE resolution and ONE
//     warning call per detection-point command (scan/copy/build) and
//     exactly ONE statfs binding (fsmeta.ResolveChangeDetection called
//     only from the cli resolver). The degradation warning therefore
//     cannot become per-image or per-item, and a CIFS multi-image copy
//     emits exactly one warning even when three images copy (the
//     warning runs at run setup, before any image loop; a CIFS mount is
//     not constructible in CI, so the once-per-run property is pinned
//     structurally here and behaviourally at the seam in
//     changedetection_cli_test.go).
//   - FORCED METADATA STAYS SILENT ACROSS A MULTI-IMAGE RUN (CLI
//     tier): a three-image copy under --change-detection=metadata
//     emits NO degradation warning on stderr and writes NO warn
//     run_events row, while the audit record still says
//     mode=metadata/provenance=forced.
//   - THE RUN_EVENTS ROW CONTENT: the once-per-run warn row carries
//     source path, detected class, the degraded set, and the retained
//     FR-014 guarantee (not merely the right count).
//   - THE 12.7 CRASH PATTERN of the whole-file hardening: a SIGKILL
//     after the final chunk's verified-but-unmarked state (whether it
//     landed BEFORE the whole-file compare or between the compare's
//     MATCH and MarkChunk - both windows leave the identical
//     catalog+media state, the wf state being run-scoped) is resumed by
//     RE-COPYING and RE-VERIFYING the final chunk against the live
//     source and marking it normally: the all-first-write eligibility
//     fails on resume (the earlier chunk is marked), so the check is
//     skipped and the copy converges - exit 0, mirror flipped.
//   - THE MEDIA-ACCUMULATION EQUIVALENCE: wholeFileCheck accumulates
//     each chunk's VERIFIED MEDIA bytes after its successful copy
//     (feedFrom reads the final destPath, which the engine only ever
//     populates by atomic rename from .partial); for a successful copy
//     media bytes == source bytes (the read-back verify proved it), so
//     the accumulated sum equals the source whole-file hash - pinned
//     directly below, and end-to-end by the clean two-image e2e
//     (TestCopyEndToEndTwoImageFixture: the final chunk is DeferMark'd,
//     so it can ONLY be marked via decideWholeFile's MATCH, and
//     assertAllCopied requires both chunk marks plus the flipped
//     parent mirror).

import (
	"crypto/sha256"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"isopress/internal/catalog"
	"isopress/internal/fsmeta"
	"isopress/internal/progress"
)

// --- the once-per-run structural pin -----------------------------------------

// TestChangeDetectionResolvedAndWarnedOncePerRun parses every
// NON-TEST .go file of this package and counts the call sites:
//
//	warnChangeDetection:     exactly 1 per detection-point command
//	                         (scan.go, copy.go, build.go, validate.go)
//	                         - the warn runs at run setup, never per
//	                         image/item. T5.2 note: validate is
//	                         read-only (no runs row, so its call is
//	                         bus-only with a nil db) but still resolves
//	                         and announces the same once-per-run Mode,
//	                         because 7.3's changed-vs-corrupt gate
//	                         threads the identical DiffFields set.
//	resolveChangeDetection:  exactly 1 per detection-point command.
//	fsmeta.ResolveChangeDetection: exactly 1 in the whole package (the
//	                         cli resolver) - ONE statfs per run.
//
// A regression that resolves or warns per image (or adds a call site
// outside that four-command set) fails here even though every
// behavioural test stays green on local filesystems (where auto never
// degrades).
func TestChangeDetectionResolvedAndWarnedOncePerRun(t *testing.T) {
	wantWarn := map[string]int{"scan.go": 1, "copy.go": 1, "build.go": 1, "validate.go": 1}
	wantResolve := map[string]int{"scan.go": 1, "copy.go": 1, "build.go": 1, "validate.go": 1}
	gotWarn := map[string]int{}
	gotResolve := map[string]int{}
	statfsBindings := 0

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				switch fun.Name {
				case "warnChangeDetection":
					gotWarn[name]++
				case "resolveChangeDetection":
					gotResolve[name]++
				}
			case *ast.SelectorExpr:
				if id, ok := fun.X.(*ast.Ident); ok && id.Name == "fsmeta" && fun.Sel.Name == "ResolveChangeDetection" {
					statfsBindings++
				}
			}
			return true
		})
	}
	if len(gotWarn) != len(wantWarn) {
		t.Errorf("warnChangeDetection call sites = %v, want exactly one each in %v", gotWarn, wantWarn)
	}
	for f, n := range wantWarn {
		if gotWarn[f] != n {
			t.Errorf("warnChangeDetection calls in %s = %d, want %d (once per RUN, never per image)", f, gotWarn[f], n)
		}
	}
	if len(gotResolve) != len(wantResolve) {
		t.Errorf("resolveChangeDetection call sites = %v, want exactly one each in %v", gotResolve, wantResolve)
	}
	for f, n := range wantResolve {
		if gotResolve[f] != n {
			t.Errorf("resolveChangeDetection calls in %s = %d, want %d (ONE statfs per run)", f, gotResolve[f], n)
		}
	}
	if statfsBindings != 1 {
		t.Errorf("fsmeta.ResolveChangeDetection call sites = %d, want 1 (the cli resolver binds the production seam exactly once)", statfsBindings)
	}
}

// --- the run_events row content (seam tier) ----------------------------------

// TestChangeDetectionWarningRunEventRowContent asserts the CONTENT of
// the once-per-run warn row (changedetection_cli_test.go pins the
// count on both surfaces): source path, detected class, the degraded
// set with the explicitly-dropped fields, and the retained FR-014
// SHA-256 guarantee.
func TestChangeDetectionWarningRunEventRowContent(t *testing.T) {
	root := "/mnt/whitebox/manga"
	db, rawDB, runID := openRunCatalog(t, root)

	bus := progress.NewBus()
	warnChangeDetection(t.Context(), db, runID, bus, root,
		fsmeta.Mode{Set: fsmeta.ComparisonMetadata, Class: fsmeta.ClassSMB3})

	var detail string
	if err := rawDB.QueryRow(
		`SELECT detail FROM run_events WHERE run_id = ? AND level = 'warn' AND detail LIKE '%change detection degraded%'`,
		runID).Scan(&detail); err != nil {
		t.Fatalf("reading the degradation warn row: %v", err)
	}
	for _, needle := range []string{
		root, "SMB3", "size+mtime", "dev and inode ignored", "SHA-256", "FR-014",
	} {
		if !strings.Contains(detail, needle) {
			t.Errorf("run_events warn row lacks %q:\n%s", needle, detail)
		}
	}
}

// --- forced metadata stays silent across a MULTI-IMAGE copy run --------------

// TestCopyForcedMetadataMultiImageRunWarnsNothing runs a real
// THREE-IMAGE copy under --change-detection=metadata: the user-forced
// mode must emit no degradation warning on stderr and no warn
// run_events row for the run (the warning informs an AUTOMATIC
// degradation only), while the audit record records the forced
// provenance. On a local filesystem the auto rule never degrades, so
// this is the CLI-tier assertion of the forced-silence rule that the
// seam test covers behaviourally.
func TestCopyForcedMetadataMultiImageRunWarnsNothing(t *testing.T) {
	root := t.TempDir()
	cat := filepath.Join(t.TempDir(), "catalog.sqlite")
	dest := filepath.Join(t.TempDir(), "dest")
	const setID = "S-FORCED"

	// Three 600,000-byte files under the synthetic 3,000,000-byte
	// usable profile (the 2026-10-20 re-baseline of the ~9000/10000
	// fixture, whose profile predated the reserved-overhead floor):
	// each file's charge (600,512) exceeds half the one-image budget
	// (901,824), so each needs its own image and the copy spans THREE
	// images.
	for i, name := range []string{"a.bin", "b.bin", "c.bin"} {
		writeFileP(t, filepath.Join(root, name), pattern(byte(i+1), 600_000), 0o644)
	}
	if _, stderr, code := runScanChildForCopy(t, "--catalog", cat, "--set-id", setID, root); code != 0 {
		t.Fatalf("scan exit = %d (stderr: %s)", code, stderr)
	}
	planSynthetic(t, cat, setID, copySyntheticUsable)

	stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest,
		"--change-detection", "metadata", "--json")
	if code != 0 {
		t.Fatalf("copy exit = %d, want 0 (stdout: %s\nstderr: %s)", code, stdout, stderr)
	}
	var rep copyReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("copy --json does not parse: %v\n%s", err, stdout)
	}
	if len(rep.Images) != 3 {
		t.Fatalf("copy spanned %d images, want 3 (the multi-image premise)", len(rep.Images))
	}

	// The silence contract: no warning on stderr ...
	if strings.Contains(stderr, "change detection degraded") {
		t.Errorf("forced-metadata multi-image copy emitted a degradation warning:\n%s", stderr)
	}
	// ... and no warn run_events row for the run.
	db := catOpen(t, cat)
	if n := countWarnEvents(t, db, newestRunID(t, db, "copy"), "change detection degraded"); n != 0 {
		t.Errorf("forced-metadata copy wrote %d degradation warn rows, want 0", n)
	}
	// The audit still records the forced resolution (DB-side only).
	audit := runStatsAudit(t, cat, "copy")
	if audit["mode"] != "metadata" || audit["provenance"] != "forced" {
		t.Errorf("forced multi-image copy audit = %v, want mode=metadata provenance=forced", audit)
	}
}

// --- the 12.7 crash pattern of the whole-file hardening ----------------------

// TestCopyResumeReverifiesFinalChunkAfterCrashBeforeMark reconstructs
// the SIGKILL state the hardening's deferred-mark window can leave and
// pins the resume convergence. The crashed run copied AND read-back
// verified the final chunk (the engine's atomic .partial->final rename
// completed) but died before the whole-file decision ran - or between
// the MATCH and MarkChunk; BOTH windows leave the identical
// catalog+media state (the wf state is run-scoped), so one
// reconstruction covers them:
//
//	chunk0: copied=1, hash stored (marked in its own transaction)
//	chunk1: media bytes present, copied=0, hash NULL (verified-but-unmarked)
//	files.copied: 0 (the mirror is a count proof, still incomplete)
//
// The resume run must NOT trust the leftover bytes: the final chunk is
// re-copied (first-write again - its hash is still NULL), re-verified
// against the LIVE source, and marked through the NORMAL path; the
// whole-file check is skipped (the earlier chunk is marked, so the
// all-first-write eligibility fails), the mirror flips, and the run
// exits 0. The outcome converges on both crash windows.
func TestCopyResumeReverifiesFinalChunkAfterCrashBeforeMark(t *testing.T) {
	root := t.TempDir()
	cat := filepath.Join(t.TempDir(), "catalog.sqlite")
	dest := filepath.Join(t.TempDir(), "dest")
	const setID = "S-CRASH"

	// chunks 901,312 + 450,688 across two images (the 2026-10-20
	// re-baseline of 15000 = chunk 10000 + 5000).
	big := pattern(9, copyBigChunkedLen)
	writeFileP(t, filepath.Join(root, "big.bin"), big, 0o644)
	if _, stderr, code := runScanChildForCopy(t, "--catalog", cat, "--set-id", setID, root); code != 0 {
		t.Fatalf("scan exit = %d (stderr: %s)", code, stderr)
	}
	planSynthetic(t, cat, setID, copySyntheticUsable) // chunk0 fills image 1; chunk1 -> image 2

	// The crashed run's first leg: only image 1 completed, so chunk0 is
	// marked and chunk1 is untouched.
	if stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest,
		"--image", "1", "--json"); code != 0 {
		t.Fatalf("image-1 copy exit = %d (stdout: %s\nstderr: %s)", code, stdout, stderr)
	}
	db := catOpen(t, cat)
	var copied0, copied1 int
	if err := db.QueryRow(`SELECT copied FROM chunks WHERE set_id = ? AND chunk_index = 0`, setID).
		Scan(&copied0); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT copied FROM chunks WHERE set_id = ? AND chunk_index = 1`, setID).
		Scan(&copied1); err != nil {
		t.Fatal(err)
	}
	if copied0 != 1 || copied1 != 0 {
		t.Fatalf("pre-crash chunk flags = (%d, %d), want (1, 0)", copied0, copied1)
	}

	// Reconstruct the crash window: the final chunk's verified bytes
	// are on media, unmarked. (The crashed run wrote them via
	// .partial+rename and passed read-back verify; writing the same
	// source bytes here is state-equivalent, and the resume re-copies
	// over them anyway - the test asserts re-copy, never trust.)
	chunk1Path := filepath.Join(destImageDir(dest, setID, 2), "chunks", "big.bin", "00002")
	writeFileP(t, chunk1Path, big[copyChunk0Len:], 0o644)

	// The resume: full copy. The final chunk re-copies (copied=1 in the
	// report - NOT already_copied), re-verifies against the live
	// source, marks, and flips the mirror; exit 0.
	stdout, stderr, code := runCopyChild(t, nil,
		"copy", "--catalog", cat, "--set-id", setID, "--dest", dest, "--json")
	if code != 0 {
		t.Fatalf("resumed copy exit = %d, want 0 (stdout: %s\nstderr: %s)", code, stdout, stderr)
	}
	var rep copyReport
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("copy --json does not parse: %v\n%s", err, stdout)
	}
	if rep.Status != "ok" || rep.Failed != 0 || rep.Skipped != 0 {
		t.Errorf("resumed report = %+v, want a clean ok run", rep)
	}
	if rep.Copied != 1 || rep.AlreadyCopied != 1 {
		t.Errorf("resumed report copied/already_copied = %d/%d, want 1/1 "+
			"(the final chunk RE-COPIES, the marked chunk is trusted)", rep.Copied, rep.AlreadyCopied)
	}

	// The converged catalog truth: final chunk marked with its verified
	// anchor, parent mirror flipped.
	if err := db.QueryRow(`SELECT copied FROM chunks WHERE set_id = ? AND chunk_index = 1`, setID).
		Scan(&copied1); err != nil {
		t.Fatal(err)
	}
	if copied1 != 1 {
		t.Errorf("resumed run: final chunk copied = %d, want 1 (re-verified and re-marked)", copied1)
	}
	var hash []byte
	if err := db.QueryRow(`SELECT hash FROM chunks WHERE set_id = ? AND chunk_index = 1`, setID).
		Scan(&hash); err != nil {
		t.Fatal(err)
	}
	wantHash := sha256.Sum256(big[copyChunk0Len:])
	if len(hash) != 32 || hash[0] != wantHash[0] || string(hash) != string(wantHash[:]) {
		t.Errorf("final chunk stored hash = %x, want the re-verified capture %x", hash, wantHash[:])
	}
	var parentCopied int
	if err := db.QueryRow(`SELECT copied FROM files WHERE set_id = ? AND relpath = 'big.bin'`, setID).
		Scan(&parentCopied); err != nil {
		t.Fatal(err)
	}
	if parentCopied != 1 {
		t.Errorf("resumed run: files.copied = %d, want 1 (the mirror is a count proof)", parentCopied)
	}
	if _, err := os.Stat(filepath.Join(destImageDir(dest, setID, 2), "MANIFEST.json")); err != nil {
		t.Errorf("resumed run: image 2 manifest missing after completion: %v", err)
	}
	assertBytes(t, chunk1Path, big[copyChunk0Len:])
}

// --- the media-accumulation equivalence (unit) -------------------------------

// TestWholeFileCheckFeedFromAccumulatesVerifiedMediaBytes pins the
// whole-file hardening's accumulation arithmetic directly: feeding the
// per-chunk MEDIA files in chunk order yields exactly the whole-file
// sha256 over the SOURCE bytes, because a successful copy's read-back
// verify proved media bytes == source bytes per chunk. This is the
// equivalence decideWholeFile's compare against the catalog
// ParentHash relies on. The TIMING half of the contract (the feed reads
// the FINAL media file AFTER verify, never a mid-write partial) is
// structural: feedFrom is called only when the CopyItem outcome is
// Copied - i.e. after the atomic .partial->final rename AND the
// read-back verify - and destPath is only ever populated by that
// rename; a partial file can never exist at the fed path.
//
// THE IN-ORDER FEED GATE (the B1 ordering fix) is pinned alongside:
// the same feeds must arrive in CHUNK-INDEX order, and an
// out-of-order feed marks the check broken WITHOUT folding any bytes -
// a permuted stream is never hashed (it could never match, so hashing
// it could only produce a false "source changed" on unchanged
// content).
func TestWholeFileCheckFeedFromAccumulatesVerifiedMediaBytes(t *testing.T) {
	big := pattern(21, 15000)
	dir := t.TempDir()

	r := &copyRunner{}
	wf := r.newWholeFileCheck(catalog.CopyChunk{
		ParentRelPath: "big.bin", ParentNumChunks: 2, ParentHash: make([]byte, 32),
	})
	if wf == nil {
		t.Fatal("newWholeFileCheck refused a well-formed parent")
	}

	// The two verified media files (the state after two successful
	// DeferMark copies, fed in chunk-INDEX order - the gate's contract).
	p0 := filepath.Join(dir, "00001")
	p1 := filepath.Join(dir, "00002")
	writeFileP(t, p0, big[:10000], 0o644)
	writeFileP(t, p1, big[10000:], 0o644)
	if err := wf.feedFrom(p0, 0); err != nil {
		t.Fatalf("feed chunk 0: %v", err)
	}
	if err := wf.feedFrom(p1, 1); err != nil {
		t.Fatalf("feed chunk 1: %v", err)
	}
	if wf.broken {
		t.Fatal("feeding two readable media files in chunk order marked the check broken")
	}
	if wf.nextExpected != 2 {
		t.Fatalf("nextExpected after two in-order feeds = %d, want 2", wf.nextExpected)
	}
	want := sha256.Sum256(big)
	if got := wf.hasher.Sum(nil); string(got) != string(want[:]) {
		t.Errorf("accumulated media hash = %x, want the source whole-file hash %x "+
			"(media bytes == source bytes after read-back verify)", got, want[:])
	}

	// THE GATE: an out-of-order feed (chunk 1 arriving before chunk 0 -
	// the cross-image interleaved-assignment shape) marks the check
	// broken WITHOUT folding bytes: the hasher state stays empty and
	// the cursor does not advance, so the permuted stream is never
	// hashed and the compare can never fire on it.
	wf3 := r.newWholeFileCheck(catalog.CopyChunk{
		ParentRelPath: "big.bin", ParentNumChunks: 2, ParentHash: make([]byte, 32),
	})
	if err := wf3.feedFrom(p1, 1); err != nil {
		t.Fatalf("out-of-order feed of chunk 1: %v", err)
	}
	if !wf3.broken {
		t.Error("an out-of-order feed must break the check (fail closed to no-compare)")
	}
	if wf3.nextExpected != 0 {
		t.Errorf("nextExpected after an out-of-order feed = %d, want 0 (nothing was consumed)", wf3.nextExpected)
	}
	empty := sha256.Sum256(nil)
	if got := wf3.hasher.Sum(nil); string(got) != string(empty[:]) {
		t.Errorf("an out-of-order feed folded bytes: hasher = %x, want the untouched empty state %x", got, empty[:])
	}
	if wf3.eligibleAtFinalChunk() {
		t.Error("a broken check must never be eligible at the final chunk")
	}

	// An unreadable media file fails CLOSED (broken -> no compare, the
	// pre-amendment behavior), never to a false mismatch.
	wf2 := r.newWholeFileCheck(catalog.CopyChunk{
		ParentRelPath: "big.bin", ParentNumChunks: 2, ParentHash: make([]byte, 32),
	})
	if err := wf2.feedFrom(filepath.Join(dir, "missing"), 0); err == nil {
		t.Fatal("feedFrom of a missing file returned nil")
	}
	if !wf2.broken || wf2.eligibleAtFinalChunk() {
		t.Error("a failed feed must break the check (fail closed to no-compare)")
	}
}
