package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// startTestIngest binds a real IngestServer on an ephemeral loopback port
// and returns its base URL plus the Store/BlobStore it persists to. Uses a
// REAL socket end to end (no mocked http.Handler) per the story's "real
// loopback/files tests, no mocks" requirement.
func startTestIngest(t *testing.T, filter string) (baseURL string, store *Store, blobs *BlobStore) {
	t.Helper()
	store = newTestStore(t, StoreConfig{})
	blobs = newTestBlobStore(t, BlobStoreConfig{})
	srv, err := NewIngestServer(IngestConfig{Port: 0, BindAddr: "127.0.0.1", Store: store, Blobs: blobs, Filter: filter})
	if err != nil {
		t.Fatalf("NewIngestServer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Start(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for srv.Addr() == "" {
		if time.Now().After(deadline) {
			t.Fatal("ingest server never bound")
		}
		time.Sleep(time.Millisecond)
	}
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Start() returned error after shutdown: %v", err)
		}
	})
	return "http://" + srv.Addr(), store, blobs
}

func postNDJSON(t *testing.T, baseURL string, lines ...string) *http.Response {
	t.Helper()
	body := strings.Join(lines, "\n") + "\n"
	resp, err := http.Post(baseURL+"/ingest", "application/x-ndjson", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /ingest: %v", err)
	}
	return resp
}

func TestIngestServerPersistsValidEventsToStore(t *testing.T) {
	base, store, _ := startTestIngest(t, "")
	line, err := Encode(toolEvent("sbx", "sess", "1", "t1"))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	resp := postNDJSON(t, base, string(line))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ingest status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	deadline := time.Now().Add(time.Second)
	for {
		got, err := store.Tail("sbx", "sess", 0)
		if err != nil {
			t.Fatalf("Tail: %v", err)
		}
		if len(got) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Tail() never observed the ingested event, got %d", len(got))
		}
		time.Sleep(time.Millisecond)
	}
}

func TestIngestServerSkipsUnparseableLinesButKeepsGoing(t *testing.T) {
	base, store, _ := startTestIngest(t, "")
	good1, _ := Encode(toolEvent("sbx", "sess", "1", "t1"))
	good2, _ := Encode(toolEvent("sbx", "sess", "1", "t2"))
	resp := postNDJSON(t, base, string(good1), "not json at all", string(good2))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ingest status = %d, want 200 (a bad line must not fail the request)", resp.StatusCode)
	}
	resp.Body.Close()

	waitForTailCount(t, store, "sbx", "sess", 2)
}

func waitForTailCount(t *testing.T, store *Store, sandboxID, sessionID string, n int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		got, err := store.Tail(sandboxID, sessionID, 0)
		if err != nil {
			t.Fatalf("Tail: %v", err)
		}
		if len(got) == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("Tail() = %d events after 1s, want %d", len(got), n)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestIngestServerFilterDropsNonMatchingSandbox(t *testing.T) {
	base, store, _ := startTestIngest(t, "match-me")
	matching, _ := Encode(toolEvent("sbx-match-me-1", "sess", "1", "t1"))
	other, _ := Encode(toolEvent("sbx-other", "sess2", "1", "t2"))
	resp := postNDJSON(t, base, string(matching), string(other))
	resp.Body.Close()

	waitForTailCount(t, store, "sbx-match-me-1", "sess", 1)
	got, err := store.Tail("sbx-other", "sess2", 0)
	if err != nil {
		t.Fatalf("Tail: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Tail(non-matching sandbox) = %d events, want 0 (filtered out)", len(got))
	}
}

func TestIngestServerBlobPostVerifiesHashAndPersists(t *testing.T) {
	base, _, blobs := startTestIngest(t, "")
	text := "full tool output text"
	body, _ := json.Marshal(Blob{Hash: hashOf(text), Text: text})
	resp, err := http.Post(base+"/blob", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /blob: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /blob status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()

	got, ok, err := blobs.Get(hashOf(text))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("blob was not persisted by the ingest server")
	}
	if got.Text != text {
		t.Errorf("Get().Text = %q, want %q", got.Text, text)
	}
}

func TestIngestServerBlobPostRejectsMismatchedHash(t *testing.T) {
	base, _, _ := startTestIngest(t, "")
	body, _ := json.Marshal(Blob{Hash: strings.Repeat("a", 64), Text: "hello"})
	resp, err := http.Post(base+"/blob", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /blob: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("POST /blob (mismatched hash) status = %d, want 400", resp.StatusCode)
	}
}

func TestIngestServerHealthz(t *testing.T) {
	base, _, _ := startTestIngest(t, "")
	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz status = %d, want 200", resp.StatusCode)
	}
}

func TestIngestServerBindsLoopbackOnly(t *testing.T) {
	base, _, _ := startTestIngest(t, "")
	if !strings.HasPrefix(base, "http://127.0.0.1:") {
		t.Errorf("ingest server bound %q, want loopback", base)
	}
}

func TestNewIngestServerRequiresStoreAndBlobs(t *testing.T) {
	store := newTestStore(t, StoreConfig{})
	blobs := newTestBlobStore(t, BlobStoreConfig{})
	if _, err := NewIngestServer(IngestConfig{Blobs: blobs}); err == nil {
		t.Error("NewIngestServer(no Store) = nil error, want a requirement error")
	}
	if _, err := NewIngestServer(IngestConfig{Store: store}); err == nil {
		t.Error("NewIngestServer(no Blobs) = nil error, want a requirement error")
	}
}

func TestIngestServerBindErrorOnPortInUse(t *testing.T) {
	base, _, _ := startTestIngest(t, "")
	var port int
	if _, err := fmt.Sscanf(strings.TrimPrefix(base, "http://127.0.0.1:"), "%d", &port); err != nil {
		t.Fatalf("parse port from %q: %v", base, err)
	}
	store := newTestStore(t, StoreConfig{})
	blobs := newTestBlobStore(t, BlobStoreConfig{})
	srv, err := NewIngestServer(IngestConfig{Port: port, BindAddr: "127.0.0.1", Store: store, Blobs: blobs})
	if err != nil {
		t.Fatalf("NewIngestServer: %v", err)
	}
	if err := srv.Start(context.Background()); err == nil {
		t.Error("Start() on an already-bound port = nil error, want a bind failure")
	}
}
