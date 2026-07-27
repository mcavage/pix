package slackoauth

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeClock is a settable Clock for deterministic time-dependent assertions.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

// fakeDoer captures the last request it served (method, URL, decoded form
// body) and returns a canned response, so tests can assert on the exact wire
// shape the Client sends without ever touching the network.
type fakeDoer struct {
	resp     *http.Response
	err      error
	lastReq  *http.Request
	lastForm url.Values
}

func (d *fakeDoer) Do(req *http.Request) (*http.Response, error) {
	d.lastReq = req
	if req.Body != nil {
		body, _ := io.ReadAll(req.Body)
		d.lastForm, _ = url.ParseQuery(string(body))
	}
	if d.err != nil {
		return nil, d.err
	}
	return d.resp, nil
}

func jsonResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}
}

const authedUserOKBody = `{
  "ok": true,
  "app_id": "A123",
  "team": {"id": "T0123", "name": "pix"},
  "access_token": "xoxb-bot-token-must-be-ignored",
  "token_type": "bot",
  "authed_user": {
    "id": "U0123",
    "scope": "channels:read,chat:write",
    "access_token": "xoxp-personal-token",
    "token_type": "user",
    "refresh_token": "xoxe-refresh-token",
    "expires_in": 3600
  }
}`

func newTestClient(doer *fakeDoer, clk *fakeClock) *Client {
	return &Client{
		Doer:           doer,
		Clock:          clk,
		ClientID:       "client-123",
		RequiredScopes: []string{"channels:read", "chat:write"},
	}
}

// TestExchangeSendsCorrectFormAndNoClientSecret proves the authorization-code
// exchange sends exactly the PKCE form fields (client_id, code,
// code_verifier, redirect_uri, grant_type=authorization_code) and NEVER a
// client_secret, since this is a public PKCE client.
func TestExchangeSendsCorrectFormAndNoClientSecret(t *testing.T) {
	doer := &fakeDoer{resp: jsonResponse(200, authedUserOKBody)}
	clk := &fakeClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	c := newTestClient(doer, clk)

	_, err := c.Exchange(context.Background(), ExchangeParams{
		Code:         "auth-code",
		CodeVerifier: "verifier-xyz",
		RedirectURI:  "https://example.com/callback",
	})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if doer.lastReq.Method != http.MethodPost {
		t.Errorf("method = %s, want POST", doer.lastReq.Method)
	}
	if !strings.Contains(doer.lastReq.URL.String(), "oauth.v2.access") {
		t.Errorf("URL = %s, want oauth.v2.access", doer.lastReq.URL.String())
	}
	form := doer.lastForm
	want := map[string]string{
		"client_id":     "client-123",
		"code":          "auth-code",
		"code_verifier": "verifier-xyz",
		"redirect_uri":  "https://example.com/callback",
		"grant_type":    "authorization_code",
	}
	for k, v := range want {
		if got := form.Get(k); got != v {
			t.Errorf("form[%s] = %q, want %q", k, got, v)
		}
	}
	if form.Has("client_secret") {
		t.Error("form contains client_secret; a PKCE public client must never send one")
	}
	if len(form) != len(want) {
		t.Errorf("form has %d keys %v, want exactly %v", len(form), form, want)
	}
}

// TestExchangeSetsGrantExpiryPlus30Days proves the fixed 30-day rotating
// grant window is stamped from the injected clock at exchange time.
func TestExchangeSetsGrantExpiryPlus30Days(t *testing.T) {
	doer := &fakeDoer{resp: jsonResponse(200, authedUserOKBody)}
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	clk := &fakeClock{now: now}
	c := newTestClient(doer, clk)

	b, err := c.Exchange(context.Background(), ExchangeParams{
		Code: "auth-code", CodeVerifier: "verifier", RedirectURI: "https://example.com/cb",
	})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	want := now.Add(30 * 24 * time.Hour)
	if !b.GrantExpiresAt.Equal(want) {
		t.Errorf("GrantExpiresAt = %v, want %v", b.GrantExpiresAt, want)
	}
	wantAccessExp := now.Add(3600 * time.Second)
	if !b.AccessExpiresAt.Equal(wantAccessExp) {
		t.Errorf("AccessExpiresAt = %v, want %v", b.AccessExpiresAt, wantAccessExp)
	}
}

// TestExchangeSelectsOnlyAuthedUserCredentials proves the top-level bot
// access_token in the response is NEVER used; only authed_user's token pair
// becomes the Blob.
func TestExchangeSelectsOnlyAuthedUserCredentials(t *testing.T) {
	doer := &fakeDoer{resp: jsonResponse(200, authedUserOKBody)}
	clk := &fakeClock{now: time.Now()}
	c := newTestClient(doer, clk)

	b, err := c.Exchange(context.Background(), ExchangeParams{
		Code: "auth-code", CodeVerifier: "verifier", RedirectURI: "https://example.com/cb",
	})
	if err != nil {
		t.Fatalf("Exchange: %v", err)
	}
	if b.AccessToken != "xoxp-personal-token" {
		t.Errorf("AccessToken = %q, want the authed_user token, not the top-level bot token", b.AccessToken)
	}
	if b.RefreshToken != "xoxe-refresh-token" {
		t.Errorf("RefreshToken = %q", b.RefreshToken)
	}
	if b.TeamID != "T0123" || b.UserID != "U0123" {
		t.Errorf("TeamID/UserID = %s/%s, want T0123/U0123", b.TeamID, b.UserID)
	}
}

