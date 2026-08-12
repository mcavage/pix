package pack

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/packinfo"
	"pix/host/sys/systest"
)

// chatPack is the shape that deadlocked: an integration whose MCP server is a
// host COMMAND, plus the setup step that installs that command. Every pack that
// ships a server it also knows how to build has it.
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

// TestAdoptForSetup_RegistrationDeferredUntilSetupInstallsTheCommand is the
// regression for a deadlock a new user could not get out of.
//
// Adoption registers the pack's MCP servers, and a server whose command is not
// on PATH cannot be registered. On a FIRST adoption that is the normal state:
// the command is missing precisely because the setup step that installs it has
// not run yet. But adoption ran first and its failure returned, so the setup
// hooks never ran — and the error told the user to run "the pack's setup step",
// which is the thing it had just made unreachable. Steven hit exactly this on
// gm-pix-pack's slack-mcp, twice, on two different integrations.
//
// The pack IS adopted when this happens (registration is the last post-commit
// side effect), so the honest answer is to carry on: run the hooks, then ask
// again. What must NOT change is the verdict when it still fails afterwards.
func TestAdoptForSetup_RegistrationDeferredUntilSetupInstallsTheCommand(t *testing.T) {
	dir := isolatePackHost(t)
	root := chatPack(t, dir)

	installed := false
	fake := &systest.Fake{
		RunFn: func(string, ...string) (string, error) { return "", nil },
		RunTimedFn: func(name string, args ...string) (string, bool, error) {
			switch name {
			case "install-chat-mcp":
				installed = true
				return "", false, nil
			case "chat-mcp":
				if !installed {
					return "", false, errors.New("executable file not found in $PATH")
				}
				return "ok", false, nil
			}
			return "", false, nil
		},
	}

	var attempts int
	register := func(_ *config.Config, _ hostenv.Env, _ io.Writer, _ []string, _ map[string]config.MCPServer) error {
		attempts++
		if !installed {
			return fmt.Errorf("not registered because a required command is missing: chat (needs %q)", "chat-mcp")
		}
		return nil
	}

	var out bytes.Buffer
	if err := adoptForSetup(hostenv.Env{System: fake}, &out, register, nil, []string{root}, nil, true); err != nil {
		t.Fatalf("setup should complete once its own hook installs the command, got: %v\n%s", err, out.String())
	}
	if !installed {
		t.Fatal("the setup hook never ran — the deadlock is back")
	}
	if attempts != 2 {
		t.Errorf("register called %d times, want 2 (once during adoption, once after setup)", attempts)
	}
	if !strings.Contains(out.String(), "deferred") {
		t.Errorf("a deferred failure must be SAID, not swallowed; output was:\n%s", out.String())
	}
}

// TestAdoptForSetup_RegistrationStillFailsWhenSetupDoesNotFixIt: deferring is
// not forgiving. A command no hook installs is still a non-zero exit, or `pix
// setup` would report success over a server the sandbox will never see.
func TestAdoptForSetup_RegistrationStillFailsWhenSetupDoesNotFixIt(t *testing.T) {
	dir := isolatePackHost(t)
	root := filepath.Join(dir, "chatpack")
	mustWritePack(t, root, packinfo.Manifest{
		Name: "chatpack", Schema: 1,
		Integrations: []packinfo.Integration{{Name: "Chat", MCP: "chat", Command: "chat-mcp"}},
	})

	fake := &systest.Fake{
		RunFn:      func(string, ...string) (string, error) { return "", nil },
		RunTimedFn: func(string, ...string) (string, bool, error) { return "", false, nil },
	}
	register := func(*config.Config, hostenv.Env, io.Writer, []string, map[string]config.MCPServer) error {
		return errors.New("not registered because a required command is missing: chat (needs \"chat-mcp\")")
	}

	var out bytes.Buffer
	err := adoptForSetup(hostenv.Env{System: fake}, &out, register, nil, []string{root}, nil, true)
	if err == nil {
		t.Fatal("a registration failure that survives setup must still fail the run")
	}
	if !strings.Contains(err.Error(), "chat-mcp") {
		t.Errorf("the error must still name the missing command, got: %v", err)
	}
}

// TestAdoptForSetup_NonRegistrationFailureIsStillFatal: only the post-commit
// registration step is deferrable. A pack that adopted NOTHING — a refused
// trust gate, an unreadable manifest, a mismatched pin — must abort before any
// setup hook runs, because there is nothing set up to fix it.
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
