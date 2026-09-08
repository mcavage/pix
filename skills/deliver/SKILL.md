---
name: deliver
description: Adaptive delivery from user outcome to verified, independently reviewed patch. Size work and crew by scope, risk, and uncertainty. Use for "cook and deliver" or "full send".
---
# deliver

Deliver an accepted patch or explicit blocker. Product thinking is mandatory;
separate PM and specialist invocations are conditional. Use the smallest team
that can resolve the problem and independently challenge its proof.

The top-level orchestrator owns framing, contract, integration, evidence, and report.
The workflow is outcome/risk framing -> acceptance contract/minimal plan ->
independent test challenge -> bounded implementation -> candidate-bound verification
-> local secret scan -> one independent review -> accepted patch or explicit blocker.

## 1. Frame and classify

Identify the user's problem and the real production caller before proposing a
solution. Inspect relevant code, existing checks, and repository instructions.
Classify by scope, risk, and uncertainty, not line count. Small does not mean safe.

- **Bounded:** one coherent outcome and implementation unit, known boundaries,
  understood acceptance checks, and no automatic risky trigger.
- **Risky:** one coherent outcome with a consequential failure mode or unresolved
  behavioral uncertainty. Automatic triggers are credentials/auth/trust,
  data loss/migration, concurrency/cancellation, public API/CLI compatibility,
  external side effects, and recovery/retry. Any trigger makes the work at least
  risky, even when the diff is tiny. Name the failure and the required proof.
- **Epic:** multiple independently useful outcomes or cross-boundary uncertainty
  that prevents one credible contract and implementation unit. Record risky
  triggers on the epic and its children; decomposition does not erase risk.

Reclassify when evidence changes: new callers, side effects, unknown invariants,
failed assumptions, or expanding scope. Record the reason, revise the affected
contract/checks/team, and pause dependent implementation until the gap is resolved.
For an epic, do product/architecture decomposition into independently testable
children with owned entry points, contracts, and dependency edges. Each child runs
the small loop, not the full epic process. Preserve integration acceptance too.

## 2. Agree the acceptance contract

Write a short contract with stable criterion IDs and a minimal plan before code.
For bounded work, a few precise bullets suffice; do not manufacture a PRD.
Include:

- **User + situation + real entry point:** who needs what, when, and through which
  CLI/API/UI/tool/skill caller. A helper-only test does not prove caller wiring.
- **Observable success and error:** outputs, state changes, failure signals, and
  an oracle that distinguishes the intended result from a plausible wrong one.
- **Unchanged behavior:** compatibility and invariants outside the requested delta.
- **Failure paths:** interruption, retry, data preservation, and trust where relevant;
  explain exclusions rather than silently ignoring a risky trigger.
- **Executable checks:** criterion -> command/steps, fixtures and test inputs,
  expected result, relevant environment/profile, and shipping-file scope.
- **Unresolved questions:** assumptions, how to resolve them, and what they block.

The implementer cannot weaken the contract, delete failing criteria, or redefine
success to fit the patch. Contract changes require the orchestrator's explicit
rationale, renewed independent challenge, and invalidate affected evidence.
A change to the user's promised outcome requires user approval, not agent triage.

## 3. Challenge the tests before implementation

Require an independent pre-implementation test challenge by a separate read-only
agent invocation. The challenger must not implement the unit. Give it the contract,
relevant caller code, existing tests, risky triggers, and unresolved questions.
Ask which missing negative and boundary cases could let an incorrect patch pass.
Deliverable: criterion/check mapping, credible counterexamples, proposed oracles,
and gaps to resolve before coding. This is not a mandatory full QA engagement.

Resolve challenge findings in the contract/check plan first. Then dispatch one
bounded `engineer` or `deep` implementation unit with the challenged contract,
owned files, minimal plan, baseline, exact checks, and stop conditions inline.
An unavailable independent challenger is a blocker, not permission to self-approve.

## 4. Implement and prove the change

For behavior, use red/green/refactor through the real caller: observe the intended
failure, make the smallest change, observe success, then refactor without changing
the contract. Require red-on-base for a behavioral regression where practical:
run the regression check against the recorded base without the fix, then the
candidate. Use an isolated base checkout, never revert someone else's work.

Reject meaningless red: syntax/import failures, unconditional failing assertions,
or a check that fails only because the new test file is absent. The failure must
expose the missing behavior. If red-on-base is impractical, record why and the
alternative fault-detection evidence; do not pretend a passing-only check is red.
For refactors, use characterization/equivalence against existing behavior, including
errors and side effects. Do not invent a failing behavior test for a pure refactor.
Docs/config/test-only edits need accuracy, examples, links, and relevant executable
checks; a prose change alone does not justify rebuilding an image.

