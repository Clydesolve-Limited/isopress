package cli

// Set/run-identity line gap tests (verification round for the
// 2026-10-19 stakeholder request). scan_test.go pins the HUMAN format
// (golden + e2e) and scan_hardening_test.go keeps the frozen --json
// schema; this file closes the remaining audit gaps around the new
// "set: <set_id> (run <run_id>)" line:
//
//   - the --json stdout must stay a SINGLE JSON document with the
//     human identity line ABSENT - the line is a human-form affordance
//     and must never leak into machine output (the frozen key-set test
//     would catch it as a trailing garbage document, but the failure
//     mode deserves its own explicit pin, and the JSON doc must still
//     carry the same identity so --json consumers lose nothing).

import (
	"strings"
	"testing"

	"isopress/internal/report"
)

// TestScanJSONStdoutCarriesIdentityNotHumanSetLine: under --json the
// stdout is exactly one JSON document (decodeSingleJSONDoc enforces
// "nothing after it"), the human "set: ... (run ...)" line must NOT
// appear anywhere on stdout, and the identity the human line carries
// is still present in the document itself (set_id/run_id keys).
func TestScanJSONStdoutCarriesIdentityNotHumanSetLine(t *testing.T) {
	root := makeFixture(t)
	cat := createCatalog(t)
	args := []string{"--catalog", cat, "--set-id", "S-JLINE", "--json"}
	for _, p := range fixtureExcludes() {
		args = append(args, "--exclude", p)
	}
	stdout, stderr, code := runScanChild(t, append(args, root)...)
	if code != report.ExitOK {
		t.Fatalf("scan --json exit = %d, want 0\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	// Exactly one JSON document; nothing trails it (this alone would
	// fail if the human line leaked after the document).
	doc := decodeSingleJSONDoc(t, stdout)

	// The human identity line must not appear on stdout in --json
	// mode. The JSON member is `"set_id": "..."` (no "set: " prefix),
	// so this substring can only come from the human-form line.
	if strings.Contains(stdout, "set: ") {
		t.Errorf("the human 'set:' identity line leaked into --json stdout:\n%s", stdout)
	}

	// The identity is still available to machine consumers via the
	// frozen keys - the human line adds information, never moves it.
	if doc["set_id"] != "S-JLINE" {
		t.Errorf("--json set_id = %v, want S-JLINE", doc["set_id"])
	}
	if run, ok := doc["run_id"].(float64); !ok || run < 1 {
		t.Errorf("--json run_id = %v, want a positive number", doc["run_id"])
	}
}
