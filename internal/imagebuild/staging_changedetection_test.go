package imagebuild

// Change-detection mode threading (DESIGN_SPEC 3.5, RESOLVED 2026-10-20;
// detection point 3, staging). THE PINS:
//
//   - the run's resolved fsmeta.Mode (StageInput.Mode, threaded by
//     BuildInput.Mode from the cli) drives the 3.5 point 3 comparison:
//     a metadata-mode staging does NOT fail on dev/inode drift (the
//     CIFS synthesized-identifier shape) while the full mode does;
//   - the content guarantee is independent of the identifier
//     relaxation: a REPLACED source (same size + mtime_ns, different
//     bytes) on a metadata-mode source FAILS staging at the read-back
//     verify (the catalog hash is the anchor) and masters no stale
//     bytes - the matrix's (c) pin at the build detection point.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"isopress/internal/fsmeta"
)

// TestStageMetadataModeIgnoresSynthesizedIdentifiers pins the point-3
// relaxation: the SAME drifted catalog snapshot (new dev+inode, same
// size/mtime_ns - the CIFS cross-session shape) fails staging under
// the full set and stages cleanly under the metadata set.
func TestStageMetadataModeIgnoresSynthesizedIdentifiers(t *testing.T) {
	for _, tc := range []struct {
		name       string
		mode       fsmeta.Mode
		wantChange bool
	}{
		{"full set flags the drift", fsmeta.ModeFull, true},
		{"metadata set ignores the drift", fsmeta.Mode{Set: fsmeta.ComparisonMetadata, Class: fsmeta.ClassCIFS}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := newSourceRoot(t)
			writeSrcFile(t, root, "iso-member.bin", []byte("member payload"), 0o644, 1_700_000_000_123_456_789)
			in := simpleStageInput(t, root, t.TempDir(), "iso-member.bin",
				[]byte("member payload"), 1_700_000_000_123_456_789)
			// The catalog snapshot remembers the previous SMB session's
			// synthesized identifiers; size and mtime_ns still match.
			in.Inventory.Files[0].Dev++
			in.Inventory.Files[0].Inode += 1_000_000
			in.Mode = tc.mode

			st, err := Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace}, in)
			if tc.wantChange {
				var sc *SourceChangedError
				if !errors.As(err, &sc) {
					t.Fatalf("full mode: err = %v, want *SourceChangedError", err)
				}
				if sc.Diff != (fsmeta.DiffDev | fsmeta.DiffInode) {
					t.Errorf("full mode: diff = %v, want dev+inode", sc.Diff)
				}
				return
			}
			if err != nil {
				t.Fatalf("metadata mode: Stage = %v, want success (synthesized identifiers are ignored)", err)
			}
			defer st.Cleanup() //nolint:errcheck // scratch
			got, rerr := os.ReadFile(filepath.Join(st.Root, "files", "iso-member.bin"))
			if rerr != nil {
				t.Fatalf("reading staged member: %v", rerr)
			}
			if string(got) != "member payload" {
				t.Errorf("staged member = %q, want the source bytes", got)
			}
		})
	}
}

// TestStageMetadataModeVerifyStillGatesContent is the matrix's (c) pin
// at detection point 3: a replaced source (same size, restored mtime,
// different bytes) passes the DEGRADED compare, but staging's
// read-back verify (anchor = the catalog whole-file hash, FR-014's
// staging twin) refuses the bytes: the build fails, no stale bytes are
// mastered, and no hash is stored.
func TestStageMetadataModeVerifyStillGatesContent(t *testing.T) {
	root := newSourceRoot(t)
	content := []byte("original payload")
	writeSrcFile(t, root, "swapped.bin", content, 0o644, 1_700_000_000_123_456_789)
	in := simpleStageInput(t, root, t.TempDir(), "swapped.bin", content, 1_700_000_000_123_456_789)
	in.Mode = fsmeta.Mode{Set: fsmeta.ComparisonMetadata, Class: fsmeta.ClassCIFS}

	// Replace the source in place, same size, original mtime restored:
	// only the bytes (and the ignored inode) differ from the catalog.
	replacement := []byte("hijacked payload")
	if len(replacement) != len(content) {
		t.Fatalf("fixture broken: replacement length %d != %d", len(replacement), len(content))
	}
	abs := filepath.Join(root, "swapped.bin")
	if err := os.WriteFile(abs, replacement, 0o644); err != nil {
		t.Fatal(err)
	}
	mt := in.Inventory.Files[0]
	if err := os.Chtimes(abs, timeUnix(mt.MtimeNS), timeUnix(mt.MtimeNS)); err != nil {
		t.Fatal(err)
	}

	st, err := Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace}, in)
	if err == nil {
		st.Cleanup() //nolint:errcheck
		t.Fatal("metadata-mode staging mastered replaced content; the read-back verify must gate it")
	}
	var sc *SourceChangedError
	if !strings.Contains(err.Error(), "read-back hash mismatch") && !errors.As(err, &sc) {
		t.Errorf("err = %v, want the read-back mismatch (write-fault) classification", err)
	}
	// No stale bytes and no leaked scratch: Stage removes the whole
	// workspace on every failure path.
	entries, serr := os.ReadDir(in.WorkDir)
	if serr != nil {
		t.Fatalf("reading work dir: %v", serr)
	}
	if len(entries) != 0 {
		t.Errorf("work dir holds %d entries after a failed Stage, want none (leaked scratch)", len(entries))
	}
}

// timeUnix converts mtime_ns to the time.Time Chtimes takes.
func timeUnix(ns int64) time.Time { return time.Unix(0, ns) }
