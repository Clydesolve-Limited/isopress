package report

// T0.5: the error-taxonomy to exit-code mapping (DESIGN_SPEC 10.1 and
// 10.2; IMPLEMENTATION_PLAN T0.5). This file is the AUTHORITATIVE mapping
// the whole tool uses: the cli layer's execution wrapper calls Classify on
// the command's returned error and exits with the resulting code.
//
// DESIGN_SPEC 10.3: errors are printed to stderr, never stdout. The
// printing itself lives in the cli wrapper (it knows the binary name);
// this package owns only classification and the typed error classes below.

import (
	"errors"
	"fmt"
	"strings"

	"isopress/internal/catalog"
	"isopress/internal/units"
)

// Exit codes of the DESIGN_SPEC 10.2 scheme. These constants are the only
// spellings of the codes allowed anywhere in the tool.
const (
	// ExitOK (0): success, no item failures (DESIGN_SPEC 10.2 row 1).
	ExitOK = 0
	// ExitFatal (1): fatal unexpected error, the bug class; a stack
	// trace is printed under --verbose (10.2 row 2).
	ExitFatal = 1
	// ExitUsage (2): usage error - bad flags, missing arguments,
	// unsatisfiable combinations (10.1 UsageError, 10.2 row 3).
	ExitUsage = 2
	// ExitDependency (3): missing external dependency - xorriso absent
	// or too old when an image operation was requested (10.1
	// DependencyError, 10.2 row 4).
	ExitDependency = 3
	// ExitCatalog (4): catalog error - cannot open, corrupt, schema too
	// new, migration failure (10.1 CatalogError, 10.2 row 5).
	ExitCatalog = 4
	// ExitCatalogBusy (5): another mutating instance holds the advisory
	// lock (10.1 CatalogBusy, D-11, 10.2 row 6).
	ExitCatalogBusy = 5
	// ExitItemFailures (10): the operation ran to completion and the
	// report enumerates every per-item failure; automation decides
	// policy (10.1 ItemFailures, 10.2 row 7, partial-failure
	// semantics).
	ExitItemFailures = 10
	// ExitInterrupted (130): the run was aborted by SIGINT or SIGTERM
	// (10.1 Interrupted, 10.2 row 8). The code follows the shell
	// convention 128 + SIGINT for BOTH registered signals, exactly as
	// 10.1 specifies.
	ExitInterrupted = 130
)

// UsageError is the typed error of the UsageError class (DESIGN_SPEC
// 10.1): bad flags, missing arguments, unsatisfiable flag combinations.
// Classify maps it to exit 2. The cli layer constructs it for flag-parse
// failures (via cobra's FlagErrorFunc) and for argument validation; the
// cli prints it to stderr together with a usage hint, per 10.1.
//
// Exactly one of Msg or Err carries the message; both are supported so
// callers can either compose a fresh message (Msg) or wrap an existing
// error (Err).
type UsageError struct {
	Msg     string // fresh message (used when Err is nil)
	Err     error  // underlying parse/validation error, optional
	Command string // command path for the usage hint, optional
}

func (e *UsageError) Error() string {
	switch {
	case e.Msg != "" && e.Err != nil:
		return fmt.Sprintf("%s: %v", e.Msg, e.Err)
	case e.Msg != "":
		return e.Msg
	case e.Err != nil:
		return e.Err.Error()
	default:
		return "usage error"
	}
}

// Unwrap exposes the wrapped parse/validation error to errors.Is/As.
func (e *UsageError) Unwrap() error { return e.Err }

// DependencyError is the typed error of the DependencyError class
// (DESIGN_SPEC 10.1): an external dependency is missing or too old when an
// image operation was requested. Classify maps it to exit 3. The message
// states what is missing and carries per-distribution install hints
// (10.1); T4.1's xorriso wrapper constructs it (missing xorriso, or a
// version below the >= 1.5.4 floor). Copy mode and copy verification never
// produce this class (NFR-004, DESIGN_SPEC 10.1).
type DependencyError struct {
	Missing string   // external tool name, e.g. "xorriso"
	Detail  string   // what exactly is wrong, e.g. "found 1.5.2, need >= 1.5.4"
	Hints   []string // per-distribution install hints
	Err     error    // underlying lookup/version-probe error, optional
}

