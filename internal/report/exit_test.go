package report

// T0.5 acceptance tests: the exit-code mapping of DESIGN_SPEC 10.2, one
// case per row of the 10.2 table (DESIGN_SPEC 12.1: "exit-code mapping"
// belongs to the unit tier). Catalog classes use the real catalog package
// types: CatalogError is constructed directly (exported struct), and
// CatalogBusy is constructed the way lock.go produces it (struct wrapping
// the ErrCatalogBusy sentinel plus the EWOULDBLOCK errno).

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"

	"isopress/internal/catalog"
	"isopress/internal/units"
)

// exitCase is one row of the DESIGN_SPEC 10.2 table in test form.
type exitCase struct {
	name string
	err  error
	want int
}

// TestClassifyOneRowPerExitCode asserts every row of the 10.2 scheme:
// nil -> 0, generic -> 1, UsageError -> 2, DependencyError -> 3,
// CatalogError -> 4, CatalogBusy -> 5, ItemFailures -> 10,
// Interrupted -> 130.
func TestClassifyOneRowPerExitCode(t *testing.T) {
	cases := []exitCase{
		{
			name: "0 success: nil error",
			err:  nil,
			want: ExitOK,
		},
		{
			name: "1 fatal unexpected: plain error",
			err:  errors.New("goroutine 1 unexpected"),
			want: ExitFatal,
		},
		{
			name: "2 usage: UsageError",
			err:  &UsageError{Msg: "unknown flag: --bogus"},
			want: ExitUsage,
		},
		{
			name: "3 dependency: DependencyError",
			err:  &DependencyError{Missing: "xorriso", Detail: "not found in PATH"},
			want: ExitDependency,
		},
		{
			name: "4 catalog: CatalogError (constructible kind)",
			err: &catalog.CatalogError{
				Kind: catalog.KindSchemaTooNew,
				Path: "/tmp/catalog.sqlite",
				Err:  catalog.ErrSchemaTooNew,
			},
			want: ExitCatalog,
		},
		{
			name: "5 catalog busy: CatalogBusy (sentinel + errno, as lock.go builds it)",
			err: &catalog.CatalogBusy{
				Path:     "/tmp/catalog.sqlite",
				LockPath: "/tmp/catalog.sqlite.lock",
				Mode:     catalog.LockExclusive,
				Err:      syscall.EWOULDBLOCK,
			},
			want: ExitCatalogBusy,
		},
		{
			name: "10 item failures: ItemFailures",
			err: &ItemFailures{Failures: []error{
				errors.New("unreadable: /src/a"),
				errors.New("source-changed: /src/b"),
			}},
			want: ExitItemFailures,
		},
		{
			name: "130 interrupted: ErrInterrupted sentinel",
			err:  fmt.Errorf("scan: %w: signal received", ErrInterrupted),
			want: ExitInterrupted,
		},
	}
	for _, tc := range cases {
		if got := Classify(tc.err); got != tc.want {
			t.Errorf("%s: Classify(%v) = %d, want %d", tc.name, tc.err, got, tc.want)
		}
	}
}

