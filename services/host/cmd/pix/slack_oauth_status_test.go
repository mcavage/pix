// slack_oauth_status_test.go covers `pix slack status`/`disable`'s OAuth-mode
// paths (slack_oauth.go's runtime section): the happy status path never
// demanding SLACK_TOKEN, the grant-expiry countdown's warning/expired
// thresholds, disable's revoke-then-archive-then-remove ordering, its
// failure-leaves-everything-in-place rollback discipline, the archive call's
// argv shape, and that config clears the OAuth-managed fields while
// retaining client_id/redirect_uri.
package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"pix/host/config"
	"pix/host/readiness"
	"pix/host/slackoauth"
)

// slackOAuthRuntimeFakeRunner is a slackoauth.CommandRunner for these tests:
// it never shells out, and routes by OPERATION (document get/delete) rather
// than call order, so one fake serves read, refresh, and delete without
// depending on exact call sequencing. calls records everything (name+args)
// in order, shared across the whole disable flow, so ordering assertions
// (revoke -> delete -> ...) can be made against a single timeline alongside
// env.Run's sbx calls (see orderRecorder below).
type slackOAuthRuntimeFakeRunner struct {
	mu        sync.Mutex
	getBlob   []byte
	getErr    error
	deleteErr error
	calls     [][]string
	onCall    func(op string) // notified (e.g. to append to a shared order log) before returning
}

func (f *slackOAuthRuntimeFakeRunner) Run(_ context.Context, _ []byte, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{name}, args...))
	f.mu.Unlock()
	if len(args) >= 2 && args[0] == "document" {
		switch args[1] {
		case "get":
			if f.onCall != nil {
				f.onCall("op:document:get")
			}
			if f.getErr != nil {
				return nil, f.getErr
			}
			return f.getBlob, nil
		case "delete":
			if f.onCall != nil {
				f.onCall("op:document:delete")
			}
			if f.deleteErr != nil {
				return nil, f.deleteErr
			}
			return nil, nil
		}
	}
	return nil, fmt.Errorf("slackOAuthRuntimeFakeRunner: unexpected op call: %v", args)
}

// slackOAuthTestBlob builds a valid v1 credential blob for these tests.
func slackOAuthTestBlob(teamID, userID string, accessExpiresAt, grantExpiresAt time.Time) []byte {
	b := slackoauth.Blob{
		Version:         slackoauth.BlobVersion,
		AccessToken:     "xoxe.xoxp-test-access-token",
		RefreshToken:    "xoxe-test-refresh-token",
		AccessExpiresAt: accessExpiresAt,
		GrantExpiresAt:  grantExpiresAt,
		TeamID:          teamID,
		UserID:          userID,
		Scopes:          append([]string(nil), slackoauth.RequiredUserScopes...),
	}
	data, err := slackoauth.MarshalBlob(b)
	if err != nil {
		panic(err) // test fixture construction only; a bad blob here is a test bug
	}
	return data
}

// slackOAuthTestConfig seeds and saves a COMPLETE [slack] OAuth wiring
// (client id + both 1Password locators + the given cached grant expiry) into
// the temp config slackTestCfg pointed PIX_CONFIG at.
func slackOAuthTestConfig(t *testing.T, grantExpiresAt time.Time) *config.Config {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	cfg.SetSlackClientID("C0PKCE")
	cfg.SetSlackRedirectURI("http://localhost:17373/slack/callback")
	cfg.SetSlackOAuthVaultID("vault-xyz")
	cfg.SetSlackOAuthDocumentID("item-abc")
	cfg.SetSlackOAuthGrantExpiresAt(grantExpiresAt)
	if err := cfg.Save(); err != nil {
		t.Fatalf("config.Save: %v", err)
	}
	reloaded, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load after save: %v", err)
	}
	return reloaded
}

// withSlackOAuthRuntimeDeps overrides slackOAuthRuntimeDepsFn for the
// duration of one test, restoring the production factory on cleanup.
func withSlackOAuthRuntimeDeps(t *testing.T, deps slackOAuthRuntimeDeps) {
	t.Helper()
	old := slackOAuthRuntimeDepsFn
	slackOAuthRuntimeDepsFn = func() slackOAuthRuntimeDeps { return deps }
	t.Cleanup(func() { slackOAuthRuntimeDepsFn = old })
}

