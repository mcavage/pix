// Package sys is every seam between this program and the operating system,
// behind interfaces narrow enough that a function signature says what it
// touches.
package sys

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"pix/host/config"
)

// Exec runs other programs. Five methods rather than one because the CALLER's
// obligations differ: Run captures, RunTimed bounds an untrusted command,
// RunInteractive hands over the terminal — collapsing them would hide that.
type Exec interface {
	// LookPath resolves a binary on PATH.
	LookPath(name string) (string, error)
	// Run executes name and returns its combined output.
	Run(name string, args ...string) (string, error)
	// RunTimed executes an UNTRUSTED command with a hard timeout and capped output,
	// so a server can neither hang nor flood us; the second return marks a timeout.
	RunTimed(name string, args ...string) (out string, timedOut bool, err error)
	// RunWithin is RunTimed with a caller-chosen bound, for a probe that must be
	// tighter than the default (status' gog check allows 2s, not 5s).
	RunWithin(timeout time.Duration, name string, args ...string) (out string, timedOut bool, err error)
	// RunInteractive inherits the terminal (browser-based OAuth, `op signin`).
	RunInteractive(name string, args ...string) error
	// RunInteractiveQuiet keeps stdin attached but captures the command's
	// chatter, folding it into the error when the command fails.
	RunInteractiveQuiet(name string, args ...string) error
}

// FS is the filesystem. Lock lives here rather than in its own interface
// because an advisory file lock IS a filesystem operation, and a caller that
// needs one always needs the rest.
type FS interface {
	ReadFile(path string) (string, error)
	// WriteFile is leaf-symlink-safe: the destination is never opened directly,
	// so a leaf that is itself a symlink is REPLACED via an atomic
	// same-directory temp file + rename, never followed or truncated through.
	WriteFile(path string, data []byte, perm os.FileMode) error
	// IsFile reports whether a REGULAR file exists at path (a directory is not
	// a file).
	IsFile(path string) bool
	// Mode returns a path's mode bits and whether it exists — file OR dir. Used
	// to flag a group/other-readable secrets file.
	Mode(path string) (os.FileMode, bool)
	// Lock serializes a cross-process critical section on lockPath with an
	// advisory exclusive file lock, running fn while held.
	Lock(lockPath string, fn func() error) error
}

// Env is ambient process and user location: things with exactly one true answer
// per process that tests must nonetheless be able to relocate.
type Env interface {
	Getenv(name string) string
	HomeDir() string
	Getwd() (string, error)
	// StateDir is the launcher's own writable state directory.
	StateDir() (string, error)
	// Executable is the path of the running binary.
	Executable() (string, error)
}

// Net is the network. One method, deliberately: the only thing this program
// asks the network at the OS level is "is a local service listening". Anything
// richer is a domain concern with its own client.
type Net interface {
	// DialLocal reports whether something accepts TCP on 127.0.0.1:port within
	// a short, fixed timeout.
	DialLocal(port int) bool
}

// System is every seam at once. Production takes this; a function that needs
// less should narrow its own parameter to Exec, FS, Env or Net — that is the
// whole point of the split, and it costs nothing at the call site.
type System interface {
	Exec
	FS
	Env
	Net
}

// Real is the production System: its zero value is fully functional and it holds NO
// nullable state — the property that makes nil guards in callers unnecessary.
type Real struct{}

const dialTimeout = 400 * time.Millisecond

func (Real) LookPath(name string) (string, error) { return exec.LookPath(name) }

func (Real) Run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

func (Real) RunInteractive(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func (Real) RunInteractiveQuiet(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Stdin = os.Stdin
	out, err := cmd.CombinedOutput()
	if err != nil && strings.TrimSpace(string(out)) != "" {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return err
}

func (Real) ReadFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}

func (Real) IsFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func (Real) Mode(path string) (os.FileMode, bool) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	return fi.Mode(), true
}

func (Real) Getenv(name string) string { return os.Getenv(name) }

func (Real) HomeDir() string {
	h, _ := os.UserHomeDir()
	return h
}

func (Real) Getwd() (string, error) { return os.Getwd() }

func (Real) Executable() (string, error) { return os.Executable() }

// StateDir delegates to config, the one owner of the launcher's data layout.
func (Real) StateDir() (string, error) { return config.StateDir() }

func (Real) DialLocal(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), dialTimeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func (Real) WriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return AtomicWriteInDir(dir, filepath.Base(path), data, perm)
}

func (Real) Lock(lockPath string, fn func() error) error { return withFlock(lockPath, nil, fn) }

func (Real) RunTimed(name string, args ...string) (string, bool, error) {
	return RunTimed(ProbeTimeout, name, args...)
}

func (Real) RunWithin(d time.Duration, name string, args ...string) (string, bool, error) {
	return RunTimed(d, name, args...)
}

// compile-time proof that Real is a complete System.
var _ System = Real{}

// Getenver is the one-method view of Env, for the many functions that read a
// single variable and nothing else. Narrowing a parameter to this is the whole
// point of splitting the interfaces: `func servePort(env sys.Getenver, ...)`
type Getenver interface{ Getenv(name string) string }

// GetenvFunc adapts a bare lookup function to Getenver. It has no nil case on
// purpose: a nil GetenvFunc is a programming error that panics at the call.
type GetenvFunc func(name string) string

func (f GetenvFunc) Getenv(name string) string { return f(name) }
