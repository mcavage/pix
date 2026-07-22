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
	setWatcherReason(`model "test-watcher" is not pulled (or Ollama is down), run ` + "`ollama pull test-watcher`")
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
	// A handler that accepts the connection and blocks, but with a maximum
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
		t.Errorf("memOllamaHasModel blocked for %v; want < 2s, timeout not enforced", elapsed)
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
			return // empty is valid, all tokens were single-char or input was empty
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

// TestQuestionOnlyUserMessage covers the exact incident messages: pure
// questions must gate facts, mixed question+assertion messages must not, and a
// correction phrased as a polite question still reads as "question-only" here
// (questionOnlyUserMessage doesn't know about correction/fact distinction -
// memCapture is the one that must never apply it to corrections).
func TestQuestionOnlyUserMessage(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"pure question about memory use", "so are you using my memories?", true},
		{"pure question about injection", "why do we think those are the right things to inject?", true},
		{
			"mixed question + assertion is not question-only",
			"can you explain how memory works? I ran the build twice and im confused. Also can you see what's stored?",
			false,
		},
		{"plain assertion, no question", "i like cheese", false},
		{"correction phrased as a polite question", "Can you stop using em dashes?", true},
		{"empty string has no assertions to gate", "", false},
		{"only punctuation/whitespace", "... ?? !!", false},
		{"acknowledgment, not a question", "thanks, that works", false},
		{"fenced code is ignored, remaining prose is a bare question", "```\nfunc main() {}\n```\nis this right?", true},
		{"fenced code is ignored, remaining prose is an assertion", "```\nfunc main() {}\n```\nthis is how I write Go.", false},
		{"inline filename period is not a sentence break", "Why does main.go fail?", true},
		{"inline version-number period is not a sentence break", "Is v1.2 installed?", true},
		{"inline URL periods are not sentence breaks", "Why https://example.com/path?", true},
		{
			"a question followed by a real declarative sentence stays mixed (not question-only)",
			"Why does main.go fail? I already checked twice.",
			false,
		},
		{"double-quote wrapped question", `"Are you using my memories?"`, true},
		{"single-quote wrapped question", `'Are you using my memories?'`, true},
		{"markdown bold wrapped question", "**Are you using my memories?**", true},
		{"parenthesis wrapped question", "(Are you using my memories?)", true},
		{"backtick wrapped question", "`Are you using my memories?`", true},
		{"bracket wrapped question", "[Are you using my memories?]", true},
		{"curly-quote wrapped question", "\u201cAre you using my memories?\u201d", true},
		{"quote-wrapped assertion is still not a question", `"I like cheese."`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := questionOnlyUserMessage(c.in); got != c.want {
				t.Errorf("questionOnlyUserMessage(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// TestSplitSentenceClauses locks in the clause-boundary fix directly: a run of
// terminal punctuation (. ! ?) only ends a clause when followed by whitespace
// or end of string. An inline mark immediately followed by more non-space text
// (a filename, a version number, a URL) must NOT split the clause.
func TestSplitSentenceClauses(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"filename period does not split", "Why does main.go fail?", []string{"Why does main.go fail?"}},
		{"version number period does not split", "Is v1.2 installed?", []string{"Is v1.2 installed?"}},
		{"URL periods do not split", "Why https://example.com/path?", []string{"Why https://example.com/path?"}},
		{
			"a real sentence boundary (period + space) still splits",
			"Why does main.go fail? I already checked twice.",
			[]string{"Why does main.go fail?", " I already checked twice."},
		},
		{
			"punctuation run collapses to its last mark and still respects the trailing-space rule",
			"explain this...? then what",
			[]string{"explain this?", " then what"},
		},
		{"trailing text with no terminal punctuation is one final clause", "no ending here", []string{"no ending here"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := splitSentenceClauses(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("splitSentenceClauses(%q) = %#v, want %#v", c.in, got, c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Fatalf("splitSentenceClauses(%q) = %#v, want %#v", c.in, got, c.want)
				}
			}
		})
	}
}

// TestWatcherNoise checks the conservative prefix list, case-insensitively,
// and proves it does NOT substring-match legitimate content that merely
// mentions "the user" mid-sentence.
func TestWatcherNoise(t *testing.T) {
	noisy := []string{
		"user requested a summary of the changes",
		"The user requested a summary of the changes",
		"USER REQUESTED a summary of the changes",
		"user asked about the deploy process",
		"the user asked about the deploy process",
		"user is interested in memory internals",
		"the user is interested in memory internals",
		"user wants to know how recall scores hits",
		"the user wants to know how recall scores hits",
		"user ran the test suite twice",
		"the user ran the test suite twice",
		"user executed the migration script",
		"the user executed the migration script",
		"  user ran the build",
	}
	for _, s := range noisy {
		if !watcherNoise(s) {
			t.Errorf("watcherNoise(%q) = false, want true", s)
		}
	}
	clean := []string{
		"the user prefers tabs over spaces",
		"the user always deploys from main, never a release branch",
		"i like cheese",
		"the user's staging DB is postgres://staging.internal:5432",
		"always ask before running destructive commands for the user",
		// "expects" was deliberately dropped from watcherNoisePrefixes: a stated
		// expectation is a durable preference, not session narration.
		"user expects a reply within a day",
		"the user expects a reply within a day",
		"User expects tests on every PR",
	}
	for _, s := range clean {
		if watcherNoise(s) {
			t.Errorf("watcherNoise(%q) = true, want false (must not substring-match)", s)
		}
	}
}

// watchServer stands in for Ollama's /api/chat: returns a fixed watcher JSON
// payload for the capture call, so memCapture can be exercised end to end
// without a real model. memCapture is called with a slot already taken from
// memCaptureSem, mirroring how the observe handler acquires it before the
// (normally goroutine) call.
func watchServer(t *testing.T, content string) *memStore {
	t.Helper()
	resetWatcherState()
	t.Cleanup(resetWatcherState)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/chat" {
			b, _ := json.Marshal(map[string]any{"message": map[string]any{"content": content}})
			w.WriteHeader(200)
			w.Write(b)
			return
		}
		w.WriteHeader(200)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OLLAMA_HOST", srv.URL)
	t.Setenv("MEMORY_WATCHER_MODEL", "test-watcher")
	st, err := newMemStore(":memory:", nil)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// TestMemCaptureDropsFactsForQuestionOnlyMessage is the end-to-end version of
// the observed incident: a question about memory itself must never land as a
// stored fact, even when the watcher model wrongly extracted one.
func TestMemCaptureDropsFactsForQuestionOnlyMessage(t *testing.T) {
	st := watchServer(t, `{"facts":["user is using memory"],"events":[],"corrections":[],"valence":0}`)
	memCaptureSem <- struct{}{}
	memCapture(st, "so are you using my memories?", "", false, "default")

	hits, err := st.recallAll(100, 1000000, "", "", "default", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 0 {
		t.Fatalf("expected the question-only false-positive fact to be dropped, got %+v", hits)
	}
}

// TestMemCaptureAppliesNoiseFilterToFactsAndEventsOnlyAndSevenDayEventTTL
// proves the watcherNoise filter runs on facts and events but is NEVER
// applied to corrections (a correction may legitimately be phrased exactly
// like a noise prefix, e.g. "the user requested the agent stop doing X"),
// that legitimate items in each channel still survive, and that a captured
// event gets the 7-day TTL (not the old 21).
func TestMemCaptureAppliesNoiseFilterToFactsAndEventsOnlyAndSevenDayEventTTL(t *testing.T) {
	content := `{"facts":["user asked about the deploy process","user requested guidance","the user prefers tabs over spaces"],` +
		`"events":["user ran the test suite twice","migrating the staging DB this week"],` +
		`"corrections":["the user requested this be ignored","always confirm before deleting a branch"],` +
		`"valence":0}`
	st := watchServer(t, content)
	memCaptureSem <- struct{}{}
	memCapture(st, "an assertion-bearing message, not a question.", "", false, "default")

	hits, err := st.recallAll(100, 1000000, "", "", "default", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, h := range hits {
		seen[h.content] = true
	}
	for _, noisy := range []string{
		"user asked about the deploy process",
		"user requested guidance",
		"user ran the test suite twice",
	} {
		if seen[noisy] {
			t.Errorf("noise item %q should have been dropped before storage, got %+v", noisy, seen)
		}
	}
	for _, legit := range []string{
		"the user prefers tabs over spaces",
		"migrating the staging DB this week",
		"always confirm before deleting a branch",
		// A correction is NEVER run through the noise filter, even though this
		// phrasing would trip the "requested" prefix on the fact/event channels.
		"the user requested this be ignored",
	} {
		if !seen[legit] {
			t.Errorf("legitimate item %q should have survived the noise filter, got %+v", legit, seen)
		}
	}

	var expiresAt string
	if err := st.db.QueryRow("SELECT expires_at FROM memories WHERE content = ?", "migrating the staging DB this week").Scan(&expiresAt); err != nil {
		t.Fatal(err)
	}
	days := time.Until(parseTime(expiresAt)).Hours() / 24
	if days < 6.9 || days > 7.1 {
		t.Fatalf("expected a ~7 day TTL on a captured event, got %.2f days (expiresAt=%s)", days, expiresAt)
	}
}
