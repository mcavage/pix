// The `pix-host mcp <name>` bridge: a compiled stdio MCP server that the
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
// main.go registers this subcommand: `case "mcp": runMcpSubcommand`.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

	goplugin "github.com/hashicorp/go-plugin"

	"pix/host/plugin"
)

// builtinMcpServerFor resolves a bridge name to its IN-PROCESS built-in McpServer
// adapter (no subprocess). This is the self-exec plugin server's view too
// (servePluginMcp), which must serve the built-in impl WITHOUT re-consulting
// config (config selection is decided by the supervisor, not the servant).
//
// The public tree now ships ZERO built-in local servers. Slack was the last
// one and was externalized (W2/U02a — see docs/design/slack-setup.md); the
// write-scoped `google-docs-create` companion retired with the built-in Google
// Workspace wizard (W2/U02B — see docs/design/gworkspace-externalization.md).
// A local stdio server now either registers as a container the sbx gateway
// runs, or arrives as a pack-trust-admitted [[services]] unit. The lookup is
// kept (rather than deleted) because it is the single "is <name> served by this
// binary" answer both the in-process bridge (runMcpBridge) and the self-exec
// plugin path (servePluginMcp) ask, and it must keep failing CLOSED.
func builtinMcpServerFor(name string) (plugin.McpServer, error) {
	return nil, fmt.Errorf("no built-in MCP server named %q", name)
}

// builtinMcpNames returns the sorted names this binary can serve locally as a
// `pix-host mcp <name>` stdio bridge: today NONE. gog is DELIBERATELY excluded
// — it is the external Google Workspace CLI, registered directly (see
// mcp.GogHardenedArgv), not served by this bridge. This is the source of truth
// for "is <name> a local stdio server" that the launcher (`pix mcp register`)
// and doctor consult via `mcp --list`.
func builtinMcpNames() []string {
	var names []string
	sort.Strings(names)
	return names
}

// runMcpList prints, one per line, the MCP server names this binary can serve
// locally (builtinMcpNames). Exit 0. It is the introspection mode
// `pix-host mcp --list` / `mcp list`.
func runMcpList() {
	for _, n := range builtinMcpNames() {
		fmt.Println(n)
	}
}

// runMcpListTools resolves the named built-in server and prints its tool names,
// one per line, then EXITS — it never enters the stdio loop. This is the
// bounded, fast, network-free introspection mode `pix-host mcp <name>
// --list-tools`, used by doctor to probe a registration without a hanging
// gateway handshake. An unknown name is a non-zero exit with a stderr message.
func runMcpListTools(name string) {
	srv, err := builtinMcpServerFor(name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pix-host mcp: "+err.Error())
		os.Exit(2)
	}
	tools, err := srv.ListTools()
	if err != nil {
		fmt.Fprintln(os.Stderr, "pix-host mcp: "+err.Error())
		os.Exit(1)
	}
	for _, t := range tools {
		fmt.Println(t.Name)
	}
}

// mcpServerFor resolves a bridge name to its McpServer implementation — always
// the in-process adapter (builtinMcpServerFor, which serves the surviving
// built-ins). The old [plugins.mcp] config OVERRIDE is RETIRED (U07d): a config
// file can no longer name an executable this process launches — the declaration
// is swept inert with a RetiredKeys notice at load (config.applyDefaults), so no
// code path from here can ever reach an exec on config input. An external MCP
// unit now enters only as a pack-trust-admitted [[services]] entry, consumed
// through reconcilePackUnits (pack_units.go) after the Tier-1
// fingerprint/consent check.
//
// It returns a cleanup func the caller MUST run on every exit path (a no-op
// today; kept so the pack-unit integration can hand back a supervisor
// shutdown without changing the call sites).
func mcpServerFor(name string) (plugin.McpServer, func(), error) {
	noop := func() {}
	srv, err := builtinMcpServerFor(name)
	return srv, noop, err
}

// pluginMcpServer adapts a supervisor-dispensed external MCP plugin to the
// plugin.McpServer interface, so runMcpBridge proxies to it transparently. It
// resolves the current client per call (a watchdog restart is invisible to the
// caller), mirroring memoryProxyMux. With the [plugins.mcp] override retired
// (U07d), its consumer is a pack-admitted [[services]] unit of kind "mcp": the
// holder reconcilePackUnits returns is wrapped in this adapter.
type pluginMcpServer struct{ h *pluginHolder }

func (p *pluginMcpServer) srv() (plugin.McpServer, error) {
	s, _ := p.h.Get().(plugin.McpServer)
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

// runMcpSubcommand dispatches the `pix-host mcp ...` argv across the three
// modes: `--list`/`list` (print servable names), `<name> --list-tools`
// (introspect a server's tools and exit), and the default `<name>` stdio bridge.
// The introspection modes are bounded + network-free so callers (the launcher,
// doctor) can ask "what can this binary serve, and what tools does <name> have"
// without spawning a hanging gateway handshake.
func runMcpSubcommand(argv []string) {
	if len(argv) == 0 {
		fmt.Fprintln(os.Stderr, "pix-host mcp: missing <name> (or --list)")
		os.Exit(2)
	}
	if argv[0] == "--list" || argv[0] == "list" {
		runMcpList()
		return
	}
	name := argv[0]
	for _, a := range argv[1:] {
		if a == "--list-tools" {
			runMcpListTools(name)
			return
		}
	}
	runMcpBridge(name)
}

// runMcpBridge is the body for the `pix-host mcp <name>` subcommand. It
// resolves the McpServer, builds a dispatcher that proxies to it, and serves it
// over the newline-delimited-JSON stdio transport the gateway speaks.
//
// An override plugin is a real subprocess owned by mcpServerFor's supervisor, so
// cleanup() MUST run on every exit path — the normal stdio-close return AND the
// os.Exit error path — or the external mcp child is orphaned. defer covers the
// normal return; the os.Exit branch calls cleanup() explicitly first (os.Exit
// skips defers).
func runMcpBridge(name string) {
	srv, cleanup, err := mcpServerFor(name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pix-host mcp: "+err.Error())
		cleanup()
		os.Exit(2)
	}
	defer cleanup()
	serverName, tools, handlers, err := bridgeFromMcpServer(srv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pix-host mcp: "+err.Error())
		cleanup()
		os.Exit(1)
	}
	handle := mcpDispatcher(serverName, tools, handlers)
	mcpStdio(handle)
}

// servePluginMcp runs the host binary AS a go-plugin MCP plugin, serving the
// built-in McpServer selected by name over the shared handshake (see
// plugin.Serve / plugin.PluginMap). This is the "self-exec as a plugin" path a
// supervisor would launch; the in-process bridge (runMcpBridge) does NOT use
// it. It resolves the BUILT-IN adapter directly (builtinMcpServerFor), never
// re-consulting config: config selection is the supervisor's job, and routing
// through mcpServerFor here would recurse into another launch.
func servePluginMcp(name string) {
	srv, err := builtinMcpServerFor(name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pix-host plugin mcp: "+err.Error())
		os.Exit(2)
	}
	plugin.Serve(map[string]goplugin.Plugin{"mcp": &plugin.McpPlugin{Impl: srv}})
}
