package reset

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/service"
	"pix/host/sys"
)

// TestMain pins THREE things for the whole suite, all of them "never touch the
// developer's real machine":
//
//   - the liveness probe never asks whether THIS host has a managed pix service
//     loaded (on darwin that is a real `launchctl` call, and a loaded unit —
//     which the author of this package has — would make every test believe a
//     daemon is up). The pidfile half stays REAL, so "a daemon is up" means a
//     real pidfile naming a real live process, and nothing else.
//   - the stop never signals anything.
//   - the restart never spawns anything.
//
// Individual tests override these to model the state they are about.
func TestMain(m *testing.M) {
	probeServeUp = func(pidPath string, settle time.Duration) (bool, int) {
		return service.ServeIdentityUp(nil, pidPath, settle)
	}
	stopServeForReset = func(io.Writer) (bool, error) { return false, nil }
	restartServeForReset = func(io.Writer) error { return nil }
	os.Exit(m.Run())
}

// defaultCfg is a minimal Config carrying the fields these tests read.
func defaultCfg() *config.Config {
	return &config.Config{Services: []string{"memory"}, MCP: []string{"gog"}}
}

// stubServeProbe drives the liveness probe's answers IN SEQUENCE — one per call,
// which is the pre-stop "was a daemon running" question and then the post-stop
// "is it really gone" one — so a decision-table test can model a daemon state
// without a process. Calls past the last answer repeat it.
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

// stubStop replaces the mode-aware serve stop with a canned answer.
func stubStop(t *testing.T, stopped bool, err error) {
	t.Helper()
	orig := stopServeForReset
	stopServeForReset = func(io.Writer) (bool, error) { return stopped, err }
	t.Cleanup(func() { stopServeForReset = orig })
}

// stubRestart records whether the clean-slate restart fired.
func stubRestart(t *testing.T) *bool {
	t.Helper()
	fired := false
	orig := restartServeForReset
	restartServeForReset = func(io.Writer) error { fired = true; return nil }
	t.Cleanup(func() { restartServeForReset = orig })
	return &fired
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
	ran      *[]string         // every command Run executed, in order
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
	line := strings.Join(append([]string{name}, args...), " ")
	if h.ran != nil {
		*h.ran = append(*h.ran, line)
	}
	return h.output[line], nil
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

// stack is a populated fake host: a $HOME with a config dir, a data root
// (memory db, a pack, personal context) and a state dir (a pidfile, a lease
// record, and a TASK CHECKOUT with a file in it — the artifact that must
// survive a reset, see the package doc).
type stack struct {
	home      string
	configDir string
	dataRoot  string
	stateDir  string
	taskFile  string // <state>/tasks/repo/co/feature/WORK.txt
	env       resetHost
	sweeps    int
	sweepErr  error
	ran       []string
	out       bytes.Buffer
	errW      bytes.Buffer
}

func newStack(t *testing.T) *stack {
	t.Helper()
	home := t.TempDir()
	s := &stack{
		home:      home,
		configDir: filepath.Join(home, ".config", "pix"),
		dataRoot:  filepath.Join(home, ".local", "share", "pix"),
		stateDir:  filepath.Join(home, ".local", "state", "pix"),
	}
	s.taskFile = filepath.Join(s.stateDir, "tasks", "repo-abc", "co", "feature", "WORK.txt")
	write(t, filepath.Join(s.configDir, "config.toml"), "services = [\"memory\"]\n")
	write(t, filepath.Join(s.configDir, "pack-trust.json"), `{"version":1}`)
	write(t, filepath.Join(s.dataRoot, "memory", "memory.db"), "sqlite")
	write(t, filepath.Join(s.dataRoot, "packs", "work", "pack.toml"), "name = \"work\"\n")
	write(t, filepath.Join(s.dataRoot, "context", "AGENTS.md"), "# mine\n")
	write(t, filepath.Join(s.stateDir, "serve.pid"), "999999\n")
	write(t, filepath.Join(s.stateDir, "sandboxes", "pix-demo", "record.json"), "{}")
	write(t, s.taskFile, "uncommitted work\n")
	s.env = resetHost{
		home:     home,
		envVars:  map[string]string{},
		binaries: map[string]bool{},
		output:   map[string]string{},
		ports:    map[int]bool{},
		ran:      &s.ran,
	}
	return s
}

// setenv points a fake env var at this stack's host (e.g. MEMORY_DB).
func (s *stack) setenv(k, v string) { s.env.envVars[k] = v }

// runtime builds the injected Runtime: the sweep counts its calls and returns
// s.sweepErr, so a test asserts ordering and failure handling without sbx.
func (s *stack) runtime(tty bool, stdin string) Runtime {
	return Runtime{
		FS:   DefaultResetFS(),
		Env:  s.env,
		IO:   cli.IO{In: strings.NewReader(stdin), Out: &s.out, IsTTY: tty},
		ErrW: &s.errW,
		Sweep: func(io.Writer, io.Writer) error {
			s.sweeps++
			// The sweep reads the per-sandbox lease records; assert here, at the
			// moment it runs, that reset has not moved them out from under it.
			// s.stateDir is "" only for the deliberately-empty clean-machine
			// stack, which has no lease records to protect.
			if s.stateDir != "" && !exists(s.stateDir) {
				return fmt.Errorf("state dir already gone when the sandbox sweep ran")
			}
			return s.sweepErr
		},
		Now: func() time.Time { return time.Unix(1700000000, 0) },
	}
}

// run is the whole verb against this stack.
func (s *stack) run(t *testing.T, opts Opts, tty bool, stdin string) error {
	t.Helper()
	return Run(defaultCfg(), ResolvePaths(s.env), opts, s.runtime(tty, stdin))
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// backupOf returns the single <path>.bak-* sibling of path, failing if there is
// not exactly one.
func backupOf(t *testing.T, path string) string {
	t.Helper()
	matches, err := filepath.Glob(path + ".bak-*")
	if err != nil {
		t.Fatalf("glob %s.bak-*: %v", path, err)
	}
	if len(matches) != 1 {
		t.Fatalf("want exactly one backup of %s, got %v", path, matches)
	}
	return matches[0]
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
