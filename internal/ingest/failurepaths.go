package ingest

// T1.5: the ingest pipeline's failure paths (DESIGN_SPEC 3.4 unreadable
// files, 3.5 point 1 mid-scan change detection with D-10; PLAN_SPEC
// FR-003, D-10). This file owns three things and NOTHING else:
//
//  1. CLASSIFICATION - turning a HashResult (T1.4) into exactly one of
//     three per-file outcomes;
//  2. WARNING EMISSION - a progress.Warn per failure class (DESIGN_SPEC
//     9.1, unthrottled per 9.3), nil-Bus safe;
//  3. AGGREGATION - the Failures accumulator that collects per-file
//     failures for the end-of-run report and the exit-10 carrier
//     (report.ItemFailures, DESIGN_SPEC 10.2 row 7).
//
// # Classification classes
//
//	ok                   digest valid; the file hashed cleanly AND a
//	                     fresh lstat at RESULT-PROCESSING time still
//	                     matches the walk-time baseline (Job.Meta)
//	unreadable           HashResult.Err != nil: the open OR a mid-stream
//	                     read failed (EACCES, EIO, ESTALE on NAS,
//	                     DESIGN_SPEC 3.4). No hash; Detail carries the
//	                     errno text for the catalog status_detail column
//	changed_during_scan  no read error, but the fresh post-hash lstat
//	                     differs from the walk-time baseline in ANY of
//	                     the RESOLVED D-10 field set (full: size,
//	                     mtime_ns, dev, inode; metadata mode - the 2026-
//	                     20 CIFS amendment - size and mtime_ns only;
//	                     compared via fsmeta.DiffFields, the single
//	                     comparison of DESIGN_SPEC 3.5, parameterised by
//	                     the run's resolved fsmeta.Mode). No hash;
//	                     Detail names the differing fields
//
// Precedence (documented, test-pinned): a read error WINS. When
// HashResult.Err is non-nil the classifier does not consult the fresh
// lstat at all - the file could not be read, so "unreadable" is the
// operative outcome regardless of what the filesystem looks like now.
//
// # The lstat seam (D-10 point-1 timing)
//
// The design mandates the second lstat AFTER hashing completes
// ("lstat before hashing; after hashing completes, lstat again"), so
// the fresh metadata must be taken at RESULT-PROCESSING time - when the
// consumer receives the HashResult - not at pool-completion or
// walk-completion time. This package makes that timing explicit and
// testable:
//
//   - Classify(result, current, mode) is the PURE core: it takes the
//     fresh fsmeta.Meta AND the run's resolved fsmeta.Mode as
//     arguments and performs no syscalls. The caller is responsible
//     for taking the fresh lstat before calling.
//   - ClassifyPath(result, src, mode) is the convenience entry point:
//     it takes the fresh meta through an injectable MetaSource (nil =
//     LstatMeta, the production os-lstat source). T1.6's result
//     consumer calls ClassifyPath (or Failures.Process) once per
//     HashResult as it drains the pool, which places the fresh lstat
//     exactly at result-processing time; tests inject a fake source to
//     pin the comparison without touching the filesystem.
//
// # Deleted-after-hash and other post-hash lstat failures (documented
// decision)
//
// If the fresh lstat FAILS after a successful hash, the file's current
// state cannot be confirmed against the walk-time baseline. The
// DESIGN_SPEC 2.3 status set has only four values
// (active/missing/unreadable/changed_during_scan), so:
//
//   - ENOENT (the file was DELETED between hash and classify) is
//     classified as changed_during_scan. Deletion is the extreme form
//     of "changed since the walk": the hashed bytes can never be
//     re-verified, so storing them as 'active' would describe content
//     the source no longer has, and 'missing' belongs to the rescan
//     reconciler's path-not-seen case (DESIGN_SPEC 3.7), not to a
//     hash that was actually taken this run. changed_during_scan with
//     no hash stored is the design-consistent outcome: excluded from
//     planning, picked up by the next scan (which will record it
//     missing if it stays gone). Detail: "deleted during scan: no such
//     file or directory".
//   - Any OTHER lstat failure (EACCES, EIO, ...) is conservatively
//     classified as changed_during_scan as well: stability cannot be
//     confirmed, so no hash is stored. Detail: "cannot re-verify after
//     hashing: <errno>".
//
// # Warnings and the no-double-report boundary
//
// Both failure classes emit progress.Warn events through the Failures
// accumulator (nil Bus silences emission, nothing else changes).
// Directories CANNOT be double-reported here: T1.3's walker already
// warns and prunes unreadable directories at walk time, and only
// regular-file Jobs ever become HashResults, so a directory failure
// never reaches this layer. Symlinks and special files never produce
// hash jobs either (DESIGN_SPEC 3.1), so they produce no failure rows.
//
// # Run-continues semantics (FR-003, DESIGN_SPEC 3.4)
//
// Classification is a pure per-file function: one file's failure never
// influences another's classification, and the pipeline NEVER aborts on
// an item failure - failures accumulate, the run completes, and the
// end-of-run exit code becomes 10 through report.ItemFailures.
//
// # Ownership split with T1.6 (documented seam)
//
// THIS task (T1.5) owns: classification, warning emission, failure
// aggregation, and the exit-10 carrier. T1.6 (the reconciler and scan
// command) owns:
//
//   - catalog writes: files.status / files.status_detail / files.hash
//     per class (use ClassifiedResult.CatalogStatus for the status
//     column: 'ok' stores as 'active' with the hash; 'unreadable' and
//     'changed_during_scan' store verbatim with Detail as status_detail
//     and hash NULL; 'missing' is the reconciler's own outcome),
//   - run_events rows (one warn row per Failures.Entries() entry:
//     relpath + Detail),
//   - the end-of-run stdout report format (human and --json),
//   - returning Failures.Err() (nil keeps exit 0) as the command error.

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"

	"isopress/internal/fsmeta"
	"isopress/internal/progress"
	"isopress/internal/report"
)

