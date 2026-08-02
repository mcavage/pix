package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/monitor"
	"pix/host/sys/systest"
)

// fakeStatusEnv builds a shellEnv where memory is up, knowledge down, sbx lists
// two boxes and reports two secrets set.
func fakeStatusEnv() shellEnv {
	return shellEnv{System: &systest.Fake{LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil }, RunFn: func(name string, args ...string) (string, error) {
		if name == "sbx" && len(args) >= 1 && args[0] == "secret" {
			return "anthropic\nopenai\n", nil
		}
		if name == "sbx" && len(args) >= 1 && args[0] == "ls" {
			return "NAME STATUS\npix-myrepo running\npix-scratch stopped\nother-box running\n", nil
		}
		if name == "sbx" && len(args) >= 2 && args[0] == "mcp" && args[1] == "ls" {
			return "google-workspace\nnotion\n", nil
		}
		return "", nil
	}, DialLocalFn: func(port int) bool { return port == memoryPortDefault }, IsFileFn: func(string) bool { return false }}, IdentityProbe: identityFake(map[int]serviceIdentityResult{
		memoryPortDefault: {Name: identityMemoryName, Ready: true},
	})}
}

func TestGatherStatus(t *testing.T) {
	cfg := &config.Config{MCP: []string{gwServerName}, KnowledgeBundles: []string{"/kb"}}
	st := gatherStatus(cfg, "default", fakeStatusEnv())

	if !st.Memory {
		t.Error("memory should be up")
	}
	if st.Knowledge {
		t.Error("knowledge should be down")
	}
	if !st.Providers["anthropic"] || !st.Providers["openai"] {
		t.Errorf("providers = %v, want anthropic+openai set", st.Providers)
	}
	if st.Providers["google"] || st.Providers["github"] {
		t.Errorf("providers = %v, want google+github unset", st.Providers)
	}
	// anthropic alone already satisfies the core model-readiness check (finding
	// #3): a missing alternate (google) is never itself outstanding, and github
	// is optional informational, never outstanding either -> zero todos.
	if len(st.Todos) != 0 {
		t.Errorf("todos = %v, want 0 (one core provider present, github is informational)", st.Todos)
	}
	// Only pix-* sandboxes, "other-box" filtered out.
	if len(st.Sandboxes) != 2 {
		t.Errorf("sandboxes = %v, want 2 pix boxes", st.Sandboxes)
	}
	for _, s := range st.Sandboxes {
		if !strings.HasPrefix(s.Name, "pix-") {
			t.Errorf("leaked non-pix sandbox: %s", s.Name)
		}
	}
}

func TestRenderStatusHuman(t *testing.T) {
	cfg := &config.Config{MCP: []string{gwServerName}, KnowledgeBundles: []string{"/kb"}}
	var out bytes.Buffer
	renderStatus(cfg, "default", fakeStatusEnv(), &out, false)
	s := out.String()
	// anthropic+openai present already satisfies core model readiness (finding
	// #3), so this fixture has nothing OUTSTANDING — but its sandboxes' MCP
	// rows are unverifiable (no receipt state dir in the fake env), and an
	// unverifiable row must prevent the "all systems go" headline without
	// becoming a false TODO (redrive finding 5).
	if strings.Contains(s, "all systems go") {
		t.Errorf("unverifiable mcp rows must prevent the all-systems-go headline:\n%s", s)
	}
	for _, want := range []string{"pix", "knowledge", "1 bundle", "integrations", "nothing outstanding, but", "unverifiable (not failed"} {
		if !strings.Contains(s, want) {
			t.Errorf("status output missing %q:\n%s", want, s)
		}
	}
}

