// slack_oauth.go is `pix slack setup`'s SECOND path: a localhost PKCE OAuth
// grant, used whenever --token-ref is absent. Unlike the static-ref flow in
// slack.go (which only ever accepts a token minted somewhere else), this path
// DOES talk to Slack's authorize endpoint — but it is still a public PKCE
// client: no client_secret is ever held, requested, or sent. See
// docs/design/slack-setup.md.
//
// The shape, end to end:
//  1. generate a random state + PKCE code_verifier (crypto/rand), derive the
//     S256 code_challenge
//  2. bind the FIXED localhost callback port from --redirect-uri (or config)
//     BEFORE sending the user anywhere, so a busy port fails fast
//  3. print the authorize URL and try to open it in a browser (best-effort;
//     the URL is always printed as the fallback)
//  4. serve exactly one GET on the exact callback path, reject anything else
//     (wrong method, wrong path, an Origin header, a state mismatch),
//     constant-time-compare state, and NEVER log the callback query string
//  5. exchange the code via slackoauth.Client (PKCE, no client_secret)
//  6. verify the freshly minted rotating token LIVE via auth.test and
//     require it resolve to the SAME team/user ids the exchange reported
//  7. confirm (unless --yes), then persist: a 1Password document via
//     slackoauth.OPStore (the ENTIRE credential blob, stdin only, never
//     argv), the non-secret SLACK_TEAM_ID/SLACK_USER_ID pins, removal of any
//     legacy SLACK_TOKEN ref, then register + save config using the exact
//     same register-first/rollback discipline as the static flow.
//
// Runtime refresh (serving a live token to services/host/slack.go on an
// ongoing basis) is NOT implemented here — that is slackoauth.Manager's job,
// wired elsewhere. This file only ever performs the INITIAL (or a repeated,
// monthly-reauthorization) grant.
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"pix/host/config"
	"pix/host/slackoauth"
)

const (
	// slackOAuthAuthorizeURL is Slack's OAuth v2 authorize endpoint. Only ever
	// hit from the user's browser (opened by this command) — this process
	// never POSTs here itself.
	slackOAuthAuthorizeURL = "https://slack.com/oauth/v2/authorize"
	// slackOAuthCallbackPath is the ONLY path the local callback server ever
	// answers on the wire; the redirect_uri registered with Slack (and passed
	// via --redirect-uri/config) must resolve to exactly this path.
	slackOAuthCallbackPath = "/slack/callback"
	// slackOAuthTimeout bounds how long the local callback server waits for
	// the user to complete the browser flow before giving up.
	slackOAuthTimeout = 5 * time.Minute
	// slackOAuthStateBytes / slackOAuthVerifierBytes are the raw entropy sizes
	// (crypto/rand) for the OAuth state and PKCE code_verifier, before
	// base64url encoding.
	slackOAuthStateBytes    = 32
	slackOAuthVerifierBytes = 64
)

// slackOAuthUserScopes is the EXACT read-only Slack user_scope set
// services/host/slack.go's tool handlers call (docs/design/slack-setup.md's
// scope table). It is an alias for slackoauth.RequiredUserScopes — the
// package-level export both this authorize URL builder and the host
// runtime's credential source reference — so the two can never drift apart.
var slackOAuthUserScopes = slackoauth.RequiredUserScopes

// slackOAuthPKCESetupUsage documents the no---token-ref path; appended to
// slackSetupUsage.
const slackOAuthPKCESetupUsage = `
Without --token-ref, setup instead runs a LOCAL PKCE OAuth grant (no client
secret is ever held or requested):
  1. opens https://slack.com/oauth/v2/authorize in your browser (client_id +
     the exact read-only user_scope this command needs + a one-time PKCE
     challenge) — prints the URL too, in case a browser can't be launched
  2. serves exactly one callback on the fixed localhost redirect uri, verifies
     state (constant-time) and rejects anything else (wrong path/method, an
     Origin header, a mismatched state), and gives up after 5 minutes
  3. exchanges the code for a ROTATING token pair (slackoauth.Client, PKCE,
     no client_secret), then verifies the new token LIVE via auth.test and
     requires it resolve to the SAME team/user the exchange reported
  4. asks you to confirm (unless --yes), then writes the whole credential
     blob to a 1Password document (stdin only, never on argv), pins
     SLACK_TEAM_ID/SLACK_USER_ID, removes any legacy SLACK_TOKEN ref, and
     registers + saves config exactly like the --token-ref path above

The rotating grant expires monthly (30 days) — re-run this command before
then to reauthorize; that is a separate step from sandbox attachment.

flags (PKCE path only):
  --client-id <id>        Slack app client id (else config.toml's [slack] client_id)
  --redirect-uri <uri>    must be http://localhost:<fixed port>/slack/callback
                          (else config.toml's [slack] redirect_uri, else the
                          built-in default)
  --vault <name-or-id>    1Password vault for the OAuth document (default: Private)
`

// slackOAuthDeps bundles the PKCE flow's injected seams. Every field has a
// production default (see defaultSlackOAuthDeps); tests override individual
// fields to stay hermetic (no real browser, no real network, no real op CLI).
type slackOAuthDeps struct {
	// openBrowser launches url in a browser and must NOT block waiting for it
	// to exit. A failure is never fatal to the flow — the URL was already
	// printed as the fallback.
	openBrowser func(url string) error
	// runner is the slackoauth.CommandRunner used to create/edit the 1Password
	// document. Production: slackoauth.ExecRunner{} (the real `op` CLI, stdin
	// only). Tests inject a fake that never shells out.
	runner slackoauth.CommandRunner
	// doer is the slackoauth.HTTPDoer used for the code exchange. Production:
	// http.DefaultClient. Tests inject a fake that never hits the network.
	doer slackoauth.HTTPDoer
	// clock supplies "now" for slackoauth.Client's grant-expiry stamping.
	clock slackoauth.Clock
	// timeout overrides slackOAuthTimeout. Zero means the production default;
	// tests set a short one so a deliberately-uncompleted flow fails fast.
	timeout time.Duration
	// revoke calls Slack's auth.revoke with a live bearer token and reports
	// whether Slack CONFIRMED revocation — never just that the HTTP call
	// succeeded. Production: liveSlackAuthRevoke. Used for the BEST-EFFORT
	// cleanup of a freshly minted grant when setup fails after Exchange but
	// before the 1Password document is durably written (see
	// slackOAuthBestEffortRevoke) — never logs the token.
	revoke func(token string) (revoked bool, err error)
}

