// mcp_credentials.go — the composition root's answer to mcp.Credentials.
//
// mcp needs four facts about the host's 1Password setup and secret is the package
// that knows them. mcp may not ask secret directly (both are L1), so the wiring
// lives here: cmd/pix asks and passes the answers down, which is what makes mcp
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
// legacy op-refs.env, resolve credentials, register. All three stay in one
// function so no caller can get two of the three.
func registerServers(cfg *config.Config, env hostenv.Env, out io.Writer,
	requested []string, servers map[string]config.MCPServer) error {
	if err := repairLegacyOpRefs(env); err != nil {
		return fmt.Errorf("repairing op-refs.env: %w", err)
	}
	return mcp.RegisterServers(cfg, env, out, requested, servers, mcpCredentials(env))
}

func mcpCredentials(env hostenv.Env) mcp.Credentials {
	opPath, err := env.LookPath("op")
	if err != nil {
		opPath = ""
	}
	return mcp.Credentials{
		OpPath:     opPath,
		OpRefsPath: secret.FindOpRefs(env),
		SeedPath:   secret.DefaultOpRefsPath(env),
	}
}

// repairLegacyOpRefs undoes a 0.1.14 bug: malformed prose in op-refs.env makes `op
// run` fail during dotenv parsing. Here, so mcp needs neither the write nor the
// knowledge of which release was broken.
func repairLegacyOpRefs(env hostenv.Env) error {
	return secret.RepairLegacyOpRefsFile(env, secret.DefaultOpRefsPath(env))
}
