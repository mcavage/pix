package main

// serve_shutdown_test.go proves the two properties this file's listener/
// shutdown ordering rework exists for:
//
//  1. bind-before-spawn: a bound-port conflict on a built-in HTTP front door
//     leaves ZERO child-spawn side effects — no pack unit, no memory plugin
//     subprocess, no pidfile — because bindFrontDoors (Phase 1) cannot spawn
//     anything by construction, and spawnChildren (Phase 2) is only ever
//     reached once every bind in Phase 1 has already succeeded.
//  2. drain-before-teardown: on shutdown, every front door stops accepting
//     and drains its in-flight work FIRST, bounded, before the supervised
//     backend is torn down and before runServe waits on monitor.

import (
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"pix/host/config"
)

// --- bind-before-spawn --------------------------------------------------

// TestBindFrontDoorsReturnsErrorWithoutSpawningAnything is the fast, in-process
// half: bindFrontDoors takes no *supervisor and no packinfo — it is
// STRUCTURALLY unable to spawn a child. A port conflict on memory's front
// door must fail closed, return the zero value, and leave nothing bound.
func TestBindFrontDoorsReturnsErrorWithoutSpawningAnything(t *testing.T) {
	t.Setenv("MEMORY_BIND", "127.0.0.1")
	// Occupy an ephemeral port ourselves, then point MEMORY_PORT at it so
	// bindFrontDoors' own net.Listen is guaranteed to lose the race.
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy a port: %v", err)
	}
	defer occupied.Close()
	_, port, _ := net.SplitHostPort(occupied.Addr().String())
	t.Setenv("MEMORY_PORT", port)

	enabledSvc := func(name string) bool { return name == "memory" }
	fd, berr := bindFrontDoors(enabledSvc, "127.0.0.1", 0)
	if berr == nil {
		t.Fatal("bindFrontDoors: want an error on an occupied memory port, got nil")
	}
	if !strings.Contains(berr.Error(), "bind memory") {
		t.Errorf("bindFrontDoors error = %q, want it to name the memory front door", berr)
	}
	if len(fd.all) != 0 || fd.monitorSrv != nil {
		t.Fatalf("bindFrontDoors returned a non-zero result on failure: %+v", fd)
	}
}

// TestServeProcessSpawnsNoChildOnMemoryPortConflict is the process-level
// proof: the REAL compiled pix-host binary, told to serve only `memory`
// against an already-occupied port. It must exit 1, and — the property that
// matters — never write a pidfile and never publish a supervision-tree
// snapshot, which only happens once a unit is actually launched. If
// spawnChildren ran before the bind failed, the memory plugin subprocess
// would already be up and the units snapshot would exist.
func TestServeProcessSpawnsNoChildOnMemoryPortConflict(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the real pix-host binary; covered by the untimed race/metrics CI jobs")
	}
	bin := buildHostBinary(t)
	stateDir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateDir)

	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("occupy a port: %v", err)
	}
	defer occupied.Close()
	_, port, _ := net.SplitHostPort(occupied.Addr().String())

	cmd := exec.Command(bin, "serve", "memory")
	cmd.Env = append(os.Environ(),
		"XDG_STATE_HOME="+stateDir,
		"MEMORY_BIND=127.0.0.1",
		"MEMORY_PORT="+port,
	)
	out, runErr := cmd.CombinedOutput()
	exit, ok := runErr.(*exec.ExitError)
	if !ok {
		t.Fatalf("expected pix-host serve to exit non-zero on a bind conflict, got err=%v out=%s", runErr, out)
	}
	if exit.ExitCode() != 1 {
		t.Errorf("exit code = %d, want 1 (preserved fatal-exit semantics)\n%s", exit.ExitCode(), out)
	}
	if !strings.Contains(string(out), "bind memory") {
		t.Errorf("stderr = %q, want it to report the memory bind failure", out)
	}

	if _, err := os.Stat(config.ServePidPath()); !os.IsNotExist(err) {
		t.Errorf("serve.pid exists after a bind conflict — pidfile must never be written before every front door binds (err=%v)", err)
	}
	if _, err := os.Stat(config.ServeUnitsPath()); !os.IsNotExist(err) {
		t.Errorf("serve.units.json exists after a bind conflict — no unit (pack or memory) may ever be spawned before every front door binds (err=%v)", err)
	}
}

