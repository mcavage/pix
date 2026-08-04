package doctor

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"pix/host/cli"
	"pix/host/hostenv"
	"pix/host/readiness"
	"pix/host/readiness/axis"
	"pix/host/secret"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/rpc"
	"pix/host/sys/systest"
	"pix/host/workspace"
)

// fakeEnv builds a hostenv.Env from a set of present binaries, canned command
// output, env vars, and open ports, so RunDoctor can be driven with no real
// sbx/ollama/gog.
type fakeEnv struct {
	present  map[string]bool        // binaries on PATH
	output   map[string]string      // "cmd arg arg" -> combined output
	envVars  map[string]string      // environment variables
	ports    map[int]bool           // open TCP ports
	statFile map[string]bool        // files that "exist"
	files    map[string]string      // file contents (for readFile)
	modes    map[string]os.FileMode // path -> mode bits (for fileMode)
	home     string                 // fake home dir
	hostBin  string                 // canonical pix-host path ("" = unresolvable)
	// identityProbe fakes the memory/knowledge `identity` JSON-RPC answer a
	// service axis needs before it may render ready (readiness_service.go).
	// Nil by default: a fixture that dials a service port "up" without also
	// faking its identity gets an honest unverifiable, never a real network
	// call and never a false ready. See identityFake / memGreen.
	identityProbe axis.IdentityProber
}

func (f fakeEnv) env() hostenv.Env {
	return hostenv.Env{System: &systest.Fake{LookPathFn: func(name string) (string, error) {
		if f.present[name] {
			return "/usr/bin/" + name, nil
		}
		return "", fmt.Errorf("exec: %q not found", name)
	}, RunFn: func(name string, args ...string) (string, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		if out, ok := f.output[key]; ok {
			return out, nil
		}
		return "", fmt.Errorf("no fake output for %q", key)
	}, GetenvFn: func(name string) string { return f.envVars[name] }, DialLocalFn: func(port int) bool { return f.ports[port] }, IsFileFn: func(path string) bool { return f.statFile[path] }, ReadFileFn: func(path string) (string, error) {
		if s, ok := f.files[path]; ok {
			return s, nil
		}
		// An undeclared file means ABSENT, not an I/O error. The distinction was
		// invisible while a nil readFile seam skipped the read entirely; now that
		// the seam is always present, every caller reaches it, and they all
		// already handle os.ErrNotExist.
		return "", os.ErrNotExist
	}, HomeDirFn: func() string { return f.home }, ModeFn: func(path string) (os.FileMode, bool) {
		if m, ok := f.modes[path]; ok {
			return m, true
		}
		return 0, false
	}}, HostBinary: func() (string, error) {
		if f.hostBin != "" {
			return f.hostBin, nil
		}
		return "", fmt.Errorf("pix-host not found")
	}, IdentityProbe: f.identityProbe}
}

// identityFake builds an axis.IdentityProber from a fixed port->result map: any
// port not in the map answers with an error, matching a real daemon that
// simply isn't there. Ready results default Version to "" so fixtures never
// have to track the launcher's build-time version string.
func identityFake(results map[int]axis.ServiceIdentityResult) axis.IdentityProber {
	return func(port int) (axis.ServiceIdentityResult, error) {
		if r, ok := results[port]; ok {
			return r, nil
		}
		return axis.ServiceIdentityResult{}, fmt.Errorf("no fake identity for port %d", port)
	}
}

// memGreen fakes a healthy, correctly-identified memory daemon on :11435 —
// the identity-probe counterpart to dialing the port "up": without this, a
// fixture that merely opens the port renders memory unverifiable, never
// ready (readiness_service.go never derives ready from a dial alone).
func memGreen(f fakeEnv) fakeEnv {
	f.identityProbe = identityFake(map[int]axis.ServiceIdentityResult{
		11435: {Name: rpc.MemoryName, Ready: true},
	})
	return f
}

