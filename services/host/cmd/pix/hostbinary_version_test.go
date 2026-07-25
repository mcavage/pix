package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeFakeHostBinary(t *testing.T, dir, reported string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	path := filepath.Join(dir, "pix-host")
	body := "#!/bin/sh\n[ \"$1\" = version ] || exit 2\nprintf '%s\\n' " + reported + "\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestFindHostBinaryRejectsVersionSkew(t *testing.T) {
	dir := t.TempDir()
	writeFakeHostBinary(t, dir, "0.0.9")
	t.Setenv("PATH", dir)
	old := version
	version = "0.1.0"
	t.Cleanup(func() { version = old })
	_, err := findHostBinary()
	if err == nil || !strings.Contains(err.Error(), `does not match pix version "0.1.0"`) {
		t.Fatalf("findHostBinary error = %v, want explicit version mismatch", err)
	}
}

func TestFindHostBinaryAcceptsExactVersion(t *testing.T) {
	dir := t.TempDir()
	writeFakeHostBinary(t, dir, "0.1.0")
	t.Setenv("PATH", dir)
	old := version
	version = "0.1.0"
	t.Cleanup(func() { version = old })
	got, err := findHostBinary()
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, "pix-host") {
		t.Fatalf("got %q", got)
	}
}
