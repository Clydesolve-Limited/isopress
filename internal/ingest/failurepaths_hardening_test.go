package ingest

// T1.5 hardening tests: coverage that pins the failure-path behaviour
// the base suite (failurepaths_test.go) leaves uncovered:
//
//   - errnoText extraction MATRIX: EACCES, EIO, ESTALE, and ENOENT
//     through *os.PathError, a PLAIN syscall.Errno with no wrapper, the
//     real T1.4 pool shape (one %w layer), a two-deep %w chain
//     (errors.As must still find the errno), a PathError whose Err is
//     itself wrapped, the honest no-errno fallback, and nil.
//   - ESTALE (NFS staleness, named by DESIGN_SPEC 3.4) cannot be
//     provoked deterministically on a local filesystem, so it is
//     asserted where it honestly can be: a synthetic *os.PathError
//     wrapping syscall.ESTALE driven through the REAL hash pool via the
//     OpenFile seam (classification, detail, exit carrier) and through
//     the injected MetaSource seam at the lstat re-verification point.
//   - CatalogStatus mapping exhaustiveness plus the DEFENSIVE default:
//     an unknown Status string passes through verbatim (so the catalog
//     CHECK constraint of DESIGN_SPEC 2.3 rejects it) and is never
//     silently coerced into a valid-but-wrong value.
//   - WarnMsg and Line EXACT formats, frozen so T1.6's run_events rows
//     and end-of-run report lines cannot drift silently.
//   - Failures.Process CONCURRENCY: the documented mutex-guarded claim
//     raced from N goroutines against concurrent Report/Entries/Len/Err
//     readers (the -race detector is the primary oracle here).
//   - Failures over a CANCELLED T1.4 pool: a partial result stream
//     processed then aggregated (no hang, no nil deref, nil exit
//     carrier when the partial stream carries no failures, exit-10
//     carrier when it carries one), and the empty stream after an
//     immediate cancel.
//   - The e2e digest-correctness strengthening: the mutated-mid-hash
//     file's digest is EXACTLY sha256 of the pre-mutation bytes (the
//     base e2e test only pins "non-zero"; this pins the value), which
//     is what proves the classification - not the pool - is what keeps
//     stale bytes out of the catalog.
//
// Stdlib only; fixtures in t.TempDir(); no sleeps - cancellation is
// coordinated through the injected OpenFile seam (parked worker, then
// cancel, then release) and every potentially-hanging drain runs under
// a watchdog.

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"isopress/internal/fsmeta"
	"isopress/internal/report"
)

// --- errnoText extraction matrix ---------------------------------------------

