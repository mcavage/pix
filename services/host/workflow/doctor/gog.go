package doctor

import (
	"pix/host/hostenv"
	"pix/host/mcp"
	"pix/host/readiness"

	"pix/host/config"
)

// gog.go builds doctor's gog (Google Workspace) group on the S04 readiness
// axes. gog is ALWAYS optional (never core, so nothing here can block
// doctor's exit code); a missing registration is optional-NOT-CONFIGURED (a
// note, not a failure).
//
// Google Workspace has no built-in guided setup left in pix (the old `pix
// gworkspace setup|status|disable` OAuth wizard, its headless-spawn/hardened-
// flags probing, and the separate `google-docs-create` write-scoped MCP were
// all retired — see docs/design/gworkspace-externalization.md). gog is now
// registered the SAME way any other local stdio MCP server is: `pix mcp
// register` (mcp.RegisterServers / mcp.GogHardenedArgv already bakes in
// --readonly/--gmail-no-send/--wrap-untrusted), or by a pack. Doctor's job
// shrinks to the same two facts every other MCP server's group renders:
// registration with the sbx gateway (gogRegistrationCheck) and sandbox
// attachment via the shared receipt-backed join row (gogAttachCheck). gog
// keeps its own group rather than joining the generic MCP servers group
// because `pix-host mcp --list` never lists it (mcp.LocalMCPNames' documented
// special case), so the generic classifier cannot place it.
func gogGroup(cfg *config.Config, env hostenv.Env, mcpOut string, mcpOK, sbxPresent bool, ctx mcpSandboxContext) readiness.Group {
	g := readiness.Group{Title: "Google Workspace (optional, via host MCP \u2014 read-only)"}
	gogReg := mcp.McpRegEvidenceFrom(mcpOut, mcpOK, config.GWServerName)
	g.Checks = append(g.Checks, gogRegistrationCheck(mcpOut, mcpOK, sbxPresent))
	g.Checks = append(g.Checks, gogAttachCheck(cfg, ctx, gogReg))
	return g
}

// gogAttachCheck is check 5: gog's sandbox attachment. With a workspace
// sandbox context it is the SAME receipt-backed join row every other MCP
// server gets (mcpAttachCheck -> mcp.JoinMCPSandboxRow): preloaded/loaded receipt
// claims render ready; a registered server a COMPLETE valid receipt has no
// entry for is a verified registered-not-attached TODO (a sandbox created
// BEFORE gog was configured is NOT attached just because cfg now names it);
// everything else stays unverifiable. Without a sandbox context, config
// membership is stated as INTENT (preloads at the next create) — an
// informational note, never a ready attachment claim.
func gogAttachCheck(cfg *config.Config, ctx mcpSandboxContext, reg mcp.McpRegEvidence) readiness.Check {
	if ctx.mode == mcpAttachReceipt {
		return mcpAttachCheck(config.GWServerName, ctx, reg)
	}
	if mcp.Configured(cfg, config.GWServerName) {
		det := "in the configured MCP set — preloads at sandbox create (intent, not attachment)"
		if ctx.mode == mcpAttachSandboxAbsent {
			det = "sandbox " + ctx.sandbox + " not created yet — gog preloads at `pix run` create"
		} else if ctx.note != "" {
			det = ctx.note + " — attachment cannot be reported"
		}
		return readiness.Check{Label: "attached", Note: true, Verdict: readiness.VerdictUnverifiable, Detail: det}
	}
	return readiness.Check{Label: "attached", Note: true, Verdict: readiness.VerdictUnverifiable,
		Detail: "run `pix config set mcp " + config.GWServerName + "` to attach it"}
}
