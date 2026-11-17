package imagebuild

// The -V volume id contract (6.4, AMENDED 2026-11-04 by the
// stakeholder): "<SET-ID>-<NNN>", the same <NNN> the image FILE NAME
// carries, capped at volumeIDMax BYTES with the suffix always
// surviving (number-preserving truncation). Pure string contract: no
// xorriso, no fixture, no store.

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"isopress/internal/media"
)

// TestVolumeIDAppendsImageNumber pins the shape for ids that fit: the
// suffix is "-%03d" (3-digit minimum width, exactly like the file
// name) and nothing else changes.
func TestVolumeIDAppendsImageNumber(t *testing.T) {
	for _, tc := range []struct {
		setID string
		n     int64
		want  string
	}{
		{"manga-20261020", 1, "manga-20261020-001"},
		{"S", 1, "S-001"},
		{"S", 9, "S-009"},
		{"S", 42, "S-042"},
		{"fixture-20261020", 2, "fixture-20261020-002"},
		// %03d is a MINIMUM width: past 999 the number grows and the
		// label keeps it (n=1000 -> "-1000", never "-000").
		{"S", 999, "S-999"},
		{"S", 1000, "S-1000"},
		{"S", 1001, "S-1001"},
	} {
		got := volumeID(tc.setID, tc.n)
		if got != tc.want {
			t.Errorf("volumeID(%q, %d) = %q, want %q", tc.setID, tc.n, got, tc.want)
		}
		if len(got) > volumeIDMax {
			t.Errorf("volumeID(%q, %d) = %q is %d bytes, over the %d-byte limit",
				tc.setID, tc.n, got, len(got), volumeIDMax)
		}
	}
}

// TestVolumeIDMaxIsTheLiteral32 is the CONSTANT guard the length
// assertions above cannot give: every other test in this file derives its
// expectations FROM volumeIDMax, so a wrong constant (64, say) would pass
// all of them - the truncation math would happily cap ids at 64 bytes,
// and xorriso would then slice the tail of a >32-byte "-V" value down to
// the ISO 9660 limit itself, silently undoing the suffix-preserving
// contract. ISO 9660 fixes the volume id at 32 bytes (ECMA-119 6.4,
// "truncated to 32 characters"), and the 2026-11-04 amendment pinned
// this project's cap to exactly that number - asserted here as a
// LITERAL so no derivation can mask a change.
func TestVolumeIDMaxIsTheLiteral32(t *testing.T) {
	const iso9660VolumeIDBytes = 32
	if volumeIDMax != iso9660VolumeIDBytes {
		t.Errorf("volumeIDMax = %d, want the literal %d (ISO 9660 volume-id limit, ECMA-119 6.4; 2026-11-04 amendment)",
			volumeIDMax, iso9660VolumeIDBytes)
	}
}

// TestVolumeIDTruncationKeepsSuffix pins the byte cap at the
// boundary: an id whose suffix lands EXACTLY on 32 bytes is untouched,
// one byte over shortens only the SET-ID portion, and the result is
// always exactly at (never over) volumeIDMax with the suffix intact.
func TestVolumeIDTruncationKeepsSuffix(t *testing.T) {
	const suffix = "-001"
	for _, setIDLen := range []int{
		volumeIDMax - len(suffix),     // 28: fits exactly, no cut
		volumeIDMax - len(suffix) + 1, // 29: one set-id byte goes
		volumeIDMax,                   // 32: the pre-amendment boundary
		volumeIDMax + 8,               // 40: well past the limit
		volumeIDMax + 200,             // absurd: still deterministic
		1, 0,
	} {
		setID := strings.Repeat("L", setIDLen)
		got := volumeID(setID, 1)
		wantLen := setIDLen + len(suffix)
		if wantLen > volumeIDMax {
			wantLen = volumeIDMax
		}
		if len(got) != wantLen {
			t.Errorf("volumeID(%d-byte id, 1) = %q (%d bytes), want %d bytes",
				setIDLen, got, len(got), wantLen)
		}
		if !strings.HasSuffix(got, suffix) {
			t.Errorf("volumeID(%d-byte id, 1) = %q, want the %q suffix to survive",
				setIDLen, got, suffix)
		}
		if len(setID)+len(suffix) <= volumeIDMax && got != setID+suffix {
			t.Errorf("volumeID(%q, 1) = %q, want the id verbatim plus the suffix", setID, got)
		}
	}
}

// TestVolumeIDLongSetIDKeepsNumber is THE amendment's reason to
// exist: a naive head-truncate would slice "-001" off a long set id
// and put every disc of the set back under one label. The number must
// survive, in exactly volumeIDMax bytes, for both the 3-digit and the
// 4-digit (past-999) suffixes.
func TestVolumeIDLongSetIDKeepsNumber(t *testing.T) {
	long := strings.Repeat("manga-set-20261020-extralong", 3) // 87 bytes
	for _, n := range []int64{1, 999, 1000} {
		got := volumeID(long, n)
		if len(got) != volumeIDMax {
			t.Errorf("volumeID(long, %d) = %q (%d bytes), want exactly %d bytes",
				n, got, len(got), volumeIDMax)
		}
		suffix := fmt.Sprintf("-%03d", n)
		if !strings.HasSuffix(got, suffix) {
			t.Errorf("volumeID(long, %d) = %q, want it to END with %q (the number must survive)",
				n, got, suffix)
		}
		if strings.TrimSuffix(got, suffix) == "" {
			t.Errorf("volumeID(long, %d) = %q carries no set-id portion at all", n, got)
		}
	}
}

