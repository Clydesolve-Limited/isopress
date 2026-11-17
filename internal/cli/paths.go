package cli

// Path-input hygiene for the stakeholder-reported "quoted ~" defect
// (PM decisions D1/D2 of 2026-10-19; self-contained brief, no spec
// re-litigation). A user ran:
//
//	isopress scan --catalog "~/tmp/catalog.db" ROOT
//
// Double quotes suppress the SHELL's tilde expansion, so the tool
// received the literal "~/tmp/catalog.db", resolved it against the CWD,
// and failed deep inside SQLite with a raw "unable to open database
// file (14)" that named neither the mistake nor the fix. D1 (fixed):
// detect a leading literal "~" and fail with a clear USAGE error - the
// tool NEVER auto-expands data paths (Go CLI convention: no magic on
// paths).
//
// Trigger rule (documented judgment call): only the two UNAMBIGUOUS
// home-shorthand forms are rejected - the bare "~" (a shell would
// expand it to $HOME) and the "~/..." prefix (a shell would expand it
// to "$HOME/..."). Everything else passes through unchanged, because
// everything else is a legitimate literal path the tool must honor
// as-is:
//
//   - "~user/name" (the login-name shell expansion) is a DIFFERENT
//     shell feature this tool does not support and never expands; it is
//     treated as a literal directory name, like any other relative
//     path. Only ~ and ~/ are unambiguous, so only those trigger.
//   - "foo/~/bar" and "./~/x" carry "~" mid-path: no shell expands a
//     tilde that is not the first character, so there is nothing to
//     detect.
//   - a relative path that genuinely starts with a directory literally
//     named "~" is the documented false-positive corner: it is rejected
//     too, because at the CLI surface the two forms are
//     indistinguishable and a clear usage error beats a deep ENOENT -
//     the operator renames the directory or passes an absolute path.
//     This case is rare; the ambiguous literal is refused deliberately.
//
// Wiring (every path-typed CLI input currently accepted): the --catalog,
// --exclude-from, --work-dir, and --image-dir global flags through
// validateGlobalFlags (root.go, the existing --exclude-from
// exists-check pattern), and scan's ROOT positional through scanArgs
// (scan.go, the usageArgs wrap convention). Future path-typed inputs
// (--image N=PATH, --target, ...) must wire this check too.

import (
	"fmt"
	"strings"
)

// checkTilde rejects a path value whose leading "~" is almost certainly
// an unexpanded shell token (see the file doc for the precise trigger
// rule). name identifies the input in the message - a flag spelling
// ("--catalog") or a positional name ("ROOT") - and value is echoed
// verbatim so the user sees exactly what the tool received.
//
// The returned error is intentionally UNTYPED: callers wrap it in a
// report.UsageError carrying the command path (the usageArgs wrap
// convention of scan.go), which report.Classify maps to exit 2
// (DESIGN_SPEC 10.1). Nil means the value passed the tilde rule.
func checkTilde(value, name string) error {
	if value != "~" && !strings.HasPrefix(value, "~/") {
		return nil
	}
	return fmt.Errorf(
		"%s: %q starts with a literal ~: a quoted path (\"~/...\") is not shell-expanded, "+
			"so isopress received the ~ verbatim and never expands it; "+
			"pass the path with an unquoted ~, use $HOME in place of ~, or give an absolute path",
		name, value)
}