const gogAcct = "you@example.com"

// gogCfgFile / gogOpRefs are the fake $PIX_CONFIG + resolved op-refs path
// the gog fixtures use: setting PIX_CONFIG makes resolveOpRefs return
// gogOpRefs (its dir + op-refs.env), which GogHeadlessOK then probes with.
const gogCfgFile = "/fake/config/config.toml"
const gogOpRefs = "/fake/config/op-refs.env"

// gogGreen / gogConfirmed used to layer elaborate headless-spawn/hardened-flag
// probe fixtures onto a fakeEnv (gog + op on PATH, GOG_ACCOUNT, a reconstructed
// --list-tools probe, a confirmed `sbx mcp get google-workspace` registration).
// That probing was retired with the built-in `pix gworkspace setup` wizard
// (workflow/gworkspace, deleted) — the gog group now renders the same two
// facts every other MCP server's group does (registration + attachment, see
// gog.go), neither of which reads gog/op PATH presence or any of those
// fixtures. Both are kept as identity no-ops rather than deleted so the many
// call sites below (which use them as readable "this env has gog available"
// markers) don't need a mechanical rewrite for a distinction that no longer
// exists in production.
func gogGreen(f fakeEnv) fakeEnv     { return f }
func gogConfirmed(f fakeEnv) fakeEnv { return f }

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
	f := memGreen(gogConfirmed(fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		output: map[string]string{
			"sbx secret ls": "anthropic\nopenai\ngoogle\ngithub\n",
			"ollama list":   "NAME\ngemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":    "google-workspace\n",
		},
		ports: map[int]bool{11434: true, 11435: true},
	}))
	r := RunDoctor(defaultCfg(), f.env())
	if got := len(r.Todos()); got != 0 {
		t.Fatalf("expected 0 todos, got %d: %v", got, r.Todos())
	}
	var buf bytes.Buffer
	r.Services, r.MCP = defaultCfg().Services, nil
	r.Render(&buf, false, Hints())
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
	r := RunDoctor(defaultCfg(), f.env())
	if !r.SbxAbsent {
		t.Error("expected sbxAbsent to be true when sbx not on PATH")
	}
	todos := r.Todos()
	if len(todos) == 0 {
		t.Fatal("expected TODOs when nothing is set up")
	}
	// The provider group can no longer VERIFY anything with sbx absent, so it
	// must not surface a provider fix command — only the still-verifiable
	// ollama TODOs remain.
	joined := strings.Join(todos, "\n")
	for _, want := range []string{
		"install ollama",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected TODO %q in %v", want, todos)
		}
	}
	if strings.Contains(joined, "sbx secret set -g") {
		t.Errorf("provider check must not claim a verified failure when sbx is absent, got %v", todos)
	}
	if strings.Contains(joined, "ollama pull") {
		t.Errorf("no pull TODO may be offered while ollama itself is absent, got %v", todos)
	}

	// The provider group's core check must degrade to unverifiable, not block.
	prov := r.Groups[0]
	if prov.Title != "Inference / credentials (proxy-injected, never in the VM)" {
		t.Fatalf("expected the providers group first, got %q", prov.Title)
	}
	core := prov.Checks[0]
	if core.Label != "model key" || core.Req() != readiness.RequirementCore || core.Result() != readiness.VerdictUnverifiable {
		t.Errorf("expected an unverifiable core model-key check, got %+v", core)
	}
	if r.Blocking() {
		t.Error("an unverifiable core check must never block")
	}

	var buf bytes.Buffer
	r.Services, r.MCP = defaultCfg().Services, nil
	r.Render(&buf, false, Hints())
	out := buf.String()
	if !strings.Contains(out, "outstanding") {
		t.Errorf("expected outstanding verdict, got:\n%s", out)
	}
	if !strings.Contains(out, "sbx not on PATH") {
		t.Errorf("expected sbx-absent note, got:\n%s", out)
	}
}

