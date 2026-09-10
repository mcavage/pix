# Deliver a product outcome

Use this path for an idea, new product, or substantial feature. The main agent is
accountable for the product and integration; workers own bounded implementation.
Do not equate a plan, green unit tests, or an open PR with a shipped product.

## Frame and challenge before code

Read the existing product and real users' entry points. For a fuzzy idea, compare
materially different approaches, including improving the existing experience.
Choose the smallest coherent end-to-end outcome, explain the tradeoff, and identify
what would disprove it. Treat supplied research as evidence with provenance;
label assumptions. Never invent customer quotes, validation, adoption or impact.

Write the existing contract, not parallel PRD/FAQ/RFC copies. Include:
- Intended user, situation, present workaround, and observable value.
- Primary journey, important alternatives, non-goals and practical constraints.
- Architecture: ownership, data/interface contracts, dependencies, trust boundaries,
  failure/recovery behavior, and how the real entry point connects to persistence
  or external effects. Match the repository's stack unless a change is justified.
- Criterion IDs covering behavior and quality: usability, architecture, security,
  QA, UAT and operational release readiness. Specify executable proof and a
  plausible wrong implementation each critical check would reject.
- Release target and authority: local runnable artifact, preview, PR, or deployment;
  record permitted side effects, rollback and any supplied budget/deadline.

Before implementation, have `product-manager` challenge value, scope and journey
completeness, and `architect` challenge the design against the actual repository.
Give each the draft contract and one owned question, with read-only instructions;
they return findings and decisions, not replacement plans. Independent questions
may run concurrently. For an interactive product, include a designer's interaction
and state plan; for a CLI/API use DX instead. Reuse current equivalent evidence
from an earlier planning stage rather than re-invoking these roles.

Resolve contradictions in the one contract. The main agent chooses reversible
product/design tradeoffs within the user's goal and records why. Do not ask the
user to pick colors, libraries or story order. A changed promised outcome or an
ungranted external action still requires authorization. Continue independent work
when only part is blocked. A specialist may challenge the premise; do not suppress
it merely to keep the pipeline moving.

## Delegate and integrate

Use [complex-work.md](complex-work.md) to assign caller-sized work orders and
parallelize independent units in isolated worktrees. Each order includes the
contract slice, interfaces, owned files, tests, and output location. Supply concise
decisions and readable artifact paths, not the full conversation. Workers may
edit existing source within their assigned scope and must report spec conflicts.
They implement and test, without recursively starting another product crew.

Keep shared composition files with the integrator. Obtain a working vertical
slice early, then extend it; do not spend the whole run polishing plans before
anything can execute. Preserve partial source, checks and model metadata on
failure. A process error does not erase work or prove that the work is correct.

## Accept the integrated experience

On the stable integrated candidate, execute [verification.md](verification.md).
Use the real browser or CLI/API caller with synthetic data. Exercise setup,
primary value, error/recovery and return usage, including save/reload/revise when
state persists. A screenshot or success toast alone does not prove the journey.

Have `qa-lead` independently exercise the acceptance matrix, including cases the
implementers did not test. QA may run tests and a sandboxed browser/CLI through
available tools; it reports gaps and reproductions without editing source/tests.
Have `security-lead` examine actual trust/data boundaries and failure cases;
record a justified not-applicable result for a product with no changed security
surface. For visual work, use an independent design assessment of the running
candidate, including narrow viewport, keyboard access and important states.
The author of a surface cannot supply its independent acceptance assessment.

Return fixes to the owning implementer. After fixes, rerun affected checks and
obtain independent dispositions on affected findings. One final cross-vendor
review under [review.md](review.md) covers the integrated code and the evidence.
Do not automatically repeat full QA/security/design reviews or full suites on an
unchanged candidate. Missing tools or unexecuted checks remain unverified.

Record a concise acceptance table with product value/completeness, architecture,
code simplicity, security, tests, UX and UAT. Each dimension needs evidence and
remaining limitations. A numeric subjective score requires a stated rubric;
never average away a critical data-loss/security defect or incomplete journey.

## Release and close

Run the repository's release path only within the recorded authority. Delegate
`ship` for an authorized PR without restarting planning or reviewing an unchanged
candidate. Respect CI-owned versioning. After deployment, probe the actual release
and rollback path as relevant. If only a PR is authorized, say PR ready, not deployed.

Update the same status record through frame, implement, integrate, verify, review
and release. Separate product acceptance, workflow completion, and release state.
Report observed authors/reviewers, critical-path elapsed time, calls/cost when
available, human interventions, and artifact links. Do not add parallel worker
seconds as wall time. If blocked, name the specific next action and preserve all
work; do not claim an invisible background worker will finish later.
