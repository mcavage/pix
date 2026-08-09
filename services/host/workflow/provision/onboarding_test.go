package provision

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pix/host/config"
)

// The onboarding half of provision, tested against REAL boundaries like the rest
// of this package: a real config file on disk, a real `pix-host mcp --list`
// fixture for the locally-known MCP probe, and the package's own composition
// (HostBinary/Register/VerifyCatalogMCPReady) rather than a Deps struct built for
// the test. Nothing here stubs an answer.

// noHostBinary makes mcp.LocalMCPNames report the local set as UNKNOWN, so
// validation fails closed on any non-gog/non-catalog mcp name.
func noHostBinary() (string, error) { return "", fmt.Errorf("no host binary in test") }

// writeProposal writes <ws>/.pix/onboarding.json against a temp config file.
func writeProposal(t *testing.T, ws, body string) string {
	t.Helper()
	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "config.toml"))
	t.Setenv("PIX_PROFILE", "")
	if err := os.MkdirAll(filepath.Join(ws, ".pix"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ws, ".pix", OnboardingFileName)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidateOnboarding_Allowlist(t *testing.T) {
	for i, r := range []*OnboardingResult{
		{Version: 1, MCP: []string{config.GWServerName}},
		{Version: 1, MCP: []string{"notion", "atlassian", "granola"}},
	} {
		if err := validateOnboarding(r, realEnv(), noHostBinary); err != nil {
			t.Errorf("ok[%d] rejected: %v", i, err)
		}
	}
	bad := map[string]*OnboardingResult{
		"bad version": {Version: 2},
		"unknown mcp": {Version: 1, MCP: []string{"evil-server"}},
		// "linear" is the drift reading mcp.McpCatalogNames directly removes: it
		// looks like a plausible catalog name, but `pix mcp bundle` cannot
		// register it, so accepting it would persist a server that never comes up.
		"unshipped catalog-looking mcp": {Version: 1, MCP: []string{"linear"}},
		"model whitespace":              {Version: 1, OllamaBridgeModel: "bad model"},
	}
	for name, r := range bad {
		if err := validateOnboarding(r, realEnv(), noHostBinary); err == nil {
			t.Errorf("%s: expected rejection, got nil", name)
		}
	}
}

// A locally-known host server is accepted only because a REAL `pix-host mcp
// --list` said so: the fixture below is the boundary, so the fail-closed rule
// above and the accept rule here are one code path with a different world.
func TestValidateOnboarding_LocallyKnownServerComesFromTheRealProbe(t *testing.T) {
	dir := binDir(t)
	fixtureBin(t, "pix-host", `[ "$1" = "mcp" ] && echo slack`)
	resolver := func() (string, error) { return filepath.Join(dir, "pix-host"), nil }

	if err := validateOnboarding(&OnboardingResult{Version: 1, MCP: []string{"slack"}}, realEnv(), resolver); err != nil {
		t.Errorf("a server the local host binary lists must be accepted: %v", err)
	}
	if err := validateOnboarding(&OnboardingResult{Version: 1, MCP: []string{"warehouse"}}, realEnv(), resolver); err == nil {
		t.Error("a server the local host binary does NOT list must be rejected")
	}
}

// applyOnboarding writes exactly the proposal's fields, and re-applying it
// changes nothing (the idempotence fitness function).
func TestApplyOnboarding_FieldsThenIdempotent(t *testing.T) {
	cfg := &config.Config{Services: []string{"memory"}}
	r := &OnboardingResult{
		Version: 1, MCP: []string{config.GWServerName, "notion"},
		OllamaBridgeModel: "qwen3.5:9b", MemoryWatcherModel: "qwen3.5:9b",
	}
	if first := applyOnboarding(r, cfg); len(first) == 0 {
		t.Fatal("first apply made no changes")
	}
	// Google Workspace authorization needs a browser, so applying a proposal
	// must never set google_workspace_account (that write is manual).
	if cfg.GogAccount != "" {
		t.Errorf("onboarding must never set google_workspace_account, got %q", cfg.GogAccount)
	}
	if !slices.Contains(cfg.MCP, config.GWServerName) || !slices.Contains(cfg.MCP, "notion") {
		t.Errorf("mcp = %v", cfg.MCP)
	}
	if !slices.Contains(cfg.Services, "memory") {
		t.Errorf("memory service should be ensured: %v", cfg.Services)
	}
	if cfg.OllamaBridgeModel != "qwen3.5:9b" || cfg.MemoryWatcherModel != "qwen3.5:9b" {
		t.Errorf("models not applied: bridge=%q watcher=%q", cfg.OllamaBridgeModel, cfg.MemoryWatcherModel)
	}
	if second := applyOnboarding(r, cfg); len(second) != 0 {
		t.Errorf("second apply not idempotent, changed: %v", second)
	}
}

// The CI path: assumeYes applies the file, persists it, clears the marker — and
// says so about the registration it did NOT perform (Register is composition the
// command layer supplies, unwired here).
func TestReconcileOnboarding_AppliesFromFile(t *testing.T) {
	ws := t.TempDir()
	// google_workspace_account is a field OnboardingResult deliberately does not
	// have: the typed unmarshal must drop it, never apply it.
	path := writeProposal(t, ws, `{"version":1,"google_workspace_account":"me@x.com","mcp":["`+config.GWServerName+`"]}`)

	var out bytes.Buffer
	ReconcileOnboarding(ws, realEnv(), strings.NewReader(""), &out, true, false)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("onboarding.json should be removed after apply, err=%v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GogAccount != "" {
		t.Errorf("onboarding must never apply google_workspace_account from the file, got %q", cfg.GogAccount)
	}
	if !slices.Contains(cfg.MCP, config.GWServerName) {
		t.Errorf("config not applied: mcp=%v", cfg.MCP)
	}
	if !strings.Contains(out.String(), "mcp add skipped") {
		t.Errorf("an unperformed registration must say so, got:\n%s", out.String())
	}
}

// Two ways a proposal does NOT get applied, and both leave the file for a human
// plus a config with nothing in it: no consent (non-TTY without --yes), and a
// name outside the allowlist.
func TestReconcileOnboarding_LeavesFileWhenNotApplied(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		assumeYes  bool
		want       string
	}{
		{"no consent", `{"version":1,"mcp":["` + config.GWServerName + `"]}`, false, "Not a terminal"},
		{"refused name", `{"version":1,"mcp":["evil-server"]}`, true, "refusing onboarding proposal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			path := writeProposal(t, ws, tc.body)
			var out bytes.Buffer
			ReconcileOnboarding(ws, realEnv(), strings.NewReader(""), &out, tc.assumeYes, false)
			if _, err := os.Stat(path); err != nil {
				t.Errorf("the file must be left for review, err=%v", err)
			}
			if !strings.Contains(out.String(), tc.want) {
				t.Errorf("output must explain itself (%q), got:\n%s", tc.want, out.String())
			}
			if cfg, err := config.Load(); err != nil || len(cfg.MCP) != 0 {
				t.Errorf("nothing may be persisted: mcp=%v err=%v", cfg.MCP, err)
			}
		})
	}
}

func TestParseSetupArgs(t *testing.T) {
	o, err := ParseSetupArgs([]string{"--mcp", config.GWServerName, "--mcp=notion", "--model", "m",
		"--pack", "one", "--pack=two", "--with", "optional", "--pull-models", "--yes",
		// accepted and discarded: kong still declares them, nothing reads them
		"--models=a,b", "--google-workspace", "--credentials", "/tmp/x.json"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if o.Model != "m" || !o.AssumeYes || !o.PullModels {
		t.Errorf("parsed = %+v", o)
	}
	if got := strings.Join(o.Mcp, ","); got != config.GWServerName+",notion" {
		t.Errorf("mcp = %q", got)
	}
	if got := strings.Join(o.Packs, ","); got != "one,two" {
		t.Errorf("packs = %q (order is the invocation order)", got)
	}
	if got := strings.Join(o.WithSetup, ","); got != "optional" {
		t.Errorf("with = %q", got)
	}
	if _, err := ParseSetupArgs([]string{"--model"}); err == nil {
		t.Error("a value flag without its value should error")
	}
	if _, err := ParseSetupArgs([]string{"--bogus"}); err == nil {
		t.Error("an unknown flag should error")
	}
}
