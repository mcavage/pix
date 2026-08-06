package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

// --- F5: an external plugin unit refuses to launch unless the bytes match -----

// TestLaunchRefusesUnpinnedAndMismatchedExternal drives the REAL refusal path:
// `launch` no longer pre-hashes the configured path (supervise owns that gate at
// both ends), so this proves the two refusals still happen through it — an
// unpinned external path dies at spec validation, before anything is spawned,
// and a pinned path whose bytes do not match dies in the stager, which is the
// check that actually precedes exec.
func TestLaunchRefusesUnpinnedAndMismatchedExternal(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	dir := t.TempDir()
	bin := filepath.Join(dir, "fake-plugin")
	if err := os.WriteFile(bin, []byte("not a real plugin"), 0o755); err != nil {
		t.Fatal(err)
	}

	sup := &supervisor{}
	t.Cleanup(sup.shutdown)

	// Unpinned: refused at spec time, no subprocess, no staging.
	h, err := sup.launch("unpinned", "memory", config.PluginSpec{Path: bin}, "", nil)
	if err == nil || !strings.Contains(err.Error(), "unpinned") {
		t.Fatalf("unpinned external plugin: err = %v, want an unpinned refusal", err)
	}
	if h != nil {
		t.Errorf("launch returned a holder for a refused unit: %v", h)
	}

	// Pinned to the WRONG bytes: the stager re-hashes and refuses.
	wrong := hex.EncodeToString(sha256.New().Sum(nil)) // sha256 of the empty input
	h, err = sup.launch("mismatch", "memory", config.PluginSpec{Path: bin, SHA: wrong}, "", nil)
	if err == nil || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("mismatched external plugin: err = %v, want a sha256 mismatch refusal", err)
	}
	if h != nil {
		t.Errorf("launch returned a holder for a refused unit: %v", h)
	}
}

// --- F2: a plugin subprocess env never contains the broker bearer ------------

// pluginEnv is the exact composition prod uses: every unit is launched with
// EnvAllow: pluginEnvAllow, which supervise.FilterEnv applies to the child's
// environment.
func pluginEnv(extra []string) []string {
	return supervise.FilterEnv(pluginEnvAllow, extra)
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

// --- R1: a losing second `serve` must never delete the WINNER's pidfile -----

// TestRemoveOwnedPidFileOnlyDeletesOurOwnPid is the regression for the
// compare-and-delete contract removeServePidFile/removeServeLazyMarker both
// run through: the file is deleted ONLY when it currently names THIS
// process's pid — never whatever pid happens to be sitting there, which is
// what a bare os.Remove (the old behavior) did regardless of content.
func TestRemoveOwnedPidFileOnlyDeletesOurOwnPid(t *testing.T) {
	t.Run("a foreign pid is left untouched", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "serve.pid")
		foreign := os.Getpid() + 1 // guaranteed != os.Getpid(); need not exist
		if err := os.WriteFile(path, []byte(strconv.Itoa(foreign)+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		removeOwnedPidFile(path)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("a foreign-owned pidfile was deleted: %v", err)
		}
		if strings.TrimSpace(string(raw)) != strconv.Itoa(foreign) {
			t.Fatalf("pidfile content changed: %q", raw)
		}
	})

	t.Run("our own pid is deleted", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "serve.pid")
		if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		removeOwnedPidFile(path)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("our own pidfile was not removed: err=%v", err)
		}
	})

	t.Run("a missing file is a silent no-op", func(t *testing.T) {
		removeOwnedPidFile(filepath.Join(t.TempDir(), "does-not-exist"))
	})

	t.Run("garbage content is left untouched", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "serve.pid")
		if err := os.WriteFile(path, []byte("not-a-pid\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		removeOwnedPidFile(path)
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("unparseable pidfile content was still deleted: %v", err)
		}
	})
}

