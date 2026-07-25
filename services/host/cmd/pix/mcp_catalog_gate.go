package main

import (
	"fmt"
	"strings"
)

// mcp_catalog_gate.go — the setup/onboard readiness gate for shipped-catalog
// remote MCP servers (mcpCatalogNames: notion/atlassian/granola).
//
// Persisting a catalog remote into config.toml (or launching a sandbox that
// preloads it) while it is unregistered or unauthorized silently claims a
// setup that cannot work: the gateway would preload a server it cannot spawn
// or that 401s on first use. So BEFORE any config save or handoff, setup and
// onboard verify each proposed catalog name with the same bounded native
// probes doctor uses (`sbx mcp ls` registration evidence + `sbx mcp auth
// status <name>`), and FAIL with the exact repair commands (`pix mcp
// bundle`, `pix mcp auth <name>`) on any gap. The gate never opens an
// OAuth flow itself — auth is always the user's explicit command — so a
// non-interactive setup can never wedge on (or silently trigger) a browser
// grant. Local stdio servers (gog/slack/…) are untouched: they keep the
// registerServers path.

// catalogMCPReadiness classifies one catalog remote's launch readiness.
type catalogMCPReadiness int

const (
	catalogMCPReady        catalogMCPReadiness = iota // registered + authorized
	catalogMCPUnregistered                            // a successful listing positively lacks it
	catalogMCPUnauthorized                            // registered, auth positively missing/expired
	catalogMCPDenied                                  // registered, EXPLICIT policy denial
	catalogMCPUnverifiable                            // probe failed/timed out — unknown, never a guess
)

// catalogMCPState resolves one catalog name's readiness from the shared
// registration evidence (mcpRegEvidenceFrom over an already-fetched bounded
// `sbx mcp ls`) plus a bounded native `sbx mcp auth status <name>` probe —
// the SAME classification doctor's mcpRemoteAuthCheck applies, so the gate
// and doctor can never disagree about what "auth-ready" means.
func catalogMCPState(env shellEnv, mcpOut string, mcpOK bool, name string) catalogMCPReadiness {
	switch mcpRegEvidenceFrom(mcpOut, mcpOK, name) {
	case mcpRegNo:
		return catalogMCPUnregistered
	case mcpRegUnknown:
		return catalogMCPUnverifiable
	}
	// Registered: bounded auth-status probe. This is read-only — it NEVER
	// starts the OAuth flow (that is `pix mcp auth <name>`, run by the
	// user), so a non-interactive setup cannot auto-open a browser grant.
	out, timedOut, err := probeRun(env, "sbx", "mcp", "auth", "status", name)
	if timedOut {
		return catalogMCPUnverifiable
	}
	// EXPLICIT denial signals win regardless of exit code: a policy denial is
	// a positive refusal, not a credential gap.
	if classifyProbeFailure(out, err) == probeDenied {
		return catalogMCPDenied
	}
	if err != nil {
		if classifyProbeFailure(out, err) == probeAuthTodo {
			return catalogMCPUnauthorized
		}
		return catalogMCPUnverifiable
	}
	switch mcpAuthStatus(out) {
	case mcpAuthOK:
		return catalogMCPReady
	case mcpAuthFailed:
		return catalogMCPUnauthorized
	default: // mcpAuthUnknown
		return catalogMCPUnverifiable
	}
}

// verifyCatalogMCPReady is the gate itself: every shipped-catalog name in
// names (derived from mcpCatalogNames — non-catalog names are never probed
// here) must classify catalogMCPReady, or the whole operation fails with the
// exact repair command BEFORE anything was saved or launched. One bounded
// `sbx mcp ls` is shared across all names.
func verifyCatalogMCPReady(env shellEnv, names []string) error {
	var catalog []string
	seen := map[string]bool{}
	for _, n := range names {
		n = strings.TrimSpace(n)
		if mcpCatalogNames[n] && !seen[n] {
			seen[n] = true
			catalog = append(catalog, n)
		}
	}
	if len(catalog) == 0 {
		return nil
	}
	mcpOut, mcpOK := "", false
	if env.lookPath != nil {
		if _, err := env.lookPath("sbx"); err == nil {
			// BOUNDED: a hung listing degrades to unverifiable, never a hang.
			if o, timedOut, err := probeRun(env, "sbx", "mcp", "ls"); err == nil && !timedOut {
				mcpOut, mcpOK = o, true
			}
		}
	}
	for _, n := range catalog {
		switch catalogMCPState(env, mcpOut, mcpOK, n) {
		case catalogMCPReady:
		case catalogMCPUnregistered:
			return fmt.Errorf("catalog MCP server %q is not registered with the sbx gateway — nothing was saved; register the shipped catalog (`pix mcp bundle`), authorize it (`pix mcp auth %s`), then re-run", n, n)
		case catalogMCPUnauthorized:
			return fmt.Errorf("catalog MCP server %q is registered but not authorized — nothing was saved; run `pix mcp auth %s`, then re-run", n, n)
		case catalogMCPDenied:
			return fmt.Errorf("catalog MCP server %q is denied by policy (sbx mcp auth status %s) — an organizational denial no setup command can fix; drop it or contact your admin", n, n)
		default: // catalogMCPUnverifiable
			return fmt.Errorf("could not verify catalog MCP server %q is registered and authorized (sbx probe unavailable, failed, or timed out) — nothing was saved; fix sbx and retry, or prepare it first: `pix mcp bundle`, then `pix mcp auth %s`", n, n)
		}
	}
	return nil
}
