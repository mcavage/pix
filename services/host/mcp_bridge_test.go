package main

import (
	"encoding/json"
	"testing"

	"pix/host/plugin"
)

// Compile-time proof the adapter satisfies the go-plugin capability interface.
var _ plugin.McpServer = (*slackMcpAdapter)(nil)

// clearSlackToken removes every token env var so the handlers degrade cleanly
// (no live Slack call) instead of depending on the ambient environment.
func clearSlackToken(t *testing.T) {
	t.Helper()
	t.Setenv("SLACK_TOKEN", "")
	t.Setenv("SLACK_USER_TOKEN", "")
	t.Setenv("SLACK_BOT_TOKEN", "")
	// A real host config may carry OAuth wiring even when the legacy env vars
	// are empty. Pin this test to the static source so it never reads the user's
	// 1Password document or calls Slack.
	resetSlackTokenSourceForTest()
	slackNewTokenSource = func() slackTokenSource { return staticSlackTokenSource{} }
	t.Cleanup(resetSlackTokenSourceForTest)
}

func TestSlackAdapterInfo(t *testing.T) {
	info, err := (slackMcpAdapter{}).Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.Name != slackMcpServerName {
		t.Errorf("Info().Name = %q, want %q", info.Name, slackMcpServerName)
	}
	if info.ProtocolVersion != slackMcpProtocol {
		t.Errorf("Info().ProtocolVersion = %q, want %q", info.ProtocolVersion, slackMcpProtocol)
	}
}

func TestSlackAdapterListTools(t *testing.T) {
	specs, err := (slackMcpAdapter{}).ListTools()
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != len(slackTools()) {
		t.Fatalf("ListTools returned %d specs, want %d", len(specs), len(slackTools()))
	}
	got := map[string]bool{}
	for _, s := range specs {
		got[s.Name] = true
		// Every InputSchema must be a valid JSON object schema.
		var m map[string]any
		if err := json.Unmarshal(s.InputSchema, &m); err != nil {
			t.Errorf("tool %q InputSchema not valid JSON: %v", s.Name, err)
			continue
		}
		if m["type"] != "object" {
			t.Errorf("tool %q schema type = %v, want object", s.Name, m["type"])
		}
	}
	for _, want := range []string{"health", "search_messages", "list_channels", "read_channel", "read_thread", "get_user", "search_users"} {
		if !got[want] {
			t.Errorf("ListTools missing slack tool %q", want)
		}
	}
}

func TestSlackAdapterCallToolRouting(t *testing.T) {
	clearSlackToken(t)
	a := slackMcpAdapter{}

	// Unknown tool -> clean error, no panic.
	if _, err := a.CallTool("does-not-exist", nil); err == nil {
		t.Fatal("CallTool on unknown tool should return an error")
	}

	// An argument-validated handler returns a structured result (no token needed):
	// proves the call routed to search_messages' handler specifically.
	out, err := a.CallTool("search_messages", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("search_messages should not hard-error without a query: %v", err)
	}
	var res map[string]any
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("search_messages result not JSON: %v", err)
	}
	if res["success"] != false {
		t.Errorf("search_messages without query: success = %v, want false", res["success"])
	}
	if _, ok := res["error"]; !ok {
		t.Errorf("search_messages without query should carry an error field: %v", res)
	}

	// A token-required handler returns a clean error (not a panic) when there is
	// no token in the environment.
	if _, err := a.CallTool("health", json.RawMessage(`{}`)); err == nil {
		t.Fatal("health without a token should return an error")
	}
}

// TestBuiltinMcpServerFor proves the built-in switch: slack resolves to the
// built-in adapter, and an unregistered name errors. The public tree has
// exactly one built-in MCP server; anything else is either a container the
// sbx gateway runs or a [plugins.mcp] external-process override.
func TestBuiltinMcpServerFor(t *testing.T) {
	srv, err := builtinMcpServerFor("slack")
	if err != nil {
		t.Fatalf("builtinMcpServerFor(slack) = %v, want the built-in adapter", err)
	}
	if _, ok := srv.(slackMcpAdapter); !ok {
		t.Errorf("builtinMcpServerFor(slack) returned %T, want slackMcpAdapter", srv)
	}

	// An unknown name errors.
	if _, err := builtinMcpServerFor("nope"); err == nil {
		t.Error("builtinMcpServerFor(nope) should error for an unregistered name")
	}
}

// TestBuiltinMcpNames proves the local-server source of truth: only slack is
// listed, gog is NEVER listed (external CLI, not bridged), and the result is
// sorted.
func TestBuiltinMcpNames(t *testing.T) {
	names := builtinMcpNames()
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	if !got["slack"] {
		t.Errorf("builtinMcpNames() = %v, want slack present", names)
	}
	if len(names) != 1 {
		t.Errorf("builtinMcpNames() = %v, want exactly [slack]", names)
	}
	if got["gog"] {
		t.Errorf("builtinMcpNames() = %v, must NOT list gog (external CLI)", names)
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Errorf("builtinMcpNames() = %v, want sorted", names)
		}
	}
}

func TestMcpServerForSelection(t *testing.T) {
	srv, cleanup, err := mcpServerFor("slack")
	if err != nil {
		t.Fatalf("mcpServerFor(slack) = %v, want built-in adapter", err)
	}
	cleanup()
	if _, ok := srv.(slackMcpAdapter); !ok {
		t.Errorf("mcpServerFor(slack) returned %T, want the built-in slackMcpAdapter", srv)
	}
	_, cleanup, err = mcpServerFor("nope")
	cleanup()
	if err == nil {
		t.Fatal("mcpServerFor on an unknown name should error")
	}
}

// TestMcpBridgeProxiesSlack drives the built-in adapter through the same
// dispatcher the stdio bridge builds, proving tool names + call routing survive
// the McpServer proxy path end to end.
func TestMcpBridgeProxiesSlack(t *testing.T) {
	clearSlackToken(t)
	srv, cleanup, err := mcpServerFor("slack")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	name, tools, handlers, err := bridgeFromMcpServer(srv)
	if err != nil {
		t.Fatal(err)
	}
	if name != slackMcpServerName {
		t.Errorf("bridge server name = %q, want %q", name, slackMcpServerName)
	}
	if len(tools) != len(slackTools()) {
		t.Errorf("bridge tool count = %d, want %d", len(tools), len(slackTools()))
	}
	if handlers["health"] == nil {
		t.Fatal("health handler not proxied through the bridge")
	}

	h := mcpDispatcher(name, tools, handlers)

	rep, ok := h(jsonObj{"id": float64(1), "method": "tools/list"})
	if !ok {
		t.Fatal("tools/list produced no reply")
	}
	if n := len(rep["result"].(jsonObj)["tools"].([]jsonObj)); n != len(slackTools()) {
		t.Errorf("tools/list through bridge returned %d tools, want %d", n, len(slackTools()))
	}

	// search_messages with no query routes through the proxy and returns a
	// structured (non-error) result.
	rep, _ = h(jsonObj{"id": float64(2), "method": "tools/call",
		"params": map[string]any{"name": "search_messages", "arguments": map[string]any{}}})
	if _, isErr := rep["error"]; isErr {
		t.Fatalf("search_messages should not error through the bridge: %v", rep)
	}

	// Unknown tool through the dispatcher -> JSON-RPC error, no panic.
	rep, _ = h(jsonObj{"id": float64(3), "method": "tools/call",
		"params": map[string]any{"name": "nope"}})
	if _, isErr := rep["error"]; !isErr {
		t.Fatal("unknown tool should produce a JSON-RPC error through the bridge")
	}
}
