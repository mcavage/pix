// slack_oauth_source_test.go covers the runtime credential source wiring
// added to slack.go: selecting OAuth over a stale static env token,
// serving from the Manager's in-process cache with no further 1Password
// read, the single invalidate-and-retry on token_expired/invalid_auth,
// static-token compatibility, the rotating xoxe.xoxp- prefix, identity pin
// enforcement under OAuth, and that no error ever leaks a token value.
//
// Every test either overrides slackNewTokenSource directly (bypassing
// config.Load()/1Password/Slack entirely) or points PIX_CONFIG/XDG_STATE_HOME
// at a temp dir, and every override is undone via resetSlackTokenSourceForTest
// in t.Cleanup — never leaking a fake (or a memoized real) source into an
// unrelated test.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pix/host/slackoauth"
)

// --- fakes -------------------------------------------------------------

// fakeSlackOAuthStore is a slackoauth.Store fake that counts Reads, so tests
// can prove the in-process cache really skips them.
type fakeSlackOAuthStore struct {
	mu    sync.Mutex
	blob  slackoauth.Blob
	reads int
}

func newFakeSlackOAuthStore(b slackoauth.Blob) *fakeSlackOAuthStore {
	return &fakeSlackOAuthStore{blob: b}
}

func (s *fakeSlackOAuthStore) Read(context.Context) (slackoauth.Blob, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	return s.blob, nil
}

func (s *fakeSlackOAuthStore) Write(_ context.Context, b slackoauth.Blob) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.blob = b
	return nil
}

func (s *fakeSlackOAuthStore) readCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

// fakeSlackOAuthLocker is a simple in-process Locker; the real cross-process
// FileLock is proven separately in slackoauth/lock_test.go.
type fakeSlackOAuthLocker struct{ mu sync.Mutex }

func (l *fakeSlackOAuthLocker) Lock(context.Context) (func(), error) {
	l.mu.Lock()
	return func() { l.mu.Unlock() }, nil
}

// fakeSlackOAuthClient panics-by-error if Refresh is ever called; none of
// the scenarios below have a stale-enough blob to need a real refresh.
type fakeSlackOAuthClient struct{}

func (fakeSlackOAuthClient) Refresh(context.Context, string) (slackoauth.Blob, error) {
	return slackoauth.Blob{}, errFakeRefreshCalled
}

var errFakeRefreshCalled = &slackTestErr{"Refresh must not be called in this test"}

type slackTestErr struct{ msg string }

func (e *slackTestErr) Error() string { return e.msg }

// freshSlackOAuthBlob builds a well-formed, currently-fresh Blob carrying
// accessToken, so tests can assert on a distinguishable "secret" value
// without ever needing a real Slack/1Password round trip.
func freshSlackOAuthBlob(accessToken string) slackoauth.Blob {
	now := time.Now()
	return slackoauth.Blob{
		Version:         slackoauth.BlobVersion,
		AccessToken:     accessToken,
		RefreshToken:    "xoxe-1-SECRET-REFRESH-TOKEN-VALUE",
		AccessExpiresAt: now.Add(2 * time.Hour),
		GrantExpiresAt:  now.Add(30 * 24 * time.Hour),
		TeamID:          "T_EXPECTED",
		UserID:          "U_EXPECTED",
		Scopes:          append([]string(nil), slackoauth.RequiredUserScopes...),
	}
}

// expiredSlackOAuthGrantBlob builds a Blob whose 30-day grant has already
// expired (and whose access token has too), so Manager.Token returns
// slackoauth.ErrGrantExpired without any network call.
func expiredSlackOAuthGrantBlob(accessToken string) slackoauth.Blob {
	now := time.Now()
	return slackoauth.Blob{
		Version:         slackoauth.BlobVersion,
		AccessToken:     accessToken,
		RefreshToken:    "xoxe-1-SECRET-REFRESH-TOKEN-VALUE",
		AccessExpiresAt: now.Add(-time.Hour),
		GrantExpiresAt:  now.Add(-time.Minute),
		TeamID:          "T_EXPECTED",
		UserID:          "U_EXPECTED",
		Scopes:          append([]string(nil), slackoauth.RequiredUserScopes...),
	}
}

// useOAuthTokenSourceForTest overrides slackNewTokenSource to hand back an
// oauthSlackTokenSource wrapping mgr, and registers the cleanup that
// undoes it (dropping the memoized instance and restoring the production
// factory) — so this fake can never leak into an unrelated test.
func useOAuthTokenSourceForTest(t *testing.T, mgr *slackoauth.Manager) {
	t.Helper()
	slackSourceMu.Lock()
	slackSourceCache = nil
	slackNewTokenSource = func() slackTokenSource { return &oauthSlackTokenSource{mgr: mgr} }
	slackSourceMu.Unlock()
	t.Cleanup(resetSlackTokenSourceForTest)
}

