package main

// Requirement classifies how much a check matters: whether it is load-bearing
// for pi-stack to function at all (core), or a nice-to-have integration that
// happens to be wired up (configured-optional) or simply hasn't been opted
// into yet (unconfigured-optional). This is the axis doctor's exit code reads
// — NEVER derived by parsing a check's detail text.
type Requirement string

const (
	// RequirementCore is load-bearing: pi-stack cannot do useful work without
	// it (the three model-provider keys today). A VERIFIED failure here is the
	// only thing that makes doctor exit 1.
	RequirementCore Requirement = "core"
	// RequirementConfiguredOptional is an optional integration the user HAS
	// opted into (github key present by policy default, gog with an account
	// set, a server in cfg.MCP, memory enabled in cfg.Services, …). Its
	// failure is worth surfacing but never blocks.
	RequirementConfiguredOptional Requirement = "configured-optional"
	// RequirementUnconfiguredOptional is an optional integration nobody has
	// opted into yet (e.g. gog with no account configured and not attached).
	// Its absence is expected, not a gap.
	RequirementUnconfiguredOptional Requirement = "unconfigured-optional"
)

// Evidence classifies what a check actually PROVED, independent of how
// important the thing being checked is:
type Evidence string

const (
	// EvidenceHealthy: verified working.
	EvidenceHealthy Evidence = "healthy"
	// EvidenceFailed: verified NOT working (a real, confirmed gap).
	EvidenceFailed Evidence = "failed"
	// EvidenceUnverifiable: could not be checked from here (sbx absent, e.g.
	// running inside the sandbox; a registration doctor couldn't read; a
	// dependency needed to run the probe is missing). Distinct from a failure:
	// doctor does not know, so it must not claim ✗.
	EvidenceUnverifiable Evidence = "unverifiable"
	// EvidenceNotConfigured: the optional feature was never set up/opted into.
	// Absence here is expected, not a gap to fix.
	EvidenceNotConfigured Evidence = "not-configured"
)

// Blocking is the single source of truth for whether a (requirement,
// evidence) pair should make `pi-stack doctor` exit 1. Only a verified
// (evidence=failed) failure of a CORE requirement blocks. Everything else —
// optional (either flavor) regardless of evidence, or ANY requirement that is
// merely unverifiable or not-configured — is non-blocking. This is read
// directly off the structured fields; doctor must never re-derive it by
// pattern-matching a check's rendered detail string.
func Blocking(req Requirement, ev Evidence) bool {
	return req == RequirementCore && ev == EvidenceFailed
}

// blocking reports whether ANY check across the whole report is a verified
// core failure — the aggregate `pi-stack doctor` reads to decide its exit
// code (1 vs 0). Usage errors are handled separately by parseDoctorArgs and
// always exit 2 regardless of this.
func (r *report) blocking() bool {
	for _, g := range r.groups {
		for _, c := range g.checks {
			if Blocking(c.requirement, c.evidence) {
				return true
			}
		}
	}
	return false
}
