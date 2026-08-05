package provision

import (
	"fmt"
	"pix/host/hostenv"
	"pix/host/mcp"
	"strings"
)

// catalog_gate.go — the readiness gate for shipped-catalog remote MCP servers
// (mcp.McpCatalogNames: notion/atlassian/granola), shared by `pix setup` and the
// onboarding reconcile. Persisting a catalog remote into config.toml, or launching
// a sandbox that preloads it, while it is unregistered or unauthorized claims a
// setup that cannot work: the gateway would preload a server it cannot spawn, or
// that 401s on first use. So each name is verified BEFORE any save or handoff,
// with the bounded native probes doctor uses, and any gap FAILS with the exact
// repair command. The gate never opens an OAuth flow itself — auth is always the
// user's explicit command — so a non-interactive setup can never wedge on, or
// silently trigger, a browser grant.

// VerifyCatalogMCPReady is the gate itself: every shipped-catalog name in names
// (non-catalog names are never probed here) must classify mcp.CatalogMCPReady, or
// the whole operation fails with the exact repair command. Pack adoption may
// already have committed launcher-owned state, so errors make no global "nothing
// was saved" claim. One bounded `sbx mcp ls` is shared across all names.
func VerifyCatalogMCPReady(env hostenv.Env, names []string) error {
	var catalog []string
	seen := map[string]bool{}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if mcp.McpCatalogNames[n] && !seen[n] {
			seen[n] = true
			catalog = append(catalog, n)
		}
	}
	if len(catalog) == 0 {
		return nil
	}
	mcpOut, mcpOK := "", false
	if _, err := env.LookPath("sbx"); err == nil {
		// BOUNDED: a hung listing degrades to unverifiable, never a hang.
		if o, timedOut, err := env.RunTimed("sbx", "mcp", "ls"); err == nil && !timedOut {
			mcpOut, mcpOK = o, true
		}
	}
	for _, n := range catalog {
		switch mcp.CatalogMCPState(env, mcpOut, mcpOK, n) {
		case mcp.CatalogMCPReady:
		case mcp.CatalogMCPUnregistered:
			return fmt.Errorf("catalog MCP server %q is not registered with the sbx gateway; register the shipped catalog (`pix mcp bundle`), authorize it (`pix mcp auth %s`), then re-run", n, n)
		case mcp.CatalogMCPUnauthorized:
			return fmt.Errorf("catalog MCP server %q is registered but not authorized; run `pix mcp auth %s`, then re-run", n, n)
		case mcp.CatalogMCPDenied:
			return fmt.Errorf("catalog MCP server %q is denied by policy (sbx mcp auth status %s) — an organizational denial no setup command can fix; drop it or contact your admin", n, n)
		default: // catalogMCPUnverifiable
			return fmt.Errorf("could not verify catalog MCP server %q is registered and authorized (sbx probe unavailable, failed, or timed out); fix sbx and retry, or prepare it first: `pix mcp bundle`, then `pix mcp auth %s`", n, n)
		}
	}
	return nil
}
