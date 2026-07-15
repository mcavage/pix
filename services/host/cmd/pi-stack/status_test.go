package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"pi-stack/host/config"
)

// fakeStatusEnv builds a shellEnv where memory is up, knowledge down, sbx lists
// two boxes and reports two secrets set.
func fakeStatusEnv() shellEnv {
	return shellEnv{
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		run: func(name string, args ...string) (string, error) {
			if name == "sbx" && len(args) >= 1 && args[0] == "secret" {
				return "anthropic\nopenai\n", nil
			}
			if name == "sbx" && len(args) >= 1 && args[0] == "ls" {
				return "NAME STATUS\npi-stack-myrepo running\npi-stack-scratch stopped\nother-box running\n", nil
			}
			return "", nil
		},
		dial:     func(port int) bool { return port == memoryPortDefault },
		statFile: func(string) bool { return false },
	}
}

func TestGatherStatus(t *testing.T) {
	cfg := &config.Config{MCP: []string{"gog"}, KnowledgeBundles: []string{"/kb"}}
	st := gatherStatus(cfg, "default", fakeStatusEnv())

	if !st.Memory {
		t.Error("memory should be up")
	}
	if st.Knowledge {
		t.Error("knowledge should be down")
	}
	if !st.Providers["anthropic"] || !st.Providers["openai"] {
		t.Errorf("providers = %v, want anthropic+openai set", st.Providers)
	}
	if st.Providers["google"] || st.Providers["github"] {
		t.Errorf("providers = %v, want google+github unset", st.Providers)
	}
	// google + github missing -> two todos.
	if len(st.Todos) != 2 {
		t.Errorf("todos = %v, want 2 (google, github)", st.Todos)
	}
	// Only pi-stack-* sandboxes, "other-box" filtered out.
	if len(st.Sandboxes) != 2 {
		t.Errorf("sandboxes = %v, want 2 pi-stack boxes", st.Sandboxes)
	}
	for _, s := range st.Sandboxes {
		if !strings.HasPrefix(s.Name, "pi-stack-") {
			t.Errorf("leaked non-pi-stack sandbox: %s", s.Name)
		}
	}
}

func TestRenderStatusHuman(t *testing.T) {
	cfg := &config.Config{MCP: []string{"gog"}, KnowledgeBundles: []string{"/kb"}}
	var out bytes.Buffer
	renderStatus(cfg, "default", fakeStatusEnv(), &out, false)
	s := out.String()
	for _, want := range []string{"pi-stack", "services", "memory ✓", "knowledge ✗", "profile: default", "outstanding"} {
		if !strings.Contains(s, want) {
			t.Errorf("status output missing %q:\n%s", want, s)
		}
	}
}

func TestRenderStatusJSON(t *testing.T) {
	cfg := &config.Config{MCP: []string{"gog"}}
	var out bytes.Buffer
	renderStatus(cfg, "default", fakeStatusEnv(), &out, true)
	var st statusReport
	if err := json.Unmarshal(out.Bytes(), &st); err != nil {
		t.Fatalf("status --json invalid: %v\n%s", err, out.String())
	}
	if st.Profile != "default" {
		t.Errorf("profile = %q, want default", st.Profile)
	}
}

func TestParseSandboxes(t *testing.T) {
	out := parseSandboxes("NAME STATUS\npi-stack-a running\nfoo bar\npi-stack-b stopped\n")
	if len(out) != 2 {
		t.Fatalf("got %d, want 2: %v", len(out), out)
	}
	if out[0].Name != "pi-stack-a" || out[0].State != "running" {
		t.Errorf("out[0] = %+v", out[0])
	}
}
