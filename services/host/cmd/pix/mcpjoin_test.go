package main

import (
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

	loaded := joinMCPSandboxRow("slack", mcpRegYes, box, r, workspace.MCPStateOK)
	if loaded.State != mcpJoinLoaded {
		t.Errorf("listed load: state = %q, want %q", loaded.State, mcpJoinLoaded)
	}

	other := joinMCPSandboxRow(gwServerName, mcpRegYes, box, r, workspace.MCPStateOK)
	if other.State != mcpJoinUnverifiable {
		t.Errorf("unlisted name on a partial receipt: state = %q, want %q", other.State, mcpJoinUnverifiable)
	}
	if !strings.Contains(other.Evidence, "partial") {
		t.Errorf("evidence should say the receipt is partial, got %q", other.Evidence)
	}
	if !strings.Contains(other.Evidence, "pix mcp load google-workspace") {
		t.Errorf("evidence should carry the attach guidance, got %q", other.Evidence)
	}

	// A FULL receipt keeps the positive registered-not-attached answer.
	full := joinMCPSandboxRow(gwServerName, mcpRegYes, box, okReceipt(box, nil, "slack"), workspace.MCPStateOK)
	if full.State != mcpJoinRegisteredNotAttached {
		t.Errorf("unlisted name on a full receipt: state = %q, want %q", full.State, mcpJoinRegisteredNotAttached)
	}
}

// TestJoinMCPSandboxRowStates covers all five join states from the single
// shared truth path both doctor and status render from.
func TestJoinMCPSandboxRowStates(t *testing.T) {
	const box = "pix-proj"
	cases := []struct {
		name         string
		reg          mcpRegEvidence
		receipt      *workspace.MCPReceipt
		rstatus      workspace.MCPStateStatus
		wantState    string
		wantEvidence string // substring
	}{
		{"preloaded", mcpRegYes, okReceipt(box, []string{"slack"}), workspace.MCPStateOK,
			mcpJoinPreloaded, "preloaded by pix at create"},
		{"loaded", mcpRegYes, okReceipt(box, nil, "slack"), workspace.MCPStateOK,
			mcpJoinLoaded, "loaded by pix"},
		{"registered-not-attached", mcpRegYes, okReceipt(box, []string{"notion"}), workspace.MCPStateOK,
			mcpJoinRegisteredNotAttached, "no receipt entry"},
		{"not-registered", mcpRegNo, okReceipt(box, nil), workspace.MCPStateOK,
			mcpJoinNotRegistered, "not in `sbx mcp ls`"},
		{"unverifiable: receipt absent", mcpRegYes, nil, workspace.MCPStateAbsent,
			mcpJoinUnverifiable, "receipt absent"},
		{"unverifiable: receipt corrupt", mcpRegYes, nil, workspace.MCPStateCorrupt,
			mcpJoinUnverifiable, "receipt corrupt"},
		{"unverifiable: schema mismatch", mcpRegYes, nil, workspace.MCPStateSchemaMismatch,
			mcpJoinUnverifiable, "receipt schema-mismatch"},
		{"unverifiable: identity mismatch", mcpRegYes, nil, workspace.MCPStateIdentityMismatch,
			mcpJoinUnverifiable, "receipt identity-mismatch"},
		{"unverifiable: receipt unreadable", mcpRegYes, nil, workspace.MCPStateUnreadable,
			mcpJoinUnverifiable, "receipt unreadable"},
		{"unverifiable: registration listing unavailable", mcpRegUnknown, okReceipt(box, nil), workspace.MCPStateOK,
			mcpJoinUnverifiable, "registration listing unavailable"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := joinMCPSandboxRow("slack", tc.reg, box, tc.receipt, tc.rstatus)
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
	row := joinMCPSandboxRow("slack", mcpRegNo, box, okReceipt(box, nil), workspace.MCPStateOK)
	if row.State != mcpJoinNotRegistered {
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

	preloaded := joinMCPSandboxRow("slack", mcpRegNo, box, okReceipt(box, []string{"slack"}), workspace.MCPStateOK)
	if preloaded.State != mcpJoinPreloaded {
		t.Fatalf("state = %q, want preloaded even though currently deregistered (row %+v)", preloaded.State, preloaded)
	}
	if !strings.Contains(preloaded.Evidence, "preloaded by pix at create") {
		t.Errorf("evidence should carry the receipt provenance: %q", preloaded.Evidence)
	}
	if !strings.Contains(preloaded.Evidence, "currently not registered") {
		t.Errorf("evidence should still name the current registration reading: %q", preloaded.Evidence)
	}

	loaded := joinMCPSandboxRow("slack", mcpRegNo, box, okReceipt(box, nil, "slack"), workspace.MCPStateOK)
	if loaded.State != mcpJoinLoaded {
		t.Fatalf("state = %q, want loaded even though currently deregistered (row %+v)", loaded.State, loaded)
	}
}

// TestJoinPositiveReceiptSurvivesUnknownRegistration pins finding #1's other
// half: an UNKNOWN current registration reading (the listing is down) must
// not block a valid receipt's positive claim either — the state is still
// preloaded/loaded, with the registration-unknown fact carried as evidence.
func TestJoinPositiveReceiptSurvivesUnknownRegistration(t *testing.T) {
	const box = "pix-proj"
	row := joinMCPSandboxRow("slack", mcpRegUnknown, box, okReceipt(box, []string{"slack"}), workspace.MCPStateOK)
	if row.State != mcpJoinPreloaded {
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
		row := joinMCPSandboxRow("slack", mcpRegYes, "pix-proj", nil, rstatus)
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
	receipt := okReceipt(box, []string{gwServerName}, "slack")
	reg := func(name string) mcpRegEvidence {
		if name == "linear" {
			return mcpRegNo
		}
		return mcpRegYes
	}
	rows := joinMCPSandboxRows([]string{gwServerName, "slack", "notion", "linear"}, reg, box, receipt, workspace.MCPStateOK)
	want := []string{mcpJoinPreloaded, mcpJoinLoaded, mcpJoinRegisteredNotAttached, mcpJoinNotRegistered}
	if len(rows) != len(want) {
		t.Fatalf("rows = %+v, want %d", rows, len(want))
	}
	for i, w := range want {
		if rows[i].State != w {
			t.Errorf("rows[%d] (%s) state = %q, want %q", i, rows[i].Name, rows[i].State, w)
		}
	}
}