// slackOAuthRegisteredEnv returns a shellEnv wired so the sbx registration
// reads back as the canonical, trusted Pix host command — i.e. the
// registration/attachment checks in status render ready, and disable's
// preflight recognizes it as safe to remove. stateDir resolves to a fresh
// per-test temp dir (never a real host state dir, and never an error —
// slackOAuthRuntime now FAILS CLOSED when stateDir is unresolvable, so a
// hermetic test needs a real, working, isolated one, not the in-process
// fallback this used to lean on).
func slackOAuthRegisteredEnv(t *testing.T, f *slackTestEnv, calls *[][]string, mu *sync.Mutex) shellEnv {
	t.Helper()
	stateDir := t.TempDir()
	e := f.env()
	e.HostBinary = func() (string, error) { return "/fake/bin/pix-host", nil }
	fakeOf(e).StateDirFn = func() (string, error) { return stateDir, nil }
	fakeOf(e).RunFn = func(name string, args ...string) (string, error) {
		if mu != nil {
			mu.Lock()
			*calls = append(*calls, append([]string{name}, args...))
			mu.Unlock()
		}
		if name == "sbx" && len(args) >= 2 && args[0] == "mcp" && args[1] == "ls" {
			return "slack\n", nil
		}
		if name == "sbx" && len(args) >= 3 && args[0] == "mcp" && args[1] == "get" {
			return "name: slack\ncommand: /fake/bin/pix-host mcp slack\n", nil
		}
		return "", nil
	}
	return e
}

// --- status: OAuth happy path never demands SLACK_TOKEN -------------------

func TestSlackStatusOAuthHappyPathNoSlackToken(t *testing.T) {
	slackTestCfg(t)
	cfg := slackOAuthTestConfig(t, time.Now().Add(20*24*time.Hour))
	opRefsWith(t, "SLACK_TEAM_ID=T123", "SLACK_USER_ID=U456") // no SLACK_TOKEN line at all

	runner := &slackOAuthRuntimeFakeRunner{
		getBlob: slackOAuthTestBlob("T123", "U456", time.Now().Add(time.Hour), time.Now().Add(20*24*time.Hour)),
	}
	withSlackOAuthRuntimeDeps(t, slackOAuthRuntimeDeps{runner: runner, clock: slackoauth.SystemClock{}})

	f := &slackTestEnv{sbxPresent: true, authTest: func(tok string) (slackIdentity, error) {
		if strings.Contains(tok, "refresh") {
			t.Errorf("auth.test called with what looks like a refresh token: %q", tok)
		}
		return slackIdentity{Team: "Acme", TeamID: "T123", User: "jane", UserID: "U456"}, nil
	}}
	var calls [][]string
	var mu sync.Mutex
	e := slackOAuthRegisteredEnv(t, f, &calls, &mu)

	var out bytes.Buffer
	exit := slackStatus(cfg, e, &out, time.Now())
	text := out.String()
	if exit != 0 {
		t.Errorf("exit = %d, want 0, output:\n%s", exit, text)
	}
	if strings.Contains(text, "SLACK_TOKEN") {
		t.Errorf("OAuth mode status must never mention SLACK_TOKEN, got:\n%s", text)
	}
	for _, want := range []string{"OAuth", "vault-xyz", "item-abc", "access", "identity", "matches the identity pinned"} {
		if !strings.Contains(text, want) {
			t.Errorf("status output missing %q, got:\n%s", want, text)
		}
	}
}

// --- status: grant expiry countdown ----------------------------------------

func TestSlackStatusOAuthGrantExpiryWarningWithin7Days(t *testing.T) {
	slackTestCfg(t)
	cfg := slackOAuthTestConfig(t, time.Now().Add(3*24*time.Hour))
	opRefsWith(t, "SLACK_TEAM_ID=T123", "SLACK_USER_ID=U456")

	runner := &slackOAuthRuntimeFakeRunner{
		getBlob: slackOAuthTestBlob("T123", "U456", time.Now().Add(time.Hour), time.Now().Add(3*24*time.Hour)),
	}
	withSlackOAuthRuntimeDeps(t, slackOAuthRuntimeDeps{runner: runner, clock: slackoauth.SystemClock{}})

	f := &slackTestEnv{sbxPresent: true, authTest: func(string) (slackIdentity, error) {
		return slackIdentity{Team: "Acme", TeamID: "T123", User: "jane", UserID: "U456"}, nil
	}}
	var calls [][]string
	var mu sync.Mutex
	e := slackOAuthRegisteredEnv(t, f, &calls, &mu)

	var out bytes.Buffer
	exit := slackStatus(cfg, e, &out, time.Now())
	text := out.String()
	if exit != 0 {
		t.Errorf("a warning-only expiry must not block exit; exit = %d, output:\n%s", exit, text)
	}
	if !strings.Contains(text, "expires in") || !strings.Contains(text, "renew soon") {
		t.Errorf("status must warn about the approaching expiry, got:\n%s", text)
	}
}

