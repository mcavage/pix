// Package systest is the ONE test double for sys.System.
package systest

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"pix/host/sys"
)

// Fake is a sys.System whose behaviour a test declares field by field. Anything
// left nil refuses when called.
type Fake struct {
	LookPathFn            func(name string) (string, error)
	RunFn                 func(name string, args ...string) (string, error)
	RunTimedFn            func(name string, args ...string) (string, bool, error)
	RunWithinFn           func(d time.Duration, name string, args ...string) (string, bool, error)
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

	// Base, when set, answers any method this fixture did not wire, instead of
	// refusing — for a test that wants "the real OS, except these two seams".
	Base sys.System

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
		if f.Base != nil {
			return f.Base.LookPath(name)
		}
		return "", unwired("LookPath")
	}
	return f.LookPathFn(name)
}

func (f *Fake) Run(name string, args ...string) (string, error) {
	f.record(name, args)
	if f.RunFn == nil {
		if f.Base != nil {
			return f.Base.Run(name, args...)
		}
		return "", unwired("Run")
	}
	return f.RunFn(name, args...)
}

// RunTimed falls back to Run when only Run is wired. The ONE convenience fallback,
// and a safe one: it substitutes a wired seam for an unwired one, never silence.
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

// RunWithin falls back to RunTimed: a fixture almost never cares about the
// bound, only about the answer, and making every fixture wire two seams to test
// one behaviour is how fixtures end up with gaps.
func (f *Fake) RunWithin(d time.Duration, name string, args ...string) (string, bool, error) {
	if f.RunWithinFn == nil {
		return f.RunTimed(name, args...)
	}
	f.record(name, args)
	return f.RunWithinFn(d, name, args...)
}

func (f *Fake) RunInteractive(name string, args ...string) error {
	f.record(name, args)
	if f.RunInteractiveFn == nil {
		if f.Base != nil {
			return f.Base.RunInteractive(name, args...)
		}
		return unwired("RunInteractive")
	}
	return f.RunInteractiveFn(name, args...)
}

func (f *Fake) RunInteractiveQuiet(name string, args ...string) error {
	f.record(name, args)
	if f.RunInteractiveQuietFn == nil {
		if f.Base != nil {
			return f.Base.RunInteractiveQuiet(name, args...)
		}
		return unwired("RunInteractiveQuiet")
	}
	return f.RunInteractiveQuietFn(name, args...)
}

func (f *Fake) ReadFile(path string) (string, error) {
	if f.ReadFileFn == nil {
		if f.Base != nil {
			return f.Base.ReadFile(path)
		}
		// os.ErrNotExist, not a refusal: "this fixture declares no files" is a
		// complete and common answer, and every caller already handles it.
		return "", os.ErrNotExist
	}
	return f.ReadFileFn(path)
}

func (f *Fake) WriteFile(path string, data []byte, perm os.FileMode) error {
	if f.WriteFileFn == nil {
		if f.Base != nil {
			return f.Base.WriteFile(path, data, perm)
		}
		return unwired("WriteFile")
	}
	return f.WriteFileFn(path, data, perm)
}

func (f *Fake) IsFile(path string) bool {
	if f.IsFileFn == nil {
		if f.Base != nil {
			return f.Base.IsFile(path)
		}
		return false // "no such file" is a complete answer; see ReadFile.
	}
	return f.IsFileFn(path)
}

func (f *Fake) Mode(path string) (os.FileMode, bool) {
	if f.ModeFn == nil {
		if f.Base != nil {
			return f.Base.Mode(path)
		}
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
		if f.Base != nil {
			return f.Base.Getenv(name)
		}
		return "" // an unset variable is a complete answer.
	}
	return f.GetenvFn(name)
}

func (f *Fake) HomeDir() string {
	if f.HomeDirFn == nil {
		if f.Base != nil {
			return f.Base.HomeDir()
		}
		return ""
	}
	return f.HomeDirFn()
}

func (f *Fake) Getwd() (string, error) {
	if f.GetwdFn == nil {
		if f.Base != nil {
			return f.Base.Getwd()
		}
		return "", unwired("Getwd")
	}
	return f.GetwdFn()
}

// StateDir resolves for real when unwired, because it is an OVERRIDE seam rather
// than a required one: config.StateDir() is driven by $XDG_STATE_HOME.
func (f *Fake) StateDir() (string, error) {
	if f.StateDirFn == nil {
		if f.Base != nil {
			return f.Base.StateDir()
		}
		return sys.Real{}.StateDir()
	}
	return f.StateDirFn()
}

func (f *Fake) Executable() (string, error) {
	if f.ExecutableFn == nil {
		if f.Base != nil {
			return f.Base.Executable()
		}
		return "", unwired("Executable")
	}
	return f.ExecutableFn()
}

func (f *Fake) DialLocal(port int) bool {
	if f.DialLocalFn == nil {
		if f.Base != nil {
			return f.Base.DialLocal(port)
		}
		return false // "nothing is listening" is a complete answer.
	}
	return f.DialLocalFn(port)
}

// compile-time proof that Fake is a complete System, so a method added to the
// interface breaks here rather than at 205 call sites.
var _ sys.System = (*Fake)(nil)

// Of asserts that s is a Fake and returns it, for a fixture that overrides one seam
// of a base env. TEST-ONLY: it panics on a real System, deliberately.
func Of(s sys.System) *Fake { return s.(*Fake) }

// --- lock observation ---------------------------------------------------
type LockRecorder struct {
	fatalf func(string, ...any)
	mu     sync.Mutex
	depth  map[string]int
	events *[]string
}

func NewLockRecorder(fatalf func(string, ...any), events *[]string) *LockRecorder {
	return &LockRecorder{fatalf: fatalf, depth: map[string]int{}, events: events}
}

// Lock is the sys.System-shaped method; assign it to Fake.LockFn.
func (l *LockRecorder) Lock(path string, fn func() error) error {
	l.mu.Lock()
	if l.depth[path] > 0 {
		l.mu.Unlock()
		l.fatalf("nested flock acquisition on %s — a real flock would deadlock here (use a *Locked variant)", path)
	}
	l.depth[path]++
	*l.events = append(*l.events, "acquire "+path)
	l.mu.Unlock()
	err := fn()
	l.mu.Lock()
	l.depth[path]--
	*l.events = append(*l.events, "release "+path)
	l.mu.Unlock()
	return err
}

// LockWindow returns the index of the first acquire and last release of
// lockPath in events; (-1, -1) if either is missing.
func LockWindow(events []string, lockPath string) (first, last int) {
	first, last = -1, -1
	for i, e := range events {
		if e == "acquire "+lockPath && first < 0 {
			first = i
		}
		if e == "release "+lockPath {
			last = i
		}
	}
	return first, last
}

// CountEvents counts events with the given prefix.
func CountEvents(events []string, prefix string) int {
	n := 0
	for _, e := range events {
		if strings.HasPrefix(e, prefix) {
			n++
		}
	}
	return n
}
