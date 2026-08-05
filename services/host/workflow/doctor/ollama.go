package doctor

import (
	"pix/host/hostenv"
	"pix/host/readiness"
	"pix/host/readiness/axis"

	"pix/host/config"
)

// doctor_ollama.go builds the Ollama / local-models cluster on the S04
// readiness axes, deriving every per-model check from the SHARED
// axis.ModelReadiness seam (modelgo). Ollama is ALWAYS optional (never
// core — a missing local model degrades fact capture/recall, it never blocks
// doctor's exit code). The "configured roles need it" distinction decides how
// its ABSENCE reads: with the memory service in the configured SERVICES set a
// missing ollama is a verified optional todo; without it, absence is merely
// not-configured (a note). Installed-but-daemon-down and a confirmed
// missing model are verified todos; a failed `ollama list` is UNVERIFIABLE —
// never "missing", and doctor NEVER pulls anything itself.

// OllamaReadinessAxes is the ONE builder set for every Ollama axis, shared by
// doctor, status, setup and run. It probes Ollama exactly once (lazily: a
// caller that requests only ollama.host pays for one probe, one that requests
// none pays for nothing) and threads the resolved endpoint into every check.
func OllamaReadinessAxes(cfg *config.Config, env hostenv.Env, sandbox string, sandboxReachable *bool) map[readiness.Axis]readiness.AxisBuilder {
	ep := axis.EffectiveOllamaEndpoint(cfg, env)
	var probed bool
	var p axis.OllamaProbe
	probe := func() axis.OllamaProbe {
		if !probed {
			p, probed = axis.ProbeOllamaAt(env, ep), true
		}
		return p
	}
	builders := map[readiness.Axis]readiness.AxisBuilder{
		readiness.AxisOllamaHost:    func() []readiness.Check { return axis.OllamaHostAxis(cfg, env, ep, probe()) },
		readiness.AxisOllamaSandbox: func() []readiness.Check { return axis.OllamaSandboxAxis(env, ep, probe(), sandbox, sandboxReachable) },
	}
	for axis, b := range axis.OllamaModelAxes(cfg, ep, probe) {
		builders[axis] = b
	}
	return builders
}
