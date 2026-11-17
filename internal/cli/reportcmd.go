package cli

// T2.3: the real `report` command (DESIGN_SPEC 1.1 command tree, 4.3
// "The `report` command runs the identical pipeline read-only (no
// transaction)", 4.4 D-05 waste visibility, 4.5, 6.5 D-06 margin
// echo; PLAN_SPEC FR-009 "without writing any media"; IMPLEMENTATION_PLAN
// T2.3). It replaces the T0.5 placeholder in commands.go.
//
// FILE NAME NOTE: the command is named "report", but a file called
// report.go would collide - readability-wise and in every import
// discussion - with internal/report (the exit-code taxonomy package
// this file imports). The command file is therefore reportcmd.go; the
// cobra command name stays exactly "report" per DESIGN_SPEC 1.1.
//
// The pipeline is SHARED with `plan` (runPlanReport in planreport.go):
// same flag pair (--size, --media-profile), same set resolution, same
// plan.Balance compute, same renderer. The one difference is the
// catalog open and the write step:
//
//   - READ-ONLY open (catalog.OpenReadOnly, URI mode=ro): NO advisory
//     flock at all (DESIGN_SPEC 2.1 as amended 2026-10-18, Option B),
//     so a report runs concurrently with any mutating run over a WAL
//     snapshot and can NEVER fail with the busy class (exit 5) -
//     reportcmd_test.go drives a real report while another process
//     holds the mutating lock and asserts success.
//   - ZERO WRITES: the mode gate in runPlanReport never invokes the
//     write hook in report mode, and the hook wired here
//     (refuseWrites) errors if it is ever reached - a tripwire on top
//     of the gate. The zero-write proof is doubled by the test suite:
//     an injected hook that fails the test if invoked (the seam), and
//     a byte-identical catalog comparison around a real report run
//     (the end-to-end property).
//
// Output is the IDENTICAL projection report as plan (FR-008/FR-009
// media count if planned now, per-destination fill including locked
// destinations' current fill and the residual waste figure, D-06
// margins), headed "report (projection)" in human form and carrying
// "mode": "report" in the frozen --json schema.

import (
	"github.com/spf13/cobra"
)

// newReportCommand builds the real report command (replaces the T0.5
// placeholder). No positional arguments; every input arrives through
// flags, exactly the plan pair plus the globals.
func newReportCommand(opts *rootOptions) *cobra.Command {
	po := planOptions{}
	cmd := &cobra.Command{
		Use:   "report",
		Short: "show the current plan: media count, fill, locked-gap waste (read-only)",
		Long: "Run the identical planning pipeline as `plan` - strictly read-only, in no " +
			"transaction, with zero writes to the catalog (DESIGN_SPEC 4.3, FR-009) - and " +
			"print the PROJECTION: how many destinations the set requires if planned now " +
			"under the given --size and --media-profile, the per-destination fill " +
			"including locked destinations' current fill, the residual space on locked " +
			"destinations (the D-05 accept-waste cost), and the profile's nominal vs " +
			"usable figures (D-06). The catalog is opened read-only (no advisory lock), so " +
			"report runs concurrently with any mutating run over a WAL snapshot. Unlike " +
			"scan, a missing catalog file at --catalog is NOT created (exit 4): run scan " +
			"first. Progress " +
			"renders on stderr; the projection lands on stdout (JSON with --json).\n\n" +
			"Exit codes: 0 success, including 'nothing to plan'; 1 fatal unexpected error; 2 usage " +
			"(missing or invalid --size, invalid --media-profile, a set that has never been scanned; " +
			"or a projection over a partially-assigned chunk set whose stored chunks cannot be " +
			"reused: a changed chunk profile, a whole-file fit, stored chunk rows that do not " +
			"verify against the current decomposition, or a secondary row the hard-link family " +
			"rule excludes - " +
			"ErrPartialChunkReplan, exit 2 carrying both remedies (keep the current media profile, " +
			"or clear the whole set), except on the family-rule leg, which carries only the " +
			"clear-set remedy (no profile choice can place a secondary row); the projection for a " +
			"partial set is genuinely undefined under " +
			"a changed profile, and this is the same deliberate deviation from the ErrItemTooLarge " +
			"fatal exit-1 precedent, classified under DESIGN_SPEC 10.1's \"unsatisfiable " +
			"combinations\"); 4 catalog error, including a missing catalog file; 130 interrupted. " +
			"report never exits 5 " +
			"(read-only open, no advisory lock), never exits 3 (it never invokes xorriso) and never " +
			"exits 10 (no per-item failure class), so --strict has no effect. JSON: --json prints the " +
			"final report as exactly one JSON document on stdout; a run that aborts before doing work " +
			"prints no document at all; --json never changes the exit code.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			// refuseWrites is the tripwire: report mode's mode gate
			// (planreport.go) never invokes it; it exists to fail loudly
			// if that gate ever regressed.
			return runPlanReport(cmd.Context(), cmd, opts, po, modeReport, refuseWrites)
		},
	}
	addPlanFlags(cmd, &po)
	return cmd
}
