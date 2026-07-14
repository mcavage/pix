---
name: deliver
description: Autonomous full-rigor delivery loop. Drives a request all the way to proven, committed, review-clean without returning to the user mid-flight. Delegates build to subagents, runs full UAT with real evidence, and runs the cross-vendor review subagent at least twice, fixing every finding. Use for "take this all the way", "do it properly", "don't stop", "full send", "full UAT and review", "don't come back until it's done", "address all the findings", or any time the user is about to hand-write delivery rigor.
---
# deliver

This skill governs YOU, the orchestrator. It is a contract, not advice. When
`deliver` is active, the acceptance bar is the ONLY condition under which you
return control to the user. Every shortcut in the Forbidden list is a contract
violation.

`build` and `ship` are the machine. `deliver` is the operator who refuses to
leave the machine until the part comes out finished.

## Checklist: hold in head under pressure

Before advancing any gate or claiming done, run this list:

- [ ] status.json written and current (schema below); every gate has real evidence with command+timestamp+exit+log path
- [ ] UAT matrix covers: request paths + spec paths + all changed-code branches + test analysis + qa-lead enumeration; no spec means FRAME built the matrix first
- [ ] Baseline labelled BASELINE-GREEN or BASELINE-RED; a BASELINE-RED gate MUST stay labelled as such; no new failures added
- [ ] Top-level agent touched ONLY .pi-agent/ artifacts and the final report; every shipping change went through a subagent; no size exemption
- [ ] Units are the smallest independently reviewable slices; single-unit plan for non-trivial work has written architect approval in status.json
- [ ] Review rounds counted ONLY as explicit top-level `subagent` calls with agent=review; code-review's internal pass does NOT count
- [ ] "Clean" means explicit LGTM/APPROVE + zero untriaged findings + no timeout/error/missing-context; hedging is not clean
- [ ] Total review rounds >= 2; final round clean; any round with findings forces another cycle
- [ ] Every emitted item (review, QA, security, tests, lint, UAT, orchestrator) is in findings_ledger[]; nothing dropped, renamed, or downgraded without evidence; nits batched and fixed
- [ ] User-triaged means a separate user message quoting the exact finding; agent rationale is not user triage
- [ ] qa-lead built and reviewed the UAT matrix; security-lead ran before REVIEW #1 for shipping code; findings in the same ledger
- [ ] Each UAT matrix row has: path, preconditions, exact command/steps, expected, actual, evidence path, pass/fail; web surfaces include screenshots
- [ ] Review model is cross-vendor and hard-pinned per the table; recorded in status.json; same-vendor review does not count
- [ ] deliver's overrides applied: build's phase stops and ship's red-stop loop back to fix, not to you

## Iron laws

**Law 0. Do not return control until the acceptance bar is met.**
The only exceptions are the three legit stop conditions. "Done", "should work",
"this should be it", a partial handoff, or stopping to ask permission for
something verifiable are all violations.

**Law 1. No claim without fresh evidence.**
Every "green", "passing", "works", "fixed" is backed by actual command output
from a run this turn. Not a remembered run. Not a subagent's "done" report.
Run the command; read the output; cite it.

**Law 2. No solo-coding, no exceptions.**
The top-level orchestrator MUST NOT write, edit, or delete product source,
tests, docs, or config. Every shipping change goes to an `engineer`, `deep`,
`architect`, `qa-lead`, or `security-lead` subagent. "Glue", "small fix",
"cleanup", "just a typo" are NOT exemptions. See Solo-Coding Prohibition.

**Law 3. UAT is mandatory, real, and thick.**
UAT MUST cover every path derived from the request, the spec (if any), all
changed-code branches, test analysis, and qa-lead review. "Built without
crashing" is not UAT. No spec means FRAME builds the UAT matrix first.

**Law 4. Run at least two explicit top-level review rounds; loop until the final round is clean.**
code-review's internal pass does NOT count. One round is never enough.
"Clean" is defined in Review Rules.

**Law 5. Every finding is a finding.**
Every item emitted by any source (review, QA, security, tests, lint, UAT,
orchestrator observation) is a finding regardless of label. MUST go in
findings_ledger[]. Nothing dropped, renamed, downgraded, or false-positived
without evidence. Nits are batched and fixed, not skipped.

