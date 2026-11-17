package ingest

// T1.6: the scan reconciler of DESIGN_SPEC 3.7 (rescan and
// reconciliation), 2.4 (modification versioning, runs justification),
// and 1.4 (ingest data flow). It turns the pipeline's outputs - the
// classified HashResults (T1.5) and the walker's symlink entries
// (T1.3) - into the five reconciliation outcomes, and runs the
// not-seen (missing) pass over the FULL scan scope. The catalog SQL
// itself lives in internal/catalog/scanstore.go (DESIGN_SPEC 1.2: the
// catalog is the only SQL issuer); this component drives it through
// the injected Store interface so it is unit-testable without a
// database.
//
// # The five outcomes (DESIGN_SPEC 3.7, one per stream record)
//
//	NEW         no catalog-active row for the relpath -> insert an
//	            unassigned row (FR-029: the planner will only ever
//	            place it on new destinations)
//	UNCHANGED   hash matches the latest active row -> touch
//	            last_seen_run ONLY (no content column is written)
//	MODIFIED    hash (or entry kind / symlink target) differs ->
//	            new row with modified_of set, old row superseded_by
//	            set, IN THE SAME TRANSACTION; the old row keeps its
//	            image_id and copied state (2.4, FR-030: the media
//	            holding the old content stays accurately described)
//	UNREADABLE  T1.5 class: open/read failure -> a row with
//	            status='unreadable', status_detail=errno text,
//	            hash NULL, NO versioning
//	CHANGED     T1.5 class (changed_during_scan): post-hash lstat
//	            delta -> a row with status='changed_during_scan',
//	            status_detail naming the fields, hash NULL,
//	            NO versioning
//
//	not seen    (the sixth, batch-independent pass) catalog-active
//	            rows whose relpath was not in this scan's stream ->
//	            status='missing' (FR-032). Records and media are
//	            untouched; deletions never create assignments and
//	            never free planner capacity (3.7).
//
// # Why failure-class rows carry NO versioning (documented decision)
//
// A T1.5 failure-class record is NOT new content: the scan could not
// read stable bytes, so there is nothing to version FROM or TO.
// Versioning would claim a content transition the tool never
// witnessed, and would supersede an active row whose content may
// still be intact on media (an unreadable SOURCE says nothing about
// the backed-up copy). The failure row is therefore inserted
// standalone (modified_of NULL, superseded_by NULL, hash NULL); the
// relpath is counted as SEEN, so an older active row for the same
// path is NOT marked missing either. A later stable scan reconciles
// the path normally (its active-row lookup ignores non-active rows,
// so a recovered file cleanly lands as UNCHANGED or MODIFIED against
// the last known-good content).
//
// # Stale-digest drop (mechanical enforcement, per the T1.5 review)
//
// HashResult.Digest carries a digest even for some failed reads
// (partial-stream hashing). The T1.5 seam rule is: failure-class rows
// store hash NULL. rowFromClassified enforces it AT THE INSERT SITE
// below - the digest is copied into the row only when
// Status == StatusOK - so no caller of this reconciler (or of the
// catalog layer underneath) can accidentally persist a stale digest
// with an 'unreadable' or 'changed_during_scan' status.
//
// # Empty directories: durable rows in the `directories` table
// (T2.2, 2026-10-19 architect ruling)
//
// Sprint 1 recorded here that empty dirs were NOT catalog rows and
// carried in WalkResult memory only. That gap is now CLOSED by design
// amendment: DESIGN_SPEC 2.3 added a dedicated `directories` table, and
// this reconciler upserts the walk's empty-dir set into it (3.7
// extended to dirs). The semantics, per the ruling:
//
//   - the empty set is WalkResult.EmptyDirs (on-disk emptiness
//     INCLUDING excluded children - the T1.3 walker semantics: any
//     entry, excluded or not, marks its parent non-empty);
//   - each empty dir is UPSERTed (dirs are not lineage-versioned: one
//     row per path per set, status re-activated to 'active', metadata
//     refreshed - contrast the files table's versioned rows);
//   - every EXISTING directories row of the set whose relpath is NOT
//     in the empty set is marked status='missing' - this covers BOTH a
//     dir deleted from the source AND a dir that gained files (the
//     2026-10-19 ruling: either way it needs no explicit
//     representation, so writers skip it);
//   - NO content or lineage logic applies to dirs (contrast files:
//     no hash, no modified_of/superseded_by, no versioning).
//
// The dir not-seen pass carries the same abort-safety gate as the file
// pass: ReconcileEmptyDirs refuses before ScanComplete (an aborted scan
// must never mark dirs missing off a partial walk).
// Symlinks, by contrast, DO get files rows (entry_kind='symlink',
// link_target set, hash NULL per 2.3 and D-03).
//
// # The not-seen safety mechanism (SAFETY, test-pinned)
//
// The missing pass would be catastrophic on an aborted scan: a walk
// that died at entry 10 of 1,000,000 would classify the other
// 999,990 active rows as missing. Two mechanical gates prevent it:
//
//  1. Reconciler-level: MarkMissing REFUSES to run until
//     ScanComplete() has been called (ErrScanIncomplete). The cli
//     calls ScanComplete only after a CLEAN pipeline: walk returned
//     no error, the hash pool drained (pool.Wait nil), and every
//     batch flushed (batcher nil). Any abort skips the pass entirely
//     and the run is finalized 'failed'.
//  2. Scope-level: the pass only considers catalog-active rows of the
//     transaction's set (Store.ListActive) whose relpath is absent
//     from the seen set accumulated from THIS scan's stream. The seen
//     set covers every stream record - regular results, failure-class
//     results, and symlinks - so a path the scan did touch is never
//     marked missing regardless of outcome.
//
// # Transaction and single-producer wiring (DESIGN_SPEC 3.3, T1.4
// contracts restated)
//
// ReconcileBatch opens ONE store transaction per batch (the 3.3
// writer batching: 2,000 records or 2 seconds), so each batch commits
// atomically - 3.7's "reconciles in one transaction" holds at batch
// granularity, which is the granularity the 3.3 design commits at.
// The cli wiring keeps the pipeline's single-producer contracts: the
// walk goroutine is the only Pool.Submit caller, and the single drain
// goroutine is the only Batcher.Add caller (it classifies via
// Failures.Process and feeds the batcher). ReconcileBatch itself is
// called from that drain goroutine only.
//
// # Uniqueness assumption (documented)
//
// filepath.WalkDir yields each relpath at most once per scan, so a
// batch never contains two records for the same path and a path never
// appears in two batches. The reconciler relies on exactly this -
// there is no in-flight dedup - and the walker contract (T1.3)
// guarantees it.

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"isopress/internal/fsmeta"
)

