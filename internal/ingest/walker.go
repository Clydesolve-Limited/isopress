package ingest

// T1.3: the scan-time directory walker of DESIGN_SPEC 3.1 (directory
// walk), 3.4 (unreadable directories), and the FR-005 scan-root safety
// rail of 3.2. It turns a source tree into three streams the rest of
// the ingest pipeline consumes:
//
//  1. hash JOBS for regular files, delivered synchronously through the
//     emit callback (T1.4 wraps this as a channel send into the hash
//     worker pool; a non-nil error from emit aborts the walk);
//  2. symlink ENTRIES with their raw targets (D-03: never followed, no
//     job) and the EMPTY-DIR list with each dir's own metadata
//     (FR-037; T2.2 durable-table ruling), both returned in WalkResult
//     for T1.6's reconciler to record;
//  3. WARNINGS for special files and unreadable directories, both in
//     WalkResult.Warnings (for run_events and the end-of-run report,
//     T1.5/T1.6) and, when a progress.Bus is supplied, emitted live as
//     progress.Warn events (DESIGN_SPEC 9.1, never throttled per 9.3).
//
// Walk decisions per entry (DESIGN_SPEC 3.1), all classification via
// fsmeta.ClassifyDirEntry (DirEntry.Type() first, Info() fallback only
// when the type is unknown):
//
//	directory  -> MatchDir?  -> fs.SkipDir (pruned subtree, cost: ZERO
//	                           directory reads on Go 1.26 because
//	                           WalkDir calls back before ReadDir)
//	             else        -> counted; recorded for the empty-dir list
//	regular    -> MatchFile? -> dropped (excluded); else lstat capture
//	                           (fsmeta.PathMeta) and one Job
//	symlink    -> MatchFile? -> dropped (excluded); else target via
//	                           fsmeta.ReadTarget + Meta, no job, never
//	                           followed (D-03)
//	special    -> skipped with a progress.Warn + Warnings entry (FIFO,
//	                           socket, device; PLAN_SPEC section 2.1
//	                           scopes the tool to files+dirs only)
//
// Unreadable directories (DESIGN_SPEC 3.4): a ReadDir failure produces
// a warning and the subtree is skipped; the walk CONTINUES over the
// siblings. The scan root itself is exempt from that tolerance - a
// scan that silently walks nothing is worse than a failed run - and its
// failures are classified by SHAPE versus OPERATIONAL:
//
//   - SHAPE errors - the root is missing (lstat fails), or exists but
//     is not a directory (regular file, symlink, or other) - are a
//     report.UsageError (exit 2 via report.Classify): the user gave a
//     wrong path.
//   - OPERATIONAL errors - EACCES/EIO on the ReadDir of a shape-valid
//     directory root - are a plain wrapped error (exit 1): the
//     environment denied access, not the user's input.
//
// Scan-root safety rail (FR-005, DESIGN_SPEC 3.2): scanning the
// filesystem root "/" with an EMPTY effective pattern set refuses with
// a report.UsageError (the cli maps it to exit 2 through
// report.Classify). The check runs BEFORE any filesystem access. "Empty
// effective pattern set" is the bar, not "no flags": an --exclude-from
// file containing only comments/blank lines still refuses. With at
// least one effective pattern, "/" walks like any other root.
//
// Path convention: Job.RelPath, SymlinkEntry.RelPath, and WalkResult
// entries are slash-separated paths RELATIVE TO THE SCAN ROOT with no
// leading or trailing slash; Job.AbsPath is the absolute filesystem
// path (Walk resolves root to an absolute, cleaned path first).

import (
	"fmt"
	"io/fs"
	"path/filepath"

	"isopress/internal/fsmeta"
	"isopress/internal/progress"
	"isopress/internal/report"
)

// Job is one regular file ready to be hashed (T1.4 consumes these off
// its channel; DESIGN_SPEC 3.1 "regular files become hash jobs"). Meta
// is captured with fsmeta.PathMeta (an lstat) at walk time per 3.1, so
// the hash pool's post-hash re-lstat for mid-scan change detection
// (3.5 point 1, D-10) compares against the WALK-time baseline.
type Job struct {
	// RelPath is the slash-separated path relative to the scan root.
	RelPath string
	// AbsPath is the absolute filesystem path of the file.
	AbsPath string
	// Meta is the lstat metadata captured at walk time (DESIGN_SPEC
	// 3.1); Dev/Inode/Nlink feed hard-link-family detection (2.4).
	Meta fsmeta.Meta
}

