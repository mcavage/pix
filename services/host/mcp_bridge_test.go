package main

import (
	"testing"
)

// TestBuiltinMcpServerFor proves the built-in switch: the google-docs-create
// adapter resolves, and an unregistered name errors. Slack was the public
// tree's other built-in local server until it was externalized (W2/U02a; see
// docs/design/slack-setup.md) — anything not in this switch is either a
// container the sbx gateway runs or a [plugins.mcp] external-process override.
func TestBuiltinMcpServerFor(t *testing.T) {
	srv, err := builtinMcpServerFor(googleDocsCreateServerName)
	if err != nil {
		t.Fatalf("builtinMcpServerFor(%s) = %v, want the built-in adapter", googleDocsCreateServerName, err)
	}
	if _, ok := srv.(googleDocsCreateMcpAdapter); !ok {
		t.Errorf("builtinMcpServerFor(%s) returned %T, want googleDocsCreateMcpAdapter", googleDocsCreateServerName, srv)
	}

	// An unknown name errors — including the retired "slack" name, which is no
	// longer a built-in.
	for _, name := range []string{"nope", "slack"} {
		if _, err := builtinMcpServerFor(name); err == nil {
			t.Errorf("builtinMcpServerFor(%q) should error for an unregistered name", name)
		}
	}
}

// TestBuiltinMcpNames proves the local-server source of truth: the
// create-only Docs adapter is listed, gog is NEVER listed (external CLI), and
// the result is sorted.
func TestBuiltinMcpNames(t *testing.T) {
	names := builtinMcpNames()
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	if !got[googleDocsCreateServerName] {
		t.Errorf("builtinMcpNames() = %v, want %s present", names, googleDocsCreateServerName)
	}
	if len(names) != 1 {
		t.Errorf("builtinMcpNames() = %v, want exactly one built-in", names)
	}
	if got["gog"] || got["slack"] {
		t.Errorf("builtinMcpNames() = %v, must NOT list gog or slack", names)
	}
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Errorf("builtinMcpNames() = %v, want sorted", names)
		}
	}
}

func TestMcpServerForSelection(t *testing.T) {
	srv, cleanup, err := mcpServerFor(googleDocsCreateServerName)
	if err != nil {
		t.Fatalf("mcpServerFor(%s) = %v, want built-in adapter", googleDocsCreateServerName, err)
	}
	cleanup()
	if _, ok := srv.(googleDocsCreateMcpAdapter); !ok {
		t.Errorf("mcpServerFor(%s) returned %T, want the built-in adapter", googleDocsCreateServerName, srv)
	}
	_, cleanup, err = mcpServerFor("nope")
	cleanup()
	if err == nil {
		t.Fatal("mcpServerFor on an unknown name should error")
	}
}
