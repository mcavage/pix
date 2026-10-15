---
name: marsh-orchestration
description: Delegate bounded role work and compose agent jobs using marsh itself, without a subagent adapter or second scheduler.
---
# Orchestration is marsh

Read `marsh context` for this job's registered commands, spawn set and admission
limits. Pick the smallest useful decomposition. The parent owns scope, ordering,
integration, acceptance and reporting; roles are prompts, not authority grants.

## One role

Read `/opt/pix/package/roles/NAME.md`. Its frontmatter describes intent, not an
enforced model/tool/turn-limit binding.
Read an optional `~/.pix-agent/agent/roster.json`: `models.ROLE` supplies a
requested binding, `defaultModel` a fallback, and explicit user/task choices win.
Pass that binding via `--model`; it is not proof of the responding model.
Include the role body in a bounded work order: accepted task, owned files, real
caller, constraints, evidence and return artifact.
Use an explicit `--model PROVIDER/ID` when required and available. For example,
from a Pi shell tool (which already has non-terminal stdio):

```sh
pix -p --no-session --model anthropic/claude-opus-5-5 \
  --append-system-prompt "$(cat /opt/pix/package/roles/review.md)" \
  'Read-only: review the current diff against the agreed contract. Return blockers and evidence.'
```

`pix` resolves through marsh, creating a real child job; do not call an image's
Pi binary to bypass admission. A job with terminal stdio is refused: from an
interactive shell use `</dev/null | cat` for a noninteractive child where needed.
`--mode json` can expose Pi response metadata for identity/usage evidence; do not
infer identity or zero cost from the requested model, role name, or receipt.

## Independent parallel work

Use `marsh fanout ... | marsh collect` for work safe to share the same files:

```sh
marsh fanout ::: tests make test ::: lint make lint | marsh collect
```

For concurrent edits use private forks instead, using registered commands:

```sh
marsh split \
  ::: implementation pix -p --no-session 'Implement only the agreed code change; run its focused tests.' \
  ::: documentation pix -p --no-session 'Update only the agreed documentation; check its examples.' \
  | marsh join --keep
```

Forks start from the same snapshot. Inspect each result and patch; apply only
the intended changes. Join does not merge/apply automatically. Collect preserves
branch output and status; it is not an automatic work-order/context compiler.

## Dependent chains

Run implementation, inspect its actual diff/checks, integrate accepted patches,
then start a reviewer against that candidate. Pass relevant output explicitly
as files/stdin or bounded prompt context. Do not pipe arbitrary agent prose into
another agent and call that a validated chain. A zero exit is process success,
not proof the requested task or acceptance tests succeeded.

Stop on failed/refused/incomplete prerequisites and report partial artifacts.
Do not run implementer and dependent reviewer in sibling split forks; that
reviewer would see the original snapshot. Do not silently retry a lost job.

Inspect `marsh jobs --tree` for progress and `marsh results` for structural exit
and cleanup evidence. They are not model-response/usage logs. Ctrl-C cancels the
foreground tree. Respect limits and use `--no-spawn` for a child that must not
delegate further; role prompts alone do not enforce that restriction.
