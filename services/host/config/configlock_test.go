//go:build unix

package config

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConfigLockPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))
	if got, want := ConfigLockPath(), filepath.Join(dir, ".config.lock"); got != want {
		t.Errorf("ConfigLockPath() = %q, want %q", got, want)
	}
}

// TestWithConfigLockMutualExclusion proves two goroutines racing on
// WithConfigLock never run their critical sections concurrently: each opens its
// OWN fd on the same lock file, so the exclusive advisory flock serializes them
// even in-process. A live-counter overlap detector catches any interleave.
func TestWithConfigLockMutualExclusion(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PI_STACK_CONFIG", filepath.Join(dir, "config.toml"))

	var live, maxLive int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := WithConfigLock(func() error {
				n := atomic.AddInt32(&live, 1)
				for {
					m := atomic.LoadInt32(&maxLive)
					if n <= m || atomic.CompareAndSwapInt32(&maxLive, m, n) {
						break
					}
				}
				time.Sleep(2 * time.Millisecond) // widen the overlap window
				atomic.AddInt32(&live, -1)
				return nil
			})
			if err != nil {
				t.Errorf("WithConfigLock: %v", err)
			}
		}()
	}
	wg.Wait()
	if maxLive != 1 {
		t.Errorf("max concurrent critical sections = %d, want 1", maxLive)
	}
}
