# Pix documentation

## Using Pix

- [Install and start](../README.md): prerequisites, setup, environments, and daily use.
- [First session](getting-started.md): choose an environment, work, and keep context.
- [Command reference](reference.md): complete CLI behavior and configuration.
- [Memory](memory.md): recall, capture, scopes, backups, and privacy.
- [Security](../SECURITY.md): mounted files, account access, and host trust.

For connections supplied by an environment, start with that environment's README.

## Developing Pix or an environment

- [AGENTS.md](../AGENTS.md): code ownership, invariants, validation, and host UAT.
- [Contributing](../CONTRIBUTING.md): development setup and PR expectations.
- [Product contract](design/pix-v2-surface.md) and
  [architecture](design/pix-v2-architecture.md): the accepted v2 design.
- [Google Workspace example](gworkspace.md): native MCP declarations, credentials,
  and meaningful probes for environment authors.
- [Host service runbook](runbooks/host-services.md): operating Pix's memory container.
- [Host module](../services/host/README.md) and
  [memory module](../services/memory/README.md): implementation entry points.
- [Subagents](design/subagents-extension.md),
  [subagent tracker](design/subagent-pin-tracker.md), and
  [output styles](design/output-styles.md): extension design notes.
- [Task design](design/worktree-tasks.md): background for isolated task checkouts;
  the current commands are in the command reference.

## Historical and upstream material

Design documents explicitly marked **HISTORICAL** describe previous implementations
or abandoned plans. They are retained as engineering evidence, not user guides or
instructions to revive removed APIs. In particular, the packs, serve lifecycle,
old onboarding, and self-learning-loop designs do not describe Pix v2.

`docs/upstream/` contains version-specific sbx/Pi investigations. Check the
version and current implementation before applying an old workaround. The active
[terminal renderer patch](upstream/tui-bottom-pin.md) is checked when Pi is bumped.

Legal and release requirements live under [legal/RELEASE-SAFEGUARDS.md](legal/RELEASE-SAFEGUARDS.md).