**Law 6. Decide and proceed.**
A verifiable or mechanical question you answer yourself. Only a genuine
user-only decision (see Legit Stop Conditions) goes to the user, in one line,
while everything else keeps moving.

## status.json schema

Write `.pi-agent/deliver/<slug>/status.json` at FRAME. Update after every gate.
A gate MUST NOT be marked green without real evidence. A gate backed by a
remembered result or a subagent's "done" report is a violation.

```json
{
  "acceptance_bar": "<one-line restatement of the specific ask>",
  "commands": {
    "build": "<exact command>",
    "test":  "<exact command>",
    "lint":  "<exact command>",
    "typecheck": "<exact command>"
  },
  "baseline": {
    "build":     "green|red",
    "tests":     "<N passing, M failing>",
    "lint":      "clean|N warnings",
    "typecheck": "clean|N errors",
    "label":     "BASELINE-GREEN|BASELINE-RED"
  },
  "review_model": "<fully-qualified model id, e.g. openai/gpt-5.6-sol>",
  "units": [
    { "id": "<unit-id>", "status": "pending|running|done|failed", "subagent_run_id": "..." }
  ],
  "subagent_runs": [
    { "id": "...", "agent": "...", "model": "...", "task_summary": "...", "status": "...", "timestamp": "..." }
  ],
  "uat_matrix": [
    {
      "path":           "<path name>",
      "preconditions":  "<state before>",
      "steps":          "<exact command or click sequence>",
      "expected":       "<observable outcome>",
      "actual":         "<what actually happened>",
      "evidence_path":  "<relative path to log or screenshot>",
      "result":         "pass|fail"
    }
  ],
  "review_rounds": [
    {
      "round":           1,
      "model":           "<fully-qualified id>",
      "timestamp":       "<ISO-8601>",
      "verdict":         "LGTM|APPROVE|REQUEST_CHANGES|BLOCK",
      "findings_count":  0,
      "clean":           true
    }
  ],
  "findings_ledger": [
    {
      "id":                  "<unique>",
      "source":              "review|qa|security|tests|typecheck|lint|uat|orchestrator",
      "round":               1,
      "text":                "<verbatim>",
      "status":              "open|fixed|user-triaged",
      "fix_commit":          "<sha or null>",
      "triage_message_ref":  "<verbatim quote or link to user message, or null>"
    }
  ],
  "attempts": [
    {
      "stage":          "<stage name>",
      "attempt":        1,
      "hypothesis":     "...",
      "change":         "...",
      "command":        "...",
      "output_summary": "...",
      "timestamp":      "..."
    }
  ],
  "final_evidence": {
    "build":     { "command": "...", "exit": 0, "timestamp": "...", "log": "<path>" },
    "tests":     { "command": "...", "exit": 0, "passed": 0, "failed": 0, "baseline_red": false, "timestamp": "...", "log": "<path>" },
    "lint":      { "command": "...", "exit": 0, "timestamp": "...", "log": "<path>" },
    "typecheck": { "command": "...", "exit": 0, "timestamp": "...", "log": "<path>" }
  },
  "commit": "<sha or null>"
}
```

## The mandatory loop

