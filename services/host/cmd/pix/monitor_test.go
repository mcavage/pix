package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pix/host/cli"
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

func TestMonitorHelpAndUsageErrors(t *testing.T) {
	for _, argv := range [][]string{{"-h"}, {"--help"}, {"--help", "mybox"}} {
		var out bytes.Buffer
		if err := runMonitorCore(context.Background(), argv, &out, t.TempDir(), false); err != nil {
			t.Fatalf("runMonitorCore(%v): %v", argv, err)
		}
		// The usage text is the only place these operational facts are
		// discoverable, so pin them rather than just "usage:". It must also
		// say plainly this is a reader with no listener of its own, and point
		// at `pix serve` for the ingest side.
		for _, want := range []string{"usage: pix monitor", "PURE READER", "never binds a port", "pix serve", "make load", "PIX_MONITOR=0", "PIX_MONITOR_URL", "CASE-SENSITIVE", "--path DIR", "--json"} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("monitor usage missing %q", want)
			}
		}
		// --bind/--port moved to `pix serve`; monitor no longer recognizes them.
		if strings.Contains(out.String(), "--bind ADDR") || strings.Contains(out.String(), "--port N") {
			t.Errorf("monitor usage still advertises --bind/--port, which moved to `pix serve`:\n%s", out.String())
		}
	}
	for _, argv := range [][]string{{"--nope"}, {"one", "two"}, {"--bind", "0.0.0.0"}, {"--port", "0"}} {
		err := runMonitorCore(context.Background(), argv, io.Discard, t.TempDir(), false)
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
// so a run has something real to read.
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
// want, then cancels — Follow polls, so a test waits rather than calling it
// once.
func runMonitorUntil(t *testing.T, argv []string, defaultRoot string, want string) string {
	t.Helper()
	var out syncBuffer
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runMonitorCore(ctx, argv, &out, defaultRoot, false) }()
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(out.String(), want) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("runMonitorCore: %v", err)
	}
	return out.String()
}

// monitor is a PURE reader: it must read a pre-existing store with no
// listener ever started, whether or not --path is given.
func TestMonitorDefaultRootReadsWithoutAListener(t *testing.T) {
	dir := t.TempDir()
	storeFixture(t, dir, "sbx", "sess", toolEndLine("sbx", "sess", "tool-1"))
	out := runMonitorUntil(t, nil, dir, "tool_end")
	if !strings.Contains(out, "sbx/sess") || !strings.Contains(out, "tool_end") {
		t.Fatalf("output = %q, want a concise tool_end line", out)
	}
}

func TestMonitorPathModeReadsAnArbitraryRoot(t *testing.T) {
	defaultRoot := t.TempDir() // deliberately empty: --path must override it
	dir := t.TempDir()
	storeFixture(t, dir, "sbx", "sess", toolEndLine("sbx", "sess", "tool-1"))
	out := runMonitorUntil(t, []string{"--path", dir}, defaultRoot, "tool_end")
	if !strings.Contains(out, "sbx/sess") || !strings.Contains(out, "tool_end") {
		t.Fatalf("output = %q, want a concise tool_end line", out)
	}
}

func TestMonitorPathModeJSONAndFilter(t *testing.T) {
	dir := t.TempDir()
	storeFixture(t, dir, "keep-me", "sess", toolEndLine("keep-me", "sess", "kept"))
	storeFixture(t, dir, "other", "sess", toolEndLine("other", "sess", "dropped"))

	out := runMonitorUntil(t, []string{"keep", "--path", dir, "--json"}, t.TempDir(), "kept")
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

func TestMonitorIsAKnownVerb(t *testing.T) {
	if !knownVerbs["monitor"] {
		t.Fatal(`knownVerbs["monitor"] = false, want true`)
	}
}
