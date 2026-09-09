---
name: verify
description: Verify claims with executable evidence. During deliver, use its candidate-bound evidence rules; otherwise run fresh checks before completion, commit, PR, or handoff.
---
# verify

During `deliver`, its verification reference governs evidence reuse: confirm the
recorded candidate, contract, inputs, and environment still match; cite the original
run time and result. Never describe a digest check as a fresh test run. If identity
cannot be established, rerun the check.

For standalone verification, follow the fresh-run procedure below.

**Iron law: no completion claim without fresh evidence.** Saying something works
when you haven't checked is not efficiency, it's a guess dressed as a fact.

Before you claim any status:
1. **Identify** the command that would prove it.
2. **Run** it fresh and in full. Not a remembered earlier run, not a partial check.
3. **Read** the whole output: exit code, failure count, the actual result.
4. **Claim** only what the output supports, and cite it.

| Claim | Proof | Not proof |
|---|---|---|
| tests pass | test command, 0 failures | "should pass", an earlier run |
| build works | build exits 0 | the linter passed |
| bug fixed | the original symptom's test now passes | the code changed |
| regression test works | fails without the fix, passes with it | it passed once |
| a subagent finished | the diff/output shows it | the agent reported "done" |
| requirements met | line-by-line against the spec | the tests are green |

Red flags that you're about to claim without proof: the words "should",
"probably", "seems"; feeling finished; being tired; "just this once". Run the
command first.
