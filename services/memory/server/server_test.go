// server_test.go is the focused MCP surface test: initialize, tools/list
// (all eight tools, with accurate annotations), and tools/call for the
// mutating and read-only paths, driven through the real Streamable HTTP
// transport with the official go-sdk client (not raw JSON-RPC), so a
// regression in either side of the protocol handshake shows up here.
package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"pix-memory/server"
	"pix-memory/store"
)

// testAuthToken is the fixed bearer token every test in this file registers
// (auth is a security invariant, not an optional feature to bypass in a
// test double).
const testAuthToken = "test-token-do-not-use-in-prod"

// bearerRoundTripper injects "Authorization: Bearer <token>" on every
// request, mirroring what a real MCP Gateway client configured with a
// header-bearing registration would send.
type bearerRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (rt bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+rt.token)
	base := rt.base
	if base == nil {
		base = http.DefaultTransport
	}
	return base.RoundTrip(req)
}

// newTestServer starts an httptest server over server.NewMux backed by a
// fresh temp-dir store, and returns a connected MCP client session (already
// carrying the correct bearer token) plus a cleanup func.
func newTestServer(t *testing.T) (*mcp.ClientSession, *httptest.Server) {
	t.Helper()
	st, err := store.Open(t.TempDir()+"/memory.db", nil) // no embedder: keyword-only recall
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	ts := httptest.NewServer(server.NewMux(st, testAuthToken))
	t.Cleanup(ts.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:   ts.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearerRoundTripper{token: testAuthToken}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("client.Connect (MCP initialize): %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess, ts
}

func TestInitializeHandshake(t *testing.T) {
	sess, _ := newTestServer(t)
	res := sess.InitializeResult()
	if res == nil {
		t.Fatal("InitializeResult is nil after Connect")
	}
	if res.ServerInfo == nil || res.ServerInfo.Name != "pix-memory" {
		t.Fatalf("serverInfo.name = %+v, want pix-memory", res.ServerInfo)
	}
}

func TestToolsListShapeAndAnnotations(t *testing.T) {
	sess, _ := newTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}

	want := map[string]struct {
		readOnly, destructive, idempotent bool
	}{
		"memory_recall":   {readOnly: true, idempotent: true},
		"memory_stats":    {readOnly: true, idempotent: true},
		"memory_remember": {readOnly: false, destructive: false, idempotent: false},
		"memory_forget":   {readOnly: false, destructive: true, idempotent: true},
		"memory_observe":  {readOnly: false, destructive: false, idempotent: false},
		"memory_status":   {readOnly: true, idempotent: true},
		"memory_snapshot": {readOnly: false, destructive: false, idempotent: false},
		"memory_restore":  {readOnly: false, destructive: true, idempotent: true},
	}
	if len(out.Tools) != len(want) {
		names := make([]string, len(out.Tools))
		for i, tl := range out.Tools {
			names[i] = tl.Name
		}
		t.Fatalf("tools/list returned %d tools %v, want %d: %v", len(out.Tools), names, len(want), keys(want))
	}
	for _, tl := range out.Tools {
		w, ok := want[tl.Name]
		if !ok {
			t.Errorf("unexpected tool %q", tl.Name)
			continue
		}
		if tl.Annotations == nil {
			t.Errorf("%s: annotations is nil", tl.Name)
			continue
		}
		if tl.Annotations.ReadOnlyHint != w.readOnly {
			t.Errorf("%s: readOnlyHint = %v, want %v", tl.Name, tl.Annotations.ReadOnlyHint, w.readOnly)
		}
		destructive := tl.Annotations.DestructiveHint != nil && *tl.Annotations.DestructiveHint
		if destructive != w.destructive {
			t.Errorf("%s: destructiveHint = %v, want %v", tl.Name, destructive, w.destructive)
		}
		if tl.Annotations.IdempotentHint != w.idempotent {
			t.Errorf("%s: idempotentHint = %v, want %v", tl.Name, tl.Annotations.IdempotentHint, w.idempotent)
		}
	}
}

func keys(m map[string]struct {
	readOnly, destructive, idempotent bool
}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func callTool[Out any](t *testing.T, sess *mcp.ClientSession, name string, args map[string]any) Out {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("tools/call %s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("tools/call %s returned an error result: %+v", name, res.Content)
	}
	var out Out
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("%s: marshal structuredContent: %v", name, err)
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%s: unmarshal structuredContent %s: %v", name, b, err)
	}
	return out
}

func TestRememberRecallForgetRoundTrip(t *testing.T) {
	sess, _ := newTestServer(t)

	type rememberOut struct {
		ID         string `json:"id"`
		Reaffirmed bool   `json:"reaffirmed"`
	}
	remembered := callTool[rememberOut](t, sess, "memory_remember", map[string]any{
		"content": "the user prefers tabs over spaces",
	})
	if remembered.ID == "" || remembered.Reaffirmed {
		t.Fatalf("memory_remember: unexpected result %+v", remembered)
	}

	// Same content again reaffirms the same row instead of creating a new one.
	again := callTool[rememberOut](t, sess, "memory_remember", map[string]any{
		"content": "the user prefers tabs over spaces",
	})
	if !again.Reaffirmed || again.ID != remembered.ID {
		t.Fatalf("memory_remember (repeat): want reaffirm of %s, got %+v", remembered.ID, again)
	}

	type recallOut struct {
		Hits []struct {
			ID      string `json:"id"`
			Content string `json:"content"`
		} `json:"hits"`
	}
	recalled := callTool[recallOut](t, sess, "memory_recall", map[string]any{"query": "tabs"})
	found := false
	for _, h := range recalled.Hits {
		if h.ID == remembered.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("memory_recall(%q): want id %s among hits, got %+v", "tabs", remembered.ID, recalled.Hits)
	}

	type statsOut struct {
		Active int `json:"active"`
		Facts  int `json:"facts"`
	}
	stats := callTool[statsOut](t, sess, "memory_stats", map[string]any{})
	if stats.Active < 1 || stats.Facts < 1 {
		t.Fatalf("memory_stats: want active/facts >= 1, got %+v", stats)
	}

	type forgetOut struct {
		OK bool `json:"ok"`
	}
	forgot := callTool[forgetOut](t, sess, "memory_forget", map[string]any{"id": remembered.ID})
	if !forgot.OK {
		t.Fatalf("memory_forget(%s): want ok=true, got %+v", remembered.ID, forgot)
	}
	// A second forget of the same, now-deleted id is a clean miss, not an error.
	missed := callTool[forgetOut](t, sess, "memory_forget", map[string]any{"id": remembered.ID})
	if missed.OK {
		t.Fatalf("memory_forget(%s) repeat: want ok=false (already gone), got %+v", remembered.ID, missed)
	}
}

func TestObserveExplicitModeIsANoOp(t *testing.T) {
	sess, _ := newTestServer(t)
	type observeOut struct {
		Accepted bool   `json:"accepted"`
		Reason   string `json:"reason"`
	}
	out := callTool[observeOut](t, sess, "memory_observe", map[string]any{"text": "I always use vim"})
	if out.Accepted {
		t.Fatalf("memory_observe with default (explicit) capture mode: want accepted=false, got %+v", out)
	}
}

func TestStatusReportsSchemaVersion(t *testing.T) {
	sess, _ := newTestServer(t)
	type statusOut struct {
		SchemaVersion int    `json:"schema_version"`
		CaptureMode   string `json:"capture_mode"`
		Ready         bool   `json:"ready"`
	}
	out := callTool[statusOut](t, sess, "memory_status", map[string]any{})
	if out.SchemaVersion != 2 {
		t.Fatalf("memory_status: schema_version = %d, want 2", out.SchemaVersion)
	}
	if out.CaptureMode != "explicit" {
		t.Fatalf("memory_status: capture_mode = %q, want explicit (default)", out.CaptureMode)
	}
	if !out.Ready {
		t.Fatalf("memory_status: ready = false, want true")
	}
}

func TestSnapshotAndRestoreRoundTrip(t *testing.T) {
	sess, _ := newTestServer(t)

	type rememberOut struct {
		ID string `json:"id"`
	}
	callTool[rememberOut](t, sess, "memory_remember", map[string]any{"content": "snapshot me"})

	type snapshotOut struct {
		Path          string `json:"path"`
		Rows          int    `json:"rows"`
		SchemaVersion int    `json:"schema_version"`
	}
	snap := callTool[snapshotOut](t, sess, "memory_snapshot", map[string]any{"path": t.TempDir() + "/snap.db"})
	if snap.Rows < 1 || snap.SchemaVersion != 2 {
		t.Fatalf("memory_snapshot: unexpected result %+v", snap)
	}

	type restoreOut struct {
		Path       string `json:"path"`
		Rows       int    `json:"rows"`
		BackupPath string `json:"backup_path"`
	}
	restored := callTool[restoreOut](t, sess, "memory_restore", map[string]any{"path": snap.Path, "force": true})
	if restored.Rows != snap.Rows {
		t.Fatalf("memory_restore: rows = %d, want %d", restored.Rows, snap.Rows)
	}
	if restored.BackupPath == "" {
		t.Fatalf("memory_restore: want a non-empty backup_path (previous db kept)")
	}

	// The store must still answer after restore swapped the live db out from
	// under it in-process.
	type recallOut struct {
		Hits []struct {
			Content string `json:"content"`
		} `json:"hits"`
	}
	recalled := callTool[recallOut](t, sess, "memory_recall", map[string]any{"query": "*"})
	if len(recalled.Hits) < 1 {
		t.Fatalf("memory_recall after restore: want at least 1 hit, got %+v", recalled.Hits)
	}
}

// --- security re-review HIGH: /mcp auth --------------------------------------

// TestMCPRequiresAuth_UnauthorizedWithoutToken proves an /mcp request with NO
// credential at all is refused before it ever reaches the MCP handler — the
// exact "any local process can read/write agent memory" gap the security
// re-review found.
func TestMCPRequiresAuth_UnauthorizedWithoutToken(t *testing.T) {
	st, err := store.Open(t.TempDir()+"/memory.db", nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ts := httptest.NewServer(server.NewMux(st, testAuthToken))
	t.Cleanup(ts.Close)

	resp, err := ts.Client().Post(ts.URL+"/mcp", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /mcp with no auth: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("POST /mcp with no auth: status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

// TestMCPRequiresAuth_UnauthorizedWithWrongToken proves a wrong bearer value
// is refused, not merely an absent one.
func TestMCPRequiresAuth_UnauthorizedWithWrongToken(t *testing.T) {
	st, err := store.Open(t.TempDir()+"/memory.db", nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ts := httptest.NewServer(server.NewMux(st, testAuthToken))
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /mcp with wrong auth: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("POST /mcp with wrong auth: status = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

// TestMCPRequiresAuth_NoConfiguredTokenRefusesEverything proves an empty
// configured token (misconfiguration) fails CLOSED — never a silent
// unauthenticated fallback.
func TestMCPRequiresAuth_NoConfiguredTokenRefusesEverything(t *testing.T) {
	st, err := store.Open(t.TempDir()+"/memory.db", nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ts := httptest.NewServer(server.NewMux(st, ""))
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", nil)
	req.Header.Set("Authorization", "Bearer anything")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("POST /mcp with no configured token: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("POST /mcp with no configured token: status = %d, want %d", resp.StatusCode, http.StatusServiceUnavailable)
	}
}

// TestMCPQueryTokenAuthorizes proves the loopback-URL credential fallback
// (?token=) a native sbx MCP declaration must use in place of a header
// works end to end over the real MCP handshake.
func TestMCPQueryTokenAuthorizes(t *testing.T) {
	st, err := store.Open(t.TempDir()+"/memory.db", nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ts := httptest.NewServer(server.NewMux(st, testAuthToken))
	t.Cleanup(ts.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	transport := &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp?token=" + testAuthToken}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("client.Connect over ?token=: %v", err)
	}
	defer sess.Close()
	if sess.InitializeResult() == nil {
		t.Fatal("InitializeResult is nil after Connect over ?token=")
	}
}

// TestHealthzNeedsNoAuth proves /healthz answers with NO credential at all,
// even when a real auth token is configured — architecture §9.1's stated
// exception.
func TestHealthzNeedsNoAuth(t *testing.T) {
	_, ts := newTestServer(t)
	resp, err := ts.Client().Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz with no auth: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /healthz with no auth: status = %d, want 200", resp.StatusCode)
	}
}

func TestHealthz(t *testing.T) {
	_, ts := newTestServer(t)
	resp, err := ts.Client().Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("GET /healthz: status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode /healthz body: %v", err)
	}
	if !body.OK {
		t.Fatalf("/healthz body.ok = false, want true")
	}
}
