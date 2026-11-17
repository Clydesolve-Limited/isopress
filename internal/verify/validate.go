package verify

// T5.2: the validate SERVICE (DESIGN_SPEC 7.3, FR-022;
// IMPLEMENTATION_PLAN 4.6 T5.2). Validate compares the SOURCE tree of
// a set against the CATALOG - no media is touched - and categorises
// every catalog-ACTIVE record, plus every source entry no active record
// covers, into the six 7.3 classes:
//
//   - ok:      the content anchor matches (regular: sha256 of the
//     source stream equals files.hash; symlink: current target equals
//     files.link_target). 7.3's rule is exactly "ok: hash matches",
//     so metadata drift NEVER reclassifies a matching content - a file
//     whose bytes are intact but whose mtime/uid/dev/inode moved is
//     `ok`, not `changed` (test-pinned: TestValidateMetadataDriftKeepsOK
//     and the inode-drift case in TestValidateCategoryMatrix).
//   - changed: the hash differs AND the metadata differs - a plausible
//     modification the rescan workflow (FR-028) will capture. The
//     metadata half is the RESOLVED comparison set of DESIGN_SPEC 3.5
//     through the ONE comparison, fsmeta.DiffFields(mode): the CIFS
//     fix's Mode threads straight through, so a CIFS-class (or
//     --change-detection=metadata) run ignores dev/inode drift and a
//     same-size + same-mtime replacement with a synthesized inode
//     classifies `corrupt` instead of falsely `changed`.
//   - corrupt: the hash differs while the metadata MATCHES - readable
//     content that no longer matches what was backed up: bit rot, or
//     an inconsistent source that rewrote bytes without moving the
//     recorded metadata (the case this whole command exists for; the
//     T5.2 bit-rot test flips same-length bytes and restores the
//     original mtime).
//   - broken:  stat succeeds but the content cannot be read - the T1.5
//     unreadable mirror (open or mid-stream read failure, errno
//     detail), plus the two documented judgment halves below (lstat /
//     readlink failures, which cannot be `missing` because absence is
//     unproven and cannot be `changed` because no comparison ran).
//   - missing: a catalog-active record whose source entry is gone
//     (ENOENT, or ENOTDIR - an intermediate path component became a
//     non-directory, so the entry as recorded cannot exist).
//   - new:     a source entry (regular file or symlink) with no
//     catalog-ACTIVE record - INFORMATIONAL per 7.3's parenthetical
//     ("informational; rescan would capture it"). new is deliberately
//     NOT an anomaly for the exit-code decision: 7.3 reserves exit 10
//     for "any anomaly", and the explicit informational annotation is
//     what separates `new` from changed/corrupt/broken/missing. The
//     cli's Result.Anomalies() implements exactly that reading.
//
// # The judgment calls, each pinned by a test
//
//   - Symlinks compare by TARGET, never by hash (a symlink stores no
//     bytes: link_target IS its content). Equal target -> `ok`, even
//     when the target path is dangling - the recorded content is
//     unchanged and restoring the link verbatim is still possible (the
//     osirrox/restore dangling contract, 8.4). Different target ->
//     `changed` UNCONDITIONALLY: a retarget is a modification of the
//     link's content, so the two-gate metadata rule does not apply
//     (there are no stored bytes whose metadata could match) and a
//     retarget is never "bit rot". An unreadable Readlink -> `broken`;
//     a vanished link -> `missing`.
//   - Entry-KIND mismatch (record says regular, source is now a
//     directory / symlink / special, or vice versa) -> `changed`. The
//     structural difference alone proves a modification, and the
//     7.3 content comparison is IMPOSSIBLE across kinds (you cannot
//     hash-compare a directory against a file record, and following a
//     symlink that replaced a regular file would violate D-03's
//     never-follow rule). Classifying that `broken` would claim
//     unreadability for a perfectly readable entry; `changed` names
//     what is true (a modification) and rescan captures it.
//   - Chunked files: the whole-file files.hash is the anchor. The
//     source stream is hashed ONCE and compared against it; validate
//     NEVER opens or walks the chunk rows (7.2/FR-041 already own the
//     per-chunk media story; validate answers "does the source still
//     match what was catalogued", which files.hash alone decides).
//   - Unassigned / unplanned records are compared like any other:
//     validate is source-versus-catalog, never source-versus-media, so
//     image_id/copied state is irrelevant (and is not even selected by
//     the catalog read).
//   - No deleted-from-catalog handling: this command is read-only
//     (2.1 Option B - no flock, mode=ro, no runs row, no run_events).
//     It never marks anything missing, never versions anything, never
//     writes a byte; the rescan workflow owns catalog mutation.
//   - Directories are not categorised: the 7.3 classes are content
//     classes over the files table (regular + symlink records).
//     Empty-directory durability is the directories table's job and is
//     reconciled by scan/rescan (FR-037), not by validate - counting
//     every implied non-empty parent as `new` would flood the
//     informational class with entries the catalog never records.
//     Special files (device/FIFO/socket) are likewise never `new`:
//     scan itself skips them with a warning (3.1), so "rescan would
//     capture it" would be false.
//   - The source walk does NOT re-apply scan-time exclusions (the
//     --exclude set lives in runs.exclude_json and validate carries no
//     filter flags): an entry the original scan skipped shows as `new`.
//     That is honest for a source-vs-catalog comparison and harmless
//     by construction, because `new` never moves the exit code.
//     Unreadable directories during that walk warn and prune (the T1.3
//     walk convention) - `missing` detection is unaffected (it drives
//     off the catalog side), only `new` discovery under-reports in the
//     pruned subtree.
//
// # Progress (DESIGN_SPEC 9.1; T5.1 patterns)
//
// One determinate Phase ("validate") whose totals the CATALOG knows up
// front: TotalFiles is the active-record count, TotalBytes the summed
// recorded size (the 2.3 symlink len(target) convention included - the
// bar's baseline is the catalog, so it converges on a clean run).
// ItemDone fires per compared RECORD (the T5.2 brief: record count is
// the work unit), its Bytes following the same baseline: readable
// content reports the bytes actually hashed (equal to the baseline
// unless the content really is a different length), symlinks report
// the target string read, and unreadable or missing records report
// their recorded size - the record WAS processed to a verdict, and the
// baseline it was counted against is the catalog's. Renderers saturate
// any overshoot (6.2's cap), so a genuinely longer changed file never
// rolls the display back. The `new` walk is event-silent by design (it
// completes BEFORE the hashing pass, so the bar fills to 100% on real
// comparison work rather than trailing it); the cli wraps the whole run
// with the run-level heartbeat (cli/validate.go, the build.go pattern),
// which fills any silent stretch longer than HeartbeatInterval - a
// single huge file hashing for 20+ seconds between ItemDone events is
// exactly the stretch the heartbeat exists for.
//
// Cancellation: a cancelled context aborts between records and inside
// the hash stream (the ctx-aware reader), and never yields a verdict
// for the record it aborted mid-way.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"isopress/internal/fsmeta"
	"isopress/internal/progress"
)