// defaultSlackOAuthDeps returns the production wiring: a real (non-blocking)
// browser launch, the real `op` CLI via ExecRunner, and the real HTTP client.
func defaultSlackOAuthDeps() slackOAuthDeps {
	return slackOAuthDeps{
		openBrowser: slackOpenBrowser,
		runner:      slackoauth.ExecRunner{},
		doer:        http.DefaultClient,
		clock:       slackoauth.SystemClock{},
		revoke:      liveSlackAuthRevoke,
	}
}

// slackOpenBrowser is the production browser launcher: `open` on macOS,
// `xdg-open` everywhere else. It uses Start(), never Run(), so it never
// blocks this process waiting for the browser to exit; its failure is never
// fatal — the caller always prints the URL as a fallback regardless.
func slackOpenBrowser(rawURL string) error {
	var cmd *exec.Cmd
	if runtime.GOOS == "darwin" {
		cmd = exec.Command("open", rawURL)
	} else {
		cmd = exec.Command("xdg-open", rawURL)
	}
	return cmd.Start()
}

// randomURLSafe returns n cryptographically random bytes (crypto/rand),
// base64url-encoded with no padding.
func randomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating %d random bytes: %w", n, err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// slackCodeChallengeS256 derives the PKCE S256 code_challenge from a
// code_verifier: base64url(sha256(verifier)), no padding.
func slackCodeChallengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// slackBuildAuthorizeURL builds Slack's oauth/v2/authorize URL: client_id,
// the exact read-only user_scope set (slackOAuthUserScopes), the exact
// redirect_uri, state, and the PKCE code_challenge/code_challenge_method.
// Never a "scope" param (that requests BOT scopes) and never a client_secret
// (this is a public PKCE client).
func slackBuildAuthorizeURL(clientID, redirectURI, state, codeChallenge string) string {
	q := url.Values{}
	q.Set("client_id", clientID)
	q.Set("user_scope", strings.Join(slackOAuthUserScopes, ","))
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("code_challenge", codeChallenge)
	q.Set("code_challenge_method", "S256")
	return slackOAuthAuthorizeURL + "?" + q.Encode()
}

// parseSlackRedirectURI validates redirectURI is EXACTLY
// http://localhost:<fixed port>/slack/callback, returning the port to bind
// and the exact path the callback handler must match. A relative/missing
// port, a non-localhost host, a non-http scheme, or any other path is
// refused outright — Slack's own redirect_uri byte-for-byte match makes a
// loose parse here worthless anyway.
func parseSlackRedirectURI(redirectURI string) (port int, path string, err error) {
	u, perr := url.Parse(strings.TrimSpace(redirectURI))
	if perr != nil {
		return 0, "", fmt.Errorf("invalid --redirect-uri: %w", perr)
	}
	if u.Scheme != "http" || u.Hostname() != "localhost" {
		return 0, "", fmt.Errorf("--redirect-uri must be http://localhost:<port>%s, got %q", slackOAuthCallbackPath, redirectURI)
	}
	if u.Path != slackOAuthCallbackPath {
		return 0, "", fmt.Errorf("--redirect-uri path must be exactly %s, got %q", slackOAuthCallbackPath, u.Path)
	}
	portStr := u.Port()
	if portStr == "" {
		return 0, "", fmt.Errorf("--redirect-uri must carry an explicit fixed port (http://localhost:<port>%s)", slackOAuthCallbackPath)
	}
	p, aerr := strconv.Atoi(portStr)
	if aerr != nil || p <= 0 || p > 65535 {
		return 0, "", fmt.Errorf("--redirect-uri has an invalid port %q", portStr)
	}
	return p, u.Path, nil
}

// slackBindCallbackListener binds 127.0.0.1:port for the one-shot OAuth
// callback. It fails immediately (rather than silently retrying or picking a
// different port) if the port is already in use — the redirect_uri Slack was
// just told about is a FIXED port, so there is no fallback port to pick.
func slackBindCallbackListener(port int) (net.Listener, error) {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("binding 127.0.0.1:%d for the Slack OAuth callback: %w "+
			"(is another pix slack setup already running, or something else using this port?)", port, err)
	}
	return ln, nil
}

// slackServeCallback serves exactly one successful GET to path on ln,
// returning the authorization code once state has been verified. Every other
// request (wrong method, wrong path, an Origin header, a state mismatch) is
// answered and discarded WITHOUT ending the wait. The random state makes
// guessing infeasible, while continuing to listen prevents a stray local probe
// from denying the genuine callback. The response is always static:
// no-store, no-referrer, and it never echoes back any part of the request.
// The raw query string is never logged or included in any returned error.
func slackServeCallback(ln net.Listener, path, wantState string, timeout time.Duration) (string, error) {
	resultCh := make(chan struct {
		code string
		err  error
	}, 1)
	var once sync.Once
	deliver := func(code string, err error) {
		once.Do(func() {
			resultCh <- struct {
				code string
				err  error
			}{code, err}
		})
	}

	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")

		if r.Method != http.MethodGet {
			// Not the real callback attempt (a stray retry, a HEAD probe, ...);
			// keep listening for the genuine one rather than ending the flow.
			w.WriteHeader(http.StatusMethodNotAllowed)
			fmt.Fprint(w, "method not allowed")
			return
		}
		if r.Header.Get("Origin") != "" {
			// A same-navigation top-level GET never legitimately carries an
			// Origin header; treat one as a CSRF-shaped probe: answer 4xx and
			// discard it, but do NOT end the wait — a genuine callback may still
			// arrive (e.g. a browser extension or proxy fired an extra probe
			// request first), so this behaves exactly like the wrong-path/
			// wrong-method cases below rather than failing the whole attempt.
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, "forbidden")
			return
		}
		q := r.URL.Query()
		gotState := q.Get("state")
		if subtle.ConstantTimeCompare([]byte(gotState), []byte(wantState)) != 1 {
			// Same reasoning as the Origin check above: answer 4xx and discard,
			// but keep waiting for the genuine callback rather than failing the
			// attempt on a stray/stale/mismatched request.
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "state mismatch")
			return
		}
		if errParam := q.Get("error"); errParam != "" {
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "You can close this tab and return to the terminal.")
			deliver("", fmt.Errorf("Slack returned an authorization error: %s", errParam))
			return
		}
		code := q.Get("code")
		if code == "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "missing authorization code")
			deliver("", errors.New("OAuth callback carried no authorization code"))
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "Slack authorized. You can close this tab and return to the terminal.")
		deliver(code, nil)
	})

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	select {
	case res := <-resultCh:
		return res.code, res.err
	case <-time.After(timeout):
		return "", fmt.Errorf("timed out waiting for the Slack OAuth callback after %s", timeout)
	}
}

