package imagebuild

// T4.2: the two capacity pre-checks (DESIGN_SPEC 6.2 free-space,
// 6.3 step 5 payload-vs-usable_size; IMPLEMENTATION_PLAN T4.2
// acceptance: "an over-capacity plan fails with the plan error before
// any xorriso process is spawned (asserted by the fake-script sentinel
// never being executed)" and "insufficient space on the temp root fails
// before staging begins with the requirement stated in the message").
//
// The no-xorriso-invocation assertion uses the established fake-script
// sentinel: a fake `xorriso` on PATH that would drop a marker file is
// never executed - structurally it cannot be (nothing in this package
// spawns processes), and the sentinel pins the acceptance end to end.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"isopress/internal/report"
	"isopress/internal/units"
)

// installFakeXorriso puts a sentinel-dropping fake xorriso at the front
// of PATH and returns the sentinel path. The staging subsystem must
// never execute it (T4.2 acceptance: the plan error fires "before any
// xorriso process is spawned").
func installFakeXorriso(t *testing.T) (sentinel string) {
	t.Helper()
	dir := t.TempDir()
	sentinel = filepath.Join(dir, "xorriso-was-executed")
	script := "#!/bin/sh\ntouch '" + sentinel + "'\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "xorriso"), []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake xorriso: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return sentinel
}

// assertNoXorrisoRan pins the sentinel.
func assertNoXorrisoRan(t *testing.T, sentinel string) {
	t.Helper()
	if _, err := os.Lstat(sentinel); !os.IsNotExist(err) {
		t.Errorf("fake xorriso was executed (sentinel err=%v); staging must never spawn xorriso", err)
	}
}

// TestPreCheckFreeSpaceInsufficient pins pre-check (a): the injected
// FreeSpaceFunc reports too little space on the staging root -> a clear
// UsageError naming required vs available, BEFORE any payload is
// staged (the scratch is cleaned) and with no xorriso invocation.
func TestPreCheckFreeSpaceInsufficient(t *testing.T) {
	fx := newMixedFixture(t)
	sentinel := installFakeXorriso(t)

	_, err := Stage(context.Background(), Deps{
		FreeSpace: func(string) (uint64, error) { return 1024, nil }, // far too little
		HashStore: fx.hashStore,
	}, fx.in)
	if err == nil {
		t.Fatal("Stage succeeded, want the 6.2 free-space failure")
	}
	ue := usageErrOf(t, err)
	for _, want := range []string{"insufficient free space", "available", "work-dir"} {
		if !strings.Contains(ue.Error(), want) {
			t.Errorf("message %q does not contain %q (required vs available must be stated)", ue.Error(), want)
		}
	}
	// Before staging begins: the scratch was created for the Statfs
	// and removed again - no staged payload survives.
	entries, rerr := os.ReadDir(fx.workDir)
	if rerr != nil {
		t.Fatalf("read work dir: %v", rerr)
	}
	if len(entries) != 0 {
		t.Errorf("free-space failure leaked scratch: %v", entries)
	}
	assertNoXorrisoRan(t, sentinel)
}

