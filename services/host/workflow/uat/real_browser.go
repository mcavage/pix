package uat

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

func findChrome() (string, error) {
	candidates := []string{
		"google-chrome",
		"google-chrome-stable",
		"chromium",
		"chromium-browser",
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
	}
	for _, c := range candidates {
		if path, err := exec.LookPath(c); err == nil {
			return path, nil
		}
	}
	return "", ErrChromeAbsent
}

type realFactory struct{}

func NewRealBrowserFactory() BrowserFactory {
	return &realFactory{}
}

func (f *realFactory) NewContext(ctx context.Context, initialURL *url.URL) (Browser, error) {
	bin, err := findChrome()
	if err != nil {
		return nil, err
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(bin),
		chromedp.Flag("headless", true),
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	
	ctx, cancelCtx := chromedp.NewContext(allocCtx)
	
	if err := chromedp.Run(ctx, chromedp.Navigate(initialURL.String())); err != nil {
		cancelCtx()
		cancelAlloc()
		return nil, err
	}

	return &realBrowser{
		ctx:         ctx,
		cancelCtx:   cancelCtx,
		cancelAlloc: cancelAlloc,
	}, nil
}

func (f *realFactory) NewOAuthContext(ctx context.Context, initialURL *url.URL) (Browser, error) {
	// For OAuth we'd need a persistent profile, but for now we just run headless as well.
	return f.NewContext(ctx, initialURL)
}

type realBrowser struct {
	ctx         context.Context
	cancelCtx   context.CancelFunc
	cancelAlloc context.CancelFunc
}

func (b *realBrowser) Snapshot(ctx context.Context) (*Snapshot, error) {
	var html string
	var buf []byte

	if err := chromedp.Run(b.ctx,
		chromedp.OuterHTML("html", &html, chromedp.ByQuery),
		chromedp.CaptureScreenshot(&buf),
	); err != nil {
		return nil, err
	}
	
	return &Snapshot{
		DOM:        html,
		Screenshot: buf,
		Clickables: make(map[string]ClickableRef), // stub for now
	}, nil
}

func (b *realBrowser) Click(ctx context.Context, refID string) error {
	return chromedp.Run(b.ctx, chromedp.Click(fmt.Sprintf("[id='%s']", refID), chromedp.ByQuery))
}

func (b *realBrowser) WaitForURL(ctx context.Context, expectedURL *url.URL) error {
	expected := expectedURL.String()
	// Polling for URL
	deadline := time.Now().Add(10 * time.Second)
	if dl, ok := ctx.Deadline(); ok {
		deadline = dl
	}
	for time.Now().Before(deadline) {
		var u string
		if err := chromedp.Run(b.ctx, chromedp.Evaluate("window.location.href", &u)); err == nil {
			if strings.HasPrefix(u, expected) {
				return nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return context.DeadlineExceeded
}

func (b *realBrowser) CurrentURL(ctx context.Context) (*url.URL, error) {
	var u string
	if err := chromedp.Run(b.ctx, chromedp.Evaluate("window.location.href", &u)); err != nil {
		return nil, err
	}
	return url.Parse(u)
}

func (b *realBrowser) Title(ctx context.Context) (string, error) {
	var t string
	if err := chromedp.Run(b.ctx, chromedp.Title(&t)); err != nil {
		return "", err
	}
	return t, nil
}

func (b *realBrowser) VisibleText(ctx context.Context) (string, error) {
	var text string
	if err := chromedp.Run(b.ctx, chromedp.Evaluate("document.body.innerText", &text)); err != nil {
		return "", err
	}
	return text, nil
}

func (b *realBrowser) Close() error {
	b.cancelCtx()
	b.cancelAlloc()
	return nil
}
