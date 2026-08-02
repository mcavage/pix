// Package systest is the ONE test double for sys.System.
//
// Its fields are nullable, and that is deliberate: this is the single place in
// the tree where "not wired" is a legitimate state, because a test that does
// not care about the filesystem should not have to fake one. What makes it safe
// — and what the old shellEnv got wrong — is that absence here is LOUD. Calling
// an unwired method returns an error naming the method, so a fixture gap
// surfaces as a failing test with an actionable message instead of a zero value
// that reads like a real answer.
//
// The three bugs in docs/design/rearchitecture.md were all fixture gaps that
// looked like results: verification that never ran and reported "0 verified",
// a setup path whose hard-error branch was unreachable because the probe was
// nil. Under this type, each of those is a test failure that says
// `systest.Fake: RunTimed is not wired`.
package systest

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"pix/host/sys"
)

// Fake is a sys.System whose behaviour a test declares field by field. Anything
// left nil refuses when called.
//
// It is a struct of funcs rather than an interface-per-test because the shape a
// test wants is "the real world, except X": listing the one seam that matters
// beside a zero value for the rest is the readable form of that, and it is what
// the 205 existing fixtures already say.
type Fake struct {
	LookPathFn            func(name string) (string, error)
	RunFn                 func(name string, args ...string) (string, error)
	RunTimedFn            func(name string, args ...string) (string, bool, error)
	RunInteractiveFn      func(name string, args ...string) error
	RunInteractiveQuietFn func(name string, args ...string) error

	ReadFileFn  func(path string) (string, error)
	WriteFileFn func(path string, data []byte, perm os.FileMode) error
	IsFileFn    func(path string) bool
	ModeFn      func(path string) (os.FileMode, bool)
	LockFn      func(lockPath string, fn func() error) error

	GetenvFn     func(name string) string
	HomeDirFn    func() string
	GetwdFn      func() (string, error)
	StateDirFn   func() (string, error)
	ExecutableFn func() (string, error)

	DialLocalFn func(port int) bool

	// Calls records every command Run/RunTimed/RunInteractive* was asked to
	// execute, in order, as "name arg arg". Recording is built in because
	// roughly half the fixtures in this tree hand-roll the same slice.
	mu    sync.Mutex
	Calls []string
}

// unwired is the loud refusal. It names the method AND the fix, because the
// reader is a developer looking at a test failure, not a user.
func unwired(method string) error {
	return fmt.Errorf("systest.Fake: %s is not wired — set %sFn on the fixture, or use a helper that does", method, method)
}

func (f *Fake) record(name string, args []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))
}

// Ran reports whether any recorded call begins with prefix.
func (f *Fake) Ran(prefix string) bool { return f.Count(prefix) > 0 }

// Count is how many recorded calls begin with prefix.
func (f *Fake) Count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.Calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func (f *Fake) LookPath(name string) (string, error) {
	if f.LookPathFn == nil {
		return "", unwired("LookPath")
	}
	return f.LookPathFn(name)
}

func (f *Fake) Run(name string, args ...string) (string, error) {
	f.record(name, args)
	if f.RunFn == nil {
		return "", unwired("Run")
	}
	return f.RunFn(name, args...)
}

// RunTimed falls back to Run when only Run is wired. This is the ONE
// convenience fallback, and it is safe in a way the old `probeRun` was not: it
// substitutes a wired seam for an unwired one, never silence for an answer. A
// fixture with neither still refuses.
func (f *Fake) RunTimed(name string, args ...string) (string, bool, error) {
	if f.RunTimedFn == nil {
		if f.RunFn == nil {
			f.record(name, args)
			return "", false, unwired("RunTimed")
		}
		out, err := f.Run(name, args...)
		return out, false, err
	}
	f.record(name, args)
	return f.RunTimedFn(name, args...)
}

func (f *Fake) RunInteractive(name string, args ...string) error {
	f.record(name, args)
	if f.RunInteractiveFn == nil {
		return unwired("RunInteractive")
	}
	return f.RunInteractiveFn(name, args...)
}

func (f *Fake) RunInteractiveQuiet(name string, args ...string) error {
	f.record(name, args)
	if f.RunInteractiveQuietFn == nil {
		return unwired("RunInteractiveQuiet")
	}
	return f.RunInteractiveQuietFn(name, args...)
}

func (f *Fake) ReadFile(path string) (string, error) {
	if f.ReadFileFn == nil {
		// os.ErrNotExist, not a refusal: "this fixture declares no files" is a
		// complete and common answer, and every caller already handles it.
		return "", os.ErrNotExist
	}
	return f.ReadFileFn(path)
}

func (f *Fake) WriteFile(path string, data []byte, perm os.FileMode) error {
	if f.WriteFileFn == nil {
		return unwired("WriteFile")
	}
	return f.WriteFileFn(path, data, perm)
}

func (f *Fake) IsFile(path string) bool {
	if f.IsFileFn == nil {
		return false // "no such file" is a complete answer; see ReadFile.
	}
	return f.IsFileFn(path)
}

func (f *Fake) Mode(path string) (os.FileMode, bool) {
	if f.ModeFn == nil {
		return 0, false
	}
	return f.ModeFn(path)
}

// Lock runs fn directly when unwired. A hermetic test must not create a real
// lock file, and running the critical section unlocked is exactly right for a
// single-goroutine test.
func (f *Fake) Lock(lockPath string, fn func() error) error {
	if f.LockFn == nil {
		return fn()
	}
	return f.LockFn(lockPath, fn)
}

func (f *Fake) Getenv(name string) string {
	if f.GetenvFn == nil {
		return "" // an unset variable is a complete answer.
	}
	return f.GetenvFn(name)
}

func (f *Fake) HomeDir() string {
	if f.HomeDirFn == nil {
		return ""
	}
	return f.HomeDirFn()
}

func (f *Fake) Getwd() (string, error) {
	if f.GetwdFn == nil {
		return "", unwired("Getwd")
	}
	return f.GetwdFn()
}

func (f *Fake) StateDir() (string, error) {
	if f.StateDirFn == nil {
		return "", unwired("StateDir")
	}
	return f.StateDirFn()
}

func (f *Fake) Executable() (string, error) {
	if f.ExecutableFn == nil {
		return "", unwired("Executable")
	}
	return f.ExecutableFn()
}

func (f *Fake) DialLocal(port int) bool {
	if f.DialLocalFn == nil {
		return false // "nothing is listening" is a complete answer.
	}
	return f.DialLocalFn(port)
}

// compile-time proof that Fake is a complete System, so a method added to the
// interface breaks here rather than at 205 call sites.
var _ sys.System = (*Fake)(nil)
