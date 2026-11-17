package cli

// T2.3: the real `plan` command (DESIGN_SPEC 1.1 command tree, 4.1
// capacity semantics, 4.3 media count FR-008/FR-009, 4.4 D-05 waste
// quantification, 4.5 single-transaction assignment, 6.5 D-06 margin
// echo; IMPLEMENTATION_PLAN T2.3). It replaces the T0.5 placeholder in
// commands.go; the report command (reportcmd.go) shares the same
// pipeline and renderer (planreport.go).
//
// Wiring contracts (all honored in runPlanReport, planreport.go):
//
//   - MUTATING catalog open (exclusive flock, D-11): contention
//     surfaces as *catalog.CatalogBusy and report.Classify maps it to
//     exit 5 (T0.4 wiring). A missing --catalog path is NOT
//     auto-created (unlike scan): there is nothing to plan in an empty
//     database, and creation is scan's job - a missing path is a
//     CatalogError (exit 4).
//   - Flags: the global set (--catalog, --set-id, --json, --quiet,
//     --strict) plus the plan-local --size and --media-profile of 4.1
//     (DESIGN_SPEC 1.1 fixes only the global flags; --media-profile
//     is named by 4.1 with default "disk" per units.DefaultProfile;
//     --size is this implementation's documented name for FR-006's
//     user-stated destination size). Every bad/missing input is a
//     usage-class error (exit 2, 10.1).
//   - Pipeline (4.3): plan.Balance computes the deterministic WFD
//     placement metadata-only (zero source reads, 12.5), then
//     Plan.Write persists it in ONE BeginImmediate transaction (4.5):
//     insert images rows, create/replace chunk state, set
//     files.image_id / chunks.image_id / directories.image_id. A plan
//     with no unassigned items is a NO-OP (T2.2): no transaction, no
//     empty destinations, exit 0 with a clear "nothing to plan"
//     report.
//   - Report on stdout (FR-046, 10.3): the shared renderer prints the
//     media count (FR-008/009), one fill line per destination, the
//     D-05 residual summary ("residual space on locked destinations:
//     X across N images", 4.4), the D-06 profile-margin echo (6.5),
//     and the 4.2 re-chunk / re-hash statements; --json emits the
//     frozen-schema document. Progress phase + one summary line on
//     stderr through the T1.1 bus.
//   - Exit codes: nil -> 0; usage inputs -> 2 (INCLUDING
//     ErrPartialChunkReplan: a partially-assigned chunk set whose
//     stored chunks cannot be reused - a changed chunk profile, a
//     whole-file fit, stored rows that do not verify against the
//     current decomposition, or a secondary the hard-link family rule
//     excludes (that leg carries only the clear-whole-set remedy, no
//     profile choice can place a secondary row) - is the
//     user-correctable 10.1 "unsatisfiable combinations" class,
//     wrapped as report.UsageError by plan/report - NOT the fatal
//     class); catalog problems -> 4;
//     lock contention -> 5; everything unexpected -> 1
//     (report.Classify in the execute wrapper). --strict has no
//     promotion target in this command: the plan pipeline has NO
//     per-item failure path (Balance errors are fatal-class by
//     construction EXCEPT ErrPartialChunkReplan - see
//     plan.ErrItemTooLarge for the fatal class - and the write is one
//     atomic transaction), so there is nothing for --strict to
//     promote; the flag is accepted and documented here as inert for
//     plan.

import (
	"github.com/spf13/cobra"
)

// newPlanCommand builds the real plan command (replaces the T0.5
// placeholder). No positional arguments; every input arrives through
// flags.
func newPlanCommand(opts *rootOptions) *cobra.Command {
	po := planOptions{}
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "assign unassigned catalog items deterministically to destinations",
		Long: "Compute the deterministic worst-fit-decreasing placement of every active, " +
			"unassigned item of the --set-id set (oversized files are chunked mandatory-only, " +
			"FR-041) and persist it in ONE transaction: destination rows, file, chunk, and " +
			"empty-directory assignments (DESIGN_SPEC 4.3, 4.5). Existing destinations are " +
			"locked (FR-011): new items only ever open new destinations. The report on stdout " +
			"states the media count, per-destination fill, the residual space on locked " +
			"destinations (D-05), and the capacity profile's nominal vs usable figures (D-06). " +
			"--size takes a nominal destination size; a bare number means decimal GB, G/GB " +
			"decimal and Gi/GiB binary (DESIGN_SPEC 4.1). Unlike scan, a missing catalog " +
			"file at --catalog is NOT created (exit 4): run scan first. Progress renders on stderr; the " +
			"result lands on stdout (JSON with --json). A run with nothing unassigned is a " +
			"no-op that exits 0 with a 'nothing to plan' report.\n\n" +
			"Exit codes: 0 success, including a 'nothing to plan' report; 1 fatal unexpected error; " +
			"2 usage (missing or invalid --size, invalid --media-profile, unknown flags; or " +
			"re-planning a partially-assigned chunk set whose stored chunks cannot be reused: " +
			"the effective chunk size changed (a changed chunk profile), the file now fits one " +
			"destination (a whole-file fit), the stored chunk rows do not verify against the " +
			"current decomposition, or the hard-link family rule excludes a secondary row - " +
			"ErrPartialChunkReplan, a user-correctable state/flag conflict " +
			"classified under DESIGN_SPEC 10.1's \"unsatisfiable combinations\", printed with both " +
			"remedies: keep the current media profile, or clear the whole set - except on the " +
			"family-rule leg, which carries only the clear-set remedy (no profile choice can " +
			"place a secondary row) - a deliberate " +
			"deviation from the ErrItemTooLarge fatal exit-1 precedent, which covers the " +
			"corrupt-catalog class); 4 catalog " +
			"error, including a missing catalog file; 5 catalog busy (another mutating run holds the " +
			"lock); 130 interrupted. --strict is accepted but has no effect: a plan commits in one " +
			"atomic transaction, so plan has no per-item failure class and never exits 10. JSON: " +
			"--json prints the final report as exactly one JSON document on stdout; a run that aborts " +
			"before doing work prints no document at all; --json never changes the exit code.",
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPlanReport(cmd.Context(), cmd, opts, po, modePlan, writeAssignments)
		},
	}
	// The --media-profile default is units.DefaultProfile ("disk"),
	// exactly the DESIGN_SPEC 4.1 wording ("applied by --media-profile
	// (default disk)"), registered by addPlanFlags.
	addPlanFlags(cmd, &po)
	return cmd
}
