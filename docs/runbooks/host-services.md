# Runbook: pix-memory host service

This runbook replaces the old pix `serve` host-services runbook. The Pix v2
cutover deleted `pix-host`, the Suture supervision tree, go-plugin, and the
`serve` verb outright (`docs/design/pix-v2-architecture.md` section 14):
there is no supervisor process left to page for, so nothing below names one.

## What runs

Each `PIX_HOME` stack owns one resident Docker container,
`pix-memory-<stack-id>` (`docs/design/pix-v2-architecture.md` section 9): a
Go MCP server published on host loopback only, with `unless-stopped` restart
and `$PIX_HOME/.state/memory` mounted writable. `pix setup` creates and
reconciles the active stack's container; nothing else supervises it.

The load-bearing property: Docker's restart policy recovers a **dead**
process automatically. It does not recover a **wedged** one that is still
running but failing its health checks, which is why `pix doctor` performs
its own behavioral probe (`/healthz` plus an MCP initialize/tool-list call)
rather than trusting the container's running state alone.

## Reading the signals

```bash
pix doctor                              # includes this stack's exact container name
docker ps --filter 'name=^pix-memory-'   # all Pix memory stacks
docker logs pix-memory-<stack-id> --since 15m
```

## Alerts -> response

### Recall/remember stopped working

1. `pix doctor` first: it names the exact gap (container not running,
   `/healthz` failing, MCP probe failing, or a schema/embedding mismatch)
   and the exact next command.
2. If the container is not running: run the exact
   `docker start pix-memory-<stack-id>` command doctor prints, or re-run
   `pix setup` to reconcile it from the pinned release manifest.
3. If the container is running but doctor reports it unhealthy, run the exact
   `docker restart pix-memory-<stack-id>` command doctor prints. Data lives on
   the mounted `/data` volume, not in the container's writable layer, so a
   restart never loses history.
4. If restart does not clear it: `docker logs pix-memory-<stack-id>` for the reason
   (a locked SQLite file, a local-inference backend that stopped answering,
   a full disk under `~/.pix/.state/memory`).

### Container will not start at all

Check the mount (`~/.pix/.state/memory` must exist and be writable by the
container's user) and the image digest doctor expects versus what is
present (`docker inspect pix-memory-<stack-id>` versus the release manifest doctor
reads). A mismatched digest means setup has not reconciled the container
since the last release; re-run `pix setup`.

## Recovery, in escalating order

```bash
pix doctor                              # 1. observe and get the exact scoped name
docker restart pix-memory-<stack-id>    # 2. restart only this stack's container
pix setup                               # 3. reconcile this stack from its manifest
```

Memory data lives in `~/.pix/.state/memory`, never in the launcher's own
state directory: restarting or reconciling the container never touches it.
`memory_snapshot`/`memory_restore` (called through MCP; see `docs/memory.md`)
are the supported backup path if you need one before a risky operation.
