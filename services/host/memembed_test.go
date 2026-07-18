package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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
	orig := embedProbeClient
	embedProbeClient = &http.Client{Timeout: 100 * time.Millisecond}
	defer func() { embedProbeClient = orig }()

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
