package cli

// DOC <-> HELP / DOC <-> CONTRACT PARITY (U.4 release gate).
//
// The U.4 round widened plan/report's exit-2 clause to the FOUR
// ErrPartialChunkReplan refusal reasons with the family leg's
// clear-only carve-out, and Track B echoed that text into README's
// exit-code table plus the two frozen --json key enumerations. Every
// one of those claims was verified by hand once; these pins make the
// verification repeatable, because nothing else in the tree links the
// prose back to the code:
//
//   - help_disclosure_test.go pins plan/report/copy --help against
//     CONSTANTS (the help side);
//   - json_contract_test.go / clear_test.go pin the BINARY's key sets
//     against copyJSONKeys / clearReportJSONKeys (the code side);
//   - qualitygate_parity_test.go pins README's gate recipes (the
//     doc-reading precedent this file follows).
//
// What was missing - and what this file adds - is the cross-links:
// the README exit-code row vs. plan --help (a one-sided edit now fails
// here), and the README key enumerations vs. the frozen key literals
// (a typo'd or dropped key name now fails here).

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// The four-reason sentence as it must appear, byte-identical, in BOTH
// README's exit-code row and plan --help (report --help carries the
// same four reasons over its own sentence shape, which
// TestPlanAndReportHelpExit2Clauses pins on the help side).
const readmePlanFourReasons = "the effective chunk size changed (a changed chunk profile), the file now fits one " +
	"destination (a whole-file fit), the stored chunk rows do not verify against the " +
	"current decomposition, or the hard-link family rule excludes a secondary row"

// The two remedy clauses that qualify those four reasons.
const (
	readmeBothRemedies    = "keep the current media profile, or clear the whole set"
	readmeFamilyCarveOut  = "except on the family-rule leg, which carries only the clear-set remedy (no profile choice can place a secondary row)"
	readmePartialSetIntro = "a partially-assigned chunk set whose stored chunks cannot be reused"
)

// readmeDoc reads README.md out of the module root (moduleRoot is the
// deps_test.go/qualitygate_parity_test.go precedent).
func readmeDoc(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(moduleRoot, "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	return string(b)
}

// readmeExit2Row returns README's "| 2 | ..." exit-code table row.
func readmeExit2Row(t *testing.T, readme string) string {
	t.Helper()
	for _, line := range strings.Split(readme, "\n") {
		if strings.HasPrefix(line, "| 2 |") {
			return line
		}
	}
	t.Fatalf("README.md has no '| 2 |' exit-code row anymore (the four-reason row must stay)")
	return ""
}

// TestREADMEExit2RowMatchesPlanHelpFourReasons is the help-vs-doc
// release gate: the four refusal reasons, the both-remedies clause and
// the family leg's clear-only carve-out must be present, word for
// word, in README's exit-code row AND in plan --help. A paraphrase on
// either side (or a fifth reason added to the balancer's help text
// without the README echo) fails here.
func TestREADMEExit2RowMatchesPlanHelpFourReasons(t *testing.T) {
	readme := readmeDoc(t)
	row := readmeExit2Row(t, readme)
	planHelp := commandByName(t, "plan").Long
	reportHelp := commandByName(t, "report").Long

	for _, want := range []string{
		readmePartialSetIntro,
		readmePlanFourReasons,
		readmeBothRemedies,
		readmeFamilyCarveOut,
		"ErrPartialChunkReplan",
	} {
		if !strings.Contains(row, want) {
			t.Errorf("README's exit-2 row lacks %q:\n%s", want, row)
		}
		if !strings.Contains(planHelp, want) {
			t.Errorf("plan --help lacks %q (README's exit-2 row quotes it verbatim):\n%s", want, planHelp)
		}
	}
	// README alone frames the clause as a four-reason set (the help
	// enumerates them without counting them).
	if !strings.Contains(row, "four reasons") {
		t.Errorf("README's exit-2 row no longer frames the refusal as 'four reasons':\n%s", row)
	}
	// report --help carries the same set over its own sentence shape
	// (6f): the shared intro and the shared remedy/carve-out wording
	// must not drift either.
	for _, want := range []string{
		readmePartialSetIntro,
		readmeBothRemedies,
		readmeFamilyCarveOut,
	} {
		if !strings.Contains(reportHelp, want) {
			t.Errorf("report --help lacks %q:\n%s", want, reportHelp)
		}
	}
	// The row must still scope the claim to plan and report - the
	// other seven commands have no ErrPartialChunkReplan path.
	if !strings.Contains(row, "`plan` and `report`") {
		t.Errorf("README's exit-2 row no longer scopes the refusal to `plan` and `report`:\n%s", row)
	}
}

// readmeKeyEnumeration pulls the backticked key list that follows
// marker (e.g. "13 top-level keys: `set_id`, ..., `wipes`. ...") and
// returns the keys in the order README lists them. The enumeration
// ends at the first sentence stop after the marker, so the prose that
// follows (which re-quotes individual keys) is excluded.
func readmeKeyEnumeration(t *testing.T, readme, marker string) []string {
	t.Helper()
	at := strings.Index(readme, marker)
	if at < 0 {
		t.Fatalf("README.md no longer documents %q - the frozen-key enumeration moved; update this pin with it", marker)
	}
	rest := readme[at+len(marker):]
	end := strings.Index(rest, ". ")
	if end < 0 {
		t.Fatalf("README.md's %q enumeration has no sentence end after it", marker)
	}
	var keys []string
	for i, part := range strings.Split(rest[:end], "`") {
		if i%2 == 1 {
			keys = append(keys, part)
		}
	}
	return keys
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

// TestREADMEJSONKeyEnumerationsMatchTheFrozenKeySets pins the two
// enumerations U.4 added to README against the frozen key literals the
// binary's own contract tests use. The stated counts (10 clear / 13
// copy) and every key NAME must match; a typo, a drop, or a stale
// count in the prose fails here instead of in a reader's jq run.
func TestREADMEJSONKeyEnumerationsMatchTheFrozenKeySets(t *testing.T) {
	readme := readmeDoc(t)

	for _, tc := range []struct {
		marker string
		count  string
		want   []string
		what   string
	}{
		{"10 top-level keys in both modes:", "10 top-level keys", clearReportJSONKeys, "clear-assignments --json"},
		{"13 top-level keys:", "13 top-level keys", copyJSONKeys, "copy --json"},
	} {
		if !strings.Contains(readme, tc.count) {
			t.Errorf("README.md no longer claims %q for %s", tc.count, tc.what)
		}
		got := readmeKeyEnumeration(t, readme, tc.marker)
		if len(got) != len(tc.want) {
			t.Errorf("README lists %d keys after %q, want %d: %v", len(got), tc.marker, len(tc.want), got)
		}
		if !reflect.DeepEqual(sortedCopy(got), sortedCopy(tc.want)) {
			t.Errorf("README's %s key set diverges from the frozen key set\nREADME:  %v\nfrozen:  %v",
				tc.what, sortedCopy(got), sortedCopy(tc.want))
		}
	}

	// The wipe record shape README documents for `wipes` - four fields,
	// always present - is the same shape the copy report marshals.
	// README wraps these sentences across lines, so they are matched on
	// a single-spaced copy of the file.
	flat := strings.Join(strings.Fields(readme), " ")
	for _, want := range []string{
		"`wipes` is the copy-time",
		"`image`, `path`, `entries_removed` and `bytes_removed`",
		"The key is always present",
		"`unlocked_images` is the selective-unlock addition",
	} {
		if !strings.Contains(flat, want) {
			t.Errorf("README.md lacks the frozen-shape sentence %q", want)
		}
	}
}
