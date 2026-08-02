package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"pix/host/sys"
	"pix/host/sys/systest"
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
		op:       "/usr/bin/op",
		opRefs:   "/abs/config/op-refs.env",
		gog:      "/usr/bin/gog",
		account:  "me@x.com",
		hostBin:  "/usr/bin/pix-host",
		gogUseOp: true,
	}
}

// TestAddArgs_Gog builds the hardened gog `sbx mcp add` command (mirrors the
// Makefile mcp-register line).
func TestAddArgs_Gog(t *testing.T) {
	args := gogRegistrar().addArgs(gwServerName)
	if !contains(args, []string{"mcp", "add", gwServerName, "--command", "/usr/bin/op"}) {
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

// TestAddArgs_Slack builds the pix-host subcommand form (op-run wrapped).
func TestAddArgs_Slack(t *testing.T) {
	args := gogRegistrar().addArgs("slack")
	if !contains(args, []string{"--args", "/usr/bin/pix-host", "--args", "mcp", "--args", "slack"}) {
		t.Errorf("slack should register pix-host mcp slack, got %v", args)
	}
}

// TestAddArgs_LocalServer builds the pix-host subcommand form for an
// arbitrary local stdio server like "pio": it registers as
// `pix-host mcp pio`, exactly like slack, via the serverCmd default.
func TestAddArgs_LocalServer(t *testing.T) {
	args := gogRegistrar().addArgs("pio")
	if !contains(args, []string{"mcp", "add", "pio", "--command", "/usr/bin/op"}) {
		t.Errorf("missing add-command prefix for pio in %v", args)
	}
	if !contains(args, []string{"--args", "/usr/bin/pix-host", "--args", "mcp", "--args", "pio"}) {
		t.Errorf("pio should register pix-host mcp pio, got %v", args)
	}
}

// TestAddArgs_GogBare: with NO op-refs (opRefs=""), gog registers DIRECTLY as a
// bare command — no `op run` wrapper. 1Password is optional for gog.
func TestAddArgs_GogBare(t *testing.T) {
	reg := gogRegistrar()
	reg.opRefs = "" // no op-refs -> bare
	args := reg.addArgs(gwServerName)
	if !contains(args, []string{"mcp", "add", gwServerName, "--command", "/usr/bin/gog"}) {
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

// TestRegisterServers_GogNoOpRefsBare: gateway on, op + op-refs ABSENT, gog
// present + account set -> gog registers DIRECTLY (bare command, no op wrapper)
// with the OAuth note. gog uses guided OAuth (`pix gworkspace setup`), never
// op-refs, so the note must NOT mention op-refs.
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
	if err := registerServers(cfg, f.env(), &buf, []string{gwServerName}, hostStub("", nil), nil); !errors.Is(err, errSbxUnavailable) {
		t.Fatalf("expected errSbxUnavailable, got: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "registered gog directly") {
		t.Errorf("expected the bare-gog note, got:\n%s", out)
	}
	if strings.Contains(out, "op-refs.env") {
		t.Errorf("gog-only note must NOT mention op-refs (gog uses OAuth), got:\n%s", out)
	}
	// The would-run command must be the bare gog command, not an op wrapper.
	if !strings.Contains(out, "sbx mcp add google-workspace --command /usr/bin/gog") {
		t.Errorf("expected a bare gog would-run command, got:\n%s", out)
	}
}

// TestRegisterServers_GogOnlyNoSeed (R5-1/R5-2): a gog-only clean state with op
// NOT resolvable must NOT create op-refs.env at the XDG path and must NOT print a
// "seeded" line. gog authenticates via guided OAuth (`pix gworkspace setup`),
// never op-refs, so seeding one contradicts setup Step 4's "No file is
// created" copy.
func TestRegisterServers_GogOnlyNoSeed(t *testing.T) {
	home := t.TempDir()
	env := (fakeEnv{
		present: map[string]bool{"gog": true}, // op absent -> not resolvable
		output:  map[string]string{},
		envVars: map[string]string{"SBX_MCP_URL": "https://gateway.docker.com"},
		home:    home,
	}).env()
	cfg := defaultCfg()
	cfg.MCP = []string{gwServerName}
	cfg.GogAccount = "me@x.com"
	var buf bytes.Buffer
	if err := registerServers(cfg, env, &buf, nil, hostStub("/usr/bin/pix-host", nil), nil); !errors.Is(err, errSbxUnavailable) {
		t.Fatalf("expected errSbxUnavailable, got: %v", err)
	}
	seeded := filepath.Join(home, ".config", "pix", "op-refs.env")
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
		output:  map[string]string{"/usr/bin/pix-host mcp --list": "slack\n"},
		envVars: map[string]string{"SBX_MCP_URL": "https://gateway.docker.com", "HOME": "/home/me"},
		home:    "/home/me",
	}
	cfg := defaultCfg()
	var buf bytes.Buffer
	if err := registerServers(cfg, f.env(), &buf, []string{"slack"}, hostStub("/usr/bin/pix-host", nil), nil); !errors.Is(err, errSbxUnavailable) {
		t.Fatalf("expected errSbxUnavailable, got: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "registered slack directly (bare, no 1Password)") {
		t.Errorf("expected the bare-registration note for slack, got:\n%s", out)
	}
	// Bare command: --command /usr/bin/pix-host, no op-run wrapper.
	if !strings.Contains(out, "sbx mcp add slack --command /usr/bin/pix-host --args mcp --args slack") {
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
		output:  map[string]string{"/usr/bin/pix-host mcp --list": "slack\n"},
		envVars: map[string]string{"SBX_MCP_URL": "https://gateway.docker.com"},
		home:    home,
	}).env()
	cfg := defaultCfg()
	var buf bytes.Buffer
	if err := registerServers(cfg, env, &buf, []string{"slack"}, hostStub("/usr/bin/pix-host", nil), nil); !errors.Is(err, errSbxUnavailable) {
		t.Fatalf("expected errSbxUnavailable, got: %v", err)
	}
	seeded := filepath.Join(home, ".config", "pix", "op-refs.env")
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
// local stdio server (per `pix-host mcp --list`) is a remote gateway-
// catalog server: it is SKIPPED with an info line, never registered as local.
// This gate fails if the local-vs-remote guard is removed (notion would then be
// wrongly registered).
func TestRegisterServers_RemoteSkipped(t *testing.T) {
	f := fakeEnv{
		present: map[string]bool{"op": true}, // no sbx -> would-run printed
		output: map[string]string{
			"/usr/bin/pix-host mcp --list": "slack\n", // notion is NOT local
		},
		envVars:  map[string]string{"PIX_CONFIG": "/fake/config/config.toml", "SBX_MCP_URL": "https://gateway.docker.com"},
		statFile: map[string]bool{"/fake/config/op-refs.env": true},
	}
	cfg := defaultCfg()
	cfg.MCP = []string{"slack", "notion"}
	var buf bytes.Buffer
	if err := registerServers(cfg, f.env(), &buf, nil, hostStub("/usr/bin/pix-host", nil), nil); !errors.Is(err, errSbxUnavailable) {
		t.Fatalf("expected errSbxUnavailable, got: %v", err)
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

// TestRegisterServers_RemoteWithURLRegistered: a remote gateway-catalog name that
// the pack carries a URL for (containers[name].RemoteURL set) is NOT skipped — it
// is registered via `sbx mcp add <name> --url <url>`, with no --local and no
// op-run wrapper. This is the pack-self-registers-remotes path; it fails if the
// RemoteURL branch is dropped and meetings/notion/etc. fall back to the skip line.
func TestRegisterServers_RemoteWithURLRegistered(t *testing.T) {
	f := fakeEnv{
		present: map[string]bool{"op": true}, // no sbx -> would-run printed
		output: map[string]string{
			"/usr/bin/pix-host mcp --list": "slack\n", // meetings is NOT a local server
		},
		envVars:  map[string]string{"PIX_CONFIG": "/fake/config/config.toml"},
		statFile: map[string]bool{"/fake/config/op-refs.env": true},
	}
	cfg := defaultCfg()
	cfg.MCP = []string{"meetings"}
	containers := map[string]packContainer{"meetings": {RemoteURL: "https://app.trymeetings.com/mcp"}}
	var buf bytes.Buffer
	if err := registerServers(cfg, f.env(), &buf, nil, hostStub("/usr/bin/pix-host", nil), containers); !errors.Is(err, errSbxUnavailable) {
		t.Fatalf("expected errSbxUnavailable, got: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "sbx mcp add meetings --url https://app.trymeetings.com/mcp") {
		t.Errorf("expected meetings registered via --url, got:\n%s", out)
	}
	if strings.Contains(out, "gateway-catalog server, not locally registered") {
		t.Errorf("meetings with a pack URL must NOT be skipped, got:\n%s", out)
	}
	if strings.Contains(out, "--local") || strings.Contains(out, "--command") {
		t.Errorf("remote-url server must not use --local/--command, got:\n%s", out)
	}
}

func TestRegisterServers_RemoteURLUsesInteractiveRunner(t *testing.T) {
	env := fakeEnv{
		present: map[string]bool{"sbx": true},
		output:  map[string]string{"/usr/bin/pix-host mcp --list": "slack\n"},
	}.env()
	var interactive []string
	systest.Of(env.System).RunInteractiveFn = func(name string, args ...string) error {
		interactive = append([]string{name}, args...)
		return nil
	}
	systest.Of(env.System).RunTimedFn = func(name string, args ...string) (string, bool, error) {
		if name == "sbx" && len(args) >= 2 && args[1] == "add" {
			t.Fatalf("remote OAuth registration was sent through the bounded probe: %s %s", name, strings.Join(args, " "))
		}
		if name == "sbx" {
			return "", false, errors.New("not registered")
		}
		return "slack\n", false, nil
	}
	cfg := defaultCfg()
	cfg.MCP = []string{"meetings"}
	containers := map[string]packContainer{"meetings": {RemoteURL: "https://app.trymeetings.com/mcp"}}
	var out bytes.Buffer
	if err := registerServers(cfg, env, &out, nil, hostStub("/usr/bin/pix-host", nil), containers); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(interactive, " "); got != "sbx mcp add meetings --url https://app.trymeetings.com/mcp" {
		t.Fatalf("interactive registration = %q", got)
	}
}

func TestRegisterServers_CurrentRemoteDoesNotReopenOAuth(t *testing.T) {
	endpoint := "https://app.trymeetings.com/mcp"
	env := fakeEnv{present: map[string]bool{"sbx": true}}.env()
	systest.Of(env.System).RunTimedFn = func(name string, args ...string) (string, bool, error) {
		command := strings.Join(args, " ")
		if name == "sbx" && command == "mcp inspect meetings" {
			return "URL: " + endpoint, false, nil
		}
		if name == "sbx" && command == "mcp auth status meetings" {
			return "meetings: authorized", false, nil
		}
		return "slack\n", false, nil
	}
	systest.Of(env.System).RunInteractiveFn = func(string, ...string) error {
		t.Fatal("an unchanged registered remote must not reopen OAuth")
		return nil
	}
	cfg := defaultCfg()
	cfg.MCP = []string{"meetings"}
	if err := registerServers(cfg, env, &bytes.Buffer{}, nil, hostStub("/usr/bin/pix-host", nil),
		map[string]packContainer{"meetings": {RemoteURL: endpoint}}); err != nil {
		t.Fatal(err)
	}
}

func TestRegisterServers_CurrentUnauthorizedRemoteRepairsOAuthOnce(t *testing.T) {
	endpoint := "https://app.trymeetings.com/mcp"
	authorized := false
	env := fakeEnv{present: map[string]bool{"sbx": true}}.env()
	systest.Of(env.System).RunTimedFn = func(name string, args ...string) (string, bool, error) {
		command := strings.Join(args, " ")
		switch command {
		case "mcp inspect meetings":
			return `{"url":"` + endpoint + `"}`, false, nil
		case "mcp auth status meetings":
			if authorized {
				return "meetings: authorized", false, nil
			}
			return "meetings: not authenticated", false, nil
		default:
			return "slack\n", false, nil
		}
	}
	var interactive []string
	systest.Of(env.System).RunInteractiveFn = func(name string, args ...string) error {
		interactive = append([]string{name}, args...)
		authorized = true
		return nil
	}
	cfg := defaultCfg()
	cfg.MCP = []string{"meetings"}
	if err := registerServers(cfg, env, &bytes.Buffer{}, nil, hostStub("/usr/bin/pix-host", nil),
		map[string]packContainer{"meetings": {RemoteURL: endpoint}}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(interactive, " "); got != "sbx mcp auth meetings" {
		t.Fatalf("repair command = %q", got)
	}
}

func TestRemoteMCPRegistrationCurrentRejectsEndpointSubstring(t *testing.T) {
	want := "https://expected.example/mcp"
	env := shellEnv{System: &systest.Fake{RunTimedFn: func(string, ...string) (string, bool, error) {
		return `{"url":"https://evil.example/?next=https://expected.example/mcp"}`, false, nil
	}}}
	if remoteMCPRegistrationCurrent(env, "meetings", want) {
		t.Fatal("an endpoint embedded inside another URL must not count as the registered endpoint")
	}
}

func TestRemoteMCPRegistrationCurrentRequiresEndpointField(t *testing.T) {
	want := "https://expected.example/mcp?a=1&b=2"
	for _, payload := range []string{
		`{"url":"https://evil.example/mcp","note":"https://expected.example/mcp?a=1&b=2"}`,
		`{"callback_url":"https://expected.example/mcp?a=1&b=2"}`,
		`{"url":"https://evil.example/mcp","nested":{"endpoint":"https://expected.example/mcp?a=1&b=2"}}`,
		`url: https://evil.example/?next=https://expected.example/mcp?a=1&b=2`,
	} {
		env := shellEnv{System: &systest.Fake{RunTimedFn: func(string, ...string) (string, bool, error) { return payload, false, nil }}}
		if remoteMCPRegistrationCurrent(env, "meetings", want) {
			t.Fatalf("non-endpoint evidence was trusted: %s", payload)
		}
	}
}

func TestRemoteMCPRegistrationCurrentCanonicalExactMatch(t *testing.T) {
	want := "https://expected.example/mcp?b=2&a=1"
	env := shellEnv{System: &systest.Fake{RunTimedFn: func(string, ...string) (string, bool, error) {
		return `{"server":{"remote_url":"HTTPS://EXPECTED.EXAMPLE:443/mcp?a=1&b=2"}}`, false, nil
	}}}
	if !remoteMCPRegistrationCurrent(env, "meetings", want) {
		t.Fatal("canonically identical endpoint field was not recognized")
	}
}

// TestRegisterServers_LocalSetUnknownFailClosed: when the local-name list can't
// be established (pix-host unresolved / `mcp --list` fails), a non-gog name
// must FAIL CLOSED — NOT be registered as a local pix-host subcommand — and
// registerServers must return a non-nil error so the command exits non-zero.
// This gate fails if the old fall-back-to-local behavior returns.
func TestRegisterServers_LocalSetUnknownFailClosed(t *testing.T) {
	f := fakeEnv{
		present:  map[string]bool{"op": true, "sbx": true},
		output:   map[string]string{}, // no `mcp --list` output -> local set UNKNOWN
		envVars:  map[string]string{"PIX_CONFIG": "/fake/config/config.toml", "SBX_MCP_URL": "https://gateway.docker.com"},
		statFile: map[string]bool{"/fake/config/op-refs.env": true},
	}
	cfg := defaultCfg()
	cfg.MCP = []string{"notion"} // a gateway-catalog name, NOT a local server
	var buf bytes.Buffer
	// hostStub fails: pix-host cannot be resolved, so the local set is unknown.
	err := registerServers(cfg, f.env(), &buf, nil, hostStub("", errors.New("pix-host not found")), nil)
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
// so `pix mcp register` exits non-zero.
func TestRegisterServers_FailuresReturnError(t *testing.T) {
	f := fakeEnv{
		present:  map[string]bool{"op": true, "gog": true, "sbx": true},
		output:   map[string]string{}, // no success output for the sbx add -> it "fails"
		envVars:  map[string]string{"PIX_CONFIG": "/fake/config/config.toml", "SBX_MCP_URL": "https://gateway.docker.com"},
		statFile: map[string]bool{"/fake/config/op-refs.env": true},
	}
	cfg := defaultCfg()
	cfg.GogAccount = "me@x.com"
	var buf bytes.Buffer
	err := registerServers(cfg, f.env(), &buf, []string{gwServerName}, hostStub("", nil), nil)
	if err == nil || !strings.Contains(err.Error(), "failed to register") || !strings.Contains(err.Error(), gwServerName) {
		t.Errorf("expected a joined registration error mentioning gog, got %v", err)
	}
	if !strings.Contains(buf.String(), "FAILED to register: "+gwServerName) {
		t.Errorf("expected the per-server failure line, got:\n%s", buf.String())
	}
}

// TestRegisterServers_GogNotFound: op present, gog requested but absent.
func TestRegisterServers_GogNotFound(t *testing.T) {
	f := fakeEnv{
		present:  map[string]bool{"op": true},
		output:   map[string]string{},
		envVars:  map[string]string{"PIX_CONFIG": "/fake/config/config.toml", "SBX_MCP_URL": "https://gateway.docker.com"},
		statFile: map[string]bool{"/fake/config/op-refs.env": true},
	}
	cfg := defaultCfg()
	cfg.GogAccount = "me@x.com"
	var buf bytes.Buffer
	err := registerServers(cfg, f.env(), &buf, []string{gwServerName}, hostStub("", nil), nil)
	if err == nil || !strings.Contains(err.Error(), "brew install openclaw/tap/gogcli") {
		t.Errorf("expected a gog-not-found guard, got %v", err)
	}
}

// TestRegisterServers_GogAccountUnset: op+gog present but no account -> guide the
// config-set command (never file editing).
func TestRegisterServers_GogAccountUnset(t *testing.T) {
	f := fakeEnv{
		present:  map[string]bool{"op": true, "gog": true},
		output:   map[string]string{},
		envVars:  map[string]string{"PIX_CONFIG": "/fake/config/config.toml", "SBX_MCP_URL": "https://gateway.docker.com"},
		statFile: map[string]bool{"/fake/config/op-refs.env": true},
	}
	cfg := defaultCfg() // GogAccount empty
	var buf bytes.Buffer
	err := registerServers(cfg, f.env(), &buf, []string{gwServerName}, hostStub("", nil), nil)
	if err == nil || !strings.Contains(err.Error(), "pix config set google_workspace_account") {
		t.Errorf("expected the config-set google_workspace_account guide, got %v", err)
	}
}

// TestRegisterServers_SbxAbsentPrintsWouldRun: everything resolves but sbx is
// absent -> print the would-run command instead of crashing.
func TestRegisterServers_SbxAbsentPrintsWouldRun(t *testing.T) {
	f := fakeEnv{
		present:  map[string]bool{"op": true, "gog": true}, // no sbx
		output:   map[string]string{},
		envVars:  map[string]string{"PIX_CONFIG": "/fake/config/config.toml", "SBX_MCP_URL": "https://gateway.docker.com"},
		statFile: map[string]bool{"/fake/config/op-refs.env": true},
	}
	cfg := defaultCfg()
	cfg.GogAccount = "me@x.com"
	var buf bytes.Buffer
	if err := registerServers(cfg, f.env(), &buf, []string{gwServerName}, hostStub("", nil), nil); !errors.Is(err, errSbxUnavailable) {
		t.Fatalf("expected errSbxUnavailable, got: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "sbx mcp add google-workspace") || !strings.Contains(out, "me@x.com") {
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
		envVars:  map[string]string{"PIX_CONFIG": "/fake/config/config.toml", "SBX_MCP_URL": "https://gateway.docker.com"},
		statFile: map[string]bool{"/fake/config/op-refs.env": true},
	}
	cfg := defaultCfg()
	cfg.GogAccount = "me@x.com"
	// Provide success output for the exact sbx call the registrar builds.
	reg := mcpRegistrar{op: "/usr/bin/op", opRefs: "/fake/config/op-refs.env", gog: "/usr/bin/gog", account: "me@x.com"}
	key := strings.Join(append([]string{"sbx"}, reg.addArgs(gwServerName)...), " ")
	if strings.Contains(key, "--command /usr/bin/op") {
		t.Fatalf("unrelated op-refs must not wrap normal gog OAuth: %s", key)
	}
	f.output[key] = "ok"
	var buf bytes.Buffer
	if err := registerServers(cfg, f.env(), &buf, []string{gwServerName}, hostStub("", nil), nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "registered: google-workspace") {
		t.Errorf("expected registered: google-workspace, got:\n%s", buf.String())
	}
	if strings.Contains(buf.String(), "Each wrapped server") {
		t.Errorf("bare gog registration claimed to use an op wrapper:\n%s", buf.String())
	}
}

// TestRegisterServers_DefaultsToConfigMCP: with no names, every entry in cfg.MCP
// is registered as a local stdio server. A non-gog name like "pio" registers as
// `pix-host mcp pio` (sbx absent -> would-run is printed).
func TestRegisterServers_DefaultsToConfigMCP(t *testing.T) {
	f := fakeEnv{
		present:  map[string]bool{"op": true}, // no sbx -> would-run printed
		output:   map[string]string{"/usr/bin/pix-host mcp --list": "pio\n"},
		envVars:  map[string]string{"PIX_CONFIG": "/fake/config/config.toml", "SBX_MCP_URL": "https://gateway.docker.com"},
		statFile: map[string]bool{"/fake/config/op-refs.env": true},
	}
	cfg := defaultCfg()
	cfg.MCP = []string{"pio"} // an arbitrary local stdio server
	var buf bytes.Buffer
	if err := registerServers(cfg, f.env(), &buf, nil, hostStub("/usr/bin/pix-host", nil), nil); !errors.Is(err, errSbxUnavailable) {
		t.Fatalf("expected errSbxUnavailable, got: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "sbx mcp add pio") ||
		!strings.Contains(out, "/usr/bin/pix-host --args mcp --args pio") {
		t.Errorf("expected pio registered as pix-host mcp pio, got:\n%s", out)
	}
}

// TestRegisterServers_EmptyConfig: with no names and an empty cfg.MCP, there is
// nothing to register.
func TestRegisterServers_EmptyConfig(t *testing.T) {
	f := fakeEnv{present: map[string]bool{}, output: map[string]string{}}
	cfg := defaultCfg()
	cfg.MCP = nil
	var buf bytes.Buffer
	if err := registerServers(cfg, f.env(), &buf, nil, hostStub("", nil), nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "Nothing to register") {
		t.Errorf("expected nothing-to-register for an empty mcp set, got:\n%s", buf.String())
	}
}

// TestMcpRegistrar_ContainerAddArgs pins the pack CONTAINER integration path:
// a name with a manifest registers via `--local --url <manifest>` and is NOT
// op-run wrapped (its creds are Docker-side), while a normal local server still
// gets the op-run wrapper when op-refs is present.
func TestMcpRegistrar_ContainerAddArgs(t *testing.T) {
	reg := mcpRegistrar{
		op:      "/usr/bin/op",
		opRefs:  "/abs/op-refs.env",
		hostBin: "/usr/local/bin/pix-host",
		containers: map[string]packContainer{
			"notion-ish": {Manifest: "https://example.com/mcp/x/server.json"},
			"hr":         {Image: "hr-mcp:0.0.1", EnvKeys: []string{"HR_API_KEY"}, EnvValues: map[string]string{"HR_COMPANY_DOMAIN": "acme"}},
			"meetings":   {RemoteURL: "https://app.trymeetings.com/mcp"},
		},
	}

	// Manifest container: --local --url, NOT op-run wrapped.
	got := strings.Join(reg.addArgs("notion-ish"), " ")
	want := "mcp add notion-ish --local --url https://example.com/mcp/x/server.json"
	if got != want {
		t.Fatalf("manifest addArgs:\n got: %s\nwant: %s", got, want)
	}

	// Remote container: --url (remote endpoint), NO --local and NOT op-run wrapped
	// (OAuth is handled host-side by the gateway).
	rem := strings.Join(reg.addArgs("meetings"), " ")
	if rem != "mcp add meetings --url https://app.trymeetings.com/mcp" {
		t.Fatalf("remote-url addArgs:\n got: %s\nwant: mcp add meetings --url https://app.trymeetings.com/mcp", rem)
	}
	if strings.Contains(rem, "--local") || strings.Contains(rem, "--command") {
		t.Fatalf("remote-url container must not use --local/--command, got:\n%s", rem)
	}

	// Image container: op-run-wrapped `docker run -i --rm -e KEY… <image>`.
	img := strings.Join(reg.addArgs("hr"), " ")
	for _, must := range []string{
		"mcp add hr --command /usr/bin/op",
		"--args run", "--args --env-file=/abs/op-refs.env", "--args --",
		"--args docker --args run --args -i --args --rm",
		"--args -e --args HR_API_KEY --args -e --args HR_COMPANY_DOMAIN=acme",
		"--args hr-mcp:0.0.1",
	} {
		if !strings.Contains(img, must) {
			t.Fatalf("image addArgs missing %q in:\n%s", must, img)
		}
	}
	if strings.Contains(img, "--local") {
		t.Fatalf("image container must not use --local, got:\n%s", img)
	}

	// A non-container name (slack) must still be op-run wrapped, not --local.
	slack := reg.addArgs("slack")
	if len(slack) < 5 || slack[3] != "--command" || slack[4] != "/usr/bin/op" {
		t.Fatalf("non-container server must be op-run wrapped, got: %v", slack)
	}
	if strings.Contains(strings.Join(slack, " "), "--local") {
		t.Fatalf("non-container server must not use --local, got: %v", slack)
	}
}

func TestBuildGogRegistrarIgnoresUnrelatedOpRefs(t *testing.T) {
	dir := t.TempDir()
	refs := filepath.Join(dir, "op-refs.env")
	if err := os.WriteFile(refs, []byte("EXAMPLE_API_KEY=op://Private/Example/key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Real OS (this test writes actual files into dir), with two seams faked.
	// systest.Base makes that intent explicit instead of it being a side effect
	// of which fields happened to be left nil.
	env := shellEnv{System: &systest.Fake{
		Base:       sys.Real{},
		LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		GetenvFn: func(key string) string {
			if key == "PIX_CONFIG" {
				return filepath.Join(dir, "config.toml")
			}
			return ""
		},
	}}
	reg := buildGogRegistrar(env, "/usr/bin/gog", "you@example.com")
	if reg.opRefs != "" || strings.HasSuffix(reg.execArgv(gwServerName)[0], "/op") {
		t.Fatalf("unrelated refs wrapped gog: %+v", reg.execArgv(gwServerName))
	}
	if err := os.WriteFile(refs, []byte("GOG_KEYRING_PASSWORD=op://Private/Gog/password\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reg = buildGogRegistrar(env, "/usr/bin/gog", "you@example.com")
	if reg.opRefs == "" {
		t.Fatal("explicit gog file-keyring password ref did not enable op wrapper")
	}
}
