package units

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Byte-count constants for the two unit families accepted by ParseSize
// (DESIGN_SPEC 4.1).
const (
	// BytesPerGB is one decimal gigabyte: 10^9 bytes.
	BytesPerGB int64 = 1_000_000_000
	// BytesPerGiB is one binary gibibyte: 2^30 bytes.
	BytesPerGiB int64 = 1 << 30
)

// ErrInvalidSize classifies every size-parsing or size-argument failure as a
// usage error: the cli layer maps errors matching it (errors.Is) to exit 2
// (DESIGN_SPEC 10.1 UsageError).
var ErrInvalidSize = errors.New("invalid size")

// ParseSize parses a user-supplied size string into bytes per DESIGN_SPEC 4.1.
//
// Accepted forms (case-insensitive suffix, surrounding whitespace trimmed,
// optional whitespace between number and suffix):
//
//	"25",   "25G",  "25GB"  -> 25 * 10^9 bytes (decimal gigabytes)
//	"25Gi", "25GiB"        -> 25 * 2^30 bytes (binary gibibytes)
//
// A bare integer with no unit suffix is interpreted as decimal GB, matching
// how sizes are phrased in the requirements ("25gb", "128gb"). The numeric
// part must be a positive whole number; fractions ("25.5GB"), zero, negative
// values, empty input, and unknown suffixes ("25TB", "25G3") are rejected.
//
// All failures wrap ErrInvalidSize so callers can classify them as usage
// errors via errors.Is.
func ParseSize(s string) (int64, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("units: empty size string: %w", ErrInvalidSize)
	}

	// Split the leading integer (with optional sign) from the unit suffix.
	i := 0
	for i < len(t) && (t[i] == '+' || t[i] == '-' || (t[i] >= '0' && t[i] <= '9')) {
		i++
	}
	numStr, suffix := t[:i], strings.ToLower(strings.TrimSpace(t[i:]))

	value, err := strconv.ParseInt(numStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("units: size %q is not a whole number of bytes: %w", s, ErrInvalidSize)
	}

	var multiplier int64
	switch suffix {
	case "", "g", "gb":
		multiplier = BytesPerGB
	case "gi", "gib":
		multiplier = BytesPerGiB
	default:
		return 0, fmt.Errorf("units: size %q has unsupported unit %q (allowed: G, GB, Gi, GiB; a bare number means GB): %w",
			s, suffix, ErrInvalidSize)
	}

	if value <= 0 {
		return 0, fmt.Errorf("units: size %q must be a positive whole number: %w", s, ErrInvalidSize)
	}
	if value > math.MaxInt64/multiplier {
		return 0, fmt.Errorf("units: size %q overflows int64 bytes: %w", s, ErrInvalidSize)
	}
	return value * multiplier, nil
}
