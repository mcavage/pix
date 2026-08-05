// serve_reload.go implements CONFIG PROPAGATION (docs/design/serve-lifecycle.md
// §3): after `pix config set/unset` writes a DAEMON-AFFECTING key, the
// running `pix-host serve` is restarted per its lifecycle mode so the

package service

import (
	"fmt"
	"io"

	"pix/host/config"
)

// daemonAffectingKeys are the config keys the daemon reads at startup and never
// re-reads: changing one requires a serve restart to take effect. Everything
// else (gog_account, mcp, ollama_bridge_model, host.*, pack) is read by the
var daemonAffectingKeys = map[string]bool{
	"services":             true,
	"memory_watcher_model": true,
	"memory_embed_model":   true,
}

// IsDaemonAffecting reports whether a config key change requires a serve
// restart.
func IsDaemonAffecting(key string) bool { return daemonAffectingKeys[key] }

// serveMode is the detected lifecycle mode of the running (or not) daemon.
type serveMode int

const (
	serveDown serveMode = iota
	serveForeground
	serveLazy
	serveManaged
)

// serveReloader bundles the injectable ops config-propagation needs: detect the
// active lifecycle mode, kickstart a managed unit, stop-and-relazy a lazy daemon.
type serveReloader struct {
	mode        func() serveMode              // detectServeMode
	kickManaged func() error                  // launchctl kickstart -k / systemctl --user restart
	Stop        func(io.Writer) (bool, error) // REUSE Stop(DefaultCtl(), out)
	ensure      func() error                  // Ensure with the config set (re-lazy-start)
}

// DefaultReloader wires the real ops (platform managed-service calls live
// behind build tags in serve_install_*.go).
func DefaultReloader() serveReloader {
	ctl := DefaultCtl()
	return serveReloader{
		mode: func() serveMode {
			return detectServeMode(ctl, ManagedActive, readServeLazyMarkerPid)
		},
		kickManaged: restartManagedService,
		Stop:        func(out io.Writer) (bool, error) { return Stop(DefaultCtl(), out) },
		ensure: func() error {
			cfg, err := config.Load()
			if err != nil {
				return err
			}
			return Ensure(DefaultStarter(), cfg, EnsureOpts{})
		},
	}
}

// detectServeMode resolves which lifecycle mode the daemon is in. Managed is
// checked FIRST and is authoritative: a managed service also writes the pidfile
// (it runs the same `pix-host serve`), so the pidfile alone cannot tell
func detectServeMode(ctl serveCtl, managedActive func() bool, lazyPid func() (int, bool)) serveMode {
	if managedActive() {
		return serveManaged
	}
	if pid, ok := readLiveServePid(ctl); ok {
		if mpid, mok := lazyPid(); mok && mpid == pid {
			return serveLazy
		}
		return serveForeground
	}
	return serveDown
}

// PropagateConfig restarts (or advises about) the running daemon after a
// daemon-affecting config write. Best-effort on every branch: the config file
// is already saved and is the source of truth — a failed restart prints a
func PropagateConfig(rl serveReloader, out io.Writer) {
	switch rl.mode() {
	case serveManaged:
		if err := rl.kickManaged(); err != nil {
			fmt.Fprintf(out, "warning: could not restart the managed pix service (%v) — restart it manually to apply the change.\n", err)
			return
		}
		fmt.Fprintln(out, "restarted managed pix services to apply the change.")
	case serveLazy:
		stopped, err := rl.Stop(io.Discard)
		if err != nil {
			fmt.Fprintf(out, "warning: could not stop the background pix services (%v) — restart them manually to apply the change.\n", err)
			return
		}
		if !stopped {
			// Stop refused (stale/hijacked/unverifiable pid) or found nothing:
			// re-spawning NOW could double-start against a still-live daemon (M4).
			fmt.Fprintln(out, "warning: the background pix services were not stopped — run `pix serve stop` then restart to apply the change.")
			return
		}
		if err := rl.ensure(); err != nil {
			fmt.Fprintf(out, "warning: pix services were stopped but did not restart (%v) — run `pix serve` or retry.\n", err)
			return
		}
		fmt.Fprintln(out, "restarted pix services (background) to apply the change.")
	case serveForeground:
		// Never kill a process the user is watching in their own terminal.
		fmt.Fprintln(out, "note: a foreground `pix serve` is running — restart it (Ctrl-C, re-run) to apply this change.")
	default: // serveDown
		fmt.Fprintln(out, "note: the change applies next time pix services start.")
	}
}
