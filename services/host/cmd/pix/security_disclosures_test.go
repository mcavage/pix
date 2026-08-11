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
	"os"
	"path/filepath"
	"strings"
	"testing"

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

// TestGworkspaceSkill_DisclosesConversationExposure (product gap #1, third
// surface): the gworkspace skill must state, before "using" returned content,
// that Google Workspace content is returned into the agent conversation and
// therefore sent to the selected model provider, plus that credentials stay
// host-side and that writing/sending takes an explicit host-side act.
// Anti-drift: pins the facts, not the exact prose, so the skill can be reworded
// without silently dropping a fact.
//
// The write fact used to be pinned as the phrase "disabled by default", which
// was a claim about a flag pix passed. Pix passes no argv now — the PACK
// declares it — so there is no pix-owned default to appeal to; the surviving
// fact is that a write happens only because the host operator declared it.
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
		"unless the host operator",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("gworkspace SKILL.md missing disclosure fact %q", want)
		}
	}
}
