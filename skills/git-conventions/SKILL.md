---
name: git-conventions
description: Branch, worktree, history, and PR operations. During deliver, use its embedded Git rules; load this only for an uncovered Git operation or an explicit request.
---
# git-conventions

## Branch discipline

Never work directly on `main` or `master`. Always branch from the default branch:

```bash
git checkout main && git pull
git checkout -b feature/<slug>
```

One commit per logical implementation unit. Commit messages: imperative mood, present tense, under 72 characters. "Fix login redirect" not "Fixed" or "Fixing login redirect". The body (optional) explains the *why*, not the *what*.

Iron laws:
- **Never force-push.** No `--force`, no `--force-with-lease`.
- **Never rewrite shared history.** If the branch has been pushed, merge instead.
- **Rebase before PR.** If the rebase produces more than 3 conflicts or structural collisions, abort and surface it to the user rather than guessing through it.
- Add generated artifacts and agent scratch dirs to `.gitignore`, not to the repo.

## Worktree-First rule

Never `git checkout` in the main working directory to do parallel work. Use worktrees:

```bash
git worktree add /tmp/feature-pr feature/<slug>
# do all work inside the worktree
git worktree remove /tmp/feature-pr
```

This applies to: merge conflict resolution, rebasing, cross-repo work, and any parallel implementation. The main working directory stays on its branch.

## GitHub operations

Use `gh` CLI for all GitHub operations:

- **Issues:** `gh issue list`, `gh issue view`, `gh issue create`
- **PRs:** `gh pr list`, `gh pr view`, `gh pr create`, `gh pr diff`, `gh pr merge`
- **Code search:** `gh search code 'pattern' --repo owner/repo`
- **API fallback:** `gh api repos/owner/repo/pulls/NUMBER/reviews`

When shipping, use the `ship` skill, which runs tests, lint, `code-review`, and opens the PR with a structured body. Do not open PRs manually unless `ship` is inappropriate for the task.
