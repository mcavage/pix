package pack

import (
	"bytes"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/packinfo"
	"pix/host/sys/systest"
)

// chatPack is the shape this ordering exists for: an integration whose MCP
// server is a host COMMAND, plus the setup step that installs that command.
// Every pack that ships a server it also builds has it.
func chatPack(t *testing.T, dir string) string {
	t.Helper()
	root := filepath.Join(dir, "chatpack")
	mustWritePack(t, root, packinfo.Manifest{
		Name: "chatpack", Schema: 1,
		Integrations: []packinfo.Integration{
			{Name: "Chat", MCP: "chat", Command: "chat-mcp", Setup: "chat"},
		},
		Setup: []packinfo.SetupStep{{
			ID: "chat", Description: "Chat", Required: true,
			Require: []packinfo.SetupRequire{{Kind: "probe", Argv: []string{"chat-mcp", "status"}}},
			Apply:   []packinfo.SetupApply{{Kind: "exec", Argv: []string{"install-chat-mcp"}}},
		}},
	})
	return root
}

// installerEnv is a host where `chat-mcp` does not work until the pack's own
// setup step installs it — the state of every clean machine on a first install.
func installerEnv(installed *bool) hostenv.Env {
	return hostenv.Env{System: &systest.Fake{
		RunFn: func(string, ...string) (string, error) { return "", nil },
		RunTimedFn: func(name string, args ...string) (string, bool, error) {
			switch name {
			case "install-chat-mcp":
				*installed = true
				return "", false, nil
			case "chat-mcp":
				if !*installed {
					return "", false, errors.New("executable file not found in $PATH")
				}
				return "ok", false, nil
			}
			return "", false, nil
		},
	}}
}

// TestAdoptForSetup_RegistersOnlyAfterSetupInstalledTheCommand is the contract,
// and it is about ORDER rather than recovery.
//
// A pack's setup hooks install the commands its MCP servers ARE, so on a first
// install those binaries do not exist during adoption. Registering first cannot
// work, and two attempts at papering over that both failed the user: first
// `pix setup` died telling them to run the setup step its own failure had made
// unreachable; then it registered, warned, deferred, and retried — which worked,
// and made them read a warning about a missing command during the very run
// whose job was to install it.
//
// So registration happens after the hooks. Once. `installed` is false until the
// hook runs, so an attempt before that would show up as a second call.
func TestAdoptForSetup_RegistersOnlyAfterSetupInstalledTheCommand(t *testing.T) {
	dir := isolatePackHost(t)
	root := chatPack(t, dir)

	installed := false
	var attempts int
	installedAtRegistration := false
	register := func(_ *config.Config, _ hostenv.Env, _ io.Writer, _ []string, _ map[string]config.MCPServer) error {
		attempts++
		installedAtRegistration = installed
		return nil
	}

	var out bytes.Buffer
	if err := adoptForSetup(installerEnv(&installed), &out, register, nil, []string{root}, nil, true); err != nil {
		t.Fatalf("setup should complete: %v\n%s", err, out.String())
	}
	if !installed {
		t.Fatal("the setup hook never ran")
	}
	if attempts != 1 {
		t.Errorf("register called %d times, want exactly 1 — not once before setup and again after", attempts)
	}
	if !installedAtRegistration {
		t.Error("registration ran before the setup hook installed the command")
	}
}

// TestAdoptForSetup_SaysNothingAboutRegistrationOrder: the user asked for a pack
// to be set up. The sequence pix uses to do that is not news, and a warning
// about a command that is missing for twenty seconds of a run whose job is to
// install it reads as a failure. None of it reaches the terminal.
func TestAdoptForSetup_SaysNothingAboutRegistrationOrder(t *testing.T) {
	dir := isolatePackHost(t)
	root := chatPack(t, dir)

	installed := false
	var out bytes.Buffer
	if err := adoptForSetup(installerEnv(&installed), &out, registerOK, nil, []string{root}, nil, true); err != nil {
		t.Fatalf("setup should complete: %v", err)
	}
	for _, noise := range []string{"not on PATH", "not registered", "deferred", "retrying"} {
		if strings.Contains(out.String(), noise) {
			t.Errorf("output mentions %q — pix narrating its own ordering:\n%s", noise, out.String())
		}
	}
}

