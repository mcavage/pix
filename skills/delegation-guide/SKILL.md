---
name: delegation-guide
description: Context-passing and delegation rules for multi-stage subagent workflows. Use when orchestrating fanout/deep/review subagents, planning wave execution, or any time you are the orchestrator handing work off to subagents.
---
# delegation-guide

Subagents do not share state. You are the orchestrator: you pass all context,
collect results, and move the pipeline forward.

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
- **Parallelize aggressively.** Launch independent subagents in parallel (Agent
  tool). Serialize only when there is a real data dependency.
- **File discipline.** Every subagent prompt must include: "Do NOT create any
  files unless explicitly required. Do all work in memory and return results in
  your response. If you must write a file, use only `/tmp/` or a project scratch
  path the task specifies."
- **Escalation.** Max 2 retries per stage. On the third failure, stop and surface
  the blocker to the user; don't keep looping.

## Subagent types (pi-stack)

| Type | Agent tool value | Use for |
|---|---|---|
| `fanout` | `subagent_type=fanout` | Parallel investigation, data gathering, parallel coding units |
| `deep` | `subagent_type=deep` | Single complex task needing a full context window (a whole story, deep analysis) |
| `review` | `subagent_type=review` | Cross-vendor adversarial pass: code review, peer review, fact-check |

## Wave execution pattern

Plans group work into waves by dependency. Within a wave, all units run in
parallel (one `deep` or `fanout` subagent per unit). You orchestrate waves; you
do not execute units yourself.

1. Identify units and their dependencies.
2. Group into waves (units with no unmet deps go in the current wave).
3. Launch the wave in parallel. Collect results before starting the next wave.
4. Gate with `code-review` (cross-vendor `review` subagent) before any wave that
   produces code that will ship. Gate with `verify` before marking a unit done.

## Quality gates

Use these sibling skills as gates, not afterthoughts:

- `code-review` before shipping any code wave.
- `verify` before any completion claim or handoff.
- `build` to produce the story files that make each `deep` unit context-complete.
- `debug` if a unit fails and the root cause is not obvious.
- `qa` after implementation, before `ship`.
