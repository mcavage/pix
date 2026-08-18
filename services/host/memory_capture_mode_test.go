package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"pix/host/plugin"
)

// TestMemCaptureModeDefaultsExplicit: no/garbled/deleted-"review" env all
// fail closed to explicit; only the exact opt-in value resolves otherwise.
func TestMemCaptureModeDefaultsExplicit(t *testing.T) {
	for _, v := range []string{"", "bogus", "review", "Experimental-Auto"} {
		t.Setenv("MEMORY_CAPTURE_MODE", v)
		if got := memCaptureMode(); got != "explicit" {
			t.Errorf("memCaptureMode() with env %q = %q, want explicit", v, got)
		}
	}
	t.Setenv("MEMORY_CAPTURE_MODE", "experimental-auto")
	if got := memCaptureMode(); got != "experimental-auto" {
		t.Errorf("memCaptureMode() = %q, want experimental-auto", got)
	}
}

// TestMemObserveExplicitDefaultNeverTouchesWatcher: no side-effect probe.
func TestMemObserveExplicitDefaultNeverTouchesWatcher(t *testing.T) {
	resetWatcherState()
	t.Cleanup(resetWatcherState)
	st, err := newMemStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	accepted, reason := memObserve(st, "remember that I like tabs not spaces", "", false, "default")
	if accepted || reason == "" {
		t.Fatalf("explicit mode must refuse with a reason, got accepted=%v reason=%q", accepted, reason)
	}
	if watcherExercised.Load() {
		t.Error("explicit mode must never exercise the watcher, not even an availability probe")
	}
}

// TestExperimentalAutoWritesWithSourceAndCannotBeSpoofed: auto write lands
// with source="watcher"; an external Remember() claiming that source cannot.
func TestExperimentalAutoWritesWithSourceAndCannotBeSpoofed(t *testing.T) {
	st := watchServer(t, `{"facts":["the user prefers dark mode"],"corrections":[]}`)
	t.Setenv("MEMORY_CAPTURE_MODE", "experimental-auto")
	memCaptureSem <- struct{}{}
	memCapture(st, "an assertion-bearing message, not a question.", "", false, "default")

	hits, err := st.recall("*", 100, 1000000, "", "", "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].content != "the user prefers dark mode" || hits[0].source != "watcher" {
		t.Fatalf("expected one watcher-sourced fact in memories, got %+v", hits)
	}

	for _, mode := range []string{"", "experimental-auto"} {
		t.Setenv("MEMORY_CAPTURE_MODE", mode)
		a := newMemoryStoreAdapter(st)
		r, err := a.Remember(plugin.RememberReq{Content: "spoof attempt " + mode, Source: "watcher"})
		if err != nil || r.ID == "" {
			t.Fatalf("mode %q: remember should still store, got id=%q err=%v", mode, r.ID, err)
		}
		var source string
		if err := st.db.QueryRow("SELECT source FROM memories WHERE id = ?", r.ID).Scan(&source); err != nil {
			t.Fatal(err)
		}
		if source == "watcher" {
			t.Fatalf("mode %q: external remember spoofed source=watcher", mode)
		}
	}
}

