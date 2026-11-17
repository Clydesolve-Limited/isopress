package catalog

// T0.4 acceptance tests: catalog locking (DESIGN_SPEC 2.1 advisory lock
// as amended 2026-10-18, 2.4 single-writer model D-11, 10.1 CatalogBusy /
// exit 5, 12.3 locking tests). The flock enforces SINGLE-WRITER only:
// mutating-vs-mutating contention produces the CatalogBusy class, while
// read-only opens take NO flock and succeed alongside a writer via the
// WAL snapshot. The headline case is the two-process contention test
// required by 12.3: the test binary re-execs itself as a helper child
// that holds the exclusive flock, and the parent asserts a second
// mutating Open fails fast with the CatalogBusy class, well under the
// 5000 ms busy_timeout, leaving the catalog byte-identical.

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// busyBound is the test-side bound for a contended acquisition. The
// design bound is "immediately, well under the 5000 ms SQLite
// busy_timeout" (DESIGN_SPEC 2.1); one second is orders of magnitude of
// headroom over the microseconds a non-blocking flock contention costs,
// while still catching any regression to a blocking wait.
const busyBound = 1 * time.Second

// --- re-exec lock helper (DESIGN_SPEC 12.3 two-process pattern) -----------

const (
	lockHelperEnv     = "ISOPRESS_TEST_LOCK_HELPER"
	lockHelperPathEnv = "ISOPRESS_TEST_LOCK_HELPER_PATH"
	lockHelperReady   = "LOCK-HELD"
)

// TestMain re-execs the test binary as a lock-holder child process when
// ISOPRESS_TEST_LOCK_HELPER=1 (classic two-process test pattern);
// otherwise it runs the normal suite.
func TestMain(m *testing.M) {
	if os.Getenv(lockHelperEnv) == "1" {
		os.Exit(lockHelperMain())
	}
	os.Exit(m.Run())
}

// lockHelperMain acquires the exclusive catalog lock on the path given by
// the environment through the production FlockLocker, signals readiness
// on stdout, then holds the lock until stdin yields a line or reaches EOF
// (the parent telling it to exit) or the process is SIGKILLed (in which
// case the kernel releases the flock with the descriptor).
func lockHelperMain() int {
	path := os.Getenv(lockHelperPathEnv)
	if path == "" {
		fmt.Fprintln(os.Stderr, "lock helper: missing catalog path")
		return 2
	}
	release, err := NewFlockLocker(path).Acquire(LockExclusive)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lock helper: acquire: %v\n", err)
		return 1
	}
	fmt.Println(lockHelperReady)
	_, _ = bufio.NewReader(os.Stdin).ReadBytes('\n')
	if err := release(); err != nil {
		fmt.Fprintf(os.Stderr, "lock helper: release: %v\n", err)
		return 1
	}
	return 0
}

// lockHelper is a running lock-holder child process.
type lockHelper struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *bytes.Buffer
}

// startLockHelper re-execs the test binary, waits for the LOCK-HELD
// readiness line, and returns the live helper. Cleanup kills any helper
// the test did not stop cleanly.
func startLockHelper(t *testing.T, path string) *lockHelper {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), lockHelperEnv+"=1", lockHelperPathEnv+"="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("helper stdout pipe: %v", err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("helper stdin pipe: %v", err)
	}
	stderr := &bytes.Buffer{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting lock helper: %v", err)
	}
	h := &lockHelper{cmd: cmd, stdin: stdin, stderr: stderr}
	t.Cleanup(func() {
		if cmd.ProcessState == nil { // not yet waited (test failed early)
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		t.Fatalf("lock helper never became ready (stderr: %s): %v", stderr.String(), err)
	}
	if got := strings.TrimSpace(line); got != lockHelperReady {
		t.Fatalf("lock helper readiness line = %q, want %q (stderr: %s)", got, lockHelperReady, stderr.String())
	}
	return h
}

// stop tells the helper to release the lock and exit by closing its
// stdin, asserting a clean exit 0.
func (h *lockHelper) stop(t *testing.T) {
	t.Helper()
	if err := h.stdin.Close(); err != nil {
		t.Fatalf("helper stdin close: %v", err)
	}
	if err := h.cmd.Wait(); err != nil {
		t.Fatalf("lock helper did not exit cleanly: %v (stderr: %s)", err, h.stderr.String())
	}
}

// wantCatalogBusy classifies err as the CatalogBusy class and checks the
// message is the clear, stderr-worthy text T0.5 will print for exit 5.
func wantCatalogBusy(t *testing.T, err error, wantPath string, wantMode LockMode) {
	t.Helper()
	if err == nil {
		t.Fatal("contended acquisition succeeded, want CatalogBusy")
	}
	var busy *CatalogBusy
	if !errors.As(err, &busy) {
		t.Fatalf("err = %v, want a *CatalogBusy", err)
	}
	if !errors.Is(err, ErrCatalogBusy) {
		t.Errorf("errors.Is(err, ErrCatalogBusy) = false: %v", err)
	}
	if busy.Path != wantPath {
		t.Errorf("CatalogBusy.Path = %q, want %q", busy.Path, wantPath)
	}
	if busy.Mode != wantMode {
		t.Errorf("CatalogBusy.Mode = %v, want %v", busy.Mode, wantMode)
	}
	// The message must stand on its own on stderr: name the catalog, the
	// lock file, and the retry guidance.
	for _, want := range []string{"busy", wantPath, lockFilePath(wantPath), "another isopress process", "retry"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("CatalogBusy message %q does not contain %q", err.Error(), want)
		}
	}
}

