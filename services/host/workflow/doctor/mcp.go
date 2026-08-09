package doctor

import (
	"pix/host/config"
	"pix/host/health"
	"pix/host/hostenv"
	"pix/host/mcp"
	"pix/host/packinfo"
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
			return health.MCPServer{Name: name, Remote: true, RegisterFix: "pix mcp add " + name}
		case c.Manifest != "" || c.Image != "":
			// The gateway runs the container on the host; there is no hosted
			// control-plane OAuth to ask about.
			return health.MCPServer{Name: name, RegisterFix: "pix mcp add " + name}
		}
	}
	// gog is the documented local special case (mcp.LocalMCPNames): the bridge
	// NEVER lists it — it is the external Google Workspace CLI, not a `pix-host
	// mcp <name>` subcommand — but `pix mcp add` registers it with the
	// gateway exactly like a local stdio server, hardened argv and all
	// (mcp.GogHardenedArgv). Classifying it off the bridge list alone would
	// print `sbx mcp add --help` for a server pix registers itself, and this is
	// the only place that fact is stated to a user.
	if name == config.GWServerName {
		return health.MCPServer{Name: name, RegisterFix: "pix mcp add " + name}
	}
	if localKnown && localSet[name] {
		return health.MCPServer{Name: name, RegisterFix: "pix mcp add " + name}
	}
	// A name pix knows the endpoint for: `pix mcp add <name>` looks the URL up,
	// so the user does not need it. (This used to say `pix mcp bundle`, which
	// registered three vendors at once; that verb is gone.)
	if mcp.McpCatalogNames[name] {
		return health.MCPServer{Name: name, Remote: true, RegisterFix: "pix mcp add " + name}
	}
	if !localKnown {
		// Unknown kind: no repair is safe. The probe renders this unknown.
		return health.MCPServer{Name: name}
	}
	// Confirmed non-local, not pack-declared, and pix does not know its
	// endpoint: only the user has the URL, so name the shape they must supply.
	return health.MCPServer{Name: name, Remote: true, RegisterFix: "pix mcp add " + name + " --url <url>"}
}
