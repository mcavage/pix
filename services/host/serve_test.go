package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/plugin"
	"pix/host/supervise"
)

// --- F1: enabled-set resolution honors cfg.Services and CLI override ---------

func TestResolveServices(t *testing.T) {
	cases := []struct {
		name string
		cli  []string
		cfg  []string
		want []string
	}{
		{"cli empty falls back to config", nil, []string{"memory"}, []string{"memory"}},
		{"cli of empty strings falls back", []string{"", " "}, []string{"memory"}, []string{"memory"}},
		{"cli overrides config", []string{"knowledge"}, []string{"memory"}, []string{"knowledge"}},
		{"both empty means all (nil)", nil, nil, nil},
		{"cli monitor overrides a memory-only config", []string{"monitor"}, []string{"memory"}, []string{"monitor"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveServices(tc.cli, tc.cfg)
			if len(got) != len(tc.want) {
				t.Fatalf("resolveServices(%v,%v) = %v, want %v", tc.cli, tc.cfg, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("resolveServices(%v,%v) = %v, want %v", tc.cli, tc.cfg, got, tc.want)
				}
			}
		})
	}
}

// --- F5: an external plugin with a mismatched SHA refuses to launch ----------

func TestVerifyPluginSHA(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-plugin")
	content := []byte("#!/bin/sh\nexit 0\n")
	if err := os.WriteFile(bin, content, 0o755); err != nil {
		t.Fatal(err)
	}

	// Compute the correct SHA-256 of the binary content.
	sum := sha256.Sum256(content)
	correctSHA := hex.EncodeToString(sum[:])

	// Empty SHA is a hard refusal (F-C): external plugins MUST be sha-pinned.
	if err := verifyPluginSHA(config.PluginSpec{Path: bin}); err == nil || !strings.Contains(err.Error(), "unpinned") {
		t.Errorf("empty SHA should be refused as unpinned, got %v", err)
	}

	// Wrong SHA is a hard refusal.
	err := verifyPluginSHA(config.PluginSpec{Path: bin, SHA: "0000deadbeef"})
	if err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("wrong SHA should refuse with a mismatch error, got %v", err)
	}

	// Correct SHA passes (M-8).
	if err := verifyPluginSHA(config.PluginSpec{Path: bin, SHA: correctSHA}); err != nil {
		t.Errorf("correct SHA should be accepted, got %v", err)
	}

	// Uppercase SHA is accepted (case-insensitive match) (M-8).
	if err := verifyPluginSHA(config.PluginSpec{Path: bin, SHA: strings.ToUpper(correctSHA)}); err != nil {
		t.Errorf("uppercase SHA should be accepted, got %v", err)
	}
}