// --- two-process contention (DESIGN_SPEC 12.3) ----------------------------

// While a helper PROCESS holds the exclusive flock on the catalog, a
// second mutating Open fails immediately with the CatalogBusy class
// (bounded well under the 5000 ms busy_timeout), the catalog file is
// byte-identical after the refused open, and the lock frees up once the
// helper exits.
func TestTwoProcessMutatingOpenFailsFastWithBusy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")
	mustCreate(t, path)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}

	h := startLockHelper(t, path)

	start := time.Now()
	db, openErr := Open(path, OpenReadWrite)
	elapsed := time.Since(start)
	if openErr == nil {
		db.Close()
		t.Fatal("mutating Open succeeded while another process held the exclusive lock")
	}
	wantCatalogBusy(t, openErr, path, LockExclusive)
	if elapsed >= busyBound {
		t.Errorf("contended mutating Open took %v, want well under %v (no blocking wait)", elapsed, busyBound)
	}

	// No partial writes: the refused open never touched the database.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read catalog: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("refused mutating open modified the catalog file")
	}

	// Clean helper exit releases the lock; a mutating open then succeeds.
	h.stop(t)
	db = mustOpen(t, path, OpenReadWrite)
	if v, err := db.SchemaVersion(t.Context()); err != nil || v != SchemaVersion {
		t.Fatalf("SchemaVersion after helper exit = %d, %v; want %d, nil", v, err, SchemaVersion)
	}
}

