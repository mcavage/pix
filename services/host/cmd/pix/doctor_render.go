package main

import (
	"pix/host/cli"
	"pix/host/readiness"
)

// Report.Render moved to the readiness package: Report lives there now, and Go
// will not let a method be declared outside its type's package. The surface's
// own words travel with the call (hints) rather than being baked into
// the renderer, which is what kept this here for as long as it was here.

// mcpHostTrustNotice is the two-fact disclosure for local command/container
// MCP servers: they run on the host, outside sandbox isolation, with your
// host-user privileges, and anything they return can end up in the
// conversation sent to your model provider. Shared verbatim by doctor's
// footer and setup's completion summary so the two surfaces never drift.
const mcpHostTrustNotice = "Note: local/container MCP servers run on the host, outside the sandbox, with your host-user privileges. Content they return can be included in the conversation sent to your model provider. Details: SECURITY.md."

func upDown(up bool) string { return cli.UpDown(up) }

// doctorHints are the surface-specific strings the readiness renderer cannot
// know for itself: the exact sbx install command, and the host-MCP trust
// notice. They are passed IN rather than baked into readiness, so doctor,
// status and setup can each say what they mean while sharing one renderer.
func doctorHints() readiness.Hints {
	return readiness.Hints{SbxInstall: sbxInstallHint, MCPHostTrust: mcpHostTrustNotice}
}
