package uat

import (
	"context"
	"net/url"
	"sync"
	"testing"
	"time"
)

func TestBrowserMethodsRace(t *testing.T) {
	initialURL, _ := url.Parse("https://github.com/login")
	b := &realBrowser{
		ctx:          context.Background(), // chromedp.Run will return ErrInvalidContext
		activeOrigin: initialURL,
		clickables:   make(map[string]ClickableRef),
	}

	// Add a dummy ref so Click has something to find
	b.clickables["test"] = ClickableRef{ID: "test"}

	var wg sync.WaitGroup

	// Click
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = b.Click(context.Background(), "test")
		}()
	}

	// WaitForURL
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			_ = b.WaitForURL(ctx, initialURL)
		}()
	}

	// ActiveOrigin / PolicyErr
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.setActiveOrigin(initialURL)
			_ = b.getActiveOrigin()
			b.setPolicyErr(nil)
			_ = b.getPolicyErr()
		}()
	}

	wg.Wait()
}

func TestSubresourceValidationRace(t *testing.T) {
	ctx := context.Background()
	policy := &OAuthPolicy{
		Provider: ProviderGitHub,
		Resolver: &realResolver{},
	}
	u, _ := url.Parse("https://github.com")

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = validateSubresource(ctx, "https://github.githubassets.com/foo.js", u, policy)
		}()
	}
	wg.Wait()
}
