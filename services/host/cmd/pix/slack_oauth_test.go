// slack_oauth_test.go covers the localhost PKCE OAuth path (slack_oauth.go):
// the authorize URL/PKCE material, the one-shot local callback server (state
// mismatch, Origin rejection, wrong path/method, timeout), the rotating
// identity check, and a full hermetic end-to-end run of slackSetupPKCE
// proving config/op-refs/1Password argv all land correctly and no secret
// ever leaks into output, an error, or an argv.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"pix/host/config"
	"pix/host/slackoauth"
)

// --- PKCE material -----------------------------------------------------

func TestRandomURLSafeLengthsAndUniqueness(t *testing.T) {
	a, err := randomURLSafe(slackOAuthStateBytes)
	if err != nil {
		t.Fatalf("randomURLSafe: %v", err)
	}
	b, err := randomURLSafe(slackOAuthStateBytes)
	if err != nil {
		t.Fatalf("randomURLSafe: %v", err)
	}
	if a == b {
		t.Fatal("two calls returned the same value; want cryptographically random, distinct output")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(a)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != slackOAuthStateBytes {
		t.Errorf("decoded length = %d, want %d", len(decoded), slackOAuthStateBytes)
	}

	v, err := randomURLSafe(slackOAuthVerifierBytes)
	if err != nil {
		t.Fatalf("randomURLSafe (verifier): %v", err)
	}
	decodedV, err := base64.RawURLEncoding.DecodeString(v)
	if err != nil {
		t.Fatalf("decode verifier: %v", err)
	}
	if len(decodedV) != slackOAuthVerifierBytes {
		t.Errorf("decoded verifier length = %d, want %d", len(decodedV), slackOAuthVerifierBytes)
	}
}

func TestSlackCodeChallengeS256IsDeterministicAndMatchesManualCompute(t *testing.T) {
	verifier := "a-fixed-test-verifier-value-not-random"
	got := slackCodeChallengeS256(verifier)
	got2 := slackCodeChallengeS256(verifier)
	if got != got2 {
		t.Error("slackCodeChallengeS256 is not deterministic for the same verifier")
	}
	// Independently recompute via the stdlib primitives the implementation
	// itself must be built on, so this test would catch a wrong hash or a
	// wrong encoding (e.g. standard base64 instead of url-safe, or padding).
	sum := sha256.Sum256([]byte(verifier))
	want := base64.RawURLEncoding.EncodeToString(sum[:])
	if got != want {
		t.Errorf("slackCodeChallengeS256(%q) = %q, want %q (base64url(sha256(verifier)))", verifier, got, want)
	}
	if strings.ContainsAny(got, "+/=") {
		t.Errorf("code_challenge must be base64url with no padding, got %q", got)
	}
}

// --- authorize URL -------------------------------------------------------

func TestSlackBuildAuthorizeURLFields(t *testing.T) {
	got := slackBuildAuthorizeURL("C123", "http://localhost:17373/slack/callback", "STATE1", "CHALLENGE1")
	if !strings.HasPrefix(got, "https://slack.com/oauth/v2/authorize?") {
		t.Fatalf("authorize URL has wrong base: %s", got)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := u.Query()
	if q.Get("client_id") != "C123" {
		t.Errorf("client_id = %q", q.Get("client_id"))
	}
	if q.Get("redirect_uri") != "http://localhost:17373/slack/callback" {
		t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
	}
	if q.Get("state") != "STATE1" {
		t.Errorf("state = %q", q.Get("state"))
	}
	if q.Get("code_challenge") != "CHALLENGE1" {
		t.Errorf("code_challenge = %q", q.Get("code_challenge"))
	}
	if q.Get("code_challenge_method") != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", q.Get("code_challenge_method"))
	}
	if q.Get("scope") != "" {
		t.Errorf("a bot-scoped `scope` param must never be set, got %q", q.Get("scope"))
	}
	gotScope := q.Get("user_scope")
	wantScopes := map[string]bool{}
	for _, s := range slackOAuthUserScopes {
		wantScopes[s] = true
	}
	for _, s := range strings.Split(gotScope, ",") {
		if !wantScopes[s] {
			t.Errorf("user_scope has unexpected scope %q", s)
		}
		delete(wantScopes, s)
	}
	if len(wantScopes) != 0 {
		t.Errorf("user_scope is missing scopes: %v", wantScopes)
	}
}

// --- redirect uri parsing -------------------------------------------------

func TestParseSlackRedirectURIRequiresLocalhostFixedPortAndExactPath(t *testing.T) {
	cases := []struct {
		name    string
		uri     string
		wantErr bool
		wantP   int
	}{
		{"valid", "http://localhost:17373/slack/callback", false, 17373},
		{"https rejected", "https://localhost:17373/slack/callback", true, 0},
		{"non-localhost host rejected", "http://127.0.0.1:17373/slack/callback", true, 0},
		{"wrong path rejected", "http://localhost:17373/oauth/cb", true, 0},
		{"missing port rejected", "http://localhost/slack/callback", true, 0},
		{"garbage uri rejected", "://not a uri", true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			port, path, err := parseSlackRedirectURI(tc.uri)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("wanted an error for %q, got port=%d path=%q", tc.uri, port, path)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseSlackRedirectURI(%q): %v", tc.uri, err)
			}
			if port != tc.wantP {
				t.Errorf("port = %d, want %d", port, tc.wantP)
			}
			if path != slackOAuthCallbackPath {
				t.Errorf("path = %q, want %q", path, slackOAuthCallbackPath)
			}
		})
	}
}