// TestPreCheckCapacityBoundary pins the EXACT 6.3 step 5 arithmetic
// through the CANONICAL estimator (units.RequiredImageBytes, shared
// with the planner since the 2026-10-20 amendment):
// payload + FixedImageOverhead(2 MiB + 512*2) + 512*payloadEntries ==
// usable_size PASSES; one byte less FAILS with the plan-class error -
// both before xorriso exists. The literal comparison below also pins
// the numerical identity with the pre-amendment spelling
// payload + 2 MiB + 512*(entries+2): only the two embedded image-root
// entries moved into the fixed term.
func TestPreCheckCapacityBoundary(t *testing.T) {
	fx := newMixedFixture(t)
	sentinel := installFakeXorriso(t)

	// The fixture's payload: 14 + 20 whole-file bytes, 60 + 40 chunk
	// bytes; PAYLOAD entries: 2 files + 2 chunks + 2 symlinks + 1 dir
	// (validateInput's count - the two embedded members live in the
	// fixed term now).
	payload := int64(14 + 20 + 60 + 40)
	payloadEntries := 2 + 2 + 2 + 1
	exact := units.RequiredImageBytes(payload, int64(payloadEntries))
	if want := payload + (2 << 20) + int64(payloadEntries+2)*512; exact != want {
		t.Fatalf("RequiredImageBytes = %d, want the literal 6.3 step 5 arithmetic %d "+
			"(= payload + 2 MiB + 512*(payloadEntries+2), the pre-amendment spelling)",
			exact, want)
	}

	// AT the boundary: staging succeeds (payload+2MiB+512n == usable).
	in := fx.in
	in.UsableSize = exact
	st, err := Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace, HashStore: fx.hashStore}, in)
	if err != nil {
		t.Fatalf("Stage at the exact capacity boundary: %v (payload+2MiB+512n == usable_size must pass)", err)
	}
	st.Cleanup() //nolint:errcheck

	// ONE BYTE over: the plan-class failure, with the overflow named.
	in.UsableSize = exact - 1
	_, err = Stage(context.Background(), Deps{FreeSpace: hugeFreeSpace, HashStore: fx.hashStore}, in)
	if err == nil {
		t.Fatal("Stage at usable_size-1 succeeded, want the 6.3 step 5 plan error")
	}
	ue := usageErrOf(t, err)
	for _, want := range []string{"usable_size", "overflow", "6.3 step 5"} {
		if !strings.Contains(ue.Error(), want) {
			t.Errorf("message %q does not contain %q", ue.Error(), want)
		}
	}
	// The over-capacity refusal has no side effects: no scratch, no
	// xorriso (the T4.2 acceptance sentinel).
	entriesOnDisk, rerr := os.ReadDir(fx.workDir)
	if rerr != nil {
		t.Fatalf("read work dir: %v", rerr)
	}
	if len(entriesOnDisk) != 0 {
		t.Errorf("over-capacity refusal leaked scratch: %v", entriesOnDisk)
	}
	assertNoXorrisoRan(t, sentinel)

	// Classification pin: report.Classify maps it to exit 2 (the
	// documented UsageError ruling for both pre-checks).
	if got := report.Classify(ue); got != report.ExitUsage {
		t.Errorf("report.Classify(pre-check failure) = %d, want ExitUsage %d", got, report.ExitUsage)
	}
}

// TestPreCheckFreeSpaceErrorFromProbe pins the probe-failure path: a
// Statfs-level error surfaces as an error, never as a silent pass.
func TestPreCheckFreeSpaceErrorFromProbe(t *testing.T) {
	fx := newMixedFixture(t)

	_, err := Stage(context.Background(), Deps{
		FreeSpace: func(string) (uint64, error) { return 0, errors.New("statfs exploded") },
		HashStore: fx.hashStore,
	}, fx.in)
	if err == nil || !strings.Contains(err.Error(), "statfs exploded") {
		t.Fatalf("err = %v, want the probe error surfaced", err)
	}
	var ue *report.UsageError
	if errors.As(err, &ue) {
		t.Error("a probe failure is not a usage error (it names a system fault, not user input)")
	}
}

// TestRequiredImageBytesMovedToUnits documents the estimator's home:
// the canonical RequiredImageBytes lives in internal/units (shared by
// the planner's balancer and this package's pre-check); its value
// pins, saturation contract, and the old-formula numerical identity
// are asserted there (units/capacity_test.go), and the builder-side
// boundary is pinned by TestPreCheckCapacityBoundary above.

// TestStatfsFreeSpaceReal exercises the production probe against the
// real filesystem (stdlib syscall.Statfs, the DESIGN_SPEC 11 gate's
// sanctioned mechanism).
func TestStatfsFreeSpaceReal(t *testing.T) {
	avail, err := statfsFreeSpace(t.TempDir())
	if err != nil {
		t.Fatalf("statfsFreeSpace: %v", err)
	}
	if avail == 0 {
		t.Error("statfsFreeSpace reported 0 available bytes on a fresh temp dir")
	}
}
