package xorriso

// Discovery of the external xorriso binary (T4.1, DESIGN_SPEC 1.2
// external integration layer, 10.1 DependencyError, NFR-004).
//
// Discovery is exec.LookPath only: the tool never ships xorriso, never
// probes network locations, and never guesses install prefixes. When the
// binary is absent the returned error is the typed
// report.DependencyError carrying per-distribution install hints, which
// the cli wrapper prints to stderr and maps to exit 3 (DESIGN_SPEC 10.1
// and 10.2). Only image operations call Discover; copy mode and copy
// verification never do, so a copy-only workflow works on hosts without
// xorriso (NFR-004, IMPLEMENTATION_PLAN T4.1 acceptance).

import (
	"errors"
	"fmt"
	"io/fs"
	"os/exec"

	"isopress/internal/report"
)

// ToolName is the external binary this package wraps, the only binary
// it ever executes.
const ToolName = "xorriso"

// installHints are the per-distribution install hints required by
// DESIGN_SPEC 10.1 ("Message states what is missing and per-distro
// install hints"). They are rendered by report.DependencyError's message
// as "(install: ...; ...)".
var installHints = []string{
	"Debian/Ubuntu: apt install xorriso",
	"Fedora: dnf install xorriso",
	"Arch: pacman -S xorriso",
	"openSUSE: zypper install xorriso",
}

// Tool is a discovered xorriso binary. Construct it with Discover (or in
// tests with an explicit Path pointing at a fake script); it carries no
// state beyond the resolved path, and its methods are safe for
// sequential use.
type Tool struct {
	// Path is the executable path resolved by exec.LookPath. It is the
	// only binary Run and Probe ever execute.
	Path string
}

// lookPathDetail renders the DependencyError.Detail from the SHAPE of a
// failed exec.LookPath, so the message never claims "not found on PATH"
// when the real failure was a found-but-unexecutable file or a
// relative-path ErrDot refusal. The underlying LookPath error stays in
// DependencyError.Err and is rendered by its Error method, so nothing is
// lost in the fallback branch either; the classification chain (and the
// install hints) are unaffected.
func lookPathDetail(err error) string {
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return "not found on PATH"
	case errors.Is(err, exec.ErrDot):
		return "found via a relative PATH entry (e.g. \".\"), which os/exec refuses to execute"
	case errors.Is(err, fs.ErrPermission):
		return "found on PATH but not executable (permission denied)"
	default:
		return fmt.Sprintf("PATH lookup failed: %v", err)
	}
}

// Discover locates xorriso on PATH via exec.LookPath. On success it
// returns a Tool ready for Probe and Run. On failure (missing,
// permission-denied, a bare "./xorriso" ErrDot hit) it returns a typed
// report.DependencyError naming the tool, carrying the per-distro
// install hints, and describing the SHAPE of the LookPath failure in
// its Detail; report.Classify maps it to exit 3. Discovery alone does
// NOT enforce the version floor - the binary may exist and be too old;
// Tool.Probe performs that second half of the NFR-004 fail-fast check.
func Discover() (*Tool, error) {
	path, err := exec.LookPath(ToolName)
	if err != nil {
		return nil, &report.DependencyError{
			Missing: ToolName,
			Detail:  lookPathDetail(err),
			Hints:   installHints,
			Err:     err,
		}
	}
	return &Tool{Path: path}, nil
}

// String renders the tool path for logs and error messages.
func (t *Tool) String() string {
	if t == nil || t.Path == "" {
		return fmt.Sprintf("%s (not discovered; call Discover first)", ToolName)
	}
	return t.Path
}
