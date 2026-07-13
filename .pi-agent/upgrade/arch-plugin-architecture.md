# pi-stack target architecture: go-plugin host, version-coupled launcher, consumer skills, stacked kits

Status: proposed target state (architect deliverable). Scope: full, no MVP hedging.
Author: architect subagent. Cites real files/lines from the tree at time of writing.

## Verdict

Re-base the entire host side on **hashicorp/go-plugin**. Today the host is one
statically-linked binary whose *only* extension seam is compile-time: overlay
`overlay_*.go` files get symlinked into `services/host/` and self-register via
`init()` into three package-level maps (`main.go:30-35`, consumed at
`main.go:56-62` and `serve.go:39-41`). That is the load-bearing contradiction the
prior review named: **a prebuilt public binary cannot carry private overlay code,
because "carry" means "link at build time."** go-plugin dissolves it by moving the
seam from *link time* to *process launch time*. The public `pi-stack-host` ships as
one released artifact; overlays and third parties ship their **own** plugin
binaries that the host launches over a local gRPC socket and never links. Private
code stays a private binary, forever unlinked from the public tree.

On top of that host change, three consumer-facing seams get promoted from
"hardcoded in `bin/pi-stack` + `Makefile`" to "declared in one config file":
version-coupled launch, bring-your-own skills, and N stacked kits.

The one thing that must be **verified before committing** the version-coupled
launcher: the sbx git-kit URL does not currently pin a ref, and CI
(`.github/workflows/publish.yml`) never creates a git tag. Both are fixable, but
they are unbuilt today. Flagged as Risk 1.

---

## 1. Component diagram (text)

### Today

```
HOST (one linked binary: pi-stack-host)
  main.go switch ─┬─ gws-token  (HTTP  :11441)   gwstoken.go
                  ├─ memory     (JSONRPC :11435) memory.go
                  ├─ slack      (stdio MCP)      slack.go  + util.go mcpStdio
                  └─ serve      runs HTTP svcs   serve.go
  overlay_*.go (symlinked, gitignored) ──init()──▶ extraCommands / extraUsage /
                                                    extraServiceFactories  (main.go:30-35)
  make link-overlay: ln -sf $(OVERLAY)/host/overlay_*.go services/host/   (Makefile)

LAUNCH
  bin/pi-stack           → sbx run pi-stack --kit pi-kit [--kit $OVERLAY/kit] .
  bin/pi-stack --dev     → + --no-skills --skill <repo>/skills --skill <overlay>/skills
  make run               → same, always Mode B
  sbx run --kit git+…    → consumer path, baked skills, image pinned by spec.yaml on main
```

### Target

```
HOST
  pi-stack (released binary; VERSION embedded via -ldflags)
    ├─ run           version-coupled launcher (wraps `sbx run`)      NEW
    ├─ serve         PLUGIN SUPERVISOR: launch + health + adapt      serve.go (rewritten)
    ├─ plugin <kind> built-in plugin server (self-exec)             NEW (memory|cred|mcp)
    └─ mcp <name>    sbx-gateway stdio shim → McpServer plugin       (replaces `slack`)

  serve supervises N go-plugin subprocesses over per-plugin unix sockets:
    ┌────────────── pi-stack serve (kernel: stable host surface) ──────────────┐
    │  :11435 JSON-RPC front-end ─gRPC─▶ [MemoryStore plugin]                   │
    │  :11441 HTTP /token        ─gRPC─▶ [CredentialBroker "gws"]               │
    │  :11442 HTTP /token        ─gRPC─▶ [CredentialBroker "warehouse" (overlay)]│
    │  (sbx gateway) ─stdio─▶ `pi-stack mcp slack` ─gRPC─▶ [McpServer "slack"]  │
    └──────────────────────────────────────────────────────────────────────────┘
    Each [ ] is EITHER self-exec built-in (`pi-stack plugin memory`)
                 OR an external binary (`pi-stack-plugin-memory-acme`) chosen by config.
    Handshake: magic cookie + ProtocolVersion → refuses a skewed plugin at launch.

LAUNCH
  pi-stack run          → sbx run pi-stack --kit "git+…#ref=v<VERSION>&dir=pi-kit"
                          + kits from config + skill mounts from config           NEW
  bin/pi-stack --dev    → unchanged (Mode B dev loop)
  make run              → unchanged (Mode B dev loop)
  config: ~/.pi-stack/config.toml is the single consumer config (kits/skills/plugins/pin)
```

