package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pi-stack/host/config"
)

// status_pack_static_test.go covers finding #2 (ship-review): status must
// fold the ACTIVE PACK's eager (`static = true`) MCP integrations into its
// attach-on-run rendering, exactly as applyPackToLaunch does at launch time
// -- WITHOUT mutating cfg or triggering any of applyPackToLaunch's host side
// effects (no skills mount, no kit synth, no credential-missing warning).

// writeStaticPack scaffolds a minimal pack.toml declaring one static and one
// default (dynamic) MCP integration, returning the pack root.
func writeStaticPack(t *testing.T, staticName, dynamicName string) string {
	t.Helper()
	root := t.TempDir()
	toml := `name = "test-pack"
schema = 1

[[integrations]]
name = "` + staticName + `"
mcp  = "` + staticName + `"
static = true

[[integrations]]
name = "` + dynamicName + `"
mcp  = "` + dynamicName + `"
`
	if err := os.WriteFile(filepath.Join(root, "pack.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestStatusResolveStaticMCP_PackStaticIntegration: a pack integration
// declared `static = true` renders attach-on-run even though it is NOT in
// cfg.MCPStatic itself.
func TestStatusResolveStaticMCP_PackStaticIntegration(t *testing.T) {
	root := writeStaticPack(t, "fastmail", "notion")
	cfg := &config.Config{Pack: root, MCP: []string{"fastmail", "notion"}}
	got := statusResolveStaticMCP(cfg, cfg.MCP)
	if !containsStr(got, "fastmail") {
		t.Errorf("expected fastmail (pack static=true) to be eager, got %v", got)
	}
}

// TestStatusResolveStaticMCP_PackDefaultDynamic: a pack integration WITHOUT
// static=true stays dynamic (not in the eager set).
func TestStatusResolveStaticMCP_PackDefaultDynamic(t *testing.T) {
	root := writeStaticPack(t, "fastmail", "notion")
	cfg := &config.Config{Pack: root, MCP: []string{"fastmail", "notion"}}
	got := statusResolveStaticMCP(cfg, cfg.MCP)
	if containsStr(got, "notion") {
		t.Errorf("a default (non-static) pack integration must stay dynamic, got %v", got)
	}
}

// TestStatusResolveStaticMCP_UserDynamicOverridesPackStatic: mcp_dynamic wins
// over a pack's static=true, same precedence resolveStaticMCP already applies
// for cfg.MCPStatic.
func TestStatusResolveStaticMCP_UserDynamicOverridesPackStatic(t *testing.T) {
	root := writeStaticPack(t, "fastmail", "notion")
	cfg := &config.Config{Pack: root, MCP: []string{"fastmail", "notion"}, MCPDynamic: []string{"fastmail"}}
	got := statusResolveStaticMCP(cfg, cfg.MCP)
	if containsStr(got, "fastmail") {
		t.Errorf("mcp_dynamic must override a pack's static=true, got %v", got)
	}
}

// TestStatusResolveStaticMCP_BrokenPackDegradesHonestly: an unreadable/broken
// active pack must not error and must not falsely claim eager -- it degrades
// to cfg's own mcp_static/mcp_dynamic only.
func TestStatusResolveStaticMCP_BrokenPackDegradesHonestly(t *testing.T) {
	cfg := &config.Config{Pack: filepath.Join(t.TempDir(), "does-not-exist"), MCP: []string{"fastmail"}}
	got := statusResolveStaticMCP(cfg, cfg.MCP)
	if containsStr(got, "fastmail") {
		t.Errorf("a broken/absent active pack must never falsely claim eager, got %v", got)
	}
}

// TestStatusResolveStaticMCP_DoesNotMutateCfg: cfg.MCPStatic must be
// byte-for-byte unchanged after the call -- the fold happens on a shallow
// COPY, never the real cfg.MCPStatic slice (no host side effects, no launch
// triggered).
func TestStatusResolveStaticMCP_DoesNotMutateCfg(t *testing.T) {
	root := writeStaticPack(t, "fastmail", "notion")
	cfg := &config.Config{Pack: root, MCP: []string{"fastmail", "notion"}}
	before := append([]string(nil), cfg.MCPStatic...)
	_ = statusResolveStaticMCP(cfg, cfg.MCP)
	if len(cfg.MCPStatic) != len(before) {
		t.Fatalf("cfg.MCPStatic must not be mutated, got %v (was %v)", cfg.MCPStatic, before)
	}
	for i := range before {
		if cfg.MCPStatic[i] != before[i] {
			t.Fatalf("cfg.MCPStatic must not be mutated, got %v (was %v)", cfg.MCPStatic, before)
		}
	}
}

// TestGatherStatus_PackStaticIntegration_HumanAndJSON: gatherStatus/render
// surface the pack-static attach state end to end, in both human and JSON
// output.
func TestGatherStatus_PackStaticIntegration_HumanAndJSON(t *testing.T) {
	root := writeStaticPack(t, "fastmail", "notion")
	cfg := &config.Config{Pack: root, MCP: []string{"fastmail", "notion"}}
	env := shellEnv{
		lookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
		run: func(name string, args ...string) (string, error) {
			if name == "sbx" && len(args) >= 1 && args[0] == "secret" {
				return "anthropic\n", nil
			}
			if name == "sbx" && len(args) >= 2 && args[0] == "mcp" && args[1] == "ls" {
				return "fastmail\nnotion\n", nil
			}
			return "", nil
		},
		dial:     func(int) bool { return false },
		statFile: func(string) bool { return false },
	}

	st := gatherStatus(cfg, "default", env)
	byName := map[string]mcpStatusLine{}
	for _, m := range st.MCPServers {
		byName[m.Name] = m
	}
	if !byName["fastmail"].Attach {
		t.Errorf("fastmail (pack static=true) should be attach-on-run: %+v", st.MCPServers)
	}
	if byName["notion"].Attach {
		t.Errorf("notion (pack default) should stay dynamic: %+v", st.MCPServers)
	}

	var human bytes.Buffer
	renderStatus(cfg, "default", env, &human, false)
	s := human.String()
	if !strings.Contains(s, "attach-on-run") {
		t.Errorf("expected attach-on-run in human render for the pack-static server, got:\n%s", s)
	}

	var jsonOut bytes.Buffer
	renderStatus(cfg, "default", env, &jsonOut, true)
	var parsed statusReport
	if err := json.Unmarshal(jsonOut.Bytes(), &parsed); err != nil {
		t.Fatalf("status --json invalid: %v\n%s", err, jsonOut.String())
	}
	jByName := map[string]mcpStatusLine{}
	for _, m := range parsed.MCPServers {
		jByName[m.Name] = m
	}
	if !jByName["fastmail"].Attach {
		t.Errorf("json fastmail should be attach-on-run: %+v", parsed.MCPServers)
	}
	if jByName["notion"].Attach {
		t.Errorf("json notion should stay dynamic: %+v", parsed.MCPServers)
	}
}
