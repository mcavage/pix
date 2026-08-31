# Memory

Memory is a separate service, **`pix-memory`**: a Go MCP server, built and
tagged independently of the `pix-agent` sandbox image
(`docs/design/pix-v2-architecture.md` section 9). `pix setup` reconciles it
as one named Docker container:

```text
name:    pix-memory
image:   the immutable digest from the release manifest
restart: unless-stopped
publish: 127.0.0.1:<port>:8080   (host loopback only)
mount:   ~/.pix/state/memory:/data
```

The sandbox never dials that container directly. The sbx MCP Gateway
registers its `/mcp` Streamable HTTP endpoint as an ordinary remote MCP
server, the same as any other integration; `/healthz` is a separate,
non-MCP liveness/readiness endpoint `pix doctor` probes.

**Pix has no top-level `memory` command in v2.** Memory is operated
entirely through MCP tool calls: the model's own tools, the slash commands,
and the deterministic lifecycle hooks all reach the same Gateway-registered
endpoint.

## The tool surface

| Tool | Semantics |
| --- | --- |
| `memory_recall` | relevance search, or a bounded `*` listing (up to 100 rows, with a truncation line if the store has more) |
| `memory_remember` | explicit durable insertion or reaffirmation |
| `memory_forget` | soft-delete by exact ID or unambiguous prefix |
| `memory_observe` | opted-in watcher extraction from one completed exchange |
| `memory_stats` | active, fact, correction, and deleted counts |
| `memory_status` | schema, embedding backend, capture mode, and readiness |
| `memory_snapshot` | a verified SQLite snapshot into `/data/backups` |
| `memory_restore` | a verified atomic restore, backing up the data it replaces |

`memory_recall` and `memory_stats` are ordinary model tools: the agent
reaches for them when you ask what is remembered. Snapshot and restore carry
accurate MCP annotations (destructive, non-idempotent where applicable) so a
model or a skill can reason about them correctly; restore in particular must
never be called casually.

## Slash commands

```
/recall <query>       # what memory would surface for this query
/remember <text>      # pin a fact immediately
/forget <id|query>    # drop a memory; id from /recall, or a query to drop its top match
```

**Capture is explicit by default**: nothing is written unless a human or an
explicit command asks for it. There is no background watcher writing memory
out of the box.

## Automatic capture (opt-in)

An environment's `pix.toml` may opt into a background watcher under
`[memory]`:

```toml
[memory]
scope = "work"
capture = "experimental-auto"
```

A watcher extracts facts and corrections from what you say, under one fixed
daily budget (10 stored rows/day). This only reaches a *new* sandbox: an
already-running one keeps the capture mode it launched with. A
watcher-captured row is tagged with an `auto` annotation on
`/recall`/`memory_recall`, visibly distinct from an explicit one, and
`/forget <id>` is the feedback/undo mechanism for it, same as any other row.

Extraction and embedding run on whichever local backend (llmman or Ollama,
authored directly in that environment's own `pix.toml`; there is no setup
interview that picks one for you) the environment declares, and never leave
the host, but recalled memory is not private from
your model provider: once a row is recalled, its content goes into the
prompt sent to whichever model is active (Claude, OpenAI, Gemini, or a local
model). Never store secrets, tokens, or credentials in memory.

Example: you tell pi your staging DB is `postgres://staging.internal:5432`
and say `/remember`. Next session, `/recall staging db` finds it, or it
surfaces unprompted in the system prompt when it is relevant. With
`experimental-auto` capture opted in, the watcher can extract and store the
same kind of fact on its own, without an explicit `/remember`.

## Durability

A durable fact (preference, decision, convention) has no automatic expiry:
every row `pix-memory` writes is durable, there is no perishable/TTL write
path. `/remember` is the reliable explicit write, driven by the human, not
the agent, though a stable fact the watcher captures on its own can also
land durable.

## What gets silently injected vs. what you can see

Each turn, only a small relevance-filtered subset is silently added to
context, through a `before_agent_start` hook calling `memory_recall` with a
strict short timeout. Every row is durable, so there is no score-based
durability floor filtering anything out. An explicit `/recall` or
`memory_recall` call skips that ranking filter entirely, but is still
capped: a blank query (or `*`) returns up to 100 rows, not a true unbounded
dump.

## Trust model and scope

Memory runs as one personal, per-machine service; it is not shared across
your laptop and your desktop, and not shared with teammates. Profiles are
organizational scopes in this local service, not security tenants: the
server enforces query/write scope on every request, but the same trusted
Gateway client can request another profile. A future multi-user or cloud
deployment adds real tenant authentication rather than treating a profile
argument as authorization; it does not change the MCP contract or Pi
extensions.

If the service is down, the commands and tools above surface a clear error
rather than failing silently; only the silent per-turn auto-injection
degrades quietly (no memory gets added that turn, so a dead container never
blocks the conversation). `pix doctor` reports the memory container's
health, storage, embeddings, capture mode, and scope isolation as one of its
probes; a wedged (but Docker-alive) container is the case Docker's
`unless-stopped` policy cannot catch on its own, so doctor prints the exact
`docker restart pix-memory` recovery command.

## Storage

Data lives at `~/.pix/state/memory` (`PIX_HOME`-relative), mounted into the
`pix-memory` container: SQLite plus FTS5, with embeddings on disk when a
local backend is configured. `memory_snapshot`/`memory_restore` are the
supported backup path; there is no separate `pix` CLI verb for it.

## Nothing here is an environment

Memory is personal, per-machine storage with no versioning and no sharing.
An environment (`~/.pix/envs/<name>`) is versioned, shared capability
context you would `git diff`; it can scope memory to itself via
`pix.toml`'s `[memory].scope`, but the store itself is never checked in or
shared by copying files.
