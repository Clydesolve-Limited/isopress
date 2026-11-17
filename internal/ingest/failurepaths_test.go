package ingest

// T1.5 failure-path tests: classification of HashResults into ok /
// unreadable / changed_during_scan (DESIGN_SPEC 3.4 and 3.5 point 1,
// D-10), errno detail extraction for the catalog status_detail column,
// warning emission on the progress bus (nil-bus safe), the Failures
// accumulator with its exit-10 carrier (DESIGN_SPEC 10.2 row 7), the
// no-double-report boundary against T1.3's directory warnings, and the
// real Walk -> pool -> classify pipeline over a fixture with one good
// file, one chmod-000 file, and one mutated-mid-hash file.
//
// Stdlib only; every fixture lives in t.TempDir(); chmod-000 cases skip
// as root where EACCES never fires. Synchronization is deterministic:
// the mid-hash mutation happens in the injected OpenFile's Close (the
// pool closes the file BEFORE delivering its result, so the mutation is
// visible to the processing-time lstat by construction), and the EIO
// reader fails after an exact byte count - no sleeps anywhere.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"isopress/internal/fsmeta"
	"isopress/internal/progress"
	"isopress/internal/report"
)

// --- helpers ----------------------------------------------------------------

// walkJob builds the walk-time job for one fixture file, capturing the
// fsmeta baseline with the same lstat the walker uses.
func walkJob(t *testing.T, root, name string) Job {
	t.Helper()
	abs := filepath.Join(root, name)
	meta, err := fsmeta.PathMeta(abs)
	if err != nil {
		t.Fatalf("lstat %s: %v", abs, err)
	}
	return Job{RelPath: name, AbsPath: abs, Meta: meta}
}

