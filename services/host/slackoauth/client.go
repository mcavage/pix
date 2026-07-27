package slackoauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://slack.com/api"
	// maxResponseBytes caps how much of an oauth.v2.access response body we
	// will ever read, so a misbehaving or hostile endpoint can't force
	// unbounded memory use. 1 read past the cap is enough to detect
	// oversize without buffering an unbounded body first.
	maxResponseBytes = 1 << 20

	requiredTokenType   = "user"
	requiredTokenPrefix = "xoxe.xoxp-"
)

// GrantWindow is the fixed lifetime Slack grants a rotating refresh token
// from the moment of the initial authorization-code exchange. It is stamped
// onto the Blob at Exchange time; Manager enforces it and Refresh never
// extends it.
const GrantWindow = 30 * 24 * time.Hour

// HTTPDoer is the minimal HTTP dependency the Client needs, so tests can
// inject a fake instead of making a real network call. *http.Client
// satisfies it.
type HTTPDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Clock abstracts time.Now so tests can control "now" deterministically.
type Clock interface {
	Now() time.Time
}

// SystemClock is the production Clock: the real wall clock.
type SystemClock struct{}

// Now returns time.Now().
func (SystemClock) Now() time.Time { return time.Now() }

// Client exchanges an authorization code (PKCE, no client secret) for a
// rotating Slack token pair, and refreshes an existing pair (again with no
// client secret and no PKCE verifier). Both flows hit the same
// oauth.v2.access endpoint and select ONLY the authed_user credentials from
// the response — Slack's bot token (if any) at the top level is never used.
type Client struct {
	// Doer performs the HTTP request. Defaults to http.DefaultClient.
	Doer HTTPDoer
	// Clock supplies "now" for computing AccessExpiresAt/GrantExpiresAt.
	// Defaults to SystemClock.
	Clock Clock
	// BaseURL overrides the Slack API base for tests. Defaults to
	// https://slack.com/api.
	BaseURL string
	// ClientID is the Slack app's public client id, required by both flows.
	ClientID string
	// RequiredScopes is the exact scope set the authed_user grant must
	// carry — not a superset, not a subset. A mismatch (missing OR extra
	// scopes) is refused.
	RequiredScopes []string
}

func (c *Client) doer() HTTPDoer {
	if c.Doer != nil {
		return c.Doer
	}
	return http.DefaultClient
}

func (c *Client) clock() Clock {
	if c.Clock != nil {
		return c.Clock
	}
	return SystemClock{}
}

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return defaultBaseURL
}

// ExchangeParams are the caller-supplied halves of a PKCE authorization-code
// exchange; ClientID comes from the Client itself.
type ExchangeParams struct {
	Code         string
	CodeVerifier string
	RedirectURI  string
}

// Exchange trades an authorization code for a rotating token pair. The
// request carries exactly client_id, code, code_verifier, redirect_uri, and
// grant_type=authorization_code — never a client_secret, since this is a
// public PKCE client. On success, GrantExpiresAt is stamped to now +
// GrantWindow (30 days).
func (c *Client) Exchange(ctx context.Context, p ExchangeParams) (Blob, error) {
	if strings.TrimSpace(c.ClientID) == "" {
		return Blob{}, errors.New("slackoauth: Client.ClientID is required")
	}
	if strings.TrimSpace(p.Code) == "" {
		return Blob{}, errors.New("slackoauth: authorization code is required")
	}
	if strings.TrimSpace(p.CodeVerifier) == "" {
		return Blob{}, errors.New("slackoauth: PKCE code_verifier is required")
	}
	if strings.TrimSpace(p.RedirectURI) == "" {
		return Blob{}, errors.New("slackoauth: redirect_uri is required")
	}
	form := url.Values{
		"client_id":     {c.ClientID},
		"code":          {p.Code},
		"code_verifier": {p.CodeVerifier},
		"redirect_uri":  {p.RedirectURI},
		"grant_type":    {"authorization_code"},
	}
	b, err := c.call(ctx, form)
	if err != nil {
		return Blob{}, err
	}
	b.GrantExpiresAt = c.clock().Now().Add(GrantWindow)
	return b, nil
}

