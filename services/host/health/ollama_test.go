package health

import (
	"context"
	"testing"

	"pix/host/inference"
)

func endpointAt(host string, port int) inference.OllamaEndpoint {
	return inference.OllamaEndpoint{URL: "http://" + host, Host: host, Port: port}
}

func TestOllamaProbe_LocalReadyWithEmbedModelPresent(t *testing.T) {
	p := OllamaProbe{
		Detect: func() inference.OllamaStatus {
			return inference.OllamaStatus{
				CLIPresent: true, Reachable: true, Mode: inference.OllamaModeLocal,
				Endpoint: endpointAt("127.0.0.1", 11434),
				Models:   map[string]inference.OllamaModelInfo{"nomic-embed-text": {Tag: "nomic-embed-text"}},
			}
		},
		EmbedModel: "nomic-embed-text",
	}
	r := p.Check(context.Background())
	if !r.OK() || r.Fix != "" {
		t.Errorf("Check = %+v, want ready with no fix", r)
	}
	if p.Required() {
		t.Error("OllamaProbe must never be required")
	}
}

func TestOllamaProbe_LocalMissingEmbedModelOffersPull(t *testing.T) {
	p := OllamaProbe{
		Detect: func() inference.OllamaStatus {
			return inference.OllamaStatus{
				CLIPresent: true, Reachable: true, Mode: inference.OllamaModeLocal,
				Endpoint: endpointAt("127.0.0.1", 11434),
				Models:   map[string]inference.OllamaModelInfo{"qwen3.5:9b": {Tag: "qwen3.5:9b"}},
			}
		},
		EmbedModel: "nomic-embed-text",
	}
	r := p.Check(context.Background())
	if r.Effective() != StatusAbsent {
		t.Fatalf("Effective = %s, want absent", r.Effective())
	}
	if r.Fix != "ollama pull nomic-embed-text" {
		t.Errorf("Fix = %q, want the local pull command", r.Fix)
	}
	if r.Required {
		t.Error("a missing embed model must never be a required (blocking) gap")
	}
}

func TestOllamaProbe_RemoteMissingEmbedModelNeverOffersPull(t *testing.T) {
	p := OllamaProbe{
		Detect: func() inference.OllamaStatus {
			return inference.OllamaStatus{
				CLIPresent: true, Reachable: true, Mode: inference.OllamaModeRemote,
				Endpoint: endpointAt("team-ollama.internal", 11434),
				Models:   map[string]inference.OllamaModelInfo{"glm-5.2:cloud": {Tag: "glm-5.2:cloud"}},
			}
		},
		EmbedModel: "nomic-embed-text",
	}
	r := p.Check(context.Background())
	if r.Effective() != StatusAbsent {
		t.Fatalf("Effective = %s, want absent (the model still is not there)", r.Effective())
	}
	if r.Fix != "" {
		t.Errorf("Fix = %q, want none \u2014 a remote endpoint is not this host's disk to pull into", r.Fix)
	}
}

func TestOllamaProbe_UnreachableLocalIsAbsentWithStartFix(t *testing.T) {
	p := OllamaProbe{Detect: func() inference.OllamaStatus {
		return inference.OllamaStatus{CLIPresent: true, Reachable: false, Mode: inference.OllamaModeLocal, Endpoint: endpointAt("127.0.0.1", 11434)}
	}}
	r := p.Check(context.Background())
	if r.Effective() != StatusAbsent || r.Fix == "" {
		t.Errorf("Check = %+v, want absent with a start-Ollama fix", r)
	}
}

func TestOllamaProbe_UnreachableRemoteNeverSuggestsStartingSomeoneElsesDaemon(t *testing.T) {
	p := OllamaProbe{Detect: func() inference.OllamaStatus {
		return inference.OllamaStatus{CLIPresent: true, Reachable: false, Mode: inference.OllamaModeRemote, Endpoint: endpointAt("team-ollama.internal", 11434)}
	}}
	r := p.Check(context.Background())
	if r.Effective() != StatusAbsent {
		t.Fatalf("Effective = %s, want absent", r.Effective())
	}
	if r.Fix != "" {
		t.Errorf("Fix = %q, want none — Pix does not own this daemon", r.Fix)
	}
}

func TestOllamaProbe_NotInstalledIsOffNotAGap(t *testing.T) {
	p := OllamaProbe{Detect: func() inference.OllamaStatus { return inference.OllamaStatus{} }}
	r := p.Check(context.Background())
	if r.Effective() != StatusOff {
		t.Errorf("Effective = %s, want off", r.Effective())
	}
	if r.Fix != "" {
		t.Errorf("Fix = %q, want none for an optional, not-installed capability", r.Fix)
	}
}
