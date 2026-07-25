// mcp_reattach_honesty_test.go; product gap #2: reattach honesty. On a
// RE-ATTACH (existing running or stopped sandbox, not --replace), `pi-stack
// run` must compare the DESIRED MCP universe for THIS invocation (cfg.MCP +
// the active/transient pack's integrations + explicit --mcp, deduped via
// allPreloadedMCP) against the launcher's own per-sandbox receipt
// (Preloaded/Loads) and warn, BEFORE reattaching, about any desired name the
// receipt cannot prove is attached. It must never auto-load, and a
// receipt-only historical name (one the receipt lists that is no longer
// desired) must never be mentioned.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pi-stack/host/config"
)

// TestMcpReattachWarning_CfgChangeWarns: a server added to cfg.MCP since this
// sandbox's create is not in the receipt -> warn, naming it and both fix
// paths (live load, recreate).
func TestMcpReattachWarning_CfgChangeWarns(t *testing.T) {
	sd := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return sd, nil })
	if err := writeCreateReceipt(sd, "pi-stack-t", "", []string{"slack"}, fixedClock("2024-01-01T00:00:00Z")); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{MCP: []string{"slack", "notion"}} // notion added since create
	o := runOpts{Workspace: "/repo", Name: "pi-stack-t"}
	msg := mcpReattachWarning(cfg, o, true)
	if msg == "" {
		t.Fatal("expected a warning when a configured server is not in the receipt")
	}
	if !strings.Contains(msg, "notion") {
		t.Errorf("warning should name the unattached server, got: %q", msg)
	}
	if strings.Contains(msg, "slack") {
		t.Errorf("an already-attached server must not be named, got: %q", msg)
	}
	if !strings.Contains(msg, "pi-stack mcp load notion") {
		t.Errorf("warning should offer the exact live-attach command, got: %q", msg)
	}
	if !strings.Contains(msg, "--replace") {
		t.Errorf("warning should offer the recreate path, got: %q", msg)
	}
}

// TestMcpReattachWarning_PackChangeWarns: the active pack declares an
// integration server that predates the receipt (a pack switched since
// create) -> warn, naming the pack's server.
func TestMcpReattachWarning_PackChangeWarns(t *testing.T) {
	sd := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return sd, nil })
	if err := writeCreateReceipt(sd, "pi-stack-t", "", []string{"slack"}, fixedClock("2024-01-01T00:00:00Z")); err != nil {
		t.Fatal(err)
	}

	packRoot := t.TempDir()
	toml := "name = \"work\"\nschema = 1\n\n[[integrations]]\nname = \"BambooHR\"\nmcp  = \"bamboohr\"\n"
	if err := os.WriteFile(filepath.Join(packRoot, "pack.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{MCP: []string{"slack"}, Pack: packRoot}
	o := runOpts{Workspace: "/repo", Name: "pi-stack-t"}
	msg := mcpReattachWarning(cfg, o, true)
	if msg == "" {
		t.Fatal("expected a warning when the active pack's integration server is not in the receipt")
	}
	if !strings.Contains(msg, "bamboohr") {
		t.Errorf("warning should name the pack's unattached server, got: %q", msg)
	}
}

// TestMcpReattachWarning_ExplicitMCPWarns: an explicit --mcp name not in the
// receipt warns exactly like a config-declared one.
func TestMcpReattachWarning_ExplicitMCPWarns(t *testing.T) {
	sd := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return sd, nil })
	if err := writeCreateReceipt(sd, "pi-stack-t", "", nil, fixedClock("2024-01-01T00:00:00Z")); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	o := runOpts{Workspace: "/repo", Name: "pi-stack-t", MCP: []string{"extra"}}
	msg := mcpReattachWarning(cfg, o, true)
	if msg == "" || !strings.Contains(msg, "extra") {
		t.Errorf("expected a warning naming the explicit --mcp server, got: %q", msg)
	}
}

// TestMcpReattachWarning_AllAttachedSilent: every desired name is a positive
// receipt claim (preloaded or loaded) -> no warning at all.
func TestMcpReattachWarning_AllAttachedSilent(t *testing.T) {
	sd := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return sd, nil })
	if err := writeCreateReceipt(sd, "pi-stack-t", "", []string{"slack"}, fixedClock("2024-01-01T00:00:00Z")); err != nil {
		t.Fatal(err)
	}
	if err := appendLoadReceipt(sd, "pi-stack-t", "gog", fixedClock("2024-01-01T00:00:00Z")); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{MCP: []string{"slack", "gog"}}
	o := runOpts{Workspace: "/repo", Name: "pi-stack-t"}
	if msg := mcpReattachWarning(cfg, o, true); msg != "" {
		t.Errorf("expected silence when every desired server is receipted, got: %q", msg)
	}
}

// TestMcpReattachWarning_ReceiptOnlyHistoricalNameSilent: the receipt lists a
// server that is no longer desired (dropped from config); that is
// legitimate history, not a gap, and must never be mentioned.
func TestMcpReattachWarning_ReceiptOnlyHistoricalNameSilent(t *testing.T) {
	sd := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return sd, nil })
	if err := writeCreateReceipt(sd, "pi-stack-t", "", []string{"slack", "old-server"}, fixedClock("2024-01-01T00:00:00Z")); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{MCP: []string{"slack"}} // old-server no longer configured
	o := runOpts{Workspace: "/repo", Name: "pi-stack-t"}
	if msg := mcpReattachWarning(cfg, o, true); msg != "" {
		t.Errorf("a receipt-only historical name must never trigger or appear in a warning, got: %q", msg)
	}
}