// The six DESIGN_SPEC 7.3 categories. The values are the report
// vocabulary: human lines are category-prefixed with them, the --json
// findings carry them in "category", and Anomalies() sums all but
// CatOK and CatNew (the informational class, 7.3).
const (
	// CatOK: the content anchor matches.
	CatOK = "ok"
	// CatChanged: metadata differs AND the content differs (plausible
	// modification; rescan captures it).
	CatChanged = "changed"
	// CatCorrupt: metadata matches AND the content differs (bit rot /
	// inconsistent source).
	CatCorrupt = "corrupt"
	// CatBroken: the entry is stat-able but its content cannot be read
	// (the T1.5 unreadable mirror) - see the file doc for the lstat /
	// readlink halves.
	CatBroken = "broken"
	// CatMissing: catalog-active record with no source entry.
	CatMissing = "missing"
	// CatNew: source entry with no catalog-active record
	// (INFORMATIONAL - never an anomaly, 7.3).
	CatNew = "new"
)

// PhaseValidate is validate's single progress phase name (9.3's
// plain-line prefix: "validate: files 1/13, ..."). One phase, so the
// name carries no parenthetical the way scan(hash)/build(...) do.
const PhaseValidate = "validate"

// SourceRecord is one catalog-ACTIVE files row as the comparison
// consumes it. The cli maps catalog.ValidateRecord onto it field for
// field (the scanStoreTx pure-mapping pattern), keeping this package
// free of catalog imports - internal/verify stays the low-level side
// (deps_test.go's layering pin) and reaches the database only through
// values a caller hands over.
type SourceRecord struct {
	FileID     int64
	RelPath    string
	EntryKind  string // 'regular' | 'symlink' (2.3 CHECK vocabulary)
	Size       int64  // bytes; len(target) for symlinks (2.3)
	MtimeNS    int64
	Dev        int64
	Inode      int64
	LinkTarget string // symlinks (D-03); "" for regular rows
	Hash       []byte // regular rows (sha256, D-09); nil for symlink rows
}

