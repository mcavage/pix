package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"pi-stack/host/config"
	"pi-stack/host/monitor"
)

// fakeStatusEnv builds a shellEnv where memory is up, knowledge down, sbx lists
// two boxes and reports two secrets set.
// statusHostBinary is the canonical pi-stack-host path fakeStatusEnv resolves
// env.hostBinary() to, so localMCPNames classification can confirm "slack" is
// a LOCAL stdio server (mirroring a real `pi-stack-host mcp --list`) rather
// than degrading to unknown classification in every MCP-related status test.
const statusHostBinary = "/usr/local/bin/pi-stack-host"

func fakeStatusEnv() shellEnv {
	return shellEnv{
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		run: func(name string, args ...string) (string, error) {
			if name == "sbx" && len(args) >= 1 && args[0] == "secret" {
				return "anthropic\nopenai\n", nil
			}
			if name == "sbx" && len(args) >= 1 && args[0] == "ls" {
				return "NAME STATUS\npi-stack-myrepo running\npi-stack-scratch stopped\nother-box running\n", nil
			}
			if name == "sbx" && len(args) >= 2 && args[0] == "mcp" && args[1] == "ls" {
				return "gog\nnotion\n", nil
			}
			if name == statusHostBinary && len(args) >= 2 && args[0] == "mcp" && args[1] == "--list" {
				return "slack", nil
			}
			return "", nil
		},
		hostBinary: func() (string, error) { return statusHostBinary, nil },
		dial:       func(port int) bool { return port == memoryPortDefault },
		statFile:   func(string) bool { return false },
	}
}

func TestGatherStatus(t *testing.T) {
	cfg := &config.Config{MCP: []string{"gog"}, KnowledgeBundles: []string{"/kb"}}
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
	// finding #4: with two of three model-provider keys already present,
	// google is merely an unused alternative, not an outstanding gap -- and
	// GitHub is always optional. Neither may add a todo.
	if len(st.Todos) != 0 {
		t.Errorf("todos = %v, want 0 (one-of-three keys already satisfied; github is optional)", st.Todos)
	}
	// Only pi-stack-* sandboxes, "other-box" filtered out.
	if len(st.Sandboxes) != 2 {
		t.Errorf("sandboxes = %v, want 2 pi-stack boxes", st.Sandboxes)
	}
	for _, s := range st.Sandboxes {
		if !strings.HasPrefix(s.Name, "pi-stack-") {
			t.Errorf("leaked non-pi-stack sandbox: %s", s.Name)
		}
	}
}

// TestGatherStatus_OneKeyNoOutstandingAlternatives is finding #4: with any
// ONE of anthropic/openai/google set, the missing alternatives must not add a
// todo, and GitHub (always optional) must never add one either -- status must
// agree with doctor's modelProviderAggregateCheck/secretCheck semantics.
func TestGatherStatus_OneKeyNoOutstandingAlternatives(t *testing.T) {
	cfg := &config.Config{}
	env := shellEnv{
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		run: func(name string, args ...string) (string, error) {
			if name == "sbx" && len(args) >= 1 && args[0] == "secret" {
				return "anthropic\n", nil // exactly ONE of three model keys
			}
			return "", nil
		},
		dial:     func(int) bool { return false },
		statFile: func(string) bool { return false },
	}
	st := gatherStatus(cfg, "default", env)
	if !st.Providers["anthropic"] {
		t.Fatalf("expected anthropic set, providers=%v", st.Providers)
	}
	if st.Providers["openai"] || st.Providers["google"] || st.Providers["github"] {
		t.Errorf("expected openai/google/github unset, providers=%v", st.Providers)
	}
	if len(st.Todos) != 0 {
		t.Errorf("one model-provider key must leave zero outstanding todos, got %v", st.Todos)
	}
}

