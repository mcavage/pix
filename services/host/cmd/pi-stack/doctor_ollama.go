package main

import (
	"strings"

	"pi-stack/host/config"
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

// ollamaGroup builds the ollama + local-models cluster: the ollama binary,
// the :11434 daemon, and the configured watcher/embed models — all derived
// from ONE probeOllama snapshot.
func ollamaGroup(cfg *config.Config, env shellEnv) group {
	ollama := group{title: "Ollama / local models (optional: fact capture + semantic recall)"}
	p := probeOllama(env)
	// `ollama list` succeeding proves the daemon answered even when the :11434
	// dial was blocked, so either signal counts as "daemon up".
	daemonUp := p.daemonUp || p.listOK
	memoryEnabled := enabled(cfg, "memory")
	switch {
	case p.installed && daemonUp:
		ollama.checks = append(ollama.checks, check{
			label:    "ollama",
			verdict:  verdictReady,
			detail:   "installed, :11434 up",
			evidence: "ollama on PATH; daemon answered",
		})
	case p.installed:
		// Installed but the daemon is down is a VERIFIED optional todo (never a
		// ✓), and the action is starting the daemon — not a blind claim about
		// pulled models (those stay unverifiable below).
		ollama.checks = append(ollama.checks, check{
			label:    "ollama",
			verdict:  verdictTodo,
			detail:   "installed but the daemon is not running (:11434 down)",
			evidence: "ollama on PATH; :11434 down; `ollama list` failed",
			todo:     "start the Ollama daemon: `ollama serve` (or open the Ollama app), then re-run `pi-stack doctor`",
		})
	case memoryEnabled:
		// A configured role (the memory service) actually depends on local
		// models, so a missing ollama is a real, verified optional gap.
		ollama.checks = append(ollama.checks, check{
			label:    "ollama",
			verdict:  verdictTodo,
			detail:   "not installed (the configured memory service needs it for capture + recall)",
			evidence: "ollama not on PATH; memory in configured services",
			todo:     "install ollama — https://ollama.com",
		})
	default:
		// Nothing configured depends on it: absence is expected, not a gap.
		ollama.checks = append(ollama.checks, check{
			label:   "ollama",
			note:    true,
			verdict: verdictUnverifiable,
			detail:  "not installed — optional; install: https://ollama.com",
		})
	}
	ollama.checks = append(ollama.checks,
		modelCheck(modelReadiness("watcher", cfg.MemoryWatcherModel, "fact capture", p, requirementOptional)),
		modelCheck(modelReadiness("embed", cfg.MemoryEmbedModel, "semantic recall", p, requirementOptional)),
	)
	return ollama
}
