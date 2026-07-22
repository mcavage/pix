package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// resetWatcherState clears the package-level watcher state between tests so
// one test's failure/backoff can't bleed into the next (these are shared
// atomics, not per-test fixtures).
func resetWatcherState() {
	watcherUnavailable.Store(false)
	setWatcherReason("")
	watcherDegradedUntil.Store(0)
	watcherLastProbe.Store(0)
}

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

// TestMemWatchTimeout proves the root-cause fix: memWatch used to call
// http.Post with NO timeout, so a wedged Ollama (accepts the connection, never
// finishes inference) hung the capture goroutine forever. With a bounded
// client, a slow /api/chat must fail fast, mark the watcher unavailable, store
// a reason that says WHY (not just the generic "model not pulled" text), and
// open a degraded-until backoff window.
func TestMemWatchTimeout(t *testing.T) {
	resetWatcherState()
	t.Cleanup(resetWatcherState)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/chat" {
			time.Sleep(300 * time.Millisecond) // >> the test's MEMORY_WATCHER_TIMEOUT_MS
			w.WriteHeader(200)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	t.Setenv("OLLAMA_HOST", srv.URL)
	t.Setenv("MEMORY_WATCHER_MODEL", "test-watcher")
	t.Setenv("MEMORY_WATCHER_TIMEOUT_MS", "50")

	before := time.Now()
	if got := memWatch("hello"); got != nil {
		t.Fatalf("expected nil result on a wedged /api/chat, got %+v", got)
	}
	if elapsed := time.Since(before); elapsed > 2*time.Second {
		t.Fatalf("memWatch should have returned promptly on timeout, took %v", elapsed)
	}
	if !watcherUnavailable.Load() {
		t.Fatal("watcherUnavailable should be true after an inference timeout")
	}
	if reason := getWatcherReason(); !strings.Contains(reason, "timed out") {
		t.Fatalf("reason should mention the timeout, got %q", reason)
	}
	if watcherDegradedUntil.Load() <= time.Now().UnixNano() {
		t.Fatal("watcherDegradedUntil should be set in the future after a timeout")
	}
}

// TestWatcherCaptureAvailableDuringBackoffSkipsProbe proves the fix for the
// /api/show lie: that metadata endpoint returns 200 instantly even while
// /api/chat is wedged, so during the post-timeout backoff window
// watcherCaptureAvailable must NOT re-probe it (that would flip capture back to
// "available" while inference is still hung).
func TestWatcherCaptureAvailableDuringBackoffSkipsProbe(t *testing.T) {
	resetWatcherState()
	t.Cleanup(resetWatcherState)
	var showHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			atomic.AddInt32(&showHits, 1)
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	t.Setenv("OLLAMA_HOST", srv.URL)
	t.Setenv("MEMORY_WATCHER_MODEL", "test-watcher")

	watcherUnavailable.Store(true)
	watcherLastProbe.Store(0)
	watcherDegradedUntil.Store(time.Now().Add(time.Minute).UnixNano())

	if watcherCaptureAvailable() {
		t.Fatal("degraded window: capture should be unavailable")
	}
	if got := atomic.LoadInt32(&showHits); got != 0 {
		t.Fatalf("expected /api/show NOT to be probed while degraded, got %d hit(s)", got)
	}
}