// TestGatherStatusMonitor (DX-5): the monitor hub's up/down state is probed
// via env.DialLocal(monitor.DefaultPort), independent of memory/knowledge.
func TestGatherStatusMonitor(t *testing.T) {
	cfg := &config.Config{}
	env := fakeStatusEnv()
	fakeOf(env).DialLocalFn = func(port int) bool { return port == monitor.DefaultPort }
	st := gatherStatus(cfg, "default", env)
	if !st.Monitor {
		t.Error("monitor should be up when its port dials")
	}
	if st.Memory {
		t.Error("memory should be down (dial only matches the monitor port here)")
	}
}

// TestRenderStatusMonitorLine (DX-5): an active on-demand monitor is visible;
// an inactive optional monitor is omitted rather than painted as a failure.
func TestRenderStatusMonitorLine(t *testing.T) {
	cfg := &config.Config{}
	env := fakeStatusEnv()
	fakeOf(env).DialLocalFn = func(port int) bool { return port == monitor.DefaultPort }
	var out bytes.Buffer
	renderStatus(cfg, "default", env, &out, false)
	s := out.String()
	if !strings.Contains(s, fmt.Sprintf("monitor      active · :%d", monitor.DefaultPort)) {
		t.Errorf("status output missing the monitor line:\n%s", s)
	}
}

// TestRenderStatusMonitorJSON (DX-5): --json carries monitor_up.
func TestRenderStatusMonitorJSON(t *testing.T) {
	cfg := &config.Config{}
	env := fakeStatusEnv()
	fakeOf(env).DialLocalFn = func(port int) bool { return port == monitor.DefaultPort }
	var out bytes.Buffer
	renderStatus(cfg, "default", env, &out, true)
	var st statusReport
	if err := json.Unmarshal(out.Bytes(), &st); err != nil {
		t.Fatalf("status --json invalid: %v\n%s", err, out.String())
	}
	if !st.Monitor {
		t.Error("json monitor_up = false, want true")
	}
}

// TestGatherStatusMCP: cfg.MCP servers get a registration state parsed from
// `sbx mcp ls` (host-global summary: registration + preload intent only);
// gog is registered, slack is not.
func TestGatherStatusMCP(t *testing.T) {
	cfg := &config.Config{MCP: []string{gwServerName, "slack"}}
	st := gatherStatus(cfg, "default", fakeStatusEnv())
	if len(st.MCPServers) != 2 {
		t.Fatalf("MCPServers = %+v, want 2 entries", st.MCPServers)
	}
	byName := map[string]mcpStatusLine{}
	for _, m := range st.MCPServers {
		byName[m.Name] = m
	}
	if !byName[gwServerName].Registered {
		t.Errorf("gog should be registered: %+v", byName[gwServerName])
	}
	if byName["slack"].Registered {
		t.Errorf("slack should NOT be registered: %+v", byName["slack"])
	}
}

// TestGatherStatusMCPSbxAbsent: with sbx off PATH, MCPServers is empty (render
// degrades to the bare names) — but the per-sandbox rows must render
// UNVERIFIABLE (discovery unavailable), never a false "no sandboxes".
func TestGatherStatusMCPSbxAbsent(t *testing.T) {
	cfg := &config.Config{MCP: []string{gwServerName}}
	env := fakeStatusEnv()
	fakeOf(env).LookPathFn = func(name string) (string, error) { return "", fmt.Errorf("not found") }
	st := gatherStatus(cfg, "default", env)
	if len(st.MCPServers) != 0 {
		t.Errorf("MCPServers = %+v, want empty when sbx absent", st.MCPServers)
	}
	if len(st.MCPRows) != 1 {
		t.Fatalf("MCPRows = %+v, want 1 unverifiable row (discovery unavailable)", st.MCPRows)
	}
	r := st.MCPRows[0]
	if r.State != mcpJoinUnverifiable || r.Registered != "unknown" || r.Sandbox != "" {
		t.Errorf("row = %+v, want unverifiable/unknown with no sandbox claim", r)
	}
	if !strings.Contains(r.Evidence, "sandbox discovery unavailable") {
		t.Errorf("evidence should name the discovery gap: %q", r.Evidence)
	}
}

