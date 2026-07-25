---
name: conventions
description: Working directory layout, scratch-file rules, output formats, terminal rendering, estimation, and how long multi-stage workflows survive compaction. Auto-loads on engineering and content work.
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
Time estimates use agent-time (wall-clock for an AI agent): a 1-day human task is
5-15 min, a 1-week sprint is 30-60 min, a 1-month project is 2-4 hours.

## Output formats
Prototypes: React + Tailwind + shadcn/ui. Docs: Markdown. Decks: Marp. Data:
Python + pandas. Architecture: Mermaid diagrams.

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