// --- callback server -------------------------------------------------------

// freeLocalPort returns an available loopback TCP port by binding then
// immediately releasing it. There is an inherent (tiny) TOCTOU race in any
// such helper; acceptable for a hermetic test that never touches a real
// network beyond 127.0.0.1.
func freeLocalPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("finding a free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func TestSlackBindCallbackListenerFailsWhenPortBusy(t *testing.T) {
	port := freeLocalPort(t)
	occupied, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("occupying port: %v", err)
	}
	defer occupied.Close()

	if _, err := slackBindCallbackListener(port); err == nil {
		t.Fatal("expected a bind failure on an already-busy port")
	}
}

func TestSlackCallbackHappyPath(t *testing.T) {
	port := freeLocalPort(t)
	ln, err := slackBindCallbackListener(port)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer ln.Close()

	resultCh := make(chan struct {
		code string
		err  error
	}, 1)
	go func() {
		code, err := slackServeCallback(ln, slackOAuthCallbackPath, "expected-state", 5*time.Second)
		resultCh <- struct {
			code string
			err  error
		}{code, err}
	}()

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s?code=abc123&state=expected-state", port, slackOAuthCallbackPath))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	if got := resp.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Errorf("Referrer-Policy = %q, want no-referrer", got)
	}

	res := <-resultCh
	if res.err != nil {
		t.Fatalf("slackServeCallback: %v", res.err)
	}
	if res.code != "abc123" {
		t.Errorf("code = %q, want abc123", res.code)
	}
}

// TestSlackCallbackStateMismatchDoesNotEndTheWait proves a state-mismatched
// request gets a 400 and is discarded, but the callback wait KEEPS LISTENING
// (docs/design/slack-setup.md's callback hardening): a genuine callback
// arriving afterward still succeeds, exactly like the wrong-path/wrong-method
// cases. A separate timeout test below covers what happens when no genuine
// callback ever arrives.
func TestSlackCallbackStateMismatchDoesNotEndTheWait(t *testing.T) {
	port := freeLocalPort(t)
	ln, err := slackBindCallbackListener(port)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer ln.Close()

	resultCh := make(chan struct {
		code string
		err  error
	}, 1)
	go func() {
		code, err := slackServeCallback(ln, slackOAuthCallbackPath, "expected-state", 5*time.Second)
		resultCh <- struct {
			code string
			err  error
		}{code, err}
	}()

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s?code=abc123&state=WRONG", port, slackOAuthCallbackPath))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}

	// The genuine callback, sent right after: must still succeed.
	resp2, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s?code=real-code&state=expected-state", port, slackOAuthCallbackPath))
	if err != nil {
		t.Fatalf("GET real callback: %v", err)
	}
	resp2.Body.Close()

	res := <-resultCh
	if res.err != nil {
		t.Fatalf("slackServeCallback: %v (a state mismatch must not end the wait)", res.err)
	}
	if res.code != "real-code" {
		t.Errorf("code = %q, want real-code", res.code)
	}
}

// TestSlackCallbackAllStateMismatchesTimeOut proves that when NO genuine
// callback ever arrives, an all-mismatched attempt still eventually times
// out rather than waiting forever.
func TestSlackCallbackAllStateMismatchesTimeOut(t *testing.T) {
	port := freeLocalPort(t)
	ln, err := slackBindCallbackListener(port)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer ln.Close()

	resultCh := make(chan error, 1)
	go func() {
		_, err := slackServeCallback(ln, slackOAuthCallbackPath, "expected-state", 100*time.Millisecond)
		resultCh <- err
	}()

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s?code=abc123&state=WRONG", port, slackOAuthCallbackPath))
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()

	err = <-resultCh
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected a timeout error when no genuine callback ever arrives, got %v", err)
	}
}

