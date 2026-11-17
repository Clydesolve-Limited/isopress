package imagebuild

// T4.2: the -graft-points pathlist (DESIGN_SPEC 6.2 path-list +
// graft-points; IMPLEMENTATION_PLAN T4.2 acceptance: "a fixed fixture
// plan produces a byte-stable pathlist.txt with the exact graft-point
// mapping ... including the manifest and export lines", plus the T4.1
// integration pin: the xorriso Invocation consumes the generated
// pathlist).

import (
	"os"
	"strings"
	"testing"

	"isopress/internal/xorriso"
)

// TestPathListGolden pins the EXACT lines of the staging pathlist for
// the mixed fixture: sorted, "/files/..." and "/chunks/.../NNNNN" ISO
// paths, absolute real staged disk paths. The full 6.2 shape - payload
// PLUS the manifest and export lines T4.3 appends via
// EmbeddedPathListEntries - is golden-checked as one file, so the
// xorriso input of the future builder is pinned byte for byte.
func TestPathListGolden(t *testing.T) {
	fx := newMixedFixture(t)

	st, err := stageFixture(t, fx)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	defer st.Cleanup() //nolint:errcheck

	// The payload pathlist Stage writes.
	payload, err := os.ReadFile(st.PathListPath)
	if err != nil {
		t.Fatalf("reading pathlist: %v", err)
	}
	wantPayload := "" +
		"/chunks/big.dat/00001=" + filepath2(st.Root, "chunks/big.dat/00001") + "\n" +
		"/chunks/big.dat/00002=" + filepath2(st.Root, "chunks/big.dat/00002") + "\n" +
		"/files/bin/tool=" + filepath2(st.Root, "files/bin/tool") + "\n" +
		"/files/docs/readme.txt=" + filepath2(st.Root, "files/docs/readme.txt") + "\n" +
		"/files/empty-dir=" + filepath2(st.Root, "files/empty-dir") + "\n" +
		"/files/link-dangling=" + filepath2(st.Root, "files/link-dangling") + "\n" +
		"/files/link-ok=" + filepath2(st.Root, "files/link-ok") + "\n"
	if string(payload) != wantPayload {
		t.Errorf("pathlist.txt mismatch:\n got: %q\nwant: %q", string(payload), wantPayload)
	}

	// The FULL 6.2 pathlist the T4.3 builder hands xorriso: payload +
	// the two embedded image-root members (MANIFEST.json,
	// catalog-export.sqlite), still sorted and deterministic.
	fullPath := st.Root + "-full-pathlist.txt"
	full := append(append([]PathListEntry(nil), st.PathList...), st.EmbeddedPathListEntries()...)
	if err := WritePathList(fullPath, full); err != nil {
		t.Fatalf("writing full pathlist: %v", err)
	}
	gotFull, err := os.ReadFile(fullPath)
	if err != nil {
		t.Fatalf("reading full pathlist: %v", err)
	}
	wantFull := "" +
		"/MANIFEST.json=" + filepath2(st.Root, "MANIFEST.json") + "\n" +
		"/catalog-export.sqlite=" + filepath2(st.Root, "catalog-export.sqlite") + "\n" +
		string(wantPayload)
	if string(gotFull) != wantFull {
		t.Errorf("full pathlist mismatch:\n got: %q\nwant: %q", string(gotFull), wantFull)
	}
	// Note: writing the extra file next to (not inside) the workspace
	// keeps the staged tree clean; the deferred Cleanup removes the
	// workspace, and the sibling file goes with the parent temp dir.
}

