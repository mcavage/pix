package uat

import (
	"context"
	"fmt"
)

type CheckLinkConfig struct {
	RunID  string
	Policy URLValidator
}

func CheckLink(ctx context.Context, factory BrowserFactory, cfg CheckLinkConfig, rawURL string) (*LinkCheckResult, error) {
	valU, err := cfg.Policy.Validate(ctx, rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}

	b, err := factory.NewContext(ctx, cfg.RunID, valU, cfg.Policy)
	if err != nil {
		return nil, fmt.Errorf("new context: %w", err)
	}
	defer b.Close()

	err = b.WaitForURL(ctx, valU.URL)
	if err != nil {
		return nil, fmt.Errorf("wait for url: %w", err)
	}

	finalU, err := b.CurrentURL(ctx)
	if err != nil {
		return nil, fmt.Errorf("get current url: %w", err)
	}

	// Validate final URL against policy (redirects to disallowed origins)
	valFinal, err := cfg.Policy.Validate(ctx, finalU.String())
	if err != nil {
		return nil, fmt.Errorf("redirected to disallowed url %q: %w", finalU.String(), err)
	}
	if valFinal.URL.Hostname() != valU.URL.Hostname() || valFinal.URL.Port() != valU.URL.Port() || valFinal.URL.Scheme != valU.URL.Scheme {
		return nil, fmt.Errorf("cross-origin redirect not allowed: original %s://%s, final %s://%s", valU.URL.Scheme, valU.URL.Host, valFinal.URL.Scheme, valFinal.URL.Host)
	}

	text, err := b.VisibleText(ctx)
	if err != nil {
		return nil, fmt.Errorf("visible text: %w", err)
	}

	// Cap text
	if len(text) > 100000 {
		text = text[:100000]
	}

	snap, err := b.Snapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}

	screenshot := snap.Screenshot
	// Cap screenshot (10MB for example)
	if len(screenshot) > 10*1024*1024 {
		screenshot = screenshot[:10*1024*1024]
	}

	title, err := b.Title(ctx)
	if err != nil {
		return nil, fmt.Errorf("title: %w", err)
	}

	return &LinkCheckResult{
		Title:      title,
		Text:       text,
		FinalURL:   finalU.String(),
		Screenshot: screenshot,
	}, nil
}
