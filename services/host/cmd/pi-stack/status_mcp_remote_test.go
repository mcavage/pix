package main

import (
	"strings"
	"testing"

	"pi-stack/host/config"
)

// status_mcp_remote_test.go covers finding #1's status-side half: status must
// use REMOTE-specific registration guidance (`pi-stack mcp bundle`) for a
// confirmed remote gateway-catalog server, never the generic local
// `pi-stack mcp register`, and must not guess when the classification itself
// is unknown.

// statusRemoteEnv wires hostBinary + a local-name list that does NOT include
// "notion", so localMCPNames classifies notion as a confirmed REMOTE server.
func statusRemoteEnv(mcpLs string) shellEnv {
	return shellEnv{
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		run: func(name string, args ...string) (string, error) {
			if name == "sbx" && len(args) >= 1 && args[0] == "secret" {
				return "anthropic\n", nil
			}
			if name == "sbx" && len(args) >= 2 && args[0] == "mcp" && args[1] == "ls" {
				return mcpLs, nil
			}
			if name == statusHostBinary && len(args) >= 2 && args[0] == "mcp" && args[1] == "--list" {
				return "slack", nil // notion is deliberately NOT in this list
			}
			return "", nil
		},
		hostBinary: func() (string, error) { return statusHostBinary, nil },
		dial:       func(int) bool { return false },
		statFile:   func(string) bool { return false },
	}
}

// TestStatus_RemoteUnregistered_RecommendsBundle: an unregistered CONFIRMED
// remote server (notion) must add `pi-stack mcp bundle`, never
// `pi-stack mcp register`.
func TestStatus_RemoteUnregistered_RecommendsBundle(t *testing.T) {
	cfg := &config.Config{MCP: []string{"notion"}}
	st := gatherStatus(cfg, "default", statusRemoteEnv("gog\n")) // notion not in sbx mcp ls
	var sawBundle, sawRegister bool
	for _, tdo := range st.Todos {
		if tdo == "pi-stack mcp bundle" {
			sawBundle = true
		}
		if tdo == "pi-stack mcp register" {
			sawRegister = true
		}
	}
	if !sawBundle {
		t.Errorf("expected `pi-stack mcp bundle` for an unregistered remote server, got %v", st.Todos)
	}
	if sawRegister {
		t.Errorf("must NOT recommend `pi-stack mcp register` for a remote server, got %v", st.Todos)
	}
}

// TestStatus_RemoteRegistered_NoTodo: a registered remote server needs no fix.
func TestStatus_RemoteRegistered_NoTodo(t *testing.T) {
	cfg := &config.Config{MCP: []string{"notion"}}
	st := gatherStatus(cfg, "default", statusRemoteEnv("gog\nnotion\n"))
	for _, tdo := range st.Todos {
		if tdo == "pi-stack mcp bundle" || tdo == "pi-stack mcp register" {
			t.Errorf("a registered server should add no MCP registration todo, got %v", st.Todos)
		}
	}
}

// TestStatus_UnknownClassification_NoTodo: when local-vs-remote can't be
// established, status must not guess either registration command.
func TestStatus_UnknownClassification_NoTodo(t *testing.T) {
	cfg := &config.Config{MCP: []string{"mystery"}}
	env := shellEnv{
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		run: func(name string, args ...string) (string, error) {
			if name == "sbx" && len(args) >= 1 && args[0] == "secret" {
				return "anthropic\n", nil
			}
			if name == "sbx" && len(args) >= 2 && args[0] == "mcp" && args[1] == "ls" {
				return "gog\n", nil // mystery not registered
			}
			return "", nil
		},
		// hostBinary intentionally nil -> localMCPNames classification unknown.
		dial:     func(int) bool { return false },
		statFile: func(string) bool { return false },
	}
	st := gatherStatus(cfg, "default", env)
	for _, tdo := range st.Todos {
		if strings.Contains(tdo, "mcp register") || strings.Contains(tdo, "mcp bundle") {
			t.Errorf("an unknown-classification server must add no registration todo, got %v", st.Todos)
		}
	}
}
