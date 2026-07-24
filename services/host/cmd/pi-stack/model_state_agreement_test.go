package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"pi-stack/host/config"
)

// model_state_agreement_test.go is QA finding #1: doctor and setup must never
// disagree about a local model's readiness EVIDENCE when probed against the
// SAME mock Ollama state. Both already route through the shared
// probeOllama/modelReadiness vocabulary (modelreadiness.go) specifically so
// this can't happen — this test is the hermetic guard that proves it, across
// the healthy, missing, and daemon-down/list-failure cases the finding names.
// requirement MAY legitimately differ (setup's receipt deliberately never
// weighs cfg.Services membership — AC-07 — while doctor's Ollama group does),
// so that axis is asserted as an EXPLICIT, documented distinction rather than
// folded into the agreement check.

// mockOllamaEnv builds a shellEnv exposing exactly the surface probeOllama
// touches (lookPath/dial/`ollama list`), so doctor's runDoctor and setup's
// setupModelReceipt/modelReadiness can be driven against the IDENTICAL fake
// host state.
func mockOllamaEnv(installed, daemonUp, listOK bool, listOut string) shellEnv {
	return shellEnv{
		lookPath: func(name string) (string, error) {
			if name == "ollama" && installed {
				return "/usr/bin/ollama", nil
			}
			return "", fmt.Errorf("exec: %q not found", name)
		},
		dial: func(port int) bool { return port == 11434 && daemonUp },
		run: func(name string, args ...string) (string, error) {
			if name == "ollama" && len(args) == 1 && args[0] == "list" {
				if listOK {
					return listOut, nil
				}
				return "", fmt.Errorf("connection refused")
			}
			return "", fmt.Errorf("no fake output for %q", name)
		},
	}
}

func TestModelStateAgreement_DoctorSetup(t *testing.T) {
	const watcherModel = "qwen3.5:9b"
	const embedModel = "nomic-embed-text"

	cases := []struct {
		name         string
		env          shellEnv
		wantEvidence Evidence
	}{
		{
			name:         "healthy: ollama installed, daemon up, both tags pulled",
			env:          mockOllamaEnv(true, true, true, "NAME\n"+watcherModel+":latest\n"+embedModel+":latest\n"),
			wantEvidence: EvidenceHealthy,
		},
		{
			name:         "missing: `ollama list` runs fine but the tag is absent",
			env:          mockOllamaEnv(true, true, true, "NAME\nsome-other-model:latest\n"),
			wantEvidence: EvidenceFailed,
		},
		{
			name:         "daemon-down/list-failure: ollama on PATH but `ollama list` cannot be verified",
			env:          mockOllamaEnv(true, false, false, ""),
			wantEvidence: EvidenceUnverifiable,
		},
		{
			name:         "not installed: ollama not on PATH at all",
			env:          mockOllamaEnv(false, false, false, ""),
			wantEvidence: EvidenceNotConfigured,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				Services:           []string{"memory"},
				MemoryWatcherModel: watcherModel,
				MemoryEmbedModel:   embedModel,
				OllamaBridgeModel:  watcherModel,
			}

			// --- doctor's evidence for the watcher model, via runDoctor's Ollama group ---
			r := runDoctor(cfg, tc.env)
			var doctorEv Evidence
			var doctorReq Requirement
			var doctorFound bool
			for _, g := range r.groups {
				if !strings.HasPrefix(g.title, "Ollama") {
					continue
				}
				for _, c := range g.checks {
					if strings.TrimSpace(c.label) == "watcher" {
						doctorEv, doctorReq, doctorFound = c.evidence, c.requirement, true
					}
				}
			}
			if !doctorFound {
				t.Fatalf("doctor did not report a watcher model check, groups=%+v", r.groups)
			}

			// --- setup's evidence for the SAME model, SAME mock env ---
			var out bytes.Buffer
			setupModelReceipt(tc.env, &out, strings.NewReader(""), cfg, false, false)
			p := probeOllama(tc.env)
			setupReadiness := modelReadiness("watcher", cfg.MemoryWatcherModel, "fact capture", p, RequirementUnconfiguredOptional)

			if doctorEv != tc.wantEvidence {
				t.Errorf("doctor evidence = %q, want %q", doctorEv, tc.wantEvidence)
			}
			if setupReadiness.Evidence != tc.wantEvidence {
				t.Errorf("setup evidence = %q, want %q", setupReadiness.Evidence, tc.wantEvidence)
			}
			if doctorEv != setupReadiness.Evidence {
				t.Errorf("doctor and setup DISAGREE on watcher model evidence against the same mock Ollama state: doctor=%q setup=%q", doctorEv, setupReadiness.Evidence)
			}

			// requirement is the axis that MAY legitimately differ (product
			// semantics, not a probe disagreement): doctor's Ollama group weighs it
			// by whether memory is in the configured SERVICES set (it is here, so
			// configured-optional); setup's receipt never asks that question at all
			// (AC-07 — it always passes RequirementUnconfiguredOptional). Assert
			// that distinction EXPLICITLY rather than requiring the two to match.
			if doctorReq != RequirementConfiguredOptional {
				t.Errorf("expected doctor's watcher requirement to reflect memory in SERVICES (configured-optional), got %q", doctorReq)
			}
			if setupReadiness.Requirement != RequirementUnconfiguredOptional {
				t.Errorf("expected setup's requirement to stay unconfigured-optional per AC-07, got %q", setupReadiness.Requirement)
			}
		})
	}
}