// The three classification outcomes. The values match the catalog
// files.status CHECK vocabulary of DESIGN_SPEC 2.3 except for
// StatusOK, which the catalog stores as 'active' (see
// ClassifiedResult.CatalogStatus); 'missing' is a reconciler outcome
// (T1.6), never a classification one.
const (
	// StatusOK: digest valid, post-hash lstat matches the baseline.
	StatusOK = "ok"
	// StatusUnreadable: open or mid-stream read failed (DESIGN_SPEC
	// 3.4); no hash, Detail is the errno text.
	StatusUnreadable = "unreadable"
	// StatusChangedDuringScan: the fresh post-hash lstat differs from
	// the walk-time baseline (DESIGN_SPEC 3.5 point 1, D-10); no hash,
	// Detail names the differing fields.
	StatusChangedDuringScan = "changed_during_scan"
)

// MetaSource is the injectable fresh-lstat source used by ClassifyPath.
// The production source is LstatMeta; tests substitute fakes to pin the
// D-10 comparison deterministically.
type MetaSource func(path string) (fsmeta.Meta, error)

// LstatMeta is the production MetaSource: fsmeta.PathMeta, an lstat that
// never follows a final symlink (D-03). ClassifyPath uses it when the
// caller passes a nil source.
func LstatMeta(path string) (fsmeta.Meta, error) { return fsmeta.PathMeta(path) }

// ClassifiedResult is one HashResult plus its T1.5 outcome. It embeds
// the HashResult so consumers keep access to RelPath, AbsPath, the
// walk-time baseline Meta, the Digest, and the byte count.
type ClassifiedResult struct {
	HashResult
	// Status is exactly one of StatusOK, StatusUnreadable,
	// StatusChangedDuringScan.
	Status string
	// Detail is the catalog status_detail text: the errno string for
	// unreadable, the changed-fields or deletion text for
	// changed_during_scan, empty for ok.
	Detail string
	// Fields is the fsmeta.Diff bitmask for changed_during_scan (which
	// of the RESOLVED set's fields differed - full: size/mtime_ns/
	// dev/inode; metadata mode: size/mtime_ns); zero for other classes.
	// T1.6 may use it for structured reporting; Detail already carries
	// the rendered form.
	Fields fsmeta.Diff
}