// useStaticTokenSourceForTest overrides slackNewTokenSource to always return
// the static env-based source, regardless of whatever config.toml this host
// happens to have on disk.
func useStaticTokenSourceForTest(t *testing.T) {
	t.Helper()
	slackSourceMu.Lock()
	slackSourceCache = nil
	slackNewTokenSource = func() slackTokenSource { return staticSlackTokenSource{} }
	slackSourceMu.Unlock()
	t.Cleanup(resetSlackTokenSourceForTest)
}

// slackFakeAPIServer starts an httptest.Server that answers Slack Web API
// calls via responder (keyed by method, the request path with its leading
// slash trimmed), and points slackAPI at it for the test's duration,
// restoring the real value in cleanup.
func slackFakeAPIServer(t *testing.T, responder func(method string, r *http.Request) (int, jsonObj)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		status, body := responder(strings.TrimPrefix(r.URL.Path, "/"), r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	prev := slackAPI
	slackAPI = srv.URL
	t.Cleanup(func() {
		srv.Close()
		slackAPI = prev
	})
	return srv
}

func slackOKResponse(fields jsonObj) jsonObj {
	out := jsonObj{"ok": true}
	for k, v := range fields {
		out[k] = v
	}
	return out
}

// --- selection: OAuth vs. static -----------------------------------------

// TestSlackTokenSourceOAuthWinsOverStaleStaticEnv proves that a complete
// [slack] OAuth wiring in config.toml is preferred over a SLACK_TOKEN env
// value, however plausible-looking and however stale.
func TestSlackTokenSourceOAuthWinsOverStaleStaticEnv(t *testing.T) {
	resetSlackTokenSourceForTest()
	t.Cleanup(resetSlackTokenSourceForTest)

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	const cfgTOML = "[slack]\nclient_id = \"C123\"\noauth_vault_id = \"Private\"\noauth_document_id = \"ITEM123\"\n"
	if err := os.WriteFile(cfgPath, []byte(cfgTOML), 0o600); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
	t.Setenv("PIX_CONFIG", cfgPath)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("SLACK_TOKEN", "xoxp-stale-static-token-that-must-be-ignored")

	source := slackGetTokenSource()
	if !source.IsOAuth() {
		t.Fatal("token source is not OAuth; want OAuth to win once config.toml carries a complete [slack] wiring")
	}
}

// TestSlackTokenSourceStaticCompatibility proves an install with no [slack]
// OAuth wiring at all falls back unchanged to the static SLACK_TOKEN path.
func TestSlackTokenSourceStaticCompatibility(t *testing.T) {
	resetSlackTokenSourceForTest()
	t.Cleanup(resetSlackTokenSourceForTest)

	t.Setenv("PIX_CONFIG", filepath.Join(t.TempDir(), "nope.toml")) // absent: defaults, no [slack]
	t.Setenv("SLACK_TOKEN", "xoxp-static-token-value")

	source := slackGetTokenSource()
	if source.IsOAuth() {
		t.Fatal("token source is OAuth; want the static fallback with no [slack] wiring configured")
	}
	tok, err := source.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if tok != "xoxp-static-token-value" {
		t.Errorf("token = %q, want the SLACK_TOKEN env value", tok)
	}
}

// --- end-to-end slackCall behavior over a fake Slack API -----------------

// TestSlackCallStaticTokenStillWorks is the no-regression proof: with no
// OAuth wiring, slackCall behaves exactly as it always did for a static
// xoxp- token.
func TestSlackCallStaticTokenStillWorks(t *testing.T) {
	resetSlackIdentityCheckForTest()
	t.Cleanup(resetSlackIdentityCheckForTest)
	useStaticTokenSourceForTest(t)
	t.Setenv("SLACK_TOKEN", "xoxp-static-token-value")
	t.Setenv("SLACK_TEAM_ID", "")
	t.Setenv("SLACK_USER_ID", "")

	slackFakeAPIServer(t, func(method string, r *http.Request) (int, jsonObj) {
		if method != "auth.test" {
			t.Fatalf("unexpected method %q", method)
		}
		return http.StatusOK, slackOKResponse(jsonObj{"team": "Acme", "team_id": "T1", "user": "jane", "user_id": "U1"})
	})

	obj, err := slackCall("auth.test", nil)
	if err != nil {
		t.Fatalf("slackCall: %v", err)
	}
	if obj["team_id"] != "T1" {
		t.Errorf("team_id = %v, want T1", obj["team_id"])
	}
}

// TestSlackCallOAuthServesFromCacheWithoutStoreRead proves that once the
// OAuth-backed Manager has served one fresh token, repeated slackCall
// invocations never touch Store.Read again — the in-process cache, not
// just a cheap re-check.
func TestSlackCallOAuthServesFromCacheWithoutStoreRead(t *testing.T) {
	resetSlackIdentityCheckForTest()
	t.Cleanup(resetSlackIdentityCheckForTest)
	t.Setenv("SLACK_TEAM_ID", "")
	t.Setenv("SLACK_USER_ID", "")

	store := newFakeSlackOAuthStore(freshSlackOAuthBlob("xoxe.xoxp-1-cached-token"))
	mgr := &slackoauth.Manager{Store: store, Client: fakeSlackOAuthClient{}, Locker: &fakeSlackOAuthLocker{}}
	useOAuthTokenSourceForTest(t, mgr)

	var calls int32
	slackFakeAPIServer(t, func(method string, r *http.Request) (int, jsonObj) {
		atomic.AddInt32(&calls, 1)
		return http.StatusOK, slackOKResponse(jsonObj{"team_id": "T_EXPECTED", "user_id": "U_EXPECTED"})
	})

	for i := 0; i < 5; i++ {
		if _, err := slackCall("auth.test", nil); err != nil {
			t.Fatalf("call %d: slackCall: %v", i, err)
		}
	}
	if got := store.readCount(); got != 1 {
		t.Errorf("store reads = %d, want exactly 1 (every later call must be served from the in-process cache)", got)
	}
	if got := atomic.LoadInt32(&calls); got != 5 {
		t.Errorf("Slack API calls = %d, want 5 (the network call itself still happens each time)", got)
	}
}

// TestSlackCallOAuthRetriesExactlyOnceOnTokenExpired proves a token_expired
// response invalidates the OAuth source's cache and retries the SAME call
// exactly once, and that a second failure would NOT be retried again (this
// scenario's second attempt succeeds, proving the retry path completes).
func TestSlackCallOAuthRetriesExactlyOnceOnTokenExpired(t *testing.T) {
	resetSlackIdentityCheckForTest()
	t.Cleanup(resetSlackIdentityCheckForTest)
	t.Setenv("SLACK_TEAM_ID", "")
	t.Setenv("SLACK_USER_ID", "")

	store := newFakeSlackOAuthStore(freshSlackOAuthBlob("xoxe.xoxp-1-token-a"))
	mgr := &slackoauth.Manager{Store: store, Client: fakeSlackOAuthClient{}, Locker: &fakeSlackOAuthLocker{}}
	useOAuthTokenSourceForTest(t, mgr)

	var calls int32
	slackFakeAPIServer(t, func(method string, r *http.Request) (int, jsonObj) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			return http.StatusOK, jsonObj{"ok": false, "error": "token_expired"}
		}
		return http.StatusOK, slackOKResponse(jsonObj{"channel": "C1", "messages": []any{}, "has_more": false})
	})

	obj, err := slackCall("conversations.history", map[string]string{"channel": "C1"})
	if err != nil {
		t.Fatalf("slackCall: %v", err)
	}
	if obj["has_more"] != false {
		t.Errorf("has_more = %v, want false", obj["has_more"])
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("Slack API calls = %d, want exactly 2 (one failure + exactly one retry)", got)
	}
	if got := store.readCount(); got != 2 {
		t.Errorf("store reads = %d, want exactly 2 (the initial fetch + one after Invalidate)", got)
	}
}

