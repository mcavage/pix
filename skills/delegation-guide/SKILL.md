---
name: delegation-guide
description: Context-passing and delegation rules for multi-stage subagent workflows. Use when orchestrating fanout/deep/review subagents, planning wave execution, or handing work off to subagents.
---
# delegation-guide

Subagents do not share state. You are the **overlord**: the top-level orchestrator.
Your job is to DIRECT the crew, not to do the units yourself. You pass all context,
collect results, and move the pipeline forward. If you catch yourself writing the
code / doc / analysis a subagent should own, stop and delegate it. The overlord
earns its keep by keeping many workers busy in parallel, not by grinding one task.

## Context-passing rules

1. **Inline, not references.** Paste actual content into the prompt. A file path
   is not context; the file's contents are.
2. **Disk bridges stages.** Stage N writes its output to disk. You read it. You
   include the relevant parts in Stage N+1's prompt.
3. **Pass forward, not back.** Upstream agents don't need downstream output; skip it.
4. **Minimize context, maximize relevance.** Extract the sections that matter.
   Don't dump entire documents into a prompt.
5. **Large raw data goes to a subagent first.** Never pull a large raw dataset
   into your context window. Delegate to a `fanout` subagent that reads it and
   returns a concise summary; pass the summary forward.

## Delegation rules

- **Be prescriptive.** Specify the exact deliverable: format, length, structure.
  Tell the subagent to return the full result in one shot.
- **No open-ended prompts.** If a subagent returns a clarifying question instead
  of output, that is a delegation failure. Rewrite the prompt with the missing
  context and retry.
- **Parallelize aggressively.** Launch independent subagents in ONE parallel call:
  the `subagent` tool with `{tasks:[...]}`. Up to 16 tasks per call, 8 run at once
  (`PI_SUBAGENT_MAX_PARALLEL` / `_MAX_CONCURRENCY`). Prefer `{tasks:[...]}` over
  `{chain:[...]}` — use `chain` ONLY when stage N literally consumes stage N-1's
  output (the `{previous}` placeholder). Independent work in a chain is throughput
  left on the table. Serialize only on a real data dependency.
- **File discipline.** Every subagent prompt must include: "Do NOT create any
  files unless explicitly required. Do all work in memory and return results in
  your response. If you must write a file, use only `/tmp/` or a project scratch
  path the task specifies."
- **Escalation.** Max 2 retries per stage. On the third failure, stop and surface
  the blocker to the user; don't keep looping.

## Subagent types (pi-stack)

Invoke with the `subagent` tool, `agent=<name>` (NOT the old `Agent` tool with
`subagent_type=`, which is not present here).

| Type | subagent agent | Resolves to | Use for |
|---|---|---|---|
| `fanout` | `agent=fanout` | Gemini Flash-Lite | Parallel investigation, data gathering, cheap breadth |
| `deep` | `agent=deep` | Opus 5 | Single hard task needing a full context window (a whole story, deep analysis) |
| `review` | `agent=review` | Gemini Pro | Cross-vendor adversarial pass: code review, peer review, fact-check |
| `engineer` | `agent=engineer` | Sonnet 5 | The workhorse for ordinary code units in a wave |

## Wave execution pattern

Plans group work into waves by dependency. Independent units are PARALLEL BY
DEFAULT, through isolated git worktrees — a shared working tree is never a
reason to run them one at a time. Within a wave, all units run in parallel (one
`deep`, `fanout`, or `engineer` subagent per unit, each in its own worktree).
You orchestrate waves; you do not execute units yourself.

1. Identify units and their full dependency DAG: not just wave order, but which
   unit's output actually feeds which other unit's input, and which pairs would
   touch the same file.
2. Group into waves (units with no unmet dependency or file-conflict edge to
   each other go in the current wave, regardless of how many share a working
   tree today). Create one isolated git worktree per concurrent unit in the
   wave (`git worktree add`) so each subagent edits its own tree.
3. Launch the whole ready wave in one parallel `{tasks:[...]}` call (up to 8 run
   at once; split a wider wave into back-to-back parallel calls). Mirror the
   wave in the todo list: mark every unit in the wave `in-progress` at dispatch,
   `completed` as each returns (the todo tool allows many in-progress at once
   for exactly this).
4. Collect results, then merge reviewed commits after collecting results
   (`--no-ff`) and remove each unit's worktree before starting the next wave.
5. Serialize ONLY units joined by a real dependency edge or file-conflict edge
   (one consumes the other's output, or both must edit the same file) — never
   because they happen to share a working tree.
6. Gate with `code-review` (cross-vendor `review` subagent) before any wave that
   produces code that will ship. Gate with `verify` before marking a unit done.

## Quality gates

Use these sibling skills as gates, not afterthoughts:

- `code-review` before shipping any code wave.
- `verify` before any completion claim or handoff.
- `build` to produce the story files that make each `deep` unit context-complete.
- `debug` if a unit fails and the root cause is not obvious.
- `qa` after implementation, before `ship`.
