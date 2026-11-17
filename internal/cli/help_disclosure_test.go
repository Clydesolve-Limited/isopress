package cli

// U.3 (AC-35, DESIGN_SPEC Block 6d/6e/6f - the help-disclosure pins
// that were still unowned): substring pins over the REAL command tree
// for the three exit-code/help paragraphs the selective-unlock design
// specifies, following the established clear-assignments pattern
// (clear_scoped_test.go's TestClearHelpDisclosureAndNeverExits10 pins
// 6a/6b/6c the same way).
//
//   - Block 6d: copy's appended wipe-gate paragraph - the claim
//     clear's 6c middle sentence and clear.go's comment make about the
//     NEXT copy run REMOVING the released tree (true from U.3 on);
//   - Block 6e: plan's exit-2 clause (ErrPartialChunkReplan classified
//     under 10.1's "unsatisfiable combinations", both remedies with the
//     family leg's clear-only carve-out, the FOUR refusal reasons the
//     balancer raises, and the documented deviation from the fatal
//     exit-1 precedent);
//   - Block 6f: report's identical refusal clause for the projection.
//
// These are substring pins DELIBERATELY: they force the approved text
// into the shipped help (a drift in wording or in the exit-code class
// fails the test), without pinning the paragraph's neighbours.

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// exit2Clause6e is Block 6e's "2 usage" clause of plan --help, verbatim
// (U.4 widened: the FOUR refusal reasons the balancer actually raises
// - changed chunk profile, whole-file fit, stored rows that do not
// verify, and the secondary-excluded partial set - plus the family
// leg's clear-only carve-out; the 10.1 citation, both remedies, and the
// ErrItemTooLarge deviation sentence are the ratified text unchanged).
const exit2Clause6e = "2 usage (missing or invalid --size, invalid --media-profile, unknown flags; " +
	"or re-planning a partially-assigned chunk set whose stored chunks cannot be reused: " +
	"the effective chunk size changed (a changed chunk profile), the file now fits one " +
	"destination (a whole-file fit), the stored chunk rows do not verify against the " +
	"current decomposition, or the hard-link family rule excludes a secondary row - " +
	"ErrPartialChunkReplan, a user-correctable state/flag conflict " +
	"classified under DESIGN_SPEC 10.1's \"unsatisfiable combinations\", printed with both " +
	"remedies: keep the current media profile, or clear the whole set - except on the " +
	"family-rule leg, which carries only the clear-set remedy (no profile choice can " +
	"place a secondary row) - a deliberate " +
	"deviation from the ErrItemTooLarge fatal exit-1 precedent, which covers the " +
	"corrupt-catalog class)"

// exit2Clause6f is Block 6f's "2 usage" clause of report --help,
// verbatim (U.4 widened the same way as 6e, over the projection
// pipeline's own sentence structure).
const exit2Clause6f = "2 usage (missing or invalid --size, invalid --media-profile, a set that has " +
	"never been scanned; or a projection over a partially-assigned chunk set whose stored " +
	"chunks cannot be reused: a changed chunk profile, a whole-file fit, stored chunk rows " +
	"that do not verify against the current decomposition, or a secondary row the hard-link " +
	"family rule excludes - ErrPartialChunkReplan, exit 2 carrying both remedies (keep the " +
	"current media profile, or clear the whole set), except on the family-rule leg, which " +
	"carries only the clear-set remedy (no profile choice can place a secondary row); the " +
	"projection for a partial set is genuinely undefined under a changed profile, and this " +
	"is the same deliberate deviation from the ErrItemTooLarge fatal exit-1 precedent, " +
	"classified under DESIGN_SPEC 10.1's \"unsatisfiable combinations\")"

// wipeParagraph6d is Block 6d - copy's appended wipe-gate paragraph,
// verbatim (the approved round-3 text; "files" -> "entries" per N9).
const wipeParagraph6d = "Before writing an image whose catalog content list is empty (a first write, " +
	"or an image released by `clear-assignments --image`), copy REMOVES the existing " +
	"`<dest>/<SET-ID>-NNN` directory and records the removal (report line, `wipes` in " +
	"`--json`, and a catalog run-log row naming the path and the number of entries " +
	"removed). A directory whose `MANIFEST.json` names a different set or image, or is " +
	"unreadable as a manifest, is refused with exit 10 instead of removed: remove or " +
	"rename it yourself, or choose a different `--dest`."

// commandByName returns one command from the REAL root tree, or fails
// the test when the command is missing (the clear help-pin pattern).
func commandByName(t *testing.T, name string) *cobra.Command {
	t.Helper()
	for _, c := range NewRootCmd().Commands() {
		if c.Name() == name {
			return c
		}
	}
	t.Fatalf("%s missing from the root command tree", name)
	return nil
}

// TestCopyHelpWipeDisclosure pins Block 6d on the REAL copy command
// (AC-35): the paragraph that makes clear's 6c promise ("the next copy
// run first REMOVES the existing directory") and clear.go's comment
// TRUE from U.3 on.
func TestCopyHelpWipeDisclosure(t *testing.T) {
	c := commandByName(t, "copy")
	if !strings.Contains(c.Long, wipeParagraph6d) {
		t.Errorf("copy --help lacks the Block 6d wipe paragraph verbatim:\n%s", c.Long)
	}
	// The two claims 6c/6d share must both be present and consistent.
	for _, want := range []string{
		"copy REMOVES the existing `<dest>/<SET-ID>-NNN` directory",
		"`wipes` in `--json`",
		"refused with exit 10 instead of removed",
	} {
		if !strings.Contains(c.Long, want) {
			t.Errorf("copy --help lacks %q:\n%s", want, c.Long)
		}
	}
}

// TestPlanAndReportHelpExit2Clauses pins Blocks 6e/6f on the REAL
// plan and report commands (AC-35): the ratified ErrPartialChunkReplan
// classification (usage, exit 2, 10.1 "unsatisfiable combinations"),
// BOTH remedies with the family leg's clear-only carve-out, all FOUR
// refusal reasons the balancer can raise, and the documented deviation
// from the ErrItemTooLarge fatal exit-1 precedent.
func TestPlanAndReportHelpExit2Clauses(t *testing.T) {
	for _, tc := range []struct{ cmd, clause string }{
		{"plan", exit2Clause6e},
		{"report", exit2Clause6f},
	} {
		c := commandByName(t, tc.cmd)
		if !strings.Contains(c.Long, tc.clause) {
			t.Errorf("%s --help lacks its exit-2 clause verbatim:\nhave: %s\nwant substring: %s",
				tc.cmd, c.Long, tc.clause)
		}
		for _, want := range []string{
			"ErrPartialChunkReplan",
			"keep the current media profile, or clear the whole set",
			`DESIGN_SPEC 10.1's "unsatisfiable combinations"`,
			// U.4: the four-reason set must be NAMED in both clauses
			// (the help-vs-behavior audit reads these strings), and the
			// secondary's clear-only carve-out must be carved out.
			"changed chunk profile",
			"whole-file fit",
			"stored chunk rows",
			"hard-link family rule",
			"only the clear-set remedy (no profile choice can place a secondary row)",
		} {
			if !strings.Contains(c.Long, want) {
				t.Errorf("%s --help lacks %q:\n%s", tc.cmd, want, c.Long)
			}
		}
	}
}