// Finding is one non-ok categorisation: the 7.3 category, the
// slash-separated relpath, and the rendered detail (the parenthesised
// tail of the human line; "" when the category says it all, e.g.
// missing). Ok records are COUNTED, never listed - see Result.
type Finding struct {
	Category string
	Path     string
	Detail   string
}

// Result is the outcome of one validate run. Compared is the number of
// catalog-ACTIVE records that went through the comparison; the six
// category counts cover records (all but New) plus source entries (New)
// and therefore sum to Compared + New.
type Result struct {
	// Compared is the catalog-ACTIVE record count the run walked.
	Compared int

	OK      int // records whose content anchor matched
	Changed int // anomaly: content differs, metadata differs
	Corrupt int // anomaly: content differs, metadata matches
	Broken  int // anomaly: entry present, content unreadable
	Missing int // anomaly: record with no source entry
	New     int // INFORMATIONAL: source entries with no record (7.3)

	// Findings carries every non-ok categorisation: record findings in
	// file_id order (the catalog read's ORDER BY), then new findings in
	// source-walk order (filepath.WalkDir's lexical order). Ok records
	// are deliberately absent - a 100k-file set's report lists
	// findings, not successes (the scan/verify family convention);
	// their COUNT travels in OK.
	Findings []Finding
}

// Anomalies is the exit-decision count of 7.3 ("exit 0 when clean; 10
// when any anomaly is found"): changed + corrupt + broken + missing.
// New is EXCLUDED by design - 7.3 marks it informational, and
// informational never moves the exit code in either direction (a
// new-only run exits 0, and --strict has nothing to promote). This is
// the single place the anomaly/informational split is decided; the cli
// consumes it verbatim.
func (r Result) Anomalies() int { return r.Changed + r.Corrupt + r.Broken + r.Missing }

// ValidateInput is one validate run: the set's source root (sets.source_root -
// 7.3 names no root argument of its own, and the catalog binds set ->
// root), every catalog-ACTIVE record of the set, and the run's ONE
// resolved change-detection mode (3.5) whose DiffFields gates the
// changed-vs-corrupt split.
type ValidateInput struct {
	// Root is the absolute source root the records' relpaths resolve
	// against (sets.source_root, 2.3).
	Root string
	// Records are the catalog-ACTIVE rows, in the order they should be
	// compared and reported (file_id order from the catalog read).
	Records []SourceRecord
	// Mode is the once-per-run resolved change-detection mode (3.5):
	// the zero value is ComparisonFull - the safe pre-amendment set -
	// so an unthreaded caller compares size+mtime_ns+dev+inode.
	Mode fsmeta.Mode
}

