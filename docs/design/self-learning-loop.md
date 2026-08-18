# Self-learning loop

**Status:** built and working (steps 1 to 3), plus `memory_capture`'s admission modes (see docs/memory.md's "How capture works"): capture is `explicit` by default (no automatic observation at all), with an opt-in `experimental-auto` (direct writes under one fixed daily budget). Automatic capture is the experiment; a review-before-store workflow is deferred until evidence says it's needed (see "Rejected" below). Remaining work in [Remaining (TODO)](#remaining-todo).

The prior memory system stored things only when the model remembered to call
a tool, which is almost never, so the same corrections came back every
session. The fix: the harness recalls and captures on every turn through pi
extension hooks, not on the model's discretion. Skills are thin overrides,
never the engine.

## As built

- **Store + host service + watcher** (`services/host/memory.go`, `memembed.go`): Go (`pix-host memory`), on pure-Go sqlite (modernc) with FTS5. Facts and learnings, scored recall (relevance x confidence x recency x frequency x project-boost), exact and semantic de-dup. One global store on the host, JSON-RPC over HTTP on `:11435` (not MCP: the only consumer is a pi extension doing an HTTP POST). The sandbox is a microVM with no arbitrary host bind-mounts, so the store lives on the host and sandboxes call it. Run it with `pix serve`. DB at `~/.pix/memory/memory.db`. The **watcher** (capture half) runs a local model (`qwen3.5:9b` by default — the SAME model the ollama-bridge/router uses, so Ollama keeps one model resident for both capture and local inference; override with `MEMORY_WATCHER_MODEL` / `pix config set memory_watcher_model`) over the user's message ONLY (never the agent's reply) and extracts durable facts and corrections. Conservative: questions and acknowledgments capture nothing. Every row written is durable with no expiry and no reward; the `durability`/`reward` columns stay in the schema, inert (docs/memory.md's Legacy data section covers the one-time schema-v2 retirement of rows an older binary wrote before this was true). (Host code is Go by convention; see AGENTS.md.)
- **Extensions** (`extensions/memory-recall.ts`, `memory-capture.ts`): the loop, in the sandbox. Recall injects a small working set on `before_agent_start`; capture forwards the previous completed exchange on `before_agent_start` (reliable, pi awaits it) plus `agent_end` (best-effort for the last turn). They use `node:http`, NOT fetch: pi routes fetch through the sbx proxy, which can't reach the host store.
- **Embeddings**: local via Ollama (`nomic-embed-text`, `MEMORY_EMBED_MODEL`), optional, full-text fallback.
- **Always-on service** (implemented): the store runs under `pix serve`; `pix serve install` registers a managed login service (a launchd LaunchAgent — pix's host lifecycle is macOS-only), and `run` / `memory` lazy-auto-start a detached serve when the ports are down (opt out with `PIX_NO_AUTOSERVE`). See docs/design/serve-lifecycle.md.

Verified end-to-end in a real sbx sandbox: recall returns seeded facts to the model, and stating a preference captures it automatically, tagged to the project.

## Remaining (TODO)

- **Promotion (step 4).** Not built: graduating a recurring lesson into a proposed, gated edit to a skill or convention. The on-demand near-duplicate merge that used to feed this (`store.synthesize`) was itself deleted (U9): no caller ever invoked it once the periodic ticker driving it was removed (U1-delete-go, "no background deletion"), so an orphaned surface with no path to promotion bought nothing.
- **Reward attribution.** Not wired. The `reward` column is inert (still in the schema, always 0, never read by recall). If this is revisited, it needs an actual "attach reward to what produced it" mechanism, not a proxy signal seeded from sentiment.
- **Entities.** Deferred. Facts and learnings only for now.

## Rejected: a trust-state/provenance schema

U5's first cut at the schema-v2 upgrade added a full trust model:
`trust_state` (admitted/proposed/quarantined) and `provenance`
(explicit/review/auto/legacy) columns, one named SQL eligibility predicate
ordinary recall/stats/dedupe/synthesis all filtered through, an automatic
hardened pre-migration snapshot (a second, fixed-path copy of
`memorySnapshot`'s own flow) before any DDL ran, and two new `stats` keys.
On final review this was more machinery than the actual problem warranted:
nothing yet reads `proposed` or a review queue (no capture mode or CLI
verb creates one), so the eligibility predicate spent a whole extra concept
(and a new file, `memory_schema.go`) buying a distinction nothing consumes.
It shipped, then was walked back to what the upgrade actually needs: a
one-time DATA sweep that reuses the store's EXISTING soft-delete
(`deleted_at`) semantics instead of inventing a parallel one, with no
automatic snapshot (an operator who wants a safety copy runs the
already-existing `pix-host memory snapshot` themselves). See docs/memory.md's
"Schema v2: a one-time source sweep" section for what shipped instead.

## Rejected: a review/staging capture mode

A `memory_capture=review` mode (staging watcher output in a separate,
disposable `memory_proposals` table for a human to `admit`/`revoke` later,
with its own TTL and live-queue cap) was drafted alongside `explicit` and
`experimental-auto`. It was cut before shipping: automatic capture itself is
the experiment worth running first, and a whole second table plus a
review/admit/revoke RPC/CLI/slash-command surface is a bet on a workflow
nobody has asked for yet. `explicit` and `experimental-auto` are the two
modes that ship; a review workflow is deferred until evidence (real usage of
`experimental-auto`) says it's needed. The feedback/undo mechanism for an
auto-captured row that turns out wrong is the existing `/forget <id>` —
`source` is exposed on a recall hit (an `auto` tag in the rendered line) so
that row is visible in the first place, with no new verb and no bulk revoke.

## How it splits

- The host service (Go, `pix-host memory`, JSON-RPC over HTTP — not MCP) is the store: facts, learnings, embeddings, and the scored query. No loop logic.
- The extensions (TypeScript, in-sandbox) are the loop: the recall injector and capture plus the watcher.
- The skills stay thin: `/recall`, `/remember`, `/forget`. Overrides, not the engine.