```
0. FRAME
   Restate the acceptance bar for this specific request. Write status.json
   (schema above, all gates pending). Detect build/test/lint/typecheck commands.
   Run each; record real output. Label baseline BASELINE-GREEN or BASELINE-RED.
   A BASELINE-RED label persists throughout; NEVER relabel it green even after
   all new work passes. If no spec exists, build the UAT matrix now from the
   request and changed-code analysis; do not defer it to Phase 5.

1. PLAN/SHARD
   Non-trivial work: run plan then build's spec+shard phases. Shard into the
   smallest independently reviewable units. A single-unit plan for non-trivial
   work MUST have explicit architect approval with written rationale recorded in
   status.json before proceeding. Fan out an `architect` for consistency check.
   Even when skipping Phase 1 per build's skip table, shard into units to
   delegate.

2. DELEGATE
   For each unit in dependency order:
     - Parallel (disjoint files): set up worktrees; fire one `engineer` subagent
       per unit IN THE SAME TURN.
     - Sequential: one `engineer` or `deep` subagent each.
   The top-level agent MUST NOT write the implementation. Collect results, merge
   branches (--no-ff), clean up worktrees.
   GATE: full build + tests must pass. Red here is a fix-loop input, NOT a stop;
   loop back to DELEGATE. Max 2 real attempts per unit (see Attempt Definition),
   then escalate.

3. INTEGRATE
   Run the full build + test + lint + typecheck on the feature branch. Real
   output. Any red loops back to DELEGATE, not to a stop.

4. SECURITY (mandatory before REVIEW #1 for shipping code)
   Dispatch `security-lead`. STRIDE, OWASP, supply-chain audit, secrets scan,
   auth/authz. Record ALL findings in findings_ledger[] with source=security.
   CRITICAL/HIGH enter the fix loop immediately before any review round.
   MEDIUM/LOW are recorded and proceed. This step MUST complete before REVIEW #1.

5. BUILD UAT MATRIX + FULL UAT
   `qa-lead` MUST build and review the UAT matrix for every deliverable type.
   The matrix MUST cover every path derived from:
     a. the request
     b. the spec (if one exists; if not, this was done in FRAME)
     c. all changed-code branches (trace every branch in modified functions)
     d. test analysis (what existing tests assume; coverage gaps)
     e. qa-lead's own edge-case enumeration
   Each matrix row: path, preconditions, exact command/steps, expected, actual,
   evidence path, pass/fail.
   For web surfaces: screenshot successful primary flows AND every bug or fix.
   For docs-only: verify every changed statement is accurate and renders correctly.
   For test-only: verify each new test fails without the code change and passes with it.
   Store all evidence under .pi-agent/deliver/<slug>/uat/. Update status.json.
   Any failed row is a fix-loop input, not a stop. Done only when every row passes.

6. REVIEW #1
   Run `code-review`, then dispatch the cross-vendor `review` subagent as an
   explicit top-level `subagent` tool call with agent=review. Record in
   review_rounds[]. The review prompt MUST include ALL of:
     - git diff <base>...HEAD output
     - list of changed files
     - spec and acceptance bar text
     - UAT evidence summary with evidence paths
     - build/test/lint/typecheck output summaries
     - complete findings ledger showing status of each item
     - list of fixes applied since the previous round (none on round 1)
   Collect ALL emitted items into findings_ledger[] regardless of label
   (BLOCK, CONCERN, nit, comment, observation). code-review's internal pass
   does NOT count as a review round.

7. FIX-LOOP
   Fix every open finding. Dispatch ALL fixes to `engineer`; the top-level agent
   MUST NOT hand-patch. Re-run build + tests after each fix batch. Nits are
   batched and fixed together, not skipped. No finding may be dropped, renamed,
   downgraded, or false-positived without evidence. User-triage is the only
   alternative to fixing; see Finding Rules.

8. RE-UAT
   Re-exercise every matrix row whose path was touched by fixes, plus the full
   happy path. Real output. Update evidence in status.json.

9. REVIEW #N (repeat from 6)
   Dispatch the cross-vendor `review` subagent again with the full prompt (all
   items from the required list, updated for this round). Any round that returns
   findings forces another fix-loop + re-UAT + review cycle. The final round
   MUST be clean. Total rounds MUST be >= 2, all explicit top-level subagent
   calls, all recorded in review_rounds[].

10. VERIFY
    Final gate: build/tests/lint/typecheck green from a fresh run this turn
    (exception: a gate marked BASELINE-RED passes iff no NEW failures were added,
    every affected/new test passes, and the failure set is unchanged-or-reduced
    vs baseline; NEVER relabel a BASELINE-RED gate green),
    docs updated for any surface change, UAT matrix complete with all rows
    passing, >= 2 explicit review rounds with the final round clean, all
    findings_ledger[] items closed or user-triaged. Write final_evidence and
    commit to status.json.

11. SHIP (if in scope)
    If the request implies a PR, run `ship`. deliver OVERRIDES ship's stop on
    red tests or rebase conflict: loop back to fix rather than stopping, unless
    the failure is a hard external block that qualifies as a Legit Stop
    Condition. Ship stops only at PR creation. Never merge. Never force-push
    to main.

12. REPORT
    Only now return to the user with the evidence bundle (see Reporting).
```

