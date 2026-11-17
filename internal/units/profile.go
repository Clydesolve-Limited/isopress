package units

import (
	"errors"
	"fmt"
	"math"
)

// Media profile names (DESIGN_SPEC 4.1, capacity semantics D-06).
const (
	// ProfileBDRSingleLayer is a 25 GB single-layer BD-R.
	ProfileBDRSingleLayer = "bd-r-sl"
	// ProfileBDRDualLayer is a 50 GB dual-layer BD-R.
	ProfileBDRDualLayer = "bd-r-dl"
	// ProfileDisk is a disk or NAS target: usable is 98 percent of nominal.
	ProfileDisk = "disk"
)

// DefaultProfile is the --media-profile default used by the cli layer
// (DESIGN_SPEC 4.1: "applied by --media-profile (default disk)").
const DefaultProfile = ProfileDisk

// Fixed nominal and usable figures for the optical profiles
// (DESIGN_SPEC 4.1; IMPLEMENTATION_PLAN T0.2).
const (
	bdRSLNominalBytes int64 = 25_000_000_000
	bdRSLUsableBytes  int64 = 23_000_000_000
	bdRDLNominalBytes int64 = 50_000_000_000
	bdRDLUsableBytes  int64 = 46_000_000_000

	// diskUsablePercent is the disk profile margin: usable = 98% of nominal.
	diskUsablePercent int64 = 98
)

// ErrUnknownProfile classifies an unknown media-profile name; the cli layer
// maps it (errors.Is) to a usage error, exit 2 (DESIGN_SPEC 10.1).
var ErrUnknownProfile = errors.New("unknown media profile")

// Profile is the resolved capacity of one homogeneous destination medium.
type Profile struct {
	Name         string // canonical profile name
	NominalBytes int64  // nominal capacity in bytes as stated by the user
	UsableBytes  int64  // capacity after the profile margin (D-06)
}

// ResolveProfile resolves a media profile against a nominal size in bytes
// (DESIGN_SPEC 4.1, D-06).
//
// The margin is applied exactly once, here; every downstream consumer
// (planner, build pre-check, capacity hints) must use only UsableBytes and
// never re-apply a margin. NominalBytes is carried through for reporting so
// the plan report can echo nominal and usable side by side (D-06: the margin
// is never silent).
//
// Profile behavior:
//
//   - "bd-r-sl" and "bd-r-dl" have fixed figures (25 GB nominal / 23 GB
//     usable and 50 GB / 46 GB). A nominalBytes <= 0 selects the profile
//     default; any other value must equal the profile's fixed nominal or
//     ResolveProfile fails (wrapping ErrInvalidSize).
//   - "disk" requires a positive nominalBytes; usable is 98 percent of it.
//
// Unknown names fail wrapping ErrUnknownProfile.
func ResolveProfile(name string, nominalBytes int64) (Profile, error) {
	switch name {
	case ProfileBDRSingleLayer:
		return resolveFixedProfile(name, nominalBytes, bdRSLNominalBytes, bdRSLUsableBytes)
	case ProfileBDRDualLayer:
		return resolveFixedProfile(name, nominalBytes, bdRDLNominalBytes, bdRDLUsableBytes)
	case ProfileDisk:
		if nominalBytes <= 0 {
			return Profile{}, fmt.Errorf("units: disk profile requires a positive nominal size: %w", ErrInvalidSize)
		}
		if nominalBytes > math.MaxInt64/diskUsablePercent {
			return Profile{}, fmt.Errorf("units: nominal size %d bytes overflows int64 at profile resolution: %w",
				nominalBytes, ErrInvalidSize)
		}
		return Profile{
			Name:         name,
			NominalBytes: nominalBytes,
			UsableBytes:  nominalBytes * diskUsablePercent / 100,
		}, nil
	default:
		return Profile{}, fmt.Errorf("%w: %q (allowed: %s, %s, %s)",
			ErrUnknownProfile, name, ProfileBDRSingleLayer, ProfileBDRDualLayer, ProfileDisk)
	}
}

func resolveFixedProfile(name string, requested, nominal, usable int64) (Profile, error) {
	if requested > 0 && requested != nominal {
		return Profile{}, fmt.Errorf("units: profile %q has fixed nominal size %d bytes, got %d: %w",
			name, nominal, requested, ErrInvalidSize)
	}
	return Profile{Name: name, NominalBytes: nominal, UsableBytes: usable}, nil
}
