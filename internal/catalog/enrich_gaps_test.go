package catalog

// Gap-closure tests for the D2 missing-parent-directory message
// enrichment (enrichMissingParentDir, migrate.go; PM decision D2 of
// 2026-10-19). The baseline coverage lives in catalog_gaps_test.go
// (TestMissingParentDirectory, TestParentDirEnrichmentDoesNotFireOtherwise);
// this file pins the remaining audit points:
//
//   - pass-through gates: *CatalogBusy and non-KindOpen kinds are never
//     touched, even when the parent really is missing;
//   - the missing-parent-makes-busy-unreachable reasoning (the sidecar
//     CREATE fails KindOpen before flock(2) is ever called);
//   - a parent component that is a FILE, not a directory (ENOTDIR) is a
//     different failure with a different message - the enrichment must
//     NOT claim the directory "does not exist";
//   - TOCTOU best-effort: enrichment stats the parent at MESSAGE-BUILD
//     time, so a directory that vanished or reappeared between the
//     original failure and the enrichment stat yields a best-effort
//     (never-panicking, class-preserving, cause-preserving) result;
//   - the error chain survives the enrichment wrap: errors.Is over the
//     original errno and errors.As over KindOpen both still work, which
//     is what keeps the class exit 4 (DESIGN_SPEC 10.2).
//
// Reachability note (verified by reading Open, migrate.go): the
// post-stat classifyOpenErr sites in Open (sql.Open/Ping) are
// UNREACHABLE with a missing parent at open time - a read-only open
// fails at the os.Stat gate ("not found or unreadable", enriched) and a
// mutating open fails even earlier at the flock sidecar create
// ("opening advisory lock file", enriched). The only way to reach them
// with a vanished directory is a TOCTOU window between the stat gate
// and Ping; that window is covered by the TOCTOU test below at the unit
// level (the message enrichment is explicitly best-effort, the class
// and cause are not). Create's sql.Open/Ping sites DO carry the
// enrichment (they are the reachable missing-parent path for scan's
// auto-create), pinned by TestMissingParentDirectory.

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestEnrichPassThroughGates pins the two pass-through gates of
// enrichMissingParentDir with the parent directory genuinely MISSING:
//
//   - a *CatalogBusy is not a *CatalogError, so errors.As fails and the
//     value returns byte/pointer-identical (the busy class keeps its own
//     exit 5 - DESIGN_SPEC 10.1/10.2 never let D2 collapse it into 4);
//   - a KindCorrupt CatalogError is not KindOpen, so it passes through
//     untouched even with a missing parent (the corruption diagnosis
//     never blames a directory);
//   - a plain non-CatalogError passes through untouched (the gate is
//     errors.As, not a nil check).
func TestEnrichPassThroughGates(t *testing.T) {
	base := t.TempDir()
	abs := filepath.Join(base, "still", "missing", "dir", "cat.db")
	if _, statErr := os.Stat(filepath.Dir(abs)); !os.IsNotExist(statErr) {
		t.Fatalf("test setup: parent dir should not exist (stat err = %v)", statErr)
	}

	// Busy: passes through as the very same value.
	busy := &CatalogBusy{
		Path:     abs,
		LockPath: abs + lockFileSuffix,
		Mode:     LockExclusive,
		Err:      syscall.EWOULDBLOCK,
	}
	if got := enrichMissingParentDir(abs, busy); got != busy {
		t.Errorf("enrichMissingParentDir mutated a *CatalogBusy:\n got: %v\nwant: the identical value", got)
	}

	// KindCorrupt with a missing parent: same value back.
	corrupt := &CatalogError{Kind: KindCorrupt, Path: abs, Err: errors.New("disk image is malformed")}
	if got := enrichMissingParentDir(abs, corrupt); got != corrupt {
		t.Errorf("enrichMissingParentDir mutated a KindCorrupt error:\n got: %v\nwant: the identical value", got)
	}

	// Plain error: same value back.
	plain := errors.New("something unrelated")
	if got := enrichMissingParentDir(abs, plain); got != plain {
		t.Errorf("enrichMissingParentDir mutated a plain error:\n got: %v\nwant: the identical value", got)
	}
}

