package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"pi-stack/host/config"
)

// fakeEnv builds a shellEnv from a set of present binaries, canned command
// output, and open ports, so runDoctor can be driven with no real sbx/ollama.
type fakeEnv struct {
	present map[string]bool   // binaries on PATH
	output  map[string]string // "cmd arg arg" -> combined output
	ports   map[int]bool      // open TCP ports
}

func (f fakeEnv) env() shellEnv {
	return shellEnv{
		lookPath: func(name string) (string, error) {
			if f.present[name] {
				return "/usr/bin/" + name, nil
			}
			return "", fmt.Errorf("exec: %q not found", name)
		},
		run: func(name string, args ...string) (string, error) {
			key := strings.Join(append([]string{name}, args...), " ")
			if out, ok := f.output[key]; ok {
				return out, nil
			}
			return "", fmt.Errorf("no fake output for %q", key)
		},
		dial: func(port int) bool { return f.ports[port] },
	}
}

func defaultCfg() *config.Config {
	c := &config.Config{}
	// apply defaults via Load's helper by round-tripping through the exported
	// fields the doctor reads.
	c.Services = []string{"memory", "gws"}
	c.MemoryWatcherModel = "gemma4"
	c.MemoryEmbedModel = "nomic-embed-text"
	return c
}

// TestDoctor_AllGreen: everything present -> verdict says all pass, no TODOs.
func TestDoctor_AllGreen(t *testing.T) {
	f := fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true, "gws": true},
		output: map[string]string{
			"sbx secret ls": "anthropic\nopenai\ngoogle\ngithub\n",
			"ollama list":   "NAME\ngemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":    "",
		},
		ports: map[int]bool{11434: true, 11435: true, 11441: true},
	}
	r := runDoctor(defaultCfg(), f.env())
	if got := len(r.todos()); got != 0 {
		t.Fatalf("expected 0 todos, got %d: %v", got, r.todos())
	}
	var buf bytes.Buffer
	r.services, r.mcp = defaultCfg().Services, nil
	r.render(&buf)
	out := buf.String()
	if !strings.Contains(out, "all checks pass") {
		t.Errorf("expected all-pass verdict, got:\n%s", out)
	}
	if strings.Contains(out, "TODO:") {
		t.Errorf("all-green report should have no TODO lines:\n%s", out)
	}
}

// TestDoctor_SbxAbsent: inside the sandbox sbx is gone -> must still run, emit
// provider TODOs, and note sbx is unavailable. This is the acceptance case.
func TestDoctor_SbxAbsent(t *testing.T) {
	f := fakeEnv{
		present: map[string]bool{}, // nothing installed
		output:  map[string]string{},
		ports:   map[int]bool{},
	}
	r := runDoctor(defaultCfg(), f.env())
	if !r.sbxAbsent {
		t.Error("expected sbxAbsent to be true when sbx not on PATH")
	}
	todos := r.todos()
	if len(todos) == 0 {
		t.Fatal("expected TODOs when nothing is set up")
	}
	// Provider TODOs must be present with the exact command grammar.
	joined := strings.Join(todos, "\n")
	for _, want := range []string{
		"sbx secret set -g anthropic",
		"sbx secret set -g github",
		"ollama pull gemma4",
		"ollama pull nomic-embed-text",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected TODO %q in %v", want, todos)
		}
	}

	var buf bytes.Buffer
	r.services, r.mcp = defaultCfg().Services, nil
	r.render(&buf)
	out := buf.String()
	if !strings.Contains(out, "outstanding") {
		t.Errorf("expected outstanding verdict, got:\n%s", out)
	}
	if !strings.Contains(out, "sbx not on PATH") {
		t.Errorf("expected sbx-absent note, got:\n%s", out)
	}
	if !strings.Contains(out, "TODO: sbx secret set -g anthropic") {
		t.Errorf("expected copy-pasteable provider TODO, got:\n%s", out)
	}
}

// TestDoctor_PartialModels: sbx keys set, ollama installed but only watcher
// pulled -> exactly one model TODO (embed), no provider TODOs.
func TestDoctor_PartialModels(t *testing.T) {
	f := fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true, "gws": true},
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"ollama list":   "gemma4:latest\n",
			"sbx mcp ls":    "",
		},
		ports: map[int]bool{11435: true, 11441: true},
	}
	r := runDoctor(defaultCfg(), f.env())
	todos := r.todos()
	if len(todos) != 1 || !strings.Contains(todos[0], "ollama pull nomic-embed-text") {
		t.Fatalf("expected exactly the embed-model TODO, got %v", todos)
	}
}

// TestDoctor_MCPRegistration: a configured MCP server not registered -> TODO.
func TestDoctor_MCPRegistration(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	f := fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true, "gws": true},
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"ollama list":   "gemma4\nnomic-embed-text\n",
			"sbx mcp ls":    "notion\n", // slack missing
		},
		ports: map[int]bool{11435: true, 11441: true},
	}
	r := runDoctor(cfg, f.env())
	found := false
	for _, c := range r.groups[len(r.groups)-1].checks {
		if c.label == "slack" && c.state == stateTODO {
			found = true
		}
	}
	if !found {
		t.Errorf("expected slack MCP TODO, groups=%v", r.groups)
	}

	// Now register it -> no MCP todo.
	f.output["sbx mcp ls"] = "notion\nslack\n"
	r = runDoctor(cfg, f.env())
	for _, c := range r.groups[len(r.groups)-1].checks {
		if c.label == "slack" && c.state == stateTODO {
			t.Errorf("registered slack should not be a TODO")
		}
	}
}

// TestGrepWord matches the Makefile's `grep -qw` semantics.
func TestGrepWord(t *testing.T) {
	if !grepWord("anthropic openai", "openai") {
		t.Error("should match whole word")
	}
	if grepWord("openaikey", "openai") {
		t.Error("should not match substring")
	}
	if !grepWord("a,b:c/d", "c") {
		t.Error("should split on punctuation")
	}
}

// TestModelPulled handles :tag suffixes.
func TestModelPulled(t *testing.T) {
	list := "NAME              ID\ngemma4:latest     abc\n"
	if !modelPulled(list, "gemma4") {
		t.Error("gemma4 should match gemma4:latest")
	}
	if modelPulled(list, "gemma") {
		t.Error("gemma should not match gemma4")
	}
}