// TestSlackCallbackOriginHeaderDoesNotEndTheWait proves a request carrying an
// Origin header gets a 403 and is discarded, but the callback wait KEEPS
// LISTENING: a genuine callback (no Origin header) arriving afterward still
// succeeds.
func TestSlackCallbackOriginHeaderDoesNotEndTheWait(t *testing.T) {
	port := freeLocalPort(t)
	ln, err := slackBindCallbackListener(port)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer ln.Close()

	resultCh := make(chan struct {
		code string
		err  error
	}, 1)
	go func() {
		code, err := slackServeCallback(ln, slackOAuthCallbackPath, "expected-state", 5*time.Second)
		resultCh <- struct {
			code string
			err  error
		}{code, err}
	}()

	req, _ := http.NewRequest(http.MethodGet,
		fmt.Sprintf("http://127.0.0.1:%d%s?code=abc123&state=expected-state", port, slackOAuthCallbackPath), nil)
	req.Header.Set("Origin", "https://evil.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}

	// The genuine callback, with no Origin header: must still succeed.
	resp2, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s?code=real-code&state=expected-state", port, slackOAuthCallbackPath))
	if err != nil {
		t.Fatalf("GET real callback: %v", err)
	}
	resp2.Body.Close()

	res := <-resultCh
	if res.err != nil {
		t.Fatalf("slackServeCallback: %v (an Origin-header probe must not end the wait)", res.err)
	}
	if res.code != "real-code" {
		t.Errorf("code = %q, want real-code", res.code)
	}
}

func TestSlackCallbackWrongPathAndMethodDoNotEndTheWait(t *testing.T) {
	port := freeLocalPort(t)
	ln, err := slackBindCallbackListener(port)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer ln.Close()

	resultCh := make(chan struct {
		code string
		err  error
	}, 1)
	go func() {
		code, err := slackServeCallback(ln, slackOAuthCallbackPath, "expected-state", 5*time.Second)
		resultCh <- struct {
			code string
			err  error
		}{code, err}
	}()

	// Wrong path: 404, must not end the wait.
	resp1, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/wrong/path", port))
	if err != nil {
		t.Fatalf("GET wrong path: %v", err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusNotFound {
		t.Errorf("wrong path status = %d, want 404", resp1.StatusCode)
	}

	// Wrong method on the right path: 405, must not end the wait.
	resp2, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d%s", port, slackOAuthCallbackPath), "text/plain", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST status = %d, want 405", resp2.StatusCode)
	}

	// Now the real callback: must still succeed.
	resp3, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s?code=real-code&state=expected-state", port, slackOAuthCallbackPath))
	if err != nil {
		t.Fatalf("GET real callback: %v", err)
	}
	resp3.Body.Close()

	res := <-resultCh
	if res.err != nil {
		t.Fatalf("slackServeCallback: %v", res.err)
	}
	if res.code != "real-code" {
		t.Errorf("code = %q, want real-code", res.code)
	}
}

func TestSlackCallbackTimesOutWithNoRequest(t *testing.T) {
	port := freeLocalPort(t)
	ln, err := slackBindCallbackListener(port)
	if err != nil {
		t.Fatalf("bind: %v", err)
	}
	defer ln.Close()

	_, err = slackServeCallback(ln, slackOAuthCallbackPath, "expected-state", 50*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected a timeout error, got %v", err)
	}
}

// --- identity check --------------------------------------------------------

func TestSlackVerifyRotatingIdentityMismatchRejectedAndNeverLeaksToken(t *testing.T) {
	blob := slackoauth.Blob{
		AccessToken:  "xoxe.xoxp-should-never-appear-in-any-error",
		RefreshToken: "xoxe-refresh-should-never-appear-either",
		TeamID:       "T1",
		UserID:       "U1",
	}
	id := slackIdentity{TeamID: "T2", UserID: "U1"}
	err := slackVerifyRotatingIdentity(id, blob)
	if err == nil {
		t.Fatal("mismatched team id must be rejected")
	}
	if strings.Contains(err.Error(), blob.AccessToken) || strings.Contains(err.Error(), blob.RefreshToken) {
		t.Errorf("identity mismatch error leaked a token: %q", err.Error())
	}

	id2 := slackIdentity{TeamID: "T1", UserID: "U2"}
	if err := slackVerifyRotatingIdentity(id2, blob); err == nil {
		t.Fatal("mismatched user id must be rejected")
	}

	idOK := slackIdentity{TeamID: "T1", UserID: "U1"}
	if err := slackVerifyRotatingIdentity(idOK, blob); err != nil {
		t.Errorf("matching identity must be accepted, got %v", err)
	}
}

// --- full hermetic end-to-end PKCE run --------------------------------------

// fakeOAuthDoer intercepts the slackoauth.Client's oauth.v2.access POST and
// records the form it was sent, so the test can assert the exchange body
// never carries a client_secret and does carry the PKCE fields.
type fakeOAuthDoer struct {
	mu       sync.Mutex
	lastForm url.Values
	respBody string
}

func (f *fakeOAuthDoer) Do(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	form, _ := url.ParseQuery(string(body))
	f.mu.Lock()
	f.lastForm = form
	f.mu.Unlock()
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(f.respBody)),
		Header:     make(http.Header),
	}, nil
}

// fakeOPRunner is a slackoauth.CommandRunner that never shells out: it
// records every call (proving the blob only ever appears in stdin) and
// returns a scripted op-document-style response.
type fakeOPRunner struct {
	mu    sync.Mutex
	calls []struct {
		name  string
		args  []string
		stdin []byte
	}
	itemID  string
	vaultID string
	// writeErr, when set, is returned by every Run call instead of the
	// scripted success response — simulates a failed `op document create/edit`
	// (e.g. a locked vault) for slackSetupPKCE's store.Write failure path.
	writeErr error
}

func (f *fakeOPRunner) Run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, struct {
		name  string
		args  []string
		stdin []byte
	}{name, append([]string(nil), args...), append([]byte(nil), stdin...)})
	if f.writeErr != nil {
		return nil, f.writeErr
	}
	return []byte(fmt.Sprintf(`{"id":%q,"vault":{"id":%q}}`, f.itemID, f.vaultID)), nil
}

func fixedTestClock(t time.Time) slackoauth.Clock { return fixedSlackOAuthClock{t} }

