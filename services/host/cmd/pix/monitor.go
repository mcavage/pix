package main

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"pix/host/cli"
	"pix/host/config"
	"pix/host/monitor"
)

// runMonitor is the `monitor` verb: a host-side debug wiretap. With no
// --path it binds monitor.NewIngestServer on loopback (:11437 by default —
// see monitor.DefaultPort) and appends incoming events to the on-disk
// store while concisely printing them as they arrive; Ctrl-C stops both.
// With --path DIR it is a pure offline reader over an already-captured
// directory: no network listener at all. There is no bubbletea TUI here
// (Story05 replaced it with this concise line reader) and no "serviceDown"
// degrade path — monitor IS the ingest+reader, not a client of one running
// elsewhere.
func runMonitor(argv []string) {
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()
	defer cancel()

	storeRootDefault, err := defaultMonitorStoreDir()
	if err != nil {
		cli.ExitFromErr("monitor", err)
		return
	}
	ttyOut := term.IsTerminal(int(os.Stdout.Fd()))

	if err := runMonitorCore(ctx, argv, monitor.NewIngestServer, os.Stdout, os.Stderr, storeRootDefault, ttyOut); err != nil {
		cli.ExitFromErr("monitor", err)
	}
}

// defaultMonitorStoreDir is <state-dir>/monitor — the same ephemeral-data
// family as serve.log (config.StateDir's doc comment), since a monitor
// capture is bounded/rotated debug scratch, not durable data like memory's
// recall store.
func defaultMonitorStoreDir() (string, error) {
	dir, err := config.StateDir()
	if err != nil {
		return "", fmt.Errorf("monitor: resolve state dir: %w", err)
	}
	return filepath.Join(dir, "monitor"), nil
}

const monitorUsage = `usage: pix monitor [name] [--port N] [--bind ADDR] [--path DIR] [--json]

  Record and concisely follow a running sandbox's out-of-sandbox traffic
  (model requests, responses, tool + MCP calls, context/control events).

  With no --path: binds :11437 (loopback-only) and appends incoming events
  to the on-disk store (` + "`<state-dir>/monitor`" + `) while printing them as they
  arrive, one concise line per event. Ctrl-C stops both the listener and
  the reader.

  With --path DIR: a pure OFFLINE reader over an already-captured (or being
  concurrently written) directory — no network listener is started at all.
  Useful to review a previous capture, or to point at a fixture directory.

  [name]        filter to one sandbox/session by name/id substring (default: all).
                CASE-SENSITIVE substring match.
  --port N      ingest port (default 11437). Ignored with --path.
  --bind ADDR   listen address (default 127.0.0.1, loopback-only). Ignored
                with --path. Linux users watching a real sandbox may need
                --bind 0.0.0.0, since host.docker.internal maps to the
                bridge gateway there rather than to loopback (unlike Docker
                Desktop on macOS/Windows). A non-loopback bind exposes the
                monitor ingest endpoint — no auth, full agent context and
                tool output — to your local network.
  --path DIR    read (and, if something else is writing to it, keep
                following) an existing store directory instead of starting
                a listener.
  --json        print the raw (redacted, capped) stored event JSON instead
                of the concise formatted line — one JSON object per line,
                safe to pipe into jq.

NOTE: live events only flow from a sandbox created with an image that
includes the monitor extension + the :11437 network allowlist entry (baked
via ` + "`make load`" + ` on the host). A stale sandbox predates that and shows no
events — rebuild/reload the image, then recreate the sandbox.

ENV (read by the in-VM extension, documented here for discoverability):
  PIX_MONITOR=0        disable the in-VM tap entirely (no events sent)
  PIX_MONITOR_URL      override the host ingest URL
                            (default http://host.docker.internal:11437)
`

// hubBindTimeout bounds how long runMonitorCore waits for the ingest server
// to either bind its listener (Addr() becomes non-empty) or fail fast
// (e.g. the port is already in use) before giving up.
const hubBindTimeout = 2 * time.Second

// pollInterval is how often the concise reader re-checks the store for new
// events in live and --path modes alike.
const pollInterval = 150 * time.Millisecond

// newIngestServerFunc matches monitor.NewIngestServer's signature; injected
// so monitor_test.go can prove flag parsing / help / dispatch without a
// real bound port.
type newIngestServerFunc func(monitor.IngestConfig) (*monitor.IngestServer, error)

// runMonitorCore is the testable core of runMonitor. ctx governs the whole
// run (both the ingest listener, if live, and the reader poll loop) so a
// test can cancel it deterministically instead of relying on a real
// signal. newIngestServer is injected for the same reason monitor_test.go
// always injected the hub constructor.
func runMonitorCore(ctx context.Context, argv []string, newIngestServer newIngestServerFunc, out, errOut io.Writer, storeRootDefault string, ttyOut bool) error {
	fs := cli.NewFlagSet()
	fs.EnableJSON()
	port := fs.Int("port", monitor.DefaultPort)
	bind := fs.Str("bind", "")
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

	live := *path == ""
	root := *path
	if root == "" {
		root = storeRootDefault
	}

	store, err := monitor.NewStore(monitor.StoreConfig{Root: root})
	if err != nil {
		return err
	}

	if live {
		blobs, err := monitor.NewBlobStore(monitor.BlobStoreConfig{Root: filepath.Join(root, "blobs")})
		if err != nil {
			return err
		}
		bindAddr := *bind
		if bindAddr == "" {
			bindAddr = monitor.DefaultBindAddr
		}
		if !isLoopbackAddr(bindAddr) {
			fmt.Fprintf(errOut, "WARNING: monitor ingest is bound to %s, exposed on your local network with NO AUTHENTICATION — anyone on the network can send this sandbox's full agent context and tool output into the store. Use a firewall, or bind loopback (drop --bind) unless you specifically need this.\n", bindAddr)
		}

		srv, err := newIngestServer(monitor.IngestConfig{Port: *port, BindAddr: bindAddr, Store: store, Blobs: blobs, Filter: name})
		if err != nil {
			return err
		}
		srvCtx, srvCancel := context.WithCancel(context.Background())
		startErr := make(chan error, 1)
		go func() { startErr <- srv.Start(srvCtx) }()
		if err := waitForIngestBind(srv, startErr); err != nil {
			srvCancel()
			return err
		}
		fmt.Fprintf(errOut, "monitor: listening on %s, storing under %s\n", srv.Addr(), root)

		readerDone := make(chan struct{})
		go func() {
			runConciseReader(ctx, store, name, fs.Json, ttyOut, out)
			close(readerDone)
		}()

		<-ctx.Done()
		srvCancel()
		<-startErr
		<-readerDone
		return nil
	}

	runConciseReader(ctx, store, name, fs.Json, ttyOut, out)
	return nil
}

