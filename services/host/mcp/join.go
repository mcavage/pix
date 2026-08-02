package mcp

import (
	"pix/host/config"
	"pix/host/workspace"
	"slices"
)

// mcpjoin.go — S09: the ONE pure truth path joining a configured MCP server
// set with host registration evidence and the per-sandbox launcher receipt
// (sandboxmcpstate.go). Both consumers render FROM these rows:
//
//   - doctor's per-server attachment check (mcpAttachCheck, doctor_mcp.go);
//   - status's per-sandbox MCP rows (status.go).
//
// so the two verbs can never drift into telling different attachment stories
// from the same evidence. Everything here is PURE — no probes, no I/O, no
// subprocesses: the caller supplies what it already fetched (the bounded
// `sbx mcp ls` result as a tri-state, and the receipt read it already did).
//
// The axes joined here are REGISTRATION (host truth from `sbx mcp ls` only —
// `sbx mcp get/inspect <name>` shows the registered DEFINITION and says
// nothing about any sandbox, and sbx has no per-sandbox inspect API at all)
// and ATTACHMENT PROVENANCE (the launcher receipt: a record of pix's OWN
// successful operations — preload at create, live `pix mcp load`). The
// remote-server native OAuth axis stays entirely separate (mcpRemoteAuthCheck
// in doctor_mcp.go); a local stdio server never has an auth status.
//
// PRECEDENCE (the point of this file): a valid receipt's POSITIVE claim
// (preloaded/loaded) is evidence pix itself already observed succeeding.
// The CURRENT registration reading is a separate, present-tense fact — sbx
// exposes no API that proves a sandbox was ever unloaded — so it must never
// override an already-observed positive claim. Registration only DECIDES the
// outcome when the receipt has no positive claim to offer.

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
// mcp ls` listing's own success/failure and content — the ONE definition
// doctor and status both build their reg evidence from, so the two verbs can
// never diverge on what "registered" means for the same (mcpOut, mcpOK,
// name).
func McpRegEvidenceFrom(mcpOut string, mcpOK bool, name string) McpRegEvidence {
	if !mcpOK {
		return McpRegUnknown
	}
	if McpRegisteredIn(mcpOut, name) {
		return McpRegYes
	}
	return McpRegNo
}

// The five join states. Exactly one is assigned per (server, sandbox) pair.
const (
	// McpJoinPreloaded: the receipt records pix preloading this server at
	// the sandbox's create (workspace.WriteCreateReceipt). A positive receipt claim, so
	// it wins regardless of the current registration reading (see PRECEDENCE
	// above).
	McpJoinPreloaded = "preloaded"
	// McpJoinLoaded: the receipt records a successful live `pix mcp load`
	// (workspace.AppendLoadReceipt). A positive receipt claim; same precedence as
	// McpJoinPreloaded.
	McpJoinLoaded = "loaded"
	// McpJoinRegisteredNotAttached: registered with the gateway, and a VALID
	// receipt for this sandbox exists but has no entry for this server —
	// pix positively has no record of ever attaching it here.
	McpJoinRegisteredNotAttached = "registered-not-attached"
	// McpJoinNotRegistered: a successful `sbx mcp ls` positively lacks the
	// name, AND the receipt has no positive claim for it either. A stale
	// receipt whose Preloaded/Loads DOES name it never reaches this state —
	// see PRECEDENCE.
	McpJoinNotRegistered = "not-registered"
	// McpJoinUnverifiable: something needed for a truthful answer is missing
	// or untrustworthy — the registration listing failed, or the receipt is
	// absent/corrupt/wrong-schema/wrong-identity/unreadable. Never guessed.
	McpJoinUnverifiable = "unverifiable"
)

// mcpJoinRow is one server's joined truth for one sandbox.
type mcpJoinRow struct {
	Name       string
	Registered McpRegEvidence
	Sandbox    string
	State      string // one of the mcpJoin* constants
	Evidence   string // concrete proof/degrade reason behind State
}

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

// regEvidenceNote renders a clarifying note about the CURRENT registration
// reading, surfaced alongside a receipt's positive claim only when that
// reading is something other than the unremarkable "yes" case (a receipted,
// still-registered server needs no comment). Registration says nothing about
// a sandbox ever being unloaded, so a "no"/"unknown" reading is stated as
// context here, never as a reason to doubt the receipt's claim.
func regEvidenceNote(reg McpRegEvidence) string {
	switch reg {
	case McpRegNo:
		return "currently not registered per `sbx mcp ls` — registration does not undo a receipted attach"
	case McpRegUnknown:
		return "current registration unknown (`sbx mcp ls` unavailable)"
	default:
		return ""
	}
}

