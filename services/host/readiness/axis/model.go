package axis

import (
	"fmt"
	"pix/host/cli"
	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/inference"
	"pix/host/readiness"
	"pix/host/routing"
	"pix/host/secret"
	"slices"
	"sort"
	"strings"
)

// modelgo is the SHARED, presentation-free local-model readiness
// vocabulary. `pix doctor` (doctor_ollama.go) derives every per-model
// (watcher/embed/bridge) check from ONE OllamaProbe + ModelReadiness, and a
// future `pix setup` receipt (S08) consumes the SAME seam — so the two
// commands can never disagree about what is pulled.

// OllamaProbe is the single snapshot of the local Ollama installation: is the
// binary on PATH, does its daemon answer on :11434, and (only meaningful when
// installed) the `ollama list` output. Building this once and passing it to
// every ModelReadiness call is what makes "probe Ollama once" true — no
// caller re-execs `ollama list` per model.
type OllamaProbe struct {
	Installed bool
	DaemonUp  bool
	ListOut   string
	ListOK    bool
	// endpoint is the resolved Ollama endpoint the dial actually targeted, so
	// every derived check can name it (EffectiveOllamaEndpoint is the only
	// place that resolution happens).
	endpoint ollamaEndpoint
}

// ProbeOllamaAt runs lookPath, a daemon dial at the RESOLVED endpoint, and
// `ollama list` — daemon
// dial and list are skipped entirely when ollama isn't even on PATH, so a
// host with no Ollama pays for exactly one failed lookPath call. The
// `ollama list` exec is BOUNDED (probeRun: hard timeout + output cap), so a
// wedged ollama classifies as list-unverified rather than hanging the caller.
func ProbeOllamaAt(env hostenv.Env, ep ollamaEndpoint) OllamaProbe {

	if _, err := env.LookPath("ollama"); err != nil {
		return OllamaProbe{}
	}
	p := OllamaProbe{Installed: true, endpoint: ep}
	p.DaemonUp = env.DialLocal(ep.Port)
	if out, timedOut, err := env.RunTimed("ollama", "list"); err == nil && !timedOut {
		p.ListOut, p.ListOK = out, true
	}
	return p
}

// ModelReadiness is the pure, presentation-free readiness of ONE configured
// Ollama model tag, evaluated against a shared OllamaProbe. It carries the
// same requirement/verdict axes every other doctor check does
// (doctor_go) so a caller never has to invent a parallel vocabulary
// for local models. Installed=false means Ollama itself isn't on PATH: the
// model is NOT CONFIGURED (an expected absence, a note) rather than any
// verdict about the tag — and it must never enter the missing OR unverifiable
// sets below.
type ModelReadiness struct {
	Role        string // "watcher" | "embed" | "bridge"
	Model       string // configured tag, e.g. "qwen3.5:9b"
	Purpose     string // short human purpose, e.g. "fact capture (memory watcher)"
	Requirement readiness.Requirement
	Verdict     readiness.Verdict // meaningful only when Installed
	Installed   bool              // ollama on PATH; false => not-configured, no verdict claimed
	PullCmd     string            // "ollama pull <tag>" — always populated, even when healthy
}

// EvalModel evaluates one (role, model) pair against p. req is supplied
// by the CALLER — doctor's Ollama group weighs it by whether configured roles
// (the memory service) actually depend on local models; a setup receipt may
// legitimately pass a different requirement for the same model.
func EvalModel(role, model, purpose string, p OllamaProbe, req readiness.Requirement) ModelReadiness {
	m := ModelReadiness{
		Role: role, Model: model, Purpose: purpose,
		Requirement: req, Installed: p.Installed,
		PullCmd: "ollama pull " + model,
	}
	switch {
	case !p.Installed:
		// Not configured: no verdict is claimed. Leave the zero verdict, which
		// the framework reads fail-safe (unverifiable) if anyone consults it —
		// but Installed=false is the authoritative signal.
	case model != "" && !IsValidOllamaTag(model):
		m.Verdict = readiness.VerdictUnverifiable
	case p.ListOK && ModelPulled(p.ListOut, model):
		m.Verdict = readiness.VerdictReady
	case p.ListOK:
		// `ollama list` ran fine and simply does not list this tag — a
		// CONFIRMED gap, not a guess.
		m.Verdict = readiness.VerdictTodo
	default:
		// ollama is on PATH but `ollama list` itself did not succeed (e.g. the
		// daemon isn't reachable) — this could not be verified one way or the
		// other, which is different from a confirmed "not pulled".
		m.Verdict = readiness.VerdictUnverifiable
	}
	return m
}

