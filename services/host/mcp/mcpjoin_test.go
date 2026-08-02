package mcp

import (
	"pix/host/config"
	"pix/host/workspace"
	"strings"
	"testing"
)

// okReceipt builds a valid FULL schema-1 receipt (CreatedAt set — a create
// pix observed) for sandbox with the given preloaded set and load
// entries. See partialReceipt for the load-only variant.
func okReceipt(sandbox string, preloaded []string, loads ...string) *workspace.MCPReceipt {
	r := &workspace.MCPReceipt{Schema: workspace.MCPStateSchema, Sandbox: sandbox,
		CreatedAt: "2026-01-02T03:04:05Z", Preloaded: preloaded}
	for _, n := range loads {
		r.Loads = append(r.Loads, workspace.MCPLoadReceipt{Name: n, At: "2026-01-02T03:04:05Z"})
	}
	return r
}

// partialReceipt builds a PARTIAL (load-only, no CreatedAt/Preloaded) receipt
// — what workspace.AppendLoadReceipt synthesizes for a sandbox whose creation pix
// never observed.
func partialReceipt(sandbox string, loads ...string) *workspace.MCPReceipt {
	r := &workspace.MCPReceipt{Schema: workspace.MCPStateSchema, Sandbox: sandbox}
	for _, n := range loads {
		r.Loads = append(r.Loads, workspace.MCPLoadReceipt{Name: n, At: "2026-01-02T03:04:05Z"})
	}
	return r
}

// TestJoinMCPSandboxRow_PartialReceipt pins D: a partial receipt proves ONLY
// the entries in Loads. A load it lists renders loaded; EVERY other name
// renders unverifiable — never registered-not-attached, because the
// create-time preload set is unknown, so "no entry" is not "positively never
// attached".
func TestJoinMCPSandboxRow_PartialReceipt(t *testing.T) {
	const box = "pix-proj"
	r := partialReceipt(box, "slack")
	if !r.IsPartial() {
		t.Fatal("precondition: a load-only receipt (empty CreatedAt) must be IsPartial")
	}

	loaded := JoinMCPSandboxRow("slack", McpRegYes, box, r, workspace.MCPStateOK)
	if loaded.State != McpJoinLoaded {
		t.Errorf("listed load: state = %q, want %q", loaded.State, McpJoinLoaded)
	}

	other := JoinMCPSandboxRow(config.GWServerName, McpRegYes, box, r, workspace.MCPStateOK)
	if other.State != McpJoinUnverifiable {
		t.Errorf("unlisted name on a partial receipt: state = %q, want %q", other.State, McpJoinUnverifiable)
	}
	if !strings.Contains(other.Evidence, "partial") {
		t.Errorf("evidence should say the receipt is partial, got %q", other.Evidence)
	}
	if !strings.Contains(other.Evidence, "pix mcp load google-workspace") {
		t.Errorf("evidence should carry the attach guidance, got %q", other.Evidence)
	}

	// A FULL receipt keeps the positive registered-not-attached answer.
	full := JoinMCPSandboxRow(config.GWServerName, McpRegYes, box, okReceipt(box, nil, "slack"), workspace.MCPStateOK)
	if full.State != McpJoinRegisteredNotAttached {
		t.Errorf("unlisted name on a full receipt: state = %q, want %q", full.State, McpJoinRegisteredNotAttached)
	}
}

// TestJoinMCPSandboxRowStates covers all five join states from the single
// shared truth path both doctor and status render from.
func TestJoinMCPSandboxRowStates(t *testing.T) {
	const box = "pix-proj"
	cases := []struct {
		Name         string
		reg          McpRegEvidence
		receipt      *workspace.MCPReceipt
		rstatus      workspace.MCPStateStatus
		wantState    string
		wantEvidence string // substring
	}{
		{"preloaded", McpRegYes, okReceipt(box, []string{"slack"}), workspace.MCPStateOK,
			McpJoinPreloaded, "preloaded by pix at create"},
		{"loaded", McpRegYes, okReceipt(box, nil, "slack"), workspace.MCPStateOK,
			McpJoinLoaded, "loaded by pix"},
		{"registered-not-attached", McpRegYes, okReceipt(box, []string{"notion"}), workspace.MCPStateOK,
			McpJoinRegisteredNotAttached, "no receipt entry"},
		{"not-registered", McpRegNo, okReceipt(box, nil), workspace.MCPStateOK,
			McpJoinNotRegistered, "not in `sbx mcp ls`"},
		{"unverifiable: receipt absent", McpRegYes, nil, workspace.MCPStateAbsent,
			McpJoinUnverifiable, "receipt absent"},
		{"unverifiable: receipt corrupt", McpRegYes, nil, workspace.MCPStateCorrupt,
			McpJoinUnverifiable, "receipt corrupt"},
		{"unverifiable: schema mismatch", McpRegYes, nil, workspace.MCPStateSchemaMismatch,
			McpJoinUnverifiable, "receipt schema-mismatch"},
		{"unverifiable: identity mismatch", McpRegYes, nil, workspace.MCPStateIdentityMismatch,
			McpJoinUnverifiable, "receipt identity-mismatch"},
		{"unverifiable: receipt unreadable", McpRegYes, nil, workspace.MCPStateUnreadable,
			McpJoinUnverifiable, "receipt unreadable"},
		{"unverifiable: registration listing unavailable", McpRegUnknown, okReceipt(box, nil), workspace.MCPStateOK,
			McpJoinUnverifiable, "registration listing unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			row := JoinMCPSandboxRow("slack", tc.reg, box, tc.receipt, tc.rstatus)
			if row.State != tc.wantState {
				t.Errorf("state = %q, want %q (row %+v)", row.State, tc.wantState, row)
			}
			if !strings.Contains(row.Evidence, tc.wantEvidence) {
				t.Errorf("evidence %q missing %q", row.Evidence, tc.wantEvidence)
			}
			if row.Name != "slack" || row.Sandbox != box || row.Registered != tc.reg {
				t.Errorf("row identity fields wrong: %+v", row)
			}
		})
	}
}

