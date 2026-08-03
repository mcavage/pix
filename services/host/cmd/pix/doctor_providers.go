package main

import (
	"fmt"
	"pix/host/cli"
	"pix/host/readiness"
	"pix/host/readiness/axis"
	"sort"
	"strings"

	"pix/host/config"
	"pix/host/routing"
)

// secretCheck reports whether a provider secret is set. When sbx is
// unreachable (e.g. inside the sandbox) it emits a TODO rather than a false OK.
// Kept for hoststate.go/onboard.go's per-provider booleans (a DIFFERENT need
// from doctor's readiness axes below: they just want a plain ok/not-ok per
// key, not the core-vs-informational split doctor renders).
func secretCheck(label, key, sbxOut string, sbxOK bool) readiness.Check {
	cmd := "sbx secret set -g " + key
	if !sbxOK {
		return readiness.Check{Label: label, Verdict: readiness.VerdictTodo, Detail: "sbx unavailable here (set on the host)", Todo: cmd}
	}
	if cli.GrepWord(sbxOut, key) {
		return readiness.Check{Label: label, Verdict: readiness.VerdictReady, Detail: "set"}
	}
	return readiness.Check{Label: label, Verdict: readiness.VerdictTodo, Detail: "not set", Todo: cmd}
}

// providersGroup builds the provider-secrets cluster: the model/github keys
// injected proxy-side (never visible in the VM), read via `sbx secret ls`.
//
// pix only needs ONE of anthropic/openai/google to launch a model, so the
// group leads with a SINGLE core check on that disjunction \u2014 the only line
// here that can make `pix doctor` exit 1:
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
func providersGroup(cfg *config.Config, sbxOut string, sbxOK bool) readiness.Group {
	g := readiness.Group{Title: "Inference / credentials (proxy-injected, never in the VM)"}
	g.Checks = append(g.Checks, axis.InferenceCoreCheck(cfg, sbxOut, sbxOK))
	if inferenceNeedsOnePassword(cfg) {
		for _, p := range []string{"anthropic", "openai", "google", "github"} {
			g.Checks = append(g.Checks, providerInfoCheck(p, sbxOut, sbxOK))
		}
	}
	if legacy := legacyVerifiedOllamaBindings(cfg); len(legacy) > 0 {
		// A pre-upgrade config asserted Verified from a LISTING. Grandfathered as
		// callable (demoting at load would empty the runtime on a working local box
		// and refuse a launch on a bookkeeping change, not on evidence), but said
		// out loud once. The row clears on the next setup either way: a re-probe
		// promotes with provenance, or demotes and the candidate row takes over.
		g.Checks = append(g.Checks, readiness.Check{Label: "inference", Note: true, Verdict: readiness.VerdictTodo,
			Detail:   fmt.Sprintf("%d ollama binding(s) were marked verified by a listing, not a request — re-verify", len(legacy)),
			Todo:     "pix setup",
			Evidence: "verified without verified_by=probe: " + strings.Join(legacy, ", ")})
	}
	if gap := inferenceBindingGapCheck(cfg); gap != nil {
		g.Checks = append(g.Checks, *gap)
	}
	g.Checks = append(g.Checks, axis.RunIntentKeyCheck(cfg, sbxOut, sbxOK))
	return g
}

// inferenceBindingGapCheck reports a provider key that resolves on this host
// but is wired to no models — present, correct, and doing nothing.
//
// That state used to be unreachable except through setup, and permanent once
// entered: `pix secret set` wrote the ref and stopped, so the key never became
// bindings. `pix models add` fixes it, and this is the row that tells a user
// the gap exists at all, since nothing else about the host looks wrong.
//
// Optional, never blocking: one wired provider is enough to launch, so a second
// unwired key is a shortfall to report, not a reason to fail. It counts in
// outstanding (note:false) because it is genuinely actionable.
func inferenceBindingGapCheck(cfg *config.Config) *readiness.Check {
	gaps := unwiredProviderKeys(cfg, defaultShellEnv())
	if len(gaps) == 0 {
		return nil
	}
	list := strings.Join(gaps, ", ")
	return &readiness.Check{
		Label:       "inference bindings",
		Requirement: readiness.RequirementOptional,
		Verdict:     readiness.VerdictTodo,
		Detail:      list + ": key set but wired to no models",
		Evidence:    "hostmode.env carries " + list + "; config.inference.models has no native binding for " + list,
		Todo:        "pix models add " + gaps[0],
	}
}

// ollamaCloudCandidates returns bound non-local ollama models: the set whose
// entitlement only a real call can establish.
func ollamaCloudCandidates(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	reg, err := routing.LoadRegistry()
	if err != nil {
		return nil
	}
	var out []string
	for _, b := range cfg.Inference.Models {
		if !b.Available || b.Source != "" || !axis.OllamaBindingDriver(cfg, b) {
			continue
		}
		if m, ok := reg.Get(b.Model); ok && !m.Local {
			out = append(out, b.Model)
		}
	}
	sort.Strings(out)
	return out
}

// legacyVerifiedOllamaBindings returns bindings that claim Verified without
// naming a probe. Only a PRE-UPGRADE config can produce this shape: everything
// this codebase promotes writes VerifiedBy="probe" in the same assignment, and
// everything it demotes clears both. So the row fires exactly once and clears
// on the next setup whether that setup promotes or demotes the binding.
func legacyVerifiedOllamaBindings(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	var out []string
	for _, b := range cfg.Inference.Models {
		if b.Verified && b.VerifiedBy != config.VerifiedByProbe && b.Source == "" && axis.OllamaBindingDriver(cfg, b) {
			out = append(out, b.Model)
		}
	}
	sort.Strings(out)
	return out
}

// providerInfoCheck is a per-provider INFORMATIONAL annotation (note: true \u2014
// see providersGroup's doc comment): ready/not-configured/unverifiable, purely
// for transparency. It never blocks and never counts toward outstanding, so an
// unset alternate provider (or an unset github, which is optional
// infrastructure that authorizes git operations, not the model) is never
// itself a gap once the core check above is satisfied.
func providerInfoCheck(key string, sbxOut string, sbxOK bool) readiness.Check {
	if !sbxOK {
		return readiness.Check{Label: key, Note: true, Verdict: readiness.VerdictUnverifiable, Detail: "cannot verify (sbx unavailable here)"}
	}
	if cli.GrepWord(sbxOut, key) {
		return readiness.Check{Label: key, Note: true, Verdict: readiness.VerdictReady, Detail: "set"}
	}
	return readiness.Check{Label: key, Note: true, Verdict: readiness.VerdictUnverifiable, Detail: "not configured"}
}