// TestServeCleanupDoesNotClobberARunningWinner reproduces the race at the
// config-path level runServe's deferred cleanup actually runs under: a
// WINNER writes serve.pid/serve.lazy naming its own (different) pid, then a
// LOSER runs the exact cleanup sequence runServe defers on its way out
// (removeServePidFile, removeServeLazyMarker, called as this test process).
// The winner's files must survive.
func TestServeCleanupDoesNotClobberARunningWinner(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(config.ServePidPath()), 0o700); err != nil {
		t.Fatal(err)
	}
	winner := os.Getpid() + 1000 // stands in for a DIFFERENT, still-running process
	if err := os.WriteFile(config.ServePidPath(), []byte(strconv.Itoa(winner)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.ServeLazyMarkerPath(), []byte(strconv.Itoa(winner)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The LOSER's own cleanup — exactly what runServe's defers/exit path call.
	removeServePidFile()
	removeServeLazyMarker()

	raw, err := os.ReadFile(config.ServePidPath())
	if err != nil {
		t.Fatalf("winner's pidfile was deleted by a losing serve's cleanup: %v", err)
	}
	if strings.TrimSpace(string(raw)) != strconv.Itoa(winner) {
		t.Fatalf("winner's pidfile content changed: %q", raw)
	}
	if _, err := os.Stat(config.ServeLazyMarkerPath()); err != nil {
		t.Fatalf("winner's lazy marker was deleted by a losing serve's cleanup: %v", err)
	}
}

// --- R2: every real memory RPC runs through Holder.Use, so Drain sees it ----

// blockingStore wraps stubStore, blocking inside Recall (closing `started`
// once the handler is actually running, so the test knows the call is truly
// in flight) until `unblock` is closed.
type blockingStore struct {
	stubStore
	started chan struct{}
	unblock chan struct{}
}

func (b blockingStore) Recall(r plugin.RecallReq) (plugin.RecallResp, error) {
	close(b.started)
	<-b.unblock
	return b.stubStore.Recall(r)
}

// TestMemoryProxyMuxRoutesRPCsThroughHolderUse is the regression for routing
// every real memory RPC through Holder.Use rather than a bare Get(): before
// that fix, memoryProxyMux resolved the dispensed client and returned
// immediately, so Holder's in-flight accounting (what Drain waits on before a
// stop kills the child) never saw an HTTP call that was still running — a
// shutdown's drain could report "clean" with a sandbox request still
// in-flight. This blocks a real call inside the handler and proves Drain
// (given a short budget) reports NOT clean while it is still running, then
// clean once it actually finishes.
func TestMemoryProxyMuxRoutesRPCsThroughHolderUse(t *testing.T) {
	h := &pluginHolder{}
	started, unblock := make(chan struct{}), make(chan struct{})
	h.Set(blockingStore{started: started, unblock: unblock}, nil)
	srv := httptest.NewServer(memoryProxyMux(h))
	defer srv.Close()

	done := make(chan struct{})
	var postErr error
	go func() {
		defer close(done)
		body := `{"jsonrpc":"2.0","id":1,"method":"recall"}`
		res, err := http.Post(srv.URL, "application/json", strings.NewReader(body))
		if err != nil {
			postErr = err
			return
		}
		res.Body.Close()
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("recall handler never started")
	}

	if h.Drain(200 * time.Millisecond) {
		t.Fatal("Drain reported clean with a memory RPC still in flight — memoryProxyMux is not routing through Holder.Use")
	}

	close(unblock)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the in-flight recall never completed")
	}
	if postErr != nil {
		t.Fatalf("POST /recall: %v", postErr)
	}

	if !h.Drain(2 * time.Second) {
		t.Fatal("Drain did not report clean once the in-flight call finished")
	}
}

// --- R3: runServe must WAIT for monitor's graceful shutdown, not race it ----

// TestServeMonitorAndWaitBlocksUntilGracefulShutdownActuallyCompletes proves
// serveMonitorAndWait's wait() reflects monitor.IngestServer.Serve's REAL
// completion, not just "the goroutine got scheduled": it holds a genuinely
// in-flight /ingest request open across the cancel, so monitor's own
// graceful shutdown (Server.Shutdown, monitor/ingest.go) has real work to
// wait for, and asserts wait() does not return while that work is still
// outstanding, only once the request actually unblocks and Serve(ctx) itself
// returns. Before this wait existed, runServe raced this window: a fatal
// failure elsewhere called os.Exit(1) without ever waiting on it.
func TestServeMonitorAndWaitBlocksUntilGracefulShutdownActuallyCompletes(t *testing.T) {
	root := t.TempDir()
	srv, err := buildMonitorIngest("127.0.0.1", 0, root)
	if err != nil {
		t.Fatalf("buildMonitorIngest: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	fatalCh := make(chan error, 1)
	wait := serveMonitorAndWait(srv, ctx, fatalCh)

	// A real, incomplete request: full headers (so net/http hands it to the
	// handler) declaring a body it never finishes sending, so handleIngest is
	// genuinely blocked reading r.Body when we cancel below.
	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial monitor ingest: %v", err)
	}
	defer conn.Close()
	if _, err := fmt.Fprintf(conn, "POST /ingest HTTP/1.1\r\nHost: %s\r\nContent-Length: 4096\r\nContent-Type: application/x-ndjson\r\n\r\n{\"partial\":true", srv.Addr()); err != nil {
		t.Fatalf("write partial request: %v", err)
	}
	// Give net/http time to parse the headers and hand the request to the
	// handler (handleIngest, now blocked reading the rest of the declared
	// body) BEFORE cancelling — otherwise Shutdown's very first idle-conn
	// sweep can close an accepted-but-not-yet-dispatched connection as idle,
	// which would race this test rather than exercise the wait.
	time.Sleep(100 * time.Millisecond)

	cancel()

	// wait() must NOT return while the handler is still blocked on the
	// in-flight (never completed) request.
	waitDone := make(chan struct{})
	go func() { wait(); close(waitDone) }()
	select {
	case <-waitDone:
		t.Fatal("wait() returned before monitor's Serve(ctx) actually finished shutting down — an in-flight request was still open")
	case <-time.After(300 * time.Millisecond):
	}

	// Let the handler's blocked read fail (EOF), which lets Serve's own
	// graceful shutdown proceed and return.
	conn.Close()

	select {
	case <-waitDone:
	case <-time.After(6 * time.Second):
		t.Fatal("wait() never returned after the in-flight request ended")
	}
	select {
	case err := <-fatalCh:
		t.Fatalf("monitor reported a fatal error on what should be a clean shutdown: %v", err)
	default:
	}
}