// hashViaPool runs exactly one job through the real T1.4 pool (with an
// optional OpenFile injection) and returns the single HashResult.
func hashViaPool(t *testing.T, job Job, opts PoolOptions) HashResult {
	t.Helper()
	if opts.Workers == 0 {
		opts.Workers = 1
	}
	pool := NewHashPool(context.Background(), opts)
	if err := pool.Submit(job); err != nil {
		t.Fatalf("Submit(%s): %v", job.RelPath, err)
	}
	pool.Close()
	results := drainResults(pool.Results())
	if err := pool.Wait(); err != nil {
		t.Fatalf("pool Wait: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("pool produced %d results, want 1", len(results))
	}
	return results[0]
}

// metaSourceReturning is a fake MetaSource returning a fixed meta and
// recording the path it was consulted with.
func metaSourceReturning(m fsmeta.Meta, err error, seen *[]string) MetaSource {
	return func(path string) (fsmeta.Meta, error) {
		if seen != nil {
			*seen = append(*seen, path)
		}
		return m, err
	}
}

// eioAfterReader reads at most remaining bytes, then fails with an
// EIO *os.PathError - the mid-stream read-failure shape of FR-003
// (EIO, ESTALE on NAS) with a REAL errno for the status_detail
// extraction to find.
type eioAfterReader struct {
	rc        io.ReadCloser
	path      string
	remaining int
}

func (e *eioAfterReader) Read(p []byte) (int, error) {
	if e.remaining <= 0 {
		return 0, &os.PathError{Op: "read", Path: e.path, Err: syscall.EIO}
	}
	if len(p) > e.remaining {
		p = p[:e.remaining]
	}
	n, err := e.rc.Read(p)
	e.remaining -= n
	return n, err
}

func (e *eioAfterReader) Close() error { return e.rc.Close() }

// closingMutator wraps a ReadCloser and runs after() exactly once,
// after the underlying Close - the deterministic mid-hash mutation
// seam: the T1.4 pool closes the file inside hashOne BEFORE the result
// is delivered to the consumer, so the mutation is guaranteed visible
// to the processing-time lstat of ClassifyPath.
type closingMutator struct {
	io.ReadCloser
	path  string
	after func() error
	once  sync.Once
}

func (m *closingMutator) Close() error {
	err := m.ReadCloser.Close()
	m.once.Do(func() {
		if aerr := m.after(); aerr != nil && err == nil {
			err = aerr
		}
	})
	return err
}

// --- classification: unreadable ---------------------------------------------

// TestClassifyUnreadableOpenFailureErrno: a chmod-000 file fails its
// open with EACCES (skipped as root); classification yields
// unreadable with the errno text in Detail - the exact string the
// catalog stores in status_detail (DESIGN_SPEC 3.4, 2.3). The fresh
// meta is irrelevant on the error path (precedence, file doc).
func TestClassifyUnreadableOpenFailureErrno(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; chmod 000 does not deny open")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "denied.bin"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "denied.bin"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(root, "denied.bin"), 0o644) })

	res := hashViaPool(t, walkJob(t, root, "denied.bin"), PoolOptions{})
	if res.Err == nil {
		t.Fatal("chmod-000 file hashed cleanly; the fixture is broken (running as root?)")
	}

	c := Classify(res, fsmeta.Meta{}, fsmeta.ModeFull) // zero current: must be ignored
	if c.Status != StatusUnreadable {
		t.Errorf("Status = %q, want %q", c.Status, StatusUnreadable)
	}
	if !strings.Contains(c.Detail, "permission denied") {
		t.Errorf("Detail = %q, want the EACCES errno text", c.Detail)
	}
	if c.Fields != 0 {
		t.Errorf("Fields = %v, want 0 for the unreadable class", c.Fields)
	}
	if c.CatalogStatus() != "unreadable" {
		t.Errorf("CatalogStatus = %q, want 'unreadable'", c.CatalogStatus())
	}
	// No hash on failure (FR-003): the pool zeroes the digest, and the
	// classification must not invent one.
	if c.Digest != ([32]byte{}) {
		t.Errorf("Digest = %x, want zero (no hash, FR-003)", c.Digest)
	}
}

// TestClassifyUnreadableMidStreamReadFailureErrno: an injected reader
// streams 4 bytes then fails with EIO; classification yields
// unreadable with the EIO errno text and the partial byte count (the
// progress accounting input).
func TestClassifyUnreadableMidStreamReadFailureErrno(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "flaky.bin"), []byte("abcdefgh"), 0o644); err != nil {
		t.Fatal(err)
	}
	open := func(path string) (io.ReadCloser, error) {
		rc, err := osOpen(path)
		if err != nil {
			return nil, err
		}
		return &eioAfterReader{rc: rc, path: path, remaining: 4}, nil
	}

	res := hashViaPool(t, walkJob(t, root, "flaky.bin"), PoolOptions{OpenFile: open})
	if !errors.Is(res.Err, syscall.EIO) {
		t.Fatalf("HashResult.Err = %v, want the injected EIO", res.Err)
	}
	if res.Bytes != 4 {
		t.Errorf("Bytes = %d, want the 4 bytes read before the failure", res.Bytes)
	}
	if res.Digest != ([32]byte{}) {
		t.Errorf("Digest = %x, want zero on a read failure", res.Digest)
	}

	c := Classify(res, fsmeta.Meta{}, fsmeta.ModeFull)
	if c.Status != StatusUnreadable {
		t.Errorf("Status = %q, want %q", c.Status, StatusUnreadable)
	}
	if !strings.Contains(c.Detail, "input/output error") {
		t.Errorf("Detail = %q, want the EIO errno text", c.Detail)
	}
	if c.Bytes != 4 {
		t.Errorf("classified Bytes = %d, want the partial count 4", c.Bytes)
	}
}

// TestClassifyUnreadablePrecedence: a read error WINS even when the
// fresh meta also differs - unreadable is the operative outcome and the
// fresh lstat is never consulted (src would fail the test if called).
func TestClassifyUnreadablePrecedence(t *testing.T) {
	src := func(string) (fsmeta.Meta, error) {
		t.Fatal("MetaSource consulted for an unreadable result; " +
			"the read error must take precedence (documented in failurepaths.go)")
		return fsmeta.Meta{}, nil
	}
	res := HashResult{
		Job:   Job{RelPath: "x.bin", AbsPath: "/x/x.bin", Meta: fsmeta.Meta{Size: 10}},
		Bytes: 10,
		Err:   &os.PathError{Op: "open", Path: "/x/x.bin", Err: syscall.EACCES},
	}
	c := ClassifyPath(res, src, fsmeta.ModeFull)
	if c.Status != StatusUnreadable {
		t.Errorf("Status = %q, want %q (error precedence)", c.Status, StatusUnreadable)
	}
	if !strings.Contains(c.Detail, "permission denied") {
		t.Errorf("Detail = %q, want the EACCES text", c.Detail)
	}
}

// --- classification: change detection (D-10 point 1) --------------------------

