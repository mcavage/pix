// mcp_credentials.go — the composition root's answer to mcp.Credentials.
//
// mcp needs four facts about the host's 1Password setup and secret is the
// package that knows them. mcp may not ask secret directly (both are L1), so
// the wiring lives here: cmd/pix asks, and passes the answers down. This file
// is the entire cost of that rule, and it is the right cost — mcp is now
// testable with a struct literal instead of a faked filesystem.
package main

import (
	"fmt"
	"io"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/mcp"
	"pix/host/secret"
)

// registerServers is the ONE way cmd/pix registers MCP servers: repair the
// legacy op-refs.env first, then resolve credentials, then register. The
// repair used to live inside mcp.RegisterServers, so every caller got it for
// free; keeping all three steps in one function is how that stays true now
// that the first two are cmd/pix's job.
func registerServers(cfg *config.Config, env hostenv.Env, out io.Writer,
	requested []string, hostResolver func() (string, error),
	containers map[string]config.MCPContainer) error {
	if err := repairLegacyOpRefs(env); err != nil {
		return fmt.Errorf("repairing op-refs.env: %w", err)
	}
	return mcp.RegisterServers(cfg, env, out, requested, hostResolver, containers, mcpCredentials(env))
}

func mcpCredentials(env hostenv.Env) mcp.Credentials {
	opPath, err := env.LookPath("op")
	if err != nil {
		opPath = ""
	}
	refs := secret.FindOpRefs(env)
	// Only READ op-refs.env when one was actually found. The && chain this
	// replaced short-circuited, so a host with no refs file never touched the
	// disk; computing GogKeyring eagerly reintroduced a read that gog setup is
	// specifically tested not to perform.
	gogKeyring := refs != "" && secret.OpRefFilled(env, "GOG_KEYRING_PASSWORD")
	return mcp.Credentials{
		OpPath:     opPath,
		OpRefsPath: refs,
		SeedPath:   secret.DefaultOpRefsPath(env),
		GogKeyring: gogKeyring,
	}
}

// repairLegacyOpRefs is the side effect RegisterServers used to perform
// itself: Pix 0.1.14 emitted malformed prose into op-refs.env, and `op run`
// fails during dotenv parsing on it. Doing it here keeps mcp free of both the
// write and the knowledge of which release was broken.
func repairLegacyOpRefs(env hostenv.Env) error {
	return secret.RepairLegacyOpRefsFile(env, secret.DefaultOpRefsPath(env))
}
