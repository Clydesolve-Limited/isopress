package catalog

// SchemaVersion is the current catalog schema version (DESIGN_SPEC 2.2).
//
// PRAGMA user_version holds this value in every catalog file; version 1 is
// the DDL below. Migrations are forward-only: opening a catalog stamped with
// a higher version is a hard error, never a downgrade.
const SchemaVersion = 1

// schemaV1DDL is the DESIGN_SPEC 2.3 DDL, copied verbatim. It is
// design-authoritative: it was executed against SQLite during the design
// phase and must not be paraphrased or "improved". Migration step 1 (see
// migrate.go) applies it inside one transaction; the caller stamps
// PRAGMA user_version afterwards.
//
// Notes carried from the design: chunks.set_id is denormalized from the
// parent file for uniform set-scoped queries and is populated
// transactionally with the parent file row; chunks.hash stays NULL until
// the chunk's first write stores it (lazy first-write hashing).
const schemaV1DDL = `-- One row per backup set (a planned collection of images sharing one set id).
CREATE TABLE sets (
  set_id        TEXT PRIMARY KEY,          -- deterministic, section 6.4
  source_root   TEXT NOT NULL,             -- absolute path scanned
  created_at    INTEGER NOT NULL,          -- unix nanoseconds, UTC
  tool_version  TEXT NOT NULL
);

-- One row per command run that produces ingest or verification state.
-- Justification: provenance for every file record (which scan saw it),
-- resumable end-of-run reporting, audit trail for cron usage (D-11),
-- and the correlation point for mid-scan change events (D-10).
CREATE TABLE runs (
  run_id        INTEGER PRIMARY KEY,
  set_id        TEXT NOT NULL REFERENCES sets(set_id),
  kind          TEXT NOT NULL CHECK (kind IN
                ('ingest','rescan','plan','copy','build',
                 'verify_media','validate','restore','clear')),
  root          TEXT NOT NULL,            -- source root or destination root
  started_at    INTEGER NOT NULL,
  finished_at   INTEGER,
  status        TEXT NOT NULL CHECK (status IN
                ('running','ok','partial','failed','interrupted')),
  exclude_json  TEXT NOT NULL DEFAULT '[]', -- snapshot of filter config (D-08)
  stats_json    TEXT NOT NULL DEFAULT '{}', -- counts, bytes, failures
  tool_version  TEXT NOT NULL
);

-- One row per (path, content version). Modifications add rows; they never
-- update content rows (PLAN_SPEC section 3, FR-030).
CREATE TABLE files (
  file_id         INTEGER PRIMARY KEY,
  set_id          TEXT NOT NULL REFERENCES sets(set_id),
  relpath         TEXT NOT NULL,          -- slash-separated, relative to root
  entry_kind      TEXT NOT NULL CHECK (entry_kind IN ('regular','symlink')),
  size            INTEGER NOT NULL,       -- bytes; for symlinks, len(target)
  mode            INTEGER NOT NULL,       -- permission bits
  uid             INTEGER NOT NULL,
  gid             INTEGER NOT NULL,
  mtime_ns        INTEGER NOT NULL,
  dev             INTEGER,                -- hard-link detection and change
  inode           INTEGER,                -- detection (D-10)
  nlink           INTEGER NOT NULL DEFAULT 1,
  link_group      INTEGER,                -- shared key across a hard-link
                                          -- family (same dev+inode+hash),
                                          -- NULL when nlink = 1
  link_target     TEXT,                   -- symlink targets (D-03)
  hash            BLOB,                   -- sha256, 32 bytes (D-09);
                                          -- NULL for symlinks and
                                          -- unreadable files
  first_seen_run  INTEGER NOT NULL REFERENCES runs(run_id),
  last_seen_run   INTEGER NOT NULL REFERENCES runs(run_id),
  first_seen_at   INTEGER NOT NULL,
  status          TEXT NOT NULL DEFAULT 'active' CHECK (status IN
                  ('active','missing','unreadable','changed_during_scan')),
  status_detail   TEXT,                   -- errno string or change detail
  modified_of     INTEGER REFERENCES files(file_id),  -- lineage (FR-030)
  superseded_by   INTEGER REFERENCES files(file_id),  -- lineage (FR-030)
  chunked         INTEGER NOT NULL DEFAULT 0,         -- FR-041 marker
  chunk_count     INTEGER NOT NULL DEFAULT 0,
  chunk_size      INTEGER,                -- effective chunk size used
  image_id        INTEGER,                -- assignment; NULL = unassigned.
                                          -- Locked once set (FR-011).
  copied          INTEGER NOT NULL DEFAULT 0  -- meaningful for non-chunked
                                              -- regular files; for chunked
                                              -- files this column mirrors
                                              -- AND(chunk.copied) and is
                                              -- maintained transactionally
);

CREATE INDEX idx_files_unassigned ON files(set_id, image_id)
  WHERE image_id IS NULL;
CREATE INDEX idx_files_path       ON files(set_id, relpath);
CREATE INDEX idx_files_linkgroup  ON files(link_group) WHERE link_group IS NOT NULL;
CREATE INDEX idx_files_image      ON files(set_id, image_id)
  WHERE image_id IS NOT NULL;

-- FR-037: empty directories need durable catalog representation. Unlike a
-- non-empty directory (which is implied by the payload paths under files/ and
-- chunks/), a directory with no files beneath it would never be created by any
-- file path, so it must be recorded explicitly. One row per currently-empty
-- directory; the planner assigns each to a destination (section 4.3) and the
-- writers/restore recreate it with metadata (FR-036/037). This table is what
-- makes the empty-dir assignment durable across processes: a later
-- plan/copy/build run has no in-memory WalkResult and reads only the catalog.
-- Clarified 2026-10-19 (S1 gap): empty-dir state moved from scan-run memory
-- into a dedicated table so it survives to the planner and writers.
CREATE TABLE directories (
  dir_id         INTEGER PRIMARY KEY,
  set_id         TEXT NOT NULL REFERENCES sets(set_id),
  relpath        TEXT NOT NULL,          -- slash-separated, relative to root
  mode           INTEGER NOT NULL,
  uid            INTEGER NOT NULL,
  gid            INTEGER NOT NULL,
  mtime_ns       INTEGER NOT NULL,
  first_seen_run INTEGER NOT NULL REFERENCES runs(run_id),
  last_seen_run  INTEGER NOT NULL REFERENCES runs(run_id),
  status         TEXT NOT NULL DEFAULT 'active' CHECK (status IN
                 ('active','missing')),
                 -- 'missing' covers both a dir deleted from source and a dir
                 -- that is no longer empty (a file was added beneath it);
                 -- either way it needs no explicit representation, so writers
                 -- skip it (FR-032 retention philosophy: mark, do not delete).
  image_id       INTEGER,                -- assignment; NULL = unassigned.
                                         -- Locked once set (FR-011); the
                                         -- planner sets it to the
                                         -- lowest-numbered destination
                                         -- (section 4.3). No copied flag:
                                         -- mkdir is idempotent (EEXIST means
                                         -- already present), so dir
                                         -- resumability is a non-issue.
  UNIQUE (set_id, relpath)               -- one row per dir path per set; dirs
                                         -- are not lineage-versioned, so a
                                         -- rescan upserts rather than
                                         -- adding rows (contrast: files)
);
CREATE INDEX idx_directories_image     ON directories(set_id, image_id);
CREATE INDEX idx_directories_unassigned ON directories(set_id)
  WHERE image_id IS NULL;

-- Placement and copied state for chunked files live here, not on the
-- file row (NOTES.md, post D-01 nuance 1).
CREATE TABLE chunks (
  chunk_id     INTEGER PRIMARY KEY,
  set_id       TEXT NOT NULL REFERENCES sets(set_id),  -- denormalized from
                                                       -- the parent file
  file_id      INTEGER NOT NULL REFERENCES files(file_id),
  chunk_index  INTEGER NOT NULL,          -- 0-based, ascending file order
  offset       INTEGER NOT NULL,          -- byte offset in the parent file
  length       INTEGER NOT NULL,
  hash         BLOB,                      -- sha256 of the chunk; NULL until
                                           -- first write stores it
                                           -- (section 4.3 lazy hashing)
  image_id     INTEGER,                   -- locked like file assignments
  copied       INTEGER NOT NULL DEFAULT 0,
  verified_at  INTEGER,
  UNIQUE (file_id, chunk_index)
);
CREATE INDEX idx_chunks_image ON chunks(set_id, image_id);

-- Destinations: one row per unit of media (image number), created by plan.
CREATE TABLE images (
  set_id        TEXT NOT NULL REFERENCES sets(set_id),
  image_id      INTEGER NOT NULL,         -- 1-based, sequential (section 6.4)
  nominal_size  INTEGER NOT NULL,         -- bytes as stated by the user
  usable_size   INTEGER NOT NULL,         -- bytes after profile margin (D-06)
  media_profile TEXT NOT NULL,            -- 'bd-r-sl' | 'bd-r-dl' | 'disk'
  state         TEXT NOT NULL DEFAULT 'planned' CHECK (state IN
                ('planned','building','built','verified','failed')),
  mode          TEXT CHECK (mode IN ('copy','iso')),  -- set at first write
  final_path    TEXT,                     -- copy dir or .iso path once built
  built_at      INTEGER,
  verified_at   INTEGER,
  bytes_written INTEGER,
  PRIMARY KEY (set_id, image_id)
);

-- FR-020: the finalized content list, materialized (not merely derived),
-- mirrored into the on-media manifest (FR-033) at finalize time.
CREATE TABLE image_contents (
  set_id       TEXT NOT NULL,
  image_id     INTEGER NOT NULL,
  media_path   TEXT NOT NULL,             -- path inside the logical tree
  kind         TEXT NOT NULL CHECK (kind IN
               ('file','chunk','symlink','dir','manifest','export')),
  file_id      INTEGER REFERENCES files(file_id),
  chunk_index  INTEGER,
  size         INTEGER,
  hash         BLOB,                      -- sha256; NULL for dirs/symlinks
  PRIMARY KEY (set_id, image_id, media_path)
);

-- Lightweight operational log for restore runs and failures; feeds the
-- end-of-run report and cron observability.
CREATE TABLE run_events (
  event_id   INTEGER PRIMARY KEY,
  run_id     INTEGER NOT NULL REFERENCES runs(run_id),
  at         INTEGER NOT NULL,
  level      TEXT NOT NULL CHECK (level IN ('info','warn','error')),
  file_id    INTEGER,
  relpath    TEXT,
  detail     TEXT NOT NULL
);
CREATE INDEX idx_run_events_run ON run_events(run_id);

CREATE VIEW v_active_files AS
SELECT * FROM files
WHERE status = 'active' AND superseded_by IS NULL;
`

