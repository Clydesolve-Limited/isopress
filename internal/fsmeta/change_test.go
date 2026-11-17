package fsmeta

// Change-detection resolution tests (DESIGN_SPEC 3.5, the 2026-10-20
// filesystem-class-aware amendment; the architect's pinned safety
// matrix (a) and (b)):
//
//   - (a) FAKE FILESYSTEM-CLASS INJECTION: the resolver takes an
//     injectable statfs seam (resolveChangeDetection's magic
//     parameter), so every class decision is pinned without mounting a
//     CIFS share; production uses stdlib syscall.Statfs (also pinned
//     below through the exported entry point + the AST import gate).
//   - (b) both CIFS-family magics degrade to the metadata set; ext4 /
//     NFS / FUSE (and other local/unrecognized magics) do NOT; the
//     --change-detection flag overrides in all directions; an invalid
//     flag value returns ErrInvalidChangeFlag (the cli maps it to
//     report.UsageError -> exit 2).
//
// Plus the DiffFields mode pins: the metadata set ignores dev+inode
// differences entirely (the CIFS incident's false positives) while
// still firing on size/mtime_ns - asserted EXPLICITLY so a future
// refactor cannot silently widen or narrow either mode's field set.

import (
	"errors"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

// fakeMagic returns a statfs seam answering with the given magic; err
// simulates an unreadable source root.
func fakeMagic(magic uint64, err error) func(string) (uint64, error) {
	return func(string) (uint64, error) { return magic, err }
}

// TestResolveChangeDetectionDecisionTable pins the WHOLE decision table
// through the injected seam: both CIFS magics degrade (metadata),
// ext4/NFS/FUSE/other do not (full), the flag overrides in both
// directions, and provenance/class are recorded for the audit.
func TestResolveChangeDetectionDecisionTable(t *testing.T) {
	cases := []struct {
		name      string
		magic     uint64
		magicErr  error
		flag      string
		wantSet   ComparisonSet
		wantClass string
		wantForce bool
	}{
		// auto + detection: only the CIFS family degrades.
		{"auto cifs", magicCIFS, nil, FlagAuto, ComparisonMetadata, ClassCIFS, false},
		{"auto smb3", magicSMB3, nil, FlagAuto, ComparisonMetadata, ClassSMB3, false},
		{"auto ext4", magicExt, nil, FlagAuto, ComparisonFull, ClassLocal, false},
		{"auto xfs", magicXFS, nil, FlagAuto, ComparisonFull, ClassLocal, false},
		{"auto btrfs", magicBtrfs, nil, FlagAuto, ComparisonFull, ClassLocal, false},
		{"auto tmpfs", magicTmpfs, nil, FlagAuto, ComparisonFull, ClassLocal, false},
		{"auto nfs NOT degraded", magicNFS, nil, FlagAuto, ComparisonFull, ClassNFS, false},
		{"auto fuse NOT degraded", magicFUSE, nil, FlagAuto, ComparisonFull, ClassFUSE, false},
		{"auto unknown magic full", 0x1234ABCD, nil, FlagAuto, ComparisonFull, ClassOther, false},
		// auto + statfs failure: the documented FULL fallback.
		{"auto statfs error falls back to full", 0, os.ErrPermission, FlagAuto, ComparisonFull, ClassUnknown, false},
		// "" is the documented default (auto).
		{"empty flag is auto", magicCIFS, nil, "", ComparisonMetadata, ClassCIFS, false},
		// Forced flags override detection in BOTH directions.
		{"forced full on cifs", magicCIFS, nil, FlagFull, ComparisonFull, ClassCIFS, true},
		{"forced metadata on ext4", magicExt, nil, FlagMetadata, ComparisonMetadata, ClassLocal, true},
		{"forced metadata on statfs error", 0, os.ErrPermission, FlagMetadata, ComparisonMetadata, ClassUnknown, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, err := resolveChangeDetection(fakeMagic(tc.magic, tc.magicErr), "/src", tc.flag)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if m.Set != tc.wantSet {
				t.Errorf("Set = %v, want %v", m.Set, tc.wantSet)
			}
			if m.Class != tc.wantClass {
				t.Errorf("Class = %q, want %q", m.Class, tc.wantClass)
			}
			if m.Forced != tc.wantForce {
				t.Errorf("Forced = %v, want %v", m.Forced, tc.wantForce)
			}
		})
	}
}

