// Package hostenvtest is the shared env fixture for packages extracted out of
// cmd/pix: a hostenv.Env whose OS seams are declared as data (which binaries
// are present, what each command prints, the env vars, the files) rather than
// wired one closure at a time.
//
// A fixture belongs next to the type it fakes, which is why this sits under
// hostenv, mirroring sys/systest sitting under sys.
//
// Anything left unset is ABSENT, not an error: an undeclared binary is not on
// PATH, an undeclared file does not exist. An undeclared COMMAND is different
// — Run returns an error naming it, so a fixture gap surfaces as a failing
// test with an actionable message instead of an empty string that reads like
// real output.
package hostenvtest

import (
	"fmt"
	"os"
	"strings"

	"pix/host/hostenv"
	"pix/host/sys/systest"
)

// Env declares the seams a test cares about. The zero value is a machine with
// nothing installed and no files.
type Env struct {
	Present  map[string]bool        // binaries on PATH
	Output   map[string]string      // "cmd arg arg" -> combined output
	EnvVars  map[string]string      // environment variables
	Files    map[string]string      // path -> contents
	StatFile map[string]bool        // paths that exist but whose contents do not matter
	Modes    map[string]os.FileMode // path -> mode bits
	Ports    map[int]bool           // open local TCP ports
	Home     string                 // fake home directory
	HostBin  string                 // canonical pix-host path ("" = unresolvable)
}

// Build returns the hostenv.Env this fixture describes.
func (f Env) Build() hostenv.Env {
	return hostenv.Env{
		System: &systest.Fake{
			LookPathFn: func(name string) (string, error) {
				if f.Present[name] {
					return "/usr/bin/" + name, nil
				}
				return "", fmt.Errorf("exec: %q not found", name)
			},
			RunFn: func(name string, args ...string) (string, error) {
				key := strings.Join(append([]string{name}, args...), " ")
				if out, ok := f.Output[key]; ok {
					return out, nil
				}
				return "", fmt.Errorf("no fake output for %q", key)
			},
			GetenvFn:    func(name string) string { return f.EnvVars[name] },
			DialLocalFn: func(port int) bool { return f.Ports[port] },
			IsFileFn:    func(path string) bool { return f.StatFile[path] || f.Files[path] != "" },
			ReadFileFn: func(path string) (string, error) {
				if s, ok := f.Files[path]; ok {
					return s, nil
				}
				return "", os.ErrNotExist
			},
			HomeDirFn: func() string { return f.Home },
			ModeFn: func(path string) (os.FileMode, bool) {
				m, ok := f.Modes[path]
				return m, ok
			},
		},
		HostBinary: func() (string, error) {
			if f.HostBin != "" {
				return f.HostBin, nil
			}
			return "", fmt.Errorf("pix-host not found")
		},
	}
}
