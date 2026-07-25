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
//
// PRECEDENCE (the point of this file): a valid receipt's POSITIVE claim
// (preloaded/loaded) is evidence pi-stack itself already observed succeeding.
// The CURRENT registration reading is a separate, present-tense fact — sbx
// exposes no API that proves a sandbox was ever unloaded — so it must never
// override an already-observed positive claim. Registration only DECIDES the
// outcome when the receipt has no positive claim to offer.

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

// mcpRegEvidenceFrom derives the registration tri-state from a bounded `sbx
// mcp ls` listing's own success/failure and content — the ONE definition
// doctor and status both build their reg evidence from, so the two verbs can
// never diverge on what "registered" means for the same (mcpOut, mcpOK,
// name).
func mcpRegEvidenceFrom(mcpOut string, mcpOK bool, name string) mcpRegEvidence {
	if !mcpOK {
		return mcpRegUnknown
	}
	if mcpRegisteredIn(mcpOut, name) {
		return mcpRegYes
	}
	return mcpRegNo
}

// The five join states. Exactly one is assigned per (server, sandbox) pair.
const (
	// mcpJoinPreloaded: the receipt records pi-stack preloading this server at
	// the sandbox's create (writeCreateReceipt). A positive receipt claim, so
	// it wins regardless of the current registration reading (see PRECEDENCE
	// above).
	mcpJoinPreloaded = "preloaded"
	// mcpJoinLoaded: the receipt records a successful live `pi-stack mcp load`
	// (appendLoadReceipt). A positive receipt claim; same precedence as
	// mcpJoinPreloaded.
	mcpJoinLoaded = "loaded"
	// mcpJoinRegisteredNotAttached: registered with the gateway, and a VALID
	// receipt for this sandbox exists but has no entry for this server —
	// pi-stack positively has no record of ever attaching it here.
	mcpJoinRegisteredNotAttached = "registered-not-attached"
	// mcpJoinNotRegistered: a successful `sbx mcp ls` positively lacks the
	// name, AND the receipt has no positive claim for it either. A stale
	// receipt whose Preloaded/Loads DOES name it never reaches this state —
	// see PRECEDENCE.
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

// regEvidenceNote renders a clarifying note about the CURRENT registration
// reading, surfaced alongside a receipt's positive claim only when that
// reading is something other than the unremarkable "yes" case (a receipted,
// still-registered server needs no comment). Registration says nothing about
// a sandbox ever being unloaded, so a "no"/"unknown" reading is stated as
// context here, never as a reason to doubt the receipt's claim.
func regEvidenceNote(reg mcpRegEvidence) string {
	switch reg {
	case mcpRegNo:
		return "currently not registered per `sbx mcp ls` — registration does not undo a receipted attach"
	case mcpRegUnknown:
		return "current registration unknown (`sbx mcp ls` unavailable)"
	default:
		return ""
	}
}

// joinMCPSandboxRow joins one server's registration evidence with one
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
//     mcpRegNo -> not-registered; mcpRegUnknown -> unverifiable.
//  3. No positive claim, registered (mcpRegYes): untrustworthy receipt
//     (corrupt / schema / identity / unreadable) or NO receipt at all ->
//     unverifiable, with the exact commands that would make attachment true
//     (and receipted).
//  4. No positive claim, registered, valid PARTIAL receipt (IsPartial:
//     load-only, no create record) -> unverifiable: a partial receipt proves
//     only the loads it lists; the create-time preload set is unknown, so
//     "no entry" is not "positively never attached".
//  5. No positive claim, registered, valid FULL receipt -> registered-not-
//     attached: pi-stack positively has no record of attaching it here.
func joinMCPSandboxRow(name string, reg mcpRegEvidence, sandbox string, receipt *sandboxMCPReceipt, rstatus sandboxMCPStateStatus) mcpJoinRow {
	row := mcpJoinRow{Name: name, Registered: reg, Sandbox: sandbox}
	claim := receiptClaim(receipt, rstatus, name)
	if claim != "" {
		row.State = claim
		switch claim {
		case mcpJoinPreloaded:
			row.Evidence = "preloaded by pi-stack at create"
		case mcpJoinLoaded:
			row.Evidence = "loaded by pi-stack"
		}
		if note := regEvidenceNote(reg); note != "" {
			row.Evidence += "; " + note
		}
		return row
	}
	switch reg {
	case mcpRegNo:
		row.State = mcpJoinNotRegistered
		row.Evidence = "not in `sbx mcp ls`"
		return row
	case mcpRegUnknown:
		row.State = mcpJoinUnverifiable
		row.Evidence = "registration listing unavailable (`sbx mcp ls`)"
		return row
	}
	// Registered (mcpRegYes), no positive claim. Attachment provenance may
	// come ONLY from a valid receipt.
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
	if receipt.IsPartial() {
		row.State = mcpJoinUnverifiable
		row.Evidence = "receipt is partial (load-only, no create record) — preload state unknown; " + mcpAttachGuidance(name)
		return row
	}
	row.State = mcpJoinRegisteredNotAttached
	row.Evidence = "no receipt entry; " + mcpAttachGuidance(name)
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

// mcpCurrentIntentNames returns the CURRENT configured-to-preload name
// universe for one config/pack pairing: cfg.MCP first (order preserved), then
// any active-pack integration name not already present, deduped. exclude
// removes names neither list should surface (doctor excludes "gog": true,
// which owns its own dedicated group; status has no such exclusion and
// passes nil).
func mcpCurrentIntentNames(cfgMCP []string, containers map[string]packContainer, exclude map[string]bool) []string {
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

// mcpConfiguredUniverse extends currentIntent (already deduped/ordered) with
// any name a sandbox's OWN receipt independently proves provenance for —
// Preloaded first, then Loads, in the receipt's own order, deduped — that
// currentIntent does not already name. This is what keeps a transient `run
// --pack` mix-in, or a since-switched pack's historical MCP provenance,
// visible on THIS sandbox even after cfg.MCP/the active pack moved on.
// receiptOnly reports which returned names are NOT part of currentIntent, so
// callers can label their evidence as sandbox provenance rather than current
// preload intent. A nil receipt (readSandboxMCPReceipt only ever returns
// non-nil on sandboxMCPStateOK) is a no-op extension.
func mcpConfiguredUniverse(currentIntent []string, receipt *sandboxMCPReceipt, exclude map[string]bool) (names []string, receiptOnly map[string]bool) {
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
