package main

// serve_monitor_test.go: U05b — monitor ingest moved under `pix-host serve`
// composition. Real loopback HTTP + a real subprocess, no mocks: a mock HTTP
// client would only prove the mock speaks HTTP, not that NewIngestServer
// actually binds and accepts a POST the way the in-VM tap does.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"pix/host/config"
	"pix/host/monitor"
)

// TestServeServiceAliasesIncludesMonitor (structural): monitor joins memory
// as a `serve`-composable capability, so `pix config set services monitor`
// (or `pix-host serve monitor`) is a recognized alias, not a "no services
// enabled"/"unknown service" refusal.
func TestServeServiceAliasesIncludesMonitor(t *testing.T) {
	alias := serveServiceAliases()
	if alias["monitor"] != "monitor" {
		t.Fatalf("serveServiceAliases()[%q] = %q, want %q", "monitor", alias["monitor"], "monitor")
	}
	if alias["memory"] != "memory" {
		t.Fatalf("adding monitor must not disturb the existing memory alias, got %v", alias)
	}
}

// TestBuildMonitorIngestRealLoopbackRoundTrip: buildMonitorIngest (the exact
// constructor runServe calls) binds a REAL loopback listener; a REAL
// http.Post to it persists through the REAL store, readable back by
// monitor.Follow. No mock server, no mock store.
func TestBuildMonitorIngestRealLoopbackRoundTrip(t *testing.T) {
	root := t.TempDir()
	srv, err := buildMonitorIngest("127.0.0.1", 0, root)
	if err != nil {
		t.Fatalf("buildMonitorIngest: %v", err)
	}
	if !isLoopbackAddr("127.0.0.1") {
		t.Fatal("127.0.0.1 must classify as loopback")
	}

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx) }()
	defer func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()

	line := `{"kind":"tool_end","sandboxId":"sbx","sessionId":"sess","turnId":"t1","seq":1,"ts":1700000000000,"toolId":"real-tool","ok":true,"resultBytes":7,"durationMs":3}` + "\n"
	resp, err := http.Post("http://"+srv.Addr()+"/ingest", "application/x-ndjson", strings.NewReader(line))
	if err != nil {
		t.Fatalf("POST /ingest: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ingest status = %d, want 200", resp.StatusCode)
	}

	text := "real assistant output"
	sum := sha256.Sum256([]byte(text))
	blobBody := `{"hash":"` + hex.EncodeToString(sum[:]) + `","bytes":22,"text":"` + text + `"}`
	blobResp, err := http.Post("http://"+srv.Addr()+"/blob", "application/json", strings.NewReader(blobBody))
	if err != nil {
		t.Fatalf("POST /blob: %v", err)
	}
	blobResp.Body.Close()
	if blobResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /blob status = %d, want 200", blobResp.StatusCode)
	}

	// Read back through the SAME reader `pix monitor` uses (monitor.Follow),
	// over a freshly opened Store against the same root — no shortcuts.
	store, err := monitor.NewStore(monitor.StoreConfig{Root: root})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	events, err := store.Tail("sbx", "sess", 0)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("Tail returned %d events, want 1", len(events))
	}
	if _, err := os.Stat(filepath.Join(root, "blobs.ndjson")); err != nil {
		t.Errorf("blob was not persisted under the store root: %v", err)
	}
}

// syncWriter is a bytes.Buffer safe for a background process's stderr while
// the test goroutine polls it.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}
func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

var monitorBannerRe = regexp.MustCompile(`starting monitor on http://(\S+) \(store (\S+)\)`)

