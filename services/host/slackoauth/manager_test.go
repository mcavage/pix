package slackoauth

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// memStore is an in-memory Store fake with independently injectable Read and
// Write hooks, so tests can prove ordering/failure behavior precisely.
type memStore struct {
	mu       sync.Mutex
	blob     Blob
	writeErr error
	reads    int32
	writes   int32
}

func newMemStore(b Blob) *memStore { return &memStore{blob: b} }

func (s *memStore) Read(context.Context) (Blob, error) {
	atomic.AddInt32(&s.reads, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.blob, nil
}

func (s *memStore) Write(_ context.Context, b Blob) error {
	atomic.AddInt32(&s.writes, 1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.writeErr != nil {
		return s.writeErr
	}
	s.blob = b
	return nil
}

// mutexLocker is a simple in-process Locker for exercising Manager's
// single-flight refresh behavior; the real cross-process primitive (FileLock)
// is proven separately in lock_test.go.
type mutexLocker struct{ mu sync.Mutex }

func (l *mutexLocker) Lock(ctx context.Context) (func(), error) {
	done := make(chan struct{})
	go func() { l.mu.Lock(); close(done) }()
	select {
	case <-done:
		return func() { l.mu.Unlock() }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// countingRefresher wraps a refresh function and counts invocations, so
// concurrency tests can assert exactly one refresh happened.
type countingRefresher struct {
	calls int32
	fn    func(ctx context.Context, refreshToken string) (Blob, error)
}

func (c *countingRefresher) Refresh(ctx context.Context, refreshToken string) (Blob, error) {
	atomic.AddInt32(&c.calls, 1)
	return c.fn(ctx, refreshToken)
}

func freshBlob(now time.Time, validFor time.Duration) Blob {
	b := validBlob()
	b.AccessExpiresAt = now.Add(validFor)
	b.GrantExpiresAt = now.Add(30 * 24 * time.Hour)
	return b
}

// TestTokenServesCachedWhenFreshBeyondWindow proves a token with more than
// the 10-minute freshness window remaining is served straight from the store
// with no lock, no reread, and no refresh call.
func TestTokenServesCachedWhenFreshBeyondWindow(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newMemStore(freshBlob(now, 20*time.Minute))
	refresher := &countingRefresher{fn: func(context.Context, string) (Blob, error) {
		t.Fatal("Refresh must not be called when the cached token is fresh")
		return Blob{}, nil
	}}
	m := &Manager{Store: store, Client: refresher, Locker: &mutexLocker{}, Clock: &fakeClock{now: now}}

	tok, err := m.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != store.blob.AccessToken {
		t.Errorf("token = %q, want the cached access token", tok)
	}
	if atomic.LoadInt32(&refresher.calls) != 0 {
		t.Errorf("refresh calls = %d, want 0", refresher.calls)
	}
	if atomic.LoadInt32(&store.writes) != 0 {
		t.Errorf("writes = %d, want 0 (nothing needed writing)", store.writes)
	}
}

// TestTokenRefreshesWithinFreshnessWindow proves a token with LESS than 10
// minutes of remaining validity triggers a lock + reread + refresh + full
// blob write, and the newly refreshed token is returned.
func TestTokenRefreshesWithinFreshnessWindow(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newMemStore(freshBlob(now, 5*time.Minute))
	refreshed := freshBlob(now, time.Hour)
	refreshed.AccessToken = "xoxp-freshly-refreshed"
	refresher := &countingRefresher{fn: func(context.Context, string) (Blob, error) { return refreshed, nil }}
	m := &Manager{Store: store, Client: refresher, Locker: &mutexLocker{}, Clock: &fakeClock{now: now}}

	tok, err := m.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "xoxp-freshly-refreshed" {
		t.Errorf("token = %q, want the refreshed token", tok)
	}
	if atomic.LoadInt32(&refresher.calls) != 1 {
		t.Errorf("refresh calls = %d, want 1", refresher.calls)
	}
	if atomic.LoadInt32(&store.writes) != 1 {
		t.Errorf("writes = %d, want 1 (the whole refreshed blob)", store.writes)
	}
	if store.blob.AccessToken != "xoxp-freshly-refreshed" {
		t.Errorf("stored blob AccessToken = %q, want the refreshed token", store.blob.AccessToken)
	}
}

// TestTokenPreservesGrantExpiryAcrossRefresh proves the 30-day grant window
// is carried forward from the prior blob across a refresh, since Refresh
// itself does not know it.
func TestTokenPreservesGrantExpiryAcrossRefresh(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	original := freshBlob(now, 5*time.Minute)
	store := newMemStore(original)
	refreshed := freshBlob(now, time.Hour)
	refreshed.GrantExpiresAt = time.Time{} // Client.Refresh never sets this
	refresher := &countingRefresher{fn: func(context.Context, string) (Blob, error) { return refreshed, nil }}
	m := &Manager{Store: store, Client: refresher, Locker: &mutexLocker{}, Clock: &fakeClock{now: now}}

	if _, err := m.Token(context.Background()); err != nil {
		t.Fatalf("Token: %v", err)
	}
	if !store.blob.GrantExpiresAt.Equal(original.GrantExpiresAt) {
		t.Errorf("GrantExpiresAt = %v, want the original grant %v carried forward", store.blob.GrantExpiresAt, original.GrantExpiresAt)
	}
}

// TestTokenRejectsExpiredGrantWithoutRefreshing proves an expired 30-day
// grant is refused BEFORE ever calling Refresh — a dead grant must not
// attempt (and burn) a refresh call.
func TestTokenRejectsExpiredGrantWithoutRefreshing(t *testing.T) {
	now := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	b := validBlob()
	b.AccessExpiresAt = now.Add(-time.Hour)  // stale, forces the lock path
	b.GrantExpiresAt = now.Add(-time.Minute) // grant already expired
	store := newMemStore(b)
	refresher := &countingRefresher{fn: func(context.Context, string) (Blob, error) {
		t.Fatal("Refresh must not be called once the grant has expired")
		return Blob{}, nil
	}}
	m := &Manager{Store: store, Client: refresher, Locker: &mutexLocker{}, Clock: &fakeClock{now: now}}

	if _, err := m.Token(context.Background()); err == nil {
		t.Fatal("Token succeeded with an expired grant; want a refusal")
	}
	if atomic.LoadInt32(&refresher.calls) != 0 {
		t.Errorf("refresh calls = %d, want 0", refresher.calls)
	}
}

// TestTokenWriteFailureReturnsErrorNoToken proves that if persisting the
// refreshed blob fails, Token returns an error and an EMPTY token — never a
// token that isn't durably stored (the next reader would refresh again using
// a refresh_token Slack has already rotated away).
func TestTokenWriteFailureReturnsErrorNoToken(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newMemStore(freshBlob(now, time.Minute))
	store.writeErr = errWriteBoom
	refreshed := freshBlob(now, time.Hour)
	refresher := &countingRefresher{fn: func(context.Context, string) (Blob, error) { return refreshed, nil }}
	m := &Manager{Store: store, Client: refresher, Locker: &mutexLocker{}, Clock: &fakeClock{now: now}}

	tok, err := m.Token(context.Background())
	if err == nil {
		t.Fatal("Token succeeded despite a store write failure; want an error")
	}
	if tok != "" {
		t.Errorf("token = %q, want empty on a write failure", tok)
	}
}

var errWriteBoom = &opFakeErr{"simulated store write failure"}

// TestConcurrentTokenCallsRefreshExactlyOnce is the single-flight proof: many
// goroutines call Token concurrently against a stale blob; only the first to
// win the lock must call Refresh — everyone else must reread the (now fresh)
// store and return the same refreshed token without ever calling Refresh
// again.
func TestConcurrentTokenCallsRefreshExactlyOnce(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newMemStore(freshBlob(now, time.Minute)) // stale: forces every caller to the lock path
	refreshed := freshBlob(now, time.Hour)
	refreshed.AccessToken = "xoxp-refreshed-once"

	refresher := &countingRefresher{fn: func(context.Context, string) (Blob, error) {
		time.Sleep(20 * time.Millisecond) // widen the race window
		return refreshed, nil
	}}
	m := &Manager{Store: store, Client: refresher, Locker: &mutexLocker{}, Clock: &fakeClock{now: now}}

	const n = 20
	var wg sync.WaitGroup
	tokens := make([]string, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tokens[i], errs[i] = m.Token(context.Background())
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: Token: %v", i, err)
		}
		if tokens[i] != "xoxp-refreshed-once" {
			t.Errorf("goroutine %d token = %q, want xoxp-refreshed-once", i, tokens[i])
		}
	}
	if got := atomic.LoadInt32(&refresher.calls); got != 1 {
		t.Errorf("refresh calls = %d, want exactly 1", got)
	}
}

// TestTokenServesFromInProcessCacheWithoutStoreRead proves that once Token
// has observed a fresh blob, a subsequent call within the freshness window
// is served entirely from the in-process cache — no additional Store.Read at
// all, not merely a cheap one.
func TestTokenServesFromInProcessCacheWithoutStoreRead(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newMemStore(freshBlob(now, 20*time.Minute))
	refresher := &countingRefresher{fn: func(context.Context, string) (Blob, error) {
		t.Fatal("Refresh must not be called when the cached token is fresh")
		return Blob{}, nil
	}}
	m := &Manager{Store: store, Client: refresher, Locker: &mutexLocker{}, Clock: &fakeClock{now: now}}

	for i := 0; i < 5; i++ {
		tok, err := m.Token(context.Background())
		if err != nil {
			t.Fatalf("call %d: Token: %v", i, err)
		}
		if tok != store.blob.AccessToken {
			t.Errorf("call %d: token = %q, want the cached access token", i, tok)
		}
	}
	if got := atomic.LoadInt32(&store.reads); got != 1 {
		t.Errorf("store reads = %d, want exactly 1 (every call after the first must be served from the in-process cache)", got)
	}
}

// TestInvalidateClearsInProcessCache proves Invalidate forces the next Token
// call back through Store.Read even though the cached blob still looks
// fresh — the escape hatch a caller uses after direct evidence (e.g. the
// Slack API itself rejecting the token) that the cache is wrong.
func TestInvalidateClearsInProcessCache(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	store := newMemStore(freshBlob(now, 20*time.Minute))
	refresher := &countingRefresher{fn: func(context.Context, string) (Blob, error) {
		t.Fatal("Refresh must not be called; the store blob is still fresh after Invalidate")
		return Blob{}, nil
	}}
	m := &Manager{Store: store, Client: refresher, Locker: &mutexLocker{}, Clock: &fakeClock{now: now}}

	if _, err := m.Token(context.Background()); err != nil {
		t.Fatalf("first Token: %v", err)
	}
	if got := atomic.LoadInt32(&store.reads); got != 1 {
		t.Fatalf("store reads after first call = %d, want 1", got)
	}

	m.Invalidate()

	if _, err := m.Token(context.Background()); err != nil {
		t.Fatalf("second Token (post-invalidate): %v", err)
	}
	if got := atomic.LoadInt32(&store.reads); got != 2 {
		t.Errorf("store reads after Invalidate + second call = %d, want 2 (cache must have been dropped)", got)
	}
}
