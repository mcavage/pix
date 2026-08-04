# Self-learning loop

**Status:** built and working (steps 1 to 3). Remaining work in [Remaining (TODO)](#remaining-todo). The original design rationale is kept below the as-built summary; some details evolved during the build (JSON-RPC not MCP, capture on `before_agent_start` not `turn_end`, no staging area yet).

The memory system we had didn't learn. It stored things when the model
remembered to call a tool, which is almost never, so the same corrections came
back every session. This is how we fixed that in pi, betting on pi's extensions
instead of hoping the model behaves.

## As built

- **Store + host service + watcher** (`services/host/memory.go`, `memembed.go`): Go (`pix-host memory`), on pure-Go sqlite (modernc) with FTS5. Facts and learnings, a durable/perishable split, scored recall (relevance x confidence x recency x frequency x reward x project-boost), exact and semantic de-dup. One global store on the host, JSON-RPC over HTTP on `:11435` (not MCP: the only consumer is a pi extension doing an HTTP POST). The sandbox is a microVM with no arbitrary host bind-mounts, so the store lives on the host and sandboxes call it. Run it with `pix serve`. DB at `~/.pix/memory/memory.db`. The **watcher** (capture half) runs a local model (`qwen3.5:9b` by default — the SAME model the ollama-bridge/router uses, so Ollama keeps one model resident for both capture and local inference; override with `MEMORY_WATCHER_MODEL` / `pix config set memory_watcher_model`) over the user's message ONLY (never the agent's reply) and extracts durable facts, time-bound events, corrections, and a valence score. Conservative: questions and acknowledgments capture nothing. (Host code is Go by convention; see AGENTS.md.)
- **Extensions** (`extensions/memory-recall.ts`, `memory-capture.ts`): the loop, in the sandbox. Recall injects a small working set on `before_agent_start`; capture forwards the previous completed exchange on `before_agent_start` (reliable, pi awaits it) plus `agent_end` (best-effort for the last turn). They use `node:http`, NOT fetch: pi routes fetch through the sbx proxy, which can't reach the host store.
- **Embeddings**: local via Ollama (`nomic-embed-text`, `MEMORY_EMBED_MODEL`), optional, full-text fallback.
- **Always-on service** (implemented): the store runs under `pix serve`; `pix serve install` registers a managed login service (a launchd LaunchAgent — pix's host lifecycle is macOS-only), and `run` / `memory` / `knowledge` lazy-auto-start a detached serve when the ports are down (opt out with `PIX_NO_AUTOSERVE`). See docs/design/serve-lifecycle.md.

Verified end-to-end in a real sbx sandbox: recall returns seeded facts to the model, and stating a preference captures it automatically, tagged to the project.

## Remaining (TODO)

- **Promotion (step 4).** Synthesis runs already (a periodic cadence that distills captures and dedupes semantically — `store.synthesize` on a timer). What's not built is *promotion*: graduating a recurring lesson into a proposed, gated edit to a skill or convention. The original "staging area" collapsed into direct capture plus semantic de-dup for v1.
- **Reward attribution.** Valence seeds a small reward on new captures, but it does not yet reinforce the specific recalled memories that earned a good outcome. The "attach reward to what produced it" idea is not wired.
- **Entities.** Deferred. Facts and learnings only for now.

## The problem, with evidence

The prior loop ran on the model's discretion. Capture, recall, and synthesis
all wait for the model to choose to call a `memory_*` tool, and it mostly
doesn't. A full read of the system turned up about a dozen failure modes, all the
same shape: the model skips the tool call and the knowledge is gone.

The worst of them:

- Capture waits on the model calling `memory_remember()`. It forgets, the fact is lost, you repeat yourself next session.
- Recall is advice ("call recall FIRST"), not enforcement. When the model skips it, it answers from nothing and sounds confident doing it.
- Learnings surface as session-start warnings, never injected into the task where the workaround actually matters.
- Nothing is scored. Confidence defaults to 1.0, recency and frequency are unused, so a six-month-old guess ranks next to something you said this morning. Recall returns noise.
- A lesson learned five times never changes behavior. There's no path from "we hit this again" to "the agent stops doing it."
- Nothing tells the system which outcomes were good, so there's nothing to reinforce.

The storage underneath is fine and worth keeping as raw material: facts,
entities, learnings, docs, and todos, in SQLite with full-text and vector search.
The store isn't the problem. The loop around it is.

## The fix, in one line

The model never decides to remember or recall. The harness does it on every turn.
Skills stop being the engine and become manual overrides.

## Why pi can do this

pi's extension docs say it directly:

> TypeScript extensions can inject messages before each turn, filter history,
> wire RAG, or implement long-term memory.

Two hook points do the work. *Before Agent Start* lets an extension modify the
system prompt before the model runs, which is auto-recall. The turn and tool
events (`turn_end`, `tool_execution_end`, `after_provider_response`,
`session_shutdown`) are how we capture without asking the model to. The existing
extensions already use these, and they follow a defensive pattern where a hook
that throws can't break the agent.

## The loop

Each phase maps to a hook, and every one runs whether or not the model cooperates.

| Phase | Hook | What it does | Automatic |
|---|---|---|---|
| Recall | before_agent_start | Query the store with the prompt plus open files, inject a small scored working set into the system prompt | yes |
| Capture | turn_end + the watcher | Stage candidates from real signals | yes |
| Reinforce | on each recall | Bump frequency and access, decay by recency, flag contradictions | yes |
| Synthesize | scheduled + session_shutdown | Distill candidates into durable facts, dedupe, resolve contradictions, retire stale | yes |
| Promote | cadence, gated by the operator | Turn a recurring lesson into a proposed edit to a skill or convention | gated |
| Decay | TTL + recency | Expire caches, downrank stale, supersede on contradiction | yes |

## Scoring, the part that's missing today

Recall is only as good as its ranking, and right now there is no ranking worth
the name. The score should combine relevance (vector plus full-text), confidence,
a recency decay, frequency, and a reward term from the watcher. The working set
stays small on purpose: the top handful within a token budget, not a dump. A
stale inferred fact should never outrank something you stated this morning.

## Keeping it out of the junk drawer

Two rules do most of the work here.

First, candidates are staged, not committed. Capture writes to a staging area
with provenance and a confidence seed. Synthesis, on a schedule, is what promotes
a candidate into a durable fact, deduping and resolving contradictions on the way.
One offhand correction never becomes permanent truth.

Second, promotion is where the loop actually closes, and it's the part every
previous attempt skipped. When a lesson recurs past a threshold, it graduates
into a proposed edit to the relevant skill or convention doc, and you approve it.
That's the difference between the agent having notes and the agent getting better.
It stays gated so it never rewrites your skills behind your back.

## The watcher

Capture should be a fast local model reading each turn, not regex looking for the
word "no." This is also where we get the reward signal we've never had: how do
you know an outcome was good unless something judges it?

The watcher emits a few labels per turn, not one sentiment score: valence (were
you satisfied or frustrated), correction (did you reverse the agent), durable fact
(did you state something worth keeping), and friction (did a tool or process
break). It attaches each label to what produced it, so reward lands on the
approach that earned it, not on your mood in general.

Be honest about where this fails. People are often frustrated at the problem, not
the agent, and terse even when they're happy. So the watcher's output is a
candidate confirmed by synthesis, never a direct write. It runs async on
`turn_end` and never blocks the main agent, which is talking to a frontier API
anyway, so the local GPU isn't in the critical path.

On the model: don't pin a version blindly — confirm the actual tags on the machine
(`ollama list`). The watcher is a small local model in the Gemma or Qwen class, set
by env (`MEMORY_WATCHER_MODEL`) so it swaps the day a better one lands. Pick the
default by a bake-off on real turns (agreement with hand labels, latency, valid-JSON
rate), and confirm the actual tags with
`ollama list`, not from anything I remember. Embeddings run locally too
(`nomic-embed-text` class, also swappable). On the M5 two small models stay
resident for free.

## How it splits

The store and the loop are different things and live in different places.

- The host service (Go, `pix-host memory`, JSON-RPC over HTTP — not MCP) is the store: facts, learnings, embeddings, and the scored query. No loop logic.
- The extensions (TypeScript, in-sandbox) are the loop: the recall injector, capture plus the watcher, and the synthesis cadence. This is where the work moved off the model.
- The skills stay thin: `/recall`, `/remember`, `/forget`, and a `review-learnings` surface for promotion. Overrides and review, not the engine. This is the exact inversion of the earlier skills-driven approach, where the skills and the model *were* the loop, and that's why it failed.

## What happens to the old skills

| old skill | Fate | Why |
|---|---|---|
| remember / recall / forget | thin commands | the work moves into extensions |
| end-session | gone, becomes automatic | session_shutdown plus the synthesis cadence |
| context-save / context-restore | gone, pi does it | pi already has --session-dir, -c/-r, --fork, /tree. Memory holds knowledge, pi holds session state |
| improve-system / self-audit | merge into promotion review | this is the gated graduation surface |
| memory-status | a readout command | trivial |
| ingest | survives, thin | batched doc to facts, same store |
| todo | deferred | task tracking, separate concern |
| refresh-context / setup-user | ops skills | not part of learning |

Net: about thirteen skills collapse into four thin ones, three extensions, and one store.

## Open decisions

1. Entities: first-class now, or start with facts and learnings plus scoring and add entities only if recall quality needs them? I lean defer.
2. Promotion: always gated, or auto-apply the low-risk convention notes? I lean always gated until it earns trust.
3. Capture scope: all watcher signals on day one, or start with valence, correction, and explicit `/remember`, then add friction and fact-mining once we trust it? I lean start narrow.
4. Watcher default: decided by the bake-off, kept swappable either way.

## Build order

1. The TS memory store and the scored query.
2. The recall injector. This is the first visible win: the agent walks in already knowing things.
3. Capture, staging, and the watcher.
4. Synthesis and the promotion cadence.

Each one is testable on its own.

## Sources

- pi extensions docs: https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/extensions.md
- pi SDK docs: https://github.com/earendil-works/pi/blob/main/packages/coding-agent/docs/sdk.md
- AgentHarness API (DeepWiki): https://deepwiki.com/earendil-works/pi/7.2-agentharness-api-(pi-agent-core)
