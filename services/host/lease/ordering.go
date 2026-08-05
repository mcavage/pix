//go:build unix

package lease

import (
	"context"
	"fmt"
)

func AttachRefUnderLifecycle(ctx context.Context, dir string, fn func() error) (*RefLease, error) {
	lc, err := OpenLifecycleLock(dir)
	if err != nil {
		return nil, err
	}
	defer lc.Close()
	if err := lc.AcquireExclusive(ctx); err != nil {
		return nil, fmt.Errorf("lease: attach %s: acquiring lifecycle lock: %w", dir, err)
	}
	defer lc.Unlock()

	if fn != nil {
		if err := fn(); err != nil {
			return nil, err
		}
	}

	rl, err := OpenRefLease(dir)
	if err != nil {
		return nil, fmt.Errorf("lease: attach %s: %w", dir, err)
	}
	if err := rl.AcquireShared(ctx); err != nil {
		rl.Close()
		return nil, fmt.Errorf("lease: attach %s: acquiring refs shared lock: %w", dir, err)
	}
	return rl, nil
}

// TryReapProof runs fn only when, under dir's lifecycle lock EXCLUSIVE, the refs
// lock's EXCLUSIVE can ALSO be proven non-blocking — zero live reference holders.
func TryReapProof(dir string, fn func() error) error {
	lc, err := OpenLifecycleLock(dir)
	if err != nil {
		return err
	}
	defer lc.Close()
	if err := lc.TryExclusive(); err != nil {
		return err
	}
	defer lc.Unlock()

	rl, err := OpenRefLease(dir)
	if err != nil {
		return err
	}
	defer rl.Close()
	if err := rl.TryExclusive(); err != nil {
		return err
	}
	defer rl.Unlock()
	return fn()
}
