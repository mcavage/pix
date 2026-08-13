package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"pix/host/workflow/uat"
)

func runUatMcp(args []string) {
	fs := flag.NewFlagSet("uat-mcp", flag.ExitOnError)
	repo := fs.String("repo", "", "absolute path to the repo")
	state := fs.String("state", "", "absolute path to the state directory")
	session := fs.String("session", "", "strict session identifier")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: pix-host uat-mcp --repo <path> --state <path> --session <id>\n")
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if *repo == "" || !filepath.IsAbs(*repo) {
		fmt.Fprintf(os.Stderr, "pix-host uat-mcp: --repo is required and must be absolute\n")
		os.Exit(2)
	}
	if *state == "" || !filepath.IsAbs(*state) {
		fmt.Fprintf(os.Stderr, "pix-host uat-mcp: --state is required and must be absolute\n")
		os.Exit(2)
	}
	if *session == "" {
		fmt.Fprintf(os.Stderr, "pix-host uat-mcp: --session is required\n")
		os.Exit(2)
	}

	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "pix-host uat-mcp: unknown arguments: %v\n", fs.Args())
		os.Exit(2)
	}

	gitAdapter := uat.NewRealGit(*repo)
	sandboxAdapter := uat.NewRealSandbox()
	mcpAdapter := uat.NewRealMCP()
	imageAdapter := uat.NewRealImage()
	execAdapter := uat.NewRealExec()
	leaseAdapter := uat.NewRealLease(*state)

	runner := uat.NewRunner(*repo, *state, gitAdapter, execAdapter, sandboxAdapter, mcpAdapter, imageAdapter, leaseAdapter, 1)
	browserFactory := uat.NewRealBrowserFactory()
	
	mcpServer := uat.NewMCPServer(runner, browserFactory, *state, os.Stdin, os.Stdout)
	if err := mcpServer.Serve(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "pix-host uat-mcp server failed: %v\n", err)
		os.Exit(1)
	}
}

func runUatBrowserOpen(args []string) {
	fs := flag.NewFlagSet("uat-browser-open", flag.ExitOnError)
	runID := fs.String("run-id", "", "run id")
	authURL := fs.String("auth-url", "", "auth URL")
	callbackURL := fs.String("callback-url", "", "callback URL")
	origin := fs.String("origin", "", "expected origin hostname")
	outPath := fs.String("out", "", "output file path")

	if err := fs.Parse(args); err != nil {
		os.Exit(2)
	}

	if *runID == "" || *authURL == "" || *callbackURL == "" || *origin == "" || *outPath == "" {
		fmt.Fprintf(os.Stderr, "pix-host uat-browser-open: missing required arguments\n")
		os.Exit(2)
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
		// Log the error but don't print secrets
		fmt.Fprintf(os.Stderr, "pix-host uat-browser-open: capture failed\n")
		os.Exit(1)
	}
}
