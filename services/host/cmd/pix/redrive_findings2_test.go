package main

// redrive_findings2_test.go — adversarial-review redrive findings 8/9/10/11/13:
//   8: setup/onboard never persist a shipped-catalog remote that is not
//      registered + auth-ready (allowlist derived from mcpCatalogNames);
//   9: doctor's local MCP probe maps an explicit policy denial to
//      readiness.VerdictDenied, and gog registration uses the shared tri-state;
//  10: every primary readiness probe (doctor/status/run-preflight sbx calls)
//      is bounded — a hanging sbx can never wedge a verb;
//  11: doctor's sbxAbsent means POSITIVELY absent, never a generic probe error;
//  13: `pix mcp load` validates NAME [DIR] strictly before deriving
//      anything.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"pix/host/config"
	"pix/host/readiness"
	"pix/host/secret"
	"pix/host/sys"
	"pix/host/sys/systest"
	"pix/host/workspace"
)

// --- finding 8: catalog MCP readiness gate ---------------------------------

// catalogGateEnv builds a shellEnv for the gate tests: sbx on PATH, canned
// probe outputs, and a runInteractive that FAILS the test — the gate must
// never open an OAuth flow itself.
func catalogGateEnv(t *testing.T, output map[string]string) shellEnv {
	t.Helper()
	return shellEnv{System: &systest.Fake{LookPathFn: func(name string) (string, error) {
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
	if err := verifyCatalogMCPReady(env, []string{"notion"}); err != nil {
		t.Fatalf("registered+authorized catalog server must pass the gate: %v", err)
	}
}

func TestVerifyCatalogMCPReady_UnregisteredNamesBundleAndAuth(t *testing.T) {
	env := catalogGateEnv(t, map[string]string{
		"sbx mcp ls": "atlassian\n", // notion positively missing
	})
	err := verifyCatalogMCPReady(env, []string{"notion"})
	if err == nil {
		t.Fatal("an unregistered catalog server must fail the gate")
	}
	if !strings.Contains(err.Error(), "pix mcp bundle") || !strings.Contains(err.Error(), "pix mcp auth notion") {
		t.Errorf("error must carry the exact repair commands, got: %v", err)
	}
}

func TestVerifyCatalogMCPReady_UnauthorizedNamesAuthCommand(t *testing.T) {
	env := catalogGateEnv(t, map[string]string{
		"sbx mcp ls":                 "notion\n",
		"sbx mcp auth status notion": "notion: not authenticated\n",
	})
	err := verifyCatalogMCPReady(env, []string{"notion"})
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
	err := verifyCatalogMCPReady(env, []string{"notion"})
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
	err := verifyCatalogMCPReady(env, []string{"notion"})
	if err == nil || !strings.Contains(err.Error(), "could not verify") {
		t.Errorf("an unclassifiable probe failure must fail as unverifiable/retry, got: %v", err)
	}
	// Listing itself unavailable (sbx absent) — also unverifiable, fail closed.
	absent := catalogGateEnv(t, nil)
	systest.Of(absent.System).LookPathFn = func(string) (string, error) { return "", fmt.Errorf("not found") }
	if err := verifyCatalogMCPReady(absent, []string{"notion"}); err == nil || !strings.Contains(err.Error(), "could not verify") {
		t.Errorf("sbx-absent must fail closed as unverifiable, got: %v", err)
	}
}

func TestVerifyCatalogMCPReady_NonCatalogNamesNeverProbed(t *testing.T) {
	env := catalogGateEnv(t, nil) // any probe would error the run below
	systest.Of(env.System).RunFn = func(name string, args ...string) (string, error) {
		t.Fatalf("non-catalog names must never be probed by the gate: %s %v", name, args)
		return "", nil
	}
	if err := verifyCatalogMCPReady(env, []string{"gog", "slack", ""}); err != nil {
		t.Fatalf("gog/local/blank names are not the gate's business: %v", err)
	}
}

// TestSetupHostPhase_CatalogGate_NoSaveNoKeysOnGap proves finding 8's core
// invariant end to end: `pix setup --mcp notion` with notion unregistered
// fails BEFORE the provider-key flow runs and BEFORE anything is saved.
func TestSetupHostPhase_CatalogGate_NoSaveNoKeysOnGap(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	t.Setenv("PIX_CONFIG", cfgPath)

	orig := setupProvisionKeysFn
	t.Cleanup(func() { setupProvisionKeysFn = orig })
	invoked := false
	setupProvisionKeysFn = func(env shellEnv, in io.Reader, out io.Writer, interactive, assumeYes bool) bool {
		invoked = true
		return true
	}

	env := catalogGateEnv(t, map[string]string{
		"sbx mcp ls": "atlassian\n", // notion positively missing
	})
	var out bytes.Buffer
	err := setupHostPhase(env, []string{"--yes", "--mcp", "notion"}, strings.NewReader(""), &out, false)
	if err == nil {
		t.Fatal("setup must fail when a proposed catalog server is not registered")
	}
	if !strings.Contains(err.Error(), "pix mcp bundle") {
		t.Errorf("setup's failure must name the exact registration command, got: %v", err)
	}
	if invoked {
		t.Error("setupProvisionKeysFn must never run for a proposal the catalog gate rejects")
	}
	if _, statErr := os.Stat(cfgPath); !os.IsNotExist(statErr) {
		t.Errorf("config.toml must NOT be written on a catalog gate failure (stat err=%v)", statErr)
	}
}

// TestReconcileOnboarding_CatalogGateLeavesFileAndConfig: `onboard --apply`
// with an unready catalog remote applies NOTHING and leaves the proposal file
// in place for inspection.
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
	reconcileOnboarding(ws, env, strings.NewReader(""), &out, true, false)

	if _, err := os.Stat(fp); err != nil {
		t.Errorf("proposal file must be left in place on a gate failure, err=%v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if containsStr(cfg.MCP, "notion") {
		t.Errorf("notion must NOT be persisted while unregistered: %v", cfg.MCP)
	}
	if !strings.Contains(out.String(), "pix mcp bundle") {
		t.Errorf("refusal must name the exact repair command, got:\n%s", out.String())
	}
}

// TestOnboardCatalogAllowlist_IsTheShippedCatalog: the accepted catalog names
// derive from mcpCatalogNames — no independent list that can drift.
func TestOnboardCatalogAllowlist_IsTheShippedCatalog(t *testing.T) {
	if len(onboardMCPCatalogAllow) != len(mcpCatalogNames) {
		t.Fatalf("allowlist (%v) must equal mcpCatalogNames (%v)", onboardMCPCatalogAllow, mcpCatalogNames)
	}
	for n := range mcpCatalogNames {
		if !onboardMCPCatalogAllow[n] {
			t.Errorf("shipped catalog name %q missing from the onboarding allowlist", n)
		}
	}
	if onboardMCPCatalogAllow["linear"] {
		t.Error("\"linear\" is not a shipped catalog server and must not be accepted (the drift finding 8 removes)")
	}
}

// --- finding 9: local denied verdict + gog registration tri-state ----------

func TestMcpLocalCheck_PolicyDeniedVerdict(t *testing.T) {
	const hostBin = "/usr/local/bin/pix-host"
	env := shellEnv{System: &systest.Fake{LookPathFn: func(name string) (string, error) {
		if name == "sbx" {
			return "/usr/bin/sbx", nil
		}
		return "", fmt.Errorf("%q not found", name)
	}, RunTimedFn: func(name string, args ...string) (string, bool, error) {
		switch strings.Join(append([]string{name}, args...), " ") {
		case "sbx mcp get slack":
			return "name: slack\ncommand: " + hostBin + " mcp slack\n", false, nil
		case hostBin + " mcp slack --list-tools":
			return "403 forbidden: access denied by org policy", false, errors.New("exit status 1")
		}
		return "", false, fmt.Errorf("no fake probe")
	}}, HostBinary: func() (string, error) { return hostBin, nil }}
	c := mcpLocalCheck(env, "slack", "slack\n")
	if c.Result() != readiness.VerdictDenied {
		t.Errorf("an explicit policy denial from the local probe must be readiness.VerdictDenied, got %+v", c)
	}
}

func TestGogRegistrationCheck_TriState(t *testing.T) {
	// Present in a successful listing -> ready.
	if c := gogRegistrationCheck("google-workspace\nslack\n", true, true); c.Result() != readiness.VerdictReady {
		t.Errorf("registered gog = %+v, want ready", c)
	}
	// Positively missing from a successful listing -> verified register TODO.
	c := gogRegistrationCheck("notion\n", true, true)
	if c.Result() != readiness.VerdictTodo || c.Todo != "pix mcp register" {
		t.Errorf("unregistered gog = %+v, want the register todo", c)
	}
	// Listing failed with sbx PRESENT -> unverifiable (daemon guidance), and
	// NEVER a false outstanding item.
	c = gogRegistrationCheck("", false, true)
	if c.Result() != readiness.VerdictUnverifiable || c.Todo != "" {
		t.Errorf("gog with failed listing (sbx present) = %+v, want unverifiable with no todo", c)
	}
	if !strings.Contains(c.Detail, "sbx daemon") {
		t.Errorf("sbx-present degrade should point at the daemon, got %q", c.Detail)
	}
	// sbx absent entirely -> unverifiable in-sandbox degrade, no todo.
	c = gogRegistrationCheck("", false, false)
	if c.Result() != readiness.VerdictUnverifiable || c.Todo != "" {
		t.Errorf("gog with sbx absent = %+v, want unverifiable with no todo", c)
	}
	if !strings.Contains(c.Detail, "sbx unavailable") {
		t.Errorf("sbx-absent degrade should say sbx unavailable, got %q", c.Detail)
	}
}

// --- finding 10: bounded probes (hanging fake executable) ------------------

// hangingExe writes an executable that sleeps far longer than any test
// deadline, standing in for a wedged sbx.
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
	env := shellEnv{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "/usr/bin/sbx", nil }, RunTimedFn: hangingProbe(t, 100*time.Millisecond)}}
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
	env := shellEnv{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "/usr/bin/sbx", nil }, RunTimedFn: hangingProbe(t, 100*time.Millisecond)}}
	present, probeOK := sbxModelKeyState(env)
	if present || probeOK {
		t.Errorf("hanging preflight must be (present=false, probeOK=false) so run proceeds, got (%v,%v)", present, probeOK)
	}
}

