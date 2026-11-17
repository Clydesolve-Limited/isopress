package units_test

import (
	"errors"
	"math"
	"testing"

	"isopress/internal/units"
)

func TestParseSize(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    int64
		wantErr bool
	}{
		// Valid forms (DESIGN_SPEC 4.1).
		{name: "bare integer means decimal GB", input: "25", want: 25_000_000_000},
		{name: "Gi suffix", input: "25Gi", want: 26_843_545_600},
		{name: "GB suffix", input: "50GB", want: 50_000_000_000},
		{name: "GiB suffix", input: "46GiB", want: 49_392_123_904},
		{name: "lowercase gb", input: "128gb", want: 128_000_000_000},
		{name: "lowercase gib", input: "1gib", want: 1 << 30},
		{name: "uppercase bare with G", input: "1G", want: 1_000_000_000},
		{name: "uppercase GI", input: "2GI", want: 2 << 30},
		{name: "surrounding whitespace trimmed", input: "  25 GB  ", want: 25_000_000_000},
		{name: "space before suffix", input: "25 Gi", want: 26_843_545_600},
		{name: "mixed case suffix", input: "25Gb", want: 25_000_000_000},
		{name: "minimal positive", input: "1", want: 1_000_000_000},

		// Malformed forms are usage-class errors (DESIGN_SPEC 10.1).
		{name: "empty string", input: "", wantErr: true},
		{name: "whitespace only", input: "   ", wantErr: true},
		{name: "non-numeric", input: "abc", wantErr: true},
		{name: "negative", input: "-5", wantErr: true},
		{name: "zero", input: "0", wantErr: true},
		{name: "unknown suffix TB", input: "25TB", wantErr: true},
		{name: "malformed suffix G3", input: "25G3", wantErr: true},
		{name: "fractional not allowed", input: "25.5GB", wantErr: true},
		{name: "suffix only", input: "GiB", wantErr: true},
		{name: "zero with unit", input: "0GiB", wantErr: true},
		{name: "negative with unit", input: "-5GB", wantErr: true},
		{name: "int64 overflow", input: "9223372036854775807", wantErr: true},

		// Case-insensitivity across both unit families (DESIGN_SPEC 4.1:
		// the suffix is case-insensitive, so G/GB/Gi/GiB each accept any
		// capitalization).
		{name: "lowercase g", input: "25g", want: 25_000_000_000},
		{name: "lowercase gb suffix", input: "25gb", want: 25_000_000_000},
		{name: "mixed-case Gb", input: "25Gb", want: 25_000_000_000},
		{name: "mixed-case gB", input: "25gB", want: 25_000_000_000},
		{name: "all-caps GB", input: "25GB", want: 25_000_000_000},
		{name: "mixed-case Gi", input: "25gI", want: 26_843_545_600},
		{name: "mixed-case GiB", input: "25GiB", want: 26_843_545_600},
		{name: "all-caps GIB", input: "25GIB", want: 26_843_545_600},

		// Whitespace tolerance: surrounding and between number and suffix.
		{name: "tab surrounded", input: "\t25Gi\t", want: 26_843_545_600},
		{name: "tab before suffix", input: "25\tGi", want: 26_843_545_600},
		{name: "bare with whitespace", input: "  25  ", want: 25_000_000_000},
		{name: "multi-space before suffix", input: "25   GB", want: 25_000_000_000},

		// Numeric-form edge cases.
		{name: "explicit plus sign", input: "+25", want: 25_000_000_000},
		{name: "leading zeros", input: "025Gi", want: 26_843_545_600},

		// int64 overflow boundaries, decimal family: the largest whole GB
		// value that fits in int64 bytes is 9,223,372,036 (9.22e18 bytes).
		{name: "max valid decimal GB", input: "9223372036", want: 9_223_372_036_000_000_000},
		{name: "one past max decimal GB", input: "9223372037", wantErr: true},
		// int64 overflow boundaries, binary family: the largest whole GiB
		// value that fits in int64 bytes is 8,589,934,591.
		{name: "max valid GiB", input: "8589934591Gi", want: 9_223_372_035_781_033_984},
		{name: "one past max GiB", input: "8589934592GiB", wantErr: true},

		// Further malformed forms.
		{name: "double plus sign", input: "++25", wantErr: true},
		{name: "trailing plus", input: "25+", wantErr: true},
		{name: "leading dot fraction", input: ".5GB", wantErr: true},
		{name: "unsupported unit family MiB", input: "25MiB", wantErr: true},
		{name: "unsupported unit mib lowercase", input: "25mib", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := units.ParseSize(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseSize(%q) = %d, want error", tt.input, got)
				}
				if !errors.Is(err, units.ErrInvalidSize) {
					t.Fatalf("ParseSize(%q) error %v does not wrap units.ErrInvalidSize", tt.input, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseSize(%q) unexpected error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Fatalf("ParseSize(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestResolveProfile(t *testing.T) {
	tests := []struct {
		name         string
		profile      string
		nominalBytes int64
		wantNominal  int64
		wantUsable   int64
		wantErr      bool
		wantErrIs    error
	}{
		{
			name:         "bd-r-sl with explicit nominal",
			profile:      units.ProfileBDRSingleLayer,
			nominalBytes: 25_000_000_000,
			wantNominal:  25_000_000_000,
			wantUsable:   23_000_000_000,
		},
		{
			name:         "bd-r-sl default nominal when size omitted",
			profile:      units.ProfileBDRSingleLayer,
			nominalBytes: 0,
			wantNominal:  25_000_000_000,
			wantUsable:   23_000_000_000,
		},
		{
			name:         "bd-r-dl with explicit nominal",
			profile:      units.ProfileBDRDualLayer,
			nominalBytes: 50_000_000_000,
			wantNominal:  50_000_000_000,
			wantUsable:   46_000_000_000,
		},
		{
			name:         "bd-r-dl default nominal when size omitted",
			profile:      units.ProfileBDRDualLayer,
			nominalBytes: -1,
			wantNominal:  50_000_000_000,
			wantUsable:   46_000_000_000,
		},
		{
			name:         "disk 128 GB nominal yields 98 percent",
			profile:      units.ProfileDisk,
			nominalBytes: 128_000_000_000,
			wantNominal:  128_000_000_000,
			wantUsable:   125_440_000_000,
		},
		{
			name:         "disk 25 GB nominal",
			profile:      units.ProfileDisk,
			nominalBytes: 25_000_000_000,
			wantNominal:  25_000_000_000,
			wantUsable:   24_500_000_000,
		},
		{
			// 25Gi nominal (a binary-sized input) under the disk profile:
			// usable = 26,843,545,600 * 98 / 100.
			name:         "disk with binary nominal from ParseSize",
			profile:      units.ProfileDisk,
			nominalBytes: 26_843_545_600,
			wantNominal:  26_843_545_600,
			wantUsable:   26_306_674_688,
		},
		{
			// Small nominal: locks the documented integer-truncation
			// semantics of "usable = 98 percent of nominal" (usable is
			// nominal * 98 / 100 in integer arithmetic).
			name:         "disk small nominal truncates toward zero",
			profile:      units.ProfileDisk,
			nominalBytes: 3,
			wantNominal:  3,
			wantUsable:   2,
		},

		// Error paths.
		{
			name:      "unknown profile rejected",
			profile:   "tape-lto9",
			wantErr:   true,
			wantErrIs: units.ErrUnknownProfile,
		},
		{
			name:      "empty profile name rejected",
			profile:   "",
			wantErr:   true,
			wantErrIs: units.ErrUnknownProfile,
		},
		{
			name:      "disk requires positive nominal",
			profile:   units.ProfileDisk,
			wantErr:   true,
			wantErrIs: units.ErrInvalidSize,
		},
		{
			name:         "bd-r-sl rejects a mismatched nominal",
			profile:      units.ProfileBDRSingleLayer,
			nominalBytes: 50_000_000_000,
			wantErr:      true,
			wantErrIs:    units.ErrInvalidSize,
		},
		{
			name:         "bd-r-dl rejects a mismatched nominal",
			profile:      units.ProfileBDRDualLayer,
			nominalBytes: 25_000_000_000,
			wantErr:      true,
			wantErrIs:    units.ErrInvalidSize,
		},
		{
			// A mismatched nominal expressed via the Gi family is rejected
			// identically: 25Gi bytes (26,843,545,600) is not the fixed
			// 25 GB (25,000,000,000) nominal of bd-r-sl.
			name:         "bd-r-sl rejects a Gi-sized nominal",
			profile:      units.ProfileBDRSingleLayer,
			nominalBytes: 26_843_545_600,
			wantErr:      true,
			wantErrIs:    units.ErrInvalidSize,
		},
		{
			name:         "disk rejects a negative nominal",
			profile:      units.ProfileDisk,
			nominalBytes: -5,
			wantErr:      true,
			wantErrIs:    units.ErrInvalidSize,
		},
		{
			// Usable = nominal * 98 / 100 must not overflow int64 during
			// resolution; the largest nominal that fits is MaxInt64/98.
			name:         "disk rejects a nominal that overflows the margin",
			profile:      units.ProfileDisk,
			nominalBytes: math.MaxInt64,
			wantErr:      true,
			wantErrIs:    units.ErrInvalidSize,
		},
		{
			// Profile names are exact, case-sensitive matches
			// (characterization of the current contract; DESIGN_SPEC 4.1
			// lists the canonical lowercase names only).
			name:      "profile names are case-sensitive",
			profile:   "Disk",
			wantErr:   true,
			wantErrIs: units.ErrUnknownProfile,
		},
		{
			name:         "bd-r-sl treats a negative nominal as omitted",
			profile:      units.ProfileBDRSingleLayer,
			nominalBytes: -1,
			wantNominal:  25_000_000_000,
			wantUsable:   23_000_000_000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := units.ResolveProfile(tt.profile, tt.nominalBytes)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ResolveProfile(%q, %d) = %+v, want error", tt.profile, tt.nominalBytes, got)
				}
				if !errors.Is(err, tt.wantErrIs) {
					t.Fatalf("ResolveProfile(%q, %d) error %v does not wrap %v", tt.profile, tt.nominalBytes, err, tt.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveProfile(%q, %d) unexpected error: %v", tt.profile, tt.nominalBytes, err)
			}
			if got.Name != tt.profile {
				t.Errorf("Name = %q, want %q", got.Name, tt.profile)
			}
			if got.NominalBytes != tt.wantNominal {
				t.Errorf("NominalBytes = %d, want %d", got.NominalBytes, tt.wantNominal)
			}
			if got.UsableBytes != tt.wantUsable {
				t.Errorf("UsableBytes = %d, want %d", got.UsableBytes, tt.wantUsable)
			}
		})
	}
}

func TestDefaultProfileConstant(t *testing.T) {
	if units.DefaultProfile != "disk" {
		t.Fatalf("DefaultProfile = %q, want %q", units.DefaultProfile, "disk")
	}
	// The default must resolve without error.
	if _, err := units.ResolveProfile(units.DefaultProfile, 128_000_000_000); err != nil {
		t.Fatalf("ResolveProfile(DefaultProfile, ...) unexpected error: %v", err)
	}
}

func TestFormatBytes(t *testing.T) {
	const bytesPerGiB = int64(1) << 30

	tests := []struct {
		name  string
		bytes int64
		want  string
	}{
		// Exact strings from the DESIGN_SPEC 9.3 plain-line examples.
		{name: "design example 4.2 GiB", bytes: 4_509_715_660, want: "4.2 GiB"},
		{name: "design example 23.0 GiB", bytes: 23 * bytesPerGiB, want: "23.0 GiB"},

		{name: "exactly one GiB", bytes: bytesPerGiB, want: "1.0 GiB"},
		{name: "just below one GiB uses GB", bytes: bytesPerGiB - 1, want: "1.1 GB"},
		{name: "just below one GB uses MB", bytes: 999_999_999, want: "1000.0 MB"},
		{name: "GB band below one GiB", bytes: 1_070_000_000, want: "1.1 GB"},
		{name: "exactly one GB", bytes: 1_000_000_000, want: "1.0 GB"},
		{name: "megabytes", bytes: 500_000_000, want: "500.0 MB"},
		{name: "exactly one MB", bytes: 1_000_000, want: "1.0 MB"},
		{name: "kilobytes", bytes: 4_096, want: "4.1 KB"},
		{name: "zero bytes", bytes: 0, want: "0 B"},
		{name: "sub-kilobyte", bytes: 512, want: "512 B"},
		{name: "large multi-TiB value stays in GiB per the rule", bytes: 3 * (bytesPerGiB << 10), want: "3072.0 GiB"},

		// Threshold boundaries for every unit band.
		{name: "exactly one KB", bytes: 1_000, want: "1.0 KB"},
		{name: "1023 bytes is one KB", bytes: 1_023, want: "1.0 KB"},
		{name: "1024 bytes is one KB (KB band spans to 1 MB)", bytes: 1_024, want: "1.0 KB"},
		{name: "999 bytes stays in whole bytes", bytes: 999, want: "999 B"},
		{name: "just below one KB", bytes: 999, want: "999 B"},
		{name: "just below one MB rounds up to 1000.0 KB", bytes: 999_999, want: "1000.0 KB"},
		{name: "exactly two GiB", bytes: 2 * bytesPerGiB, want: "2.0 GiB"},
		{name: "usable figure of bd-r-sl formats as GiB", bytes: 23_000_000_000, want: "21.4 GiB"},
		{name: "max int64 formats without overflow", bytes: math.MaxInt64, want: "8589934592.0 GiB"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := units.FormatBytes(tt.bytes); got != tt.want {
				t.Fatalf("FormatBytes(%d) = %q, want %q", tt.bytes, got, tt.want)
			}
		})
	}
}

// TestParseSizeResolveProfileRoundTrip exercises the real consumer flow
// (DESIGN_SPEC 4.1, IMPLEMENTATION_PLAN T0.5/T2.3): a user-supplied size
// string is parsed to bytes and fed into profile resolution. The optical
// profiles accept their own nominal expressions in either unit family and
// reject everything else.
func TestParseSizeResolveProfileRoundTrip(t *testing.T) {
	tests := []struct {
		name        string
		size        string
		profile     string
		wantNominal int64
		wantUsable  int64
		wantErrIs   error
	}{
		{
			name:        "bare 25 resolves against bd-r-sl",
			size:        "25",
			profile:     units.ProfileBDRSingleLayer,
			wantNominal: 25_000_000_000,
			wantUsable:  23_000_000_000,
		},
		{
			name:        "25GB resolves against bd-r-sl",
			size:        "25GB",
			profile:     units.ProfileBDRSingleLayer,
			wantNominal: 25_000_000_000,
			wantUsable:  23_000_000_000,
		},
		{
			name:        "50GB resolves against bd-r-dl",
			size:        "50GB",
			profile:     units.ProfileBDRDualLayer,
			wantNominal: 50_000_000_000,
			wantUsable:  46_000_000_000,
		},
		{
			// 25Gi is 26,843,545,600 bytes, which conflicts with the fixed
			// 25,000,000,000-byte nominal of bd-r-sl: the margin must never be
			// silently reinterpreted across unit families.
			name:      "25Gi conflicts with the bd-r-sl fixed nominal",
			size:      "25Gi",
			profile:   units.ProfileBDRSingleLayer,
			wantErrIs: units.ErrInvalidSize,
		},
		{
			name:        "128 resolves against the disk profile at 98 percent",
			size:        "128",
			profile:     units.ProfileDisk,
			wantNominal: 128_000_000_000,
			wantUsable:  125_440_000_000,
		},
		{
			name:        "25Gi resolves against the disk profile",
			size:        "25Gi",
			profile:     units.ProfileDisk,
			wantNominal: 26_843_545_600,
			wantUsable:  26_306_674_688,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nominal, err := units.ParseSize(tt.size)
			if err != nil {
				t.Fatalf("ParseSize(%q) unexpected error: %v", tt.size, err)
			}
			got, err := units.ResolveProfile(tt.profile, nominal)
			if tt.wantErrIs != nil {
				if err == nil {
					t.Fatalf("ResolveProfile(%q, %d) = %+v, want error", tt.profile, nominal, got)
				}
				if !errors.Is(err, tt.wantErrIs) {
					t.Fatalf("ResolveProfile(%q, %d) error %v does not wrap %v", tt.profile, nominal, err, tt.wantErrIs)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveProfile(%q, %d) unexpected error: %v", tt.profile, nominal, err)
			}
			if got.NominalBytes != tt.wantNominal {
				t.Errorf("NominalBytes = %d, want %d", got.NominalBytes, tt.wantNominal)
			}
			if got.UsableBytes != tt.wantUsable {
				t.Errorf("UsableBytes = %d, want %d", got.UsableBytes, tt.wantUsable)
			}
		})
	}
}

// TestProfileFiguresFormatForReport locks the exact nominal/usable display
// strings the plan report will echo side by side so the profile margin is
// never silent (DESIGN_SPEC 4.1 D-06, IMPLEMENTATION_PLAN T2.3).
func TestProfileFiguresFormatForReport(t *testing.T) {
	tests := []struct {
		name        string
		profile     string
		nominal     int64 // 0 = profile default
		wantNominal string
		wantUsable  string
	}{
		{
			name:        "bd-r-sl default",
			profile:     units.ProfileBDRSingleLayer,
			wantNominal: "23.3 GiB",
			wantUsable:  "21.4 GiB",
		},
		{
			name:        "bd-r-dl default",
			profile:     units.ProfileBDRDualLayer,
			wantNominal: "46.6 GiB",
			wantUsable:  "42.8 GiB",
		},
		{
			name:        "disk 128 GB",
			profile:     units.ProfileDisk,
			nominal:     128_000_000_000,
			wantNominal: "119.2 GiB",
			wantUsable:  "116.8 GiB",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := units.ResolveProfile(tt.profile, tt.nominal)
			if err != nil {
				t.Fatalf("ResolveProfile(%q, %d) unexpected error: %v", tt.profile, tt.nominal, err)
			}
			if got := units.FormatBytes(p.NominalBytes); got != tt.wantNominal {
				t.Errorf("FormatBytes(NominalBytes=%d) = %q, want %q", p.NominalBytes, got, tt.wantNominal)
			}
			if got := units.FormatBytes(p.UsableBytes); got != tt.wantUsable {
				t.Errorf("FormatBytes(UsableBytes=%d) = %q, want %q", p.UsableBytes, got, tt.wantUsable)
			}
		})
	}
}
