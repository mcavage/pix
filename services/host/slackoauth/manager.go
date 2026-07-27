package slackoauth

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// Store persists and retrieves the whole credential Blob. OPStore is the
// production implementation.
type Store interface {
	Read(ctx context.Context) (Blob, error)
	Write(ctx context.Context, b Blob) error
}

// Locker serializes the refresh critical section (reread -> refresh ->
// write) across every caller sharing the same underlying credential — across
// goroutines in one process, and (with FileLock) across processes. Lock
// blocks until acquired, ctx is done, or an error occurs; the returned func
// releases it and must be safe to call more than once.
type Locker interface {
	Lock(ctx context.Context) (unlock func(), err error)
}

// RefreshClient is the slice of Client's API the Manager depends on, so
// tests can inject a fake refresher without standing up an HTTP client.
type RefreshClient interface {
	Refresh(ctx context.Context, refreshToken string) (Blob, error)
}

// defaultFreshWindow is how much remaining access-token validity is required
// to serve a cached token without taking the lock at all.
const defaultFreshWindow = 10 * time.Minute

// ErrGrantExpired is returned when the stored blob's 30-day rotating grant
// has already expired: no refresh is attempted (a dead grant can't be
// refreshed, and attempting one would just burn a doomed call), and the
// caller must drive a full re-authorization (a fresh Client.Exchange).
var ErrGrantExpired = errors.New("slackoauth: refresh grant has expired; a full re-authorization is required")

// Manager is the single entry point for "give me a valid Slack access
// token": it serves a cached token when it has more than FreshWindow left,
// and otherwise serializes a reread-then-maybe-refresh-then-write critical
// section behind Locker so concurrent callers trigger at most one refresh.
// It NEVER returns a token that was not durably written first — if the write
// fails, Token returns an error and an empty string, never the token that
// only exists in memory.
// Manager caches the last-known-good Blob IN PROCESS (protected by mu) so a
// hot path (many tool calls in a row, all within the freshness window) never
// touches Store.Read at all — not just "cheaply", but not at ALL, since a
// 1Password read is a real subprocess exec (OPStore -> `op document get`)
// and this is called on every Slack API call. The cache is populated any
// time Token observes a fresh blob (from the initial read, a reread under
// the lock, or a fresh refresh), and Invalidate drops it so the very next
// Token call is forced back through Store.Read (and, if still stale, the
// lock+reread+refresh path) — used after the caller sees a token_expired/
// invalid_auth response that proves the cached token is no longer good
// despite looking unexpired locally.
type Manager struct {
	Store  Store
	Client RefreshClient
	Locker Locker
	Clock  Clock

	// FreshWindow overrides the default 10-minute freshness window. Zero
	// means the default.
	FreshWindow time.Duration

	mu         sync.Mutex
	cached     Blob
	haveCached bool
}

// cachedFreshToken returns the in-process cached token when it is still
// fresh, without ever touching Store. The second return is false when there
// is no cache yet or the cached blob has fallen inside the freshness window.
func (m *Manager) cachedFreshToken() (string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.haveCached || !m.fresh(m.cached) {
		return "", false
	}
	return m.cached.AccessToken, true
}

// setCached records b as the in-process cache. Called any time Token
// observes a blob it is about to hand back (fresh from a read, from a
// reread, or freshly refreshed).
func (m *Manager) setCached(b Blob) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cached = b
	m.haveCached = true
}

// Invalidate drops the in-process cache, forcing the next Token call back
// through Store.Read (and the lock+reread+refresh path if that read is
// still stale). It does not touch Store or Client — it only forgets what
// this Manager currently believes is true, for a caller that has direct
// evidence the cached token no longer works (e.g. the Slack API itself
// rejected it as expired) even though it looks unexpired locally.
func (m *Manager) Invalidate() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.haveCached = false
	m.cached = Blob{}
}

func (m *Manager) freshWindow() time.Duration {
	if m.FreshWindow > 0 {
		return m.FreshWindow
	}
	return defaultFreshWindow
}

func (m *Manager) clock() Clock {
	if m.Clock != nil {
		return m.Clock
	}
	return SystemClock{}
}

// fresh reports whether b's access token still has more than the freshness
// window left, i.e. it can be served without taking the lock.
func (m *Manager) fresh(b Blob) bool {
	if b.AccessToken == "" {
		return false
	}
	return m.clock().Now().Add(m.freshWindow()).Before(b.AccessExpiresAt)
}

// Token returns a currently-valid Slack access token. The fast path serves
// the in-process cache with NO Store.Read at all while it has more than
// FreshWindow left; only a missing or stale cache falls through to
// Store.Read and, if that is also stale, the lock+reread+refresh path
// (serializing at most one refresh per staleness window across every
// caller, in this process and — with the production FileLock — every other
// process sharing the same credential).
func (m *Manager) Token(ctx context.Context) (string, error) {
	if tok, ok := m.cachedFreshToken(); ok {
		return tok, nil
	}

	blob, err := m.Store.Read(ctx)
	if err != nil {
		return "", fmt.Errorf("slackoauth: read credential store: %w", err)
	}
	if m.fresh(blob) {
		m.setCached(blob)
		return blob.AccessToken, nil
	}

	unlock, err := m.Locker.Lock(ctx)
	if err != nil {
		return "", fmt.Errorf("slackoauth: acquire refresh lock: %w", err)
	}
	defer unlock()

	// Reread under the lock: another caller (this process or another one,
	// with the production FileLock) may have already refreshed while we
	// waited.
	blob, err = m.Store.Read(ctx)
	if err != nil {
		return "", fmt.Errorf("slackoauth: reread credential store: %w", err)
	}
	if m.fresh(blob) {
		m.setCached(blob)
		return blob.AccessToken, nil
	}

	now := m.clock().Now()
	if !now.Before(blob.GrantExpiresAt) {
		return "", ErrGrantExpired
	}

	refreshed, err := m.Client.Refresh(ctx, blob.RefreshToken)
	if err != nil {
		return "", fmt.Errorf("slackoauth: refresh: %w", err)
	}
	refreshed.Version = BlobVersion
	// Refresh never knows the grant window; it is fixed at the original
	// Exchange and carried forward here.
	refreshed.GrantExpiresAt = blob.GrantExpiresAt
	if err := refreshed.Validate(); err != nil {
		return "", fmt.Errorf("slackoauth: refreshed blob failed validation: %w", err)
	}

	// Write the ENTIRE blob before returning a token: a token handed back
	// without a durable write would be unrecoverable once Slack rotates the
	// refresh_token out from under a store that never saw the new one.
	if err := m.Store.Write(ctx, refreshed); err != nil {
		return "", fmt.Errorf("slackoauth: write refreshed credentials: %w", err)
	}
	m.setCached(refreshed)
	return refreshed.AccessToken, nil
}
