package main

import (
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"time"

	hostuat "pix/host/uat"
)

// uatConnectAttempts/uatConnectDelay bound how long the gateway relay waits
// for pix-host uat-worker's socket to appear. The worker is a separate
// process `pix run --dev` starts once the sandbox's session directory exists
// (docs/design/self-development-uat.md); a short race with sandbox/session
// startup is expected, an absent worker after this bound is not.
const (
	uatConnectAttempts = 15
	uatConnectDelay    = 200 * time.Millisecond
)

// runUatMcp is `pix-host uat-mcp`, the process the sbx gateway spawns per MCP
// client connection. It is a DUMB stdio<->Unix-socket relay and nothing else:
// it must never construct a UAT Runner or execute a host command, because
// gateway-spawned ancestry does not carry the operator's authenticated
// sbx/docker/browser context (docs/design/self-development-uat.md). That
// authority lives entirely in `pix-host uat-worker`, started later by
// `pix run --dev` so it inherits that context. TestUatMcpGatewayIsADumbRelay
// pins that this file can never import os/exec or pix/host/workflow/uat
// again.
func runUatMcp(args []string) error {
	fs := flag.NewFlagSet("uat-mcp", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	connect := fs.String("connect", "", "absolute path to the session's UAT worker Unix socket")

	if err := fs.Parse(args); err != nil {
		return err
	}
	if *connect == "" || !filepath.IsAbs(*connect) {
		return fmt.Errorf("uat-mcp: --connect is required and must be an absolute Unix socket path")
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("uat-mcp: unknown arguments: %v", fs.Args())
	}

	conn, err := hostuat.DialSocket(*connect, uatConnectAttempts, uatConnectDelay)
	if err != nil {
		return fmt.Errorf("uat-mcp: %w", err)
	}
	defer conn.Close()

	return relayBytes(os.Stdin, os.Stdout, conn)
}

// relayBytes copies bytes bidirectionally between the local stdio pair (in,
// out — how sbx's gateway wires this process to the MCP client) and conn (the
// connected uat-worker socket), stopping once either direction reaches EOF or
// errors. It has zero knowledge of the MCP protocol carried inside: a pure
// byte relay is the entire point of this process.
func relayBytes(in io.Reader, out io.Writer, conn io.ReadWriteCloser) error {
	done := make(chan error, 2)
	go func() {
		_, err := io.Copy(conn, in)
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- err
	}()
	go func() {
		_, err := io.Copy(out, conn)
		done <- err
	}()
	first := <-done
	_ = conn.Close()
	<-done
	return first
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
