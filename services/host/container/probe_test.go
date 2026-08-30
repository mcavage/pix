package container

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// jsonMCPServer is a minimal fake MCP Streamable-HTTP server: it answers
// `initialize` and `tools/list` as plain JSON (no SSE), which
// architecture §8.1 explicitly allows for a local deployment. It exists to
// prove HTTPProber's request/response handling independent of the real
// modelcontextprotocol/go-sdk server this host module does not depend on.
func jsonMCPServer(t *testing.T, tools []string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int    `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		w.Header().Set("Mcp-Session-Id", "test-session")
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "initialize":
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"protocolVersion": mcpProtocolVersion},
			})
		case "tools/list":
			toolList := make([]map[string]any, 0, len(tools))
			for _, name := range tools {
				toolList = append(toolList, map[string]any{"name": name})
			}
			json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"tools": toolList},
			})
		default:
			http.Error(w, "unknown method", 400)
		}
	})
	return httptest.NewServer(mux)
}

func TestHTTPProber_Success(t *testing.T) {
	srv := jsonMCPServer(t, []string{"memory_recall", "memory_remember"})
	defer srv.Close()

	prober := HTTPProber{}
	if err := prober.Probe(srv.URL); err != nil {
		t.Fatalf("Probe: %v", err)
	}
}

func TestHTTPProber_FailsOnUnhealthy(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	prober := HTTPProber{}
	if err := prober.Probe(srv.URL); err == nil {
		t.Fatal("expected an error for a 503 /healthz")
	}
}

func TestHTTPProber_FailsOnZeroTools(t *testing.T) {
	srv := jsonMCPServer(t, nil)
	defer srv.Close()

	prober := HTTPProber{}
	if err := prober.Probe(srv.URL); err == nil {
		t.Fatal("expected an error for zero tools")
	}
}

func TestHTTPProber_FailsWhenUnreachable(t *testing.T) {
	prober := HTTPProber{}
	if err := prober.Probe("http://127.0.0.1:1"); err == nil {
		t.Fatal("expected an error for an unreachable endpoint")
	}
}