// Validate runs the 7.3 comparison of source versus catalog (see the
// file doc for the categories, the readings, and the progress
// contract). bus may be nil (no events); a non-nil bus receives the
// determinate Phase, one ItemDone per compared record, and walk
// warnings. The returned error is reserved for aborts - a cancelled
// context - and for a catalog-integrity violation (an active REGULAR
// record with no stored hash: the 2.3 D-09 shape makes that row
// impossible, so reporting it as a category would misname a catalog
// fault as a source verdict); every per-record problem is a Finding.
func Validate(ctx context.Context, in ValidateInput, bus *progress.Bus) (Result, error) {
	if in.Root == "" {
		return Result{}, fmt.Errorf("verify: validate requires the set's source root")
	}
	var res Result
	res.Compared = len(in.Records)
	if err := ctx.Err(); err != nil {
		return res, err
	}

	// THE DETERMINATE PHASE (T5.2 brief): the catalog knows both
	// totals before any syscall - record count and summed recorded
	// size (symlink len(target) included, the 2.3 convention; it is
	// content the run reads verbatim).
	if bus != nil {
		var totalBytes int64
		for _, rec := range in.Records {
			totalBytes += rec.Size
		}
		bus.Emit(progress.Phase{
			Name:       PhaseValidate,
			TotalKnown: true,
			TotalFiles: int64(len(in.Records)),
			TotalBytes: totalBytes,
		})
	}

	// The SOURCE-ENTRY walk first (the `new` discovery): it is
	// syscall-cheap next to hashing, and running it before the
	// comparison lets the bar's final stretch be the hash work that
	// reaches 100% - instead of a silent tail after it (file doc,
	// Progress). Its findings are STAGED and appended after the record
	// findings below, so the report always leads with the anomalies
	// (record order) and tails with the informational new entries
	// (walk order) - the documented Result.Findings order.
	known := make(map[string]struct{}, len(in.Records))
	for _, rec := range in.Records {
		known[rec.RelPath] = struct{}{}
	}
	newFindings, err := walkNew(ctx, in.Root, known, bus)
	if err != nil {
		return res, err
	}
	res.New = len(newFindings)

	// The RECORD side: one lstat + at most one hash (or one readlink)
	// per active record, one ItemDone per compared record.
	for _, rec := range in.Records {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		finding, item, err := classifyRecord(ctx, in.Root, rec, in.Mode)
		if err != nil {
			return res, err // cancellation or the catalog-integrity gate
		}
		switch finding.Category {
		case CatOK:
			res.OK++
		case CatChanged:
			res.Changed++
		case CatCorrupt:
			res.Corrupt++
		case CatBroken:
			res.Broken++
		case CatMissing:
			res.Missing++
		}
		if finding.Category != CatOK {
			res.Findings = append(res.Findings, finding)
		}
		if bus != nil {
			bus.Emit(progress.ItemDone{Bytes: item, Path: rec.RelPath})
		}
	}
	// The informational `new` tail (staged by the walk above), in walk
	// order behind every record finding.
	res.Findings = append(res.Findings, newFindings...)
	return res, nil
}

// itemBytes is a record's progress byte figure when its content could
// NOT be read (broken, missing, kind-mismatched): the RECORDED size.
// The Phase declared its byte totals in recorded sizes, and the record
// WAS processed to a verdict against that baseline, so the bar's
// accounting stays on one baseline whether or not bytes materialised.
// Readable content reports the bytes ACTUALLY hashed/read instead (the
// classifyRecord return), which equals the baseline unless the content
// really is a different length - an overshoot the renderers saturate
// per 6.2, never roll back.
func itemBytes(rec SourceRecord) int64 { return rec.Size }

