// memory_recall_star_test.go — the recall query "*" (trimmed) gets explicit,
// deterministic list-all semantics: newest-first, respecting kind/project/
// profile visibility, limit, and charBudget, with normal hit fields/scores —
// and it must work with NO FTS match and NO embedder (keyword-only store),
// which is exactly why `pix memory recall '*'` and a blank sandbox
// /recall need their own explicit path rather than falling through to the
// relevance scorer (which can legitimately return nothing for "*").
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRecallStar_ReturnsInsertedRows_NoEmbedderNoFTSMatch is the core store-
// level gate: a store with no embedder configured (nil embedder, so recall's
// normal vector path is unavailable) and a query that would never match FTS
// (content has no shared keyword with "*") must still return every inserted
// row for "*", newest first.
func TestRecallStar_ReturnsInsertedRows_NoEmbedderNoFTSMatch(t *testing.T) {
	st, err := newMemStore(":memory:", nil) // no embedder: keyword-only store
	if err != nil {
		t.Fatal(err)
	}
	contents := []string{"alpha content one", "bravo content two", "charlie content three"}
	for _, c := range contents {
		if _, err := st.remember(rememberInput{content: c}); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := st.recall("*", 0, 0, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != len(contents) {
		t.Fatalf("recall(\"*\") returned %d hits, want %d: %+v", len(hits), len(contents), hits)
	}
	// Newest first: the last-remembered row (charlie) comes first.
	if hits[0].content != "charlie content three" {
		t.Errorf("recall(\"*\") hits[0].content = %q, want newest row first", hits[0].content)
	}
	if hits[len(hits)-1].content != "alpha content one" {
		t.Errorf("recall(\"*\") last hit = %q, want oldest row last", hits[len(hits)-1].content)
	}
	// Normal hit fields/scores: every hit must carry a non-empty id/kind/
	// durability/createdAt and a positive score, exactly like a relevance match.
	for _, h := range hits {
		if h.id == "" || h.kind == "" || h.durability == "" || h.createdAt == "" {
			t.Errorf("recall(\"*\") hit missing normal fields: %+v", h)
		}
		if h.score <= 0 {
			t.Errorf("recall(\"*\") hit has non-positive score: %+v", h)
		}
	}
}

// TestRecallStar_TrimsWhitespace: " * " (leading/trailing space) is still the
// literal list-all query, since callers pass a trimmed or untrimmed "*"
// depending on the surface (CLI vs extension).
func TestRecallStar_TrimsWhitespace(t *testing.T) {
	st, err := newMemStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.remember(rememberInput{content: "only fact"}); err != nil {
		t.Fatal(err)
	}
	hits, err := st.recall("  *  ", 0, 0, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("recall(\"  *  \") returned %d hits, want 1", len(hits))
	}
}

// TestRecallStar_RespectsLimit proves the limit cap is applied on the list-all
// path exactly as it is on the normal relevance path.
func TestRecallStar_RespectsLimit(t *testing.T) {
	st, err := newMemStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := st.remember(rememberInput{content: "fact number " + string(rune('a'+i))}); err != nil {
			t.Fatal(err)
		}
	}
	hits, err := st.recall("*", 3, 0, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 3 {
		t.Fatalf("recall(\"*\", limit=3) returned %d hits, want 3", len(hits))
	}
}

// TestRecallStar_RespectsCharBudget proves the char budget truncation applies
// on the list-all path: once the running content-length total would exceed
// charBudget, no further hits are added (but at least one hit is always kept).
func TestRecallStar_RespectsCharBudget(t *testing.T) {
	st, err := newMemStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("x", 50)
	for i := 0; i < 5; i++ {
		if _, err := st.remember(rememberInput{content: long + string(rune('a'+i))}); err != nil {
			t.Fatal(err)
		}
	}
	// Budget for ~2 rows (51 chars each).
	hits, err := st.recall("*", 0, 110, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 1 || len(hits) > 2 {
		t.Fatalf("recall(\"*\", charBudget=110) returned %d hits, want 1-2", len(hits))
	}
}

// TestRecallStar_ProfileVisibility proves "*" respects the same profile
// visibility rule as a normal relevance recall: a named profile sees its own
// rows UNION the default bucket, but never a sibling named profile's rows.
func TestRecallStar_ProfileVisibility(t *testing.T) {
	st, err := newMemStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.remember(rememberInput{content: "shared default fact", profile: ""}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.remember(rememberInput{content: "work-only fact", profile: "work"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.remember(rememberInput{content: "personal-only fact", profile: "personal"}); err != nil {
		t.Fatal(err)
	}

	work, err := st.recall("*", 0, 0, "", "", "work")
	if err != nil {
		t.Fatal(err)
	}
	if len(work) != 2 {
		t.Fatalf("work profile recall(\"*\") = %d hits, want 2 (own + default): %+v", len(work), work)
	}
	for _, h := range work {
		if h.content == "personal-only fact" {
			t.Errorf("work profile recall(\"*\") leaked a personal-only row: %+v", h)
		}
	}

	def, err := st.recall("*", 0, 0, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(def) != 1 {
		t.Fatalf("default profile recall(\"*\") = %d hits, want 1 (shared only): %+v", len(def), def)
	}
	if def[0].content != "shared default fact" {
		t.Errorf("default profile recall(\"*\") = %+v, want only the shared fact", def)
	}
}

// TestRecallStar_KindFilter proves "*" still honors an explicit kind filter.
func TestRecallStar_KindFilter(t *testing.T) {
	st, err := newMemStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.remember(rememberInput{content: "a fact", kind: "fact"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.remember(rememberInput{content: "a learning", kind: "learning"}); err != nil {
		t.Fatal(err)
	}
	hits, err := st.recall("*", 0, 0, "learning", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].content != "a learning" {
		t.Fatalf("recall(\"*\", kind=learning) = %+v, want only the learning", hits)
	}
}

// TestMemoryMux_RecallStar_EndToEnd is the RPC-level end-to-end gate: a
// JSON-RPC "recall" call with query "*" against a store with no embedder
// returns every visible row over the wire, with normal hit fields.
func TestMemoryMux_RecallStar_EndToEnd(t *testing.T) {
	store, err := newMemStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{"first fact", "second fact", "third fact"} {
		if _, err := store.remember(rememberInput{content: c}); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(newMemoryMux(store, false))
	defer srv.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"recall","params":{"query":"*"}}`
	res, err := http.Post(srv.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var parsed struct {
		Result struct {
			Hits []map[string]any `json:"hits"`
		} `json:"result"`
	}
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		t.Fatalf("bad json response: %v", err)
	}
	if len(parsed.Result.Hits) != 3 {
		t.Fatalf("recall RPC with query \"*\" returned %d hits, want 3: %+v", len(parsed.Result.Hits), parsed.Result.Hits)
	}
	for _, h := range parsed.Result.Hits {
		if _, ok := h["id"].(string); !ok {
			t.Errorf("recall RPC hit missing id: %+v", h)
		}
		ca, ok := h["createdAt"].(string)
		if !ok || strings.TrimSpace(ca) == "" {
			t.Errorf("recall RPC hit missing createdAt: %+v", h)
		}
		if _, ok := h["score"].(float64); !ok {
			t.Errorf("recall RPC hit missing score: %+v", h)
		}
	}
}
