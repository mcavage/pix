package main

import (
	"strings"

	"pi-stack/host/config"
)

// modelCheck reports whether an ollama model is pulled.
func modelCheck(role, model, purpose string, ollamaInstalled bool, listOut string, listOK bool) check {
	label := "  " + role
	detail := purpose + " [" + model + "]"
	cmd := "ollama pull " + model
	if !ollamaInstalled {
		return check{label: label, verdict: verdictTodo, detail: detail + " — needs ollama", todo: cmd}
	}
	if listOK && modelPulled(listOut, model) {
		return check{label: label, verdict: verdictReady, detail: "pulled — " + detail}
	}
	return check{label: label, verdict: verdictTodo, detail: detail + " — not pulled", todo: cmd}
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
// the :11434 daemon, and the configured watcher/embed models.
func ollamaGroup(cfg *config.Config, env shellEnv) group {
	ollama := group{title: "Ollama / local models (optional: fact capture + semantic recall)"}
	ollamaInstalled := false
	if _, err := env.lookPath("ollama"); err == nil {
		ollamaInstalled = true
		up := env.dial(11434)
		ollama.checks = append(ollama.checks, check{
			label:   "ollama",
			verdict: verdictReady,
			detail:  "installed, :11434 " + upDown(up),
		})
	} else {
		ollama.checks = append(ollama.checks, check{
			label:   "ollama",
			verdict: verdictTodo,
			detail:  "not installed",
			todo:    "install ollama — https://ollama.com",
		})
	}
	// List models once, reuse for both watcher + embed.
	modelOut, modelOK := "", false
	if ollamaInstalled {
		if out, err := env.run("ollama", "list"); err == nil {
			modelOut, modelOK = out, true
		}
	}
	ollama.checks = append(ollama.checks,
		modelCheck("watcher", cfg.MemoryWatcherModel, "fact capture", ollamaInstalled, modelOut, modelOK),
		modelCheck("embed", cfg.MemoryEmbedModel, "semantic recall", ollamaInstalled, modelOut, modelOK),
	)
	return ollama
}
