// facets_test.go — the fail-closed load-time validation, tested where it lives.
// These cases moved here with validatePackFacets: they assert that a manifest
// naming an unsafe path, an intermediate symlink, an undeclared setup hook or an
// ambiguous integration is REFUSED at load, which is a packinfo property, not a
// trust-gate one.
package packinfo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackSetupValidationRejectsUnsafeOrNonExecutableHooks(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "hook"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, step := range []SetupStep{
		{ID: "../bad", Path: "hook"},
		{ID: "good", Path: "../escape"},
		{ID: "good", Path: "hook"},
	} {
		m := Manifest{Name: "x", Schema: 1, Setup: []SetupStep{step}}
		if err := validatePackFacets(root, &m); err == nil {
			t.Fatalf("expected validation failure for %+v", step)
		}
	}
}

func TestPackSetupValidationRejectsIntermediateSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "hook"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "setup")); err != nil {
		t.Fatal(err)
	}
	m := Manifest{Name: "x", Schema: 1, Setup: []SetupStep{{ID: "bad", Path: "setup/hook"}}}
	if err := validatePackFacets(root, &m); err == nil || !strings.Contains(err.Error(), "symlink component") {
		t.Fatalf("got %v, want intermediate symlink rejection", err)
	}
}

func TestPackIntegrationSetupMustReferenceDeclaredHook(t *testing.T) {
	root := t.TempDir()
	m := Manifest{Name: "x", Schema: 1, Integrations: []Integration{{Name: "CRM", MCP: "crm", Setup: "missing"}}}
	if err := validatePackFacets(root, &m); err == nil || !strings.Contains(err.Error(), "unknown setup hook") {
		t.Fatalf("got %v, want unknown setup hook failure", err)
	}
}

func TestValidatePackFacetsRejectsAmbiguousIntegrationExecution(t *testing.T) {
	cases := []struct {
		name         string
		integrations []Integration
	}{
		{"duplicate MCP", []Integration{{MCP: "sneaky", Image: "safe"}, {MCP: "sneaky", Manifest: "https://evil.invalid/server.json"}}},
		{"multiple execution kinds", []Integration{{MCP: "sneaky", Image: "safe", Manifest: "https://evil.invalid/server.json"}}},
		{"manifest with ignored env", []Integration{{MCP: "sneaky", Manifest: "https://example.invalid/server.json", Env: "TOKEN"}}},
		{"remote URL with ignored env keys", []Integration{{MCP: "sneaky", URL: "https://example.invalid/mcp", EnvKeys: []string{"TOKEN"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manifest{Integrations: tc.integrations}
			if err := validatePackFacets(t.TempDir(), m); err == nil {
				t.Fatal("ambiguous integration passed validation; trust rendering and execution could disagree")
			}
		})
	}
}
