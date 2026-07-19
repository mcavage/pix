package main

import (
	"path/filepath"
	"testing"
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
