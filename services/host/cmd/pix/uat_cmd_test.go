package main

import (
	"bytes"
	"encoding/base64"
	"os"
	"strings"
	"testing"

	"pix/host/cli"
	workflowUat "pix/host/workflow/uat"
)

func TestUatBrowserBootstrapURLIsUseful(t *testing.T) {
	raw := uatBrowserBootstrapURL()
	const prefix = "data:text/html;base64,"
	if !strings.HasPrefix(raw, prefix) {
		t.Fatalf("bootstrap URL = %q, want an embedded HTML page", raw)
	}
	page, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(raw, prefix))
	if err != nil {
		t.Fatalf("decode bootstrap page: %v", err)
	}
	text := string(page)
	for _, want := range []string{"Dedicated pix UAT browser", "accounts.google.com", "id.atlassian.com", "notion.so/login", "github.com/login"} {
		if !strings.Contains(text, want) {
			t.Errorf("bootstrap page missing %q", want)
		}
	}
	if strings.Contains(strings.ToLower(text), "<script") {
		t.Error("bootstrap page must not execute script")
	}
}

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
