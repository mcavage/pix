// The `pi-stack-host mcp <name>` bridge: a compiled stdio MCP server that the
// sbx gateway spawns and that forwards every tools/list + tools/call to a
// plugin.McpServer implementation.
//
// The built-in case runs the McpServer IN-PROCESS (mcpServerFor returns the
// adapter directly — no per-request subprocess), honoring the arch rule that a
// plugin subprocess is only ever spawned once at startup, never per request. A
// config-selected OVERRIDE plugin would be dispensed once here at startup too
// (see the TODO in mcpServerFor); the bridge body below is identical either way
// because it only speaks the plugin.McpServer interface.
//
// main.go does NOT register this subcommand yet — a later unit wires it.

package main

import (
	"encoding/json"
	"fmt"
	"os"

	"pi-stack/host/plugin"
)

// mcpServerFor resolves a bridge name to its McpServer implementation. Built-in
// names return their in-process adapter (no subprocess). This is the single seam
// where an override would be chosen.
//
// TODO(plugin-override): consult config (config.Config.Plugin) for the "mcp"
// slot / this name; when Impl != config.BuiltinImpl, launch the external plugin
// binary via go-plugin ONCE here and dispense its plugin.McpServer client
// instead of the built-in adapter. Until that unit lands, only built-ins serve.
func mcpServerFor(name string) (plugin.McpServer, error) {
	switch name {
	case "slack":
		return slackMcpAdapter{}, nil
	default:
		return nil, fmt.Errorf("no built-in MCP server named %q", name)
	}
}

// bridgeFromMcpServer turns an McpServer into the (serverName, tools, handlers)
// triple that mcpDispatcher (util.go) needs. Each handler proxies through the
// interface: marshal the dispatcher's args, CallTool, unmarshal the result back
// to a value mcpDispatcher can wrap in an MCP text-content reply.
func bridgeFromMcpServer(srv plugin.McpServer) (string, []mcpTool, map[string]func(jsonObj) (any, error), error) {
	info, err := srv.Info()
	if err != nil {
		return "", nil, nil, err
	}
	specs, err := srv.ListTools()
	if err != nil {
		return "", nil, nil, err
	}
	tools := make([]mcpTool, 0, len(specs))
	handlers := map[string]func(jsonObj) (any, error){}
	for _, sp := range specs {
		tools = append(tools, toolSpecToMcpTool(sp))
		name := sp.Name // capture per iteration for the closure
		handlers[name] = func(args jsonObj) (any, error) {
			raw, err := json.Marshal(args)
			if err != nil {
				return nil, err
			}
			out, err := srv.CallTool(name, raw)
			if err != nil {
				return nil, err
			}
			if len(out) == 0 {
				return nil, nil
			}
			var v any
			if err := json.Unmarshal(out, &v); err != nil {
				return nil, err
			}
			return v, nil
		}
	}
	return info.Name, tools, handlers, nil
}

// toolSpecToMcpTool rebuilds a util.go mcpTool from a plugin.ToolSpec so
// mcpTool.schema() regenerates the same inputSchema the adapter emitted.
func toolSpecToMcpTool(sp plugin.ToolSpec) mcpTool {
	t := mcpTool{Name: sp.Name, Description: sp.Description}
	if len(sp.InputSchema) > 0 {
		var s struct {
			Properties jsonObj  `json:"properties"`
			Required   []string `json:"required"`
		}
		if json.Unmarshal(sp.InputSchema, &s) == nil {
			t.Properties = s.Properties
			t.Required = s.Required
		}
	}
	return t
}

// runMcpBridge is the body for a future `pi-stack-host mcp <name>` subcommand. It
// resolves the McpServer, builds a dispatcher that proxies to it, and serves it
// over the newline-delimited-JSON stdio transport the gateway speaks.
func runMcpBridge(name string) {
	srv, err := mcpServerFor(name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pi-stack-host mcp: "+err.Error())
		os.Exit(2)
	}
	serverName, tools, handlers, err := bridgeFromMcpServer(srv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pi-stack-host mcp: "+err.Error())
		os.Exit(1)
	}
	handle := mcpDispatcher(serverName, tools, handlers)
	mcpStdio(handle)
}