The kernel (`serve`) owns the **stable host surface** every sandbox already
depends on (JSON-RPC :11435, HTTP :11441, the gateway stdio contract). Plugins
never bind those ports themselves; the kernel adapts gRPC ↔ the wire protocol the
sandbox speaks. So swapping a memory implementation changes nothing sandbox-side.

---

## 2. Plugin interfaces (Go sketches)

One shared handshake and a single protocol version gate all three plugin kinds.
Bumping `ProtocolVersion` on any breaking interface change makes go-plugin refuse a
stale plugin at launch with a clear error — this is the skew guard.

```go
// plugin/handshake.go  (a new public package: pi-stack/host/plugin)
const ProtocolVersion = 1

var Handshake = plugin.HandshakeConfig{
    ProtocolVersion:  ProtocolVersion,           // host & plugin MUST agree or launch fails
    MagicCookieKey:   "PI_STACK_PLUGIN",
    MagicCookieValue: "pi-stack.v1",              // stops a random exec being treated as a plugin
}

// VersionedPlugins lets the kernel serve >1 protocol during a migration window:
//   map[int]plugin.PluginSet{ 1: {"memory": &MemoryPlugin{}}, 2: {...} }
```

### 2a. MemoryStore — maps 1:1 to `memory.go`

```go
type MemoryStore interface {
    Remember(ctx context.Context, in RememberReq) (RememberResp, error) // memory.go remember()
    Recall(ctx context.Context, in RecallReq) ([]Hit, error)            // memory.go recall()
    Forget(ctx context.Context, idOrPrefix string) (bool, error)        // memory.go forget()
    Synthesize(ctx context.Context, threshold float64) (Merged, error)  // memory.go synthesize()
    Promotable(ctx context.Context, minFreq int) ([]Candidate, error)   // memory.go promotable()
    Observe(ctx context.Context, in ObserveReq) (Accepted, error)       // memory.go observe/memCapture
    Stats(ctx context.Context) (Stats, error)                           // memory.go stats()
    Health(ctx context.Context) (Health, error)                         // memory.go health method
}
```

The kernel keeps the JSON-RPC dispatcher that lives in `memoryMux()`
(`memory.go`, the `methods` map) and turns each RPC method into a typed gRPC call.
The sandbox recall extension is unchanged: it still POSTs JSON-RPC to :11435.

### 2b. CredentialBroker — generalizes `gwstoken.go` and every future exec-proxy

```go
type CredentialBroker interface {
    // Mint returns a short-lived credential for an audience/scope. gws ignores
    // audience today (gwstoken.go mint()); a warehouse/CRM broker uses it.
    Mint(ctx context.Context, in MintReq) (Credential, error) // {Audience,Scopes}->{Token,Type,ExpiresIn}
    Check(ctx context.Context) error                          // serve-preflight: gwsTokenCheck()
    Describe(ctx context.Context) (BrokerInfo, error)         // {Name, DefaultPort, AuthHeader, RequiresHostCLI}
}
```

This is the single most valuable generalization: the "keep the long-lived
credential on the host, mint a short-lived one, run the real CLI in the VM"
pattern (`gwstoken.go` header comment; the overlay `:11442` exec-proxy in
`spec.yaml`) becomes one interface a third party implements for any provider.
`Check()` feeds the existing serve preflight (`serve.go:65-79`).

### 2c. McpServer — maps to `slack.go` + `util.go` mcp scaffolding

```go
type McpServer interface {
    Info(ctx context.Context) (ServerInfo, error)                        // util.go initialize result
    ListTools(ctx context.Context) ([]ToolSpec, error)                   // util.go tools/list
    CallTool(ctx context.Context, name string, args map[string]any) (ToolResult, error) // tools/call
}
```

MCP is special because the sbx gateway spawns a **stdio** command from network
input, and AGENTS.md is explicit that this must stay a compiled Go binary (EDR).
That constraint is preserved: the registered command becomes
`pi-stack mcp <name>` (replacing today's `pi-stack-host slack`, see
`Makefile` `mcp-register` and `slack.go` header). That subcommand is a thin,
compiled **bridge**: it reads MCP-stdio via the existing `mcpStdio` /
`mcpDispatcher` machinery (`util.go:186`, `util.go:137`) and forwards
`ListTools`/`CallTool` to the McpServer plugin over gRPC. So the process that
"spawns a child from network input" is still first-party compiled Go; the actual
Slack/BambooHR logic is an overridable plugin behind it.

---

## 3. Process & supervision model

`pi-stack serve` becomes the supervisor. Rewrite of `serve.go`:

