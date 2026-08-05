package doctor

import (
	"pix/host/sys"
)

// mcp.go is what survived the MCP group: one exact command.
//
// The group itself (registration classification, per-sandbox receipt joins,
// remote auth probing — 600 lines across three axes) was doctor's and status's
// alone, and it went with the readiness model it was written against. What the
// rest of the tree still needs from it is the repair command, because
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
