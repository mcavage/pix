package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/plugin"
)

// --- in-process knowledgeMux over a real temp-indexed store (FTS-only) --------

// TestKnowledgeMuxHealthAndQuery drives the builtin fast path: a real store
// indexed from a temp OKF bundle, served through knowledgeMux over httptest.
// Hermetic — nil embedder (no Ollama), :memory: sqlite, no subprocess.
func TestKnowledgeMuxHealthAndQuery(t *testing.T) {
	dir := writeKnowledgeBundle(t)
	st := newTestKnowledgeStore(t)
	if _, _, err := st.reindex([]string{dir}); err != nil {
		t.Fatalf("reindex: %v", err)
	}

	srv := httptest.NewServer(knowledgeMux(st))
	defer srv.Close()

	// health: shape matches KnowledgeHealth ({ok, vector, bundles, concepts}).
	resp := rpcCall(t, srv, "health")
	if resp["jsonrpc"] != "2.0" || resp["id"].(float64) != 7 {
		t.Fatalf("bad JSON-RPC envelope: %v", resp)
	}
	hr, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("health missing result: %v", resp)
	}
	for _, k := range []string{"ok", "vector", "bundles", "concepts"} {
		if _, present := hr[k]; !present {
			t.Errorf("health result missing %q: %v", k, hr)
		}
	}
	if hr["ok"] != true {
		t.Errorf("health ok = %v, want true", hr["ok"])
	}
	if hr["vector"] != false {
		t.Errorf("health vector = %v, want false (nil embedder)", hr["vector"])
	}
	if hr["concepts"].(float64) != 2 {
		t.Errorf("health concepts = %v, want 2", hr["concepts"])
	}

	// query: returns the refund policy concept with a snippet + citations.
	qr := rpcParamCall(t, srv, "query", map[string]any{"query": "refund approval", "limit": 3})
	res, ok := qr["result"].(map[string]any)
	if !ok {
		t.Fatalf("query missing result: %v", qr)
	}
	concepts, ok := res["concepts"].([]any)
	if !ok || len(concepts) == 0 {
		t.Fatalf("query returned no concepts: %v", res)
	}
	top := concepts[0].(map[string]any)
	if top["id"] != "policies/refunds" {
		t.Errorf("top concept id = %v, want policies/refunds", top["id"])
	}
	if top["title"] != "Refund Policy" {
		t.Errorf("top concept title = %v", top["title"])
	}
	if s, _ := top["snippet"].(string); s == "" {
		t.Error("top concept snippet should be non-empty")
	}
	if cites, ok := top["citations"].([]any); !ok || len(cites) == 0 {
		t.Errorf("top concept citations = %v, want non-empty", top["citations"])
	}
}

// TestKnowledgeMuxBundleSetParam drives the JSON-RPC `query` param parsing: the
// new `bundles` array scopes the search, and a legacy single `bundle` string is
// still honoured (wrapped into a 1-elem set) for back-compat.
func TestKnowledgeMuxBundleSetParam(t *testing.T) {
	dir := writeKnowledgeBundle(t)
	st := newTestKnowledgeStore(t)
	if _, _, err := st.reindex([]string{dir}); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	srv := httptest.NewServer(knowledgeMux(st))
	defer srv.Close()

	concepts := func(params map[string]any) []any {
		qr := rpcParamCall(t, srv, "query", params)
		res, ok := qr["result"].(map[string]any)
		if !ok {
			t.Fatalf("query missing result: %v", qr)
		}
		c, _ := res["concepts"].([]any)
		return c
	}

	// New `bundles` array with the real bundle -> hits.
	if got := concepts(map[string]any{"query": "refund approval", "bundles": []string{dir}}); len(got) == 0 {
		t.Error("bundles=[dir] returned no concepts")
	}
	// `bundles` array with a bogus path -> filtered to empty.
	if got := concepts(map[string]any{"query": "refund", "bundles": []string{"/no/such/bundle"}}); len(got) != 0 {
		t.Errorf("bundles=[/no/such] should drop all hits, got %d", len(got))
	}
	// Back-compat: a single `bundle` string still filters.
	if got := concepts(map[string]any{"query": "refund approval", "bundle": dir}); len(got) == 0 {
		t.Error("legacy single bundle=dir returned no concepts")
	}
	if got := concepts(map[string]any{"query": "refund", "bundle": "/no/such/bundle"}); len(got) != 0 {
		t.Errorf("legacy single bundle=/no/such should drop all hits, got %d", len(got))
	}
}

// --- plugin proxy path over a deterministic stub -----------------------------

type stubKnowledge struct{}

func (stubKnowledge) Query(plugin.QueryArgs) (plugin.QueryResult, error) {
	return plugin.QueryResult{Concepts: []plugin.CitedConcept{{
		ID: "c-1", Type: "Reference", Title: "Refund policy", Description: "how refunds work",
		Snippet: "Refunds are issued within 30 days.", Score: 0.9,
		Citations: []string{"src://policy"}, Bundle: "/tmp/kb",
	}}}, nil
}
func (stubKnowledge) Reindex(r plugin.ReindexArgs) (plugin.ReindexResult, error) {
	return plugin.ReindexResult{Indexed: len(r.BundlePaths), Bundles: r.BundlePaths}, nil
}
func (stubKnowledge) Health() (plugin.KnowledgeHealth, error) {
	return plugin.KnowledgeHealth{OK: true, Vector: true, Bundles: []string{"/tmp/kb"}, Concepts: 1}, nil
}