// SymlinkEntry is a captured symlink: the raw target (never resolved or
// followed, D-03 confirmed 2026-10-18) plus the lstat metadata of the
// link itself. Symlinks produce NO hash job; the
// reconciler stores them as entry_kind='symlink' rows with
// link_target set (DESIGN_SPEC 2.3).
type SymlinkEntry struct {
	// RelPath is the slash-separated path relative to the scan root.
	RelPath string
	// AbsPath is the absolute filesystem path of the link.
	AbsPath string
	// Target is the raw os.Readlink result; for a dangling symlink it
	// is the recorded target string, which still restores verbatim.
	Target string
	// Meta is the lstat metadata of the link itself (Size is
	// len(Target) per fsmeta.Capture).
	Meta fsmeta.Meta
}

// EmptyDirEntry is a captured empty directory: its relpath plus the
// lstat metadata of the directory itself. T2.2 (2026-10-19 architect
// ruling, DESIGN_SPEC 2.3/3.7): empty dirs are recorded in the durable
// `directories` table, whose mode/uid/gid/mtime_ns columns are NOT
// NULL, so the walker carries the directory's own metadata alongside
// its relpath. The metadata is captured at the END of the walk (see
// Walk), only for directories that actually ended up empty, so a
// non-empty directory costs no extra lstat.
type EmptyDirEntry struct {
	// RelPath is the slash-separated path relative to the scan root.
	RelPath string
	// Meta is the lstat metadata of the directory itself (mode, uid,
	// gid, mtime_ns land in the directories table; FR-037 restore
	// recreates the dir with them).
	Meta fsmeta.Meta
}

// WalkResult is the complete walk report (DESIGN_SPEC 3.1; sufficient
// for T1.5's failure reporting and T1.6's runs.stats_json and run_events
// bookkeeping). Counts do NOT include the scan root itself: Dirs counts
// subdirectories visited.
type WalkResult struct {
	// Dirs is the number of directories visited (the scan root and
	// pruned directories are not counted).
	Dirs int
	// Files is the number of regular-file Jobs emitted. It doubles as
	// the walk's honest TOTAL file count for the percent-after-walk
	// amendment (DESIGN_SPEC 3.6/9.1, 2026-10-19): every counted file
	// emits exactly one hash job, so Files already IS the hash phase's
	// TotalFiles and a second "total files" field would only duplicate
	// it (field decision documented in the amendment report).
	Files int
	// TotalBytes is the sum of the lstat-captured Meta.Size of every
	// emitted Job - the byte total that walk completion makes known
	// for the still-running hash phase (DESIGN_SPEC 3.6/9.1,
	// percent-after-walk, 2026-10-19; surfaced via the totals Phase
	// event in internal/cli/scan.go). A pure counter over metadata
	// already in hand at ZERO extra syscalls; symlinks (no job) and
	// excluded files are not counted, matching the hash phase's
	// ItemDone byte accounting exactly.
	TotalBytes int64
	// Symlinks holds every captured symlink; len(Symlinks) is the
	// symlink count.
	Symlinks []SymlinkEntry
	// SpecialsSkipped counts FIFO/socket/device entries skipped with a
	// warning (no job, no catalog record; DESIGN_SPEC 3.1).
	SpecialsSkipped int
	// ExcludedDirsPruned counts directories removed by the matcher via
	// fs.SkipDir (DESIGN_SPEC 3.1 prune contract).
	ExcludedDirsPruned int
	// ExcludedFiles counts files and symlinks dropped by the matcher
	// (no job, no record). Reported for run statistics; excluded paths
	// never enter the catalog (DESIGN_SPEC 3.2).
	ExcludedFiles int
	// EmptyDirs lists, in lexical walk order, the directories that
	// contained no entries at all on disk - including no excluded or
	// pruned ones - for FR-037 empty-directory preservation, each with
	// the directory's own lstat metadata (T2.2: the durable
	// `directories` table stores mode/uid/gid/mtime_ns, DESIGN_SPEC
	// 2.3). A pruned directory contributes nothing here, even if it is
	// empty on disk, because the tree below an exclusion is out of
	// scope.
	EmptyDirs []EmptyDirEntry
	// Warnings holds every warning the walk produced (special files,
	// unreadable directories), in order. Each is also emitted on the
	// bus as a progress.Warn when a Bus is supplied.
	Warnings []string
}

