// security_disclosures_test.go; anti-drift tests for a security disclosure
// that must keep stating its facts rather than merely printing something.
//
// The MCP host-trust footer that used to live here is GONE: it printed on
// every `pix doctor` run, and a paragraph a user reads once and then skims
// forever is not a control. SECURITY.md documents the property.
package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