// TestRenderStatusMCPJSON: --json carries the mcp_servers registration state.
func TestRenderStatusMCPJSON(t *testing.T) {
	cfg := &config.Config{MCP: []string{gwServerName, "slack"}}
	var out bytes.Buffer
	renderStatus(cfg, "default", fakeStatusEnv(), &out, true)
	var st statusReport
	if err := json.Unmarshal(out.Bytes(), &st); err != nil {
		t.Fatalf("status --json invalid: %v\n%s", err, out.String())
	}
	if len(st.MCPServers) != 2 || !st.MCPServers[0].Registered || st.MCPServers[1].Registered {
		t.Errorf("json mcp_servers = %+v, want gog registered + slack not", st.MCPServers)
	}
}

func TestRenderStatusJSON(t *testing.T) {
	cfg := &config.Config{MCP: []string{gwServerName}}
	var out bytes.Buffer
	renderStatus(cfg, "default", fakeStatusEnv(), &out, true)
	var st statusReport
	if err := json.Unmarshal(out.Bytes(), &st); err != nil {
		t.Fatalf("status --json invalid: %v\n%s", err, out.String())
	}
	if st.Profile != "default" {
		t.Errorf("profile = %q, want default", st.Profile)
	}
}

// TestStatusRegisterTodo: with sbx reachable and a configured server NOT
// registered (slack, classified local via pix-host mcp --list), status
// appends exactly one TYPE-CORRECT register TODO so it can't claim "all
// systems go" while a server is unregistered.
func TestStatusRegisterTodo(t *testing.T) {
	cfg := &config.Config{MCP: []string{gwServerName, "slack"}}
	env := fakeStatusEnv() // sbx mcp ls -> gog,notion
	env.HostBinary = func() (string, error) { return "/usr/local/bin/pix-host", nil }
	// probe answers the `pix-host mcp --list` classification call; the
	// sbx probes (secret ls / mcp ls / sbx ls also route through probeRun now)
	// fall back to the canned env.Run outputs.
	run := fakeOf(env).RunFn
	fakeOf(env).RunTimedFn = func(name string, args ...string) (string, bool, error) {
		if name == "/usr/local/bin/pix-host" && len(args) == 2 && args[0] == "mcp" && args[1] == "--list" {
			return "slack\n", false, nil
		}
		out, err := run(name, args...)
		return out, false, err
	}
	st := gatherStatus(cfg, "default", env)
	n := 0
	for _, tdo := range st.Todos {
		if tdo == "pix mcp register slack" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("expected one type-correct `pix mcp register slack` TODO, got %d: %v", n, st.Todos)
	}
}

// TestStatusRegisterTodoUnclassifiable: a verified registration gap whose kind
// can't be established (pix-host mcp --list unavailable) still surfaces
// an outstanding item, but never invents a possibly-wrong repair command.
func TestStatusRegisterTodoUnclassifiable(t *testing.T) {
	cfg := &config.Config{MCP: []string{"slack"}}
	st := gatherStatus(cfg, "default", fakeStatusEnv()) // no hostBinary seam -> kind unknown
	found := false
	for _, tdo := range st.Todos {
		if strings.Contains(tdo, "slack") && strings.Contains(tdo, "pix doctor") {
			found = true
		}
		if strings.HasPrefix(tdo, "pix mcp register") || strings.HasPrefix(tdo, "pix mcp bundle") || strings.HasPrefix(tdo, "sbx mcp add") {
			t.Errorf("unclassifiable server must not get a guessed repair command: %q", tdo)
		}
	}
	if !found {
		t.Errorf("expected an outstanding item pointing at doctor: %v", st.Todos)
	}
}

// TestStatusNoRegisterTodoWhenRegistered: every configured server registered ->
// no register TODO.
func TestStatusNoRegisterTodoWhenRegistered(t *testing.T) {
	cfg := &config.Config{MCP: []string{gwServerName, "notion"}} // both in `sbx mcp ls`
	st := gatherStatus(cfg, "default", fakeStatusEnv())
	for _, tdo := range st.Todos {
		if strings.Contains(tdo, "mcp register") || strings.Contains(tdo, "mcp bundle") {
			t.Errorf("did not expect a register TODO when all servers registered: %v", st.Todos)
		}
	}
}