// waitForIngestBind polls srv.Addr() until the listener has bound, or
// returns early with the bind error if Start fails fast (e.g. "port
// already in use"). It never blocks past hubBindTimeout.
func waitForIngestBind(srv *monitor.IngestServer, startErr <-chan error) error {
	deadline := time.Now().Add(hubBindTimeout)
	for {
		select {
		case err := <-startErr:
			return err
		default:
		}
		if srv.Addr() != "" {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("monitor: ingest server did not bind within %s", hubBindTimeout)
		}
		time.Sleep(time.Millisecond)
	}
}

// isLoopbackAddr reports whether host (a bare hostname/IP, no port) refers
// to the loopback interface.
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

// runConciseReader polls store for new events (across every stream that
// matches filter) until ctx is done, printing each as either the concise
// human line (asJSON=false) or the raw stored event JSON (asJSON=true).
// ttyOut adds minimal ANSI emphasis to the concise form; both forms are
// otherwise identical whether stdout is a terminal or piped, so a redirect
// never silently changes WHAT is printed, only its styling.
func runConciseReader(ctx context.Context, store *monitor.Store, filter string, asJSON, ttyOut bool, out io.Writer) {
	printed := map[string]int{} // streamKey -> events already printed for that stream
	poll := func() {
		metas, err := store.List()
		if err != nil {
			return
		}
		for _, m := range metas {
			if filter != "" && !strings.Contains(m.SandboxID, filter) && !strings.Contains(m.SessionID, filter) {
				continue
			}
			key := m.SandboxID + "/" + m.SessionID
			events, err := store.Tail(m.SandboxID, m.SessionID, 0)
			if err != nil {
				continue
			}
			already := printed[key]
			if len(events) <= already {
				continue
			}
			for _, e := range events[already:] {
				printEvent(out, e, asJSON, ttyOut)
			}
			printed[key] = len(events)
		}
	}

	poll() // pick up whatever's already there before the first sleep
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollInterval):
			poll()
		}
	}
}

// printEvent writes one event to out, either as its raw stored JSON
// (asJSON) or as a concise one-line summary.
func printEvent(out io.Writer, e monitor.Event, asJSON, ttyOut bool) {
	if asJSON {
		line, err := monitor.Encode(e)
		if err != nil {
			return
		}
		fmt.Fprintln(out, string(line))
		return
	}
	fmt.Fprintln(out, formatConcise(e, ttyOut))
}

// formatConcise renders one event as a single readable line. ttyOut wraps
// the kind token in ANSI bold when true; the CONTENT is identical either
// way (no information is TTY-gated, only its styling), so a piped run and
// an interactive run of the same capture are diffable against each other.
func formatConcise(e monitor.Event, ttyOut bool) string {
	env := e.Envelope()
	ts := time.UnixMilli(env.TS).UTC().Format("15:04:05")
	label := env.SandboxID
	if env.SessionID != "" {
		label += "/" + env.SessionID
	}

	kind := ""
	detail := ""
	switch v := e.(type) {
	case monitor.TurnStart:
		kind = "turn_start"
		detail = fmt.Sprintf("model=%s trigger=%s", v.Model, v.Trigger)
	case monitor.ProviderRequest:
		kind = "request"
		detail = fmt.Sprintf("model=%s msgs=+%d tools=%d ~%dtok", v.Model, len(v.Summary.NewMessages), v.Summary.ToolCount, v.Summary.EstTokens)
	case monitor.ProviderResponse:
		kind = "response"
		usage := ""
		if v.Usage != nil {
			usage = fmt.Sprintf(" in=%d out=%d", v.Usage.InputTokens, v.Usage.OutputTokens)
		}
		detail = fmt.Sprintf("status=%d stop=%s%s", v.Status, v.StopReason, usage)
	case monitor.ToolStart:
		kind = "tool"
		detail = fmt.Sprintf("%s (%s) %s", v.Name, v.Source, v.ArgsSummary)
	case monitor.ToolEnd:
		kind = "tool_end"
		okStr := "ok"
		if !v.OK {
			okStr = "FAIL"
		}
		detail = fmt.Sprintf("%s %dB %dms", okStr, v.ResultBytes, v.DurationMs)
	case monitor.ContextEvent:
		kind = "ctx"
		detail = fmt.Sprintf("%s %s", v.CtxKind, v.Detail)
	case monitor.UnknownEvent:
		kind = string(v.Kind())
		detail = "(unrecognized kind)"
	default:
		kind = string(e.Kind())
	}

	if ttyOut {
		kind = "\x1b[1m" + kind + "\x1b[0m"
	}
	return fmt.Sprintf("%s %s %s %s", ts, label, kind, detail)
}
