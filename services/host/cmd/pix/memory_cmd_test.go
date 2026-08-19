package main

// memory_cmd_test.go pins the honesty fix for the 3s EnsureMemoryTimeout
// (architect round 2): `pix memory` used to flatly tell a down daemon's user
// to "start it with `pix serve`", even though withMemory's own EnsureUp call,
// one line above, had already TRIED that — the real cause of a persistent
// ErrServiceDown is at least as often a cold start that outran the 3s budget
// (sqlite init under an advisory flock) as it is a genuinely never-started
// daemon. Telling the user to start "another" one is actively wrong advice in
// the first case, not merely unhelpful.

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"pix/host/cli"
	"pix/host/service"
)

// deadMemoryPort returns a port with nothing listening on it — a connection
// there is REFUSED immediately, so the RPC call fails fast instead of riding
// out its own multi-second client timeout (see rpc.MemoryClient).
func deadMemoryPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return port
}

// TestMemoryCmd_ServiceDownMessage_NeverTellsUserToStartAnotherDaemon runs
// `pix memory recall` for real, through the SAME cli.RunRoot entry point
// production uses, against a daemon that is down and an autostart that is
// opted out (so the test never spawns a real pix-host process). The
// resulting message must be honest about BOTH real causes of a persistent
// ErrServiceDown — a slow autostart still coming up, or a genuinely down
// daemon — and must never flatly say "start it" as if nothing had already
// tried.
func TestMemoryCmd_ServiceDownMessage_NeverTellsUserToStartAnotherDaemon(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("PIX_NO_AUTOSERVE", "1") // never spawn a real pix-host in a unit test
	t.Setenv("MEMORY_PORT", strconv.Itoa(deadMemoryPort(t)))
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("services = [\"memory\"]\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var out, errb bytes.Buffer
	d := &cli.Deps{Out: &out, Err: &errb, In: strings.NewReader("")}
	err := cli.RunRoot[rootCmd]("pix", "", "", []string{"memory", "recall", "anything"}, d)
	if err == nil {
		t.Fatal("recall against a down daemon must fail")
	}

	msg := errb.String()
	if !strings.Contains(msg, "if it just started, wait a moment and retry") {
		t.Errorf("message must acknowledge EnsureUp already tried to bring it up, got %q", msg)
	}
	if strings.Contains(msg, "service unreachable: start it with `pix serve`") {
		t.Errorf("message must never flatly tell the user to start ANOTHER daemon when one may already be starting, got %q", msg)
	}
}

// TestEnsureMemoryTimeoutIsPinnedAt3s locks the deliberately short read-side
// cold-start budget: `pix memory …` is a foreground command a human is
// staring at, so a slow cold start must degrade to one honest, FAST error
// rather than a long silent hang — but the choice of exactly 3s (not some
// other bounded value) is a decision this test pins, so a future change is
// deliberate rather than an accidental widening or narrowing.
func TestEnsureMemoryTimeoutIsPinnedAt3s(t *testing.T) {
	if service.EnsureMemoryTimeout != 3*time.Second {
		t.Errorf("EnsureMemoryTimeout = %s, want 3s (see service/start.go's doc comment for why, and update the honest ErrServiceDown message in memory_cmd.go if this changes)", service.EnsureMemoryTimeout)
	}
}

// fakeMemoryDaemon stands up a tiny JSON-RPC server that answers ONLY the
// "forget" method with a canned {"ok": ...} result, and returns its port —
// wired via MEMORY_PORT, this is enough for withMemory's TCP-dial liveness
// probe to see the daemon as already up, so the test never spawns a real
// pix-host.
func fakeMemoryDaemon(t *testing.T, ok bool) int {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0", "id": 1,
			"result": map[string]any{"ok": ok},
		})
	}))
	t.Cleanup(srv.Close)
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parsing test server port %q: %v", port, err)
	}
	return p
}

// memoryCmdDeps wires a config.toml + MEMORY_PORT so `pix memory forget`
// runs through the SAME cli.RunRoot entry point production uses, against a
// fake daemon, without ever spawning a real pix-host.
func memoryCmdDeps(t *testing.T, port int) *cli.Deps {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("PIX_NO_AUTOSERVE", "1")
	t.Setenv("MEMORY_PORT", strconv.Itoa(port))
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("services = [\"memory\"]\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	var out, errb bytes.Buffer
	return &cli.Deps{Out: &out, Err: &errb, In: strings.NewReader("")}
}

// QA-2: `pix memory forget <missing>` must be exit 1 with the diagnostic on
// stderr — never a quiet exit 0 with the miss buried in stdout.
func TestMemoryCmd_ForgetMiss_Exit1AndStderr(t *testing.T) {
	port := fakeMemoryDaemon(t, false)
	d := memoryCmdDeps(t, port)

	// dispatch, not cli.RunRoot: it is dispatch that owns the single "pix: %v"
	// stderr render for a plain (non-silent) error, exactly as production's
	// main() invokes it.
	code := dispatch([]string{"memory", "forget", "missing123"}, d)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}

	out := d.Out.(*bytes.Buffer).String()
	errb := d.Err.(*bytes.Buffer).String()
	if out != "" {
		t.Errorf("forget miss must write nothing to stdout, got %q", out)
	}
	if !strings.Contains(errb, "missing123") {
		t.Errorf("forget miss diagnostic must land on stderr and name the id, got %q", errb)
	}
}

// The JSON-mode miss keeps stdout parseable (the {"ok":false} result) while
// still failing honestly: exit 1, plus the same diagnostic on stderr.
func TestMemoryCmd_ForgetMissJSON_ParseableStdoutHonestExit(t *testing.T) {
	port := fakeMemoryDaemon(t, false)
	d := memoryCmdDeps(t, port)

	code := dispatch([]string{"memory", "forget", "missing123", "--json"}, d)
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}

	out := d.Out.(*bytes.Buffer).String()
	var parsed map[string]any
	if jerr := json.Unmarshal([]byte(out), &parsed); jerr != nil {
		t.Fatalf("--json miss stdout is not valid JSON: %v\n%s", jerr, out)
	}
	if ok, _ := parsed["ok"].(bool); ok {
		t.Errorf("--json miss result must report ok:false, got %v", parsed)
	}

	errb := d.Err.(*bytes.Buffer).String()
	if !strings.Contains(errb, "missing123") {
		t.Errorf("forget --json miss diagnostic must still land on stderr, got %q", errb)
	}
}

// Regression: a successful forget stays stdout/exit0 — QA-2 must not touch
// the happy path.
func TestMemoryCmd_ForgetHit_Exit0AndStdout(t *testing.T) {
	port := fakeMemoryDaemon(t, true)
	d := memoryCmdDeps(t, port)

	code := dispatch([]string{"memory", "forget", "abc123"}, d)
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}

	out := d.Out.(*bytes.Buffer).String()
	if !strings.Contains(out, "forgot abc123") {
		t.Errorf("forget hit must report success on stdout, got %q", out)
	}
	if errb := d.Err.(*bytes.Buffer).String(); errb != "" {
		t.Errorf("forget hit must write nothing to stderr, got %q", errb)
	}
}
