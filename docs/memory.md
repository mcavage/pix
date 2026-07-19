# Memory

pi-stack remembers things across sessions. A durable fact you state once ("I
deploy from `main`, never a release branch") is recalled into a later session's
context automatically, so you don't re-teach the agent every time you open a new
sandbox.

This is one host service and two sandbox extensions. Here is what it is, how it
behaves, and where it stops.

## The shape

- **A host service.** `pi-stack-host serve` runs the memory daemon on
  `127.0.0.1:11435`, speaking JSON-RPC 2.0 over HTTP. It stores rows in a
  pure-Go SQLite database (`~/.local/share/pi-stack/memory/memory.db`) with an FTS5
  full-text index and a vector embedding per row.
- **Reached from the sandbox over `host.docker.internal`.** The VM never holds
  the store; it makes RPC calls to the host. The kit network allowlist permits
  that one host, nothing else.
- **Embeddings and capture run on local Ollama.** The daemon calls the host's
  Ollama for the embedding model (recall ranking) and the watcher model (fact
  extraction). No cloud provider sees your memory contents.

## How recall works

Before each turn, the `memory-recall` extension queries the daemon with the
current context and injects the most relevant memories. Ranking blends vector
similarity with FTS5 keyword match, then adjusts for:

- **Recency.** A 90-day half-life, so stale facts fade rather than compete
  forever.
- **Project match.** Memories tagged with the current project are boosted;
  memories from other projects are down-weighted, not hidden.
- **A relevance floor.** Below it, a memory is not injected at all. You get the
  few things that matter, not a wall of everything you ever said.

## How capture works

After a turn, the `memory-capture` extension sends the exchange to the watcher
model, which extracts durable facts: preferences, decisions, project
conventions, corrections. It stores them with a durability class and a
confidence score, de-duplicated by content hash. You never call it. Corrections
overwrite: tell the agent it got a fact wrong and the capture pass records the
fix.

## Driving it by hand

Inside the sandbox:

- `/recall <query>` — search memory and show what matches.
- `/remember <fact>` — store a fact now, explicitly.
- `/forget <query>` — soft-delete matching memories.
- `/learnings` — show what the watcher has captured repeatedly (the raw material
  the `promote` skill graduates into skills or conventions).

From the host, without launching a sandbox:

```bash
pi-stack memory recall "<query>"
pi-stack memory remember "<fact>"
pi-stack memory forget "<query>"
pi-stack memory learnings
pi-stack memory stats
```

If the daemon is down, the host commands exit with a clear message and code 3.
Start it with `pi-stack serve`.

## Without Ollama

Memory still works, degraded. Recall falls back to FTS5 keyword search (no vector
ranking), and automatic capture is disabled (there is no watcher model to extract
facts). `/remember` still works, because that is an explicit store, not an
extraction. Install Ollama and set the embed and watcher models
(`pi-stack config set memory_embed_model ...`, `memory_watcher_model ...`) to get
the full loop.

### When semantic recall is silently keyword-only

If recall has dropped to keyword-only even though Ollama is installed, the embed
model was almost certainly unavailable when the daemon started (or an embed call
failed once). The embedder **latches off on the first failure** and — unlike the
capture watcher, which live re-probes every 30s and self-recovers — it does **not**
retry, so semantic recall stays degraded for the life of the daemon process. Pull
the embed model, then restart the daemon so it re-probes at startup:

```bash
ollama pull nomic-embed-text          # or whatever MEMORY_EMBED_MODEL names
pi-stack serve stop && pi-stack serve  # restart so the embedder re-probes
```

A daemon-affecting `pi-stack config set` (e.g. `memory_embed_model`) already
restarts a managed or lazy daemon for you; only a foreground `pi-stack serve` must
be restarted by hand.

## Profiles

Memory is one shared store across profiles today. Recall is scoped per profile at
query time via the `.pi-stack/profile` file that `pi-stack run` writes, so a
`work` session does not surface `personal` facts. Tagging rows by profile at the
storage layer is a documented v2 gap, not done yet.

## Trust model

The daemon is **unauthenticated by design**. It binds loopback and assumes a
single user: your machine, your disposable VMs, your own store, so any sandbox
you launch may read and write it. Do not bind it to a routable interface
(`MEMORY_BIND`) or run it on a shared host without an auth proxy in front. See
[../SECURITY.md](../SECURITY.md).

## Optional authentication

By default the memory (`:11435`) and knowledge (`:11436`) services are
**loopback-only and unauthenticated** — the trust boundary is the `127.0.0.1`
bind, nothing more. The built-in daemons do **not** check a bearer token today
(the JSON-RPC mux is served directly), so for a shared or multi-user host the
correct control is an **authenticating reverse proxy in front of the service**, or
keeping it strictly loopback.

The `MEMORY_AUTH` / `KNOWLEDGE_AUTH` env vars are the intended shared-secret hook:
the design is that, when set, the service requires the matching token as an
`Authorization: Bearer <token>` header on every JSON-RPC request and the in-sandbox
wrapper sends it. **Note:** the daemon-side enforcement of these vars is not wired
in the built-in services yet — setting them alone does not lock the port. Until it
lands, rely on the loopback bind (and an auth proxy for shared hosts), and treat
`MEMORY_AUTH` / `KNOWLEDGE_AUTH` as reserved.

## Environment knobs

| var | default | what |
| --- | --- | --- |
| `MEMORY_PORT` | `11435` | daemon port |
| `MEMORY_BIND` | `127.0.0.1` | bind address (keep it loopback) |
| `MEMORY_DB` | `~/.local/share/pi-stack/memory/memory.db` | store path |
| `MEMORY_EMBED_MODEL` | (config) | Ollama embedding model for recall ranking |
| `MEMORY_WATCHER_MODEL` | (config) | Ollama model for capture extraction |
| `OLLAMA_HOST` | Ollama default | where the daemon reaches Ollama |

The design reasoning lives in
[design/self-learning-loop.md](design/self-learning-loop.md).
