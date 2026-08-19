package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
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
// TestWatcherBudgetQueryUsesSourceCreatedAtIndex proves REV2-1's whole point:
// watcherUsedToday's COUNT is a plain indexable range (created_at >= ? AND
// created_at < ?) against source='watcher', not the old
// substr(created_at,1,10)=? equality that forced sqlite to evaluate a
// function against every row (no index can serve a function-of-column
// predicate). EXPLAIN QUERY PLAN is the proof: sqlite's planner reports a
// SEARCH using idx_memories_source_created_at, never a SCAN of the whole
// table.
//
// It EXPLAINs memWatcherUsedTodaySQL, the same production const
// watcherUsedToday executes, so the plan proven here is the plan that runs:
// a hand-copied query string could drift out of the index's reach while this
// test went on explaining the old one.
func TestWatcherBudgetQueryUsesSourceCreatedAtIndex(t *testing.T) {
	st, err := newMemStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	start, end := watcherDayBoundsUTC(time.Now())
	rows, err := st.db.Query("EXPLAIN QUERY PLAN "+memWatcherUsedTodaySQL, start, end)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "USING COVERING INDEX idx_memories_source_created_at") && !strings.Contains(plan, "USING INDEX idx_memories_source_created_at") {
		t.Fatalf("watcherUsedToday's query plan does not use idx_memories_source_created_at, got:\n%s", plan)
	}
	if strings.Contains(plan, "SCAN memories") {
		t.Fatalf("watcherUsedToday's query plan falls back to a full table SCAN, got:\n%s", plan)
	}
}

// TestWatcherDayBoundsUTC_HalfOpenRangeAtExactMidnight proves the UTC
// boundary math is correct at the exact instant it is most fragile: a row
// timestamped precisely at day-start (whole second, no fractional digits,
// as memNowIso's RFC3339Nano formatter writes it) must land in that day's
// count, and a row one nanosecond into the NEXT day must not — the classic
// off-by-one a half-open range gets wrong when boundaries are computed
// carelessly. It also proves a row a fraction of a second after midnight
// (variable-length trailing digits, per watcherDayBoundsUTC's doc comment)
// still compares as inside the day, not before it.
func TestWatcherDayBoundsUTC_HalfOpenRangeAtExactMidnight(t *testing.T) {
	st, err := newMemStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	day := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
	insert := func(createdAt string) {
		if _, err := st.rememberWatcherCapture(rememberInput{content: "fact @ " + createdAt, kind: "fact"}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.db.Exec("UPDATE memories SET created_at = ? WHERE content = ?", createdAt, "fact @ "+createdAt); err != nil {
			t.Fatal(err)
		}
	}
	// Exactly at day-start, whole second (no fractional component at all,
	// the shape memNowIso() writes for an exact-second timestamp): must count.
	insert(day.Format(time.RFC3339Nano))
	// A fraction of a second after day-start: must still count as this day,
	// not sort before the (Z-less) lower bound.
	insert(day.Add(1 * time.Millisecond).Format(time.RFC3339Nano))
	// The last nanosecond of THIS day: must count.
	insert(day.Add(24*time.Hour - time.Nanosecond).Format(time.RFC3339Nano))
	// One nanosecond into the NEXT day: must NOT count.
	insert(day.Add(24 * time.Hour).Format(time.RFC3339Nano))
	// The last instant of the PREVIOUS day: must NOT count.
	insert(day.Add(-time.Nanosecond).Format(time.RFC3339Nano))

	today := func() time.Time { return day }
	start, end := watcherDayBoundsUTC(today())
	var used int
	if err := st.db.QueryRow(
		"SELECT COUNT(*) FROM memories WHERE source = 'watcher' AND created_at >= ? AND created_at < ?",
		start, end,
	).Scan(&used); err != nil {
		t.Fatal(err)
	}
	if used != 3 {
		t.Fatalf("watcher rows counted for the day = %d, want 3 (day-start exact, day-start+1ms, and day-end-1ns; excluding the two rows that fall on adjacent days)", used)
	}
}

// TestSourceCreatedAtIndex_BackfillsOnAnExistingV2DatabaseNoNewMigration
// proves the index reaches a database that was already at memSchemaVersion
// BEFORE this fix shipped (so migrateMemorySchema's one-time sweep, gated on
// user_version, never runs again for it) without bumping the schema version:
// CREATE INDEX IF NOT EXISTS lives in memSchema itself, which newMemStore
// executes unconditionally on every open, the same way the CREATE TABLE
// statement beside it already reaches every db regardless of version.
func TestSourceCreatedAtIndex_BackfillsOnAnExistingV2DatabaseNoNewMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing-v2.db")
	// Hand-build a v2 database shaped exactly like memSchema minus the index
	// this fix adds — i.e. what every real db on disk looks like today.
	preFixSchema := `
CREATE TABLE memories (
  rowid INTEGER PRIMARY KEY, id TEXT UNIQUE NOT NULL, kind TEXT NOT NULL, content TEXT NOT NULL,
  content_hash TEXT NOT NULL, durability TEXT NOT NULL, confidence REAL NOT NULL,
  frequency INTEGER NOT NULL DEFAULT 1, reward REAL NOT NULL DEFAULT 0, access_count INTEGER NOT NULL DEFAULT 0,
  created_at TEXT NOT NULL, last_accessed TEXT, expires_at TEXT, source TEXT NOT NULL,
  tags TEXT NOT NULL DEFAULT '[]', project TEXT, embedding TEXT, deleted_at TEXT, profile TEXT
);
CREATE VIRTUAL TABLE memories_fts USING fts5(content);`
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(preFixSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = " + strconv.Itoa(memSchemaVersion)); err != nil {
		t.Fatal(err)
	}
	db.Close()

	indexExists := func() bool {
		check, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer check.Close()
		var name string
		err = check.QueryRow("SELECT name FROM sqlite_master WHERE type='index' AND name='idx_memories_source_created_at'").Scan(&name)
		if err == sql.ErrNoRows {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		return true
	}
	if indexExists() {
		t.Fatal("precondition: the hand-built pre-fix fixture already has the index")
	}

	st, err := newMemStore(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	var uv int
	if err := st.db.QueryRow("PRAGMA user_version").Scan(&uv); err != nil {
		t.Fatal(err)
	}
	if uv != memSchemaVersion {
		t.Fatalf("user_version = %d after opening an already-v2 db, want unchanged at %d (no new migration version)", uv, memSchemaVersion)
	}
	st.db.Close()

	if !indexExists() {
		t.Fatal("idx_memories_source_created_at was not backfilled onto the existing v2 database")
	}

	// Reopening again (the index now present) must be a no-op, not an error.
	st2, err := newMemStore(path, nil)
	if err != nil {
		t.Fatalf("reopen with the index already present: %v", err)
	}
	st2.db.Close()
}

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
