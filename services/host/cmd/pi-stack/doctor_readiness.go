package main

import "strings"

// doctor's readiness model: every check carries two structured axes plus a
// concrete evidence string, and everything downstream (glyphs, the headline,
// the TODO list, the JSON payload, the exit code) is DERIVED from them — never
// re-parsed out of a check's human detail text.
//
//   - requirement says how much the check matters: core (pi-stack cannot do
//     useful work without it) or optional (an integration that is nice to have).
//   - verdict says what the probe actually PROVED: ready (verified working),
//     todo (positively verified NOT working, with an exact fix command when
//     applicable), unverifiable (could not be checked from here — sbx absent,
//     a probe dependency missing, a timeout), or denied (positively refused by
//     policy/permission — org policy, not a missing setup step).
//
// Only a POSITIVELY VERIFIED failure of a core requirement (verdict todo or
// denied) makes `pi-stack doctor` exit 1. Optional failures and anything
// unverifiable never block.

// requirement classifies how load-bearing a check is. The zero value ("") is
// treated as optional so an un-migrated group builder can never accidentally
// block the exit code.
type requirement string

const (
	// requirementCore is load-bearing: a verified failure here is the only
	// thing that makes doctor exit 1.
	requirementCore requirement = "core"
	// requirementOptional is an integration doctor surfaces but never blocks
	// on, whether or not the user opted into it.
	requirementOptional requirement = "optional"
)

// verdict classifies what a check's probe actually proved.
type verdict string

const (
	// verdictReady: verified working.
	verdictReady verdict = "ready"
	// verdictTodo: positively verified NOT working — a real, confirmed gap
	// with (when applicable) an exact copy-pasteable fix command.
	verdictTodo verdict = "todo"
	// verdictUnverifiable: could not be checked from here (sbx absent, e.g.
	// running inside the sandbox; a probe dependency missing; a timeout or
	// transport failure). Doctor does not KNOW, so it must neither claim ✗
	// nor surface a repair command.
	verdictUnverifiable verdict = "unverifiable"
	// verdictDenied: positively refused by policy/permission (an explicit org
	// policy denial, not a missing credential). Blocks like a verified todo
	// when the requirement is core, but the fix is organizational, not a
	// setup command.
	verdictDenied verdict = "denied"
)

// checkState is the rendered presentation class of a check, DERIVED from its
// verdict (see check.state) — kept as a named type so render and the callers
// that only care about the glyph class (hoststate, onboard) stay readable.
type checkState int

const (
	stateOK   checkState = iota // verified ready
	stateTODO                   // verified todo/denied; carries an exact command when applicable
	stateInfo                   // informational annotation, no claim implied
	stateWarn                   // unverifiable: could not be checked from here
)

// check is one line in a doctor group.
type check struct {
	label  string
	detail string // short human note after the label
	todo   string // exact copy-pasteable command; surfaced only for a verified todo/denied
	// requirement: core | optional. Zero value reads as optional (fail-open on
	// the exit code, never a surprise block).
	requirement requirement
	// verdict: ready | todo | unverifiable | denied. The zero value reads as
	// unverifiable (fail-SAFE on presentation: an unset verdict can never
	// render a false green and can never block).
	verdict verdict
	// evidence is the concrete machine-readable proof string behind the
	// verdict (a probed command, a matched output token, a dialed port).
	// Empty falls back to detail so the JSON payload is never blank.
	evidence string
	// note marks a pure annotation line (transparency/context, e.g. "probing
	// the sbx-registered command: …"): it renders as · and makes no health
	// claim of its own, so it never counts toward any tally.
	note bool
}

// req returns the effective requirement: the zero value reads as optional.
func (c check) req() requirement {
	if c.requirement == requirementCore {
		return requirementCore
	}
	return requirementOptional
}

// result returns the effective verdict. A note is presentational only and
// reads as ready; an UNSET verdict on a non-note check reads as unverifiable —
// the fail-safe direction (never a false green, never a false block).
func (c check) result() verdict {
	if c.note {
		return verdictReady
	}
	switch c.verdict {
	case verdictReady, verdictTodo, verdictUnverifiable, verdictDenied:
		return c.verdict
	default:
		return verdictUnverifiable
	}
}

