package uatenvmatrix

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestIsolatedExecEnvRehomesEveryPixRoot mirrors uatmatrix's
// TestIsolatedEnvRehomesEveryPixRoot: every XDG root and PIX_CONFIG must
// point inside phaseDir. HOME is deliberately excluded from this table: see
// TestIsolatedExecEnvPreservesHostHomeForSbxAuth for why it stays real.
func TestIsolatedExecEnvRehomesEveryPixRoot(t *testing.T) {
	phaseDir := t.TempDir()
	t.Setenv("HOME", "/normal-home-must-not-leak")
	env := isolatedExecEnv(phaseDir)
	want := map[string]string{
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

// TestIsolatedExecEnvPreservesHostHomeForSbxAuth is the regression test for
// the proven bootstrap bug (run run-20260823-155824-4d96352e,
// environment_create_then_exec_invocation): rehoming HOME under phaseDir hid
// the host's sbx/Docker Desktop authentication, so every check that shells
// out to `sbx` failed with "Not authenticated to Docker; Sign in with: sbx
// login" even though the operator was logged in. sbx and Docker Desktop
// discover their runtime/auth state beneath the real HOME (macOS keeps
// Docker Desktop's socket and `sbx login` its session there), exactly like
// the candidate_smoke override in workflow/uat/execute.go. HOME must pass
// through unmodified while every Pix XDG root and PIX_CONFIG still isolate
// under phaseDir.
func TestIsolatedExecEnvPreservesHostHomeForSbxAuth(t *testing.T) {
	realHome := t.TempDir()
	t.Setenv("HOME", realHome)
	phaseDir := t.TempDir()
	env := isolatedExecEnv(phaseDir)
	want := "HOME=" + realHome
	found := false
	for _, got := range env {
		if strings.HasPrefix(got, "HOME=") {
			if got != want {
				t.Fatalf("isolatedExecEnv rehomed HOME to %q; want the real host HOME %q so sbx/Docker auth stays discoverable", got, want)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("isolatedExecEnv did not set HOME at all")
	}
}

// TestIsolatedExecEnvPreservesDockerAuthVars proves DOCKER_HOST and
// DOCKER_CONFIG pass through when the host process has them set, matching
// the candidate_smoke override (workflow/uat/execute.go) that this matrix
// must not regress behind.
func TestIsolatedExecEnvPreservesDockerAuthVars(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dockerHost := "unix:///tmp/fixture-docker.sock"
	dockerConfig := t.TempDir()
	t.Setenv("DOCKER_HOST", dockerHost)
	t.Setenv("DOCKER_CONFIG", dockerConfig)
	env := isolatedExecEnv(t.TempDir())
	wantHost := "DOCKER_HOST=" + dockerHost
	wantConfig := "DOCKER_CONFIG=" + dockerConfig
	hasHost, hasConfig := false, false
	for _, got := range env {
		if got == wantHost {
			hasHost = true
		}
		if got == wantConfig {
			hasConfig = true
		}
	}
	if !hasHost {
		t.Errorf("missing %q in %#v", wantHost, env)
	}
	if !hasConfig {
		t.Errorf("missing %q in %#v", wantConfig, env)
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

// TestIsolatedExecEnvNeverPointsAtDefaultPixRoots proves the rehomed Pix
// roots (XDG_*, PIX_CONFIG) never resolve to the process's own default
// config/state locations, even when HOME happens to already contain a real
// ~/.config/pix. HOME itself is expected to stay real (it is the sbx/Docker
// auth discovery seam, not a Pix root), so it is excluded from the
// default-pix-root check but still constrained to the allowed-passthrough
// set below.
func TestIsolatedExecEnvNeverPointsAtDefaultPixRoots(t *testing.T) {
	realHome := t.TempDir()
	t.Setenv("HOME", realHome)
	phaseDir := filepath.Join(t.TempDir(), "phase")
	env := isolatedExecEnv(phaseDir)
	defaultConfig := filepath.Join(realHome, ".config", "pix")
	for _, e := range env {
		if strings.HasPrefix(e, "HOME=") {
			continue
		}
		if strings.Contains(e, defaultConfig) {
			t.Errorf("isolatedExecEnv referenced the default pix config root: %q", e)
		}
		if !strings.HasPrefix(e, "PATH=") && !strings.HasPrefix(e, "TMPDIR=") &&
			!strings.HasPrefix(e, "TMP=") && !strings.HasPrefix(e, "TEMP=") &&
			!strings.HasPrefix(e, "LANG=") && !strings.HasPrefix(e, "LC_ALL=") &&
			!strings.HasPrefix(e, "DOCKER_HOST=") && !strings.HasPrefix(e, "DOCKER_CONFIG=") {
			if !strings.HasPrefix(e, "XDG_") && !strings.HasPrefix(e, "PIX_CONFIG=") {
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