func TestGatherStatus_HangingSbxBounded(t *testing.T) {
	cfg := &config.Config{MCP: []string{"notion"}}
	sd := t.TempDir()
	env := shellEnv{System: &systest.Fake{LookPathFn: func(name string) (string, error) {
		if name == "sbx" {
			return "/usr/bin/sbx", nil
		}
		return "", fmt.Errorf("%q not found", name)
	}, RunTimedFn: hangingProbe(t, 100*time.Millisecond), DialLocalFn: func(int) bool { return false }, IsFileFn: func(string) bool { return false }, StateDirFn: func() (string, error) { return sd, nil }}}
	start := time.Now()
	st := gatherStatus(cfg, "default", env)
	if el := time.Since(start); el > 30*time.Second {
		t.Fatalf("gatherStatus took %s with a hanging sbx — unbounded", el)
	}
	found := false
	for _, tdo := range st.Todos {
		if strings.Contains(tdo, "could not verify provider keys") {
			found = true
		}
	}
	if !found {
		t.Errorf("hanging secret ls must degrade to an honest could-not-verify todo, got %v", st.Todos)
	}
	if len(st.MCPRows) != 1 || st.MCPRows[0].State != mcpJoinUnverifiable {
		t.Errorf("hanging sbx ls must yield an unverifiable row, never a false claim: %+v", st.MCPRows)
	}
	if st.MCPRows[0].Registered != "unknown" {
		t.Errorf("hanging mcp ls must leave registration unknown, got %q", st.MCPRows[0].Registered)
	}
}

