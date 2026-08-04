package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/plugin"
)

// buildExampleMcp compiles examples/mcp-example to a temp binary and returns its
// path + sha256 — the artifact a pack would ship and pin in a [[services]]
// entry (the [plugins.mcp] config declaration is retired and inert, U07d).
func buildExampleMcp(t *testing.T) (bin, sha string) {
	t.Helper()
	bin = filepath.Join(t.TempDir(), "mcp-example")
	out, err := exec.Command("go", "build", "-o", bin, "./examples/mcp-example").CombinedOutput()
	if err != nil {
		t.Fatalf("go build mcp-example failed: %v\n%s", err, out)
	}
	b, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return bin, hex.EncodeToString(sum[:])
}

// TestExternalMcpOverrideEndToEnd proves the external-MCP-unit MECHANISM (the
// machinery a pack-admitted [[services]] unit of kind "mcp" runs on): an
// external MCP binary is sha-verified, launched by the supervisor as a real
// out-of-process go-plugin subprocess with the "mcp" capability, dispensed over
// net/rpc, and its Info / ListTools / CallTool round-trip. Then pluginMcpServer
// (the adapter for a dispensed external unit) is shown to proxy to that
// same dispensed client — exactly what runMcpBridge serves over stdio.
func TestExternalMcpOverrideEndToEnd(t *testing.T) {
	bin, sha := buildExampleMcp(t)

	spec := config.PluginSpec{Impl: "example", Path: bin, SHA: sha}

	sup := &supervisor{}
	defer sup.shutdown()
	h, err := sup.launch("example", "mcp", spec, "", nil)
	if err != nil {
		t.Fatalf("launch external mcp plugin: %v", err)
	}

	srv, ok := h.Get().(plugin.McpServer)
	if !ok || srv == nil {
		t.Fatalf("dispensed impl is not an McpServer: %T", h.Get())
	}

	// Wrap it exactly as mcpServerFor does for an override, and drive the bridge
	// surface through the wrapper so the whole runMcpBridge path is exercised.
	wrapped := &pluginMcpServer{h: h}

	info, err := wrapped.Info()
	if err != nil {
		t.Fatalf("Info over RPC: %v", err)
	}
	if info.Name != "example-mcp" || info.ProtocolVersion != "2025-06-18" {
		t.Errorf("Info = %+v, want Name=example-mcp ProtocolVersion=2025-06-18", info)
	}

	tools, err := wrapped.ListTools()
	if err != nil {
		t.Fatalf("ListTools over RPC: %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("ListTools = %+v, want a single echo tool", tools)
	}

	out, err := wrapped.CallTool("echo", json.RawMessage(`{"msg":"hi"}`))
	if err != nil {
		t.Fatalf("CallTool over RPC: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("CallTool result not JSON: %v (%s)", err, out)
	}
	if got["echoed"] != true {
		t.Errorf("CallTool = %v, want {echoed:true}", got)
	}

	// bridgeFromMcpServer builds the dispatcher runMcpBridge serves; prove the
	// override flows all the way into a usable tool table.
	name, bt, handlers, err := bridgeFromMcpServer(wrapped)
	if err != nil {
		t.Fatalf("bridgeFromMcpServer: %v", err)
	}
	if name != "example-mcp" || len(bt) != 1 || handlers["echo"] == nil {
		t.Errorf("bridge = (%q, %d tools, echo=%v), want example-mcp / 1 / non-nil", name, len(bt), handlers["echo"] != nil)
	}
}

// TestMcpServerForConfigPluginsInert proves U07d: the [plugins.mcp] config
// override is RETIRED. A config.toml that names an external binary (pinned and
// otherwise launchable) is INERT — the slot loads as builtin with no path/sha,
// the declared key surfaces through RetiredKeys (the operator-facing notice),
// and mcpServerFor("slack") returns the in-process built-in adapter. No
// subprocess is ever launched from a config declaration; the only admission
// path for an external unit is a pack-trust-admitted [[services]] entry.
func TestMcpServerForConfigPluginsInert(t *testing.T) {
	bin, sha := buildExampleMcp(t)

	// The exact config an operator used to write to override the MCP bridge.
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	raw := "[plugins.mcp]\nimpl = \"example\"\npath = \"" + bin + "\"\nsha = \"" + sha + "\"\n"
	if err := os.WriteFile(cfgPath, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIX_CONFIG", cfgPath)

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if spec := cfg.Plugin("mcp"); spec.Impl != config.BuiltinImpl || spec.Path != "" {
		t.Fatalf("Plugin(mcp) = %+v, want inert builtin (plugins.* is retired)", spec)
	}
	found := false
	for _, k := range cfg.RetiredKeys() {
		if k == "plugins.mcp" {
			found = true
		}
	}
	if !found {
		t.Errorf("RetiredKeys() = %v, want it to include plugins.mcp (the inert notice)", cfg.RetiredKeys())
	}

	srv, cleanup, err := mcpServerFor("slack")
	if err != nil {
		t.Fatalf("mcpServerFor(slack) = %v", err)
	}
	defer cleanup()
	if _, ok := srv.(*pluginMcpServer); ok {
		t.Fatal("mcpServerFor(slack) launched an external plugin from config; [plugins.mcp] must be inert")
	}
	if _, ok := srv.(slackMcpAdapter); !ok {
		t.Fatalf("mcpServerFor(slack) returned %T, want the built-in slackMcpAdapter", srv)
	}
}

// TestExternalMcpRefusesUnpinned proves F-C: an external plugin with no pinned
// sha in config is REFUSED at launch (never exec'd). External plugins must be
// sha-pinned; an empty sha is a hard failure, not a warning.
func TestExternalMcpRefusesUnpinned(t *testing.T) {
	bin, _ := buildExampleMcp(t)

	if err := verifyPluginSHA(config.PluginSpec{Impl: "example", Path: bin}); err == nil {
		t.Fatal("verifyPluginSHA with an empty sha should refuse, got nil error")
	} else if !strings.Contains(err.Error(), "unpinned") {
		t.Errorf("expected an unpinned-refusal error, got %v", err)
	}

	sup := &supervisor{}
	defer sup.shutdown()
	h, err := sup.launch("example", "mcp", config.PluginSpec{Impl: "example", Path: bin}, "", nil)
	if err == nil {
		t.Fatal("launch with an empty sha should refuse, got nil error")
	}
	if h != nil {
		t.Errorf("launch should not return a holder on unpinned refusal, got %v", h)
	}
}

// TestExternalMcpRefusesOnSHAMismatch proves the pinned-checksum gate for the MCP
// slot: the same real example binary is refused at launch when config pins the
// wrong sha, so no subprocess is ever spawned.
func TestExternalMcpRefusesOnSHAMismatch(t *testing.T) {
	bin, sha := buildExampleMcp(t)

	// Flip the last hex nibble to guarantee a mismatch against the real binary.
	bad := sha[:len(sha)-1] + map[bool]string{true: "0", false: "1"}[sha[len(sha)-1] != '0']

	sup := &supervisor{}
	defer sup.shutdown()
	h, err := sup.launch("example", "mcp", config.PluginSpec{Impl: "example", Path: bin, SHA: bad}, "", nil)
	if err == nil {
		t.Fatal("launch with a mismatched sha should refuse, got nil error")
	}
	if h != nil {
		t.Errorf("launch should not return a holder on sha refusal, got %v", h)
	}
	if !strings.Contains(err.Error(), "mismatch") {
		t.Errorf("expected a sha256 mismatch error, got %v", err)
	}
}