// JoinMCPSandboxRow joins one server's registration evidence with one
// sandbox's receipt into a row. Decision order (each earlier rule dominates):
//
//  1. A valid receipt's POSITIVE claim (preloaded / loaded) determines state
//     FIRST — regardless of the CURRENT registration reading (yes/no/
//     unknown). Registration is a separate, present-tense field: sbx exposes
//     no API that proves a sandbox was ever unloaded, so a "no" or "unknown"
//     reading must never override an already-observed positive claim. The
//     current registration reading is still carried as evidence (never
//     silently dropped) except in the unremarkable yes+claim case, where it
//     adds nothing.
//  2. No positive claim, and registration decides the outcome:
//     McpRegNo -> not-registered; McpRegUnknown -> unverifiable.
//  3. No positive claim, registered (McpRegYes): untrustworthy receipt
//     (corrupt / schema / identity / unreadable) or NO receipt at all ->
//     unverifiable, with the exact commands that would make attachment true
//     (and receipted).
//  4. No positive claim, registered, valid PARTIAL receipt (IsPartial:
//     load-only, no create record) -> unverifiable: a partial receipt proves
//     only the loads it lists; the create-time preload set is unknown, so
//     "no entry" is not "positively never attached".
//  5. No positive claim, registered, valid FULL receipt -> registered-not-
//     attached: pix positively has no record of attaching it here.
func JoinMCPSandboxRow(name string, reg McpRegEvidence, sandbox string, receipt *workspace.MCPReceipt, rstatus workspace.MCPStateStatus) mcpJoinRow {
	row := mcpJoinRow{Name: name, Registered: reg, Sandbox: sandbox}
	claim := ReceiptClaim(receipt, rstatus, name)
	if claim != "" {
		row.State = claim
		switch claim {
		case McpJoinPreloaded:
			row.Evidence = "preloaded by pix at create"
		case McpJoinLoaded:
			row.Evidence = "loaded by pix"
		}
		if note := regEvidenceNote(reg); note != "" {
			row.Evidence += "; " + note
		}
		return row
	}
	switch reg {
	case McpRegNo:
		row.State = McpJoinNotRegistered
		row.Evidence = "not in `sbx mcp ls`"
		return row
	case McpRegUnknown:
		row.State = McpJoinUnverifiable
		row.Evidence = "registration listing unavailable (`sbx mcp ls`)"
		return row
	}
	// Registered (McpRegYes), no positive claim. Attachment provenance may
	// come ONLY from a valid receipt.
	if rstatus.Unverifiable() {
		row.State = McpJoinUnverifiable
		row.Evidence = "receipt " + rstatus.String() + "; " + McpAttachGuidance(name)
		return row
	}
	if rstatus == workspace.MCPStateAbsent {
		row.State = McpJoinUnverifiable
		row.Evidence = "receipt absent; " + McpAttachGuidance(name)
		return row
	}
	if receipt.IsPartial() {
		row.State = McpJoinUnverifiable
		row.Evidence = "receipt is partial (load-only, no create record) — preload state unknown; " + McpAttachGuidance(name)
		return row
	}
	row.State = McpJoinRegisteredNotAttached
	row.Evidence = "no receipt entry; " + McpAttachGuidance(name)
	return row
}

// JoinMCPSandboxRows joins every configured name against one sandbox's
// receipt, preserving the configured order. reg supplies each name's
// registration tri-state (from the ONE listing the caller already fetched).
func JoinMCPSandboxRows(names []string, reg func(name string) McpRegEvidence, sandbox string, receipt *workspace.MCPReceipt, rstatus workspace.MCPStateStatus) []mcpJoinRow {
	rows := make([]mcpJoinRow, 0, len(names))
	for _, n := range names {
		rows = append(rows, JoinMCPSandboxRow(n, reg(n), sandbox, receipt, rstatus))
	}
	return rows
}

// McpCurrentIntentNames returns the CURRENT configured-to-preload name
// universe for one config/pack pairing: cfg.MCP first (order preserved), then
// any active-pack integration name not already present, deduped. exclude
// removes names neither list should surface (doctor excludes "gog": true,
// which owns its own dedicated group; status has no such exclusion and
// passes nil).
func McpCurrentIntentNames(cfgMCP []string, containers map[string]config.MCPContainer, exclude map[string]bool) []string {
	var names []string
	seen := map[string]bool{}
	for k := range exclude {
		seen[k] = true
	}
	for _, m := range append(append([]string(nil), cfgMCP...), packContainerNames(containers)...) {
		if m == "" || seen[m] {
			continue
		}
		seen[m] = true
		names = append(names, m)
	}
	return names
}

// McpConfiguredUniverse extends currentIntent (already deduped/ordered) with
// any name a sandbox's OWN receipt independently proves provenance for —
// Preloaded first, then Loads, in the receipt's own order, deduped — that
// currentIntent does not already name. This is what keeps a transient `run
// --pack` mix-in, or a since-switched pack's historical MCP provenance,
// visible on THIS sandbox even after cfg.MCP/the active pack moved on.
// receiptOnly reports which returned names are NOT part of currentIntent, so
// callers can label their evidence as sandbox provenance rather than current
// preload intent. A nil receipt (workspace.ReadMCPReceipt only ever returns
// non-nil on workspace.MCPStateOK) is a no-op extension.
func McpConfiguredUniverse(currentIntent []string, receipt *workspace.MCPReceipt, exclude map[string]bool) (names []string, receiptOnly map[string]bool) {
	seen := map[string]bool{}
	for k := range exclude {
		seen[k] = true
	}
	receiptOnly = map[string]bool{}
	for _, n := range currentIntent {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		names = append(names, n)
	}
	if receipt == nil {
		return names, receiptOnly
	}
	for _, n := range receipt.Preloaded {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		receiptOnly[n] = true
		names = append(names, n)
	}
	for _, l := range receipt.Loads {
		if l.Name == "" || seen[l.Name] {
			continue
		}
		seen[l.Name] = true
		receiptOnly[l.Name] = true
		names = append(names, l.Name)
	}
	return names, receiptOnly
}

// packContainerNames returns the pack-integration server names in a stable
// (sorted) order, so the group renders deterministically.
func packContainerNames(containers map[string]config.MCPContainer) []string {
	if len(containers) == 0 {
		return nil
	}
	names := make([]string, 0, len(containers))
	for n := range containers {
		names = append(names, n)
	}
	// small n; insertion sort avoids an import for sort in this file's diff
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}
