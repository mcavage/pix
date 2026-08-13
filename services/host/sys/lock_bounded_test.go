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

// TestLock_SaysItIsWaiting: a bounded wait that prints nothing is still
// indistinguishable from a hang, which is the confusion that made the
// unbounded version expensive to diagnose. Ten silent seconds during a real
// `pix setup` is what prompted this: the command was fine, and there was no way
// to know that from the terminal.
func TestLock_SaysItIsWaiting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "noisy.lock")
	holder, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(holder.Fd()), syscall.LOCK_UN)

	prevWait, prevNotice, prevAfter := flockWait, flockNotice, flockNoticeAfter
	var said []string
	flockWait, flockNoticeAfter = 300*time.Millisecond, 50*time.Millisecond
	flockNotice = func(m string) { said = append(said, m) }
	defer func() { flockWait, flockNotice, flockNoticeAfter = prevWait, prevNotice, prevAfter }()

	_ = Lock(path, func() error { return nil })

	if len(said) != 1 {
		t.Fatalf("expected exactly one waiting notice, got %d: %v", len(said), said)
	}
	if !strings.Contains(said[0], path) {
		t.Errorf("the notice must name the lock, got %q", said[0])
	}
}

// TestLock_SaysNothingWhenTheLockIsFree: the notice is for a wait, not for
// every lock. An uncontended section must stay silent, or every pix command
// grows a line of noise.
func TestLock_SaysNothingWhenTheLockIsFree(t *testing.T) {
	prev := flockNotice
	var said []string
	flockNotice = func(m string) { said = append(said, m) }
	defer func() { flockNotice = prev }()

	if err := Lock(filepath.Join(t.TempDir(), "quiet.lock"), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(said) != 0 {
		t.Errorf("an uncontended lock must say nothing, got %v", said)
	}
}