// TestStatusNoRegisterTodoWhenSbxAbsent: sbx off PATH -> registration is
// unknowable, so status must NOT invent a register TODO.
func TestStatusNoRegisterTodoWhenSbxAbsent(t *testing.T) {
	cfg := &config.Config{MCP: []string{gwServerName, "slack"}}
	env := fakeStatusEnv()
	fakeOf(env).LookPathFn = func(name string) (string, error) { return "", fmt.Errorf("not found") }
	st := gatherStatus(cfg, "default", env)
	for _, tdo := range st.Todos {
		if strings.Contains(tdo, "mcp register") || strings.Contains(tdo, "mcp bundle") {
			t.Errorf("did not expect a register TODO when sbx absent: %v", st.Todos)
		}
	}
}

// TestStatusSbxAbsentNotAllGreen: with sbx off PATH provider keys can't be
// verified, so the verdict must not be falsely "all systems go" — status adds an
// outstanding item and --json/human reflect it.
func TestStatusSbxAbsentNotAllGreen(t *testing.T) {
	cfg := &config.Config{}
	env := shellEnv{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "", fmt.Errorf("not found") }, DialLocalFn: func(int) bool { return false }, IsFileFn: func(string) bool { return false }}}
	st := gatherStatus(cfg, "default", env)
	if len(st.Todos) == 0 {
		t.Fatalf("expected a non-empty Todos when sbx is absent, got none")
	}
	var out bytes.Buffer
	renderStatus(cfg, "default", env, &out, false)
	if strings.Contains(out.String(), "all systems go") {
		t.Errorf("verdict must not be falsely green when sbx is absent, got:\n%s", out.String())
	}
}

// TestStatusGogNeedsAuthTodoNotAllGreen: a configured gog account that is NOT
// authenticated is an outstanding item — status appends a `pix gworkspace setup`
// TODO and the verdict must not be falsely "all systems go", even when every
// provider key is set.
func TestStatusGogNeedsAuthTodoNotAllGreen(t *testing.T) {
	cfg := &config.Config{GogAccount: "me@x.com"}
	env := shellEnv{System: &systest.Fake{LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil }, RunFn: func(name string, args ...string) (string, error) {
		if name == "gog" {
			return "", fmt.Errorf("not authed")
		}
		if name == "sbx" && len(args) >= 1 && args[0] == "secret" {
			return "anthropic\nopenai\ngoogle\ngithub\n", nil
		}
		return "", nil
	}, DialLocalFn: func(int) bool { return false }, IsFileFn: func(string) bool { return false }}}
	st := gatherStatus(cfg, "default", env)
	var gogTodo bool
	for _, tdo := range st.Todos {
		if tdo == gogSetupHint {
			gogTodo = true
		}
	}
	if !gogTodo {
		t.Errorf("expected a `%s` TODO for an unauthed account, got %v", gogSetupHint, st.Todos)
	}
	var out bytes.Buffer
	renderStatus(cfg, "default", env, &out, false)
	if strings.Contains(out.String(), "all systems go") {
		t.Errorf("verdict must not be green when gog is unauthed, got:\n%s", out.String())
	}
}

