package xorriso

// The typed xorriso invocation builder (T4.1, DESIGN_SPEC 6.2 xorriso
// invocation strategy).
//
// Injection posture, fixed here and asserted by tests: this package never
// runs a shell. Every value in an Invocation becomes exactly ONE argv
// element (exec.CommandContext direct exec in run.go), so volume ids or
// paths containing shell metacharacters ("; rm -rf /", "$(touch pwned)",
// quotes, spaces, newlines) are inert data that xorriso receives verbatim
// as a single argument. There is no string join, no quoting step, and no
// /bin/sh anywhere in the package; the structural deps test pins this.

import (
	"fmt"
	"strconv"
	"strings"
)

// IsoLevel is the ISO 9660 level passed to the mkisofs emulation, FIXED
// at 3 by DESIGN_SPEC 6.2. The emulation's default is level 1, which
// caps single files at 4 GiB; level 3 enables the multi-extent files
// larger than 4 GiB the design requires. It is a constant, not a field:
// letting callers lower it would silently re-introduce the 4 GiB cap.
const IsoLevel = 3

// Invocation is the typed shape of the DESIGN_SPEC 6.2 arguments
// template for one image build:
//
//	xorriso -as mkisofs
//	  -iso-level 3                       # fixed, see IsoLevel
//	  [-J]                               # Joliet tree (6.2), opt-in
//	  -V <VolumeID>                      # volume id <SET-ID>-<NNN> (6.4)
//	  -o <OutputPath>                    # the .partial path (6.2, 6.3)
//	  -graft-points
//	  -path-list <PathListPath>          # read by xorriso FROM DISK
//
// Rock Ridge stays ON by default in xorrisofs emulation, so this builder
// deliberately never emits --norock, -r (would rationalize ownership to
// 0), or -f/--follow-links (symlinks must stay symlinks); DESIGN_SPEC
// 6.2 names all three as forbidden.
//
// Contract with the caller: fields are validated for NON-EMPTINESS, for
// NUL bytes, and (for PathListPath only) against the bare value "-",
// which xorriso would treat as READ-FROM-STDIN for -path-list (see
// validate). Truncating the set id to the 32-character ISO 9660
// volume-id limit is the CALLER's responsibility (DESIGN_SPEC 6.4,
// implemented by the imagebuild service - since the 2026-11-04
// amendment that value is "<SET-ID>-<NNN>", with the set-id portion
// shortened so the suffix survives), not this builder's: by the
// time an Invocation exists, VolumeID is already the final, at most
// 32-byte value. The builder passing a > 32-char id straight through is
// intentional (xorriso truncates and warns); silently truncating here
// would duplicate 6.4 logic in two places.
//
// The pathlist file is READ BY XORRISO from disk; the argv carries only
// its path. That is what keeps large trees off the operating system's
// argv limit (6.2's stated reason for the -path-list design).
type Invocation struct {
	// VolumeID is the -V value: the FINAL volume id, already shaped
	// and shortened to at most 32 characters by the caller per 6.4 -
	// imagebuild's volumeID renders it as "<SET-ID>-<NNN>" (the same
	// <NNN> as the image file name) with number-preserving truncation.
	VolumeID string
	// OutputPath is the -o value: the destination image path with the
	// .partial suffix (atomic finalize per 6.3 renames it afterwards).
	OutputPath string
	// PathListPath is the -path-list value: the path of the
	// graft-points pathlist file ("<iso path>=<staging path>" lines).
	// The bare value "-" is rejected by validate: xorriso would read
	// the pathlist from STDIN instead of from disk (see validate).
	PathListPath string
	// Joliet adds -J (Joliet directory tree for Windows browsing,
	// DESIGN_SPEC 6.2). Rock Ridge is unaffected by this flag.
	Joliet bool
}

// Argv builds the complete argument vector that follows the binary name
// for one mastering run, per the 6.2 template. The result is passed to
// Tool.Run verbatim; each element reaches xorriso as one argv entry.
func (inv Invocation) Argv() ([]string, error) {
	if err := inv.validate(); err != nil {
		return nil, err
	}
	argv := []string{"-as", "mkisofs", "-iso-level", strconv.Itoa(IsoLevel)}
	if inv.Joliet {
		argv = append(argv, "-J")
	}
	argv = append(argv,
		"-V", inv.VolumeID,
		"-o", inv.OutputPath,
		"-graft-points",
		"-path-list", inv.PathListPath,
	)
	return argv, nil
}

// validate enforces the builder's field contract (non-empty, no NUL
// bytes, and PathListPath not the bare "-"). The errors are
// programmer-error class: an Invocation reaching a service with an
// empty required field - or with the stdin sentinel in the field that
// must name a real on-disk file - is a wiring bug, and the message says
// which field and why it matters.
func (inv Invocation) validate() error {
	for _, f := range []struct{ name, value string }{
		{"VolumeID", inv.VolumeID},
		{"OutputPath", inv.OutputPath},
		{"PathListPath", inv.PathListPath},
	} {
		if f.value == "" {
			return fmt.Errorf("xorriso: Invocation.%s is empty; DESIGN_SPEC 6.2 requires it on every build (32-char VolumeID truncation is the caller's 6.4 contract)", f.name)
		}
		if strings.ContainsRune(f.value, 0) {
			return fmt.Errorf("xorriso: Invocation.%s contains a NUL byte, which no argv element may carry", f.name)
		}
	}
	// HARDENING (QA follow-up): a bare "-" as -path-list makes xorriso
	// READ THE PATHLIST FROM STDIN instead of from disk (xorriso's
	// "- means stdin" convention). DESIGN_SPEC 6.2 never intends that:
	// imagebuild always writes a real graft-points pathlist file and
	// passes its path, so a "-" here is always a wiring bug that would
	// otherwise hang the build on a blocking stdin read or swallow
	// unrelated input. VolumeID and OutputPath deliberately do NOT get
	// this check: "-" is a legal filename (and set-id) byte there.
	if inv.PathListPath == "-" {
		return fmt.Errorf("xorriso: Invocation.PathListPath is \"-\", which xorriso treats as READ-FROM-STDIN for -path-list; DESIGN_SPEC 6.2 always passes a real pathlist file path, so \"-\" is a wiring bug, never a valid value")
	}
	return nil
}
