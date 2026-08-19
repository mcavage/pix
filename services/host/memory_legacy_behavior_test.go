// memory_legacy_behavior_test.go — behavior coverage for two of the
// write-path deletions this unit makes: a legacy caller's durability/ttlDays
// input is silently ignored (not just absent from the struct), and reward no
// longer moves recall's score no matter how it got into a row. The one-time
// legacy-perishable-row retirement is now part of the schema v2 migration
// (folded in, U9); its classification/idempotence/reversibility coverage
// lives with the rest of that migration in memory_schema_v2_test.go.
package main

import (
	"database/sql"
	"testing"
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