// TestSlackCallOAuthDoesNotRetryArbitraryErrors proves an error other than
// token_expired/invalid_auth is never retried, even for an OAuth source.
func TestSlackCallOAuthDoesNotRetryArbitraryErrors(t *testing.T) {
	resetSlackIdentityCheckForTest()
	t.Cleanup(resetSlackIdentityCheckForTest)
	t.Setenv("SLACK_TEAM_ID", "")
	t.Setenv("SLACK_USER_ID", "")

	store := newFakeSlackOAuthStore(freshSlackOAuthBlob("xoxe.xoxp-1-token-a"))
	mgr := &slackoauth.Manager{Store: store, Client: fakeSlackOAuthClient{}, Locker: &fakeSlackOAuthLocker{}}
	useOAuthTokenSourceForTest(t, mgr)

	var calls int32
	slackFakeAPIServer(t, func(method string, r *http.Request) (int, jsonObj) {
		atomic.AddInt32(&calls, 1)
		return http.StatusOK, jsonObj{"ok": false, "error": "rate_limited"}
	})

	if _, err := slackCall("conversations.history", map[string]string{"channel": "C1"}); err == nil {
		t.Fatal("slackCall succeeded; want the rate_limited failure to propagate")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("Slack API calls = %d, want exactly 1 (a non-expired error must never be retried)", got)
	}
	if got := store.readCount(); got != 1 {
		t.Errorf("store reads = %d, want exactly 1 (Invalidate must not have been triggered)", got)
	}
}