// Classification works through wrapping: the cli and services wrap typed
// errors with context, and the exit code must follow the typed cause
// (errors.As / errors.Is semantics, DESIGN_SPEC 10.1).
func TestClassifyThroughWrapping(t *testing.T) {
	cases := []exitCase{
		{
			name: "wrapped UsageError",
			err:  fmt.Errorf("scan: %w", &UsageError{Msg: "scanning / requires an exclusion"}),
			want: ExitUsage,
		},
		{
			name: "wrapped DependencyError",
			err:  fmt.Errorf("build: %w", &DependencyError{Missing: "xorriso"}),
			want: ExitDependency,
		},
		{
			name: "wrapped CatalogError",
			err:  fmt.Errorf("plan: %w", &catalog.CatalogError{Kind: catalog.KindOpen, Err: errors.New("missing")}),
			want: ExitCatalog,
		},
		{
			name: "CatalogBusy via errors.Is sentinel and wrapped errno",
			err: fmt.Errorf("copy: open catalog: %w", &catalog.CatalogBusy{
				Path: "/c.sqlite", LockPath: "/c.sqlite.lock",
				Mode: catalog.LockExclusive, Err: syscall.EWOULDBLOCK,
			}),
			want: ExitCatalogBusy,
		},
		{
			name: "wrapped ItemFailures",
			err:  fmt.Errorf("restore: %w", &ItemFailures{Failures: []error{errors.New("missing chunk 2")}}),
			want: ExitItemFailures,
		},
	}
	for _, tc := range cases {
		if got := Classify(tc.err); got != tc.want {
			t.Errorf("%s: Classify(%v) = %d, want %d", tc.name, tc.err, got, tc.want)
		}
	}

	// A busy error nested in a catalog-shaped chain still classifies 5,
	// the more specific code (Classify checks CatalogBusy first).
	nested := fmt.Errorf("mutating open: %w", &catalog.CatalogBusy{
		Path: "/c.sqlite", LockPath: "/c.sqlite.lock",
		Mode: catalog.LockExclusive, Err: syscall.EWOULDBLOCK,
	})
	if got := Classify(nested); got != ExitCatalogBusy {
		t.Errorf("CatalogBusy chain: Classify = %d, want %d", got, ExitCatalogBusy)
	}
}

// units.ErrInvalidSize is usage-class per T0.2's contract (units
// parse.go: "the cli layer maps errors matching it (errors.Is) to exit
// 2"); the mapping lives in Classify, which the cli wrapper calls.
func TestClassifyUnitsSizeErrorsAreUsage(t *testing.T) {
	_, err := units.ParseSize("banana")
	if err == nil {
		t.Fatal("ParseSize(banana) succeeded, want error")
	}
	if got := Classify(err); got != ExitUsage {
		t.Errorf("Classify(units size error) = %d, want %d (ExitUsage)", got, ExitUsage)
	}
}

// Every typed class produces a non-empty, standalone message: the cli
// prints it verbatim to stderr (DESIGN_SPEC 10.1, 10.3), so it must name
// the problem without dereferencing anything else.
func TestTypedErrorMessagesArePresentable(t *testing.T) {
	msgs := map[string]error{
		"usage":     &UsageError{Msg: "unknown command \"foo\" for \"isopress\""},
		"usage-err": &UsageError{Err: errors.New("unknown flag: --bogus"), Command: "isopress scan"},
		"dep": &DependencyError{
			Missing: "xorriso",
			Detail:  "found version 1.5.2, need >= 1.5.4",
			Hints:   []string{"Debian/Ubuntu: apt install xorriso", "Fedora: dnf install xorriso"},
		},
		"catalog": &catalog.CatalogError{Kind: catalog.KindOpen, Path: "/c.sqlite", Err: errors.New("no such file")},
		"busy": &catalog.CatalogBusy{
			Path: "/c.sqlite", LockPath: "/c.sqlite.lock",
			Mode: catalog.LockExclusive, Err: syscall.EWOULDBLOCK,
		},
		"items":     &ItemFailures{Failures: []error{errors.New("x"), errors.New("y")}},
		"interrupt": ErrInterrupted,
	}
	for name, err := range msgs {
		if err.Error() == "" {
			t.Errorf("%s: empty Error() message", name)
		}
	}
	// The DependencyError message carries the install-hint clause of
	// DESIGN_SPEC 10.1.
	dep := (&DependencyError{Missing: "xorriso", Hints: []string{"apt install xorriso"}}).Error()
	for _, want := range []string{"xorriso", "apt install xorriso"} {
		if !strings.Contains(dep, want) {
			t.Errorf("DependencyError message %q does not contain %q", dep, want)
		}
	}
	// The ItemFailures message points at the run report (10.1).
	if items := (&ItemFailures{Failures: []error{errors.New("x")}}).Error(); !strings.Contains(items, "run report") {
		t.Errorf("ItemFailures message %q does not mention the run report", items)
	}
}

