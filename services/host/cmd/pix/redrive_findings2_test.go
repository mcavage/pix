package main

// redrive_findings2_test.go — adversarial-review redrive findings 8/9/10/11/13:
//   8: setup/onboard never persist a shipped-catalog remote that is not
//      registered + auth-ready (allowlist derived from mcp.McpCatalogNames);
//   9: doctor's local MCP probe maps an explicit policy denial to
//      readiness.VerdictDenied, and gog registration uses the shared tri-state;
//  10: every primary readiness probe (doctor/status/run-preflight sbx calls)
//      is bounded — a hanging sbx can never wedge a verb;
//  11: doctor's launch.SbxAbsent means POSITIVELY absent, never a generic probe error;
//  13: `pix mcp load` validates NAME [DIR] strictly before deriving
//      anything.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"pix/host/config"
	"pix/host/hostenv"
	"pix/host/secret"
	"pix/host/sys"
	"pix/host/sys/systest"
	"pix/host/workflow/launch"
	"pix/host/workflow/provision"
)

// --- finding 8: catalog MCP readiness gate ---------------------------------

// catalogGateEnv builds a hostenv.Env for the gate tests: sbx on PATH, canned
// probe outputs, and a runInteractive that FAILS the test — the gate must
// never open an OAuth flow itself.
func catalogGateEnv(t *testing.T, output map[string]string) hostenv.Env {
	t.Helper()
	return hostenv.Env{System: &systest.Fake{LookPathFn: func(name string) (string, error) {
		if name == "sbx" {
			return "/usr/bin/sbx", nil
		}
		return "", fmt.Errorf("%q not found", name)
	}, RunFn: func(name string, args ...string) (string, error) {
		key := strings.Join(append([]string{name}, args...), " ")
		if out, ok := output[key]; ok {
			return out, nil
		}
		return "", fmt.Errorf("no fake output for %q", key)
	}, RunInteractiveFn: func(name string, args ...string) error {
		t.Fatalf("the catalog gate must NEVER launch an interactive command (OAuth): %s %v", name, args)
		return nil
	}}}
}

func TestVerifyCatalogMCPReady_Ready(t *testing.T) {
	env := catalogGateEnv(t, map[string]string{
		"sbx mcp ls":                 "notion\natlassian\n",
		"sbx mcp auth status notion": "notion: authorized\n",
	})
	if err := provision.VerifyCatalogMCPReady(env, []string{"notion"}); err != nil {
		t.Fatalf("registered+authorized catalog server must pass the gate: %v", err)
	}
}

func TestVerifyCatalogMCPReady_UnregisteredNamesAddAndAuth(t *testing.T) {
	env := catalogGateEnv(t, map[string]string{
		"sbx mcp ls": "atlassian\n", // notion positively missing
	})
	err := provision.VerifyCatalogMCPReady(env, []string{"notion"})
	if err == nil {
		t.Fatal("an unregistered catalog server must fail the gate")
	}
	if !strings.Contains(err.Error(), "pix mcp add notion") || !strings.Contains(err.Error(), "pix mcp auth notion") {
		t.Errorf("error must carry the exact repair commands, got: %v", err)
	}
}

func TestVerifyCatalogMCPReady_UnauthorizedNamesAuthCommand(t *testing.T) {
	env := catalogGateEnv(t, map[string]string{
		"sbx mcp ls":                 "notion\n",
		"sbx mcp auth status notion": "notion: not authenticated\n",
	})
	err := provision.VerifyCatalogMCPReady(env, []string{"notion"})
	if err == nil {
		t.Fatal("an unauthorized catalog server must fail the gate")
	}
	if !strings.Contains(err.Error(), "pix mcp auth notion") {
		t.Errorf("error must carry the exact auth command, got: %v", err)
	}
}

func TestVerifyCatalogMCPReady_ExplicitPolicyDenial(t *testing.T) {
	env := catalogGateEnv(t, map[string]string{
		"sbx mcp ls":                 "notion\n",
		"sbx mcp auth status notion": "access denied by org policy\n",
	})
	err := provision.VerifyCatalogMCPReady(env, []string{"notion"})
	if err == nil || !strings.Contains(err.Error(), "denied by policy") {
		t.Errorf("an explicit policy denial must fail as DENIED (not a setup todo), got: %v", err)
	}
	if strings.Contains(err.Error(), "pix mcp auth notion`") && strings.Contains(err.Error(), "then re-run") {
		t.Errorf("a policy denial must not pretend an auth command fixes it: %v", err)
	}
}

func TestVerifyCatalogMCPReady_ProbeFailureIsUnverifiableRetry(t *testing.T) {
	// Listing succeeds but the auth probe errors with nothing classifiable.
	env := catalogGateEnv(t, map[string]string{"sbx mcp ls": "notion\n"})
	err := provision.VerifyCatalogMCPReady(env, []string{"notion"})
	if err == nil || !strings.Contains(err.Error(), "could not verify") {
		t.Errorf("an unclassifiable probe failure must fail as unverifiable/retry, got: %v", err)
	}
	// Listing itself unavailable (sbx absent) — also unverifiable, fail closed.
	absent := catalogGateEnv(t, nil)
	systest.Of(absent.System).LookPathFn = func(string) (string, error) { return "", fmt.Errorf("not found") }
	if err := provision.VerifyCatalogMCPReady(absent, []string{"notion"}); err == nil || !strings.Contains(err.Error(), "could not verify") {
		t.Errorf("sbx-absent must fail closed as unverifiable, got: %v", err)
	}
}

