package hosttrust

// LoadMutateSave is the fresh-load -> mutate -> save shape every trust
// document write uses: load a FRESH copy, apply mutate, then save it — so no
// caller can commit a stale in-memory object over a concurrent writer's
// record. It is meant to be called from INSIDE WithLock's fn (store.go), and
// it deliberately has NO lockPath parameter and imports NOTHING
// lock-related: this file has no way to reach sys.Lock, WithLock, or a flock
// of any kind, so nesting a second lock acquisition inside an
// already-locked mutation is impossible BY CONSTRUCTION — there is no lock
// acquisition anywhere in this function to nest. See
// TestLoadMutateSave_NeverAcquiresALock, which fails the day that stops being
// true rather than trusting this comment.
func LoadMutateSave[T any](load func() (T, error), mutate func(T) error, save func(T) error) (T, error) {
	var zero T
	fresh, err := load()
	if err != nil {
		return zero, err
	}
	if err := mutate(fresh); err != nil {
		return zero, err
	}
	if err := save(fresh); err != nil {
		return zero, err
	}
	return fresh, nil
}
