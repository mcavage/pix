package uat

import (
	"context"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
)

func TestBrowserPolicyHandler(t *testing.T) {
	initialURL, _ := url.Parse("https://github.com/login")
	b := &realBrowser{
		ctx:          context.Background(),
		activeOrigin: initialURL,
	}

	policy := &OAuthPolicy{Provider: ProviderGitHub, Resolver: &realResolver{}}

	type actionRec struct {
		id     string
		isFail bool
	}
	actions := make(chan actionRec, 10)

	runCmd := func(ctx context.Context, acts ...chromedp.Action) error {
		for _, a := range acts {
			switch a.(type) {
			case *fetch.ContinueRequestParams:
				// RequestId is private? Let's check cdproto.
				// For test purposes we just record the type
				actions <- actionRec{"", false}
			case *fetch.FailRequestParams:
				actions <- actionRec{"", true}
			}
		}
		return nil
	}

	handler := b.requestHandler(policy, runCmd)

	// 1. simulate document request (allowed)
	handler(&fetch.EventRequestPaused{
		ResourceType: network.ResourceTypeDocument,
		Request:      &network.Request{URL: "https://github.com/login"},
		RequestID:    "1",
	})
	if r := <-actions; r.isFail {
		t.Errorf("expected ContinueRequest for document")
	}

	// 2. simulate CDN asset (allowed)
	handler(&fetch.EventRequestPaused{
		ResourceType: network.ResourceTypeStylesheet,
		Request:      &network.Request{URL: "https://github.githubassets.com/assets/frameworks.css"},
		RequestID:    "2",
	})
	if r := <-actions; r.isFail {
		t.Errorf("expected ContinueRequest for allowed CDN asset")
	}

	// 3. simulate private/cross-origin asset (denied)
	handler(&fetch.EventRequestPaused{
		ResourceType: network.ResourceTypeImage,
		Request:      &network.Request{URL: "https://127.0.0.1/evil.png"},
		RequestID:    "3",
	})
	if r := <-actions; !r.isFail {
		t.Errorf("expected FailRequest for private asset")
	}

	// Policy error should be surfaced on the browser
	err := b.getPolicyErr()
	if err == nil || !strings.Contains(err.Error(), "localhost not allowed") {
		t.Errorf("expected localhost error on browser context, got %v", err)
	}
}
func TestBuildNavigateActions(t *testing.T) {
	// Test nil policy (bootstrap browser)
	actionsNil := buildNavigateActions(nil, "about:blank")
	if len(actionsNil) != 1 {
		t.Errorf("expected 1 action for nil policy, got %d", len(actionsNil))
	}

	// Test non-nil policy (e.g. OAuth)
	actionsPolicy := buildNavigateActions(&OAuthPolicy{Resolver: &realResolver{}}, "https://github.com/login")
	if len(actionsPolicy) != 2 {
		t.Errorf("expected 2 actions for non-nil policy, got %d", len(actionsPolicy))
	}
}