// foldedV1RepairDDL is the T2.2 upgrade-hazard repair: the `directories`
// table and its two indexes, in CREATE ... IF NOT EXISTS form, for
// catalogs stamped user_version = 1 by a PRE-T2.2 binary.
//
// Why this exists (discovered after the 2026-10-19 fold-into-v1
// ratification): the directories table was added to the v1 DDL with NO
// migration step (PM-ratified: the whole v1 executes as one script;
// pre-release, so no released binary predates the table). But a
// catalog CREATED by an earlier development binary is already stamped
// user_version = 1, so the forward-migration path (version <
// SchemaVersion) never runs and the table is simply absent -
// scan would then fail with "no such table: directories" at the first
// empty-dir upsert. ensureFoldedV1Objects (migrate.go) applies this
// repair on every mutating Open, closing that gap without a schema
// version bump and without weakening the forward-only migration model.
//
// DRIFT GUARD: this text must stay schema-identical to the verbatim
// directories block inside schemaV1DDL above (the only difference is
// the IF NOT EXISTS keyword, which is a no-op on a fresh creation).
// TestFoldedRepairMatchesV1DDL pins the two against each other by
// comparing PRAGMA table_info and the index set of a fresh v1 catalog
// against a repaired one - change one, the test demands the other.
//
// Read-only opens deliberately do NOT repair (mode=ro cannot write):
// a read-only command against a pre-T2.2 catalog fails loudly on the
// missing table rather than mutating the file behind the user's back;
// running any mutating command (scan) heals the catalog first.
const foldedV1RepairDDL = `-- FR-037: empty directories need durable catalog representation (see
-- the verbatim block in schemaV1DDL for the full justification).
CREATE TABLE IF NOT EXISTS directories (
  dir_id         INTEGER PRIMARY KEY,
  set_id         TEXT NOT NULL REFERENCES sets(set_id),
  relpath        TEXT NOT NULL,
  mode           INTEGER NOT NULL,
  uid            INTEGER NOT NULL,
  gid            INTEGER NOT NULL,
  mtime_ns       INTEGER NOT NULL,
  first_seen_run INTEGER NOT NULL REFERENCES runs(run_id),
  last_seen_run  INTEGER NOT NULL REFERENCES runs(run_id),
  status         TEXT NOT NULL DEFAULT 'active' CHECK (status IN
                 ('active','missing')),
  image_id       INTEGER,
  UNIQUE (set_id, relpath)
);
CREATE INDEX IF NOT EXISTS idx_directories_image     ON directories(set_id, image_id);
CREATE INDEX IF NOT EXISTS idx_directories_unassigned ON directories(set_id)
  WHERE image_id IS NULL;
`