// The reconciliation outcomes. These are observability labels for the
// end-of-run report; the catalog stores the underlying rows and status
// values of DESIGN_SPEC 2.3, never these strings.
const (
	OutcomeNew        = "new"
	OutcomeUnchanged  = "unchanged"
	OutcomeModified   = "modified"
	OutcomeMissing    = "missing"
	OutcomeUnreadable = StatusUnreadable
	OutcomeChanged    = StatusChangedDuringScan
)

// ErrScanIncomplete is returned by MarkMissing when the not-seen pass
// is requested before ScanComplete: the SAFETY gate of 3.7 (an aborted
// scan must never mark rows missing). The cli never triggers it in
// production (it gates the call on a clean pipeline); the error exists
// so misuse fails loudly instead of silently corrupting state.
var ErrScanIncomplete = errors.New("ingest: not-seen pass refused: scan did not complete cleanly")

// FileRow is the reconciler's normalized content record - the shape it
// hands to the Store. It mirrors catalog.FileRow field for field; the
// cli wiring maps between the two (this package never imports the
// catalog, DESIGN_SPEC 1.2: services depend on interfaces).
type FileRow struct {
	RelPath      string
	EntryKind    string // 'regular' | 'symlink'
	Size         int64
	Mode         uint32
	UID          uint32
	GID          uint32
	MtimeNS      int64
	Dev          uint64
	Inode        uint64
	Nlink        uint64
	LinkTarget   string // "" -> NULL
	Hash         []byte // nil -> NULL (failure classes, symlinks)
	Status       string // 'active' | 'unreadable' | 'changed_during_scan'
	StatusDetail string
}

// ActiveRecord is one catalog-active row read back for comparison.
type ActiveRecord struct {
	FileID     int64
	RelPath    string
	EntryKind  string
	Hash       []byte
	LinkTarget string
	Status     string
}