// ItemFailures.Unwrap() []error is the Go 1.20 multi-error contract:
// errors.Is/As must traverse EVERY collected failure, including
// errors.Is-matched sentinels nested inside individual failures, and must
// NOT match errors outside the collection. Commands collect per-item
// failures in report order (DESIGN_SPEC 10.1); automation and the run
// report rely on this traversal.
func TestItemFailuresUnwrapYieldsAllFailures(t *testing.T) {
	f1 := errors.New("unreadable: /src/a")
	nestedSentinel := errors.New("hash mismatch")
	f2 := fmt.Errorf("verify /src/b: %w", nestedSentinel)
	f3 := errors.New("missing chunk 2 of /src/c")
	all := &ItemFailures{Failures: []error{f1, f2, f3}}

	if got := len(all.Unwrap()); got != 3 {
		t.Fatalf("Unwrap() yielded %d errors, want 3", got)
	}
	// Every collected failure is reachable through the carrier.
	for _, target := range []error{f1, f2, f3, nestedSentinel} {
		if !errors.Is(all, target) {
			t.Errorf("errors.Is(ItemFailures, %v) = false, want true (multi-error traversal)", target)
		}
	}
	// Errors outside the collection must not match.
	if errors.Is(all, errors.New("unreadable: /src/a")) {
		t.Error("errors.Is matched a non-member error by value equality; the carrier must only contain its failures")
	}
	// Traversal holds through wrapping, the shape services return.
	wrapped := fmt.Errorf("copy: completed with failures: %w", all)
	if !errors.Is(wrapped, f2) {
		t.Errorf("errors.Is(wrapped carrier, %v) = false, want true", f2)
	}
	// errors.As finds an individual typed failure inside the collection.
	type typedFailure struct{ error }
	typed := &typedFailure{error: errors.New("source-changed: /src/d")}
	mixed := &ItemFailures{Failures: []error{f1, typed, f3}}
	var found *typedFailure
	if !errors.As(mixed, &found) || found != typed {
		t.Errorf("errors.As did not surface the typed failure inside the collection: got %v", found)
	}
}

// The DependencyError message carries the DESIGN_SPEC 10.1 contract
// content: what is missing, why, and the per-distribution install-hint
// clause joined with "; " in declaration order. Without hints the
// install clause is absent rather than dangling.
func TestDependencyErrorInstallHintClause(t *testing.T) {
	e := &DependencyError{
		Missing: "xorriso",
		Detail:  "not found in PATH",
		Hints:   []string{"Debian/Ubuntu: apt install xorriso", "Fedora: dnf install xorriso"},
	}
	msg := e.Error()
	for _, want := range []string{
		`missing external dependency "xorriso"`,
		"not found in PATH",
		"(install: Debian/Ubuntu: apt install xorriso; Fedora: dnf install xorriso)",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("DependencyError message %q does not contain %q", msg, want)
		}
	}
	if bare := (&DependencyError{Missing: "xorriso"}).Error(); strings.Contains(bare, "install:") {
		t.Errorf("DependencyError without hints must omit the install clause, got: %q", bare)
	}
}

// The exit-code constants are exactly the 10.2 table values; a drift
// here would silently break cron automation.
func TestExitCodeConstantsMatchTable(t *testing.T) {
	want := map[string]int{
		"ExitOK":           0,
		"ExitFatal":        1,
		"ExitUsage":        2,
		"ExitDependency":   3,
		"ExitCatalog":      4,
		"ExitCatalogBusy":  5,
		"ExitItemFailures": 10,
		"ExitInterrupted":  130,
	}
	got := map[string]int{
		"ExitOK":           ExitOK,
		"ExitFatal":        ExitFatal,
		"ExitUsage":        ExitUsage,
		"ExitDependency":   ExitDependency,
		"ExitCatalog":      ExitCatalog,
		"ExitCatalogBusy":  ExitCatalogBusy,
		"ExitItemFailures": ExitItemFailures,
		"ExitInterrupted":  ExitInterrupted,
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s = %d, want %d", name, got[name], w)
		}
	}
}
