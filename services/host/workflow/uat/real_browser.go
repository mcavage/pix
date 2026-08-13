package uat

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
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

func (f *realFactory) NewContext(ctx context.Context, runID string, initialURL *url.URL, policy *URLPolicy) (Browser, error) {
	bin, err := findChrome()
	if err != nil {
		return nil, err
	}

	profile, err := TempProfilePath(runID)
	if err != nil {
		return nil, err
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(bin),
		chromedp.Flag("headless", true),
		chromedp.UserDataDir(profile),
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)

	c, cancelCtx := chromedp.NewContext(allocCtx)

	chromedp.ListenTarget(c, func(ev interface{}) {
		if policy == nil {
			return
		}
		if evReq, ok := ev.(*network.EventRequestWillBeSent); ok {
			u := evReq.Request.URL
			if strings.HasPrefix(u, "about:") || strings.HasPrefix(u, "data:") {
				return
			}
			if _, err := policy.Validate(u); err != nil {
				cancelCtx()
			}
		}
	})

	if err := chromedp.Run(c, chromedp.Navigate(initialURL.String())); err != nil {
		cancelCtx()
		cancelAlloc()
		return nil, err
	}

	return &realBrowser{
		ctx:         c,
		cancelCtx:   cancelCtx,
		cancelAlloc: cancelAlloc,
		clickables:  make(map[string]ClickableRef),
	}, nil
}

func (f *realFactory) NewOAuthContext(ctx context.Context, initialURL *url.URL, policy *URLPolicy) (Browser, error) {
	bin, err := findChrome()
	if err != nil {
		return nil, err
	}

	profile, err := ProfilePath()
	if err != nil {
		return nil, err
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(bin),
		chromedp.Flag("headless", false), // non-headless for OAuth
		chromedp.UserDataDir(profile),
	)
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)

	c, cancelCtx := chromedp.NewContext(allocCtx)

	chromedp.ListenTarget(c, func(ev interface{}) {
		if policy == nil {
			return
		}
		if evReq, ok := ev.(*network.EventRequestWillBeSent); ok {
			u := evReq.Request.URL
			if strings.HasPrefix(u, "about:") || strings.HasPrefix(u, "data:") {
				return
			}
			if _, err := policy.Validate(u); err != nil {
				cancelCtx()
			}
		}
	})

	if err := chromedp.Run(c, chromedp.Navigate(initialURL.String())); err != nil {
		cancelCtx()
		cancelAlloc()
		return nil, err
	}

	return &realBrowser{
		ctx:         c,
		cancelCtx:   cancelCtx,
		cancelAlloc: cancelAlloc,
		clickables:  make(map[string]ClickableRef),
	}, nil
}

type realBrowser struct {
	ctx         context.Context
	cancelCtx   context.CancelFunc
	cancelAlloc context.CancelFunc
	clickables  map[string]ClickableRef
}

func (b *realBrowser) Snapshot(ctx context.Context) (*Snapshot, error) {
	var html string
	var buf []byte

	js := `
		(() => {
			const items = [];
			document.querySelectorAll('a, button, input[type="submit"], input[type="button"]').forEach((el, i) => {
				const id = 'ref-' + i;
				el.setAttribute('data-uat-ref', id);
				items.push({
					id: id,
					tag: el.tagName.toLowerCase(),
					text: el.innerText || el.value || ''
				});
			});
			return items;
		})();
	`
	var jsRes []map[string]interface{}

	if err := chromedp.Run(b.ctx,
		chromedp.Evaluate(js, &jsRes),
		chromedp.OuterHTML("html", &html, chromedp.ByQuery),
		chromedp.CaptureScreenshot(&buf),
	); err != nil {
		return nil, err
	}

	clickables := make(map[string]ClickableRef)
	for _, m := range jsRes {
		id, _ := m["id"].(string)
		tag, _ := m["tag"].(string)
		text, _ := m["text"].(string)
		clickables[id] = ClickableRef{ID: id, Tag: tag, Text: text}
	}
	b.clickables = clickables

	return &Snapshot{
		DOM:        html,
		Screenshot: buf,
		Clickables: clickables,
	}, nil
}

func (b *realBrowser) Click(ctx context.Context, refID string) error {
	ref, ok := b.clickables[refID]
	if !ok {
		return fmt.Errorf("invalid or unknown ref ID: %s", refID)
	}
	return chromedp.Run(b.ctx, chromedp.Click(fmt.Sprintf("[data-uat-ref='%s']", ref.ID), chromedp.ByQuery))
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
