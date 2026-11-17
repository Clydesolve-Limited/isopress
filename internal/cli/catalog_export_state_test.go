package cli

// T6.5 CATALOG-HONESTY / 6.6 EXPORT-SNAPSHOT interaction pin.
//
// RecordCopyFinalPath stamps images.mode='copy' + images.state='built'
// on the LIVE catalog at copy completion (T6.5 part A), while the
// on-media catalog-export.sqlite is ONE VACUUM-INTO snapshot produced
// at RUN START (6.6 / FR-035: copy.go's produceRunExport runs before
// any image is written, and the SAME bytes embed into every image).
// The two therefore legitimately DISAGREE on first-run media, and this
// test pins both halves plus the consumer half:
//
//   - the live row carries the completion stamp;
//   - the embedded export still carries the plan-time row (mode NULL,
//     state 'planned') - the run-start snapshot semantics;
//   - media-alone restore (`restore --from-media` over media whose
//     ORIGINAL CATALOG IS DELETED) succeeds over that snapshot, which
//     is the behavioral proof that the media-alone flow never branches
//     on images.state / images.mode (a grep-level claim until now: the
//     only images reader in the flow is CopyImageIDs' image_id column).
//     Legacy pre-T6.5 media produces exactly this 'planned' snapshot,
//     so the pin is also the legacy-media guarantee.
//
// If either the export timing or a future images.state predicate lands
// in the media-alone path, this test fails on the half that moved.

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"isopress/internal/media"
	"isopress/internal/report"
)

func TestCopyExportSnapshotCarriesRunStartImageStateAndMediaAloneRestores(t *testing.T) {
	const setID = "S-EXPSNAP"
	cat, dest, root := fromMediaRichTwoImageFixture(t, setID)

	// --- half 1: the LIVE catalog carries the T6.5 completion stamp ---
	live := catOpen(t, cat)
	var lState, lMode sql.NullString
	if err := live.QueryRow(`SELECT state, mode FROM images WHERE set_id = ? AND image_id = 1`, setID).
		Scan(&lState, &lMode); err != nil {
		t.Fatalf("reading the live images row: %v", err)
	}
	if lState.String != "built" || lMode.String != "copy" {
		t.Errorf("live images row = (state %q, mode %q), want ('built', 'copy') - the T6.5 completion stamp must hold on the live catalog",
			lState.String, lMode.String)
	}

	// --- half 2: the ON-MEDIA export is the run-start snapshot -------
	// Read the published bytes from the media dir (copied to a scratch
	// path first: the copy_test export pattern - no sidecar may ever be
	// created inside a media directory).
	exportMediaPath := filepath.Join(destImageDir(dest, setID, 1), media.ExportFileName)
	exportBytes, err := os.ReadFile(exportMediaPath)
	if err != nil {
		t.Fatalf("reading the on-media export: %v", err)
	}
	exportScratch := filepath.Join(t.TempDir(), media.ExportFileName)
	if err := os.WriteFile(exportScratch, exportBytes, 0o644); err != nil {
		t.Fatalf("staging the on-media export: %v", err)
	}
	exportDB := catOpen(t, exportScratch)
	var eState, eMode sql.NullString
	if err := exportDB.QueryRow(`SELECT state, mode FROM images WHERE set_id = ? AND image_id = 1`, setID).
		Scan(&eState, &eMode); err != nil {
		t.Fatalf("reading the images row of the on-media export: %v", err)
	}
	if eState.String != "planned" || eMode.Valid {
		t.Errorf("on-media export images row = (state %q, mode %v), want ('planned', NULL): the export is the RUN-START snapshot (6.6: one VACUUM INTO per run, produced before any image is written), so first-run media must still carry the plan-time row",
			eState.String, eMode)
	}

	// --- half 3: the consumer - media-alone restore over that snapshot
	if err := os.Remove(cat); err != nil {
		t.Fatalf("deleting the ORIGINAL catalog (the media-alone precondition): %v", err)
	}
	target := restoreTarget(t)
	stdout, stderr, code := runInProcess(t, "restore",
		"--from-media", dest, "--target", target, "--work-dir", tempWorkDir(t))
	if code != report.ExitOK {
		t.Fatalf("restore --from-media over the plan-time-state export = %d, want 0 (images.state must be inert to the media-alone flow)\nstdout: %s\nstderr: %s",
			code, stdout, stderr)
	}
	if strings.Contains(stderr, "warning:") {
		t.Errorf("first-run media must restore WITHOUT an anchor warning:\n%s", stderr)
	}
	assertSameRestoredTree(t, root, target)
}