// TestLaunchRefusesOnSHAMismatch proves the refusal is enforced at launch time
// (before any subprocess is spawned) — hermetic, no real go-plugin handshake.
func TestLaunchRefusesOnSHAMismatch(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-plugin")
	if err := os.WriteFile(bin, []byte("not a real plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	sup := &supervisor{}
	h, err := sup.launch("test", "memory", config.PluginSpec{Path: bin, SHA: "wrongsha"}, "", nil)
	if err == nil {
		t.Fatal("launch with a mismatched SHA should refuse, got nil error")
	}
	if h != nil {
		t.Errorf("launch should not return a holder on SHA refusal, got %v", h)
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("expected a sha256 mismatch error, got %v", err)
	}
}

// --- F2: a plugin subprocess env never contains the broker bearer ------------

// pluginEnv is the exact composition prod uses: every unit is launched with
// EnvAllow: pluginEnvAllowNames(), which supervise.FilterEnv applies to the
// child's environment.
func pluginEnv(extra []string) []string {
	return supervise.FilterEnv(pluginEnvAllowNames(), extra)
}

func TestPluginEnvStripsBearer(t *testing.T) {
	t.Setenv("PIX_BROKER_AUTH", "super-secret-bearer")

	// A generic plugin (memory/mcp): the bearer must be gone.
	for _, kv := range pluginEnv(nil) {
		if strings.HasPrefix(kv, "PIX_BROKER_AUTH=") {
			t.Fatalf("pluginEnv(nil) leaked the broker bearer: %q", kv)
		}
	}

	// The broker gets its bearer back — and ONLY the granted value, never the
	// stripped process-global one.
	got := ""
	for _, kv := range pluginEnv([]string{"PIX_BROKER_AUTH=broker-only"}) {
		if strings.HasPrefix(kv, "PIX_BROKER_AUTH=") {
			got = kv
		}
	}
	if got != "PIX_BROKER_AUTH=broker-only" {
		t.Fatalf("broker env = %q, want the explicitly-granted value only", got)
	}
}

// TestPluginEnvAllowlistStripsSecrets verifies the full allowlist contract:
// sensitive host env vars that are NOT in the allowlist must not pass through
// to plugin subprocesses, regardless of their name. This covers the broader
// security guarantee introduced by the allowlist refactor of pluginEnv() (M-3).
func TestPluginEnvAllowlistStripsSecrets(t *testing.T) {
	sensitive := map[string]string{
		"AWS_ACCESS_KEY_ID":     "AKIAIOSFODNN7EXAMPLE",
		"AWS_SECRET_ACCESS_KEY": "wJalrXUtnFEMI/K7MDENG",
		"GITHUB_TOKEN":          "ghp_example1234",
		"SSH_AUTH_SOCK":         "/tmp/ssh-agent.sock",
		"ANTHROPIC_API_KEY":     "sk-ant-example",
		"OPENAI_API_KEY":        "sk-openai-example",
	}
	for k, v := range sensitive {
		t.Setenv(k, v)
	}
	env := pluginEnv(nil)
	for _, kv := range env {
		for k := range sensitive {
			if strings.HasPrefix(kv, k+"=") {
				t.Errorf("pluginEnv leaked sensitive var %q = %q", k, kv)
			}
		}
	}
	// Allowlisted vars must still pass through.
	t.Setenv("PATH", "/usr/bin:/usr/local/bin")
	pathFound := false
	for _, kv := range pluginEnv(nil) {
		if strings.HasPrefix(kv, "PATH=") {
			pathFound = true
			break
		}
	}
	if !pathFound {
		t.Error("PATH (an allowlisted var) was stripped — allowlist is too aggressive")
	}
}

// --- F7(c): memoryProxyMux reproduces the JSON-RPC contract over a stub -------

// stubStore is a deterministic in-memory MemoryStore for the proxy tests. No
// sqlite, no Ollama, no subprocess.
type stubStore struct{}

func (stubStore) Remember(r plugin.RememberReq) (plugin.RememberResp, error) {
	return plugin.RememberResp{ID: "id-1", Reaffirmed: false}, nil
}
func (stubStore) Recall(r plugin.RecallReq) (plugin.RecallResp, error) {
	return plugin.RecallResp{Hits: []plugin.Hit{{ID: "id-1", Content: "hello", Score: 0.5, Kind: "fact", Durability: "durable", Project: ""}}}, nil
}
func (stubStore) Forget(r plugin.ForgetReq) (plugin.ForgetResp, error) {
	return plugin.ForgetResp{OK: r.ID != ""}, nil
}
func (stubStore) Synthesize(plugin.SynthesizeReq) (plugin.SynthesizeResp, error) {
	return plugin.SynthesizeResp{Merged: 1, Expired: 2}, nil
}
func (stubStore) Promotable(plugin.PromotableReq) (plugin.PromotableResp, error) {
	return plugin.PromotableResp{}, nil
}
func (stubStore) Observe(plugin.ObserveReq) (plugin.ObserveResp, error) {
	return plugin.ObserveResp{Accepted: true}, nil
}
func (stubStore) Stats(string) (plugin.Stats, error) {
	return plugin.Stats{Active: 3, Durable: 2, Perishable: 1}, nil
}
func (stubStore) Health() (plugin.Health, error) {
	return plugin.Health{OK: true, Vector: true, Capture: true, WatcherModel: "stub-model"}, nil
}

func rpcCall(t *testing.T, srv *httptest.Server, method string) map[string]any {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":7,"method":"` + method + `"}`
	res, err := http.Post(srv.URL, "application/json", strings.NewReader(body))
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

func TestMemoryProxyMuxContract(t *testing.T) {
	h := &pluginHolder{}
	h.Set(stubStore{}, nil)
	srv := httptest.NewServer(memoryProxyMux(h))
	defer srv.Close()

	// health: envelope + shape matches the in-process memoryMux().
	resp := rpcCall(t, srv, "health")
	if resp["jsonrpc"] != "2.0" || resp["id"].(float64) != 7 {
		t.Fatalf("bad JSON-RPC envelope: %v", resp)
	}
	result, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("health missing result: %v", resp)
	}
	for _, k := range []string{"ok", "vector", "capture", "watcherModel"} {
		if _, present := result[k]; !present {
			t.Errorf("health result missing %q: %v", k, result)
		}
	}
	if result["watcherModel"] != "stub-model" {
		t.Errorf("health watcherModel = %v, want stub-model", result["watcherModel"])
	}

	// stats: same fields the built-in mux emits.
	sresult, _ := rpcCall(t, srv, "stats")["result"].(map[string]any)
	for _, k := range []string{"active", "durable", "perishable", "facts", "learnings", "deleted"} {
		if _, present := sresult[k]; !present {
			t.Errorf("stats result missing %q: %v", k, sresult)
		}
	}

	// method-not-found path mirrors jsonrpcMux/memoryMux.
	nf := rpcCall(t, srv, "does-not-exist")
	e, ok := nf["error"].(map[string]any)
	if !ok || e["code"].(float64) != -32601 {
		t.Errorf("unknown method should yield -32601, got %v", nf)
	}
}