// TestDoctor_PartialModels: sbx keys set, ollama installed but only watcher
// pulled -> exactly one model TODO (embed), no provider/gog TODOs.
func TestDoctor_PartialModels(t *testing.T) {
	f := memGreen(gogConfirmed(fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"ollama list":   "gemma4:latest\n",
			"sbx mcp ls":    "google-workspace\n",
		},
		ports: map[int]bool{11435: true},
	}))
	r := RunDoctor(defaultCfg(), f.env())
	todos := r.Todos()
	if len(todos) != 1 || !strings.Contains(todos[0], "ollama pull nomic-embed-text") {
		t.Fatalf("expected exactly the embed-model TODO, got %v", todos)
	}
}

// TestDoctor_GogAttachDespiteMissingExecutable pins closure finding #2:
// receipt-backed attachment reporting must never be skipped just because the
// gog executable is missing from PATH (and sbx exposes no readable
// registered command). The registration check and gogAttachCheck must still
// be emitted — and read ready off a valid preload receipt — even though the
// executable/hardened/tools checks short-circuit on their own.
func TestDoctor_GogAttachDespiteMissingExecutable(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{config.GWServerName}
	const ws = "/home/u/proj"
	const box = "pix-proj"
	f := fakeEnv{
		present: map[string]bool{"sbx": true}, // gog NOT on PATH
		output: map[string]string{
			"sbx ls": box + "  running\n",
			// no `sbx mcp get google-workspace` / `sbx mcp ls -o json` fixture -> RegisteredGogCommand
			// returns (nil,false): the registered command is unreadable.
		},
	}
	env := f.env()
	systest.Of(env.System).GetwdFn = func() (string, error) { return ws, nil }
	stateDir := t.TempDir()
	systest.Of(env.System).StateDirFn = func() (string, error) { return stateDir, nil }
	if err := workspace.WriteCreateReceipt(stateDir, box, ws, []string{config.GWServerName}, receiptClock); err != nil {
		t.Fatal(err)
	}
	ctx := resolveMCPSandboxContext(env)
	if ctx.mode != mcpAttachReceipt {
		t.Fatalf("expected a receipt sandbox context, got mode=%v", ctx.mode)
	}
	g := gogGroup(cfg, env, "google-workspace\n", true, true, ctx)

	reg := findCheck(t, g, config.GWServerName)
	if reg.Result() != readiness.VerdictReady {
		t.Errorf("registration check must still be emitted and ready: %+v", reg)
	}
	attach := findCheck(t, g, config.GWServerName+" attachment")
	if attach.Result() != readiness.VerdictReady || !strings.Contains(attach.Evidence, "preloaded by pix at create") {
		t.Errorf("attach check must be emitted and ready despite the missing gog executable: %+v", attach)
	}
}

// TestResolveOpRefs: the op-refs path resolves to an ABSOLUTE, canonical
// location (here from $PIX_CONFIG's dir) so doctor probes the same file the
// gateway registration uses, never a cwd-relative one.
func TestResolveOpRefs(t *testing.T) {
	f := fakeEnv{
		envVars:  map[string]string{"PIX_CONFIG": "/etc/pix/config.toml"},
		statFile: map[string]bool{"/etc/pix/op-refs.env": true},
	}
	got := secret.FindOpRefs(f.env())
	if got != "/etc/pix/op-refs.env" {
		t.Errorf("expected the PIX_CONFIG-dir op-refs, got %q", got)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("resolved op-refs must be absolute, got %q", got)
	}
	// Home-dir fallback when nothing else exists.
	f2 := fakeEnv{
		envVars:  map[string]string{},
		statFile: map[string]bool{"/home/me/.config/pix/op-refs.env": true},
		home:     "/home/me",
	}
	if got := secret.FindOpRefs(f2.env()); got != "/home/me/.config/pix/op-refs.env" {
		t.Errorf("expected the home-dir op-refs fallback, got %q", got)
	}
}

