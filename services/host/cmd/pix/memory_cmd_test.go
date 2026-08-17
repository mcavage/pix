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
	"net"
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
