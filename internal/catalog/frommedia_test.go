package catalog

// T5.5: the media-alone restore's (DESIGN_SPEC 8.5) export-copy read
// surface - internal/catalog frommedia.go. Every reader here is a
// read-only surface, so all tests run against OpenReadOnly handles (the
// 2.1 Option B shape the restore command uses).
//
//   - SetIDs is the SET-IDENTITY single-row guarantee: an FR-034 export is
//     set-scoped, so it answers EXACTLY one set id; a whole-catalog file
//     answers many (the caller's ==1 gate is what refuses the latter, exit
//     4 in the cli); a database with no schema at all fails LOUDLY rather
//     than answering nothing - as a KindCorrupt CatalogError (the media
//     path's export-failure class, exit 4), with the driver's reason still
//     readable inside it.
//   - MediaReadErr is that classification: every media-path read of the
//     export copy the PRAGMA gate passed but the schema cannot answer is
//     KindCorrupt (exit 4), except a context cancellation, which stays
//     itself.
//   - IntegrityCheck is 8.5's cheap first gate over the copy ("the
//     export's internal hashes are the deep anchor" - the export cannot
//     carry its own hash, so SQLite's internal consistency is what it can
//     answer): "ok" passes, a structurally damaged file is a KindCorrupt
//     CatalogError (exit 4), never a silent pass.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// insertTestSets writes n set rows through a raw connection (fixtures for
// the SetIDs reads).
func insertTestSets(t *testing.T, path string, ids ...string) {
	t.Helper()
	raw := rawOpen(t, path)
	defer raw.Close()
	for i, id := range ids {
		if _, err := raw.Exec(
			`INSERT INTO sets (set_id, source_root, created_at, tool_version) VALUES (?, ?, ?, ?)`,
			id, "/tmp/source", i, "test"); err != nil {
			t.Fatalf("inserting set %q: %v", id, err)
		}
	}
}

// An FR-034 set-scoped export answers exactly ONE set id; a whole-catalog
// handle answers every set, ascending - which is what lets the 8.5 flow
// derive the set identity from the export alone and refuse a file that
// holds more than one.
func TestSetIDsSetScopedAndWholeCatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "catalog.sqlite")
	if err := Create(path); err != nil {
		t.Fatalf("Create: %v", err)
	}
	insertTestSets(t, path, "S-B", "S-A")

	db := mustOpen(t, path, OpenReadOnly)
	defer db.Close() //nolint:errcheck // assertion handle

	ids, err := db.SetIDs(context.Background())
	if err != nil {
		t.Fatalf("SetIDs: %v", err)
	}
	if len(ids) != 2 || ids[0] != "S-A" || ids[1] != "S-B" {
		t.Errorf("SetIDs = %v, want [S-A S-B] (ascending)", ids)
	}

	// The set-scoped case: a second catalog holding one set answers a
	// single-element slice - exactly what openRestoreCatalog's ==1 gate
	// consumes.
	one := filepath.Join(t.TempDir(), "export.sqlite")
	if err := Create(one); err != nil {
		t.Fatalf("Create: %v", err)
	}
	insertTestSets(t, one, "S-ONLY")
	dbOne := mustOpen(t, one, OpenReadOnly)
	defer dbOne.Close() //nolint:errcheck // assertion handle
	got, err := dbOne.SetIDs(context.Background())
	if err != nil {
		t.Fatalf("SetIDs (set-scoped): %v", err)
	}
	if len(got) != 1 || got[0] != "S-ONLY" {
		t.Errorf("SetIDs = %v, want exactly [S-ONLY]", got)
	}
}

// A database with NO schema (user_version 0, no tables - the shape a
// wrong-file export takes) must fail LOUDLY and name what it could not
// read; answering zero rows silently would read as "empty set-scoped
// export" and mask the corruption. THE CLASS IS PART OF THE CONTRACT
// (T5.5 fix round): the failure is a KindCorrupt CatalogError - the
// media path's export-failure class, exit 4 in the cli - with the driver's
// reason still readable inside it, so the refusal names both the class
// and the table it could not read.
func TestSetIDsOnSchemalessDatabaseFailsLoudly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schemaless.sqlite")
	materializeV0(t, path)

	db := mustOpen(t, path, OpenReadOnly)
	defer db.Close() //nolint:errcheck // assertion handle

	ids, err := db.SetIDs(context.Background())
	if err == nil {
		t.Fatalf("SetIDs on a schemaless database = %v, want an error (a silent empty answer would hide the corruption)", ids)
	}
	if !strings.Contains(err.Error(), "no such table") || !strings.Contains(err.Error(), "sets") {
		t.Errorf("error %q must name the unreadable table", err)
	}
	var cerr *CatalogError
	if !errors.As(err, &cerr) || cerr.Kind != KindCorrupt {
		t.Errorf("SetIDs = %v (%T), want a KindCorrupt CatalogError (the media path's export-failure class, exit 4)", err, err)
	}
	if !strings.Contains(cerr.Error(), "sets") {
		t.Errorf("the classification must still name the failing read: %v", cerr)
	}
}