// A crashed (SIGKILLed) holder cannot leave the catalog locked: flock
// locks die with the owning descriptor at process death.
func TestCrashedHolderDoesNotStrandLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")
	mustCreate(t, path)

	h := startLockHelper(t, path)
	if err := h.cmd.Process.Kill(); err != nil {
		t.Fatalf("killing lock helper: %v", err)
	}
	if err := h.cmd.Wait(); err == nil {
		t.Fatal("lock helper exited cleanly after Kill, want a kill error")
	}

	// The kernel released the lock with the helper's descriptors.
	start := time.Now()
	db, err := Open(path, OpenReadWrite)
	if err != nil {
		t.Fatalf("mutating Open after SIGKILLed holder: %v (elapsed %v)", err, time.Since(start))
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// --- reader/writer matrix (amended 2.1: readers hold no flock) ------------

// Two read-only opens succeed concurrently and both can query: readers
// take no advisory lock at all, so nothing serializes them (the
// pre-amendment shared-lock framing is gone).
func TestReadOnlyOpensSucceedConcurrently(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")
	mustCreate(t, path)

	db1 := mustOpen(t, path, OpenReadOnly)
	db2 := mustOpen(t, path, OpenReadOnly)
	if db1.Path() != db2.Path() {
		t.Fatalf("reader paths differ: %q vs %q", db1.Path(), db2.Path())
	}
	// Both handles are usable while the other is open.
	for _, db := range []*DB{db1, db2} {
		if v, err := db.SchemaVersion(t.Context()); err != nil || v != SchemaVersion {
			t.Fatalf("SchemaVersion on concurrent reader = %d, %v; want %d, nil", v, err, SchemaVersion)
		}
	}
}

// A read-only open SUCCEEDS while a mutating run holds the exclusive
// flock and an open write connection: readers take no flock, and the WAL
// snapshot gives them consistent, queryable catalog state (amended
// DESIGN_SPEC 2.1 and 12.3: assert SUCCESS via a WAL snapshot). Data the
// writer committed before the reader's query is visible, and the writer
// is undisturbed by the reader's arrival.
func TestReadOnlyOpenSucceedsWhileExclusiveHeld(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")
	mustCreate(t, path)

	writer := mustOpen(t, path, OpenReadWrite) // exclusive flock + write connection

	// No busy, no wait, no sidecar involvement: the reader opens fine.
	reader := mustOpen(t, path, OpenReadOnly)

	// Real catalog queries through the WAL snapshot while the writer
	// holds both the flock and a live write connection.
	if got := integrityCheck(t, reader.sql); got != "ok" {
		t.Fatalf("reader integrity_check during writer = %q, want ok", got)
	}
	if _, err := writer.sql.ExecContext(t.Context(),
		`INSERT INTO sets (set_id, source_root, created_at, tool_version)
			VALUES ('S-1', '/src', 1, 'test')`); err != nil {
		t.Fatalf("writer insert: %v", err)
	}
	var count int
	if err := reader.sql.QueryRowContext(t.Context(),
		`SELECT count(*) FROM sets WHERE set_id = 'S-1'`).Scan(&count); err != nil {
		t.Fatalf("reader query during writer: %v", err)
	}
	if count != 1 {
		t.Fatalf("reader saw %d committed writer rows during the run, want 1 (WAL snapshot)", count)
	}

	// The writer handle is still fully usable after the reader arrived.
	if v, err := writer.SchemaVersion(t.Context()); err != nil || v != SchemaVersion {
		t.Fatalf("writer SchemaVersion after reader arrived = %d, %v; want %d, nil", v, err, SchemaVersion)
	}
	// And the reader survives the writer's Close.
	if err := writer.Close(); err != nil {
		t.Fatalf("Close writer: %v", err)
	}
	if v, err := reader.SchemaVersion(t.Context()); err != nil || v != SchemaVersion {
		t.Fatalf("reader SchemaVersion after writer Close = %d, %v; want %d, nil", v, err, SchemaVersion)
	}
}

// A mutating open SUCCEEDS while a read-only handle is open (readers
// hold no flock, amended 2.1), and the reader is undisturbed by the
// writer. The read-only open on the fresh catalog left no sidecar behind.
func TestExclusiveOpenSucceedsWhileReadOnlyHeld(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")
	mustCreate(t, path)

	reader := mustOpen(t, path, OpenReadOnly)
	if _, err := os.Stat(lockFilePath(path)); !os.IsNotExist(err) {
		t.Fatalf("read-only open created the sidecar (stat err = %v)", err)
	}

	writer := mustOpen(t, path, OpenReadWrite)
	if _, err := os.Stat(lockFilePath(path)); err != nil {
		t.Fatalf("mutating open did not create the sidecar: %v", err)
	}
	if v, err := writer.SchemaVersion(t.Context()); err != nil || v != SchemaVersion {
		t.Fatalf("writer SchemaVersion = %d, %v; want %d, nil", v, err, SchemaVersion)
	}
	if v, err := reader.SchemaVersion(t.Context()); err != nil || v != SchemaVersion {
		t.Fatalf("reader SchemaVersion during writer = %d, %v; want %d, nil", v, err, SchemaVersion)
	}

	// Both close cleanly, in either order.
	if err := writer.Close(); err != nil {
		t.Fatalf("Close writer: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatalf("Close reader: %v", err)
	}
}

// At the Locker SEAM level (not via Open), FlockLocker still supports
// LockShared: LOCK_SH and LOCK_EX contend at the flock layer, both
// non-blocking, both classified CatalogBusy. Open never requests
// LockShared since the 2026-10-18 amendment; this pins the seam behavior
// for direct users of the Locker interface.
func TestFlockLockerSharedExclusiveContention(t *testing.T) {
	locker := NewFlockLocker(filepath.Join(t.TempDir(), "catalog.sqlite"))

	relShared, err := locker.Acquire(LockShared)
	if err != nil {
		t.Fatalf("shared Acquire: %v", err)
	}
	// Exclusive contends with a shared holder.
	if _, err := locker.Acquire(LockExclusive); err == nil {
		t.Fatal("exclusive Acquire under shared holder succeeded, want CatalogBusy")
	} else {
		wantCatalogBusy(t, err, locker.catalogPath, LockExclusive)
	}
	if err := relShared(); err != nil {
		t.Fatalf("shared release: %v", err)
	}

	// Exclusive holds; shared contends with it.
	relExclusive, err := locker.Acquire(LockExclusive)
	if err != nil {
		t.Fatalf("exclusive Acquire: %v", err)
	}
	if _, err := locker.Acquire(LockShared); err == nil {
		t.Fatal("shared Acquire under exclusive holder succeeded, want CatalogBusy")
	} else {
		wantCatalogBusy(t, err, locker.catalogPath, LockShared)
	}
	if err := relExclusive(); err != nil {
		t.Fatalf("exclusive release: %v", err)
	}

	// Both modes are free again.
	rel, err := locker.Acquire(LockExclusive)
	if err != nil {
		t.Fatalf("exclusive Acquire after releases: %v", err)
	}
	if err := rel(); err != nil {
		t.Fatalf("release: %v", err)
	}
}

// --- release semantics -----------------------------------------------------

// After release the lock can be re-acquired; the release function is
// idempotent (second call is a safe no-op returning nil); the sidecar is
// NOT deleted on release (unlink-race rationale in the FlockLocker doc).
func TestFlockReleaseSemantics(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")
	locker := NewFlockLocker(path)

	release, err := locker.Acquire(LockExclusive)
	if err != nil {
		t.Fatalf("exclusive Acquire: %v", err)
	}
	// While held, a second acquisition on a distinct descriptor (even
	// in-process) is refused with the busy error.
	_, secondErr := locker.Acquire(LockExclusive)
	wantCatalogBusy(t, secondErr, path, LockExclusive)

	if err := release(); err != nil {
		t.Fatalf("first release: %v", err)
	}
	if err := release(); err != nil {
		t.Fatalf("second release must be a safe no-op returning nil, got %v", err)
	}

	// Sidecar hygiene: no unlink on release.
	if _, err := os.Stat(lockFilePath(path)); err != nil {
		t.Fatalf("lock sidecar missing after release: %v", err)
	}

	// Free again after release.
	release2, err := locker.Acquire(LockExclusive)
	if err != nil {
		t.Fatalf("re-Acquire after release: %v", err)
	}
	if err := release2(); err != nil {
		t.Fatalf("second-generation release: %v", err)
	}
}

// --- sidecar and default-locker wiring -------------------------------------

// The sidecar path is exactly "<catalog>.lock".
func TestLockSidecarPathDerivation(t *testing.T) {
	if got := lockFilePath("/x/catalog.sqlite"); got != "/x/catalog.sqlite.lock" {
		t.Fatalf("lockFilePath = %q, want %q", got, "/x/catalog.sqlite.lock")
	}
}

// Open without WithLocker uses the default flock Locker on the MUTATING
// path: a read-write open creates the sidecar next to the catalog while
// the handle is open.
func TestDefaultLockerCreatesSidecar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")
	mustCreate(t, path)

	db := mustOpen(t, path, OpenReadWrite)
	if _, err := os.Stat(lockFilePath(path)); err != nil {
		t.Fatalf("default locker did not create the sidecar: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// A read-only open takes NO advisory lock (amended DESIGN_SPEC 2.1): on a
// fresh catalog path it succeeds without creating the ".lock" sidecar at
// all, and Close leaves the directory free of lock artefacts.
func TestReadOnlyOpenCreatesNoSidecar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")
	mustCreate(t, path)

	db := mustOpen(t, path, OpenReadOnly)
	if _, err := os.Stat(lockFilePath(path)); !os.IsNotExist(err) {
		t.Fatalf("read-only open created the sidecar (stat err = %v)", err)
	}
	if v, err := db.SchemaVersion(t.Context()); err != nil || v != SchemaVersion {
		t.Fatalf("SchemaVersion = %d, %v; want %d, nil", v, err, SchemaVersion)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(lockFilePath(path)); !os.IsNotExist(err) {
		t.Fatalf("sidecar appeared after read-only Close (stat err = %v)", err)
	}
}

// An Open whose lock sidecar cannot be created (missing parent directory)
// fails as a typed KindOpen error naming the sidecar, before any database
// work.
func TestAcquireFailsKindOpenWithoutParentDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing", "catalog.sqlite")

	_, err := Open(path, OpenReadWrite)
	var cerr *CatalogError
	if !errors.As(err, &cerr) || cerr.Kind != KindOpen {
		t.Fatalf("Open in missing dir: err = %v, want KindOpen CatalogError", err)
	}
	if !strings.Contains(err.Error(), lockFilePath(path)) {
		t.Errorf("message %q does not name the lock sidecar", err.Error())
	}
}

// An undefined LockMode is rejected before touching the filesystem.
func TestAcquireRejectsInvalidMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")

	if _, err := NewFlockLocker(path).Acquire(LockMode(42)); err == nil {
		t.Fatal("Acquire(LockMode(42)) succeeded, want an error")
	}
	if _, err := os.Stat(lockFilePath(path)); !os.IsNotExist(err) {
		t.Fatalf("invalid-mode Acquire touched the sidecar (stat err = %v)", err)
	}
}

// --- dependency gate (DESIGN_SPEC 11) --------------------------------------

// No import of golang.org/x/sys/unix anywhere in the package: the
// dependency gate allows golang.org/x/sys as an indirect dependency only;
// the lock uses stdlib syscall.Flock and the free-space pre-check uses
// stdlib syscall.Statfs. The check parses the real import declarations of
// every file in the package (comments naming the ban are fine).
func TestNoDirectXSysUnixImport(t *testing.T) {
	const banned = "golang.org/x/sys/unix"
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, e.Name(), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		for _, imp := range file.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("unquote import path in %s: %v", e.Name(), err)
			}
			if path == banned {
				t.Errorf("%s imports %s; only stdlib syscall is allowed (DESIGN_SPEC 11)", e.Name(), banned)
			}
		}
	}
}