// slackVerifyRotatingIdentity requires the LIVE auth.test identity for the
// freshly exchanged rotating token resolve to the SAME team/user ids the
// token exchange itself reported. A mismatch here would mean the exchange
// response and the live identity check disagree about whose credential this
// is — refuse rather than trust either one. Never references a token value,
// only ids, so this can never leak a secret regardless of which side is
// wrong.
func slackVerifyRotatingIdentity(id slackIdentity, blob slackoauth.Blob) error {
	if id.teamID != blob.TeamID || id.userID != blob.UserID {
		return fmt.Errorf("Slack auth.test identity (team %s / user %s) does not match the OAuth exchange identity (team %s / user %s); refusing to trust this credential",
			id.teamID, id.userID, blob.TeamID, blob.UserID)
	}
	return nil
}

// slackOAuthBestEffortRevoke performs a best-effort revoke of a freshly
// minted rotating grant's access token and returns a note describing the
// outcome, meant to be appended to any error (or printed alongside an abort)
// returned AFTER Client.Exchange succeeded but BEFORE the 1Password document
// was durably written (store.Write). A grant that only ever existed for a
// few seconds before this process decided to give up on it (a failed
// auth.test, an identity mismatch, a declined/non-TTY confirmation, or the
// 1Password write itself failing) should not be left live at Slack with
// nothing telling the operator so — but a revoke failing here is reported IN
// the note, never propagated as the primary error, since the caller already
// has a more important one to report. Once the document is persisted, this
// is NEVER called again for a later failure (see slackSetupPKCE) — from that
// point on the credential is real and recoverable (slackOAuthOrphanNote),
// not something to auto-revoke.
func slackOAuthBestEffortRevoke(deps slackOAuthDeps, accessToken string) string {
	revoke := deps.revoke
	if revoke == nil {
		revoke = liveSlackAuthRevoke
	}
	revoked, err := revoke(accessToken)
	if err != nil || !revoked {
		if err == nil {
			err = errors.New("Slack did not confirm the token was revoked")
		}
		return fmt.Sprintf("attempted to revoke the freshly minted OAuth grant but that failed too (%v); "+
			"it may remain active at Slack until you revoke it yourself (Slack's Apps management page)", err)
	}
	return "the freshly minted OAuth grant was revoked; nothing was persisted"
}

// slackOAuthOrphanNote builds the recovery text appended to every error that
// occurs AFTER the 1Password document was already written: from this point on
// a failure must never be silently swallowed, and must never claim success,
// because a real credential now lives in 1Password whether or not the rest
// of this command finishes.
func slackOAuthOrphanNote(itemID, vaultID string) string {
	return fmt.Sprintf("the 1Password document (item %s, vault %s) was already created and holds a valid Slack credential, "+
		"but this run failed before config.toml was updated to reference it. Either delete that 1Password item and re-run "+
		"`pix slack setup`, or fix the error above and re-run `pix slack setup` again (a rerun creates/edits its own document; "+
		"it will not automatically resume this one unless config already points at it)",
		itemID, vaultID)
}

// slackPrintAttachmentNote prints the closing "registration is not the same
// as attachment" reminder shared by both the static-token and PKCE setup
// paths (docs/design/slack-setup.md).
func slackPrintAttachmentNote(out io.Writer) {
	fmt.Fprintln(out, "Registration and sandbox attachment are separate:")
	fmt.Fprintln(out, "  a sandbox already running does not see it until recreated (pix run --replace)")
	fmt.Fprintln(out, "  or attached live (pix mcp load slack).")
}

// slackRegistrationPreflight runs the SAME preflight the static-token flow
// has always run, before ANY side effect: sbx must be present, and an
// existing slack registration (if any) must already be the canonical Pix
// host command — never silently overwritten. Returns whether a registration
// already existed, so the caller can decide whether a later failure may roll
// a NEW registration back (never a pre-existing one).
func slackRegistrationPreflight(env shellEnv) (wasRegistered bool, err error) {
	if env.run == nil {
		return false, fmt.Errorf("internal: shellEnv.run not wired")
	}
	if env.lookPath == nil {
		return false, fmt.Errorf("internal: shellEnv.lookPath not wired")
	}
	if _, err := env.lookPath("sbx"); err != nil {
		return false, fmt.Errorf("sbx not found: pix slack setup requires sbx to register the MCP server; install it, then re-run this command")
	}
	wasRegistered, err = slackRegistrationPresence(env)
	if err != nil {
		return false, err
	}
	if wasRegistered {
		argv, ok := registeredMCPCommand(env, slackServerName)
		if _, trusted := recognizedMCPArgv(env, argv, slackServerName); !ok || !trusted {
			return false, fmt.Errorf("the existing slack registration is not the canonical Pix host command; refusing to overwrite it (inspect: sbx mcp get slack)")
		}
	}
	return wasRegistered, nil
}

