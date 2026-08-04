package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pix/host/cli"
	"pix/host/monitor"
)

// syncBuffer is a bytes.Buffer safe for the test goroutine to read while a
// backgrounded monitor run writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// panicIngestServer proves a --help / usage-error / --path run never
// constructs an ingest server.
func panicIngestServer(monitor.IngestConfig) (*monitor.IngestServer, error) {
	panic("runMonitorCore must not construct an ingest server here")
}

func TestMonitorHelpAndUsageErrors(t *testing.T) {
	for _, argv := range [][]string{{"-h"}, {"--help"}, {"--help", "mybox"}} {
		var out bytes.Buffer
		if err := runMonitorCore(context.Background(), argv, panicIngestServer, &out, io.Discard, t.TempDir(), false); err != nil {
			t.Fatalf("runMonitorCore(%v): %v", argv, err)
		}
		// The usage text is the only place these operational facts are
		// discoverable, so pin them rather than just "usage:".
		for _, want := range []string{"usage: pix monitor", "make load", "PIX_MONITOR=0", "PIX_MONITOR_URL", "CASE-SENSITIVE", "--path DIR", "--json", "loopback-only"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("monitor usage missing %q", want)
			}
		}
	}
	for _, argv := range [][]string{{"--nope"}, {"one", "two"}} {
		err := runMonitorCore(context.Background(), argv, panicIngestServer, io.Discard, io.Discard, t.TempDir(), false)
		if err == nil {
			t.Fatalf("runMonitorCore(%v) = nil error, want a usage error", argv)
		}
		var usage cli.UsageError2
		if !errors.As(err, &usage) {
			t.Errorf("runMonitorCore(%v) error = %v (%T), want a usage error (exit 2)", argv, err, err)
		}
	}
}

// storeFixture writes one captured stream into dir the way the store does,
// so --path mode has something real to read.
func storeFixture(t *testing.T, dir, sandboxID, sessionID string, events ...string) {
	t.Helper()
	streamDir := filepath.Join(dir, sandboxID+"="+sessionID)
	if err := os.MkdirAll(streamDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := strings.Join(events, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(streamDir, "events.ndjson"), []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func toolEndLine(sandboxID, sessionID, toolID string) string {
	return `{"kind":"tool_end","sandboxId":"` + sandboxID + `","sessionId":"` + sessionID +
		`","turnId":"t1","seq":1,"ts":1700000000000,"toolId":"` + toolID + `","ok":true,"resultBytes":7,"durationMs":3}`
}

// runMonitorUntil runs runMonitorCore in the background until out contains
// want, then cancels — the reader polls, so a test waits rather than calling
// it once.
func runMonitorUntil(t *testing.T, argv []string, newSrv newIngestServerFunc, want string) (string, string) {
	t.Helper()
	var out, errOut syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runMonitorCore(ctx, argv, newSrv, &out, &errOut, t.TempDir(), false) }()
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(out.String()+errOut.String(), want) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runMonitorCore: %v", err)
	}
	return out.String(), errOut.String()
}

// --path is a pure OFFLINE reader: no listener, ever.
func TestMonitorPathModeReadsWithoutAListener(t *testing.T) {
	dir := t.TempDir()
	storeFixture(t, dir, "sbx", "sess", toolEndLine("sbx", "sess", "tool-1"))
	out, _ := runMonitorUntil(t, []string{"--path", dir}, panicIngestServer, "tool_end")
	if !strings.Contains(out, "sbx/sess") || !strings.Contains(out, "tool_end") {
		t.Fatalf("output = %q, want a concise tool_end line", out)
	}
}

func TestMonitorPathModeJSONAndFilter(t *testing.T) {
	dir := t.TempDir()
	storeFixture(t, dir, "keep-me", "sess", toolEndLine("keep-me", "sess", "kept"))
	storeFixture(t, dir, "other", "sess", toolEndLine("other", "sess", "dropped"))

	out, _ := runMonitorUntil(t, []string{"keep", "--path", dir, "--json"}, panicIngestServer, "kept")
	if strings.Contains(out, "dropped") {
		t.Errorf("filtered output %q includes a non-matching stream", out)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("--json emitted a non-JSON line %q: %v", line, err)
		}
		if m["kind"] != "tool_end" {
			t.Errorf("--json line kind = %v, want tool_end", m["kind"])
		}
	}
}

