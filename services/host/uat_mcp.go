package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"

	hostuat "pix/host/uat"
	"pix/host/workflow/uat"
)

func runUatMcp(args []string) error {
	fs := flag.NewFlagSet("uat-mcp", flag.ContinueOnError)
	// We want to return an error instead of exiting, but ContinueOnError will just return err.
	// But usage should not os.Exit either.
	fs.SetOutput(io.Discard) // Handle usage manually or don't print

	repo := fs.String("repo", "", "absolute path to the repo")
	state := fs.String("state", "", "absolute path to the state directory")
	session := fs.String("session", "", "strict session identifier")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *repo == "" || !filepath.IsAbs(*repo) {
		return fmt.Errorf("uat-mcp: --repo is required and must be absolute")
	}
	if *state == "" || !filepath.IsAbs(*state) {
		return fmt.Errorf("uat-mcp: --state is required and must be absolute")
	}
	if *session == "" {
		return fmt.Errorf("uat-mcp: --session is required")
	}

	if err := hostuat.ValidateID(*session); err != nil {
		return fmt.Errorf("uat-mcp: invalid session: %w", err)
	}

	if st, err := os.Stat(*repo); err != nil || !st.IsDir() {
		return fmt.Errorf("uat-mcp: repo must be an existing directory")
	}
	if st, err := os.Stat(*state); err != nil || !st.IsDir() {
		return fmt.Errorf("uat-mcp: state must be an existing directory")
	}

	if fs.NArg() > 0 {
		return fmt.Errorf("uat-mcp: unknown arguments: %v", fs.Args())
	}

	execAdapter := uat.NewRealExec()
	gitAdapter := uat.NewRealGit(*repo, execAdapter)
	sandboxAdapter := uat.NewRealSandbox(execAdapter)

	hostBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("uat-mcp: failed to get executable path: %w", err)
	}

	browserFactory := uat.NewRealBrowserFactory()
	mcpAdapter := uat.NewRealMCP(hostBin, *state, execAdapter, browserFactory)
	imageAdapter := uat.NewRealImage(execAdapter)
	leaseAdapter := uat.NewRealLease(*state, execAdapter)

	runner, err := uat.NewRunner(hostBin, *repo, *state, gitAdapter, execAdapter, sandboxAdapter, mcpAdapter, imageAdapter, leaseAdapter, 1)
	if err != nil {
		return fmt.Errorf("uat-mcp: failed to initialize runner: %w", err)
	}
	retryReport := runner.RetryCleanups()

	mcpServer := uat.NewMCPServer(runner, browserFactory, *state, os.Stdin, os.Stdout, retryReport)
	if err := mcpServer.Serve(context.Background()); err != nil {
		return fmt.Errorf("uat-mcp server failed: %w", err)
	}
	return nil
}

func runUatBrowserCaptureShim(args []string) error {
	capturePath := os.Getenv("PIX_UAT_BROWSER_CAPTURE")
	if capturePath == "" {
		return fmt.Errorf("PIX_UAT_BROWSER_CAPTURE is not set")
	}

	if len(args) != 1 {
		return fmt.Errorf("expected exactly 1 argument (URL), got %d", len(args))
	}

	rawURL := args[0]
	if len(rawURL) > 8192 {
		return fmt.Errorf("URL too long")
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}

	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("invalid scheme: %s", u.Scheme)
	}

	tmpPath := capturePath + ".tmp"
	if err := os.WriteFile(tmpPath, []byte(rawURL), 0600); err != nil {
		return err
	}
	return os.Rename(tmpPath, capturePath)
}
