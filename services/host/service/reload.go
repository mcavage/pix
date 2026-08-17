// reload.go implements CONFIG PROPAGATION (docs/design/serve-lifecycle.md §3):
// after `pix config set/unset` writes a DAEMON-AFFECTING key, the running
// `pix-host serve` is restarted PER ITS LIFECYCLE MODE so the change takes
// effect — and so no mode is ever stopped the wrong way.

package service

import (
	"fmt"
	"io"

	"pix/host/config"
)

// daemonAffectingKeys are the config keys the daemon reads at startup and never
// re-reads: changing one requires a serve restart to take effect. Everything
// else is read per-invocation by the launcher, so it needs no restart.
var daemonAffectingKeys = map[string]bool{
	"services":             true,
	"memory_watcher_model": true,
	"memory_embed_model":   true,
	"memory_capture":       true,
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

// serveReloader bundles the ops config propagation needs: detect the mode,
// kickstart a managed unit, stop-and-relazy a lazy one.
type serveReloader struct {
	mode        func() serveMode              // detectServeMode
	kickManaged func() error                  // launchctl kickstart -k / systemctl --user restart
	Stop        func(io.Writer) (bool, error) // REUSE Stop(DefaultCtl(), out)
	ensure      func() error                  // Ensure with the config set (re-lazy-start)
}

// DefaultReloader wires the real ops (platform managed-service calls live behind
// build tags in serve_install_*.go). progress carries the ensure's re-lazy-start
// chatter, for the reason DefaultStarter takes one.
func DefaultReloader(progress io.Writer) serveReloader {
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
			return Ensure(DefaultStarter(progress), cfg, EnsureOpts{})
		},
	}
}

// detectServeMode resolves which lifecycle mode the daemon is in. Managed is
// checked FIRST and is authoritative: a managed service also writes the pidfile
// (it runs the same `pix-host serve`), so the pidfile alone cannot tell managed
// from lazy — and the lazy marker must MATCH the live pid, so a stale marker
// from a crash can never make a foreground daemon look lazy.
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

// serveRelazyResult is why a stop-then-lazy-start sequence ended: at most one
// field is set. Config propagation is the one caller that recycles a
// background daemon this way (U3-lifecycle: the read-side version-restart that
// used to share this sequence was deleted — see EnsureUp in start.go) — it only
// needs to word the report once, but the notStopped case still must not be
// forgotten, where re-spawning would double-start against a still-live daemon
// (M4).
type serveRelazyResult struct {
	stopErr    error // Stop failed
	notStopped bool  // Stop refused an unverified pid, or found nothing to stop
	startErr   error // stopped, but the fresh daemon did not come up
}

// relazyServe stops a lazy (or unrecorded-orphan) daemon SAFELY and lazy-starts
// a fresh one.
func relazyServe(rl serveReloader) serveRelazyResult {
	stopped, err := rl.Stop(io.Discard)
	switch {
	case err != nil:
		return serveRelazyResult{stopErr: err}
	case !stopped:
		return serveRelazyResult{notStopped: true}
	}
	if err := rl.ensure(); err != nil {
		return serveRelazyResult{startErr: err}
	}
	return serveRelazyResult{}
}

// PropagateConfig restarts (or advises about) the running daemon after a
// daemon-affecting config write. Best-effort on every branch: the config file is
// already saved and is the source of truth, so a failed restart prints a warning
// with the manual step rather than failing the write.
func PropagateConfig(rl serveReloader, out io.Writer) {
	switch rl.mode() {
	case serveManaged:
		// SAY IT FIRST. kickManaged kills the daemon and waits for it to die,
		// which is up to its whole budget of silence at the end of a command
		// that has otherwise been narrating every step — and a user watching
		// `pix setup` stop dead after "note: mcp registration…" reasonably reads
		// that as a hang and Ctrl-Cs a run that was about to finish. Someone
		// did.
		fmt.Fprintln(out, "restarting pix services to apply the change…")
		if err := rl.kickManaged(); err != nil {
			fmt.Fprintf(out, "warning: could not restart the managed pix service (%v) — restart it manually to apply the change.\n", err)
			return
		}
		fmt.Fprintln(out, "restarted managed pix services to apply the change.")
	case serveLazy:
		switch r := relazyServe(rl); {
		case r.stopErr != nil:
			fmt.Fprintf(out, "warning: could not stop the background pix services (%v) — restart them manually to apply the change.\n", r.stopErr)
		case r.notStopped:
			fmt.Fprintln(out, "warning: the background pix services were not stopped — run `pix serve stop` then restart to apply the change.")
		case r.startErr != nil:
			fmt.Fprintf(out, "warning: pix services were stopped but did not restart (%v) — run `pix serve` or retry.\n", r.startErr)
		default:
			fmt.Fprintln(out, "restarted pix services (background) to apply the change.")
		}
	case serveForeground:
		// Never kill a process the user is watching in their own terminal.
		fmt.Fprintln(out, "note: a foreground `pix serve` is running — restart it (Ctrl-C, re-run) to apply this change.")
	default: // serveDown
		fmt.Fprintln(out, "note: the change applies next time pix services start.")
	}
}
