---
name: code-review
description: Independently review a complete candidate for correctness, architecture, security and evidence quality. Use for "code review", "check my diff", or a missing release review.
---
# code-review

Use [the delivery review contract](../deliver/references/review.md) for candidate
identity, local secret scanning, observed model independence and finding disposition.
This is one review stage, not an extra pipeline after a valid delivery review.

1. Identify the base and complete shipping candidate: committed, staged, unstaged,
   untracked and explicitly ignored shipping files. Include the real callers and
   tests, acceptance contract and executed evidence. Preserve unrelated work.
2. Inspect correctness, architecture and simplicity, security/trust boundaries,
   error/recovery behavior, concurrency, compatibility, tests and documentation.
   Each finding needs a concrete failure/maintenance scenario and source location.
3. After local scanning, give the read-only `review` agent accessible source,
   a saved complete diff and the criterion/evidence map. It must challenge the
   implementation and acceptance assumptions, not merely agree with your summary.
4. Verify the reviewer's actual vendor from response metadata against all authors.
   Missing or same-vendor identity is not independent review. A role name or model
   self-description is not identity evidence.
5. Preserve findings and dispositions. Fix real defects, rerun affected checks and
   obtain follow-up review of the changed candidate. A refutation needs independent
   validation. Do not rerun a clean review because a workflow stage changed.

Emit BLOCK for defects or incomplete review, CONCERNS for documented unresolved
quality issues, or LGTM for complete clean coverage. Do not let CONCERNS silently
become product acceptance. Formatting enforced by tools is not a useful finding.
