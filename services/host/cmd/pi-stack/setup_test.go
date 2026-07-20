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

// TestSetupSandboxExists: setup blocks only on a POSITIVELY present sandbox.
func TestSetupSandboxExists(t *testing.T) {
	cases := []struct {
		state sbxState
		block bool
	}{
		{sbxRunning, true},
		{sbxStopped, true},
		{sbxAbsent, false},
		{sbxUnknown, false},
	}
	for _, c := range cases {
		if got := setupSandboxExists(c.state); got != c.block {
			t.Errorf("setupSandboxExists(%v) = %v, want %v", c.state, got, c.block)
		}
	}
}

// TestSetupSandboxName derives pi-stack-<base> under the default profile.
func TestSetupSandboxName(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("PI_STACK_PROFILE", "")
	name, ok := setupSandboxName("/some/path/tact")
	if !ok {
		t.Fatal("expected name resolution to succeed under default profile")
	}
	if want := "pi-stack-tact"; name != want {
		t.Errorf("setupSandboxName = %q, want %q", name, want)
	}
}

// setupHostPhase must ABORT (return error, write nothing further) when no model
// key can be provisioned and it's non-interactive (no prompt) — the fix for the
// double-prompt + false "ready".
func TestSetupHostPhase_NoKeyAborts(t *testing.T) {
	env := shellEnv{
		lookPath: func(n string) (string, error) { return "/usr/bin/" + n, nil },
		readFile: func(string) (string, error) { return "", os.ErrNotExist },
		run: func(name string, args ...string) (string, error) {
			if name == "sbx" && len(args) >= 2 && args[0] == "secret" && args[1] == "ls" {
				return "github\n", nil // no model key
			}
			return "", nil
		},
	}
	var out bytes.Buffer
	// flags present -> non-interactive -> no prompt; missing key -> error.
	err := setupHostPhase(env, []string{"--yes"}, strings.NewReader(""), &out, true)
	if err == nil {
		t.Fatal("setup must abort when no model key is configured")
	}
	if strings.Contains(out.String(), "1Password") && strings.Contains(out.String(), "Manage model keys") {
		t.Error("non-interactive setup must not show the 1Password prompt")
	}
}

// host-state reports host readiness only when enabled AND provisioned.
func TestHostStateHostReadiness(t *testing.T) {
	cfg := &config.Config{MemoryWatcherModel: "x", MemoryEmbedModel: "y"}
	cfg.Host.Enabled = true
	hs := buildHostState(cfg, "", false, func(int) bool { return false }, false, "", hostStatePack{})
	if !hs.Host.Enabled {
		t.Error("host.enabled should reflect config")
	}
	// In this test env host mode is not provisioned, so Ready must be false even
	// though Enabled is true (the exact enabled!=ready bug).
	if hs.Host.Ready && !hs.Host.Provisioned {
		t.Error("Ready must be false when not provisioned")
	}
}

func TestProviderRefSet(t *testing.T) {
	mk := func(refs string) shellEnv {
		return shellEnv{
			getenv: func(k string) string {
				if k == "XDG_CONFIG_HOME" {
					return "/cfg"
				}
				return ""
			},
			readFile: func(p string) (string, error) {
				if p == filepath.Join("/cfg", "pi-stack", "op-refs.env") {
					return refs, nil
				}
				return "", os.ErrNotExist
			},
		}
	}
	if providerRefSet(mk(""), "ANTHROPIC_API_KEY") {
		t.Error("empty op-refs.env must report no ref")
	}
	if !providerRefSet(mk("ANTHROPIC_API_KEY=op://v/a/k\n"), "ANTHROPIC_API_KEY") {
		t.Error("a filled ref must be detected")
	}
	if providerRefSet(mk("OPENAI_API_KEY=op://v/o/k\n"), "ANTHROPIC_API_KEY") {
		t.Error("a different provider's ref must not count")
	}
}

// setupProvisionKeys, non-interactive with no op and no sbx: it must not prompt
// and must fail OPEN (true) so a box without sbx isn't blocked.
func TestSetupProvisionKeys_NoSbxFailsOpen(t *testing.T) {
	env := shellEnv{
		lookPath: func(string) (string, error) { return "", fmt.Errorf("not found") },
		getenv:   func(string) string { return "/cfg" },
		readFile: func(string) (string, error) { return "", os.ErrNotExist },
	}
	var out bytes.Buffer
	if !setupProvisionKeys(env, strings.NewReader(""), &out, false) {
		t.Error("must fail open (true) when sbx can't be probed")
	}
	if strings.Contains(out.String(), ": ") && strings.Contains(out.String(), "Paste an op://") {
		t.Error("non-interactive must not prompt for refs")
	}
}
