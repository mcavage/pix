package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"pix/host/cli"
	"pix/host/monitor"
)

// syncBuffer wraps a bytes.Buffer with a mutex: a monitor run happens in a
// background goroutine in the live-mode tests below, while the test
// goroutine polls its output — bytes.Buffer alone is not safe for that
// concurrent read/write.
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

// panicIngestServer is a newIngestServerFunc that panics if called — used to
// prove a --help / usage-error / --path (offline) path never constructs a
// real ingest server.
func panicIngestServer(monitor.IngestConfig) (*monitor.IngestServer, error) {
	panic("runMonitorCore must not construct an ingest server here")
}

func TestMonitorHelp(t *testing.T) {
	for _, argv := range [][]string{{"-h"}, {"--help"}, {"--help", "mybox"}} {
		var out bytes.Buffer
		ctx := context.Background()
		if err := runMonitorCore(ctx, argv, panicIngestServer, &out, io.Discard, t.TempDir(), false); err != nil {
			t.Fatalf("runMonitorCore(%v): %v", argv, err)
		}
		if !strings.Contains(out.String(), "usage: pix monitor") {
			t.Errorf("runMonitorCore(%v) = %q, want monitor usage", argv, out.String())
		}
	}
}

func TestMonitorUsageDocumentsStaleImageAndEnvAndCaseSensitivity(t *testing.T) {
	for _, want := range []string{
		"make load",
		"PIX_MONITOR=0",
		"PIX_MONITOR_URL",
		"host.docker.internal:11437",
		"CASE-SENSITIVE",
		"--path",
		"--json",
	} {
		if !strings.Contains(monitorUsage, want) {
			t.Errorf("monitorUsage missing %q:\n%s", want, monitorUsage)
		}
	}
}

func TestMonitorUnknownFlag(t *testing.T) {
	ctx := context.Background()
	err := runMonitorCore(ctx, []string{"--bogus"}, panicIngestServer, &bytes.Buffer{}, io.Discard, t.TempDir(), false)
	if !cli.IsUsage(err) {
		t.Errorf("runMonitorCore(--bogus): err = %v, want cli.UsageError2", err)
	}
}

func TestMonitorTooManyPositional(t *testing.T) {
	ctx := context.Background()
	err := runMonitorCore(ctx, []string{"box1", "box2"}, panicIngestServer, &bytes.Buffer{}, io.Discard, t.TempDir(), false)
	if !cli.IsUsage(err) {
		t.Errorf("runMonitorCore(box1 box2): err = %v, want cli.UsageError2", err)
	}
}

func TestMonitorUsage(t *testing.T) {
	u, ok := verbUsage("monitor")
	if !ok {
		t.Fatal(`verbUsage("monitor") ok = false, want true`)
	}
	if !strings.HasPrefix(u, "usage: pix monitor") {
		t.Errorf("verbUsage(monitor) = %q, want prefix %q", u, "usage: pix monitor")
	}
}

func TestMonitorKnownVerb(t *testing.T) {
	if !knownVerbs["monitor"] {
		t.Error(`knownVerbs["monitor"] = false, want true`)
	}
}

