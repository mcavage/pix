package uat

import (
	"context"
	"fmt"
	"net/url"
	"os/exec"
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
	// TODO: implement real CDP adapter if a small and safe one is needed.
	// For now, we return an error indicating it's not implemented, but we have
	// honestly failed if Chrome is absent.
	return nil, fmt.Errorf("real CDP adapter not yet implemented (found %s)", bin)
}

func (f *realFactory) NewOAuthContext(ctx context.Context, initialURL *url.URL) (Browser, error) {
	bin, err := findChrome()
	if err != nil {
		return nil, err
	}
	// TODO: implement real CDP adapter
	return nil, fmt.Errorf("real CDP adapter not yet implemented (found %s)", bin)
}