func TestKnowledgeProxyMuxContract(t *testing.T) {
	h := &pluginHolder{}
	h.Set(stubKnowledge{}, nil)
	srv := httptest.NewServer(knowledgeProxyMux(h))
	defer srv.Close()

	// health envelope + shape.
	hr, _ := rpcCall(t, srv, "health")["result"].(map[string]any)
	for _, k := range []string{"ok", "vector", "bundles", "concepts"} {
		if _, present := hr[k]; !present {
			t.Errorf("health result missing %q: %v", k, hr)
		}
	}

	// query: shape carries the concept fields.
	qr := rpcParamCall(t, srv, "query", map[string]any{"query": "refund"})
	res, _ := qr["result"].(map[string]any)
	concepts, ok := res["concepts"].([]any)
	if !ok || len(concepts) != 1 {
		t.Fatalf("query concepts = %v, want 1", res["concepts"])
	}
	top := concepts[0].(map[string]any)
	for _, k := range []string{"id", "type", "title", "description", "path", "snippet", "score", "citations", "bundle"} {
		if _, present := top[k]; !present {
			t.Errorf("concept missing %q: %v", k, top)
		}
	}

	// method-not-found path mirrors jsonrpcMux.
	nf := rpcCall(t, srv, "does-not-exist")
	e, ok := nf["error"].(map[string]any)
	if !ok || e["code"].(float64) != -32601 {
		t.Errorf("unknown method should yield -32601, got %v", nf)
	}

	// unavailable plugin surfaces an error, not a panic.
	empty := &pluginHolder{}
	esrv := httptest.NewServer(knowledgeProxyMux(empty))
	defer esrv.Close()
	er := rpcCall(t, esrv, "health")
	if _, ok := er["error"].(map[string]any); !ok {
		t.Errorf("nil plugin should yield an error envelope, got %v", er)
	}
}

// --- enabled-set / config wiring ---------------------------------------------

// TestResolveServicesIncludesKnowledge: knowledge is opt-in (not a default), but
// resolveServices carries it through when named in config or on the CLI.
func TestResolveServicesIncludesKnowledge(t *testing.T) {
	// Not in the fresh-install defaults.
	for _, s := range config.DefaultServices {
		if s == "knowledge" {
			t.Fatalf("knowledge should NOT be a default service, DefaultServices = %v", config.DefaultServices)
		}
	}
	// Config carries it.
	got := resolveServices(nil, []string{"memory", "knowledge"})
	found := false
	for _, s := range got {
		if s == "knowledge" {
			found = true
		}
	}
	if !found {
		t.Fatalf("resolveServices dropped knowledge: %v", got)
	}
	// CLI override wins and can select knowledge alone.
	if got := resolveServices([]string{"knowledge"}, []string{"memory"}); len(got) != 1 || got[0] != "knowledge" {
		t.Fatalf("CLI override = %v, want [knowledge]", got)
	}
}

// TestConfigKnowledgeBundles: the config decodes knowledge_bundles and defaults
// to empty (absence is not an error, no bundles).
func TestConfigKnowledgeBundles(t *testing.T) {
	// Absent file -> empty bundles, existing defaults intact.
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "nope.toml"))
	c, err := config.Load()
	if err != nil {
		t.Fatalf("Load absent: %v", err)
	}
	if len(c.KnowledgeBundles) != 0 {
		t.Errorf("default KnowledgeBundles = %v, want empty", c.KnowledgeBundles)
	}

	// Decoded from TOML.
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("knowledge_bundles = [\"/kb/a\", \"/kb/b\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIX_CONFIG", path)
	c2, err := config.Load()
	if err != nil {
		t.Fatalf("Load decode: %v", err)
	}
	if len(c2.KnowledgeBundles) != 2 || c2.KnowledgeBundles[0] != "/kb/a" || c2.KnowledgeBundles[1] != "/kb/b" {
		t.Errorf("KnowledgeBundles = %v, want [/kb/a /kb/b]", c2.KnowledgeBundles)
	}
}

// TestKnowledgeBundlesEnvFallback: knowledgeBundles falls back to the
// KNOWLEDGE_BUNDLES env (comma / path-list separated) when config is empty, and
// the config value wins when set.
func TestKnowledgeBundlesEnvFallback(t *testing.T) {
	t.Setenv("KNOWLEDGE_BUNDLES", "/kb/x,/kb/y")
	if got := knowledgeBundles(&config.Config{}); len(got) != 2 || got[0] != "/kb/x" || got[1] != "/kb/y" {
		t.Fatalf("env fallback = %v, want [/kb/x /kb/y]", got)
	}
	// Config wins over env.
	cfg := &config.Config{KnowledgeBundles: []string{"/kb/cfg"}}
	if got := knowledgeBundles(cfg); len(got) != 1 || got[0] != "/kb/cfg" {
		t.Fatalf("config should win over env, got %v", got)
	}
	// No config, no env -> nil.
	os.Unsetenv("KNOWLEDGE_BUNDLES")
	if got := knowledgeBundles(&config.Config{}); got != nil {
		t.Fatalf("empty everything = %v, want nil", got)
	}
}

// rpcParamCall is rpcCall with params (rpcCall in serve_test.go covers the
// param-less case).
func rpcParamCall(t *testing.T, srv *httptest.Server, method string, params map[string]any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 7, "method": method, "params": params})
	res, err := http.Post(srv.URL, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}
