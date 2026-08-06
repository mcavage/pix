package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startTestIngest binds a REAL IngestServer on an ephemeral loopback port —
// real socket, real files, no mocked handler — and returns its base URL and
// store.
func startTestIngest(t *testing.T) (string, *Store) {
	t.Helper()
	store := newTestStore(t, StoreConfig{})
	srv, err := NewIngestServer(IngestConfig{Port: 0, BindAddr: "127.0.0.1", Store: store})
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
		got := mustTail(t, store, sandboxID, sessionID, 0)
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
	base, store := startTestIngest(t)
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
	base, store := startTestIngest(t)
	resp := post(t, base+"/ingest", "application/x-ndjson",
		encodeLine(t, toolEvent("../escape", "sess", "x", 1))+"\n")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ingest = %d, want 200", resp.StatusCode)
	}
	time.Sleep(20 * time.Millisecond)
	metas := mustList(t, store)
	if len(metas) != 0 {
		t.Fatalf("stored %d streams, want none", len(metas))
	}
}

func TestIngestBlobEndpointVerifiesHash(t *testing.T) {
	base, store := startTestIngest(t)
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

// TestIngestRedactsSecretShapesEndToEnd drives the full pipeline a real tap
// uses — HTTP POST to /ingest and /blob, through Store, to bytes on disk —
// and proves every canary shape (AWS key, Google AIza key, JWT,
// Authorization: Bearer token) is scrubbed from BOTH events and blob bodies,
// while ordinary text passes through byte-for-byte (no false redaction).
func TestIngestRedactsSecretShapesEndToEnd(t *testing.T) {
	base, store := startTestIngest(t)
	const ordinary = "ran ls -l in /tmp, read 42 files, model=claude-opus-5"

	body := encodeLine(t, toolEvent("sbx", "sess", "leak: "+strings.Join(allCanaries, " "), 1)) + "\n" +
		encodeLine(t, toolEvent("sbx", "sess", ordinary, 2)) + "\n"
	if resp := post(t, base+"/ingest", "application/x-ndjson", body); resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ingest = %d, want 200", resp.StatusCode)
	}
	secretBlob := "tool output with " + strings.Join(allCanaries, " and ")
	for _, text := range []string{secretBlob, ordinary} {
		payload := fmt.Sprintf(`{"hash":%q,"text":%q}`, hashOf(text), text)
		if resp := post(t, base+"/blob", "application/json", payload); resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /blob = %d, want 200", resp.StatusCode)
		}
	}

	if got := waitForTail(t, store, "sbx", "sess", 2); len(got) != 2 {
		t.Fatalf("stored %d events, want 2", len(got))
	}
	for name, path := range map[string]string{
		"events": streamFile(store, "sbx", "sess"),
		"blobs":  filepath.Join(store.cfg.Root, "blobs.ndjson"),
	} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s file: %v", name, err)
		}
		for _, c := range allCanaries {
			if strings.Contains(string(raw), c) {
				t.Errorf("%s file still contains canary %q:\n%s", name, c, raw)
			}
		}
		if !strings.Contains(string(raw), redactionMarker) {
			t.Errorf("%s file has no %s marker, so nothing was scrubbed:\n%s", name, redactionMarker, raw)
		}
		if !strings.Contains(string(raw), ordinary) {
			t.Errorf("%s file lost or altered the ordinary text %q (false redaction):\n%s", name, ordinary, raw)
		}
	}
	blobs := readStoredBlobs(t, store)
	if len(blobs) != 2 || !blobs[0].Redacted || blobs[1].Redacted || blobs[1].Text != ordinary {
		t.Fatalf("blobs = %+v, want the secret one marked redacted and the ordinary one stored verbatim", blobs)
	}
}

// The constructor contract, in one place: Store is required, the default
// bind is LOOPBACK (this endpoint carries full agent context and tool output
// with no auth), and a taken port is reported HERE rather than
// asynchronously from Serve — which is why it binds eagerly.
func TestNewIngestServerRequiresStoreBindsLoopbackAndReportsBindFailure(t *testing.T) {
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

	host, port, err := net.SplitHostPort(first.Addr())
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", first.Addr(), err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		t.Fatalf("default bind is %q, want a loopback address", host)
	}
	var n int
	fmt.Sscanf(port, "%d", &n)
	if _, err := NewIngestServer(IngestConfig{Port: n, Store: store}); err == nil {
		t.Fatal("NewIngestServer on a taken port = nil error, want a bind error")
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Serve returned %v after shutdown, want nil", err)
	}
}

