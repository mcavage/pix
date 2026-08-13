package main

import (
	"context"
	"flag"
	"fmt"
	"io"
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
	mcpAdapter := uat.NewRealMCP(execAdapter)
	imageAdapter := uat.NewRealImage(execAdapter)
	leaseAdapter := uat.NewRealLease(*state, execAdapter)

	runner := uat.NewRunner(*repo, *state, gitAdapter, execAdapter, sandboxAdapter, mcpAdapter, imageAdapter, leaseAdapter, 1)
	runner.RetryCleanups()
	browserFactory := uat.NewRealBrowserFactory()

	mcpServer := uat.NewMCPServer(runner, browserFactory, *state, os.Stdin, os.Stdout)
	if err := mcpServer.Serve(context.Background()); err != nil {
		return fmt.Errorf("uat-mcp server failed: %w", err)
	}
	return nil
}

func runUatBrowserOpen(args []string) error {
	fs := flag.NewFlagSet("uat-browser-open", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	runID := fs.String("run-id", "", "run id")
	authURL := fs.String("auth-url", "", "auth URL")
	callbackURL := fs.String("callback-url", "", "callback URL")
	origin := fs.String("origin", "", "expected origin hostname")
	outPath := fs.String("out", "", "output file path")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *runID == "" || *authURL == "" || *callbackURL == "" || *origin == "" || *outPath == "" {
		return fmt.Errorf("uat-browser-open: missing required arguments")
	}

	cfg := uat.OAuthConfig{
		RunID:       *runID,
		AuthURL:     *authURL,
		CallbackURL: *callbackURL,
		Origin:      *origin,
		Policy:      &uat.URLPolicy{LeasedPorts: []int{}},
	}

	factory := uat.NewRealBrowserFactory()

	if err := uat.CaptureOAuth(context.Background(), factory, cfg, *outPath); err != nil {
		return fmt.Errorf("uat-browser-open: capture failed")
	}
	return nil
}
