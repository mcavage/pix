package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestBaseKitUsesV3Grammar guards the workload descriptor and its OCI recipe.
func TestBaseKitUsesV3Grammar(t *testing.T) {
	root := repoRootForVersionLockstep(t)
	b, err := os.ReadFile(filepath.Join(root, "pi-kit", "pix", "pix.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# syntax=docker/sandbox-kit:3", "schemaVersion: '3'", "kind: workload", "- type: com.docker.sandbox/sbx@1", "- type: com.docker.sandbox/network-policy@1", "- type: com.docker.sandbox/credential@1", "- type: com.docker.sandbox/lifecycle@1", "- type: com.docker.sandbox/agent-context@1"} {
		if !regexp.MustCompile(regexp.QuoteMeta(want)).Match(b) {
			t.Errorf("v3 workload is missing %q", want)
		}
	}
	for _, retired := range []string{"permissions:", "setup:", "agentInstructions:", "sandbox:"} {
		if regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(retired)).Match(b) {
			t.Errorf("v3 workload restored v2 key %q", retired)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "pi-kit", "pix", "pix.dockerfile")); err != nil {
		t.Fatal(err)
	}
}
