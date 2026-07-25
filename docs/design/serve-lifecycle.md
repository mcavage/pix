# Serve lifecycle overhaul

Status: IMPLEMENTED. Scope: three capabilities so users stop babysitting
`pix serve`, matching the Docker/Ollama model.

## Problem

Today `pix serve` is a FOREGROUND supervisor. To use memory (`:11435`) or
knowledge (`:11436`) the user must have a terminal running `serve` somewhere, or
have hand-installed the launchd plist via `scripts/macos/host-setup.sh`. Every
consumer path degrades silently or prints "service unreachable — start it with
`pix serve`" (`main.go` `exitFromErr`, `memory.go:461`). That is babysitting.

Ollama and Docker both solve this two ways at once:

- **Lazy auto-start** (Ollama CLI): the first `ollama run` spins up the server in
  the background if it is not up. No always-on RAM cost until you use it.
- **Managed login service** (Docker Desktop): an opt-in always-on background
  service, started at login, that you never think about.

We want both, plus config changes that take effect without a manual restart.

## What we keep exactly as-is

- `pix serve` (foreground) — the supervisor in `services/host/serve.go`.
  Unchanged. This is still the thing everything else starts.
- `pix serve stop` / `serve status` — `serve_ctl.go`. Reused, not
  rewritten. `verifyServeProc` (pidfile ownership check), `stopServe`,
  `resolveServeStatus`, the injectable `serveCtl` struct: all reused.
- The pidfile contract: `pix-host serve` writes `config.ServePidPath()`
  (`<config-dir>/serve.pid`) on start and removes it on graceful shutdown.

## The three lifecycle modes and how they coexist

There are now three ways `serve` can be running. Every new code path must be able
to DETECT which one is active, because config propagation (capability 3) behaves
differently per mode.

| mode | who started it | detect it by |
| --- | --- | --- |
| **foreground** | user ran `pix serve` in a terminal | pidfile present + verified-ours, and NO managed plist loaded, and NOT flagged as lazy |
| **lazy** | auto-started detached by `ensureServe` | pidfile present + verified-ours + a sibling marker file `serve.lazy` exists |
| **managed** | launchd/systemd login service | `launchctl print` (macOS) / `systemctl --user is-active` shows our unit loaded |

Detection precedence for config propagation: managed > lazy > foreground/none.
Managed is checked first because a managed service also writes the pidfile
(it runs the same `pix-host serve`), so the pidfile alone cannot
distinguish managed from foreground — the launchd/systemd query is authoritative.

The `serve.lazy` marker is written by `ensureServe` right after a successful
detached spawn and removed by `serveStop` and by graceful shutdown. It is how
config propagation knows a running daemon is safe to stop-and-restart (lazy)
versus should be left alone with a printed note (foreground).

---

## Capability 1 — Lazy auto-start (the default)

### Behavior

A reusable `ensureServe(cfg, opts)` that the launcher calls from `run`, the
`memory` command path, and the `knowledge` command path. If the configured
services' ports are already up, it is a fast no-op (one short dial each). If they
are down, it spawns `pix-host serve` DETACHED, polls health until ready or a
timeout, and prints legible progress. On any failure it explains why and returns
an error the caller degrades on — it never hangs and never blocks the primary
action longer than the timeout.

### The backdoor-shape reasoning (address this in a code comment)

AGENTS.md forbids a host daemon that "spawns a child process from network input"
because that is backdoor-shaped and trips EDR. Lazy auto-start does NOT violate
this and the spec requires a comment at the `ensureServe` definition saying so:

> The spawn trigger here is a USER-invoked CLI command (`pix run`,
> `pix memory recall`) run in the user's own login session, spawning the
> user's OWN already-installed `pix-host serve`. There is no network input
> in the trigger path — nothing listening on a socket decides to spawn. This is
> exactly the Ollama-CLI model (`ollama run` starts `ollama serve`). The thing
> AGENTS.md prohibits is a *listening* Go/Node process that forks on a received
> request; this is a foreground CLI forking a sibling binary. Different shape.

### Signatures and new file

New file: `services/host/cmd/pix/serve_start.go` (launcher side; sits next
to `serve_ctl.go` and shares its `serveCtl` infra).

```go
// serveStarter bundles the injectable OS ops ensureServe needs, mirroring
// serveCtl. defaultServeStarter() wires the real ops; tests substitute fakes.
type serveStarter struct {
    hostBin   func() (string, error)          // findHostBinary
    dial      func(port int) bool             // liveness probe (dialLocalPort)
    spawn     func(bin string, args, env []string, logPath string) (int, error) // detached spawn -> child pid
    lockPath  func() string                   // serveSpawnLockPath()
    lazyMark  func() (mark func(), clear func()) // write/remove serve.lazy marker
    ctl       serveCtl                        // REUSE: verify an already-running pid is ours
    sleep     func(d time.Duration)           // poll delay (injected)
    now       func() time.Time                // clock (injected)
    logPath   func() string                   // serveLogPath()
    stderr    io.Writer                       // user-facing progress messages
}

func defaultServeStarter() serveStarter { /* real ops */ }

// ensureServeOpts lets a caller narrow the wait to the ports it actually needs
// (run needs memory+knowledge; `memory recall` only needs memory) and opt the
// whole thing out.
type ensureServeOpts struct {
    Services []string      // subset to require up; empty = the config `services` set
    Timeout  time.Duration // health-wait budget; 0 = default (see below)
    Quiet    bool          // suppress the "starting…" line when already up (still logs failures)
}

// ensureServe makes the configured services reachable, auto-starting a detached
// `pix-host serve` if needed. Returns nil when the required ports answer
// (already up OR started-and-became-ready), or an error describing why it could
// not (spawn failed / timed out / opted out and down). NEVER hangs past Timeout.
func ensureServe(st serveStarter, cfg *config.Config, opts ensureServeOpts) error
```

