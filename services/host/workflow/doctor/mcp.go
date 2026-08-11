package doctor

import (
	"pix/host/config"
	"pix/host/health"
	"pix/host/mcp"
	"pix/host/packinfo"
)

// mcp.go is the MCP diagnosis, and it is two halves in two places: the
// CLASSIFICATION (what kind of server is this, therefore which repair is
// honest and what would prove it healthy) lives here, where the pack is
// visible; the CHECKING lives in health.MCPProbe, which asks sbx and the
// server itself.
//
// Attachment is deliberately not a third truth: only registration (`sbx mcp
// ls`), auth (the control plane), and the server's own probe are checkable,
// and nothing here can see what a live session has attached.

// MCPServers classifies every configured MCP server into the shape
// health.MCPProbe checks.
//
// Every server comes from the active pack's manifest, so classification is a
// map lookup rather than a runtime probe, and a name the pack does not declare
// is reported as exactly that — undeclared — instead of being guessed at. That
// case is the one this file exists to catch: a registration left behind by a
// pack that is no longer active runs a command that may no longer exist, and
// the gateway will report it "ready" forever regardless.
func MCPServers(cfg *config.Config, creds mcp.Credentials) []health.MCPServer {
	if cfg == nil || len(cfg.MCP) == 0 {
		return nil
	}
	declared, err := packinfo.ActiveServerMCP(cfg)
	out := make([]health.MCPServer, 0, len(cfg.MCP))
	for _, name := range cfg.MCP {
		if err != nil {
			// An active pack exists but will not load, so we cannot say what is
			// declared. FAIL CLOSED: report each server unclassified rather than
			// accusing it of being undeclared, whose repair is `sbx mcp rm`. One
			// manifest typo must not produce advice to delete a working host.
			out = append(out, health.MCPServer{Name: name, Unreadable: err.Error()})
			continue
		}
		out = append(out, classifyMCPServer(name, declared, creds))
	}
	return out
}

func classifyMCPServer(name string, declared map[string]config.MCPServer, creds mcp.Credentials) health.MCPServer {
	s, ok := declared[name]
	if !ok {
		// A curated catalog name is registerable unaided: pix knows its
		// endpoint, so there is an honest repair even with no pack.
		if mcp.McpCatalogNames[name] {
			return health.MCPServer{Name: name, Remote: true, RegisterFix: "pix mcp add " + name}
		}
		// Otherwise: no active pack provides this name, and pix ships none of
		// its own. There is no honest repair command — we do not know what it
		// was meant to be — only what the user should do about it, which the
		// probe words.
		return health.MCPServer{Name: name, Undeclared: true}
	}
	out := health.MCPServer{
		Name:        name,
		RegisterFix: "pix mcp add " + name,
	}
	// The probe runs through the SAME op-run wrapper the gateway will use to
	// spawn this server — but ONLY when the probe actually invokes that server's
	// own binary, and so needs that server's environment. A probe run in
	// doctor's own shell would inherit whatever the operator happens to have
	// exported, which is precisely how a broken credential setup passes every
	// check and then fails on first use.
	//
	// Wrapping every probe was wrong in the other direction: BambooHR's probe is
	// `docker image inspect <image>`, which needs no credential and answers in
	// milliseconds. Wrapped, it inherited the server's 1Password dependency, so a
	// locked vault turned a determinable answer into "not checkable" — and made
	// `pix doctor`, a read-only diagnostic, provoke an authorization prompt.
	if len(s.Probe) > 0 && len(s.EnvKeys) > 0 && s.Command != "" && s.Probe[0] == s.Command {
		out.Probe = mcp.OpRunWrap(creds.OpPath, creds.OpRefsPath, s.Probe)
	} else {
		out.Probe = s.Probe
	}
	switch {
	case s.RemoteURL != "":
		out.Remote = true
	case s.Command != "":
		// The only kind whose registration can rot into a command that no
		// longer exists, so the only kind that carries a binary to verify.
		out.Command = s.Command
	}
	return out
}
