// memory_recall_timestamp_test.go — FIX 3: every recall hit carries the
// row's created_at timestamp (RFC3339, via memNowIso), all the way out to the
// JSON-RPC response, so `pix memory recall` can print it.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// scoredHit.createdAt is populated straight from the stored row.
func TestMemStoreRecall_HitCarriesCreatedAt(t *testing.T) {
	st, err := newMemStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(-time.Second)
	if _, err := st.remember(rememberInput{content: "the deploy runbook lives in docs/deploy.md"}); err != nil {
		t.Fatal(err)
	}
	hits, err := st.recall("deploy runbook", 8, 1200, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("recall found nothing")
	}
	if hits[0].createdAt == "" {
		t.Fatal("scoredHit.createdAt must not be empty")
	}
	ts, err := time.Parse(time.RFC3339Nano, hits[0].createdAt)
	if err != nil {
		ts, err = time.Parse(time.RFC3339, hits[0].createdAt)
	}
	if err != nil {
		t.Fatalf("createdAt %q does not parse as RFC3339: %v", hits[0].createdAt, err)
	}
	if ts.Before(before) || ts.After(time.Now().Add(time.Second)) {
		t.Errorf("createdAt %v not within the expected window around now", ts)
	}
}

// The JSON-RPC "recall" method surfaces createdAt on every hit.
func TestMemoryMux_RecallHitsIncludeCreatedAt(t *testing.T) {
	store, err := newMemStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.remember(rememberInput{content: "prod incidents page #ops-oncall"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newMemoryMux(store, false))
	defer srv.Close()

	body := `{"jsonrpc":"2.0","id":1,"method":"recall","params":{"query":"incidents page"}}`
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
	if len(parsed.Result.Hits) == 0 {
		t.Fatal("recall RPC returned no hits")
	}
	ca, ok := parsed.Result.Hits[0]["createdAt"].(string)
	if !ok || strings.TrimSpace(ca) == "" {
		t.Fatalf("recall RPC hit missing a non-empty createdAt: %+v", parsed.Result.Hits[0])
	}
	if _, err := time.Parse(time.RFC3339Nano, ca); err != nil {
		t.Errorf("createdAt %q from the RPC does not parse as RFC3339: %v", ca, err)
	}
}
