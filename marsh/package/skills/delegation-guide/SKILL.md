---
name: delegation-guide
description: Prepare bounded role work orders and use marsh for child jobs, private forks, dependencies and cleanup.
---
# Delegation guide

Load the `marsh-orchestration` skill for executable forms. There is no custom
subagent tool or task/chain object. Delegate only when the unit has a real caller,
owned files, acceptance checks and a useful return artifact. The parent retains
product decisions, integration and the final verification/report.

For independent shared-file-safe work use fanout/collect; for concurrent edits
use split/join. Dependent steps wait for accepted predecessor output. A reviewer
that needs an implementation must run against the integrated candidate, not a
sibling fork's original snapshot. Use public role prompts and explicit model
flags where appropriate, but prove identity/usage from actual response metadata.

Supply the agreed scope, accessible input paths, bounded context, file ownership,
whether edits/commits are allowed, tests, constraints and expected output. Honor
spawn limits, no-replay policy and spending caps. Do not manufacture a crew or
invent a budget. Empty output, truncation, refusal and errors remain incomplete.
