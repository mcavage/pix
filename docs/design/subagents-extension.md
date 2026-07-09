# DESIGN: pi-stack subagents extension

A first-party subagent extension for pi that does not freeze. Replaces the
off-the-shelf `@tintinweb/pi-subagents` (disabled in the image; see
`docs/upstream/pi-subagents-hang-pi-0.80.md`) and the stock `subagent` example
that ships with pi.

## Problem

Parallel subagent trees are core to the workflow, but every existing option is
unstable in this stack:

- **`@tintinweb/pi-subagents`** drove a spawned agent through an in-process API
  that broke between pi 0.79 and 0.80: the agent registers but never runs a turn,
  and `get_subagent_result(wait:true)` parks the event loop forever. No self-audit
  ever passes; the only recovery is killing pi from the host.
- **pi's stock `subagent` example** spawns a child `pi --mode json` per task —
  robust in principle — but has two fatal gaps *in this sandbox*:
  1. It spawns the child with the **full extension set** loaded. In pi-stack the
     child re-loads `ollama-bridge` and the memory extensions, which
     `await new Promise(resolve => server.listen(port, resolve))`. The parent pi
     already holds those ports, so the child's `listen` emits `EADDRINUSE`, the
     error handler swallows it, `resolve` is never called, and **the child pi
     deadlocks at startup before it ever runs a turn** (verified: exits silently,
     ~280ms, zero output). This is the real "everything freezes" bug.
  2. **No timeout of any kind.** pi has no client read timeout, so a dead SSE
     stream (a known failure mode here) spins a child forever and the parent tool
     call never returns.
- It also **skips every pi-stack agent preset**: its discovery requires a
  `name:` frontmatter field, but our 17 agents use filename-as-name plus
  `thinking` / `max_turns`, so `discoverAgents` finds none of them.

## Approach — "proven, better, new" (Pincus)

