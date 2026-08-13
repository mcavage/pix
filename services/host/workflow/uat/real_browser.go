package uat

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

func findChrome() (string, error) {
	if override := os.Getenv("PIX_CHROME_BIN"); override != "" {
		return override, nil
	}
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

func (f *realFactory) NewContext(ctx context.Context, runID string, initialURL *ValidatedURL, policy URLValidator) (Browser, error) {
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
	if initialURL.ResolvedIP != "" {
		opts = append(opts, chromedp.Flag("host-resolver-rules", fmt.Sprintf("MAP %s %s", initialURL.URL.Hostname(), initialURL.ResolvedIP)))
	}
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)

	c, cancelCtx := chromedp.NewContext(allocCtx)

	chromedp.ListenTarget(c, func(ev interface{}) {
		if policy == nil {
			return
		}
		if evReq, ok := ev.(*fetch.EventRequestPaused); ok {
			u := evReq.Request.URL
			if strings.HasPrefix(u, "about:") || strings.HasPrefix(u, "data:") {
				go chromedp.Run(c, fetch.ContinueRequest(evReq.RequestID))
				return
			}
			if _, err := policy.Validate(u); err != nil {
				go chromedp.Run(c, fetch.FailRequest(evReq.RequestID, network.ErrorReasonAccessDenied))
				cancelCtx()
			} else {
				go chromedp.Run(c, fetch.ContinueRequest(evReq.RequestID))
			}
		}
	})

	if err := chromedp.Run(c, fetch.Enable(), chromedp.Navigate(initialURL.URL.String())); err != nil {
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

func (f *realFactory) NewOAuthContext(ctx context.Context, initialURL *ValidatedURL, policy URLValidator) (Browser, error) {
	bin, err := findChrome()
	if err != nil {
		return nil, err
	}

	profile, err := ProfilePath()
	if err != nil {
		return nil, err
	}

	ProfileLock.Lock()
	unlockOnce := &sync.Once{}
	unlock := func() {
		unlockOnce.Do(func() {
			ProfileLock.Unlock()
		})
	}
	handedOff := false
	defer func() {
		if !handedOff {
			unlock()
		}
	}()

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(bin),
		chromedp.Flag("headless", false), // non-headless for OAuth
		chromedp.UserDataDir(profile),
	)
	if initialURL.ResolvedIP != "" {
		opts = append(opts, chromedp.Flag("host-resolver-rules", fmt.Sprintf("MAP %s %s", initialURL.URL.Hostname(), initialURL.ResolvedIP)))
	}
	allocCtx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)

	c, cancelCtx := chromedp.NewContext(allocCtx)

	chromedp.ListenTarget(c, func(ev interface{}) {
		if policy == nil {
			return
		}
		if evReq, ok := ev.(*fetch.EventRequestPaused); ok {
			u := evReq.Request.URL
			if strings.HasPrefix(u, "about:") || strings.HasPrefix(u, "data:") {
				go chromedp.Run(c, fetch.ContinueRequest(evReq.RequestID))
				return
			}
			if _, errPolicy := policy.Validate(u); errPolicy != nil {
				go chromedp.Run(c, fetch.FailRequest(evReq.RequestID, network.ErrorReasonAccessDenied))
				cancelCtx()
			} else {
				go chromedp.Run(c, fetch.ContinueRequest(evReq.RequestID))
			}
		}
	})

	if err = chromedp.Run(c, fetch.Enable(), chromedp.Navigate(initialURL.URL.String())); err != nil {
		cancelCtx()
		cancelAlloc()
		return nil, err
	}

	handedOff = true
	return &realBrowser{
		ctx:         c,
		cancelCtx:   cancelCtx,
		cancelAlloc: cancelAlloc,
		clickables:  make(map[string]ClickableRef),
		unlock:      unlock,
	}, nil
}

type realBrowser struct {
	ctx         context.Context
	cancelCtx   context.CancelFunc
	cancelAlloc context.CancelFunc
	clickables  map[string]ClickableRef
	unlock      func()
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
	// Clear click refs on action that might navigate
	b.clickables = make(map[string]ClickableRef)
	return chromedp.Run(b.ctx, chromedp.Click(fmt.Sprintf("[data-uat-ref='%s']", ref.ID), chromedp.ByQuery))
}

func (b *realBrowser) WaitForURL(ctx context.Context, expectedURL *url.URL) error {
	// Polling for URL
	deadline := time.Now().Add(10 * time.Second)
	if dl, ok := ctx.Deadline(); ok {
		deadline = dl
	}
	for time.Now().Before(deadline) {
		var u string
		if err := chromedp.Run(b.ctx, chromedp.Evaluate("window.location.href", &u)); err == nil {
			parsedU, parseErr := url.Parse(u)
			if parseErr == nil {
				if parsedU.Scheme == expectedURL.Scheme &&
					parsedU.Host == expectedURL.Host &&
					parsedU.Path == expectedURL.Path &&
					parsedU.User == nil &&
					parsedU.Fragment == "" {
					// Clear clickables on navigation
					b.clickables = make(map[string]ClickableRef)
					return nil
				}
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
	if b.unlock != nil {
		b.unlock()
	}
	return nil
}
