package container

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

// authenticatedMCPServer mirrors services/memory/server's real requireAuth:
// /healthz stays unauthenticated, but /mcp answers 401 unless the request
// carries "Authorization: Bearer <token>" — the exact shape a real
// pix-memory container enforces once `pix setup` has generated a bearer
// token (container/authtoken.go), and the exact shape the reported bug
// ("mcp initialize at http://127.0.0.1:<port>/mcp returns 401") comes from:
// a Prober built with no token at all against an authenticated container.
func authenticatedMCPServer(t *testing.T, token string, tools []string) *httptest.Server {
	t.Helper()
	inner := jsonMCPServer(t, tools)
	t.Cleanup(inner.Close)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		bearer, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if bearer != token {
			http.Error(w, "pix-memory: unauthorized", http.StatusUnauthorized)
			return
		}
		inner.Config.Handler.ServeHTTP(w, r)
	})
	return httptest.NewServer(mux)
}

// TestHTTPProber_FailsWithoutTokenAgainstAuthenticatedServer is the RED
// anchor for the reported false-unhealthy report: a Prober built with no
// Token at all against a container that requires bearer auth on /mcp must
// fail with the real 401, not silently pass — this is exactly what a
// tokenless `container.HTTPProber{}` (the pre-fix production wiring in both
// `pix doctor` and `pix setup`) does against a real pix-memory container.
func TestHTTPProber_FailsWithoutTokenAgainstAuthenticatedServer(t *testing.T) {
	srv := authenticatedMCPServer(t, "s3cr3t-token", []string{"memory_recall"})
	defer srv.Close()

	prober := HTTPProber{}
	err := prober.Probe(srv.URL)
	if err == nil {
		t.Fatal("expected a 401 error for a tokenless probe against an authenticated server")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("error = %q, want it to name the real 401", err.Error())
	}
}

// TestHTTPProber_SendsBearerTokenWhenSet is the GREEN half: a Prober
// carrying the real bearer token authenticates successfully, and does so
// via the "Authorization: Bearer" header — never a "?token=" query
// parameter — so the token can never end up formatted into this package's
// own error strings (which only ever interpolate the bare baseURL/endpoint,
// never a full URL a caller might have embedded a token in).
func TestHTTPProber_SendsBearerTokenWhenSet(t *testing.T) {
	const token = "s3cr3t-token"
	srv := authenticatedMCPServer(t, token, []string{"memory_recall"})
	defer srv.Close()

	prober := HTTPProber{Token: token}
	if err := prober.Probe(srv.URL); err != nil {
		t.Fatalf("Probe with correct token: %v", err)
	}
}

// TestHTTPProber_WrongTokenErrorNeverContainsTheRealToken proves the "no
// leak" half explicitly: even on a failing, wrong-token probe, the returned
// error text never contains the configured token value — it can only ever
// carry the bare baseURL/endpoint and the server's own (already-redacted-
// by-construction) response body.
func TestHTTPProber_WrongTokenErrorNeverContainsTheRealToken(t *testing.T) {
	const realToken = "s3cr3t-token"
	srv := authenticatedMCPServer(t, realToken, []string{"memory_recall"})
	defer srv.Close()

	prober := HTTPProber{Token: "wrong-token"}
	err := prober.Probe(srv.URL)
	if err == nil {
		t.Fatal("expected an error for a wrong token")
	}
	if strings.Contains(err.Error(), realToken) {
		t.Fatalf("error leaked the real token: %q", err.Error())
	}
}
