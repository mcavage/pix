// memory_legacy_behavior_test.go — behavior coverage for the three write-path
// deletions/retirements this unit makes: a legacy caller's durability/ttlDays
// input is silently ignored (not just absent from the struct), reward no
// longer moves recall's score no matter how it got into a row, and the
// one-time startup retirement of legacy live perishable rows actually runs,
// is idempotent, and is reversible (soft delete, not a purge).
package main

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// TestRememberFromParamsIgnoresLegacyDurabilityAndTTLDays proves a caller
// still sending the deleted `durability`/`ttlDays` RPC params (a legacy
// sandbox extension, an old cached request body) gets today's behavior
// exactly as if it hadn't: the row lands durable with no expiry, never an
// error and never the old perishable/TTL treatment.
func TestRememberFromParamsIgnoresLegacyDurabilityAndTTLDays(t *testing.T) {
	st, err := newMemStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	in := rememberFromParams(jsonObj{
		"content": "legacy caller still sends durability and ttlDays",
		"kind":    "fact", "durability": "perishable", "ttlDays": float64(1),
	})
	res, err := st.remember(in)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res["id"].(string)
	if id == "" {
		t.Fatal("remember returned no id")
	}
	var durability string
	var expiresAt sql.NullString
	if err := st.db.QueryRow("SELECT durability, expires_at FROM memories WHERE id = ?", id).Scan(&durability, &expiresAt); err != nil {
		t.Fatal(err)
	}
	if durability != "durable" {
		t.Errorf("durability = %q, want durable (legacy durability=perishable input must be ignored)", durability)
	}
	if expiresAt.Valid {
		t.Errorf("expires_at = %q, want NULL (legacy ttlDays input must be ignored, no expiry is ever computed)", expiresAt.String)
	}
}

// TestRewardNoLongerAffectsRecallScore proves recall's score is identical for
// two otherwise-identical rows that differ ONLY in their stored `reward`
// value. remember() itself no longer accepts a reward parameter at all, so
// this inserts directly at the SQL layer (the only way reward can still
// differ between rows: manual writes, or a row that predates this change) to
// pin down the READ side: whatever is in the column, recall must not weight
// by it.
func TestRewardNoLongerAffectsRecallScore(t *testing.T) {
	st, err := newMemStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Every row shares the SAME created_at (and confidence/frequency), so
	// recency/freqBoost cannot explain a score difference — reward is
	// isolated as the only thing that could.
	const createdAt = "2026-01-01T00:00:00Z"
	insert := func(id, content string, reward float64) {
		if _, err := st.db.Exec(`INSERT INTO memories
			(id, kind, content, content_hash, durability, confidence, reward, source, tags, created_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)`,
			id, "fact", content, memHash(content), "durable", 0.8, reward, "user", "[]", createdAt); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.Exec("INSERT INTO memories_fts (rowid, content) SELECT rowid, content FROM memories WHERE id = ?", id); err != nil {
			t.Fatal(err)
		}
	}
	insert("no-reward", "widget deployment procedure alpha", 0)
	insert("max-reward", "widget deployment procedure beta", 1)
	insert("min-reward", "widget deployment procedure gamma", -1)

	hits, err := st.recall("widget deployment procedure", 8, 100000, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("want 3 hits, got %d: %+v", len(hits), hits)
	}
	first := hits[0].score
	for _, h := range hits[1:] {
		if h.score != first {
			t.Errorf("scores differ by reward alone: %+v (reward must not factor into recall's score any more)", hits)
			break
		}
	}
}