type fixedSlackOAuthClock struct{ t time.Time }

func (c fixedSlackOAuthClock) Now() time.Time { return c.t }

const testRotatingAccessToken = "xoxe.xoxp-rotating-test-token"
const testRotatingRefreshToken = "xoxe-refresh-test-token"

func slackOAuthTestExchangeBody(teamID, userID string) string {
	scopes := strings.Join(slackOAuthUserScopes, ",")
	return fmt.Sprintf(`{
  "ok": true,
  "team": {"id": %q},
  "authed_user": {
    "id": %q,
    "scope": %q,
    "access_token": %q,
    "token_type": "user",
    "refresh_token": %q,
    "expires_in": 3600
  }
}`, teamID, userID, scopes, testRotatingAccessToken, testRotatingRefreshToken)
}

// TestSlackSetupPKCEHappyPath drives slackSetupPKCE end to end, hermetically:
// a fake browser opener performs the real (loopback-only) HTTP callback GET,
// a fake HTTPDoer stands in for Slack's oauth.v2.access, a fake CommandRunner
// stands in for `op`, and env.SlackAuth stands in for the live auth.test
// call. Proves: authorize URL/exchange form correctness, the identity match
// gate, that op-refs.env ends up with only the non-secret pins (legacy
// SLACK_TOKEN removed), that config.toml is fully populated + MCP added, that
// the OPStore write only ever puts the blob on stdin, and that neither the
// rotating access nor refresh token appears anywhere in printed output.
func TestSlackSetupPKCEHappyPath(t *testing.T) {
	slackTestCfg(t)
	// Seed a legacy SLACK_TOKEN ref to prove the PKCE path removes it.
	opRefsWith(t, "SLACK_TOKEN=op://Private/Slack/credential")

	port := freeLocalPort(t)
	redirectURI := fmt.Sprintf("http://localhost:%d/slack/callback", port)

	doer := &fakeOAuthDoer{respBody: slackOAuthTestExchangeBody("T123", "U456")}
	runner := &fakeOPRunner{itemID: "item-abc", vaultID: "vault-xyz"}
	deps := slackOAuthDeps{
		openBrowser: func(authURL string) error {
			u, err := url.Parse(authURL)
			if err != nil {
				return err
			}
			q := u.Query()
			cbURL := fmt.Sprintf("%s?code=test-auth-code&state=%s", q.Get("redirect_uri"), q.Get("state"))
			go func() {
				resp, err := http.Get(cbURL)
				if err == nil {
					resp.Body.Close()
				}
			}()
			return nil
		},
		runner:  runner,
		doer:    doer,
		clock:   fixedTestClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)),
		timeout: 5 * time.Second,
	}

	authTestIdentity := slackIdentity{Team: "Acme", TeamID: "T123", User: "jane", UserID: "U456"}
	f := &slackTestEnv{sbxPresent: true, authTest: func(token string) (slackIdentity, error) {
		if token != testRotatingAccessToken {
			t.Errorf("auth.test called with unexpected token %q", token)
		}
		return authTestIdentity, nil
	}}
	env := f.env()

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	opts := slackSetupOpts{
		assumeYes:     true,
		clientID:      "C0PKCE",
		redirectURI:   redirectURI,
		vault:         "Private",
		vaultExplicit: true,
	}
	var out bytes.Buffer
	err = slackSetupPKCE(env, cfg, opts, deps, strings.NewReader(""), &out, false, fakeHostResolver)
	if err != nil {
		t.Fatalf("slackSetupPKCE: %v\n--- output ---\n%s", err, out.String())
	}

	outText := out.String()
	for _, secret := range []string{testRotatingAccessToken, testRotatingRefreshToken} {
		if strings.Contains(outText, secret) {
			t.Errorf("output leaked a rotating token: %q", outText)
		}
	}
	if !strings.Contains(outText, "monthly") && !strings.Contains(outText, "expires") {
		t.Errorf("setup output must state the reauthorization deadline, got:\n%s", outText)
	}
	if !strings.Contains(outText, "Registration and sandbox attachment are separate") {
		t.Errorf("setup output must call out registration vs attachment separately, got:\n%s", outText)
	}

	// exchange form: PKCE fields present, no client_secret ever.
	doer.mu.Lock()
	form := doer.lastForm
	doer.mu.Unlock()
	if form.Get("client_id") != "C0PKCE" {
		t.Errorf("exchange client_id = %q", form.Get("client_id"))
	}
	if form.Get("code") != "test-auth-code" {
		t.Errorf("exchange code = %q", form.Get("code"))
	}
	if form.Get("redirect_uri") != redirectURI {
		t.Errorf("exchange redirect_uri = %q, want %q", form.Get("redirect_uri"), redirectURI)
	}
	if form.Get("code_verifier") == "" {
		t.Error("exchange must carry a code_verifier")
	}
	if form.Get("client_secret") != "" {
		t.Error("exchange must NEVER carry a client_secret (PKCE, public client)")
	}

	// op argv: never a secret, blob only over stdin.
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if len(runner.calls) != 1 {
		t.Fatalf("expected exactly one op call, got %d", len(runner.calls))
	}
	call := runner.calls[0]
	if call.name != "op" {
		t.Errorf("runner name = %q, want op", call.name)
	}
	argvJoined := strings.Join(call.args, " ")
	if !strings.Contains(argvJoined, "Pix Slack OAuth - jane") {
		t.Errorf("op document title missing/wrong in argv: %v", call.args)
	}
	for _, secret := range []string{testRotatingAccessToken, testRotatingRefreshToken} {
		if strings.Contains(argvJoined, secret) {
			t.Errorf("op argv leaked a secret: %v", call.args)
		}
	}
	if len(call.stdin) == 0 {
		t.Error("op document write must send the blob over stdin")
	}
	if !strings.Contains(string(call.stdin), testRotatingAccessToken) {
		t.Error("stdin must carry the credential blob (access token missing)")
	}

	// op-refs.env: legacy SLACK_TOKEN gone, non-secret pins present.
	remaining := map[string]string{}
	for _, r := range parseOpRefs(opRefsFileContent(t)) {
		remaining[r.key] = r.value
	}
	if _, ok := remaining["SLACK_TOKEN"]; ok {
		t.Error("the legacy SLACK_TOKEN ref must be removed by the PKCE path")
	}
	if remaining["SLACK_TEAM_ID"] != "T123" {
		t.Errorf("SLACK_TEAM_ID = %q, want T123", remaining["SLACK_TEAM_ID"])
	}
	if remaining["SLACK_USER_ID"] != "U456" {
		t.Errorf("SLACK_USER_ID = %q, want U456", remaining["SLACK_USER_ID"])
	}

	// config.toml: fully populated + MCP added.
	got, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load after setup: %v", err)
	}
	if got.Slack.ClientID != "C0PKCE" {
		t.Errorf("Slack.ClientID = %q", got.Slack.ClientID)
	}
	if got.Slack.RedirectURI != redirectURI {
		t.Errorf("Slack.RedirectURI = %q, want %q", got.Slack.RedirectURI, redirectURI)
	}
	if got.Slack.OAuthVaultID != "vault-xyz" {
		t.Errorf("Slack.OAuthVaultID = %q", got.Slack.OAuthVaultID)
	}
	if got.Slack.OAuthDocumentID != "item-abc" {
		t.Errorf("Slack.OAuthDocumentID = %q", got.Slack.OAuthDocumentID)
	}
	wantExpiry := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC).Add(slackoauth.GrantWindow)
	if !got.Slack.OAuthGrantExpiresAt.Equal(wantExpiry) {
		t.Errorf("Slack.OAuthGrantExpiresAt = %v, want %v", got.Slack.OAuthGrantExpiresAt, wantExpiry)
	}
	if !mcpConfigured(got, slackServerName) {
		t.Error("slack must be added to the configured mcp set")
	}
	if !f.ranSbxAdd() {
		t.Error("slack must be registered with the sbx gateway")
	}
}