func TestSlackStatusOAuthGrantExpiredIsTodoAndExit1(t *testing.T) {
	slackTestCfg(t)
	cfg := slackOAuthTestConfig(t, time.Now().Add(-2*24*time.Hour))
	opRefsWith(t, "SLACK_TEAM_ID=T123", "SLACK_USER_ID=U456")

	runner := &slackOAuthRuntimeFakeRunner{
		getErr: slackoauth.ErrGrantExpired,
	}
	withSlackOAuthRuntimeDeps(t, slackOAuthRuntimeDeps{runner: runner, clock: slackoauth.SystemClock{}})

	f := &slackTestEnv{sbxPresent: true}
	var calls [][]string
	var mu sync.Mutex
	e := slackOAuthRegisteredEnv(t, f, &calls, &mu)

	var out bytes.Buffer
	exit := slackStatus(cfg, e, &out, time.Now())
	text := out.String()
	if exit != 1 {
		t.Errorf("an expired grant must be a verified TODO (exit 1), got exit=%d, output:\n%s", exit, text)
	}
	if !strings.Contains(text, "expired") {
		t.Errorf("status must report the grant as expired, got:\n%s", text)
	}
	if !strings.Contains(text, "pix slack auth") {
		t.Errorf("status must name pix slack auth as the fix, got:\n%s", text)
	}
}

// --- disable: revoke -> archive -> remove ordering -------------------------

func TestSlackDisableOAuthOrdering(t *testing.T) {
	slackTestCfg(t)
	cfg := slackOAuthTestConfig(t, time.Now().Add(20*24*time.Hour))
	opRefsWith(t, "SLACK_TEAM_ID=T123", "SLACK_USER_ID=U456")

	var mu sync.Mutex
	var order []string
	runner := &slackOAuthRuntimeFakeRunner{
		getBlob: slackOAuthTestBlob("T123", "U456", time.Now().Add(time.Hour), time.Now().Add(20*24*time.Hour)),
		onCall: func(op string) {
			mu.Lock()
			order = append(order, op)
			mu.Unlock()
		},
	}
	revokeCalled := false
	deps := slackOAuthRuntimeDeps{
		runner: runner, clock: slackoauth.SystemClock{},
		revoke: func(tok string) (bool, error) {
			mu.Lock()
			order = append(order, "revoke")
			mu.Unlock()
			revokeCalled = true
			if strings.TrimSpace(tok) == "" {
				t.Error("revoke called with an empty token")
			}
			return true, nil
		},
	}

	f := &slackTestEnv{sbxPresent: true}
	var calls [][]string
	e := slackOAuthRegisteredEnv(t, f, &calls, &mu)
	// Wrap env.Run so its calls land in the SAME order log as revoke/op calls.
	inner := fakeOf(e).RunFn
	fakeOf(e).RunFn = func(name string, args ...string) (string, error) {
		if name == "sbx" && len(args) >= 3 && args[0] == "mcp" && args[1] == "rm" {
			mu.Lock()
			order = append(order, "sbx:mcp:rm")
			mu.Unlock()
		}
		return inner(name, args...)
	}

	var out bytes.Buffer
	if err := slackDisableOAuth(cfg, e, &out, deps); err != nil {
		t.Fatalf("slackDisableOAuth: %v\n--- output ---\n%s", err, out.String())
	}
	if !revokeCalled {
		t.Fatal("revoke was never called")
	}

	mu.Lock()
	got := append([]string(nil), order...)
	mu.Unlock()
	wantPrefix := []string{"op:document:get", "revoke", "op:document:delete", "sbx:mcp:rm"}
	if len(got) < len(wantPrefix) {
		t.Fatalf("order = %v, want at least %v", got, wantPrefix)
	}
	for i, want := range wantPrefix {
		if got[i] != want {
			t.Errorf("order[%d] = %q, want %q (full order: %v)", i, got[i], want, got)
		}
	}
}

// --- disable: revoke failure leaves everything untouched -------------------

