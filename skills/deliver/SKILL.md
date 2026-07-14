---
name: deliver
description: Autonomous full-rigor delivery loop. Drives a request all the way to proven, committed, review-clean without returning to the user mid-flight. Delegates build to subagents, runs full UAT with real evidence, and runs the cross-vendor review subagent at least twice, fixing every finding. Use for "take this all the way", "do it properly", "don't stop", "full send", "full UAT and review", "don't come back until it's done", "address all the findings", or any time the user is about to hand-write delivery rigor.
---
# deliver

This skill governs YOU, the orchestrator. It is a contract, not advice. When
`deliver` is active, the acceptance bar below is the ONLY condition under which
you return control to the user. Every shortcut listed under Forbidden is a
violation of that contract.

`build` and `ship` are the machine. `deliver` is the operator who refuses to
leave the machine until the part comes out finished.

## The iron laws

**Law 0. You do not return control until the acceptance bar is met.**
The only exceptions are the three legit stop conditions below. "Done", "should
work", "this should be it", a partial handoff, or stopping to ask permission for
something you could verify yourself are all violations of this law.

1. **No claim without fresh evidence.** Every "green", "passing", "works",
   "fixed" is backed by the actual command output from a run you just did this
   turn. Not a remembered run. Not a subagent's "done" report. Run the command;
   read the output; cite it.

2. **Delegate the build; don't hand-code it.** If a unit is real implementation
   work, an `engineer` (or `architect` for design) subagent writes it. You are
   the orchestrator: you shard, dispatch, integrate, and gate. Solo-coding what
   a subagent should build burns your context and skips the fresh-brief
   discipline that keeps quality up. Fan out parallel across disjoint units;
   serialize only on a real data dependency.

3. **UAT is mandatory and real.** Exercise every path with actual commands: the
   happy path plus every error and edge path the spec names. Read the output.
   Paste it. "Built without crashing" is not UAT. No UAT means not done.

4. **Adversarial review runs at least twice.** The cross-vendor `review`
   subagent (a different vendor than you, so its blind spots differ) reviews the
   work. Run it, fix everything it raises, run it again. Keep looping until a
   round comes back clean. One review round is never enough.

5. **Address every finding.** No "minor, skipping." No batching a finding into
   "future cleanup." Each BLOCK or CONCERN is either fixed (with evidence) or
   triaged in one line WITH the user's explicit blessing. Silent triage is
   forbidden.

