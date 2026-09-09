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


## Parallel units, not manufactured handoffs

For an epic, decompose independently testable outcomes with owned callers. Each
child runs the small delivery loop, not the full epic process.

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
