# Profiles: running work + personal contexts from one host

## Problem

A solo power-user runs pi-stack in more than one context — a **personal** one and
a **work** one — that differ in:

- the Google Workspace account `gog` serves,
- which MCP servers are attached,
- which OKF knowledge bundle(s) recall draws from,
- which overlay mixin kit is stacked onto the sandbox.

Before this change the only isolation seam was the `PI_STACK_CONFIG` env var (a
whole separate config file) and a single make-time `OVERLAY`. There was no
first-class way to say "run this as work" without hand-swapping config.

## Model

A **profile** is a named override set layered onto the base (flat) config. The
base config is the implicit `default` profile; `[profiles.<name>]` tables
override individual fields.

```toml
# ~/.config/pi-stack/config.toml
gog_account = "me@personal.com"       # base == the `default` profile
mcp = ["gog"]
knowledge_bundles = ["/kb/personal"]

active_profile = ""                    # empty = default

[profiles.work]
gog_account = "me@work.com"
mcp = ["gog", "slack"]
knowledge_bundles = ["/kb/work"]
[profiles.work.kits]
stack = ["../work-overlay/kit"]
```

**Override semantics:** a slice field that is *present* (even empty `[]`)
**replaces** the base value; an *absent* (nil) field **inherits** it. A non-empty
`gog_account` overrides; empty inherits. This lets a profile disable MCP entirely
(`mcp = []`) or inherit the base set (omit `mcp`).

## Resolution

`config.Config.Resolve(name) *Config` returns a **flat** config with the named
profile's overrides applied, so every existing consumer (`run`, `serve`,
`doctor`, `status`, `buildSbxArgs`) keeps working against a plain flat config and
never learns about profiles. `Resolve` copies the receiver and never mutates it.

Active-profile precedence (highest first):

1. `--profile <name>` flag (global; works before or after the subcommand)
2. `PI_STACK_PROFILE` env var
3. `active_profile` in config (`pi-stack profile use <name>` sets this)
4. `default`

## What is and isn't profile-aware

| surface | profile-aware? | how |
|---|---|---|
| `run` | yes | resolves the profile, composes the profile's `kits`/`mcp`/`gog`, and namespaces the sandbox name (`pi-stack-<dir>-<profile>`) so contexts never collide |
| `doctor` | yes | resolves + prints the active profile; probes that profile's gog/mcp |
| `status` | yes | header shows the active profile; shows the resolved bundles/mcp |
| `serve` | **no (by design)** | indexes the **union** of all profiles' knowledge bundles into one shared index; per-profile scoping happens at *query* time via the `.pi-stack/knowledge.scope` file `run` writes |
| memory store | **no (MVP gap)** | one shared `:11435` store across profiles — see below |

### Why `serve` indexes the union

`serve` is a single long-running supervisor; making it profile-aware would mean
one daemon per profile (port contention, lifecycle sprawl). Instead it indexes
every profile's bundles once, and a launched sandbox scopes recall to its
profile's subset at query time. `config.AllKnowledgeBundles()` returns the
de-duplicated union.

### The memory-isolation gap (accepted for MVP)

Memory (`:11435`) is a single store shared across profiles, so a fact captured
under `work` is recallable under `personal`. For a solo user this is usually
fine (it's all *you*), but it's a real leak for hard separation. **v2:** tag each
memory row with the capturing profile and filter recall by it (mirrors the
knowledge scope-file mechanism). Deferred to keep the MVP to config + resolution.

## Overlay host plugins can't be per-profile

Overlay **host** plugins (`overlay_*.go`) are symlinked into `services/host/` and
compiled **into the single `pi-stack-host` binary** at build time. A runtime
`--profile` flag cannot swap compiled-in code. Two overlays coexist in one binary
only if their `init()` registrations (command names, service names, ports) don't
collide.

The runtime-swappable host route is the SHA-pinned `[plugins.<slot>]` external
binary override, which `serve` reads at startup. Everything the profile model
swaps (kit, mcp, gog, knowledge scope) lives in the **runtime** half — the
sandbox side — so the MVP never touches the binary. Per-profile host plugins are
out of scope; use `PI_STACK_CONFIG` + a distinct `serve` if you truly need two
different compiled host stacks.

## Known limitations

- **Shared `.pi-stack/profile` per workspace.** Two sandboxes launched on the
  SAME workspace under different profiles share the one launcher-written
  `.pi-stack/profile` file — the later launch wins and silently reassigns the
  first sandbox's recall/capture scope (the exact same constraint as
  `.pi-stack/knowledge.scope`). The in-VM extensions read the file EXACTLY ONCE
  at load and freeze it, so recall and capture never diverge within a single
  session, but they can't detect a mid-session overwrite by a sibling sandbox.
  Per-sandbox immutable profile identity (e.g. an env var stamped at creation,
  independent of the shared file) is a future improvement.
- **Plugin-backed memory stats is unscoped.** The built-in (in-process) memory
  store scopes `stats` to `{active}∪{default}`, but the go-plugin
  `MemoryStore.Stats()` interface takes no profile arg and is left unchanged to
  avoid breaking the external plugin contract, so a plugin-backed store reports
  the whole-store view. Recall/remember/forget/promotable/observe ARE profile-
  scoped on both paths.

## Secrets (op-refs.env) are profile-shared

`op-refs.env` (the 1Password refs the gateway resolves for host MCP servers)
resolves to a SINGLE XDG path, while `config.toml` is profile-aware. So every
profile's MCP set shares one refs file. This is acceptable because the file holds
only `op://` *references*, not secret values — the real authorization boundary is
the **1Password vault ACL**, not this file (a ref only resolves if the host
operator's `op` session can read that vault). Keep work and personal creds in
separate 1Password vaults if you want hard segregation. Per-profile op-refs is a
documented **v2 gap** (mirroring the shared memory store); the fix would key the
refs path off the resolved profile. See the `secret` verb + doctor's "Secrets
(1Password)" group for how this surfaces.

## Migration

Zero-touch. A config with no `[profiles.*]` tables and no `active_profile`
behaves exactly as before — the base config *is* the `default` profile.

## Commands

```
pi-stack profile ls              list profiles (* = active) with their resolved gog/mcp/bundles
pi-stack profile use <name>      persist active_profile (use `default` to revert)
pi-stack --profile work run      one-off run under a profile
pi-stack --profile work doctor   diagnose a profile
PI_STACK_PROFILE=work pi-stack   status for a profile
```