**Proven** (keep what works, from pi's example + Claude Code + opencode):
subprocess isolation (one child `pi` per task, isolated context window), markdown
agent definitions, single / parallel / chain modes, live streaming of tool calls
and text, per-agent usage + cost, Ctrl+C abort propagation, markdown-rendered
final output.

**Better** (fix what's broken here):

1. **Stability pillar #1 — curated child extension set.** Spawn the child with
   `--no-extensions -e <this-extension>`. `--no-extensions` drops auto-discovery
   (so the port-binding extensions never load and never deadlock), while the
   explicit `-e` re-adds *only* this subagent extension — which is also what makes
   trees possible.
2. **Stability pillar #2 — a real watchdog.** Every child has an **inactivity
   timeout** (no stdout event for N seconds → kill) and a **hard wall-clock cap**.
   On either, SIGTERM then SIGKILL, and return a clear error result to the parent
   model. A subagent can be slow; it can never hang the parent.
3. **Correct discovery.** Filename is the agent name (matches skills and our 17
   presets); parse `description`, `tools`, `model`, `thinking`, `max_turns`.
   `thinking` maps to `--thinking`; `model` is passed fully-qualified.
4. **Fully-qualified model guard.** Warn if an agent's model has no `provider/`
   prefix (a bare name can resolve to a keyless provider and hang — the classic
   trap).

**New** (things neither benchmark ships):

1. **`/subagents doctor` — a self-audit that actually passes.** Spawns a real
   canary subagent end-to-end with a short timeout and reports PASS/FAIL with
   timing. This is the thing that "never passed" before; here it is a first-class,
   fast, deterministic check.
2. **Depth-capped subagent trees.** Because children carry this extension, a
   subagent can itself fan out. `PI_SUBAGENT_DEPTH` increments per level and the
   tool refuses to spawn past `PI_SUBAGENT_MAX_DEPTH` (default 3), with a global
   concurrency cap, so trees can never fork-bomb.
3. **Deterministic timeout semantics surfaced to the model**, so the parent can
   react (retry smaller, split the task) instead of silently losing a branch.

## MVP scope

- One self-contained file: `extensions/subagents.ts` (matches pi-stack's flat
  `extensions/*.ts` convention; no multi-file symlink dance).
- Tool `subagent` with modes: single `{agent, task}`, parallel `{tasks:[...]}`,
  chain `{chain:[...]}` with `{previous}` substitution.
- `/subagents` command: list discovered agents + config; `/subagents doctor`:
  live canary self-audit.
- Reuses the existing `agents/*.md` presets unchanged.

## Architecture sketch

```text
parent pi (this extension registers tool "subagent")
  └─ execute(): discover agents → for each task:
       spawn: node <cli.js> --mode json -p --no-session \
                 --no-extensions -e <self> \
                 [--model provider/id] [--thinking lvl] \
                 [--tools a,b,c] [--append-system-prompt <tmp>] \
                 "Task: ..."
         env: PI_SUBAGENT_DEPTH = depth+1
       parse stdout JSON event stream (message_end / tool_result_end)
       watchdog: idle-timeout + wall-clock → SIGTERM/SIGKILL
       collect messages, usage, stopReason → AgentToolResult
```

Child invocation is resolved robustly: prefer `process.execPath` +
the resolved `cli.js` from the pi package; fall back to `pi` on PATH.
`PI_SUBAGENT_PI_COMMAND` overrides it (testing / non-standard installs).

## Usage

The model calls the `subagent` tool; you steer it in plain language. Agents are
the markdown files in `agents/` (filename = name): `fanout`, `deep`, `review`,
`architect`, `engineer`, `qa-lead`, `security-lead`, and the rest.

- **Single:** "use the `deep` subagent to find the root cause of X" →
  `{agent:"deep", task:"..."}`.
- **Parallel:** "run 3 fanout subagents, one per package" →
  `{tasks:[{agent:"fanout",task:"..."}, ...]}` (max `PI_SUBAGENT_MAX_PARALLEL`,
  `PI_SUBAGENT_MAX_CONCURRENCY` at a time).
- **Chain:** "scout, then plan from what it found" →
  `{chain:[{agent:"fanout",task:"..."},{agent:"architect",task:"...{previous}..."}]}`.
- **Trees:** an agent that keeps the `subagent` tool (no `tools:` restriction, or
  one that lists `subagent`) can fan out further, up to `PI_SUBAGENT_MAX_DEPTH`.
- **Project-local agents:** pass `agentScope:"both"` to also load `.pi/agents/*.md`.

Commands:

- `/subagents` — list discovered agents (model, thinking, tool count, warnings)
  and the current config.
- `/subagents doctor` — spawn a real canary subagent end-to-end and report
  PASS/FAIL with timing. Run this first if anything seems off.

Workflow prompt templates (in `prompts/`): `/fan-out`, `/deep-dive`,
`/second-opinion`.

## Config (env)

| var | default | meaning |
| --- | --- | --- |
| `PI_SUBAGENT_IDLE_MS` | 120000 | kill a child after this long with no output |
| `PI_SUBAGENT_TIMEOUT_MS` | 600000 | hard wall-clock cap per child |
| `PI_SUBAGENT_MAX_CONCURRENCY` | 4 | concurrent children in parallel mode |
| `PI_SUBAGENT_MAX_PARALLEL` | 8 | max tasks in one parallel call |
| `PI_SUBAGENT_MAX_DEPTH` | 3 | tree depth cap (fork-bomb guard) |
| `PI_SUBAGENT_DEPTH` | (internal) | current depth, set on children |

## Risks / unknowns

- **max_turns has no CLI flag** in pi 0.80.5, so it is advisory (injected into the
  child's prompt as a budget hint); the wall-clock cap is the real bound.
- **Trees + `-e <self>`** rely on `--no-extensions` still honoring explicit `-e`
  (verified in `resource-loader.js`: `noExtensions` ⇒ extension set is exactly the
  CLI `-e` paths).
- If a future pi renames the JSON event shape, discovery of `message_end` output
  must track it (same coupling the stock example has).
</content>

</invoke>
