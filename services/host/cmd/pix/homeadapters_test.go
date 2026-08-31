package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"pix/host/container"
	"pix/host/pixhome"
	"pix/host/release"
	"pix/host/workflow/provision"
)

func TestHomeContainerSpecUsesCanonicalReleaseImageReference(t *testing.T) {
	home := pixhome.New(t.TempDir())
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	manifest := release.Manifest{Version: "1.2.3", PixAgentDigest: digest, PixMemoryDigest: digest, RuntimeDigest: digest, KitRevision: "abcdef1"}
	if err := release.SaveInstalled(home.Home, manifest); err != nil {
		t.Fatal(err)
	}
	if got, want := homeContainerSpec(home).Image, provision.MemoryImageRef(manifest); got != want {
		t.Fatalf("container image = %q, want canonical release reference %q", got, want)
	}
}

func installSbxRegistrarFixture(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "sbx")
	script := "#!/bin/sh\nd='" + dir + "'\n" + body
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestSbxMemoryRegistrarExplicitlyAllowsOwnedLoopbackEndpoint(t *testing.T) {
	dir := installSbxRegistrarFixture(t, `
if [ "$1 $2" = "mcp ls" ]; then exit 0; fi
printf '%s\n' "$@" > "$d/argv"
`)
	receipt := filepath.Join(dir, "argv")

	const endpoint = "http://127.0.0.1:18080/mcp?token=secret"
	state, err := (sbxMemoryRegistrar{}).EnsureMemoryRemote("pix-memory", endpoint)
	if err != nil {
		t.Fatalf("EnsureMemoryRemote: %v", err)
	}
	if state != provision.MCPRegistrationAdded {
		t.Fatalf("state = %v, want added", state)
	}
	data, err := os.ReadFile(receipt)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(data))
	want := []string{"mcp", "add", "pix-memory", "--url", endpoint, "--skip-ssrf-check", "--skip_auth"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sbx argv = %#v, want %#v", got, want)
	}
}

func TestSbxMemoryRegistrarReportsFailureWithoutLeakingToken(t *testing.T) {
	const token = "0123456789abcdef"
	installSbxRegistrarFixture(t, `
if [ "$1 $2" = "mcp ls" ]; then exit 0; fi
printf 'SSRF guard refused loopback; rejected URL %s\n' "$5" >&2
exit 1
`)
	_, err := (sbxMemoryRegistrar{}).EnsureMemoryRemote("pix-memory", "http://127.0.0.1:18080/mcp?token="+token)
	if err == nil {
		t.Fatal("EnsureMemoryRemote succeeded, want failure")
	}
	got := err.Error()
	if strings.Contains(got, token) {
		t.Fatalf("error leaked token: %s", got)
	}
	if !strings.Contains(got, container.RedactedTokenPlaceholder) || !strings.Contains(got, "SSRF guard refused loopback") {
		t.Fatalf("error = %q, want redacted sbx stderr", got)
	}
}
