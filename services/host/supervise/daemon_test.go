package supervise

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// daemon_test.go — behavioural tests for the LAUNCH path of a supervised daemon.
//
// This file exists because that path had none, and it shipped two bugs in a row
// that no unit test could have missed:
//
//   1. UnitSpec.Validate refused the PATH-command form, so snow-proxy could
//      never start at all. packinfo validated the manifest, the health probe had
//      tests, Tree.AddDaemon had tests — and the thing that actually execs the
//      binary was covered by nothing.
//   2. The first health check was budgeted against HealthTimeout (3s) instead of
//      the startup budget, so a daemon taking a normal ~3.5s cold start lost a
//      coin flip and was dropped from the tree.
//
// Both are shape errors in the launch contract, so the tests here drive a REAL
// child process. A mock would have agreed with the broken code.

// fakeDaemon writes a tiny shell script that behaves like a daemon: it binds a
// port after `delay`, then serves a fixed HTTP 200 on any path until killed.
//
// It is a script rather than a Go fixture binary because what is under test is
// exec + PATH resolution + process groups — properties of a real OS process,
// which is exactly what the production bugs were about.
func fakeDaemon(t *testing.T, dir, name string, delay time.Duration, opts ...func(*daemonScript)) string {
	t.Helper()
	sc := &daemonScript{delay: delay}
	for _, o := range opts {
		o(sc)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(sc.render()), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

type daemonScript struct {
	delay time.Duration
	// wedgeAfter, if set, makes the daemon STOP answering after this long while
	// keeping its pid and its listening socket — the failure launchd cannot see.
	wedgeAfter time.Duration
	// spawnChild makes the daemon fork a long-lived grandchild, to prove the stop
	// path signals the whole process group rather than just the parent.
	spawnChild string
}

// render emits a python3 daemon rather than a bash one, because the WEDGE case
// cannot be expressed with netcat: `nc -l` serves one connection and exits, so
// a bash fixture "wedges" by dying — which the supervisor catches on the
// process-exited path and the test passes without ever exercising eviction.
// (It did. That is why this is python.) A real wedge has to hold its pid AND its
// listening socket while answering nothing, which is the failure launchd cannot
// see and the only reason this runtime beats a LaunchAgent.
func (s *daemonScript) render() string {
	var b strings.Builder
	b.WriteString("#!/usr/bin/env python3\n")
	b.WriteString("import socket, subprocess, sys, time\n")
	// Flags are accepted and mostly ignored: a real daemon takes them, and the
	// launch path must pass them through without the child needing to care.
	b.WriteString("port = 11599\n")
	b.WriteString("a = sys.argv[1:]\n")
	b.WriteString("for i, v in enumerate(a):\n")
	b.WriteString("    if v == '--port': port = int(a[i+1])\n")
	if s.spawnChild != "" {
		// A long-lived grandchild that only a PROCESS-GROUP signal reaches.
		fmt.Fprintf(&b, "c = subprocess.Popen(['sleep', '600'])\n")
		fmt.Fprintf(&b, "open(%q, 'w').write(str(c.pid))\n", s.spawnChild)
	}
	fmt.Fprintf(&b, "time.sleep(%.2f)\n", s.delay.Seconds())
	b.WriteString("srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)\n")
	b.WriteString("srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)\n")
	b.WriteString("srv.bind(('127.0.0.1', port)); srv.listen(16)\n")
	if s.wedgeAfter > 0 {
		fmt.Fprintf(&b, "wedge_at = time.time() + %.2f\n", s.wedgeAfter.Seconds())
	} else {
		b.WriteString("wedge_at = None\n")
	}
	b.WriteString("held = []\n")
	b.WriteString("while True:\n")
	b.WriteString("    conn, _ = srv.accept()\n")
	// Wedged: accept, then hold the connection open forever and answer nothing.
	// The pid lives, the socket stays bound, every probe times out.
	b.WriteString("    if wedge_at is not None and time.time() >= wedge_at:\n")
	b.WriteString("        held.append(conn)\n")
	b.WriteString("        continue\n")
	b.WriteString("    try:\n")
	b.WriteString("        conn.recv(4096)\n")
	b.WriteString("        conn.sendall(b'HTTP/1.1 200 OK\\r\\nContent-Length: 2\\r\\nConnection: close\\r\\n\\r\\nok')\n")
	b.WriteString("    except Exception:\n")
	b.WriteString("        pass\n")
	b.WriteString("    conn.close()\n")
	return b.String()
}

// freePort asks the kernel for a port nobody is using, so parallel tests and a
// developer's own running snow-proxy cannot collide.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func daemonTree(t *testing.T, mutate func(*Budgets)) (*Tree, string) {
	t.Helper()
	dir := t.TempDir()
	b := testBudgets()
	if mutate != nil {
		mutate(&b)
	}
	tr := NewTree(Config{
		StageDir: filepath.Join(dir, "stage"),
		StateDir: filepath.Join(dir, "state"),
		Budgets:  b,
		Logf:     func(string, ...any) {},
	})
	ctx, cancel := context.WithCancel(context.Background())
	tr.Start(ctx)
	t.Cleanup(func() { tr.Stop(); cancel() })
	return tr, dir
}

func daemonStatus(t *testing.T, tr *Tree, name string) UnitStatus {
	t.Helper()
	st, ok := tr.Unit(name)
	if !ok {
		t.Fatalf("unit %q is not in the tree", name)
	}
	return st
}

// TestDaemonStartsFromAPathCommand is the regression for bug (1): the whole
// feature was reachable up to the last gate, where the unit could not describe
// the binary it was asked to run.
func TestDaemonStartsFromAPathCommand(t *testing.T) {
	tr, dir := daemonTree(t, nil)
	bin := filepath.Dir(fakeDaemon(t, t.TempDir(), "fake-proxy", 0))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	_ = dir
	port := freePort(t)

	err := tr.AddDaemon(DaemonSpec{
		Unit: UnitSpec{
			Name: "fake-proxy", Kind: "daemon", Command: "fake-proxy",
			Argv: []string{"--connection", "pix", "--port", fmt.Sprint(port)},
			// PATH must be inherited or exec of a script cannot find bash.
			EnvAllow: []string{"PATH"},
		},
		Port: port, Listen: "127.0.0.1", Health: "/health",
	})
	if err != nil {
		t.Fatalf("a PATH-command daemon must start: %v", err)
	}
	st := daemonStatus(t, tr, "fake-proxy")
	if st.State != UnitRunning || !st.HealthOK {
		t.Errorf("state=%s health=%v, want running+healthy", st.State, st.HealthOK)
	}
	if st.PID == 0 {
		t.Error("a running daemon must report its pid")
	}
	// A real HTTP GET over a socket is never 0us. Leaving this unset made
	// `pix serve status` print probe=0us, which reads as "instant" rather than
	// "never measured" — and the latency of a probe that timed out is the number
	// that explains the eviction after it.
	if st.LastProbeUS <= 0 {
		t.Errorf("probe latency is %dus; an HTTP health check cannot take zero time", st.LastProbeUS)
	}
}

// TestDaemonSlowStartIsNotAFailure is the regression for bug (2), and it is the
// one that matters to a first-run user.
//
// The daemon here takes LONGER than a single health probe is allowed to take,
// and less than the startup budget. That is not a slow daemon, it is a normal
// one: snow-proxy measured ~1.5s warm and ~3.5s cold against a 3s per-probe
// timeout, so a correct install failed nondeterministically — and a failed start
// removes the unit from the tree, which the user meets as "the warehouse is
// down".
func TestDaemonSlowStartIsNotAFailure(t *testing.T) {
	tr, _ := daemonTree(t, func(b *Budgets) {
		b.HealthTimeout = 200 * time.Millisecond // one probe is quick...
		b.Handshake = 10 * time.Second           // ...but starting up is allowed to take a while
	})
	bin := filepath.Dir(fakeDaemon(t, t.TempDir(), "slow-proxy", 1500*time.Millisecond))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	port := freePort(t)

	if err := tr.AddDaemon(DaemonSpec{
		Unit: UnitSpec{Name: "slow-proxy", Kind: "daemon", Command: "slow-proxy",
			Argv: []string{"--port", fmt.Sprint(port)}, EnvAllow: []string{"PATH"}},
		Port: port, Listen: "127.0.0.1", Health: "/health",
	}); err != nil {
		t.Fatalf("a daemon slower than one probe but faster than the startup budget must start: %v", err)
	}
	if st := daemonStatus(t, tr, "slow-proxy"); st.State != UnitRunning || !st.HealthOK {
		t.Errorf("state=%s health=%v, want running+healthy", st.State, st.HealthOK)
	}
}

// TestDaemonMissingCommandIsPermanent: a binary that is not installed is
// OPERATOR state. Restarting it on a loop would spin forever and bury the one
// message that fixes it, so the subtree must stop and name the command.
func TestDaemonMissingCommandIsPermanent(t *testing.T) {
	tr, _ := daemonTree(t, nil)
	port := freePort(t)
	err := tr.AddDaemon(DaemonSpec{
		Unit: UnitSpec{Name: "absent-proxy", Kind: "daemon", Command: "pix-no-such-daemon-binary",
			EnvAllow: []string{"PATH"}},
		Port: port, Listen: "127.0.0.1", Health: "tcp",
	})
	if err == nil {
		t.Fatal("a missing binary must fail the add, not report a running daemon")
	}
	if !strings.Contains(err.Error(), "pix-no-such-daemon-binary") {
		t.Errorf("the error must name the missing command, got: %v", err)
	}
	if !strings.Contains(err.Error(), "PATH") {
		t.Errorf("the error must say it was looked for on PATH, got: %v", err)
	}
}

// TestDaemonNeverBindingFailsWithinTheBudget: a daemon that never serves must
// not be reported as running, and the failure has to arrive on a schedule the
// caller set rather than hanging `pix serve` forever.
func TestDaemonNeverBindingFailsWithinTheBudget(t *testing.T) {
	tr, _ := daemonTree(t, func(b *Budgets) {
		b.Handshake = 1200 * time.Millisecond
		b.HealthTimeout = 200 * time.Millisecond
	})
	// Binds far later than the budget allows.
	bin := filepath.Dir(fakeDaemon(t, t.TempDir(), "never-proxy", 60*time.Second))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	port := freePort(t)

	start := time.Now()
	err := tr.AddDaemon(DaemonSpec{
		Unit: UnitSpec{Name: "never-proxy", Kind: "daemon", Command: "never-proxy",
			Argv: []string{"--port", fmt.Sprint(port)}, EnvAllow: []string{"PATH"}},
		Port: port, Listen: "127.0.0.1", Health: "/health",
	})
	if err == nil {
		t.Fatal("a daemon that never binds must not be reported as started")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Errorf("the failure took %v; it must be bounded by the budgets", elapsed)
	}
	if !strings.Contains(err.Error(), "healthy") {
		t.Errorf("the error should say it never became healthy, got: %v", err)
	}
}

// TestDaemonWedgedIsEvicted is the property that justifies this whole runtime
// over the LaunchAgent it replaced.
//
// launchd's KeepAlive restarts a process that EXITS. It has no opinion about one
// that holds its pid and its socket and answers nothing — which is the failure
// that actually happened to snow-proxy, and which the in-sandbox wrapper
// experiences as a hang. The supervisor must notice and replace it.
func TestDaemonWedgedIsEvicted(t *testing.T) {
	tr, _ := daemonTree(t, func(b *Budgets) {
		b.HealthInterval = 100 * time.Millisecond
		b.HealthTimeout = 300 * time.Millisecond
		b.HealthFailures = 2
		b.Handshake = 5 * time.Second
	})
	bin := filepath.Dir(fakeDaemon(t, t.TempDir(), "wedge-proxy", 0, func(s *daemonScript) {
		s.wedgeAfter = 1 * time.Second
	}))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	port := freePort(t)

	if err := tr.AddDaemon(DaemonSpec{
		Unit: UnitSpec{Name: "wedge-proxy", Kind: "daemon", Command: "wedge-proxy",
			Argv: []string{"--port", fmt.Sprint(port)}, EnvAllow: []string{"PATH"}},
		Port: port, Listen: "127.0.0.1", Health: "/health",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}

	// It wedges, so the supervisor must eventually record a NEW generation: the
	// old process was stopped and replaced, not left holding the port.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if st := daemonStatus(t, tr, "wedge-proxy"); st.Generations > 1 || st.Restarts > 0 {
			return // replaced
		}
		time.Sleep(100 * time.Millisecond)
	}
	st := daemonStatus(t, tr, "wedge-proxy")
	t.Errorf("a wedged daemon was never replaced (generations=%d restarts=%d health=%v); "+
		"this is the exact failure launchd could not see, so the runtime buys nothing",
		st.Generations, st.Restarts, st.HealthOK)
}

