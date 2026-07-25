package main

import (
	"encoding/json"
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

// TestDoctor_MCPCustom_UnverifiableNoBundleTodo: a confirmed non-local,
// non-catalog server (linear) must never recommend `pi-stack mcp bundle`
// (broken -- the bundle doesn't carry it) nor `pi-stack mcp register` (that's
// for local stdio servers); it renders unverifiable with no repair command,
// while a real catalog name (notion) in the same run still gets the remote
// bundle guidance.
func TestDoctor_MCPCustom_UnverifiableNoBundleTodo(t *testing.T) {
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
	if linear.evidence != EvidenceUnverifiable {
		t.Errorf("linear (custom, non-catalog) must be unverifiable, got %+v", linear)
	}
	if linear.todo != "" {
		t.Errorf("linear must carry no repair command (bundle would be broken for it), got %q", linear.todo)
	}

	notion := findCheck(r, "Other MCP servers", "notion")
	if notion == nil || notion.todo != "pi-stack mcp bundle" {
		t.Fatalf("notion (real catalog name) must still get the remote bundle guidance, got %+v", notion)
	}
}

// TestStatus_MCPCustom_NoBundleTodo mirrors the doctor test on the status
// side: an unregistered custom (non-catalog) server must not add the broken
// `pi-stack mcp bundle` todo, while a real catalog name still does.
func TestStatus_MCPCustom_NoBundleTodo(t *testing.T) {
	cfg := &config.Config{MCP: []string{"linear", "notion"}}
	st := gatherStatus(cfg, "default", statusRemoteEnv("gog\n")) // neither registered
	var sawBundle, sawRegister int
	for _, tdo := range st.Todos {
		if tdo == "pi-stack mcp bundle" {
			sawBundle++
		}
		if tdo == "pi-stack mcp register" {
			sawRegister++
		}
	}
	if sawBundle != 1 {
		t.Errorf("expected exactly one deduped `pi-stack mcp bundle` todo (for notion only), got %d in %v", sawBundle, st.Todos)
	}
	if sawRegister != 0 {
		t.Errorf("must never recommend `pi-stack mcp register` here, got %v", st.Todos)
	}
}