// TestExchangeRejectsNonPersonalTokenType proves a response whose authed_user
// token_type is not "user" is refused, even though the top-level response was
// ok:true.
func TestExchangeRejectsNonPersonalTokenType(t *testing.T) {
	body := strings.Replace(authedUserOKBody, `"token_type": "user"`, `"token_type": "bot"`, 1)
	doer := &fakeDoer{resp: jsonResponse(200, body)}
	c := newTestClient(doer, &fakeClock{now: time.Now()})
	if _, err := c.Exchange(context.Background(), ExchangeParams{
		Code: "c", CodeVerifier: "v", RedirectURI: "https://example.com/cb",
	}); err == nil {
		t.Fatal("Exchange succeeded with a non-user authed_user token_type; want a rejection")
	}
}

// TestExchangeRejectsMissingXoxpPrefix proves the access_token must carry the
// personal user token prefix, independent of the declared token_type.
func TestExchangeRejectsMissingXoxpPrefix(t *testing.T) {
	body := strings.Replace(authedUserOKBody, "xoxp-personal-token", "xoxa-not-a-user-token", 1)
	doer := &fakeDoer{resp: jsonResponse(200, body)}
	c := newTestClient(doer, &fakeClock{now: time.Now()})
	if _, err := c.Exchange(context.Background(), ExchangeParams{
		Code: "c", CodeVerifier: "v", RedirectURI: "https://example.com/cb",
	}); err == nil {
		t.Fatal("Exchange succeeded with a non xoxp- access_token; want a rejection")
	}
}

// TestExchangeRejectsScopeMismatch proves the authed_user scope set must
// exactly match the client's configured RequiredScopes — both extra and
// missing scopes are refused.
func TestExchangeRejectsScopeMismatch(t *testing.T) {
	for _, scope := range []string{
		"channels:read", // missing chat:write
		"channels:read,chat:write,admin",
	} {
		body := strings.Replace(authedUserOKBody, "channels:read,chat:write", scope, 1)
		doer := &fakeDoer{resp: jsonResponse(200, body)}
		c := newTestClient(doer, &fakeClock{now: time.Now()})
		if _, err := c.Exchange(context.Background(), ExchangeParams{
			Code: "c", CodeVerifier: "v", RedirectURI: "https://example.com/cb",
		}); err == nil {
			t.Fatalf("Exchange succeeded with scope %q; want a scope-mismatch rejection", scope)
		}
	}
}

// TestExchangeRequiresParams proves the required call parameters are checked
// before any request is sent.
func TestExchangeRequiresParams(t *testing.T) {
	doer := &fakeDoer{resp: jsonResponse(200, authedUserOKBody)}
	c := newTestClient(doer, &fakeClock{now: time.Now()})
	cases := []ExchangeParams{
		{CodeVerifier: "v", RedirectURI: "https://example.com/cb"},
		{Code: "c", RedirectURI: "https://example.com/cb"},
		{Code: "c", CodeVerifier: "v"},
	}
	for _, p := range cases {
		if _, err := c.Exchange(context.Background(), p); err == nil {
			t.Errorf("Exchange(%+v) succeeded; want a validation error", p)
		}
	}
	if doer.lastReq != nil {
		t.Error("Exchange sent a request despite invalid params")
	}
}

// TestRefreshSendsCorrectFormNoVerifierNoSecret proves the refresh flow sends
// exactly client_id, refresh_token, grant_type=refresh_token — no
// code_verifier, no client_secret.
func TestRefreshSendsCorrectFormNoVerifierNoSecret(t *testing.T) {
	doer := &fakeDoer{resp: jsonResponse(200, authedUserOKBody)}
	c := newTestClient(doer, &fakeClock{now: time.Now()})

	_, err := c.Refresh(context.Background(), "xoxe-refresh-token")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	form := doer.lastForm
	want := map[string]string{
		"client_id":     "client-123",
		"refresh_token": "xoxe-refresh-token",
		"grant_type":    "refresh_token",
	}
	for k, v := range want {
		if got := form.Get(k); got != v {
			t.Errorf("form[%s] = %q, want %q", k, got, v)
		}
	}
	for _, forbidden := range []string{"client_secret", "code_verifier", "code"} {
		if form.Has(forbidden) {
			t.Errorf("refresh form contains %q; must never be sent on a refresh", forbidden)
		}
	}
	if len(form) != len(want) {
		t.Errorf("refresh form has %d keys %v, want exactly %v", len(form), form, want)
	}
}

