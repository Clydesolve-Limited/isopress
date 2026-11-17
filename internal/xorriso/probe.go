package xorriso

// The xorriso version probe (T4.1, DESIGN_SPEC 11 "runtime version
// probe >= 1.5.4 stands", NFR-004 fail-fast).
//
// Verified against the real binary (xorriso 1.5.6 on the dev machine,
// pre-flight 2026-10-18): `xorriso -version` exits 0 and prints to BOTH
// streams - the one-line banner "xorriso 1.5.6 : RockRidge filesystem
// manipulator, libburnia project." goes to stderr while the detailed
// version report ("xorriso 1.5.6 / ISO 9660 Rock Ridge filesystem
// manipulator ...") goes to stdout. The probe therefore captures COMBINED
// output and parses the first banner line out of it, which works for
// either stream layout.
//
// Timeout posture: the probe wraps the caller's context with
// ProbeTimeout so a hung xorriso binary cannot hang the tool. A BUILD
// has no internal timeout at all (builds are long-running; the caller's
// context governs - see Tool.Run); only the probe is bounded. Cancellation
// by the CALLER (signal-driven ctx cancel) propagates as the raw context
// error, not as a DependencyError, so the cli can map SIGINT to the
// Interrupted class (exit 130) rather than the dependency class (exit 3).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"isopress/internal/report"
)

// ProbeTimeout bounds a single version probe. It is a package var rather
// than a const purely so tests can shorten it; production code must not
// mutate it. Ten seconds is generous for a local process that prints one
// screen of text while still turning a hung or wedged binary into an
// immediate, actionable DependencyError.
var ProbeTimeout = 10 * time.Second

// rawSnippetLimit bounds how much raw probe output is embedded in a
// DependencyError message, so a pathological xorriso cannot turn an
// error message into a multi-megabyte transcript. The full (10 MiB-capped)
// transcript remains available on the returned Info for run_events.
const rawSnippetLimit = 512

// Info is the successful result of a version probe.
type Info struct {
	// Path is the resolved binary path that was probed.
	Path string
	// Version is the parsed xorriso version, guaranteed >= MinVersion
	// on success.
	Version Version
	// Raw is the combined stdout+stderr of `xorriso -version`, capped
	// at MaxCapturedOutput bytes (tail), for transcripts and reports.
	Raw string
}

// Probe runs `xorriso -version` against the discovered binary, parses
// the version, and enforces the >= MinVersion floor. Every
// dependency-shaped failure is a typed report.DependencyError (exit 3
// class) with a message that names what is wrong:
//
//   - binary cannot be executed although LookPath found it;
//   - `xorriso -version` exits non-zero (status carried in the message);
//   - the probe exceeds ProbeTimeout (hung binary);
//   - the output yields no parseable version (raw output embedded,
//     bounded by rawSnippetLimit) - an unknown xorriso is never a pass;
//   - the parsed version is below the floor (found and required
//     versions both named).
//
// The caller's context cancellation is the one non-dependency outcome:
// it propagates as the raw context error.
func (t *Tool) Probe(ctx context.Context) (Info, error) {
	probeCtx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()

	res, runErr := t.Run(probeCtx, []string{"-version"})

	// Caller-driven cancellation (signal handling) outranks every
	// dependency classification: report it raw so exit mapping lands
	// on the Interrupted class (130), not the dependency class (3).
	if ctx.Err() != nil {
		return Info{Path: t.Path, Raw: string(res.Output)}, ctx.Err()
	}

	info := Info{Path: t.Path, Raw: string(res.Output)}
	switch {
	case runErr == nil:
		// fall through to parsing below
	case errors.Is(runErr, context.DeadlineExceeded):
		return info, &report.DependencyError{
			Missing: ToolName,
			Detail:  fmt.Sprintf("version probe timed out after %v; the binary did not answer `xorriso -version`", ProbeTimeout),
			Hints:   installHints,
			Err:     runErr,
		}
	default:
		var buildErr *BuildError
		if errors.As(runErr, &buildErr) {
			return info, &report.DependencyError{
				Missing: ToolName,
				Detail:  fmt.Sprintf("`xorriso -version` exited with status %d", buildErr.ExitCode),
				Hints:   installHints,
				Err:     runErr,
			}
		}
		// LookPath found it but exec could not start it (vanished
		// between discovery and probe, not executable, ...): still a
		// dependency problem, not a bug class.
		return info, &report.DependencyError{
			Missing: ToolName,
			Detail:  "found on PATH but could not be executed",
			Hints:   installHints,
			Err:     runErr,
		}
	}

	version, ok := parseVersion(string(res.Output))
	if !ok {
		return info, &report.DependencyError{
			Missing: ToolName,
			Detail:  fmt.Sprintf("could not parse a version from `xorriso -version` output: %s", snippet(string(res.Output), rawSnippetLimit)),
			Hints:   installHints,
		}
	}
	if !version.AtLeast(MinVersion) {
		return info, &report.DependencyError{
			Missing: ToolName,
			Detail:  fmt.Sprintf("found xorriso %s, need >= %s (DESIGN_SPEC 11 minimum)", version, MinVersion),
			Hints:   installHints,
		}
	}

	info.Version = version
	return info, nil
}

// snippet quotes at most the first max bytes of s for an error message,
// with an explicit omission marker when anything was cut.
func snippet(s string, max int) string {
	if len(s) <= max {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("%q [... %d more bytes omitted ...]", s[:max], len(s)-max)
}
