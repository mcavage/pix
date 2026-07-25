package main

import (
	"strings"
	"testing"

	"pi-stack/host/config"
)

// status_mcp_custom_test.go covers the status-side half of the final
// custom-MCP false-green regression: a confirmed-unregistered custom server
// (mcpClassCustom -- non-local, outside the shipped catalog) must add an
// outstanding TODO so status cannot print "all systems go" over it. The
// guidance must be the exact native `sbx mcp add --help` command, never
// `pi-stack mcp bundle` (shipped catalog only) or `pi-stack mcp
// register` (local stdio only). A confirmed-REGISTERED custom server must
// add no todo at all.

// statusCustomEnv wires hostBinary + a local-name list that does NOT include
// "linear", so localMCPNames classifies linear as CONFIRMED CUSTOM (it is
// also not in mcpCatalogNames).
func statusCustomEnv(mcpLs string) shellEnv {
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
				return "slack", nil // linear is deliberately NOT in this list
			}
			return "", nil
		},
		hostBinary: func() (string, error) { return statusHostBinary, nil },
		dial:       func(int) bool { return false },
		statFile:   func(string) bool { return false },
	}
}

// TestStatus_CustomUnregistered_AddsOutstandingNativeGuidance: a confirmed
// custom server that `sbx mcp ls` shows is absent must add an outstanding
// TODO (so the verdict can't read "all systems go"), pointed at native
// `sbx mcp add --help`, never `pi-stack mcp bundle`/`pi-stack mcp register`,
// and never an invented URL.
func TestStatus_CustomUnregistered_AddsOutstandingNativeGuidance(t *testing.T) {
	cfg := &config.Config{MCP: []string{"linear"}}
	st := gatherStatus(cfg, "default", statusCustomEnv("gog\n")) // linear not registered
	if len(st.Todos) == 0 {
		t.Fatalf("expected an outstanding todo for a confirmed-unregistered custom server, got none")
	}
	var found string
	for _, tdo := range st.Todos {
		if tdo == "sbx mcp add --help" {
			found = tdo
		}
		if tdo == "pi-stack mcp bundle" {
			t.Errorf("must NOT recommend `pi-stack mcp bundle` for a custom server, got %v", st.Todos)
		}
		if tdo == "pi-stack mcp register" {
			t.Errorf("must NOT recommend `pi-stack mcp register` for a custom server, got %v", st.Todos)
		}
	}
	if found == "" {
		t.Fatalf("expected native add help todo, got %v", st.Todos)
	}
	if strings.Contains(found, "https://") || strings.Contains(found, "http://") {
		t.Errorf("must never invent a URL for a custom server, got %q", found)
	}
}

// TestStatus_CustomRegistered_NoTodo: a registered custom server needs no
// registration fix from status (auth/tool health is a doctor-only concern).
func TestStatus_CustomRegistered_NoTodo(t *testing.T) {
	cfg := &config.Config{MCP: []string{"linear"}}
	st := gatherStatus(cfg, "default", statusCustomEnv("gog\nlinear\n"))
	for _, tdo := range st.Todos {
		if tdo == "sbx mcp add --help" {
			t.Errorf("a registered custom server should add no todo, got %v", st.Todos)
		}
	}
}
