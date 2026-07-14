package main

import (
	"bytes"
	"os"
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

// TestRegisterServers_GatewayOff: SBX_MCP_URL unset -> a clear up-front guard
// naming the export, before any registration is attempted.
func TestRegisterServers_GatewayOff(t *testing.T) {
	f := fakeEnv{present: map[string]bool{"op": true, "gog": true}, output: map[string]string{}}
	cfg := defaultCfg()
	cfg.GogAccount = "me@x.com"
	var buf bytes.Buffer
	err := registerServers(cfg, f.env(), &buf, []string{"gog"}, hostStub("", nil))
	if err == nil || !strings.Contains(err.Error(), "SBX_MCP_URL=https://gateway.docker.com") {
		t.Errorf("expected an SBX_MCP_URL gateway guard, got %v", err)
	}
}

// TestRegisterServers_GogNoOpRefsBare: gateway on, op + op-refs ABSENT, gog
// present + account set -> gog registers DIRECTLY (bare command, no op wrapper)
// with the "registered gog directly" note. 1Password is optional for gog.
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
	if !strings.Contains(out, ".config/pi-stack/op-refs.env") {
		t.Errorf("note must reference the absolute XDG op-refs path, got:\n%s", out)
	}
	// The would-run command must be the bare gog command, not an op wrapper.
	if !strings.Contains(out, "sbx mcp add gog --command /usr/bin/gog") {
		t.Errorf("expected a bare gog would-run command, got:\n%s", out)
	}
}

// TestRegisterServers_SlackNeedsOp: slack strictly needs op + op-refs. With op
// absent, register must error clearly (naming 1password-cli), and reference the
// absolute XDG op-refs path — never a repo-relative config/op-refs.env.
func TestRegisterServers_SlackNeedsOp(t *testing.T) {
	f := fakeEnv{
		present: map[string]bool{}, // no op
		output:  map[string]string{},
		envVars: map[string]string{"SBX_MCP_URL": "https://gateway.docker.com", "HOME": "/home/me"},
		home:    "/home/me",
	}
	cfg := defaultCfg()
	var buf bytes.Buffer
	err := registerServers(cfg, f.env(), &buf, []string{"slack"}, hostStub("/usr/bin/pi-stack-host", nil))
	if err == nil || !strings.Contains(err.Error(), "1password-cli") {
		t.Errorf("expected slack to require op (1password-cli), got %v", err)
	}
}

// TestRegisterServers_SlackOpRefsAbsentSeeds: slack with op present but op-refs
// ABSENT -> seed a template at the absolute XDG path and error naming it.
func TestRegisterServers_SlackOpRefsAbsentSeeds(t *testing.T) {
	var wrote string
	env := (fakeEnv{
		present: map[string]bool{"op": true},
		output:  map[string]string{},
		envVars: map[string]string{"SBX_MCP_URL": "https://gateway.docker.com"},
		home:    "/home/me",
	}).env()
	env.writeFile = func(path string, data []byte, perm os.FileMode) error { wrote = path; return nil }
	cfg := defaultCfg()
	var buf bytes.Buffer
	err := registerServers(cfg, env, &buf, []string{"slack"}, hostStub("/usr/bin/pi-stack-host", nil))
	if err == nil || !strings.Contains(err.Error(), ".config/pi-stack/op-refs.env") {
		t.Errorf("expected an error naming the absolute XDG op-refs path, got %v", err)
	}
	if !strings.Contains(wrote, ".config/pi-stack/op-refs.env") {
		t.Errorf("expected the template seeded at the XDG path, wrote %q", wrote)
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

// TestRegisterServers_DefaultsToConfigMCP: with no names, registers the local
// stdio servers from cfg.MCP (and ignores remote catalog entries).
func TestRegisterServers_DefaultsToConfigMCP(t *testing.T) {
	f := fakeEnv{present: map[string]bool{}, output: map[string]string{}}
	cfg := defaultCfg()
	cfg.MCP = []string{"notion"} // remote-only, not a local stdio server
	var buf bytes.Buffer
	if err := registerServers(cfg, f.env(), &buf, nil, hostStub("", nil)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), "Nothing to register") {
		t.Errorf("expected nothing-to-register for a remote-only mcp set, got:\n%s", buf.String())
	}
}
