package fsmeta

// Change-detection mode resolution (DESIGN_SPEC 3.5, RESOLVED 2026-10-20;
// the stakeholder CIFS failure). This file owns the once-per-run
// resolution of the RESOLVED COMPARISON SET that DiffFields consumes:
//
//   - the filesystem-class rule: statfs the source root (stdlib
//     syscall.Statfs, reading the f_type magic - the DESIGN_SPEC 11
//     gate forbids direct golang.org/x/sys imports) and, when the
//     source sits on the CIFS family, degrade the set to size+mtime_ns;
//   - the --change-detection override (auto | full | metadata);
//   - the Mode value that threads the resolved set through the three
//     detection points (ingest, copy, staging) of 3.5.
//
// WHY THE DEGRADATION EXISTS (the 2026-10-20 incident): CIFS clients
// SYNTHESIZE inode and dev numbers that are not contractually stable
// across SMB sessions; two scans of an unchanged tree recorded
// wholesale-different inodes, so the full set flagged EVERY file
// "source changed (inode)". The relaxation removes only the EARLY SKIP,
// never the content guarantee: read-back verify (FR-014) and the
// whole-file restore hash (FR-041) remain the content gates (3.5
// "safety of the relaxation").
//
// RUNTIME DERIVATION (3.5): the mode is resolved from the CURRENT
// filesystem at the source root at run start - never from a stored
// scan-time class - so each of scan/copy/build resolves its own Mode
// once per run and threads it uniformly to all detection points that
// run participates in. The resolution and the audit figures (mode,
// class, provenance) are recorded into runs.stats_json by the cli layer
// (informational only; they never drive comparison).

import (
	"errors"
	"fmt"
	"syscall"
)

// ComparisonSet names the RESOLVED change-detection comparison set of
// DESIGN_SPEC 3.5 (the fields fsmeta.DiffFields compares).
type ComparisonSet int

const (
	// ComparisonFull is the original D-10 set: size + mtime_ns + dev +
	// inode. It assumes the source filesystem provides STABLE dev and
	// inode identifiers across sessions (true for local filesystems).
	//
	// THE ZERO VALUE OF Mode IS ComparisonFull - the SAFE default: the
	// full set never weakens detection, so any code path that forgets
	// to thread a resolved mode keeps the strongest comparison (the
	// copyengine/imagebuild/ingest test matrices rely on this: a zero
	// Mode behaves exactly like the pre-amendment tool).
	ComparisonFull ComparisonSet = iota
	// ComparisonMetadata is the degraded set: size + mtime_ns ONLY.
	// dev and inode differences are IGNORED - both are synthesized on
	// the CIFS family and neither is contractually stable (3.5: dev is
	// dropped too, not just inode, because its observed stability on
	// CIFS is incidental, not guaranteed).
	ComparisonMetadata
)

// String renders the set name ("full" / "metadata") for reports,
// warnings, and runs.stats_json audit records.
func (s ComparisonSet) String() string {
	switch s {
	case ComparisonMetadata:
		return "metadata"
	default:
		return "full"
	}
}

// The --change-detection flag vocabulary (DESIGN_SPEC 3.5). The flag is
// GLOBAL (registered on the root command) and resolved once per run
// against the source root.
const (
	// FlagAuto follows the filesystem-class rule (the default).
	FlagAuto = "auto"
	// FlagFull forces the full set regardless of detection: the user
	// asserts the source provides stable identifiers.
	FlagFull = "full"
	// FlagMetadata forces size+mtime_ns regardless of detection: the
	// escape hatch for filesystems the detector does not know (an
	// unstable FUSE mount, a network fs with an unrecognized magic).
	FlagMetadata = "metadata"
)

// ErrInvalidChangeFlag is the sentinel an invalid --change-detection
// value wraps: the cli maps it (via errors.Is) to a report.UsageError,
// which classifies to exit 2 (DESIGN_SPEC 10.1 usage class).
var ErrInvalidChangeFlag = errors.New("fsmeta: invalid --change-detection value")

