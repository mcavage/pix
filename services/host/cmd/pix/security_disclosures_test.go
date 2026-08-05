// security_disclosures_test.go; anti-drift tests for the two high-leverage
// human-output security disclosures (product gap #1): doctor's footer and
// setup's completion summary must both state that local/container MCP
// servers run on the HOST, outside the sandbox, with host-user privileges,
// and that content they return can be included in the conversation sent to
// the model provider. Both surfaces share ONE constant (doctor.McpHostTrustNotice,
// doctor_render.go) so they can never say different things; these tests
// pin the exact facts, not just "something got printed".
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pix/host/config"
	"pix/host/hostenv/hostenvtest"
	"pix/host/workflow/doctor"
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
		if !strings.Contains(doctor.McpHostTrustNotice, want) {
			t.Errorf("doctor.McpHostTrustNotice missing fact %q, got: %q", want, doctor.McpHostTrustNotice)
		}
	}
	if strings.Contains(doctor.McpHostTrustNotice, "\u2014") {
		t.Errorf("doctor.McpHostTrustNotice must not use an em dash, got: %q", doctor.McpHostTrustNotice)
	}
}

// TestDoctorRender_DisclosesHostMCPTrust_WhenMCPConfigured: doctor's footer
// must print the disclosure when at least one MCP server is configured.
func TestDoctorRender_DisclosesHostMCPTrust_WhenMCPConfigured(t *testing.T) {
	r := doctor.RunDoctor(defaultCfg(), hostenvtest.Env{}.Build())
	r.Services, r.MCP = defaultCfg().Services, []string{config.GWServerName}
	var buf bytes.Buffer
	r.Render(&buf, false, doctor.Hints())
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
	r := doctor.RunDoctor(defaultCfg(), hostenvtest.Env{}.Build())
	r.Services, r.MCP = defaultCfg().Services, nil
	var buf bytes.Buffer
	r.Render(&buf, false, doctor.Hints())
	if strings.Contains(buf.String(), doctor.McpHostTrustNotice) {
		t.Errorf("doctor must not print the MCP host-trust notice with no MCP configured, got:\n%s", buf.String())
	}
}
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