// Classify is the PURE classification core: it compares result against
// current (the fresh fsmeta.Meta, taken by the CALLER at
// result-processing time) under the run's resolved change-detection
// mode and returns the ClassifiedResult. It performs no syscalls and
// never mutates its inputs, so one file's classification can never
// affect another's (the FR-003 run-continues property).
//
// Precedence: result.Err != nil wins outright - the fresh meta is
// ignored and the outcome is StatusUnreadable with the errno text in
// Detail (see the file doc). On the success path the RESOLVED D-10
// field set is compared via fsmeta.DiffFields (the SINGLE comparison
// of DESIGN_SPEC 3.5, never a hand-rolled variant). The mode parameter
// is the once-per-run resolved fsmeta.Mode the cli wired in (3.5
// filesystem-class rule): the zero value is the FULL set - the safe
// pre-amendment default - and a metadata mode drops dev/inode from the
// comparison (the CIFS amendment; the content guarantee is unaffected
// because no hash is ever stored for a changed file either way).
func Classify(result HashResult, current fsmeta.Meta, mode fsmeta.Mode) ClassifiedResult {
	c := ClassifiedResult{HashResult: result, Status: StatusOK}
	if result.Err != nil {
		c.Status = StatusUnreadable
		c.Detail = errnoText(result.Err)
		return c
	}
	if d := fsmeta.DiffFields(result.Meta, current, mode); d.Any() {
		c.Status = StatusChangedDuringScan
		c.Fields = d
		c.Detail = fmt.Sprintf("fields %s changed during scan", d)
	}
	return c
}

// ClassifyPath classifies result under the run's resolved
// change-detection mode after taking the fresh lstat through src at
// CALL time (nil src selects the production LstatMeta). It is the
// convenience entry point for T1.6's result consumer: calling it once
// per drained HashResult places the second lstat exactly where
// DESIGN_SPEC 3.5 point 1 requires it - after hashing completes, at
// result-processing time.
//
// An lstat failure in src is itself a classification input (documented
// in the file doc): ENOENT becomes changed_during_scan with a
// "deleted during scan" detail; any other lstat failure becomes
// changed_during_scan with a "cannot re-verify after hashing" detail.
// A non-nil result.Err short-circuits before src is consulted.
func ClassifyPath(result HashResult, src MetaSource, mode fsmeta.Mode) ClassifiedResult {
	if result.Err != nil {
		// Unreadable wins: no fresh lstat is taken for a file that
		// could not be read (error precedence, file doc).
		return Classify(result, fsmeta.Meta{}, mode)
	}
	if src == nil {
		src = LstatMeta
	}
	fresh, err := src(result.AbsPath)
	if err != nil {
		c := ClassifiedResult{HashResult: result, Status: StatusChangedDuringScan}
		if errors.Is(err, os.ErrNotExist) {
			c.Detail = fmt.Sprintf("deleted during scan: %s", errnoText(err))
		} else {
			c.Detail = fmt.Sprintf("cannot re-verify after hashing: %s", errnoText(err))
		}
		return c
	}
	return Classify(result, fresh, mode)
}