// TestStatusSbxProbeFailedTodo: sbx IS on PATH but `sbx secret ls` fails. Status
// must emit the "could not verify" TODO (distinct from the install TODO), never
// the install-sbx guidance, and never claim "all systems go".
func TestStatusSbxProbeFailedTodo(t *testing.T) {
	cfg := &config.Config{}
	env := shellEnv{System: &systest.Fake{LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil }, RunFn: func(name string, args ...string) (string, error) {
		if name == "sbx" && len(args) >= 1 && args[0] == "secret" {
			return "", fmt.Errorf("sbx secret ls boom")
		}
		return "", nil
	}, DialLocalFn: func(int) bool { return false }, IsFileFn: func(string) bool { return false }}}
	st := gatherStatus(cfg, "default", env)
	var sawVerify, sawInstall bool
	for _, tdo := range st.Todos {
		if strings.Contains(tdo, "could not verify provider keys") {
			sawVerify = true
		}
		if strings.Contains(tdo, "install the Docker Sandboxes CLI") {
			sawInstall = true
		}
	}
	if !sawVerify {
		t.Errorf("expected a 'could not verify provider keys' TODO, got %v", st.Todos)
	}
	if sawInstall {
		t.Errorf("must NOT emit the install-sbx TODO when sbx is on PATH, got %v", st.Todos)
	}
	var out bytes.Buffer
	renderStatus(cfg, "default", env, &out, false)
	if strings.Contains(out.String(), "all systems go") {
		t.Errorf("verdict must not be green when the key probe failed, got:\n%s", out.String())
	}
}

// TestStatusGogNeedsAuth: with a gog account set but no usable auth, the human
// render shows the "needs auth (run pix gworkspace setup)" integrations line.
func TestStatusGogNeedsAuth(t *testing.T) {
	cfg := &config.Config{GogAccount: "me@x.com"}
	env := shellEnv{System: &systest.Fake{LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil }, RunFn: func(name string, args ...string) (string, error) {
		if name == "gog" {
			return "", fmt.Errorf("not authed")
		}
		return "", nil
	}, DialLocalFn: func(int) bool { return false }, IsFileFn: func(string) bool { return false }}}
	var out bytes.Buffer
	renderStatus(cfg, "default", env, &out, false)
	s := out.String()
	if !strings.Contains(s, "workspace") || !strings.Contains(s, "needs auth (run "+gogSetupHint+")") {
		t.Errorf("expected gog needs-auth integrations line, got:\n%s", s)
	}
}

// TestStatusOpenAIOnlyAllSystemsGo pins finding #3: with ONLY openai present
// (anthropic/google unset), status must still read all-systems-go — core
// model readiness needs just one of the three, and the missing alternates
// (plus an absent github) are informational, never outstanding.
func TestStatusOpenAIOnlyAllSystemsGo(t *testing.T) {
	cfg := &config.Config{}
	env := shellEnv{System: &systest.Fake{LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil }, RunFn: func(name string, args ...string) (string, error) {
		if name == "sbx" && len(args) >= 1 && args[0] == "secret" {
			return "openai\n", nil
		}
		return "", nil
	}, DialLocalFn: func(int) bool { return false }, IsFileFn: func(string) bool { return false }}}
	st := gatherStatus(cfg, "default", env)
	if len(st.Todos) != 0 {
		t.Errorf("todos = %v, want none (openai alone satisfies core readiness)", st.Todos)
	}
	var out bytes.Buffer
	renderStatus(cfg, "default", env, &out, false)
	if !strings.Contains(out.String(), "all systems go") {
		t.Errorf("want all-systems-go with only openai present, got:\n%s", out.String())
	}
}

// TestStatusZeroModelKeysOneTodo pins finding #3's other half: POSITIVELY
// zero model-provider keys confirmed -> exactly ONE core fix TODO (never a
// per-key TODO per missing provider), and the verdict is not falsely green.
func TestStatusZeroModelKeysOneTodo(t *testing.T) {
	cfg := &config.Config{}
	env := shellEnv{System: &systest.Fake{LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil }, RunFn: func(name string, args ...string) (string, error) {
		if name == "sbx" && len(args) >= 1 && args[0] == "secret" {
			return "", nil // sbx reachable, nothing set at all
		}
		return "", nil
	}, DialLocalFn: func(int) bool { return false }, IsFileFn: func(string) bool { return false }}}
	st := gatherStatus(cfg, "default", env)
	if len(st.Todos) != 1 || st.Todos[0] != modelKeyFixCmd {
		t.Errorf("todos = %v, want exactly [%q]", st.Todos, modelKeyFixCmd)
	}
	var out bytes.Buffer
	renderStatus(cfg, "default", env, &out, false)
	if strings.Contains(out.String(), "all systems go") {
		t.Errorf("verdict must not be green with zero confirmed keys, got:\n%s", out.String())
	}
}

