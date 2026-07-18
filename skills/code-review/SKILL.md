---
name: code-review
description: Review the current diff for correctness and safety, then get a cross-vendor second opinion from the `review` subagent (a different model vendor than yours, so its blind spots differ). Use for "review", "code review", "check my diff", or as a gate before shipping.
---
# code-review

Two passes: your own analysis plus an independent cross-vendor adversary. The
point of the second pass is *different blind spots*, not a rubber stamp.

## Steps
1. **Scope the diff.** `git diff <base>...HEAD` (or the working tree if not yet
   committed). Identify the base branch if you don't know it. Read the *changed
   files*, not just the hunks.
2. **Your pass.** Hunt for these, roughly in order of damage. Record each as
   `path:line` + a concrete failure scenario:
   - **Secrets**: keys, tokens, passwords, high-entropy literals, real-looking fixtures.
   - **Correctness**: broken logic, unhandled edge cases, off-by-one, wrong assumptions, silent behavior changes.
   - **Injection / trust boundaries**: raw SQL with interpolated input; user input flowing into a prompt or a shell; untrusted tool output treated as instructions.
   - **Error handling**: swallowed exceptions, unchecked returns, retries with no limit, new paths with no fallback.
   - **Concurrency**: shared mutable state, races, missing locks/transactions, fire-and-forget tasks that need coordination.
   - **Breaking changes**: removed/renamed fields, changed signatures or types, stricter validation with no migration.
   - **Test coverage**: new logic with no test, changed behavior with stale tests, deleted tests with no replacement.
   - **Doc drift**: a changed user-facing surface (CLI verb/flag, config key, env var, default, public API, or a skill/agent's triggers) whose docs (man page, help/usage, README, `AGENTS.md`, CHANGELOG, or frontmatter `description`) were NOT updated in the same diff. Grep the changed identifier across the docs; an un-updated hit is a finding. Where the surface is enumerable, flag the absence of an anti-drift test as its own finding.
3. **Cross-vendor pass.** Spawn a `review` subagent (the `subagent` tool with
   `agent=review`) with the diff and your findings, instructed to
   *refute* your analysis and surface what you missed. It runs on a different
   vendor than your main model on purpose.
4. **Reconcile.** Merge both passes. Drop findings the adversary convincingly
   refutes; keep what survives. De-dup.
5. **Verdict.** Emit `BLOCK` (real defect), `CONCERNS` (worth fixing, not
   blocking), or `LGTM`, then the surviving findings (`path:line`, why, suggested
   fix).

Don't invent issues to look thorough, and don't wave through a real one. Skip
naming nits and formatting the repo already automates.
