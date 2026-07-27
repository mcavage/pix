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
// scope table) — kept in ONE place so the authorize URL and the post-exchange
// RequiredScopes check can never drift out of sync with each other.
var slackOAuthUserScopes = []string{
	"search:read",
	"channels:read", "channels:history",
	"groups:read", "groups:history",
	"im:read", "im:history",
	"mpim:read", "mpim:history",
	"users:read", "users:read.email",
}

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
}

// defaultSlackOAuthDeps returns the production wiring: a real (non-blocking)
// browser launch, the real `op` CLI via ExecRunner, and the real HTTP client.
func defaultSlackOAuthDeps() slackOAuthDeps {
	return slackOAuthDeps{
		openBrowser: slackOpenBrowser,
		runner:      slackoauth.ExecRunner{},
		doer:        http.DefaultClient,
		clock:       slackoauth.SystemClock{},
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
// answered and discarded WITHOUT ending the wait, except an Origin header or
// a state mismatch — both end the attempt immediately as a security event
// (rather than let a client keep guessing). The response is always static:
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
			// Origin header; treat one as a CSRF-shaped probe and end the
			// attempt rather than keep waiting.
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, "forbidden")
			deliver("", errors.New("rejected an OAuth callback that carried an Origin header"))
			return
		}
		q := r.URL.Query()
		gotState := q.Get("state")
		if subtle.ConstantTimeCompare([]byte(gotState), []byte(wantState)) != 1 {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "state mismatch")
			deliver("", errors.New("OAuth callback state did not match the expected value; discarding (possible CSRF or a stale attempt)"))
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
		return fmt.Errorf("Slack auth.test failed on the newly issued token: %w (the exchange succeeded but the token did not authenticate)", err)
	}
	if err := slackVerifyRotatingIdentity(id, blob); err != nil {
		return err
	}
	fmt.Fprintf(out, "Slack identity: %s (user %s) on team %s (%s)\n", id.user, id.userID, id.team, id.teamID)

	if !opts.assumeYes {
		if !tty {
			return fmt.Errorf("refusing to wire up Slack non-interactively without --yes")
		}
		if !confirmYN(in, out, fmt.Sprintf("Wire up Slack as %s on %s via OAuth? [y/N]: ", id.user, id.team), false) {
			fmt.Fprintln(out, "aborted; nothing written")
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
		return fmt.Errorf("writing the Slack credential to 1Password: %w", err)
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