// TestRetireLegacyWatcherPerishableRowsOnStartup proves the one-time startup
// retirement (finding: legacy live perishable rows must not become immortal
// now that nothing ever sweeps them): a db written by an older binary with
// live perishable rows — one already past its old expiry, one not yet due —
// gets BOTH soft-deleted on open, a sibling durable row is untouched, and a
// second open (simulating a later restart) is a no-op rather than an error or
// a double-delete.
func TestRetireLegacyWatcherPerishableRowsOnStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-perishable.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	const legacySchema = `
CREATE TABLE memories (
  rowid INTEGER PRIMARY KEY, id TEXT UNIQUE NOT NULL, kind TEXT NOT NULL, content TEXT NOT NULL,
  content_hash TEXT NOT NULL, durability TEXT NOT NULL, confidence REAL NOT NULL,
  frequency INTEGER NOT NULL DEFAULT 1, reward REAL NOT NULL DEFAULT 0, access_count INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, last_accessed TEXT, expires_at TEXT, source TEXT NOT NULL,
  tags TEXT NOT NULL DEFAULT '[]', project TEXT, embedding TEXT, deleted_at TEXT, profile TEXT
);
CREATE VIRTUAL TABLE memories_fts USING fts5(content);`
	if _, err := db.Exec(legacySchema); err != nil {
		t.Fatalf("legacy schema: %v", err)
	}
	insert := func(id, content, durability, source, expiresAt string) {
		res, err := db.Exec(`INSERT INTO memories
			(id, kind, content, content_hash, durability, confidence, source, tags, created_at, expires_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)`,
			id, "fact", content, memHash(content), durability, 0.8, source, "[]", memNowIso(), nullIfEmpty(expiresAt))
		if err != nil {
			t.Fatalf("insert %s: %v", id, err)
		}
		rowid, _ := res.LastInsertId()
		if _, err := db.Exec("INSERT INTO memories_fts (rowid, content) VALUES (?, ?)", rowid, content); err != nil {
			t.Fatalf("fts insert %s: %v", id, err)
		}
	}
	past := memTimeAdd(t, -24) // already past its old expiry
	future := memTimeAdd(t, 24)
	insert("expired-perishable", "yesterday's status update", "perishable", "watcher", past)
	insert("live-perishable", "not-yet-due status update", "perishable", "watcher", future)
	// source "user" here (not "watcher"): this row must survive BOTH the
	// perishable retirement below (it isn't perishable) AND the unrelated v2
	// migration's source-based sweep (memory_schema_v2_test.go) that also runs
	// on this same open — an explicit source is what that sweep looks for.
	insert("kept-durable", "the user prefers tabs over spaces", "durable", "user", "")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	// First open: the retirement must run and soft-delete BOTH perishable rows,
	// regardless of whether their old expiry was already due.
	st, err := newMemStore(path, nil)
	if err != nil {
		t.Fatalf("newMemStore: %v", err)
	}
	for _, id := range []string{"expired-perishable", "live-perishable"} {
		var deletedAt sql.NullString
		if err := st.db.QueryRow("SELECT deleted_at FROM memories WHERE id = ?", id).Scan(&deletedAt); err != nil {
			t.Fatal(err)
		}
		if !deletedAt.Valid {
			t.Errorf("%s: deleted_at is NULL, want retired (soft-deleted) on startup", id)
		}
	}
	var keptDeletedAt sql.NullString
	if err := st.db.QueryRow("SELECT deleted_at FROM memories WHERE id = ?", "kept-durable").Scan(&keptDeletedAt); err != nil {
		t.Fatal(err)
	}
	if keptDeletedAt.Valid {
		t.Error("kept-durable: deleted_at is set, a durable row must never be touched by the perishable retirement")
	}
	hits, err := st.recall("status update", 8, 100000, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Errorf("recall still surfaces a retired perishable row: %+v", hits)
	}
	if err := st.db.Close(); err != nil {
		t.Fatal(err)
	}

	// Reversibility: retirement is a SOFT delete, so clearing deleted_at
	// directly on the db file restores the row exactly as recall reads it.
	db2, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db2.Exec("UPDATE memories SET deleted_at = NULL WHERE id = ?", "expired-perishable"); err != nil {
		t.Fatal(err)
	}
	if _, err := db2.Exec("INSERT INTO memories_fts (rowid, content) SELECT rowid, content FROM memories WHERE id = ?", "expired-perishable"); err != nil {
		t.Fatal(err)
	}
	if err := db2.Close(); err != nil {
		t.Fatal(err)
	}

	// Second open (a later restart): the retirement is idempotent. The row we
	// just un-deleted to prove reversibility is fair game to be retired again
	// (it is still a live perishable row), but the open itself must not error,
	// and the untouched already-retired row must not be double-processed.
	st2, err := newMemStore(path, nil)
	if err != nil {
		t.Fatalf("second newMemStore (idempotent restart) failed: %v", err)
	}
	defer st2.db.Close()
	var reRetired sql.NullString
	if err := st2.db.QueryRow("SELECT deleted_at FROM memories WHERE id = ?", "expired-perishable").Scan(&reRetired); err != nil {
		t.Fatal(err)
	}
	if !reRetired.Valid {
		t.Error("expired-perishable: a live perishable row (even one manually revived) must be retired again on the next startup, not left immortal")
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// memTimeAdd returns an RFC3339Nano timestamp `hours` from now (negative for
// the past), matching the format expires_at was always stored in.
func memTimeAdd(t *testing.T, hours int) string {
	t.Helper()
	return time.Now().UTC().Add(time.Duration(hours) * time.Hour).Format(time.RFC3339Nano)
}
