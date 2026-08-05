//go:build unix

package lease

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenLifecycleLock_Creates0600LockFile(t *testing.T) {
	dir := mustDir(t)
	lc, err := OpenLifecycleLock(dir)
	if err != nil {
		t.Fatalf("OpenLifecycleLock: %v", err)
	}
	defer lc.Close()
	fi, err := os.Stat(filepath.Join(dir, lifecycleLockFileName))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("lifecycle lock file perm = %o, want 0600", perm)
	}
	if lc.Path() != filepath.Join(dir, lifecycleLockFileName) {
		t.Errorf("Path = %q", lc.Path())
	}
	// Separate file from refs.lock: a lifecycle lock must not collide with
	// (or borrow) the refs lock's identity.
	if lifecycleLockFileName == refsLockFileName {
		t.Fatal("lifecycle and refs lock files must have distinct names")
	}
}

func TestOpenLifecycleLock_RefusesMissingDir(t *testing.T) {
	root := t.TempDir()
	if _, err := OpenLifecycleLock(filepath.Join(root, "never-created")); err == nil {
		t.Error("OpenLifecycleLock on a nonexistent dir = nil error, want error")
	}
}

func TestOpenLifecycleLock_RefusesSymlinkDir(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "sbx-1")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if _, err := OpenLifecycleLock(link); err == nil {
		t.Error("OpenLifecycleLock on a symlinked dir = nil error, want refusal")
	}
}

func TestLifecycleLock_TryExclusive_SucceedsWithZeroHolders(t *testing.T) {
	dir := mustDir(t)
	lc, err := OpenLifecycleLock(dir)
	if err != nil {
		t.Fatalf("OpenLifecycleLock: %v", err)
	}
	defer lc.Close()
	if err := lc.TryExclusive(); err != nil {
		t.Fatalf("TryExclusive with zero holders = %v, want nil", err)
	}
}

func TestLifecycleLock_TryExclusive_FailsWhileHeld(t *testing.T) {
	dir := mustDir(t)
	holder, err := OpenLifecycleLock(dir)
	if err != nil {
		t.Fatalf("OpenLifecycleLock holder: %v", err)
	}
	defer holder.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := holder.AcquireExclusive(ctx); err != nil {
		t.Fatalf("holder AcquireExclusive: %v", err)
	}

	prober, err := OpenLifecycleLock(dir)
	if err != nil {
		t.Fatalf("OpenLifecycleLock prober: %v", err)
	}
	defer prober.Close()
	if err := prober.TryExclusive(); !errors.Is(err, ErrHeld) {
		t.Errorf("TryExclusive while held = %v, want ErrHeld", err)
	}
}

func TestLifecycleLock_AcquireExclusive_DeadlineExceededWhileHeld(t *testing.T) {
	dir := mustDir(t)
	holder, err := OpenLifecycleLock(dir)
	if err != nil {
		t.Fatalf("OpenLifecycleLock holder: %v", err)
	}
	defer holder.Close()
	if err := holder.TryExclusive(); err != nil {
		t.Fatalf("holder TryExclusive: %v", err)
	}

	waiter, err := OpenLifecycleLock(dir)
	if err != nil {
		t.Fatalf("OpenLifecycleLock waiter: %v", err)
	}
	defer waiter.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = waiter.AcquireExclusive(ctx)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("AcquireExclusive error = %v, want wrapping context.DeadlineExceeded", err)
	}
	if elapsed < 90*time.Millisecond {
		t.Errorf("AcquireExclusive returned after %v, want roughly the 100ms deadline", elapsed)
	}
}

func TestLifecycleLock_AcquireExclusive_SucceedsOnceReleased(t *testing.T) {
	dir := mustDir(t)
	holder, err := OpenLifecycleLock(dir)
	if err != nil {
		t.Fatalf("OpenLifecycleLock holder: %v", err)
	}
	if err := holder.TryExclusive(); err != nil {
		t.Fatalf("holder TryExclusive: %v", err)
	}

	waiter, err := OpenLifecycleLock(dir)
	if err != nil {
		t.Fatalf("OpenLifecycleLock waiter: %v", err)
	}
	defer waiter.Close()

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		done <- waiter.AcquireExclusive(ctx)
	}()

	time.Sleep(30 * time.Millisecond)
	if err := holder.Close(); err != nil {
		t.Fatalf("holder.Close: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("AcquireExclusive after release = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AcquireExclusive did not return after the exclusive holder released")
	}
}
