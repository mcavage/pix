package main

import (
	"bytes"
	"os"
	"testing"

	"pix/host/cli"
	workflowUat "pix/host/workflow/uat"
)

func TestUatStatus_ReadOnlyProfile(t *testing.T) {
	// uat.PeekProfilePath() uses config.StateDir(), so we need to set PIX_STATE_DIR.
	tmpDir, err := os.MkdirTemp("", "pix-test-uat-status-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	os.Setenv("XDG_STATE_HOME", tmpDir)
	defer os.Unsetenv("XDG_STATE_HOME")

	d := &cli.Deps{
		Out: &bytes.Buffer{},
		Err: &bytes.Buffer{},
	}

	cmd := &uatStatusCmd{JSON: true}
	if err := cmd.Run(d); err != nil {
		t.Fatalf("Run() error: %v", err)
	}

	p, err := workflowUat.PeekProfilePath()
	if err != nil {
		t.Fatalf("PeekProfilePath() error: %v", err)
	}

	// The profile directory should NOT exist after running status.
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("Expected profile dir %q to not exist, but Stat returned err=%v", p, err)
	}
}
