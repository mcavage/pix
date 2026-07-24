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
| `host/overlay_*.go` (host MCP servers) | a **container** MCP server, referenced by a pack `[[integrations]]` `manifest` and run via `sbx mcp add <name> --local --url <manifest>` |
| a host service that can't containerize (browser OAuth, host-only creds) | a standalone host daemon + installer, in a separate repo |

## How to add a private integration now

1. **Skills / routing / knowledge:** put them in a pack (`pi-stack pack new`,
   then `pi-stack pack use <path>`). Routing is a `capabilities.json` at the pack
   root. See [design/packs.md](design/packs.md).
2. **A remote MCP server:** `pi-stack config set mcp <name>` (gateway-catalog), or
   a pack `[[integrations]]` with `mcp = "<name>"`.
3. **A host-executing MCP server:** build it as an OCI image with a `server.json`
   manifest, then reference it from a pack `[[integrations]]` with
   `manifest = "<server.json URL>"`. pi-stack registers it with
   `sbx mcp add <name> --local --url <manifest>`; the gateway runs the container
   on the host. Credentials are provided Docker-side (declared in `server.json`),
   never compiled in.
4. **A host-only service** (needs a browser/OAuth or host-cached creds that can't
   run in a container): ship a standalone host daemon + installer, and have the
   pack carry a thin in-sandbox `[[proxy]]` wrapper that forwards to it.

The Docker-employee reference implementation of (3) and (4) — BambooHR (container)
and the Snowflake exec proxy (host daemon) — lives in the private
`pix-docker-integrations` repo, referenced by the `gm-pix-pack` pack.
