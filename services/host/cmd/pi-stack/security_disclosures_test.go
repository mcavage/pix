// security_disclosures_test.go; anti-drift tests for the two high-leverage
// human-output security disclosures (product gap #1): doctor's footer and
// setup's completion summary must both state that local/container MCP
// servers run on the HOST, outside the sandbox, with host-user privileges,
// and that content they return can be included in the conversation sent to
// the model provider. Both surfaces share ONE constant (mcpHostTrustNotice,
// doctor_render.go) so they can never say different things; these tests
// pin the exact facts, not just "something got printed".
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pi-stack/host/config"
)

// mcpHostTrustNoticeFacts are the exact facts the disclosure must state,
// checked individually so a future rewrite can't accidentally drop one while
// keeping the string "looking" similar.
var mcpHostTrustNoticeFacts = []string{
	"run on the host",
	"outside the sandbox",
	"host-user privileges",
	"sent to your model provider",
}

func TestMcpHostTrustNotice_StatesBothFacts(t *testing.T) {
	for _, want := range mcpHostTrustNoticeFacts {
		if !strings.Contains(mcpHostTrustNotice, want) {
			t.Errorf("mcpHostTrustNotice missing fact %q, got: %q", want, mcpHostTrustNotice)
		}
	}
	if strings.Contains(mcpHostTrustNotice, "\u2014") {
		t.Errorf("mcpHostTrustNotice must not use an em dash, got: %q", mcpHostTrustNotice)
	}
}

// TestDoctorRender_DisclosesHostMCPTrust_WhenMCPConfigured: doctor's footer
// must print the disclosure when at least one MCP server is configured.
func TestDoctorRender_DisclosesHostMCPTrust_WhenMCPConfigured(t *testing.T) {
	r := runDoctor(defaultCfg(), fakeEnv{}.env())
	r.services, r.mcp = defaultCfg().Services, []string{gwServerName}
	var buf bytes.Buffer
	r.render(&buf, false)
	out := buf.String()
	for _, want := range mcpHostTrustNoticeFacts {
		if !strings.Contains(out, want) {
			t.Errorf("doctor render missing disclosure fact %q, got:\n%s", want, out)
		}
	}
}

// TestDoctorRender_NoDisclosure_WhenNoMCPConfigured: with nothing configured
// there is nothing to disclose, so doctor must stay notice-free (concise,
// never alarmist about something the user hasn't touched).
func TestDoctorRender_NoDisclosure_WhenNoMCPConfigured(t *testing.T) {
	r := runDoctor(defaultCfg(), fakeEnv{}.env())
	r.services, r.mcp = defaultCfg().Services, nil
	var buf bytes.Buffer
	r.render(&buf, false)
	if strings.Contains(buf.String(), mcpHostTrustNotice) {
		t.Errorf("doctor must not print the MCP host-trust notice with no MCP configured, got:\n%s", buf.String())
	}
}

// hostTrustSummaryEnv is a minimal shellEnv sufficient for
// printSetupSummary's own reads (hostModeProviderKeys, gogSetupAccountHealthy)
// without touching the real filesystem.
func hostTrustSummaryEnv(t *testing.T) shellEnv {
	t.Helper()
	home := t.TempDir()
	return shellEnv{
		getenv:  func(string) string { return "" },
		homeDir: func() string { return home },
	}
}

// TestPrintSetupSummary_DisclosesHostMCPTrust_WhenMCPConfigured: setup's
// completion summary must state the same two facts when MCP is configured.
func TestPrintSetupSummary_DisclosesHostMCPTrust_WhenMCPConfigured(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	cfg := &config.Config{MCP: []string{gwServerName}}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	printSetupSummary(cfg, hostTrustSummaryEnv(t), &out, setupModelsOutcome{})
	got := out.String()
	for _, want := range mcpHostTrustNoticeFacts {
		if !strings.Contains(got, want) {
			t.Errorf("setup summary missing disclosure fact %q, got:\n%s", want, got)
		}
	}
}

// TestPrintSetupSummary_NoDisclosure_WhenNoMCPConfigured mirrors doctor's
// same-gate behavior: no MCP configured, no notice.
func TestPrintSetupSummary_NoDisclosure_WhenNoMCPConfigured(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	cfg := &config.Config{}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	printSetupSummary(cfg, hostTrustSummaryEnv(t), &out, setupModelsOutcome{})
	if strings.Contains(out.String(), mcpHostTrustNotice) {
		t.Errorf("setup summary must not print the MCP host-trust notice with no MCP configured, got:\n%s", out.String())
	}
}

// TestGworkspaceSkill_DisclosesConversationExposure (product gap #1, third
// surface): the gworkspace skill must state, before "using" returned content,
// that Google Workspace content is returned into the agent conversation and
// therefore sent to the selected model provider, plus that credentials stay
// host-side and write/send is disabled by default. Anti-drift: pins the
// facts, not the exact prose, so the skill can be reworded without silently
// dropping a fact.
func TestGworkspaceSkill_DisclosesConversationExposure(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "skills", "gworkspace", "SKILL.md"))
	if err != nil {
		t.Fatalf("reading gworkspace SKILL.md: %v", err)
	}
	content := strings.ToLower(string(b))
	for _, want := range []string{
		"returned into",
		"conversation",
		"model provider",
		"credentials",
		"host-side",
		"write",
		"disabled by default",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("gworkspace SKILL.md missing disclosure fact %q", want)
		}
	}
}
