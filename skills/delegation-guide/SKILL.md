---
name: delegation-guide
description: Prepare bounded work orders and parallel isolated workers when delegation is useful; retain main-agent product ownership.
---
# delegation-guide

The main agent owns product decisions, architecture and integration and can
implement directly. Delegate when ready units have clear boundaries, not to keep
a crew busy. Do not manufacture shards or another planning/review team.

## A useful work order

Include the agreed product excerpt, exact interfaces/examples, owned files and
real caller, acceptance checks, constraints, and required return artifact. Inline
small decisions; give accessible paths for large source/spec/evidence and require
the worker to read the relevant ranges. A path the child cannot access is not
context. Do not paste entire histories or raw datasets into every handoff.

Workers make routine choices within scope. A material product/interface
contradiction is a useful escalation, not a reason to retry a vague order blindly.
Specify writable scope and whether commits are authorized. Coding workers write
real code in their assigned worktree; reviewers stay read-only. Tests must not
synthesize missing production modules to obtain a green result.

## Optional parallel implementation

Once delegated units are ready, independent units are PARALLEL BY DEFAULT.
Identify the dependency DAG, including shared-file conflicts. Create one isolated
git worktree per concurrent unit. Launch the whole ready wave in one parallel
`{tasks:[...]}` call in the same turn. Serialize only a real dependency edge or
file-conflict edge; a shared working tree is never a reason to serialize.
Respect the tool's configured concurrency limits rather than assuming a number.
Use `chain` only when a step consumes its predecessor's actual output.

Collect results, then integrate worker commits after collecting results when
commits are authorized; otherwise integrate returned patches without committing.
Preserve partial work and attribution. Remove worktrees only after their useful
patches and evidence are preserved. The main owner verifies affected integration
paths and obtains independent review of the combined product. Unit checks do not
prove the merged tree or require another full review crew per unit.

## Models and evidence

Use the existing `engineer`, `fanout`, `deep` or `review` presets as appropriate.
Model selection follows explicit frontmatter, environment bindings, then inherited
context; a role name is not a vendor guarantee. Verify observed identities from
response metadata. Do not switch vendors or bypass a supplied spending cap.

Record actual returned checks, usage, timing, stop reason and partial artifacts.
Truncation, empty output and errors remain incomplete even with process exit zero.
Do not accept an author's assertion as proof of successful integration or review.
For release evidence and focused repair/review, use the existing `deliver` loop;
do not restart unchanged gates or claim a planned check already passed.
