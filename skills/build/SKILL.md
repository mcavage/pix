---
name: build
description: Implement and integrate a product contract with scoped workers, real-user verification and independent review. Use for "build X" or "implement this".
---
# build

Use [deliver](../deliver/SKILL.md) as the single implementation and acceptance
loop. A new product or substantial feature takes its product path; an understood
patch takes the bounded path. A valid contract from `plan` is input, not a reason
to repeat discovery or create another set of spec files.

For product work the main agent owns decisions, interfaces and integration;
engineer workers implement independently testable caller outcomes. Use isolated
worktrees for concurrent units, explicit file ownership, relevant tests and
bounded result artifacts. Workers do not run nested product crews. Respect
no-commit requests; otherwise follow repository commit conventions. Preserve
user changes and partial work before cleanup; never force-remove an uncollected
worktree because a worker returned an error.

Verify the stable integrated candidate, not only individual components. QA reports
and reproduces gaps without editing tests; send fixes to the implementer. Security
and UX assessments cover actual changed boundaries and interactions. Changed
source invalidates affected evidence and review. A clean independent review of
an unchanged candidate does not need another confirmation pass.

A prototype request may narrow scope and release target explicitly. "Quick" alone
does not waive correctness, data preservation or required repository checks. State
what is experimental and which product criteria remain unverified.

By default finish at verified-local, with the working artifact, checks, independent
review, limitations and branch/patch. If the user authorized shipping, continue
through `ship` using this same evidence and contract. Do not stop at a handoff when
the requested outcome includes release. Do not claim deployed from an open PR.