func TestSlackSetupPKCERequiresClientID(t *testing.T) {
	slackTestCfg(t)
	f := &slackTestEnv{sbxPresent: true}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = slackSetupPKCE(f.env(), cfg, slackSetupOpts{assumeYes: true, vault: "Private"}, defaultSlackOAuthDeps(),
		strings.NewReader(""), &out, false, fakeHostResolver)
	if err == nil || !strings.Contains(err.Error(), "client id") {
		t.Fatalf("missing client id must be refused, got %v", err)
	}
}

// TestSlackSetupDispatchesToPKCEWithoutTokenRef proves the umbrella
// slackSetup dispatcher runs the PKCE path (not the static one) whenever
// --token-ref is absent — i.e. the token-ref path is a genuine fallback, not
// the only path.
func TestSlackSetupDispatchesToPKCEWithoutTokenRef(t *testing.T) {
	slackTestCfg(t)
	f := &slackTestEnv{sbxPresent: true}
	var out bytes.Buffer
	err := slackSetup(f.env(), slackSetupOpts{assumeYes: true, vault: "Private"}, strings.NewReader(""), &out, false, fakeHostResolver)
	if err == nil || !strings.Contains(err.Error(), "client id") {
		t.Fatalf("expected the PKCE path's missing-client-id error, got %v", err)
	}
}

// --- best-effort revoke of a minted-but-unpersisted grant -------------------

