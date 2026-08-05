package mcp

import (
	"pix/host/cli"
	"pix/host/workspace"
	"slices"
)

// evidence.go — the two PURE questions the MCP surfaces ask about a server:
// what host registration a bounded `sbx mcp ls` can prove, and what the
// per-sandbox launcher receipt claims about attachment. Nothing here probes,
// execs, or does I/O: the caller supplies what it already fetched. The
// renderers live in health/mcp.go, which owns the live, EPHEMERAL per-sandbox
// snapshot both `pix status` and `pix doctor` render from — this package no
// longer carries a second join of the same facts for them to drift against.

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

// The two positive attachment claims a receipt can make. Registration says
// nothing about a sandbox ever being unloaded, so a positive claim is evidence
// pix itself observed succeeding and is never overridden by a later listing.
const (
	// McpJoinPreloaded: the receipt records pix preloading this server at the
	// sandbox's create (workspace.WriteCreateReceipt).
	McpJoinPreloaded = "preloaded"
	// McpJoinLoaded: the receipt records a successful live `pix mcp load`
	// (workspace.AppendLoadReceipt).
	McpJoinLoaded = "loaded"
)

// ReceiptClaim reports what a VALID receipt says about name: "preloaded" (the
// create-time static set), "loaded" (a live `pix mcp load`), or "" (no
// entry). It reads the receipt ONLY when rstatus vouches for it — an absent or
// unverifiable receipt claims nothing.
func ReceiptClaim(receipt *workspace.MCPReceipt, rstatus workspace.MCPStateStatus, name string) string {
	if rstatus != workspace.MCPStateOK || receipt == nil {
		return ""
	}
	if slices.Contains(receipt.Preloaded, name) {
		return McpJoinPreloaded
	}
	for _, l := range receipt.Loads {
		if l.Name == name {
			return McpJoinLoaded
		}
	}
	return ""
}
