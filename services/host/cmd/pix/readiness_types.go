package main

import (
	"strings"
	"time"
)

// doctor's readiness model: every check carries two structured axes plus a
// concrete evidence string, and everything downstream (glyphs, the headline,
// the TODO list, the JSON payload, the exit code) is DERIVED from them — never
// re-parsed out of a check's human detail text.
//
//   - requirement says how much the check matters: core (pix cannot do
//     useful work without it) or optional (an integration that is nice to have).
//   - verdict says what the probe actually PROVED: ready (verified working),
//     todo (positively verified NOT working, with an exact fix command when
//     applicable), unverifiable (could not be checked from here — sbx absent,
//     a probe dependency missing, a timeout), or denied (positively refused by
//     policy/permission — org policy, not a missing setup step).
//
// Only a POSITIVELY VERIFIED failure of a core requirement (verdict todo or
// denied) makes `pix doctor` exit 1. Optional failures and anything
// unverifiable never block.

// requirement classifies how load-bearing a check is. The zero value ("") is
// treated as optional so an un-migrated group builder can never accidentally
// block the exit code.
type requirement string

const (
	// requirementCore is load-bearing: a verified failure here is the only
	// thing that makes doctor exit 1.
	requirementCore requirement = "core"
	// requirementRequested is an optional axis PROMOTED to blocking because
	// the user explicitly asked for it on THIS invocation (`--pull-models`,
	// `--google-workspace`, `--mcp X`). It blocks exactly like core, and only
	// for that invocation — stale optional config never blocks unrelated
	// repair. Promotion is applied in buildSnapshot, never in flag handling.
	requirementRequested requirement = "requested"
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
	// the sbx-registered command: …"): it ALWAYS renders as · (see state())
	// and NEVER counts toward outstanding/blocking (outstanding() and
	// blockingCheck both exclude notes explicitly), regardless of its verdict.
	// The verdict field itself must still be TRUTHFUL — see result()'s doc
	// comment — so a JSON consumer reading verdict=ready can trust it means
	// verified working, even on a note-only line.
	note bool
	// axis is the machine-stable readiness axis this check asserts about.
	// Stamped by buildSnapshot from the builder it came from, so a renderer
	// keys off the axis rather than a group title or a human label.
	axis Axis
	// endpoint is the concrete address/URL the probe actually talked to (the
	// resolved Ollama endpoint, a service port). Rendered into JSON so a
	// reader can tell WHICH endpoint produced the verdict.
	endpoint string
	// duration is how long the probe took. Defaults to the owning builder's
	// wall time when a check does not measure itself.
	duration time.Duration
}

// req returns the effective requirement: the zero value reads as optional.
func (c check) req() requirement {
	switch c.requirement {
	case requirementCore, requirementRequested:
		return c.requirement
	default:
		return requirementOptional
	}
}

// result returns the effective verdict. It NEVER special-cases note: a
// note-only check's constructor is required to set an EXPLICIT, truthful
// verdict (ready for a confirmed positive fact, unverifiable for "cannot
// verify"/"not configured"/anything else — see providerInfoCheck and the
// other note builders) — result() must not silently override that with a
// blanket ready just because note is set (the bug this fixes: a note whose
// evidence said "cannot verify"/"not configured" still serialized verdict=
// ready to JSON, breaking the invariant that ready means verified working).
// An UNSET verdict (the zero value, on any check, note or not) reads as
// unverifiable — the fail-safe direction (never a false green, never a false
// block). Note that outstanding()/blockingCheck() still exclude notes
// explicitly, so a note's verdict — whatever it truthfully is — never counts
// toward either tally.
func (c check) result() verdict {
	switch c.verdict {
	case verdictReady, verdictTodo, verdictUnverifiable, verdictDenied:
		return c.verdict
	default:
		return verdictUnverifiable
	}
}

// axisOf returns the readiness axis this check asserts about. Checks built by
// a snapshot builder are stamped by buildSnapshot; a check built outside a
// snapshot has no axis and reports "".
func (c check) axisOf() Axis { return c.axis }

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
// verdict) pair should make `pix doctor` exit 1: only a POSITIVELY
// VERIFIED failure (todo or denied) of a CORE (or explicitly REQUESTED)
// requirement blocks. Plain optional anything, and any requirement that is
// merely unverifiable, is non-blocking here — an unverifiable core axis is
// exit 3, derived by Snapshot.ExitCode.
func blockingCheck(req requirement, v verdict) bool {
	return blocksExit(req) && (v == verdictTodo || v == verdictDenied)
}

// group is a titled cluster of checks in dependency order. axis names the
// readiness axis the group reports on, so the report can be projected onto a
// Snapshot without re-deriving anything from the human title.
type group struct {
	title  string
	axis   Axis
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
// with duplicate commands dropped (so e.g. a `pix mcp register` that two
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
// leading command but differ only in a trailing parenthetical (e.g. `pix
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
// core failure — the aggregate `pix doctor` reads to decide its exit
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