// TestRefreshLeavesGrantExpiryUnset proves Refresh does not itself decide the
// grant window (that's the Manager's job, carried forward from the original
// exchange) — it returns a zero GrantExpiresAt.
func TestRefreshLeavesGrantExpiryUnset(t *testing.T) {
	doer := &fakeDoer{resp: jsonResponse(200, authedUserOKBody)}
	c := newTestClient(doer, &fakeClock{now: time.Now()})
	b, err := c.Refresh(context.Background(), "xoxe-refresh-token")
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !b.GrantExpiresAt.IsZero() {
		t.Errorf("GrantExpiresAt = %v, want zero (Manager carries it forward)", b.GrantExpiresAt)
	}
}

// TestRefreshRequiresRefreshToken proves an empty refresh token is rejected
// before any request is sent.
func TestRefreshRequiresRefreshToken(t *testing.T) {
	doer := &fakeDoer{resp: jsonResponse(200, authedUserOKBody)}
	c := newTestClient(doer, &fakeClock{now: time.Now()})
	if _, err := c.Refresh(context.Background(), "  "); err == nil {
		t.Fatal("Refresh succeeded with a blank refresh token; want a validation error")
	}
	if doer.lastReq != nil {
		t.Error("Refresh sent a request despite a blank refresh token")
	}
}

// TestCallRejectsSlackAPIError proves an ok:false Slack response surfaces the
// Slack error code and is refused.
func TestCallRejectsSlackAPIError(t *testing.T) {
	doer := &fakeDoer{resp: jsonResponse(200, `{"ok": false, "error": "invalid_grant"}`)}
	c := newTestClient(doer, &fakeClock{now: time.Now()})
	_, err := c.Refresh(context.Background(), "xoxe-refresh-token")
	if err == nil {
		t.Fatal("Refresh succeeded on an ok:false response; want an error")
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("error = %q, want it to mention the Slack error code", err.Error())
	}
}

// TestCallRejectsNonOKHTTPStatus proves a non-200 HTTP status is refused even
// if the body happens to parse.
func TestCallRejectsNonOKHTTPStatus(t *testing.T) {
	doer := &fakeDoer{resp: jsonResponse(500, `{"ok": true}`)}
	c := newTestClient(doer, &fakeClock{now: time.Now()})
	if _, err := c.Refresh(context.Background(), "xoxe-refresh-token"); err == nil {
		t.Fatal("Refresh succeeded on HTTP 500; want an error")
	}
}

// TestCallRejectsOversizedBody proves the response body is capped at 1MB, so
// a misbehaving or malicious endpoint can't force unbounded memory use.
func TestCallRejectsOversizedBody(t *testing.T) {
	big := bytes.Repeat([]byte("a"), (1<<20)+1024)
	body := `{"ok": true, "padding": "` + string(big) + `"}`
	doer := &fakeDoer{resp: jsonResponse(200, body)}
	c := newTestClient(doer, &fakeClock{now: time.Now()})
	if _, err := c.Refresh(context.Background(), "xoxe-refresh-token"); err == nil {
		t.Fatal("Refresh succeeded on an oversized (>1MB) body; want an error")
	}
}

// TestCallPropagatesContext proves the outbound request carries the caller's
// context (so a caller-imposed deadline actually reaches the HTTP layer),
// and a context error surfaces as the call's error.
func TestCallPropagatesContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	doer := &fakeDoer{}
	doer.resp = jsonResponse(200, authedUserOKBody)
	// Override Do to fail fast on an already-cancelled context, the way a real
	// http.Client / http.Transport does.
	checkingDoer := doerFunc(func(req *http.Request) (*http.Response, error) {
		if err := req.Context().Err(); err != nil {
			return nil, err
		}
		return doer.resp, nil
	})
	c := &Client{Doer: checkingDoer, Clock: &fakeClock{now: time.Now()}, ClientID: "cid", RequiredScopes: []string{"channels:read", "chat:write"}}
	if _, err := c.Refresh(ctx, "xoxe-refresh-token"); err == nil {
		t.Fatal("Refresh succeeded despite an already-cancelled context; want it to propagate")
	}
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

// TestErrorsNeverContainTokenSecrets proves the package never leaks a token
// value into an error string, across the failure modes that carry a live
// token in scope (scope mismatch, wrong prefix).
func TestErrorsNeverContainTokenSecrets(t *testing.T) {
	secret := "xoxp-super-secret-should-never-leak"
	body := strings.Replace(authedUserOKBody, "xoxp-personal-token", secret, 1)
	body = strings.Replace(body, "channels:read,chat:write", "channels:read", 1) // force scope mismatch too
	doer := &fakeDoer{resp: jsonResponse(200, body)}
	c := newTestClient(doer, &fakeClock{now: time.Now()})
	_, err := c.Refresh(context.Background(), "xoxe-refresh-token")
	if err == nil {
		t.Fatal("expected a scope-mismatch error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error leaked the access token secret: %q", err.Error())
	}
}