1. **Resolve slots from config** (see §5). Each slot = a capability
   (`memory`, `cred:gws`, `cred:warehouse`, `mcp:slack`) bound to an
   implementation (`builtin` or a named external binary).
2. **Launch each plugin** as a go-plugin subprocess:
   ```go
   client := plugin.NewClient(&plugin.ClientConfig{
       HandshakeConfig:  Handshake,
       VersionedPlugins: versioned,          // protocol-gated
       Cmd:              exec.Command(binPath, args...), // builtin: self "pi-stack plugin memory"
       AllowedProtocols: []plugin.Protocol{plugin.ProtocolGRPC},
       AutoMTLS:         true,               // mutual TLS over the local pipe
       Managed:          true,               // CleanupClients() kills all on shutdown
   })
   raw, _ := client.Client(); impl := raw.Dispense("memory")
   ```
   Built-ins re-exec the same binary (`pi-stack plugin memory`) — the exact
   self-exec pattern `subagents.ts` already relies on (`pi --no-extensions -e
   <self>`, see its header comment). No extra binaries ship for the default case.
3. **Adapt to the stable host surface.** The supervisor keeps the existing
   `hostService` shape (`serve.go:16-21`) but `mux`/handler now proxies to the
   plugin: `memoryMux()` calls the MemoryStore client; `gwsTokenMux()`
   (`gwstoken.go`) calls the CredentialBroker client.
4. **Preflight** (unchanged intent, `serve.go:65-79`): call `Check()` on every
   enabled broker; the whole `serve` refuses to start if any fails, so a dark
   capability is loud up front.
5. **Health + restart.** A 30s ticker calls `Health()`/pings each plugin; go-plugin
   surfaces subprocess exit via `client.Exited()`. On crash: exponential-backoff
   relaunch (cap ~5, then mark the slot degraded and log loudly — mirror the
   memory "degrade loudly" ethos). A crashed plugin never takes down the kernel or
   a sibling.
6. **Shutdown.** `plugin.CleanupClients()` on SIGTERM kills every child; no
   orphaned subprocesses.

Transport is a **unix-domain socket** (go-plugin default on the host), not TCP —
nothing new listens on a routable interface, preserving the memory service's
loopback-only trust model (`memory.go` header).

### How this replaces `init()`-overlay

| today | target |
|---|---|
| `extraCommands` / `extraServiceFactories` maps (`main.go:30-35`) | deleted |
| overlay `overlay_*.go` self-register via `init()` (`docs/OVERLAY.md` §2) | overlay ships a **plugin binary**, referenced by path in config |
| `make link-overlay` symlink dance (`Makefile`) | deleted |
| `.gitignore services/host/overlay_*.go` + `check-open-core.sh` scanning host source | host-source scan retired (no overlay source ever enters the tree); skill/agent allowlist checks stay |
| public binary rebuilt to include overlay | public binary is released once; overlay binary is independent |

The open-core boundary gets *stronger*: private code is a separate binary in a
private repo, never a symlink into the public tree.

---

## 4. Version-coupled launcher (`pi-stack run`)

Add a `run` subcommand to the released binary. The binary embeds its version at
build time (`-ldflags "-X main.version=<V>"`, stamped by CI the same way
`publish.yml` already stamps `VERSION`/`spec.yaml`).

```
pi-stack run [workspace] [-- pi args…]
  → sbx run pi-stack \
       --kit "git+https://github.com/<user>/pi-stack.git#ref=v<version>&dir=pi-kit" \
       <configured extra --kit …> \
       <workspace> <configured skill mounts> \
       -- <pi args>
```

Because the kit is fetched **at the ref matching the binary's own version**, and
`spec.yaml` at that ref pins `image: …/pi-stack:<version>` (it already does, see
`pi-kit/spec.yaml` `image:` + the `publish.yml` bump job), the host binary, the
kit, and the image are the same version **by construction**. Skew is impossible,
not merely discouraged.

Dev is untouched: `bin/pi-stack --dev` and `make run` stay Mode B (skills live
from the tree). `pi-stack run` is the consumer/baked path only.

### Load-bearing unknown (VERIFY before building)

Two facts are false today and must be made true:

1. **CI does not tag releases.** `publish.yml` computes `0.0.<run_number>`, builds,
   pushes `:<version>`+`:latest`, and commits a version bump to `main` — but there
   is **no `git tag`** anywhere in it. `run --kit #ref=v<version>` has nothing to
   resolve. Fix: add a `git tag v<version> && git push --tags` step to the `bump`
   job.
