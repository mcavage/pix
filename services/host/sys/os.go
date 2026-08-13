package sys

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Probe budgets for RunTimed. A registered MCP server is UNTRUSTED input: it
// can hang and it can flood, so both are bounded rather than trusted.
const (
	ProbeTimeout   = 5 * time.Second
	ProbeMaxOutput = 64 << 10 // 64KB
)

// RunTimed runs name with a caller-chosen deadline and a capped output. It is
// exported with an explicit timeout so a fast caller (status' auth probe) can
// bound itself tighter than ProbeTimeout without a second implementation.
func RunTimed(timeout time.Duration, name string, args ...string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	// Hard wall-clock bound: if the child (or a descendant it spawned that still
	// holds stdout/stderr) is alive when the context fires, WaitDelay forces the
	// pipes closed and the process killed, so CombinedOutput cannot hang past it.
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if len(out) > ProbeMaxOutput {
		out = out[:ProbeMaxOutput]
	}
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), true, ctx.Err()
	}
	return string(out), false, err
}

// AtomicWriteInDir writes data to dir/name atomically, and is the ONE hardened
// writer in the tree (config saves, workspace state, secret refs all route here).
func AtomicWriteInDir(dir, name string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(dir, name+".tmp-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	fail := func(err error) error {
		tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	// chmod the REQUESTED mode before Sync (CreateTemp makes 0600): fchmod on the
	// open handle, so the fsync flushes data AND metadata with the mode in place.
	if err := tmp.Chmod(perm); err != nil {
		return fail(err)
	}
	// fsync before rename: the write must be durable before the atomic rename makes
	// it visible, so a crash can never expose a truncated file.
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// Lock is the package-level form of FS.Lock, for the few callers that hold a
// lock without holding a System (the serve supervisor takes its spawn lock
// before any command context exists).
func Lock(lockPath string, fn func() error) error { return withFlock(lockPath, nil, fn) }

// LockNotifying is Lock for a caller that has somewhere to narrate: notify is
// called ONCE if the lock is not available promptly, with a line naming it.
//
// The notifier is a PARAMETER rather than a package var because this is a leaf:
// a function-valued global here is a dependency seam, which is an L3/L4
// technique and not a leaf's to own (wiring_test.go enforces it), and printing
// from here would put a terminal stream in a package whose only sanctioned
// stream use is handing a terminal to a child process (processboundary_test.go
// enforces that one). Both rules point the same way — the layer that owns a
// writer owns the message.
func LockNotifying(lockPath string, notify func(string), fn func() error) error {
	return withFlock(lockPath, notify, fn)
}