// slackRegisterAndSave applies the register-first/save-second discipline
// shared by both setup paths: a registration failure leaves whatever the
// caller already wrote (refs, and for PKCE the 1Password document) untouched;
// a save failure after a NEW registration rolls that registration back (a
// PRE-EXISTING one, per wasRegistered, is left alone). extra, when non-empty,
// is appended to every returned error (the PKCE flow's orphan-document note).
func slackRegisterAndSave(cfg *config.Config, env shellEnv, out io.Writer, hostResolver func() (string, error), wasRegistered bool, extra string) error {
	if err := registerServers(cfg, env, out, []string{slackServerName}, hostResolver, nil); err != nil {
		if extra != "" {
			return fmt.Errorf("registering %s with the sbx gateway: %w (re-run this pix slack setup command after fixing sbx); %s",
				slackServerName, err, extra)
		}
		return fmt.Errorf("registering %s with the sbx gateway: %w (re-run this pix slack setup command after fixing sbx)",
			slackServerName, err)
	}

	cfg.AddMCP(slackServerName)
	if err := cfg.Save(); err != nil {
		if wasRegistered {
			if extra != "" {
				return fmt.Errorf("saving config: %w (the pre-existing slack registration was left in place); %s", err, extra)
			}
			return fmt.Errorf("saving config: %w (the pre-existing slack registration was left in place)", err)
		}
		if _, rerr := env.run("sbx", "mcp", "rm", slackServerName); rerr != nil {
			if extra != "" {
				return fmt.Errorf("saving config: %w; additionally, rollback of the %s registration failed: %v; fix by hand (sbx mcp rm %s); %s",
					err, slackServerName, rerr, slackServerName, extra)
			}
			return fmt.Errorf("saving config: %w; additionally, rollback of the %s registration failed: %v; fix by hand (sbx mcp rm %s)",
				err, slackServerName, rerr, slackServerName)
		}
		if extra != "" {
			return fmt.Errorf("saving config: %w (the just-created registration was rolled back); %s", err, extra)
		}
		return fmt.Errorf("saving config: %w (the just-created registration was rolled back)", err)
	}
	return nil
}

