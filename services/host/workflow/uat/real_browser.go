package uat

import (
	"context"
	"fmt"
	"net"
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

func buildNavigateActions(policy URLValidator, u string) []chromedp.Action {
	var actions []chromedp.Action
	if policy != nil {
		actions = append(actions, fetch.Enable())
	}
	return append(actions, chromedp.Navigate(u))
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

	b := &realBrowser{
		ctx:          c,
		cancelCtx:    cancelCtx,
		cancelAlloc:  cancelAlloc,
		clickables:   make(map[string]ClickableRef),
		activeOrigin: initialURL.URL,
	}

	chromedp.ListenTarget(c, b.requestHandler(policy, chromedp.Run))

	actions := buildNavigateActions(policy, initialURL.URL.String())

	if err := chromedp.Run(c, actions...); err != nil {
		if pErr := b.getPolicyErr(); pErr != nil {
			err = pErr
		}
		cancelCtx()
		cancelAlloc()
		return nil, err
	}
	if pErr := b.getPolicyErr(); pErr != nil {
		cancelCtx()
		cancelAlloc()
		return nil, pErr
	}

	return b, nil
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

	b := &realBrowser{
		ctx:          c,
		cancelCtx:    cancelCtx,
		cancelAlloc:  cancelAlloc,
		clickables:   make(map[string]ClickableRef),
		unlock:       unlock,
		activeOrigin: initialURL.URL,
	}

	chromedp.ListenTarget(c, b.requestHandler(policy, chromedp.Run))

	actions := buildNavigateActions(policy, initialURL.URL.String())

	if err = chromedp.Run(c, actions...); err != nil {
		if pErr := b.getPolicyErr(); pErr != nil {
			err = pErr
		}
		cancelCtx()
		cancelAlloc()
		return nil, err
	}
	if pErr := b.getPolicyErr(); pErr != nil {
		cancelCtx()
		cancelAlloc()
		return nil, pErr
	}

	handedOff = true
	return b, nil
}

type realBrowser struct {
	ctx         context.Context
	cancelCtx   context.CancelFunc
	cancelAlloc context.CancelFunc

	clickablesMu sync.Mutex
	clickables   map[string]ClickableRef

	unlock func()

	policyErrMu sync.Mutex
	policyErr   error

	originMu     sync.RWMutex
	activeOrigin *url.URL
}

func (b *realBrowser) setActiveOrigin(u *url.URL) {
	b.originMu.Lock()
	defer b.originMu.Unlock()
	b.activeOrigin = u
}

func (b *realBrowser) getActiveOrigin() *url.URL {
	b.originMu.RLock()
	defer b.originMu.RUnlock()
	return b.activeOrigin
}

func (b *realBrowser) setPolicyErr(err error) {
	b.policyErrMu.Lock()
	defer b.policyErrMu.Unlock()
	if b.policyErr == nil {
		b.policyErr = err
	}
}

func (b *realBrowser) getPolicyErr() error {
	b.policyErrMu.Lock()
	defer b.policyErrMu.Unlock()
	return b.policyErr
}

func (b *realBrowser) requestHandler(policy URLValidator, runCmd func(context.Context, ...chromedp.Action) error) func(ev interface{}) {
	return func(ev interface{}) {
		if policy == nil {
			return
		}
		if evReq, ok := ev.(*fetch.EventRequestPaused); ok {
			u := evReq.Request.URL
			if strings.HasPrefix(u, "about:") || strings.HasPrefix(u, "data:") || strings.HasPrefix(u, "blob:") {
				go runCmd(b.ctx, fetch.ContinueRequest(evReq.RequestID))
				return
			}
			go func() {
				ctx, cancel := context.WithTimeout(b.ctx, 5*time.Second)
				defer cancel()
				var errPolicy error
				if evReq.ResourceType == network.ResourceTypeDocument {
					_, errPolicy = policy.Validate(ctx, u)
					if errPolicy == nil {
						if parsedU, err := url.Parse(u); err == nil {
							b.setActiveOrigin(parsedU)
						}
					}
				} else {
					errPolicy = validateSubresource(ctx, u, b.getActiveOrigin(), policy)
				}

				if errPolicy != nil {
					b.setPolicyErr(errPolicy)
					runCmd(b.ctx, fetch.FailRequest(evReq.RequestID, network.ErrorReasonAccessDenied))
				} else {
					runCmd(b.ctx, fetch.ContinueRequest(evReq.RequestID))
				}
			}()
		}
	}
}

func validateSubresource(ctx context.Context, rawURL string, activeOrigin *url.URL, policy URLValidator) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid subresource URL: %v", err)
	}

	switch p := policy.(type) {
	case *PublicLinkPolicy:
		if u.Scheme != activeOrigin.Scheme || u.Hostname() != activeOrigin.Hostname() || u.Port() != activeOrigin.Port() {
			return fmt.Errorf("cross-origin subresource not allowed: %s", rawURL)
		}
		return nil
	case *OAuthPolicy:
		if u.Scheme != "https" {
			return fmt.Errorf("subresource scheme must be https: %s", rawURL)
		}
		host := u.Hostname()
		if host == "localhost" || host == "127.0.0.1" {
			return fmt.Errorf("localhost not allowed for subresources")
		}
		if net.ParseIP(host) != nil {
			return fmt.Errorf("IP literals not allowed for subresources")
		}

		for _, h := range ProviderAssetHosts[p.Provider] {
			if host == h {
				return nil
			}
		}

		if p.Resolver == nil {
			return fmt.Errorf("OAuthPolicy.Resolver not initialized")
		}
		ips, err := p.Resolver.LookupIPAddr(ctx, host)
		if err != nil || len(ips) == 0 {
			return fmt.Errorf("dns resolution failed for subresource %s: %v", host, err)
		}
		for _, ip := range ips {
			if ip.IP.IsLoopback() || ip.IP.IsPrivate() || ip.IP.IsLinkLocalMulticast() || ip.IP.IsLinkLocalUnicast() || ip.IP.IsUnspecified() || ip.IP.IsMulticast() {
				return fmt.Errorf("resolved to non-public IP: %v", ip.IP)
			}
		}
		return nil
	default:
		return fmt.Errorf("unknown policy type")
	}
}

