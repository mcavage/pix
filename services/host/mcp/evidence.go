package mcp

import "pix/host/cli"

// evidence.go — the ONE pure question the MCP surfaces ask about a server:
// what host registration a bounded `sbx mcp ls` can prove. Nothing here
// probes, execs, or does I/O: the caller supplies what it already fetched.
//
// U04e deleted the second question this file used to answer. A per-sandbox
// "attachment" claim was read out of a launcher-written receipt — a record
// that pix ITSELF once preloaded or loaded a server — and rendered by
// status/doctor as "attached". That is a verdict derived from a past action,
// not a probe: the sandbox it described can be torn down and recreated by any
// other shell (U04d), and nothing pix can run reports what a live gateway
// session currently has attached. Registration is the only MCP fact this host
// can actually establish, so it is the only one anything here answers.

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

// String renders the tri-state for JSON/report rows.
func (r McpRegEvidence) String() string {
	switch r {
	case McpRegYes:
		return "yes"
	case McpRegNo:
		return "no"
	default:
		return "unknown"
	}
}

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