// TestSlackDisableOAuthRevokeFailureLeavesLocalWiringUntouched proves an
// ARBITRARY (non-Slack-typed) revoke failure — a transient network/op
// problem, not a proof the credential is already dead — still aborts with
// nothing removed. Contrast with TestSlackDisableOAuthContinuesWhenRevoke
// ReportsAlreadyDead below, where a TYPED invalid_auth/token_revoked response
// continues cleanup instead.
func TestSlackDisableOAuthRevokeFailureLeavesLocalWiringUntouched(t *testing.T) {
	slackTestCfg(t)
	cfg := slackOAuthTestConfig(t, time.Now().Add(20*24*time.Hour))
	opRefsWith(t, "SLACK_TEAM_ID=T123", "SLACK_USER_ID=U456")

	runner := &slackOAuthRuntimeFakeRunner{
		getBlob: slackOAuthTestBlob("T123", "U456", time.Now().Add(time.Hour), time.Now().Add(20*24*time.Hour)),
	}
	deps := slackOAuthRuntimeDeps{
		runner: runner, clock: slackoauth.SystemClock{},
		revoke: func(string) (bool, error) { return false, fmt.Errorf("network timeout calling slack") },
	}

	f := &slackTestEnv{sbxPresent: true}
	var calls [][]string
	var mu sync.Mutex
	e := slackOAuthRegisteredEnv(t, f, &calls, &mu)

	var out bytes.Buffer
	err := slackDisableOAuth(cfg, e, &out, deps)
	if err == nil || !strings.Contains(err.Error(), "nothing was removed") {
		t.Fatalf("a revoke failure must fail with 'nothing was removed', got %v", err)
	}

	runner.mu.Lock()
	deleteCalled := false
	for _, c := range runner.calls {
		if len(c) >= 3 && c[0] == "op" && c[1] == "document" && c[2] == "delete" {
			deleteCalled = true
		}
	}
	runner.mu.Unlock()
	if deleteCalled {
		t.Error("the 1Password document must never be deleted when revoke fails")
	}

	mu.Lock()
	for _, c := range calls {
		if len(c) >= 3 && c[0] == "sbx" && c[1] == "mcp" && c[2] == "rm" {
			t.Errorf("the registration must never be removed when revoke fails: calls=%v", calls)
		}
	}
	mu.Unlock()

	reloaded, lerr := config.Load()
	if lerr != nil {
		t.Fatalf("config.Load: %v", lerr)
	}
	if reloaded.Slack.OAuthVaultID == "" || reloaded.Slack.OAuthDocumentID == "" {
		t.Error("config must be untouched when revoke fails")
	}
	for _, r := range parseOpRefs(opRefsFileContent(t)) {
		if r.key == "SLACK_TEAM_ID" && r.value != "T123" {
			t.Error("identity pin refs must be untouched when revoke fails")
		}
	}
}

// --- disable: archive failure after a confirmed revoke ----------------------

func TestSlackDisableOAuthArchiveFailureReportsIncomplete(t *testing.T) {
	slackTestCfg(t)
	cfg := slackOAuthTestConfig(t, time.Now().Add(20*24*time.Hour))
	opRefsWith(t, "SLACK_TEAM_ID=T123", "SLACK_USER_ID=U456")

	runner := &slackOAuthRuntimeFakeRunner{
		getBlob:   slackOAuthTestBlob("T123", "U456", time.Now().Add(time.Hour), time.Now().Add(20*24*time.Hour)),
		deleteErr: fmt.Errorf("op: vault locked"),
	}
	deps := slackOAuthRuntimeDeps{
		runner: runner, clock: slackoauth.SystemClock{},
		revoke: func(string) (bool, error) { return true, nil },
	}

	f := &slackTestEnv{sbxPresent: true}
	var calls [][]string
	var mu sync.Mutex
	e := slackOAuthRegisteredEnv(t, f, &calls, &mu)

	var out bytes.Buffer
	err := slackDisableOAuth(cfg, e, &out, deps)
	if err == nil {
		t.Fatal("an archive failure after revoke must be reported, not swallowed")
	}
	if !strings.Contains(err.Error(), "revoked") || !strings.Contains(err.Error(), "incomplete") {
		t.Errorf("error must read as revoked-but-local-cleanup-incomplete, got: %v", err)
	}

	mu.Lock()
	for _, c := range calls {
		if len(c) >= 3 && c[0] == "sbx" && c[1] == "mcp" && c[2] == "rm" {
			t.Errorf("the registration must never be removed when the archive fails: calls=%v", calls)
		}
	}
	mu.Unlock()

	reloaded, lerr := config.Load()
	if lerr != nil {
		t.Fatalf("config.Load: %v", lerr)
	}
	if reloaded.Slack.OAuthVaultID == "" {
		t.Error("config must stay untouched when the archive fails after a confirmed revoke")
	}
}

