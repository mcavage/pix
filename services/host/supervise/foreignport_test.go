package supervise

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// foreignport_test.go — the six-hour bug.
//
// An orphaned snow-proxy held :11442 after a `pkill` of its supervisor (Setpgid
// puts the daemon in its own process group, so it outlived the parent). Every
// supervised start then went: child launches, cannot bind, exits 1 — while
// waitHealthy dialled the port, got the ORPHAN's 200, and declared the unit
// healthy. 4,057 restarts. `pix doctor` reported `daemons ✓ 1 answering` the
// entire time, because something genuinely was answering.
//
// A port probe cannot distinguish "my child is serving" from "someone else owns
// this port". The fix is to believe a passing probe only while the child lives.

// foreignListener serves 200 on a port, standing in for the orphan.
func foreignListener(t *testing.T, port int) {
	t.Helper()
	srv := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", port),
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }),
	}
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		t.Fatalf("could not take the port for the test: %v", err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = ln.Close() })
}

// TestDaemonRefusesWhenAnotherProcessOwnsThePort is the regression.
func TestDaemonRefusesWhenAnotherProcessOwnsThePort(t *testing.T) {
	port := freePort(t)
	foreignListener(t, port) // the orphan, already answering

	tr, _ := daemonTree(t, func(b *Budgets) { b.Handshake = 5 * time.Second })
	// A daemon that exits immediately, exactly as one that cannot bind does.
	dir := t.TempDir()
	exe := dir + "/doomed-proxy"
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	err := tr.AddDaemon(DaemonSpec{
		Unit: UnitSpec{Name: "doomed-proxy", Kind: "daemon", Command: "doomed-proxy",
			EnvAllow: []string{"PATH"}},
		Port: port, Listen: "127.0.0.1", Health: "/health",
	})
	if err == nil {
		t.Fatal("a daemon whose child died must NOT be reported as started, " +
			"however healthy the port looks — this is the 4,057-restart loop")
	}
	// And the message must name the actual problem, or the next person spends six
	// hours on it too: WHICH address, WHO owns it, and why the health check is
	// not to be believed.
	for _, want := range []string{"already being served by another process", "fail to bind", fmt.Sprint(port)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must say %q so it is actionable; got: %v", want, err)
		}
	}
	// And it must be PERMANENT. Restarting into a port somebody else owns is the
	// loop itself; the unit has to stop and stay stopped.
	if !strings.Contains(err.Error(), "should not be restarted") {
		t.Errorf("this is operator state, not a transient fault; it must not restart: %v", err)
	}
}

// TestDaemonStartupExitWithNoListenerReportsPlainly: when the port is genuinely
// free and the daemon just dies, do NOT accuse an innocent third party.
func TestDaemonStartupExitWithNoListenerReportsPlainly(t *testing.T) {
	port := freePort(t)
	tr, _ := daemonTree(t, func(b *Budgets) { b.Handshake = 3 * time.Second })
	dir := t.TempDir()
	exe := dir + "/quick-exit"
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	err := tr.AddDaemon(DaemonSpec{
		Unit: UnitSpec{Name: "quick-exit", Kind: "daemon", Command: "quick-exit",
			EnvAllow: []string{"PATH"}},
		Port: port, Listen: "127.0.0.1", Health: "/health",
	})
	if err == nil {
		t.Fatal("a daemon that exits during startup must fail the add")
	}
	if !strings.Contains(err.Error(), "exited during startup") {
		t.Errorf("got: %v", err)
	}
	if strings.Contains(err.Error(), "another process owns") {
		t.Errorf("no other process owns this port; the error must not claim one:\n%v", err)
	}
}

// TestDaemonStillStartsNormally: the child-liveness check must not break the
// ordinary case, where the daemon binds a moment after launch.
func TestDaemonStillStartsNormally(t *testing.T) {
	tr, _ := daemonTree(t, func(b *Budgets) { b.Handshake = 10 * time.Second })
	bin := t.TempDir()
	fakeDaemon(t, bin, "normal-proxy", 400*time.Millisecond)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	port := freePort(t)

	if err := tr.AddDaemon(DaemonSpec{
		Unit: UnitSpec{Name: "normal-proxy", Kind: "daemon", Command: "normal-proxy",
			Argv: []string{"--port", fmt.Sprint(port)}, EnvAllow: []string{"PATH"}},
		Port: port, Listen: "127.0.0.1", Health: "/health",
	}); err != nil {
		t.Fatalf("a healthy daemon must still start: %v", err)
	}
	if st := daemonStatus(t, tr, "normal-proxy"); st.State != UnitRunning || !st.HealthOK {
		t.Errorf("state=%s health=%v", st.State, st.HealthOK)
	}
}
