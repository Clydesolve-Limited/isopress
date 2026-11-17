package cli

// The once-per-run change-detection resolution and warning wiring
// (DESIGN_SPEC 3.5, RESOLVED 2026-10-20 - the stakeholder CIFS
// failure). The three detection-point commands (scan, copy, build)
// share this file so the resolution, the warning, and the
// runs.stats_json audit record have exactly ONE implementation each:
//
//   - resolveChangeDetection resolves the run's fsmeta.Mode from the
//     SOURCE ROOT - scan: the ROOT positional; copy/build: the
//     catalog's set source_root - via fsmeta.ResolveChangeDetection
//     (one statfs per run). An invalid flag value (the sentinel
//     fsmeta.ErrInvalidChangeFlag; unreachable through the CLI because
//     validateGlobalFlags gates it eagerly, wrapped here for defense
//     in depth) becomes a report.UsageError (exit 2).
//   - warnChangeDetection emits the 3.5 once-per-run degradation
//     warning - progress.Warn on stderr AND a run_events warn row -
//     ONLY when the mode is metadata-via-DETECTION. The distinction is
//     deliberate: the warning informs an AUTOMATIC degradation the
//     user did not ask for; a user-FORCED --change-detection=metadata
//     is their explicit choice (documented in 3.5 and on
//     fsmeta.Mode.DegradedViaDetection) and stays silent. The message
//     names the source path, the detected class, the degraded set,
//     AND the retained content guarantee.
//   - changeDetectionAudit is the runs.stats_json record (mode, class,
//     provenance). DB-side audit only: the frozen --json REPORT
//     key-sets of scan/copy/build are untouched.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"isopress/internal/catalog"
	"isopress/internal/fsmeta"
	"isopress/internal/progress"
	"isopress/internal/report"
)

// resolveChangeDetection resolves the run's ONE change-detection Mode
// from sourceRoot (see the file doc). It is called exactly once per
// scan/copy/build run.
func resolveChangeDetection(cmd *cobra.Command, opts *rootOptions, sourceRoot string) (fsmeta.Mode, error) {
	mode, err := fsmeta.ResolveChangeDetection(sourceRoot, opts.changeDetection)
	if err != nil {
		if errors.Is(err, fsmeta.ErrInvalidChangeFlag) {
			return mode, &report.UsageError{Command: cmd.CommandPath(), Err: err}
		}
		return mode, err
	}
	return mode, nil
}

// warnChangeDetection emits the 3.5 degradation warning for the ONE
// resolved mode: nothing for full modes and nothing for user-FORCED
// metadata (the DegradedViaDetection predicate); otherwise one
// progress.Warn (stderr) naming source path, class, degraded set, and
// the retained hash-verify guarantee, plus ONE run_events warn row for
// the audit trail. bus may be nil (silenced emission, row still
// written); the row insert is best-effort during an interrupted tail
// (the copy/build convention: report lines stay the authoritative
// record when the run context is already cancelled).
func warnChangeDetection(ctx context.Context, db *catalog.DB, runID int64, bus *progress.Bus, sourceRoot string, mode fsmeta.Mode) {
	if !mode.DegradedViaDetection() {
		return
	}
	msg := changeDetectionWarnMsg(sourceRoot, mode)
	if bus != nil {
		bus.Emit(progress.Warn{Msg: msg})
	}
	if db != nil && ctx.Err() == nil {
		_ = db.InsertRunEvent(ctx, catalog.RunEvent{
			RunID: runID, At: time.Now().UnixNano(), Level: "warn", Detail: msg,
		})
	}
}

// changeDetectionWarnMsg renders the 3.5 warning text (the spec's
// example wording, class-adapted): source path, detected class, the
// degraded set, and the RETAINED guarantee - read-back verify (FR-014)
// still gates every write against the scan-time hash, so replaced or
// corrupt content still fails verification.
func changeDetectionWarnMsg(sourceRoot string, mode fsmeta.Mode) string {
	return fmt.Sprintf(
		"source %s is %s (synthesized, unstable inode numbers); change detection degraded to size+mtime (dev and inode ignored). "+
			"Content is still SHA-256 verified against the scan-time hash on every write (FR-014), so replaced or corrupt data still fails verification",
		sourceRoot, classDisplay(mode.Class))
}

// classDisplay uppercases the audit class for the warning sentence
// ("cifs" -> "CIFS"); unknown classes pass through.
func classDisplay(class string) string {
	switch class {
	case fsmeta.ClassCIFS:
		return "CIFS"
	case fsmeta.ClassSMB3:
		return "SMB3"
	case fsmeta.ClassNFS:
		return "NFS"
	case fsmeta.ClassFUSE:
		return "FUSE"
	case fsmeta.ClassLocal:
		return "a local filesystem"
	case fsmeta.ClassOther:
		return "an unclassified filesystem"
	default:
		return class
	}
}

// changeDetectionAudit is the runs.stats_json audit record of the
// resolved mode (3.5: "the resolved mode and detected class are
// additionally recorded into runs.stats_json ... informational only
// and never drives comparison"). It is merged into each command's
// stats document beside its own counters; the frozen --json REPORT
// key-sets never see it.
type changeDetectionAudit struct {
	Mode       string `json:"mode"`       // "full" | "metadata"
	Class      string `json:"class"`      // detected fs class ("unknown" when statfs failed)
	Provenance string `json:"provenance"` // "detected" (auto) | "forced" (flag)
}

// auditOf builds the stats record for a resolved mode.
func auditOf(mode fsmeta.Mode) changeDetectionAudit {
	class := mode.Class
	if class == "" {
		class = fsmeta.ClassUnknown
	}
	return changeDetectionAudit{
		Mode: mode.SetName(), Class: class, Provenance: mode.ProvenanceString(),
	}
}