// slackOAuthRevokeFixture builds a slackSetupPKCE fixture that drives a
// SUCCESSFUL browser callback + Slack code exchange (minting a real rotating
// grant, testRotatingAccessToken/testRotatingRefreshToken), so a test can
// then fail a LATER step and prove the freshly minted grant gets a
// best-effort revoke via the injected deps.revoke seam. revokedTokens
// records every token passed to revoke, in order.
func slackOAuthRevokeFixture(t *testing.T, authTest func(token string) (slackIdentity, error)) (deps slackOAuthDeps, opts slackSetupOpts, env shellEnv, cfg *config.Config, revokedTokens *[]string) {
	t.Helper()
	slackTestCfg(t)
	port := freeLocalPort(t)
	redirectURI := fmt.Sprintf("http://localhost:%d/slack/callback", port)

	doer := &fakeOAuthDoer{respBody: slackOAuthTestExchangeBody("T123", "U456")}
	runner := &fakeOPRunner{itemID: "item-abc", vaultID: "vault-xyz"}
	var tokens []string
	deps = slackOAuthDeps{
		openBrowser: func(authURL string) error {
			u, err := url.Parse(authURL)
			if err != nil {
				return err
			}
			q := u.Query()
			cbURL := fmt.Sprintf("%s?code=test-auth-code&state=%s", q.Get("redirect_uri"), q.Get("state"))
			go func() {
				resp, err := http.Get(cbURL)
				if err == nil {
					resp.Body.Close()
				}
			}()
			return nil
		},
		runner:  runner,
		doer:    doer,
		clock:   fixedTestClock(time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)),
		timeout: 5 * time.Second,
		revoke: func(tok string) (bool, error) {
			tokens = append(tokens, tok)
			return true, nil
		},
	}

	f := &slackTestEnv{sbxPresent: true, authTest: authTest}
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	opts = slackSetupOpts{
		assumeYes:     true,
		clientID:      "C0PKCE",
		redirectURI:   redirectURI,
		vault:         "Private",
		vaultExplicit: true,
	}
	return deps, opts, f.env(), cfg, &tokens
}

// TestSlackSetupPKCERevokesOnAuthTestFailure proves a failed auth.test on the
// freshly exchanged token (grant minted, but never usable) triggers a
// best-effort revoke, and the error names the outcome rather than just
// reporting "nothing written".
func TestSlackSetupPKCERevokesOnAuthTestFailure(t *testing.T) {
	deps, opts, env, cfg, revoked := slackOAuthRevokeFixture(t, func(string) (slackIdentity, error) {
		return slackIdentity{}, fmt.Errorf("auth.test: invalid_auth")
	})
	var out bytes.Buffer
	err := slackSetupPKCE(env, cfg, opts, deps, strings.NewReader(""), &out, false, fakeHostResolver)
	if err == nil {
		t.Fatal("expected an error when auth.test fails on the newly issued token")
	}
	if !strings.Contains(err.Error(), "revoked") {
		t.Errorf("error must say whether the minted grant was revoked, got: %v", err)
	}
	if len(*revoked) != 1 || (*revoked)[0] != testRotatingAccessToken {
		t.Errorf("revoked tokens = %v, want exactly [%s]", *revoked, testRotatingAccessToken)
	}
}

// TestSlackSetupPKCERevokesOnIdentityMismatch proves the same for the
// exchange-vs-auth.test identity mismatch gate.
func TestSlackSetupPKCERevokesOnIdentityMismatch(t *testing.T) {
	deps, opts, env, cfg, revoked := slackOAuthRevokeFixture(t, func(string) (slackIdentity, error) {
		return slackIdentity{Team: "Other", TeamID: "T_OTHER", User: "eve", UserID: "U_OTHER"}, nil
	})
	var out bytes.Buffer
	err := slackSetupPKCE(env, cfg, opts, deps, strings.NewReader(""), &out, false, fakeHostResolver)
	if err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("expected an identity mismatch error, got %v", err)
	}
	if !strings.Contains(err.Error(), "revoked") {
		t.Errorf("error must say whether the minted grant was revoked, got: %v", err)
	}
	if len(*revoked) != 1 || (*revoked)[0] != testRotatingAccessToken {
		t.Errorf("revoked tokens = %v, want exactly [%s]", *revoked, testRotatingAccessToken)
	}
}

