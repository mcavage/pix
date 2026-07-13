package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pi-stack/host/config"
)

// fakeEnv builds a shellEnv from a set of present binaries, canned command
// output, env vars, and open ports, so runDoctor can be driven with no real
// sbx/ollama/gog.
type fakeEnv struct {
	present  map[string]bool   // binaries on PATH
	output   map[string]string // "cmd arg arg" -> combined output
	envVars  map[string]string // environment variables
	ports    map[int]bool      // open TCP ports
	statFile map[string]bool   // files that "exist"
	files    map[string]string // file contents (for readFile)
	home     string            // fake home dir
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
		getenv:   func(name string) string { return f.envVars[name] },
		dial:     func(port int) bool { return f.ports[port] },
		statFile: func(path string) bool { return f.statFile[path] },
		readFile: func(path string) (string, error) {
			if s, ok := f.files[path]; ok {
				return s, nil
			}
			return "", fmt.Errorf("no fake file %q", path)
		},
		homeDir: func() string { return f.home },
	}
}

const gogAcct = "you@example.com"

// gogCfgFile / gogOpRefs are the fake $PI_STACK_CONFIG + resolved op-refs path
// the gog fixtures use: setting PI_STACK_CONFIG makes resolveOpRefs return
// gogOpRefs (its dir + op-refs.env), which gogHeadlessOK then probes with.
const gogCfgFile = "/fake/config/config.toml"
const gogOpRefs = "/fake/config/op-refs.env"

// gogGreen adds the fixtures that make the whole gog group green: gog + op on
// PATH, GOG_ACCOUNT set, interactive auth passing, the headless op-run probe
// returning a non-empty tool list, and gog registered with the gateway.
func gogGreen(f fakeEnv) fakeEnv {
	f.present["gog"] = true
	f.present["op"] = true
	if f.envVars == nil {
		f.envVars = map[string]string{}
	}
	if f.statFile == nil {
		f.statFile = map[string]bool{}
	}
	f.envVars["GOG_ACCOUNT"] = gogAcct
	f.envVars["PI_STACK_CONFIG"] = gogCfgFile // makes resolveOpRefs -> gogOpRefs
	f.statFile[gogOpRefs] = true
	f.output["gog --account "+gogAcct+" auth doctor --check"] = "ok"
	f.output["op run --env-file="+gogOpRefs+" -- gog --account "+gogAcct+" mcp --list-tools"] =
		"gmail_search\ncalendar_events\ndocs_get\n"
	return f
}

func defaultCfg() *config.Config {
	c := &config.Config{}
	// apply defaults via Load's helper by round-tripping through the exported
	// fields the doctor reads.
	c.Services = []string{"memory"}
	c.MemoryWatcherModel = "gemma4"
	c.MemoryEmbedModel = "nomic-embed-text"
	return c
}

// TestDoctor_AllGreen: everything present -> verdict says all pass, no TODOs.
func TestDoctor_AllGreen(t *testing.T) {
	f := gogGreen(fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		output: map[string]string{
			"sbx secret ls": "anthropic\nopenai\ngoogle\ngithub\n",
			"ollama list":   "NAME\ngemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":    "gog\n",
		},
		ports: map[int]bool{11434: true, 11435: true},
	})
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
// pulled -> exactly one model TODO (embed), no provider/gog TODOs.
func TestDoctor_PartialModels(t *testing.T) {
	f := gogGreen(fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"ollama list":   "gemma4:latest\n",
			"sbx mcp ls":    "gog\n",
		},
		ports: map[int]bool{11435: true},
	})
	r := runDoctor(defaultCfg(), f.env())
	todos := r.todos()
	if len(todos) != 1 || !strings.Contains(todos[0], "ollama pull nomic-embed-text") {
		t.Fatalf("expected exactly the embed-model TODO, got %v", todos)
	}
}

