//go:build !unix

// FileLock (non-unix): there is no syscall.Flock here, and pulling in a
// Windows-specific locking API is not worth a new module dependency for a
// platform pix-host does not ship the daemon on (mirrors the same tradeoff
// pix-host's own lock_windows.go makes for the memory store). This falls
// back to a same-process, in-memory mutex keyed by Path: within one process
// the refresh critical section is still correctly serialized. It does NOT
// protect against a second OS process racing the same 1Password item on this
// platform.

package slackoauth

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// FileLock is a Locker. On this platform it degrades to an in-process mutex
// per Path; see the package-level comment above.
type FileLock struct {
	// Path identifies the lock; kept for API parity with the unix
	// implementation (and still used to touch a lock file on disk, so a
	// mixed-platform deployment can see the same path).
	Path string
	// PollInterval is unused on this platform; kept for API parity.
	PollInterval interface{}
}

var (
	fileLockMu    sync.Mutex
	fileLockLocks = map[string]*sync.Mutex{}
)

// Lock acquires the in-process mutex for Path, honoring ctx cancellation.
func (l *FileLock) Lock(ctx context.Context) (func(), error) {
	if l.Path == "" {
		return nil, fmt.Errorf("slackoauth: FileLock.Path is required")
	}
	if dir := filepath.Dir(l.Path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("slackoauth: create lock dir %s: %w", dir, err)
		}
	}
	if _, err := os.OpenFile(l.Path, os.O_CREATE|os.O_RDWR, 0o600); err == nil {
		// best-effort: just ensure the path exists for parity; we don't keep
		// the fd open since no real OS-level lock is taken on this platform.
	}

	fileLockMu.Lock()
	mu, ok := fileLockLocks[l.Path]
	if !ok {
		mu = &sync.Mutex{}
		fileLockLocks[l.Path] = mu
	}
	fileLockMu.Unlock()

	done := make(chan struct{})
	go func() {
		mu.Lock()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		// sync.Mutex has no cancelable Lock: let the goroutine finish
		// acquiring eventually and release immediately, so it isn't leaked
		// held forever.
		go func() {
			<-done
			mu.Unlock()
		}()
		return nil, fmt.Errorf("slackoauth: lock %s: %w", l.Path, ctx.Err())
	}

	var once sync.Once
	return func() {
		once.Do(func() { mu.Unlock() })
	}, nil
}
