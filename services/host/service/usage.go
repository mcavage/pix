package service

// Usage is the `pix serve` help text. It lives with the capability rather than
// in a central help file, so a change to what serve does and a change to what
// serve says are the same edit.
const Usage = `usage: pix serve [args...]
       pix serve start   (alias: install)
       pix serve stop
       pix serve status [--json]
       pix serve install
       pix serve uninstall

Run the long-running host services (execs the sibling pix-host serve):
memory (:11435) and knowledge (:11436, when enabled). Any args are passed
through to pix-host serve unchanged.

You usually do NOT need to run this yourself: pix run / memory /
knowledge query auto-start a detached serve when its ports are down (lazy
auto-start; logs in ~/.local/state/pix/serve.log). Opt out with
PIX_NO_AUTOSERVE=1 or 'pix config set host.autoserve false'.

subcommands:
  stop              stop a running 'pix-host serve' (safe: verifies the
                    process is ours before signalling; SIGTERM then SIGKILL if
                    it doesn't exit). Mode-aware: a MANAGED service (launchd/
                    systemd) is stopped via its supervisor so KeepAlive/Restart=
                    can't respawn it; if the pidfile is missing it falls back to
                    discovering a verified 'pix-host serve' (e.g. an orphan
                    left after 'pix reset' moved the config dir).
  status [--json]   report whether serve is running (pid) and which service
                    ports (:11435 / :11436) are up
  start             alias for 'install'; (re)start the managed service, picking
                    up a freshly-rebuilt binary. The partner to 'stop'.
  install           install serve as a managed login service (launchd on macOS,
                    systemd --user on Linux): starts at login, auto-restarts.
                    stops a lazily-started daemon first; refuses over a
                    foreground serve. captures install-time env into the unit
                    (PIX_CONFIG always; XDG_CONFIG_HOME, MEMORY_DB,
                    MEMORY_PORT, KNOWLEDGE_PORT, OLLAMA_HOST when set) and
                    verifies the service came up.
                    logs: ~/.local/state/pix/serve.log (same file the
                    lazy auto-start uses, on both macOS and Linux)
  uninstall         remove the managed login service
`
