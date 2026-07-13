---
name: build
description: Ship a feature with the crew. Spec-first: compile the request into context-complete story files, then execute story-by-story across parallel worktrees with architect + engineer subagents, cross-vendor code review, QA + security, and a verification gate. Stops at verified-local changes on a feature branch; hand to `ship` to open the PR. Use for "build X", "implement this", "let's do this properly". For a throwaway to just see it, say so and it skips the rigor.
---
# build

Compile a request into context-complete work orders, then execute them with the
crew. The point: **think hard once**, then let each unit run **fresh and fully
briefed** so no agent drifts or runs out of context. Full crew is the default;
skip logic scales it down; the throwaway mode strips it to nothing.

`build` ends at **verified-local**: all changes committed on a feature branch,
every gate passed. It does NOT open the PR. Run `ship` for the release (rebase,
tests, cross-vendor review, version bump, PR). Upstream, `plan` produces the
spec `build` consumes; you can also start `build` straight from a clear request.

## Throwaway mode (opt-in)
If the user says "throwaway", "quick", "just to see it", or "prototype": skip the
spec, the crew, tests, review, security, and the PR. Ralph-loop a single agent to
a working thing as fast as possible, then show it. For a web artifact, cover the
interactive states that make it real (empty, loading, error, populated) and lean
on `design-system`. Say plainly that it is not production code. Everything below
is the real path.

## Pre-flight
1. Resume check: `.pi-agent/build/<feature-slug>/status.json` — if present, read
   it and skip to the current stage.
2. Git: `git rev-parse --is-inside-work-tree`. If yes, branch from the default
   branch. If no, scratch mode (skip branch management).
3. Create `.pi-agent/build/<feature-slug>/` for artifacts; ensure `.pi-agent/` is
   gitignored.
4. Baseline: run the project's build + test commands (detect from package.json,
   Makefile, go.mod, Cargo.toml, ...). Record pass/fail and test count. Note
   pre-existing failures; do not chase them later.
5. Write `status.json`: all stages pending, baseline recorded, `repo_root` from
   `git rev-parse --show-toplevel`.

All pipeline state lives on disk, not in context. Never tell a subagent to "read
the spec" — paste the relevant section inline. Disk is the bridge between stages.

## Phase 1: Spec (think hard, once)
Skip for bug fixes under ~50 lines. If `plan` already produced a PRD +
architecture, use it as the input and go to Phase 2.

1. **Understand.** Restate the goal in a few sentences; ask only the 1-3
   questions you truly need. Still exploratory ("should we even build this?")?
   Run `brainstorm`, or `plan` for a full pre-build review.
