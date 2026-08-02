// serve_reload.go implements CONFIG PROPAGATION (docs/design/serve-lifecycle.md
// §3): after `pix config set/unset` writes a DAEMON-AFFECTING key, the
// running `pix-host serve` is restarted per its lifecycle mode so the
// change takes effect with no manual step. The daemon reads services /
// memory_*_model / knowledge_bundles at startup only, never live.
//
// Everything OS-shaped (launchctl/systemctl query + restart, stop, re-spawn) is
// injected via serveReloader so every mode routes are unit-tested with no real
// process.

package service

import (
	"fmt"
	"io"

	"pix/host/config"
)

// daemonAffectingKeys are the config keys the daemon reads at startup and never
// re-reads: changing one requires a serve restart to take effect. Everything
// else (gog_account, mcp, ollama_bridge_model, host.*, pack) is read by the
// launcher or the gateway, NOT by serve — and must trigger NOTHING.
var daemonAffectingKeys = map[string]bool{
	"services":             true,
	"memory_watcher_model": true,
	"memory_embed_model":   true,
	"knowledge_bundles":    true,
}

// IsDaemonAffecting reports whether a config key change requires a serve
// restart. knowledge_bundles is included because serve indexes it at startup
// via AllKnowledgeBundles (now just the single deduped list; profiles, which
// it used to union across, were removed).
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
// managed from foreground. A pidfile that is live + verified-ours is lazy ONLY
// when the serve.lazy marker exists AND carries that same pid (H4): a marker
// left behind by a lazy spawn that crashed before its pidfile landed must not
// misclassify a LATER foreground daemon as lazy — config propagation would
// stop+restart a process the user is watching. A mismatched/legacy/absent
// marker means foreground; everything without a live pid is down (self-heals).
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
// warning and the user restarts manually.
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
