package main

import (
	"fmt"
	"testing"
)

// --- probeOllama: the ONE probe (lookPath, daemon reachability, `ollama
// list`) shared by doctor and setup. ---

func TestProbeOllama_NotInstalled(t *testing.T) {
	env := shellEnv{
		lookPath: func(string) (string, error) { return "", fmt.Errorf("not found") },
		dial:     func(int) bool { t.Fatal("must not dial when ollama is not installed"); return false },
		run: func(string, ...string) (string, error) {
			t.Fatal("must not run when ollama is not installed")
			return "", nil
		},
	}
	p := probeOllama(env)
	if p.installed {
		t.Error("expected installed=false")
	}
	if p.daemonUp || p.listOK {
		t.Errorf("expected no daemon/list state when not installed, got %+v", p)
	}
}

func TestProbeOllama_InstalledHealthy(t *testing.T) {
	env := shellEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/ollama", nil },
		dial:     func(port int) bool { return port == 11434 },
		run: func(name string, args ...string) (string, error) {
			if name == "ollama" && len(args) == 1 && args[0] == "list" {
				return "NAME\nqwen3.5:9b\n", nil
			}
			return "", fmt.Errorf("not found")
		},
	}
	p := probeOllama(env)
	if !p.installed || !p.daemonUp || !p.listOK {
		t.Errorf("expected fully healthy probe, got %+v", p)
	}
	if p.listOut == "" {
		t.Error("expected listOut to carry the `ollama list` output")
	}
}

func TestProbeOllama_InstalledDaemonDownListFails(t *testing.T) {
	env := shellEnv{
		lookPath: func(string) (string, error) { return "/usr/bin/ollama", nil },
		dial:     func(int) bool { return false },
		run:      func(string, ...string) (string, error) { return "", fmt.Errorf("not found") },
	}
	p := probeOllama(env)
	if !p.installed {
		t.Error("expected installed=true")
	}
	if p.daemonUp {
		t.Error("expected daemonUp=false")
	}
	if p.listOK {
		t.Error("expected listOK=false when `ollama list` errors")
	}
}

// --- modelReadiness: the pure per-model classification against a shared probe. ---

func TestModelReadiness_NotInstalled_NotConfigured(t *testing.T) {
	m := modelReadiness("watcher", "qwen3.5:9b", "fact capture", ollamaProbe{}, RequirementUnconfiguredOptional)
	if m.Evidence != EvidenceNotConfigured {
		t.Errorf("evidence = %q, want not-configured", m.Evidence)
	}
	if m.PullCmd != "ollama pull qwen3.5:9b" {
		t.Errorf("PullCmd = %q", m.PullCmd)
	}
	if m.Requirement != RequirementUnconfiguredOptional {
		t.Errorf("requirement not propagated: %q", m.Requirement)
	}
}

func TestModelReadiness_Pulled_Healthy(t *testing.T) {
	p := ollamaProbe{installed: true, listOK: true, listOut: "NAME\nqwen3.5:9b:latest\n"}
	m := modelReadiness("bridge", "qwen3.5:9b", "local chat", p, RequirementCore)
	if m.Evidence != EvidenceHealthy {
		t.Errorf("evidence = %q, want healthy", m.Evidence)
	}
}

func TestModelReadiness_NotPulled_Failed(t *testing.T) {
	p := ollamaProbe{installed: true, listOK: true, listOut: "NAME\ngemma4:latest\n"}
	m := modelReadiness("embed", "nomic-embed-text", "semantic recall", p, RequirementCore)
	if m.Evidence != EvidenceFailed {
		t.Errorf("evidence = %q, want failed", m.Evidence)
	}
}

func TestModelReadiness_ListUnavailable_Unverifiable(t *testing.T) {
	// Installed but `ollama list` itself could not be run/verified (e.g. the
	// daemon is unreachable) — distinct from a CONFIRMED "not pulled".
	p := ollamaProbe{installed: true, listOK: false}
	m := modelReadiness("watcher", "qwen3.5:9b", "fact capture", p, RequirementCore)
	if m.Evidence != EvidenceUnverifiable {
		t.Errorf("evidence = %q, want unverifiable", m.Evidence)
	}
}

// --- computeMissingModels: dedup identical tags across roles, order preserved. ---

func TestComputeMissingModels_DedupsSharedTag(t *testing.T) {
	readinesses := []ModelReadiness{
		{Role: "watcher", Model: "qwen3.5:9b", Evidence: EvidenceFailed},
		{Role: "embed", Model: "nomic-embed-text", Evidence: EvidenceHealthy},
		{Role: "bridge", Model: "qwen3.5:9b", Evidence: EvidenceFailed},
	}
	missing := computeMissingModels(readinesses)
	if len(missing) != 1 {
		t.Fatalf("expected exactly one deduped missing model, got %d: %+v", len(missing), missing)
	}
	if missing[0].tag != "qwen3.5:9b" {
		t.Errorf("tag = %q, want qwen3.5:9b", missing[0].tag)
	}
	if len(missing[0].roles) != 2 || missing[0].roles[0] != "watcher" || missing[0].roles[1] != "bridge" {
		t.Errorf("roles = %v, want [watcher bridge]", missing[0].roles)
	}
}

func TestComputeMissingModels_HealthyExcluded(t *testing.T) {
	readinesses := []ModelReadiness{
		{Role: "watcher", Model: "qwen3.5:9b", Evidence: EvidenceHealthy},
		{Role: "embed", Model: "nomic-embed-text", Evidence: EvidenceFailed},
	}
	missing := computeMissingModels(readinesses)
	if len(missing) != 1 || missing[0].tag != "nomic-embed-text" {
		t.Errorf("expected only the failed embed model, got %+v", missing)
	}
}

func TestComputeMissingModels_UnverifiableIncluded(t *testing.T) {
	// Unverifiable is not proven healthy — it must still surface so setup
	// never silently claims readiness it couldn't confirm.
	readinesses := []ModelReadiness{
		{Role: "watcher", Model: "qwen3.5:9b", Evidence: EvidenceUnverifiable},
	}
	missing := computeMissingModels(readinesses)
	if len(missing) != 1 {
		t.Errorf("expected the unverifiable model to be listed as missing, got %+v", missing)
	}
}