// slackSetupPKCE is the hermetically-testable core of the no---token-ref
// path. Every side effect (the 1Password document, op-refs.env, the sbx
// registration, config.toml) happens strictly in this order: preflight ->
// browser/callback/exchange/identity check (no side effects at all) ->
// confirm -> 1Password document -> refs -> register -> save. From the
// 1Password write onward, any later failure is reported with the orphan
// document's item/vault id (slackOAuthOrphanNote) and NEVER claims success.
func slackSetupPKCE(env shellEnv, cfg *config.Config, opts slackSetupOpts, deps slackOAuthDeps,
	in io.Reader, out io.Writer, tty bool, hostResolver func() (string, error)) error {

	clientID := strings.TrimSpace(opts.clientID)
	if clientID == "" {
		clientID = strings.TrimSpace(cfg.Slack.ClientID)
	}
	if clientID == "" {
		return fmt.Errorf("no Slack OAuth client id configured; pass --client-id or set config.toml's [slack] client_id (see docs/design/slack-setup.md)")
	}
	redirectURI := strings.TrimSpace(opts.redirectURI)
	if redirectURI == "" {
		redirectURI = strings.TrimSpace(cfg.Slack.RedirectURI)
	}
	if redirectURI == "" {
		redirectURI = config.DefaultSlackOAuthRedirectURI
	}
	port, path, err := parseSlackRedirectURI(redirectURI)
	if err != nil {
		return err
	}

	wasRegistered, err := slackRegistrationPreflight(env)
	if err != nil {
		return err
	}

	// Bind BEFORE printing/opening anything: a busy port must fail fast,
	// before the user is ever sent to Slack's authorize page.
	ln, err := slackBindCallbackListener(port)
	if err != nil {
		return err
	}
	defer ln.Close()

	state, err := randomURLSafe(slackOAuthStateBytes)
	if err != nil {
		return fmt.Errorf("generating OAuth state: %w", err)
	}
	verifier, err := randomURLSafe(slackOAuthVerifierBytes)
	if err != nil {
		return fmt.Errorf("generating PKCE code_verifier: %w", err)
	}
	challenge := slackCodeChallengeS256(verifier)
	authURL := slackBuildAuthorizeURL(clientID, redirectURI, state, challenge)

	fmt.Fprintln(out, "Open this URL to authorize Slack access:")
	fmt.Fprintln(out, authURL)
	openBrowser := deps.openBrowser
	if openBrowser == nil {
		openBrowser = slackOpenBrowser
	}
	if err := openBrowser(authURL); err != nil {
		fmt.Fprintf(out, "(could not launch a browser automatically: %v — open the URL above manually)\n", err)
	}

	timeout := deps.timeout
	if timeout <= 0 {
		timeout = slackOAuthTimeout
	}
	code, err := slackServeCallback(ln, path, state, timeout)
	if err != nil {
		return fmt.Errorf("Slack OAuth callback: %w", err)
	}

	runner := deps.runner
	if runner == nil {
		runner = slackoauth.ExecRunner{}
	}
	doer := deps.doer
	if doer == nil {
		doer = http.DefaultClient
	}
	clock := deps.clock
	if clock == nil {
		clock = slackoauth.SystemClock{}
	}

	client := &slackoauth.Client{
		Doer: doer, Clock: clock, ClientID: clientID,
		RequiredScopes: append([]string(nil), slackOAuthUserScopes...),
	}
	exCtx, exCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer exCancel()
	blob, err := client.Exchange(exCtx, slackoauth.ExchangeParams{Code: code, CodeVerifier: verifier, RedirectURI: redirectURI})
	if err != nil {
		return fmt.Errorf("exchanging the Slack authorization code: %w", err)
	}

	if env.slackAuthTest == nil {
		return fmt.Errorf("internal: shellEnv.slackAuthTest not wired")
	}
	id, err := env.slackAuthTest(blob.AccessToken)
	if err != nil {
		note := slackOAuthBestEffortRevoke(deps, blob.AccessToken)
		return fmt.Errorf("Slack auth.test failed on the newly issued token: %w (the exchange succeeded but the token did not authenticate); %s", err, note)
	}
	if err := slackVerifyRotatingIdentity(id, blob); err != nil {
		note := slackOAuthBestEffortRevoke(deps, blob.AccessToken)
		return fmt.Errorf("%w; %s", err, note)
	}
	fmt.Fprintf(out, "Slack identity: %s (user %s) on team %s (%s)\n", id.user, id.userID, id.team, id.teamID)

	// A DIFFERENT identity than whatever was already pinned (SLACK_TEAM_ID/
	// SLACK_USER_ID) must never be silently re-pinned: under --yes that is a
	// hard refusal (a script that expected to reauthorize the SAME person
	// must not accidentally re-pin to someone else's identity), and
	// interactively it needs its own explicit confirmation, distinct from the
	// ordinary "wire up Slack" prompt below. --allow-identity-change (or
	// --yes with it set) skips this gate entirely.
	_, existingRefsContent, _ := opRefsContent(env)
	pinTeam, pinUser := slackIdentityPins(existingRefsContent)
	identityChanged := (pinTeam != "" || pinUser != "") && (pinTeam != id.teamID || pinUser != id.userID)
	if identityChanged && !opts.allowIdentityChange {
		if opts.assumeYes {
			note := slackOAuthBestEffortRevoke(deps, blob.AccessToken)
			return fmt.Errorf("the existing identity pin (team %s / user %s) does not match this OAuth identity (team %s / user %s); "+
				"refusing under --yes to avoid silently re-pinning to a different person — rerun interactively to confirm, or pass --allow-identity-change; %s",
				pinTeam, pinUser, id.teamID, id.userID, note)
		}
		if !tty {
			note := slackOAuthBestEffortRevoke(deps, blob.AccessToken)
			return fmt.Errorf("refusing to wire up Slack non-interactively without --yes; %s", note)
		}
		if !confirmYN(in, out, fmt.Sprintf("The pinned Slack identity differs (was %s / %s; this OAuth grant resolves to %s / %s). Replace the pin? [y/N]: ",
			pinTeam, pinUser, id.teamID, id.userID), false) {
			note := slackOAuthBestEffortRevoke(deps, blob.AccessToken)
			fmt.Fprintf(out, "aborted; %s\n", note)
			return nil
		}
	}

	if !opts.assumeYes {
		if !tty {
			note := slackOAuthBestEffortRevoke(deps, blob.AccessToken)
			return fmt.Errorf("refusing to wire up Slack non-interactively without --yes; %s", note)
		}
		if !confirmYN(in, out, fmt.Sprintf("Wire up Slack as %s on %s via OAuth? [y/N]: ", id.user, id.team), false) {
			note := slackOAuthBestEffortRevoke(deps, blob.AccessToken)
			fmt.Fprintf(out, "aborted; %s\n", note)
			return nil
		}
	}

	vault := opts.vault
	if !opts.vaultExplicit && strings.TrimSpace(cfg.Slack.OAuthVaultID) != "" {
		vault = cfg.Slack.OAuthVaultID
	}
	item := strings.TrimSpace(cfg.Slack.OAuthDocumentID)
	title := fmt.Sprintf("Pix Slack OAuth - %s", id.user)
	store := slackoauth.NewOPStore(runner, vault, title, item)
	opCtx, opCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer opCancel()
	if err := store.Write(opCtx, blob); err != nil {
		note := slackOAuthBestEffortRevoke(deps, blob.AccessToken)
		return fmt.Errorf("writing the Slack credential to 1Password: %w; %s", err, note)
	}
	itemID, vaultID := store.ItemID(), store.VaultID()
	orphan := slackOAuthOrphanNote(itemID, vaultID)

	if err := runSecretSet(env, out, "SLACK_TEAM_ID", blob.TeamID); err != nil {
		return fmt.Errorf("writing SLACK_TEAM_ID: %w; %s", err, orphan)
	}
	if err := runSecretSet(env, out, "SLACK_USER_ID", blob.UserID); err != nil {
		return fmt.Errorf("writing SLACK_USER_ID: %w; %s", err, orphan)
	}
	// The static-token flow's SLACK_TOKEN ref is meaningless once the rotating
	// credential lives in 1Password under Slack.OAuthVaultID/OAuthDocumentID —
	// remove it so a stale ref never shadows the OAuth path. A no-op when
	// absent (runSecretRm is idempotent).
	if err := runSecretRm(env, out, "SLACK_TOKEN"); err != nil {
		return fmt.Errorf("removing the legacy SLACK_TOKEN ref: %w; %s", err, orphan)
	}

	cfg.SetSlackClientID(clientID)
	cfg.SetSlackRedirectURI(redirectURI)
	cfg.SetSlackOAuthVaultID(vaultID)
	cfg.SetSlackOAuthDocumentID(itemID)
	cfg.SetSlackOAuthGrantExpiresAt(blob.GrantExpiresAt)

	if err := slackRegisterAndSave(cfg, env, out, hostResolver, wasRegistered, orphan); err != nil {
		return err
	}

	fmt.Fprintf(out, "Slack OAuth is set up. This rotating grant expires %s (~monthly) — re-run "+
		"`pix slack setup` before then to reauthorize.\n", blob.GrantExpiresAt.Format("2006-01-02"))
	slackPrintAttachmentNote(out)
	return nil
}

// ---------------------------------------------------------------------------
// runtime (status/disable): the SAME kind of manager/source the running MCP
// server (services/host/slack.go's slackOAuthManagerFromConfig) uses to
// read/refresh the 1Password document. That function lives in a DIFFERENT
// binary (pix-host, not pix) so it can't be imported directly; the three-
// field completeness gate and the Manager/OPStore/Client wiring below are
// deliberately kept in lockstep with it rather than derived from it.
// ---------------------------------------------------------------------------