// TestDoctor_RegisteredCommandNeverLeaksSecret covers F3 end-to-end: a full
// doctor run with a legacy gog registration that carries a pasted secret must
// not echo that secret in ANY rendered group.
func TestDoctor_RegisteredCommandNeverLeaksSecret(t *testing.T) {
	const secret = "SEKRET-DO-NOT-PRINT"
	regCmd := "gog --account " + gogAcct + " --client-secret " + secret + " mcp"
	f := fakeEnv{
		present: map[string]bool{"sbx": true, "gog": true},
		output: map[string]string{
			"sbx secret ls":                "anthropic openai google github",
			"sbx mcp ls":                   "google-workspace\n",
			"sbx mcp get google-workspace": "name: gog\ncommand: " + regCmd + "\n",
		},
		ports: map[int]bool{11435: true},
	}
	r := RunDoctor(defaultCfg(), f.env())
	for _, g := range r.Groups {
		for _, c := range g.Checks {
			if strings.Contains(c.Detail, secret) || strings.Contains(c.Todo, secret) {
				t.Errorf("doctor leaked the pasted secret in group %q: detail=%q todo=%q", g.Title, c.Detail, c.Todo)
			}
		}
	}
}

// TestDoctor_SecretsGroupShortLiteralFlagged covers F4: a short, NOT-secret-
// shaped literal in op-refs.env still produces a TODO in the Secrets group and
// its value is never printed.
func TestDoctor_SecretsGroupShortLiteralFlagged(t *testing.T) {
	const val = "correcthorsebattery"
	f := fakeEnv{
		present: map[string]bool{"op": true},
		envVars: map[string]string{"PIX_CONFIG": gogCfgFile},
		files:   map[string]string{gogOpRefs: "SLACK_TOKEN=" + val + "\n"},
	}
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	g := secretsGroup(cfg, f.env())
	var found bool
	for _, c := range g.Checks {
		if strings.Contains(c.Detail, val) {
			t.Errorf("secrets group LEAKED the literal value: %q", c.Detail)
		}
		if c.Label == "SLACK_TOKEN" {
			if c.State() != readiness.StateTODO {
				t.Errorf("SLACK_TOKEN state = %v, want readiness.StateTODO", c.State())
			}
			if !strings.Contains(c.Detail, "not an op:// ref") {
				t.Errorf("SLACK_TOKEN detail should flag refs-only: %q", c.Detail)
			}
			found = true
		}
	}
	if !found {
		t.Errorf("expected a SLACK_TOKEN check in the Secrets readiness.Group, readiness.Group=%+v", g)
	}
}

// TestDoctor_GogRegistration: a fully-authed gog that is not registered with the
// gateway -> a `pix mcp register` TODO on the gog check.
func TestDoctor_GogRegistration(t *testing.T) {
	f := gogGreen(fakeEnv{
		present: map[string]bool{"sbx": true},
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"sbx mcp ls":    "notion\n", // gog missing
		},
		ports: map[int]bool{11435: true},
	})
	r := RunDoctor(defaultCfg(), f.env())
	var found bool
	for _, g := range r.Groups {
		if !strings.HasPrefix(g.Title, "Google Workspace") {
			continue
		}
		for _, c := range g.Checks {
			if c.Label == config.GWServerName && c.State() == readiness.StateTODO {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected an unregistered Google Workspace TODO, groups=%+v", r.Groups)
	}
}

// TestDoctor_MCPRegistration: a configured MCP server not registered -> TODO.
func TestDoctor_MCPRegistration(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	f := gogGreen(fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		hostBin: "/usr/local/bin/pix-host",
		output: map[string]string{
			"sbx secret ls":                      "anthropic openai google github",
			"ollama list":                        "gemma4\nnomic-embed-text\n",
			"sbx mcp ls":                         "notion\ngoogle-workspace\n", // slack missing
			"/usr/local/bin/pix-host mcp --list": "slack\n",
		},
		ports: map[int]bool{11435: true},
	})
	r := RunDoctor(cfg, f.env())
	found := false
	for _, c := range r.Groups[len(r.Groups)-1].Checks {
		if c.Label == "slack" && c.State() == readiness.StateTODO {
			found = true
		}
	}
	if !found {
		t.Errorf("expected slack MCP TODO, groups=%v", r.Groups)
	}

	// Now register it -> no MCP todo.
	f.output["sbx mcp ls"] = "notion\nslack\n"
	r = RunDoctor(cfg, f.env())
	for _, c := range r.Groups[len(r.Groups)-1].Checks {
		if c.Label == "slack" && c.State() == readiness.StateTODO {
			t.Errorf("registered slack should not be a TODO")
		}
	}
}