// DirRow is the reconciler's empty-directory record - the shape it
// hands to the Store for the directories table (DESIGN_SPEC 2.3,
// T2.2). It mirrors catalog.DirRow field for field.
type DirRow struct {
	RelPath string
	Mode    uint32
	UID     uint32
	GID     uint32
	MtimeNS int64
}

// ActiveDir is one catalog-active directories row read back for the
// dir not-seen pass (T2.2). It mirrors catalog.ActiveDir.
type ActiveDir struct {
	DirID   int64
	RelPath string
}

// Store is the transaction-scoped catalog surface the reconciler
// needs. Every method runs inside ONE store transaction begun by the
// TxFactory; the wiring supplies a BEGIN IMMEDIATE catalog transaction
// (catalog.ScanTx), tests supply a fake. Method sets mirror
// catalog.ScanTx's exactly.
type Store interface {
	// ActiveByRelPaths returns the catalog-active row per given
	// relpath (absent key = no active row).
	ActiveByRelPaths(ctx context.Context, relpaths []string) (map[string]ActiveRecord, error)
	// InsertScanFile stores a new unassigned row, returning its id.
	InsertScanFile(ctx context.Context, rec FileRow) (int64, error)
	// UpdateSeen touches last_seen_run only (UNCHANGED outcome).
	UpdateSeen(ctx context.Context, fileID int64) error
	// InsertModification stores the versioned pair (MODIFIED
	// outcome): new row with modified_of, old row superseded_by, in
	// the same transaction. Returns the new row id.
	InsertModification(ctx context.Context, oldFileID int64, rec FileRow) (int64, error)
	// ListActive returns every catalog-active row of the set (the
	// not-seen pass read side).
	ListActive(ctx context.Context) ([]ActiveRecord, error)
	// MarkMissing stamps status='missing' on the given row ids.
	MarkMissing(ctx context.Context, fileIDs []int64) (int, error)
	// UpsertEmptyDir stores one currently-empty directory (T2.2): a
	// new path inserts; an existing path refreshes metadata and
	// last_seen_run and re-activates status, preserving first_seen_run
	// and any locked image_id.
	UpsertEmptyDir(ctx context.Context, rec DirRow) error
	// ListActiveDirs returns every catalog-active directories row of
	// the set (the dir not-seen pass read side).
	ListActiveDirs(ctx context.Context) ([]ActiveDir, error)
	// MarkDirsMissing stamps status='missing' on the given dir ids
	// (status write only - image_id stays, FR-032).
	MarkDirsMissing(ctx context.Context, dirIDs []int64) (int, error)
}

// Tx pairs a transaction-scoped Store with its commit and rollback.
// Exactly one of Commit/Rollback runs per factory call; the reconciler
// commits only after every statement of the batch succeeded.
type Tx struct {
	Store
	Commit   func() error
	Rollback func()
}

// TxFactory begins one reconciliation transaction. The wiring supplies
// catalog.DB.BeginScanTx adapted to this shape; tests supply fakes.
type TxFactory func(ctx context.Context) (*Tx, error)

// Counts is the reconciler's outcome tally - the numbers behind the
// end-of-run report and runs.stats_json (Scanned = Regular + Symlinks;
// Unreadable/Changed count failure-class ROWS written; Missing counts
// rows stamped by the not-seen pass; Dirs counts the currently-empty
// directories recorded by the T2.2 dir reconciliation - the active
// empty-dir set after the run; Bytes is the sum of successfully hashed
// bytes).
type Counts struct {
	Scanned    int   `json:"scanned"`
	Regular    int   `json:"regular"`
	Symlinks   int   `json:"symlinks"`
	Added      int   `json:"added"`
	Modified   int   `json:"modified"`
	Unchanged  int   `json:"unchanged"`
	Missing    int   `json:"missing"`
	Unreadable int   `json:"unreadable"`
	Changed    int   `json:"changed"`
	Dirs       int   `json:"dirs"`
	Bytes      int64 `json:"bytes"`
}

