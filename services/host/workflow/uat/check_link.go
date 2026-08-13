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
	u, err := cfg.Policy.Validate(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}

	b, err := factory.NewContext(ctx, cfg.RunID, u, cfg.Policy)
	if err != nil {
		return nil, fmt.Errorf("new context: %w", err)
	}
	defer b.Close()

	err = b.WaitForURL(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("wait for url: %w", err)
	}

	finalU, err := b.CurrentURL(ctx)
	if err != nil {
		return nil, fmt.Errorf("get current url: %w", err)
	}

	// Validate final URL against policy (redirects to disallowed origins)
	if _, err := cfg.Policy.Validate(finalU.String()); err != nil {
		return nil, fmt.Errorf("redirected to disallowed url %q: %w", finalU.String(), err)
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
