package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"pi-stack/host/config"
)

// fakeStatusEnv builds a shellEnv where memory is up, knowledge down, sbx lists
// two boxes and reports two secrets set.
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
			return "", nil
		},
		dial:     func(port int) bool { return port == memoryPortDefault },
		statFile: func(string) bool { return false },
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
	// google + github missing -> two todos.
	if len(st.Todos) != 2 {
		t.Errorf("todos = %v, want 2 (google, github)", st.Todos)
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

func TestRenderStatusHuman(t *testing.T) {
	cfg := &config.Config{MCP: []string{"gog"}, KnowledgeBundles: []string{"/kb"}}
	var out bytes.Buffer
	renderStatus(cfg, "default", fakeStatusEnv(), &out, false)
	s := out.String()
	for _, want := range []string{"pi-stack", "services", "memory ✓", "knowledge ✗", "profile: default", "outstanding"} {
		if !strings.Contains(s, want) {
			t.Errorf("status output missing %q:\n%s", want, s)
		}
	}
}

// TestGatherStatusMCP: cfg.MCP servers get a registration + attach-on-run state
// parsed from `sbx mcp ls`; gog is registered, slack is not.
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
		if !m.Attach {
			t.Errorf("%s should be attach-on-run (it's in cfg.MCP)", m.Name)
		}
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

// TestRenderStatusMCPJSON: --json carries the mcp_servers registration state.
func TestRenderStatusMCPJSON(t *testing.T) {
	cfg := &config.Config{MCP: []string{"gog", "slack"}}
	var out bytes.Buffer
	renderStatus(cfg, "default", fakeStatusEnv(), &out, true)
	var st statusReport
	if err := json.Unmarshal(out.Bytes(), &st); err != nil {
		t.Fatalf("status --json invalid: %v\n%s", err, out.String())
	}
	if len(st.MCPServers) != 2 || !st.MCPServers[0].Attach {
		t.Errorf("json mcp_servers = %+v, want 2 attach-on-run entries", st.MCPServers)
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
// authenticated is an outstanding item — status appends a `gog auth login` TODO
// and the verdict must not be falsely "all systems go", even when every provider
// key is set.
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
		if tdo == "gog auth login" {
			gogTodo = true
		}
	}
	if !gogTodo {
		t.Errorf("expected a `gog auth login` TODO for an unauthed account, got %v", st.Todos)
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

// TestStatusGogNeedsAuth: with a gog account set but no usable auth, the human
// render shows the "needs auth (run gog auth login)" integrations line.
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
	if !strings.Contains(s, "gog") || !strings.Contains(s, "needs auth (run gog auth login)") {
		t.Errorf("expected gog needs-auth integrations line, got:\n%s", s)
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
