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

// bareGog / opWrappedGog are the two ways `pi-stack mcp register` actually wires
// gog (see mcp.go serverCmd/addArgs): a BARE `gog … mcp …` command when no
// op-refs.env is present (1Password is optional for gog), or that same command
// behind the `op run --env-file=<refs> -- …` wrapper when op-refs is present.
// Tests use these so the fixtures match a real registration rather than a
// stand-in binary.
func bareGog(acct string) string {
	return "gog --account " + acct +
		" --gmail-no-send --wrap-untrusted --readonly mcp --allow-tool read"
}
func opWrappedGog(refs, acct string) string {
	return "op run --env-file=" + refs + " -- " + bareGog(acct)
}

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

// gogConfirmed layers the sbx-registered-command fixtures on top of gogGreen so
// the gog group takes the HONEST confirmed path (doctor reads the registered
// command via `sbx mcp get gog` and probes THAT). Only this path is a real
// green ✓ — the best-effort reconstruction fallback (gogGreen alone) is now a
// TODO because it can't confirm what the gateway registered.
func gogConfirmed(f fakeEnv) fakeEnv {
	f = gogGreen(f)
	regCmd := opWrappedGog(gogOpRefs, gogAcct)
	f.output["sbx mcp get gog"] = "name: gog\ncommand: " + regCmd + "\n"
	f.output[regCmd+" --list-tools"] = "gmail_search\ncalendar_events\n"
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
// The gog group must take the confirmed-registered-command path (gogConfirmed):
// a best-effort fallback pass is no longer a green.
func TestDoctor_AllGreen(t *testing.T) {
	f := gogConfirmed(fakeEnv{
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
	f := gogConfirmed(fakeEnv{
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
	// The fallback (sbx exposes no registered command) must be labeled best-effort
	// so a pass can't masquerade as a confirmed registration.
	if !strings.Contains(out, "best-effort (sbx unavailable)") {
		t.Errorf("expected a best-effort fallback label, got:\n%s", out)
	}
}

// TestDoctor_GogRegisteredCommand is the HONEST path: sbx exposes the ACTUAL
// registered gog command, so doctor probes THAT exact command (with
// --list-tools) rather than reconstructing it from config. A non-empty tool
// list reads as a confirmed-green headless spawn.
func TestDoctor_GogRegisteredCommand(t *testing.T) {
	regCmd := opWrappedGog("/abs/config/op-refs.env", gogAcct)
	probeKey := regCmd + " --list-tools"
	f := fakeEnv{
		present: map[string]bool{"sbx": true, "op": true},
		output: map[string]string{
			"sbx secret ls":   "anthropic openai google github",
			"sbx mcp ls":      "gog\n",
			"sbx mcp get gog": "name: gog\ncommand: " + regCmd + "\n",
			probeKey:          "gmail_search\ncalendar_events\ndocs_get\n",
		},
		ports: map[int]bool{11435: true},
	}
	r := runDoctor(defaultCfg(), f.env())
	var gog group
	for _, g := range r.groups {
		if strings.HasPrefix(g.title, "gog") {
			gog = g
		}
	}
	var regShown, headOK bool
	for _, c := range gog.checks {
		if c.label == "registration" && strings.Contains(c.detail, "-- "+bareGog(gogAcct)) {
			regShown = true
		}
		if c.label == "headless spawn" && c.state == stateOK {
			headOK = true
		}
	}
	if !regShown {
		t.Errorf("expected doctor to name the sbx-registered command, group=%+v", gog)
	}
	if !headOK {
		t.Errorf("expected a confirmed headless spawn from probing the registered command, group=%+v", gog)
	}
	// No fallback/reconstruction lines when the honest path fires.
	for _, c := range gog.checks {
		if strings.Contains(c.detail, "best-effort") {
			t.Errorf("honest path should not print best-effort fallback lines, got %q", c.detail)
		}
	}
}

// TestDoctor_GogFallbackUnconfirmedIsTODO: sbx lists gog but `get` is
// partial/unsupported (command with no op-run tail) and `ls -o json` is
// unparseable, so doctor CANNOT confirm the registered command. Even though the
// reconstructed best-effort probe would pass (gogGreen), the gog headless result
// MUST be a TODO (verdict NOT all-clear), never a silent green.
func TestDoctor_GogFallbackUnconfirmedIsTODO(t *testing.T) {
	f := gogGreen(fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		output: map[string]string{
			"sbx secret ls":      "anthropic openai google github",
			"ollama list":        "gemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":         "gog\n",
			"sbx mcp get gog":    "name: gog\ncommand: op\n", // partial: no `-- <cmd>` tail
			"sbx mcp ls -o json": "not json{",                // unparseable
		},
		ports: map[int]bool{11434: true, 11435: true},
	})
	r := runDoctor(defaultCfg(), f.env())
	// The headless spawn must be a TODO whose detail says it could not confirm.
	var headTODO bool
	for _, g := range r.groups {
		if !strings.HasPrefix(g.title, "gog") {
			continue
		}
		for _, c := range g.checks {
			if c.label == "headless spawn" && c.state == stateTODO &&
				strings.Contains(c.detail, "could not confirm the sbx-registered command") {
				headTODO = true
			}
		}
	}
	if !headTODO {
		t.Fatalf("expected an unconfirmed-fallback headless TODO, groups=%+v", r.groups)
	}
	// Verdict must NOT be all-clear.
	var buf bytes.Buffer
	r.services, r.mcp = defaultCfg().Services, []string{"gog"}
	r.render(&buf)
	out := buf.String()
	if strings.Contains(out, "all checks pass") {
		t.Errorf("unconfirmed fallback must not report all-clear, got:\n%s", out)
	}
	if !strings.Contains(out, "TODO: confirm the registered gog command") {
		t.Errorf("expected a copy-pasteable confirm-command TODO, got:\n%s", out)
	}
}

// TestDoctor_GogRegisteredCommandLineFallsThrough: `sbx mcp get gog` emits only
// a partial `command:` line (no `-- <cmd>` tail), so the line parser must FALL
// THROUGH to the JSON form, which carries the full argv and confirms green.
func TestDoctor_GogRegisteredCommandLineFallsThrough(t *testing.T) {
	probeKey := opWrappedGog("/abs/config/op-refs.env", gogAcct) + " --list-tools"
	f := fakeEnv{
		present: map[string]bool{"sbx": true},
		output: map[string]string{
			"sbx secret ls":      "anthropic openai google github",
			"sbx mcp ls":         "gog\n",
			"sbx mcp get gog":    "name: gog\ncommand: op\n", // partial line -> fall through
			"sbx mcp ls -o json": `[{"name":"gog","command":"op","args":["run","--env-file=/abs/config/op-refs.env","--","gog","--account","you@example.com","--gmail-no-send","--wrap-untrusted","--readonly","mcp","--allow-tool","read"]}]`,
			probeKey:             "gmail_search\n",
		},
		ports: map[int]bool{11435: true},
	}
	r := runDoctor(defaultCfg(), f.env())
	var headOK bool
	for _, g := range r.groups {
		if !strings.HasPrefix(g.title, "gog") {
			continue
		}
		for _, c := range g.checks {
			if c.label == "headless spawn" && c.state == stateOK {
				headOK = true
			}
		}
	}
	if !headOK {
		t.Errorf("expected line form to fall through to JSON and confirm green, groups=%+v", r.groups)
	}
}

// TestDoctor_GogRegisteredCommandJSON: sbx exposes the registration only via
// `sbx mcp ls -o json`; doctor parses command+args and probes it.
func TestDoctor_GogRegisteredCommandJSON(t *testing.T) {
	probeKey := opWrappedGog("/abs/config/op-refs.env", gogAcct) + " --list-tools"
	f := fakeEnv{
		present: map[string]bool{"sbx": true},
		output: map[string]string{
			"sbx secret ls":      "anthropic openai google github",
			"sbx mcp ls":         "gog\n",
			"sbx mcp ls -o json": `[{"name":"gog","command":"op","args":["run","--env-file=/abs/config/op-refs.env","--","gog","--account","you@example.com","--gmail-no-send","--wrap-untrusted","--readonly","mcp","--allow-tool","read"]}]`,
			probeKey:             "gmail_search\n",
		},
		ports: map[int]bool{11435: true},
	}
	r := runDoctor(defaultCfg(), f.env())
	var headOK bool
	for _, g := range r.groups {
		if !strings.HasPrefix(g.title, "gog") {
			continue
		}
		for _, c := range g.checks {
			if c.label == "headless spawn" && c.state == stateOK {
				headOK = true
			}
		}
	}
	if !headOK {
		t.Errorf("expected a confirmed headless spawn from the JSON-parsed registration, groups=%+v", r.groups)
	}
}

// TestDoctor_GogBareRegisteredCommand: a system-keychain macOS user with no
// op-refs.env registers gog DIRECTLY (bare `gog … mcp …`, no op-run wrapper).
// doctor must recognize that as a confirmed registration, name it, probe it with
// --list-tools, and read a non-empty tool list as a confirmed-green headless
// spawn — same as the op-wrapped form.
func TestDoctor_GogBareRegisteredCommand(t *testing.T) {
	regCmd := bareGog(gogAcct)
	probeKey := regCmd + " --list-tools"
	f := fakeEnv{
		present: map[string]bool{"sbx": true, "gog": true},
		output: map[string]string{
			"sbx secret ls":   "anthropic openai google github",
			"sbx mcp ls":      "gog\n",
			"sbx mcp get gog": "name: gog\ncommand: " + regCmd + "\n",
			probeKey:          "gmail_search\ncalendar_events\ndocs_get\n",
		},
		ports: map[int]bool{11435: true},
	}
	r := runDoctor(defaultCfg(), f.env())
	var gog group
	for _, g := range r.groups {
		if strings.HasPrefix(g.title, "gog") {
			gog = g
		}
	}
	var regShown, headOK bool
	for _, c := range gog.checks {
		if c.label == "registration" && strings.Contains(c.detail, regCmd) {
			regShown = true
		}
		if c.label == "headless spawn" && c.state == stateOK {
			headOK = true
		}
	}
	if !regShown {
		t.Errorf("expected doctor to name the bare sbx-registered gog command, group=%+v", gog)
	}
	if !headOK {
		t.Errorf("expected a confirmed headless spawn from probing the bare registered command, group=%+v", gog)
	}
	// A confirmed bare registration must NOT print the best-effort fallback lines.
	for _, c := range gog.checks {
		if strings.Contains(c.detail, "best-effort") {
			t.Errorf("confirmed bare path should not print best-effort fallback lines, got %q", c.detail)
		}
	}
}

// TestDoctor_GogBareRegisteredCommandJSON: same bare (no op-run) registration,
// but surfaced only via `sbx mcp ls -o json` (command="gog", args=[…]). doctor
// must parse command+args, recognize it as a valid gog registration, and probe
// it to a confirmed green.
func TestDoctor_GogBareRegisteredCommandJSON(t *testing.T) {
	probeKey := bareGog(gogAcct) + " --list-tools"
	f := fakeEnv{
		present: map[string]bool{"sbx": true, "gog": true},
		output: map[string]string{
			"sbx secret ls":      "anthropic openai google github",
			"sbx mcp ls":         "gog\n",
			"sbx mcp ls -o json": `[{"name":"gog","command":"gog","args":["--account","you@example.com","--gmail-no-send","--wrap-untrusted","--readonly","mcp","--allow-tool","read"]}]`,
			probeKey:             "gmail_search\n",
		},
		ports: map[int]bool{11435: true},
	}
	r := runDoctor(defaultCfg(), f.env())
	var headOK bool
	for _, g := range r.groups {
		if !strings.HasPrefix(g.title, "gog") {
			continue
		}
		for _, c := range g.checks {
			if c.label == "headless spawn" && c.state == stateOK {
				headOK = true
			}
		}
	}
	if !headOK {
		t.Errorf("expected a confirmed headless spawn from the bare JSON-parsed registration, groups=%+v", r.groups)
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
