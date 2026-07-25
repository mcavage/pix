package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"
)

// This is the shared host-side JSON-RPC client the launcher uses to drive the
// memory (:11435) and knowledge (:11436) daemons from the CLI, so a user can
// inspect and repair the agent's brain WITHOUT launching a sandbox. It is
// deliberately tiny (stdlib only) and short-timeout so a down daemon never hangs
// a command.
//
// Ports honor the same env overrides `serve` uses (MEMORY_PORT / KNOWLEDGE_PORT)
// so a non-default bind stays reachable from the CLI.

const (
	memoryPortDefault    = 11435
	knowledgePortDefault = 11436

	// exitServiceDown is the distinct exit code CLI verbs return when the target
	// daemon is unreachable, so scripts can tell "service down" (3) apart from a
	// usage error (2) or a generic failure (1).
	exitServiceDown = 3
)

// rpcClient talks JSON-RPC 2.0 to a local daemon on 127.0.0.1:Port.
type rpcClient struct {
	Port    int
	Timeout time.Duration
}

// memoryClient / knowledgeClient return clients pointed at the right port,
// honoring the MEMORY_PORT / KNOWLEDGE_PORT env overrides.
func memoryClient() rpcClient {
	return rpcClient{Port: portFromEnv("MEMORY_PORT", memoryPortDefault), Timeout: 3 * time.Second}
}
func knowledgeClient() rpcClient {
	return rpcClient{Port: portFromEnv("KNOWLEDGE_PORT", knowledgePortDefault), Timeout: 3 * time.Second}
}

// portFromEnv reads a port from an env var, falling back to def when unset or
// unparseable.
func portFromEnv(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// Up reports whether the daemon is reachable within a short dial timeout.
func (c rpcClient) Up() bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", c.Port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// Call POSTs a JSON-RPC request and returns the decoded `result` object. A
// JSON-RPC error envelope maps to a Go error; a transport failure (daemon down)
// maps to errServiceDown so callers can exit with exitServiceDown.
func (c rpcClient) Call(method string, params map[string]any) (map[string]any, error) {
	if params == nil {
		params = map[string]any{}
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 3 * time.Second
	}
	client := &http.Client{Timeout: timeout}
	resp, err := client.Post(fmt.Sprintf("http://127.0.0.1:%d/", c.Port), "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, errServiceDown
	}
	defer resp.Body.Close()
	var parsed map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decoding %s response: %w", method, err)
	}
	if e, ok := parsed["error"].(map[string]any); ok {
		return nil, fmt.Errorf("%v", e["message"])
	}
	result, _ := parsed["result"].(map[string]any)
	if result == nil {
		result = map[string]any{}
	}
	return result, nil
}

// errServiceDown is the sentinel returned when a daemon can't be reached, so
// callers render a consistent "start it with pix serve" message + exit 3.
var errServiceDown = fmt.Errorf("service unreachable")

// asList coerces a decoded JSON array (of objects) into []map[string]any.
func asList(v any) []map[string]any {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(arr))
	for _, e := range arr {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// str safely reads a string field from a decoded JSON object.
func str(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}
