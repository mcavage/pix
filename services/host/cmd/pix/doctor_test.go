package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
)

// fakeEnv builds a shellEnv from a set of present binaries, canned command
// output, env vars, and open ports, so runDoctor can be driven with no real
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
	identityProbe identityProber
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
		fileMode: func(path string) (os.FileMode, bool) {
			if m, ok := f.modes[path]; ok {
				return m, true
			}
			return 0, false
		},
		hostBinary: func() (string, error) {
			if f.hostBin != "" {
				return f.hostBin, nil
			}
			return "", fmt.Errorf("pix-host not found")
		},
		identityProbe: f.identityProbe,
	}
}

// identityFake builds an identityProber from a fixed port->result map: any
// port not in the map answers with an error, matching a real daemon that
// simply isn't there. Ready results default Version to "" so fixtures never
// have to track the launcher's build-time version string.
func identityFake(results map[int]serviceIdentityResult) identityProber {
	return func(port int) (serviceIdentityResult, error) {
		if r, ok := results[port]; ok {
			return r, nil
		}
		return serviceIdentityResult{}, fmt.Errorf("no fake identity for port %d", port)
	}
}

// memGreen fakes a healthy, correctly-identified memory daemon on :11435 —
// the identity-probe counterpart to dialing the port "up": without this, a
// fixture that merely opens the port renders memory unverifiable, never
// ready (readiness_service.go never derives ready from a dial alone).
func memGreen(f fakeEnv) fakeEnv {
	f.identityProbe = identityFake(map[int]serviceIdentityResult{
		11435: {Name: identityMemoryName, Ready: true},
	})
	return f
}

const gogAcct = "you@example.com"

// gogCfgFile / gogOpRefs are the fake $PIX_CONFIG + resolved op-refs path
// the gog fixtures use: setting PIX_CONFIG makes resolveOpRefs return
// gogOpRefs (its dir + op-refs.env), which gogHeadlessOK then probes with.
const gogCfgFile = "/fake/config/config.toml"
const gogOpRefs = "/fake/config/op-refs.env"

// bareGog / opWrappedGog are the two ways `pix mcp register` actually wires
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
	return "op run --no-masking --env-file=" + refs + " -- " + bareGog(acct)
}

// reconstructedGogProbe is the EXACT best-effort headless probe command the
// gog group runs when sbx exposes no registered command: gogRegisteredArgv
// with the fakeEnv's lookPath-resolved paths (/usr/bin/…) — the same hardened
// argv + op wrapper registration would produce, plus --list-tools.
func reconstructedGogProbe(refs, acct string) string {
	return strings.Join(append(gogRegisteredArgv("/usr/bin/gog", "/usr/bin/op", refs, acct), "--list-tools"), " ")
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
	f.envVars["PIX_CONFIG"] = gogCfgFile // makes resolveOpRefs -> gogOpRefs
	f.statFile[gogOpRefs] = true
	f.output["gog --account "+gogAcct+" auth doctor --check"] = "ok"
	f.output[reconstructedGogProbe(gogOpRefs, gogAcct)] =
		"gmail_search\ncalendar_events\ndocs_get\n"
	return f
}

