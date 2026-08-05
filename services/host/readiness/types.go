// Package readiness is COMPATIBILITY-ONLY. It is the pre-health readiness
// model (Requirement x Verdict, a lazy axis registry, four exit codes, two
// renderers), and nothing new may be written against it: `pix status` and
// `pix doctor` render pix/host/health now, and so must every surface that
// follows them.
//
// It is retained, and its deletion DEFERRED, because it still has real
// consumers this unit did not own. As of W5/U10c they are, exactly:
//
//	workflow/models       `pix models add`'s inference selection/verification
//	workflow/launch       the launch key gate and the host-state read
//	workflow/doctor       the LEAF helpers the others call (SecretCheck,
//	                      OllamaReadinessAxes); workflow/gworkspace was a
//	                      consumer too until W2/U02B retired the built-in
//	                      Google Workspace wizard and its gog probes
//	cmd/pix               `run`/`agent`/`models` use readiness/axis for model
//	                      RESOLUTION (ResolveSessionModel) and the sbx key
//	                      evidence probe — utilities that live here by history,
//	                      not because they are about readiness reporting
//
// The deletion condition is mechanical, not a judgement call: when that list
// is empty, this package goes. cmd/pix's TestReadinessConsumersOnlyShrink
// pins the list and lets it only shrink, so "we will delete it later" is a
// ratchet rather than a promise.
package readiness

import (
	"strings"
	"time"
)

// doctor's readiness model: every Check carries two structured axes plus a
// concrete evidence string, and everything downstream (glyphs, the headline,
// the TODO list, the JSON payload, the exit code) is DERIVED from them — never
// re-parsed out of a Check's human detail text.
//
//   - Requirement says how much the Check matters: core (pix cannot do
//     useful work without it) or optional (an integration that is nice to have).
//   - Verdict says what the probe actually PROVED: ready (verified working),
//     todo (positively verified NOT working, with an exact fix command when
//     applicable), unverifiable (could not be checked from here — sbx absent,
//     a probe dependency missing, a timeout), or denied (positively refused by
//     policy/permission — org policy, not a missing setup step).
//
// Only a POSITIVELY VERIFIED failure of a core Requirement (Verdict todo or
// denied) makes `pix doctor` exit 1. Optional failures and anything
// unverifiable never block.

// Requirement classifies how load-bearing a Check is. The zero value ("") is
// treated as optional so an un-migrated Group builder can never accidentally
// block the exit code.
type Requirement string

const (
	// RequirementCore is load-bearing: a verified failure here is the only
	// thing that makes doctor exit 1.
	RequirementCore Requirement = "core"
	// RequirementRequested is an optional axis PROMOTED to Blocking because
	// the user explicitly asked for it on THIS invocation (`--pull-models`,
	// `--google-workspace`, `--mcp X`). It blocks exactly like core, and only
	// for that invocation — stale optional config never blocks unrelated
	// repair. Promotion is applied in Build, never in flag handling.
	RequirementRequested Requirement = "requested"
	// RequirementOptional is an integration doctor surfaces but never blocks
	// on, whether or not the user opted into it.
	RequirementOptional Requirement = "optional"
)

// Verdict classifies what a Check's probe actually proved.
type Verdict string

const (
	// VerdictReady: verified working.
	VerdictReady Verdict = "ready"
	// VerdictTodo: positively verified NOT working — a real, confirmed gap
	// with (when applicable) an exact copy-pasteable fix command.
	VerdictTodo Verdict = "todo"
	// VerdictUnverifiable: could not be checked from here (sbx absent, e.g.
	// running inside the sandbox; a probe dependency missing; a timeout or
	// transport failure). Doctor does not KNOW, so it must neither claim ✗
	// nor surface a repair command.
	VerdictUnverifiable Verdict = "unverifiable"
	// VerdictDenied: positively refused by policy/permission (an explicit org
	// policy denial, not a missing credential). Blocks like a verified todo
	// when the Requirement is core, but the fix is organizational, not a
	// setup command.
	VerdictDenied Verdict = "denied"
)

// CheckState is the rendered presentation class of a Check, DERIVED from its
// Verdict (see Check.state) — kept as a named type so render and the callers
// that only care about the glyph class (hoststate, onboard) stay readable.
type CheckState int

const (
	StateOK   CheckState = iota // verified ready
	StateTODO                   // verified todo/denied; carries an exact command when applicable
	StateInfo                   // informational annotation, no claim implied
	StateWarn                   // unverifiable: could not be checked from here
)

