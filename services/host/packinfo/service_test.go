package packinfo

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeServicePack writes a minimal pack whose only content is the [[services]]
// body under test, and returns its root.
func writeServicePack(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	manifest := "name = \"svc-pack\"\nschema = 1\n" + body
	if err := os.WriteFile(filepath.Join(dir, PackManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestValidateServices_DaemonRuntime pins the vocabulary of the daemon runtime,
// which exists because go-plugin is the wrong shape for a process that already
// speaks its own protocol on a port (snow-proxy is reached over HTTP by an
// in-sandbox wrapper, so a net/rpc handshake would replace a working transport
// to gain nothing).
//
// The refusals matter more than the acceptances. A daemon with no health check
// is a process nothing can evict, and eviction is most of what the supervisor
// buys over a LaunchAgent's KeepAlive — launchd restarts a process that exits
// and has no opinion about one that is wedged.
func TestValidateServices_DaemonRuntime(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		wantErr    string
	}{
		{
			name: "command form is accepted unpinned",
			body: `[[services]]
name = "snow-proxy"
runtime = "daemon"
activation = "always"
command = "snow-proxy"
port = 11442
health = "/health"
license = "MIT"
source = "https://github.com/docker/pix-integrations"
`,
		},
		{
			name: "no health check is refused",
			body: `[[services]]
name = "d"
runtime = "daemon"
activation = "always"
command = "d"
port = 1234
license = "MIT"
source = "https://example.com"
`,
			wantErr: "requires health",
		},
		{
			name: "no port is refused",
			body: `[[services]]
name = "d"
runtime = "daemon"
activation = "always"
command = "d"
health = "tcp"
license = "MIT"
source = "https://example.com"
`,
			wantErr: "requires a port",
		},
		{
			name: "both path and command is refused",
			body: `[[services]]
name = "d"
runtime = "daemon"
activation = "always"
command = "d"
path = "bin/d"
sha = "0000000000000000000000000000000000000000000000000000000000000000"
port = 1234
health = "tcp"
license = "MIT"
source = "https://example.com"
`,
			wantErr: "exactly one of path",
		},
		{
			name: "a command with a sha is refused: the pin would describe a file nothing reads",
			body: `[[services]]
name = "d"
runtime = "daemon"
activation = "always"
command = "d"
sha = "0000000000000000000000000000000000000000000000000000000000000000"
port = 1234
health = "tcp"
license = "MIT"
source = "https://example.com"
`,
			wantErr: "must not set sha",
		},
		{
			name: "a path traversal in command is refused",
			body: `[[services]]
name = "d"
runtime = "daemon"
activation = "always"
command = "../../bin/sh"
port = 1234
health = "tcp"
license = "MIT"
source = "https://example.com"
`,
			wantErr: "bare binary name",
		},
		{
			name: "a nonsense health value is refused",
			body: `[[services]]
name = "d"
runtime = "daemon"
activation = "always"
command = "d"
port = 1234
health = "maybe"
license = "MIT"
source = "https://example.com"
`,
			wantErr: `health must be "tcp"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeServicePack(t, tc.body)
			_, err := LoadPack(dir)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("want accepted, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want refused for %q, got accepted", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error must explain the problem; want %q in: %v", tc.wantErr, err)
			}
		})
	}
}