// TestDoctor_MCPToolProbe is the generalized honest probe: a NON-gog configured
// server (slack) that is registered gets its ACTUAL registered command read via
// sbx and probed with --list-tools, so the readout reports the real tool count
// ("registered, spawns N tools"), not just "registered".
func TestDoctor_MCPToolProbe(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	regCmd := "/usr/local/bin/pix-host mcp slack"
	f := gogGreen(fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		hostBin: "/usr/local/bin/pix-host",
		output: map[string]string{
			"sbx secret ls":                      "anthropic openai google github",
			"ollama list":                        "gemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":                         "google-workspace\nslack\n",
			"sbx mcp get slack":                  "name: slack\ncommand: " + regCmd + "\n",
			regCmd + " --list-tools":             "slack_search\nslack_post\nslack_channels\n",
			"/usr/local/bin/pix-host mcp --list": "slack\n",
		},
		ports: map[int]bool{11434: true, 11435: true},
	})
	r := RunDoctor(cfg, f.env())
	// The generic mcp group is last; slack must read as a real tool count.
	var found bool
	for _, c := range r.Groups[len(r.Groups)-1].Checks {
		if c.Label == "slack" && c.State() == readiness.StateOK && strings.Contains(c.Detail, "spawns 3 tools") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected slack to report a real tool count, readiness.Group=%+v", r.Groups[len(r.Groups)-1])
	}
}

// TestDoctor_MCPToolProbeZero: a registered server whose spawned command returns
// 0 tools is a TODO (the generalized headless-creds trap), not a silent green.
func TestDoctor_MCPToolProbeZero(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	regCmd := "/usr/local/bin/pix-host mcp slack"
	f := gogGreen(fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		hostBin: "/usr/local/bin/pix-host",
		output: map[string]string{
			"sbx secret ls":                      "anthropic openai google github",
			"ollama list":                        "gemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":                         "google-workspace\nslack\n",
			"sbx mcp get slack":                  "name: slack\ncommand: " + regCmd + "\n",
			regCmd + " --list-tools":             "", // spawns but returns 0 tools
			"/usr/local/bin/pix-host mcp --list": "slack\n",
		},
		ports: map[int]bool{11434: true, 11435: true},
	})
	r := RunDoctor(cfg, f.env())
	var todo bool
	for _, c := range r.Groups[len(r.Groups)-1].Checks {
		if c.Label == "slack" && c.State() == readiness.StateTODO && strings.Contains(c.Detail, "0 tools") {
			todo = true
		}
	}
	if !todo {
		t.Errorf("expected a 0-tools TODO for slack, group=%+v", r.Groups[len(r.Groups)-1])
	}
}