// slackOAuthRuntimeTimeout bounds every call `pix slack status`/`disable`
// makes through the OAuth runtime (a Manager.Token, which may exec `op
// document get/edit`, or hit Slack's oauth.v2.access/auth.revoke endpoints),
// so a hung 1Password CLI or network call can never wedge the CLI forever.
// Matches oauthSlackTokenSourceTimeout's rationale in services/host/slack.go.
const slackOAuthRuntimeTimeout = 30 * time.Second

// slackOAuthGrantExpiryWarning is how much remaining grant lifetime turns the
// status countdown from a plain fact into a call-it-out warning: seven days
// is enough runway to act (re-run pix slack setup) before the 30-day rotating
// grant actually expires and OAuth-mode Slack access goes dark.
const slackOAuthGrantExpiryWarning = 7 * 24 * time.Hour

// slackOAuthConfigComplete reports whether cfg carries a COMPLETE [slack]
// OAuth wiring: a client id AND both 1Password locators. This is the same
// three-field gate slackOAuthManagerFromConfig uses in services/host/slack.go
// (a different binary, so it can't be called directly) to decide whether the
// running MCP server itself uses OAuth or falls back to static SLACK_TOKEN —
// status/disable must agree with the server about which mode is active, or a
// status/disable run could report on (or tear down) the wrong credential.
func slackOAuthConfigComplete(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	return strings.TrimSpace(cfg.Slack.ClientID) != "" &&
		strings.TrimSpace(cfg.Slack.OAuthVaultID) != "" &&
		strings.TrimSpace(cfg.Slack.OAuthDocumentID) != ""
}

// slackOAuthRuntimeDeps bundles the seams `pix slack status`/`disable` use to
// read/refresh/revoke the OAuth-mode credential, mirroring slackOAuthDeps's
// injection discipline for the setup flow: production uses the real `op` CLI
// and real HTTP; tests inject fakes so nothing ever shells out or hits the
// network.
type slackOAuthRuntimeDeps struct {
	// runner is the slackoauth.CommandRunner used to read/edit/delete the
	// 1Password document. Production: slackoauth.ExecRunner{}.
	runner slackoauth.CommandRunner
	// clock supplies "now" for the Manager's freshness/expiry comparisons.
	clock slackoauth.Clock
	// revoke calls Slack's auth.revoke with a live bearer token and reports
	// whether Slack CONFIRMED revocation — never just that the HTTP call
	// succeeded. Production: liveSlackAuthRevoke. Never logs the token.
	revoke func(token string) (revoked bool, err error)
}

// defaultSlackOAuthRuntimeDeps returns the production wiring.
func defaultSlackOAuthRuntimeDeps() slackOAuthRuntimeDeps {
	return slackOAuthRuntimeDeps{
		runner: slackoauth.ExecRunner{},
		clock:  slackoauth.SystemClock{},
		revoke: liveSlackAuthRevoke,
	}
}

// slackOAuthRuntimeDepsFn is the seam slackStatus/slackDisable use to build
// their OAuth runtime dependencies. Production leaves it at
// defaultSlackOAuthRuntimeDeps; a test overrides it for the duration of one
// call (there is nothing process-lifetime cached here, unlike the running
// MCP server's slackGetTokenSource memoization) to inject a fake
// CommandRunner/clock/revoke without ever shelling out or hitting the
// network. Tests MUST restore it via t.Cleanup so a fake can never leak into
// an unrelated test.
var slackOAuthRuntimeDepsFn = defaultSlackOAuthRuntimeDeps

// slackOAuthRuntime builds the SAME kind of slackoauth.Manager/OPStore
// services/host/slack.go's slackOAuthManagerFromConfig builds for the
// running MCP server: an OPStore over cfg's vault/document (item pre-filled,
// so a Read never needs a Title), a Client carrying the exact shared
// read-only scope set (slackoauth.RequiredUserScopes), and a FileLock at the
// SAME path the server uses, so a refresh triggered from this CLI is
// serialized with a concurrently running gateway process sharing the same
// credential, not just with itself. ok is false when cfg does not carry a
// complete OAuth wiring, OR when the shared state dir cannot be resolved —
// this FAILS CLOSED rather than fall back to a process-local Locker: an
// in-process-only lock would not actually serialize anything against a
// concurrently running gateway process sharing the same 1Password document
// (the entire reason this Locker exists), so pretending it were safe would
// be a silent refresh race, strictly worse than refusing to proceed. In
// production config.StateDir only fails when $HOME cannot be determined at
// all, so this should never actually trigger outside a broken environment.
func slackOAuthRuntime(cfg *config.Config, env shellEnv, deps slackOAuthRuntimeDeps) (*slackoauth.Manager, *slackoauth.OPStore, bool) {
	if !slackOAuthConfigComplete(cfg) {
		return nil, nil, false
	}
	if env.stateDir == nil {
		return nil, nil, false
	}
	sd, err := env.stateDir()
	if err != nil || strings.TrimSpace(sd) == "" {
		return nil, nil, false
	}
	runner := deps.runner
	if runner == nil {
		runner = slackoauth.ExecRunner{}
	}
	store := slackoauth.NewOPStore(runner, cfg.Slack.OAuthVaultID, "", cfg.Slack.OAuthDocumentID)
	client := &slackoauth.Client{
		ClientID:       cfg.Slack.ClientID,
		RequiredScopes: append([]string(nil), slackoauth.RequiredUserScopes...),
	}
	clock := deps.clock
	if clock == nil {
		clock = slackoauth.SystemClock{}
	}
	locker := &slackoauth.FileLock{Path: filepath.Join(sd, "slack-oauth.lock")}
	return &slackoauth.Manager{Store: store, Client: client, Locker: locker, Clock: clock}, store, true
}

