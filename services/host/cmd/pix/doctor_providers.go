package main

import (
	"fmt"
	"pix/host/readiness"
	"sort"
	"strings"

	"pix/host/config"
	"pix/host/inference"
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
	if grepWord(sbxOut, key) {
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
	g.Checks = append(g.Checks, inferenceCoreCheck(cfg, sbxOut, sbxOK))
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
	g.Checks = append(g.Checks, runIntentKeyCheck(cfg, sbxOut, sbxOK))
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

// runIntentKeyCheck warns when the top-level session intent (config.run_intent,
// the "overlord") resolves to a provider whose key is NOT set. This is the
// specific trap of the baked overlord -> GPT-5.6 Sol default: a host with only an
// Anthropic key launches fine (the core check is green) but every INTERACTIVE
// turn 401s because the session model is OpenAI. It is INFORMATIONAL (note: true
// — never blocks, never counts as outstanding): the fix is a config change, not
// a missing requirement, and the core "at least one key" gate already stands.
func runIntentKeyCheck(cfg *config.Config, sbxOut string, sbxOK bool) readiness.Check {
	intent := config.DefaultRunIntent
	if cfg != nil && strings.TrimSpace(cfg.RunIntent) != "" {
		intent = strings.TrimSpace(cfg.RunIntent)
	}
	label := "session model (run_intent=" + intent + ")"
	// "none"/"off" is the explicit opt-out (run.go): pi picks its own default model,
	// which needs no specific provider key beyond the core "at least one" gate.
	if strings.EqualFold(intent, "none") || strings.EqualFold(intent, "off") {
		return readiness.Check{Label: label, Note: true, Verdict: readiness.VerdictReady, Detail: "opt-out: pi's own default model"}
	}
	model, err := resolveSessionModel(intent)
	if err != nil || model == "" {
		// A bad run_intent degrades to pi's own default at launch (run.go), so this
		// is a soft note, not a failure.
		return readiness.Check{Label: label, Note: true, Verdict: readiness.VerdictUnverifiable,
			Detail: "run_intent does not resolve to a model — launch will use pi's default; fix with `pix config set run_intent <intent>`"}
	}
	if b, ok := configuredBindingForModel(cfg, model); ok {
		runtimeID := inference.RuntimeID(routing.Binding{Model: b.Model, Backend: b.Backend, UpstreamID: b.Upstream, Available: b.Available})
		return readiness.Check{Label: label, Note: true, Verdict: readiness.VerdictReady,
			Detail: "-> " + runtimeID + " via inference backend " + b.Backend}
	}
	// The intent's model IS bound here — it just has not answered a request. The
	// fix is a pull, not somebody else's cloud key.
	if containsStr(unverifiedOllamaCandidates(cfg), model) {
		return readiness.Check{Label: label, Note: true, Verdict: readiness.VerdictTodo,
			Detail: "-> " + model + " is bound but has not passed a probe (not pulled, or the probe failed)",
			Todo:   pullModelsFixCmd}
	}
	provider := model
	if i := strings.IndexByte(model, '/'); i > 0 {
		provider = model[:i]
	}
	if !sbxOK {
		return readiness.Check{Label: label, Note: true, Verdict: readiness.VerdictUnverifiable,
			Detail: "-> " + model + " (cannot verify " + provider + " key: sbx unavailable here)"}
	}
	// Only the model providers carry a launch-relevant key here; a local (ollama)
	// model needs none.
	if provider == "ollama" || grepWord(sbxOut, provider) {
		return readiness.Check{Label: label, Note: true, Verdict: readiness.VerdictReady, Detail: "-> " + model + " (" + provider + " key set)"}
	}
	// If NO model key is set at all, the core "at least one key" check already owns
	// the fix — don't double up a second secret-set todo here. This check earns its
	// keep in the SPECIFIC trap: you HAVE a key, just not the session model's
	// provider (e.g. Anthropic-only host + baked overlord -> OpenAI).
	if !anyModelKeyInOutput(sbxOut) {
		return readiness.Check{Label: label, Note: true, Verdict: readiness.VerdictUnverifiable,
			Detail: "-> " + model + " (needs a " + provider + " key; set a model key first — see the core check above)"}
	}
	return readiness.Check{Label: label, Note: true, Verdict: readiness.VerdictTodo,
		Detail: "-> " + model + " but the " + provider + " key is NOT set: interactive turns will fail. Set " + provider + "'s key, or point run_intent at a provider you have (or `none` for pi's default)",
		// `models add` rather than `secret set`: the latter stores the ref and stops,
		// which leaves this check failing for the same reason after the user has
		// done exactly what it told them to.
		Todo: "pix models add " + provider}
}

// modelKeyFixCmd is the ONE copy-pasteable command surfaced when doctor has
// POSITIVELY confirmed zero model-provider keys are set. It fixes any one
// provider (anthropic, chosen as the example); the other two are named in the
// core check's evidence, not repeated here as alternative commands.
const modelKeyFixCmd = "pix models add anthropic"

// pullModelsFixCmd is the ONE copy-pasteable command for the state honest
// Ollama verification creates: candidates are bound, none has passed a probe,
// and the reason is almost always "the weights are not on disk". It is NOT a
// provider-key fix, and remediating this state with one (which is what falling
// through to modelKeyCoreCheck does) tells a pure-Ollama user to go buy an
// Anthropic key to fix a download they declined.
const pullModelsFixCmd = "pix setup --pull-models"

// ollamaBindingDriver reports whether a binding runs through an ollama backend.
func ollamaBindingDriver(cfg *config.Config, b config.InferenceModelBinding) bool {
	backend, ok := cfg.Inference.Backends[b.Backend]
	return ok && backend.Driver == "ollama"
}

// unverifiedOllamaCandidates returns bound-but-unproven ollama bindings — the
// declined-pull state. Non-empty means the fix is a pull, never a key.
func unverifiedOllamaCandidates(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	var out []string
	for _, b := range cfg.Inference.Models {
		if !b.Available || b.Verified || b.Source != "" || !ollamaBindingDriver(cfg, b) {
			continue
		}
		if !inference.Allowed(cfg, b) {
			continue
		}
		out = append(out, b.Model)
	}
	sort.Strings(out)
	return out
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
		if !b.Available || b.Source != "" || !ollamaBindingDriver(cfg, b) {
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
		if b.Verified && b.VerifiedBy != config.VerifiedByProbe && b.Source == "" && ollamaBindingDriver(cfg, b) {
			out = append(out, b.Model)
		}
	}
	sort.Strings(out)
	return out
}

func configuredBindingForModel(cfg *config.Config, model string) (config.InferenceModelBinding, bool) {
	if cfg == nil {
		return config.InferenceModelBinding{}, false
	}
	for _, b := range cfg.Inference.Models {
		runtimeID := inference.RuntimeID(routing.Binding{Model: b.Model, Backend: b.Backend, UpstreamID: b.Upstream, Available: b.Available})
		if (b.Model == model || runtimeID == model) && inference.Callable(cfg, b) {
			return b, true
		}
	}
	return config.InferenceModelBinding{}, false
}

// inferenceCoreCheck makes readiness topology-aware. Direct-provider hosts
// retain the established sbx-secret evidence; gateway and Ollama hosts earn
// readiness from their availability-specific bindings instead of being told
// to configure an unrelated cloud-provider key.
func inferenceCoreCheck(cfg *config.Config, sbxOut string, sbxOK bool) readiness.Check {
	count, _ := configuredInferenceSummary(cfg)
	if count > 0 {
		return readiness.Check{Label: "inference", Requirement: readiness.RequirementCore, Verdict: readiness.VerdictReady,
			Detail:   fmt.Sprintf("%d configured callable model(s)", count),
			Evidence: "availability-specific inference bindings"}
	}
	// Nothing callable, but ollama candidates ARE bound: this host does not need
	// a provider key, it needs weights. Falling through to modelKeyCoreCheck here
	// would remediate a not-pulled-a-model problem with `pix secret set
	// ANTHROPIC_API_KEY`, which is the wrong command for the wrong product.
	if pending := unverifiedOllamaCandidates(cfg); len(pending) > 0 {
		return readiness.Check{Label: "inference", Requirement: readiness.RequirementCore, Verdict: readiness.VerdictTodo,
			Detail:   fmt.Sprintf("%d local model candidate(s) bound but unproven (not pulled, or the probe failed)", len(pending)),
			Todo:     pullModelsFixCmd,
			Evidence: "ollama bindings without a probe: " + strings.Join(pending, ", ")}
	}
	return modelKeyCoreCheck(sbxOut, sbxOK)
}

func configuredInferenceSummary(cfg *config.Config) (int, []string) {
	if cfg == nil {
		return 0, nil
	}
	seen := map[string]bool{}
	var backends []string
	count := 0
	for _, b := range cfg.Inference.Models {
		if !inference.Callable(cfg, b) {
			continue
		}
		count++
		if !seen[b.Backend] {
			seen[b.Backend] = true
			backends = append(backends, b.Backend)
		}
	}
	return count, backends
}

// modelKeyCoreCheck is the sole core launch-readiness check in this group: does
// pix have AT LEAST ONE usable model-provider key. It reuses
// anyModelKeyInOutput \u2014 the exact same "what counts as present" definition
// sbxModelKeyState uses for `run`'s launch gate \u2014 so doctor and the launch
// gate can never disagree about what "a key is present" means.
func modelKeyCoreCheck(sbxOut string, sbxOK bool) readiness.Check {
	names := strings.Join(modelProviders, "/")
	if !sbxOK {
		return readiness.Check{
			Label:       "model key",
			Requirement: readiness.RequirementCore,
			Verdict:     readiness.VerdictUnverifiable,
			Detail:      "cannot verify (sbx unavailable here) \u2014 re-run `pix doctor` on the host",
			Evidence:    "sbx secret ls: unavailable",
		}
	}
	if anyModelKeyInOutput(sbxOut) {
		return readiness.Check{
			Label:       "model key",
			Requirement: readiness.RequirementCore,
			Verdict:     readiness.VerdictReady,
			Detail:      "at least one of " + names + " is set",
			Evidence:    "sbx secret ls: " + presentModelProviders(sbxOut) + " set",
		}
	}
	return readiness.Check{
		Label:       "model key",
		Requirement: readiness.RequirementCore,
		Verdict:     readiness.VerdictTodo,
		Detail:      "none of " + names + " is set \u2014 pix cannot launch a model",
		Todo:        modelKeyFixCmd,
		Evidence:    "sbx secret ls: none of " + strings.Join(modelProviders, ", ") + " present",
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
func providerInfoCheck(key string, sbxOut string, sbxOK bool) readiness.Check {
	if !sbxOK {
		return readiness.Check{Label: key, Note: true, Verdict: readiness.VerdictUnverifiable, Detail: "cannot verify (sbx unavailable here)"}
	}
	if grepWord(sbxOut, key) {
		return readiness.Check{Label: key, Note: true, Verdict: readiness.VerdictReady, Detail: "set"}
	}
	return readiness.Check{Label: key, Note: true, Verdict: readiness.VerdictUnverifiable, Detail: "not configured"}
}