// TestSlackSetupPKCERevokesOnDeclinedConfirmation proves a declined confirm
// prompt revokes the minted grant and reports it, never just "nothing
// written" (which would be misleading: SOMETHING was minted, even though
// nothing was PERSISTED).
func TestSlackSetupPKCERevokesOnDeclinedConfirmation(t *testing.T) {
	deps, opts, env, cfg, revoked := slackOAuthRevokeFixture(t, func(string) (slackIdentity, error) {
		return slackIdentity{Team: "Acme", TeamID: "T123", User: "jane", UserID: "U456"}, nil
	})
	opts.assumeYes = false
	var out bytes.Buffer
	err := slackSetupPKCE(env, cfg, opts, deps, strings.NewReader("n\n"), &out, true, fakeHostResolver)
	if err != nil {
		t.Fatalf("a declined confirmation must not itself be an error, got %v", err)
	}
	if !strings.Contains(out.String(), "aborted") {
		t.Errorf("output must say aborted, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "aborted; nothing written") {
		t.Errorf("output must not claim plain 'nothing written' — a grant WAS minted; got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "revoked") {
		t.Errorf("output must say whether the minted grant was revoked, got:\n%s", out.String())
	}
	if len(*revoked) != 1 || (*revoked)[0] != testRotatingAccessToken {
		t.Errorf("revoked tokens = %v, want exactly [%s]", *revoked, testRotatingAccessToken)
	}
}

// TestSlackSetupPKCERevokesOnNonInteractiveWithoutYes proves the non-TTY,
// no --yes refusal ALSO revokes the minted grant (this path only reaches
// here once a real code exchange already minted a live credential).
func TestSlackSetupPKCERevokesOnNonInteractiveWithoutYes(t *testing.T) {
	deps, opts, env, cfg, revoked := slackOAuthRevokeFixture(t, func(string) (slackIdentity, error) {
		return slackIdentity{Team: "Acme", TeamID: "T123", User: "jane", UserID: "U456"}, nil
	})
	opts.assumeYes = false
	var out bytes.Buffer
	err := slackSetupPKCE(env, cfg, opts, deps, strings.NewReader(""), &out, false, fakeHostResolver)
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("expected the non-interactive refusal, got %v", err)
	}
	if !strings.Contains(err.Error(), "revoked") {
		t.Errorf("error must say whether the minted grant was revoked, got: %v", err)
	}
	if len(*revoked) != 1 || (*revoked)[0] != testRotatingAccessToken {
		t.Errorf("revoked tokens = %v, want exactly [%s]", *revoked, testRotatingAccessToken)
	}
}

// TestSlackSetupPKCERevokesOnStoreWriteFailure proves a failed 1Password
// write (the grant minted, but never durably persisted) ALSO triggers the
// best-effort revoke.
func TestSlackSetupPKCERevokesOnStoreWriteFailure(t *testing.T) {
	deps, opts, env, cfg, revoked := slackOAuthRevokeFixture(t, func(string) (slackIdentity, error) {
		return slackIdentity{Team: "Acme", TeamID: "T123", User: "jane", UserID: "U456"}, nil
	})
	deps.runner.(*fakeOPRunner).writeErr = fmt.Errorf("op: vault locked")
	var out bytes.Buffer
	err := slackSetupPKCE(env, cfg, opts, deps, strings.NewReader(""), &out, false, fakeHostResolver)
	if err == nil || !strings.Contains(err.Error(), "writing the Slack credential to 1Password") {
		t.Fatalf("expected the 1Password write failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "revoked") {
		t.Errorf("error must say whether the minted grant was revoked, got: %v", err)
	}
	if len(*revoked) != 1 || (*revoked)[0] != testRotatingAccessToken {
		t.Errorf("revoked tokens = %v, want exactly [%s]", *revoked, testRotatingAccessToken)
	}
}

// --- identity-pin-change gate ------------------------------------------

// TestSlackSetupPKCERefusesDifferentIdentityUnderYes proves that an existing
// SLACK_TEAM_ID/SLACK_USER_ID pin plus a NEW, DIFFERENT OAuth identity is
// refused under --yes, rather than silently re-pinning to someone else. The
// freshly minted grant must be best-effort revoked.
func TestSlackSetupPKCERefusesDifferentIdentityUnderYes(t *testing.T) {
	deps, opts, env, cfg, revoked := slackOAuthRevokeFixture(t, func(string) (slackIdentity, error) {
		return slackIdentity{Team: "Acme", TeamID: "T123", User: "jane", UserID: "U456"}, nil
	})
	opRefsWith(t, "SLACK_TEAM_ID=T_OLD", "SLACK_USER_ID=U_OLD")
	var out bytes.Buffer
	err := slackSetupPKCE(env, cfg, opts, deps, strings.NewReader(""), &out, false, fakeHostResolver)
	if err == nil || !strings.Contains(err.Error(), "does not match this OAuth identity") {
		t.Fatalf("expected an identity-pin-change refusal under --yes, got %v", err)
	}
	if !strings.Contains(err.Error(), "--allow-identity-change") {
		t.Errorf("error must name the override flag, got: %v", err)
	}
	if len(*revoked) != 1 || (*revoked)[0] != testRotatingAccessToken {
		t.Errorf("revoked tokens = %v, want exactly [%s]", *revoked, testRotatingAccessToken)
	}
	for _, r := range parseOpRefs(opRefsFileContent(t)) {
		if r.key == "SLACK_TEAM_ID" && r.value != "T_OLD" {
			t.Errorf("the OLD pin must be untouched on refusal, got %s=%s", r.key, r.value)
		}
	}
}

// TestSlackSetupPKCEAllowsDifferentIdentityInteractivelyOnConfirm proves an
// interactive user CAN confirm past the identity-pin-change gate, and the
// new identity is then pinned as normal.
func TestSlackSetupPKCEAllowsDifferentIdentityInteractivelyOnConfirm(t *testing.T) {
	deps, opts, env, cfg, revoked := slackOAuthRevokeFixture(t, func(string) (slackIdentity, error) {
		return slackIdentity{Team: "Acme", TeamID: "T123", User: "jane", UserID: "U456"}, nil
	})
	opts.assumeYes = false
	opRefsWith(t, "SLACK_TEAM_ID=T_OLD", "SLACK_USER_ID=U_OLD")
	// Two prompts now: "replace the pin?" then "wire up Slack?" — both answered y.
	var out bytes.Buffer
	err := slackSetupPKCE(env, cfg, opts, deps, strings.NewReader("y\ny\n"), &out, true, fakeHostResolver)
	if err != nil {
		t.Fatalf("slackSetupPKCE: %v\n--- output ---\n%s", err, out.String())
	}
	if len(*revoked) != 0 {
		t.Errorf("a confirmed identity change must not revoke anything, got: %v", *revoked)
	}
	for _, r := range parseOpRefs(opRefsFileContent(t)) {
		if r.key == "SLACK_TEAM_ID" && r.value != "T123" {
			t.Errorf("the pin must be replaced with the new identity, got %s=%s", r.key, r.value)
		}
	}
}

// TestSlackSetupPKCEDeclinesDifferentIdentityInteractively proves declining
// the identity-change prompt aborts (revoking the minted grant) WITHOUT
// reaching the ordinary wire-up confirmation at all.
func TestSlackSetupPKCEDeclinesDifferentIdentityInteractively(t *testing.T) {
	deps, opts, env, cfg, revoked := slackOAuthRevokeFixture(t, func(string) (slackIdentity, error) {
		return slackIdentity{Team: "Acme", TeamID: "T123", User: "jane", UserID: "U456"}, nil
	})
	opts.assumeYes = false
	opRefsWith(t, "SLACK_TEAM_ID=T_OLD", "SLACK_USER_ID=U_OLD")
	var out bytes.Buffer
	err := slackSetupPKCE(env, cfg, opts, deps, strings.NewReader("n\n"), &out, true, fakeHostResolver)
	if err != nil {
		t.Fatalf("a declined confirmation must not itself be an error, got %v", err)
	}
	if !strings.Contains(out.String(), "aborted") {
		t.Errorf("output must say aborted, got:\n%s", out.String())
	}
	if len(*revoked) != 1 || (*revoked)[0] != testRotatingAccessToken {
		t.Errorf("revoked tokens = %v, want exactly [%s]", *revoked, testRotatingAccessToken)
	}
	for _, r := range parseOpRefs(opRefsFileContent(t)) {
		if r.key == "SLACK_TEAM_ID" && r.value != "T_OLD" {
			t.Errorf("the OLD pin must be untouched, got %s=%s", r.key, r.value)
		}
	}
}

// TestSlackSetupPKCEAllowIdentityChangeSkipsGateUnderYes proves
// --allow-identity-change lets --yes proceed past a different identity
// without any extra prompt.
func TestSlackSetupPKCEAllowIdentityChangeSkipsGateUnderYes(t *testing.T) {
	deps, opts, env, cfg, revoked := slackOAuthRevokeFixture(t, func(string) (slackIdentity, error) {
		return slackIdentity{Team: "Acme", TeamID: "T123", User: "jane", UserID: "U456"}, nil
	})
	opts.allowIdentityChange = true
	opRefsWith(t, "SLACK_TEAM_ID=T_OLD", "SLACK_USER_ID=U_OLD")
	var out bytes.Buffer
	err := slackSetupPKCE(env, cfg, opts, deps, strings.NewReader(""), &out, false, fakeHostResolver)
	if err != nil {
		t.Fatalf("slackSetupPKCE: %v\n--- output ---\n%s", err, out.String())
	}
	if len(*revoked) != 0 {
		t.Errorf("an allowed identity change must not revoke anything, got: %v", *revoked)
	}
}

// TestSlackSetupPKCESameIdentityNeverTriggersTheGate proves a re-run against
// the SAME already-pinned identity never even reaches the extra prompt (only
// the ordinary wire-up confirmation).
func TestSlackSetupPKCESameIdentityNeverTriggersTheGate(t *testing.T) {
	deps, opts, env, cfg, revoked := slackOAuthRevokeFixture(t, func(string) (slackIdentity, error) {
		return slackIdentity{Team: "Acme", TeamID: "T123", User: "jane", UserID: "U456"}, nil
	})
	opRefsWith(t, "SLACK_TEAM_ID=T123", "SLACK_USER_ID=U456")
	var out bytes.Buffer
	err := slackSetupPKCE(env, cfg, opts, deps, strings.NewReader(""), &out, false, fakeHostResolver)
	if err != nil {
		t.Fatalf("slackSetupPKCE: %v\n--- output ---\n%s", err, out.String())
	}
	if len(*revoked) != 0 {
		t.Errorf("the same identity must never revoke anything, got: %v", *revoked)
	}
}

// TestSlackSetupPKCEDoesNotRevokeAfterDocumentPersisted proves that ONCE the
// 1Password document is durably written, a LATER failure (here: the
// provider-refs lock cannot be acquired, failing the SLACK_TEAM_ID ref write)
// never triggers an auto-revoke — the credential is real and recoverable
// from this point on (slackOAuthOrphanNote), not something to tear down
// automatically.
func TestSlackSetupPKCEDoesNotRevokeAfterDocumentPersisted(t *testing.T) {
	deps, opts, env, cfg, revoked := slackOAuthRevokeFixture(t, func(string) (slackIdentity, error) {
		return slackIdentity{Team: "Acme", TeamID: "T123", User: "jane", UserID: "U456"}, nil
	})
	fakeOf(env).LockFn = func(string, func() error) error { return fmt.Errorf("lock busy") }
	var out bytes.Buffer
	err := slackSetupPKCE(env, cfg, opts, deps, strings.NewReader(""), &out, false, fakeHostResolver)
	if err == nil || !strings.Contains(err.Error(), "writing SLACK_TEAM_ID") {
		t.Fatalf("expected the SLACK_TEAM_ID ref write to fail, got %v", err)
	}
	if !strings.Contains(err.Error(), "1Password document") {
		t.Errorf("error must carry the orphan-document note once persisted, got: %v", err)
	}
	if len(*revoked) != 0 {
		t.Errorf("revoke must NOT be called once the document was already persisted, but got calls: %v", *revoked)
	}
}