// TestServeMonitorProcessUAT is the process-level acceptance test: the REAL
// compiled pix-host binary, `serve monitor --port 0`, a REAL POST from this
// test (standing in for the in-VM tap), then a graceful SIGTERM — proving
// ingest ownership actually lives inside `pix-host serve` end to end, not
// just in unit-level Go calls.
func TestServeMonitorProcessUAT(t *testing.T) {
	bin := buildHostBinary(t)
	stateDir := t.TempDir()
	// Set it in THIS process too (not just the child's) so config.MonitorStoreRoot()
	// below, called from the test, resolves the same root the child announces.
	t.Setenv("XDG_STATE_HOME", stateDir)

	cmd := exec.Command(bin, "serve", "monitor", "--port", "0", "--bind", "127.0.0.1")
	cmd.Env = append(os.Environ(), "XDG_STATE_HOME="+stateDir)
	var stderr syncWriter
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start pix-host serve: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	// Wait for the real bind banner and parse the real ephemeral address.
	deadline := time.Now().Add(5 * time.Second)
	var addr, storeRoot string
	for time.Now().Before(deadline) {
		if m := monitorBannerRe.FindStringSubmatch(stderr.String()); m != nil {
			addr, storeRoot = m[1], m[2]
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if addr == "" {
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("monitor ingest never announced its bind address; stderr:\n%s", stderr.String())
	}
	wantRoot, rerr := config.MonitorStoreRoot()
	if rerr != nil {
		t.Fatalf("config.MonitorStoreRoot: %v", rerr)
	}
	if storeRoot != wantRoot {
		t.Errorf("serve monitor store root = %q, want %q (config.MonitorStoreRoot honoring XDG_STATE_HOME)", storeRoot, wantRoot)
	}

	// A real POST against the real bound port, as the in-VM extension would.
	line := `{"kind":"tool_end","sandboxId":"uat-sbx","sessionId":"uat-sess","turnId":"t1","seq":1,"ts":1700000000000,"toolId":"uat-tool","ok":true,"resultBytes":4,"durationMs":1}` + "\n"
	resp, err := http.Post("http://"+addr+"/ingest", "application/x-ndjson", strings.NewReader(line))
	if err != nil {
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("POST /ingest: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ingest status = %d, want 200", resp.StatusCode)
	}

	// The pidfile is the same one `serve status` reads back — prove it's ours.
	pidPath := filepath.Join(stateDir, "pix", "serve.pid")
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(pidPath); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Errorf("serve.pid was not written under the state dir: %v", err)
	}

	// The event landed on disk under the SAME root the banner announced,
	// readable by `pix monitor` with no listener of its own.
	streamFile := filepath.Join(storeRoot, "uat-sbx=uat-sess", "events.ndjson")
	deadline = time.Now().Add(2 * time.Second)
	var body []byte
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(streamFile); err == nil && len(b) > 0 {
			body = b
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !bytes.Contains(body, []byte("uat-tool")) {
		t.Fatalf("events.ndjson at %s = %q, want it to contain the ingested event", streamFile, body)
	}

	// Graceful shutdown: SIGTERM must drain and exit 0, and clear the pidfile
	// (same invariant serve's other shutdown path already guarantees).
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("pix-host serve exit = %v, want a clean exit\nstderr:\n%s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("pix-host serve did not exit within 5s of SIGTERM")
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("serve.pid still present after graceful shutdown (err=%v)", err)
	}
}

// TestServeCLIExitCodes: `pix-host serve` now parses --bind/--port itself
// (moved down from `pix monitor`), over the REAL compiled binary — a
// subprocess is the only way to observe the os.Exit code these paths use.
func TestServeCLIExitCodes(t *testing.T) {
	bin := buildHostBinary(t)
	env := append(os.Environ(), "XDG_STATE_HOME="+t.TempDir())

	t.Run("-h prints usage and exits 0", func(t *testing.T) {
		cmd := exec.Command(bin, "serve", "-h")
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("`serve -h` exit err = %v\n%s", err, out)
		}
		for _, want := range []string{"pix-host serve", "--bind ADDR", "--port N", "11437"} {
			if !strings.Contains(string(out), want) {
				t.Errorf("serve -h output missing %q:\n%s", want, out)
			}
		}
	})

	t.Run("unrecognized flag exits 2", func(t *testing.T) {
		cmd := exec.Command(bin, "serve", "--nope")
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("expected a non-zero exit, got err=%v out=%s", err, out)
		}
		if exit.ExitCode() != 2 {
			t.Errorf("`serve --nope` exit code = %d, want 2\n%s", exit.ExitCode(), out)
		}
	})

	t.Run("unknown service name exits 1", func(t *testing.T) {
		cmd := exec.Command(bin, "serve", "bogus-service")
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		exit, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("expected a non-zero exit, got err=%v out=%s", err, out)
		}
		if exit.ExitCode() != 1 {
			t.Errorf("`serve bogus-service` exit code = %d, want 1\n%s", exit.ExitCode(), out)
		}
		if !strings.Contains(string(out), "unknown service") {
			t.Errorf("output missing 'unknown service':\n%s", out)
		}
	})
}
