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
	m := Manifest{Name: "x", Schema: 1, Integrations: []Integration{
		{Name: "CRM", MCP: "crm", Image: "crm-mcp:1", Setup: "missing"},
	}}
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

// TestLoadPack_IntegrationWithoutTransportRefused: pix ships NO built-in MCP
// servers, so an [[integrations]] entry that names an `mcp` but declares no
// transport is a server nothing can ever start. It must be refused at LOAD —
// the alternative is a name that lands in cfg.MCP and then fails registration
// with a mystery "not declared". An integration with no `mcp` at all is
// reference-only and still fine (it contributes a credential name, not a
// server).
func TestLoadPack_IntegrationWithoutTransportRefused(t *testing.T) {
	root := t.TempDir()
	writeManifest := func(t *testing.T, body string) error {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, PackManifestName), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := LoadPack(root)
		return err
	}
	err := writeManifest(t, "name = \"p\"\nschema = 1\n\n[[integrations]]\nname = \"CRM\"\nmcp = \"crm\"\nenv = \"CRM_TOKEN\"\n")
	if err == nil || !strings.Contains(err.Error(), "declares no transport") {
		t.Fatalf("got %v, want a no-transport refusal naming the missing choice", err)
	}
	for _, want := range []string{"command", "image", "manifest", "url"} {
		if err != nil && !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal must name %q as a choice, got: %v", want, err)
		}
	}
	// Reference-only (no mcp name): nothing to register, so nothing to refuse.
	if err := writeManifest(t, "name = \"p\"\nschema = 1\n\n[[integrations]]\nname = \"CRM\"\nenv = \"CRM_TOKEN\"\n"); err != nil {
		t.Errorf("an integration with no mcp name is reference-only and must still load: %v", err)
	}
	// Each of the four transports on its own is enough.
	for _, transport := range []string{
		"command = \"crm-mcp\"", "image = \"crm-mcp:1\"",
		"manifest = \"https://example.invalid/server.json\"", "url = \"https://example.invalid/mcp\"",
	} {
		if err := writeManifest(t, "name = \"p\"\nschema = 1\n\n[[integrations]]\nname = \"CRM\"\nmcp = \"crm\"\n"+transport+"\n"); err != nil {
			t.Errorf("transport %s must be accepted: %v", transport, err)
		}
	}
}

// TestValidateDeclarativeSetup_ClosedVocabulary: the require/apply vocabulary
// is CLOSED and every rejection here is a step that could never have passed.
// An unknown kind is a typo pix must catch at load rather than at 2am, and a
// `bin` requirement with no install hint is a dead end for the user — pix
// cannot guess a package manager, so the pack must say how its dependency is
// obtained.
func TestValidateDeclarativeSetup_ClosedVocabulary(t *testing.T) {
	for _, tc := range []struct {
		name string
		step SetupStep
		want string
	}{
		{"unknown require kind", SetupStep{ID: "s", Require: []SetupRequire{{Kind: "binary", Name: "gog", Install: "brew install gog"}}},
			"require kind \"binary\" is not one of bin, op-ref, probe"},
		{"unknown apply kind", SetupStep{ID: "s",
			Require: []SetupRequire{{Kind: "bin", Name: "gog", Install: "brew install gog"}},
			Apply:   []SetupApply{{Kind: "shell", Argv: []string{"gog", "auth"}}}},
			"apply kind \"shell\" is not one of interactive, exec"},
		{"bin without install hint", SetupStep{ID: "s", Require: []SetupRequire{{Kind: "bin", Name: "gog"}}},
			"needs an install hint"},
		{"op-ref without env", SetupStep{ID: "s", Require: []SetupRequire{{Kind: "op-ref"}}},
			"needs an env var name"},
		{"probe without argv", SetupStep{ID: "s", Require: []SetupRequire{{Kind: "probe"}}},
			"needs an argv"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := Manifest{Name: "x", Schema: 1, Setup: []SetupStep{tc.step}}
			err := validatePackFacets(t.TempDir(), &m)
			if err == nil {
				t.Fatalf("validation accepted a step that can never pass: %+v", tc.step)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not explain the refusal (%q)", err, tc.want)
			}
		})
	}
	// The well-formed declarative step this vocabulary exists for still loads.
	ok := Manifest{Name: "x", Schema: 1, Setup: []SetupStep{{
		ID:      "account",
		Require: []SetupRequire{{Kind: "bin", Name: "gog", Install: "brew install example/tap/gog"}, {Kind: "op-ref", Env: "GOG_KEYRING_PASSWORD"}, {Kind: "probe", Argv: []string{"gog", "auth", "doctor"}}},
		Apply:   []SetupApply{{Kind: "interactive", Argv: []string{"gog", "auth", "login"}, Explain: "opens a browser"}},
	}}}
	if err := validatePackFacets(t.TempDir(), &ok); err != nil {
		t.Fatalf("a well-formed declarative step must load: %v", err)
	}
}
