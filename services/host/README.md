# services/host — the `pix` launcher

The single compiled **Go** binary a user runs on the **host**. It is the only
Pix host binary and the only user-facing CLI: it resolves a named environment,
compiles it into one effective native `.sbxenv.yaml`, and runs `sbx env create`
/ `sbx exec` to launch the pinned `pix-agent` image.

**Convention:** host code is Go (one static binary); in-sandbox code (pi
extensions) is TypeScript. Why Go on the host: a single binary is saner to
ship, and a Node/Python interpreter that listens on a socket and spawns a child
process from network input is backdoor-shaped — endpoint security / EDR flags
exactly that. A compiled Go binary doing the same work runs unflagged.

There is **no `pix-host`**, no resident daemon, no `serve`, no pack system, no
model router, and no host-side memory process. `pix-memory` is a separate Go
module (`services/memory`) that runs as one Docker container and is reached
only through the sbx MCP Gateway; nothing in this module speaks a private
memory protocol.

## Command surface

Nine groups plus help/version — `pix help --all` is the generated source of
truth, and `docs/reference.md` §0 is the live capability map:

```
pix run [dir]              create or attach a sandbox for a workspace
pix ls                     list the pix-* sandboxes this host owns
pix rm NAME                proof-gated removal (never force, never foreign)
pix task {new,ls,path,rm}  disposable task checkouts
pix env {list,show,default,trust}
pix secret {list,set,rm,check}
pix setup                  initialize PIX_HOME, reconcile pix-memory, register its MCP name
pix doctor                 report, never repair
pix reset                  scoped teardown
```

A removed verb (`mcp`, `models`, `config`, `agent`, `pack`, `serve`, `resume`,
`status`, `uat`) gets the ordinary unknown-command answer. There is no
migration path because there are no released users.

## Layout

| package | owns |
| --- | --- |
| `cmd/pix` | the dispatch tree and the production adapters (docker, `sbx`, HTTP prober) |
| `pixhome` | PIX_HOME resolution + layout + `config.toml` (`pixhome.Machine`, the SOLE schema over that file) |
| `config` | the launcher-runtime config values (models, kits, skills, inference). It declares no services, no MCP servers, and no packs, and has no write path |
| `container` | the one named `pix-memory` container: spec, reconcile, per-PIX_HOME port, bearer token |
| `envinfo`, `workflow/env` | the native `.sbxenv.yaml` compiler, effective document, and BOM/trust fingerprint |
| `hosttrust` | HMAC-bound environment trust records, stored outside the environment |
| `sandbox`, `session`, `workflow/launch` | launch/attach/teardown, leases, and the lifecycle proofs |
| `secret` | `op://` references only, resolved through 1Password, never written to disk |
| `mcp` | the op-run wrapper grammar and Gateway readiness probes (no registration admin) |
| `health`, `workflow/doctor` | probes that report; nothing here repairs |

## Build and test

```bash
cd services/host && go build ./... && go test ./...
bash ../../scripts/gate.sh   # the fast PR gate CI runs
```

Host-only acceptance (Docker + `sbx`, cannot run inside a pix sandbox):
`scripts/host-uat.sh`.

## Config files

Everything user-owned lives under `PIX_HOME` (default `~/.pix`, overridable
with `$PIX_HOME`). There is no XDG split and no `$PIX_CONFIG`:

- `~/.pix/config.toml` — sparse explicit choices, primarily
  `default_environment` from `pix env default`
- `~/.pix/secrets.env` — the ONE secrets file, `op://` references only, 0600
- `~/.pix/.state/` — release identity, sandboxes, sessions, trust, memory data and port, effective documents