// TestMcpReattachWarning_AbsentReceipt: no receipt at all for this sandbox
// (predates the feature, or never receipted) -> attachment cannot be
// verified for the desired set, both fix paths offered.
func TestMcpReattachWarning_AbsentReceipt(t *testing.T) {
	sd := t.TempDir() // never written to
	withSandboxMCPStateDirFn(t, func() (string, error) { return sd, nil })

	cfg := &config.Config{MCP: []string{"gog"}}
	o := runOpts{Workspace: "/repo", Name: "pi-stack-t"}
	msg := mcpReattachWarning(cfg, o, true)
	if msg == "" {
		t.Fatal("expected a warning with no receipt at all")
	}
	if !strings.Contains(msg, "cannot be verified") {
		t.Errorf("expected an honest cannot-be-verified message, got: %q", msg)
	}
	if !strings.Contains(msg, "gog") {
		t.Errorf("expected the desired name named, got: %q", msg)
	}
	if !strings.Contains(msg, "pi-stack mcp load gog") || !strings.Contains(msg, "--replace") {
		t.Errorf("expected both fix paths offered, got: %q", msg)
	}
}

// TestMcpReattachWarning_CorruptReceipt: a receipt file exists but is not
// valid JSON for this sandbox -> unverifiable, same honest message + both
// fix paths, never silently trusted.
func TestMcpReattachWarning_CorruptReceipt(t *testing.T) {
	sd := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return sd, nil })
	dir := filepath.Join(sd, "sandboxes", "pi-stack-t")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{MCP: []string{"gog"}}
	o := runOpts{Workspace: "/repo", Name: "pi-stack-t"}
	msg := mcpReattachWarning(cfg, o, true)
	if msg == "" || !strings.Contains(msg, "cannot be verified") {
		t.Errorf("expected a cannot-be-verified warning for a corrupt receipt, got: %q", msg)
	}
	if !strings.Contains(msg, "gog") {
		t.Errorf("expected the desired name named, got: %q", msg)
	}
}

// TestMcpReattachWarning_SilentOnCreateOrReplace: never fires on a fresh
// create (reattaching=false) or a --replace (recreates fresh, so a stale
// receipt is irrelevant).
func TestMcpReattachWarning_SilentOnCreateOrReplace(t *testing.T) {
	sd := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return sd, nil })
	cfg := &config.Config{MCP: []string{"gog"}}
	o := runOpts{Workspace: "/repo", Name: "pi-stack-t"}

	if msg := mcpReattachWarning(cfg, o, false); msg != "" {
		t.Errorf("must not warn on a fresh create, got: %q", msg)
	}
	o.Replace = true
	if msg := mcpReattachWarning(cfg, o, true); msg != "" {
		t.Errorf("must not warn on --replace, got: %q", msg)
	}
}

// TestMcpReattachWarning_NoDesiredServersSilent: nothing configured/desired
// at all -> nothing to check, silent regardless of receipt state.
func TestMcpReattachWarning_NoDesiredServersSilent(t *testing.T) {
	sd := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return sd, nil })
	cfg := &config.Config{}
	o := runOpts{Workspace: "/repo", Name: "pi-stack-t"}
	if msg := mcpReattachWarning(cfg, o, true); msg != "" {
		t.Errorf("no desired MCP servers should never warn, got: %q", msg)
	}
}

// TestMcpReattachWarning_FiresOnBothRunningAndStopped: the warning is keyed
// on `reattaching` (the same signal planSandboxLaunch resolves for BOTH a
// running and a stopped sandbox; see TestPlanSandboxLaunch_RunningReattaches
// / _StoppedReattaches), so it must fire identically whichever state
// produced that reattach decision.
func TestMcpReattachWarning_FiresOnBothRunningAndStopped(t *testing.T) {
	sd := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return sd, nil })
	if err := writeCreateReceipt(sd, "pi-stack-t", "", nil, fixedClock("2024-01-01T00:00:00Z")); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{MCP: []string{"gog"}}
	o := runOpts{Workspace: "/repo", Name: "pi-stack-t"}

	for _, state := range []sbxState{sbxRunning, sbxStopped} {
		plan := planSandboxLaunch(state, false, cfg, o, "0.0.99")
		if !plan.Reattach {
			t.Fatalf("expected %v to reattach", state)
		}
		if msg := mcpReattachWarning(cfg, o, plan.Reattach); msg == "" {
			t.Errorf("expected a warning reattaching from state %v", state)
		}
	}
}

// TestMcpReattachWarning_NeverAutoLoads: calling mcpReattachWarning must never
// itself execute `pi-stack mcp load` or write a load receipt; it is
// read-only reporting. Proven by asserting the receipt is byte-for-byte
// unchanged after the call.
func TestMcpReattachWarning_NeverAutoLoads(t *testing.T) {
	sd := t.TempDir()
	withSandboxMCPStateDirFn(t, func() (string, error) { return sd, nil })
	if err := writeCreateReceipt(sd, "pi-stack-t", "", []string{"slack"}, fixedClock("2024-01-01T00:00:00Z")); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(sd, "sandboxes", "pi-stack-t", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{MCP: []string{"slack", "notion"}}
	o := runOpts{Workspace: "/repo", Name: "pi-stack-t"}
	_ = mcpReattachWarning(cfg, o, true)

	after, err := os.ReadFile(filepath.Join(sd, "sandboxes", "pi-stack-t", "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("mcpReattachWarning must never mutate the receipt (no auto-load); before=%q after=%q", before, after)
	}
}