func TestVerifyCatalogMCPReady_NonCatalogNamesNeverProbed(t *testing.T) {
	env := catalogGateEnv(t, nil) // any probe would error the run below
	systest.Of(env.System).RunFn = func(name string, args ...string) (string, error) {
		t.Fatalf("non-catalog names must never be probed by the gate: %s %v", name, args)
		return "", nil
	}
	if err := provision.VerifyCatalogMCPReady(env, []string{"gog", "slack", ""}); err != nil {
		t.Fatalf("gog/local/blank names are not the gate's business: %v", err)
	}
}
func TestReconcileOnboarding_CatalogGateLeavesFileAndConfig(t *testing.T) {
	ws := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("PIX_CONFIG", cfgPath)
	t.Setenv("PIX_PROFILE", "")
	dir := filepath.Join(ws, ".pix")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	fp := filepath.Join(dir, "onboarding.json")
	if err := os.WriteFile(fp, []byte(`{"version":1,"mcp":["notion"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	env := catalogGateEnv(t, map[string]string{"sbx mcp ls": "atlassian\n"})

	var out bytes.Buffer
	provision.ReconcileOnboarding(ws, env, strings.NewReader(""), &out, true, false)

	if _, err := os.Stat(fp); err != nil {
		t.Errorf("proposal file must be left in place on a gate failure, err=%v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(cfg.MCP, "notion") {
		t.Errorf("notion must NOT be persisted while unregistered: %v", cfg.MCP)
	}
	if !strings.Contains(out.String(), "pix mcp add notion") {
		t.Errorf("refusal must name the exact repair command, got:\n%s", out.String())
	}
}

// Finding 8's drift guard is now structural: onboarding reads
// mcp.McpCatalogNames directly, so the copy that could drift out of step with
// `pix mcp bundle` no longer exists to be tested. What a proposal does with a
// catalog-shaped name that is NOT shipped is covered where the validation
// lives (provision: TestValidateOnboarding_Allowlist rejects "linear").

// --- finding 9: local denied verdict + gog registration tri-state ----------

func hangingExe(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake executable; unix-only test")
	}
	p := filepath.Join(t.TempDir(), "hang")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// hangingProbe returns a probe seam that execs the hanging binary under a
// SHORT injectable deadline — the real runWithTimeoutD path, so this proves
// the context deadline actually kills a wedged child.
func hangingProbe(t *testing.T, deadline time.Duration) func(string, ...string) (string, bool, error) {
	exe := hangingExe(t)
	return func(name string, args ...string) (string, bool, error) {
		return sys.RunTimed(deadline, exe, args...)
	}
}

func TestRunWithTimeoutD_HangingProcessBounded(t *testing.T) {
	exe := hangingExe(t)
	start := time.Now()
	_, timedOut, _ := sys.RunTimed(100*time.Millisecond, exe)
	if !timedOut {
		t.Fatal("a hanging process must report timedOut under the injected deadline")
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("runWithTimeoutD took %s — the deadline did not bound the child", el)
	}
}

func TestProbeSbxSecrets_HangingSbxIsErrorNotAbsent(t *testing.T) {
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "/usr/bin/sbx", nil }, RunTimedFn: hangingProbe(t, 100*time.Millisecond)}}
	start := time.Now()
	_, state := secret.ProbeSbxSecrets(env)
	if state != secret.SbxSecretsError {
		t.Errorf("hanging `sbx secret ls` must classify secret.SbxSecretsError, got %v", state)
	}
	if el := time.Since(start); el > 10*time.Second {
		t.Fatalf("secret.ProbeSbxSecrets took %s — unbounded", el)
	}
}

// TestSbxModelKeyState_HangingProbeUnknownProceeds pins run's launch-preflight
// tri-state under a hang: probeOK=false (unknown), which under the existing
// rule PROCEEDS — only a POSITIVELY confirmed missing key blocks a launch.
func TestSbxModelKeyState_HangingProbeUnknownProceeds(t *testing.T) {
	env := hostenv.Env{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "/usr/bin/sbx", nil }, RunTimedFn: hangingProbe(t, 100*time.Millisecond)}}
	present, probeOK := launch.SbxModelKeyState(env)
	if present || probeOK {
		t.Errorf("hanging preflight must be (present=false, probeOK=false) so run proceeds, got (%v,%v)", present, probeOK)
	}
}

// --- finding 13: mcp load argument validation --------------------------------

// TestMcpLoadSandbox_IsRunsOwnDefaultName: `pix mcp load NAME [DIR]` must
// target the SAME box `pix run DIR` would, and it derives that name rather than
// looking it up — U04e deleted the receipt store the old resolver scanned, and
// with it the stale "pix-<basename>" fallback that named a sandbox nothing
// creates. One derivation shared by both commands is what makes them agree.
func TestMcpLoadSandbox_IsRunsOwnDefaultName(t *testing.T) {
	ws := t.TempDir()
	got := resolveSandboxName("", ws)
	if got != resolveSandboxName("", ws) {
		t.Error("the derivation must be stable for one workspace")
	}
	if other := resolveSandboxName("", t.TempDir()); other == got {
		t.Error("two different workspaces must not derive one sandbox name")
	}
	if !strings.HasPrefix(got, "pix-") {
		t.Errorf("derived sandbox %q must be in the pix-* scope pix rm owns", got)
	}
}
