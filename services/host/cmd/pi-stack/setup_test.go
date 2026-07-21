package main

import (
	"bytes"
	"encoding/json"
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

func TestHostModeProviderKeys(t *testing.T) {
	mk := func(hostmode string) shellEnv {
		return shellEnv{
			getenv: func(k string) string {
				if k == "XDG_CONFIG_HOME" {
					return "/cfg"
				}
				return ""
			},
			readFile: func(p string) (string, error) {
				if p == filepath.Join("/cfg", "pi-stack", "hostmode.env") {
					return hostmode, nil
				}
				return "", os.ErrNotExist
			},
		}
	}
	if got := hostModeProviderKeys(mk("")); len(got) != 0 {
		t.Errorf("empty hostmode.env: want none, got %v", got)
	}
	got := hostModeProviderKeys(mk("OPENAI_API_KEY=op://v/o/k\nANTHROPIC_API_KEY=op://v/a/k\nSLACK_TOKEN=op://v/s/t\n"))
	if len(got) != 2 || got[0] != "anthropic" || got[1] != "openai" {
		t.Errorf("want [anthropic openai] sorted, got %v", got)
	}
}

// writeOnboardingMarker writes the full five-capability checklist (all false)
// plus the persisted turns counter, at <dir>/.pi-stack/onboarding.state.
func TestWriteOnboardingMarker_WritesFullChecklist(t *testing.T) {
	dir := t.TempDir()
	writeOnboardingMarker(dir)

	markerPath := filepath.Join(dir, ".pi-stack", "onboarding.state")
	b, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("reading marker: %v", err)
	}

	var got struct {
		Active  bool            `json:"active"`
		Covered map[string]bool `json:"covered"`
		Turns   int             `json:"turns"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("marker is not valid JSON: %v (%q)", err, string(b))
	}
	if !got.Active {
		t.Error("freshly written marker must be active")
	}
	if got.Turns != 0 {
		t.Errorf("turns = %d, want 0", got.Turns)
	}
	want := []string{"memory", "skills", "crew", "packs", "knowledge"}
	if len(got.Covered) != len(want) {
		t.Fatalf("covered has %d keys, want %d (%v)", len(got.Covered), len(want), got.Covered)
	}
	for _, cap := range want {
		if v, ok := got.Covered[cap]; !ok || v {
			t.Errorf("covered[%q] = (present=%v, value=%v), want (true, false)", cap, ok, v)
		}
	}
}

// A symlinked .pi-stack dir must be REFUSED — writeOnboardingMarker must not
// follow it and clobber whatever it points at.
func TestWriteOnboardingMarker_SkipsSymlinkedPiStackDir(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	link := filepath.Join(dir, ".pi-stack")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	writeOnboardingMarker(dir)

	if _, err := os.Stat(filepath.Join(target, "onboarding.state")); err == nil {
		t.Error("writeOnboardingMarker must not write through a symlinked .pi-stack dir")
	}
}

// A symlinked onboarding.state PATH itself (dir is real, the marker file is a
// symlink to something else) must also be refused.
func TestWriteOnboardingMarker_SkipsSymlinkedMarkerFile(t *testing.T) {
	dir := t.TempDir()
	d := filepath.Join(dir, ".pi-stack")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatalf("mkdir .pi-stack: %v", err)
	}
	targetFile := filepath.Join(dir, "secret-host-file")
	if err := os.WriteFile(targetFile, []byte("do not touch\n"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	markerPath := filepath.Join(d, "onboarding.state")
	if err := os.Symlink(targetFile, markerPath); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	writeOnboardingMarker(dir)

	b, err := os.ReadFile(targetFile)
	if err != nil {
		t.Fatalf("reading target: %v", err)
	}
	if string(b) != "do not touch\n" {
		t.Errorf("symlinked marker file was overwritten: %q", string(b))
	}
}