// --- disable: document archive argv shape -----------------------------------

func TestSlackDisableOAuthDocumentArchiveArgv(t *testing.T) {
	slackTestCfg(t)
	cfg := slackOAuthTestConfig(t, time.Now().Add(20*24*time.Hour))
	opRefsWith(t, "SLACK_TEAM_ID=T123", "SLACK_USER_ID=U456")

	runner := &slackOAuthRuntimeFakeRunner{
		getBlob: slackOAuthTestBlob("T123", "U456", time.Now().Add(time.Hour), time.Now().Add(20*24*time.Hour)),
	}
	deps := slackOAuthRuntimeDeps{
		runner: runner, clock: slackoauth.SystemClock{},
		revoke: func(string) (bool, error) { return true, nil },
	}

	f := &slackTestEnv{sbxPresent: true}
	var calls [][]string
	var mu sync.Mutex
	e := slackOAuthRegisteredEnv(t, f, &calls, &mu)

	var out bytes.Buffer
	if err := slackDisableOAuth(cfg, e, &out, deps); err != nil {
		t.Fatalf("slackDisableOAuth: %v\n--- output ---\n%s", err, out.String())
	}

	runner.mu.Lock()
	defer runner.mu.Unlock()
	var deleteCall []string
	for _, c := range runner.calls {
		if len(c) >= 3 && c[0] == "op" && c[1] == "document" && c[2] == "delete" {
			deleteCall = c
		}
	}
	if deleteCall == nil {
		t.Fatalf("expected an op document delete call, got calls=%v", runner.calls)
	}
	wantCall := []string{"op", "document", "delete", "item-abc", "--vault", "vault-xyz", "--archive"}
	if strings.Join(deleteCall, " ") != strings.Join(wantCall, " ") {
		t.Errorf("delete call = %v, want %v", deleteCall, wantCall)
	}
	// The secret credential must never appear anywhere on argv — there is none
	// to leak here, but assert the shape anyway so a future change can't
	// accidentally start passing the blob on the command line.
	for _, secret := range []string{"xoxe.xoxp-test-access-token", "xoxe-test-refresh-token"} {
		if strings.Contains(strings.Join(deleteCall, " "), secret) {
			t.Errorf("delete argv leaked a secret: %v", deleteCall)
		}
	}
}

// --- disable: config clear retains client_id/redirect_uri -------------------

func TestSlackDisableOAuthConfigClearRetainsClientID(t *testing.T) {
	slackTestCfg(t)
	cfg := slackOAuthTestConfig(t, time.Now().Add(20*24*time.Hour))
	cfg.AddMCP(slackServerName)
	if err := cfg.Save(); err != nil {
		t.Fatalf("config.Save: %v", err)
	}
	cfg, _ = config.Load()
	opRefsWith(t, "SLACK_TEAM_ID=T123", "SLACK_USER_ID=U456")

	runner := &slackOAuthRuntimeFakeRunner{
		getBlob: slackOAuthTestBlob("T123", "U456", time.Now().Add(time.Hour), time.Now().Add(20*24*time.Hour)),
	}
	withSlackOAuthRuntimeDeps(t, slackOAuthRuntimeDeps{
		runner: runner, clock: slackoauth.SystemClock{},
		revoke: func(string) (bool, error) { return true, nil },
	})

	f := &slackTestEnv{sbxPresent: true}
	var calls [][]string
	var mu sync.Mutex
	e := slackOAuthRegisteredEnv(t, f, &calls, &mu)

	var out bytes.Buffer
	if err := slackDisable(cfg, e, &out); err != nil {
		t.Fatalf("slackDisable: %v\n--- output ---\n%s", err, out.String())
	}

	got, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if got.Slack.ClientID != "C0PKCE" {
		t.Errorf("ClientID = %q, want retained C0PKCE", got.Slack.ClientID)
	}
	if got.Slack.RedirectURI != "http://localhost:17373/slack/callback" {
		t.Errorf("RedirectURI = %q, want retained", got.Slack.RedirectURI)
	}
	if got.Slack.OAuthVaultID != "" || got.Slack.OAuthDocumentID != "" {
		t.Errorf("OAuth vault/document = %q/%q, want cleared", got.Slack.OAuthVaultID, got.Slack.OAuthDocumentID)
	}
	if !got.Slack.OAuthGrantExpiresAt.IsZero() {
		t.Errorf("OAuthGrantExpiresAt = %v, want cleared", got.Slack.OAuthGrantExpiresAt)
	}
	if mcpConfigured(got, slackServerName) {
		t.Error("slack must be removed from the configured mcp set")
	}
	for _, r := range parseOpRefs(opRefsFileContent(t)) {
		if r.key == "SLACK_TEAM_ID" || r.key == "SLACK_USER_ID" {
			t.Errorf("identity pin %s must be removed, got:\n%s", r.key, opRefsFileContent(t))
		}
	}
}

