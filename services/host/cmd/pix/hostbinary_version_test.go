package main

import (
	"os"
	"path/filepath"
	"pix/host/launcher"
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
	// Swap launcher.Version, which is what FindHostBinary reads. main's
	// `version` is the -ldflags stamp and is pushed down once in init(), so
	// assigning it here would be a no-op the test could not see.
	old := launcher.Version
	launcher.Version = "0.1.0"
	t.Cleanup(func() { launcher.Version = old })
	_, err := findHostBinary()
	if err == nil || !strings.Contains(err.Error(), `does not match pix version "0.1.0"`) {
		t.Fatalf("findHostBinary error = %v, want explicit version mismatch", err)
	}
}

func TestFindHostBinaryAcceptsExactVersion(t *testing.T) {
	dir := t.TempDir()
	writeFakeHostBinary(t, dir, "0.1.0")
	t.Setenv("PATH", dir)
	// Swap launcher.Version, which is what FindHostBinary reads. main's
	// `version` is the -ldflags stamp and is pushed down once in init(), so
	// assigning it here would be a no-op the test could not see.
	old := launcher.Version
	launcher.Version = "0.1.0"
	t.Cleanup(func() { launcher.Version = old })
	got, err := findHostBinary()
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(dir, "pix-host") {
		t.Fatalf("got %q", got)
	}
}
