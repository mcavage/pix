package uat

import (
	"testing"
)

func TestMCPPlanner(t *testing.T) {
	planner, err := NewMCPPlanner("/usr/local/bin/pix-host", "/repo", "/state", "session-id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cmd := planner.PlanRegistrationAdd("mcp-server")

	expected := []string{
		"mcp", "add", "mcp-server",
		"--command", "/usr/local/bin/pix-host",
		"--args", "uat-mcp",
		"--args", "--repo", "--args", "/repo",
		"--args", "--state", "--args", "/state",
		"--args", "--session", "--args", "session-id",
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

func TestMCPPlannerValidation(t *testing.T) {
	_, err := NewMCPPlanner("relative-path", "/repo", "/state", "session-id")
	if err == nil {
		t.Error("expected error for relative pixHost")
	}

	_, err = NewMCPPlanner("/usr/local/bin/pix-host", "relative-repo", "/state", "session-id")
	if err == nil {
		t.Error("expected error for relative repoPath")
	}

	_, err = NewMCPPlanner("/usr/local/bin/pix-host", "/repo", "relative-state", "session-id")
	if err == nil {
		t.Error("expected error for relative statePath")
	}

	_, err = NewMCPPlanner("/usr/local/bin/pix-host", "/repo", "/state", "")
	if err == nil {
		t.Error("expected error for empty sessionID")
	}
}

func TestRegistrationRemove(t *testing.T) {
	planner, err := NewMCPPlanner("/usr/local/bin/pix-host", "/repo", "/state", "session-id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cmd := planner.PlanRegistrationRemove("mcp-server")

	expected := []string{
		"mcp", "rm", "mcp-server",
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
	planner, err := NewMCPPlanner("/usr/local/bin/pix-host", "/repo", "/state", "session-id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	cmd, err := planner.PlanTag("sha256:abc")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := []string{
		"docker", "tag", "sha256:abc", "pix:uat-session-id",
	}

	if len(cmd) != len(expected) {
		t.Fatalf("expected len %d, got %d. cmd: %v", len(expected), len(cmd), cmd)
	}

	for i, v := range expected {
		if cmd[i] != v {
			t.Errorf("at index %d: expected %s, got %s", i, v, cmd[i])
		}
	}

	_, err = planner.PlanTag("-flag")
	if err == nil {
		t.Errorf("expected error for flag sourceRef")
	}
}
