//go:build unix

package slackoauth

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestFileLockExclusiveBlocksUntilReleased proves the production Locker
// serializes two lockers over the SAME path: the second Lock call does not
// return until the first releases.
func TestFileLockExclusiveBlocksUntilReleased(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".slackoauth.lock")
	l1 := &FileLock{Path: path, PollInterval: 5 * time.Millisecond}
	l2 := &FileLock{Path: path, PollInterval: 5 * time.Millisecond}

	unlock1, err := l1.Lock(context.Background())
	if err != nil {
		t.Fatalf("first Lock: %v", err)
	}

	var mu sync.Mutex
	acquired2 := false
	done := make(chan struct{})
	go func() {
		unlock2, err := l2.Lock(context.Background())
		if err != nil {
			t.Errorf("second Lock: %v", err)
			close(done)
			return
		}
		mu.Lock()
		acquired2 = true
		mu.Unlock()
		unlock2()
		close(done)
	}()

	time.Sleep(40 * time.Millisecond)
	mu.Lock()
	stillWaiting := !acquired2
	mu.Unlock()
	if !stillWaiting {
		t.Fatal("second Lock acquired while the first still held it")
	}

	unlock1()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("second Lock never acquired after release")
	}
}

// TestFileLockContextCancellation proves a blocked Lock call gives up
// promptly (returning ctx.Err()) once its context is cancelled, instead of
// hanging until the holder releases.
func TestFileLockContextCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".slackoauth.lock")
	l1 := &FileLock{Path: path, PollInterval: 5 * time.Millisecond}
	l2 := &FileLock{Path: path, PollInterval: 5 * time.Millisecond}

	unlock1, err := l1.Lock(context.Background())
	if err != nil {
		t.Fatalf("first Lock: %v", err)
	}
	defer unlock1()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = l2.Lock(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("second Lock succeeded while the first still held it")
	}
	if elapsed > 2*time.Second {
		t.Errorf("Lock took %v to give up after a 50ms timeout", elapsed)
	}
}

// TestFileLockReleaseIdempotent proves the returned unlock func is safe to
// call more than once.
func TestFileLockReleaseIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".slackoauth.lock")
	l := &FileLock{Path: path}
	unlock, err := l.Lock(context.Background())
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	unlock()
	unlock() // must not panic
}

// TestFileLockRequiresPath proves a misconfigured FileLock fails clearly
// rather than locking an empty path.
func TestFileLockRequiresPath(t *testing.T) {
	l := &FileLock{}
	if _, err := l.Lock(context.Background()); err == nil {
		t.Fatal("Lock succeeded with an empty Path; want a configuration error")
	}
}
