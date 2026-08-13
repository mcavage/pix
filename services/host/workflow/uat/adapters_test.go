package uat

import (
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type captureExec struct {
	lastArgs []string
}

func (c *captureExec) CommandContext(ctx context.Context, name string, args ...string) ExecCmd {
	c.lastArgs = append([]string{name}, args...)
	return &fakeCmd{}
}

type fakeCmd struct{}

func (f *fakeCmd) Run() error                         { return nil }
func (f *fakeCmd) Start() error                       { return nil }
func (f *fakeCmd) Wait() error                        { return nil }
func (f *fakeCmd) Output() ([]byte, error)            { return nil, nil }
func (f *fakeCmd) StdoutPipe() (io.ReadCloser, error) { return nil, nil }
func (f *fakeCmd) StderrPipe() (io.ReadCloser, error) { return nil, nil }
func (f *fakeCmd) SetEnv(env []string)                {}
func (f *fakeCmd) SetDir(dir string)                  {}

func TestAdapters_LeaseCleanupSandboxRemoveArgv(t *testing.T) {
	ce := &captureExec{}
	stateDir := t.TempDir()

	// Create a run and a sandbox lease
	runID := "testrun123"
	leaseDir := filepath.Join(stateDir, "leases", runID)
	os.MkdirAll(leaseDir, 0700)
	l := NewRealLease(stateDir, ce)
	if err := l.Acquire(context.Background(), runID, "sandbox_pix-uat-"+runID); err != nil {
		t.Fatalf("acquire error: %v", err)
	}

	err := l.Cleanup(context.Background(), runID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	joined := strings.Join(ce.lastArgs, " ")
	expected := "sbx rm -f pix-uat-" + runID
	if joined != expected {
		t.Errorf("expected %q, got %q", expected, joined)
	}
}

func TestAdapters_GitShowArgv(t *testing.T) {
	ce := &captureExec{}
	g := NewRealGit("/repo", ce)
	_, err := g.ReadTreeFile(context.Background(), "abc1234", "dir/file.txt")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	joined := strings.Join(ce.lastArgs, " ")
	expected := "git -C /repo show --end-of-options abc1234:dir/file.txt"
	if joined != expected {
		t.Errorf("expected %q, got %q", expected, joined)
	}
}

func TestAdapters_MCPAddArgv(t *testing.T) {
	ce := &captureExec{}
	m := NewRealMCP("/pix-host", "/state", ce, &mockBrowserFactory{})
	err := m.Add(context.Background(), "my-mcp", []string{"mcp", "add", "my-mcp", "--command", "host"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	joined := strings.Join(ce.lastArgs, " ")
	expected := "sbx mcp add my-mcp --command host"
	if joined != expected {
		t.Errorf("expected %q, got %q", expected, joined)
	}
}

func TestAdapters_Integration(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}

	repoPath := t.TempDir()

	runGit := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v, output: %s", args, err, out)
		}
	}

	runGit(repoPath, "init")
	runGit(repoPath, "config", "user.name", "test")
	runGit(repoPath, "config", "user.email", "test@test.com")

	err := os.WriteFile(filepath.Join(repoPath, "hello.txt"), []byte("world\n"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	runGit(repoPath, "add", "hello.txt")
	runGit(repoPath, "commit", "-m", "init")

	g := NewRealGit(repoPath, nil)

	// Test ResolveCommit(HEAD)
	headSHA, err := g.ResolveCommit(context.Background(), "HEAD")
	if err != nil {
		t.Fatalf("ResolveCommit failed: %v", err)
	}
	if len(headSHA) != 40 {
		t.Fatalf("Expected full SHA, got: %q", headSHA)
	}

	// Test ReadTreeFile
	content, err := g.ReadTreeFile(context.Background(), headSHA, "hello.txt")
	if err != nil {
		t.Fatalf("ReadTreeFile failed: %v", err)
	}
	if string(content) != "world\n" {
		t.Fatalf("Expected 'world\\n', got %q", content)
	}

	// Test Clone
	destPath := t.TempDir()
	os.RemoveAll(destPath)
	err = g.Clone(context.Background(), headSHA, destPath)
	if err != nil {
		t.Fatalf("Clone failed: %v", err)
	}

	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = destPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Failed to get HEAD in cloned repo: %v", err)
	}
	clonedSHA := strings.TrimSpace(string(out))
	if clonedSHA != headSHA {
		t.Fatalf("Expected cloned HEAD to be %q, got %q", headSHA, clonedSHA)
	}

	badRevs := []string{
		"-HEAD",
		"HEAD..main",
		"HEAD@{1}",
		"HEAD ",
		"HEAD\t",
	}
	for _, bad := range badRevs {
		_, err := g.ResolveCommit(context.Background(), bad)
		if err == nil {
			t.Errorf("expected error for bad revision %q", bad)
		}
	}
}

type authCaptureExec struct {
	cmds     [][]string
	env      []string
	waitFunc func() error
}

func (c *authCaptureExec) CommandContext(ctx context.Context, name string, args ...string) ExecCmd {
	c.cmds = append(c.cmds, append([]string{name}, args...))
	return &authFakeCmd{exec: c, waitCh: make(chan error, 1)}
}

type authFakeCmd struct {
	exec   *authCaptureExec
	waitCh chan error
}

func (f *authFakeCmd) Run() error { return nil }
func (f *authFakeCmd) Start() error {
	if f.exec.waitFunc != nil {
		go func() {
			f.waitCh <- f.exec.waitFunc()
		}()
	} else {
		f.waitCh <- nil
	}
	return nil
}
func (f *authFakeCmd) Wait() error                        { return <-f.waitCh }
func (f *authFakeCmd) Output() ([]byte, error)            { return nil, nil }
func (f *authFakeCmd) StdoutPipe() (io.ReadCloser, error) { return nil, nil }
func (f *authFakeCmd) StderrPipe() (io.ReadCloser, error) { return nil, nil }
func (f *authFakeCmd) SetEnv(env []string) {
	f.exec.env = env
}
func (f *authFakeCmd) SetDir(dir string) {}

func TestAdapters_MCPAuthCaptureSuccess(t *testing.T) {
	stateDir := t.TempDir()
	runID := "testrun-auth-123"

	pixHost := filepath.Join(stateDir, "fake-pix-host")
	os.WriteFile(pixHost, []byte("#!/bin/sh\n"), 0755)

	factory := &mockBrowserFactory{
		browser: &mockBrowser{
			currURL: &url.URL{
				Scheme:   "http",
				Host:     "localhost:8080",
				Path:     "/cb",
				RawQuery: "code=123&state=xyz",
			},
		},
	}
	ce := &authCaptureExec{}

	m := NewRealMCP(pixHost, stateDir, ce, factory)

	// Simulate sbx writing to the capture path
	ce.waitFunc = func() error {
		capturePath := filepath.Join(stateDir, "runs", runID, "browser_capture")
		os.MkdirAll(filepath.Dir(capturePath), 0700)
		os.WriteFile(capturePath, []byte("https://github.com/login?redirect_uri=http%3A%2F%2Flocalhost%3A8080%2Fcb"), 0600)
		return nil
	}

	err := m.Auth(context.Background(), runID, "some-mcp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify env
	foundBrowser := false
	foundCapture := false
	for _, e := range ce.env {
		if strings.HasPrefix(e, "BROWSER=") {
			foundBrowser = true
			expectedShim := filepath.Join(stateDir, "runs", runID, "bin", "pix-uat-browser")
			if e != "BROWSER="+expectedShim {
				t.Errorf("unexpected BROWSER: %s", e)
			}
		}
		if strings.HasPrefix(e, "PIX_UAT_BROWSER_CAPTURE=") {
			foundCapture = true
		}
	}
	if !foundBrowser || !foundCapture {
		t.Errorf("missing env vars. env: %v", ce.env)
	}
}

func TestAdapters_MCPAuthCaptureTimeout(t *testing.T) {
	stateDir := t.TempDir()
	runID := "testrun-auth-456"

	pixHost := filepath.Join(stateDir, "fake-pix-host")
	os.WriteFile(pixHost, []byte("#!/bin/sh\n"), 0755)

	factory := &mockBrowserFactory{}
	ce := &authCaptureExec{}

	m := NewRealMCP(pixHost, stateDir, ce, factory)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// Simulate sbx ignoring BROWSER and not writing to capture path but hanging open
	ce.waitFunc = func() error {
		<-ctx.Done()
		return nil
	}

	err := m.Auth(ctx, runID, "some-mcp")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !errors.Is(err, ErrIncomplete) {
		t.Errorf("expected ErrIncomplete, got %v", err)
	}
	if !strings.Contains(err.Error(), "host bootstrap") {
		t.Errorf("expected host bootstrap evidence, got %v", err)
	}
}

func TestAdapters_MCPAuthExitZeroBeforeCapture(t *testing.T) {
	stateDir := t.TempDir()
	runID := "testrun-auth-exitzero"

	pixHost := filepath.Join(stateDir, "fake-pix-host")
	os.WriteFile(pixHost, []byte("#!/bin/sh\n"), 0755)

	factory := &mockBrowserFactory{}
	ce := &authCaptureExec{}

	m := NewRealMCP(pixHost, stateDir, ce, factory)

	// Make it exit 0 instantly but NO capture written
	ce.waitFunc = func() error {
		return nil
	}

	// NO timeout here
	ctx := context.Background()

	err := m.Auth(ctx, runID, "some-mcp")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	if !errors.Is(err, ErrIncomplete) {
		t.Errorf("expected ErrIncomplete, got %v", err)
	}
	if !strings.Contains(err.Error(), "exited without writing capture") {
		t.Errorf("expected exited without writing capture message, got %v", err)
	}
}

func TestAdapters_MCPAuthExitBeforeCapture(t *testing.T) {
	stateDir := t.TempDir()
	runID := "testrun-auth-exit"

	pixHost := filepath.Join(stateDir, "fake-pix-host")
	os.WriteFile(pixHost, []byte("#!/bin/sh\n"), 0755)

	factory := &mockBrowserFactory{}
	ce := &authCaptureExec{}

	m := NewRealMCP(pixHost, stateDir, ce, factory)

	exitErr := errors.New("simulated sbx failure")
	// Make it fail instantly
	ce.waitFunc = func() error {
		return exitErr
	}

	// NO timeout here, proving it doesn't wait 30 seconds
	ctx := context.Background()

	err := m.Auth(ctx, runID, "some-mcp")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	// Should contain the simulated error
	if !strings.Contains(err.Error(), "simulated sbx failure") {
		t.Errorf("expected simulated failure, got %v", err)
	}
	if !strings.Contains(err.Error(), "failed before capture") {
		t.Errorf("expected failed before capture message, got %v", err)
	}
}

type mockBrowserFactory struct {
	browser *mockBrowser
}

func (m *mockBrowserFactory) NewContext(ctx context.Context, runID string, initialURL *ValidatedURL, policy URLValidator) (Browser, error) {
	return nil, nil
}

func (m *mockBrowserFactory) NewOAuthContext(ctx context.Context, initialURL *ValidatedURL, policy URLValidator) (Browser, error) {
	if m.browser == nil {
		m.browser = &mockBrowser{}
	}
	return m.browser, nil
}

// Using the same fakeBrowser pattern from oauth_test.go
type mockBrowser struct {
	currURL *url.URL
}

func (f *mockBrowser) Snapshot(ctx context.Context) (*Snapshot, error) { return nil, nil }
func (f *mockBrowser) Click(ctx context.Context, refID string) error   { return nil }
func (f *mockBrowser) WaitForURL(ctx context.Context, expectedURL *url.URL) error {
	if f.currURL != nil {
		if f.currURL.Scheme != expectedURL.Scheme ||
			f.currURL.Host != expectedURL.Host ||
			f.currURL.Path != expectedURL.Path ||
			f.currURL.User != nil ||
			f.currURL.Fragment != "" {
			return context.DeadlineExceeded
		}
	}
	return nil
}
func (f *mockBrowser) CurrentURL(ctx context.Context) (*url.URL, error) { return f.currURL, nil }
func (f *mockBrowser) Title(ctx context.Context) (string, error)        { return "", nil }
func (f *mockBrowser) VisibleText(ctx context.Context) (string, error)  { return "", nil }
func (f *mockBrowser) Close() error                                     { return nil }
func TestAdapters_MCPAuthCaptureSuccessWaitSettle(t *testing.T) {
	stateDir := t.TempDir()
	runID := "testrun-auth-settle"

	pixHost := filepath.Join(stateDir, "fake-pix-host")
	os.WriteFile(pixHost, []byte("#!/bin/sh\n"), 0755)

	factory := &mockBrowserFactory{
		browser: &mockBrowser{
			currURL: &url.URL{
				Scheme:   "http",
				Host:     "localhost:8080",
				Path:     "/cb",
				RawQuery: "code=123&state=xyz",
			},
		},
	}
	ce := &authCaptureExec{}
	m := NewRealMCP(pixHost, stateDir, ce, factory)

	ce.waitFunc = func() error {
		capturePath := filepath.Join(stateDir, "runs", runID, "browser_capture")
		os.MkdirAll(filepath.Dir(capturePath), 0700)
		os.WriteFile(capturePath, []byte("https://github.com/login?redirect_uri=http%3A%2F%2Flocalhost%3A8080%2Fcb"), 0600)
		// Don't return immediately, simulate waiting for browser
		time.Sleep(100 * time.Millisecond)
		return nil
	}

	err := m.Auth(context.Background(), runID, "some-mcp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
func TestAdapters_LeaseRecords(t *testing.T) {
	stateDir := t.TempDir()
	ce := &authCaptureExec{}
	l := NewRealLease(stateDir, ce)
	ctx := context.Background()
	runID := "run-special-chars"

	// acquire with slashes, colons, underscores
	resNames := []string{
		"sandbox_my-sandbox_1",
		"image_uat-some/image:v1",
		"template_docker.io/mcavage/pix:uat-123",
		"mcp:uat-run1-gworkspace",
	}
	for _, res := range resNames {
		if err := l.Acquire(ctx, runID, res); err != nil {
			t.Fatalf("Acquire(%s) error: %v", res, err)
		}
	}

	// add a malformed record manually
	leaseDir := filepath.Join(stateDir, "leases", runID)
	os.WriteFile(filepath.Join(leaseDir, "malformed_hash123"), []byte("not-json"), 0600)

	err := l.Cleanup(ctx, runID)
	if err == nil {
		t.Fatal("expected cleanup to return error for malformed record")
	}
	if !strings.Contains(err.Error(), "malformed lease record") {
		t.Fatalf("expected malformed error, got: %v", err)
	}

	// write unknown kind manually
	os.Remove(filepath.Join(leaseDir, "malformed_hash123")) // clean it
	os.WriteFile(filepath.Join(leaseDir, "unknown_hash123"), []byte(`{"kind":"magic","name":"wand"}`), 0600)

	err = l.Cleanup(ctx, runID)
	if err == nil {
		t.Fatal("expected cleanup to return error for unknown kind")
	}
	if !strings.Contains(err.Error(), "refusing foreign lease kind: magic") {
		t.Fatalf("expected unknown kind error, got: %v", err)
	}
}

func TestMCPAuthCaptureErrorImmediate(t *testing.T) {
	origRead := readCaptureFileFunc
	defer func() { readCaptureFileFunc = origRead }()
	
	readCaptureFileFunc = func(path string) (string, error) {
		return "", os.ErrPermission
	}

	stateDir := t.TempDir()
	ce := &captureExec{}
	factory := &mockBrowserFactory{}
	m := NewRealMCP("localhost", stateDir, ce, factory)

	err := m.Auth(context.Background(), "run2", "test-srv")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "security or platform error") {
		t.Fatalf("expected security error, got %v", err)
	}
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("expected ErrIncomplete, got %v", err)
	}
}
