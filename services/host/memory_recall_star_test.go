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
	// createdAt and a positive score, exactly like a relevance match.
	for _, h := range hits {
		if h.id == "" || h.kind == "" || h.createdAt == "" {
			t.Errorf("recall(\"*\") hit missing normal fields: %+v", h)
		}
		if h.score <= 0 {
			t.Errorf("recall(\"*\") hit has non-positive score: %+v", h)
		}
	}
}

// TestRecallStar_Basics table-drives four independent list-all knobs that
// share the exact same shape (seed rows, call recall("*", ...) with one
// non-default param, assert a hit-count range): whitespace-trimming the
// literal query, the limit cap, the char-budget cutoff, and an explicit kind
// filter all apply on the list-all path exactly as they do on the normal
// relevance path.
func TestRecallStar_Basics(t *testing.T) {
	long := strings.Repeat("x", 50)
	cases := []struct {
		name              string
		contents          []string
		query             string
		limit, charBudget int
		kind              string
		wantMin, wantMax  int
	}{
		{name: "trims whitespace", contents: []string{"only fact"}, query: "  *  ", wantMin: 1, wantMax: 1},
		{name: "respects limit", contents: []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j"}, query: "*", limit: 3, wantMin: 3, wantMax: 3},
		{name: "respects char budget", contents: []string{long + "a", long + "b", long + "c", long + "d", long + "e"}, query: "*", charBudget: 110, wantMin: 1, wantMax: 2}, // budget for ~2 rows (51 chars each)
		{name: "kind filter", contents: []string{"a fact", "a learning"}, query: "*", kind: "learning", wantMin: 1, wantMax: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, err := newMemStore(":memory:", nil)
			if err != nil {
				t.Fatal(err)
			}
			for i, c := range tc.contents {
				k := "fact"
				if tc.kind != "" && i == len(tc.contents)-1 {
					k = tc.kind // kind-filter case: only the last seeded row is the target kind
				}
				if _, err := st.remember(rememberInput{content: c, kind: k}); err != nil {
					t.Fatal(err)
				}
			}
			hits, err := st.recall(tc.query, tc.limit, tc.charBudget, tc.kind, "", "")
			if err != nil {
				t.Fatal(err)
			}
			if len(hits) < tc.wantMin || len(hits) > tc.wantMax {
				t.Fatalf("recall(%q) returned %d hits, want %d-%d: %+v", tc.query, len(hits), tc.wantMin, tc.wantMax, hits)
			}
		})
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

// TestRecallStar_OrdersByRowidNotCreatedAt proves the fix directly: rowid
// (the sqlite autoincrement counter, which only ever tracks insertion order)
// is the authoritative "newest" sort key for "*", NOT created_at. A wall-clock
// read can regress (NTP step-back) or tie (coarse clock resolution, two
// inserts landing in the same tick), so this test inserts rows in a known
// order via remember() (fixing their rowid sequence) and then overwrites
// created_at directly to a NONMONOTONIC, partially-EQUAL sequence that
// contradicts insertion order. If recall("*") ordered by created_at (as it
// used to), this would return the rows in the wrong order; ordering by rowid
// alone must still return them in true insertion order regardless.
func TestRecallStar_OrdersByRowidNotCreatedAt(t *testing.T) {
	st, err := newMemStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Insertion order (and therefore rowid order): first, second, third, fourth.
	ids := make([]string, 0, 4)
	for _, c := range []string{"first fact", "second fact", "third fact", "fourth fact"} {
		res, err := st.remember(rememberInput{content: c})
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res["id"].(string)
		if id == "" {
			t.Fatalf("remember(%q) did not return an id: %+v", c, res)
		}
		ids = append(ids, id)
	}

	// Rewrite created_at so wall-clock order contradicts insertion order:
	// first+second tie at the SAME timestamp, third regresses BEHIND them,
	// and fourth (last inserted, highest rowid) is stamped with the OLDEST
	// timestamp of all — the worst case for a created_at-ordered query.
	stamps := map[string]string{
		ids[0]: "2024-01-05T00:00:00Z", // first: tied with second
		ids[1]: "2024-01-05T00:00:00Z", // second: tied with first
		ids[2]: "2024-01-01T00:00:00Z", // third: regressed behind first/second
		ids[3]: "2023-01-01T00:00:00Z", // fourth: last inserted, oldest stamp
	}
	for id, ts := range stamps {
		if _, err := st.db.Exec("UPDATE memories SET created_at = ? WHERE id = ?", ts, id); err != nil {
			t.Fatalf("failed to rewrite created_at for %s: %v", id, err)
		}
	}

	hits, err := st.recall("*", 0, 0, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 4 {
		t.Fatalf("recall(\"*\") returned %d hits, want 4: %+v", len(hits), hits)
	}

	// True insertion order, newest first: fourth, third, second, first —
	// the OPPOSITE of what created_at-ordering would produce (which would put
	// the tied first/second ahead of the older-stamped fourth).
	want := []string{"fourth fact", "third fact", "second fact", "first fact"}
	got := make([]string, len(hits))
	for i, h := range hits {
		got[i] = h.content
	}
	if got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
		t.Fatalf("recall(\"*\") order = %v, want %v (rowid/insertion order, not created_at)", got, want)
	}
	// createdAt is still returned in the output for display, just not used
	// as the sort key.
	for i, h := range hits {
		if h.createdAt == "" {
			t.Errorf("hit %d missing createdAt in output: %+v", i, h)
		}
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
	srv := httptest.NewServer(newMemoryMux(store))
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