// TestStatusProbeFailureNoProviderTodo pins finding #3: when the key probe
// itself is unavailable (sbx absent, or `sbx secret ls` failed), status must
// never invent a provider/model-key TODO — it does not KNOW keys are
// missing. The distinct "can't verify" TODO still fires (covered by
// TestStatusSbxAbsentNotAllGreen / TestStatusSbxProbeFailedTodo); this test
// pins the NEGATIVE: no core or per-key provider command leaks in either case.
func TestStatusProbeFailureNoProviderTodo(t *testing.T) {
	assertNoProviderTodo := func(t *testing.T, st statusReport) {
		t.Helper()
		for _, tdo := range st.Todos {
			if tdo == modelKeyFixCmd || strings.HasPrefix(tdo, "sbx secret set -g ") {
				t.Errorf("must not invent a provider-key TODO when the probe is unavailable, got %q in %v", tdo, st.Todos)
			}
		}
	}

	t.Run("sbx absent", func(t *testing.T) {
		cfg := &config.Config{}
		env := shellEnv{System: &systest.Fake{LookPathFn: func(string) (string, error) { return "", fmt.Errorf("not found") }, DialLocalFn: func(int) bool { return false }, IsFileFn: func(string) bool { return false }}}
		assertNoProviderTodo(t, gatherStatus(cfg, "default", env))
	})

	t.Run("sbx present, secret ls failed", func(t *testing.T) {
		cfg := &config.Config{}
		env := shellEnv{System: &systest.Fake{LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil }, RunFn: func(name string, args ...string) (string, error) {
			if name == "sbx" && len(args) >= 1 && args[0] == "secret" {
				return "", fmt.Errorf("sbx secret ls boom")
			}
			return "", nil
		}, DialLocalFn: func(int) bool { return false }, IsFileFn: func(string) bool { return false }}}
		assertNoProviderTodo(t, gatherStatus(cfg, "default", env))
	})
}