// MissingModel is one Ollama tag, plus every role that depends on it (e.g.
// qwen3.5:9b is watcher+bridge by default). Used both for CONFIRMED-missing
// tags (ComputeMissingModels) and for unverifiable tags
// (ComputeUnverifiableModels) — same (tag, roles) shape, two disjoint sets.
type MissingModel struct {
	Tag   string
	Roles []string
}

// FilterModelsByVerdict reduces readinesses to the distinct INSTALLED tags
// whose verdict satisfies match, deduping identical tags across roles so a
// shared model is named once, with every dependent role listed, in first-seen
// order. Not-installed (not-configured) entries never match anything. Shared
// by ComputeMissingModels and ComputeUnverifiableModels so the two never
// drift into different dedup/order behavior.
func FilterModelsByVerdict(readinesses []ModelReadiness, match func(readiness.Verdict) bool) []MissingModel {
	var out []MissingModel
	index := make(map[string]int, len(readinesses))
	for _, m := range readinesses {
		if m.Model == "" || !m.Installed || !match(m.Verdict) {
			continue
		}
		if i, ok := index[m.Model]; ok {
			out[i].Roles = append(out[i].Roles, m.Role)
			continue
		}
		index[m.Model] = len(out)
		out = append(out, MissingModel{Tag: m.Model, Roles: []string{m.Role}})
	}
	return out
}

// ComputeMissingModels reduces a set of ModelReadiness to the distinct tags
// that are CONFIRMED missing (readiness.VerdictTodo only — `ollama list` ran fine and
// simply does not list the tag). readiness.VerdictUnverifiable must NEVER enter this
// set — a stopped daemon or a failed `ollama list` proves nothing about
// whether the tag is actually pulled, so treating it as "missing" would
// contradict the evidence and could force-repull an already-installed model.
// See ComputeUnverifiableModels for the disjoint unverifiable set. Nothing
// here pulls anything: callers only surface the PullCmd for the user to act on.
func ComputeMissingModels(readinesses []ModelReadiness) []MissingModel {
	return FilterModelsByVerdict(readinesses, func(v readiness.Verdict) bool { return v == readiness.VerdictTodo })
}

// ComputeUnverifiableModels reduces a set of ModelReadiness to the distinct
// tags that could not be verified one way or the other (ollama installed but
// the daemon was down or `ollama list` itself failed). These are reported
// separately from ComputeMissingModels: never offered for pull, never called
// "missing" — only receipted with an accurate diagnostic of why they
// couldn't be checked.
func ComputeUnverifiableModels(readinesses []ModelReadiness) []MissingModel {
	return FilterModelsByVerdict(readinesses, func(v readiness.Verdict) bool { return v == readiness.VerdictUnverifiable })
}

// OllamaVerifyFailureReason names WHY a model tag could not be verified, so a
// receipt can distinguish a down daemon from a daemon that's up but whose
// `ollama list` call itself failed.
func OllamaVerifyFailureReason(p OllamaProbe) string {
	if !p.DaemonUp {
		return "the Ollama daemon is not answering at " + p.endpoint.String()
	}
	return "`ollama list` did not succeed"
}

// modelCheck renders one ModelReadiness as a doctor check line.
func modelCheck(m ModelReadiness) readiness.Check {
	label := "  " + m.Role
	detail := m.Purpose + " [" + m.Model + "]"
	if strings.TrimSpace(m.Model) == "" {
		// No tag configured for this role at all (e.g. the bridge model before
		// any `pix run` has written one) — an expected absence, never a
		// confirmed-missing todo. ModelReadiness still computes a PullCmd of
		// "ollama pull " for an empty tag; this branch is what keeps that
		// meaningless command from ever reaching a renderer.
		return readiness.Check{Label: label, Note: true, Verdict: readiness.VerdictUnverifiable,
			Detail: m.Purpose + " — no model configured for this role"}
	}
	if !m.Installed {
		// Not configured: ollama itself is absent, so no claim about the tag.
		return readiness.Check{Label: label, Note: true, Verdict: readiness.VerdictUnverifiable,
			Detail: detail + " — needs ollama (then: " + m.PullCmd + ")"}
	}
	switch m.Verdict {
	case readiness.VerdictReady:
		return readiness.Check{Label: label, Verdict: readiness.VerdictReady,
			Detail:   "pulled — " + detail,
			Evidence: "`ollama list` includes " + m.Model}
	case readiness.VerdictTodo:
		return readiness.Check{Label: label, Verdict: readiness.VerdictTodo,
			Detail:   detail + " — not pulled",
			Evidence: "`ollama list` ran cleanly and does not include " + m.Model,
			Todo:     m.PullCmd}
	default: // readiness.VerdictUnverifiable
		return readiness.Check{Label: label, Verdict: readiness.VerdictUnverifiable,
			Detail:   detail + " — could not verify (`ollama list` unavailable)",
			Evidence: "`ollama list` did not succeed"}
	}
}