// --- disable: foreign registration is never touched -------------------------

func TestSlackDisableOAuthNeverRemovesForeignRegistration(t *testing.T) {
	slackTestCfg(t)
	cfg := slackOAuthTestConfig(t, time.Now().Add(20*24*time.Hour))

	revokeCalled := false
	deps := slackOAuthRuntimeDeps{
		revoke: func(string) (bool, error) { revokeCalled = true; return true, nil },
	}

	f := &slackTestEnv{sbxPresent: true}
	e := f.env()
	fakeOf(e).RunFn = func(name string, args ...string) (string, error) {
		if name == "sbx" && len(args) >= 2 && args[0] == "mcp" && args[1] == "ls" {
			return "slack\n", nil
		}
		if name == "sbx" && len(args) >= 3 && args[0] == "mcp" && args[1] == "get" {
			return "name: slack\ncommand: /tmp/not-pix-host mcp slack\n", nil
		}
		if len(args) >= 2 && args[0] == "mcp" && args[1] == "rm" {
			t.Fatalf("a foreign registration must never be removed")
		}
		return "", nil
	}

	var out bytes.Buffer
	err := slackDisableOAuth(cfg, e, &out, deps)
	if err == nil || !strings.Contains(err.Error(), "refusing to remove") {
		t.Fatalf("foreign registration should be preserved, got %v", err)
	}
	if revokeCalled {
		t.Error("revoke must never be called before the registration preflight passes")
	}
}

// --- disable: retryable after a manual/prior revocation or dead grant ------

// TestSlackDisableOAuthContinuesWhenTokenObtainFailsWithDeadCredential proves
// that when the runtime manager cannot obtain ANY token because the
// credential is already provably dead (an expired grant, or a refresh chain
// Slack itself rejected as invalid_refresh_token/token_revoked), disable
// treats that as "nothing left to revoke" and continues the archive/local
// cleanup, rather than aborting forever on a credential that can never come
// back to life.
func TestSlackDisableOAuthContinuesWhenTokenObtainFailsWithDeadCredential(t *testing.T) {
	slackTestCfg(t)
	cfg := slackOAuthTestConfig(t, time.Now().Add(-2*24*time.Hour))
	opRefsWith(t, "SLACK_TEAM_ID=T123", "SLACK_USER_ID=U456")

	runner := &slackOAuthRuntimeFakeRunner{getErr: slackoauth.ErrGrantExpired}
	revokeCalled := false
	deps := slackOAuthRuntimeDeps{
		runner: runner, clock: slackoauth.SystemClock{},
		revoke: func(string) (bool, error) { revokeCalled = true; return true, nil },
	}

	f := &slackTestEnv{sbxPresent: true}
	var calls [][]string
	var mu sync.Mutex
	e := slackOAuthRegisteredEnv(t, f, &calls, &mu)

	var out bytes.Buffer
	if err := slackDisableOAuth(cfg, e, &out, deps); err != nil {
		t.Fatalf("slackDisableOAuth: %v\n--- output ---\n%s", err, out.String())
	}
	if revokeCalled {
		t.Error("live revoke must never be attempted once the grant is already expired")
	}
	if !strings.Contains(out.String(), "already invalid") {
		t.Errorf("output must explain the credential was already dead, got:\n%s", out.String())
	}

	runner.mu.Lock()
	deleteCalled := false
	for _, c := range runner.calls {
		if len(c) >= 3 && c[0] == "op" && c[1] == "document" && c[2] == "delete" {
			deleteCalled = true
		}
	}
	runner.mu.Unlock()
	if !deleteCalled {
		t.Error("local cleanup (archiving the 1Password document) must still proceed")
	}
}