// TestWatcherBudget: counts stored rows only, rolls over at UTC midnight,
// never negative, gates memObserve before the watcher is exercised.
func TestWatcherBudget(t *testing.T) {
	st, err := newMemStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	if remaining, _ := st.watcherBudgetRemaining(); remaining != memWatcherDailyBudget {
		t.Fatalf("fresh store remaining = %d, want %d", remaining, memWatcherDailyBudget)
	}
	if _, err := st.rememberWatcherCapture(rememberInput{content: "fact one", kind: "fact"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.remember(rememberInput{content: "an explicit fact", source: "user"}); err != nil {
		t.Fatal(err)
	}
	if remaining, _ := st.watcherBudgetRemaining(); remaining != memWatcherDailyBudget-1 {
		t.Fatalf("remaining after 1 watcher row + 1 explicit remember = %d, want %d (explicit must not consume budget)", remaining, memWatcherDailyBudget-1)
	}

	yesterday := time.Now().UTC().Add(-25 * time.Hour).Format(time.RFC3339Nano)
	if _, err := st.db.Exec("UPDATE memories SET created_at = ? WHERE source = 'watcher'", yesterday); err != nil {
		t.Fatal(err)
	}
	if remaining, _ := st.watcherBudgetRemaining(); remaining != memWatcherDailyBudget {
		t.Fatalf("remaining after backdating the watcher row to yesterday = %d, want the FULL budget %d", remaining, memWatcherDailyBudget)
	}

	for i := 0; i < memWatcherDailyBudget+5; i++ {
		if _, err := st.rememberWatcherCapture(rememberInput{content: fmt.Sprintf("fact %d", i), kind: "fact"}); err != nil {
			t.Fatal(err)
		}
	}
	if remaining, _ := st.watcherBudgetRemaining(); remaining != 0 {
		t.Fatalf("remaining after exceeding the budget = %d, want 0 (never negative)", remaining)
	}

	resetWatcherState()
	t.Cleanup(resetWatcherState)
	t.Setenv("MEMORY_CAPTURE_MODE", "experimental-auto")
	accepted, reason := memObserve(st, "remember I like Go", "", false, "default")
	if accepted || reason == "" {
		t.Fatal("exhausted budget must return accepted:false with a reason")
	}
	if watcherExercised.Load() {
		t.Error("exhausted budget must be refused before the watcher is ever exercised")
	}
}

// TestWatcherBudgetCountsSoftDeletedRows proves a forgotten (soft-deleted)
// watcher row still counts against today's budget: the row still STORED
// today and still cost a real watcher-model call, and forget is feedback on
// recall content, not a refund on capture volume. Only a new UTC day resets
// the count (see the boundary case above), never a delete.
func TestWatcherBudgetCountsSoftDeletedRows(t *testing.T) {
	st, err := newMemStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := st.rememberWatcherCapture(rememberInput{content: "a forgettable watcher fact", kind: "fact"})
	if err != nil {
		t.Fatal(err)
	}
	id := res["id"].(string)
	if remaining, _ := st.watcherBudgetRemaining(); remaining != memWatcherDailyBudget-1 {
		t.Fatalf("remaining after 1 stored watcher row = %d, want %d", remaining, memWatcherDailyBudget-1)
	}
	if !st.forget(id, "default") {
		t.Fatal("forget should succeed on the row just stored")
	}
	var deletedAt sql.NullString
	if err := st.db.QueryRow("SELECT deleted_at FROM memories WHERE id = ?", id).Scan(&deletedAt); err != nil {
		t.Fatal(err)
	}
	if !deletedAt.Valid || deletedAt.String == "" {
		t.Fatal("forget should soft-delete (stamp deleted_at), not purge the row")
	}
	if remaining, _ := st.watcherBudgetRemaining(); remaining != memWatcherDailyBudget-1 {
		t.Fatalf("remaining after forgetting the watcher row = %d, want still %d (soft delete is not a budget refund)", remaining, memWatcherDailyBudget-1)
	}
}

// TestMemCaptureBudgetOnlyCountsNewRows proves the fix: a batch that includes
// a content-hash REAFFIRM of an already-stored watcher fact must not consume
// budget for that item -- only the genuinely NEW row counts. Before the fix,
// memCapture's local `stored` tally incremented on every attempted call
// regardless of outcome, so a reaffirm could stop a batch from storing a
// later, genuinely new item that the actual remaining budget still allowed.
func TestMemCaptureBudgetOnlyCountsNewRows(t *testing.T) {
	st := watchServer(t, `{"facts":["existing dup","brand new fact"],"corrections":[]}`)
	if _, err := st.rememberWatcherCapture(rememberInput{content: "existing dup", kind: "fact"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEMORY_CAPTURE_MODE", "experimental-auto")
	memCaptureSem <- struct{}{}
	memCapture(st, "an assertion-bearing message, not a question.", "", false, "default")

	// One pre-existing row + one genuinely new row = 2 rows stored, never 3
	// (the reaffirmed "existing dup" hit must not count a second time).
	if remaining, err := st.watcherBudgetRemaining(); err != nil {
		t.Fatal(err)
	} else if remaining != memWatcherDailyBudget-2 {
		t.Fatalf("remaining = %d, want %d (reaffirm must not double-count against the budget)", remaining, memWatcherDailyBudget-2)
	}
}

// TestMemCaptureBudgetSemanticDedupeNotCounted is the vector-similarity twin
// of the content-hash case above: a watcher fact that collapses into an
// existing row via findSimilar (different text, same meaning) is still a
// reaffirm, not a new row, and must not consume budget either.
func TestMemCaptureBudgetSemanticDedupeNotCounted(t *testing.T) {
	resetWatcherState()
	t.Cleanup(resetWatcherState)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := json.Marshal(map[string]any{"message": map[string]any{"content": `{"facts":["alpha two"],"corrections":[]}`}})
		w.WriteHeader(200)
		w.Write(b)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OLLAMA_HOST", srv.URL)
	t.Setenv("MEMORY_WATCHER_MODEL", "test-watcher")
	st, err := newMemStore(":memory:", fakeKeywordEmbedder(16))
	if err != nil {
		t.Fatal(err)
	}
	// "alpha *" all share a first token -> cosine 1.0 between them, so "alpha
	// two" collapses into this pre-existing row via findSimilar, not a hash match.
	if _, err := st.rememberWatcherCapture(rememberInput{content: "alpha one", kind: "fact"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEMORY_CAPTURE_MODE", "experimental-auto")
	memCaptureSem <- struct{}{}
	memCapture(st, "an assertion-bearing message, not a question.", "", false, "default")

	if remaining, err := st.watcherBudgetRemaining(); err != nil {
		t.Fatal(err)
	} else if remaining != memWatcherDailyBudget-1 {
		t.Fatalf("remaining = %d, want %d (semantic dedupe collapse must not consume budget)", remaining, memWatcherDailyBudget-1)
	}
	if bucketActive(t, st, "default") != 1 {
		t.Fatalf("expected the semantic duplicate to collapse into 1 active row, got %d", bucketActive(t, st, "default"))
	}
}

// TestMemCaptureBudgetFailedRememberNotCounted proves a store-level failure
// (simulated with a trigger that aborts one specific insert, standing in for
// a real write failure) does not consume budget either -- only what actually
// landed a new row counts.
func TestMemCaptureBudgetFailedRememberNotCounted(t *testing.T) {
	st := watchServer(t, `{"facts":["force-fail-trigger","a fact that should land"],"corrections":[]}`)
	if _, err := st.db.Exec(`CREATE TRIGGER force_fail BEFORE INSERT ON memories
		WHEN NEW.content = 'force-fail-trigger' BEGIN SELECT RAISE(ABORT, 'forced test failure'); END;`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEMORY_CAPTURE_MODE", "experimental-auto")
	memCaptureSem <- struct{}{}
	memCapture(st, "an assertion-bearing message, not a question.", "", false, "default")

	if remaining, err := st.watcherBudgetRemaining(); err != nil {
		t.Fatal(err)
	} else if remaining != memWatcherDailyBudget-1 {
		t.Fatalf("remaining = %d, want %d (a failed remember must not consume budget)", remaining, memWatcherDailyBudget-1)
	}
	hits, err := st.recall("*", 100, 1000000, "", "", "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].content != "a fact that should land" {
		t.Fatalf("expected only the successful fact to land, got %+v", hits)
	}
}

// TestWatcherBudgetExactUnderConcurrentCaptures is the SEC-2 regression: the
// daily 10-stored-rows/day cap must hold EXACTLY even when many captures
// race to store a new row at once (memCaptureMaxConcurrency allows up to 8
// concurrent memCapture goroutines in production). Before the fix, each
// concurrent caller peeked its own watcherBudgetRemaining() snapshot and
// then stored up to that stale count independently, so N concurrent callers
// could collectively store far more than memWatcherDailyBudget rows. Firing
// many more attempts than the budget allows and asserting the stored count
// is EXACTLY the budget (never more, and — since every attempt is a
// distinct, never-reaffirmed row — never less either) proves the fix
// without depending on any sleep/timing: every goroutine is launched before
// any is allowed to proceed (an ordinary WaitGroup fan-out), so the result is
// deterministic regardless of scheduling order.
func TestWatcherBudgetExactUnderConcurrentCaptures(t *testing.T) {
	st, err := newMemStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}

	const attempts = 50 // far more than memWatcherDailyBudget, on purpose
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	wg.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func(i int) {
			defer wg.Done()
			// Distinct content per attempt: a hash/semantic reaffirm would
			// legitimately not consume budget, which would defeat this test's
			// point (proving the CAP holds under concurrency, not the
			// already-covered dedupe behavior).
			_, err := st.rememberWatcherCapture(rememberInput{content: fmt.Sprintf("concurrent fact %d", i), kind: "fact"})
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		t.Fatal(err)
	}

	var stored int
	if err := st.db.QueryRow("SELECT COUNT(*) FROM memories WHERE source = 'watcher'").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != memWatcherDailyBudget {
		t.Fatalf("stored %d watcher rows under %d concurrent attempts, want exactly %d", stored, attempts, memWatcherDailyBudget)
	}
	if remaining, err := st.watcherBudgetRemaining(); err != nil {
		t.Fatal(err)
	} else if remaining != 0 {
		t.Fatalf("remaining = %d after exhausting the budget concurrently, want 0", remaining)
	}
}

// drainMemCaptureSem blocks until every memCapture goroutine currently
// holding a memCaptureSem slot has returned, with no sleep/timing dependency.
// memCaptureSem is a buffered channel of capacity memCaptureMaxConcurrency;
// filling it to capacity can only succeed once every in-flight holder's
// `defer func() { <-memCaptureSem }()` has run (that defer is the LAST thing
// memCapture does), so the fill is the synchronization point. Draining it
// back to empty afterward leaves the semaphore clean for the next test.
// Requires the caller hold no slot of its own and that no other capture is
// concurrently in flight (true for this package's sequential, non-parallel
// tests).
func drainMemCaptureSem(t *testing.T) {
	t.Helper()
	for i := 0; i < memCaptureMaxConcurrency; i++ {
		memCaptureSem <- struct{}{}
	}
	for i := 0; i < memCaptureMaxConcurrency; i++ {
		<-memCaptureSem
	}
}

// TestMemCaptureSecretFilterRunsOnUntruncatedText is the SEC-4 regression.
// It goes through memObserve, the real admission path (not memCapture
// directly): the pre-fix bug was memObserve's OWN truncation, applied to the
// message it then handed to `go memCapture(...)`, so a regression test that
// calls memCapture directly never exercises the line that broke — it would
// pass against the pre-fix code just as well as the fixed code, because
// memCapture itself never truncated before the stage-1 filter. Only a test
// that drives the real memObserve -> memCapture handoff can tell the two
// apart. A secret placed past the truncation cutoff (8000 chars) would be
// invisible to a filter that ran on already-truncated text — a fail-open
// gap. watchServer's fixed watcher response would surface a fact if the
// watcher were ever invoked, so asserting zero stored hits proves the filter
// blocked capture before that point, not merely that the watcher happened to
// extract nothing. memObserve spawns memCapture in its own goroutine, so
// drainMemCaptureSem stands in for the sleep a timing-dependent test would
// otherwise need.
func TestMemCaptureSecretFilterRunsOnUntruncatedText(t *testing.T) {
	st := watchServer(t, `{"facts":["should never be seen"],"corrections":[]}`)
	t.Setenv("MEMORY_CAPTURE_MODE", "experimental-auto")

	padding := ""
	for len(padding) <= 8000 {
		padding += "harmless filler text, nothing secret here. "
	}
	user := padding + "here is my key AKIAABCDEFGHIJKLMNOP"
	if len(user) <= 8000 {
		t.Fatalf("test input too short: %d chars, want > 8000 so the secret sits past the truncation cutoff", len(user))
	}

	accepted, reason := memObserve(st, user, "", false, "default")
	if !accepted {
		t.Fatalf("memObserve should admit (the secret filter runs inside memCapture, after admission, not at the observe gate), got reason=%q", reason)
	}
	drainMemCaptureSem(t)

	hits, err := st.recall("*", 100, 1000000, "", "", "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("a secret sitting past the 8000-char truncation point must still block capture, got %+v", hits)
	}
}
