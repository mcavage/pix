package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"

	hostuat "pix/host/uat"
	wuat "pix/host/workflow/uat"
)

// connServer is the minimal shape uatWorkerLoop needs from a per-connection
// handler. workflow/uat.MCPServer satisfies it; tests substitute a trivial
// fake so the accept/serialize/survive-EOF loop is provable without
// constructing a real Runner.
type connServer interface {
	Serve(ctx context.Context) error
}

// runUatWorker is `pix-host uat-worker`: the process that actually owns the
// UAT Runner and executes host commands (docker, sbx, git, the browser). It
// is started later by `pix run --dev`, never by the sbx gateway, so it
// inherits the operator's authenticated host context instead of the
// gateway's own unauthenticated ancestry (docs/design/self-development-uat.md).
// It listens on a session-owned Unix socket that the gateway-spawned
// `pix-host uat-mcp` relays stdio to/from.
func runUatWorker(args []string) error {
	fs := flag.NewFlagSet("uat-worker", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	repo := fs.String("repo", "", "absolute path to the repo")
	state := fs.String("state", "", "absolute path to the session's UAT runner state directory")
	session := fs.String("session", "", "strict session identifier")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if *repo == "" || !filepath.IsAbs(*repo) {
		return fmt.Errorf("uat-worker: --repo is required and must be absolute")
	}
	if *state == "" || !filepath.IsAbs(*state) {
		return fmt.Errorf("uat-worker: --state is required and must be absolute")
	}
	if *session == "" {
		return fmt.Errorf("uat-worker: --session is required")
	}
	if err := hostuat.ValidateID(*session); err != nil {
		return fmt.Errorf("uat-worker: invalid session: %w", err)
	}
	if st, err := os.Stat(*repo); err != nil || !st.IsDir() {
		return fmt.Errorf("uat-worker: repo must be an existing directory")
	}
	if st, err := os.Stat(*state); err != nil || !st.IsDir() {
		return fmt.Errorf("uat-worker: state must be an existing directory")
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("uat-worker: unknown arguments: %v", fs.Args())
	}

	components, err := buildUatWorkerComponents(*repo, *state)
	if err != nil {
		return err
	}

	socketPath := hostuat.SessionSocketPath(*state)
	listener, err := hostuat.ListenSocket(socketPath)
	if err != nil {
		return fmt.Errorf("uat-worker: %w", err)
	}
	defer listener.Close()

	newServer := func(in io.Reader, out io.Writer) connServer {
		return wuat.NewMCPServer(components.runner, components.browserFactory, *state, in, out, components.retryReport)
	}

	return uatWorkerLoop(context.Background(), listener, newServer)
}

// uatWorkerLoop accepts UAT MCP client connections one at a time — single-
// flight, never more than one Serve call in progress — builds a fresh
// per-connection server that shares the one long-lived Runner passed in via
// newServer's closure, and always loops back to Accept once Serve returns.
// That includes a client EOF: MCPServer.Serve returns cleanly when its input
// reader is exhausted, which ends only that connection, never the worker
// process. A gateway relay disconnecting and a later one reconnecting (a new
// pi session in the same sandbox, or the gateway simply restarting) must never
// tear down the Runner that holds the operator's authenticated host context —
// that persistence is the entire reason this process exists.
func uatWorkerLoop(ctx context.Context, l net.Listener, newServer func(in io.Reader, out io.Writer) connServer) error {
	for {
		conn, err := l.Accept()
		if err != nil {
			return fmt.Errorf("uat-worker: accept: %w", err)
		}
		srv := newServer(conn, conn)
		if err := srv.Serve(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "uat-worker: connection error: %v\n", err)
		}
		_ = conn.Close()
	}
}