// TestDoctor_MCPUnrecognizedCommand is the probe-safety gate: a registered
// server whose command is NOT a recognized shape (not gog, not the canonical
// `pix-host mcp <name>`) must NOT be exec'd. The check reports the
// confirmed registration but stays UNVERIFIABLE (no false health claim) with
// an explicit "never executed" note, and the fake run PANICS if doctor ever
// tries to exec the untrusted command — proving it was never run.
func TestDoctor_MCPUnrecognizedCommand(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"evil"}
	f := gogConfirmed(fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		hostBin: "/usr/local/bin/pix-host",
		output: map[string]string{
			"sbx secret ls":                      "anthropic openai google github",
			"ollama list":                        "gemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":                         "google-workspace\nevil\n",
			"sbx mcp get evil":                   "name: evil\ncommand: /bin/rm -rf /\n",
			"/usr/local/bin/pix-host mcp --list": "evil\n",
		},
		ports: map[int]bool{11434: true, 11435: true},
	})
	// Wrap the fake run so an attempt to exec the untrusted command fails loudly.
	env := f.env()
	inner := systest.Of(env.System).RunFn
	systest.Of(env.System).RunFn = func(name string, args ...string) (string, error) {
		if name == "/bin/rm" {
			t.Fatalf("doctor exec'd an unrecognized registered command: %s %v", name, args)
		}
		return inner(name, args...)
	}
	r := RunDoctor(cfg, env)
	var found bool
	for _, c := range r.Groups[len(r.Groups)-1].Checks {
		if c.Label == "evil" && c.State() == readiness.StateWarn &&
			strings.Contains(c.Detail, "never executed") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected evil to report a skipped probe, readiness.Group=%+v", r.Groups[len(r.Groups)-1])
	}
}

// TestDoctor_GogTodoOnce is the duplicate-TODO gate: gog is UNREGISTERED and
// also present in cfg.MCP. The dedicated gog group owns gog's registration TODO;
// the generic mcp group must SKIP gog, and report.todos() dedupes regardless, so
// `pix mcp register` appears AT MOST ONCE.
func TestDoctor_GogTodoOnce(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{config.GWServerName}
	f := gogGreen(fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"ollama list":   "gemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":    "notion\n", // gog NOT registered
		},
		ports: map[int]bool{11434: true, 11435: true},
	})
	r := RunDoctor(cfg, f.env())
	n := 0
	for _, tdo := range r.Todos() {
		if tdo == "pix mcp register" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("expected `pix mcp register` exactly once, got %d: %v", n, r.Todos())
	}
	// The generic mcp group must not carry a gog check at all.
	for _, c := range r.Groups[len(r.Groups)-1].Checks {
		if c.Label == config.GWServerName {
			t.Errorf("generic mcp group should skip gog, got readiness.Check %+v", c)
		}
	}
}

// TestDoctorTodosDedup proves report.todos() drops exact-duplicate commands
// while preserving first-occurrence order.
func TestDoctorTodosDedup(t *testing.T) {
	r := &readiness.Report{Groups: []readiness.Group{
		{Checks: []readiness.Check{{Verdict: readiness.VerdictTodo, Todo: "a"}, {Verdict: readiness.VerdictTodo, Todo: "b"}}},
		{Checks: []readiness.Check{{Verdict: readiness.VerdictTodo, Todo: "a"}, {Verdict: readiness.VerdictTodo, Todo: "c"}}},
	}}
	got := r.Todos()
	want := []string{"a", "b", "c"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("todos() = %v, want %v", got, want)
	}
}

// TestGrepWord matches the Makefile's `grep -qw` semantics.
func TestGrepWord(t *testing.T) {
	if !cli.GrepWord("anthropic openai", "openai") {
		t.Error("should match whole word")
	}
	if cli.GrepWord("openaikey", "openai") {
		t.Error("should not match substring")
	}
	if !cli.GrepWord("a,b:c/d", "c") {
		t.Error("should split on punctuation")
	}
}

// TestModelPulled handles :tag suffixes.
func TestModelPulled(t *testing.T) {
	list := "NAME              ID\ngemma4:latest     abc\n"
	if !axis.ModelPulled(list, "gemma4") {
		t.Error("gemma4 should match gemma4:latest")
	}
	if axis.ModelPulled(list, "gemma") {
		t.Error("gemma should not match gemma4")
	}
}

