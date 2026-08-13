package uat

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"pix/host/config"
)

type OAuthProvider string

const (
	ProviderGitHub    OAuthProvider = "github"
	ProviderGitLab    OAuthProvider = "gitlab"
	ProviderGoogle    OAuthProvider = "google"
	ProviderNotion    OAuthProvider = "notion"
	ProviderAtlassian OAuthProvider = "atlassian"
	ProviderGranola   OAuthProvider = "granola"
)

var ProviderAuthHosts = map[OAuthProvider][]string{
	ProviderGitHub:    {"github.com"},
	ProviderGitLab:    {"gitlab.com"},
	ProviderGoogle:    {"accounts.google.com"},
	ProviderNotion:    {"api.notion.com", "www.notion.so", "notion.so"},
	ProviderAtlassian: {"auth.atlassian.com", "api.atlassian.com"},
	ProviderGranola:   {"api.granola.so", "app.granola.so", "auth.granola.so", "granola.so"},
}

var ProviderAssetHosts = map[OAuthProvider][]string{
	ProviderGitHub:    {"github.githubassets.com", "avatars.githubusercontent.com"},
	ProviderGitLab:    {"assets.gitlab-static.net"},
	ProviderGoogle:    {"ssl.gstatic.com", "fonts.gstatic.com", "fonts.googleapis.com"},
	ProviderNotion:    {"cdnjs.cloudflare.com"},
}

var ProfileLock sync.Mutex

func PeekProfilePath() (string, error) {
	state, err := config.StateDir()
	if err != nil {
		return "", fmt.Errorf("resolve state dir: %w", err)
	}
	return filepath.Join(state, "uat", "browser", "profile"), nil
}

func ProfilePath() (string, error) {
	state, err := config.StateDir()
	if err != nil {
		return "", fmt.Errorf("resolve state dir: %w", err)
	}
	p := filepath.Join(state, "uat", "browser", "profile")
	if err := os.MkdirAll(p, 0700); err != nil {
		return "", fmt.Errorf("create uat browser profile: %w", err)
	}
	os.Chmod(p, 0700)
	return p, nil
}

func TempProfilePath(runID string) (string, error) {
	state, err := config.StateDir()
	if err != nil {
		return "", fmt.Errorf("resolve state dir: %w", err)
	}
	p := filepath.Join(state, "uat", "browser", "temp", runID)
	if err := os.MkdirAll(p, 0700); err != nil {
		return "", fmt.Errorf("create temp profile: %w", err)
	}
	os.Chmod(p, 0700)
	return p, nil
}

type ValidatedURL struct {
	URL        *url.URL
	ResolvedIP string
}

type URLValidator interface {
	Validate(rawURL string) (*ValidatedURL, error)
}

type OAuthPolicy struct {
	Provider    OAuthProvider
	LeasedPorts []int
}

func (p *OAuthPolicy) Validate(rawURL string) (*ValidatedURL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %v", err)
	}

	// Dynamic query is allowed for state/code, but userinfo and fragments remain refused as a security invariant
	if u.User != nil {
		return nil, fmt.Errorf("userinfo not allowed")
	}

	if u.Fragment != "" {
		return nil, fmt.Errorf("fragments not allowed")
	}

	if u.Scheme == "https" {
		host := u.Hostname()
		if p.Provider != "" {
			allowed := false
			for _, h := range ProviderAuthHosts[p.Provider] {
				if host == h {
					allowed = true
					break
				}
			}
			if !allowed {
				return nil, fmt.Errorf("origin %q not in OAuth registry for provider %q", host, p.Provider)
			}
		} else {
			return nil, fmt.Errorf("https origins are only allowed for specific providers, but no provider was set")
		}
	} else if u.Scheme == "http" {
		host := u.Hostname()
		if host != "localhost" && host != "127.0.0.1" {
			return nil, fmt.Errorf("http only allowed for localhost/127.0.0.1")
		}
		portStr := u.Port()
		if portStr == "" {
			return nil, fmt.Errorf("http requires explicit leased port")
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, fmt.Errorf("invalid port")
		}
		allowed := false
		for _, lp := range p.LeasedPorts {
			if port == lp {
				allowed = true
				break
			}
		}
		if !allowed {
			return nil, fmt.Errorf("port %d not leased to run", port)
		}
	} else {
		return nil, fmt.Errorf("scheme %q not allowed", u.Scheme)
	}

	return &ValidatedURL{URL: u}, nil
}

type PublicLinkPolicy struct {
	Resolver Resolver
}

func (p *PublicLinkPolicy) Validate(rawURL string) (*ValidatedURL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %v", err)
	}

	if u.User != nil {
		return nil, fmt.Errorf("userinfo not allowed")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("scheme %q not allowed", u.Scheme)
	}

	host := u.Hostname()
	if host == "localhost" {
		return nil, fmt.Errorf("localhost not allowed")
	}

	ips, err := p.Resolver.LookupIPAddr(context.Background(), host)
	if err != nil {
		return nil, fmt.Errorf("dns resolution failed: %v", err)
	}

	if len(ips) == 0 {
		return nil, fmt.Errorf("no IPs resolved")
	}

	for _, ip := range ips {
		parsed := ip.IP
		if parsed.IsLoopback() || parsed.IsPrivate() || parsed.IsLinkLocalMulticast() || parsed.IsLinkLocalUnicast() || parsed.IsUnspecified() || parsed.IsMulticast() {
			return nil, fmt.Errorf("resolved to non-public IP: %v", parsed)
		}
	}

	return &ValidatedURL{URL: u, ResolvedIP: ips[0].IP.String()}, nil
}
