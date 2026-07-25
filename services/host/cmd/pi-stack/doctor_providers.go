package main

import "strings"

// secretCheck reports whether a provider secret is set. When sbx is
// unreachable (e.g. inside the sandbox) it emits a TODO rather than a false OK.
// Kept for hoststate.go/onboard.go's per-provider booleans (a DIFFERENT need
// from doctor's readiness axes below: they just want a plain ok/not-ok per
// key, not the core-vs-informational split doctor renders).
func secretCheck(label, key, sbxOut string, sbxOK bool) check {
	cmd := "sbx secret set -g " + key
	if !sbxOK {
		return check{label: label, verdict: verdictTodo, detail: "sbx unavailable here (set on the host)", todo: cmd}
	}
	if grepWord(sbxOut, key) {
		return check{label: label, verdict: verdictReady, detail: "set"}
	}
	return check{label: label, verdict: verdictTodo, detail: "not set", todo: cmd}
}

// providersGroup builds the provider-secrets cluster: the model/github keys
// injected proxy-side (never visible in the VM), read via `sbx secret ls`.
//
// pi-stack only needs ONE of anthropic/openai/google to launch a model, so the
// group leads with a SINGLE core check on that disjunction \u2014 the only line
// here that can make `pi-stack doctor` exit 1:
//   - at least one present                -> ready
//   - sbx reachable, POSITIVELY zero set  -> todo, with ONE copy-pasteable fix
//     command (the other providers are named in evidence, never in the
//     command itself)
//   - sbx absent / `sbx secret ls` failed -> unverifiable (never denied, never
//     blocking \u2014 doctor does not know, so it must not claim a failure)
//
// Everything below the core check is purely informational (note: true \u2014
// never blocks, never counts as outstanding or unverifiable): naming which
// INDIVIDUAL provider is set is a convenience, not a second requirement \u2014 a
// missing alternate is expected once one provider exists, and github merely
// authorizes git operations (not the model), so it is never itself
// outstanding.
func providersGroup(sbxOut string, sbxOK bool) group {
	g := group{title: "Providers / keys (proxy-injected, never in the VM)"}
	g.checks = append(g.checks, modelKeyCoreCheck(sbxOut, sbxOK))
	for _, p := range []string{"anthropic", "openai", "google", "github"} {
		g.checks = append(g.checks, providerInfoCheck(p, sbxOut, sbxOK))
	}
	return g
}

// modelKeyFixCmd is the ONE copy-pasteable command surfaced when doctor has
// POSITIVELY confirmed zero model-provider keys are set. It fixes any one
// provider (anthropic, chosen as the example); the other two are named in the
// core check's evidence, not repeated here as alternative commands.
const modelKeyFixCmd = "pi-stack secret set ANTHROPIC_API_KEY op://vault/item/field && pi-stack secret sync"

// modelKeyCoreCheck is the sole core launch-readiness check in this group: does
// pi-stack have AT LEAST ONE usable model-provider key. It reuses
// anyModelKeyInOutput \u2014 the exact same "what counts as present" definition
// sbxModelKeyState uses for `run`'s launch gate \u2014 so doctor and the launch
// gate can never disagree about what "a key is present" means.
func modelKeyCoreCheck(sbxOut string, sbxOK bool) check {
	names := strings.Join(modelProviders, "/")
	if !sbxOK {
		return check{
			label:       "model key",
			requirement: requirementCore,
			verdict:     verdictUnverifiable,
			detail:      "cannot verify (sbx unavailable here) \u2014 re-run `pi-stack doctor` on the host",
			evidence:    "sbx secret ls: unavailable",
		}
	}
	if anyModelKeyInOutput(sbxOut) {
		return check{
			label:       "model key",
			requirement: requirementCore,
			verdict:     verdictReady,
			detail:      "at least one of " + names + " is set",
			evidence:    "sbx secret ls: " + presentModelProviders(sbxOut) + " set",
		}
	}
	return check{
		label:       "model key",
		requirement: requirementCore,
		verdict:     verdictTodo,
		detail:      "none of " + names + " is set \u2014 pi-stack cannot launch a model",
		todo:        modelKeyFixCmd,
		evidence:    "sbx secret ls: none of " + strings.Join(modelProviders, ", ") + " present",
	}
}

// presentModelProviders lists which of modelProviders sbxOut shows as set, for
// the core check's evidence string (alternatives belong in evidence, never in
// the fix command).
func presentModelProviders(sbxOut string) string {
	var got []string
	for _, k := range modelProviders {
		if grepWord(sbxOut, k) {
			got = append(got, k)
		}
	}
	return strings.Join(got, ", ")
}

// providerInfoCheck is a per-provider INFORMATIONAL annotation (note: true \u2014
// see providersGroup's doc comment): ready/not-configured/unverifiable, purely
// for transparency. It never blocks and never counts toward outstanding, so an
// unset alternate provider (or an unset github, which is optional
// infrastructure that authorizes git operations, not the model) is never
// itself a gap once the core check above is satisfied.
func providerInfoCheck(key string, sbxOut string, sbxOK bool) check {
	if !sbxOK {
		return check{label: key, note: true, verdict: verdictUnverifiable, detail: "cannot verify (sbx unavailable here)"}
	}
	if grepWord(sbxOut, key) {
		return check{label: key, note: true, verdict: verdictReady, detail: "set"}
	}
	return check{label: key, note: true, verdict: verdictUnverifiable, detail: "not configured"}
}
