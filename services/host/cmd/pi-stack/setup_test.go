package main

import (
	"bytes"
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
