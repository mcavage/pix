package monitor

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// startTestIngest binds a REAL IngestServer on an ephemeral loopback port —
// real socket, real files, no mocked handler — and returns its base URL and
// store.
func startTestIngest(t *testing.T, filter string) (string, *Store) {
	t.Helper()
	store := newTestStore(t, StoreConfig{})
	srv, err := NewIngestServer(IngestConfig{Port: 0, BindAddr: "127.0.0.1", Store: store, Filter: filter})
	if err != nil {
		t.Fatalf("NewIngestServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve returned %v after shutdown, want nil", err)
		}
	})
	return "http://" + srv.Addr(), store
}

func post(t *testing.T, url, contentType, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, contentType, strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func encodeLine(t *testing.T, e Event) string {
	t.Helper()
	line, err := Encode(e)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	return string(line)
}

func waitForTail(t *testing.T, store *Store, sandboxID, sessionID string, want int) []Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := store.Tail(sandboxID, sessionID, 0)
		if err != nil {
			t.Fatalf("Tail: %v", err)
		}
		if len(got) >= want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestIngestPersistsAndSurvivesBadLines: a valid line lands in the store, and
// neither an unparseable line, a blank line, nor an oversized one may drop
// the rest of the stream or fail the request.
func TestIngestPersistsAndSurvivesBadLines(t *testing.T) {
	base, store := startTestIngest(t, "")
	body := strings.Join([]string{
		encodeLine(t, toolEvent("sbx", "sess", "first", 1)),
		"{not json",
		"",
		strings.Repeat("A", maxIngestLine+10),
		encodeLine(t, toolEvent("sbx", "sess", "last", 2)),
	}, "\n") + "\n"

	resp := post(t, base+"/ingest", "application/x-ndjson", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ingest = %d, want 200", resp.StatusCode)
	}
	got := waitForTail(t, store, "sbx", "sess", 2)
	if len(got) != 2 {
		t.Fatalf("stored %d events, want exactly the 2 valid ones", len(got))
	}
	if got[0].(ToolEnd).ResultSummary != "first" || got[1].(ToolEnd).ResultSummary != "last" {
		t.Fatalf("stored %+v, want first then last", got)
	}
}

// An event whose ids the store refuses must be dropped, not written and not
// fatal to the request.
func TestIngestDropsEventsWithInvalidIDs(t *testing.T) {
	base, store := startTestIngest(t, "")
	resp := post(t, base+"/ingest", "application/x-ndjson",
		encodeLine(t, toolEvent("../escape", "sess", "x", 1))+"\n")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ingest = %d, want 200", resp.StatusCode)
	}
	time.Sleep(20 * time.Millisecond)
	metas, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(metas) != 0 {
		t.Fatalf("stored %d streams, want none", len(metas))
	}
}

func TestIngestFilterDropsNonMatchingStreams(t *testing.T) {
	base, store := startTestIngest(t, "keep")
	body := encodeLine(t, toolEvent("keep-me", "sess", "yes", 1)) + "\n" +
		encodeLine(t, toolEvent("other", "sess", "no", 1)) + "\n"
	post(t, base+"/ingest", "application/x-ndjson", body)
	waitForTail(t, store, "keep-me", "sess", 1)
	if got, _ := store.Tail("other", "sess", 0); len(got) != 0 {
		t.Fatalf("filtered stream stored %d events, want 0", len(got))
	}
}

func TestIngestBlobEndpointVerifiesHash(t *testing.T) {
	base, store := startTestIngest(t, "")
	text := "full tool output"
	resp := post(t, base+"/blob", "application/json",
		fmt.Sprintf(`{"hash":%q,"bytes":%d,"text":%q}`, hashOf(text), len(text), text))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /blob = %d, want 200", resp.StatusCode)
	}
	blobs := readStoredBlobs(t, store)
	if len(blobs) != 1 || blobs[0].Text != text {
		t.Fatalf("stored blobs = %+v, want the posted text", blobs)
	}

	bad := post(t, base+"/blob", "application/json", `{"hash":"deadbeef","text":"other"}`)
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /blob with a mismatched hash = %d, want 400", bad.StatusCode)
	}
	if malformed := post(t, base+"/blob", "application/json", "{"); malformed.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /blob with malformed json = %d, want 400", malformed.StatusCode)
	}
	if got := len(readStoredBlobs(t, store)); got != 1 {
		t.Fatalf("stored %d blobs after two rejected posts, want 1", got)
	}
}

// The ingest endpoint carries full agent context and tool output with NO
// auth, so the default bind must be loopback and nothing else.
func TestIngestBindsLoopbackByDefault(t *testing.T) {
	store := newTestStore(t, StoreConfig{})
	srv, err := NewIngestServer(IngestConfig{Port: 0, Store: store})
	if err != nil {
		t.Fatalf("NewIngestServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go srv.Serve(ctx) //nolint:errcheck // shutdown path is covered elsewhere
	host, _, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", srv.Addr(), err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		t.Fatalf("default bind is %q, want a loopback address", host)
	}
}

func TestNewIngestServerRequiresStoreAndReportsBindFailure(t *testing.T) {
	if _, err := NewIngestServer(IngestConfig{Port: 0}); err == nil {
		t.Fatal("NewIngestServer with no Store = nil error, want an error")
	}
	store := newTestStore(t, StoreConfig{})
	first, err := NewIngestServer(IngestConfig{Port: 0, Store: store})
	if err != nil {
		t.Fatalf("NewIngestServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- first.Serve(ctx) }()
	_, port, err := net.SplitHostPort(first.Addr())
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	var n int
	fmt.Sscanf(port, "%d", &n)
	// The port is taken, so the CONSTRUCTOR must report it — that is why it
	// binds eagerly instead of failing asynchronously inside Serve.
	if _, err := NewIngestServer(IngestConfig{Port: n, Store: store}); err == nil {
		t.Fatal("NewIngestServer on a taken port = nil error, want a bind error")
	}
	cancel()
	<-done
}
