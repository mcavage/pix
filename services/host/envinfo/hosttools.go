package envinfo

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	"pix/host/stack"
)

const HostToolsSubcommand = "__pix-tools-mcp"

// HostToolsID fixes the workspace and authority of a Gateway registration.
// It is an identity, not a secret or an authentication token.
func HostToolsID(home, workspace, sandbox string, dev bool) string {
	sum := sha256.Sum256([]byte(home + "\x00" + workspace + "\x00" + sandbox + "\x00" + strconv.FormatBool(dev)))
	return hex.EncodeToString(sum[:8])
}

func WithHostTools(facts BuiltinMCPFacts, home, workspace, sandbox string, dev, disabled bool) BuiltinMCPFacts {
	if disabled {
		facts.SessionName = ""
		facts.SessionCommand = ""
		facts.SessionArgs = nil
		return facts
	}
	id, err := stack.ID(home)
	if err != nil {
		return facts
	}
	base, _ := stack.MCPSessionName(id)
	facts.SessionName = base + "-" + HostToolsID(home, workspace, sandbox, dev)
	facts.SessionArgs = []string{HostToolsSubcommand, "--home", home, "--workspace", workspace, "--sandbox", sandbox}
	if dev {
		facts.SessionArgs = append(facts.SessionArgs, "--dev")
	}
	return facts
}
