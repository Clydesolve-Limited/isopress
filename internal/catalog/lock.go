package catalog

// T0.4: advisory catalog locking (DESIGN_SPEC 2.1 connection policy as
// amended 2026-10-18, 2.4 single-writer model D-11, 10.1 CatalogBusy
// class, 12.3 locking tests).
//
// The tool coordinates whole-run SINGLE-WRITER exclusivity with an
// advisory flock(2) lock on a sidecar file "<catalog>.lock" - never with
// SQLite contention. Mutating commands (scan, plan, copy, build,
// restore-with-marking, clear-assignments) hold LOCK_EX|LOCK_NB for their
// whole run; a contended acquisition fails immediately with the
// CatalogBusy class (exit 5), never a wait. The flock's role is
// single-WRITER enforcement only (2.4), not reader coordination.
//
// Read-only commands (report, verify-media, validate, restore-read-only)
// take NO flock at all (amended 2.1): they open mode=ro via the URI flag
// and rely on the WAL snapshot for consistency, so they run concurrently
// with each other and with a writer at the system level. A read-only open
// therefore never creates the sidecar and can never return the busy
// class from lock contention.
//
// Only stdlib syscall.Flock is used. The DESIGN_SPEC 11 dependency gate
// forbids direct golang.org/x/sys/unix imports (golang.org/x/sys stays an
// indirect dependency only), so the raw syscall surface is the sanctioned
// mechanism here (as is syscall.Statfs for the imagebuild free-space
// pre-check).

import (
	"errors"
	"fmt"
	"sync"
	"syscall"
)

// lockFileSuffix is appended to the catalog path to form the advisory
// lock sidecar path (DESIGN_SPEC 2.1): "<catalog>.lock".
const lockFileSuffix = ".lock"

// lockFilePath derives the sidecar lock path from the catalog path. The
// sidecar lives next to the catalog so it shares the catalog's directory
// permissions and its filesystem (flock only arbitrates between
// descriptors of the same inode, so the sidecar must never be placed on a
// different filesystem than the processes that contend for it).
func lockFilePath(catalogPath string) string {
	return catalogPath + lockFileSuffix
}

// ErrCatalogBusy is the sentinel of the CatalogBusy error class
// (DESIGN_SPEC 10.1: "another mutating instance holds the lock (D-11)",
// exit 5). Every *CatalogBusy unwraps to it, so callers may classify with
// errors.Is(err, ErrCatalogBusy) or errors.As(err, &busy).
var ErrCatalogBusy = errors.New("catalog is locked by another isopress process")

// CatalogBusy is the typed error of the CatalogBusy class
// (DESIGN_SPEC 10.1): the exclusive advisory lock could not be acquired
// because another isopress process (or an in-process acquisition on
// another descriptor) already holds it. ONLY a mutating open can produce
// this class: read-only opens take no flock (amended DESIGN_SPEC 2.1),
// so a read-only command never fails with the busy class from lock
// contention. T0.5 maps *CatalogBusy to exit 5 and prints its message to
// stderr; the message below is written to be that stderr text.
//
// Design note (why not a KindBusy on CatalogError): DESIGN_SPEC 10.1
// places CatalogBusy in a SEPARATE taxonomy class from CatalogError with
// its own exit code (5 vs 4), and migrate.go documents that every
// *CatalogError maps to exit 4. Folding busy into the CatalogError kind
// set would collapse the two exit codes, so CatalogBusy is a distinct
// type - same structural pattern as *CatalogError (path + underlying
// cause + Unwrap), plus the sentinel ErrCatalogBusy for errors.Is.
type CatalogBusy struct {
	Path     string   // catalog path the acquisition referred to
	LockPath string   // sidecar lock file ("<catalog>.lock")
	Mode     LockMode // the acquisition mode that was refused
	Err      error    // underlying flock error (EWOULDBLOCK); always non-nil
}

func (e *CatalogBusy) Error() string {
	switch e.Mode {
	case LockShared:
		return fmt.Sprintf(
			"catalog: busy: %s is write-locked by another isopress process (lock file %s): "+
				"a mutating run holds the catalog exclusively; retry after it finishes",
			e.Path, e.LockPath)
	default:
		return fmt.Sprintf(
			"catalog: busy: %s is locked by another isopress process (lock file %s): "+
				"the catalog is single-writer (one mutating or planning run at a time); "+
				"retry after the other run finishes",
			e.Path, e.LockPath)
	}
}

// Unwrap exposes both the class sentinel (ErrCatalogBusy) and the
// underlying flock errno (EWOULDBLOCK) to errors.Is, while keeping the
// raw cause available to errors.As chains.
func (e *CatalogBusy) Unwrap() error { return errors.Join(ErrCatalogBusy, e.Err) }

