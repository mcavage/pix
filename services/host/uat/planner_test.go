package uat

import (
	"testing"
)

func TestMCPPlanner(t *testing.T) {
	planner := NewMCPPlanner("/usr/local/bin/pix-host", "/repo", "/state", "session-id")
	
	cmd := planner.PlanAdd("mcp-server", "some-url")
	
	expected := []string{
		"/usr/local/bin/pix-host",
		"mcp", "add", "mcp-server",
		"--url", "some-url",
		"--repo", "/repo",
		"--state", "/state",
		"--session", "session-id",
	}

	if len(cmd) != len(expected) {
		t.Fatalf("expected len %d, got %d. cmd: %v", len(expected), len(cmd), cmd)
	}

	for i, v := range expected {
		if cmd[i] != v {
			t.Errorf("at index %d: expected %s, got %s", i, v, cmd[i])
		}
	}
}

func TestTagPlanner(t *testing.T) {
	planner := NewMCPPlanner("/usr/local/bin/pix-host", "/repo", "/state", "session-id")
	
	cmd := planner.PlanTag("my-image", "v1")
	
	expected := []string{
		"docker", "tag", "my-image", "uat-v1",
	}

	if len(cmd) != len(expected) {
		t.Fatalf("expected len %d, got %d. cmd: %v", len(expected), len(cmd), cmd)
	}

	for i, v := range expected {
		if cmd[i] != v {
			t.Errorf("at index %d: expected %s, got %s", i, v, cmd[i])
		}
	}
}
