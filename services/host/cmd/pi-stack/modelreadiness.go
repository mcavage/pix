// modelreadiness.go is the SHARED, presentation-free local-model readiness
// vocabulary. `pi-stack doctor` (doctor_ollama.go) derives every per-model
// (watcher/embed/bridge) check from ONE ollamaProbe + modelReadiness, and a
// future `pi-stack setup` receipt (S08) consumes the SAME seam — so the two
// commands can never disagree about what is pulled.
package main

// ollamaProbe is the single snapshot of the local Ollama installation: is the
// binary on PATH, does its daemon answer on :11434, and (only meaningful when
// installed) the `ollama list` output. Building this once and passing it to
// every modelReadiness call is what makes "probe Ollama once" true — no
// caller re-execs `ollama list` per model.
type ollamaProbe struct {
	installed bool
	daemonUp  bool
	listOut   string
	listOK    bool
}

// probeOllama runs lookPath, a :11434 daemon dial, and `ollama list` — daemon
// dial and list are skipped entirely when ollama isn't even on PATH, so a
// host with no Ollama pays for exactly one failed lookPath call. The
// `ollama list` exec is BOUNDED (probeRun: hard timeout + output cap), so a
// wedged ollama classifies as list-unverified rather than hanging the caller.
func probeOllama(env shellEnv) ollamaProbe {
	if env.lookPath == nil {
		return ollamaProbe{}
	}
	if _, err := env.lookPath("ollama"); err != nil {
		return ollamaProbe{}
	}
	p := ollamaProbe{installed: true}
	if env.dial != nil {
		p.daemonUp = env.dial(11434)
	}
	if out, timedOut, err := probeRun(env, "ollama", "list"); err == nil && !timedOut {
		p.listOut, p.listOK = out, true
	}
	return p
}

// ModelReadiness is the pure, presentation-free readiness of ONE configured
// Ollama model tag, evaluated against a shared ollamaProbe. It carries the
// same requirement/verdict axes every other doctor check does
// (doctor_readiness.go) so a caller never has to invent a parallel vocabulary
// for local models. Installed=false means Ollama itself isn't on PATH: the
// model is NOT CONFIGURED (an expected absence, a note) rather than any
// verdict about the tag — and it must never enter the missing OR unverifiable
// sets below.
type ModelReadiness struct {
	Role        string // "watcher" | "embed" | "bridge"
	Model       string // configured tag, e.g. "qwen3.5:9b"
	Purpose     string // short human purpose, e.g. "fact capture (memory watcher)"
	Requirement requirement
	Verdict     verdict // meaningful only when Installed
	Installed   bool    // ollama on PATH; false => not-configured, no verdict claimed
	PullCmd     string  // "ollama pull <tag>" — always populated, even when healthy
}

// modelReadiness evaluates one (role, model) pair against p. req is supplied
// by the CALLER — doctor's Ollama group weighs it by whether configured roles
// (the memory service) actually depend on local models; a setup receipt may
// legitimately pass a different requirement for the same model.
func modelReadiness(role, model, purpose string, p ollamaProbe, req requirement) ModelReadiness {
	m := ModelReadiness{
		Role: role, Model: model, Purpose: purpose,
		Requirement: req, Installed: p.installed,
		PullCmd: "ollama pull " + model,
	}
	switch {
	case !p.installed:
		// Not configured: no verdict is claimed. Leave the zero verdict, which
		// the framework reads fail-safe (unverifiable) if anyone consults it —
		// but Installed=false is the authoritative signal.
	case p.listOK && modelPulled(p.listOut, model):
		m.Verdict = verdictReady
	case p.listOK:
		// `ollama list` ran fine and simply does not list this tag — a
		// CONFIRMED gap, not a guess.
		m.Verdict = verdictTodo
	default:
		// ollama is on PATH but `ollama list` itself did not succeed (e.g. the
		// daemon isn't reachable) — this could not be verified one way or the
		// other, which is different from a confirmed "not pulled".
		m.Verdict = verdictUnverifiable
	}
	return m
}

// missingModel is one Ollama tag, plus every role that depends on it (e.g.
// qwen3.5:9b is watcher+bridge by default). Used both for CONFIRMED-missing
// tags (computeMissingModels) and for unverifiable tags
// (computeUnverifiableModels) — same (tag, roles) shape, two disjoint sets.
type missingModel struct {
	tag   string
	roles []string
}

// filterModelsByVerdict reduces readinesses to the distinct INSTALLED tags
// whose verdict satisfies match, deduping identical tags across roles so a
// shared model is named once, with every dependent role listed, in first-seen
// order. Not-installed (not-configured) entries never match anything. Shared
// by computeMissingModels and computeUnverifiableModels so the two never
// drift into different dedup/order behavior.
func filterModelsByVerdict(readinesses []ModelReadiness, match func(verdict) bool) []missingModel {
	var out []missingModel
	index := make(map[string]int, len(readinesses))
	for _, m := range readinesses {
		if m.Model == "" || !m.Installed || !match(m.Verdict) {
			continue
		}
		if i, ok := index[m.Model]; ok {
			out[i].roles = append(out[i].roles, m.Role)
			continue
		}
		index[m.Model] = len(out)
		out = append(out, missingModel{tag: m.Model, roles: []string{m.Role}})
	}
	return out
}

// computeMissingModels reduces a set of ModelReadiness to the distinct tags
// that are CONFIRMED missing (verdictTodo only — `ollama list` ran fine and
// simply does not list the tag). verdictUnverifiable must NEVER enter this
// set — a stopped daemon or a failed `ollama list` proves nothing about
// whether the tag is actually pulled, so treating it as "missing" would
// contradict the evidence and could force-repull an already-installed model.
// See computeUnverifiableModels for the disjoint unverifiable set. Nothing
// here pulls anything: callers only surface the PullCmd for the user to act on.
func computeMissingModels(readinesses []ModelReadiness) []missingModel {
	return filterModelsByVerdict(readinesses, func(v verdict) bool { return v == verdictTodo })
}

// computeUnverifiableModels reduces a set of ModelReadiness to the distinct
// tags that could not be verified one way or the other (ollama installed but
// the daemon was down or `ollama list` itself failed). These are reported
// separately from computeMissingModels: never offered for pull, never called
// "missing" — only receipted with an accurate diagnostic of why they
// couldn't be checked.
func computeUnverifiableModels(readinesses []ModelReadiness) []missingModel {
	return filterModelsByVerdict(readinesses, func(v verdict) bool { return v == verdictUnverifiable })
}

// ollamaVerifyFailureReason names WHY a model tag could not be verified, so a
// receipt can distinguish a down daemon from a daemon that's up but whose
// `ollama list` call itself failed.
func ollamaVerifyFailureReason(p ollamaProbe) string {
	if !p.daemonUp {
		return "the Ollama daemon is not running (:11434 down)"
	}
	return "`ollama list` did not succeed"
}