2. **The git-kit URL ref syntax is unconfirmed.** README/Makefile only ever use
   `git+https://…#dir=pi-kit` (no ref). Whether sbx parses `#ref=<tag>&dir=pi-kit`
   (or `#dir=pi-kit@<tag>`, or a `?ref=`) is unverified. This must be spiked
   against the actual sbx kit-URL parser before committing.

Fallback if ref-pinning is unsupported: have `pi-stack run` pass
`--template docker.io/<user>/pi-stack:<version>` explicitly (the mechanism
`make run` already uses via `out/.local-image-tag`, `Makefile` `run` target) and
fetch the kit from `main`. That still couples the *image* to the binary; it only
loses kit-file coupling, which is a smaller blast radius.

---

## 5. Consumer config: skills, kits, plugins, pin

Promote everything currently hardcoded in `bin/pi-stack` (the `--dev` block,
`bin/pi-stack:44-54`; the overlay-kit line, `bin/pi-stack:39`) and `Makefile`
(`DEV_SKILLS`, `OVERLAY_KIT_FLAG`) into one consumer file the launcher reads:
`~/.pi-stack/config.toml`.

```toml
version_pin = "auto"            # "auto" = the binary's embedded version; or "0.0.16" to override

[kits]
# Stacked in order after the pinned self-kit. Each maps to one `--kit`.
# Replaces the hardcoded single-overlay logic in bin/pi-stack.
stack = [
  "git+https://github.com/acme/pi-kit.git#dir=kit",   # a published third-party mixin kit
  "~/work/mykit",                                       # a local mixin kit
]

[skills]
# Consumer BYO skills WITHOUT cloning pi-stack. Each path is bind-mounted as an
# sbx workspace and passed to pi as `--skill <path>`. Editing on the host +
# `/reload` in pi is live — same hot-reload as Mode B, minus the repo.
paths   = ["~/dev/my-skills"]
replace = false                 # false = add to baked; true = `--no-skills` first

[plugins]
memory = "builtin"              # or "acme" → external binary pi-stack-plugin-memory-acme

  [plugins.credential.gws]
  impl = "builtin"
  port = 11441

  [[plugins.credential.external]]
  name = "warehouse"            # → :11442 broker, overlay-supplied
  path = "~/work/bin/pi-stack-plugin-warehouse"
  port = 11442

  [plugins.mcp.slack]
  impl = "builtin"              # or path to an override binary
```

### Skills for a consumer (task item 3)

`pi-stack run` reads `[skills]`: for each path it appends a bind-mount workspace
and a `--skill <in-sandbox-path>` arg, and prepends `--no-skills` iff
`replace = true`. This is exactly the transformation `bin/pi-stack --dev` does
(`bin/pi-stack:44-54`) and `make run`'s `DEV_SKILLS` does, but exposed as a
first-class consumer flag/config instead of a repo-only affordance. A `--skills
<dir>` CLI flag (repeatable) overrides the config for one run. Hot reload is free
because sbx mounts are live.

### Kits for a consumer (task item 4)

`pi-stack run` reads `[kits].stack` and emits `--kit <self, pinned>` followed by
one `--kit` per entry, in order. A `--kit <ref>` CLI flag (repeatable) appends
more for one run. This generalizes the single hardcoded overlay kit
(`bin/pi-stack:39`, `Makefile` `OVERLAY_KIT_FLAG`) to N kits with no code change
per kit. Publishing a kit is unchanged from what README already documents: a git
repo (or dir) with `spec.yaml` `kind: mixin` and a `files/` tree; the consumer
just lists its URL. sbx already supports N `--kit` flags — pi-stack simply stops
assuming exactly one overlay.

---

## 6. Migration path

Phased, each phase shippable and reversible. Host and launcher migrate
independently.

**Phase 0 — spike the unknowns (no code committed).**
Verify the sbx git-kit ref syntax and add the CI `git tag` step (§4). If ref-pin
is unsupported, adopt the `--template` fallback. Gate all of Phase 3 on this.

**Phase 1 — introduce the plugin package + built-in self-exec, memory first.**
Add `pi-stack/host/plugin` (handshake, three interfaces, gRPC protos). Add
`pi-stack plugin memory` self-exec that serves the existing `memStore` over
gRPC. Rewrite `memoryMux()` to *optionally* proxy to a plugin when
`[plugins].memory != "builtin"`, else run in-process as today. Ship. Nothing
sandbox-side changes (still JSON-RPC :11435).

