package xorriso

// T4.1 argv construction tests (DESIGN_SPEC 6.2 arguments template).
// These pin the EXACT argv slice for a sample invocation and prove the
// no-shell/no-injection posture at the builder level: a VolumeID full of
// shell metacharacters passes through as ONE argv element, verbatim.
// The end-to-end proof (a real child process receiving exactly these
// elements, nothing evaluated) lives in run_test.go.

import (
	"reflect"
	"strings"
	"testing"
)

// sampleInvocation is the frozen sample from the 6.2 template.
func sampleInvocation() Invocation {
	return Invocation{
		VolumeID:     "SET-20261018",
		OutputPath:   "/work/SET-20261018-001.iso.partial",
		PathListPath: "/work/pathlist.txt",
		Joliet:       true,
	}
}

// The exact argv for the 6.2 template with Joliet enabled. Element order
// follows the invocation template spelled out in IMPLEMENTATION_PLAN
// T4.3: -as mkisofs -iso-level 3 -J -V <id> -o <out> -graft-points
// -path-list <file>. Forbidden flags (--norock, -r, -f/--follow-links)
// must appear nowhere.
func TestArgvMatchesSpecTemplateExactly(t *testing.T) {
	argv, err := sampleInvocation().Argv()
	if err != nil {
		t.Fatalf("Argv: %v", err)
	}
	want := []string{
		"-as", "mkisofs",
		"-iso-level", "3",
		"-J",
		"-V", "SET-20261018",
		"-o", "/work/SET-20261018-001.iso.partial",
		"-graft-points",
		"-path-list", "/work/pathlist.txt",
	}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("Argv() =\n%q\nwant\n%q", argv, want)
	}
	joined := strings.Join(argv, " ")
	for _, forbidden := range []string{"--norock", "-r ", "-f ", "--follow-links"} {
		if strings.Contains(joined+" ", forbidden) {
			t.Errorf("argv %q must never contain the forbidden flag %q (DESIGN_SPEC 6.2)", joined, forbidden)
		}
	}
}

// Without Joliet the -J element is absent entirely (not an empty string
// placeholder): xorriso must see no -J flag at all.
func TestArgvOmitsJolietWhenDisabled(t *testing.T) {
	inv := sampleInvocation()
	inv.Joliet = false
	argv, err := inv.Argv()
	if err != nil {
		t.Fatalf("Argv: %v", err)
	}
	for _, a := range argv {
		if a == "-J" {
			t.Fatalf("argv %q contains -J although Joliet is disabled", argv)
		}
	}
	want := []string{
		"-as", "mkisofs",
		"-iso-level", "3",
		"-V", "SET-20261018",
		"-o", "/work/SET-20261018-001.iso.partial",
		"-graft-points",
		"-path-list", "/work/pathlist.txt",
	}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("Argv() =\n%q\nwant\n%q", argv, want)
	}
}

// IsoLevel is pinned at 3 (the mkisofs-emulation default is level 1,
// which caps files at 4 GiB; DESIGN_SPEC 6.2 requires level 3).
func TestIsoLevelIsThree(t *testing.T) {
	if IsoLevel != 3 {
		t.Fatalf("IsoLevel = %d, want 3 (DESIGN_SPEC 6.2 multi-extent requirement)", IsoLevel)
	}
}

// Each required field empty is rejected before any argv is produced.
func TestArgvRejectsEmptyFields(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Invocation)
	}{
		{"empty VolumeID", func(inv *Invocation) { inv.VolumeID = "" }},
		{"empty OutputPath", func(inv *Invocation) { inv.OutputPath = "" }},
		{"empty PathListPath", func(inv *Invocation) { inv.PathListPath = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv := sampleInvocation()
			tt.mutate(&inv)
			argv, err := inv.Argv()
			if err == nil {
				t.Fatalf("Argv() = %q, want an error for %s", argv, tt.name)
			}
			if argv != nil {
				t.Errorf("Argv() returned argv %q alongside an error, want nil", argv)
			}
		})
	}
}

// Documented contract edge: the builder validates EMPTINESS only. A
// whitespace-only VolumeID is non-empty and passes straight through -
// content policy (32-char truncation, sanitization) belongs to the 6.4
// caller, not to this builder.
func TestArgvPassesWhitespaceVolumeIDThrough(t *testing.T) {
	inv := sampleInvocation()
	inv.VolumeID = "   "
	argv, err := inv.Argv()
	if err != nil {
		t.Fatalf("Argv rejected a non-empty VolumeID: %v", err)
	}
	if argv[6] != "   " {
		t.Errorf("argv[6] = %q, want the whitespace VolumeID verbatim", argv[6])
	}
}

// A NUL byte in ANY of the three fields is rejected (no argv element may
// contain one; exec would fail with a confusing OS error otherwise).
func TestArgvRejectsNULByte(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Invocation)
	}{
		{"NUL in VolumeID", func(inv *Invocation) { inv.VolumeID = "SET\x00ID" }},
		{"NUL in OutputPath", func(inv *Invocation) { inv.OutputPath = "/work/out\x00.iso" }},
		{"NUL in PathListPath", func(inv *Invocation) { inv.PathListPath = "/work/list\x00.txt" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv := sampleInvocation()
			tt.mutate(&inv)
			if argv, err := inv.Argv(); err == nil {
				t.Fatalf("Argv() = %q, want an error for %s", argv, tt.name)
			}
		})
	}
}

