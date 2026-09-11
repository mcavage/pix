---
name: deliver
description: Adaptive delivery with a small implementation loop, executable proof, and one independent review. Use for "cook and deliver" or "full send".
---
# deliver

Deliver an accepted patch or explicit blocker. The main agent owns scope,
implementation, verification, and reporting. Product agreement precedes new product implementation;
separate PM and specialist invocations are conditional.

## Start small

For a new product or material feature, first load
[product-agreement.md](references/product-agreement.md). Reuse the approved
PR/FAQ and PRD when provided. Reuse existing approval; brainstorming alone is not approval to code.

Read repository instructions, then locate the real caller with a targeted symbol
search. Bound search output; read relevant ranges and tests. Do not preload `plan`, `build`, `ship`, `delegation-guide`, or all the references
below. `deliver` overrides their unconditional crew, authorship, and review-count rules, but never suppresses product agreement.

Keep scratch in `.pi-agent/` or `/tmp/`; preserve user work in an isolated branch
or worktree. Never force-push or rewrite shared history. Update affected docs; write concise, factual reports.
Repository safety rules and explicit user scope still apply.

Before code, write a short contract at `.pi-agent/deliver/<slug>/contract.md`
and a minimal `status.json` beside it. The contract contains:
- User, situation, and real entry point; observable success and error.
- Criterion IDs, unchanged behavior, and executable checks.
- Unresolved questions and required evidence.

Classify by scope, risk, and uncertainty, not line count: **bounded** for one
understood outcome, **risky** for consequential or uncertain failure, **epic** for
multiple independently useful outcomes. Small does not mean safe. Automatic risky
triggers: credentials/auth/trust, data loss/migration, concurrency/cancellation,
public API/CLI compatibility, external side effects, recovery/retry. Record triggers and proof for interruption, retry, data preservation and trust. Reclassify when evidence changes. Risk requires proof, not crew size.

## Implement one coherent outcome

The default is one implementer (you) and one independent reviewer. Do not shard a
bounded one-unit change just to create handoffs. The implementer cannot weaken
the contract to fit the patch. Contract changes require explicit rationale, invalidate affected evidence, and independent review;
changing the promised user outcome requires the user's approval.

If a concrete uncertainty prevents a credible acceptance check, request an early
read-only challenge from the reviewer before implementing the affected part.
The challenger stays read-only; request negative cases and a wrong-patch oracle.
Otherwise let the final reviewer challenge both tests and implementation.
Load [complex-work.md](references/complex-work.md) only when an unresolved question
needs a specialist or the outcome needs decomposition. Give each invocation an explicit question and deliverable with bounded context.

For behavior, use red/green/refactor through the real caller. Prove red-on-base where practical, then green on the candidate. Syntax/import
errors and unconditional failures are meaningless red. Use an isolated base;
never revert someone else's work. Use characterization/equivalence for refactors and accuracy checks for docs/config.

Before final verification, exercise the first working product against its original
user journey, not only its checklist. Improve in-scope friction, feedback and recovery.
Keep product specs current.

## Verify, then review

When a candidate exists, load [verification.md](references/verification.md).
Bind checks to the contract, full candidate (including dirty, untracked, and
ignored shipping files), inputs, and environment. Record command, timestamps,
exit, result, and log. Run required repository gates at the appropriate scope.
Candidate changes invalidate affected evidence; do not rerun unchanged full suites
between gates. Never infer correctness or completion from exit 0 alone.

Before dispatching the reviewer, load [review.md](references/review.md). Locally
scan the exact outgoing content for secrets first. The reviewer reads the actual
patch, caller, criteria, negative cases, and check evidence. Prove cross-vendor
identity against implementation authors using response metadata, not self-report.
No independent reviewer, unknown identity, or incomplete coverage means blocked.

One complete review suffices: LGTM/APPROVE or explicit approval with nonblocking
CONCERNS; zero unresolved blocking findings. Record nonblocking dispositions.
Fix material findings, rerun affected checks and obtain focused delta review.
A disputed blocking finding needs independent disposition validation.
Do not add a second whole-patch review to an unchanged clean candidate.

## Finish honestly

Keep status small: contract, classification, candidate, evidence, review, findings
and decision; link logs.
Accept only when all criteria have current candidate-bound proof and review
covers the candidate with no unresolved blocking findings. Errors, timeouts, truncated or empty final output, or exhausted supplied budgets are
blocked outcomes even if a subprocess exits 0. Report partial patch correctness
separately from workflow completion. After two distinct failed hypotheses, seek
a focused specialist or report the blocker.

Respect no-commit and no-push requests; commit or `ship` only when in scope.
Write completion claims in README/status/final text only after the corresponding
checks and review have recorded results. Missing evidence remains pending or
blocked, never passed. Report outcome, candidate, checks, review, and limitations.
Use existing cost accounting; unknown cost is not zero. Paid evaluations require
an explicit total cap covering children and retries. Ordinary delivery must not
invent a dollar cap unless the user supplied one.