Gates are hard. MUST NOT advance past a gate on a remembered result. Every gate
is verified with fresh command output this turn. Raw logs and transcripts MUST
be saved under `.pi-agent/deliver/<slug>/`.

## Review rules

**What counts as a review round:** An explicit top-level `subagent` tool call
with `agent=review`. code-review's internal pass does NOT count. Self-summaries
do NOT count. A call that times out, errors, or returns missing-context does NOT
count toward the round total; it must be retried.

**Round count rule:** >= 2 explicit top-level rounds total. The FINAL round
(after all findings from every prior round are fixed) MUST be clean. Any round
that returns findings forces another fix-loop, re-UAT, and another review round.
There is no ambiguity: final round must be clean, total rounds must be >= 2.

**"Clean" defined:** The review subagent returns an explicit LGTM or APPROVE
with zero untriaged findings and no timeout, error, or missing-context condition.
A review that hedges, lists any concern, or fails to return explicitly is not
clean.

**Review prompt MUST include every round:**
- `git diff <base>...HEAD` output
- list of changed files
- spec and acceptance bar text
- UAT evidence summary with evidence paths
- build/test/lint/typecheck output summaries
- complete findings ledger (all rounds, showing each item's current status)
- list of fixes applied since the previous round

**Review model hard-pin:**

| Your main vendor | Use this review model |
|---|---|
| Anthropic (Claude) | `openai/gpt-5.6-sol` |
| OpenAI (GPT) | `anthropic/claude-opus-4-8` |
| Gemini or Ollama | `openai/gpt-5.6-sol`, else `anthropic/claude-opus-4-8` |

Record the review model in status.json at FRAME. Same-vendor review does NOT
count. A Claude-to-Claude review is not a review.

## Finding rules

**Every emitted item is a finding.** This covers output from: review subagent,
qa-lead, security-lead, tests, lint, typecheck, UAT matrix rows, and your own
orchestrator observations. The label does not matter: BLOCK, CONCERN, nit,
comment, note, observation are all findings. Every one goes in findings_ledger[].

**Findings MUST NOT be:**
- Dropped (removed from the ledger)
- Renamed (relabelled to a lesser category without evidence)
- Downgraded (severity reduced without concrete evidence in the ledger entry)
- False-positived (dismissed as inapplicable without a specific documented reason)

**Nits:** Batch into a single fix commit. Do not skip.

**"User-triaged" defined:** A SEPARATE user message that explicitly approves
skipping that exact finding after seeing the risk. The message MUST be verbatim-
quoted or linked in the finding's `triage_message_ref` field. The agent's own
rationale for skipping does NOT constitute user triage. A user message that says
"ok" to a batched list without naming the specific finding does NOT constitute
triage of that finding.

## Solo-coding prohibition

The top-level orchestrator MUST NOT write, edit, or delete any file in:
- product source code
- tests
- documentation
- configuration

Permitted top-level edits (NONE are shipping files):
- `.pi-agent/` artifacts (status.json, reports, UAT matrix)
- Mechanical merge-conflict resolution (choose one side; no logic rewrites)
- The final in-chat report

A changelog entry or version bump IS a shipping change: dispatch it to an
`engineer` subagent (or let `ship` handle it), NEVER hand-edit it top-level.

There are NO size-based exemptions. "Glue code", "one-liner", "small fix",
"cleanup", "just a typo" are all prohibited. Dispatch to `engineer`, `deep`,
`architect`, `qa-lead`, or `security-lead`.

## Unit sizing rule

Units are the smallest independently reviewable slices of work. A unit that
spans the full feature is not a unit; it is the whole feature undivided. For
non-trivial work, a plan that proposes a single unit MUST get explicit architect
approval with written rationale before execution. The architect's approval and
rationale go in status.json under the relevant unit entry.

## Attempt definition

An attempt = one hypothesis + one concrete change or investigation + one fresh
run + recorded output written to `attempts[]` in status.json. Two identical
reruns without a changed hypothesis count as ONE attempt, not two. The second
attempt MUST have a different hypothesis from the first. A stage that completes
in one attempt never escalates. Max 2 real attempts per stage; on the third
failure, escalate. Before escalating, dispatch a `deep` or specialist subagent
unless the failure is clearly external tooling (network down, CI provider outage,
third-party API). Escalation MUST include the full attempts ledger.

## Legit stop conditions

Three only. Everything else is a contract violation.

1. **Acceptance bar met.** Report with the evidence bundle. This is the win
   condition.

2. **A genuine user-only decision.** This condition has a narrow definition.
   ALL THREE of the following MUST be true:
     a. No safe reversible default exists
     b. All work not dependent on this decision has already continued
     c. Every remaining gate is blocked by this specific decision and nothing else
   "Taste call" means: reasonable engineers would make genuinely different choices
   and neither choice is objectively wrong (naming a public API, a non-critical
   UI color). Required gates, review fixes, tests, docs, and UAT are NEVER scope
   expansion and NEVER qualify as user-only decisions. Surface the question in
   one line with a recommended default.

3. **Hard failure after 2 real attempts per stage.** See Attempt Definition.
   Before escalating, dispatch a `deep` or specialist subagent unless the failure
   is confirmed external tooling. Escalate with the full attempts ledger, what
   failed, your hypotheses, and a recommended next step. Never loop silently.
   Never report "mostly done."

## Overrides of build and ship

`deliver` OVERRIDES the stop-and-return behavior of both `build` and `ship`.

**Override build:** build's intermediate stops (BLOCK verdict, red phase,
phase-level failures) are fix-loop inputs under `deliver`, not turn-enders.
`deliver` loops back to DELEGATE or the relevant fix stage; it NEVER surfaces
a build-level stop to the user.

