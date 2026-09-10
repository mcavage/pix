---
name: conventions
description: Scratch layout, persisted workflow state, and output conventions. During deliver, use its embedded rules; load this only for an uncovered requirement or an explicit request.
---
# conventions

A few rules that apply across skills.

## Working directory
Intermediate artifacts go in `.pi-agent/` at the project root (gitignored): a full
audit trail without touching the source tree.
```
.pi-agent/
  <kind>/<slug>/   # artifacts + status.json for a multi-stage piece of work
  scratch/         # one-off queries, debug scripts, intermediate results
```
**Hard rule: never write scratch files in the repo.** No `query*.py`, `tmp_*`,
throwaway scripts, or one-off tests in the repo root, `src/`, `scripts/`, or any
source directory. Scratch goes in `.pi-agent/scratch/` or `/tmp/`.

## Persistence
For ordinary work you do not manage state by hand. pi persists the session
(`--session-dir`, resume with `-c`/`-r`), and the memory service recalls relevant
facts automatically. Do not build a parallel state file for normal tasks.

The exception is a long, MULTI-STAGE workflow (the wf-* skills): a run that spans
many stages and could be interrupted or compacted mid-pipeline. Those write a
`.pi-agent/<kind>/<slug>/status.json` (stages done, in progress, verdicts, pending)
and each stage writes its artifact to disk before advancing. On resume, read
status.json and the completed artifacts to reconstruct where the pipeline was. Read
the file; do not trust context memory for what a subagent produced.

## Estimation
Estimate from observed comparable runs and the dependency path. Separate model,
tool, integration and review time. Human-to-agent conversion ratios are not
evidence; report uncertainty rather than promising a fixed compression factor.

## Output formats
Preserve the project stack and requested output format. Choose new dependencies
only for the actual artifact; do not introduce a UI framework for a CLI change.

## Terminal rendering
This rule is about what you PRINT to the terminal at runtime, not what a skill or
doc file contains (tables inside files and docs are fine, the model reads them).
When you print structured data to the TUI, prefer bold-label lines or bullet lists;
the terminal renderer may not render markdown tables. Example:
`**Name:** Alice  **Role:** PM  **Status:** green`

## Docs travel with code
A change to a user-facing surface and the docs that describe it are ONE change,
never two. When you add or rename a CLI verb/flag, a config key, an env var, a
default, a public API, or a skill/agent's triggers, update the docs that describe
it in the SAME commit: man page, `--help`/usage text, README, `AGENTS.md`,
CHANGELOG, and any frontmatter `description`. Grep the changed identifier across
the whole doc set and reconcile every hit before you commit.

Vigilance is not a mechanism. Where a surface is ENUMERABLE (a verb table, a
config-key list, a set of env vars), back it with an **anti-drift test** that
fails when code and docs diverge, so the next person can't forget. The canonical
pattern: pix's `services/host/cmd/pix/man_test.go` parses the man page
and asserts it documents every entry in `knownVerbs` AND every key the CLI's own
help lists, failing CI on any gap. If you fix doc drift by hand and no such test
exists for that surface, add one as part of the fix, that is what stops it
recurring. Prefer generating docs from the source of truth over asserting they
match; assert only when generation is impractical.

## Secrets
Secret injection exports keys as environment variables for the whole process tree,
so a subagent with bash can read any key via `env`. The tool-permission model does
not cover raw bash. That is fine for a single-user local setup where all agents
share one trust domain. For multi-user or cloud, scope secrets per process and
rotate often.
