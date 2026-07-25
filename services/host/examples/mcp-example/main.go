// Command mcp-example is a REFERENCE example of an external MCP server plugin:
// how an operator ships their OWN out-of-process MCP binary that OVERRIDES
// the built-in bridge without ever linking into the public tree.
//
// It is NOT wired into the bridge by default. To use it, a user points
// ~/.config/pi-stack/config.toml's [plugins.mcp] at this binary:
//
//	[plugins.mcp]
//	impl = "example"
//	path = "/path/to/mcp-example"
//	sha  = "<sha256 of the binary>"
//
// The MCP bridge (services/host/mcp_bridge.go, mcpServerFor) then sha-verifies
// and launches it ONCE at startup as a go-plugin subprocess, dispenses the "mcp"
// capability, and proxies every tools/list + tools/call to it. This example
// serves a single trivial "echo" tool so the override can be proven end-to-end.
package main

import (
	"encoding/json"

	goplugin "github.com/hashicorp/go-plugin"

	"pi-stack/host/plugin"
)

// exampleMcp is a trivial McpServer: it advertises one "echo" tool and returns a
// fixed JSON result, enough to prove dispense + round-trip over real RPC.
type exampleMcp struct{}

func (exampleMcp) Info() (plugin.ServerInfo, error) {
	return plugin.ServerInfo{Name: "example-mcp", Version: "0.0.1", ProtocolVersion: "2025-06-18"}, nil
}

func (exampleMcp) ListTools() ([]plugin.ToolSpec, error) {
	return []plugin.ToolSpec{{
		Name:        "echo",
		Description: "echoes its input back",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"msg":{"type":"string"}}}`),
	}}, nil
}

func (exampleMcp) CallTool(name string, args json.RawMessage) (json.RawMessage, error) {
	return json.RawMessage(`{"echoed":true}`), nil
}

func main() {
	plugin.Serve(map[string]goplugin.Plugin{"mcp": &plugin.McpPlugin{Impl: exampleMcp{}}})
}