// TestAdoptForSetup_RegistrationFailureStillFails: ordering is not forgiveness.
// A server that genuinely cannot register — after every hook has run — is a
// non-zero exit, or setup claims success over a server the sandbox never sees.
func TestAdoptForSetup_RegistrationFailureStillFails(t *testing.T) {
	dir := isolatePackHost(t)
	root := chatPack(t, dir)

	installed := false
	register := func(*config.Config, hostenv.Env, io.Writer, []string, map[string]config.MCPServer) error {
		return errors.New("sbx refused: chat")
	}

	var out bytes.Buffer
	err := adoptForSetup(installerEnv(&installed), &out, register, nil, []string{root}, nil, true)
	if err == nil {
		t.Fatal("a registration failure after setup must fail the run")
	}
	if !strings.Contains(err.Error(), "chat") {
		t.Errorf("the error must name what failed, got: %v", err)
	}
}

// TestAdoptForSetup_RegistersEvenWhenAStepFails: a pack's steps are
// independent. An unrelated step dying — a broken OAuth scope, an expired grant
// — says nothing about the command an earlier step installed, or about the
// remote servers that needed no setup at all. Skipping registration there is a
// run that half-works and has to be done twice.
func TestAdoptForSetup_RegistersEvenWhenAStepFails(t *testing.T) {
	dir := isolatePackHost(t)
	root := filepath.Join(dir, "chatpack")
	mustWritePack(t, root, packinfo.Manifest{
		Name: "chatpack", Schema: 1,
		Integrations: []packinfo.Integration{{Name: "Chat", MCP: "chat", Command: "chat-mcp", Setup: "grant"}},
		Setup: []packinfo.SetupStep{{
			ID: "grant", Description: "Grant", Required: true,
			Require: []packinfo.SetupRequire{{Kind: "probe", Argv: []string{"chat-mcp", "auth", "status"}}},
			Apply:   []packinfo.SetupApply{{Kind: "exec", Argv: []string{"chat-mcp", "auth", "login"}}},
		}},
	})

	fake := &systest.Fake{
		RunFn:      func(string, ...string) (string, error) { return "", nil },
		RunTimedFn: func(string, ...string) (string, bool, error) { return "", false, errors.New("invalid_scope") },
	}
	var attempts int
	register := func(*config.Config, hostenv.Env, io.Writer, []string, map[string]config.MCPServer) error {
		attempts++
		return nil
	}

	var out bytes.Buffer
	err := adoptForSetup(hostenv.Env{System: fake}, &out, register, nil, []string{root}, nil, true)
	if err == nil {
		t.Fatal("a failed required step must still fail the run")
	}
	if attempts != 1 {
		t.Errorf("register called %d times, want 1 — one step failing must not cost the pack its registrations", attempts)
	}
}

// TestAdoptForSetup_NonRegistrationFailureIsStillFatal: a pack that adopted
// NOTHING — a refused trust gate, an unreadable manifest, a mismatched pin —
// aborts before any hook runs, because there is nothing set up to fix it.
func TestAdoptForSetup_NonRegistrationFailureIsStillFatal(t *testing.T) {
	dir := isolatePackHost(t)
	fake := &systest.Fake{
		RunFn:      func(string, ...string) (string, error) { return "", nil },
		RunTimedFn: func(string, ...string) (string, bool, error) { return "", false, nil },
	}
	missing := filepath.Join(dir, "no-such-pack")

	var out bytes.Buffer
	err := adoptForSetup(hostenv.Env{System: fake}, &out, registerOK, nil, []string{missing}, nil, true)
	if err == nil {
		t.Fatal("adopting a pack that does not exist must fail")
	}
	if !strings.Contains(err.Error(), "adopting pack") {
		t.Errorf("a pre-commit failure keeps its own framing, got: %v", err)
	}
}
