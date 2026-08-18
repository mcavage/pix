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
The silent auto-injection path used to also drop a low-scoring **perishable**
hit below a 0.30 floor. That filter was deleted along with the write-side
perishable/TTL behavior it existed to police: every row pix writes is durable
now (see Legacy data below for a store with rows written before that was
true), so there was nothing left for a durability-based floor to filter.

The injected block itself says as much: it tells the model this is a
relevance-filtered subset from the host daemon, not the full store, and to use
`memory_recall` to inspect the store directly.

**"Everything" is capped at 100 rows, not unbounded.** A blank `/recall`, or
`memory_recall` with its default (or explicit) `*` query, asks the daemon for
up to 100 rows (with a large enough `charBudget` that the daemon's normal
1200-character response cap doesn't cut it short first), not a true
unbounded dump of the store. If the store has more than 100 visible rows, the
response says so with a truncation line rather than silently showing a
partial page as if it were everything. An explicit non-`*` search query's
default is **not the same number everywhere**: the `memory_recall` agent
tool defaults to 6 hits (tuned for the silent per-turn injection path, which
uses the same small number); the `/recall` slash command sends no explicit
`limit` for a non-`*` query, so it falls through to the daemon's own default
of 8; and `pix memory recall <query>` (the host CLI) has its own `--limit`
flag defaulted to 8, landing on the same number by a different route. Neither
is a security boundary, just a relevance tuning choice, and each is
overridable (`memory_recall`'s `limit` param, or `pix memory recall --limit
N`).

## How capture works

**Capture is explicit by default.** `memory_capture` (a `pix config` key) has
two values:

- **`explicit` (the default).** No automatic observation at all. The
  `memory-capture` extension sends **zero** `observe` requests. It decides
  this from a launch-scoped marker file, `.pix/memory-capture` — the
  launcher writes it at launch from `memory_capture`, and the extension
  reads it once, at load, exactly like `.pix/profile` — so there is no RPC
  round trip involved in the decision at all. Even if something called
  `observe` anyway, the daemon refuses it before ever touching the
  watcher: no watcher inference, no side-effect Ollama probe. **The host
  is always authoritative, but the two directions take effect on different
  schedules**: setting `explicit` takes effect *immediately*, even for an
  already-running sandbox — `memObserve` re-reads the live host config on
  every call and refuses there regardless of what the sandbox's marker
  says. Enabling `experimental-auto` only reaches a sandbox's marker at
  launch: an already-running sandbox's `memory-capture` extension still
  checks the marker it launched with before ever placing the `observe`
  call, so a marker stuck on `explicit` goes on sending zero requests
  until that sandbox is recreated, even though the host would now accept
  them. Facts still land the way they always could: `/remember`,
  `pix memory remember`, or the agent's own explicit tools — a human or an
  explicit command chose to store it, not an automatic listener.
- **`experimental-auto` (opt-in).** The watcher extracts durable **facts**
  (preferences, decisions, project conventions, no automatic expiry) and
  **corrections** (the agent got something wrong and you told it so), and
  writes them straight to memories with internal `source="watcher"`. A
  correction is stored with the row `kind` set to `learning`, not
  `correction` — that reuses the schema's pre-existing `kind` vocabulary
  (the same one the now-deleted pix memory learnings / /learnings command
  once read) rather than adding a new one. This is naming, not a leftover of
  that deleted command: `pix memory stats`'s `learnings` count **is** the
  count of captured corrections, and the row's `kind` is stored, and any
  `--json` output emits it, as `learning` verbatim, never `correction`.
  `/recall` and `pix memory recall` render that same stored `learning` kind
  as `correction` for a person reading the line (a render-only alias, no
  schema migration: see DX-6a) — so the internal/JSON value and the
  human-facing label are deliberately different words for one row. Under
  **one fixed daily budget: at most 10 STORED rows/day** (UTC calendar
  day), counted by a real `SELECT COUNT(*)` over `memories` rows with
  `source='watcher'` created today — not by counting `observe` attempts, so
  it survives a daemon restart exactly, and an empty/noise-filtered/
  secret-filtered watcher call never costs anything against it. The budget
  is peeked *before* the watcher is ever invoked (at `observe` admission,
  and again fresh right before the actual watcher call), so an exhausted
  day costs zero inference; if a single watcher result would extract more
  items than remain, only the remaining rows are stored and the rest are
  dropped (logged as a count, never content). **Only a row that actually
  lands NEW counts against the budget**: an item that reaffirms an existing
  row (same content hash) or collapses into one via the embedding-similarity
  dedupe path — either way, the same `reaffirmed` outcome `remember` already
  reports — or that fails to store, is never counted, matching exactly what
  the persisted `SELECT COUNT(*)` would report; only a genuinely stored row
  moves the needle. A row that is later `/forget`-ed (soft-deleted) still
  counts against the day it was stored — forgetting is feedback on what's
  recalled, not a refund on capture volume, and only a new UTC day resets
  the count. Like the mode switch above, this budget lives on the host and
  is unaffected by anything sandbox-side. Budget exhaustion is an
  honest `{accepted:false, reason}`, not a silently dropped attempt. This is
  UX policy on an experimental feature (a sane cap, not a security
  boundary) — there are no session ids, maps, or per-session counters, just
  a SQLite COUNT.

Change it with `pix config set memory_capture <mode>`; it is daemon-affecting
(the running `pix-host serve` gets restarted, per its lifecycle mode, to pick
it up — the standalone `pix-host memory` daemon applies the same config->env
translation, so it is never silently ignored there either). **Enabling or
disabling the mode this way only reaches a *new* sandbox**: each sandbox
reads the mode once, at launch, into its own `.pix/memory-capture` marker, so
an already-running sandbox keeps whatever mode it launched with until it is
recreated (`pix config set memory_capture <mode>` itself confirms this in its
output). That marker can go stale in EITHER direction once the host config
changes after launch — stuck on `explicit` after the host moves to
`experimental-auto`, or stuck on `experimental-auto` after the host moves
back to `explicit`. Either way, EFFECTIVE capture behavior never drifts from
what the host's *live* config allows today: the marker only decides whether
the sandbox extension bothers sending an `observe` call at all, while the
host's own admission check (`memObserve`) re-reads its live config on every
call and is authoritative regardless. So a marker stuck on
`experimental-auto` can cause a harmless `observe` attempt the host then
refuses, and a marker stuck on `explicit` can suppress an attempt the host
would have accepted — but no combination of a stale marker and the current
host config ever stores a row the host's live setting would not have
allowed. `pix config unset memory_capture` restores `explicit`, same
new-sandboxes-only rule.

There is no review/staging mode: automatic capture is the experiment, and a
review-before-store workflow is deferred until evidence says it's needed. The
feedback/undo mechanism today is the existing `/forget <id>` — a `/recall` hit
names its `source`, and the sandbox/CLI render an `auto` tag when it's
`watcher`, so an auto-captured row is visibly distinct from an explicit one.
There is no bulk revoke and no new verb: `/forget` by id is it.

External `remember` (the RPC/plugin surface, `/remember`, `pix memory
remember`) is always explicit and can never claim `source="watcher"` — an
external caller trying to spoof it is normalized to `"unknown"` instead,
regardless of which capture mode is live.

**A conservative, two-stage secret filter runs in capture only** (never on
an explicit `remember`): once before the watcher ever sees your message, and
again before any extracted fact/correction is stored. A match — a private
key block, a recognizable vendor token shape (AWS, GitHub, Slack, OpenAI,
Stripe, Google, a JWT, a labeled `api_key=`/`token=`/`password=` assignment,
including a realistic `SCREAMING_SNAKE_CASE` env-var name like
`AWS_SECRET_ACCESS_KEY=`), a 1Password reference (`op://vault/item/field`,
treated as secret-shaped even though it's a LOCATOR rather than the value
itself — it still names exactly where to go fetch one), or a long unbroken
high-entropy run — drops the content entirely (fails closed: never a
partial or redacted store). An
all-hex run (a git commit SHA, a content digest) is deliberately NOT
treated as secret-shaped: it is indistinguishable from an ordinary hash by
shape alone, and flagging every SHA a user types is a false positive this
filter cannot tell apart from a real secret. Neither the matched text nor
the raw watcher output is ever logged — a parse failure logs only the
model, the error, and the content length.

**This is a best-effort heuristic, never a guarantee.** A secret with no
recognizable shape (no vendor prefix, no labeling keyword, not high-entropy
enough) can still slip through. Do not rely on it as your only safeguard
for anything sensitive.

## Legacy data

An older pix watcher also extracted perishable, time-bound **events** ("doing
X right now") that expired after 7 days, and seeded a small reward from a
sentiment score. Both are gone: the watcher only emits facts and corrections
now, and nothing it writes ever expires. That leaves one loose end for a
store that predates the change — a live perishable row waiting to expire,
with nothing left to ever expire it — which the schema v2 migration below
retires as part of its one-time sweep. The `reward` column itself is
untouched by any of this: it stays in the schema, always 0, ignored by
recall's scoring.

Two precision guards run before anything is stored:

- **Question-only messages never produce facts.** If your entire message is
  substantively question(s) ("so are you using my memories?"), any "fact" the
  watcher still tried to extract from it is discarded, a question asserts
  nothing about you. This does not apply to corrections: "Can you stop using
  em dashes?" is a real, capturable rule even though it's phrased as a
  question.
- **A conservative noise filter** drops watcher output that's session
  narration rather than a durable thing worth recalling ("user asked about
  X", "user ran the tests"), applied to **facts only**, matched by prefix so
  it never eats legitimate content that happens to mention "the user"
  mid-sentence. **Corrections are never filtered**: a correction can
  legitimately be phrased exactly like a noise prefix ("the user requested
  the agent stop doing X" is a real, capturable rule, not narration).

## Schema v2: a one-time source sweep

Upgrading a pre-v2 store runs a small, one-time DATA migration — no new
column, no new concept, just a sweep of what's already there. In ONE
database transaction (`migrateMemorySchema`, `services/host/memory.go`),
three things happen: (1) the legacy `profile` column is added if a
pre-profile-scoping db still lacks it; (2) every LIVE row whose recorded
`source` is neither `user` nor `cli` (the watcher's past captures, or a
source pix has never seen) is **soft-deleted** — the exact same `deleted_at`
mechanism `/forget` already uses; (3) every LIVE row still carrying the
legacy `durability = 'perishable'` marker (see "Legacy data" above) is
**also soft-deleted** the same way — this used to be its own every-startup
query, now it's folded into this same one-time sweep. `PRAGMA user_version`
is then stamped to 2. A crash or error anywhere in that transaction rolls
back everything, leaving the store fully at its old version; completion is
judged by `user_version` alone, so the sweep runs exactly once per store,
and a row written after the stamp (including a brand new watcher capture) is
never touched by it again.

This is an ADVISORY, one-time reading of pre-v2 free-text history, never a
verified trust boundary: pre-v2 `source` was operator-set text with no
enforcement behind it. `user`/`cli` (an explicit ask, from `/remember` or
`pix memory remember`) survives outright; everything else predates any
verification of where it came from, so it's swept. An already soft-deleted
row is left exactly alone.

**Reversibility is nothing new, but it's two statements, not one.** Soft-delete
(`/forget`, and this sweep) always drops the row's FTS index entry alongside
stamping `deleted_at` — the pair that must always happen together, since a
row left in the index would still answer keyword searches after being
"deleted". So reviving it needs the same pair run backwards: clearing
`deleted_at` alone puts the row back in the active set (visible to `pix
memory recall '*'` and to vector-similarity recall if it still has an
embedding), but it stays invisible to plain keyword search until its FTS
entry is rebuilt too. Directly in the database file, with the service
stopped:

```sql
UPDATE memories SET deleted_at = NULL WHERE id = '...';
INSERT INTO memories_fts (rowid, content)
  SELECT rowid, content FROM memories WHERE id = '...';
```

There is no live undelete verb; this is a manual, service-stopped edit of
the db file, on purpose. If you want a point-in-time copy
before upgrading at all, run `pix-host memory snapshot` first (see "Backing
it up, and putting it back" below); the migration itself does not take one
automatically.

**Keeping `source` closed and un-spoofable.** The free-text `source` column
is normalized to a closed vocabulary (`user`/`cli`/`watcher`, else
`unknown`). `remember` — the RPC/plugin surface every external caller
reaches — additionally treats a caller-supplied `source="watcher"` as
spoofing: it normalizes to `unknown` instead of being stored verbatim. Only
the watcher's own internal capture path (`rememberWatcherCapture`, an
unexported Go call no request can reach) ever writes `source="watcher"`.

> A trust-state/provenance schema (admitted/proposed/quarantined rows, a
> named eligibility predicate, an automatic pre-migration snapshot) was
> designed and built for this upgrade, then rejected on review as more
> machinery than the actual problem warranted — see
> docs/design/self-learning-loop.md's "Rejected" note.

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

From the host, without launching a sandbox:

```bash
pix memory recall "<query>"
pix memory remember "<fact>"
pix memory forget "<query>"
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
model was unavailable at the moment of a real recall/remember call (there is no
startup probe: the store never blocks on Ollama just to construct itself).
The embedder **latches off on that failure** and, like the capture watcher,
re-probes automatically (once per `embedProbeInterval`, 60s) on the next
real call, so semantic recall recovers on its own — **no daemon restart
required**. `pix-memory identity`/`health` report the LIVE state (never a
boot-time snapshot), so a degraded reading always reflects what's true right
now.

`health`'s `vector`/`capture` fields are **tri-state**: `null` means "not yet
exercised" — a fresh daemon that has never actually attempted a real
embed/capture call, which is the normal state right after `pix serve` starts,
since construction makes no boot-time probe. It only becomes `true`/`false`
once a real attempt has happened; a brand-new daemon reporting `true` before
that would be a guess dressed up as a fact. `identity`'s `degraded_reason`
follows the same rule: it is only set on a CONFIRMED `false`, never on
`null`, so a daemon that simply hasn't been asked to embed anything yet is
not reported as degraded.

To restore a confirmed-degraded embedder immediately instead of waiting for
the next call:

```bash
ollama pull nomic-embed-text          # or whatever MEMORY_EMBED_MODEL names
```

The next recall or remember re-probes and, on success, semantic recall is back.

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

**Migrating off an old backup archive.** Before U07b there was a top-level
backup verb, and it wrote a versioned `tar.gz` bundling `memory.db` +
`config.toml` + `op-refs.env` + a manifest. The verb is gone — it now gets the
ordinary unknown-command answer — and it was never something you could "restore"
back into a running install anyway, since `config.toml`/`op-refs.env` don't need
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

By default the memory service (`:11435`) is **loopback-only and
unauthenticated**, the trust boundary is the `127.0.0.1` bind, nothing more.
The built-in daemon does **not** check a bearer token today (the JSON-RPC mux
is served directly), so for a shared or multi-user host the correct control
is an **authenticating reverse proxy in front of the service**, or keeping it
strictly loopback.

(There is no separate knowledge service to reason about here — the built-in
host knowledge daemon and its `:11436` port were deleted outright, not merely
turned off; `knowledge` is a capability a pack wires directly, `files` or
`http`, never through `pix-host serve`. See AGENTS.md's go-plugin + Suture
architecture note and `hostmode_gone_test.go`.)

The `MEMORY_AUTH` env var is the intended shared-secret hook: the design is
that, when set, the service requires the matching token as an
`Authorization: Bearer <token>` header on every JSON-RPC request and the
in-sandbox wrapper sends it. **Note:** the daemon-side enforcement of this var
is not wired in the built-in service yet, setting it alone does not lock the
port. Until it lands, rely on the loopback bind (and an auth proxy for shared
hosts), and treat `MEMORY_AUTH` as reserved.

## Environment knobs

| var | default | what |
| --- | --- | --- |
| `MEMORY_PORT` | `11435` | daemon port |
| `MEMORY_BIND` | `127.0.0.1` | bind address (keep it loopback) |
| `MEMORY_DB` | `~/.local/share/pix/memory/memory.db` | store path |
| `MEMORY_EMBED_MODEL` | (config) | Ollama embedding model for recall ranking |
| `MEMORY_WATCHER_MODEL` | (config) | Ollama model for capture extraction |
| `MEMORY_CAPTURE_MODE` | `explicit` | capture admission mode (`explicit`\|`experimental-auto`); set via `pix config set memory_capture`, never by hand |
| `OLLAMA_HOST` | Ollama default | where the daemon reaches Ollama |

The design reasoning lives in
[design/self-learning-loop.md](design/self-learning-loop.md).
