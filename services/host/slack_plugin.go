// slackMcpAdapter exposes the built-in Slack MCP server (slack.go) through the
// go-plugin plugin.McpServer interface, so the same tools + handlers that
// runSlack() serves over stdio can also be dispensed as a go-plugin capability.
//
// This is ADDITIVE: it reuses slack.go's slackTools() / slackToolHandlers() /
// the MCP scaffolding in util.go unchanged. Nothing here mutates existing files.

package main

import (
	"encoding/json"
	"fmt"
	"os"

	goplugin "github.com/hashicorp/go-plugin"

	"pi-stack/host/plugin"
)

// These mirror the values runSlack()'s dispatcher reports at initialize (see
// mcpDispatcher in util.go), so the plugin surface matches the stdio surface.
const (
	slackMcpServerName = "pi-stack-slack"
	slackMcpVersion    = "0.0.1"
	slackMcpProtocol   = "2025-06-18"
)

// slackMcpAdapter implements plugin.McpServer over slack.go's existing tools and
// handlers. It holds no state: slackToolHandlers() builds fresh closures and the
// user-name cache in slack.go is package-global, exactly as under runSlack().
type slackMcpAdapter struct{}

func (slackMcpAdapter) Info() (plugin.ServerInfo, error) {
	return plugin.ServerInfo{
		Name:            slackMcpServerName,
		Version:         slackMcpVersion,
		ProtocolVersion: slackMcpProtocol,
	}, nil
}

func (slackMcpAdapter) ListTools() ([]plugin.ToolSpec, error) {
	tools := slackTools()
	specs := make([]plugin.ToolSpec, 0, len(tools))
	for _, t := range tools {
		// Reuse mcpTool.schema() so the JSON Schema is byte-identical to what the
		// stdio dispatcher emits; ToolSpec carries only the inputSchema subobject.
		schema, err := json.Marshal(t.schema()["inputSchema"])
		if err != nil {
			return nil, err
		}
		specs = append(specs, plugin.ToolSpec{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: schema,
		})
	}
	return specs, nil
}

func (slackMcpAdapter) CallTool(name string, args json.RawMessage) (json.RawMessage, error) {
	fn := slackToolHandlers()[name]
	if fn == nil {
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
	a := jsonObj{}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
	}
	// stripMeta matches tools/call in util.go: drop _meta and nil values before
	// handing the args to the handler.
	res, err := fn(stripMeta(a))
	if err != nil {
		return nil, err
	}
	return json.Marshal(res)
}

// servePluginMcp runs the host binary AS a go-plugin MCP plugin, serving the
// built-in McpServer selected by name over the shared handshake (see
// plugin.Serve / plugin.PluginMap). This is the "self-exec as a plugin" path a
// supervisor would launch; the in-process bridge (runMcpBridge) does NOT use it.
// It resolves the BUILT-IN adapter directly (builtinMcpServerFor), never
// re-consulting config: config selection is the supervisor's job, and routing
// through mcpServerFor here would recurse into another launch.
func servePluginMcp(name string) {
	srv, err := builtinMcpServerFor(name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pi-stack-host plugin mcp: "+err.Error())
		os.Exit(2)
	}
	plugin.Serve(map[string]goplugin.Plugin{"mcp": &plugin.McpPlugin{Impl: srv}})
}