// slackOAuthGrantExpiryCheck reports the cached rotating-grant countdown
// (cfg.Slack.OAuthGrantExpiresAt, mirrored from slackoauth.Blob.GrantExpiresAt
// at the last setup/refresh) WITHOUT a live 1Password round trip — the cache
// is advisory (SlackOAuth's doc comment), and a countdown display is exactly
// what it exists for. An already-expired grant is a VERIFIED todo (blocks
// exit 1): no refresh can revive a dead grant, only a full re-authorization
// (pix slack setup). Seven days or fewer remaining is a non-blocking warning
// — still reported ready, called out so it is not missed until it actually
// breaks.
func slackOAuthGrantExpiryCheck(cfg *config.Config, now time.Time) check {
	exp := cfg.Slack.OAuthGrantExpiresAt
	if exp.IsZero() {
		return check{label: "grant expiry", verdict: verdictUnverifiable,
			detail: "no cached grant expiry recorded (set by pix slack setup)"}
	}
	remaining := exp.Sub(now)
	if remaining <= 0 {
		agoDays := int((-remaining).Hours() / 24)
		return check{label: "grant expiry", verdict: verdictTodo,
			detail: fmt.Sprintf("OAuth grant expired %s ago", plural(agoDays, "day")),
			todo:   "pix slack setup"}
	}
	days := int(remaining.Hours() / 24)
	if remaining <= slackOAuthGrantExpiryWarning {
		return check{label: "grant expiry", verdict: verdictReady,
			detail: fmt.Sprintf("\u26a0 expires in %s — renew soon", plural(days, "day")),
			todo:   "pix slack setup (reauthorize before it expires)"}
	}
	return check{label: "grant expiry", verdict: verdictReady,
		detail: fmt.Sprintf("expires in %s", plural(days, "day"))}
}

// slackOAuthAccessChecks obtains a live access token through mgr (reading,
// and refreshing if needed, the 1Password document — the SAME operation the
// running MCP server performs on every call), calls auth.test on it, and
// compares the resulting identity against the SLACK_TEAM_ID/SLACK_USER_ID
// pin exactly like the static status path does. A failure to obtain a token
// is reported as a verified todo ONLY when the grant itself has expired
// (slackoauth.ErrGrantExpired — nothing else can be done from here but
// re-authorize); any other read/refresh failure is unverifiable, since it
// may be a transient 1Password/network problem rather than a proven gap.
func slackOAuthAccessChecks(env shellEnv, mgr *slackoauth.Manager, pinTeam, pinUser string) []check {
	var checks []check
	ctx, cancel := context.WithTimeout(context.Background(), slackOAuthRuntimeTimeout)
	defer cancel()
	tok, err := mgr.Token(ctx)
	if err != nil {
		v := verdictUnverifiable
		todo := ""
		if errors.Is(err, slackoauth.ErrGrantExpired) {
			v, todo = verdictTodo, "pix slack setup"
		}
		checks = append(checks, check{label: "access", verdict: v,
			detail: "could not obtain a valid OAuth access token: " + err.Error(), todo: todo})
		checks = append(checks, check{label: "identity", verdict: verdictUnverifiable,
			detail: "cannot verify live identity (no access token available)"})
		return checks
	}
	checks = append(checks, check{label: "access", verdict: verdictReady,
		detail: "obtained a valid OAuth access token from 1Password"})

	if env.slackAuthTest == nil {
		checks = append(checks, check{label: "identity", verdict: verdictUnverifiable,
			detail: "cannot verify live identity (no auth.test probe wired here)"})
		return checks
	}
	id, aerr := env.slackAuthTest(tok)
	if aerr != nil {
		checks = append(checks, check{label: "identity", verdict: verdictTodo,
			detail: "auth.test failed: " + aerr.Error(), todo: "pix slack setup"})
		return checks
	}
	checks = append(checks, check{label: "identity", verdict: verdictReady,
		detail: fmt.Sprintf("%s (user %s) on %s (%s)", id.user, id.userID, id.team, id.teamID)})

	switch {
	case pinTeam == "" && pinUser == "":
		checks = append(checks, check{label: "identity pin", note: true, verdict: verdictUnverifiable,
			detail: "no SLACK_TEAM_ID/SLACK_USER_ID pin recorded yet (set by pix slack setup)"})
	case pinTeam == id.teamID && pinUser == id.userID:
		checks = append(checks, check{label: "identity pin", verdict: verdictReady,
			detail: "matches the identity pinned at setup"})
	default:
		checks = append(checks, check{label: "identity pin", verdict: verdictTodo,
			detail: fmt.Sprintf("live identity (team %s / user %s) does not match the pin (team %s / user %s) — "+
				"the token was likely swapped; re-run pix slack setup if this is expected",
				id.teamID, id.userID, pinTeam, pinUser),
			todo: "pix slack setup"})
	}
	return checks
}

// slackOAuthStatusChecks is `pix slack status`'s OAuth-mode surface: it
// reports the mode itself, the non-secret 1Password vault/document ids, the
// cached grant-expiry countdown, and then (through the same kind of runtime
// manager the MCP server uses) live access + identity + identity-pin checks.
// It never reads or reports SLACK_TOKEN — that variable plays no role in
// this mode.
func slackOAuthStatusChecks(cfg *config.Config, env shellEnv, now time.Time) []check {
	var checks []check
	checks = append(checks, check{label: "mode", verdict: verdictReady,
		detail: "OAuth (rotating PKCE credential in 1Password)"})
	checks = append(checks, check{label: "1Password doc", verdict: verdictReady,
		detail: fmt.Sprintf("vault %s, document %s", cfg.Slack.OAuthVaultID, cfg.Slack.OAuthDocumentID)})
	checks = append(checks, slackOAuthGrantExpiryCheck(cfg, now))

	mgr, _, ok := slackOAuthRuntime(cfg, env, slackOAuthRuntimeDepsFn())
	if !ok {
		checks = append(checks, check{label: "access", verdict: verdictUnverifiable,
			detail: "could not build the OAuth runtime source (the shared 1Password-credential lock's state directory could not be resolved)"})
		return checks
	}
	_, content, _ := opRefsContent(env)
	pinTeam, pinUser := slackIdentityPins(content)
	checks = append(checks, slackOAuthAccessChecks(env, mgr, pinTeam, pinUser)...)
	return checks
}