// TestResolveChangeDetectionInvalidFlag pins the usage-class sentinel:
// a garbage flag value returns ErrInvalidChangeFlag (discoverable via
// errors.Is; the cli wraps it in report.UsageError -> exit 2) and a
// zero Mode.
func TestResolveChangeDetectionInvalidFlag(t *testing.T) {
	for _, flag := range []string{"Auto", "AUTO", "relaxed", "none", "full-ish", "0"} {
		m, err := resolveChangeDetection(fakeMagic(magicCIFS, nil), "/src", flag)
		if !errors.Is(err, ErrInvalidChangeFlag) {
			t.Errorf("flag %q: err = %v, want ErrInvalidChangeFlag", flag, err)
		} else if !strings.Contains(err.Error(), flag) {
			t.Errorf("flag %q: error does not name the offending value: %v", flag, err)
		}
		if m != (Mode{}) {
			t.Errorf("flag %q: Mode = %+v, want the zero Mode", flag, m)
		}
	}
}

// TestResolveChangeDetectionRealFilesystem exercises the EXPORTED entry
// point with the PRODUCTION statfs seam against the real filesystem
// (t.TempDir is a local fs in every test environment): no error, a
// determined (non-empty, non-unknown) class, and - the safety pin -
// the FULL set, because no local filesystem class degrades.
func TestResolveChangeDetectionRealFilesystem(t *testing.T) {
	dir := t.TempDir()
	m, err := ResolveChangeDetection(dir, FlagAuto)
	if err != nil {
		t.Fatalf("ResolveChangeDetection(%q): %v", dir, err)
	}
	if m.Set != ComparisonFull {
		t.Errorf("real-filesystem auto mode = %v, want ComparisonFull (only the CIFS family auto-degrades)", m.Set)
	}
	if m.Class == "" || m.Class == ClassUnknown {
		t.Errorf("real-filesystem class = %q, want a determined class", m.Class)
	}
	if m.Forced {
		t.Error("real-filesystem auto mode reports Forced")
	}
}

// TestModeZeroValueIsFullPinsSafeDefault pins the load-bearing
// zero-value contract: Mode{} is the FULL comparison set with detected
// provenance - the pre-amendment behavior - so every pipeline that is
// handed no mode (or a test that constructs none) keeps the STRONGEST
// comparison and can never silently weaken detection.
func TestModeZeroValueIsFullPinsSafeDefault(t *testing.T) {
	var zero Mode
	if zero.Set != ComparisonFull {
		t.Errorf("zero Mode.Set = %v, want ComparisonFull", zero.Set)
	}
	if zero.DegradedViaDetection() {
		t.Error("zero Mode reports DegradedViaDetection; only metadata-via-auto does")
	}
	if zero.ProvenanceString() != "detected" {
		t.Errorf("zero Mode provenance = %q, want %q", zero.ProvenanceString(), "detected")
	}
	// ModeFull exists as the readable spelling of the same thing.
	if ModeFull != zero {
		t.Errorf("ModeFull = %+v, want the zero Mode %+v", ModeFull, zero)
	}
}

