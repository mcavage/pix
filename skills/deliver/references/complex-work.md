# Escalate a concrete uncertainty

## Conditional specialists

Every invocation gets an explicit question and deliverable, with bounded context
and owned output. Choose a role because it can resolve a concrete question:

- `security-lead`: actual trust/security triggers require an adversarial boundary
  assessment and attack/failure checks, not an audit of every shipping change.
- `qa-lead`: broad acceptance risk across workflows/platforms beyond the focused review; deliver a focused matrix and executed evidence.
- `product-manager`: uncertain user outcome, scope tradeoffs, or epic decomposition;
  deliver decisions and acceptance criteria, not a ceremonial PRD or closeout.
- `architect`: architecture uncertainty, dependency boundaries, or epic decomposition;
  deliver a constrained design or child contracts and integration checks.
- DX, designer, copy, legal, finance, or SRE: only when that surface is touched
  and an unresolved question warrants the role; specify the decision or check.

Independent investigations may run concurrently. Do not schedule extra roles to
make a small change look rigorous; do not omit a needed role to keep it "bounded".


## Plan decisions before distributing edits

The main model owns product intent and architecture. In the existing contract,
record the user journey, why the chosen scope solves it, the simplest coherent
design, shared data/interface contracts, error behavior, and acceptance checks.
Choose among material alternatives; do not produce a separate PRD/RFC by default.
Resolve an interface uncertainty before delegating code that depends on it.

Use the configured `engineer` for implementation and report the actual model; do not
claim cheaper execution from a role name. Do not silently change model bindings.
Keep the main model on consequential decisions and integration, rather than
writing all implementation while workers merely research. Escalate a specific
failed hypothesis with its evidence; avoid restarting the entire task on a
stronger model. Workers must flag contradictions and missing acceptance cases.

## Parallel units, not manufactured handoffs

Decompose by independently testable caller outcomes with owned files, stable
interfaces and integration checks. Each child runs the small delivery loop: implement its contract slice and prove it through its caller. Children do not
recursively launch planning/review crews; one independent review covers the full
integrated patch and every author's observed vendor.

Do not shard a bounded one-unit change just to create handoffs. Identify the full
dependency DAG, including shared-file conflicts. Independent units are PARALLEL
BY DEFAULT: a shared working tree is never a reason to serialize. Create one
isolated git worktree per concurrent unit; launch the whole ready wave in one
parallel `{tasks:[...]}` call in the same turn. Serialize only a real dependency
edge or file-conflict edge. Give each worker the contract slice, interface
examples, owned files/caller, tests and return path. Do not give every worker the
whole planning transcript. Reserve shared composition files for the integrator.

Collect patches and evidence. Merge reviewed commits after collecting results
when current review already exists and commits are authorized; otherwise integrate
returned patches without committing for the combined review. Do not require a
separate review for every unit. Preserve unrelated work.
Remove worktrees only after changes and evidence have been preserved. Child
evidence proves its child candidate, not the merged tree: execute real integrated
journeys and relevant regressions, then obtain one cross-vendor independent review.
Include planning, worker retries, integration, review and blocked time in elapsed
and cost reporting; parallel worker durations must not be added as wall time.
