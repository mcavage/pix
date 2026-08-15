package uat

import (
	"context"
	"errors"
	"net/url"
)

var (
	ErrChromeAbsent = errors.New("chrome is not installed")
	ErrIncomplete   = errors.New("incomplete: profile expired, MFA, or login required")
)

type Snapshot struct {
	DOM        string
	Screenshot []byte
	Clickables map[string]ClickableRef
}

type ClickableRef struct {
	ID   string
	Tag  string
	Text string
}

type Browser interface {
	Snapshot(ctx context.Context) (*Snapshot, error)
	Click(ctx context.Context, refID string) error
	WaitForURL(ctx context.Context, expectedURL *url.URL) error
	CurrentURL(ctx context.Context) (*url.URL, error)
	Title(ctx context.Context) (string, error)
	VisibleText(ctx context.Context) (string, error)
	Close() error
}

type LinkCheckResult struct {
	Title      string
	Text       string
	FinalURL   string
	Screenshot []byte
}

type BrowserFactory interface {
	// NewContext creates a new incognito/fresh context starting at initialURL.
	NewContext(ctx context.Context, runID string, initialURL *ValidatedURL, policy URLValidator) (Browser, error)
	// NewOAuthContext creates a context using the persistent 0700 UAT profile starting at initialURL.
	NewOAuthContext(ctx context.Context, initialURL *ValidatedURL, policy URLValidator) (Browser, error)
}