The orchestrator may make a tiny mechanical shipping edit when delegation costs
more than the change; record why. This does not make direct coding the default.
Behavior, security, concurrency, and recovery work goes to an implementation subagent.
The mechanical exception changes authorship only, not challenge, verification, or review.
This recorded exception overrides `delegation-guide`'s general rule that the
orchestrator never writes the unit itself, and it overrides that rule only while
`deliver` is active. Every other `delegation-guide` requirement still binds in
full: context passing, prescriptive prompts, parallel dispatch, file discipline,
per-unit worktrees, and escalation limits.

## Parallel units, not manufactured handoffs

Do not shard a bounded one-unit change just to create handoffs. When there really
are independent units, identify the full dependency DAG, including shared-file
conflicts. Independent units are PARALLEL BY DEFAULT: a shared working tree is
never a reason to serialize. Create one isolated git worktree per concurrent unit;
launch the whole ready wave in one parallel `{tasks:[...]}` call in the same turn.
Serialize only a real dependency edge or file-conflict edge. Collect results, then
merge reviewed commits after collecting results when commits are authorized;
otherwise integrate returned patches without committing. Preserve unrelated work.
Remove worktrees only after accepted changes and evidence have been preserved.
Child evidence proves its child candidate, not the merged tree. Verify affected
integration paths and review new integration seams, not the entire epic process.

## 5. Verify the exact candidate

Record base and head SHAs plus a candidate digest. Compute SHA-256 over
length-delimited base/head records, staged and unstaged binary diffs, and a sorted
manifest of untracked shipping files including paths, types, modes, symlink targets,
and content bytes. Include ignored shipping files explicitly; never omit a new
shipping test because Git has not tracked it. Exclude logs/status under `.pi-agent/`,
not shipping inputs. Keep the shipping inventory with the evidence.

Every check binds candidate digest + contract digest to criterion IDs, exact
command, cwd, relevant environment/profile, test inputs, start/end timestamps,
exit code, actual result, and log path. Record toolchain/lockfile/fixture identities
and external-state versions or probes; redact credentials, not the useful result.
Fingerprint before and after each command; a mutation during a check invalidates
that check until rerun on a stable candidate. Do not validate a moving worktree.

Candidate, contract, or input changes invalidate affected evidence. Record impact
analysis and rerun affected checks; when impact is uncertain, widen verification.
Reuse evidence for an identical candidate, contract, inputs, and environment;
do not rerun full suites between unchanged gates. Run required repository gates
at the appropriate integrated scope, not a full repository build after every edit.

Final claims still require fresh evidence in the current turn per `verify`: run
the full command supporting each claim and read its output. A digest check alone
is not a test rerun. After resuming, old logs are historical evidence, not a fresh
"tests pass" claim. Reuse a current-turn run across unchanged gates.
Keep baseline-red failures explicit: no new failures, affected/new checks pass,
and the known failure set is unchanged or reduced. Never report that as all green.

## 6. Review once, then follow findings

One focused independent cross-vendor review is the default. Dispatch a read-only
`review` invocation after verification. Supply the contract, full base-to-candidate
change (committed, staged, unstaged, and untracked shipping content), relevant
caller context, criterion/evidence mapping, risky triggers, findings, and limitations.
Ask for concrete failure scenarios with file/line evidence and a verdict.

Before any diff, file content, or log excerpt leaves for a model reviewer, run a
local secret scan over the exact bytes you intend to send: use the repository's own
scan or gate where one exists, plus a working-content scan if it covers only history.
Here `scripts/check-secret-history.sh` scans committed refs, not dirty or untracked
bytes, so it is insufficient alone. Apply an equivalent local high-entropy and
credential-pattern check to outgoing content, including untracked shipping files. An unresolved detection blocks the prompt; clear
it by removing or rotating the credential, or by recording a reviewed allowlist
decision for a known fixture or example. Redact matched values while preserving
paths, line numbers, and surrounding context so the reviewer can still judge the
code. A model reviewer is never the first secret boundary: it sits behind the
local scan, and its own detection is a late backstop, not the gate.

Prove identity from assistant response metadata in `details.results[]`:
`messages[]` entries with role=assistant carry `provider` and `model`. Compare the
reviewer's observed vendor to every implementation author's observed vendor, not
the orchestrator's vendor alone. Never accept self-report or requested model as
proof. A gateway or local runner is not itself the model vendor; resolve the
observed model's vendor from catalog evidence, or mark it unknown.
Environment bindings remain authoritative under existing model precedence.
There is no hardcoded model table or silent model fallback in this workflow.
Same-vendor or unknown identity is a review blocker: use an authorized configured
cross-vendor binding, or report the missing capability without calling it reviewed.

Clean means explicit LGTM or APPROVE, zero unresolved findings, and complete scope.
Timeout, error, or missing context is not a clean review. A clean first review
finishes the review stage. Do not require a clean second review of an unchanged
candidate, and do not add a confirmation pass to a clean one. Record every finding; fix it, substantiate a refutation, or
obtain explicit user acceptance of that specific risk. Never silently drop or
downgrade findings. A finding that needs a fix causes fix + impacted verification
+ another review because the candidate changed. New shipping edits invalidate its
review even if described as a nit.

