package main

import (
	"os"
	"path/filepath"
	"pix/host/sys/systest"
	"strings"
	"testing"
)

func TestRegisteredMCPCommandUsesCurrentInspectCommand(t *testing.T) {
	var calls []string
	env := shellEnv{System: &systest.Fake{LookPathFn: func(name string) (string, error) { return "/usr/local/bin/" + name, nil }, RunFn: func(name string, args ...string) (string, error) {
		call := strings.Join(append([]string{name}, args...), " ")
		calls = append(calls, call)
		if call == "sbx mcp inspect slack" {
			return "Name: slack\nType: local\nCommand: /opt/homebrew/bin/op run --no-masking --env-file=/Users/me/.config/pix/op-refs.env -- /Users/me/.local/bin/pix-host mcp slack\nResolved: /opt/homebrew/bin/op\n", nil
		}
		return "", os.ErrNotExist
	}}}
	argv, ok := registeredMCPCommand(env, "slack")
	if !ok {
		t.Fatalf("current sbx inspect output was not recognized; calls=%v", calls)
	}
	if got := strings.Join(argv, " "); !strings.Contains(got, "/Users/me/.local/bin/pix-host mcp slack") {
		t.Fatalf("registered argv = %q", got)
	}
	if len(calls) == 0 || calls[0] != "sbx mcp inspect slack" {
		t.Fatalf("first definition probe = %v, want current `sbx mcp inspect slack`", calls)
	}
}

func TestTrustedHostBinaryAcceptsInstalledSymlinkToCanonicalBinary(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "out", "pix-host")
	if err := os.MkdirAll(filepath.Dir(real), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("binary"), 0o700); err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(dir, "bin", "pix-host")
	if err := os.MkdirAll(filepath.Dir(installed), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, installed); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	env := shellEnv{System: &systest.Fake{LookPathFn: func(name string) (string, error) {
		if name == "pix-host" {
			return installed, nil
		}
		return "", os.ErrNotExist
	}}, hostBinary: func() (string, error) { return real, nil }}
	if _, ok := trustedHostBinaryExecPath(env, installed); !ok {
		t.Fatal("installed pix-host symlink to the canonical binary was rejected")
	}
	lookalike := filepath.Join(dir, "lookalike-pix-host")
	if err := os.Symlink(real, lookalike); err != nil {
		t.Fatal(err)
	}
	if _, ok := trustedHostBinaryExecPath(env, lookalike); ok {
		t.Fatal("an arbitrary symlink to the canonical binary was trusted")
	}
	foreign := filepath.Join(dir, "foreign")
	if err := os.WriteFile(foreign, []byte("other"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, ok := trustedHostBinaryExecPath(env, foreign); ok {
		t.Fatal("a different absolute binary was trusted")
	}
}
