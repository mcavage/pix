package doctor

import (
	"strings"

	"pix/host/config"
	"pix/host/health"
	"pix/host/hostenv"
	"pix/host/mcp"
	"pix/host/sys"
	"pix/host/workflow/pack"
	"pix/host/workspace"
)

// mcp.go is the MCP diagnosis, and it is now two halves in two places: the
// CLASSIFICATION (what kind of server is this, and therefore which repair is
// honest) lives here, where the pack and the local inventory are visible; the
// CHECKING lives in health.MCPProbe, which asks sbx and the launcher receipt.
//
// The 600-line readiness group this replaces was doctor's and status's alone
// and went with the model it was written against; the three truths it
// established (registration from `sbx mcp ls`, attachment from the launcher's
// own receipt, auth from the control plane, never one inferred from another)
// did not, because they are the whole value of the check.
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
	containers := pack.ActiveContainerMCP(cfg)
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

// MCPAttachment reads the LAUNCHER's per-sandbox receipt for ws and reports
// what it proves. known is false whenever the receipt cannot be trusted to
// answer for a name it does not list: no state dir, an ambiguous
// workspace->sandbox mapping, an unverifiable receipt, or a PARTIAL one
// (synthesized for a sandbox whose creation pix never observed, so its
// silence about a server means nothing).
func MCPAttachment(env hostenv.Env, ws string) (attached []string, known bool, sandbox string) {
	if strings.TrimSpace(ws) == "" {
		return nil, false, ""
	}
	sd, err := env.StateDir()
	if err != nil || strings.TrimSpace(sd) == "" {
		return nil, false, ""
	}
	res := workspace.ResolveSandbox(sd, ws)
	if res.Sandbox == "" {
		return nil, false, ""
	}
	r, status, err := workspace.ReadMCPReceipt(sd, res.Sandbox)
	if err != nil || status.Unverifiable() || r == nil || r.IsPartial() {
		return nil, false, res.Sandbox
	}
	seen := map[string]bool{}
	for _, n := range r.Preloaded {
		if !seen[n] {
			seen[n], attached = true, append(attached, n)
		}
	}
	for _, l := range r.Loads {
		if !seen[l.Name] {
			seen[l.Name], attached = true, append(attached, l.Name)
		}
	}
	return attached, true, res.Sandbox
}
