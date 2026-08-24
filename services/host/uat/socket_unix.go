//go:build unix

package uat

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

// checkOwnedByCurrentUser refuses a session socket directory owned by any uid
// other than the one running this process — the same "stranger's endpoint"
// standard supervise/service.go's verifyReattachTarget applies to a reattach
// socket.
func checkOwnedByCurrentUser(dir string, info os.FileInfo) error {
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("UAT socket directory %s: cannot verify owner on this platform", dir)
	}
	if int(sys.Uid) != os.Getuid() {
		return fmt.Errorf("UAT socket directory %s is not owned by uid %d", dir, os.Getuid())
	}
	return nil
}

// ListenSocket creates the session-owned Unix socket a UAT worker serves
// connections on, after hardening the path with ValidateSocketPath. A stale
// socket file left behind by a crashed worker (nothing answers a dial) is
// removed and replaced; one with a live listener on the other end aborts
// instead of silently displacing a running worker.
func ListenSocket(path string) (net.Listener, error) {
	if err := ValidateSocketPath(path); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("UAT worker socket path %s exists and is not a socket", path)
		}
		if socketIsLive(path) {
			return nil, fmt.Errorf("UAT worker socket %s already has an active listener", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove stale UAT worker socket: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat UAT worker socket path: %w", err)
	}

	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on UAT worker socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("secure UAT worker socket: %w", err)
	}
	return l, nil
}

func socketIsLive(path string) bool {
	c, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// DialSocket connects the uat-mcp gateway relay to a running uat-worker,
// hardening the path first and retrying up to attempts times (delay apart):
// the worker is a separate process started later by `pix run --dev`, so a
// short race on sandbox/session startup is expected. An absent worker after
// the bound is a real, actionable failure, not a transient one.
func DialSocket(path string, attempts int, delay time.Duration) (net.Conn, error) {
	if err := ValidateSocketPath(path); err != nil {
		return nil, err
	}
	if attempts < 1 {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		c, err := net.Dial("unix", path)
		if err == nil {
			return c, nil
		}
		lastErr = err
		if i < attempts-1 {
			time.Sleep(delay)
		}
	}
	return nil, fmt.Errorf("no UAT worker listening on %s after %d attempts: %w; start it with `pix run --dev`", path, attempts, lastErr)
}