**Override ship:** ship's "STOP and report" on red tests or rebase conflict is
a fix-loop input under `deliver`. `deliver` dispatches the fix and retries.
Ship stops only at PR creation. The only exception is a hard external block
that qualifies as Legit Stop Condition 3.

## Skip logic

Skip logic from `build` MAY reduce spec depth and crew size only. The following
MUST NEVER be skipped regardless of deliverable type:
- >= 2 explicit cross-vendor review rounds
- UAT matrix with real thick evidence
- findings_ledger[] (all findings recorded, all resolved or user-triaged)
- verify gate
- commit

**Docs-only UAT:** Verify every changed statement is accurate, all links
resolve, and the rendered output matches intent. qa-lead reviews for
completeness and coverage gaps.

**Test-only UAT:** Verify each new test fails without the target code change
and passes with it. qa-lead reviews for coverage gaps.

qa-lead MUST run on every deliverable that ships (every type). security-lead
MUST run before REVIEW #1 on every deliverable that ships code, config, or
infrastructure. For docs-only or test-only, security-lead reviews for
information leakage and credential exposure instead of a full STRIDE pass.
All security findings go in the same findings_ledger[].

## Forbidden (anti-patterns)

| Anti-pattern | What it looks like | Correction |
|---|---|---|
| Solo-coding | Editing product/test/docs/config yourself | Dispatch to subagent; no size exemption |
| Done-without-UAT | "Implemented, should work" | Run every UAT matrix row; paste evidence |
| Thin UAT | Only spec-named paths | Cover spec + changed branches + test analysis + qa-lead review |
| No-spec thin-UAT | Skipping matrix build when no spec | FRAME builds the matrix from request + code analysis |
| One-and-done review | A single review round then ship | >= 2 explicit rounds; final round clean |
| code-review counted | Counting build's code-review as a round | Only explicit top-level subagent calls with agent=review count |
| Same-vendor review | Claude reviewed by Claude | Cross-vendor only; hard-pin per the table |
| Fuzzy clean | "No major issues" counts as clean | Clean = explicit LGTM/APPROVE + zero untriaged findings + no timeout |
| Final round not clean | Stopping at 2 rounds with findings | Loop until the final round returns clean |
| Finding-dropping | "Minor, skipping" or deleting from ledger | Every finding in ledger; fix or user-triage with evidence |
| Downgrading without evidence | Relabelling BLOCK to nit | Evidence required for any severity change |
| Self-triage | Agent decides a finding is inapplicable | Separate user message quoting the exact finding |
| Should-work | "probably", "seems", "should pass" | Run the command; read the output; cite it |
| Baseline-red called green | "Tests pass" when baseline was BASELINE-RED | Label stays BASELINE-RED; pass means no new failures + affected tests pass |
| Giant unit | Full feature as one undivided unit | Smallest independently reviewable slices; architect approval for single-unit plans |
| Counting reruns | Two identical reruns = 2 attempts | Different hypothesis required; same rerun = 1 attempt |
| Scope-expansion abuse | Calling required gates "scope expansion" | Required gates are never scope expansion |
| First-question bail | Return when anything is unclear | Decide mechanical questions; surface only true user-only calls |
| Partial handoff | Return with gates pending | Acceptance bar is all-or-nothing |
| Silent give-up | "Mostly working" after two tries | Escalate with full attempts ledger |
| Serial-when-parallel | Disjoint units run one at a time | Fan out in the same turn |
| Stopping for context | Pausing because the session is long | Save to disk; continue |

