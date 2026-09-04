package inference

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"pix/host/hostenv"
	"pix/host/sys/systest"
)

// tagRow is one row this file's fake /api/tags servers emit.
type tagRow struct {
	Name       string `json:"name"`
	RemoteHost string `json:"remote_host,omitempty"`
	Size       int64  `json:"size"`
}

func tagsBody(rows ...tagRow) []byte {
	b, _ := json.Marshal(map[string]any{"models": rows})
	return b
}

// ollamaDetectEnv points OLLAMA_HOST at handler and wires LookPath, so a test
// can drive DetectOllama end to end against a fake daemon.
func ollamaDetectEnv(t *testing.T, cliPresent bool, hostOverride string, handler http.HandlerFunc) hostenv.Env {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	host := hostOverride
	if host == "" {
		host = u.Host
	}
	fake := &systest.Fake{
		LookPathFn: func(string) (string, error) {
			if cliPresent {
				return "/usr/local/bin/ollama", nil
			}
			return "", fmt.Errorf("not found")
		},
		GetenvFn: func(name string) string {
			if name == "OLLAMA_HOST" {
				return host
			}
			return ""
		},
	}
	return hostenv.Env{System: fake}
}

func TestDetectOllama_LocalReachable(t *testing.T) {
	env := ollamaDetectEnv(t, true, "", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(tagsBody(tagRow{Name: "nomic-embed-text:latest", Size: 274_000_000}))
	})
	st := DetectOllama(env)
	if !st.CLIPresent {
		t.Error("CLIPresent = false, want true")
	}
	if st.Mode != OllamaModeLocal {
		t.Errorf("Mode = %q, want local (127.0.0.1 endpoint)", st.Mode)
	}
	if !st.Reachable || st.ListErr != nil {
		t.Fatalf("Reachable = %v, ListErr = %v", st.Reachable, st.ListErr)
	}
	if !st.HasModel("nomic-embed-text:latest") {
		t.Errorf("Models = %+v, want nomic-embed-text:latest present", st.Models)
	}
	if !st.CanPull() {
		t.Error("CanPull() = false for a reachable LOCAL endpoint, want true")
	}
}

// TestDetectOllama_RemoteNeverOffersPull asserts the MODE/CanPull
// classification for a non-loopback OLLAMA_HOST. It deliberately points at
// an address nothing answers on: reachability is covered by the local test
// above, this one only proves mode is derived from the endpoint HOST string,
// never from whether the daemon happens to respond.
func TestDetectOllama_RemoteNeverOffersPull(t *testing.T) {
	fake := &systest.Fake{
		LookPathFn: func(string) (string, error) { return "/usr/local/bin/ollama", nil },
		GetenvFn:   func(name string) string { return "team-ollama.internal:11434" },
	}
	st := DetectOllama(hostenv.Env{System: fake})
	if st.Mode != OllamaModeRemote {
		t.Errorf("Mode = %q, want remote for a non-loopback OLLAMA_HOST", st.Mode)
	}
	if st.CanPull() {
		t.Error("CanPull() = true for a remote endpoint, want false — never offer a pull across the network")
	}
}

func TestDetectOllama_CLIAbsent(t *testing.T) {
	env := ollamaDetectEnv(t, false, "", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	st := DetectOllama(env)
	if st.CLIPresent {
		t.Error("CLIPresent = true, want false")
	}
	if st.Reachable {
		t.Error("Reachable = true against a 503, want false")
	}
	if st.ListErr == nil {
		t.Error("ListErr = nil, want the HTTP failure recorded")
	}
	if st.CanPull() {
		t.Error("CanPull() = true when unreachable, want false")
	}
}

func TestDetectOllama_UnreachableTimesOutFast(t *testing.T) {
	orig := ollamaDetectTimeout
	ollamaDetectTimeout = 50 * time.Millisecond
	defer func() { ollamaDetectTimeout = orig }()
	// An address nothing listens on: 127.0.0.1 with a port test servers never
	// bind, resolved instantly as "connection refused" rather than a real
	// timeout — either way DetectOllama must report unreachable, not hang.
	fake := &systest.Fake{
		LookPathFn: func(string) (string, error) { return "/usr/local/bin/ollama", nil },
		GetenvFn:   func(string) string { return "127.0.0.1:1" },
	}
	st := DetectOllama(hostenv.Env{System: fake})
	if st.Reachable {
		t.Error("Reachable = true for a closed port, want false")
	}
}

func TestListOllamaModels_DropsUnsafeTagNames(t *testing.T) {
	env := ollamaDetectEnv(t, true, "", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"models":[{"name":"good:latest","size":1},{"name":"\u001b[2Kbad","size":1}]}`)
	})
	seen, err := ListOllamaModels(env)
	if err != nil {
		t.Fatalf("ListOllamaModels: %v", err)
	}
	if _, ok := seen["good:latest"]; !ok {
		t.Errorf("seen = %+v, want good:latest present", seen)
	}
	for name := range seen {
		if strings.ContainsAny(name, "\x1b\r\n") {
			t.Errorf("an unsafe tag name %q survived the ingestion boundary", name)
		}
	}
	if len(seen) != 1 {
		t.Errorf("len(seen) = %d, want exactly the one safe row", len(seen))
	}
}

func TestContainerOllamaHost(t *testing.T) {
	local := OllamaStatus{Mode: OllamaModeLocal, Endpoint: OllamaEndpoint{Host: "127.0.0.1", Port: 11434}}
	if got := ContainerOllamaHost(local); got != "host.docker.internal:11434" {
		t.Errorf("local -> %q, want host.docker.internal:11434", got)
	}
	remote := OllamaStatus{Mode: OllamaModeRemote, Endpoint: OllamaEndpoint{Host: "team-ollama.internal", Port: 11434}}
	if got := ContainerOllamaHost(remote); got != "team-ollama.internal:11434" {
		t.Errorf("remote -> %q, want the endpoint passed through unchanged", got)
	}
}