// TestSlackDisableOAuthContinuesWhenRevokeReportsAlreadyDead proves that when
// a live token WAS obtained but Slack's own auth.revoke says it is already
// dead (invalid_auth or token_revoked \u2014 e.g. a human revoked it by hand, or
// a prior disable run revoked it but failed before archiving), disable
// continues cleanup instead of aborting.
func TestSlackDisableOAuthContinuesWhenRevokeReportsAlreadyDead(t *testing.T) {
	slackTestCfg(t)
	cfg := slackOAuthTestConfig(t, time.Now().Add(20*24*time.Hour))
	opRefsWith(t, "SLACK_TEAM_ID=T123", "SLACK_USER_ID=U456")

	runner := &slackOAuthRuntimeFakeRunner{
		getBlob: slackOAuthTestBlob("T123", "U456", time.Now().Add(time.Hour), time.Now().Add(20*24*time.Hour)),
	}
	deps := slackOAuthRuntimeDeps{
		runner: runner, clock: slackoauth.SystemClock{},
		revoke: func(string) (bool, error) {
			return false, slackoauth.ClassifyAPIError("auth.revoke", "invalid_auth")
		},
	}

	f := &slackTestEnv{sbxPresent: true}
	var calls [][]string
	var mu sync.Mutex
	e := slackOAuthRegisteredEnv(t, f, &calls, &mu)

	var out bytes.Buffer
	if err := slackDisableOAuth(cfg, e, &out, deps); err != nil {
		t.Fatalf("slackDisableOAuth: %v\n--- output ---\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "already invalid/revoked") {
		t.Errorf("output must explain Slack already reports it dead, got:\n%s", out.String())
	}
	if strings.Contains(out.String(), "revoked the Slack OAuth token") {
		t.Errorf("output must not claim a live revoke happened when none did, got:\n%s", out.String())
	}

	runner.mu.Lock()
	deleteCalled := false
	for _, c := range runner.calls {
		if len(c) >= 3 && c[0] == "op" && c[1] == "document" && c[2] == "delete" {
			deleteCalled = true
		}
	}
	runner.mu.Unlock()
	if !deleteCalled {
		t.Error("local cleanup (archiving the 1Password document) must still proceed")
	}
}

// --- runtime: fail closed when the shared state dir can't be resolved -----

// TestSlackOAuthRuntimeFailsClosedWithoutStateDir proves slackOAuthRuntime
// refuses (ok=false) rather than silently falling back to a process-local
// Locker when the shared state dir cannot be resolved \u2014 that fallback used
// to exist and would NOT actually serialize a refresh against a concurrently
// running gateway process sharing the same 1Password document (the entire
// point of the FileLock), a silent race this must fail closed against
// instead.
func TestSlackOAuthRuntimeFailsClosedWithoutStateDir(t *testing.T) {
	slackTestCfg(t)
	cfg := slackOAuthTestConfig(t, time.Now().Add(20*24*time.Hour))

	f := &slackTestEnv{sbxPresent: true}
	e := f.env()
	fakeOf(e).StateDirFn = func() (string, error) { return "", fmt.Errorf("$HOME could not be determined") }

	_, _, ok := slackOAuthRuntime(cfg, e, slackOAuthRuntimeDeps{runner: &slackOAuthRuntimeFakeRunner{}, clock: slackoauth.SystemClock{}})
	if ok {
		t.Fatal("slackOAuthRuntime must fail closed (ok=false) when the state dir cannot be resolved")
	}

	// Both status and disable must surface this as a real failure, never a
	// silent skip.
	checks := slackOAuthStatusChecks(cfg, e, time.Now())
	foundUnverifiableAccess := false
	for _, c := range checks {
		if c.Label == "access" && c.Verdict == readiness.VerdictUnverifiable {
			foundUnverifiableAccess = true
		}
	}
	if !foundUnverifiableAccess {
		t.Errorf("status must report an unverifiable access readiness.Check when the runtime can't be built, got: %+v", checks)
	}

	var out bytes.Buffer
	if err := slackDisableOAuth(cfg, e, &out, slackOAuthRuntimeDeps{}); err == nil {
		t.Fatal("slackDisableOAuth must fail when the runtime cannot be built")
	}
}