// --- drain-before-teardown ----------------------------------------------

// TestPerformShutdownOrdersFrontDoorsBeforeBackendBeforeMonitorWait drives a
// REAL http.Server with a genuinely in-flight, blocked request, then calls
// performShutdown with recording stand-ins for the backend teardown and the
// monitor wait. It asserts: the in-flight front-door request is drained to
// completion BEFORE the backend teardown runs, which runs BEFORE the monitor
// wait — and that performShutdown cancelled the monitor context so monitor's
// own Serve(ctx) begins draining at the same time as the front doors, not
// after the backend is already gone.
func TestPerformShutdownOrdersFrontDoorsBeforeBackendBeforeMonitorWait(t *testing.T) {
	var mu sync.Mutex
	var order []string
	record := func(tag string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, tag)
	}

	started := make(chan struct{})
	unblock := make(chan struct{})
	var startOnce sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		startOnce.Do(func() { close(started) })
		<-unblock
		record("frontdoor-drained")
		w.WriteHeader(http.StatusOK)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)

	// A genuinely in-flight request against the front door, started BEFORE
	// shutdown begins.
	reqDone := make(chan struct{})
	var reqErr error
	go func() {
		defer close(reqDone)
		resp, gerr := http.Get("http://" + ln.Addr().String() + "/slow")
		if gerr != nil {
			reqErr = gerr
			return
		}
		resp.Body.Close()
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the /slow handler never started — not genuinely in-flight")
	}

	ctx, cancel := context.WithCancel(context.Background())
	backendDone := make(chan struct{})
	shutdownBackend := func() {
		record("backend")
		close(backendDone)
	}
	monitorWaitDone := make(chan struct{})
	waitMonitor := func() {
		record("monitor-wait")
		close(monitorWaitDone)
	}

	// Unblock the handler shortly after shutdown starts, from a side
	// goroutine — proving performShutdown genuinely WAITS on it rather than
	// racing past the still-in-flight request.
	go func() {
		time.Sleep(100 * time.Millisecond)
		close(unblock)
	}()

	shutdownDone := make(chan struct{})
	go func() {
		performShutdown([]*http.Server{srv}, cancel, shutdownBackend, waitMonitor)
		close(shutdownDone)
	}()

	select {
	case <-shutdownDone:
	case <-time.After(5 * time.Second):
		t.Fatal("performShutdown never returned")
	}
	<-reqDone
	if reqErr != nil {
		t.Fatalf("in-flight request failed instead of draining cleanly: %v", reqErr)
	}

	if ctx.Err() == nil {
		t.Fatal("performShutdown did not cancel the monitor context — monitor's own Serve(ctx) would never start draining")
	}

	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()
	want := []string{"frontdoor-drained", "backend", "monitor-wait"}
	if len(got) != len(want) {
		t.Fatalf("shutdown order = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("shutdown order = %v, want %v", got, want)
		}
	}
}

// TestShutdownFrontDoorsStopsAcceptingImmediately proves the front door
// closes its listener to NEW connections as soon as shutdown starts, rather
// than continuing to accept work while a slow in-flight request (or the
// backend teardown that follows) drains — the actual bug this file fixes
// (memory's front door previously had no Shutdown call at all).
func TestShutdownFrontDoorsStopsAcceptingImmediately(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)

	// Sanity: the front door is actually up before we shut it down.
	if resp, gerr := http.Get("http://" + addr + "/"); gerr != nil {
		t.Fatalf("pre-shutdown GET failed: %v", gerr)
	} else {
		resp.Body.Close()
	}

	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	shutdownFrontDoors([]*http.Server{srv}, cancel)

	// A NEW connection attempt after shutdownFrontDoors has returned must be
	// refused — the listener is closed, not merely draining old work.
	if resp, gerr := http.Get("http://" + addr + "/"); gerr == nil {
		resp.Body.Close()
		t.Fatal("a new connection succeeded after shutdownFrontDoors returned — the front door is still accepting")
	}
}
