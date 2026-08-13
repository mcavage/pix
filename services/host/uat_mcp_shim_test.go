package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUatBrowserCaptureShim(t *testing.T) {
	dir := t.TempDir()
	capturePath := filepath.Join(dir, "capture.txt")

	os.Setenv("PIX_UAT_BROWSER_CAPTURE", capturePath)
	defer os.Unsetenv("PIX_UAT_BROWSER_CAPTURE")

	// Missing args
	err := runUatBrowserCaptureShim([]string{})
	if err == nil {
		t.Errorf("expected error for missing args")
	}

	// Invalid URL
	err = runUatBrowserCaptureShim([]string{"not a url"})
	if err == nil {
		t.Errorf("expected error for invalid URL")
	}

	// Valid URL
	err = runUatBrowserCaptureShim([]string{"https://github.com/login?foo=bar"})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}

	data, err := os.ReadFile(capturePath)
	if err != nil {
		t.Errorf("unexpected error reading capture file: %v", err)
	}
	if string(data) != "https://github.com/login?foo=bar" {
		t.Errorf("unexpected content: %s", data)
	}

	// Verify permissions are exactly 0600
	info, err := os.Stat(capturePath)
	if err != nil {
		t.Errorf("unexpected stat error: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("expected permissions 0600, got %v", info.Mode().Perm())
	}
}
