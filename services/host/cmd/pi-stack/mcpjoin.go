package main

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
// and ATTACHMENT PROVENANCE (the launcher receipt: a record of pi-stack's OWN
// successful operations — preload at create, live `pi-stack mcp load`). The
// remote-server native OAuth axis stays entirely separate (mcpRemoteAuthCheck
// in doctor_mcp.go); a local stdio server never has an auth status.

// mcpRegEvidence is the tri-state host registration evidence for one server
// name: what the bounded `sbx mcp ls` listing the caller already ran could
// actually prove. Unknown (listing failed / sbx absent) is a first-class
// state — it is never collapsed into "not registered".
type mcpRegEvidence int

const (
	// mcpRegUnknown: the registration listing could not be obtained, so
	// registration is unknowable here — never guessed either way.
	mcpRegUnknown mcpRegEvidence = iota
	// mcpRegYes: the name is positively present in a successful `sbx mcp ls`.
	mcpRegYes
	// mcpRegNo: a successful `sbx mcp ls` positively lacks the name.
	mcpRegNo
)

// String renders the tri-state for JSON/report rows.
func (r mcpRegEvidence) String() string {
	switch r {
	case mcpRegYes:
		return "yes"
	case mcpRegNo:
		return "no"
	default:
		return "unknown"
	}
}

// The five join states. Exactly one is assigned per (server, sandbox) pair.
const (
	// mcpJoinPreloaded: the receipt records pi-stack preloading this server at
	// the sandbox's create (writeCreateReceipt), and it is still registered.
	mcpJoinPreloaded = "preloaded"
	// mcpJoinLoaded: the receipt records a successful live `pi-stack mcp load`
	// (appendLoadReceipt), and it is still registered.
	mcpJoinLoaded = "loaded"
	// mcpJoinRegisteredNotAttached: registered with the gateway, and a VALID
	// receipt for this sandbox exists but has no entry for this server —
	// pi-stack positively has no record of ever attaching it here.
	mcpJoinRegisteredNotAttached = "registered-not-attached"
	// mcpJoinNotRegistered: a successful `sbx mcp ls` positively lacks the
	// name. Registration truth is CURRENT and dominates any receipt claim
	// (the receipt records a past pi-stack action; the gateway cannot spawn
	// an unregistered server now).
	mcpJoinNotRegistered = "not-registered"
	// mcpJoinUnverifiable: something needed for a truthful answer is missing
	// or untrustworthy — the registration listing failed, or the receipt is
	// absent/corrupt/wrong-schema/wrong-identity/unreadable. Never guessed.
	mcpJoinUnverifiable = "unverifiable"
)

// mcpJoinRow is one server's joined truth for one sandbox.
type mcpJoinRow struct {
	Name       string
	Registered mcpRegEvidence
	Sandbox    string
	State      string // one of the mcpJoin* constants
	Evidence   string // concrete proof/degrade reason behind State
}

// receiptClaim reports what a VALID receipt says about name: "preloaded" (the
// create-time static set), "loaded" (a live `pi-stack mcp load`), or "" (no
// entry). It reads the receipt ONLY when rstatus vouches for it — an absent or
// unverifiable receipt claims nothing.
func receiptClaim(receipt *sandboxMCPReceipt, rstatus sandboxMCPStateStatus, name string) string {
	if rstatus != sandboxMCPStateOK || receipt == nil {
		return ""
	}
	if containsStr(receipt.Preloaded, name) {
		return mcpJoinPreloaded
	}
	for _, l := range receipt.Loads {
		if l.Name == name {
			return mcpJoinLoaded
		}
	}
	return ""
}

// joinMCPSandboxRow joins one server's registration evidence with one
// sandbox's receipt into a row. Decision order (each earlier rule dominates):
//
//  1. mcpRegNo -> not-registered. A receipt claim does not survive
//     deregistration; it is surfaced as STALE evidence, never as a state.
//  2. mcpRegUnknown -> unverifiable (registration unknowable; a receipt claim
//     is surfaced as evidence of past pi-stack provenance only).
//  3. Registered + untrustworthy receipt (corrupt / schema / identity /
//     unreadable) or NO receipt at all -> unverifiable, with the exact
//     commands that would make attachment true (and receipted).
//  4. Registered + valid receipt entry -> preloaded / loaded.
//  5. Registered + valid receipt WITHOUT an entry -> registered-not-attached:
//     pi-stack positively has no record of attaching it to this sandbox.
func joinMCPSandboxRow(name string, reg mcpRegEvidence, sandbox string, receipt *sandboxMCPReceipt, rstatus sandboxMCPStateStatus) mcpJoinRow {
	row := mcpJoinRow{Name: name, Registered: reg, Sandbox: sandbox}
	claim := receiptClaim(receipt, rstatus, name)
	switch reg {
	case mcpRegNo:
		row.State = mcpJoinNotRegistered
		row.Evidence = "not in `sbx mcp ls`"
		if claim != "" {
			row.Evidence += "; stale receipt claims " + claim + " — registration truth wins"
		}
		return row
	case mcpRegUnknown:
		row.State = mcpJoinUnverifiable
		row.Evidence = "registration listing unavailable (`sbx mcp ls`)"
		if claim != "" {
			row.Evidence += "; receipt records " + claim + " by pi-stack"
		}
		return row
	}
	// Registered. Attachment provenance may come ONLY from a valid receipt.
	if rstatus.Unverifiable() {
		row.State = mcpJoinUnverifiable
		row.Evidence = "receipt " + rstatus.String() + "; " + mcpAttachGuidance(name)
		return row
	}
	if rstatus == sandboxMCPStateAbsent {
		row.State = mcpJoinUnverifiable
		row.Evidence = "receipt absent; " + mcpAttachGuidance(name)
		return row
	}
	switch claim {
	case mcpJoinPreloaded:
		row.State = mcpJoinPreloaded
		row.Evidence = "preloaded by pi-stack at create"
	case mcpJoinLoaded:
		row.State = mcpJoinLoaded
		row.Evidence = "loaded by pi-stack"
	default:
		row.State = mcpJoinRegisteredNotAttached
		row.Evidence = "no receipt entry; " + mcpAttachGuidance(name)
	}
	return row
}

// joinMCPSandboxRows joins every configured name against one sandbox's
// receipt, preserving the configured order. reg supplies each name's
// registration tri-state (from the ONE listing the caller already fetched).
func joinMCPSandboxRows(names []string, reg func(name string) mcpRegEvidence, sandbox string, receipt *sandboxMCPReceipt, rstatus sandboxMCPStateStatus) []mcpJoinRow {
	rows := make([]mcpJoinRow, 0, len(names))
	for _, n := range names {
		rows = append(rows, joinMCPSandboxRow(n, reg(n), sandbox, receipt, rstatus))
	}
	return rows
}