Control flow of `ensureServe`:

1. **Opt-out gate.** If `PIX_NO_AUTOSERVE` is set (any non-empty value) OR
   config `host.autoserve = false`, skip spawning entirely. Probe the required
   ports; return nil if up, else a sentinel `errAutoserveDisabled` whose message
   is "serve not running and auto-start is disabled (PIX_NO_AUTOSERVE /
   host.autoserve=false) — run `pix serve` yourself". Callers degrade.
2. **Resolve required ports** from `opts.Services` (or `cfg.Services`, or the
   default set). Map service name -> port via the same `MEMORY_PORT` /
   `KNOWLEDGE_PORT` env-aware resolution `serve_ctl.go` already uses (`servePort`).
   Only services in the enabled set are required — capability requirement (b).
3. **Fast path.** Dial each required port. If ALL up, return nil (print nothing
   unless `!Quiet` and we want a one-liner; default silent when already up).
4. **Take the spawn lock** (see below). Under the lock, RE-DIAL (double-checked
   locking): a racing process may have started serve between step 3 and the lock.
   If now up, release and return nil.
5. **Idempotency via pidfile.** Still under the lock, read the pidfile through
   `st.ctl`. If it points at a live, verified-ours `pix-host serve`
   (`verify(pid)` -> ours), treat that as success-in-progress: the process exists
   but its port has not bound yet, so fall through to the health-wait (do NOT
   spawn a second one). This reuses `serve_ctl`'s ownership check verbatim.
6. **Spawn detached.** Print `starting pix services (memory:11435,
   knowledge:11436)…` to stderr. Call `st.spawn(bin, ["serve", <enabled>...],
   env, logPath)`. Write the `serve.lazy` marker. Release the lock.