// IntegrityCheck: "ok" passes; a structurally damaged file (its LAST page
// cut) is a KindCorrupt CatalogError - the media flow's first gate before a
// single row of an aged export is read (FR-035: media ages on a shelf).
func TestIntegrityCheckVerdicts(t *testing.T) {
	t.Run("healthy database passes", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "healthy.sqlite")
		if err := Create(path); err != nil {
			t.Fatalf("Create: %v", err)
		}
		insertTestSets(t, path, "S-OK")
		db := mustOpen(t, path, OpenReadOnly)
		defer db.Close() //nolint:errcheck // assertion handle
		if err := db.IntegrityCheck(context.Background()); err != nil {
			t.Fatalf("IntegrityCheck on a healthy database: %v", err)
		}
	})

	t.Run("structurally damaged database is KindCorrupt", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "healthy.sqlite")
		if err := Create(path); err != nil {
			t.Fatalf("Create: %v", err)
		}
		insertTestSets(t, path, "S-DMG")
		// Fill pages so the file spans several (a one-page database has no
		// tail to damage without taking the schema with it).
		raw := rawOpen(t, path)
		for i := 0; i < 200; i++ {
			if _, err := raw.Exec(
				`INSERT INTO sets (set_id, source_root, created_at, tool_version) VALUES (?, ?, ?, ?)`,
				fmt.Sprintf("S-FILL-%03d", i), "/tmp/source", i, "test"); err != nil {
				t.Fatalf("filling the database: %v", err)
			}
		}
		var pageSize int
		if err := raw.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
			t.Fatalf("reading page_size: %v", err)
		}
		if err := raw.Close(); err != nil {
			t.Fatal(err)
		}
		blob, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading the database: %v", err)
		}
		if len(blob) < 3*pageSize {
			t.Fatalf("fixture anomaly: %d bytes over a %d-byte page - not enough pages to damage", len(blob), pageSize)
		}
		// One byte short of the whole last page: every earlier page (and the
		// schema) still reads, so Open accepts the file and the VERDICT is
		// IntegrityCheck's to give.
		damaged := filepath.Join(t.TempDir(), "damaged.sqlite")
		if err := os.WriteFile(damaged, blob[:len(blob)-(pageSize-1)], 0o600); err != nil {
			t.Fatalf("writing the damaged copy: %v", err)
		}
		db, oerr := Open(damaged, OpenReadOnly)
		if oerr != nil {
			// A SQLite build that refuses the damage earlier must still
			// refuse it as the SAME corrupt class (the caller - 8.5's open
			// path - maps both to exit 4).
			var cerr *CatalogError
			if !errors.As(oerr, &cerr) || cerr.Kind != KindCorrupt {
				t.Fatalf("early refusal = %v (%T), want a KindCorrupt CatalogError", oerr, oerr)
			}
			return
		}
		defer db.Close() //nolint:errcheck // assertion handle

		err = db.IntegrityCheck(context.Background())
		if err == nil {
			t.Fatal("IntegrityCheck passed on a database missing its last page")
		}
		var cerr *CatalogError
		if !errors.As(err, &cerr) || cerr.Kind != KindCorrupt {
			t.Fatalf("IntegrityCheck = %v (%T), want a KindCorrupt CatalogError", err, err)
		}
		if !strings.Contains(err.Error(), "integrity_check") {
			t.Errorf("the refusal must name the gate: %v", err)
		}
	})
}

// MediaReadErr is the classification the media path wraps its export-copy
// reads in (the T5.5 fix round: a schemaless or foreign export must fail
// as the catalog class, exit 4, not as the generic fatal): the driver's
// reason stays readable INSIDE a KindCorrupt CatalogError, and a context
// cancellation is returned unchanged - an interrupted read must never
// masquerade as a corrupt export.
func TestMediaReadErrClassification(t *testing.T) {
	path := filepath.Join(t.TempDir(), "healthy.sqlite")
	if err := Create(path); err != nil {
		t.Fatalf("Create: %v", err)
	}
	insertTestSets(t, path, "S-MRE")
	db := mustOpen(t, path, OpenReadOnly)
	defer db.Close() //nolint:errcheck // assertion handle

	t.Run("a failed read is KindCorrupt with the reason inside", func(t *testing.T) {
		err := db.MediaReadErr("reading the set ids from the sets table", errors.New("no such table: sets"))
		var cerr *CatalogError
		if !errors.As(err, &cerr) || cerr.Kind != KindCorrupt {
			t.Fatalf("MediaReadErr = %v (%T), want a KindCorrupt CatalogError", err, err)
		}
		if !strings.Contains(cerr.Path, "healthy.sqlite") {
			t.Errorf("the classification must name the export copy: path %q", cerr.Path)
		}
		for _, want := range []string{"reading the set ids from the sets table", "no such table: sets"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q must keep the failing read readable (%q)", err, want)
			}
		}
	})

	t.Run("a context cancellation stays itself", func(t *testing.T) {
		err := db.MediaReadErr("reading the set ids", context.Canceled)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("MediaReadErr(context.Canceled) = %v, want the cancellation itself", err)
		}
		var cerr *CatalogError
		if errors.As(err, &cerr) {
			t.Errorf("an interrupted read must not be classified as a corrupt export: %v", cerr)
		}
	})
}