// TestStatusMCPRegistrationUnverifiableBlocksAllGreen pins closure finding
// #1: configured/current-intent MCP names exist, sbx is present and
// provider keys ARE ready, but `sbx mcp ls` fails, and `sbx ls` succeeds
// with ZERO pix sandboxes. Registration cannot be verified — the
// verdict must not read "all systems go" even though nothing else is
// outstanding and there are no sandboxes/rows to render unverifiable.
func TestStatusMCPRegistrationUnverifiableBlocksAllGreen(t *testing.T) {
	cfg := &config.Config{MCP: []string{gwServerName}}
	env := shellEnv{System: &systest.Fake{LookPathFn: func(name string) (string, error) { return "/usr/bin/" + name, nil }, RunFn: func(name string, args ...string) (string, error) {
		if name == "sbx" && len(args) >= 1 && args[0] == "secret" {
			return "anthropic\n", nil // provider-ready
		}
		if name == "sbx" && len(args) >= 2 && args[0] == "mcp" && args[1] == "ls" {
			return "", fmt.Errorf("mcp ls boom") // registration probe fails
		}
		if name == "sbx" && len(args) >= 1 && args[0] == "ls" {
			return "NAME STATUS\n", nil // zero pix sandboxes
		}
		return "", nil
	}, DialLocalFn: func(int) bool { return false }, IsFileFn: func(string) bool { return false }}}
	st := gatherStatus(cfg, "default", env)
	if len(st.Todos) != 0 {
		t.Errorf("todos = %v, want none (a failed registration probe is unverifiable, never a false TODO)", st.Todos)
	}
	foundUnverifiable := false
	for _, m := range st.MCPServers {
		if m.Name == gwServerName && m.Unverifiable {
			foundUnverifiable = true
		}
	}
	if !foundUnverifiable {
		t.Errorf("MCPServers = %+v, want a gog entry flagged unverifiable", st.MCPServers)
	}
	var out bytes.Buffer
	renderStatus(cfg, "default", env, &out, false)
	if strings.Contains(out.String(), "all systems go") {
		t.Errorf("verdict must not read all-systems-go with unverifiable registration and zero sandboxes, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "unverifiable (not failed") {
		t.Errorf("expected the unverifiable-not-failed headline, got:\n%s", out.String())
	}

	var jout bytes.Buffer
	renderStatus(cfg, "default", env, &jout, true)
	var jst statusReport
	if err := json.Unmarshal(jout.Bytes(), &jst); err != nil {
		t.Fatalf("status --json invalid: %v\n%s", err, jout.String())
	}
	found := false
	for _, m := range jst.MCPServers {
		if m.Name == gwServerName && m.Unverifiable {
			found = true
		}
	}
	if !found {
		t.Errorf("json mcp_servers = %+v, want a gog entry with unverifiable:true", jst.MCPServers)
	}
}

// TestStatusMCPLoadTodoQuotesWorkspace pins closure finding #3: the mcp/box
// registered-not-attached repair command status prints shell-quotes both the
// server name and the workspace, so a path with spaces, an apostrophe, and a
// shell metacharacter round-trips safely when copy-pasted.
func TestStatusMCPLoadTodoQuotesWorkspace(t *testing.T) {
	cfg := &config.Config{MCP: []string{"slack"}}
	const ws = "/home/u/my repo's proj; touch pwned"
	const box = "pix-proj"
	env := fakeStatusEnv()
	stateDir := t.TempDir()
	fakeOf(env).StateDirFn = func() (string, error) { return stateDir, nil }
	fakeOf(env).RunFn = func(name string, args ...string) (string, error) {
		if name == "sbx" && len(args) >= 1 && args[0] == "secret" {
			return "anthropic\n", nil
		}
		if name == "sbx" && len(args) >= 2 && args[0] == "mcp" && args[1] == "ls" {
			return "slack\n", nil
		}
		if name == "sbx" && len(args) >= 1 && args[0] == "ls" {
			return "NAME STATUS\n" + box + " running\n", nil
		}
		return "", nil
	}
	if err := writeCreateReceipt(stateDir, box, ws, []string{"notion"}, receiptClock); err != nil {
		t.Fatal(err)
	}
	st := gatherStatus(cfg, "default", env)
	var td string
	for _, tdo := range st.Todos {
		if strings.HasPrefix(tdo, "pix mcp load") {
			td = tdo
		}
	}
	want := "pix mcp load " + shellQuoteArg("slack") + " " + shellQuoteArg(ws)
	if td != want {
		t.Errorf("todo = %q, want %q", td, want)
	}
}

func TestParseSandboxes(t *testing.T) {
	out := parseSandboxes("NAME STATUS\npix-a running\nfoo bar\npix-b stopped\n")
	if len(out) != 2 {
		t.Fatalf("got %d, want 2: %v", len(out), out)
	}
	if out[0].Name != "pix-a" || out[0].State != "running" {
		t.Errorf("out[0] = %+v", out[0])
	}
}

func TestStatusRendersConfiguredInferenceInsteadOfIrrelevantProviderKeys(t *testing.T) {
	st := statusReport{
		Providers: map[string]bool{}, InferenceModels: 3,
		InferenceBackends: []string{"work-anthropic", "work-openai"},
	}
	var out bytes.Buffer
	st.render(&out)
	got := out.String()
	if !strings.Contains(got, "inference    3 model(s) via work-anthropic, work-openai") {
		t.Fatalf("missing inference summary:\n%s", got)
	}
	if strings.Contains(got, "providers    ") {
		t.Fatalf("gateway topology must hide irrelevant provider-key row:\n%s", got)
	}
}
