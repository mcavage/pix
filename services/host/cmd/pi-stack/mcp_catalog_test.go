package main

import (
	"encoding/json"
	"strings"
	"testing"

	"pi-stack/host/config"
)

// mcp_catalog_test.go is the anti-drift guard for ship blocker #2: there must
// be ONE public catalog name set (mcpCatalogNames) matching the servers
// config/mcp-catalog.bundle.json actually ships (notion/atlassian/granola),
// reused by BOTH onboarding's allowlist validation and classifyMCP's
// remote-vs-custom split, so a name like "linear" that is NOT in the shipped
// bundle can never be misclassified as a confirmed remote-catalog server
// (which would recommend the broken `pi-stack mcp bundle` repair -- that
// command only ever registers what's actually in the bundle).

// TestMCPCatalogNames_MatchesShippedBundle parses config/mcp-catalog.bundle.json
// itself (never a second hand-copied literal) and fails if mcpCatalogNames
// drifts from it in either direction.
func TestMCPCatalogNames_MatchesShippedBundle(t *testing.T) {
	page := readRepoFile(t, "config/mcp-catalog.bundle.json")
	var entries []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(page), &entries); err != nil {
		t.Fatalf("parsing config/mcp-catalog.bundle.json: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("config/mcp-catalog.bundle.json parsed to zero entries")
	}
	fromBundle := map[string]bool{}
	for _, e := range entries {
		fromBundle[e.Name] = true
	}
	for name := range fromBundle {
		if !mcpCatalogNames[name] {
			t.Errorf("mcpCatalogNames is missing %q, present in the shipped bundle", name)
		}
	}
	for name := range mcpCatalogNames {
		if !fromBundle[name] {
			t.Errorf("mcpCatalogNames has %q, absent from the shipped bundle (config/mcp-catalog.bundle.json)", name)
		}
	}
}

// TestMCPCatalogNames_LinearNotIncluded pins the concrete regression: "linear"
// is a plausible-looking gateway-catalog name but ships in NEITHER the bundle
// nor mcpCatalogNames.
func TestMCPCatalogNames_LinearNotIncluded(t *testing.T) {
	if mcpCatalogNames["linear"] {
		t.Error(`mcpCatalogNames must not include "linear" -- it is not in the shipped bundle`)
	}
}

// TestClassifyMCP_CatalogNameIsRemote_NonCatalogIsCustom: a confirmed
// non-local name IN the catalog (notion) classifies as mcpClassRemote; a
// confirmed non-local name OUTSIDE the catalog (linear) classifies as the
// distinct mcpClassCustom, never mcpClassRemote (which would recommend the
// broken `pi-stack mcp bundle` repair for a name the bundle doesn't carry).
func TestClassifyMCP_CatalogNameIsRemote_NonCatalogIsCustom(t *testing.T) {
	localSet := map[string]bool{"slack": true}
	if got := classifyMCP("notion", localSet, true); got != mcpClassRemote {
		t.Errorf("classifyMCP(notion) = %v, want mcpClassRemote", got)
	}
	if got := classifyMCP("linear", localSet, true); got != mcpClassCustom {
		t.Errorf("classifyMCP(linear) = %v, want mcpClassCustom", got)
	}
	if got := classifyMCP("slack", localSet, true); got != mcpClassLocal {
		t.Errorf("classifyMCP(slack) = %v, want mcpClassLocal", got)
	}
	if got := classifyMCP("anything", localSet, false); got != mcpClassUnknown {
		t.Errorf("classifyMCP with localKnown=false = %v, want mcpClassUnknown", got)
	}
}

// TestDoctor_MCPCustom_ConfirmedAbsentIsFailureNoBundleTodo: a confirmed
// non-local, non-catalog server (linear) must never recommend `pi-stack mcp
// bundle` (broken -- the bundle doesn't carry it) nor `pi-stack mcp register`
// (that's for local stdio servers). The FINAL false-green regression this
// guards: a confirmed-ABSENT custom server (sbx mcp ls plainly doesn't list
// it) must be a VERIFIED failure with exact native `sbx mcp add --help` guidance, not a
// silent unverifiable that lets doctor claim a clean bill of health -- while
// a real catalog name (notion) in the same run still gets the remote bundle
// guidance.
func TestDoctor_MCPCustom_ConfirmedAbsentIsFailureNoBundleTodo(t *testing.T) {
	cfg := defaultCfg()
	cfg.MCP = []string{"linear", "notion"}
	f := remoteEnv(fakeEnv{
		present: map[string]bool{"sbx": true, "ollama": true},
		output: map[string]string{
			"sbx secret ls": "anthropic openai google github",
			"ollama list":   "gemma4:latest\nnomic-embed-text:latest\n",
			"sbx mcp ls":    "gog\n", // neither registered
		},
		ports: map[int]bool{11434: true, 11435: true},
	})
	r := runDoctor(cfg, f.env())

	linear := findCheck(r, "Other MCP servers", "linear")
	if linear == nil {
		t.Fatalf("expected a linear check, groups=%+v", r.groups)
	}
	if linear.evidence != EvidenceFailed {
		t.Errorf("a confirmed-absent custom server must be a VERIFIED failure (no false-green), got %+v", linear)
	}
	if linear.todo != "sbx mcp add --help" {
		t.Errorf("linear must carry the exact native help command, got %q", linear.todo)
	}
	if strings.Contains(linear.todo, "pi-stack mcp bundle") || strings.Contains(linear.todo, "pi-stack mcp register") {
		t.Errorf("linear must never carry the broken bundle/register repair, got %q", linear.todo)
	}

	notion := findCheck(r, "Other MCP servers", "notion")
	if notion == nil || notion.todo != "pi-stack mcp bundle" {
		t.Fatalf("notion (real catalog name) must still get the remote bundle guidance, got %+v", notion)
	}
}

// TestStatus_MCPCustom_NoBundleTodo mirrors the doctor test on the status
// side: an unregistered custom (non-catalog) server must not add the broken
// `pi-stack mcp bundle` todo (a real catalog name still does), but it DOES
// add its own native `sbx mcp add --help` outstanding item -- the final false-green
// regression: status must not read "all systems go" over a confirmed-missing
// custom server just because neither existing repair command applies to it.
func TestStatus_MCPCustom_NoBundleTodo(t *testing.T) {
	cfg := &config.Config{MCP: []string{"linear", "notion"}}
	st := gatherStatus(cfg, "default", statusRemoteEnv("gog\n")) // neither registered
	var sawBundle, sawRegister, sawLinear int
	for _, tdo := range st.Todos {
		if tdo == "pi-stack mcp bundle" {
			sawBundle++
		}
		if tdo == "pi-stack mcp register" {
			sawRegister++
		}
		if tdo == "sbx mcp add --help" {
			sawLinear++
		}
	}
	if sawBundle != 1 {
		t.Errorf("expected exactly one deduped `pi-stack mcp bundle` todo (for notion only), got %d in %v", sawBundle, st.Todos)
	}
	if sawRegister != 0 {
		t.Errorf("must never recommend `pi-stack mcp register` here, got %v", st.Todos)
	}
	if sawLinear != 1 {
		t.Errorf("expected exactly one native add-help todo, got %d in %v", sawLinear, st.Todos)
	}
}
