---
name: architecture-audit
description: Full-spectrum architecture audit with system mapping, domain/API review, semantic-diff checks, crew findings, and a ship recommendation. Use before GA, open source, or a major refactor.
---
# architecture-audit

Use this for a repository, service, or unusually large PR where ordinary diff
review cannot answer whether the system is well-shaped. This is an audit, not a
rewrite. Read broadly, measure, delegate independent passes, verify the strongest
claims, then recommend a sequenced program of work.

## Ground rules

1. Read a file before judging it. Every finding cites `path:line`.
2. Label facts **Observed** and hypotheses **Inferred**. Put anything not checked
   under **Unverified**.
3. Rank concrete failure or maintenance cost, not disagreement with a pattern.
4. Skip formatter/linter findings when CI already enforces them.
5. Name what is good. Uniformly negative reviews are not calibrated.
6. Consolidate at roughly 40 findings. More usually means the themes are wrong.
7. Do not edit production code during the audit. Illustrative diffs are allowed
   only for the highest-impact findings.
8. Prefer fewer types, exports, dependencies, globals, branches, and lines. An
   abstraction must remove more complexity than it introduces.
9. Treat each logical domain as independently consumable: a small explicit API,
   owned types and errors, no caller-specific knowledge, and no hidden global
   wiring. Shared types belong below their consumers only when the concept is
   genuinely shared, never merely to break an import cycle.
10. Prefer real implementations, fixtures, temporary stores, and process-level
    tests over mocks. Call out mocks that duplicate production logic or freeze
    implementation details.

## State and scope

For a long audit, write artifacts under `.pi-agent/audit/<slug>/`:

- `scope.md`: target, base revision, exclusions, context, commands.
- `system-map.md`: Phase 1 evidence.
- `passes/<role>.md`: normalized crew findings.
- `report.md`: final report.
- `status.json`: pending/in-progress/completed stages.

Never put scratch files in source directories. For a PR, audit both the full
changed files and the resulting codebase. Compare against the merge base, not
only the previous commit.

## Phase 1: orient before critique

Measure and map:

- Purpose, lifecycle stage, supported platforms, and stated constraints.
- Executable entry points, handlers, jobs, event consumers, and plugin seams.
- Package/module boundaries and the actual import graph.
- Data stores, schemas, migration/versioning rules, and write ownership.
- External processes, network calls, credentials, and trust boundaries.
- Build, test, release, and deployment paths from executable config.
- Two or three primary request/data flows end to end.
- LOC by language, production/test split, largest files/functions/types,
  exported surface, fan-in/fan-out, globals, and dependency counts.

State the system invariants before reviewing them. Mark each as enforced,
partially enforced, unenforced, or contradicted, with evidence.

## Phase 2: full-crew passes

Launch independent tasks in parallel. Give every subagent the target, base,
context, exclusions, required output, and this evidence rule. Tell each not to
create files. Use the roles that apply; for a major public release the default
set is:

- `architect`: boundaries, dependency direction, domain/API shape, evolvability.
- `engineer`: complexity, duplication, globals, idiomatic code, exported surface.
- `qa-lead`: risk-based tests, real-vs-mocked coverage, determinism, contract gaps.
- `security-lead`: trust boundaries, secrets, host execution, supply chain.
- `sre-lead`: failure modes, lifecycle, concurrency, timeouts, diagnostics.
- `dx-consultant`: public APIs, CLI contracts, contributor path, extension cost.
- `designer` or `product-manager`: user mental model and scope simplification.
- `legal`: licensing and redistribution when public artifacts ship.
- `deep`: one forensic pass over the hardest or highest-risk question.
- `fanout`: quantitative measurements and shrink-only ratchets for size,
  exports, globals, dependency edges, and process exits.

Parallelize independent passes in one `subagent` call; do not serialize roles
that inspect the same tree read-only. Run the cross-vendor `review` pass only
after the parent has formed a preliminary consensus, so it has claims to refute.

Review dimensions, using N/A when truly irrelevant:

- Architecture and domain boundaries
- Domain modeling and persisted data
- Public/internal APIs and compatibility
- Correctness, concurrency, resources, and edge cases
- Security and trust boundaries
- Performance and scaling appropriate to the system's real deployment model
- Reliability and operability
- Risk-aligned testing and mock dependence
- Code quality, complexity, duplication, globals, and dead exports
- Dependencies, build reproducibility, licensing, and supply chain
- Documentation and onboarding
- Evolvability for the next three to five likely changes

## Phase 3: semantic-diff pass for refactors

Compiler and tests can agree on the same mechanically introduced bug. For any
codemod, package extraction, bulk rename, or generated edit, separately compare
against the base:

- String literals and user-visible prose
- JSON/YAML/TOML tags and schema keys
- Subprocess names and argv
- Environment variables, file names, permissions, and paths
- Numeric constants, durations, and defaults
- Embedded templates and generated artifacts
- Stdout/stderr routing and exit codes
- Tests whose expected values changed in the same commit

Inspect every difference that is not an advertised behavior change. A test
updated in lockstep is not independent evidence.

## Phase 4: verify and reconcile

The parent reviewer must inspect the cited line ranges and enough surrounding
code to verify every High or Critical finding; do not load whole large files
when a targeted read proves the claim. Run focused reproductions for those
claims, plus the repository's canonical build/test gate. Compare old and new
binaries or artifacts when a public contract may have changed. Then give the
preliminary findings to a cross-vendor `review` subagent and ask it to refute
them and find omissions. Reconcile the crew:

- Drop refuted or duplicate findings.
- Separate PR regressions from pre-existing debt.
- Calibrate severity to this system, not a generic internet service.
- Distinguish merge blockers, pre-release blockers, and follow-up work.

## Output

Write and return:

1. **Executive summary**: 5-8 sentences, largest risk, and ship/block verdict.
2. **System map**: boundaries, stores, dependencies, key flows, and size.
3. **Invariant matrix**: status and evidence.
4. **Findings sorted by severity**, each with:
   - title, severity, dimensions
   - Observed/Inferred label
   - `path:line` evidence
   - concrete consequence
   - recommendation, agent-time estimate, and prerequisite status
5. **Top five actions**, sequenced by dependency and risk reduction.
6. **What is good**, with locations.
7. **Unverified assumptions and coverage gaps**.
8. **Program recommendation**: merge now, repair then merge, split/rewrite, or
   stop; include a parallelizable follow-up shape when work is needed.

Severity:

- **Critical**: likely data loss, security breach, or sustained outage.
- **High**: likely incident, contract break, or blocker to the stated near-term goal.
- **Medium**: meaningful risk or compounding cost, but survivable.
- **Low**: contained cleanliness, consistency, or efficiency issue.

No completion claim without fresh command output. Use `code-review` for ordinary
diffs; use this skill when the question is whether the system itself is sound.
