package secret

import (
	"fmt"
	"os"
	"strings"

	"pix/host/hostenv"
	"pix/host/sys/systest"
)

// fakeEnv is this package's own env fixture: present binaries, canned command
// output, env vars, and file contents — the four seams every secret test
// actually touches. cmd/pix has a larger one of the same shape (ports, home,
// modes, an identity probe); copying the four fields is cheaper and more
// honest than exporting a fixture whose other half means nothing here.
type fakeEnv struct {
	present map[string]bool   // binaries on PATH
	output  map[string]string // "cmd arg arg" -> combined output
	envVars map[string]string // environment variables
	files   map[string]string // file contents
}

func (f fakeEnv) env() hostenv.Env {
	return hostenv.Env{System: &systest.Fake{
		LookPathFn: func(name string) (string, error) {
			if f.present[name] {
				return "/usr/bin/" + name, nil
			}
			return "", fmt.Errorf("exec: %q not found", name)
		},
		RunFn: func(name string, args ...string) (string, error) {
			key := strings.Join(append([]string{name}, args...), " ")
			if out, ok := f.output[key]; ok {
				return out, nil
			}
			return "", fmt.Errorf("no fake output for %q", key)
		},
		GetenvFn: func(name string) string { return f.envVars[name] },
		ReadFileFn: func(path string) (string, error) {
			if s, ok := f.files[path]; ok {
				return s, nil
			}
			return "", os.ErrNotExist
		},
	}}
}
