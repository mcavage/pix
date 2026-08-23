package uatenvmatrix

import (
	"strings"
	"testing"
)

// TestHostToolExecEnvPreservesHostHomeForSbxAuth is the original regression
// test for the proven bootstrap bug (run run-20260823-155824-4d96352e,
// environment_create_then_exec_invocation): rehoming HOME under phaseDir hid
// the host's sbx/Docker Desktop authentication, so every check that shells
// out to `sbx` failed with "Not authenticated to Docker; Sign in with: sbx
// login" even though the operator was logged in.
func TestHostToolExecEnvPreservesHostHomeForSbxAuth(t *testing.T) {
	realHome := t.TempDir()
	t.Setenv("HOME", realHome)
	env := hostToolExecEnv()
	want := "HOME=" + realHome
	found := false
	for _, got := range env {
		if strings.HasPrefix(got, "HOME=") {
			if got != want {
				t.Fatalf("hostToolExecEnv rehomed HOME to %q; want the real host HOME %q so sbx/Docker auth stays discoverable", got, want)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("hostToolExecEnv did not set HOME at all")
	}
}

// TestHostToolExecEnvPreservesDockerAuthVars proves DOCKER_HOST and
// DOCKER_CONFIG pass through when the host process has them set, matching
// the candidate_smoke override (workflow/uat/execute.go) that this matrix
// must not regress behind.
func TestHostToolExecEnvPreservesDockerAuthVars(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dockerHost := "unix:///tmp/fixture-docker.sock"
	dockerConfig := t.TempDir()
	t.Setenv("DOCKER_HOST", dockerHost)
	t.Setenv("DOCKER_CONFIG", dockerConfig)
	env := hostToolExecEnv()
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

// TestHostToolExecEnvPassesThroughUnknownHostChannel is the regression test
// for the escalated bootstrap bug (run run-20260823-160820-41a5b981,
// environment_create_then_exec_invocation): "Not authenticated to Docker"
// persisted even after HOME/DOCKER_HOST/DOCKER_CONFIG were preserved,
// because uatenvmatrix's checks invoke ONLY host tools (`sbx`, `docker`) and
// never pix/pix-host, so there is no legitimate reason to run them under a
// curated allowlist at all — sbx's own auth/session state can live behind
// any host channel (an XDG root, a gateway/session variable, or something
// this package's author never anticipated). This test proves an arbitrary,
// otherwise-unrecognized variable the daemon happens to have set passes
// through unchanged, so a future unknown auth channel is not silently
// dropped the way the allowlist dropped this one.
func TestHostToolExecEnvPassesThroughUnknownHostChannel(t *testing.T) {
	t.Setenv("SBX_FIXTURE_UNKNOWN_HOST_CHANNEL", "some-session-token-value")
	env := hostToolExecEnv()
	want := "SBX_FIXTURE_UNKNOWN_HOST_CHANNEL=some-session-token-value"
	for _, got := range env {
		if got == want {
			return
		}
	}
	t.Fatalf("hostToolExecEnv dropped an unrecognized host variable; want %q present, got %#v", want, env)
}

// TestHostToolExecEnvPreservesXDGRootsUnchanged is the direct regression
// test for the escalated bug's actual root cause: the old isolatedExecEnv
// rehomed XDG_CONFIG_HOME (and its siblings) under a run-local phaseDir
// believing that protected "normal Pix config" from these checks. It does
// not — these checks never invoke pix/pix-host, so nothing here ever reads a
// Pix-rooted XDG path — and sbx itself may keep its own CLI config/session
// beneath the operator's real XDG roots, so rehoming them broke sbx's own
// auth discovery. XDG_* must now pass through exactly as the host set it.
func TestHostToolExecEnvPreservesXDGRootsUnchanged(t *testing.T) {
	hostXDGConfig := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", hostXDGConfig)
	env := hostToolExecEnv()
	want := "XDG_CONFIG_HOME=" + hostXDGConfig
	for _, got := range env {
		if strings.HasPrefix(got, "XDG_CONFIG_HOME=") {
			if got != want {
				t.Fatalf("hostToolExecEnv rewrote XDG_CONFIG_HOME to %q; want the host's own %q unchanged", got, want)
			}
			return
		}
	}
	t.Fatalf("hostToolExecEnv dropped XDG_CONFIG_HOME entirely; want %q", want)
}

// TestHostToolExecEnvStripsPixVariables proves the one thing this function
// still isolates: Pix's own runtime variables. sbx/docker never consult a
// PIX_ variable, and the daemon process (pix-host serve) may itself be
// running with one set (PIX_CONFIG, PIX_UAT_SMOKE, ...), so stripping the
// PIX_ prefix is what actually keeps the promise that a check can never leak
// normal Pix config into a host-tool subprocess — not XDG rehoming, which
// broke sbx's own auth discovery instead without protecting anything real
// (uatenvmatrix never runs pix/pix-host, unlike uatmatrix's isolatedEnv).
func TestHostToolExecEnvStripsPixVariables(t *testing.T) {
	t.Setenv("PIX_CONFIG", "/host/real/.config/pix/config.toml")
	t.Setenv("PIX_UAT_SMOKE", "1")
	env := hostToolExecEnv()
	for _, got := range env {
		if strings.HasPrefix(got, "PIX_") {
			t.Errorf("hostToolExecEnv leaked a Pix-specific variable to a host-tool subprocess: %q", got)
		}
	}
}
