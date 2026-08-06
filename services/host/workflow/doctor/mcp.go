package doctor

import (
	"pix/host/config"
	"pix/host/health"
	"pix/host/hostenv"
	"pix/host/mcp"
	"pix/host/packinfo"
	"pix/host/sys"
)

// mcp.go is the MCP diagnosis, and it is two halves in two places: the
// CLASSIFICATION (what kind of server is this, and therefore which repair is
// honest) lives here, where the pack and the local inventory are visible; the
// CHECKING lives in health.MCPProbe, which asks sbx.
//
// Attachment is deliberately not a third truth: only registration (`sbx mcp
// ls`) and auth (the control plane) are probes, and nothing here can check
// what a live session has attached.
//
// McpLoadCommand stays a plain exported helper because
// workflow/launch prints the same one when a sandbox comes up without a server
// attached, and two spellings of one copy-paste command is how a user learns
// to distrust both.

// McpLoadCommand returns the exact `pix mcp load NAME [WORKSPACE]`
// command for name, workspace-qualified the same way runReplaceCommand is
// (bare for ".", quoted otherwise) so the two recovery commands read
// consistently. Both name and workspace are shell-quoted via the shared
// sys.ShellQuote (closure finding #3) — a server name is ordinarily a plain
// token, but quoting it too costs nothing and keeps every generated
// copy-paste command uniformly safe.
func McpLoadCommand(name, ws string) string {
	if ws == "" || ws == "." {
		return "pix mcp load " + sys.ShellQuote(name)
	}
	return "pix mcp load " + sys.ShellQuote(name) + " " + sys.ShellQuote(ws)
}

// MCPServers classifies every configured MCP server into the shape
// health.MCPProbe checks. Classification decides which repair command is
// HONEST — `pix mcp register` for a local stdio server, `pix mcp bundle` for
// a shipped catalog name, `sbx mcp add` for something pix cannot register —
// so it is done here, where the pack and the local inventory are visible, and
// not inside the probe.
//
// It FAILS CLOSED. When the local-name set could not be established, a name
// that is neither pack-declared nor a catalog name gets NO register command,
// and the probe reports it unknown rather than recommending a repair that
// may be wrong for its kind.
func MCPServers(cfg *config.Config, env hostenv.Env, hostResolver func() (string, error)) []health.MCPServer {
	if cfg == nil || len(cfg.MCP) == 0 {
		return nil
	}
	containers := packinfo.ActiveContainerMCP(cfg)
	localSet, localKnown := mcp.LocalMCPNames(env, hostResolver)
	out := make([]health.MCPServer, 0, len(cfg.MCP))
	for _, name := range cfg.MCP {
		out = append(out, classifyMCPServer(name, containers, localSet, localKnown))
	}
	return out
}

func classifyMCPServer(name string, containers map[string]config.MCPContainer, localSet map[string]bool, localKnown bool) health.MCPServer {
	// A pack states its own kind explicitly, so it needs no inventory probe.
	if c, ok := containers[name]; ok {
		switch {
		case c.RemoteURL != "":
			return health.MCPServer{Name: name, Remote: true, RegisterFix: "pix mcp register " + name}
		case c.Manifest != "" || c.Image != "":
			// The gateway runs the container on the host; there is no hosted
			// control-plane OAuth to ask about.
			return health.MCPServer{Name: name, RegisterFix: "pix mcp register " + name}
		}
	}
	// gog is the documented local special case (mcp.LocalMCPNames): the bridge
	// NEVER lists it — it is the external Google Workspace CLI, not a `pix-host
	// mcp <name>` subcommand — but `pix mcp register` registers it with the
	// gateway exactly like a local stdio server, hardened argv and all
	// (mcp.GogHardenedArgv). Classifying it off the bridge list alone would
	// print `sbx mcp add --help` for a server pix registers itself, and this is
	// the only place that fact is stated to a user.
	if name == config.GWServerName {
		return health.MCPServer{Name: name, RegisterFix: "pix mcp register " + name}
	}
	if localKnown && localSet[name] {
		return health.MCPServer{Name: name, RegisterFix: "pix mcp register " + name}
	}
	if mcp.McpCatalogNames[name] {
		return health.MCPServer{Name: name, Remote: true, RegisterFix: "pix mcp bundle"}
	}
	if !localKnown {
		// Unknown kind: no repair is safe. The probe renders this unknown.
		return health.MCPServer{Name: name}
	}
	// Confirmed non-local, not pack-declared, not in the shipped catalog: pix
	// has no command that can register it, so the honest pointer is native sbx
	// with the server's own URL — which only the user has.
	return health.MCPServer{Name: name, Remote: true, RegisterFix: "sbx mcp add --help"}
}