// Check is one line in a doctor Group.
type Check struct {
	Label  string
	Detail string // short human note after the label
	Todo   string // exact copy-pasteable command; surfaced only for a verified todo/denied
	// Requirement: core | optional. Zero value reads as optional (fail-open on
	// the exit code, never a surprise block).
	Requirement Requirement
	// Verdict: ready | todo | unverifiable | denied. The zero value reads as
	// unverifiable (fail-SAFE on presentation: an unset Verdict can never
	// render a false green and can never block).
	Verdict Verdict
	// evidence is the concrete machine-readable proof string behind the
	// Verdict (a probed command, a matched output token, a dialed port).
	// Empty falls back to detail so the JSON payload is never blank.
	Evidence string
	// note marks a pure annotation line (transparency/context, e.g. "probing
	// the sbx-registered command: …"): it ALWAYS renders as · (see state())
	// and NEVER counts toward Outstanding/Blocking (Outstanding() and
	// BlockingCheck both exclude notes explicitly), regardless of its Verdict.
	// The Verdict field itself must still be TRUTHFUL — see result()'s doc
	// comment — so a JSON consumer reading Verdict=ready can trust it means
	// verified working, even on a note-only line.
	Note bool
	// axis is the machine-stable readiness axis this Check asserts about.
	// Stamped by Build from the builder it came from, so a renderer
	// keys off the axis rather than a Group title or a human label.
	Axis Axis
	// endpoint is the concrete address/URL the probe actually talked to (the
	// resolved Ollama endpoint, a service port). Rendered into JSON so a
	// reader can tell WHICH endpoint produced the Verdict.
	Endpoint string
	// duration is how long the probe took. Defaults to the owning builder's
	// wall time when a Check does not measure itself.
	Duration time.Duration
}

// req returns the effective Requirement: the zero value reads as optional.
func (c Check) Req() Requirement {
	switch c.Requirement {
	case RequirementCore, RequirementRequested:
		return c.Requirement
	default:
		return RequirementOptional
	}
}

// result returns the effective Verdict. It NEVER special-cases note: a
// note-only Check's constructor is required to set an EXPLICIT, truthful
// Verdict (ready for a confirmed positive fact, unverifiable for "cannot
// verify"/"not configured"/anything else — see providerInfoCheck and the
// other note builders) — result() must not silently override that with a
// blanket ready just because note is set (the bug this fixes: a note whose
// evidence said "cannot verify"/"not configured" still serialized Verdict=
// ready to JSON, breaking the invariant that ready means verified working).
// An UNSET Verdict (the zero value, on any Check, note or not) reads as
// unverifiable — the fail-safe direction (never a false green, never a false
// block). Note that Outstanding()/BlockingCheck() still exclude notes
// explicitly, so a note's Verdict — whatever it truthfully is — never counts
// toward either tally.
func (c Check) Result() Verdict {
	switch c.Verdict {
	case VerdictReady, VerdictTodo, VerdictUnverifiable, VerdictDenied:
		return c.Verdict
	default:
		return VerdictUnverifiable
	}
}

// AxisOf returns the readiness axis this Check asserts about. Checks built by
// a snapshot builder are stamped by Build; a Check built outside a
// snapshot has no axis and reports "".
func (c Check) AxisOf() Axis { return c.Axis }

// evidenceString returns the machine-readable evidence, falling back to the
// human detail so JSON consumers always get something concrete.
func (c Check) EvidenceString() string {
	if strings.TrimSpace(c.Evidence) != "" {
		return c.Evidence
	}
	return c.Detail
}

// state derives the rendered CheckState from the structured axes: the Verdict
// is AUTHORITATIVE for the glyph, so a glyph/Verdict contradiction is
// impossible by construction.
func (c Check) State() CheckState {
	if c.Note {
		return StateInfo
	}
	switch c.Result() {
	case VerdictReady:
		return StateOK
	case VerdictTodo, VerdictDenied:
		return StateTODO
	default: // VerdictUnverifiable
		return StateWarn
	}
}

// BlockingCheck is the single source of truth for whether a (Requirement,
// Verdict) pair should make `pix doctor` exit 1: only a POSITIVELY
// VERIFIED failure (todo or denied) of a CORE (or explicitly REQUESTED)
// Requirement blocks. Plain optional anything, and any Requirement that is
// merely unverifiable, is non-Blocking here — an unverifiable core axis is
// exit 3, derived by Snapshot.ExitCode.
func BlockingCheck(req Requirement, v Verdict) bool {
	return BlocksExit(req) && (v == VerdictTodo || v == VerdictDenied)
}

// Group is a titled cluster of checks in dependency order. axis names the
// readiness axis the Group reports on, so the Report can be projected onto a
// Snapshot without re-deriving anything from the human title.
type Group struct {
	Title  string
	Axis   Axis
	Checks []Check
}

// TodoDedupKey normalizes a TODO for dedup so two commands that share the same
// leading command but differ only in a trailing parenthetical (e.g. `pix
// secret set <ENV_VAR> op://vault/item/field` vs the same command with a
// trailing `  (creates …)`) collapse to one. It keys
// on the string up to the first `  (` separator, trimmed.
func TodoDedupKey(todo string) string {
	if i := strings.Index(todo, "  ("); i >= 0 {
		return strings.TrimSpace(todo[:i])
	}
	return strings.TrimSpace(todo)
}