## How deliver composes

`deliver` WRAPS `plan`, `build`, and `ship`. Those skills own the how of each
stage. `deliver` owns: run all of them, gate hard, override their exits, never
stop early.

- `plan` produces the spec `deliver` feeds to `build`.
- `build` runs under `deliver`; `deliver` overrides its phase-level stops.
- `ship` is the final stage when a PR is in scope; `deliver` overrides its red-stop.
- `code-review`, `verify`, `qa` are gates `deliver` refuses to skip.
- `delegation-guide` governs fan-out rules (inline context, disk bridges,
  parallelize disjoint, attempt caps).
- `conventions` governs disk discipline.

**Subagent tool.** Call with `{agent, task}` (single), `{tasks:[...]}` (parallel),
or `{chain:[...]}`. Always fully-qualify model ids; a bare name can resolve to a
keyless provider and hang the subagent forever.

| Agent | Role | When |
|---|---|---|
| `architect` | design, consistency check, single-unit approval | spec, shard, unit-sizing gate |
| `engineer` | unit implementation | every implementation unit |
| `deep` | hard problems, second-attempt escalation | 2nd-attempt escalation on a failing stage |
| `qa-lead` | UAT matrix build + review, coverage gaps | Phase 5, re-UAT |
| `security-lead` | STRIDE, OWASP, secrets, auth | Phase 4, before REVIEW #1 |
| `review` | cross-vendor adversarial review | every explicit review round |

## Acceptance bar

Return to the user ONLY when every row is true with evidence on disk.

| Gate | Passing | Proof |
|---|---|---|
| Build | exits 0 | fresh build output this turn |
| Tests | 0 failures, count >= baseline, new tests for new behavior (BASELINE-RED: no NEW failures vs baseline + affected/new tests pass; never reported green) | fresh test output this turn |
| Lint / typecheck | clean or repo-tolerated warnings only | fresh lint + typecheck output this turn |
| Full UAT | every matrix row passing with thick evidence | evidence files in .pi-agent/deliver/<slug>/uat/ |
| Security | security-lead ran; all CRITICAL/HIGH fixed | security report; findings in ledger |
| Review | >= 2 explicit cross-vendor rounds; final round clean | review_rounds[] in status.json; all findings closed or user-triaged |
| Findings | every finding fixed or user-triaged with separate user message | findings_ledger[] complete; no open items |
| Committed | changes committed on feature branch | git log output; PR URL if ship was in scope |

## Reporting

When the acceptance bar is met, return this bundle. No prose padding.

```
Delivered: <one line>
Build:      <exact command>  exit=<N>  <timestamp>
Tests:      <exact command>  exit=<N>  <passed> passed / <failed> failed  <timestamp>
Lint:       <exact command>  exit=<N>  <timestamp>
Typecheck:  <exact command>  exit=<N>  <timestamp>
Baseline:   BASELINE-GREEN|BASELINE-RED

UAT:        <N rows>, all pass
            evidence: .pi-agent/deliver/<slug>/uat/

Security:   <findings summary>  CRITICAL=<N> HIGH=<N> MEDIUM=<N> LOW=<N>

Review:     <N> rounds, model=<review_model>
            Round 1 verdict: "<VERBATIM verdict line from subagent>"
            Round 2 verdict: "<VERBATIM verdict line from subagent>"
            (repeat for each round)
            Final round clean: yes
            Findings fixed: <X>  user-triaged: <Y>

Committed:  <branch> <commit-sha>   PR: <url or "verified-local">

Open user-calls (if any): <one line each with the default taken or recommended>

Raw logs: .pi-agent/deliver/<slug>/
```
