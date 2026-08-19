// memory_schema_v2_test.go — the v2 upgrade migration: a one-time DATA sweep
// (no new column, no trust-state/provenance schema — see
// docs/design/self-learning-loop.md's "Rejected: a trust-state/provenance
// schema" note for what this replaced). Compact behavioral coverage:
// classification, one-shot/new-watcher-survives, reversibility, and
// transaction rollback on an injected failure. "Newer schema refusal" has
// its own test already: memory_schema_version_test.go.
package main

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"testing"
)

// legacyRow is one row for a hand-built legacy-migration fixture: source and
// (optional) deletedAt ("" means live), shared by every fixture in this
// package instead of each redeclaring the same anonymous struct.
type legacyRow struct{ source, deletedAt string }

// legacySchemaNoProfile is memSchema's exact v1 shape minus `profile`, a
// store from BEFORE profile-scoping existed, shared with TestMemProfileMigration
// (host_test.go).
const legacySchemaNoProfile = `
CREATE TABLE memories (
  rowid INTEGER PRIMARY KEY, id TEXT UNIQUE NOT NULL, kind TEXT NOT NULL, content TEXT NOT NULL,
  content_hash TEXT NOT NULL, durability TEXT NOT NULL, confidence REAL NOT NULL,
  frequency INTEGER NOT NULL DEFAULT 1, reward REAL NOT NULL DEFAULT 0, access_count INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, last_accessed TEXT, expires_at TEXT, source TEXT NOT NULL,
  tags TEXT NOT NULL DEFAULT '[]', project TEXT, embedding TEXT, deleted_at TEXT
);
CREATE VIRTUAL TABLE memories_fts USING fts5(content);`

// legacyDB builds an on-disk pre-migration store against schema (one row +
// FTS entry per id->legacyRow), stamps PRAGMA user_version to userVersion (0
// skips the stamp, matching a store that predates the pragma's use here
// entirely), and returns its path for a caller to open with newMemStore and
// drive the real migration. Shared by every legacy-migration fixture in this
// package so none hand-roll their own CREATE TABLE + INSERT.
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
		var deletedAt any
		if r.deletedAt != "" {
			deletedAt = r.deletedAt
		}
		res, err := db.Exec(`INSERT INTO memories
			(id, kind, content, content_hash, durability, confidence, source, tags, created_at, deleted_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)`,
			id, "fact", content, memHash(content), "durable", 0.8, r.source, "[]", memNowIso(), deletedAt)
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

// legacyV1DB builds an on-disk pre-v2 store (full v1 shape, user_version=1).
func legacyV1DB(t *testing.T, rows map[string]legacyRow) string {
	return legacyDB(t, memSchema, rows, 1)
}

func rowDeletedAt(t *testing.T, db *sql.DB, id string) sql.NullString {
	t.Helper()
	var d sql.NullString
	if err := db.QueryRow("SELECT deleted_at FROM memories WHERE id = ?", id).Scan(&d); err != nil {
		t.Fatalf("query deleted_at for %s: %v", id, err)
	}
	return d
}

// TestSchemaV2Migration_ClassifiesBySource proves the classification rule:
// a pre-v2 row whose recorded source is 'user' or 'cli' survives live; any
// other source (the watcher, or one this binary has never seen) is
// soft-deleted. An already-deleted row is left exactly as it was — this
// migration never touches deleted_at on a row that already has one.
func TestSchemaV2Migration_ClassifiesBySource(t *testing.T) {
	const preDeletedAt = "2020-01-01T00:00:00Z"
	path := legacyV1DB(t, map[string]legacyRow{
		"user-row":     {"user", ""},
		"cli-row":      {"cli", ""},
		"watcher-row":  {"watcher", ""},
		"unknown-row":  {"some-future-tool", ""},
		"already-gone": {"watcher", preDeletedAt},
	})
	st, err := newMemStore(path, nil)
	if err != nil {
		t.Fatalf("newMemStore: %v", err)
	}
	defer st.db.Close()

	var uv int
	if err := st.db.QueryRow("PRAGMA user_version").Scan(&uv); err != nil {
		t.Fatal(err)
	}
	if uv != memSchemaVersion {
		t.Errorf("user_version = %d, want %d", uv, memSchemaVersion)
	}

	for _, id := range []string{"user-row", "cli-row"} {
		if d := rowDeletedAt(t, st.db, id); d.Valid {
			t.Errorf("%s: deleted_at = %q, want live (explicit source must survive)", id, d.String)
		}
	}
	for _, id := range []string{"watcher-row", "unknown-row"} {
		if d := rowDeletedAt(t, st.db, id); !d.Valid {
			t.Errorf("%s: deleted_at NULL, want soft-deleted (non-user/cli source)", id)
		}
	}
	if d := rowDeletedAt(t, st.db, "already-gone"); d.String != preDeletedAt {
		t.Errorf("already-gone: deleted_at = %q, want untouched %q", d.String, preDeletedAt)
	}
}

