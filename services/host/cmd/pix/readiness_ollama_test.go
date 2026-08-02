package main

import (
	"errors"
	"pix/host/readiness"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/sys/systest"
)

// errNotFoundForTest is the stand-in for exec.LookPath's not-found error.
var errNotFoundForTest = errors.New("not found")

func ollamaEnv(t *testing.T, ollamaHost string, dialOK bool, list string) shellEnv {
	t.Helper()
	return shellEnv{System: &systest.Fake{LookPathFn: func(name string) (string, error) {
		if name == "ollama" {
			return "/usr/local/bin/ollama", nil
		}
		return "", errNotFoundForTest
	}, GetenvFn: func(k string) string {
		if k == "OLLAMA_HOST" {
			return ollamaHost
		}
		return ""
	}, DialLocalFn: func(int) bool { return dialOK }, RunFn: func(name string, args ...string) (string, error) {
		if name == "ollama" && len(args) > 0 && args[0] == "list" {
			if !dialOK {
				return "", errNotFoundForTest // a down daemon fails `ollama list` too
			}
			return list, nil
		}
		return "", errNotFoundForTest
	}}}
}

func TestEffectiveOllamaEndpoint(t *testing.T) {
	cfg := &config.Config{}
	cases := []struct {
		env      string
		wantURL  string
		wantPort int
		wantSrc  string
	}{
		{"", "http://127.0.0.1:11434", 11434, "default"},
		{"0.0.0.0:11434", "http://0.0.0.0:11434", 11434, "OLLAMA_HOST"},
		{"box.local", "http://box.local:11434", 11434, "OLLAMA_HOST"},
		{"box.local:9999", "http://box.local:9999", 9999, "OLLAMA_HOST"},
		{"http://10.0.0.2:1234", "http://10.0.0.2:1234", 1234, "OLLAMA_HOST"},
		{"https://ollama.internal:443", "https://ollama.internal:443", 443, "OLLAMA_HOST"},
	}
	for _, tc := range cases {
		ep := effectiveOllamaEndpoint(cfg, shellEnv{System: &systest.Fake{GetenvFn: func(string) string { return tc.env }}})
		if ep.URL != tc.wantURL || ep.Port != tc.wantPort || ep.Source != tc.wantSrc {
			t.Errorf("OLLAMA_HOST=%q -> %+v, want url=%s port=%d source=%s", tc.env, ep, tc.wantURL, tc.wantPort, tc.wantSrc)
		}
	}
}

// The resolved endpoint must appear in the evidence of every Ollama verdict:
// a verdict about an endpoint nobody names is unactionable.
func TestOllamaEvidenceNamesTheResolvedEndpoint(t *testing.T) {
	cfg := &config.Config{MemoryWatcherModel: "w:1", MemoryEmbedModel: "e:1", OllamaBridgeModel: "b:1", Services: []string{"memory"}}
	env := ollamaEnv(t, "box.local:9999", false, "")
	s := readiness.Build(
		readiness.Request{Axes: []readiness.Axis{readiness.AxisOllamaHost, readiness.AxisModelWatcher, readiness.AxisModelEmbed, readiness.AxisModelBridge}},
		ollamaReadinessAxes(cfg, env, "", nil),
	)
	for _, c := range s.All() {
		if c.Endpoint != "http://box.local:9999" {
			t.Errorf("%s: endpoint = %q, want the resolved endpoint", c.Label, c.Endpoint)
		}
	}
	host, _ := s.Checks(readiness.AxisOllamaHost)
	if !strings.Contains(host[0].Evidence, "http://box.local:9999") {
		t.Errorf("ollama.host evidence must name the endpoint, got %q", host[0].Evidence)
	}
	if host[0].Result() != readiness.VerdictTodo {
		t.Errorf("installed with a dead endpoint is a verified todo, got %q", host[0].Result())
	}
}

// All THREE model roles are checked. The bridge role used to be invisible.
func TestEveryModelRoleHasAnAxis(t *testing.T) {
	cfg := &config.Config{MemoryWatcherModel: "w:1", MemoryEmbedModel: "e:1", OllamaBridgeModel: "b:1"}
	env := ollamaEnv(t, "", true, "w:1\ne:1\n")
	s := readiness.Build(
		readiness.Request{Axes: []readiness.Axis{readiness.AxisModelWatcher, readiness.AxisModelEmbed, readiness.AxisModelBridge}},
		ollamaReadinessAxes(cfg, env, "", nil),
	)
	for axis, want := range map[readiness.Axis]readiness.Verdict{
		readiness.AxisModelWatcher: readiness.VerdictReady,
		readiness.AxisModelEmbed:   readiness.VerdictReady,
		readiness.AxisModelBridge:  readiness.VerdictTodo, // configured, `ollama list` ran clean, not listed
	} {
		_, got, ok := s.AxisVerdict(axis)
		if !ok {
			t.Fatalf("%s missing from the snapshot", axis)
		}
		if got != want {
			t.Errorf("%s verdict = %q, want %q", axis, got, want)
		}
	}
}

// Diagnostics never create a sandbox to answer ollama.sandbox: with none
// present the axis is unverifiable + optional and names what would resolve it.
func TestOllamaSandboxAxisNeverCreatesASandbox(t *testing.T) {
	cfg := &config.Config{}
	env := ollamaEnv(t, "", true, "")
	s := readiness.Build(readiness.Request{Axes: []readiness.Axis{readiness.AxisOllamaSandbox}}, ollamaReadinessAxes(cfg, env, "", nil))
	c, _ := s.Checks(readiness.AxisOllamaSandbox)
	if c[0].Result() != readiness.VerdictUnverifiable || c[0].Req() != readiness.RequirementOptional {
		t.Fatalf("no sandbox => unverifiable+optional, got %q/%q", c[0].Result(), c[0].Req())
	}
	if c[0].Todo != "" {
		t.Error("an unverifiable check must not carry a repair command")
	}
	if !strings.Contains(c[0].Evidence, "pix run") {
		t.Errorf("evidence must name what makes it verifiable, got %q", c[0].Evidence)
	}
	if s.ExitCode() != readiness.ExitReady {
		t.Errorf("an optional unverifiable axis must not change the exit code, got %d", s.ExitCode())
	}
}

// Bind-address inference is remediation context on an already-probed failure,
// never a verdict of its own — and never a ready.
func TestBindInferenceNeverProducesReady(t *testing.T) {
	cfg := &config.Config{}
	env := ollamaEnv(t, "", true, "")
	no := false
	s := readiness.Build(readiness.Request{Axes: []readiness.Axis{readiness.AxisOllamaSandbox}}, ollamaReadinessAxes(cfg, env, "pix-demo", &no))
	c, _ := s.Checks(readiness.AxisOllamaSandbox)
	if c[0].Result() != readiness.VerdictTodo {
		t.Fatalf("a probed failure is a todo, got %q", c[0].Result())
	}
	if !strings.Contains(c[0].Detail, "loopback-only") {
		t.Errorf("loopback inference should add remediation context, got %q", c[0].Detail)
	}
	yes := true
	s2 := readiness.Build(readiness.Request{Axes: []readiness.Axis{readiness.AxisOllamaSandbox}}, ollamaReadinessAxes(cfg, env, "pix-demo", &yes))
	c2, _ := s2.Checks(readiness.AxisOllamaSandbox)
	if c2[0].Result() != readiness.VerdictReady {
		t.Fatalf("only a positive probe produces ready, got %q", c2[0].Result())
	}
}