// TestMissingParentNeverYieldsBusyClass reasons the impossibility
// through the real Open path: a mutating open of a catalog in a MISSING
// directory fails in FlockLocker.Acquire at the sidecar open(2)
// (O_CREAT on "<catalog>.lock" in a nonexistent directory -> ENOENT,
// KindOpen), because CatalogBusy can only be produced by a FAILED
// flock(2) - and flock is only reached after the sidecar descriptor was
// successfully opened, which requires the directory to exist. So the
// busy class (exit 5) is unreachable with a missing parent; the error
// must be KindOpen and enriched. The injected-locker sub-case then
// bypasses the lock layer entirely to show the os.Stat gate catches the
// missing parent next - proving the post-stat classifyOpenErr sites of
// Open stay unreachable (the enrichment there is absent BY DESIGN,
// because they cannot fire with an absent parent).
func TestMissingParentNeverYieldsBusyClass(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "no", "such", "dir", "catalog.sqlite")
	missing := filepath.Dir(path)

	// Default locker: the sidecar create fails first; busy is impossible.
	_, err := Open(path, OpenReadWrite)
	var busy *CatalogBusy
	if errors.As(err, &busy) {
		t.Fatalf("mutating open with missing parent returned the busy class %v; "+
			"the sidecar create must fail KindOpen before flock(2) is reached", err)
	}
	var cerr *CatalogError
	if !errors.As(err, &cerr) || cerr.Kind != KindOpen {
		t.Fatalf("Open(missing dir, read-write) = %v, want a KindOpen CatalogError", err)
	}
	if !strings.Contains(err.Error(), "advisory lock file") {
		t.Errorf("mutating open did not fail at the sidecar create: %v", err)
	}
	if !strings.Contains(err.Error(), "catalog directory does not exist: "+missing) {
		t.Errorf("lock-site failure not enriched with the missing directory: %v", err)
	}

	// Lock layer bypassed (injected locker succeeds unconditionally): the
	// os.Stat gate catches the missing parent BEFORE sql.Open/Ping, so the
	// message is the stat-site diagnosis ("not found or unreadable"),
	// enriched - never the raw sqlite "unable to open database file (14)"
	// the stakeholder originally hit.
	_, err = Open(path, OpenReadWrite, WithLocker(&recordingLocker{}))
	if !errors.As(err, &cerr) || cerr.Kind != KindOpen {
		t.Fatalf("Open(missing dir, injected locker) = %v, want a KindOpen CatalogError", err)
	}
	if !strings.Contains(err.Error(), "not found or unreadable") {
		t.Errorf("bypassing the locker did not fall through to the stat gate: %v", err)
	}
	if !strings.Contains(err.Error(), "catalog directory does not exist: "+missing) {
		t.Errorf("stat-site failure not enriched with the missing directory: %v", err)
	}
}

// TestParentIsFileNotDirectory pins the CURRENT behavior for a catalog
// path whose parent component is a regular FILE (the mkdir-shaped
// ENOTDIR failure): it is NOT a missing directory, so the D2 enrichment
// must never claim "catalog directory does not exist". All three entry
// points fail KindOpen (exit 4) with a "not a directory" message:
//
//   - Create: refused at its pre-create os.Stat (ENOTDIR is not
//     os.IsNotExist, so the stat-error branch fires);
//   - Open read-write: the flock sidecar open(2) fails ENOTDIR;
//   - Open read-only: the os.Stat gate reports it inside the
//     "not found or unreadable" diagnosis.
func TestParentIsFileNotDirectory(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "blocker-file")
	if err := os.WriteFile(blocker, []byte("i am a file, not a directory"), 0o644); err != nil {
		t.Fatalf("seed blocker file: %v", err)
	}
	path := filepath.Join(blocker, "catalog.sqlite")

	notEnriched := func(t *testing.T, label string, err error) {
		t.Helper()
		var cerr *CatalogError
		if !errors.As(err, &cerr) || cerr.Kind != KindOpen {
			t.Fatalf("%s = %v, want a KindOpen CatalogError", label, err)
		}
		if strings.Contains(err.Error(), "catalog directory does not exist") {
			t.Errorf("%s wrongly blamed a missing directory (the parent EXISTS, as a file): %v", label, err)
		}
		if !strings.Contains(err.Error(), "not a directory") {
			t.Errorf("%s lost the not-a-directory diagnosis: %v", label, err)
		}
	}

	if err := Create(path); err == nil {
		t.Error("Create succeeded with a FILE as the parent directory, want a KindOpen error")
	} else {
		notEnriched(t, "Create(parent=file)", err)
	}
	if _, err := Open(path, OpenReadWrite); err == nil {
		t.Error("Open(parent=file, read-write) succeeded, want a KindOpen error")
	} else {
		notEnriched(t, "Open(parent=file, read-write)", err)
	}
	if _, err := Open(path, OpenReadOnly); err == nil {
		t.Error("Open(parent=file, read-only) succeeded, want a KindOpen error")
	} else {
		notEnriched(t, "Open(parent=file, read-only)", err)
	}
}

