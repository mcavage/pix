//go:build unix

// Advisory file locking (unix: linux + darwin, the only hosts pix-host runs on).
// acquireLock is the correctness primitive that makes every memory server and
// `restore` mutually exclusive around the sqlite store: all take an EXCLUSIVE
// flock on the SAME file (config.MemoryLockPath()), so restore can never swap
// the db out from under a running daemon — including the window where the
// daemon has opened the store but not yet bound its port, which a port probe
// alone leaves open.

package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"pix/host/config"
)

// acquireLock takes an EXCLUSIVE, NON-BLOCKING advisory flock on path (creating
// the lock file 0600 if absent). It NEVER blocks: an already-held lock returns
// an error immediately (EWOULDBLOCK/EAGAIN wrapped), so callers refuse cleanly
// instead of deadlocking. release drops the lock and closes the fd; idempotent,
// safe from a defer and a signal handler both.
func acquireLock(path string) (release func(), err error) {
	// A fresh install's memory dir may not exist (O_CREATE only creates the leaf
	// file), so the parent is ensured first — else a misleading ENOENT.
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create lock dir %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	// Stamp the holder INTO the lock file, now that we hold it. The flock
	// itself is anonymous: the kernel will tell a loser that someone holds the
	// lock and nothing more, so a holder that outlives its supervisor (a
	// go-plugin child of a SIGKILLed `pix serve`, which nothing reaps and
	// nothing can find) turns every later start into a dead end — the failure
	// is perfectly diagnosed and completely unactionable. A pid makes it a one
	// -line fix. It can only ever be read UNDER CONTENTION, i.e. while a real
	// holder exists, so it cannot go stale in a way that matters: a crashed
	// holder releases the lock, and whoever takes it next overwrites this.
	// Best-effort — failing to annotate a lock we already hold must never fail
	// the acquire.
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		})
	}, nil
}

// lockHolderHint reads the pid a CONTENDED lock file records (see acquireLock)
// and renders it as a sentence naming what to look at and what to do about it.
// Returns "" when there is no usable pid — an older holder that predates the
// stamp, a truncated write, or our own pid (which would send an operator after
// themselves). Purely advisory: it is appended to a refusal, never acted on.
func lockHolderHint(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 || pid == os.Getpid() {
		return ""
	}
	return fmt.Sprintf("\n  the lock file records pid %d as the holder — check it with `ps -p %d`;"+
		" if it is a memory server left behind by a hard-killed `pix serve`, stop it with `kill %d` and retry", pid, pid, pid)
}

// lockMemoryStoreOrFatal is the shared prologue every LIVE-SERVING memory entry
// point runs BEFORE opening the store, returning the release func to hold for
// the process lifetime. On failure it does NOT open the store, failing fast
// through fatal (MUST NOT return; pass a cleanup-aware one like serve's fatalf,
// nil defaults to log.Fatalf). acquireMemLockFn indirects so tests drive both
// paths hermetically.
var acquireMemLockFn = acquireLock

func lockMemoryStoreOrFatal(fatal func(format string, a ...any)) func() {
	if fatal == nil {
		fatal = log.Fatalf
	}
	path := config.MemoryLockPath()
	release, err := acquireMemLockFn(path)
	if err != nil {
		fatal("memory: could not acquire store lock at %s — another memory server or a restore is using the database, only one may hold it: %v%s", path, err, lockHolderHint(path))
		return func() {} // unreachable once fatal exits; a no-op keeps callers defer-safe
	}
	return release
}