func (e *DependencyError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "missing external dependency %q", e.Missing)
	if e.Detail != "" {
		fmt.Fprintf(&b, ": %s", e.Detail)
	}
	if e.Err != nil {
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	if len(e.Hints) > 0 {
		fmt.Fprintf(&b, " (install: %s)", strings.Join(e.Hints, "; "))
	}
	return b.String()
}

// Unwrap exposes the underlying probe error to errors.Is/As.
func (e *DependencyError) Unwrap() error { return e.Err }

// ItemFailures is the typed carrier of the ItemFailures class
// (DESIGN_SPEC 10.1): per-file failures that never abort the run -
// unreadable source, source-changed skip, write fault after retries, hash
// mismatch, missing chunk. Commands collect the per-item errors into this
// type and return it (possibly wrapping it) at the end of the run;
// Classify maps it to exit 10 and the run report enumerates the
// individual failures (10.1: "collected into the run report and
// run_events"). T3.3 and later tasks construct it.
//
// Unwrap() []error lets errors.Is/As traverse the individual item
// failures (Go 1.20 multi-error unwrapping).
type ItemFailures struct {
	Failures []error // one entry per failed item, in report order
}

func (e *ItemFailures) Error() string {
	return fmt.Sprintf("completed with %d item failure(s) (see the run report)", len(e.Failures))
}

// Unwrap exposes the individual item failures to errors.Is/As.
func (e *ItemFailures) Unwrap() []error { return e.Failures }

// ErrInterrupted is the sentinel of the Interrupted class (DESIGN_SPEC
// 10.1): SIGINT or SIGTERM aborted the run. Classify maps it (directly or
// through wrapping) to exit 130. The cli's signal scaffolding wraps it
// with the received signal name; the finish-in-flight-item semantics of
// 10.1 ("finishes the in-flight item's marking transaction ... records
// status='interrupted'") arrive with T3.3 and T6.1 - at T0.5 the signal
// cancels the command context and the run maps to 130.
var ErrInterrupted = errors.New("interrupted by signal")

// Classify maps an error to its DESIGN_SPEC 10.2 exit code. It is the
// authoritative taxonomy-to-code mapping, called by the cli execution
// wrapper for every command result:
//
//	err == nil                                  ->  0 (ExitOK)
//	*UsageError (or units.ErrInvalidSize)       ->  2 (ExitUsage)
//	*DependencyError                            ->  3 (ExitDependency)
//	*catalog.CatalogBusy                        ->  5 (ExitCatalogBusy)
//	*catalog.CatalogError                       ->  4 (ExitCatalog)
//	*ItemFailures                               -> 10 (ExitItemFailures)
//	ErrInterrupted                              -> 130 (ExitInterrupted)
//	anything else                               ->  1 (ExitFatal)
//
// Classification uses errors.As/errors.Is, so wrapped errors classify by
// their typed cause. CatalogBusy is checked before CatalogError because
// 10.1 gives the busy class its own code (5) separate from the general
// catalog class (4).
//
// units.ErrInvalidSize maps to ExitUsage because T0.2's contract (units
// parse.go, DESIGN_SPEC 10.1) designates every size-parse failure a
// usage-class error and routes its mapping through this function.
func Classify(err error) int {
	if err == nil {
		return ExitOK
	}
	var usage *UsageError
	if errors.As(err, &usage) {
		return ExitUsage
	}
	if errors.Is(err, units.ErrInvalidSize) {
		return ExitUsage
	}
	var dep *DependencyError
	if errors.As(err, &dep) {
		return ExitDependency
	}
	var busy *catalog.CatalogBusy
	if errors.As(err, &busy) {
		return ExitCatalogBusy
	}
	var cerr *catalog.CatalogError
	if errors.As(err, &cerr) {
		return ExitCatalog
	}
	var items *ItemFailures
	if errors.As(err, &items) {
		return ExitItemFailures
	}
	if errors.Is(err, ErrInterrupted) {
		return ExitInterrupted
	}
	return ExitFatal
}
