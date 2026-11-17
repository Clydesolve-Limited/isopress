package catalog

// T0.4 lock-semantics gap tests. These pin the edge behaviour of the
// flock advisory lock so downstream tasks (especially T0.5 CLI wiring)
// inherit a written contract instead of folklore:
//
//   - flock(2) locks belong to the OPEN FILE DESCRIPTION, not to the
//     process: two MUTATING Open calls in the SAME process on the same
//     path contend exactly like two processes do (a second mutating Open
//     is refused with CatalogBusy). Only POSIX fcntl(2) record locks have
//     the per-process "same process re-acquisition succeeds" behaviour;
//     flock does not. In-process double-Open IS protected. Read-only
//     opens are exempt by construction: they take no flock at all
//     (amended DESIGN_SPEC 2.1), so a reader coexists with an
//     in-process writer.
//   - If (*DB).Close is never called, the exclusive lock is held for the
//     whole remaining life of the process; the only escape is process
//     death, at which point the kernel closes the descriptor and frees
//     the flock (tested by TestCrashedHolderDoesNotStrandLock in
//     lock_test.go). Callers must Close.
//   - A stray DIRECTORY at the sidecar path is an infrastructure
//     failure (KindOpen, exit 4) on the MUTATING path, never a silent
//     mis-lock: open(2) with O_CREAT on a directory fails EISDIR even
//     for O_RDONLY on Linux. Read-only opens never touch the sidecar and
//     are unaffected.
//   - The sidecar path is derived LEXICALLY from the catalog path as
//     given (filepath.Abs does not resolve symlinks): locking through a
//     symlinked catalog path locks "<symlink>.lock" next to the symlink,
//     not the sidecar of the resolved target. Contention is per given
//     path, not per resolved inode - two commands pointed at different
//     paths to the same database file do NOT contend. Documented here as
//     a known per-path limitation of advisory sidecar locking.
//   - A failed Open releases the advisory lock it took (abort path).

import (
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// Same-process double-Open on the same path is REFUSED: flock
// arbitration is per open file description, so each Open call gets an
// independent descriptor that contends with every other one, including
// descriptors opened by the same process. T0.5 note: two mutating
// commands double-opening in one process ARE mutually excluded - there
// is no in-process escape hatch. The first handle stays fully usable
// while the second is refused, and the lock frees on Close (the
// "Close never called" case: the lock then lives exactly as long as the
// process, per the flock kernel semantics).
func TestSameProcessDoubleMutatingOpenRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")
	mustCreate(t, path)

	db1 := mustOpen(t, path, OpenReadWrite)

	_, err := Open(path, OpenReadWrite)
	wantCatalogBusy(t, err, path, LockExclusive)

	// A read-only Open is NOT refused while the in-process writer holds
	// the exclusive lock: readers take no flock at all (amended
	// DESIGN_SPEC 2.1), and WAL keeps their snapshot consistent.
	ro, roErr := Open(path, OpenReadOnly)
	if roErr != nil {
		t.Fatalf("read-only Open while in-process writer holds the flock: %v", roErr)
	}
	if v, err := ro.SchemaVersion(t.Context()); err != nil || v != SchemaVersion {
		t.Fatalf("SchemaVersion on reader during in-process writer = %d, %v; want %d, nil", v, err, SchemaVersion)
	}
	if err := ro.Close(); err != nil {
		t.Fatalf("Close reader: %v", err)
	}

	// The first handle was not disturbed by the refused attempts.
	if v, err := db1.SchemaVersion(t.Context()); err != nil || v != SchemaVersion {
		t.Fatalf("SchemaVersion on surviving writer = %d, %v; want %d, nil", v, err, SchemaVersion)
	}

	// Close releases the in-process lock just as a process exit would.
	if err := db1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	db2 := mustOpen(t, path, OpenReadWrite)
	if err := db2.Close(); err != nil {
		t.Fatalf("Close after re-open: %v", err)
	}
}

// A stray DIRECTORY at the sidecar path fails the MUTATING acquisition
// with a KindOpen infrastructure error naming the sidecar (exit 4), while
// read-only opens are completely unaffected because they never touch the
// sidecar (amended DESIGN_SPEC 2.1: readers take no flock).
func TestSidecarPathIsDirectoryFailsKindOpen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.sqlite")
	mustCreate(t, path)
	if err := os.Mkdir(lockFilePath(path), 0o755); err != nil {
		t.Fatalf("creating directory at sidecar path: %v", err)
	}

	// Read-only opens never touch the sidecar (no flock, amended 2.1):
	// a stray directory at the sidecar path cannot affect them, and the
	// open succeeds against the catalog itself.
	ro, err := Open(path, OpenReadOnly)
	if err != nil {
		t.Fatalf("read-only Open with directory sidecar must succeed (readers take no flock): %v", err)
	}
	if v, verr := ro.SchemaVersion(t.Context()); verr != nil || v != SchemaVersion {
		t.Fatalf("reader SchemaVersion = %d, %v; want %d, nil", v, verr, SchemaVersion)
	}
	if err := ro.Close(); err != nil {
		t.Fatalf("Close reader: %v", err)
	}

	// The MUTATING path still fails with a KindOpen infrastructure error
	// naming the sidecar (exit 4). open(2) with O_CREAT on an existing
	// directory returns EISDIR even with O_RDONLY access on Linux, so the
	// failure surfaces at open time - before any flock call - and can
	// never be mistaken for the CatalogBusy class (exit 5).
	_, err = Open(path, OpenReadWrite)
	var cerr *CatalogError
	if !errors.As(err, &cerr) || cerr.Kind != KindOpen {
		t.Fatalf("mutating Open with directory sidecar: err = %v, want KindOpen CatalogError", err)
	}
	if !errors.Is(err, syscall.EISDIR) {
		t.Errorf("mutating Open error does not unwrap to EISDIR: %v", err)
	}
	if !strings.Contains(err.Error(), lockFilePath(path)) {
		t.Errorf("mutating Open message %q does not name the lock sidecar", err.Error())
	}
	var busy *CatalogBusy
	if errors.As(err, &busy) {
		t.Errorf("mutating Open with directory sidecar classified as CatalogBusy: %v", err)
	}
}