// classifyRecord categorises ONE active record against its source path
// (file doc for the rules) and returns the record's ItemDone byte
// figure beside the Finding: content actually read reports the bytes
// actually read (hash count for regular files, target length for
// symlinks), content that could not be read reports the recorded size
// (itemBytes). Every non-ok branch renders the exact detail the human
// report pins. The error return is abort-only: a cancelled context, or
// the regular-row-without-hash integrity gate.
func classifyRecord(ctx context.Context, root string, rec SourceRecord, mode fsmeta.Mode) (Finding, int64, error) {
	abs := filepath.Join(root, filepath.FromSlash(rec.RelPath))

	fi, err := os.Lstat(abs)
	if err != nil {
		// MISSING vs BROKEN at the stat gate (file doc): ENOENT and
		// ENOTDIR both mean the entry as recorded cannot exist (the
		// path is gone, or an intermediate component stopped being a
		// directory); every other errno leaves existence UNPROVEN, so
		// it lands in the unreadable class with the errno detail
		// instead of falsely claiming absence.
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return Finding{Category: CatMissing, Path: rec.RelPath}, itemBytes(rec), nil
		}
		return Finding{Category: CatBroken, Path: rec.RelPath,
			Detail: fmt.Sprintf("lstat failed: %s", errnoDetail(err))}, itemBytes(rec), nil
	}

	srcKind := fsmeta.Classify(fi)
	recKind := rec.EntryKind
	switch {
	case recKind == "symlink" && srcKind == fsmeta.KindSymlink:
		// The symlink half (file doc): the TARGET is the content.
		target, err := os.Readlink(abs)
		if err != nil {
			return Finding{Category: CatBroken, Path: rec.RelPath,
				Detail: fmt.Sprintf("readlink failed: %s", errnoDetail(err))}, itemBytes(rec), nil
		}
		if target != rec.LinkTarget {
			return Finding{Category: CatChanged, Path: rec.RelPath,
					Detail: fmt.Sprintf("symlink target differs: recorded %q, now %q", rec.LinkTarget, target)},
				int64(len(target)), nil
		}
		// Equal target -> ok, DANGLED or not: the recorded content is
		// intact (file doc; the dangling case is test-pinned).
		return Finding{Category: CatOK, Path: rec.RelPath}, int64(len(target)), nil

	case recKind == "regular" && srcKind == fsmeta.KindRegular:
		// The regular half: hash FIRST, metadata only decides between
		// changed and corrupt (7.3 - a matching hash is ok even when
		// every metadata field drifted).
		cur, err := fsmeta.Capture(fi)
		if err != nil {
			// Capture only fails when FileInfo carries no Linux
			// *syscall.Stat_t (NFR-001 violation) - a systemic fault,
			// not a per-record verdict. Surface it as an abort.
			return Finding{}, itemBytes(rec), fmt.Errorf("verify: validate metadata capture of %s: %w", rec.RelPath, err)
		}
		if len(rec.Hash) == 0 {
			// The 2.3 D-09 shape makes an active regular row carry a
			// hash (no-hash rows are unreadable/changed_during_scan
			// status); reporting this as a source verdict would misname
			// a catalog fault.
			return Finding{}, itemBytes(rec), fmt.Errorf(
				"verify: catalog integrity: active regular record %q (file_id %d) has no stored hash",
				rec.RelPath, rec.FileID)
		}
		digest, n, err := hashSourceFile(ctx, abs)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return Finding{}, itemBytes(rec), ctxErr // cancellation aborts, it is no verdict
			}
			// The T1.5 unreadable mirror: stat succeeded, the READ
			// failed (EACCES on open, EIO/ESTALE mid-stream).
			return Finding{Category: CatBroken, Path: rec.RelPath,
				Detail: fmt.Sprintf("unreadable: %s", errnoDetail(err))}, itemBytes(rec), nil
		}
		if bytes.Equal(digest, rec.Hash) {
			return Finding{Category: CatOK, Path: rec.RelPath}, n, nil // 7.3: hash matches = ok, full stop
		}
		// Content differs: the resolved metadata set (3.5, the ONE
		// fsmeta.DiffFields) decides plausible-modification vs bit rot.
		if d := fsmeta.DiffFields(recordMeta(rec), cur, mode); d.Any() {
			return Finding{Category: CatChanged, Path: rec.RelPath,
				Detail: fmt.Sprintf("metadata: %s; hash differs", d)}, n, nil
		}
		return Finding{Category: CatCorrupt, Path: rec.RelPath,
			Detail: "bit rot: metadata matches, hash differs"}, n, nil

	default:
		// The kind-mismatch half (file doc): a proven modification
		// whose content comparison is impossible across kinds.
		return Finding{Category: CatChanged, Path: rec.RelPath,
			Detail: fmt.Sprintf("entry kind changed: %s -> %s (content not comparable across kinds)",
				recKind, kindWord(srcKind))}, itemBytes(rec), nil
	}
}