7. **Health-wait.** Poll every 200ms up to `Timeout` (default 15s) dialing the
   required ports. On all-up: print `pix services ready`. On timeout: print
   the failure + `see logs: <logPath>` and return an error. Tail the last ~10
   lines of the log file into the error message so the user sees WHY (e.g. "port
   in use", "ollama unreachable") without opening the file.
8. **Honest failure.** If `spawn` itself errors (bin not found, fork failed),
   return immediately with that reason — never poll a process that never started.

### Detached spawn mechanics (`spawn` real impl)

The real `spawn` must fully detach so the child outlives the launcher and is not
tied to the user's terminal:

```go
cmd := exec.Command(bin, args...)
cmd.Env = env
logf, _ := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
cmd.Stdin = nil
cmd.Stdout = logf
cmd.Stderr = logf
cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // new session + process group
if err := cmd.Start(); err != nil { return 0, err }
pid := cmd.Process.Pid
_ = cmd.Process.Release() // do NOT wait; we are launching, not supervising
return pid, nil
```

`Setsid: true` (same field on darwin and linux) gives the child its own session
so a terminal close / SIGHUP to the launcher does not reach it. `Release()` +
never `Wait()` means the launcher exits and the daemon keeps running. The daemon
writes its OWN pidfile on startup (existing `writeServePidFile`), so the launcher
does not need the returned pid for tracking — it is returned only for the
health-wait's optional liveness cross-check and for tests.

### The spawn lock (race-safety, requirement a)

Reuse the `withTaskLock` pattern from `task.go` (`syscall.Flock(LOCK_EX)` on a
lock file). New lock path: `config.ServeSpawnLockPath()` =
`<config-dir>/serve.spawn.lock`.

```go
func withServeSpawnLock(fn func() error) error // flock(LOCK_EX) on ServeSpawnLockPath, always LOCK_UN + Close
```

Two concurrent `pix run`s: the first takes the flock, double-checks the
port, spawns, releases. The second blocks on the flock, then double-checks and
finds the port up (or the pidfile owned) and does NOT spawn. Flock is chosen over
an atomic pidfile-create because we ALREADY have this exact helper (`task.go`) and
the daemon — not the launcher — owns the real pidfile; the launcher just needs a
short mutual-exclusion window around the spawn decision. Blocking (not
`LOCK_NB`) is correct: the critical section is a couple of port dials plus a
`fork`, sub-second.

Reuse note: `withServeSpawnLock` is a near-copy of `withTaskLock`. Factor the
shared body into a small `withFlock(path string, fn func() error) error` helper
that both call, rather than duplicating the flock dance a third time.

### Log path

`serveLogPath()` returns an XDG-state path (NOT the config dir, which is for
config + pidfile only). There is no state/log helper today, so add one to
`config`:

```go
// StateDir resolves the per-user state dir: $XDG_STATE_HOME/pix, else
// ~/.local/state/pix (Linux). Used for every serve log across every
// launch mode — see the unification note below.
func StateDir() (string, error)
func ServeLogPath() string // <state-dir>/serve.log
```

Decision (UPDATED — unified): every launch mode logs to the SAME
`~/.local/state/pix/serve.log`, on both macOS and Linux. Lazy logs there
directly; the managed launchd LaunchAgent points BOTH `StandardOutPath` and
`StandardErrorPath` at it (launchd interleaves stdout+stderr into one file,
matching the lazy path's combined-output behavior); the managed systemd
--user unit sets `StandardOutput=append:<path>` / `StandardError=append:<path>`
instead of journald. This replaced an earlier split (the managed launchd
service logging to `~/Library/Logs/pix-serve.{out,err}.log` separately)
that left three destinations for "the serve log" depending on how it was
started — confusing enough in practice that it was collapsed to one file.
`journalctl --user -u pix-serve` still works on Linux as a secondary
view, but the file is primary and is what every message/doc points at.

### Timeout

Default health-wait: **15s**. Rationale: memory opens a sqlite store and takes an
advisory flock; knowledge reindexes bundles at startup (can be a few seconds on a
large bundle). 15s is generous enough to avoid a false failure on a cold start
and short enough that a genuinely broken start (e.g. port permanently in use)
fails fast. Overridable via `opts.Timeout`. The per-poll dial stays at the
existing 300ms (`dialLocalPort`).

### Call sites

| caller | file | hook |
| --- | --- | --- |
| `run` | `run.go` `runRun` | after config resolve, BEFORE `wireKnowledgeScope`. `ensureServe(st, cfg, {Services: cfg.Services})`. On error: print the reason and CONTINUE the launch (recall/knowledge degrade in-VM exactly as today — the sandbox does not require the daemon to boot). |
| `memory` | `memory.go` `runMemoryCore`, right before `dispatchMemory` | `ensureServe(st, cfg, {Services: []string{"memory"}})`. On error: fall through; the existing `errServiceDown` path still prints the friendly message and exits 3. Auto-start turns the common case (daemon just not up yet) into "it just works". |
| `knowledge` | `knowledge.go` query/sync entry points that RPC `:11436` | `ensureServe(st, cfg, {Services: []string{"knowledge"}})` before the RPC. Same degrade. |

`run`'s hook must stay best-effort and NON-blocking-on-failure: launching a
sandbox must never be gated on the host daemon. The memory/knowledge hooks may
surface the failure (they are ABOUT the daemon) but still must not hang.

---

## Capability 2 — Managed login service

### Behavior

New launcher subverbs `pix serve install` / `pix serve uninstall`,
dispatched in the existing `runServe` switch in `main.go` alongside `stop` /
`status`. `install` generates the real plist from the template (no CHANGEME),
writes it to `~/Library/LaunchAgents`, and `launchctl bootstrap`s it (RunAtLoad +
KeepAlive). `uninstall` `bootout`s and removes it. Both print what happened and
where logs live.

### macOS (primary)

New file: `services/host/cmd/pix/serve_install_darwin.go` +
`serve_install_other.go` (build-tagged) so the launcher compiles on Linux
without launchd symbols. The cross-platform dispatch and message formatting live
in a shared `serve_install.go`; only the OS-specific `launchctl` / `systemctl`
calls are behind build tags.

Templating approach: **do NOT ship a separate .plist asset to sed at runtime.**
`go:embed` the template into the binary and fill it via `text/template`,
replacing the two CHANGEME-bearing fields. (IMPLEMENTATION NOTE: `go:embed`
cannot reach outside the Go module — `scripts/macos/` is above `services/host/`
— so the template lives IN-PACKAGE at
`services/host/cmd/pix/templates/com.pix.serve.plist.tmpl` and is the
single source of truth; the old `scripts/macos/com.pix.serve.plist` was
removed and `host-setup.sh` now delegates to `pix serve install`.)

```go
type plistData struct {
    HostBin  string // absolute path to installed pix-host (findHostBinary + EvalSymlinks)
    Home     string // os.UserHomeDir()
    LogPath  string // config.ServeLogPath() -- StandardOutPath AND StandardErrorPath both point here (unified serve log)
    Label    string // com.pix.serve
}
```

Rewrite the embedded template's `/Users/CHANGEME/...` literals into
`{{.HostBin}}`, `{{.Home}}`, `{{.LogPath}}` placeholders so the
same file is both the human-readable reference AND the template source (single
source of truth; no drift between the doc plist and the generated one). Keep the
existing `EnvironmentVariables > PATH` block so the launchd-launched daemon can
reach a homebrew ollama.

`install` steps:

1. Resolve `pix-host` absolute path via `findHostBinary()` + `EvalSymlinks`
   (launchd has a minimal PATH; the path MUST be absolute and real, not a
   `~/.local/bin` symlink into `out/`). Fail loudly if the binary is not found.
2. Render the plist to `~/Library/LaunchAgents/com.pix.serve.plist` (0644),
   `MkdirAll` the LaunchAgents dir plus `config.ServeLogPath()`'s parent dir
   (0700) so launchd can create the log file.
3. If a plist is already loaded, `launchctl bootout gui/$(id -u)/com.pix.serve`
   first (idempotent re-install), ignore "not loaded" errors.
4. `launchctl bootstrap gui/$(id -u) <plist>` (modern replacement for the
   deprecated `launchctl load -w` that `host-setup.sh` uses). Fall back to
   `load -w` if `bootstrap` fails on an old macOS.
5. `launchctl kickstart -k gui/$(id -u)/com.pix.serve` to start it now
   (RunAtLoad covers reboots; kickstart covers "start immediately").
6. Print: `installed managed service com.pix.serve (starts at login,
   auto-restarts). logs: <config.ServeLogPath()>`.

`uninstall` steps: `launchctl bootout gui/$(id -u)/com.pix.serve` (ignore
not-loaded), `os.Remove` the plist, print `removed managed service; run
`pix serve install` to re-enable or `pix serve` to run it in the
foreground`.

The domain target `gui/$(id -u)` is used (not the deprecated bare label) because
these are per-user LaunchAgents. `id -u` comes from `os.Getuid()`.

### Linux

Decision: **ship a `systemd --user` unit generator**, not a "not supported"
message. Rationale: the whole point is to stop babysitting on the machine the
user runs the host on; a Linux host that only gets lazy-start is a second-class
experience, and the systemd `--user` unit is ~15 lines and symmetrical to the
launchd path (both are "generate a unit file, ask the init system to load it").

`serve_install_linux.go`: write `~/.config/systemd/user/pix-serve.service`
(embedded `text/template`, `ExecStart=<host-bin> serve`, `Restart=always`,
`WantedBy=default.target`), then `systemctl --user daemon-reload`,
`systemctl --user enable --now pix-serve.service`. `uninstall`:
`systemctl --user disable --now`, remove the unit, `daemon-reload`. Logs go to
the SAME `config.ServeLogPath()` file (`StandardOutput=append:<path>` /
`StandardError=append:<path>` in the unit, not journald); print
`logs: <config.ServeLogPath()>` (`journalctl --user -u pix-serve` still
works as a secondary view). If `systemctl` is absent (non-systemd distro),
degrade to the explicit message: "no systemd --user found; use lazy
auto-start (default) or run `pix serve` yourself."

### Setup wizard hook (DO NOT implement — another agent owns onboarding)

Leave EXACTLY this marker where `setup` would call install, and nothing more:

```go
// TODO(setup-onboarding): the `pix setup` wizard should offer to run
// `serve install` here (managed always-on service) as an opt-in step. Owned by
// the onboarding agent — do not wire it from this change. See docs/design/
// serve-lifecycle.md §"Setup wizard hook".
```

Place it in `setup.go` at the point after config is written and services are
chosen. This change adds the marker only; it does not modify `setup` behavior.

---

## Capability 3 — Config propagation

### Which keys are daemon-affecting

Daemon-affecting (changing them requires the running `serve` to restart to take
effect): **`services`, `memory_watcher_model`, `memory_embed_model`,
`knowledge_bundles`**. These are read by the daemon at startup
(`applyMemoryModelEnv`, `resolveServices`, `knowledgeBundles`), never re-read
live.

NOT daemon-affecting (must NOT trigger a restart): `gog_account` (host MCP
server, not `serve`), `host.enabled` / `host.autonomy` (gate `pix host`),
`ollama_bridge_model` (a per-run workspace file written at `run` time, read by
the in-VM bridge — the daemon never sees it), `mcp` (gateway registration),
`pack`, `kit`. Encode the affecting set as a single predicate:

```go
var daemonAffectingKeys = map[string]bool{
    "services": true, "memory_watcher_model": true,
    "memory_embed_model": true, "knowledge_bundles": true,
}
func isDaemonAffecting(key string) bool { return daemonAffectingKeys[key] }
```

Note: profiles were removed — `knowledge_bundles` is a single base-config list
now, and `serve` indexes it directly (`AllKnowledgeBundles` just dedupes the
base list, no cross-profile union). Keep it simple: any set/unset of an
affecting key triggers propagation.

### Where it hooks

In `runConfigWrite` (`config.go`), AFTER `cfg.Save()` succeeds and the summary is
printed, call:

```go
if isDaemonAffecting(argv[0]) {
    propagateServeConfig(defaultServeReloader(), os.Stdout)
}
```

New file: `services/host/cmd/pix/serve_reload.go`.

```go
// serveReloader bundles the injectable ops config-propagation needs: detect the
// active lifecycle mode, kickstart a managed unit, stop-and-relazy a lazy daemon.
type serveReloader struct {
    mode        func() serveMode                 // detectServeMode(): managed | lazy | foreground | down
    kickManaged func() error                     // launchctl kickstart -k / systemctl restart
    stopServe   func(io.Writer) (bool, error)    // REUSE stopServe(defaultServeCtl(), out)
    ensure      func() error                     // ensureServe with the config set (re-lazy-start)
}

type serveMode int
const (
    serveDown serveMode = iota
    serveForeground
    serveLazy
    serveManaged
)

func propagateServeConfig(rl serveReloader, out io.Writer)
```

`detectServeMode` logic:

1. Query the managed unit: macOS `launchctl print gui/$(uid)/com.pix.serve`
   exit 0 => `serveManaged`; Linux `systemctl --user is-active pix-serve`
   == "active" => `serveManaged`.
2. Else read the pidfile via `serveCtl`; if live + verified-ours:
   `serveLazy` when the `serve.lazy` marker file exists, else `serveForeground`.
3. Else `serveDown`.

`propagateServeConfig` behavior per mode:

| mode | action | message |
| --- | --- | --- |
| `serveManaged` | `launchctl kickstart -k gui/$uid/com.pix.serve` (macOS) / `systemctl --user restart pix-serve` (linux) | `restarted managed pix services to apply the change.` |
| `serveLazy` | `stopServe(...)` then `ensureServe(...)` to re-spawn detached | `restarted pix services (background) to apply the change.` |
| `serveForeground` | do nothing to the process | `note: a `pix serve` is running in the foreground — restart it (Ctrl-C, re-run) to apply this change.` |
| `serveDown` | nothing | `note: the change applies next time pix services start.` |

Kickstart is chosen for managed (not bootout+bootstrap) because `-k` kills and
restarts the running instance in place while keeping RunAtLoad/KeepAlive intact —
exactly the "reload config" primitive. The lazy path must stop-then-ensure (not
kickstart, there is no unit) reusing BOTH existing pieces: `stopServe`
(`serve_ctl.go`) and `ensureServe` (capability 1). Foreground is deliberately
left running — we never kill a process the user is watching in their own
terminal; we tell them.

Every branch is best-effort and prints; a failure to restart prints a warning and
the change is still saved (the file is the source of truth; worst case the user
restarts manually).

---

## Injectable seams (testability)

Real detached-spawn, launchctl, and systemctl CANNOT run in CI or the build
sandbox, so every OS boundary is injected, following the established `serveCtl` /
`resetFS` / `shellEnv` pattern.

| seam | injected in | fakes prove |
| --- | --- | --- |
| `serveStarter.spawn` | `ensureServe` | spawn-once under race, honest failure when spawn errs, no double-spawn when pidfile owned |
| `serveStarter.dial` + `sleep` + `now` | `ensureServe` | fast path (all up), health-wait success, timeout path (never real-sleeps) |
| `withServeSpawnLock` (real flock on a temp path) | test drives two goroutines | second caller sees port up, does not spawn (mirrors `lock_test.go` cross-process flock test) |
| `serveReloader.mode` | `propagateServeConfig` | each of the four modes routes to the right action + message |
| `serveReloader.kickManaged` / `stopServe` / `ensure` | `propagateServeConfig` | managed kickstarts, lazy stop-then-ensure, foreground/down touch nothing |
| plist/unit rendering | pure `renderPlist(plistData) string` / `renderUnit(...)` | golden-file assert on rendered output; no launchctl call needed |
| `launchctl`/`systemctl` exec | `serve_install*.go` via an injected `run func(name string, args ...string) (string, error)` | install/uninstall issue the right argv in the right order; not-loaded errors ignored |

`isDaemonAffecting`, `detectServeMode`'s pidfile/marker logic (with injected
`serveCtl` + a fake marker-exists func), and the port-set resolution are all pure
and unit-tested without any process.

The one thing that stays a thin, untested shim (like `defaultServeCtl`'s real
syscalls): the concrete `spawn` with `Setsid`, the concrete `launchctl`/`systemctl`
`exec.Command` wrappers, and the real flock open. Keep them one-liners so the
untested surface is minimal.

---

## Man page + help wiring (keep man_test green)

`TestManPageDocumentsEveryKnownVerb` maps `knownVerbs` 1:1 to `"pix <verb>"`
forms in the embedded man page. `serve` is ALREADY a known verb, so `install` /
`uninstall` are SUBverbs of `serve` — they do NOT add to `knownVerbs`, and the
verb test stays green as-is. But we still must document them for humans:

1. `help.go` `serveUsage`: add `install` and `uninstall` to the subcommands list,
   and add the `PIX_NO_AUTOSERVE` opt-out + auto-start note to the serve
   help prose.
2. `pix.1` man page: extend the `.SS serve` section with the `install` /
   `uninstall` subcommands, the lazy auto-start behavior, `PIX_NO_AUTOSERVE`,
   and the log locations. Because the man-verb regex only matches
   `"pix serve`, subverbs need no separate synopsis line, but document them
   in prose so the page is accurate.
3. `TestManPageDocumentsEveryConfigKey`: this change adds NO new config key
   (`host.autoserve` is the one exception below). If we add `host.autoserve`, it
   MUST be added to `configKeysHelp` in `config.go` AND to the man page's config
   `.SS`, or the test fails. Do both in the same commit.

New config key `host.autoserve` (bool, default true) — the config-flag opt-out
from requirement (d), sibling of `PIX_NO_AUTOSERVE` (env wins). Add to
`applyConfigChange`, `configValue`, `configKeysHelp`, and the man page together.
It is NOT daemon-affecting (it changes launcher behavior, not the daemon), so it
must NOT be in `daemonAffectingKeys`.

---

## Message table (user-facing, the whole point is legibility)

| situation | stream | message |
| --- | --- | --- |
| lazy: services already up | — | (silent; `Quiet` default) |
| lazy: starting | stderr | `starting pix services (memory:11435, knowledge:11436)…` |
| lazy: ready | stderr | `pix services ready` |
| lazy: spawn failed | stderr | `could not start pix services: <reason>. run `pix serve` to see the error.` |
| lazy: health timed out | stderr | `pix services did not become ready in 15s. last log lines:\n<tail>\nsee logs: ~/.local/state/pix/serve.log` |
| lazy: opted out + down | stderr | `serve not running and auto-start is disabled (PIX_NO_AUTOSERVE / host.autoserve=false) — run `pix serve`.` |
| managed install ok (launchd) | stdout | `installed managed service com.pix.serve (starts at login, auto-restarts). logs: ~/.local/state/pix/serve.log` |
| managed install ok (systemd) | stdout | `installed managed service pix-serve.service (starts at login, auto-restarts). logs: ~/.local/state/pix/serve.log` |
| managed install, no host bin | stderr | `serve install: pix-host not found — run `make install` first.` |
| managed uninstall ok | stdout | `removed managed service. run `pix serve install` to re-enable, or `pix serve` for foreground.` |
| linux, no systemd | stderr | `no systemd --user found; use lazy auto-start (default) or run `pix serve` yourself.` |
| config propagate, managed | stdout | `restarted managed pix services to apply the change.` |
| config propagate, lazy | stdout | `restarted pix services (background) to apply the change.` |
| config propagate, foreground | stdout | `note: a foreground `pix serve` is running — restart it to apply this change.` |
| config propagate, down | stdout | `note: the change applies next time pix services start.` |

---

## New files and touched files (build checklist)

New:
- `services/host/cmd/pix/serve_start.go` — `ensureServe`, `serveStarter`, `withServeSpawnLock`, opt-out.
- `services/host/cmd/pix/serve_install.go` — shared install/uninstall dispatch + message formatting + `renderPlist`/`renderUnit`.
- `services/host/cmd/pix/serve_install_darwin.go` / `_linux.go` / `_other.go` — build-tagged launchctl/systemctl.
- `services/host/cmd/pix/serve_reload.go` — `propagateServeConfig`, `detectServeMode`, `serveReloader`.
- Tests: `serve_start_test.go`, `serve_install_test.go`, `serve_reload_test.go`.

Touched:
- `config/config.go` — `StateDir()`, `ServeLogPath()`, `ServeSpawnLockPath()`; `host.autoserve` field on `HostMode`.
- `serve.go` (host) — write/remove the `serve.lazy` marker is LAUNCHER-side (ensureServe), NOT here; but graceful shutdown should also best-effort remove the marker so a crash-free stop clears it. Add `removeServeLazyMarker()` next to `removeServePidFile()` and call it in the shutdown goroutine. (The marker is launcher-owned but the daemon clearing it on clean exit prevents a stale-lazy misdetection.)
- `main.go` `runServe` switch — add `install` / `uninstall` cases.
- `run.go`, `memory.go`, `knowledge.go` — `ensureServe` call sites.
- `config.go` — `propagateServeConfig` hook after `Save()`; `host.autoserve` in `applyConfigChange` + `configValue` + `configKeysHelp`; refactor `withTaskLock`/`withServeSpawnLock` shared `withFlock`.
- `help.go` `serveUsage` — install/uninstall + auto-start prose.
- `pix.1` — serve `.SS` update + `host.autoserve` config key.
- `setup.go` — the `TODO(setup-onboarding)` marker ONLY.
- `scripts/macos/com.pix.serve.plist` — convert CHANGEME literals to `text/template` placeholders (still human-readable) and `go:embed` it.

---

## Open decisions

1. **`serve.lazy` marker ownership.** Launcher writes it, but a lazy daemon that
   is later `serve stop`ped by the user (foreground stop verb) must clear it.
   Decision: `stopServe` (serve_ctl.go) removes the marker on a successful stop;
   the daemon's own graceful shutdown also removes it (belt + suspenders). A hard
   `kill -9` leaves a stale marker — acceptable, because `detectServeMode` also
   requires the pidfile to be live+ours, and a dead pid downgrades to `serveDown`
   regardless of the marker. So the marker is only consulted when the process is
   confirmed alive. RESOLVED as stated; flag if a reviewer disagrees.
2. **Should `run`'s lazy start wait, or fire-and-forget?** Waiting adds up to 15s
   to a cold `pix run`. Decision: wait, but with a SHORTER budget for `run`
   (e.g. 8s) since the sandbox degrades gracefully anyway — better to not block a
   launch long. Alternatively fire-and-forget for `run` (spawn, don't health-wait)
   and full-wait for `memory`/`knowledge`. LEANING fire-and-forget for `run`
   (the sandbox reaches the daemon over `host.docker.internal` a few seconds into
   the session, by which time it is up). Needs a call; both are one `opts` flag.
3. **systemd vs "not supported" on Linux.** Chose systemd `--user`. If the team
   wants to ship macOS-only first and defer Linux, downgrade `_linux.go` to the
   explicit message and file a follow-up — the seam is identical.
4. **`host.autoserve` vs a dedicated `[serve]` config table.** Put the flag under
   the existing `host.*` namespace to avoid a new table + man-page section. If
   more serve knobs appear later (custom timeout, log path), promote to `[serve]`.

## Risks

- **Double-start under a lost lock.** If flock silently fails on an exotic FS
  (NFS home dir), two daemons could race the port. Mitigation: the daemon takes
  the memory STORE flock (`lockMemoryStoreOrFatal`) and fatals if held, so the
  second loser dies loudly rather than corrupting — the port-bind also fails. The
  spawn lock is an optimization; the store lock is the correctness backstop.
- **Stale lazy daemon after a crash.** A `-9`'d daemon leaves a stale pidfile +
  marker. `detectServeMode` requires live+ours, so it self-heals to `serveDown`.
  `ensureServe`'s double-checked pidfile path also re-verifies ownership before
  trusting it.
- **launchctl API drift across macOS versions.** `bootstrap`/`bootout`/`kickstart`
  are the modern (Big Sur+) surface; older `load -w`/`unload` is the fallback.
  Guard with a fallback exec and clear error text. All behind an injected `run`
  so the argv choices are unit-tested.
- **EDR flagging the detached spawn.** Addressed by the backdoor-shape comment:
  the trigger is a user CLI, not network input. Still, a paranoid EDR might flag
  a forked-and-detached child. Lazy is the DEFAULT but fully opt-out
  (`PIX_NO_AUTOSERVE` / `host.autoserve=false`), and managed (launchd) is the
  EDR-friendliest path since it is a declared, signed LaunchAgent.
- **Blocking a launch on a slow cold start.** Mitigated by open decision #2 (short
  or no wait for `run`) — a `run` must never feel slower because of this feature.
```
---

## Hardening addendum (post-review fix-loop)

A security-lead pass + cross-vendor review over the first implementation found
real defects. The contracts below are NOW part of the design; the sections
above describe the original intent and stand except where amended here.

### H1 — serve.log is symlink-proof

`serve.log` lives in a user-writable state dir, so both ends refuse symlinks:

- **Write side** (`openServeLogFile`, serve_start_unix.go): `Lstat` before
  open; a symlink is removed and replaced by a regular file, and the open uses
  `O_NOFOLLOW` (+ `0600`) so the Lstat→open TOCTOU is closed. A symlink that
  cannot be removed is a loud error, never followed.
- **Read side** (`tailFileLines`, serve_start.go): `Lstat`; a symlink returns
  "" — the failed-start tail never echoes an attacker-linked file's last lines
  to the terminal.

### H2 — explicitly-empty `services` survives Save→Load

`services` has a NON-EMPTY default, so a plain `[]string` + omitempty could not
distinguish "unset → default" from "explicitly empty → stays empty":
`config unset services memory` reported `[]` but reload silently restored
`["memory"]`. The config schema now carries the presence bit: `Config.Services`
(`toml:"-"`) is the resolved runtime field every consumer reads, and
`Config.ServicesRaw *[]string`
(`toml:"services,omitempty"`) is the TOML image — nil = absent (default),
present-empty = `services = []` (stays empty). A list that becomes empty only
through removed-service filtering (stale `["gws"]`) still falls back to the
default. Round-trip gated by `TestRemoveLastServiceRoundTripsExplicitEmpty`.

### H3 — no double-spawn during cold init

The daemon writes `serve.pid` only after config load + store open + indexing;
a second caller acquiring the spawn lock in that window used to see no pidfile
and fork a second daemon. Now `ensureServe` writes a LAUNCHER-owned pidfile
(`recordPid`, the spawned child's pid) BEFORE the spawn lock releases, so a
racing caller's `readLiveServePid` sees a live verified-ours pid and WAITS. The
daemon's own `writeServePidFile` later overwrites the file with the same pid.

### H4 — the lazy marker carries the pid

A crash before the pidfile landed used to leave a bare `serve.lazy` marker
that misclassified a LATER foreground `pix serve` as lazy — config
propagation would stop+restart a process the user was watching. The marker now
contains the spawned PID, and `detectServeMode` classifies lazy ONLY when the
marker pid equals the live, verified pidfile pid. A mismatched, legacy
(`lazy\n`), or unparseable marker means FOREGROUND (the conservative
direction: advise, never kill).

### H5 — `serve install` clears the ground and verifies

Installing a KeepAlive/Restart=always unit over an already-running daemon
collided on ports + store lock and crash-looped while install reported
success. `runServeInstall` now runs `preInstallGuard` first: a VERIFIED lazy
daemon is stopped; a FOREGROUND daemon is REFUSED with instructions; managed
(idempotent re-install) and down proceed. After the platform install it runs
`reportManagedServeHealth` (bounded, 10s) and reports honestly when the
services did not come up.

### H6 — the managed unit captures install-time env

The generated plist/systemd unit renders an absolute
`PIX_CONFIG=<config.Path()>` ALWAYS, plus these daemon-relevant vars when
set at install time: `XDG_CONFIG_HOME`, `MEMORY_DB`, `MEMORY_PORT`,
`KNOWLEDGE_PORT`, `OLLAMA_HOST` (`capturedServeEnvVars`). Without this, a
launcher running with any of those set installed a daemon reading a DIFFERENT
config/store/ports — and config propagation "restarted" a daemon that never
saw the change.

### H7 — template rendering is injection-proof

`text/template` does not escape. `renderPlist` XML-escapes every interpolated
value (a `</string>` in a path can no longer inject plist structure) and
`renderUnit` systemd-quotes the ExecStart binary path (spaces stay one argv
element) and each `Environment="KEY=value"` line. Values carrying
newlines/control chars are refused loudly — no quoting can contain them.

### H8 — round 2 fix-loop (security + cross-vendor re-review)

Five residual issues from a second review pass, each closed with a proving
test:

1. **`recordPid` now returns an error.** `recordSpawnedServePid`'s
   `MkdirAll`/`WriteFile` failures were swallowed, so `ensureServe`'s spawn
   lock released as if the pid had landed — reopening the H3 double-spawn
   window with no memory-store flock to catch it for a knowledge-only config.
   `serveStarter.recordPid` is now `func(pid int) error`; on failure
   `ensureServe` kills the just-spawned child (`SIGKILL`) and returns the
   error instead of releasing the lock as success.
   (`TestEnsureServeRecordPidFailureKillsChildAndFails`,
   `TestRecordSpawnedServePidReturnsErrorOnFailure`.)
2. **`tailFileLines` closes an Lstat→ReadFile TOCTOU.** The read side now
   goes through `readFileNoSymlink` (unix: `os.OpenFile` with
   `syscall.O_NOFOLLOW`, so the open itself atomically refuses a symlink;
   non-unix: refuses to read at all). (`TestTailFileLinesRefusesSymlink`.)
3. **`verifyServeProcPS` is space-safe.** The darwin/BSD fallback used to run
   `ps -o command=` and `strings.Fields` the WHOLE line, so a binary path
   containing spaces (e.g. `/Users/alice/My Projects/pix-host`) parsed
   argv[0] as just `/Users/alice/My` and verification failed — breaking
   `serve stop`/`status`, `detectServeMode`, and the H5 install guard. It now
   asks `ps -o comm=` (one field: the executable path alone, no argv —
   nothing to mis-split) for the basename check, and a separate
   `ps -o args=` call scanned only for a standalone `serve` token.
   (`TestVerifyServeProcPS_PathWithSpaces`.)
4. **A `config.Load()` failure during post-install verification is now a
   reported failure, not a silent skip.** `if err == nil` used to guard the
   whole health check with nothing in the else branch, so a malformed
   `config.toml` printed "installed managed service" while verification
   never ran and the unit crash-looped. `verifyManagedInstallHealth` now
   prints an honest warning naming the load error and returns unhealthy.
   (`TestVerifyManagedInstallHealthConfigLoadFailure`.)
5. **Template escaping covers `%`/`$` and drops paths from XML comments.**
   `systemdQuote` now escapes `%`→`%%` (systemd specifier expansion) and
   `$`→`$$` (ExecStart/Environment variable expansion) in addition to the H7
   backslash/quote escaping. The launchd plist template no longer
   interpolates `OutLog`/`ErrLog` (both nested under `Home`) inside its
   top `<!-- -->` comment — XML text-escaping cannot make two adjacent
   hyphens legal inside a comment body, so a home directory like
   `/Users/alice--work` used to render an invalid plist. Those paths already
   have proper `<string>` elements (`StandardOutPath`/`StandardErrorPath`);
   the comment now only points at them.
   (`TestSystemdQuoteEscapesPercentAndDollar`,
   `TestRenderUnitEscapesPercentInHostBin`,
   `TestRenderPlistHomeWithDoubleDashProducesValidXML`.)

### M1 — platform seams

Detached spawn (`Setsid`), flock, and `syscall.Kill` live behind
`//go:build unix` files (`serve_start_unix.go`, `serve_ctl_unix.go`, root
`lock.go`) with non-unix shims (`serve_start_windows.go`,
`serve_ctl_windows.go`, root `lock_windows.go`) so `GOOS=windows` compiles:
lazy auto-start degrades to "auto-start is not supported on this platform; run
`pix serve` yourself", signalling degrades to refusal, and the store lock
degrades to a loud warn-and-proceed. darwin/linux/windows cross-compiles are
part of the verify gate.

### M2 — one deadline bounds lock + health

The spawn lock is now NON-blocking (`tryLock`, `LOCK_EX|LOCK_NB`) retried
under the SAME deadline as the health wait, so a wedged lock-holder can hang
`pix run` for at most its budget (8s), not forever. The run-path comment
now states the real bounded wait instead of "never gated".

### M4 / L1 — propagation honesty

Config propagation's lazy branch honors `stopServe`'s `stopped` bool: a
refused stop (stale/hijacked/unverifiable pid) warns and does NOT re-spawn (a
double-start risk). `knowledge init` / `knowledge use` save `knowledge_bundles`
(daemon-affecting) and now run the same propagation as
`config set knowledge_bundles` — the README/man no longer tell users to
restart manually on those paths.