// Refresh trades a rotating refresh token for a new token pair. The request
// carries exactly client_id, refresh_token, and grant_type=refresh_token —
// never a code_verifier and never a client_secret. The returned Blob's
// GrantExpiresAt is left zero: the 30-day grant window is fixed at the
// original Exchange and is the Manager's responsibility to carry forward,
// not something Refresh can compute on its own.
func (c *Client) Refresh(ctx context.Context, refreshToken string) (Blob, error) {
	if strings.TrimSpace(c.ClientID) == "" {
		return Blob{}, errors.New("slackoauth: Client.ClientID is required")
	}
	if strings.TrimSpace(refreshToken) == "" {
		return Blob{}, errors.New("slackoauth: refresh token is required")
	}
	form := url.Values{
		"client_id":     {c.ClientID},
		"refresh_token": {refreshToken},
		"grant_type":    {"refresh_token"},
	}
	return c.call(ctx, form)
}

// oauthResponse is the slice of Slack's oauth.v2.access response we care
// about. authed_user is the ONLY credential source we trust; any top-level
// access_token (a bot token) is deliberately never decoded here.
type oauthResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error"`
	Team  *struct {
		ID string `json:"id"`
	} `json:"team"`
	AuthedUser *struct {
		ID           string `json:"id"`
		Scope        string `json:"scope"`
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	} `json:"authed_user"`
}

// call performs one oauth.v2.access POST with the given form and turns the
// response into a Blob, enforcing every safety property this package exists
// to enforce: 1MB response cap, context-friendly request, authed_user-only
// credential selection, personal user token type/prefix, and an exact
// required-scope match. It never returns a Blob with GrantExpiresAt set —
// that is Exchange's job alone.
func (c *Client) call(ctx context.Context, form url.Values) (Blob, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL()+"/oauth.v2.access", strings.NewReader(form.Encode()))
	if err != nil {
		return Blob{}, fmt.Errorf("slackoauth: build oauth request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.doer().Do(req)
	if err != nil {
		return Blob{}, fmt.Errorf("slackoauth: oauth.v2.access request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return Blob{}, fmt.Errorf("slackoauth: read oauth response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return Blob{}, errors.New("slackoauth: oauth.v2.access response exceeded the 1MB limit")
	}
	if resp.StatusCode != http.StatusOK {
		return Blob{}, fmt.Errorf("slackoauth: oauth.v2.access returned HTTP %d", resp.StatusCode)
	}

	var raw oauthResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return Blob{}, fmt.Errorf("slackoauth: decode oauth response: %w", err)
	}
	if !raw.OK {
		e := raw.Error
		if e == "" {
			e = "unknown_error"
		}
		return Blob{}, fmt.Errorf("slackoauth: oauth.v2.access failed: %s", e)
	}
	if raw.AuthedUser == nil {
		return Blob{}, errors.New("slackoauth: response has no authed_user credentials")
	}
	au := raw.AuthedUser

	if au.TokenType != requiredTokenType {
		return Blob{}, errors.New("slackoauth: authed_user token_type is not a personal user token")
	}
	if au.AccessToken == "" || !strings.HasPrefix(au.AccessToken, requiredTokenPrefix) {
		return Blob{}, fmt.Errorf("slackoauth: authed_user access_token is not a personal user token (missing %s prefix)", requiredTokenPrefix)
	}
	if au.RefreshToken == "" {
		return Blob{}, errors.New("slackoauth: authed_user credentials are incomplete (no refresh_token)")
	}
	gotScopes := splitScopes(au.Scope)
	if !scopeSetEqual(gotScopes, c.RequiredScopes) {
		return Blob{}, errors.New("slackoauth: authed_user scope set does not exactly match the required scopes")
	}

	teamID := ""
	if raw.Team != nil {
		teamID = raw.Team.ID
	}
	if teamID == "" || au.ID == "" {
		return Blob{}, errors.New("slackoauth: response is missing team_id or user_id")
	}

	now := c.clock().Now()
	return Blob{
		Version:         BlobVersion,
		AccessToken:     au.AccessToken,
		RefreshToken:    au.RefreshToken,
		AccessExpiresAt: now.Add(time.Duration(au.ExpiresIn) * time.Second),
		TeamID:          teamID,
		UserID:          au.ID,
		Scopes:          gotScopes,
	}, nil
}

// splitScopes turns Slack's comma-separated scope string into a sorted,
// blank-filtered slice for deterministic storage and comparison.
func splitScopes(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// scopeSetEqual compares two scope lists as SETS (order and duplicates do
// not matter): got must contain exactly the scopes in want, no more, no
// fewer.
func scopeSetEqual(got, want []string) bool {
	toSet := func(s []string) map[string]bool {
		m := make(map[string]bool, len(s))
		for _, v := range s {
			m[v] = true
		}
		return m
	}
	g, w := toSet(got), toSet(want)
	if len(g) != len(w) {
		return false
	}
	for k := range g {
		if !w[k] {
			return false
		}
	}
	return true
}