// Live mode is the real thing end to end: a bound loopback listener, a real
// POST from a "sandbox", and the reader printing what was persisted.
func TestMonitorLiveModeIngestsAndPrints(t *testing.T) {
	// The constructor runs on the run goroutine, so hand its address back
	// over a channel rather than a shared variable.
	addrCh := make(chan string, 1)
	capture := func(cfg monitor.IngestConfig) (*monitor.IngestServer, error) {
		if cfg.Port != 0 {
			t.Errorf("test asked for an ephemeral port but got %d", cfg.Port)
		}
		srv, err := monitor.NewIngestServer(cfg)
		if err == nil {
			addrCh <- srv.Addr()
		}
		return srv, err
	}
	var out, errOut syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	root := t.TempDir()
	go func() {
		done <- runMonitorCore(ctx, []string{"--port", "0"}, capture, &out, &errOut, root, false)
	}()
	deadline := time.Now().Add(3 * time.Second)
	var srvAddr string
	select {
	case srvAddr = <-addrCh:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatal("ingest server never bound")
	}
	resp, err := http.Post("http://"+srvAddr+"/ingest", "application/x-ndjson",
		strings.NewReader(toolEndLine("sbx", "sess", "live-tool")+"\n"))
	if err != nil {
		cancel()
		<-done
		t.Fatalf("POST /ingest: %v", err)
	}
	resp.Body.Close()

	text := "assistant said this"
	sum := sha256.Sum256([]byte(text))
	blobResp, err := http.Post("http://"+srvAddr+"/blob", "application/json",
		strings.NewReader(`{"hash":"`+hex.EncodeToString(sum[:])+`","bytes":19,"text":"`+text+`"}`))
	if err != nil {
		cancel()
		<-done
		t.Fatalf("POST /blob: %v", err)
	}
	blobResp.Body.Close()

	for !strings.Contains(out.String(), "tool_end") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runMonitorCore: %v", err)
	}
	if !strings.Contains(out.String(), "sbx/sess") {
		t.Fatalf("output = %q, want the ingested event printed", out.String())
	}
	if !strings.Contains(errOut.String(), "monitor: listening on") {
		t.Errorf("stderr = %q, want the listening banner", errOut.String())
	}
	if _, err := os.Stat(filepath.Join(root, "sbx=sess", "events.ndjson")); err != nil {
		t.Errorf("event was not persisted under the store root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "blobs.ndjson")); err != nil {
		t.Errorf("blob was not persisted under the store root: %v", err)
	}
}

// A non-loopback bind exposes full agent context with no auth, so it must
// warn loudly — and only then.
func TestMonitorNonLoopbackBindWarns(t *testing.T) {
	cfgCh := make(chan monitor.IngestConfig, 2)
	stub := func(cfg monitor.IngestConfig) (*monitor.IngestServer, error) {
		cfgCh <- cfg
		cfg.Port, cfg.BindAddr = 0, "127.0.0.1"
		return monitor.NewIngestServer(cfg)
	}
	_, errOut := runMonitorUntil(t, []string{"--bind", "0.0.0.0", "--port", "0"}, stub, "WARNING")
	if !strings.Contains(errOut, "NO AUTHENTICATION") {
		t.Errorf("stderr = %q, want the no-auth warning", errOut)
	}
	if got := <-cfgCh; got.BindAddr != "0.0.0.0" {
		t.Errorf("bind addr passed through = %q, want 0.0.0.0", got.BindAddr)
	}
	_, quiet := runMonitorUntil(t, []string{"--port", "0"}, stub, "monitor: listening on")
	if strings.Contains(quiet, "WARNING") {
		t.Errorf("loopback bind warned anyway: %q", quiet)
	}
	if got := <-cfgCh; got.BindAddr != monitor.DefaultBindAddr {
		t.Errorf("default bind = %q, want %q", got.BindAddr, monitor.DefaultBindAddr)
	}
}

func TestMonitorIsAKnownVerb(t *testing.T) {
	if !knownVerbs["monitor"] {
		t.Fatal(`knownVerbs["monitor"] = false, want true`)
	}
}