**Phase 2 — brokers + MCP.**
Same treatment for `CredentialBroker` (gws) and `McpServer` (slack). Replace the
`slack` subcommand registration with the `mcp <name>` bridge; update
`make mcp-register` accordingly. Delete `extraCommands`/`extraServiceFactories`
and `link-overlay` once the overlay has migrated its `overlay_*.go` to a plugin
binary (document in `docs/OVERLAY.md`).

**Phase 3 — the launcher + config.**
Add `pi-stack run` and `~/.pi-stack/config.toml`. Move the `--dev`/overlay/skills
logic out of `bin/pi-stack` for the *consumer* path; keep `bin/pi-stack --dev` and
`make run` as the dev loop. Document the consumer path in README (replacing the
raw `sbx run --kit git+…` snippet).

**Phase 4 — retire the old seam.**
Remove overlay host-source scanning from `check-open-core.sh` (keep skill/agent
allowlist checks). Overlay is now purely: a mixin kit + separately-built plugin
binaries listed in config. Update `docs/OVERLAY.md` §2.

Rollback: each phase keeps the in-process built-in path, so reverting a plugin is
config (`impl = "builtin"`), not a redeploy.

---

## 7. Top 3 risks + mitigations

**Risk 1 — Version coupling rests on two things that don't exist yet.**
CI never tags (`publish.yml` has no `git tag`), and the git-kit ref syntax is
unverified (README/Makefile only ever use `#dir=pi-kit`). If either is wrong,
`pi-stack run` can't pin.
*Mitigation:* Phase 0 gate — spike the sbx URL parser and add the tag step before
any launcher work. Fallback: pin the **image** via `--template :<version>` (proven
in `make run`) and fetch the kit from `main`; smaller coupling, still no image
skew.

**Risk 2 — go-plugin puts a subprocess + gRPC hop on the memory hot path.**
Recall runs on *every* turn (recall extension → :11435). An extra local round-trip
and a supervised subprocess add latency and failure modes.
*Mitigation:* unix-socket gRPC is sub-millisecond; the kernel holds one persistent
client, not per-request dials. Health-check + backoff restart keeps a crashed
plugin from wedging the kernel. And the built-in default can run **in-process**
(the Phase-1 optional-proxy design) so the common path pays zero plugin cost;
out-of-process is only forced when a slot is overridden. Benchmark recall p99
before/after as a Phase-1 exit criterion.

**Risk 3 — launching third-party plugin binaries widens host trust, and
"parent spawns child, talks over a socket" is the EDR-tripping shape AGENTS.md
warns about.**
*Mitigation:* it stays compiled-Go-spawns-compiled-Go over a **unix socket**
(not an interpreter spawning from *network* input — the specific pattern EDR
flags, per AGENTS.md), with a magic-cookie + mutual-TLS (`AutoMTLS`) + protocol
handshake, no default network listen. Plugins are local files the user placed —
identical trust to overlay Go today, just unlinked. Harden further: pin plugin
paths + SHA-256 in config and refuse an unpinned/mismatched binary; keep the
loopback-only bind for the kernel's HTTP surfaces (`memory.go` trust-model
comment).

---

## Files this touches (for the implementer)

- `services/host/main.go` — delete `extra*` maps (`:30-35`), add `run` / `plugin` /
  `mcp` subcommands to the switch (`:44-62`).
- `services/host/serve.go` — rewrite `runServe` into the supervisor; keep the
  `hostService`/preflight shape (`:16-21`, `:65-79`).
- `services/host/memory.go` — `memoryMux()` proxies to MemoryStore; extract
  `memStore` behind the interface.
- `services/host/gwstoken.go` — `gwsTokenMux()`/`gwsTokenCheck()` behind
  CredentialBroker.
- `services/host/slack.go` + `services/host/util.go` — `mcpStdio`/`mcpDispatcher`
  (`:186`,`:137`) become the `mcp <name>` bridge to McpServer.
- new `services/host/plugin/` — handshake, interfaces, gRPC protos.
- `Makefile` — drop `link-overlay`; update `mcp-register`, `serve`.
- `bin/pi-stack` — keep `--dev`; consumer logic moves to `pi-stack run` + config.
- `.github/workflows/publish.yml` — add `git tag v<version>` + ldflags version
  stamp.
- `docs/OVERLAY.md` — rewrite §2 (host half = plugin binary, not `overlay_*.go`).
- `scripts/check-open-core.sh` — retire host-source overlay scan; keep allowlists.
```