// TestDoctor_GogHeadlessTrap is THE footgun: interactive `gog auth doctor`
// passes but the gateway-equivalent headless op-run probe returns 0 tools. The
// account check must stay green while a distinct "headless spawn" TODO names the
// keyring/op-refs fix — doctor must NOT pass on `gog auth doctor` alone.
func TestDoctor_GogHeadlessTrap(t *testing.T) {
	f := fakeEnv{
		present: map[string]bool{"sbx": true, "gog": true, "op": true},
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"sbx mcp ls":    "gog\n",
			"gog --account " + gogAcct + " auth doctor --check": "ok",
			// headless probe returns an empty tool list -> the trap.
			"op run --env-file=" + gogOpRefs + " -- gog --account " + gogAcct + " mcp --list-tools": "",
		},
		envVars:  map[string]string{"GOG_ACCOUNT": gogAcct, "PI_STACK_CONFIG": gogCfgFile},
		statFile: map[string]bool{gogOpRefs: true},
		ports:    map[int]bool{11435: true},
	}
	r := runDoctor(defaultCfg(), f.env())
	var gog group
	for _, g := range r.groups {
		if strings.HasPrefix(g.title, "gog") {
			gog = g
		}
	}
	var acctOK, headTODO bool
	for _, c := range gog.checks {
		if c.label == "account" && c.state == stateOK {
			acctOK = true
		}
		if c.label == "headless spawn" && c.state == stateTODO &&
			strings.Contains(c.todo, "GOG_KEYRING_BACKEND=file") {
			headTODO = true
		}
	}
	if !acctOK {
		t.Errorf("interactive auth should read as OK, group=%+v", gog)
	}
	if !headTODO {
		t.Errorf("expected the headless-keyring TODO naming config/op-refs.env, group=%+v", gog)
	}
}

// TestDoctor_GogAccountUnset: gog installed but GOG_ACCOUNT unset -> a TODO to
// set it, and no crash (the account/headless probes are skipped).
func TestDoctor_GogAccountUnset(t *testing.T) {
	f := fakeEnv{
		present: map[string]bool{"gog": true},
		output:  map[string]string{},
		envVars: map[string]string{},
		ports:   map[int]bool{},
	}
	r := runDoctor(defaultCfg(), f.env())
	joined := strings.Join(r.todos(), "\n")
	if !strings.Contains(joined, "set gog_account") {
		t.Errorf("expected a gog_account TODO, got %v", r.todos())
	}
	// It must NOT report green: the account check carries the "cannot verify"
	// detail and stays a TODO (not stateOK).
	var buf bytes.Buffer
	r.services, r.mcp = defaultCfg().Services, nil
	r.render(&buf)
	if !strings.Contains(buf.String(), "cannot verify (GOG_ACCOUNT unset in env/config/local.mk)") {
		t.Errorf("expected a 'cannot verify' account detail, got:\n%s", buf.String())
	}
}

// TestResolveOpRefs: the op-refs path resolves to an ABSOLUTE, canonical
// location (here from $PI_STACK_CONFIG's dir) so doctor probes the same file the
// gateway registration uses, never a cwd-relative one.
func TestResolveOpRefs(t *testing.T) {
	f := fakeEnv{
		envVars:  map[string]string{"PI_STACK_CONFIG": "/etc/pi-stack/config.toml"},
		statFile: map[string]bool{"/etc/pi-stack/op-refs.env": true},
	}
	got := resolveOpRefs(f.env())
	if got != "/etc/pi-stack/op-refs.env" {
		t.Errorf("expected the PI_STACK_CONFIG-dir op-refs, got %q", got)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("resolved op-refs must be absolute, got %q", got)
	}
	// Home-dir fallback when nothing else exists.
	f2 := fakeEnv{
		envVars:  map[string]string{},
		statFile: map[string]bool{"/home/me/.config/pi-stack/op-refs.env": true},
		home:     "/home/me",
	}
	if got := resolveOpRefs(f2.env()); got != "/home/me/.config/pi-stack/op-refs.env" {
		t.Errorf("expected the home-dir op-refs fallback, got %q", got)
	}
}