// evidenceString returns the machine-readable evidence, falling back to the
// human detail so JSON consumers always get something concrete.
func (c check) evidenceString() string {
	if strings.TrimSpace(c.evidence) != "" {
		return c.evidence
	}
	return c.detail
}

// state derives the rendered checkState from the structured axes: the verdict
// is AUTHORITATIVE for the glyph, so a glyph/verdict contradiction is
// impossible by construction.
func (c check) state() checkState {
	if c.note {
		return stateInfo
	}
	switch c.result() {
	case verdictReady:
		return stateOK
	case verdictTodo, verdictDenied:
		return stateTODO
	default: // verdictUnverifiable
		return stateWarn
	}
}

// blockingCheck is the single source of truth for whether a (requirement,
// verdict) pair should make `pi-stack doctor` exit 1: only a POSITIVELY
// VERIFIED failure (todo or denied) of a CORE requirement blocks. Optional
// anything, and any requirement that is merely unverifiable, is non-blocking.
func blockingCheck(req requirement, v verdict) bool {
	return req == requirementCore && (v == verdictTodo || v == verdictDenied)
}

// group is a titled cluster of checks in dependency order.
type group struct {
	title  string
	checks []check
}

// report is the full doctor result: an ordered set of groups. It knows how to
// tally its verdicts (for the headline + exit code) and render itself.
type report struct {
	groups    []group
	sbxAbsent bool     // sbx not on PATH — provider/mcp checks can't be verified here
	services  []string // configured SERVICES, for the footer
	mcp       []string // configured MCP, for the footer
}

// todos returns every outstanding TODO command across all groups, in order,
// with duplicate commands dropped (so e.g. a `pi-stack mcp register` that two
// groups both surface only appears once). Only a VERIFIED failure (verdict
// todo/denied) may surface a repair command: unverifiable checks never do,
// even if a constructor left a suggestion in the todo field. Dedup is
// normalized via todoDedupKey so two commands that differ only in a trailing
// parenthetical collapse. Order is preserved: the first occurrence's full
// string wins.
func (r *report) todos() []string {
	var out []string
	seen := map[string]bool{}
	for _, g := range r.groups {
		for _, c := range g.checks {
			if v := c.result(); v != verdictTodo && v != verdictDenied {
				continue
			}
			if c.todo == "" {
				continue
			}
			key := todoDedupKey(c.todo)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, c.todo)
		}
	}
	return out
}

// todoDedupKey normalizes a TODO for dedup so two commands that share the same
// leading command but differ only in a trailing parenthetical (e.g. `pi-stack
// secret set <ENV_VAR> op://vault/item/field` vs the same command with a
// trailing `  (creates …)`) collapse to one. It keys
// on the string up to the first `  (` separator, trimmed.
func todoDedupKey(todo string) string {
	if i := strings.Index(todo, "  ("); i >= 0 {
		return strings.TrimSpace(todo[:i])
	}
	return strings.TrimSpace(todo)
}

// blocking reports whether ANY check across the whole report is a verified
// core failure — the aggregate `pi-stack doctor` reads to decide its exit
// code (1 vs 0). Usage errors are handled separately by parseDoctorArgs and
// always exit 2 regardless of this.
func (r *report) blocking() bool {
	for _, g := range r.groups {
		for _, c := range g.checks {
			if blockingCheck(c.req(), c.result()) {
				return true
			}
		}
	}
	return false
}

// outstanding counts the verified failures (verdict todo/denied, notes
// excluded) across the report — the headline's ⚠ tally.
func (r *report) outstanding() int {
	n := 0
	for _, g := range r.groups {
		for _, c := range g.checks {
			if v := c.result(); !c.note && (v == verdictTodo || v == verdictDenied) {
				n++
			}
		}
	}
	return n
}

// unverifiableCount counts the checks whose verdict is unverifiable (notes
// excluded): they never block or count as outstanding, but the headline must
// not claim "all checks pass" over an unverified axis.
func (r *report) unverifiableCount() int {
	n := 0
	for _, g := range r.groups {
		for _, c := range g.checks {
			if !c.note && c.result() == verdictUnverifiable {
				n++
			}
		}
	}
	return n
}
