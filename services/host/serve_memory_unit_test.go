// serve_memory_unit_test.go — memory is ALWAYS a supervised self-exec plugin
// unit now, so this proves what that could break: the :11435 JSON-RPC surface
// the sandbox extensions call, over a REAL child process and a REAL SQLite
// file. It builds this binary, the tree execs it as `pix-host plugin memory`,
// and asserts against the same handler serve installs.
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
)

// call POSTs one JSON-RPC request and returns the decoded envelope.
func rpcPost(t *testing.T, url, method string, params map[string]any) map[string]any {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	res, err := http.Post(url, "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatalf("%s: bad json: %v", method, err)
	}
	if e, bad := out["error"]; bad {
		t.Fatalf("%s: rpc error: %v", method, e)
	}
	return out
}

func TestMemoryUnitIsSelfExecAndPreservesTheRPCSurface(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the host binary and execs it as a plugin")
	}
	dir := t.TempDir()
	self := filepath.Join(dir, "pix-host")
	if out, err := exec.Command("go", "build", "-o", self, ".").CombinedOutput(); err != nil {
		t.Fatalf("build pix-host: %v\n%s", err, out)
	}
	db := filepath.Join(dir, "memory.db")
	t.Setenv("MEMORY_DB", db)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	sup := &supervisor{}
	defer sup.shutdown()
	// The BUILTIN impl must still be launched as a unit, never in-process.
	h, err := sup.launch("memory", "memory", config.PluginSpec{Impl: config.BuiltinImpl}, self, nil)
	if err != nil {
		t.Fatalf("launch memory unit: %v", err)
	}
	st, ok := sup.tree.Unit("memory")
	if !ok || st.PID <= 0 || st.PID == os.Getpid() {
		t.Fatalf("memory must run in a CHILD process, status = %+v", st)
	}

	srv := httptest.NewServer(memoryProxyMux(h))
	defer srv.Close()

	// remember -> a real row in a real SQLite file.
	rem, _ := rpcPost(t, srv.URL, "remember", map[string]any{
		"content": "the deploy runbook lives in docs/deploy.md", "kind": "fact"})["result"].(map[string]any)
	if id, _ := rem["id"].(string); strings.TrimSpace(id) == "" {
		t.Fatalf("remember returned no id: %v", rem)
	}
	if fi, err := os.Stat(db); err != nil || fi.Size() == 0 {
		t.Fatalf("no sqlite database at %s (%v)", db, err)
	}

	// recall -> the hit shape the extension parses, createdAt included.
	rec, _ := rpcPost(t, srv.URL, "recall", map[string]any{"query": "deploy runbook"})["result"].(map[string]any)
	hits, _ := rec["hits"].([]any)
	if len(hits) == 0 {
		t.Fatalf("recall over the plugin unit found nothing: %v", rec)
	}
	top, _ := hits[0].(map[string]any)
	// durability is deliberately absent from this list: the U9 schema work
	// retired the read thread end-to-end (no caller, host CLI or sandbox
	// extension, ever consumed it again after U4); the DB column stays for
	// on-disk compatibility, but the JSON-RPC response no longer carries it.
	for _, k := range []string{"id", "content", "score", "kind", "project", "createdAt"} {
		if _, present := top[k]; !present {
			t.Errorf("recall hit is missing %q: %v", k, top)
		}
	}
	if ca, _ := top["createdAt"].(string); strings.TrimSpace(ca) == "" {
		t.Errorf("createdAt must be a real timestamp, got %q", top["createdAt"])
	}
	// identity still proves WHO holds :11435.
	ident, _ := rpcPost(t, srv.URL, "identity", nil)["result"].(map[string]any)
	if svc, _ := ident["name"].(string); svc == "" {
		t.Errorf("identity lost its service field: %v", ident)
	}

	// stats honours the `profile` param the extension always sends: a row
	// remembered INTO a profile is counted there and nowhere else.
	rpcPost(t, srv.URL, "remember", map[string]any{"content": "profile-scoped note", "profile": "p1"})
	mine, _ := rpcPost(t, srv.URL, "stats", map[string]any{"profile": "p1"})["result"].(map[string]any)
	theirs, _ := rpcPost(t, srv.URL, "stats", map[string]any{"profile": "p2"})["result"].(map[string]any)
	m, _ := mine["active"].(float64)
	o, _ := theirs["active"].(float64)
	if m <= o {
		t.Errorf("stats is not profile-scoped over the plugin path: p1=%v p2=%v", mine, theirs)
	}

	// health keeps every key the doctor/extension read.
	hres, _ := rpcPost(t, srv.URL, "health", nil)["result"].(map[string]any)
	for _, k := range []string{"ok", "vector", "capture", "captureReason", "watcherModel"} {
		if _, present := hres[k]; !present {
			t.Errorf("health missing %q: %v", k, hres)
		}
	}
	// the remaining method is still routable.
	rpcPost(t, srv.URL, "observe", map[string]any{"user": "hello"})
}
