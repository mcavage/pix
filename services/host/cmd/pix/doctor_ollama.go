package main

import (
	"pix/host/hostenv"
	"pix/host/readiness"
	"strings"

	"pix/host/config"
)

// doctor_ollama.go builds the Ollama / local-models cluster on the S04
// readiness axes, deriving every per-model check from the SHARED
// ModelReadiness seam (modelgo). Ollama is ALWAYS optional (never
// core — a missing local model degrades fact capture/recall, it never blocks
// doctor's exit code). The "configured roles need it" distinction decides how
// its ABSENCE reads: with the memory service in the configured SERVICES set a
// missing ollama is a verified optional todo; without it, absence is merely
// not-configured (a note). Installed-but-daemon-down and a confirmed
// missing model are verified todos; a failed `ollama list` is UNVERIFIABLE —
// never "missing", and doctor NEVER pulls anything itself.

// modelCheck renders one ModelReadiness as a doctor check line.
func modelCheck(m ModelReadiness) readiness.Check {
	label := "  " + m.Role
	detail := m.Purpose + " [" + m.Model + "]"
	if strings.TrimSpace(m.Model) == "" {
		// No tag configured for this role at all (e.g. the bridge model before
		// any `pix run` has written one) — an expected absence, never a
		// confirmed-missing todo. modelReadiness still computes a PullCmd of
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
func ollamaGroup(cfg *config.Config, env hostenv.Env) readiness.Group {
	ollama := readiness.Group{Title: "Ollama / local models (optional: fact capture + semantic recall)", Axis: readiness.AxisOllamaHost}
	s := readiness.Build(
		readiness.Request{Axes: []readiness.Axis{readiness.AxisOllamaHost, readiness.AxisOllamaSandbox, readiness.AxisModelWatcher, readiness.AxisModelEmbed, readiness.AxisModelBridge}},
		ollamaReadinessAxes(cfg, env, resolveMCPSandboxContext(env).sandbox, nil),
	)
	ollama.Checks = append(ollama.Checks, s.All()...)
	// The hardware reading sits with the local models it sizes. It is appended
	// directly rather than through an axis because it asserts NOTHING about
	// readiness: it is an inference, so it is always a note and never a verdict
	// (see readiness_hardware.go).
	ollama.Checks = append(ollama.Checks, hardwareCheck(probeHostMemory(env))...)
	return ollama
}

// ollamaReadinessAxes is the ONE builder set for every Ollama axis, shared by
// doctor, status, setup and run. It probes Ollama exactly once (lazily: a
// caller that requests only ollama.host pays for one probe, one that requests
// none pays for nothing) and threads the resolved endpoint into every check.
func ollamaReadinessAxes(cfg *config.Config, env hostenv.Env, sandbox string, sandboxReachable *bool) map[readiness.Axis]readiness.AxisBuilder {
	ep := effectiveOllamaEndpoint(cfg, env)
	var probed bool
	var p ollamaProbe
	probe := func() ollamaProbe {
		if !probed {
			p, probed = probeOllamaAt(env, ep), true
		}
		return p
	}
	builders := map[readiness.Axis]readiness.AxisBuilder{
		readiness.AxisOllamaHost:    func() []readiness.Check { return ollamaHostAxis(cfg, env, ep, probe()) },
		readiness.AxisOllamaSandbox: func() []readiness.Check { return ollamaSandboxAxis(env, ep, probe(), sandbox, sandboxReachable) },
	}
	for axis, b := range ollamaModelAxes(cfg, ep, probe) {
		builders[axis] = b
	}
	return builders
}