// TestWatcherCaptureAvailableRecoversAfterBackoff proves recovery still works
// once the backoff window elapses: the throttled /api/show re-probe runs again,
// and a subsequent successful memWatch clears every piece of degraded state.
func TestWatcherCaptureAvailableRecoversAfterBackoff(t *testing.T) {
	resetWatcherState()
	t.Cleanup(resetWatcherState)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/chat":
			w.Header().Set("content-type", "application/json")
			w.WriteHeader(200)
			w.Write([]byte(`{"message":{"content":"{\"facts\":[],\"events\":[],\"corrections\":[],\"valence\":0}"}}`))
		default: // /api/show
			w.WriteHeader(200)
		}
	}))
	defer srv.Close()
	t.Setenv("OLLAMA_HOST", srv.URL)
	t.Setenv("MEMORY_WATCHER_MODEL", "test-watcher")

	watcherUnavailable.Store(true)
	setWatcherReason(`model "test-watcher" is not pulled (or Ollama is down) — run ` + "`ollama pull test-watcher`")
	watcherDegradedUntil.Store(0) // backoff window elapsed
	watcherLastProbe.Store(0)

	if !watcherCaptureAvailable() {
		t.Fatal("backoff elapsed + model present: capture should be available")
	}
	if reason := getWatcherReason(); reason != "" {
		t.Fatalf("reason should be cleared after the recovery probe, got %q", reason)
	}

	// A subsequent real memWatch success clears unavailable/reason/degradedUntil too.
	if got := memWatch("hi"); got == nil {
		t.Fatal("expected a successful watch result")
	}
	if watcherUnavailable.Load() || getWatcherReason() != "" || watcherDegradedUntil.Load() != 0 {
		t.Fatal("successful memWatch should clear unavailable/reason/degradedUntil")
	}
}

// TestObserveReturnsDegradedReason proves the reason travels all the way to
// the RPC client: observe() must return {accepted:false, reason:...} with the
// LIVE reason (e.g. the timeout text), not just the generic
// "model unavailable" copy, while capture is degraded.
func TestObserveReturnsDegradedReason(t *testing.T) {
	resetWatcherState()
	t.Cleanup(resetWatcherState)
	store, err := newMemStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newMemoryMux(store, false))
	defer srv.Close()

	watcherUnavailable.Store(true)
	setWatcherReason("Ollama not responding: watcher inference timed out after 1s (is ollama healthy? try restarting it)")
	watcherDegradedUntil.Store(time.Now().Add(time.Minute).UnixNano())

	body := `{"jsonrpc":"2.0","id":1,"method":"observe","params":{"user":"remember that I like tabs not spaces"}}`
	res, err := http.Post(srv.URL, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var parsed struct {
		Result struct {
			Accepted bool   `json:"accepted"`
			Reason   string `json:"reason"`
		} `json:"result"`
	}
	if json.NewDecoder(res.Body).Decode(&parsed) != nil {
		t.Fatal("bad json response")
	}
	if parsed.Result.Accepted {
		t.Fatal("observe should report accepted:false while degraded")
	}
	if !strings.Contains(parsed.Result.Reason, "timed out") {
		t.Fatalf("observe reason should surface the live timeout reason, got %q", parsed.Result.Reason)
	}
}

// TestMemEmbedTimeout proves the recall side degrades instead of hanging: a
// wedged Ollama on /api/embed must return nil promptly (recall already falls
// back to keyword search on a nil vector) rather than blocking a turn.
func TestMemEmbedTimeout(t *testing.T) {
	embedDisabled.Store(false)
	t.Cleanup(func() { embedDisabled.Store(false) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond) // >> the test's MEMORY_EMBED_TIMEOUT_MS
		w.WriteHeader(200)
	}))
	defer srv.Close()
	t.Setenv("OLLAMA_HOST", srv.URL)
	t.Setenv("MEMORY_EMBED_TIMEOUT_MS", "50")

	before := time.Now()
	if got := memEmbed("hello"); got != nil {
		t.Fatalf("expected nil on embed timeout, got %v", got)
	}
	if elapsed := time.Since(before); elapsed > 2*time.Second {
		t.Fatalf("memEmbed should have returned promptly on timeout, took %v", elapsed)
	}
}

