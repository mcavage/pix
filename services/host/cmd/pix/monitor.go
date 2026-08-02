package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"pix/host/monitor"
	"pix/host/monitor/tui"
)

// runMonitor is the `monitor` verb: a host-side live wiretap of a running
// sandbox's out-of-sandbox traffic (model requests/responses, tool + MCP
// calls). It binds the monitor.Hub (:11437 by default) and renders a
// bubbletea TUI (Unit B) until the user quits. Unlike `memory`/`knowledge`,
// there is no "serviceDown" degrade path — monitor IS the service, so a bind
// failure is just a generic error (exit 1).
func runMonitor(argv []string) {
	if err := runMonitorCore(argv, monitor.NewHub, tui.RunTUI, os.Stdout, os.Stderr); err != nil {
		exitFromErr("monitor", err)
	}
}

const monitorUsage = `usage: pix monitor [name] [--port N] [--bind ADDR]

  Live-follow a running sandbox's out-of-sandbox traffic (model requests,
  responses, tool + MCP calls). Binds :11437 and renders a TUI until you quit.

  [name]        filter to one sandbox by name/id substring (default: all).
                CASE-SENSITIVE substring match on sandbox name/id.
  --port N      hub port (default 11437)
  --bind ADDR   listen address (default 127.0.0.1, loopback-only). Linux users
                watching a real sandbox may need --bind 0.0.0.0, since
                host.docker.internal maps to the bridge gateway there rather
                than to loopback (unlike Docker Desktop on macOS/Windows). A
                non-loopback bind exposes the monitor hub — no auth, full
                agent context and tool output — to your local network.

NOTE: events only flow from a sandbox created with an image that includes the
monitor extension + the :11437 network allowlist entry (baked via ` + "`make load`" + ` on
the host). A stale sandbox predates that and shows no events — rebuild/reload
the image, then recreate the sandbox.

ENV (read by the in-VM extension, documented here for discoverability):
  PIX_MONITOR=0        disable the in-VM tap entirely (no events sent)
  PIX_MONITOR_URL      override the host hub URL
                            (default http://host.docker.internal:11437)
`

// hubBindTimeout bounds how long runMonitorCore waits for the hub to either
// bind its listener (Addr() becomes non-empty) or fail fast (e.g. the port is
// already in use) before giving up.
const hubBindTimeout = 2 * time.Second

// runMonitorCore is the testable core of runMonitor. newHub and runTUI are
// injected so monitor_test.go can prove flag parsing / help / dispatch
// without a real terminal or a bound port: a fake newHub can capture the
// HubConfig it was called with (and hand back a Hub bound to an ephemeral
// port instead of a real one), and a fake runTUI can return immediately
// without spinning bubbletea.
func runMonitorCore(argv []string, newHub func(monitor.HubConfig) *monitor.Hub, runTUI func(tui.TUIConfig) error, out, errOut io.Writer) error {
	fs := newFlagSet()
	port := fs.int("port", monitor.DefaultPort)
	bind := fs.str("bind", "")
	positional, err := fs.parse(argv)
	if err != nil {
		return err
	}
	if fs.help {
		fmt.Fprint(out, monitorUsage)
		return nil
	}
	if len(positional) > 1 {
		return usageErr(monitorUsage)
	}
	name := ""
	if len(positional) == 1 {
		name = positional[0]
	}

	bindAddr := *bind
	if bindAddr == "" {
		bindAddr = monitor.DefaultBindAddr
	}
	if !isLoopbackAddr(bindAddr) {
		fmt.Fprintf(errOut, "WARNING: monitor hub is bound to %s, exposed on your local network with NO AUTHENTICATION — anyone on the network can read this sandbox's full agent context and tool output. Use a firewall, or bind loopback (drop --bind) unless you specifically need this.\n", bindAddr)
	}

	hub := newHub(monitor.HubConfig{Port: *port, BindAddr: bindAddr, Filter: name})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startErr := make(chan error, 1)
	go func() { startErr <- hub.Start(ctx) }()

	if err := waitForHubBind(hub, startErr); err != nil {
		return err
	}

	sub, unsubscribe := hub.Subscribe()
	defer unsubscribe()

	tuiErr := runTUI(tui.TUIConfig{Events: sub, Blob: hub.Blob, Filter: name, Port: *port})

	// Cancel and wait for Start to actually finish shutting the listener down
	// before returning, so the port is released deterministically (avoids the
	// EADDRINUSE-on-restart trap noted in architecture.md Section 5) rather
	// than leaving that race to happen after runMonitor has already exited.
	cancel()
	<-startErr

	return tuiErr
}

// isLoopbackAddr reports whether host (a bare hostname/IP, no port) refers
// to the loopback interface — used to decide whether --bind needs the loud
// non-loopback warning above.
func isLoopbackAddr(host string) bool {
	switch host {
	case "", "localhost":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// waitForHubBind polls hub.Addr() until the listener has bound, or returns
// early with the bind error if Start fails fast (e.g. "port already in use").
// It never blocks past hubBindTimeout.
func waitForHubBind(hub *monitor.Hub, startErr <-chan error) error {
	deadline := time.Now().Add(hubBindTimeout)
	for {
		select {
		case err := <-startErr:
			return err
		default:
		}
		if hub.Addr() != "" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("monitor: hub did not bind within %s", hubBindTimeout)
		}
		time.Sleep(time.Millisecond)
	}
}
