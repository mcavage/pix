// memory_live_embed_test.go — U4-live-embed-recall: the store's embedder is
// always the live, self-retrying memEmbed, attached with NO synchronous probe
// at construction. These tests prove: (1) construction never calls Ollama at
// all, checked functionally (did the handler fire?), with a generous
// wall-clock backstop only to catch an outright hang — not as the primary
// assertion, since timing alone is flaky under load; and (2) a real embed
// failure followed by Ollama recovering restores semantic recall on the SAME
// store, with no restart and no new adapter/store built in between.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// constructionBackstop is a generous ceiling for "construction must not wait
// on the network": long enough that a loaded CI box never trips it by
// accident, short enough to still fail a genuine synchronous-probe
// regression (which would block for the full embed/watcher timeout, tens of
// seconds) well before a test-suite-level timeout does.
const constructionBackstop = 5 * time.Second

// TestStoreConstruction_NeverCallsOllama proves the root fix directly and
// functionally: with the old boot-time memEmbedderAvailable() probe,
// buildMemStore/newMemStore made a synchronous /api/embed round-trip before
// returning. Here Ollama is wedged (accepts the connection, never answers);
// the PRIMARY assertion is that the handler was never hit at all — embedding
// only happens later, lazily, on an actual remember/recall — with the wall
// clock only as a backstop against an outright hang.
//
// Consolidates what used to be two separate tests (one for buildMemStore, one
// for newMemStore) that asserted the same property two ways; both
// constructors are exercised here instead.
func TestStoreConstruction_NeverCallsOllama(t *testing.T) {
	resetEmbedState()
	t.Cleanup(resetEmbedState)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		<-r.Context().Done() // hang until the client (or test) gives up
	}))
	defer srv.Close()
	t.Setenv("OLLAMA_HOST", srv.URL)
	t.Setenv("MEMORY_DB", ":memory:")
	t.Setenv("MEMORY_EMBED_TIMEOUT_MS", "5000") // long enough that waiting on it would fail this test

	before := time.Now()
	store, err := buildMemStore()
	if err != nil {
		t.Fatal(err)
	}
	if store == nil {
		t.Fatal("expected a non-nil store")
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("buildMemStore hit the embed endpoint %d time(s); construction must never call Ollama", got)
	}
	if elapsed := time.Since(before); elapsed > constructionBackstop {
		t.Fatalf("buildMemStore took %v with a wedged Ollama; construction must never wait on the embedder", elapsed)
	}

	// Same property one level down: newMemStore itself, handed the live
	// memEmbed function directly.
	before = time.Now()
	if _, err := newMemStore(":memory:", memEmbed); err != nil {
		t.Fatal(err)
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("newMemStore hit the embed endpoint %d time(s); construction must never call Ollama", got)
	}
	if elapsed := time.Since(before); elapsed > constructionBackstop {
		t.Fatalf("newMemStore(live embedder) took %v; must not call Ollama at construction", elapsed)
	}
}

// TestEmbedFailureThenRecoveryWithoutStoreRestart proves recall recovers on
// the SAME store/adapter after a real Ollama outage clears: no new store, no
// new adapter, just the live embedder re-succeeding.
func TestEmbedFailureThenRecoveryWithoutStoreRestart(t *testing.T) {
	resetEmbedState()
	t.Cleanup(resetEmbedState)

	var available atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !available.Load() {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float64{{1, 0, 0}}})
	}))
	defer srv.Close()
	t.Setenv("OLLAMA_HOST", srv.URL)

	// One store, built once, used across the whole outage-and-recovery.
	st, err := newMemStore(":memory:", memEmbed)
	if err != nil {
		t.Fatal(err)
	}

	// Ollama down: remember still succeeds, but stores no embedding, and the
	// failure latches embedDisabled (and marks the embedder exercised).
	if _, err := st.remember(rememberInput{content: "alpha topic content"}); err != nil {
		t.Fatal(err)
	}
	if !embedDisabled.Load() {
		t.Fatal("expected embedDisabled to latch true after a real embed failure")
	}
	if hv := embedHealthState(); hv == nil || *hv {
		t.Fatalf("expected embedHealthState to report degraded (false) after a real failure, got %v", hv)
	}
	var embCountDuringOutage int
	st.db.QueryRow("SELECT count(*) FROM memories WHERE embedding IS NOT NULL").Scan(&embCountDuringOutage)
	if embCountDuringOutage != 0 {
		t.Fatalf("expected 0 rows with a stored embedding during the outage, got %d", embCountDuringOutage)
	}

	// Ollama recovers. Force the re-probe throttle open (mirrors the watcher's
	// own recovery tests) so the very next embed call re-probes instead of
	// waiting out embedProbeInterval.
	available.Store(true)
	embedLastProbe.Store(0)

	// SAME store, no restart, no new adapter: a second remember must now embed
	// successfully and clear the latch.
	if _, err := st.remember(rememberInput{content: "beta topic content"}); err != nil {
		t.Fatal(err)
	}
	if embedDisabled.Load() {
		t.Fatal("expected embedDisabled to clear after a successful embed call, same store, no restart")
	}
	if hv := embedHealthState(); hv == nil || !*hv {
		t.Fatalf("expected embedHealthState to report healthy (true) after recovery, got %v", hv)
	}
	var embCountAfterRecovery int
	st.db.QueryRow("SELECT count(*) FROM memories WHERE embedding IS NOT NULL").Scan(&embCountAfterRecovery)
	if embCountAfterRecovery != 1 {
		t.Fatalf("expected exactly 1 row with a stored embedding (the post-recovery one), got %d", embCountAfterRecovery)
	}
}
