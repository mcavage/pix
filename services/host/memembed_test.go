package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestWatcherCaptureAvailableBreaksLatch proves the fix for the silent-capture
// bug: the startup probe used to latch watcherUnavailable=true forever (observe
// short-circuited, so the watcher never ran to reset it), meaning capture stayed
// dead until a daemon restart even after the user pulled the model. The throttled
// live re-probe must recover without a restart.
func TestWatcherCaptureAvailableBreaksLatch(t *testing.T) {
	var present bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if present {
			w.WriteHeader(200)
		} else {
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	t.Setenv("OLLAMA_HOST", srv.URL)
	t.Setenv("MEMORY_WATCHER_MODEL", "test-watcher")

	// Latched off, model absent, throttle window open (last probe = 0).
	watcherUnavailable.Store(true)
	watcherLastProbe.Store(0)
	if watcherCaptureAvailable() {
		t.Fatal("model absent: capture should be unavailable")
	}

	// A second call inside the throttle window must NOT flip anything, even though
	// it does not re-probe.
	if watcherCaptureAvailable() {
		t.Fatal("still absent within throttle window: should stay unavailable")
	}

	// User pulls the model; force the throttle open and re-probe. Capture recovers
	// with no restart, and the latch clears.
	present = true
	watcherLastProbe.Store(0)
	if !watcherCaptureAvailable() {
		t.Fatal("model now present: capture should recover without a daemon restart")
	}
	if watcherUnavailable.Load() {
		t.Fatal("latch should be cleared after a successful re-probe")
	}

	// Available is the immediate-return common path (no probe needed).
	if !watcherCaptureAvailable() {
		t.Fatal("available watcher should stay available")
	}
}
