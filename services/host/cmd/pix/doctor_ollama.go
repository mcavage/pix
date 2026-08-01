package main

import (
	"strings"

	"pix/host/config"
)

// doctor_ollama.go builds the Ollama / local-models cluster on the S04
// readiness axes, deriving every per-model check from the SHARED
// ModelReadiness seam (modelreadiness.go). Ollama is ALWAYS optional (never
// core — a missing local model degrades fact capture/recall, it never blocks
// doctor's exit code). The "configured roles need it" distinction decides how
// its ABSENCE reads: with the memory service in the configured SERVICES set a
// missing ollama is a verified optional todo; without it, absence is merely
// not-configured (a note). Installed-but-daemon-down and a confirmed
// missing model are verified todos; a failed `ollama list` is UNVERIFIABLE —
// never "missing", and doctor NEVER pulls anything itself.

// modelCheck renders one ModelReadiness as a doctor check line.
func modelCheck(m ModelReadiness) check {
	label := "  " + m.Role
	detail := m.Purpose + " [" + m.Model + "]"
	if strings.TrimSpace(m.Model) == "" {
		// No tag configured for this role at all (e.g. the bridge model before
		// any `pix run` has written one) — an expected absence, never a
		// confirmed-missing todo. modelReadiness still computes a PullCmd of
		// "ollama pull " for an empty tag; this branch is what keeps that
		// meaningless command from ever reaching a renderer.
		return check{label: label, note: true, verdict: verdictUnverifiable,
			detail: m.Purpose + " — no model configured for this role"}
	}
	if !m.Installed {
		// Not configured: ollama itself is absent, so no claim about the tag.
		return check{label: label, note: true, verdict: verdictUnverifiable,
			detail: detail + " — needs ollama (then: " + m.PullCmd + ")"}
	}
	switch m.Verdict {
	case verdictReady:
		return check{label: label, verdict: verdictReady,
			detail:   "pulled — " + detail,
			evidence: "`ollama list` includes " + m.Model}
	case verdictTodo:
		return check{label: label, verdict: verdictTodo,
			detail:   detail + " — not pulled",
			evidence: "`ollama list` ran cleanly and does not include " + m.Model,
			todo:     m.PullCmd}
	default: // verdictUnverifiable
		return check{label: label, verdict: verdictUnverifiable,
			detail:   detail + " — could not verify (`ollama list` unavailable)",
			evidence: "`ollama list` did not succeed"}
	}
}

// modelPulled reports whether `ollama list` output lists the given model. The
// first column may carry a :tag suffix (e.g. "gemma4:latest").
func modelPulled(listOut, model string) bool {
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

// ollamaGroup renders the ollama + local-models cluster from the readiness
// snapshot: the ollama binary at the RESOLVED endpoint, sandbox reachability
// (never by creating a sandbox), and all THREE configured model roles —
// watcher, embed and bridge. Every fact comes from ollamaReadinessAxes, so
// doctor, status, setup and run cannot disagree about them.
func ollamaGroup(cfg *config.Config, env shellEnv) group {
	ollama := group{title: "Ollama / local models (optional: fact capture + semantic recall)", axis: axisOllamaHost}
	s := buildSnapshot(
		Request{Axes: []Axis{axisOllamaHost, axisOllamaSandbox, axisModelWatcher, axisModelEmbed, axisModelBridge}},
		ollamaReadinessAxes(cfg, env, resolveMCPSandboxContext(env).sandbox, nil),
	)
	ollama.checks = append(ollama.checks, s.All()...)
	// The hardware reading sits with the local models it sizes. It is appended
	// directly rather than through an axis because it asserts NOTHING about
	// readiness: it is an inference, so it is always a note and never a verdict
	// (see readiness_hardware.go).
	ollama.checks = append(ollama.checks, hardwareCheck(probeHostMemory(env))...)
	return ollama
}

// ollamaReadinessAxes is the ONE builder set for every Ollama axis, shared by
// doctor, status, setup and run. It probes Ollama exactly once (lazily: a
// caller that requests only ollama.host pays for one probe, one that requests
// none pays for nothing) and threads the resolved endpoint into every check.
func ollamaReadinessAxes(cfg *config.Config, env shellEnv, sandbox string, sandboxReachable *bool) map[Axis]axisBuilder {
	ep := effectiveOllamaEndpoint(cfg, env)
	var probed bool
	var p ollamaProbe
	probe := func() ollamaProbe {
		if !probed {
			p, probed = probeOllamaAt(env, ep), true
		}
		return p
	}
	builders := map[Axis]axisBuilder{
		axisOllamaHost:    func() []check { return ollamaHostAxis(cfg, env, ep, probe()) },
		axisOllamaSandbox: func() []check { return ollamaSandboxAxis(env, ep, probe(), sandbox, sandboxReachable) },
	}
	for axis, b := range ollamaModelAxes(cfg, ep, probe) {
		builders[axis] = b
	}
	return builders
}