// TestClassifyUnchangedFileIsOK: a cleanly hashed file whose fresh
// lstat matches the walk-time baseline classifies ok, with the digest
// intact and the catalog status 'active'.
func TestClassifyUnchangedFileIsOK(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "keep.bin"), []byte("keep me"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := walkJob(t, root, "keep.bin")
	res := hashViaPool(t, job, PoolOptions{})
	if res.Err != nil {
		t.Fatalf("hash: %v", res.Err)
	}

	c := ClassifyPath(res, nil, fsmeta.ModeFull) // nil = production LstatMeta
	if c.Status != StatusOK {
		t.Fatalf("Status = %q, want %q (unchanged file)", c.Status, StatusOK)
	}
	if c.Detail != "" || c.Fields != 0 {
		t.Errorf("Detail/Fields = %q/%v, want empty for ok", c.Detail, c.Fields)
	}
	if want := sha256.Sum256([]byte("keep me")); c.Digest != want {
		t.Errorf("Digest = %x, want %x (digest intact on ok)", c.Digest, want)
	}
	if c.CatalogStatus() != "active" {
		t.Errorf("CatalogStatus = %q, want 'active' (DESIGN_SPEC 2.3 CHECK value)", c.CatalogStatus())
	}
	if c.WarnMsg() != "" {
		t.Errorf("WarnMsg = %q, want empty for ok (no warning for a clean file)", c.WarnMsg())
	}
}

// TestClassifyChangedAfterHashSizeAndMtime: bytes appended between hash
// and classify fire DiffFields on size (and mtime); the result is
// changed_during_scan with the which-fields detail, even though the
// pool produced a valid digest of the PRE-mutation bytes - the
// classification, not the pool, is what keeps stale bytes out of the
// catalog (DESIGN_SPEC 3.5 point 1).
func TestClassifyChangedAfterHashSizeAndMtime(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(root, "growing.bin")
	if err := os.WriteFile(abs, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := walkJob(t, root, "growing.bin")
	res := hashViaPool(t, job, PoolOptions{})
	if res.Err != nil {
		t.Fatalf("hash: %v", res.Err)
	}

	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(" and then some more")); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	c := ClassifyPath(res, nil, fsmeta.ModeFull)
	if c.Status != StatusChangedDuringScan {
		t.Fatalf("Status = %q, want %q (size changed after the hash)", c.Status, StatusChangedDuringScan)
	}
	if c.Fields&fsmeta.DiffSize == 0 {
		t.Errorf("Fields = %v, want DiffSize set", c.Fields)
	}
	if !strings.Contains(c.Detail, "size") {
		t.Errorf("Detail = %q, want the differing field name (size)", c.Detail)
	}
	if !strings.Contains(c.Detail, "changed during scan") {
		t.Errorf("Detail = %q, want the change-during-scan phrasing", c.Detail)
	}
	if c.Digest == ([32]byte{}) {
		t.Error("Digest is zero; the pool hashed the pre-mutation bytes, " +
			"the classification is what must drop them")
	}
	if c.CatalogStatus() != "changed_during_scan" {
		t.Errorf("CatalogStatus = %q, want 'changed_during_scan'", c.CatalogStatus())
	}
}

