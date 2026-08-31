package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveAuthToken_PrefersMountedFile is the container-side half of
// security re-review round 1 blocker #1: pix-memory reads its bearer token
// from the bind-mounted FILE first, never requiring a literal env var value
// in the container's own Config.Env.
func TestResolveAuthToken_PrefersMountedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth-token")
	if err := os.WriteFile(path, []byte("file-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEMORY_AUTH_TOKEN_FILE", path)
	t.Setenv("MEMORY_AUTH_TOKEN", "env-token")

	if got := resolveAuthToken(); got != "file-token" {
		t.Fatalf("resolveAuthToken() = %q, want the mounted file's token, not the env var", got)
	}
}

// TestResolveAuthToken_FallsBackToEnvVarWhenFileAbsent: the DEV-ONLY
// fallback — no container, no mount — still works.
func TestResolveAuthToken_FallsBackToEnvVarWhenFileAbsent(t *testing.T) {
	t.Setenv("MEMORY_AUTH_TOKEN_FILE", filepath.Join(t.TempDir(), "does-not-exist"))
	t.Setenv("MEMORY_AUTH_TOKEN", "env-token")

	if got := resolveAuthToken(); got != "env-token" {
		t.Fatalf("resolveAuthToken() = %q, want the env-var dev fallback", got)
	}
}

// TestResolveAuthToken_EmptyFileFallsBackToEnvVar: a mounted-but-empty file
// is treated the same as absent, not as "deliberately no auth".
func TestResolveAuthToken_EmptyFileFallsBackToEnvVar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth-token")
	if err := os.WriteFile(path, []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MEMORY_AUTH_TOKEN_FILE", path)
	t.Setenv("MEMORY_AUTH_TOKEN", "env-token")

	if got := resolveAuthToken(); got != "env-token" {
		t.Fatalf("resolveAuthToken() = %q, want the env-var fallback for a blank file", got)
	}
}

// TestResolveAuthToken_NothingResolvesIsEmpty: no file, no env var — the
// caller (main) logs its warning and /mcp refuses every request; this
// function itself just reports "".
func TestResolveAuthToken_NothingResolvesIsEmpty(t *testing.T) {
	t.Setenv("MEMORY_AUTH_TOKEN_FILE", filepath.Join(t.TempDir(), "does-not-exist"))
	t.Setenv("MEMORY_AUTH_TOKEN", "")

	if got := resolveAuthToken(); got != "" {
		t.Fatalf("resolveAuthToken() = %q, want empty", got)
	}
}
