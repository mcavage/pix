---
description: Get a cross-vendor review of the current diff or plan from the review subagent
argument-hint: "[what to focus on]"
---
Use the `subagent` tool in single mode with agent `review` to get an adversarial,
cross-vendor second opinion. Hand it the concrete artifact to review (the current
`git diff`, or the plan/claim in question) pasted into the task, not a summary.
Ask it to hunt for correctness bugs, security holes, and broken edge cases, and
to end with a one-line verdict (BLOCK / CONCERNS / LGTM). Relay its verdict and
the specific findings.

Focus: $ARGUMENTS