// TestJoinNotRegisteredWhenNoPositiveClaim: with NO positive claim for the
// name (the receipt is silent on it), a positively-confirmed deregistration
// still wins — not-registered.
func TestJoinNotRegisteredWhenNoPositiveClaim(t *testing.T) {
	const box = "pix-proj"
	row := JoinMCPSandboxRow("slack", McpRegNo, box, okReceipt(box, nil), workspace.MCPStateOK)
	if row.State != McpJoinNotRegistered {
		t.Fatalf("state = %q, want not-registered when the receipt has no claim for this name (row %+v)", row.State, row)
	}
}

// TestJoinPositiveReceiptDominatesDeregistration pins finding #1: a valid
// receipt's POSITIVE claim (preloaded/loaded) determines state FIRST,
// regardless of the current registration reading — registration is a
// separate, present-tense fact and does not prove the sandbox was ever
// unloaded. A confirmed "not registered right now" reading is surfaced only
// as evidence context, never promoted over an already-observed attach.
func TestJoinPositiveReceiptDominatesDeregistration(t *testing.T) {
	const box = "pix-proj"

	preloaded := JoinMCPSandboxRow("slack", McpRegNo, box, okReceipt(box, []string{"slack"}), workspace.MCPStateOK)
	if preloaded.State != McpJoinPreloaded {
		t.Fatalf("state = %q, want preloaded even though currently deregistered (row %+v)", preloaded.State, preloaded)
	}
	if !strings.Contains(preloaded.Evidence, "preloaded by pix at create") {
		t.Errorf("evidence should carry the receipt provenance: %q", preloaded.Evidence)
	}
	if !strings.Contains(preloaded.Evidence, "currently not registered") {
		t.Errorf("evidence should still name the current registration reading: %q", preloaded.Evidence)
	}

	loaded := JoinMCPSandboxRow("slack", McpRegNo, box, okReceipt(box, nil, "slack"), workspace.MCPStateOK)
	if loaded.State != McpJoinLoaded {
		t.Fatalf("state = %q, want loaded even though currently deregistered (row %+v)", loaded.State, loaded)
	}
}

// TestJoinPositiveReceiptSurvivesUnknownRegistration pins finding #1's other
// half: an UNKNOWN current registration reading (the listing is down) must
// not block a valid receipt's positive claim either — the state is still
// preloaded/loaded, with the registration-unknown fact carried as evidence.
func TestJoinPositiveReceiptSurvivesUnknownRegistration(t *testing.T) {
	const box = "pix-proj"
	row := JoinMCPSandboxRow("slack", McpRegUnknown, box, okReceipt(box, []string{"slack"}), workspace.MCPStateOK)
	if row.State != McpJoinPreloaded {
		t.Fatalf("state = %q, want preloaded when registration is merely unknowable", row.State)
	}
	if !strings.Contains(row.Evidence, "preloaded by pix at create") {
		t.Errorf("evidence should carry the receipt provenance: %q", row.Evidence)
	}
	if !strings.Contains(row.Evidence, "registration unknown") {
		t.Errorf("evidence should name the registration-unknown fact: %q", row.Evidence)
	}
}

// TestJoinUnverifiableCarriesRepairGuidance: an unverifiable receipt row
// carries the exact evidence-producing commands (`pix mcp load` /
// `pix run --replace`) — guidance, never a claim.
func TestJoinUnverifiableCarriesRepairGuidance(t *testing.T) {
	for _, rstatus := range []workspace.MCPStateStatus{workspace.MCPStateAbsent, workspace.MCPStateCorrupt} {
		row := JoinMCPSandboxRow("slack", McpRegYes, "pix-proj", nil, rstatus)
		if !strings.Contains(row.Evidence, "pix mcp load slack") ||
			!strings.Contains(row.Evidence, "pix run --replace") {
			t.Errorf("%s: evidence missing repair guidance: %q", rstatus, row.Evidence)
		}
	}
}

// TestJoinMCPSandboxRowsOrderAndFanout: the plural form preserves configured
// order and applies each name's own registration tri-state.
func TestJoinMCPSandboxRowsOrderAndFanout(t *testing.T) {
	const box = "pix-proj"
	receipt := okReceipt(box, []string{config.GWServerName}, "slack")
	reg := func(name string) McpRegEvidence {
		if name == "linear" {
			return McpRegNo
		}
		return McpRegYes
	}
	rows := JoinMCPSandboxRows([]string{config.GWServerName, "slack", "notion", "linear"}, reg, box, receipt, workspace.MCPStateOK)
	want := []string{McpJoinPreloaded, McpJoinLoaded, McpJoinRegisteredNotAttached, McpJoinNotRegistered}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v, want %d", rows, len(want))
	}
	for i, w := range want {
		if rows[i].State != w {
			t.Errorf("rows[%d] (%s) state = %q, want %q", i, rows[i].Name, rows[i].State, w)
		}
	}
}