// secretsGroupFor runs doctor with the given cfg.MCP + fake env and returns the
// "Secrets (1Password...)" group.
func secretsGroupFor(t *testing.T, mcp []string, f fakeEnv) readiness.Group {
	t.Helper()
	cfg := defaultCfg()
	cfg.MCP = mcp
	r := RunDoctor(cfg, f.env())
	for _, g := range r.Groups {
		if strings.HasPrefix(g.Title, "Secrets") {
			return g
		}
	}
	t.Fatal("no Secrets group in doctor output")
	return readiness.Group{}
}

func TestDoctor_SecretsGroup_NotNeeded(t *testing.T) {
	g := secretsGroupFor(t, nil, fakeEnv{present: map[string]bool{}})
	if len(g.Checks) != 1 || !strings.Contains(g.Checks[0].Detail, "not needed") {
		t.Errorf("no-server config should say 1Password not needed, got %+v", g.Checks)
	}
}

func TestDoctor_SecretsGroup_GogOnlyNotNeeded(t *testing.T) {
	// A gog-only config must NOT trigger the Secrets group: gog authenticates via
	// OAuth, never op-refs, so a fresh gog-only install must not surface a phantom
	// `pix secret set <ENV_VAR> op://vault/item/field` TODO for a missing op-refs.env.
	g := secretsGroupFor(t, []string{config.GWServerName}, fakeEnv{present: map[string]bool{}})
	if len(g.Checks) != 1 || !strings.Contains(g.Checks[0].Detail, "not needed") {
		t.Errorf("gog-only config should say 1Password not needed, got %+v", g.Checks)
	}
	for _, c := range g.Checks {
		if c.State() == readiness.StateTODO {
			t.Errorf("gog-only config must raise no Secrets TODO, got %+v", c)
		}
	}
}

func TestDoctor_SecretsGroup_SlackOnly(t *testing.T) {
	// A slack-only config must still get the Secrets group (not gog-only).
	f := fakeEnv{
		present: map[string]bool{"op": true},
		output:  map[string]string{"op account list": "me@x.com\n"},
		envVars: map[string]string{"PIX_CONFIG": "/fake/config/config.toml"},
		files:   map[string]string{"/fake/config/op-refs.env": "SLACK_TOKEN=op://Private/Slack/credential\n"},
		modes:   map[string]os.FileMode{"/fake/config/op-refs.env": 0o600, "/fake/config": 0o700},
	}
	g := secretsGroupFor(t, []string{"slack"}, f)
	var sawRef bool
	for _, c := range g.Checks {
		if c.Label == "SLACK_TOKEN" && c.State() == readiness.StateOK {
			sawRef = true
		}
	}
	if !sawRef {
		t.Errorf("slack-only Secrets group should readiness.Report SLACK_TOKEN filled, got %+v", g.Checks)
	}
}

func TestDoctor_SecretsGroup_PermsFinding(t *testing.T) {
	f := fakeEnv{
		present: map[string]bool{"op": true},
		output:  map[string]string{"op account list": "me@x.com\n"},
		envVars: map[string]string{"PIX_CONFIG": "/fake/config/config.toml"},
		files:   map[string]string{"/fake/config/op-refs.env": "SLACK_TOKEN=op://Private/Slack/credential\n"},
		modes:   map[string]os.FileMode{"/fake/config/op-refs.env": 0o644, "/fake/config": 0o700},
	}
	g := secretsGroupFor(t, []string{"slack"}, f)
	var perms *readiness.Check
	for i := range g.Checks {
		if g.Checks[i].Label == "perms" {
			perms = &g.Checks[i]
		}
	}
	if perms == nil || perms.State() != readiness.StateTODO || !strings.Contains(perms.Todo, "chmod 600") {
		t.Errorf("0644 op-refs.env should raise a chmod 600 perms TODO, got %+v", g.Checks)
	}
}

