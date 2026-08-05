# Memory

pix remembers things across sessions. A durable fact you state once ("I
deploy from `main`, never a release branch") is recalled into a later session's
context automatically, so you don't re-teach the agent every time you open a new
sandbox.

This is one host service and two sandbox extensions. Here is what it is, how it
behaves, and where it stops.

## The shape

- **A host service.** `pix-host serve` runs the memory daemon on
  `127.0.0.1:11435`, speaking JSON-RPC 2.0 over HTTP. It stores rows in a
  pure-Go SQLite database (`~/.local/share/pix/memory/memory.db`) with an FTS5
  full-text index and a vector embedding per row.
- **Reached from the sandbox over `host.docker.internal`.** The VM never holds
  the store; it makes RPC calls to the host. The kit network allowlist permits
  that one host, nothing else.
- **Embeddings and capture run on local Ollama.** The daemon calls the host's
  Ollama for the embedding model (recall ranking) and the watcher model (fact
  extraction), that part never leaves your machine.
- **But recalled memory is NOT private from your model provider.** Once a row
  is recalled (auto-injected each turn, or via `/recall` / `memory_recall`),
  its content goes into the prompt sent to whichever model is active -
  Claude, OpenAI, Gemini, or a local Ollama model. Only extraction and
  embedding are local; the recalled text itself is visible to your cloud
  provider like any other context. **Never store secrets, tokens, or
  credentials in memory**, treat it like anything else you'd type into a
  chat with that provider.

## How recall works

Before each turn, the `memory-recall` extension queries the daemon with the
current context and **silently injects a small, relevance-filtered subset** of
memories into the system prompt, not the full store. Ranking blends vector
similarity with FTS5 keyword match, then adjusts for:

- **Recency.** A 90-day half-life, so stale facts fade rather than compete
  forever.
- **Project match.** Memories tagged with the current project are boosted;
  memories from other projects are down-weighted, not hidden.
- **A perishable relevance floor.** A **perishable** (watcher-captured event)
  hit scoring below 0.30 is dropped from this silent injection entirely, it's
  the least trustworthy long-term signal, so it doesn't get to compete for
  space in every relevant turn. **Durable** hits are never filtered by score
  here. This floor applies only to the silent auto-injection path: an explicit
  `/recall`, or the agent calling the `memory_recall` tool, skips this score
  filter entirely.

The injected block itself says as much: it tells the model this is a
relevance-filtered subset from the host daemon, not the full store, and to use
`memory_recall` to inspect the store directly.

**"Everything" is capped at 100 rows, not unbounded.** A blank `/recall`, or
`memory_recall` with its default (or explicit) `*` query, asks the daemon for
up to 100 rows (with a large enough `charBudget` that the daemon's normal
1200-character response cap doesn't cut it short first), not a true
unbounded dump of the store. If the store has more than 100 visible rows, the
response says so with a truncation line rather than silently showing a
partial page as if it were everything. An explicit non-`*` search query keeps
the smaller default (6 hits) tuned for relevance search.

## How capture works

After a turn, the `memory-capture` extension sends the exchange to the watcher
model, which extracts three things: durable **facts** (preferences, decisions,
project conventions, no automatic expiry), perishable **events**
(time-bound status: what you're doing right now, what's installed today -
these expire after **7 days**), and **corrections** (the agent got something
wrong and you told it so; stored durably). Everything is stored with a
durability class and a confidence score, de-duplicated by content hash. You
never call it directly.

Two precision guards run before anything is stored:

- **Question-only messages never produce facts.** If your entire message is
  substantively question(s) ("so are you using my memories?"), any "fact" the
  watcher still tried to extract from it is discarded, a question asserts
  nothing about you. This does not apply to corrections: "Can you stop using
  em dashes?" is a real, capturable rule even though it's phrased as a
  question.
- **A conservative noise filter** drops watcher output that's session
  narration rather than a durable thing worth recalling ("user asked about
  X", "user ran the tests"), applied to **facts and events only**, matched
  by prefix so it never eats legitimate content that happens to mention "the
  user" mid-sentence. **Corrections are never filtered**: a correction can
  legitimately be phrased exactly like a noise prefix ("the user requested
  the agent stop doing X" is a real, capturable rule, not narration).

## Driving it by hand

**The agent itself has direct, typed tools**, `memory_recall` and
`memory_stats`, that call the host daemon directly (never by shelling out to
`pix` or `curl`). It uses `memory_recall` when you ask what's
remembered, how memory works, or whether it can see something (up to 100
rows, not the unbounded store, see the truncation note above);
`memory_stats` when you ask how much is stored. **This tool surface is
read-only:** writing and deleting are **human-driven slash commands**
(`/remember`, `/forget`), not agent tools, so the agent's normal way of
touching memory only ever reads it.

That is a UX/safety design choice on this tool surface, **not a security
boundary**, don't rely on it as one. The daemon is unauthenticated and
reachable at `host.docker.internal` (see [Trust model](#trust-model) below),
so any sandbox code capable of an HTTP POST, not just the typed tools above -
could still call its `remember`/`forget` RPCs directly. The read-only tool
surface keeps the agent's *intended* path from silently rewriting memory; it
does not, and cannot, stop arbitrary code execution from doing so.

Inside the sandbox, the write/delete operations are available as slash commands:

- `/recall <query>`, search memory and show what matches (blank = everything).
- `/remember <fact>`, store a fact now, explicitly.
- `/forget <id|query>`, soft-delete a memory by id or its top query match.
- `/learnings`, show what the watcher has captured repeatedly (the raw material
  the `promote` skill graduates into skills or conventions).

From the host, without launching a sandbox:

```bash
pix memory recall "<query>"
pix memory remember "<fact>"
pix memory forget "<query>"
pix memory learnings
pix memory stats
```

If the daemon is down, the host commands and the agent's tools/slash commands
all surface a clear error, they do not fail silently. Only the silent
per-turn auto-injection degrades quietly (a dead daemon just means no memory
gets injected that turn, so a stalled service never blocks the conversation).
Start the daemon with `pix serve`.

## Without Ollama

Memory still works, degraded. Recall falls back to FTS5 keyword search (no vector
ranking), and automatic capture is disabled (there is no watcher model to extract
facts). `/remember` still works, because that is an explicit store, not an
extraction. Install Ollama and set the embed and watcher models
(`pix config set memory_embed_model ...`, `memory_watcher_model ...`) to get
the full loop.

### When semantic recall is silently keyword-only

If recall has dropped to keyword-only even though Ollama is installed, the embed
model was almost certainly unavailable when the daemon started (or an embed call
failed once). The embedder **latches off on the first failure** and, unlike the
capture watcher, which live re-probes every 30s and self-recovers, it does **not**
retry, so semantic recall stays degraded for the life of the daemon process. Pull
the embed model, then restart the daemon so it re-probes at startup:

```bash
ollama pull nomic-embed-text          # or whatever MEMORY_EMBED_MODEL names
pix serve stop && pix serve  # restart so the embedder re-probes
```

A daemon-affecting `pix config set` (e.g. `memory_embed_model`) already
restarts a managed or lazy daemon for you; only a foreground `pix serve` must
be restarted by hand.

## Backing it up, and putting it back

`memory.db` is the one artifact here you cannot regenerate. It has exactly two
commands, both on the host binary:

```bash
pix-host memory snapshot ~/pix-memory-2026-08-10.db   # hot: safe while serve runs
pix-host memory restore  ~/pix-memory-2026-08-10.db   # cold: service must be stopped
```

**A snapshot is just a sqlite file.** It is written with `VACUUM INTO` through a
read-only handle, so it is a consistent, defragmented single file even while the
daemon is writing (the `-wal`/`-shm` sidecars are folded in, never copied). It
is verified before it lands (integrity check, schema version, and the real
`memories`/`memories_fts` queries), created `0600`, and never written over an
existing path. Retention, rotation and naming are yours: keep it wherever you
keep backups, and copy it like any other file. Nothing else rides along —
`config.toml` is reproducible with `pix config set`, and `op-refs.env` holds
`op://` pointers, not secrets.

**Restore is the stopped-service primitive.** Stop the daemon first:

```bash
pix serve stop
pix-host memory restore ~/pix-memory-2026-08-10.db --force
pix serve
```

It takes the same advisory lock the daemon holds, so if anything is still
serving the store it refuses and tells you to stop it — the lock, not a port
probe, is the authority, because the daemon opens the db before it binds. Then
it validates the snapshot, moves the current db and any `-wal`/`-shm` sidecars
aside to a kept `memory.db.bak-<ts>-<rand>` set (never deleted, so a restore is
reversible by hand), and renames the validated copy into place. Without
`--force` it refuses to touch an existing live db. The FTS index travels inside
the file, so keyword recall works the moment the daemon comes back — there is
nothing to rebuild.

**Migrating off an old `pix backup` archive.** Before U07b, `pix backup`
wrote a versioned `tar.gz` bundling `memory.db` + `config.toml` +
`op-refs.env` + a manifest. That top-level verb is retired now (it answers
`PIX_RETIRED`); it was never something you could "restore" back into a
running install anyway, since `config.toml`/`op-refs.env` don't need
restoring. If you're holding one of those old archives: untar it, take just
the `memory.db` inside, and hand it to `pix-host memory restore`. Recreate
`config.toml` with `pix config set` (or copy it back by hand if you trust its
provenance) and re-seed `op-refs.env` with `pix secret` — it only ever held
`op://` pointers, so there was nothing secret to lose. Nothing else in the old
archive is worth keeping.

## Memory scope (packs)

Memory is **one shared store by default**, every sandbox reads and writes the
same rows. There is no standalone "profiles" feature; scoping is a property of
the active **pack** (see `pix pack`). A pack's `memory_scope` in its
manifest controls it:

- **No explicit `memory_scope`** (the common case, including a bare pack name):
  memory stays the shared default. Adopting a pack does not silently wall off
  your memory.
- **An explicit `memory_scope = "work"`**: `pix run` writes it to
  `<workspace>/.pix/profile`, and recall/capture in that sandbox scope to
  `{that scope} ∪ {default}`, a scoped session sees its own rows plus the
  shared ones, but a different scope's rows stay invisible.

Rows are tagged by scope at the storage layer (a `profile` column), and recall
and the delete/forget path both restrict matches to what's visible to the
active scope, a session can't read or delete another scope's rows by
guessing an id.

## Trust model

The daemon is **unauthenticated by design**. It binds loopback and assumes a
single user: your machine, your disposable VMs, your own store, so any sandbox
you launch may read and write it. Do not bind it to a routable interface
(`MEMORY_BIND`) or run it on a shared host without an auth proxy in front. See
[../SECURITY.md](../SECURITY.md).

## Optional authentication

By default the memory (`:11435`) and knowledge (`:11436`) services are
**loopback-only and unauthenticated**, the trust boundary is the `127.0.0.1`
bind, nothing more. The built-in daemons do **not** check a bearer token today
(the JSON-RPC mux is served directly), so for a shared or multi-user host the
correct control is an **authenticating reverse proxy in front of the service**, or
keeping it strictly loopback.

The `MEMORY_AUTH` / `KNOWLEDGE_AUTH` env vars are the intended shared-secret hook:
the design is that, when set, the service requires the matching token as an
`Authorization: Bearer <token>` header on every JSON-RPC request and the in-sandbox
wrapper sends it. **Note:** the daemon-side enforcement of these vars is not wired
in the built-in services yet, setting them alone does not lock the port. Until it
lands, rely on the loopback bind (and an auth proxy for shared hosts), and treat
`MEMORY_AUTH` / `KNOWLEDGE_AUTH` as reserved.

## Environment knobs

| var | default | what |
| --- | --- | --- |
| `MEMORY_PORT` | `11435` | daemon port |
| `MEMORY_BIND` | `127.0.0.1` | bind address (keep it loopback) |
| `MEMORY_DB` | `~/.local/share/pix/memory/memory.db` | store path |
| `MEMORY_EMBED_MODEL` | (config) | Ollama embedding model for recall ranking |
| `MEMORY_WATCHER_MODEL` | (config) | Ollama model for capture extraction |
| `OLLAMA_HOST` | Ollama default | where the daemon reaches Ollama |

The design reasoning lives in
[design/self-learning-loop.md](design/self-learning-loop.md).
