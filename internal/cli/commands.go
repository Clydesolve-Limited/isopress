package cli

// T0.5: the nine commands of DESIGN_SPEC 1.1. Three of them were
// stubs wired to nothing (IMPLEMENTATION_PLAN T0.5): each parsed the
// global flags and printed a not-yet-implemented placeholder to stderr
// (FR-046: stderr is the progress/warning channel; stdout stays
// reserved for results), then succeeded with exit 0. No business logic
// lives here (DESIGN_SPEC 1.2): each command's service wiring, its own
// flags, and its argument validation arrive with that command's task
// (T5.1 verify-media, T5.2 validate, T5.3 restore).
//
// T1.6: the `scan` STUB WAS REPLACED by the real command (scan.go).
// T2.3: the `plan` and `report` STUBS ARE REPLACED by the real
// commands (plan.go and reportcmd.go; shared pipeline and renderer in
// planreport.go). T2.4: the `clear-assignments` STUB IS REPLACED by
// the real command (clear.go). T3.3: the `copy` STUB IS REPLACED by
// the real command (copy.go). T4.3/T4.4: the `build` STUB IS REPLACED
// by the real command (build.go). T5.1: the `verify-media` STUB IS
// REPLACED by the real command (verifymedia.go). T5.2: the `validate`
// STUB IS REPLACED by the real command (validate.go). T5.3: the
// `restore` STUB - the LAST placeholder - IS REPLACED by the real
// command (restore.go): no stubs remain, so newStubCommand is gone
// with them. newCommands builds the full nine-command surface in the
// 1.1 tree order, sharing the parsed global options.

import (
	"github.com/spf13/cobra"
)

// newCommands builds the nine subcommands in the DESIGN_SPEC 1.1 tree
// order, sharing the parsed global options: every command is real since
// T5.3 (scan T1.6, plan/report T2.3, clear-assignments T2.4, copy T3.3,
// build T4.4, verify-media T5.1, validate T5.2, restore T5.3).
func newCommands(opts *rootOptions) []*cobra.Command {
	return []*cobra.Command{
		newScanCommand(opts),
		newPlanCommand(opts),
		newReportCommand(opts),
		newClearAssignmentsCommand(opts),
		newCopyCommand(opts),
		newBuildCommand(opts),
		newVerifyMediaCommand(opts),
		newValidateCommand(opts),
		newRestoreCommand(opts),
	}
}
