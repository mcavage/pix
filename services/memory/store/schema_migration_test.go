// schema_migration_test.go is the old-DB compatibility test: a store
// written by a pre-v2 (pre-profile-scoping) binary must open cleanly, gain
// the profile column, and have its historical non-explicit/perishable rows
// classified exactly the way services/host's migrateMemorySchema already
// proved (see services/host/memory_schema_v2_test.go, the sibling this test
// mirrors for the standalone module). This is the compatibility contract
// docs/design/pix-v2-architecture.md §9.2 requires: "Existing database
// schema and on-disk rows are preserved unless a specific migration is
// required."
package store

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"
)

// legacySchemaNoProfile is schemaSQL's exact v1 shape minus `profile`: a
// store from before profile-scoping existed.
const legacySchemaNoProfile = `
CREATE TABLE memories (
  rowid INTEGER PRIMARY KEY, id TEXT UNIQUE NOT NULL, kind TEXT NOT NULL, content TEXT NOT NULL,
  content_hash TEXT NOT NULL, durability TEXT NOT NULL, confidence REAL NOT NULL,
  frequency INTEGER NOT NULL DEFAULT 1, reward REAL NOT NULL DEFAULT 0, access_count INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, last_accessed TEXT, expires_at TEXT, source TEXT NOT NULL,
  tags TEXT NOT NULL DEFAULT '[]', project TEXT, embedding TEXT, deleted_at TEXT
);
CREATE VIRTUAL TABLE memories_fts USING fts5(content);`

type legacyRow struct{ source, durability, deletedAt string }

// legacyDB builds an on-disk pre-migration store (one row + FTS entry per
// id->legacyRow), stamps PRAGMA user_version (0 skips the stamp, matching a
// store that predates the pragma's use here entirely), and returns its path.
func legacyDB(t *testing.T, schema string, rows map[string]legacyRow, userVersion int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	for id, r := range rows {
		content := "content for " + id
		durability := r.durability
		if durability == "" {
			durability = "durable"
		}
		var deletedAt any
		if r.deletedAt != "" {
			deletedAt = r.deletedAt
		}
		res, err := db.Exec(`INSERT INTO memories
			(id, kind, content, content_hash, durability, confidence, source, tags, created_at, deleted_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)`,
			id, "fact", content, hashContent(content), durability, 0.8, r.source, "[]", nowISO(), deletedAt)
		if err != nil {
			t.Fatal(err)
		}
		rowid, _ := res.LastInsertId()
		if _, err := db.Exec("INSERT INTO memories_fts (rowid, content) VALUES (?, ?)", rowid, content); err != nil {
			t.Fatal(err)
		}
	}
	if userVersion != 0 {
		if _, err := db.Exec("PRAGMA user_version = " + strconv.Itoa(userVersion)); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func liveIDs(t *testing.T, db *sql.DB) map[string]bool {
	t.Helper()
	rows, err := db.Query("SELECT id FROM memories WHERE deleted_at IS NULL")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out[id] = true
	}
	return out
}

// TestOldDBWithoutProfileColumnOpensAndBackfills is the exact "pre-existing
// installation upgrades pix-memory" scenario: a v1 db with no profile
// column, entirely explicit rows, opened for the first time by this module.
func TestOldDBWithoutProfileColumnOpensAndBackfills(t *testing.T) {
	path := legacyDB(t, legacySchemaNoProfile, map[string]legacyRow{
		"a": {source: "user"},
		"b": {source: "cli"},
	}, 1)

	st, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open(legacy v1 db): %v", err)
	}
	defer st.Close()

	if got := st.SchemaVersion(); got != schemaVersion {
		t.Fatalf("SchemaVersion() = %d, want %d", got, schemaVersion)
	}
	has, err := columnExists(st.db, "memories", "profile")
	if err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Fatal("profile column was not backfilled")
	}
	stats := st.Stats("")
	if stats.Active != 2 || stats.Deleted != 0 {
		t.Fatalf("Stats() = %+v, want 2 active, 0 deleted (both rows are explicit, so neither is swept)", stats)
	}

	// The old data is READABLE through the normal API, not just present on
	// disk: recall("*") must surface both pre-existing rows.
	hits, err := st.Recall("*", 10, 0, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("Recall(\"*\") returned %d hits, want 2: %+v", len(hits), hits)
	}
}

// TestOldDBNonExplicitRowsSoftDeletedOnUpgrade proves the v2 migration's
// advisory classification sweep: a legacy row whose source is neither
// "user" nor "cli" (the pre-v2 free-text vocabulary, e.g. an old watcher
// capture or an unrecognized label) is soft-deleted on the FIRST open by
// this module, while explicit rows are left alone.
func TestOldDBNonExplicitRowsSoftDeletedOnUpgrade(t *testing.T) {
	path := legacyDB(t, legacySchemaNoProfile, map[string]legacyRow{
		"explicit-user":     {source: "user"},
		"explicit-cli":      {source: "cli"},
		"legacy-watcher":    {source: "watcher"},
		"legacy-unknown":    {source: "something-else"},
		"already-deleted":   {source: "watcher", deletedAt: "2020-01-01T00:00:00Z"},
		"legacy-perishable": {source: "user", durability: "perishable"},
	}, 1)

	st, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	live := liveIDs(t, st.db)
	for _, id := range []string{"explicit-user", "explicit-cli"} {
		if !live[id] {
			t.Errorf("%s: explicit row was soft-deleted by migration, want it left alone", id)
		}
	}
	for _, id := range []string{"legacy-watcher", "legacy-unknown", "legacy-perishable"} {
		if live[id] {
			t.Errorf("%s: non-explicit/perishable legacy row survived migration as live, want soft-deleted", id)
		}
	}
	if live["already-deleted"] {
		t.Fatalf("already-deleted: row that was already soft-deleted must not become live")
	}
}

// TestOldDBAlreadyAtCurrentVersionIsUntouched proves migrateSchema is a
// true no-op past its gate: a store already stamped schemaVersion, even one
// holding rows that LOOK like they'd match the legacy sweep predicates
// (e.g. source="watcher"), is never touched by it, because production
// rows the live binary itself wrote after migrating must never be swept
// again.
func TestOldDBAlreadyAtCurrentVersionIsUntouched(t *testing.T) {
	path := legacyDB(t, schemaSQL, map[string]legacyRow{
		"watcher-row-post-v2": {source: "watcher"},
	}, schemaVersion)

	st, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if !liveIDs(t, st.db)["watcher-row-post-v2"] {
		t.Fatal("a store already at schemaVersion must not re-run the migration sweep")
	}
}

// TestNewerSchemaVersionIsRefused proves Open refuses to silently downgrade
// a db written by a newer pix-memory.
func TestNewerSchemaVersionIsRefused(t *testing.T) {
	path := legacyDB(t, schemaSQL, nil, schemaVersion+1)
	if _, err := Open(path, nil); err == nil {
		t.Fatal("Open(db from a newer schema version) succeeded, want a refusal")
	}
}
