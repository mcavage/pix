---
name: ship
description: Take the working tree from "done" to an open PR with green CI by rebasing, testing, reviewing, committing, pushing, and watching required checks. Use for "ship" or "make a PR".
---
# ship

Goal: working tree → open PR with green required CI checks. **Never merge**,
never force-push, never push to `main`/`master`, and never call a PR shipped
while its checks are queued or running.

## Steps
1. **Branch.** If on the default branch, create a feature branch first. Identify
   the base branch from the remote default.
2. **Rebase on base.** Fetch, then rebase onto the base branch. Resolve understood conflicts while preserving both intended behaviors; run
   affected checks. Abort only when the conflict cannot be resolved reliably.
3. **Status.** `git status` + `git diff` to see exactly what's shipping.
4. **Tests.** Reuse current candidate-bound evidence from `deliver` when source,
   contract, inputs and environment match. Run missing repository checks and
   affected checks after rebasing or changes. Fix new failures within scope;
   preserve baseline failures explicitly. Never claim a failing check passed.
5. **Lint.** Run the linter if the repo has one. Warnings don't block unless the
   repo treats them as errors. If there's no linter, say so.
6. **Review gate.** Reuse a clean independent review covering this exact candidate.
   Otherwise use `code-review`; address findings and verify affected paths before
   follow-up review. New source edits require current review, not a second review
   simply because execution moved from build to ship.
7. **Docs gate (no drift ships).** If the diff changes any user-facing surface,
   the docs that describe it MUST change in the SAME PR, never "later":
   - CLI verbs/subcommands, flags, config keys, env vars, defaults → man page,
     `--help`/usage text, README, and `AGENTS.md` (or the repo's equivalents).
   - public API / exported behavior → its reference docs and the CHANGELOG.
   - a skill/agent's triggers or behavior → its own `description` frontmatter.
   Grep the diff for the changed identifier across the doc set and reconcile every
   hit. **Prefer a test over vigilance:** where the surface is enumerable (a verb
   table, a config-key list), add or extend an anti-drift test that fails when
   code and docs diverge (see `conventions` → "Docs travel with code"; e.g.
   pix's `man_test.go` gates every verb AND every config key). If you had to
   fix drift by hand and no such test exists, add one now so it can't recur.
8. **Version + changelog.** Follow the repository release owner. Do not manually
   bump versions or generated changelogs when CI owns them. Add authored release
   notes only where that repository requires them.
9. **Commit.** Imperative subject, the *why* in the body. Follow the repo's
   existing commit convention.
10. **PR.** Push the branch (`-u` if it has no upstream) and `gh pr create` with a
    concise title and this body:
    ```
    ## Summary
    ## Testing
    ## Risks
    ```
11. **CI gate.** Watch the PR's required checks to completion with
    `gh pr checks --watch --fail-fast`. Queued or running checks are not success.
    If a check fails, inspect its job log, reproduce the failure where possible,
    fix the root cause, rerun the local quality and review gates affected by the
    fix, commit, push, and watch CI again. Repeat until required checks pass or a
    failure needs user action. Never dismiss a failure as flaky without evidence.

Report the PR URL, test/lint results, review verdict, and final CI result. If a
step fails (tests red, rebase conflict, `gh` error, or CI failure that cannot be
fixed), stop and report precisely what failed so no work is lost. Do not merge
or deploy.
