# Private integrations (the overlay is retired)

> **The peer-repo "overlay" model has been removed.** It required building
> `pi-stack-host` with private Go symlinked in (`make ... OVERLAY=..`), which is
> exactly the coupling packs were meant to kill. Private company context now
> lives in a **pack**, and host-executing integrations ship as **containers** the
> sbx gateway runs (or, when host-only, a small host daemon). Nothing private is
> compiled into `pi-stack` anymore.

## Where each piece went

| Old overlay half | New home |
| --- | --- |
| `kit/files/.../skills/` (company skills) | the pack's `skills/` |
| `kit/.../capabilities.json` (routing) | the pack's `capabilities.json` (mounts to `~/.pi/agent/`) |
| in-sandbox `bin/` wrappers | the pack's `[[proxy]]` bin/ |
| `host/overlay_*.go` (host MCP servers) | a **container** MCP server, referenced by a pack `[[integrations]]` `image` (`docker run <ref>`, op-run wrapped) or `manifest` (`sbx mcp add --local --url`) |
| a host service that can't containerize (browser OAuth, host-only creds) | a standalone host daemon + installer, in a separate repo |

## How to add a private integration now

1. **Skills / routing / knowledge:** put them in a pack (`pi-stack pack new`,
   then `pi-stack pack use <path>`). Routing is a `capabilities.json` at the pack
   root. See [design/packs.md](design/packs.md).
2. **A remote MCP server:** `pi-stack config set mcp <name>` (gateway-catalog), or
   a pack `[[integrations]]` with `mcp = "<name>"`.
3. **A host-executing MCP server:** package it as an OCI image, then reference it
   from a pack `[[integrations]]` one of two ways:
   - `image = "<ref>"` (simplest) → pi-stack registers `docker run <ref>`, op-run
     wrapped like slack, forwarding `env`/`env_keys` into the container via `-e`.
     A locally-built image tag works — no registry or manifest hosting; push to a
     registry only to share it. Creds stay in 1Password (op-refs).
   - `manifest = "<server.json URL>"` → pi-stack registers
     `sbx mcp add <name> --local --url <manifest>`; the gateway resolves the image
     and runs it, with credentials provided Docker-side (declared in `server.json`).
   Either way, nothing private is compiled into `pi-stack`.
4. **A host-only service** (needs a browser/OAuth or host-cached creds that can't
   run in a container): ship a standalone host daemon + installer, and have the
   pack carry a thin in-sandbox `[[proxy]]` wrapper that forwards to it.

The Docker-employee reference implementation of (3) and (4) — BambooHR (container)
and the Snowflake exec proxy (host daemon) — lives in the private
`pix-docker-integrations` repo, referenced by the `gm-pix-pack` pack.
