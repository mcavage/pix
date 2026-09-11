---
name: deliver
description: Adaptive product and patch delivery with framing, architecture, scoped workers, real-user checks and independent review. Use for "cook and deliver" or "full send".
---
# deliver

Deliver an accepted patch or explicit blocker. The main agent owns framing,
integration, verification, and reporting. Product thinking is mandatory;
separate PM and specialist invocations are conditional.

## Choose the outcome

For a new product, idea-to-release request, or feature spanning user journeys,
load [product.md](references/product.md) before implementation. Product mode owns
framing, architecture, delegated work and release acceptance. A small diff does
not turn a product request into a bug fix. For one understood patch, use the
small loop below. Risk classification applies in either mode.

## Start small

Read repository instructions, then locate the real caller with a targeted symbol
search. Bound search output; read relevant ranges and tests. Do not dump whole
files or inventories first. Expand only to resolve a named question.
Do not preload every skill or reference. `deliver` owns the loop; `plan`, `build`,
and `ship` are stage entry points, not additional review pipelines.

Load conventions or specialist guidance only for an uncovered requirement or
explicit user request; do not recursively expand the skill catalog.
Keep scratch in `.pi-agent/` or `/tmp/`. Preserve user work; use an isolated branch
or worktree for changes. Never force-push or rewrite shared history. Update affected
docs with code. Write concise, factual reports; honor the active output style.
Repository safety rules and explicit user scope still apply.

Before code, write a short contract at `.pi-agent/deliver/<slug>/contract.md`
and a minimal `status.json` beside it. The contract contains:
- User, situation, and real entry point; observable success and error.
- Stable criterion IDs, unchanged behavior, and executable checks per criterion.
- Unresolved questions and the evidence needed to resolve them.

Classify by scope, risk, and uncertainty, not line count: **bounded** for one
understood outcome, **risky** for consequential or uncertain failure, **epic** for
multiple independently useful outcomes. Small does not mean safe. Automatic risky
triggers: credentials/auth/trust, data loss/migration, concurrency/cancellation,
public API/CLI compatibility, external side effects, recovery/retry. Record each
trigger and required proof, including interruption, retry, data preservation, and
trust where relevant. Reclassify when evidence changes. Risk requires proof, not
a fixed crew size or automatic handoffs.

## Implement one coherent outcome

For a bounded patch, the default is one implementer (you) and one independent reviewer. Do not shard a
bounded one-unit change just to create handoffs. The implementer cannot weaken
the contract to fit the patch. Contract changes require explicit rationale,
invalidate affected evidence, and must be challenged in independent review;
changing the promised user outcome requires the user's approval.

If a concrete uncertainty prevents a credible acceptance check, request an early
read-only challenge from the reviewer before implementing the affected part.
The challenger must not implement it. Ask for missing negative/boundary cases
and an oracle that rejects a plausible wrong patch. Resolve the gap before code.
Otherwise let the final reviewer challenge both tests and implementation.
Load [complex-work.md](references/complex-work.md) only when an unresolved question
needs a specialist or the outcome needs decomposition. Give each invocation one
explicit question and deliverable; include only its needed context and owned scope.

For behavior, use red/green/refactor through the real caller. Prove a behavioral
regression red-on-base where practical, then green on the candidate. Syntax/import
errors and unconditional failures are meaningless red. Use an isolated base;
never revert someone else's work. For refactors use characterization/equivalence;
for docs/config use relevant accuracy, example, and executable checks.

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

One clean review completes review: explicit LGTM/APPROVE, zero unresolved findings.
Fix findings and rerun affected checks; review a changed candidate again. A refuted
finding needs a focused independent disposition validation, not self-approval.
Do not add a second whole-patch review to an unchanged clean candidate.

## Finish honestly

Keep `.pi-agent/deliver/<slug>/status.json` small: contract, classification,
candidate, evidence, review identity/verdict, findings, and decision; link logs.
Accept only when all criteria have current candidate-bound proof and review is
clean. Errors, timeouts, empty final output, or exhausted supplied budgets are
blocked outcomes even if a subprocess exits 0. Report partial patch correctness
separately from workflow completion. After two distinct failed hypotheses, seek a focused specialist or report the blocker.

Respect no-commit and no-push requests; commit or `ship` only when in scope.
Report outcome, contract/candidate, checks, review, findings, limitations, and logs.
Do not claim background continuation without an observable running worker.
Use existing cost accounting; unknown cost is not zero. Paid evaluations require
an explicit total cap covering children and retries. Ordinary delivery must not
invent a dollar cap unless the user supplied one.
