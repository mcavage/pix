package monitor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// startTestHub starts a Hub on an OS-assigned port (Port:0) and returns it
// plus a cleanup func that cancels ctx and waits for Start to return.
func startTestHub(t *testing.T, cfg HubConfig) (*Hub, string) {
	t.Helper()
	cfg.Port = 0 // ephemeral, avoid colliding with a real :11437 or other tests
	h := NewHub(cfg)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- h.Start(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for h.Addr() == "" {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("hub did not report an Addr() within the deadline")
		}
		time.Sleep(time.Millisecond)
	}
	addr := h.Addr()

	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Hub.Start returned error on shutdown: %v", err)
			}
		case <-time.After(10 * time.Second):
			// Comfortably above shutdownGrace (5s) so this never races the
			// server's own internal shutdown timeout.
			t.Errorf("Hub.Start did not return within 10s of ctx cancel")
		}
	})
	return h, addr
}

// waitForRingLen polls until h.Events() reaches at least n entries or the
// deadline expires, so ingest tests don't race the async HTTP handler.
func waitForRingLen(t *testing.T, h *Hub, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if len(h.Events()) >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ring never reached len %d; got %+v", n, h.Events())
		}
		time.Sleep(time.Millisecond)
	}
}

func waitHealthy(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		resp, err := http.Get("http://" + addr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			err = fmt.Errorf("status %d", resp.StatusCode)
		}
		if time.Now().After(deadline) {
			t.Fatalf("hub never became reachable at %s: %v", addr, err)
		}
		time.Sleep(time.Millisecond)
	}
}

// sha256Blob builds a real content-addressed Blob (Hash == sha256(text)) for
// tests exercising the /blob endpoints, which now reject (R1-11) any Blob
// whose Hash doesn't match its Text.
func sha256Blob(t *testing.T, text string) Blob {
	t.Helper()
	sum := sha256.Sum256([]byte(text))
	return Blob{Hash: hex.EncodeToString(sum[:]), Bytes: len(text), Text: text}
}

func ndjsonLine(t *testing.T, e Event) string {
	t.Helper()
	b, err := Encode(e)
	if err != nil {
		t.Fatalf("Encode() error: %v", err)
	}
	return string(b)
}

func TestHubAddrBindsEphemeralPort(t *testing.T) {
	_, addr := startTestHub(t, HubConfig{})
	if addr == "" || addr == "0.0.0.0:0" {
		t.Fatalf("Addr() = %q, want a concrete host:port", addr)
	}
}

func TestHubHealthz(t *testing.T) {
	_, addr := startTestHub(t, HubConfig{})
	waitHealthy(t, addr)

	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode /healthz body: %v", err)
	}
	if ok, _ := body["ok"].(bool); !ok {
		t.Fatalf("/healthz body = %v, want ok=true", body)
	}
}

// TestHubHealthzDoesNotLeakSandboxIDs is SEC-4: /healthz is unauthenticated,
// so it must not report the sandboxIds observed — only {"ok":true}.
func TestHubHealthzDoesNotLeakSandboxIDs(t *testing.T) {
	h, addr := startTestHub(t, HubConfig{})
	waitHealthy(t, addr)

	h.publish(TurnStart{env: env{Kind: KindTurnStart, SandboxID: "secret-sandbox-name", Seq: 1}, Model: "opus"})

	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz error: %v", err)
	}
	defer resp.Body.Close()

	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode /healthz body: %v", err)
	}
	if _, present := body["sandboxes"]; present {
		t.Fatalf("/healthz body = %v, want no \"sandboxes\" key (info leak)", body)
	}
	if len(body) != 1 {
		t.Fatalf("/healthz body = %v, want only {\"ok\":true}", body)
	}
}