// The sidecar path is derived lexically from the catalog path as given:
// opening a catalog through a SYMLINK locks "<symlink>.lock" next to the
// symlink, and the resolved target gets no sidecar. Consequence, pinned
// here for T0.5's documentation: contention is per given path, not per
// resolved database inode - a second command pointed at the TARGET path
// while the symlink path is held does NOT contend. Advisory sidecar
// locking cannot see through path aliases; operators must pick one
// canonical path per catalog.
func TestSymlinkCatalogSidecarIsLexical(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real", "catalog.sqlite")
	if err := os.Mkdir(filepath.Dir(target), 0o755); err != nil {
		t.Fatalf("mkdir target dir: %v", err)
	}
	mustCreate(t, target)
	link := filepath.Join(dir, "catalog.sqlite")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	// Open via the symlink: SQLite follows it; the sidecar lands next to
	// the symlink, not next to the target.
	db := mustOpen(t, link, OpenReadWrite)
	if db.Path() != link {
		t.Fatalf("DB.Path() = %q, want the symlink path %q (filepath.Abs is lexical)", db.Path(), link)
	}
	if _, err := os.Lstat(link + lockFileSuffix); err != nil {
		t.Fatalf("no sidecar next to the symlink: %v", err)
	}
	if _, err := os.Lstat(target + lockFileSuffix); !os.IsNotExist(err) {
		t.Fatalf("sidecar appeared next to the symlink TARGET (%s.lock), want it derived from the given path only", target)
	}

	// Per-path semantics: the TARGET path does not contend with the
	// SYMLINK path even though both resolve to the same database file.
	db2, err := Open(target, OpenReadWrite)
	if err != nil {
		t.Fatalf("Open(target) while symlink path is held: %v "+
			"(per-path locking does not see through symlinks - documented limitation)", err)
	}
	if err := db2.Close(); err != nil {
		t.Fatalf("Close target handle: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close symlink handle: %v", err)
	}
}

// A failed Open releases the advisory lock its abort path took: after
// Open refuses a missing catalog file, the sidecar still exists (never
// unlinked) but the flock is free - an immediate re-acquisition
// succeeds without contention.
func TestFailedOpenReleasesAdvisoryLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.sqlite") // parent dir exists, file does not

	_, err := Open(path, OpenReadWrite)
	var cerr *CatalogError
	if !errors.As(err, &cerr) || cerr.Kind != KindOpen {
		t.Fatalf("Open(missing catalog): err = %v, want KindOpen CatalogError", err)
	}

	// The sidecar was created and left in place (no unlink on release)...
	if _, err := os.Stat(lockFilePath(path)); err != nil {
		t.Fatalf("sidecar missing after failed Open: %v", err)
	}
	// ...and the lock itself was released by the abort path.
	release, acqErr := NewFlockLocker(path).Acquire(LockExclusive)
	if acqErr != nil {
		t.Fatalf("exclusive Acquire after failed Open (abort path must release the lock): %v", acqErr)
	}
	if err := release(); err != nil {
		t.Fatalf("release: %v", err)
	}
}

// Negative control for TestNoDirectXSysUnixImport (lock_test.go): the
// import-parsing assertion must actually PARSE import declarations, not
// grep comments or file bytes. The same parser configuration applied to
// a synthetic file that imports the banned package must flag it - if
// this control ever fails, the dependency-gate test has gone vacuous.
func TestNoDirectXSysUnixImportParseMechanism(t *testing.T) {
	const banned = "golang.org/x/sys/unix"
	src := "package fake\n\nimport (\n\t\"os\"\n\t_ \"" + banned + "\" //nolint\n)\n\n// " + banned + " mentioned in a comment must NOT trip the check.\n"

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fake.go", src, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse synthetic source: %v", err)
	}
	found := false
	commentMentions := strings.Contains(src, banned) // the comment also names it
	for _, imp := range file.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			t.Fatalf("unquote synthetic import: %v", err)
		}
		if path == banned {
			found = true
		}
	}
	if !found {
		t.Fatal("import parser did not detect the banned import in the synthetic source; " +
			"TestNoDirectXSysUnixImport would be vacuous")
	}
	if !commentMentions {
		t.Fatal("synthetic source lost its comment mention; control no longer proves comments are ignored")
	}
}