// TestEnrichTOCTOUBestEffort pins the documented best-effort contract
// of the enrichment under directory churn between the ORIGINAL failure
// and the enrichment's own os.Stat (migrate.go: the stat at
// message-build time is the only evidence the helper trusts):
//
//   - the directory vanished AFTER an unrelated KindOpen failure (the
//     stat gate saw it, then a later step failed and the dir was
//     deleted before the enrichment stat): the message IS enriched -
//     best-effort, slightly wrong about the cause - but the class stays
//     KindOpen and the original cause stays errors.Is-reachable through
//     the %w wrap; nothing panics;
//   - the directory REAPPEARED between the failure and the enrichment
//     stat: the original message stands untouched; nothing panics.
func TestEnrichTOCTOUBestEffort(t *testing.T) {
	base := t.TempDir()

	// Case A: dir disappears after the failure was constructed.
	dirA := filepath.Join(base, "churn-a")
	if err := os.Mkdir(dirA, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dirA, err)
	}
	pathA := filepath.Join(dirA, "cat.db")
	causeA := errors.New("unrelated kindopen failure that happened while the dir still existed")
	origA := &CatalogError{Kind: KindOpen, Path: pathA, Err: causeA}
	if err := os.RemoveAll(dirA); err != nil {
		t.Fatalf("remove %s: %v", dirA, err)
	}
	gotA := enrichMissingParentDir(pathA, origA) // best-effort: no panic is the contract
	var cerr *CatalogError
	if !errors.As(gotA, &cerr) || cerr.Kind != KindOpen {
		t.Fatalf("TOCTOU (dir vanished) result = %v, want KindOpen (class preserved)", gotA)
	}
	if !strings.Contains(gotA.Error(), "catalog directory does not exist: "+dirA) {
		t.Errorf("TOCTOU (dir vanished) not enriched with the directory name: %v", gotA)
	}
	if !errors.Is(gotA, causeA) {
		t.Errorf("TOCTOU (dir vanished) lost the original cause from the errors.Is chain: %v", gotA)
	}

	// Case B: dir reappears after a missing-parent failure was observed.
	dirB := filepath.Join(base, "churn-b")
	pathB := filepath.Join(dirB, "cat.db")
	causeB := errors.New("kindopen failure observed while the dir was missing")
	origB := &CatalogError{Kind: KindOpen, Path: pathB, Err: causeB}
	if err := os.Mkdir(dirB, 0o755); err != nil { // reappeared before the enrichment stat
		t.Fatalf("mkdir %s: %v", dirB, err)
	}
	gotB := enrichMissingParentDir(pathB, origB)
	if gotB != origB {
		t.Errorf("TOCTOU (dir reappeared) mutated the error:\n got: %v\nwant: the identical value", gotB)
	}
}

// TestMissingParentEnrichmentKeepsErrChainSurvivesClassification drives
// the REAL Open entry points in both modes on a missing-parent path and
// asserts everything the CLI layer's report.Classify depends on (the
// cli-side mirror of this lives in internal/cli/tilde_gaps_test.go):
// the enriched error still errors.As to *CatalogError KindOpen (which
// Classify maps to exit 4, DESIGN_SPEC 10.2) and still errors.Is to
// fs.ErrNotExist through the %w-preserved errno chain.
func TestMissingParentEnrichmentKeepsErrChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "dir", "catalog.sqlite")
	missing := filepath.Dir(path)

	for _, mode := range []OpenMode{OpenReadWrite, OpenReadOnly} {
		_, err := Open(path, mode)
		if err == nil {
			t.Fatalf("Open(missing dir, mode %d) succeeded, want an error", mode)
		}
		var cerr *CatalogError
		if !errors.As(err, &cerr) || cerr.Kind != KindOpen {
			t.Errorf("Open(missing dir, mode %d) = %v, want KindOpen CatalogError (exit 4 class)", mode, err)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Open(missing dir, mode %d) lost fs.ErrNotExist in the enriched chain: %v", mode, err)
		}
		if !strings.Contains(err.Error(), "catalog directory does not exist: "+missing) {
			t.Errorf("Open(missing dir, mode %d) does not name the missing directory: %v", mode, err)
		}
	}

	// Create (scan's auto-create path): same class and enrichment; its
	// sqlite-side cause (SQLITE_CANTOPEN errno 14) is NOT an fs.ErrNotExist
	// wrap - the chain guarantee here is the KindOpen class only, which is
	// what Classify needs for exit 4.
	err := Create(path)
	var cerr *CatalogError
	if !errors.As(err, &cerr) || cerr.Kind != KindOpen {
		t.Fatalf("Create(missing dir) = %v, want KindOpen CatalogError (exit 4 class)", err)
	}
	if !strings.Contains(err.Error(), "catalog directory does not exist: "+missing) {
		t.Errorf("Create(missing dir) does not name the missing directory: %v", err)
	}
}
