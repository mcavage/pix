package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"golang.org/x/term"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/monitor"
)

// runMonitor is the `monitor` verb: a host-side debug wiretap. With no
// --path it binds a loopback ingest listener (:11437 by default) and appends
// incoming events to the on-disk store while printing each as it arrives;
// with --path DIR it is a pure offline reader over an already-captured
// directory and starts no listener at all. There is no TUI and no
// "serviceDown" degrade path — monitor IS the service.
func runMonitor(argv []string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigCh; cancel() }()

	// <state-dir>/monitor: a capture is bounded, rotated debug scratch, the
	// same ephemeral-data family as serve.log, not durable data.
	stateDir, err := config.StateDir()
	if err != nil {
		cli.ExitFromErr("monitor", fmt.Errorf("resolve state dir: %w", err))
		return
	}
	defaultRoot := filepath.Join(stateDir, "monitor")
	tty := term.IsTerminal(int(os.Stdout.Fd()))
	if err := runMonitorCore(ctx, argv, monitor.NewIngestServer, os.Stdout, os.Stderr, defaultRoot, tty); err != nil {
		cli.ExitFromErr("monitor", err)
	}
}

const monitorUsage = `usage: pix monitor [name] [--port N] [--bind ADDR] [--path DIR] [--json]

  Record and concisely follow a running sandbox's out-of-sandbox traffic
  (model requests, responses, tool + MCP calls, context/control events).

  With no --path: binds :11437 (loopback-only), appends events to the store
  under <state-dir>/monitor, and prints each as it arrives, one line per
  event. Ctrl-C stops both the listener and the reader.

  With --path DIR: an OFFLINE reader over an existing (possibly still being
  written) store directory. No listener is started.

  [name]        filter to one sandbox/session by id substring, CASE-SENSITIVE.
  --port N      ingest port (default 11437). Ignored with --path.
  --bind ADDR   listen address (default 127.0.0.1, loopback-only). Ignored
                with --path. Linux users watching a real sandbox may need
                --bind 0.0.0.0, since host.docker.internal maps to the bridge
                gateway there rather than to loopback (unlike Docker Desktop
                on macOS/Windows). A non-loopback bind exposes the ingest
                endpoint — no auth, full agent context and tool output — to
                your local network.
  --path DIR    read an existing store directory instead of listening.
  --json        print the raw (redacted, capped) stored event JSON instead of
                the concise line — one object per line, safe to pipe to jq.

NOTE: live events only flow from a sandbox created with an image that
includes the monitor extension + the :11437 network allowlist entry (baked
via ` + "`make load`" + ` on the host). A stale sandbox predates that and shows no
events — rebuild/reload the image, then recreate the sandbox.

ENV (read by the in-VM extension, documented here for discoverability):
  PIX_MONITOR=0        disable the in-VM tap entirely (no events sent)
  PIX_MONITOR_URL      override the host ingest URL
                            (default http://host.docker.internal:11437)
`

// newIngestServerFunc matches monitor.NewIngestServer; injected so
// monitor_test.go can prove flag parsing and dispatch without a bound port.
type newIngestServerFunc func(monitor.IngestConfig) (*monitor.IngestServer, error)

// runMonitorCore is the testable core: ctx governs the whole run (listener
// and reader alike) so a test cancels deterministically instead of signaling.
func runMonitorCore(ctx context.Context, argv []string, newIngestServer newIngestServerFunc, out, errOut io.Writer, defaultRoot string, tty bool) error {
	fs := cli.NewFlagSet()
	fs.EnableJSON()
	port := fs.Int("port", monitor.DefaultPort)
	bind := fs.Str("bind", monitor.DefaultBindAddr)
	path := fs.Str("path", "")
	positional, err := fs.Parse(argv)
	if err != nil {
		return err
	}
	if fs.Help {
		fmt.Fprint(out, monitorUsage)
		return nil
	}
	if len(positional) > 1 {
		return cli.UsageErr("usage: pix monitor [name] [--port N] [--bind ADDR] [--path DIR] [--json]")
	}
	name := ""
	if len(positional) == 1 {
		name = positional[0]
	}
	root := *path
	if root == "" {
		root = defaultRoot
	}
	store, err := monitor.NewStore(monitor.StoreConfig{Root: root})
	if err != nil {
		return err
	}
	follow := monitor.FollowConfig{Filter: name, JSON: fs.Json, TTY: tty, Out: out}

	if *path != "" { // offline: no listener at all
		monitor.Follow(ctx, store, follow)
		return nil
	}
	if !isLoopbackAddr(*bind) {
		fmt.Fprintf(errOut, "WARNING: monitor ingest is bound to %s, exposed on your local network with NO AUTHENTICATION — anyone on the network can send this sandbox's full agent context and tool output into the store. Use a firewall, or bind loopback (drop --bind) unless you specifically need this.\n", *bind)
	}
	srv, err := newIngestServer(monitor.IngestConfig{Port: *port, BindAddr: *bind, Store: store, Filter: name})
	if err != nil {
		return err
	}
	fmt.Fprintf(errOut, "monitor: listening on %s, storing under %s\n", srv.Addr(), root)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ctx) }()
	monitor.Follow(ctx, store, follow)
	return <-served
}

// isLoopbackAddr reports whether host (no port) is the loopback interface.
func isLoopbackAddr(host string) bool {
	switch host {
	case "", "localhost":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