// TestGatherStatus_ZeroKeysStillOutstanding: the flip side -- with NONE of the
// three model-provider keys set, status must still surface a genuine,
// required TODO (this is not a blanket suppression, only the one-key and
// github-optional cases are exempted).
func TestGatherStatus_ZeroKeysStillOutstanding(t *testing.T) {
	cfg := &config.Config{}
	env := shellEnv{
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		run: func(name string, args ...string) (string, error) {
			if name == "sbx" && len(args) >= 1 && args[0] == "secret" {
				return "github\n", nil // github set, but zero of the three model keys
			}
			return "", nil
		},
		dial:     func(int) bool { return false },
		statFile: func(string) bool { return false },
	}
	st := gatherStatus(cfg, "default", env)
	found := false
	for _, tdo := range st.Todos {
		if tdo == "sbx secret set -g anthropic" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a genuine model-provider-key todo when zero of three are set, got %v", st.Todos)
	}
	var out bytes.Buffer
	renderStatus(cfg, "default", env, &out, false)
	if strings.Contains(out.String(), "all systems go") {
		t.Errorf("verdict must not be green with zero model-provider keys, got:\n%s", out.String())
	}
}

func TestRenderStatusHuman(t *testing.T) {
	// slack is configured but not in fakeStatusEnv's `sbx mcp ls` output ("gog\nnotion"),
	// which is a genuine outstanding item independent of provider-key semantics --
	// keeps this test decoupled from finding #4's one-key/github-optional fix.
	cfg := &config.Config{MCP: []string{"gog", "slack"}, KnowledgeBundles: []string{"/kb"}}
	var out bytes.Buffer
	renderStatus(cfg, "default", fakeStatusEnv(), &out, false)
	s := out.String()
	for _, want := range []string{"pi-stack", "services", "memory ✓", "knowledge ✗", "outstanding"} {
		if !strings.Contains(s, want) {
			t.Errorf("status output missing %q:\n%s", want, s)
		}
	}
}

// TestGatherStatusMonitor (DX-5): the monitor hub's up/down state is probed
// via env.dial(monitor.DefaultPort), independent of memory/knowledge.
func TestGatherStatusMonitor(t *testing.T) {
	cfg := &config.Config{}
	env := fakeStatusEnv()
	env.dial = func(port int) bool { return port == monitor.DefaultPort }
	st := gatherStatus(cfg, "default", env)
	if !st.Monitor {
		t.Error("monitor should be up when its port dials")
	}
	if st.Memory {
		t.Error("memory should be down (dial only matches the monitor port here)")
	}
}

// TestRenderStatusMonitorLine (DX-5): the human render shows a monitor line
// with its glyph and port, consistent with the memory/knowledge line style.
func TestRenderStatusMonitorLine(t *testing.T) {
	cfg := &config.Config{}
	env := fakeStatusEnv()
	env.dial = func(port int) bool { return port == monitor.DefaultPort }
	var out bytes.Buffer
	renderStatus(cfg, "default", env, &out, false)
	s := out.String()
	if !strings.Contains(s, fmt.Sprintf("monitor     ✓ :%d", monitor.DefaultPort)) {
		t.Errorf("status output missing the monitor line:\n%s", s)
	}
}