// TestSlackCallOAuthAcceptsRotatingPrefix proves the rotating xoxe.xoxp-
// token shape (what slackoauth.Client mints) is accepted end to end, not
// just the static xoxp- shape.
func TestSlackCallOAuthAcceptsRotatingPrefix(t *testing.T) {
	resetSlackIdentityCheckForTest()
	t.Cleanup(resetSlackIdentityCheckForTest)
	t.Setenv("SLACK_TEAM_ID", "")
	t.Setenv("SLACK_USER_ID", "")

	store := newFakeSlackOAuthStore(freshSlackOAuthBlob("xoxe.xoxp-1-rotating-token"))
	mgr := &slackoauth.Manager{Store: store, Client: fakeSlackOAuthClient{}, Locker: &fakeSlackOAuthLocker{}}
	useOAuthTokenSourceForTest(t, mgr)

	var gotAuth string
	slackFakeAPIServer(t, func(method string, r *http.Request) (int, jsonObj) {
		gotAuth = r.Header.Get("Authorization")
		return http.StatusOK, slackOKResponse(jsonObj{"team_id": "T_EXPECTED", "user_id": "U_EXPECTED"})
	})

	if _, err := slackCall("auth.test", nil); err != nil {
		t.Fatalf("slackCall: %v", err)
	}
	if gotAuth != "Bearer xoxe.xoxp-1-rotating-token" {
		t.Errorf("Authorization header = %q, want the rotating token as bearer", gotAuth)
	}
}

// TestSlackCallOAuthEnforcesIdentityPin proves SLACK_TEAM_ID/SLACK_USER_ID
// identity pin enforcement still runs before any data-bearing call, exactly
// as it does for the static path, when the credential source is OAuth.
func TestSlackCallOAuthEnforcesIdentityPin(t *testing.T) {
	resetSlackIdentityCheckForTest()
	t.Cleanup(resetSlackIdentityCheckForTest)
	t.Setenv("SLACK_TEAM_ID", "T_EXPECTED")
	t.Setenv("SLACK_USER_ID", "U_EXPECTED")

	store := newFakeSlackOAuthStore(freshSlackOAuthBlob("xoxe.xoxp-1-token-a"))
	mgr := &slackoauth.Manager{Store: store, Client: fakeSlackOAuthClient{}, Locker: &fakeSlackOAuthLocker{}}
	useOAuthTokenSourceForTest(t, mgr)

	slackFakeAPIServer(t, func(method string, r *http.Request) (int, jsonObj) {
		if method == "auth.test" {
			// A different identity than the pin: the mismatch must block the
			// data call below from ever running.
			return http.StatusOK, slackOKResponse(jsonObj{"team_id": "T_OTHER", "user_id": "U_OTHER"})
		}
		t.Fatal("conversations.history must never be called after an identity mismatch")
		return http.StatusOK, jsonObj{"ok": true}
	})

	_, err := slackCall("conversations.history", map[string]string{"channel": "C1"})
	if err == nil || !strings.Contains(err.Error(), "identity mismatch") {
		t.Fatalf("slackCall error = %v, want an identity mismatch refusal", err)
	}
}

// TestSlackCallOAuthGrantExpiredErrorNamesSetupWithoutLeakingSecrets proves
// an expired 30-day grant's error tells the operator to run `pix slack
// setup` and never includes the access or refresh token values.
func TestSlackCallOAuthGrantExpiredErrorNamesSetupWithoutLeakingSecrets(t *testing.T) {
	resetSlackIdentityCheckForTest()
	t.Cleanup(resetSlackIdentityCheckForTest)
	t.Setenv("SLACK_TEAM_ID", "")
	t.Setenv("SLACK_USER_ID", "")

	const secretAccess = "xoxe.xoxp-1-SECRET-ACCESS-TOKEN-VALUE"
	store := newFakeSlackOAuthStore(expiredSlackOAuthGrantBlob(secretAccess))
	mgr := &slackoauth.Manager{Store: store, Client: fakeSlackOAuthClient{}, Locker: &fakeSlackOAuthLocker{}}
	useOAuthTokenSourceForTest(t, mgr)

	// No fake server: Token must fail before any network call is made.
	_, err := slackCall("conversations.history", map[string]string{"channel": "C1"})
	if err == nil {
		t.Fatal("slackCall succeeded despite an expired grant; want a refusal")
	}
	msg := err.Error()
	if !strings.Contains(msg, "run pix slack setup") {
		t.Errorf("error = %q, want it to say to run pix slack setup", msg)
	}
	if strings.Contains(msg, secretAccess) || strings.Contains(msg, "SECRET-REFRESH-TOKEN") {
		t.Errorf("error = %q, leaked a token value", msg)
	}
}
