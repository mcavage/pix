package service

// serve_upgrade_test.go: U3-lifecycle regression coverage. The read-side
// EnsureUp (used by `pix run` / `pix memory …`) used to detect a
// version-mismatched daemon and restart it itself (staleServeVersion /
// restartStaleServe). That restart's "success" was only ever a mechanical
// TCP/launchctl signal, never a re-check that the daemon actually came up as
// the NEW version — so a restart that never converged printed "updated" and
// then repeated the same non-convergent restart on every subsequent
// invocation: `pix memory recall '*'` never returned, and never fixed
// anything. These tests prove that loop is gone (EnsureUp never restarts, no
// matter how many times it is called) and that the machinery it used to call
// no longer exists on this path. The replacement, verified reconciliation
// seam is tested where it now actually lives: install.go's
// reportManagedServeHealth / verifyManagedInstallHealth (see
// serve_install_test.go).

import (
	"bytes"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"pix/host/config"
)

// forbiddenReadSideRestartSymbols are the deleted read-side mutation functions
// (U3-lifecycle): a grep-based sentinel, mirroring cmd/pix/hostmode_gone_test.go's
// pattern, so a future edit can never quietly reintroduce the automatic,
// unverified version-restart into start.go without this test noticing — the
// dynamic tests above prove the BEHAVIOR is gone, this proves the CODE is.
var forbiddenReadSideRestartSymbols = []string{
	"func staleServeVersion(",
	"func restartStaleServe(",
}

// TestStaleVersionRestartMachineryDeletedFromStartGo is the sentinel: neither
// symbol may appear anywhere in start.go again. Version reconciliation now
// lives ONLY in install.go (verifyServeIdentity / reportManagedServeHealth),
// on the explicit `pix serve start`/`install` path.
func TestStaleVersionRestartMachineryDeletedFromStartGo(t *testing.T) {
	b, err := os.ReadFile("start.go")
	if err != nil {
		t.Fatalf("read start.go: %v", err)
	}
	src := string(b)
	for _, sym := range forbiddenReadSideRestartSymbols {
		if strings.Contains(src, sym) {
			t.Errorf("start.go must never again contain %q — the read-side EnsureUp path must not mutate the running daemon (U3-lifecycle)", sym)
		}
	}
}

// TestEnsureUp_100CallsNeverRestartOrProbeIdentity is the direct regression for
// the non-converging loop: with the required port already answering, 100
// EnsureUp calls must all take the silent fast path — no identity RPC traffic
// (staleServeVersion used to probe `identity` on every single call), no
// restart-shaped output, and no meaningful wall-clock cost (a live identity
// probe budgets ~900ms x2 retries per call; 100 of those would take minutes).
func TestEnsureUp_100CallsNeverRestartOrProbeIdentity(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	bytesReceived := 0
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 256)
			conn.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
			n, _ := conn.Read(buf)
			bytesReceived += n // EnsureUp's fast path only dials; it must never WRITE an RPC request
			conn.Close()
		}
	}()

	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("MEMORY_PORT", strconv.Itoa(port))
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("services = [\"memory\"]\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, err := config.Load(); err != nil {
		t.Fatalf("sanity config.Load: %v", err)
	}

	var out bytes.Buffer
	start := time.Now()
	for i := 0; i < 100; i++ {
		EnsureUp(&out, []string{"memory"}, EnsureMemoryTimeout)
	}
	elapsed := time.Since(start)

	ln.Close()
	<-done

	if out.Len() != 0 {
		t.Errorf("100 calls against an already-up port must be silent (fast path, no restart chatter), got %q", out.String())
	}
	if bytesReceived != 0 {
		t.Errorf("EnsureUp's read-side fast path must never send RPC traffic (e.g. an identity probe), got %d bytes on the wire", bytesReceived)
	}
	if elapsed > 2*time.Second {
		t.Errorf("100 EnsureUp calls took %s — a restart/identity-probe loop would cost ~seconds per call; this must be near-instant", elapsed)
	}
}

// TestEnsureUp_NeverPrintsUpdated_EvenWhenPortAnswersOldVersion is a second,
// output-shaped angle on the same regression: even if a hostile/old daemon on
// the required port would answer an identity probe with a stale version,
// EnsureUp never gets that far — it dials TCP only. The word "updated" (the
// literal old bug's success wording) must never appear from this path.
func TestEnsureUp_NeverPrintsUpdated_EvenWhenPortAnswersOldVersion(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()

	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("MEMORY_PORT", strconv.Itoa(port))
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("services = [\"memory\"]\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var out bytes.Buffer
	for i := 0; i < 10; i++ {
		EnsureUp(&out, []string{"memory"}, EnsureMemoryTimeout)
	}
	if strings.Contains(out.String(), "updated") {
		t.Errorf("EnsureUp must never print \"updated\": %q", out.String())
	}
}