// TestMemOllamaHasModelTimeout verifies that memOllamaHasModel returns within the
// probe timeout when Ollama accepts the TCP connection but never sends a response
// (slow-loris / wedged inference scenario). Without the timeout fix (H-2) the
// function would block indefinitely, leaking the goroutine.
func TestMemOllamaHasModelTimeout(t *testing.T) {
	// A handler that accepts the connection and blocks — but with a maximum
	// dwell time so the test server can shut down cleanly when the test ends.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(1 * time.Second): // safety exit so srv.Close() doesn't stall
		}
	}))
	defer srv.Close()

	// Swap in a probe client with a very short timeout so the test runs quickly.
	orig := modelProbeClient
	modelProbeClient = &http.Client{Timeout: 100 * time.Millisecond}
	defer func() { modelProbeClient = orig }()

	// Override OLLAMA_HOST so the function hits our test server.
	t.Setenv("OLLAMA_HOST", srv.URL)

	start := time.Now()
	got := memOllamaHasModel("test-model")
	elapsed := time.Since(start)

	if got {
		t.Error("memOllamaHasModel should return false when server never responds")
	}
	// Should have returned well within 1s (probe timeout is 100ms + overhead).
	if elapsed > 2*time.Second {
		t.Errorf("memOllamaHasModel blocked for %v; want < 2s — timeout not enforced", elapsed)
	}
}

// FuzzMemFtsQuery ensures the FTS5 query builder never produces output containing
// unquoted metacharacters that could trigger FTS5 syntax errors or injection.
func FuzzMemFtsQuery(f *testing.F) {
	// Seed corpus with interesting inputs.
	for _, seed := range []string{
		"hello world",
		`a "b" c`,
		"drop; table--",
		"* OR AND NOT",
		"()",
		`"quoted"`,
		`back\slash`,
		"OR AND NOT NEAR",
		"term1 NEAR/3 term2",
		"^anchor",
		"",
		"a",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, q string) {
		got := memFtsQuery(q)
		if got == "" {
			return // empty is valid — all tokens were single-char or input was empty
		}
		// The output must only consist of quoted terms joined by ' OR '.
		// Unquoted FTS5 metacharacters would cause a sqlite syntax error on query.
		for _, bad := range []string{";", "*", "(", ")", "^"} {
			if strings.Contains(got, bad) {
				t.Errorf("memFtsQuery(%q) = %q contains unsafe char %q", q, got, bad)
			}
		}
		// Every term in the output must be wrapped in double quotes.
		parts := strings.Split(got, " OR ")
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if !strings.HasPrefix(part, `"`) || !strings.HasSuffix(part, `"`) {
				t.Errorf("memFtsQuery(%q) = %q has unquoted term: %q", q, got, part)
			}
		}
	})
}

func TestFlexibleStringList(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []string
		wantErr bool
	}{
		{"proper array", `["a","b"]`, []string{"a", "b"}, false},
		{"empty array", `[]`, []string{}, false},
		{"null", `null`, []string{}, false},
		{"absent (empty raw)", ``, []string{}, false},
		{"empty object normalizes to empty slice", `{}`, []string{}, false},
		{"non-empty object rejected", `{"a":1}`, nil, true},
		{"wrong scalar: string rejected", `"x"`, nil, true},
		{"wrong scalar: number rejected", `1`, nil, true},
		{"wrong scalar: bool rejected", `true`, nil, true},
		{"array of non-strings rejected", `[1,2]`, nil, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := flexibleStringList(json.RawMessage(c.in))
			if c.wantErr {
				if err == nil {
					t.Fatalf("flexibleStringList(%q) = %v, nil; want error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("flexibleStringList(%q) unexpected error: %v", c.in, err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("flexibleStringList(%q) = %v, want %v", c.in, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("flexibleStringList(%q) = %v, want %v", c.in, got, c.want)
				}
			}
		})
	}
}

