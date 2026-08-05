package reset

import (
	"fmt"
	"strings"
	"time"

	"pix/host/config"
	"pix/host/sys"
)

// defaultCfg is a minimal Config carrying the fields these tests read.
func defaultCfg() *config.Config {
	return &config.Config{Services: []string{"memory"}}
}

// resetHost is this suite's host: a REAL filesystem (every test operates on
// t.TempDir(), so faking file I/O would only weaken the assertions) with the
// three things reset actually asks the outside world about declared as data —
// which binaries are on PATH, what each one prints, and which local ports
// answer. Anything undeclared is ABSENT: no sbx on PATH, no daemon listening.
type resetHost struct {
	sys.Real                   // real temp-dir file I/O, locks, cwd, state dir
	binaries map[string]bool   // what LookPath resolves
	output   map[string]string // "cmd arg arg" -> stdout, for binaries that exist
	envVars  map[string]string // the environment this host sees
	home     string            // $HOME
	ports    map[int]bool      // local TCP ports something is listening on
}

func (h resetHost) LookPath(name string) (string, error) {
	if h.binaries[name] {
		return "/usr/bin/" + name, nil
	}
	return "", fmt.Errorf("exec: %q not found", name)
}

func (h resetHost) Run(name string, args ...string) (string, error) {
	if _, err := h.LookPath(name); err != nil {
		return "", err
	}
	return h.output[strings.Join(append([]string{name}, args...), " ")], nil
}

func (h resetHost) RunTimed(name string, args ...string) (string, bool, error) {
	out, err := h.Run(name, args...)
	return out, false, err
}

func (h resetHost) RunWithin(_ time.Duration, name string, args ...string) (string, bool, error) {
	return h.RunTimed(name, args...)
}

func (h resetHost) RunInteractive(name string, args ...string) error {
	_, err := h.Run(name, args...)
	return err
}

func (h resetHost) RunInteractiveQuiet(name string, args ...string) error {
	return h.RunInteractive(name, args...)
}

func (h resetHost) Getenv(name string) string { return h.envVars[name] }
func (h resetHost) HomeDir() string           { return h.home }
func (h resetHost) DialLocal(port int) bool   { return h.ports[port] }

// dialUpThenDownHost answers "listening" for the FIRST probe and "down"
// afterwards — the sequence a running daemon actually produces across one
// reset: the wasUp probe finds it, the post-stop guard finds it gone.
type dialUpThenDownHost struct {
	resetHost
	probed *bool
}

func (h dialUpThenDownHost) DialLocal(int) bool {
	if *h.probed {
		return false
	}
	*h.probed = true
	return true
}
