package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

func resetSlackIdentityCheckForTest() {
	slackIdentityMu.Lock()
	slackIdentityChecked = false
	slackIdentityErr = nil
	slackIdentityMu.Unlock()
}

func TestSlackCallRejectsNonPersonalTokenBeforeNetwork(t *testing.T) {
	resetSlackIdentityCheckForTest()
	t.Cleanup(resetSlackIdentityCheckForTest)
	// Force the static source regardless of whatever this host's real
	// config.toml happens to carry, so this test never depends on
	// on-disk state (see slack_oauth_source_test.go).
	useStaticTokenSourceForTest(t)
	t.Setenv("SLACK_TOKEN", "xoxb-not-a-personal-token")
	if _, err := slackCall("auth.test", nil); err == nil || !strings.Contains(err.Error(), "personal xoxp-") {
		t.Fatalf("slackCall with bot token error = %v, want personal-token rejection", err)
	}
}

func TestSlackCheckExpectedIdentityEnforcesPins(t *testing.T) {
	t.Setenv("SLACK_TEAM_ID", "T_EXPECTED")
	t.Setenv("SLACK_USER_ID", "U_EXPECTED")

	if err := slackCheckExpectedIdentity(jsonObj{"team_id": "T_EXPECTED", "user_id": "U_EXPECTED"}); err != nil {
		t.Fatalf("matching identity rejected: %v", err)
	}
	if err := slackCheckExpectedIdentity(jsonObj{"team_id": "T_OTHER", "user_id": "U_EXPECTED"}); err == nil || !strings.Contains(err.Error(), "team") {
		t.Fatalf("team mismatch error = %v", err)
	}
	if err := slackCheckExpectedIdentity(jsonObj{"team_id": "T_EXPECTED", "user_id": "U_OTHER"}); err == nil || !strings.Contains(err.Error(), "user") {
		t.Fatalf("user mismatch error = %v", err)
	}
}

func TestSlackCheckExpectedIdentityAllowsLegacyUnpinnedSetup(t *testing.T) {
	t.Setenv("SLACK_TEAM_ID", "")
	t.Setenv("SLACK_USER_ID", "")
	if err := slackCheckExpectedIdentity(jsonObj{"team_id": "T_ANY", "user_id": "U_ANY"}); err != nil {
		t.Fatalf("legacy unpinned setup rejected: %v", err)
	}
}

// identityTestSource is a minimal slackTokenSource fake whose Token fails a
// configurable number of times (simulating a transient network/1Password
// blip) before succeeding, so tests can drive slackVerifyExpectedIdentity's
// cache decision directly without a real OAuth Manager.
type identityTestSource struct {
	calls int
	fail  int // Token fails this many times before succeeding
	token string
}

func (s *identityTestSource) Token(context.Context) (string, error) {
	s.calls++
	if s.calls <= s.fail {
		return "", errors.New("transient: could not reach 1Password/network")
	}
	return s.token, nil
}
func (s *identityTestSource) Invalidate()   {}
func (s *identityTestSource) IsOAuth() bool { return false }

// TestSlackVerifyExpectedIdentityRetriesTransientErrorButCachesDefinitiveResult
// proves a transient failure to even REACH auth.test (source.Token erroring)
// is never cached — the very next call gets a fresh attempt — while a
// definitive result (auth.test answered, identity matched) IS cached and
// never re-verified again. This is the regression a sync.Once previously
// caused: any first-call error, transient or not, would wedge every later
// tool call behind it for the rest of the process's life.
func TestSlackVerifyExpectedIdentityRetriesTransientErrorButCachesDefinitiveResult(t *testing.T) {
	resetSlackIdentityCheckForTest()
	t.Cleanup(resetSlackIdentityCheckForTest)
	t.Setenv("SLACK_TEAM_ID", "T_EXPECTED")
	t.Setenv("SLACK_USER_ID", "U_EXPECTED")

	slackFakeAPIServer(t, func(method string, r *http.Request) (int, jsonObj) {
		return http.StatusOK, slackOKResponse(jsonObj{"team_id": "T_EXPECTED", "user_id": "U_EXPECTED"})
	})

	src := &identityTestSource{fail: 1, token: "xoxp-whatever"}

	if err := slackVerifyExpectedIdentity(context.Background(), src); err == nil {
		t.Fatal("first (transient) call should fail")
	}
	if src.calls != 1 {
		t.Fatalf("calls after first attempt = %d, want 1", src.calls)
	}

	// A transient failure must never be cached: the next call retries rather
	// than replaying the stale error.
	if err := slackVerifyExpectedIdentity(context.Background(), src); err != nil {
		t.Fatalf("second call should succeed once the transient failure clears, got %v", err)
	}
	if src.calls != 2 {
		t.Fatalf("calls after second attempt = %d, want 2 (must have retried, not replayed a cached error)", src.calls)
	}

	// A definitive success IS cached: a third call must be served from cache
	// with no further Token call.
	if err := slackVerifyExpectedIdentity(context.Background(), src); err != nil {
		t.Fatalf("third call: %v", err)
	}
	if src.calls != 2 {
		t.Fatalf("calls after third attempt = %d, want still 2 (a definitive success must be cached)", src.calls)
	}
}

// TestSlackVerifyExpectedIdentityCachesDefinitiveMismatch proves a PROVEN
// pin mismatch (auth.test answered, but with a different identity) is also
// cached — not just success — since it is just as definitive.
func TestSlackVerifyExpectedIdentityCachesDefinitiveMismatch(t *testing.T) {
	resetSlackIdentityCheckForTest()
	t.Cleanup(resetSlackIdentityCheckForTest)
	t.Setenv("SLACK_TEAM_ID", "T_EXPECTED")
	t.Setenv("SLACK_USER_ID", "U_EXPECTED")

	slackFakeAPIServer(t, func(method string, r *http.Request) (int, jsonObj) {
		return http.StatusOK, slackOKResponse(jsonObj{"team_id": "T_OTHER", "user_id": "U_OTHER"})
	})

	src := &identityTestSource{token: "xoxp-whatever"}

	err1 := slackVerifyExpectedIdentity(context.Background(), src)
	if err1 == nil || !strings.Contains(err1.Error(), "identity mismatch") {
		t.Fatalf("first call = %v, want an identity mismatch", err1)
	}
	if src.calls != 1 {
		t.Fatalf("calls = %d, want 1", src.calls)
	}

	err2 := slackVerifyExpectedIdentity(context.Background(), src)
	if err2 == nil || !strings.Contains(err2.Error(), "identity mismatch") {
		t.Fatalf("second call = %v, want the same cached mismatch", err2)
	}
	if src.calls != 1 {
		t.Fatalf("calls after second call = %d, want still 1 (a definitive mismatch must be cached, not re-verified)", src.calls)
	}
}
