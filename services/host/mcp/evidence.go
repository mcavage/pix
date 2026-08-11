package mcp

import (
	"strings"

	"pix/host/cli"
)

// evidence.go — the ONE pure question the MCP surfaces ask about a server:
// what host registration a bounded `sbx mcp ls` can prove. Nothing here
// probes, execs, or does I/O: the caller supplies what it already fetched.
//
// Attachment is deliberately NOT answered here. A launcher-written receipt is
// a verdict derived from a past action, not a probe: the sandbox it described
// can be recreated by any other shell, and nothing pix can run reports what a
// live gateway session has attached. Registration is the only MCP fact this
// host can establish, so it is the only one anything here answers.

// McpRegEvidence is the tri-state host registration evidence for one server
// name: what the bounded `sbx mcp ls` listing the caller already ran could
// actually prove. Unknown (listing failed / sbx absent) is a first-class
// state — it is never collapsed into "not registered".
type McpRegEvidence int

const (
	// McpRegUnknown: the registration listing could not be obtained, so
	// registration is unknowable here — never guessed either way.
	McpRegUnknown McpRegEvidence = iota
	// McpRegYes: the name is positively present in a successful `sbx mcp ls`.
	McpRegYes
	// McpRegNo: a successful `sbx mcp ls` positively lacks the name.
	McpRegNo
)

// McpRegEvidenceFrom derives the registration tri-state from a bounded `sbx
// mcp ls` listing's own success/failure and content — the ONE definition every
// surface builds its reg evidence from, so they can never diverge on what
// "registered" means for the same (mcpOut, mcpOK, name). Registration truth
// comes from the listing ONLY — never from config, a receipt, or a definition
// inspection.
func McpRegEvidenceFrom(mcpOut string, mcpOK bool, name string) McpRegEvidence {
	if !mcpOK {
		return McpRegUnknown
	}
	if cli.GrepWord(mcpOut, name) {
		return McpRegYes
	}
	return McpRegNo
}

// RegisteredNamesFrom lists the server names a successful `sbx mcp ls` reported.
//
// Deliberately CONSERVATIVE and fail-quiet. sbx has no machine-readable listing
// (`--json` and `-o json` are both rejected), so this reads its human table, and
// a table format is not a contract. Every line must look like
// `<name> <local|remote> …` before a name is believed; anything else is skipped
// silently. The only consumer reports EXTRA registrations as information, so
// under-reading degrades to saying nothing — which is the safe direction, and
// the reason this may not be used to decide that something is absent.
func RegisteredNamesFrom(mcpOut string) []string {
	var names []string
	for _, line := range strings.Split(mcpOut, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[1] {
		case "local", "remote":
			// A header row would have to be literally "<word> local" to fool
			// this; sbx's is "LOCAL · managed by you · ✓ on", which is not.
			names = append(names, fields[0])
		}
	}
	return names
}
