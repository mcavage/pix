# Building a private overlay

pi-stack is open-core: the public repo and image ship a generic coding stack, and
anything company-specific (proprietary skills, a CRM/warehouse/HR connector, an
internal `capabilities.json`) lives in a **private overlay**. The overlay is its
own **peer repo** — a sibling directory you keep private, never a subdirectory of
pi-stack. pi-stack references it by path (`OVERLAY`, default `../pi-stack-work`).

An overlay has two halves, because pi-stack runs in two places:

| half | runs | mechanism |
|---|---|---|
| **sandbox** | inside the disposable VM | a **mixin kit** (`kit/`) stacked with `--kit` |
| **host** | on your host (outside the VM) | **`host/overlay_*.go` plugins**, symlinked into `pi-stack/services/host/` at build |

A copyable scaffold lives in [`examples/overlay/`](../examples/overlay). The layout:

```
../my-overlay/                 # a peer repo (sibling of pi-stack), kept private
  kit/
    spec.yaml                  # kind: mixin
    files/                       # `home/` maps to $HOME (/home/agent), NOT /home —
      home/.pi/agent/            # so it's files/home/.pi/..., not files/home/agent/.pi/...
        skills/<your-skill>/SKILL.md     # private skills
        capabilities.json                # overwrites the public generic one
      home/.local/bin/<wrapper>          # in-sandbox CLI wrappers (on PATH; sbx
                                         # only delivers files under home/ + workspace/)
  host/
    overlay_<name>.go          # host plugins (Go, package main)
  overlay.mk                   # private make targets
```

pi-stack looks for it at `../pi-stack-work` by default. If your peer repo lives
elsewhere, pass `OVERLAY` to make (or export it in your shell):

```bash
make run OVERLAY=../my-overlay
```

## 1. Sandbox half — the mixin kit (`kit/`)

A mixin kit is a directory with a `spec.yaml` (`kind: mixin`) and a `files/` tree
that maps directly into the sandbox filesystem. `make run` stacks it automatically
when `$(OVERLAY)/kit/spec.yaml` exists:

```bash
sbx run pi-stack --kit ./pi-kit --kit ../my-overlay/kit --mcp ... .
```

- **Skills** under `files/home/.pi/agent/skills/` are added to the agent's
  skill set (additive — they don't touch the baked public skills).
- **`capabilities.json`** overwrites the public generic one, so your skills can ask
  for `crm`/`warehouse`/etc. and resolve to your real providers. Write
  capabilities, not vendors (see the `capability-routing` skill).
- **Wrappers** under `files/usr/local/bin/` are thin in-sandbox shims that forward
  to a host service, so credentials/SSO stay on the host (see half 2).

## 2. Host half — `host/overlay_*.go` plugins

A mixin kit can't ship host binary code, so host-side services (a warehouse exec
proxy, an extra MCP server) are Go files that compile into `pi-stack-host`. Put
them in your overlay's `host/` named `overlay_*.go`. `make serve` / `make
mcp-register` (via the `link-overlay` target) **symlink** them into
`pi-stack/services/host/` before building — the symlinks are gitignored there, so
your private code never enters the public tree, and a public clone (no overlay)
builds clean.

Each plugin **self-registers** via `init()`:

```go
package main

func init() {
    extraCommands["my-svc"] = runMySvc
    extraUsage = append(extraUsage, "  my-svc       my private host service  [overlay]")

    // optional long-running service started by `make serve` (add "my-svc" to SERVICES):
    extraServiceFactories = append(extraServiceFactories, func() hostService {
        return hostService{"my-svc", env("MY_SVC_BIND", "127.0.0.1") + ":12000", myMux()}
    })
}