func TestRunDoctor_HangingMcpLsUnverifiable(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"slack"}
	hang := hangingProbe(t, 100*time.Millisecond)
	env := shellEnv{System: &systest.Fake{LookPathFn: func(name string) (string, error) {
		if name == "sbx" {
			return "/usr/bin/sbx", nil
		}
		return "", fmt.Errorf("%q not found", name)
	}, RunTimedFn: func(name string, args ...string) (string, bool, error) {
		if name == "sbx" && len(args) == 2 && args[0] == "secret" && args[1] == "ls" {
			return "anthropic\nopenai\ngoogle\n", false, nil
		}
		return hang(name, args...)
	}, DialLocalFn: func(int) bool { return false }, IsFileFn: func(string) bool { return false }, GetenvFn: func(string) string { return "" }, HomeDirFn: func() string { return "" }}}
	start := time.Now()
	r := runDoctor(cfg, env)
	if el := time.Since(start); el > 30*time.Second {
		t.Fatalf("runDoctor took %s with a hanging `sbx mcp ls` — unbounded", el)
	}
	if r.SbxAbsent {
		t.Error("sbx is on PATH — a hanging mcp ls must not read as sbx absent")
	}
	c := findCheck(t, r.Groups[len(r.Groups)-1], "slack")
	if c.Result() != readiness.VerdictUnverifiable {
		t.Errorf("hanging mcp ls must render the server unverifiable, got %+v", c)
	}
}

