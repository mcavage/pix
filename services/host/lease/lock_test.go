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

func mustOpenLease(t *testing.T) (*Lease, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "sbx-1")
	if _, err := CreateRecord(dir, "sbx-1"); err != nil {
		t.Fatalf("CreateRecord: %v", err)
	}
	l, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { l.Close() })
	return l, dir
}

func TestOpen_Creates0600LockFile(t *testing.T) {
	l, dir := mustOpenLease(t)
	fi, err := os.Stat(filepath.Join(dir, lockFileName))
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("lock file perm = %o, want 0600", perm)
	}
	if l.Path() != filepath.Join(dir, lockFileName) {
		t.Errorf("Path = %q", l.Path())
	}
}

func TestOpen_RefusesMissingDir(t *testing.T) {
	root := t.TempDir()
	if _, err := Open(filepath.Join(root, "never-created")); err == nil {
		t.Error("Open on a nonexistent dir = nil error, want error")
	}
}

func TestOpen_RefusesSymlinkDir(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "sbx-1")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if _, err := Open(link); err == nil {
		t.Error("Open on a symlinked dir = nil error, want refusal")
	}
}

func TestTryExclusive_SucceedsWithZeroHolders(t *testing.T) {
	l, _ := mustOpenLease(t)
	if err := l.TryExclusive(); err != nil {
		t.Fatalf("TryExclusive with zero holders = %v, want nil (the zero-holder proof)", err)
	}
}

func TestSharedThenSharedBothSucceed(t *testing.T) {
	dir := mustDir(t)
	a, err := Open(dir)
	if err != nil {
		t.Fatalf("Open a: %v", err)
	}
	defer a.Close()
	b, err := Open(dir)
	if err != nil {
		t.Fatalf("Open b: %v", err)
	}
	defer b.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.AcquireShared(ctx); err != nil {
		t.Fatalf("a.AcquireShared: %v", err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if err := b.AcquireShared(ctx2); err != nil {
		t.Fatalf("b.AcquireShared while a holds shared: %v, want success (many shared holders)", err)
	}
}

func TestTryExclusive_FailsWhileSharedHeld(t *testing.T) {
	dir := mustDir(t)
	holder, err := Open(dir)
	if err != nil {
		t.Fatalf("Open holder: %v", err)
	}
	defer holder.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := holder.AcquireShared(ctx); err != nil {
		t.Fatalf("AcquireShared: %v", err)
	}

	prober, err := Open(dir)
	if err != nil {
		t.Fatalf("Open prober: %v", err)
	}
	defer prober.Close()
	if err := prober.TryExclusive(); !errors.Is(err, ErrHeld) {
		t.Errorf("TryExclusive while shared held = %v, want ErrHeld", err)
	}
}

func TestTryExclusive_SucceedsAfterSharedReleased(t *testing.T) {
	dir := mustDir(t)
	holder, err := Open(dir)
	if err != nil {
		t.Fatalf("Open holder: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := holder.AcquireShared(ctx); err != nil {
		t.Fatalf("AcquireShared: %v", err)
	}
	if err := holder.Close(); err != nil { // releases the shared lock
		t.Fatalf("Close: %v", err)
	}

	prober, err := Open(dir)
	if err != nil {
		t.Fatalf("Open prober: %v", err)
	}
	defer prober.Close()
	if err := prober.TryExclusive(); err != nil {
		t.Errorf("TryExclusive after release = %v, want nil (zero-holder proof)", err)
	}
}

func TestAcquireExclusive_DeadlineExceededWhileHeld(t *testing.T) {
	dir := mustDir(t)
	holder, err := Open(dir)
	if err != nil {
		t.Fatalf("Open holder: %v", err)
	}
	defer holder.Close()
	if err := holder.TryExclusive(); err != nil {
		t.Fatalf("holder TryExclusive: %v", err)
	}

	waiter, err := Open(dir)
	if err != nil {
		t.Fatalf("Open waiter: %v", err)
	}
	defer waiter.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = waiter.AcquireExclusive(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("AcquireExclusive against a held exclusive lock = nil, want deadline error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("AcquireExclusive error = %v, want wrapping context.DeadlineExceeded", err)
	}
	if elapsed < 90*time.Millisecond {
		t.Errorf("AcquireExclusive returned after %v, want roughly the 100ms deadline", elapsed)
	}
	if elapsed > 2*time.Second {
		t.Errorf("AcquireExclusive returned after %v, deadline bound was not honored", elapsed)
	}
}

func TestAcquireShared_SucceedsOnceExclusiveReleased(t *testing.T) {
	dir := mustDir(t)
	holder, err := Open(dir)
	if err != nil {
		t.Fatalf("Open holder: %v", err)
	}
	if err := holder.TryExclusive(); err != nil {
		t.Fatalf("holder TryExclusive: %v", err)
	}

	waiter, err := Open(dir)
	if err != nil {
		t.Fatalf("Open waiter: %v", err)
	}
	defer waiter.Close()

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		done <- waiter.AcquireShared(ctx)
	}()

	time.Sleep(30 * time.Millisecond)
	if err := holder.Close(); err != nil {
		t.Fatalf("holder.Close: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("AcquireShared after release = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("AcquireShared did not return after the exclusive holder released")
	}
}