// TestDoctor_GogTransparency: the gog group PRINTS which account + op-refs path
// it is verifying, plus the one-line 'must match make mcp-register' note, so a
// green result can't hide a mismatch with the gateway registration.
func TestDoctor_GogTransparency(t *testing.T) {
	f := gogGreen(fakeEnv{
		present: map[string]bool{"sbx": true},
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"sbx mcp ls":    "gog\n",
		},
		ports: map[int]bool{11435: true},
	})
	r := runDoctor(defaultCfg(), f.env())
	var buf bytes.Buffer
	r.services, r.mcp = defaultCfg().Services, []string{"gog"}
	r.render(&buf)
	out := buf.String()
	if !strings.Contains(out, "verifying") || !strings.Contains(out, gogAcct) || !strings.Contains(out, gogOpRefs) {
		t.Errorf("expected a transparency line naming account+op-refs, got:\n%s", out)
	}
	if !strings.Contains(out, "must match your `make mcp-register`") {
		t.Errorf("expected the must-match note, got:\n%s", out)
	}
}

// TestDoctor_GogAccountFromLocalMk: with config.toml gog_account AND $GOG_ACCOUNT
// both empty, doctor greps GOG_ACCOUNT out of a located config/local.mk (exactly
// what `make mcp-register` uses), so it no longer false-reports "cannot verify".
func TestDoctor_GogAccountFromLocalMk(t *testing.T) {
	f := gogGreen(fakeEnv{
		present: map[string]bool{"sbx": true},
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"sbx mcp ls":    "gog\n",
		},
		ports: map[int]bool{11435: true},
	})
	delete(f.envVars, "GOG_ACCOUNT") // force the local.mk path
	wd, _ := os.Getwd()
	mk := filepath.Join(wd, "config", "local.mk")
	f.statFile[filepath.Join(wd, "Makefile")] = true
	f.statFile[mk] = true
	f.files = map[string]string{mk: "# comment\nGOG_ACCOUNT ?= " + gogAcct + "\n"}
	r := runDoctor(defaultCfg(), f.env())
	joined := strings.Join(r.todos(), "\n")
	if strings.Contains(joined, "gog_account") || strings.Contains(joined, "GOG_ACCOUNT") {
		t.Errorf("account from config/local.mk should not TODO, got %v", r.todos())
	}
}

// TestDoctor_GogAccountFromConfig: the account is read from config.toml's
// gog_account (the Go-side source of truth) even with GOG_ACCOUNT absent from the
// env, so doctor no longer false-reports "unset" when make/config has it.
func TestDoctor_GogAccountFromConfig(t *testing.T) {
	f := gogGreen(fakeEnv{
		present: map[string]bool{"sbx": true},
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"sbx mcp ls":    "gog\n",
		},
		ports: map[int]bool{11435: true},
	})
	// Drop the env var; the account must come from config instead.
	delete(f.envVars, "GOG_ACCOUNT")
	cfg := defaultCfg()
	cfg.GogAccount = gogAcct
	r := runDoctor(cfg, f.env())
	joined := strings.Join(r.todos(), "\n")
	if strings.Contains(joined, "gog_account") || strings.Contains(joined, "GOG_ACCOUNT") {
		t.Errorf("account from config should not TODO, got %v", r.todos())
	}
}

// TestDoctor_GogRegistration: a fully-authed gog that is not registered with the
// gateway -> a `pi-stack mcp register` TODO on the gog check.
func TestDoctor_GogRegistration(t *testing.T) {
	f := gogGreen(fakeEnv{
		present: map[string]bool{"sbx": true},
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"sbx mcp ls":    "notion\n", // gog missing
		},
		ports: map[int]bool{11435: true},
	})
	r := runDoctor(defaultCfg(), f.env())
	var found bool
	for _, g := range r.groups {
		if !strings.HasPrefix(g.title, "gog") {
			continue
		}
		for _, c := range g.checks {
			if c.label == "gog" && c.state == stateTODO {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected an unregistered-gog TODO, groups=%+v", r.groups)
	}
}

// TestDoctor_MCPRegistration: a configured MCP server not registered -> TODO.
func TestDoctor_MCPRegistration(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	f := gogGreen(fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"ollama list":   "gemma4\nnomic-embed-text\n",
			"sbx mcp ls":    "notion\ngog\n", // slack missing
		},
		ports: map[int]bool{11435: true},
	})
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