// --- finding 11: sbxAbsent means POSITIVELY absent --------------------------

func TestDoctor_SecretLsFailure_IsNotSbxAbsent(t *testing.T) {
	cfg := defaultCfg()
	f := fakeEnv{
		present: map[string]bool{"sbx": true},
		output:  map[string]string{}, // `sbx secret ls` errors (no canned output)
	}
	r := runDoctor(cfg, f.env())
	if r.SbxAbsent {
		t.Fatal("sbx IS on PATH — a failing `sbx secret ls` must not set sbxAbsent")
	}
	// Human rendering: the in-sandbox note must NOT appear.
	var buf bytes.Buffer
	r.Render(&buf, false, doctorHints())
	if strings.Contains(buf.String(), "sbx not on PATH") {
		t.Errorf("human output must not claim sbx is off PATH:\n%s", buf.String())
	}
	// JSON: sbx_absent false.
	if jsonView(r, "").SbxAbsent {
		t.Error("JSON sbx_absent must be false when sbx is present but the probe failed")
	}

	// Converse: sbx genuinely off PATH -> sbxAbsent true, note rendered.
	absent := runDoctor(cfg, fakeEnv{present: map[string]bool{}}.env())
	if !absent.SbxAbsent || !jsonView(absent, "").SbxAbsent {
		t.Error("sbx off PATH must set sbxAbsent (human + JSON)")
	}
	buf.Reset()
	absent.Render(&buf, false, doctorHints())
	if !strings.Contains(buf.String(), "sbx not on PATH") {
		t.Errorf("sbx off PATH should render the in-sandbox note:\n%s", buf.String())
	}
}

// --- finding 13: mcp load argument validation --------------------------------

func TestParseMcpLoadArgs(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	ok := []struct {
		argv     []string
		name, ws string
	}{
		{[]string{"slack"}, "slack", "."},
		{[]string{"slack", dir}, "slack", dir},
	}
	for _, tc := range ok {
		name, ws, err := parseMcpLoadArgs(tc.argv)
		if err != nil || name != tc.name || ws != tc.ws {
			t.Errorf("parseMcpLoadArgs(%v) = (%q,%q,%v), want (%q,%q,nil)", tc.argv, name, ws, err, tc.name, tc.ws)
		}
	}

	bad := map[string][]string{
		"no args":            {},
		"blank name":         {"   "},
		"empty name":         {""},
		"flag as name":       {"--sandbox"},
		"flag as dir":        {"slack", "--dir"},
		"extra arg":          {"slack", dir, "extra"},
		"nonexistent dir":    {"slack", filepath.Join(dir, "missing")},
		"file not directory": {"slack", file},
	}
	for label, argv := range bad {
		if _, _, err := parseMcpLoadArgs(argv); err == nil {
			t.Errorf("%s: parseMcpLoadArgs(%v) must fail", label, argv)
		}
	}
}

// TestParseMcpLoadArgs_FailureWritesNoReceipt: a usage failure returns before
// any sandbox name is derived, so nothing downstream (exec, receipt) can run.
// The receipt path is only reachable via execSbxMcpLoadAndRecord, which the
// wiring tests already gate on a successful exec; here we prove the parse
// layer rejects without touching launcher state at all.
func TestParseMcpLoadArgs_FailureWritesNoReceipt(t *testing.T) {
	sd := t.TempDir()
	orig := workspace.MCPStateDirFn
	workspace.MCPStateDirFn = func() (string, error) { return sd, nil }
	t.Cleanup(func() { workspace.MCPStateDirFn = orig })

	if _, _, err := parseMcpLoadArgs([]string{"slack", filepath.Join(sd, "nope")}); err == nil {
		t.Fatal("expected a usage failure")
	}
	entries, err := os.ReadDir(sd)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("usage failure must write nothing to launcher state, found %v", entries)
	}
}
