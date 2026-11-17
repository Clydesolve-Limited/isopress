package units

import "fmt"

// FormatBytes renders a byte count with one decimal place for the plan report
// and the plain progress renderer (DESIGN_SPEC 9.3).
//
// Rule (documented judgment call; the DESIGN_SPEC examples fix the shape but
// not the full unit ladder): the largest unit at which the count is >= 1 is
// chosen, with binary GiB at and above 1 GiB and decimal GB/MB/KB below it.
// Values below 1024 bytes render as whole bytes. Examples from
// DESIGN_SPEC 9.3: 4.2 GiB and 23.0 GiB appear verbatim.
//
//	>= 1 GiB        -> "N.N GiB"   (divisor 2^30)
//	>= 1 GB         -> "N.N GB"    (divisor 10^9)
//	>= 1 MB         -> "N.N MB"    (divisor 10^6)
//	>= 1 KB         -> "N.N KB"    (divisor 10^3)
//	else            -> "N B"
//
// The input is expected to be a non-negative byte count; negative values are
// formatted by the same thresholds without special handling.
func FormatBytes(bytes int64) string {
	switch {
	case bytes >= BytesPerGiB:
		return fmt.Sprintf("%.1f GiB", float64(bytes)/float64(BytesPerGiB))
	case bytes >= BytesPerGB:
		return fmt.Sprintf("%.1f GB", float64(bytes)/float64(BytesPerGB))
	case bytes >= 1_000_000:
		return fmt.Sprintf("%.1f MB", float64(bytes)/1_000_000)
	case bytes >= 1_000:
		return fmt.Sprintf("%.1f KB", float64(bytes)/1_000)
	default:
		return fmt.Sprintf("%d B", bytes)
	}
}