func TestBrowserAllocatorArgv(t *testing.T) {

	tmpDir := t.TempDir()
	fakeBin := filepath.Join(tmpDir, "fake-chrome")
	os.WriteFile(fakeBin, []byte("#!/bin/sh\necho \"$@\" > "+filepath.Join(tmpDir, "args.txt")+"\nsleep 1\n"), 0755)

	os.Setenv("PIX_CHROME_BIN", fakeBin)
	defer os.Unsetenv("PIX_CHROME_BIN")

	factory := NewRealBrowserFactory()

	u, _ := url.Parse("https://public.com/test")
	valU := &ValidatedURL{
		URL:        u,
		ResolvedIP: "93.184.216.34",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	b, err := factory.NewContext(ctx, "testrun1", valU, &PublicLinkPolicy{Resolver: &realResolver{}})
	if b != nil {
		defer b.Close()
	}

	// wait a bit for script to write args
	time.Sleep(100 * time.Millisecond)

	// read args.txt
	argsBytes, err := os.ReadFile(filepath.Join(tmpDir, "args.txt"))
	if err != nil {
		t.Fatalf("failed to read args: %v", err)
	}
	args := string(argsBytes)

	if !strings.Contains(args, "--host-resolver-rules=MAP public.com 93.184.216.34") {
		t.Errorf("expected host-resolver-rules in argv, got: %s", args)
	}
}

func TestOAuthBrowserConcurrency(t *testing.T) {
	tmpDir := t.TempDir()
	fakeBin := filepath.Join(tmpDir, "fake-chrome")
	os.WriteFile(fakeBin, []byte("#!/bin/sh\nexit 1\n"), 0755)

	os.Setenv("PIX_CHROME_BIN", fakeBin)
	defer os.Unsetenv("PIX_CHROME_BIN")

	factory := NewRealBrowserFactory()
	u, _ := url.Parse("https://github.com/login")
	valU := &ValidatedURL{URL: u}

	// 1. Failure case unlocks immediately
	// fakeBin exits immediately, chromedp.Run fails to start browser.
	// This acquires the lock and then releases it on error.
	_, err := factory.NewOAuthContext(context.Background(), valU, nil)
	if err == nil {
		t.Fatal("expected error from fake chrome")
	}

	// If it deadlocked, this test would hang. We prove it unlocked by grabbing it.
	acquired := make(chan struct{})
	go func() {
		ProfileLock.Lock()
		ProfileLock.Unlock()
		close(acquired)
	}()

	select {
	case <-acquired:
		// success
	case <-time.After(1 * time.Second):
		t.Fatal("ProfileLock deadlocked after constructor failure")
	}
}

func TestOAuthBrowserLockHeldDuringLifetime(t *testing.T) {
	os.Unsetenv("PIX_CHROME_BIN") // Use real chrome if available

	factory := NewRealBrowserFactory()
	u, _ := url.Parse("data:text/html,<html><body>ok</body></html>")
	valU := &ValidatedURL{URL: u}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	b, err := factory.NewOAuthContext(ctx, valU, nil)
	if err != nil {
		t.Skipf("skipping because real chrome failed to start: %v", err)
	}

	// The lock should currently be HELD by the browser.
	// If we try to lock it now, it should block.
	acquired := make(chan struct{})
	go func() {
		ProfileLock.Lock()
		ProfileLock.Unlock()
		close(acquired)
	}()

	select {
	case <-acquired:
		t.Fatal("ProfileLock was unlocked before Close() was called")
	case <-time.After(500 * time.Millisecond):
		// Expected: it is blocked
	}

	// Now close the browser
	b.Close()

	// It should now unlock
	select {
	case <-acquired:
		// success
	case <-time.After(1 * time.Second):
		t.Fatal("ProfileLock did not unlock after Close()")
	}
}

func TestOAuthBrowserCloseIdempotent(t *testing.T) {
	// We will manually construct a realBrowser to test its Close method
	// since we can't easily start a real chrome.

	ProfileLock.Lock()
	unlockOnce := &sync.Once{}
	unlock := func() {
		unlockOnce.Do(func() {
			ProfileLock.Unlock()
		})
	}

	b := &realBrowser{
		cancelCtx:   func() {},
		cancelAlloc: func() {},
		unlock:      unlock,
	}

	// Close once
	b.Close()

	// Lock should be available
	acquired := make(chan struct{})
	go func() {
		ProfileLock.Lock()
		ProfileLock.Unlock()
		close(acquired)
	}()

	select {
	case <-acquired:
	case <-time.After(1 * time.Second):
		t.Fatal("Lock not released on Close")
	}

	// Close again shouldn't panic or unlock a lock we don't hold
	ProfileLock.Lock() // Grab it again
	defer ProfileLock.Unlock()
	b.Close() // Should do nothing because of sync.Once
}

func TestOAuthBrowserPanicRecovery(t *testing.T) {
	// Provide a fake chrome so findChrome doesn't fail before locking
	tmpDir := t.TempDir()
	fakeBin := filepath.Join(tmpDir, "fake-chrome")
	os.WriteFile(fakeBin, []byte("#!/bin/sh\nexit 1\n"), 0755)
	os.Setenv("PIX_CHROME_BIN", fakeBin)
	defer os.Unsetenv("PIX_CHROME_BIN")

	factory := NewRealBrowserFactory()

	// Cause a nil pointer dereference panic after lock is acquired
	// initialURL.URL is nil, but initialURL.ResolvedIP is set
	// This will panic at initialURL.URL.Hostname()
	valU := &ValidatedURL{
		URL:        nil,
		ResolvedIP: "1.2.3.4",
	}

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected panic, got none")
			}
		}()
		_, _ = factory.NewOAuthContext(context.Background(), valU, nil)
	}()

	// If the lock was not released by the deferred unlock(), this will deadlock
	acquired := make(chan struct{})
	go func() {
		ProfileLock.Lock()
		ProfileLock.Unlock()
		close(acquired)
	}()

	select {
	case <-acquired:
		// success, the lock was released during the panic!
	case <-time.After(1 * time.Second):
		t.Fatal("ProfileLock deadlocked after constructor panic")
	}
}

type delayedResolver struct {
	delay time.Duration
	fake  Resolver
}