func TestHubIngestMultiLineReplayAndLive(t *testing.T) {
	h, addr := startTestHub(t, HubConfig{})
	waitHealthy(t, addr)

	e1 := TurnStart{env: env{Kind: KindTurnStart, SandboxID: "sbx-1", Seq: 1}, Model: "opus", Trigger: "user"}
	e2 := ToolStart{env: env{Kind: KindToolStart, SandboxID: "sbx-1", Seq: 2}, ToolID: "t1", Source: "builtin", Name: "bash"}

	body := ndjsonLine(t, e1) + "\n" + ndjsonLine(t, e2) + "\n"
	resp, err := http.Post("http://"+addr+"/ingest", "application/x-ndjson", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /ingest error: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ingest status = %d, want 200", resp.StatusCode)
	}

	// Ring should now hold both, in order.
	waitForRingLen(t, h, 2)
	snap := h.Events()
	if len(snap) != 2 {
		t.Fatalf("Events() len = %d, want 2: %+v", len(snap), snap)
	}
	if snap[0].Kind() != KindTurnStart || snap[1].Kind() != KindToolStart {
		t.Fatalf("Events() kinds = %v, want [turn_start tool_start]", []Kind{snap[0].Kind(), snap[1].Kind()})
	}

	// Subscribe AFTER ingest: must replay both events first.
	sub, unsub := h.Subscribe()
	defer unsub()

	got := []Event{<-sub, <-sub}
	if got[0].Kind() != KindTurnStart || got[1].Kind() != KindToolStart {
		t.Fatalf("replay kinds = %v, want [turn_start tool_start]", []Kind{got[0].Kind(), got[1].Kind()})
	}

	// Now a live event, sent after Subscribe, must also arrive on the channel.
	e3 := ToolEnd{env: env{Kind: KindToolEnd, SandboxID: "sbx-1", Seq: 3}, ToolID: "t1", OK: true}
	resp2, err := http.Post("http://"+addr+"/ingest", "application/x-ndjson", strings.NewReader(ndjsonLine(t, e3)+"\n"))
	if err != nil {
		t.Fatalf("POST /ingest (live) error: %v", err)
	}
	resp2.Body.Close()

	select {
	case live := <-sub:
		if live.Kind() != KindToolEnd {
			t.Fatalf("live event kind = %q, want tool_end", live.Kind())
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for live event on Subscribe channel")
	}
}

func TestHubIngestSkipsUnparseableLinesButKeepsGoing(t *testing.T) {
	h, addr := startTestHub(t, HubConfig{})
	waitHealthy(t, addr)

	e1 := TurnStart{env: env{Kind: KindTurnStart, SandboxID: "sbx-1", Seq: 1}, Model: "opus"}
	body := ndjsonLine(t, e1) + "\n" + "not json at all\n" + `{"kind":"unknown_kind"}` + "\n"
	resp, err := http.Post("http://"+addr+"/ingest", "application/x-ndjson", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /ingest error: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ingest status = %d, want 200 even with bad lines", resp.StatusCode)
	}

	waitForRingLen(t, h, 1)
	snap := h.Events()
	if len(snap) != 1 {
		t.Fatalf("Events() len = %d, want 1 (only the one parseable line)", len(snap))
	}
}

func TestHubBlobPostAndGetRoundTrip(t *testing.T) {
	_, addr := startTestHub(t, HubConfig{})
	waitHealthy(t, addr)

	// R1-11: /blob now verifies Hash == sha256(Text), so the Blob under test
	// must be a real content-addressed pair, not an arbitrary label.
	bl := sha256Blob(t, "hello")
	b, err := json.Marshal(bl)
	if err != nil {
		t.Fatalf("Marshal(Blob) error: %v", err)
	}
	resp, err := http.Post("http://"+addr+"/blob", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /blob error: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /blob status = %d, want 200", resp.StatusCode)
	}

	resp2, err := http.Get("http://" + addr + "/blob/" + bl.Hash)
	if err != nil {
		t.Fatalf("GET /blob/%s error: %v", bl.Hash, err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("GET /blob/%s status = %d, want 200", bl.Hash, resp2.StatusCode)
	}
	var got Blob
	if err := json.NewDecoder(resp2.Body).Decode(&got); err != nil {
		t.Fatalf("decode blob response: %v", err)
	}
	if got != bl {
		t.Fatalf("GET /blob/%s = %+v, want %+v", bl.Hash, got, bl)
	}

	resp3, err := http.Get("http://" + addr + "/blob/does-not-exist")
	if err != nil {
		t.Fatalf("GET /blob/does-not-exist error: %v", err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /blob/does-not-exist status = %d, want 404", resp3.StatusCode)
	}
}

// TestHubBlobPostRejectsMismatchedHash is R1-11 at the HTTP layer: a POST
// /blob whose Hash doesn't match sha256(Text) must 400, and must never end
// up retrievable via GET.
func TestHubBlobPostRejectsMismatchedHash(t *testing.T) {
	_, addr := startTestHub(t, HubConfig{})
	waitHealthy(t, addr)

	bl := Blob{Hash: "not-a-real-sha256", Bytes: 5, Text: "hello"}
	b, err := json.Marshal(bl)
	if err != nil {
		t.Fatalf("Marshal(Blob) error: %v", err)
	}
	resp, err := http.Post("http://"+addr+"/blob", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /blob error: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /blob (mismatched hash) status = %d, want 400", resp.StatusCode)
	}

	resp2, err := http.Get("http://" + addr + "/blob/not-a-real-sha256")
	if err != nil {
		t.Fatalf("GET error: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /blob/not-a-real-sha256 status = %d, want 404 (must never be stored)", resp2.StatusCode)
	}
}

func TestHubBlobInProcessLookup(t *testing.T) {
	h, addr := startTestHub(t, HubConfig{})
	waitHealthy(t, addr)

	bl := sha256Blob(t, "hi")
	b, _ := json.Marshal(bl)
	resp, err := http.Post("http://"+addr+"/blob", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /blob error: %v", err)
	}
	resp.Body.Close()

	got, ok := h.Blob(bl.Hash)
	if !ok || got != bl {
		t.Fatalf("Hub.Blob(%s) = %+v ok=%v, want %+v ok=true", bl.Hash, got, ok, bl)
	}
	if _, ok := h.Blob("missing"); ok {
		t.Fatalf("Hub.Blob(missing) ok=true, want false")
	}
}

func TestHubFilterDropsNonMatchingSandbox(t *testing.T) {
	h, addr := startTestHub(t, HubConfig{Filter: "sbx-keep"})
	waitHealthy(t, addr)

	dropped := TurnStart{env: env{Kind: KindTurnStart, SandboxID: "sbx-drop-me", Seq: 1}, Model: "opus"}
	kept := TurnStart{env: env{Kind: KindTurnStart, SandboxID: "my-sbx-keep-box", Seq: 2}, Model: "opus"}

	body := ndjsonLine(t, dropped) + "\n" + ndjsonLine(t, kept) + "\n"
	resp, err := http.Post("http://"+addr+"/ingest", "application/x-ndjson", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /ingest error: %v", err)
	}
	resp.Body.Close()

	waitForRingLen(t, h, 1)
	snap := h.Events()
	if len(snap) != 1 {
		t.Fatalf("Events() len = %d, want 1 (dropped should never reach the ring): %+v", len(snap), snap)
	}
	if snap[0].Envelope().SandboxID != "my-sbx-keep-box" {
		t.Fatalf("Events()[0].SandboxID = %q, want the kept sandbox", snap[0].Envelope().SandboxID)
	}
}

func TestHubStreamSSEReplayThenLive(t *testing.T) {
	h, addr := startTestHub(t, HubConfig{})
	waitHealthy(t, addr)

	e1 := TurnStart{env: env{Kind: KindTurnStart, SandboxID: "sbx-1", Seq: 1}, Model: "opus"}
	h.publish(e1) // seed the ring directly, avoiding an extra ingest round-trip

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/stream", nil)
	if err != nil {
		t.Fatalf("NewRequest error: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /stream error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /stream status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	reader := bufio.NewReader(resp.Body)
	readFrame := func() (string, error) {
		var lines []string
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return "", err
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				return strings.Join(lines, "\n"), nil
			}
			lines = append(lines, line)
		}
	}

	frame, err := readFrame()
	if err != nil {
		t.Fatalf("reading replay frame: %v", err)
	}
	if !strings.HasPrefix(frame, "data: ") {
		t.Fatalf("frame = %q, want a data: line", frame)
	}
	if !strings.Contains(frame, `"kind":"turn_start"`) {
		t.Fatalf("replay frame = %q, want turn_start", frame)
	}

	// Now push a live event and read the next frame in a goroutine so a
	// stuck stream fails the test instead of hanging it forever.
	e2 := ToolStart{env: env{Kind: KindToolStart, SandboxID: "sbx-1", Seq: 2}, ToolID: "t1", Name: "bash"}
	frameCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		f, err := readFrame()
		if err != nil {
			errCh <- err
			return
		}
		frameCh <- f
	}()
	h.publish(e2)

	select {
	case f := <-frameCh:
		if !strings.Contains(f, `"kind":"tool_start"`) {
			t.Fatalf("live frame = %q, want tool_start", f)
		}
	case err := <-errCh:
		t.Fatalf("reading live frame: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for live SSE frame")
	}
}

func TestHubSubscribeUnsubscribeStopsDelivery(t *testing.T) {
	h, addr := startTestHub(t, HubConfig{})
	waitHealthy(t, addr)

	sub, unsub := h.Subscribe()
	unsub()

	// Channel must be closed after unsubscribe.
	select {
	case _, ok := <-sub:
		if ok {
			t.Fatalf("received a value on an unsubscribed channel, want it closed")
		}
	case <-time.After(time.Second):
		t.Fatalf("unsubscribed channel never closed")
	}

	// Publishing afterward must not panic (dead subscriber already removed).
	h.publish(TurnStart{env: env{Kind: KindTurnStart, Seq: 99}, Model: "opus"})
}

// TestHubStartHonorsExplicitBindAddr proves the opt-in path (SEC-1): an
// explicit non-default BindAddr (here the wildcard, the same one Start used
// to hardcode unconditionally) is still honored when a caller deliberately
// asks for it, and Addr() still rewrites it to a dialable loopback address.
func TestHubStartHonorsExplicitBindAddr(t *testing.T) {
	h, addr := startTestHub(t, HubConfig{BindAddr: "0.0.0.0"})
	waitHealthy(t, addr)
	if h.cfg.BindAddr != "0.0.0.0" {
		t.Fatalf("cfg.BindAddr = %q, want the explicit 0.0.0.0 preserved", h.cfg.BindAddr)
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", addr, err)
	}
	if host != "127.0.0.1" {
		t.Fatalf("Addr() host = %q, want 127.0.0.1 (dialableAddr rewrite)", host)
	}
}

func TestHubStartReturnsOnCtxCancelAndReleasesPort(t *testing.T) {
	h := NewHub(HubConfig{Port: 0})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Start(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for h.Addr() == "" {
		if time.Now().After(deadline) {
			t.Fatalf("hub never bound an address")
		}
		time.Sleep(time.Millisecond)
	}
	addr := h.Addr()
	waitHealthy(t, addr)

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start() returned error on ctx cancel: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Start() did not return within 5s of ctx cancel")
	}

	// Port must be released: a fresh dial should fail (connection refused),
	// proving Shutdown actually closed the listener rather than leaking it.
	_, err := http.Get("http://" + addr + "/healthz")
	if err == nil {
		t.Fatalf("GET /healthz succeeded after shutdown, want the port released")
	}
}

// TestHubDefaultBindAddrIsLoopback is SEC-1: NewHub with no BindAddr set
// must default to loopback, never a wildcard/exposed bind.
func TestHubDefaultBindAddrIsLoopback(t *testing.T) {
	h := NewHub(HubConfig{Port: 0})
	if h.cfg.BindAddr != DefaultBindAddr {
		t.Fatalf("NewHub({}).cfg.BindAddr = %q, want default %q", h.cfg.BindAddr, DefaultBindAddr)
	}
	if DefaultBindAddr != "127.0.0.1" {
		t.Fatalf("DefaultBindAddr = %q, want 127.0.0.1", DefaultBindAddr)
	}
}

// TestHubBlobPostRejectsOversizedBody is SEC-3: a POST /blob body over
// maxBlobPost must not be decoded/stored in full — it must fail (bad JSON,
// truncated by the LimitReader) rather than silently accepted or buffered
// unbounded.
func TestHubBlobPostRejectsOversizedBody(t *testing.T) {
	_, addr := startTestHub(t, HubConfig{})
	waitHealthy(t, addr)

	// Build a JSON body whose Text field alone exceeds maxBlobPost, so the
	// LimitReader cuts it off mid-stream and the decode fails.
	huge := Blob{Hash: "toolarge", Bytes: 1, Text: strings.Repeat("z", maxBlobPost+1024)}
	b, err := json.Marshal(huge)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}

	resp, err := http.Post("http://"+addr+"/blob", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST /blob error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /blob (oversized) status = %d, want 400", resp.StatusCode)
	}

	resp2, err := http.Get("http://" + addr + "/blob/toolarge")
	if err != nil {
		t.Fatalf("GET /blob/toolarge error: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /blob/toolarge status = %d, want 404 (must never have been stored)", resp2.StatusCode)
	}
}

// TestHubIngestSkipsOversizedLineButKeepsGoing is SEC-3: a single NDJSON
// line over maxIngestLine must be skipped — not buffered unbounded, and not
// fatal to the rest of the request — while lines before and after it are
// still published.
func TestHubIngestSkipsOversizedLineButKeepsGoing(t *testing.T) {
	h, addr := startTestHub(t, HubConfig{})
	waitHealthy(t, addr)

	e1 := TurnStart{env: env{Kind: KindTurnStart, SandboxID: "sbx-1", Seq: 1}, Model: "opus"}
	// A line that decodes fine as far as size goes but is individually way
	// over maxIngestLine: pad it with a huge unknown field so it's just "a
	// giant NDJSON line", not necessarily valid Event JSON (it need not
	// decode; it only needs to be skipped without wedging the scan).
	hugeLine := `{"kind":"turn_start","pad":"` + strings.Repeat("p", maxIngestLine+1024) + `"}`
	e3 := ToolStart{env: env{Kind: KindToolStart, SandboxID: "sbx-1", Seq: 3}, ToolID: "t1", Name: "bash"}

	body := ndjsonLine(t, e1) + "\n" + hugeLine + "\n" + ndjsonLine(t, e3) + "\n"
	resp, err := http.Post("http://"+addr+"/ingest", "application/x-ndjson", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /ingest error: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ingest status = %d, want 200 even with an oversized line", resp.StatusCode)
	}

	waitForRingLen(t, h, 2)
	snap := h.Events()
	if len(snap) != 2 {
		t.Fatalf("Events() len = %d, want 2 (the oversized line skipped, both real lines kept): %+v", len(snap), snap)
	}
	if snap[0].Kind() != KindTurnStart || snap[1].Kind() != KindToolStart {
		t.Fatalf("Events() kinds = %v, want [turn_start tool_start]", []Kind{snap[0].Kind(), snap[1].Kind()})
	}
}

func TestHubStartBindErrorOnPortInUse(t *testing.T) {
	h1, addr := startTestHub(t, HubConfig{})
	waitHealthy(t, addr)

	// addr is host:port; extract the numeric port and try to bind it again.
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("parsing addr %q: %v", addr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parsing port %q: %v", portStr, err)
	}
	_ = h1

	h2 := NewHub(HubConfig{Port: port})
	err = h2.Start(context.Background())
	if err == nil {
		t.Fatalf("Start() on an in-use port: got nil error, want a bind error")
	}
}

// TestHubConcurrentIngestAndSubscribe exercises the Hub's locking under
// -race: many concurrent ingests racing many concurrent Subscribe/unsubscribe
// calls.
func TestHubConcurrentIngestAndSubscribe(t *testing.T) {
	h, addr := startTestHub(t, HubConfig{})
	waitHealthy(t, addr)

	// DisableKeepAlives: a burst of concurrent requests against a brand-new
	// host:port makes the default pooling transport speculatively open more
	// TCP connections than it ends up using; the unused ones sit accepted but
	// idle-before-first-request (net/http's StateNew), which Shutdown's
	// graceful drain does not consider idle — so it would otherwise wait out
	// the full grace period on every run of this test. Closing each
	// connection right after its one request avoids manufacturing that.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e := TurnStart{env: env{Kind: KindTurnStart, SandboxID: "sbx", Seq: uint64(i)}, Model: "opus"}
			resp, err := client.Post("http://"+addr+"/ingest", "application/x-ndjson", strings.NewReader(ndjsonLine(t, e)+"\n"))
			if err != nil {
				t.Errorf("POST /ingest error: %v", err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}(i)
	}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub, unsub := h.Subscribe()
			defer unsub()
			select {
			case <-sub:
			case <-time.After(2 * time.Second):
			}
		}()
	}
	wg.Wait()
}

