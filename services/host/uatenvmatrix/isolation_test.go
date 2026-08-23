package uatenvmatrix

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestIsolatedExecEnvRehomesEveryPixRoot mirrors uatmatrix's
// TestIsolatedEnvRehomesEveryPixRoot: every XDG root and PIX_CONFIG must
// point inside phaseDir, and the real HOME set on this test process must
// never leak through.
func TestIsolatedExecEnvRehomesEveryPixRoot(t *testing.T) {
	phaseDir := t.TempDir()
	t.Setenv("HOME", "/normal-home-must-not-leak")
	env := isolatedExecEnv(phaseDir)
	want := map[string]string{
		"HOME":            filepath.Join(phaseDir, "home"),
		"XDG_CONFIG_HOME": filepath.Join(phaseDir, "config"),
		"XDG_DATA_HOME":   filepath.Join(phaseDir, "data"),
		"XDG_STATE_HOME":  filepath.Join(phaseDir, "state"),
		"XDG_CACHE_HOME":  filepath.Join(phaseDir, "cache"),
		"PIX_CONFIG":      filepath.Join(phaseDir, "config", "config.toml"),
	}
	for key, value := range want {
		entry := key + "=" + value
		found := false
		for _, got := range env {
			if got == entry {
				found = true
			}
			if strings.HasPrefix(got, key+"=") && got != entry {
				t.Errorf("%s escaped phase dir: %q", key, got)
			}
		}
		if !found {
			t.Errorf("missing %q", entry)
		}
	}
}

// TestIsolatedExecEnvNeverSetsMemoryPort proves this matrix never touches the
// memory daemon's world: no check here has any legitimate reason to set
// MEMORY_PORT/MEMORY_BIND/MEMORY_DB, unlike uatmatrix, which is the one
// place that does. A future check adding one of these by mistake fails this
// test, not silently drifting onto the real memory daemon's port.
func TestIsolatedExecEnvNeverSetsMemoryPort(t *testing.T) {
	env := isolatedExecEnv(t.TempDir())
	for _, e := range env {
		for _, forbidden := range []string{"MEMORY_PORT=", "MEMORY_BIND=", "MEMORY_DB=", "OLLAMA_HOST="} {
			if strings.HasPrefix(e, forbidden) {
				t.Errorf("isolatedExecEnv set %q; this matrix must never reference the memory daemon's world", e)
			}
		}
	}
}

// TestIsolatedExecEnvNeverPointsAtDefaultPixRoots proves the rehomed roots
// never resolve to the process's own default config/state locations, even
// when HOME happens to already contain a real ~/.config/pix.
func TestIsolatedExecEnvNeverPointsAtDefaultPixRoots(t *testing.T) {
	realHome := t.TempDir()
	t.Setenv("HOME", realHome)
	phaseDir := filepath.Join(t.TempDir(), "phase")
	env := isolatedExecEnv(phaseDir)
	defaultConfig := filepath.Join(realHome, ".config", "pix")
	for _, e := range env {
		if strings.Contains(e, defaultConfig) {
			t.Errorf("isolatedExecEnv referenced the default pix config root: %q", e)
		}
		if !strings.HasPrefix(e, "PATH=") && !strings.HasPrefix(e, "TMPDIR=") &&
			!strings.HasPrefix(e, "TMP=") && !strings.HasPrefix(e, "TEMP=") &&
			!strings.HasPrefix(e, "LANG=") && !strings.HasPrefix(e, "LC_ALL=") {
			if !strings.HasPrefix(e, "HOME="+phaseDir) && !strings.HasPrefix(e, "XDG_") && !strings.HasPrefix(e, "PIX_CONFIG=") {
				t.Errorf("unexpected passthrough env entry: %q", e)
			}
		}
	}
}

// TestBuildExecArgvIsNameBasedExecWithExactInvocation is a pure unit test of
// the exact-invocation builder the first named check relies on: given the
// package's own fixture, the resulting argv must be name-based (no image, no
// re-derivation from the environment path) and carry every typed fact in
// order.
func TestBuildExecArgvIsNameBasedExecWithExactInvocation(t *testing.T) {
	f := customAgentFixture()
	got := buildExecArgv(f)
	want := []string{
		"exec", "-it", "pix-uatenv-fixture-0", "--", "pi",
		"--kit", "/opt/pix/kit",
		"--skill", "/opt/pix/kit/skills",
		"--skill", "/home/uat/personal-context/skills",
		"--model", "anthropic/claude-sonnet-5",
		"--resume", "session-fixture-1",
	}
	if len(got) != len(want) {
		t.Fatalf("buildExecArgv = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("buildExecArgv[%d] = %q, want %q (full: %#v)", i, got[i], want[i], got)
		}
	}
}