func TestParseWatchJSON(t *testing.T) {
	cases := []struct {
		name       string
		in         string
		wantFacts  []string
		wantErr    bool
		wantValNum float64
	}{
		{
			name:      "proper arrays",
			in:        `{"facts":["uses pnpm"],"events":[],"corrections":[],"valence":0.5}`,
			wantFacts: []string{"uses pnpm"}, wantValNum: 0.5,
		},
		{
			// observed qwen output: fenced JSON with facts as {} instead of [].
			name:      "fenced JSON with empty object facts, salvaged via extractJSONObject",
			in:        "here you go:\n```json\n{\"facts\":{},\"events\":[],\"corrections\":[],\"valence\":0}\n```",
			wantFacts: []string{}, wantValNum: 0,
		},
		{
			name:      "empty object facts",
			in:        `{"facts":{},"events":[],"corrections":[],"valence":0}`,
			wantFacts: []string{}, wantValNum: 0,
		},
		{
			name:      "null facts",
			in:        `{"facts":null,"events":[],"corrections":[],"valence":0}`,
			wantFacts: []string{}, wantValNum: 0,
		},
		{
			name:    "non-empty object facts rejected",
			in:      `{"facts":{"a":"b"},"events":[],"corrections":[],"valence":0}`,
			wantErr: true,
		},
		{
			name:    "wrong scalar facts rejected",
			in:      `{"facts":"nope","events":[],"corrections":[],"valence":0}`,
			wantErr: true,
		},
		{
			name:    "not JSON at all",
			in:      `not json`,
			wantErr: true,
		},
		{
			name:    "top-level null rejected",
			in:      `null`,
			wantErr: true,
		},
		{
			name:    "top-level empty object rejected (missing all required keys)",
			in:      `{}`,
			wantErr: true,
		},
		{
			name:    "missing facts key rejected",
			in:      `{"events":[],"corrections":[],"valence":0}`,
			wantErr: true,
		},
		{
			name:    "missing events key rejected",
			in:      `{"facts":[],"corrections":[],"valence":0}`,
			wantErr: true,
		},
		{
			name:    "missing corrections key rejected",
			in:      `{"facts":[],"events":[],"valence":0}`,
			wantErr: true,
		},
		{
			name:    "missing valence key rejected",
			in:      `{"facts":[],"events":[],"corrections":[]}`,
			wantErr: true,
		},
		{
			name:    "top-level array rejected (wrong top-level type)",
			in:      `["facts","events"]`,
			wantErr: true,
		},
		{
			name:    "top-level array containing otherwise-valid object is not salvaged",
			in:      `[{"facts":[],"events":[],"corrections":[],"valence":0}]`,
			wantErr: true,
		},
		{
			name:    "top-level string rejected (wrong top-level type)",
			in:      `"just a string"`,
			wantErr: true,
		},
		{
			name:    "top-level number rejected (wrong top-level type)",
			in:      `42`,
			wantErr: true,
		},
		{
			name:    "top-level bool rejected (wrong top-level type)",
			in:      `true`,
			wantErr: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseWatchContent(c.in)
			if c.wantErr {
				if err == nil {
					t.Fatalf("parseWatchContent(%q) = %+v, nil; want error", c.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseWatchContent(%q) unexpected error: %v", c.in, err)
			}
			if len(got.Facts) != len(c.wantFacts) {
				t.Fatalf("parseWatchContent(%q).Facts = %v, want %v", c.in, got.Facts, c.wantFacts)
			}
			for i := range got.Facts {
				if got.Facts[i] != c.wantFacts[i] {
					t.Fatalf("parseWatchContent(%q).Facts = %v, want %v", c.in, got.Facts, c.wantFacts)
				}
			}
			if got.Valence != c.wantValNum {
				t.Fatalf("parseWatchContent(%q).Valence = %v, want %v", c.in, got.Valence, c.wantValNum)
			}
		})
	}
}

func TestExtractJSONObject(t *testing.T) {
	cases := []struct{ in, want string }{
		{`{"facts":["x"]}`, `{"facts":["x"]}`},
		{"here you go:\n```json\n{\"facts\":[\"x\"]}\n```", `{"facts":["x"]}`},
		{`prefix {"a":{"b":1}} suffix`, `{"a":{"b":1}}`},
		{`{"s":"has } brace in string"}`, `{"s":"has } brace in string"}`},
		{`no json here`, ``},
		{`{unbalanced`, ``},
	}
	for _, c := range cases {
		if got := extractJSONObject(c.in); got != c.want {
			t.Errorf("extractJSONObject(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