// SPACES AND NEWLINES ARE LEGAL FILENAME BYTES: OutputPath and
// PathListPath carrying them must pass through as SINGLE argv elements,
// byte-for-byte. A path is data, never command shape (DESIGN_SPEC 6.2,
// DESIGN_SPEC 1.2 no-shell posture).
func TestArgvPassesSpacesAndNewlinesInPathsAsSingleElements(t *testing.T) {
	out := "/work/my backup (final)/SET 2026\nnewline.iso.partial"
	list := "/work/path list\twith\twhitespace.txt"
	inv := sampleInvocation()
	inv.OutputPath = out
	inv.PathListPath = list

	argv, err := inv.Argv()
	if err != nil {
		t.Fatalf("Argv with spaces/newlines in paths: %v", err)
	}
	want := []string{
		"-as", "mkisofs",
		"-iso-level", "3",
		"-J",
		"-V", "SET-20261018",
		"-o", out,
		"-graft-points",
		"-path-list", list,
	}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("Argv() =\n%q\nwant\n%q", argv, want)
	}
}

// HARDENED EDGE (QA follow-up to the documented pass-through pin):
// PathListPath="-" is REJECTED before any argv is produced. xorriso
// interprets `-path-list -` as READ-FROM-STDIN, which the 6.2 builder
// never intends: imagebuild always writes a real graft-points pathlist
// file and passes its path, so a bare "-" is always a wiring bug that
// would otherwise hang the build on a blocking stdin read or swallow
// unrelated input. VolumeID and OutputPath keep "-" as a legal
// filename/set-id byte - pinned separately in
// TestValidateRejectsDashOnlyInPathList.
func TestArgvRejectsDashPathList(t *testing.T) {
	inv := sampleInvocation()
	inv.PathListPath = "-"
	argv, err := inv.Argv()
	if err == nil {
		t.Fatalf("Argv() = %q, want a rejection of PathListPath=%q", argv, "-")
	}
	if argv != nil {
		t.Errorf("Argv() returned argv %q alongside the rejection, want nil", argv)
	}
	if !strings.Contains(err.Error(), "PathListPath") {
		t.Errorf("rejection error %q must name the offending field PathListPath", err)
	}
	if !strings.Contains(err.Error(), "STDIN") {
		t.Errorf("rejection error %q must explain the stdin hazard", err)
	}
}

// QA-ADVERSARIAL GAP: the "-" rejection is scoped to PathListPath ONLY.
// A table across all three validated fields proves that "-" is still
// accepted verbatim as ONE argv element for VolumeID and OutputPath
// (both are legal filename bytes there; e.g. a path literally named
// "-" is legal on POSIX), while the same value in PathListPath is
// rejected. This guards against a future over-tightening that would
// start rejecting legitimate names, and against a silent widening that
// would let "-" back into PathListPath.
func TestValidateRejectsDashOnlyInPathList(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Invocation)
		wantErr bool
	}{
		{"dash VolumeID passes through", func(inv *Invocation) { inv.VolumeID = "-" }, false},
		{"dash OutputPath passes through", func(inv *Invocation) { inv.OutputPath = "-" }, false},
		{"dash PathListPath rejected", func(inv *Invocation) { inv.PathListPath = "-" }, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inv := sampleInvocation()
			tt.mutate(&inv)
			argv, err := inv.Argv()
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Argv() = %q, want a rejection", argv)
				}
				if argv != nil {
					t.Errorf("Argv() returned argv %q alongside the rejection, want nil", argv)
				}
				return
			}
			if err != nil {
				t.Fatalf("Argv() rejected a legal \"-\" filename value: %v", err)
			}
			// The "-" value must reach argv verbatim in exactly its slot.
			for i, a := range argv {
				if a == "-" && argv[i-1] != "-V" && argv[i-1] != "-o" {
					t.Errorf("argv %q: bare \"-\" appeared after %q, want only after -V or -o", argv, argv[i-1])
				}
			}
		})
	}
}

// INJECTION PIN: a VolumeID carrying every shell metacharacter pattern
// passes through as exactly ONE argv element, byte-for-byte verbatim.
// Whatever a shell would have made of "rm -rf /" or "$(touch pwned)"
// here is inert data; the end-to-end proof that nothing evaluates it is
// in run_test.go (fake xorriso dumps "$@" and no pwned file appears).
func TestArgvVolumeIDMetacharactersPassThroughVerbatim(t *testing.T) {
	hostile := `S-$(touch pwned); rm -rf / #` + "`id`" + ` "quoted" 'single' * ? [a-z] | & > <newline`
	inv := sampleInvocation()
	inv.VolumeID = hostile
	argv, err := inv.Argv()
	if err != nil {
		t.Fatalf("Argv with a hostile VolumeID: %v", err)
	}
	want := []string{
		"-as", "mkisofs",
		"-iso-level", "3",
		"-J",
		"-V", hostile,
		"-o", "/work/SET-20261018-001.iso.partial",
		"-graft-points",
		"-path-list", "/work/pathlist.txt",
	}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("Argv() with hostile VolumeID =\n%q\nwant\n%q", argv, want)
	}
	// Exactly one element carries the hostile payload, and it is the
	// -V value: no splitting, no quoting, no expansion.
	count := 0
	for _, a := range argv {
		if a == hostile {
			count++
		}
	}
	if count != 1 {
		t.Errorf("hostile VolumeID appears %d times as a whole argv element, want exactly 1", count)
	}
}