// recordMeta builds the CATALOG side of the DiffFields comparison. The
// size/mtime_ns/dev/inode fields are the resolved D-10 set's four; the
// mode/uid/gid/nlink fields stay ZERO because DiffFields never
// compares them (3.5: a chmod, chown, or link-count change does not
// invalidate a captured hash) and the 7.3 metadata gate is that same
// comparison - nothing may be compared here that 3.5 excludes.
func recordMeta(rec SourceRecord) fsmeta.Meta {
	return fsmeta.Meta{
		Size:    rec.Size,
		MtimeNS: rec.MtimeNS,
		Dev:     uint64(rec.Dev),
		Inode:   uint64(rec.Inode),
	}
}

// kindWord renders an fsmeta.EntryKind for the kind-transition detail.
// The catalog vocabulary ("regular", "symlink") matches
// EntryKind.String() for the two record kinds; "directory" and
// "special" name the non-record source kinds a record can encounter.
func kindWord(k fsmeta.EntryKind) string { return k.String() }

// walkNew discovers source entries with no catalog-ACTIVE record: the
// `new` class (file doc for why directories and special files never
// count). It returns the findings in WALK order (lexical), which the
// caller appends behind the record findings so `new` always tails the
// report. Failures of the walk itself are WARNINGS that prune the
// affected subtree (the T1.3 walker convention): a directory validate
// cannot read hides nothing from the record side (missing detection
// runs off the catalog), it only under-reports `new` beneath it, so
// one unreadable directory must never abort the comparison. A
// cancelled context still aborts (cancellation is not a verdict).
func walkNew(ctx context.Context, root string, known map[string]struct{}, bus *progress.Bus) ([]Finding, error) {
	var findings []Finding
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if bus != nil {
				bus.Emit(progress.Warn{Msg: fmt.Sprintf("validate: walking %s: %v (new-entry discovery pruned there)", path, walkErr)})
			}
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil // includes root vanished/unreadable: nothing to discover
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == root {
			return nil
		}
		kind, err := fsmeta.ClassifyDirEntry(d)
		if err != nil {
			// Entry vanished between readdir and classification: it is
			// not a verdict, just undiscoverable - the same tolerance
			// the T1.3 walker applies to a mid-walk lstat failure.
			return nil
		}
		switch kind {
		case fsmeta.KindDirectory:
			return nil // implied parents are never records; empty-dir durability is scan's (file doc)
		case fsmeta.KindSpecial:
			return nil // scan never records specials (3.1), so "rescan would capture it" would be false
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil // path outside root: unreachable for WalkDir; tolerate, never abort
		}
		rel = filepath.ToSlash(rel)
		if _, ok := known[rel]; ok {
			return nil // covered by a record: already compared (or reported) above
		}
		findings = append(findings, Finding{Category: CatNew, Path: rel,
			Detail: "informational; a rescan would capture it"})
		return nil
	})
	if err != nil {
		// Only an abort reaches here (the callback swallows per-entry
		// walk errors as warnings): a cancelled context, chiefly.
		return findings, err
	}
	return findings, nil
}

// hashSourceFile streams one source file through SHA-256 (the
// established streaming pattern; ctx-aware so a cancelled run never
// sits inside a multi-gigabyte read). Returns the digest and the byte
// count read.
func hashSourceFile(ctx context.Context, path string) ([]byte, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, fmt.Errorf("verify: opening source file %s: %w", path, err)
	}
	defer f.Close() //nolint:errcheck // read-only close; the verdict dominates

	h := sha256.New()
	n, err := io.Copy(h, &verifyCtxReader{ctx: ctx, r: f})
	if err != nil {
		return nil, 0, fmt.Errorf("verify: hashing source file %s: %w", path, err)
	}
	return h.Sum(nil), n, nil
}

// errnoDetail extracts the human errno text ("permission denied",
// "input/output error", "not a directory") from an error wrapping
// *os.PathError and/or syscall.Errno - the T1.5 detail shape
// (ingest/errnoText), mirrored here as the small local helper the
// broken-category details pin. The full error string is the honest
// fallback when no errno is present.
func errnoDetail(err error) string {
	if err == nil {
		return ""
	}
	var perr *os.PathError
	if errors.As(err, &perr) {
		var errno syscall.Errno
		if errors.As(perr.Err, &errno) {
			return errno.Error()
		}
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno.Error()
	}
	return err.Error()
}
