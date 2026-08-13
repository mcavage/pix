package uat

import (
	"context"
	"fmt"
)

func CheckLink(ctx context.Context, factory BrowserFactory, cfg OAuthConfig, rawURL string) (*LinkCheckResult, error) {
	u, err := cfg.Policy.Validate(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}

	b, err := factory.NewContext(ctx, cfg.RunID, u, cfg.Policy)
	if err != nil {
		return nil, fmt.Errorf("new context: %w", err)
	}
	defer b.Close()

	// Wait for the URL to settle (this might just wait for idle in a real implementation)
	// For CheckLink, we probably just want to wait until it's loaded, then get info.
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

	snap, err := b.Snapshot(ctx)
	if err != nil {
		return nil, fmt.Errorf("snapshot: %w", err)
	}

	title, err := b.Title(ctx)
	if err != nil {
		return nil, fmt.Errorf("title: %w", err)
	}

	return &LinkCheckResult{
		Title:      title,
		Text:       text,
		FinalURL:   finalU.String(),
		Screenshot: snap.Screenshot,
	}, nil
}
