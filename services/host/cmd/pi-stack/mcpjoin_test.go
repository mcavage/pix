package main

import (
	"strings"
	"testing"
)

// okReceipt builds a valid FULL schema-1 receipt (CreatedAt set — a create
// pi-stack observed) for sandbox with the given preloaded set and load
// entries. See partialReceipt for the load-only variant.
func okReceipt(sandbox string, preloaded []string, loads ...string) *sandboxMCPReceipt {
	r := &sandboxMCPReceipt{Schema: sandboxMCPStateSchema, Sandbox: sandbox,
		CreatedAt: "2026-01-02T03:04:05Z", Preloaded: preloaded}
	for _, n := range loads {
		r.Loads = append(r.Loads, sandboxMCPLoadReceipt{Name: n, At: "2026-01-02T03:04:05Z"})
	}
	return r
}

// partialReceipt builds a PARTIAL (load-only, no CreatedAt/Preloaded) receipt
// — what appendLoadReceipt synthesizes for a sandbox whose creation pi-stack
// never observed.
func partialReceipt(sandbox string, loads ...string) *sandboxMCPReceipt {
	r := &sandboxMCPReceipt{Schema: sandboxMCPStateSchema, Sandbox: sandbox}
	for _, n := range loads {
		r.Loads = append(r.Loads, sandboxMCPLoadReceipt{Name: n, At: "2026-01-02T03:04:05Z"})
	}
	return r
}

// TestJoinMCPSandboxRow_PartialReceipt pins D: a partial receipt proves ONLY
// the entries in Loads. A load it lists renders loaded; EVERY other name
// renders unverifiable — never registered-not-attached, because the
// create-time preload set is unknown, so "no entry" is not "positively never
// attached".
func TestJoinMCPSandboxRow_PartialReceipt(t *testing.T) {
	const box = "pi-stack-proj"
	r := partialReceipt(box, "slack")
	if !r.IsPartial() {
		t.Fatal("precondition: a load-only receipt (empty CreatedAt) must be IsPartial")
	}

	loaded := joinMCPSandboxRow("slack", mcpRegYes, box, r, sandboxMCPStateOK)
	if loaded.State != mcpJoinLoaded {
		t.Errorf("listed load: state = %q, want %q", loaded.State, mcpJoinLoaded)
	}

	other := joinMCPSandboxRow("gog", mcpRegYes, box, r, sandboxMCPStateOK)
	if other.State != mcpJoinUnverifiable {
		t.Errorf("unlisted name on a partial receipt: state = %q, want %q", other.State, mcpJoinUnverifiable)
	}
	if !strings.Contains(other.Evidence, "partial") {
		t.Errorf("evidence should say the receipt is partial, got %q", other.Evidence)
	}
	if !strings.Contains(other.Evidence, "pi-stack mcp load gog") {
		t.Errorf("evidence should carry the attach guidance, got %q", other.Evidence)
	}

	// A FULL receipt keeps the positive registered-not-attached answer.
	full := joinMCPSandboxRow("gog", mcpRegYes, box, okReceipt(box, nil, "slack"), sandboxMCPStateOK)
	if full.State != mcpJoinRegisteredNotAttached {
		t.Errorf("unlisted name on a full receipt: state = %q, want %q", full.State, mcpJoinRegisteredNotAttached)
	}
}

