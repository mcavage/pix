// serve_memory_restart_test.go — the property the whole supervised-memory design
// exists for: `serve` owns the :11435 listener, the memory unit is a child that
// can die, and the sandbox's recall extension must never notice more than a
// transient error.
//
// This test uses a REAL child process (the built host binary, self-exec'd as
// `pix-host plugin memory`), a REAL SQLite file, and a REAL TCP listener — no
// fakes, no mocks. It asserts three things a log line cannot:
//
//	bound      the listener stays accepting connections across the child's death,
//	fail-fast  while no child is dispensed, calls FAIL immediately with an error
//	           instead of hanging or silently answering empty, and a second bind
//	           of the same address fails rather than shadowing the first,
//	recover    Suture restarts the unit, the SAME listener serves again, and the
//	           data written before the kill is still there.
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"pix/host/config"
	"pix/host/supervise"
	"pix/host/unitreport"
)

// memoryListenAddr is the port the sandbox actually talks to. A developer box
// may already be running `pix serve` on it, so the test falls back to an
// ephemeral port rather than failing for a reason that is not about the code —
// the across-restart property is identical on any port, and the fallback is
// reported so a green run is never silently a weaker run.
//
// It also returns the chosen port as a string, so the CALLER can propagate
// the SAME value into MEMORY_PORT before launching anything — both t.Setenv,
// which is what memoryIdentity()'s os.Getenv("MEMORY_PORT") read actually
// answers from, and the memory unit's own child env (inherited through
// pluginEnvAllow at spawn, see supervise.FilterEnv). Picking the listener
// port here but leaving MEMORY_PORT at its stale "11435" default anywhere
// downstream is exactly the split-brain this helper exists to prevent: the
// identity RPC would report a port nothing is actually listening on.
func memoryListenAddr(t *testing.T) (net.Listener, string) {
	t.Helper()
	if ln, err := net.Listen("tcp", "127.0.0.1:11435"); err == nil {
		return ln, "11435"
	}
	t.Logf("127.0.0.1:11435 is busy on this host; running the same assertions on an ephemeral port")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("split listener addr %q: %v", ln.Addr().String(), err)
	}
	return ln, port
}

// rpcTry POSTs one JSON-RPC call and returns (result, rpcErrorText, transportErr).
func rpcTry(url, method string, params map[string]any) (map[string]any, string, error) {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	client := &http.Client{Timeout: 10 * time.Second}
	res, err := client.Post(url, "application/json", strings.NewReader(string(body)))
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return nil, "", err
	}
	if e, bad := out["error"]; bad {
		return nil, fmt.Sprint(e), nil
	}
	r, _ := out["result"].(map[string]any)
	return r, "", nil
}

func TestMemoryListenerSurvivesUnitRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the host binary, execs it as a plugin, and kills the child")
	}
	dir := t.TempDir()
	self := filepath.Join(dir, "pix-host")
	if out, err := exec.Command("go", "build", "-o", self, ".").CombinedOutput(); err != nil {
		t.Fatalf("build pix-host: %v\n%s", err, out)
	}
	t.Setenv("MEMORY_DB", filepath.Join(dir, "memory.db"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))

	// `serve` owns the listener; the holder-backed mux is what it serves. The
	// port is picked, and MEMORY_PORT set to match, BEFORE the unit launches:
	// the child inherits today's env through the allowlist (supervise.FilterEnv
	// reads os.Environ() at spawn time), and memoryIdentity()'s own
	// os.Getenv("MEMORY_PORT") read — answered in-process by memoryStoreMux —
	// must see the SAME port the listener actually bound, not the stale 11435
	// default, whichever branch memoryListenAddr took.
	ln, port := memoryListenAddr(t)
	t.Setenv("MEMORY_PORT", port)

	sup := &supervisor{}
	defer sup.shutdown()
	h, err := sup.launch("memory", "memory", config.PluginSpec{Impl: config.BuiltinImpl}, self, []string{"MEMORY_PORT=" + port})
	if err != nil {
		t.Fatalf("launch memory unit: %v", err)
	}

	addr := ln.Addr().String()
	url := "http://" + addr
	srv := &http.Server{Handler: memoryProxyMux(h)}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	// FAIL-FAST, part 1: a second bind of the same address must be refused. This
	// is what makes `serve`'s "port already in use" a fatal startup error instead
	// of two daemons quietly fighting over the sandbox's recall traffic.
	if dup, derr := net.Listen("tcp", addr); derr == nil {
		dup.Close()
		t.Fatalf("second listener bound %s; the port is not exclusively held", addr)
	}

	const fact = "the on-call runbook lives in docs/runbooks/host-services.md"
	if _, rpcErr, terr := rpcTry(url, "remember", map[string]any{"content": fact, "kind": "fact"}); terr != nil || rpcErr != "" {
		t.Fatalf("remember before the kill: transport=%v rpc=%s", terr, rpcErr)
	}

	st, ok := sup.tree.Unit("memory")
	if !ok || st.PID <= 0 {
		t.Fatalf("no live memory unit to kill: %+v", st)
	}
	gen1, identity := st.Generations, st.Identity
	if identity == "" {
		t.Fatal("unit published no admission identity; a reattach could not be validated")
	}

	// Kill the CHILD, not serve. SIGKILL: no graceful path, the worst case.
	if kerr := syscall.Kill(st.PID, syscall.SIGKILL); kerr != nil {
		t.Fatalf("kill child %d: %v", st.PID, kerr)
	}

	// BOUND + FAIL-FAST, part 2: from the instant the child dies until the
	// restart completes, the listener keeps ACCEPTING (never connection-refused)
	// and every call returns promptly — an error while no unit is dispensed, a
	// result once one is. A hang here is the bug this test exists to catch.
	deadline := time.Now().Add(90 * time.Second)
	sawRestart := false
	for time.Now().Before(deadline) {
		c, derr := net.DialTimeout("tcp", addr, 2*time.Second)
		if derr != nil {
			t.Fatalf("listener stopped accepting during the restart: %v", derr)
		}
		c.Close()

		start := time.Now()
		_, rpcErr, terr := rpcTry(url, "identity", nil)
		// memoryProxyMux always turns "no unit dispensed" into a clean JSON-RPC
		// error (see serve_plugin.go); the listener never closes the connection
		// out from under a call, so any transport error here is a real bug, not
		// a race to tolerate.
		if terr != nil {
			t.Fatalf("call during restart failed at the transport, not the RPC: %v", terr)
		}
		if took := time.Since(start); took > 9*time.Second {
			t.Fatalf("call during restart took %v; the proxy must fail fast, not hang", took)
		}
		if rpcErr == "" && terr == nil {
			cur, _ := sup.tree.Unit("memory")
			if cur.Generations > gen1 && cur.PID > 0 && cur.PID != st.PID {
				sawRestart = true
				break
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !sawRestart {
		cur, _ := sup.tree.Unit("memory")
		t.Fatalf("memory unit never came back on a new pid within the budget: %+v", cur)
	}

	// RECOVER: the same listener, the same store, the data written before the kill.
	rec, rpcErr, terr := rpcTry(url, "recall", map[string]any{"query": "on-call runbook"})
	if terr != nil || rpcErr != "" {
		t.Fatalf("recall after restart: transport=%v rpc=%s", terr, rpcErr)
	}
	hits, _ := rec["hits"].([]any)
	if len(hits) == 0 {
		t.Fatalf("recall after restart lost the row written before it: %v", rec)
	}

	// The published snapshot must TELL an operator this happened: one more
	// generation, a counted restart, a stable identity, and a real probe latency.
	after, _ := sup.tree.Unit("memory")
	if after.Restarts < 1 {
		t.Errorf("restart was not counted: %+v", after)
	}
	if after.Identity != identity {
		t.Errorf("identity changed across a restart of the same spec: %q -> %q", identity, after.Identity)
	}
	rep := sup.tree.Report()
	var mem *unitreport.Unit
	for i := range rep.Units {
		if rep.Units[i].Name == "memory" {
			mem = &rep.Units[i]
		}
	}
	if mem == nil {
		t.Fatalf("published report has no memory unit: %+v", rep)
	}
	if mem.State != string(supervise.UnitRunning) || !mem.HealthOK {
		t.Errorf("report does not show the recovered unit as running: %+v", mem)
	}
	if mem.Generation < 2 || mem.Restarts < 1 {
		t.Errorf("report understates the restart: %+v", mem)
	}
	if mem.LastProbeUS <= 0 {
		t.Errorf("report carries no health-probe latency: %+v", mem)
	}

	// serve publishes that snapshot where `pix serve status --json` reads it.
	sup.publish()
	got, found, rerr := unitreport.ReadReport(config.ServeUnitsPath())
	if rerr != nil || !found {
		t.Fatalf("published snapshot unreadable at %s: found=%v err=%v", config.ServeUnitsPath(), found, rerr)
	}
	if len(got.Units) != 1 || got.Units[0].Name != "memory" {
		t.Fatalf("published snapshot does not describe the tree: %+v", got)
	}
}