// TestClassifyChangedMtimeOnly: a chtimes-only mutation (same size,
// same inode) still fires DiffFields on mtime_ns - the field-exactness
// of the D-10 comparison.
func TestClassifyChangedMtimeOnly(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(root, "touched.bin")
	if err := os.WriteFile(abs, []byte("same bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := walkJob(t, root, "touched.bin")
	res := hashViaPool(t, job, PoolOptions{})
	if res.Err != nil {
		t.Fatalf("hash: %v", res.Err)
	}

	// A 2017 timestamp can never equal the fixture's creation time.
	stamp := time.Unix(1_500_000_000, 0)
	if err := os.Chtimes(abs, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	c := ClassifyPath(res, nil, fsmeta.ModeFull)
	if c.Status != StatusChangedDuringScan {
		t.Fatalf("Status = %q, want %q (mtime changed after the hash)", c.Status, StatusChangedDuringScan)
	}
	if c.Fields != fsmeta.DiffMtimeNS {
		t.Errorf("Fields = %v, want exactly DiffMtimeNS (size and inode are untouched)", c.Fields)
	}
	if !strings.Contains(c.Detail, "mtime_ns") {
		t.Errorf("Detail = %q, want the mtime_ns field name", c.Detail)
	}
	if strings.Contains(c.Detail, "size") {
		t.Errorf("Detail = %q must not name size: only mtime_ns changed", c.Detail)
	}
}

// TestClassifyDeletedAfterHashIsChangedDuringScan pins the documented
// ENOENT-after-hash decision (failurepaths.go, file doc): a file
// DELETED between hash and classify cannot have its hashed bytes
// re-verified, and the design's four-value status set has no better
// home for it than changed_during_scan - no hash stored, warning
// emitted, excluded from planning until a later scan.
func TestClassifyDeletedAfterHashIsChangedDuringScan(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(root, "vanishing.bin")
	if err := os.WriteFile(abs, []byte("here today"), 0o644); err != nil {
		t.Fatal(err)
	}
	job := walkJob(t, root, "vanishing.bin")
	res := hashViaPool(t, job, PoolOptions{})
	if res.Err != nil {
		t.Fatalf("hash: %v", res.Err)
	}

	if err := os.Remove(abs); err != nil {
		t.Fatalf("remove: %v", err)
	}

	c := ClassifyPath(res, nil, fsmeta.ModeFull)
	if c.Status != StatusChangedDuringScan {
		t.Fatalf("Status = %q, want %q (deletion after hash is the extreme change)",
			c.Status, StatusChangedDuringScan)
	}
	if !strings.Contains(c.Detail, "deleted during scan") {
		t.Errorf("Detail = %q, want the deletion phrasing", c.Detail)
	}
	if !strings.Contains(c.Detail, "no such file or directory") {
		t.Errorf("Detail = %q, want the ENOENT errno text", c.Detail)
	}
}

// TestClassifyLstatFailureAfterHashIsConservative: an lstat failure
// OTHER than ENOENT (here EACCES through an injected source) also
// degrades to changed_during_scan with the errno text - stability
// cannot be confirmed, so no hash is stored. The source is consulted
// with the result's AbsPath (the seam contract).
func TestClassifyLstatFailureAfterHashIsConservative(t *testing.T) {
	res := HashResult{
		Job:   Job{RelPath: "sub/e.bin", AbsPath: "/x/sub/e.bin", Meta: fsmeta.Meta{Size: 7}},
		Bytes: 7,
	}
	var seen []string
	src := metaSourceReturning(fsmeta.Meta{},
		&os.PathError{Op: "lstat", Path: "/x/sub/e.bin", Err: syscall.EACCES}, &seen)

	c := ClassifyPath(res, src, fsmeta.ModeFull)
	if c.Status != StatusChangedDuringScan {
		t.Errorf("Status = %q, want %q", c.Status, StatusChangedDuringScan)
	}
	if !strings.Contains(c.Detail, "cannot re-verify after hashing") ||
		!strings.Contains(c.Detail, "permission denied") {
		t.Errorf("Detail = %q, want 'cannot re-verify after hashing: permission denied'", c.Detail)
	}
	if !reflect.DeepEqual(seen, []string{"/x/sub/e.bin"}) {
		t.Errorf("MetaSource consulted with %v, want the result's AbsPath", seen)
	}
}

// TestClassifyPathUsesInjectedMetaSource pins the test seam: an
// injected source returning the unchanged baseline yields ok, and the
// source receives the AbsPath exactly once.
func TestClassifyPathUsesInjectedMetaSource(t *testing.T) {
	base := fsmeta.Meta{Size: 42, MtimeNS: 1234, Dev: 7, Inode: 99}
	res := HashResult{Job: Job{RelPath: "f.bin", AbsPath: "/x/f.bin", Meta: base}, Bytes: 42}
	var seen []string

	c := ClassifyPath(res, metaSourceReturning(base, nil, &seen), fsmeta.ModeFull)
	if c.Status != StatusOK {
		t.Errorf("Status = %q, want %q (fresh meta equals the baseline)", c.Status, StatusOK)
	}
	if !reflect.DeepEqual(seen, []string{"/x/f.bin"}) {
		t.Errorf("MetaSource consulted with %v, want [/x/f.bin]", seen)
	}
}

// --- warning emission ---------------------------------------------------------

// TestFailureWarningsReachBus: both failure classes emit progress.Warn
// events (unthrottled, DESIGN_SPEC 9.3) rendered by the real plain
// renderer with path AND reason content; ok results emit no warning.
func TestFailureWarningsReachBus(t *testing.T) {
	bus := progress.NewBus()
	var buf bytes.Buffer
	r := progress.New(progress.Options{
		Mode:   progress.ModePlain,
		Stderr: &buf,
		Now:    func() time.Time { return time.Unix(0, 0) },
	})
	done := make(chan progress.Done, 1)
	go func() { done <- r.Run(bus) }()

	f := NewFailures(bus, fsmeta.Mode{})
	// Unreadable, through Process with the error-precedence path.
	f.Process(HashResult{
		Job: Job{RelPath: "var/log/app.log", AbsPath: "/x/var/log/app.log"},
		Err: &os.PathError{Op: "open", Path: "/x/var/log/app.log", Err: syscall.EACCES},
	}, func(string) (fsmeta.Meta, error) {
		t.Error("source consulted for an unreadable result")
		return fsmeta.Meta{}, nil
	})
	// Changed, through Process with an injected differing baseline.
	f.Process(HashResult{
		Job:   Job{RelPath: "src/main.go", AbsPath: "/x/src/main.go", Meta: fsmeta.Meta{Size: 10}},
		Bytes: 10,
	}, metaSourceReturning(fsmeta.Meta{Size: 11}, nil, nil))
	// Ok: classified, never accumulated, never warned.
	f.Process(HashResult{
		Job:   Job{RelPath: "docs/readme.txt", AbsPath: "/x/docs/readme.txt", Meta: fsmeta.Meta{Size: 5}},
		Bytes: 5,
	}, metaSourceReturning(fsmeta.Meta{Size: 5}, nil, nil))

	bus.Finish(progress.Done{Summary: "scan complete"})
	<-done

	out := buf.String()
	if !strings.Contains(out, "warning: unreadable file var/log/app.log: permission denied") {
		t.Errorf("output %q lacks the unreadable warning with path and errno reason", out)
	}
	if !strings.Contains(out, "warning: file src/main.go: fields size changed during scan") {
		t.Errorf("output %q lacks the changed-during-scan warning with path and fields reason", out)
	}
	if strings.Contains(out, "docs/readme.txt") {
		t.Errorf("output %q warns about an ok file; only failures emit warnings", out)
	}
	if n := strings.Count(out, "warning: "); n != 2 {
		t.Errorf("rendered %d warnings, want exactly 2 (one per failure class entry)", n)
	}
}

// TestFailureNilBusSafe: a nil Bus silences emission and nothing else
// changes - classification, accumulation, and the exit carrier all
// behave identically.
func TestFailureNilBusSafe(t *testing.T) {
	f := NewFailures(nil, fsmeta.Mode{}) // nil bus: production default for tests and --quiet internals
	c := f.Process(HashResult{
		Job: Job{RelPath: "a.bin", AbsPath: "/x/a.bin"},
		Err: &os.PathError{Op: "open", Path: "/x/a.bin", Err: syscall.EIO},
	}, nil)
	if c.Status != StatusUnreadable {
		t.Errorf("Status = %q, want %q", c.Status, StatusUnreadable)
	}
	if f.Len() != 1 {
		t.Errorf("Len = %d, want 1 (nil bus must not change accumulation)", f.Len())
	}
}

// --- accumulator and exit-10 carrier -------------------------------------------

// TestFailuresAccumulatorCountsAndExitCarrier: multiple failures across
// both classes aggregate with counts by class and per-file lines; the
// exit carrier is nil when empty (exit 0 preserved) and a
// report.ItemFailures enumerating every failure otherwise (exit 10).
func TestFailuresAccumulatorCountsAndExitCarrier(t *testing.T) {
	f := NewFailures(nil, fsmeta.Mode{})

	// Empty accumulator: nil carrier, exit 0 preserved.
	if err := f.Err(); err != nil {
		t.Fatalf("empty accumulator Err() = %v, want nil (clean runs keep exit 0)", err)
	}
	if got := report.Classify(nil); got != report.ExitOK {
		t.Errorf("report.Classify(nil) = %d, want %d", got, report.ExitOK)
	}

	mk := func(status, rel, detail string) ClassifiedResult {
		return ClassifiedResult{
			HashResult: HashResult{Job: Job{RelPath: rel, AbsPath: "/x/" + rel}},
			Status:     status,
			Detail:     detail,
		}
	}
	f.Add(mk(StatusUnreadable, "a.bin", "permission denied"))
	f.Add(mk(StatusChangedDuringScan, "b.bin", "fields size changed during scan"))
	f.Add(mk(StatusUnreadable, "c.bin", "input/output error"))
	f.Add(mk(StatusOK, "d.bin", "")) // ok is never a failure

	if f.Len() != 3 {
		t.Errorf("Len = %d, want 3 (ok entries are ignored)", f.Len())
	}
	rep := f.Report()
	if rep.Total != 3 || rep.Unreadable != 2 || rep.ChangedDuringScan != 1 {
		t.Errorf("Report counts = total %d, unreadable %d, changed %d; want 3/2/1",
			rep.Total, rep.Unreadable, rep.ChangedDuringScan)
	}
	wantLines := []string{
		"unreadable a.bin: permission denied",
		"changed_during_scan b.bin: fields size changed during scan",
		"unreadable c.bin: input/output error",
	}
	if !reflect.DeepEqual(rep.Lines, wantLines) {
		t.Errorf("Report.Lines = %v, want %v (call order, one line per failure)", rep.Lines, wantLines)
	}

	entries := f.Entries()
	if len(entries) != 3 || entries[0].RelPath != "a.bin" || entries[2].Status != StatusUnreadable {
		t.Errorf("Entries() = %+v, want the three failures in call order", entries)
	}
	// Entries returns a COPY: T1.6's run_events loop cannot corrupt the
	// accumulator by mutating its slice.
	entries[0].RelPath = "mutated"
	if f.Entries()[0].RelPath != "a.bin" {
		t.Error("Entries() aliased the internal slice; it must return a copy")
	}

	err := f.Err()
	if err == nil {
		t.Fatal("Err() = nil with 3 failures, want the exit-10 carrier")
	}
	if len(err.Failures) != 3 {
		t.Errorf("ItemFailures enumerates %d failures, want 3", len(err.Failures))
	}
	for i, ferr := range err.Failures {
		if ferr.Error() != wantLines[i] {
			t.Errorf("failure %d message = %q, want %q (one enumerated error per failure)",
				i, ferr.Error(), wantLines[i])
		}
	}
	if got := report.Classify(err); got != report.ExitItemFailures {
		t.Errorf("report.Classify(ItemFailures) = %d, want %d (exit 10, DESIGN_SPEC 10.2 row 7)",
			got, report.ExitItemFailures)
	}
}

// TestFailuresClassificationIsPurePerFile pins the run-continues
// semantics (FR-003): classification is a pure function of its inputs,
// so classifying one file can never affect another's outcome, and
// re-classifying an identical input yields an identical result.
func TestFailuresClassificationIsPurePerFile(t *testing.T) {
	resA := HashResult{
		Job:    Job{RelPath: "a.bin", AbsPath: "/x/a.bin", Meta: fsmeta.Meta{Size: 10, Inode: 1}},
		Digest: [32]byte{1},
		Bytes:  10,
	}
	mutatedA := fsmeta.Meta{Size: 11, Inode: 1} // differs: size
	resB := HashResult{
		Job: Job{RelPath: "b.bin", AbsPath: "/x/b.bin", Meta: fsmeta.Meta{Size: 3, Inode: 2}},
		Err: &os.PathError{Op: "open", Path: "/x/b.bin", Err: syscall.EACCES},
	}

	first := ClassifyPath(resA, metaSourceReturning(mutatedA, nil, nil), fsmeta.ModeFull)
	_ = ClassifyPath(resB, metaSourceReturning(fsmeta.Meta{}, nil, nil), fsmeta.ModeFull) // interleaved foreign failure
	second := ClassifyPath(resA, metaSourceReturning(mutatedA, nil, nil), fsmeta.ModeFull)

	if !reflect.DeepEqual(first, second) {
		t.Errorf("re-classification diverged: %+v vs %+v (classification must be pure)", first, second)
	}
	if first.Status != StatusChangedDuringScan {
		t.Errorf("a.bin Status = %q, want %q", first.Status, StatusChangedDuringScan)
	}
	if b := ClassifyPath(resB, nil, fsmeta.ModeFull); b.Status != StatusUnreadable || !strings.Contains(b.Detail, "permission denied") {
		t.Errorf("b.bin = %+v, want unreadable with the errno text (unaffected by a.bin)", b)
	}
}

// --- boundaries: directories, symlinks, specials ------------------------------

// TestUnreadableDirectoryAndSpecialsNeverReachFailurePaths verifies the
// no-double-report boundary against T1.3: the walker warns once for the
// unreadable directory (subtree skipped) and once for the FIFO, while
// the file-failure layer sees ONLY regular-file results - the blocked
// subtree's file, the symlink, and the FIFO produce no HashResults and
// therefore no failure rows or duplicate warnings (DESIGN_SPEC 3.4,
// FR-003; walker.go is read-and-pinned here, not modified).
func TestUnreadableDirectoryAndSpecialsNeverReachFailurePaths(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; chmod 000 does not deny directory reads")
	}
	root := t.TempDir()
	for _, d := range []string{"keep", "blocked"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "keep/good.txt"), []byte("good"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "blocked/hidden.txt"), []byte("hidden"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "denied.txt"), []byte("denied"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("keep/good.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(root, "special.fifo"), 0o644); err != nil {
		if errors.Is(err, syscall.ENOSYS) || errors.Is(err, syscall.EOPNOTSUPP) || errors.Is(err, syscall.EPERM) {
			t.Skipf("mkfifo unsupported in this environment: %v", err)
		}
		t.Fatalf("mkfifo: %v", err)
	}
	for _, p := range []string{"blocked", "denied.txt"} {
		if err := os.Chmod(filepath.Join(root, p), 0o000); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		os.Chmod(filepath.Join(root, "blocked"), 0o755)
		os.Chmod(filepath.Join(root, "denied.txt"), 0o644)
	})

	var jobs []Job
	wres, err := Walk(root, Matcher{}, collectJobs(&jobs), nil)
	if err != nil {
		t.Fatalf("Walk: %v (unreadable subtree must not abort)", err)
	}

	// Exactly the two regular files became jobs; the blocked subtree's
	// file, the symlink, and the FIFO never do.
	if got := jobRelPaths(jobs); !reflect.DeepEqual(got, []string{"denied.txt", "keep/good.txt"}) {
		t.Fatalf("job relpaths = %v, want [denied.txt keep/good.txt]", got)
	}

	// Walker warnings: exactly the unreadable directory and the FIFO,
	// each once - the directory warning belongs to T1.3 alone.
	if len(wres.Warnings) != 2 {
		t.Errorf("walk warnings = %v, want exactly 2 (unreadable dir + FIFO)", wres.Warnings)
	}
	dirWarns := 0
	for _, w := range wres.Warnings {
		if strings.Contains(w, "cannot read directory") && strings.Contains(w, "blocked") {
			dirWarns++
		}
	}
	if dirWarns != 1 {
		t.Errorf("got %d 'cannot read directory ... blocked' warnings, want exactly 1 (T1.3 owns it)", dirWarns)
	}

	// The file-failure layer sees only the two jobs: denied.txt fails
	// (unreadable), keep/good.txt passes (ok); nothing else ever
	// reaches it.
	failures := NewFailures(nil, fsmeta.Mode{})
	pool := NewHashPool(context.Background(), PoolOptions{Workers: 2})
	for _, j := range jobs {
		if err := pool.Submit(j); err != nil {
			t.Fatalf("Submit(%s): %v", j.RelPath, err)
		}
	}
	pool.Close()
	got := resultsByRel(drainResults(pool.Results()))
	if err := pool.Wait(); err != nil {
		t.Fatalf("pool Wait: %v", err)
	}
	statuses := map[string]string{}
	for rel, res := range got {
		statuses[rel] = failures.Process(res, nil).Status
	}

	if statuses["keep/good.txt"] != StatusOK {
		t.Errorf("keep/good.txt = %q, want ok", statuses["keep/good.txt"])
	}
	if statuses["denied.txt"] != StatusUnreadable {
		t.Errorf("denied.txt = %q, want unreadable", statuses["denied.txt"])
	}
	if len(statuses) != 2 {
		t.Errorf("classified %d files, want exactly the 2 jobs (no dir/symlink/FIFO rows)", len(statuses))
	}
	if failures.Len() != 1 {
		t.Errorf("Failures.Len() = %d, want 1 (denied.txt only; directories are T1.3's)", failures.Len())
	}
	for _, e := range failures.Entries() {
		if e.RelPath != "denied.txt" {
			t.Errorf("unexpected failure entry for %q (dirs/symlinks/specials must produce no rows)", e.RelPath)
		}
	}
}

// --- the real pipeline -----------------------------------------------------------

// TestIngestPipelineClassifiesFixtureEndToEnd runs the full T1.3+T1.4+
// T1.5 pipeline - Walk feeding the pool, a consumer classifying every
// drained result through Failures.Process - over a fixture with one
// good file, one chmod-000 file (skipped as root), and one file
// mutated mid-hash through the OpenFile seam (deterministic: the pool
// closes the file before delivering its result). All three statuses
// must come out right, the good digest must be intact, and the run's
// exit carrier must be the exit-10 ItemFailures.
func TestIngestPipelineClassifiesFixtureEndToEnd(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root; chmod 000 does not deny open")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "good.txt"), []byte("good content"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "denied.txt"), []byte("denied"), 0o644); err != nil {
		t.Fatal(err)
	}
	mutateAbs := filepath.Join(root, "mutate.bin")
	if err := os.WriteFile(mutateAbs, []byte("before-mutation"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "denied.txt"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(root, "denied.txt"), 0o644) })

	// OpenFile seam: real open, except mutate.bin whose Close appends
	// bytes AFTER the hash stream finished (see closingMutator).
	open := func(path string) (io.ReadCloser, error) {
		rc, err := osOpen(path)
		if err != nil {
			return nil, err
		}
		if filepath.Base(path) != "mutate.bin" {
			return rc, nil
		}
		return &closingMutator{
			ReadCloser: rc,
			path:       path,
			after: func() error {
				f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
				if err != nil {
					return err
				}
				if _, err := f.Write([]byte("-MUTATED")); err != nil {
					f.Close()
					return err
				}
				return f.Close()
			},
		}, nil
	}

	failures := NewFailures(nil, fsmeta.Mode{})
	pool := NewHashPool(context.Background(), PoolOptions{Workers: 3, OpenFile: open})

	var mu sync.Mutex
	got := map[string]ClassifiedResult{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for r := range pool.Results() {
			c := failures.Process(r, nil) // processing-time lstat, per D-10 point 1
			mu.Lock()
			got[c.RelPath] = c
			mu.Unlock()
		}
	}()

	if _, err := Walk(root, Matcher{}, pool.Submit, nil); err != nil {
		t.Fatalf("Walk: %v", err)
	}
	pool.Close()
	wg.Wait()
	if err := pool.Wait(); err != nil {
		t.Fatalf("pool Wait: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("classified %d files, want 3 (%v)", len(got), got)
	}

	good := got["good.txt"]
	if good.Status != StatusOK {
		t.Errorf("good.txt = %q, want ok", good.Status)
	}
	if want := sha256.Sum256([]byte("good content")); good.Digest != want {
		t.Errorf("good.txt digest = %x, want %x (digest intact)", good.Digest, want)
	}

	denied := got["denied.txt"]
	if denied.Status != StatusUnreadable {
		t.Errorf("denied.txt = %q, want unreadable", denied.Status)
	}
	if !strings.Contains(denied.Detail, "permission denied") {
		t.Errorf("denied.txt Detail = %q, want the EACCES text", denied.Detail)
	}

	mutated := got["mutate.bin"]
	if mutated.Status != StatusChangedDuringScan {
		t.Errorf("mutate.bin = %q, want changed_during_scan (mutated on close, before classification)",
			mutated.Status)
	}
	if mutated.Fields&fsmeta.DiffSize == 0 {
		t.Errorf("mutate.bin Fields = %v, want DiffSize set (bytes appended)", mutated.Fields)
	}
	if mutated.Digest == ([32]byte{}) {
		t.Error("mutate.bin digest is zero; the pool hashed the pre-mutation bytes " +
			"(the classification, not the pool, drops them)")
	}

	if failures.Len() != 2 {
		t.Errorf("Failures.Len() = %d, want 2 (denied + mutated)", failures.Len())
	}
	err := failures.Err()
	if err == nil {
		t.Fatal("Err() = nil with 2 failures, want the exit-10 carrier")
	}
	if got := report.Classify(err); got != report.ExitItemFailures {
		t.Errorf("report.Classify = %d, want %d (exit 10)", got, report.ExitItemFailures)
	}
	rep := failures.Report()
	if rep.Total != 2 || rep.Unreadable != 1 || rep.ChangedDuringScan != 1 {
		t.Errorf("Report = %+v, want total 2, unreadable 1, changed 1", rep)
	}
}
