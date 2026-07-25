---
name: debug
description: Root-cause-first debugging — reproduce, investigate, form a falsifiable hypothesis, verify it, THEN fix. Use for "debug this", "why is X broken", error reports/stack traces, or "it worked yesterday".
---
# debug

**Iron law: no fix without a confirmed root cause.** A patch that makes the
symptom disappear without an explanation is not a fix.

## Discipline
- **Freeze the scope.** While debugging, touch only the code on the failure's
  path. No "while I'm here" refactors of adjacent modules; that turns one bug
  into an uncontrolled diff. The freeze lifts once the bug is fixed and verified.
- **Fix the source, not the symptom.** Trace the bad value back up the call chain
  to where it first goes wrong, and fix it there.
- **Three strikes, then architecture.** If three real fixes have failed, stop. A
  bug that resists three attempts is usually a design problem, not a line. Step
  back and question the structure before a fourth try.

## Phases
1. **Reproduce.** Get a deterministic repro. If you can't, say so and gather
   signal first: logs, the stack trace, the recent diff, the env.
2. **Investigate.** Read the actual failing code path. When the search space is
   wide, fan out: spawn several `fanout` subagents in parallel (the `subagent`
   tool with `agent=fanout`), each chasing one area, then collect.
3. **Hypothesize.** State the single most likely root cause as a falsifiable
   claim: "X fails because Y at `path:line`."
4. **Verify.** Prove it: add a log, write a failing test, inspect the state. If
   the hypothesis is wrong, return to step 2. Don't skip this.
5. **Fix.** Only now: make the smallest change that addresses the root cause.
   Re-run the repro to confirm it's gone, and add a regression test (`tdd`).

Report: the root cause, the fix (`path:line`), and how you verified it.
