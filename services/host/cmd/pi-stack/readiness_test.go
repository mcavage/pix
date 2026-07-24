package main

import "testing"

// TestReadiness_Blocking is the exit-code contract at the heart of U1: only a
// CORE requirement paired with FAILED (verified) evidence blocks the command
// (exit 1). Every other combination — optional (either flavor), or ANY
// requirement paired with unverifiable/not-configured evidence — is
// non-blocking (exit 0). This is the single source of truth runDoctorCmd's
// exit path reads; it must never be re-derived from detail-text parsing.
func TestReadiness_Blocking(t *testing.T) {
	cases := []struct {
		name string
		req  Requirement
		ev   Evidence
		want bool
	}{
		{"core+failed blocks", RequirementCore, EvidenceFailed, true},
		{"core+healthy does not block", RequirementCore, EvidenceHealthy, false},
		{"core+unverifiable does not block", RequirementCore, EvidenceUnverifiable, false},
		{"core+not-configured does not block", RequirementCore, EvidenceNotConfigured, false},
		{"configured-optional+failed does not block", RequirementConfiguredOptional, EvidenceFailed, false},
		{"unconfigured-optional+failed does not block", RequirementUnconfiguredOptional, EvidenceFailed, false},
		{"unconfigured-optional+not-configured does not block", RequirementUnconfiguredOptional, EvidenceNotConfigured, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Blocking(c.req, c.ev); got != c.want {
				t.Errorf("Blocking(%v,%v) = %v, want %v", c.req, c.ev, got, c.want)
			}
		})
	}
}

// TestReport_Blocking exercises the report-level aggregate: reportBlocking
// scans every check across every group and reports true iff at least one is a
// verified core failure — never by parsing detail text.
func TestReport_Blocking(t *testing.T) {
	allGreen := &report{groups: []group{
		{checks: []check{
			{label: "anthropic", state: stateOK, requirement: RequirementCore, evidence: EvidenceHealthy},
			{label: "github", state: stateTODO, todo: "x", requirement: RequirementConfiguredOptional, evidence: EvidenceFailed},
		}},
	}}
	if allGreen.blocking() {
		t.Errorf("optional failure alone must not block")
	}

	sandboxed := &report{groups: []group{
		{checks: []check{
			{label: "anthropic", state: stateTODO, todo: "x", requirement: RequirementCore, evidence: EvidenceUnverifiable},
		}},
	}}
	if sandboxed.blocking() {
		t.Errorf("unverifiable core check must not block (sandbox-without-sbx case)")
	}

	broken := &report{groups: []group{
		{checks: []check{
			{label: "anthropic", state: stateTODO, todo: "x", requirement: RequirementCore, evidence: EvidenceFailed},
		}},
	}}
	if !broken.blocking() {
		t.Errorf("a verified core failure must block")
	}
}