// TestDegradedViaDetectionWarningPredicate pins the 3.5 warning rule:
// the warning fires ONLY for metadata-via-DETECTION (auto followed a
// CIFS-class detection); a user-FORCED metadata mode is their choice
// and stays silent; full modes never degrade.
func TestDegradedViaDetectionWarningPredicate(t *testing.T) {
	detectedMeta := Mode{Set: ComparisonMetadata, Class: ClassCIFS}
	forcedMeta := Mode{Set: ComparisonMetadata, Class: ClassCIFS, Forced: true}
	full := Mode{Set: ComparisonFull, Class: ClassLocal}
	if !detectedMeta.DegradedViaDetection() {
		t.Error("auto-detected metadata mode must be the warning case")
	}
	if forcedMeta.DegradedViaDetection() {
		t.Error("forced metadata mode must NOT warn (the user chose it)")
	}
	if full.DegradedViaDetection() {
		t.Error("full mode must NOT warn")
	}
}

// --- DiffFields mode pins (both field sets, asserted explicitly) ------------

// TestDiffFieldsMetadataModeIgnoresDevAndInode pins the degraded set
// precisely: on a metadata-mode source (CIFS family) dev AND inode
// differences are IGNORED - the 2026-10-20 incident's false-positive
// fields - while size and mtime_ns STILL fire. The full mode is pinned
// alongside so both sets live in ONE test.
func TestDiffFieldsMetadataModeIgnoresDevAndInode(t *testing.T) {
	base := Meta{Size: 100, MtimeNS: 1_000_000_000, Dev: 7, Inode: 99}
	meta := Mode{Set: ComparisonMetadata}

	// The CIFS incident shape: same size+mtime_ns, different dev+inode
	// (synthesized identifiers) -> NO diff in metadata mode, DIFF in
	// full mode.
	replaced := Meta{Size: 100, MtimeNS: 1_000_000_000, Dev: 8, Inode: 1_796_385}
	if d := DiffFields(base, replaced, meta); d != 0 {
		t.Errorf("metadata mode: dev+inode-only diff = %v, want 0 (synthesized identifiers are ignored, 3.5)", d)
	}
	if d := DiffFields(base, replaced, ModeFull); d != (DiffDev | DiffInode) {
		t.Errorf("full mode: dev+inode-only diff = %v, want %v", d, DiffDev|DiffInode)
	}
	if Changed(base, replaced, meta) {
		t.Error("metadata mode Changed = true for dev+inode-only differences, want false")
	}

	// size still fires in metadata mode (with and without dev noise).
	bigger := Meta{Size: 101, MtimeNS: 1_000_000_000, Dev: 8, Inode: 5}
	if d := DiffFields(base, bigger, meta); d != DiffSize {
		t.Errorf("metadata mode: size diff = %v, want %v", d, DiffSize)
	}
	// mtime_ns still fires in metadata mode.
	touched := Meta{Size: 100, MtimeNS: 2_000_000_000, Dev: 8, Inode: 5}
	if d := DiffFields(base, touched, meta); d != DiffMtimeNS {
		t.Errorf("metadata mode: mtime diff = %v, want %v", d, DiffMtimeNS)
	}
	// The combined metadata-mode diff is exactly size+mtime_ns even when
	// dev and inode ALSO differ - the set can never name them.
	both := Meta{Size: 101, MtimeNS: 2_000_000_000, Dev: 999, Inode: 999}
	if d := DiffFields(base, both, meta); d != (DiffSize | DiffMtimeNS) {
		t.Errorf("metadata mode: combined diff = %v, want %v (dev/inode must never appear)", d, DiffSize|DiffMtimeNS)
	}
	// Unchanged: zero in both modes.
	if d := DiffFields(base, base, meta); d != 0 {
		t.Errorf("metadata mode: identical metas = %v, want 0", d)
	}
}

// --- dependency gate (DESIGN_SPEC 11) ---------------------------------------

// No import of golang.org/x/sys/unix anywhere in the package: the
// change-detection resolver reads the statfs magic through stdlib
// syscall.Statfs (the same gate imagebuild's free-space pre-check and
// catalog's flock already pass). The check parses the real import
// declarations of every file in the package (comments naming the ban
// are fine).
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
