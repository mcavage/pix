package main

import (
	"strings"
	"testing"
	"time"

	"pix/host/plugin"
)

// Compile-time guarantee the adapter satisfies the plugin interface.
var _ plugin.MemoryStore = (*memoryStoreAdapter)(nil)

// newTestAdapter builds an adapter over an in-memory, FTS-only store (nil
// embedder -> no Ollama needed), mirroring host_test.go's setup.
func newTestAdapter(t *testing.T) *memoryStoreAdapter {
	t.Helper()
	st, err := newMemStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	return newMemoryStoreAdapter(st)
}

// resetEmbedState clears the package-level embed atomics between tests, the
// same rationale as memembed_test.go's resetWatcherState: these are shared
// globals, not per-test fixtures.
func resetEmbedState() {
	embedDisabled.Store(false)
	embedExercised.Store(false)
	embedLastProbe.Store(0)
}

func TestAdapterStatsHealth(t *testing.T) {
	resetEmbedState()
	resetWatcherState()
	watcherExercised.Store(false)
	t.Cleanup(func() {
		resetEmbedState()
		resetWatcherState()
		watcherExercised.Store(false)
	})
	a := newTestAdapter(t)

	// empty store
	s, err := a.Stats("")
	if err != nil {
		t.Fatal(err)
	}
	if s.Active != 0 || s.Facts != 0 || s.Deleted != 0 {
		t.Fatalf("empty store stats not zero: %+v", s)
	}

	// A fresh, never-exercised adapter must report UNKNOWN for both embed and
	// capture health, never "healthy" — nothing has actually confirmed either
	// works yet (buildMemStore/newMemStore make no boot-time probe).
	h, err := a.Health()
	if err != nil {
		t.Fatal(err)
	}
	if !h.OK {
		t.Error("health.OK should be true")
	}
	if h.Vector != nil {
		t.Errorf("health.Vector should be nil (unknown) before any real embed attempt, got %v", *h.Vector)
	}
	if h.Capture != nil {
		t.Errorf("health.Capture should be nil (unknown) before any real capture attempt, got %v", *h.Capture)
	}
	if h.WatcherModel != memWatcherModel() {
		t.Errorf("health.WatcherModel = %q, want %q", h.WatcherModel, memWatcherModel())
	}

	// Exercise embed: a real (simulated) success flips unknown -> healthy, then
	// a real failure flips healthy -> degraded, live, on the SAME adapter.
	embedExercised.Store(true)
	embedDisabled.Store(false)
	if hv, _ := a.Health(); hv.Vector == nil || !*hv.Vector {
		t.Errorf("health.Vector should be true once exercised and not disabled, got %v", hv.Vector)
	}
	embedDisabled.Store(true)
	if hv, _ := a.Health(); hv.Vector == nil || *hv.Vector {
		t.Errorf("health.Vector should be false once exercised and disabled, got %v", hv.Vector)
	}

	// Exercise capture the same way: unknown -> degraded -> healthy recovery.
	watcherUnavailable.Store(true)
	watcherExercised.Store(true)
	watcherDegradedUntil.Store(0)
	watcherLastProbe.Store(time.Now().UnixNano()) // hold the reprobe throttle open, no network call
	if hc, _ := a.Health(); hc.Capture == nil || *hc.Capture {
		t.Errorf("health.Capture should be false once exercised and unavailable, got %v", hc.Capture)
	}
	watcherHealthy() // simulates the real recovery memWatch() success performs
	if hc, _ := a.Health(); hc.Capture == nil || !*hc.Capture {
		t.Errorf("health.Capture should be true after a real recovery, got %v", hc.Capture)
	}
}

func TestAdapterRememberRecallForget(t *testing.T) {
	a := newTestAdapter(t)

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
	if s, _ := a.Stats(""); s.Active != 1 {
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
	if s, _ := a.Stats(""); s.Active != 0 {
		t.Fatalf("expected 0 active after forget, got %d", s.Active)
	}
}

func TestAdapterObserveEmpty(t *testing.T) {
	a := newTestAdapter(t)
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