// gogConfirmed layers the sbx-registered-command fixtures on top of gogGreen so
// the gog group takes the HONEST confirmed path (doctor reads the registered
// command via `sbx mcp get google-workspace` and probes THAT). Only this path is a real
// green ✓ — the best-effort reconstruction fallback (gogGreen alone) is now a
// TODO because it can't confirm what the gateway registered.
func gogConfirmed(f fakeEnv) fakeEnv {
	f = gogGreen(f)
	regCmd := opWrappedGog(gogOpRefs, gogAcct)
	f.output["sbx mcp get google-workspace"] = "name: gog\ncommand: " + regCmd + "\n"
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
	f := memGreen(gogConfirmed(fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		output: map[string]string{
			"sbx secret ls": "anthropic\nopenai\ngoogle\ngithub\n",
			"ollama list":   "NAME\ngemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":    "google-workspace\n",
		},
		ports: map[int]bool{11434: true, 11435: true},
	}))
	r := runDoctor(defaultCfg(), f.env())
	if got := len(r.todos()); got != 0 {
		t.Fatalf("expected 0 todos, got %d: %v", got, r.todos())
	}
	var buf bytes.Buffer
	r.services, r.mcp = defaultCfg().Services, nil
	r.render(&buf, false)
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
	prov := r.groups[0]
	if prov.title != "Providers / keys (proxy-injected, never in the VM)" {
		t.Fatalf("expected the providers group first, got %q", prov.title)
	}
	core := prov.checks[0]
	if core.label != "model key" || core.req() != requirementCore || core.result() != verdictUnverifiable {
		t.Errorf("expected an unverifiable core model-key check, got %+v", core)
	}
	if r.blocking() {
		t.Error("an unverifiable core check must never block")
	}

	var buf bytes.Buffer
	r.services, r.mcp = defaultCfg().Services, nil
	r.render(&buf, false)
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
			"sbx mcp ls":    "google-workspace\n",
			"gog --account " + gogAcct + " auth doctor --check": "ok",
			// headless probe exits CLEANLY with an empty tool list -> the trap
			// (a probe ERROR would be unverifiable, not this verified todo).
			reconstructedGogProbe(gogOpRefs, gogAcct): "",
		},
		envVars:  map[string]string{"GOG_ACCOUNT": gogAcct, "PIX_CONFIG": gogCfgFile},
		statFile: map[string]bool{gogOpRefs: true},
		ports:    map[int]bool{11435: true},
	}
	r := runDoctor(defaultCfg(), f.env())
	var gog group
	for _, g := range r.groups {
		if strings.HasPrefix(g.title, "Google Workspace") {
			gog = g
		}
	}
	var acctOK, headTODO bool
	for _, c := range gog.checks {
		if c.label == "account" && c.state() == stateOK {
			acctOK = true
		}
		if c.label == "headless spawn" && c.state() == stateTODO &&
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

// TestDoctor_GogAccountUnset: gog installed but GOG_ACCOUNT unset is
// optional-NOT-CONFIGURED — a note pointing at the guided setup, never a
// verified failure/TODO (absence of an optional integration is expected), and
// no crash (the account/headless probes are skipped).
func TestDoctor_GogAccountUnset(t *testing.T) {
	f := fakeEnv{
		present: map[string]bool{"gog": true},
		output:  map[string]string{},
		envVars: map[string]string{},
		ports:   map[int]bool{},
	}
	r := runDoctor(defaultCfg(), f.env())
	joined := strings.Join(r.todos(), "\n")
	if strings.Contains(joined, "google_workspace_account") || strings.Contains(joined, "gog setup") {
		t.Errorf("an unset gog account is not-configured, never a TODO, got %v", r.todos())
	}
	// It must NOT report green either: the account line is a note that says
	// it is not configured and names the guided setup command.
	var buf bytes.Buffer
	r.services, r.mcp = defaultCfg().Services, nil
	r.render(&buf, false)
	if !strings.Contains(buf.String(), "not configured (gog_account unset) — set up: pix gworkspace setup") {
		t.Errorf("expected a not-configured account note naming pix gworkspace setup, got:\n%s", buf.String())
	}
	// And never the raw legacy auth recipe.
	if strings.Contains(buf.String(), "gog auth login") {
		t.Errorf("raw `gog auth login` guidance is banned, got:\n%s", buf.String())
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
	cfg.MCP = []string{gwServerName}
	const ws = "/home/u/proj"
	const box = "pix-proj"
	f := fakeEnv{
		present: map[string]bool{"sbx": true}, // gog NOT on PATH
		output: map[string]string{
			"sbx ls": box + "  running\n",
			// no `sbx mcp get google-workspace` / `sbx mcp ls -o json` fixture -> registeredGogCommand
			// returns (nil,false): the registered command is unreadable.
		},
	}
	env := f.env()
	env.getwd = func() (string, error) { return ws, nil }
	stateDir := t.TempDir()
	env.stateDir = func() (string, error) { return stateDir, nil }
	if err := writeCreateReceipt(stateDir, box, ws, []string{gwServerName}, receiptClock); err != nil {
		t.Fatal(err)
	}
	ctx := resolveMCPSandboxContext(env)
	if ctx.mode != mcpAttachReceipt {
		t.Fatalf("expected a receipt sandbox context, got mode=%v", ctx.mode)
	}
	g := gogGroup(cfg, env, "google-workspace\n", true, true, ctx)

	reg := findCheck(t, g, gwServerName)
	if reg.result() != verdictReady {
		t.Errorf("registration check must still be emitted and ready: %+v", reg)
	}
	attach := findCheck(t, g, gwServerName+" attachment")
	if attach.result() != verdictReady || !strings.Contains(attach.evidence, "preloaded by pix at create") {
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
	got := resolveOpRefs(f.env())
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
	if got := resolveOpRefs(f2.env()); got != "/home/me/.config/pix/op-refs.env" {
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
			"sbx mcp ls":    "google-workspace\n",
		},
		ports: map[int]bool{11435: true},
	})
	r := runDoctor(defaultCfg(), f.env())
	var buf bytes.Buffer
	r.services, r.mcp = defaultCfg().Services, []string{gwServerName}
	r.render(&buf, false)
	out := buf.String()
	if !strings.Contains(out, "verifying") || !strings.Contains(out, gogAcct) || !strings.Contains(out, gogOpRefs) {
		t.Errorf("expected a transparency line naming account+op-refs, got:\n%s", out)
	}
	if !strings.Contains(out, "must match the sbx-registered gog command") {
		t.Errorf("expected the must-match note, got:\n%s", out)
	}
	// The fallback (sbx exposes no registered command) must be labeled best-effort
	// so a pass can't masquerade as a confirmed registration. Here sbx IS present
	// (the CLI is on PATH, `sbx mcp ls` succeeded) but the registered command
	// couldn't be read, so the label must blame the registration read / gateway,
	// NOT claim sbx is unavailable.
	if !strings.Contains(out, "best-effort (couldn't read sbx MCP registrations") {
		t.Errorf("expected a best-effort registration-read fallback label, got:\n%s", out)
	}
	if strings.Contains(out, "sbx unavailable") {
		t.Errorf("sbx is present here — must not say 'sbx unavailable', got:\n%s", out)
	}
}

// TestDoctor_SbxPresentMcpListFailed reproduces the HOST symptom: sbx is on PATH
// and `sbx secret ls` SUCCEEDS (providers green), but every `sbx mcp ...` call
// ERRORS (no fake output — the sbx daemon/gateway is unhealthy). doctor
// must NOT claim "sbx unavailable" anywhere, must point at the sbx daemon/gateway
// rather than "register on the host", and must emit
// `pix secret set <ENV_VAR> op://vault/item/field` at most once.
func TestDoctor_SbxPresentMcpListFailed(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{gwServerName}
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
	r := runDoctor(cfg, f.env())

	// sbx is present — the report-level sbxAbsent flag must be false.
	if r.sbxAbsent {
		t.Errorf("sbx is present (secret ls ok) — sbxAbsent must be false")
	}

	// No check detail may claim sbx is unavailable.
	for _, g := range r.groups {
		for _, c := range g.checks {
			if strings.Contains(c.detail, "sbx unavailable") {
				t.Errorf("sbx is present — no detail may say 'sbx unavailable', got group %q: %q", g.title, c.detail)
			}
		}
	}

	// The gog + mcp guidance must point at the sbx daemon/gateway, not
	// "register on the host".
	var buf bytes.Buffer
	r.services, r.mcp = cfg.Services, cfg.MCP
	r.render(&buf, false)
	out := buf.String()
	if !strings.Contains(out, "sbx mcp status") && !strings.Contains(out, "sbx daemon") {
		t.Errorf("expected sbx daemon/gateway guidance, got:\n%s", out)
	}
	if strings.Contains(out, "register on the host") {
		t.Errorf("sbx is present — must not say 'register on the host', got:\n%s", out)
	}

	// providers still green (sanity): no provider TODO.
	joined := strings.Join(r.todos(), "\n")
	if strings.Contains(joined, "sbx secret set -g") {
		t.Errorf("providers are set — no provider TODO expected, got %v", r.todos())
	}

	// `pix secret set <ENV_VAR> op://vault/item/field` appears at most once across all todos.
	n := 0
	for _, tdo := range r.todos() {
		if todoDedupKey(tdo) == "pix secret set <ENV_VAR> op://vault/item/field" {
			n++
		}
	}
	if n > 1 {
		t.Errorf("`pix secret set` must appear at most once, got %d: %v", n, r.todos())
	}
}

// TestRedactRegisteredCommand covers F3: a value token (e.g. a pasted secret
// behind --client-secret) is never echoed verbatim, while recognizable
// subcommands/flag names survive.
func TestRedactRegisteredCommand(t *testing.T) {
	argv := []string{"/usr/bin/op", "run", "--env-file=/abs/config/op-refs.env", "--",
		"gog", "--account", "you@example.com", "--client-secret", "SEKRET", "mcp"}
	got := redactRegisteredCommand(argv)
	if strings.Contains(got, "SEKRET") {
		t.Errorf("redacted command leaked the value token: %q", got)
	}
	if strings.Contains(got, "you@example.com") {
		t.Errorf("redacted command leaked the account value: %q", got)
	}
	if strings.Contains(got, "/abs/config/op-refs.env") {
		t.Errorf("redacted command leaked the env-file path value: %q", got)
	}
	// Recognizable structure survives.
	for _, want := range []string{"op", "run", "--env-file=…", "--", "gog", "--account", "mcp", "‹redacted›"} {
		if !strings.Contains(got, want) {
			t.Errorf("redacted command missing %q: %q", want, got)
		}
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
	r := runDoctor(defaultCfg(), f.env())
	for _, g := range r.groups {
		for _, c := range g.checks {
			if strings.Contains(c.detail, secret) || strings.Contains(c.todo, secret) {
				t.Errorf("doctor leaked the pasted secret in group %q: detail=%q todo=%q", g.title, c.detail, c.todo)
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
	for _, c := range g.checks {
		if strings.Contains(c.detail, val) {
			t.Errorf("secrets group LEAKED the literal value: %q", c.detail)
		}
		if c.label == "SLACK_TOKEN" {
			if c.state() != stateTODO {
				t.Errorf("SLACK_TOKEN state = %v, want stateTODO", c.state())
			}
			if !strings.Contains(c.detail, "not an op:// ref") {
				t.Errorf("SLACK_TOKEN detail should flag refs-only: %q", c.detail)
			}
			found = true
		}
	}
	if !found {
		t.Errorf("expected a SLACK_TOKEN check in the Secrets group, group=%+v", g)
	}
}

// TestDoctor_GogRegisteredCommand is the HONEST path: sbx exposes the ACTUAL
// registered gog command, so doctor probes THAT exact command (with
// --list-tools) rather than reconstructing it from config. A non-empty tool
// list reads as a confirmed-green headless spawn. The registered wrapper is
// the exact launcher grammar against the RESOLVED op-refs path (gogOpRefs via
// PIX_CONFIG) — doctor refuses to probe anything else.
func TestDoctor_GogRegisteredCommand(t *testing.T) {
	regCmd := opWrappedGog(gogOpRefs, gogAcct)
	probeKey := regCmd + " --list-tools"
	f := fakeEnv{
		present:  map[string]bool{"sbx": true, "op": true},
		envVars:  map[string]string{"PIX_CONFIG": gogCfgFile},
		statFile: map[string]bool{gogOpRefs: true},
		output: map[string]string{
			"sbx secret ls":                "anthropic openai google github",
			"sbx mcp ls":                   "google-workspace\n",
			"sbx mcp get google-workspace": "name: gog\ncommand: " + regCmd + "\n",
			probeKey:                       "gmail_search\ncalendar_events\ndocs_get\n",
		},
		ports: map[int]bool{11435: true},
	}
	r := runDoctor(defaultCfg(), f.env())
	var gog group
	for _, g := range r.groups {
		if strings.HasPrefix(g.title, "Google Workspace") {
			gog = g
		}
	}
	var regShown, headOK bool
	for _, c := range gog.checks {
		if c.label == "registration" {
			// The registered command is shown REDACTED: recognizable skeleton (op
			// run/env-file/gog/mcp), but the account value scrubbed to ‹redacted›.
			if strings.Contains(c.detail, "op run --no-masking --env-file=… -- gog --account ‹redacted›") &&
				strings.Contains(c.detail, "mcp") {
				regShown = true
			}
			if strings.Contains(c.detail, gogAcct) {
				t.Errorf("registered command detail must not echo the account verbatim: %q", c.detail)
			}
		}
		if c.label == "headless spawn" && c.state() == stateOK {
			headOK = true
		}
	}
	if !regShown {
		t.Errorf("expected doctor to name the sbx-registered command (redacted), group=%+v", gog)
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
			"sbx secret ls":                "anthropic openai google github",
			"ollama list":                  "gemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":                   "google-workspace\n",
			"sbx mcp get google-workspace": "name: gog\ncommand: op\n", // partial: no `-- <cmd>` tail
			"sbx mcp ls -o json":           "not json{",                // unparseable
		},
		ports: map[int]bool{11434: true, 11435: true},
	})
	r := runDoctor(defaultCfg(), f.env())
	// The headless spawn must be UNVERIFIABLE (⚠) — doctor genuinely does not
	// know whether the best-effort pass matches the real registration — and it
	// must carry NO repair TODO (nothing is confirmed broken to fix).
	var headWarn bool
	for _, g := range r.groups {
		if !strings.HasPrefix(g.title, "Google Workspace") {
			continue
		}
		for _, c := range g.checks {
			if c.label == "headless spawn" {
				if c.state() == stateWarn &&
					strings.Contains(c.detail, "could not be confirmed") {
					headWarn = true
				}
				if c.todo != "" {
					t.Errorf("an unverifiable best-effort pass must not carry a TODO, got %q", c.todo)
				}
			}
		}
	}
	if !headWarn {
		t.Fatalf("expected an unconfirmed-fallback headless ⚠, groups=%+v", r.groups)
	}
	// Verdict must NOT be all-clear: the headline calls out the unverified
	// checks instead.
	var buf bytes.Buffer
	r.services, r.mcp = defaultCfg().Services, []string{gwServerName}
	r.render(&buf, false)
	out := buf.String()
	if strings.Contains(out, "all checks pass") {
		t.Errorf("unconfirmed fallback must not report all-clear, got:\n%s", out)
	}
	if !strings.Contains(out, "could not be verified") {
		t.Errorf("expected the could-not-verify headline, got:\n%s", out)
	}
}

// TestDoctor_GogRegisteredCommandLineFallsThrough: `sbx mcp get google-workspace` emits only
// a partial `command:` line (no `-- <cmd>` tail), so the line parser must FALL
// THROUGH to the JSON form, which carries the full argv and confirms green.
func TestDoctor_GogRegisteredCommandLineFallsThrough(t *testing.T) {
	probeKey := opWrappedGog(gogOpRefs, gogAcct) + " --list-tools"
	f := fakeEnv{
		present:  map[string]bool{"sbx": true},
		envVars:  map[string]string{"PIX_CONFIG": gogCfgFile},
		statFile: map[string]bool{gogOpRefs: true},
		output: map[string]string{
			"sbx secret ls":                "anthropic openai google github",
			"sbx mcp ls":                   "google-workspace\n",
			"sbx mcp get google-workspace": "name: gog\ncommand: op\n", // partial line -> fall through
			"sbx mcp ls -o json":           `[{"name":"google-workspace","command":"op","args":["run","--no-masking","--env-file=` + gogOpRefs + `","--","gog","--account","you@example.com","--gmail-no-send","--wrap-untrusted","--readonly","mcp","--allow-tool","read"]}]`,
			probeKey:                       "gmail_search\n",
		},
		ports: map[int]bool{11435: true},
	}
	r := runDoctor(defaultCfg(), f.env())
	var headOK bool
	for _, g := range r.groups {
		if !strings.HasPrefix(g.title, "Google Workspace") {
			continue
		}
		for _, c := range g.checks {
			if c.label == "headless spawn" && c.state() == stateOK {
				headOK = true
			}
		}
	}
	if !headOK {
		t.Errorf("expected line form to fall through to JSON and confirm green, groups=%+v", r.groups)
	}
}

func TestRegisteredGogCommand_CurrentSbxPlainTable(t *testing.T) {
	regCmd := opWrappedGog(gogOpRefs, gogAcct)
	f := fakeEnv{
		present:  map[string]bool{"sbx": true, "op": true, "gog": true},
		envVars:  map[string]string{"PIX_CONFIG": gogCfgFile},
		statFile: map[string]bool{gogOpRefs: true},
		output: map[string]string{
			"sbx mcp ls": "NAME  TYPE   URL/COMMAND\n" +
				"google-workspace   local  " + regCmd + "\n",
		},
	}
	env := f.env()
	argv, ok := registeredGogCommand(env)
	if !ok {
		t.Fatal("current sbx plain table carries the complete command and must be readable")
	}
	if got := strings.Join(argv, " "); got != regCmd {
		t.Fatalf("registered argv = %q, want %q", got, regCmd)
	}
	snap := snapshotGogRegistration(env)
	if snap.state != gogRegPresent {
		t.Fatalf("gog setup snapshot state = %v, want present", snap.state)
	}
	if got := strings.Join(snap.argv, " "); got != regCmd {
		t.Fatalf("snapshot argv = %q, want %q", got, regCmd)
	}
}

// TestDoctor_GogRegisteredCommandJSON: sbx exposes the registration only via
// `sbx mcp ls -o json`; doctor parses command+args and probes it.
func TestDoctor_GogRegisteredCommandJSON(t *testing.T) {
	probeKey := opWrappedGog(gogOpRefs, gogAcct) + " --list-tools"
	f := fakeEnv{
		present:  map[string]bool{"sbx": true},
		envVars:  map[string]string{"PIX_CONFIG": gogCfgFile},
		statFile: map[string]bool{gogOpRefs: true},
		output: map[string]string{
			"sbx secret ls":      "anthropic openai google github",
			"sbx mcp ls":         "google-workspace\n",
			"sbx mcp ls -o json": `[{"name":"google-workspace","command":"op","args":["run","--no-masking","--env-file=` + gogOpRefs + `","--","gog","--account","you@example.com","--gmail-no-send","--wrap-untrusted","--readonly","mcp","--allow-tool","read"]}]`,
			probeKey:             "gmail_search\n",
		},
		ports: map[int]bool{11435: true},
	}
	r := runDoctor(defaultCfg(), f.env())
	var headOK bool
	for _, g := range r.groups {
		if !strings.HasPrefix(g.title, "Google Workspace") {
			continue
		}
		for _, c := range g.checks {
			if c.label == "headless spawn" && c.state() == stateOK {
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
			"sbx secret ls":                "anthropic openai google github",
			"sbx mcp ls":                   "google-workspace\n",
			"sbx mcp get google-workspace": "name: gog\ncommand: " + regCmd + "\n",
			probeKey:                       "gmail_search\ncalendar_events\ndocs_get\n",
		},
		ports: map[int]bool{11435: true},
	}
	r := runDoctor(defaultCfg(), f.env())
	var gog group
	for _, g := range r.groups {
		if strings.HasPrefix(g.title, "Google Workspace") {
			gog = g
		}
	}
	var regShown, headOK bool
	for _, c := range gog.checks {
		if c.label == "registration" {
			// Bare command shown REDACTED: recognizable `gog --account ‹redacted› … mcp`.
			if strings.Contains(c.detail, "gog --account ‹redacted›") &&
				strings.Contains(c.detail, "mcp") {
				regShown = true
			}
			if strings.Contains(c.detail, gogAcct) {
				t.Errorf("registered command detail must not echo the account verbatim: %q", c.detail)
			}
		}
		if c.label == "headless spawn" && c.state() == stateOK {
			headOK = true
		}
	}
	if !regShown {
		t.Errorf("expected doctor to name the bare sbx-registered gog command (redacted), group=%+v", gog)
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
// but surfaced only via `sbx mcp ls -o json` (command=gwServerName, args=[…]). doctor
// must parse command+args, recognize it as a valid gog registration, and probe
// it to a confirmed green.
func TestDoctor_GogBareRegisteredCommandJSON(t *testing.T) {
	probeKey := bareGog(gogAcct) + " --list-tools"
	f := fakeEnv{
		present: map[string]bool{"sbx": true, "gog": true},
		output: map[string]string{
			"sbx secret ls":      "anthropic openai google github",
			"sbx mcp ls":         "google-workspace\n",
			"sbx mcp ls -o json": `[{"name":"google-workspace","command":"gog","args":["--account","you@example.com","--gmail-no-send","--wrap-untrusted","--readonly","mcp","--allow-tool","read"]}]`,
			probeKey:             "gmail_search\n",
		},
		ports: map[int]bool{11435: true},
	}
	r := runDoctor(defaultCfg(), f.env())
	var headOK bool
	for _, g := range r.groups {
		if !strings.HasPrefix(g.title, "Google Workspace") {
			continue
		}
		for _, c := range g.checks {
			if c.label == "headless spawn" && c.state() == stateOK {
				headOK = true
			}
		}
	}
	if !headOK {
		t.Errorf("expected a confirmed headless spawn from the bare JSON-parsed registration, groups=%+v", r.groups)
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
			"sbx mcp ls":    "google-workspace\n",
		},
		ports: map[int]bool{11435: true},
	})
	// Drop the env var; the account must come from config instead.
	delete(f.envVars, "GOG_ACCOUNT")
	cfg := defaultCfg()
	cfg.GogAccount = gogAcct
	r := runDoctor(cfg, f.env())
	joined := strings.Join(r.todos(), "\n")
	if strings.Contains(joined, "google_workspace_account") || strings.Contains(joined, "GOG_ACCOUNT") {
		t.Errorf("account from config should not TODO, got %v", r.todos())
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
	r := runDoctor(defaultCfg(), f.env())
	var found bool
	for _, g := range r.groups {
		if !strings.HasPrefix(g.title, "Google Workspace") {
			continue
		}
		for _, c := range g.checks {
			if c.label == gwServerName && c.state() == stateTODO {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("expected an unregistered Google Workspace TODO, groups=%+v", r.groups)
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
	r := runDoctor(cfg, f.env())
	found := false
	for _, c := range r.groups[len(r.groups)-1].checks {
		if c.label == "slack" && c.state() == stateTODO {
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
		if c.label == "slack" && c.state() == stateTODO {
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
	r := runDoctor(cfg, f.env())
	// The generic mcp group is last; slack must read as a real tool count.
	var found bool
	for _, c := range r.groups[len(r.groups)-1].checks {
		if c.label == "slack" && c.state() == stateOK && strings.Contains(c.detail, "spawns 3 tools") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected slack to report a real tool count, group=%+v", r.groups[len(r.groups)-1])
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
	r := runDoctor(cfg, f.env())
	var todo bool
	for _, c := range r.groups[len(r.groups)-1].checks {
		if c.label == "slack" && c.state() == stateTODO && strings.Contains(c.detail, "0 tools") {
			todo = true
		}
	}
	if !todo {
		t.Errorf("expected a 0-tools TODO for slack, group=%+v", r.groups[len(r.groups)-1])
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
	inner := env.run
	env.run = func(name string, args ...string) (string, error) {
		if name == "/bin/rm" {
			t.Fatalf("doctor exec'd an unrecognized registered command: %s %v", name, args)
		}
		return inner(name, args...)
	}
	r := runDoctor(cfg, env)
	var found bool
	for _, c := range r.groups[len(r.groups)-1].checks {
		if c.label == "evil" && c.state() == stateWarn &&
			strings.Contains(c.detail, "never executed") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected evil to report a skipped probe, group=%+v", r.groups[len(r.groups)-1])
	}
}

// TestDoctor_GogTodoOnce is the duplicate-TODO gate: gog is UNREGISTERED and
// also present in cfg.MCP. The dedicated gog group owns gog's registration TODO;
// the generic mcp group must SKIP gog, and report.todos() dedupes regardless, so
// `pix mcp register` appears AT MOST ONCE.
func TestDoctor_GogTodoOnce(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{gwServerName}
	f := gogGreen(fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"ollama list":   "gemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":    "notion\n", // gog NOT registered
		},
		ports: map[int]bool{11434: true, 11435: true},
	})
	r := runDoctor(cfg, f.env())
	n := 0
	for _, tdo := range r.todos() {
		if tdo == "pix mcp register" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("expected `pix mcp register` exactly once, got %d: %v", n, r.todos())
	}
	// The generic mcp group must not carry a gog check at all.
	for _, c := range r.groups[len(r.groups)-1].checks {
		if c.label == gwServerName {
			t.Errorf("generic mcp group should skip gog, got check %+v", c)
		}
	}
}

// TestDoctorTodosDedup proves report.todos() drops exact-duplicate commands
// while preserving first-occurrence order.
func TestDoctorTodosDedup(t *testing.T) {
	r := &report{groups: []group{
		{checks: []check{{verdict: verdictTodo, todo: "a"}, {verdict: verdictTodo, todo: "b"}}},
		{checks: []check{{verdict: verdictTodo, todo: "a"}, {verdict: verdictTodo, todo: "c"}}},
	}}
	got := r.todos()
	want := []string{"a", "b", "c"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("todos() = %v, want %v", got, want)
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

// secretsGroupFor runs doctor with the given cfg.MCP + fake env and returns the
// "Secrets (1Password...)" group.
func secretsGroupFor(t *testing.T, mcp []string, f fakeEnv) group {
	t.Helper()
	cfg := defaultCfg()
	cfg.MCP = mcp
	r := runDoctor(cfg, f.env())
	for _, g := range r.groups {
		if strings.HasPrefix(g.title, "Secrets") {
			return g
		}
	}
	t.Fatal("no Secrets group in doctor output")
	return group{}
}

func TestDoctor_SecretsGroup_NotNeeded(t *testing.T) {
	g := secretsGroupFor(t, nil, fakeEnv{present: map[string]bool{}})
	if len(g.checks) != 1 || !strings.Contains(g.checks[0].detail, "not needed") {
		t.Errorf("no-server config should say 1Password not needed, got %+v", g.checks)
	}
}

func TestDoctor_SecretsGroup_GogOnlyNotNeeded(t *testing.T) {
	// A gog-only config must NOT trigger the Secrets group: gog authenticates via
	// OAuth, never op-refs, so a fresh gog-only install must not surface a phantom
	// `pix secret set <ENV_VAR> op://vault/item/field` TODO for a missing op-refs.env.
	g := secretsGroupFor(t, []string{gwServerName}, fakeEnv{present: map[string]bool{}})
	if len(g.checks) != 1 || !strings.Contains(g.checks[0].detail, "not needed") {
		t.Errorf("gog-only config should say 1Password not needed, got %+v", g.checks)
	}
	for _, c := range g.checks {
		if c.state() == stateTODO {
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
	for _, c := range g.checks {
		if c.label == "SLACK_TOKEN" && c.state() == stateOK {
			sawRef = true
		}
	}
	if !sawRef {
		t.Errorf("slack-only Secrets group should report SLACK_TOKEN filled, got %+v", g.checks)
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
	var perms *check
	for i := range g.checks {
		if g.checks[i].label == "perms" {
			perms = &g.checks[i]
		}
	}
	if perms == nil || perms.state() != stateTODO || !strings.Contains(perms.todo, "chmod 600") {
		t.Errorf("0644 op-refs.env should raise a chmod 600 perms TODO, got %+v", g.checks)
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
	for _, c := range g.checks {
		if c.label == "SLACK_TOKEN" && strings.Contains(c.detail, "possible pasted secret") {
			flagged = true
		}
		if strings.Contains(c.detail, pasted) || strings.Contains(c.todo, pasted) {
			t.Errorf("doctor LEAKED the pasted value in a check: %+v", c)
		}
	}
	if !flagged {
		t.Errorf("a pasted secret should be flagged, got %+v", g.checks)
	}
}