// ModelPulled reports whether `ollama list` output lists the given model. The
// first column may carry a :tag suffix (e.g. "gemma4:latest").
func ModelPulled(listOut, model string) bool {
	for _, line := range strings.Split(listOut, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		name := fields[0]
		if name == model || strings.HasPrefix(name, model+":") {
			return true
		}
	}
	return false
}

// RunIntentKeyCheck warns when the top-level session intent (config.run_intent,
// the "overlord") resolves to a provider whose key is NOT set. This is the
// specific trap of the baked overlord -> GPT-5.6 Sol default: a host with only an
// Anthropic key launches fine (the core check is green) but every INTERACTIVE
// turn 401s because the session model is OpenAI. It is INFORMATIONAL (note: true
// — never blocks, never counts as outstanding): the fix is a config change, not
// a missing requirement, and the core "at least one key" gate already stands.
func RunIntentKeyCheck(cfg *config.Config, sbxOut string, sbxOK bool) readiness.Check {
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
	model, err := ResolveSessionModel(intent)
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
	if slices.Contains(UnverifiedOllamaCandidates(cfg), model) {
		return readiness.Check{Label: label, Note: true, Verdict: readiness.VerdictTodo,
			Detail: "-> " + model + " is bound but has not passed a probe (not pulled, or the probe failed)",
			Todo:   PullModelsFixCmd}
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
	if provider == "ollama" || cli.GrepWord(sbxOut, provider) {
		return readiness.Check{Label: label, Note: true, Verdict: readiness.VerdictReady, Detail: "-> " + model + " (" + provider + " key set)"}
	}
	// If NO model key is set at all, the core "at least one key" check already owns
	// the fix — don't double up a second secret-set todo here. This check earns its
	// keep in the SPECIFIC trap: you HAVE a key, just not the session model's
	// provider (e.g. Anthropic-only host + baked overlord -> OpenAI).
	if !AnyModelKeyInOutput(sbxOut) {
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

// InferenceCoreCheck makes readiness topology-aware. Direct-provider hosts
// retain the established sbx-secret evidence; gateway and Ollama hosts earn
// readiness from their availability-specific bindings instead of being told
// to configure an unrelated cloud-provider key.
func InferenceCoreCheck(cfg *config.Config, sbxOut string, sbxOK bool) readiness.Check {
	count, _ := ConfiguredInferenceSummary(cfg)
	if count > 0 {
		return readiness.Check{Label: "inference", Requirement: readiness.RequirementCore, Verdict: readiness.VerdictReady,
			Detail:   fmt.Sprintf("%d configured callable model(s)", count),
			Evidence: "availability-specific inference bindings"}
	}
	// Nothing callable, but ollama candidates ARE bound: this host does not need
	// a provider key, it needs weights. Falling through to modelKeyCoreCheck here
	// would remediate a not-pulled-a-model problem with `pix secret set
	// ANTHROPIC_API_KEY`, which is the wrong command for the wrong product.
	if pending := UnverifiedOllamaCandidates(cfg); len(pending) > 0 {
		return readiness.Check{Label: "inference", Requirement: readiness.RequirementCore, Verdict: readiness.VerdictTodo,
			Detail:   fmt.Sprintf("%d local model candidate(s) bound but unproven (not pulled, or the probe failed)", len(pending)),
			Todo:     PullModelsFixCmd,
			Evidence: "ollama bindings without a probe: " + strings.Join(pending, ", ")}
	}
	return modelKeyCoreCheck(sbxOut, sbxOK)
}

// PullModelsFixCmd is the ONE copy-pasteable command for the state honest
// Ollama verification creates: candidates are bound, none has passed a probe,
// and the reason is almost always "the weights are not on disk". It is NOT a
// provider-key fix, and remediating this state with one (which is what falling
// through to modelKeyCoreCheck does) tells a pure-Ollama user to go buy an
// Anthropic key to fix a download they declined.
const PullModelsFixCmd = "pix setup --pull-models"

// UnverifiedOllamaCandidates returns bound-but-unproven ollama bindings — the
// declined-pull state. Non-empty means the fix is a pull, never a key.
func UnverifiedOllamaCandidates(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	var out []string
	for _, b := range cfg.Inference.Models {
		if !b.Available || b.Verified || b.Source != "" || !OllamaBindingDriver(cfg, b) {
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

func ConfiguredInferenceSummary(cfg *config.Config) (int, []string) {
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
// AnyModelKeyInOutput \u2014 the exact same "what counts as present" definition
// sbxModelKeyState uses for `run`'s launch gate \u2014 so doctor and the launch
// gate can never disagree about what "a key is present" means.
func modelKeyCoreCheck(sbxOut string, sbxOK bool) readiness.Check {
	names := strings.Join(secret.ModelProviders, "/")
	if !sbxOK {
		return readiness.Check{
			Label:       "model key",
			Requirement: readiness.RequirementCore,
			Verdict:     readiness.VerdictUnverifiable,
			Detail:      "cannot verify (sbx unavailable here) \u2014 re-run `pix doctor` on the host",
			Evidence:    "sbx secret ls: unavailable",
		}
	}
	if AnyModelKeyInOutput(sbxOut) {
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
		Todo:        ModelKeyFixCmd,
		Evidence:    "sbx secret ls: none of " + strings.Join(secret.ModelProviders, ", ") + " present",
	}
}

// AnyModelKeyInOutput reports whether out (the text of `sbx secret ls`) shows
// any of the model provider keys set. Pure — the SINGLE definition of "what
// counts as a present model key", shared by sbxModelKeyState (which owns the
// live sbx probe) and doctor's providers group (which reuses an
// already-fetched probe result) so the two can never diverge.
func AnyModelKeyInOutput(out string) bool {
	for _, k := range secret.ModelProviders {
		if cli.GrepWord(out, k) {
			return true
		}
	}
	return false
}

// ResolveSessionModel turns a --intent into a concrete model id for the
// interactive session, using the same router (registry + scorecard + policy)
// the subagent crew uses. An unknown intent name is treated as an ad-hoc
// accuracy-objective intent on that task type, matching `route pick`.
func ResolveSessionModel(intent string) (string, error) {
	reg, err := routing.LoadRegistry()
	if err != nil {
		return "", err
	}
	sc, err := routing.LoadScorecard()
	if err != nil {
		return "", err
	}
	pol, err := routing.LoadPolicy()
	if err != nil {
		return "", err
	}
	it, ok := pol.Intent(intent)
	if !ok {
		// An unknown intent must NOT silently fabricate a task type and fall back to
		// the policy default (that hid a bad --intent/run_intent behind a Sonnet
		// launch). Error instead: run.go exits on an explicit --intent typo and
		// degrades to pi's default on a bad config-sourced run_intent; doctor renders
		// "does not resolve".
		return "", fmt.Errorf("unknown intent %q (see `pix models show` for the intent list)", intent)
	}
	// Once backend bindings exist they are the availability authority. The
	// shipped catalog alone never proves that a model is callable.
	var binding *routing.Binding
	if cfg, cerr := config.Load(); cerr == nil && len(cfg.Inference.Models) > 0 {
		bindings := inference.Bindings(cfg)
		reg = routing.RegistryForBindings(reg, bindings, "")
		d := routing.Resolve(reg, sc, pol, it)
		for _, b := range bindings {
			if b.Available && b.Model == d.Model {
				bb := b
				binding = &bb
				break
			}
		}
		if binding == nil {
			return "", fmt.Errorf("intent %q has no callable model binding", intent)
		}
		return inference.RuntimeID(*binding), nil
	}
	d := routing.Resolve(reg, sc, pol, it)
	if d.Model == "" {
		return "", fmt.Errorf("router returned no model")
	}
	return d.Model, nil
}

// ModelKeyFixCmd is the ONE copy-pasteable command surfaced when doctor has
// POSITIVELY confirmed zero model-provider keys are set. It fixes any one
// provider (anthropic, chosen as the example); the other two are named in the
// core check's evidence, not repeated here as alternative commands.
const ModelKeyFixCmd = "pix models add anthropic"

// OllamaBindingDriver reports whether a binding runs through an ollama backend.
func OllamaBindingDriver(cfg *config.Config, b config.InferenceModelBinding) bool {
	backend, ok := cfg.Inference.Backends[b.Backend]
	return ok && backend.Driver == "ollama"
}

// presentModelProviders lists which of secret.ModelProviders sbxOut shows as set, for
// the core check's evidence string (alternatives belong in evidence, never in
// the fix command).
func presentModelProviders(sbxOut string) string {
	var got []string
	for _, k := range secret.ModelProviders {
		if cli.GrepWord(sbxOut, k) {
			got = append(got, k)
		}
	}
	return strings.Join(got, ", ")
}