// Reconciler drives the 3.7 outcomes over a Store. Construct one per
// scan run (NewReconciler); it accumulates the seen set and the counts
// across batches. Not safe for concurrent use: the wiring calls it
// from the single drain goroutine (and, for symlinks, from the walk
// goroutine after the drain has finished - serialized, never
// concurrent).
type Reconciler struct {
	factory  TxFactory
	runID    int64
	seen     map[string]struct{}
	complete bool
	counts   Counts
}

// NewReconciler returns a reconciler writing provenance under runID.
// factory begins per-batch transactions; runID is carried for
// provenance context in errors (the Store itself stamps it).
func NewReconciler(factory TxFactory, runID int64) *Reconciler {
	return &Reconciler{
		factory: factory,
		runID:   runID,
		seen:    make(map[string]struct{}),
	}
}

// Counts returns a copy of the outcome tally.
func (r *Reconciler) Counts() Counts { return r.counts }

// ScanComplete marks the stream fully consumed: walk clean, pool
// drained, every batch flushed. It arms the not-seen pass; until it is
// called, MarkMissing refuses (ErrScanIncomplete, the safety gate).
func (r *Reconciler) ScanComplete() { r.complete = true }

// ReconcileBatch reconciles one batch of classified hash results in ONE
// transaction (DESIGN_SPEC 3.3 writer batching at 3.7 semantics). Any
// statement failure rolls the whole batch back and propagates: the
// pipeline stops, the run is failed, and the catalog is unchanged for
// this batch (atomic batch, 3.7).
func (r *Reconciler) ReconcileBatch(ctx context.Context, batch []ClassifiedResult) error {
	if len(batch) == 0 {
		return nil
	}
	tx, err := r.factory(ctx)
	if err != nil {
		return fmt.Errorf("ingest: beginning reconciliation batch: %w", err)
	}
	committed := false
	// Snapshot discipline: counts and the seen set are speculative
	// until the batch commits, so a rolled-back batch leaves no trace
	// of itself in either (single-goroutine access, so a shallow copy
	// plus per-batch seen-list is sufficient and allocation-light).
	savedCounts := r.counts
	var batchSeen []string
	defer func() {
		if !committed {
			tx.Rollback()
			r.counts = savedCounts
			for _, p := range batchSeen {
				delete(r.seen, p)
			}
		}
	}()

	paths := make([]string, len(batch))
	for i, c := range batch {
		paths[i] = c.RelPath
	}
	active, err := tx.ActiveByRelPaths(ctx, paths)
	if err != nil {
		return err
	}
	for _, c := range batch {
		// Append BEFORE reconciling: reconcileRecord marks the path
		// seen before writing, so the rollback path must cover the
		// failing record too.
		batchSeen = append(batchSeen, c.RelPath)
		if err := r.reconcileRecord(ctx, tx.Store, c, active); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// reconcileRecord applies the 3.7 decision table to one classified
// result and writes the outcome through the store. old may be nil when
// the set has no catalog-active row for the relpath.
func (r *Reconciler) reconcileRecord(ctx context.Context, store Store, c ClassifiedResult, active map[string]ActiveRecord) error {
	r.seen[c.RelPath] = struct{}{}
	r.counts.Scanned++
	r.counts.Regular++

	rec := rowFromClassified(c) // stale-digest drop enforced inside

	switch c.Status {
	case StatusUnreadable, StatusChangedDuringScan:
		// Failure classes: a status row, hash NULL, NO versioning
		// (documented decision in the file doc).
		if c.Status == StatusUnreadable {
			r.counts.Unreadable++
		} else {
			r.counts.Changed++
		}
		_, err := store.InsertScanFile(ctx, rec)
		return err
	}

	r.counts.Bytes += c.Bytes
	old, ok := active[c.RelPath]
	if !ok {
		r.counts.Added++
		_, err := store.InsertScanFile(ctx, rec)
		return err
	}
	if outcome := classifyAgainstActive(old, rec); outcome == OutcomeUnchanged {
		r.counts.Unchanged++
		return store.UpdateSeen(ctx, old.FileID)
	}
	r.counts.Modified++
	_, err := store.InsertModification(ctx, old.FileID, rec)
	return err
}

// ReconcileSymlinks reconciles the walker's captured symlinks (T1.3
// WalkResult.Symlinks) in ONE transaction, after the hash stream has
// drained. Symlinks are captured but never hashed (D-03); they get
// rows with entry_kind='symlink', link_target set, hash NULL (2.3).
// Their outcomes fold into the same Added/Unchanged/Modified counters
// as regular files (Scanned/Symlinks track them separately).
func (r *Reconciler) ReconcileSymlinks(ctx context.Context, links []SymlinkEntry) error {
	if len(links) == 0 {
		return nil
	}
	tx, err := r.factory(ctx)
	if err != nil {
		return fmt.Errorf("ingest: beginning symlink reconciliation: %w", err)
	}
	committed := false
	// Same snapshot discipline as ReconcileBatch: a rolled-back
	// symlink batch leaves no counts or seen-set trace.
	savedCounts := r.counts
	var batchSeen []string
	defer func() {
		if !committed {
			tx.Rollback()
			r.counts = savedCounts
			for _, p := range batchSeen {
				delete(r.seen, p)
			}
		}
	}()

	paths := make([]string, len(links))
	for i, l := range links {
		paths[i] = l.RelPath
	}
	active, err := tx.ActiveByRelPaths(ctx, paths)
	if err != nil {
		return err
	}
	for _, l := range links {
		rec := rowFromSymlink(l)
		batchSeen = append(batchSeen, l.RelPath)
		r.seen[l.RelPath] = struct{}{}
		r.counts.Scanned++
		r.counts.Symlinks++
		old, ok := active[l.RelPath]
		if !ok {
			r.counts.Added++
			if _, err := tx.InsertScanFile(ctx, rec); err != nil {
				return err
			}
			continue
		}
		switch classifyAgainstActive(old, rec) {
		case OutcomeUnchanged:
			r.counts.Unchanged++
			if err := tx.UpdateSeen(ctx, old.FileID); err != nil {
				return err
			}
		default:
			r.counts.Modified++
			if _, err := tx.InsertModification(ctx, old.FileID, rec); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// ReconcileEmptyDirs reconciles the walker's empty-dir set (T1.3
// WalkResult.EmptyDirs, carrying each dir's own lstat metadata) into
// the durable directories table, in ONE transaction (T2.2, 2026-10-19
// architect ruling; spec basis in the file doc).
//
// Semantics:
//
//   - every entry in the current empty set is UPSERTed (new path:
//     insert; existing path: metadata refresh + last_seen_run bump +
//     status re-activated 'active'; first_seen_run and any locked
//     image_id preserved - FR-011 holds for dirs);
//   - every EXISTING active directories row of the set whose relpath
//     is NOT in the empty set becomes status='missing' - covering BOTH
//     a dir deleted from the source and a dir that gained files (the
//     ruling's explicit wording).
//
// Unlike ReconcileBatch, an EMPTY entries slice is NOT a no-op: a
// rescan on which every previously-empty dir gained files (or was
// deleted) must still mark the whole active set missing. The
// ScanComplete gate applies to the whole operation (an aborted scan
// never runs the missing half - ErrScanIncomplete, the same safety
// gate as the file pass); counts.Dirs (the recorded active empty-dir
// set) and the returned missing count are committed only on success.
// No lineage or content logic applies to dirs (contrast the file
// outcomes above).
func (r *Reconciler) ReconcileEmptyDirs(ctx context.Context, entries []EmptyDirEntry) (int, error) {
	if !r.complete {
		return 0, ErrScanIncomplete
	}
	tx, err := r.factory(ctx)
	if err != nil {
		return 0, fmt.Errorf("ingest: beginning empty-dir reconciliation: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback()
		}
	}()

	empty := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		empty[e.RelPath] = struct{}{}
		if err := tx.UpsertEmptyDir(ctx, DirRow{
			RelPath: e.RelPath,
			Mode:    fsmeta.UnixMode(e.Meta.Mode),
			UID:     e.Meta.UID,
			GID:     e.Meta.GID,
			MtimeNS: e.Meta.MtimeNS,
		}); err != nil {
			return 0, err
		}
	}

	active, err := tx.ListActiveDirs(ctx)
	if err != nil {
		return 0, err
	}
	var missing []int64
	for _, d := range active {
		if _, ok := empty[d.RelPath]; !ok {
			missing = append(missing, d.DirID)
		}
	}
	n, err := tx.MarkDirsMissing(ctx, missing)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	committed = true
	r.counts.Dirs = len(entries)
	return n, nil
}

// MarkMissing runs the 3.7 not-seen pass: every catalog-active row of
// the set whose relpath is NOT in this scan's seen set becomes
// status='missing' (FR-032), in one transaction. It refuses to run
// before ScanComplete (ErrScanIncomplete, the safety gate documented
// in the file doc) and returns the number of rows stamped.
func (r *Reconciler) MarkMissing(ctx context.Context) (int, error) {
	if !r.complete {
		return 0, ErrScanIncomplete
	}
	tx, err := r.factory(ctx)
	if err != nil {
		return 0, fmt.Errorf("ingest: beginning not-seen pass: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			tx.Rollback()
		}
	}()

	active, err := tx.ListActive(ctx)
	if err != nil {
		return 0, err
	}
	var missing []int64
	for _, a := range active {
		if _, seen := r.seen[a.RelPath]; !seen {
			missing = append(missing, a.FileID)
		}
	}
	n, err := tx.MarkMissing(ctx, missing)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	committed = true
	r.counts.Missing = n
	return n, nil
}

// classifyAgainstActive decides UNCHANGED vs MODIFIED for a record
// that has a catalog-active predecessor. Comparison keys, in order:
// entry kind (a regular file that turns into a symlink is content
// modification, not deletion-plus-creation), then the kind's content
// identity - digest for regular rows, raw target for symlinks (2.4:
// "hash matches latest active row"; D-03 symlink preservation).
func classifyAgainstActive(old ActiveRecord, rec FileRow) string {
	if old.EntryKind != rec.EntryKind {
		return OutcomeModified
	}
	if rec.EntryKind == "symlink" {
		if old.LinkTarget == rec.LinkTarget {
			return OutcomeUnchanged
		}
		return OutcomeModified
	}
	if bytes.Equal(old.Hash, rec.Hash) {
		return OutcomeUnchanged
	}
	return OutcomeModified
}

// rowFromClassified converts a T1.5 ClassifiedResult into the stored
// FileRow, carrying the WALK-time baseline metadata (Job.Meta - the
// DESIGN_SPEC 2.3 columns) and enforcing the stale-digest drop: the
// digest is stored ONLY for StatusOK, so 'unreadable' and
// 'changed_during_scan' rows always carry hash NULL (T1.5 seam rule,
// mechanically enforced here).
func rowFromClassified(c ClassifiedResult) FileRow {
	return FileRow{
		RelPath:      c.RelPath,
		EntryKind:    "regular",
		Size:         c.Meta.Size,
		Mode:         fsmeta.UnixMode(c.Meta.Mode),
		UID:          c.Meta.UID,
		GID:          c.Meta.GID,
		MtimeNS:      c.Meta.MtimeNS,
		Dev:          c.Meta.Dev,
		Inode:        c.Meta.Inode,
		Nlink:        c.Meta.Nlink,
		Hash:         digestOrNull(c),
		Status:       c.CatalogStatus(),
		StatusDetail: c.Detail,
	}
}

// rowFromSymlink converts a captured symlink (T1.3) into the stored
// FileRow: entry_kind 'symlink', raw target (D-03, never followed),
// hash ALWAYS nil (2.3: "NULL for symlinks").
func rowFromSymlink(l SymlinkEntry) FileRow {
	return FileRow{
		RelPath:    l.RelPath,
		EntryKind:  "symlink",
		Size:       l.Meta.Size, // len(target) per fsmeta.Capture
		Mode:       fsmeta.UnixMode(l.Meta.Mode),
		UID:        l.Meta.UID,
		GID:        l.Meta.GID,
		MtimeNS:    l.Meta.MtimeNS,
		Dev:        l.Meta.Dev,
		Inode:      l.Meta.Inode,
		Nlink:      l.Meta.Nlink,
		LinkTarget: l.Target,
		Hash:       nil,
		Status:     "active",
	}
}

// digestOrNull is the mechanical stale-digest drop: the raw digest is
// returned only for a clean classification; every other class yields
// nil, which the catalog stores as NULL.
func digestOrNull(c ClassifiedResult) []byte {
	if c.Status != StatusOK {
		return nil
	}
	h := make([]byte, len(c.Digest))
	copy(h, c.Digest[:])
	return h
}
