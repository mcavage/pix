package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestBaseKitUsesStrictV2Grammar guards the July 2026 spelling cutover. All
// names below still describe the same canonical artifact, so ordinary YAML
// parsing cannot catch accidentally restoring the retired lenient-v2 keys.
func TestBaseKitUsesStrictV2Grammar(t *testing.T) {
	root := repoRootForVersionLockstep(t)
	b, err := os.ReadFile(filepath.Join(root, "pi-kit", "spec.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"permissions:", "setup:", "agentInstructions:", "  entrypoint:"} {
		if !regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(want)).Match(b) {
			t.Errorf("strict-v2 base kit is missing %q", want)
		}
	}
	for _, retired := range []string{"caps:", "commands:", "agentContext:", "  aiFilename:", "    run:"} {
		if regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(retired)).Match(b) {
			t.Errorf("base kit restored retired lenient-v2 key %q", retired)
		}
	}
}
