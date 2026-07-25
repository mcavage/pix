---
name: guard
description: Session-level guardrails for destructive commands and edit scope restrictions. Use for "guard", "warn before deletes", "restrict edits to src/", or working near production or irreversible operations.
---
# guard

Guard is a reminder system, not enforcement. The agent must check and honor
guard state before acting. It cannot physically block tool calls.

## Modes

- `/guard` or `guard status`: show current state
- `/guard --scope <dir>`: restrict all edits to one directory subtree
- `/guard off`: clear all restrictions for this session

State lasts for the current session only.

## Destructive command warnings

Before executing any of the following, emit a blunt warning and wait for
explicit confirmation:

- `rm -rf`, `rm -r` (outside `node_modules/` or `.git/`)
- `DROP TABLE`, `DROP DATABASE`, `TRUNCATE`
- `git push --force`, `git reset --hard`, `git clean -fd`
- `kubectl delete`, `docker system prune`
- Any command targeting a production host, URL, or environment marker

**Production detection:** treat a target as production when the command text
contains `prod`, `production`, known prod hostnames, live customer domains, or
cloud resources labeled prod. When unclear, assume higher risk and warn.

## Warning format

```
Guard warning: destructive command detected.
Command: <command>
Reason: <why it is risky>
Scope: <what may be deleted or changed>
Action required: explicit user confirmation
```

Do not bury this in a paragraph. Surface it before the command runs.

## Edit scope restriction

When a scope is set, all edits must stay under that path. When a requested
change falls outside scope:
1. Call it out before editing.
2. Ask whether to widen or remove scope.
3. Do not silently proceed.

## Agent behavior

Before destructive commands: inspect the command text, match against the list
above, check for prod indicators, warn and wait.

Before edits: check the scope, compare every target path against the allowed
subtree, stop if any edit escapes it.

Guard is not a substitute for judgment. It is a check for moments when speed
makes mistakes likely.