// errnoText extracts the human errno text ("permission denied",
// "input/output error", "no such file or directory") from an error that
// wraps *os.PathError and/or syscall.Errno - exactly the shape T1.4's
// pool produces ("ingest: open <rel>: open <abs>: <errno>"). When no
// errno is present the full error string is the honest fallback.
func errnoText(err error) string {
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

// CatalogStatus maps the classification Status to the catalog
// files.status CHECK value of DESIGN_SPEC 2.3: ok stores as 'active'
// (with the hash); the two failure classes store verbatim (with
// status_detail and hash NULL). 'missing' never originates here - it is
// the T1.6 reconciler's path-not-seen outcome (DESIGN_SPEC 3.7).
func (c ClassifiedResult) CatalogStatus() string {
	if c.Status == StatusOK {
		return "active"
	}
	return c.Status
}

// WarnMsg renders the progress.Warn / run_events message for a failure
// class: path plus reason, per DESIGN_SPEC 3.4 and 3.5 point 1 ("a
// warning event" for both classes). Empty for StatusOK (nothing to
// warn about).
func (c ClassifiedResult) WarnMsg() string {
	switch c.Status {
	case StatusUnreadable:
		return fmt.Sprintf("unreadable file %s: %s (no hash recorded)", c.RelPath, c.Detail)
	case StatusChangedDuringScan:
		return fmt.Sprintf("file %s: %s (no hash recorded; excluded from planning until a later stable scan)",
			c.RelPath, c.Detail)
	default:
		return ""
	}
}

// Line renders the one-line end-of-run report form:
// "<status> <relpath>: <detail>".
func (c ClassifiedResult) Line() string {
	return fmt.Sprintf("%s %s: %s", c.Status, c.RelPath, c.Detail)
}

// Failures is the per-run accumulator of item failures (FR-003,
// DESIGN_SPEC 3.4: the scan CONTINUES on every item failure; failures
// accumulate here instead). It also owns the warning emission for both
// failure classes AND carries the run's resolved change-detection mode
// (3.5: ONE fsmeta.Mode per run threads from the cli into every
// classification this accumulator performs). Add is safe for
// concurrent use (a mutex guards the entries; the progress.Bus is
// already concurrency-safe by design).
//
// T1.6 wiring: construct one Failures per run with the run's Bus and
// the run's resolved fsmeta.Mode, call Process once per drained
// HashResult (classification + accumulation + warning in one call),
// persist catalog status/status_detail from the returned
// ClassifiedResult, write run_events from Entries(), render the stdout
// report from Report(), and return Err() as the command error - nil
// keeps exit 0, non-nil classifies to exit 10.
type Failures struct {
	mu      sync.Mutex
	bus     *progress.Bus
	mode    fsmeta.Mode
	entries []ClassifiedResult
}

// NewFailures returns an accumulator whose failure warnings are emitted
// on bus (nil silences emission; everything else is unchanged) and
// whose classifications compare under mode (the zero value is the FULL
// set - fsmeta.ModeFull, the safe default documented on
// fsmeta.ComparisonFull).
func NewFailures(bus *progress.Bus, mode fsmeta.Mode) *Failures {
	return &Failures{bus: bus, mode: mode}
}

// Process classifies result against a fresh lstat taken through src at
// call time (nil src = LstatMeta; see ClassifyPath) under the
// accumulator's resolved mode, adds any failure to the accumulator
// (emitting its progress.Warn), and returns the classification so the
// caller can write the catalog record. Ok results are classified but
// NOT accumulated.
func (f *Failures) Process(result HashResult, src MetaSource) ClassifiedResult {
	c := ClassifyPath(result, src, f.mode)
	f.Add(c)
	return c
}

// Add records one classification and emits its warning. StatusOK
// entries are ignored (a successful file is not a failure); every other
// status is recorded, in call order.
func (f *Failures) Add(c ClassifiedResult) {
	if c.Status == StatusOK {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.entries = append(f.entries, c)
	if f.bus != nil {
		f.bus.Emit(progress.Warn{Msg: c.WarnMsg()})
	}
}

// Len reports the number of accumulated failures.
func (f *Failures) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.entries)
}

// Entries returns a copy of the accumulated failures in call order -
// the T1.6 seam for run_events rows (relpath + Detail per entry).
func (f *Failures) Entries() []ClassifiedResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ClassifiedResult, len(f.entries))
	copy(out, f.entries)
	return out
}

// FailureReport is the aggregated end-of-run failure summary: counts by
// class plus one rendered line per failure, in call order. T1.6 owns
// the stdout/JSON presentation of this data, not this package.
type FailureReport struct {
	// Total is the number of failures across both classes.
	Total int
	// Unreadable counts StatusUnreadable failures.
	Unreadable int
	// ChangedDuringScan counts StatusChangedDuringScan failures.
	ChangedDuringScan int
	// Lines holds one ClassifiedResult.Line() per failure, in call
	// order.
	Lines []string
}

// Report aggregates the accumulated failures (counts by class plus
// per-file lines) for the end-of-run report.
func (f *Failures) Report() FailureReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	var r FailureReport
	for _, e := range f.entries {
		switch e.Status {
		case StatusUnreadable:
			r.Unreadable++
		case StatusChangedDuringScan:
			r.ChangedDuringScan++
		}
		r.Lines = append(r.Lines, e.Line())
	}
	r.Total = len(f.entries)
	return r
}

// Err produces the exit-10 carrier: nil when no failures accumulated
// (report.Classify(nil) = exit 0, the clean-run guarantee) and a
// report.ItemFailures enumerating every failure otherwise
// (report.Classify maps it to report.ExitItemFailures = 10, DESIGN_SPEC
// 10.2 row 7). The run itself always completes; this is only the
// end-of-run error the cli layer classifies.
func (f *Failures) Err() *report.ItemFailures {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.entries) == 0 {
		return nil
	}
	errs := make([]error, 0, len(f.entries))
	for _, e := range f.entries {
		errs = append(errs, errors.New(e.Line()))
	}
	return &report.ItemFailures{Failures: errs}
}
