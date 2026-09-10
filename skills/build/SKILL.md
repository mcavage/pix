---
name: build
description: Implement an agreed product with one accountable owner, optional parallel workers, verification and independent review. Use for "build X" or "implement this".
---
# build

Use [deliver](../deliver/SKILL.md) as the implementation loop. It owns scope/risk,
worktree isolation, verification, independent review, evidence and completion.
Do not layer another crew or second set of review gates on top of it.

For a new product or material feature, reuse the agreed PR/FAQ, PRD and architecture
from `plan`. If they are missing, establish product agreement before production
code using [product agreement](../deliver/references/product-agreement.md).
A clear approved proposal plus “build it” is sufficient authorization to execute
that scope; do not request the same approval again. A plan-only or exploratory
request does not authorize implementation. Bounded maintenance uses a short
contract without manufacturing product documents.

The main agent is the default implementer and integration owner. Delegate only
ready independent units with exact interfaces, owned files, a real caller,
acceptance checks and a return artifact. Use isolated worktrees for concurrency.
Do not require a second unit or specialist to make a small change look rigorous.
Workers implement their unit; the owner resolves product ambiguity, integrates,
and obtains independent review of the actual combined product.

For a UI, exercise the primary and failure journeys in a real browser and provide
actual desktop/mobile images to the reviewer. For a CLI/API, exercise the real
entry point and caller-visible failure/recovery behavior. A green unit suite alone
is not product acceptance. Tests must not manufacture missing production modules
or silently replace the real integration dependency with a fallback stub.

An explicitly requested disposable prototype can use reduced scope and checks;
record that exception and label it unverified where appropriate. “Quick” alone
does not waive data safety, product agreement or honest completion reporting.

Finish with the runnable artifact, exact start/check instructions, actual review
and remaining limitations. Write past-tense verification claims only from recorded
results. Do not commit, publish or push unless in scope. A local artifact is not a
production deployment, and `ship` must reuse current evidence rather than restart
the whole planning and review process.
