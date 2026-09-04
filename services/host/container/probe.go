package container

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// HTTPProber is the production Prober: a real /healthz GET followed by a
// minimal MCP Streamable-HTTP round trip — `initialize` then `tools/list` —
// against /mcp. It requires at least one tool name back, which is as far as
// this package goes toward "the right tools": naming pix-memory's exact tool
// set is server/server.go's concern, not the host adapter's.
type HTTPProber struct {
	// Client is the http.Client used for both requests. A nil Client falls
	// back to a client with a short default timeout, so production callers
	// never have to build one by hand.
	Client *http.Client
	// Timeout bounds each of the two requests when Client is nil-defaulted.
	// Zero uses DefaultProbeTimeout.
	Timeout time.Duration
	// Token is the pix-memory bearer token (container.ReadMemoryAuthToken),
	// sent as "Authorization: Bearer <token>" on the /mcp initialize and
	// tools/list requests — never as a "?token=" query parameter, so it can
	// never be formatted into this package's own error strings (which only
	// ever interpolate the bare baseURL/endpoint, never a full request URL).
	// Empty leaves requests unauthenticated, matching pix-memory's own
	// pre-`pix setup` posture (no token generated yet). /healthz never needs
	// it: services/memory/server.NewMux serves /healthz unauthenticated by
	// design.
	Token string
}

// DefaultProbeTimeout bounds one HTTPProber request when neither Client nor
// Timeout customizes it.
const DefaultProbeTimeout = 5 * time.Second

// WithToken implements TokenAuthenticated: it returns a copy of p carrying
// token, never mutating the receiver.
func (p HTTPProber) WithToken(token string) Prober {
	p.Token = token
	return p
}

func (p HTTPProber) client() *http.Client {
	if p.Client != nil {
		return p.Client
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultProbeTimeout
	}
	return &http.Client{Timeout: timeout}
}

// Probe implements Prober: GET baseURL/healthz, then an MCP
// initialize+tools/list round trip against baseURL/mcp.
func (p HTTPProber) Probe(baseURL string) error {
	client := p.client()
	if err := probeHealthz(client, baseURL); err != nil {
		return err
	}
	tools, err := probeMCPTools(client, baseURL, p.Token)
	if err != nil {
		return err
	}
	if len(tools) == 0 {
		return fmt.Errorf("mcp tools/list at %s/mcp returned zero tools", baseURL)
	}
	return nil
}

func probeHealthz(client *http.Client, baseURL string) error {
	resp, err := client.Get(strings.TrimRight(baseURL, "/") + "/healthz")
	if err != nil {
		return fmt.Errorf("GET %s/healthz: %w", baseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("GET %s/healthz: status %d: %s", baseURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

// jsonRPCRequest and jsonRPCResponse are the minimal envelope shapes this
// package needs from the MCP Streamable HTTP transport
// (https://modelcontextprotocol.io): JSON-RPC 2.0 over POST /mcp, with an
// optional `Mcp-Session-Id` response header a client must echo back on
// subsequent requests in the same session.
type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

const mcpProtocolVersion = "2025-06-18"

// probeMCPTools performs the minimal handshake needed to call tools/list:
// initialize, then tools/list, carrying forward any session id the server
// assigns. It returns the tool names reported.
func probeMCPTools(client *http.Client, baseURL, token string) ([]string, error) {
	endpoint := strings.TrimRight(baseURL, "/") + "/mcp"

	initParams := map[string]any{
		"protocolVersion": mcpProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "pix-setup-probe", "version": "1"},
	}
	_, sessionID, err := mcpCall(client, endpoint, "", token, jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "initialize", Params: initParams})
	if err != nil {
		return nil, fmt.Errorf("mcp initialize at %s: %w", endpoint, err)
	}

	raw, _, err := mcpCall(client, endpoint, sessionID, token, jsonRPCRequest{JSONRPC: "2.0", ID: 2, Method: "tools/list", Params: map[string]any{}})
	if err != nil {
		return nil, fmt.Errorf("mcp tools/list at %s: %w", endpoint, err)
	}

	var listed struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &listed); err != nil {
		return nil, fmt.Errorf("parse tools/list result: %w", err)
	}
	names := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
	}
	return names, nil
}

// mcpCall issues one JSON-RPC request against a Streamable HTTP MCP
// endpoint and returns its result payload plus any `Mcp-Session-Id` the
// server assigned. The transport may answer either `application/json`
// directly or `text/event-stream` (one or more `data: <json>` frames,
// architecture §8.1: "the transport may return plain JSON responses and use
// streams only when an operation needs them") — both are handled.
func mcpCall(client *http.Client, endpoint, sessionID, token string, req jsonRPCRequest) (json.RawMessage, string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, "", err
	}
	httpReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	if sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", sessionID)
	}
	if token != "" {
		// Header only, never a "?token=" query parameter on endpoint: the
		// latter would land the secret in this function's own request URL,
		// which a future caller could all too easily log or format into an
		// error — exactly the leak this package's error strings (mcp
		// initialize/tools/list above) are written to avoid by interpolating
		// only the bare endpoint.
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	newSession := resp.Header.Get("Mcp-Session-Id")
	if newSession == "" {
		newSession = sessionID
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		out, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, newSession, fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(out)))
	}

	contentType := resp.Header.Get("Content-Type")
	var payload []byte
	if strings.Contains(contentType, "text/event-stream") {
		payload, err = firstSSEData(resp.Body)
	} else {
		payload, err = io.ReadAll(resp.Body)
	}
	if err != nil {
		return nil, newSession, err
	}
	if len(strings.TrimSpace(string(payload))) == 0 {
		// A notification-shaped response (no body) has no result to read; the
		// caller only reaches here for request methods that always answer.
		return nil, newSession, fmt.Errorf("empty response body")
	}

	var rpcResp jsonRPCResponse
	if err := json.Unmarshal(payload, &rpcResp); err != nil {
		return nil, newSession, fmt.Errorf("parse json-rpc response: %w", err)
	}
	if rpcResp.Error != nil {
		return nil, newSession, fmt.Errorf("json-rpc error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}
	return rpcResp.Result, newSession, nil
}

// firstSSEData reads a `text/event-stream` body and returns the first
// event's `data:` payload, joining multiple `data:` lines within that one
// event per the SSE spec.
func firstSSEData(r io.Reader) ([]byte, error) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var data []string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if len(data) > 0 {
				return []byte(strings.Join(data, "\n")), nil
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(data) > 0 {
		return []byte(strings.Join(data, "\n")), nil
	}
	return nil, fmt.Errorf("no data frame in event stream")
}