// TestJoinMCPSandboxRowStates covers all five join states from the single
// shared truth path both doctor and status render from.
func TestJoinMCPSandboxRowStates(t *testing.T) {
	const box = "pi-stack-proj"
	cases := []struct {
		name         string
		reg          mcpRegEvidence
		receipt      *sandboxMCPReceipt
		rstatus      sandboxMCPStateStatus
		wantState    string
		wantEvidence string // substring
	}{
		{"preloaded", mcpRegYes, okReceipt(box, []string{"slack"}), sandboxMCPStateOK,
			mcpJoinPreloaded, "preloaded by pi-stack at create"},
		{"loaded", mcpRegYes, okReceipt(box, nil, "slack"), sandboxMCPStateOK,
			mcpJoinLoaded, "loaded by pi-stack"},
		{"registered-not-attached", mcpRegYes, okReceipt(box, []string{"notion"}), sandboxMCPStateOK,
			mcpJoinRegisteredNotAttached, "no receipt entry"},
		{"not-registered", mcpRegNo, okReceipt(box, nil), sandboxMCPStateOK,
			mcpJoinNotRegistered, "not in `sbx mcp ls`"},
		{"unverifiable: receipt absent", mcpRegYes, nil, sandboxMCPStateAbsent,
			mcpJoinUnverifiable, "receipt absent"},
		{"unverifiable: receipt corrupt", mcpRegYes, nil, sandboxMCPStateCorrupt,
			mcpJoinUnverifiable, "receipt corrupt"},
		{"unverifiable: schema mismatch", mcpRegYes, nil, sandboxMCPStateSchemaMismatch,
			mcpJoinUnverifiable, "receipt schema-mismatch"},
		{"unverifiable: identity mismatch", mcpRegYes, nil, sandboxMCPStateIdentityMismatch,
			mcpJoinUnverifiable, "receipt identity-mismatch"},
		{"unverifiable: receipt unreadable", mcpRegYes, nil, sandboxMCPStateUnreadable,
			mcpJoinUnverifiable, "receipt unreadable"},
		{"unverifiable: registration listing unavailable", mcpRegUnknown, okReceipt(box, []string{"slack"}), sandboxMCPStateOK,
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

// TestJoinNotRegisteredDominatesStaleReceipt: a receipt claim never survives
// positive deregistration — the state is not-registered, and the stale claim
// is surfaced only as evidence.
func TestJoinNotRegisteredDominatesStaleReceipt(t *testing.T) {
	const box = "pi-stack-proj"
	row := joinMCPSandboxRow("slack", mcpRegNo, box, okReceipt(box, []string{"slack"}), sandboxMCPStateOK)
	if row.State != mcpJoinNotRegistered {
		t.Fatalf("state = %q, want not-registered even with a preload receipt (row %+v)", row.State, row)
	}
	if !strings.Contains(row.Evidence, "stale receipt claims preloaded") {
		t.Errorf("evidence should surface the stale receipt claim: %q", row.Evidence)
	}
}

// TestJoinUnknownRegistrationNeverClaimsAttached: with the listing down,
// even a valid preload receipt yields unverifiable — provenance is stated as
// evidence, never promoted to an attached state.
func TestJoinUnknownRegistrationNeverClaimsAttached(t *testing.T) {
	const box = "pi-stack-proj"
	row := joinMCPSandboxRow("slack", mcpRegUnknown, box, okReceipt(box, []string{"slack"}), sandboxMCPStateOK)
	if row.State != mcpJoinUnverifiable {
		t.Fatalf("state = %q, want unverifiable when registration is unknowable", row.State)
	}
	if !strings.Contains(row.Evidence, "receipt records preloaded by pi-stack") {
		t.Errorf("evidence should carry the receipt provenance: %q", row.Evidence)
	}
}

// TestJoinUnverifiableCarriesRepairGuidance: an unverifiable receipt row
// carries the exact evidence-producing commands (`pi-stack mcp load` /
// `pi-stack run --replace`) — guidance, never a claim.
func TestJoinUnverifiableCarriesRepairGuidance(t *testing.T) {
	for _, rstatus := range []sandboxMCPStateStatus{sandboxMCPStateAbsent, sandboxMCPStateCorrupt} {
		row := joinMCPSandboxRow("slack", mcpRegYes, "pi-stack-proj", nil, rstatus)
		if !strings.Contains(row.Evidence, "pi-stack mcp load slack") ||
			!strings.Contains(row.Evidence, "pi-stack run --replace") {
			t.Errorf("%s: evidence missing repair guidance: %q", rstatus, row.Evidence)
		}
	}
}

// TestJoinMCPSandboxRowsOrderAndFanout: the plural form preserves configured
// order and applies each name's own registration tri-state.
func TestJoinMCPSandboxRowsOrderAndFanout(t *testing.T) {
	const box = "pi-stack-proj"
	receipt := okReceipt(box, []string{"gog"}, "slack")
	reg := func(name string) mcpRegEvidence {
		if name == "linear" {
			return mcpRegNo
		}
		return mcpRegYes
	}
	rows := joinMCPSandboxRows([]string{"gog", "slack", "notion", "linear"}, reg, box, receipt, sandboxMCPStateOK)
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