// TestIngestUnknownKindRawJSONRedactedEndToEnd drives the full pipeline —
// HTTP POST to /ingest and /blob, through Store, to bytes on disk — with an
// UNRECOGNIZED kind whose raw JSON carries an Authorization Bearer value and
// a generic secret assignment, alongside a known event and a JSON blob body.
// It proves the security property AND the structural one together: no canary
// reaches disk, yet every stored line (and the blob text) is still valid
// JSON with the unknown event's kind and unknown fields retained. This is
// the regression test for the whole-line redaction pass corrupting stored
// JSON by consuming the key/colon/quotes around a matched secret.
func TestIngestUnknownKindRawJSONRedactedEndToEnd(t *testing.T) {
	base, store := startTestIngest(t)
	const (
		bearerTok  = "canary.e2e-bearer-9876543210"
		genericVal = "canaryE2EGenericValue123456"
	)
	unknownLine := `{"kind":"tap_v99_probe","sandboxId":"sbx","sessionId":"sess","turnId":"t1","seq":1,"ts":1700000000123,` +
		`"authorization":"Bearer ` + bearerTok + `",` +
		`"probe":{"api_key":"` + genericVal + `","note":"ordinary probe detail"}}`
	known := toolEvent("sbx", "sess", `curl -H '{"authorization":"Bearer `+bearerTok+`"}'`, 2)
	body := unknownLine + "\n" + encodeLine(t, known) + "\n"
	if resp := post(t, base+"/ingest", "application/x-ndjson", body); resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /ingest = %d, want 200", resp.StatusCode)
	}
	blobText := `{"authorization":"Bearer ` + bearerTok + `","api_key":"` + genericVal + `","kept":"ordinary"}`
	payload := fmt.Sprintf(`{"hash":%q,"text":%q}`, hashOf(blobText), blobText)
	if resp := post(t, base+"/blob", "application/json", payload); resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /blob = %d, want 200", resp.StatusCode)
	}
	if got := waitForTail(t, store, "sbx", "sess", 2); len(got) != 2 {
		t.Fatalf("stored %d events, want 2", len(got))
	}

	raw, err := os.ReadFile(streamFile(store, "sbx", "sess"))
	if err != nil {
		t.Fatalf("read stream file: %v", err)
	}
	var sawUnknown bool
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("stored line is not valid JSON (redaction corrupted it): %v\n%s", err, line)
		}
		if m["kind"] != "tap_v99_probe" {
			continue
		}
		sawUnknown = true
		probe, ok := m["probe"].(map[string]any)
		if !ok || probe["note"] != "ordinary probe detail" {
			t.Errorf("unknown event lost its unknown fields: %s", line)
		}
		if !strings.Contains(line, `"ts":1700000000123`) {
			t.Errorf("unknown event ts lost its exact wire form: %s", line)
		}
	}
	if !sawUnknown {
		t.Errorf("unknown kind tap_v99_probe was not retained:\n%s", raw)
	}
	for _, c := range []string{bearerTok, genericVal} {
		if strings.Contains(string(raw), c) {
			t.Errorf("events file still contains canary %q:\n%s", c, raw)
		}
	}

	blobs := readStoredBlobs(t, store)
	if len(blobs) != 1 || !blobs[0].Redacted {
		t.Fatalf("blobs = %+v, want one, marked redacted", blobs)
	}
	var b map[string]any
	if err := json.Unmarshal([]byte(blobs[0].Text), &b); err != nil {
		t.Fatalf("redacted blob text is no longer valid JSON: %v\n%s", err, blobs[0].Text)
	}
	if b["kept"] != "ordinary" {
		t.Errorf("blob lost its ordinary field: %s", blobs[0].Text)
	}
	for _, c := range []string{bearerTok, genericVal} {
		if strings.Contains(blobs[0].Text, c) {
			t.Errorf("blob text still contains canary %q: %s", c, blobs[0].Text)
		}
	}
}