// TestVolumeIDMatchesFileNamingSuffix pins FILE-NAME PARITY: for every
// n the label's suffix is byte-identical to the suffix of the image
// file name media.ImageISOFileName renders (<SET-ID>-NNN.iso), for a
// short id (no cut) AND for a long id (set-id portion cut, suffix
// untouched). One naming scheme, one implementation of "-%03d".
func TestVolumeIDMatchesFileNamingSuffix(t *testing.T) {
	for _, setID := range []string{"S", "manga-20261020", strings.Repeat("L", 40)} {
		for _, n := range []int64{1, 9, 99, 999, 1000, 1001} {
			name, err := media.ImageISOFileName(setID, n)
			if err != nil {
				t.Fatalf("ImageISOFileName(%q, %d): %v", setID, n, err)
			}
			// "<SET-ID>-NNN.iso" -> the file-name suffix "-NNN".
			fileSuffix := strings.TrimSuffix(strings.TrimPrefix(name, setID), ".iso")
			got := volumeID(setID, n)
			if !strings.HasSuffix(got, fileSuffix) {
				t.Errorf("volumeID(%q, %d) = %q does not carry the file-name suffix %q (%q)",
					setID, n, got, fileSuffix, name)
			}
			if len(got) > volumeIDMax {
				t.Errorf("volumeID(%q, %d) = %q is %d bytes, over the limit",
					setID, n, got, len(got))
			}
		}
	}
}

// TestVolumeIDEmptySetID documents the honest edge: an empty id would
// render as just "-001" (the suffix still attaches - the old "empty
// stays empty" contract does NOT carry over, because the amendment
// appends unconditionally). It is UNREACHABLE in the pipeline:
// validateBuildInput refuses the empty set id at step 0 through
// media.ImageISOFileName -> media.validateSetID, so no build can ever
// pass "" here.
func TestVolumeIDEmptySetID(t *testing.T) {
	if got := volumeID("", 1); got != "-001" {
		t.Errorf("volumeID(\"\", 1) = %q, want %q (suffix still attaches)", got, "-001")
	}
	if got := volumeID("", 1000); got != "-1000" {
		t.Errorf("volumeID(\"\", 1000) = %q, want %q", got, "-1000")
	}
	// The unreachable-in-pipeline claim is pinned here so a future
	// change to the step-0 gates fails loudly instead of letting an
	// empty id master an ISO.
	if err := validateBuildInput(BuildInput{SetID: "", ImageID: 1, SourceRoot: "/src",
		DestDir: "/dest", UsableSize: 1, Export: EmbeddedExport{Path: "/e", Size: 1}}); err == nil {
		t.Error("validateBuildInput accepted an empty set id; volumeID's \"unreachable\" contract no longer holds")
	}
}

// TestBuildVolumeIDCarriesTheImagesOwnNumber pins the CALL SITE
// (build.go step 6), which the pure-function tests above cannot see:
// every other fixture in this package builds IMAGE 1, so a call site
// hardcoded to volumeID(in.SetID, 1) would pass the ENTIRE suite -
// argv pins, the pure volume-id tests, and the real PVD read-back -
// while mislabelling disc 2 (and disc 1000) of every set, which is
// exactly the multi-disc ambiguity the 2026-11-04 amendment exists to
// remove. Two numbers: the first multi-disc image and the 4-digit
// boundary, both through the REAL Build pipeline over the fakes (no
// xorriso needed - the argv is what xorriso would be exec'd with).
// The -V suffix and the -o file name must carry the SAME number.
func TestBuildVolumeIDCarriesTheImagesOwnNumber(t *testing.T) {
	for _, n := range []int64{2, 1000} {
		t.Run(fmt.Sprintf("image %d", n), func(t *testing.T) {
			f := newBuildFixture(t)
			f.in.ImageID = n
			// The manifest seed's placement record names its own
			// image number too (6.6: every image's manifest carries
			// the whole-set placement), so it moves with the id -
			// otherwise buildPlacement refuses the mismatch and the
			// test would exercise nothing.
			f.in.Manifest.Placement[0].ImageNumber = n
			f.refreshManifest(t)
			f.extract.result = f.happyEntries()

			if _, err := Build(context.Background(), f.deps(), f.in); err != nil {
				t.Fatalf("Build (image %d): %v", n, err)
			}

			f.master.mu.Lock()
			argv := append([]string(nil), f.master.argvs[0]...)
			f.master.mu.Unlock()

			// The file name's own suffix for this image - the label
			// must agree with it (one naming scheme, one "%03d").
			wantBase, err := media.ImageISOFileName(f.in.SetID, n)
			if err != nil {
				t.Fatalf("ImageISOFileName(%q, %d): %v", f.in.SetID, n, err)
			}
			wantV := strings.TrimSuffix(wantBase, ".iso")

			if i := indexOf(argv, "-V"); i < 0 || i+1 >= len(argv) {
				t.Fatalf("argv %v lacks -V", argv)
			} else if argv[i+1] != wantV {
				t.Errorf("-V = %q, want %q (the label must carry THIS image's number, not a constant)",
					argv[i+1], wantV)
			}
			wantO := filepath.Join(f.in.DestDir, wantBase) + ".partial"
			if i := indexOf(argv, "-o"); i < 0 || i+1 >= len(argv) {
				t.Fatalf("argv %v lacks -o", argv)
			} else if argv[i+1] != wantO {
				t.Errorf("-o = %q, want %q (the output name must agree with the label)", argv[i+1], wantO)
			}
		})
	}
}