// FlockLocker is the default Locker implementation (T0.4): an flock(2)
// advisory lock over the sidecar file "<catalog>.lock"
// (DESIGN_SPEC 2.1 as amended 2026-10-18, 2.4). Open constructs one via
// NewFlockLocker and calls Acquire(LockExclusive) for MUTATING opens
// only; read-only opens never touch the locker (see below).
//
// Acquisition semantics:
//
//   - LockExclusive is LOCK_EX|LOCK_NB: exactly one mutating run admits
//     at a time (single-writer model, D-11). Contention fails IMMEDIATELY
//     with *CatalogBusy - a well-bounded operation, orders of magnitude
//     under the 5000 ms SQLite busy_timeout, which is deliberately left
//     as the second line of defense only. This is the only mode Open
//     requests.
//   - LockShared (LOCK_SH|LOCK_NB) remains supported for completeness of
//     the Locker seam and for direct use, but Open NEVER requests it: as
//     amended 2026-10-18 the flock's role is single-WRITER enforcement
//     only, and read-only opens take no lock at all - their consistency
//     mechanism is mode=ro plus the WAL snapshot, which lets readers run
//     alongside a writer at the system level. (The pre-amendment design
//     had readers take LOCK_SH, which self-contradicted at the flock
//     layer: LOCK_SH and LOCK_EX are mutually exclusive, so a reader
//     would have been blocked by - or worse, contended with - the very
//     writer it was supposed to coexist with.)
//
// Lifecycle:
//
//   - The sidecar is created if absent and is NEVER deleted on release.
//     Unlinking a lock file is a classic race: between unlink and close a
//     third process can create a fresh inode and lock it while the second
//     holder still holds the old one, splitting one logical lock into two
//     concurrently-held locks. The sidecar is zero bytes and costs
//     nothing to keep. Only mutating opens create it; a read-only open
//     leaves no trace on the filesystem.
//   - The release function unlocks and closes the descriptor exactly
//     once; further calls are safe no-ops returning the first result.
//   - A crashed or SIGKILLed holder cannot leave the catalog locked: the
//     kernel releases flock locks when the owning descriptor closes at
//     process death. The lock lives in the kernel, never in user state.
//   - The sidecar is invisible to SQLite (it is not a database file); a
//     scan whose root contains the catalog directory will see it as an
//     ordinary file - excluding ingest artefacts from scans is ingest's
//     concern (DESIGN_SPEC 3.2), recorded here for traceability.
//
// Only stdlib syscall.Flock is used (DESIGN_SPEC 11 forbids direct
// golang.org/x/sys/unix imports).
type FlockLocker struct {
	catalogPath string
}

// NewFlockLocker returns the flock Locker for the catalog at catalogPath.
// Open calls it with the ABSOLUTE catalog path when no Locker is injected
// via WithLocker, for MUTATING opens only (read-only opens take no lock);
// tests may construct one directly against any path. The catalog file
// itself need not exist - the lock is taken on the sidecar, which Acquire
// creates if absent.
func NewFlockLocker(catalogPath string) *FlockLocker {
	return &FlockLocker{catalogPath: catalogPath}
}

// Acquire takes the advisory lock on "<catalogPath>.lock" and returns the
// release function (Locker interface, DESIGN_SPEC 2.1 as amended
// 2026-10-18). Open calls it with LockExclusive for mutating opens only;
// both modes are non-blocking (see FlockLocker for the semantics). Failures
// classify as:
//
//   - *CatalogBusy: the lock is held by another holder (contention);
//   - *CatalogError (KindOpen): lock-file infrastructure failure (the
//     sidecar cannot be created or locked - missing parent directory,
//     permissions, I/O error). Open passes locker errors through
//     untouched, so both surface from Open directly.
func (l *FlockLocker) Acquire(mode LockMode) (func() error, error) {
	if mode != LockShared && mode != LockExclusive {
		return nil, fmt.Errorf("catalog: invalid LockMode %d", mode)
	}
	lockPath := lockFilePath(l.catalogPath)

	// O_RDONLY suffices for flock (the lock does not depend on the access
	// mode) and is the most permissive choice for a pre-existing sidecar
	// left by another user. O_CLOEXEC keeps the descriptor - and with it
	// the lock - out of any child process (xorriso, restores).
	fd, err := syscall.Open(lockPath, syscall.O_RDONLY|syscall.O_CREAT|syscall.O_CLOEXEC, 0o644)
	if err != nil {
		return nil, &CatalogError{
			Kind: KindOpen,
			Path: l.catalogPath,
			Err:  fmt.Errorf("opening advisory lock file %s: %w", lockPath, err),
		}
	}

	how := syscall.LOCK_SH
	if mode == LockExclusive {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(fd, how|syscall.LOCK_NB); err != nil {
		// The descriptor carries no lock now; close it before reporting.
		_ = syscall.Close(fd)
		// EWOULDBLOCK == EAGAIN on every platform this tool targets, so
		// one check covers both spellings.
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, &CatalogBusy{
				Path:     l.catalogPath,
				LockPath: lockPath,
				Mode:     mode,
				Err:      err,
			}
		}
		return nil, &CatalogError{
			Kind: KindOpen,
			Path: l.catalogPath,
			Err:  fmt.Errorf("flocking advisory lock file %s: %w", lockPath, err),
		}
	}

	// Release semantics: unlock and close exactly once; later calls are
	// safe no-ops returning the first call's result (nil on success). The
	// sidecar file is intentionally left on disk (unlink-race rationale
	// in the FlockLocker doc).
	var once sync.Once
	var releaseErr error
	release := func() error {
		once.Do(func() {
			if err := syscall.Flock(fd, syscall.LOCK_UN); err != nil {
				releaseErr = fmt.Errorf("catalog: unlocking %s: %w", lockPath, err)
			}
			if err := syscall.Close(fd); err != nil && releaseErr == nil {
				releaseErr = fmt.Errorf("catalog: closing lock file %s: %w", lockPath, err)
			}
		})
		return releaseErr
	}
	return release, nil
}