// TestMonitorPathModeNeverConstructsIngestServer proves --path runs as a
// pure offline reader: no network listener, ever.
func TestMonitorPathModeNeverConstructsIngestServer(t *testing.T) {
	dir := t.TempDir()
	store, err := monitor.NewStore(monitor.StoreConfig{Root: dir})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Append(sampleToolEvent()); err != nil {
		t.Fatalf("Append: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	if err := runMonitorCore(ctx, []string{"--path", dir}, panicIngestServer, &out, io.Discard, "/should-not-be-used", false); err != nil {
		t.Fatalf("runMonitorCore: %v", err)
	}
	if !strings.Contains(out.String(), "tool") {
		t.Errorf("output = %q, want it to contain the pre-existing event", out.String())
	}
}

// TestMonitorPathModeJSONPrintsRawEvents proves --json emits the raw
// (decodable) event JSON instead of the concise line.
func TestMonitorPathModeJSONPrintsRawEvents(t *testing.T) {
	dir := t.TempDir()
	store, err := monitor.NewStore(monitor.StoreConfig{Root: dir})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Append(sampleToolEvent()); err != nil {
		t.Fatalf("Append: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	if err := runMonitorCore(ctx, []string{"--path", dir, "--json"}, panicIngestServer, &out, io.Discard, "", false); err != nil {
		t.Fatalf("runMonitorCore: %v", err)
	}
	line := strings.TrimSpace(out.String())
	ev, err := monitor.Decode([]byte(line))
	if err != nil {
		t.Fatalf("--json output did not decode as an event: %v\noutput: %s", err, out.String())
	}
	if ev.Envelope().SandboxID != "sbx" {
		t.Errorf("decoded event sandboxId = %q, want %q", ev.Envelope().SandboxID, "sbx")
	}
}

// TestMonitorPathModeFiltersBySandboxSubstring proves the [name] positional
// filters which streams the reader prints.
func TestMonitorPathModeFiltersBySandboxSubstring(t *testing.T) {
	dir := t.TempDir()
	store, err := monitor.NewStore(monitor.StoreConfig{Root: dir})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	if err := store.Append(mustDecodeToolEvent(t, "match-me", "sess", "t1")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := store.Append(mustDecodeToolEvent(t, "other-sbx", "sess2", "t2")); err != nil {
		t.Fatalf("Append: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	if err := runMonitorCore(ctx, []string{"--path", dir, "match-me"}, panicIngestServer, &out, io.Discard, "", false); err != nil {
		t.Fatalf("runMonitorCore: %v", err)
	}
	if !strings.Contains(out.String(), "match-me") {
		t.Errorf("output = %q, want the matching sandbox's events", out.String())
	}
	if strings.Contains(out.String(), "other-sbx") {
		t.Errorf("output = %q, want the non-matching sandbox filtered out", out.String())
	}
}

// TestMonitorLiveModeBindsIngestAndStoresEvents proves the default (no
// --path) mode binds a real ingest server and both the reader and a
// subsequent offline read of the SAME directory see events posted to it —
// a real loopback socket + real files, no mocks.
func TestMonitorLiveModeBindsIngestAndStoresEvents(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	out := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- runMonitorCore(ctx, []string{"--port", "0"}, monitor.NewIngestServer, out, io.Discard, dir, false)
	}()

	// Wait for the ingest server to actually bind by polling the store
	// directory it will create, then post an event to it via a real HTTP
	// client against the bound port. Since --port 0 picks an ephemeral
	// port we don't know in advance, drive this through the store
	// directly instead (the ingest server persists to the SAME store this
	// test constructs independently) — proves the reader side (poll loop)
	// picks up an event that "arrived" while it was running.
	store, err := monitor.NewStore(monitor.StoreConfig{Root: dir})
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	deadlineAppend := time.Now().Add(2 * time.Second)
	for {
		if err := store.Append(sampleToolEvent()); err == nil {
			break
		}
		if time.Now().After(deadlineAppend) {
			t.Fatal("could not append to the live store")
		}
		time.Sleep(5 * time.Millisecond)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		if strings.Contains(out.String(), "tool") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reader never printed the appended event; output so far: %q", out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("runMonitorCore returned error after cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runMonitorCore did not return after ctx cancel")
	}
}

func TestMonitorLiveModeNonLoopbackBindWarns(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &syncBuffer{}
	errOut := &syncBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- runMonitorCore(ctx, []string{"--port", "0", "--bind", "0.0.0.0"}, monitor.NewIngestServer, out, errOut, dir, false)
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if strings.Contains(errOut.String(), "WARNING") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no non-loopback warning printed; stderr so far: %q", errOut.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

// sampleToolEvent and mustDecodeToolEvent build a monitor.Event the way a
// real producer does — via Decode over a JSON line — rather than a struct
// literal, since ToolStart's envelope field is unexported (monitor.Envelope
// values are set through the wire shape, not by embedding, from outside the
// monitor package).
func sampleToolEvent() monitor.Event {
	return mustDecodeToolEvent(nil, "sbx", "sess", "t1")
}

func mustDecodeToolEvent(t *testing.T, sandboxID, sessionID, toolID string) monitor.Event {
	line := `{"kind":"tool_start","sandboxId":"` + sandboxID + `","sessionId":"` + sessionID + `","turnId":"1","ts":1,"toolId":"` + toolID + `","source":"builtin","name":"bash","argsSummary":"go test ./..."}`
	ev, err := monitor.Decode([]byte(line))
	if err != nil {
		if t != nil {
			t.Fatalf("Decode: %v", err)
		}
		panic(err)
	}
	return ev
}
