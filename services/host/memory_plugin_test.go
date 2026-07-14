package main

import (
	"strings"
	"testing"

	"pi-stack/host/plugin"
)

// Compile-time guarantee the adapter satisfies the plugin interface.
var _ plugin.MemoryStore = (*memoryStoreAdapter)(nil)

// newTestAdapter builds an adapter over an in-memory, FTS-only store (nil
// embedder -> no Ollama needed), mirroring host_test.go's setup.
func newTestAdapter(t *testing.T, hasVector bool) *memoryStoreAdapter {
	t.Helper()
	st, err := newMemStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	return newMemoryStoreAdapter(st, hasVector)
}

func TestAdapterStatsHealth(t *testing.T) {
	a := newTestAdapter(t, true)

	// empty store
	s, err := a.Stats()
	if err != nil {
		t.Fatal(err)
	}
	if s.Active != 0 || s.Durable != 0 || s.Deleted != 0 {
		t.Fatalf("empty store stats not zero: %+v", s)
	}

	// health reports the vector flag we constructed with, and reflects the
	// live watcher availability the same way memoryMux() does.
	h, err := a.Health()
	if err != nil {
		t.Fatal(err)
	}
	if !h.OK {
		t.Error("health.OK should be true")
	}
	if !h.Vector {
		t.Error("health.Vector should mirror hasVector=true")
	}
	if h.WatcherModel != memWatcherModel() {
		t.Errorf("health.WatcherModel = %q, want %q", h.WatcherModel, memWatcherModel())
	}
	if h.Capture == watcherUnavailable.Load() {
		t.Error("health.Capture should be the negation of watcherUnavailable")
	}

	// hasVector=false path
	if hv, _ := newTestAdapter(t, false).Health(); hv.Vector {
		t.Error("health.Vector should be false when constructed with hasVector=false")
	}
}

func TestAdapterRememberRecallForget(t *testing.T) {
	a := newTestAdapter(t, false)

	const fact = "The user prefers Go for host services and TypeScript in the sandbox."
	r, err := a.Remember(plugin.RememberReq{Content: fact})
	if err != nil {
		t.Fatal(err)
	}
	if r.Reaffirmed {
		t.Fatal("first remember should not be a reaffirm")
	}
	if r.ID == "" {
		t.Fatal("remember should return an id")
	}

	rc, err := a.Recall(plugin.RecallReq{Query: "what language for host services", Limit: 8, CharBudget: 1200})
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Hits) == 0 || !strings.Contains(rc.Hits[0].Content, "Go for host services") {
		t.Fatalf("recall did not surface the fact: %+v", rc.Hits)
	}
	if rc.Hits[0].ID != r.ID {
		t.Errorf("recall hit id = %q, want %q", rc.Hits[0].ID, r.ID)
	}

	// duplicate -> reaffirmed, still 1 active
	r2, err := a.Remember(plugin.RememberReq{Content: fact})
	if err != nil {
		t.Fatal(err)
	}
	if !r2.Reaffirmed {
		t.Fatal("duplicate remember should reaffirm")
	}
	if s, _ := a.Stats(); s.Active != 1 {
		t.Fatalf("expected 1 active memory, got %d", s.Active)
	}

	// forget by id prefix
	f, err := a.Forget(plugin.ForgetReq{ID: r.ID[:8]})
	if err != nil {
		t.Fatal(err)
	}
	if !f.OK {
		t.Fatal("forget by 8-char prefix should succeed")
	}
	if s, _ := a.Stats(); s.Active != 0 {
		t.Fatalf("expected 0 active after forget, got %d", s.Active)
	}
}

func TestAdapterObserveEmpty(t *testing.T) {
	a := newTestAdapter(t, false)
	// Empty user is rejected without touching the watcher — deterministic and
	// Ollama-free.
	resp, err := a.Observe(plugin.ObserveReq{User: "   "})
	if err != nil {
		t.Fatal(err)
	}
	if resp.Accepted {
		t.Error("observe with empty user should not be accepted")
	}
}