// TestPathListInvocationIntegration pins the T4.1 hand-off: the
// xorriso Invocation builder consumes the generated pathlist path, and
// the resulting Argv is exactly the 6.2 template.
func TestPathListInvocationIntegration(t *testing.T) {
	fx := newMixedFixture(t)

	st, err := stageFixture(t, fx)
	if err != nil {
		t.Fatalf("Stage: %v", err)
	}
	defer st.Cleanup() //nolint:errcheck

	// 6.4 (amended 2026-11-04): the volume id is volumeID's
	// "<SET-ID>-<NNN>" shortened to 32 bytes by the CALLER; here the
	// fixture's id is short, so the suffix simply attaches.
	inv := xorriso.Invocation{
		VolumeID:     volumeID(fx.in.SetID, fx.in.ImageID),
		OutputPath:   "/tmp/" + fx.in.SetID + "-001.iso.partial",
		PathListPath: st.PathListPath,
		Joliet:       true,
	}
	argv, err := inv.Argv()
	if err != nil {
		t.Fatalf("Argv: %v", err)
	}
	want := []string{
		"-as", "mkisofs",
		"-iso-level", "3",
		"-J",
		"-V", volumeID(fx.in.SetID, fx.in.ImageID),
		"-o", "/tmp/" + fx.in.SetID + "-001.iso.partial",
		"-graft-points",
		"-path-list", st.PathListPath,
	}
	if len(argv) != len(want) {
		t.Fatalf("argv = %v, want %v", argv, want)
	}
	for i := range want {
		if argv[i] != want[i] {
			t.Errorf("argv[%d] = %q, want %q", i, argv[i], want[i])
		}
	}
	// The pathlist the argv points at is the REAL generated file with
	// the graft lines xorriso will read.
	data, err := os.ReadFile(st.PathListPath)
	if err != nil {
		t.Fatalf("reading the argv-referenced pathlist: %v", err)
	}
	if !strings.Contains(string(data), "/files/docs/readme.txt=") {
		t.Errorf("argv-referenced pathlist lacks graft lines: %q", string(data))
	}
}

// TestWritePathListValidation pins the local gate of WritePathList
// (defense in depth for T4.3-fed entries): leading-slash ISO paths, no
// line-break/'='/NUL bytes on either side, no duplicate ISO paths, and
// deterministic sorted output.
func TestWritePathListValidation(t *testing.T) {
	dir := t.TempDir()

	cases := []struct {
		name    string
		entries []PathListEntry
		wantErr string
	}{
		{
			name:    "iso path without leading slash",
			entries: []PathListEntry{{ISOPath: "files/a", DiskPath: "/stage/files/a"}},
			wantErr: "must start with",
		},
		{
			name:    "newline in disk path",
			entries: []PathListEntry{{ISOPath: "/files/a", DiskPath: "/stage/f\niles/a"}},
			wantErr: "cannot express",
		},
		{
			name:    "equals in iso path",
			entries: []PathListEntry{{ISOPath: "/files=a", DiskPath: "/stage/files=a"}},
			wantErr: "cannot express",
		},
		{
			name: "duplicate iso path",
			entries: []PathListEntry{
				{ISOPath: "/files/a", DiskPath: "/stage/files/a"},
				{ISOPath: "/files/a", DiskPath: "/stage/other/a"},
			},
			wantErr: "duplicate",
		},
		{
			name:    "empty disk path",
			entries: []PathListEntry{{ISOPath: "/files/a"}},
			wantErr: "empty disk path",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := WritePathList(dir+"/pathlist.txt", tc.entries)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want a %q error", err, tc.wantErr)
			}
		})
	}

	// Sorted, stable output for valid entries (any input order).
	out := dir + "/sorted.txt"
	entries := []PathListEntry{
		{ISOPath: "/files/z", DiskPath: "/stage/files/z"},
		{ISOPath: "/MANIFEST.json", DiskPath: "/stage/MANIFEST.json"},
		{ISOPath: "/chunks/a/00001", DiskPath: "/stage/chunks/a/00001"},
	}
	if err := WritePathList(out, entries); err != nil {
		t.Fatalf("WritePathList: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading sorted pathlist: %v", err)
	}
	want := "" +
		"/MANIFEST.json=/stage/MANIFEST.json\n" +
		"/chunks/a/00001=/stage/chunks/a/00001\n" +
		"/files/z=/stage/files/z\n"
	if string(got) != want {
		t.Errorf("sorted pathlist = %q, want %q", string(got), want)
	}
	// The input slice is not mutated by the sort (defensive contract).
	if entries[0].ISOPath != "/files/z" {
		t.Errorf("WritePathList mutated its input slice: %v", entries[0])
	}
}

// filepath2 is the slash-preserving join for golden strings (the staged
// tree uses slash-separated media paths under the workspace root).
func filepath2(root, mediaPath string) string {
	return root + "/" + mediaPath
}
