// modelreadiness.go is the SHARED, presentation-free local-model readiness
// vocabulary (PRD AC-06/07, Unit U2). Both `pi-stack doctor` (doctor.go's
// Ollama group) and `pi-stack setup` (its final receipt) probe Ollama exactly
// once via probeOllama and derive every per-model (watcher/embed/bridge)
// check from the SAME ollamaProbe + modelReadiness, so the two commands can
// never disagree about what is pulled.
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
// host with no Ollama pays for exactly one failed lookPath call. R2-02: the
// `ollama list` exec is BOUNDED (probeRun: hard timeout + output cap), so a
// wedged ollama classifies as list-unverified for BOTH callers — doctor's
// Ollama group and setup's receipt share this one bounded probe — rather
// than hanging either command.
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
// same Requirement/Evidence pair every other doctor check does (readiness.go)
// so a caller never has to invent a parallel vocabulary for local models.
type ModelReadiness struct {
	Role        string // "watcher" | "embed" | "bridge"
	Model       string // configured tag, e.g. "qwen3.5:9b"
	Purpose     string // short human purpose, e.g. "fact capture (memory watcher)"
	Requirement Requirement
	Evidence    Evidence
	PullCmd     string // "ollama pull <tag>" — always populated, even when healthy
}

// modelReadiness evaluates one (role, model) pair against p. req is supplied
// by the CALLER — doctor's Ollama group weighs it by whether memory is in the
// configured SERVICES set; setup's receipt deliberately does NOT (AC-07: setup
// never calls service-membership operational readiness), so the two callers
// can legitimately pass different requirements for the same model.
func modelReadiness(role, model, purpose string, p ollamaProbe, req Requirement) ModelReadiness {
	m := ModelReadiness{
		Role: role, Model: model, Purpose: purpose,
		Requirement: req, PullCmd: "ollama pull " + model,
	}
	switch {
	case !p.installed:
		m.Evidence = EvidenceNotConfigured
	case p.listOK && modelPulled(p.listOut, model):
		m.Evidence = EvidenceHealthy
	case p.listOK:
		// `ollama list` ran fine and simply does not list this tag — a
		// CONFIRMED gap, not a guess.
		m.Evidence = EvidenceFailed
	default:
		// ollama is on PATH but `ollama list` itself did not succeed (e.g. the
		// daemon isn't reachable) — this could not be verified one way or the
		// other, which is different from a confirmed "not pulled".
		m.Evidence = EvidenceUnverifiable
	}
	return m
}

// missingModel is one Ollama tag, plus every role that depends on it (e.g.
// qwen3.5:9b is watcher+bridge by default). Used both for CONFIRMED-missing
// tags (computeMissingModels) and for unverifiable tags (
// computeUnverifiableModels) — same (tag, roles) shape, two disjoint sets.
type missingModel struct {
	tag   string
	roles []string
}

// filterModelsByEvidence reduces readinesses to the distinct tags whose
// Evidence satisfies match, deduping identical tags across roles so a shared
// model is named once, with every dependent role listed, in first-seen
// order. Shared by computeMissingModels and computeUnverifiableModels so the
// two never drift into different dedup/order behavior.
func filterModelsByEvidence(readinesses []ModelReadiness, match func(Evidence) bool) []missingModel {
	var out []missingModel
	index := make(map[string]int, len(readinesses))
	for _, m := range readinesses {
		if m.Model == "" || !match(m.Evidence) {
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
// that are CONFIRMED missing (EvidenceFailed only — `ollama list` ran fine
// and simply does not list the tag). R1-09: EvidenceUnverifiable must NEVER
// enter this set — a stopped daemon or a failed `ollama list` proves nothing
// about whether the tag is actually pulled, so treating it as "missing" would
// contradict the evidence and could force-repull an already-installed model.
// See computeUnverifiableModels for the disjoint unverifiable set.
func computeMissingModels(readinesses []ModelReadiness) []missingModel {
	return filterModelsByEvidence(readinesses, func(e Evidence) bool { return e == EvidenceFailed })
}

// computeUnverifiableModels reduces a set of ModelReadiness to the distinct
// tags that could not be verified one way or the other (ollama installed but
// the daemon was down or `ollama list` itself failed). These are reported
// separately from computeMissingModels: never offered for pull, never called
// "missing" — only receipted with an accurate diagnostic of why they
// couldn't be checked.
func computeUnverifiableModels(readinesses []ModelReadiness) []missingModel {
	return filterModelsByEvidence(readinesses, func(e Evidence) bool { return e == EvidenceUnverifiable })
}
