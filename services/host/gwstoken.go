// Google Workspace token service. Ported from the former Node gws-token service.
//
// Keeps long-lived Google OAuth refresh credentials on the host: the sandbox
// `gws` wrapper GETs /token for a short-lived bearer and passes it to the real
// `gws`. The refresh credential never enters the VM.
//
// This is one of the two host services that spawn a child process (`gws auth
// export`); doing it in a compiled Go binary (not a Node interpreter) is the
// reason it lives here — same rationale as the overlay exec proxies.
//
// Env:
//   GWS_TOKEN_PORT  default 11441
//   GWS_TOKEN_BIND  default 127.0.0.1
//   GWS_TOKEN_AUTH  optional shared secret (Authorization: Bearer <secret>)

package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const gwsTokenURL = "https://oauth2.googleapis.com/token"

type gwsCreds struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
}

type gwsBearer struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int    `json:"expires_in"`
	TokenType   string `json:"token_type"`
}

type gwsTokenSvc struct {
	mu        sync.Mutex
	cached    string
	expiresAt time.Time
}

// exportCreds shells out to the host `gws` to read the long-lived OAuth creds.
func (s *gwsTokenSvc) exportCreds() (*gwsCreds, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gws", "auth", "export", "--unmasked").Output()
	if err != nil {
		return nil, errors.New("gws auth export failed: " + err.Error())
	}
	start := strings.IndexByte(string(out), '{')
	if start < 0 {
		return nil, errors.New("gws auth export returned no JSON")
	}
	var c gwsCreds
	if err := json.Unmarshal(out[start:], &c); err != nil {
		return nil, errors.New("gws auth export: bad JSON: " + err.Error())
	}
	if c.ClientID == "" || c.ClientSecret == "" || c.RefreshToken == "" {
		return nil, errors.New("gws auth export missing client_id, client_secret, or refresh_token")
	}
	if strings.Contains(c.ClientSecret, "...") {
		return nil, errors.New("gws auth export returned redacted credentials; expected --unmasked output")
	}
	return &c, nil
}

func (s *gwsTokenSvc) mint() (*gwsBearer, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	if s.cached != "" && now.Before(s.expiresAt) {
		return &gwsBearer{AccessToken: s.cached, ExpiresIn: int(time.Until(s.expiresAt).Seconds()), TokenType: "Bearer"}, nil
	}

	creds, err := s.exportCreds()
	if err != nil {
		return nil, err
	}
	form := url.Values{
		"client_id":     {creds.ClientID},
		"client_secret": {creds.ClientSecret},
		"refresh_token": {creds.RefreshToken},
		"grant_type":    {"refresh_token"},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reqHTTP, _ := http.NewRequestWithContext(ctx, http.MethodPost, gwsTokenURL, strings.NewReader(form.Encode()))
	reqHTTP.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := http.DefaultClient.Do(reqHTTP)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	var parsed gwsBearer
	if err := json.NewDecoder(res.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK || parsed.AccessToken == "" {
		return nil, errors.New("oauth refresh failed: " + res.Status)
	}

	ttl := parsed.ExpiresIn
	if ttl <= 0 {
		ttl = 3600
	}
	safety := ttl / 2
	if safety > 300 {
		safety = 300
	}
	if safety < 1 {
		safety = 1
	}
	s.cached = parsed.AccessToken
	s.expiresAt = time.Now().Add(time.Duration(ttl-safety) * time.Second)
	return &gwsBearer{AccessToken: s.cached, ExpiresIn: ttl, TokenType: "Bearer"}, nil
}

// gwsTokenMux builds the /token mux. auth is the required shared bearer (empty
// disables the check). It is passed EXPLICITLY rather than read from a
// process-global env var so `serve` can hand it only to this handler and never
// leak it into unrelated plugin subprocesses (see serve.go / F2).
func gwsTokenMux(auth string) *http.ServeMux {
	svc := &gwsTokenSvc{}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "not_found"})
			return
		}
		if auth != "" && r.Header.Get("Authorization") != "Bearer "+auth {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		bearer, err := svc.mint()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "token_error", "message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, bearer)
	})
	return mux
}

// gwsTokenCheck is the serve preflight (see serve.go): confirm the host `gws` is
// authenticated so the token service can actually mint bearers. Without it the
// service binds its port but every /token call fails ("no host token") and the
// sandbox's gws (Gmail/Calendar) is silently dark.
func gwsTokenCheck() error {
	if _, err := (&gwsTokenSvc{}).exportCreds(); err != nil {
		return errors.New(err.Error() + " (run `gws auth login` on the host)")
	}
	return nil
}

func runGwsToken() {
	addr := env("GWS_TOKEN_BIND", "127.0.0.1") + ":" + env("GWS_TOKEN_PORT", "11441")
	log.Printf("gws token service on http://%s/token", addr)
	if err := http.ListenAndServe(addr, gwsTokenMux(env("GWS_TOKEN_AUTH", ""))); err != nil {
		log.Fatal(err)
	}
}