func (b *realBrowser) Snapshot(ctx context.Context) (*Snapshot, error) {
	if err := b.getPolicyErr(); err != nil {
		return nil, err
	}
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
	b.clickablesMu.Lock()
	b.clickables = clickables
	b.clickablesMu.Unlock()

	return &Snapshot{
		DOM:        html,
		Screenshot: buf,
		Clickables: clickables,
	}, nil
}

func (b *realBrowser) Click(ctx context.Context, refID string) error {
	if err := b.getPolicyErr(); err != nil {
		return err
	}
	b.clickablesMu.Lock()
	ref, ok := b.clickables[refID]
	if ok {
		// Clear click refs on action that might navigate
		b.clickables = make(map[string]ClickableRef)
	}
	b.clickablesMu.Unlock()
	if !ok {
		return fmt.Errorf("invalid or unknown ref ID: %s", refID)
	}
	return chromedp.Run(b.ctx, chromedp.Click(fmt.Sprintf("[data-uat-ref='%s']", ref.ID), chromedp.ByQuery))
}

func (b *realBrowser) WaitForURL(ctx context.Context, expectedURL *url.URL) error {
	if err := b.getPolicyErr(); err != nil {
		return err
	}
	// Polling for URL
	deadline := time.Now().Add(10 * time.Second)
	if dl, ok := ctx.Deadline(); ok {
		deadline = dl
	}
	for time.Now().Before(deadline) {
		if err := b.getPolicyErr(); err != nil {
			return err
		}
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
					b.clickablesMu.Lock()
					b.clickables = make(map[string]ClickableRef)
					b.clickablesMu.Unlock()
					return nil
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return context.DeadlineExceeded
}

func (b *realBrowser) CurrentURL(ctx context.Context) (*url.URL, error) {
	if err := b.getPolicyErr(); err != nil {
		return nil, err
	}
	var u string
	if err := chromedp.Run(b.ctx, chromedp.Evaluate("window.location.href", &u)); err != nil {
		return nil, err
	}
	return url.Parse(u)
}

func (b *realBrowser) Title(ctx context.Context) (string, error) {
	if err := b.getPolicyErr(); err != nil {
		return "", err
	}
	var t string
	if err := chromedp.Run(b.ctx, chromedp.Title(&t)); err != nil {
		return "", err
	}
	return t, nil
}

func (b *realBrowser) VisibleText(ctx context.Context) (string, error) {
	if err := b.getPolicyErr(); err != nil {
		return "", err
	}
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
