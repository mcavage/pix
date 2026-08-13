//go:build unix

package sys

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestLock_TimesOutRatherThanHangingForever is the regression for a hang with no
// diagnosis. This was a plain blocking LOCK_EX, so a lock some other process
// held — or leaked — stopped the caller dead with nothing printed. `pix pack
// use` did exactly that: it registered every server, then went silent inside
// the post-commit wrapper refresh, which is the one part of that command
// explicitly allowed to fail with a note instead of taking the terminal.
//
// The point is not the duration. It is that the wait ENDS, with an error naming
// the lock file, so the caller has something to report and the user has
// something to run.
func TestLock_TimesOutRatherThanHangingForever(t *testing.T) {
	path := filepath.Join(t.TempDir(), "held.lock")
	holder, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)

	prev := flockWait
	flockWait = 150 * time.Millisecond
	defer func() { flockWait = prev }()

	ran := false
	done := make(chan error, 1)
	go func() { done <- Lock(path, func() error { ran = true; return nil }) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a lock held by another fd must not be handed out")
		}
		if !strings.Contains(err.Error(), path) {
			t.Errorf("the error must name the lock file so the user can find the holder, got: %v", err)
		}
		if ran {
			t.Error("the critical section ran without the lock")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Lock never returned — the unbounded wait is back")
	}
}

// TestLock_AcquiresOnceTheHolderReleases: bounding the wait must not turn
// ordinary contention into a failure. A caller that arrives while a short
// section is in flight still gets the lock, which is the whole reason the wait
// polls instead of refusing outright.
func TestLock_AcquiresOnceTheHolderReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "brief.lock")
	holder, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	prev := flockWait
	flockWait = 5 * time.Second
	defer func() { flockWait = prev }()

	done := make(chan error, 1)
	go func() { done <- Lock(path, func() error { return nil }) }()

	time.Sleep(100 * time.Millisecond)
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("a released lock must be acquired, not refused: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("waiter never acquired the released lock")
	}
}

// TestLock_ReportsTheCriticalSectionsOwnError: the bound is transport, not
// policy. fn's error still reaches the caller unwrapped by lock machinery.
func TestLock_ReportsTheCriticalSectionsOwnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "free.lock")
	want := errors.New("the section failed")
	if got := Lock(path, func() error { return want }); !errors.Is(got, want) {
		t.Errorf("Lock returned %v, want the section's own error", got)
	}
}
