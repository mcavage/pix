package reset

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"pix/host/config"
	"pix/host/service"
	"pix/host/sys"
)

// TestMain pins ONE thing for the whole suite: the liveness probe never asks the
// DEVELOPER'S machine whether IT has a managed pix service loaded (on darwin
// that is a real `launchctl` call, and a loaded unit would make every reset test
// think a daemon is up). The pidfile half stays the REAL one — process identity
// is what these tests are about — so "a daemon is up" means a real pidfile
// naming a real live process, and nothing else.
func TestMain(m *testing.M) {
	probeServeUp = func(pidPath string, settle time.Duration) (bool, int) {
		return service.ServeIdentityUp(nil, pidPath, settle)
	}
	os.Exit(m.Run())
}

// defaultCfg is a minimal Config carrying the fields these tests read.
func defaultCfg() *config.Config {
	return &config.Config{Services: []string{"memory"}}
}

// stubServeProbe drives the liveness probe's answers IN SEQUENCE — one per call,
// which is the pre-stop "was a daemon running" question and then the post-stop
// "is it really gone" one — so a decision-table test can model a daemon state
// without a process. Calls past the last answer repeat it. The real-process
// layer (reset_process_test.go) proves the probe itself.
func stubServeProbe(t *testing.T, answers ...bool) {
	t.Helper()
	orig := probeServeUp
	calls := 0
	probeServeUp = func(string, time.Duration) (bool, int) {
		i := calls
		if i >= len(answers) {
			i = len(answers) - 1
		}
		calls++
		return answers[i], 4242
	}
	t.Cleanup(func() { probeServeUp = orig })
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

// DialLocal exists because sys.System requires it. Reset itself no longer asks a
// PORT anything: whether a daemon is running is a question about the daemon's
// identity (managed unit / pidfile ownership), and :11435 answers it wrong in
// both directions — see probeServeUp.
func (h resetHost) DialLocal(port int) bool { return h.ports[port] }
