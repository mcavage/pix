package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hostStub is a hostResolver that returns a fixed path (or an error).
func hostStub(path string, err error) func() (string, error) {
	return func() (string, error) { return path, err }
}

// gogRegistrar returns a registrar with fixed absolutes for builder tests.
func gogRegistrar() mcpRegistrar {
	return mcpRegistrar{
		op:      "/usr/bin/op",
		opRefs:  "/abs/config/op-refs.env",
		gog:     "/usr/bin/gog",
		account: "me@x.com",
		hostBin: "/usr/bin/pi-stack-host",
	}
}

// TestAddArgs_Gog builds the hardened gog `sbx mcp add` command (mirrors the
// Makefile mcp-register line).
func TestAddArgs_Gog(t *testing.T) {
	args := gogRegistrar().addArgs("gog")
	if !contains(args, []string{"mcp", "add", "gog", "--command", "/usr/bin/op"}) {
		t.Errorf("missing add-command prefix in %v", args)
	}
	// op-run wrapper.
	if !contains(args, []string{"--args", "run", "--args", "--no-masking",
		"--args", "--env-file=/abs/config/op-refs.env", "--args", "--"}) {
		t.Errorf("missing op-run wrapper in %v", args)
	}
	// gog binary + hardened flags.
	if !contains(args, []string{"--args", "/usr/bin/gog", "--args", "--account", "--args", "me@x.com"}) {
		t.Errorf("missing gog account args in %v", args)
	}
	for _, flag := range []string{"--gmail-no-send", "--wrap-untrusted", "--readonly", "read"} {
		if !contains(args, []string{"--args", flag}) {
			t.Errorf("missing hardened flag %q in %v", flag, args)
		}
	}
}

// TestAddArgs_Slack builds the pi-stack-host subcommand form (op-run wrapped).
func TestAddArgs_Slack(t *testing.T) {
	args := gogRegistrar().addArgs("slack")
	if !contains(args, []string{"--args", "/usr/bin/pi-stack-host", "--args", "mcp", "--args", "slack"}) {
		t.Errorf("slack should register pi-stack-host mcp slack, got %v", args)
	}
}

// TestAddArgs_LocalServer builds the pi-stack-host subcommand form for an
// arbitrary (overlay) local stdio server like "pio": it registers as
// `pi-stack-host mcp pio`, exactly like slack, via the serverCmd default.
func TestAddArgs_LocalServer(t *testing.T) {
	args := gogRegistrar().addArgs("pio")
	if !contains(args, []string{"mcp", "add", "pio", "--command", "/usr/bin/op"}) {
		t.Errorf("missing add-command prefix for pio in %v", args)
	}
	if !contains(args, []string{"--args", "/usr/bin/pi-stack-host", "--args", "mcp", "--args", "pio"}) {
		t.Errorf("pio should register pi-stack-host mcp pio, got %v", args)
	}
}

// TestAddArgs_GogBare: with NO op-refs (opRefs=""), gog registers DIRECTLY as a
// bare command — no `op run` wrapper. 1Password is optional for gog.
func TestAddArgs_GogBare(t *testing.T) {
	reg := gogRegistrar()
	reg.opRefs = "" // no op-refs -> bare
	args := reg.addArgs("gog")
	if !contains(args, []string{"mcp", "add", "gog", "--command", "/usr/bin/gog"}) {
		t.Errorf("bare gog should register --command /usr/bin/gog, got %v", args)
	}
	for _, a := range args {
		if a == "/usr/bin/op" || a == "run" {
			t.Errorf("bare gog must not wrap in op run, got %v", args)
		}
	}
	// Hardened flags survive in the bare form.
	if !contains(args, []string{"--args", "--account", "--args", "me@x.com"}) {
		t.Errorf("bare gog missing account args in %v", args)
	}
	for _, flag := range []string{"--gmail-no-send", "--wrap-untrusted", "--readonly", "read"} {
		if !contains(args, []string{"--args", flag}) {
			t.Errorf("bare gog missing hardened flag %q in %v", flag, args)
		}
	}
}