6. **Decide and proceed; don't stop at the first question.** If a question has
   a verifiable or mechanical answer, answer it yourself (read the code, run the
   command, apply `plan`'s decision principles) and keep going. Only a genuinely
   irreversible or scope-expanding call goes to the user, surfaced in ONE line,
   while you keep progressing everything that doesn't depend on it.

## The mandatory loop

Write `.pi-agent/deliver/<slug>/status.json` at FRAME and update it after every
gate. Per `conventions`, disk is the bridge; never rely on context memory for
what a prior stage produced.

```
0. FRAME
   Restate the acceptance bar for this specific request. Write status.json
   (all gates pending). Detect build/test/lint/typecheck commands and record
   a green/red baseline. If any baseline is red, note it; don't chase it.

1. PLAN/SHARD
   If the work is non-trivial, run plan then build's spec+shard phases: PRD,
   architecture, self-contained story files, parallelization groups (disjoint-
   file cross-check). Fan out an `architect` subagent for the consistency check.
   Skip per build's skip table. Even when skipping Phase 1, you must shard into
   units you can delegate.

2. DELEGATE
   For each unit in dependency order:
     - Parallel units (disjoint files): set up worktrees, fire one `engineer`
       subagent per unit IN THE SAME TURN.
     - Sequential units: one `engineer` or `deep` subagent each.
   You never write the unit yourself. Collect results, merge branches (--no-ff),
   clean up worktrees.
   GATE: after integration, full build + tests must be green (verify). Red here
   loops back to DELEGATE (fix-unit); max 2 retries per unit, then escalate.

3. INTEGRATE
   Run the full build + test + lint + typecheck on the feature branch. Real
   output. Any red loops back to DELEGATE.

4. FULL UAT
   Exercise every path with real commands: happy path + each named error/edge
   path. For a web surface, run `qa` (browser, screenshot evidence). Paste
   evidence into status.json. Any failed path loops back to DELEGATE; not done
   until every path passes.

5. REVIEW #1
   Run `code-review` then the cross-vendor `review` subagent on the diff + spec.
   Collect all findings (BLOCK / CONCERN / nit).

6. FIX-LOOP
   Fix every finding. Dispatch real fixes back to `engineer`; don't hand-patch
   around a subagent's domain. Re-run build + tests after each fix batch. No
   finding is skipped without one-line user triage.

7. RE-UAT
   Re-exercise every path the fixes touched plus the full happy path. Real output.

8. REVIEW #2
   Run the `review` subagent again on the updated diff. If it raises anything
   new, go to step 6. Keep looping steps 5-8 until a review round is clean.
   Minimum two full review rounds; more until clean.

9. VERIFY
   Final `verify` gate: build/tests/lint/typecheck green (fresh), docs updated
   for any surface change, UAT evidence recorded, >= 2 clean review rounds, all
   findings closed. Commit.

10. SHIP (if in scope)
    If the request implies a PR ("ship", "open a PR", "all the way"), run `ship`:
    rebase, tests, review gate, version bump, PR. Stop at PR creation. Never
    merge. If the request was "get it working locally", stop at verified-local.

11. REPORT
    Only now return to the user, with the evidence bundle (see Reporting).
```

Gates are hard. You may not advance past a gate on a remembered result. Every
gate is a `verify` checkpoint with pasted output. Max 2 retries per stage; the
third failure escalates.

## The acceptance bar

You may return to the user ONLY when every row below is true, with evidence on
disk. Miss any row and you are not done.

| Gate | What passing looks like | Proof |
|---|---|---|
| Build | exits 0 | fresh build output this turn |
| Tests | 0 failures, count >= baseline, new tests for new behavior | fresh test run output |
| Lint / typecheck | clean (or repo-tolerated warnings only) | fresh lint + typecheck output |
| Full UAT | every named path exercised and passing | pasted command output or screenshots |
| Review | >= 2 cross-vendor `review` rounds, converged to clean | review reports; all findings closed or user-triaged |
| Findings | every BLOCK/CONCERN fixed or explicitly triaged with user blessing | fix commits or one-line triage user approved |
| Committed | changes committed on the feature branch | `git log` output; PR URL if ship was in scope |

## Legit stop conditions

Three only. Everything else is a violation.

1. **Acceptance bar met.** Report with the evidence bundle. This is the win
   condition.

2. **A genuine user-only decision.** Irreversible (drop a table, delete data),
   a taste call reasonable people disagree on, or a scope expansion beyond the
   original ask. Surface it in ONE line with a recommended default. Keep working
   everything that doesn't depend on it. Never stall the whole loop on one fork.
   Mechanical or verifiable questions are not this; decide them yourself.

3. **Repeated hard failure after 2 attempts per stage.** Escalate with: what
   you tried, the failing output, your hypotheses, and a recommended next step.
   Never loop silently. Never quietly give up and report "mostly done."

## Forbidden (anti-patterns)

| Anti-pattern | What it looks like | Correction |
|---|---|---|
| Solo-coding | Writing the implementation yourself instead of dispatching `engineer` or `architect` | Shard the work; fan out subagents; you orchestrate |
| Done-without-UAT | "Implemented, should work" | Exercise every path with real commands; paste evidence; then claim |
| One-and-done review | A single review round, then ship | Run >= 2 cross-vendor `review` rounds; loop until clean |
| Finding-dropping | "Minor, skipping" or batching to "later" | Fix every finding, or one-line triage WITH user blessing |
| Should-work | "probably", "seems", "should pass" | Run the command; read the output; cite it |
| First-question bail | Return control the moment anything is unclear | Decide mechanical questions yourself; surface only true user-calls, one line, keep moving |
| Partial handoff | Return with gates still pending | The acceptance bar is all-or-nothing; keep looping |
| Silent give-up | Report "mostly working" after a couple of tries | Escalate explicitly with tried / failed / hypotheses |
| Serial-when-parallel | Units with disjoint files run one at a time | Fan out in the same turn; serialize only on real dependencies |
| Stopping for context length | Pausing because the session feels long | Save progress to disk and continue; disk is the bridge |

## How deliver composes

`deliver` WRAPS; it does not replace. It is the driver loop that calls `plan`,
`build`, and `ship`. Those skills own the how of each stage. `deliver` owns
"run all of them, gate hard, don't exit early."

- `plan` produces the spec `deliver` feeds to `build`.
- `build` runs under `deliver`, but `deliver` overrides its "stop and summarize"
  ending: the loop continues into review + UAT + ship instead of handing back.
- `ship` is the final stage when a PR is in scope.
- `code-review`, `verify`, `qa` are gates `deliver` refuses to skip.
- `delegation-guide` governs the fan-out rules (inline context, disk bridges,
  parallelize disjoint, 2-retry cap).
- `conventions` governs disk discipline (`.pi-agent/deliver/<slug>/status.json`).

**The `subagent` tool (per AGENTS.md).** Call it with `{agent, task}` (single),
`{tasks:[...]}` (parallel), or `{chain:[...]}`. Always fully-qualify model ids;
a bare name can resolve to a keyless provider and hang the subagent forever.
Agents `deliver` uses:

| Agent | Role | When |
|---|---|---|
| `architect` | design / consistency check | spec, shard review |
| `engineer` | unit implementation | every implementation unit |
| `qa-lead` | edge-case coverage, missing test gaps | Phase 4 UAT |
| `security-lead` | STRIDE, OWASP, secrets scan | when code ships |
| `review` | cross-vendor adversarial review | review rounds 1, 2, ... |

`review` runs as `openai/gpt-5.6-sol` when the main model is Claude (or the
other vendor's strong model; the point is different vendor, different blind
spots). Note: some older skills reference an `Agent` tool with
`subagent_type=review`; that API is not present on this harness. Use the
`subagent` tool with `agent=review`.

Skip logic is inherited from `build` (throwaway / bugfix / docs-only scale the
crew down), but the review loop, UAT, and verify gates never skip for anything
that ships. If the user wants a throwaway, they don't want `deliver`; hand to
`build`'s throwaway mode and say so.

## Reporting

Evidence first, one screen. When the acceptance bar is met:

```
Delivered: <one line what shipped>
Build / tests / lint / typecheck: <fresh results, counts>
UAT: <paths exercised, result> (evidence in .pi-agent/deliver/<slug>/)
Review: <N rounds, converged clean; findings fixed: X, triaged: Y>
Committed: <branch> / <commit range>   PR: <url or "verified-local, run ship for PR">
Open user-calls (if any): <one line each, with the default you took or recommend>
```

No prose padding. The evidence is the message.
