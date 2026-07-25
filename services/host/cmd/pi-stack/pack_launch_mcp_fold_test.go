package main

import (
	"os"
	"path/filepath"
	"testing"

	"pi-stack/host/config"
)

// TestApplyPackToLaunch_FoldsAllIntegrationNamesIntoCfgMCP is the failing-first
// regression for the transient `run --pack` ship blocker: applyPackToLaunch
// already folded a STATIC (`static = true`) integration's mcp name into
// cfg.MCPStatic, but never folded ANY integration name (static or dynamic)
// into cfg.MCP itself. resolveStaticMCPForRun's eager set is computed from
// cfg.MCP (the "servers" list) intersected with cfg.MCPStatic (the eager
// override) -- a name that is only in MCPStatic and absent from cfg.MCP is
// never eager (it isn't even a candidate), so an inactive pack's static
// integration silently never got --static-mcp, and its dynamic integration
// was never discoverable at all (mcp-find only walks cfg.MCP). Both must be
// folded in memory (never saved -- the pack manifest is the source of truth,
// re-read every launch).
func TestApplyPackToLaunch_FoldsAllIntegrationNamesIntoCfgMCP(t *testing.T) {
	root := t.TempDir()
	mustWritePack(t, root, packManifest{
		Name:   "work",
		Schema: 1,
		Integrations: []packIntegration{
			{Name: "Fastmail", MCP: "fastmail", Static: true}, // eager
			{Name: "Notion", MCP: "notion"},                   // default dynamic
		},
	})

	cfg := &config.Config{} // no persistent MCP list at all
	o := runOpts{Pack: root}
	if _, err := applyPackToLaunch(cfg, &o, fakeGitEnv(nil)); err != nil {
		t.Fatalf("applyPackToLaunch: %v", err)
	}

	if !containsStr(cfg.MCP, "fastmail") {
		t.Errorf("cfg.MCP must gain the pack's static integration name, got %v", cfg.MCP)
	}
	if !containsStr(cfg.MCP, "notion") {
		t.Errorf("cfg.MCP must gain the pack's dynamic integration name too (discoverable via mcp-find), got %v", cfg.MCP)
	}
	if !containsStr(cfg.MCPStatic, "fastmail") {
		t.Errorf("cfg.MCPStatic must still carry the static override, got %v", cfg.MCPStatic)
	}
	if containsStr(cfg.MCPStatic, "notion") {
		t.Errorf("cfg.MCPStatic must NOT gain the dynamic integration, got %v", cfg.MCPStatic)
	}
}

// TestApplyPackToLaunch_MCPDynamicOverrideStillWins: a user's persistent
// mcp_dynamic pin still beats a pack's static=true declaration once both
// names are folded into cfg.MCP/cfg.MCPStatic.
func TestApplyPackToLaunch_MCPDynamicOverrideStillWins(t *testing.T) {
	root := t.TempDir()
	mustWritePack(t, root, packManifest{
		Name:   "work",
		Schema: 1,
		Integrations: []packIntegration{
			{Name: "Fastmail", MCP: "fastmail", Static: true},
		},
	})

	cfg := &config.Config{MCPDynamic: []string{"fastmail"}}
	o := runOpts{Pack: root}
	if _, err := applyPackToLaunch(cfg, &o, fakeGitEnv(nil)); err != nil {
		t.Fatalf("applyPackToLaunch: %v", err)
	}
	if got := resolveStaticMCPForRun(cfg.MCP, o.MCP, cfg); containsStr(got, "fastmail") {
		t.Errorf("mcp_dynamic must still override the pack's static=true, got eager set %v", got)
	}
}

// TestRun_InactivePackTransientArgv_StaticAndDynamicIntegrations is the
// end-to-end run/create argv test the ship blocker calls for: a PREVIOUSLY
// INACTIVE pack (cfg.Pack empty; only the transient `--pack PATH` override is
// set) with one static and one dynamic mcp integration must, on a create,
// reach sbx with --static-mcp for the static integration and NOT for the
// dynamic one -- while the on-disk config file is byte-for-byte unchanged
// (the fold is launch-local only, never persisted).
func TestRun_InactivePackTransientArgv_StaticAndDynamicIntegrations(t *testing.T) {
	root := t.TempDir()
	mustWritePack(t, root, packManifest{
		Name:   "work",
		Schema: 1,
		Integrations: []packIntegration{
			{Name: "Fastmail", MCP: "fastmail", Static: true},
			{Name: "Notion", MCP: "notion"},
		},
	})

	// A real config.toml on disk, pre-existing and with NO active pack and NO
	// mcp list -- exactly "a previously inactive --pack".
	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "config.toml")
	preExisting := "gog_account = \"me@example.com\"\n"
	if err := os.WriteFile(configPath, []byte(preExisting), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PI_STACK_CONFIG", configPath)

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Pack != "" {
		t.Fatalf("precondition: no active pack, got %q", cfg.Pack)
	}

	// Mirrors run.go's willCreate(...) sequence: applyPackToLaunch first, then
	// resolveStaticMCPForRun, then buildSbxArgs -- for a transient `--pack`
	// override (o.Pack set, cfg.Pack never touched).
	o := runOpts{Workspace: ".", Pack: root}
	if _, err := applyPackToLaunch(cfg, &o, fakeGitEnv(nil)); err != nil {
		t.Fatalf("applyPackToLaunch: %v", err)
	}
	o.StaticMCP = resolveStaticMCPForRun(cfg.MCP, o.MCP, cfg)
	args := buildSbxArgs(cfg, o, "0.0.99")

	if !contains(args, []string{"--static-mcp", "fastmail"}) {
		t.Errorf("expected --static-mcp fastmail in argv, got %v", args)
	}
	if contains(args, []string{"--static-mcp", "notion"}) {
		t.Errorf("notion is a dynamic integration; must not be emitted as --static-mcp, got %v", args)
	}
	// Still DISCOVERABLE (mcp-find/mcp-exec walk cfg.MCP): the whole point of
	// folding it in is that it isn't silently dropped from the launch's MCP set.
	if !containsStr(cfg.MCP, "notion") {
		t.Errorf("notion must remain in cfg.MCP (discoverable dynamically), got %v", cfg.MCP)
	}

	// The config file on disk must be COMPLETELY UNCHANGED -- this whole fold
	// is launch-local, never saved.
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading config file after launch: %v", err)
	}
	if string(after) != preExisting {
		t.Errorf("config.toml changed by a transient --pack launch:\nbefore: %q\nafter:  %q", preExisting, string(after))
	}
}
