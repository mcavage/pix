---
name: delegation-guide
description: Context-passing and delegation rules for multi-stage subagent workflows. Use when orchestrating fanout/deep/review subagents, planning wave execution, or handing work off to subagents.
---
# delegation-guide

The main agent owns product intent, architecture and integration. Delegate concrete
implementation or investigation units when that separation helps the outcome.
`deliver` owns stage and review policy; this guide describes handoffs, not another
pipeline. Keeping workers busy is not a goal by itself.

## Context-passing rules

1. **Decisions inline, evidence by reference.** Include the task, acceptance
   criteria, owned scope and key interfaces inline. Supply accessible artifact
   paths for source and detailed evidence; do not copy the full prior transcript.
2. **Disk bridges stages.** Stage N writes its output to disk. You read it. You
   include the relevant parts in Stage N+1's prompt.
3. **Return relevant findings.** A failed integration or acceptance check goes
   back to the owning worker with its evidence, not the whole downstream transcript.
4. **Minimize context, maximize relevance.** Extract the sections that matter.
   Don't dump entire documents into a prompt.
5. **Large raw data goes to a subagent first.** Never pull a large raw dataset
   into your context window. Delegate to a `fanout` subagent that reads it and
   returns a concise summary; pass the summary forward.

## Delegation rules

- **Be prescriptive.** Specify the exact deliverable: format, length, structure.
  Tell the subagent to return the full result in one shot.
- **Bound the question.** Include the acceptance criterion and needed interfaces.
  If a worker exposes missing context, resolve that gap before retrying dependent
  work. A well-founded question is not automatically an agent failure.
- **Parallelize aggressively.** Launch independent subagents in ONE parallel call:
  the `subagent` tool with `{tasks:[...]}`. Up to 16 tasks per call, 8 run at once
  (`PI_SUBAGENT_MAX_PARALLEL` / `_MAX_CONCURRENCY`). Prefer `{tasks:[...]}` over
  `{chain:[...]}` — use `chain` ONLY when stage N literally consumes stage N-1's
  output or candidate artifacts. Prefer artifact references to a full `{previous}`
  transcript. Serialize only on a real data dependency.
- **File discipline.** Assign the implementation workspace and owned source files.
  For investigation, request a final report or a unique scratch artifact. Do not
  apply report-only restrictions to a worker assigned to change existing code.
- **Escalation.** Max 2 retries per stage. On the third failure, stop and surface
  the blocker to the user; don't keep looping.

## Model identity

Use the existing named presets and explicit environment bindings. Role names do
not establish model identity or price. Verify observed model metadata on success
and failure; unknown usage is not zero. Never silently switch to a cheaper or
stronger model. A worker's question can expose a missing interface contract;
resolve it instead of treating every question as an agent failure.

## Wave execution pattern

Plans group work into waves by dependency. Independent units are PARALLEL BY
DEFAULT, through isolated git worktrees — a shared working tree is never a
reason to run them one at a time. Within a wave, all units run in parallel (one
`deep`, `fanout`, or `engineer` subagent per unit, each in its own worktree).
The main agent orchestrates waves and owns integration; workers own their assigned units.

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
4. Merge reviewed commits after collecting results when current review already
   exists; otherwise integrate the preserved unit patches into the candidate for
   the combined review. Respect commit authority. Remove unit worktrees only
   after source and evidence are preserved.
5. Serialize ONLY units joined by a real dependency edge or file-conflict edge
   (one consumes the other's output, or both must edit the same file) — never
   because they happen to share a working tree.
6. Collect unit proof, then verify the integrated candidate and get independent
   review. Do not impose full product review per unit and again per unchanged
   wave. Preserve the actual evidence before marking a unit complete.

## Quality gates

Use these sibling skills as gates, not afterthoughts:

- `code-review` before shipping any code wave.
- `verify` before any completion claim or handoff.
- `build` to produce the story files that make each `deep` unit context-complete.
- `debug` if a unit fails and the root cause is not obvious.
- `qa` after implementation, before `ship`.