func runMySvc() { /* ... */ }
```

`extraCommands`/`extraUsage`/`extraServiceFactories` are declared (empty) in
pi-stack's `main.go`; your plugin populates them only when present. The public
binary builds and runs identically without it. Plugins are `package main` and use
pi-stack-host's helpers (`env`, `writeJSON`, `mcpStdio`, `hostService`, …), so they
compile only when symlinked in — edit them in your overlay, build from pi-stack.

Reach the host service from the sandbox over `host.docker.internal:<port>` via the
in-sandbox wrapper from half 1. Add the port to your overlay kit's network rules
(`allowedDomains: host.docker.internal:<port>` + `localhost:<port>`) — the public
kit deliberately allows no overlay ports, so the overlay grants its own.

## 3. Make targets — `overlay.mk`

Put private make targets (auth helpers, a `doctor-overlay` readout) in your
overlay's `overlay.mk`; pi-stack's Makefile `-include`s `$(OVERLAY)/overlay.mk`.
`make doctor` calls `doctor-overlay` automatically, so your private integrations
show up in the status readout for you but not for a public cloner. Targets that
build the binary should depend on `link-overlay`.

## 4. Host plugin overrides — `[plugins.*]` in `config.toml`

The `overlay_*.go` route in half 2 compiles your service *into* `pi-stack-host`.
The plugin-override route does the opposite: it keeps your code in a **separate
binary** the public host launcher never links, and points at it from config. Use
this when a host capability is private end to end (a credential broker for an
internal data warehouse, a company memory/OKF backend) and you don't want it in
the build at all.

`pi-stack serve` reads `~/.config/pi-stack/config.toml` at startup. Three host
capabilities are pluggable slots — `broker` (the credential broker),
`memory` (the recall backend), and `mcp` (extra MCP servers). If a slot has a
`[plugins.<slot>]` table, `serve` launches that plugin binary once at startup in
place of the built-in; otherwise it runs the built-in. Each entry pins three
fields:

```toml
# ~/.config/pi-stack/config.toml
[plugins.broker]
impl = "warehouse-broker"                      # a name, for logs and doctor
path = "/opt/acme/bin/warehouse-broker"        # the go-plugin binary to launch
sha  = "sha256:1f3a…"                          # required; serve refuses a mismatch

[plugins.memory]
impl = "okf-memory"
path = "/opt/acme/bin/okf-memory"
sha  = "sha256:9c02…"
```

- **`impl`** is a label only (shows up in `pi-stack doctor` and logs).
- **`path`** is the plugin binary. Ship it however you like (your overlay's build,
  an internal artifact store); the public repo never sees it.
- **`sha`** is mandatory and SHA-pinned: `serve` hashes the file at `path` and
  refuses to launch it if the digest doesn't match, so a swapped or tampered binary
  fails closed instead of running.

The plugin is a `go-plugin` binary: `pi-stack serve` starts it once, speaks to it
over the plugin protocol for the life of the process, and shuts it down on exit.

This is the seam a company uses to plug private host infrastructure into pi-stack
**without a fork**: point `[plugins.broker]` at your internal warehouse credential
broker (the `snow`/warehouse examples elsewhere in this repo are illustrative
only — substitute your real one), or point `[plugins.memory]` at a company OKF /
knowledge backend, and the public launcher drives it unchanged. Nothing
company-specific lands in the public tree — the config lives under your `$HOME`,
the binary lives wherever you built it, and the public `pi-stack-host` keeps its
generic built-ins as the default.

Two routes, one decision: compile a service into the binary (half 2, `overlay_*.go`)
when it's an *additive* host service you're fine building from the pi-stack tree;
ship a separate plugin and override a slot here when the capability is private
end to end and must stay out of the build.

## What keeps the public repo clean

`.gitignore` ignores `services/host/overlay_*.go` (the symlinks), and
`scripts/check-open-core.sh` (run in CI) fails if any overlay symlink or known
internal marker is ever tracked, and asserts the skills/agents allowlists mirror.
The public image is verified to bake only allowlisted skills + agents. So you can
develop your overlay right next to pi-stack without risk of leaking it.