// TestGogRegisteredArgv_MatchesAddArgs is finding #2's exact-argv-equality
// test: gogRegisteredArgv (the canonical builder gog_setup.go's headless
// verification and doctor's fallback probe both call) must produce the
// IDENTICAL command line that mcpRegistrar.addArgs("gog") will register with
// sbx -- not merely an equivalent one. We derive the expected argv from
// addArgs' own --command/--args encoding (the real registration path) rather
// than hand-typing a second copy, so the two can never independently drift.
func TestGogRegisteredArgv_MatchesAddArgs(t *testing.T) {
	reg := gogRegistrar()
	want := reg.execArgv("gog")
	got := gogRegisteredArgv(reg.gog, reg.op, reg.opRefs, reg.account)
	if !equalSlice(got, want) {
		t.Fatalf("gogRegisteredArgv = %v, want exactly reg.execArgv(gog) = %v", got, want)
	}
	// Must actually carry the hardened flags + op wrapper, not just agree with
	// itself.
	for _, tok := range []string{"--no-masking", "--gmail-no-send", "--wrap-untrusted", "--readonly", "read"} {
		found := false
		for _, g := range got {
			if g == tok {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("gogRegisteredArgv missing hardened token %q in %v", tok, got)
		}
	}

	// Bare (no op-refs): still hardened, still exactly execArgv's bare form.
	wantBare := mcpRegistrar{gog: reg.gog, account: reg.account}.execArgv("gog")
	gotBare := gogRegisteredArgv(reg.gog, "", "", reg.account)
	if !equalSlice(gotBare, wantBare) {
		t.Errorf("bare gogRegisteredArgv = %v, want %v", gotBare, wantBare)
	}
}

// TestRegisterServers_GogNoOpRefsBare: gateway on, op + op-refs ABSENT, gog
// present + account set -> gog registers DIRECTLY (bare command, no op wrapper)
// with the OAuth note. gog uses OAuth (gog auth login), never op-refs, so the
// note must NOT mention op-refs.
func TestRegisterServers_GogNoOpRefsBare(t *testing.T) {
	f := fakeEnv{
		present: map[string]bool{"gog": true}, // no op, no sbx
		output:  map[string]string{},
		envVars: map[string]string{"SBX_MCP_URL": "https://gateway.docker.com", "HOME": "/home/me"},
		home:    "/home/me",
	}
	cfg := defaultCfg()
	cfg.GogAccount = "me@x.com"
	var buf bytes.Buffer
	if err := registerServers(cfg, f.env(), &buf, []string{"gog"}, hostStub("", nil)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "registered gog directly") {
		t.Errorf("expected the bare-gog note, got:\n%s", out)
	}
	if strings.Contains(out, "op-refs.env") {
		t.Errorf("gog-only note must NOT mention op-refs (gog uses OAuth), got:\n%s", out)
	}
	// The would-run command must be the bare gog command, not an op wrapper.
	if !strings.Contains(out, "sbx mcp add gog --command /usr/bin/gog") {
		t.Errorf("expected a bare gog would-run command, got:\n%s", out)
	}
}

// TestRegisterServers_GogOnlyNoSeed (R5-1/R5-2): a gog-only clean state with op
// NOT resolvable must NOT create op-refs.env at the XDG path and must NOT print a
// "seeded" line. gog authenticates via OAuth (gog auth login), never op-refs, so
// seeding one contradicts setup Step 4's "No file is created" copy.
func TestRegisterServers_GogOnlyNoSeed(t *testing.T) {
	home := t.TempDir()
	env := (fakeEnv{
		present: map[string]bool{"gog": true}, // op absent -> not resolvable
		output:  map[string]string{},
		envVars: map[string]string{"SBX_MCP_URL": "https://gateway.docker.com"},
		home:    home,
	}).env()
	cfg := defaultCfg()
	cfg.MCP = []string{"gog"}
	cfg.GogAccount = "me@x.com"
	var buf bytes.Buffer
	if err := registerServers(cfg, env, &buf, nil, hostStub("/usr/bin/pi-stack-host", nil)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	seeded := filepath.Join(home, ".config", "pi-stack", "op-refs.env")
	if _, err := os.Stat(seeded); !os.IsNotExist(err) {
		t.Errorf("gog-only must NOT seed op-refs.env at %s (err=%v)", seeded, err)
	}
	out := buf.String()
	if strings.Contains(out, "seeded") {
		t.Errorf("gog-only must NOT print a seeded line, got:\n%s", out)
	}
	if strings.Contains(out, "op-refs.env") {
		t.Errorf("gog-only note must NOT mention op-refs, got:\n%s", out)
	}
}

// TestRegisterServers_SlackNoOpRefsBare: slack with op absent + op-refs absent no
// longer errors — it registers BARE (no op-run wrapper), so a no-creds server
// registers without 1Password. sbx absent -> the would-run command is printed.
func TestRegisterServers_SlackNoOpRefsBare(t *testing.T) {
	f := fakeEnv{
		present: map[string]bool{}, // no op, no sbx
		output:  map[string]string{"/usr/bin/pi-stack-host mcp --list": "slack\n"},
		envVars: map[string]string{"SBX_MCP_URL": "https://gateway.docker.com", "HOME": "/home/me"},
		home:    "/home/me",
	}
	cfg := defaultCfg()
	var buf bytes.Buffer
	if err := registerServers(cfg, f.env(), &buf, []string{"slack"}, hostStub("/usr/bin/pi-stack-host", nil)); err != nil {
		t.Fatalf("unexpected error (slack should register bare, not fail): %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "registered slack directly (bare, no 1Password)") {
		t.Errorf("expected the bare-registration note for slack, got:\n%s", out)
	}
	// Bare command: --command /usr/bin/pi-stack-host, no op-run wrapper.
	if !strings.Contains(out, "sbx mcp add slack --command /usr/bin/pi-stack-host --args mcp --args slack") {
		t.Errorf("expected a bare slack would-run command, got:\n%s", out)
	}
	if strings.Contains(out, "op") && strings.Contains(out, "run --no-masking") {
		t.Errorf("bare slack must not wrap in op run, got:\n%s", out)
	}
}

// TestRegisterServers_SlackOpRefsAbsentSeeds: slack with op present but op-refs
// ABSENT -> seed a template at the absolute XDG path (via the ONE seeder,
// config.SeedOpRefsAt) and register BARE (no error), noting the seeded path.
func TestRegisterServers_SlackOpRefsAbsentSeeds(t *testing.T) {
	home := t.TempDir()
	env := (fakeEnv{
		present: map[string]bool{"op": true},
		output:  map[string]string{"/usr/bin/pi-stack-host mcp --list": "slack\n"},
		envVars: map[string]string{"SBX_MCP_URL": "https://gateway.docker.com"},
		home:    home,
	}).env()
	cfg := defaultCfg()
	var buf bytes.Buffer
	if err := registerServers(cfg, env, &buf, []string{"slack"}, hostStub("/usr/bin/pi-stack-host", nil)); err != nil {
		t.Fatalf("unexpected error (slack should register bare, not fail): %v", err)
	}
	seeded := filepath.Join(home, ".config", "pi-stack", "op-refs.env")
	info, err := os.Stat(seeded)
	if err != nil {
		t.Fatalf("expected the template seeded at %s: %v", seeded, err)
	}
	// The ONE seeder writes 0600 (owner-only).
	if info.Mode().Perm() != 0o600 {
		t.Errorf("seeded op-refs.env mode = %o, want 600", info.Mode().Perm())
	}
	if !strings.Contains(buf.String(), "seeded a template op-refs.env") {
		t.Errorf("expected the seeded-template note, got:\n%s", buf.String())
	}
}

// TestRegisterServers_RemoteSkipped: a cfg.MCP entry that is NEITHER gog NOR a
// local stdio server (per `pi-stack-host mcp --list`) is a remote gateway-
// catalog server: it is SKIPPED with an info line, never registered as local.
// This gate fails if the local-vs-remote guard is removed (notion would then be
// wrongly registered).
func TestRegisterServers_RemoteSkipped(t *testing.T) {
	f := fakeEnv{
		present: map[string]bool{"op": true}, // no sbx -> would-run printed
		output: map[string]string{
			"/usr/bin/pi-stack-host mcp --list": "slack\n", // notion is NOT local
		},
		envVars:  map[string]string{"PI_STACK_CONFIG": "/fake/config/config.toml", "SBX_MCP_URL": "https://gateway.docker.com"},
		statFile: map[string]bool{"/fake/config/op-refs.env": true},
	}
	cfg := defaultCfg()
	cfg.MCP = []string{"slack", "notion"}
	var buf bytes.Buffer
	if err := registerServers(cfg, f.env(), &buf, nil, hostStub("/usr/bin/pi-stack-host", nil)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "notion: gateway-catalog server, not locally registered") {
		t.Errorf("expected notion to be skipped as a gateway-catalog server, got:\n%s", out)
	}
	if strings.Contains(out, "sbx mcp add notion") {
		t.Errorf("notion (remote) must NOT be registered as local, got:\n%s", out)
	}
	if !strings.Contains(out, "sbx mcp add slack") {
		t.Errorf("slack (local) should still be registered, got:\n%s", out)
	}
}

// TestRegisterServers_LocalSetUnknownFailClosed: when the local-name list can't
// be established (pi-stack-host unresolved / `mcp --list` fails), a non-gog name
// must FAIL CLOSED — NOT be registered as a local pi-stack-host subcommand — and
// registerServers must return a non-nil error so the command exits non-zero.
// This gate fails if the old fall-back-to-local behavior returns.
func TestRegisterServers_LocalSetUnknownFailClosed(t *testing.T) {
	f := fakeEnv{
		present:  map[string]bool{"op": true, "sbx": true},
		output:   map[string]string{}, // no `mcp --list` output -> local set UNKNOWN
		envVars:  map[string]string{"PI_STACK_CONFIG": "/fake/config/config.toml", "SBX_MCP_URL": "https://gateway.docker.com"},
		statFile: map[string]bool{"/fake/config/op-refs.env": true},
	}
	cfg := defaultCfg()
	cfg.MCP = []string{"notion"} // a gateway-catalog name, NOT a local server
	var buf bytes.Buffer
	// hostStub fails: pi-stack-host cannot be resolved, so the local set is unknown.
	err := registerServers(cfg, f.env(), &buf, nil, hostStub("", errors.New("pi-stack-host not found")))
	if err == nil {
		t.Fatal("expected a non-nil error when the local set is unknown, got nil (silent success)")
	}
	if !strings.Contains(err.Error(), "could not determine local MCP servers") {
		t.Errorf("expected a fail-closed error, got %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "sbx mcp add notion") {
		t.Errorf("notion must NOT be registered as local when the local set is unknown, got:\n%s", out)
	}
	if !strings.Contains(out, "cannot determine local MCP servers") {
		t.Errorf("expected the fail-closed skip warning, got:\n%s", out)
	}
}

// TestRegisterServers_FailuresReturnError: when `sbx mcp add` fails for a
// server, register attempts it, prints the failure, AND returns a non-nil error
// so `pi-stack mcp register` exits non-zero.
func TestRegisterServers_FailuresReturnError(t *testing.T) {
	f := fakeEnv{
		present:  map[string]bool{"op": true, "gog": true, "sbx": true},
		output:   map[string]string{}, // no success output for the sbx add -> it "fails"
		envVars:  map[string]string{"PI_STACK_CONFIG": "/fake/config/config.toml", "SBX_MCP_URL": "https://gateway.docker.com"},
		statFile: map[string]bool{"/fake/config/op-refs.env": true},
	}
	cfg := defaultCfg()
	cfg.GogAccount = "me@x.com"
	var buf bytes.Buffer
	err := registerServers(cfg, f.env(), &buf, []string{"gog"}, hostStub("", nil))
	if err == nil || !strings.Contains(err.Error(), "failed to register") || !strings.Contains(err.Error(), "gog") {
		t.Errorf("expected a joined registration error mentioning gog, got %v", err)
	}
	if !strings.Contains(buf.String(), "FAILED to register: gog") {
		t.Errorf("expected the per-server failure line, got:\n%s", buf.String())
	}
}

// TestRegisterServers_GogNotFound: op present, gog requested but absent.
func TestRegisterServers_GogNotFound(t *testing.T) {
	f := fakeEnv{
		present:  map[string]bool{"op": true},
		output:   map[string]string{},
		envVars:  map[string]string{"PI_STACK_CONFIG": "/fake/config/config.toml", "SBX_MCP_URL": "https://gateway.docker.com"},
		statFile: map[string]bool{"/fake/config/op-refs.env": true},
	}
	cfg := defaultCfg()
	cfg.GogAccount = "me@x.com"
	var buf bytes.Buffer
	err := registerServers(cfg, f.env(), &buf, []string{"gog"}, hostStub("", nil))
	if err == nil || !strings.Contains(err.Error(), "brew install gog") {
		t.Errorf("expected a gog-not-found guard, got %v", err)
	}
}

// TestRegisterServers_GogAccountUnset: op+gog present but no account -> guide the
// config-set command (never file editing).
func TestRegisterServers_GogAccountUnset(t *testing.T) {
	f := fakeEnv{
		present:  map[string]bool{"op": true, "gog": true},
		output:   map[string]string{},
		envVars:  map[string]string{"PI_STACK_CONFIG": "/fake/config/config.toml", "SBX_MCP_URL": "https://gateway.docker.com"},
		statFile: map[string]bool{"/fake/config/op-refs.env": true},
	}
	cfg := defaultCfg() // GogAccount empty
	var buf bytes.Buffer
	err := registerServers(cfg, f.env(), &buf, []string{"gog"}, hostStub("", nil))
	if err == nil || !strings.Contains(err.Error(), "pi-stack config set gog_account") {
		t.Errorf("expected the config-set gog_account guide, got %v", err)
	}
}

// TestRegisterServers_SbxAbsentPrintsWouldRun: everything resolves but sbx is
// absent -> print the would-run command instead of crashing.
func TestRegisterServers_SbxAbsentPrintsWouldRun(t *testing.T) {
	f := fakeEnv{
		present:  map[string]bool{"op": true, "gog": true}, // no sbx
		output:   map[string]string{},
		envVars:  map[string]string{"PI_STACK_CONFIG": "/fake/config/config.toml", "SBX_MCP_URL": "https://gateway.docker.com"},
		statFile: map[string]bool{"/fake/config/op-refs.env": true},
	}
	cfg := defaultCfg()
	cfg.GogAccount = "me@x.com"
	var buf bytes.Buffer
	if err := registerServers(cfg, f.env(), &buf, []string{"gog"}, hostStub("", nil)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "sbx mcp add gog") || !strings.Contains(out, "me@x.com") {
		t.Errorf("expected a would-run command, got:\n%s", out)
	}
	if !strings.Contains(out, "install Docker Sandboxes") {
		t.Errorf("expected the sbx-absent note, got:\n%s", out)
	}
}

// TestRegisterServers_Registers: everything present + sbx runs -> registered.
func TestRegisterServers_Registers(t *testing.T) {
	f := fakeEnv{
		present:  map[string]bool{"op": true, "gog": true, "sbx": true},
		output:   map[string]string{},
		envVars:  map[string]string{"PI_STACK_CONFIG": "/fake/config/config.toml", "SBX_MCP_URL": "https://gateway.docker.com"},
		statFile: map[string]bool{"/fake/config/op-refs.env": true},
	}
	cfg := defaultCfg()
	cfg.GogAccount = "me@x.com"
	// Provide success output for the exact sbx call the registrar builds.
	reg := mcpRegistrar{op: "/usr/bin/op", opRefs: "/fake/config/op-refs.env", gog: "/usr/bin/gog", account: "me@x.com"}
	key := strings.Join(append([]string{"sbx"}, reg.addArgs("gog")...), " ")
	f.output[key] = "ok"
	var buf bytes.Buffer
	if err := registerServers(cfg, f.env(), &buf, []string{"gog"}, hostStub("", nil)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "registered: gog") {
		t.Errorf("expected registered: gog, got:\n%s", buf.String())
	}
}

// TestRegisterServers_DefaultsToConfigMCP: with no names, every entry in cfg.MCP
// is registered as a local stdio server. A non-gog name like "pio" registers as
// `pi-stack-host mcp pio` (sbx absent -> would-run is printed).
func TestRegisterServers_DefaultsToConfigMCP(t *testing.T) {
	f := fakeEnv{
		present:  map[string]bool{"op": true}, // no sbx -> would-run printed
		output:   map[string]string{"/usr/bin/pi-stack-host mcp --list": "pio\n"},
		envVars:  map[string]string{"PI_STACK_CONFIG": "/fake/config/config.toml", "SBX_MCP_URL": "https://gateway.docker.com"},
		statFile: map[string]bool{"/fake/config/op-refs.env": true},
	}
	cfg := defaultCfg()
	cfg.MCP = []string{"pio"} // an overlay local stdio server
	var buf bytes.Buffer
	if err := registerServers(cfg, f.env(), &buf, nil, hostStub("/usr/bin/pi-stack-host", nil)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "sbx mcp add pio") ||
		!strings.Contains(out, "/usr/bin/pi-stack-host --args mcp --args pio") {
		t.Errorf("expected pio registered as pi-stack-host mcp pio, got:\n%s", out)
	}
}

// TestRegisterServers_EmptyConfig: with no names and an empty cfg.MCP, there is
// nothing to register.
func TestRegisterServers_EmptyConfig(t *testing.T) {
	f := fakeEnv{present: map[string]bool{}, output: map[string]string{}}
	cfg := defaultCfg()
	cfg.MCP = nil
	var buf bytes.Buffer
	if err := registerServers(cfg, f.env(), &buf, nil, hostStub("", nil)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "Nothing to register") {
		t.Errorf("expected nothing-to-register for an empty mcp set, got:\n%s", buf.String())
	}
}