// TestDaemonStopKillsTheProcessGroup: snow-proxy execs the vendor CLI per
// request. Signalling only the parent leaves those behind, and a survivor that
// still holds the port makes the REPLACEMENT fail to bind — turning one dead
// daemon into a permanently dead capability.
func TestDaemonStopKillsTheProcessGroup(t *testing.T) {
	tr, dir := daemonTree(t, nil)
	pidFile := filepath.Join(dir, "grandchild.pid")
	bin := filepath.Dir(fakeDaemon(t, t.TempDir(), "group-proxy", 0, func(s *daemonScript) {
		s.spawnChild = pidFile
	}))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	port := freePort(t)

	if err := tr.AddDaemon(DaemonSpec{
		Unit: UnitSpec{Name: "group-proxy", Kind: "daemon", Command: "group-proxy",
			Argv: []string{"--port", fmt.Sprint(port)}, EnvAllow: []string{"PATH"}},
		Port: port, Listen: "127.0.0.1", Health: "/health",
	}); err != nil {
		t.Fatalf("start: %v", err)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("the fixture did not record its grandchild pid: %v", err)
	}
	var gpid int
	if _, err := fmt.Sscan(strings.TrimSpace(string(raw)), &gpid); err != nil || gpid <= 0 {
		t.Fatalf("unusable grandchild pid %q: %v", raw, err)
	}

	// Liveness is signal 0, NOT os.FindProcess + Process.Signal(nil): on Unix
	// FindProcess never fails and Signal(nil) returns "unsupported signal type"
	// for every pid alive or dead, so that spelling reports "reaped" instantly
	// and the test passes with the group-kill deleted. It did.
	alive := func() bool { return syscall.Kill(gpid, 0) == nil }
	if !alive() {
		t.Fatal("the grandchild was already gone before the stop; the test proves nothing")
	}

	tr.Stop()

	// The grandchild must be gone, not merely orphaned.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !alive() {
			return // reaped with the group
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("grandchild pid %d survived the stop; a survivor holding the port makes the next start fail to bind", gpid)
}