func (d *delayedResolver) LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error) {
	select {
	case <-time.After(d.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return d.fake.LookupIPAddr(ctx, host)
}

func TestBrowserPolicyRedirectTracking(t *testing.T) {
	initialURL, _ := url.Parse("http://example.com")
	b := &realBrowser{
		ctx:          context.Background(),
		activeOrigin: initialURL,
	}

	policy := &PublicLinkPolicy{Resolver: &fakeResolver{
		ips: map[string][]net.IPAddr{
			"example.com":     {{IP: net.ParseIP("93.184.216.34")}},
			"www.example.com": {{IP: net.ParseIP("93.184.216.34")}},
		},
	}}

	type actionRec struct {
		id     string
		isFail bool
	}
	actions := make(chan actionRec, 10)
	runCmd := func(ctx context.Context, acts ...chromedp.Action) error {
		for _, a := range acts {
			switch a.(type) {
			case *fetch.ContinueRequestParams:
				actions <- actionRec{"", false}
			case *fetch.FailRequestParams:
				actions <- actionRec{"", true}
			}
		}
		return nil
	}

	handler := b.requestHandler(policy, runCmd)

	// 1. Redirect http -> https document
	handler(&fetch.EventRequestPaused{
		ResourceType: network.ResourceTypeDocument,
		Request:      &network.Request{URL: "https://example.com"},
		RequestID:    "1",
	})
	if r := <-actions; r.isFail {
		t.Errorf("expected ContinueRequest for https redirect")
	}

	// Active origin should now be https://example.com
	if b.getActiveOrigin().Scheme != "https" {
		t.Errorf("expected active origin scheme https, got %s", b.getActiveOrigin().Scheme)
	}

	// 2. Subresource on https://example.com (allowed)
	handler(&fetch.EventRequestPaused{
		ResourceType: network.ResourceTypeScript,
		Request:      &network.Request{URL: "https://example.com/app.js"},
		RequestID:    "2",
	})
	if r := <-actions; r.isFail {
		t.Errorf("expected ContinueRequest for subresource on https origin")
	}

	// 3. Subresource on http://example.com (denied, as active origin is https)
	handler(&fetch.EventRequestPaused{
		ResourceType: network.ResourceTypeScript,
		Request:      &network.Request{URL: "http://example.com/app.js"},
		RequestID:    "3",
	})
	if r := <-actions; !r.isFail {
		t.Errorf("expected FailRequest for http subresource under https active origin")
	}
	// clear policy err
	b.policyErr = nil

	// 4. Redirect apex -> www
	handler(&fetch.EventRequestPaused{
		ResourceType: network.ResourceTypeDocument,
		Request:      &network.Request{URL: "https://www.example.com"},
		RequestID:    "4",
	})
	if r := <-actions; r.isFail {
		t.Errorf("expected ContinueRequest for www redirect")
	}

	if b.getActiveOrigin().Hostname() != "www.example.com" {
		t.Errorf("expected active origin host www.example.com, got %s", b.getActiveOrigin().Hostname())
	}

	// 5. Subresource on www.example.com (allowed)
	handler(&fetch.EventRequestPaused{
		ResourceType: network.ResourceTypeScript,
		Request:      &network.Request{URL: "https://www.example.com/app.js"},
		RequestID:    "5",
	})
	if r := <-actions; r.isFail {
		t.Errorf("expected ContinueRequest for www asset")
	}
}

func TestDelayedResolverNonBlocking(t *testing.T) {
	initialURL, _ := url.Parse("https://github.com/login")
	b := &realBrowser{
		ctx:          context.Background(),
		activeOrigin: initialURL,
	}

	policy := &OAuthPolicy{
		Provider: ProviderGitHub,
		Resolver: &delayedResolver{
			delay: 200 * time.Millisecond,
			fake: &fakeResolver{
				ips: map[string][]net.IPAddr{
					"unknown.com": {{IP: net.ParseIP("93.184.216.34")}},
				},
			},
		},
	}

	actions := make(chan bool, 10)
	runCmd := func(ctx context.Context, acts ...chromedp.Action) error {
		for _, a := range acts {
			switch a.(type) {
			case *fetch.ContinueRequestParams:
				actions <- false
			case *fetch.FailRequestParams:
				actions <- true
			}
		}
		return nil
	}

	handler := b.requestHandler(policy, runCmd)

	start := time.Now()
	// trigger an unknown subresource that hits the resolver
	handler(&fetch.EventRequestPaused{
		ResourceType: network.ResourceTypeScript,
		Request:      &network.Request{URL: "https://unknown.com/script.js"},
		RequestID:    "1",
	})
	duration := time.Since(start)

	if duration > 50*time.Millisecond {
		t.Errorf("handler blocked for %v, should return immediately", duration)
	}

	// Eventually it continues because unknown.com resolves to public IP
	isFail := <-actions
	if isFail {
		t.Errorf("expected ContinueRequest for public asset")
	}
}
