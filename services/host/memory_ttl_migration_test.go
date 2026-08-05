// memory_ttl_migration_test.go — migrateLegacyWatcherPerishableTTL, the
// idempotent startup migration that shortens pre-existing watcher/perishable
// rows still carrying the old 21-day TTL down to the current 7-day TTL. No
// schema change: it only rewrites expires_at on rows that qualify, via
// newMemStore on an existing on-disk database (mirroring the legacy-schema
// migration test in host_test.go).
package main

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// insertLegacyMemory inserts a row directly (bypassing store.remember, whose
// TTL logic is exactly what we're simulating a database written BEFORE) with
// an explicit created_at/expires_at/source/durability, so the migration has
// something concrete to act on.
func insertLegacyMemory(t *testing.T, db *sql.DB, id, content, source, durability string, createdAt, expiresAt time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO memories
		(id, kind, content, content_hash, durability, confidence, reward, source, tags, created_at, expires_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		id, "fact", content, memHash(content), durability, 0.6, 0.0, source, "[]",
		createdAt.UTC().Format(time.RFC3339Nano), expiresAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatalf("insert legacy memory %s: %v", id, err)
	}
}

func expiresAtFor(t *testing.T, db *sql.DB, id string) time.Time {
	t.Helper()
	var s string
	if err := db.QueryRow("SELECT expires_at FROM memories WHERE id = ?", id).Scan(&s); err != nil {
		t.Fatalf("read expires_at for %s: %v", id, err)
	}
	got, ok := parseTimeStrict(s)
	if !ok {
		t.Fatalf("expires_at for %s did not parse: %q", id, s)
	}
	return got
}

func TestMigrateLegacyWatcherPerishableTTL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(memSchema); err != nil {
		t.Fatalf("schema: %v", err)
	}

	created := time.Now().Add(-2 * 24 * time.Hour) // created 2 days ago
	legacy21d := created.Add(21 * 24 * time.Hour)  // the old TTL: expires 19 days from now
	sevenDayCap := created.Add(7 * 24 * time.Hour) // what it should become: 5 days from now

	// (a) A watcher/perishable row still on the old 21-day TTL — must be
	// shortened to created_at+7d.
	insertLegacyMemory(t, db, "watcher-legacy", "currently migrating the staging DB", "watcher", "perishable", created, legacy21d)

	// (b) A non-watcher row with the same far-out expiry — a user's own custom
	// TTL, or an "agent" source — must be left exactly alone.
	insertLegacyMemory(t, db, "user-custom-ttl", "user-set reminder, do not touch", "user", "perishable", created, legacy21d)

	// (c) A watcher/perishable row already at (or before) the 7-day cap — the
	// current/expected shape — must be left exactly alone (idempotent, and
	// never LENGTHENS a shorter TTL).
	alreadyShort := created.Add(3 * 24 * time.Hour)
	insertLegacyMemory(t, db, "watcher-already-short", "already on the current 7-day TTL", "watcher", "perishable", created, alreadyShort)

	// (d) A watcher/durable row (a fact, not an event) — durability != perishable
	// must never be touched even though the source matches.
	insertLegacyMemory(t, db, "watcher-durable", "a durable watcher-captured fact", "watcher", "durable", created, legacy21d)

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen through newMemStore — the migration must run cleanly, exactly like
	// the profile-column ALTER migration it sits next to.
	st, err := newMemStore(path, nil)
	if err != nil {
		t.Fatalf("newMemStore: %v", err)
	}

	if got := expiresAtFor(t, st.db, "watcher-legacy"); got.After(sevenDayCap.Add(time.Second)) || got.Before(sevenDayCap.Add(-time.Second)) {
		t.Errorf("watcher-legacy expires_at = %v, want ~%v (created_at+7d)", got, sevenDayCap)
	}
	if got := expiresAtFor(t, st.db, "user-custom-ttl"); !got.Equal(legacy21d) {
		t.Errorf("user-custom-ttl expires_at = %v, want unchanged %v (non-watcher rows must never be touched)", got, legacy21d)
	}
	if got := expiresAtFor(t, st.db, "watcher-already-short"); !got.Equal(alreadyShort) {
		t.Errorf("watcher-already-short expires_at = %v, want unchanged %v (must not lengthen an already-shorter TTL)", got, alreadyShort)
	}
	if got := expiresAtFor(t, st.db, "watcher-durable"); !got.Equal(legacy21d) {
		t.Errorf("watcher-durable expires_at = %v, want unchanged %v (durable rows are never touched)", got, legacy21d)
	}

	// Re-running the migration (e.g. a second newMemStore open) must be a no-op:
	// the now-shortened watcher-legacy row must not be touched again.
	if err := migrateLegacyWatcherPerishableTTL(st.db); err != nil {
		t.Fatalf("second migration pass: %v", err)
	}
	if got := expiresAtFor(t, st.db, "watcher-legacy"); got.After(sevenDayCap.Add(time.Second)) || got.Before(sevenDayCap.Add(-time.Second)) {
		t.Errorf("watcher-legacy expires_at after re-running migration = %v, want unchanged ~%v", got, sevenDayCap)
	}
}

// TestMemStoreSchemaVersionStamp covers the OTHER half of opening an existing
// db: a fresh store stamps user_version=1 (what a snapshot records and a
// restore gates on), and a db written by a NEWER pix (user_version=2) is
// refused rather than silently downgraded to the 1 marker, which would corrupt
// a forward-incompatible schema.
func TestMemStoreSchemaVersionStamp(t *testing.T) {
	st, err := newMemStore(filepath.Join(t.TempDir(), "fresh.db"), nil)
	if err != nil {
		t.Fatalf("newMemStore: %v", err)
	}
	var uv int
	if err := st.db.QueryRow("PRAGMA user_version").Scan(&uv); err != nil {
		t.Fatalf("PRAGMA user_version: %v", err)
	}
	st.db.Close()
	if uv != 1 {
		t.Errorf("fresh store user_version = %d, want 1", uv)
	}

	path := filepath.Join(t.TempDir(), "future.db")
	future, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := future.Exec("PRAGMA user_version = 2"); err != nil {
		t.Fatal(err)
	}
	if _, err := future.Exec("CREATE TABLE t(x)"); err != nil { // force the header to disk
		t.Fatal(err)
	}
	future.Close()
	if _, err := newMemStore(path, nil); err == nil {
		t.Error("newMemStore accepted a db with user_version=2; want a version error")
	}
}