// TestRenderStatusMonitorJSON (DX-5): --json carries monitor_up.
func TestRenderStatusMonitorJSON(t *testing.T) {
	cfg := &config.Config{}
	env := fakeStatusEnv()
	env.dial = func(port int) bool { return port == monitor.DefaultPort }
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

// TestGatherStatusMCP: cfg.MCP servers get a registration + attach-on-run state
// parsed from `sbx mcp ls`; gog is registered, slack is not. Neither is pinned
// via mcp_static, so BOTH are default-dynamic (not attach-on-run) -- this is
// the tri-state fix: cfg.MCP membership alone must never imply eager attach.
func TestGatherStatusMCP(t *testing.T) {
	cfg := &config.Config{MCP: []string{"gog", "slack"}}
	st := gatherStatus(cfg, "default", fakeStatusEnv())
	if len(st.MCPServers) != 2 {
		t.Fatalf("MCPServers = %+v, want 2 entries", st.MCPServers)
	}
	byName := map[string]mcpStatusLine{}
	for _, m := range st.MCPServers {
		byName[m.Name] = m
	}
	if !byName["gog"].Registered {
		t.Errorf("gog should be registered: %+v", byName["gog"])
	}
	if byName["slack"].Registered {
		t.Errorf("slack should NOT be registered: %+v", byName["slack"])
	}
	for _, m := range st.MCPServers {
		if m.Attach {
			t.Errorf("%s should be default-dynamic, not attach-on-run (no mcp_static pin): %+v", m.Name, m)
		}
	}
}

// TestGatherStatusMCPStaticPin: a server pinned via cfg.MCPStatic renders
// attach-on-run/eager, using the exact resolveStaticMCP semantics run.go uses
// for launch -- status must never invent its own eager/lazy rule.
func TestGatherStatusMCPStaticPin(t *testing.T) {
	cfg := &config.Config{MCP: []string{"gog", "slack"}, MCPStatic: []string{"gog"}}
	st := gatherStatus(cfg, "default", fakeStatusEnv())
	byName := map[string]mcpStatusLine{}
	for _, m := range st.MCPServers {
		byName[m.Name] = m
	}
	if !byName["gog"].Attach {
		t.Errorf("gog is in mcp_static, should be attach-on-run: %+v", byName["gog"])
	}
	if byName["slack"].Attach {
		t.Errorf("slack has no mcp_static pin, should be default-dynamic: %+v", byName["slack"])
	}
}

// TestGatherStatusMCPDynamicOverride: mcp_dynamic wins over mcp_static (same
// precedence as resolveStaticMCP/resolveStaticMCPForRun) -- a server in BOTH
// lists stays dynamic, never attach-on-run.
func TestGatherStatusMCPDynamicOverride(t *testing.T) {
	cfg := &config.Config{MCP: []string{"gog"}, MCPStatic: []string{"gog"}, MCPDynamic: []string{"gog"}}
	st := gatherStatus(cfg, "default", fakeStatusEnv())
	if len(st.MCPServers) != 1 || st.MCPServers[0].Attach {
		t.Errorf("mcp_dynamic must win over mcp_static, got %+v", st.MCPServers)
	}
}

// TestRenderStatusMCPDynamicLabel: a default-dynamic registered server renders
// as dynamically discoverable, never with the attach-on-run wording or a ✗
// glyph (it isn't a failure -- it's the default, working-as-intended state).
func TestRenderStatusMCPDynamicLabel(t *testing.T) {
	cfg := &config.Config{MCP: []string{"gog"}}
	var out bytes.Buffer
	renderStatus(cfg, "default", fakeStatusEnv(), &out, false)
	s := out.String()
	if !strings.Contains(s, "dynamically discoverable") {
		t.Errorf("expected dynamically discoverable label for default-dynamic gog, got:\n%s", s)
	}
	if strings.Contains(s, "✗ attach-on-run") {
		t.Errorf("must never render a ✗ attach-on-run for a default-dynamic server, got:\n%s", s)
	}
}

// TestRenderStatusMCPStaticLabel: an mcp_static-pinned server renders the
// attach-on-run/eager wording.
func TestRenderStatusMCPStaticLabel(t *testing.T) {
	cfg := &config.Config{MCP: []string{"gog"}, MCPStatic: []string{"gog"}}
	var out bytes.Buffer
	renderStatus(cfg, "default", fakeStatusEnv(), &out, false)
	s := out.String()
	if !strings.Contains(s, "attach-on-run") {
		t.Errorf("expected attach-on-run label for mcp_static-pinned gog, got:\n%s", s)
	}
}

// TestGatherStatusMCPSbxAbsent: with sbx off PATH, MCPServers is empty so render
// degrades to the bare names.
func TestGatherStatusMCPSbxAbsent(t *testing.T) {
	cfg := &config.Config{MCP: []string{"gog"}}
	env := fakeStatusEnv()
	env.lookPath = func(name string) (string, error) { return "", fmt.Errorf("not found") }
	st := gatherStatus(cfg, "default", env)
	if len(st.MCPServers) != 0 {
		t.Errorf("MCPServers = %+v, want empty when sbx absent", st.MCPServers)
	}
}

// TestRenderStatusMCPJSON: --json carries the mcp_servers registration state,
// and Attach reflects resolveStaticMCP (mcp_static-pinned only).
func TestRenderStatusMCPJSON(t *testing.T) {
	cfg := &config.Config{MCP: []string{"gog", "slack"}, MCPStatic: []string{"gog"}}
	var out bytes.Buffer
	renderStatus(cfg, "default", fakeStatusEnv(), &out, true)
	var st statusReport
	if err := json.Unmarshal(out.Bytes(), &st); err != nil {
		t.Fatalf("status --json invalid: %v\n%s", err, out.String())
	}
	if len(st.MCPServers) != 2 {
		t.Fatalf("json mcp_servers = %+v, want 2 entries", st.MCPServers)
	}
	byName := map[string]mcpStatusLine{}
	for _, m := range st.MCPServers {
		byName[m.Name] = m
	}
	if !byName["gog"].Attach {
		t.Errorf("json gog should be attach-on-run (mcp_static-pinned): %+v", st.MCPServers)
	}
	if byName["slack"].Attach {
		t.Errorf("json slack should be default-dynamic (not pinned): %+v", st.MCPServers)
	}
}

func TestRenderStatusJSON(t *testing.T) {
	cfg := &config.Config{MCP: []string{"gog"}}
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
// registered (slack), status appends exactly one `pi-stack mcp register` TODO so
// it can't claim "all systems go" while a server is unregistered.
func TestStatusRegisterTodo(t *testing.T) {
	cfg := &config.Config{MCP: []string{"gog", "slack"}}
	st := gatherStatus(cfg, "default", fakeStatusEnv()) // sbx mcp ls -> gog,notion
	n := 0
	for _, tdo := range st.Todos {
		if tdo == "pi-stack mcp register" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("expected one `pi-stack mcp register` TODO (slack unregistered), got %d: %v", n, st.Todos)
	}
}

// TestStatusNoRegisterTodoWhenRegistered: every configured server registered ->
// no register TODO.
func TestStatusNoRegisterTodoWhenRegistered(t *testing.T) {
	cfg := &config.Config{MCP: []string{"gog", "notion"}} // both in `sbx mcp ls`
	st := gatherStatus(cfg, "default", fakeStatusEnv())
	for _, tdo := range st.Todos {
		if tdo == "pi-stack mcp register" {
			t.Errorf("did not expect a register TODO when all servers registered: %v", st.Todos)
		}
	}
}

// TestStatusNoRegisterTodoWhenSbxAbsent: sbx off PATH -> registration is
// unknowable, so status must NOT invent a register TODO.
func TestStatusNoRegisterTodoWhenSbxAbsent(t *testing.T) {
	cfg := &config.Config{MCP: []string{"gog", "slack"}}
	env := fakeStatusEnv()
	env.lookPath = func(name string) (string, error) { return "", fmt.Errorf("not found") }
	st := gatherStatus(cfg, "default", env)
	for _, tdo := range st.Todos {
		if tdo == "pi-stack mcp register" {
			t.Errorf("did not expect a register TODO when sbx absent: %v", st.Todos)
		}
	}
}

// TestStatusSbxAbsentNotAllGreen: with sbx off PATH provider keys can't be
// verified, so the verdict must not be falsely "all systems go" — status adds an
// outstanding item and --json/human reflect it.
func TestStatusSbxAbsentNotAllGreen(t *testing.T) {
	cfg := &config.Config{}
	env := shellEnv{
		lookPath: func(string) (string, error) { return "", fmt.Errorf("not found") },
		dial:     func(int) bool { return false },
		statFile: func(string) bool { return false },
	}
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
// authenticated is an outstanding item — status appends a `pi-stack gog setup`
// TODO (the guided recovery path, finding #3 -- never the legacy `gog auth
// login` recipe) and the verdict must not be falsely "all systems go", even
// when every provider key is set.
func TestStatusGogNeedsAuthTodoNotAllGreen(t *testing.T) {
	cfg := &config.Config{GogAccount: "me@x.com"}
	env := shellEnv{
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		run: func(name string, args ...string) (string, error) {
			if name == "gog" {
				return "", fmt.Errorf("not authed")
			}
			if name == "sbx" && len(args) >= 1 && args[0] == "secret" {
				return "anthropic\nopenai\ngoogle\ngithub\n", nil
			}
			return "", nil
		},
		dial:     func(int) bool { return false },
		statFile: func(string) bool { return false },
	}
	st := gatherStatus(cfg, "default", env)
	var gogTodo bool
	for _, tdo := range st.Todos {
		if tdo == "pi-stack gog setup" {
			gogTodo = true
		}
		if strings.Contains(tdo, "gog auth login") || strings.Contains(tdo, "add-client") {
			t.Errorf("must never recommend the legacy gog auth recipe, got TODO %q", tdo)
		}
	}
	if !gogTodo {
		t.Errorf("expected a `pi-stack gog setup` TODO for an unauthed account, got %v", st.Todos)
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
	env := shellEnv{
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		run: func(name string, args ...string) (string, error) {
			if name == "sbx" && len(args) >= 1 && args[0] == "secret" {
				return "", fmt.Errorf("sbx secret ls boom")
			}
			return "", nil
		},
		dial:     func(int) bool { return false },
		statFile: func(string) bool { return false },
	}
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

// TestStatusSbxAbsentProvidersUnverifiable is the tri-state fix: with sbx off
// PATH, every provider must be reported EvidenceUnverifiable (not a confirmed
// EvidenceFailed), and the human/JSON render must use the unverifiable glyph
// (⚠), never a bare ✗ that reads as "confirmed missing key".
func TestStatusSbxAbsentProvidersUnverifiable(t *testing.T) {
	cfg := &config.Config{}
	env := shellEnv{
		lookPath: func(string) (string, error) { return "", fmt.Errorf("not found") },
		dial:     func(int) bool { return false },
		statFile: func(string) bool { return false },
	}
	st := gatherStatus(cfg, "default", env)
	for _, key := range []string{"anthropic", "openai", "google", "github"} {
		if st.ProviderEvidence[key] != EvidenceUnverifiable {
			t.Errorf("provider_evidence[%s] = %q, want %q", key, st.ProviderEvidence[key], EvidenceUnverifiable)
		}
		// JSON compatibility: the bool map stays false (never a confirmed
		// present), it just isn't the SOLE signal anymore.
		if st.Providers[key] {
			t.Errorf("providers[%s] = true, want false when unverifiable", key)
		}
	}
	var out bytes.Buffer
	renderStatus(cfg, "default", env, &out, false)
	s := out.String()
	if !strings.Contains(s, "⚠") {
		t.Errorf("expected the unverifiable glyph (⚠) in provider render, got:\n%s", s)
	}
	if strings.Contains(s, "anthropic ✗") || strings.Contains(s, "openai ✗") || strings.Contains(s, "google ✗") || strings.Contains(s, "github ✗") {
		t.Errorf("must not render a confirmed-missing ✗ glyph for an unverifiable provider, got:\n%s", s)
	}

	// --json carries the same tri-state.
	var jout bytes.Buffer
	renderStatus(cfg, "default", env, &jout, true)
	var jst statusReport
	if err := json.Unmarshal(jout.Bytes(), &jst); err != nil {
		t.Fatalf("status --json invalid: %v\n%s", err, jout.String())
	}
	for _, key := range []string{"anthropic", "openai", "google", "github"} {
		if jst.ProviderEvidence[key] != EvidenceUnverifiable {
			t.Errorf("json provider_evidence[%s] = %q, want %q", key, jst.ProviderEvidence[key], EvidenceUnverifiable)
		}
	}
}

// TestStatusSbxProbeFailedProvidersUnverifiable: sbx IS on PATH but `sbx
// secret ls` fails -- same tri-state as sbx-absent (unverifiable, not
// confirmed-failed), since the probe never actually ran.
func TestStatusSbxProbeFailedProvidersUnverifiable(t *testing.T) {
	cfg := &config.Config{}
	env := shellEnv{
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		run: func(name string, args ...string) (string, error) {
			if name == "sbx" && len(args) >= 1 && args[0] == "secret" {
				return "", fmt.Errorf("sbx secret ls boom")
			}
			return "", nil
		},
		dial:     func(int) bool { return false },
		statFile: func(string) bool { return false },
	}
	st := gatherStatus(cfg, "default", env)
	for _, key := range []string{"anthropic", "openai", "google", "github"} {
		if st.ProviderEvidence[key] != EvidenceUnverifiable {
			t.Errorf("provider_evidence[%s] = %q, want %q when the secret-ls probe fails", key, st.ProviderEvidence[key], EvidenceUnverifiable)
		}
	}
}

// TestStatusProvidersHealthyAndFailedEvidence: a successful probe still
// distinguishes a confirmed-set key (healthy) from a confirmed-absent one
// (failed) -- the tri-state fix only changes the UNVERIFIABLE case.
func TestStatusProvidersHealthyAndFailedEvidence(t *testing.T) {
	cfg := &config.Config{}
	env := shellEnv{
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		run: func(name string, args ...string) (string, error) {
			if name == "sbx" && len(args) >= 1 && args[0] == "secret" {
				return "anthropic\n", nil
			}
			return "", nil
		},
		dial:     func(int) bool { return false },
		statFile: func(string) bool { return false },
	}
	st := gatherStatus(cfg, "default", env)
	if st.ProviderEvidence["anthropic"] != EvidenceHealthy {
		t.Errorf("anthropic evidence = %q, want healthy", st.ProviderEvidence["anthropic"])
	}
	if st.ProviderEvidence["openai"] != EvidenceFailed {
		t.Errorf("openai evidence = %q, want failed (confirmed absent)", st.ProviderEvidence["openai"])
	}
}

// TestStatusGogNeedsAuth: with a gog account set but no usable auth, the human
// render shows the "needs auth (run pi-stack gog setup)" integrations line --
// the guided recovery path, never the legacy `gog auth login` recipe.
func TestStatusGogNeedsAuth(t *testing.T) {
	cfg := &config.Config{GogAccount: "me@x.com"}
	env := shellEnv{
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		run: func(name string, args ...string) (string, error) {
			if name == "gog" {
				return "", fmt.Errorf("not authed")
			}
			return "", nil
		},
		dial:     func(int) bool { return false },
		statFile: func(string) bool { return false },
	}
	var out bytes.Buffer
	renderStatus(cfg, "default", env, &out, false)
	s := out.String()
	if !strings.Contains(s, "gog") || !strings.Contains(s, "needs auth (run pi-stack gog setup)") {
		t.Errorf("expected gog needs-auth integrations line, got:\n%s", s)
	}
	if strings.Contains(s, "gog auth login") || strings.Contains(s, "add-client") {
		t.Errorf("must never surface the legacy gog auth recipe, got:\n%s", s)
	}
}

func TestParseSandboxes(t *testing.T) {
	out := parseSandboxes("NAME STATUS\npi-stack-a running\nfoo bar\npi-stack-b stopped\n")
	if len(out) != 2 {
		t.Fatalf("got %d, want 2: %v", len(out), out)
	}
	if out[0].Name != "pi-stack-a" || out[0].State != "running" {
		t.Errorf("out[0] = %+v", out[0])
	}
}