// The filesystem-class strings recorded for AUDIT (runs.stats_json,
// warning messages). They name what the detector saw; only "cifs" and
// "smb3" ever drive the auto degradation.
const (
	ClassCIFS    = "cifs"    // CIFS_MAGIC_NUMBER
	ClassSMB3    = "smb3"    // SMB2_SUPER_MAGIC (modern SMB3 mounts)
	ClassLocal   = "local"   // stable local filesystems (ext4/xfs/btrfs/tmpfs)
	ClassNFS     = "nfs"     // NFS_SUPER_MAGIC: NOT auto-degraded (stable filehandles)
	ClassFUSE    = "fuse"    // FUSE_SUPER_MAGIC: NOT auto-degraded (too heterogeneous)
	ClassOther   = "other"   // recognized as a filesystem, not a known class
	ClassUnknown = "unknown" // statfs failed; class could not be determined
)

// The statfs f_type magic numbers (Linux UAPI; the values are 32-bit,
// so the production seam truncates st.Type to its low 32 bits - see
// statfsMagic).
const (
	magicCIFS  uint64 = 0xFF534D42 // CIFS_MAGIC_NUMBER
	magicSMB3  uint64 = 0xFE534D42 // SMB2_SUPER_MAGIC
	magicExt   uint64 = 0x0000EF53 // EXT2_SUPER_MAGIC (ext2/3/4)
	magicXFS   uint64 = 0x58465342 // XFS_SUPER_MAGIC
	magicBtrfs uint64 = 0x9123683E // BTRFS_SUPER_MAGIC
	magicTmpfs uint64 = 0x01021994 // TMPFS_MAGIC
	magicNFS   uint64 = 0x00006969 // NFS_SUPER_MAGIC
	magicFUSE  uint64 = 0x65735546 // FUSE_SUPER_MAGIC
)

// Mode is the RESOLVED once-per-run change-detection mode of
// DESIGN_SPEC 3.5: the comparison set the three detection points share
// (single-pinned invariant: one DiffFields, one resolved set), plus the
// audit figures. It is resolved by ResolveChangeDetection at run start
// from the source root and threaded - as ONE value - into ingest
// (post-hash compare), copyengine (5.1 step 1/7), and imagebuild
// staging (3.5 point 3).
//
// The zero value is the SAFE mode: the full comparison set, class
// unknown, auto provenance - i.e. exactly the pre-amendment behavior.
type Mode struct {
	// Set is the resolved comparison field set (see ComparisonFull /
	// ComparisonMetadata).
	Set ComparisonSet
	// Class is the DETECTED filesystem class of the source root
	// ("cifs", "smb3", "local", "nfs", "fuse", "other", or "unknown"
	// when statfs failed). AUDIT ONLY: it never drives the comparison
	// - Set does - and is recorded into runs.stats_json and named in
	// the 3.5 degradation warning.
	Class string
	// Forced reports whether Set came from an explicit
	// --change-detection value (full/metadata) instead of the auto
	// filesystem-class rule. The distinction matters for the 3.5
	// warning: an auto-detected degradation is announced once per run,
	// while a USER-FORCED metadata mode is the user's own choice and
	// stays silent.
	Forced bool
}

// ModeFull is the full-set mode (size + mtime_ns + dev + inode). It
// equals the zero value of Mode - the SAFE default documented on
// ComparisonFull - and exists so tests and explicit call sites read
// intent rather than {}.
var ModeFull = Mode{Set: ComparisonFull}

// SetName renders the resolved set name ("full" / "metadata").
func (m Mode) SetName() string { return m.Set.String() }

// ProvenanceString renders how the set was resolved: "forced" (an
// explicit --change-detection value) or "detected" (the auto
// filesystem-class rule).
func (m Mode) ProvenanceString() string {
	if m.Forced {
		return "forced"
	}
	return "detected"
}

// DegradedViaDetection reports whether this mode is the 3.5
// WARNING case: metadata set via AUTO DETECTION (the warning informs an
// automatic degradation). A user-forced metadata mode is NOT warned
// about - forcing is the user's explicit choice - and full modes never
// degrade.
func (m Mode) DegradedViaDetection() bool {
	return m.Set == ComparisonMetadata && !m.Forced
}

// statfsMagic reads the f_type magic of the filesystem holding path.
// Production implementation: stdlib syscall.Statfs (DESIGN_SPEC 11
// gate: no direct golang.org/x/sys imports; the imagebuild free-space
// pre-check uses the same stdlib Statfs). The magic numbers are 32-bit
// UAPI values, so the int64 kernel field is truncated to its low 32
// bits - correct on every Linux arch, where f_type carries the magic in
// that width.
//
// SEAM: resolveChangeDetection takes this function as a parameter, so
// the resolver's decision table is testable against FAKE filesystem
// classes (a CIFS magic can be injected without a CIFS mount); the
// exported ResolveChangeDetection binds the production implementation.
func statfsMagic(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(uint32(st.Type)), nil
}

