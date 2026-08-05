package main

import (
	"testing"
)

// TestBuiltinMcpServerFor proves the built-in switch fails CLOSED: the public
// tree ships NO built-in local stdio server, so every name errors — including
// the two that used to resolve here. Slack was externalized in W2/U02a (see
// docs/design/slack-setup.md) and the write-scoped `google-docs-create`
// companion retired with the built-in Google Workspace wizard in W2/U02B (see
// docs/design/gworkspace-externalization.md). Anything not served here is
// either a container the sbx gateway runs or a pack-admitted [[services]] unit.
func TestBuiltinMcpServerFor(t *testing.T) {
	for _, name := range []string{"nope", "slack", "google-docs-create", "gog"} {
		if _, err := builtinMcpServerFor(name); err == nil {
			t.Errorf("builtinMcpServerFor(%q) should error: this binary serves no built-in MCP server", name)
		}
	}
}

// TestBuiltinMcpNames proves the local-server source of truth: it is EMPTY
// (both former built-ins are externalized), gog is NEVER listed (external CLI,
// registered directly — see mcp.GogHardenedArgv), and the result is sorted.
func TestBuiltinMcpNames(t *testing.T) {
	names := builtinMcpNames()
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	if len(names) != 0 {
		t.Errorf("builtinMcpNames() = %v, want no built-ins", names)
	}
	for _, name := range []string{"gog", "slack", "google-docs-create"} {
		if got[name] {
			t.Errorf("builtinMcpNames() = %v, must NOT list %q", names, name)
		}
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Errorf("builtinMcpNames() = %v, want sorted", names)
		}
	}
}

// TestMcpServerForSelection proves the resolver mirrors the (now empty)
// built-in set: an unknown name errors and the cleanup func is always safe to
// call, including on the error path.
func TestMcpServerForSelection(t *testing.T) {
	for _, name := range []string{"nope", "google-docs-create", "slack"} {
		srv, cleanup, err := mcpServerFor(name)
		cleanup()
		if err == nil {
			t.Errorf("mcpServerFor(%q) should error: no built-in MCP servers remain (got %T)", name, srv)
		}
	}
}
