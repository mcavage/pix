package uat

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBuildNavigateActions(t *testing.T) {
	// Test nil policy (bootstrap browser)
	actionsNil := buildNavigateActions(nil, "about:blank")
	if len(actionsNil) != 1 {
		t.Errorf("expected 1 action for nil policy, got %d", len(actionsNil))
	}

	// Test non-nil policy (e.g. OAuth)
	actionsPolicy := buildNavigateActions(&OAuthPolicy{}, "https://github.com/login")
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

	b, err := factory.NewContext(ctx, "testrun1", valU, &PublicLinkPolicy{})
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
