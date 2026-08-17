// memory_access_metadata_test.go — U4-live-embed-recall: recall performs ZERO
// access_count/last_accessed writes, star (plain listing) or scored. Those
// columns have no reader anywhere in this codebase (see recall's own comment
// in memory.go): they stay in the schema, inert, for additive/legacy
// compatibility only. This single test covers both recall shapes; a prior
// version tested star and scored separately because scored recall used to
// write a batched UPDATE that star deliberately skipped — now neither path
// writes anything, so there is nothing left to distinguish between them.
package main

import "testing"

// accessMeta reads back access_count/last_accessed for one id.
func accessMeta(t *testing.T, st *memStore, id string) (count int, accessed *string) {
	t.Helper()
	var lastAccessed *string
	if err := st.db.QueryRow("SELECT access_count, last_accessed FROM memories WHERE id = ?", id).Scan(&count, &lastAccessed); err != nil {
		t.Fatalf("accessMeta(%s): %v", id, err)
	}
	return count, lastAccessed
}

// TestRecall_NeverMutatesAccessMetadata proves recall is read-only with
// respect to access_count/last_accessed on both the star (list-all) and
// scored paths: a caller can browse or search the store (a UI view, a
// diagnostic dump, a normal recall lookup) without perturbing either column,
// because nothing reads them back to make that bookkeeping worth doing.
func TestRecall_NeverMutatesAccessMetadata(t *testing.T) {
	st, err := newMemStore(":memory:", fakeKeywordEmbedder(16))
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, 3)
	for _, c := range []string{"widget spins fast", "widget on the shelf", "widget maintenance schedule"} {
		res, err := st.remember(rememberInput{content: c})
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res["id"].(string)
		ids = append(ids, id)
	}

	// Star: plain listing.
	starHits, err := st.recall("*", 0, 0, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(starHits) != 3 {
		t.Fatalf("recall(\"*\") returned %d hits, want 3", len(starHits))
	}

	// Scored: an ordinary keyword/vector lookup that actually matches every row.
	scoredHits, err := st.recall("widget", 10, 0, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(scoredHits) != 3 {
		t.Fatalf("recall(\"widget\") returned %d hits, want 3: %+v", len(scoredHits), scoredHits)
	}

	for _, id := range ids {
		count, accessed := accessMeta(t, st, id)
		if count != 0 {
			t.Errorf("id %s: access_count = %d after recall, want 0 (no reader consumes it)", id, count)
		}
		if accessed != nil {
			t.Errorf("id %s: last_accessed = %v after recall, want unset (no reader consumes it)", id, *accessed)
		}
	}
}