// slackDisableOAuth is `pix slack disable`'s OAuth-mode path. Unlike the
// static path (which only ever removes local wiring, since it never held a
// revocable client credential of its own), OAuth mode DOES call Slack's own
// auth.revoke before touching anything local — pix minted this credential
// via its own PKCE client, so it both can and must kill it at the source.
// The order is fixed and each step gates the next:
//
//  1. preflight the registration exactly like the static path (never touch a
//     foreign, non-canonical registration)
//  2. obtain a live bearer through the runtime manager and revoke it at
//     Slack; a failure here (including a response that doesn't CONFIRM
//     revocation) stops immediately with NOTHING removed — UNLESS the
//     failure itself proves the credential is already dead (an expired
//     30-day grant, a dead refresh chain, or Slack's own invalid_auth/
//     token_revoked/invalid_refresh_token), in which case there is simply
//     nothing left to revoke and cleanup continues (this is what makes a
//     manually- or previously-revoked credential's disable retryable rather
//     than permanently stuck on step 2); any other error (a network/op
//     failure) still aborts here
//  3. only once revoke is confirmed (or proven unnecessary), archive the 1Password document
//     (op document delete --archive; item/vault ids only, no secret ever on
//     argv); a failure here is reported as revoked-but-local-cleanup-
//     incomplete — everything from here on is left in place, and re-running
//     disable (which will simply fail to find a live token to revoke again)
//     is how the cleanup finishes, never a repeat live revoke
//  4. only once archived, remove the sbx registration, clear the OAuth
//     vault/document/expiry from config (keeping client_id/redirect_uri so a
//     later setup is one step), and remove the identity pin refs
func slackDisableOAuth(cfg *config.Config, env shellEnv, out io.Writer, deps slackOAuthRuntimeDeps) error {
	registered, err := slackRegistrationPresence(env)
	if err != nil {
		return err
	}
	if registered {
		argv, ok := registeredMCPCommand(env, slackServerName)
		if _, trusted := recognizedMCPArgv(env, argv, slackServerName); !ok || !trusted {
			return fmt.Errorf("a server named slack is registered, but it is not the canonical Pix host command; refusing to remove it (inspect: sbx mcp get slack)")
		}
	}

	mgr, store, ok := slackOAuthRuntime(cfg, env, deps)
	if !ok {
		return fmt.Errorf("internal: OAuth config reported complete but the runtime source could not be built " +
			"(the shared 1Password-credential lock's state directory could not be resolved); nothing was removed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), slackOAuthRuntimeTimeout)
	defer cancel()
	liveRevokePerformed := false
	tok, err := mgr.Token(ctx)
	if err != nil {
		if !slackoauth.IsDeadCredential(err) {
			return fmt.Errorf("could not obtain a valid OAuth access token to revoke: %w; nothing was removed", err)
		}
		// The credential is already provably dead (an expired grant, or a dead
		// refresh chain Slack itself rejected as invalid_refresh_token/
		// token_revoked) — there is nothing live left to revoke. This is what
		// makes disable retryable after a manual/prior revocation rather than
		// permanently stuck trying to obtain a token that will never come back.
		fmt.Fprintf(out, "  the OAuth credential was already invalid (%v); skipping the live revoke\n", err)
	} else {
		revoke := deps.revoke
		if revoke == nil {
			revoke = liveSlackAuthRevoke
		}
		revoked, rerr := revoke(tok)
		switch {
		case rerr != nil && slackoauth.IsDeadCredential(rerr):
			// Slack itself says the token is already dead (invalid_auth or
			// token_revoked — e.g. someone revoked it by hand, or a prior
			// disable already revoked it but failed before archiving). Nothing
			// left to revoke; continue exactly as above.
			fmt.Fprintf(out, "  Slack reports the token was already invalid/revoked (%v); skipping the live revoke\n", rerr)
		case rerr != nil:
			return fmt.Errorf("Slack auth.revoke failed: %w; nothing was removed (the credential in 1Password and the sbx registration are untouched)", rerr)
		case !revoked:
			return fmt.Errorf("Slack auth.revoke failed: Slack did not confirm the token was revoked; nothing was removed (the credential in 1Password and the sbx registration are untouched)")
		default:
			fmt.Fprintln(out, "  revoked the Slack OAuth token")
			liveRevokePerformed = true
		}
	}

	if derr := store.Delete(ctx); derr != nil {
		return fmt.Errorf("the Slack token was revoked, but deleting the 1Password document failed: %w; "+
			"local cleanup is incomplete (registration, config, refs left in place) — re-run pix slack disable to retry", derr)
	}
	fmt.Fprintln(out, "  archived the 1Password document")

	if registered {
		if env.run == nil {
			return fmt.Errorf("internal: shellEnv.run not wired")
		}
		if _, err := env.run("sbx", "mcp", "rm", slackServerName); err != nil {
			return fmt.Errorf("removing the %s registration: %w (the token was revoked and the 1Password document archived; remove the registration by hand: sbx mcp rm %s)",
				slackServerName, err, slackServerName)
		}
		fmt.Fprintln(out, "  removed registration: "+slackServerName)
	}

	cfg.RemoveMCP(slackServerName)
	cfg.ClearSlackOAuthManaged()
	if err := cfg.Save(); err != nil {
		return fmt.Errorf("saving config: %w (the token was revoked and the 1Password document was already archived; the sbx registration was already removed)", err)
	}
	fmt.Fprintln(out, "  cleared config: mcp "+slackServerName+", OAuth vault/document (client_id and redirect_uri kept for re-setup)")

	for _, key := range []string{"SLACK_TEAM_ID", "SLACK_USER_ID", "SLACK_TOKEN"} {
		if err := runSecretRm(env, out, key); err != nil {
			return fmt.Errorf("removing %s: %w", key, err)
		}
	}

	if liveRevokePerformed {
		fmt.Fprintln(out, "Slack OAuth is off here. The token was revoked at Slack and the 1Password")
	} else {
		fmt.Fprintln(out, "Slack OAuth is off here. The credential was already invalid/revoked, and the 1Password")
	}
	fmt.Fprintln(out, "document holding it was archived. Re-run pix slack setup (no --token-ref) to")
	fmt.Fprintln(out, "reauthorize — your client_id and redirect_uri are still configured.")
	return nil
}
