package uat

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"pix/host/config"
)

var OAuthOrigins = map[string]bool{
	"github.com":          true,
	"gitlab.com":          true,
	"accounts.google.com": true,
}

var ProfileLock sync.Mutex

func ProfilePath() (string, error) {
	state, err := config.StateDir()
	if err != nil {
		return "", fmt.Errorf("resolve state dir: %w", err)
	}
	p := filepath.Join(state, "uat", "browser", "profile")
	if err := os.MkdirAll(p, 0700); err != nil {
		return "", fmt.Errorf("create uat browser profile: %w", err)
	}
	// Ensure permissions in case it existed
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

type URLPolicy struct {
	LeasedPorts []int
}

func (p *URLPolicy) Validate(rawURL string) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid URL: %v", err)
	}

	if u.User != nil {
		return nil, fmt.Errorf("userinfo not allowed")
	}

	if u.Fragment != "" {
		return nil, fmt.Errorf("fragments not allowed")
	}

	if u.Scheme == "https" {
		host := u.Hostname()
		if !OAuthOrigins[host] {
			return nil, fmt.Errorf("origin %q not in OAuth registry", host)
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

	return u, nil
}