// TestHubSubscribeReplaysFullRingBeyondOldSubscriberBufferSize is R1-9(a):
// before the fix, Subscribe's replay loop pushed into a channel buffered at
// a fixed 256, so subscribing after more than 256 ring events silently lost
// most of the replay (kept the oldest 256, dropped the rest via the
// replay loop's own "default: drop" branch). This proves the fix: every
// ring event reaches a new subscriber, in order.
func TestHubSubscribeReplaysFullRingBeyondOldSubscriberBufferSize(t *testing.T) {
	h, addr := startTestHub(t, HubConfig{})
	waitHealthy(t, addr)

	const n = 500 // comfortably more than the old hardcoded 256 buffer
	for i := 0; i < n; i++ {
		h.publish(TurnStart{env: env{Kind: KindTurnStart, SandboxID: "sbx", Seq: uint64(i)}, Model: "opus"})
	}
	if got := len(h.Events()); got != n {
		t.Fatalf("ring len = %d, want %d", got, n)
	}

	sub, unsub := h.Subscribe()
	defer unsub()

	for i := 0; i < n; i++ {
		select {
		case e := <-sub:
			if e.Envelope().Seq != uint64(i) {
				t.Fatalf("replay[%d].Seq = %d, want %d (replay must be complete and in order)", i, e.Envelope().Seq, i)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for replay event %d/%d — replay lost events", i, n)
		}
	}
}

// TestHubPublishLiveOverflowDropsOldestKeepsNewest is R1-9(b): when a
// subscriber's channel is full of undrained LIVE events, publish must evict
// the OLDEST queued event to make room for the newest one — not silently
// drop the incoming (newest) event, which the old code's comment claimed
// was "drop-oldest" but actually did the opposite (drop-newest via the
// send's own "default" branch).
func TestHubPublishLiveOverflowDropsOldestKeepsNewest(t *testing.T) {
	h, addr := startTestHub(t, HubConfig{})
	waitHealthy(t, addr)

	sub, unsub := h.Subscribe() // empty ring: channel buffer = subscriberLiveHeadroom (256)
	defer unsub()

	const overflow = 40
	total := subscriberLiveHeadroom + overflow
	for i := 0; i < total; i++ {
		h.publish(TurnStart{env: env{Kind: KindTurnStart, SandboxID: "sbx", Seq: uint64(i)}, Model: "opus"})
	}

	// Drain everything queued without blocking further.
	var got []Event
	draining := true
	for draining {
		select {
		case e := <-sub:
			got = append(got, e)
		default:
			draining = false
		}
	}

	if len(got) != subscriberLiveHeadroom {
		t.Fatalf("queued events = %d, want exactly %d (the buffer capacity)", len(got), subscriberLiveHeadroom)
	}
	// The oldest `overflow` events must have been evicted from the front, so
	// what remains is the newest subscriberLiveHeadroom events, oldest of
	// THOSE first: seq == overflow .. total-1, contiguous.
	wantFirst := uint64(overflow)
	wantLast := uint64(total - 1)
	if got[0].Envelope().Seq != wantFirst {
		t.Fatalf("got[0].Seq = %d, want %d (oldest surviving event, after drop-oldest evicted the first %d)", got[0].Envelope().Seq, wantFirst, overflow)
	}
	if last := got[len(got)-1].Envelope().Seq; last != wantLast {
		t.Fatalf("got[last].Seq = %d, want %d (the newest published event must survive)", last, wantLast)
	}
	for i, e := range got {
		if want := wantFirst + uint64(i); e.Envelope().Seq != want {
			t.Fatalf("got[%d].Seq = %d, want %d (must be contiguous, no gaps — no double-eviction)", i, e.Envelope().Seq, want)
		}
	}

	// R2-5: Dropped() must actually count the drop-oldest evictions, not
	// stay 0 while silently discarding `overflow` events.
	if d := h.Dropped(); d != uint64(overflow) {
		t.Fatalf("Dropped() = %d, want %d (one per drop-oldest eviction)", d, overflow)
	}
}

// TestHubPublishDoesNotFanOutRingRejectedEvent is R4-3: when Ring.Add
// rejects an event outright (it alone exceeds the ring's byte budget, R3-3),
// publish must not fan it out to subscribers either — Ring.Add returning
// false means the event was NEVER stored, and delivering it anyway would
// bypass the byte protection entirely via subscriber channel buffers, which
// have no size bound of their own.
func TestHubPublishDoesNotFanOutRingRejectedEvent(t *testing.T) {
	// Build the oversized event first so RingBytes can be set just below its
	// encoded size.
	oversized := ContextEvent{
		env:     env{Kind: KindContextEvent, SandboxID: "sbx-1", Seq: 1},
		CtxKind: "model_change",
		Detail:  strings.Repeat("d", maxFieldBytes), // capped on decode, but publish() is called directly here with the pre-capped struct so eventSize reflects the encoded size
	}
	sz := len(ndjsonLine(t, oversized))

	h := NewHub(HubConfig{Port: 0, RingBytes: sz - 1})

	sub, unsub := h.Subscribe()
	defer unsub()

	h.publish(oversized)

	if got := h.Events(); len(got) != 0 {
		t.Fatalf("Events() = %+v, want empty (Ring.Add must have rejected the oversized event)", got)
	}

	select {
	case e := <-sub:
		t.Fatalf("subscriber received %+v, want nothing (Ring-rejected event must not fan out)", e)
	case <-time.After(200 * time.Millisecond):
		// Expected: no delivery.
	}
}
