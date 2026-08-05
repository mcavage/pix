package pack

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"pix/host/config"
	"pix/host/hostenv"
)

// registerOK is the RegisterFn for tests that are not about registration.
// Activating a pack registers its MCP servers through an injected function
// now, so a test that does not care says so in one word instead of faking an
// sbx gateway.
func registerOK(*config.Config, hostenv.Env, io.Writer, []string,
	func() (string, error), map[string]config.MCPContainer) error {
	return nil
}

// readFile is this package's own copy of a three-line test read. cmd/pix has
// the same one; sharing a helper this small across a package boundary costs
// more than the duplication.
func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// isolatePackHost points config + host state at one throwaway dir and returns
// it. Every pack test needs the same isolation (the trust store, the activation
// ledger and the config all live under these two roots), so it is one call
// instead of three repeated lines per test.
func isolatePackHost(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("PIX_CONFIG", filepath.Join(dir, "config.toml"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	return dir
}

// hostExecPack writes a minimal Tier-1 pack at <dir>/<name> whose single
// host-exec facet is bin/<artifact>: a host=true [[proxy]] wrapper for
// kind "proxy", a SHA-pinned host=true [[bin]] for kind "bin". One fixture for
// both because every host-exec test needs the same shape — a pack root, one
// executable under bin/, and a manifest that declares it.
func hostExecPack(t *testing.T, dir, name, kind, artifact string) string {
	t.Helper()
	root := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := []byte("#!/bin/sh\necho " + artifact + "\n")
	if err := os.WriteFile(filepath.Join(root, "bin", artifact), content, 0o755); err != nil {
		t.Fatal(err)
	}
	m := Manifest{Name: name, Schema: 1}
	if kind == "bin" {
		m.Bins = []packBin{{Name: artifact, Path: filepath.Join("bin", artifact), Host: true, SHA: sha256Hex(content)}}
	} else {
		m.Proxies = []PackProxy{{Name: artifact, Host: true}}
	}
	mustWritePack(t, root, m)
	return root
}
