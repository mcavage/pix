package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/monitor"
)

// runMonitor is the `monitor` verb: a PURE OFFLINE READER over the on-disk
// event store. It never binds a port and never starts a listener — the
// ingest listener now lives inside `pix-host serve` (services/host/serve.go),
// composed alongside memory. With no --path it tails the SAME root serve
// writes to (config.MonitorStoreRoot, <state-dir>/monitor); with --path DIR
// it reads an arbitrary (possibly still being written) store directory
// instead. Either way this is `monitor.Follow` over the filesystem: reader
// and writer share nothing else, so `pix monitor` works whether or not serve
// is running right now (it just prints nothing new until it is).
func runMonitor(argv []string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigCh; cancel() }()

	defaultRoot, err := config.MonitorStoreRoot()
	if err != nil {
		cli.ExitFromErr("monitor", fmt.Errorf("resolve monitor store root: %w", err))
		return
	}
	tty := term.IsTerminal(int(os.Stdout.Fd()))
	if err := runMonitorCore(ctx, argv, os.Stdout, defaultRoot, tty); err != nil {
		cli.ExitFromErr("monitor", err)
	}
}

const monitorUsage = `usage: pix monitor [name] [--path DIR] [--json]

  Concisely follow a sandbox's out-of-sandbox traffic (model requests,
  responses, tool + MCP calls, context/control events), as captured on disk.

  This is a PURE READER: it never binds a port or starts a listener. The
  ingest listener that receives events from the in-VM tap runs inside
  ` + "`pix serve`" + ` (:11437, loopback-only by default — see ` + "`pix serve --help`" + `
  for its --bind/--port flags). With no --path, monitor tails the same store
  root serve writes to; run ` + "`pix serve`" + ` first (or already have it
  running) for there to be anything to follow.

  [name]        filter to one sandbox/session by id substring, CASE-SENSITIVE.
  --path DIR    read an existing store directory instead of the default
                (<state-dir>/monitor) — e.g. a capture copied off another
                host, or one you're pointing serve at explicitly.
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

// runMonitorCore is the testable core: ctx governs the run so a test cancels
// deterministically instead of signaling. There is no listener to inject —
// monitor is argv parsing plus a Follow loop over the store.
func runMonitorCore(ctx context.Context, argv []string, out io.Writer, defaultRoot string, tty bool) error {
	fs := cli.NewFlagSet()
	fs.EnableJSON()
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
		return cli.UsageErr("usage: pix monitor [name] [--path DIR] [--json]")
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
	monitor.Follow(ctx, store, follow)
	return nil
}