2. **PRD** → `docs/spec/PRD.md`: the JTBD ("when [situation], I want
   [motivation], so I can [outcome]"), goals / non-goals, epics, numbered
   functional requirements each with testable acceptance criteria and real edge
   cases (empty, error, overflow, permission, concurrency). Top 3-5 falsifiable
   assumptions; settle a low-confidence critical one with an experiment before
   speccing on top of it.
3. **Architecture** → `docs/spec/architecture.md`: stack, components, data
   models, key patterns/constraints, affected-file map, testability notes, and
   (if there's a dev-facing surface) a DX pass — API shape, naming,
   composability, progressive disclosure, defaults. **Read the real codebase
   first** and match what exists.

Self-check before sharding: could someone build this without coming back with
questions? No vague language; dependencies named and owned.

## Phase 2: Shard + plan (compile to work orders)
4. **Story files** → `docs/spec/stories/NN-<slug>.md`, each SELF-CONTAINED: the
   requirement(s) it satisfies, the architecture constraints that apply, explicit
   acceptance criteria, the exact files it touches, the read-context budget, the
   build/test command to verify it, and dev notes. Litmus test: an agent that
   read ONLY this file could implement it.
5. **Parallelization groups:** units in the same group MUST touch disjoint files
   — list every file per unit and cross-check; any overlap forces a later
   sequential step. Mark clearly:
   ```
   Step 1 (sequential): Unit A, foundation types
   Step 2 (parallel):  Unit B (cmd/foo.go), Unit C (internal/state.go), Unit D (docs/foo.md)
   Step 3 (sequential): Unit E, wire together
   ```
   Consistency check (fan out an `architect`): every architecture component has a
   unit, every unit traces to the spec, no parallel-group file conflicts. Verdict
   PASS / ISSUES; one revision loop max. Update `status.json`.

## Phase 3: Implement (engineer subagents, parallel via worktrees)
For each unit in dependency order:

- **Sequential units:** work on the feature branch directly, read only the listed
  files, run build + tests, commit (imperative subject, the `why` in the body,
  matching the repo convention).
- **Parallel units:** the orchestrator (not the engineer) sets up worktrees
  first, then fires one `engineer` subagent per unit in the same turn:
  ```bash
  git worktree add .pi-agent/build/<slug>/worktrees/<unit> -b feat/<slug>/<unit> HEAD
  ```
  Each agent gets its worktree absolute path, only its unit's files, the
  build/test commands, and an instruction to commit before returning. Test-first
  where there's logic to get right (`tdd`). After all complete, the orchestrator
  merges each unit branch back sequentially (`git merge --no-ff`); a conflict
  means the plan was wrong about disjointness — resolve manually. Clean up
  (`git worktree remove --force`, `git branch -D`, `git worktree prune`), then
  run full build + tests on the feature branch. Update `status.json`.

If a story reveals the spec was wrong, **stop, fix the spec, re-shard the
affected stories.** The spec is the source of truth, not the code.

## Phase 4: Review (cross-vendor, via `code-review`)
Diff from the feature-branch base + the spec as input. Evaluate correctness vs
spec, design adherence, security basics, error handling, test coverage, and
user-facing-surface consistency (help text, docs, examples). Verdict APPROVE /
REQUEST CHANGES (max 2 loops back to engineer) / BLOCK (escalate, stop). Write
`review-report.md`.

## Phase 5: QA + security (concurrent)
Fire both in the same turn.
- **QA (`qa-lead`):** run the full suite; enumerate uncovered edge cases (nulls,
  boundaries, error paths); write tests for gaps; re-run; audit user-facing
  surfaces not covered by tests. Write `qa-report.md`.
- **Security (`security-lead`; skip for refactor/docs/test-only):** STRIDE, OWASP
  Top 10, supply-chain audit on new deps, secrets scan, auth/authz. CRITICAL/HIGH
  → fix loop (max 2) then escalate; MEDIUM/LOW → note and proceed. Write
  `security-report.md`.

## Phase 6: Verification gate (no skip)
Run `verify` on each; do not proceed until all pass:
- **Tests:** full suite, zero failures, count >= baseline, new tests for new
  behavior.
- **Docs:** help text matches behavior, README/docs updated for any API/CLI/
  config change, examples actually run.
- **UAT:** build the feature branch, run the happy path + one error path, read
  the output. Not just "no crash."
Record results in `status.json`.

## Done (hand to ship)
`build` finishes here: feature branch, all gates green, worktrees cleaned up,
`status.json` complete. Summarize to the user (what was built, review verdict, QA
results, security findings, branch, commit log). To open the PR, run **`ship`**
— it owns the rebase, the release checks, and the PR.

## Skip logic
| Change type | Skip |
|---|---|
| Throwaway / "just to see it" | everything except a working artifact |
| Bug fix <~50 lines | Phase 1, DX pass |
| Simple feature | Phase 1 |
| Refactor | Phase 5 security, DX pass if no API/CLI change |
| Docs-only | Phases 4-5 |
| Test-only | Phase 1, Phase 5 security |
| Never skip | Phase 6 (tests, docs, UAT) |

## Retry + escalation
Max 2 loops per stage. On the third failure, escalate to the user with what was
tried, what failed, and a recommendation. Never loop silently. The user can
interrupt at any phase boundary; save progress to disk first.