// Walk traverses root, applies the exclusion matcher at walk time, and
// delivers one Job per included regular file to emit. It is synchronous
// and single-goroutine by design: T1.4 owns concurrency (emit becomes a
// channel send into the hash pool; a pool-side failure or cancellation
// surfaces as a non-nil emit result, which aborts the walk and is
// returned verbatim so callers can errors.Is it).
//
// Arguments:
//
//	root  scan root; resolved to an absolute cleaned path. Must be a
//	      directory that is not a symlink (the tool never follows
//	      symlinks, D-03, not even for the root).
//	m     the exclusion matcher; the zero value matches nothing.
//	emit  per-file job callback; nil discards jobs (count-only walks).
//	      emit must not return fs.SkipDir or fs.SkipAll;
//	      filepath.WalkDir would swallow them and end the walk with a
//	      success result.
//	bus   optional progress bus; warnings are emitted as
//	      progress.Warn events when non-nil (DESIGN_SPEC 9.1).
//
// Errors: the scan-root safety rail and root-SHAPE violations (root
// missing, regular file, symlink, or otherwise not a directory - the
// user gave a wrong path) return a report.UsageError (exit 2 via
// report.Classify). A root-ReadDir failure on a shape-valid directory
// root (OPERATIONAL: EACCES/EIO - the environment denied access, not
// the user's input) is returned as a plain wrapped error (exit 1), NOT
// a usage error. An emit failure is returned as-is; everything else
// that can go wrong at entry level (vanishing file, unreadable symlink
// target, unknown type) degrades to a warning and the walk continues
// (DESIGN_SPEC 3.4 tolerance).
func Walk(root string, m Matcher, emit func(Job) error, bus *progress.Bus) (WalkResult, error) {
	var res WalkResult

	abs, err := filepath.Abs(root)
	if err != nil {
		return res, fmt.Errorf("ingest: resolve scan root %q: %w", root, err)
	}
	root = filepath.Clean(abs)

	// Scan-root safety rail (FR-005, DESIGN_SPEC 3.2): refuse to walk
	// "/" with no exclusions, BEFORE touching the filesystem.
	if isFilesystemRoot(root) && len(m.patterns) == 0 {
		return WalkResult{}, &report.UsageError{
			Msg: fmt.Sprintf("refusing to scan the filesystem root %q without any exclusion patterns; "+
				"pass --exclude PATTERN (repeatable) or --exclude-from FILE "+
				"(for broad root scans consider: proc/ sys/ dev/ run/ tmp/ mnt/ media/)", root),
		}
	}

	warn := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		res.Warnings = append(res.Warnings, msg)
		if bus != nil {
			bus.Emit(progress.Warn{Msg: msg})
		}
	}

	// Empty-dir detection without extra syscalls (FR-037): record every
	// visited, non-pruned directory; mark every directory that is
	// observed to hold at least one entry (any entry marks its parent,
	// including excluded ones - a directory whose children were all
	// EXCLUDED is still non-empty ON DISK, which is what the restore
	// skeleton must reproduce). dirsSeen keeps lexical walk order.
	dirsSeen := make([]string, 0)
	hasEntries := make(map[string]bool)

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		// WalkDir contract (io/fs walk.go, Go 1.26):
		//  - root stat/lstat failure: d == nil, err != nil;
		//  - a directory's ReadDir failure: the callback is invoked a
		//    SECOND time for the same path with err != nil.
		if d == nil {
			// SHAPE error: the root failed its lstat (most
			// commonly a missing path) - a wrong path is the
			// user's mistake, so this is a usage-class error
			// (exit 2), per the classification in the file doc.
			return &report.UsageError{
				Msg: fmt.Sprintf("cannot open scan root %q", path),
				Err: err,
			}
		}

		if path == root {
			if !d.IsDir() {
				// SHAPE error: the root exists but is not a
				// directory. Covers a regular-file root and a
				// symlink root (Lstat sees the link itself; it
				// is never followed, D-03). A wrong path is
				// usage-class (exit 2), per the classification
				// in the file doc.
				return &report.UsageError{
					Msg: fmt.Sprintf("scan root %q is not a directory (%s); "+
						"point the scan at a real directory (symlinks are never followed)", root, d.Type()),
				}
			}
			if err != nil {
				// OPERATIONAL error (second call, root ReadDir
				// failure): the root's SHAPE is valid (it
				// lstat'ed as a directory) but the environment
				// denied the read (EACCES/EIO), so this is NOT
				// a usage error - a plain wrapped error (exit
				// 1), per the classification in the file doc.
				// Still hard, not a 3.4 warning: the whole scan
				// would be vacuous.
				return fmt.Errorf("cannot read scan root %q: %w", root, err)
			}
			// The root itself is never matched or counted.
			return nil
		}

		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return fmt.Errorf("ingest: relative path for %q: %w", path, rerr)
		}
		rel = filepath.ToSlash(rel)

		if err != nil {
			// Second call for a subdirectory whose ReadDir failed
			// (DESIGN_SPEC 3.4): warning, subtree skipped, walk
			// continues over the siblings.
			hasEntries[filepath.Dir(path)] = true // parent is non-empty
			hasEntries[path] = true               // unknown contents: never claim "empty"
			warn("cannot read directory %s: %v; subtree skipped", rel, err)
			return nil
		}

		// Any visited entry makes its parent directory non-empty (see
		// the empty-dir bookkeeping above).
		hasEntries[filepath.Dir(path)] = true

		kind, cerr := fsmeta.ClassifyDirEntry(d)
		if cerr != nil {
			warn("skipping %s: %v", rel, cerr)
			return nil
		}

		switch kind {
		case fsmeta.KindDirectory:
			if m.MatchDir(rel) {
				// Prune BEFORE the directory is read: returning
				// fs.SkipDir on the pre-ReadDir call skips the subtree
				// at the cost of zero reads (DESIGN_SPEC 3.1's "one
				// directory read" upper bound holds).
				res.ExcludedDirsPruned++
				return fs.SkipDir
			}
			dirsSeen = append(dirsSeen, path)
			res.Dirs++

		case fsmeta.KindRegular:
			if m.MatchFile(rel) {
				res.ExcludedFiles++
				return nil
			}
			meta, merr := fsmeta.PathMeta(path)
			if merr != nil {
				// Vanished or became unstatable between the directory
				// read and the lstat (DESIGN_SPEC 3.4 tolerance).
				warn("skipping %s: %v", rel, merr)
				return nil
			}
			res.Files++
			// Percent-after-walk (DESIGN_SPEC 3.6/9.1): sum the
			// walk-time lstat size into the byte total the hash
			// phase's totals event will surface. The Meta is
			// already captured - no extra syscall.
			res.TotalBytes += meta.Size
			if emit != nil {
				if emitErr := emit(Job{RelPath: rel, AbsPath: path, Meta: meta}); emitErr != nil {
					return emitErr
				}
			}

		case fsmeta.KindSymlink:
			if m.MatchFile(rel) {
				res.ExcludedFiles++
				return nil
			}
			target, terr := fsmeta.ReadTarget(path)
			if terr != nil {
				warn("skipping symlink %s: %v", rel, terr)
				return nil
			}
			meta, merr := fsmeta.PathMeta(path)
			if merr != nil {
				warn("skipping symlink %s: %v", rel, merr)
				return nil
			}
			res.Symlinks = append(res.Symlinks, SymlinkEntry{
				RelPath: rel,
				AbsPath: path,
				Target:  target,
				Meta:    meta,
			})

		case fsmeta.KindSpecial:
			// FIFOs, sockets, device nodes: skipped with a warning, no
			// job, no catalog record; the walk continues (3.1).
			res.SpecialsSkipped++
			warn("skipping special file %s (%s): not a regular file or directory", rel, d.Type())
		}
		return nil
	})
	if err != nil {
		return res, err
	}

	for _, dir := range dirsSeen {
		if !hasEntries[dir] {
			rel, rerr := filepath.Rel(root, dir)
			if rerr != nil {
				return res, fmt.Errorf("ingest: relative path for %q: %w", dir, rerr)
			}
			// T2.2: the durable directories table needs the
			// directory's own metadata (mode/uid/gid/mtime_ns, 2.3),
			// so each empty dir is lstat'ed HERE - at the very end of
			// the walk, and only for dirs that ended up empty, so no
			// non-empty directory pays this syscall. A dir that
			// vanished or became unstatable in the meantime degrades
			// to a 3.4 warning and is skipped (recording invented
			// metadata would be worse than not recording the dir).
			meta, merr := fsmeta.PathMeta(dir)
			if merr != nil {
				warn("skipping empty dir %s: %v", filepath.ToSlash(rel), merr)
				continue
			}
			res.EmptyDirs = append(res.EmptyDirs, EmptyDirEntry{
				RelPath: filepath.ToSlash(rel),
				Meta:    meta,
			})
		}
	}
	return res, nil
}

// isFilesystemRoot reports whether the absolute cleaned path is the
// filesystem root. Both separator spellings are accepted defensively,
// but on the tool's only supported platform (Linux, NFR-001) the
// cleaned path is always "/".
func isFilesystemRoot(abs string) bool {
	return abs == string(filepath.Separator) || abs == "/"
}