// ResolveChangeDetection resolves the run's change-detection Mode from
// sourceRoot per the DESIGN_SPEC 3.5 rules (see the file doc):
//
//   - flag "auto" (or ""): statfs the root; CIFS-family magic ->
//     metadata set, everything else -> full. A statfs FAILURE falls
//     back to the FULL set with class "unknown" (the safe default:
//     full never weakens detection, and a scan of an unreadable root
//     fails downstream anyway - see the fallback note below);
//   - flag "full" / "metadata": the set is FORCED regardless of
//     detection (the user asserts knowledge the detector lacks); the
//     class is still statfs'd for the audit record, best-effort;
//   - any other flag value: ErrInvalidChangeFlag (the cli wraps it in
//     a report.UsageError, exit 2).
//
// THE STATFS-FAILURE FALLBACK (documented decision): on auto, an
// unreadable source root degrades the resolver to the FULL set rather
// than erroring. Rationale: the full set is the pre-amendment behavior
// and can never falsely SKIP a stable file; and the very next pipeline
// step (the walk / the inventory read) fails loudly on an unreadable
// root regardless, so erroring here would only trade one honest failure
// message for another. The audit class records "unknown" so the
// fallback is visible in runs.stats_json.
func ResolveChangeDetection(sourceRoot, flag string) (Mode, error) {
	return resolveChangeDetection(statfsMagic, sourceRoot, flag)
}

// resolveChangeDetection is the injectable-core form of
// ResolveChangeDetection: magic is the statfs seam (production:
// statfsMagic). Tests substitute fakes to pin the decision table
// against every filesystem class without mounting anything.
func resolveChangeDetection(magic func(path string) (uint64, error), sourceRoot, flag string) (Mode, error) {
	switch flag {
	case "", FlagAuto:
		// The filesystem-class rule below. "" is treated as the
		// documented default (auto) for robustness; the cli layer
		// always passes the parsed flag value, defaulting to
		// FlagAuto.
		m, err := magic(sourceRoot)
		if err != nil {
			// Documented fallback: full set, unknown class (see the
			// ResolveChangeDetection doc).
			return Mode{Set: ComparisonFull, Class: ClassUnknown}, nil
		}
		class := className(m)
		if class == ClassCIFS || class == ClassSMB3 {
			return Mode{Set: ComparisonMetadata, Class: class}, nil
		}
		// Local (ext4/xfs/btrfs/tmpfs), NFS, FUSE, and unrecognized
		// magics all keep the FULL set: only the CIFS family is
		// auto-degraded (3.5; FUSE is too heterogeneous and NFS
		// identifiers derive from stable filehandles).
		return Mode{Set: ComparisonFull, Class: class}, nil
	case FlagFull:
		return Mode{Set: ComparisonFull, Class: detectClass(magic, sourceRoot), Forced: true}, nil
	case FlagMetadata:
		return Mode{Set: ComparisonMetadata, Class: detectClass(magic, sourceRoot), Forced: true}, nil
	default:
		return Mode{}, fmt.Errorf(
			"fsmeta: --change-detection value %q is not one of %s, %s, %s: %w",
			flag, FlagAuto, FlagFull, FlagMetadata, ErrInvalidChangeFlag)
	}
}

// detectClass statfs'd the root for the AUDIT record of a FORCED mode:
// best-effort, never fatal (the set is forced either way); a failure
// yields "unknown".
func detectClass(magic func(path string) (uint64, error), sourceRoot string) string {
	m, err := magic(sourceRoot)
	if err != nil {
		return ClassUnknown
	}
	return className(m)
}

// className maps a statfs f_type magic to the audit class string.
func className(magic uint64) string {
	switch magic {
	case magicCIFS:
		return ClassCIFS
	case magicSMB3:
		return ClassSMB3
	case magicExt, magicXFS, magicBtrfs, magicTmpfs:
		return ClassLocal
	case magicNFS:
		return ClassNFS
	case magicFUSE:
		return ClassFUSE
	default:
		return ClassOther
	}
}