When a finding is refuted and the candidate therefore stays unchanged, the review
is not accepted until a short independent validation invocation reads the finding,
the refutation, and its evidence, and explicitly accepts that disposition. Bound
it to the disposition; it is not a second review of the whole candidate, and a
refuted finding never becomes acceptance by the orchestrator's own assertion.

## Conditional specialists

Every invocation gets an explicit question and deliverable, with bounded context
and owned output. Choose a role because it can resolve a concrete question:

- `security-lead`: actual trust/security triggers require an adversarial boundary
  assessment and attack/failure checks, not an audit of every shipping change.
- `qa-lead`: broad acceptance risk across workflows/platforms beyond the mandatory
  independent test challenge; deliver a focused matrix and executed evidence.
- `product-manager`: uncertain user outcome, scope tradeoffs, or epic decomposition;
  deliver decisions and acceptance criteria, not a ceremonial PRD or closeout.
- `architect`: architecture uncertainty, dependency boundaries, or epic decomposition;
  deliver a constrained design or child contracts and integration checks.
- DX, designer, copy, legal, finance, or SRE: only when that surface is touched
  and an unresolved question warrants the role; specify the decision or check.

Independent investigations may run concurrently. Do not schedule extra roles to
make a small change look rigorous; do not omit a needed role to keep it "bounded".

## State and cost

At framing, write `.pi-agent/deliver/<slug>/status.json`; update after each stage.
Keep contracts and logs nearby, referenced rather than copied into every record.
This is a minimal schema; arrays hold real records, not invented zero-cost results:

```json
{
  "classification": {"kind": "bounded|risky|epic", "reason": "...", "triggers": []},
  "contract": {"path": "contract.md", "digest": "sha256:...", "unresolved": []},
  "stages": {"frame": "done", "contract": "done", "challenge": "pending", "implement": "pending", "verify": "pending", "review": "pending"},
  "candidate": {"base": "sha", "head": "sha", "digest": "sha256:...", "shippingManifest": "shipping.json"},
  "evidence": [{
    "candidate": "sha256:...", "contract": "sha256:...", "criteria": ["AC-1"],
    "command": "...", "cwd": "...", "environment": "redacted profile/toolchain identity",
    "inputs": {"manifest": "inputs.json", "digest": "sha256:..."},
    "startedAt": "ISO-8601", "endedAt": "ISO-8601", "exit": 0,
    "result": "pass|fail|baseline-red", "log": "checks/1.log"
  }],
  "subagents": [{
    "id": "...", "agent": "...", "question": "...", "deliverable": "...",
    "resultRef": "tool-call-id/details.results[0]", "startedAt": 0, "endedAt": 0,
    "durationMs": 0, "usage": {}, "observedModels": [], "exitCode": 0
  }],
  "findings": [{"id": "F-1", "source": "...", "text": "...", "status": "open|fixed|refuted|user-accepted", "evidence": "...", "dispositionValidatedBy": null}],
  "decision": {"status": "pending|accepted|blocked", "reason": "...", "next": null}
}
```

Copy subagent timing (`startedAt`, `endedAt`, `durationMs`), usage, and model
metadata from `details.results[]`; retain result references for single, parallel,
and chain calls, including failures/retries. Do not infer duration from log order.
Use the existing usage/cost accounting; there is no separate rate table.

Paid evaluations require one explicit total cap before any run. Optional
`evaluationBudget` records currency, totalCap, spent, reserved, and user approval.
All child, retry, and cloud costs count, including orchestration and external
services. Reserve the bounded cost of in-flight work before launching more; stop
if the remaining cap cannot cover the run. Unknown cost is not zero: resolve it
or block the evaluation. Do not double-count nested usage already aggregated.
Ordinary delivery must not invent a dollar cap unless the user supplied one.

## 7. Decide and report

Accept only when each contract criterion has valid evidence, required risky-path
proof and independent challenge are complete, the current candidate has a clean
review, and findings have supported dispositions. Otherwise fix within scope or
report an explicit blocker, missing proof, and the next concrete action.
After two distinct failed hypotheses at a stage, consult `deep` or the relevant
specialist; if still blocked, report the attempts instead of silently retrying.
User-only decisions, unavailable capabilities, and exhausted supplied budgets
are legitimate blockers. Do not weaken acceptance to avoid reporting one.

Respect explicit no-commit and no-push requests. Commit or invoke `ship` only when
in scope; never merge a PR or override repository safety rules. Use `plan`/`build`
techniques when needed, not their entire crew/review ceremony for every child.
This adaptive contract owns team size, review count, and the mechanical-edit
exception while `deliver` is active; `delegation-guide` still supplies context,
file, and worktree discipline, and `verify` owns fresh claims.

Report outcome, classification, contract and candidate identity, exact checks and
results, review identity/verdict, findings, limitations, and status/log paths.
Report only completed work; do not claim background continuation after returning
unless an observable independent worker is actually running.
