package pack

import (
	"io"
	"os"
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
