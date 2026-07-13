// The `pi-stack-host mcp <name>` bridge: a compiled stdio MCP server that the
// sbx gateway spawns and that forwards every tools/list + tools/call to a
// plugin.McpServer implementation.
//
// The built-in case runs the McpServer IN-PROCESS (builtinMcpServerFor returns
// the adapter directly — no per-request subprocess), honoring the arch rule that
// a plugin subprocess is only ever spawned once at startup, never per request. A
// config-selected OVERRIDE plugin (config [plugins.mcp] with impl != builtin) is
// launched ONCE here at bridge startup via the same supervisor helper the broker/
// memory paths use (SHA-verified, env-isolated) and dispensed as a plugin.McpServer
// client; the bridge body below is identical either way because it only speaks
// the plugin.McpServer interface.
//
// main.go does NOT register this subcommand yet — a later unit wires it.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"pi-stack/host/config"
	"pi-stack/host/plugin"
)

// builtinMcpServerFor resolves a bridge name to its IN-PROCESS built-in McpServer
// adapter (no subprocess). This is the self-exec plugin server's view too
// (servePluginMcp), which must serve the built-in impl WITHOUT re-consulting
// config (config selection is decided by the supervisor, not the servant).
func builtinMcpServerFor(name string) (plugin.McpServer, error) {
	switch name {
	case "slack":
		return slackMcpAdapter{}, nil
	default:
		return nil, fmt.Errorf("no built-in MCP server named %q", name)
	}
}

// mcpServerFor resolves a bridge name to its McpServer implementation, honoring a
// config-selected OVERRIDE. When [plugins.mcp] is builtin (or the name is the
// built-in "slack"), the in-process adapter is returned. Otherwise the external
// plugin binary is launched ONCE here via the shared supervisor helper (which
// SHA-verifies the binary before exec and isolates its env so the broker bearer
// never leaks), dispensed as a plugin.McpServer, and wrapped so the bridge body
// proxies to it. This is a startup-only spawn — never per request.
func mcpServerFor(name string) (plugin.McpServer, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	spec := cfg.Plugin("mcp")
	// slack is always the in-process built-in; a non-builtin [plugins.mcp] impl
	// overrides every other name.
	if name == "slack" || spec.Impl == config.BuiltinImpl {
		return builtinMcpServerFor(name)
	}
	selfPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate self: %w", err)
	}
	sup := &supervisor{}
	h, err := sup.launch(name, "mcp", spec, selfPath, nil)
	if err != nil {
		return nil, fmt.Errorf("launch external mcp plugin %q: %w", name, err)
	}
	return &pluginMcpServer{h: h}, nil
}

// pluginMcpServer adapts a supervisor-dispensed external MCP plugin to the
// plugin.McpServer interface, so runMcpBridge proxies to it transparently. It
// resolves the current client per call (a watchdog restart is invisible to the
// caller), mirroring memoryProxyMux / gwsBrokerProxyMux.
type pluginMcpServer struct{ h *pluginHolder }

func (p *pluginMcpServer) srv() (plugin.McpServer, error) {
	s, _ := p.h.get().(plugin.McpServer)
	if s == nil {
		return nil, errors.New("mcp plugin unavailable")
	}
	return s, nil
}

func (p *pluginMcpServer) Info() (plugin.ServerInfo, error) {
	s, err := p.srv()
	if err != nil {
		return plugin.ServerInfo{}, err
	}
	return s.Info()
}

func (p *pluginMcpServer) ListTools() ([]plugin.ToolSpec, error) {
	s, err := p.srv()
	if err != nil {
		return nil, err
	}
	return s.ListTools()
}

func (p *pluginMcpServer) CallTool(name string, args json.RawMessage) (json.RawMessage, error) {
	s, err := p.srv()
	if err != nil {
		return nil, err
	}
	return s.CallTool(name, args)
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