// TestDoctor_SecretsGroup_LintNoLeak: a pasted secret is flagged WITHOUT its
// value appearing anywhere in the rendered doctor output.
func TestDoctor_SecretsGroup_LintNoLeak(t *testing.T) {
	const pasted = "xoxb-PASTED-SECRET-VALUE"
	f := fakeEnv{
		present: map[string]bool{"op": true},
		output:  map[string]string{"op account list": "me@x.com\n"},
		envVars: map[string]string{"PIX_CONFIG": "/fake/config/config.toml"},
		files:   map[string]string{"/fake/config/op-refs.env": "SLACK_TOKEN=" + pasted + "\n"},
		modes:   map[string]os.FileMode{"/fake/config/op-refs.env": 0o600, "/fake/config": 0o700},
	}
	g := secretsGroupFor(t, []string{"slack"}, f)
	var flagged bool
	for _, c := range g.Checks {
		if c.Label == "SLACK_TOKEN" && strings.Contains(c.Detail, "possible pasted secret") {
			flagged = true
		}
		if strings.Contains(c.Detail, pasted) || strings.Contains(c.Todo, pasted) {
			t.Errorf("doctor LEAKED the pasted value in a check: %+v", c)
		}
	}
	if !flagged {
		t.Errorf("a pasted secret should be flagged, got %+v", g.Checks)
	}
}

// `pix secret set <ENV_VAR> op://vault/item/field` at most once.
func TestDoctor_SbxPresentMcpListFailed(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{config.GWServerName}
	cfg.GogAccount = gogAcct
	f := fakeEnv{
		present: map[string]bool{"sbx": true, "gog": true, "op": true},
		output: map[string]string{
			// secret ls works: providers all green, sbx clearly present.
			"sbx secret ls": "anthropic\nopenai\ngoogle\ngithub\n",
			// every `sbx mcp ...` errors (no fake output) — daemon/gateway unhealthy.
		},
		envVars: map[string]string{"GOG_ACCOUNT": gogAcct},
		ports:   map[int]bool{11435: true},
	}
	r := RunDoctor(cfg, f.env())

	// sbx is present — the report-level sbxAbsent flag must be false.
	if r.SbxAbsent {
		t.Errorf("sbx is present (secret ls ok) — sbxAbsent must be false")
	}

	// No check detail may claim sbx is unavailable.
	for _, g := range r.Groups {
		for _, c := range g.Checks {
			if strings.Contains(c.Detail, "sbx unavailable") {
				t.Errorf("sbx is present — no detail may say 'sbx unavailable', got group %q: %q", g.Title, c.Detail)
			}
		}
	}

	// The gog + mcp guidance must point at the sbx daemon/gateway, not
	// "register on the host".
	var buf bytes.Buffer
	r.Services, r.MCP = cfg.Services, cfg.MCP
	r.Render(&buf, false, Hints())
	out := buf.String()
	if !strings.Contains(out, "sbx mcp status") && !strings.Contains(out, "sbx daemon") {
		t.Errorf("expected sbx daemon/gateway guidance, got:\n%s", out)
	}
	if strings.Contains(out, "register on the host") {
		t.Errorf("sbx is present — must not say 'register on the host', got:\n%s", out)
	}

	// providers still green (sanity): no provider TODO.
	joined := strings.Join(r.Todos(), "\n")
	if strings.Contains(joined, "sbx secret set -g") {
		t.Errorf("providers are set — no provider TODO expected, got %v", r.Todos())
	}

	// `pix secret set <ENV_VAR> op://vault/item/field` appears at most once across all todos.
	n := 0
	for _, tdo := range r.Todos() {
		if readiness.TodoDedupKey(tdo) == "pix secret set <ENV_VAR> op://vault/item/field" {
			n++
		}
	}
	if n > 1 {
		t.Errorf("`pix secret set` must appear at most once, got %d: %v", n, r.Todos())
	}
}