// TestErrnoTextExtractionMatrix pins the errno extraction layer
// (failurepaths.go errnoText) across every error shape the pipeline can
// produce or hand in: real syscalls, the T1.4 pool's one-layer wrap, a
// two-deep %w chain, a PathError wrapping a wrap, a plain bare Errno,
// an error with no errno at all (honest full-string fallback), and nil.
func TestErrnoTextExtractionMatrix(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "EACCES PathError",
			err:  &os.PathError{Op: "open", Path: "/x/a.bin", Err: syscall.EACCES},
			want: "permission denied",
		},
		{
			name: "EIO PathError",
			err:  &os.PathError{Op: "read", Path: "/x/a.bin", Err: syscall.EIO},
			want: "input/output error",
		},
		{
			name: "ESTALE PathError (NFS staleness, DESIGN_SPEC 3.4)",
			err:  &os.PathError{Op: "read", Path: "/mnt/nas/a.bin", Err: syscall.ESTALE},
			want: "stale file handle",
		},
		{
			name: "ENOENT PathError",
			err:  &os.PathError{Op: "lstat", Path: "/x/a.bin", Err: syscall.ENOENT},
			want: "no such file or directory",
		},
		{
			name: "plain syscall.Errno with no wrapper",
			err:  syscall.EIO,
			want: "input/output error",
		},
		{
			// The exact T1.4 pool shape: fmt.Errorf("ingest: open
			// %s: %w", rel, *os.PathError) - one %w layer.
			name: "pool shape: one wrap around a PathError",
			err:  fmt.Errorf("ingest: open %s: %w", "a.bin", &os.PathError{Op: "open", Path: "/x/a.bin", Err: syscall.EACCES}),
			want: "permission denied",
		},
		{
			// Two %w layers deep: errors.As must traverse the whole
			// chain to find the PathError/errno.
			name: "two-deep %w chain still yields the errno",
			err: fmt.Errorf("outer: %w",
				fmt.Errorf("middle: %w",
					&os.PathError{Op: "read", Path: "/x/a.bin", Err: syscall.ESTALE})),
			want: "stale file handle",
		},
		{
			// A PathError whose Err is itself a wrap of the errno:
			// the inner errors.As inside the PathError branch must
			// traverse too.
			name: "PathError wrapping a wrapped errno",
			err:  &os.PathError{Op: "read", Path: "/x/a.bin", Err: fmt.Errorf("inner: %w", syscall.EIO)},
			want: "input/output error",
		},
		{
			// No errno anywhere: the honest full-string fallback, not
			// a made-up or empty detail.
			name: "error with no errno falls back to the full string",
			err:  errors.New("ingest: open a.bin: something completely custom"),
			want: "ingest: open a.bin: something completely custom",
		},
		{
			name: "nil error yields empty detail",
			err:  nil,
			want: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := errnoText(tc.err); got != tc.want {
				t.Errorf("errnoText(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestErrnoTextESTALEClassifiesUnreadableThroughPool drives a synthetic
// ESTALE open failure through the REAL T1.4 pool (OpenFile seam) and
// the full T1.5 classification. ESTALE cannot be provoked
// deterministically on a local filesystem - it is an NFS staleness
// errno - so this is the honest port of the DESIGN_SPEC 3.4 "EACCES,
// EIO, ESTALE on NAS" acceptance shape: real pool, real classification,
// synthetic errno. The record must come out unreadable with the errno
// text in Detail, no hash, and the run's exit carrier must be exit 10.
func TestErrnoTextESTALEClassifiesUnreadableThroughPool(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "stale.bin"), []byte("nas bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	open := func(path string) (io.ReadCloser, error) {
		return nil, &os.PathError{Op: "open", Path: path, Err: syscall.ESTALE}
	}

	res := hashViaPool(t, walkJob(t, root, "stale.bin"), PoolOptions{OpenFile: open})
	if !errors.Is(res.Err, syscall.ESTALE) {
		t.Fatalf("HashResult.Err = %v, want the injected ESTALE", res.Err)
	}
	if res.Digest != ([32]byte{}) {
		t.Errorf("Digest = %x, want zero on a failed open", res.Digest)
	}

	failures := NewFailures(nil, fsmeta.Mode{})
	c := failures.Process(res, func(string) (fsmeta.Meta, error) {
		t.Error("MetaSource consulted for an ESTALE-failed result; the read error takes precedence")
		return fsmeta.Meta{}, nil
	})
	if c.Status != StatusUnreadable {
		t.Errorf("Status = %q, want %q", c.Status, StatusUnreadable)
	}
	if c.Detail != "stale file handle" {
		t.Errorf("Detail = %q, want the ESTALE errno text %q (the catalog status_detail value)", c.Detail, "stale file handle")
	}
	if c.CatalogStatus() != "unreadable" {
		t.Errorf("CatalogStatus = %q, want 'unreadable'", c.CatalogStatus())
	}
	if !strings.Contains(c.WarnMsg(), "stale.bin") || !strings.Contains(c.WarnMsg(), "stale file handle") {
		t.Errorf("WarnMsg = %q, want path AND reason content", c.WarnMsg())
	}
	err := failures.Err()
	if err == nil {
		t.Fatal("Err() = nil with an ESTALE failure, want the exit-10 carrier")
	}
	if got := report.Classify(err); got != report.ExitItemFailures {
		t.Errorf("report.Classify = %d, want %d (exit 10)", got, report.ExitItemFailures)
	}
}

// TestClassifyPathLstatErrnoMatrixAfterHash pins the post-hash
// lstat-failure branch of ClassifyPath for the full errno matrix:
// ENOENT is the documented deletion case, every other errno (EACCES,
// ESTALE) takes the conservative cannot-re-verify path, and an error
// with no errno at all still yields an honest detail. All are
// changed_during_scan with no hash stored (DESIGN_SPEC 3.5 point 1, D-10
// documented decision in the file doc).
func TestClassifyPathLstatErrnoMatrixAfterHash(t *testing.T) {
	res := HashResult{
		Job:    Job{RelPath: "sub/f.bin", AbsPath: "/x/sub/f.bin", Meta: fsmeta.Meta{Size: 7}},
		Bytes:  7,
		Digest: [32]byte{9},
	}
	cases := []struct {
		name       string
		lstatErr   error
		wantDetail string
	}{
		{
			name:       "ENOENT is the documented deletion case",
			lstatErr:   &os.PathError{Op: "lstat", Path: "/x/sub/f.bin", Err: syscall.ENOENT},
			wantDetail: "deleted during scan: no such file or directory",
		},
		{
			name:       "EACCES takes the conservative path",
			lstatErr:   &os.PathError{Op: "lstat", Path: "/x/sub/f.bin", Err: syscall.EACCES},
			wantDetail: "cannot re-verify after hashing: permission denied",
		},
		{
			name:       "ESTALE takes the conservative path with the errno text",
			lstatErr:   &os.PathError{Op: "lstat", Path: "/x/sub/f.bin", Err: syscall.ESTALE},
			wantDetail: "cannot re-verify after hashing: stale file handle",
		},
		{
			// fsmeta.PathMeta wraps the lstat error in one %w layer;
			// the classification must traverse to the errno.
			name:       "wrapped lstat error (fsmeta.PathMeta shape)",
			lstatErr:   fmt.Errorf("fsmeta: lstat %q: %w", "/x/sub/f.bin", &os.PathError{Op: "lstat", Path: "/x/sub/f.bin", Err: syscall.EIO}),
			wantDetail: "cannot re-verify after hashing: input/output error",
		},
		{
			name:       "no-errno error falls back to the full string",
			lstatErr:   errors.New("something custom"),
			wantDetail: "cannot re-verify after hashing: something custom",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := func(path string) (fsmeta.Meta, error) {
				if path != "/x/sub/f.bin" {
					t.Errorf("MetaSource consulted with %q, want the result's AbsPath", path)
				}
				return fsmeta.Meta{}, tc.lstatErr
			}
			c := ClassifyPath(res, src, fsmeta.ModeFull)
			if c.Status != StatusChangedDuringScan {
				t.Errorf("Status = %q, want %q (stability unconfirmable)", c.Status, StatusChangedDuringScan)
			}
			if c.Detail != tc.wantDetail {
				t.Errorf("Detail = %q, want %q", c.Detail, tc.wantDetail)
			}
			if c.CatalogStatus() != "changed_during_scan" {
				t.Errorf("CatalogStatus = %q, want 'changed_during_scan'", c.CatalogStatus())
			}
			if !strings.Contains(c.WarnMsg(), "sub/f.bin") {
				t.Errorf("WarnMsg = %q, want the relpath content", c.WarnMsg())
			}
		})
	}
}

// --- CatalogStatus mapping exhaustiveness and defensive default --------------

// TestCatalogStatusMappingExhaustiveAndDefensive pins every mapping of
// ClassifiedResult.Status to the catalog files.status CHECK vocabulary
// of DESIGN_SPEC 2.3. The three classification outcomes map
// deterministically; 'missing' (a T1.6 reconciler outcome that never
// originates in classification) passes through verbatim; and an UNKNOWN
// status also passes through verbatim - the documented defensive
// default: a value the catalog CHECK constraint rejects loudly beats a
// silent coercion to a valid-but-wrong status.
func TestCatalogStatusMappingExhaustiveAndDefensive(t *testing.T) {
	cases := []struct {
		name   string
		status string
		want   string
	}{
		{"ok stores as active", StatusOK, "active"},
		{"unreadable stores verbatim", StatusUnreadable, "unreadable"},
		{"changed_during_scan stores verbatim", StatusChangedDuringScan, "changed_during_scan"},
		{"reconciler-owned missing passes through verbatim", "missing", "missing"},
		{
			// Defensive default: never silently map an unknown
			// status onto a valid CHECK value; let the catalog
			// reject it instead of storing a wrong-but-legal row.
			name:   "unknown status passes through verbatim (CHECK rejects it)",
			status: "bogus_status",
			want:   "bogus_status",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := ClassifiedResult{Status: tc.status}
			if got := c.CatalogStatus(); got != tc.want {
				t.Errorf("CatalogStatus(%q) = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}

// TestFailuresAddUnknownStatusCountsTotalOnly pins the accumulator's
// defensive behaviour for an unknown status: Add records it (it is not
// ok), Report().Total counts it, but neither per-class bucket does, and
// the exit carrier still enumerates it. (Classify can never PRODUCE an
// unknown status; this pins the guard for a miswired caller.)
func TestFailuresAddUnknownStatusCountsTotalOnly(t *testing.T) {
	f := NewFailures(nil, fsmeta.Mode{})
	f.Add(ClassifiedResult{
		HashResult: HashResult{Job: Job{RelPath: "weird.bin", AbsPath: "/x/weird.bin"}},
		Status:     "bogus_status",
		Detail:     "unexpected",
	})

	if f.Len() != 1 {
		t.Errorf("Len = %d, want 1 (non-ok statuses accumulate)", f.Len())
	}
	rep := f.Report()
	if rep.Total != 1 {
		t.Errorf("Report.Total = %d, want 1", rep.Total)
	}
	if rep.Unreadable != 0 || rep.ChangedDuringScan != 0 {
		t.Errorf("Report class counts = %d/%d, want 0/0 (unknown status belongs to no class bucket)",
			rep.Unreadable, rep.ChangedDuringScan)
	}
	wantLine := "bogus_status weird.bin: unexpected"
	if len(rep.Lines) != 1 || rep.Lines[0] != wantLine {
		t.Errorf("Report.Lines = %v, want [%q]", rep.Lines, wantLine)
	}
	err := f.Err()
	if err == nil || len(err.Failures) != 1 || err.Failures[0].Error() != wantLine {
		t.Fatalf("Err() = %v, want the one enumerated unknown-status failure", err)
	}
}

// --- format freezes: WarnMsg and Line -----------------------------------------

// TestWarnMsgAndLineFormatsFrozen freezes the EXACT renderings T1.6
// turns into run_events warning rows (WarnMsg) and end-of-run report
// lines / exit-carrier errors (Line). These strings are a cross-task
// contract: a wording change here silently changes catalog-adjacent
// output, so it must fail a test rather than drift.
func TestWarnMsgAndLineFormatsFrozen(t *testing.T) {
	unreadable := ClassifiedResult{
		HashResult: HashResult{Job: Job{RelPath: "var/log/app.log", AbsPath: "/x/var/log/app.log"}},
		Status:     StatusUnreadable,
		Detail:     "permission denied",
	}
	changed := ClassifiedResult{
		HashResult: HashResult{Job: Job{RelPath: "src/main.go", AbsPath: "/x/src/main.go"}},
		Status:     StatusChangedDuringScan,
		Detail:     "fields size mtime_ns changed during scan",
	}
	ok := ClassifiedResult{
		HashResult: HashResult{Job: Job{RelPath: "docs/readme.txt", AbsPath: "/x/docs/readme.txt"}},
		Status:     StatusOK,
	}
	unknown := ClassifiedResult{
		HashResult: HashResult{Job: Job{RelPath: "x.bin", AbsPath: "/x/x.bin"}},
		Status:     "bogus_status",
		Detail:     "unexpected",
	}

	// WarnMsg: the run_events / progress.Warn rendering.
	if got, want := unreadable.WarnMsg(),
		"unreadable file var/log/app.log: permission denied (no hash recorded)"; got != want {
		t.Errorf("unreadable WarnMsg = %q, want %q", got, want)
	}
	if got, want := changed.WarnMsg(),
		"file src/main.go: fields size mtime_ns changed during scan (no hash recorded; excluded from planning until a later stable scan)"; got != want {
		t.Errorf("changed WarnMsg = %q, want %q", got, want)
	}
	if got := ok.WarnMsg(); got != "" {
		t.Errorf("ok WarnMsg = %q, want empty (no warning for a clean file)", got)
	}
	if got := unknown.WarnMsg(); got != "" {
		t.Errorf("unknown-status WarnMsg = %q, want empty (default branch)", got)
	}

	// Line: the end-of-run report / exit-carrier rendering.
	if got, want := unreadable.Line(), "unreadable var/log/app.log: permission denied"; got != want {
		t.Errorf("unreadable Line = %q, want %q", got, want)
	}
	if got, want := changed.Line(), "changed_during_scan src/main.go: fields size mtime_ns changed during scan"; got != want {
		t.Errorf("changed Line = %q, want %q", got, want)
	}
	if got, want := unknown.Line(), "bogus_status x.bin: unexpected"; got != want {
		t.Errorf("unknown-status Line = %q, want %q", got, want)
	}
}

// --- Failures.Process concurrency (the mutex-guarded claim) -------------------

// TestFailuresProcessConcurrentRace races N goroutines of Process calls
// (all three classes, no filesystem: injected MetaSource and
// error-precedence inputs) against a concurrent reader hammering the
// guarded accessors Report/Entries/Len/Err. The primary oracle is the
// -race detector; the secondary assertion is exact final counts -
// every result must land exactly once, no matter the interleaving.
func TestFailuresProcessConcurrentRace(t *testing.T) {
	const (
		goroutines   = 8
		perGoroutine = 300 // divisible by 3: 100 per class per goroutine
	)
	f := NewFailures(nil, fsmeta.Mode{})
	base := fsmeta.Meta{Size: 10, MtimeNS: 111, Dev: 22, Inode: 33}
	changed := fsmeta.Meta{Size: 11, MtimeNS: 111, Dev: 22, Inode: 33} // size delta

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				rel := fmt.Sprintf("g%d/f%d.bin", g, i)
				switch i % 3 {
				case 0: // ok: fresh meta equals the baseline
					res := HashResult{
						Job:    Job{RelPath: rel, AbsPath: "/x/" + rel, Meta: base},
						Digest: [32]byte{1},
						Bytes:  10,
					}
					c := f.Process(res, metaSourceReturning(base, nil, nil))
					if c.Status != StatusOK {
						t.Errorf("%s: Status = %q, want ok", rel, c.Status)
					}
				case 1: // unreadable: error precedence, src never consulted
					res := HashResult{
						Job: Job{RelPath: rel, AbsPath: "/x/" + rel, Meta: base},
						Err: &os.PathError{Op: "open", Path: "/x/" + rel, Err: syscall.EACCES},
					}
					src := func(string) (fsmeta.Meta, error) {
						t.Errorf("%s: MetaSource consulted for an unreadable result", rel)
						return fsmeta.Meta{}, nil
					}
					c := f.Process(res, src)
					if c.Status != StatusUnreadable {
						t.Errorf("%s: Status = %q, want unreadable", rel, c.Status)
					}
				case 2: // changed: fresh meta differs
					res := HashResult{
						Job:    Job{RelPath: rel, AbsPath: "/x/" + rel, Meta: base},
						Digest: [32]byte{2},
						Bytes:  10,
					}
					c := f.Process(res, metaSourceReturning(changed, nil, nil))
					if c.Status != StatusChangedDuringScan {
						t.Errorf("%s: Status = %q, want changed_during_scan", rel, c.Status)
					}
				}
			}
		}(g)
	}

	// Concurrent reader: bait for any unguarded accessor read.
	readsDone := make(chan struct{})
	go func() {
		defer close(readsDone)
		for i := 0; i < 2000; i++ {
			_ = f.Len()
			_ = f.Entries()
			_ = f.Report()
			_ = f.Err()
		}
	}()

	wg.Wait()
	<-readsDone

	wantPerClass := goroutines * (perGoroutine / 3) // 800 each
	if got := f.Len(); got != 2*wantPerClass {
		t.Errorf("Len = %d, want %d (every failure lands exactly once)", got, 2*wantPerClass)
	}
	rep := f.Report()
	if rep.Total != 2*wantPerClass || rep.Unreadable != wantPerClass || rep.ChangedDuringScan != wantPerClass {
		t.Errorf("Report = total %d, unreadable %d, changed %d; want %d/%d/%d",
			rep.Total, rep.Unreadable, rep.ChangedDuringScan,
			2*wantPerClass, wantPerClass, wantPerClass)
	}
	if len(rep.Lines) != 2*wantPerClass {
		t.Errorf("len(Report.Lines) = %d, want %d (one line per failure)", len(rep.Lines), 2*wantPerClass)
	}
	err := f.Err()
	if err == nil {
		t.Fatal("Err() = nil with failures, want the exit-10 carrier")
	}
	if len(err.Failures) != 2*wantPerClass {
		t.Errorf("ItemFailures enumerates %d failures, want %d", len(err.Failures), 2*wantPerClass)
	}
	if got := report.Classify(err); got != report.ExitItemFailures {
		t.Errorf("report.Classify = %d, want %d (exit 10)", got, report.ExitItemFailures)
	}
}

// --- Failures over a cancelled T1.4 pool ---------------------------------------

// parkOpen parks every open until release is closed (the deterministic
// no-sleep worker-coordination seam), then optionally FAILS the open
// with a synthetic errno - letting a test hold a pool mid-file, cancel
// it, and control exactly what the single delivered result looks like.
type parkOpen struct {
	opened  chan string
	release chan struct{}
	failErr error // when non-nil, the open fails with this errno after release
}

func (o *parkOpen) open(path string) (io.ReadCloser, error) {
	o.opened <- path
	<-o.release
	if o.failErr != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: o.failErr}
	}
	return osOpen(path)
}

// TestFailuresOverCancelledPoolPartialStream pins the T1.4-cancel ->
// T1.5-aggregate interaction: the pool is cancelled while a worker is
// parked mid-open, so exactly ONE in-flight result is delivered and the
// queued jobs are abandoned (documented cancellation policy). Draining
// that partial stream into Failures.Process and then calling Err() must
// complete cleanly - no hang, no nil deref - and the exit carrier must
// reflect exactly the delivered results: nil for a partial stream with
// no failures, exit 10 when the one delivered result is a failure.
//
// POLICY-CHANGE NOTE (PM decision, defect A of 2026-10-19): the
// "partial stream with no failures" subtest previously asserted that
// the in-flight file COMPLETED cleanly after the release. Under the
// prompt-abort policy the released open is followed by hashOne's ctx
// check, so the delivered in-flight result is now ABORTED (ctx-canceled)
// - and the T1.6 wiring drops aborted results BEFORE classification
// (FR-014: an interrupted hash records nothing about the file), which
// the updated subtest mirrors. The EIO subtest is unaffected: its open
// FAILS for a genuine reason, which is a real per-file failure, not an
// abort.
func TestFailuresOverCancelledPoolPartialStream(t *testing.T) {
	newFixture := func(t *testing.T) string {
		t.Helper()
		root := t.TempDir()
		for _, n := range []string{"a.bin", "b.bin", "c.bin"} {
			if err := os.WriteFile(filepath.Join(root, n), []byte("content-"+n), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return root
	}

	// runCancelled drives one job set through a pool cancelled while
	// the single worker is parked inside open(), then drains the
	// partial stream into Failures under a hang watchdog.
	runCancelled := func(t *testing.T, failErr error) (*Failures, []HashResult, error) {
		t.Helper()
		root := newFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		ob := &parkOpen{opened: make(chan string, 8), release: make(chan struct{}), failErr: failErr}

		pool := NewHashPool(ctx, PoolOptions{Workers: 1, OpenFile: ob.open})
		for _, n := range []string{"a.bin", "b.bin", "c.bin"} {
			if err := pool.Submit(walkJob(t, root, n)); err != nil {
				t.Fatalf("Submit(%s): %v", n, err)
			}
		}

		// Wait until the single worker is parked inside a.bin's open.
		select {
		case p := <-ob.opened:
			if filepath.Base(p) != "a.bin" {
				t.Fatalf("first parked open = %q, want a.bin", p)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("worker never entered open; the coordination seam is broken")
		}

		// Cancel BEFORE release: after this file the worker must take
		// no new job, so exactly one result is delivered and b/c are
		// abandoned; the released in-flight file itself ABORTS at its
		// first read boundary (prompt-abort policy, PM decision defect
		// A of 2026-10-19).
		cancel()
		close(ob.release)

		// Drain under a watchdog: the results channel must close.
		drained := make(chan []HashResult, 1)
		go func() { drained <- drainResults(pool.Results()) }()
		var results []HashResult
		select {
		case results = <-drained:
		case <-time.After(30 * time.Second):
			t.Fatal("drain hung after pool cancellation; Results() must close")
		}
		werr := pool.Wait()

		failures := NewFailures(nil, fsmeta.Mode{})
		for _, r := range results {
			if r.Aborted() {
				// Mirror the T1.6 scan wiring: an aborted in-flight
				// hash is dropped BEFORE classification (FR-014: it
				// records nothing about the file).
				continue
			}
			_ = failures.Process(r, nil) // nil src = real lstat; fixtures are unchanged
		}
		return failures, results, werr
	}

	t.Run("partial stream with no failures: nil exit carrier, no hang", func(t *testing.T) {
		failures, results, werr := runCancelled(t, nil)
		if !errors.Is(werr, context.Canceled) {
			t.Errorf("pool.Wait() = %v, want context.Canceled (the cancellation is reported at the join)", werr)
		}
		if len(results) != 1 {
			t.Fatalf("delivered %d results, want exactly 1 (the in-flight file; the queue is abandoned)", len(results))
		}
		if results[0].RelPath != "a.bin" {
			t.Errorf("delivered result = %q, want a.bin (the parked in-flight file)", results[0].RelPath)
		}
		// Prompt-abort policy (PM decision, defect A): the in-flight
		// file ABORTS instead of completing, and the wiring drops it
		// before classification - so a partial stream carries no
		// failures and keeps exit 0.
		if !results[0].Aborted() {
			t.Errorf("delivered result Err = %v, want an aborted ctx-canceled result (prompt-abort policy)", results[0].Err)
		}
		if failures.Len() != 0 {
			t.Errorf("Failures.Len() = %d, want 0 (the aborted in-flight result is dropped, not classified)", failures.Len())
		}
		if err := failures.Err(); err != nil {
			t.Errorf("Err() = %v, want nil (a partial stream with no failures keeps exit 0)", err)
		}
		if got := report.Classify(nil); got != report.ExitOK {
			t.Errorf("report.Classify(nil) = %d, want %d", got, report.ExitOK)
		}
		if rep := failures.Report(); rep.Total != 0 {
			t.Errorf("Report.Total = %d, want 0", rep.Total)
		}
	})

	t.Run("partial stream with one failure: exit-10 carrier", func(t *testing.T) {
		failures, results, werr := runCancelled(t, syscall.EIO)
		if !errors.Is(werr, context.Canceled) {
			t.Errorf("pool.Wait() = %v, want context.Canceled", werr)
		}
		if len(results) != 1 {
			t.Fatalf("delivered %d results, want exactly 1", len(results))
		}
		if results[0].Err == nil || !errors.Is(results[0].Err, syscall.EIO) {
			t.Fatalf("delivered result Err = %v, want the injected EIO", results[0].Err)
		}
		entries := failures.Entries()
		if len(entries) != 1 {
			t.Fatalf("Failures.Len() = %d, want 1 (the one delivered result failed)", len(entries))
		}
		if entries[0].Status != StatusUnreadable || entries[0].Detail != "input/output error" {
			t.Errorf("entry = %+v, want unreadable with the EIO errno text", entries[0])
		}
		err := failures.Err()
		if err == nil {
			t.Fatal("Err() = nil with one failure, want the exit-10 carrier")
		}
		if len(err.Failures) != 1 {
			t.Errorf("ItemFailures enumerates %d failures, want 1", len(err.Failures))
		}
		if got := report.Classify(err); got != report.ExitItemFailures {
			t.Errorf("report.Classify = %d, want %d (exit 10)", got, report.ExitItemFailures)
		}
	})

	t.Run("empty stream after immediate cancel: no hang, nil carrier", func(t *testing.T) {
		root := newFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // cancelled before any submission

		pool := NewHashPool(ctx, PoolOptions{Workers: 1})
		for _, n := range []string{"a.bin", "b.bin", "c.bin"} {
			// Intake aborts on cancellation (documented priority).
			if err := pool.Submit(walkJob(t, root, n)); !errors.Is(err, context.Canceled) {
				t.Fatalf("Submit(%s) = %v, want context.Canceled", n, err)
			}
		}
		pool.Close()

		drained := make(chan []HashResult, 1)
		go func() { drained <- drainResults(pool.Results()) }()
		var results []HashResult
		select {
		case results = <-drained:
		case <-time.After(30 * time.Second):
			t.Fatal("drain hung on an empty cancelled stream; Results() must close")
		}
		if len(results) != 0 {
			t.Fatalf("delivered %d results, want 0 (cancelled before any job was taken)", len(results))
		}
		if werr := pool.Wait(); !errors.Is(werr, context.Canceled) {
			t.Errorf("pool.Wait() = %v, want context.Canceled", werr)
		}

		// Aggregating the empty stream: no hang, no nil deref, clean
		// exit-0 carrier.
		failures := NewFailures(nil, fsmeta.Mode{})
		for _, r := range results {
			_ = failures.Process(r, nil)
		}
		if failures.Len() != 0 {
			t.Errorf("Failures.Len() = %d, want 0", failures.Len())
		}
		if err := failures.Err(); err != nil {
			t.Errorf("Err() = %v, want nil (empty stream keeps exit 0)", err)
		}
	})
}

// --- e2e digest-correctness strengthening --------------------------------------

// TestPipelineMutatedDigestIsPreMutationBytes replicates the base
// e2e's deterministic mid-hash mutation seam (the T1.4 pool closes the
// file inside hashOne BEFORE delivering the result, so the closingMutator
// append is guaranteed visible to the processing-time lstat) and pins
// what the base test only approximates: the classified digest is
// EXACTLY sha256 of the PRE-mutation bytes, and the byte count is the
// pre-mutation size. That is the value the catalog would have stored
// had classification not dropped it - proving the classification, not
// the pool, is what keeps stale bytes out (DESIGN_SPEC 3.5 point 1).
func TestPipelineMutatedDigestIsPreMutationBytes(t *testing.T) {
	root := t.TempDir()
	const original = "before-mutation"
	abs := filepath.Join(root, "mutate.bin")
	if err := os.WriteFile(abs, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	open := func(path string) (io.ReadCloser, error) {
		rc, err := osOpen(path)
		if err != nil {
			return nil, err
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

	res := hashViaPool(t, walkJob(t, root, "mutate.bin"), PoolOptions{OpenFile: open})
	if res.Err != nil {
		t.Fatalf("hash: %v", res.Err)
	}
	if res.Bytes != int64(len(original)) {
		t.Errorf("pool Bytes = %d, want %d (the pool read only the pre-mutation bytes)", res.Bytes, len(original))
	}

	c := ClassifyPath(res, nil, fsmeta.ModeFull) // nil src = real lstat, which sees the append
	if c.Status != StatusChangedDuringScan {
		t.Fatalf("Status = %q, want %q (the file grew on Close, before classification)", c.Status, StatusChangedDuringScan)
	}
	if want := sha256.Sum256([]byte(original)); c.Digest != want {
		t.Errorf("classified Digest = %x, want %x (exactly the pre-mutation bytes the catalog must NOT store)",
			c.Digest, want)
	}
	if want := int64(len(original)); c.Bytes != want {
		t.Errorf("classified Bytes = %d, want %d", c.Bytes, want)
	}
	if c.Detail == "" || !strings.Contains(c.Detail, "size") {
		t.Errorf("Detail = %q, want the changed-field detail naming size", c.Detail)
	}
}