// TestSchemaV2Migration_OneShotAndNewWatcherSurvives proves the migration is
// keyed off user_version alone: it runs exactly once (a second open against
// the same, already-migrated file does not re-sweep), and a watcher row
// written AFTER the version stamp is never touched by it, even on a
// subsequent reopen.
func TestSchemaV2Migration_OneShotAndNewWatcherSurvives(t *testing.T) {
	path := legacyV1DB(t, map[string]legacyRow{
		"old-watcher-row": {"watcher", ""},
	})
	st, err := newMemStore(path, nil)
	if err != nil {
		t.Fatalf("newMemStore: %v", err)
	}
	if d := rowDeletedAt(t, st.db, "old-watcher-row"); !d.Valid {
		t.Fatal("old-watcher-row should have been swept by the v2 migration")
	}

	if _, err := st.rememberWatcherCapture(rememberInput{content: "new watcher fact"}); err != nil {
		t.Fatalf("rememberWatcherCapture: %v", err)
	}
	var newID string
	if err := st.db.QueryRow("SELECT id FROM memories WHERE content = ?", "new watcher fact").Scan(&newID); err != nil {
		t.Fatal(err)
	}
	if d := rowDeletedAt(t, st.db, newID); d.Valid {
		t.Fatal("new watcher row was soft-deleted; migration must not re-sweep post-upgrade writes")
	}
	st.db.Close()

	// Reopen: user_version is already memSchemaVersion, so migrateMemorySchema
	// must no-op entirely (no re-classification of anything).
	st2, err := newMemStore(path, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.db.Close()
	if d := rowDeletedAt(t, st2.db, "old-watcher-row"); !d.Valid {
		t.Error("old-watcher-row: reopen resurrected a row the migration had swept")
	}
	if d := rowDeletedAt(t, st2.db, newID); d.Valid {
		t.Error("new watcher row: reopen swept a row written after the version stamp")
	}
}

// TestSchemaV2Migration_RetiresLegacyPerishableRows proves the second sweep
// folded into the same one-time migration (U9): a legacy live perishable row
// (the watcher's TTL/expiry behavior, deleted along with the sweep that used
// to garbage-collect it) is soft-deleted on the SAME v1->v2 upgrade as the
// source classification, a sibling durable row is untouched, and — now that
// this is a one-time migration rather than a separate every-startup sweep —
// reviving a swept row after the schema is already at v2 is permanent, the
// same reversibility contract TestSchemaV2Migration_ReversibleBySource pins
// for the source-classification sweep.
func TestSchemaV2Migration_RetiresLegacyPerishableRows(t *testing.T) {
	path := legacyV1DB(t, map[string]legacyRow{
		"expired-perishable": {"user", ""},
		"live-perishable":    {"user", ""},
		"kept-durable":       {"user", ""},
	})
	// legacyV1DB always writes durability="durable"; flip two rows to the
	// legacy perishable marker directly. The retirement no longer distinguishes
	// an expired TTL from one not yet due (the TTL-expiry sweep itself was
	// deleted) — only the durability marker matters.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"expired-perishable", "live-perishable"} {
		if _, err := db.Exec("UPDATE memories SET durability = 'perishable' WHERE id = ?", id); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := newMemStore(path, nil)
	if err != nil {
		t.Fatalf("newMemStore: %v", err)
	}
	for _, id := range []string{"expired-perishable", "live-perishable"} {
		if d := rowDeletedAt(t, st.db, id); !d.Valid {
			t.Errorf("%s: deleted_at is NULL, want retired by the one-time migration", id)
		}
	}
	if d := rowDeletedAt(t, st.db, "kept-durable"); d.Valid {
		t.Error("kept-durable: a durable row must never be touched by the perishable retirement")
	}
	if err := st.db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reversibility + idempotence: reviving a row AFTER the schema is already
	// stamped v2 is permanent — there is no every-start sweep left to re-retire
	// it, exactly matching how the source classification's own revival behaves.
	db2, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db2.Exec("UPDATE memories SET deleted_at = NULL WHERE id = ?", "expired-perishable"); err != nil {
		t.Fatal(err)
	}
	if err := db2.Close(); err != nil {
		t.Fatal(err)
	}

	st2, err := newMemStore(path, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.db.Close()
	if d := rowDeletedAt(t, st2.db, "expired-perishable"); d.Valid {
		t.Error("expired-perishable: reopen re-swept a row revived after the migration already ran once (schema at v2); a one-time migration must not re-run")
	}
}

// TestSchemaV2Migration_ReversibleBySource proves reversibility is nothing
// new: clearing deleted_at for a swept row's id (the same mechanism an
// operator already uses to reverse an ordinary forget()) restores it to the
// standard read path.
func TestSchemaV2Migration_ReversibleBySource(t *testing.T) {
	path := legacyV1DB(t, map[string]legacyRow{
		"watcher-row": {"watcher", ""},
	})
	st, err := newMemStore(path, nil)
	if err != nil {
		t.Fatalf("newMemStore: %v", err)
	}
	defer st.db.Close()
	if d := rowDeletedAt(t, st.db, "watcher-row"); !d.Valid {
		t.Fatal("watcher-row should have been swept")
	}
	hits, err := st.recall("*", 50, 100000, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hits {
		if h.id == "watcher-row" {
			t.Fatal("swept row still visible through recall before reversal")
		}
	}

	if _, err := st.db.Exec("UPDATE memories SET deleted_at = NULL WHERE id = ?", "watcher-row"); err != nil {
		t.Fatal(err)
	}
	hits, err = st.recall("*", 50, 100000, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, h := range hits {
		if h.id == "watcher-row" {
			found = true
		}
	}
	if !found {
		t.Error("clearing deleted_at did not restore the row to recall")
	}
}

// TestSchemaV2Migration_RollsBackOnInjectedFailure proves the transaction is
// truly atomic: a real failure partway through (here, the classification
// query hitting a schema that has no `source` column at all) rolls back
// EVERY statement already run in the same transaction, including the
// earlier profile-column ALTER, not just the one that failed.
func TestSchemaV2Migration_RollsBackOnInjectedFailure(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// A table missing BOTH `profile` (so the ALTER branch runs first) and
	// `source` (so the classification query that runs after it fails) —
	// an injected mid-transaction failure with no snapshot/seam needed.
	if _, err := db.Exec(`CREATE TABLE memories (
		rowid INTEGER PRIMARY KEY, id TEXT UNIQUE NOT NULL, kind TEXT NOT NULL, content TEXT NOT NULL,
		content_hash TEXT NOT NULL, durability TEXT NOT NULL, confidence REAL NOT NULL,
		created_at TEXT NOT NULL, tags TEXT NOT NULL DEFAULT '[]', deleted_at TEXT
	)`); err != nil {
		t.Fatal(err)
	}

	if err := migrateMemorySchema(db, 0); err == nil {
		t.Fatal("migrateMemorySchema succeeded against a schema missing `source`; want an error")
	}

	hasProfile, err := memColumnExists(db, "memories", "profile")
	if err != nil {
		t.Fatal(err)
	}
	if hasProfile {
		t.Error("profile column present after a rolled-back migration; the ALTER must not have survived")
	}
	var uv int
	if err := db.QueryRow("PRAGMA user_version").Scan(&uv); err != nil {
		t.Fatal(err)
	}
	if uv != 0 {
		t.Errorf("user_version = %d after a rolled-back migration, want 0 (unchanged)", uv)
	}
}
